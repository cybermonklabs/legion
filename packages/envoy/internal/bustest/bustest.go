// Package bustest is the one fixture helper for the buckets a listener copies (bus.CopiedBuckets)
// on a test's or a dev rig's own NATS server. No program of a deployment creates one of those
// buckets (INFRA-4094): the stores open them only if they exist, and the operator creates them with
// `envoy-infra buckets`. A fixture creates them explicitly instead, in one of two ways:
//
//   - a Go test that opens a store in its own process marks its connection as a fixture's
//     (Connect, Fixture), and the store then creates a copied bucket it finds missing with its own
//     configuration (bus.CreateCopiedBucketsOn);
//   - a rig that starts a listener binary provisions its server before the listener starts
//     (Provision, or the fixturebuckets command for a script), and marks the server as a fixture's
//     (MarkerStream), which refillrig's destructive commands require (RequireFixtureServer).
package bustest

import (
	"errors"
	"fmt"
	"testing"

	"github.com/nats-io/nats.go"

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
func Connect(t testing.TB, uri string) *nats.Conn {
	t.Helper()
	return Fixture(testnats.Connect(t, uri))
}

// Fixture marks conn, a connection to a test's own server, as a fixture's
// (bus.CreateCopiedBucketsOn), and returns it.
func Fixture(conn *nats.Conn) *nats.Conn {
	bus.CreateCopiedBucketsOn(conn)
	return conn
}

// Provision creates every bucket a listener copies on the server conn reaches, with its
// configuration at replicas copies (bus.CreateCopiedBucket), and the marker stream that says the
// server is a fixture's (MarkerStream). A rig runs it on its own server before it starts a listener,
// which creates none of those buckets. It acts once per server: a server that holds the marker
// already is left as it is, so a test that deletes a bucket and starts a listener again on the same
// server finds the bucket still missing.
func Provision(conn *nats.Conn, replicas int) error {
	js, err := conn.JetStream()
	if err != nil {
		return err
	}
	if _, err := js.StreamInfo(MarkerStream); err == nil {
		return nil
	} else if !errors.Is(err, nats.ErrStreamNotFound) {
		return fmt.Errorf("read %s: %w", MarkerStream, err)
	}
	for _, bucket := range bus.CopiedBuckets() {
		if _, err := bus.CreateCopiedBucket(conn, js, bucket, replicas); err != nil {
			return fmt.Errorf("create %s: %w", bucket, err)
		}
	}
	if _, err := js.AddStream(&nats.StreamConfig{Name: MarkerStream, Subjects: []string{markerSubject}, Storage: nats.MemoryStorage}); err != nil && !errors.Is(err, nats.ErrStreamNameAlreadyInUse) {
		return fmt.Errorf("mark the server as a fixture's (%s): %w", MarkerStream, err)
	}
	return nil
}

// RequireFixtureServer answers an error unless the server js reaches holds the marker Provision
// leaves (MarkerStream): a command that creates or deletes a copied bucket outside `envoy-infra
// buckets` runs only on a server a fixture provisioned.
func RequireFixtureServer(js nats.JetStreamContext) error {
	if _, err := js.StreamInfo(MarkerStream); err != nil {
		return fmt.Errorf("the server holds no %s stream, so no fixture provisioned it (bustest.Provision, the fixturebuckets command): refusing to create or recreate a bucket a listener copies on it: %w", MarkerStream, err)
	}
	return nil
}
