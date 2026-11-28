package workflow

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
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

// seedAncestorOnTheBacklog records LEGION-100 as a move to the backlog leaves a root (workflow's
// leave, beginLinger): at phase done with its tree lingering, and with the status Dispatch showed,
// backlog. R1 counts such an ancestor as not accepted.
func seedAncestorOnTheBacklog(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	until := basisEngineNow.Add(time.Hour)
	seedIssue(t, pool, record.Issue{Key: basisAncestor, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: basisAncestor, Project: "LEGION", Title: "ancestor", Phase: phase.Done, Generation: 1, Status: "backlog", DispatchStatus: "backlog",
		Rank: "T", HandedOver: true, LastDispatchSeq: 1, LingerUntil: &until})
}

// pendingSeeds is every pending-basis seed the outbox holds for issue, in order.
func pendingSeeds(t *testing.T, pool *pgxpool.Pool, issue string) []record.PendingBasisSeed {
	t.Helper()
	var seeds []record.PendingBasisSeed
	for _, row := range issueOutboxOf(t, pool, issue, string(record.OutboxKindPendingBasisSeed)) {
		var seed record.PendingBasisSeed
		if err := json.Unmarshal(row.payload, &seed); err != nil {
			t.Fatalf("decode a pending-basis seed of %s: %v", issue, err)
		}
		seeds = append(seeds, seed)
	}
	return seeds
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
		gate.KeptPerson != nil || !slices.Equal(gate.RestsOn, []string{basisAncestor}) {
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
	if got := outboxKindCount(t, pool, gatedRoot, record.OutboxKindGateSeed); got != 1 {
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

// A registration of the same step on the same basis again keeps the moment the step first opened
// (R2): the basis keeps the daemon's time of the first registration, and so the waiting line's
// moment for the tree (record.TreeGate.OpenedAt) does not move when the architect registers the
// open step again, at a later sequence and a later time of the daemon's clock. The same holds for a
// step that waits on the same request of the ancestor.
func TestARegistrationOfAnOpenBasisStepAgainKeepsTheMomentItOpened(t *testing.T) {
	for _, tc := range []struct {
		name  string
		basis func() *record.GateBasis
	}{
		{"a step open on the basis", approvedBasis},
		{"a step pending on the basis", pendingBasis},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := testEngine(config.DesignGateRootIssues, nil)
			apply := applyFacts(t, pool, engine)
			seedGatedTree(t, pool)
			seedLiveAncestor(t, pool)

			apply("register-on-basis", onBasis(tc.basis(), 6))
			later := basisEngineNow.Add(3 * time.Hour)
			engine.cfg.Clock = func() time.Time { return later }
			apply("register-on-basis-again", onBasis(tc.basis(), 7))

			gates := gatesOf(t, pool, gatedRoot)
			if got := gates[record.DesignStepSpec].Basis; got == nil || !got.RegisteredAt.Equal(basisEngineNow) {
				t.Fatalf("the basis after the registration again = %#v, want it registered at the first registration's time %s", got, basisEngineNow)
			}
			if tree := (record.TreeGate{Steps: engine.cfg.Steps, Rows: gates}); tree.Open() && !tree.OpenedAt().Equal(basisEngineNow) {
				t.Fatalf("the tree's gate opened at %s after the registration again, want the first registration's time %s", tree.OpenedAt(), basisEngineNow)
			}
		})
	}
}

// register_gate reads the ancestor not accepted and builds its fact outside any transaction, so
// the ancestor's sign-off can commit between that read and the registration's commit. The
// registration reads the ancestor again in its own transaction, under the lock every fact applies
// under (intake.SerializeFacts) and so in one order with the sign-off's read of the roots that rest
// on the ancestor, finds it accepted, and is refused as register_gate refuses an accepted
// ancestor: nothing is recorded, no gate row, no mirrored request, no view fact and no outbox row,
// and its event is left unmarked, so a call made again is judged afresh. The other order is
// TestRestingRootsHoldTheAncestorSignOff's: the registration first, and the sign-off refused while
// the root rests on the ancestor.
func TestARegistrationOnAnAncestorAcceptedSinceItsReadIsRefused(t *testing.T) {
	pool := migratedPool(t)
	engine := testEngine(config.DesignGateRootIssues, nil)
	apply := applyFacts(t, pool, engine)
	seedGatedTree(t, pool)
	seedAcceptingAncestor(t, pool)
	if result := signOffNow(t, pool, "signoff", basisAncestor); result.Refusal != nil {
		t.Fatalf("the ancestor's sign-off = %+v, want it accepted", result.Refusal)
	}

	refusedWith(t, apply("register-on-basis", onBasis(restingBasis(), 6)), "BASIS_ACCEPTED", basisAncestor, "retro")

	refusedRegistrationLeftNothing(t, pool, "register-on-basis")
}

// refusedRegistrationLeftNothing fails t unless the registration refused under the event id event
// left nothing of the root behind: no gate row, no mirrored request, no outbox row, no mark of the
// event and no view fact.
func refusedRegistrationLeftNothing(t *testing.T, pool *pgxpool.Pool, event string) {
	t.Helper()
	if gates := gatesOf(t, pool, gatedRoot); len(gates) != 0 {
		t.Fatalf("the root's gates = %#v, want none: the refused registration records nothing", gates)
	}
	var asks, rows, marked int
	if err := pool.QueryRow(t.Context(), `select (select count(*) from root_asks where issue = $1), (select count(*) from outbox where issue = $1),
		(select count(*) from processed_events where event_id = $2)`, gatedRoot, event).Scan(&asks, &rows, &marked); err != nil {
		t.Fatalf("read what the refused registration left: %v", err)
	}
	if asks != 0 || rows != 0 || marked != 0 {
		t.Fatalf("the refused registration left %d mirrored requests, %d outbox rows and %d marked events, want none", asks, rows, marked)
	}
	if got := recordedRoot(t, pool).ViewOf; got != "" {
		t.Fatalf("the root's view fact = %q, want none written", got)
	}
}

// register_gate reads the ancestor open outside any transaction, so the ancestor's close_root can
// commit between that read and the registration's commit. close_root writes the ancestor's record
// done and queues Dispatch's status write, so the daemon's mirror of Dispatch and the status the
// record last read from Dispatch both show the ancestor open until Dispatch's event of the close
// comes back. The registration reads the record's own status in its transaction, finds the close,
// and is refused as a registration on an ancestor accepted since its read is: nothing is recorded,
// and its event is left unmarked. TestARegistrationOnAnAncestorAcceptedSinceItsReadIsRefused is
// the same race with the ancestor's sign-off.
func TestARegistrationOnAnAncestorClosedSinceItsReadIsRefused(t *testing.T) {
	pool := migratedPool(t)
	engine := testEngine(config.DesignGateRootIssues, nil)
	apply := applyFacts(t, pool, engine)
	seedGatedTree(t, pool)
	seedIssue(t, pool, record.Issue{Key: basisAncestor, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: basisAncestor, Project: "LEGION", Title: "ancestor", Phase: phase.Admitted, Generation: 1, Status: "in_progress",
		DispatchStatus: "in_progress", Rank: "T", HandedOver: true, LastDispatchSeq: 1})
	seedTreeLifecycle(t, pool, basisAncestor)
	if result := closeRootNow(t, pool, "close", basisAncestor); result.Refusal != nil {
		t.Fatalf("the ancestor's close_root = %+v, want it closed", result.Refusal)
	}
	if closed := storedIssue(t, pool, basisAncestor); closed.Status != record.DoneStatus || closed.DispatchStatus != "in_progress" {
		t.Fatalf("the ancestor after its close_root has status %q and Dispatch status %q, want done and in_progress: Dispatch's event of the close has not come back",
			closed.Status, closed.DispatchStatus)
	}

	refusedWith(t, apply("register-on-basis", onBasis(approvedBasis(), 6)), "BASIS_ACCEPTED", "The daemon records "+basisAncestor+" done")

	refusedRegistrationLeftNothing(t, pool, "register-on-basis")
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
		!gate.Basis.RegisteredAt.Equal(basisEngineNow) || gate.RestsOn != nil || !gate.RestsOnIssue(basisAncestor) {
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
	want := record.PendingBasisSeed{Step: record.DesignStepSpec, ArtifactID: gateDoc, BasisArtifactID: basisDoc, BasisVersion: 4, Ask: "ask-100",
		Generation: 1, RegisteredAt: basisEngineNow}
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
// ancestor it does not record, and one moved out of the workflow to the backlog, get no row, while
// the root's own opening is told as ever. Nothing is recorded as told then, so a registration once
// the ancestor's tree runs tells its architect.
func TestABasisOnAnAncestorInNoLiveTreeQueuesNoRestingRoot(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed func(t *testing.T, pool *pgxpool.Pool)
	}{
		{"an ancestor the daemon does not record", func(*testing.T, *pgxpool.Pool) {}},
		{"an ancestor on the backlog", seedAncestorOnTheBacklog},
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
		restsOn []string
		resting int
	}{
		{"after an approved basis", approvedBasis(), []string{basisAncestor}, 1},
		{"after a pending basis", pendingBasis(), nil, 2},
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
				!slices.Equal(gate.RestsOn, tc.restsOn) || gate.RestsOnIssue(basisAncestor) != (tc.restsOn != nil) {
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

// A relaunched architect reads a step on its ancestor's basis from its catch-up as the step stands
// (dispatch://INFRA-3320, R2). A step a basis opened reads open and registered at the root's
// version, with no version a person approved, since no person approved it, and the ancestor's
// approval beside it, so the architect neither asks a person for an approval nor reads one that
// was never given. A step that waits on the ancestor's open request reads closed and pending,
// with the person's approval of the root's own earlier version it holds and the request beside
// it. Once the wait ends, the step reads as registered on the root's own document, with no basis.
func TestACatchUpStatesAStepOnABasisAsItStands(t *testing.T) {
	personal := 2
	own := record.DesignGate{Issue: gatedRoot, Step: record.DesignStepSpec, ArtifactID: gateDoc, LatestVersion: 4,
		ApprovedVersion: &personal, ApprovedMark: record.EventMark{Seq: 3, At: basisApprovedAt.Add(-time.Hour)}}
	registered := func(basis *record.GateBasis) record.GateBasis {
		basis.RegisteredAt = basisEngineNow
		return *basis
	}
	pending := record.PendOnBasis(own, registered(pendingBasis()), "ask-100")
	for _, tc := range []struct {
		name string
		gate record.DesignGate
		want record.CatchUpGate
	}{
		{"a step a basis opened", record.OpenOnBasis(own, registered(approvedBasis())), record.CatchUpGate{
			Step: record.DesignStepSpec, Gated: true, Artifact: gateDoc, Version: 4, Open: true,
			Basis: &record.CatchUpBasis{Issue: basisAncestor, Artifact: basisDoc, Version: 3, Approver: "user:alice", ApprovedAt: basisApprovedAt},
		}},
		{"a step that waits on the ancestor's request", pending, record.CatchUpGate{
			Step: record.DesignStepSpec, Gated: true, Artifact: gateDoc, Version: 4, Approved: 2, Pending: true,
			Basis: &record.CatchUpBasis{Issue: basisAncestor, Artifact: basisDoc, Version: 4, Ask: "ask-100"},
		}},
		{"a step whose wait ended", record.EndPendingBasis(pending), record.CatchUpGate{
			Step: record.DesignStepSpec, Gated: true, Artifact: gateDoc, Version: 4, Approved: 2,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedGatedTree(t, pool)
			seedGate(t, pool, tc.gate)
			claimReady(t, pool, testEngine(config.DesignGateRootIssues, nil), gatedRoot, claim.RoleArchitect, 1)
			gates := lastCatchUp(t, pool).Gates
			if len(gates) != 1 {
				t.Fatalf("the catch-up states %#v, want the one spec step", gates)
			}
			got := gates[0]
			// Dispatch's time of the approval is compared as an instant: the notice crossed the
			// outbox as JSON.
			if got.Basis != nil && tc.want.Basis != nil {
				if !got.Basis.ApprovedAt.Equal(tc.want.Basis.ApprovedAt) {
					t.Fatalf("the catch-up names the approval at %s, want %s", got.Basis.ApprovedAt, tc.want.Basis.ApprovedAt)
				}
				basis, want := *got.Basis, *tc.want.Basis
				basis.ApprovedAt, want.ApprovedAt = time.Time{}, time.Time{}
				got.Basis, tc.want.Basis = &basis, &want
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("the catch-up states %#v (basis %#v), want %#v (basis %#v)", got, got.Basis, tc.want, tc.want.Basis)
			}
		})
	}
}
