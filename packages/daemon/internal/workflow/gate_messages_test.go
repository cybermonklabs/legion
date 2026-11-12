package workflow

import (
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A new version of an approved spec closes the tree's design gate under the workers already
// running on it, and its approval opens it again. Each move sends one message naming the version
// to every worker holding a phase in the tree and to the root architect, so nobody goes on
// building to a design the person is being asked about again (dispatch://INFRA-714/spec R5).
func TestEveryPhaseHolderAndTheRootArchitectAreToldTheGateClosedAndOpened(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Planning, Generation: 4, Status: "in_progress", Rank: "U"})
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-208", Project: "LEGION", Title: "a child under test", Parent: new("LEGION-208"), Phase: phase.Testing,
		Generation: 4, Status: "testing", Rank: "V"})
	// A member waiting before its first phase holds none, so nobody is addressed for it.
	seedIssue(t, pool, record.Issue{Key: "LEGION-210", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-208", Project: "LEGION", Title: "a child not started", Parent: new("LEGION-208"), Phase: phase.Admitted,
		Generation: 4, Status: "todo", Rank: "W"})
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 3, ApprovedVersion: new(3)})
	seedSlot(t, pool, record.Slot{Issue: "LEGION-208", Index: 0, AdmittedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)})
	engine := testEngine(config.DesignGateRootIssues, nil)

	applyMirrored(t, pool, engine, "version-4", intake.DispatchArtifact{
		Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactVersion, Version: 4})
	applyMirrored(t, pool, engine, "approved-4", intake.DispatchArtifact{
		Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactApproved, Version: 4})

	closed := gateMovedBody(record.DesignStepSpec, "artifact-208", 4, false)
	opened := gateMovedBody(record.DesignStepSpec, "artifact-208", 4, true)
	assertRoleMessages(t, pool, "LEGION-208", []record.RoleMessage{
		{Role: treeRoleToken(t, "LEGION-208", claim.RolePlanner), Body: closed},
		{Role: treeRoleToken(t, "LEGION-208", claim.RoleArchitect), Body: closed},
		{Role: treeRoleToken(t, "LEGION-208", claim.RolePlanner), Body: opened},
		{Role: treeRoleToken(t, "LEGION-208", claim.RoleArchitect), Body: opened},
	})
	assertRoleMessages(t, pool, "LEGION-209", []record.RoleMessage{
		{Role: treeRoleToken(t, "LEGION-209", claim.RoleTester), Body: closed},
		{Role: treeRoleToken(t, "LEGION-209", claim.RoleTester), Body: opened},
	})
	assertRoleMessages(t, pool, "LEGION-210", nil)
}

// A gate event that moves nothing — an approval of a gate already open — tells nobody: the
// messages are of the two moves, not of every event of the document.
func TestAGateEventThatMovesNothingTellsNobody(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Planning, Generation: 4, Status: "in_progress", Rank: "U"})
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 3, ApprovedVersion: new(3)})
	seedSlot(t, pool, record.Slot{Issue: "LEGION-208", Index: 0, AdmittedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)})

	applyMirrored(t, pool, testEngine(config.DesignGateRootIssues, nil), "approved-again", intake.DispatchArtifact{
		Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactApproved, Version: 3})

	assertOutboxCount(t, pool, "role_message", 0)
}

