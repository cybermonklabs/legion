package dispatch_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/envoy/redirects/redirectstest"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
)

// TestARedirectIsDispatchsRefusalAndNoLocationReachesTheError is the round-7 red team's probe of
// the daemon's Dispatch client as rows: whatever answers at Dispatch's address answers a read with
// a redirect whose Location quotes the bearer the read carried. One net/http cannot parse
// (unparsed) is net/http's error whatever the client's redirect policy; one that parses was
// followed, to an address nothing listens at (unanswered, whose error net/http names by the
// Location) or to a stand-in that answers (followed). No read's error holds a run of the bearer or
// a word of the Location, and the stand-in a Location names is asked nothing: the client follows
// no redirect, so a 3xx is Dispatch's refusal by its status. The twin shows the stand-in's echo
// reaches net/http's own error; plain shows a 200 still reads. Captures
// dispatch.redirect/<case>.
func TestARedirectIsDispatchsRefusalAndNoLocationReachesTheError(t *testing.T) {
	const token = "dsp_not-a-secret-RDX-7f3a9c2e1b5d"
	target, asked := redirectstest.Target(t)
	dead := redirectstest.Dead(t)
	t.Run("twin", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodGet, redirectstest.EchoBearer(t, redirectstest.Unparsed)+"/api/v1/issues/LEGION-9/events", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		redirectstest.RequireEcho(t, request, token)
	})
	for _, c := range []struct {
		name     string
		location func(carried string) string
	}{
		{"unparsed", redirectstest.Unparsed},
		{"unanswered", func(carried string) string { return redirectstest.To(dead, carried) }},
		{"followed", func(carried string) string { return redirectstest.To(target, carried) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := dispatch.New(redirectstest.EchoBearer(t, c.location), dispatch.BearerTransport(token, nil)).EventsAfter(context.Background(), "LEGION-9", 0)
			if err == nil {
				t.Fatal("EventsAfter answered no error")
			}
			oauth2calltest.RequireNoPiece(t, "dispatch.redirect/"+c.name, err.Error(), token)
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
			fmt.Fprint(w, `[]`)
		}))
		defer server.Close()
		if _, err := dispatch.New(server.URL, dispatch.BearerTransport(token, nil)).EventsAfter(context.Background(), "LEGION-9", 0); err != nil {
			t.Fatalf("a plain 200 = %v, want it read", err)
		}
	})
}
