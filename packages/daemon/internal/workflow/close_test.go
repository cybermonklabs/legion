package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A child leaving Legion's work ends only the child (R20 of dispatch://INFRA-3320): here the root is
// mid-implementation when its child's pull request merges, or a human moves the child out of work.
// The merge queues the child's close, and Dispatch's done on it closes the child into phase done;
// a human's move keeps its phase. The root keeps its
// phase, no deadline is armed, none of its claims is stopped, and the architect above is told which
// child left and how. What the child's claims get is the status's: a child closed as done by a
// human has every claim closed (issue_close); one closed at its merge has five claims closed and
// the role that works its production check kept for the check the merge opens, whose close writes
// the suspension (merge_close_test.go); and one parked in backlog, icebox or triage has every claim
// suspended, and its pod goes with the Sandbox the one IssueSuspend its leave writes deletes. The
// merged child writes none at its close: that role's claim and the pod stay for the production
// check's first look.
func TestAChildLeavingTheWorkflowNeverClosesItsTree(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fact   intake.Fact
		notice record.NoticeKind
		op     string
		// check is the merge's: the tester is left out of the stops and owes the check.
		check bool
	}{
		{name: "merged", fact: intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 43, MergeSHA: "merge-43"}, notice: "child-closed", op: "issue_close", check: true},
		{name: "closed by a human", fact: intake.DispatchIssue{Key: "LEGION-209", ResolvedRepo: "sjawhar/legion", Seq: 2, Type: "issue.closed", Status: "done", Title: "child", Parent: "LEGION-208", Rank: "V"}, notice: "child-closed", op: "issue_close"},
		{name: "moved to backlog", fact: intake.DispatchIssue{Key: "LEGION-209", ResolvedRepo: "sjawhar/legion", Seq: 2, Type: "issue.updated", Status: "backlog", Title: "child", Parent: "LEGION-208", Rank: "V"}, notice: "child-status", op: "suspend"},
		{name: "moved to icebox", fact: intake.DispatchIssue{Key: "LEGION-209", ResolvedRepo: "sjawhar/legion", Seq: 2, Type: "issue.updated", Status: "icebox", Title: "child", Parent: "LEGION-208", Rank: "V"}, notice: "child-status", op: "suspend"},
		{name: "moved to triage", fact: intake.DispatchIssue{Key: "LEGION-209", ResolvedRepo: "sjawhar/legion", Seq: 2, Type: "issue.updated", Status: "triage", Title: "child", Parent: "LEGION-208", Rank: "V"}, notice: "child-status", op: "suspend"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			now := time.Date(2026, 9, 23, 4, 0, 0, 0, time.UTC)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 7, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
			parent := "LEGION-208"
			seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "child", Parent: &parent, Phase: phase.Merging, Generation: 7, Status: "retro", Rank: "V", LastDispatchSeq: 1})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implementer"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-209", Role: claim.RoleMerger, Claim: "child-merger", HandoffCommit: "head-43"})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-209", Repo: "sjawhar/legion", Number: 43, Branch: "legion/LEGION-209", HeadSHA: "head-43", Failing: []string{}})
			engine := New(record.NewStore(), Config{Project: "LEGION", Steps: config.DesignGateRootIssues.Steps(), ProductionCheckRole: claim.RoleTester, Repositories: testRepositories, Clock: func() time.Time { return now }}, nil)

			result, err := intake.ApplyFact(context.Background(), pool, "api", "child-leaves", tc.fact, engine, admissionStub{})
			if err != nil || result.Refusal != nil {
				t.Fatalf("ApplyFact = %#v, %v", result, err)
			}
			if tc.check {
				landClose(t, pool, engine, "LEGION-209")
			}
			var rootPhase phase.Phase
			var lingering bool
			if err := pool.QueryRow(context.Background(), "select phase, linger_until is not null from issues where key = 'LEGION-208'").Scan(&rootPhase, &lingering); err != nil {
				t.Fatalf("read root: %v", err)
			}
			if rootPhase != phase.Implementing || lingering {
				t.Fatalf("root phase=%s lingering=%t, want implementing and no linger", rootPhase, lingering)
			}
			var childPhase phase.Phase
			if err := pool.QueryRow(context.Background(), "select phase from issues where key = 'LEGION-209'").Scan(&childPhase); err != nil {
				t.Fatalf("read child: %v", err)
			}
			wantPhase := phase.Merging
			if tc.check {
				wantPhase = phase.Done
			}
			if childPhase != wantPhase {
				t.Fatalf("child phase = %s, want %s: a stop keeps the phase, the merge's close ends the run", childPhase, wantPhase)
			}
			assertOutboxCount(t, pool, "linger_close", 0)
			stopped := superviseRequests(t, pool, tc.op)
			for _, role := range claim.Roles {
				want := 1
				if tc.check && role == claim.RoleTester {
					want = 0
				}
				if stopped["LEGION-209/"+string(role)] != want || stopped["LEGION-208/"+string(role)] != 0 {
					t.Fatalf("%s rows %v, want one for every one of the child's claims but a merge's tester, and none of the root's", tc.op, stopped)
				}
			}
			for _, other := range []string{"suspend", "issue_close"} {
				if other != tc.op && len(superviseRequests(t, pool, other)) != 0 {
					t.Fatalf("the child's leave as %s queued %s rows %v, want %s rows alone", tc.notice, other, superviseRequests(t, pool, other), tc.op)
				}
			}
			wantStarts := 0
			if tc.check {
				wantStarts = 1
			}
			if started := superviseRequests(t, pool, "start"); len(started) != wantStarts || started["LEGION-209/tester"] != wantStarts {
				t.Fatalf("starts = %v, want the tester's check look alone for a merge and none otherwise", started)
			}
			got := issueSuspensions(t, pool, "LEGION-209")
			if tc.check && len(got) != 0 {
				t.Fatalf("issue suspensions of the merged child = %+v, want none until its check closes", got)
			}
			if !tc.check && (len(got) != 1 || got[0].Tree != "LEGION-208" || got[0].Generation != 7 || got[0].TreeGeneration != 7) {
				t.Fatalf("issue suspensions of the child = %+v, want one of its tree at both generations 7", got)
			}
			if got := issueSuspensions(t, pool, "LEGION-208"); len(got) != 0 {
				t.Fatalf("the child's leave suspended the root's issue: %+v", got)
			}
			if got := noticeKinds(t, pool, "LEGION-209"); !containsNotice(got, tc.notice) {
				t.Fatalf("child notices = %v, want %s for the tree's architect", got, tc.notice)
			}
		})
	}
}

