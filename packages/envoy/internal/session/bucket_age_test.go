package session

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
)

// A session bucket the registry creates keeps a record one hour past the gone horizon. A record
// the bucket expires reads gone, so a bucket that kept it a shorter time would drop a stalled
// session's record while the session is only unresponsive. The registry is opened on a fresh
// account with no TTL option, so it creates the bucket at its own default. The age is written as
// the horizon plus an hour, not as the constant, so the test names the age it requires.
func TestANewSessionBucketOutlivesTheGoneHorizon(t *testing.T) {
	client := setupNATS(t)
	registry, err := OpenSessionRegistry(t.Context(), client.Conn, WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open the session registry: %v", err)
	}
	t.Cleanup(registry.StopWatch)
	want := GoneHorizon + time.Hour

	js, err := client.Conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	info, err := js.StreamInfo("KV_" + bus.SessionBucket)
	if err != nil {
		t.Fatalf("read the session bucket's stream: %v", err)
	}
	if info.Config.MaxAge != want {
		t.Errorf("a new session bucket's maximum age is %s, want %s: the gone horizon (%s) plus one hour", info.Config.MaxAge, want, GoneHorizon)
	}
	if got := registry.TTL(); got != want {
		t.Errorf("the registry's TTL on a new session bucket is %s, want %s: the gone horizon (%s) plus one hour", got, want, GoneHorizon)
	}
}
