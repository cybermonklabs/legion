package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// TestRigWorld is the label's own code for the skill-scenario rig's worlds
// (packages/pi-legion/scripts/skill-scenarios/label-texts.sh world): it walks a world through
// this label's engine, as the steps tests walk a task, and writes every start row and every
// notice row the engine queued for the world's subject, oldest first. So each text an agent of
// the rig starts from is one this label's engine wrote. With RIG_TEXTS_REQUEST and
// RIG_TEXTS_ANSWER it reads the world request (scored-runs-contracts.ts, WorldRequest) and writes
// the world answer; with neither it runs its own cases.
func TestRigWorld(t *testing.T) {
	t.Parallel()
	request, answer := rigWorldFiles(t)
	if request != "" {
		writeRigWorldAnswer(t, answer, rigWorld(t, readRigWorldRequest(t, request)))
		return
	}

	// A label the daemon writes (`Task: <id>.`) and a planted line of the task's text, which
	// the implementer's start carries, and the working copy WorkspaceDir answers.
	t.Run("the task", func(t *testing.T) {
		const marker = "rig-marker-line-5d1e: the planted line of this task's text."
		world := rigOwnWorld()
		world.Plan = &rigWorldPlan{Task: "rig-marker-7f3", Steps: []string{"implementer", "reviewer"}, Text: "Build the rig's widget.\n" + marker}
		world.Role = string(claim.RoleImplementer)
		start := rigOneStart(t, rigWorld(t, world), claim.RoleImplementer)
		for _, want := range []string{" Task: rig-marker-7f3.", marker, world.Workspace} {
			if !strings.Contains(start, want) {
				t.Errorf("the implementer's start = %q, want it to hold %q", start, want)
			}
		}
	})

	// The report line the engine writes for an earlier step, from the record and version its
	// completion named.
	t.Run("the reports", func(t *testing.T) {
		world := rigOwnWorld()
		world.Plan = &rigWorldPlan{Task: "rig-reports", Steps: []string{"implementer", "reviewer"}, Text: "Build the rig's widget."}
		world.Earlier = []rigWorldStep{{Role: string(claim.RoleImplementer), HandoffRecord: "rig-implement-record", HandoffVersion: 3, Commit: rigWorldHead}}
		world.Role = string(claim.RoleReviewer)
		const want = " Step 1 (implementer) reported: implement version 3."
		if start := rigOneStart(t, rigWorld(t, world), claim.RoleReviewer); !strings.Contains(start, want) {
			t.Errorf("the reviewer's start = %q, want it to hold %q", start, want)
		}
	})

	// An earlier reviewer step passes by the review App's approval, its Legion footer naming the
	// pull request's head and the reviewer's session, and the reviewer's completion.
	t.Run("the reviewer's approval", func(t *testing.T) {
		world := rigOwnWorld()
		world.Plan = &rigWorldPlan{Task: "rig-approval", Steps: []string{"implementer", "reviewer", "tester"}, Text: "Build the rig's widget."}
		world.Earlier = []rigWorldStep{
			{Role: string(claim.RoleImplementer), HandoffRecord: "rig-implement-record", HandoffVersion: 3, Commit: rigWorldHead},
			{Role: string(claim.RoleReviewer), HandoffRecord: "rig-review-record", HandoffVersion: 1, Commit: rigWorldHead},
		}
		world.Role = string(claim.RoleTester)
		const want = " Step 2 (reviewer) reported: review version 1."
		if start := rigOneStart(t, rigWorld(t, world), claim.RoleTester); !strings.Contains(start, want) {
			t.Errorf("the tester's start = %q, want it to hold %q", start, want)
		}
	})

	// A root whose design gate is open, at its architect's launch: the start task its admission
	// queues, and the catch-up its launch queues, which names the root and its backlog child.
	t.Run("the owner", func(t *testing.T) {
		world := rigOwnWorld()
		backlog := "LEGION-2"
		world.Issues = append(world.Issues, rigWorldIssue{Key: backlog, Title: "The rig's child", Parent: &world.Subject, Tree: world.Subject, Status: "backlog"})
		world.PullRequest = nil
		world.Moment, world.Role = "owner", string(claim.RoleArchitect)
		got := rigWorld(t, world)
		if start := rigOneStart(t, got, claim.RoleArchitect); !strings.Contains(start, world.Subject) {
			t.Errorf("the architect's start = %q, want a task naming %s", start, world.Subject)
		}
		var catchUps []rigWorldNotice
		for _, notice := range got.Notices {
			if notice.Kind == "catch-up" {
				catchUps = append(catchUps, notice)
			}
		}
		if len(catchUps) != 1 {
			t.Fatalf("the notices = %+v, want one catch-up", got.Notices)
		}
		var notice record.Notice
		if err := json.Unmarshal(catchUps[0].Payload, &notice); err != nil || notice.CatchUp == nil {
			t.Fatalf("the catch-up's payload %s holds no catch-up: %v", catchUps[0].Payload, err)
		}
		var keys []string
		for _, issue := range notice.CatchUp.Issues {
			keys = append(keys, issue.Key)
		}
		if !slices.Contains(keys, world.Subject) || !slices.Contains(keys, backlog) {
			t.Errorf("the catch-up names %v, want the root %s and its backlog child %s", keys, world.Subject, backlog)
		}
	})
}