// Under more than one gated step, the message is of the standing of every step the deployment
// gates, not of the one row the event names. The merger merges on it, and the merger's own start
// is held while any required step is closed (the outbox's designGateClosed over
// classify.StandingGate), so an "opened" message sent while another document is still under
// review would merge a head whose design nobody has approved (dispatch://INFRA-714/spec R5,
// under the steps INFRA-1272 gates).
func TestTheGateMessageIsOfEveryGatedStepAndNotTheOneTheEventNames(t *testing.T) {
	pool := migratedPool(t)
	seedMergingRoot(t, pool)
	seedGate(t, pool, record.DesignGate{Issue: gatedRoot, Step: record.DesignStepCriteria, ArtifactID: criteriaDoc,
		LatestVersion: 1, ApprovedVersion: new(1)})
	seedGate(t, pool, record.DesignGate{Issue: gatedRoot, Step: record.DesignStepSpec, ArtifactID: "artifact-208",
		LatestVersion: 3, ApprovedVersion: new(3)})
	seedSlot(t, pool, record.Slot{Issue: gatedRoot, Index: 0, AdmittedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)})
	engine := stepsEngine(twoSteps(), nil)

	// The spec's new version closes the standing gate; the Verification document's own new
	// version finds it closed already, and the approval of that document leaves the spec closed.
	// One message for the move, and none for the two events that move nothing.
	applyMirrored(t, pool, engine, "spec-4", intake.DispatchArtifact{
		Key: gatedRoot, ArtifactID: "artifact-208", Kind: intake.DispatchArtifactVersion, Version: 4})
	applyMirrored(t, pool, engine, "criteria-2", intake.DispatchArtifact{
		Key: gatedRoot, ArtifactID: criteriaDoc, Kind: intake.DispatchArtifactVersion, Version: 2})
	applyMirrored(t, pool, engine, "criteria-approved-2", stepApproval(gatedRoot, criteriaDoc, 2, record.EventMark{Seq: 11}))
	assertRoleMessages(t, pool, gatedRoot, []record.RoleMessage{
		{Role: treeRoleToken(t, gatedRoot, claim.RoleMerger), Body: gateMovedBody(record.DesignStepSpec, "artifact-208", 4, false)},
		{Role: treeRoleToken(t, gatedRoot, claim.RoleArchitect), Body: gateMovedBody(record.DesignStepSpec, "artifact-208", 4, false)},
	})

	// The spec's own approval is the one that opens every gated step, so it is the one told.
	applyMirrored(t, pool, engine, "spec-approved-4", stepApproval(gatedRoot, "artifact-208", 4, record.EventMark{Seq: 12}))
	assertRoleMessages(t, pool, gatedRoot, []record.RoleMessage{
		{Role: treeRoleToken(t, gatedRoot, claim.RoleMerger), Body: gateMovedBody(record.DesignStepSpec, "artifact-208", 4, false)},
		{Role: treeRoleToken(t, gatedRoot, claim.RoleArchitect), Body: gateMovedBody(record.DesignStepSpec, "artifact-208", 4, false)},
		{Role: treeRoleToken(t, gatedRoot, claim.RoleMerger), Body: gateMovedBody(record.DesignStepSpec, "artifact-208", 4, true)},
		{Role: treeRoleToken(t, gatedRoot, claim.RoleArchitect), Body: gateMovedBody(record.DesignStepSpec, "artifact-208", 4, true)},
	})
}

// The message and the merger's start hold ask one question, so they read one predicate
// (classify.StandingGate, the outbox's designGateClosed). A step the deployment gates that
// nothing has registered names no document, so the hold passes it over; a message decided over a
// predicate that let such a step hold the work would say nothing at all while the hold moved,
// which is what a step list that grew left behind (dispatch://INFRA-714/spec R5).
func TestAnUnregisteredGatedStepLeavesTheMessageAndTheHoldAgreed(t *testing.T) {
	pool := migratedPool(t)
	seedMergingRoot(t, pool)
	// The criteria step is gated and registered by nothing; the spec step is approved, so the
	// merger's hold lets it go and the tree's work stands released.
	seedGate(t, pool, record.DesignGate{Issue: gatedRoot, Step: record.DesignStepSpec, ArtifactID: "artifact-208",
		LatestVersion: 3, ApprovedVersion: new(3)})
	seedSlot(t, pool, record.Slot{Issue: gatedRoot, Index: 0, AdmittedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)})
	engine := stepsEngine(twoSteps(), nil)

	applyMirrored(t, pool, engine, "spec-4", intake.DispatchArtifact{
		Key: gatedRoot, ArtifactID: "artifact-208", Kind: intake.DispatchArtifactVersion, Version: 4})

	closed := gateMovedBody(record.DesignStepSpec, "artifact-208", 4, false)
	assertRoleMessages(t, pool, gatedRoot, []record.RoleMessage{
		{Role: treeRoleToken(t, gatedRoot, claim.RoleMerger), Body: closed},
		{Role: treeRoleToken(t, gatedRoot, claim.RoleArchitect), Body: closed},
	})
}

