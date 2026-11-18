// packages/envoy/cmd/agent-secrets/secret_broker_calls_test.go
//
// R5: no secret form asks the broker for a credential in order to sign in to the store. Each form
// runs on each store with the fake broker behind a recorder that notes every call it is asked.
package main

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/policy"
)

// recordBrokerCalls puts a recorder in front of the broker AGENT_SECRETS_URL names for the rest
// of t, and points AGENT_SECRETS_URL at it. It answers the calls it has passed on so far, each as
// "METHOD PATH", in order.
func recordBrokerCalls(t *testing.T) func() []string {
	t.Helper()
	target, err := url.Parse(os.Getenv("AGENT_SECRETS_URL"))
	if err != nil || target.Host == "" {
		t.Fatalf("AGENT_SECRETS_URL %q names no broker to record", os.Getenv("AGENT_SECRETS_URL"))
	}
	var mu sync.Mutex
	var calls []string
	proxy := httputil.NewSingleHostReverseProxy(target)
	recorder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(recorder.Close)
	t.Setenv("AGENT_SECRETS_URL", recorder.URL)
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), calls...)
	}
}

// TestNoSecretFormAsksTheBrokerForACredential: every secret form, on each store, asks the broker
// for its settings and for a reread and for nothing else: no request (POST /v1/requests) and no
// grant route, which is how a credential leaves the broker.
func TestNoSecretFormAsksTheBrokerForACredential(t *testing.T) {
	onEachStore(t, func(t *testing.T, store typeStore, _ *secretBroker) {
		store.put(t, "HELD_TEST", map[string]string{policy.TagOwner: "ada@example.com", policy.TagTier: policy.TierAgent}, "v1")
		calls := recordBrokerCalls(t)
		for _, form := range []struct {
			stdin string
			args  []string
		}{
			{"", []string{"list"}},
			{"", []string{"show", "HELD_TEST"}},
			{"first", []string{"create", "NEW_TEST", "--owner", "shared", "--tier", "agent"}},
			{"second", []string{"set", "HELD_TEST"}},
			{"", []string{"retag", "HELD_TEST", "--tier", "human"}},
			{"", []string{"delete", "HELD_TEST"}},
			{"", []string{"restore", "HELD_TEST"}},
		} {
			t.Run(form.args[0], func(t *testing.T) {
				before := len(calls())
				useStdin(t, form.stdin)
				if _, stderr, code := runSecret(form.args...); code != 0 {
					t.Fatalf("exit %d, stderr %q; want 0", code, stderr)
				}
				made := calls()[before:]
				if len(made) == 0 {
					t.Fatal("the form asked the broker nothing, so the recorder saw no call to judge")
				}
				var requests, grants int
				for _, call := range made {
					method, path, _ := strings.Cut(call, " ")
					switch {
					case method == http.MethodPost && path == "/v1/requests":
						requests++
					case strings.HasPrefix(path, "/v1/grants"):
						grants++
					}
					reread := method == http.MethodPost && strings.HasPrefix(path, "/v1/secrets/") && strings.HasSuffix(path, "/reread")
					if call != "GET /v1/settings" && !reread {
						t.Errorf("the form called %s, which is neither GET /v1/settings nor a reread", call)
					}
				}
				if requests != 0 || grants != 0 {
					t.Fatalf("the form made POST /v1/requests %d times and %d calls under /v1/grants, want none: %v", requests, grants, made)
				}
			})
		}
	})
}
