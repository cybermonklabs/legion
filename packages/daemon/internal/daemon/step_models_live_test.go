//go:build e2e

// The whole chain of a step record's models, driven through the real shim and the real daemon on
// both harnesses, and the reads of a stand-in's own stream that the chain counts by.
//
// What a reader of a stand-in's stream must hold: one response is one id. A step's record counts
// one call for each response its agent made, so two responses that carry one id count as one
// call, whatever the harness did. TestTheStandInGivesEachResponseItsOwnID holds the stand-in
// helpers of live_agent_proof_test.go to that, over the recorder alone: no pane, no daemon and no
// harness, so a reader of this file sees what the counting rests on before it reads the chain.
package daemon

// What the chain reads outside its own package, which no import of this file carries: each is a
// program it spawns or a file that program reads. The packs of the two Oh My Pi plugins are
// declared in live_agent_proof_test.go, whose installProofPlugins this test runs.
//ci:reads packages/claude-envoy/** TestStepModelsLive builds the claude-envoy plugin each Claude Code pane loads, with bun packages/claude-envoy/scripts/build.ts
//ci:reads packages/envoy-client/** TestStepModelsLive reads each issue's records with the real dispatch CLI, bun packages/envoy-client/bin/dispatch.ts (dispatchCLI)
//ci:reads scripts/e2e/step-models-check.sh TestStepModelsLive runs the comparator over both issues
//ci:reads scripts/e2e/lib/step-models-check.ts scripts/e2e/step-models-check.sh, which TestStepModelsLive runs, is its wrapper
//ci:reads scripts/e2e/lib/dispatch-read.sh scripts/e2e/step-models-check.sh, which TestStepModelsLive runs, sources it to run the real dispatch CLI

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/testbin"
)

// ---- the model stand-in of one driven pane, and the responses it served --------------------------

// stepModelsAnswerDelay is what every stand-in of the chain waits before each answer, the same for
// every request, as a model's own latency. A step's start read is made before its prompt reaches
// the pane, so this puts the read more than the comparator's two seconds from the first response
// of the turn that prompt starts.
const stepModelsAnswerDelay = 3 * time.Second

// stepModelsWaiting is the text a stand-in answers a turn it makes no call in, the tool's result
// among them: the WAITING line the phase-stall reminder takes as a worker that said it is waiting,
// so the reminder adds no turn of its own (packages/pi-shared/src/phase-stall.ts, WAITING_REPLY).
const stepModelsWaiting = "WAITING on the daemon's next task: the legion tool has answered."

// stepModelsSubagentMarker is in the prompt of each Claude Code subagent this chain starts, which
// is how a Claude pane's stand-in tells a subagent's request from a turn of the role's. Oh My Pi's
// subagent is told by deepWorkerSystemLine instead, which Oh My Pi puts in the system prompt of a
// `task` subagent of the deep-worker agent and nowhere else (busy_relaunch_live_test.go).
const stepModelsSubagentMarker = "STEP-MODELS-SUBAGENT"

// servedResponse is one response a stand-in served, as the run writes it to standin-served.json:
// the claim whose pane asked, the response's own id, whether it was a subagent's rather than the
// agent's own conversation, the model the answer named, and when the answer ended. A record's
// counts are held to this list rather than to the transcript, since the transcript is the
// harness's own account of the same calls, which the comparator reads apart.
type servedResponse struct {
	Claim    string    `json:"claim"`
	ID       string    `json:"id"`
	Subagent bool      `json:"subagent"`
	Model    string    `json:"model"`
	EndedAt  time.Time `json:"endedAt"`
}

// stepModelsStep is what a stand-in answers for one step of its claim: four responses where the
// agent ends the step itself, three where another fact ends it.
//
//   - ended: a text block and a shell call under one id, then a subagent call, then the call of
//     the `legion` tool that completes the handoff, then the WAITING text that ends the turn after
//     the tool's result. The end read comes as the third response's call is applied, and the
//     fourth waits three seconds, so it ends after that read: the record answers the three calls
//     of the set model and none of the subagent's.
//   - stopped: the first two as above, then the WAITING text, which ends the turn. The architect
//     ends the step over the daemon's route while the claim sits idle.
type stepModelsStep struct {
	ended bool
	// verdict rides the handoff completion of an ended step: a tester's `fail` sends the issue
	// back for a second step of the same implementer, which is how one issue gives each harness
	// two steps its agent ended.
	verdict string
}

