package workflow

import (
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// firstConflictRead is when the first CONFLICTING read of these tests is made. Each later read is
// applied through an engine whose clock stands that long after it (mergingEngineAt): a fact a test
// mints carries no read time, so the engine's clock times both reads (mergeability).
var firstConflictRead = time.Date(2026, 11, 19, 1, 0, 0, 0, time.UTC)

// readsAfter is an engine whose clock stands d after the first read.
func readsAfter(d time.Duration) *Engine { return mergingEngineAt(firstConflictRead.Add(d)) }

// seedConflictIssue seeds LEGION-208 standing in at with Dispatch status status, its open pull
// request #42 at head onto main as pr records it otherwise, the implementer's phases row, and the
// phases row of the phase's own worker where that is another role, as the issue stands when
// GitHub first answers its head CONFLICTING.
func seedConflictIssue(t *testing.T, at phase.Phase, status string, pr record.PullRequest) *pgxpool.Pool {
	t.Helper()
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: at, Generation: 1, Status: status, Rank: "U"})
	pr.State, pr.Issue, pr.Repo, pr.Number, pr.Branch, pr.Base, pr.HeadSHA =
		record.PullRequestOpen, "LEGION-208", "sjawhar/legion", 42, "legion/LEGION-208", "main", "head"
	if pr.Failing == nil {
		pr.Failing = []string{}
	}
	seedPR(t, pool, pr)
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim", Entry: 1})
	if worker := RoleFor(at); worker != "" && worker != claim.RoleImplementer && worker != claim.RoleArchitect {
		seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: worker, Claim: claim.Token(string(worker) + "-claim"), Entry: 1})
	}
	return pool
}

// settledGreen is a pull request whose CI settled at head with a run of each name given, every one
// of them succeeded, as the record keeps a settlement (classify.ApplySettlement).
func settledGreen(names ...string) record.PullRequest {
	runs := make([]record.AttemptRun, len(names))
	for i, name := range names {
		runs[i] = record.AttemptRun{Name: name, ID: int64(i + 1), Head: "head", Conclusion: record.RunSucceeded}
	}
	return record.PullRequest{CheckedHead: "head", CheckRuns: runs, Failing: []string{}, Cancelled: []string{}}
}

// conflictingRead is the daemon's read of #42 finding its head CONFLICTING with main, carrying the
// base's rules as the daemon read them.
func conflictingRead(rules *record.BaseRules) intake.PullRequestMergeability {
	return intake.PullRequestMergeability{Repo: "sjawhar/legion", Number: 42, Base: "main", HeadSHA: "head",
		Mergeable: record.MergeabilityConflicting, MergeState: record.MergeStateDirty, Rules: rules}
}

// confirmsConflict is a first CONFLICTING read and the second read that confirms it, one
// conflictConfirmWait later.
func confirmsConflict(t *testing.T, pool *pgxpool.Pool, rules *record.BaseRules) {
	t.Helper()
	applyRefusingNothing(t, pool, readsAfter(0), conflictingRead(rules))
	applyRefusingNothing(t, pool, readsAfter(conflictConfirmWait), conflictingRead(rules))
}

// mergeForwardMessages is every merge-forward message the outbox holds, in the order written, each
// decoded as the outbox decodes it (queuedRows).
func mergeForwardMessages(t *testing.T, pool *pgxpool.Pool) []record.MergeForwardMessage {
	t.Helper()
	messages := []record.MergeForwardMessage{}
	for _, row := range queuedRows(t, pool) {
		if message, ok := row.payload.(record.MergeForwardMessage); ok {
			messages = append(messages, message)
		}
	}
	return messages
}

// conflictConfirms is the due time of every second read of a conflict the outbox holds, in the
// order written, each of them a read of #42 at head.
func conflictConfirms(t *testing.T, pool *pgxpool.Pool) []time.Time {
	t.Helper()
	for _, row := range queuedRows(t, pool) {
		if read, ok := row.payload.(record.ConflictConfirm); ok && read != (record.ConflictConfirm{Number: 42, Head: "head"}) {
			t.Fatalf("a second read reads %+v, want #42 at head", read)
		}
	}
	rows, err := pool.Query(t.Context(), "select next_at from outbox where kind = 'conflict_confirm' order by id")
	if err != nil {
		t.Fatalf("read the second reads: %v", err)
	}
	defer rows.Close()
	due := []time.Time{}
	for rows.Next() {
		var at time.Time
		if err := rows.Scan(&at); err != nil {
			t.Fatalf("scan a second read: %v", err)
		}
		due = append(due, at)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the second reads: %v", err)
	}
	return due
}

