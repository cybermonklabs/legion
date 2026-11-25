// One plan of an issue, through the whole daemon over a real Dispatch of this tree
// (ownerStepsRig, owner_steps_rig_test.go). The issue's plan.md holds the owner's task block and
// the planner's task blocks under it, by their parent. Each planner's block is an element of the
// owner's task, and Legion carries out the owner's task alone. An owner's version that changes
// only the text of its block leaves each planner's block as it was. The scratch Dispatch creates
// the elements its manifest stages for the next version (createStaged), so the test can write
// parent.
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	planwords "github.com/sjawhar/legion/daemon/internal/plan"
)

// defaultText is Legion's text of the task default, where its owner gave none.
const defaultText = "The project's default steps."

// elementTexts are the texts of the planner's task blocks t1, t2 and t3 (plannerTasks). No start
// task of the task they stand under holds any of them.
var elementTexts = []string{"Build the reader.", "Build the writer.", "Write each record once", "Join the two."}

// stored is a document version Dispatch stored: the document's id and the version's number.
type stored struct {
	id      string
	version int
}

// upload is a write of markdown as the document name on the walk's issue, as `dispatch artifact`
// writes it. A first upload of a name stores version 1. A later one replaces the document and
// names token, the token of the text it replaces (none for a first upload).
func (r *ownerStepsRig) upload(name, markdown, token string) stored {
	r.t.Helper()
	body := map[string]any{"name": name, "content": markdown}
	if token != "" {
		body["precondition"] = map[string]string{"document": token}
	}
	var answer struct {
		Artifact struct {
			ID string `json:"id"`
		} `json:"artifact"`
		Version struct {
			Number int `json:"number"`
		} `json:"version"`
	}
	r.person(http.MethodPost, "/api/v1/issues/"+r.key+"/artifacts", body, http.StatusCreated, &answer)
	if answer.Artifact.ID == "" || answer.Version.Number == 0 {
		r.t.Fatalf("Dispatch answered the upload of %s on %s with no document or no version: %+v", name, r.key, answer)
	}
	return stored{id: answer.Artifact.ID, version: answer.Version.Number}
}

// liveBlock is one block of a live document as Dispatch lists it: its id, its type, the byte
// range of its rendering in the markdown, its token and the comments anchored in it.
type liveBlock struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	From       int    `json:"from"`
	To         int    `json:"to"`
	Token      string `json:"token"`
	References struct {
		Comments int `json:"comments"`
	} `json:"references"`
}

// liveDocument is a document as it stands now: its markdown, its token, its version and its
// blocks, from one read of Dispatch's text route.
type liveDocument struct {
	Markdown string      `json:"markdown"`
	Token    string      `json:"token"`
	Version  *int        `json:"version"`
	Blocks   []liveBlock `json:"blocks"`
}

// live reads the document artifact as it stands now, with its blocks.
func (r *ownerStepsRig) live(artifact string) liveDocument {
	r.t.Helper()
	var read liveDocument
	r.person(http.MethodGet, "/api/v1/artifacts/"+artifact+"/text?blocks=true", nil, http.StatusOK, &read)
	if read.Version == nil || read.Token == "" {
		r.t.Fatalf("the read of %s names no version or no token: %+v", artifact, read)
	}
	return read
}

// taskRenderings is each plan_task block of document by id, in the document's order, and the
// markdown of each, its opening line through its closing fence.
func taskRenderings(t *testing.T, document liveDocument) ([]string, map[string]string) {
	t.Helper()
	var ids []string
	renderings := map[string]string{}
	for _, block := range document.Blocks {
		if block.Type != planwords.TaskBlockType {
			continue
		}
		if block.From < 0 || block.From > block.To || block.To > len(document.Markdown) {
			t.Fatalf("Dispatch places task block %s at %d to %d of a markdown of %d bytes", block.ID, block.From, block.To, len(document.Markdown))
		}
		rendering := document.Markdown[block.From:block.To]
		if !strings.HasPrefix(rendering, ":::plan_task{#"+block.ID) {
			t.Fatalf("the range Dispatch gives task block %s holds %q, not the block", block.ID, rendering)
		}
		ids = append(ids, block.ID)
		renderings[block.ID] = rendering
	}
	return ids, renderings
}

// review is the review state of a document as `dispatch read` prints it on its Approval line:
// the state, the version the review approved, and the document's newest version.
type review struct {
	state           string
	version, latest int
}

