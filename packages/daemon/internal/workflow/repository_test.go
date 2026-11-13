package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// fixedRepositories is a project's repositories as the daemon would hold them: each served
// repository's default branch by its name, and every left-out repository with its reason.
type fixedRepositories struct {
	served  map[string]string
	leftOut map[string]string
}

func (r fixedRepositories) ResolveRepository(resolved string) (Resolution, error) {
	if branch, ok := r.served[resolved]; ok {
		return Resolution{Repo: resolved, DefaultBranch: branch, GitHubRepo: resolved}, nil
	}
	if reason, ok := r.leftOut[resolved]; ok {
		return Resolution{Refusal: reason, LeftOut: true}, nil
	}
	return Resolution{Refusal: resolved + " is not mapped to project LEGION in Dispatch"}, nil
}

// testRepositories serves every repository the workflow tests record issues on, and leaves out
// acme/gadgets, whose review App is not installed.
var testRepositories = fixedRepositories{
	served:  map[string]string{"sjawhar/legion": "main", "acme/widgets": "main", "acme/tools": "release/v7.1"},
	leftOut: map[string]string{"acme/gadgets": "the review App is not installed on acme/gadgets"},
}

// openTree seeds a running root with an open gate, under which a todo child is recorded.
func openTree(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Planning, Generation: 1, Status: "in_progress", Rank: "U"})
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 1, ApprovedVersion: new(1)})
	seedSlot(t, pool, record.Slot{Issue: "LEGION-208", Index: 0, AdmittedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)})
}

func childTodo(seq int64, resolved string) intake.DispatchIssue {
	return intake.DispatchIssue{Key: "LEGION-209", Seq: seq, Type: "issue.updated", Status: "todo", Title: "child", Parent: "LEGION-208", Rank: "V", ResolvedRepo: resolved}
}

// A child is recorded on the repository its event resolves to, at that repository's default
// branch, whatever the branch is named.
func TestAChildIsRecordedOnItsResolvedRepositoryAndItsDefaultBranch(t *testing.T) {
	pool := migratedPool(t)
	openTree(t, pool)
	apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))

	apply("child-todo", childTodo(1, "acme/tools"))
	// The release records the child; the waiting line is what starts its first phase.
	startUnblockedChildren(t, pool, testEngine(config.DesignGateRootIssues, nil))
	child := issueRecord(t, pool, "LEGION-209")
	if child == nil || child.Repo != "acme/tools" || child.DefaultBranch != "release/v7.1" || child.Phase != phase.Planning {
		t.Fatalf("child = %#v, want it planning on acme/tools at release/v7.1", child)
	}
}

// A child whose repository resolves to none, to one Legion leaves out, or to one not mapped to the
// project is not recorded: the daemon says why on the child and tells the tree's architect, once
// for one resolution, so a repeated event says nothing again.
func TestAChildOnARepositoryLegionDoesNotServeIsNotRecordedAndSaysWhy(t *testing.T) {
	for _, tc := range []struct {
		name, resolved, note, reason string
	}{
		{name: "none", resolved: "",
			note:   "Legion did not admit this issue: its repository resolves to none. Set its repository (the issue header's Repository picker, or dispatch issue-update --repo owner/name) and move it to todo again.",
			reason: "its repository resolves to none"},
		{name: "left out", resolved: "acme/gadgets",
			note:   "Legion did not admit this issue: the review App is not installed on acme/gadgets. Install it on acme/gadgets, or set another repository (the issue header's Repository picker, or dispatch issue-update --repo owner/name), and move the issue to todo again.",
			reason: "the review App is not installed on acme/gadgets"},
		{name: "not mapped", resolved: "other/thing",
			note:   "Legion did not admit this issue: other/thing is not mapped to project LEGION in Dispatch. Set its repository (the issue header's Repository picker, or dispatch issue-update --repo owner/name) and move it to todo again.",
			reason: "other/thing is not mapped to project LEGION in Dispatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			openTree(t, pool)
			apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))

			apply("child-todo", childTodo(1, tc.resolved))
			apply("child-edited", childTodo(2, tc.resolved))
			if child := issueRecord(t, pool, "LEGION-209"); child != nil {
				t.Fatalf("child recorded as %#v, want none", child)
			}
			messages := outboxOf(t, pool, "dispatch_message")
			if len(messages) != 1 || messages[0].issue != "LEGION-209" || messages[0].payload["body"] != tc.note {
				t.Fatalf("Dispatch messages = %#v, want one on LEGION-209 saying %q", messages, tc.note)
			}
			notices := outboxOf(t, pool, "notice")
			if len(notices) != 1 || notices[0].issue != "LEGION-208" || notices[0].payload["kind"] != "child-not-recorded" ||
				!strings.Contains(notices[0].payload["reason"].(string), tc.reason) {
				t.Fatalf("notices = %#v, want one child-not-recorded on LEGION-208 naming %q", notices, tc.reason)
			}
		})
	}
}

