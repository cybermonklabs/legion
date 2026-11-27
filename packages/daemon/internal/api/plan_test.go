package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/steprecord"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/redact"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
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
// its tasks. No id decides whether a registration given again is the one taken (intake.ApplyFact
// applies every PlanGiven), so each registration reaches this handler, which stores no version,
// under the one id the route names it by, and is answered the same.
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
	twoSinks := PlanReason{Reason: "the plan has 2 tasks that no task needs (a, b), and the work of each would merge by itself. This release of Legion does not carry it out yet."}
	assertPlanRefused(t, registerPlan(h, architect.grant(t), "LEGION-208", planDoc, 1), twoSinks)
	assertPlanRefused(t, registerPlan(h, architect.grant(t), "LEGION-208", planDoc, 2),
		PlanReason{Task: "a", Reason: "step 2 of task a names the role prober, which neither the deployment nor the plan defines."})
	assertPlanRefused(t, registerPlan(h, architect.grant(t), "LEGION-208", planDoc, 3), twoSinks,
		PlanReason{Task: "b", Reason: "step 2 of task b names the role prober, which neither the deployment nor the plan defines."})
	// A version that keeps the task of the default steps and adds a second that neither needs is a
	// plan of two tasks whose work would each merge, like any other: the route refuses it, so the
	// workflow never takes it.
	assertPlanRefused(t, registerPlan(h, architect.grant(t), "LEGION-208", planDoc, 6),
		PlanReason{Reason: "the plan has 2 tasks that no task needs (a, default), and the work of each would merge by itself. This release of Legion does not carry it out yet."})
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("refused registrations recorded %#v, want nothing", got)
	}

	for range 2 {
		taken := registerPlan(h, architect.grant(t), "LEGION-208", planDoc, 4)
		if taken.Code != http.StatusOK || strings.TrimSpace(taken.Body.String()) != `{"plan":{"version":4,"added":[],"tasks":[{"id":"a","steps":["implementer","reviewer"],"needs":[]}]}}` {
			t.Fatalf("register version 4 = %d %s, want 200 with its one task and two steps", taken.Code, taken.Body)
		}
	}
	// A task that names no steps runs the project's default steps when its work starts, and the
	// answer says it names none rather than guessing what the project will run then.
	if bare := registerPlan(h, architect.grant(t), "LEGION-208", planDoc, 5); bare.Code != http.StatusOK ||
		strings.TrimSpace(bare.Body.String()) != `{"plan":{"version":5,"added":[],"tasks":[{"id":"a","steps":[],"needs":[]}]}}` {
		t.Fatalf("register version 5 = %d %s, want 200 with task a naming no step", bare.Code, bare.Body)
	}
	want := []intake.Fact{
		givenPlan("LEGION-208", 1, architect.token, planDoc, 4, implementerReviewer...),
		givenPlan("LEGION-208", 1, architect.token, planDoc, 4, implementerReviewer...),
		givenPlan("LEGION-208", 1, architect.token, planDoc, 5),
	}
	if got := facts.recorded(); !reflect.DeepEqual(got, want) {
		t.Fatalf("facts = %#v, want %#v: one for each registration, version 4's second among them, the store holding no version of version 4 when it is given again", got, want)
	}
	assertProcessedEvent(t, h, "plan:LEGION-208:1:"+planDoc+":4")
}

// register_plan judges a plan against the deployment the server is given (Options.Deployment),
// the roles the daemon's settings define: with checker in it, `implementer checker` is taken;
// without it, the plan is refused naming checker; and `reviewer implementer` is refused with the
// reason of a step that reads a pull request before the implementer, whatever the deployment.
func TestRegisterPlanTakesARoleTheDeploymentDefines(t *testing.T) {
	readsBeforeTheImplementer := PlanReason{Task: "a", Reason: "step 1 of task a names reviewer, and no step before it names the role implementer, whose pull request reviewer reads; a role of this deployment that reports pass or fail needs none. This release of Legion does not carry it out yet."}
	for _, tc := range []struct {
		name       string
		deployment plan.Deployment
		steps      string
		refused    []PlanReason
	}{
		{"checker in the deployment", checkerDeployment(), "implementer checker", nil},
		{"checker in no deployment", plan.Deployment{Roles: plan.BuiltinRoles()}, "implementer checker",
			[]PlanReason{{Task: "a", Reason: "step 2 of task a names the role checker, which neither the deployment nor the plan defines."}}},
		{"a review before the implementer", checkerDeployment(), "reviewer implementer", []PlanReason{readsBeforeTheImplementer}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, facts, statuses := planHarness(t)
			h.handler = NewServer("127.0.0.1", 8437, Options{Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject,
				OperatorToken: testOperatorToken, Controller: h.store, Grants: credential.New(nil), Pool: h.store.Pool(), Record: record.NewStore(),
				Handlers: []intake.Handler{facts}, Dispatch: statuses, Deployment: tc.deployment}).Handler
			architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
			statuses.givePlan(planDoc, 4, taskBlock("a", tc.steps))

			answer := registerPlan(h, architect.grant(t), "LEGION-208", planDoc, 4)

			if tc.refused != nil {
				assertPlanRefused(t, answer, tc.refused...)
				return
			}
			if answer.Code != http.StatusOK {
				t.Fatalf("register %s = %d %s, want 200", tc.steps, answer.Code, answer.Body)
			}
		})
	}
}

