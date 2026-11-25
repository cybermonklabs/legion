package workflow

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The events of an ancestor's document, request and issue can reach a step that waits on the
// ancestor's approval request late: main's #4590 replays a record the intake gave up on through
// the handling a live delivery takes, after later events applied (dispatch://INFRA-4728). Such an
// event opens and ends nothing itself: it queues the step's pending-basis seed, and only the
// seed's read of Dispatch as it stands now (intake.PendingBasisRead) opens or ends the wait. Each
// case here applies, after the pending registration, an event a later act undid, which queues one
// seed and moves nothing (cueThenRead), and then that seed's read. The first three cases' reads
// show the request still open, and the step must stay pending; the fourth's shows the changes
// request that superseded the approval, so the step's wait ends on it, and that changes request,
// applied after, adds nothing. Each control applies an event and the read that shows its act, and
// the step must move. So the cases fail where the engine acts on the event, and the controls where
// it acts on nothing.

// stillPending fails t unless LEGION-208's spec step still waits on ask-100 at version 4 after
// what, with nothing opened or ended, nobody told, and one pending-basis seed queued beside the
// registration's: the late event's.
func stillPending(t *testing.T, pool *pgxpool.Pool, what string) {
	t.Helper()
	if gate := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]; !gate.PendingOnBasis() || gate.Basis.Pending.Ask != "ask-100" ||
		gate.Basis.Version != 4 || record.GateOpen(gate) {
		t.Errorf("the spec step after %s = %#v, want it still closed and pending on ask-100 at version 4", what, gate)
	}
	for _, kind := range []record.NoticeKind{"basis-ended", "design-approved", "basis-changes-requested"} {
		if got := noticesOfKind(t, pool, gatedRoot, kind); len(got) != 0 {
			t.Errorf("%s notices after %s = %#v, want none: the late event moved nothing", kind, what, got)
		}
	}
	if got := noticesOfKind(t, pool, basisAncestor, "resting-root"); len(got) != 1 {
		t.Errorf("resting-root notices after %s = %#v, want the pending registration's alone", what, got)
	}
	if seeds := pendingSeeds(t, pool, gatedRoot); len(seeds) != 2 || seeds[1] != theSeed {
		t.Errorf("pending-basis seeds after %s = %#v, want the registration's and the late event's", what, seeds)
	}
}

// ended fails t unless LEGION-208's spec step no longer waits and stands closed on its own
// document with no basis, after one basis-ended notice naming act, and nothing opened.
func ended(t *testing.T, pool *pgxpool.Pool, act string) {
	t.Helper()
	if gate := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]; gate.Basis != nil || record.GateOpen(gate) || gate.ArtifactID != gateDoc {
		t.Errorf("the spec step = %#v, want it closed on the root's own document with no basis", gate)
	}
	if got := noticesOfKind(t, pool, gatedRoot, "basis-ended"); len(got) != 1 || !strings.Contains(got[0].Reason, act) {
		t.Errorf("basis-ended notices = %#v, want one naming %q", got, act)
	}
	if got := noticesOfKind(t, pool, gatedRoot, "design-approved"); len(got) != 0 {
		t.Errorf("design-approved notices = %#v, want none: the step did not open", got)
	}
}

