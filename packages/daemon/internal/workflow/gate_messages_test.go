package workflow

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A new version of an approved spec closes the tree's design gate under the workers already
// running on it, and its approval opens it again. The close sends one message naming the version
// to every worker holding a phase in the tree and to the root architect, so nobody goes on
// building to a design the person is being asked about again (dispatch://INFRA-714/spec R5). The
// opening reaches each of them once as well: here each took version 3 and is owed version 4, so
// each is told the opening by the task that carries that news, which leads with it. None is sent
// the opened message beside that task: each told row records the opening as owed, and the approval
// queues the reconcile that sends the news.
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
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 3, ApprovedVersion: new(3), EverOpened: true})
	seedSlot(t, pool, record.Slot{Issue: "LEGION-208", Index: 0, AdmittedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)})
	took3 := map[record.DesignStep]int{record.DesignStepSpec: 3}
	seedTold(t, pool, "LEGION-208", record.AudienceHolder, took3)
	seedTold(t, pool, "LEGION-209", record.AudienceHolder, took3)
	seedTold(t, pool, "LEGION-208", record.AudienceArchitect, took3)
	engine := testEngine(config.DesignGateRootIssues, nil)

	applyMirrored(t, pool, engine, "version-4", intake.DispatchArtifact{
		Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactVersion, Version: 4})
	assertOpeningsOwed(t, pool, "LEGION-208")
	applyMirrored(t, pool, engine, "approved-4", intake.DispatchArtifact{
		Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactApproved, Version: 4})

	closed := gateMovedBody(record.DesignStepSpec, "artifact-208", 4, false)
	assertRoleMessages(t, pool, "LEGION-208", []record.RoleMessage{
		{Role: treeRoleToken(t, "LEGION-208", claim.RolePlanner), Body: closed},
		{Role: treeRoleToken(t, "LEGION-208", claim.RoleArchitect), Body: closed},
	})
	assertRoleMessages(t, pool, "LEGION-209", []record.RoleMessage{
		{Role: treeRoleToken(t, "LEGION-209", claim.RoleTester), Body: closed},
	})
	assertRoleMessages(t, pool, "LEGION-210", nil)
	assertOpeningsOwed(t, pool, "LEGION-208", "LEGION-208/architect", "LEGION-208/holder", "LEGION-209/holder")
	assertOutboxCount(t, pool, "design_changes", 1)
}

// The opening is decided for each holder in one place (openingDelivery): a holder owed news of the
// approved design is told the opening by the task that carries that news and is sent no role
// message, its told row recording the opening as owed; a holder owed nothing is sent the opened
// message with the clause that nothing its issue holds changed since the version its phase took.
// Here a new version of the Verification document is approved: its checks are the root's alone, so
// the root's reviewer and the root architect are owed it, and the child is not.
func TestAnOpeningGoesAsNewsToAnOwedReviewerAndAsTheMessageWithTheClauseToAnUnowedChild(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: gatedRoot, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: gatedRoot, Project: "LEGION", Title: "root", Phase: phase.Reviewing, Generation: 4, Status: "needs_review", Rank: "U"})
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: gatedRoot, Project: "LEGION", Title: "a child at work", Parent: new(gatedRoot), Phase: phase.Implementing,
		Generation: 4, Status: "in_progress", Rank: "V"})
	seedGate(t, pool, record.DesignGate{Issue: gatedRoot, Step: record.DesignStepCriteria, ArtifactID: criteriaDoc,
		LatestVersion: 1, ApprovedVersion: new(1), EverOpened: true})
	seedGate(t, pool, record.DesignGate{Issue: gatedRoot, Step: record.DesignStepSpec, ArtifactID: "artifact-208",
		LatestVersion: 3, ApprovedVersion: new(3), EverOpened: true})
	seedSlot(t, pool, record.Slot{Issue: gatedRoot, Index: 0, AdmittedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)})
	took := map[record.DesignStep]int{record.DesignStepCriteria: 1, record.DesignStepSpec: 3}
	seedTold(t, pool, gatedRoot, record.AudienceHolder, took)
	seedTold(t, pool, "LEGION-209", record.AudienceHolder, took)
	seedTold(t, pool, gatedRoot, record.AudienceArchitect, took)
	engine := stepsEngine(twoSteps(), nil)

	applyMirrored(t, pool, engine, "criteria-2", intake.DispatchArtifact{
		Key: gatedRoot, ArtifactID: criteriaDoc, Kind: intake.DispatchArtifactVersion, Version: 2})
	applyMirrored(t, pool, engine, "criteria-approved-2", stepApproval(gatedRoot, criteriaDoc, 2, record.EventMark{Seq: 11}))

	closed := gateMovedBody(record.DesignStepCriteria, criteriaDoc, 2, false)
	opened := gateMovedBody(record.DesignStepCriteria, criteriaDoc, 2, true)
	assertRoleMessages(t, pool, gatedRoot, []record.RoleMessage{
		{Role: treeRoleToken(t, gatedRoot, claim.RoleReviewer), Body: closed},
		{Role: treeRoleToken(t, gatedRoot, claim.RoleArchitect), Body: closed},
	})
	assertRoleMessages(t, pool, "LEGION-209", []record.RoleMessage{
		{Role: treeRoleToken(t, "LEGION-209", claim.RoleImplementer), Body: closed},
		{Role: treeRoleToken(t, "LEGION-209", claim.RoleImplementer), Body: opened + " " + nothingChangedClause(1)},
	})
	assertOpeningsOwed(t, pool, gatedRoot, gatedRoot+"/architect", gatedRoot+"/holder")
}

