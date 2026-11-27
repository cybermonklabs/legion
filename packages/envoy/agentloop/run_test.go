package agentloop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testwait"
)

// The prices the fixture's models charge, in USD per million tokens.
var (
	anthropicPrice = Price{Input: 5, Output: 25, CacheWrite: 6.25, CacheRead: 0.5}
	openaiPrice    = Price{Input: 2, Output: 10, CacheWrite: 2.5, CacheRead: 0.1}
)

// toolName is the name of the one tool every fixture registers.
const toolName = "read"

// recordingTool is the fixture's tool: it records every call it is given and answers with the
// output naming it. onCall, where a test sets one, runs inside the call before it answers.
type recordingTool struct {
	mu     sync.Mutex
	calls  []ToolCall
	onCall func(ToolCall)
}

func (r *recordingTool) register() Tool {
	return Tool{
		Name:        toolName,
		Description: "Read a file.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`),
		Run: func(_ context.Context, call ToolCall) (string, bool) {
			r.mu.Lock()
			r.calls = append(r.calls, call)
			onCall := r.onCall
			r.mu.Unlock()
			if onCall != nil {
				onCall(call)
			}
			return "the contents of " + string(call.Input), false
		},
	}
}

// made are the calls the tool was given, in order.
func (r *recordingTool) made() []ToolCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ToolCall(nil), r.calls...)
}

// fixture is one run against a stand-in: the request, the tool it registers and the path of the
// file its endpoint reads its token from, which a test may rewrite between calls.
type fixture struct {
	req   Request
	tool  *recordingTool
	token string
}

// newFixture is a request against s on the protocol kind speaks, allowed calls model calls, with
// the one recording tool registered, a token read from a file on every attempt, and the time
// bound the test binary's deadline leaves (runnerTime).
func newFixture(t *testing.T, s *standIn, kind Kind, calls int) *fixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	token := "standin-anthropic-token"
	if kind == OpenAIResponses {
		token = "standin-openai-token"
	}
	writeToken(t, path, token)
	tool := &recordingTool{}
	model := Model{
		Endpoint: Endpoint{Kind: kind, BaseURL: s.server.URL, Token: fileToken(path)},
		ID:       "claude-standin", Effort: "high", Price: anthropicPrice,
	}
	if kind == OpenAIResponses {
		model.Endpoint.BaseURL = s.server.URL + "/v1"
		model.ID, model.Price = "gpt-6.1-standin", openaiPrice
	}
	return &fixture{
		req: Request{
			Model:  model,
			System: "You answer the question with the tools you are given.",
			Input:  []Message{{Role: User, Blocks: []Block{{Kind: TextBlock, Text: "What does note.txt say?"}}}},
			Tools:  []Tool{tool.register()},
			Bounds: Bounds{Time: runnerTime(t), Calls: calls, OutputTokens: 4000},
		},
		tool: tool, token: path,
	}
}

// runnerTime is the time bound of a fixture's run or decision, whose subject is not the time
// bound: what the test binary's deadline (go test's -timeout) leaves, less runnerMargin
// (testwait.Remaining), and never more than runnerTimeCap. A call that is merely slow on a loaded
// machine is then no time fault, and one that never answers still ends, as a fault the test
// reports by name, before go test's alarm panics the binary. Under -timeout 0 the binary has no
// deadline and the bound is runnerTimeCap. A row whose subject is the time bound sets a short
// bound of its own (TestARunPastItsTimeBoundIsAFault). A deadline that leaves no more than
// runnerMargin fails the test at once with Remaining's line, which names the cause: a -timeout no
// longer than runnerMargin, or what ran before this row, such as an earlier row whose run waited
// out its own bound, using the deadline up.
func runnerTime(t *testing.T) time.Duration {
	t.Helper()
	return min(runnerTimeCap, testwait.Remaining(t, runnerMargin))
}

const (
	// runnerMargin is what runnerTime leaves of the test binary's deadline for a run that met its
	// bound to end, its test to report the fault and its stand-ins to close: a run past its
	// bound ends within milliseconds of it (TestARunPastItsTimeBoundIsAFault).
	runnerMargin = 30 * time.Second
	// runnerTimeCap is the most runnerTime gives: the bound under -timeout 0, where the binary
	// has no deadline to derive one from, a day, far past any run against a stand-in.
	runnerTimeCap = 24 * time.Hour
)

// writeToken writes the endpoint's token to its file.
func writeToken(t *testing.T, path, token string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fileToken reads the endpoint's token from its file, as a caller holding a rotated key would.
func fileToken(path string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(raw)), nil
	}
}

// messages are an Anthropic request's messages.
func messages(t *testing.T, r recorded) []map[string]any {
	t.Helper()
	var body struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(r.body, &body); err != nil {
		t.Fatal(err)
	}
	return body.Messages
}

// blocks are a message's content blocks.
func blocks(m map[string]any) []map[string]any {
	var out []map[string]any
	content, _ := m["content"].([]any)
	for _, b := range content {
		out = append(out, b.(map[string]any))
	}
	return out
}

// seen is one call the recording tool was given, as a test compares it.
type seen struct {
	index int
	id    string
	input string
}

func madeSeen(calls []ToolCall) []seen {
	out := make([]seen, len(calls))
	for i, c := range calls {
		out[i] = seen{index: c.Index, id: c.ID, input: string(c.Input)}
	}
	return out
}

// historySignature is the signature over the replayed history's thinking, with the characters a
// signature carries that JSON could escape.
const historySignature = "EqQBCkgIARABGAIiQL+/hist0ry==/sig"

// anthropicHistory is a conversation on Anthropic: a request, and a model turn of signed thinking
// and text.
func anthropicHistory() []Message {
	return []Message{
		{Role: User, Blocks: []Block{{Kind: TextBlock, Text: "Write the plan."}}},
		{Role: Assistant, Blocks: []Block{
			{Kind: ThinkingBlock, Text: "The plan needs a migration.", Signature: historySignature},
			{Kind: TextBlock, Text: "The plan is written."},
		}},
	}
}

// threeCallAnthropic are the replies of a three-call run: thinking and a tool call, a second tool
// call, then the answer.
func threeCallAnthropic() []reply {
	return []reply{
		anthropicStream("tool_use", anthropicUsage{Input: 10, Output: 20, CacheWrite: 1000},
			anthropicThinking("Read the note.", "sig-step-1+/=="), anthropicToolUse("toolu_1", toolName, map[string]any{"path": "note.txt"})),
		anthropicStream("tool_use", anthropicUsage{Input: 3, Output: 15, CacheWrite: 50, CacheRead: 1000},
			anthropicToolUse("toolu_2", toolName, map[string]any{"path": "other.txt"})),
		anthropicStream("end_turn", anthropicUsage{Input: 2, Output: 30, CacheWrite: 40, CacheRead: 1050},
			anthropicText("The note says plum.")),
	}
}

// The encrypted reasoning of the OpenAI Responses history, as the caller kept it.
const (
	openaiHistoryOne = "gAAAAhist+/one=="
	openaiHistoryTwo = "gAAAAhist+/two=="
)

// openaiHistory is a conversation on OpenAI Responses: a request, a turn of reasoning and a tool
// call, that call's result, and a turn of reasoning and an answer.
func openaiHistory() []Message {
	return []Message{
		{Role: User, Blocks: []Block{{Kind: TextBlock, Text: "Read note.txt."}}},
		{Role: Assistant, Blocks: []Block{
			{Kind: ReasoningBlock, Text: "Reading the note", ItemID: "rs_h1", Encrypted: openaiHistoryOne},
			{Kind: ToolCallBlock, ToolCallID: "call_h1", Name: toolName, Input: json.RawMessage(`{"path":"note.txt"}`)},
		}},
		{Role: ToolRole, Blocks: []Block{
			{Kind: ToolResultBlock, ToolCallID: "call_h1", Name: toolName, Content: []Block{{Kind: TextBlock, Text: "plum"}}},
		}},
		{Role: Assistant, Blocks: []Block{
			{Kind: ReasoningBlock, Text: "Answering", ItemID: "rs_h2", Encrypted: openaiHistoryTwo},
			{Kind: TextBlock, Text: "It says plum."},
		}},
	}
}

// inputItems are an OpenAI Responses request's input items.
func inputItems(t *testing.T, r recorded) []map[string]any {
	t.Helper()
	var body struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(r.body, &body); err != nil {
		t.Fatal(err)
	}
	return body.Input
}

