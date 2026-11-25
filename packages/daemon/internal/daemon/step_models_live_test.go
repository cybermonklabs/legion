//go:build e2e

// The whole chain of a step record's models, driven through the real shim and the real daemon on
// both harnesses, and the reads of a stand-in's own stream that the chain counts by.
//
// What a reader of a stand-in's stream must hold: one response is one id. A step's record counts
// one call for each response its agent made, so two responses that carry one id count as one
// call, whatever the harness did. TestTheStandInGivesEachResponseItsOwnID holds the stand-in
// helpers of live_agent_proof_test.go to that, over the recorder alone: no pane, no daemon and no
// harness, so a reader of this file sees what the counting rests on before it reads the chain.
//
// TestStepModelsLive is the chain. Everything is real but the model endpoints: the whole daemon,
// the Dispatch this checkout builds, read with the real `dispatch` CLI, and each pane the real
// `legion worker-shim` around a real harness. Each implementer is the pinned Oh My Pi with this
// checkout's two plugins, and each tester the pinned Claude Code behind `legion claude-bridge`
// with this checkout's claude-envoy build. Each architect is the test's own session on the worker
// stream: its grant gives each issue its plan and stops task a, and no check reads its record.
// Each pane talks to a stand-in of the test's own, which waits three seconds before every answer
// and keeps a list of each response it served, the oracle the record's counts are held to. The
// comparator then judges the records against the transcripts. The run also captures what a real
// Claude Code session writes when the model that answers is not the one it asked for.
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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/claudelaunch"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/tmux"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/stream"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testbin"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
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

// stepModelsCaptureMarker is the one prompt of a capture run (captureClaude), which starts its
// one turn as a task of the daemon's starts a step of a pane.
const stepModelsCaptureMarker = "STEP-MODELS-CAPTURE"

// servedResponse is one response a stand-in served, as the run writes it to standin-served.json:
// the claim whose pane asked, the response's own id, whether it was a subagent's rather than the
// agent's own conversation, whether it was a side request that is neither, the model the request
// asked for and the model the answer named, when the answer ended, and what asked for it with the
// place of its step it answered. A record's counts are held to this list rather than to the
// transcript, since the transcript is the harness's own account of the same calls, which the
// comparator reads apart.
type servedResponse struct {
	Claim    string `json:"claim"`
	ID       string `json:"id"`
	Subagent bool   `json:"subagent"`
	// Side is a request that is no turn of a conversation: the plugin's own side question, a
	// harness's own upkeep call, which offers no tool, or a call of another model than the one the
	// pane asks for.
	Side    bool      `json:"side,omitempty"`
	Asked   string    `json:"asked"`
	Model   string    `json:"model"`
	EndedAt time.Time `json:"endedAt"`
	// Turn says what asked for this answer (stepModelsTurn.shape), and Step and Place which step
	// of the script and which of its answers it was: 0 for an answer of no step's turn.
	Turn  string `json:"turn"`
	Step  int    `json:"step,omitempty"`
	Place int    `json:"place,omitempty"`
}

