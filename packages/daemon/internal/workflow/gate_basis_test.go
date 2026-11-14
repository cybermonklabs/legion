package workflow

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The ancestor these tests rest LEGION-208's spec step on (dispatch://INFRA-3320, R1), its primary
// document, and Dispatch's time of the person's approval of its version 3.
const (
	basisAncestor = "LEGION-100"
	basisDoc      = "artifact-100"
)

var basisApprovedAt = time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)

// basisEngineNow is the clock of testEngine: the daemon's own time of every registration here.
var basisEngineNow = time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)

// approvedBasis is a person's approval of version 3 of LEGION-100's primary document, as
// register_gate records it: no registration time, which the engine stamps.
func approvedBasis() *record.GateBasis {
	return &record.GateBasis{Issue: basisAncestor, ArtifactID: basisDoc, Version: 3, Approval: record.EventMark{Seq: 40, At: basisApprovedAt},
		Approver: "user:alice", Serves: []string{"dispatch://LEGION-100/spec#b-r1@v3", "dispatch://LEGION-100/spec#b-r2@v3"},
		Reason: "LEGION-208 delivers r1 and part of r2 of LEGION-100.", Session: "session-architect"}
}

// pendingBasis is LEGION-100's version 4 with its approval request ask-100 open on the person.
func pendingBasis() *record.GateBasis {
	basis := approvedBasis()
	basis.Version, basis.Approval, basis.Approver = 4, record.EventMark{}, ""
	basis.Serves = []string{"dispatch://LEGION-100/spec#b-r1@v4"}
	basis.Pending = &record.PendingBasis{Ask: "ask-100"}
	return basis
}

// onBasis is the registration of version 4 of LEGION-208's primary document on basis, whose read
// began at since and found the root's own approval request ask-spec open on the person.
func onBasis(basis *record.GateBasis, since int64) intake.GateRegistered {
	return intake.GateRegistered{Issue: gatedRoot, ArtifactID: gateDoc, Version: 4, DispatchVersion: 4, Since: record.EventMark{Seq: since},
		RequestAsk: "ask-spec", Basis: basis, ViewOf: basisAncestor}
}

// seedLiveAncestor records LEGION-100 as the root of a tree of its own that runs, so a notice keyed
// by it reaches its architect.
func seedLiveAncestor(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	seedIssue(t, pool, record.Issue{Key: basisAncestor, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: basisAncestor, Project: "LEGION", Title: "ancestor", Phase: phase.Planning, Generation: 1, Status: "in_progress", Rank: "T",
		HandedOver: true, LastDispatchSeq: 1})
	seedTreeLifecycle(t, pool, basisAncestor)
}

// pendingSeeds is every pending-basis seed the outbox holds for issue, in order.
func pendingSeeds(t *testing.T, pool *pgxpool.Pool, issue string) []record.PendingBasisSeed {
	t.Helper()
	rows, err := pool.Query(t.Context(), `select payload from outbox where kind = $1 and issue = $2 order by id`,
		string(record.OutboxKindPendingBasisSeed), issue)
	if err != nil {
		t.Fatalf("read the pending-basis seeds of %s: %v", issue, err)
	}
	defer rows.Close()
	var seeds []record.PendingBasisSeed
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			t.Fatalf("scan a pending-basis seed of %s: %v", issue, err)
		}
		var seed record.PendingBasisSeed
		if err := json.Unmarshal(payload, &seed); err != nil {
			t.Fatalf("decode a pending-basis seed of %s: %v", issue, err)
		}
		seeds = append(seeds, seed)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the pending-basis seeds of %s: %v", issue, err)
	}
	return seeds
}

// gateSeedCount is how many gate seeds the outbox holds for issue.
func gateSeedCount(t *testing.T, pool *pgxpool.Pool, issue string) int {
	t.Helper()
	var got int
	if err := pool.QueryRow(t.Context(), `select count(*) from outbox where kind = $1 and issue = $2`,
		string(record.OutboxKindGateSeed), issue).Scan(&got); err != nil {
		t.Fatalf("count the gate seeds of %s: %v", issue, err)
	}
	return got
}

