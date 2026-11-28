package aws

import (
	"context"
	"fmt"
	"strings"

	sdkaws "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/sjawhar/envoy/awsbody"
	"github.com/sjawhar/envoy/internal/cipher/awssdk"
	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/ratelimit"
)

// Backend is the AWS store's registry entry.
type Backend struct{}

func (Backend) Name() string { return Name }
func (Backend) ID() string   { return "aws" }

// ServiceSDKs are the AWS SDK's service packages this store alone imports: Secrets Manager and
// KMS, which hold and encrypt the secrets, and STS, SSO and SSO OIDC, the writer's sign-in.
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
// (awssdk.LoadConfig), and reads through Secrets Manager and KMS (storeOn).
func (Backend) OpenStore(ctx context.Context, cfg secretstore.Config) (secretstore.Opened, error) {
	awsCfg, err := awssdk.LoadConfig(ctx)
	if err != nil {
		return secretstore.Opened{}, fmt.Errorf("load the AWS configuration: %w", err)
	}
	return secretstore.Opened{Store: storeOn(awsCfg, cfg.(config).keyARN), Credentials: "aws-sdk"}, nil
}

// storeOn is the store over awsCfg on the agent-secrets key keyARN: its Secrets Manager and KMS
// clients send each request through awsbody.Client, so an answered call over HTTP/1.1 is not
// failed by smithy-go closing its request body early (the awsbody package comment).
func storeOn(awsCfg sdkaws.Config, keyARN string) Store {
	return Store{
		Client: secretsmanager.NewFromConfig(awsCfg, func(o *secretsmanager.Options) { o.HTTPClient = awsbody.Client(o.HTTPClient) }),
		KMS:    kms.NewFromConfig(awsCfg, func(o *kms.Options) { o.HTTPClient = awsbody.Client(o.HTTPClient) }),
		KeyARN: keyARN,
	}
}

// Kind is the store's kind as the CLI's lines name it.
func (Backend) Kind() string { return "AWS Secrets Manager" }

// Place is the account and region the broker's settings put the agent secrets in, as a line names
// them after Kind: "account <id> in <region>", "account <id>" or "in <region>", "" where they
// name neither; each as the broker's address lets its answer print (Settings.Words).
func (Backend) Place(s secretstore.Settings) string {
	settings := settingsOf(s)
	var place []string
	if settings.AWSAccountID != "" {
		place = append(place, "account "+settings.words(settings.AWSAccountID))
	}
	if settings.AWSRegion != "" {
		place = append(place, "in "+settings.words(settings.AWSRegion))
	}
	return strings.Join(place, " ")
}

// OpenWriter refuses settings whose account or region is not the key ARN's
// (checkSettingsKeyARN), then loads the sign-in the command's environment gives, pinned to the key
// ARN's region (Hooks.Clients). It makes no AWS call: RequireAccount, which every form calls
// first, is the writer's first.
func (Backend) OpenWriter(ctx context.Context, s secretstore.Settings, _ secretstore.WriterOptions) (secretstore.Writer, error) {
	settings := settingsOf(s)
	if err := checkSettingsKeyARN(settings); err != nil {
		return nil, err
	}
	sm, st, sources, err := hooks.Clients(ctx, settings.AWSRegion)
	if err != nil {
		return nil, err
	}
	return newWriter(settings, sm, st, sources), nil
}

// HelpRows are the AWS variables of the CLI's environment table.
func (Backend) HelpRows() string {
	return `  AWS_PROFILE                the AWS profile the secret forms sign in with, against a broker on
                             AWS; access keys in AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY, and
                             AWS_WEB_IDENTITY_TOKEN_FILE, outrank it, as in the AWS CLI, and
                             without it the AWS SDK's default credential chain signs in; every
                             secret form needs a sign-in in the broker's account, and the
                             store's IAM decides what it may do there
  AWS_DEFAULT_PROFILE        the AWS profile the secret forms sign in with where AWS_PROFILE is
                             unset; a profile either names that the AWS config does not hold
                             stops the command
`
}

// ProfileHint is the AWS clause of the CLI's --profile refusal.
func (Backend) ProfileHint() (secretstore.ProfileHint, bool) {
	return secretstore.ProfileHint{Rank: 20, Clause: "on AWS set AWS_PROFILE, which access keys in AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY, and AWS_WEB_IDENTITY_TOKEN_FILE, outrank when set, as in the AWS CLI"}, true
}