// rigWorldHead is the head of the own cases' pull request, which a review App's footer names.
const rigWorldHead = "5d1e5d1e5d1e5d1e5d1e5d1e5d1e5d1e5d1e5d1e"

// rigOwnWorld is the world each own case starts from: the root LEGION-1 at admitted, its design
// gate open at version 1 of a planted spec document, and its open pull request #7. Its project is
// LEGION, the one this package's fixtures spell their claims in (treeRoleToken), so a fixture's
// record of this world reads as this world's own.
func rigOwnWorld() rigWorldRequest {
	root, admitted := "LEGION-1", string(phase.Admitted)
	return rigWorldRequest{
		Project: "LEGION", Repository: "rig-owner/rig-repo", DefaultBranch: "trunk", Workspace: "/rig/workspaces/rig-owner/rig-repo/LEGION-1",
		Issues:      []rigWorldIssue{{Key: root, Title: "The rig's root", Tree: root, Status: "in_progress", Phase: &admitted}},
		Gate:        &rigWorldGate{Issue: root, Step: string(record.DesignStepSpec), Version: 1, Approved: true, ArtifactID: "rig-spec-document-5d1e"},
		Subject:     root,
		PullRequest: &rigWorldPullRequest{Number: 7, Head: rigWorldHead},
		Earlier:     []rigWorldStep{},
		Moment:      "step",
	}
}

// rigOneStart is the task of the one start of role the answer holds.
func rigOneStart(t *testing.T, answer rigWorldAnswer, role claim.Role) string {
	t.Helper()
	var tasks []string
	for _, start := range answer.Starts {
		if start.Role == string(role) {
			tasks = append(tasks, start.Task)
		}
	}
	if len(tasks) != 1 {
		t.Fatalf("the starts = %+v, want one of the %s", answer.Starts, role)
	}
	return tasks[0]
}

// rigWorldRequest is texts/world-request.json (WorldRequest in
// packages/pi-legion/scripts/skill-scenarios/scored-runs-contracts.ts).
type rigWorldRequest struct {
	Project       string               `json:"project"`
	Repository    string               `json:"repository"`
	DefaultBranch string               `json:"defaultBranch"`
	Workspace     string               `json:"workspace"`
	Issues        []rigWorldIssue      `json:"issues"`
	Gate          *rigWorldGate        `json:"gate"`
	Subject       string               `json:"subject"`
	Plan          *rigWorldPlan        `json:"plan"`
	Earlier       []rigWorldStep       `json:"earlier"`
	PullRequest   *rigWorldPullRequest `json:"pullRequest"`
	Moment        string               `json:"moment"`
	Role          string               `json:"role"`
}

type rigWorldIssue struct {
	Key    string  `json:"key"`
	Title  string  `json:"title"`
	Parent *string `json:"parent"`
	Tree   string  `json:"tree"`
	Status string  `json:"status"`
	// Phase is nil for an issue in no Legion phase, which this label records at admitted, where
	// every issue it records stands before its first phase starts (Engine.enterChild).
	Phase *string `json:"phase"`
}

