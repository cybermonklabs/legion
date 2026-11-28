package omp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/conversation"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/testbin"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
)

// The stand-in model the real-binary tests point the pinned Oh My Pi at: a models.yml provider on
// the Anthropic Messages API whose base URL is a local server that records each request and
// answers a canned stream.
const (
	standInProvider = "standin"
	standInModel    = "claude-test"
	standInGPT      = "gpt-test"
)

// standInEndpoint is how the stand-in's endpoint treats replayed thinking: a reasoning model on an
// Anthropic-compatible endpoint that is not Anthropic's own API, for which Oh My Pi's compat
// replays unsigned thinking.
var standInEndpoint = Options{ReplayUnsignedThinking: true}

// standIn is a local model endpoint: it records every request body and answers each with the next
// canned server-sent-event stream, or, once they run out, its fallback stream.
type standIn struct {
	server   *httptest.Server
	mu       sync.Mutex
	requests [][]byte
	replies  []string
}

func newStandIn(t *testing.T, fallback string, replies ...string) *standIn {
	t.Helper()
	s := &standIn{replies: replies}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.requests = append(s.requests, body)
		reply := fallback
		if len(s.replies) > 0 {
			reply, s.replies = s.replies[0], s.replies[1:]
		}
		s.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(s.server.Close)
	return s
}

// newAnthropicStandIn is a stand-in Anthropic Messages endpoint whose fallback is a short signed
// answer.
func newAnthropicStandIn(t *testing.T, replies ...string) *standIn {
	t.Helper()
	return newStandIn(t, anthropicStream("msg_default", "end_turn", sseThinking("Resumed.", "cmVzdW1lZA=="), sseText("Resumed.")), replies...)
}

// last is the body of the last request.
func (s *standIn) last(t *testing.T) []byte {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) == 0 {
		t.Fatal("the stand-in model received no request")
	}
	return s.requests[len(s.requests)-1]
}

// lastAnthropic is the last request, decoded as an Anthropic Messages request.
func (s *standIn) lastAnthropic(t *testing.T) anthropicRequest {
	t.Helper()
	body := s.last(t)
	var request anthropicRequest
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatalf("decode the request: %v\n%s", err, body)
	}
	return request
}

// sseBlock is one content block of a canned stream.
type sseBlock struct {
	start  string
	deltas []string
}

func sseThinking(thinking, signature string) sseBlock {
	return sseBlock{
		start: `{"type":"thinking","thinking":"","signature":""}`,
		deltas: []string{
			`{"type":"thinking_delta","thinking":` + quote(thinking) + `}`,
			`{"type":"signature_delta","signature":` + quote(signature) + `}`,
		},
	}
}

func sseRedacted(data string) sseBlock {
	return sseBlock{start: `{"type":"redacted_thinking","data":` + quote(data) + `}`}
}

func sseText(text string) sseBlock {
	return sseBlock{start: `{"type":"text","text":""}`, deltas: []string{`{"type":"text_delta","text":` + quote(text) + `}`}}
}

func sseToolUse(id, name, input string) sseBlock {
	return sseBlock{
		start:  `{"type":"tool_use","id":` + quote(id) + `,"name":` + quote(name) + `,"input":{}}`,
		deltas: []string{`{"type":"input_json_delta","partial_json":` + quote(input) + `}`},
	}
}

// anthropicStream is an Anthropic Messages streaming response of blocks.
func anthropicStream(id, stopReason string, blocks ...sseBlock) string {
	var b strings.Builder
	event := func(name, data string) { fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", name, data) }
	event("message_start", `{"type":"message_start","message":{"id":"`+id+`","type":"message","role":"assistant","model":"`+standInModel+
		`","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":1}}}`)
	for i, block := range blocks {
		event("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":%s}`, i, block.start))
		for _, delta := range block.deltas {
			event("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":%s}`, i, delta))
		}
		event("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, i))
	}
	event("message_delta", `{"type":"message_delta","delta":{"stop_reason":"`+stopReason+`","stop_sequence":null},"usage":{"output_tokens":20}}`)
	event("message_stop", `{"type":"message_stop"}`)
	return b.String()
}

// ompRig is a fresh HOME whose one model is the stand-in on api, a working directory holding
// note.txt, and the directory sessions go to.
type ompRig struct {
	omp, home, work, sessions string
	env                       []string
}

// standInModelOf is the stand-in's model id on api: a Claude id on Anthropic Messages, a GPT id on
// OpenAI Responses, so Oh My Pi's dialect for each is its family's.
func standInModelOf(api string) string {
	if api == conversation.OpenAIResponses {
		return standInGPT
	}
	return standInModel
}

func newOMPRig(t *testing.T, api string, model *standIn, extraEnv ...string) *ompRig {
	t.Helper()
	return newOMPRigForModel(t, api, standInModelOf(api), model, extraEnv...)
}

func newOMPRigForModel(t *testing.T, api, modelID string, model *standIn, extraEnv ...string) *ompRig {
	t.Helper()
	return newOMPRigForProviderModel(t, api, standInProvider, modelID, model, extraEnv...)
}

