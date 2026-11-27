package dispatchread_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/dispatchread"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/redirects/redirectstest"
)

// TestADispatchReadFollowsNoRedirect: whatever answers at Dispatch's address answers the broker's
// read, which carries its project-access token, with a 302, a 307 or a 308 whose Location names a
// stand-in on another host and quotes the token in its query. Under the default client (the nil
// cmd/cipherd passes for the repository read) and a client its caller gives (the project access
// read's), the stand-in is sent nothing, so no request carries the token on to it, and the read
// hands back Dispatch's 3xx by its status, which every caller refuses as a status other than 200.
// The twins show a client that follows each of them sends the stand-in the token, so the rows can
// see one sent on. Captures dispatchread.redirect-followed/<client> (the 302) and
// dispatchread.redirect-followed/<client>-<status>, what the stand-in was sent.
func TestADispatchReadFollowsNoRedirect(t *testing.T) {
	const token = "pn5vy-not-a-secret-RDX-0c4b"
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	target, sent := redirectstest.Sent(t)
	location := func(bearer string) string { return redirectstest.To(target, bearer) }
	// moved is the stand-in at Dispatch's address for the 307 and 308 rows:
	// redirectstest.EchoBearer's, answering status where that one answers 302.
	moved := func(t *testing.T, status int) string {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", location(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")))
			w.WriteHeader(status)
		}))
		t.Cleanup(server.Close)
		return server.URL
	}
	const path = "/api/v1/access/projects/PRIV/repositories"
	t.Run("twin", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodGet, redirectstest.EchoBearer(t, location)+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		redirectstest.RequireSentOn(t, request, token, sent)
	})
	clients := []struct {
		name   string
		client *http.Client
	}{
		{"default", nil},
		{"given", &http.Client{}},
	}
	for _, c := range clients {
		t.Run(c.name, func(t *testing.T) {
			reader := dispatchread.New(redirectstest.EchoBearer(t, location), ciphertoken.NewAccountToken("BROKER_PROJECT_ACCESS_TOKEN_FILE", file), c.client, 10*time.Second, 1<<20)
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
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		code := strconv.Itoa(status)
		t.Run("twin-"+code, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodGet, moved(t, status)+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+token)
			redirectstest.RequireSentOn(t, request, token, sent)
		})
		for _, c := range clients {
			t.Run(c.name+"-"+code, func(t *testing.T) {
				reader := dispatchread.New(moved(t, status), ciphertoken.NewAccountToken("BROKER_PROJECT_ACCESS_TOKEN_FILE", file), c.client, 10*time.Second, 1<<20)
				answer, err := reader.Get(t.Context(), path, nil)
				text := sent()
				redacttest.RequireNoPiece(t, "dispatchread.redirect-followed/"+c.name+"-"+code, text, token)
				if text != "" {
					t.Errorf("the stand-in the Location names was asked: %q", text)
				}
				if err != nil || answer.Status != status {
					t.Errorf("Get = %d, %v, want Dispatch's %d handed back by its status", answer.Status, err, status)
				}
			})
		}
	}
}
