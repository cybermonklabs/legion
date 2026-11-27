package delivery

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The merge commits the revert path's fixtures name, each 40 hex digits as delivery_reverts keeps
// them.
var (
	revertMergeCommit   = strings.Repeat("a1", 20)
	revertedMergeCommit = strings.Repeat("b2", 20)
	otherMergeCommit    = strings.Repeat("c3", 20)
)

// revertPullJSON is GitHub's REST answer for pull request number of acme/widgets, authored by
// someone outside every population: its body, its merge commit, and merged at merged.
func revertPullJSON(number int, body, merged, mergeCommit string) map[string]any {
	return map[string]any{
		"number": number, "title": fmt.Sprintf("Revert of #%d", number),
		"html_url": fmt.Sprintf("https://github.com/acme/widgets/pull/%d", number),
		"user":     map[string]any{"login": "stranger"}, "body": body, "state": "closed", "merged": true,
		"created_at": "2026-11-19T08:00:00Z", "merged_at": merged, "merge_commit_sha": mergeCommit,
	}
}

// pullReads answers GET /repos/acme/widgets/pulls/{number} on fake with each pull request of
// pulls, by number, and counts the reads of each number, a number pulls does not hold included,
// which it answers 404.
func pullReads(t *testing.T, fake *fakeGitHub, pulls map[int]map[string]any) func(number int) int32 {
	t.Helper()
	var mu sync.Mutex
	reads := map[int]*atomic.Int32{}
	counter := func(number int) *atomic.Int32 {
		mu.Lock()
		defer mu.Unlock()
		if reads[number] == nil {
			reads[number] = &atomic.Int32{}
		}
		return reads[number]
	}
	fake.handle("GET /repos/acme/widgets/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		var number int
		if _, err := fmt.Sscan(r.PathValue("number"), &number); err != nil {
			t.Errorf("read the pull request number %q: %v", r.PathValue("number"), err)
		}
		counter(number).Add(1)
		pull, held := pulls[number]
		if !held {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
			return
		}
		mustEncode(t, w, pull)
	})
	return func(number int) int32 { return counter(number).Load() }
}

// storedRevert is one row of delivery_reverts: the Revert it records, and when it was last
// written.
type storedRevert struct {
	Revert
	recordedAt time.Time
}

// String is the row as a failure names it.
func (s storedRevert) String() string {
	shown := func(sha *string) string {
		if sha == nil {
			return "none"
		}
		return *sha
	}
	return fmt.Sprintf("%s#%d reverts %s#%d (merge commit %s), merged %s as %s",
		s.Repo, s.Number, s.RevertsRepo, s.RevertsNumber, shown(s.RevertsMergeCommitSHA),
		s.MergedAt.UTC().Format(time.RFC3339), shown(s.MergeCommitSHA))
}

// readReverts is every row of delivery_reverts, by repository and number.
func readReverts(t *testing.T, ctx context.Context, pool *store.Pool) []storedRevert {
	t.Helper()
	rows, err := pool.Query(ctx, `
		select repo, number, reverts_repo, reverts_number, reverts_merge_commit_sha, merged_at, merge_commit_sha, recorded_at
		from delivery_reverts order by repo, number`)
	if err != nil {
		t.Fatalf("read delivery_reverts: %v", err)
	}
	defer rows.Close()
	var stored []storedRevert
	for rows.Next() {
		var one storedRevert
		if err := rows.Scan(&one.Repo, &one.Number, &one.RevertsRepo, &one.RevertsNumber, &one.RevertsMergeCommitSHA,
			&one.MergedAt, &one.MergeCommitSHA, &one.recordedAt); err != nil {
			t.Fatalf("read delivery_reverts: %v", err)
		}
		stored = append(stored, one)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read delivery_reverts: %v", err)
	}
	return stored
}

// requireOneRevert fails the test unless rows is the one row want names, and answers it.
func requireOneRevert(t *testing.T, when string, rows []storedRevert, want string) storedRevert {
	t.Helper()
	if len(rows) != 1 || rows[0].String() != want {
		t.Fatalf("%s, delivery_reverts holds %v; want the one row %s", when, rows, want)
	}
	return rows[0]
}

