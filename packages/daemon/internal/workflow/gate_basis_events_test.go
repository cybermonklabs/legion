package workflow

import (
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

// basisPerson is the person whose acts on LEGION-100's document these tests apply.
var basisPerson = dispatch.Actor{Kind: "user", ID: "alice"}

// ancestorReview is an event of version of LEGION-100's primary document: a person's review of
// kind, or a version event, at seq in LEGION-100's event log.
func ancestorReview(kind intake.DispatchArtifactKind, version int, seq int64, reason string) intake.DispatchArtifact {
	fact := intake.DispatchArtifact{Key: basisAncestor, ArtifactID: basisDoc, Kind: kind, Version: version, Reason: reason,
		Mark: record.EventMark{Seq: seq, At: basisApprovedAt.Add(time.Duration(seq) * time.Minute)}}
	if kind != intake.DispatchArtifactVersion {
		fact.Actor = basisPerson
	}
	return fact
}

// registeredPending is LEGION-208 admitted in its slot with its spec step registered at version 4
// on LEGION-100's version 4, pending on request ask-100, while LEGION-100 runs as a tree of its
// own: the point every pending case below starts from.
func registeredPending(t *testing.T) (*pgxpool.Pool, *Engine, func(string, intake.Fact) intake.Result) {
	t.Helper()
	pool := migratedPool(t)
	engine := testEngine(config.DesignGateRootIssues, nil)
	apply := applyFacts(t, pool, engine)
	seedGatedTree(t, pool)
	seedLiveAncestor(t, pool)
	apply("register-pending", onBasis(pendingBasis(), 6))
	if gate := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]; !gate.PendingOnBasis() {
		t.Fatalf("the spec step after the pending registration = %#v, want it pending", gate)
	}
	return pool, engine, apply
}

// The person's approval of the ancestor's version a step waits on opens the step, as a
// registration on that approval would: the step stands open at the version registered on the
// approval, rests on the ancestor, and the root's issue gets the message that records the basis,
// its architect a design-approved notice naming the ancestor's version as the basis, the
// ancestor's architect the root's first rest, and the gate seed is queued. The same approval
// delivered again tells nobody anything new, and admission's pass then starts the tree.
func TestAPendingBasisOpensOnThePersonsApprovalOfTheAncestorsVersion(t *testing.T) {
	pool, engine, apply := registeredPending(t)
	approved := ancestorReview(intake.DispatchArtifactApproved, 4, 60, "")

	apply("ancestor-approved", approved)

	gate := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]
	if !gate.OnBasis() || !record.GateOpen(gate) || *gate.ApprovedVersion != 4 || gate.RestsOn != basisAncestor {
		t.Fatalf("the spec step = %#v, want it open at version 4 on the basis, resting on %s", gate, basisAncestor)
	}
	if basis := gate.Basis; basis.Version != 4 || basis.Approver != "user:alice" || basis.Approval != approved.Mark || !basis.RegisteredAt.Equal(basisEngineNow) {
		t.Fatalf("the basis = %#v, want the person's approval of version 4 at %#v, registered at the registration's time", basis, approved.Mark)
	}
	opened := noticesOfKind(t, pool, gatedRoot, "design-approved")
	if len(opened) != 1 || opened[0].Ref != "dispatch://LEGION-100/spec" || opened[0].Version != 4 || opened[0].Actor != "user:alice" ||
		!strings.Contains(opened[0].Reason, "No person approved that version") {
		t.Fatalf("design-approved notices = %#v, want one naming version 4 of LEGION-100's document as the basis", opened)
	}
	if bodies := messageBodies(t, pool); len(bodies) != 1 || !strings.HasPrefix(bodies[0], basisOpenedLine+"\n") || !strings.Contains(bodies[0], "A person approved version 4") {
		t.Fatalf("Dispatch messages = %q, want the one that records the basis of version 4", bodies)
	}
	resting := noticesOfKind(t, pool, basisAncestor, "resting-root")
	if len(resting) != 2 || !strings.Contains(resting[1].Reason, "An ancestor's approval opened the spec step of LEGION-208") {
		t.Fatalf("resting-root notices = %#v, want the pending registration's and the opening's", resting)
	}
	if got := gateSeedCount(t, pool, gatedRoot); got != 2 {
		t.Fatalf("gate seeds = %d, want the registration's and the opening's", got)
	}
	if got := noticesOfKind(t, pool, gatedRoot, "basis-ended"); len(got) != 0 {
		t.Fatalf("basis-ended notices = %#v, want none for a wait the approval answered", got)
	}

	apply("ancestor-approved-again", approved)
	if got := noticesOfKind(t, pool, gatedRoot, "design-approved"); len(got) != 1 {
		t.Fatalf("design-approved notices after the approval again = %#v, want the first alone", got)
	}
	if bodies := messageBodies(t, pool); len(bodies) != 1 {
		t.Fatalf("Dispatch messages after the approval again = %d, want the first alone", len(bodies))
	}

	admissionPass(t, pool, engine)
	if got := phaseOf(t, pool, gatedRoot); got != phase.Planning {
		t.Fatalf("the root = %s after admission's pass, want planning: its step stands open", got)
	}
}

