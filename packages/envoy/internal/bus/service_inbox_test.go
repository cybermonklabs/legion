package bus_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testnats/servicecallout"
	"github.com/sjawhar/envoy/natscred"
	"github.com/sjawhar/envoy/natsgrant"
)

// A client connecting with a service's platform token asks under the reply prefix its token's
// account names (natscred.InboxPrefix) and gets answers there: the auth service grants that prefix
// in place of _INBOX.>, so a JetStream request, answered on the connection's response inbox, is
// answered only when the client uses it. The inbox under which it asks a session for an answer is
// the account's session reply inbox, the one the auth service grants it (natsgrant.SessionReplyInbox).
func TestAServiceConnectionAsksUnderItsOwnInboxPrefix(t *testing.T) {
	url := servicecallout.Start(t, "listener/example-hub/*="+serviceTokenAccount)
	tokenFile := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte(unsignedToken(time.Now().Add(50*time.Minute))), 0o600))
	environment := map[string]string{bus.ServiceTokenFileEnvVar: tokenFile, bus.AllowRemoteEnvVar: "1"}
	client, err := bus.Connect([]string{url}, bus.WithEnvironment(func(name string) (string, bool) {
		value, set := environment[name]
		return value, set
	}))
	require.NoError(t, err)
	defer client.Close()

	inbox := client.Connection().NewInbox()
	require.True(t, strings.HasPrefix(inbox, natscred.InboxPrefix(serviceTokenAccount)+"."),
		"the connection's inbox %s is not under its account's prefix %s", inbox, natscred.InboxPrefix(serviceTokenAccount))
	require.Equal(t, natsgrant.SessionReplyInbox(serviceTokenAccount), bus.SessionReplyInboxOf(client.Connection()))
	_, err = client.JS().AccountInfo()
	require.NoError(t, err, "a JetStream request got no answer on the connection's own prefix")
}

// A service connection's stored requests, a window's Request and StartRequestStoredTo, are
// answered: each takes the stream's acknowledgement on an inbox it subscribes itself, which must
// lie under the connection's own prefix, the one inbox space the auth service grants it.
func TestAServiceConnectionsStoredRequestsAreAnswered(t *testing.T) {
	adminSeed, adminKey := testnats.User(t)
	url := servicecallout.Start(t, "listener/example-hub/*="+serviceTokenAccount,
		testnats.StaticUser{NKey: adminKey, Publish: []string{">"}, Subscribe: []string{">"}})
	admin, err := testnats.ConnectSeed(t, url, adminSeed)
	require.NoError(t, err)
	defer admin.Close()
	js, err := admin.JetStream()
	require.NoError(t, err)
	_, err = js.AddStream(&natsgo.StreamConfig{Name: "STORED_REQUESTS", Subjects: []string{"notifications.example.>"}})
	require.NoError(t, err)

	tokenFile := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte(unsignedToken(time.Now().Add(50*time.Minute))), 0o600))
	environment := map[string]string{bus.ServiceTokenFileEnvVar: tokenFile, bus.AllowRemoteEnvVar: "1"}
	client, err := bus.Connect([]string{url}, bus.WithEnvironment(func(name string) (string, bool) {
		value, set := environment[name]
		return value, set
	}))
	require.NoError(t, err)
	defer client.Close()

	item := contracts.Envelope{Payload: "stored"}
	forward, err := client.StartRequestStoredTo("notifications.example.forward", item, "forward-1", 5*time.Second)
	require.NoError(t, err)
	require.NoError(t, forward.Wait(), "StartRequestStoredTo's acknowledgement never reached the service")
	window := client.WindowUntil(func() time.Time { return time.Now().Add(time.Minute) })
	require.NoError(t, <-window.Request("notifications.example.window", item, bus.RequestOptions{MsgID: "window-1", Timeout: 5 * time.Second}),
		"a window's Request's acknowledgement never reached the service")
}

// A client connecting with a seed, or with no credential, keeps nats.go's own inbox, and asks a
// session under a new inbox of it: one token after _INBOX., which a session's grant answers.
func TestASeedConnectionKeepsNatsGosInbox(t *testing.T) {
	unsetNkeyEnvironment(t)
	_, url := testnats.Start(t)
	client, err := bus.Connect([]string{url}, bus.WithEnvironment(func(name string) (string, bool) {
		if name == bus.AllowRemoteEnvVar {
			return "1", true
		}
		return "", false
	}))
	require.NoError(t, err)
	defer client.Close()
	require.True(t, strings.HasPrefix(client.Connection().NewInbox(), natsgo.InboxPrefix))
	asked := bus.SessionReplyInboxOf(client.Connection())
	token, found := strings.CutPrefix(asked, natsgo.InboxPrefix)
	require.True(t, found && token != "" && !strings.Contains(token, "."), "a seed client asks a session under %s, want one token after _INBOX.", asked)
}
