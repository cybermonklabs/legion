package workflow

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A root that leaves the workflow takes its step's wait on the ancestor's request with it
// (dispatch://INFRA-3320, R3 and R8): the ancestor's architect hears that the root left, and
// nothing of the root after that. Moved to the backlog or closed, the root keeps its step closed
// with no basis when the person then approves the ancestor's version, whether that approval comes
// as its live event, followed by a pending-basis seed's read of it, or as that read alone, as the
// boot queues a seed for each step still waiting: the root's issue gets no message, its architect
// no notice, and the ancestor's architect no notice after the one that says the root left. A step
// still waiting on a root out of the workflow, as one written before a leave ended such waits,
// opens on neither, and its wait ends with nobody told.
func TestAPendingBasisOpensNothingOnARootThatLeftTheWorkflow(t *testing.T) {
	approved := dispatch.Approval{IssueKey: basisAncestor, State: "approved", LatestVersion: 4, Version: new(4), At: basisApprovedAt.Add(24 * time.Hour),
		Seq: 60, By: basisPerson}
	for _, place := range []struct {
		name  string
		leave func(t *testing.T, pool *pgxpool.Pool, engine *Engine)
		// told is how many resting-root notices the ancestor's architect holds once the root
		// stands there: the pending registration's, and the leave's where the engine applied one.
		told int
	}{
		{"moved to the backlog", func(t *testing.T, pool *pgxpool.Pool, engine *Engine) {
			closeIssue(t, pool, engine, gatedRoot, "", "backlog", 20)
		}, 2},
		{"closed", func(t *testing.T, pool *pgxpool.Pool, engine *Engine) {
			closeIssue(t, pool, engine, gatedRoot, "", "done", 20)
		}, 2},
		{"out of the workflow with its step still waiting", func(t *testing.T, pool *pgxpool.Pool, _ *Engine) {
			leave(t, pool, gatedRoot, phase.Done, "backlog")
		}, 1},
	} {
		read := seedRead(approved, ancestorAtWork)
		for _, delivery := range []struct {
			name  string
			facts []intake.Fact
		}{
			{"the approval's live event and the seed's read of it", []intake.Fact{ancestorReview(intake.DispatchArtifactApproved, 4, 60, ""), read}},
			{"the pending-basis seed's read of the approval", []intake.Fact{read}},
		} {
			t.Run(place.name+", "+delivery.name, func(t *testing.T) {
				pool, engine, apply := registeredPending(t)
				place.leave(t, pool, engine)
				messages := messageBodies(t, pool)
				if told := noticesOfKind(t, pool, basisAncestor, "resting-root"); len(told) != place.told {
					t.Fatalf("resting-root notices once the root is %s = %#v, want %d", place.name, told, place.told)
				}

				for i, fact := range delivery.facts {
					apply(fmt.Sprintf("ancestor-approved-%d", i), fact)
				}

				if gate := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]; record.GateOpen(gate) || gate.Basis != nil || gate.RestsOnIssue(basisAncestor) {
					t.Fatalf("the spec step = %#v, want it closed with no basis, resting on nothing: the root left the workflow before the approval", gate)
				}
				if got := messageBodies(t, pool); !slices.Equal(got, messages) {
					t.Fatalf("Dispatch messages = %q, want %q: nothing is posted on a root out of the workflow", got, messages)
				}
				for _, kind := range []record.NoticeKind{"design-approved", "basis-ended"} {
					if got := noticesOfKind(t, pool, gatedRoot, kind); len(got) != 0 {
						t.Fatalf("%s notices of the root = %#v, want none: its architect is told nothing of a step out of the workflow", kind, got)
					}
				}
				if told := noticesOfKind(t, pool, basisAncestor, "resting-root"); len(told) != place.told {
					t.Fatalf("resting-root notices after the approval = %#v, want the %d before it: the ancestor's architect hears nothing of the root after it left", told, place.told)
				}
				if got := outboxKindCount(t, pool, gatedRoot, record.OutboxKindGateSeed); got != 1 {
					t.Fatalf("gate seeds = %d, want the registration's alone", got)
				}
			})
		}
	}
}