// The reader of 219 takes every field of register_plan's answer as optional, since no plugin code
// reads one (legion-api.ts's LEGION_ANSWER_READS: a field that set does not name is optional). So
// the reader takes a task written without `steps`, and the daemon holds what it does not check:
// every task of the answer, as shapeAnswer writes it to a caller at OwnerPlanContract, carries
// `steps` as a list, never null and never absent, a task whose block names none included. At
// tasksContract each task carries `needs` the same way, a task that needs none included, and the
// plan carries `added`, a list for none too; a caller below it is served neither
// (withoutNeedsOrAdded). TestRegisterPlanTakesAPlanAndRefusesWithReasons holds the same for one
// task through the route.
func TestRegisterPlanWritesEveryTasksStepsAsAList(t *testing.T) {
	answer := PlanRegisterResponse{Plan: registeredPlan(plan.Plan{ArtifactID: planDoc, Version: 7,
		Tasks: []plan.Task{{ID: "a"}, {ID: "b", Steps: []plan.Step{{Role: "implementer"}}, Needs: []string{"a"}}}}, nil)}
	for _, tc := range []struct {
		contract int
		want     string
	}{
		{OwnerPlanContract, `{"plan":{"version":7,"tasks":[{"id":"a","steps":[]},{"id":"b","steps":["implementer"]}]}}`},
		{entryNumber(tasksContract), `{"plan":{"version":7,"added":[],"tasks":[{"id":"a","steps":[],"needs":[]},{"id":"b","steps":["implementer"],"needs":["a"]}]}}`},
	} {
		shaped, _ := shapeAnswer(AnswerPlanRegister, answer, tc.contract, tc.contract)
		encoded, err := json.Marshal(shaped)
		if err != nil {
			t.Fatalf("marshal the answer: %v", err)
		}
		if string(encoded) != tc.want {
			t.Errorf("register_plan answers %s at %d, want %s: every task carries `steps`, and from tasksContract `needs`, each a list and never null",
				encoded, tc.contract, tc.want)
		}
	}
}

// What register_plan answers for a document it cannot read a plan from, each before anything is
// recorded: Dispatch's version without blocks, a document of another issue, a failed read, a
// block outside the plan's grammar and a document with no task block.
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
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("unreadable documents recorded %#v, want nothing", got)
	}
}

// needing is block with `needs` writing the words needs.
func needing(block dispatch.DocumentBlock, needs string) dispatch.DocumentBlock {
	block.Attributes["needs"] = needs
	return block
}

// register_plan reads each need as the task it names (plan.FromBlocks) before either check, so a
// task a reference names is needed and the plan recorded names it by its id: the first plan, b
// needing a by a reference, is taken, and its fact's b needs ["a"], b alone ending it. A word that
// names no task gets the reason no release lifts, beside a and b as two tasks that no task needs,
// and records nothing. The fault is a route that checks, and records, the words as the blocks
// write them; the control is a FromBlocks that keeps each word whole, under which b's need is
// recorded as the reference and the second plan is taken.
func TestRegisterPlanReadsEachNeedAsTheTaskItNames(t *testing.T) {
	h, facts, statuses := planHarness(t)
	architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
	statuses.givePlan(planDoc, 1, taskBlock("a", "implementer reviewer"),
		needing(taskBlock("b", "implementer reviewer"), "dispatch://LEGION-208/artifact/plan-md#b-a"))
	statuses.givePlan(planDoc, 2, taskBlock("a", "implementer reviewer"), needing(taskBlock("b", "implementer reviewer"), "x"))

	if taken := registerPlan(h, architect.grant(t), "LEGION-208", planDoc, 1); taken.Code != http.StatusOK {
		t.Fatalf("register version 1 = %d %s, want 200: b needs a, and b alone ends the plan", taken.Code, taken.Body)
	}
	assertPlanRefused(t, registerPlan(h, architect.grant(t), "LEGION-208", planDoc, 2),
		PlanReason{Reason: "the plan has 2 tasks that no task needs (a, b), and the work of each would merge by itself. This release of Legion does not carry it out yet."},
		PlanReason{Task: "b", Reason: "task b needs x, which names no task of this plan."})
	got := facts.recorded()
	if len(got) != 1 {
		t.Fatalf("facts = %#v, want the one fact of version 1", got)
	}
	given, ok := got[0].(intake.PlanGiven)
	if !ok || len(given.Plan.Tasks) != 2 || !slices.Equal(given.Plan.Tasks[1].Needs, []string{"a"}) {
		t.Fatalf("the fact of version 1 = %#v, want b needing [a], the id of the task its reference names", got[0])
	}
}

// register_plan reads each need against the issue it registers (plan.FromBlocks), so a need
// written as a reference into another issue, here d's of task a of the plan of LEGION-209, is no
// need of this plan's task a: it is refused with a reason of its own, naming the reference and that
// issue, and nothing is recorded. Since d needs nothing of this plan, a and d are two tasks that no
// task needs, the reason of this release beside it. The fault is the reference read as this plan's
// task a, under which the plan is taken; the control is plan.FromBlocks without the check of the
// issue, under which the route answers 200.
func TestRegisterPlanRefusesANeedIntoAnotherIssue(t *testing.T) {
	h, facts, statuses := planHarness(t)
	architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
	statuses.givePlan(planDoc, 1, taskBlock("a", "implementer reviewer"),
		needing(taskBlock("d", "implementer reviewer"), "dispatch://LEGION-209/artifact/plan-md#b-a"))

	assertPlanRefused(t, registerPlan(h, architect.grant(t), "LEGION-208", planDoc, 1),
		PlanReason{Reason: "the plan has 2 tasks that no task needs (a, d), and the work of each would merge by itself. This release of Legion does not carry it out yet."},
		PlanReason{Task: "d", Reason: "task d needs dispatch://LEGION-209/artifact/plan-md#b-a, which names another issue, LEGION-209: " +
			"a task can need only a task of this plan, and an issue waits for another issue by a blocked_by link between the two."})
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("a refused registration recorded %#v, want nothing", got)
	}
}

