package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testwait"
)

// staleClaimKV is the role bucket as a direct get of a replicated bucket can answer it: while
// behind is set, a Get of role answers stale, the claim as it stood before a takeover, as a copy of
// the bucket that has not applied the takeover answers. Every other call is the bucket's own, and
// a read from the stream's leader does not go through this handle.
type staleClaimKV struct {
	natsgo.KeyValue
	role   string
	stale  natsgo.KeyValueEntry
	behind atomic.Bool
}

func (kv *staleClaimKV) Get(key string) (natsgo.KeyValueEntry, error) {
	if key == kv.role && kv.behind.Load() {
		return kv.stale, nil
	}
	return kv.KeyValue.Get(key)
}

// A role message stored for the role's previous holder after another session took the role over
// reaches the new holder and not the previous one, even while the copy of the role bucket that the
// listener's direct gets reach has not applied the takeover. The previous holder's session relay
// judges whether the message is still its session's from the role's claim (takeOverFrame); on a
// replicated bucket a direct get of that claim can answer the previous holder, and the relay then
// hands that holder the message: the sender got its answer, and the new holder never receives it.
// The kept lane's handover that follows the relay's copy resolves the holder from the claim too,
// and a stale read there walks the lane for the previous holder and gives the copy up until the
// next sweep. The copy stays behind until the relay has judged the message, then catches up, as a
// copy does. Control: with a current copy the message reaches the new holder, and not the previous
// one.
func TestARoleMessageForThePreviousHolderReachesTheNewHolderWhileTheClaimsCopyIsBehind(t *testing.T) {
	for _, tc := range []struct {
		name   string
		behind bool
	}{
		{"the role bucket's copy is behind the takeover", true},
		{"control: the role bucket's copy is current", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newDelivererRig(t)
			const (
				previous = "ses_copy_behind_previous"
				next     = "ses_copy_behind_next"
				role     = "copy-behind"
			)
			previousStandIn := startSessionStandIn(t, rig.url, previous, answerDone)
			heardPrevious := hear(t, previousStandIn)
			rig.subscribe(t, consumerRegistration(previous, "epoch-1"))
			rig.claim(t, previous, role, "")
			bucket, err := rig.harness.client.JS().KeyValue(bus.RoleClaimBucket)
			if err != nil {
				t.Fatalf("open the role bucket: %v", err)
			}
			stale, err := bucket.Get(role)
			if err != nil {
				t.Fatalf("read %s's claim by %s: %v", role, previous, err)
			}

			nextStandIn := startSessionStandIn(t, rig.url, next, answerDone)
			heardNext := hear(t, nextStandIn)
			rig.subscribe(t, consumerRegistration(next, "epoch-1"))
			rig.claim(t, next, role, "")

			copyBehind := &staleClaimKV{KeyValue: registryRoleKV(t, rig.harness.registry), role: role, stale: stale}
			setRegistryRoleKV(t, rig.harness.registry, copyBehind)
			copyBehind.behind.Store(tc.behind)
			want := map[bool]string{true: previous, false: next}[tc.behind]
			if claim, err := rig.harness.registry.RoleClaim(context.Background(), role); err != nil || claim.HolderSessionID != want {
				t.Fatalf("the registry's direct read of %s's claim names %q (%v), want %q: the copy is not as the row needs it", role, claim.HolderSessionID, err, want)
			}

			event := rig.storeDispatchMessage(t, previous, role)
			if tc.behind {
				testwait.Holds(15*time.Second, 20*time.Millisecond, func() bool {
					return rig.copies.Load() > 0 || heardPrevious.count(event) > 0
				})
				copyBehind.behind.Store(false)
			}
			if !heardNext.await(30*time.Second, event) {
				t.Errorf("the new holder %s received %v within 30s, want the role message %s", next, heardNext.all(), event)
			} else if n := heardNext.count(event); n != 1 {
				t.Errorf("the new holder %s received the role message %s %d times, want once", next, event, n)
			}
			requireNoneHeard(t, heardPrevious, "the previous holder "+previous, event)
		})
	}
}