func newOMPRigForProviderModel(t *testing.T, api, provider, modelID string, model *standIn, extraEnv ...string) *ompRig {
	t.Helper()
	omp := testbin.OMP(t)
	dir := t.TempDir()
	rig := &ompRig{omp: omp, home: filepath.Join(dir, "home"), work: filepath.Join(dir, "work"), sessions: filepath.Join(dir, "sessions")}
	testbin.OMPHome(t, omp, rig.home)
	agent := filepath.Join(rig.home, ".omp", "agent")
	baseURL := model.server.URL
	if api == conversation.OpenAIResponses {
		baseURL += "/v1"
	}
	for path, content := range map[string]string{
		filepath.Join(agent, "models.yml"): "providers:\n  " + provider + ":\n    baseUrl: " + baseURL +
			"\n    auth: apiKey\n    api: " + api + "\n    apiKey: unused\n    models:\n      - id: " + modelID +
			"\n        name: Stand-in\n        reasoning: true\n",
		filepath.Join(agent, "config.yml"):  "modelRoles:\n  default: " + provider + "/" + modelID + "\n",
		filepath.Join(rig.work, "note.txt"): "plum\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rig.env = append([]string{"HOME=" + rig.home, "PATH=/usr/bin:/bin", "AWS_EC2_METADATA_DISABLED=true"}, extraEnv...)
	return rig
}

// run runs `omp -p` with prompt, resuming the session at resume when it is not empty.
func (r *ompRig) run(t *testing.T, resume, prompt string) {
	t.Helper()
	args := []string{"-p", "--session-dir", r.sessions, "--no-title", "--no-extensions", "--no-skills", "--no-rules", "--tools", "read"}
	if resume != "" {
		args = append(args, "--resume", resume)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.omp, append(args, prompt)...)
	cmd.Dir, cmd.Env = r.work, r.env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("omp %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// sessionFile is the one session file the rig's runs wrote.
func (r *ompRig) sessionFile(t *testing.T) string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(r.sessions, "*.jsonl"))
	if err != nil || len(files) != 1 {
		t.Fatalf("session files under %s = %v, %v; want one", r.sessions, files, err)
	}
	return files[0]
}

// sessionContent is the session file at path as it is now.
func sessionContent(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

// The first run's two model calls: a turn that thinks (signed and redacted), says it reads the
// note and calls read, then an answer.
func firstRunReplies() []string {
	return []string{
		anthropicStream("msg_1", "tool_use", sseThinking("I should read the note.", "c2lnLW9uZQ=="), sseRedacted("cmVkYWN0ZWQtb25l"),
			sseText("Reading the note."), sseToolUse("toolu_01ABC", "read", `{"path":"note.txt"}`)),
		anthropicStream("msg_2", "end_turn", sseThinking("The note says plum.", "c2lnLXR3bw=="), sseText("The note says plum.")),
	}
}

// anthropicRequest is the part of an Anthropic Messages request the tests compare.
type anthropicRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

// wireTurn is one Anthropic message reduced to what the comparison checks: its role and each
// block's type, text, signature or data, tool id, name and input, and result.
type wireTurn struct {
	Role   string      `json:"role"`
	Blocks []wireBlock `json:"blocks"`
}

type wireBlock struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	Signature string `json:"signature,omitempty"`
	Data      string `json:"data,omitempty"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Input     any    `json:"input,omitempty"`
	Result    string `json:"result,omitempty"`
	IsError   bool   `json:"isError,omitempty"`
}

// turns reduces the request's messages. date-cwd-reminder.ts says “Each placement is journaled
// in the session”. The comparison removes only the journaled text that the caller supplies anew.
func (request anthropicRequest) turns(t *testing.T, content []byte) []wireTurn {
	t.Helper()
	reminders := readReminderJournal(t, content)
	var turns []wireTurn
	for _, message := range request.Messages {
		turn := wireTurn{Role: message.Role}
		var blocks []struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			Thinking  string          `json:"thinking"`
			Signature string          `json:"signature"`
			Data      string          `json:"data"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Input     any             `json:"input"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content"`
			IsError   bool            `json:"is_error"`
		}
		var text string
		if json.Unmarshal(message.Content, &text) == nil {
			turn.Blocks = []wireBlock{{Type: "text", Text: text}}
		} else if err := json.Unmarshal(message.Content, &blocks); err != nil {
			t.Fatalf("decode a request message: %v\n%s", err, message.Content)
		}
		for _, b := range blocks {
			switch b.Type {
			case "text":
				turn.Blocks = append(turn.Blocks, wireBlock{Type: "text", Text: b.Text})
			case "thinking":
				turn.Blocks = append(turn.Blocks, wireBlock{Type: "thinking", Text: b.Thinking, Signature: b.Signature})
			case "redacted_thinking":
				turn.Blocks = append(turn.Blocks, wireBlock{Type: "redacted_thinking", Data: b.Data})
			case "tool_use":
				turn.Blocks = append(turn.Blocks, wireBlock{Type: "tool_use", ID: b.ID, Name: b.Name, Input: b.Input})
			case "tool_result":
				turn.Blocks = append(turn.Blocks, wireBlock{Type: "tool_result", ID: b.ToolUseID, Result: resultText(t, b.Content), IsError: b.IsError})
			default:
				t.Fatalf("a request block of type %q", b.Type)
			}
		}
		turns = append(turns, turn)
	}
	var kept []wireTurn
	for _, turn := range turns {
		// Anthropic sends the injector's developer control as a user turn.
		if turn.Role == "user" && len(turn.Blocks) > 0 && turn.Blocks[0].Type == "text" {
			first := &turn.Blocks[0]
			if len(turn.Blocks) == 1 && reminders.takeControl(first.Text) {
				continue
			}
			if rest, ok := reminders.cutPrefix(first.Text, "\n"); ok {
				if rest != "" {
					first.Text = rest
				} else {
					turn.Blocks = turn.Blocks[1:]
					if len(turn.Blocks) == 0 {
						continue
					}
				}
			}
		}
		kept = append(kept, turn)
	}
	return kept
}

type reminderJournal struct {
	prefixes map[string]int
	controls map[string]int
}

func readReminderJournal(t *testing.T, content []byte) reminderJournal {
	t.Helper()
	journal := reminderJournal{prefixes: map[string]int{}, controls: map[string]int{}}
	p, err := parseSession(content)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]*entry, len(p.entries))
	for _, e := range p.entries {
		byID[e.ID] = e
	}
	path := activePath(resolveLeaf(p.entries, byID), byID)
	for _, e := range path {
		if e.Type == "reset_boundary" {
			clear(journal.prefixes)
			clear(journal.controls)
		}
		if e.Type != "custom" || e.CustomType != "date-cwd-reminder" {
			continue
		}
		var placement struct {
			Conversation *string `json:"conversation"`
			Turn         *string `json:"turn"`
			Reminder     string  `json:"reminder"`
			Placement    string  `json:"placement"`
		}
		if err := json.Unmarshal(e.Data, &placement); err != nil || placement.Conversation == nil ||
			placement.Turn == nil || placement.Reminder == "" {
			t.Fatalf("invalid date-cwd-reminder placement: %s", e.Data)
		}
		switch placement.Placement {
		case "prefix":
			journal.prefixes[placement.Reminder]++
		case "control":
			journal.controls[placement.Reminder]++
		default:
			t.Fatalf("unknown date-cwd-reminder placement: %s", e.Data)
		}
	}
	return journal
}

func (j reminderJournal) takeControl(text string) bool {
	if j.controls[text] == 0 {
		return false
	}
	j.controls[text]--
	return true
}

func (j reminderJournal) cutPrefix(text, separator string) (string, bool) {
	for reminder, count := range j.prefixes {
		if count == 0 {
			continue
		}
		if text == reminder {
			j.prefixes[reminder]--
			return "", true
		}
		if rest, ok := strings.CutPrefix(text, reminder+separator); ok {
			j.prefixes[reminder]--
			return rest, true
		}
	}
	return text, false
}

