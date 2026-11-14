package admit

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The ancestor LEGION-208's spec step waits on in these tests (dispatch://INFRA-3320, R1): its
// primary document at version 4, which one more requirement block made, and the person's open
// approval request of that version. The daemon records the ancestor in no tree, so no resting-root
// notice is queued for it, and nothing here reads one: these tests are about the root's own wait.
const (
	pendingAncestor    = "LEGION-150"
	pendingAncestorDoc = "artifact-150"
	pendingAsk         = "ask-150"
)

// treeArchitect is the session seedArchitectClaim records as LEGION-208's architect: a close it
// makes is a close by the root's own tree.
var treeArchitect = dispatch.Actor{Kind: "session", ID: "ses_architect"}

// pendingRegistration is LEGION-208's architect registering version 3 of its primary document on
// version 4 of LEGION-150's primary document as a pending basis, register_gate's read beginning at
// sequence 6. requestAsk is the root's own approval request that read found open on the person, ""
// for none.
func pendingRegistration(requestAsk string) intake.GateRegistered {
	return intake.GateRegistered{Issue: "LEGION-208", ArtifactID: "artifact-208", Version: 3, DispatchVersion: 3, Since: record.EventMark{Seq: 6},
		RequestAsk: requestAsk, ViewOf: pendingAncestor, Basis: &record.GateBasis{Issue: pendingAncestor, ArtifactID: pendingAncestorDoc, Version: 4,
			Serves: []string{"dispatch://LEGION-150/spec#b-p4@v4"}, Reason: "LEGION-208 delivers p4 of LEGION-150.", Session: treeArchitect.ID,
			Pending: &record.PendingBasis{Ask: pendingAsk}}}
}

// ancestorAct is the person's review of kind on version 4 of LEGION-150's primary document, at seq
// in LEGION-150's own event log.
func ancestorAct(kind intake.DispatchArtifactKind, seq int64, reason string) intake.DispatchArtifact {
	return intake.DispatchArtifact{Key: pendingAncestor, ArtifactID: pendingAncestorDoc, Kind: kind, Version: 4, Reason: reason, Actor: alice,
		Mark: record.EventMark{Seq: seq, At: fixedNow.Add(-time.Duration(100-seq) * time.Minute)}}
}

// basisNotices is every notice queued for LEGION-208's architect that tells a gate's opening, its
// end or a review: what a root waiting on a person is told by its resume instead.
func basisNotices(t *testing.T, pool *pgxpool.Pool) []record.Notice {
	t.Helper()
	var told []record.Notice
	for _, kind := range []record.NoticeKind{"design-approved", "basis-ended", "design-changes-requested", "basis-changes-requested"} {
		told = append(told, gateNotices(t, pool, "LEGION-208", kind)...)
	}
	return told
}

// registeredPendingWhileWaiting applies the pending registration to LEGION-208, waiting on a
// person, and holds what R1 says of it: the root keeps that wait, unanswered, beside its step's
// wait on the ancestor's request, the registration declaring no hand-back, and nobody is told.
func registeredPendingWhileWaiting(t *testing.T, pool *pgxpool.Pool, admission *Admission, requestAsk string) {
	t.Helper()
	applyFact(t, pool, admission, "register-pending", pendingRegistration(requestAsk))
	if root := issue(t, pool, "LEGION-208"); !root.AwaitingPerson() || root.Answered() {
		t.Fatalf("the root after the pending registration = %s (awaiting %+v), want it still waiting, unanswered", root.Phase, root.Awaiting)
	}
	if gate := storedGate(t, pool, "LEGION-208"); !gate.PendingOnBasis() || record.GateOpen(*gate) || gate.Basis.Pending.Ask != pendingAsk ||
		gate.Basis.Version != 4 {
		t.Fatalf("the spec step after the pending registration = %+v, want it closed, waiting on %s of version 4", gate, pendingAsk)
	}
	if told := basisNotices(t, pool); len(told) != 0 {
		t.Fatalf("the pending registration queued %+v, want nothing told", told)
	}
}

