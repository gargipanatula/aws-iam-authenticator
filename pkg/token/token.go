/*
Copyright 2017-2020 by the contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package token

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/pkg/apis/clientauthentication"
	clientauthv1beta1 "k8s.io/client-go/pkg/apis/clientauthentication/v1beta1"
	"k8s.io/utils/strings/slices"
	"sigs.k8s.io/aws-iam-authenticator/pkg"
	"sigs.k8s.io/aws-iam-authenticator/pkg/arn"
	"sigs.k8s.io/aws-iam-authenticator/pkg/ec2provider"
	"sigs.k8s.io/aws-iam-authenticator/pkg/filecache"
	"sigs.k8s.io/aws-iam-authenticator/pkg/metrics"
)

// Identity is returned on successful Verify() results. It contains a parsed
// version of the AWS identity used to create the token.
type Identity struct {
	// ARN is the raw Amazon Resource Name returned by sts:GetCallerIdentity
	ARN string

	// CanonicalARN is the Amazon Resource Name converted to a more canonical
	// representation. In particular, STS assumed role ARNs like
	// "arn:aws:sts::ACCOUNTID:assumed-role/ROLENAME/SESSIONNAME" are converted
	// to their IAM ARN equivalent "arn:aws:iam::ACCOUNTID:role/NAME"
	CanonicalARN string

	// AccountID is the 12 digit AWS account number.
	AccountID string

	// UserID is the unique user/role ID (e.g., "AROAAAAAAAAAAAAAAAAAA").
	UserID string

	// SessionName is the STS session name (or "" if this is not a
	// session-based identity). For EC2 instance roles, this will be the EC2
	// instance ID (e.g., "i-0123456789abcdef0"). You should only rely on it
	// if you trust that _only_ EC2 is allowed to assume the IAM Role. If IAM
	// users or other roles are allowed to assume the role, they can provide
	// (nearly) arbitrary strings here.
	SessionName string

	// The AWS Access Key ID used to authenticate the request.  This can be used
	// in conjunction with CloudTrail to determine the identity of the individual
	// if the individual assumed an IAM role before making the request.
	AccessKeyID string

	// ASW STS endpoint used to authenticate (expected values is sts endpoint eg: sts.us-west-2.amazonaws.com)
	STSEndpoint string
}

const (
	// The sts GetCallerIdentity request is valid for 15 minutes regardless of this parameters value after it has been
	// signed, but we set this unused parameter to 60 for legacy reasons (we check for a value between 0 and 60 on the
	// server side in 0.3.0 or earlier).  IT IS IGNORED.  If we can get STS to support x-amz-expires, then we should
	// set this parameter to the actual expiration, and make it configurable.
	requestPresignParam = 60
	// The actual token expiration (presigned STS urls are valid for 15 minutes after timestamp in x-amz-date).
	presignedURLExpiration = 15 * time.Minute
	v1Prefix               = "k8s-aws-v1."
	maxTokenLenBytes       = 1024 * 4
	clusterIDHeader        = "x-k8s-aws-id"
	// Format of the X-Amz-Date header used for expiration
	// https://golang.org/pkg/time/#pkg-constants
	dateHeaderFormat   = "20060102T150405Z"
	kindExecCredential = "ExecCredential"
	execInfoEnvKey     = "KUBERNETES_EXEC_INFO"
	stsServiceID       = "sts"
)

// Token is generated and used by Kubernetes client-go to authenticate with a Kubernetes cluster.
type Token struct {
	Token      string
	Expiration time.Time
}

// GetTokenOptions is passed to GetWithOptions to provide an extensible get token interface
type GetTokenOptions struct {
	Region               string
	ClusterID            string
	AssumeRoleARN        string
	AssumeRoleExternalID string
	SessionName          string
}

// FormatError is returned when there is a problem with token that is
// an encoded sts request.  This can include the url, data, action or anything
// else that prevents the sts call from being made.
type FormatError struct {
	message string
}

func (e FormatError) Error() string {
	return "input token was not properly formatted: " + e.message
}

// STSError is returned when there was either an error calling STS or a problem
// processing the data returned from STS.
type STSError struct {
	message string
}

func (e STSError) Error() string {
	return "sts getCallerIdentity failed: " + e.message
}

// NewSTSError creates a error of type STS.
func NewSTSError(m string) STSError {
	return STSError{message: m}
}

// STSThrottling is returned when there was STS Throttling.
type STSThrottling struct {
	message string
}

func (e STSThrottling) Error() string {
	return "sts getCallerIdentity was throttled: " + e.message
}

// NewSTSError creates a error of type STS.
func NewSTSThrottling(m string) STSThrottling {
	return STSThrottling{message: m}
}

var parameterWhitelist = map[string]bool{
	"action":               true,
	"version":              true,
	"x-amz-algorithm":      true,
	"x-amz-credential":     true,
	"x-amz-date":           true,
	"x-amz-expires":        true,
	"x-amz-security-token": true,
	"x-amz-signature":      true,
	"x-amz-signedheaders":  true,
}

// this is the result type from the GetCallerIdentity endpoint
type getCallerIdentityWrapper struct {
	GetCallerIdentityResponse struct {
		GetCallerIdentityResult struct {
			Account string `json:"Account"`
			Arn     string `json:"Arn"`
			UserID  string `json:"UserId"`
		} `json:"GetCallerIdentityResult"`
		ResponseMetadata struct {
			RequestID string `json:"RequestId"`
		} `json:"ResponseMetadata"`
	} `json:"GetCallerIdentityResponse"`
}

// Generator provides new tokens for the AWS IAM Authenticator.
type Generator interface {
	// Get a token using the provided options
	GetWithOptions(ctx context.Context, options *GetTokenOptions) (Token, error)
	// GetWithSTS returns a token valid for clusterID using the given STS client.
	GetWithSTS(clusterID string, stsClient *sts.Client) (Token, error)
	// FormatJSON returns the client auth formatted json for the ExecCredential auth
	FormatJSON(Token) string
}

type generator struct {
	forwardSessionName bool
	cache              bool
	nowFunc            func() time.Time
}

// NewGenerator creates a Generator and returns it.
func NewGenerator(forwardSessionName bool, cache bool) (Generator, error) {
	return generator{
		forwardSessionName: forwardSessionName,
		cache:              cache,
		nowFunc:            time.Now,
	}, nil
}

// StdinStderrTokenProvider gets MFA token from standard input.
func StdinStderrTokenProvider() (string, error) {
	var v string
	fmt.Fprint(os.Stderr, "Assume Role MFA token code: ")
	_, err := fmt.Scanln(&v)
	return v, err
}

// GetWithOptions takes a GetTokenOptions struct, builds the STS client, and wraps GetWithSTS.
// If no session has been passed in options, it will build a new session. If an
// AssumeRoleARN was passed in then assume the role for the session.
func (g generator) GetWithOptions(ctx context.Context, options *GetTokenOptions) (Token, error) {
	if options.ClusterID == "" {
		return Token{}, fmt.Errorf("ClusterID is required")
	}

	// create a session with the "base" credentials available
	// (from environment variable, profile files, EC2 metadata, etc)
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithAssumeRoleCredentialOptions(func(options *stscreds.AssumeRoleOptions) {
			options.TokenProvider = StdinStderrTokenProvider
		}),
		config.WithEC2IMDSClientEnableState(imds.ClientEnabled),
	)
	if err != nil {
		return Token{}, fmt.Errorf("could not create config: %v", err)
	}
	cfg.APIOptions = append(cfg.APIOptions,
		middleware.AddUserAgentKeyValue("aws-iam-authenticator", pkg.Version),
	)
	if options.Region != "" {
		cfg.Region = options.Region
	}

	// The SDK requires a region for clients
	// https://docs.aws.amazon.com/sdk-for-go/v2/developer-guide/configure-gosdk.html
	if cfg.Region == "" {
		// Attempt to get the region from IMDS (applicable if run on an EC2 instance)
		imdsClient := imds.NewFromConfig(cfg)
		region, err := imdsClient.GetRegion(context.Background(), &imds.GetRegionInput{})
		if err != nil {
			// Default to the global region
			logrus.Infof("failed to get region from IMDS for token generation, defaulting to us-east-1. imds error: %v", err)
			cfg.Region = "us-east-1"
		} else {
			cfg.Region = region.Region
		}
	}

	if g.cache {
		// figure out what profile we're using
		var profile string
		if v := os.Getenv("AWS_PROFILE"); len(v) > 0 {
			profile = v
		} else {
			profile = "default"
		}
		// create a cacheing Provider wrapper around the Credentials
		if cacheProvider, err := filecache.NewFileCacheProvider(
			options.ClusterID,
			profile,
			options.AssumeRoleARN,
			cfg.Credentials); err == nil {
			cfg.Credentials = cacheProvider
		} else {
			fmt.Fprintf(os.Stderr, "unable to use cache: %v\n", err)
		}
	}

	// use an STS client based on the direct credentials
	stsClient := sts.NewFromConfig(cfg)

	// if a roleARN was specified, replace the STS client with one that uses
	// temporary credentials from that role.
	if options.AssumeRoleARN != "" {
		var sessionSetters []func(*stscreds.AssumeRoleOptions)

		if options.AssumeRoleExternalID != "" {
			sessionSetters = append(sessionSetters, func(provider *stscreds.AssumeRoleOptions) {
				provider.ExternalID = &options.AssumeRoleExternalID
			})
		}

		if g.forwardSessionName {
			// If the current session is already a federated identity, carry through
			// this session name onto the new session to provide better debugging
			// capabilities
			resp, err := stsClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
			if err != nil {
				return Token{}, err
			}

			userIDParts := strings.Split(*resp.UserId, ":")
			if len(userIDParts) == 2 {
				sessionSetters = append(sessionSetters, func(provider *stscreds.AssumeRoleOptions) {
					provider.RoleSessionName = userIDParts[1]
				})
			}
		} else if options.SessionName != "" {
			sessionSetters = append(sessionSetters, func(provider *stscreds.AssumeRoleOptions) {
				provider.RoleSessionName = options.SessionName
			})
		}

		// create STS-based credentials that will assume the given role
		creds := stscreds.NewAssumeRoleProvider(stsClient, options.AssumeRoleARN, sessionSetters...)
		cfg.Credentials = creds
		// create an STS API interface that uses the assumed role's temporary credentials
		stsClient = sts.NewFromConfig(cfg)
	}

	return g.GetWithSTS(options.ClusterID, stsClient)
}

type presignFixedTime struct {
	p           sts.HTTPPresignerV4
	signingTime time.Time
}

func (w *presignFixedTime) PresignHTTP(
	ctx context.Context, credentials aws.Credentials, r *http.Request,
	payloadHash string, service string, region string, signingTime time.Time,
	optFns ...func(*v4.SignerOptions),
) (url string, signedHeader http.Header, err error) {
	return w.p.PresignHTTP(ctx, credentials, r,
		payloadHash, service, region, w.signingTime,
		optFns...)
}

func withPresignFixedTime(t time.Time) func(*sts.PresignOptions) {
	return func(o *sts.PresignOptions) {
		o.Presigner = &presignFixedTime{
			p:           o.Presigner,
			signingTime: t,
		}
	}
}

// GetWithSTS returns a token valid for clusterID using the given STS client.
func (g generator) GetWithSTS(clusterID string, stsClient *sts.Client) (Token, error) {
	// Generate an sts:GetCallerIdentity presigned request
	presignClient := sts.NewPresignClient(stsClient)

	presignedRequest, err := presignClient.PresignGetCallerIdentity(context.Background(), &sts.GetCallerIdentityInput{},
		func(presignOptions *sts.PresignOptions) {
			// Configure the presigned request with a fixed time so we can control the now time for testing
			withPresignFixedTime(g.nowFunc())(presignOptions)

			presignOptions.ClientOptions = append(presignOptions.ClientOptions, func(stsOptions *sts.Options) {
				stsOptions.APIOptions = append(stsOptions.APIOptions,
					// Add our custom cluster ID header
					smithyhttp.SetHeaderValue(clusterIDHeader, clusterID),
					// Sign the request.  The expires parameter (sets the x-amz-expires header) is
					// currently ignored by STS, and the token expires 15 minutes after the x-amz-date
					// timestamp regardless.  We set it to 60 seconds for backwards compatibility (the
					// parameter is a required argument to Presign(), and authenticators 0.3.0 and older are expecting a value between
					// 0 and 60 on the server side).
					// https://pkg.go.dev/github.com/aws/aws-sdk-go-v2/aws/signer/v4#:~:text=request%20add%20the%20%22-,X%2DAmz%2DExpires,-%22%20query%20parameter%20on
					// Broader context: https://github.com/aws/aws-sdk-go/issues/2167
					smithyhttp.SetHeaderValue("X-Amz-Expires", strconv.Itoa(requestPresignParam)))
			})
		})

	if err != nil {
		return Token{}, err
	}

	// Set token expiration to 1 minute before the presigned URL expires for some cushion
	tokenExpiration := g.nowFunc().Local().Add(presignedURLExpiration - 1*time.Minute)
	// TODO: this may need to be a constant-time base64 encoding
	return Token{v1Prefix + base64.RawURLEncoding.EncodeToString([]byte(presignedRequest.URL)), tokenExpiration}, nil
}

// FormatJSON formats the json to support ExecCredential authentication
func (g generator) FormatJSON(token Token) string {
	apiVersion := clientauthv1beta1.SchemeGroupVersion.String()
	env := os.Getenv(execInfoEnvKey)
	if env != "" {
		cred := &clientauthentication.ExecCredential{}
		if err := json.Unmarshal([]byte(env), cred); err == nil {
			apiVersion = cred.APIVersion
		}
	}

	expirationTimestamp := metav1.NewTime(token.Expiration)
	execInput := &clientauthv1beta1.ExecCredential{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiVersion,
			Kind:       kindExecCredential,
		},
		Status: &clientauthv1beta1.ExecCredentialStatus{
			ExpirationTimestamp: &expirationTimestamp,
			Token:               token.Token,
		},
	}
	enc, _ := json.Marshal(execInput)
	return string(enc)
}

// Verifier validates tokens by calling STS and returning the associated identity.
type Verifier interface {
	Verify(token string) (*Identity, error)
}

type tokenVerifier struct {
	client            *http.Client
	clusterID         string
	validSTShostnames map[string]bool
}

func stsHostsForPartition(ctx context.Context, partitionID, region string, ec2Client ec2provider.EC2API) map[string]bool {
	validSTShostnames := map[string]bool{}

	var tlds []string
	serviceNames := []string{"sts", "sts-fips"}
	switch partitionID {
	case "aws":
		tlds = []string{"amazonaws.com", "api.aws"}
		validSTShostnames["sts.amazonaws.com"] = true
	case "aws-us-gov":
		tlds = []string{"amazonaws.com", "api.aws"}
	case "aws-cn":
		serviceNames = []string{"sts"}
		tlds = []string{"amazonaws.com.cn"}
	case "aws-iso":
		tlds = []string{"c2s.ic.gov"}
	case "aws-iso-b":
		tlds = []string{"sc2s.sgov.gov"}
	case "aws-iso-e":
		tlds = []string{"cloud.adc-e.uk"}
	case "aws-iso-f":
		tlds = []string{"csp.hci.ic.gov"}
	default:
		logrus.Errorf("unrecognized partition %s", partitionID)
		return validSTShostnames
	}

	// Get a list of regions available to this account
	var regions []string
	regionsOutput, err := ec2Client.DescribeRegions(ctx, &ec2.DescribeRegionsInput{
		AllRegions: aws.Bool(true),
	})
	if err != nil {
		logrus.Errorf("failed to get regions: %v", err)
		return validSTShostnames
	}
	for _, regionInfo := range regionsOutput.Regions {
		regions = append(regions, *regionInfo.RegionName)
	}

	// Add the host of the current instances region if not already exists so we don't fail if the region is not
	// present in the go sdk but matches the instances region.
	if !slices.Contains(regions, region) {
		regions = append(regions, region)
	}

	for _, regionInfo := range regionsOutput.Regions {
		for _, serviceName := range serviceNames {
			for _, tld := range tlds {
				hostname := fmt.Sprintf("%s.%s.%s", serviceName, *regionInfo.RegionName, tld)
				validSTShostnames[hostname] = true
			}
		}
	}

	return validSTShostnames
}

// NewVerifier creates a Verifier that is bound to the clusterID and uses the default http client.
func NewVerifier(ctx context.Context, clusterID, partitionID, region string, ec2Client ec2provider.EC2API) Verifier {
	// Initialize metrics if they haven't already been initialized to avoid a
	// nil pointer panic when setting metric values.
	if !metrics.Initialized() {
		metrics.InitMetrics(prometheus.NewRegistry())
	}

	return tokenVerifier{
		client: &http.Client{
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Timeout: 10 * time.Second,
		},
		clusterID:         clusterID,
		validSTShostnames: stsHostsForPartition(ctx, partitionID, region, ec2Client),
	}
}

// verify a sts host, doc: http://docs.amazonaws.cn/en_us/general/latest/gr/rande.html#sts_region
func (v tokenVerifier) verifyHost(host string) error {
	if _, ok := v.validSTShostnames[host]; !ok {
		return FormatError{fmt.Sprintf("unexpected hostname %q in pre-signed URL", host)}
	}
	return nil
}

// Verify a token is valid for the specified clusterID. On success, returns an
// Identity that contains information about the AWS principal that created the
// token. On failure, returns nil and a non-nil error.
func (v tokenVerifier) Verify(token string) (*Identity, error) {
	if len(token) > maxTokenLenBytes {
		return nil, FormatError{"token is too large"}
	}

	if !strings.HasPrefix(token, v1Prefix) {
		return nil, FormatError{fmt.Sprintf("token is missing expected %q prefix", v1Prefix)}
	}

	// TODO: this may need to be a constant-time base64 decoding
	tokenBytes, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, v1Prefix))
	if err != nil {
		return nil, FormatError{err.Error()}
	}

	parsedURL, err := url.Parse(string(tokenBytes))
	if err != nil {
		return nil, FormatError{err.Error()}
	}

	if parsedURL.Scheme != "https" {
		return nil, FormatError{fmt.Sprintf("unexpected scheme %q in pre-signed URL", parsedURL.Scheme)}
	}

	if err = v.verifyHost(parsedURL.Host); err != nil {
		return nil, err
	}

	stsRegion, err := getStsRegion(parsedURL.Host)
	if err != nil {
		return nil, err
	}

	if parsedURL.Path != "/" {
		return nil, FormatError{"unexpected path in pre-signed URL"}
	}

	queryParamsLower := make(url.Values)
	queryParams, err := url.ParseQuery(parsedURL.RawQuery)
	if err != nil {
		return nil, FormatError{"malformed query parameter"}
	}

	if err = validateDuplicateParameters(queryParams); err != nil {
		return nil, err
	}

	for key, values := range queryParams {
		if !parameterWhitelist[strings.ToLower(key)] {
			return nil, FormatError{fmt.Sprintf("non-whitelisted query parameter %q", key)}
		}
		if len(values) != 1 {
			return nil, FormatError{"query parameter with multiple values not supported"}
		}
		queryParamsLower.Set(strings.ToLower(key), values[0])
	}

	if queryParamsLower.Get("action") != "GetCallerIdentity" {
		return nil, FormatError{"unexpected action parameter in pre-signed URL"}
	}

	if !hasSignedClusterIDHeader(&queryParamsLower) {
		return nil, FormatError{fmt.Sprintf("client did not sign the %s header in the pre-signed URL", clusterIDHeader)}
	}

	// We validate x-amz-expires is between 0 and 15 minutes (900 seconds) although currently pre-signed STS URLs, and
	// therefore tokens, expire exactly 15 minutes after the x-amz-date header, regardless of x-amz-expires.
	expires, err := strconv.Atoi(queryParamsLower.Get("x-amz-expires"))
	if err != nil || expires < 0 || expires > 900 {
		return nil, FormatError{fmt.Sprintf("invalid X-Amz-Expires parameter in pre-signed URL: %d", expires)}
	}

	date := queryParamsLower.Get("x-amz-date")
	if date == "" {
		return nil, FormatError{"X-Amz-Date parameter must be present in pre-signed URL"}
	}

	// Obtain AWS Access Key ID from supplied credentials
	accessKeyID := strings.Split(queryParamsLower.Get("x-amz-credential"), "/")[0]

	dateParam, err := time.Parse(dateHeaderFormat, date)
	if err != nil {
		return nil, FormatError{fmt.Sprintf("error parsing X-Amz-Date parameter %s into format %s: %s", date, dateHeaderFormat, err.Error())}
	}

	now := time.Now()
	expiration := dateParam.Add(presignedURLExpiration)
	if now.After(expiration) {
		return nil, FormatError{fmt.Sprintf("X-Amz-Date parameter is expired (%.f minute expiration) %s", presignedURLExpiration.Minutes(), dateParam)}
	}

	req, err := http.NewRequest("GET", parsedURL.String(), nil)
	req.Header.Set(clusterIDHeader, v.clusterID)
	req.Header.Set("accept", "application/json")

	response, err := v.client.Do(req)
	if err != nil {
		metrics.Get().StsConnectionFailure.WithLabelValues(stsRegion).Inc()
		// special case to avoid printing the full URL if possible
		if urlErr, ok := err.(*url.Error); ok {
			return nil, NewSTSError(fmt.Sprintf("error during GET: %v on %s endpoint", urlErr.Err, stsRegion))
		}
		return nil, NewSTSError(fmt.Sprintf("error during GET: %v on %s endpoint", err, stsRegion))
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, NewSTSError(fmt.Sprintf("error reading HTTP result: %v", err))
	}

	metrics.Get().StsResponses.WithLabelValues(fmt.Sprint(response.StatusCode), stsRegion).Inc()
	if response.StatusCode != 200 {
		responseStr := string(responseBody[:])
		// refer to https://docs.aws.amazon.com/STS/latest/APIReference/CommonErrors.html and log
		// response body for STS Throttling is {"Error":{"Code":"Throttling","Message":"Rate exceeded","Type":"Sender"},"RequestId":"xxx"}
		if strings.Contains(responseStr, "Throttling") {
			metrics.Get().StsThrottling.WithLabelValues(stsRegion).Inc()
			return nil, NewSTSThrottling(responseStr)
		}
		return nil, NewSTSError(fmt.Sprintf("error from AWS (expected 200, got %d) on %s endpoint. Body: %s", response.StatusCode, stsRegion, responseStr))
	}

	var callerIdentity getCallerIdentityWrapper
	err = json.Unmarshal(responseBody, &callerIdentity)
	if err != nil {
		return nil, NewSTSError(err.Error())
	}

	id := &Identity{
		AccessKeyID: accessKeyID,
		STSEndpoint: parsedURL.Host,
	}
	return getIdentityFromSTSResponse(id, callerIdentity)
}

func getIdentityFromSTSResponse(id *Identity, wrapper getCallerIdentityWrapper) (*Identity, error) {
	var err error
	result := wrapper.GetCallerIdentityResponse.GetCallerIdentityResult

	id.ARN = result.Arn
	id.AccountID = result.Account

	var principalType arn.PrincipalType
	principalType, id.CanonicalARN, err = arn.Canonicalize(id.ARN)
	if err != nil {
		return nil, NewSTSError(err.Error())
	}

	// The user ID is one of:
	// 1. UserID:SessionName (for assumed roles)
	// 2. UserID (for IAM User principals).
	// 3. AWSAccount:CallerSpecifiedName (for federated users)
	// We want the entire UserID for federated users because otherwise,
	// its just the account ID and is indistinguishable from the UserID
	// of the root user.
	if principalType == arn.FEDERATED_USER || principalType == arn.USER || principalType == arn.ROOT {
		id.UserID = result.UserID
	} else {
		userIDParts := strings.Split(result.UserID, ":")
		if len(userIDParts) == 2 {
			id.UserID = userIDParts[0]
			id.SessionName = userIDParts[1]
		} else {
			return nil, NewSTSError(fmt.Sprintf("malformed UserID %q", result.UserID))
		}
	}

	return id, nil
}

func validateDuplicateParameters(queryParams url.Values) error {
	duplicateCheck := make(map[string]bool)
	for key, _ := range queryParams {
		if _, found := duplicateCheck[strings.ToLower(key)]; found {
			return FormatError{fmt.Sprintf("duplicate query parameter found: %q", key)}
		}
		duplicateCheck[strings.ToLower(key)] = true
	}
	return nil
}

func hasSignedClusterIDHeader(paramsLower *url.Values) bool {
	signedHeaders := strings.Split(paramsLower.Get("x-amz-signedheaders"), ";")
	for _, hdr := range signedHeaders {
		if strings.ToLower(hdr) == strings.ToLower(clusterIDHeader) {
			return true
		}
	}
	return false
}

func getStsRegion(host string) (string, error) {
	if host == "" {
		return "", fmt.Errorf("host is empty")
	}

	parts := strings.Split(host, ".")
	if len(parts) < 3 {
		return "", fmt.Errorf("invalid host format: %v", host)
	}

	if host == "sts.amazonaws.com" {
		return "global", nil
	}
	return parts[1], nil
}