// The message a registration on an approved basis posts on the root (R2): its first line, by which
// a reader counts such messages, then one fact to a line, and the architect's reason quoted.
const basisOpenedBody = "Design gate opened on an ancestor's approval.\n" +
	"\n" +
	"- The step is spec.\n" +
	"- The primary document of LEGION-208 for this step is dispatch://LEGION-208/spec (artifact-208).\n" +
	"- The architect of LEGION-208 registered version 4 of this document.\n" +
	"- No person approved version 4 of this document.\n" +
	"- The ancestor is LEGION-100.\n" +
	"- The primary document of LEGION-100 for this step is dispatch://LEGION-100/spec (artifact-100).\n" +
	"- A person approved version 3 of this document.\n" +
	"- The person is user:alice.\n" +
	"- Dispatch recorded this approval at 2026-09-22T10:00:00Z.\n" +
	"- The checks of LEGION-208 serve these requirements of LEGION-100:\n" +
	"  - dispatch://LEGION-100/spec#b-r1@v3\n" +
	"  - dispatch://LEGION-100/spec#b-r2@v3\n" +
	"- The architect session session-architect registered this step.\n" +
	"\n" +
	"The reason of the architect:\n" +
	"\n" +
	"> LEGION-208 delivers r1 and part of r2 of LEGION-100."

// A registration on a person's approval of the ancestor's latest version opens the root's step at
// the version registered, as a person's approval of that version would, and records the basis:
// the row stands open on it, rests on the ancestor, and gives the waiting line the daemon's own time
// of the registration. The root's issue gets one message that names the approval, its architect a
// design-approved notice that names the ancestor's document and version as the basis, the
// ancestor's architect one resting-root notice, and the gate seed is queued. The view fact is
// written, and admission's pass starts the tree. A second registration of the same reads tells
// nobody anything again.
func TestARegistrationOnAnApprovedBasisOpensTheStepAndRecordsIt(t *testing.T) {
	pool := migratedPool(t)
	engine := testEngine(config.DesignGateRootIssues, nil)
	apply := applyFacts(t, pool, engine)
	seedGatedTree(t, pool)
	seedLiveAncestor(t, pool)

	apply("register-on-basis", onBasis(approvedBasis(), 6))

	gates := gatesOf(t, pool, gatedRoot)
	gate := gates[record.DesignStepSpec]
	if !gate.OnBasis() || !record.GateOpen(gate) || *gate.ApprovedVersion != 4 || gate.ApprovedMark != (record.EventMark{}) ||
		gate.KeptPerson != nil || gate.RestsOn != basisAncestor {
		t.Fatalf("the spec step = %#v, want it open at version 4 on the basis, resting on %s, with no person's approval", gate, basisAncestor)
	}
	basis := gate.Basis
	if basis.Issue != basisAncestor || basis.ArtifactID != basisDoc || basis.Version != 3 || basis.Approver != "user:alice" ||
		basis.Approval.Seq != 40 || !basis.Approval.At.Equal(basisApprovedAt) || !basis.RegisteredAt.Equal(basisEngineNow) ||
		basis.Session != "session-architect" || len(basis.Serves) != 2 {
		t.Fatalf("the basis = %#v, want the ancestor's approval as registered, stamped with the engine's time", basis)
	}
	if opened := (record.TreeGate{Steps: engine.cfg.Steps, Rows: gates}).OpenedAt(); !opened.Equal(basisEngineNow) {
		t.Fatalf("the tree's gate opened at %s, want the registration's own time %s", opened, basisEngineNow)
	}

	if bodies := messageBodies(t, pool); len(bodies) != 1 || bodies[0] != basisOpenedBody {
		t.Fatalf("Dispatch messages = %q, want the one that records the basis:\n%s", bodies, basisOpenedBody)
	}
	approved := noticesOfKind(t, pool, gatedRoot, "design-approved")
	if len(approved) != 1 || approved[0].Ref != "dispatch://LEGION-100/spec" || approved[0].Version != 3 || approved[0].Actor != "user:alice" ||
		!strings.Contains(approved[0].Reason, "No person approved that version") || !strings.Contains(approved[0].Reason, "version 4") {
		t.Fatalf("design-approved notices = %#v, want one naming LEGION-100's document at version 3 as the basis", approved)
	}
	resting := noticesOfKind(t, pool, basisAncestor, "resting-root")
	if len(resting) != 1 || resting[0].Ref != "dispatch://LEGION-100/spec" || resting[0].Version != 3 {
		t.Fatalf("resting-root notices = %#v, want one keyed by %s naming its document at version 3", resting, basisAncestor)
	}
	for _, part := range []string{gatedRoot, "spec step", "dispatch://LEGION-100/spec#b-r1@v3", "dispatch://LEGION-100/spec#b-r2@v3"} {
		if !strings.Contains(resting[0].Reason, part) {
			t.Fatalf("the resting-root reason %q names no %q", resting[0].Reason, part)
		}
	}
	if got := gateSeedCount(t, pool, gatedRoot); got != 1 {
		t.Fatalf("gate seeds = %d, want the registration's one", got)
	}
	if got := recordedRoot(t, pool); got.ViewOf != basisAncestor || got.DeclaredSeq != 0 {
		t.Fatalf("the root's view fact = %q and declared hand-back %d, want %s and none", got.ViewOf, got.DeclaredSeq, basisAncestor)
	}

	admissionPass(t, pool, engine)
	if got := phaseOf(t, pool, gatedRoot); got != phase.Planning {
		t.Fatalf("the root = %s after admission's pass, want planning: its step stands open", got)
	}

	apply("register-on-basis-again", onBasis(approvedBasis(), 7))
	if bodies := messageBodies(t, pool); len(bodies) != 1 {
		t.Fatalf("Dispatch messages after the same registration again = %d, want the first alone", len(bodies))
	}
	if got := noticesOfKind(t, pool, gatedRoot, "design-approved"); len(got) != 1 {
		t.Fatalf("design-approved notices after the same registration again = %#v, want the first alone", got)
	}
	if got := noticesOfKind(t, pool, basisAncestor, "resting-root"); len(got) != 1 {
		t.Fatalf("resting-root notices after the same registration again = %#v, want the first alone: the first rest is told once", got)
	}
}

