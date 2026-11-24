package workflow

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/intake"
)

const (
	// standsForNoHeadLine is checks' line for a settlement of a head the record does not hold.
	standsForNoHeadLine = "workflow: a checks settlement stands for no head the record holds"
	// headNotAppliedLine is checks' line for a settlement of the head the fence does not take.
	headNotAppliedLine = "workflow: a checks settlement of the head was not applied"
)

// droppedSettlementLines is every line of logged that names a checks settlement as dropped, in
// the order they were written.
func droppedSettlementLines(logged string) []string {
	var lines []string
	for line := range strings.SplitSeq(logged, "\n") {
		if strings.Contains(line, `msg="workflow: a checks settlement`) {
			lines = append(lines, line)
		}
	}
	return lines
}

// roleMessageCount is how many role messages the outbox holds.
func roleMessageCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(t.Context(), `select count(*) from outbox where kind = 'role_message'`).Scan(&count); err != nil {
		t.Fatalf("count the role messages: %v", err)
	}
	return count
}

// laterHandlerFails is a handler that runs after the engine in a fact's transaction and fails it,
// so the transaction rolls back after the engine has done its part.
type laterHandlerFails struct{}

func (laterHandlerFails) Apply(context.Context, pgx.Tx, intake.Fact) (intake.Result, error) {
	return intake.Result{}, errors.New("a later handler fails")
}

