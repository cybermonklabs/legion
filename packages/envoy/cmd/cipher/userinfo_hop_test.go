package main

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/redirects/redirectstest"
)

// userinfoBearer is the sign-in's access token the userinfo rows read with. It holds the gate's
// marker -RDX-, so the text a row captures shows it where a stand-in was sent it.
const userinfoBearer = "lent-k7v2p-not-a-secret-RDX-f3h8"

// TestTheUserinfoReadFollowsNoRedirect: whatever answers the userinfo read --owner me makes
// (gcpUserinfo) in Google's place answers it 302 to a stand-in that keeps what it is sent
// (redirectstest.Sent), at the read's own path: on Google's host name at the stand-in's port
// (other-port), and at the stand-in's own address (other-host). oauth2.Transport sets the
// sign-in's bearer on every request it carries, a redirect's among them, whatever its host, and
// oauth2.NewClient copies the redirect rule of the context's client, http.DefaultClient's, which
// follows; so a followed 302 sends the stand-in the bearer. The read follows no redirect
// (redirects.Refused): the stand-in is sent nothing, and the read is refused with the 302's
// status. The twin shows a client oauth2.NewClient builds over http.DefaultClient follows the 302
// and sends the stand-in the bearer, so the rows can see one sent on. Captures
// agent-secrets.redirect-followed/<case>, what the stand-in was sent.
func TestTheUserinfoReadFollowsNoRedirect(t *testing.T) {
	target, sent := redirectstest.Sent(t)
	_, port, err := net.SplitHostPort(target)
	if err != nil {
		t.Fatal(err)
	}
	samePort := "www.googleapis.com:" + port
	// The read's client reaches http.DefaultTransport. A request to Google's host name at the
	// stand-in's port goes to the stand-in; useGoogleAPIsStandIn answers the userinfo call itself.
	base := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != samePort {
			return base.RoundTrip(r)
		}
		moved := r.Clone(r.Context())
		moved.URL.Host = target
		return base.RoundTrip(moved)
	})
	t.Cleanup(func() { http.DefaultTransport = base })
	// movedTo answers the userinfo call 302 to host, at the call's own path, so the bearer can
	// reach host only in the Authorization a request there carries.
	movedTo := func(host string) func(*http.Request) (*http.Response, error) {
		return func(r *http.Request) (*http.Response, error) {
			answer := googleAnswer(r, http.StatusFound, "")
			answer.Header.Set("Location", "http://"+host+r.URL.RequestURI())
			return answer, nil
		}
	}
	signIn := &google.Credentials{TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: userinfoBearer})}

	t.Run("twin", func(t *testing.T) {
		useGoogleAPIsStandIn(t, movedTo(samePort))
		response, err := oauth2.NewClient(context.Background(), signIn.TokenSource).Get(googleUserinfoURL)
		if err != nil {
			t.Fatalf("a client that follows the 302 failed: %v", err)
		}
		_ = response.Body.Close()
		if text := sent(); !strings.Contains(text, userinfoBearer) {
			t.Fatalf("a client that follows the 302 sent the stand-in no bearer, so the rows cannot see one sent on: %q", text)
		}
	})
	for _, c := range []struct{ name, host string }{
		{"other-port", samePort},
		{"other-host", target},
	} {
		t.Run(c.name, func(t *testing.T) {
			calls := useGoogleAPIsStandIn(t, movedTo(c.host))
			_, err := gcpUserinfo(context.Background(), signIn)
			text := sent()
			redacttest.RequireNoPiece(t, "agent-secrets.redirect-followed/"+c.name, text, userinfoBearer)
			if text != "" {
				t.Errorf("the stand-in the Location names was asked: %q", text)
			}
			if want := "read the Google sign-in's email from " + googleUserinfoURL + ": the server answered HTTP 302"; err == nil || !strings.HasPrefix(err.Error(), want) {
				t.Errorf("gcpUserinfo = %v, want it refused with the 302's status (%q)", err, want)
			}
			if n := calls.Load(); n != 1 {
				t.Errorf("Google's host was asked %d times, want once", n)
			}
		})
	}
}