// Each act R1 names ends a pending row: a changes request on the ancestor's version, a newer
// version of the ancestor's document, a session's close of the request, the person's close of it
// with no review, the ancestor done or in Dispatch's retro, the ancestor losing the legion label, and
// a newer version or a changes request of the root's own document. The step stays closed,
// registered on the root's own document with no basis and resting on nothing, the root's
// architect gets a basis-ended notice that names the act, and the ancestor's architect a
// resting-root notice of the end.
func TestEachActOnAPendingBasisEndsItWithABasisEndedNotice(t *testing.T) {
	session := dispatch.Actor{Kind: "session", ID: "session-other"}
	closed := func(actor dispatch.Actor) intake.DispatchAskClosed {
		return intake.DispatchAskClosed{Key: basisAncestor, Ask: "ask-100", Kind: record.AskApproval, Artifact: basisDoc, State: "resolved",
			Actor: actor, Mark: askSeq(61)}
	}
	ancestorAt := func(status string, labelled bool) intake.DispatchIssue {
		return intake.DispatchIssue{Key: basisAncestor, Seq: 5, Type: "issue.updated", Status: status, Title: "ancestor", Rank: "T", HandedOver: labelled}
	}
	for _, tc := range []struct {
		name    string
		fact    intake.Fact
		act     string
		version int
		changes int
	}{
		{name: "a changes request on the ancestor's version", fact: ancestorReview(intake.DispatchArtifactChangesRequested, 4, 61, "name the rollback path"),
			act: `user:alice requested changes on version 4 of the primary document of LEGION-100 (dispatch://LEGION-100/spec): "name the rollback path".`},
		{name: "a newer version of the ancestor's document", fact: ancestorReview(intake.DispatchArtifactVersion, 5, 61, ""),
			act: "Version 5 of the primary document of LEGION-100 (dispatch://LEGION-100/spec) is newer than version 4"},
		{name: "a session's close of the request", fact: closed(session),
			act: "session:session-other closed approval request ask-100 of version 4 of the primary document of LEGION-100"},
		{name: "the person's close of the request with no review", fact: closed(basisPerson),
			act: "user:alice closed approval request ask-100 of version 4"},
		{name: "the ancestor done", fact: ancestorAt("done", true), act: "Dispatch shows LEGION-100 done"},
		{name: "the ancestor in Dispatch's retro", fact: ancestorAt("retro", true), act: "Dispatch shows LEGION-100 retro"},
		{name: "the ancestor without the legion label", fact: ancestorAt("in_progress", false), act: "LEGION-100 no longer carries the legion label."},
		{name: "a newer version of the root's own document", fact: intake.DispatchArtifact{Key: gatedRoot, ArtifactID: gateDoc,
			Kind: intake.DispatchArtifactVersion, Version: 5, Mark: askSeq(9)}, act: "Version 5 of the primary document of LEGION-208", version: 5},
		{name: "a changes request on the root's own document", fact: intake.DispatchArtifact{Key: gatedRoot, ArtifactID: gateDoc,
			Kind: intake.DispatchArtifactChangesRequested, Version: 4, Reason: "say more", Actor: basisPerson, Mark: askSeq(9)},
			act: `user:alice requested changes on version 4 of the primary document of LEGION-208 (dispatch://LEGION-208/spec), the root's own document: "say more".`, changes: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, _, apply := registeredPending(t)

			apply("act", tc.fact)

			gate := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]
			version := max(tc.version, 4)
			if gate.Basis != nil || record.GateOpen(gate) || gate.ArtifactID != gateDoc || gate.LatestVersion != version || gate.RestsOnIssue(basisAncestor) {
				t.Fatalf("the spec step = %#v, want it closed at version %d on the root's own document, with no basis and resting on nothing", gate, version)
			}
			ended := noticesOfKind(t, pool, gatedRoot, "basis-ended")
			if len(ended) != 1 || ended[0].Ref != "dispatch://LEGION-100/spec" || ended[0].Version != 4 || ended[0].Role != claim.RoleArchitect ||
				!strings.Contains(ended[0].Reason, tc.act) || !strings.Contains(ended[0].Reason, "no longer waits on approval request ask-100 of version 4") {
				t.Fatalf("basis-ended notices = %#v, want one naming %q and the end of the wait on ask-100", ended, tc.act)
			}
			resting := noticesOfKind(t, pool, basisAncestor, "resting-root")
			if len(resting) != 2 || !strings.Contains(resting[1].Reason, tc.act) {
				t.Fatalf("resting-root notices = %#v, want the pending registration's and its end naming %q", resting, tc.act)
			}
			if got := noticesOfKind(t, pool, gatedRoot, "design-changes-requested"); len(got) != tc.changes {
				t.Fatalf("design-changes-requested notices = %#v, want %d", got, tc.changes)
			}
			if got := noticesOfKind(t, pool, gatedRoot, "design-approved"); len(got) != 0 {
				t.Fatalf("design-approved notices = %#v, want none: the step stays closed", got)
			}
		})
	}
}

