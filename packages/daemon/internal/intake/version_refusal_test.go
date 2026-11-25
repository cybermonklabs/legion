package intake

import (
	"context"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/natsversion"

	"github.com/sjawhar/legion/daemon/internal/testnats"
)

// The daemon opens no consumer on a NATS server that can skip a message on a consumer filtering
// on a wildcard and a literal at the same token: OpenConsumers answers an error naming the
// server's version and the fixed releases (natsversion.Refusal), and the daemon's boot ends on it.
// nats:2.10, v2.10.29, is such a server. The control is the package's own server, the hub's
// v2.15.1, on which the same call opens the consumers.
func TestOpenConsumersRefusesAServerThatSkipsAFilter(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	ctx := context.Background()

	url := testnats.StartImage(t, testnats.SkippingImage)
	conn, err := nats.Connect(url, nats.Name(t.Name()))
	if err != nil {
		t.Fatalf("connect to nats:2.10: %v", err)
	}
	t.Cleanup(conn.Close)
	refused, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("JetStream on nats:2.10: %v", err)
	}
	if _, err := refused.CreateStream(ctx, jetstream.StreamConfig{Name: NotificationStream, Subjects: []string{"notifications.>"}}); err != nil {
		t.Fatalf("create the stream on nats:2.10: %v", err)
	}
	consumers, err := OpenConsumers(ctx, refused, pool, consumerSpec(&lockedBuffer{}))
	if err == nil {
		t.Fatalf("OpenConsumers on nats:2.10 opened %v, want a refusal naming its version", consumers)
	}
	if !strings.Contains(err.Error(), "2.10.29") || !strings.Contains(err.Error(), natsversion.FixedReleases) {
		t.Fatalf("the refusal = %q, want it to name 2.10.29 and %q", err, natsversion.FixedReleases)
	}

	js, _ := testJetStream(t)
	if _, err := OpenConsumers(ctx, js, pool, consumerSpec(&lockedBuffer{})); err != nil {
		t.Fatalf("OpenConsumers on the package's server (%s): %v, want the consumers open", js.Conn().ConnectedServerVersion(), err)
	}
}