// issueStatus is LEGION-208's Dispatch status as the record holds it.
func issueStatus(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var status string
	if err := pool.QueryRow(t.Context(), "select status from issues where key = 'LEGION-208'").Scan(&status); err != nil {
		t.Fatalf("read the issue's status: %v", err)
	}
	return status
}

// assertNothingActed is a conflict the daemon acted on in no way: no merge-forward message, no
// notice to the architect, no worker started, no status written, and the issue in the phase it
// stood in.
func assertNothingActed(t *testing.T, pool *pgxpool.Pool, at phase.Phase) {
	t.Helper()
	if messages := mergeForwardMessages(t, pool); len(messages) != 0 {
		t.Fatalf("merge-forward messages %+v, want none", messages)
	}
	if notices := noticeKinds(t, pool, "LEGION-208"); len(notices) != 0 {
		t.Fatalf("notices %v, want none", notices)
	}
	assertOutboxCount(t, pool, "supervise", 0)
	assertOutboxCount(t, pool, "dispatch_status", 0)
	if got := issuePhase(t, pool); got != at {
		t.Fatalf("the issue is in %s, want it still in %s", got, at)
	}
}

// askedOnce is the one merge forward the record and the outbox hold, asked of head onto main in at
// for why: the record's ask, the implementer's ask and, where hold names a role, that role's hold,
// and the architect's one checks-red notice outside implementing. Nothing moved: the issue is in
// at with the status it had, and no worker was started.
func askedOnce(t *testing.T, pool *pgxpool.Pool, at phase.Phase, status, why string, hold claim.Role) {
	t.Helper()
	asked := recordedPR(t, pool, "LEGION-208").MergeForward
	if asked == nil || asked.Head != "head" || asked.Base != "main" || asked.Phase != at || asked.Why != why || asked.Landed != "" || asked.AskedAt.IsZero() {
		t.Fatalf("the record's merge forward = %+v, want head onto main in %s for %s, not landed", asked, at, why)
	}
	want := []record.MergeForwardMessage{{Role: claim.RoleImplementer, Step: record.MergeForwardAsk, Head: "head", Base: "main", Phase: at, PullRequest: 42, Why: why}}
	if hold != "" {
		want = append(want, record.MergeForwardMessage{Role: hold, Step: record.MergeForwardHold, Head: "head", Base: "main", Phase: at, PullRequest: 42, Why: why})
	}
	if got := mergeForwardMessages(t, pool); !reflect.DeepEqual(got, want) {
		t.Fatalf("merge-forward messages %+v, want %+v", got, want)
	}
	notices := noticeKinds(t, pool, "LEGION-208")
	if at == phase.Implementing {
		if len(notices) != 0 {
			t.Fatalf("notices %v, want none: a merge forward in implementing tells the implementer alone", notices)
		}
	} else {
		if len(notices) != 1 || notices[0] != "checks-red" {
			t.Fatalf("notices %v, want the one checks-red", notices)
		}
		if reason, stays := checksRedNotice(t, pool); stays != at || reason == "" {
			t.Fatalf("the checks-red notice names phase %q with reason %q, want %s, the phase the issue stays in, and a reason", stays, reason, at)
		}
	}
	assertOutboxCount(t, pool, "supervise", 0)
	assertOutboxCount(t, pool, "phase_ended", 0)
	assertOutboxCount(t, pool, "dispatch_status", 0)
	assertOutboxCount(t, pool, "dispatch_message", 0)
	if got, gotStatus := issuePhase(t, pool), issueStatus(t, pool); got != at || gotStatus != status {
		t.Fatalf("the issue is in %s with status %q, want it still in %s with %q", got, gotStatus, at, status)
	}
}

