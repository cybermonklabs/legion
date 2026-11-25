package answer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/conversation"
	"github.com/sjawhar/legion/daemon/internal/conversation/claude"
	"github.com/sjawhar/legion/daemon/internal/conversation/omp"
	"github.com/sjawhar/legion/daemon/internal/dispatchtool"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/ghtool"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// The provider-level proofs of the loop — the cache marks, the effort and the thinking options on
// the wire, the retry, the tool choice of the last call, the reply read back against its schema —
// are agentloop's own (packages/envoy/agentloop). What is proved here is the daemon's half: the
// conversation and the prompt as the request agentloop is given, the gh tool and its numbering,
// the faults under the daemon's own names, the turns to append and the cost line.

// stubGH is the gh tool's stand-in: it records each call and answers each with output naming it.
type stubGH struct {
	mu     sync.Mutex
	calls  []GHCall
	onCall func(seq int)
}

func (g *stubGH) Run(_ context.Context, call GHCall) ghtool.Result {
	g.mu.Lock()
	g.calls = append(g.calls, call)
	onCall := g.onCall
	g.mu.Unlock()
	if onCall != nil {
		onCall(call.Seq)
	}
	return ghtool.Result{Output: "output of " + call.Args, Exit: 0, Ran: true}
}

func (g *stubGH) made() []GHCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]GHCall(nil), g.calls...)
}

func testBounds(modelCalls int) config.AnswerBounds {
	return config.AnswerBounds{
		Answer: 30 * time.Second, ModelCalls: modelCalls, OutputTokens: 4000, GHCall: 30 * time.Second,
		GHOutputChars: 60000, GHCalls: 20, Concurrent: 4, DailyUSD: 50,
	}
}

// keyFile is a provider key file holding key, and its credential.
func keyFile(t *testing.T, key string) (string, config.AnswerCredential) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte(key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, config.AnswerCredential{File: path}
}

var (
	anthropicPrice = config.AnswerPrice{Input: 5, Output: 25, CacheWrite: 6.25, CacheRead: 0.5}
	openaiPrice    = config.AnswerPrice{Input: 2, Output: 10, CacheWrite: 2.5, CacheRead: 0.1}
)

// historySignature is the signature of the Anthropic conversation's thinking, with the characters
// a signature carries that JSON could escape.
const historySignature = "EqQBCkgIARABGAIiQL+/hist0ry==/sig"

// anthropicConversation is a conversation on Anthropic: a request, the agent's signed thinking
// and its answer.
func anthropicConversation() conversation.Conversation {
	return conversation.Conversation{
		Model: "anthropic/claude-standin", API: anthropicMessages, ThinkingLevel: "high",
		Messages: []conversation.Message{
			{Role: conversation.User, Content: []conversation.Block{{Type: conversation.Text, Text: "Write the plan for INFRA-1."}}},
			{
				Role: conversation.Assistant, Provider: "anthropic", API: anthropicMessages, Model: "claude-standin",
				Content: []conversation.Block{
					{Type: conversation.Thinking, Text: "The plan needs a migration.", Signature: historySignature},
					{Type: conversation.Text, Text: "The plan is written."},
				},
			},
		},
	}
}

// anthropicRequest is a question on the Anthropic conversation, against the stand-in.
func anthropicRequest(t *testing.T, s *standIn, gh GH, modelCalls int) Request {
	t.Helper()
	_, credential := keyFile(t, "sk-ant-standin")
	return Request{
		Conversation: anthropicConversation(),
		Agent:        Agent{Role: "planner", Issue: "INFRA-1", Session: "s-planner", Phase: "implementing", GHUsage: ghtool.Usage{Repository: ghrepo.MustParse("acme/widgets")}},
		ChangeNote:   "Dispatch status: in progress. PR #7: open, head abc123.",
		Question:     Question{ID: "dispatch:m-1", Text: "Is PR 7 green?", Asker: "a person", Where: "the INFRA-1 thread"},
		Providers:    map[config.AnswerProvider]config.AnswerEndpoint{config.AnswerAnthropic: {BaseURL: s.server.URL, Credential: credential}},
		Prices:       map[string]config.AnswerPrice{"anthropic/claude-standin": anthropicPrice},
		Bounds:       testBounds(modelCalls),
		GH:           gh,
		Log:          slog.New(slog.DiscardHandler),
	}
}