type rigWorldGate struct {
	Issue      string `json:"issue"`
	Step       string `json:"step"`
	Version    int    `json:"version"`
	Approved   bool   `json:"approved"`
	ArtifactID string `json:"artifactId"`
}

type rigWorldPlan struct {
	Task  string   `json:"task"`
	Steps []string `json:"steps"`
	Text  string   `json:"text"`
}

type rigWorldStep struct {
	Role           string  `json:"role"`
	HandoffRecord  string  `json:"handoffRecord"`
	HandoffVersion int     `json:"handoffVersion"`
	Commit         string  `json:"commit"`
	Verdict        *string `json:"verdict"`
}

type rigWorldPullRequest struct {
	Number int    `json:"number"`
	Head   string `json:"head"`
}

// rigWorldAnswer is texts/world-answer.json (WorldAnswer).
type rigWorldAnswer struct {
	Starts  []rigWorldStart  `json:"starts"`
	Notices []rigWorldNotice `json:"notices"`
}

type rigWorldStart struct {
	Role  string `json:"role"`
	Issue string `json:"issue"`
	Task  string `json:"task"`
}

type rigWorldNotice struct {
	Issue   string          `json:"issue"`
	Kind    string          `json:"kind"`
	Row     int64           `json:"row"`
	Payload json.RawMessage `json:"payload"`
}

// rigWorldFiles is the request and the answer label-texts.sh names, both "" when it names
// neither and the test runs its own cases.
func rigWorldFiles(t *testing.T) (request, answer string) {
	t.Helper()
	request, answer = os.Getenv("RIG_TEXTS_REQUEST"), os.Getenv("RIG_TEXTS_ANSWER")
	if (request == "") != (answer == "") {
		t.Fatalf("RIG_TEXTS_REQUEST is %q and RIG_TEXTS_ANSWER is %q: give both or neither", request, answer)
	}
	return request, answer
}

// readRigWorldRequest reads the request at path, refusing a field this test does not handle by
// its name, and a request that leaves out what the walk needs.
func readRigWorldRequest(t *testing.T, path string) rigWorldRequest {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var req rigWorldRequest
	if err := decoder.Decode(&req); err != nil {
		t.Fatalf("the world request %s: %v", path, err)
	}
	if decoder.More() {
		t.Fatalf("the world request %s holds more than one JSON value", path)
	}
	return req
}