// main is whether the response is one of the agent's own conversation, which a record counts.
func (s servedResponse) main() bool {
	return !s.Subagent && !s.Side
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

// stepModelsEvent is what a stand-in tells the run's driver (driveTurns): a step its agent ends
// is about to call the `legion` tool, so the driver writes the step's handoff record as the
// claim's session and closes written; or, with written nil, the turn of a step another fact ends
// has ended, so the driver stops its task.
type stepModelsEvent struct {
	token   claim.Token
	step    int
	written chan struct{}
}

// stepModelsStandin is the model endpoint of one driven pane. Its harness decides the names of the
// three tools a turn calls and how a subagent's request is told from the role's; its steps are the
// script, one entry per step the claim works in turn; and its list is what it answered.
type stepModelsStandin struct {
	*modelStandin

	token   claim.Token
	harness string
	// served is the model id every answer of this stand-in names, and asked the one a turn of its
	// pane asks for. They are one model everywhere in the chain; only the capture that measures
	// what a transcript holds of a model that never answered serves another.
	served, asked string
	// marker is what the prompt that starts a step carries: the pane's own phase in a task of the
	// daemon's, or the capture's one prompt.
	marker string
	steps  []stepModelsStep
	// events reaches the run's driver, nil for a capture, which no driver drives; stopped closes
	// when the run ends, which lets a held answer go.
	events  chan<- stepModelsEvent
	stopped <-chan struct{}

	mu sync.Mutex
	// next is the step the next prompt carrying marker starts, step the one being answered,
	// inStep how many responses of the agent's own conversation it has served within it, and
	// inTurn whether its turn is still open: a step ends with the text that ends its turn.
	next, step, inStep int
	inTurn             bool
	list               []servedResponse
	// asking is how many Messages requests this stand-in holds unanswered, and answered when it
	// last ended an answer of any kind: what tells the driver the pane has gone quiet (quietFor).
	asking   int
	answered time.Time
}

// newStepModelsStandin serves one pane's endpoint under name. For an Oh My Pi implementer, name is
// also the provider half of its model (standinModel). For a Claude Code pane it is a label alone.
func newStepModelsStandin(t *testing.T, name, harness, served, asked, marker string, token claim.Token, steps []stepModelsStep,
	events chan<- stepModelsEvent, stopped <-chan struct{},
) *stepModelsStandin {
	t.Helper()
	s := &stepModelsStandin{
		modelStandin: &modelStandin{name: name}, token: token, harness: harness, served: served, asked: asked, marker: marker,
		steps: steps, events: events, stopped: stopped,
	}
	s.serve(t, s)
	return s
}

// The names each harness offers its three tools under, a shell command, a subagent and the
// `legion` tool, each first as this chain's stand-ins call it and then as another build of the
// harness may offer it: Claude Code prefixes its one MCP server's name, and Oh My Pi offers a tool
// under a spelling of its own (late_completion_live_test.go, callFor).
var (
	claudeTools = [3][]string{{"Bash"}, {"Task", "Agent"}, {"mcp__legion__legion"}}
	ompTools    = [3][]string{{"bash"}, {"task"}, {"legion"}}
)

// toolName is the name the request offers the tool of place under, the stand-in's own first name
// for it where the request offers none of its names.
func (s *stepModelsStandin) toolName(offered []string, place int) string {
	names := ompTools[place]
	if s.harness == "claude" {
		names = claudeTools[place]
	}
	for _, name := range names {
		if at := slices.IndexFunc(offered, func(tool string) bool { return strings.TrimLeft(tool, "_") == name }); at >= 0 {
			return offered[at]
		}
	}
	return names[0]
}

func (s *stepModelsStandin) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, ok := s.take(w, req)
	if !ok {
		return
	}
	// Only a Messages request is a response. Oh My Pi asks the same base for a token count of its
	// own, which is answered a count and served nothing.
	if !strings.HasSuffix(req.URL.Path, "/v1/messages") {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"input_tokens":1000}`)
		return
	}
	s.mu.Lock()
	s.asking++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.asking--
		s.answered = time.Now()
		s.mu.Unlock()
	}()
	turn := readStepModelsTurn([]byte(body))
	served := servedResponse{Claim: string(s.token), Asked: turn.model, Model: s.served, Turn: turn.shape}
	served.Subagent = strings.Contains(turn.system, deepWorkerSystemLine)
	if s.harness == "claude" {
		served.Subagent = strings.Contains(turn.firstUser, stepModelsSubagentMarker)
	}
	// Every turn of a session offers the session's tools. A request that offers none is the
	// harness's own upkeep, no turn of the session: Oh My Pi asks its "routing hint" calls while a
	// step's turn runs, and one read as a prompt would end that turn in the script.
	served.Side = !served.Subagent && (turn.side || len(turn.tools) == 0 || (turn.model != "" && turn.model != s.asked))

	var answer stepModelsAnswer
	var hold, turnEnded bool
	step := -1
	switch {
	case served.Subagent:
		// A subagent's request is answered with one text that ends it, whatever the role's step is
		// doing: a subagent is no turn of the agent's own conversation, and no count of a record
		// names it.
		answer = s.text("The subagent ran the scratch check and reports that it printed nothing.")
	case served.Side:
		// The plugin's own `<btw>` question, an upkeep call, or a call of another model: answered
		// as the plugins' own pane harness answers a side question, and counted by no record.
		answer = s.text("PROCEEDING")
	default:
		var script stepModelsStep
		var place int
		step, script, place = s.place(turn)
		if place > 0 {
			served.Step, served.Place = step+1, place
		}
		answer, hold, turnEnded = s.answerAt(turn.tools, script, place)
	}

	// A step its agent ends has its handoff record written before the call that completes it, as
	// a worker writes its record before it calls the tool.
	if hold && s.events != nil {
		written := make(chan struct{})
		if !s.tell(req.Context(), stepModelsEvent{token: s.token, step: step, written: written}) {
			return
		}
		select {
		case <-written:
		case <-s.stopped:
		case <-req.Context().Done():
			return
		}
	}
	// The same wait before every answer, a model's own latency, so a read made before a prompt is
	// more than the comparator's two seconds from the response that prompt earns.
	time.Sleep(stepModelsAnswerDelay)
	served.ID = answer(w)
	served.EndedAt = time.Now()
	s.mu.Lock()
	s.list = append(s.list, served)
	s.mu.Unlock()
	if turnEnded && s.events != nil {
		s.tell(req.Context(), stepModelsEvent{token: s.token, step: step})
	}
}

// tell hands the driver one event, and says whether it did: the run's end or the request's own
// end lets it go unsent.
func (s *stepModelsStandin) tell(asked context.Context, event stepModelsEvent) bool {
	select {
	case s.events <- event:
		return true
	case <-s.stopped:
		return false
	case <-asked.Done():
		return false
	}
}

// stepModelsAnswer writes one response to w and answers its id.
type stepModelsAnswer func(w http.ResponseWriter) string

// text is one answer of one text block that ends the turn.
func (s *stepModelsStandin) text(text string) stepModelsAnswer {
	return s.blocks("end_turn", anthropicText(text))
}

// blocks is one answer of blocks, which stops for stop, under the model the stand-in serves.
func (s *stepModelsStandin) blocks(stop string, blocks ...anthropicBlock) stepModelsAnswer {
	return func(w http.ResponseWriter) string { return streamAnthropicBlocks(w, s.served, stop, blocks...) }
}

// answerAt is the answer of the agent's own conversation at place in a step of script, place 0
// for a turn that is no step's; whether it waits for the step's handoff record first; and whether
// it is the text that ends the turn of a step another fact ends.
func (s *stepModelsStandin) answerAt(offered []string, script stepModelsStep, place int) (answer stepModelsAnswer, hold, stoppedTurnEnded bool) {
	switch {
	case place == 1:
		// One response of two content blocks under one id: what the turn is about to do, and the
		// shell call it makes. A reader that counts blocks rather than responses counts this twice.
		return s.blocks("tool_use",
			anthropicText("I will run the scratch check of this step."),
			stepModelsToolUse(s.name+"-shell", s.toolName(offered, 0), s.input(map[string]any{"command": "true"}))), false, false
	case place == 2:
		return s.blocks("tool_use",
			stepModelsToolUse(s.name+"-subagent", s.toolName(offered, 1), s.input(s.subagentInput()))), false, false
	case place == 3 && script.ended:
		completion := map[string]any{"op": "handoff_complete", "summary": "The step is done, and this reports it."}
		if script.verdict != "" {
			completion["verdict"] = script.verdict
		}
		return s.blocks("tool_use", stepModelsToolUse(s.name+"-legion", s.toolName(offered, 2), s.input(completion))), true, false
	default:
		return s.text(stepModelsWaiting), false, place == 3
	}
}

// input is a tool call's input as the harness takes it: on Oh My Pi with the one-line intent
// every tool of it requires (busy_relaunch_live_test.go, callTool).
func (s *stepModelsStandin) input(input map[string]any) map[string]any {
	if s.harness == "claude" {
		return input
	}
	called := map[string]any{"i": "Running the chain's step"}
	for key, value := range input {
		called[key] = value
	}
	return called
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

// place is the step this response of the agent's own conversation belongs to, its script and its
// place in that step, 1 to 4, or step -1 and place 0 for a turn that is no step's. A prompt that
// carries the stand-in's marker starts the next step of the script; any other prompt, a reminder
// or a notice among them, is a turn of its own, answered with the WAITING text. A request whose
// newest message is a tool's result goes on with the turn it belongs to, and a step's turn ends
// with the text that ends it.
func (s *stepModelsStandin) place(turn stepModelsTurn) (int, stepModelsStep, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !turn.continues {
		if !strings.Contains(turn.text, s.marker) || s.next >= len(s.steps) {
			s.inTurn = false
			return -1, stepModelsStep{}, 0
		}
		s.step, s.inStep, s.inTurn = s.next, 0, true
		s.next++
	} else if !s.inTurn {
		return -1, stepModelsStep{}, 0
	}
	s.inStep++
	script := s.steps[s.step]
	if s.inStep >= 4 || (s.inStep == 3 && !script.ended) {
		s.inTurn = false
	}
	return s.step, script, s.inStep
}

// quietFor says whether this stand-in holds no request and has answered none for at least d.
func (s *stepModelsStandin) quietFor(d time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.asking == 0 && time.Since(s.answered) >= d
}

// servedList is every response this stand-in answered, oldest first.
func (s *stepModelsStandin) servedList() []servedResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]servedResponse(nil), s.list...)
}

// where says how far this stand-in's script has gone, and what asked for its last answers, for a
// failure that waited on it.
func (s *stepModelsStandin) where() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var last []string
	for _, served := range s.list[max(0, len(s.list)-3):] {
		last = append(last, fmt.Sprintf("{place %d of step %d, subagent %t, side %t, asked %q: %s}",
			served.Place, served.Step, served.Subagent, served.Side, served.Asked, served.Turn))
	}
	return fmt.Sprintf("%s started %d of %d steps, served %d responses, last at place %d of step %d (turn open %t), the last answered %s",
		s.token, s.next, len(s.steps), len(s.list), s.inStep, s.step+1, s.inTurn, strings.Join(last, " "))
}

// mainResponsesBetween is how many responses of the agent's own conversation this stand-in served
// between two times, which is what a step's record must count: a subagent's response is in none of
// them. The bounds are a step's two read times, and a response counts where it ended after the
// first and at or before the second.
func (s *stepModelsStandin) mainResponsesBetween(after, until time.Time) int {
	count := 0
	for _, served := range s.servedList() {
		if served.main() && served.EndedAt.After(after) && !served.EndedAt.After(until) {
			count++
		}
	}
	return count
}

// stepModelsTurn is one Messages request as a chain's stand-in reads it: the model it asks for,
// its system prompt, the text of its first user message, the tools it offers, the text of what
// asks for this answer, whether that carries a tool's result, and whether the request is the
// plugin's own side question (isSideQuestion).
type stepModelsTurn struct {
	model, system, firstUser, text string
	tools                          []string
	continues, side                bool
	// shape says what asked for this answer: the roles of the messages after the last assistant
	// message, the type of each of their blocks, and the start of their text.
	shape string
}

// readStepModelsTurn reads a Messages request's body. What asks for this answer is every message
// after the request's last assistant message (turnOf), as completionStandin reads a turn: the
// host and the plugins may put messages of their own beside a task or a tool's result, a
// reminder or a background job's notice among them. A turn whose blocks carry a tool's result
// goes on with the turn it belongs to, whatever text the host put beside the result.
func readStepModelsTurn(body []byte) stepModelsTurn {
	turn := stepModelsTurn{side: isSideQuestion(string(body))}
	var asked struct {
		Model  string          `json:"model"`
		System json.RawMessage `json:"system"`
	}
	var request messagesRequest
	if json.Unmarshal(body, &asked) != nil || json.Unmarshal(body, &request) != nil || len(request.Messages) == 0 {
		return turn
	}
	turn.model, turn.system = asked.Model, string(asked.System)
	for _, tool := range request.Tools {
		turn.tools = append(turn.tools, tool.Name)
	}
	for _, message := range request.Messages {
		if message.Role == "user" {
			turn.firstUser, _ = contentText(message.Content)
			break
		}
	}
	blocks := turnOf(request)
	turn.text = blocksText(blocks)
	turn.continues = slices.ContainsFunc(blocks, func(block requestBlock) bool { return block.Type == "tool_result" })
	turn.shape = stepModelsShape(request, blocks, turn.text)
	return turn
}

// stepModelsShape is what asked for an answer, as a failure names it: the roles of every message
// after the request's last assistant message, each of their blocks' types, and the first 160
// characters of their text.
func stepModelsShape(request messagesRequest, blocks []requestBlock, text string) string {
	var roles []string
	for i := len(request.Messages) - 1; i >= 0 && request.Messages[i].Role != "assistant"; i-- {
		roles = append([]string{request.Messages[i].Role}, roles...)
	}
	types := make([]string, 0, len(blocks))
	for _, block := range blocks {
		types = append(types, block.Type)
	}
	if len(text) > 160 {
		text = text[:160] + "…"
	}
	return fmt.Sprintf("%d messages %v after the last answer, blocks %v, text %q", len(roles), roles, types, text)
}

// stepModelsToolUse is one tool_use block (anthropicToolUse). Its id carries a number of its own,
// so each call of a pane's steps has an id no other call has, as a model's ids are.
func stepModelsToolUse(id, name string, input map[string]any) anthropicBlock {
	return anthropicToolUse("toolu_"+id+"_"+strconv.FormatUint(stepModelsToolCalls.Add(1), 10), name, input)
}

// stepModelsToolCalls numbers each tool call a chain's stand-in makes, so each call's id is its
// own across the steps of one pane, as a model's ids are.
var stepModelsToolCalls atomic.Uint64

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
			anthropicText("what I am about to do"),
			anthropicToolUse("toolu_standin", "bash", map[string]any{"command": "true"}))

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

// stepModelsSpareProvider is the Oh My Pi provider of every claim this run drives no pane for,
// which answers every turn with text alone.
const stepModelsSpareProvider = "stepmodelsspare"

// stepModelsClaudeModel is what each Claude pane is told to ask for (ANTHROPIC_MODEL) and what its
// record's set model must therefore be, under the provider name a record spells it with.
// stepModelsCaptureServed is what the capture that measures what a transcript holds of a set
// model that never answered serves in its place.
const (
	stepModelsClaudeModel    = "claude-model"
	stepModelsClaudeSetModel = "anthropic/" + stepModelsClaudeModel
	stepModelsCaptureServed  = "claude-served-model"
)

// stepModelsEvidenceEnv names the directory the run writes its evidence to, which the CI job
// uploads whatever the result. Unset, the run writes under its own temporary directory, so a
// developer's run leaves nothing behind.
const stepModelsEvidenceEnv = "LEGION_E2E_STEP_MODELS_EVIDENCE"

// stepModelsReadLine is the daemon's line for each read a step's start or end used. The
// comparator joins it to each printed record, and checks 2 to 5 read the step's two read times
// from it.
const stepModelsReadLine = "workflow: a step used a read of its agent's calls"

// The endedBy of a step's end line: its own agent's completion, and the owner's stop of its task.
const (
	stepModelsEndedByAgent = "agent"
	stepModelsEndedByStop  = "TaskStopped"
)

// stepModelsTask is the one task of each issue's plan (planOf), whose two steps are the
// implementer's and the tester's.
const stepModelsTask = "a"

// stepModelsChainBound is how long the chain's turns are given, from the first gate's opening to
// the second stop: nine steps of about fifteen seconds of answers each, the launches of the four
// panes, and every tool a turn runs.
const stepModelsChainBound = 25 * time.Minute

// stepModelsStall is how long the chain's turns may go with no stand-in telling the driver
// anything: longer than a pane's launch (paneReady) and a step's turn together, so only a chain
// that stopped moving runs it out.
const stepModelsStall = 8 * time.Minute

// stepModelsWait bounds each other wait of the run on the daemon or on Dispatch.
const stepModelsWait = 3 * time.Minute

// stepModelsTitles is each issue's title. Dispatch refuses a new issue whose title is a near copy
// of an open one's (POSSIBLE_DUPLICATE), so the two share no word that counts.
var stepModelsTitles = map[string]string{
	"A": "Alpha widget counts its clicks",
	"B": "Second gadget drops stale caches",
}

// stepModelsRuns is how many records each issue's read holds at the chain's end: on A, three of
// its implementer and two of its tester; on B, two of each.
var stepModelsRuns = map[string]int{"A": 5, "B": 4}

// stepModelsClaudeQuiet is what every Claude Code process of the run carries so that it reaches
// nothing outside the run: no update, no telemetry and no error report, as the repository's own
// run of the real Claude Code turns them off (packages/claude-envoy/tests/
// legion-phase-hook-claude.test.ts). Its model calls are left as the pinned build makes them, and
// each reaches the pane's own stand-in.
var stepModelsClaudeQuiet = []string{"DISABLE_AUTOUPDATER=1", "DISABLE_TELEMETRY=1", "DISABLE_ERROR_REPORTING=1"}

// stepModelsIssue is one of the two issues the chain drives: its key as Dispatch made it, its
// title, its spec document, the rank and sequence the seed copies, the commit every record names,
// the panes' shared workspace, its pull request, the three claims of its roles, and what
// `dispatch steps` printed of it at the chain's end.
type stepModelsIssue struct {
	name                           string
	key, title, spec, rank         string
	lastSeq                        int64
	head, workspace                string
	number                         int
	implementer, tester, architect claim.Token
	// printed is what `dispatch steps --issue <key>` printed at the chain's end, lines that output
	// parsed, and readErr why the last read of it held no whole record.
	printed string
	lines   []stepLine
	readErr error
}

// stepModelsLive is the run: the daemon booted over the local runtime, the real Dispatch this
// branch builds, two issues whose implementers are real Oh My Pi panes and whose testers are real
// Claude Code panes, and the evidence the job uploads.
type stepModelsLive struct {
	liveAgentProof
	// top is the test itself, whose cleanups outlive every check: what the run holds until it ends
	// is made under it, the architects' connections among them.
	top       *testing.T
	dispatch  *handoffDispatch
	tokens    *workflowTokenRecorder
	githubAPI string
	// root is the checkout this driver was built from; work the run's own temporary directory;
	// home and profile the Oh My Pi home and profile every pane runs under; cliHome the home of
	// the person's `dispatch`.
	root, work, home, profile, cliHome string
	// bun, claude and pluginDir are what a Claude Code pane runs: the bun its MCP server runs on,
	// the pinned Claude Code, and the claude-envoy plugin this run built from the checkout.
	bun, claude, pluginDir string
	// evidence is the directory the run writes its records, logs, transcripts and captures to.
	evidence string
	// reader is the personal token Dispatch minted for the project's person, which every read of
	// the CLI carries.
	reader string
	issues []*stepModelsIssue
	// panes is each driven claim's own stand-in, and spare the one every other claim talks to.
	panes map[claim.Token]*stepModelsStandin
	spare *paneStandin
	// events is what the stand-ins tell the driver, and done closes when the run ends.
	events chan stepModelsEvent
	done   chan struct{}
	// overlays is each Oh My Pi claim's PI_CONFIG_FILES, and spareOverlays every other Oh My Pi
	// claim's. A Claude Code claim reads none: its model comes from ANTHROPIC_MODEL in its pane's
	// own environment (config.checkRoleConfigFiles refuses an overlay for such a role).
	overlays      map[claim.Token][]string
	spareOverlays []string
	// gitRunner runs every git and jj command the driver makes.
	gitRunner fixturegit.Runner
	// architects is each issue's architect, the driver's own session.
	architects map[claim.Token]*stepModelsArchitect
	// seen is each driven claim as the operator's listing last showed it to the driver.
	seen map[claim.Token]api.OperatorClaim
	// chained says the chain's turns reached both stops and each issue's read holds its records.
	chained bool
	// compared is the comparator's run over both issues, made once.
	compared *stepModelsCompared
	// captures is each capture attempt that ran, by its name.
	captures map[string]*stepModelsCapture
	// failedAt is the assertion the running check failed at, which run prints.
	failedAt string
}

// TestStepModelsLive runs the chain over two issues, judges the records with the comparator,
// captures what a real Claude Code session writes, and prints each check between its `== name`
// and `ok name` lines. It writes passed.txt to its evidence only where it neither failed nor
// skipped, which the CI job requires of it.
func TestStepModelsLive(t *testing.T) {
	t.Parallel()
	r := newStepModelsLive(t)
	r.run("the-chain", r.driveChain)
	r.run("the-captures", r.runCaptures)
	r.run("claude-pane-registers", r.claudePaneRegisters)
	r.run("omp-agent-ended-recorded", func() { r.recordHolds("A", claim.RoleImplementer, 2, stepModelsEndedByAgent) })
	r.run("claude-agent-ended-recorded", func() { r.recordHolds("A", claim.RoleTester, 2, stepModelsEndedByAgent) })
	r.run("omp-stopped-recorded", func() { r.recordHolds("A", claim.RoleImplementer, 3, stepModelsEndedByStop) })
	r.run("claude-stopped-recorded", func() { r.recordHolds("B", claim.RoleTester, 2, stepModelsEndedByStop) })
	r.run("live-at-both-reads-verdict", r.liveAtBothReadsVerdict)
	r.run("c-real-verdict", r.cRealVerdict)
	r.run("control-lower-one", r.controlLowerOne)
	r.run("control-wrong-settings", r.controlWrongSettings)
	r.run("claude-capture", r.claudeCapture)
	r.run("set-model-held", r.setModelHeld)
	// A skip ends the test inside newStepModelsLive, so a run that skipped never reaches this line.
	if !t.Failed() {
		r.writeEvidence("passed.txt", []byte("TestStepModelsLive passed every check at "+time.Now().UTC().Format(time.RFC3339)+"\n"))
	}
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

// issueOf is the issue of a driven claim, nil for none.
func (r *stepModelsLive) issueOf(token claim.Token) *stepModelsIssue {
	for _, issue := range r.issues {
		if token == issue.implementer || token == issue.tester || token == issue.architect {
			return issue
		}
	}
	return nil
}

// isArchitect says whether token is an architect of the run, which the driver stands in for.
func (r *stepModelsLive) isArchitect(token claim.Token) bool {
	issue := r.issueOf(token)
	return issue != nil && token == issue.architect
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

// setModelOf is the model the run's settings give a driven pane's claim, as a record spells it:
// the one model an implementer's overlay names, and a tester's ANTHROPIC_MODEL under the provider
// name Claude Code's models carry.
func (r *stepModelsLive) setModelOf(pane *stepModelsStandin) string {
	if pane.harness == "claude" {
		return stepModelsClaudeSetModel
	}
	return standinModel(pane.name)
}

// evidenceFileAt is the path of one file of the run's evidence.
func (r *stepModelsLive) evidenceFileAt(name string) string {
	return filepath.Join(r.evidence, name)
}

// writeEvidence writes one file of the run's evidence, which the job uploads whatever the result.
// A failed write fails the test and stops nothing, since it may run as a check ends.
func (r *stepModelsLive) writeEvidence(name string, body []byte) {
	r.t.Helper()
	if err := os.WriteFile(r.evidenceFileAt(name), body, 0o600); err != nil {
		r.t.Errorf("write the evidence file %s: %v", name, err)
	}
}

// writeEvidenceJSON writes one file of the run's evidence as indented JSON.
func (r *stepModelsLive) writeEvidenceJSON(name string, value any) {
	r.t.Helper()
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		r.t.Errorf("encode the evidence file %s: %v", name, err)
		return
	}
	r.writeEvidence(name, append(encoded, '\n'))
}

// newStepModelsLive readies everything the panes need and boots the daemon: the real Dispatch
// with the project and the two issues; the `legion` binary and the two Oh My Pi plugins built from
// this checkout; the claude-envoy plugin built from it too; the stand-ins and an Oh My Pi profile
// routed to them; the local runtime with its Claude Code launch; each issue's jj workspace at its
// implementer's pushed head; GitHub's stand-in at those heads; the store seeded with each issue
// at admitted and its architect mid-launch; the daemon booted with both harnesses resolved; and
// each architect met as the driver's own session.
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
	proof.cfg.AdmissionCap = 4
	// The testers run on Claude Code and the implementers and architects on Oh My Pi, so one run
	// drives both real harnesses behind the real shim.
	proof.cfg.WorkerBackends = map[claim.Role]runtime.Backend{claim.RoleTester: runtime.BackendClaude}
	if proof.project, err = claim.ProjectToken(proof.cfg.Project); err != nil {
		t.Fatal(err)
	}

	r := &stepModelsLive{
		liveAgentProof: liveAgentProof{liveProof: proof, omp: omp},
		top:            t,
		tokens:         &workflowTokenRecorder{},
		bun:            bun,
		claude:         claudeCode,
		panes:          map[claim.Token]*stepModelsStandin{},
		events:         make(chan stepModelsEvent, 16),
		done:           make(chan struct{}),
		overlays:       map[claim.Token][]string{},
		architects:     map[claim.Token]*stepModelsArchitect{},
		seen:           map[claim.Token]api.OperatorClaim{},
	}
	r.root = proofCheckoutRoot(t)
	r.work = t.TempDir()
	r.cliHome = filepath.Join(r.work, "person")
	if err := os.MkdirAll(r.cliHome, 0o700); err != nil {
		t.Fatal(err)
	}
	r.evidence = os.Getenv(stepModelsEvidenceEnv)
	if r.evidence == "" {
		r.evidence = filepath.Join(r.work, "step-models-evidence")
	}
	if err := os.MkdirAll(r.evidence, 0o700); err != nil {
		t.Fatalf("make the evidence directory %s: %v", r.evidence, err)
	}
	r.note("the run writes its evidence to %s", r.evidence)
	for _, binary := range []string{omp, claudeCode} {
		version, err := exec.Command(binary, "--version").Output()
		if err != nil {
			t.Fatalf("read the version of %s: %v", binary, err)
		}
		r.note("%s at %s", strings.TrimSpace(string(version)), binary)
	}

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

	r.setUpDispatch()
	r.mintReader()
	r.serveStandins()
	r.writeOmpProfile()
	r.writeOverlays()
	r.startRuntime()
	r.gitRunner = fixturegit.Isolated(t, r.home)
	for _, issue := range r.issues {
		r.initWorkspace(issue)
	}
	r.serveGitHub()
	r.seed()
	r.boot()
	for _, issue := range r.issues {
		r.architects[issue.architect] = r.meetArchitect(issue)
	}
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
// and creates the two issues, each with a spec, in progress and carrying the legion label, as a
// running daemon leaves one. It fills in each issue's key, spec, rank and sequence, its pull
// request number and its three claim tokens.
func (r *stepModelsLive) setUpDispatch() {
	t := r.t
	t.Helper()
	r.dispatch.user(http.MethodPost, "/api/v1/projects",
		map[string]string{"key": r.cfg.Project, "name": "Step models chain"}, http.StatusCreated, nil)
	r.dispatch.user(http.MethodPut, "/api/v1/projects/"+r.cfg.Project+"/default-repository",
		map[string]any{"repo": liveRepo}, http.StatusOK, nil)
	for _, name := range []string{"A", "B"} {
		issue := &stepModelsIssue{name: name, title: stepModelsTitles[name]}
		var created struct {
			Key               string `json:"key"`
			Rank              string `json:"rank"`
			LastSeq           int64  `json:"last_seq"`
			PrimaryArtifactID string `json:"primary_artifact_id"`
		}
		r.dispatch.user(http.MethodPost, "/api/v1/issues", map[string]string{
			"project": r.cfg.Project, "title": issue.title, "spec": "The issue's change, as its owner states it.\n",
		}, http.StatusCreated, &created)
		if created.Key == "" || created.PrimaryArtifactID == "" {
			t.Fatalf("Dispatch created the issue %q as %q with the spec %q; want a key and a spec", issue.title, created.Key, created.PrimaryArtifactID)
		}
		r.dispatch.user(http.MethodPatch, "/api/v1/issues/"+created.Key,
			map[string]any{"status": "in_progress", "labels": []string{"legion"}}, http.StatusOK, &created)
		issue.key, issue.spec, issue.rank, issue.lastSeq = created.Key, created.PrimaryArtifactID, created.Rank, created.LastSeq
		number, err := strconv.ParseInt(randomSuffix(t)[:6], 16, 64)
		if err != nil {
			t.Fatal(err)
		}
		issue.number = int(number) + 1
		issue.implementer = r.token(issue.key, claim.RoleImplementer)
		issue.tester = r.token(issue.key, claim.RoleTester)
		issue.architect = r.token(issue.key, claim.RoleArchitect)
		r.issues = append(r.issues, issue)
		r.note("issue %s: %s, %q, spec %s, pull request #%d", name, issue.key, issue.title, issue.spec, issue.number)
	}
}

// mintReader has Dispatch mint a personal token for the person who set the run's project up
// (POST /api/v1/me/agent-tokens), which every read of the CLI then carries.
func (r *stepModelsLive) mintReader() {
	r.t.Helper()
	var minted struct {
		Token string `json:"token"`
	}
	r.dispatch.user(http.MethodPost, "/api/v1/me/agent-tokens", map[string]string{"name": "step-models-reader"}, http.StatusCreated, &minted)
	if minted.Token == "" {
		r.t.Fatal("Dispatch minted the person a personal token with no plaintext")
	}
	r.reader = minted.Token
}

// personsShell is the shell of a person running dispatch by hand: a session of their own, the
// run's Dispatch and the personal token Dispatch minted for them, under the person's own home.
func (r *stepModelsLive) personsShell() cliShell {
	return cliShell{t: r.t, dir: r.cliHome, session: "ses_step_models_reader", base: r.dispatch.base, token: r.reader}
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
		implementer := "smimplementer" + lower
		r.panes[issue.implementer] = newStepModelsStandin(t, implementer, "omp", implementer+standinModelID, implementer+standinModelID,
			"Phase: "+string(phase.Implementing)+".", issue.implementer, script[0], r.events, r.done)
		r.panes[issue.tester] = newStepModelsStandin(t, "smtester"+lower, "claude", stepModelsClaudeModel, stepModelsClaudeModel,
			"Phase: "+string(phase.Testing)+".", issue.tester, script[1], r.events, r.done)
	}
	r.spare = newPaneStandin(t, stepModelsSpareProvider)
	// Registered after every stand-in's own close, so it runs first: an answer held for a record
	// the driver will never write is let go, and no server's close waits on it.
	t.Cleanup(func() { close(r.done) })
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
// model is its pane's ANTHROPIC_MODEL (claudeStandinEnvironment).
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
// stepModelsStandinKey the literal this run's Claude processes carry in it. No endpoint of this
// run checks it: every one of them is a stand-in of the test's own, and CI holds no model
// credential. The two are joined at run time rather than written as one literal, so the source
// carries nothing shaped like a key assignment.
const (
	claudeKeyVariable    = "ANTHROPIC_API_KEY"
	stepModelsStandinKey = "a-literal-value-no-endpoint-checks"
)

// claudeStandinEnvironment is what a Claude Code process of the run carries to reach the stand-in
// at url: the endpoint, a stand-in key, the one model it asks for, and stepModelsClaudeQuiet. A
// Claude Code role reads no Oh My Pi overlay, so this is where its set model is decided. Its
// stand-in serves that same model, as every endpoint of the chain serves the model asked for. Each
// tester has a stand-in of its own, so each record's counts are held to its own claim's answers.
func claudeStandinEnvironment(url string) []string {
	return append([]string{
		"ANTHROPIC_BASE_URL=" + url,
		claudeKeyVariable + "=" + stepModelsStandinKey,
		"ANTHROPIC_MODEL=" + stepModelsClaudeModel,
	}, stepModelsClaudeQuiet...)
}

// startRuntime builds the local runtime every pane of the run is a child of: the shim and Oh My
// Pi this run built and was given, the Claude Code launch the daemon's boot resolves too, each Oh
// My Pi claim's overlays, each Claude claim's own endpoint, and the architects, which the driver
// stands in for.
func (r *stepModelsLive) startRuntime() {
	r.rt = newLocalRuntime(r.t, r.cfg.StateDir, r.legion, r.omp, r.home, r.profile)
	r.rt.daemonURL, r.rt.envoyURL, r.rt.natsURLs = r.cfg.DaemonURL, newPaneListener(r.t), r.cfg.NatsURLs
	r.rt.dispatchURL, r.rt.dispatchTokenFile = r.cfg.DispatchURL, r.cfg.DispatchTokenFile
	r.rt.claude = tmux.ClaudeLaunch{Binary: r.claude, PluginDir: r.pluginDir, Bun: r.bun}
	r.rt.overlays = func(token claim.Token) []string {
		if overlays, driven := r.overlays[token]; driven {
			return overlays
		}
		if pane, driven := r.panes[token]; driven && pane.harness == "claude" {
			return nil
		}
		return r.spareOverlays
	}
	r.rt.claimEnvironment = func(token claim.Token) []string {
		if pane, driven := r.panes[token]; driven && pane.harness == "claude" {
			return claudeStandinEnvironment(pane.url)
		}
		return nil
	}
	r.rt.standsIn = r.isArchitect
}

// initWorkspace makes the issue's panes' working directory its jj workspace as its implementer
// leaves a pushed round, as late_completion_live_test.go's initWorkspace makes it: a colocated
// repository whose `@-` is the implementer's commit, on `legion/<issue>` of an origin of the run's
// own, with `legion/<issue>@origin` at that commit. That commit is the pull request's head, which
// every record names and each implementer's completion is held to. Every command runs under
// fixturegit.Isolated of the panes' HOME, so the panes' jj reads the same configuration.
func (r *stepModelsLive) initWorkspace(issue *stepModelsIssue) {
	t := r.t
	t.Helper()
	issue.workspace = r.rt.workDir(issue.key)
	if err := os.MkdirAll(issue.workspace, 0o700); err != nil {
		t.Fatalf("make the working directory of %s's panes %s: %v", issue.key, issue.workspace, err)
	}
	origin := filepath.Join(r.work, "origin-"+strings.ToLower(issue.key)+".git")
	branch := "legion/" + issue.key
	r.gitRunner.Run(issue.workspace, fixturegit.JJInit("", true)...)
	writeProofFile(t, filepath.Join(issue.workspace, "implementation.txt"), "The change "+issue.key+" asks for.\n")
	r.workspaceJJ(issue, "commit", "-m", "implement "+issue.key, "implementation.txt")
	r.workspaceJJ(issue, "bookmark", "set", branch, "-r", "@-")
	issue.head = r.workspaceJJ(issue, "log", "-r", "@-", "--no-graph", "-T", "commit_id")
	ref := "refs/heads/" + branch
	r.gitRunner.Run(r.work, fixturegit.InitBare(origin, "")...)
	r.gitRunner.Run(r.work, "git", "--git-dir", origin, "fetch", "--quiet", r.workspaceJJ(issue, "git", "root"), ref+":"+ref)
	r.workspaceJJ(issue, "git", "remote", "add", "origin", origin)
	r.workspaceJJ(issue, "git", "fetch", "--remote", "origin")
	pushed := r.workspaceJJ(issue, "log", "-r", fmt.Sprintf("remote_bookmarks(exact:%q, exact:\"origin\")", branch), "--no-graph", "-T", "commit_id")
	if pushed != issue.head {
		t.Fatalf("%s@origin is %q, want the implementer's commit %s", branch, pushed, issue.head)
	}
	r.note("%s's workspace %s stands on the implementer's commit %s, which %s@origin is", issue.key, issue.workspace, issue.head, branch)
}

// workspaceJJ runs jj in issue's workspace under the driver's runner and answers its trimmed
// stdout; a failure ends the test with its stderr.
func (r *stepModelsLive) workspaceJJ(issue *stepModelsIssue, args ...string) string {
	r.t.Helper()
	return strings.TrimSpace(r.gitRunner.Out(issue.workspace, append([]string{"jj"}, args...)...))
}

// stepModelsGitHub is GitHub's REST API as the run's daemon reads it: every repository with
// default branch main, and each issue's pull request open on its branch at the head its
// workspace pushed, clean, with no reviews, listed on the branch as the implementer's completion
// reads it (api's implementationPullRequest, which counts a listed pull request only where its
// head.ref is the branch). The GraphQL root knows no pull request, and every other route is a
// 404, so nothing reaches github.com. It is stepRecordsGitHub with a head of each pull request's
// own, since each issue's workspace pushed a commit of its own.
type stepModelsGitHub struct {
	pulls []stepModelsPull
}

// stepModelsPull is one pull request of the run: its number, the branch it is open on and its
// head.
type stepModelsPull struct {
	number       int
	branch, head string
}

// answer is the pull request as GitHub's REST API answers it.
func (p stepModelsPull) answer() string {
	return fmt.Sprintf(`{"number":%d,"state":"open","merged":false,"merge_commit_sha":%q,"mergeable":true,"mergeable_state":"clean",`+
		`"head":{"ref":%q,"sha":%q},"base":{"ref":"main"},"updated_at":"2026-11-24T00:00:00Z"}`, p.number, "merge-"+p.head, p.branch, p.head)
}

// serveGitHub starts the run's GitHub stand-in over each issue's pull request at its head.
func (r *stepModelsLive) serveGitHub() {
	g := &stepModelsGitHub{}
	for _, issue := range r.issues {
		g.pulls = append(g.pulls, stepModelsPull{number: issue.number, branch: "legion/" + issue.key, head: issue.head})
	}
	r.githubAPI = githubtest.Server(r.t, githubtest.Spec{Handle: g.ServeHTTP})
}

func (g *stepModelsGitHub) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	rest, ours := strings.CutPrefix(req.URL.Path, "/repos/"+liveRepo)
	number, reviews := strings.CutSuffix(strings.TrimPrefix(rest, "/pulls/"), "/reviews")
	var pull *stepModelsPull
	if n, err := strconv.Atoi(number); err == nil && strings.HasPrefix(rest, "/pulls/") {
		if at := slices.IndexFunc(g.pulls, func(p stepModelsPull) bool { return p.number == n }); at >= 0 {
			pull = &g.pulls[at]
		}
	}
	switch {
	case req.Method == http.MethodPost && req.URL.Path == "/graphql":
		_, _ = io.WriteString(w, `{"data":{"repository":{"pullRequest":null}}}`)
	case req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, "/repos/") && strings.Count(req.URL.Path, "/") == 3:
		parts := strings.Split(strings.TrimPrefix(req.URL.Path, "/repos/"), "/")
		_, _ = io.WriteString(w, `{"full_name":"`+parts[0]+"/"+parts[1]+`","default_branch":"main"}`)
	case req.Method == http.MethodGet && ours && rest == "/git/ref/heads/main":
		_, _ = io.WriteString(w, `{"ref":"refs/heads/main","object":{"sha":"base-main"}}`)
	case req.Method == http.MethodPost && ours && rest == "/git/refs":
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{}`)
	case req.Method == http.MethodGet && ours && rest == "/pulls":
		answers := make([]string, len(g.pulls))
		for i, p := range g.pulls {
			answers[i] = p.answer()
		}
		_, _ = fmt.Fprintf(w, "[%s]", strings.Join(answers, ","))
	case req.Method == http.MethodGet && ours && pull != nil && !reviews:
		_, _ = io.WriteString(w, pull.answer())
	case req.Method == http.MethodGet && ours && pull != nil && reviews:
		_, _ = io.WriteString(w, `[]`)
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	}
}

