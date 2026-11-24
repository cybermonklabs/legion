package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/envoy/redirects/redirectstest"
)

// TestACLIReadOfTheDaemonFollowsNoRedirectAndNamesNoLocation: whatever answers at a daemon's
// address answers the CLI's reads with a redirect whose Location quotes the operator bearer the
// request carried (an operator call), or names a stand-in with that query (a state read, which
// carries no credential: its Location is the answering party's own words). One net/http cannot
// parse (unparsed) is net/http's error whatever the client's redirect policy; one that parses, to
// a stand-in that answers (followed), was followed. No error holds a run of the bearer or a word of
// the Location, and the stand-in a Location names is asked nothing: the CLI follows no redirect
// (daemonClient), so a 3xx is the daemon's answer, refused by its status. The twin shows the
// stand-in's echo reaches net/http's own error; plain shows a 200 still reads. Captures
// legion.redirect/<call>-<case>.
func TestACLIReadOfTheDaemonFollowsNoRedirectAndNamesNoLocation(t *testing.T) {
	const bearer = "xv9pk-not-a-secret-RDX-9d2e"
	target, asked := redirectstest.Target(t)
	t.Run("twin", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodGet, redirectstest.Echo(t, "Authorization", redirectstest.Unparsed)+"/legion/v1/operator/state", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+bearer)
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			_ = response.Body.Close()
			t.Fatal("net/http answered the unparsed Location with no error: the rows cannot see one")
		}
		if !strings.Contains(err.Error(), bearer) {
			t.Fatalf("net/http's own error holds no echo of the bearer, so the rows cannot see a leak: %q", err)
		}
	})
	// operatorCall is an operator request's error, or the daemon's answer other than 2xx as the
	// operator reads it (refusal).
	operatorCall := func(base string) error {
		answer, err := operator{base: base, bearer: bearer}.do(context.Background(), http.MethodPost, "/legion/v1/operator/pause", map[string]string{"reason": "a test"})
		if err != nil {
			return err
		}
		if answer.Status()/100 != 2 {
			return errors.New(refusal(answer))
		}
		return nil
	}
	stateRead := func(base string) error {
		_, err := get(context.Background(), base+"/legion/v1/state")
		return err
	}
	for _, c := range []struct {
		name     string
		call     func(base string) error
		location func(credential string) string
	}{
		{"operator-unparsed", operatorCall, redirectstest.Unparsed},
		{"operator-followed", operatorCall, func(credential string) string {
			return redirectstest.To(target, strings.TrimPrefix(credential, "Bearer "))
		}},
		{"state-unparsed", stateRead, func(string) string { return redirectstest.Unparsed(bearer) }},
		{"state-followed", stateRead, func(string) string { return redirectstest.To(target, bearer) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := c.call(redirectstest.Echo(t, "Authorization", c.location))
			if err == nil {
				t.Fatal("the read answered no error")
			}
			oauth2calltest.RequireNoPiece(t, "legion.redirect/"+c.name, err.Error(), bearer)
			if strings.Contains(err.Error(), "moved") {
				t.Errorf("the read's error names the Location: %q", err)
			}
			if n := asked(); n != 0 {
				t.Errorf("the redirect's target was asked %d times, want none", n)
			}
		})
	}
	t.Run("plain", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{}`))
		}))
		defer server.Close()
		if err := operatorCall(server.URL); err != nil {
			t.Fatalf("a plain 200 to the operator call = %v, want it answered", err)
		}
		if err := stateRead(server.URL); err != nil {
			t.Fatalf("a plain 200 to the state read = %v, want it read", err)
		}
	})
}
