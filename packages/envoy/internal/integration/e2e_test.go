package integration

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

func waitUntil(t *testing.T, timeout time.Duration, desc string, fn func() bool) {
	t.Helper()
	testwait.WithinPolling(t, timeout, testwait.Poll, desc, fn)
}

// A dead session's interest row is stopped once it is past the reaper's allowance, keeping its
// subscription for the session's next registration, and deleted once it is past the session's
// life (store.StoppedInterestRetention).
func TestE2E_InterestReaperCleanup(t *testing.T) {
	env := setupTestEnv(t)
	const topic = "notifications.slack.*.*.mention"
	if _, err := env.registry.Upsert(store.Interest{SessionID: "ses_stale", MachineID: "test-machine", Dir: "/stale"}, []string{topic}); err != nil {
		t.Fatalf("upsert stale interest: %v", err)
	}

	interest, err := env.registry.Get(context.Background(), "ses_stale")
	if err != nil {
		t.Fatalf("get stale interest: %v", err)
	}
	js, err := env.client.Conn.JetStream(natsgo.MaxWait(5 * time.Second))
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	kv, err := js.KeyValue(bus.InterestBucket)
	if err != nil {
		t.Fatalf("open kv bucket: %v", err)
	}
	// age writes the row back last written by ago and waits for the registry's copy to hold it.
	age := func(row store.Interest, by time.Duration) store.Interest {
		t.Helper()
		row.UpdatedAt = time.Now().Add(-by).UnixMilli()
		data, err := json.Marshal(row)
		if err != nil {
			t.Fatalf("marshal stale interest: %v", err)
		}
		if _, err := kv.Put("ses_stale", data); err != nil {
			t.Fatalf("put stale interest: %v", err)
		}
		waitUntil(t, 5*time.Second, "stale cache update", func() bool {
			current, getErr := env.registry.Get(context.Background(), "ses_stale")
			return getErr == nil && current.UpdatedAt == row.UpdatedAt
		})
		return row
	}
	reap := func(want store.InterestSweep) {
		t.Helper()
		sweep, err := env.registry.Reap(func(string) (bool, error) { return false, nil }, store.InterestReapGrace)
		if err != nil {
			t.Fatalf("reap stale interests: %v", err)
		}
		if sweep != want {
			t.Fatalf("reap = %+v, want %+v", sweep, want)
		}
	}

	age(interest, store.InterestReapGrace+store.InterestRefreshAge+time.Minute)
	reap(store.InterestSweep{Stopped: 1})
	stopped, err := env.registry.Get(context.Background(), "ses_stale")
	if err != nil || !stopped.Stopped || stopped.MachineID != "" || len(stopped.Topics) != 1 || stopped.Topics[0] != topic {
		t.Fatalf("the stopped row = %+v, %v; want it stopped, naming no listener and keeping %s", stopped, err, topic)
	}

	age(stopped, store.StoppedInterestRetention+time.Minute)
	reap(store.InterestSweep{Reaped: 1})
	waitUntil(t, 5*time.Second, "stale interest deletion", func() bool {
		_, getErr := env.registry.Get(context.Background(), "ses_stale")
		return getErr != nil && errors.Is(getErr, natsgo.ErrKeyNotFound)
	})
}