// seed records each issue as the daemon holds a root its architect has just begun, as
// owner_steps_rig_test.go's seedAdmitted records one: handed to Legion at admitted, its lifecycle
// under the workflow, no design gate registered, no pull request, and its architect's claim a
// launch a previous daemon began and never recorded, which the boot relaunches. So each issue's
// plan is given before its work starts, the one moment a first plan is taken (workflow's
// beforeItsWork), and the stop of its task finds a task to stop. Every role's prompt is an
// operator's short prompt file (writePrompt): nothing here reads a prompt.
func (r *stepModelsLive) seed() {
	t := r.t
	t.Helper()
	ctx := context.Background()
	for _, issue := range r.issues {
		if err := store.Transact(ctx, r.st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
			return r.records.PutIssue(ctx, tx, record.Issue{
				Key: issue.key, Repo: liveRepo, DefaultBranch: "main", GitHubRepo: liveRepo, Tree: issue.key, Project: r.cfg.Project,
				Title: issue.title, Phase: phase.Admitted, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress",
				Rank: issue.rank, LastDispatchSeq: issue.lastSeq, HandedOver: true,
			})
		}); err != nil {
			t.Fatalf("seed %s: %v", issue.key, err)
		}
		if _, err := r.st.OpenTreeLifecycle(ctx, r.project, issue.key, treelifecycle.AuthorityWorkflow); err != nil {
			t.Fatalf("open the lifecycle of %s: %v", issue.key, err)
		}
		if _, err := r.st.AdmitClaim(ctx, supervise.Claim{
			Token: issue.architect, Project: r.project, Tree: issue.key, Issue: issue.key, Role: claim.RoleArchitect,
			Generation: 1, State: supervise.StateLaunching, BootTokenHash: supervise.HashBootToken("interrupted-" + randomSuffix(t)),
		}); err != nil {
			t.Fatalf("record the architect's unfinished launch on %s: %v", issue.key, err)
		}
		for _, token := range []claim.Token{issue.architect, issue.implementer, issue.tester} {
			writePrompt(t, r.cfg, token)
		}
	}
}