// The daemon's own move of the ancestor into merging accepts it, which ends every step waiting on
// its request as R1's first condition ends it.
func TestTheAncestorsMoveIntoMergingEndsAPendingBasis(t *testing.T) {
	pool := migratedPool(t)
	engine := testEngine(config.DesignGateRootIssues, nil)
	apply := applyFacts(t, pool, engine)
	seedGatedTree(t, pool)
	seedIssue(t, pool, record.Issue{Key: basisAncestor, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: basisAncestor, Project: "LEGION", Title: "ancestor", Phase: phase.Retro, Generation: 1, Status: "retro", Rank: "T",
		HandedOver: true, LastDispatchSeq: 1})
	seedTreeLifecycle(t, pool, basisAncestor)
	seedPhase(t, pool, record.PhaseRow{Issue: basisAncestor, Role: claim.RoleImplementer, Claim: "claim"})
	apply("register-pending", onBasis(pendingBasis(), 6))

	apply("retro-done", intake.HandoffComplete{Generation: 1, Issue: basisAncestor, Role: claim.RoleImplementer, Claim: "claim", Commit: "retro"})

	if got := phaseOf(t, pool, basisAncestor); got != phase.Merging {
		t.Fatalf("the ancestor = %s, want merging", got)
	}
	if gate := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]; gate.Basis != nil || record.GateOpen(gate) {
		t.Fatalf("the spec step = %#v, want it closed with no basis", gate)
	}
	ended := noticesOfKind(t, pool, gatedRoot, "basis-ended")
	if len(ended) != 1 || !strings.Contains(ended[0].Reason, "The daemon moved LEGION-100 into merging") {
		t.Fatalf("basis-ended notices = %#v, want one naming the ancestor's move into merging", ended)
	}
}

