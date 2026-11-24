//go:build natslive

package natscallout

import (
	"testing"

	"github.com/sjawhar/envoy/internal/testnats"
)

// TestAnEmptyAllowListIsRefused on testnats.HubImage, nats-server 2.15.3, the clustered hub's: the
// server there lets an empty-publish user publish anywhere too, and the service refuses the grant.
func TestAnEmptyAllowListIsRefusedOnTheHubImage(t *testing.T) {
	requireAnEmptyPublishListRefused(t, func(keys testnats.CalloutKeys, static ...testnats.StaticUser) string {
		return testnats.StartImageWithCallout(t, keys, static...)
	})
}
