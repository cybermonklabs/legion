package workflow

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A retro of the ancestor ends a step waiting on its request at once only while the daemon's copy
// of Dispatch shows the ancestor accepted (endPendingOnLeave). A retro delivered late or replayed,
// after the copy took a newer reading back in the workflow, ends nothing and leaves the wait to the
// pending-basis seed, though the daemon's record of the ancestor still holds the retro it took:
// the record's Dispatch status can be older than the copy's (dispatch://INFRA-3904; at 1ecb4444 the
// end read the record too, through record.BasisAcceptance, and ended the wait on it). A session's
// retro on the ancestor, which the daemon may set back, ends nothing either. The fresh retro, with
// the copy showing it, ends the wait with the act the seed's verdict names.
func TestARetroOfTheAncestorEndsAWaitOnlyWhileTheCopyShowsIt(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	engine := testEngine(config.DesignGateRootIssues, nil)
	apply := applyFacts(t, pool, engine)
	seedGatedTree(t, pool)
	seedLiveAncestor(t, pool)
	apply("register-pending", onBasis(pendingBasis(), 6))
	seeds := len(pendingSeeds(t, pool, gatedRoot))

	// The record took the person's retro at seq 10 and the copy then took a reading at seq 11
	// that shows the ancestor back in_progress, as a boot's listing does.
	ancestor := issueRecord(t, pool, basisAncestor)
	ancestor.DispatchStatus, ancestor.LastDispatchSeq = "retro", 10
	seedIssue(t, pool, *ancestor)
	copyReading(t, pool, basisAncestor, "in_progress", 11)
	retro := func(seq int64, session string) intake.DispatchIssue {
		return intake.DispatchIssue{Key: basisAncestor, Type: "issue.updated", Seq: seq, Status: "retro",
			Title: "ancestor", Rank: "T", HandedOver: true, ResolvedRepo: "sjawhar/legion", ActorSession: session}
	}

	apply("late-retro", retro(10, ""))
	if gate := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]; !gate.PendingOnBasis() {
		t.Fatalf("a retro at seq 10, delivered after the copy took seq 11 showing in_progress, left the spec step %#v; want it still waiting on ask-100", gate)
	}
	if got := len(pendingSeeds(t, pool, gatedRoot)); got != seeds+1 {
		t.Fatalf("pending-basis seeds after the late retro = %d, want %d: the event is a cue, and the seed's read of Dispatch judges it", got, seeds+1)
	}

	// A session's retro, with the copy showing it, ends nothing at once either.
	copyReading(t, pool, basisAncestor, "retro", 12)
	apply("session-retro", retro(12, "session-outside"))
	if gate := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]; !gate.PendingOnBasis() {
		t.Fatalf("a session's retro at seq 12 left the spec step %#v; want it still waiting: the daemon may set that write back", gate)
	}

	// The person's fresh retro, with the copy showing it, ends the wait.
	copyReading(t, pool, basisAncestor, "retro", 13)
	apply("fresh-retro", retro(13, ""))
	if gate := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]; gate.PendingOnBasis() {
		t.Fatalf("a person's retro at seq 13, with the copy showing it, left the spec step %#v waiting; want the wait ended", gate)
	}
	const act = "Dispatch shows LEGION-100 retro, so LEGION-100 is accepted."
	if ended := noticesOfKind(t, pool, gatedRoot, "basis-ended"); len(ended) != 1 || !strings.Contains(ended[0].Reason, act) {
		t.Fatalf("basis-ended notices = %#v, want one naming %q", ended, act)
	}
}

// copyReading writes the daemon's copy of Dispatch for key as one reading at seq showing status,
// as the copy's own handler writes it for an event of the issue.
func copyReading(t *testing.T, pool *pgxpool.Pool, key, status string, seq int64) {
	t.Helper()
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return record.NewStore().PutDispatchIssue(t.Context(), tx, record.DispatchReading{Key: key, Project: "LEGION", Seq: seq,
			Status: record.Carry(status), Applied: true})
	})
}