// register_plan of the plan of tasks a to e, each need written as Dispatch writes a reference to a
// block (c needs b, d needs a and c, e needs d), is taken: the workflow stores each need as the id
// of the task it names, so plan_tasks.needs of d reads {a,c}, and the answer names each task's
// needs by id, d's ["a","c"] and [] for a task that needs none. The steps of each task name one
// implementer and one reviewer by their keys, i and r, the same two agents in each task: this
// release runs one agent of each role for an issue, and an agent does the steps of several tasks.
//
// The fault it catches is a plan recorded with the words of its needs, which no reader of a task
// id reads (its control: the fact's plan read with each word kept whole, plan.FromBlocks without
// its resolution of needs, under which d's needs read the words).
func TestARegisteredPlanStoresItsNeedsAsTaskIds(t *testing.T) {
	h, _, statuses := planHarness(t)
	serveTheEngine(h, statuses)
	architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
	ref := func(id string) string { return "dispatch://LEGION-208/artifact/plan-md#b-" + id }
	const steps = "implementer:i reviewer:r"
	statuses.givePlan(planDoc, 1, taskBlock("a", steps), taskBlock("b", steps),
		needing(taskBlock("c", steps), ref("b")), needing(taskBlock("d", steps), ref("a")+" "+ref("c")),
		needing(taskBlock("e", steps), ref("d")))

	taken := registerPlan(h, architect.grant(t), "LEGION-208", planDoc, 1)
	task := func(id, needs string) string {
		return `{"id":"` + id + `","steps":["implementer:i","reviewer:r"],"needs":[` + needs + `]}`
	}
	want := `{"plan":{"version":1,"added":["a","b","c","d","e"],"tasks":[` + strings.Join([]string{task("a", ""), task("b", ""), task("c", `"b"`), task("d", `"a","c"`),
		task("e", `"d"`)}, ",") + `]}}`
	if taken.Code != http.StatusOK || strings.TrimSpace(taken.Body.String()) != want {
		t.Fatalf("register the plan of tasks a to e = %d %s, want 200 with %s", taken.Code, taken.Body, want)
	}
	var needs []string
	if err := h.store.Pool().QueryRow(context.Background(), `select needs from plan_tasks where issue = 'LEGION-208' and task = 'd'`).Scan(&needs); err != nil {
		t.Fatalf("read the needs of task d: %v", err)
	}
	if !slices.Equal(needs, []string{"a", "c"}) {
		t.Fatalf("plan_tasks.needs of d = %q, want {a,c}: each need stored as the id of the task it names", needs)
	}
}