// stepModelsStandin is the model endpoint of one driven pane. Its harness decides the names of the
// three tools a turn calls and how a subagent's request is told from the role's; its steps are the
// script, one entry per step the claim works in turn; and its list is what it answered.
type stepModelsStandin struct {
	*modelStandin

	token   claim.Token
	harness string
	// model is what every answer of this stand-in names. It is the model the pane asked for
	// everywhere in the chain; only the capture that measures what a transcript holds of a model
	// that never answered serves another.
	model string
	steps []stepModelsStep

	mu sync.Mutex
	// step is the step of steps this stand-in is answering, and inStep how many responses of the
	// agent's own conversation it has served within it. A step ends with the text that ends its
	// turn, and the next prompt of the role's starts the next.
	step, inStep int
	ending       bool
	list         []servedResponse
}

// newStepModelsStandin serves one pane's endpoint under the provider name, which is also the
// provider half of the model an Oh My Pi overlay names (standinModel).
func newStepModelsStandin(t *testing.T, name, harness, model string, token claim.Token, steps []stepModelsStep) *stepModelsStandin {
	t.Helper()
	s := &stepModelsStandin{
		modelStandin: &modelStandin{name: name}, token: token, harness: harness, model: model, steps: steps,
	}
	s.serve(t, s)
	return s
}

// claudeTools and ompTools are the three tools a turn calls on each harness: a shell command, a
// subagent, and the `legion` tool, which Claude Code prefixes with its one MCP server's name.
var (
	claudeTools = [3]string{"Bash", "Task", "mcp__legion__legion"}
	ompTools    = [3]string{"bash", "task", "legion"}
)

// tools is this harness's three tool names.
func (s *stepModelsStandin) tools() [3]string {
	if s.harness == "claude" {
		return claudeTools
	}
	return ompTools
}

func (s *stepModelsStandin) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, ok := s.take(w, req)
	if !ok {
		return
	}
	seen := readStandinRequest([]byte(body), time.Now())
	subagent := seen.subagent
	if s.harness == "claude" {
		subagent = strings.Contains(seen.conversation, stepModelsSubagentMarker)
	}
	// The same wait before every answer, a model's own latency, so a read made before a prompt is
	// more than the comparator's two seconds from the response that prompt earns.
	time.Sleep(stepModelsAnswerDelay)
	id := s.answer(w, seen, subagent)
	s.mu.Lock()
	s.list = append(s.list, servedResponse{
		Claim: string(s.token), ID: id, Subagent: subagent, Model: s.model, EndedAt: time.Now(),
	})
	s.mu.Unlock()
}

// answer writes one response and gives back its id. A subagent's request is answered with one text
// that ends it, whatever the role's step is doing: a subagent is no turn of the agent's own
// conversation, and no count of a record names it.
func (s *stepModelsStandin) answer(w http.ResponseWriter, seen seenRequest, subagent bool) string {
	if subagent {
		return streamAnthropicBlocks(w, s.model, "end_turn",
			stepModelsText("The subagent ran the scratch check and reports that it printed nothing."))
	}
	step, place := s.place(seen)
	tools := s.tools()
	switch {
	case place == 1:
		// One response of two content blocks under one id: what the turn is about to do, and the
		// shell call it makes. A reader that counts blocks rather than responses counts this twice.
		return streamAnthropicBlocks(w, s.model, "tool_use",
			stepModelsText("I will run the scratch check of this step."),
			stepModelsToolUse(s.name+"-shell", tools[0], map[string]any{"command": "true"}))
	case place == 2:
		return streamAnthropicBlocks(w, s.model, "tool_use",
			stepModelsToolUse(s.name+"-subagent", tools[1], s.subagentInput()))
	case place == 3 && step.ended:
		input := map[string]any{"op": "handoff_complete", "summary": "The step is done, and this reports it."}
		if step.verdict != "" {
			input["verdict"] = step.verdict
		}
		if s.harness != "claude" {
			input["i"] = "Reporting the step's completion"
		}
		return streamAnthropicBlocks(w, s.model, "tool_use",
			stepModelsToolUse(s.name+"-legion", tools[2], input))
	default:
		return streamAnthropicBlocks(w, s.model, "end_turn", stepModelsText(stepModelsWaiting))
	}
}