// No issue lingers and none is handed over (R19 and R20 of dispatch://INFRA-3320): a root a person
// moves out of Legion's work, to triage or to done, stops itself alone. Its phase stays where a
// person's move leaves it, no deadline is armed for it, no row the release before queued for a
// linger, a tree close, a workspace removal or a hand-over is queued, and its children run on:
// their records, their claims and their statuses are left as they stood.
func TestNoTreeLingersAndNoIssueIsHandedOver(t *testing.T) {
	for _, status := range []string{"triage", "done"} {
		t.Run(status, func(t *testing.T) {
			pool := migratedPool(t)
			now := time.Date(2026, 9, 23, 4, 0, 0, 0, time.UTC)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 7, Status: "in_progress", Rank: "U", LastDispatchSeq: 1, HoldsGate: true})
			parent := "LEGION-208"
			for _, child := range []struct {
				key    string
				at     phase.Phase
				status string
			}{{"LEGION-209", phase.Testing, "testing"}, {"LEGION-210", phase.Merging, "retro"}} {
				seedIssue(t, pool, record.Issue{Key: child.key, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: child.key, Parent: &parent, Phase: child.at, Generation: 7, Status: child.status, Rank: "V", LastDispatchSeq: 1})
			}
			engine := New(record.NewStore(), Config{Project: "LEGION", Steps: config.DesignGateRootIssues.Steps(), ProductionCheckRole: claim.RoleTester, Repositories: testRepositories, Clock: func() time.Time { return now }}, nil)
			if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "root-out", intake.DispatchIssue{Key: "LEGION-208", ResolvedRepo: "sjawhar/legion", Seq: 2, Type: "issue.updated", Status: status, Title: "root", Rank: "U"}, engine, admissionStub{}); err != nil {
				t.Fatalf("ApplyFact: %v", err)
			}
			var rootPhase phase.Phase
			var lingering bool
			if err := pool.QueryRow(context.Background(), "select phase, linger_until is not null from issues where key = 'LEGION-208'").Scan(&rootPhase, &lingering); err != nil {
				t.Fatalf("read root: %v", err)
			}
			if rootPhase != phase.Implementing || lingering {
				t.Fatalf("root out of work at %s: phase=%s lingering=%t, want it stopped in implementing and no linger", status, rootPhase, lingering)
			}
			for _, kind := range []string{"linger_close", "workspace_remove"} {
				assertOutboxCount(t, pool, kind, 0)
			}
			if got := superviseRequests(t, pool, "tree_close"); len(got) != 0 {
				t.Fatalf("tree_close rows %v, want none", got)
			}
			var handOvers int
			if err := pool.QueryRow(context.Background(), "select count(*) from outbox where kind = 'dispatch_status' and payload->>'kind' = 'hand-over'").Scan(&handOvers); err != nil || handOvers != 0 {
				t.Fatalf("hand-over rows = %d, %v; want none", handOvers, err)
			}
			for _, child := range []string{"LEGION-209", "LEGION-210"} {
				var childPhase phase.Phase
				var childStatus string
				var generation int64
				if err := pool.QueryRow(context.Background(), "select phase, status, generation from issues where key = $1", child).Scan(&childPhase, &childStatus, &generation); err != nil {
					t.Fatalf("read %s: %v", child, err)
				}
				if childPhase == phase.Done || childStatus == "todo" || generation != 7 {
					t.Fatalf("%s after its root left = phase %s, status %s, generation %d; want it running on where it stood", child, childPhase, childStatus, generation)
				}
				if writes := statusWritesOf(t, pool, child); len(writes) != 0 {
					t.Fatalf("status writes of %s = %v, want none: no other issue's status is written", child, writes)
				}
			}
			stopped := superviseRequests(t, pool, "suspend")
			for key := range superviseRequests(t, pool, "issue_close") {
				stopped[key]++
			}
			for key := range stopped {
				if !strings.HasPrefix(key, "LEGION-208/") {
					t.Fatalf("stops %v, want the root's own claims alone", stopped)
				}
			}
		})
	}
}