// A step still waiting on an ancestor's request on a root out of the workflow, as a release that
// does not end such waits at a root's leave leaves it (a rollback, then this release again), ends
// with nobody told when the pending-basis seed that request's close queues reads it closed with no
// approval, as the person's approval opens nothing there (dispatch://INFRA-3320, R3 and R8): the
// root's tree runs nothing, so neither architect has a step to hear of. The root is moved to the
// backlog in the store, with no leave of this release, and the ancestor's close withdraws the
// request: the step stays closed, registered on the root's own document with no basis and resting
// on nothing, and beside the seed the close queues no basis-ended notice, no resting-root notice,
// no message and no other row.
func TestAPendingBasisOnARootOutOfTheWorkflowEndsWithNobodyToldWhenItsRequestCloses(t *testing.T) {
	pool, _, apply := registeredPending(t)
	leave(t, pool, gatedRoot, phase.Done, "backlog")
	rows, messages := outboxRows(t, pool), messageBodies(t, pool)
	if told := noticesOfKind(t, pool, basisAncestor, "resting-root"); len(told) != 1 {
		t.Fatalf("resting-root notices once the root is on the backlog = %#v, want the pending registration's alone", told)
	}

	cueThenRead(t, pool, apply, "request-withdrawn", intake.DispatchAskClosed{Key: basisAncestor, Ask: "ask-100", Kind: record.AskApproval,
		Artifact: basisDoc, State: "resolved", Actor: basisPerson, Mark: askSeq(61)},
		closedBy(basisPerson, intake.AncestorRead{Status: record.DoneStatus, HandedOver: true}))

	if gate := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]; gate.PendingOnBasis() || gate.Basis != nil || record.GateOpen(gate) ||
		gate.ArtifactID != gateDoc || gate.LatestVersion != 4 || gate.RestsOnIssue(basisAncestor) {
		t.Fatalf("the spec step = %#v, want its wait ended: closed at version 4 of the root's own document, with no basis and resting on nothing", gate)
	}
	if got := noticesOfKind(t, pool, gatedRoot, "basis-ended"); len(got) != 0 {
		t.Fatalf("basis-ended notices of the root = %#v, want none: its architect is told nothing of a step out of the workflow", got)
	}
	if told := noticesOfKind(t, pool, basisAncestor, "resting-root"); len(told) != 1 {
		t.Fatalf("resting-root notices after the close = %#v, want the pending registration's alone: the ancestor's architect hears nothing of a root out of the workflow", told)
	}
	if got := messageBodies(t, pool); !slices.Equal(got, messages) {
		t.Fatalf("Dispatch messages after the close = %q, want %q: nothing is posted on a root out of the workflow", got, messages)
	}
	if got := outboxRows(t, pool); got != rows+1 {
		t.Fatalf("outbox rows after the close = %d, want the %d before it and the close's pending-basis seed: the end of the wait queues nothing", got, rows)
	}
}

// A registration that carries a basis on a root that has left the workflow takes no basis
// (dispatch://INFRA-3320, R3 and R8): the root's architect can still be in the turn the leave
// suspends, and its register_gate on an ancestor's approval, pending or standing, then lands after
// the leave. The step is written as a registration with no basis writes it: registered on the
// root's own document, closed, with no basis and resting on nothing. Nobody is told anything of a
// basis: the ancestor's architect gets no resting-root notice, the root's issue no message, the
// root's architect no design-approved notice, and no pending-basis seed is queued.
func TestARegistrationOnABasisOfARootThatLeftTheWorkflowTakesNoBasis(t *testing.T) {
	pool := migratedPool(t)
	engine := testEngine(config.DesignGateRootIssues, nil)
	apply := applyFacts(t, pool, engine)
	seedGatedTree(t, pool)
	seedLiveAncestor(t, pool)
	closeIssue(t, pool, engine, gatedRoot, "", "backlog", 20)
	messages := messageBodies(t, pool)

	for _, registration := range []struct {
		name  string
		id    string
		basis *record.GateBasis
		since int64
	}{
		{"a pending basis", "register-pending", pendingBasis(), 21},
		{"an approved basis", "register-on-basis", approvedBasis(), 22},
	} {
		if result := apply(registration.id, onBasis(registration.basis, registration.since)); result.Refusal != nil {
			t.Fatalf("the registration on %s = %+v, want it written with no basis", registration.name, result.Refusal)
		}
		gate := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]
		if gate.ArtifactID != gateDoc || gate.LatestVersion != 4 || gate.Basis != nil || record.GateOpen(gate) || gate.RestsOnIssue(basisAncestor) {
			t.Fatalf("the spec step after the registration on %s = %#v, want it registered at version 4 of the root's own document, closed, with no basis and resting on nothing",
				registration.name, gate)
		}
		if got := noticesOfKind(t, pool, basisAncestor, "resting-root"); len(got) != 0 {
			t.Fatalf("resting-root notices after the registration on %s = %#v, want none: the ancestor's architect is told nothing of a root out of the workflow", registration.name, got)
		}
		if got := messageBodies(t, pool); !slices.Equal(got, messages) {
			t.Fatalf("Dispatch messages after the registration on %s = %q, want %q: nothing is posted on a root out of the workflow", registration.name, got, messages)
		}
		if got := noticesOfKind(t, pool, gatedRoot, "design-approved"); len(got) != 0 {
			t.Fatalf("design-approved notices after the registration on %s = %#v, want none", registration.name, got)
		}
		if seeds := pendingSeeds(t, pool, gatedRoot); len(seeds) != 0 {
			t.Fatalf("pending-basis seeds after the registration on %s = %#v, want none", registration.name, seeds)
		}
	}
}