// A recorded issue's repository follows the one Dispatch resolves it to: an event naming another
// repository the project serves corrects the record onto it and its default branch, keeps the
// repository the issue left with the moment of the correction, and says nothing on the issue,
// since there is nothing for anyone to do. A child set back to todo re-enters its tree on the
// corrected repository: its re-entry writes the record from a copy read before the correction and
// moves no repository back (record.Store.PutIssue).
func TestARecordedIssuesRepositoryFollowsDispatchToARepositoryLegionServes(t *testing.T) {
	pool := migratedPool(t)
	openTree(t, pool)
	parent := "LEGION-208"
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "acme/tools", DefaultBranch: "release/v7.1", GitHubRepo: "acme/tools", Tree: "LEGION-208", Project: "LEGION", Title: "child", Parent: &parent,
		Phase: phase.Done, Generation: 1, Status: "backlog", DispatchStatus: "backlog", Rank: "V", LastDispatchSeq: 2})
	apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
	apply("child-back-to-todo-elsewhere", childTodo(3, "acme/widgets"))
	edited := childTodo(4, "acme/widgets")
	edited.Title = "child, renamed"
	apply("child-renamed", edited)
	child := issueRecord(t, pool, "LEGION-209")
	if child == nil || child.Repo != "acme/widgets" || child.DefaultBranch != "main" || child.GitHubRepo != "acme/widgets" || child.Generation != 2 {
		t.Fatalf("child = %#v, want it re-entered at generation 2 on acme/widgets at main", child)
	}
	if child.RepoCorrection == nil || child.RepoCorrection.Before != "acme/tools" {
		t.Fatalf("the correction = %+v, want acme/tools left at the moment the correction was applied", child.RepoCorrection)
	}
	if messages := outboxOf(t, pool, "dispatch_message"); len(messages) != 0 {
		t.Fatalf("Dispatch messages = %#v, want none: the record followed Dispatch, so there is nothing to tell anyone", messages)
	}
}

// A recorded issue whose Dispatch resolution is now its repository's rename partner is told
// nothing and corrected in nothing: the record keeps sjawhar/legion, Dispatch resolves the issue
// to cybermonklabs/legion, and a configured rename makes the two one repository, so re-pointing
// every open issue at the repository a transfer moved to — the step the transfer takes in
// Dispatch — writes nothing on any of them. With no pair configured the same resolution is a
// repository change like any other, onto a repository this project does not serve: the record
// keeps sjawhar/legion, the issue is told once, and the change waits on the record.
func TestARecordedIssueSaysNothingWhenItsResolutionIsItsRenamePartner(t *testing.T) {
	for _, tc := range []struct {
		name    string
		renames ghrepo.Renames
		want    string
	}{
		{name: "the pair configured", renames: renamedLegion},
		{name: "no pair configured", renames: ghrepo.Renames{},
			want: "Legion did not move this issue off sjawhar/legion: cybermonklabs/legion is not mapped to project LEGION in Dispatch. Map cybermonklabs/legion to the project in Dispatch, or set another repository (the issue header's Repository picker, or dispatch issue-update --repo owner/name). Legion moves the issue itself once it serves the repository that Dispatch resolves the issue to."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			openTree(t, pool)
			parent := "LEGION-208"
			seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "child", Parent: &parent,
				Phase: phase.Implementing, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress", Rank: "V", LastDispatchSeq: 2})
			apply := applyFacts(t, pool, renamedEngine(tc.renames))
			repointed := childTodo(3, "cybermonklabs/legion")
			repointed.Status, repointed.Title = "in_progress", "child"
			apply("re-pointed", repointed)
			apply("re-pointed-again", childTodo(4, "cybermonklabs/legion"))
			child := issueRecord(t, pool, "LEGION-209")
			if child == nil || child.Repo != "sjawhar/legion" || child.GitHubRepo != "sjawhar/legion" {
				t.Fatalf("child = %#v, want it still recorded on sjawhar/legion: a rename rewrites no record", child)
			}
			var kept []outboxEntry
			for _, message := range outboxOf(t, pool, "dispatch_message") {
				if strings.HasPrefix(message.payload["body"].(string), "Legion did not move this issue off ") {
					kept = append(kept, message)
				}
			}
			if tc.want == "" {
				if len(kept) != 0 {
					t.Fatalf("repository notes = %#v, want none: the resolution is the issue's repository under the configured pair", kept)
				}
				if child.RepoChange != nil {
					t.Fatalf("the waiting change = %+v, want none: nothing changed", child.RepoChange)
				}
				return
			}
			if len(kept) != 1 || kept[0].issue != "LEGION-209" || kept[0].payload["body"] != tc.want {
				t.Fatalf("repository notes = %#v, want one saying %q", kept, tc.want)
			}
			if c := child.RepoChange; c == nil || c.Pending != "cybermonklabs/legion" || c.Reason == "" {
				t.Fatalf("the waiting change = %+v, want cybermonklabs/legion and why it waits", c)
			}
		})
	}
}