// boot starts the whole daemon over the local runtime. Its one log goes twice: as text where the
// base's live proofs write theirs (liveProof.log, which LEGION_E2E_DAEMON_LOG names), and as JSON
// into daemon.jsonl of the evidence, the log the comparator reads, as `legion start` writes its
// log (cmd/legion's start).
func (r *stepModelsLive) boot() {
	t := r.t
	t.Helper()
	jsonLog := slog.NewJSONHandler(evidenceFile(t, r.evidenceFileAt("daemon.jsonl")), &slog.HandlerOptions{Level: slog.LevelDebug})
	r.daemon = newRecordingLog(slog.New(stepModelsLogs{r.log().Handler(), jsonLog}))
	r.d = startDaemonWithin(t, r.cfg, r.overrides(), slog.New(r.daemon), 2*time.Minute)
	r.note("Dispatch %s (database of its own), project %s with default repository %s, issues %s and %s; daemon at dispatch_url %s",
		r.dispatch.base, r.cfg.Project, liveRepo, r.issues[0].key, r.issues[1].key, r.cfg.DispatchURL)
}

// overrides is the daemon this run boots: the local runtime over the real worker stream, the API
// listener the fixture holds, the workflow's App tokens stood in for, GitHub's stand-in, and an
// environment that names the pinned Claude Code, the bun its MCP server runs on and the
// claude-envoy build, which the boot resolves its Claude Code launch from (resolveClaude). That
// environment is the daemon's alone, not the process's.
func (r *stepModelsLive) overrides() overrides {
	return overrides{
		runtime: func(_ context.Context, listener *stream.Listener, address string, _ appauth.Source,
			_ func(context.Context, string) (ghrepo.Repository, string, error), _ *store.Store, _ func() []ghrepo.Repository) (runtime.Runtime, error) {
			r.rt.conns, r.rt.stream = listener, address
			return r.rt, nil
		},
		listen:         heldListen,
		workflowTokens: r.tokens,
		githubAPI:      r.githubAPI,
		environ: append(os.Environ(),
			toolEnv("claude")+"="+r.claude, toolEnv("bun")+"="+r.bun, claudePluginDirEnv+"="+r.pluginDir),
	}
}

