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

// pushRecordRig is an engine whose push handler records into the daemon's own store, at a clock
// the test moves, for one issue, LEGION-208, on sjawhar/legion. asked is every issue the handler
// asked a copy of the workspace for, in order, and committed whether the push's record could be
// read when it asked.
type pushRecordRig struct {
	pool      *pgxpool.Pool
	clock     *checkClock
	apply     func(id string, fact intake.Fact) intake.Result
	asked     []string
	committed []bool
}

func newPushRecordRig(t *testing.T) *pushRecordRig {
	t.Helper()
	pool := migratedPool(t)
	st, err := store.Open(context.Background(), pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store over the test pool: %v", err)
	}
	t.Cleanup(st.Close)
	rig := &pushRecordRig{pool: pool, clock: &checkClock{now: time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC)}}
	engine := New(record.NewStore(), Config{Project: "LEGION", Clock: rig.clock.Now, Pushes: st, PushRecorded: func(issue string) {
		rig.asked = append(rig.asked, issue)
		_, _, held := recordedPush(t, pool, issue)
		rig.committed = append(rig.committed, held)
	}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 1,
		Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
	rig.apply = applyFacts(t, pool, engine)
	return rig
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
	rig := newPushRecordRig(t)

	rig.apply("push-1", intake.Push{Repo: "sjawhar/legion", Branch: "legion/LEGION-208", Before: "c0", After: "c1"})
	if at, head, held := recordedPush(t, rig.pool, "LEGION-208"); !held || head != "c1" || !at.Equal(rig.clock.now) {
		t.Fatalf("the push recorded head %q at %s (held=%t), want c1 at the daemon's %s", head, at, held, rig.clock.now)
	}
	if !slices.Equal(rig.asked, []string{"LEGION-208"}) || !slices.Equal(rig.committed, []bool{true}) {
		t.Fatalf("the push asked for copies of %v, reading its own record as %v; want one copy of LEGION-208, asked after the record committed", rig.asked, rig.committed)
	}

	rig.clock.now = rig.clock.now.Add(time.Minute)
	rig.apply("push-2", intake.Push{Repo: "sjawhar/legion", Branch: "legion/LEGION-208", Before: "c1", After: "c2"})
	if at, head, _ := recordedPush(t, rig.pool, "LEGION-208"); head != "c2" || !at.Equal(rig.clock.now) {
		t.Fatalf("the later push left the record at %q at %s, want c2 at %s", head, at, rig.clock.now)
	}

	rig.apply("push-3", intake.Push{Repo: "someone/else", Branch: "legion/LEGION-208", Before: "x0", After: "x1"})
	rig.apply("push-4", intake.Push{Repo: "sjawhar/legion", Branch: "main", Before: "m0", After: "m1"})
	if _, head, _ := recordedPush(t, rig.pool, "LEGION-208"); head != "c2" {
		t.Fatalf("a push of another repository's branch moved the record to %q, want c2", head)
	}
	if !slices.Equal(rig.asked, []string{"LEGION-208", "LEGION-208"}) {
		t.Fatalf("the pushes asked for copies of %v, want two of LEGION-208 and nothing for the others", rig.asked)
	}
}

// A push the record already holds asks for no copy of the issue's workspace. The copy is asked for
// a push that moved the record, since only such a push refuses a copy the restore would otherwise
// send; a copy asked for any other would discard the copy in flight as overtaken and bundle the
// workspace again for nothing. A webhook GitHub delivers again under an event id of its own gets
// past the fact's own dedupe, as does the per-issue read's rebuild of a push whose webhook was
// applied, and both are applied later than the first; so are a late webhook of a push behind the
// newest and the branch's deletion, neither of which moves the record either. The push after them
// moves it, and asks for its copy.
func TestAPushTheRecordAlreadyHoldsAsksForNoCopy(t *testing.T) {
	rig := newPushRecordRig(t)
	push := func(id, before, after string) {
		t.Helper()
		rig.clock.now = rig.clock.now.Add(time.Minute)
		rig.apply(id, intake.Push{Repo: "sjawhar/legion", Branch: "legion/LEGION-208", Before: before, After: after})
	}
	push("push-webhook:c0..c1", "c0", "c1")
	push("push-webhook:c1..c2", "c1", "c2")
	if !slices.Equal(rig.asked, []string{"LEGION-208", "LEGION-208"}) {
		t.Fatalf("the two pushes asked for copies of %v, want one of LEGION-208 each", rig.asked)
	}
	pushed, _, _ := recordedPush(t, rig.pool, "LEGION-208")

	push("push-webhook:c1..c2-redelivered", "c1", "c2")
	push("pull-request-read:c1..c2", "c1", "c2")
	push("push-webhook:c0..c1-late", "c0", "c1")
	push("push-webhook:c2..deleted", "c2", "0000000000000000000000000000000000000000")
	if !slices.Equal(rig.asked, []string{"LEGION-208", "LEGION-208"}) {
		t.Fatalf("pushes the record already held asked for copies of %v, want none beyond the two the pushes that moved it asked for", rig.asked)
	}
	if at, head, _ := recordedPush(t, rig.pool, "LEGION-208"); head != "c2" || !at.Equal(pushed) {
		t.Fatalf("pushes the record already held moved it to %q at %s, want c2 at %s", head, at, pushed)
	}

	push("push-webhook:c2..c3", "c2", "c3")
	if !slices.Equal(rig.asked, []string{"LEGION-208", "LEGION-208", "LEGION-208"}) {
		t.Fatalf("the push of c3 asked for copies of %v, want a third of LEGION-208", rig.asked)
	}
}
