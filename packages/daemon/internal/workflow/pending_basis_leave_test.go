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
// pending-basis seed, though the daemon's record of the ancestor still holds the retro it took: the
// record's Dispatch status can be older than the copy's. The seed it queues ends nothing either,
// since the copy decides the seed's acceptance too (record.SeedAcceptance; at 1ecb4444 the event's
// end read the record and ended the wait, and at b984d3dc the seed did, one outbox row later). A
// session's retro on the ancestor, which the daemon sets back, ends nothing at once, and the seed
// takes no status the daemon queued a re-assert over as acceptance (record.ReassertedStatuses). The
// person's fresh retro, with the copy showing it, ends the wait with the act the seed's verdict
// names. dispatch://INFRA-3904.
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
	// The seed the late retro queued reads Dispatch showing the person's undo, in_progress.
	apply("late-retro-seed", seedRead(requestOpen(), ancestorAtWork))
	if gate := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]; !gate.PendingOnBasis() {
		t.Fatalf("the seed of the late retro, with the copy at seq 11 showing in_progress and the record still holding retro, left the spec step %#v; want it still waiting", gate)
	}

	// A session's retro, with the copy showing it, ends nothing at once either.
	copyReading(t, pool, basisAncestor, "retro", 12)
	apply("session-retro", retro(12, "session-outside"))
	if gate := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]; !gate.PendingOnBasis() {
		t.Fatalf("a session's retro at seq 12 left the spec step %#v; want it still waiting: the daemon may set that write back", gate)
	}
	// The daemon set the session's write back, and the seed reads Dispatch before that write lands,
	// still showing the session's retro.
	var over map[string]bool
	seedRecord(t, pool, func(tx pgx.Tx) error {
		var err error
		over, err = record.ReassertedStatuses(t.Context(), record.NewStore(), tx, "LEGION", basisAncestor)
		return err
	})
	if !over["retro"] {
		t.Fatalf("the re-asserts queued over %s's statuses after the session's retro = %v, want one over retro: the daemon sets that write back", basisAncestor, over)
	}
	apply("session-retro-seed", seedRead(requestOpen(), intake.AncestorRead{Status: "retro", HandedOver: true}))
	if gate := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]; !gate.PendingOnBasis() {
		t.Fatalf("the seed after the session's retro, reading Dispatch still showing retro with the daemon's re-assert queued, left the spec step %#v; want it still waiting", gate)
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