// The tester's three late events (dispatch://INFRA-4728's probe) and the fourth, each applied
// after the pending registration, then the read of Dispatch the seed it queues makes. Before the
// registration, the person asked for changes on version 4 at sequence 3, and the architect then
// asked for approval of version 4 again, as ask-100: so Dispatch shows ask-100 open, with that
// changes request as the latest review (latestThree).
//
//   - An approval of version 4 older than that changes request, given up then and replayed now,
//     opens nothing: Dispatch's latest review is no approval.
//   - That changes request itself, older than ask-100, replayed now, ends nothing: ask-100 is open.
//   - The withdrawal of ask-100 that the ancestor's close recorded, given up, replayed after the
//     ancestor's reopen opened ask-100 again (Dispatch records no restore of an approval request:
//     the reopen's own ask.opened is the event), ends nothing: the read shows ask-100 open and the
//     ancestor at work.
//   - The person's approval at 60, delivered after their changes request at 62 superseded it,
//     opens nothing, and the seed's read ends the wait on the changes request it shows; that
//     changes request, applied after, adds nothing.
func TestALateEventOnAPendingBasisChangesNothingDispatchNoLongerShows(t *testing.T) {
	reason := "name the rollback path"
	latestThree := requestOpen()
	latestThree.Version, latestThree.Reason, latestThree.By = new(4), &reason, basisPerson
	latestThree.Seq, latestThree.At = 3, ancestorReview(intake.DispatchArtifactChangesRequested, 4, 3, reason).Mark.At
	withdrawal := intake.DispatchAskClosed{Key: basisAncestor, Ask: "ask-100", Kind: record.AskApproval, Artifact: basisDoc, State: "resolved",
		Actor: basisPerson, Mark: askSeq(61)}

	t.Run("an approval of the version older than the person's changes request on it", func(t *testing.T) {
		pool, _, apply := registeredPending(t)

		cueThenRead(t, pool, apply, "late", ancestorReview(intake.DispatchArtifactApproved, 4, 2, ""), seedRead(latestThree, ancestorAtWork))

		stillPending(t, pool, "the late approval")
	})

	t.Run("a changes request on the version older than the request the step waits on", func(t *testing.T) {
		pool, _, apply := registeredPending(t)

		cueThenRead(t, pool, apply, "late", ancestorReview(intake.DispatchArtifactChangesRequested, 4, 3, reason), seedRead(latestThree, ancestorAtWork))

		stillPending(t, pool, "the late changes request")
	})

	t.Run("a withdrawal of the request the ancestor's reopen opened again", func(t *testing.T) {
		pool, _, apply := registeredPending(t)
		apply("reopened", intake.DispatchIssue{Key: basisAncestor, Seq: 63, Type: "issue.updated", Status: "in_progress", Title: "ancestor", Rank: "T",
			HandedOver: true})
		apply("request-opened-again", intake.DispatchAskOpened{Key: basisAncestor, Ask: "ask-100", Kind: record.AskApproval, Artifact: basisDoc, Mark: askSeq(64)})

		cueThenRead(t, pool, apply, "late", withdrawal, seedRead(requestOpen(), ancestorAtWork))

		stillPending(t, pool, "the late withdrawal")
	})

	t.Run("an approval a later changes request superseded", func(t *testing.T) {
		pool, _, apply := registeredPending(t)
		superseding := ancestorReview(intake.DispatchArtifactChangesRequested, 4, 62, reason)
		changes := `user:alice requested changes on version 4 of the primary document of LEGION-100 (dispatch://LEGION-100/spec): "name the rollback path".`

		cueThenRead(t, pool, apply, "late", ancestorReview(intake.DispatchArtifactApproved, 4, 60, ""), currentRead(superseding))

		ended(t, pool, changes)
		if got := noticesOfKind(t, pool, basisAncestor, "resting-root"); len(got) != 2 || !strings.Contains(got[1].Reason, changes) {
			t.Errorf("resting-root notices = %#v, want the registration's and the end naming the changes request", got)
		}

		apply("superseding", superseding)

		ended(t, pool, changes)
		for _, kind := range []record.NoticeKind{"basis-changes-requested", "design-changes-requested"} {
			if got := noticesOfKind(t, pool, gatedRoot, kind); len(got) != 0 {
				t.Errorf("%s notices after the changes request = %#v, want none: the wait it ended had ended", kind, got)
			}
		}
		if seeds := pendingSeeds(t, pool, gatedRoot); len(seeds) != 2 {
			t.Errorf("pending-basis seeds = %#v, want the registration's and the late approval's: the changes request is no cue for a wait that ended", seeds)
		}
	})
}

// The control of the late events: the person's approval at 60, delivered after they asked for
// changes at 61 and approved again at 63, queues the seed whose read shows the approval at 63 as
// Dispatch's latest review of the latest version. The read opens the step, on that latest
// review's place in the ancestor's event log and its reviewer, as a registration on that approval
// would, not on the event's. The open step keeps that place as its approval (BasisChangesRequest's
// mark.After), so the changes request at 61, delivered after the opening, passes nothing on and
// leaves the step open, while one recorded after the approval at 63 is passed on.
func TestAnApprovalTheReadShowsOpensTheStepAndAnOlderChangesRequestPassesNothing(t *testing.T) {
	pool, _, apply := registeredPending(t)
	latest := ancestorReview(intake.DispatchArtifactApproved, 4, 63, "")

	cueThenRead(t, pool, apply, "late-approval", ancestorReview(intake.DispatchArtifactApproved, 4, 60, ""), currentRead(latest))

	gate := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]
	if !gate.OnBasis() || !record.GateOpen(gate) || gate.Basis.Approval != latest.Mark || gate.Basis.Approver != "user:alice" {
		t.Fatalf("the spec step = %#v, want it open on the approval the read shows, at %#v by user:alice", gate, latest.Mark)
	}
	if got := noticesOfKind(t, pool, gatedRoot, "design-approved"); len(got) != 1 {
		t.Fatalf("design-approved notices = %#v, want one", got)
	}

	apply("older-changes", ancestorReview(intake.DispatchArtifactChangesRequested, 4, 61, "an earlier objection"))

	if gate := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]; !gate.OnBasis() || !record.GateOpen(gate) || gate.Basis.Approval != latest.Mark {
		t.Fatalf("the spec step after the older changes request = %#v, want it still open on the approval at 63", gate)
	}
	for _, kind := range []record.NoticeKind{"basis-changes-requested", "basis-ended"} {
		if got := noticesOfKind(t, pool, gatedRoot, kind); len(got) != 0 {
			t.Fatalf("%s notices after the older changes request = %#v, want none: it came before the approval the step opened on", kind, got)
		}
	}

	apply("newer-changes", ancestorReview(intake.DispatchArtifactChangesRequested, 4, 64, "name the rollback path"))

	if got := noticesOfKind(t, pool, gatedRoot, "basis-changes-requested"); len(got) != 1 || !strings.Contains(got[0].Reason, `"name the rollback path"`) {
		t.Fatalf("basis-changes-requested notices after the newer changes request = %#v, want one naming its reason", got)
	}
	if gate := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]; !gate.OnBasis() || !record.GateOpen(gate) {
		t.Fatalf("the spec step after the newer changes request = %#v, want it still open (R3)", gate)
	}
}