// stepModelsLogs is one log written to each of its handlers.
type stepModelsLogs []slog.Handler

func (h stepModelsLogs) Enabled(ctx context.Context, level slog.Level) bool {
	return slices.ContainsFunc(h, func(handler slog.Handler) bool { return handler.Enabled(ctx, level) })
}

func (h stepModelsLogs) Handle(ctx context.Context, entry slog.Record) error {
	var failed []error
	for _, handler := range h {
		if handler.Enabled(ctx, entry.Level) {
			failed = append(failed, handler.Handle(ctx, entry.Clone()))
		}
	}
	return errors.Join(failed...)
}

func (h stepModelsLogs) WithAttrs(attrs []slog.Attr) slog.Handler {
	derived := make(stepModelsLogs, len(h))
	for i, handler := range h {
		derived[i] = handler.WithAttrs(attrs)
	}
	return derived
}

func (h stepModelsLogs) WithGroup(name string) slog.Handler {
	derived := make(stepModelsLogs, len(h))
	for i, handler := range h {
		derived[i] = handler.WithGroup(name)
	}
	return derived
}

// ---- the architects: the driver's own sessions on the worker stream -------------------------------

// stepModelsArchitect is one issue's architect, the driver's own session on the worker stream:
// the package's own shim (dialShim) over the boot token the daemon's launch of the claim carries,
// registered over the daemon's own route as a pane's plugin registers. The driver mints its
// grants with the registration's secret, which gives the issue its plan and stops task a: the
// daemon mints that secret at the registration (api's claims.go) and keeps only its hash, so no
// file a pane holds can mint one. It works each task the daemon sends it in a turn of its own that
// ends at once, as stepClaimant's workAlone does for the walks' architects: a turn of an
// architect's task that nothing ended is what the daemon reads as the issue's working copy in
// use, and the issue's next phase would wait on it. No check reads its record.
type stepModelsArchitect struct {
	issue           *stepModelsIssue
	session, secret string
	sh              *shim
}

// meetArchitect waits for the daemon's launch of issue's architect, which the boot makes for the
// seeded unfinished launch, and registers it as the driver's own session, ready. Its connection
// is the test's own, so it lasts until the run ends.
func (r *stepModelsLive) meetArchitect(issue *stepModelsIssue) *stepModelsArchitect {
	t := r.top
	t.Helper()
	var spec runtime.SpawnSpec
	r.await("the daemon's launch of "+string(issue.architect), stepModelsWait, func() bool {
		launches := r.rt.LaunchesOf(issue.architect)
		if len(launches) == 0 {
			return false
		}
		spec = launches[len(launches)-1]
		return true
	})
	a := &stepModelsArchitect{issue: issue, session: "ses_architect_" + strings.ToLower(issue.name) + "_" + randomSuffix(t)[:6]}
	// A session file that exists, so a relaunch that resumes it is not refused for a missing one.
	sessionFile := filepath.Join(r.work, "architect-sessions", a.session+".jsonl")
	if err := os.MkdirAll(filepath.Dir(sessionFile), 0o700); err != nil {
		t.Fatal(err)
	}
	writeProofFile(t, sessionFile, "")
	a.sh = dialShim(t, r.rt.stream, spec.BootToken)
	status, body := r.d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: spec.BootToken, SessionID: a.session, OmpSessionFile: sessionFile, AgentID: a.session, PluginContract: api.DaemonAPIVersion,
	}, false)
	if status != http.StatusOK {
		t.Fatalf("register %s = %d %s, want 200", issue.architect, status, body)
	}
	var registered claim.RegisterResponse
	if err := json.Unmarshal(body, &registered); err != nil {
		t.Fatalf("decode the registration of %s: %v", issue.architect, err)
	}
	a.secret = registered.Secret
	a.sh.serve(a.serve)
	if status, body := r.d.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: issue.architect, SessionID: a.session, Secret: a.secret, Generation: spec.Generation,
	}, false); status != http.StatusNoContent {
		t.Fatalf("ready %s = %d %s, want 204", issue.architect, status, body)
	}
	r.note("%s's architect %s is the driver's own session %s, registered and ready", issue.key, issue.architect, a.session)
	return a
}

// serve answers every frame the daemon sends the architect's connection until it closes: the
// protocol's negotiation, the state probe, and each task, worked in a turn that ends at once.
func (a *stepModelsArchitect) serve() {
	reply := func(frame shimwire.Frame) { _ = a.sh.writer.WriteFrame(frame) }
	for {
		line, err := a.sh.lines.ReadBytes('\n')
		if err != nil {
			return
		}
		frame, err := shimwire.Decode(line)
		if err != nil {
			continue
		}
		switch f := frame.(type) {
		case shimwire.NegotiateProtocol:
			reply(shimwire.Response{ID: f.ID, Command: shimwire.TypeNegotiateProtocol, Success: true})
		case shimwire.GetState:
			streaming := false
			data, _ := json.Marshal(shimwire.StateData{IsStreaming: &streaming})
			reply(shimwire.Response{ID: f.ID, Command: shimwire.TypeGetState, Success: true, Data: data})
		case shimwire.Prompt:
			reply(shimwire.Response{ID: f.ID, Command: shimwire.TypePrompt, Success: true})
			reply(shimwire.AgentStart{DeliveryID: f.DeliveryID})
			reply(shimwire.AgentEnd{})
			reply(shimwire.PromptResult{ID: f.ID, AgentInvoked: true})
		}
	}
}

// architectGrant is a grant minted now for issue's architect, as its `legion` tool mints one per
// operation.
func (r *stepModelsLive) architectGrant(issue *stepModelsIssue) string {
	r.t.Helper()
	a := r.architects[issue.architect]
	status, body := r.d.request(http.MethodPost, "/legion/v1/grants",
		api.GrantRequest{SessionID: a.session, Secret: a.secret, Tree: issue.key, Issue: issue.key}, false)
	if status != http.StatusOK {
		r.fails("the architect's grant", "the grant of %s = %d %s, want 200", issue.architect, status, body)
	}
	var granted api.GrantResponse
	if err := json.Unmarshal(body, &granted); err != nil {
		r.fails("the architect's grant", "decode the grant of %s: %v", issue.architect, err)
	}
	return granted.GrantID
}

// ---- the chain ------------------------------------------------------------------------------------

// driveChain gives each issue its plan of task a, opens each issue's gate, which starts its
// implementer, and drives the turns until each issue's task is stopped; then it waits for each
// issue to stand held for that task and for each record to reach Dispatch ended. Whatever it
// reached, it keeps the run's evidence.
func (r *stepModelsLive) driveChain() {
	defer r.keepEvidence()
	for _, issue := range r.issues {
		r.givePlan(issue)
	}
	for _, issue := range r.issues {
		r.openGate(issue)
	}
	r.driveTurns()
	for _, issue := range r.issues {
		r.await(issue.key+" held for its stopped task", stepModelsWait, func() bool {
			got := r.d.state().Issues[issue.key]
			return string(got.Phase) == string(phase.Held) && got.HoldReason == string(record.HoldTaskStopped)
		})
		r.note("read back: %s stands held for its stopped task", issue.key)
	}
	for _, issue := range r.issues {
		r.readSteps(issue)
	}
	r.chained = true
}

// givePlan uploads plan.md onto issue as its person, one task a whose steps are the implementer's
// and the tester's (planOf), and has its architect give version 1 of it (register_plan).
func (r *stepModelsLive) givePlan(issue *stepModelsIssue) {
	r.t.Helper()
	var created struct {
		Artifact struct {
			ID string `json:"id"`
		} `json:"artifact"`
	}
	r.dispatch.user(http.MethodPost, "/api/v1/issues/"+issue.key+"/artifacts",
		map[string]string{"name": "plan.md", "content": planOf("implementer", "tester")}, http.StatusCreated, &created)
	if created.Artifact.ID == "" {
		r.fails("the plan's upload", "Dispatch answered the upload of %s's plan.md with no artifact id", issue.key)
	}
	status, body := r.d.request(http.MethodPost, "/legion/v1/plans/register", api.PlanRegisterRequest{
		GrantID: r.architectGrant(issue), Issue: issue.key, ArtifactID: created.Artifact.ID, Version: 1}, false)
	if status != http.StatusOK {
		r.fails("the plan's registration", "register_plan of %s's plan = %d %s, want 200", issue.key, status, body)
	}
	r.note("%s's architect gave version 1 of plan.md %s: task %s, steps implementer and tester", issue.key, created.Artifact.ID, stepModelsTask)
}

// openGate is the person approving issue's spec at version 1 and its architect registering that
// version (register_gate), which opens the issue's gate; its plan's first step then starts its
// implementer.
func (r *stepModelsLive) openGate(issue *stepModelsIssue) {
	r.t.Helper()
	r.dispatch.user(http.MethodPost, "/api/v1/artifacts/"+issue.spec+"/reviews", map[string]any{"state": "approved"}, http.StatusCreated, nil)
	status, body := r.d.request(http.MethodPost, "/legion/v1/gates/register", map[string]any{
		"grantId": r.architectGrant(issue), "issue": issue.key, "artifactId": issue.spec, "version": 1}, false)
	if status != http.StatusOK {
		r.fails("the gate's registration", "register_gate of %s = %d %s, want 200", issue.key, status, body)
	}
	var stands string
	r.await(issue.key+" to leave admitted once its gate opened", stepModelsWait, func() bool {
		stands = string(r.d.state().Issues[issue.key].Phase)
		return stands != "" && stands != string(phase.Admitted)
	})
	if stands != string(phase.Implementing) {
		r.fails("the first step", "%s's gate opened and it stands in %s, want implementing, its plan's first step", issue.key, stands)
	}
	r.note("%s's gate opened: %s stands in %s", issue.key, issue.key, stands)
}

