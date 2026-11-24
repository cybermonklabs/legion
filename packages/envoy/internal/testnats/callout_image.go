//go:build natslive

package testnats

import "testing"

// StartImageWithCallout runs HubImage, the nats-server the hub deploys, as a test container whose
// configuration is CalloutConfig, with JetStream, removed when the test ends, and returns its
// client URL once it admits the service's own user (awaitSeed).
func StartImageWithCallout(t testing.TB, keys CalloutKeys, static ...StaticUser) (url string) {
	t.Helper()
	_, url = startImage(t, HubImage, CalloutConfig(keys, static...), []string{"-js"})
	awaitSeed(t, url, keys.ServiceSeed)
	return url
}
