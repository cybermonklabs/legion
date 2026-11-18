package appauth

import (
	"crypto"
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

// anAppCredential is the githubtest.Spec.Credential of a stand-in GitHub a Manager whose App key
// is key mints against: an App JWT key signed, as each App route's request carries it, or an
// installation token in GitHub's own `token` scheme, as its read of the App's bot user carries the
// token the mint before it was answered. A request without one is another process's, which
// githubtest answers 404 before the handler records or fails anything. It is internal/daemon's
// anAppCredential.
func anAppCredential(key *rsa.PublicKey) func(*http.Request) bool {
	return func(r *http.Request) bool {
		authorization := r.Header.Get("Authorization")
		if token, installation := strings.CutPrefix(authorization, "token "); installation {
			return token != ""
		}
		jwt, bearer := strings.CutPrefix(authorization, "Bearer ")
		dot := strings.LastIndexByte(jwt, '.')
		if !bearer || dot < 0 {
			return false
		}
		signature, err := base64.RawURLEncoding.DecodeString(jwt[dot+1:])
		digest := sha256.Sum256([]byte(jwt[:dot]))
		return err == nil && rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature) == nil
	}
}

// anInstallationToken is the githubtest.Spec.Credential of token mode's stand-in GitHub: an
// installation token of the fake broker's release (ghs_…) in GitHub's `token` scheme, which every
// read token mode makes carries.
func anInstallationToken(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get("Authorization"), "token ghs_")
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

// standInRequest sends GET url, with authorization as its Authorization ("" for none), and
// answers the status the stand-in gave.
func standInRequest(t *testing.T, url, authorization string) int {
	t.Helper()
	request, err := http.NewRequestWithContext(testwait.Context(t), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("send GET %s: %v", url, err)
	}
	_ = response.Body.Close()
	return response.StatusCode
}

// metadataIdentity is the request a helper of the machine sent to a reused loopback port, which
// failed a test of the daemon (dispatch://INFRA-3904): a read of its metadata server's identity
// token, carrying no credential of the Manager's.
const metadataIdentity = "/computeMetadata/v1/instance/service-accounts/default/identity"

// A stand-in GitHub of this package answers a request that carries no credential of the
// Manager's 404 and fails nothing, while one that carries the Manager's credential to a path it
// does not model still fails the test. The first half sends the metadata read of
// dispatch://INFRA-3904 to the key mode's and the token mode's stand-ins, each run under this
// test itself, so a failure it reported would fail this test. The second half runs the key mode's
// stand-in under failures, which reads what it reported: nothing for the read without a
// credential, and the read under the App's own JWT of a path it does not model.
func TestAStandInFailsItsTestOnlyOnTheManagersOwnRequests(t *testing.T) {
	key, privatePEM := testAppKey(t)
	for _, row := range []struct{ name, url string }{
		{"the key mode's GitHub", (&keyModeGitHub{slug: "legion-implementer"}).serve(t, &key.PublicKey).URL},
		{"the token mode's GitHub", (&fakeTokenGitHub{slug: "legion-implementer", appID: "12345"}).serve(t).URL},
	} {
		t.Run(row.name, func(t *testing.T) {
			if status := standInRequest(t, row.url+metadataIdentity, ""); status != http.StatusNotFound {
				t.Errorf("GET %s without the Manager's credential: status %d, want 404", metadataIdentity, status)
			}
		})
	}

	recorded := &failures{TB: t}
	github := &keyModeGitHub{slug: "legion-implementer"}
	url := github.serve(recorded, &key.PublicKey).URL
	if status := standInRequest(t, url+metadataIdentity, ""); status != http.StatusNotFound {
		t.Errorf("GET %s without the Manager's credential: status %d, want 404", metadataIdentity, status)
	}
	if got := recorded.reported(); len(got) != 0 {
		t.Fatalf("a request without the Manager's credential made the stand-in report %q, want nothing", got)
	}
	jwt, err := GenerateJWT("12345", privatePEM, time.Now())
	if err != nil {
		t.Fatalf("GenerateJWT: %v", err)
	}
	standInRequest(t, url+"/app/hooks", "Bearer "+jwt)
	if got, want := recorded.reported(), []string{"unexpected GitHub path /app/hooks"}; !slices.Equal(got, want) {
		t.Fatalf("a request under the App's JWT that the stand-in does not expect made it report %q, want %q", got, want)
	}
	if reads := github.permissionReads(); len(reads) != 0 {
		t.Fatalf("the stand-in recorded the permission reads %q, want none", reads)
	}
}