// serveTheEngine serves h with the workflow's engine as its one handler, so each fact a route
// applies is the workflow's to judge and write, as the daemon's are, and statuses as its Dispatch.
// It answers the engine, for a case that starts a step as the daemon's own transitions do.
func serveTheEngine(h *harness, statuses *statusRecorder) *workflow.Engine {
	records := record.NewStore()
	engine := workflow.New(records, workflow.Config{Project: testProject, Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.handler = NewServer("127.0.0.1", 8437, Options{Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject,
		OperatorToken: testOperatorToken, Controller: h.store, Grants: credential.New(nil), Pool: h.store.Pool(), Record: records,
		Handlers: []intake.Handler{engine}, Dispatch: statuses, Deployment: plan.Deployment{Roles: plan.BuiltinRoles()}}).Handler
	return engine
}

// registeredAdded is the `added` of register_plan's answer to version of the document doc of
// issue, given with grant: the test fails unless the answer is 200 naming that version and a list.
func registeredAdded(t *testing.T, h *harness, grant, issue, doc string, version int) []string {
	t.Helper()
	taken := registerPlan(h, grant, issue, doc, version)
	var answer struct {
		Plan struct {
			Version int       `json:"version"`
			Added   *[]string `json:"added"`
		} `json:"plan"`
	}
	if taken.Code != http.StatusOK || json.Unmarshal(taken.Body.Bytes(), &answer) != nil || answer.Plan.Version != version {
		t.Fatalf("register version %d of %s = %d %s, want 200 with that version", version, issue, taken.Code, taken.Body)
	}
	if answer.Plan.Added == nil {
		t.Fatalf("register version %d of %s answers %s, want it to name `added`, a list and never null", version, issue, taken.Body)
	}
	return *answer.Plan.Added
}

// register_plan's answer names each task its version adds over the version in force, in the plan's
// order (`plan.added`): every task of the generation's first version, the one task a later version
// adds and no task beside it, and none, an empty list and never null, for a version that changes a
// task's steps and adds no task. The version in force, given again, is answered with what it first
// added. A version below it is refused, even when the store still holds that version.
//
// The faults it catches are an answer built from the given document, which names every task of each
// version (its control: the route answering every task of the plan given as added, under which
// version 2 answers a, b and c), and a version below the one in force answered with its old
// additions (its control: versionRefusal treating every stored version as the version in force).
func TestRegisterPlanNamesTheTasksItsVersionAdds(t *testing.T) {
	h, _, statuses := planHarness(t)
	serveTheEngine(h, statuses)
	architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
	ref := func(id string) string { return "dispatch://LEGION-208/artifact/plan-md#b-" + id }
	const steps = "implementer:i reviewer:r"
	statuses.givePlan(planDoc, 1, taskBlock("a", steps), needing(taskBlock("b", steps), ref("a")))
	statuses.givePlan(planDoc, 2, taskBlock("a", steps), needing(taskBlock("b", steps), ref("a")), needing(taskBlock("c", steps), ref("b")))
	statuses.givePlan(planDoc, 3, taskBlock("a", steps), needing(taskBlock("b", "implementer:i"), ref("a")), needing(taskBlock("c", steps), ref("b")))

	for _, tc := range []struct {
		version int
		want    []string
		why     string
	}{
		{1, []string{"a", "b"}, "the generation's first version adds every task"},
		{2, []string{"c"}, "version 2 adds c and keeps a and b as they were"},
		{3, []string{}, "version 3 changes the steps of b and adds no task"},
	} {
		if got := registeredAdded(t, h, architect.grant(t), "LEGION-208", planDoc, tc.version); !slices.Equal(got, tc.want) {
			t.Errorf("register version %d answers added %q, want %q: %s", tc.version, got, tc.want, tc.why)
		}
	}
	for _, version := range []int{2, 1} {
		refusal := intake.PlanVersionOld("LEGION-208", 3, version)
		refusedSaying(t, registerPlan(h, architect.grant(t), "LEGION-208", planDoc, version), refusal.Status, refusal.Code, refusal.Message)
	}
	if got := registeredAdded(t, h, architect.grant(t), "LEGION-208", planDoc, 3); !slices.Equal(got, []string{}) {
		t.Errorf("register version 3 given again answers added %q, want []: the version in force keeps its first answer", got)
	}
}

// firstStepOf starts the first step of key as the opening of its gate does (workflow's
// StartFirstPhase), under the fact lock, in a transaction of its own with the commit scope a
// fact's application gives the engine (Engine.OpenFact, Engine.SettleFact).
func firstStepOf(t *testing.T, h *harness, engine *workflow.Engine, key string) {
	t.Helper()
	if err := store.Transact(context.Background(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		if err := intake.SerializeFacts(ctx, tx); err != nil {
			return err
		}
		issue, err := record.NewStore().Issue(ctx, tx, key)
		if err != nil {
			return err
		}
		if issue == nil {
			return errors.New("the issue is not recorded")
		}
		ctx = engine.OpenFact(ctx)
		if err := engine.StartFirstPhase(ctx, tx, *issue, nil); err != nil {
			return err
		}
		return engine.SettleFact(ctx, tx)
	}); err != nil {
		t.Fatalf("start the first step of %s: %v", key, err)
	}
}

// planRows is what a registration can store for an issue at a generation: each version of its plan
// the store holds, `<version> <document> <giver>` in version order, each of its tasks in task id
// order, and the number of outbox rows of every issue.
type planRows struct {
	versions, tasks []string
	outbox          int
}

func planRowsOf(t *testing.T, h *harness, key string, generation uint64) planRows {
	t.Helper()
	ctx, pool := context.Background(), h.store.Pool()
	var rows planRows
	for _, read := range []struct {
		into  *[]string
		query string
	}{
		{&rows.versions, `select version::text || ' ' || artifact_id || ' ' || given_by from issue_plans where issue = $1 and generation = $2 order by version`},
		{&rows.tasks, `select task from plan_tasks where issue = $1 and generation = $2 order by task`},
	} {
		found, err := pool.Query(ctx, read.query, key, int64(generation))
		if err != nil {
			t.Fatalf("read the plan rows of %s: %v", key, err)
		}
		if *read.into, err = pgx.CollectRows(found, pgx.RowTo[string]); err != nil {
			t.Fatalf("read the plan rows of %s: %v", key, err)
		}
	}
	if err := pool.QueryRow(ctx, `select count(*) from outbox`).Scan(&rows.outbox); err != nil {
		t.Fatalf("count the outbox rows: %v", err)
	}
	return rows
}

// passedOverChild is the plan harness served by the engine, with LEGION-209 and LEGION-211
// members of the tree LEGION-208 at generation 1, for the case of a plan given for LEGION-209 by
// the architect of LEGION-211, a sub-architect, which the first step of LEGION-209 passes over:
// LEGION-211 stands in a tree of its own by then, so its architect owns LEGION-209 no more, and the
// step runs the default steps at generation 1, the generation that plan was given at. The plan's
// document, childDoc, holds at version 1 task a and task b, which needs a.
func passedOverChild(t *testing.T) (h *harness, root, sub liveClaim) {
	t.Helper()
	h, _, statuses := planHarness(t)
	engine := serveTheEngine(h, statuses)
	seedMember(t, h, "LEGION-209", "LEGION-208")
	seedMember(t, h, "LEGION-211", "LEGION-208")
	root = newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
	sub = newLiveClaimIn(t, h, "LEGION-208", "LEGION-211", claim.RoleArchitect)
	const steps = "implementer:i reviewer:r"
	statuses.givePlan(childDoc, 1, taskBlock("a", steps), needing(taskBlock("b", steps), "dispatch://LEGION-209/artifact/plan-md#b-a"))

	if got := registeredAdded(t, h, sub.grant(t), "LEGION-209", childDoc, 1); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("premise: the sub-architect's version 1 answers added %q, want [a b]", got)
	}
	seedIssueAt(t, h, "LEGION-211", phase.Admitted)
	firstStepOf(t, h, engine, "LEGION-209")
	if got := planRowsOf(t, h, "LEGION-209", 1); !slices.Equal(got.versions, []string{"0  "}) || !slices.Equal(got.tasks, []string{plan.DefaultTask}) {
		t.Fatalf("premise: after its first step LEGION-209 holds versions %q and tasks %q at generation 1, want the default steps alone: the sub-architect's plan passed over",
			got.versions, got.tasks)
	}
	return h, root, sub
}

// takenBy fails unless the plan in force of LEGION-209 at generation 1 is version 1 of childDoc,
// given by giver, with its tasks a and b, and the default steps it replaced gone.
func takenBy(t *testing.T, h *harness, giver liveClaim) {
	t.Helper()
	got := planRowsOf(t, h, "LEGION-209", 1)
	if want := []string{"0  ", "1 " + childDoc + " " + string(giver.token)}; !slices.Equal(got.versions, want) || !slices.Equal(got.tasks, []string{"a", "b"}) {
		t.Fatalf("LEGION-209 holds versions %q and tasks %q at generation 1, want versions %q and tasks [a b]: version 1 stored as %s gave it",
			got.versions, got.tasks, want, giver.token)
	}
}

// A give of a version of a plan that a first step passed over is no duplicate of the give that was
// passed over: the record no longer holds that version, so the give is judged and stored as any
// version is, though its id, which names the issue, the generation, the document and the version,
// is the id of the give passed over. Here the root's architect, the issue's owner once the
// sub-architect owns it no more, stops the default steps the first step ran and gives version 1
// of the same document at the same generation, and the answer names the tasks it added.
//
// The fault it catches is a give answered as a duplicate for its id alone, stored nothing (its
// control: intake.ApplyFact answering a plan's seen id as a duplicate where the record does not
// hold the version, under which the answer names no task and the default steps stay).
func TestAPlanVersionPassedOverIsTakenWhenANewOwnerGivesIt(t *testing.T) {
	h, root, _ := passedOverChild(t)
	if stopped := stopTask(h, root.grant(t), "LEGION-209", plan.DefaultTask); stopped.Code != http.StatusOK {
		t.Fatalf("stop the default steps = %d %s, want 200", stopped.Code, stopped.Body)
	}

	if got := registeredAdded(t, h, root.grant(t), "LEGION-209", childDoc, 1); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("the new owner's version 1 answers added %q, want [a b]: the version passed over adds both tasks over the default steps", got)
	}
	takenBy(t, h, root)
}