// subagentInput is the call that starts one subagent, in the shape each harness takes: Oh My Pi a
// `task` of the deep-worker agent, whose own request the system prompt names
// (deepWorkerSystemLine); Claude Code a `Task` of the general-purpose agent, whose prompt carries
// the marker this stand-in finds in the subagent's own first user message.
func (s *stepModelsStandin) subagentInput() map[string]any {
	if s.harness == "claude" {
		return map[string]any{
			"subagent_type": "general-purpose",
			"description":   "Run the scratch check",
			"prompt":        stepModelsSubagentMarker + ": run the scratch check in the workspace and report what it printed.",
		}
	}
	return subagentCall(stepModelsSubagentMarker)
}

// place is the step this response belongs to and its place in that step, counted over the agent's
// own conversation alone. A step ends with the text that ends its turn, and the next prompt of the
// role's — a new task from the daemon — starts the next step.
func (s *stepModelsStandin) place(seen seenRequest) (stepModelsStep, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ending && seen.prompt {
		s.step++
		s.inStep = 0
		s.ending = false
	}
	s.inStep++
	step := stepModelsStep{}
	if s.step < len(s.steps) {
		step = s.steps[s.step]
	}
	if s.inStep > 3 || (s.inStep == 3 && !step.ended) {
		// The text that ends this turn: the next prompt of the role's starts the next step.
		s.ending = true
	}
	return step, s.inStep
}

// servedList is every response this stand-in answered, oldest first.
func (s *stepModelsStandin) servedList() []servedResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]servedResponse(nil), s.list...)
}

// mainResponsesBetween is how many responses of the agent's own conversation this stand-in served
// between two times, which is what a step's record must count: a subagent's response is in none of
// them. The bounds are a step's two read times, and a response counts where it ended after the
// first and at or before the second.
func (s *stepModelsStandin) mainResponsesBetween(after, until time.Time) int {
	count := 0
	for _, served := range s.servedList() {
		if served.Subagent {
			continue
		}
		if served.EndedAt.After(after) && !served.EndedAt.After(until) {
			count++
		}
	}
	return count
}

// stepModelsText is one text block of a stand-in's answer.
func stepModelsText(text string) anthropicBlock {
	return anthropicBlock{
		open:  `{"type":"text","text":""}`,
		delta: fmt.Sprintf(`{"type":"text_delta","text":%q}`, text),
	}
}

// stepModelsToolUse is one tool_use block: the block opens with an empty input and the one delta
// carries the whole of it, as the plugins' own pane harness writes one.
func stepModelsToolUse(id, name string, input map[string]any) anthropicBlock {
	arguments, _ := json.Marshal(input)
	open, _ := json.Marshal(map[string]any{"type": "tool_use", "id": "toolu_" + id, "name": name, "input": map[string]any{}})
	delta, _ := json.Marshal(map[string]string{"type": "input_json_delta", "partial_json": string(arguments)})
	return anthropicBlock{open: string(open), delta: string(delta)}
}

// ---- the reads of a stand-in's own stream --------------------------------------------------------

// standinEvent is one event of a stand-in's Messages stream, as a harness reads it off the wire:
// the `event:` name and the JSON of the `data:` line under it.
type standinEvent struct {
	name string
	data map[string]any
}

// standinResponse is what one response of a stand-in says, read back from its own stream: the id
// of its `message_start`, the model that message names, and the index of each content block it
// opened, in the order it opened them.
type standinResponse struct {
	id     string
	model  string
	blocks []int
}

// readStandinEvents reads the events of one written response. The stand-ins write `event: <name>`
// and `data: <json>` with a blank line between events (streamAnthropicBlocks), which is the shape
// the Anthropic Messages stream has.
func readStandinEvents(t *testing.T, stream string) []standinEvent {
	t.Helper()
	var events []standinEvent
	var name string
	for _, line := range strings.Split(stream, "\n") {
		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			var data map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &data); err != nil {
				t.Fatalf("the stand-in wrote a data line of event %q that is no JSON object: %v\n%s", name, err, line)
			}
			events = append(events, standinEvent{name: name, data: data})
		}
	}
	if len(events) == 0 {
		t.Fatalf("the stand-in wrote no event at all:\n%s", stream)
	}
	return events
}

