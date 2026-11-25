package workflow

import (
	"context"
	"encoding/json"
	"slices"
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

// A child leaving the workflow ends only the child: the shipped daemon lingers the closed issue's
// own tree and wakes the tree's architect for a child (reducers.ts reduceIssueClosed). Here the
// root is mid-implementation when its child's pull request merges, or a human moves the child out
// of the workflow. The child leaves the table: its phase is parked in done, so no transition or status
// write follows. The root keeps its phase, its linger stays unarmed, none of its claims is stopped,
// and the architect is told which child left and how. What the child's claims get is the
// status's: a child closed as done by a human has every claim closed (issue_close), and one
// parked in backlog, icebox or triage has every claim suspended, and its pod goes with the Sandbox
// the one IssueSuspend its leave writes deletes. A merge no longer takes a child out of the
// workflow: it waits in verifying for the checks of its goal (close_wait_test.go).
func TestAChildLeavingTheWorkflowNeverClosesItsTree(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fact   intake.Fact
		notice record.NoticeKind
		op     string
	}{
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
			engine := New(record.NewStore(), Config{Project: "LEGION", Steps: config.DesignGateRootIssues.Steps(), Linger: time.Hour, ProductionCheckRole: claim.RoleTester, Repositories: testRepositories, Clock: func() time.Time { return now }}, nil)

			result, err := intake.ApplyFact(context.Background(), pool, "api", "child-leaves", tc.fact, engine, admissionStub{})
			if err != nil || result.Refusal != nil {
				t.Fatalf("ApplyFact = %#v, %v", result, err)
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
			if childPhase != phase.Done {
				t.Fatalf("child phase = %s, want parked in done", childPhase)
			}
			assertOutboxCount(t, pool, "linger_close", 0)
			stopped := superviseRequests(t, pool, tc.op)
			for _, role := range claim.Roles {
				if stopped["LEGION-209/"+string(role)] != 1 || stopped["LEGION-208/"+string(role)] != 0 {
					t.Fatalf("%s rows %v, want one for every one of the child's claims, and none of the root's", tc.op, stopped)
				}
			}
			for _, other := range []string{"suspend", "issue_close", "tree_close"} {
				if other != tc.op && len(superviseRequests(t, pool, other)) != 0 {
					t.Fatalf("the child's leave as %s queued %s rows %v, want %s rows alone", tc.notice, other, superviseRequests(t, pool, other), tc.op)
				}
			}
			if started := superviseRequests(t, pool, "start"); len(started) != 0 {
				t.Fatalf("starts = %v, want none", started)
			}
			got := issueSuspensions(t, pool, "LEGION-209")
			if len(got) != 1 || got[0].Tree != "LEGION-208" || got[0].Generation != 7 || got[0].TreeGeneration != 7 {
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

// Admission frees the slot of a root a human moves to triage, so the engine lingers its tree as it
// does for backlog, icebox, and done: a tree without a slot must not keep running.
func TestARootMovedToTriageLingersItsTree(t *testing.T) {
	pool := migratedPool(t)
	now := time.Date(2026, 9, 23, 4, 0, 0, 0, time.UTC)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 7, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
	engine := New(record.NewStore(), Config{Project: "LEGION", Steps: config.DesignGateRootIssues.Steps(), Linger: time.Hour, ProductionCheckRole: claim.RoleTester, Repositories: testRepositories, Clock: func() time.Time { return now }}, nil)
	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "root-triage", intake.DispatchIssue{Key: "LEGION-208", ResolvedRepo: "sjawhar/legion", Seq: 2, Type: "issue.updated", Status: "triage", Title: "root", Rank: "U"}, engine, admissionStub{}); err != nil {
		t.Fatalf("ApplyFact: %v", err)
	}
	var rootPhase phase.Phase
	var lingering bool
	if err := pool.QueryRow(context.Background(), "select phase, linger_until is not null from issues where key = 'LEGION-208'").Scan(&rootPhase, &lingering); err != nil {
		t.Fatalf("read root: %v", err)
	}
	if rootPhase != phase.Done || !lingering {
		t.Fatalf("root in triage: phase=%s lingering=%t, want its tree lingering", rootPhase, lingering)
	}
	assertOutboxCount(t, pool, "linger_close", 1)
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
// never dropped. It names no linger, so no root is read for it; a tree close acts on its linger
// alone.
func TestStopActsFencesAnIssueCloseLikeASuspend(t *testing.T) {
	until := time.Date(2026, 9, 23, 5, 0, 0, 0, time.UTC)
	lingering := &record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Generation: 7, LingerUntil: &until}
	for _, tc := range []struct {
		name      string
		id        int64
		stop      record.SuperviseRequest
		lastStart int64
		root      *record.Issue
		want      bool
	}{
		{name: "an issue close no start followed", id: 10, stop: record.SuperviseRequest{Op: "issue_close"}, lastStart: 4, want: true},
		{name: "an issue close a newer start superseded", id: 10, stop: record.SuperviseRequest{Op: "issue_close"}, lastStart: 11, want: false},
		{name: "an issue close with no id", id: 0, stop: record.SuperviseRequest{Op: "issue_close"}, lastStart: 11, want: true},
		{name: "an issue close of a lingering tree reads no root", id: 10, stop: record.SuperviseRequest{Op: "issue_close"}, lastStart: 11, root: lingering, want: false},
		{name: "a suspend a newer start superseded", id: 10, stop: record.SuperviseRequest{Op: "suspend"}, lastStart: 11, want: false},
		// A stopped task's step stop is fenced as a suspend is (stopTask): a later version of the
		// plan that starts the task again writes a start the stop must not undo.
		{name: "a step stop no start followed", id: 10, stop: record.SuperviseRequest{Op: "step_stop"}, lastStart: 4, want: true},
		{name: "a step stop a newer start superseded", id: 10, stop: record.SuperviseRequest{Op: "step_stop"}, lastStart: 11, want: false},
		{name: "a tree close of its linger", id: 10, stop: record.SuperviseRequest{Op: "tree_close", Linger: 7}, lastStart: 11, root: lingering, want: true},
		{name: "a tree close with no root", id: 10, stop: record.SuperviseRequest{Op: "tree_close", Linger: 7}, lastStart: 4, want: false},
		{name: "a start is not a stop", id: 10, stop: record.SuperviseRequest{Op: "start"}, lastStart: 4, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := StopActs(tc.id, tc.stop, tc.lastStart, tc.root); got != tc.want {
				t.Fatalf("StopActs(%d, %s, %d, %v) = %t, want %t", tc.id, tc.stop.Op, tc.lastStart, tc.root != nil, got, tc.want)
			}
		})
	}
}

// The record keeps whether its pull request merged or closed, whatever phase the issue is in, so a
// re-admitted generation drops a finished pull request and keeps an open one.
func TestTheRecordKeepsWhetherItsPullRequestMergedOrClosed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		at    phase.Phase
		fact  intake.Fact
		want  record.PullRequestState
		phase phase.Phase
	}{
		{name: "merged when merging", at: phase.Merging, fact: intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 42, MergeSHA: "merge"}, want: record.PullRequestMerged, phase: phase.Verifying},
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

// The notice that tells the architect above a child that the child left the workflow keeps its
// first sentence, "<KEY> is <status>", and names after it what the move leaves for that architect
// to act on (dispatch://INFRA-3178/spec R4): the child's pull request while it is open, since the
// daemon closes no pull request, and each recorded issue under the child that is not done, with
// its status and in key order, since the move ends the child alone. A pull request that merged or
// closed, an issue under the child that is done and an issue outside the child's subtree are not
// named, and a child with nothing to name gets the first sentence alone, which every earlier reader
// of the notice reads.
func TestTheNoticeOfAChildsMoveNamesItsOpenPullRequestAndEachIssueUnderItNotDone(t *testing.T) {
	member := func(key, parent string, at phase.Phase, status string) record.Issue {
		return record.Issue{Key: key, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION",
			Title: key, Parent: &parent, Phase: at, Generation: 7, Status: status, Rank: "V", LastDispatchSeq: 1}
	}
	under := []record.Issue{
		member("LEGION-210", "LEGION-209", phase.Implementing, "in_progress"),
		member("LEGION-211", "LEGION-209", phase.Done, "backlog"),
		member("LEGION-212", "LEGION-209", phase.Done, "done"),
		member("LEGION-213", "LEGION-210", phase.Admitted, "todo"),
		member("LEGION-214", "LEGION-208", phase.Implementing, "in_progress"),
	}
	for _, tc := range []struct {
		name   string
		status string
		// pr is the state of the child's pull request #43, "" for none.
		pr    record.PullRequestState
		under []record.Issue
		kind  record.NoticeKind
		want  string
	}{
		{name: "an open pull request", status: "done", pr: record.PullRequestOpen, kind: "child-closed",
			want: "LEGION-209 is done. Its pull request #43 is open"},
		{name: "issues under it", status: "done", under: under, kind: "child-closed",
			want: "LEGION-209 is done. Under it, not done: LEGION-210 (in_progress), LEGION-211 (backlog), LEGION-213 (todo)"},
		{name: "an open pull request and an issue under it", status: "triage", pr: record.PullRequestOpen, under: under[:1], kind: "child-status",
			want: "LEGION-209 is triage. Its pull request #43 is open. Under it, not done: LEGION-210 (in_progress)"},
		{name: "nothing to name beside a closed pull request", status: "backlog", pr: record.PullRequestClosed, under: under[2:3], kind: "child-status",
			want: "LEGION-209 is backlog"},
		{name: "nothing to name beside a merged pull request", status: "done", pr: record.PullRequestMerged, under: under[4:], kind: "child-closed",
			want: "LEGION-209 is done"},
		{name: "nothing to name at all", status: "icebox", kind: "child-status", want: "LEGION-209 is icebox"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			now := time.Date(2026, 9, 23, 4, 0, 0, 0, time.UTC)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 7, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
			seedIssue(t, pool, member("LEGION-209", "LEGION-208", phase.Implementing, "in_progress"))
			for _, issue := range tc.under {
				seedIssue(t, pool, issue)
			}
			if tc.pr != "" {
				seedPR(t, pool, record.PullRequest{State: tc.pr, Issue: "LEGION-209", Repo: "sjawhar/legion", Number: 43, Branch: "legion/LEGION-209", HeadSHA: "head-43", MergeSHA: "merge-43", Failing: []string{}})
			}
			engine := New(record.NewStore(), Config{Project: "LEGION", Steps: config.DesignGateRootIssues.Steps(), Linger: time.Hour, ProductionCheckBound: 48 * time.Hour, ProductionCheckRole: claim.RoleTester, Repositories: testRepositories, Clock: func() time.Time { return now }}, nil)
			kind := "issue.updated"
			if tc.status == record.DoneStatus {
				kind = "issue.closed"
			}
			move := intake.DispatchIssue{Key: "LEGION-209", ResolvedRepo: "sjawhar/legion", Seq: 2, Type: kind, Status: tc.status, Title: "LEGION-209", Parent: "LEGION-208", Rank: "V"}
			if result, err := intake.ApplyFact(context.Background(), pool, "dispatch", "child-moves", move, engine, admissionStub{}); err != nil || result.Refusal != nil {
				t.Fatalf("ApplyFact = %#v, %v", result, err)
			}
			var told []record.Notice
			for _, notice := range architectNotices(t, pool) {
				if notice.Kind == "child-closed" || notice.Kind == "child-status" {
					told = append(told, notice)
				}
			}
			if len(told) != 1 || told[0].Kind != tc.kind || told[0].Reason != tc.want {
				var got []string
				for _, notice := range told {
					got = append(got, string(notice.Kind)+": "+notice.Reason)
				}
				t.Fatalf("the notices of the child's move to %s = %q, want one %s reading %q", tc.status, got, tc.kind, tc.want)
			}
		})
	}
}

