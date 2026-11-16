package store

import (
	"context"
	"errors"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
)

// withoutInterestReassert and withoutRoleReassert are the registry's caches with the write-back
// left out: a read of a recreated bucket begins exactly as the registry's own does and writes
// nothing back. They are the control of the test below.
type withoutInterestReassert struct{ interestCache }

func (c withoutInterestReassert) BeginRead(read kvwatch.Read) { c.r.beginInterestRead(read) }

type withoutRoleReassert struct{ roleCache }

func (c withoutRoleReassert) BeginRead(read kvwatch.Read) { c.r.beginRoleRead(read) }

// A role claim and the interest row it wrote, held by the registry's copies when both buckets are
// deleted and created again empty, are in the new buckets once the reads that find them complete:
// each read writes back what its copy held (reassertInterests, reassertRoleClaims). The control
// runs the same schedule with the write-back left out, and both are missing.
func TestRecreatedInterestAndRoleBucketsGetBackWhatTheCopiesHeld(t *testing.T) {
	for _, reassert := range []bool{true, false} {
		name := "write-back"
		if !reassert {
			name = "control: no write-back"
		}
		t.Run(name, func(t *testing.T) {
			conn, cleanup := connectNATS(t)
			defer cleanup()
			reg, err := Open(conn, WithReplicas(1))
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			t.Cleanup(reg.StopWatch)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := reg.WaitForCacheReady(ctx); err != nil {
				t.Fatalf("wait for the caches: %v", err)
			}
			reg.watcher.End(ctx)
			reg.roleWatcher.End(ctx)
			var interests kvwatch.Cache = interestCache{reg}
			var roles kvwatch.Cache = roleCache{reg}
			if !reassert {
				interests, roles = withoutInterestReassert{interestCache{reg}}, withoutRoleReassert{roleCache{reg}}
			}
			reg.watcher = kvwatch.New("interest registry", reg.watcher.KV(), interests, kvwatch.WithStreamProbeInterval(0))
			reg.roleWatcher = kvwatch.New("role claims", reg.roleWatcher.KV(), roles, kvwatch.WithStreamProbeInterval(0))
			reg.watcher.Start()
			reg.roleWatcher.Start()
			waitUntil(t, "the rebuilt watchers' first reads", func() bool { return reg.watcher.Following() && reg.roleWatcher.Following() })

			if _, err := reg.SetRole("ses_holder", "m1", "reviewer", false); err != nil {
				t.Fatalf("claim reviewer: %v", err)
			}
			waitUntil(t, "the role copy to name the holder", func() bool {
				roles, err := reg.RolesHeldBy("ses_holder")
				return err == nil && len(roles) == 1
			})

			js, err := conn.JetStream()
			if err != nil {
				t.Fatalf("jetstream: %v", err)
			}
			newBuckets := map[string]natsgo.KeyValue{}
			for _, bucket := range []string{bus.InterestBucket, bus.RoleClaimBucket} {
				if err := js.DeleteKeyValue(bucket); err != nil {
					t.Fatalf("delete %s: %v", bucket, err)
				}
				newBuckets[bucket] = testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: bucket, Storage: natsgo.FileStorage, Replicas: 1})
			}
			if err := reg.Rewatch(conn); err != nil {
				t.Fatalf("Rewatch: %v", err)
			}
			waitUntil(t, "the reads of the new buckets", func() bool {
				return reg.watcher.Epoch() == 1 && reg.watcher.Following() && reg.roleWatcher.Epoch() == 1 && reg.roleWatcher.Following()
			})

			_, interestErr := newBuckets[bus.InterestBucket].Get("ses_holder")
			claimEntry, claimErr := newBuckets[bus.RoleClaimBucket].Get("reviewer")
			if !reassert {
				if !errors.Is(interestErr, natsgo.ErrKeyNotFound) || !errors.Is(claimErr, natsgo.ErrKeyNotFound) {
					t.Fatalf("with no write-back the new buckets hold the row (%v) and the claim (%v), want both missing", interestErr, claimErr)
				}
				return
			}
			if interestErr != nil {
				t.Fatalf("the new interest bucket's row of ses_holder: %v, want it written back", interestErr)
			}
			if claimErr != nil {
				t.Fatalf("the new role bucket's claim of reviewer: %v, want it written back", claimErr)
			}
			claim, err := DecodeRoleClaim(claimEntry.Value())
			if err != nil || claim.HolderSessionID != "ses_holder" || claim.ClaimedAt == 0 {
				t.Fatalf("the written-back claim = %+v, %v; want ses_holder's claim with its claimed_at", claim, err)
			}
			if roles, err := reg.RolesHeldBy("ses_holder"); err != nil || len(roles) != 1 {
				t.Fatalf("RolesHeldBy(ses_holder) after the read = %v, %v; want reviewer", roles, err)
			}
		})
	}
}

// A role released in a recreated bucket that had not been given its claim back stays released:
// the release found no claim, and the copy still held one it frees, so it marked the role's key
// (releaseUnreturned), which refuses the write-back of the read that finds the bucket.
func TestARoleReleasedBeforeItsClaimCameBackStaysReleased(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	reg, err := Open(conn, WithReplicas(1))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(reg.StopWatch)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := reg.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("wait for the caches: %v", err)
	}
	reg.roleWatcher.End(ctx)
	reg.roleWatcher = kvwatch.New("role claims", reg.roleWatcher.KV(), roleCache{reg}, kvwatch.WithStreamProbeInterval(0))
	reg.roleWatcher.Start()
	waitUntil(t, "the rebuilt role watcher's first read", reg.roleWatcher.Following)
	if _, err := reg.SetRole("ses_holder", "m1", "reviewer", false); err != nil {
		t.Fatalf("claim reviewer: %v", err)
	}
	waitUntil(t, "the role copy to name the holder", func() bool {
		roles, err := reg.RolesHeldBy("ses_holder")
		return err == nil && len(roles) == 1
	})

	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if err := js.DeleteKeyValue(bus.RoleClaimBucket); err != nil {
		t.Fatalf("delete the role bucket: %v", err)
	}
	recreated := testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: bus.RoleClaimBucket, Storage: natsgo.FileStorage, Replicas: 1})
	if _, err := reg.Remove(ctx, "ses_holder", []string{contracts.RoleTopicPrefix + "reviewer"}, RoleReleaseAuthority{}); err != nil {
		t.Fatalf("release reviewer: %v", err)
	}
	if err := reg.Rewatch(conn); err != nil {
		t.Fatalf("Rewatch: %v", err)
	}
	waitUntil(t, "the read of the new role bucket", func() bool { return reg.roleWatcher.Epoch() == 1 && reg.roleWatcher.Following() })
	if entry, err := recreated.Get("reviewer"); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("the new role bucket holds reviewer's claim %v (%v) after its release, want none", entry, err)
	}
	if roles, err := reg.RolesHeldBy("ses_holder"); err != nil || len(roles) != 0 {
		t.Fatalf("RolesHeldBy(ses_holder) after the read = %v, %v; want none", roles, err)
	}
}

func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
