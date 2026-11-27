package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvchunk"
	"github.com/sjawhar/envoy/internal/testnats"
)

// openReplicatedRegistry opens a registry whose buckets hold three copies each on a cluster of
// three, on a connection to member 0, and has member 0 lead both, so a write the registry sends
// reaches the leader without a route (testnats.Cluster.RaceKeyValue).
func openReplicatedRegistry(t *testing.T) (*testnats.Cluster, *Registry) {
	t.Helper()
	cluster := testnats.StartCluster(t, 3)
	conn := testnats.Connect(t, cluster.Servers[0].URL())
	t.Cleanup(conn.Close)
	reg, err := Open(t.Context(), conn, WithReplicas(3))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(reg.StopWatch)
	for _, bucket := range []string{bus.InterestBucket, bus.RoleClaimBucket} {
		cluster.MoveStreamLeader(t, "KV_"+bucket, 0)
	}
	waitFor(t, 30*time.Second, reg.watcher.Ready)
	return cluster, reg
}

// raceRoles has the next conditional write of reg's role claims lose a race to a write of its key
// still being replicated (testnats.Cluster.RaceKeyValue).
func raceRoles(t *testing.T, cluster *testnats.Cluster, reg *Registry) *testnats.RacedKeyValue {
	t.Helper()
	race := cluster.RaceKeyValue(t, reg.roleKV.Legacy(), 1)
	reg.roleKV = bus.NewKeyValue(race, reg.roleKV.WatchHandle(), reg.roleKV.Streams())
	return race
}