// readStandinResponse is the one response a stand-in wrote, read back from its stream. It fails
// the test where the stream holds no `message_start`, since every later read of it would be of
// nothing.
func readStandinResponse(t *testing.T, stream string) standinResponse {
	t.Helper()
	response := standinResponse{}
	started := false
	for _, event := range readStandinEvents(t, stream) {
		switch event.name {
		case "message_start":
			started = true
			message, _ := event.data["message"].(map[string]any)
			response.id, _ = message["id"].(string)
			response.model, _ = message["model"].(string)
		case "content_block_start":
			index, _ := event.data["index"].(float64)
			response.blocks = append(response.blocks, int(index))
		}
	}
	if !started {
		t.Fatalf("the stand-in wrote no message_start, so the response has no id:\n%s", stream)
	}
	return response
}

// TestTheStandInGivesEachResponseItsOwnID holds the stand-ins of the live proofs to one id per
// response. A record counts one call for each response of the agent's own conversation, and a
// reader tells one response from the next by the id its `message_start` carries, so two responses
// under one id would be counted as one call and a turn of two answers would be recorded as one.
func TestTheStandInGivesEachResponseItsOwnID(t *testing.T) {
	t.Parallel()

	t.Run("two responses have two ids", func(t *testing.T) {
		first, second := httptest.NewRecorder(), httptest.NewRecorder()
		streamAnthropicText(first, "omp", "the first answer")
		streamAnthropicText(second, "omp", "the second answer")

		one := readStandinResponse(t, first.Body.String())
		two := readStandinResponse(t, second.Body.String())
		if one.id == "" || two.id == "" {
			t.Fatalf("a response carries no id: %q and %q", one.id, two.id)
		}
		if one.id == two.id {
			t.Fatalf("two responses of the stand-in carry the one id %q, so a reader that counts "+
				"one call for each id counts one call for two responses", one.id)
		}
	})

	t.Run("one response of two blocks carries one id", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		streamAnthropicBlocks(recorder, standinModel("omp"), "tool_use",
			anthropicBlock{open: `{"type":"text","text":""}`, delta: `{"type":"text_delta","text":"what I am about to do"}`},
			anthropicBlock{
				open:  `{"type":"tool_use","id":"toolu_standin","name":"bash","input":{}}`,
				delta: `{"type":"input_json_delta","partial_json":"{\"command\":\"true\"}"}`,
			})

		response := readStandinResponse(t, recorder.Body.String())
		if got := response.blocks; !slices.Equal(got, []int{0, 1}) {
			t.Fatalf("the response opened blocks at %v; want one block at 0 and one at 1, so a host "+
				"reads the text and the call of one answer", got)
		}
		if response.id == "" {
			t.Fatal("the response of two blocks carries no id")
		}
		if response.model != standinModel("omp") {
			t.Fatalf("the response names the model %q; want the served model %q", response.model, standinModel("omp"))
		}
	})
}

// ---- the run: two issues, four driven panes, both real harnesses --------------------------------

// stepModelsOmpProvider and stepModelsSpareProvider are the Oh My Pi providers of the run's
// models.yml: one per driven implementer, and one every other Oh My Pi claim talks to, which
// answers every turn with text alone.
const stepModelsSpareProvider = "stepmodelsspare"

// stepModelsClaudeModel is what each Claude pane is told to ask for (ANTHROPIC_MODEL) and what its
// record's set model must therefore be, under the provider name a record spells it with.
const (
	stepModelsClaudeModel    = "claude-model"
	stepModelsClaudeSetModel = "anthropic/" + stepModelsClaudeModel
)

// stepModelsEvidenceEnv names the directory the run writes its evidence to, which the CI job
// uploads whatever the result. Unset, the run writes under its own temporary directory, so a
// developer's run leaves nothing behind.
const stepModelsEvidenceEnv = "LEGION_E2E_STEP_MODELS_EVIDENCE"

