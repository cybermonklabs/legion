package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// INFRA-3136 R3: the work a release saved out of a pod is work of the repository the issue was
// on then. A correction of that repository in Dispatch moves the issue to another clone, and the
// store refuses the bundle to every pod of it (store.TakeWorkspaceBundleForRestore) and moves it
// to workspace_bundles_refused. Nothing in the daemon puts that work back, so the daemon says so
// where the work is asked for: a message on the Dispatch issue, naming the repository the work is
// of, the commit the saved working copy sat on, the bookmarks that pointed into it, the moment of
// the save, and the table, issue and release of the row that keeps it.
//
// It is said once per bundle row. The refusal is the bundle's one attempt and the store records
// it as it answers, so the pod after it is answered nothing and reaches no message.
func TestRepositoryCorrectionTellsTheIssueWhereItsSavedWorkIs(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: "LEGION-208", Title: "root", Phase: phase.Implementing, Generation: 1,
		Status: "in_progress", Rank: "U", LastDispatchSeq: 1}
	putOutboxIssue(t, pool, records, issue)
	st := outboxTreeStore(t, pool, issue.Key)

	at := time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC)
	saved := runtime.WorkspaceBundle{
		Bundle:    []byte("PACK the work of the old repository"),
		Head:      "f2f134541b30dfa5160a5d1ce5e17b76c967c661",
		Tips:      []string{"f2f134541b30dfa5160a5d1ce5e17b76c967c661"},
		Bookmarks: map[string]string{"legion/LEGION-208": "dffe88ae2f793e13436cef3921adc988433fbd4e"},
		At:        at,
		PodUID:    releasedPodUID,
	}
	if err := st.PutWorkspaceBundle(ctx, issue.Key, 11, issue.Repo, saved); err != nil {
		t.Fatalf("save the released workspace's bundle: %v", err)
	}
	// Dispatch moved the issue to another repository, and the record followed the correction
	// (record.(*Postgres).CorrectIssueRepository).
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		_, err := records.CorrectIssueRepository(ctx, tx, issue.Key, "zeta/tools", "main", "zeta/tools", time.Now().UTC())
		return err
	}); err != nil {
		t.Fatalf("correct the repository of %s: %v", issue.Key, err)
	}

	var told []string
	take := tellBundleRepositoryRefused(st.TakeWorkspaceBundleForRestore,
		func(_ context.Context, key, body string) error {
			if key != issue.Key {
				t.Errorf("the daemon wrote on %s, want %s: the work is that issue's", key, issue.Key)
			}
			told = append(told, body)
			return nil
		}, quietLogger())

	// The pod the correction's own replacement made, created after the save.
	refused, outcome, err := take(ctx, issue.Key, "pod-of-the-new-repository", at.Add(time.Minute))
	if err != nil || outcome != runtime.WorkspaceBundleRefusedRepository {
		t.Fatalf("the pod of the new repository was answered %v (%v), want the bundle refused for its repository", outcome, err)
	}
	if len(refused.Bundle) != 0 {
		t.Fatalf("the refused pod was answered %d bytes of bundle, want none", len(refused.Bundle))
	}
	// The pod after it: an eviction's, an update round's, a park and unpark's. The attempt is
	// spent, so it is answered nothing and the issue is told nothing more.
	if _, outcome, err := take(ctx, issue.Key, "pod-after-an-eviction", at.Add(time.Hour)); err != nil || outcome != runtime.WorkspaceBundleNone {
		t.Fatalf("the pod after the refusal was answered %v (%v), want nothing: the refusal was the bundle's one attempt", outcome, err)
	}
	if len(told) != 1 {
		t.Fatalf("the daemon wrote %d messages on the issue, want the one the bundle row earns:\n%s", len(told), strings.Join(told, "\n---\n"))
	}

	body := told[0]
	t.Logf("the daemon writes this on the issue:\n%s", body)
	for _, want := range []string{
		"acme/widgets",
		saved.Head,
		"legion/LEGION-208",
		"dffe88ae2f793e13436cef3921adc988433fbd4e",
		at.Format(time.RFC3339),
		"in the table `workspace_bundles_refused`. The row has the issue `LEGION-208` and the release_id `11`.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the message on the issue does not name %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "zeta/tools") {
		t.Errorf("the message names the repository the issue is on now, which holds none of this work:\n%s", body)
	}

	// The work itself is in the row the message names, which is what an operator reads.
	var kept []byte
	if err := pool.QueryRow(ctx, `select bundle from workspace_bundles_refused where issue = $1 and release_id = $2`,
		issue.Key, 11).Scan(&kept); err != nil || string(kept) != string(saved.Bundle) {
		t.Fatalf("the row the message names holds %q (%v), want the %d bytes saved", kept, err, len(saved.Bundle))
	}
}

