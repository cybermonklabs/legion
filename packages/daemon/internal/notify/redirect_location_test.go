package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/redirects/redirectstest"
)

// TestARedirectIsTheListenersAnswerAndNoLocationReachesTheError: whatever answers at the Envoy
// listener's address answers each of the publisher's four calls with a redirect whose Location
// quotes the bearer the call carried. One net/http cannot parse (unparsed) is net/http's error
// whatever the client's redirect policy; one that parses, to a stand-in that answers (followed),
// was followed. No call's error holds a run of the bearer or a word of the Location, and the
// stand-in a Location names is asked nothing: the publisher follows no redirect, so a 3xx is the
// listener's answer, refused by its status. The twin shows the stand-in's echo reaches net/http's
// own error; plain shows a 200 still publishes. Captures notify.redirect/<call>-<case>.
func TestARedirectIsTheListenersAnswerAndNoLocationReachesTheError(t *testing.T) {
	const token = "zq7wk-not-a-secret-RDX-41c7"
	target, asked := redirectstest.Target(t)
	t.Run("twin", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodGet, redirectstest.Echo(t, "Authorization", redirectstest.Unparsed)+"/v1/roles/architect", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			_ = response.Body.Close()
			t.Fatal("net/http answered the unparsed Location with no error: the rows cannot see one")
		}
		if !strings.Contains(err.Error(), token) {
			t.Fatalf("net/http's own error holds no echo of the bearer, so the rows cannot see a leak: %q", err)
		}
	})
	calls := map[string]func(*HTTPPublisher) error{
		"publish": func(p *HTTPPublisher) error {
			return p.Publish(context.Background(), "notifications.role.architect", "a notice", map[string]string{"issue": "LEGION-9"}, "dedupe-1")
		},
		"registration": func(p *HTTPPublisher) error {
			_, err := p.ReadSessionRegistration(context.Background(), "ses_architect")
			return err
		},
		"role": func(p *HTTPPublisher) error {
			_, _, err := p.RoleHolder(context.Background(), "architect")
			return err
		},
		"delete": func(p *HTTPPublisher) error {
			_, err := p.DeleteSession(context.Background(), "ses_architect", DeleteConditions{NotRegisteredAfter: time.Unix(1700000000, 0)})
			return err
		},
	}
	for call, do := range calls {
		for _, c := range []struct {
			name     string
			location func(credential string) string
		}{
			{"unparsed", redirectstest.Unparsed},
			{"followed", func(credential string) string {
				return redirectstest.To(target, strings.TrimPrefix(credential, "Bearer "))
			}},
		} {
			t.Run(call+"-"+c.name, func(t *testing.T) {
				err := do(New(redirectstest.Echo(t, "Authorization", c.location), token))
				if err == nil {
					t.Fatal("the call answered no error")
				}
				oauth2calltest.RequireNoPiece(t, "notify.redirect/"+call+"-"+c.name, err.Error(), token)
				if strings.Contains(err.Error(), "moved") {
					t.Errorf("the call's error names the Location: %q", err)
				}
				if n := asked(); n != 0 {
					t.Errorf("the redirect's target was asked %d times, want none", n)
				}
			})
		}
	}
	t.Run("plain", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()
		if err := calls["publish"](New(server.URL, token)); err != nil {
			t.Fatalf("a plain 200 = %v, want the notice published", err)
		}
	})
}
