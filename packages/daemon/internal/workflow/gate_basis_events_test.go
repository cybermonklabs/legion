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

// ancestorAtWork is LEGION-100's issue as Dispatch shows it at work: in progress and carrying the
// legion label, as R1's first condition asks.
var ancestorAtWork = intake.AncestorRead{Status: "in_progress", HandedOver: true}

// requestOpen is Dispatch's approval of LEGION-100's document while ask-100 asks the person to
// approve version 4 and nobody has reviewed that version.
func requestOpen() dispatch.Approval {
	return dispatch.Approval{IssueKey: basisAncestor, State: "awaiting", LatestVersion: 4, AskID: "ask-100", WaitingOn: "human"}
}

// requestClosed is Dispatch's approval of LEGION-100's document once ask-100 closed with no review
// of version 4.
func requestClosed() dispatch.Approval {
	return dispatch.Approval{IssueKey: basisAncestor, State: "draft", LatestVersion: 4}
}

// latestOf is Dispatch's approval of LEGION-100's document once act, an event of it, is the last
// thing done to it: act's review as the latest review of its version, or for a version event that
// version as the latest, which moves ask-100 to its agent.
func latestOf(act intake.DispatchArtifact) dispatch.Approval {
	if act.Kind == intake.DispatchArtifactVersion {
		return dispatch.Approval{IssueKey: basisAncestor, State: "awaiting", LatestVersion: act.Version, AskID: "ask-100", WaitingOn: "agent"}
	}
	approval := dispatch.Approval{IssueKey: basisAncestor, State: string(act.Kind), LatestVersion: act.Version, Version: new(act.Version),
		At: act.Mark.At, Seq: act.Mark.Seq, By: act.Actor}
	if act.Reason != "" {
		approval.Reason = new(act.Reason)
	}
	return approval
}

// seedRead is the pending-basis seed's read (intake.PendingBasisRead) of LEGION-208's spec step
// waiting on ask-100 at version 4, as registeredPending registers it, when Dispatch shows approval
// for LEGION-100's document and ancestor for its issue.
func seedRead(approval dispatch.Approval, ancestor intake.AncestorRead) intake.PendingBasisRead {
	return intake.PendingBasisRead{Issue: gatedRoot, Step: record.DesignStepSpec, Generation: 1, ArtifactID: gateDoc, BasisArtifactID: basisDoc,
		BasisVersion: 4, Ask: "ask-100", RegisteredAt: basisEngineNow, Read: intake.BasisRead{Approval: approval, Ancestor: ancestor}}
}

// closedBy is the pending-basis seed's read (seedRead) once actor closed ask-100 with no review of
// version 4 (requestClosed), with ancestor for LEGION-100's issue: the seed reads that close by the
// request's id as well (intake.BasisRead.Closed), resolved by actor.
func closedBy(actor dispatch.Actor, ancestor intake.AncestorRead) intake.PendingBasisRead {
	read := seedRead(requestClosed(), ancestor)
	read.Read.Closed = &intake.BasisClose{Actor: actor, State: "resolved"}
	return read
}

// closedSentence is the act that ends LEGION-208's wait once closer, an actor's name or the words
// for none, closed ask-100 with no approval of version 4 (closedBy).
func closedSentence(closer string) string {
	return closer + " closed approval request ask-100 of version 4 of the primary document of LEGION-100 (dispatch://LEGION-100/spec), as resolved, with no approval of that version."
}

// currentRead is the seed's read (seedRead) when act is the last thing done to LEGION-100's
// document (latestOf) and LEGION-100 is at work.
func currentRead(act intake.DispatchArtifact) intake.PendingBasisRead {
	return seedRead(latestOf(act), ancestorAtWork)
}

// theSeed is the pending-basis seed of LEGION-208's spec step waiting on ask-100 at version 4, as
// registeredPending registers it: the one row its registration and each cue for it queue.
var theSeed = record.PendingBasisSeed{Step: record.DesignStepSpec, ArtifactID: gateDoc, BasisArtifactID: basisDoc, BasisVersion: 4, Ask: "ask-100",
	Generation: 1, RegisteredAt: basisEngineNow}

