package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/testwait"
)

func TestRoleHolderLookupUsesTheServersClock(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		expire bool
	}{{"answer before virtual deadline", false}, {"virtual deadline before answer", true}} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			fake := clocktest.New(start)
			entered := make(chan struct{}, 1)
			release := make(chan struct{})
			listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/v1/roles/reviewer" {
					http.NotFound(w, r)
					return
				}
				entered <- struct{}{}
				select {
				case <-release:
					_, _ = w.Write([]byte(`{"role":"reviewer","holder":"s2","title":"reviewer","capabilities":["steer"]}`))
				case <-r.Context().Done():
				}
			}))
			t.Cleanup(listener.Close)
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
			})
			const clientWindow = 5 * time.Second
			s := &server{deps: Deps{
				Clock: fake,
				Envoy: envoy.New(listener.URL,
					envoy.WithClock(fake), envoy.WithTimeout(clientWindow),
					envoy.WithAccountTokenFile(testEnvoyAccountTokenFile(t, listener.URL))),
			}}
			result := make(chan bool, 1)
			go func() { result <- s.roleHolderMayBe(ctx, "reviewer", "s1") }()
			testwait.Until(t, "the listener to receive the role lookup", func() (bool, bool) {
				select {
				case <-entered:
					return true, true
				default:
				}
				select {
				case got := <-result:
					t.Fatalf("role lookup returned %v before the held listener received it", got)
				default:
				}
				return false, false
			})
			armed := fake.Armed()
			if !slices.ContainsFunc(armed, start.Add(mentionLookupTimeout).Equal) {
				t.Fatalf("role lookup did not arm its %s bound on the server clock: %v", mentionLookupTimeout, armed)
			}
			if !slices.ContainsFunc(armed, start.Add(clientWindow).Equal) {
				t.Fatalf("listener client did not arm its distinct %s window: %v", clientWindow, armed)
			}
			if tc.expire {
				fake.Advance(mentionLookupTimeout)
			} else {
				close(release)
			}
			if got := testwait.Receive(t, "the role-holder decision", result); got != tc.expire {
				t.Fatalf("role may be s1 = %v, want %v (a timed-out read may hold it; the answered s2 lookup may not)", got, tc.expire)
			}
		})
	}
}