// A registration on the ancestor's open approval request leaves the step closed and pending on that
// request, declares no hand-back although the root's own request is open on the person, and keeps
// the root at admitted in its slot. It queues the pending-basis seed and the resting-root notice of
// the pending registration, and tells the root's architect nothing and posts nothing.
func TestARegistrationOnAPendingBasisLeavesTheStepClosed(t *testing.T) {
	pool := migratedPool(t)
	engine := testEngine(config.DesignGateRootIssues, nil)
	apply := applyFacts(t, pool, engine)
	seedGatedTree(t, pool)
	seedLiveAncestor(t, pool)

	apply("register-pending", onBasis(pendingBasis(), 6))

	gate := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]
	if !gate.PendingOnBasis() || record.GateOpen(gate) || gate.Basis.Pending.Ask != "ask-100" || gate.Basis.Version != 4 ||
		!gate.Basis.RegisteredAt.Equal(basisEngineNow) || gate.RestsOn != "" || !gate.RestsOnIssue(basisAncestor) {
		t.Fatalf("the spec step = %#v, want it closed, pending on ask-100 of version 4 and resting on %s through it", gate, basisAncestor)
	}
	root := recordedRoot(t, pool)
	if root.Phase != phase.Admitted || root.DeclaredSeq != 0 || slotCount(t, pool, gatedRoot) != 1 {
		t.Fatalf("the root = %s with declared hand-back %d and %d slots, want it admitted in its slot with nothing handed back",
			root.Phase, root.DeclaredSeq, slotCount(t, pool, gatedRoot))
	}
	if entered := reconcileAwaiting(t, pool, engine); len(entered) != 0 || recordedRoot(t, pool).Phase != phase.Admitted {
		t.Fatalf("admission's pass moved %v into a wait, want the root left at admitted", entered)
	}
	seeds := pendingSeeds(t, pool, gatedRoot)
	want := record.PendingBasisSeed{Step: record.DesignStepSpec, ArtifactID: gateDoc, BasisArtifactID: basisDoc, BasisVersion: 4, Ask: "ask-100", Generation: 1}
	if len(seeds) != 1 || seeds[0] != want {
		t.Fatalf("pending-basis seeds = %#v, want %#v", seeds, want)
	}
	resting := noticesOfKind(t, pool, basisAncestor, "resting-root")
	if len(resting) != 1 || resting[0].Version != 4 || !strings.Contains(resting[0].Reason, "ask-100") ||
		!strings.Contains(resting[0].Reason, "dispatch://LEGION-100/spec#b-r1@v4") {
		t.Fatalf("resting-root notices = %#v, want one of the pending registration naming ask-100 and the requirement served", resting)
	}
	if got := noticesOfKind(t, pool, gatedRoot, "design-approved"); len(got) != 0 {
		t.Fatalf("design-approved notices = %#v, want none for a step still closed", got)
	}
	if bodies := messageBodies(t, pool); len(bodies) != 0 {
		t.Fatalf("Dispatch messages = %q, want none for a step still closed", bodies)
	}
}

