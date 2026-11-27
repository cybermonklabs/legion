package bustest

import (
	"context"
	"errors"
	"fmt"
	"testing"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

// MarkerStream is the stream Provision leaves on a server it provisioned, which marks the server as
// a fixture's own: refillrig's commands that create or recreate a copied bucket refuse a server
// without it (RequireFixtureServer). No deployment creates it.
const MarkerStream = "ENVOY_TEST_FIXTURE"

// markerSubject is the one subject MarkerStream captures; nothing publishes to it.
const markerSubject = "envoy.test.fixture"

// Connect is testnats.Connect, a connection to the test's own server at uri, marked as a fixture's
// (Fixture): a store opened through it creates a copied bucket it finds missing.
func Connect(t testing.TB, uri string) *natsgo.Conn {
	t.Helper()
	return Fixture(testnats.Connect(t, uri))
}

// Fixture marks conn, a connection to a test's own server, as a fixture's
// (bus.CreateCopiedBucketsOn), and returns it.
func Fixture(conn *natsgo.Conn) *natsgo.Conn {
	bus.CreateCopiedBucketsOn(conn)
	return conn
}

// Provision creates every bucket a listener copies on the server conn reaches, with its
// configuration at replicas copies (bus.CreateCopiedBucket), and the marker stream that says the
// server is a fixture's (MarkerStream). A rig runs it on its own server before it starts a listener,
// which creates none of those buckets. It acts once per server: a server that holds the marker
// already is left as it is, so a test that deletes a bucket and starts a listener again on the same
// server finds the bucket still missing.
func Provision(conn *natsgo.Conn, replicas int) error {
	js, err := conn.JetStream()
	if err != nil {
		return err
	}
	if _, err := js.StreamInfo(MarkerStream); err == nil {
		return nil
	} else if !errors.Is(err, natsgo.ErrStreamNotFound) {
		return fmt.Errorf("read %s: %w", MarkerStream, err)
	}
	for _, bucket := range bus.CopiedBuckets() {
		if _, err := bus.CreateCopiedBucket(context.Background(), conn, js, bucket, replicas); err != nil {
			return fmt.Errorf("create %s: %w", bucket, err)
		}
	}
	if _, err := js.AddStream(&natsgo.StreamConfig{Name: MarkerStream, Subjects: []string{markerSubject}, Storage: natsgo.MemoryStorage}); err != nil && !errors.Is(err, natsgo.ErrStreamNameAlreadyInUse) {
		return fmt.Errorf("mark the server as a fixture's (%s): %w", MarkerStream, err)
	}
	return nil
}

// ProvisionURL is Provision of the server at uri, a test's own, through a connection of its own
// (testnats.Connect, with opts) that it closes before it returns, so the server's connections are
// as the test left them. A server that admits no client without a credential is given one in opts
// (testnats.SeedOption). A failed provisioning fails t.
func ProvisionURL(t testing.TB, uri string, replicas int, opts ...natsgo.Option) {
	t.Helper()
	conn := testnats.Connect(t, uri, opts...)
	defer conn.Close()
	if err := Provision(conn, replicas); err != nil {
		t.Fatalf("provision %s with the buckets a listener copies: %v", uri, err)
	}
}

// RequireFixtureServer answers an error unless the server js reaches holds the marker Provision
// leaves (MarkerStream): a command that creates or deletes a copied bucket outside `envoy-infra
// buckets` runs only on a server a fixture provisioned.
func RequireFixtureServer(js natsgo.JetStreamContext) error {
	if _, err := js.StreamInfo(MarkerStream); err != nil {
		return fmt.Errorf("the server holds no %s stream, so no fixture provisioned it (bustest.Provision, the fixturebuckets command): refusing to create or recreate a bucket a listener copies on it: %w", MarkerStream, err)
	}
	return nil
}