// The same owner's give of a version of a plan that a first step passed over is no duplicate
// either: the sub-architect's issue stands in the tree again, so the sub-architect owns the issue
// again, and its give of the version it gave before, at the same generation, is stored. An id
// that named the giver would still be the id of the give passed over here.
//
// The fault it catches is a give answered as a duplicate for an id that names the giver (its
// control: the route's id naming the giver's claim, with intake.ApplyFact answering a seen id as
// a duplicate, under which this give stores nothing, while the new owner's give of
// TestAPlanVersionPassedOverIsTakenWhenANewOwnerGivesIt is stored).
func TestAPlanVersionPassedOverIsTakenWhenItsOwnerGivesItAgain(t *testing.T) {
	h, _, sub := passedOverChild(t)
	seedMember(t, h, "LEGION-211", "LEGION-208")
	if stopped := stopTask(h, sub.grant(t), "LEGION-209", plan.DefaultTask); stopped.Code != http.StatusOK {
		t.Fatalf("stop the default steps = %d %s, want 200", stopped.Code, stopped.Body)
	}

	if got := registeredAdded(t, h, sub.grant(t), "LEGION-209", childDoc, 1); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("the owner's version 1 given again answers added %q, want [a b]: the version passed over adds both tasks over the default steps", got)
	}
	takenBy(t, h, sub)
}

