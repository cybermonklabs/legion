//go:build odatrace

package consumernames

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	selection "github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/internal/holdrecord"
)

// Families 3 and 4 of the design's forced-order tests (§7) for restore, each a race regression on
// the writer hold's stack: its control, an earlier round's rule (restoreRuleSet), runs first and
// loses the row's messages; the treatment, the current rule, delivers them on the restored fixed
// name, which the previous release pulls by name. Each asserts its hold point was reached and that
// the messages it expects are not none. Each runs with the stream and the selection bucket at one
// copy and at three on the stack's three servers, on the nats-server forced-order.sh links: on
// 2.10.29, which the session consumers refuse by design, each proves that refusal and skips.

// restoreReplicaCounts are the copies each row runs its stream and selection bucket at.
var restoreReplicaCounts = []int{1, 3}

// restoreSkipUnlessRan skips a row none of whose subtests got past the session family's version
// gate (restoreRig.sessionFamilyOrSkip), so a row on a server the family refuses reads as skipped,
// never as a pass over skipped subtests.
func restoreSkipUnlessRan(t *testing.T, ran bool) {
	t.Helper()
	if !ran {
		t.Skip("every subtest skipped: the session family refuses this nats-server by design")
	}
}

// restoreFamily3Rig is family 3's interrupted-conversion rollback, at replicas copies: session
// lower's messages 1 and 2 on its own subject, 3 to 20 on another, its fixed name at floor 20 over
// both; convert saved S = 1 for it (op = {replace, old F, candidate N}, start 1) and stopped before
// it deleted F, with round 17's generation-free last floor of 20 in the record; then five messages
// more on its subject, and the rollback hold set.
func restoreFamily3Rig(t *testing.T, replicas int, ran *bool) (*restoreRig, string, []uint64) {
	rig := newRestoreRigOf(t, replicas)
	rig.sessionFamilyOrSkip()
	*ran = true
	own := "notifications.agent.lower"
	lower := rig.publish(own, 2)
	rig.publish("notifications.agent.other", 18)
	rig.fixed(restoreFixedConfig(sessionFixedPrefix+"lower", own, "notifications.agent.other"), 20)
	rig.cutover(restoreConversion{fixed: sessionFixedPrefix + "lower", start: 1, interrupted: true,
		edit: func(record *selection.Record, _ string) {
			record.LastFloor = &selection.Floor{Floor: 20, Name: sessionFixedPrefix + "lower"}
		}})
	later := rig.publish(own, 5)
	require.Equal(t, []uint64{1, 2}, lower)
	return rig, rig.heldHold(holdrecord.WorkflowRollback), later
}

// TestForcedOrderRestoreKeepsTheSavedLowerStart (§7 family 3, the interrupted-conversion rollback):
// restore --prepare deletes F because its floor 20 + 1 > S = 1, restore --finish creates F at 1
// after its restart, and messages 1 and 2 arrive. Controls: round 17's generation-free last floor
// starts at 21, and round 17's phase kept leaves F at 21; each loses 1 and 2.
func TestForcedOrderRestoreKeepsTheSavedLowerStart(t *testing.T) {
	target := sessionFixedPrefix + "lower"
	ran := false
	for _, replicas := range restoreReplicaCounts {
		t.Run(fmt.Sprintf("R%d", replicas), func(t *testing.T) {
			for _, control := range []struct {
				name  string
				rules restoreRuleSet
			}{
				{"control: round 17's generation-free last floor", restoreRuleSet{lastFloorFirst: true}},
				{"control: round 17's phase kept", restoreRuleSet{keepAnyTarget: true}},
			} {
				t.Run(control.name, func(t *testing.T) {
					rig, rollback, later := restoreFamily3Rig(t, replicas, &ran)
					restoreWithRules(t, control.rules)
					code, _, stderr := rig.restore(rollback, "--prepare")
					require.Zero(t, code, stderr)
					require.Equal(t, restoreKept, rig.record(familySession, "lower").Restore.Phase, "the control keeps F at floor 20")
					code, _, stderr = rig.restore(rollback, "--finish")
					require.Zero(t, code, stderr)
					require.NoError(t, rig.release(rollback))
					got := rig.delivered(target)
					require.NotContains(t, got, uint64(1), "the control loses message 1")
					require.NotContains(t, got, uint64(2), "the control loses message 2")
					require.Contains(t, got, later[0], "the control delivers past F's floor")
				})
			}
			t.Run("treatment", func(t *testing.T) {
				rig, rollback, later := restoreFamily3Rig(t, replicas, &ran)
				before := rig.record(familySession, "lower")
				require.NotNil(t, before.Op, "the hold point: convert's operation is open")
				require.Equal(t, uint64(20), rig.consumer(target).AckFloor.Stream, "the hold point: F is at floor 20")
				code, _, stderr := rig.restore(rollback, "--prepare")
				require.Zero(t, code, stderr)
				require.Nil(t, rig.consumer(target), "restore --prepare deleted F, since 21 > 1")
				require.Equal(t, &selection.Restore{Target: target, Start: 1, Phase: restoreSourceDeleted}, rig.record(familySession, "lower").Restore)
				code, _, stderr = rig.restore(rollback, "--finish")
				require.Zero(t, code, stderr)
				require.Equal(t, uint64(1), rig.consumer(target).Config.OptStartSeq)
				require.NoError(t, rig.release(rollback))
				got := rig.delivered(target)
				for _, seq := range append([]uint64{1, 2}, later...) {
					require.Contains(t, got, seq, "F at 1 delivers %d", seq)
				}
			})
		})
	}
	restoreSkipUnlessRan(t, ran)
}