// An issue Dispatch shows out of Legion's work gets no worker (R20 of dispatch://INFRA-3320): a
// person's backlog, icebox, triage or done on an issue the daemon runs in implementing stops its
// claims — suspended, or at done closed — and queues the deletion of its pod, keeps its phase, its
// generation and its pull request, and writes no status of any other issue. A later fact that
// would start the phase's worker of an issue in work starts none of it.
func TestAnIssueOutOfWorkGetsNoWorkers(t *testing.T) {
	for _, tc := range []struct{ status, op string }{
		{"backlog", "suspend"}, {"icebox", "suspend"}, {"triage", "suspend"}, {"done", "issue_close"},
	} {
		t.Run(tc.status, func(t *testing.T) {
			pool := migratedPool(t)
			now := time.Date(2026, 9, 23, 4, 0, 0, 0, time.UTC)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 7, Status: "in_progress", Rank: "U", LastDispatchSeq: 1, HoldsGate: true})
			parent := "LEGION-208"
			seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "child", Parent: &parent, Phase: phase.Implementing, Generation: 7, Status: "in_progress", Rank: "V", LastDispatchSeq: 1})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-209", Role: claim.RoleImplementer, Claim: "implementer", Entry: 2})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-209", Repo: "sjawhar/legion", Number: 43, Branch: "legion/LEGION-209", HeadSHA: "head-43", Generation: 7, Failing: []string{}})
			engine := New(record.NewStore(), Config{Project: "LEGION", Steps: config.DesignGateRootIssues.Steps(), ProductionCheckRole: claim.RoleTester, Repositories: testRepositories, Clock: func() time.Time { return now }}, nil)
			apply := applyFacts(t, pool, engine)

			apply("child-out", intake.DispatchIssue{Key: "LEGION-209", ResolvedRepo: "sjawhar/legion", Seq: 2, Type: "issue.updated", Status: tc.status, Title: "child", Parent: "LEGION-208", Rank: "V"})

			var childPhase phase.Phase
			var generation int64
			if err := pool.QueryRow(context.Background(), "select phase, generation from issues where key = 'LEGION-209'").Scan(&childPhase, &generation); err != nil {
				t.Fatalf("read the child: %v", err)
			}
			if childPhase != phase.Implementing || generation != 7 {
				t.Fatalf("the stopped child = phase %s, generation %d; want implementing at 7", childPhase, generation)
			}
			stopped := superviseRequests(t, pool, tc.op)
			for _, role := range claim.Roles {
				if stopped["LEGION-209/"+string(role)] != 1 || stopped["LEGION-208/"+string(role)] != 0 {
					t.Fatalf("%s rows %v, want one for every claim of the child and none of the root's", tc.op, stopped)
				}
			}
			if got := issueSuspensions(t, pool, "LEGION-209"); len(got) != 1 || got[0].Generation != 7 {
				t.Fatalf("issue suspensions of the child = %+v, want one of its generation 7", got)
			}
			var prState string
			if err := pool.QueryRow(context.Background(), "select state from pull_requests where issue = 'LEGION-209'").Scan(&prState); err != nil || prState != string(record.PullRequestOpen) {
				t.Fatalf("the child's pull request = %q, %v; want it kept open", prState, err)
			}
			if writes := statusWritesOf(t, pool, "LEGION-208"); len(writes) != 0 {
				t.Fatalf("status writes of the parent = %v, want none", writes)
			}
			startsBefore := len(superviseRequests(t, pool, "start"))
			apply("worker-done", intake.HandoffComplete{Issue: "LEGION-209", Role: claim.RoleImplementer, Generation: 7, Commit: "head-44", Summary: "done"})
			if starts := superviseRequests(t, pool, "start"); len(starts) != startsBefore {
				t.Fatalf("starts after a completion of the stopped child = %v, want none new", starts)
			}
		})
	}
}

