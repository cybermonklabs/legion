package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/session"
)

// sessionReadFault is a session bucket each read of which, a direct get (Get) or a read from the
// bucket stream's leader (NewestFromLeader, bus.LeaderReads), first asks fault about its key: an
// error is the read's answer, and nil lets the read go on to the bucket, a leader read through
// leader, the handle this one stands in for. A resolution reads a holder its cache does not show
// live the one way or the other (roleHolderSessions, leaderHolderSessions), so a fault planted
// here reaches it whichever way it reads.
type sessionReadFault struct {
	natsgo.KeyValue
	leader bus.KeyValue
	fault  func(key string) error
}

func (kv sessionReadFault) Get(key string) (natsgo.KeyValueEntry, error) {
	if err := kv.fault(key); err != nil {
		return nil, err
	}
	return kv.KeyValue.Get(key)
}

func (kv sessionReadFault) NewestFromLeader(ctx context.Context, key string) (natsgo.KeyValueEntry, error) {
	if err := kv.fault(key); err != nil {
		return nil, err
	}
	return kv.leader.NewestFromLeader(ctx, key)
}

// faultSessionReads has every read sessions makes of the session bucket from here on ask fault
// first (sessionReadFault, swapSessionHandle).
func faultSessionReads(t *testing.T, sessions *session.SessionRegistry, fault func(key string) error) {
	t.Helper()
	swapSessionHandle(t, sessions, func(old bus.KeyValue) bus.KeyValue {
		return bus.NewKeyValue(sessionReadFault{KeyValue: old.Legacy(), leader: old, fault: fault}, old.WatchHandle(), old.Streams())
	})
}

// countSessionBucketReads counts every read sessions makes of the session bucket from here on,
// direct gets and leader reads alike (faultSessionReads), as failTheNextSessionRead fails the
// next of them. A resolution of a role's holder reads the holder's session that way whenever this
// listener's cache does not hold it, so the counter is that resolution made visible.
func countSessionBucketReads(t *testing.T, sessions *session.SessionRegistry) *atomic.Int64 {
	t.Helper()
	reads := &atomic.Int64{}
	faultSessionReads(t, sessions, func(string) error {
		reads.Add(1)
		return nil
	})
	return reads
}

// failTheNextSessionRead makes the next bucket read of sessions fail and every read after it
// answer (faultSessionReads), one round trip the bus missed made deterministic, as
// countSessionBucketReads counts them. A resolution of a role's holder reads the holder's session
// that way whenever this listener's cache does not hold it, so the flag it returns reports
// whether that resolution took the failure.
func failTheNextSessionRead(t *testing.T, sessions *session.SessionRegistry) *atomic.Bool {
	t.Helper()
	return failTheNextSessionReadOf(t, sessions, "")
}

// failTheNextSessionReadOf is failTheNextSessionRead for the one session key, or for any key
// where key is empty: the next bucket read of key fails, and the flag it returns reports whether
// a read of key took the failure.
func failTheNextSessionReadOf(t *testing.T, sessions *session.SessionRegistry, key string) *atomic.Bool {
	t.Helper()
	failed := &atomic.Bool{}
	faultSessionReads(t, sessions, func(read string) error {
		if (key == "" || read == key) && failed.CompareAndSwap(false, true) {
			return errors.New("the session bucket's read did not answer")
		}
		return nil
	})
	return failed
}