// GitHub computes mergeability in a background job and can answer CONFLICTING about a base it has
// not caught up with, so one read acts on nothing (dispatch://INFRA-4374 R2). The first read
// records itself and queues the second read a minute after it; a MERGEABLE read releases it; a
// CONFLICTING read sooner than the minute changes nothing, and the one a minute after the first
// confirms the conflict and acts; the second read's row ending on GitHub's UNKNOWN releases the
// first read, and the next CONFLICTING read is a first read again; and a read against another base
// branch is a first read of its own.
func TestAConflictIsActedOnOnlyAfterASecondRead(t *testing.T) {
	// The rules are unread, so a confirmed conflict is acted on at once (classify.ConflictNeedsMerge):
	// what holds the act back below is the second read alone.
	unread := &record.BaseRules{Unread: "HTTP 502"}
	firstRead := func(t *testing.T, pool *pgxpool.Pool) {
		t.Helper()
		applyRefusingNothing(t, pool, readsAfter(0), conflictingRead(unread))
		if due := conflictConfirms(t, pool); len(due) != 1 || !due[0].Equal(firstConflictRead.Add(time.Minute)) {
			t.Fatalf("second reads due at %v, want exactly one, a minute after the first read at %s", due, firstConflictRead)
		}
		if first := recordedPR(t, pool, "LEGION-208").ConflictRead; first == nil ||
			first.Head != "head" || first.Base != "main" || !first.At.Equal(firstConflictRead) {
			t.Fatalf("the first read = %+v, want head on main read at %s", first, firstConflictRead)
		}
		if held := recordedPR(t, pool, "LEGION-208").ConflictingHead; held != "" {
			t.Fatalf("the record holds %q conflicting after one read, want none", held)
		}
		assertNothingActed(t, pool, phase.Testing)
	}

	t.Run("a mergeable read a minute later releases the first read", func(t *testing.T) {
		pool := seedConflictIssue(t, phase.Testing, "testing", record.PullRequest{})
		firstRead(t, pool)
		applyRefusingNothing(t, pool, readsAfter(time.Minute), intake.PullRequestMergeability{Repo: "sjawhar/legion", Number: 42,
			Base: "main", HeadSHA: "head", Mergeable: record.MergeabilityMergeable, MergeState: record.MergeStateClean})
		if pr := recordedPR(t, pool, "LEGION-208"); pr.ConflictRead != nil || pr.ConflictingHead != "" {
			t.Fatalf("after the mergeable read the record holds first read %+v and %q conflicting, want neither", pr.ConflictRead, pr.ConflictingHead)
		}
		assertNothingActed(t, pool, phase.Testing)
	})

	t.Run("a conflicting read sooner than a minute waits, and the one a minute after the first acts", func(t *testing.T) {
		pool := seedConflictIssue(t, phase.Testing, "testing", record.PullRequest{})
		firstRead(t, pool)
		applyRefusingNothing(t, pool, readsAfter(30*time.Second), conflictingRead(unread))
		if first := recordedPR(t, pool, "LEGION-208").ConflictRead; first == nil || !first.At.Equal(firstConflictRead) {
			t.Fatalf("after a read 30 seconds on the first read = %+v, want the one read at %s", first, firstConflictRead)
		}
		if due := conflictConfirms(t, pool); len(due) != 1 {
			t.Fatalf("second reads due at %v, want still the one", due)
		}
		assertNothingActed(t, pool, phase.Testing)

		applyRefusingNothing(t, pool, readsAfter(time.Minute), conflictingRead(unread))
		if pr := recordedPR(t, pool, "LEGION-208"); pr.ConflictRead != nil || pr.ConflictingHead != "head" {
			t.Fatalf("after the confirming read the record holds first read %+v and %q conflicting, want the conflict held for head alone", pr.ConflictRead, pr.ConflictingHead)
		}
		askedOnce(t, pool, phase.Testing, "testing", classify.ConflictRulesUnread, claim.RoleTester)
	})

	t.Run("the second read's row ending on unknown releases the first read", func(t *testing.T) {
		pool := seedConflictIssue(t, phase.Testing, "testing", record.PullRequest{})
		firstRead(t, pool)
		applyRefusingNothing(t, pool, readsAfter(3*time.Minute), intake.ConflictConfirmDone{Issue: "LEGION-208", Number: 42, Head: "head"})
		if pr := recordedPR(t, pool, "LEGION-208"); pr.ConflictRead != nil || pr.ConflictingHead != "" {
			t.Fatalf("after the second read ended the record holds first read %+v and %q conflicting, want neither", pr.ConflictRead, pr.ConflictingHead)
		}
		assertNothingActed(t, pool, phase.Testing)

		// The next CONFLICTING read is a first read again, with a second read of its own.
		applyRefusingNothing(t, pool, readsAfter(4*time.Minute), conflictingRead(unread))
		if due := conflictConfirms(t, pool); len(due) != 2 || !due[1].Equal(firstConflictRead.Add(5*time.Minute)) {
			t.Fatalf("second reads due at %v, want a second one a minute after the read that followed the release", due)
		}
		assertNothingActed(t, pool, phase.Testing)
	})

	t.Run("a read against another base branch is a first read of its own", func(t *testing.T) {
		pool := seedConflictIssue(t, phase.Testing, "testing", record.PullRequest{})
		firstRead(t, pool)
		retargeted := conflictingRead(unread)
		retargeted.Base = "release"
		applyRefusingNothing(t, pool, readsAfter(time.Minute), retargeted)
		if first := recordedPR(t, pool, "LEGION-208").ConflictRead; first == nil ||
			first.Head != "head" || first.Base != "release" || !first.At.Equal(firstConflictRead.Add(time.Minute)) {
			t.Fatalf("after a read against release the first read = %+v, want that read's own", first)
		}
		assertNothingActed(t, pool, phase.Testing)
	})
}