// approval is the review state of the document artifact, the zero review where it has none.
func (r *ownerStepsRig) approval(artifact string) review {
	r.t.Helper()
	var read struct {
		Approval *struct {
			State         string `json:"state"`
			Version       *int   `json:"version"`
			LatestVersion int    `json:"latest_version"`
		} `json:"approval"`
	}
	r.person(http.MethodGet, "/api/v1/artifacts/"+artifact, nil, http.StatusOK, &read)
	if read.Approval == nil || read.Approval.Version == nil {
		return review{}
	}
	return review{state: read.Approval.State, version: *read.Approval.Version, latest: read.Approval.LatestVersion}
}

// planReview is the plan review approving version of the plan artifact, as `dispatch
// plan-approve` records it.
func (r *ownerStepsRig) planReview(artifact string, version int) {
	r.t.Helper()
	r.person(http.MethodPost, "/api/v1/artifacts/"+artifact+"/plan-reviews", map[string]any{"version": version}, http.StatusCreated, nil)
}

// plannersPlan is what the later steps read of the planner's part of a plan.md. It holds the
// planner's task blocks by id, in the document's order. It holds each block of the planner's with
// its type, its token, its count of anchored comments and, for a task block, its markdown. It
// holds each comment on the document with whether its anchor still holds, the version the plan
// review approved, and how many plan tasks of the issue serve each requirement of its spec.
type plannersPlan struct {
	tasks    []string
	blocks   map[string]string
	comments []string
	approved int
	served   map[string]int
}

// readPlan is the walk's issue's plan.md, the document artifact, as the later steps read the
// planner's part of it: every block but the owner's task block owner and the blocks inside it.
func (r *ownerStepsRig) readPlan(artifact, owner string) plannersPlan {
	r.t.Helper()
	read := plannersPlan{blocks: map[string]string{}, approved: r.approval(artifact).version, served: map[string]int{}}
	document := r.live(artifact)
	_, renderings := taskRenderings(r.t, document)
	at := slices.IndexFunc(document.Blocks, func(block liveBlock) bool { return block.Type == planwords.TaskBlockType && block.ID == owner })
	if at < 0 {
		r.t.Fatalf("plan.md of %s holds no task block %s, the owner's: %+v", r.key, owner, document.Blocks)
	}
	owners := document.Blocks[at]
	for _, block := range document.Blocks {
		if owners.From <= block.From && block.To <= owners.To {
			continue
		}
		entry := block.Type + " " + block.Token + " comments " + strconv.Itoa(block.References.Comments)
		if block.Type == planwords.TaskBlockType {
			read.tasks = append(read.tasks, block.ID)
			entry += " " + strconv.Quote(renderings[block.ID])
		}
		read.blocks[block.ID] = entry
	}
	var comments []struct {
		Body   string `json:"body"`
		Anchor *struct {
			ArtifactID string `json:"artifact_id"`
			Quote      string `json:"quote"`
			Orphaned   bool   `json:"orphaned"`
		} `json:"anchor"`
	}
	r.person(http.MethodGet, "/api/v1/issues/"+r.key+"/comments?artifact="+artifact, nil, http.StatusOK, &comments)
	for _, comment := range comments {
		if comment.Anchor != nil && comment.Anchor.ArtifactID == artifact {
			read.comments = append(read.comments, comment.Body+" on "+strconv.Quote(comment.Anchor.Quote)+" orphaned "+strconv.FormatBool(comment.Anchor.Orphaned))
		}
	}
	var coverage struct {
		Requirements []struct {
			BlockID   string `json:"block_id"`
			PlanTasks int    `json:"plan_tasks"`
		} `json:"requirements"`
	}
	r.person(http.MethodGet, "/api/v1/issues/"+r.key+"/coverage", nil, http.StatusOK, &coverage)
	for _, requirement := range coverage.Requirements {
		read.served[requirement.BlockID] = requirement.PlanTasks
	}
	return read
}

// holdsPlan fails the test unless the planner's part of plan.md, the document artifact, reads as
// want at the moment after names. owner is the owner's task block.
func (r *ownerStepsRig) holdsPlan(after, artifact, owner string, want plannersPlan) {
	r.t.Helper()
	if len(want.tasks) == 0 {
		r.t.Fatalf("the plan.md to compare with holds no planner's task block, so the comparison after %s sees nothing", after)
	}
	if got := r.readPlan(artifact, owner); !reflect.DeepEqual(got, want) {
		r.t.Fatalf("after %s the planner's part of plan.md reads\n%+v\nwant it as the planner left it\n%+v", after, got, want)
	}
}

