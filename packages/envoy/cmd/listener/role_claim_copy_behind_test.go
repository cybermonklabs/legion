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

// staleClaimKV is the role bucket as a direct get of a replicated bucket can answer it: once
// fallBehind has given it a claim, a Get of role answers that claim, as a copy of the bucket that
// has not applied a later takeover answers, until catchUp. Every other call is the bucket's own,
// and a read from the stream's leader does not go through this handle. The registry reads through
// it from before the rig's first claim, so its installation races no read of the bucket.
type staleClaimKV struct {
	natsgo.KeyValue
	role  string
	stale atomic.Pointer[natsgo.KeyValueEntry]
}

func (kv *staleClaimKV) fallBehind(stale natsgo.KeyValueEntry) { kv.stale.Store(&stale) }

func (kv *staleClaimKV) catchUp() { kv.stale.Store(nil) }

func (kv *staleClaimKV) Get(key string) (natsgo.KeyValueEntry, error) {
	if stale := kv.stale.Load(); key == kv.role && stale != nil {
		return *stale, nil
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
// next sweep. The copy stays behind through the relay's judgement and the handover's resolutions,
// until the new holder has received the message or the row's 30 s have passed: a copy that caught
// up while the handover was still resolving would let a handover that reads it pass. Control: with
// a current copy the message reaches the new holder, and not the previous one.
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
			copyBehind := &staleClaimKV{KeyValue: registryRoleKV(t, rig.harness.registry), role: role}
			setRegistryRoleKV(t, rig.harness.registry, copyBehind)
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

			if tc.behind {
				copyBehind.fallBehind(stale)
			}
			want := map[bool]string{true: previous, false: next}[tc.behind]
			if claim, err := rig.harness.registry.RoleClaim(context.Background(), role); err != nil || claim.HolderSessionID != want {
				t.Fatalf("the registry's direct read of %s's claim names %q (%v), want %q: the copy is not as the row needs it", role, claim.HolderSessionID, err, want)
			}

			event := rig.storeDispatchMessage(t, previous, role)
			if tc.behind {
				if !testwait.Holds(15*time.Second, 20*time.Millisecond, func() bool {
					return rig.copies.Load() > 0 || heardPrevious.count(event) > 0
				}) {
					t.Fatalf("the previous holder's relay neither copied the role message %s to the kept lane nor handed it to %s within 15s: no read the copy behind is there for was made", event, previous)
				}
			}
			heard := heardNext.await(30*time.Second, event)
			copyBehind.catchUp()
			if !heard {
				t.Errorf("the new holder %s received %v within 30s, want the role message %s", next, heardNext.all(), event)
			} else if n := heardNext.count(event); n != 1 {
				t.Errorf("the new holder %s received the role message %s %d times, want once", next, event, n)
			}
			requireNoneHeard(t, heardPrevious, "the previous holder "+previous, event)
		})
	}
}