// stepModelsIssue is one of the two issues the chain drives: its key as Dispatch made it, its
// pull request, the commit every record names, the panes' shared workspace, GitHub's stand-in for
// it, and the three claims of its roles.
type stepModelsIssue struct {
	name                           string
	key, title, head, workspace    string
	number                         int
	github                         *liveGitHub
	implementer, tester, architect claim.Token
}

// stepModelsLive is the run: the daemon booted over the local runtime, the real Dispatch this
// branch builds, two issues whose implementers are real Oh My Pi panes and whose testers are real
// Claude Code panes, and the evidence the job uploads.
type stepModelsLive struct {
	liveAgentProof
	dispatch *handoffDispatch
	tokens   *workflowTokenRecorder
	// root is the checkout this driver was built from; work the run's own temporary directory;
	// home and profile the Oh My Pi home and profile every pane runs under.
	root, work, home, profile string
	// bun, claude and pluginDir are what a Claude Code pane runs: the bun its MCP server runs on,
	// the pinned Claude Code, and the claude-envoy plugin this run built from the checkout.
	bun, claude, pluginDir string
	// evidence is the directory the run writes its records, logs, transcripts and captures to.
	evidence string
	issues   []*stepModelsIssue
	// panes is each driven claim's own stand-in, and spare the one every other claim talks to.
	panes map[claim.Token]*stepModelsStandin
	spare *paneStandin
	// overlays is each Oh My Pi claim's PI_CONFIG_FILES, and spareOverlays every other claim's. A
	// Claude Code claim reads none: its model comes from ANTHROPIC_MODEL in its pane's own
	// environment (config.checkRoleConfigFiles refuses an overlay for such a role).
	overlays      map[claim.Token][]string
	spareOverlays []string
	// gitRunner runs every git and jj command the driver makes.
	gitRunner fixturegit.Runner
	// failedAt is the assertion the running check failed at, which run prints.
	failedAt string
}

// run runs one check as a subtest of its own, between its `== name` and `ok name (<s>s)` lines,
// with the run's reads failing that subtest rather than the whole test: every read of
// liveAgentProof goes through r.t, which is the check's own testing.T while it runs. So a failed
// check leaves the later checks to run, and every measurement is printed.
func (r *stepModelsLive) run(name string, body func()) {
	parent := r.t
	parent.Run(name, func(t *testing.T) {
		r.t, r.failedAt = t, ""
		started := time.Now()
		defer func() {
			r.t = parent
			if t.Failed() {
				at := r.failedAt
				if at == "" {
					at = "none of its named assertions (a wait or a read of its own)"
				}
				fmt.Printf("FAIL %s at %s (%ds)\n", name, at, int(time.Since(started).Seconds()))
			}
		}()
		fmt.Printf("== %s\n", name)
		body()
		fmt.Printf("ok %s (%ds)\n", name, int(time.Since(started).Seconds()))
	})
}

// fails ends the running check at its named assertion, which run prints.
func (r *stepModelsLive) fails(assertion, format string, args ...any) {
	r.t.Helper()
	r.failedAt = assertion
	r.t.Fatalf(format, args...)
}

// issueNamed is the run's issue of that name, A or B.
func (r *stepModelsLive) issueNamed(name string) *stepModelsIssue {
	for _, issue := range r.issues {
		if issue.name == name {
			return issue
		}
	}
	r.t.Fatalf("this run has no issue %s", name)
	return nil
}

// token is the claim token of role on key, as the daemon spells it.
func (r *stepModelsLive) token(key string, role claim.Role) claim.Token {
	r.t.Helper()
	token, err := claim.NewToken(r.project, key, role)
	if err != nil {
		r.t.Fatal(err)
	}
	return token
}

// evidenceFileAt is the path of one file of the run's evidence.
func (r *stepModelsLive) evidenceFileAt(name string) string {
	return filepath.Join(r.evidence, name)
}

// writeEvidence writes one file of the run's evidence, which the job uploads whatever the result.
func (r *stepModelsLive) writeEvidence(name string, body []byte) {
	r.t.Helper()
	if err := os.WriteFile(r.evidenceFileAt(name), body, 0o600); err != nil {
		r.t.Fatalf("write the evidence file %s: %v", name, err)
	}
}