// openGateAt is a person approving the walk's issue's spec at version and its architect
// registering that version (register_gate), which opens the issue's gate. The issue's first step
// must then be first.
func (r *ownerStepsRig) openGateAt(version int, first phase.Phase) {
	r.t.Helper()
	spec := r.specs[r.key]
	r.person(http.MethodPost, "/api/v1/artifacts/"+spec+"/reviews", map[string]any{"state": "approved"}, http.StatusCreated, nil)
	status, body := r.d.request(http.MethodPost, "/legion/v1/gates/register", map[string]any{
		"grantId": r.worker(claim.RoleArchitect).grant(), "issue": r.key, "artifactId": spec, "version": version}, false)
	if status != http.StatusOK {
		r.t.Fatalf("register_gate of %s's spec version %d = %d %s, want 200", r.key, version, status, body)
	}
	r.waitPhase(first)
}

// threeRequirements is a spec part marking three requirements, r1, r2 and r3.
const threeRequirements = "\n:::requirement{#r1}\nThe reader reads the input.\n:::\n\n" +
	":::requirement{#r2}\nThe writer writes the output.\n:::\n\n" +
	":::requirement{#r3}\nThe reader and the writer run as one.\n:::\n"

// plannerTasks is a planner's plan.md on issue key of the default steps: a heading and three task
// blocks t1, t2 and t3. Each serves one requirement of the spec (threeRequirements), and the
// second needs the first and the third the second. Where text is not empty, the plan opens with
// the block default, whose text is text, and each block that under names stands under default by
// parent. Where text is empty, the plan holds no default and each block stands at the top.
func plannerTasks(key, text string, under ...string) string {
	spec := "dispatch://" + key + "/spec#b-"
	document := "dispatch://" + key + "/artifact/plan-md#b-"
	markdown := "# Plan\n\n"
	if text != "" {
		markdown += textPlan("default", "", text) + "\n"
	}
	block := func(id, needs, serves, body string) string {
		attributes := "#" + id
		if text != "" && slices.Contains(under, id) {
			attributes += ` parent="` + document + `default"`
		}
		if needs != "" {
			attributes += ` needs="` + document + needs + `"`
		}
		return ":::plan_task{" + attributes + ` serves="` + spec + serves + "\"}\n" + body + "\n:::\n"
	}
	return markdown + block("t1", "", "r1", "Build the reader.") + "\n" +
		block("t2", "t1", "r2", "Build the writer. Write each record once.") + "\n" +
		block("t3", "t2", "r3", "Join the two.")
}

// plannerUnder is a planner's two task blocks of issue key's plan.md, p1 and p2, each under the
// task block task by parent. p2 needs p1.
func plannerUnder(key, task string) string {
	document := "dispatch://" + key + "/artifact/plan-md#b-"
	return ":::plan_task{#p1 parent=\"" + document + task + "\"}\nRead the input first.\n:::\n\n" +
		":::plan_task{#p2 parent=\"" + document + task + "\" needs=\"" + document + "p1\"}\nWrite the output after.\n:::\n"
}

// holdsOwnersText fails the test unless task, the start task of who, holds want on a line of its
// own and none of not.
func holdsOwnersText(t *testing.T, who, task, want string, not ...string) {
	t.Helper()
	if !strings.Contains(task, "\n"+want+"\n") {
		t.Fatalf("the %s's start task = %q,\nwant it to hold %q", who, task, want)
	}
	for _, text := range not {
		if strings.Contains(task, text) {
			t.Fatalf("the %s's start task = %q,\nwant it to hold %q and not %q", who, task, want, text)
		}
	}
}

// stepAt is the act of the step at place of a task, which role's agent works.
func stepAt(place int, role claim.Role) runningAct {
	return runningAct{Kind: "step", Place: place, Role: string(role)}
}