// A person's approval of the root's own document while its step waits on the ancestor's request
// opens the step on that approval and ends the wait, so the root's architect gets both notices:
// design-approved from the approval, and basis-ended, whose reason says the person's approval of
// the root's own document at that version opened the step and ended the wait.
func TestAPersonsApprovalOfTheRootsOwnDocumentEndsAPendingBasisWithBothNotices(t *testing.T) {
	pool, _, apply := registeredPending(t)

	apply("own-approval", stepApproval(gatedRoot, gateDoc, 4, askSeq(9)))

	gate := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]
	if gate.Basis != nil || !record.GateOpen(gate) || gate.ApprovedMark.Seq != 9 {
		t.Fatalf("the spec step = %#v, want it open on the person's approval of version 4 with no basis", gate)
	}
	opened := noticesOfKind(t, pool, gatedRoot, "design-approved")
	if len(opened) != 1 || opened[0].Ref != "dispatch://LEGION-208/spec" || opened[0].Version != 4 {
		t.Fatalf("design-approved notices = %#v, want one naming the root's own document at version 4", opened)
	}
	ended := noticesOfKind(t, pool, gatedRoot, "basis-ended")
	if len(ended) != 1 || !strings.Contains(ended[0].Reason, "user:alice approved version 4 of the primary document of LEGION-208") ||
		!strings.Contains(ended[0].Reason, "That approval opened the spec step of LEGION-208 and ended its wait") {
		t.Fatalf("basis-ended notices = %#v, want one saying the person's approval of the root's own version 4 opened the step and ended the wait", ended)
	}
	if bodies := messageBodies(t, pool); len(bodies) != 0 {
		t.Fatalf("Dispatch messages = %q, want none: no ancestor's approval opened the step", bodies)
	}
}

// A root waiting on a person keeps that wait beside its step's wait on the ancestor's request: the
// ancestor's approval answers the root's wait as an approval does, and the end of the step's wait
// answers it as a changes request does, with no notice beside either; the end's answer names what
// ended the wait for the resume. A person's approval of the root's own document answers it too,
// and sends neither notice. The ancestor's architect is told the end as ever.
func TestAPendingBasisAnswersARootAwaitingAPersonWithNoNotice(t *testing.T) {
	for _, wait := range []struct {
		name    string
		waiting record.Awaiting
	}{
		{"on a question of its own", record.Awaiting{Ask: "ask-q", Since: record.EventMark{Seq: 5}}},
		{"on its own approval request", record.Awaiting{Artifact: gateDoc, GateVersion: 4, Since: record.EventMark{Seq: 5}}},
	} {
		for _, tc := range []struct {
			name   string
			fact   intake.Fact
			by     record.Answer
			detail string
			open   bool
		}{
			{name: "the ancestor's approval", fact: ancestorReview(intake.DispatchArtifactApproved, 4, 60, ""), by: record.AnsweredByApproval, open: true},
			{name: "the ancestor's changes request", fact: ancestorReview(intake.DispatchArtifactChangesRequested, 4, 61, "name the rollback path"),
				by: record.AnsweredByChanges, detail: basisEndedAnswer + `user:alice requested changes on version 4 of the primary document of LEGION-100`},
			{name: "the person's approval of the root's own document", fact: stepApproval(gatedRoot, gateDoc, 4, askSeq(9)), by: record.AnsweredByApproval, open: true},
		} {
			t.Run(wait.name+", "+tc.name, func(t *testing.T) {
				pool := migratedPool(t)
				engine := testEngine(config.DesignGateRootIssues, nil)
				apply := applyFacts(t, pool, engine)
				waiting := wait.waiting
				seedIssue(t, pool, record.Issue{Key: gatedRoot, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
					Tree: gatedRoot, Project: "LEGION", Title: "root", Phase: phase.AwaitingPerson, Generation: 1, Status: "in_progress", Rank: "U",
					HandedOver: true, LastDispatchSeq: 1, Awaiting: &waiting})
				seedSlot(t, pool, record.Slot{Issue: gatedRoot, Index: 0, AdmittedAt: basisEngineNow})
				seedArchitectClaim(t, pool, gatedRoot)
				seedGate(t, pool, record.DesignGate{Issue: gatedRoot, ArtifactID: gateDoc, LatestVersion: 4})
				seedLiveAncestor(t, pool)
				apply("register-pending", onBasis(pendingBasis(), 6))
				if root := recordedRoot(t, pool); root.Awaiting == nil || root.Awaiting.AnsweredAt != nil {
					t.Fatalf("the root after the pending registration = %#v, want it still waiting", root.Awaiting)
				}

				apply("act", tc.fact)

				root := recordedRoot(t, pool)
				if root.Awaiting == nil || root.Awaiting.AnsweredAt == nil || root.Awaiting.AnsweredBy != tc.by ||
					!strings.HasPrefix(root.Awaiting.AnsweredDetail, tc.detail) {
					t.Fatalf("the waiting root = %#v, want its wait answered by %q with a detail starting %q", root.Awaiting, tc.by, tc.detail)
				}
				if basisEndedWait(*root.Awaiting) != (tc.by == record.AnsweredByChanges) {
					t.Fatalf("the waiting root's answer %#v reads as the end of the step's wait = %t, want %t", root.Awaiting,
						basisEndedWait(*root.Awaiting), tc.by == record.AnsweredByChanges)
				}
				if gate := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]; record.GateOpen(gate) != tc.open || gate.PendingOnBasis() {
					t.Fatalf("the spec step = %#v, want open=%t and no longer pending", gate, tc.open)
				}
				for _, kind := range []record.NoticeKind{"design-approved", "basis-ended"} {
					if got := noticesOfKind(t, pool, gatedRoot, kind); len(got) != 0 {
						t.Fatalf("%s notices = %#v, want none: a waiting root is told by its resume", kind, got)
					}
				}
				if resting := noticesOfKind(t, pool, basisAncestor, "resting-root"); len(resting) != 2 {
					t.Fatalf("resting-root notices = %#v, want the pending registration's and the end or opening", resting)
				}
			})
		}
	}
}