// statusWritesOf is every Dispatch status row queued for issue, by its status.
func statusWritesOf(t *testing.T, pool *pgxpool.Pool, issue string) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), "select payload->>'status' from outbox where kind = 'dispatch_status' and issue = $1 order by id", issue)
	if err != nil {
		t.Fatalf("read the status writes of %s: %v", issue, err)
	}
	writes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the status writes of %s: %v", issue, err)
	}
	return writes
}

func noticeKinds(t *testing.T, pool *pgxpool.Pool, issue string) []record.NoticeKind {
	t.Helper()
	rows, err := pool.Query(t.Context(), "select payload->>'kind' from outbox where kind = 'notice' and issue = $1 order by id", issue)
	if err != nil {
		t.Fatalf("read notices: %v", err)
	}
	defer rows.Close()
	var kinds []record.NoticeKind
	for rows.Next() {
		var kind record.NoticeKind
		if err := rows.Scan(&kind); err != nil {
			t.Fatalf("scan notice: %v", err)
		}
		kinds = append(kinds, kind)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate notices: %v", err)
	}
	return kinds
}

func containsNotice(kinds []record.NoticeKind, want record.NoticeKind) bool {
	for _, kind := range kinds {
		if kind == want {
			return true
		}
	}
	return false
}

// issueSuspensions decodes the queued issue_suspend rows of issue, oldest first.
func issueSuspensions(t *testing.T, pool *pgxpool.Pool, issue string) []record.IssueSuspend {
	t.Helper()
	rows, err := pool.Query(t.Context(), "select payload from outbox where kind = 'issue_suspend' and issue = $1 order by id", issue)
	if err != nil {
		t.Fatalf("read issue suspensions: %v", err)
	}
	defer rows.Close()
	var suspensions []record.IssueSuspend
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			t.Fatalf("scan issue suspension: %v", err)
		}
		var suspension record.IssueSuspend
		if err := json.Unmarshal(payload, &suspension); err != nil {
			t.Fatalf("decode issue suspension %s: %v", payload, err)
		}
		suspensions = append(suspensions, suspension)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate issue suspensions: %v", err)
	}
	return suspensions
}