// A confirmed conflict that needs a merge keeps the issue in its phase (dispatch://INFRA-4374 R1):
// the implementer is asked to merge the base forward on its own role topic, the worker of the phase
// - the tester, the reviewer or the merger - is told to hold while the merge runs, and the
// architect gets one checks-red notice naming the phase the issue stays in. No worker starts, no
// status is written and no phase ends. In testing and reviewing the need is a check the base
// requires that is not green; past the review it is the merge that comes next. The same read again
// asks nothing a second time.
func TestAConflictKeepsPhaseAndAsksTheImplementerToMergeForward(t *testing.T) {
	requiresCI := &record.BaseRules{Required: []string{"ci"}}
	for _, tc := range []struct {
		at     phase.Phase
		status string
		why    string
		hold   claim.Role
	}{
		{phase.Testing, "testing", classify.ConflictCheckNotGreen, claim.RoleTester},
		{phase.Reviewing, "needs_review", classify.ConflictCheckNotGreen, claim.RoleReviewer},
		{phase.Accepting, "needs_review", classify.ConflictMergeNext, ""},
		{phase.Retro, "retro", classify.ConflictMergeNext, ""},
		{phase.Merging, "retro", classify.ConflictMergeNext, claim.RoleMerger},
	} {
		t.Run(string(tc.at), func(t *testing.T) {
			pool := seedConflictIssue(t, tc.at, tc.status, settledGreen("lint"))
			confirmsConflict(t, pool, requiresCI)
			askedOnce(t, pool, tc.at, tc.status, tc.why, tc.hold)

			applyRefusingNothing(t, pool, readsAfter(2*time.Minute), conflictingRead(requiresCI))
			askedOnce(t, pool, tc.at, tc.status, tc.why, tc.hold)
		})
	}
}

