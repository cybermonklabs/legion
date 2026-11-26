package legionfacts

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
)

// TestAMoveOfTheAdmissionBlockAloneRewritesTheWaitingReasonItMoves: a stored row's waiting reason
// reads the daemon's admission block (reasonOf), which the sampler records for the project under
// legion.AdmissionKey and on no issue. An admitted issue with no slot of its own whose blocker
// finishes is moved from the block's Blocked to its Waiting, and nothing of the issue itself is
// observed. The pass that reads the new block must rewrite that issue's reason from `blocked` to
// `ready`, as a computation of the issue answers it, and rewrite no other row: every other open
// row's reason is the same under both blocks.
func TestAMoveOfTheAdmissionBlockAloneRewritesTheWaitingReasonItMoves(t *testing.T) {
	run := newConsumer(t, 3)
	issue, other, blocker := run.fixture.Open[0], run.fixture.Open[1], run.fixture.Keys[1]
	run.observe(t, issue, legion.FieldPhase, legion.PhaseAdmitted, run.at.Add(-time.Hour))
	run.observe(t, legion.AdmissionKey, legion.FieldAdmission, legion.Admission{
		Blocked: []legion.BlockedIssue{{Issue: issue, BlockedBy: []string{blocker}}},
	}, run.at.Add(-time.Hour))
	run.pass(t)
	before := run.rows(t)
	if reason := before[issue].WaitingReason; reason != legion.WaitingBlocked {
		t.Fatalf("%s is admitted with no slot and the block holds it on %s, and its row waits for %q, want %q: this test examined nothing",
			issue, blocker, reason, legion.WaitingBlocked)
	}
	if row, has := before[other]; !has || row.ClosedAt != nil {
		t.Fatalf("%s holds no open row, and this test needs one whose reason the block does not move", other)
	}

	run.at = run.at.Add(time.Minute)
	run.observe(t, legion.AdmissionKey, legion.FieldAdmission, legion.Admission{Waiting: []string{issue}}, run.at)
	changed := run.pass(t)
	after := run.rows(t)
	fresh := computedPlacement(t, run, issue)

	if reason := after[issue].WaitingReason; reason != legion.WaitingReady {
		t.Errorf("the block moved %s from Blocked to Waiting and its row waits for %q, want %q", issue, reason, legion.WaitingReady)
	}
	if difference := rowDifference(after[issue], fresh); difference != "" {
		t.Errorf("%s's stored row stands apart from its computation: %s", issue, difference)
	}
	if !slices.Equal(changed, []string{issue}) {
		t.Errorf("the pass that read the block alone changed %v, want %s alone: no other row's values moved", changed, issue)
	}
	if !after[other].UpdatedAt.Equal(before[other].UpdatedAt) {
		t.Errorf("%s is open and its reason did not move, and its row was rewritten at %s (it was written at %s)",
			other, after[other].UpdatedAt.UTC(), before[other].UpdatedAt.UTC())
	}
	t.Logf("%s waits for %q, then %q after the block alone moved it (the computation answers %q); the pass changed %v, and %s (open) was written at %s both times",
		issue, before[issue].WaitingReason, after[issue].WaitingReason, fresh.WaitingReason, changed, other, after[other].UpdatedAt.UTC())
}

// TestAMoveOfTheAdmissionBlockThatMovesNoReasonRecomputesNoRow: a row's waiting reason reads three
// things of the daemon's admission block (legion.NewWaitingLine): whether a block is recorded, its
// Waiting and its Blocked. The sampler records the block whenever any part of it moves, its cap,
// its active slots and its live trees among them, and a move of those alone moves no reason, so
// the pass that reads it recomputes no row; nor does a move of the daemon's settings, observed
// under the same key, which no row reads. An open row left stale by hand shows it, since a pass
// that recomputed the project's open rows (openRows) would write that row back as its
// computation answers it; a move of the Waiting after it does recompute them, which shows the
// stale row is one a recompute rewrites.
func TestAMoveOfTheAdmissionBlockThatMovesNoReasonRecomputesNoRow(t *testing.T) {
	run := newConsumer(t, 3)
	issue, other := run.fixture.Open[0], run.fixture.Open[1]
	block := legion.Admission{Waiting: []string{issue}}
	run.observe(t, issue, legion.FieldPhase, legion.PhaseAdmitted, run.at.Add(-time.Hour))
	run.observe(t, legion.AdmissionKey, legion.FieldAdmission, block, run.at.Add(-time.Hour))
	run.pass(t)
	if row, has := run.rows(t)[other]; !has || row.ClosedAt != nil {
		t.Fatalf("%s holds no open row, and this test needs one a recompute of the open rows rewrites", other)
	}
	const stale = "stale by hand"
	run.exec(t, "leave the row of "+other+" stale", `update issue_facts set waiting_reason = $3 where project_key = $1 and issue_key = $2`,
		run.fixture.Project, other, stale)

	run.at = run.at.Add(time.Minute)
	block.Active = []string{issue}
	run.observe(t, legion.AdmissionKey, legion.FieldAdmission, block, run.at)
	run.observe(t, legion.AdmissionKey, legion.FieldSettings, map[string]any{"maxSlots": 4}, run.at)
	changed := run.pass(t)
	if read, head := run.cursors(t)[observationsCursor].position, run.feedHeads(t)[observationsCursor]; read != head {
		t.Fatalf("the pass left the observations cursor at %d, below the block's row at %d: it read no move of the block", read, head)
	}
	if len(changed) != 0 {
		t.Errorf("the pass that read a move of the block's active slots and of the settings alone changed %v, want no row", changed)
	}
	left := run.rows(t)[other].WaitingReason
	if left != stale {
		t.Errorf("the pass that read a move of the block's active slots and of the settings alone recomputed %s's open row, which now waits for %q", other, left)
	}

	run.at = run.at.Add(time.Minute)
	block.Waiting = []string{issue, other}
	run.observe(t, legion.AdmissionKey, legion.FieldAdmission, block, run.at)
	run.pass(t)
	recomputed := run.rows(t)[other].WaitingReason
	if recomputed == stale {
		t.Errorf("the pass that read a move of the block's Waiting left %s's open row stale, so the check above examined nothing", other)
	}
	t.Logf("a move of the block's active slots and of the settings changed %v and left %s waiting for %q; a move of its Waiting rewrote it to %q",
		changed, other, left, recomputed)
}

// observe records one observation of the project's daemon, of key's field, as the sampler writes
// one: value as JSON, observed at the moment given.
func (c *consumer) observe(t *testing.T, key, field string, value any, at time.Time) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode the %s observation: %v", field, err)
	}
	c.exec(t, "observe the "+field+" of "+c.fixture.Project+" "+key, `
		insert into legion_observations (project, issue_key, field, value, observed_at)
		values ($1, $2, $3, $4::jsonb, $5)
	`, c.fixture.Project, key, field, string(encoded), at)
}