// Each checks settlement the daemon drops before it applies it is named by one INFO line, which
// begins with the settlement's repository, number, head, generation and snapshot, by which it is
// matched to the listener's line that published it (dispatch://INFRA-4135 R6), on the wake rig
// (LEGION-208 implementing, its pull request #42 recorded at head1, its implementer released):
//
//   - a settlement of pull request #99, which no record holds, says no pull request is recorded
//     for it;
//   - a settlement of a head a push replaced before its last check run finished, as a deployment
//     repository's 7b52b7c1 was by fe771e3a's push, says it stands for no head the record holds and
//     names the recorded head, the checked head, that no head run is open and the issue, and it
//     wakes nobody and tells no role;
//   - the same with the record at fe771e3a reached by a synchronize whose push has not arrived,
//     so a head run is open, names the run open;
//   - so does a settlement of a head whose synchronize has not arrived yet, with a run open;
//   - a settlement of the head older than the one applied says the head's settlement was not
//     applied, classified stale, with the generation and snapshot the record holds;
//   - one of the head with the fence's check runs and generation and another snapshot says the
//     same, classified conflict;
//   - a redelivery of a settlement already applied is no drop and writes no line;
//   - the replaced head's settlement delivered ten times writes one line.
//
// A drop whose transaction rolls back writes nothing, and its retry that commits writes the line.
// No drop wakes the released implementer or tells a role: none queues a start row or records a
// resume past what the facts before it left (wakeOnSettlement, dispatch://INFRA-4135 R4).
//
// Negative control: at ac57d598 checks returns with no line on a missing record and on every
// settlement it does not apply, so no row finds its line, and the wake reads no head, so the
// replaced head's settlement wakes the implementer.
func TestADroppedSettlementLogsWhy(t *testing.T) {
	const dev = "legion-implementer[bot]"
	// replacedHead is the head the pull request held until a push replaced it with nextHead, and
	// earlyHead a head whose synchronize has not arrived.
	const replacedHead, nextHead, earlyHead = "7b52b7c1", "fe771e3a", "3c9e0d7f"
	reachedReplaced := []intake.Fact{sentPush("head1", replacedHead, dev), namedSync("head1", replacedHead, dev, wakeAt.Add(-2*time.Hour))}
	// The push that replaced it arrived: the record is at nextHead with no head run open.
	replacedNoRun := slices.Concat(reachedReplaced, []intake.Fact{sentPush(replacedHead, nextHead, dev), namedSync(replacedHead, nextHead, dev, wakeAt.Add(-time.Hour))})
	// Its synchronize arrived before its push: the record is at nextHead with a head run open.
	replacedRunOpen := slices.Concat(reachedReplaced, []intake.Fact{namedSync(replacedHead, nextHead, dev, wakeAt.Add(-time.Hour))})
	ofAnother := settledAt("head1", 1).(intake.PullRequestChecks)
	ofAnother.Number = 99
	conflicting := settledAt("head1", 2).(intake.PullRequestChecks)
	conflicting.Snapshot = "head1-2-rebuilt"
	for _, tc := range []struct {
		name    string
		before  []intake.Fact
		at      string
		runOpen bool
		deliver intake.Fact
		times   int
		line    string
		names   []string
	}{
		{name: "a settlement of a pull request no record holds", at: "head1", deliver: ofAnother, line: intake.NoRecordedPullRequestLine,
			names: []string{" repo=sjawhar/legion", " number=99", " head=head1", " generation=1", " snapshot=head1-1"}},
		{name: "a settlement of a replaced head with no head run open", before: replacedNoRun, at: nextHead, deliver: settledAt(replacedHead, 1),
			line: standsForNoHeadLine, names: []string{" repo=sjawhar/legion", " number=42", " head=" + replacedHead, " generation=1", " snapshot=" + replacedHead + "-1",
				" recorded_head=" + nextHead, ` checked_head=""`, " head_run_open=false", " issue=LEGION-208"}},
		{name: "a settlement of a replaced head with a head run open", before: replacedRunOpen, at: nextHead, runOpen: true, deliver: settledAt(replacedHead, 1),
			line: standsForNoHeadLine, names: []string{" number=42", " head=" + replacedHead, " generation=1", " snapshot=" + replacedHead + "-1",
				" recorded_head=" + nextHead, " head_run_open=true", " issue=LEGION-208"}},
		{name: "a settlement of a head whose synchronize has not arrived, with a head run open", before: replacedRunOpen, at: nextHead, runOpen: true,
			deliver: settledAt(earlyHead, 1), line: standsForNoHeadLine, names: []string{" number=42", " head=" + earlyHead, " generation=1",
				" snapshot=" + earlyHead + "-1", " recorded_head=" + nextHead, " head_run_open=true", " issue=LEGION-208"}},
		{name: "a stale settlement of the head", before: []intake.Fact{settledAt("head1", 2)}, at: "head1", deliver: settledAt("head1", 1),
			line: headNotAppliedLine, names: []string{" number=42", " head=head1", " generation=1", " snapshot=head1-1", " classification=stale",
				" recorded_generation=2", " recorded_snapshot=head1-2", " issue=LEGION-208"}},
		{name: "a settlement of the head that conflicts with the fence", before: []intake.Fact{settledAt("head1", 2)}, at: "head1", deliver: conflicting,
			line: headNotAppliedLine, names: []string{" number=42", " head=head1", " generation=2", " snapshot=head1-2-rebuilt", " classification=conflict",
				" recorded_generation=2", " recorded_snapshot=head1-2", " issue=LEGION-208"}},
		{name: "a redelivery of a settlement applied", before: []intake.Fact{settledAt("head1", 2)}, at: "head1", deliver: settledAt("head1", 2)},
		{name: "a replaced head's settlement delivered ten times", before: replacedNoRun, at: nextHead, deliver: settledAt(replacedHead, 1), times: 10,
			line: standsForNoHeadLine, names: []string{" head=" + replacedHead, " generation=1", " snapshot=" + replacedHead + "-1", " recorded_head=" + nextHead}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newWakeRig(t)
			for i, fact := range tc.before {
				rig.apply(fmt.Sprintf("before-%d", i), fact)
			}
			if pr := probePullRequest(t, rig.pool); pr.HeadSHA != tc.at || (pr.Run != nil) != tc.runOpen {
				t.Fatalf("before the settlement the pull request is at %s with a head run open %t, want %s and %t", pr.HeadSHA, pr.Run != nil, tc.at, tc.runOpen)
			}
			if lines := droppedSettlementLines(rig.logged.String()); len(lines) != 0 {
				t.Fatalf("the facts before the settlement logged %q, want no drop", lines)
			}
			told := roleMessageCount(t, rig.pool)
			starts, resumes := len(wakeStarts(t, rig.pool)), len(wakeResumes(t, rig.pool))
			for i := range max(tc.times, 1) {
				rig.apply(fmt.Sprintf("settlement-%d", i), tc.deliver)
			}
			// No dropped settlement tells a role or wakes the released implementer: a settlement
			// wakes only where checks applies it for the record's head (wakeOnSettlement). Where
			// the facts before it woke the implementer, its resume is still open, so those rows
			// would wake nobody either way; the rows of a head the record does not hold are the
			// ones this rule alone holds.
			if got := roleMessageCount(t, rig.pool); got != told {
				t.Fatalf("the dropped settlement queued %d role messages, want none", got-told)
			}
			if got := wakeStarts(t, rig.pool); len(got) != starts {
				t.Fatalf("after the dropped settlement the supervise start rows = %+v, want the %d before it and no other", got, starts)
			}
			if got := wakeResumes(t, rig.pool); len(got) != resumes {
				t.Fatalf("after the dropped settlement the pod resumes = %v, want the %d before it and no other", got, resumes)
			}
			lines := droppedSettlementLines(rig.logged.String())
			if tc.line == "" {
				if len(lines) != 0 {
					t.Fatalf("logged %q, want no drop line", lines)
				}
				return
			}
			if len(lines) != 1 {
				t.Fatalf("logged %d drop lines %q, want one", len(lines), lines)
			}
			if !strings.Contains(lines[0], "level=INFO") || !strings.Contains(lines[0], `msg="`+tc.line+`"`) {
				t.Fatalf("the drop line is %q, want %q at INFO", lines[0], tc.line)
			}
			for _, name := range tc.names {
				if !strings.Contains(lines[0], name) {
					t.Fatalf("the drop line %q does not name %q", lines[0], name)
				}
			}
		})
	}

	t.Run("a drop whose transaction rolls back", func(t *testing.T) {
		rig := newWakeRig(t)
		if _, err := intake.ApplyFact(context.Background(), rig.pool, "api", "rolled-back", ofAnother, rig.engine, laterHandlerFails{}); err == nil {
			t.Fatal("the fact applied, want the later handler's failure to roll it back")
		}
		if lines := droppedSettlementLines(rig.logged.String()); len(lines) != 0 {
			t.Fatalf("the rolled-back fact logged %q, want nothing", lines)
		}
		rig.apply("retried", ofAnother)
		if lines := droppedSettlementLines(rig.logged.String()); len(lines) != 1 || !strings.Contains(lines[0], intake.NoRecordedPullRequestLine) {
			t.Fatalf("the retry logged %q, want the one line the rolled-back fact did not write", lines)
		}
	})
}
