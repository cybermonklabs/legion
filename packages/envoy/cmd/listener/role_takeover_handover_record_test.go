package main

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
)

// handoverRecords is how many of role's takeover records hand previous's messages to successor
// (bus.HandoverSubject).
func (rig *copiesRig) handoverRecords(t *testing.T, role, previous, successor string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	records, err := rig.harness.client.TakeoverRecords(ctx, role)
	if err != nil {
		t.Fatalf("read role %s's takeover records: %v", role, err)
	}
	n := 0
	for _, stored := range records {
		if got, ok := bus.TakeoverSuccessor(stored.Subject); ok && got == successor && stored.Record.Previous == previous {
			n++
		}
	}
	return n
}

// The claim route's handover record (publishHandover): one for a claimant and the session it names,
// however often it claims, and a claim whose record did not store is answered 503 so its client
// claims again, which writes the record.
func TestAClaimNamingTheSessionItReplacesWritesOneHandoverRecord(t *testing.T) {
	t.Run("a claim made again naming the same session", func(t *testing.T) {
		rig := newCopiesRig(t)
		setTakeoverFaults(t, rig.harness.config, takeoverFaultSet{relayCopy: copiesRelayCopyHeld(handoverRole)})
		old, direct, _ := rig.handoverUnread(t, handoverOld, handoverOldProcess, "agent", 2, handoverRole)
		rig.stopCleanly(t, handoverOld, handoverOldProcess, old, handoverRole)

		taker := rig.startTaker(t, handoverNew)
		rig.claimReplacing(t, handoverNew, handoverRole, handoverOld, "")
		received := rig.watch(t, taker, handoverNew, handoverRole, direct...)
		copiesRequireEach(t, received, handoverNew, direct...)
		// The plugin claims its role again at each registration, naming the same session, and the
		// sweep runs the record's passes again.
		rig.claimReplacing(t, handoverNew, handoverRole, handoverOld, "")
		rig.awaitHandoverIdle(t, handoverRole)
		rig.sweepPass()
		rig.awaitHandoverIdle(t, handoverRole)

		t.Run("case: the role holds one handover record from the replaced session to the claimant", func(t *testing.T) {
			if n := rig.handoverRecords(t, handoverRole, handoverOld, handoverNew); n != 1 {
				t.Errorf("role %s holds %d handover records from %s to %s; want exactly one", handoverRole, n, handoverOld, handoverNew)
			}
		})
		t.Run("case: the stream holds one copy of each direct message on the claimant's own subject", func(t *testing.T) {
			for _, eventID := range direct {
				if n := rig.handedOverCopies(t, handoverNew, eventID); n != 1 {
					t.Errorf("%s's own subject holds %d copies of %s; want exactly one", handoverNew, n, eventID)
				}
			}
		})
	})

	t.Run("the record of the handover did not store", func(t *testing.T) {
		rig := newCopiesRig(t)
		var refusing atomic.Bool
		refusing.Store(true)
		setTakeoverFaults(t, rig.harness.config, takeoverFaultSet{
			relayCopy: copiesRelayCopyHeld(handoverRole),
			handoverRecordWrite: func(role, previous, successor string) error {
				if refusing.Load() && role == handoverRole && previous == handoverOld && successor == handoverNew {
					return errors.New("the row refuses the handover record's write")
				}
				return nil
			},
		})
		old, direct, _ := rig.handoverUnread(t, handoverOld, handoverOldProcess, "agent", 1, handoverRole)
		rig.stopCleanly(t, handoverOld, handoverOldProcess, old, handoverRole)

		taker := rig.startTaker(t, handoverNew)
		refused := rig.postClaimReplacing(handoverNew, handoverRole, handoverOld, "")
		t.Run("case: the claim is answered 503 with Retry-After, and it stands", func(t *testing.T) {
			if refused.Code != http.StatusServiceUnavailable || refused.Header().Get("Retry-After") != "1" {
				t.Errorf("the claim whose handover record did not store answered %d (Retry-After %q) %s; want 503 with Retry-After: 1", refused.Code, refused.Header().Get("Retry-After"), refused.Body.String())
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if claim, err := rig.harness.registry.RoleClaim(ctx, handoverRole); err != nil || claim.HolderSessionID != handoverNew {
				t.Errorf("role %s's claim names %q (%v); want %s", handoverRole, claim.HolderSessionID, err, handoverNew)
			}
		})

		refusing.Store(false)
		rig.claimReplacing(t, handoverNew, handoverRole, handoverOld, "")
		received := rig.watch(t, taker, handoverNew, handoverRole, direct...)
		t.Run("case: the claim made again writes the record, and the claimant receives the unread direct message", func(t *testing.T) {
			copiesRequireEach(t, received, handoverNew, direct...)
		})
	})
}

// A handover record's pass copies a message that is no role message to the session that replaced
// the previous one only where its topic is the previous session's own agent subject, in either
// form; a record of a role taken with no session named copies no such message, and no pass copies
// a message to another session or a topic the previous session followed.
func TestAHandoverPassTellsAMessageToTheReplacedSessionByEitherFormOfItsTopic(t *testing.T) {
	for _, c := range []struct {
		name, topic string
		want        takenKind
	}{
		{"the old form of its agent subject", "notifications.agent.ses_gone", takenDirectCopy},
		{"the new form under a project", "notifications.agent.INFRA.ses_gone", takenDirectCopy},
		{"another session's agent subject", "notifications.agent.ses_other", takenNone},
		{"another session's agent subject in the new form", "notifications.agent.INFRA.ses_other", takenNone},
		{"a topic the session followed", "notifications.github.owner.repo.push", takenNone},
	} {
		t.Run(c.name, func(t *testing.T) {
			data := []byte(`{"event_id":"evt-handover","topic":"` + c.topic + `","message":"m"}`)
			if _, kind := takenFrame(data, "some-role", "ses_gone", true); kind != c.want {
				t.Errorf("a handover pass takes a message on %s as %v; want %v", c.topic, kind, c.want)
			}
			if _, kind := takenFrame(data, "some-role", "ses_gone", false); kind != takenNone {
				t.Errorf("a takeover pass of a record that names no successor takes a message on %s as %v; want it copied nowhere", c.topic, kind)
			}
		})
	}
}
