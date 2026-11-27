package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/steprecord"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/redact"
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
// its tasks. No id decides whether a registration given again is the one taken (intake.ApplyFact
// applies every PlanGiven), so each registration reaches this handler, which stores nothing, under
// the one id the route names it by, and is answered the same.
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
	twoSinks := PlanReason{Reason: "the plan has 2 tasks that no task needs (a, b), and the work of each would merge by itself. This release of Legion does not carry it out yet."}
	assertPlanRefused(t, registerPlan(h, architect.grant(t), "LEGION-208", planDoc, 1), twoTasks, twoSinks)
	assertPlanRefused(t, registerPlan(h, architect.grant(t), "LEGION-208", planDoc, 2),
		PlanReason{Task: "a", Reason: "step 2 of task a names the role prober, which neither the deployment nor the plan defines."})
	assertPlanRefused(t, registerPlan(h, architect.grant(t), "LEGION-208", planDoc, 3), twoTasks, twoSinks,
		PlanReason{Task: "b", Reason: "step 2 of task b names the role prober, which neither the deployment nor the plan defines."})
	// A version that keeps the task of the default steps and adds a second is a plan of two tasks
	// like any other: the route refuses it, so the workflow never takes it.
	assertPlanRefused(t, registerPlan(h, architect.grant(t), "LEGION-208", planDoc, 6),
		PlanReason{Reason: "the plan has 2 tasks (default, a), and this release runs one task for each issue. This release of Legion does not carry it out yet."},
		PlanReason{Reason: "the plan has 2 tasks that no task needs (a, default), and the work of each would merge by itself. This release of Legion does not carry it out yet."})
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
		givenPlan("LEGION-208", 1, architect.token, planDoc, 4, implementerReviewer...),
		givenPlan("LEGION-208", 1, architect.token, planDoc, 5),
	}
	if got := facts.recorded(); !reflect.DeepEqual(got, want) {
		t.Fatalf("facts = %#v, want %#v: one for each registration, version 4's second among them", got, want)
	}
	assertProcessedEvent(t, h, "plan:LEGION-208:1:"+planDoc+":4")
}

// The reader of 219 takes every field of register_plan's answer as optional, since no plugin code
// reads one (legion-api.ts's LEGION_ANSWER_READS: a field that set does not name is optional). So
// the reader takes a task written without `steps`, and the daemon holds what it does not check:
// every task of the answer, as shapeAnswer writes it to a caller at OwnerPlanContract, carries
// `steps` as a list, never null and never absent, a task whose block names none included.
// TestRegisterPlanTakesAPlanAndRefusesWithReasons holds the same for one task through the route.
func TestRegisterPlanWritesEveryTasksStepsAsAList(t *testing.T) {
	answer := PlanRegisterResponse{Plan: registeredPlan(plan.Plan{ArtifactID: planDoc, Version: 7,
		Tasks: []plan.Task{{ID: "a"}, {ID: "b", Steps: []plan.Step{{Role: "implementer"}}}}})}
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

// register_plan reads each need as the task it names (plan.FromBlocks) before either check, so
// the reasons name the task a reference names, a task a reference names is needed (b alone ends
// the first plan), and a word that names no task gets the reason no release lifts rather than the reason of a
// need this release does not carry out yet; the plan recorded is the one both checks read. The
// fault is a route that checks, and records, the words as the blocks write them; the control is
// a FromBlocks that keeps each word whole, which answers `task b needs dispatch://LEGION-208/artifact/plan-md#b-a.`
// and `task b needs x.`, each as not carried out yet, and a and b as two tasks that no task needs.
func TestRegisterPlanReadsEachNeedAsTheTaskItNames(t *testing.T) {
	h, facts, statuses := planHarness(t)
	architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
	statuses.givePlan(planDoc, 1, taskBlock("a", "implementer reviewer"),
		needing(taskBlock("b", "implementer reviewer"), "dispatch://LEGION-208/artifact/plan-md#b-a"))
	statuses.givePlan(planDoc, 2, taskBlock("a", "implementer reviewer"), needing(taskBlock("b", "implementer reviewer"), "x"))

	twoTasks := PlanReason{Reason: "the plan has 2 tasks (a, b), and this release runs one task for each issue. This release of Legion does not carry it out yet."}
	assertPlanRefused(t, registerPlan(h, architect.grant(t), "LEGION-208", planDoc, 1), twoTasks,
		PlanReason{Task: "b", Reason: "task b needs a. This release of Legion does not carry it out yet."})
	assertPlanRefused(t, registerPlan(h, architect.grant(t), "LEGION-208", planDoc, 2), twoTasks,
		PlanReason{Reason: "the plan has 2 tasks that no task needs (a, b), and the work of each would merge by itself. This release of Legion does not carry it out yet."},
		PlanReason{Task: "b", Reason: "task b needs x, which names no task of this plan."})
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("refused registrations recorded %#v, want nothing", got)
	}
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
// nobody defines, and 116,508 steps `tester:k`, each with a key and with no implementer before
// it. Each answer is within perKind bytes for each kind it holds, and counts the rest of each kind
// past the bound, in its sentence too. Three more quote or list long values, each within
// perLongKind for each kind, a bound computed from those constants: a ring of 40,000 tasks, whose
// reasons list the plan's tasks and the cycle through them; a task whose id is 900 KB; and a task
// whose one need is a word of 1 MiB, each of the last two quoted with its mark. The faults are an
// answer with a reason for each cause and one that quotes or lists a value whole; the controls
// take out plan's bound of the reasons, under which the three versions are answered in 34, 44 and
// 66 MB and the ring in 9.6 MB, and quoted's cut with listed's and chainOf's, under which the
// ring, the long id and the long need are answered whole. It holds the counts and the size, and no
// sentence.
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
	steps := mib / len("tester:k ")
	keyed := strings.TrimSuffix(strings.Repeat("tester:k ", steps), " ")
	// The ring: t00000 to t39999, each needing the next and the last the first.
	const ringSize = 40_000
	ring := make([]dispatch.DocumentBlock, ringSize)
	for i := range ring {
		ring[i] = needing(taskBlock(fmt.Sprintf("t%05d", i), "implementer reviewer"), fmt.Sprintf("t%05d", (i+1)%ringSize))
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
		// Four kinds: a key at each step, the role tester at two steps, no implementer, and no
		// implementer before each step.
		{"116,508 steps tester:k", []dispatch.DocumentBlock{taskBlock("a", keyed)},
			4 * perKind, 2*(plan.MostReasons+1) + 2, []int{steps - plan.MostReasons, steps - plan.MostReasons}, ""},
		// Three kinds: the plan's tasks, the need of each task, and the one cycle.
		{"a ring of 40,000 tasks", ring, 3 * perLongKind, 1 + plan.MostReasons + 1 + 1, []int{ringSize - plan.MostReasons}, ""},
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
