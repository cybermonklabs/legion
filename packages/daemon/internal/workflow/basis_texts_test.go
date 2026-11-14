package workflow

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The texts that turn an approval into words never say that a person approved the root's own
// document for a step an ancestor's approval opened (dispatch://INFRA-3320, R2): the messages a
// gate's opening sends the roles of the tree (tellGateMoved), the resume task of a root waiting on
// a person (resumeTask), and the start task of a re-admission (KeptDecisionsTask).

// basisOpening is the part of an opening of LEGION-208's gate that names its spec step, at
// version 4 of artifact-208, standing open on the person's approval of version 3 of LEGION-100's
// primary document.
const basisOpening = " The spec step stands open on an ancestor's approval: a person approved version 3 of the primary document of LEGION-100 (dispatch://LEGION-100/spec). " +
	"No person approved version 4 of its spec document (artifact-208)."

// An opening of the standing gate on an ancestor's approval names that approval: the ancestor, its
// document and the version a person approved, and says that no person approved the root's version.
// It keeps the two parts roles/merger.md quotes as the release of a merge, `The design gate of
// <tree> opened:` and `every step the deployment gates now stands open`. Where a person's approval
// of another step opens the gate while the spec step stands on a basis, the message names that
// person's approval first, and then the step on the basis.
func TestAGateOpeningOnAnAncestorsApprovalNamesItAndNoPersonsApprovalOfTheRoot(t *testing.T) {
	for _, tc := range []struct {
		name  string
		steps []record.DesignStep
		want  string
	}{
		{
			name:  "a registration on an approved basis opens the gate",
			steps: config.DesignGateRootIssues.Steps(),
			want:  "The design gate of LEGION-208 opened: every step the deployment gates now stands open. A merge the closed gate held is released." + basisOpening,
		},
		{
			name:  "a person's approval of another step opens the gate beside a step on a basis",
			steps: twoSteps(),
			want: "The design gate of LEGION-208 opened: a person approved version 2 of its criteria document (artifact-criteria), " +
				"and every step the deployment gates now stands open. A merge the closed gate held is released." + basisOpening,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := stepsEngine(tc.steps, nil)
			seedMergingRoot(t, pool)
			seedSlot(t, pool, record.Slot{Issue: gatedRoot, Index: 0, AdmittedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)})
			if len(tc.steps) == 1 {
				// The spec step stands at a version with its reviewer, and the registration on the
				// ancestor's approval opens it.
				seedGate(t, pool, record.DesignGate{Issue: gatedRoot, ArtifactID: gateDoc, LatestVersion: 4})
				applyMirrored(t, pool, engine, "register-on-basis", onBasis(approvedBasis(), 6))
			} else {
				// The spec step stands open on the ancestor's approval, and the criteria step at a
				// version with its reviewer, whose approval opens the gate.
				basis := *approvedBasis()
				basis.RegisteredAt = basisEngineNow
				seedGate(t, pool, record.OpenOnBasis(record.DesignGate{Issue: gatedRoot, ArtifactID: gateDoc, LatestVersion: 4}, basis))
				seedGate(t, pool, record.DesignGate{Issue: gatedRoot, Step: record.DesignStepCriteria, ArtifactID: criteriaDoc, LatestVersion: 2})
				applyMirrored(t, pool, engine, "criteria-approved-2", stepApproval(gatedRoot, criteriaDoc, 2, record.EventMark{Seq: 12}))
			}

			assertRoleMessages(t, pool, gatedRoot, []record.RoleMessage{
				{Role: treeRoleToken(t, gatedRoot, claim.RoleMerger), Body: tc.want},
				{Role: treeRoleToken(t, gatedRoot, claim.RoleArchitect), Body: tc.want},
			})
			for _, quoted := range []string{"The design gate of LEGION-208 opened: ", "every step the deployment gates now stands open"} {
				if !strings.Contains(tc.want, quoted) {
					t.Fatalf("the opening %q does not carry %q, which roles/merger.md quotes as the release of a merge", tc.want, quoted)
				}
			}
			if strings.Contains(strings.ToLower(tc.want), "a person approved version 4") {
				t.Fatalf("the opening %q names a person's approval of version 4 of the root's document, which no person gave", tc.want)
			}
		})
	}
}

