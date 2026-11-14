package workflow

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// clearedIssue is the issue these cases record and clear the generation of, in implementing.
func clearedIssue(key string) record.Issue {
	return record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: key,
		Project: "LEGION", Title: "Clear", Phase: phase.Implementing, Generation: 3, Status: "in_progress", Rank: "U"}
}

// clearIssueGeneration clears issue's generation in a transaction of its own (Engine.ClearGeneration).
func clearIssueGeneration(t *testing.T, pool *pgxpool.Pool, issue record.Issue) {
	t.Helper()
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return testEngine(config.DesignGateRootIssues, nil).ClearGeneration(t.Context(), tx, issue, time.Now().UTC())
	})
}

// issuePhases is every phases row issue holds.
func issuePhases(t *testing.T, pool *pgxpool.Pool, issue string) []record.PhaseRow {
	t.Helper()
	var rows []record.PhaseRow
	seedRecord(t, pool, func(tx pgx.Tx) error {
		var err error
		rows, err = record.NewStore().Phases(t.Context(), tx, issue)
		return err
	})
	return rows
}

// A new generation keeps an open pull request but none of the old generation's reading of its CI:
// the settlement it recorded is of a head nobody has judged since, and its head run would replay
// the old generation's judgement.
func TestClearGenerationResetsAnOpenPullRequestsCheckSettlement(t *testing.T) {
	pool := migratedPool(t)
	issue := clearedIssue("LEGION-285")
	seedIssue(t, pool, issue)
	seedPR(t, pool, record.PullRequest{
		Issue: issue.Key, Repo: "acme/widgets", Number: 1359, Branch: "legion/LEGION-285",
		HeadSHA: "red-tests", HeadUpdatedAt: time.Date(2026, 9, 25, 21, 0, 0, 0, time.UTC),
		CheckedHead: "red-tests", Failing: []string{"test"}, CheckRuns: []record.AttemptRun{},
		Run:   &record.HeadRun{Anchor: record.Judgement{HeadSHA: "fix"}, Steps: []record.RunStep{{Head: "red-tests"}}},
		State: record.PullRequestOpen,
	})
	clearIssueGeneration(t, pool, issue)
	var got *record.PullRequest
	seedRecord(t, pool, func(tx pgx.Tx) error {
		var err error
		got, err = record.NewStore().PullRequest(t.Context(), tx, issue.Key)
		return err
	})
	if got == nil {
		t.Fatal("the open pull request is gone, want it kept")
	}
	if got.CheckedHead != "" || len(got.Failing) != 0 || got.Run != nil {
		t.Fatalf("after ClearGeneration: checkedHead=%q failing=%v run=%+v, want \"\", none and nil",
			got.CheckedHead, got.Failing, got.Run)
	}
}

// A new generation's phase row keeps its pass and loses its rounds. The rounds are the last
// generation's reviews, which review_round_cap bounds within one run; the pass is what tells one
// pass through a phase from the last in the handoff completion key, so the new run's completion at
// a commit the old run already reported has to take a key of its own.
func TestClearGenerationKeepsThePassAndResetsTheRounds(t *testing.T) {
	pool := migratedPool(t)
	issue := clearedIssue("LEGION-286")
	seedIssue(t, pool, issue)
	seedPhase(t, pool, record.PhaseRow{Issue: issue.Key, Role: claim.RoleImplementer, Claim: "legion-286-implementer",
		HandoffCommit: "handoff-4", Pass: 4, Rounds: 2, Verdict: "pass"})
	clearIssueGeneration(t, pool, issue)
	phases := issuePhases(t, pool, issue.Key)
	if len(phases) != 1 || phases[0].Pass != 4 || phases[0].Rounds != 0 || phases[0].HandoffCommit != "" {
		t.Fatalf("phases after ClearGeneration = %#v, want one row with pass 4, no rounds and no handoff", phases)
	}
}

// A new generation clears each role's handoff of its current phase — the commit it reported and the
// Dispatch handoff record it named — so no role of the new generation reads as having completed,
// and keeps the record each role last named, which a completion naming it again is refused against.
// It clears the implementer's counts of the moves back as well, and the cap value the row told
// for, so the new generation counts and tells from its own first crossing.
func TestClearGenerationClearsTheHandoffRecordAndKeepsTheLastNamed(t *testing.T) {
	pool := migratedPool(t)
	issue := clearedIssue("LEGION-286")
	seedIssue(t, pool, issue)
	seedPhase(t, pool, record.PhaseRow{Issue: issue.Key, Role: claim.RoleTester, Claim: "tester", HandoffRecord: "test-record", HandoffVersion: 2, LastHandoff: "test-record", Verdict: "pass"})
	seedPhase(t, pool, record.PhaseRow{Issue: issue.Key, Role: claim.RoleImplementer, Claim: "implementer", HandoffCommit: "head", HandoffRecord: "implement-record", HandoffVersion: 1, LastHandoff: "implement-record", Rounds: 1,
		BackwardMoves: map[string]int{"testing-implementing": 1}, CapTold: 3})
	clearIssueGeneration(t, pool, issue)
	rows := issuePhases(t, pool, issue.Key)
	want := map[claim.Role]string{claim.RoleImplementer: "implement-record", claim.RoleTester: "test-record"}
	if len(rows) != len(want) {
		t.Fatalf("phases after ClearGeneration = %#v, want both rows kept", rows)
	}
	for _, row := range rows {
		if row.Completed() || row.HandoffVersion != 0 || row.LastHandoff != want[row.Role] {
			t.Fatalf("the %s row after ClearGeneration = %#v, want no commit, record or version, and %q kept as the record it last named", row.Role, row, want[row.Role])
		}
		if len(row.BackwardMoves) != 0 || row.CapTold != 0 {
			t.Fatalf("the %s row after ClearGeneration counts %#v moves back and told cap %d, want no count and no cap told", row.Role, row.BackwardMoves, row.CapTold)
		}
	}
}

// A re-admission clears the work a role's phase entry named and leaves the number where it
// stands. The task goes because it is the old generation's, cleared with that generation's
// handoffs (ClearGeneration): the tree runs again, and what the role is owed is whatever its
// promotion hands it, not the task of a run that ended. The number stays because it counts the
// entries the role has been handed on this issue and nothing of a re-admission unhands one: the
// promotion raises it again (StartPromotedWorker), so the entry the new work is sent under is
// above every entry a completion still in flight could name.
func TestClearGenerationClearsTheEntryTaskAndLeavesTheEntry(t *testing.T) {
	pool := migratedPool(t)
	issue := clearedIssue("LEGION-287")
	raised := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	seedIssue(t, pool, issue)
	seedPhase(t, pool, record.PhaseRow{Issue: issue.Key, Role: claim.RoleImplementer, Claim: "implementer",
		Entry: 3, EntryTask: "Continue the change. Issue: LEGION-287. Phase: implementing.", EntryRaisedAt: raised})
	clearIssueGeneration(t, pool, issue)
	rows := issuePhases(t, pool, issue.Key)
	if len(rows) != 1 {
		t.Fatalf("phases after ClearGeneration = %#v, want the one row kept", rows)
	}
	row := rows[0]
	if row.EntryTask != "" {
		t.Fatalf("the implementer's row still names the work %q of the generation that ended, want none: its promotion writes the task it is given",
			row.EntryTask)
	}
	if row.Entry != 3 || !row.EntryRaisedAt.Equal(raised) {
		t.Fatalf("the implementer's row after ClearGeneration = entry %d raised at %s, want the entry 3 raised at %s it stood on",
			row.Entry, row.EntryRaisedAt, raised)
	}
}
