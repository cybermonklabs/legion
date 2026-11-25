package api

//ci:reads packages/contracts/fixtures/dispatch-version-blocks/* servedVersion reads each answer a real Dispatch served, for the route's tests

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// planDoc is the plan document of the tree root LEGION-208 these tests give Legion, and childDoc
// the plan document of LEGION-209, a child of that tree.
const (
	planDoc  = "6a1f0c3e-2b7d-4e5a-9c81-3d4f5e6a7b80"
	childDoc = "7b2e1d4f-3c8e-4f6b-8d92-4e5f6a7b8c91"
)

// taskBlock is one `plan_task` block of a plan document as Dispatch gives it, naming steps when
// steps is not empty.
func taskBlock(id, steps string) dispatch.DocumentBlock {
	attributes := map[string]string{}
	if steps != "" {
		attributes["steps"] = steps
	}
	return dispatch.DocumentBlock{ID: id, Type: plan.TaskBlockType, Attributes: attributes, Text: "Do the change."}
}

// planHarness is the architect harness with the tree root LEGION-208 recorded at admitted in its
// first run, before its gate opens: the window in which its owner gives the plan. Dispatch shows
// planDoc at version 9 and childDoc at version 3, and holds no version of either until a case
// gives one (givePlan).
func planHarness(t *testing.T) (*harness, *factRecorder, *statusRecorder) {
	t.Helper()
	h, facts, statuses := newArchitectHarness(t, nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Admitted)
	statuses.documents = map[string]string{planDoc: "LEGION-208", childDoc: "LEGION-209"}
	statuses.approvals = map[string]dispatch.Approval{planDoc: {State: "none", LatestVersion: 9}, childDoc: {State: "none", LatestVersion: 3}}
	statuses.plans = map[string]dispatch.DocumentVersion{}
	return h, facts, statuses
}

// givePlan makes version of the document artifactID hold blocks, on the issue the fake files the
// document under.
func (r *statusRecorder) givePlan(artifactID string, version int, blocks ...dispatch.DocumentBlock) {
	r.plans[planVersion(artifactID, version)] = dispatch.DocumentVersion{ArtifactID: artifactID, IssueKey: r.documents[artifactID], Version: version, Blocks: blocks}
}

func registerPlan(h *harness, grant, issue, artifactID string, version int) *httptest.ResponseRecorder {
	return h.request(http.MethodPost, "/legion/v1/plans/register", map[string]any{
		"grantId": grant, "issue": issue, "artifactId": artifactID, "version": version,
	}, nil)
}

// assertPlanRefused holds the answer to a 409 PLAN_REFUSED with exactly reasons, in their order,
// and the sentence that holds every one of them.
func assertPlanRefused(t *testing.T, recorder *httptest.ResponseRecorder, reasons ...PlanReason) {
	t.Helper()
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", recorder.Code, recorder.Body)
	}
	var refusal PlanRefusal
	decodeInto(t, recorder, &refusal)
	texts := make([]string, len(reasons))
	for i, reason := range reasons {
		texts[i] = reason.Reason
	}
	want := PlanRefusal{Code: "PLAN_REFUSED", Error: "Legion refused this plan and recorded nothing: " + strings.Join(texts, " "), Reasons: reasons}
	if !reflect.DeepEqual(refusal, want) {
		t.Fatalf("refusal = %+v, want %+v", refusal, want)
	}
}

// givenPlan is the fact a plan of one task a with steps records.
func givenPlan(issue string, generation uint64, by claim.Token, artifactID string, version int, steps ...plan.Step) intake.PlanGiven {
	return intake.PlanGiven{Issue: issue, Generation: generation, GivenBy: by, Plan: plan.Plan{ArtifactID: artifactID, Version: version,
		Tasks: []plan.Task{{ID: "a", Steps: steps, Text: "Do the change."}}}}
}

var implementerReviewer = []plan.Step{{Role: "implementer"}, {Role: "reviewer"}}