// roleMessageCount is how many role messages the outbox holds: what a move of a tree's standing
// gate tells its phase holders (tellGateMoved).
func roleMessageCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var got int
	if err := pool.QueryRow(t.Context(), `select count(*) from outbox where kind = $1`, string(record.OutboxKindRoleMessage)).Scan(&got); err != nil {
		t.Fatalf("count the role messages: %v", err)
	}
	return got
}

// A step an ancestor's approval opened changes as an approved step of the root's own document
// changes (R3): a newer version of that document closes it, falling back to the person's approval
// of an earlier version it kept, and tells the tree the close as any close does; a person's changes
// request on it closes it with design-changes-requested; a person's approval of the version
// registered opens it on that approval with no basis left; and a person's approval of the earlier
// version delivered again changes nothing. None of these is a wait's end, so no basis-ended notice
// goes.
func TestTheRootsOwnDocumentMovesAnOpenBasisStepAsAnApprovedStep(t *testing.T) {
	personal := record.EventMark{Seq: 3, At: basisApprovedAt.Add(-time.Hour)}
	for _, tc := range []struct {
		name     string
		fact     intake.DispatchArtifact
		open     bool
		approved int
		changes  int
		moved    int
	}{
		{name: "a newer version", fact: intake.DispatchArtifact{Key: gatedRoot, ArtifactID: gateDoc, Kind: intake.DispatchArtifactVersion, Version: 5, Mark: askSeq(9)},
			approved: 2, moved: 1},
		{name: "a person's changes request", fact: intake.DispatchArtifact{Key: gatedRoot, ArtifactID: gateDoc, Kind: intake.DispatchArtifactChangesRequested,
			Version: 4, Reason: "say more", Actor: basisPerson, Mark: askSeq(9)}, changes: 1, moved: 1},
		{name: "a person's approval of the version registered", fact: stepApproval(gatedRoot, gateDoc, 4, askSeq(9)), open: true, approved: 4},
		{name: "the person's approval of the earlier version again", fact: stepApproval(gatedRoot, gateDoc, 2, personal), open: true, approved: 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := testEngine(config.DesignGateRootIssues, nil)
			apply := applyFacts(t, pool, engine)
			seedGatedTree(t, pool)
			seedLiveAncestor(t, pool)
			seedGate(t, pool, record.DesignGate{Issue: gatedRoot, ArtifactID: gateDoc, LatestVersion: 4, ApprovedVersion: new(2), ApprovedMark: personal})
			apply("register-on-basis", onBasis(approvedBasis(), 6))
			moved := roleMessageCount(t, pool)

			apply("own-act", tc.fact)

			gate := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]
			stillOnBasis := tc.name == "the person's approval of the earlier version again"
			if record.GateOpen(gate) != tc.open || gate.OnBasis() != stillOnBasis || gate.RestsOn != basisAncestor {
				t.Fatalf("the spec step = %#v, want open=%t, on its basis=%t, still resting on %s", gate, tc.open, stillOnBasis, basisAncestor)
			}
			switch {
			case tc.approved == 0 && gate.ApprovedVersion != nil, tc.approved != 0 && (gate.ApprovedVersion == nil || *gate.ApprovedVersion != tc.approved):
				t.Fatalf("the spec step's approval = %v, want version %d (0 for none)", gate.ApprovedVersion, tc.approved)
			case !stillOnBasis && gate.KeptPerson != nil:
				t.Fatalf("the spec step = %#v, want no kept approval once the basis ended", gate)
			}
			if got := noticesOfKind(t, pool, gatedRoot, "design-changes-requested"); len(got) != tc.changes {
				t.Fatalf("design-changes-requested notices = %#v, want %d", got, tc.changes)
			}
			if got := roleMessageCount(t, pool) - moved; got != tc.moved {
				t.Fatalf("role messages the act queued = %d, want %d", got, tc.moved)
			}
			if got := noticesOfKind(t, pool, gatedRoot, "design-approved"); len(got) != 1 {
				t.Fatalf("design-approved notices = %#v, want the registration's alone", got)
			}
			if got := noticesOfKind(t, pool, gatedRoot, "basis-ended"); len(got) != 0 {
				t.Fatalf("basis-ended notices = %#v, want none: no wait ended", got)
			}
		})
	}
}

