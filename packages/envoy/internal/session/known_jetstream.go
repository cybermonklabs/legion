package session

import (
	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
)

// knownJetStream is the JetStream context one attempt to open the bucket asks through: its
// requests wait knownBucketOpenWait rather than the 10 seconds nats.go gives its own by default.
// bus.JetStreamOn builds it, so a request it sends while nats.go reconnects conn fails as
// bus.ErrReconnecting, not as the lookup's `nats: key-value requires at least server version 2.6.2`
// (conn reports no server version then), which the start's WARN and the repair would carry.
func knownJetStream(conn *nats.Conn) (nats.JetStreamContext, error) {
	return bus.JetStreamOn(conn, nats.MaxWait(knownBucketOpenWait))
}