// register_plan takes a plan from the architect of the issue's tree alone, at a version Dispatch
// has, and answers each cause of a plan this release does not carry out with its own reason
// (plan.Supported) and a role nobody defines with plan.Validate's: both checks run, so a plan
// with a cause of each kind is answered both, and none of them records anything. A plan both
// checks take is recorded as the one fact the workflow reads (intake.PlanGiven) and answered with
// its tasks; the same version given again is that fact, answered the same.
func TestRegisterPlanTakesAPlanAndRefusesWithReasons(t *testing.T) {
	h, facts, statuses := planHarness(t)
	architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
	worker := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	statuses.givePlan(planDoc, 1, taskBlock("a", "implementer reviewer"), taskBlock("b", "implementer reviewer"))
	statuses.givePlan(planDoc, 2, taskBlock("a", "implementer prober"))
	statuses.givePlan(planDoc, 3, taskBlock("a", "implementer reviewer"), taskBlock("b", "implementer prober"))
	statuses.givePlan(planDoc, 4, taskBlock("a", "implementer reviewer"))
	statuses.givePlan(planDoc, 5, taskBlock("a", ""))
	statuses.givePlan(planDoc, 6, taskBlock(plan.DefaultTask, ""), taskBlock("a", "implementer reviewer"))

	refused(t, registerPlan(h, worker.grant(t), "LEGION-208", planDoc, 4), http.StatusForbidden, "ARCHITECT_REQUIRED")
	refusedSaying(t, registerPlan(h, architect.grant(t), "LEGION-208", planDoc, 10), http.StatusConflict, "VERSION_NOT_IN_DISPATCH",
		"Dispatch shows "+planDoc+" at version 9, not 10; give register_plan a version that Dispatch has")
	twoTasks := PlanReason{Reason: "the plan has 2 tasks (a, b), and this release runs one task for each issue. This release of Legion does not carry it out yet."}
	assertPlanRefused(t, registerPlan(h, architect.grant(t), "LEGION-208", planDoc, 1), twoTasks)
	assertPlanRefused(t, registerPlan(h, architect.grant(t), "LEGION-208", planDoc, 2),
		PlanReason{Task: "a", Reason: "step 2 of task a names the role prober, which neither the deployment nor the plan defines."})
	assertPlanRefused(t, registerPlan(h, architect.grant(t), "LEGION-208", planDoc, 3), twoTasks,
		PlanReason{Task: "b", Reason: "step 2 of task b names the role prober, which neither the deployment nor the plan defines."})
	// A version that keeps the task of the default steps and adds a second is a plan of two tasks
	// like any other: the route refuses it, so the workflow never takes it.
	assertPlanRefused(t, registerPlan(h, architect.grant(t), "LEGION-208", planDoc, 6),
		PlanReason{Reason: "the plan has 2 tasks (default, a), and this release runs one task for each issue. This release of Legion does not carry it out yet."})
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("refused registrations recorded %#v, want nothing", got)
	}

	for range 2 {
		taken := registerPlan(h, architect.grant(t), "LEGION-208", planDoc, 4)
		if taken.Code != http.StatusOK || strings.TrimSpace(taken.Body.String()) != `{"plan":{"version":4,"tasks":[{"id":"a","steps":["implementer","reviewer"]}]}}` {
			t.Fatalf("register version 4 = %d %s, want 200 with its one task and two steps", taken.Code, taken.Body)
		}
	}
	// A task that names no steps runs the project's default steps when its work starts, and the
	// answer says it names none rather than guessing what the project will run then.
	if bare := registerPlan(h, architect.grant(t), "LEGION-208", planDoc, 5); bare.Code != http.StatusOK ||
		strings.TrimSpace(bare.Body.String()) != `{"plan":{"version":5,"tasks":[{"id":"a","steps":[]}]}}` {
		t.Fatalf("register version 5 = %d %s, want 200 with task a naming no step", bare.Code, bare.Body)
	}
	want := []intake.Fact{
		givenPlan("LEGION-208", 1, architect.token, planDoc, 4, implementerReviewer...),
		givenPlan("LEGION-208", 1, architect.token, planDoc, 5),
	}
	if got := facts.recorded(); !reflect.DeepEqual(got, want) {
		t.Fatalf("facts = %#v, want %#v: one for each version taken, the second registration of version 4 being the first's", got, want)
	}
	assertProcessedEvent(t, h, "plan:LEGION-208:1:"+planDoc+":4")
}

// planReadFailing is the record store whose read of an issue's plan fails while fail is set, as a
// database that refuses the read would. Every other read and write is the store's own.
type planReadFailing struct {
	record.Store
	fail bool
}

func (p *planReadFailing) Plan(ctx context.Context, tx pgx.Tx, issue string, generation uint64) (*record.StoredPlan, error) {
	if p.fail {
		return nil, errors.New("the read of the plan failed")
	}
	return p.Store.Plan(ctx, tx, issue, generation)
}

