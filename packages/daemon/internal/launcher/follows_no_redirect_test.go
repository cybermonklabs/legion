package launcher

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/envoy/redirects/redirectstest"
)

// TestATokenExchangeFollowsNoRedirect: whatever answers at the security token service's address
// answers the endpoint's exchange, whose form carries the pod's token as its subject token, with
// a 302 or a 307 to a stand-in on the same host name at another port, whose requests the stand-in
// keeps (redirectstest.Sent), and a Location that quotes the subject token in its query. A client
// that follows the 302 asks that Location, and one that follows the 307 sends the form there
// again. The stand-in is sent nothing, so no request carries the token on to it; the token
// request is answered 500 with the endpoint's short text, and the failure line names the 3xx and
// no Location. The twin shows a client that follows the redirect sends the stand-in the token, so
// the rows can see one sent on. Captures launcher.redirect-followed/<status>, what the stand-in
// was sent.
func TestATokenExchangeFollowsNoRedirect(t *testing.T) {
	const token = "k7v2p-not-a-secret-RDX-f3h8"
	target, sent := redirectstest.Sent(t)
	statuses := []int{http.StatusFound, http.StatusTemporaryRedirect}
	// sts is the stand-in at the service's address: it answers every exchange status, with the
	// Location quoting the subject token the exchange's form carried.
	sts := func(t *testing.T, status int) string {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, redirectstest.To(target, r.PostFormValue("subject_token")), status)
		}))
		t.Cleanup(server.Close)
		return server.URL
	}
	t.Run("twin", func(t *testing.T) {
		for _, status := range statuses {
			response, err := http.PostForm(sts(t, status), url.Values{"subject_token": {token}})
			if err != nil {
				t.Fatalf("a client that follows the %d failed: %v", status, err)
			}
			_ = response.Body.Close()
			if text := sent(); !strings.Contains(text, token) {
				t.Fatalf("a client that follows the %d sent the stand-in no token, so the rows cannot see one sent on: %q", status, text)
			}
		}
	})
	for _, status := range statuses {
		name := strconv.Itoa(status)
		t.Run(name, func(t *testing.T) {
			tokenFile := filepath.Join(t.TempDir(), WorkloadIdentityTokenFile)
			writeSubjectToken(t, tokenFile, token)
			log := &lockedBuffer{}
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			endpoint := httptest.NewServer(newMetadataServer(ctx, MetadataConfig{
				WorkloadPool: testWorkloadPool, IdentityProvider: testIdentityProvider, tokenFile: tokenFile, tokenURL: sts(t, status),
			}, func(format string, args ...any) { _, _ = fmt.Fprintf(log, format+"\n", args...) }))
			t.Cleanup(endpoint.Close)
			response, body := getMetadata(t, endpoint.URL, tokenPath, flavor)
			text := sent()
			oauth2calltest.RequireNoPiece(t, "launcher.redirect-followed/"+name, text, token)
			if text != "" {
				t.Errorf("the stand-in the Location names was asked: %q", text)
			}
			if response.StatusCode != http.StatusInternalServerError || body != "token exchange failed\n" {
				t.Errorf("a redirected exchange answered %d %q, want 500 %q", response.StatusCode, redacted(body), "token exchange failed\n")
			}
			if logged := log.String(); !strings.Contains(logged, "status code "+name) || strings.Contains(logged, target) {
				t.Errorf("the failure line is %q, want it to name the service's %d and no Location", logged, status)
			}
		})
	}
}
