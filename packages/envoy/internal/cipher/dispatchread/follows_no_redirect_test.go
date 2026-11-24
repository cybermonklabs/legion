package dispatchread_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/dispatchread"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/redirects/redirectstest"
)

// TestADispatchReadFollowsNoRedirect: whatever answers at Dispatch's address answers the broker's
// read, which carries its project-access token, with a 302 whose Location names a stand-in on
// another host and quotes the token in its query. Under the default client (the nil cmd/cipherd
// passes for the repository read) and a client its caller gives (the project access read's), the
// stand-in is sent nothing, so no request carries the token on to it, and the read hands back
// Dispatch's 302 by its status, which every caller refuses as a status other than 200. The twin
// shows a client that follows the redirect sends the stand-in the token, so the row can see one
// sent on. Captures dispatchread.redirect-followed/<client>, what the stand-in was sent.
func TestADispatchReadFollowsNoRedirect(t *testing.T) {
	const token = "pn5vy-not-a-secret-RDX-0c4b"
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	target, sent := redirectstest.Sent(t)
	location := func(credential string) string {
		return redirectstest.To(target, strings.TrimPrefix(credential, "Bearer "))
	}
	const path = "/api/v1/access/projects/PRIV/repositories"
	t.Run("twin", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodGet, redirectstest.Echo(t, "Authorization", location)+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("a client that follows the redirect failed: %v", err)
		}
		_ = response.Body.Close()
		if text := sent(); !strings.Contains(text, token) {
			t.Fatalf("a client that follows the redirect sent the stand-in no token, so the row cannot see one sent on: %q", text)
		}
	})
	for _, c := range []struct {
		name   string
		client *http.Client
	}{
		{"default", nil},
		{"given", &http.Client{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			reader := dispatchread.New(redirectstest.Echo(t, "Authorization", location), ciphertoken.NewAccountToken(file), c.client, 10*time.Second, 1<<20)
			answer, err := reader.Get(t.Context(), path, nil)
			text := sent()
			redacttest.RequireNoPiece(t, "dispatchread.redirect-followed/"+c.name, text, token)
			if text != "" {
				t.Errorf("the stand-in the Location names was asked: %q", text)
			}
			if err != nil || answer.Status != http.StatusFound {
				t.Errorf("Get = %d, %v, want Dispatch's 302 handed back by its status", answer.Status, err)
			}
		})
	}
}
