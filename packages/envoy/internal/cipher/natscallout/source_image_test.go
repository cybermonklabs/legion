//go:build natslive

package natscallout

import (
	"testing"

	"github.com/sjawhar/envoy/internal/testnats"
)

// TestNoNonOperatorGrantCanSourceTheNotificationStream on testnats.HubImage, the nats-server image
// the hub deploys, where the measurement of 2026-11-17 was made against v2.15.1 in process: under
// every answer of the auth service but the operator's, a stream that sources ENVOY_NOTIFICATIONS
// and republishes it to the connection's inbox is refused, and the connection reads nothing
// (requireNoGrantSourcesTheNotificationStream), while the measurement job's grant before it
// configured no stream reads the private message.
func TestNoNonOperatorGrantCanSourceTheNotificationStream(t *testing.T) {
	requireNoGrantSourcesTheNotificationStream(t, func(keys testnats.CalloutKeys, static ...testnats.StaticUser) string {
		return testnats.StartImageWithCallout(t, keys, static...)
	})
}