// A recorded issue Dispatch resolves to no repository at all is told so, and leaves no change
// waiting on the record: there is no repository to change to, so the change a refused resolution
// wrote is cleared rather than written again with an empty repository beside a reason
// (record.Issue.RepoChange). The record keeps the repository it has.
func TestARecordedIssueResolvedToNoRepositoryIsToldAndLeavesNoChangeWaiting(t *testing.T) {
	pool := migratedPool(t)
	openTree(t, pool)
	parent := "LEGION-208"
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "child", Parent: &parent,
		Phase: phase.Implementing, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress", Rank: "V", LastDispatchSeq: 2})
	apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
	leftOut := childTodo(3, "acme/gadgets")
	leftOut.Status = "in_progress"
	apply("left-out", leftOut)
	if child := issueRecord(t, pool, "LEGION-209"); child == nil || child.RepoChange == nil || child.RepoChange.Pending != "acme/gadgets" {
		t.Fatalf("child = %#v, want acme/gadgets waiting on the record before Dispatch resolves it to none", child)
	}
	none := childTodo(4, "")
	none.Status = "in_progress"
	apply("resolved-to-none", none)
	child := issueRecord(t, pool, "LEGION-209")
	if child == nil || child.Repo != "sjawhar/legion" || child.RepoCorrection != nil {
		t.Fatalf("child = %#v, want it still recorded on sjawhar/legion, corrected in nothing", child)
	}
	if child.RepoChange != nil {
		t.Fatalf("the waiting change = %+v, want none: Dispatch has named no repository to change to", child.RepoChange)
	}
	want := "Legion did not move this issue off sjawhar/legion: its repository resolves to none. Set its repository (the issue header's Repository picker, or dispatch issue-update --repo owner/name). Legion moves the issue itself once it serves the repository that Dispatch resolves the issue to."
	messages := outboxOf(t, pool, "dispatch_message")
	if len(messages) != 2 || messages[1].issue != "LEGION-209" || messages[1].payload["body"] != want {
		t.Fatalf("Dispatch messages = %#v, want a second one on LEGION-209 saying %q", messages, want)
	}
}

// A retained event names no repository (ResolvedRepoMissing). A recorded issue's is applied with
// its record's repository, so it says nothing of a repository change; an unrecorded issue's is
// refused, so the event is retried until its repository is read, and nothing is recorded on a
// repository nobody read.
func TestARetainedEventIsAppliedWithTheRecordsRepositoryAndNeverRecordsAnUnreadOne(t *testing.T) {
	pool := migratedPool(t)
	openTree(t, pool)
	parent := "LEGION-208"
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "acme/tools", DefaultBranch: "release/v7.1", GitHubRepo: "acme/tools", Tree: "LEGION-208", Project: "LEGION", Title: "child", Parent: &parent,
		Phase: phase.Done, Generation: 1, Status: "backlog", DispatchStatus: "backlog", Rank: "V", LastDispatchSeq: 2})
	engine := testEngine(config.DesignGateRootIssues, nil)
	retained := childTodo(3, "")
	retained.ResolvedRepoMissing = true
	applyFacts(t, pool, engine)("retained-reopen", retained)
	if child := issueRecord(t, pool, "LEGION-209"); child.Repo != "acme/tools" || child.Generation != 2 {
		t.Fatalf("child = %#v, want it re-entered on acme/tools", child)
	}
	if messages := outboxOf(t, pool, "dispatch_message"); len(messages) != 0 {
		t.Fatalf("Dispatch messages = %#v, want none: the event names no repository change", messages)
	}

	unrecorded := intake.DispatchIssue{Key: "LEGION-210", Seq: 1, Type: "issue.updated", Status: "todo", Title: "new child", Parent: "LEGION-208", Rank: "W", ResolvedRepoMissing: true}
	if _, err := intake.ApplyFact(context.Background(), pool, "api", "retained-new-child", unrecorded, engine, admissionStub{}); err == nil ||
		!strings.Contains(err.Error(), "predates Dispatch's resolved_repo") {
		t.Fatalf("applying a retained event of an unrecorded child = %v, want it refused for retry", err)
	}
	if child := issueRecord(t, pool, "LEGION-210"); child != nil {
		t.Fatalf("LEGION-210 recorded as %#v, want nothing recorded", child)
	}
}

type outboxEntry struct {
	issue   string
	payload map[string]any
}

func outboxOf(t *testing.T, pool *pgxpool.Pool, kind string) []outboxEntry {
	t.Helper()
	rows, err := pool.Query(context.Background(), "select issue, payload from outbox where kind = $1 order by id", kind)
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	defer rows.Close()
	var entries []outboxEntry
	for rows.Next() {
		var entry outboxEntry
		var payload []byte
		if err := rows.Scan(&entry.issue, &payload); err != nil {
			t.Fatalf("scan outbox: %v", err)
		}
		if err := json.Unmarshal(payload, &entry.payload); err != nil {
			t.Fatalf("decode outbox payload: %v", err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate outbox: %v", err)
	}
	return entries
}

func issueRecord(t *testing.T, pool *pgxpool.Pool, key string) *record.Issue {
	t.Helper()
	var issue *record.Issue
	seedRecord(t, pool, func(tx pgx.Tx) error {
		var err error
		issue, err = record.NewStore().Issue(context.Background(), tx, key)
		return err
	})
	return issue
}