// Each conditional write of an interest record or a role claim that loses to a write of its key
// still being replicated, on buckets of three copies, is read as the lost compare-and-swap it is,
// as one that loses to a write already stored is: the bucket answers it 10164, which
// nats.ErrKeyExists does not match, where a bucket of one copy answers 10071, which it does.
// Before, each row's write answered the loss as a failure of the bucket.
func TestAWriteThatLosesToAReplicatingWriteIsALostRace(t *testing.T) {
	cluster, reg := openReplicatedRegistry(t)
	ctx := context.Background()
	putRecord := func(t *testing.T, sessionID string) (Interest, []byte, uint64) {
		t.Helper()
		row := Interest{SessionID: sessionID, MachineID: "m1", Topics: []string{"notifications.replicated"}}
		value, err := json.Marshal(row)
		if err != nil {
			t.Fatalf("encode %s's record: %v", sessionID, err)
		}
		revision, err := reg.watcher.KV().Put(sessionID, value)
		if err != nil {
			t.Fatalf("put %s's record: %v", sessionID, err)
		}
		return row, value, revision
	}
	// claim claims role for sessionID and returns once every copy of the role claims holds the
	// claim, so the reads after it, which any copy may answer, read it.
	claim := func(t *testing.T, sessionID, instance, role string) {
		t.Helper()
		if _, err := reg.SetRoleWithPrevious(ctx, RoleClaimRequest{SessionID: sessionID, MachineID: "m1", Role: role, ClientInstanceID: instance}); err != nil {
			t.Fatalf("claim %s for %s: %v", role, sessionID, err)
		}
		cluster.AwaitReplicated(t, "KV_"+bus.RoleClaimBucket)
	}
	const holder, claimant = "ses_replicated_holder", "ses_replicated_claimant"

	t.Run("a conditional delete of an interest record keeps the record", func(t *testing.T) {
		const sessionID = "ses_replicated_interest_delete"
		_, _, revision := putRecord(t, sessionID)
		race := cluster.RaceKeyValue(t, reg.watcher.KV().Legacy(), 1)
		useKV(t, reg, race)

		deleted, err := reg.deleteInterestAt(ctx, sessionID, revision, kvchunk.Layout{})
		if err != nil || deleted {
			t.Fatalf("deleteInterestAt = %t, %v; want the record kept and no error", deleted, err)
		}
		if race.Lost() != 1 {
			t.Fatalf("%d deletes lost the race, want 1", race.Lost())
		}
		if _, err := reg.watcher.KV().Get(sessionID); err != nil {
			t.Fatalf("read the kept record: %v", err)
		}
	})

	t.Run("a conditional write of an interest record is refused for a plan over the new row", func(t *testing.T) {
		const sessionID = "ses_replicated_interest_write"
		row, value, revision := putRecord(t, sessionID)
		race := cluster.RaceKeyValue(t, reg.watcher.KV().Legacy(), 1)
		useKV(t, reg, race)

		written, _, err := reg.putInterestAt(ctx, sessionID, row, value, revision, kvchunk.Layout{})
		if err != nil || written.Stored {
			t.Fatalf("putInterestAt took the write: %+v, %v; want it refused and no error", written, err)
		}
		if race.Lost() != 1 {
			t.Fatalf("%d writes lost the race, want 1", race.Lost())
		}
	})

	t.Run("a renewal of a role claim is dropped", func(t *testing.T) {
		const role = "replicated-renewal"
		claim(t, holder, roleClaimInstance, role)
		race := raceRoles(t, cluster, reg)

		renewed, err := reg.renewRoleClaim(ctx, role, holder, roleClaimInstance)
		if err != nil || renewed {
			t.Fatalf("renewRoleClaim = %t, %v; want the renewal dropped and no error", renewed, err)
		}
		if race.Lost() != 1 {
			t.Fatalf("%d renewals lost the race, want 1", race.Lost())
		}
	})

	t.Run("a release of an expired role claim is superseded", func(t *testing.T) {
		const role = "replicated-expired"
		claim(t, holder, roleClaimInstance, role)
		read := storedRoleClaim(t, reg, role)
		race := raceRoles(t, cluster, reg)

		release, err := reg.releaseReadExpiredRoleClaim(ctx, role, read, 0)
		if err != nil || release != ExpiredRoleClaimSuperseded {
			t.Fatalf("releaseReadExpiredRoleClaim = %v, %v; want superseded and no error", release, err)
		}
		if race.Lost() != 1 {
			t.Fatalf("%d deletes lost the race, want 1", race.Lost())
		}
		if stored := storedRoleClaim(t, reg, role); stored.HolderSessionID != holder {
			t.Fatalf("the role is held by %q, want %q", stored.HolderSessionID, holder)
		}
	})

	t.Run("a release of a role claim deletes it at the revision it reads again", func(t *testing.T) {
		const role = "replicated-release"
		claim(t, holder, roleClaimInstance, role)
		read := storedRoleClaim(t, reg, role)
		race := raceRoles(t, cluster, reg)

		stands, err := reg.releaseReadRoleClaim(ctx, holder, role, read, RoleReleaseAuthority{ClientInstanceID: roleClaimInstance})
		if err != nil || stands {
			t.Fatalf("releaseReadRoleClaim = %t, %v; want the claim freed and no error", stands, err)
		}
		if race.Lost() != 1 {
			t.Fatalf("%d deletes lost the race, want 1", race.Lost())
		}
		cluster.AwaitReplicated(t, "KV_"+bus.RoleClaimBucket)
		if stored := storedRoleClaim(t, reg, role); stored.Revision != 0 {
			t.Fatalf("the role is claimed at revision %d by %s, want it free", stored.Revision, stored.HolderSessionID)
		}
	})

	t.Run("an outright claim of a held role writes again over the claim it reads again", func(t *testing.T) {
		const role = "replicated-outright"
		claim(t, holder, roleClaimInstance, role)
		race := raceRoles(t, cluster, reg)

		claim(t, claimant, roleClaimSecondInstance, role)
		if race.Lost() != 1 {
			t.Fatalf("%d claims lost the race, want 1", race.Lost())
		}
		if stored := storedRoleClaim(t, reg, role); stored.HolderSessionID != claimant {
			t.Fatalf("the role is held by %q, want %q", stored.HolderSessionID, claimant)
		}
	})

	t.Run("a conditional claim of a held role is a revision mismatch", func(t *testing.T) {
		const role = "replicated-conditional"
		claim(t, holder, roleClaimInstance, role)
		required := storedRoleClaim(t, reg, role).Revision
		race := raceRoles(t, cluster, reg)

		_, err := reg.SetRoleWithPrevious(ctx, RoleClaimRequest{SessionID: claimant, MachineID: "m1", Role: role, ClientInstanceID: roleClaimSecondInstance, IfClaimRevision: &required})
		var mismatch *ErrRoleClaimRevisionMismatch
		if !errors.As(err, &mismatch) {
			t.Fatalf("the conditional claim = %v, want *ErrRoleClaimRevisionMismatch", err)
		}
		if race.Lost() != 1 {
			t.Fatalf("%d claims lost the race, want 1", race.Lost())
		}
		if stored := storedRoleClaim(t, reg, role); stored.HolderSessionID != holder {
			t.Fatalf("the role is held by %q, want %q", stored.HolderSessionID, holder)
		}
	})
}
