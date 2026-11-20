package brokertoken_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/brokertoken/brokertokentest"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestARefusedMintPrintsNoCredentialItsRefusalEchoes: something in IAM Service Account
// Credentials' place refuses the mint with words that echo the bearer the call carried, the pod's
// own access token, and quote a URL with a user and a password in it. The mint's error
// holds none of them: the refusal's body is held as GitHub's and Google's words are
// (redact.RemoteText), read by the rule with the request's credentials redacted, while its status
// and its other words print (the merge of main's generateIdToken into #4368). Capture
// brokertoken.mint-refusal.
func TestARefusedMintPrintsNoCredentialItsRefusalEchoes(t *testing.T) {
	brokertokentest.NewMetadataStand(t)
	iam := brokertokentest.NewIAMStand(t)
	// The URL's user information, joined here so no line of this file reads as a credential.
	userinfo := strings.Join([]string{redacttest.User, redacttest.Password}, ":")
	iam.AnswersStatus(http.StatusForbidden, `{"error":{"code":403,"message":"bearer `+brokertokentest.MetadataAccessBearer+
		` refused; see https://`+userinfo+`@iam.example/why for the reason","status":"PERMISSION_DENIED"}}`)
	_, err := mintingAccount(iam).Read()
	if err == nil {
		t.Fatal("the refused mint answered no error")
	}
	text := err.Error()
	for _, want := range []string{"HTTP 403", "PERMISSION_DENIED", "see https://iam.example/why for the reason"} {
		if !strings.Contains(text, want) {
			t.Errorf("the error %q does not carry %q", text, want)
		}
	}
	redacttest.RequireNone(t, "brokertoken.mint-refusal", text)
	if strings.Contains(text, brokertokentest.MetadataAccessBearer) {
		t.Errorf("REDACT-ASSERT brokertoken.mint-refusal: the output prints the access bearer the mint carried: %q", text)
	}
}