// A give of the version in force is answered with the tasks that version first added. A giver the
// version already names changes no row, and another owner becomes its last giver; a version below
// the version in force is refused. A version given at generation 0 and carried to the issue's
// generation by its first step is in force there, so its giver receives its original additions
// again under the generation's new id.
//
// The faults it catches are a give of the version in force judged again as a later version (its
// control: versionRefusal taking no give as the version in force, under which each give again is
// refused 409 PLAN_VERSION_OLD), and an earlier version treated as taken after a later version
// reached the store.
func TestAGiveOfAVersionInForceReturnsItsOriginalAdditions(t *testing.T) {
	const steps = "implementer:i reviewer:r"
	ref := func(id string) string { return "dispatch://LEGION-209/artifact/plan-md#b-" + id }
	given := func(t *testing.T, h *harness, grant string, version int, want []string, why string) {
		t.Helper()
		before := planRowsOf(t, h, "LEGION-209", 1)
		if got := registeredAdded(t, h, grant, "LEGION-209", childDoc, version); !slices.Equal(got, want) {
			t.Errorf("version %d given again answers added %q, want %q: %s", version, got, want, why)
		}
		if after := planRowsOf(t, h, "LEGION-209", 1); !reflect.DeepEqual(after, before) {
			t.Errorf("version %d given again changed the store:\n before %+v\n after  %+v\nwant nothing stored: %s", version, before, after, why)
		}
	}

	t.Run("a version taken at the issue's generation, given again by its giver and by another", func(t *testing.T) {
		h, _, statuses := planHarness(t)
		serveTheEngine(h, statuses)
		seedMember(t, h, "LEGION-209", "LEGION-208")
		seedMember(t, h, "LEGION-211", "LEGION-208")
		root := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
		sub := newLiveClaimIn(t, h, "LEGION-208", "LEGION-211", claim.RoleArchitect)
		statuses.givePlan(childDoc, 1, taskBlock("a", steps), needing(taskBlock("b", steps), ref("a")))
		statuses.givePlan(childDoc, 2, taskBlock("a", steps), needing(taskBlock("b", "implementer:i"), ref("a")))
		if got := registeredAdded(t, h, sub.grant(t), "LEGION-209", childDoc, 1); !slices.Equal(got, []string{"a", "b"}) {
			t.Fatalf("premise: version 1 answers added %q, want [a b]", got)
		}
		given(t, h, sub.grant(t), 1, []string{"a", "b"}, "the sub-architect gives the version it gave")
		before := planRowsOf(t, h, "LEGION-209", 1)
		if got := registeredAdded(t, h, root.grant(t), "LEGION-209", childDoc, 1); !slices.Equal(got, []string{"a", "b"}) {
			t.Errorf("the root's architect's version 1 answers added %q, want [a b]", got)
		}
		if after := planRowsOf(t, h, "LEGION-209", 1); reflect.DeepEqual(after, before) {
			t.Errorf("the root's architect's give changed no row, want it added as a giver")
		}
		if got := registeredAdded(t, h, root.grant(t), "LEGION-209", childDoc, 2); !slices.Equal(got, []string{}) {
			t.Fatalf("premise: version 2 answers added %q, want []", got)
		}
		before = planRowsOf(t, h, "LEGION-209", 1)
		if got := registeredAdded(t, h, sub.grant(t), "LEGION-209", childDoc, 2); !slices.Equal(got, []string{}) {
			t.Errorf("the sub-architect's version 2 answers added %q, want []", got)
		}
		if after := planRowsOf(t, h, "LEGION-209", 1); reflect.DeepEqual(after, before) {
			t.Errorf("the sub-architect's give changed no row, want it added as a giver")
		}
		refusal := intake.PlanVersionOld("LEGION-209", 2, 1)
		refusedSaying(t, registerPlan(h, root.grant(t), "LEGION-209", childDoc, 1), refusal.Status, refusal.Code, refusal.Message)
	})

	t.Run("a version given at generation 0 and carried to the issue's generation by its first step", func(t *testing.T) {
		h, _, statuses := planHarness(t)
		engine := serveTheEngine(h, statuses)
		root := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
		parent := "LEGION-208"
		statuses.issues = map[string]dispatch.Issue{
			"LEGION-208": {Key: "LEGION-208", Status: "todo", LastSeq: 7},
			"LEGION-209": {Key: "LEGION-209", Status: "backlog", Parent: &parent},
		}
		statuses.givePlan(childDoc, 1, taskBlock("a", steps), needing(taskBlock("b", steps), ref("a")))
		if got := registeredAdded(t, h, root.grant(t), "LEGION-209", childDoc, 1); !slices.Equal(got, []string{"a", "b"}) {
			t.Fatalf("premise: version 1 given before the daemon recorded LEGION-209 answers added %q, want [a b]", got)
		}
		seedMember(t, h, "LEGION-209", "LEGION-208")
		firstStepOf(t, h, engine, "LEGION-209")
		if got := planRowsOf(t, h, "LEGION-209", 1); !slices.Equal(got.versions, []string{"1 " + childDoc + " " + string(root.token)}) {
			t.Fatalf("premise: after its first step LEGION-209 holds versions %q at generation 1, want version 1 carried from generation 0", got.versions)
		}
		given(t, h, root.grant(t), 1, []string{"a", "b"}, "the first step carried version 1 to generation 1, where the give names it")
	})
}

// A plan whose tasks need each other in a cycle is refused, and among its reasons is the cycle's,
// which names the chain, whatever reasons of this release's come before it. The fault is a cycle
// answered only as needs this release does not carry out yet; the control is Validate without its
// check of cycles, whose answer holds no reason naming the chain.
func TestRegisterPlanRefusesACycleOfNeedsNamingIt(t *testing.T) {
	h, facts, statuses := planHarness(t)
	architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
	statuses.givePlan(planDoc, 1, needing(taskBlock("a", "implementer reviewer"), "dispatch://LEGION-208/artifact/plan-md#b-b"),
		needing(taskBlock("b", "implementer reviewer"), "dispatch://LEGION-208/artifact/plan-md#b-a"))

	answer := registerPlan(h, architect.grant(t), "LEGION-208", planDoc, 1)
	if answer.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", answer.Code, answer.Body)
	}
	var refusal PlanRefusal
	decodeInto(t, answer, &refusal)
	cycle := PlanReason{Task: "a", Reason: "task a needs itself: a needs b, b needs a."}
	if refusal.Code != "PLAN_REFUSED" || !slices.Contains(refusal.Reasons, cycle) || !strings.Contains(refusal.Error, cycle.Reason) {
		t.Fatalf("refusal = %+v, want PLAN_REFUSED holding the reason %+v", refusal, cycle)
	}
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("a refused registration recorded %#v, want nothing", got)
	}
}

