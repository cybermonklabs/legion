package notify

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// lapsedBesideAURL is the listener's 404 for a role whose holder is unresponsive, its error naming
// a URL right before the JSON quote that ends it, which the rule cannot read as clean, so it prints
// the whole body as not readable.
const lapsedBesideAURL = `{"error":"role legion-demo-controller holder ses_a lapsed; see https://listener.example/roles#legion-demo-controller","reason":"holder_lapsed","holder":"ses_a","holder_state":"unresponsive","last_seen":1}`

// TestAListenerRefusalIsReadFromItsWordsAsTheyCame: the listener refuses a role publish, and a
// role lookup, with refusals whose error names a URL beside a JSON quote, so the rule prints each
// whole body as not readable. What each refusal is, is decided on the body as it came
// (oauth2call.Answer.Decode): the 404 is a holder that is unresponsive (ErrHolderUnresponsive),
// the 504 an envelope its holder did not confirm (ErrNotDelivered), the 403 a holder that does not
// advertise the delivery mode (ErrRefusedByHolder), and the lookup's 404 names its lapsed holder
// and that holder's state. What each error prints is the rule's reading, which names no URL.
func TestAListenerRefusalIsReadFromItsWordsAsTheyCame(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name, body string
		status     int
		want       func(error) bool
	}{
		{"unresponsive", lapsedBesideAURL, http.StatusNotFound, func(err error) bool { return errors.Is(err, ErrHolderUnresponsive) }},
		{"not-delivered", `{"error":"no receipt; see https://listener.example/roles#receipts","reason":"receipt_timeout","event_id":"evt-1","holder":"ses_a","attempts":3}`,
			http.StatusGatewayTimeout, func(err error) bool {
				var undelivered ErrNotDelivered
				return errors.As(err, &undelivered) && undelivered.Reason == ReasonReceiptTimeout && undelivered.Holder.Equal("ses_a") && undelivered.EventID.Equal("evt-1")
			}},
		{"capability", `{"error":"the holder takes no prompt; see https://listener.example/roles#capabilities","reason":"capability","holder":"ses_a"}`,
			http.StatusForbidden, func(err error) bool {
				var refused ErrRefusedByHolder
				return errors.As(err, &refused) && refused.Holder.Equal("ses_a")
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			}))
			t.Cleanup(server.Close)
			_, err := New(server.URL, "envoy-bearer").Publish(ctx, "notifications.role.legion-demo-controller", "a held notice", map[string]string{"kind": "held"}, "legion-outbox:9")
			if !c.want(err) {
				t.Errorf("the publish refused %d beside a URL = %v, want the refusal its body names", c.status, err)
			}
			if err != nil && strings.Contains(err.Error(), "listener.example/roles#") {
				t.Errorf("the refusal prints %q, want the listener's words as the rule reads them", err)
			}
		})
	}
	t.Run("lookup", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(lapsedBesideAURL))
		}))
		t.Cleanup(server.Close)
		holder, state, err := New(server.URL, "envoy-bearer").RoleHolder(ctx, "legion-demo-controller")
		if err != nil || !holder.Equal("ses_a") || state != "unresponsive" {
			t.Errorf("RoleHolder = %q, %q, %v; want the lapsed holder ses_a, unresponsive", holder, state, err)
		}
	})
}
