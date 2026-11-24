package aws

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/sjawhar/envoy/internal/cipher/awssdk"
	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/ratelimit"
)

// Backend is the AWS store's registry entry.
type Backend struct{}

func (Backend) Name() string { return Name }
func (Backend) ID() string   { return "aws" }

// ServiceSDKs are the AWS SDK's service packages agent-secrets and cipherd import for this store
// alone: Secrets Manager and KMS, which hold and encrypt the secrets, and STS, SSO and SSO OIDC, the
// CLI's sign-in.
func (Backend) ServiceSDKs() []string {
	return []string{
		"github.com/aws/aws-sdk-go-v2/service/secretsmanager",
		"github.com/aws/aws-sdk-go-v2/service/kms",
		"github.com/aws/aws-sdk-go-v2/service/sts",
		"github.com/aws/aws-sdk-go-v2/service/sso",
		"github.com/aws/aws-sdk-go-v2/service/ssooidc",
	}
}

func (Backend) Development() bool             { return false }
func (Backend) RereadBudget() ratelimit.Limit { return secretstore.DefaultRereadBudget }
func (Backend) Variables() []string           { return []string{keyVariable} }

// OpenStore loads the AWS configuration under the SDK's credential chain, with the broker's logger
// (awssdk.LoadConfig), and reads through Secrets Manager and KMS.
func (Backend) OpenStore(ctx context.Context, cfg secretstore.Config, _ secretstore.OpenEnv) (secretstore.Opened, error) {
	awsCfg, err := awssdk.LoadConfig(ctx)
	if err != nil {
		return secretstore.Opened{}, fmt.Errorf("load the AWS configuration: %w", err)
	}
	store := Store{Client: secretsmanager.NewFromConfig(awsCfg), KMS: kms.NewFromConfig(awsCfg), KeyARN: cfg.(config).keyARN}
	return secretstore.Opened{Store: store, Credentials: "aws-sdk"}, nil
}
