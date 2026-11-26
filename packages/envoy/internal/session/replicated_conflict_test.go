package session

import (
	"context"
	"errors"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

// openReplicatedRegistry opens a registry whose bucket holds three copies on a cluster of three,
// on a connection to member 0, and has member 0 lead the bucket, so a write the registry sends
// reaches the leader without a route (testnats.Cluster.RaceKeyValue).
func openReplicatedRegistry(t *testing.T) (*testnats.Cluster, *SessionRegistry) {
	t.Helper()
	cluster := testnats.StartCluster(t, 3)
	conn := testnats.Connect(t, cluster.Servers[0].URL())
	t.Cleanup(conn.Close)
	reg, err := OpenSessionRegistry(t.Context(), conn, WithSessionReplicas(3), WithSessionTTL(10*time.Second))
	if err != nil {
		t.Fatalf("OpenSessionRegistry: %v", err)
	}
	t.Cleanup(reg.StopWatch)
	cluster.MoveStreamLeader(t, "KV_"+bus.SessionBucket, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := reg.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("WaitForCacheReady: %v", err)
	}
	return cluster, reg
}

// A conditional delete of a session's registration that loses to a write of that registration
// still being replicated, on a bucket of three copies, is read as the lost compare-and-swap it is,
// as one that loses to a write already stored is: the bucket answers it 10164, which
// nats.ErrKeyExists does not match. DeleteIf runs its read and its delete once more and deletes the
// entry, and the gone-session reaper keeps the registration and reports no error. Before, both
// answered the loss as a failure of the bucket.
func TestASessionDeleteThatLosesToAReplicatingWriteIsALostRace(t *testing.T) {
	cluster, reg := openReplicatedRegistry(t)
	const instanceID = "4f1c2a9e8b7d6c5a4f1c2a9e8b7d6c5a"
	// Any copy may answer a read, and the leader answers a write once a quorum holds it, so each
	// read of what a write left waits until every copy holds it.
	replicated := func(t *testing.T) { cluster.AwaitReplicated(t, "KV_"+bus.SessionBucket) }

	t.Run("a conditional delete", func(t *testing.T) {
		const sessionID = "ses_replicated_delete_if"
		putForInstanceDelete(t, reg, sessionID, instanceID)
		replicated(t)
		race := cluster.RaceKeyValue(t, reg.watcher.KV().Legacy(), 1)
		useKV(t, reg, race)

		outcome, err := reg.DeleteIf(context.Background(), sessionID, DeleteConditions{ClientInstanceID: instanceID})
		if err != nil {
			t.Fatalf("DeleteIf: %v", err)
		}
		if outcome != EntryDeleted {
			t.Fatalf("DeleteIf = %s, want %s where a write being replicated lost it one compare-and-swap", outcome, EntryDeleted)
		}
		if race.Lost() != 1 {
			t.Fatalf("%d deletes lost the race, want 1", race.Lost())
		}
		replicated(t)
		if _, err := reg.watcher.KV().Get(sessionID); !errors.Is(err, natsgo.ErrKeyNotFound) {
			t.Fatalf("read the deleted key: %v, want nats.ErrKeyNotFound", err)
		}
	})

	t.Run("the reaper's delete of a gone session", func(t *testing.T) {
		const sessionID = "ses_replicated_gone"
		putForInstanceDelete(t, reg, sessionID, instanceID)
		replicated(t)
		entry, err := reg.watcher.KV().Get(sessionID)
		if err != nil {
			t.Fatalf("read the registration: %v", err)
		}
		race := cluster.RaceKeyValue(t, reg.watcher.KV().Legacy(), 1)
		useKV(t, reg, race)

		deleted, err := reg.deleteGone(context.Background(), goneCandidate{sessionID: sessionID, revision: entry.Revision()})
		if err != nil || deleted {
			t.Fatalf("deleteGone = %t, %v; want the registration kept and no error", deleted, err)
		}
		if race.Lost() != 1 {
			t.Fatalf("%d deletes lost the race, want 1", race.Lost())
		}
		if _, err := reg.watcher.KV().Get(sessionID); err != nil {
			t.Fatalf("read the kept registration: %v", err)
		}
	})
}