// Every event that is a cue for a waiting step opens and ends nothing on its own, whatever it
// says: it queues the step's pending-basis seed, whose read the workflow judges instead. The
// person's approval, a close of the request and the ancestor's done are each such a cue here.
func TestACueQueuesThePendingBasisSeedAndChangesNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		fact intake.Fact
	}{
		{"the person's approval", ancestorReview(intake.DispatchArtifactApproved, 4, 60, "")},
		{"a session's close of the request", intake.DispatchAskClosed{Key: basisAncestor, Ask: "ask-100", Kind: record.AskApproval, Artifact: basisDoc,
			State: "resolved", Actor: basisPerson, Mark: askSeq(61)}},
		{"the ancestor done", intake.DispatchIssue{Key: basisAncestor, Seq: 70, Type: "issue.updated", Status: record.DoneStatus, Title: "ancestor", Rank: "T",
			HandedOver: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, _, apply := registeredPending(t)

			apply("cue", tc.fact)

			if gate := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]; !gate.PendingOnBasis() || record.GateOpen(gate) {
				t.Fatalf("the spec step = %#v, want it still pending on ask-100", gate)
			}
			for _, kind := range []record.NoticeKind{"basis-ended", "design-approved"} {
				if got := noticesOfKind(t, pool, gatedRoot, kind); len(got) != 0 {
					t.Fatalf("%s notices = %#v, want none: an event moves nothing", kind, got)
				}
			}
			if seeds := pendingSeeds(t, pool, gatedRoot); len(seeds) != 2 || seeds[1] != theSeed {
				t.Fatalf("pending-basis seeds = %#v, want the registration's and the cue's %#v", seeds, theSeed)
			}
		})
	}
}

// The ancestor's done, given up and replayed while its reopen is not yet applied, ends no wait
// on its request: the seed it queues reads the ancestor at work and labelled (R1's first
// condition), with the request still open. The control: the same event whose seed reads the
// ancestor done ends the wait, naming that.
func TestALateAncestorDoneEndsNoWaitWhileDispatchShowsItOpen(t *testing.T) {
	done := intake.DispatchIssue{Key: basisAncestor, Seq: 70, Type: "issue.updated", Status: record.DoneStatus, Title: "ancestor", Rank: "T", HandedOver: true}

	t.Run("the read shows the ancestor reopened", func(t *testing.T) {
		pool, _, apply := registeredPending(t)

		cueThenRead(t, pool, apply, "late-done", done, seedRead(requestOpen(), ancestorAtWork))

		stillPending(t, pool, "the late done")
	})

	t.Run("the read shows the ancestor done", func(t *testing.T) {
		pool, _, apply := registeredPending(t)

		cueThenRead(t, pool, apply, "done", done, seedRead(requestOpen(), intake.AncestorRead{Status: record.DoneStatus, HandedOver: true}))

		ended(t, pool, "Dispatch shows LEGION-100 done, so LEGION-100 is accepted.")
	})
}

