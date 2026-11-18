package intake_test

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// signedByTheApp is the githubtest.Spec.Credential of the App's stand-in GitHub: a bearer whose
// RS256 signature key verifies, the credential the redeliverer under test signs every App request
// with. A request without one is another process's; the claims of a signed one are the handler's
// to judge (verifyAppJWT).
func signedByTheApp(key *rsa.PublicKey) func(*http.Request) bool {
	return func(r *http.Request) bool {
		jwt, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		dot := strings.LastIndexByte(jwt, '.')
		if !bearer || dot < 0 {
			return false
		}
		signature, err := base64.RawURLEncoding.DecodeString(jwt[dot+1:])
		digest := sha256.Sum256([]byte(jwt[:dot]))
		return err == nil && rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature) == nil
	}
}

// failures is a testing.TB that keeps each failure a stand-in reports with Errorf instead of
// failing the test it runs in, so a test reads whether a request failed it.
type failures struct {
	testing.TB
	mu     sync.Mutex
	failed []string
}

func (f *failures) Errorf(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failed = append(f.failed, fmt.Sprintf(format, args...))
}

// reported is every failure kept so far.
func (f *failures) reported() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.failed)
}

// The App's stand-in GitHub answers a request without a JWT its key signed 404 and fails nothing,
// while one the key signed with claims GitHub refuses still fails the test. It runs under
// failures, which reads what it reported: nothing for the listing asked with no credential, with
// another process's bearer and with a JWT another key signed; and the listing under the App's key
// for another client id.
func TestAStandInFailsItsTestOnlyOnTheRedeliverersOwnRequests(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	recorded := &failures{TB: t}
	github := newFakeGitHub(recorded, &key.PublicKey, "Iv1.the-apps-client-id", "http://127.0.0.1:1/webhook/github")
	sign := func(signer *rsa.PrivateKey, issuer string) string {
		encode := func(part string) string { return base64.RawURLEncoding.EncodeToString([]byte(part)) }
		signed := encode(`{"alg":"RS256","typ":"JWT"}`) + "." + encode(fmt.Sprintf(`{"iss":%q,"exp":%d}`, issuer, time.Now().Add(5*time.Minute).Unix()))
		digest := sha256.Sum256([]byte(signed))
		signature, err := rsa.SignPKCS1v15(rand.Reader, signer, crypto.SHA256, digest[:])
		if err != nil {
			t.Fatal(err)
		}
		return signed + "." + base64.RawURLEncoding.EncodeToString(signature)
	}
	send := func(authorization string) int {
		request, err := http.NewRequestWithContext(testwait.Context(t), http.MethodGet, github.url+"/app/hook/deliveries", nil)
		if err != nil {
			t.Fatal(err)
		}
		if authorization != "" {
			request.Header.Set("Authorization", authorization)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("GET /app/hook/deliveries: %v", err)
		}
		_ = response.Body.Close()
		return response.StatusCode
	}
	for _, stranger := range []string{"", "Bearer another-process-token-not-a-secret", "Bearer " + sign(other, "Iv1.the-apps-client-id")} {
		if status := send(stranger); status != http.StatusNotFound {
			t.Errorf("the listing with Authorization %.40q: status %d, want 404", stranger, status)
		}
	}
	if got := recorded.reported(); len(got) != 0 {
		t.Fatalf("requests without a JWT the App's key signed made the stand-in report %q, want nothing", got)
	}
	send("Bearer " + sign(key, "Iv1.another-client-id"))
	if got := recorded.reported(); len(got) != 1 || !strings.Contains(got[0], `JWT iss "Iv1.another-client-id"`) {
		t.Fatalf("a JWT the App's key signed for another client id made the stand-in report %q, want its claims refused", got)
	}
}
