package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// A controller replacing a jump mark and a merge run spending one are two transactions, and what
// matters is the window where they overlap. The run reads the mark, calls GitHub for the queue
// the jump is asked to go ahead of, and only then clears the mark and writes its account of the
// jump; the route replaces the mark in place and takes the controller's newest word
// (api's mergeJump, record.Store.SetMergeJump).
//
// The mark that is spent is the mark the clear took, so the record of the jump and the line
// posted on the issue carry that mark's reason and that mark's session. The words of a mark
// replaced inside the window reach nobody, which is what they must do: the controller took the
// first decision back, and one merge spends one mark. Written from the mark read before the
// call, the record and the line would quote a decision the controller had already replaced, and
// the newer decision would exist nowhere at all.
//
// No concurrency is needed to drive it. The window is two committed transactions with one GitHub
// call between them, so the replacement is made from the fake's own queue read, which is inside
// that window by construction.
func TestAJumpMarkReplacedWhileTheQueueIsReadIsSpentInItsOwnWords(t *testing.T) {
	const (
		replaced = "the release waits on this merge"
		newest   = "the hotfix waits on this merge instead"
		session  = "ses_hotfix"
	)
	github := newQueueGitHub(t, "CLEAN")
	pool, records, runner, now := jumpRunner(t, github, replaced)
	github.set(func(g *armableGitHub) {
		g.queue = []queueEntry{{number: 11, position: 1, state: "MERGEABLE"}}
		g.atQueue = func() {
			markJumpAsTheRouteDoes(t, pool, records, record.MergeJump{Issue: "LEGION-208", Reason: newest,
				Session: session, SetAt: now.Add(time.Minute)})
		}
	})

	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if github.enqueued() != 1 {
		t.Fatalf("enqueues %d, want one: the issue carried a mark when the run cleared it", github.enqueued())
	}
	if mergeJumpStands(t, pool, "LEGION-208") {
		t.Fatal("the jump mark of LEGION-208 still stands, want it spent: one merge spends the one mark the issue carries")
	}
	jumped := onlyJumpRecord(t, pool, records)
	if jumped.Reason != newest {
		t.Fatalf("the jump record's reason %q, want the reason of the mark the run cleared, %q", jumped.Reason, newest)
	}
	posts := messagePosts(t, pool)
	if len(posts) != 1 {
		t.Fatalf("Dispatch messages %q, want the one line that posts the jump", posts)
	}
	for _, named := range []string{newest, session} {
		if !strings.Contains(posts[0], named) {
			t.Fatalf("the line posted on the issue %q, want %q of the mark the run cleared", posts[0], named)
		}
	}
	for _, gone := range []string{replaced, "ses_controller"} {
		if strings.Contains(posts[0], gone) {
			t.Fatalf("the line posted on the issue %q, want nothing of the replaced mark, which carried %q", posts[0], gone)
		}
	}
}

// markJumpAsTheRouteDoes writes one jump mark in a transaction of its own, as the controller's
// route writes it: an issue carries at most one mark, so this replaces whatever mark the issue
// carries (record.Store.SetMergeJump). The write is conditional on the issue still being one the
// daemon runs, and the answer is read, since a write that landed nowhere would leave the run the
// older mark to spend and prove nothing.
//
// The route itself is the api package's and cannot be served from here, so what this drives is
// the record store that route is made of. That the route replaces a mark in place rather than
// refusing the second call is pinned where the route lives
// (api's TestMergeJumpReplacesTheMarkTheIssueHolds).
//
// It reports a failure rather than ending the test, because a test gives it to the GitHub fake
// to run (armableGitHub.atQueue), where it runs on the server's goroutine and not the test's.
func markJumpAsTheRouteDoes(t *testing.T, pool *pgxpool.Pool, records record.Store, mark record.MergeJump) {
	t.Helper()
	ctx := context.Background()
	var wrote bool
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var err error
		wrote, err = records.SetMergeJump(ctx, tx, mark)
		return err
	}); err != nil {
		t.Errorf("mark %s for the jump as the controller's route does: %v", mark.Issue, err)
		return
	}
	if !wrote {
		t.Errorf("the jump mark of %s was not written, want it on an issue still in the workflow", mark.Issue)
	}
}