func TestResumeComparisonRemovesOnlyJournaledReminders(t *testing.T) {
	f := newFixture()
	f.add(`{"type":"custom","customType":"date-cwd-reminder","data":{"conversation":"user:1:user","turn":"user:1:user","reminder":"old date","placement":"prefix"}}`)
	f.add(`{"type":"custom","customType":"date-cwd-reminder","data":{"conversation":"user:1:user","turn":"assistant:2","reminder":"control date","placement":"control"}}`)
	f.add(`{"type":"custom","customType":"date-cwd-reminder","data":{"conversation":"user:1:user","turn":"user:3:user","reminder":"new date","placement":"prefix"}}`)
	var request anthropicRequest
	if err := json.Unmarshal([]byte(`{"messages":[
		{"role":"user","content":"old date\nfirst"},
		{"role":"assistant","content":"answer"},
		{"role":"user","content":"control date"},
		{"role":"user","content":[{"type":"text","text":"new date"},{"type":"text","text":"next"},{"type":"text","text":"old date"}]},
		{"role":"user","content":"<system-reminder>\nunrelated\n</system-reminder>"}]}`), &request); err != nil {
		t.Fatal(err)
	}
	got := request.turns(t, f.bytes())
	want := []wireTurn{
		{Role: "user", Blocks: []wireBlock{{Type: "text", Text: "first"}}},
		{Role: "assistant", Blocks: []wireBlock{{Type: "text", Text: "answer"}}},
		{Role: "user", Blocks: []wireBlock{{Type: "text", Text: "next"}, {Type: "text", Text: "old date"}}},
		{Role: "user", Blocks: []wireBlock{{Type: "text", Text: "<system-reminder>\nunrelated\n</system-reminder>"}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("turns = %+v, want %+v", got, want)
	}
}

func resultText(t *testing.T, content json.RawMessage) string {
	t.Helper()
	var text string
	if json.Unmarshal(content, &text) == nil {
		return text
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &parts); err != nil {
		t.Fatalf("decode a tool result's content: %v", err)
	}
	var texts []string
	for _, part := range parts {
		texts = append(texts, part.Text)
	}
	return strings.Join(texts, "\n")
}

// readerTurns reduces the reader's messages the same way, grouped as an Anthropic request groups
// them: each run of tool results one user turn.
func readerTurns(t *testing.T, messages []conversation.Message) []wireTurn {
	t.Helper()
	var turns []wireTurn
	for _, m := range messages {
		var blocks []wireBlock
		for _, b := range m.Content {
			switch b.Type {
			case conversation.Text:
				blocks = append(blocks, wireBlock{Type: "text", Text: b.Text})
			case conversation.Thinking:
				blocks = append(blocks, wireBlock{Type: "thinking", Text: b.Text, Signature: b.Signature})
			case conversation.RedactedThinking:
				blocks = append(blocks, wireBlock{Type: "redacted_thinking", Data: b.Data})
			case conversation.ToolCall:
				var input any
				if err := json.Unmarshal(b.Arguments, &input); err != nil {
					t.Fatal(err)
				}
				blocks = append(blocks, wireBlock{Type: "tool_use", ID: b.ToolCallID, Name: b.ToolName, Input: input})
			case conversation.ToolResult:
				var texts []string
				for _, part := range b.Content {
					texts = append(texts, part.Text)
				}
				blocks = append(blocks, wireBlock{Type: "tool_result", ID: b.ToolCallID, Result: strings.Join(texts, "\n"), IsError: b.IsError})
			default:
				t.Fatalf("a reader block of type %q", b.Type)
			}
		}
		if m.Role == conversation.Tool && len(turns) > 0 && turns[len(turns)-1].Role == "user" &&
			turns[len(turns)-1].Blocks[0].Type == "tool_result" {
			turns[len(turns)-1].Blocks = append(turns[len(turns)-1].Blocks, blocks...)
			continue
		}
		role := string(m.Role)
		if m.Role == conversation.Tool {
			role = "user"
		}
		turns = append(turns, wireTurn{Role: role, Blocks: blocks})
	}
	return turns
}

// resumedSession is the session as the pinned Oh My Pi's resume sent it: the reader's read of
// content, the session after the resume, cut just before the resume's follow-up, the first user
// message entry the session before the resume did not hold. The follow-up is found by its id, not
// past the size read before the resume, since Oh My Pi writes a session with a torn record out
// again before it appends to it (session-manager.ts). The test fails, naming the cause, when
// content holds no such entry or that entry's text is not followUp.
func resumedSession(t *testing.T, before *Session, content []byte, opts Options, followUp string) *Session {
	t.Helper()
	start := 0
	for raw := range bytes.Lines(content) {
		at := start
		start += len(raw)
		line := bytes.TrimSuffix(raw, []byte("\n"))
		var e entry
		if json.Unmarshal(line, &e) != nil || e.Type != "message" || e.Message == nil || e.Message.Role != "user" || before.ids[e.ID] {
			continue
		}
		blocks, err := parseBlocks(e.Message.Content, inputBlocks, "the resume's follow-up")
		if err != nil {
			t.Fatalf("the first user message the session before the resume did not hold: %v\n%s", err, line)
		}
		if len(blocks) != 1 || blocks[0].Type != "text" || blocks[0].Text != followUp {
			t.Fatalf("the first user message the session before the resume did not hold is not the follow-up %q:\n%s", followUp, line)
		}
		resumed, err := Read(content[:at], opts)
		if err != nil {
			t.Fatalf("the reader's read of the session as the resume left it before its request: %v", err)
		}
		return resumed
	}
	t.Fatalf("the session after the resume holds no user message the session before it did not; want the follow-up %q", followUp)
	return nil
}

// assertBlankedOnly checks the conversation the reader rebuilt from the session before the resume
// against resumed, the session as the resume sent it: the same messages, one for one, but for
// tool results of the same call whose content differs, which a prune record the resume wrote
// blanked. Any other difference fails the test, naming the message. The calls so blanked are
// logged, not checked: which they are depends on the pinned Oh My Pi, the provider's cache and
// the time.
func assertBlankedOnly(t *testing.T, before, resumed *Session) {
	t.Helper()
	if len(before.Messages) != len(resumed.Messages) {
		t.Errorf("the reader rebuilt %d messages from the session before the resume and %d from the session as the resume sent it; want as many",
			len(before.Messages), len(resumed.Messages))
		return
	}
	var blanked []string
	for i, was := range before.Messages {
		is := resumed.Messages[i]
		if reflect.DeepEqual(was, is) {
			continue
		}
		if call, ok := blankedResult(was, is); ok {
			blanked = append(blanked, call)
			continue
		}
		wasJSON, _ := json.Marshal(was)
		isJSON, _ := json.Marshal(is)
		t.Errorf("message %d the reader rebuilt from the session before the resume:\n%s\nfrom the session as the resume sent it:\n%s\nwant the same, or the same call's tool result blanked",
			i, wasJSON, isJSON)
	}
	if len(blanked) > 0 {
		t.Logf("the resume blanked the results of %s", strings.Join(blanked, ", "))
	}
}

// blankedResult is the call of the one tool result that was and is each hold, and whether the
// two messages differ in that result's content alone.
func blankedResult(was, is conversation.Message) (string, bool) {
	if was.Role != conversation.Tool || len(was.Content) != 1 || len(is.Content) != 1 {
		return "", false
	}
	wasResult, isResult := was.Content[0], is.Content[0]
	wasResult.Content, isResult.Content = nil, nil
	was.Content, is.Content = nil, nil
	return wasResult.ToolCallID, wasResult.Type == conversation.ToolResult && reflect.DeepEqual(wasResult, isResult) && reflect.DeepEqual(was, is)
}

// assertResumeMatches checks the request the pinned Oh My Pi sent on resuming a session against
// the reader's rebuild of the session as the resume left it before its request: the request is
// that conversation, then the follow-up. content is the session after the resume, and
// resumedSession cuts that session from it. It is the session from before the resume plus any
// record Oh My Pi writes on resume. Before sending a prompt, agent-session.ts calls the full
// checkCompaction if the last assistant stopped with error or length and the check is not skipped.
// Its #pruneStaleToolResults pass calls pruneSupersededToolResults (pruning.ts) and writes
// tool_result_prune through #commitPrune (session-maintenance.ts). A superseded read qualifies
// only if its suffix fits the token and cache-lookback limits, or the last message is at least
// PRUNE_IDLE_FLUSH_MS (90 minutes) old. assertBlankedOnly checks for changes of that kind alone.
func assertResumeMatches(t *testing.T, before *Session, content []byte, request anthropicRequest, followUp string) []wireTurn {
	t.Helper()
	resumed := resumedSession(t, before, content, standInEndpoint, followUp)
	got := request.turns(t, content)
	want := append(readerTurns(t, resumed.Messages), wireTurn{Role: "user", Blocks: []wireBlock{{Type: "text", Text: followUp}}})
	observed, _ := json.Marshal(got)
	t.Logf("Oh My Pi's resume request, reduced: %s", observed)
	if !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.MarshalIndent(got, "", "  ")
		wantJSON, _ := json.MarshalIndent(want, "", "  ")
		t.Errorf("Oh My Pi's resume request:\n%s\nthe reader's conversation and the follow-up:\n%s", gotJSON, wantJSON)
	}
	if resumed.Model != standInProvider+"/"+standInModel || resumed.API != conversation.AnthropicMessages {
		t.Errorf("the reader's model %q, API %q", resumed.Model, resumed.API)
	}
	assertBlankedOnly(t, before, resumed)
	return got
}

// The pinned Oh My Pi, resuming a session it wrote with a tool call and signed and redacted
// thinking in it, sends the stand-in model the conversation the reader rebuilds from the session
// as the resume left it before its request: the same turns, roles, block order, text, tool ids,
// thinking signatures and redacted data.
func TestThePinnedOhMyPiResumesTheConversationTheReaderRebuilds(t *testing.T) {
	model := newAnthropicStandIn(t, firstRunReplies()...)
	rig := newOMPRig(t, conversation.AnthropicMessages, model)
	rig.run(t, "", "Read note.txt and tell me what it says.")
	file := rig.sessionFile(t)
	before, err := ReadFile(file, standInEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Messages) != 4 {
		t.Fatalf("the reader rebuilt %d messages from the first run, want the request, the tool turn, its result and the answer", len(before.Messages))
	}

	rig.run(t, file, "What colour is the fruit?")
	assertResumeMatches(t, before, sessionContent(t, file), model.lastAnthropic(t), "What colour is the fruit?")
}

func TestThePinnedOhMyPiDropsThinkingOnClaudeResume(t *testing.T) {
	const visible = "The note says plum."
	const thinking = "I read the note."
	const followUp = "What colour is the fruit?"
	for _, tc := range []struct {
		name, modelID string
		compaction    bool
	}{
		{name: "another Claude model", modelID: standInModel},
		{name: "compaction rewrites the prefix", modelID: "claude-opus-5-5", compaction: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := newAnthropicStandIn(t, anthropicStream("msg_1", "end_turn", sseThinking(thinking, "c2ln"), sseRedacted("cmVk"), sseText(visible)))
			rig := newOMPRigForModel(t, conversation.AnthropicMessages, tc.modelID, model)
			rig.run(t, "", "What does the note say?")
			file := rig.sessionFile(t)
			first, err := ReadFile(file, standInEndpoint)
			if err != nil {
				t.Fatal(err)
			}
			carriedOnce(t, readerTurns(t, first.Messages), func(_ wireTurn, b wireBlock) bool {
				return b.Type == "thinking" && b.Text == thinking && b.Signature == "c2ln"
			}, "the initial signed thinking")
			now := time.Now()
			if tc.compaction {
				parsed, err := parseSession(sessionContent(t, file))
				if err != nil {
					t.Fatal(err)
				}
				kept := ""
				for _, e := range parsed.entries {
					if e.Type == "message" && e.Message.Role == "assistant" {
						kept = e.ID
					}
				}
				if kept == "" {
					t.Fatal("the first session has no assistant entry to retain")
				}
				appendRaw(t, file, fmt.Sprintf(`{"type":"compaction","id":"d4000001","parentId":%q,"timestamp":%q,"summary":"The user asked about the note.","firstKeptEntryId":%q,"tokensBefore":1000}`+"\n",
					first.Leaf, conversation.ISOTime(now), kept))
			} else {
				appendRaw(t, file, fmt.Sprintf(`{"type":"message","id":"d4000001","parentId":%q,"timestamp":%q,"message":{"role":"assistant","content":[{"type":"thinking","thinking":"Foreign reasoning.","thinkingSignature":"Zm9yZWlnbg=="},{"type":"redactedThinking","data":"Zm9yZWlnbg=="},{"type":"text","text":"Another model answered."}],"api":%q,"provider":%q,"model":"claude-sonnet-5","usage":%s,"stopReason":"stop","timestamp":%d}}`+"\n"+
					`{"type":"model_change","id":"d4000002","parentId":"d4000001","timestamp":%q,"model":%q}`+"\n",
					first.Leaf, conversation.ISOTime(now), conversation.AnthropicMessages, standInProvider, handWrittenUsage, now.UnixMilli(),
					conversation.ISOTime(now), standInProvider+"/"+tc.modelID))
			}
			before, err := ReadFile(file, standInEndpoint)
			if err != nil {
				t.Fatal(err)
			}
			rig.run(t, file, followUp)
			content := sessionContent(t, file)
			request := model.lastAnthropic(t)
			if request.Model != tc.modelID {
				t.Fatalf("request model = %q, want %q", request.Model, tc.modelID)
			}
			got := request.turns(t, content)
			kept, droppedText, droppedData := visible, thinking, "cmVk"
			if !tc.compaction {
				kept, droppedText, droppedData = "Another model answered.", "Foreign reasoning.", "Zm9yZWlnbg=="
				carriedOnce(t, got, func(_ wireTurn, b wireBlock) bool {
					return b.Type == "thinking" && b.Text == thinking && b.Signature == "c2ln"
				}, "the resumed model's own signed thinking")
			}
			carriedOnce(t, got, func(turn wireTurn, b wireBlock) bool {
				return turn.Role == "assistant" && b.Type == "text" && b.Text == kept
			}, "the answer whose thinking is dropped")
			for _, turn := range got {
				for _, b := range turn.Blocks {
					if strings.Contains(b.Text, droppedText) || b.Data == droppedData || b.Signature == droppedData {
						t.Fatalf("pinned Oh My Pi kept thinking that transform-messages.ts drops: %+v\n%s", b, model.last(t))
					}
				}
			}
			resumed := resumedSession(t, before, content, standInEndpoint, followUp)
			want := append(readerTurns(t, resumed.Messages), wireTurn{Role: "user", Blocks: []wireBlock{{Type: "text", Text: followUp}}})
			if !reflect.DeepEqual(got, want) {
				t.Errorf("Oh My Pi's resume request:\n%+v\nthe reader's conversation and the follow-up:\n%+v", got, want)
			}
		})
	}
}