// takesOne fails the test unless register_plan takes version of the document artifact with task
// as the plan's one task, which runs at act.
func (r *ownerStepsRig) takesOne(artifact string, version int, task string, act runningAct) {
	r.t.Helper()
	running := r.registeredRunning(artifact, version)
	if got, ok := running[task]; len(running) == 1 && ok && slices.Equal(got, []runningAct{act}) {
		return
	}
	var named []string
	for id, at := range running {
		if len(at) == 0 {
			named = append(named, id+" not running")
			continue
		}
		named = append(named, fmt.Sprintf("%s running at %+v", id, at))
	}
	slices.Sort(named)
	r.t.Fatalf("register_plan of %s's version %d names %q, want the one task %s running at %+v", r.key, version, named, task, act)
}

// refused fails the test unless register_plan refuses version of the document artifact with
// 409 PLAN_REFUSED, and answers the refusal.
func (r *ownerStepsRig) refused(artifact string, version int) api.PlanRefusal {
	r.t.Helper()
	status, body := r.registerPlan(artifact, version)
	var refusal api.PlanRefusal
	if err := json.Unmarshal([]byte(body), &refusal); err != nil || status != http.StatusConflict || refusal.Code != "PLAN_REFUSED" {
		r.t.Fatalf("register_plan of %s's version %d = %d %s, want 409 PLAN_REFUSED", r.key, version, status, body)
	}
	return refusal
}

// standsAsItWas fails the test unless task of the walk's issue stands where it stood before
// (taskRow) after the moment after. task must be the one task the daemon stores for the issue,
// and each step start of the issue must be one of task's (stepStarts).
func (r *ownerStepsRig) standsAsItWas(after, task, before string) {
	r.t.Helper()
	if got := r.taskRow(task); got != before {
		r.t.Fatalf("task %s after %s is %s, want it as it was: %s", task, after, got, before)
	}
	rows, err := r.st.Pool().Query(context.Background(), `select distinct task from plan_tasks where issue = $1 order by task`, r.key)
	if err != nil {
		r.t.Fatalf("read the tasks of %s: %v", r.key, err)
	}
	tasks, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		r.t.Fatalf("read the tasks of %s: %v", r.key, err)
	}
	if !slices.Equal(tasks, []string{task}) {
		r.t.Fatalf("after %s the daemon stores the tasks %q of %s, want the one task %s", after, tasks, r.key, task)
	}
	starts := r.stepStarts()
	if len(starts) == 0 {
		r.t.Fatalf("after %s the daemon stores no step start of %s, so the read of the starts sees nothing", after, r.key)
	}
	for _, start := range starts {
		if !strings.HasPrefix(start, task+" ") {
			r.t.Fatalf("after %s the step starts of %s are %q, want each a start of task %s", after, r.key, starts, task)
		}
	}
}

// The one plan of an issue, through the whole daemon over a real Dispatch of this tree. Five
// issues are walked. C, D, E and F run on a project whose default steps start with the planner,
// and G on one whose default steps name no planner.
//
// Issue C runs the project's default steps. Its planner writes plan.md with the block default and
// three task blocks under it by parent. The three have needs and serves, and a comment is anchored
// on t2. The plan review approves that version. While the implementer works, the owner gives a
// version that changes only the text of default, and while the tester works a second one. Each
// answer is 200 and names default, the one task, running at its step. The task keeps its state,
// its place and its counts, and the daemon stores no other task and starts no other step. The
// planner's part of plan.md reads as the planner left it: the same blocks, ids, texts, serves,
// needs and tokens, the comment anchored, the approval of version 1, and one plan task for each
// requirement. The planner then writes a version that changes t2, and the owner gives it. It
// changes no task. The planner writes a newer version that takes t3's parent away, which Legion
// refuses, and the owner gives the version before it again. Legion reads each parent at that
// version and finds the one task default. Each later start task holds the owner's newest text and
// no planner's text.
//
// Issue D runs an owner's plan whose task a names the planner as a step. The planner writes two
// task blocks under a. While the implementer works, the owner gives a version that changes only
// a's text. The answer is 200, task a goes on at its step, and the planner's blocks stay as they
// were.
//
// Issue E runs the default steps, and its planner's three blocks stand at the top of plan.md with
// no block default. The owner's version adds default and gives each block the parent default. The
// answer is 200, the three blocks keep their ids, and Legion carries out the one task default.
//
// Issue F is as E, but the owner's version leaves t1 at the top. Legion refuses it, naming default
// and t1 and neither block under default, and records nothing.
//
// Issue G runs default steps that name no planner and has no plan.md. While the implementer works,
// the owner writes plan.md with the one block default and gives it. The answer is 200, and the
// reviewer's start task holds the owner's text.
func TestOnePlanHoldsTheOwnersTextAndThePlannersBlocks(t *testing.T) {
	t.Parallel()
	// The scratch Dispatch creates the parent its manifest reads now and creates from the next
	// version, which every write of a parent here needs.
	staged := scratchDispatchOptions{createStaged: true}
	t.Run("default steps with a planner", func(t *testing.T) {
		r := newOwnerStepsRig(t, newLiveProofOn(t, workflowNATS(t)), startScratchDispatchOn(t, staged), 4, config.Project{})
		r.ownersTextOverThePlannersBlocks()
		r.on(r.issues[1].key)
		r.ownersPlanOverThePlannersBlocks()
		r.on(r.issues[2].key)
		r.ownerPlacesThePlannersBlocks()
		r.on(r.issues[3].key)
		r.ownerLeavesABlockAtTheTop()
	})
	t.Run("default steps with no planner", func(t *testing.T) {
		r := newOwnerStepsRig(t, newLiveProofOn(t, workflowNATS(t)), startScratchDispatchOn(t, staged), 1,
			config.Project{DefaultSteps: []planwords.Step{{Role: "implementer"}, {Role: "reviewer"}}})
		r.ownerWritesThePlan()
	})
}