// The person's approval of the version a step waits on, of an ancestor that is a labelled root
// parked on the backlog, is that person's handover of the ancestor, and it opens the waiting step
// as well: the ancestor's steps are read apart from the handover's branch (item 6 of the plan).
func TestAPendingBasisOpensOnTheApprovalThatReadmitsAParkedAncestor(t *testing.T) {
	pool := migratedPool(t)
	engine := testEngine(config.DesignGateRootIssues, nil)
	apply := applyFacts(t, pool, engine)
	seedGatedTree(t, pool)
	seedIssue(t, pool, record.Issue{Key: basisAncestor, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: basisAncestor, Project: "LEGION", Title: "ancestor", Phase: phase.Admitted, Generation: 1, Status: "backlog", Rank: "T",
		HandedOver: true, LastDispatchSeq: 1})
	apply("register-pending", onBasis(pendingBasis(), 6))
	approved := ancestorReview(intake.DispatchArtifactApproved, 4, 60, "")
	approved.Issue = &dispatch.Issue{Key: basisAncestor, Status: "backlog", HandedOver: true, PrimaryArtifactID: basisDoc}

	apply("ancestor-approved", approved)

	if gate := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]; !gate.OnBasis() || !record.GateOpen(gate) || gate.Basis.Version != 4 {
		t.Fatalf("the spec step = %#v, want it open on version 4 of the parked ancestor's document", gate)
	}
	if got := noticesOfKind(t, pool, gatedRoot, "design-approved"); len(got) != 1 {
		t.Fatalf("design-approved notices = %#v, want one", got)
	}
	var queued int
	if err := pool.QueryRow(t.Context(), `select count(*) from outbox where kind = 'dispatch_status' and issue = $1`, basisAncestor).Scan(&queued); err != nil || queued != 1 {
		t.Fatalf("status writes queued for the ancestor = %d, %v; want the person's handover", queued, err)
	}
}