// An OpenAI Responses run of a request that sets no cache field replays every reasoning item of
// the history with its id and encrypted content byte for byte, runs stateless at the model's
// effort within the run's output token bound, marks the end of the history for the cache once,
// sends the first call's reasoning back in the second, offers no tool on the last call, and
// reports each call's output items as the API sent them.
func TestOpenAIResponsesReplaysReasoningItemsByteForByteAndSendsTheStepsReasoningBack(t *testing.T) {
	t.Parallel()
	stepOne := []string{
		openaiReasoning("rs_s1", "gAAAAstep+/one==", "Checking the note"),
		openaiToolCall("fc_s1", "call_s1", toolName, map[string]any{"path": "note.txt"}),
	}
	stepTwo := []string{openaiMessage("msg_s2", "It said plum.")}
	s := newStandIn(t,
		openaiStream(openaiUsage{Input: 1000, CacheWrite: 900, Output: 50, Reasoning: 20}, "", stepOne...),
		openaiStream(openaiUsage{Input: 1100, Cached: 900, Output: 30}, "", stepTwo...),
	)
	f := newFixture(t, s, OpenAIResponses, 2)
	f.req.History = openaiHistory()
	result, err := Run(context.Background(), f.req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Fault != "" || result.Text != "It said plum." {
		t.Fatalf("result %+v, want the answer and no fault", result)
	}
	got := s.requests()
	if len(got) != 2 {
		t.Fatalf("%d model calls, want 2", len(got))
	}
	for i, r := range got {
		if r.path != "/v1/responses" {
			t.Errorf("call %d went to %s, want /v1/responses", i+1, r.path)
		}
		body := r.decoded(t)
		if body["store"] != false || !reflect.DeepEqual(body["include"], []any{"reasoning.encrypted_content"}) {
			t.Errorf("call %d: store %v, include %v; want a stateless call returning the encrypted reasoning", i+1, body["store"], body["include"])
		}
		if reasoning, _ := body["reasoning"].(map[string]any); reasoning["effort"] != "high" {
			t.Errorf("call %d's reasoning %v, want effort high, the model's", i+1, body["reasoning"])
		}
		if body["max_output_tokens"] != 4000.0 {
			t.Errorf("call %d's max_output_tokens %v, want the run's bound", i+1, body["max_output_tokens"])
		}
		if last := i == 1; (body["tool_choice"] == "none") != last {
			t.Errorf("call %d's tool_choice %v; want none on the last call only", i+1, body["tool_choice"])
		}
		for _, encrypted := range []string{openaiHistoryOne, openaiHistoryTwo} {
			if !bytes.Contains(r.body, []byte(`"encrypted_content":`+quote(encrypted))) {
				t.Errorf("call %d does not carry %s byte for byte", i+1, encrypted)
			}
		}
		var ids, breakpoints []string
		for _, item := range inputItems(t, r) {
			if item["type"] == "reasoning" {
				ids = append(ids, item["id"].(string))
			}
			if countKey(item, "prompt_cache_breakpoint") > 0 {
				breakpoints = append(breakpoints, item["type"].(string)+":"+item["call_id"].(string))
			}
		}
		wantIDs := []string{"rs_h1", "rs_h2"}
		if i == 1 {
			wantIDs = append(wantIDs, "rs_s1")
		}
		if !reflect.DeepEqual(ids, wantIDs) {
			t.Errorf("call %d's reasoning items %v, want %v", i+1, ids, wantIDs)
		}
		if !reflect.DeepEqual(breakpoints, []string{"function_call_output:call_h1"}) {
			t.Errorf("call %d's cache breakpoints %v, want one on the history's last tool result", i+1, breakpoints)
		}
	}
	if !bytes.Contains(got[1].body, []byte(`"encrypted_content":"gAAAAstep+/one=="`)) {
		t.Error("the second call does not carry the first call's reasoning")
	}
	if len(result.Steps) != 2 {
		t.Fatalf("%d steps, want 2", len(result.Steps))
	}
	for i, want := range [][]string{stepOne, stepTwo} {
		var native []string
		for _, item := range result.Steps[i].Native {
			native = append(native, string(item))
		}
		if !reflect.DeepEqual(native, want) {
			t.Errorf("step %d's output items\n%v\nwant them as the API sent them\n%v", i+1, native, want)
		}
	}
}

// finalSchema is the form a Final reply must take, and finalAnswer one reply of that form.
const (
	finalSchema = `{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}`
	finalAnswer = `{"answer":"plum"}`
)

// A last reply the provider cut off at the output token limit is a length fault: no value comes
// back, and the step and the usage that call spent are still reported.
func TestALastReplyCutOffIsAFault(t *testing.T) {
	t.Parallel()
	s := newStandIn(t, anthropicStream("max_tokens", anthropicUsage{Input: 7, Output: 4000}, anthropicText(`{"answer":"pl`)))
	f := newFixture(t, s, AnthropicMessages, 1)
	f.req.Final = json.RawMessage(finalSchema)
	result, err := Run(context.Background(), f.req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Fault != FaultLength {
		t.Errorf("fault %q, want %q: %s", result.Fault, FaultLength, result.Detail)
	}
	if result.Value != nil {
		t.Errorf("the cut-off reply gave the value %s", result.Value)
	}
	if len(result.Steps) != 1 || result.Steps[0].Finish != "length" {
		t.Errorf("steps %+v, want the one call that was made", result.Steps)
	}
	if u := result.Usage; u.Input != 7 || u.Output != 4000 || !u.Metered {
		t.Errorf("usage %+v, want the cut-off call's own", u)
	}
}

// More than one of a provider's own reasons reaches the loop as the same normalized length:
// Anthropic ends a reply that way both for the run's output token bound and for a prompt past
// the model's context window, which is no reply cut off at that bound. The detail names the
// provider's own reason, so a reader of the log can tell the two apart.
func TestALengthFaultsDetailNamesTheProvidersOwnReason(t *testing.T) {
	t.Parallel()
	for _, stopReason := range []string{"max_tokens", "model_context_window_exceeded"} {
		t.Run(stopReason, func(t *testing.T) {
			t.Parallel()
			s := newStandIn(t, anthropicStream(stopReason, anthropicUsage{Input: 7, Output: 11}, anthropicText("The note says")))
			result, err := Run(context.Background(), newFixture(t, s, AnthropicMessages, 1).req)
			if err != nil {
				t.Fatal(err)
			}
			if result.Fault != FaultLength {
				t.Fatalf("fault %q, want %q: %s", result.Fault, FaultLength, result.Detail)
			}
			if !strings.Contains(result.Detail, stopReason) {
				t.Errorf("the detail is %q, want the provider's own reason %q in it", result.Detail, stopReason)
			}
		})
	}
}

// cutSays is what the cut reply says, and cutAnswer that reply as the provider cut it at the
// output token bound: a whole answer inside finalSchema's string, then the newline escapes a
// model running on inside its structured reply writes until the bound stops it. No other reply
// says cutSays, so a request that carries it carries the cut reply.
const cutSays = "The note says plum."

var cutAnswer = `{"answer":"` + cutSays + strings.Repeat(`\n`, 64)

// A reply the provider cut at the output token bound is made again where the request asks for
// it (Request.CallAgainWhenCut), on the run's first call and on one after a tool call, on both
// protocols the Legion daemon calls: the call made again is the cut call byte for byte, its
// cache marks and the tool call and result before it included, so it carries none of the cut
// reply, and its reply is the run's answer. Result.Steps holds every call, the cut one alone
// marked dropped and still carrying its own output items, and Result.Usage sums them all, the
// cut call's 4000 output tokens included. The control is the same replies with the field zero:
// the run sends the same requests up to the cut and ends on it with FaultLength, dropping
// nothing.
func TestACutReplyIsMadeAgainWithoutIt(t *testing.T) {
	t.Parallel()
	const cutItem, finalItem = "msg_cut", "msg_final"
	anthropicCut := func() reply {
		return anthropicStream("max_tokens", anthropicUsage{Input: 30, Output: 4000, CacheRead: 2200}, anthropicText(cutAnswer))
	}
	anthropicAnswer := func() reply {
		return anthropicStream("end_turn", anthropicUsage{Input: 30, Output: 5, CacheRead: 2200}, anthropicText(finalAnswer))
	}
	openaiCut := func() reply {
		return openaiStream(openaiUsage{Input: 1000, Cached: 900, Output: 4000}, "max_output_tokens", openaiMessage(cutItem, cutAnswer))
	}
	openaiAnswer := func() reply {
		return openaiStream(openaiUsage{Input: 1000, Cached: 900, Output: 5}, "", openaiMessage(finalItem, finalAnswer))
	}
	for _, c := range []struct {
		name    string
		kind    Kind
		history func() []Message
		replies func() []reply
		// cut is the index of the cut call among the replies; the reply after it answers.
		cut int
	}{
		{name: "anthropic-messages cut on its first call", kind: AnthropicMessages, history: anthropicAnswered,
			replies: func() []reply { return []reply{anthropicCut(), anthropicAnswer()} }},
		{name: "anthropic-messages cut after a tool call", kind: AnthropicMessages, history: anthropicAnswered, cut: 1,
			replies: func() []reply {
				return []reply{
					anthropicStream("tool_use", anthropicUsage{Input: 10, Output: 20, CacheWrite: 2200},
						anthropicThinking("Read the note.", "sig-cut-1+/=="), anthropicToolUse("toolu_1", toolName, map[string]any{"path": "note.txt"})),
					anthropicCut(), anthropicAnswer(),
				}
			}},
		{name: "openai-responses cut on its first call", kind: OpenAIResponses, history: openaiHistory,
			replies: func() []reply { return []reply{openaiCut(), openaiAnswer()} }},
		{name: "openai-responses cut after a tool call", kind: OpenAIResponses, history: openaiHistory, cut: 1,
			replies: func() []reply {
				return []reply{
					openaiStream(openaiUsage{Input: 1000, CacheWrite: 900, Output: 20, Reasoning: 10},
						"", openaiReasoning("rs_c1", "gAAAAcut+/one==", "Reading the note"), openaiToolCall("fc_1", "call_1", toolName, map[string]any{"path": "note.txt"})),
					openaiCut(), openaiAnswer(),
				}
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			run := func(callAgain int) (Result, []recorded, *fixture) {
				t.Helper()
				replies := c.replies()
				s := newStandIn(t, replies...)
				// A call more than the replies, so the call made again is not the run's last
				// allowed one and offers the tool the cut call did.
				f := newFixture(t, s, c.kind, len(replies)+1)
				f.req.History = c.history()
				f.req.Final, f.req.CallAgainWhenCut = json.RawMessage(finalSchema), callAgain
				result, err := Run(context.Background(), f.req)
				if err != nil {
					t.Fatal(err)
				}
				return result, s.requests(), f
			}
			result, got, f := run(1)
			if result.Fault != "" || string(result.Value) != finalAnswer {
				t.Fatalf("fault %q (%s), value %s; want no fault and the reply after the cut's %s", result.Fault, result.Detail, result.Value, finalAnswer)
			}
			calls := c.cut + 2
			if len(got) != calls {
				t.Fatalf("%d model calls, want %d", len(got), calls)
			}
			if again, cut := got[c.cut+1].body, got[c.cut].body; !bytes.Equal(again, cut) {
				t.Errorf("the call made again is\n%s\nwant it byte for byte the cut call's\n%s", again, cut)
			}
			for i, r := range got {
				if bytes.Contains(r.body, []byte(cutSays)) {
					t.Errorf("call %d carries the cut reply", i+1)
				}
			}
			if len(result.Steps) != calls {
				t.Fatalf("%d steps, want every one of the %d calls", len(result.Steps), calls)
			}
			var total Usage
			for i, step := range result.Steps {
				if step.Dropped != (i == c.cut) {
					t.Errorf("step %d dropped %t, want the cut call's step dropped and no other", i+1, step.Dropped)
				}
				total = SumUsage(total, step.Usage)
			}
			if cut := result.Steps[c.cut]; cut.Finish != "length" || cut.Usage.Output != 4000 {
				t.Errorf("the cut step %+v, want its length finish and its 4000 output tokens", cut)
			}
			if result.Usage != total || !result.Usage.Metered {
				t.Errorf("usage %+v, want every call summed, the cut one's included: %+v", result.Usage, total)
			}
			if c.kind == OpenAIResponses {
				for i, want := range map[int]string{c.cut: openaiMessage(cutItem, cutAnswer), c.cut + 1: openaiMessage(finalItem, finalAnswer)} {
					if native := result.Steps[i].Native; len(native) != 1 || string(native[0]) != want {
						t.Errorf("step %d's output items %s, want its own call's %s", i+1, native, want)
					}
				}
			}
			if made := len(f.tool.made()); made != c.cut {
				t.Errorf("the tool was called %d times, want %d", made, c.cut)
			}

			plain, plainSent, _ := run(0)
			if plain.Fault != FaultLength || plain.Value != nil {
				t.Errorf("with no call again: fault %q, value %s; want %q", plain.Fault, plain.Value, FaultLength)
			}
			if len(plainSent) != c.cut+1 || len(plain.Steps) != c.cut+1 {
				t.Fatalf("with no call again: %d model calls and %d steps, want %d, ending on the cut", len(plainSent), len(plain.Steps), c.cut+1)
			}
			for i, r := range plainSent {
				if !bytes.Equal(r.body, got[i].body) {
					t.Errorf("call %d with no call again is\n%s\nwant it byte for byte the run's that calls again\n%s", i+1, r.body, got[i].body)
				}
				if plain.Steps[i].Dropped {
					t.Errorf("with no call again step %d is dropped", i+1)
				}
			}
		})
	}
}

// A cut call is made again no more than the request's CallAgainWhenCut, and only while the run
// has a model call left: a second cut, or a cut on the run's last allowed call, ends the run with
// FaultLength, every call it made reported with its usage and only a call made again dropped.
func TestACutPastCallAgainIsALengthFault(t *testing.T) {
	t.Parallel()
	cut := func() reply {
		return anthropicStream("max_tokens", anthropicUsage{Input: 30, Output: 4000}, anthropicText(cutAnswer))
	}
	answer := func() reply {
		return anthropicStream("end_turn", anthropicUsage{Input: 30, Output: 5}, anthropicText(finalAnswer))
	}
	for _, c := range []struct {
		name    string
		calls   int
		replies []reply
		// dropped is, for each call the run makes, whether its step is dropped.
		dropped []bool
	}{
		{name: "cut twice", calls: 3, replies: []reply{cut(), cut(), answer()}, dropped: []bool{true, false}},
		{name: "cut on the last allowed call", calls: 1, replies: []reply{cut(), answer()}, dropped: []bool{false}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := newStandIn(t, c.replies...)
			f := newFixture(t, s, AnthropicMessages, c.calls)
			f.req.Final, f.req.CallAgainWhenCut = json.RawMessage(finalSchema), 1
			result, err := Run(context.Background(), f.req)
			if err != nil {
				t.Fatal(err)
			}
			if result.Fault != FaultLength || result.Value != nil {
				t.Fatalf("fault %q (%s), value %s; want %q", result.Fault, result.Detail, result.Value, FaultLength)
			}
			if n := len(s.requests()); n != len(c.dropped) {
				t.Errorf("%d model calls, want %d", n, len(c.dropped))
			}
			if len(result.Steps) != len(c.dropped) {
				t.Fatalf("%d steps, want %d", len(result.Steps), len(c.dropped))
			}
			for i, step := range result.Steps {
				if step.Dropped != c.dropped[i] || step.Finish != "length" {
					t.Errorf("step %d dropped %t, finish %q; want dropped %t and length", i+1, step.Dropped, step.Finish, c.dropped[i])
				}
			}
			if u := result.Usage; u.Output != 4000*len(c.dropped) || !u.Metered {
				t.Errorf("usage %+v, want every cut call's 4000 output tokens", u)
			}
		})
	}
}

// outOfFormSays is the sentence of prose a reply out of form writes before the Final object, as
// the one reply of 121 measured without the schema did (doc.go, "The structured reply"), and
// outOfForm that reply. No other reply or request says outOfFormSays, so a request that carries
// it carries that reply.
const outOfFormSays = "Here is my reply to the question:"

var outOfForm = outOfFormSays + " " + finalAnswer

// On the Messages API a reply out of form on a call that offers a tool, which carries no Final
// schema, is no answer and no fault while the run has a model call and time left: its step is
// dropped, as a cut call's is, and the run's next call is its last whatever number it has. That
// call offers no tool, carries the schema in Final's order and Request.LastCall, and is sent the
// messages the dropped call was sent, so none of the dropped reply, its thinking included, goes
// back to the model. Its object is the run's Value, the run has two steps, the first dropped,
// and its usage is both calls'. Each case allows four calls, so the second call is the run's last
// only because the first reply was out of form.
func TestAReplyOutOfFormOnACallWithToolsIsDroppedAndTheNextCallIsTheLast(t *testing.T) {
	t.Parallel()
	for _, kind := range []Kind{AnthropicMessages, AzureAnthropic} {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			s := newStandIn(t,
				anthropicStream("end_turn", anthropicUsage{Input: 10, Output: 20}, anthropicThinking("Answer at once.", "sig-out-of-form+/=="), anthropicText(outOfForm)),
				anthropicStream("end_turn", anthropicUsage{Input: 30, Output: 5}, anthropicText(finalAnswer)),
			)
			f := newFixture(t, s, kind, 4)
			f.req.Final, f.req.LastCall = json.RawMessage(finalSchema), lastCallSentence
			result, err := Run(context.Background(), f.req)
			if err != nil || result.Fault != "" || string(result.Value) != finalAnswer {
				t.Fatalf("result %+v, %v; want no fault and the second reply's %s", result, err, finalAnswer)
			}
			if len(result.Steps) != 2 || !result.Steps[0].Dropped || result.Steps[1].Dropped {
				t.Fatalf("steps %+v, want the reply out of form dropped and the reply after it", result.Steps)
			}
			if u := result.Usage; u != SumUsage(result.Steps[0].Usage, result.Steps[1].Usage) || u.Input != 40 || u.Output != 25 || !u.Metered {
				t.Errorf("usage %+v, want both calls summed, the dropped one's included", u)
			}
			got := s.requests()
			if len(got) != 2 {
				t.Fatalf("%d model calls, want 2: the call out of form and the last", len(got))
			}
			if _, carried := valueAt(got[0].body, finalSchemaPath(kind)); carried {
				t.Error("the first call, which offers the tool, carries the schema")
			}
			last := got[1].decoded(t)
			if choice, _ := last["tool_choice"].(map[string]any); choice["type"] != "none" {
				t.Errorf("the call after the reply out of form has the tool choice %v, want none", last["tool_choice"])
			}
			if schema, carried := valueAt(got[1].body, finalSchemaPath(kind)); !carried {
				t.Error("the call after the reply out of form carries no schema")
			} else {
				holdsFinalsOrder(t, schema, json.RawMessage(finalSchema), "the last call's schema")
			}
			for _, said := range []string{outOfFormSays, "Answer at once.", "sig-out-of-form+/=="} {
				if bytes.Contains(got[1].body, []byte(said)) {
					t.Errorf("the call after the reply out of form carries %q of that reply", said)
				}
			}
			if sent, want := withoutLastCall(t, kind, last)["messages"], got[0].decoded(t)["messages"]; !reflect.DeepEqual(sent, want) {
				t.Errorf("the last call's messages less its sentence are\n%v\nwant the dropped call's\n%v", sent, want)
			}
			if made := len(f.tool.made()); made != 0 {
				t.Errorf("the tool was called %d times, want none", made)
			}
		})
	}
}