// driveTurns answers what the stand-ins tell the driver until each issue's task is stopped: a
// step its agent ends has its handoff record written as its claim's session just before the call
// that completes it, and the turn of a step another fact ends is followed by the stop of the
// issue's task. It fails naming where each stand-in stands once the whole chain's bound runs out,
// or once no stand-in has told it anything for stepModelsStall.
func (r *stepModelsLive) driveTurns() {
	deadline := time.NewTimer(stepModelsChainBound)
	defer deadline.Stop()
	stall := time.NewTimer(stepModelsStall)
	defer stall.Stop()
	stopped := map[string]bool{}
	ended := func(why string) {
		var where []string
		for _, issue := range r.issues {
			where = append(where, r.panes[issue.implementer].where(), r.panes[issue.tester].where())
		}
		r.fails("the chain's turns", "the chain's turns stopped %d of %d issues' tasks: %s; the stand-ins: %s",
			len(stopped), len(r.issues), why, strings.Join(where, "; "))
	}
	for len(stopped) < len(r.issues) {
		select {
		case event := <-r.events:
			stall.Reset(stepModelsStall)
			issue := r.issueOf(event.token)
			if event.written != nil {
				r.writeStepRecord(issue, event)
				close(event.written)
				continue
			}
			r.stopTask(issue, event)
			stopped[issue.name] = true
		case <-stall.C:
			ended(fmt.Sprintf("no stand-in told the driver anything for %s", stepModelsStall))
		case <-deadline.C:
			ended(fmt.Sprintf("the chain's bound of %s ran out", stepModelsChainBound))
		}
	}
}

// writeStepRecord writes the handoff record of the step event names to Dispatch's own route as
// its claim's session, the session the pane registered, with a body naming the pull request's
// head, as late_completion_live_test.go writes one. Each step has a record of its own, since a
// completion that names a record an earlier completion named is refused.
func (r *stepModelsLive) writeStepRecord(issue *stepModelsIssue, event stepModelsEvent) {
	r.t.Helper()
	held := r.claim(event.token)
	r.seen[event.token] = held
	word := "implement"
	if event.token == issue.tester {
		word = "test"
	}
	status, answer := r.dispatch.agentPost("/api/v1/issues/"+issue.key+"/handoffs/"+word, map[string]any{
		"actor": map[string]string{"kind": "session", "id": held.Session}, "body": string(headBody(issue.key, word, issue.head)),
	})
	if status != http.StatusCreated {
		r.fails("the step's handoff record", "the write of %s's %s record as session %s = %d %s, want 201", issue.key, word, held.Session, status, answer)
	}
	r.note("%s's step %d calls handoff_complete next: wrote %s's %s record as its session %s, naming head %s",
		event.token, event.step+1, issue.key, word, held.Session, issue.head)
}

// stopTask is the architect's stop of issue's task a once the claim whose step another fact ends
// sits idle and its stand-in has been quiet for three seconds: no request held and no answer
// ended in that time. So the end read is more than the comparator's two seconds from the claim's
// last response, a turn its harness starts of its own after the step's turn included: Claude
// Code starts one when its background subagent finishes, after the claim has gone idle.
func (r *stepModelsLive) stopTask(issue *stepModelsIssue, event stepModelsEvent) {
	r.t.Helper()
	pane := r.panes[event.token]
	r.await(string(event.token)+" to sit idle with its stand-in quiet", stepModelsWait, func() bool {
		held := r.claim(event.token)
		r.seen[event.token] = held
		idle := held.State == string(supervise.StateIdle) || held.State == string(supervise.StateReady)
		return idle && pane.quietFor(stepModelsAnswerDelay)
	})
	status, body := r.d.request(http.MethodPost, "/legion/v1/tasks/stop",
		api.TaskStopRequest{GrantID: r.architectGrant(issue), Issue: issue.key, Task: stepModelsTask}, false)
	if status != http.StatusOK {
		r.fails("the stop of the task", "stop_task of %s's task %s = %d %s, want 200", issue.key, stepModelsTask, status, body)
	}
	r.note("%s's step %d ended its turn, and the claim sat idle with its stand-in quiet for %s: the architect stopped %s's task %s",
		event.token, event.step+1, stepModelsAnswerDelay, issue.key, stepModelsTask)
}

// readSteps reads `dispatch steps --issue <key>` as the person until it holds the issue's
// records, each ended: the outbox writes each record to Dispatch after the fact that ended it.
func (r *stepModelsLive) readSteps(issue *stepModelsIssue) {
	r.t.Helper()
	want := stepModelsRuns[issue.name]
	read := func() bool {
		out, code := r.personsShell().bun(nil, dispatchCLI(r.root), "steps", "--issue", issue.key)
		issue.printed = string(out.stdout)
		if code != 0 {
			issue.readErr = fmt.Errorf("exit %d: %s%s", code, out.stdout, out.stderr)
			return false
		}
		printed, err := parseSteps(issue.key, issue.printed)
		if err != nil {
			issue.readErr = err
			return false
		}
		issue.lines, issue.readErr = printed.lines, nil
		if len(printed.lines) != want {
			issue.readErr = fmt.Errorf("it holds %d records, want %d", len(printed.lines), want)
			return false
		}
		for _, line := range printed.lines {
			if line.ended == "running" {
				issue.readErr = fmt.Errorf("the record %q is still running", line.text)
				return false
			}
		}
		return true
	}
	if !testwait.Holds(stepModelsWait, 2*time.Second, read) {
		r.fails("the read of the records", "`dispatch steps --issue %s` did not hold its %d records, each ended, within %s: %v\n%s",
			issue.key, want, stepModelsWait, issue.readErr, issue.printed)
	}
	r.note("`dispatch steps --issue %s` as the person:\n%s", issue.key, indentOutput(strings.TrimSpace(issue.printed)))
}

// keepEvidence writes what the run holds to its evidence, whatever the chain reached: the list of
// each response the stand-ins served, the last `dispatch steps` output of each issue, each driven
// claim's transcript, and agents.json, which names each claim's harness, its transcript and the
// model the run's settings gave it. Nothing here ends the check it runs in.
func (r *stepModelsLive) keepEvidence() {
	var served []servedResponse
	for _, issue := range r.issues {
		served = append(served, r.panes[issue.implementer].servedList()...)
		served = append(served, r.panes[issue.tester].servedList()...)
	}
	r.writeEvidenceJSON("standin-served.json", served)
	for _, issue := range r.issues {
		r.writeEvidence("dispatch-steps-"+issue.name+".txt", []byte(issue.printed))
	}
	for _, listed := range r.d.claims() {
		if _, driven := r.panes[listed.Token]; driven && listed.SessionFile != "" {
			r.seen[listed.Token] = listed
		}
	}
	agents := map[string]stepModelsAgent{}
	for _, issue := range r.issues {
		for _, token := range []claim.Token{issue.implementer, issue.tester} {
			pane := r.panes[token]
			name := "transcript-" + string(token) + ".jsonl"
			if held, ok := r.seen[token]; !ok || held.SessionFile == "" {
				r.t.Errorf("the run saw no session file of %s, so the evidence holds no transcript of it", token)
			} else if err := copyEvidenceFile(held.SessionFile, r.evidenceFileAt(name)); err != nil {
				r.t.Errorf("keep the transcript of %s: %v", token, err)
			}
			agents[string(token)] = stepModelsAgent{Harness: pane.harness, Transcript: name, SetModel: r.setModelOf(pane)}
		}
	}
	r.writeEvidenceJSON("agents.json", agents)
}

// stepModelsAgent is one claim of agents.json as the comparator reads it: its harness, its
// transcript, which a relative path names from the file's own directory, and the model the run's
// settings gave its role.
type stepModelsAgent struct {
	Harness    string `json:"harness"`
	Transcript string `json:"transcript"`
	SetModel   string `json:"setModel"`
}

// copyEvidenceFile copies the file at from to to, 0600.
func copyEvidenceFile(from, to string) error {
	body, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		return err
	}
	return os.WriteFile(to, body, 0o600)
}

// ---- checks 1 to 5: the panes and the records ------------------------------------------------------

// claudePaneRegisters is check 1: each tester registered a Claude Code session, a session id of
// Claude Code's form whose file is a transcript Claude Code writes (claudelaunch.Transcript), from
// a launch on Claude Code.
func (r *stepModelsLive) claudePaneRegisters() {
	for _, issue := range r.issues {
		held, ok := r.seen[issue.tester]
		if !ok {
			r.fails("the tester's registration", "%s's tester %s registered no session the chain saw: no step of it reached its completion or its stop",
				issue.key, issue.tester)
		}
		session, isTranscript := claudelaunch.Transcript(held.SessionFile)
		if !claudelaunch.IsSessionID(held.Session) || !isTranscript || session != held.Session {
			r.fails("a Claude Code session", "%s registered session %q with the file %q; want a Claude Code session and its own transcript",
				issue.tester, held.Session, held.SessionFile)
		}
		launches := r.rt.LaunchesOf(issue.tester)
		for _, launch := range launches {
			if runtime.Of(launch.Backend) != runtime.BackendClaude {
				r.fails("a Claude Code launch", "a launch of %s ran on %s, want claude", issue.tester, launch.Backend)
			}
		}
		r.note("%s registered the Claude Code session %s, transcript %s, over %d launches on claude", issue.tester, held.Session, held.SessionFile, len(launches))
	}
}

// recordHolds is checks 2 to 5: the record of role's attempt on issueName has ended, its end line
// in the daemon's log says endedBy, and it names the set model the run's settings gave the claim
// and answers that model for every one of n calls, where n is the count of responses of the
// agent's own conversation its stand-in served between the step's two read times. n is at least
// 3 and leaves out each subagent's response. The stand-in's own list is the oracle, not the
// transcript.
func (r *stepModelsLive) recordHolds(issueName string, role claim.Role, attempt int, endedBy string) {
	issue := r.issueNamed(issueName)
	token := issue.implementer
	if role == claim.RoleTester {
		token = issue.tester
	}
	pane := r.panes[token]
	model := r.setModelOf(pane)
	if issue.lines == nil {
		r.fails("the read of the records", "`dispatch steps --issue %s` holds no record this check can read: %v\n%s", issue.key, issue.readErr, issue.printed)
	}
	line, found := stepModelsLineOf(issue.lines, role, attempt)
	if !found {
		r.fails("the record", "`dispatch steps --issue %s` holds no record of the %s's attempt %d:\n%s", issue.key, role, attempt, issue.printed)
	}
	r.note("dispatch steps: %s", line.text)
	if line.ended == "running" {
		r.fails("the record's end", "the record of %s's %s attempt %d is still running", issue.key, role, attempt)
	}
	start, end, err := r.readsOf(issue, line)
	if err != nil {
		r.fails("the step's two reads", "%v", err)
	}
	r.note("the daemon's log: the start read at %s (read %d of generation %d), the end read at %s (read %d), endedBy %q",
		start.ReadAt.Format(time.RFC3339Nano), start.Read, start.Generation, end.ReadAt.Format(time.RFC3339Nano), end.Read, end.EndedBy)
	if end.EndedBy != endedBy {
		r.fails("endedBy", "the end line of %s's %s attempt %d says endedBy %q, want %q", issue.key, role, attempt, end.EndedBy, endedBy)
	}
	n := pane.mainResponsesBetween(start.ReadAt, end.ReadAt)
	r.note("%s's stand-in served %d responses of its own conversation between the two reads", token, n)
	if n < 3 {
		r.fails("the served count", "%s's stand-in served %d responses of its own conversation between the step's reads, want at least 3", token, n)
	}
	want := fmt.Sprintf("%s=%d(100%%)", model, n)
	if line.setModel != model || line.answered != want {
		r.fails("the record's models", "the record of %s's %s attempt %d says set %s answered %s, want set %s answered %s",
			issue.key, role, attempt, line.setModel, line.answered, model, want)
	}
}

