package notify

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
)

// TestAListenerAnswerNamesItsStatusWithNoPhraseItWrote: the Envoy listener, or whatever answers in
// its place, answers each of the daemon's listener calls under a reason phrase that quotes a
// credential's URL, with a body cut off mid-read (unread) or a refusal (refused). Each error names
// the status by its code and the standard phrase (oauth2call.StatusLine), and none of the phrase
// the listener's stand-in wrote. Captures notify.status-line/<case>.
func TestAListenerAnswerNamesItsStatusWithNoPhraseItWrote(t *testing.T) {
	ctx := context.Background()
	publish := func(base string) error {
		return New(base, "").Publish(ctx, "notifications.role.legion-x", "a notice", map[string]string{"k": "v"}, "key-1")
	}
	registration := func(base string) error {
		_, err := New(base, "").ReadSessionRegistration(ctx, "ses_s")
		return err
	}
	holder := func(base string) error {
		_, _, err := New(base, "").RoleHolder(ctx, "legion-x")
		return err
	}
	remove := func(base string) error {
		_, err := New(base, "").DeleteSession(ctx, "ses_s", DeleteConditions{NotRegisteredAfter: time.Now()})
		return err
	}
	cut := func(status int, body string) func(*testing.T) string {
		return func(t *testing.T) string { return oauth2calltest.CutServer(t, status, body) }
	}
	whole := func(status int, body string) func(*testing.T) string {
		return func(t *testing.T) string {
			return oauth2calltest.StatusLineServer(t, status, "Content-Type: application/json\r\n", body)
		}
	}
	for _, c := range []struct {
		name   string
		status int
		server func(*testing.T) string
		call   func(string) error
	}{
		{"publish-unread", http.StatusNotFound, cut(http.StatusNotFound, `{"error":"no live session holds the role","reason":"unclaimed"}`), publish},
		{"publish-refused", http.StatusBadRequest, whole(http.StatusBadRequest, `{"error":"the notice is malformed"}`), publish},
		{"registration-refused", http.StatusInternalServerError, whole(http.StatusInternalServerError, `{"error":"the registry is down"}`), registration},
		{"role-holder-unread-200", http.StatusOK, cut(http.StatusOK, `{"role":"legion-x","holder":"ses_s","holder_state":"live","last_seen":1,"title":"a title long enough to cut"}`), holder},
		{"role-holder-unread-404", http.StatusNotFound, cut(http.StatusNotFound, `{"error":"the holder lapsed","reason":"holder_lapsed","holder":"ses_s","holder_state":"unresponsive"}`), holder},
		{"role-holder-busy", http.StatusServiceUnavailable, whole(http.StatusServiceUnavailable, `{"error":"the bus is unavailable"}`), holder},
		{"role-holder-refused", http.StatusTeapot, whole(http.StatusTeapot, `{"error":"no"}`), holder},
		{"delete-unread", http.StatusOK, cut(http.StatusOK, `{"deleted":false,"reason":"the registration is newer than the stop"}`), remove},
		{"delete-refused", http.StatusInternalServerError, whole(http.StatusInternalServerError, `{"error":"the registry is down"}`), remove},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := c.call(c.server(t))
			if err == nil {
				t.Fatal("the call answered no error")
			}
			oauth2calltest.RequireNone(t, "notify.status-line/"+c.name, err.Error())
			if want := fmt.Sprintf("%d %s", c.status, http.StatusText(c.status)); !strings.Contains(err.Error(), want) {
				t.Errorf("the error is %q, want %q in it", err, want)
			}
		})
	}
}