// TestTheIntakeRecordsAMergedRevertOnceWithTheRevertedPullRequestsMergeCommit: the merge of a
// deploy repository's pull request whose envelope body carries GitHub's revert line is read from
// GitHub and recorded from GitHub's answers (Intake.recordRevert, recordRevertFromGitHub): the
// revert's own merge time and merge commit, the pull request its body's revert line names, and
// that pull request's merge commit, read from GitHub's answer for it. A second record of the same
// revert, as the reconcile's search makes of one the intake recorded, writes no second row and
// rewrites none. A merge whose envelope body holds no revert line asks GitHub nothing and records
// nothing, and a merge whose body GitHub answers holds no revert line records nothing either,
// whatever its envelope said: what is stored is GitHub's answer.
func TestTheIntakeRecordsAMergedRevertOnceWithTheRevertedPullRequestsMergeCommit(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	rules := readTestRules(t, ctx, pool)
	fake := newFakeGitHub(t)
	reads := pullReads(t, fake, map[int]map[string]any{
		90: revertPullJSON(90, "Back out the retries: they doubled the load.\r\n\r\nReverts acme/widgets#80\r\n", "2026-11-19T10:00:00Z", revertMergeCommit),
		80: revertPullJSON(80, "Add retries", "2026-11-18T10:00:00Z", revertedMergeCommit),
		92: revertPullJSON(92, "Edited since it merged: it names no pull request now.", "2026-11-19T12:00:00Z", otherMergeCommit),
	})
	intake := newIntake(tuningFor(t), nil, pool, nil, fake.newTestClient(), nil, nil, unrenamed())
	want := "acme/widgets#90 reverts acme/widgets#80 (merge commit " + revertedMergeCommit + "), merged 2026-11-19T10:00:00Z as " + revertMergeCommit

	intake.recordRevert(ctx, rules, "acme/widgets", 90, map[string]string{"body": "Back out the retries: they doubled the load.\r\n\r\nReverts acme/widgets#80"})
	first := requireOneRevert(t, "after the revert's merge envelope", readReverts(t, ctx, pool), want)
	if reads(90) != 1 || reads(80) != 1 {
		t.Fatalf("the record read #90 %d times and #80 %d times; want each once: the revert and the pull request it reverts", reads(90), reads(80))
	}

	if err := recordRevertFromGitHub(ctx, pool, fake.newTestClient(), "acme/widgets", 90); err != nil {
		t.Fatalf("record the revert a second time: %v", err)
	}
	again := requireOneRevert(t, "after a second record of the revert", readReverts(t, ctx, pool), want)
	if !again.recordedAt.Equal(first.recordedAt) {
		t.Errorf("a second record of the revert rewrote its row (recorded_at %s, then %s); want it left as it was", first.recordedAt, again.recordedAt)
	}

	intake.recordRevert(ctx, rules, "acme/widgets", 91, map[string]string{"body": "Reverts are hard to read; this one names none."})
	if reads(91) != 0 {
		t.Errorf("a merge whose envelope body holds no revert line read #91 from GitHub %d times; want none", reads(91))
	}
	if err := recordRevertFromGitHub(ctx, pool, fake.newTestClient(), "acme/widgets", 92); err != nil {
		t.Fatalf("record #92, whose body GitHub answers holds no revert line: %v", err)
	}
	requireOneRevert(t, "after two merges that revert nothing", readReverts(t, ctx, pool), want)
	if reads(92) != 1 || reads(80) != 2 {
		t.Errorf("#92 was read %d times and #80 %d times; want #92 once and #80 only for #90's two records", reads(92), reads(80))
	}
}