// Nothing done to the ancestor's document moves a step that stands open on its approval (R3): a
// newer version of it and another approval leave the step open and tell nobody. A person's changes
// request on the version the step rests on, made after that approval, is passed on as
// basis-changes-requested to the root's architect and, keyed by the ancestor, to the ancestor's
// architect, and the step stays open. Each review is told once: the same review delivered again,
// or read again by a seed with its time alone, tells nothing more, while a second changes request
// on the same version is told as well. A changes request Dispatch recorded before the approval is
// none the step passes on.
func TestAnOpenBasisPassesOnOnlyAPersonsChangesRequestOnItsVersion(t *testing.T) {
	pool := migratedPool(t)
	engine := testEngine(config.DesignGateRootIssues, nil)
	apply := applyFacts(t, pool, engine)
	seedGatedTree(t, pool)
	seedLiveAncestor(t, pool)
	apply("register-on-basis", onBasis(approvedBasis(), 6))
	stillOpen := func(after string) {
		t.Helper()
		gate := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]
		if !gate.OnBasis() || !record.GateOpen(gate) || gate.Basis.Version != 3 || *gate.ApprovedVersion != 4 {
			t.Fatalf("the spec step after %s = %#v, want it open at version 4 on version 3 of the ancestor's document", after, gate)
		}
	}
	told := func(after string, want int) {
		t.Helper()
		root, ancestor := noticesOfKind(t, pool, gatedRoot, "basis-changes-requested"), noticesOfKind(t, pool, basisAncestor, "basis-changes-requested")
		if len(root) != want || len(ancestor) != want {
			t.Fatalf("basis-changes-requested notices after %s: %d to the root's architect and %d to the ancestor's, want %d each", after, len(root), len(ancestor), want)
		}
	}
	asked := ancestorReview(intake.DispatchArtifactChangesRequested, 3, 45, "name the rollback path")

	apply("header-changes", asked)
	stillOpen("the changes request")
	told("the changes request", 1)
	root, ancestor := noticesOfKind(t, pool, gatedRoot, "basis-changes-requested")[0], noticesOfKind(t, pool, basisAncestor, "basis-changes-requested")[0]
	if root.Ref != "dispatch://LEGION-100/spec" || root.Version != 3 || root.Actor != "user:alice" || !strings.Contains(root.Reason, `"name the rollback path"`) ||
		!strings.Contains(root.Reason, "The step stays open.") {
		t.Fatalf("the root's basis-changes-requested = %#v, want it naming LEGION-100's version 3, the person, the reason and the step left open", root)
	}
	if !strings.Contains(ancestor.Reason, "The step of LEGION-208 stays open.") || !strings.Contains(ancestor.Reason, "dispatch://LEGION-100/spec#b-r1@v3") {
		t.Fatalf("the ancestor's basis-changes-requested = %#v, want it naming the root and the requirements it serves", ancestor)
	}

	apply("header-changes-again", asked)
	apply("seed-read", intake.BasisChangesRead{Issue: gatedRoot, Step: record.DesignStepSpec, Generation: 1, BasisArtifactID: basisDoc, BasisVersion: 3,
		Changes: intake.BasisReview{Mark: record.EventMark{At: asked.Mark.At}, Actor: basisPerson, Reason: asked.Reason}})
	told("the same review delivered and read again", 1)

	apply("changes-before-the-approval", ancestorReview(intake.DispatchArtifactChangesRequested, 3, 30, "older"))
	told("a changes request older than the approval", 1)

	apply("second-changes", ancestorReview(intake.DispatchArtifactChangesRequested, 3, 47, "and the restore path"))
	told("a second changes request on the version", 2)

	apply("ancestor-v4", ancestorReview(intake.DispatchArtifactVersion, 4, 50, ""))
	apply("ancestor-v4-approved", ancestorReview(intake.DispatchArtifactApproved, 4, 51, ""))
	apply("ancestor-v4-changes", ancestorReview(intake.DispatchArtifactChangesRequested, 4, 52, "not this one"))
	stillOpen("a newer version, its approval and a changes request on it")
	told("a newer version, its approval and a changes request on it", 2)
	if got := noticesOfKind(t, pool, gatedRoot, "design-approved"); len(got) != 1 {
		t.Fatalf("design-approved notices = %#v, want the registration's alone", got)
	}
	for _, kind := range []record.NoticeKind{"basis-ended", "design-changes-requested"} {
		if got := noticesOfKind(t, pool, gatedRoot, kind); len(got) != 0 {
			t.Fatalf("%s notices = %#v, want none: nothing done to the ancestor's document moves the step", kind, got)
		}
	}
}