// register_plan reads where each task of the plan stands once the fact is taken (runningAt), on
// the first registration and on its retry alike: the one path of `running`. When that read fails,
// the registration is answered 500 RECORD_UNAVAILABLE with the version taken, and a retry while
// the read still fails is answered the same and records nothing more. The fault is a registration
// answered 200 without `running`, which would tell the owner that no step of the plan runs. The
// control is the same registration once the read works again: it is answered 200 with the plan.
func TestARegistrationWhoseReadOfTheRecordFailsIsAnsweredRecordUnavailable(t *testing.T) {
	h, facts, statuses := planHarness(t)
	records := &planReadFailing{Store: record.NewStore(), fail: true}
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken,
		Controller: h.store, Grants: credential.New(nil), Pool: h.store.Pool(), Record: records,
		Handlers: []intake.Handler{facts}, Dispatch: statuses,
	}).Handler
	architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
	statuses.givePlan(planDoc, 4, taskBlock("a", "implementer reviewer"))
	const unread = "Legion took version 4 of the plan, but could not read where its tasks stand; give register_plan the same version again to read them"
	given := []intake.Fact{givenPlan("LEGION-208", 1, architect.token, planDoc, 4, implementerReviewer...)}

	refusedSaying(t, registerPlan(h, architect.grant(t), "LEGION-208", planDoc, 4), http.StatusInternalServerError, "RECORD_UNAVAILABLE", unread)
	if got := facts.recorded(); !reflect.DeepEqual(got, given) {
		t.Fatalf("facts after the first registration = %#v, want %#v: the version is taken though its answer could not be read", got, given)
	}
	refusedSaying(t, registerPlan(h, architect.grant(t), "LEGION-208", planDoc, 4), http.StatusInternalServerError, "RECORD_UNAVAILABLE", unread)
	records.fail = false
	const taken = `{"plan":{"version":4,"tasks":[{"id":"a","steps":["implementer","reviewer"]}]}}`
	if again := registerPlan(h, architect.grant(t), "LEGION-208", planDoc, 4); again.Code != http.StatusOK || strings.TrimSpace(again.Body.String()) != taken {
		t.Fatalf("the retry once the read works = %d %s, want 200 with its one task", again.Code, again.Body)
	}
	if got := facts.recorded(); !reflect.DeepEqual(got, given) {
		t.Fatalf("facts = %#v, want %#v: the one registration taken, and nothing for either retry", got, given)
	}
}

// The reader of 219 takes every field of register_plan's answer as optional, since no plugin code
// reads one (legion-api.ts's LEGION_ANSWER_READS: a field that set does not name is optional). So
// the reader takes a task written without `steps`, and the daemon holds what it does not check:
// every task of the answer, as shapeAnswer writes it to a caller at OwnerPlanContract, carries
// `steps` as a list, never null and never absent, a task whose block names none included.
// TestRegisterPlanTakesAPlanAndRefusesWithReasons holds the same for one task through the route.
func TestRegisterPlanWritesEveryTasksStepsAsAList(t *testing.T) {
	answer := PlanRegisterResponse{Plan: registeredPlan(plan.Plan{ArtifactID: planDoc, Version: 7,
		Tasks: []plan.Task{{ID: "a"}, {ID: "b", Steps: []plan.Step{{Role: "implementer"}}}}}, nil)}
	shaped, _ := shapeAnswer(AnswerPlanRegister, answer, OwnerPlanContract, OwnerPlanContract)
	encoded, err := json.Marshal(shaped)
	if err != nil {
		t.Fatalf("marshal the answer: %v", err)
	}
	const want = `{"plan":{"version":7,"tasks":[{"id":"a","steps":[]},{"id":"b","steps":["implementer"]}]}}`
	if string(encoded) != want {
		t.Errorf("register_plan answers %s, want %s: every task carries `steps`, a list and never null", encoded, want)
	}
}

// register_plan's answer names the act each running task stands at (contract 262), as a list of
// one entry (record.StoredPlan.Running gives one for each running task): step 2 of task a, the
// reviewer. A task that does not run, b, carries no `running` key at all, rather than an empty or
// a null list.
func TestRegisterPlanNamesTheActEachRunningTaskStandsAt(t *testing.T) {
	running := []record.RunningStep{{Task: "a", Place: record.TaskPlace{Kind: record.PlaceStep, Position: 2}, Role: claim.RoleReviewer}}
	answer := PlanRegisterResponse{Plan: registeredPlan(plan.Plan{ArtifactID: planDoc, Version: 3, Tasks: []plan.Task{
		{ID: "a", Steps: []plan.Step{{Role: "implementer"}, {Role: "reviewer"}}}, {ID: "b"}}}, running)}
	shaped, _ := shapeAnswer(AnswerPlanRegister, answer, runningStepContract, runningStepContract)
	encoded, err := json.Marshal(shaped)
	if err != nil {
		t.Fatalf("marshal the answer: %v", err)
	}
	const want = `{"plan":{"version":3,"tasks":[{"id":"a","steps":["implementer","reviewer"],"running":[` +
		`{"kind":"step","place":2,"role":"reviewer"}]},{"id":"b","steps":[]}]}}`
	if string(encoded) != want {
		t.Errorf("register_plan answers %s,\nwant %s: the act task a stands at, and no `running` on task b", encoded, want)
	}
}