// A refused plan's answer has a bound in bytes, whatever the plan holds. It names the first
// plan.MostReasons reasons of each kind of cause and counts the rest of the kind in one more; a
// task id is at most steprecord.TaskIDMax, since a longer one is refused by one reason of its own;
// each other word of the plan a reason quotes is cut to plan.MostQuotedBytes and marked; and a
// reason lists at most plan.MostReasons task ids. Three versions below are one `plan_task` block
// whose one attribute holds 1 MiB of words, the most a Dispatch document holds as its server
// writes it, each word a cause of its own: 262,144 `needs` words that name no task, 209,712 roles
// nobody defines, and 80,659 steps `acceptance:k`, each with a key but owner on acceptance and
// with no implementer before it. Each answer is within perKind bytes for each kind it holds, and
// counts the rest of each kind past the bound, in its sentence too. Three more quote or list long
// values, each within perLongKind for each kind, a bound computed from those constants: a ring of
// 40,000 tasks, each needing a word that names no task too, whose reasons are those words and the
// cycle through the tasks, its chain cut; a task whose id is 900 KB; and a task whose one need is
// a word of 1 MiB, each of the last two quoted with its mark. The faults are an answer with a
// reason for each cause and one that quotes or lists a value whole; the controls take out plan's
// bound of the reasons, under which the first two versions are answered in 34 and 44 MB, and
// quoted's cut with chainOf's, under which the long id and the long need are answered whole. It
// holds the counts and the size, and no sentence.
func TestRegisterPlanAnswersAPlanOfManyCausesWithinABound(t *testing.T) {
	// perKind is plan.MostReasons reasons of one kind and its count, each written twice (in
	// `error` and in `reasons`) at under 200 bytes with its JSON.
	const perKind = (plan.MostReasons + 1) * 2 * 200
	// longReason is the longest reason: its sentence, under 300 bytes, and a list of
	// plan.MostReasons pairs of task ids, `<id> needs <id>, ` (a cycle's chain), which is longer
	// than a word cut to plan.MostQuotedBytes with its mark. perLongKind is plan.MostReasons such
	// reasons and a count, each written twice with its task id and its JSON.
	const longReason = 300 + plan.MostReasons*(2*steprecord.TaskIDMax+len(" needs , "))
	const perLongKind = (plan.MostReasons + 1) * (2*longReason + steprecord.TaskIDMax + 50)
	const mib = 1 << 20
	// digits is i written in n digits of the base len(alphabet), the first digit the largest.
	digits := func(i int, alphabet string, n int) string {
		word := make([]byte, n)
		for at := n - 1; at >= 0; at-- {
			word[at] = alphabet[i%len(alphabet)]
			i /= len(alphabet)
		}
		return string(word)
	}
	// Each three-character word over 64 characters, each a need that names no task.
	words := make([]string, 64*64*64)
	for i := range words {
		words[i] = digits(i, "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ-_", 3)
	}
	// The implementer, then roles of four characters, a letter and three of [a-z0-9-], each once.
	const letters, rest = "abcdefghijklmnopqrstuvwxyz", "abcdefghijklmnopqrstuvwxyz0123456789-"
	roles := []string{"implementer"}
	for i := range (mib - len("implementer ")) / len("abcd ") {
		roles = append(roles, string(letters[i/(37*37*37)])+digits(i, rest, 3))
	}
	steps := mib / len("acceptance:k ")
	keyed := strings.TrimSuffix(strings.Repeat("acceptance:k ", steps), " ")
	// The ring: t00000 to t39999, each needing the next and the last the first, and each needing
	// a word that names no task, x00000 to x39999.
	const ringSize = 40_000
	ring := make([]dispatch.DocumentBlock, ringSize)
	for i := range ring {
		ring[i] = needing(taskBlock(fmt.Sprintf("t%05d", i), "implementer reviewer"), fmt.Sprintf("t%05d x%05d", (i+1)%ringSize, i))
	}
	longID := strings.Repeat("a", 900<<10)
	longNeed := strings.Repeat("x", mib)
	for _, tc := range []struct {
		name    string
		blocks  []dispatch.DocumentBlock
		bound   int
		reasons int    // the reasons the answer holds
		counts  []int  // what the reasons that count the rest of a kind count
		cut     string // the mark of a value quoted in part, which the sentence holds
	}{
		{"262,144 needs words that name no task", []dispatch.DocumentBlock{needing(taskBlock("a", "implementer reviewer"), strings.Join(words, " "))},
			perKind, plan.MostReasons + 1, []int{len(words) - plan.MostReasons}, ""},
		{"209,712 roles nobody defines", []dispatch.DocumentBlock{taskBlock("a", strings.Join(roles, " "))},
			perKind, plan.MostReasons + 1, []int{len(roles) - 1 - plan.MostReasons}, ""},
		// Four kinds: a key at each step, the role acceptance at two steps, no implementer, and
		// no implementer before each step.
		{"80,659 steps acceptance:k", []dispatch.DocumentBlock{taskBlock("a", keyed)},
			4 * perKind, 2*(plan.MostReasons+1) + 2, []int{steps - plan.MostReasons, steps - plan.MostReasons}, ""},
		// Two kinds: the word of each task that names no task, and the one cycle, its chain cut.
		{"a ring of 40,000 tasks", ring, 2 * perLongKind, plan.MostReasons + 1 + 1, []int{ringSize - plan.MostReasons}, ""},
		// The id alone is refused; with steps that each name it, a whole id gave tens of MB.
		{"a task whose id is 900 KB", []dispatch.DocumentBlock{taskBlock(longID, "tester:k tester:k")},
			perLongKind, 1, nil, fmt.Sprintf("… (cut from %d bytes)", len(longID))},
		{"a task whose one need is a word of 1 MiB", []dispatch.DocumentBlock{needing(taskBlock("a", "implementer reviewer"), longNeed)},
			perLongKind, 1, nil, fmt.Sprintf("… (cut from %d bytes)", len(longNeed))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, facts, statuses := planHarness(t)
			architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
			statuses.givePlan(planDoc, 1, tc.blocks...)

			answer := registerPlan(h, architect.grant(t), "LEGION-208", planDoc, 1)
			size := answer.Body.Len()
			t.Logf("%s: answered %d in %d bytes, the bound %d", tc.name, answer.Code, size, tc.bound)
			if answer.Code != http.StatusConflict || size > tc.bound {
				t.Fatalf("%s is answered %d in %d bytes, want 409 in at most %d", tc.name, answer.Code, size, tc.bound)
			}
			var refusal PlanRefusal
			decodeInto(t, answer, &refusal)
			if refusal.Code != "PLAN_REFUSED" || len(refusal.Reasons) != tc.reasons {
				t.Fatalf("refusal %s of %d reasons, want PLAN_REFUSED of %d", refusal.Code, len(refusal.Reasons), tc.reasons)
			}
			if !strings.Contains(refusal.Error, tc.cut) {
				t.Errorf("the sentence holds no mark %q of a value quoted in part: %.3000s", tc.cut, refusal.Error)
			}
			for _, left := range tc.counts {
				count := fmt.Sprintf(" %d ", left)
				if !strings.Contains(refusal.Error, count) || !slices.ContainsFunc(refusal.Reasons, func(reason PlanReason) bool {
					return reason.Task == "" && strings.Contains(reason.Reason, count)
				}) {
					t.Errorf("no reason of the whole plan counts %d more, in `reasons` and in the sentence: %.3000s", left, refusal.Error)
				}
			}
			if got := facts.recorded(); len(got) != 0 {
				t.Fatalf("a refused registration recorded %#v, want nothing", got)
			}
		})
	}
}