// A confirmed conflict is acted on only when the issue's next step needs a head that merges
// (dispatch://INFRA-4374 R0). Where every check the base requires is green on the head and no rule
// of the base requires the head to be current, the conflict is recorded and nothing is sent: the
// tester and the reviewer go on at that head. Any required check not green, a strict ruleset, and
// rules the daemon could not read each act at once. GitHub's refusal of rulesets to a plan that has
// none is no failed read: it reads as no rules, and so records only. A classic protection rule is
// read as not strict, so its contexts decide; and a required context is green by a success status
// the record keeps for the head as well as by a check run.
func TestAConflictIsActedOnOnlyWhenNeeded(t *testing.T) {
	greenStatus := settledGreen("lint")
	greenStatus.Statuses = []record.CommitStatus{{Context: "ci", State: record.CommitStatusSuccess,
		CreatedAt: firstConflictRead.Add(-time.Hour), Head: "head"}}
	for _, tc := range []struct {
		name   string
		at     phase.Phase
		status string
		pr     record.PullRequest
		rules  *record.BaseRules
		why    string
	}{
		{"every required check green in testing", phase.Testing, "testing", settledGreen("ci", "pr-title"),
			&record.BaseRules{Required: []string{"ci", "pr-title"}}, ""},
		{"every required check green in reviewing with no approval", phase.Reviewing, "needs_review", settledGreen("ci", "pr-title"),
			&record.BaseRules{Required: []string{"ci", "pr-title"}}, ""},
		{"no required check green", phase.Testing, "testing", settledGreen("lint"),
			&record.BaseRules{Required: []string{"ci", "pr-title"}}, classify.ConflictCheckNotGreen},
		{"one of two required checks green", phase.Testing, "testing", settledGreen("ci"),
			&record.BaseRules{Required: []string{"ci", "pr-title"}}, classify.ConflictCheckNotGreen},
		{"a strict ruleset with every check green in testing", phase.Testing, "testing", settledGreen("ci"),
			&record.BaseRules{Required: []string{"ci"}, Strict: true}, classify.ConflictRequiresCurrent},
		{"a strict ruleset with every check green in reviewing with no approval", phase.Reviewing, "needs_review", settledGreen("ci"),
			&record.BaseRules{Required: []string{"ci"}, Strict: true}, classify.ConflictRequiresCurrent},
		{"rules unread", phase.Testing, "testing", settledGreen("ci"),
			&record.BaseRules{Unread: "HTTP 502"}, classify.ConflictRulesUnread},
		{"GitHub's plan refusal of rulesets", phase.Testing, "testing", settledGreen(),
			&record.BaseRules{}, ""},
		{"a classic rule's contexts all green", phase.Reviewing, "needs_review", settledGreen("ci", "lint"),
			&record.BaseRules{Required: []string{"ci", "lint"}}, ""},
		{"a required context green by a success status of the head", phase.Testing, "testing", greenStatus,
			&record.BaseRules{Required: []string{"ci"}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := seedConflictIssue(t, tc.at, tc.status, tc.pr)
			confirmsConflict(t, pool, tc.rules)
			if held := recordedPR(t, pool, "LEGION-208").ConflictingHead; held != "head" {
				t.Fatalf("the record holds %q conflicting, want the confirmed conflict at head recorded", held)
			}
			if tc.why == "" {
				if asked := recordedPR(t, pool, "LEGION-208").MergeForward; asked != nil {
					t.Fatalf("the record's merge forward = %+v, want none asked", asked)
				}
				assertNothingActed(t, pool, tc.at)
				return
			}
			askedOnce(t, pool, tc.at, tc.status, tc.why, RoleFor(tc.at))
		})
	}
}

// In implementing the work a conflict needs is where the issue already stands, so a confirmed
// conflict that needs a merge sends the implementer the one ask and nothing else: no hold, no
// notice to the architect, no worker started, and the implementer's own phase entry as it was, so
// its assignment and its completion stand (dispatch://INFRA-4374 R0). A second confirmed read of
// the same head asks nothing more. A strict ruleset needs the merge though every check is green;
// with every required check green and no strict rule nothing is sent.
func TestAConflictInImplementingIsToldOnlyWhenNeeded(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pr    record.PullRequest
		rules *record.BaseRules
		why   string
	}{
		{"a required check not green", settledGreen("lint"), &record.BaseRules{Required: []string{"ci"}}, classify.ConflictCheckNotGreen},
		{"a strict ruleset with every check green", settledGreen("ci"), &record.BaseRules{Required: []string{"ci"}, Strict: true}, classify.ConflictRequiresCurrent},
		{"every required check green and no strict rule", settledGreen("ci"), &record.BaseRules{Required: []string{"ci"}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := seedConflictIssue(t, phase.Implementing, "in_progress", tc.pr)
			entry := entryRow(t, pool, claim.RoleImplementer)
			confirmsConflict(t, pool, tc.rules)
			if tc.why == "" {
				assertNothingActed(t, pool, phase.Implementing)
			} else {
				askedOnce(t, pool, phase.Implementing, "in_progress", tc.why, "")
			}
			if after := entryRow(t, pool, claim.RoleImplementer); !reflect.DeepEqual(after, entry) {
				t.Fatalf("the implementer's phases row went from %+v to %+v, want it as it was: its assignment stands", entry, after)
			}

			applyRefusingNothing(t, pool, readsAfter(2*time.Minute), conflictingRead(tc.rules))
			if tc.why == "" {
				assertNothingActed(t, pool, phase.Implementing)
				return
			}
			askedOnce(t, pool, phase.Implementing, "in_progress", tc.why, "")
		})
	}
}
