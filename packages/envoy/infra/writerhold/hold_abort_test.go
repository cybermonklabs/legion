package writerhold

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/writerhold/holdtest"
	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/natsgrant"
)

// journal reads hold id's journal.
func (h *heldStack) journal(id string) holdrecord.Done {
	h.t.Helper()
	d, _, err := h.s.Records.Journal(context.Background(), id)
	require.NoError(h.t, err)
	return d
}

// hubRestoreHold sets a hub-restore hold through `hold set --workflow hub-restore`, which reads every
// claim's disk into the journal's step-1 map before the row turns held, and returns its id.
func (h *heldStack) hubRestoreHold() string {
	h.t.Helper()
	h.awaitStreamLeaders()
	code, stdout, _ := h.run(actionSet, "--workflow", string(holdrecord.WorkflowHubRestore), "--reason", "a hub restore")
	require.Zero(h.t, code, "hold set --workflow hub-restore")
	row := h.row()
	require.Equal(h.t, natsgrant.OperatorPhaseHeld, row.Phase)
	require.Contains(h.t, stdout, row.HoldID)
	step1, ok := holdrecord.Step1(h.journal(row.HoldID))
	require.True(h.t, ok, "hold set wrote no step-1 map")
	for i := range h.s.Cluster.Servers {
		require.NotEmpty(h.t, step1["step1_disk_id."+holdtest.Claim(i)], "the step-1 map lacks %s", holdtest.Claim(i))
	}
	return row.HoldID
}