// A reply out of form on a call that carried the schema is FaultMalformed as before: on the
// run's last allowed call, the only way a run has no model call left; on the call made after a
// reply out of form, which is the run's last; on openai-responses, every call of which carries
// the schema; and on a run with no tools, which offers a tool on no call. Every call the run made
// is reported with its usage, and only a reply out of form on a call that offered a tool is
// dropped.
func TestAReplyOutOfFormOnACallThatCarriedTheSchemaIsMalformed(t *testing.T) {
	t.Parallel()
	anthropicOut := func() reply {
		return anthropicStream("end_turn", anthropicUsage{Input: 10, Output: 20}, anthropicText(outOfForm))
	}
	anthropicAnswer := func() reply {
		return anthropicStream("end_turn", anthropicUsage{Input: 30, Output: 5}, anthropicText(finalAnswer))
	}
	for _, c := range []struct {
		name    string
		kind    Kind
		calls   int
		noTools bool
		replies []reply
		// dropped is, for each call the run makes, whether its step is dropped.
		dropped []bool
	}{
		{name: "out of form on the last allowed call", kind: AnthropicMessages, calls: 2, dropped: []bool{false, false}, replies: []reply{
			anthropicStream("tool_use", anthropicUsage{Input: 10, Output: 5}, anthropicToolUse("toolu_1", toolName, map[string]any{"path": "note.txt"})),
			anthropicOut(), anthropicAnswer(),
		}},
		{name: "out of form again on the call after it", kind: AnthropicMessages, calls: 4, dropped: []bool{true, false},
			replies: []reply{anthropicOut(), anthropicOut(), anthropicAnswer()}},
		{name: "openai-responses", kind: OpenAIResponses, calls: 4, dropped: []bool{false}, replies: []reply{
			openaiStream(openaiUsage{Input: 100, Output: 20}, "", openaiMessage("msg_1", outOfForm)),
			openaiStream(openaiUsage{Input: 120, Output: 5}, "", openaiMessage("msg_2", finalAnswer)),
		}},
		{name: "anthropic-messages with no tools", kind: AnthropicMessages, calls: 4, noTools: true, dropped: []bool{false},
			replies: []reply{anthropicOut(), anthropicAnswer()}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := newStandIn(t, c.replies...)
			f := newFixture(t, s, c.kind, c.calls)
			f.req.Final, f.req.LastCall = json.RawMessage(finalSchema), lastCallSentence
			if c.noTools {
				f.req.Tools = nil
			}
			result, err := Run(context.Background(), f.req)
			if err != nil {
				t.Fatal(err)
			}
			if result.Fault != FaultMalformed || result.Value != nil {
				t.Fatalf("fault %q (%s), value %s; want %q", result.Fault, result.Detail, result.Value, FaultMalformed)
			}
			if n := len(s.requests()); n != len(c.dropped) {
				t.Errorf("%d model calls, want %d", n, len(c.dropped))
			}
			if len(result.Steps) != len(c.dropped) {
				t.Fatalf("%d steps, want %d", len(result.Steps), len(c.dropped))
			}
			var total Usage
			for i, step := range result.Steps {
				if step.Dropped != c.dropped[i] {
					t.Errorf("step %d dropped %t, want %t", i+1, step.Dropped, c.dropped[i])
				}
				total = SumUsage(total, step.Usage)
			}
			if result.Usage != total || !result.Usage.Metered {
				t.Errorf("usage %+v, want every call summed: %+v", result.Usage, total)
			}
		})
	}
}