// The pinned Oh My Pi and the reader rebuild alike a session that, after a real first run, holds
// records written by hand: a compaction (its summary in Oh My Pi's template, the entries it keeps
// after it), a tool call interrupted before its result, failed turns and their recovery records,
// late model usage, a torn record, a branch summary, and an extension's custom message.
// Each resumed request is the reader's conversation, then the follow-up.
func TestThePinnedOhMyPiResumesHandWrittenRecordsAsTheReaderRebuildsThem(t *testing.T) {
	stamp := func() string { return conversation.ISOTime(time.Now()) }
	for _, testCase := range []struct {
		name string
		// records is what is written after the first run: entries is the first run's entry ids
		// by kind, leaf its resolved active entry.
		records func(entries map[string]string, leaf string) string
	}{
		{"a compaction", func(entries map[string]string, leaf string) string {
			return `{"type":"compaction","id":"c0c0c0c0","parentId":"` + leaf + `","timestamp":"` + stamp() +
				`","summary":"The user asked what note.txt says.\n\n\n\n| It | said |\n|--|--|\n| plum |  |  ","firstKeptEntryId":"` + entries["toolResult"] + `","tokensBefore":1000}` + "\n"
		}},
		{"an interrupted tool call", func(_ map[string]string, leaf string) string {
			return `{"type":"message","id":"1a1a1a1a","parentId":"` + leaf + `","timestamp":"` + stamp() + `","message":{"role":"assistant","content":[` +
				`{"type":"thinking","thinking":"Read it again.","thinkingSignature":"aW50ZXJydXB0ZWQ="},{"type":"redactedThinking","data":"cmVk"},` +
				`{"type":"text","text":"Reading again."},{"type":"toolCall","id":"toolu_interrupted","name":"read","arguments":{"path":"note.txt"}}],` +
				`"api":"anthropic-messages","provider":"` + standInProvider + `","model":"` + standInModel + `","usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":2,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"stopReason":"toolUse","timestamp":` +
				fmt.Sprint(time.Now().UnixMilli()) + `}}` + "\n"
		}},
		{"an OpenAI tool call between text blocks", func(entries map[string]string, leaf string) string {
			now := time.Now()
			stamp := conversation.ISOTime(now)
			return fmt.Sprintf(`{"type":"message","id":"d5000001","parentId":%q,"timestamp":%q,"runtime":%s,"message":{"role":"assistant","api":"openai-responses","provider":"openai","model":"gpt-test","stopReason":"toolUse","usage":%s,"timestamp":%d,"content":[{"type":"text","text":"Before."},{"type":"toolCall","id":"call_read|fc_read","name":"read","arguments":{"path":"note.txt"}},{"type":"text","text":"After."}]}}`+"\n"+
				`{"type":"message","id":"d5000002","parentId":"d5000001","timestamp":%q,"runtime":%s,"message":{"role":"toolResult","toolCallId":"call_read|fc_read","toolName":"read","content":[{"type":"text","text":"plum"}],"timestamp":%d}}`+"\n",
				leaf, stamp, entries["runtime"], handWrittenUsage, now.UnixMilli(), stamp, entries["runtime"], now.UnixMilli()) +
				fmt.Sprintf(`{"type":"model_change","id":"d5000003","parentId":"d5000002","timestamp":%q,"model":%q}`+"\n",
					stamp, standInProvider+"/"+standInModel)
		}},
		{"an aborted turn and its result", func(_ map[string]string, leaf string) string {
			ms := fmt.Sprint(time.Now().UnixMilli())
			return `{"type":"message","id":"2b2b2b2b","parentId":"` + leaf + `","timestamp":"` + stamp() + `","message":{"role":"assistant","content":[` +
				`{"type":"text","text":"Reading once more."},{"type":"toolCall","id":"toolu_aborted","name":"read","arguments":{"path":"note.txt"}}],` +
				`"api":"anthropic-messages","provider":"` + standInProvider + `","model":"` + standInModel + `","usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":2,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"stopReason":"aborted","timestamp":` + ms + `}}` + "\n" +
				`{"type":"message","id":"3c3c3c3c","parentId":"2b2b2b2b","timestamp":"` + stamp() + `","message":{"role":"toolResult","toolCallId":"toolu_aborted","toolName":"read","content":[{"type":"text","text":"aborted"}],"isError":true,"timestamp":` + ms + `}}` + "\n"
		}},
		{"an errored turn a retry recovered", func(_ map[string]string, leaf string) string {
			ms := fmt.Sprint(time.Now().UnixMilli())
			usage := `"usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":2,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}}`
			return `{"type":"message","id":"7a7a7a7a","parentId":"` + leaf + `","timestamp":"` + stamp() + `","message":{"role":"assistant","content":[{"type":"text","text":"Reading it ag"}],` +
				`"api":"anthropic-messages","provider":"` + standInProvider + `","model":"` + standInModel + `",` + usage + `,"stopReason":"error","errorMessage":"overloaded",` +
				`"retryRecovery":{"kind":"auto-retry","status":"recovered","attempt":1,"recoveredAt":"` + stamp() + `","recovery":"plain","note":"error; retried",` +
				`"supersededBy":{"timestamp":` + ms + `,"responseId":"resp_x","provider":"` + standInProvider + `","model":"` + standInModel + `"}},"timestamp":` + ms + `}}` + "\n" +
				`{"type":"message","id":"8b8b8b8b","parentId":"7a7a7a7a","timestamp":"` + stamp() + `","message":{"role":"assistant","content":[{"type":"text","text":"It still says plum."}],` +
				`"api":"anthropic-messages","provider":"` + standInProvider + `","model":"` + standInModel + `",` + usage + `,"stopReason":"stop","timestamp":` + ms + `}}` + "\n"
		}},
		{"late model usage", func(entries map[string]string, leaf string) string {
			return fmt.Sprintf(`{"type":"model_usage","id":"d1000001","parentId":%q,"timestamp":%q,"runtime":%s,"purpose":"title","api":%q,"provider":%q,"model":%q,"usage":%s,"stopReason":"stop","activeLeafId":%q}`+"\n",
				entries["assistant"], stamp(), entries["runtime"], conversation.AnthropicMessages, standInProvider, standInModel, handWrittenUsage, leaf)
		}},
		{"a continued retry with its reminder", func(entries map[string]string, leaf string) string {
			return retryWithReminderRecords(entries["runtime"], leaf, true)
		}},
		{"a reminder after a turn left out", func(entries map[string]string, leaf string) string {
			return retryWithReminderRecords(entries["runtime"], leaf, false)
		}},
		{"a context notebook revision", func(entries map[string]string, leaf string) string {
			// tools/context-notes.ts:143–145 and session-manager.ts:7665–7668.
			return fmt.Sprintf(`{"type":"custom","customType":"experimental_context_notes","data":{"version":1,"text":"The note says plum."},"id":"d3000001","parentId":%q,"timestamp":%q,"runtime":%s}`+"\n",
				leaf, stamp(), entries["runtime"])
		}},
		{"an experimental context rollover", func(entries map[string]string, leaf string) string {
			// session-maintenance.ts:2088–2103,2750–2766 and session-manager.ts:7635–7648.
			summary := "<context-window-rollover>\n" +
				"The earlier raw transcript remains canonical and recoverable at history://current/full. Continue from the persistent notebook, the retained recent tail, and the latest user request or parent assignment, which is retained separately when it falls outside the tail. Earlier task instructions and constraints may still require recovery from raw history. Do not assume this boundary is an LLM summary; use the notebook and raw-history URI when prior details are needed.\n" +
				"</context-window-rollover>\n"
			return fmt.Sprintf(`{"type":"compaction","id":"d3000002","parentId":%q,"timestamp":%q,"runtime":%s,"summary":%s,"firstKeptEntryId":%q,"tokensBefore":1000,"tokensAfter":400,"details":{"kind":"experimental-context-rollover","version":1},"fromExtension":false}`+"\n",
				leaf, stamp(), entries["runtime"], quote(summary), entries["lastAssistant"])
		}},
		{"an interrupted thinking continuity note", func(entries map[string]string, leaf string) string {
			// agent-session.ts:5504–5521,5548–5570 and session-manager.ts:7730–7757.
			now := time.Now()
			at := now.Add(time.Millisecond)
			const reasoning = "I should verify that the note still contains the same word before answering."
			aborted := fmt.Sprintf(`{"type":"message","id":"d3000003","parentId":%q,"timestamp":%q,"runtime":%s,"message":{"role":"assistant","content":[{"type":"text","text":"The note says"},{"type":"thinking","thinking":%q}],"api":%q,"provider":%q,"model":%q,"usage":%s,"stopReason":"aborted","errorMessage":"Interrupted by user","timestamp":%d}}`+"\n",
				leaf, conversation.ISOTime(now), entries["runtime"], reasoning, conversation.AnthropicMessages, standInProvider, standInModel, handWrittenUsage, now.UnixMilli())
			note := "You were saying this but I interrupted you:\n```\n" + reasoning + "\n```"
			return aborted + fmt.Sprintf(`{"type":"custom_message","customType":"interrupted-thinking","content":%s,"display":false,"details":{"interruptedAt":%d,"provider":%q,"model":%q,"blockCount":1},"attribution":"agent","id":"d3000004","parentId":"d3000003","timestamp":%q,"runtime":%s}`+"\n",
				quote(note), at.UnixMilli(), standInProvider, standInModel, conversation.ISOTime(at), entries["runtime"])
		}},
		{"a continued user interrupt with no continuity note", func(entries map[string]string, leaf string) string {
			// agent-session.ts:5548–5555,14193–14196 and turn-recovery.ts:1636–1683.
			now := time.Now()
			aborted := fmt.Sprintf(`{"type":"message","id":"d3000006","parentId":%q,"timestamp":%q,"runtime":%s,"message":{"role":"assistant","content":[{"type":"text","text":"The note says"},{"type":"thinking","thinking":"check"}],"api":%q,"provider":%q,"model":%q,"usage":%s,"stopReason":"aborted","errorMessage":"Interrupted by user","errorId":67108864,"timestamp":%d}}`+"\n",
				leaf, conversation.ISOTime(now), entries["runtime"], conversation.AnthropicMessages, standInProvider, standInModel, handWrittenUsage, now.UnixMilli())
			doneAt := now.Add(time.Millisecond)
			answer := fmt.Sprintf(`{"type":"message","id":"d3000007","parentId":"d3000006","timestamp":%q,"runtime":%s,"message":{"role":"assistant","content":[{"type":"text","text":"plum."}],"api":%q,"provider":%q,"model":%q,"usage":%s,"stopReason":"stop","timestamp":%d}}`+"\n",
				conversation.ISOTime(doneAt), entries["runtime"], conversation.AnthropicMessages, standInProvider, standInModel, handWrittenUsage, doneAt.UnixMilli())
			patch := fmt.Sprintf(`{"type":"custom","customType":"entry_patch","data":{"patches":[{"entryId":"d3000006","message":{"continued":true}}]},"id":"d3000008","parentId":"d3000007","timestamp":%q,"runtime":%s}`+"\n",
				conversation.ISOTime(doneAt), entries["runtime"])
			return aborted + answer + patch
		}},
		{"a terminal error with only a fallback boundary", func(entries map[string]string, leaf string) string {
			// anthropic.ts:1416–1421,2971–2996 and turn-recovery.ts:1505–1509.
			now := time.Now()
			return fmt.Sprintf(`{"type":"message","id":"d3000005","parentId":%q,"timestamp":%q,"runtime":%s,"message":{"role":"assistant","content":[{"type":"fallback","from":{"model":%q},"to":{"model":"claude-test-fallback"}}],"api":%q,"provider":%q,"model":%q,"usage":%s,"stopReason":"error","errorMessage":"stream failed after fallback","timestamp":%d}}`+"\n",
				leaf, conversation.ISOTime(now), entries["runtime"], standInModel, conversation.AnthropicMessages, standInProvider, standInModel, handWrittenUsage, now.UnixMilli())
		}},
		{"a torn last record", func(_ map[string]string, leaf string) string {
			return `{"type":"message","id":"4d4d4d4d","parentId":"` + leaf + `","timestamp":"` + stamp() + `","message":{"role":"user","content":[{"type":"text","text":"cut o`
		}},
		{"a branch summary", func(entries map[string]string, _ string) string {
			return `{"type":"branch_summary","id":"5e5e5e5e","parentId":"` + entries["user"] + `","timestamp":"` + stamp() + `","fromId":"` + entries["assistant"] + `","summary":"Looked at note.txt  \n\n\nand stopped."}` + "\n"
		}},
		{"a custom message", func(_ map[string]string, leaf string) string {
			return `{"type":"custom_message","customType":"envoy-message","content":"A message from a peer arrived.","display":true,"id":"6f6f6f6f","parentId":"` + leaf + `","timestamp":"` + stamp() + `"}` + "\n"
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			model := newAnthropicStandIn(t, firstRunReplies()...)
			rig := newOMPRig(t, conversation.AnthropicMessages, model)
			rig.run(t, "", "Read note.txt and tell me what it says.")
			file := rig.sessionFile(t)
			content := sessionContent(t, file)
			first, err := Read(content, standInEndpoint)
			if err != nil {
				t.Fatal(err)
			}
			entries := map[string]string{}
			for _, line := range bytes.Split(content, []byte("\n")) {
				var e struct {
					Type    string          `json:"type"`
					ID      string          `json:"id"`
					Runtime json.RawMessage `json:"runtime"`
					Message struct {
						Role string `json:"role"`
					} `json:"message"`
				}
				if json.Unmarshal(line, &e) == nil && e.Type == "message" {
					if entries[e.Message.Role] == "" {
						entries[e.Message.Role] = e.ID
						entries["runtime"] = string(e.Runtime)
					}
					if e.Message.Role == "assistant" {
						entries["lastAssistant"] = e.ID
					}
				}
			}
			records := testCase.records(entries, first.Leaf)
			appendRaw(t, file, records)
			before, err := ReadFile(file, standInEndpoint)
			if err != nil {
				t.Fatal(err)
			}
			rig.run(t, file, "What colour is the fruit?")
			got := assertResumeMatches(t, before, sessionContent(t, file), model.lastAnthropic(t), "What colour is the fruit?")
			if testCase.name == "an OpenAI tool call between text blocks" {
				index := carriedOnce(t, got, func(turn wireTurn, b wireBlock) bool {
					return turn.Role == "assistant" && b.Type == "text" && b.Text == "Before."
				}, "the OpenAI assistant's opening text")
				want := []wireBlock{
					{Type: "text", Text: "Before."}, {Type: "text", Text: "After."},
					{Type: "tool_use", ID: "call_read_fc_read", Name: "read", Input: map[string]any{"path": "note.txt"}},
				}
				if index >= 0 && !reflect.DeepEqual(got[index].Blocks, want) {
					t.Errorf("reordered assistant blocks = %+v, want %+v", got[index].Blocks, want)
				}
			}
		})
	}
}