// threeStepAnthropic are the replies of a three-call answer: thinking and a gh call, another gh
// call, then the reply.
func threeStepAnthropic() []reply {
	return []reply{
		anthropicStream("tool_use", anthropicUsage{Input: 10, Output: 20, CacheWrite: 1000}, anthropicThinking("Check the PR.", "sig-step-1+/=="), anthropicGH("toolu_1", "pr", "view", "7")),
		anthropicStream("tool_use", anthropicUsage{Input: 3, Output: 15, CacheWrite: 50, CacheRead: 1000}, anthropicGH("toolu_2", "pr", "checks", "7")),
		anthropicStream("end_turn", anthropicUsage{Input: 2, Output: 30, CacheWrite: 40, CacheRead: 1050}, anthropicText(`{"kind":"answered","text":"PR 7 is green.","needs":""}`)),
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

// An Anthropic answer sends the conversation's signed thinking as the session stored it, sends
// each call's own turn back to the next call, and appends the model's turns with their
// signatures, their gh calls and each call's result, under the question the loop asked.
func TestAnAnthropicAnswerReplaysItsSignaturesAndAppendsEveryTurn(t *testing.T) {
	s := newStandIn(t, threeStepAnthropic()...)
	gh := &stubGH{}
	reply, err := Answer(context.Background(), anthropicRequest(t, s, gh, 3))
	if err != nil {
		t.Fatal(err)
	}
	if reply.Fault != "" || reply.Kind != Answered || reply.Text != "PR 7 is green." {
		t.Fatalf("reply %+v, want answered %q", reply, "PR 7 is green.")
	}
	got := s.requests()
	if len(got) != 3 {
		t.Fatalf("%d model calls, want 3", len(got))
	}
	for i, r := range got {
		if !bytes.Contains(r.body, []byte(`"signature":"`+historySignature+`"`)) {
			t.Errorf("call %d does not carry the history's signature byte for byte", i+1)
		}
	}
	// The wire's messages: the history's request and answer (0, 1), the question (2), then each
	// call's turn and its tool result.
	history := blocks(messages(t, got[0])[1])
	if history[0]["type"] != "thinking" || history[0]["signature"] != historySignature || history[0]["thinking"] != "The plan needs a migration." {
		t.Errorf("the history's thinking went as %v", history[0])
	}
	stepOne := blocks(messages(t, got[1])[3])
	if stepOne[0]["type"] != "thinking" || stepOne[0]["signature"] != "sig-step-1+/==" || stepOne[0]["thinking"] != "Check the PR." {
		t.Errorf("the second call carries the first call's turn as %v, want its thinking with its signature", stepOne)
	}
	if calls := gh.made(); !reflect.DeepEqual(calls, []GHCall{{1, 1, `["pr","view","7"]`}, {2, 2, `["pr","checks","7"]`}}) {
		t.Errorf("gh calls %+v", calls)
	}

	turns := reply.Append.Turns
	if len(turns) != 3 || reply.Append.QuestionID != "dispatch:m-1" || reply.Append.Model != "claude-standin" || reply.Append.Provider != "anthropic" {
		t.Fatalf("append %+v", reply.Append)
	}
	if first := turns[0].Content[0]; first.Type != conversation.Thinking || first.Signature != "sig-step-1+/==" || first.Text != "Check the PR." {
		t.Errorf("the first turn's thinking %+v", first)
	}
	if call := turns[0].Content[1]; call.Type != conversation.ToolCall || call.ToolCallID != "toolu_1" || call.ToolName != "gh" {
		t.Errorf("the first turn's call %+v", call)
	}
	if result := turns[1].Results[0]; result.ToolCallID != "toolu_2" || result.Content[0].Text != `output of ["pr","checks","7"]` || result.IsError {
		t.Errorf("the second turn's result %+v", result)
	}
	if !strings.Contains(reply.Append.Question, "Is PR 7 green?") || !strings.Contains(reply.Append.Question, "head abc123") {
		t.Errorf("the appended question %q holds no question or change note", reply.Append.Question)
	}
}

// The answer's cost is every call's tokens at the model's prices, cache writes and reads included,
// and one `answers: answered` line carries it with the tokens summed over every call.
func TestTheCostSumsEveryCallCacheWritesIncludedAndOneLineLogsIt(t *testing.T) {
	s := newStandIn(t, threeStepAnthropic()...)
	var logged bytes.Buffer
	req := anthropicRequest(t, s, &stubGH{}, 3)
	req.Log = slog.New(slog.NewJSONHandler(&logged, nil))
	reply, err := Answer(context.Background(), req)
	if err != nil || reply.Kind != Answered {
		t.Fatalf("reply %+v, %v", reply, err)
	}
	// By hand: input 10+3+2, output 20+15+30, cache writes 1000+50+40, cache reads 0+1000+1050,
	// at $5, $25, $6.25 and $0.50 per million tokens.
	want := (15*5 + 65*25 + 1090*6.25 + 2050*0.5) / 1e6
	if math.Abs(reply.CostUSD-want) > 1e-12 {
		t.Errorf("cost $%.10f, want $%.10f", reply.CostUSD, want)
	}
	if u := reply.Usage; u.InputTokens != 15 || u.OutputTokens != 65 || u.CacheWriteTokens != 1090 || u.CacheReadTokens != 2050 || reply.Calls != 3 {
		t.Errorf("usage %+v over %d calls", u, reply.Calls)
	}
	// Each turn carries its own call's cost, which the appender writes beside it.
	if first := reply.Append.Turns[0].Usage; first.InputTokens != 10 || math.Abs(first.CacheWriteCost-1000*6.25/1e6) > 1e-12 {
		t.Errorf("the first turn's usage %+v", first)
	}
	var lines []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(logged.Bytes()), []byte("\n")) {
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		if record["msg"] == "answers: answered" {
			lines = append(lines, record)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("%d answers: answered lines, want 1: %s", len(lines), logged.Bytes())
	}
	line := lines[0]
	for key, value := range map[string]any{
		"question": "dispatch:m-1", "issue": "INFRA-1", "role": "planner", "session": "s-planner",
		"model": "anthropic/claude-standin", "calls": 3.0, "inputTokens": 15.0, "outputTokens": 65.0,
		"cacheWriteTokens": 1090.0, "cacheReadTokens": 2050.0, "reasoningTokens": 0.0,
	} {
		if line[key] != value {
			t.Errorf("the line's %s is %v, want %v", key, line[key], value)
		}
	}
	if cost, _ := line["costUSD"].(float64); math.Abs(cost-want) > 1e-12 {
		t.Errorf("the line's costUSD %v, want %v", line["costUSD"], want)
	}
}

// offered is what an Anthropic model call offered the model: the system prompt's blocks, and
// each tool's name, description and input schema's properties.
type offered struct {
	System []struct {
		Text string `json:"text"`
	} `json:"system"`
	Tools []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		InputSchema struct {
			Properties map[string]struct {
				Type        string `json:"type"`
				Description string `json:"description"`
			} `json:"properties"`
		} `json:"input_schema"`
	} `json:"tools"`
}

// offerOf is what the call r offered.
func offerOf(t *testing.T, r recorded) offered {
	t.Helper()
	var body offered
	if err := json.Unmarshal(r.body, &body); err != nil {
		t.Fatal(err)
	}
	return body
}

// The loop's tool list holds its tools, and the Verification document names this test: gh, which
// every model call offers; dispatch, Dispatch's own command run as the agent's session, which a
// model call offers where the daemon found the command (Request.Dispatch); and web_search, one web
// search the daemon runs, which a model call offers where answers.web_search configures it
// (Request.WebSearch). Each offered tool is named with its input schema and its description, and
// the system prompt names each.
func TestTheLoopsToolListHoldsItsThreeTools(t *testing.T) {
	for _, name := range []string{"gh", "dispatch", "web_search"} {
		if !slices.ContainsFunc(tools, func(t tool) bool { return t.name == name }) {
			t.Fatalf("the loop's tools %v hold no %s", tools, name)
		}
	}
	s := newStandIn(t,
		anthropicStream("end_turn", anthropicUsage{Input: 1, Output: 1}, anthropicText(`{"kind":"answered","text":"Yes.","needs":""}`)),
		anthropicStream("end_turn", anthropicUsage{Input: 1, Output: 1}, anthropicText(`{"kind":"answered","text":"Yes.","needs":""}`)),
	)
	req := anthropicRequest(t, s, &stubGH{}, 1)
	req.Dispatch = &stubDispatch{}
	req.WebSearch = &stubWebSearch{}
	if _, err := Answer(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	body := offerOf(t, s.requests()[0])
	offered := map[string]int{}
	for i, tool := range body.Tools {
		offered[tool.Name] = i
	}
	gh, hasGH := offered["gh"]
	at, hasDispatch := offered["dispatch"]
	search, hasWebSearch := offered["web_search"]
	if len(body.Tools) != 3 || !hasGH || !hasDispatch || !hasWebSearch || body.Tools[gh].InputSchema.Properties["args"].Type != "array" {
		t.Fatalf("the call offers tools %+v, want gh taking args, an array, dispatch and web_search", body.Tools)
	}
	dispatch := body.Tools[at]
	if dispatch.InputSchema.Properties["args"].Type != "array" || dispatch.InputSchema.Properties["stdin"].Type != "string" {
		t.Errorf("the call offers dispatch as %+v, want it taking args, an array, and stdin, a string", dispatch)
	}
	for _, want := range append([]string{
		"Dispatch's own `dispatch` command", "as your own session, s-planner", "with that session's access",
		`{"args":["read","INFRA-1"]}`,
		"Every other command is refused", "the loop's mark, which the tool adds",
	}, slices.Concat(dispatchtool.ReadCommands, dispatchtool.WriteCommands)...) {
		if !strings.Contains(dispatch.Description, want) {
			t.Errorf("the dispatch tool's description lacks %q:\n%s", want, dispatch.Description)
		}
	}
	webSearch := body.Tools[search]
	if query := webSearch.InputSchema.Properties["query"]; len(webSearch.InputSchema.Properties) != 1 || query.Type != "string" || query.Description == "" {
		t.Errorf("the call offers web_search as %+v, want it taking query alone, a described string", webSearch)
	}
	for _, want := range []string{
		"The daemon runs one web search for you", "facts your conversation and your other tools do not hold",
		"at most 3 times in this answer", "Your reply must name the address of each source you use",
	} {
		if !strings.Contains(webSearch.Description, want) {
			t.Errorf("the web_search tool's description lacks %q:\n%s", want, webSearch.Description)
		}
	}
	if len(body.System) == 0 {
		t.Fatal("the call carries no system prompt")
	}
	for _, name := range []string{"`gh`", "`dispatch`", "`web_search`"} {
		if !strings.Contains(body.System[0].Text, name) {
			t.Errorf("the system prompt does not name %s:\n%s", name, body.System[0].Text)
		}
	}
	// An answer whose request carries no dispatch tool, a daemon that found no dispatch command,
	// is offered gh alone, and its prompt names no dispatch.
	if _, err := Answer(context.Background(), anthropicRequest(t, s, &stubGH{}, 1)); err != nil {
		t.Fatal(err)
	}
	if body := offerOf(t, s.requests()[1]); len(body.Tools) != 1 || body.Tools[0].Name != "gh" || strings.Contains(body.System[0].Text, "dispatch") {
		t.Errorf("an answer with no dispatch tool offers %+v under %q, want gh alone and no dispatch named", body.Tools, body.System[0].Text)
	}
}

// Every example call the dispatch tool's description gives is a read, and it gives one: each JSON
// object with args in the description the loop offers the model decodes as the tool's input (args,
// stdin and no other field) and names one of the tool's reads (dispatchtool.ReadCommands). In proof
// run 45683361567 a model ran the description's one write example, a comment, as a probe with the
// stdin "placeholder", and the comment posted.
func TestEveryExampleCallOfTheDispatchToolsDescriptionIsARead(t *testing.T) {
	s := newStandIn(t, anthropicStream("end_turn", anthropicUsage{Input: 1, Output: 1}, anthropicText(`{"kind":"answered","text":"Yes.","needs":""}`)))
	req := anthropicRequest(t, s, &stubGH{}, 1)
	req.Dispatch = &stubDispatch{}
	if _, err := Answer(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	var description string
	for _, tool := range offerOf(t, s.requests()[0]).Tools {
		if tool.Name == store.ToolDispatch {
			description = tool.Description
		}
	}
	if description == "" {
		t.Fatal("the call offers no dispatch tool")
	}
	examples := 0
	for rest := description; ; {
		at := strings.IndexByte(rest, '{')
		if at < 0 {
			break
		}
		rest = rest[at:]
		object := json.NewDecoder(strings.NewReader(rest))
		var fields map[string]json.RawMessage
		if object.Decode(&fields) != nil || fields["args"] == nil {
			rest = rest[1:]
			continue
		}
		example := rest[:object.InputOffset()]
		rest = rest[len(example):]
		examples++
		var input struct {
			Args  []string `json:"args"`
			Stdin string   `json:"stdin"`
		}
		strict := json.NewDecoder(strings.NewReader(example))
		strict.DisallowUnknownFields()
		if err := strict.Decode(&input); err != nil || len(input.Args) == 0 || !slices.Contains(dispatchtool.ReadCommands, input.Args[0]) {
			t.Errorf("the dispatch tool's description gives the example call %s (%v), want the tool's input naming one of its reads %v:\n%s",
				example, err, dispatchtool.ReadCommands, description)
		}
	}
	if examples == 0 {
		t.Errorf("the dispatch tool's description gives no example call:\n%s", description)
	}
}

// ghOffer is what an Anthropic model call offered of the gh tool and said of it: the tool's
// description, its args' description in the input schema, and the system prompt.
func ghOffer(t *testing.T, r recorded) (description, args, system string) {
	t.Helper()
	body := offerOf(t, r)
	if len(body.Tools) != 1 || len(body.System) == 0 {
		t.Fatalf("the call offers tools %+v under the system prompt %+v, want gh alone", body.Tools, body.System)
	}
	return body.Tools[0].Description, body.Tools[0].InputSchema.Properties["args"].Description, body.System[0].Text
}

// ghOfferProblems are what the gh tool's offer to an agent of acme/widgets gets wrong, for the
// issue's pull request pr (0 for none), whose example call is example: the description says what
// gh is and how the daemon runs it, that it runs with no checkout so every call names its
// repository, the repository, the pull request where there is one and none where there is none,
// the commands the tool takes, the example call, what it writes and its footer, and never that it
// only reads; the input schema gives the same example; and the system prompt's sentence on gh says
// the same of the checkout and the repository.
func ghOfferProblems(pr int, example, description, args, system string) []string {
	var problems []string
	for _, want := range []string{
		"GitHub's own gh command line", "as your role's App", "no checkout of any repository", "-R <owner>/<repo>",
		"a github.com URL", "repos/<owner>/<repo>/… endpoint", "Your issue's repository is acme/widgets",
		"api, pr (not pr checkout), run (not run download), workflow, repo view", `{"args":` + example + `}`,
		"comment, review", "Legion footer, which the tool adds",
	} {
		if !strings.Contains(description, want) {
			problems = append(problems, "the description lacks "+want)
		}
	}
	if strings.Contains(description, "read-only") {
		problems = append(problems, "the description says the tool is read-only")
	}
	if !strings.Contains(args, "For example "+example) {
		problems = append(problems, "the input schema gives no example "+example)
	}
	for _, want := range []string{"no checkout of any repository", "-R <owner>/<repo>", "Your issue's repository is acme/widgets"} {
		if !strings.Contains(system, want) {
			problems = append(problems, "the system prompt lacks "+want)
		}
	}
	named := fmt.Sprintf("its pull request is #%d", pr)
	for where, text := range map[string]string{"description": description, "system prompt": system} {
		switch {
		case pr > 0 && !strings.Contains(text, named):
			problems = append(problems, "the "+where+" lacks "+named)
		case pr == 0 && (strings.Contains(text, "its pull request") || strings.Contains(text, "#")):
			problems = append(problems, "the "+where+" names a pull request the record does not hold")
		}
	}
	return problems
}

// The gh tool the model is offered is described for the request, as its refusals describe it
// (ghtool.Usage): GitHub's own gh, which the daemon runs as the agent's role App with no checkout,
// so every call names its repository; the record's repository, and its pull request's number where
// the request has one and no pull request at all where it has none; the commands the tool takes,
// and an example call against that repository, which the input schema gives too; and the writes it
// makes, each comment or review carrying the footer the tool adds. The system prompt's sentence on
// gh says the same, in the same words. The description the tool had before, which named no command
// and no repository, is read the same way first and fails, so the words are what the test reads.
func TestTheGhToolIsDescribedForTheRecordsRepositoryAndPullRequest(t *testing.T) {
	const before = "Run GitHub's own gh command line with your GitHub access: your role's App, with full access to " +
		"your issue's repository owner and read access to the project's other owners, each call routed to the owner it " +
		"names. Use it to read code at any commit, pull requests, reviews, checks and runs, and to comment, review, push " +
		"or merge when the message asks for it and you judge you should. Every comment or review it posts carries your " +
		"Legion footer, which the tool adds. Commands unsafe on the daemon's host are refused with the reason."
	if problems := ghOfferProblems(0, `["pr","list","-R","acme/widgets","--limit","5"]`, before, before, before); len(problems) == 0 {
		t.Fatal("the description the tool had before passes the reading below, which would then prove nothing")
	}
	for _, c := range []struct {
		pr      int
		example string
	}{
		{7, `["pr","view","7","-R","acme/widgets","--json","state,headRefOid,reviews"]`},
		{0, `["pr","list","-R","acme/widgets","--limit","5"]`},
	} {
		t.Run(fmt.Sprintf("pull request %d", c.pr), func(t *testing.T) {
			s := newStandIn(t, anthropicStream("end_turn", anthropicUsage{Input: 1, Output: 1}, anthropicText(`{"kind":"answered","text":"Yes.","needs":""}`)))
			req := anthropicRequest(t, s, &stubGH{}, 1)
			req.Agent.GHUsage.PullRequest = c.pr
			if _, err := Answer(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			description, args, system := ghOffer(t, s.requests()[0])
			for _, problem := range ghOfferProblems(c.pr, c.example, description, args, system) {
				t.Error(problem)
			}
			usage := req.Agent.GHUsage
			if !strings.Contains(description, usage.String()) || !strings.Contains(system, usage.Checkout()) {
				t.Errorf("the description %q and the system prompt %q do not carry the gh tool's own usage %q", description, system, usage)
			}
		})
	}
}

// ompSession and claudeSession are the agent's conversation as each harness's own reader reads
// it, a request and the agent's answer on the model anthropicConversation names, which the
// exchange of an answer is appended after.
func ompSession(t *testing.T) *omp.Session {
	t.Helper()
	content := `{"type":"session","version":3,"id":"s-planner","timestamp":"2026-10-26T00:00:00.000Z","cwd":"/work"}` + "\n" +
		`{"id":"e0000001","parentId":null,"timestamp":"2026-10-26T00:00:00.000Z","type":"model_change","model":"anthropic/claude-standin"}` + "\n" +
		`{"id":"e0000002","parentId":"e0000001","timestamp":"2026-10-26T00:00:00.000Z","type":"message","message":{"role":"user","content":[{"type":"text","text":"Write the plan for INFRA-1."}],"attribution":"user","timestamp":1}}` + "\n" +
		`{"id":"e0000003","parentId":"e0000002","timestamp":"2026-10-26T00:00:00.000Z","type":"message","message":{"role":"assistant","content":[{"type":"text","text":"The plan is written."}],"api":"anthropic-messages","provider":"anthropic","model":"claude-standin","usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":2,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"stopReason":"stop","timestamp":2}}` + "\n"
	s, err := omp.Read([]byte(content), omp.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func claudeSession(t *testing.T) *claude.Session {
	t.Helper()
	const tail = `"userType":"external","entrypoint":"sdk-cli","cwd":"/work","sessionId":"s-planner","version":"2.1.310","gitBranch":"main"}`
	content := `{"parentUuid":null,"isSidechain":false,"type":"user","message":{"role":"user","content":"Write the plan for INFRA-1."},"uuid":"00000000-0000-4000-8000-000000000001","timestamp":"2026-10-26T00:00:01.000Z",` + tail + "\n" +
		`{"parentUuid":"00000000-0000-4000-8000-000000000001","isSidechain":false,"message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-standin","content":[{"type":"text","text":"The plan is written."}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}},"type":"assistant","effort":"high","uuid":"00000000-0000-4000-8000-000000000002","timestamp":"2026-10-26T00:00:02.000Z",` + tail + "\n" +
		`{"type":"last-prompt","lastPrompt":"Write the plan for INFRA-1.","leafUuid":"00000000-0000-4000-8000-000000000002","sessionId":"s-planner"}` + "\n"
	s, err := claude.Read([]byte(content))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The answer's last model call, the one that offers no tool, is told it is the last (lastCall),
// after everything else it carries and with no cache mark, and no earlier call is. The exchange
// appended to the conversation holds no trace of that message: the question, the model's turns, a
// reply the form refused with the sentence it was asked again with, the reply, and nothing else.
// Oh My Pi's appender and Claude Code's each encode it with none of that message in what they
// write (the answer already validated it: finish faults an append that does not, and this reply
// has no fault). Two runs reach their last call: one whose first call reads with gh, so the
// message comes after the call's result, and one whose first reply the form refuses, so the last
// call is the one asked again and the message comes after the form's sentence.
func TestTheLastCallIsToldItIsTheLastAndTheAppendHoldsNoTraceOfIt(t *testing.T) {
	const answered = `{"kind":"answered","text":"PR 7 is green.","needs":""}`
	refused := `{"kind":"not_answered","text":"I cannot tell from here.","needs":""}`
	_, refusal := parseReply(json.RawMessage(refused), false)
	if refusal == nil {
		t.Fatal("parseReply takes a not_answered reply naming no need")
	}
	for _, c := range []struct {
		name  string
		first reply
		// after is the block the last call's message follows: a tool result, or a text.
		after string
	}{
		{name: "after a gh call", first: anthropicStream("tool_use", anthropicUsage{Input: 2, Output: 3}, anthropicGH("toolu_1", "pr", "view", "7")), after: "tool_result"},
		{name: "asked again on the last call", first: anthropicStream("end_turn", anthropicUsage{Input: 2, Output: 3}, anthropicText(refused)), after: refusal.Error()},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newStandIn(t, c.first, anthropicStream("end_turn", anthropicUsage{Input: 3, Output: 4}, anthropicText(answered)))
			reply, err := Answer(context.Background(), anthropicRequest(t, s, &stubGH{}, 2))
			if err != nil || reply.Fault != "" || reply.Kind != Answered || reply.Text != "PR 7 is green." {
				t.Fatalf("reply %+v, %v; want answered on the last call", reply, err)
			}
			got := s.requests()
			if len(got) != 2 {
				t.Fatalf("%d model calls, want 2", len(got))
			}
			if bytes.Contains(got[0].body, []byte(lastCall)) {
				t.Error("the first call, not the last, carries the last call's message")
			}
			sent := messages(t, got[1])
			newest := blocks(sent[len(sent)-1])
			if sent[len(sent)-1]["role"] != "user" || len(newest) != 2 || newest[1]["text"] != lastCall || newest[1]["cache_control"] != nil {
				t.Fatalf("the last call ends with %v, want the last call's message, unmarked, after one block", newest)
			}
			if before := newest[0]; before["type"] != c.after && before["text"] != c.after {
				t.Errorf("the last call's message follows %v, want %s", before, c.after)
			}

			appended := reply.Append
			if encoded, err := json.Marshal(appended); err != nil || bytes.Contains(encoded, []byte(lastCall)) {
				t.Errorf("the append %s (%v) holds the last call's message", encoded, err)
			}
			if len(appended.Turns) != 2 || !strings.Contains(appended.Question, "Is PR 7 green?") {
				t.Errorf("the append %+v, want the question and the two calls' turns", appended)
			}
			if asked := appended.Turns[0].AskedAgain; (c.after == refusal.Error()) != (asked == refusal.Error()) || appended.Turns[1].AskedAgain != "" {
				t.Errorf("the turns were asked again with %q and %q, want the form's sentence on a refused first reply alone", asked, appended.Turns[1].AskedAgain)
			}
			for name, encode := range map[string]func(conversation.Append, time.Time) ([]byte, error){
				"Oh My Pi": ompSession(t).Encode, "Claude Code": claudeSession(t).Encode,
			} {
				data, err := encode(appended, time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC))
				if err != nil {
					t.Fatalf("%s's appender refuses the append: %v", name, err)
				}
				if bytes.Contains(data, []byte(lastCall)) {
					t.Errorf("%s's appender writes the last call's message:\n%s", name, data)
				}
				for _, want := range []string{"Is PR 7 green?", "PR 7 is green.", c.after} {
					if want != "tool_result" && !bytes.Contains(data, []byte(want)) {
						t.Errorf("%s's appender writes no %q:\n%s", name, want, data)
					}
				}
			}
		})
	}
}

const openaiHistoryOne = "gAAAAhist+/one=="
const openaiHistoryTwo = "gAAAAhist+/two=="

// openaiConversation is a conversation on OpenAI Responses: a request, a turn of reasoning and a
// tool call, its result, and a turn of reasoning and an answer, each turn's native items as the
// session holds them.
func openaiConversation() conversation.Conversation {
	read := `{"id":"fc_h1","type":"function_call","status":"completed","call_id":"call_h1","name":"read","arguments":"{\"path\":\"note.txt\"}"}`
	native := func(items ...string) *conversation.Native {
		raw := make([]json.RawMessage, len(items))
		for i, item := range items {
			raw[i] = json.RawMessage(item)
		}
		return &conversation.Native{Kind: conversation.OpenAIResponsesHistory, Provider: "openai", Incremental: true, Items: raw}
	}
	return conversation.Conversation{
		Model: "openai/gpt-6.1-sol", API: openaiResponses, ThinkingLevel: "high",
		Messages: []conversation.Message{
			{Role: conversation.User, Content: []conversation.Block{{Type: conversation.Text, Text: "Read note.txt."}}},
			{
				Role: conversation.Assistant, Provider: "openai", API: openaiResponses, Model: "gpt-6.1-sol",
				Native:  native(openaiReasoning("rs_h1", openaiHistoryOne, "Reading the note"), read),
				Content: []conversation.Block{{Type: conversation.ToolCall, ToolCallID: "call_h1", ToolName: "read", Arguments: json.RawMessage(`{"path":"note.txt"}`)}},
			},
			{Role: conversation.Tool, Content: []conversation.Block{{Type: conversation.ToolResult, ToolCallID: "call_h1", ToolName: "read", Content: []conversation.Block{{Type: conversation.Text, Text: "plum"}}}}},
			{
				Role: conversation.Assistant, Provider: "openai", API: openaiResponses, Model: "gpt-6.1-sol",
				Native:  native(openaiReasoning("rs_h2", openaiHistoryTwo, "Answering"), openaiMessage("msg_h2", "It says plum.")),
				Content: []conversation.Block{{Type: conversation.Text, Text: "It says plum."}},
			},
		},
	}
}

func openaiRequest(t *testing.T, s *standIn, gh GH, modelCalls int) Request {
	t.Helper()
	_, credential := keyFile(t, "sk-openai-standin")
	return Request{
		Conversation: openaiConversation(),
		Agent:        Agent{Role: "implementer", Issue: "INFRA-2", Session: "s-impl", Phase: "testing", GHUsage: ghtool.Usage{Repository: ghrepo.MustParse("acme/widgets")}},
		ChangeNote:   "Dispatch status: in review.",
		Question:     Question{ID: "envoy:k-1", Text: "What did the note say?", Asker: "the tester", Where: "an Envoy message"},
		Providers:    map[config.AnswerProvider]config.AnswerEndpoint{config.AnswerOpenAI: {BaseURL: s.server.URL + "/v1", Credential: credential}},
		Prices:       map[string]config.AnswerPrice{"openai/gpt-6.1-sol": openaiPrice},
		Bounds:       testBounds(modelCalls),
		GH:           gh,
		Log:          slog.New(slog.DiscardHandler),
	}
}

// input is an OpenAI Responses request's input items.
func input(t *testing.T, r recorded) []map[string]any {
	t.Helper()
	var body struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(r.body, &body); err != nil {
		t.Fatal(err)
	}
	return body.Input
}

// An OpenAI Responses answer sends every reasoning item of the conversation with its id and
// encrypted content as the session stored them, sends messages and calls without the item ids and
// statuses Oh My Pi's replay drops, and gives the append each turn's own output items byte for
// byte, which is what the appender writes the turn from.
func TestAnOpenAIAnswerReplaysTheSessionsNativeItemsAndAppendsTheCallsOwn(t *testing.T) {
	stepOne := []string{openaiReasoning("rs_s1", "gAAAAstep+/one==", "Checking the PR"), openaiGH("fc_s1", "call_s1", "pr", "view", "7")}
	stepTwo := []string{openaiMessage("msg_s2", `{"kind":"answered","text":"It said plum.","needs":""}`)}
	s := newStandIn(t,
		openaiStream(openaiUsage{Input: 1000, CacheWrite: 900, Output: 50, Reasoning: 20}, "", stepOne...),
		openaiStream(openaiUsage{Input: 1100, Cached: 900, Output: 30, Reasoning: 0}, "", stepTwo...),
	)
	gh := &stubGH{}
	reply, err := Answer(context.Background(), openaiRequest(t, s, gh, 2))
	if err != nil {
		t.Fatal(err)
	}
	if reply.Fault != "" || reply.Kind != Answered || reply.Text != "It said plum." {
		t.Fatalf("reply %+v", reply)
	}
	got := s.requests()
	if len(got) != 2 {
		t.Fatalf("%d model calls, want 2", len(got))
	}
	for i, r := range got {
		if r.path != "/v1/responses" {
			t.Errorf("call %d went to %s", i+1, r.path)
		}
		for _, encrypted := range []string{openaiHistoryOne, openaiHistoryTwo} {
			if !bytes.Contains(r.body, []byte(`"encrypted_content":`+quote(encrypted))) {
				t.Errorf("call %d does not carry %s byte for byte", i+1, encrypted)
			}
		}
		var ids []string
		for _, item := range input(t, r) {
			switch item["type"] {
			case "reasoning":
				ids = append(ids, item["id"].(string))
			case "function_call", "message":
				if _, ok := item["id"]; ok {
					t.Errorf("call %d sends %s with its item id: %v", i+1, item["type"], item)
				}
				if _, ok := item["status"]; ok {
					t.Errorf("call %d sends %s with its status: %v", i+1, item["type"], item)
				}
			}
		}
		wantIDs := []string{"rs_h1", "rs_h2"}
		if i == 1 {
			wantIDs = append(wantIDs, "rs_s1")
		}
		if !reflect.DeepEqual(ids, wantIDs) {
			t.Errorf("call %d's reasoning items %v, want %v", i+1, ids, wantIDs)
		}
	}

	turns := reply.Append.Turns
	if len(turns) != 2 {
		t.Fatalf("%d turns", len(turns))
	}
	for i, want := range [][]string{stepOne, stepTwo} {
		var native []string
		for _, item := range turns[i].Native {
			native = append(native, string(item))
		}
		if !reflect.DeepEqual(native, want) {
			t.Errorf("turn %d's output items\n%v\nwant them as the API sent them\n%v", i+1, native, want)
		}
	}
	if call := turns[0].Content[len(turns[0].Content)-1]; call.Type != conversation.ToolCall || call.ToolCallID != "call_s1" {
		t.Errorf("the first turn's call %+v", call)
	}
}

// Every call goes to the base URL `answers.providers` names, never one the environment names, and
// carries the key that provider's key file holds when the call is made.
func TestEveryCallCarriesTheConfiguredEndpointAndTheKeyFilesKey(t *testing.T) {
	elsewhere := newStandIn(t)
	t.Setenv("OPENAI_BASE_URL", elsewhere.server.URL+"/v1")
	t.Setenv("ANTHROPIC_BASE_URL", elsewhere.server.URL)
	t.Setenv("OPENAI_API_KEY", "sk-from-the-environment")

	openaiEndpoint := newStandIn(t, openaiStream(openaiUsage{Input: 1, Output: 1}, "", openaiMessage("m", `{"kind":"answered","text":"Plum.","needs":""}`)))
	if reply, err := Answer(context.Background(), openaiRequest(t, openaiEndpoint, &stubGH{}, 1)); err != nil || reply.Kind != Answered {
		t.Fatalf("openai reply %+v, %v", reply, err)
	}
	if got := openaiEndpoint.requests()[0].header.Get("Authorization"); got != "Bearer sk-openai-standin" {
		t.Errorf("the OpenAI call's Authorization %q, want the configured key's", got)
	}

	s := newStandIn(t,
		anthropicStream("tool_use", anthropicUsage{Input: 1, Output: 1}, anthropicGH("toolu_1", "pr", "view", "7")),
		anthropicStream("end_turn", anthropicUsage{Input: 1, Output: 1}, anthropicText(`{"kind":"answered","text":"Yes.","needs":""}`)),
	)
	gh := &stubGH{}
	req := anthropicRequest(t, s, gh, 2)
	path := req.Providers[config.AnswerAnthropic].Credential.File
	gh.onCall = func(int) {
		if err := os.WriteFile(path, []byte("sk-ant-rotated\n"), 0o600); err != nil {
			t.Error(err)
		}
	}
	if reply, err := Answer(context.Background(), req); err != nil || reply.Kind != Answered {
		t.Fatalf("anthropic reply %+v, %v", reply, err)
	}
	var keys []string
	for _, r := range s.requests() {
		keys = append(keys, r.header.Get("X-Api-Key"))
	}
	if !reflect.DeepEqual(keys, []string{"sk-ant-standin", "sk-ant-rotated"}) {
		t.Errorf("the calls carried keys %v, want the key file's at each call", keys)
	}
	if n := len(elsewhere.requests()); n != 0 {
		t.Errorf("the environment's base URL got %d requests", n)
	}
}

// Each way a run ends short of an answer is no answer under the daemon's own name for it, which
// is the reason the controller reads and the daemon's reply to the asker names, and the turn its
// one call returned is kept for the caller to append: none where the call returned nothing, a
// provider that failed or a reply past the time bound, or an empty refusal.
func TestEveryAgentloopFaultIsNoAnswerUnderTheDaemonsOwnName(t *testing.T) {
	answered := `{"kind":"answered","text":"Yes.","needs":""}`
	for _, c := range []struct {
		name    string
		replies []reply
		openai  bool
		answer  time.Duration
		want    Fault
		// nothing is a run whose call returned nothing to append.
		nothing bool
	}{
		{name: "cut off", replies: []reply{anthropicStream("max_tokens", anthropicUsage{Output: 4000}, anthropicText(`{"kind":"answered","text":"Ye`))}, want: Length},
		{name: "refused", replies: []reply{anthropicStream("refusal", anthropicUsage{Output: 1}, anthropicText(""))}, want: Refusal, nothing: true},
		{name: "OpenAI refusal", openai: true, replies: []reply{openaiStream(openaiUsage{Output: 1}, "",
			`{"id":"msg_r","type":"message","role":"assistant","status":"completed","content":[{"type":"refusal","refusal":"I can't help with that."}]}`)}, want: Refusal},
		{name: "OpenAI cut off", openai: true, replies: []reply{openaiStream(openaiUsage{Output: 4000}, "max_output_tokens", openaiMessage("msg_l", `{"kind":"answered","text":"Ye`))}, want: Length},
		{name: "prose", replies: []reply{anthropicStream("end_turn", anthropicUsage{Output: 1}, anthropicText("PR 7 is green."))}, want: Malformed},
		{name: "fenced", replies: []reply{anthropicStream("end_turn", anthropicUsage{Output: 1}, anthropicText("```json\n"+answered+"\n```"))}, want: Malformed},
		{name: "two objects", replies: []reply{anthropicStream("end_turn", anthropicUsage{Output: 1}, anthropicText(answered+answered))}, want: Malformed},
		{name: "a field the form does not hold", replies: []reply{anthropicStream("end_turn", anthropicUsage{Output: 1}, anthropicText(`{"kind":"answered","text":"Yes.","needs":"","aside":"and a pear"}`))}, want: Malformed},
		{name: "unknown kind", replies: []reply{anthropicStream("end_turn", anthropicUsage{Output: 1}, anthropicText(`{"kind":"maybe","text":"Yes.","needs":""}`))}, want: Malformed},
		{name: "none not offered", replies: []reply{anthropicStream("end_turn", anthropicUsage{Output: 1}, anthropicText(`{"kind":"none","text":"","needs":""}`))}, want: Malformed},
		{name: "not answered naming no need", replies: []reply{anthropicStream("end_turn", anthropicUsage{Output: 1}, anthropicText(`{"kind":"not_answered","text":"I cannot tell.","needs":""}`))}, want: Malformed},
		{name: "answered with no text", replies: []reply{anthropicStream("end_turn", anthropicUsage{Output: 1}, anthropicText(`{"kind":"answered","text":"  ","needs":""}`))}, want: Malformed},
		{name: "a placeholder on the last allowed call", replies: []reply{anthropicStream("end_turn", anthropicUsage{Output: 1}, anthropicText(`{"kind":"answered","text":"x","needs":""}`))}, want: Malformed},
		{name: "the provider failed", replies: []reply{{status: http.StatusBadRequest, body: []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`)}}, want: ProviderError, nothing: true},
		{name: "past the time bound", replies: []reply{{delay: 5 * time.Second, body: anthropicStream("end_turn", anthropicUsage{}, anthropicText(answered)).body}}, answer: time.Second, want: AnswerSeconds, nothing: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newStandIn(t, c.replies...)
			req := anthropicRequest(t, s, &stubGH{}, 1)
			if c.openai {
				req = openaiRequest(t, s, &stubGH{}, 1)
			}
			if c.answer > 0 {
				req.Bounds.Answer = c.answer
			}
			got, err := Answer(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			turns := 1
			if c.nothing {
				turns = 0
			}
			if got.Fault != c.want || got.Kind != NotAnswered || got.Text != "" || len(got.Append.Turns) != turns {
				t.Errorf("reply %+v, want no answer with fault %s and %d turn(s) to append", got, c.want, turns)
			}
		})
	}
}

// A reply the form's own rules refuse where the strict schema cannot, a not_answered reply naming
// no need, is asked for once again: agentloop sends parseReply's sentence as the message after it,
// within the answer's model_calls, and the reply to it is the agent's. Both calls are counted and
// priced, and the append holds the refused reply, the sentence and the reply after it, in the
// order the run made them, which is what the appender writes into the conversation. The form the
// model is held to states the rule it broke in its own descriptions.
func TestAReplyTheFormRefusesIsAskedForOnceAgain(t *testing.T) {
	const (
		refused = `{"kind":"not_answered","text":"I cannot tell from here.","needs":""}`
		needs   = "My working copy, to run the checks."
	)
	_, refusal := parseReply(json.RawMessage(refused), false)
	if refusal == nil {
		t.Fatal("parseReply takes a not_answered reply naming no need")
	}
	s := newStandIn(t,
		anthropicStream("end_turn", anthropicUsage{Input: 10, Output: 20}, anthropicThinking("I cannot see the checks.", "sig-refused+/=="), anthropicText(refused)),
		anthropicStream("end_turn", anthropicUsage{Input: 30, Output: 25}, anthropicText(`{"kind":"not_answered","text":"I cannot tell from here.","needs":"`+needs+`"}`)),
	)
	reply, err := Answer(context.Background(), anthropicRequest(t, s, &stubGH{}, 3))
	if err != nil {
		t.Fatal(err)
	}
	if reply.Fault != "" || reply.Kind != NotAnswered || reply.Text != "I cannot tell from here." || reply.Needs != needs {
		t.Fatalf("reply %+v, want not_answered needing %q", reply, needs)
	}
	if reply.Calls != 2 || reply.Usage.InputTokens != 40 || reply.Usage.OutputTokens != 45 {
		t.Errorf("calls %d, usage %+v; want both calls counted", reply.Calls, reply.Usage)
	}
	got := s.requests()
	if len(got) != 2 {
		t.Fatalf("%d model calls, want 2", len(got))
	}
	// The second call's messages: the history's request and answer (0, 1), the question (2), the
	// refused reply (3) and the sentence it was asked again with (4).
	sent := messages(t, got[1])
	if len(sent) != 5 || sent[4]["role"] != "user" || blocks(sent[4])[0]["text"] != refusal.Error() {
		t.Fatalf("the second call's messages %v, want the refused reply and parseReply's sentence after it", sent)
	}
	if turn := blocks(sent[3]); len(turn) != 2 || turn[0]["signature"] != "sig-refused+/==" || turn[1]["text"] != refused {
		t.Errorf("the second call carries the refused reply as %v, want its signed thinking and its text", turn)
	}
	var body struct {
		OutputConfig struct {
			Format struct {
				Schema struct {
					Properties map[string]struct {
						Description string `json:"description"`
					} `json:"properties"`
				} `json:"schema"`
			} `json:"format"`
		} `json:"output_config"`
	}
	if err := json.Unmarshal(got[0].body, &body); err != nil {
		t.Fatal(err)
	}
	properties := body.OutputConfig.Format.Schema.Properties
	if text := properties["text"].Description; !strings.Contains(text, "Never empty") {
		t.Errorf("the form's text says %q, want it never empty", text)
	}
	if needs := properties["needs"].Description; !strings.Contains(needs, "not_answered") || !strings.Contains(needs, "needs_work") || !strings.Contains(needs, "Never empty for those two kinds") {
		t.Errorf("the form's needs says %q, want it never empty for not_answered and needs_work", needs)
	}
	turns := reply.Append.Turns
	if len(turns) != 2 || turns[0].AskedAgain != refusal.Error() || turns[1].AskedAgain != "" {
		t.Fatalf("turns %+v, want the refused reply asked again with parseReply's sentence, then the reply", turns)
	}
	if first := turns[0].Content; len(first) != 2 || first[0].Signature != "sig-refused+/==" || first[1].Text != refused {
		t.Errorf("the refused reply's turn %+v, want its signed thinking and its text", first)
	}
	if second := turns[1].Content; len(second) != 1 || !strings.Contains(second[0].Text, needs) {
		t.Errorf("the reply's turn %+v, want the reply naming the need", second)
	}
	if err := reply.Append.Validate(); err != nil {
		t.Errorf("the append does not validate: %v", err)
	}
}

// anthropicAnswer is a model turn on the Anthropic conversation that says text.
func anthropicAnswer(text string) conversation.Message {
	return conversation.Message{
		Role: conversation.Assistant, Provider: "anthropic", API: anthropicMessages, Model: "claude-standin",
		Content: []conversation.Block{{Type: conversation.Text, Text: text}},
	}
}

// A reply of any kind but none whose text is a placeholder, one word of letters alone that is
// neither yes nor no and no word of the conversation, the question or the run's gh results, is
// asked for once again with the placeholder sentence, as a reply the form's other rules refuse
// is, and the reply after it is the one posted. Neither the request's history nor its question
// holds any of the placeholders, but for the guard's own exchange in one case: an earlier answer
// whose x was refused with the sentence and then answered, as finish appends it, whose refused
// reply and sentence the guard does not read.
func TestAPlaceholderReplyIsAskedForOnceAgain(t *testing.T) {
	const sentence = "your reply's text is a placeholder: answer the question from your conversation"
	for _, c := range []struct {
		name, refused, second string
		kind                  Kind
		text, needs           string
		// history changes the request's conversation.
		history func(*conversation.Conversation)
	}{
		{
			name: "answered x", refused: `{"kind":"answered","text":"x","needs":""}`,
			second: `{"kind":"answered","text":"PR 7 is green.","needs":""}`, kind: Answered, text: "PR 7 is green.",
		},
		{
			name: "answered x refused once before", refused: `{"kind":"answered","text":"x","needs":""}`,
			second: `{"kind":"answered","text":"PR 7 is green.","needs":""}`, kind: Answered, text: "PR 7 is green.",
			history: func(c *conversation.Conversation) {
				c.Messages = append(c.Messages,
					conversation.Message{Role: conversation.User, Content: []conversation.Block{{Type: conversation.Text, Text: "Is the plan written?"}}},
					anthropicAnswer(`{"kind":"answered","text":"x","needs":""}`),
					conversation.Message{Role: conversation.User, Content: []conversation.Block{{Type: conversation.Text, Text: sentence}}},
					anthropicAnswer(`{"kind":"answered","text":"The plan is written.","needs":""}`),
				)
			},
		},
		{
			name: "answered placeholder", refused: `{"kind":"answered","text":"placeholder","needs":""}`,
			second: `{"kind":"answered","text":"PR 7 is green.","needs":""}`, kind: Answered, text: "PR 7 is green.",
		},
		{
			name: "not_answered tbd", refused: `{"kind":"not_answered","text":"tbd","needs":"My working copy."}`,
			second: `{"kind":"not_answered","text":"I cannot tell from here.","needs":"My working copy."}`,
			kind:   NotAnswered, text: "I cannot tell from here.", needs: "My working copy.",
		},
		{
			name: "needs_work x", refused: `{"kind":"needs_work","text":"x","needs":"A run of my phase."}`,
			second: `{"kind":"needs_work","text":"I cannot run the checks again now.","needs":"A run of my phase."}`,
			kind:   NeedsWork, text: "I cannot run the checks again now.", needs: "A run of my phase.",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newStandIn(t,
				anthropicStream("end_turn", anthropicUsage{Input: 10, Output: 20}, anthropicText(c.refused)),
				anthropicStream("end_turn", anthropicUsage{Input: 30, Output: 25}, anthropicText(c.second)),
			)
			req := anthropicRequest(t, s, &stubGH{}, 3)
			if c.history != nil {
				c.history(&req.Conversation)
			}
			reply, err := Answer(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if reply.Fault != "" || reply.Kind != c.kind || reply.Text != c.text || reply.Needs != c.needs {
				t.Fatalf("reply %+v, want the second reply: %s %q needing %q", reply, c.kind, c.text, c.needs)
			}
			got := s.requests()
			if len(got) != 2 || reply.Calls != 2 {
				t.Fatalf("%d model calls (reply calls %d), want 2", len(got), reply.Calls)
			}
			sent := messages(t, got[1])
			last := sent[len(sent)-1]
			if said := blocks(last); last["role"] != "user" || len(said) != 1 || said[0]["text"] != sentence {
				t.Fatalf("the second call ends with %v, want the placeholder sentence as a user message", last)
			}
			if turns := reply.Append.Turns; len(turns) != 2 || turns[0].AskedAgain != sentence || turns[1].AskedAgain != "" {
				t.Errorf("turns %+v, want the placeholder asked again with the sentence, then the reply", turns)
			}
		})
	}
}

// An exchange stored with an earlier placeholder sentence, one placeholderSentences holds beside
// the current one, is left out as one stored with the current sentence is: the x its refused
// reply holds is no word the agent has in front of it, so a reply of x is asked for once again.
// The earlier text is the test's own entry; askedAgain reads the set.
func TestAnExchangeAskedAgainWithAnEarlierSentenceIsLeftOut(t *testing.T) {
	const earlier = "your reply is a placeholder: answer from your conversation"
	current := placeholderSentences
	placeholderSentences = append(slices.Clone(current), earlier)
	t.Cleanup(func() { placeholderSentences = current })
	s := newStandIn(t,
		anthropicStream("end_turn", anthropicUsage{Input: 10, Output: 20}, anthropicText(`{"kind":"answered","text":"x","needs":""}`)),
		anthropicStream("end_turn", anthropicUsage{Input: 30, Output: 25}, anthropicText(`{"kind":"answered","text":"PR 7 is green.","needs":""}`)),
	)
	req := anthropicRequest(t, s, &stubGH{}, 3)
	req.Conversation.Messages = append(req.Conversation.Messages,
		conversation.Message{Role: conversation.User, Content: []conversation.Block{{Type: conversation.Text, Text: "Is the plan written?"}}},
		anthropicAnswer(`{"kind":"answered","text":"x","needs":""}`),
		conversation.Message{Role: conversation.User, Content: []conversation.Block{{Type: conversation.Text, Text: earlier}}},
		anthropicAnswer(`{"kind":"answered","text":"The plan is written.","needs":""}`),
	)
	reply, err := Answer(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(s.requests()); n != 2 || reply.Kind != Answered || reply.Text != "PR 7 is green." {
		t.Fatalf("%d model calls, reply %s %q; want the x asked for once again and the second reply taken", n, reply.Kind, reply.Text)
	}
}

// ghExchange changes the Anthropic conversation's answer to end on a gh call whose input is the
// raw JSON input, then the call's result output and one more turn of the agent's.
func ghExchange(input, output string) func(*conversation.Conversation) {
	return func(c *conversation.Conversation) {
		c.Messages[1].Content = append(c.Messages[1].Content, conversation.Block{
			Type: conversation.ToolCall, ToolCallID: "toolu_h1", ToolName: "gh", Arguments: json.RawMessage(input),
		})
		c.Messages = append(c.Messages,
			conversation.Message{Role: conversation.Tool, Content: []conversation.Block{{
				Type: conversation.ToolResult, ToolCallID: "toolu_h1", ToolName: "gh",
				Content: []conversation.Block{{Type: conversation.Text, Text: output}},
			}}},
			anthropicAnswer("Done."),
		)
	}
}

// A terse right reply is no placeholder and is posted at once, never asked for again: a text with
// a digit, a punctuation mark or a space in it, a bare yes or no, a word of letters alone that
// only the question holds ("Is PR 7 green?"), and one that the conversation's history holds (in
// another case, or only behind a JSON escape in a tool call's input or a tool result's raw JSON)
// each on one model call, and a word of letters alone that only a gh result of the run holds on
// two, the gh call's and the reply's. No text of the request holds the word no. A none reply to a
// message that may ask nothing is never held to the rule: its text, a word of letters alone that
// no text of the request holds, is taken on one model call.
func TestATerseReplyIsPostedAtOnce(t *testing.T) {
	for _, c := range []struct {
		text string
		// kind is the reply's kind, Answered when empty, and mayAskNothing the request's
		// MayAskNothing.
		kind          Kind
		mayAskNothing bool
		// history changes the request's conversation to hold the reply's word.
		history func(*conversation.Conversation)
		// gh is the args of a gh call the model makes before it replies, whose output holds the
		// reply's word.
		gh []string
	}{
		{text: "Yes, merged in #12."},
		{text: "SBOM-ZINC-512"},
		{text: "No."},
		{text: "No"},
		{text: "yes"},
		{text: "4f1c2ab"},
		{text: "go 1.22"},
		{text: "green"},
		{text: "bramble", history: func(c *conversation.Conversation) {
			c.Messages[1].Content[1].Text = "The plan is written, on the branch named Bramble."
		}},
		{text: "Medlar", history: ghExchange(`{"args":["pr","comment","7","--body","Merged.\nMedlar is next."]}`, "Commented.")},
		{text: "Sloe", history: ghExchange(`{"args":["pr","view","7","--json","body"]}`, `{"body":"Merged.\nSloe is next."}`)},
		{text: "Quince", gh: []string{"release", "view", "quince", "-R", "acme/widgets"}},
		{text: "Acknowledged", kind: None, mayAskNothing: true},
	} {
		t.Run(c.text, func(t *testing.T) {
			kind := c.kind
			if kind == "" {
				kind = Answered
			}
			var replies []reply
			if c.gh != nil {
				replies = append(replies, anthropicStream("tool_use", anthropicUsage{Input: 2, Output: 3}, anthropicGH("toolu_1", c.gh...)))
			}
			replies = append(replies, anthropicStream("end_turn", anthropicUsage{Input: 2, Output: 3},
				anthropicText(`{"kind":`+quote(string(kind))+`,"text":`+quote(c.text)+`,"needs":""}`)))
			s := newStandIn(t, replies...)
			gh := &stubGH{}
			req := anthropicRequest(t, s, gh, 3)
			req.MayAskNothing = c.mayAskNothing
			if c.history != nil {
				c.history(&req.Conversation)
			}
			reply, err := Answer(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if reply.Fault != "" || reply.Kind != kind || reply.Text != c.text {
				t.Fatalf("reply %+v, want %s %q", reply, kind, c.text)
			}
			if n := len(s.requests()); n != len(replies) || reply.Calls != len(replies) || len(gh.made()) != len(replies)-1 {
				t.Errorf("%d model calls (reply calls %d) and %d gh calls, want %d model calls", n, reply.Calls, len(gh.made()), len(replies))
			}
		})
	}
}

// cutReply is a reply of the form the provider cut at output_tokens, and cutSays what it says: a
// whole answer inside the form's text, then the newline escapes a model running on inside the
// form writes until the bound stops it, as the one reply the bound cut in 415 measured calls was.
// No other reply says cutSays, so a request or an append holding it holds the cut reply.
const cutSays = "PR 7 was green at abc123."

var cutReply = `{"kind":"answered","text":"` + cutSays + strings.Repeat(`\n`, 64)

// A reply the provider cut at output_tokens is not the agent's and is never sent back: agentloop
// makes that call once again without it, inside the same model_calls, and the reply to the call
// made again is posted, on both provider APIs. Every call is counted and priced, the cut one's
// 4000 output tokens included, and the append holds the turns of every other call, the gh call
// before the cut and the reply, with nothing of the cut reply in it or in what Oh My Pi's and
// Claude Code's appenders write, so the conversation never holds a reply no later call was sent.
func TestACutReplyIsMadeAgainAndTheAppendHoldsNoTraceOfIt(t *testing.T) {
	const answered = `{"kind":"answered","text":"PR 7 is green.","needs":""}`
	for _, c := range []struct {
		name    string
		openai  bool
		replies []reply
	}{
		{name: "anthropic-messages", replies: []reply{
			anthropicStream("tool_use", anthropicUsage{Input: 10, Output: 20}, anthropicThinking("Check the PR.", "sig-cut-1+/=="), anthropicGH("toolu_1", "pr", "view", "7")),
			anthropicStream("max_tokens", anthropicUsage{Input: 30, Output: 4000}, anthropicText(cutReply)),
			anthropicStream("end_turn", anthropicUsage{Input: 30, Output: 25}, anthropicText(answered)),
		}},
		{name: "openai-responses", openai: true, replies: []reply{
			openaiStream(openaiUsage{Input: 10, Output: 20}, "", openaiReasoning("rs_c1", "gAAAAcut+/one==", "Checking the PR"), openaiGH("fc_c1", "call_c1", "pr", "view", "7")),
			openaiStream(openaiUsage{Input: 30, Output: 4000}, "max_output_tokens", openaiMessage("msg_cut", cutReply)),
			openaiStream(openaiUsage{Input: 30, Output: 25}, "", openaiMessage("msg_answer", answered)),
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newStandIn(t, c.replies...)
			req, price := anthropicRequest(t, s, &stubGH{}, 4), anthropicPrice
			if c.openai {
				req, price = openaiRequest(t, s, &stubGH{}, 4), openaiPrice
			}
			reply, err := Answer(context.Background(), req)
			if err != nil || reply.Fault != "" || reply.Kind != Answered || reply.Text != "PR 7 is green." {
				t.Fatalf("reply %+v, %v; want the reply to the call made again, answered", reply, err)
			}
			want := (70*price.Input + 4045*price.Output) / 1e6
			if u := reply.Usage; reply.Calls != 3 || u.InputTokens != 70 || u.OutputTokens != 4045 || math.Abs(reply.CostUSD-want) > 1e-12 {
				t.Errorf("calls %d, usage %+v, cost $%.10f; want all three calls counted and priced at $%.10f, the cut one included", reply.Calls, u, reply.CostUSD, want)
			}
			got := s.requests()
			if len(got) != 3 {
				t.Fatalf("%d model calls, want 3", len(got))
			}
			for i, r := range got {
				if bytes.Contains(r.body, []byte(cutSays)) {
					t.Errorf("call %d carries the cut reply", i+1)
				}
			}
			appended := reply.Append
			if encoded, err := json.Marshal(appended); err != nil || bytes.Contains(encoded, []byte(cutSays)) {
				t.Errorf("the append %s (%v) holds the cut reply", encoded, err)
			}
			if turns := appended.Turns; len(turns) != 2 || turns[0].Usage.OutputTokens != 20 || turns[1].Usage.OutputTokens != 25 {
				t.Fatalf("turns %+v, want the gh call's and the reply's, not the cut call's", turns)
			}
			if err := appended.Validate(); err != nil {
				t.Fatalf("the append does not validate: %v", err)
			}
			if c.openai {
				return
			}
			for name, encode := range map[string]func(conversation.Append, time.Time) ([]byte, error){
				"Oh My Pi": ompSession(t).Encode, "Claude Code": claudeSession(t).Encode,
			} {
				data, err := encode(appended, time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC))
				if err != nil {
					t.Fatalf("%s's appender refuses the append: %v", name, err)
				}
				if bytes.Contains(data, []byte(cutSays)) {
					t.Errorf("%s's appender writes the cut reply:\n%s", name, data)
				}
				if !bytes.Contains(data, []byte("PR 7 is green.")) {
					t.Errorf("%s's appender writes no reply:\n%s", name, data)
				}
			}
		})
	}
}

// A reply cut at output_tokens again after the call made again is no answer under length, as a
// cut on the last model call model_calls allows is: the answer made each call it could, counts
// and prices them all, and keeps the last cut reply alone to append, the one made again dropped.
func TestACutReplyCutAgainIsALengthFault(t *testing.T) {
	cut := func() reply {
		return anthropicStream("max_tokens", anthropicUsage{Input: 30, Output: 4000}, anthropicText(cutReply))
	}
	answered := anthropicStream("end_turn", anthropicUsage{Input: 30, Output: 25}, anthropicText(`{"kind":"answered","text":"PR 7 is green.","needs":""}`))
	for _, c := range []struct {
		name       string
		modelCalls int
		replies    []reply
		// calls is how many model calls the answer makes.
		calls int
	}{
		{name: "cut twice", modelCalls: 3, replies: []reply{cut(), cut(), answered}, calls: 2},
		{name: "cut on the last allowed call", modelCalls: 1, replies: []reply{cut(), answered}, calls: 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newStandIn(t, c.replies...)
			reply, err := Answer(context.Background(), anthropicRequest(t, s, &stubGH{}, c.modelCalls))
			if err != nil {
				t.Fatal(err)
			}
			if reply.Fault != Length || reply.Kind != NotAnswered || reply.Text != "" {
				t.Fatalf("reply %+v, want no answer with fault %s", reply, Length)
			}
			if turns := reply.Append.Turns; len(turns) != 1 || turns[0].Content[0].Text != cutReply {
				t.Fatalf("turns %+v, want the last cut reply's alone", turns)
			}
			if n := len(s.requests()); n != c.calls || reply.Calls != c.calls || reply.Usage.OutputTokens != int64(4000*c.calls) {
				t.Errorf("%d model calls, reply calls %d, usage %+v; want %d cut calls counted", n, reply.Calls, reply.Usage, c.calls)
			}
		})
	}
}

// A run no model call of which the provider metered is no answer either: a provider that sends
// no usage, or a gateway between it and the daemon that strips it, leaves the answer unpriced,
// so it would cost the project's daily bound nothing however many calls it made (Unmetered).
// Both readings of a stripped usage object are that run: the Responses stream whose completed
// response carries none, which agentloop reports as no counts at all (Usage.Metered false), and
// the Messages stream whose message carries none, which the Anthropic adapter answers with zero
// counts, so the zeros are what say it. The reply the model gave in the form goes with either:
// its turn is kept to append as any fault's is, and the question is the controller's, as an
// unpriced model's is. The same stream with its usage object answers and is priced, so it is the
// counts and nothing else that decide.
func TestARunTheProviderMeteredNothingForIsNoAnswer(t *testing.T) {
	const form = `{"kind":"answered","text":"PR 7 is green.","needs":""}`
	for _, c := range []struct {
		name      string
		unmetered reply
		metered   reply
		openai    bool
	}{
		{name: "a Responses stream reporting no counts at all", openai: true,
			unmetered: openaiUnmetered(openaiMessage("msg_1", form)),
			metered:   openaiStream(openaiUsage{Input: 2, Output: 3}, "", openaiMessage("msg_1", form))},
		{name: "a Messages stream whose counts are all zero",
			unmetered: anthropicUnmetered("end_turn", anthropicText(form)),
			metered:   anthropicStream("end_turn", anthropicUsage{Input: 2, Output: 3}, anthropicText(form))},
	} {
		t.Run(c.name, func(t *testing.T) {
			request := anthropicRequest
			if c.openai {
				request = openaiRequest
			}
			s := newStandIn(t, c.unmetered)
			got, err := Answer(context.Background(), request(t, s, &stubGH{}, 2))
			switch {
			case err != nil:
				t.Fatal(err)
			case got.Fault != Unmetered || got.Kind != NotAnswered || got.Text != "" || len(got.Append.Turns) != 1:
				t.Fatalf("reply = %+v; want no answer, faulted %s, with the one call's turn to append", got, Unmetered)
			case got.Calls != 1 || got.CostUSD != 0 || got.Usage != (conversation.Usage{}):
				t.Fatalf("reply = %+v; want the one call it made, no usage and no cost", got)
			}
			metered := newStandIn(t, c.metered)
			switch priced, err := Answer(context.Background(), request(t, metered, &stubGH{}, 2)); {
			case err != nil:
				t.Fatal(err)
			case priced.Fault != "" || priced.Kind != Answered || priced.Text != "PR 7 is green." || priced.CostUSD <= 0:
				t.Fatalf("the metered stand-in's reply = %+v; want the answer it gave, priced", priced)
			}
		})
	}
}

// The last allowed call still calling a tool is no answer past the model_calls bound.
func TestTheLastCallStillCallingAToolIsPastTheModelCallsBound(t *testing.T) {
	s := newStandIn(t, anthropicStream("tool_use", anthropicUsage{Output: 1}, anthropicGH("toolu_1", "pr", "view", "7")))
	got, err := Answer(context.Background(), anthropicRequest(t, s, &stubGH{}, 1))
	if err != nil || got.Fault != ModelCalls || got.Calls != 1 {
		t.Errorf("reply %+v, %v; want no answer past model_calls after 1 call", got, err)
	}
}

// A run that writes with gh and then passes model_calls keeps its turns for the caller to append:
// the model merges the pull request, then still calls gh on the last call the bound allows. The
// reply is no answer past model_calls, and its append holds the question and both calls' turns,
// each gh call with its result, the merge among them, and nothing of the last call's message.
// Ended with the caller's note (conversation.Append.NoAnswer), Oh My Pi's and Claude Code's
// appenders write it, the merge and its result before the note, so the agent's conversation holds
// the write it made before the fault.
func TestAWriteBeforeAModelCallsFaultIsKeptToAppend(t *testing.T) {
	merge := []string{"pr", "merge", "7", "-R", "acme/widgets", "--squash"}
	s := newStandIn(t,
		anthropicStream("tool_use", anthropicUsage{Input: 10, Output: 5}, anthropicThinking("Merge it.", "sig-merge"), anthropicGH("toolu_1", merge...)),
		anthropicStream("tool_use", anthropicUsage{Input: 12, Output: 4}, anthropicGH("toolu_2", "pr", "view", "7", "-R", "acme/widgets")),
	)
	gh := &stubGH{}
	reply, err := Answer(context.Background(), anthropicRequest(t, s, gh, 2))
	if err != nil || reply.Fault != ModelCalls || reply.Kind != NotAnswered || reply.Text != "" || reply.Calls != 2 {
		t.Fatalf("reply %+v, %v; want no answer past model_calls after 2 calls", reply, err)
	}
	if calls := gh.made(); len(calls) != 2 {
		t.Fatalf("gh calls %+v, want the merge and the last call's read", calls)
	}
	args, _ := json.Marshal(merge)
	appended := reply.Append
	turns := appended.Turns
	if len(turns) != 2 || appended.QuestionID != "dispatch:m-1" || !strings.Contains(appended.Question, "Is PR 7 green?") {
		t.Fatalf("append %+v, want the question and both calls' turns", appended)
	}
	if call := turns[0].Content[1]; call.Type != conversation.ToolCall || !bytes.Contains(call.Arguments, args) {
		t.Errorf("the first turn's call %+v, want the merge", call)
	}
	if results := turns[0].Results; len(results) != 1 || results[0].Content[0].Text != "output of "+string(args) {
		t.Errorf("the first turn's results %+v, want the merge's output", results)
	}
	if len(turns[1].Results) != 1 {
		t.Errorf("the last turn's results %+v, want the read's", turns[1].Results)
	}
	if encoded, err := json.Marshal(appended); err != nil || bytes.Contains(encoded, []byte(lastCall)) {
		t.Errorf("the append %s (%v) holds the last call's message", encoded, err)
	}
	const note = "The daemon posted its own reply in your name."
	appended.NoAnswer = note
	for name, encode := range map[string]func(conversation.Append, time.Time) ([]byte, error){
		"Oh My Pi": ompSession(t).Encode, "Claude Code": claudeSession(t).Encode,
	} {
		data, err := encode(appended, time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC))
		if err != nil {
			t.Fatalf("%s's appender refuses the faulted run's append: %v", name, err)
		}
		call, result := bytes.Index(data, []byte(`"args":`+string(args))), bytes.Index(data, []byte(`output of [\"pr\",\"merge\"`))
		if call < 0 || result < call || bytes.LastIndex(data, []byte(note)) < result || bytes.Contains(data, []byte(lastCall)) {
			t.Errorf("%s's appender writes no merge, its result and then the note, or writes the last call's message:\n%s", name, data)
		}
	}
}

// A conversation on a provider the configuration does not name, on a model it does not price, or
// holding history the loop cannot send is no answer, and no model call is made.
func TestNoProviderNoPriceAndAnUnknownFormatMakeNoModelCall(t *testing.T) {
	s := newStandIn(t)
	for _, c := range []struct {
		name   string
		change func(*Request)
		want   Fault
	}{
		{name: "provider not configured", change: func(r *Request) { delete(r.Providers, config.AnswerAnthropic) }, want: NoProvider},
		{name: "API the loop does not call", change: func(r *Request) { r.Conversation.API = "openai-completions" }, want: NoProvider},
		{name: "model not priced", change: func(r *Request) { r.Conversation.Model = "anthropic/claude-unpriced" }, want: NoPrice},
		{name: "provider-side compaction", change: func(r *Request) {
			r.Conversation.Messages[0].Native = &conversation.Native{Kind: conversation.AnthropicCompaction, Provider: "anthropic"}
		}, want: UnknownFormat},
	} {
		t.Run(c.name, func(t *testing.T) {
			req := anthropicRequest(t, s, &stubGH{}, 1)
			c.change(&req)
			got, err := Answer(context.Background(), req)
			if err != nil || got.Fault != c.want || got.Kind != NotAnswered || got.Calls != 0 {
				t.Errorf("reply %+v, %v; want no answer with %s", got, err, c.want)
			}
		})
	}
	if n := len(s.requests()); n != 0 {
		t.Errorf("%d model calls were made", n)
	}
}

// None is offered only for a message that may ask nothing, in the prompt and in the reply form
// the model is held to, and is then a reply.
func TestNoneIsOfferedOnlyForAMessageThatMayAskNothing(t *testing.T) {
	s := newStandIn(t, anthropicStream("end_turn", anthropicUsage{Output: 1}, anthropicText(`{"kind":"none","text":"","needs":""}`)))
	req := anthropicRequest(t, s, &stubGH{}, 1)
	req.MayAskNothing = true
	got, err := Answer(context.Background(), req)
	if err != nil || got.Kind != None || got.Fault != "" {
		t.Fatalf("reply %+v, %v; want none", got, err)
	}
	var body struct {
		System []struct {
			Text string `json:"text"`
		} `json:"system"`
	}
	if err := json.Unmarshal(s.requests()[0].body, &body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body.System[0].Text, `"none"`) {
		t.Errorf("the system prompt offers no none: %s", body.System[0].Text)
	}
	if !bytes.Contains(s.requests()[0].body, []byte(`\"none\"`)) && !bytes.Contains(s.requests()[0].body, []byte(`"none"`)) {
		t.Error("the call carries no reply form offering none")
	}
	if strings.Contains(systemPrompt(anthropicRequest(t, s, &stubGH{}, 1)), `"none"`) {
		t.Error("the system prompt offers none to a message that asks something")
	}
	if strings.Contains(string(finalSchema(false)), `"none"`) {
		t.Errorf("the reply form of a message that asks something offers none: %s", finalSchema(false))
	}
}

// On a re-run the question carries the gh calls already made with their results, a call with no
// result of unknown outcome, and new calls are numbered after them.
func TestARerunCarriesTheEarlierCallsAndNumbersNewCallsAfterThem(t *testing.T) {
	s := newStandIn(t,
		anthropicStream("tool_use", anthropicUsage{Output: 1}, anthropicGH("toolu_1", "pr", "view", "7")),
		anthropicStream("end_turn", anthropicUsage{Output: 1}, anthropicText(`{"kind":"answered","text":"Done.","needs":""}`)),
	)
	gh := &stubGH{}
	req := anthropicRequest(t, s, gh, 2)
	output, exit := "comment posted", 0
	req.Earlier = []store.AnswerToolCall{
		{Seq: 1, Tool: store.ToolGH, Args: []string{"pr", "comment", "7", "--body", "on it"}, Result: &output, Exit: &exit},
		{Seq: 2, Tool: store.ToolGH, Args: []string{"pr", "review", "7", "--approve"}},
	}
	if got, err := Answer(context.Background(), req); err != nil || got.Kind != Answered {
		t.Fatalf("reply %+v, %v", got, err)
	}
	question := messages(t, s.requests()[0])[2]
	text := blocks(question)[0]["text"].(string)
	for _, want := range []string{`1. gh ["pr","comment","7","--body","on it"]`, "Exit 0:\ncomment posted", `2. gh ["pr","review","7","--approve"]`, "Outcome unknown"} {
		if !strings.Contains(text, want) {
			t.Errorf("the question does not hold %q:\n%s", want, text)
		}
	}
	if calls := gh.made(); len(calls) != 1 || calls[0].Seq != 3 {
		t.Errorf("the new calls %+v, want one numbered 3", calls)
	}
}

// A gh call that failed or never ran goes back to the model as an error result, with gh's exit
// status, and the append records it as the call's result. The bound leaves a call after the
// second, so the second call's message holding the result holds nothing else (the last call
// carries its own message after it: TestTheLastCallIsToldItIsTheLastAndTheAppendHoldsNoTraceOfIt).
func TestAFailedGhCallGoesBackAsAnErrorResult(t *testing.T) {
	s := newStandIn(t,
		anthropicStream("tool_use", anthropicUsage{Output: 1}, anthropicGH("toolu_1", "pr", "view", "7")),
		anthropicStream("end_turn", anthropicUsage{Output: 1}, anthropicText(`{"kind":"answered","text":"Done.","needs":""}`)),
	)
	req := anthropicRequest(t, s, failingGH{}, 3)
	got, err := Answer(context.Background(), req)
	if err != nil || got.Kind != Answered {
		t.Fatalf("reply %+v, %v", got, err)
	}
	result := got.Append.Turns[0].Results[0]
	if !result.IsError || !strings.Contains(result.Content[0].Text, "gh exited 1") {
		t.Errorf("the appended result %+v, want an error result naming the exit status", result)
	}
	sent := blocks(messages(t, s.requests()[1])[4])
	if len(sent) != 1 || sent[0]["is_error"] != true {
		t.Fatalf("the result the model was given %v, want one error result", sent)
	}
	if text, _ := json.Marshal(sent[0]["content"]); !strings.Contains(string(text), "gh exited 1") {
		t.Errorf("the result the model was given %s, want it naming the exit status", text)
	}
}

// failingGH is a gh tool whose call runs and fails.
type failingGH struct{}

func (failingGH) Run(context.Context, GHCall) ghtool.Result {
	return ghtool.Result{Output: "could not resolve to a PullRequest", Exit: 1, Ran: true}
}
