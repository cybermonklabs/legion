// Package embedtest holds fakes shared by this module's own tests and its callers' tests, kept
// out of the embed package proper so no test-only type ships in the production binary.
package embedtest

import (
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
)

// FakeThrottleError stands in for a model service's throttle/capacity error, as a client hands it
// back: it carries the Throttled() marker embed.IsThrottled reads, so a caller's test means
// "the provider said back off" without a live call. Code names the condition in the error's text.
// Shared by embed's own tests, embedqueue's, and api's - before this, two independent copies were
// hardcoded to "ThrottlingException" alone and a third, parameterized one in embed_test.go
// duplicated the same handful of lines a third time.
type FakeThrottleError struct{ Code string }

func (e FakeThrottleError) Error() string   { return "simulated: " + e.Code }
func (e FakeThrottleError) Throttled() bool { return true }

// BedrockConfig is the AWS configuration a test's BedrockClient is built from
// (embed.NewBedrockWithConfig): a fake region, static fake credentials and the SDK's own retries
// off. The SDK still signs every request (SigV4), but nothing on a stand-in Bedrock's side checks
// the signature, and a test that answers an error sees it on the first call rather than after the
// SDK's backoff. It returns the configuration rather than a client because embed's own tests use
// it, and this package cannot import embed.
func BedrockConfig() aws.Config {
	return aws.Config{
		Region:      "us-west-2",
		Credentials: credentials.NewStaticCredentialsProvider("test-access-key", "test-secret-key", ""),
		Retryer:     func() aws.Retryer { return aws.NopRetryer{} },
	}
}