// requestWaitingRoot seeds LEGION-208 waiting on its own design gate at version 3: its architect
// handed its approval request ask-approval of that version to the person at sequence 5, and it
// holds the row it keeps through the wait, with LEGION-300 waiting in line behind it.
func requestWaitingRoot(t *testing.T) (*pgxpool.Pool, *Admission) {
	t.Helper()
	pool := migratedPool(t)
	seedSlotted(t, pool, "LEGION-208", "U")
	seedWaiting(t, pool, "LEGION-300", "V")
	seedArchitectClaim(t, pool, "LEGION-208", true)
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 3})
	inTx(t, pool, func(tx pgx.Tx) {
		records := record.NewStore()
		if err := records.PutRootAsk(context.Background(), tx, record.RootAsk{Issue: "LEGION-208", Ask: "ask-approval", Kind: record.AskApproval,
			Artifact: "artifact-208", Open: true, Seq: 5, WaitingOn: "human", WaitingOnSeq: 5, ByTree: true}); err != nil {
			t.Fatalf("record the approval request of LEGION-208: %v", err)
		}
		if err := records.Declare(context.Background(), tx, "LEGION-208", record.EventMark{Seq: 5}); err != nil {
			t.Fatalf("declare the hand-back of LEGION-208: %v", err)
		}
	})
	admission := newAdmissionWithGate(t, 1, config.DesignGateRootIssues, nil)
	tick(t, pool, admission, "tick-1")
	if root := issue(t, pool, "LEGION-208"); !root.AwaitingPerson() || root.Awaiting.GateVersion != 3 || root.Awaiting.Artifact != "artifact-208" {
		t.Fatalf("the root = %s (awaiting %+v), want it waiting on its gate at version 3", root.Phase, root.Awaiting)
	}
	return pool, admission
}

