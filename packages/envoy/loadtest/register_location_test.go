package main

import (
	"context"
	"log"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/redirects/redirectstest"
)

// registrationRowBearer is the listener bearer the registration row's requests carry, which its
// stand-in echoes; it holds the gate's marker.
const registrationRowBearer = "not-a-secret-listener-RDX-5c27"

// TestARegistrationCarriesNoBearerOn: whatever answers at the listener's address answers a
// session's registration, which carries the listener's bearer, with a 302 whose Location names a
// stand-in on another host and quotes the bearer in its query. The stand-in is sent nothing, so no
// request carries the bearer on to it, and the registration is counted refused by the 302:
// registerSessions' client follows no redirect (redirects.Refused). The twin shows a client that
// follows it sends the stand-in the bearer, so the row can see one sent on. Captures
// loadtest.register-redirect/followed, what the stand-in was sent.
func TestARegistrationCarriesNoBearerOn(t *testing.T) {
	target, sent := redirectstest.Sent(t)
	location := func(bearer string) string { return redirectstest.To(target, bearer) }
	t.Run("twin", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodPost, redirectstest.EchoBearer(t, location)+"/v1/interests/subscribe", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+registrationRowBearer)
		redirectstest.RequireSentOn(t, request, registrationRowBearer, sent)
	})
	t.Run("followed", func(t *testing.T) {
		var logged strings.Builder
		st := &step{cfg: runConfig{ep: endpoints{listener: redirectstest.EchoBearer(t, location), listenerToken: registrationRowBearer}},
			lg: log.New(&logged, "", 0), ids: []string{"ses-lg-0000"}}
		reg := st.registerSessions(context.Background(), &natsSessions{relay: newRelaySessions(1, 1)})
		text := sent()
		redacttest.RequireNoPiece(t, "loadtest.register-redirect/followed", text, registrationRowBearer)
		if text != "" {
			t.Errorf("the stand-in the Location names was asked: %q", text)
		}
		if reg.ok.Load() != 0 || reg.errs.Load() != 1 || !strings.Contains(logged.String(), "register ses-lg-0000: 302 ") {
			t.Errorf("registerSessions took %d and refused %d, and logged %q; want the one registration refused by the redirect's own 302",
				reg.ok.Load(), reg.errs.Load(), logged.String())
		}
	})
}