const handWrittenUsage = `{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":2,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}}`

func retryWithReminderRecords(runtime, leaf string, continued bool) string {
	now := time.Now()
	stamp := conversation.ISOTime(now)
	reminder := "<system-injection>\n" +
		"The provider limited the rate of requests: your last turn was cut off after the text above, possibly mid-sentence. The user already saw that text. The wait for the limit is over.\n" +
		"Continue exactly where it stopped. Do not repeat, restate, or summarize what you already wrote.\n" +
		"Attempt #1/3\n</system-injection>"
	failed := fmt.Sprintf(`{"type":"message","id":"d2000001","parentId":%q,"timestamp":%q,"runtime":%s,"message":{"role":"assistant","content":[{"type":"text","text":"The fruit is"}],"api":%q,"provider":%q,"model":%q,"usage":%s,"stopReason":"error","errorMessage":"rate limited","errorStatus":429,"timestamp":%d}}`+"\n",
		leaf, stamp, runtime, conversation.AnthropicMessages, standInProvider, standInModel, handWrittenUsage, now.UnixMilli())
	note := fmt.Sprintf(`{"type":"custom_message","customType":"continue-reminder","content":%s,"display":false,"attribution":"agent","id":"d2000002","parentId":"d2000001","timestamp":%q,"runtime":%s}`+"\n",
		quote(reminder), stamp, runtime)
	if !continued {
		return failed + note
	}
	doneAt := now.Add(time.Millisecond)
	answer := fmt.Sprintf(`{"type":"message","id":"d2000003","parentId":"d2000002","timestamp":%q,"runtime":%s,"message":{"role":"assistant","content":[{"type":"text","text":"plum."}],"api":%q,"provider":%q,"model":%q,"usage":%s,"stopReason":"stop","timestamp":%d}}`+"\n",
		conversation.ISOTime(doneAt), runtime, conversation.AnthropicMessages, standInProvider, standInModel, handWrittenUsage, doneAt.UnixMilli())
	patch := fmt.Sprintf(`{"type":"custom","customType":"entry_patch","data":{"patches":[{"entryId":"d2000001","message":{"retryRecovery":{"kind":"auto-retry","status":"recovered","attempt":1,"recoveredAt":%q,"recovery":"plain","note":"error; retried","supersededBy":{"timestamp":%d,"provider":%q,"model":%q}},"continued":true}}]},"id":"d2000004","parentId":"d2000003","timestamp":%q,"runtime":%s}`+"\n",
		conversation.ISOTime(doneAt), doneAt.UnixMilli(), standInProvider, standInModel, conversation.ISOTime(doneAt), runtime)
	return failed + note + answer + patch
}