// writeEvidenceJSON writes one file of the run's evidence as indented JSON.
func (r *stepModelsLive) writeEvidenceJSON(name string, value any) {
	r.t.Helper()
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		r.t.Fatalf("encode the evidence file %s: %v", name, err)
	}
	r.writeEvidence(name, append(encoded, '\n'))
}

// newStepModelsLive readies everything the panes need before the daemon boots: the real Dispatch
// with the project and the two issues; the `legion` binary and the two Oh My Pi plugins built from
// this checkout; the claude-envoy plugin built from it too; an Oh My Pi profile routed to the
// stand-ins; each issue's jj workspace at its implementer's pushed head; GitHub's stand-in at that
// head; the store seeded; and the daemon booted over the local runtime with both harnesses
// resolved.
//
// It sets no environment variable of its own process and no working directory, and it calls no
// fixtureenv.Live: it reaches no cluster and no GitHub, and the daemon's boot gets the Claude Code
// paths through overrides.environ, which is the daemon's environment alone.
func newStepModelsLive(t *testing.T) *stepModelsLive {
	t.Helper()
	omp, claudeCode := testbin.OMP(t), testbin.Claude(t)
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Fatalf("bun is not on PATH, and the chain builds the claude-envoy plugin and reads each record with it: %v", err)
	}
	for _, tool := range []string{"jj", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is not on PATH: %v", tool, err)
		}
	}
	proof := newLiveProofOn(t, workflowNATS(t))
	// A Dispatch project key is at most ten characters (contracts ProjectKeyPattern), and the
	// daemon's project is its Dispatch project, so the run takes a short one of its own.
	proof.cfg.Project = "SM" + randomSuffix(t)[:6]
	proof.cfg.Projects = map[string]config.Project{proof.cfg.Project: {}}
	// The testers run on Claude Code and the implementers and architects on Oh My Pi, so one run
	// drives both real harnesses behind the real shim.
	proof.cfg.WorkerBackends = map[claim.Role]runtime.Backend{claim.RoleTester: runtime.BackendClaude}
	if proof.project, err = claim.ProjectToken(proof.cfg.Project); err != nil {
		t.Fatal(err)
	}

	r := &stepModelsLive{
		liveAgentProof: liveAgentProof{liveProof: proof, omp: omp},
		tokens:         &workflowTokenRecorder{},
		bun:            bun,
		claude:         claudeCode,
		panes:          map[claim.Token]*stepModelsStandin{},
		overlays:       map[claim.Token][]string{},
	}
	r.root = proofCheckoutRoot(t)
	r.work = t.TempDir()
	r.evidence = os.Getenv(stepModelsEvidenceEnv)
	if r.evidence == "" {
		r.evidence = filepath.Join(r.work, "step-models-evidence")
	}
	if err := os.MkdirAll(r.evidence, 0o700); err != nil {
		t.Fatalf("make the evidence directory %s: %v", r.evidence, err)
	}
	r.note("the run writes its evidence to %s", r.evidence)

	r.dispatch = startHandoffDispatch(t, r.root)
	if err := os.WriteFile(r.cfg.DispatchTokenFile, []byte(r.dispatch.token+"\n"), 0o600); err != nil {
		t.Fatalf("write the Dispatch token file: %v", err)
	}
	r.cfg.DispatchURL = r.dispatch.base

	r.legion = buildProofLegion(t, r.root, r.work)
	r.home = filepath.Join(r.work, "omp-home")
	r.profile = "legion-step-models-" + strings.ToLower(r.cfg.Project)
	installProofPlugins(t, r.root, r.work, r.home, r.profile, omp)
	r.pluginDir = r.buildClaudeEnvoy()
	return r
}

// buildClaudeEnvoy builds the claude-envoy plugin this run's Claude panes load, from this
// checkout, as packages/claude-envoy/tests/legion-phase-hook-claude.test.ts builds it. The daemon
// resolves it from LEGION_CLAUDE_PLUGIN_DIR in its own environment (resolveClaude), which
// overrides.environ carries.
func (r *stepModelsLive) buildClaudeEnvoy() string {
	t := r.t
	t.Helper()
	out := filepath.Join(r.work, "claude-envoy")
	build := exec.Command(r.bun, filepath.Join(r.root, "packages", "claude-envoy", "scripts", "build.ts"), "--out", out)
	build.Dir = r.root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the claude-envoy plugin into %s: %v\n%s", out, err, output)
	}
	r.note("built the claude-envoy plugin this checkout ships into %s", out)
	return out
}

