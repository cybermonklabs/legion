//go:build natslive

package testnats_test

import (
	"testing"

	"github.com/sjawhar/envoy/internal/testnats"
)

// The image fixture is the in-process callout fixture on the nats-server image the hub deploys: a
// client with no credential is sent to the service, encrypted to its key, and a listed static user
// is admitted without a request.
func TestTheImageFixtureSendsUnknownClientsToTheService(t *testing.T) {
	keys := testnats.NewCalloutKeys(t)
	staticSeed, staticKey := testnats.User(t)
	url := testnats.StartImageWithCallout(t, keys, testnats.StaticUser{
		NKey: staticKey, Publish: []string{"example.>"}, Subscribe: []string{"example.>"},
	})
	requireCalloutRequests(t, url, keys, staticSeed)
}