func writeRigWorldAnswer(t *testing.T, path string, answer rigWorldAnswer) {
	t.Helper()
	data, err := json.MarshalIndent(answer, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// rigWorld walks req through this label's engine on a database of its own and answers every
// start row and every notice row of the subject, oldest first:
//
//   - the engine is returnEngine's, for the request's project, the design-gate steps of a
//     deployment that leaves `gates.design` unset (config.Gates.Steps), the request's repository
//     and the request's workspace, which WorkspaceDir answers for every issue;
//   - each issue is recorded at its phase and status, each root with its tree's lifecycle, then
//     the gate and the subject's pull request;
//   - a plan is given as register_plan gives it (intake.PlanGiven), at version 2, whose one task
//     is the request's id and text;
//   - a subject at admitted with a plan, or at a step's moment, starts its first phase as the
//     waiting line starts it (Engine.StartFirstPhase), and each earlier step passes by its facts;
//   - at the owner's moment, a root at admission is started as admission starts it
//     (Engine.StartArchitect), and the subject's catch-up is queued as its architect's launch
//     queues it (QueueCatchUp).
func rigWorld(t *testing.T, req rigWorldRequest) rigWorldAnswer {
	t.Helper()
	checkRigWorldRequest(t, req)
	pool := migratedPool(t)
	engine := returnEngine()
	engine.cfg.Project = req.Project
	engine.cfg.Steps = config.Gates{}.Steps()
	engine.cfg.Repositories = fixedRepositories{served: map[string]string{req.Repository: req.DefaultBranch}}
	engine.cfg.WorkspaceDir = func(record.Issue) string { return req.Workspace }

	for i, issue := range req.Issues {
		at := phase.Admitted
		if issue.Phase != nil {
			at = phase.Phase(*issue.Phase)
		}
		seedIssue(t, pool, record.Issue{Key: issue.Key, Tree: issue.Tree, Project: req.Project, Title: issue.Title, Parent: issue.Parent,
			Phase: at, Generation: 1, Status: issue.Status, DispatchStatus: issue.Status, Rank: fmt.Sprintf("R%04d", i),
			LastDispatchSeq: 1, HandedOver: claim.IsTreeRoot(issue.Key, issue.Tree),
			Repo: req.Repository, DefaultBranch: req.DefaultBranch, GitHubRepo: req.Repository})
		if claim.IsTreeRoot(issue.Key, issue.Tree) {
			seedTreeLifecycle(t, pool, issue.Key)
		}
	}
	if gate := req.Gate; gate != nil {
		seeded := record.DesignGate{Issue: gate.Issue, Step: record.DesignStep(gate.Step), ArtifactID: gate.ArtifactID,
			LatestVersion: gate.Version, EverOpened: gate.Approved}
		if gate.Approved {
			seeded.ApprovedVersion = &gate.Version
		}
		seedGate(t, pool, seeded)
	}
	if pr := req.PullRequest; pr != nil {
		seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: req.Subject, Repo: req.Repository, Number: pr.Number,
			Branch: claim.IssueBranch(req.Subject), HeadSHA: pr.Head, Failing: []string{}})
	}

	walk := rigWalk{t: t, pool: pool, engine: engine, req: req}
	subject := issueOf(t, pool, engine, req.Subject)
	if req.Plan != nil {
		walk.givePlan(subject)
	}
	if subject.Phase == phase.Admitted && (req.Plan != nil || req.Moment == "step") {
		applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error { return engine.StartFirstPhase(ctx, tx, subject, nil) })
	}
	for i, step := range req.Earlier {
		walk.pass(i+1, step)
	}
	if req.Moment == "owner" {
		walk.owner()
	}

	answer := walk.rows()
	switch req.Moment {
	case "step":
		if !slices.ContainsFunc(answer.Starts, func(start rigWorldStart) bool { return start.Role == req.Role }) {
			t.Fatalf("the walk queued no start of the %s on %s: the starts are %+v", req.Role, req.Subject, answer.Starts)
		}
	case "owner":
		if !slices.ContainsFunc(answer.Notices, func(notice rigWorldNotice) bool { return notice.Kind == "catch-up" }) {
			t.Fatalf("the walk queued no catch-up on %s: the notices are %+v", req.Subject, answer.Notices)
		}
	}
	return answer
}

// checkRigWorldRequest refuses a request this test cannot walk, naming what it lacks.
func checkRigWorldRequest(t *testing.T, req rigWorldRequest) {
	t.Helper()
	for name, value := range map[string]string{"project": req.Project, "repository": req.Repository, "defaultBranch": req.DefaultBranch,
		"workspace": req.Workspace, "subject": req.Subject, "role": req.Role} {
		if value == "" {
			t.Fatalf("the world request names no %s", name)
		}
	}
	if req.Moment != "step" && req.Moment != "owner" {
		t.Fatalf("the world request's moment is %q, want step or owner", req.Moment)
	}
	if req.Moment == "owner" && req.Role != string(claim.RoleArchitect) {
		t.Fatalf("the world request's moment is the owner's and its role is %s: an owner is the architect", req.Role)
	}
	if !slices.ContainsFunc(req.Issues, func(issue rigWorldIssue) bool { return issue.Key == req.Subject }) {
		t.Fatalf("the world request's subject %s is none of its issues", req.Subject)
	}
	for _, issue := range req.Issues {
		if issue.Phase != nil && !slices.Contains(phase.All, phase.Phase(*issue.Phase)) {
			t.Fatalf("issue %s of the world request is in phase %q, which this label does not have", issue.Key, *issue.Phase)
		}
	}
	if req.Gate != nil && req.Gate.ArtifactID == "" {
		t.Fatal("the world request's gate names no artifactId")
	}
	if req.Plan == nil && len(req.Earlier) > 0 {
		t.Fatal("the world request names earlier steps and no plan they are steps of")
	}
	if req.Plan != nil {
		if len(req.Earlier) >= len(req.Plan.Steps) {
			t.Fatalf("the world request names %d earlier steps of a task of %d", len(req.Earlier), len(req.Plan.Steps))
		}
		for i, step := range req.Earlier {
			if role, _, _ := strings.Cut(req.Plan.Steps[i], ":"); role != step.Role {
				t.Fatalf("earlier step %d of the world request is the %s's, and step %d of its plan is %s", i+1, step.Role, i+1, req.Plan.Steps[i])
			}
		}
		if role, _, _ := strings.Cut(req.Plan.Steps[len(req.Earlier)], ":"); req.Moment == "step" && role != req.Role {
			t.Fatalf("the world request starts the %s, and step %d of its plan is %s", req.Role, len(req.Earlier)+1, req.Plan.Steps[len(req.Earlier)])
		}
	}
}