func appendRaw(t *testing.T, path, records string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(records); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

// carriedOnce is the index in turns of the last turn holding a block match accepts, -1 when no
// block is accepted, and marks the test failed, naming the block as name, unless exactly one
// block is: each part of an appended answer reaches the model once.
func carriedOnce(t *testing.T, turns []wireTurn, match func(wireTurn, wireBlock) bool, name string) int {
	t.Helper()
	index, count := -1, 0
	for i, turn := range turns {
		for _, b := range turn.Blocks {
			if match(turn, b) {
				index = i
				count++
			}
		}
	}
	if count != 1 {
		t.Errorf("the request carries %s %d times, want once", name, count)
	}
	return index
}

// The answer an append writes reaches the model when the pinned Oh My Pi resumes the session,
// under file storage and under SQL storage: its next request carries the question, the tool call,
// its result and the answer once each, in that order, with the appended thinking signed as it was
// written, and is the conversation the reader rebuilds from the appended session as the resume
// left it before its request.
func TestThePinnedOhMyPiResumesAnAppendedAnswer(t *testing.T) {
	answer := conversation.Append{
		QuestionID: "q-1", Question: "Which fruit does the note name?",
		Provider: standInProvider, API: conversation.AnthropicMessages, Model: standInModel,
		Turns: []conversation.Turn{
			{
				Content: []conversation.Block{
					thinking("I will read the note again.", "YXBwZW5kLW9uZQ=="),
					text("Reading the note again."),
					call("toolu_append_01", "read", `{"path":"note.txt"}`),
				},
				Results: []conversation.Block{{Type: conversation.ToolResult, ToolCallID: "toolu_append_01", ToolName: "read", Content: []conversation.Block{text("plum")}}},
				Usage:   conversation.Usage{InputTokens: 12, OutputTokens: 8},
			},
			{
				Content: []conversation.Block{thinking("It names plum.", "YXBwZW5kLXR3bw=="), text("The note names plum.")},
				Usage:   conversation.Usage{InputTokens: 20, OutputTokens: 5},
			},
		},
	}
	assertAppended := func(t *testing.T, model *standIn, appended *Session, content []byte) {
		t.Helper()
		turns := assertResumeMatches(t, appended, content, model.lastAnthropic(t), "Thanks.")
		question := carriedOnce(t, turns, func(turn wireTurn, b wireBlock) bool {
			return turn.Role == "user" && b.Type == "text" && b.Text == answer.Question
		}, "the question")
		toolCall := carriedOnce(t, turns, func(turn wireTurn, b wireBlock) bool {
			return turn.Role == "assistant" && b.Type == "tool_use" && b.ID == "toolu_append_01"
		}, "the tool call")
		toolResult := carriedOnce(t, turns, func(turn wireTurn, b wireBlock) bool {
			return turn.Role == "user" && b.Type == "tool_result" && b.ID == "toolu_append_01" && b.Result == "plum"
		}, "the tool result")
		finalAnswer := carriedOnce(t, turns, func(turn wireTurn, b wireBlock) bool {
			return turn.Role == "assistant" && b.Type == "text" && b.Text == "The note names plum."
		}, "the answer")
		carriedOnce(t, turns, func(turn wireTurn, b wireBlock) bool {
			return b.Type == "thinking" && b.Signature == "YXBwZW5kLXR3bw==" && b.Text == "It names plum."
		}, "the answer's signed thinking")
		if !(question < toolCall && toolCall < toolResult && toolResult < finalAnswer && finalAnswer == len(turns)-2) {
			t.Errorf("the question at %d, the call at %d, its result at %d, the answer at %d of %d turns; want them in order, then the follow-up",
				question, toolCall, toolResult, finalAnswer, len(turns))
		}
	}

	t.Run("file storage", func(t *testing.T) {
		model := newAnthropicStandIn(t, firstRunReplies()...)
		rig := newOMPRig(t, conversation.AnthropicMessages, model)
		rig.run(t, "", "Read note.txt and tell me what it says.")
		file := rig.sessionFile(t)
		before, err := ReadFile(file, standInEndpoint)
		if err != nil {
			t.Fatal(err)
		}
		if err := AppendFile(file, before, answer, time.Now()); err != nil {
			t.Fatal(err)
		}
		appended, err := ReadFile(file, standInEndpoint)
		if err != nil {
			t.Fatal(err)
		}
		rig.run(t, file, "Thanks.")
		assertAppended(t, model, appended, sessionContent(t, file))
	})

	t.Run("SQL storage", func(t *testing.T) {
		dsn, dsnFile := testdb.DSNFile(t, "legion_conversation_omp_test")
		model := newAnthropicStandIn(t, firstRunReplies()...)
		rig := newOMPRig(t, conversation.AnthropicMessages, model, ompsessions.StorageVariable+"="+ompsessions.SQLStorage, ompsessions.DSNFileVariable+"="+dsnFile)
		rig.run(t, "", "Read note.txt and tell me what it says.")
		ctx := context.Background()
		conn, err := ompsessions.ConnectURL(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close(context.Background()) })
		admin, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = admin.Close(context.Background()) })
		var path string
		if err := admin.QueryRow(ctx, "SELECT path FROM omp_session_files").Scan(&path); err != nil {
			t.Fatalf("the session Oh My Pi wrote to the table: %v", err)
		}
		if files, _ := filepath.Glob(filepath.Join(rig.sessions, "*.jsonl")); len(files) != 0 {
			t.Fatalf("Oh My Pi under SQL storage wrote session files %v", files)
		}
		before, err := ReadSQL(ctx, conn, path, standInEndpoint)
		if err != nil {
			t.Fatal(err)
		}
		if err := AppendSQL(ctx, conn, path, before, answer, time.Now()); err != nil {
			t.Fatal(err)
		}
		appended, err := ReadSQL(ctx, conn, path, standInEndpoint)
		if err != nil {
			t.Fatal(err)
		}
		rig.run(t, path, "Thanks.")
		content, _, err := ompsessions.Read(ctx, conn, path)
		if err != nil {
			t.Fatal(err)
		}
		assertAppended(t, model, appended, content)
	})
}