// setUpDispatch creates the run's project on Dispatch, sets liveRepo as its default repository,
// and creates the two issues, each in progress and carrying the legion label, as a running daemon
// leaves one. It fills in each issue's key, its pull request number and its three claim tokens.
func (r *stepModelsLive) setUpDispatch() {
	t := r.t
	t.Helper()
	r.dispatch.user(http.MethodPost, "/api/v1/projects",
		map[string]string{"key": r.cfg.Project, "name": "Step models chain"}, http.StatusCreated, nil)
	r.dispatch.user(http.MethodPut, "/api/v1/projects/"+r.cfg.Project+"/default-repository",
		map[string]any{"repo": liveRepo}, http.StatusOK, nil)
	for _, name := range []string{"A", "B"} {
		issue := &stepModelsIssue{name: name, title: "the chain's issue " + name}
		var created struct {
			Key string `json:"key"`
		}
		r.dispatch.user(http.MethodPost, "/api/v1/issues",
			map[string]string{"project": r.cfg.Project, "title": issue.title}, http.StatusCreated, &created)
		if created.Key == "" {
			t.Fatalf("Dispatch created the issue %q with no key", issue.title)
		}
		r.dispatch.user(http.MethodPatch, "/api/v1/issues/"+created.Key,
			map[string]any{"status": "in_progress", "labels": []string{"legion"}}, http.StatusOK, nil)
		issue.key = created.Key
		number, err := strconv.ParseInt(randomSuffix(t)[:6], 16, 64)
		if err != nil {
			t.Fatal(err)
		}
		issue.number = int(number) + 1
		issue.implementer = r.token(issue.key, claim.RoleImplementer)
		issue.tester = r.token(issue.key, claim.RoleTester)
		issue.architect = r.token(issue.key, claim.RoleArchitect)
		r.issues = append(r.issues, issue)
	}
}

// serveStandins starts one endpoint per driven claim, each scripted for the steps that claim
// works in turn, and the spare every other claim talks to.
//
// Issue A: its implementer ends its first and second steps itself and has its third stopped by
// the architect; its tester ends its first and second. Issue B: its implementer ends its first
// and second, and its tester ends its first and has its second stopped. Each tester's completion
// carries the verdict `fail`, which sends the issue back for another step of the same
// implementer, so one issue gives each harness two steps its agent ended.
func (r *stepModelsLive) serveStandins() {
	t := r.t
	t.Helper()
	ended := stepModelsStep{ended: true}
	failed := stepModelsStep{ended: true, verdict: "fail"}
	stopped := stepModelsStep{}
	scripts := map[string][2][]stepModelsStep{
		"A": {{ended, ended, stopped}, {failed, failed}},
		"B": {{ended, ended}, {failed, stopped}},
	}
	for _, issue := range r.issues {
		script := scripts[issue.name]
		lower := strings.ToLower(issue.name)
		r.panes[issue.implementer] = newStepModelsStandin(t, "smimplementer"+lower, "omp",
			standinModel("smimplementer"+lower), issue.implementer, script[0])
		r.panes[issue.tester] = newStepModelsStandin(t, "smtester"+lower, "claude",
			stepModelsClaudeSetModel, issue.tester, script[1])
	}
	r.spare = newPaneStandin(t, stepModelsSpareProvider)
}

// ompStandins is every Oh My Pi endpoint of the run, the spare last: the providers the run's
// models.yml names.
func (r *stepModelsLive) ompStandins() []*modelStandin {
	var standins []*modelStandin
	for _, issue := range r.issues {
		standins = append(standins, r.panes[issue.implementer].modelStandin)
	}
	return append(standins, r.spare.modelStandin)
}