// A seed's read decides only the wait of the step it was queued for: an event that is a cue for
// two steps queues a seed for each, and one step's read never decides the other's wait. Here
// LEGION-209 registers a step on version 4 (on ask-200, which the architect opened on that version
// after the changes request and the reopen, or on ask-100 itself) beside LEGION-208's wait on
// ask-100, and an event that is a cue for both arrives: the person's changes request on version 4,
// ask-100 closed, or the ancestor done. LEGION-208's seed reads that wait's end and ends it, and
// LEGION-209's wait stays, with its own seed queued, though LEGION-208's read would end it. The
// same holds for LEGION-208 itself registered again on ask-200: the read of its seed for ask-100
// changes nothing of its wait on ask-200, whose own seed the cue queued.
func TestEachSeedsReadDecidesOnlyItsOwnStepsWait(t *testing.T) {
	const later, laterDoc = "LEGION-209", "artifact-209"
	reason := "name the rollback path"
	changes := ancestorReview(intake.DispatchArtifactChangesRequested, 4, 62, reason)
	for _, tc := range []struct {
		name string
		ask  string
		fact intake.Fact
		read intake.PendingBasisRead
		act  string
	}{
		{name: "the person's changes request", ask: "ask-200", fact: changes, read: currentRead(changes),
			act: `user:alice requested changes on version 4 of the primary document of LEGION-100 (dispatch://LEGION-100/spec): "name the rollback path".`},
		{name: "a session's close of the request", ask: "ask-100", fact: intake.DispatchAskClosed{Key: basisAncestor, Ask: "ask-100", Kind: record.AskApproval,
			Artifact: basisDoc, State: "resolved", Actor: basisPerson, Mark: askSeq(61)}, read: closedBy(basisPerson, ancestorAtWork),
			act: closedSentence("user:alice")},
		{name: "the ancestor done", ask: "ask-200", fact: intake.DispatchIssue{Key: basisAncestor, Seq: 70, Type: "issue.updated", Status: record.DoneStatus,
			Title: "ancestor", Rank: "T", HandedOver: true}, read: seedRead(requestOpen(), intake.AncestorRead{Status: record.DoneStatus, HandedOver: true}),
			act: "Dispatch shows LEGION-100 done, so LEGION-100 is accepted."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, _, apply := registeredPending(t)
			seedIssue(t, pool, record.Issue{Key: later, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
				Tree: later, Project: "LEGION", Title: "later", Phase: phase.Admitted, Generation: 1, Status: "in_progress", Rank: "V",
				HandedOver: true, LastDispatchSeq: 1})
			waiting := pendingBasis()
			waiting.RegisteredAt = basisEngineNow.Add(time.Hour)
			seedGate(t, pool, record.PendOnBasis(record.DesignGate{Issue: later, ArtifactID: laterDoc, LatestVersion: 4}, *waiting, tc.ask))

			cueThenRead(t, pool, apply, "cue", tc.fact, tc.read)

			ended(t, pool, tc.act)
			if gate := gatesOf(t, pool, later)[record.DesignStepSpec]; !gate.PendingOnBasis() || gate.Basis.Pending.Ask != tc.ask || record.GateOpen(gate) {
				t.Errorf("the spec step of %s = %#v, want it still pending on %s: the read was not its seed's", later, gate, tc.ask)
			}
			if got := noticesOfKind(t, pool, later, "basis-ended"); len(got) != 0 {
				t.Errorf("basis-ended notices of %s = %#v, want none", later, got)
			}
			want := record.PendingBasisSeed{Step: record.DesignStepSpec, ArtifactID: laterDoc, BasisArtifactID: basisDoc, BasisVersion: 4, Ask: tc.ask,
				Generation: 1, RegisteredAt: waiting.RegisteredAt}
			if seeds := pendingSeeds(t, pool, later); len(seeds) != 1 || seeds[0] != want {
				t.Errorf("pending-basis seeds of %s = %#v, want its own, %#v", later, seeds, want)
			}
		})
	}

	t.Run("the step registered again on another request", func(t *testing.T) {
		pool, _, apply := registeredPending(t)
		again := pendingBasis()
		again.Pending = &record.PendingBasis{Ask: "ask-200"}
		apply("register-on-ask-200", onBasis(again, 7))

		apply("cue", changes)
		apply("read-for-ask-100", currentRead(changes))

		if gate := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]; !gate.PendingOnBasis() || gate.Basis.Pending.Ask != "ask-200" || record.GateOpen(gate) {
			t.Fatalf("the spec step = %#v, want it still pending on ask-200: the read was its seed's for ask-100", gate)
		}
		if got := noticesOfKind(t, pool, gatedRoot, "basis-ended"); len(got) != 0 {
			t.Fatalf("basis-ended notices = %#v, want none", got)
		}
		if seeds := pendingSeeds(t, pool, gatedRoot); len(seeds) != 3 || seeds[2].Ask != "ask-200" {
			t.Fatalf("pending-basis seeds = %#v, want the two registrations' and the cue's on ask-200", seeds)
		}
	})
}