// A root waiting on its design gate is answered by a registration that opens the step on an
// approved basis, as a person's approval of the version registered answers it, and its architect
// gets no design-approved notice beside its resume; a pending registration keeps the wait. The
// architect's own resolve of its approval request after the registration then answers the wait as
// the registration left the step: an opened step's answer stands, and a pending step's wait is
// answered by the close, since nothing else of it is with the person.
func TestARegistrationOnABasisAnswersAWaitingRootAsItLeavesTheStep(t *testing.T) {
	for _, tc := range []struct {
		name     string
		basis    *record.GateBasis
		answered record.Answer
		resolved record.Answer
	}{
		{"an approved basis", approvedBasis(), record.AnsweredByApproval, record.AnsweredByApproval},
		{"a pending basis", pendingBasis(), "", record.AnsweredByClosed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := testEngine(config.DesignGateRootIssues, nil)
			apply := applyFacts(t, pool, engine)
			seedIssue(t, pool, record.Issue{Key: gatedRoot, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
				Tree: gatedRoot, Project: "LEGION", Title: "root", Phase: phase.AwaitingPerson, Generation: 1, Status: "in_progress", Rank: "U",
				HandedOver: true, LastDispatchSeq: 1, Awaiting: &record.Awaiting{Artifact: gateDoc, GateVersion: 4, Since: record.EventMark{Seq: 5}}})
			seedSlot(t, pool, record.Slot{Issue: gatedRoot, Index: 0, AdmittedAt: basisEngineNow})
			seedArchitectClaim(t, pool, gatedRoot)
			seedGate(t, pool, record.DesignGate{Issue: gatedRoot, ArtifactID: gateDoc, LatestVersion: 4})
			seedLiveAncestor(t, pool)

			apply("register", onBasis(tc.basis, 6))
			root := recordedRoot(t, pool)
			if root.Phase != phase.AwaitingPerson || root.Awaiting == nil || root.Awaiting.AnsweredBy != tc.answered ||
				(tc.answered == "") != (root.Awaiting.AnsweredAt == nil) {
				t.Fatalf("the waiting root after the registration = %s %#v, want its wait answered by %q", root.Phase, root.Awaiting, tc.answered)
			}
			if got := noticesOfKind(t, pool, gatedRoot, "design-approved"); len(got) != 0 {
				t.Fatalf("design-approved notices = %#v, want none: a waiting root is told by its resume", got)
			}

			apply("resolve", intake.DispatchAskClosed{Key: gatedRoot, Ask: "ask-spec", Kind: record.AskApproval, Artifact: gateDoc, State: "resolved",
				Actor: dispatch.Actor{Kind: "session", ID: "session-architect"}, Mark: askSeq(8)})
			root = recordedRoot(t, pool)
			if root.Awaiting == nil || root.Awaiting.AnsweredAt == nil || root.Awaiting.AnsweredBy != tc.resolved {
				t.Fatalf("the waiting root after its architect resolved its request = %#v, want its wait answered by %q", root.Awaiting, tc.resolved)
			}
		})
	}
}