// servedVersion is version 1 of the document artifactID as the real client
// (dispatch.HTTPClient.PlanDocument) reads it from a Dispatch that answers with status and the
// fixture name, an answer under packages/contracts/fixtures/dispatch-version-blocks that a real
// Dispatch served for ACME-1's plan.md: the version, or that Dispatch's refusal of it.
func servedVersion(t *testing.T, name string, status int, artifactID string) (dispatch.DocumentVersion, error) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "fixtures", "dispatch-version-blocks", name))
	if err != nil {
		t.Fatalf("read the fixture %s: %v", name, err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/artifacts/" + artifactID + "/versions/1":
			w.WriteHeader(status)
			_, _ = w.Write(body)
		case "/api/v1/artifacts/" + artifactID:
			_, _ = w.Write([]byte(`{"id":"` + artifactID + `","issue_key":"ACME-1","slug":"plan-md"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	return dispatch.New(server.URL, "dispatch-test-token").PlanDocument(t.Context(), artifactID, 1)
}

// What register_plan answers for a document it cannot read a plan from, each before anything is
// recorded: Dispatch's version without blocks, a document of another issue, a failed read, an
// older build's refusal of a version that holds `parent` (409 DOC_NEWER_CONTENT, read by the real
// client from the body that build served), a block outside the plan's grammar and a document with
// no task block. The refusal is a failed read the owner retries, never a refusal of the plan. Its
// control is a planAt that reads a 409 as a version with no blocks: the answer is then 409
// PLAN_REFUSED, the plan refused for having no task.
func TestRegisterPlanAnswersADocumentItCannotRead(t *testing.T) {
	h, facts, statuses := planHarness(t)
	architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
	statuses.givePlan(planDoc, 1, taskBlock("a", "Implementer!"))
	statuses.givePlan(planDoc, 2, dispatch.DocumentBlock{ID: "note", Type: "callout", Attributes: map[string]string{}, Text: "No task here."})

	assertPlanRefused(t, registerPlan(h, architect.grant(t), "LEGION-208", planDoc, 1),
		PlanReason{Reason: `task a: the step "Implementer!" is neither a role nor a role:key, where each matches [a-z][a-z0-9-]*.`})
	assertPlanRefused(t, registerPlan(h, architect.grant(t), "LEGION-208", planDoc, 2),
		PlanReason{Reason: "the plan has no task block, so it gives Legion no work."})
	refusedSaying(t, registerPlan(h, architect.grant(t), "LEGION-208", childDoc, 1), http.StatusNotFound, "ARTIFACT_NOT_ON_ISSUE",
		childDoc+" is not a document of LEGION-208")
	statuses.planErr = dispatch.ErrNoBlocks
	refusedSaying(t, registerPlan(h, architect.grant(t), "LEGION-208", planDoc, 1), http.StatusBadGateway, "PLAN_UNREADABLE",
		"Dispatch gave version 1 of "+planDoc+" without its blocks, so Legion cannot read a plan from it: that Dispatch predates the plan's blocks. "+
			"Nothing was recorded; give the plan again once Dispatch is upgraded")
	statuses.planErr = errors.New("connection reset")
	refusedSaying(t, registerPlan(h, architect.grant(t), "LEGION-208", planDoc, 1), http.StatusBadGateway, "DISPATCH_FAILED",
		"Dispatch read of "+planDoc+" at version 1 failed; retry register_plan")
	if _, statuses.planErr = servedVersion(t, "older-build-newer-content.json", http.StatusConflict, planDoc); statuses.planErr == nil {
		t.Fatal("the real client read the older build's refusal as a version")
	}
	refusedSaying(t, registerPlan(h, architect.grant(t), "LEGION-208", planDoc, 1), http.StatusBadGateway, "DISPATCH_FAILED",
		"Dispatch read of "+planDoc+" at version 1 failed; retry register_plan")
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("unreadable documents recorded %#v, want nothing", got)
	}
}

// register_plan reads each task block's `parent` against the document it registers (plan.ParentOf,
// which plan.FromBlocks calls), as a reference to one of its blocks names it (dispatch.DocumentRef):
// `dispatch://<KEY>/spec` for the issue's primary document, and `dispatch://<KEY>/artifact/<slug>`
// for any other. So a
// `parent` in the registered document's form puts its block under the block it names, and the
// other form names another document, whatever its id. Validate's refusals of an element and of a
// `parent` that names a block the version does not hold reach the answer among its reasons. A
// `parent` that is not one reference, and task blocks under each other, are a plan Legion cannot
// read, answered with one reason. None records anything. The fault is a route that reads a
// `parent` against no document, or against the wrong one. The control is planAt given the artifact
// form of the primary document: version 1 is refused as two tasks, and version 3 refused as an
// element with steps.
func TestRegisterPlanReadsEachParentInTheRegisteredDocument(t *testing.T) {
	h, facts, statuses := planHarness(t)
	architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
	parented := func(block dispatch.DocumentBlock, parent string) dispatch.DocumentBlock {
		block.Attributes["parent"] = parent
		return block
	}
	const spec, artifact = "dispatch://LEGION-208/spec#b-a", "dispatch://LEGION-208/artifact/plan-md#b-a"
	elementWithSteps := func(id string) PlanReason {
		return PlanReason{Task: id, Reason: "task block " + id + " is an element of task a: it stands under a, which names no sub-owner. " +
			"Legion does not carry out an element, so an element names no steps, owner or merges, and " + id + " names steps."}
	}
	twoTasks := PlanReason{Reason: "the plan has 2 tasks (a, b), and this release runs one task for each issue. This release of Legion does not carry it out yet."}
	// The fake gives planDoc no slug, so it is the primary document of LEGION-208.
	statuses.givePlan(planDoc, 1, taskBlock("a", "implementer reviewer"), parented(taskBlock("e1", ""), spec))
	statuses.givePlan(planDoc, 2, taskBlock("a", "implementer reviewer"), parented(taskBlock("e1", "implementer"), spec))
	statuses.givePlan(planDoc, 3, taskBlock("a", "implementer reviewer"), parented(taskBlock("b", "implementer reviewer"), artifact))
	statuses.givePlan(planDoc, 4, taskBlock("a", "implementer reviewer"), parented(taskBlock("b", "implementer reviewer"), "dispatch://LEGION-208/spec#b-gone"))
	statuses.givePlan(planDoc, 5, taskBlock("a", "implementer reviewer"), parented(taskBlock("b", "implementer reviewer"), spec+" "+spec))
	statuses.givePlan(planDoc, 6, parented(taskBlock("a", "implementer reviewer"), "dispatch://LEGION-208/spec#b-b"), parented(taskBlock("b", "implementer reviewer"), spec))

	if taken := registerPlan(h, architect.grant(t), "LEGION-208", planDoc, 1); taken.Code != http.StatusOK {
		t.Fatalf("register version 1 = %d %s, want 200: e1 is an element of a, which names nothing only a task carries out", taken.Code, taken.Body)
	}
	assertPlanRefused(t, registerPlan(h, architect.grant(t), "LEGION-208", planDoc, 2), elementWithSteps("e1"))
	assertPlanRefused(t, registerPlan(h, architect.grant(t), "LEGION-208", planDoc, 3), twoTasks)
	assertPlanRefused(t, registerPlan(h, architect.grant(t), "LEGION-208", planDoc, 4), twoTasks,
		PlanReason{Task: "b", Reason: "task block b names gone as its parent, which no typed block of version 4 of the plan holds. " +
			"Name a typed block of this version in parent, or leave parent out."})
	assertPlanRefused(t, registerPlan(h, architect.grant(t), "LEGION-208", planDoc, 5),
		PlanReason{Reason: `task block b: its parent "` + spec + ` ` + spec + `" is not one dispatch:// reference.`})
	assertPlanRefused(t, registerPlan(h, architect.grant(t), "LEGION-208", planDoc, 6),
		PlanReason{Reason: "task blocks a, b stand under each other, so none of them has a place in the plan."})

	// The same document, filed under a slug and not as the primary one: the artifact form names it.
	statuses.approvals[planDoc] = dispatch.Approval{State: "none", LatestVersion: 9, Slug: "plan-md"}
	assertPlanRefused(t, registerPlan(h, architect.grant(t), "LEGION-208", planDoc, 3), elementWithSteps("b"))

	got := facts.recorded()
	if len(got) != 1 {
		t.Fatalf("facts = %#v, want the one fact of version 1", got)
	}
	given, ok := got[0].(intake.PlanGiven)
	if want := []plan.Element{{ID: "e1", Of: "a", Under: "a"}}; !ok || len(given.Plan.Tasks) != 1 || given.Plan.Tasks[0].ID != "a" || !reflect.DeepEqual(given.Plan.Elements, want) {
		t.Fatalf("the fact of version 1 = %#v, want the one task a and the element %+v", got[0], want)
	}
}

// A real Dispatch's version of a plan whose t1, t2 and t3 stand under the owner's task default,
// read through the real client (servedVersion), is a plan of one task: register_plan answers 200
// with the task default alone, and records it with the three blocks as its elements. The fault is
// an element read as a task, which Legion would carry out. The control is a FromBlocks with Places
// taken out: the version is then refused as a plan of four tasks.
func TestAVersionWithElementsAnswersItsOneTask(t *testing.T) {
	const acmeDoc = "8c3f2e5a-4d9f-4a7c-9ea3-5f6a7b8c9d02"
	document, err := servedVersion(t, "plan-with-parent.json", http.StatusOK, acmeDoc)
	if err != nil {
		t.Fatalf("read the fixture's version through the client: %v", err)
	}
	h, facts, statuses := planHarness(t)
	seedIssueAt(t, h, "ACME-1", phase.Admitted)
	architect := newLiveClaim(t, h, "ACME-1", claim.RoleArchitect)
	statuses.documents[acmeDoc] = "ACME-1"
	statuses.approvals[acmeDoc] = dispatch.Approval{State: "none", LatestVersion: 1, Slug: "plan-md"}
	statuses.plans[planVersion(acmeDoc, 1)] = document

	taken := registerPlan(h, architect.grant(t), "ACME-1", acmeDoc, 1)
	if want := `{"plan":{"version":1,"tasks":[{"id":"default","steps":[]}]}}`; taken.Code != http.StatusOK || strings.TrimSpace(taken.Body.String()) != want {
		t.Fatalf("register version 1 = %d %s, want 200 %s: t1, t2 and t3 are elements of default", taken.Code, taken.Body, want)
	}
	got := facts.recorded()
	if len(got) != 1 {
		t.Fatalf("facts = %#v, want the one fact of version 1", got)
	}
	given, ok := got[0].(intake.PlanGiven)
	want := []plan.Element{{ID: "t1", Of: "default", Under: "default"}, {ID: "t2", Of: "default", Under: "default"}, {ID: "t3", Of: "default", Under: "default"}}
	if !ok || len(given.Plan.Tasks) != 1 || given.Plan.Tasks[0].ID != plan.DefaultTask || given.Plan.Tasks[0].Text != "Ship the parser." || !reflect.DeepEqual(given.Plan.Elements, want) {
		t.Fatalf("the fact of version 1 = %#v, want the one task default, its own text alone, and the elements %+v", got[0], want)
	}
}

// The rules of a later version are the workflow's, which answers each refusal of them as
// a committed refusal of the fact: the route writes it as it came, a refused plan with its
// reasons beside the code.
func TestRegisterPlanAnswersTheWorkflowsRefusals(t *testing.T) {
	for _, tc := range []struct {
		name    string
		refusal *intake.Refusal
	}{
		{"a version that changes the running task beyond its text", intake.PlanRefused([]plan.Refusal{plan.RunningTaskChanged("a")})},
		{"a version at or below the one in force", intake.PlanVersionOld("LEGION-208", 4, 4)},
		{"a version of another document", intake.PlanDocumentChanged("LEGION-208", childDoc)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, facts, statuses := planHarness(t)
			facts.refusal = tc.refusal
			architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
			statuses.givePlan(planDoc, 4, taskBlock("a", "implementer reviewer"))
			recorder := registerPlan(h, architect.grant(t), "LEGION-208", planDoc, 4)
			if tc.refusal.Reasons != nil {
				assertPlanRefused(t, recorder, PlanReason{Task: "a", Reason: "task a is running, so a later version can change only its text. " +
					"Stop it with stop_task before you change any other part of it or remove it."})
				return
			}
			refusedSaying(t, recorder, tc.refusal.Status, tc.refusal.Code, tc.refusal.Message)
		})
	}
}

// An owner gives the plan before the work starts: a root after its own start and before
// its gate opens, and a child before release_children records it, which the daemon has no record
// of yet. The route takes a plan for such a child when Dispatch's parent chain reaches the
// architect's tree and the child has not entered the workflow, and records it at generation 0;
// it answers an issue whose chain does not reach the tree, a key Dispatch does not have, one in
// the workflow already, and a recorded issue of another tree, as release_children answers them.
//
// This is the route's half: what the engine does with the fact — the
// root's gate opens and its first step is implementing, the released child's first phase is
// implementing, and the same child released with no plan given starts planning — is the engine's
// half, which reads these facts by these names (intake.PlanGiven).
func TestAPlanGivenBeforeTheWorkStartsIsTheFirstStep(t *testing.T) {
	h, facts, statuses := planHarness(t)
	architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
	seedIssueAt(t, h, "LEGION-500", phase.Admitted)
	root, otherRoot := "LEGION-208", "LEGION-299"
	statuses.issues = map[string]dispatch.Issue{
		"LEGION-208": {Key: "LEGION-208", Status: "todo", LastSeq: 7},
		"LEGION-209": {Key: "LEGION-209", Status: "backlog", Parent: &root},
		"LEGION-211": {Key: "LEGION-211", Status: "in_progress", Parent: &root},
		"LEGION-299": {Key: "LEGION-299", Status: "todo"},
		"LEGION-300": {Key: "LEGION-300", Status: "backlog", Parent: &otherRoot},
	}
	statuses.givePlan(planDoc, 4, taskBlock("a", "implementer reviewer"))
	statuses.givePlan(childDoc, 1, taskBlock("a", "implementer reviewer"))

	if recorder := registerPlan(h, architect.grant(t), "LEGION-208", planDoc, 4); recorder.Code != http.StatusOK {
		t.Fatalf("register the root's plan = %d %s, want 200", recorder.Code, recorder.Body)
	}
	if recorder := registerPlan(h, architect.grant(t), "LEGION-209", childDoc, 1); recorder.Code != http.StatusOK {
		t.Fatalf("register the unreleased child's plan = %d %s, want 200", recorder.Code, recorder.Body)
	}
	refusedSaying(t, registerPlan(h, architect.grant(t), "LEGION-300", childDoc, 1), http.StatusForbidden, "ISSUE_OUTSIDE_TREE",
		"LEGION-300 is outside the architect tree LEGION-208")
	refusedSaying(t, registerPlan(h, architect.grant(t), "LEGION-404", childDoc, 1), http.StatusNotFound, "ISSUE_NOT_FOUND",
		"LEGION-404 is not a Dispatch issue")
	refusedSaying(t, registerPlan(h, architect.grant(t), "LEGION-211", childDoc, 1), http.StatusConflict, "ISSUE_ALREADY_RELEASED",
		"LEGION-211 is in_progress in Dispatch and the daemon holds no record of it; register_plan takes a plan for an issue the daemon has not recorded only while it is triage, backlog, icebox or todo")
	refusedSaying(t, registerPlan(h, architect.grant(t), "LEGION-500", childDoc, 1), http.StatusForbidden, "ISSUE_OUTSIDE_TREE",
		"issue is outside the architect tree")

	want := []intake.Fact{
		givenPlan("LEGION-208", 1, architect.token, planDoc, 4, implementerReviewer...),
		givenPlan("LEGION-209", 0, architect.token, childDoc, 1, implementerReviewer...),
	}
	if got := facts.recorded(); !reflect.DeepEqual(got, want) {
		t.Fatalf("facts = %#v, want %#v: the root's at its run, the child's at generation 0", got, want)
	}
}

// A plan names the owner that gave it: the architect of tree LEGION-208 gives a plan for
// LEGION-209 before releasing it, and the fact names that architect. Once LEGION-209 is admitted
// again as a root of its own, the plan is its new architect's to give: that architect's plan is
// recorded at the issue's run, naming it, and the first architect, whose tree the issue has left,
// is refused.
//
// This is the route's half: that the first step of the re-admitted root
// takes no plan of an earlier owner — the default steps or its own owner's plan, the log line and
// the `plan-not-taken` notice — is the engine's half, which reads GivenBy.
func TestAPlanOfAnEarlierOwnerIsNotTaken(t *testing.T) {
	h, facts, statuses := planHarness(t)
	earlier := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
	root := "LEGION-208"
	statuses.issues = map[string]dispatch.Issue{
		"LEGION-208": {Key: "LEGION-208", Status: "todo", LastSeq: 7},
		"LEGION-209": {Key: "LEGION-209", Status: "backlog", Parent: &root},
	}
	for version := 1; version <= 3; version++ {
		statuses.givePlan(childDoc, version, taskBlock("a", "implementer reviewer"))
	}
	if recorder := registerPlan(h, earlier.grant(t), "LEGION-209", childDoc, 1); recorder.Code != http.StatusOK {
		t.Fatalf("the first owner's plan = %d %s, want 200", recorder.Code, recorder.Body)
	}

	seedIssueAt(t, h, "LEGION-209", phase.Admitted)
	owner := newLiveClaim(t, h, "LEGION-209", claim.RoleArchitect)
	if recorder := registerPlan(h, owner.grant(t), "LEGION-209", childDoc, 2); recorder.Code != http.StatusOK {
		t.Fatalf("the new owner's plan = %d %s, want 200", recorder.Code, recorder.Body)
	}
	refusedSaying(t, registerPlan(h, earlier.grant(t), "LEGION-209", childDoc, 3), http.StatusForbidden, "ISSUE_OUTSIDE_TREE",
		"issue is outside the architect tree")

	want := []intake.Fact{
		givenPlan("LEGION-209", 0, earlier.token, childDoc, 1, implementerReviewer...),
		givenPlan("LEGION-209", 1, owner.token, childDoc, 2, implementerReviewer...),
	}
	if got := facts.recorded(); !reflect.DeepEqual(got, want) {
		t.Fatalf("facts = %#v, want %#v: each plan naming the owner that gave it", got, want)
	}
}

// seedMember records key in tree at admitted in its first run, a child of the tree's root.
func seedMember(t *testing.T, h *harness, key, tree string) {
	t.Helper()
	parent := tree
	if err := store.Transact(context.Background(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		return record.NewStore().PutIssue(context.Background(), tx, record.Issue{
			Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: tree, Project: testProject, Title: key,
			Parent: &parent, Phase: phase.Admitted, Generation: 1, Status: "in_progress",
		})
	}); err != nil {
		t.Fatalf("seed %s in the tree %s: %v", key, tree, err)
	}
}

// Who owns a recorded issue is one rule, record.ArchitectOfTree: an architect of the issue's tree
// now, the root's or a sub-architect's, the rule the workflow holds the giver of a stored plan to
// at the issue's first step. A sub-architect, the architect of LEGION-211, gives a plan for
// LEGION-209 while LEGION-211 stands in the tree LEGION-208. Once LEGION-211 stands in a tree of
// its own it is no architect of that tree, whatever tree its claim was registered in:
// register_plan and stop_task both refuse it, and the root's architect is still taken.
func TestAnArchitectWhoseIssueLeftTheTreeOwnsNoIssueOfIt(t *testing.T) {
	h, facts, statuses := planHarness(t)
	seedMember(t, h, "LEGION-209", "LEGION-208")
	seedMember(t, h, "LEGION-211", "LEGION-208")
	sub := newLiveClaimIn(t, h, "LEGION-208", "LEGION-211", claim.RoleArchitect)
	root := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
	for version := 1; version <= 3; version++ {
		statuses.givePlan(childDoc, version, taskBlock("a", "implementer reviewer"))
	}
	if recorder := registerPlan(h, sub.grant(t), "LEGION-209", childDoc, 1); recorder.Code != http.StatusOK {
		t.Fatalf("the sub-architect's plan while its issue stands in the tree = %d %s, want 200", recorder.Code, recorder.Body)
	}

	seedIssueAt(t, h, "LEGION-211", phase.Admitted)
	refusedSaying(t, registerPlan(h, sub.grant(t), "LEGION-209", childDoc, 2), http.StatusForbidden, "ISSUE_OUTSIDE_TREE",
		"issue is outside the architect tree")
	refusedSaying(t, stopTask(h, sub.grant(t), "LEGION-209", "a"), http.StatusForbidden, "ISSUE_OUTSIDE_TREE",
		"issue is outside the architect tree")
	if recorder := registerPlan(h, root.grant(t), "LEGION-209", childDoc, 3); recorder.Code != http.StatusOK {
		t.Fatalf("the root's architect's plan = %d %s, want 200", recorder.Code, recorder.Body)
	}

	want := []intake.Fact{
		givenPlan("LEGION-209", 1, sub.token, childDoc, 1, implementerReviewer...),
		givenPlan("LEGION-209", 1, root.token, childDoc, 3, implementerReviewer...),
	}
	if got := facts.recorded(); !reflect.DeepEqual(got, want) {
		t.Fatalf("facts = %#v, want %#v: the sub-architect's plan while it owned the issue, then the root's, and no fact of the refused calls", got, want)
	}
}

func stopTask(h *harness, grant, issue, task string) *httptest.ResponseRecorder {
	return h.request(http.MethodPost, "/legion/v1/tasks/stop", map[string]any{"grantId": grant, "issue": issue, "task": task}, nil)
}

// stop_task is the owner's stop of one task of a recorded issue: the architect of the
// issue's tree asks for it, and the route records the one fact the workflow stops the task by
// (intake.TaskStopped), each request its own, so a task stopped, started again by a later version
// and stopped once more is stopped twice. Whether the plan holds the task and whether it is done
// is the workflow's to answer, in the words the route writes as they came.
func TestStopTaskRecordsTheOwnersStop(t *testing.T) {
	h, facts, _ := planHarness(t)
	seedIssueAt(t, h, "LEGION-500", phase.Implementing)
	architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
	worker := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)

	refused(t, stopTask(h, worker.grant(t), "LEGION-208", "a"), http.StatusForbidden, "ARCHITECT_REQUIRED")
	refusedSaying(t, stopTask(h, architect.grant(t), "LEGION-208", " "), http.StatusBadRequest, "MISSING_FIELD", "task is required")
	refusedSaying(t, stopTask(h, architect.grant(t), "LEGION-404", "a"), http.StatusNotFound, "ISSUE_NOT_FOUND", "issue is not recorded")
	refusedSaying(t, stopTask(h, architect.grant(t), "LEGION-500", "a"), http.StatusForbidden, "ISSUE_OUTSIDE_TREE", "issue is outside the architect tree")
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("refused stops recorded %#v, want nothing", got)
	}
	for range 2 {
		if stopped := stopTask(h, architect.grant(t), "LEGION-208", "a"); stopped.Code != http.StatusOK || strings.TrimSpace(stopped.Body.String()) != `{}` {
			t.Fatalf("stop task a = %d %s, want 200 {}", stopped.Code, stopped.Body)
		}
	}
	// The fact carries the generation the route read, which the workflow judges again.
	stop := intake.TaskStopped{Issue: "LEGION-208", Task: "a", By: architect.token, Generation: 1}
	if got, want := facts.recorded(), []intake.Fact{stop, stop}; !reflect.DeepEqual(got, want) {
		t.Fatalf("facts = %#v, want %#v: each stop its own fact", got, want)
	}

	for _, refusal := range []*intake.Refusal{intake.TaskNotFound("LEGION-208", "b", []string{"a"}), intake.FinishedTask("LEGION-208", "a", record.TaskDone, intake.StopOfTask)} {
		facts.refusal = refusal
		refusedSaying(t, stopTask(h, architect.grant(t), "LEGION-208", "a"), refusal.Status, refusal.Code, refusal.Message)
	}
}