// A root waiting on a person is answered by its step's opening on an ancestor's approval, or by
// the end of its step's wait on the ancestor's request, with no notice beside either, so its
// resume task is what tells it; the wait's recorded answer carries what the resume names
// (dispatch://INFRA-3320, R1). An opening names the ancestor's approval and says no person approved
// the root's version. An end names the act, the ancestor's document and its version, and not a
// person's changes request on the root's document. A person's approval of the root's own document
// while the step waited names that approval and then the end of the wait.
func TestTheResumeOfARootWaitingOnAPersonNamesWhatItsBasisDid(t *testing.T) {
	for _, tc := range []struct {
		name  string
		basis *record.GateBasis
		act   intake.Fact
		want  []string
		not   []string
	}{
		{
			name:  "a registration on an approved basis",
			basis: approvedBasis(),
			want: []string{"An ancestor's approval opened the spec step of LEGION-208 at version 4: a person approved version 3 of the primary document of LEGION-100 (dispatch://LEGION-100/spec). No person approved your primary document at that version.",
				"The step stands open on that approval at version 4", DesignApprovedWake},
			not: []string{"was approved", "is approved at version"},
		},
		{
			name:  "the ancestor's changes request on the version the step waited on",
			basis: pendingBasis(),
			act:   ancestorReview(intake.DispatchArtifactChangesRequested, 4, 61, "name the rollback path"),
			want: []string{"The wait of your spec step on an ancestor's approval request ended: user:alice requested changes on version 4 of the primary document of LEGION-100",
				"no longer waits on approval request ask-100", "Read that document before you register the spec step again."},
			not: []string{"A person requested changes to your", "revise the primary document as they ask"},
		},
		{
			name:  "a person's approval of the root's own document while its step waited",
			basis: pendingBasis(),
			act:   stepApproval(gatedRoot, gateDoc, 4, askSeq(9)),
			want: []string{"Your primary document version 4 on LEGION-208 was approved.",
				"The wait of your spec step on an ancestor's approval request also ended: user:alice approved version 4 of the primary document of LEGION-208",
				"Your primary document is approved at version 4", DesignApprovedWake},
			not: []string{"\n"},
		},
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
			if tc.act != nil {
				apply("act", tc.act)
			}

			root := recordedRoot(t, pool)
			if root.Awaiting == nil || root.Awaiting.AnsweredAt == nil {
				t.Fatalf("the waiting root = %#v, want its wait answered", root.Awaiting)
			}
			step, gate := gateInQuestion(engine.cfg.Steps, gatesOf(t, pool, gatedRoot))
			task := resumeTask(gatedRoot, *root.Awaiting, step, gate, nil, true)
			for _, want := range tc.want {
				if !strings.Contains(task, want) {
					t.Fatalf("resume task %q, want it to name %q", task, want)
				}
			}
			for _, not := range tc.not {
				if strings.Contains(task, not) {
					t.Fatalf("resume task %q, want no %q", task, not)
				}
			}
		})
	}
}

// The start task of a re-admission names the person's approval of an earlier version of the
// root's own document, never the version an ancestor's approval opened (admission keeps the
// person's approval alone, Admission.keepStaleApproval). Beside its request of the latest version
// it names R1's path: a registration on an approved ancestor's approval, where the root is a view
// on that ancestor (dispatch://INFRA-3320, R2; k3's re-admission of R). A task with no approval
// kept names neither.
func TestTheKeptDecisionsTaskNamesThePersonsApprovalAndR1sPath(t *testing.T) {
	task := KeptDecisionsTask("LEGION-208", record.KeptDecisions{KeptApproval: record.KeptApproval{Step: record.DesignStepSpec, Approved: 2, Latest: 5}})
	want := "A person approved version 2 of the primary document, and the primary document is now at version 5. " +
		"Request approval of version 5, as skill://legion-architect/references/design-gate.md (\"The armed gate\") says. " +
		"The other way is to write LEGION-208 as a view on an approved ancestor and register the step on that ancestor's approval, as the same page says."
	if task != want {
		t.Fatalf("the start task = %q, want %q", task, want)
	}
	if strings.Contains(task, "approved at version 5") || strings.Contains(task, "approved version 5") {
		t.Fatalf("the start task %q names an approval of the latest version, which no person gave", task)
	}
	decided := KeptDecisionsTask("LEGION-208", record.KeptDecisions{Decisions: []record.DecisionMark{{Ref: "dispatch://LEGION-208/spec",
		Decision: record.DecisionDesignApproved, Actor: "user:alice"}}})
	if strings.Contains(decided, "view on an approved ancestor") || strings.Contains(decided, "Request approval") {
		t.Fatalf("the start task %q with no approval kept names a request or R1's path", decided)
	}
}