func TestThePinnedOhMyPiResumesAnAppendedExchangeWithNoAnswer(t *testing.T) {
	a := claudeAnswer()
	a.Provider, a.Model = standInProvider, standInModel
	a.Turns = a.Turns[:1]
	a.NoAnswer = "The model returned no answer."
	toolID := a.Turns[0].Results[0].ToolCallID
	turns := resumeAfterFileAppend(t, func(path string, before *Session) error {
		return AppendFile(path, before, a, time.Now())
	})
	question := carriedOnce(t, turns, func(turn wireTurn, b wireBlock) bool {
		return turn.Role == "user" && b.Type == "text" && b.Text == a.Question
	}, "the question")
	call := carriedOnce(t, turns, func(turn wireTurn, b wireBlock) bool {
		return turn.Role == "assistant" && b.Type == "tool_use" && b.ID == toolID
	}, "the tool call")
	result := carriedOnce(t, turns, func(turn wireTurn, b wireBlock) bool {
		return turn.Role == "user" && b.Type == "tool_result" && b.ID == toolID && b.Result == "plum"
	}, "the tool result")
	note := carriedOnce(t, turns, func(turn wireTurn, b wireBlock) bool {
		return turn.Role == "user" && b.Type == "text" && b.Text == a.NoAnswer
	}, "the no-answer note")
	if !(question+1 == call && call+1 == result && result+1 == note && note == len(turns)-2) {
		t.Errorf("question, call, result and note at %d, %d, %d, %d of %d turns; want them before the follow-up",
			question, call, result, note, len(turns))
	}
}

