package answer

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/dispatchtool"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// stubDispatch is the dispatch tool's stand-in: it records each call and answers each with
// output naming it, failing a call whose arguments name a failure.
type stubDispatch struct {
	mu    sync.Mutex
	calls []dispatchtool.Call
}

func (d *stubDispatch) Run(_ context.Context, call dispatchtool.Call) dispatchtool.Result {
	d.mu.Lock()
	d.calls = append(d.calls, call)
	d.mu.Unlock()
	if strings.Contains(call.Args, "fails") {
		return dispatchtool.Result{Output: "dispatch comment was refused: no such issue", Exit: 1, Ran: true}
	}
	return dispatchtool.Result{Output: "dispatch output of " + call.Args, Exit: 0, Ran: true}
}

func (d *stubDispatch) made() []dispatchtool.Call {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]dispatchtool.Call(nil), d.calls...)
}

// anthropicDispatch is an Anthropic tool_use block calling the dispatch tool with args, and
// stdin where it is not "".
func anthropicDispatch(id, stdin string, args ...string) map[string]any {
	input := map[string]any{"args": args}
	if stdin != "" {
		input["stdin"] = stdin
	}
	return map[string]any{"type": "tool_use", "id": id, "name": "dispatch", "input": input}
}

// The system prompt names each tool's bound: at most gh_calls gh calls and, apart from them, at
// most gh_calls dispatch calls.
func TestThePromptNamesTheBoundOfEachTool(t *testing.T) {
	s := newStandIn(t)
	req := anthropicRequest(t, s, &stubGH{}, 3)
	req.Dispatch = &stubDispatch{}
	prompt := systemPrompt(req)
	for _, want := range []string{"at most 20 gh calls", "at most 20 dispatch calls, counted apart from its gh calls", "3 model calls"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the system prompt lacks %q:\n%s", want, prompt)
		}
	}
}

// A dispatch call runs through the request's dispatch tool with the model's arguments and stdin,
// and its output reaches the model as the call's result, a failed call's as an error result naming
// its exit. Each tool's calls are numbered apart, an earlier run's counted, which is what each
// tool's bound reads, while every call takes its seq from one count across both tools.
func TestADispatchCallsResultReachesTheModelAndEachToolCountsItsOwnCalls(t *testing.T) {
	s := newStandIn(t,
		anthropicStream("tool_use", anthropicUsage{Input: 4, Output: 5},
			anthropicGH("toolu_1", "pr", "view", "7"),
			anthropicDispatch("toolu_2", "Plum.", "comment", "--issue", "INFRA-1", "--body-file", "-"),
			anthropicDispatch("toolu_3", "", "comment", "--issue", "fails", "--body", "x")),
		anthropicStream("end_turn", anthropicUsage{Input: 2, Output: 3}, anthropicText(`{"kind":"answered","text":"Posted.","needs":""}`)),
	)
	gh, dispatch := &stubGH{}, &stubDispatch{}
	req := anthropicRequest(t, s, gh, 3)
	req.Dispatch = dispatch
	output, exit := "done", 0
	req.Earlier = []store.AnswerToolCall{
		{Seq: 1, Tool: store.ToolGH, Args: []string{"pr", "checks", "7"}, Result: &output, Exit: &exit},
		{Seq: 2, Tool: store.ToolDispatch, Args: []string{"read", "INFRA-1"}, Result: &output, Exit: &exit},
		{Seq: 3, Tool: store.ToolDispatch, Args: []string{"search", "--query", "plum"}, Result: &output, Exit: &exit},
	}
	reply, err := Answer(context.Background(), req)
	if err != nil || reply.Kind != Answered {
		t.Fatalf("reply %+v, %v; want answered", reply, err)
	}
	if calls := gh.made(); !reflect.DeepEqual(calls, []GHCall{{4, 2, `["pr","view","7"]`}}) {
		t.Errorf("gh calls %+v; want seq 4, the second gh call", calls)
	}
	// agentloop runs a step's calls one at a time, in whichever order they take its lock, so each
	// call's seq is the model's order and its number among the dispatch calls the order they ran
	// in: the two calls are the third and fourth dispatch calls, in either order.
	calls := dispatch.made()
	slices.SortFunc(calls, func(a, b dispatchtool.Call) int { return a.Seq - b.Seq })
	if len(calls) == 2 && calls[0].N == 4 && calls[1].N == 3 {
		calls[0].N, calls[1].N = 3, 4
	}
	want := []dispatchtool.Call{
		{Seq: 5, N: 3, Args: `["comment","--issue","INFRA-1","--body-file","-"]`, Stdin: "Plum."},
		{Seq: 6, N: 4, Args: `["comment","--issue","fails","--body","x"]`},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Errorf("dispatch calls %+v; want %+v, numbered 3 and 4 in either order", calls, want)
	}
	results := blocks(messages(t, s.requests()[1])[4])
	if len(results) != 3 {
		t.Fatalf("the second call carries %d results, want 3: %v", len(results), results)
	}
	if text, _ := json.Marshal(results[1]["content"]); results[1]["tool_use_id"] != "toolu_2" || results[1]["is_error"] == true ||
		!strings.Contains(string(text), `dispatch output of [\"comment\",\"--issue\",\"INFRA-1\",\"--body-file\",\"-\"]`) {
		t.Errorf("the dispatch call's result reached the model as %v, want its output", results[1])
	}
	if text, _ := json.Marshal(results[2]["content"]); results[2]["is_error"] != true || !strings.Contains(string(text), "[dispatch exited 1]") {
		t.Errorf("the failed dispatch call's result reached the model as %v, want an error result naming the exit", results[2])
	}
	if turn := reply.Append.Turns[0]; len(turn.Results) != 3 || turn.Results[1].ToolName != "dispatch" {
		t.Errorf("the append's first turn %+v; want the dispatch call's result among its three", turn)
	}
}