// k11's awaiting-person runs (dispatch://INFRA-3320, R1), which the scratch Dispatch rig does not
// build, since no rig act puts a root in awaiting_person with its architect's ask of its own. A
// root that waits on a person keeps that wait beside its step's wait on an ancestor's open approval
// request: the pending registration declares no hand-back, the person's approval of the ancestor's
// version answers the wait as an approval does, and a changes request on it answers the wait as a
// changes request does, with no notice beside either; the resume task names the ancestor's
// document, its version and what happened.
//
// A root waiting on its own approval request whose architect makes the pending registration and
// then resolves that request: the resolve, by a session of the root's own tree, answers the wait
// as closed. With the resume first, the resume names the ancestor's request while the step is
// pending and asks for no approval request of the root's own, and the opening that follows sends
// design-approved naming the ancestor's version 4 as the basis. With the admission pass held
// (Admission.HoldForCatchUp), so the person's approval comes before the resume, the opening sends
// no notice, and the resume names the ancestor's version 4 as the basis.
func TestAPendingBasisMeetsARootWaitingOnAPerson(t *testing.T) {
	const ancestorDocument = "the primary document of LEGION-150 (dispatch://LEGION-150/spec)"
	for _, tc := range []struct {
		name string
		act  intake.DispatchArtifact
		by   record.Answer
		says []string
		not  []string
	}{
		{
			name: "a question of its own, then the person's approval of the ancestor's version", act: ancestorAct(intake.DispatchArtifactApproved, 60, ""),
			by: record.AnsweredByApproval,
			says: []string{"An ancestor's approval opened the spec step of LEGION-208 at version 3: a person approved version 4 of " + ancestorDocument +
				". No person approved your primary document at that version.", "The step stands open on that approval at version 3", workflow.DesignApprovedWake},
			not: []string{"was approved", "Do not request approval"},
		},
		{
			name: "a question of its own, then the person's changes request on the ancestor's version",
			act:  ancestorAct(intake.DispatchArtifactChangesRequested, 61, "name the rollback path"), by: record.AnsweredByChanges,
			says: []string{"The wait of your spec step on an ancestor's approval request ended: user:alice requested changes on version 4 of " +
				ancestorDocument + `: "name the rollback path".`, "The spec step of LEGION-208 no longer waits on approval request ask-150 of version 4 of " +
				ancestorDocument, "The step stays closed, registered on the primary document of LEGION-208 (dispatch://LEGION-208/spec) with no basis.",
				"Still with the person: ask ask-q."},
			not: []string{"A person requested changes to your", workflow.DesignApprovedWake},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, admission := waitingRoot(t, config.DesignGateRootIssues)
			registeredPendingWhileWaiting(t, pool, admission, "")

			// The act answers the wait, and the pass that applied it resumes the root on the row
			// it kept through the wait.
			admission.HoldForCatchUp("LEGION-208")
			applyFact(t, pool, admission, "act", tc.act)
			root := issue(t, pool, "LEGION-208")
			if !root.Answered() || root.Awaiting.AnsweredBy != tc.by {
				t.Fatalf("the root after the act = %s (awaiting %+v), want its wait answered by %s", root.Phase, root.Awaiting, tc.by)
			}
			if told := basisNotices(t, pool); len(told) != 0 {
				t.Fatalf("the act queued %+v for a root waiting on a person, want nothing: its resume tells it", told)
			}
			admission.ReleaseCatchUp("LEGION-208")
			tick(t, pool, admission, "tick-2")
			resumes := architectResumes(t, pool)
			if len(resumes) != 1 {
				t.Fatalf("architect starts = %q, want one resume", resumes)
			}
			for _, want := range tc.says {
				if !strings.Contains(resumes[0], want) {
					t.Errorf("resume %q does not say %q", resumes[0], want)
				}
			}
			for _, unwanted := range tc.not {
				if strings.Contains(resumes[0], unwanted) {
					t.Errorf("resume %q says %q", resumes[0], unwanted)
				}
			}
			if told := basisNotices(t, pool); len(told) != 0 {
				t.Fatalf("the resume left %+v queued beside it, want nothing", told)
			}
			t.Logf("resume: %s", resumes[0])
		})
	}

	pendingResume := []string{"Your ask", "was closed by a session of your tree",
		"Your spec step is closed at version 3. It waits on approval request ask-150 of version 4 of " + ancestorDocument + ".",
		"A person's approval of that version opens the step. Do not request approval of your own primary document.",
		"The daemon tells you when the step opens or its wait ends."}
	for _, tc := range []struct {
		name string
		held bool
	}{
		{name: "its own approval request resolved, the resume before the person's approval"},
		{name: "its own approval request resolved, the person's approval before the held resume", held: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, admission := requestWaitingRoot(t)
			registeredPendingWhileWaiting(t, pool, admission, "ask-approval")
			if tc.held {
				admission.HoldForCatchUp("LEGION-208")
			}
			applyFact(t, pool, admission, "resolve", intake.DispatchAskClosed{Key: "LEGION-208", Ask: "ask-approval", Kind: record.AskApproval,
				Artifact: "artifact-208", State: "resolved", Actor: treeArchitect, Mark: seqMark(7)})
			if tc.held {
				if root := issue(t, pool, "LEGION-208"); !root.Answered() || root.Awaiting.AnsweredBy != record.AnsweredByClosed {
					t.Fatalf("the root after its architect resolved its request = %s (awaiting %+v), want its wait answered as closed", root.Phase, root.Awaiting)
				}
				applyFact(t, pool, admission, "ancestor-approved", ancestorAct(intake.DispatchArtifactApproved, 60, ""))
				if gate := storedGate(t, pool, "LEGION-208"); !gate.OnBasis() || !record.GateOpen(*gate) || gate.Basis.Version != 4 {
					t.Fatalf("the spec step after the person's approval = %+v, want it open on version 4 of %s", gate, pendingAncestor)
				}
				if told := basisNotices(t, pool); len(told) != 0 {
					t.Fatalf("the opening before the resume queued %+v, want nothing: the resume tells it", told)
				}
				admission.ReleaseCatchUp("LEGION-208")
				tick(t, pool, admission, "tick-2")
				resumes := architectResumes(t, pool)
				if len(resumes) != 1 {
					t.Fatalf("architect starts = %q, want one resume", resumes)
				}
				for _, want := range []string{"Your ask", "was closed by a session of your tree",
					"Your spec step stands open at version 3 on an ancestor's approval: a person approved version 4 of " + ancestorDocument +
						". No person approved that version of your primary document.", workflow.DesignApprovedWake} {
					if !strings.Contains(resumes[0], want) {
						t.Errorf("resume %q does not say %q", resumes[0], want)
					}
				}
				if told := basisNotices(t, pool); len(told) != 0 {
					t.Fatalf("the resume left %+v queued beside it, want nothing", told)
				}
				t.Logf("resume: %s", resumes[0])
				return
			}

			// The resolve answers the wait as closed, and the pass that applied it resumes the
			// root on the row it kept.
			resumes := architectResumes(t, pool)
			if root := issue(t, pool, "LEGION-208"); root.Awaiting != nil || root.Phase != phase.Admitted || len(resumes) != 1 {
				t.Fatalf("the root after its architect resolved its request = %s (awaiting %+v) with architect starts %q, want it resumed at admitted",
					root.Phase, root.Awaiting, resumes)
			}
			for _, want := range pendingResume {
				if !strings.Contains(resumes[0], want) {
					t.Errorf("resume %q does not say %q", resumes[0], want)
				}
			}
			for _, unwanted := range []string{"request approval again", "register_gate", workflow.DesignApprovedWake} {
				if strings.Contains(resumes[0], unwanted) {
					t.Errorf("resume %q says %q while the step waits on the ancestor's request", resumes[0], unwanted)
				}
			}
			t.Logf("resume: %s", resumes[0])
			applyFact(t, pool, admission, "ancestor-approved", ancestorAct(intake.DispatchArtifactApproved, 60, ""))
			approved := gateNotices(t, pool, "LEGION-208", "design-approved")
			if len(approved) != 1 || approved[0].Ref != "dispatch://LEGION-150/spec" || approved[0].Version != 4 || approved[0].Actor != "user:alice" ||
				!strings.Contains(approved[0].Reason, "No person approved that version.") {
				t.Fatalf("the design-approved notices after the opening = %+v, want one naming version 4 of %s as the basis", approved, pendingAncestor)
			}
			if gate := storedGate(t, pool, "LEGION-208"); !gate.OnBasis() || gate.Basis.Version != 4 {
				t.Fatalf("the spec step after the person's approval = %+v, want it open on version 4 of %s", gate, pendingAncestor)
			}
		})
	}
}
