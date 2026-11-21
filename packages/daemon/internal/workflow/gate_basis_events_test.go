package workflow

import (
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
	if !gate.OnBasis() || !record.GateOpen(gate) || *gate.ApprovedVersion != 4 || !slices.Equal(gate.RestsOn, []string{basisAncestor}) {
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
	if got := outboxKindCount(t, pool, gatedRoot, record.OutboxKindGateSeed); got != 2 {
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
// with no review, the withdrawal of the request that the ancestor's close records (the ask event
// Dispatch appends before the ancestor's own issue.closed), the ancestor done or in Dispatch's
// retro, the ancestor losing the legion label, and a newer version or a changes request of the
// root's own document. The step stays closed, registered on the root's own document with no basis
// and resting on nothing, the root's architect gets a basis-ended notice that names the act, and
// the ancestor's architect a resting-root notice of the end.
func TestEachActOnAPendingBasisEndsItWithABasisEndedNotice(t *testing.T) {
	session := dispatch.Actor{Kind: "session", ID: "session-other"}
	closed := func(actor dispatch.Actor) intake.DispatchAskClosed {
		return intake.DispatchAskClosed{Key: basisAncestor, Ask: "ask-100", Kind: record.AskApproval, Artifact: basisDoc, State: "resolved",
			Actor: actor, Mark: askSeq(61)}
	}
	withdrawn := closed(basisPerson)
	withdrawn.Withdrawn = true
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
		{name: "the ancestor's close, which withdraws the request", fact: withdrawn,
			act: "Dispatch shows LEGION-100 done, so LEGION-100 is accepted. Its close withdrew approval request ask-100 of version 4 of the primary document of LEGION-100"},
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

// A row of a step the deployment does not gate holds nothing and takes no event, as basisArtifact
// rules: the close of the request it waits on and the ancestor's acceptance leave its wait as it
// is, and tell nobody.
func TestAPendingBasisOfAStepTheDeploymentDoesNotGateTakesNoEvent(t *testing.T) {
	for _, tc := range []struct {
		name string
		fact intake.Fact
	}{
		{"a session's close of the request", intake.DispatchAskClosed{Key: basisAncestor, Ask: "ask-100", Kind: record.AskApproval, Artifact: basisDoc,
			State: "resolved", Actor: dispatch.Actor{Kind: "session", ID: "session-other"}, Mark: askSeq(61)}},
		{"the ancestor done", intake.DispatchIssue{Key: basisAncestor, Seq: 5, Type: "issue.updated", Status: "done", Title: "ancestor", Rank: "T", HandedOver: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, _, _ := registeredPending(t)
			// The next boot gates the criteria step alone.
			criteriaOnly := stepsEngine([]record.DesignStep{record.DesignStepCriteria}, nil)

			applyFacts(t, pool, criteriaOnly)("act", tc.fact)

			if gate := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]; !gate.PendingOnBasis() || gate.Basis.Pending.Ask != "ask-100" {
				t.Fatalf("the spec step = %#v, want it still pending on ask-100: the deployment does not gate it", gate)
			}
			if got := noticesOfKind(t, pool, gatedRoot, "basis-ended"); len(got) != 0 {
				t.Fatalf("basis-ended notices = %#v, want none for a step the deployment does not gate", got)
			}
			if got := noticesOfKind(t, pool, basisAncestor, "resting-root"); len(got) != 1 {
				t.Fatalf("resting-root notices = %#v, want the pending registration's alone", got)
			}
		})
	}
}

// The daemon's own move of the ancestor into merging accepts it, which ends every step waiting on
// its request as R1's first condition ends it. No registration on an ancestor in retro records a
// basis (acceptedBasis), so the step's wait is written as it stands.
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
	waiting := pendingBasis()
	waiting.RegisteredAt = basisEngineNow
	seedGate(t, pool, record.PendOnBasis(record.DesignGate{Issue: gatedRoot, ArtifactID: gateDoc, LatestVersion: 4}, *waiting, "ask-100"))

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
			moved := outboxKindCount(t, pool, gatedRoot, record.OutboxKindRoleMessage)

			apply("own-act", tc.fact)

			gate := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]
			stillOnBasis := tc.name == "the person's approval of the earlier version again"
			if record.GateOpen(gate) != tc.open || gate.OnBasis() != stillOnBasis || !slices.Equal(gate.RestsOn, []string{basisAncestor}) {
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
			if got := outboxKindCount(t, pool, gatedRoot, record.OutboxKindRoleMessage) - moved; got != tc.moved {
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

// The person's approval of the version a step waits on, of an ancestor that is a labelled root on
// the backlog, is that person's handover of the ancestor, and it opens the waiting step as well:
// the ancestor's steps are read apart from the handover's branch.
func TestAPendingBasisOpensOnTheApprovalThatReadmitsAnAncestorOnTheBacklog(t *testing.T) {
	pool := migratedPool(t)
	engine := testEngine(config.DesignGateRootIssues, nil)
	apply := applyFacts(t, pool, engine)
	seedGatedTree(t, pool)
	seedAncestorOnTheBacklog(t, pool)
	apply("register-pending", onBasis(pendingBasis(), 6))
	approved := ancestorReview(intake.DispatchArtifactApproved, 4, 60, "")
	approved.Issue = &dispatch.Issue{Key: basisAncestor, Status: "backlog", HandedOver: true, PrimaryArtifactID: basisDoc}

	apply("ancestor-approved", approved)

	if gate := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]; !gate.OnBasis() || !record.GateOpen(gate) || gate.Basis.Version != 4 {
		t.Fatalf("the spec step = %#v, want it open on version 4 of the document of the ancestor on the backlog", gate)
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
// a closed request ends it, and a request still waiting changes nothing. The person's approval
// opens nothing on an ancestor the seed's read of its issue shows done or retro, or without the
// legion label: the wait ends instead. A read of a wait that has ended since, or of another
// generation, changes nothing.
func TestThePendingBasisReadOpensOrEndsOnlyAStepStillPendingOnIt(t *testing.T) {
	approval := &intake.BasisReview{Mark: record.EventMark{At: basisApprovedAt.Add(24 * time.Hour)}, Actor: basisPerson}
	changes := &intake.BasisReview{Mark: record.EventMark{At: basisApprovedAt.Add(25 * time.Hour)}, Actor: basisPerson, Reason: "name the rollback path"}
	// The seed's read of the ancestor's issue shows it at work and carrying the legion label.
	read := func(edit func(*intake.PendingBasisRead)) intake.PendingBasisRead {
		fact := intake.PendingBasisRead{Issue: gatedRoot, Step: record.DesignStepSpec, Generation: 1, ArtifactID: gateDoc, BasisArtifactID: basisDoc,
			BasisVersion: 4, Ask: "ask-100", LatestVersion: 4, RequestOpen: true, AncestorStatus: "in_progress", AncestorHandedOver: true}
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
		{name: "approved by the person, with the ancestor in Dispatch's retro", fact: read(func(f *intake.PendingBasisRead) {
			f.Approval, f.RequestOpen, f.AncestorStatus = approval, false, "retro"
		}), ended: 1},
		{name: "approved by the person, with the ancestor no longer labelled", fact: read(func(f *intake.PendingBasisRead) {
			f.Approval, f.RequestOpen, f.AncestorHandedOver = approval, false, false
		}), ended: 1},
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

// The pending-basis seed's read of the person's approval opens no step on an ancestor that has
// left R1's first condition since the registration, as the daemon's own store shows it: recorded
// in its retro or its merge, or shown done or in retro by the daemon's mirror of Dispatch. The wait
// ends instead, as the ancestor's own event of that move ends it, with the root's architect told
// why and the step closed on the root's own document with no basis.
func TestThePendingBasisReadOpensNoStepOnAnAcceptedAncestor(t *testing.T) {
	approval := &intake.BasisReview{Mark: record.EventMark{At: basisApprovedAt.Add(24 * time.Hour)}, Actor: basisPerson}
	for _, tc := range []struct {
		name   string
		accept func(t *testing.T, pool *pgxpool.Pool)
		act    string
	}{
		{"the daemon records it in retro", func(t *testing.T, pool *pgxpool.Pool) {
			seedIssue(t, pool, record.Issue{Key: basisAncestor, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
				Tree: basisAncestor, Project: "LEGION", Title: "ancestor", Phase: phase.Retro, Generation: 1, Status: "retro", Rank: "T",
				HandedOver: true, LastDispatchSeq: 1})
		}, "The daemon records LEGION-100 in retro, so LEGION-100 is accepted."},
		{"the mirror of Dispatch shows it done", func(t *testing.T, pool *pgxpool.Pool) { seedMirror(t, pool, basisAncestor, "done") },
			"Dispatch shows LEGION-100 done, so LEGION-100 is accepted."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, _, apply := registeredPending(t)
			tc.accept(t, pool)

			apply("read", intake.PendingBasisRead{Issue: gatedRoot, Step: record.DesignStepSpec, Generation: 1, ArtifactID: gateDoc,
				BasisArtifactID: basisDoc, BasisVersion: 4, Ask: "ask-100", LatestVersion: 4, Approval: approval})

			if gate := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]; gate.Basis != nil || record.GateOpen(gate) || gate.RestsOnIssue(basisAncestor) {
				t.Fatalf("the spec step = %#v, want it closed with no basis, resting on nothing", gate)
			}
			if ended := noticesOfKind(t, pool, gatedRoot, "basis-ended"); len(ended) != 1 || !strings.Contains(ended[0].Reason, tc.act) {
				t.Fatalf("basis-ended notices = %#v, want one naming %q", ended, tc.act)
			}
			if opened := noticesOfKind(t, pool, gatedRoot, "design-approved"); len(opened) != 0 {
				t.Fatalf("design-approved notices = %#v, want none: the step did not open", opened)
			}
		})
	}
}

// The person's approval of the ancestor's version a step waits on, arriving as its live event,
// opens no step on an ancestor that has left R1's first condition as the daemon's own store shows
// it: recorded in its retro, shown done by the daemon's mirror of Dispatch, or recorded without
// the legion label as Dispatch last showed it. The wait ends instead, as the pending-basis seed
// ends it, with the root's architect told why and the step closed on the root's own document with
// no basis.
func TestAnApprovalEventOpensNoPendingStepOnAnAncestorThatLeftR1(t *testing.T) {
	ancestorAt := func(at phase.Phase, status string, labelled bool) func(*testing.T, *pgxpool.Pool) {
		return func(t *testing.T, pool *pgxpool.Pool) {
			seedIssue(t, pool, record.Issue{Key: basisAncestor, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
				Tree: basisAncestor, Project: "LEGION", Title: "ancestor", Phase: at, Generation: 1, Status: status, Rank: "T",
				HandedOver: labelled, LastDispatchSeq: 1})
		}
	}
	for _, tc := range []struct {
		name  string
		leave func(t *testing.T, pool *pgxpool.Pool)
		act   string
	}{
		{"the daemon records it in retro", ancestorAt(phase.Retro, "retro", true), "The daemon records LEGION-100 in retro, so LEGION-100 is accepted."},
		{"the mirror of Dispatch shows it done", func(t *testing.T, pool *pgxpool.Pool) { seedMirror(t, pool, basisAncestor, "done") },
			"Dispatch shows LEGION-100 done, so LEGION-100 is accepted."},
		{"the daemon records it without the legion label", ancestorAt(phase.Planning, "in_progress", false), "LEGION-100 no longer carries the legion label."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, _, apply := registeredPending(t)
			tc.leave(t, pool)

			apply("ancestor-approved", ancestorReview(intake.DispatchArtifactApproved, 4, 60, ""))

			if gate := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]; gate.Basis != nil || record.GateOpen(gate) || gate.RestsOnIssue(basisAncestor) {
				t.Fatalf("the spec step = %#v, want it closed with no basis, resting on nothing", gate)
			}
			if ended := noticesOfKind(t, pool, gatedRoot, "basis-ended"); len(ended) != 1 || !strings.Contains(ended[0].Reason, tc.act) {
				t.Fatalf("basis-ended notices = %#v, want one naming %q", ended, tc.act)
			}
			if opened := noticesOfKind(t, pool, gatedRoot, "design-approved"); len(opened) != 0 {
				t.Fatalf("design-approved notices = %#v, want none: the step did not open", opened)
			}
		})
	}
}

// A row an earlier release moved still names the ancestor's document in its basis columns and
// reads back with no basis (record's basisAsWritten): a release before the basis wrote a person's
// approval over a step pending on ask-100, as a rollback and a roll forward leave it (migration
// 20261114064234). Such a row is no step on that document. It sorts first among the rows that name
// the document and changes nothing: the person's approval of the ancestor's version opens the step
// still pending on the request, and the moved row stays as the earlier release left it.
func TestAPendingBasisOpensBesideARowAnEarlierReleaseMoved(t *testing.T) {
	pool, _, apply := registeredPending(t)
	// LEGION-207 sorts before LEGION-208, whose step waits on ask-100.
	const moved = "LEGION-207"
	seedIssue(t, pool, record.Issue{Key: moved, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: moved, Project: "LEGION", Title: "moved", Phase: phase.Admitted, Generation: 1, Status: "in_progress", Rank: "S",
		HandedOver: true, LastDispatchSeq: 1})
	waiting := pendingBasis()
	waiting.RegisteredAt = basisEngineNow
	seedGate(t, pool, record.PendOnBasis(record.DesignGate{Issue: moved, ArtifactID: "artifact-207", LatestVersion: 4}, *waiting, "ask-100"))
	if _, err := pool.Exec(t.Context(), `update design_gates set approved_version = 4, approved_seq = 50 where issue = $1`, moved); err != nil {
		t.Fatalf("approve the step of %s as an earlier release does: %v", moved, err)
	}
	if gate := gatesOf(t, pool, moved)[record.DesignStepSpec]; gate.Basis != nil {
		t.Fatalf("the step of %s an earlier release moved = %#v, want it read with no basis", moved, gate)
	}

	apply("ancestor-approved", ancestorReview(intake.DispatchArtifactApproved, 4, 60, ""))

	if gate := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]; !gate.OnBasis() || !record.GateOpen(gate) || gate.Basis.Version != 4 {
		t.Fatalf("the spec step of %s = %#v, want it open on version 4 of the ancestor's document", gatedRoot, gate)
	}
	if opened := noticesOfKind(t, pool, gatedRoot, "design-approved"); len(opened) != 1 || opened[0].Version != 4 {
		t.Fatalf("design-approved notices of %s = %#v, want one naming version 4 of the ancestor's document", gatedRoot, opened)
	}
	if gate := gatesOf(t, pool, moved)[record.DesignStepSpec]; gate.Basis != nil || gate.ApprovedVersion == nil || *gate.ApprovedVersion != 4 ||
		gate.ApprovedMark != (record.EventMark{Seq: 50}) {
		t.Fatalf("the step of %s = %#v, want it as the earlier release left it: approved at version 4 with no basis", moved, gate)
	}
	if got := noticesOfKind(t, pool, moved, "design-approved"); len(got) != 0 {
		t.Fatalf("design-approved notices of %s = %#v, want none", moved, got)
	}
}

// A root that leaves the workflow takes its step's wait on the ancestor's request with it
// (dispatch://INFRA-3320, R3 and R8): the ancestor's architect hears that the root left, and
// nothing of the root after that. Moved to the backlog or closed, the root keeps its step closed
// with no basis when the person then approves the ancestor's version, whether that approval comes
// as its live event or as the pending-basis seed's read of it, which the boot queues for each step
// still waiting: the root's issue gets no message, its architect no notice, and the ancestor's
// architect no notice after the one that says the root left. A step still waiting on a root out of
// the workflow, as one written before a leave ended such waits, opens on neither, and its wait
// ends with nobody told.
func TestAPendingBasisOpensNothingOnARootThatLeftTheWorkflow(t *testing.T) {
	approval := &intake.BasisReview{Mark: record.EventMark{At: basisApprovedAt.Add(24 * time.Hour)}, Actor: basisPerson}
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
		for _, delivery := range []struct {
			name string
			fact intake.Fact
		}{
			{"the approval's live event", ancestorReview(intake.DispatchArtifactApproved, 4, 60, "")},
			{"the pending-basis seed's read of the approval", intake.PendingBasisRead{Issue: gatedRoot, Step: record.DesignStepSpec, Generation: 1,
				ArtifactID: gateDoc, BasisArtifactID: basisDoc, BasisVersion: 4, Ask: "ask-100", LatestVersion: 4, Approval: approval,
				AncestorStatus: "in_progress", AncestorHandedOver: true}},
		} {
			t.Run(place.name+", "+delivery.name, func(t *testing.T) {
				pool, engine, apply := registeredPending(t)
				place.leave(t, pool, engine)
				messages := messageBodies(t, pool)
				if told := noticesOfKind(t, pool, basisAncestor, "resting-root"); len(told) != place.told {
					t.Fatalf("resting-root notices once the root is %s = %#v, want %d", place.name, told, place.told)
				}

				apply("ancestor-approved", delivery.fact)

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
// with nobody told when that request closes with no approval, as the person's approval opens
// nothing there (dispatch://INFRA-3320, R3 and R8): the root's tree runs nothing, so neither
// architect has a step to hear of. The root is moved to the backlog in the store, with no leave of
// this release, and the ancestor's close withdraws the request: the step stays closed, registered
// on the root's own document with no basis and resting on nothing, and the close queues no
// basis-ended notice, no resting-root notice, no message and no other row.
func TestAPendingBasisOnARootOutOfTheWorkflowEndsWithNobodyToldWhenItsRequestCloses(t *testing.T) {
	pool, _, apply := registeredPending(t)
	leave(t, pool, gatedRoot, phase.Done, "backlog")
	rows, messages := outboxRows(t, pool), messageBodies(t, pool)
	if told := noticesOfKind(t, pool, basisAncestor, "resting-root"); len(told) != 1 {
		t.Fatalf("resting-root notices once the root is on the backlog = %#v, want the pending registration's alone", told)
	}

	apply("request-withdrawn", intake.DispatchAskClosed{Key: basisAncestor, Ask: "ask-100", Kind: record.AskApproval, Artifact: basisDoc,
		State: "resolved", Actor: basisPerson, Mark: askSeq(61), Withdrawn: true})

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
	if got := outboxRows(t, pool); got != rows {
		t.Fatalf("outbox rows after the close = %d, want the %d before it: the end of the wait queues nothing", got, rows)
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
