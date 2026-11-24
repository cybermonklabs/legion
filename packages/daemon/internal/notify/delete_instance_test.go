package notify

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
)

// TestADeleteWhoseInstanceTheRuleWouldChangeIsNotSent: the clean stop reads the session's
// registration and sends back the client instance id it names. An id the listener's answer names
// that holds the request's own bearer (echoed), or a URL with user information, which the rule
// removes (userinfo), is one the rule would change, so the delete cannot name it: DeleteSession
// sends no delete and answers an error that prints none of the id, where it sent the delete with
// no instance at all, which holds under the moment alone. An id the rule prints as it came (plain)
// is sent, and an entry that names none (none) is sent with no instance parameter, as before. A
// stand-in listener answers the registration and records each delete's query. Captures
// notify.delete-instance/<case>.
func TestADeleteWhoseInstanceTheRuleWouldChangeIsNotSent(t *testing.T) {
	bearer := oauth2calltest.Token + "-envoy-7Qz4k"
	for _, c := range []struct {
		name, id string
		sent     bool
		instance string
	}{
		{"echoed", "inst_" + bearer, false, ""},
		{"userinfo", "inst see https://" + "not-a-secret-user:not-a-secret-password" + "@issuer.example/kept", false, ""},
		{"plain", "inst_dead", true, "inst_dead"},
		{"none", "", true, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			deletes := make(chan url.Values, 1)
			listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodDelete {
					deletes <- r.URL.Query()
					_, _ = io.WriteString(w, `{"deleted":true}`)
					return
				}
				_, _ = fmt.Fprintf(w, `{"session_id":"ses_dead","client_instance_id":%q,"last_seen":1}`, c.id)
			}))
			defer listener.Close()
			publisher := New(listener.URL, bearer)
			registration, err := publisher.ReadSessionRegistration(context.Background(), "ses_dead")
			if err != nil || !registration.Registered {
				t.Fatalf("ReadSessionRegistration = %+v, %v; want the stand-in's registration", registration, err)
			}
			stopped, err := publisher.DeleteSession(context.Background(), "ses_dead", DeleteConditions{
				ClientInstanceID: registration.ClientInstanceID, NotRegisteredAfter: time.UnixMilli(1700000000000),
			})
			var query url.Values
			select {
			case query = <-deletes:
			default:
			}
			if !c.sent {
				if query != nil {
					t.Fatalf("a delete reached the listener with query %q; want none sent for an instance id the rule would change", query.Encode())
				}
				if err == nil {
					t.Fatalf("DeleteSession = %+v, nil; want an error saying no delete was sent", stopped)
				}
				oauth2calltest.RequireNoPiece(t, "notify.delete-instance/"+c.name, err.Error(), bearer, "not-a-secret-user", "not-a-secret-password")
				return
			}
			if err != nil || !stopped.Deleted {
				t.Fatalf("DeleteSession = %+v, %v; want the listener's delete taken", stopped, err)
			}
			if query == nil {
				t.Fatal("no delete reached the listener")
			}
			if got, named := query["client_instance_id"]; c.instance == "" && named || c.instance != "" && (len(got) != 1 || got[0] != c.instance) {
				t.Errorf("the delete named client instance %q; want %q", got, c.instance)
			}
		})
	}
}