// ownersTextOverThePlannersBlocks is issue C (TestOnePlanHoldsTheOwnersTextAndThePlannersBlocks).
func (r *ownerStepsRig) ownersTextOverThePlannersBlocks() {
	t := r.t
	t.Helper()
	const cOne, cTwo = "Owner text one", "Owner text two"
	r.editPlan(r.specs[r.key], map[string]any{"op": "insert", "after": "end", "markdown": threeRequirements}, 2)
	r.openGateAt(2, phase.Planning)
	planner := r.worker(claim.RolePlanner)
	holdsOwnersText(t, "planner", takeText(planner), defaultText, cOne)
	doc := r.upload("plan.md", plannerTasks(r.key, defaultText, "t1", "t2", "t3"), "").id
	r.person(http.MethodPost, "/api/v1/issues/"+r.key+"/comments", map[string]any{
		"body": "The writer needs the reader's format.", "anchor": map[string]any{"artifact": doc, "quote": "Build the writer."}}, http.StatusCreated, nil)
	r.planReview(doc, 1)
	approved := r.readPlan(doc, "default")
	if !slices.Equal(approved.tasks, []string{"t1", "t2", "t3"}) || len(approved.comments) != 1 || !strings.HasSuffix(approved.comments[0], "orphaned false") ||
		!maps.Equal(approved.served, map[string]int{"r1": 1, "r2": 1, "r3": 1}) || approved.approved != 1 {
		t.Fatalf("the planner's approved plan.md reads %+v, want t1, t2 and t3, one anchored comment, one plan task for each requirement and the review's approval of version 1", approved)
	}
	r.finish(planner, "plan", api.HandoffCompleteRequest{Summary: "planned"}, phase.Implementing)
	implementer := r.worker(claim.RoleImplementer)
	holdsOwnersText(t, "implementer", takeText(implementer), defaultText, cOne)

	before := r.taskRow("default")
	r.editPlan(doc, map[string]any{"op": "replace", "find": defaultText, "with": cOne}, 2)
	r.takesOne(doc, 2, "default", stepAt(2, claim.RoleImplementer))
	r.standsAsItWas("the owner's first version", "default", before)
	r.holdsPlan("the owner's first version", doc, "default", approved)
	if !r.github.opened(r.number) {
		r.github.open("legion/"+r.key, r.number)
	}
	r.finish(implementer, "implement", api.HandoffCompleteRequest{Summary: "implemented", Commit: stepRecordsHead}, phase.Testing)
	tester := r.worker(claim.RoleTester)
	holdsOwnersText(t, "tester", takeText(tester), cOne, append([]string{defaultText}, elementTexts...)...)

	before = r.taskRow("default")
	r.editPlan(doc, map[string]any{"op": "replace", "find": cOne, "with": cTwo}, 3)
	r.takesOne(doc, 3, "default", stepAt(3, claim.RoleTester))
	r.standsAsItWas("the owner's second version", "default", before)
	r.holdsPlan("the owner's second version", doc, "default", approved)

	// The planner's version 4 changes t2, and the owner gives it.
	r.editPlan(doc, map[string]any{"op": "replace", "find": "Write each record once.", "with": "Write each record once, and log it."}, 4)
	r.takesOne(doc, 4, "default", stepAt(3, claim.RoleTester))
	r.standsAsItWas("the version that changes t2", "default", before)
	changed := r.readPlan(doc, "default")
	if !slices.Equal(changed.tasks, approved.tasks) || changed.blocks["t1"] != approved.blocks["t1"] || changed.blocks["t3"] != approved.blocks["t3"] ||
		!strings.Contains(changed.blocks["t2"], "Write each record once, and log it.") || !slices.Equal(changed.comments, approved.comments) ||
		!maps.Equal(changed.served, approved.served) || changed.approved != approved.approved {
		t.Fatalf("after the version that changes t2 the planner's part of plan.md reads\n%+v\nwant t2's new text, and t1, t3, the comment, the served requirements and the approval as approved\n%+v", changed, approved)
	}

	// The planner's version 5 takes t3's parent away, so t3 stands at the top and Legion refuses
	// it. The owner gives version 4 again, whose t3 stands under default.
	r.editPlan(doc, map[string]any{"op": "retype", "block": "t3", "type": planwords.TaskBlockType, "attributes": map[string]any{
		"needs": []string{"dispatch://" + r.key + "/artifact/plan-md#b-t2"}, "serves": []string{"dispatch://" + r.key + "/spec#b-r3"}}}, 5)
	if refusal := r.refused(doc, 5); !slices.ContainsFunc(refusal.Reasons, func(reason api.PlanReason) bool { return strings.Contains(reason.Reason, "t3") }) {
		t.Fatalf("Legion refuses version 5 with %+v, want a reason naming t3, the block at the top", refusal.Reasons)
	}
	r.takesOne(doc, 4, "default", stepAt(3, claim.RoleTester))
	r.standsAsItWas("version 4 given again", "default", before)
	r.finish(tester, "test", api.HandoffCompleteRequest{Summary: "the criteria read pass", Verdict: "pass"}, phase.Reviewing)
	holdsOwnersText(t, "reviewer", takeText(r.worker(claim.RoleReviewer)), cTwo, append([]string{cOne, defaultText}, elementTexts...)...)
}