// hold abort. From prepare it deletes the hold's cohort record, then moves the row to released; a
// held row is refused in every workflow but hub-restore. In hub-restore it is admitted and final
// before any done prove, with every claim still on its step-1 disk id, and releases the hold; it
// is refused at its disk check once a restore has landed, writing a stop entry, after which a
// prove of the restored claim is admitted, and once that prove is done, abort is refused
// ErrExcluded. An abort killed before its row move reruns and releases; one killed after the move
// leaves the row released, so the next hold is prepared. Controls: an abort intent with no stop
// entry leaves every other command refused ErrBusy; v50's order, the done entry before the move,
// leaves a complete record over a held row whose rerun is refused ErrAlreadyDone; and a fresh
// reading at abort's start, in place of the step-1 map, passes the restored disk. It fails when
// abort releases over a restored disk, or leaves the hold with no way out.
func TestHoldAbort(t *testing.T) {
	h := newHeldStack(t)
	h.seedProtectedStreams()
	ctx := context.Background()

	// From prepare: the cohort record goes, then the row.
	id := h.prepared(holdrecord.WorkflowCutover)
	_, err := h.s.Records.PutRecord(ctx, id, "cohort", "cohort", 0, []byte(`{"boundary":1}`))
	require.NoError(t, err)
	code, _, _ := h.run(actionAbort, "--hold", id, "--reason", "abort from prepare")
	require.Zero(t, code)
	require.Equal(t, natsgrant.OperatorPhaseReleased, h.row().Phase)
	cohort, err := h.s.Records.Records(ctx, id, "cohort")
	require.NoError(t, err)
	require.Empty(t, cohort, "abort left the cohort record")

	// A held row outside hub-restore is refused.
	id = h.prepared(holdrecord.WorkflowBucketsCreate)
	h.heldWithoutTheSteps(id)
	code, _, stderr := h.run(actionAbort, "--hold", id, "--reason", "abort a held buckets hold")
	require.Equal(t, 1, code)
	require.Contains(t, stderr, "where abort is refused")
	require.Equal(t, natsgrant.OperatorPhaseHeld, h.row().Phase)
	require.NoError(t, abortHeldForTest(ctx, h, id))

	// In hub-restore, before any prove, on every step-1 disk: admitted, final, released.
	id = h.hubRestoreHold()
	code, _, _ = h.run(actionAbort, "--hold", id, "--reason", "nothing was restored")
	require.Zero(t, code)
	require.Equal(t, natsgrant.OperatorPhaseReleased, h.row().Phase)
	d := h.journal(id)
	require.Equal(t, "done", latestState(d, holdrecord.CommandAbort))
	require.True(t, d.Complete, "abort's done entry did not complete the record")

	// A restore lands on claim 0: abort is refused at its disk check, with a stop entry, and a
	// prove of the restored claim is then admitted and done; abort after it is refused ErrExcluded.
	id = h.hubRestoreHold()
	h.disks.Remake(0)
	code, _, stderr = h.run(actionAbort, "--hold", id, "--reason", "abort over a restore")
	require.Equal(t, 1, code)
	require.Contains(t, stderr, holdtest.Claim(0))
	require.Equal(t, natsgrant.OperatorPhaseHeld, h.row().Phase, "abort released over a restored disk")
	require.Equal(t, "stop", latestState(h.journal(id), holdrecord.CommandAbort), "the refused abort wrote no stop entry")
	// The control of R27-C: the same claims read afresh at abort's start match themselves, so an
	// abort that compared with a fresh reading would have released over the restore.
	fresh, _, err := readDisks(ctx, h.op, StepPreflight)
	require.NoError(t, err)
	require.NoError(t, sameDisks(StepPreflight, "a fresh reading", fresh, fresh), "the control")
	code, _, _ = h.run(actionProve, "--hold", id, "--restored", holdtest.Claim(0)+"=pvc-"+holdtest.Pod(0))
	require.Zero(t, code, "the prove after the refused abort")
	require.Equal(t, "done", latestState(h.journal(id), holdrecord.CommandProve))
	code, _, stderr = h.run(actionAbort, "--hold", id, "--reason", "abort after a done prove")
	require.Equal(t, 1, code)
	require.Contains(t, stderr, holdrecord.ErrExcluded.Error())
	require.NoError(t, abortHeldForTest(ctx, h, id))

	// The control of the stop entry: an abort intent with no stop leaves every command ErrBusy.
	id = h.hubRestoreHold()
	_, err = h.s.Records.Admit(ctx, id, holdrecord.Request{Command: holdrecord.CommandAbort}, nil)
	require.NoError(t, err)
	_, err = h.s.Records.Admit(ctx, id, holdrecord.Request{Command: holdrecord.CommandProve, DisksMoved: true}, nil)
	require.ErrorIs(t, err, holdrecord.ErrBusy, "the control: a prove was admitted over abort's pending intent")
	// That intent is an abort killed before its row move: its rerun is admitted, rechecks the disks
	// (all on step 1's) and releases.
	code, _, _ = h.run(actionAbort, "--hold", id, "--reason", "the rerun of a killed abort")
	require.Zero(t, code, "the rerun of an abort killed before its move")
	require.Equal(t, natsgrant.OperatorPhaseReleased, h.row().Phase)

	// An abort killed after its move and before its done entry: the row is released, and the next
	// hold is prepared at once.
	id = h.hubRestoreHold()
	_, err = h.s.Records.Admit(ctx, id, holdrecord.Request{Command: holdrecord.CommandAbort}, nil)
	require.NoError(t, err)
	_, err = h.s.Records.MoveRow(ctx, h.row().Gen, natsgrant.OperatorPhaseReleased, id, "abort's move, then its crash")
	require.NoError(t, err)
	next := h.prepared(holdrecord.WorkflowBucketsCreate)
	require.NotEqual(t, id, next)
	code, _, _ = h.run(actionAbort, "--hold", next, "--reason", "end the next hold")
	require.Zero(t, code)

	// The control of v50's order: the done entry before the move, killed between the two, leaves
	// a complete record over a held row, and the rerun is refused ErrAlreadyDone.
	id = h.hubRestoreHold()
	_, err = h.s.Records.Admit(ctx, id, holdrecord.Request{Command: holdrecord.CommandAbort}, nil)
	require.NoError(t, err)
	require.NoError(t, h.s.Records.AppendDone(ctx, id, holdrecord.CommandAbort, nil))
	code, _, stderr = h.run(actionAbort, "--hold", id, "--reason", "the rerun after v50's order")
	require.Equal(t, 1, code, "the control")
	require.Contains(t, stderr, holdrecord.ErrAlreadyDone.Error())
	require.Equal(t, natsgrant.OperatorPhaseHeld, h.row().Phase)
}