// A dropped reply out of form leaves a cut reply and a refused Value as they were, within
// Bounds.Calls: a cut call is made again and a reply Request.Check refuses is asked for again
// before a reply out of form, neither counting it nor counted by it, and the call after the
// reply out of form is the run's last though the bound, AskAgain and CallAgainWhenCut would
// allow more, so a Value Check refuses on it is the run's, with no fault, and a reply cut on it
// is FaultLength, as on a run's last allowed call. Only the last call carries Request.LastCall,
// and no call carries the cut reply or the reply out of form.
func TestADroppedReplyOutOfFormLeavesACutAndARefusalAsTheyWere(t *testing.T) {
	t.Parallel()
	cut := func() reply {
		return anthropicStream("max_tokens", anthropicUsage{Input: 30, Output: 4000}, anthropicText(cutAnswer))
	}
	text := func(text string) reply {
		return anthropicStream("end_turn", anthropicUsage{Input: 10, Output: 5}, anthropicText(text))
	}
	for _, c := range []struct {
		name    string
		calls   int
		check   func(json.RawMessage) error
		replies []reply
		fault   Fault
		value   string
		// dropped and asked are, for each call the run makes, whether its step is dropped
		// and whether it was asked again.
		dropped, asked []bool
	}{
		{name: "a cut and a refusal before the reply out of form", calls: 6, check: refuseEmptyAnswer,
			replies: []reply{cut(), text(refusedAnswer), text(outOfForm), text(finalAnswer)}, value: finalAnswer,
			dropped: []bool{true, false, true, false}, asked: []bool{false, true, false, false}},
		{name: "a refusal after the reply out of form", calls: 4, check: refuseEveryAnswer,
			replies: []reply{text(outOfForm), text(refusedAnswer), text(finalAnswer)}, value: refusedAnswer,
			dropped: []bool{true, false}, asked: []bool{false, false}},
		{name: "a cut after the reply out of form", calls: 4,
			replies: []reply{text(outOfForm), cut(), text(finalAnswer)}, fault: FaultLength,
			dropped: []bool{true, false}, asked: []bool{false, false}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := newStandIn(t, c.replies...)
			f := newFixture(t, s, AnthropicMessages, c.calls)
			f.req.Final, f.req.LastCall = json.RawMessage(finalSchema), lastCallSentence
			f.req.Check, f.req.AskAgain, f.req.CallAgainWhenCut = c.check, 1, 1
			result, err := Run(context.Background(), f.req)
			if err != nil || result.Fault != c.fault || string(result.Value) != c.value || (c.fault != "" && result.Value != nil) {
				t.Fatalf("result %+v, %v; want fault %q and the value %s", result, err, c.fault, c.value)
			}
			if len(result.Steps) != len(c.dropped) {
				t.Fatalf("%d steps, want %d: %+v", len(result.Steps), len(c.dropped), result.Steps)
			}
			var total Usage
			for i, step := range result.Steps {
				if step.Dropped != c.dropped[i] || (step.AskedAgain == askAgainSentence) != c.asked[i] {
					t.Errorf("step %d dropped %t and asked again with %q; want dropped %t and asked again %t", i+1, step.Dropped, step.AskedAgain, c.dropped[i], c.asked[i])
				}
				total = SumUsage(total, step.Usage)
			}
			if result.Usage != total || !result.Usage.Metered {
				t.Errorf("usage %+v, want every call summed: %+v", result.Usage, total)
			}
			got := s.requests()
			if len(got) != len(c.dropped) {
				t.Fatalf("%d model calls, want %d", len(got), len(c.dropped))
			}
			for i, r := range got {
				if last := i == len(got)-1; bytes.Contains(r.body, []byte(lastCallSentence)) != last {
					t.Errorf("call %d carries the last-call sentence: %t, want it on the run's last call alone", i+1, !last)
				}
				for _, said := range []string{cutSays, outOfFormSays} {
					if bytes.Contains(r.body, []byte(said)) {
						t.Errorf("call %d carries %q", i+1, said)
					}
				}
			}
			last := got[len(got)-1].decoded(t)
			if choice, _ := last["tool_choice"].(map[string]any); choice["type"] != "none" {
				t.Errorf("the last call has the tool choice %v, want none", last["tool_choice"])
			}
			if _, carried := valueAt(got[len(got)-1].body, finalSchemaPath(AnthropicMessages)); !carried {
				t.Error("the last call carries no schema")
			}
		})
	}
}

// A reply the model refused is a refusal fault, on each protocol's own way of saying so, and
// whether or not the request asked for a Final object: a refusal is why the run ended, not a
// reply of the wrong form.
func TestARefusedReplyIsAFault(t *testing.T) {
	t.Parallel()
	refusal := `{"id":"msg_r","type":"message","role":"assistant","status":"completed","content":[{"type":"refusal","refusal":"I will not do that."}]}`
	for _, c := range []struct {
		name  string
		kind  Kind
		reply reply
	}{
		{name: "an Anthropic refusal stop reason", kind: AnthropicMessages, reply: anthropicStream("refusal", anthropicUsage{Output: 1}, anthropicText(""))},
		{name: "an OpenAI Responses refusal part", kind: OpenAIResponses, reply: openaiStream(openaiUsage{Output: 1}, "", refusal)},
		{name: "a Chat Completions refusal delta", kind: OpenAIChat, reply: chatRefusalStream("I will not do that.", false)},
		{name: "a Chat Completions refusal message", kind: OpenAIChat, reply: chatRefusalStream("I will not do that.", true)},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			for _, final := range []struct {
				name  string
				value json.RawMessage
			}{
				{name: "with free text asked for"},
				{name: "with a final object asked for", value: json.RawMessage(finalSchema)},
			} {
				t.Run(final.name, func(t *testing.T) {
					t.Parallel()
					s := newStandIn(t, c.reply)
					f := newFixture(t, s, c.kind, 1)
					f.req.Final = final.value
					result, err := Run(context.Background(), f.req)
					if err != nil {
						t.Fatal(err)
					}
					if result.Fault != FaultRefusal {
						t.Errorf("fault %q, want %q: %s", result.Fault, FaultRefusal, result.Detail)
					}
				})
			}
		})
	}
}

// A last reply that is not exactly one object of the request's form is malformed, and one that is
// comes back as the value.
func TestALastReplyThatIsNotTheFinalObjectIsMalformed(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		text string
		want Fault
	}{
		{name: "prose", text: "The note says plum.", want: FaultMalformed},
		{name: "two objects", text: finalAnswer + finalAnswer, want: FaultMalformed},
		{name: "an object the form refuses", text: `{"answer":"plum","aside":"and a pear"}`, want: FaultMalformed},
		{name: "the object itself", text: finalAnswer},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := newStandIn(t, anthropicStream("end_turn", anthropicUsage{Output: 5}, anthropicText(c.text)))
			f := newFixture(t, s, AnthropicMessages, 1)
			f.req.Final = json.RawMessage(finalSchema)
			result, err := Run(context.Background(), f.req)
			if err != nil {
				t.Fatal(err)
			}
			if result.Fault != c.want {
				t.Fatalf("fault %q, want %q: %s", result.Fault, c.want, result.Detail)
			}
			switch {
			case c.want != "" && result.Value != nil:
				t.Errorf("the malformed reply gave the value %s", result.Value)
			case c.want == "" && string(result.Value) != finalAnswer:
				t.Errorf("the value is %s, want %s", result.Value, finalAnswer)
			}
		})
	}
}

// askAgainSentence is the sentence the tests' checks refuse a reply with, which the run sends the
// model as it asks for the reply again.
const askAgainSentence = "The answer names no fruit: name the fruit the note names."

// refusedAnswer is a reply of finalSchema's form that refuseEmptyAnswer refuses.
const refusedAnswer = `{"answer":""}`

// refuseEmptyAnswer is a Check of finalSchema's object: an answer that names nothing is refused,
// which is a rule the strict subset cannot state.
func refuseEmptyAnswer(value json.RawMessage) error {
	var reply struct {
		Answer string `json:"answer"`
	}
	if err := json.Unmarshal(value, &reply); err != nil {
		return err
	}
	if reply.Answer == "" {
		return errors.New(askAgainSentence)
	}
	return nil
}

// refuseEveryAnswer is a Check that refuses whatever it is given.
func refuseEveryAnswer(json.RawMessage) error { return errors.New(askAgainSentence) }