// A version whose only change is an answered decision block arrives as Dispatch writes it: the
// approval it carries forward first, then the version (docs/mutation.go appends the carried
// approval before the version), so the gate goes from open to open and never moves. Nothing is
// sent: no role message, and no opening is owed to anyone. The carried approval raises the gate's
// approved version, so it queues the reconcile, which finds the change decision-only and raises
// what each holder took without a task (the daemon's design_changes executor).
func TestADecisionOnlyCarryFromOpenToOpenSendsNothing(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: gatedRoot, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: gatedRoot, Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 4, Status: "in_progress", Rank: "U"})
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: gatedRoot, Project: "LEGION", Title: "a child under test", Parent: new(gatedRoot), Phase: phase.Testing,
		Generation: 4, Status: "testing", Rank: "V"})
	seedGate(t, pool, record.DesignGate{Issue: gatedRoot, ArtifactID: "artifact-208", LatestVersion: 4, ApprovedVersion: new(4), EverOpened: true})
	seedSlot(t, pool, record.Slot{Issue: gatedRoot, Index: 0, AdmittedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)})
	took4 := map[record.DesignStep]int{record.DesignStepSpec: 4}
	seedTold(t, pool, gatedRoot, record.AudienceHolder, took4)
	seedTold(t, pool, "LEGION-209", record.AudienceHolder, took4)
	seedTold(t, pool, gatedRoot, record.AudienceArchitect, took4)
	engine := testEngine(config.DesignGateRootIssues, nil)

	applyMirrored(t, pool, engine, "carried-5", intake.DispatchArtifact{Key: gatedRoot, ArtifactID: "artifact-208",
		Kind: intake.DispatchArtifactApproved, Version: 5, Reason: intake.CarriedApprovalReasonPrefix + " 4.", Mark: record.EventMark{Seq: 11}})
	applyMirrored(t, pool, engine, "version-5", intake.DispatchArtifact{Key: gatedRoot, ArtifactID: "artifact-208",
		Kind: intake.DispatchArtifactVersion, Version: 5})

	if gate := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]; gate.LatestVersion != 5 || gate.ApprovedVersion == nil || *gate.ApprovedVersion != 5 {
		t.Fatalf("the spec gate = %+v, want it at version 5 and approved at 5: the carry this case reads never landed", gate)
	}
	assertOutboxCount(t, pool, "role_message", 0)
	assertOpeningsOwed(t, pool, gatedRoot)
	assertOutboxCount(t, pool, "design_changes", 1)
}