func TestThePinnedOhMyPiResumesAnAppendedDelivery(t *testing.T) {
	d := conversation.Delivery{ID: "delivery-1", Text: "The tester says the note still says plum."}
	turns := resumeAfterFileAppend(t, func(path string, before *Session) error {
		return AppendDeliveryFile(path, before, d, time.Now())
	})
	delivery := carriedOnce(t, turns, func(turn wireTurn, b wireBlock) bool {
		return turn.Role == "user" && b.Type == "text" && b.Text == d.Text
	}, "the delivery")
	if delivery != len(turns)-2 {
		t.Errorf("delivery at %d of %d turns; want it before the follow-up", delivery, len(turns))
	}
}

func resumeAfterFileAppend(t *testing.T, appendTo func(string, *Session) error) []wireTurn {
	t.Helper()
	model := newAnthropicStandIn(t, firstRunReplies()...)
	rig := newOMPRig(t, conversation.AnthropicMessages, model)
	rig.run(t, "", "Read note.txt and tell me what it says.")
	file := rig.sessionFile(t)
	before, err := ReadFile(file, standInEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	if err := appendTo(file, before); err != nil {
		t.Fatal(err)
	}
	appended, err := ReadFile(file, standInEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	const followUp = "Thanks."
	rig.run(t, file, followUp)
	return assertResumeMatches(t, appended, sessionContent(t, file), model.lastAnthropic(t), followUp)
}

// After resuming an appended exchange, Oh My Pi sends the question, first reply, retry prompt,
// second reply and follow-up in that order, once each. The request matches what the reader
// rebuilds from the session as it stood just before the follow-up.
func TestThePinnedOhMyPiResumesAnAnswerAskedForAgain(t *testing.T) {
	answer := conversation.Append{
		QuestionID: "q-1", Question: "Which fruit does the note name?",
		Provider: standInProvider, API: conversation.AnthropicMessages, Model: standInModel,
		Turns: []conversation.Turn{
			{
				Content:    []conversation.Block{thinking("I cannot tell from here.", "cmVmdXNlZA=="), text(refusedReply)},
				AskedAgain: askedAgain,
				Usage:      conversation.Usage{InputTokens: 12, OutputTokens: 8},
			},
			{
				Content: []conversation.Block{thinking("Name what I need.", "YXNrZWQtYWdhaW4="), text(replyAgain)},
				Usage:   conversation.Usage{InputTokens: 20, OutputTokens: 5},
			},
		},
	}
	model := newAnthropicStandIn(t, firstRunReplies()...)
	rig := newOMPRig(t, conversation.AnthropicMessages, model)
	rig.run(t, "", "Read note.txt and tell me what it says.")
	file := rig.sessionFile(t)
	before, err := ReadFile(file, standInEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	if err := AppendFile(file, before, answer, time.Now()); err != nil {
		t.Fatal(err)
	}
	appended, err := ReadFile(file, standInEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	rig.run(t, file, "Thanks.")
	turns := assertResumeMatches(t, appended, sessionContent(t, file), model.lastAnthropic(t), "Thanks.")

	// The reader rebuilds from the same appended session Oh My Pi resumes, so a re-ask the
	// appender dropped or wrote out of place would leave the two agreeing; these checks hold the
	// request to the order the run made.
	question := carriedOnce(t, turns, func(turn wireTurn, b wireBlock) bool {
		return turn.Role == "user" && b.Type == "text" && b.Text == answer.Question
	}, "the question")
	refused := carriedOnce(t, turns, func(turn wireTurn, b wireBlock) bool {
		return turn.Role == "assistant" && b.Type == "text" && b.Text == refusedReply
	}, "the refused reply")
	reAsk := carriedOnce(t, turns, func(turn wireTurn, b wireBlock) bool {
		return turn.Role == "user" && b.Type == "text" && b.Text == askedAgain
	}, "the message the loop asked again with")
	accepted := carriedOnce(t, turns, func(turn wireTurn, b wireBlock) bool {
		return turn.Role == "assistant" && b.Type == "text" && b.Text == replyAgain
	}, "the reply after it")
	if !(question+1 == refused && refused+1 == reAsk && reAsk+1 == accepted && accepted+2 == len(turns)) {
		t.Errorf("the question at %d, the refused reply at %d, the message it was asked again with at %d, the reply after it at %d of %d turns; want them one after another, then the follow-up",
			question, refused, reAsk, accepted, len(turns))
	}
	if refused >= 0 {
		for _, b := range turns[refused].Blocks {
			if b.Type == "tool_use" {
				t.Errorf("the refused reply's turn carries a tool call %s; want a reply with none", b.ID)
			}
		}
	}
	if reAsk >= 0 && len(turns[reAsk].Blocks) != 1 {
		t.Errorf("the message the loop asked again with is a user turn of %d blocks; want it alone", len(turns[reAsk].Blocks))
	}
}
