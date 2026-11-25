package workflow

import (
	"context"
	"reflect"
	"slices"
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
// the settlement it recorded is of a head nobody has judged since, with the commit statuses it
// named, and its head run would replay the old generation's judgement. The old run's ask to merge
// the base forward goes with it, and a first conflicting read still waiting for its second stays:
// that is GitHub's answer about the head and the base, which the new generation does not change.
func TestClearGenerationResetsAnOpenPullRequestsCheckSettlement(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	issue := clearedIssue("LEGION-285")
	seedIssue(t, pool, issue)
	pr := record.PullRequest{
		Issue: issue.Key, Repo: "acme/widgets", Number: 1359, Branch: "legion/LEGION-285",
		HeadSHA: "red-tests", HeadUpdatedAt: time.Date(2026, 9, 25, 21, 0, 0, 0, time.UTC),
		CheckedHead: "red-tests", Failing: []string{"test"}, CheckRuns: []record.AttemptRun{},
		Statuses:     []record.CommitStatus{{Context: "ci", State: record.CommitStatusFailure, CreatedAt: time.Date(2026, 9, 25, 20, 59, 0, 0, time.UTC), Head: "red-tests"}},
		Run:          &record.HeadRun{Anchor: record.Judgement{HeadSHA: "fix"}, Steps: []record.RunStep{{Head: "red-tests"}}},
		State:        record.PullRequestOpen,
		ConflictRead: &record.ConflictRead{Head: "red-tests", Base: "main", At: time.Date(2026, 9, 25, 21, 1, 0, 0, time.UTC)},
		MergeForward: &record.MergeForward{Head: "fix", Base: "main", Phase: phase.Reviewing, Why: record.ConflictRequiresCurrent,
			AskedAt: time.Date(2026, 9, 25, 20, 58, 0, 0, time.UTC), Landed: "red-tests"},
	}
	seedPR(t, pool, pr)
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
	if got.CheckedHead != "" || len(got.Failing) != 0 || len(got.Statuses) != 0 || got.Run != nil {
		t.Fatalf("after ClearGeneration: checkedHead=%q failing=%v statuses=%v run=%+v, want \"\", none, none and nil",
			got.CheckedHead, got.Failing, got.Statuses, got.Run)
	}
	if got.MergeForward != nil {
		t.Fatalf("after ClearGeneration: the ask to merge forward = %+v, want none: it was the old run's", *got.MergeForward)
	}
	if !reflect.DeepEqual(got.ConflictRead, pr.ConflictRead) {
		t.Fatalf("after ClearGeneration: the first conflicting read = %+v, want %+v kept", got.ConflictRead, pr.ConflictRead)
	}
}

// A new generation's phase row keeps its pass and loses its rounds. The rounds are the last
// generation's reviews, which review_round_cap bounds within one run; the pass is what tells one
// pass through a phase from the last in the handoff completion key, so the new run's completion at
// a commit the old run already reported has to take a key of its own.
func TestClearGenerationKeepsThePassAndResetsTheRounds(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// A root architect's row that keeps the wait a resume answered (record.PhaseRow.EntryAwaiting,
// which only the resume of an answered root writes) loses it with the task when a re-admission
// clears the generation, since the promotion that follows raises a new entry with work of its own.
// Both clears a re-admission makes are read: of one issue (ClearGeneration) and of a whole tree
// (ClearTreeGeneration). The entry and the moment it was raised stay, as they do for every role.
func TestClearGenerationClearsTheWaitTheArchitectsEntryKept(t *testing.T) {
	t.Parallel()
	raised := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	const resumed = "A person answered your ask ask-1; read it. Continue."
	for _, tc := range []struct {
		name  string
		key   string
		clear func(tx pgx.Tx, issue record.Issue) error
	}{
		{name: "a re-admission of the issue", key: "LEGION-291", clear: func(tx pgx.Tx, issue record.Issue) error {
			return testEngine(config.DesignGateRootIssues, nil).ClearGeneration(t.Context(), tx, issue, time.Now().UTC())
		}},
		{name: "a re-admission of the tree", key: "LEGION-292", clear: func(tx pgx.Tx, issue record.Issue) error {
			return testEngine(config.DesignGateRootIssues, nil).ClearTreeGeneration(t.Context(), tx, issue.Tree, time.Now().UTC())
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			issue := clearedIssue(tc.key)
			seedIssue(t, pool, issue)
			seedPhase(t, pool, record.PhaseRow{Issue: issue.Key, Role: claim.RoleArchitect, Claim: "architect", Entry: 3,
				EntryTask: resumed, EntryAwaiting: &record.Awaiting{Ask: "ask-1", AnsweredBy: record.AnsweredByAsk}, EntryRaisedAt: raised})
			seedRecord(t, pool, func(tx pgx.Tx) error { return tc.clear(tx, issue) })
			got := record.PhaseRowOf(issuePhases(t, pool, issue.Key), claim.RoleArchitect)
			if got.EntryTask != "" || got.EntryAwaiting != nil {
				t.Fatalf("the architect's row after %s = task %q, wait %+v; want no task and no wait: both were the generation's that ended",
					tc.name, got.EntryTask, got.EntryAwaiting)
			}
			if got.Entry != 3 || !got.EntryRaisedAt.Equal(raised) {
				t.Fatalf("the architect's row after %s = entry %d raised at %s, want the entry 3 raised at %s it stood on",
					tc.name, got.Entry, got.EntryRaisedAt, raised)
			}
		})
	}
}

// A clear of a generation forgets exactly the kept decisions a start forgets, because both run the
// one forget (ForgetSuperseded): the engine hands it to the record's clear (forgetSuperseded,
// record.KeptForget), and the start task's read runs it before it names anything
// (ParkedDecisionTask). The same decisions, kept on two issues of one tree, go through each path
// on a record of their own: a changes request on the root that the root's gate's later approval
// superseded, and one on the child made after the child's gate's approval, which is still the
// person's word. The start, the clear of the tree and the clear of each issue in turn all forget
// the first and keep the second, so a clear that asked another rule, or none, would differ.
func TestAClearForgetsTheKeptDecisionsAStartForgets(t *testing.T) {
	t.Parallel()
	approvedAt := time.Date(2026, 9, 22, 9, 0, 0, 0, time.UTC)
	approved := record.EventMark{Seq: 10, At: approvedAt}
	superseded := record.ParkedDecision{Issue: "LEGION-1", Ref: "dispatch://LEGION-1/spec", Decision: record.DecisionDesignChangesRequested,
		Actor: "user:alice", At: approvedAt.Add(-time.Hour), Seq: 8}
	stands := record.ParkedDecision{Issue: "LEGION-2", Ref: "dispatch://LEGION-2/spec", Decision: record.DecisionDesignChangesRequested,
		Actor: "user:bob", At: approvedAt.Add(time.Hour), Seq: 12}
	engine := testEngine(config.DesignGateRootIssues, nil)
	records := record.NewStore()
	// keptAfter runs path over a fresh record of the tree, its two gates approved and its two
	// decisions kept, and answers the references the tree keeps after it.
	keptAfter := func(t *testing.T, path func(ctx context.Context, tx pgx.Tx, tree string) error) []string {
		t.Helper()
		pool := migratedPool(t)
		root := seedAdmittedTree(t, pool, 1)
		version := 2
		seedRecord(t, pool, func(tx pgx.Tx) error {
			for _, d := range []record.ParkedDecision{superseded, stands} {
				if err := records.PutGate(t.Context(), tx, record.DesignGate{Issue: d.Issue, Step: record.DesignStepSpec, ArtifactID: "artifact-" + d.Issue,
					LatestVersion: version, ApprovedVersion: &version, ApprovedMark: approved, EverOpened: true}); err != nil {
					return err
				}
				if err := records.AddParkedDecision(t.Context(), tx, d); err != nil {
					return err
				}
			}
			return nil
		})
		applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error { return path(ctx, tx, root.Tree) })
		var refs []string
		seedRecord(t, pool, func(tx pgx.Tx) error {
			kept, err := records.ParkedDecisions(t.Context(), tx, root.Tree)
			for _, d := range kept {
				refs = append(refs, d.Ref)
			}
			return err
		})
		slices.Sort(refs)
		return refs
	}

	start := keptAfter(t, func(ctx context.Context, tx pgx.Tx, tree string) error {
		root, err := records.Issue(ctx, tx, tree)
		if err != nil {
			return err
		}
		_, _, err = engine.ParkedDecisionTask(ctx, tx, *root)
		return err
	})
	if want := []string{stands.Ref}; !slices.Equal(start, want) {
		t.Fatalf("the tree keeps %v after the start's read, want %v: the root's changes request came before its gate's approval, "+
			"and the child's after its own", start, want)
	}
	for _, tc := range []struct {
		name  string
		clear func(ctx context.Context, tx pgx.Tx, tree string) error
	}{
		{name: "the clear of the tree", clear: func(ctx context.Context, tx pgx.Tx, tree string) error {
			return engine.ClearTreeGeneration(ctx, tx, tree, engine.now())
		}},
		{name: "the clear of each issue", clear: func(ctx context.Context, tx pgx.Tx, tree string) error {
			for _, key := range []string{superseded.Issue, stands.Issue} {
				issue, err := records.Issue(ctx, tx, key)
				if err != nil {
					return err
				}
				if err := engine.ClearGeneration(ctx, tx, *issue, engine.now()); err != nil {
					return err
				}
			}
			return nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := keptAfter(t, tc.clear); !slices.Equal(got, start) {
				t.Fatalf("the tree keeps %v after %s and %v after the start's read of the same decisions, want the same: "+
					"both run the one forget (ForgetSuperseded)", got, tc.name, start)
			}
		})
	}
}