// A changes request closes the gate on the version its holders took, and a person's approval of
// that same version opens it again. Nothing any holder took changed, so nobody is owed a news task:
// every holder told the gate closed is sent the opened message, with the clause that nothing its
// issue holds changed since that version, and the architect with the words for the tree.
func TestAChangesRequestThenARestoreTellsEveryHolderNothingChanged(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: gatedRoot, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: gatedRoot, Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 4, Status: "in_progress", Rank: "U"})
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: gatedRoot, Project: "LEGION", Title: "a child under test", Parent: new(gatedRoot), Phase: phase.Testing,
		Generation: 4, Status: "testing", Rank: "V"})
	seedGate(t, pool, record.DesignGate{Issue: gatedRoot, ArtifactID: "artifact-208", LatestVersion: 4, ApprovedVersion: new(4),
		ApprovedMark: record.EventMark{Seq: 9}, EverOpened: true})
	seedSlot(t, pool, record.Slot{Issue: gatedRoot, Index: 0, AdmittedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)})
	took4 := map[record.DesignStep]int{record.DesignStepSpec: 4}
	seedTold(t, pool, gatedRoot, record.AudienceHolder, took4)
	seedTold(t, pool, "LEGION-209", record.AudienceHolder, took4)
	seedTold(t, pool, gatedRoot, record.AudienceArchitect, took4)
	engine := testEngine(config.DesignGateRootIssues, nil)

	applyMirrored(t, pool, engine, "changes-4", intake.DispatchArtifact{Key: gatedRoot, ArtifactID: "artifact-208",
		Kind: intake.DispatchArtifactChangesRequested, Version: 4, Reason: "name the rollback path", Mark: record.EventMark{Seq: 11}})
	applyMirrored(t, pool, engine, "restored-4", stepApproval(gatedRoot, "artifact-208", 4, record.EventMark{Seq: 12}))

	closed := gateMovedBody(record.DesignStepSpec, "artifact-208", 4, false)
	opened := gateMovedBody(record.DesignStepSpec, "artifact-208", 4, true)
	assertRoleMessages(t, pool, gatedRoot, []record.RoleMessage{
		{Role: treeRoleToken(t, gatedRoot, claim.RoleImplementer), Body: closed},
		{Role: treeRoleToken(t, gatedRoot, claim.RoleArchitect), Body: closed},
		{Role: treeRoleToken(t, gatedRoot, claim.RoleImplementer), Body: opened + " " + nothingChangedClause(4)},
		{Role: treeRoleToken(t, gatedRoot, claim.RoleArchitect), Body: opened + " " + treeNothingChangedClause(4)},
	})
	assertRoleMessages(t, pool, "LEGION-209", []record.RoleMessage{
		{Role: treeRoleToken(t, "LEGION-209", claim.RoleTester), Body: closed},
		{Role: treeRoleToken(t, "LEGION-209", claim.RoleTester), Body: opened + " " + nothingChangedClause(4)},
	})
	assertOpeningsOwed(t, pool, gatedRoot)
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
// under the steps INFRA-1272 gates). The opening is told at the spec's own approval and at no
// earlier event: the merger and the architect each took the versions before the two new ones, so
// each is owed their news, and each is told the opening by the task that carries it, its told row
// recording the opening as owed once the spec's approval opens the gate, and not before.
func TestTheGateMessageIsOfEveryGatedStepAndNotTheOneTheEventNames(t *testing.T) {
	pool := migratedPool(t)
	seedMergingRoot(t, pool)
	seedGate(t, pool, record.DesignGate{Issue: gatedRoot, Step: record.DesignStepCriteria, ArtifactID: criteriaDoc,
		LatestVersion: 1, ApprovedVersion: new(1), EverOpened: true})
	seedGate(t, pool, record.DesignGate{Issue: gatedRoot, Step: record.DesignStepSpec, ArtifactID: "artifact-208",
		LatestVersion: 3, ApprovedVersion: new(3), EverOpened: true})
	seedSlot(t, pool, record.Slot{Issue: gatedRoot, Index: 0, AdmittedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)})
	took := map[record.DesignStep]int{record.DesignStepCriteria: 1, record.DesignStepSpec: 3}
	seedTold(t, pool, gatedRoot, record.AudienceHolder, took)
	seedTold(t, pool, gatedRoot, record.AudienceArchitect, took)
	engine := stepsEngine(twoSteps(), nil)

	// The spec's new version closes the standing gate; the Verification document's own new
	// version finds it closed already, and the approval of that document leaves the spec closed.
	// One message for the move, and none for the two events that move nothing.
	applyMirrored(t, pool, engine, "spec-4", intake.DispatchArtifact{
		Key: gatedRoot, ArtifactID: "artifact-208", Kind: intake.DispatchArtifactVersion, Version: 4})
	applyMirrored(t, pool, engine, "criteria-2", intake.DispatchArtifact{
		Key: gatedRoot, ArtifactID: criteriaDoc, Kind: intake.DispatchArtifactVersion, Version: 2})
	applyMirrored(t, pool, engine, "criteria-approved-2", stepApproval(gatedRoot, criteriaDoc, 2, record.EventMark{Seq: 11}))
	closed := []record.RoleMessage{
		{Role: treeRoleToken(t, gatedRoot, claim.RoleMerger), Body: gateMovedBody(record.DesignStepSpec, "artifact-208", 4, false)},
		{Role: treeRoleToken(t, gatedRoot, claim.RoleArchitect), Body: gateMovedBody(record.DesignStepSpec, "artifact-208", 4, false)},
	}
	assertRoleMessages(t, pool, gatedRoot, closed)
	assertOpeningsOwed(t, pool, gatedRoot)

	// The spec's own approval is the one that opens every gated step, so it is the one told.
	applyMirrored(t, pool, engine, "spec-approved-4", stepApproval(gatedRoot, "artifact-208", 4, record.EventMark{Seq: 12}))
	assertRoleMessages(t, pool, gatedRoot, closed)
	assertOpeningsOwed(t, pool, gatedRoot, gatedRoot+"/architect", gatedRoot+"/holder")
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
// approval the registration carries moves no gate afterwards. The merger and the architect took the
// version the registration finds approved (a changes request had closed the gate on it), so each
// is owed no news, and each is sent the opened message with the words that nothing changed since.
func TestARegistrationThatOpensTheStandingGateTellsTheMerger(t *testing.T) {
	pool := migratedPool(t)
	seedMergingRoot(t, pool)
	seedGate(t, pool, record.DesignGate{Issue: gatedRoot, Step: record.DesignStepCriteria, ArtifactID: criteriaDoc,
		LatestVersion: 1, ApprovedVersion: new(1)})
	// The spec step stands at a version with its reviewer, so the merger's hold holds it.
	seedGate(t, pool, record.DesignGate{Issue: gatedRoot, Step: record.DesignStepSpec, ArtifactID: "artifact-208",
		LatestVersion: 4})
	seedSlot(t, pool, record.Slot{Issue: gatedRoot, Index: 0, AdmittedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)})
	took := map[record.DesignStep]int{record.DesignStepCriteria: 1, record.DesignStepSpec: 4}
	seedTold(t, pool, gatedRoot, record.AudienceHolder, took)
	seedTold(t, pool, gatedRoot, record.AudienceArchitect, took)
	engine := stepsEngine(twoSteps(), nil)

	applyMirrored(t, pool, engine, "register-approved-4", intake.GateRegistered{Issue: gatedRoot, Step: record.DesignStepSpec,
		ArtifactID: "artifact-208", Version: 4, DispatchVersion: 4, ApprovedVersion: 4,
		ApprovedMark: record.EventMark{Seq: 12}, Since: record.EventMark{Seq: 13}})

	opened := gateMovedBody(record.DesignStepSpec, "artifact-208", 4, true)
	assertRoleMessages(t, pool, gatedRoot, []record.RoleMessage{
		{Role: treeRoleToken(t, gatedRoot, claim.RoleMerger), Body: opened + " " + nothingChangedClause(4)},
		{Role: treeRoleToken(t, gatedRoot, claim.RoleArchitect), Body: opened + " " + treeNothingChangedClause(4)},
	})
	assertOpeningsOwed(t, pool, gatedRoot)
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

