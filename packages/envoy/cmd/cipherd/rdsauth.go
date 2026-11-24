package main

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/rds/auth"

	"github.com/sjawhar/envoy/internal/cipher/store"
	"github.com/sjawhar/envoy/internal/redact"
)

// rdsAuthTokens is the store.TokenMinter of RDS IAM database authentication: an auth token, an
// rds-db connect request for the endpoint and user presigned with awsCfg's credentials in its
// region, good for 15 minutes. It is signed locally, a credential refresh aside, so a token per
// new connection costs no call to RDS. The credentials need rds-db:connect on the database user.
// A config with no region is refused, since RDS refuses a token signed for none at every sign-in. A
// mint that fails prints through redact.AWS: what fails is the credential refresh, the SDK's call
// to STS, a container's credentials endpoint or the instance metadata service with the credential
// that call carries (a web identity token, an authorization token), whose error quotes net/http's
// (a Location whatever answers there can write that credential into) and the answer's message.
func rdsAuthTokens(awsCfg aws.Config) (store.TokenMinter, error) {
	if awsCfg.Region == "" {
		return nil, errors.New("signing in to the database by RDS IAM token needs an AWS region (AWS_REGION, AWS_DEFAULT_REGION or the shared AWS config), and none is set")
	}
	return func(ctx context.Context, endpoint, user string) (string, error) {
		token, err := auth.BuildAuthToken(ctx, endpoint, awsCfg.Region, user, awsCfg.Credentials)
		return token, redact.AWS(err)
	}, nil
}