// restoreFamily4Rig is family 4's restore hole, at replicas copies: session hole has one pending
// message, on a shared topic only, and none on its own subject; its fixed name was converted, then
// a Delete of its current was killed before floor_recorded (op = {delete, old N}, no start saved,
// last floor 0 read from N on this stream), and N was deleted outside. Session nostart was converted
// and its current deleted outside with no start and no last floor saved. Then the rollback hold is
// set.
func restoreFamily4Rig(t *testing.T, replicas int, ran *bool) (*restoreRig, string, uint64) {
	rig := newRestoreRigOf(t, replicas)
	rig.sessionFamilyOrSkip()
	*ran = true
	shared := rig.publish("notifications.github.shared", 1)
	rig.fixed(restoreFixedConfig(sessionFixedPrefix+"hole", "notifications.agent.hole", "notifications.github.shared"), 0)
	rig.fixed(restoreFixedConfig(sessionFixedPrefix+"nostart", "notifications.agent.nostart"), 0)
	rig.cutover(
		restoreConversion{fixed: sessionFixedPrefix + "hole", start: 1, deleteFresh: true,
			edit: func(record *selection.Record, fresh string) {
				record.Start = 0
				record.Op = &selection.Op{Kind: selection.OpDelete, Old: fresh, ReservedAt: time.Now()}
				record.LastFloor = &selection.Floor{Floor: 0, Name: fresh, StreamID: restoreStreamID}
			}},
		restoreConversion{fixed: sessionFixedPrefix + "nostart", start: 1, deleteFresh: true,
			edit: func(record *selection.Record, _ string) { record.Start = 0 }},
	)
	return rig, rig.heldHold(holdrecord.WorkflowRollback), shared[0]
}

// TestForcedOrderRestoreStartsAtOneAfterTheRestoreHole (§7 family 4, round 16's Restore hole):
// restore starts the hole's fixed name at 1, from the last floor 0 read from its current on this
// stream, and the pending shared-topic message arrives; a name with no start makes restore
// --finish stop, naming it, until restore --key gives it one. Control: round 16's rule
// (first-create) starts at the stream's end, and the message is lost.
func TestForcedOrderRestoreStartsAtOneAfterTheRestoreHole(t *testing.T) {
	target := sessionFixedPrefix + "hole"
	ran := false
	for _, replicas := range restoreReplicaCounts {
		t.Run(fmt.Sprintf("R%d", replicas), func(t *testing.T) {
			t.Run("control: round 16's first-create rule", func(t *testing.T) {
				rig, rollback, shared := restoreFamily4Rig(t, replicas, &ran)
				restoreWithRules(t, restoreRuleSet{firstCreateWhenAbsent: true})
				code, _, stderr := rig.restore(rollback, "--prepare")
				require.Zero(t, code, stderr)
				code, _, stderr = rig.restore(rollback, "--finish")
				require.Zero(t, code, stderr)
				require.Equal(t, shared+1, rig.consumer(target).Config.OptStartSeq, "round 16 starts at the stream's end")
				require.NoError(t, rig.release(rollback))
				require.NotContains(t, rig.delivered(target), shared, "the control loses the shared-topic message")
			})
			t.Run("treatment", func(t *testing.T) {
				rig, rollback, shared := restoreFamily4Rig(t, replicas, &ran)
				before := rig.record(familySession, "hole")
				require.True(t, before.Op != nil && before.Op.Kind == selection.OpDelete && !before.Op.FloorRecorded,
					"the hold point: a Delete killed before floor_recorded")
				require.Nil(t, rig.consumer(before.Current), "the hold point: the current was deleted outside")
				code, stdout, stderr := rig.restore(rollback, "--prepare")
				require.Zero(t, code, stderr)
				require.Equal(t, &selection.Restore{Target: target, Start: 1, Phase: restoreSourceDeleted}, rig.record(familySession, "hole").Restore,
					"restore starts the hole at 1, from the last floor 0 read from its current on this stream")
				require.Contains(t, stdout, "session/nostart has no start")
				require.Equal(t, restoreNoStart, rig.record(familySession, "nostart").Restore.Phase)

				code, _, stderr = rig.restore(rollback, "--finish")
				require.Equal(t, 1, code, "restore --finish refuses while a name has no start")
				require.Contains(t, stderr, "session/nostart")
				stop := rig.lastEntry(rollback, restoreCommandFinish)
				require.Equal(t, "stop", stop.State, "its refusal writes its stop entry")
				require.Contains(t, stop.Outputs["reason"], "session/nostart")
				require.Nil(t, rig.consumer(target), "nothing is created before every name has its start")

				code, _, stderr = rig.restore(rollback, "--key", "nostart", "--from", "1")
				require.Zero(t, code, stderr)
				code, _, stderr = rig.restore(rollback, "--finish")
				require.Zero(t, code, stderr)
				require.True(t, rig.journal(rollback).Complete)
				require.Equal(t, uint64(1), rig.consumer(target).Config.OptStartSeq)
				require.Equal(t, uint64(1), rig.consumer(sessionFixedPrefix+"nostart").Config.OptStartSeq)
				require.NoError(t, rig.release(rollback))
				require.Equal(t, []uint64{shared}, rig.delivered(target), "the shared-topic message arrives")
			})
		})
	}
	restoreSkipUnlessRan(t, ran)
}
