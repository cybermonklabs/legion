package bus_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// testInboxPrefix is the inbox prefix of this file's clients, and the one subject family their
// user may subscribe.
const testInboxPrefix = "_INBOX_bus_test"

// A client given an inbox prefix names every inbox under it, on its own connection and on the
// session consumers', and takes JetStream's replies there, under a user that may subscribe that
// prefix and no other inbox. The same user's client without the prefix asks and is answered
// nothing: its replies come under _INBOX., which the server refuses it. That refusal is the fact
// the second subtest waits for: the server's permissions violation on the client's connection
// (nats.go keeps it as the connection's last error). After it no reply can reach the ask, so the
// ask is then cancelled, and an ask answered before that fails the subtest.
func TestAClientWithAnInboxPrefixTakesItsRepliesUnderIt(t *testing.T) {
	seed, public := testnats.User(t)
	_, uri := testnats.StartConfig(t, testnats.SubscribeGrantConfig(public, testInboxPrefix+".>"), []string{"-js"}, false)
	t.Setenv("NATS_NKEY_SEED", seed)
	ask := func(ctx context.Context, client *bus.Client) error {
		_, err := client.Connection().RequestWithContext(ctx, "$JS.API.INFO", nil)
		return err
	}

	t.Run("with the prefix", func(t *testing.T) {
		client, err := bus.Connect([]string{uri}, bus.WithInboxPrefix(testInboxPrefix))
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(client.Close)
		if inbox := client.Connection().NewInbox(); !strings.HasPrefix(inbox, testInboxPrefix+".") {
			t.Fatalf("the connection's inbox is %q, want one under %s", inbox, testInboxPrefix)
		}
		if err := ask(testwait.Context(t), client); err != nil {
			t.Fatalf("JetStream's account info under the prefix: %v", err)
		}
		consumers, err := bus.ConnectSessionConsumers([]string{uri}, 0, nil, bus.WithInboxPrefix(testInboxPrefix))
		if err != nil {
			t.Fatalf("connect the session consumers: %v", err)
		}
		t.Cleanup(consumers.Close)
		if inbox := consumers.NewInbox(); !strings.HasPrefix(inbox, testInboxPrefix+".") {
			t.Fatalf("the session consumers' inbox is %q, want one under %s", inbox, testInboxPrefix)
		}
		conn, err := bus.Dial("inbox-prefix-dial", []string{uri}, os.LookupEnv, bus.WithInboxPrefix(testInboxPrefix))
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(conn.Close)
		if inbox := conn.NewInbox(); !strings.HasPrefix(inbox, testInboxPrefix+".") {
			t.Fatalf("the dialled connection's inbox is %q, want one under %s", inbox, testInboxPrefix)
		}
	})
	t.Run("without it", func(t *testing.T) {
		client, err := bus.Connect([]string{uri})
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(client.Close)
		ctx, cancel := context.WithCancel(testwait.Context(t))
		defer cancel()
		asked := make(chan error, 1)
		go func() { asked <- ask(ctx, client) }()
		testwait.Until(t, "the server to refuse the client its _INBOX. reply subscription", func() (error, bool) {
			err := client.Connection().LastError()
			return err, errors.Is(err, nats.ErrPermissionViolation) && strings.Contains(err.Error(), "_INBOX.")
		})
		cancel()
		if err := testwait.Receive(t, "the ask cancelled after the refusal to return", asked); err == nil {
			t.Fatal("a client whose replies come under _INBOX. was answered by a server that refuses it that subscription")
		}
	})
}