// The pending-basis seed's read applies as the events it read would have, and only to a step of
// that generation still pending on that request: the person's approval opens it, an approval
// followed by a changes request opens it and passes the changes request on, a changes request or
// a closed request ends it, and a request still waiting changes nothing. A read of a wait that has
// ended since, or of another generation, changes nothing.
func TestThePendingBasisReadOpensOrEndsOnlyAStepStillPendingOnIt(t *testing.T) {
	approval := &intake.BasisReview{Mark: record.EventMark{At: basisApprovedAt.Add(24 * time.Hour)}, Actor: basisPerson}
	changes := &intake.BasisReview{Mark: record.EventMark{At: basisApprovedAt.Add(25 * time.Hour)}, Actor: basisPerson, Reason: "name the rollback path"}
	read := func(edit func(*intake.PendingBasisRead)) intake.PendingBasisRead {
		fact := intake.PendingBasisRead{Issue: gatedRoot, Step: record.DesignStepSpec, Generation: 1, ArtifactID: gateDoc, BasisArtifactID: basisDoc,
			BasisVersion: 4, Ask: "ask-100", LatestVersion: 4, RequestOpen: true}
		edit(&fact)
		return fact
	}
	for _, tc := range []struct {
		name    string
		fact    intake.PendingBasisRead
		open    bool
		pending bool
		ended   int
		asked   int
	}{
		{name: "approved by the person", fact: read(func(f *intake.PendingBasisRead) { f.Approval, f.RequestOpen = approval, false }), open: true},
		{name: "approved and then sent back", fact: read(func(f *intake.PendingBasisRead) { f.Approval, f.Changes, f.RequestOpen = approval, changes, false }),
			open: true, asked: 1},
		{name: "sent back", fact: read(func(f *intake.PendingBasisRead) { f.Changes, f.RequestOpen = changes, false }), ended: 1},
		{name: "a newer version", fact: read(func(f *intake.PendingBasisRead) { f.LatestVersion = 5 }), ended: 1},
		{name: "the request closed with no approval", fact: read(func(f *intake.PendingBasisRead) { f.RequestOpen = false }), ended: 1},
		{name: "still waiting", fact: read(func(*intake.PendingBasisRead) {}), pending: true},
		{name: "another generation", fact: read(func(f *intake.PendingBasisRead) { f.Approval, f.Generation = approval, 2 }), pending: true},
		{name: "another request", fact: read(func(f *intake.PendingBasisRead) { f.Approval, f.Ask = approval, "ask-99" }), pending: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, _, apply := registeredPending(t)

			apply("read", tc.fact)

			gate := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]
			if record.GateOpen(gate) != tc.open || gate.PendingOnBasis() != tc.pending || gate.OnBasis() != tc.open {
				t.Fatalf("the spec step = %#v, want open=%t pending=%t", gate, tc.open, tc.pending)
			}
			if tc.open && gate.Basis.Approval != approval.Mark {
				t.Fatalf("the basis = %#v, want the approval the read found", gate.Basis)
			}
			if got := noticesOfKind(t, pool, gatedRoot, "basis-ended"); len(got) != tc.ended {
				t.Fatalf("basis-ended notices = %#v, want %d", got, tc.ended)
			}
			if got := noticesOfKind(t, pool, gatedRoot, "basis-changes-requested"); len(got) != tc.asked {
				t.Fatalf("basis-changes-requested notices = %#v, want %d", got, tc.asked)
			}
			if opened := noticesOfKind(t, pool, gatedRoot, "design-approved"); (len(opened) == 1) != tc.open || len(opened) > 1 {
				t.Fatalf("design-approved notices = %#v, want one only for an opening", opened)
			}
		})
	}

	t.Run("a wait that ended since", func(t *testing.T) {
		pool, _, apply := registeredPending(t)
		apply("ancestor-v5", ancestorReview(intake.DispatchArtifactVersion, 5, 61, ""))
		apply("read", read(func(f *intake.PendingBasisRead) { f.Approval = approval }))
		if gate := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]; gate.Basis != nil || record.GateOpen(gate) {
			t.Fatalf("the spec step = %#v, want it left closed with no basis", gate)
		}
		if got := noticesOfKind(t, pool, gatedRoot, "basis-ended"); len(got) != 1 {
			t.Fatalf("basis-ended notices = %#v, want the version's alone", got)
		}
	})
}
