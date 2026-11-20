package launcher

import (
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
)

// The probe's made-up credentials, and the parameter an access token rides under, in two parts so
// no line of this file reads as a credential to a scanner.
const (
	probeUser     = "not-a-secret-user"
	probePassword = "not-a-secret-password"
	probeToken    = "not-a-secret-token"
)

var probeWords = "the issuer at https://" + probeUser + ":" + probePassword + "@issuer.example/path?" +
	strings.Join([]string{"access", "token"}, "_") + "=" + probeToken

// TestAFailedExchangesLogLinePrintsNoCredentialTheServicesWordsQuote: the security token service
// refuses the exchange with words that quote a URL carrying a user, a password and an access
// token. The one line the endpoint logs for the failure names the service's answer only as the
// rule for remote text reads it (redact.OAuth2, through oauth2call.Call), and none of the three.
// It prints the line it read as `REDACT-CAPTURE launcher.exchange-failed <base64>` and fails with
// a message that begins `REDACT-ASSERT launcher.exchange-failed`, as the redaction check's rows do.
func TestAFailedExchangesLogLinePrintsNoCredentialTheServicesWordsQuote(t *testing.T) {
	g := newMetadataRig(t)
	g.sts.refuseWith(probeWords)
	if response, body := g.get(t, tokenPath, flavor); response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("a refused exchange answered %d %q, want 500", response.StatusCode, redacted(body))
	}
	logged := g.log.String()
	if !strings.Contains(logged, "launcher: metadata: token exchange failed ") {
		t.Fatalf("the endpoint logged %q, want its failure line", logged)
	}
	const capture = "launcher.exchange-failed"
	t.Logf("REDACT-CAPTURE %s %s", capture, base64.StdEncoding.EncodeToString([]byte(logged)))
	for _, secret := range []string{probeUser, probePassword, probeToken} {
		if strings.Contains(logged, secret) {
			t.Errorf("REDACT-ASSERT %s: the output prints the secret %q: %q", capture, secret, logged)
		}
	}
}

// TestAFailedExchangesLogLinePrintsNoCredentialItsRefusalEchoes: something in the security token
// service's place refuses the exchange with words that echo the subject token the exchange sent.
// The endpoint's failure line holds none of it: the exchange's client records what each refused
// request carried (oauth2call.RefusalClient), and the boundary reads the library's error with it
// redacted (the review of #4368 at db7e4a0226, finding 3). Capture launcher.exchange-echo.
func TestAFailedExchangesLogLinePrintsNoCredentialItsRefusalEchoes(t *testing.T) {
	g := newMetadataRig(t)
	writeSubjectToken(t, g.tokenFile, probeToken)
	g.sts.refuseWith("subject_token " + probeToken + " was not accepted")
	g.get(t, tokenPath, flavor)
	logged := g.log.String()
	if !strings.Contains(logged, "launcher: metadata: token exchange failed ") {
		t.Fatalf("the endpoint logged %q, want its failure line", logged)
	}
	const capture = "launcher.exchange-echo"
	t.Logf("REDACT-CAPTURE %s %s", capture, base64.StdEncoding.EncodeToString([]byte(logged)))
	if strings.Contains(logged, probeToken) {
		t.Errorf("REDACT-ASSERT %s: the output prints the secret %q: %q", capture, probeToken, logged)
	}
}