// stepModelsLineOf is the record of kind step that role's attempt ran as, and whether lines hold
// it.
func stepModelsLineOf(lines []stepLine, role claim.Role, attempt int) (stepLine, bool) {
	for _, line := range lines {
		if line.kind == "step" && line.role == string(role) && line.attempt == attempt {
			return line, true
		}
	}
	return stepLine{}, false
}

// stepModelsRead is one line of the daemon's JSON log for a read a step used (stepModelsReadLine),
// by the keys the comparator reads.
type stepModelsRead struct {
	Msg        string    `json:"msg"`
	Issue      string    `json:"issue"`
	Task       string    `json:"task"`
	Kind       string    `json:"kind"`
	Place      int       `json:"place"`
	StartedAt  time.Time `json:"startedAt"`
	Side       string    `json:"side"`
	Generation uint64    `json:"generation"`
	Read       int64     `json:"read"`
	ReadAt     time.Time `json:"readAt"`
	EndedBy    string    `json:"endedBy"`
}

// readsOf is the start line and the end line of the daemon's log that line's record used, joined
// as the comparator joins them: by issue, task, place, kind and start, two starts being one where
// they differ by less than a microsecond, since `dispatch steps` prints at most six fraction
// digits.
func (r *stepModelsLive) readsOf(issue *stepModelsIssue, line stepLine) (start, end stepModelsRead, err error) {
	started, err := time.Parse(time.RFC3339Nano, line.started)
	if err != nil {
		return start, end, fmt.Errorf("the record's start %q: %v", line.started, err)
	}
	reads, err := readStepModelsReads(r.evidenceFileAt("daemon.jsonl"))
	if err != nil {
		return start, end, err
	}
	var starts, ends []stepModelsRead
	for _, read := range reads {
		apart := read.StartedAt.Sub(started)
		if read.Issue != issue.key || read.Task != line.task || read.Kind != line.kind || read.Place != line.step ||
			apart <= -time.Microsecond || apart >= time.Microsecond {
			continue
		}
		switch read.Side {
		case "start":
			starts = append(starts, read)
		case "end":
			ends = append(ends, read)
		}
	}
	if len(starts) != 1 || len(ends) != 1 {
		return start, end, fmt.Errorf("the daemon's log holds %d start and %d end lines %q of %s's record %q, want one of each",
			len(starts), len(ends), stepModelsReadLine, issue.key, line.text)
	}
	return starts[0], ends[0], nil
}

// readStepModelsReads is every line of the daemon's JSON log at path whose message is
// stepModelsReadLine. A log with none answers an error: a check that read no line has read
// nothing.
func readStepModelsReads(path string) ([]stepModelsRead, error) {
	lines, err := readStepModelsJSONLines(path)
	if err != nil {
		return nil, err
	}
	var reads []stepModelsRead
	for _, raw := range lines {
		var read stepModelsRead
		if json.Unmarshal(raw.text, &read) == nil && read.Msg == stepModelsReadLine {
			reads = append(reads, read)
		}
	}
	if len(reads) == 0 {
		return nil, fmt.Errorf("the daemon's log %s holds no line %q among its %d lines", path, stepModelsReadLine, len(lines))
	}
	return reads, nil
}

// stepModelsJSONLine is one line of a JSON-lines file, with its number from 1.
type stepModelsJSONLine struct {
	number int
	text   []byte
}

// readStepModelsJSONLines is each line of the file at path that is a JSON object.
func readStepModelsJSONLines(path string) ([]stepModelsJSONLine, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 1<<20), 64<<20)
	var lines []stepModelsJSONLine
	for number := 1; scanner.Scan(); number++ {
		text := bytes.TrimSpace(scanner.Bytes())
		if len(text) > 0 && text[0] == '{' && json.Valid(text) {
			lines = append(lines, stepModelsJSONLine{number: number, text: bytes.Clone(text)})
		}
	}
	return lines, scanner.Err()
}

// ---- checks 6 to 9: the comparator and its two controls --------------------------------------------

// stepModelsCompared is one run of the comparator: what it printed and its exit code.
type stepModelsCompared struct {
	out  string
	code int
}

// compare runs scripts/e2e/step-models-check.sh over both issues, the daemon's JSON log and the
// agents file agents, with extra after them, in the person's shell. The `dispatch` on its PATH
// runs this checkout's CLI under bun as the person, as the installed command does
// (plan_ratings_live_test.go, readScript).
func (r *stepModelsLive) compare(agents string, extra ...string) stepModelsCompared {
	r.t.Helper()
	if !r.chained {
		r.fails("the chain", "the chain's turns did not reach both stops and each issue's records, so there is nothing to compare")
	}
	cli := dispatchCLI(r.root)
	if strings.Contains(cli, "'") {
		r.fails("the dispatch command", "the checkout's path %s holds a quotation mark the dispatch wrapper cannot quote", r.root)
	}
	bin := filepath.Join(r.cliHome, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "dispatch"), []byte("#!/bin/sh\nexec bun '"+cli+"' \"$@\"\n"), 0o700); err != nil {
		r.t.Fatalf("write the dispatch command of the person's shell: %v", err)
	}
	args := []string{filepath.Join(r.root, "scripts", "e2e", "step-models-check.sh")}
	for _, issue := range r.issues {
		args = append(args, "--issue", issue.key)
	}
	args = append(args, "--daemon-log", r.evidenceFileAt("daemon.jsonl"), "--agents", agents)
	out, code := r.personsShell().run([]string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH")}, "bash", append(args, extra...)...)
	return stepModelsCompared{out: string(out.stdout) + string(out.stderr), code: code}
}

// comparison is the comparator's run over both issues with the run's own agents file, made once
// and kept as compare.txt.
func (r *stepModelsLive) comparison() stepModelsCompared {
	r.t.Helper()
	if r.compared == nil {
		compared := r.compare(r.evidenceFileAt("agents.json"))
		r.compared = &compared
		r.writeEvidence("compare.txt", []byte(compared.out))
		r.note("scripts/e2e/step-models-check.sh exited %d:\n%s", compared.code, indentOutput(strings.TrimRight(compared.out, "\n")))
	}
	return *r.compared
}

// liveAtBothReadsVerdict is check 6: the comparison beside c-real prints `live-at-both-reads:
// PASS`, with a PASS line for each step the architect stopped, A's implementer's third and B's
// tester's second. A PASS line names the comparison, the harness and the step as the comparator
// writes them (scripts/e2e/lib/step-models-check.ts, stepName).
func (r *stepModelsLive) liveAtBothReadsVerdict() {
	compared := r.comparison()
	lines := stepModelsOutputLines(compared.out)
	if !slices.Contains(lines, "live-at-both-reads: PASS") {
		r.fails("the verdict", "the comparator did not print `live-at-both-reads: PASS`:\n%s", compared.out)
	}
	for _, stopped := range []struct {
		issue   string
		role    claim.Role
		attempt int
	}{{"A", claim.RoleImplementer, 3}, {"B", claim.RoleTester, 2}} {
		issue := r.issueNamed(stopped.issue)
		token := issue.implementer
		if stopped.role == claim.RoleTester {
			token = issue.tester
		}
		line, found := stepModelsLineOf(issue.lines, stopped.role, stopped.attempt)
		if !found {
			r.fails("the stopped step", "`dispatch steps --issue %s` holds no record of the %s's attempt %d", issue.key, stopped.role, stopped.attempt)
		}
		prefix := fmt.Sprintf("PASS: live-at-both-reads: harness %s: %s %s %d %s attempt %d: ",
			r.panes[token].harness, issue.key, line.task, line.step, line.kind, line.attempt)
		if !slices.ContainsFunc(lines, func(out string) bool { return strings.HasPrefix(out, prefix) }) {
			r.fails("the PASS line", "the comparator printed no line starting %q, the step of %s's %s attempt %d", prefix, issue.key, stopped.role, stopped.attempt)
		}
		r.note("the comparator passed %s's stopped %s step: %q", issue.key, stopped.role, prefix)
	}
}

// cRealVerdict is check 7: the same run exits 0 and prints `c-real: PASS`. It prints the GAP lines,
// the measured gaps of each step its agent ended. Where c-real answers UNKNOWN the check fails and
// the stop rule applies: nothing in the stand-ins, the comparator or the steps changes to make it
// pass.
func (r *stepModelsLive) cRealVerdict() {
	compared := r.comparison()
	verdict := ""
	for _, line := range stepModelsOutputLines(compared.out) {
		switch {
		case strings.HasPrefix(line, "GAP:"):
			fmt.Println(line)
		case strings.HasPrefix(line, "c-real: "):
			verdict = line
		}
	}
	fmt.Println(verdict)
	if strings.HasPrefix(verdict, "c-real: UNKNOWN") {
		r.fails("c-real", "the comparator answered %q, exit %d: the stop rule applies, with the GAP lines above", verdict, compared.code)
	}
	if compared.code != 0 || verdict != "c-real: PASS" {
		r.fails("c-real", "the comparator answered %q, exit %d, want `c-real: PASS` and exit 0", verdict, compared.code)
	}
}

// controlLowerOne is check 8: the same command with --lower-one exits 1, with both verdicts FAIL.
func (r *stepModelsLive) controlLowerOne() {
	compared := r.compare(r.evidenceFileAt("agents.json"), "--lower-one")
	r.writeEvidence("compare-lower-one.txt", []byte(compared.out))
	lines := stepModelsOutputLines(compared.out)
	r.note("with --lower-one the comparator exited %d, printing %q and %q", compared.code,
		stepModelsLineWith(lines, "c-real: "), stepModelsLineWith(lines, "live-at-both-reads: "))
	if compared.code != 1 || !slices.Contains(lines, "c-real: FAIL") || !slices.Contains(lines, "live-at-both-reads: FAIL") {
		r.fails("the negative control", "with --lower-one the comparator exited %d, want 1 with both verdicts FAIL:\n%s", compared.code, compared.out)
	}
}

// controlWrongSettings is check 9: the same command, over a copy of agents.json whose A's
// implementer names another setModel, exits 1 and names both models.
func (r *stepModelsLive) controlWrongSettings() {
	body, err := os.ReadFile(r.evidenceFileAt("agents.json"))
	if err != nil {
		r.fails("the agents file", "read the run's agents file: %v", err)
	}
	var agents map[string]stepModelsAgent
	if err := json.Unmarshal(body, &agents); err != nil {
		r.fails("the agents file", "decode the run's agents file: %v", err)
	}
	implementer := string(r.issueNamed("A").implementer)
	settings := agents[implementer]
	recorded, wrong := settings.SetModel, settings.SetModel+"-not-the-settings"
	settings.SetModel = wrong
	agents[implementer] = settings
	r.writeEvidenceJSON("agents-wrong-settings.json", agents)
	compared := r.compare(r.evidenceFileAt("agents-wrong-settings.json"))
	r.writeEvidence("compare-wrong-settings.txt", []byte(compared.out))
	r.note("with %s's setModel %s in a copy of agents.json the comparator exited %d", implementer, wrong, compared.code)
	if compared.code != 1 || !strings.Contains(compared.out, recorded) || !strings.Contains(compared.out, wrong) {
		r.fails("the second control", "with %s's setModel %s the comparator exited %d, want 1 naming %s and %s:\n%s",
			implementer, wrong, compared.code, recorded, wrong, compared.out)
	}
}

