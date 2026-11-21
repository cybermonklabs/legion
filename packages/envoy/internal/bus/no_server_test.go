package bus_test

import (
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
)

// A client that names no server is refused at construction: nats.go fills an empty server list
// with its DefaultURL, nats://127.0.0.1:4222, so such a client would dial whatever listens on the
// shared loopback's default NATS port, presenting its credential there.
func TestAClientNamingNoServerIsRefused(t *testing.T) {
	client, err := bus.Connect(nil, bus.WithName("no-server-red-test"), bus.WithoutStreamSubjects(), bus.WithConnectInBackground())
	if err == nil {
		client.Close()
		t.Fatalf("bus.Connect built a client that names no server; nats.go dials nats://127.0.0.1:4222 for it")
	}
}