// A registration writes a gate row as much as an artifact event does, and under more than one
// gated step it can close the standing gate by itself: the architect registers its spec anew at a
// version nobody has approved. The merger merges on the message, so a registration that told
// nobody left it merging under a document that is with its reviewer.
func TestARegistrationThatClosesTheStandingGateTellsTheMerger(t *testing.T) {
	pool := migratedPool(t)
	seedMergingRoot(t, pool)
	seedGate(t, pool, record.DesignGate{Issue: gatedRoot, Step: record.DesignStepCriteria, ArtifactID: criteriaDoc,
		LatestVersion: 1, ApprovedVersion: new(1)})
	seedGate(t, pool, record.DesignGate{Issue: gatedRoot, Step: record.DesignStepSpec, ArtifactID: "artifact-208",
		LatestVersion: 3, ApprovedVersion: new(3)})
	seedSlot(t, pool, record.Slot{Issue: gatedRoot, Index: 0, AdmittedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)})
	engine := stepsEngine(twoSteps(), nil)

	applyMirrored(t, pool, engine, "register-spec-4", intake.GateRegistered{Issue: gatedRoot, Step: record.DesignStepSpec,
		ArtifactID: "artifact-208", Version: 4, DispatchVersion: 4, Since: record.EventMark{Seq: 11}})

	closed := gateMovedBody(record.DesignStepSpec, "artifact-208", 4, false)
	assertRoleMessages(t, pool, gatedRoot, []record.RoleMessage{
		{Role: treeRoleToken(t, gatedRoot, claim.RoleMerger), Body: closed},
		{Role: treeRoleToken(t, gatedRoot, claim.RoleArchitect), Body: closed},
	})
}

// The other half of the same hole: a registration whose read of Dispatch carries the person's
// approval opens the standing gate, and a merger told the gate closed waits for the message that
// says it opened again. Without it that merger waits for a message no event will send, since the
// approval the registration carries moves no gate afterwards.
func TestARegistrationThatOpensTheStandingGateTellsTheMerger(t *testing.T) {
	pool := migratedPool(t)
	seedMergingRoot(t, pool)
	seedGate(t, pool, record.DesignGate{Issue: gatedRoot, Step: record.DesignStepCriteria, ArtifactID: criteriaDoc,
		LatestVersion: 1, ApprovedVersion: new(1)})
	// The spec step stands at a version with its reviewer, so the merger's hold holds it.
	seedGate(t, pool, record.DesignGate{Issue: gatedRoot, Step: record.DesignStepSpec, ArtifactID: "artifact-208",
		LatestVersion: 4})
	seedSlot(t, pool, record.Slot{Issue: gatedRoot, Index: 0, AdmittedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)})
	engine := stepsEngine(twoSteps(), nil)

	applyMirrored(t, pool, engine, "register-approved-4", intake.GateRegistered{Issue: gatedRoot, Step: record.DesignStepSpec,
		ArtifactID: "artifact-208", Version: 4, DispatchVersion: 4, ApprovedVersion: 4,
		ApprovedMark: record.EventMark{Seq: 12}, Since: record.EventMark{Seq: 13}})

	opened := gateMovedBody(record.DesignStepSpec, "artifact-208", 4, true)
	assertRoleMessages(t, pool, gatedRoot, []record.RoleMessage{
		{Role: treeRoleToken(t, gatedRoot, claim.RoleMerger), Body: opened},
		{Role: treeRoleToken(t, gatedRoot, claim.RoleArchitect), Body: opened},
	})
}

// seedMergingRoot records the gated tree's root in merging, where its merger holds the phase the
// design gate holds: the one phase a gate that moves under a running worker can let merge a head
// whose design nobody approved.
func seedMergingRoot(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	seedIssue(t, pool, record.Issue{Key: gatedRoot, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: gatedRoot, Project: "LEGION", Title: "root", Phase: phase.Merging, Generation: 4, Status: "retro", Rank: "U"})
}

// gateMovedBody is the whole message a role is sent for one move of LEGION-208's design gate
// (workflow's tellGateMoved).
func gateMovedBody(step record.DesignStep, document string, version int, open bool) string {
	if open {
		return fmt.Sprintf("The design gate of LEGION-208 opened: a person approved version %d of its %s document (%s), "+
			"and every step the deployment gates now stands open, so the design you are working to is the approved one. "+
			"A merge the closed gate held is released.", version, step, document)
	}
	return fmt.Sprintf("The design gate of LEGION-208 closed: version %d of its %s document (%s) is with its reviewer, "+
		"so the design you are working to may change. Your phase goes on; the merge waits until every gated step "+
		"stands open again.", version, step, document)
}

// treeRoleToken is the Envoy role token of one issue's role, the address a role message carries.
func treeRoleToken(t *testing.T, issue string, role claim.Role) string {
	t.Helper()
	token, err := claim.NewToken("legion", issue, role)
	if err != nil {
		t.Fatalf("role token of %s's %s: %v", issue, role, err)
	}
	return string(token)
}