// writeOmpProfile writes the profile every Oh My Pi pane shares: one provider per endpoint, one
// model each, and the settings a pane needs to run unattended.
func (r *stepModelsLive) writeOmpProfile() {
	t := r.t
	t.Helper()
	agent := filepath.Join(r.home, ".omp", "profiles", r.profile, "agent")
	if err := os.MkdirAll(agent, 0o700); err != nil {
		t.Fatalf("make the profile's agent directory %s: %v", agent, err)
	}
	var models, enabled strings.Builder
	models.WriteString("# Written by packages/daemon/internal/daemon/step_models_live_test.go.\nproviders:\n")
	for _, stand := range r.ompStandins() {
		fmt.Fprintf(&models, "  %s:\n    baseUrl: %s\n    api: anthropic-messages\n    apiKey: a-literal-key-no-endpoint-checks\n"+
			"    models:\n      - id: %s\n        name: %s\n        contextWindow: 200000\n        maxTokens: 8192\n",
			stand.name, stand.url, stand.name+standinModelID, stand.name)
		fmt.Fprintf(&enabled, "  - %s\n", standinModel(stand.name))
	}
	writeProofFile(t, filepath.Join(agent, "models.yml"), models.String())
	writeProofFile(t, filepath.Join(agent, "config.yml"),
		"# Written by packages/daemon/internal/daemon/step_models_live_test.go.\n"+
			"enabledModels:\n"+enabled.String()+
			// A fresh profile opens Oh My Pi's setup wizard, and a pane waiting on it never starts.
			"startup:\n  setupWizard: false\n")
}

// writeOverlays writes the Oh My Pi overlays each pane is told through PI_CONFIG_FILES: the
// operator's own, which every pane names first and which turns `retry.enabled` off so the host
// walks no fallback chain of its own, then one naming the single model the claim's stand-in
// serves. Only the Oh My Pi claims take one: a Claude Code role reads no PI_CONFIG_FILES, and its
// model is its pane's ANTHROPIC_MODEL (claudePaneEnvironment).
func (r *stepModelsLive) writeOverlays() {
	t := r.t
	t.Helper()
	dir := filepath.Join(r.work, "overlays")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	operator := filepath.Join(dir, "operator.yml")
	writeProofFile(t, operator, "# The operator's pane overlay (runtime.tmux.pi_config_files).\nretry:\n  enabled: false\n")
	for _, issue := range r.issues {
		pane := r.panes[issue.implementer]
		path := filepath.Join(dir, pane.name+".yml")
		writeRoleOverlay(t, path, standinModel(pane.name))
		r.overlays[issue.implementer] = []string{operator, path}
	}
	spare := filepath.Join(dir, "spare.yml")
	writeRoleOverlay(t, spare, standinModel(stepModelsSpareProvider))
	r.spareOverlays = []string{operator, spare}
}

// claudeKeyVariable is the name of the variable Claude Code reads its provider key from, and
// stepModelsStandinKey the literal this run's panes carry in it. No endpoint of this run checks
// it: every one of them is a stand-in of the test's own, and CI holds no model credential. The
// two are joined at run time rather than written as one literal, so the source carries nothing
// shaped like a key assignment.
const (
	claudeKeyVariable    = "ANTHROPIC_API_KEY"
	stepModelsStandinKey = "a-literal-value-no-endpoint-checks"
)

// claudePaneEnvironment is what every pane of this run carries beyond what a pane is told: a
// Claude pane's endpoint, a stand-in key, and the one model it asks for. A Claude Code role reads
// no Oh My Pi overlay, so this is where its set model is decided, and its stand-in serves that
// same model, as every endpoint of the chain serves the model it was asked for.
//
// Every child of this run carries these names, since the local runtime gives one environment to
// all of them, and an Oh My Pi pane reads none of them. The two testers therefore share one
// endpoint, and the chain tells their records apart by the claim, which every served response
// carries, rather than by the endpoint.
func (r *stepModelsLive) claudePaneEnvironment() []string {
	r.t.Helper()
	for _, issue := range r.issues {
		if pane, driven := r.panes[issue.tester]; driven && pane.url != "" {
			return []string{
				"ANTHROPIC_BASE_URL=" + pane.url,
				claudeKeyVariable + "=" + stepModelsStandinKey,
				"ANTHROPIC_MODEL=" + stepModelsClaudeModel,
			}
		}
	}
	r.t.Fatal("no Claude pane has an endpoint yet: serveStandins runs before this")
	return nil
}