// stepModelsOutputLines is each line a command printed, without its line end.
func stepModelsOutputLines(out string) []string {
	return strings.Split(strings.TrimRight(out, "\n"), "\n")
}

// stepModelsLineWith is the last of lines that starts with prefix, "" for none.
func stepModelsLineWith(lines []string, prefix string) string {
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.HasPrefix(lines[i], prefix) {
			return lines[i]
		}
	}
	return ""
}

// ---- checks 10 and 11: the captures of a real Claude Code session --------------------------------

// stepModelsCapture is one capture attempt that ran: its name, whether its stream wrote its
// `result` line, its stream and the copy of its projects directory in the evidence, its
// stand-in's list, and what capture-<name>-exit.txt holds.
type stepModelsCapture struct {
	name             string
	result           bool
	stream, projects string
	served           []servedResponse
	exit             string
}

// runCaptures runs the pinned Claude Code alone, outside the chain, to capture the two shapes no
// file of the repository holds: capture-differ always, whose stand-in serves another model than
// the one asked for, and capture-same only where capture-differ wrote no `result` line.
func (r *stepModelsLive) runCaptures() {
	r.captures = map[string]*stepModelsCapture{}
	differ := r.captureClaude("capture-differ", stepModelsCaptureServed)
	r.captures[differ.name] = differ
	if differ.result {
		r.note("capture-differ wrote its result line, so capture-same does not run")
		return
	}
	same := r.captureClaude("capture-same", stepModelsClaudeModel)
	r.captures[same.name] = same
}

// captureClaude runs one capture attempt: `claude -p` with the flags of claudelaunch.Argv that
// shape its output, under a HOME and a CLAUDE_CONFIG_DIR of its own, with claudelaunch.Environment's
// pairs, PATH, and its own stand-in's endpoint, which serves served for every response. Its stdin
// is one user line and then the end of the file. The stand-in answers the first two requests as a
// stopped step's turn does and the third with a text that ends. The attempt keeps its stdout, each
// transcript under its projects directory, its stand-in's list, and its exit code with the last 50
// lines of its stderr, each under its own name.
func (r *stepModelsLive) captureClaude(name, served string) *stepModelsCapture {
	t := r.t
	t.Helper()
	dir := t.TempDir()
	home, configDir, cwd := filepath.Join(dir, "home"), filepath.Join(dir, "claude"), filepath.Join(dir, "work")
	for _, made := range []string{home, configDir, cwd} {
		if err := os.MkdirAll(made, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	standin := newStepModelsStandin(t, name, "claude", served, stepModelsClaudeModel, stepModelsCaptureMarker,
		claim.Token(name), []stepModelsStep{{}}, nil, r.done)
	capture := &stepModelsCapture{name: name, stream: r.evidenceFileAt(name + "-stream.jsonl"), projects: r.evidenceFileAt(name + "-projects")}
	stdout, err := os.Create(capture.stream)
	if err != nil {
		t.Fatalf("create %s: %v", capture.stream, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.claude, "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--permission-mode", "bypassPermissions", "--session-id", claudelaunch.NewSessionID())
	cmd.Dir = cwd
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + home}, claudelaunch.Environment(configDir)...)
	cmd.Env = append(cmd.Env, claudeStandinEnvironment(standin.url)...)
	cmd.Stdin = strings.NewReader(`{"type":"user","message":{"role":"user","content":"` + stepModelsCaptureMarker + `"}}` + "\n")
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = stdout, &stderr
	ran := cmd.Run()
	_ = stdout.Close()
	code := 0
	if ran != nil {
		var exit *exec.ExitError
		if !errors.As(ran, &exit) {
			code = -1
			stderr.WriteString("\n" + ran.Error() + "\n")
		} else {
			code = exit.ExitCode()
		}
	}
	capture.exit = fmt.Sprintf("exit %d\n%s\n", code, strings.Join(stepModelsLastLines(stderr.String(), 50), "\n"))
	r.writeEvidence(name+"-exit.txt", []byte(capture.exit))
	capture.served = standin.servedList()
	r.writeEvidenceJSON(name+"-served.json", capture.served)
	copied := r.keepProjects(filepath.Join(configDir, "projects"), capture.projects)
	lines, err := readStepModelsJSONLines(capture.stream)
	if err != nil {
		t.Errorf("read %s: %v", capture.stream, err)
	}
	capture.result = slices.ContainsFunc(lines, func(line stepModelsJSONLine) bool {
		var typed struct {
			Type string `json:"type"`
		}
		return json.Unmarshal(line.text, &typed) == nil && typed.Type == "result"
	})
	r.note("%s: Claude Code exited %d, its stream holds %d lines (result line %t), its stand-in served %d responses, %d transcripts kept under %s",
		name, code, len(lines), capture.result, len(capture.served), copied, capture.projects)
	for _, answered := range capture.served {
		r.note("  %s answered place %d of step %d (subagent %t, side %t, asked %q) for %s",
			answered.ID, answered.Place, answered.Step, answered.Subagent, answered.Side, answered.Asked, answered.Turn)
	}
	return capture
}

// keepProjects copies each `.jsonl` file under from, a Claude Code projects directory, to the same
// place under to, and answers how many it copied: a directory Claude Code never made copies none.
func (r *stepModelsLive) keepProjects(from, to string) int {
	copied := 0
	err := filepath.WalkDir(from, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return filepath.SkipAll
			}
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		relative, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		copied++
		return copyEvidenceFile(path, filepath.Join(to, relative))
	})
	if err != nil {
		r.t.Errorf("keep the transcripts under %s: %v", from, err)
	}
	return copied
}

// stepModelsLastLines is at most the last n lines of text.
func stepModelsLastLines(text string, n int) []string {
	lines := stepModelsOutputLines(text)
	return lines[max(0, len(lines)-n):]
}

// claudeCapture is check 10: the capture that supplies the shapes, capture-differ where it wrote
// its `result` line and else capture-same, holds a `system` `init` line, an assistant line whose
// `parent_tool_use_id` is a string, which is a subagent's, and one response id on two or more
// assistant lines. It names which attempt that was.
func (r *stepModelsLive) claudeCapture() {
	var capture *stepModelsCapture
	for _, name := range []string{"capture-differ", "capture-same"} {
		if attempt := r.captures[name]; attempt != nil && attempt.result {
			capture = attempt
			break
		}
	}
	if capture == nil {
		exit := ""
		if differ := r.captures["capture-differ"]; differ != nil {
			exit = differ.exit
		}
		r.fails("a capture's result", "no capture attempt wrote its result line, so no capture supplies the shapes; capture-differ-exit.txt:\n%s", exit)
	}
	lines, err := readStepModelsJSONLines(capture.stream)
	if err != nil {
		r.fails("the capture's stream", "read %s: %v", capture.stream, err)
	}
	hasInit, subagent := false, false
	ids := map[string]int{}
	for _, line := range lines {
		var shaped struct {
			Type            string `json:"type"`
			Subtype         string `json:"subtype"`
			ParentToolUseID any    `json:"parent_tool_use_id"`
			Message         struct {
				ID string `json:"id"`
			} `json:"message"`
		}
		if json.Unmarshal(line.text, &shaped) != nil {
			continue
		}
		switch shaped.Type {
		case "system":
			hasInit = hasInit || shaped.Subtype == "init"
		case "assistant":
			if _, isString := shaped.ParentToolUseID.(string); isString {
				subagent = true
			}
			if shaped.Message.ID != "" {
				ids[shaped.Message.ID]++
			}
		}
	}
	var shared []string
	for id, count := range ids {
		if count >= 2 {
			shared = append(shared, fmt.Sprintf("%s on %d lines", id, count))
		}
	}
	slices.Sort(shared)
	r.note("%s supplies the shapes: a system init line %t, an assistant line of a subagent %t, response ids on two or more assistant lines %v",
		capture.name, hasInit, subagent, shared)
	if !hasInit || !subagent || len(shared) == 0 {
		r.fails("the capture's shapes", "%s's stream %s holds a system init line %t, an assistant line whose parent_tool_use_id is a string %t, "+
			"and %d response ids on two or more assistant lines; want all three", capture.name, capture.stream, hasInit, subagent, len(shared))
	}
}

// setModelHeld is check 11, from capture-differ alone. Its stand-in's list must show that every
// response of the conversation served claude-served-model, so the set model never answered. It
// then prints each JSON path of each of its transcripts whose value is claude-model, or
// `set-model-held: none`, and the model the stream's init line names. Where capture-differ wrote
// no result line it prints `set-model-held: unknown` with capture-differ-exit.txt. None and
// unknown each fail the check: the stop rule applies, and nothing in the stand-ins, the
// comparator or the steps changes to make it pass.
func (r *stepModelsLive) setModelHeld() {
	differ := r.captures["capture-differ"]
	if differ == nil {
		r.fails("capture-differ", "capture-differ did not run")
	}
	if !differ.result {
		fmt.Println("set-model-held: unknown")
		r.note("capture-differ-exit.txt:\n%s", indentOutput(strings.TrimRight(differ.exit, "\n")))
		r.fails("set-model-held", "capture-differ wrote no result line, so no capture shows whether a transcript holds a set model that never answered: the stop rule applies")
	}
	answered := 0
	for _, served := range differ.served {
		if !served.main() {
			continue
		}
		answered++
		if served.Model != stepModelsCaptureServed {
			r.fails("the served list", "capture-differ's stand-in served %s for response %s, want %s for each", served.Model, served.ID, stepModelsCaptureServed)
		}
	}
	if answered == 0 {
		r.fails("the served list", "capture-differ's stand-in served no response of the conversation")
	}
	r.note("capture-differ's stand-in served %s for each of its %d responses of the conversation, so %s never answered",
		stepModelsCaptureServed, answered, stepModelsClaudeModel)
	if lines, err := readStepModelsJSONLines(differ.stream); err == nil {
		for _, line := range lines {
			var initLine struct {
				Type, Subtype, Model string
			}
			if json.Unmarshal(line.text, &initLine) == nil && initLine.Type == "system" && initLine.Subtype == "init" {
				fmt.Printf("init model: %s\n", initLine.Model)
			}
		}
	}
	var held []string
	err := filepath.WalkDir(differ.projects, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		lines, err := readStepModelsJSONLines(path)
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(r.evidence, path)
		for _, line := range lines {
			var value any
			if json.Unmarshal(line.text, &value) != nil {
				continue
			}
			for _, at := range stepModelsPathsHolding(value, stepModelsClaudeModel, "$") {
				held = append(held, fmt.Sprintf("%s:%d %s", relative, line.number, at))
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		r.fails("capture-differ's transcripts", "read the transcripts under %s: %v", differ.projects, err)
	}
	if len(held) == 0 {
		fmt.Println("set-model-held: none")
		r.fails("set-model-held", "no transcript of capture-differ holds %s at any path: the stop rule applies", stepModelsClaudeModel)
	}
	for _, at := range held {
		fmt.Printf("set-model-held: %s\n", at)
	}
}

// stepModelsPathsHolding is the JSON path, from at, of each string in value that is want, keys in
// sorted order.
func stepModelsPathsHolding(value any, want, at string) []string {
	switch v := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		var found []string
		for _, key := range keys {
			found = append(found, stepModelsPathsHolding(v[key], want, at+"."+key)...)
		}
		return found
	case []any:
		var found []string
		for i, item := range v {
			found = append(found, stepModelsPathsHolding(item, want, fmt.Sprintf("%s[%d]", at, i))...)
		}
		return found
	case string:
		if v == want {
			return []string{at}
		}
	}
	return nil
}