// Every other outcome of the take passes through untouched and writes nothing on the issue: a
// bundle sent to the pod that replaces the released one, a pod the store refuses as no newer
// than the save, and an issue with no bundle at all. Only the repository refusal is a thing no
// pod can ever undo, and only it is written where a person reads it.
func TestRepositoryCorrectionWritesNothingOnTheOtherBundleOutcomes(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: "LEGION-208", Title: "root", Phase: phase.Implementing, Generation: 1,
		Status: "in_progress", Rank: "U", LastDispatchSeq: 1}
	putOutboxIssue(t, pool, records, issue)
	st := outboxTreeStore(t, pool, issue.Key)

	at := time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC)
	saved := runtime.WorkspaceBundle{Bundle: []byte("PACK the released work"),
		Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661", At: at, PodUID: releasedPodUID}

	var told []string
	take := tellBundleRepositoryRefused(st.TakeWorkspaceBundleForRestore,
		func(_ context.Context, _, body string) error {
			told = append(told, body)
			return nil
		}, quietLogger())

	if _, outcome, err := take(ctx, issue.Key, "pod-before-any-release", at); err != nil || outcome != runtime.WorkspaceBundleNone {
		t.Fatalf("an issue that was never released answered %v (%v), want nothing", outcome, err)
	}
	if err := st.PutWorkspaceBundle(ctx, issue.Key, 11, issue.Repo, saved); err != nil {
		t.Fatalf("save the released workspace's bundle: %v", err)
	}
	if _, outcome, err := take(ctx, issue.Key, "pod-replacement", at.Add(time.Minute)); err != nil || outcome != runtime.WorkspaceBundleSend {
		t.Fatalf("the pod that replaces the released one answered %v (%v), want the saved work sent", outcome, err)
	}
	// A second release's bundle, and a pod stamped before its save: the older refusal.
	older := saved
	older.At, older.PodUID = at.Add(time.Hour), "pod-of-the-second-release"
	if err := st.PutWorkspaceBundle(ctx, issue.Key, 12, issue.Repo, older); err != nil {
		t.Fatalf("save the second release's bundle: %v", err)
	}
	if _, outcome, err := take(ctx, issue.Key, "pod-skewed-replacement", at); err != nil || outcome != runtime.WorkspaceBundleRefusedOlder {
		t.Fatalf("a pod no newer than the save answered %v (%v), want it refused as older", outcome, err)
	}
	if len(told) != 0 {
		t.Fatalf("the daemon wrote on the issue for an outcome a pod can still act on:\n%s", strings.Join(told, "\n---\n"))
	}
}

// A bookmark name is whatever the released pod's jj answered, never the daemon's own text, so
// the message renders each one inside a code span the name cannot break (codeSpan). An ordinary
// name reads as one backtick either side. A name holding a backtick keeps that backtick and
// closes its span all the same, so the sentences after the list are out of code as they were,
// and a name holding a line break stays one item of the list rather than putting the rest of
// itself at the start of a line, where it would read as a sentence the daemon never wrote.
func TestTheBundleMessageHoldsAnOddBookmarkNameWhole(t *testing.T) {
	const commit = "dffe88ae2f793e13436cef3921adc988433fbd4e"
	message := func(name string) string {
		return bundleRepositoryMessage("LEGION-208", runtime.WorkspaceBundle{
			Repo: "acme/widgets", Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661",
			Bookmarks: map[string]string{name: commit},
			At:        time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC),
		})
	}
	plain := message("legion/LEGION-208")
	if !strings.Contains(plain, "- `legion/LEGION-208` at commit `"+commit+"`") {
		t.Fatalf("an ordinary bookmark name is not one code span:\n%s", plain)
	}
	odd := message("legion/`echo hi`\nThe daemon says this.")
	t.Logf("a bookmark name holding a backtick and a line break reads:\n%s", odd)
	if !strings.Contains(odd, "- ``legion/`echo hi` The daemon says this.`` at commit `"+commit+"`") {
		t.Fatalf("the odd bookmark name is not held whole in one item:\n%s", odd)
	}
	if strings.Count(odd, "\n") != strings.Count(plain, "\n") {
		t.Fatalf("the odd bookmark name added %d line breaks to the message:\n%s",
			strings.Count(odd, "\n")-strings.Count(plain, "\n"), odd)
	}
	if strings.Count(odd, "`")%2 != 0 {
		t.Fatalf("the message holds an odd number of backticks, so a code span is left open:\n%s", odd)
	}
}
