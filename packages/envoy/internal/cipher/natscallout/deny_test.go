package natscallout

import (
	"testing"
	"time"

	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nkeys"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/natsgrant"
)

// A deny list narrows a direction and never opens one: a grant whose publish allow list is empty
// is refused even with a publish deny entry beside it, since the user it would encode may publish
// to every subject the deny does not name.
func TestADenyEntryDoesNotCloseAnEmptyAllowList(t *testing.T) {
	issuer, err := nkeys.CreateAccount()
	require.NoError(t, err)
	user, err := nkeys.CreateUser()
	require.NoError(t, err)
	userKey, err := user.PublicKey()
	require.NoError(t, err)
	grant := natsgrant.Permissions{Subscribe: []string{"_INBOX.>"}, PublishDeny: []string{"notifications.dispatch.>"}}
	encoded, err := encodeAnswer(issuer, Request{UserNkey: userKey, ServerID: "NSERVER"},
		Decision{Name: "session/ses_a", Grant: grant, Expires: time.Now().Add(time.Hour)}, nil)
	require.NoError(t, err)
	response, err := jwt.DecodeAuthorizationResponseClaims(encoded)
	require.NoError(t, err)
	require.Equal(t, "grant opens every subject: its publish allow list is empty", response.Error)
	require.Empty(t, response.Jwt)
}

// A grant's deny entries reach the user the service admits, and the server applies them: a
// connection allowed every notification topic but denied Dispatch's lane is refused a publish there
// in nats-server's own words, and still publishes elsewhere. The control is the same grant without
// the deny, whose publish on Dispatch's lane is served, so the refusal is the deny's.
func TestADenyEntryIsEncodedAndEnforced(t *testing.T) {
	for _, row := range []struct {
		name   string
		deny   []string
		denied bool
	}{
		{"denied Dispatch's lane", []string{"notifications.dispatch.>"}, true},
		{"control: the same grant with no deny", nil, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			keys := testnats.NewCalloutKeys(t)
			readerSeed, readerKey := testnats.User(t)
			url := testnats.StartWithCallout(t, keys, testnats.StaticUser{NKey: readerKey, Publish: []string{"_INBOX.>"}, Subscribe: []string{"notifications.>"}})
			reader, err := testnats.ConnectSeed(t, url, readerSeed)
			require.NoError(t, err)
			defer reader.Close()
			lane, err := reader.SubscribeSync("notifications.>")
			require.NoError(t, err)
			require.NoError(t, reader.Flush())

			a := newSession(t, "ses_a", 15*time.Minute)
			startService(t, url, keys, Deps{Introspect: introspector(a)}, answering(natsgrant.Permissions{
				Publish: []string{"_INBOX.>", "notifications.>"}, Subscribe: []string{"_INBOX.>"},
				PublishDeny: row.deny,
			}))
			errs := make(chan error, 16)
			nc, err := connectSession(t, url, a, errs)
			require.NoError(t, err)
			require.NoError(t, nc.Publish("notifications.dispatch.x", []byte("dispatch")))
			require.NoError(t, nc.Publish("notifications.other.x", []byte("other")))
			require.NoError(t, nc.Flush())

			var got []string
			for {
				msg, err := lane.NextMsg(time.Second)
				if err != nil {
					break
				}
				got = append(got, string(msg.Data))
			}
			if row.denied {
				require.Equal(t, []string{"other"}, got, "the denied publish reached the subscriber")
				select {
				case err := <-errs:
					require.Contains(t, err.Error(), `Permissions Violation for Publish to "notifications.dispatch.x"`)
				case <-time.After(5 * time.Second):
					t.Fatal("the server sent no permissions violation for the denied publish")
				}
				return
			}
			require.ElementsMatch(t, []string{"dispatch", "other"}, got, "the control's publishes did not both arrive")
		})
	}
}