// basisTold is how many notices tell of LEGION-208's step on the ancestor's approval: the root's
// design-approved, basis-ended and basis-changes-requested notices, and the ancestor's
// resting-root notices.
func basisTold(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	told := len(noticesOfKind(t, pool, basisAncestor, "resting-root"))
	for _, kind := range []record.NoticeKind{"design-approved", "basis-ended", "basis-changes-requested"} {
		told += len(noticesOfKind(t, pool, gatedRoot, kind))
	}
	return told
}

// cueThenRead applies fact under id, an event that is a cue for LEGION-208's spec step waiting on
// ask-100 (pendingBasisCue), and then read under id-read, the read of Dispatch the pending-basis
// seed the event queued makes. The event queues that one seed (theSeed) and moves nothing: the step
// still waits on ask-100 at version 4, and nobody is told anything of it. The read then decides.
func cueThenRead(t *testing.T, pool *pgxpool.Pool, apply func(string, intake.Fact) intake.Result, id string, fact intake.Fact, read intake.PendingBasisRead) {
	t.Helper()
	seeds, told := pendingSeeds(t, pool, gatedRoot), basisTold(t, pool)

	apply(id, fact)

	if gate := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]; !gate.PendingOnBasis() || gate.Basis.Pending.Ask != "ask-100" ||
		gate.Basis.Version != 4 || record.GateOpen(gate) {
		t.Fatalf("the spec step after %s = %#v, want it still closed and pending on ask-100 at version 4: an event opens and ends nothing", id, gate)
	}
	if queued := pendingSeeds(t, pool, gatedRoot); len(queued) != len(seeds)+1 || queued[len(queued)-1] != theSeed {
		t.Fatalf("pending-basis seeds after %s = %#v, want the %d before it and the cue's %#v", id, queued, len(seeds), theSeed)
	}
	if now := basisTold(t, pool); now != told {
		t.Fatalf("notices of the step on the basis after %s = %d, want the %d before it: an event tells nothing", id, now, told)
	}

	apply(id+"-read", read)
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
// registration on that approval would, once the pending-basis seed the approval's event queues
// reads that approval as the latest review of the latest version: the step stands open at the
// version registered on the approval, rests on the ancestor, and the root's issue gets the message
// that records the basis, its architect a design-approved notice naming the ancestor's version as
// the basis, the ancestor's architect the root's first rest, and the gate seed is queued. The same
// approval delivered again tells nobody anything new, and admission's pass then starts the tree.
func TestAPendingBasisOpensOnThePersonsApprovalOfTheAncestorsVersion(t *testing.T) {
	pool, engine, apply := registeredPending(t)
	approved := ancestorReview(intake.DispatchArtifactApproved, 4, 60, "")

	cueThenRead(t, pool, apply, "ancestor-approved", approved, currentRead(approved))

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

// Each act R1 names ends a pending row, as the read of Dispatch the pending-basis seed its event
// queues shows it: a changes request on the ancestor's version, a newer version of the ancestor's
// document, a session's close of the request, the person's close of it with no review, the
// withdrawal of the request that the ancestor's close records (the ask event Dispatch appends
// before the ancestor's own issue.closed), the ancestor done or in Dispatch's retro, and the
// ancestor losing the legion label; and a newer version or a changes request of the root's own
// document, which ends the wait as its own event. The seed reads the close of the request by its
// id, so the end of a close names whoever closed it, as the end's actor too, and, where the
// ancestor has left R1's first condition, that leave first. The step stays closed, registered on
// the root's own document with no basis and resting on nothing, the root's architect gets a
// basis-ended notice that names the act and who made it, and the ancestor's architect a
// resting-root notice of the end: also where the ancestor, set done, has left Legion's work by
// the time the seed reads, as a root or as a child of another issue, since the daemon still
// records it on a known chain (ancestorRecorded). An ancestor the daemon does not record is told
// nothing, of the registration or of the end.
func TestEachActOnAPendingBasisEndsItWithABasisEndedNotice(t *testing.T) {
	session := dispatch.Actor{Kind: "session", ID: "session-other"}
	closed := func(actor dispatch.Actor) intake.DispatchAskClosed {
		return intake.DispatchAskClosed{Key: basisAncestor, Ask: "ask-100", Kind: record.AskApproval, Artifact: basisDoc, State: "resolved",
			Actor: actor, Mark: askSeq(61)}
	}
	ancestorAt := func(status string, labelled bool) intake.DispatchIssue {
		return intake.DispatchIssue{Key: basisAncestor, Seq: 5, Type: "issue.updated", Status: status, Title: "ancestor", Rank: "T", HandedOver: labelled}
	}
	shown := func(status string, labelled bool) *intake.PendingBasisRead {
		return new(seedRead(requestOpen(), intake.AncestorRead{Status: status, HandedOver: labelled}))
	}
	done := intake.AncestorRead{Status: record.DoneStatus, HandedOver: true}
	changes := ancestorReview(intake.DispatchArtifactChangesRequested, 4, 61, "name the rollback path")
	newer := ancestorReview(intake.DispatchArtifactVersion, 5, 61, "")
	for _, tc := range []struct {
		name string
		fact intake.Fact
		// read is the pending-basis seed's read the event's seed makes, nil for an event of the
		// root's own document, which ends the wait itself.
		read *intake.PendingBasisRead
		act  string
		// actor is who made the act, as the basis-ended notice names it ("" for nobody).
		actor   string
		version int
		changes int
		// member records LEGION-100 as a child of another issue in Legion's work, LEGION-090, on
		// its Dispatch chain, rather than as a root.
		member bool
		// unrecorded leaves LEGION-100 an issue the daemon does not record, so its chain is
		// unknown, from before the registration.
		unrecorded bool
	}{
		{name: "a changes request on the ancestor's version", fact: changes, read: new(currentRead(changes)),
			act: `user:alice requested changes on version 4 of the primary document of LEGION-100 (dispatch://LEGION-100/spec): "name the rollback path".`, actor: "user:alice"},
		{name: "a newer version of the ancestor's document", fact: newer, read: new(currentRead(newer)),
			act: "Version 5 of the primary document of LEGION-100 (dispatch://LEGION-100/spec) is newer than version 4"},
		{name: "a session's close of the request", fact: closed(session), read: new(closedBy(session, ancestorAtWork)),
			act: closedSentence("session:session-other"), actor: "session:session-other"},
		{name: "the person's close of the request with no review", fact: closed(basisPerson), read: new(closedBy(basisPerson, ancestorAtWork)),
			act: closedSentence("user:alice"), actor: "user:alice"},
		{name: "the ancestor's close, which withdraws the request", fact: closed(basisPerson), read: new(closedBy(basisPerson, done)),
			act: "Dispatch shows LEGION-100 done, so LEGION-100 is accepted. " + closedSentence("user:alice"), actor: "user:alice"},
		// The withdrawal read after the ancestor's reopen, which left ask-100 closed: the read shows
		// the ancestor at work, so the end names no acceptance of it.
		{name: "the ancestor's close, which withdraws the request, read after its reopen", fact: closed(basisPerson),
			read: new(closedBy(basisPerson, ancestorAtWork)), act: closedSentence("user:alice"), actor: "user:alice"},
		{name: "the ancestor done", fact: ancestorAt("done", true), read: shown("done", true), act: "Dispatch shows LEGION-100 done"},
		{name: "the ancestor done, a child of another issue at work", fact: intake.DispatchIssue{Key: basisAncestor, Seq: 5, Type: "issue.updated",
			Status: "done", Title: "ancestor", Rank: "T", HandedOver: true, Parent: "LEGION-090"}, read: shown("done", true),
			act: "Dispatch shows LEGION-100 done", member: true},
		{name: "the ancestor done, an issue the daemon does not record", fact: ancestorAt("done", true), read: shown("done", true),
			act: "Dispatch shows LEGION-100 done", unrecorded: true},
		{name: "the ancestor in Dispatch's retro", fact: ancestorAt("retro", true), read: shown("retro", true), act: "Dispatch shows LEGION-100 retro"},
		{name: "the ancestor without the legion label", fact: ancestorAt("in_progress", false), read: shown("in_progress", false),
			act: "LEGION-100 no longer carries the legion label."},
		{name: "a newer version of the root's own document", fact: intake.DispatchArtifact{Key: gatedRoot, ArtifactID: gateDoc,
			Kind: intake.DispatchArtifactVersion, Version: 5, Mark: askSeq(9)}, act: "Version 5 of the primary document of LEGION-208", version: 5},
		{name: "a changes request on the root's own document", fact: intake.DispatchArtifact{Key: gatedRoot, ArtifactID: gateDoc,
			Kind: intake.DispatchArtifactChangesRequested, Version: 4, Reason: "say more", Actor: basisPerson, Mark: askSeq(9)},
			act:   `user:alice requested changes on version 4 of the primary document of LEGION-208 (dispatch://LEGION-208/spec), the root's own document: "say more".`,
			actor: "user:alice", changes: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var pool *pgxpool.Pool
			var apply func(string, intake.Fact) intake.Result
			if tc.unrecorded {
				pool = migratedPool(t)
				apply = applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
				seedGatedTree(t, pool)
				apply("register-pending", onBasis(pendingBasis(), 6))
				if gate := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]; !gate.PendingOnBasis() {
					t.Fatalf("the spec step after the pending registration on an ancestor the daemon does not record = %#v, want it pending", gate)
				}
			} else {
				pool, _, apply = registeredPending(t)
			}
			if tc.member {
				seedIssue(t, pool, record.Issue{Key: "LEGION-090", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
					Tree: "LEGION-090", Project: "LEGION", Title: "tree", Phase: phase.Planning, Generation: 1, Status: "in_progress", Rank: "S",
					HandedOver: true, LastDispatchSeq: 1})
				seedIssue(t, pool, record.Issue{Key: basisAncestor, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
					Tree: "LEGION-090", Parent: new("LEGION-090"), Project: "LEGION", Title: "ancestor", Phase: phase.Planning, Generation: 1,
					Status: "in_progress", Rank: "T", HandedOver: true, LastDispatchSeq: 1})
			}

			if tc.read != nil {
				cueThenRead(t, pool, apply, "act", tc.fact, *tc.read)
			} else {
				apply("act", tc.fact)
			}

			gate := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]
			version := max(tc.version, 4)
			if gate.Basis != nil || record.GateOpen(gate) || gate.ArtifactID != gateDoc || gate.LatestVersion != version || gate.RestsOnIssue(basisAncestor) {
				t.Fatalf("the spec step = %#v, want it closed at version %d on the root's own document, with no basis and resting on nothing", gate, version)
			}
			ended := noticesOfKind(t, pool, gatedRoot, "basis-ended")
			if len(ended) != 1 || ended[0].Ref != "dispatch://LEGION-100/spec" || ended[0].Version != 4 || ended[0].Role != claim.RoleArchitect ||
				ended[0].Actor != tc.actor || !strings.Contains(ended[0].Reason, tc.act) ||
				!strings.Contains(ended[0].Reason, "no longer waits on approval request ask-100 of version 4") {
				t.Fatalf("basis-ended notices = %#v, want one naming %q, made by %q, and the end of the wait on ask-100", ended, tc.act, tc.actor)
			}
			resting := noticesOfKind(t, pool, basisAncestor, "resting-root")
			switch {
			case tc.unrecorded && len(resting) != 0:
				t.Fatalf("resting-root notices = %#v, want none: the daemon records no tree of the ancestor", resting)
			case !tc.unrecorded && (len(resting) != 2 || !strings.Contains(resting[1].Reason, tc.act)):
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
// rules: the close of the request it waits on and the ancestor's acceptance queue no seed, and a
// seed's read of Dispatch that shows either, as one queued before the deployment changed reads it,
// leaves the wait as it is and tells nobody.
func TestAPendingBasisOfAStepTheDeploymentDoesNotGateTakesNoEvent(t *testing.T) {
	for _, tc := range []struct {
		name string
		fact intake.Fact
		read intake.PendingBasisRead
	}{
		{"a session's close of the request", intake.DispatchAskClosed{Key: basisAncestor, Ask: "ask-100", Kind: record.AskApproval, Artifact: basisDoc,
			State: "resolved", Actor: dispatch.Actor{Kind: "session", ID: "session-other"}, Mark: askSeq(61)}, closedBy(dispatch.Actor{Kind: "session", ID: "session-other"}, ancestorAtWork)},
		{"the ancestor done", intake.DispatchIssue{Key: basisAncestor, Seq: 5, Type: "issue.updated", Status: "done", Title: "ancestor", Rank: "T", HandedOver: true},
			seedRead(requestOpen(), intake.AncestorRead{Status: record.DoneStatus, HandedOver: true})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, _, _ := registeredPending(t)
			// The next boot gates the criteria step alone.
			criteriaOnly := applyFacts(t, pool, stepsEngine([]record.DesignStep{record.DesignStepCriteria}, nil))

			criteriaOnly("act", tc.fact)
			if seeds := pendingSeeds(t, pool, gatedRoot); len(seeds) != 1 {
				t.Fatalf("pending-basis seeds after the event = %#v, want the registration's alone", seeds)
			}
			criteriaOnly("read", tc.read)

			if gate := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]; !gate.PendingOnBasis() || gate.Basis.Pending.Ask != "ask-100" {
				t.Fatalf("the spec step = %#v, want it still pending on ask-100: the deployment does not gate it", gate)
			}
			if got := noticesOfKind(t, pool, gatedRoot, "basis-ended"); len(got) != 0 {
				t.Fatalf("basis-ended notices = %#v, want none for a step the deployment does not gate", got)
			}
			if got := noticesOfKind(t, pool, basisAncestor, "resting-root"); len(got) != 1 {
				t.Fatalf("resting-root notices = %#v, want the pending registration's alone", got)
			}
			if seeds := pendingSeeds(t, pool, gatedRoot); len(seeds) != 1 {
				t.Fatalf("pending-basis seeds = %#v, want the registration's alone", seeds)
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
// ancestor's approval, as the pending-basis seed its event queues reads it, answers the root's wait
// as an approval does, and the end of the step's wait answers it as a changes request does, with
// no notice beside either; the end's answer names what ended the wait for the resume. A person's
// approval of the root's own document answers it too, and sends neither notice. The ancestor's
// architect is told the end as ever.
func TestAPendingBasisAnswersARootAwaitingAPersonWithNoNotice(t *testing.T) {
	for _, wait := range []struct {
		name    string
		waiting record.Awaiting
	}{
		{"on a question of its own", record.Awaiting{Ask: "ask-q", Since: record.EventMark{Seq: 5}}},
		{"on its own approval request", record.Awaiting{Artifact: gateDoc, GateVersion: 4, Since: record.EventMark{Seq: 5}}},
	} {
		approved := ancestorReview(intake.DispatchArtifactApproved, 4, 60, "")
		changes := ancestorReview(intake.DispatchArtifactChangesRequested, 4, 61, "name the rollback path")
		for _, tc := range []struct {
			name string
			fact intake.Fact
			// read is the read the pending-basis seed the ancestor's event queues makes, nil for
			// an event of the root's own document.
			read   *intake.PendingBasisRead
			by     record.Answer
			detail string
			open   bool
		}{
			{name: "the ancestor's approval", fact: approved, read: new(currentRead(approved)), by: record.AnsweredByApproval, open: true},
			{name: "the ancestor's changes request", fact: changes, read: new(currentRead(changes)),
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

				if tc.read != nil {
					cueThenRead(t, pool, apply, "act", tc.fact, *tc.read)
				} else {
					apply("act", tc.fact)
				}

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
// the backlog, is that person's handover of the ancestor, and its event queues the waiting step's
// pending-basis seed as well, whose read of that approval opens the step: the ancestor's steps are
// read apart from the handover's branch.
func TestAPendingBasisOpensOnTheApprovalThatReadmitsAnAncestorOnTheBacklog(t *testing.T) {
	pool := migratedPool(t)
	engine := testEngine(config.DesignGateRootIssues, nil)
	apply := applyFacts(t, pool, engine)
	seedGatedTree(t, pool)
	seedAncestorOnTheBacklog(t, pool)
	apply("register-pending", onBasis(pendingBasis(), 6))
	approved := ancestorReview(intake.DispatchArtifactApproved, 4, 60, "")
	approved.Issue = &dispatch.Issue{Key: basisAncestor, Status: "backlog", HandedOver: true, PrimaryArtifactID: basisDoc}

	cueThenRead(t, pool, apply, "ancestor-approved", approved, seedRead(latestOf(approved), intake.AncestorRead{Status: "backlog", HandedOver: true}))

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

// The pending-basis seed's read is judged by the one verdict (judgeBasisRead), and only for a step
// of that generation still pending on that request: the person's approval of the version, as
// Dispatch's latest review of its latest version, opens it at that review's place in the
// ancestor's event log; a changes request as the latest review ends it, also where the person
// approved the version before (decision E1: Dispatch's read shows the latest review alone, so the
// seed no longer opens on an earlier approval and passes the changes request on); a
// newer version or a closed request ends it; and a request still waiting changes nothing. The
// person's approval opens nothing on an ancestor the read shows done or retro, or without the
// legion label: the wait ends instead. A read of a wait that has ended since, or of another
// generation, request or registration, changes nothing.
func TestThePendingBasisReadOpensOrEndsOnlyAStepStillPendingOnIt(t *testing.T) {
	approvedAt, sentBackAt := basisApprovedAt.Add(24*time.Hour), basisApprovedAt.Add(25*time.Hour)
	approved := dispatch.Approval{IssueKey: basisAncestor, State: "approved", LatestVersion: 4, Version: new(4), At: approvedAt, Seq: 60, By: basisPerson}
	sentBack := dispatch.Approval{IssueKey: basisAncestor, State: "changes_requested", LatestVersion: 4, Version: new(4), Reason: new("name the rollback path"),
		At: sentBackAt, Seq: 61, By: basisPerson}
	read := func(approval dispatch.Approval, edit func(*intake.PendingBasisRead)) intake.PendingBasisRead {
		fact := seedRead(approval, ancestorAtWork)
		edit(&fact)
		return fact
	}
	as := func(*intake.PendingBasisRead) {}
	newer := requestOpen()
	newer.LatestVersion, newer.WaitingOn = 5, "agent"
	for _, tc := range []struct {
		name    string
		fact    intake.PendingBasisRead
		open    bool
		pending bool
		ended   string
	}{
		{name: "approved by the person", fact: read(approved, as), open: true},
		{name: "approved and then sent back", fact: read(sentBack, as),
			ended: `user:alice requested changes on version 4 of the primary document of LEGION-100 (dispatch://LEGION-100/spec): "name the rollback path".`},
		{name: "approved by the person, with the ancestor in Dispatch's retro", fact: read(approved, func(f *intake.PendingBasisRead) {
			f.Read.Ancestor.Status = "retro"
		}), ended: "Dispatch shows LEGION-100 retro, so LEGION-100 is accepted."},
		{name: "approved by the person, with the ancestor no longer labelled", fact: read(approved, func(f *intake.PendingBasisRead) {
			f.Read.Ancestor.HandedOver = false
		}), ended: "LEGION-100 no longer carries the legion label."},
		{name: "a newer version", fact: read(newer, as), ended: "Version 5 of the primary document of LEGION-100 (dispatch://LEGION-100/spec) is newer than version 4"},
		{name: "the request closed with no approval", fact: read(requestClosed(), func(f *intake.PendingBasisRead) {
			f.Read.Closed = &intake.BasisClose{Actor: basisPerson, State: "resolved"}
		}), ended: closedSentence("user:alice")},
		{name: "still waiting", fact: read(requestOpen(), as), pending: true},
		{name: "another generation", fact: read(approved, func(f *intake.PendingBasisRead) { f.Generation = 2 }), pending: true},
		{name: "another request", fact: read(approved, func(f *intake.PendingBasisRead) { f.Ask = "ask-99" }), pending: true},
		{name: "an earlier registration on the same request", fact: read(approved, func(f *intake.PendingBasisRead) {
			f.RegisteredAt = f.RegisteredAt.Add(-time.Hour)
		}), pending: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, _, apply := registeredPending(t)

			apply("read", tc.fact)

			gate := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]
			if record.GateOpen(gate) != tc.open || gate.PendingOnBasis() != tc.pending || gate.OnBasis() != tc.open {
				t.Fatalf("the spec step = %#v, want open=%t pending=%t", gate, tc.open, tc.pending)
			}
			if tc.open && gate.Basis.Approval != (record.EventMark{Seq: 60, At: approvedAt}) {
				t.Fatalf("the basis = %#v, want the approval the read found", gate.Basis)
			}
			ended := noticesOfKind(t, pool, gatedRoot, "basis-ended")
			if (tc.ended == "") != (len(ended) == 0) || len(ended) > 1 || tc.ended != "" && !strings.Contains(ended[0].Reason, tc.ended) {
				t.Fatalf("basis-ended notices = %#v, want one naming %q, or none", ended, tc.ended)
			}
			if got := noticesOfKind(t, pool, gatedRoot, "basis-changes-requested"); len(got) != 0 {
				t.Fatalf("basis-changes-requested notices = %#v, want none: no step opened before the changes request", got)
			}
			if opened := noticesOfKind(t, pool, gatedRoot, "design-approved"); (len(opened) == 1) != tc.open || len(opened) > 1 {
				t.Fatalf("design-approved notices = %#v, want one only for an opening", opened)
			}
		})
	}

	t.Run("a wait that ended since", func(t *testing.T) {
		pool, _, apply := registeredPending(t)
		newer := ancestorReview(intake.DispatchArtifactVersion, 5, 61, "")
		cueThenRead(t, pool, apply, "ancestor-v5", newer, currentRead(newer))
		apply("read", read(approved, as))
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
// ends instead, with the root's architect told why and the step closed on the root's own document
// with no basis.
func TestThePendingBasisReadOpensNoStepOnAnAcceptedAncestor(t *testing.T) {
	approved := dispatch.Approval{IssueKey: basisAncestor, State: "approved", LatestVersion: 4, Version: new(4), At: basisApprovedAt.Add(24 * time.Hour),
		Seq: 60, By: basisPerson}
	for _, tc := range []struct {
		name   string
		accept func(t *testing.T, pool *pgxpool.Pool)
		act    string
	}{
		// The daemon moved it into retro while the board still shows needs_review: the record is
		// what answers.
		{"the daemon records it in retro", func(t *testing.T, pool *pgxpool.Pool) {
			seedIssue(t, pool, record.Issue{Key: basisAncestor, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
				Tree: basisAncestor, Project: "LEGION", Title: "ancestor", Phase: phase.Retro, Generation: 1, Status: "retro", DispatchStatus: "needs_review", Rank: "T",
				HandedOver: true, LastDispatchSeq: 1})
		}, "The daemon records LEGION-100 in retro, so LEGION-100 is accepted."},
		// Above the sequence of the copy row the live ancestor's seed wrote.
		{"the mirror of Dispatch shows it done", func(t *testing.T, pool *pgxpool.Pool) { mirrorChild(t, pool, basisAncestor, "", "done", 5) },
			"Dispatch shows LEGION-100 done, so LEGION-100 is accepted."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, _, apply := registeredPending(t)
			tc.accept(t, pool)

			apply("read", seedRead(approved, ancestorAtWork))

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

// The pending-basis seed that ends a wait on an ancestor that has left Legion's work since the
// registration (it closed as done, or went to the backlog and lost the legion label) runs after
// that ancestor's own leave. The end of the wait is still told to the ancestor's architect as a
// resting-root notice, which the outbox delivers by the owner walk and finishes undelivered where
// no architect holds its role; a wait on an ancestor the daemon does not record tells nobody.
func TestTheEndOfAPendingWaitIsToldToAnAncestorThatLeftLegionsWork(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		leave    func(t *testing.T, pool *pgxpool.Pool)
		ancestor intake.AncestorRead
		told     int
	}{
		{"the ancestor closed as done", func(t *testing.T, pool *pgxpool.Pool) {
			seedIssue(t, pool, record.Issue{Key: basisAncestor, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
				Tree: basisAncestor, Project: "LEGION", Title: "ancestor", Phase: phase.Done, Generation: 1, Status: "done", Rank: "T",
				HandedOver: true, LastDispatchSeq: 2})
		}, intake.AncestorRead{Status: record.DoneStatus, HandedOver: true}, 2},
		{"the ancestor on the backlog without the legion label", func(t *testing.T, pool *pgxpool.Pool) { seedAncestorOnTheBacklog(t, pool) },
			intake.AncestorRead{Status: "backlog", HandedOver: false}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, _, apply := registeredPending(t)
			tc.leave(t, pool)

			apply("read", seedRead(requestOpen(), tc.ancestor))

			ended := noticesOfKind(t, pool, gatedRoot, "basis-ended")
			if len(ended) != 1 {
				t.Fatalf("basis-ended notices = %#v, want the one that ended the wait", ended)
			}
			resting := noticesOfKind(t, pool, basisAncestor, "resting-root")
			if len(resting) != tc.told || !strings.HasPrefix(resting[tc.told-1].Reason, ended[0].Reason) {
				t.Fatalf("resting-root notices = %#v, want the registration's and an end that tells %q", resting, ended[0].Reason)
			}
		})
	}

	t.Run("an ancestor the daemon does not record", func(t *testing.T) {
		pool := migratedPool(t)
		engine := testEngine(config.DesignGateRootIssues, nil)
		apply := applyFacts(t, pool, engine)
		seedGatedTree(t, pool)
		apply("register-pending", onBasis(pendingBasis(), 6))

		apply("read", closedBy(basisPerson, ancestorAtWork))

		if got := noticesOfKind(t, pool, gatedRoot, "basis-ended"); len(got) != 1 {
			t.Fatalf("basis-ended notices = %#v, want the one that ended the wait", got)
		}
		if got := noticesOfKind(t, pool, basisAncestor, "resting-root"); len(got) != 0 {
			t.Fatalf("resting-root notices = %#v, want none for an ancestor the daemon does not record", got)
		}
	})
}

// The person's approval of the ancestor's version a step waits on, arriving as its live event,
// queues the step's pending-basis seed, whose read opens no step on an ancestor that has left R1's
// first condition: as the daemon's own store shows it, recorded in its retro or shown done by the
// daemon's mirror of Dispatch, or as the read shows it, without the legion label. The wait ends
// instead, with the root's architect told why and the step closed on the root's own document with
// no basis.
func TestAnApprovalEventOpensNoPendingStepOnAnAncestorThatLeftR1(t *testing.T) {
	for _, tc := range []struct {
		name     string
		leave    func(t *testing.T, pool *pgxpool.Pool)
		ancestor intake.AncestorRead
		act      string
	}{
		// The daemon moved it into retro while the board still shows needs_review: the record is
		// what answers.
		{"the daemon records it in retro", func(t *testing.T, pool *pgxpool.Pool) {
			seedIssue(t, pool, record.Issue{Key: basisAncestor, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
				Tree: basisAncestor, Project: "LEGION", Title: "ancestor", Phase: phase.Retro, Generation: 1, Status: "retro", DispatchStatus: "needs_review", Rank: "T",
				HandedOver: true, LastDispatchSeq: 1})
		}, ancestorAtWork, "The daemon records LEGION-100 in retro, so LEGION-100 is accepted."},
		// Above the sequence of the copy row the live ancestor's seed wrote.
		{"the mirror of Dispatch shows it done", func(t *testing.T, pool *pgxpool.Pool) { mirrorChild(t, pool, basisAncestor, "", "done", 5) },
			ancestorAtWork, "Dispatch shows LEGION-100 done, so LEGION-100 is accepted."},
		{"the read shows it without the legion label", func(*testing.T, *pgxpool.Pool) {}, intake.AncestorRead{Status: "in_progress"},
			"LEGION-100 no longer carries the legion label."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, _, apply := registeredPending(t)
			tc.leave(t, pool)
			approved := ancestorReview(intake.DispatchArtifactApproved, 4, 60, "")

			cueThenRead(t, pool, apply, "ancestor-approved", approved, seedRead(latestOf(approved), tc.ancestor))

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
// the document and changes nothing: the person's approval of the ancestor's version queues the
// pending-basis seed of the step still pending on the request, and none for the moved row, and
// the seed's read opens that step, while the moved row stays as the earlier release left it.
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

	approved := ancestorReview(intake.DispatchArtifactApproved, 4, 60, "")
	cueThenRead(t, pool, apply, "ancestor-approved", approved, currentRead(approved))
	if seeds := pendingSeeds(t, pool, moved); len(seeds) != 0 {
		t.Fatalf("pending-basis seeds of %s = %#v, want none: its row is no step on the document", moved, seeds)
	}

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
