package main

import (
	"errors"
	"os"
	"strings"
	"testing"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/bustest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// TestMain removes the NATS server the package's tests share (testnats.Main).
func TestMain(m *testing.M) { os.Exit(testnats.Main(m)) }

// refillrig's commands that create or recreate a bucket a listener copies act only on a server a
// fixture provisioned (bustest.Provision's ENVOY_TEST_FIXTURE marker): on any other they refuse
// and change nothing. The control is the same commands on the same server once a fixture has
// provisioned it, which act. At 7b262c2e0a both acted on any server.
func TestRefillrigCreatesOrRecreatesOnlyOnAFixturesServer(t *testing.T) {
	uri := testnats.URL(t)
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	config, _ := bus.CopiedBucketConfig(bus.InterestBucket, 1)
	if _, err := js.CreateKeyValue(&config); err != nil {
		t.Fatalf("create the interest bucket as an operator would: %v", err)
	}
	before, err := js.StreamInfo("KV_" + bus.InterestBucket)
	if err != nil {
		t.Fatalf("read the interest bucket: %v", err)
	}

	if err := runSeed([]string{"-nats", uri, "-buckets", "sessions", "-sets", "a=1", "-create"}); err == nil || !strings.Contains(err.Error(), bustest.MarkerStream) {
		t.Fatalf("seed -create on a server no fixture provisioned = %v, want refused naming %s", err, bustest.MarkerStream)
	}
	if _, err := js.KeyValue(bus.SessionBucket); !errors.Is(err, natsgo.ErrBucketNotFound) {
		t.Fatalf("the refused seed -create made the session bucket: %v", err)
	}
	if err := runRecreate([]string{"-nats", uri, "-bucket", "interests", "-sets", "a=1"}); err == nil || !strings.Contains(err.Error(), bustest.MarkerStream) {
		t.Fatalf("recreate on a server no fixture provisioned = %v, want refused naming %s", err, bustest.MarkerStream)
	}
	if after, err := js.StreamInfo("KV_" + bus.InterestBucket); err != nil || !after.Created.Equal(before.Created) {
		t.Fatalf("the refused recreate changed the interest bucket: %v, created %v, was %v", err, after.Created, before.Created)
	}

	if err := bustest.Provision(conn, 1); err != nil {
		t.Fatalf("provision the server: %v", err)
	}
	if err := runRecreate([]string{"-nats", uri, "-bucket", "interests", "-sets", "a=1"}); err != nil {
		t.Fatalf("control: recreate on the fixture's server: %v", err)
	}
	if after, err := js.StreamInfo("KV_" + bus.InterestBucket); err != nil || after.Created.Equal(before.Created) {
		t.Fatalf("control: the recreate left the interest bucket as it was: %v", err)
	}
}
