package bus_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// R12 of the auth service's spec (unchanged without the callout): a hub configured with no auth
// service, and clients configured with none, run exactly as they run today. Today (the merge base,
// 1610472cb1) a bus connection that holds a stored nkey seed, and that the hub refuses twice in a
// row - a seed rotated or revoked on the hub - closes for good, as nats.go does by default
// (nats.go v1.55.0, nats.go:4217-4222). It must still do so: IgnoreAuthErrorAbort is what a connection
// the auth service decides needs, and natscred.Session and natscred.Service set it on those
// connections. internal/bus's background connect (connectInBackground) sets it too, on every
// connection it makes, nkey ones included, as it did at the merge base; this test's connection is
// not a background one.
//
// Measured at 40c57f5ee5 on nats:2.15.1 (testnats.HubImage) with static nkey users only, the hub
// restarted without the client's user for 12 s: the client built at the merge base logged two
// authorization violations and "envoy nats connection closed"; the client built at the head logged
// one authorization violation a second for 12 s and never closed.
func TestANkeyConnectionRefusedTwiceClosesAsAtTheMergeBase(t *testing.T) {
	seed, public := testnats.User(t)
	_, other := testnats.User(t)
	server, url := testnats.StartNkeyAuthorizedServer(t, public)
	nc, err := bus.Dial("nkey-refusal-test", []string{url}, func(name string) (string, bool) {
		if name == bus.NkeySeedEnvVar {
			return seed, true
		}
		return "", false
	})
	require.NoError(t, err)
	defer nc.Close()
	require.True(t, nc.IsConnected())

	server.Restart(t, fmt.Sprintf("jetstream {}\nauthorization {\n  users = [ { nkey: %q } ]\n}\n", other))
	testwait.WithinPolling(t, 30*time.Second, 100*time.Millisecond,
		"the bus connection whose seed the hub refuses twice to close, as it does at the merge base", func() bool {
			return nc.Status() == nats.CLOSED
		})
}