// rigWalk is one world's walk through the engine.
type rigWalk struct {
	t      *testing.T
	pool   *pgxpool.Pool
	engine *Engine
	req    rigWorldRequest
}

// token is the claim token of role on issue in the request's project.
func (w rigWalk) token(issue string, role claim.Role) claim.Token {
	w.t.Helper()
	project, err := claim.ProjectToken(w.req.Project)
	if err == nil {
		var token claim.Token
		if token, err = claim.NewToken(project, issue, role); err == nil {
			return token
		}
	}
	w.t.Fatalf("the %s's claim on %s: %v", role, issue, err)
	return ""
}

// apply applies fact as the daemon applies a fact (applyFacts), and fails on a refusal.
func (w rigWalk) apply(id string, fact intake.Fact) {
	w.t.Helper()
	if result := applyFacts(w.t, w.pool, w.engine)(id, fact); result.Refusal != nil {
		w.t.Fatalf("this label refused %s: %d %s: %s", id, result.Refusal.Status, result.Refusal.Code, result.Refusal.Message)
	}
}

// givePlan gives the subject's plan as the register_plan route records it (intake.PlanGiven), from
// the architect of its tree: version 2, one task with the request's id, steps and text.
func (w rigWalk) givePlan(subject record.Issue) {
	w.t.Helper()
	steps, err := plan.ParseSteps(strings.Join(w.req.Plan.Steps, " "))
	if err != nil {
		w.t.Fatalf("the world request's steps %q: %v", w.req.Plan.Steps, err)
	}
	w.apply("rig-plan", intake.PlanGiven{Issue: subject.Key, Generation: subject.Generation, GivenBy: w.token(subject.Tree, claim.RoleArchitect),
		Plan: plan.Plan{ArtifactID: "rig-plan-" + subject.Key, Version: 2, Tasks: []plan.Task{{ID: w.req.Plan.Task, Steps: steps, Text: w.req.Plan.Text}}}})
}

// rigReviewerSession is the session the reviewer's claim records, which its review's footer names.
const rigReviewerSession = "rig-reviewer-session"

// pass passes earlier step position by its facts: every step's completion with its record, version
// and commit, and a reviewer's then by the review App's approval of the pull request's head, its
// footer naming the reviewer's session and the entry of its round (review_test.go's footer). The
// reviewer's completion comes first, as a reviewer's agent completes once it has posted its review:
// the completion records the reviewer's handoff and moves nothing, and the review decides when it
// lands, so the next step's start names the reviewer's report. A review that landed first would move
// the task on before any report of the reviewer's was recorded.
func (w rigWalk) pass(position int, step rigWorldStep) {
	w.t.Helper()
	subject := issueOf(w.t, w.pool, w.engine, w.req.Subject)
	role := claim.Role(step.Role)
	verdict := ""
	if step.Verdict != nil {
		verdict = *step.Verdict
	}
	reviewed, entry := role == claim.RoleReviewer, int64(0)
	if reviewed {
		if w.req.PullRequest == nil {
			w.t.Fatalf("earlier step %d is a review, and the world request names no pull request", position)
		}
		if verdict != "" && verdict != "approved" {
			w.t.Fatalf("earlier step %d is a review with the verdict %q: this test passes a review by an approval alone", position, verdict)
		}
		verdict = ""
		entry = roleRowOf(w.t, w.pool, w.engine, subject.Key, role).Entry
		if _, err := w.pool.Exec(context.Background(), `insert into claims (token, project, tree, issue, role, generation, session, session_file,
			state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
			values ($1, $2, $3, $4, 'reviewer', $5, $6, '', 'working', 0, 0, 0, 0)
			on conflict (token) do update set session = excluded.session`,
			string(w.token(subject.Key, role)), w.req.Project, subject.Tree, subject.Key, int64(subject.Generation), rigReviewerSession); err != nil {
			w.t.Fatalf("record the reviewer's session: %v", err)
		}
	}
	w.apply(fmt.Sprintf("rig-step-%d", position), intake.HandoffComplete{Issue: subject.Key, Role: role, Claim: w.token(subject.Key, role),
		Generation: subject.Generation, Summary: fmt.Sprintf("Step %d (%s) is done: see its handoff record.", position, role), Verdict: verdict,
		Commit: step.Commit, HandoffRecord: step.HandoffRecord, HandoffVersion: step.HandoffVersion})
	if reviewed {
		pr := w.req.PullRequest
		w.apply(fmt.Sprintf("rig-review-%d", position), intake.PullRequestReview{Repo: w.req.Repository, Number: pr.Number, ID: int64(position),
			State: "approved", CommitID: pr.Head, HeadSHA: pr.Head, Author: w.engine.cfg.ReviewAppLogin,
			Body:        reviewBodyOfEntry("Approved.", rigReviewerSession, pr.Head, entry),
			SubmittedAt: w.engine.cfg.Clock().Add(5 * time.Second)})
	}
}

