package envoy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/redirects/redirectstest"
)

// TestAListenerCallFollowsNoRedirect: whatever answers at the listener's address answers a call,
// which carries the shared bearer, with a 302 to a stand-in on the same host name at another port,
// whose requests the stand-in keeps (redirectstest.Sent). net/http compares host names without
// their ports when it copies Authorization onto a redirect's request, so a client that follows
// the 302 sends the stand-in the bearer whatever the Location says. The Location quotes the bearer
// in its query (sessions, send), or names the stand-in with the call's own path and nothing more
// (moved, the red team's probe: the bearer reaches the stand-in only in the Authorization
// net/http copies). The stand-in is sent nothing, so no request carries the bearer on to it, and
// the call is ErrUnavailable on the listener's 302, a status it does not answer a call with. The
// twin shows a client that follows the redirect sends the stand-in the bearer, so the rows can see
// one sent on. Captures envoy.redirect-followed/<case>, what the stand-in was sent.
func TestAListenerCallFollowsNoRedirect(t *testing.T) {
	const token = "mh6tq-not-a-secret-RDX-c94a"
	target, sent := redirectstest.Sent(t)
	quoted := redirectstest.Echo(t, "Authorization", func(credential string) string {
		return redirectstest.To(target, strings.TrimPrefix(credential, "Bearer "))
	})
	moved := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://"+target+r.URL.RequestURI(), http.StatusFound)
	}))
	t.Cleanup(moved.Close)
	t.Run("twin", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodGet, moved.URL+"/v1/sessions", nil)
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
			t.Fatalf("a client that follows the redirect sent the stand-in no bearer, so the rows cannot see one sent on: %q", text)
		}
	})
	sessions := func(c *Client) error { _, err := c.Sessions(context.Background()); return err }
	send := func(c *Client) error {
		_, err := c.Send(context.Background(), SendInput{TargetSession: "s1", Message: "m", IdempotencyKey: "k"})
		return err
	}
	for _, c := range []struct {
		name, listener string
		call           func(*Client) error
	}{
		{"sessions", quoted, sessions},
		{"send", quoted, send},
		{"moved", moved.URL, sessions},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := c.call(New(c.listener, WithToken(token)))
			text := sent()
			redacttest.RequireNoPiece(t, "envoy.redirect-followed/"+c.name, text, token)
			if text != "" {
				t.Errorf("the stand-in the Location names was asked: %q", text)
			}
			if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "302") {
				t.Errorf("a redirected call answered %v, want ErrUnavailable on the listener's 302", err)
			}
		})
	}
}
