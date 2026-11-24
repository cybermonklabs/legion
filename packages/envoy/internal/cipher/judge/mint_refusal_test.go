// packages/envoy/internal/cipher/judge/mint_refusal_test.go
// What the judge's error says of a mint Google's place refused with words that echo a credential.
package judge

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/redact/answertest"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestTheJudgesMintRefusalPrintsNoCredentialItEchoes: something in IAM Service Account
// Credentials' place refuses the judge's mint with words that echo the bearer the mint carried,
// the pod's own access token, and quote a URL with a user and a password in it. Ask's error, which
// the audit row and the log hold, names the account, the audience, the refusal's status and its
// other words, and none of those credentials: the run names the token it could not read in the
// account's own words alone, and those hold the refusal as the rule for remote text reads it, with
// the mint's credentials redacted. The twin shows the stand answers the refusal with every one of
// them. Captures judge.mint-refusal.
func TestTheJudgesMintRefusalPrintsNoCredentialItEchoes(t *testing.T) {
	ciphertokentest.NewMetadataStand(t)
	iam := ciphertokentest.NewIAMStand(t)
	// The URL's user information, joined here so no line of this file reads as a credential.
	userinfo := strings.Join([]string{redacttest.User, redacttest.Password}, ":")
	iam.AnswersStatus(http.StatusForbidden, `{"error":{"code":403,"message":"bearer `+ciphertokentest.MetadataAccessBearer+
		` refused; see https://`+userinfo+`@iam.example/why for the reason","status":"PERMISSION_DENIED"}}`)
	t.Run("twin", func(t *testing.T) {
		resp, err := http.Post(iam.Server.URL+"/v1/projects/-/serviceAccounts/"+judgeAccount+":generateIdToken", "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := answertest.Read(resp)
		if err != nil {
			t.Fatal(err)
		}
		for _, echoed := range []string{redacttest.User, redacttest.Password, ciphertokentest.MetadataAccessBearer} {
			if !strings.Contains(string(body), echoed) {
				t.Fatalf("the stand's refusal %q does not echo %q, so the row cannot see a leak", body, echoed)
			}
		}
	})
	t.Run("ask", func(t *testing.T) {
		model := newModel(t, verdictReply(Grant, "The reason names the issue."))
		account := ciphertoken.NewGoogleAccount(gatewayAudience)
		account.ServiceAccount = judgeAccount
		account.IAMCredentialsURL = iam.Server.URL
		verdict, _, _, err := tokenModel(model, account).Ask(t.Context(), tokenData())
		if verdict.Word != Deny || err == nil {
			t.Fatalf("Ask = %+v, %v; want no verdict but an error", verdict, err)
		}
		text := err.Error()
		redacttest.RequireNoPiece(t, "judge.mint-refusal", text, ciphertokentest.MetadataAccessBearer)
		for _, want := range []string{judgeAccount, gatewayAudience, "HTTP 403", "PERMISSION_DENIED", "see https://iam.example/why for the reason"} {
			if !strings.Contains(text, want) {
				t.Errorf("the judge's error %q does not name %q", text, want)
			}
		}
		if got := model.asked(); got != 0 {
			t.Errorf("the model got %d calls; want none without a token", got)
		}
	})
}