// A resting-root notice is queued only where the daemon records the ancestor in a live tree: an
// ancestor it does not record, and one parked out of the workflow, get no row, while the root's own
// opening is told as ever. Nothing is recorded as told then, so a registration once the ancestor's
// tree runs tells its architect.
func TestABasisOnAnAncestorInNoLiveTreeQueuesNoRestingRoot(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed func(t *testing.T, pool *pgxpool.Pool)
	}{
		{"an ancestor the daemon does not record", func(*testing.T, *pgxpool.Pool) {}},
		{"an ancestor parked on the backlog", func(t *testing.T, pool *pgxpool.Pool) {
			seedIssue(t, pool, record.Issue{Key: basisAncestor, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
				Tree: basisAncestor, Project: "LEGION", Title: "ancestor", Phase: phase.Admitted, Generation: 1, Status: "backlog", Rank: "T",
				HandedOver: true, LastDispatchSeq: 1})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := testEngine(config.DesignGateRootIssues, nil)
			apply := applyFacts(t, pool, engine)
			seedGatedTree(t, pool)
			tc.seed(t, pool)

			apply("register-on-basis", onBasis(approvedBasis(), 6))
			if got := noticesOfKind(t, pool, basisAncestor, "resting-root"); len(got) != 0 {
				t.Fatalf("resting-root notices = %#v, want none for an ancestor in no live tree", got)
			}
			if got := noticesOfKind(t, pool, gatedRoot, "design-approved"); len(got) != 1 {
				t.Fatalf("design-approved notices = %#v, want the root's architect told its opening", got)
			}

			seedLiveAncestor(t, pool)
			apply("register-on-basis-again", onBasis(approvedBasis(), 7))
			if got := noticesOfKind(t, pool, basisAncestor, "resting-root"); len(got) != 1 {
				t.Fatalf("resting-root notices once the ancestor's tree runs = %#v, want one", got)
			}
		})
	}
}

// A registration with no basis after one on a basis at the same version waits for a person's
// approval, as a registration did before the basis (R3): an opened step closes and holds no basis,
// and still rests on the ancestor for the generation (R8); a pending step stops waiting and rests on
// nothing, and the ancestor's architect is told that wait ended.
func TestARegistrationWithNoBasisAfterABasisLeavesNoneBehind(t *testing.T) {
	for _, tc := range []struct {
		name    string
		basis   *record.GateBasis
		restsOn string
		resting int
	}{
		{"after an approved basis", approvedBasis(), basisAncestor, 1},
		{"after a pending basis", pendingBasis(), "", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := testEngine(config.DesignGateRootIssues, nil)
			apply := applyFacts(t, pool, engine)
			seedGatedTree(t, pool)
			seedLiveAncestor(t, pool)

			apply("register-on-basis", onBasis(tc.basis, 6))
			again := onBasis(nil, 7)
			again.ViewOf = ""
			apply("register-with-no-basis", again)

			gate := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]
			if gate.Basis != nil || gate.KeptPerson != nil || record.GateOpen(gate) || gate.ApprovedVersion != nil || gate.LatestVersion != 4 ||
				gate.RestsOn != tc.restsOn || gate.RestsOnIssue(basisAncestor) != (tc.restsOn != "") {
				t.Fatalf("the spec step = %#v, want it closed at version 4 with no basis, resting on %q", gate, tc.restsOn)
			}
			resting := noticesOfKind(t, pool, basisAncestor, "resting-root")
			if len(resting) != tc.resting {
				t.Fatalf("resting-root notices = %#v, want %d", resting, tc.resting)
			}
			if tc.resting == 2 && !strings.Contains(resting[1].Reason, "again with no basis") {
				t.Fatalf("the second resting-root reason = %q, want it to say the wait ended", resting[1].Reason)
			}
			if got := recordedRoot(t, pool).ViewOf; got != "" {
				t.Fatalf("the root's view fact = %q, want the last registration's, none", got)
			}
		})
	}
}

