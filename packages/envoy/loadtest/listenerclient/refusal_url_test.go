package listenerclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/listenerwire"
)

// TestARefusalNamesItsRouteWithNoCredentialTheListenerURLCarries: a listener refuses a publish.
// The refusal names the route's URL whole when the listener's URL is clean (clean), and none of
// a listener URL whose user information and query carry redacttest's credentials
// (credentials), where it printed the URL as the request was sent; the twin shows such a request,
// sent with no bearer, carries them all, as Basic credentials and in its query. Captures
// listenerclient.refusal-url/credentials.
func TestARefusalNamesItsRouteWithNoCredentialTheListenerURLCarries(t *testing.T) {
	var mu sync.Mutex
	var carried []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, _ := r.BasicAuth()
		mu.Lock()
		carried = append(carried, user, password, r.URL.RawQuery)
		mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	t.Cleanup(srv.Close)
	credentialed := strings.Replace(srv.URL, "://", "://"+redacttest.User+":"+redacttest.Password+"@", 1) + "/listener?access" + "_token=" + redacttest.Token
	publish := func(t *testing.T, base, token string) string {
		t.Helper()
		err := (&Client{URL: base, Token: token, HTTP: srv.Client()}).Publish(context.Background(), listenerwire.PublishRequest{Topic: "notifications.team.a"})
		var refusal *Refusal
		if !errors.As(err, &refusal) || refusal.Status != http.StatusUnauthorized {
			t.Fatalf("Publish = %v; want the stand-in's 401 as a Refusal", err)
		}
		return err.Error()
	}
	t.Run("clean", func(t *testing.T) {
		if said, want := publish(t, srv.URL+"/listener", "join-bearer"), srv.URL+"/listener/v1/messages/publish: 401 "; !strings.HasPrefix(said, want) {
			t.Errorf("the refusal is %q; want it to begin %q", said, want)
		}
	})
	t.Run("twin", func(t *testing.T) {
		mu.Lock()
		carried = nil
		mu.Unlock()
		publish(t, credentialed, "")
		mu.Lock()
		defer mu.Unlock()
		if held := redacttest.Held(strings.Join(carried, " ")); len(held) != len(redacttest.Secrets) {
			t.Errorf("the refused request carried %q of the credentials; want all of %q, so the row can see them printed", held, redacttest.Secrets)
		}
	})
	t.Run("credentials", func(t *testing.T) {
		said := publish(t, credentialed, "join-bearer")
		redacttest.RequireNoPiece(t, "listenerclient.refusal-url/credentials", said)
		if want := redact.NotPrinted + ": 401 "; !strings.HasPrefix(said, want) {
			t.Errorf("the refusal is %q; want it to begin %q", said, want)
		}
	})
}
