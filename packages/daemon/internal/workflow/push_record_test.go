package workflow

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// recordedPush is the newest push of issue's branch the daemon's store holds, and whether it holds
// one at all.
func recordedPush(t *testing.T, pool *pgxpool.Pool, issue string) (time.Time, string, bool) {
	t.Helper()
	var at time.Time
	var head string
	err := pool.QueryRow(context.Background(), `select pushed_at, head from issue_pushes where issue = $1`, issue).Scan(&at, &head)
	if err != nil {
		return time.Time{}, "", false
	}
	return at, head, true
}

// A push of an issue's branch is recorded from the push fact itself, whether or not the issue has
// a pull request: a branch is pushed before its pull request opens, and an issue with no
// pull-request record is exactly the one a stale copy of its workspace would otherwise be sent
// for (store.TakeWorkspaceCopyForRestore). The record is the daemon's clock when it applied the
// push and the head the push left, and the copy the daemon takes for it is asked for only once
// the fact has committed, so the copy can read the record it was asked for. A later push moves
// the record; a push of a branch of the same name on another repository, and a branch that names
// no issue, record nothing and ask for nothing.
func TestAPushOfAnIssueWithNoPullRequestIsRecordedAndAsksForACopyOnCommit(t *testing.T) {
	pool := migratedPool(t)
	st, err := store.Open(context.Background(), pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store over the test pool: %v", err)
	}
	t.Cleanup(st.Close)
	clock := &checkClock{now: time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC)}
	var asked []string
	var committed []bool
	engine := New(record.NewStore(), Config{Project: "LEGION", Clock: clock.Now, Pushes: st, PushRecorded: func(issue string) {
		asked = append(asked, issue)
		_, _, held := recordedPush(t, pool, issue)
		committed = append(committed, held)
	}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 1,
		Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
	apply := applyFacts(t, pool, engine)

	apply("push-1", intake.Push{Repo: "sjawhar/legion", Branch: "legion/LEGION-208", Before: "c0", After: "c1"})
	if at, head, held := recordedPush(t, pool, "LEGION-208"); !held || head != "c1" || !at.Equal(clock.now) {
		t.Fatalf("the push recorded head %q at %s (held=%t), want c1 at the daemon's %s", head, at, held, clock.now)
	}
	if !slices.Equal(asked, []string{"LEGION-208"}) || !slices.Equal(committed, []bool{true}) {
		t.Fatalf("the push asked for copies of %v, reading its own record as %v; want one copy of LEGION-208, asked after the record committed", asked, committed)
	}

	clock.now = clock.now.Add(time.Minute)
	apply("push-2", intake.Push{Repo: "sjawhar/legion", Branch: "legion/LEGION-208", Before: "c1", After: "c2"})
	if at, head, _ := recordedPush(t, pool, "LEGION-208"); head != "c2" || !at.Equal(clock.now) {
		t.Fatalf("the later push left the record at %q at %s, want c2 at %s", head, at, clock.now)
	}

	apply("push-3", intake.Push{Repo: "someone/else", Branch: "legion/LEGION-208", Before: "x0", After: "x1"})
	apply("push-4", intake.Push{Repo: "sjawhar/legion", Branch: "main", Before: "m0", After: "m1"})
	if _, head, _ := recordedPush(t, pool, "LEGION-208"); head != "c2" {
		t.Fatalf("a push of another repository's branch moved the record to %q, want c2", head)
	}
	if !slices.Equal(asked, []string{"LEGION-208", "LEGION-208"}) {
		t.Fatalf("the pushes asked for copies of %v, want two of LEGION-208 and nothing for the others", asked)
	}
}