// A person's approval of an earlier version of the root's own document is the approval the step
// keeps while a basis stands, and the one a later registration carries: never the version the
// basis opened the step at, which no person approved.
func TestARegistrationCarriesThePersonsApprovalNotTheBasisVersion(t *testing.T) {
	pool := migratedPool(t)
	engine := testEngine(config.DesignGateRootIssues, nil)
	apply := applyFacts(t, pool, engine)
	seedGatedTree(t, pool)
	seedLiveAncestor(t, pool)
	personal := record.EventMark{Seq: 3, At: basisApprovedAt.Add(-time.Hour)}
	seedGate(t, pool, record.DesignGate{Issue: gatedRoot, ArtifactID: gateDoc, LatestVersion: 4, ApprovedVersion: new(2), ApprovedMark: personal})

	apply("register-on-basis", onBasis(approvedBasis(), 6))
	gate := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]
	if !gate.OnBasis() || *gate.ApprovedVersion != 4 || gate.KeptPerson == nil || gate.KeptPerson.Version != 2 || gate.KeptPerson.Mark.Seq != 3 {
		t.Fatalf("the spec step = %#v, want it open at 4 on the basis, keeping the person's approval of version 2", gate)
	}
	if approval := gate.ApprovedByPerson(); approval == nil || approval.Version != 2 {
		t.Fatalf("the person's approval the step records = %#v, want version 2", approval)
	}

	apply("register-v5", intake.GateRegistered{Issue: gatedRoot, ArtifactID: gateDoc, Version: 5, DispatchVersion: 5, Since: record.EventMark{Seq: 8}})
	gate = gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]
	if gate.Basis != nil || gate.ApprovedVersion == nil || *gate.ApprovedVersion != 2 || gate.ApprovedMark.Seq != 3 || record.GateOpen(gate) {
		t.Fatalf("the spec step after registering version 5 = %#v, want it closed, carrying the person's approval of version 2", gate)
	}
}

// Each registration of the spec step writes the view fact it carries, the newest one standing,
// and a registration of another step leaves it as it is.
func TestEachRegistrationOfTheSpecStepWritesItsViewFact(t *testing.T) {
	pool := migratedPool(t)
	engine := stepsEngine(twoSteps(), nil)
	apply := applyFacts(t, pool, engine)
	seedGatedTree(t, pool)
	bindStep(t, pool, record.DesignStepCriteria, criteriaDoc)

	apply("register-spec", intake.GateRegistered{Issue: gatedRoot, ArtifactID: gateDoc, Version: 4, ViewOf: basisAncestor})
	if got := recordedRoot(t, pool).ViewOf; got != basisAncestor {
		t.Fatalf("the view fact after the spec registration = %q, want %s", got, basisAncestor)
	}
	apply("register-criteria", intake.GateRegistered{Issue: gatedRoot, Step: record.DesignStepCriteria, ArtifactID: criteriaDoc, Version: 2})
	if got := recordedRoot(t, pool).ViewOf; got != basisAncestor {
		t.Fatalf("the view fact after the criteria registration = %q, want %s kept", got, basisAncestor)
	}
	apply("register-spec-again", intake.GateRegistered{Issue: gatedRoot, ArtifactID: gateDoc, Version: 5})
	if got := recordedRoot(t, pool).ViewOf; got != "" {
		t.Fatalf("the view fact after a spec registration that reads no view = %q, want none", got)
	}
}
