package ec2provider

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/aws-iam-authenticator/pkg/metrics"
)

func TestGetPrivateDNSName(t *testing.T) {
	metrics.InitMetrics(prometheus.NewRegistry())
	ec2Provider := newMockedEC2ProviderImpl()
	ec2Provider.ec2 = &MockEc2Client{Reservations: prepareSingleInstanceOutput()}
	go ec2Provider.StartEc2DescribeBatchProcessing(context.TODO())
	dns_name, err := ec2Provider.GetPrivateDNSName(context.TODO(), "ec2-1")
	if err != nil {
		t.Error("There is an error which is not expected when calling ec2 API with setting up mocks")
	}
	if dns_name != "ec2-dns-1" {
		t.Errorf("want: %v, got: %v", "ec2-dns-1", dns_name)
	}
}

func prepareSingleInstanceOutput() []*ec2types.Reservation {
	reservations := []*ec2types.Reservation{
		{
			Groups: nil,
			Instances: []ec2types.Instance{
				ec2types.Instance{
					InstanceId:     aws.String("ec2-1"),
					PrivateDnsName: aws.String("ec2-dns-1"),
				},
			},
			OwnerId:       nil,
			RequesterId:   nil,
			ReservationId: nil,
		},
	}
	return reservations
}

func TestGetPrivateDNSNameWithBatching(t *testing.T) {
	metrics.InitMetrics(prometheus.NewRegistry())
	ec2Provider := newMockedEC2ProviderImpl()
	reservations := prepare100InstanceOutput()
	ec2Provider.ec2 = &MockEc2Client{Reservations: reservations}
	go ec2Provider.StartEc2DescribeBatchProcessing(context.TODO())
	var wg sync.WaitGroup
	for i := 1; i < 101; i++ {
		instanceString := "ec2-" + strconv.Itoa(i)
		dnsString := "ec2-dns-" + strconv.Itoa(i)
		wg.Add(1)
		// This code helps test the batch functionality twice
		if i == 50 {
			time.Sleep(200 * time.Millisecond)
		}
		go getPrivateDNSName(ec2Provider, instanceString, dnsString, t, &wg)
	}
	wg.Wait()
}

func getPrivateDNSName(ec2provider *ec2ProviderImpl, instanceString string, dnsString string, t *testing.T, wg *sync.WaitGroup) {
	defer wg.Done()
	dnsName, err := ec2provider.GetPrivateDNSName(context.TODO(), instanceString)
	if err != nil {
		t.Error("There is an error which is not expected when calling ec2 API with setting up mocks")
	}
	if dnsName != dnsString {
		t.Errorf("want: %v, got: %v", dnsString, dnsName)
	}
}

func prepare100InstanceOutput() []*ec2types.Reservation {

	var reservations []*ec2types.Reservation

	for i := 1; i < 101; i++ {
		instanceString := "ec2-" + strconv.Itoa(i)
		dnsString := "ec2-dns-" + strconv.Itoa(i)
		instance := ec2types.Instance{
			InstanceId:     aws.String(instanceString),
			PrivateDnsName: aws.String(dnsString),
		}
		var instances []ec2types.Instance
		instances = append(instances, instance)
		res1 := &ec2types.Reservation{
			Groups:        nil,
			Instances:     instances,
			OwnerId:       nil,
			RequesterId:   nil,
			ReservationId: nil,
		}
		reservations = append(reservations, res1)
	}
	return reservations

}

func TestGetSourceAcctAndArn(t *testing.T) {
	type args struct {
		roleARN string
	}
	tests := []struct {
		name    string
		args    args
		want    string
		wantErr bool
	}{
		{
			name: "corect role arn",
			args: args{
				roleARN: "arn:aws:iam::123456789876:role/test-cluster",
			},
			want:    "123456789876",
			wantErr: false,
		},
		{
			name: "incorect role arn",
			args: args{
				roleARN: "arn:aws:iam::123456789876",
			},
			want:    "",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := getSourceAccount(tt.args.roleARN)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetSourceAccount() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("GetSourceAccount() got = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestApplySTSRequestHeaders(t *testing.T) {
	tests := []struct {
		name            string
		headerSourceArn string
		want            map[string]string
		wantErr         bool
	}{
		{
			name:            "header with source arn",
			headerSourceArn: "arn:aws:eks:us-east-1:123456789012:MyCluster/res1",
			want: map[string]string{
				headerSourceAccount: "123456789012",
				headerSourceArn:     "arn:aws:eks:us-east-1:123456789012:MyCluster/res1",
			},
			wantErr: false,
		},
		{
			name:            "header without source arn",
			headerSourceArn: "",
			want: map[string]string{
				headerSourceAccount: "",
				headerSourceArn:     "",
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var capturedHeaders map[string]string
			mockRT := &MockRoundTripper{
				VerifyHeaders: func(headers http.Header) error {
					capturedHeaders = make(map[string]string)
					capturedHeaders[headerSourceAccount] = headers.Get(headerSourceAccount)
					capturedHeaders[headerSourceArn] = headers.Get(headerSourceArn)
					return nil
				},
			}

			cfg := aws.Config{
				Region: "us-west-2",
				Credentials: aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(
					"AKID", "SECRET", "SESSION")),
				HTTPClient: &http.Client{
					Transport: mockRT,
				},
			}
			stsClient := sts.NewFromConfig(*applySTSRequestHeaders(&cfg, tt.headerSourceArn))
			_, err := stsClient.GetCallerIdentity(context.TODO(), &sts.GetCallerIdentityInput{})
			if err != nil {
				t.Errorf("error making sts client call, %v", err)
			}

			// Verify headers
			for k, v := range tt.want {
				if v != "" {
					got, ok := capturedHeaders[k]
					if !ok {
						t.Errorf("header %s not found", k)
						return
					}
					if got != v {
						t.Errorf("header %s = %v, want %v", k, got, v)
					}
				}
			}
		})
	}
}

type MockRoundTripper struct {
	ExpectedHeaders map[string]string
	VerifyHeaders   func(headers http.Header) error
}

func (m *MockRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := m.VerifyHeaders(req.Header); err != nil {
		return nil, err
	}

	resp := &http.Response{
		StatusCode: 200,
		Body:       nil,
	}

	return resp, nil
}