// A child its owning architect, or a person, closed done while its pull request was open leaves
// that pull request open: the daemon changes no pull request. Here the child awaited its merge,
// so its pull request may stand in the merge queue, and the queue merges it after the close. That
// merge is recorded, with its merge commit, and the architect is told pr-merged, as for the merge
// of any issue that does not await it (merged), and nothing else is written: the child is not
// closed again, no status is written to Dispatch, so the child is not reopened, no claim is
// stopped or started, and no production check opens (dispatch://INFRA-3178/spec R4). The record
// is the same whoever wrote the done, so a person's done stands in for the owner's here.
func TestAMergeOfTheOpenPullRequestOfAClosedChildClosesNothing(t *testing.T) {
	pool := migratedPool(t)
	now := time.Date(2026, 10, 19, 4, 0, 0, 0, time.UTC)
	seedMergingChild(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 7, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
	engine := New(record.NewStore(), Config{Project: "LEGION", Steps: config.DesignGateRootIssues.Steps(), Linger: time.Hour, ProductionCheckBound: 48 * time.Hour, ProductionCheckRole: claim.RoleTester, Repositories: testRepositories, Clock: func() time.Time { return now }}, nil)
	done := intake.DispatchIssue{Key: "LEGION-209", ResolvedRepo: "sjawhar/legion", Seq: 2, Type: "issue.closed", Status: "done", Title: "child", Parent: "LEGION-208", Rank: "V"}
	if result, err := intake.ApplyFact(context.Background(), pool, "dispatch", "child-done", done, engine, admissionStub{}); err != nil || result.Refusal != nil {
		t.Fatalf("ApplyFact done = %#v, %v", result, err)
	}
	// Admission records the done's status beneath the engine (admit's recordObservation), and the
	// stub here records nothing, so the test writes that observation as admission would.
	seedRecord(t, pool, func(tx pgx.Tx) error {
		store := record.NewStore()
		child, err := store.Issue(t.Context(), tx, "LEGION-209")
		if err != nil || child == nil {
			t.Fatalf("read the child after its done: %+v, %v", child, err)
		}
		child.Status, child.DispatchStatus, child.LastDispatchSeq = record.DoneStatus, record.DoneStatus, done.Seq
		return store.PutIssue(t.Context(), tx, *child)
	})
	var childPhase phase.Phase
	var childStatus string
	if err := pool.QueryRow(context.Background(), "select phase, status from issues where key = 'LEGION-209'").Scan(&childPhase, &childStatus); err != nil || childPhase != phase.Done || childStatus != record.DoneStatus {
		t.Fatalf("the child after its done = %s/%s, %v; want done/done: this test proves nothing", childPhase, childStatus, err)
	}
	var last int64
	if err := pool.QueryRow(context.Background(), "select coalesce(max(id), 0) from outbox").Scan(&last); err != nil {
		t.Fatalf("read the last outbox row: %v", err)
	}

	merge := intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 43, MergeSHA: "merge-43", HeadSHA: "head-43"}
	if result, err := intake.ApplyFact(context.Background(), pool, "github", "merged", merge, engine, admissionStub{}); err != nil || result.Refusal != nil {
		t.Fatalf("ApplyFact merge = %#v, %v", result, err)
	}

	var pr *record.PullRequest
	seedRecord(t, pool, func(tx pgx.Tx) error {
		var err error
		pr, err = record.NewStore().PullRequest(t.Context(), tx, "LEGION-209")
		return err
	})
	if pr == nil || pr.State != record.PullRequestMerged || pr.MergeSHA != "merge-43" {
		t.Fatalf("the child's pull request after the merge = %+v, want it recorded merged at merge-43", pr)
	}
	if err := pool.QueryRow(context.Background(), "select phase, status from issues where key = 'LEGION-209'").Scan(&childPhase, &childStatus); err != nil || childPhase != phase.Done || childStatus != record.DoneStatus {
		t.Fatalf("the child after the merge = %s/%s, %v; want done/done as the done left it", childPhase, childStatus, err)
	}
	rows, err := pool.Query(context.Background(), "select kind, coalesce(payload->>'kind', ''), coalesce(payload->>'reason', '') from outbox where id > $1 order by id", last)
	if err != nil {
		t.Fatalf("read the rows the merge added: %v", err)
	}
	type added struct{ kind, notice, reason string }
	got, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (added, error) {
		var a added
		return a, row.Scan(&a.kind, &a.notice, &a.reason)
	})
	if err != nil {
		t.Fatalf("collect the rows the merge added: %v", err)
	}
	want := []added{{kind: "notice", notice: "pr-merged", reason: "pull request #43 merged while LEGION-209 was in done, not merging"}}
	if !slices.Equal(got, want) {
		t.Fatalf("the rows the merge added = %+v, want the pr-merged notice alone, %+v", got, want)
	}
	if check := productionCheck(t, pool, "LEGION-209"); check != nil {
		t.Fatalf("the production check of the child = %+v, want none: its done came before the merge", check)
	}
}