// plantedUser is the user name a planted URL carries as its userinfo, 300 bytes, written as the
// redact package's tests write theirs: a user name with no password, which a secret scanner takes
// for no credential, and which redact.URLCredentials removes as it removes a password or a token.
// plantedURL carries it, and redactedURL is what the URL rule (redact.URLCredentials) leaves of
// plantedURL, read from the rule and never written out here, so that a rule that removes the
// userinfo and one that writes a mark in place of the whole authority both leave the test true;
// it is still past plan.MostQuotedBytes, so a reason quotes it cut.
var (
	plantedUser = strings.Repeat("planted-reader-", 20)
	plantedURL  = "https://" + plantedUser + "@h.example/" + strings.Repeat("p", 200)
	redactedURL = redact.URLCredentials(plantedURL)
)

// credentialIn is the first run of six bytes of plantedUser that text holds, and "" where it holds
// none.
func credentialIn(text string) string {
	for at := 0; at+6 <= len(plantedUser); at++ {
		if part := plantedUser[at : at+6]; strings.Contains(text, part) {
			return part
		}
	}
	return ""
}

// register_plan's answer to a plan whose words carry a URL with a credential holds no byte of the
// credential, in its `error` and in each of its `reasons`, however long the word: the plan
// package reads each word it quotes whole through the URL rule before it cuts it, so the route's
// own redaction of `error` has nothing left to find, and `reasons`, which the route writes as they
// came, holds what the rule left. A need, a step whose role is the URL, a step whose key is the
// URL, a task block's id, a role block's id and one id on two blocks each carry plantedURL. Each
// answer quotes redactedURL cut and marked, in a reason of the task named and in the sentence, and
// holds no run of six bytes of plantedUser. The fault is a cut made before the rule, which ends
// inside the userinfo, before its "@", so the route's redaction of `error` finds no userinfo and
// `reasons` are never redacted; the control is the plan package's quoted cutting first, under
// which each answer keeps 120 bytes of plantedUser in both.
func TestRegisterPlanQuotesNoByteOfAURLsCredential(t *testing.T) {
	cut := func(word string) string {
		return fmt.Sprintf("%s… (cut from %d bytes)", word[:plan.MostQuotedBytes], len(word))
	}
	quote := cut(redactedURL)
	roleBlock := dispatch.DocumentBlock{ID: plantedURL, Type: plan.RoleBlockType, Attributes: map[string]string{}}
	for _, tc := range []struct {
		name   string
		blocks []dispatch.DocumentBlock
		task   string // the task of the reason that quotes the word
		quote  string
	}{
		{"a need", []dispatch.DocumentBlock{needing(taskBlock("a", "implementer reviewer"), plantedURL)}, "a", quote},
		{"a step whose role is the URL", []dispatch.DocumentBlock{taskBlock("a", plantedURL)}, "", quote},
		{"a step whose key is the URL", []dispatch.DocumentBlock{taskBlock("a", "implementer reviewer:"+plantedURL)}, "", cut("reviewer:" + redactedURL)},
		{"a task block's id", []dispatch.DocumentBlock{taskBlock(plantedURL, "implementer reviewer")}, "", quote},
		{"a role block's id", []dispatch.DocumentBlock{roleBlock, taskBlock("a", "implementer reviewer")}, "", quote},
		{"one id on two blocks", []dispatch.DocumentBlock{roleBlock, taskBlock(plantedURL, "implementer reviewer")}, "", quote},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, facts, statuses := planHarness(t)
			architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
			statuses.givePlan(planDoc, 1, tc.blocks...)

			answer := registerPlan(h, architect.grant(t), "LEGION-208", planDoc, 1)
			if answer.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409; body %.2000s", answer.Code, answer.Body)
			}
			var refusal PlanRefusal
			decodeInto(t, answer, &refusal)
			if refusal.Code != "PLAN_REFUSED" || !strings.Contains(refusal.Error, tc.quote) || !slices.ContainsFunc(refusal.Reasons, func(reason PlanReason) bool {
				return reason.Task == tc.task && strings.Contains(reason.Reason, tc.quote)
			}) {
				t.Fatalf("refusal %.3000v, want PLAN_REFUSED quoting %.300q in its sentence and in a reason of task %q", refusal, tc.quote, tc.task)
			}
			if part := credentialIn(refusal.Error); part != "" {
				t.Errorf("`error` %.2000q holds %q of the URL's credential", refusal.Error, part)
			}
			for _, reason := range refusal.Reasons {
				if part := credentialIn(reason.Reason); part != "" {
					t.Errorf("the reason %.2000q holds %q of the URL's credential", reason.Reason, part)
				}
			}
			if got := facts.recorded(); len(got) != 0 {
				t.Fatalf("a refused registration recorded %#v, want nothing", got)
			}
		})
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
		{"a version that changes the running task", intake.PlanRefused([]plan.Refusal{intake.RunningTaskChanged("a")})},
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
				assertPlanRefused(t, recorder, PlanReason{Task: "a", Reason: "task a is running: stop it with stop_task before you change it."})
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