// owner brings the subject to its owner's moment: a tree root at admission gets the start its
// admission queues (Engine.StartArchitect), and every subject the catch-up of its architect's
// first launch (QueueCatchUp).
func (w rigWalk) owner() {
	w.t.Helper()
	subject := issueOf(w.t, w.pool, w.engine, w.req.Subject)
	if claim.IsTreeRoot(subject.Key, subject.Tree) && subject.Phase == phase.Admitted {
		applyInTx(w.t, w.pool, w.engine, func(ctx context.Context, tx pgx.Tx) error { return w.engine.StartArchitect(ctx, tx, subject) })
	}
	applyInTx(w.t, w.pool, w.engine, func(ctx context.Context, tx pgx.Tx) error {
		issue, err := w.engine.store.Issue(ctx, tx, w.req.Subject)
		if err != nil {
			return err
		}
		return QueueCatchUp(ctx, tx, w.engine.store, w.engine.cfg.Steps, *issue, 1, "", w.engine.cfg.Clock())
	})
}

// rows reads every start row and every notice row of the subject, oldest first, in one read of
// the outbox. A start that carries no task delivers nothing to its agent (it only launches the
// claim), so it is no text and is left out.
func (w rigWalk) rows() rigWorldAnswer {
	w.t.Helper()
	answer := rigWorldAnswer{Starts: []rigWorldStart{}, Notices: []rigWorldNotice{}}
	supervise, notice := string(record.SuperviseRequest{}.OutboxKind()), string(record.Notice{}.OutboxKind())
	rows, err := w.pool.Query(context.Background(), "select id, kind, payload from outbox where issue = $1 and kind in ($2, $3) order by id",
		w.req.Subject, supervise, notice)
	if err != nil {
		w.t.Fatalf("read the outbox of %s: %v", w.req.Subject, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var kind string
		var payload []byte
		if err := rows.Scan(&id, &kind, &payload); err != nil {
			w.t.Fatalf("read the outbox of %s: %v", w.req.Subject, err)
		}
		decoded, err := record.DecodeOutboxPayload(record.OutboxRow{ID: id, Kind: record.OutboxKind(kind), Issue: w.req.Subject, Payload: payload})
		if err != nil {
			w.t.Fatalf("decode outbox row %d of %s: %v", id, w.req.Subject, err)
		}
		switch value := decoded.(type) {
		case record.SuperviseRequest:
			if string(value.Op) == "start" && value.Task != "" {
				answer.Starts = append(answer.Starts, rigWorldStart{Role: string(value.Role), Issue: w.req.Subject, Task: value.Task})
			}
		case record.Notice:
			answer.Notices = append(answer.Notices, rigWorldNotice{Issue: w.req.Subject, Kind: string(value.Kind), Row: id, Payload: payload})
		}
	}
	if err := rows.Err(); err != nil {
		w.t.Fatalf("read the outbox of %s: %v", w.req.Subject, err)
	}
	return answer
}