// An Anthropic reply the request's Check refuses is answered with the check's sentence as a user
// message and asked for again in the same run: the second call carries the conversation, the
// refused reply with its signed thinking as the provider returned it and the sentence after it,
// marks the history as the first call did and the sentence as its newest message, four marks in
// all, and offers no tool, being the run's last allowed call. The second reply is the value, the
// refused step carries the sentence, and the usage sums both calls.
func TestAnAnthropicReplyTheCheckRefusesIsAskedForAgain(t *testing.T) {
	t.Parallel()
	s := newStandIn(t,
		anthropicStream("end_turn", anthropicUsage{Input: 10, Output: 20, CacheWrite: 2200},
			anthropicThinking("Weigh the note.", "sig-ask-1+/=="), anthropicText(refusedAnswer)),
		anthropicStream("end_turn", anthropicUsage{Input: 30, Output: 5, CacheRead: 2200}, anthropicText(finalAnswer)),
	)
	f := newFixture(t, s, AnthropicMessages, 2)
	f.req.History = anthropicAnswered()
	f.req.Final, f.req.Check, f.req.AskAgain = json.RawMessage(finalSchema), refuseEmptyAnswer, 1
	result, err := Run(context.Background(), f.req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Fault != "" || string(result.Value) != finalAnswer {
		t.Fatalf("fault %q (%s), value %s; want no fault and the second reply's %s", result.Fault, result.Detail, result.Value, finalAnswer)
	}
	if len(result.Steps) != 2 || result.Steps[0].AskedAgain != askAgainSentence || result.Steps[1].AskedAgain != "" {
		t.Fatalf("steps %+v, want the refused reply carrying the sentence and the reply after it", result.Steps)
	}
	if u := result.Usage; u.Input != 40 || u.Output != 25 || u.CacheWrite != 2200 || u.CacheRead != 2200 || !u.Metered {
		t.Errorf("usage %+v, want both calls summed", u)
	}
	got := s.requests()
	if len(got) != 2 {
		t.Fatalf("%d model calls, want 2", len(got))
	}
	// The second call's messages: the history's two exchanges (0 to 3), the question (4),
	// the refused reply (5) and the check's sentence (6).
	sent := messages(t, got[1])
	if len(sent) != 7 {
		t.Fatalf("the second call carries %d messages, want 7", len(sent))
	}
	refused := blocks(sent[5])
	if sent[5]["role"] != "assistant" || len(refused) != 2 || refused[0]["type"] != "thinking" ||
		refused[0]["signature"] != "sig-ask-1+/==" || refused[0]["thinking"] != "Weigh the note." || refused[1]["text"] != refusedAnswer {
		t.Errorf("the second call carries the refused reply as %v, want its signed thinking and its text", sent[5])
	}
	if asked := blocks(sent[6]); sent[6]["role"] != "user" || len(asked) != 1 || asked[0]["text"] != askAgainSentence {
		t.Errorf("the second call's newest message is %v, want the check's sentence", sent[6])
	}
	wantMarks := [][]int{{1, 2, 3, 4}, {1, 2, 3, 6}}
	for i, r := range got {
		body := r.decoded(t)
		if n := countKey(body, "cache_control"); n != len(wantMarks[i]) {
			t.Errorf("call %d carries %d cache marks, want %d (never above 4)", i+1, n, len(wantMarks[i]))
		}
		if marks := marked(t, r); !reflect.DeepEqual(marks, wantMarks[i]) {
			t.Errorf("call %d marks messages %v, want %v", i+1, marks, wantMarks[i])
		}
		choice, _ := body["tool_choice"].(map[string]any)
		if last := i == 1; (choice["type"] == "none") != last {
			t.Errorf("call %d's tool_choice %v; want none on the run's last allowed call only", i+1, body["tool_choice"])
		}
	}
}

// An OpenAI Responses reply the request's Check refuses is asked for again the same way: the
// second call carries the refused reply's reasoning item with its encrypted content byte for
// byte, the reply's own text, and the check's sentence as the newest input item, and each step
// reports its own call's output items.
func TestAnOpenAIResponsesReplyTheCheckRefusesIsAskedForAgain(t *testing.T) {
	t.Parallel()
	stepOne := []string{openaiReasoning("rs_a1", "gAAAAask+/one==", "Weighing the note"), openaiMessage("msg_a1", refusedAnswer)}
	stepTwo := []string{openaiMessage("msg_a2", finalAnswer)}
	s := newStandIn(t,
		openaiStream(openaiUsage{Input: 1000, CacheWrite: 900, Output: 40, Reasoning: 20}, "", stepOne...),
		openaiStream(openaiUsage{Input: 1100, Cached: 900, Output: 10}, "", stepTwo...),
	)
	f := newFixture(t, s, OpenAIResponses, 2)
	f.req.Final, f.req.Check, f.req.AskAgain = json.RawMessage(finalSchema), refuseEmptyAnswer, 1
	result, err := Run(context.Background(), f.req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Fault != "" || string(result.Value) != finalAnswer {
		t.Fatalf("fault %q (%s), value %s; want no fault and the second reply's %s", result.Fault, result.Detail, result.Value, finalAnswer)
	}
	if len(result.Steps) != 2 || result.Steps[0].AskedAgain != askAgainSentence || result.Steps[1].AskedAgain != "" {
		t.Fatalf("steps %+v, want the refused reply carrying the sentence and the reply after it", result.Steps)
	}
	for i, want := range [][]string{stepOne, stepTwo} {
		var native []string
		for _, item := range result.Steps[i].Native {
			native = append(native, string(item))
		}
		if !reflect.DeepEqual(native, want) {
			t.Errorf("step %d's output items\n%v\nwant them as the API sent them\n%v", i+1, native, want)
		}
	}
	if u, steps := result.Usage, SumUsage(result.Steps[0].Usage, result.Steps[1].Usage); u != steps || u.Output != 50 || u.Reasoning != 20 || !u.Metered {
		t.Errorf("usage %+v, want both calls summed: %+v", u, steps)
	}
	got := s.requests()
	if len(got) != 2 {
		t.Fatalf("%d model calls, want 2", len(got))
	}
	if !bytes.Contains(got[1].body, []byte(`"encrypted_content":"gAAAAask+/one=="`)) {
		t.Error("the second call does not carry the refused reply's reasoning byte for byte")
	}
	if !bytes.Contains(got[1].body, []byte(quote(refusedAnswer))) {
		t.Error("the second call does not carry the refused reply's text")
	}
	items := inputItems(t, got[1])
	newest := items[len(items)-1]
	content, _ := newest["content"].([]any)
	if newest["role"] != "user" || len(content) != 1 || content[0].(map[string]any)["text"] != askAgainSentence {
		t.Errorf("the second call's newest input item is %v, want the check's sentence", newest)
	}
	for i, r := range got {
		if last := i == 1; (r.decoded(t)["tool_choice"] == "none") != last {
			t.Errorf("call %d's tool_choice %v; want none on the run's last allowed call only", i+1, r.decoded(t)["tool_choice"])
		}
	}
}

// The calls a run makes before and after it asks again are one run's: the tool calls are numbered
// on from the request's first index across the ask, the ask's tool results and the sentence ride
// in the order they were made, and only the last of the run's allowed calls, made after the ask,
// offers no tool.
func TestAnAskAgainNumbersToolCallsOnAndOffersNoToolBeforeTheRunsLastCall(t *testing.T) {
	t.Parallel()
	s := newStandIn(t,
		anthropicStream("tool_use", anthropicUsage{Output: 1}, anthropicToolUse("toolu_1", toolName, map[string]any{"path": "note.txt"})),
		anthropicStream("end_turn", anthropicUsage{Output: 1}, anthropicText(refusedAnswer)),
		anthropicStream("tool_use", anthropicUsage{Output: 1}, anthropicToolUse("toolu_2", toolName, map[string]any{"path": "other.txt"})),
		anthropicStream("end_turn", anthropicUsage{Output: 1}, anthropicText(finalAnswer)),
	)
	f := newFixture(t, s, AnthropicMessages, 4)
	f.req.FirstToolIndex = 2
	f.req.Final, f.req.Check, f.req.AskAgain = json.RawMessage(finalSchema), refuseEmptyAnswer, 1
	result, err := Run(context.Background(), f.req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Fault != "" || string(result.Value) != finalAnswer || len(result.Steps) != 4 || result.Steps[1].AskedAgain != askAgainSentence {
		t.Fatalf("result %+v, want the answer after four calls, the second asked again", result)
	}
	want := []seen{{3, "toolu_1", `{"path":"note.txt"}`}, {4, "toolu_2", `{"path":"other.txt"}`}}
	if calls := madeSeen(f.tool.made()); !reflect.DeepEqual(calls, want) {
		t.Errorf("the tool was given %+v, want %+v", calls, want)
	}
	got := s.requests()
	if len(got) != 4 {
		t.Fatalf("%d model calls, want 4", len(got))
	}
	// The last call's messages: the question (0), the first tool call and its result (1, 2),
	// the refused reply and the sentence (3, 4), the second tool call and its result (5, 6),
	// the newest of which alone is marked.
	var roles []string
	for _, m := range messages(t, got[3]) {
		roles = append(roles, m["role"].(string))
	}
	if strings.Join(roles, ",") != "user,assistant,user,assistant,user,assistant,user" {
		t.Errorf("the last call's roles %v", roles)
	}
	if asked := blocks(messages(t, got[3])[4]); asked[0]["text"] != askAgainSentence {
		t.Errorf("the last call carries %v where the sentence was sent", asked)
	}
	for i, r := range got {
		body := r.decoded(t)
		if marks := marked(t, r); len(marks) != 1 || marks[0] != len(messages(t, r))-1 {
			t.Errorf("call %d marks messages %v, want its newest alone", i+1, marks)
		}
		choice, _ := body["tool_choice"].(map[string]any)
		if last := i == 3; (choice["type"] == "none") != last {
			t.Errorf("call %d's tool_choice %v; want none on the run's last allowed call only", i+1, body["tool_choice"])
		}
	}
}

// A check that refuses the reply it is given again is asked again no more than the request's
// AskAgain: the run returns the last reply as its value, with no fault, and leaves refusing it to
// the caller, though the call bound would allow another call.
func TestACheckRefusingPastAskAgainReturnsTheLastReply(t *testing.T) {
	t.Parallel()
	const pear = `{"answer":"pear"}`
	s := newStandIn(t,
		anthropicStream("end_turn", anthropicUsage{Output: 1}, anthropicText(refusedAnswer)),
		anthropicStream("end_turn", anthropicUsage{Output: 1}, anthropicText(pear)),
		anthropicStream("end_turn", anthropicUsage{Output: 1}, anthropicText(finalAnswer)),
	)
	f := newFixture(t, s, AnthropicMessages, 3)
	f.req.Final, f.req.Check, f.req.AskAgain = json.RawMessage(finalSchema), refuseEveryAnswer, 1
	result, err := Run(context.Background(), f.req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Fault != "" || string(result.Value) != pear {
		t.Fatalf("fault %q (%s), value %s; want no fault and the last reply's %s", result.Fault, result.Detail, result.Value, pear)
	}
	if len(result.Steps) != 2 || result.Steps[0].AskedAgain != askAgainSentence || result.Steps[1].AskedAgain != "" {
		t.Errorf("steps %+v, want the one ask and the reply to it", result.Steps)
	}
	if n := len(s.requests()); n != 2 {
		t.Errorf("%d model calls, want 2: one ask and the one ask again", n)
	}
}

// A refused reply on the run's last allowed call is not asked for again: Bounds.Calls counts the
// tool call before it, no further call is made, and the run returns the reply as its value with
// no fault.
func TestACheckWithNoModelCallLeftReturnsTheLastReply(t *testing.T) {
	t.Parallel()
	s := newStandIn(t,
		anthropicStream("tool_use", anthropicUsage{Output: 1}, anthropicToolUse("toolu_1", toolName, map[string]any{"path": "note.txt"})),
		anthropicStream("end_turn", anthropicUsage{Output: 1}, anthropicText(refusedAnswer)),
		anthropicStream("end_turn", anthropicUsage{Output: 1}, anthropicText(finalAnswer)),
	)
	f := newFixture(t, s, AnthropicMessages, 2)
	f.req.Final, f.req.Check, f.req.AskAgain = json.RawMessage(finalSchema), refuseEveryAnswer, 1
	result, err := Run(context.Background(), f.req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Fault != "" || string(result.Value) != refusedAnswer {
		t.Fatalf("fault %q (%s), value %s; want no fault and the refused reply's %s", result.Fault, result.Detail, result.Value, refusedAnswer)
	}
	if len(result.Steps) != 2 || result.Steps[1].AskedAgain != "" {
		t.Errorf("steps %+v, want the two calls the bound allows, neither asked again", result.Steps)
	}
	if n := len(s.requests()); n != 2 {
		t.Errorf("%d model calls, want the 2 the bound allows", n)
	}
}

// The last allowed model call still calling a tool is past the run's call bound, with the call it
// made reported.
func TestTheLastAllowedCallStillCallingAToolIsPastTheCallBound(t *testing.T) {
	t.Parallel()
	s := newStandIn(t, anthropicStream("tool_use", anthropicUsage{Output: 1}, anthropicToolUse("toolu_1", toolName, map[string]any{"path": "note.txt"})))
	result, err := Run(context.Background(), newFixture(t, s, AnthropicMessages, 1).req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Fault != FaultCalls || len(result.Steps) != 1 {
		t.Errorf("result %+v, want %q after the one call it was allowed: %s", result, FaultCalls, result.Detail)
	}
}

// lastCallSentence is the sentence the tests' requests tell the run's last call with
// (Request.LastCall).
const lastCallSentence = "This is your last call, and it offers no tool: answer now from what you know."

// newestPart is what a request body ends with: on anthropic-messages the last block of its last
// message, where the provider puts a user message after the tool results of the same turn, and
// on openai-responses its last input item. A tool result reads "tool_result", a user text its
// text, and on anthropic-messages a user text carrying a cache mark reads "marked: " and its text.
func newestPart(t *testing.T, kind Kind, body map[string]any) string {
	t.Helper()
	if kind == OpenAIResponses {
		items, _ := body["input"].([]any)
		item, _ := items[len(items)-1].(map[string]any)
		content, _ := item["content"].([]any)
		switch {
		case item["type"] == "function_call_output":
			return "tool_result"
		case item["role"] == "user" && len(content) == 1:
			text, _ := content[0].(map[string]any)["text"].(string)
			return text
		}
		t.Fatalf("the request ends with %v, neither a tool result nor a user text", item)
	}
	messages, _ := body["messages"].([]any)
	message, _ := messages[len(messages)-1].(map[string]any)
	content, _ := message["content"].([]any)
	block, _ := content[len(content)-1].(map[string]any)
	switch {
	case block["type"] == "tool_result":
		return "tool_result"
	case message["role"] == "user" && block["type"] == "text" && block["cache_control"] != nil:
		return "marked: " + block["text"].(string)
	case message["role"] == "user" && block["type"] == "text":
		return block["text"].(string)
	}
	t.Fatalf("the request ends with %v, neither a tool result nor a user text", block)
	return ""
}

// withoutLastCall is a request body that ends with the last-call sentence as it would read
// without it: the sentence's block taken off its message on anthropic-messages, the message with
// it where the sentence was all it held, and its input item off openai-responses.
func withoutLastCall(t *testing.T, kind Kind, body map[string]any) map[string]any {
	t.Helper()
	if newest := newestPart(t, kind, body); newest != lastCallSentence {
		t.Fatalf("the request ends with %q, want the last-call sentence, unmarked", newest)
	}
	if kind == OpenAIResponses {
		items := body["input"].([]any)
		body["input"] = items[:len(items)-1]
		return body
	}
	messages := body["messages"].([]any)
	message := messages[len(messages)-1].(map[string]any)
	content := message["content"].([]any)
	if len(content) == 1 {
		body["messages"] = messages[:len(messages)-1]
	} else {
		message["content"] = content[:len(content)-1]
	}
	return body
}

// The run's last allowed call alone carries Request.LastCall, as a user message after everything
// else it carries, and offers no tool; with LastCall empty every request is the request the run
// sent before the field existed. Each case is one script run twice, with the sentence and
// without, on both protocols the Legion daemon calls: a run whose model calls the tool until the
// bound, so its last call carries the sentence after the tool results; a run of one call, the
// run's first and its last, which carries it after the question; and a run whose last call asks
// again, which carries it after the check's sentence. Every request but the last is byte for byte
// the run's without the sentence, which none of that run's requests holds; the last is that run's
// request with the sentence after its end and nothing else changed, its cache marks included
// (never more than four); and the Result is the same, no step holding the sentence.
func TestOnlyTheLastCallCarriesTheLastCallSentence(t *testing.T) {
	t.Parallel()
	openaiTools := func() []reply {
		return []reply{
			openaiStream(openaiUsage{Input: 100, Output: 5}, "", openaiToolCall("fc_1", "call_1", toolName, map[string]any{"path": "note.txt"})),
			openaiStream(openaiUsage{Input: 120, Cached: 100, Output: 5}, "", openaiToolCall("fc_2", "call_2", toolName, map[string]any{"path": "other.txt"})),
			openaiStream(openaiUsage{Input: 140, Cached: 120, Output: 8}, "", openaiMessage("msg_3", "The note says plum.")),
		}
	}
	for _, c := range []struct {
		name    string
		kind    Kind
		calls   int
		history func() []Message
		replies func() []reply
		check   bool
		// after is what the last call's request ends with before the sentence (newestPart).
		after string
	}{
		{name: "anthropic-messages calling the tool to the bound", kind: AnthropicMessages, calls: 3, history: anthropicHistory, replies: threeCallAnthropic, after: "tool_result"},
		{name: "anthropic-messages with one call", kind: AnthropicMessages, calls: 1, history: anthropicHistory,
			replies: func() []reply {
				return []reply{anthropicStream("end_turn", anthropicUsage{Input: 10, Output: 5}, anthropicText("The note says plum."))}
			}, after: "marked: What does note.txt say?"},
		{name: "anthropic-messages asking again on the last call", kind: AnthropicMessages, calls: 2, history: anthropicAnswered, check: true,
			replies: func() []reply {
				return []reply{
					anthropicStream("end_turn", anthropicUsage{Input: 10, Output: 20, CacheWrite: 2200}, anthropicThinking("Weigh the note.", "sig-ask-1+/=="), anthropicText(refusedAnswer)),
					anthropicStream("end_turn", anthropicUsage{Input: 30, Output: 5, CacheRead: 2200}, anthropicText(finalAnswer)),
				}
			}, after: "marked: " + askAgainSentence},
		{name: "openai-responses calling the tool to the bound", kind: OpenAIResponses, calls: 3, history: openaiHistory, replies: openaiTools, after: "tool_result"},
		{name: "openai-responses with one call", kind: OpenAIResponses, calls: 1, history: openaiHistory,
			replies: func() []reply {
				return []reply{openaiStream(openaiUsage{Input: 100, Output: 5}, "", openaiMessage("msg_1", "The note says plum."))}
			}, after: "What does note.txt say?"},
		{name: "openai-responses asking again on the last call", kind: OpenAIResponses, calls: 2, history: openaiHistory, check: true,
			replies: func() []reply {
				return []reply{
					openaiStream(openaiUsage{Input: 1000, CacheWrite: 900, Output: 40}, "", openaiMessage("msg_a1", refusedAnswer)),
					openaiStream(openaiUsage{Input: 1100, Cached: 900, Output: 10}, "", openaiMessage("msg_a2", finalAnswer)),
				}
			}, after: askAgainSentence},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			run := func(lastCall string) (Result, []recorded) {
				t.Helper()
				s := newStandIn(t, c.replies()...)
				f := newFixture(t, s, c.kind, c.calls)
				f.req.History = c.history()
				f.req.LastCall = lastCall
				if c.check {
					f.req.Final, f.req.Check, f.req.AskAgain = json.RawMessage(finalSchema), refuseEmptyAnswer, 1
				}
				result, err := Run(context.Background(), f.req)
				if err != nil {
					t.Fatal(err)
				}
				got := s.requests()
				if len(got) != c.calls {
					t.Fatalf("%d model calls, want %d", len(got), c.calls)
				}
				return result, got
			}
			plain, plainSent := run("")
			told, toldSent := run(lastCallSentence)
			for i, r := range plainSent {
				if bytes.Contains(r.body, []byte(lastCallSentence)) {
					t.Errorf("call %d of the run with no LastCall carries the sentence", i+1)
				}
			}
			last := c.calls - 1
			for i := range last {
				if !bytes.Equal(toldSent[i].body, plainSent[i].body) {
					t.Errorf("call %d, not the last, is\n%s\nwant it byte for byte the run's with no LastCall\n%s", i+1, toldSent[i].body, plainSent[i].body)
				}
			}
			body := toldSent[last].decoded(t)
			if n := countKey(body, "cache_control"); n > 4 {
				t.Errorf("the last call carries %d cache marks, more than the four Anthropic allows", n)
			}
			switch choice := body["tool_choice"]; {
			case c.kind == OpenAIResponses && choice != "none":
				t.Errorf("the last call's tool_choice %v, want none", choice)
			case c.kind == AnthropicMessages && choice.(map[string]any)["type"] != "none":
				t.Errorf("the last call's tool_choice %v, want none", choice)
			}
			stripped := withoutLastCall(t, c.kind, body)
			if before := newestPart(t, c.kind, stripped); before != c.after {
				t.Errorf("the last call carries the sentence after %q, want after %q", before, c.after)
			}
			if want := plainSent[last].decoded(t); !reflect.DeepEqual(stripped, want) {
				t.Errorf("the last call less its sentence is\n%v\nwant the run's with no LastCall\n%v", stripped, want)
			}
			if !reflect.DeepEqual(told, plain) {
				t.Errorf("the result %+v, want the run's with no LastCall %+v", told, plain)
			}
			if c.check && (len(told.Steps) != 2 || told.Steps[0].AskedAgain != askAgainSentence || string(told.Value) != finalAnswer) {
				t.Errorf("steps %+v, value %s; want the refused reply asked again and the reply to it", told.Steps, told.Value)
			}
		})
	}
}

// Every call of a run before its last allowed call offers the run's tools with the protocol's own
// auto tool choice, which leaves the model to call a tool or to answer, and never a forced one
// (Anthropic's any or tool, OpenAI's required or a named function), which would have it call a
// tool on a call it could answer on, and none carries Request.LastCall. On the Messages API
// (anthropic-messages and azure-anthropic) such a call carries no Request.Final schema, so the
// model is not held to the reply object on a call it may make a tool call on, and keeps the rest
// of output_config, the model's effort; on openai-responses it carries the schema at the
// protocol's path (finalSchemaPath), as every call does there (doc.go, "The structured reply").
// A reply on such a call that is the Final object ends the run with that object on that call.
// Each case is a run of three allowed calls whose model calls the tool on its first call and
// answers on its second, so neither is the last allowed call; the last call's own request is
// TestTheMessagesAPISendsTheSchemaOnlyOnACallThatOffersNoTool's and
// TestOnlyTheLastCallCarriesTheLastCallSentence's.
func TestEveryCallBeforeTheLastLeavesTheToolChoiceToTheModelAndCarriesNoSchemaOnTheMessagesAPI(t *testing.T) {
	t.Parallel()
	var final any
	if err := json.Unmarshal([]byte(finalSchema), &final); err != nil {
		t.Fatal(err)
	}
	anthropicReplies := func() []reply {
		return []reply{
			anthropicStream("tool_use", anthropicUsage{Input: 10, Output: 5}, anthropicToolUse("toolu_1", toolName, map[string]any{"path": "note.txt"})),
			anthropicStream("end_turn", anthropicUsage{Input: 20, Output: 5}, anthropicText(finalAnswer)),
		}
	}
	for _, c := range []struct {
		kind    Kind
		replies []reply
		// auto is the tool choice as the protocol writes its own auto.
		auto any
	}{
		{kind: AnthropicMessages, auto: map[string]any{"type": "auto"}, replies: anthropicReplies()},
		{kind: AzureAnthropic, auto: map[string]any{"type": "auto"}, replies: anthropicReplies()},
		{kind: OpenAIResponses, auto: "auto", replies: []reply{
			openaiStream(openaiUsage{Input: 100, Output: 5}, "", openaiToolCall("fc_1", "call_1", toolName, map[string]any{"path": "note.txt"})),
			openaiStream(openaiUsage{Input: 120, Output: 5}, "", openaiMessage("msg_2", finalAnswer)),
		}},
	} {
		t.Run(string(c.kind), func(t *testing.T) {
			t.Parallel()
			s := newStandIn(t, c.replies...)
			f := newFixture(t, s, c.kind, 3)
			f.req.Final, f.req.LastCall = json.RawMessage(finalSchema), lastCallSentence
			result, err := Run(context.Background(), f.req)
			if err != nil || result.Fault != "" || string(result.Value) != finalAnswer || len(result.Steps) != 2 || result.Steps[0].Dropped || result.Steps[1].Dropped {
				t.Fatalf("result %+v, %v; want the answer on the second of three allowed calls, no step dropped", result, err)
			}
			got := s.requests()
			if len(got) != 2 {
				t.Fatalf("%d model calls, want 2", len(got))
			}
			for i, r := range got {
				body := r.decoded(t)
				if tools, _ := body["tools"].([]any); len(tools) != 1 {
					t.Errorf("call %d offers the tools %v, want the run's one tool", i+1, body["tools"])
				}
				if choice := body["tool_choice"]; !reflect.DeepEqual(choice, c.auto) {
					t.Errorf("call %d's tool_choice %v, want %v: the model's own choice, never a forced tool", i+1, choice, c.auto)
				}
				var schema any = body
				for _, key := range finalSchemaPath(c.kind) {
					object, _ := schema.(map[string]any)
					schema = object[key]
				}
				switch config, _ := body["output_config"].(map[string]any); {
				case anthropicKind(c.kind) && (schema != nil || config["format"] != nil):
					t.Errorf("call %d, which offers a tool, carries the output format %v, want none on the Messages API", i+1, config["format"])
				case anthropicKind(c.kind) && config["effort"] != f.req.Model.Effort:
					t.Errorf("call %d carries the output config %v, want the model's effort %q kept", i+1, config, f.req.Model.Effort)
				case !anthropicKind(c.kind) && !reflect.DeepEqual(schema, final):
					t.Errorf("call %d carries %v at %v, want the run's Final schema %s", i+1, schema, finalSchemaPath(c.kind), finalSchema)
				}
				if bytes.Contains(r.body, []byte(lastCallSentence)) {
					t.Errorf("call %d, not the last allowed call, carries the last-call sentence", i+1)
				}
			}
		})
	}
}

// A run whose model call does not answer within the run's time is a time fault.
func TestARunPastItsTimeBoundIsAFault(t *testing.T) {
	t.Parallel()
	answered := anthropicStream("end_turn", anthropicUsage{Output: 1}, anthropicText("The note says plum."))
	s := newStandIn(t, reply{delay: 30 * time.Second, body: answered.body})
	f := newFixture(t, s, AnthropicMessages, 1)
	f.req.Bounds.Time = 200 * time.Millisecond
	result, err := Run(context.Background(), f.req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Fault != FaultTime {
		t.Errorf("fault %q, want %q: %s", result.Fault, FaultTime, result.Detail)
	}
}

// A run cut by its time after an OpenAI Responses call finished with a tool call still gives that
// step its call's own output items, byte for byte, as a run that answers does: a caller writing the
// steps of a faulted run into a conversation writes an OpenAI turn from them. The call in progress
// when the time ran out is no step.
func TestARunPastItsTimeBoundKeepsEachFinishedStepsOutputItems(t *testing.T) {
	t.Parallel()
	stepOne := []string{
		openaiReasoning("rs_t1", "gAAAAtime+/one==", "Reading the note"),
		openaiToolCall("fc_t1", "call_t1", toolName, map[string]any{"path": "note.txt"}),
	}
	answered := openaiStream(openaiUsage{Input: 20, Output: 5}, "", openaiMessage("msg_t2", "It said plum."))
	s := newStandIn(t,
		openaiStream(openaiUsage{Input: 10, Output: 5}, "", stepOne...),
		reply{delay: 30 * time.Second, body: answered.body},
	)
	f := newFixture(t, s, OpenAIResponses, 3)
	f.req.Bounds.Time = 2 * time.Second
	result, err := Run(context.Background(), f.req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Fault != FaultTime || len(result.Steps) != 1 {
		t.Fatalf("fault %q with %d steps, want %q after the one finished step: %s", result.Fault, len(result.Steps), FaultTime, result.Detail)
	}
	var native []string
	for _, item := range result.Steps[0].Native {
		native = append(native, string(item))
	}
	if !reflect.DeepEqual(native, stepOne) {
		t.Errorf("the finished step's native items %q, want its call's %q", native, stepOne)
	}
}

// The cost of a run of a request that sets no cache field is every call's tokens at the model's
// prices, its cache writes and reads included, and every step carries its own call's.
func TestTheCostSumsEveryCallWithItsCacheWrites(t *testing.T) {
	t.Parallel()
	s := newStandIn(t, threeCallAnthropic()...)
	f := newFixture(t, s, AnthropicMessages, 3)
	f.req.History = anthropicHistory()
	result, err := Run(context.Background(), f.req)
	if err != nil || result.Fault != "" {
		t.Fatalf("result %+v, %v", result, err)
	}
	// By hand, from the stand-in's usage objects: input 10+3+2, output 20+15+30, cache writes
	// 1000+50+40 and cache reads 0+1000+1050, at $5, $25, $6.25 and $0.50 per million tokens.
	want := (15*5 + 65*25 + 1090*6.25 + 2050*0.5) / 1e6
	if u := result.Usage; u.Input != 15 || u.Output != 65 || u.CacheWrite != 1090 || u.CacheRead != 2050 || !u.Metered {
		t.Errorf("usage %+v, want every call's summed", u)
	}
	if math.Abs(result.Usage.CostUSD-want) > 1e-12 {
		t.Errorf("cost $%.10f, want $%.10f", result.Usage.CostUSD, want)
	}
	steps := []Usage{
		{Input: 10, Output: 20, CacheWrite: 1000, CostUSD: (10*5 + 20*25 + 1000*6.25) / 1e6, Metered: true},
		{Input: 3, Output: 15, CacheWrite: 50, CacheRead: 1000, CostUSD: (3*5 + 15*25 + 50*6.25 + 1000*0.5) / 1e6, Metered: true},
		{Input: 2, Output: 30, CacheWrite: 40, CacheRead: 1050, CostUSD: (2*5 + 30*25 + 40*6.25 + 1050*0.5) / 1e6, Metered: true},
	}
	if len(result.Steps) != len(steps) {
		t.Fatalf("%d steps, want %d", len(result.Steps), len(steps))
	}
	for i, want := range steps {
		if got := result.Steps[i].Usage; got != want {
			t.Errorf("step %d's usage %+v, want its own call's %+v", i+1, got, want)
		}
	}
}

// environmentHeader is the name of a header the environment asks a vendor client to put on every
// request it makes, whatever endpoint the caller configured.
const environmentHeader = "X-From-The-Environment"

// The endpoint the run calls is the request's, whatever the environment's own base URLs and key
// say, and the call carries the request's token and nothing else the environment holds: neither
// the organization and the project openai-go reads, nor the headers either vendor SDK reads.
func TestTheEndpointComesFromTheRequestAndNeverTheEnvironment(t *testing.T) {
	elsewhere := newStandIn(t)
	t.Setenv("OPENAI_BASE_URL", elsewhere.server.URL+"/v1")
	t.Setenv("ANTHROPIC_BASE_URL", elsewhere.server.URL)
	t.Setenv("OPENAI_API_KEY", "a-key-from-the-environment")
	t.Setenv("OPENAI_ORG_ID", "an-organization-from-the-environment")
	t.Setenv("OPENAI_PROJECT_ID", "a-project-from-the-environment")
	t.Setenv("OPENAI_CUSTOM_HEADERS", environmentHeader+": yes\n"+environmentHeader+"-Too: yes")
	t.Setenv("ANTHROPIC_CUSTOM_HEADERS", environmentHeader+": yes")

	openaiStandIn := newStandIn(t, openaiStream(openaiUsage{Input: 1, Output: 1}, "", openaiMessage("msg_1", "It says plum.")))
	if result, err := Run(context.Background(), newFixture(t, openaiStandIn, OpenAIResponses, 1).req); err != nil || result.Fault != "" {
		t.Fatalf("the OpenAI run: %+v, %v", result, err)
	}
	anthropicStandIn := newStandIn(t, anthropicStream("end_turn", anthropicUsage{Input: 1, Output: 1}, anthropicText("It says plum.")))
	if result, err := Run(context.Background(), newFixture(t, anthropicStandIn, AnthropicMessages, 1).req); err != nil || result.Fault != "" {
		t.Fatalf("the Anthropic run: %+v, %v", result, err)
	}
	chatStandIn := newStandIn(t, chatStream("stop", chatUsage{Prompt: 1, Completion: 1}, "It says plum."))
	if result, err := Run(context.Background(), newFixture(t, chatStandIn, OpenAIChat, 1).req); err != nil || result.Fault != "" {
		t.Fatalf("the Chat Completions run: %+v, %v", result, err)
	}
	azureStandIn := newStandIn(t, anthropicStream("end_turn", anthropicUsage{Input: 1, Output: 1}, anthropicText("It says plum.")))
	if result, err := Run(context.Background(), newFixture(t, azureStandIn, AzureAnthropic, 1).req); err != nil || result.Fault != "" {
		t.Fatalf("the Azure run: %+v, %v", result, err)
	}
	if n := len(elsewhere.requests()); n != 0 {
		t.Errorf("the environment's endpoint got %d requests, want none", n)
	}
	if got := openaiStandIn.requests()[0].header.Get("Authorization"); got != "Bearer standin-openai-token" {
		t.Errorf("the OpenAI call's Authorization %q, want the request's token", got)
	}
	for _, c := range []struct {
		kind Kind
		s    *standIn
	}{
		{OpenAIResponses, openaiStandIn}, {AnthropicMessages, anthropicStandIn},
		{OpenAIChat, chatStandIn}, {AzureAnthropic, azureStandIn},
	} {
		for _, name := range []string{"OpenAI-Organization", "OpenAI-Project", environmentHeader, environmentHeader + "-Too"} {
			if got := c.s.requests()[0].header.Get(name); got != "" {
				t.Errorf("the %s call carries %s %q, want none: the package reads nothing from the environment", c.kind, name, got)
			}
		}
	}
}

// A header the environment names is never put on the call, so a header the call needs is never
// taken off it either: OPENAI_CUSTOM_HEADERS naming Content-Type and Accept leaves the
// openai-responses request carrying both, because the package builds openai-go's Responses
// service itself rather than its client, and that service reads no environment at all.
func TestAnEnvironmentHeaderNamingTheCallsOwnHeadersLeavesThemOnTheCall(t *testing.T) {
	t.Setenv("OPENAI_CUSTOM_HEADERS", "Content-Type: application/json\nAccept: application/json")
	s := newStandIn(t, openaiStream(openaiUsage{Input: 1, Output: 1}, "", openaiMessage("msg_1", "It says plum.")))
	result, err := Run(context.Background(), newFixture(t, s, OpenAIResponses, 1).req)
	if err != nil || result.Fault != "" {
		t.Fatalf("the OpenAI run: %+v, %v", result, err)
	}
	got := s.requests()
	if len(got) != 1 {
		t.Fatalf("%d model calls, want 1", len(got))
	}
	for _, name := range []string{"Content-Type", "Accept"} {
		if value := got[0].header.Get(name); value != "application/json" {
			t.Errorf("the call's %s is %q, want application/json: the environment takes no header off a call", name, value)
		}
	}
}

// Every model call carries the token as the endpoint reads it at that call, so a token rotated
// between two calls reaches the second.
func TestTheTokenIsReadAgainForEveryModelCall(t *testing.T) {
	t.Parallel()
	s := newStandIn(t,
		anthropicStream("tool_use", anthropicUsage{Output: 1}, anthropicToolUse("toolu_1", toolName, map[string]any{"path": "note.txt"})),
		anthropicStream("end_turn", anthropicUsage{Output: 1}, anthropicText("The note says plum.")),
	)
	f := newFixture(t, s, AnthropicMessages, 2)
	writeToken(t, f.token, "the-first-token")
	f.tool.onCall = func(ToolCall) { writeToken(t, f.token, "the-rotated-token") }
	if result, err := Run(context.Background(), f.req); err != nil || result.Fault != "" {
		t.Fatalf("result %+v, %v", result, err)
	}
	var tokens []string
	for _, r := range s.requests() {
		tokens = append(tokens, r.header.Get("X-Api-Key"))
	}
	if want := []string{"the-first-token", "the-rotated-token"}; !reflect.DeepEqual(tokens, want) {
		t.Errorf("the calls carried %v, want the file's token at each call, %v", tokens, want)
	}
}

// A 500 is retried once and the retry's reply is the answer; a second 500 is a provider error
// rather than a second retry, and a 400 is a provider error on the first request.
func TestAServerErrorIsRetriedOnceAndARequestErrorIsNot(t *testing.T) {
	t.Parallel()
	overloaded := reply{
		status: http.StatusInternalServerError, headers: map[string]string{"Retry-After": "0"},
		body: []byte(`{"type":"error","error":{"type":"api_error","message":"overloaded"}}`),
	}
	answered := anthropicStream("end_turn", anthropicUsage{Input: 1, Output: 1}, anthropicText("The note says plum."))

	retried := newStandIn(t, overloaded, answered)
	result, err := Run(context.Background(), newFixture(t, retried, AnthropicMessages, 1).req)
	if err != nil || result.Fault != "" || result.Text != "The note says plum." {
		t.Fatalf("after a 500: result %+v, %v", result, err)
	}
	if n := len(retried.attempts()); n != 2 {
		t.Errorf("a 500 then a reply took %d requests, want 2", n)
	}

	twice := newStandIn(t, overloaded, overloaded, answered)
	result, err = Run(context.Background(), newFixture(t, twice, AnthropicMessages, 1).req)
	if err != nil || result.Fault != FaultProviderError {
		t.Errorf("after two 500s: result %+v, %v; want %q", result, err, FaultProviderError)
	}
	if n := len(twice.attempts()); n != 2 {
		t.Errorf("two 500s took %d requests, want 2: one retry only", n)
	}

	bad := newStandIn(t, reply{status: http.StatusBadRequest, body: []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`)})
	result, err = Run(context.Background(), newFixture(t, bad, AnthropicMessages, 1).req)
	if err != nil || result.Fault != FaultProviderError {
		t.Errorf("after a 400: result %+v, %v; want %q", result, err, FaultProviderError)
	}
	if n := len(bad.attempts()); n != 1 {
		t.Errorf("a 400 took %d requests, want 1: a request error is not retried", n)
	}
}

// Every outage status is retried once, as a 500 is: a 408 the endpoint (or a proxy in front of it)
// did not take in time, a 429, a 501 and a 505 (RFC 9110 sections 15.5.9 and 15.6, RFC 6585
// section 4), and the retry's reply is the answer. This transport is the run's one retry: the SDK's
// own retries are off (languageModel).
func TestEveryOutageStatusIsRetriedOnce(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusNotImplemented, http.StatusHTTPVersionNotSupported} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			outage := reply{
				status: status, headers: map[string]string{"Retry-After": "0"},
				body: []byte(`{"type":"error","error":{"type":"api_error","message":"not now"}}`),
			}
			answered := anthropicStream("end_turn", anthropicUsage{Input: 1, Output: 1}, anthropicText("The note says plum."))
			s := newStandIn(t, outage, answered)
			result, err := Run(context.Background(), newFixture(t, s, AnthropicMessages, 1).req)
			if err != nil || result.Fault != "" || result.Text != "The note says plum." {
				t.Fatalf("after a %d: result %+v, %v; want the retry's reply", status, result, err)
			}
			if n := len(s.attempts()); n != 2 {
				t.Errorf("a %d then a reply took %d requests, want 2", status, n)
			}
		})
	}
}

// A call whose connection drops before any answer is a transport error, retried once a second
// later, and the retry's reply is the answer: the endpoint hears the call twice, and that is one
// model call. The stand-in's one reply answers the retry, since an answer that never went out
// uses no reply up. That holds when the handler that dropped the call has not run again by the
// time the retry comes, as a handler the scheduler set aside after the close would not have: the
// stand-in puts the reply back before the close, so the retry depends on nothing that handler
// does later.
func TestACallThatLosesItsConnectionIsRetriedOnce(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// held is whether the dropping handler waits, after the close, until the row ends.
		held bool
	}{
		{name: "the dropping handler returns"},
		{name: "the dropping handler is held until the run ends", held: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			answered := anthropicStream("end_turn", anthropicUsage{Input: 1, Output: 1}, anthropicText("The note says plum."))
			s := newStandIn(t, answered).dropping(1)
			if c.held {
				held := make(chan struct{})
				s.holding(held)
				// After the stand-in's own cleanup is registered, so it runs first and the
				// server's close does not wait on the held handler.
				t.Cleanup(func() { close(held) })
			}
			result, err := Run(context.Background(), newFixture(t, s, AnthropicMessages, 1).req)
			if err != nil || result.Fault != "" || result.Text != "The note says plum." {
				t.Fatalf("after a dropped connection: result %+v, %v", result, err)
			}
			if n, calls := len(s.attempts()), len(s.requests()); n != 2 || calls != 1 {
				t.Errorf("the endpoint got %d requests, counted as %d model calls, want 2 and 1: the call and its one retry", n, calls)
			}
		})
	}
}

// A redirect is never followed, in a run and in a decision alike: the transport puts the
// endpoint's credential on every request it carries, and Go's own client strips Authorization
// on a cross-domain redirect before a transport runs, so following one would hand the token to
// whatever server the Location names. The client's redirect check fails the call instead, with
// a detail naming the host the redirect points at, and the server the redirect points at hears
// nothing at all. A redirect is not retried: the endpoint answers the run's one model call once.
// That holds for the transport's one retry too: a call that loses its connection before the
// redirect answers is asked again a second later, meets the redirect, and follows it no further,
// so the endpoint gets the call and its retry and answers once. The redirecting endpoint redirects
// every request it gets, as a real one does: a stand-in that answered once would answer the run's
// call after a request from a client that is not the run's with "the stand-in has no more
// replies", and the detail would name no redirect at all. Such a request carries none of the
// run's credential, so the counts below leave it out.
func TestARedirectIsNotFollowedAndItsTargetIsNeverCalled(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// answer is what the redirect's target would reply with, were it ever called,
		// and call makes the request through the stand-in that redirects, returning the
		// fault it ended on and what that fault says.
		answer reply
		call   func(*testing.T, *standIn) (Fault, string)
		// drops is how many of the first calls lose their connection before the
		// redirect answers them, each retried once by the transport.
		drops int
	}{
		{
			name:   "a run",
			answer: anthropicStream("end_turn", anthropicUsage{Input: 1, Output: 1}, anthropicText("The target answered.")),
			call: func(t *testing.T, s *standIn) (Fault, string) {
				result, err := Run(context.Background(), newFixture(t, s, AnthropicMessages, 1).req)
				if err != nil {
					t.Fatal(err)
				}
				if result.Text != "" {
					t.Errorf("the run answered %q, want no answer from the redirect's target", result.Text)
				}
				return result.Fault, result.Detail
			},
		},
		{
			// The transport's one retry meets the redirect, and follows it no more than
			// the first call would have.
			name:   "a run whose first call loses its connection",
			answer: anthropicStream("end_turn", anthropicUsage{Input: 1, Output: 1}, anthropicText("The target answered.")),
			drops:  1,
			call: func(t *testing.T, s *standIn) (Fault, string) {
				result, err := Run(context.Background(), newFixture(t, s, AnthropicMessages, 1).req)
				if err != nil {
					t.Fatal(err)
				}
				if result.Text != "" {
					t.Errorf("the run answered %q, want no answer from the redirect's target", result.Text)
				}
				// The drop falls on the endpoint's first request. Where that is the run's
				// call, the endpoint gets the call and its one retry; where a request from
				// another client came first and took the drop, the run's call is not dropped.
				got := s.attempts()
				want := 2
				if len(got) > 0 && credentialed(got[:1]) == 0 {
					want = 1
				}
				if n := credentialed(got); n != want {
					t.Errorf("the redirecting endpoint got %d requests carrying the run's credential, want %d: the call, and its one retry where the drop fell on it", n, want)
				}
				return result.Fault, result.Detail
			},
		},
		{
			name:   "a decision",
			answer: reply{body: []byte(decideAnswer)},
			call: func(t *testing.T, s *standIn) (Fault, string) {
				result, err := Decide(context.Background(), newDecideFixture(t, s))
				if err != nil {
					t.Fatal(err)
				}
				if len(result.Answers) != 0 {
					t.Errorf("the decision answered %+v, want no answer from the redirect's target", result.Answers)
				}
				return result.Fault, result.Detail
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			target := newStandIn(t, c.answer)
			redirecting := newStandInAnsweringEvery(t, reply{
				status:  http.StatusTemporaryRedirect,
				headers: map[string]string{"Location": target.server.URL + "/elsewhere"},
			}).dropping(c.drops)
			fault, detail := c.call(t, redirecting)
			// One model call answered with the redirect: a retry of the redirect would be a
			// second, while a dropped call and its retry are one (standIn.requests).
			if n := credentialed(redirecting.requests()); n != 1 {
				t.Errorf("the redirecting endpoint answered %d model calls carrying the run's credential, want 1: a redirect is not retried", n)
			}
			if fault != FaultProviderError {
				t.Errorf("fault %q, want %q: a redirect is the provider failing the call: %s", fault, FaultProviderError, detail)
			}
			// The host the redirect names, so a caller reading the detail can see
			// where the call was sent and why it went no further.
			if host := target.server.Listener.Addr().String(); !strings.Contains(detail, host) {
				t.Errorf("the detail is %q, want the redirect's target host, %s, named in it", detail, host)
			}
			got := target.attempts()
			if len(got) != 0 {
				t.Errorf("the redirect's target got %d requests, want none", len(got))
			}
			for i, r := range got {
				for _, name := range []string{"Authorization", "X-Api-Key"} {
					if credential := r.header.Get(name); credential != "" {
						t.Errorf("request %d to the redirect's target carries %s %q, want no credential anywhere", i+1, name, credential)
					}
				}
			}
		})
	}
}

// credentialed is how many of rs carry an endpoint credential, as every request a run or a
// decision makes does: a request from a client that is not the run's carries none.
func credentialed(rs []recorded) int {
	n := 0
	for _, r := range rs {
		if r.header.Get("X-Api-Key") != "" || r.header.Get("Authorization") != "" {
			n++
		}
	}
	return n
}

// A request the loop cannot run at all is an error rather than a fault: nothing comes back in the
// result and no model call is made.
func TestARequestTheLoopCannotRunIsAnError(t *testing.T) {
	t.Parallel()
	s := newStandIn(t)
	for _, c := range []struct {
		name   string
		change func(*Request)
	}{
		{name: "no time bound", change: func(r *Request) { r.Bounds.Time = 0 }},
		{name: "no call bound", change: func(r *Request) { r.Bounds.Calls = 0 }},
		{name: "no output token bound", change: func(r *Request) { r.Bounds.OutputTokens = 0 }},
		{name: "an endpoint that reads no token", change: func(r *Request) { r.Model.Endpoint.Token = nil }},
		{name: "no model", change: func(r *Request) { r.Model.ID = "" }},
		{name: "a tool with no name", change: func(r *Request) { r.Tools[0].Name = "" }},
		{name: "a tool that runs nothing", change: func(r *Request) { r.Tools[0].Run = nil }},
		{name: "a tool registered twice", change: func(r *Request) { r.Tools = append(r.Tools, r.Tools[0]) }},
		{name: "a protocol the loop makes no run on", change: func(r *Request) { r.Model.Endpoint.Kind = SystemOne }},
		{name: "a history block the protocol cannot carry", change: func(r *Request) {
			r.History = []Message{{Role: Assistant, Blocks: []Block{{Kind: ToolResultBlock, ToolCallID: "toolu_1", Name: toolName}}}}
		}},
		{name: "a final that is no JSON schema", change: func(r *Request) { r.Final = json.RawMessage(`{"type":"object","properties":5}`) }},
		{name: "a check with no final to read", change: func(r *Request) { r.Check, r.AskAgain = refuseEmptyAnswer, 1 }},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, s, AnthropicMessages, 1)
			c.change(&f.req)
			result, err := Run(context.Background(), f.req)
			if err == nil {
				t.Fatalf("the request ran, with result %+v", result)
			}
			if !reflect.DeepEqual(result, Result{}) {
				t.Errorf("the result is %+v, want nothing", result)
			}
		})
	}
	if n := len(s.requests()); n != 0 {
		t.Errorf("%d model calls were made, want none", n)
	}
}
