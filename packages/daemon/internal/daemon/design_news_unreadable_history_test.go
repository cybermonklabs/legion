package daemon

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A review history Dispatch could not read is a failure of the read, never a finding about the
// document (dispatch://INFRA-175, the INFRA-138 architect's decision on implement handoff v1's
// second open question). So the reconcile that gives up on it after its bounded attempts tells the
// holder that the history could not be read, with the reason, and never that the version is a first
// approval; and it leaves nothing that a later build reads as "no earlier approval". Once Dispatch
// answers again, the next build of the issue's news reads the history again and carries what it
// says: the true earlier version, or, for a document with no earlier approval, the first approval,
// from version 0, in those words.
//
// The first build is the reviewer's phase start after the bounded attempts; the reviewer never
// takes that task, so the issue's told row never rises, and the issue moves on to merging. The
// second build is the merger's phase start once Dispatch answers again, after the reconcile of the
// tree's news has run once more.
func TestAnUnreadableReviewHistoryIsReadAgainOnceDispatchAnswers(t *testing.T) {
	for _, tc := range []struct {
		name string
		// before is the version Dispatch's review history shows approved before version 5, once it
		// answers.
		before int
		// says is what the second build must say of that version.
		says string
	}{
		{name: "an earlier approval", before: 3, says: "version 5, since version 3, which your phase last took"},
		{name: "no earlier approval", before: 0, says: "version 5, the first version a person approved; your phase took none before it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newNewsReconcileRig(t)
			root := r.member(reconcileRoot, "", phase.Reviewing)
			r.told(reconcileRoot, record.AudienceHolder, -1)
			r.holder(reconcileRoot, claim.RoleReviewer, supervise.StateQueued, "", 0)
			answers := false
			r.client.before = func(artifact string, version int) (int, error) {
				if !answers {
					return 0, errors.New("dispatch: 502 BAD_GATEWAY")
				}
				if artifact != reconcileDoc || version != 5 {
					return 0, errors.New("asked the history of another document or version")
				}
				return tc.before, nil
			}

			for attempts := range designChangesAttempts {
				err := r.reconcile(attempts)
				if attempts < designChangesAttempts-1 && err == nil {
					t.Fatalf("attempt %d under a failing review history finished, want the failure returned for the outbox to retry", attempts+1)
				}
				if attempts == designChangesAttempts-1 && err != nil {
					t.Fatalf("the last attempt under a failing review history = %v, want the row finished", err)
				}
			}

			// The first build: the reviewer's phase start, after the bounded attempts.
			const review = "Review the change. Issue: LEGION-600. Phase: reviewing."
			if err := r.runner.execute(r.ctx, mustOutboxRow(t, reconcileRoot, record.SuperviseRequest{Op: "start", Tree: reconcileRoot,
				Role: claim.RoleReviewer, Generation: 1, Phase: phase.Reviewing, Entry: 1, Task: review}, time.Now())); err != nil {
				t.Fatalf("run the reviewer's start: %v", err)
			}
			reviewer, found := r.sup.Machine(mustClaimToken(t, reconcileRoot, claim.RoleReviewer))
			if !found {
				t.Fatal("the reviewer's start created no claim")
			}
			first, _ := pendingNews(t, reviewer)
			if !(strings.Contains(first, "could not be read") || strings.Contains(first, "unreadable")) || !strings.Contains(first, "502 BAD_GATEWAY") {
				t.Fatalf("the first build = %q, want it to say the review history could not be read, and why", first)
			}
			if strings.Contains(first, "the first version a person approved") {
				t.Fatalf("the first build = %q, want no first approval: the history was not read", first)
			}

			// Dispatch answers again. The reviewer never took its task, so the issue's told row did not
			// rise, and the issue moves on to merging.
			answers = true
			root.Phase = phase.Merging
			putOutboxIssue(t, r.pool, r.records, root)
			r.holder(reconcileRoot, claim.RoleMerger, supervise.StateQueued, "", 0)
			if err := r.reconcile(0); err != nil {
				t.Fatalf("reconcile the tree's news once Dispatch answers: %v", err)
			}

			// The second build: the merger's phase start.
			const merge = "Merge it."
			if err := r.runner.execute(r.ctx, mustOutboxRow(t, reconcileRoot, record.SuperviseRequest{Op: "start", Tree: reconcileRoot,
				Role: claim.RoleMerger, Generation: 1, Phase: phase.Merging, Entry: 1, Task: merge}, time.Now())); err != nil {
				t.Fatalf("run the merger's start: %v", err)
			}
			merger, found := r.sup.Machine(mustClaimToken(t, reconcileRoot, claim.RoleMerger))
			if !found {
				t.Fatal("the merger's start created no claim")
			}
			second, news := pendingNews(t, merger)
			if !strings.Contains(second, tc.says) {
				t.Fatalf("the second build = %q, want it to say %q: Dispatch answered, so the news carries what its history says", second, tc.says)
			}
			if strings.Contains(second, "could not be read") || strings.Contains(second, "unreadable") || strings.Contains(second, "502 BAD_GATEWAY") {
				t.Fatalf("the second build = %q, want no word of the failed read: Dispatch answered", second)
			}
			if len(news.Items) != 1 || news.Items[0].From == nil || *news.Items[0].From != tc.before {
				t.Fatalf("the second build carries %+v, want the spec item from version %d", news.Items, tc.before)
			}
			if len(r.client.history) <= designChangesAttempts {
				t.Fatalf("Dispatch was asked the review history %d times, want more than the %d failed reads: once it answers, the next build reads it again",
					len(r.client.history), designChangesAttempts)
			}
		})
	}
}