// An issue close is fenced as a suspend is: it acts unless a newer start has run against the claim
// — a person set the done child todo again, whose run it must not end — and a row without an id is
// never dropped. Nor does a stop act that is of an earlier run or phase entry than the fence its
// issue stands at (record.Fence): a return raises the entry, so a stop queued before it stops
// nothing of the run the return opened. A caller that holds no fence weighs the row ids alone, and
// a stop that names no fence (a row of the release before) is fenced by them alone.
func TestStopActsFencesAnIssueCloseLikeASuspend(t *testing.T) {
	at := &record.Fence{Generation: 7, Entry: 4}
	for _, tc := range []struct {
		name      string
		id        int64
		stop      record.SuperviseRequest
		lastStart int64
		at        *record.Fence
		want      bool
	}{
		{name: "an issue close no start followed", id: 10, stop: record.SuperviseRequest{Op: "issue_close"}, lastStart: 4, want: true},
		{name: "an issue close a newer start superseded", id: 10, stop: record.SuperviseRequest{Op: "issue_close"}, lastStart: 11, want: false},
		{name: "an issue close with no id", id: 0, stop: record.SuperviseRequest{Op: "issue_close"}, lastStart: 11, want: true},
		{name: "a suspend a newer start superseded", id: 10, stop: record.SuperviseRequest{Op: "suspend"}, lastStart: 11, want: false},
		{name: "a suspend of the entry its issue stands at", id: 10, stop: record.SuperviseRequest{Op: "suspend", Generation: 7, Entry: 4}, lastStart: 4, at: at, want: true},
		{name: "a suspend of an earlier phase entry", id: 10, stop: record.SuperviseRequest{Op: "suspend", Generation: 7, Entry: 3}, lastStart: 4, at: at, want: false},
		{name: "an issue close of an earlier run", id: 10, stop: record.SuperviseRequest{Op: "issue_close", Generation: 6, Entry: 4}, lastStart: 4, at: at, want: false},
		{name: "a suspend that names no fence", id: 10, stop: record.SuperviseRequest{Op: "suspend"}, lastStart: 4, at: at, want: true},
		{name: "a start is not a stop", id: 10, stop: record.SuperviseRequest{Op: "start"}, lastStart: 4, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := StopActs(tc.id, tc.stop, tc.lastStart, tc.at); got != tc.want {
				t.Fatalf("StopActs(%d, %s, %d, %+v) = %t, want %t", tc.id, tc.stop.Op, tc.lastStart, tc.at, got, tc.want)
			}
		})
	}
}

// The record keeps whether its pull request merged or closed, whatever phase the issue is in, so a
// re-admitted generation drops a finished pull request and keeps an open one. A merge in merging
// queues the close and leaves the issue in merging until Dispatch shows it done (closeMerged).
func TestTheRecordKeepsWhetherItsPullRequestMergedOrClosed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		at    phase.Phase
		fact  intake.Fact
		want  record.PullRequestState
		phase phase.Phase
	}{
		{name: "merged when merging", at: phase.Merging, fact: intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 42, MergeSHA: "merge"}, want: record.PullRequestMerged, phase: phase.Merging},
		{name: "merged early", at: phase.Reviewing, fact: intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 42, MergeSHA: "merge"}, want: record.PullRequestMerged, phase: phase.Reviewing},
		{name: "closed unmerged", at: phase.Reviewing, fact: intake.PullRequestClosed{Repo: "sjawhar/legion", Number: 42}, want: record.PullRequestClosed, phase: phase.Reviewing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: tc.at, Generation: 1, Status: "needs_review", Rank: "U"})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head",
				Failing: []string{}, CheckRuns: []record.AttemptRun{}})
			if _, err := intake.ApplyFact(context.Background(), pool, "github", "pr-finished", tc.fact, testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
				t.Fatalf("ApplyFact: %v", err)
			}
			var state record.PullRequestState
			var current phase.Phase
			if err := pool.QueryRow(context.Background(), "select pr.state, i.phase from pull_requests pr join issues i on i.key = pr.issue where pr.issue = 'LEGION-208'").Scan(&state, &current); err != nil {
				t.Fatalf("read pull request: %v", err)
			}
			if state != tc.want || current != tc.phase {
				t.Fatalf("pull request %s with the issue in %s, want %s in %s", state, current, tc.want, tc.phase)
			}
		})
	}
}