// nothingChangedClause is the clause the opened message carries to a holder owed no news, whose
// phase took version of the step's document (openingDelivery).
func nothingChangedClause(version int) string {
	return fmt.Sprintf("Nothing your issue holds changed since version %d, which your phase took.", version)
}

// treeNothingChangedClause is the same clause in the words the root architect is sent, which took
// version for the tree.
func treeNothingChangedClause(version int) string {
	return fmt.Sprintf("Nothing the tree holds changed since version %d, which you took.", version)
}

// seedTold records what issue's audience took of its tree's approved design.
func seedTold(t *testing.T, pool *pgxpool.Pool, issue string, audience record.DesignAudience, versions map[record.DesignStep]int) {
	t.Helper()
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return record.NewStore().RaiseTold(t.Context(), tx, issue, audience, versions, time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC))
	})
}

// assertOpeningsOwed holds the told rows of tree that owe the opening, each `<issue>/<audience>`,
// to want, in any order.
func assertOpeningsOwed(t *testing.T, pool *pgxpool.Pool, tree string, want ...string) {
	t.Helper()
	var owed []string
	seedRecord(t, pool, func(tx pgx.Tx) error {
		rows, err := record.NewStore().TreeTold(t.Context(), tx, tree)
		for _, row := range rows {
			if row.OpeningOwed {
				owed = append(owed, row.Issue+"/"+string(row.Audience))
			}
		}
		return err
	})
	slices.Sort(owed)
	want = slices.Sorted(slices.Values(want))
	if !slices.Equal(owed, want) {
		t.Fatalf("the told rows of %s owing the opening = %v, want %v", tree, owed, want)
	}
}