// ownersPlanOverThePlannersBlocks is issue D (TestOnePlanHoldsTheOwnersTextAndThePlannersBlocks).
func (r *ownerStepsRig) ownersPlanOverThePlannersBlocks() {
	t := r.t
	t.Helper()
	const dOne, dTwo = "Owner text three", "Owner text four"
	doc := r.document("plan.md", textPlan("a", "planner implementer reviewer", dOne))
	if status, body := r.registerPlan(doc, 1); status != http.StatusOK {
		t.Fatalf("register_plan of %s's version 1 = %d %s, want 200", r.key, status, body)
	}
	r.openGate(phase.Planning)
	planner := r.worker(claim.RolePlanner)
	holdsOwnersText(t, "planner", takeText(planner), dOne)
	r.editPlan(doc, map[string]any{"op": "insert", "after": "end", "markdown": plannerUnder(r.key, "a")}, 2)
	r.planReview(doc, 2)
	approved := r.readPlan(doc, "a")
	if !slices.Equal(approved.tasks, []string{"p1", "p2"}) || approved.approved != 2 {
		t.Fatalf("the planner's approved plan.md reads %+v, want p1 and p2 under a and the review's approval of version 2", approved)
	}
	r.finish(planner, "plan", api.HandoffCompleteRequest{Summary: "planned"}, phase.Implementing)
	holdsOwnersText(t, "implementer", takeText(r.worker(claim.RoleImplementer)), dOne)

	before := r.taskRow("a")
	r.editPlan(doc, map[string]any{"op": "replace", "find": dOne, "with": dTwo}, 3)
	r.takesOne(doc, 3, "a", stepAt(2, claim.RoleImplementer))
	r.standsAsItWas("the owner's text-only version", "a", before)
	r.holdsPlan("the owner's text-only version", doc, "a", approved)
}