// TestTheReconcilesRevertSearchRecordsARevertTheIntakeMissedAndARepeatRecordsNoSecondRow: a
// merged revert no envelope reached is found by the reconcile's search of the deploy repository
// (SearchMergedReverts: GitHub's search, with each body, of the pull requests merged into it in
// the step's window whose body holds the word Reverts) and recorded from GitHub's answers for it
// and for the pull request it reverts (reconcileReverts, recordRevertFromGitHub). A result whose
// body only mentions the word costs no read. The step records its progress through the window's
// end, and a second pass, which searches from there less the overlap, finds the same revert and
// writes no second row and rewrites none.
func TestTheReconcilesRevertSearchRecordsARevertTheIntakeMissedAndARepeatRecordsNoSecondRow(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	until := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	fake := newFakeGitHub(t)
	var mu sync.Mutex
	var searches []string
	fake.handle("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		document, variables := decodeGraphQLRequest(t, r)
		if document != searchRevertsQuery {
			t.Errorf("the revert search sent the GraphQL document %q; want searchRevertsQuery, which asks for each body", document)
		}
		search, _ := variables["q"].(string)
		mu.Lock()
		searches = append(searches, search)
		mu.Unlock()
		revert := searchNodeJSON(95, `Revert "Add retries"`, "stranger", "2026-11-19T08:00:00Z", "2026-11-19T10:00:00Z")
		revert["body"] = "Back out the retries.\r\n\r\nReverts acme/widgets#85\r\n"
		mention := searchNodeJSON(96, "docs: how reverts are counted", "octocat", "2026-11-19T09:00:00Z", "2026-11-19T11:00:00Z")
		mention["body"] = "Reverts are counted once their deploy ships."
		mustEncode(t, w, searchResponseJSON(2, []map[string]any{revert, mention}, false, ""))
	})
	reads := pullReads(t, fake, map[int]map[string]any{
		95: revertPullJSON(95, "Back out the retries.\r\n\r\nReverts acme/widgets#85\r\n", "2026-11-19T10:00:00Z", revertMergeCommit),
		85: revertPullJSON(85, "Add retries", "2026-11-18T10:00:00Z", revertedMergeCommit),
	})
	reconcile := NewReconcile(pool, fake.newTestClient(), events.NewBroker(), unrenamed())
	want := "acme/widgets#95 reverts acme/widgets#85 (merge commit " + revertedMergeCommit + "), merged 2026-11-19T10:00:00Z as " + revertMergeCommit

	if err := reconcile.reconcileReverts(ctx, "acme", "widgets", "acme/widgets", until); err != nil {
		t.Fatalf("the reconcile's revert step: %v", err)
	}
	first := requireOneRevert(t, "after the reconcile's revert step", readReverts(t, ctx, pool), want)
	if reads(95) != 1 || reads(85) != 1 || reads(96) != 0 {
		t.Errorf("the step read #95 %d, #85 %d and #96 %d times; want #95 and #85 once and #96, whose body only mentions the word, never", reads(95), reads(85), reads(96))
	}
	windowed := func(since time.Time) string {
		return `repo:acme/widgets is:pr is:merged in:body "Reverts" merged:` + since.Format(time.RFC3339) + ".." + until.Format(time.RFC3339)
	}
	if want := windowed(until.Add(-BackfillWindow)); len(searches) != 1 || searches[0] != want {
		t.Fatalf("the step searched %q; want one search, %q", searches, want)
	}
	through, err := ReconcileProgressThrough(ctx, pool, revertsStep("acme/widgets"), "acme/widgets")
	if err != nil || !through.Equal(until) {
		t.Fatalf("the step's progress is %s (%v); want the window's end, %s", through, err, until)
	}

	if err := reconcile.reconcileReverts(ctx, "acme", "widgets", "acme/widgets", until); err != nil {
		t.Fatalf("the reconcile's second revert step: %v", err)
	}
	again := requireOneRevert(t, "after a second pass over the window", readReverts(t, ctx, pool), want)
	if !again.recordedAt.Equal(first.recordedAt) {
		t.Errorf("the second pass rewrote the revert's row (recorded_at %s, then %s); want it left as it was", first.recordedAt, again.recordedAt)
	}
	if want := windowed(until.Add(-reconcileOverlap)); len(searches) != 2 || searches[1] != want {
		t.Errorf("the second pass searched %q; want %q, from the recorded progress less the overlap", searches[1:], want)
	}
}
