package notify

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// presentingTransport stands in for the daemon's broker credential (cipherclient.NewTokenTransport):
// it sets the one Authorization the publisher's every request must carry, and sends the request on.
type presentingTransport struct{ authorization string }

func (p presentingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	presented := r.Clone(r.Context())
	presented.Header.Set("Authorization", p.authorization)
	return http.DefaultTransport.RoundTrip(presented)
}

// TestNewWithTransportPresentsWhatItsTransportSets: a publisher made with a transport sends every
// call the daemon makes of the listener — the publish, the role lookup, the registration read and
// the clean stop — through that transport, so each carries the Authorization the transport sets and
// no bearer of the publisher's own. It follows no redirect: a 3xx is the listener's answer, never a
// request to another URL with the daemon's token and a proof of it. The control is New with a
// bearer: the same four calls carry `Bearer <token>`, as they always have.
func TestNewWithTransportPresentsWhatItsTransportSets(t *testing.T) {
	const presented = "DPoP daemon-broker-token"
	var mu sync.Mutex
	seen := map[string]string{}
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.Method+" "+r.URL.Path] = r.Header.Get("Authorization")
		mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			if r.URL.Path == "/v1/roles/legion-example-controller" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"holder":"ses-controller"}`))
				return
			}
			http.NotFound(w, r)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(listener.Close)

	calls := func(t *testing.T, publisher *HTTPPublisher) map[string]string {
		t.Helper()
		mu.Lock()
		clear(seen)
		mu.Unlock()
		ctx := context.Background()
		if err := publisher.Publish(ctx, RoleTopicPrefix+"legion-example-architect", "a notice", map[string]any{}, "dedupe-1"); err != nil {
			t.Fatalf("Publish: %v", err)
		}
		if holder, _, err := publisher.RoleHolder(ctx, "legion-example-controller"); err != nil || holder != "ses-controller" {
			t.Fatalf("RoleHolder = %q, %v", holder, err)
		}
		if _, err := publisher.ReadSessionRegistration(ctx, "ses-role"); err != nil {
			t.Fatalf("ReadSessionRegistration: %v", err)
		}
		if _, err := publisher.DeleteSession(ctx, "ses-role", DeleteConditions{NotRegisteredAfter: time.Now()}); err != nil {
			t.Fatalf("DeleteSession: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		return maps.Clone(seen)
	}
	want := func(authorization string) map[string]string {
		return map[string]string{
			"POST /v1/messages/publish":               authorization,
			"GET /v1/roles/legion-example-controller": authorization,
			"GET /v1/registry/ses-role":               authorization,
			"DELETE /v1/sessions/ses-role":            authorization,
		}
	}

	t.Run("through the transport", func(t *testing.T) {
		got := calls(t, NewWithTransport(listener.URL, presentingTransport{authorization: presented}))
		if !maps.Equal(got, want(presented)) {
			t.Fatalf("the listener saw %v, want every call carrying %q and nothing else", got, presented)
		}
	})
	t.Run("control: the bearer", func(t *testing.T) {
		got := calls(t, New(listener.URL, "envoy-token"))
		if !maps.Equal(got, want("Bearer envoy-token")) {
			t.Fatalf("the listener saw %v, want every call carrying the bearer", got)
		}
	})

	t.Run("a redirect is not followed", func(t *testing.T) {
		elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("the publisher followed a redirect to %s %s carrying %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}))
		t.Cleanup(elsewhere.Close)
		redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusTemporaryRedirect)
		}))
		t.Cleanup(redirecting.Close)
		if err := NewWithTransport(redirecting.URL, presentingTransport{authorization: presented}).Publish(context.Background(),
			RoleTopicPrefix+"legion-example-architect", "a notice", map[string]any{}, "dedupe-2"); err == nil {
			t.Fatal("a publish the listener answered 307 succeeded, want the 307 read as the listener's refusal")
		}
	})
}