// plannerAtTheTop walks the walk's issue on the default steps to its implementer. Its spec marks
// three requirements, and its planner writes plan.md with its three task blocks at the top and no
// block default (plannerTasks), which the plan review approves. It answers the plan's id.
func (r *ownerStepsRig) plannerAtTheTop() string {
	r.t.Helper()
	r.editPlan(r.specs[r.key], map[string]any{"op": "insert", "after": "end", "markdown": threeRequirements}, 2)
	r.openGateAt(2, phase.Planning)
	planner := r.worker(claim.RolePlanner)
	takeText(planner)
	written := r.upload("plan.md", plannerTasks(r.key, ""), "")
	r.planReview(written.id, written.version)
	r.finish(planner, "plan", api.HandoffCompleteRequest{Summary: "planned"}, phase.Implementing)
	holdsOwnersText(r.t, "implementer", takeText(r.worker(claim.RoleImplementer)), defaultText)
	return written.id
}

// ownerPlacesThePlannersBlocks is issue E (TestOnePlanHoldsTheOwnersTextAndThePlannersBlocks).
func (r *ownerStepsRig) ownerPlacesThePlannersBlocks() {
	t := r.t
	t.Helper()
	doc := r.plannerAtTheTop()
	before := r.taskRow("default")
	given := r.upload("plan.md", plannerTasks(r.key, "Owner text five", "t1", "t2", "t3"), r.live(doc).Token)
	if given.id != doc || given.version != 2 {
		t.Fatalf("the owner's version of plan.md stored %+v, want version 2 of %s", given, doc)
	}
	r.takesOne(doc, 2, "default", stepAt(2, claim.RoleImplementer))
	r.standsAsItWas("the owner's version that places the planner's blocks", "default", before)
	ids, renderings := taskRenderings(t, r.live(doc))
	if !slices.Equal(ids, []string{"default", "t1", "t2", "t3"}) {
		t.Fatalf("after the owner's version plan.md holds the task blocks %q, want default and the planner's t1, t2 and t3", ids)
	}
	for _, id := range ids[1:] {
		if !strings.Contains(renderings[id], `parent="dispatch://`+r.key+`/artifact/plan-md#b-default"`) {
			t.Fatalf("after the owner's version task block %s reads %q, want it under default by parent", id, renderings[id])
		}
	}
}

// ownerLeavesABlockAtTheTop is issue F (TestOnePlanHoldsTheOwnersTextAndThePlannersBlocks).
func (r *ownerStepsRig) ownerLeavesABlockAtTheTop() {
	t := r.t
	t.Helper()
	doc := r.plannerAtTheTop()
	before := r.taskRow("default")
	given := r.upload("plan.md", plannerTasks(r.key, "Owner text six", "t2", "t3"), r.live(doc).Token)
	refusal := r.refused(given.id, given.version)
	if len(refusal.Reasons) != 1 {
		t.Fatalf("Legion refuses the version that leaves t1 at the top with %+v, want one reason", refusal.Reasons)
	}
	if reason := refusal.Reasons[0].Reason; !strings.Contains(reason, "default") || !strings.Contains(reason, "t1") ||
		strings.Contains(reason, "t2") || strings.Contains(reason, "t3") {
		t.Fatalf("Legion refuses the version that leaves t1 at the top with %q, want it to name default and t1 and neither block under default", reason)
	}
	r.standsAsItWas("the refused version", "default", before)
}

// ownerWritesThePlan is issue G (TestOnePlanHoldsTheOwnersTextAndThePlannersBlocks).
func (r *ownerStepsRig) ownerWritesThePlan() {
	t := r.t
	t.Helper()
	const gText = "Owner text seven"
	r.openGate(phase.Implementing)
	implementer := r.worker(claim.RoleImplementer)
	holdsOwnersText(t, "implementer", takeText(implementer), defaultText, gText)
	before := r.taskRow("default")
	doc := r.document("plan.md", textPlan("default", "", gText))
	r.takesOne(doc, 1, "default", stepAt(1, claim.RoleImplementer))
	r.standsAsItWas("the owner's plan.md", "default", before)
	if !r.github.opened(r.number) {
		r.github.open("legion/"+r.key, r.number)
	}
	r.finish(implementer, "implement", api.HandoffCompleteRequest{Summary: "implemented", Commit: stepRecordsHead}, phase.Reviewing)
	holdsOwnersText(t, "reviewer", takeText(r.worker(claim.RoleReviewer)), gText, defaultText)
}