// On a re-run the question names each earlier call's tool with its arguments and its result: a gh
// call and a dispatch call, a dispatch write whose outcome a restart recorded as unknown (it ran,
// and what it did is unknown, so it is never said not to have run), a call the tool refused, which
// did not run, and a write with no result at all, of unknown outcome.
func TestTheRerunListNamesEachEarlierCallsTool(t *testing.T) {
	s := newStandIn(t, anthropicStream("end_turn", anthropicUsage{Input: 1, Output: 1}, anthropicText(`{"kind":"answered","text":"Done.","needs":""}`)))
	req := anthropicRequest(t, s, &stubGH{}, 1)
	req.Dispatch = &stubDispatch{}
	output, exit, unknown, refused := "open", 0, dispatchtool.UnknownOutcome, "refused: `dispatch claim` is none of the commands this tool runs"
	req.Earlier = []store.AnswerToolCall{
		{Seq: 1, Tool: store.ToolGH, Args: []string{"pr", "view", "7"}, Result: &output, Exit: &exit},
		{Seq: 2, Tool: store.ToolDispatch, Args: []string{"read", "INFRA-1"}, Result: &output, Exit: &exit},
		{Seq: 3, Tool: store.ToolDispatch, Args: []string{"comment", "--issue", "INFRA-1", "--body-file", "-"}, Result: &unknown},
		{Seq: 4, Tool: store.ToolDispatch, Args: []string{"claim", "--issue", "INFRA-1"}, Result: &refused},
		{Seq: 5, Tool: store.ToolDispatch, Args: []string{"message", "--issue", "INFRA-1", "--body", "x"}},
	}
	if _, err := Answer(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	text := blocks(messages(t, s.requests()[0])[2])[0]["text"].(string)
	for _, want := range []string{
		"\n1. gh [\"pr\",\"view\",\"7\"]\nExit 0:\nopen\n",
		"\n2. dispatch [\"read\",\"INFRA-1\"]\nExit 0:\nopen\n",
		"\n3. dispatch [\"comment\",\"--issue\",\"INFRA-1\",\"--body-file\",\"-\"]\nNo exit status:\n" + dispatchtool.UnknownOutcome + "\n",
		"\n4. dispatch [\"claim\",\"--issue\",\"INFRA-1\"]\nDid not run:\n" + refused + "\n",
		"\n5. dispatch [\"message\",\"--issue\",\"INFRA-1\",\"--body\",\"x\"]\nOutcome unknown: it may or may not have run.\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the question does not hold %q:\n%s", want, text)
		}
	}
}
