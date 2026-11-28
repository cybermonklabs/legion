package omp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/conversation"
)

const fixtureHeader = `{"type":"session","version":3,"id":"0001","timestamp":"2026-10-26T00:00:00.000Z","cwd":"/work"}`

// fixture builds a session's content an entry at a time, each entry chained to the one before
// unless it names its own parent.
type fixture struct {
	lines []string
	last  string
	n     int
}

func newFixture() *fixture { return &fixture{lines: []string{fixtureHeader}} }

// add appends entry, a JSON object with neither id nor parentId, as the next entry, and returns
// its id.
func (f *fixture) add(entry string) string {
	return f.addAfter(f.last, entry)
}

// addAfter appends entry with parent as its parent ("" for none).
func (f *fixture) addAfter(parent, entry string) string {
	f.n++
	id := fmt.Sprintf("e%07d", f.n)
	parentJSON := "null"
	if parent != "" {
		parentJSON = `"` + parent + `"`
	}
	f.lines = append(f.lines, fmt.Sprintf(`{"id":%q,"parentId":%s,"timestamp":"2026-10-26T00:00:00.000Z",%s`, id, parentJSON, entry[1:]))
	f.last = id
	return id
}

func (f *fixture) bytes() []byte { return []byte(strings.Join(f.lines, "\n") + "\n") }

func userEntry(text string) string {
	return `{"type":"message","message":{"role":"user","content":[{"type":"text","text":` + quote(text) + `}],"attribution":"user","timestamp":1}}`
}

func assistantEntry(provider, api, model, stopReason, content string) string {
	return `{"type":"message","message":{"role":"assistant","content":` + content + `,"api":"` + api + `","provider":"` + provider +
		`","model":"` + model + `","usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":2,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"stopReason":"` +
		stopReason + `","timestamp":2}}`
}

func claudeEntry(stopReason, content string) string {
	return assistantEntry("anthropic", conversation.AnthropicMessages, "claude-opus-5-5", stopReason, content)
}

func toolResultEntry(id, name, text string, isError bool) string {
	return fmt.Sprintf(`{"type":"message","message":{"role":"toolResult","toolCallId":%s,"toolName":%s,"content":[{"type":"text","text":%s}],"isError":%t,"timestamp":3}}`,
		quote(id), quote(name), quote(text), isError)
}

func quote(s string) string {
	encoded, _ := json.Marshal(s)
	return string(encoded)
}

func read(t *testing.T, content []byte) *Session {
	t.Helper()
	s, err := Read(content, Options{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return s
}

func text(s string) conversation.Block { return conversation.Block{Type: conversation.Text, Text: s} }

func systemText(s string) conversation.Message {
	return conversation.Message{Role: conversation.System, Content: []conversation.Block{text(s)}}
}

func user(texts ...string) conversation.Message {
	m := conversation.Message{Role: conversation.User}
	for _, s := range texts {
		m.Content = append(m.Content, text(s))
	}
	return m
}

func claude(blocks ...conversation.Block) conversation.Message {
	return conversation.Message{Role: conversation.Assistant, Provider: "anthropic", API: conversation.AnthropicMessages, Model: "claude-opus-5-5", Content: blocks}
}

func result(id, name, output string, isError bool) conversation.Message {
	return conversation.Message{Role: conversation.Tool, Content: []conversation.Block{{
		Type: conversation.ToolResult, ToolCallID: id, ToolName: name, IsError: isError, Content: []conversation.Block{text(output)},
	}}}
}

func call(id, name, arguments string) conversation.Block {
	return conversation.Block{Type: conversation.ToolCall, ToolCallID: id, ToolName: name, Arguments: json.RawMessage(arguments)}
}

func thinking(s, signature string) conversation.Block {
	return conversation.Block{Type: conversation.Thinking, Text: s, Signature: signature}
}

func assertMessages(t *testing.T, got, want []conversation.Message) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.MarshalIndent(got, "", "  ")
		wantJSON, _ := json.MarshalIndent(want, "", "  ")
		t.Errorf("messages:\n%s\nwant:\n%s", gotJSON, wantJSON)
	}
}

// A session reads as the request Oh My Pi sends on resume: the user turn, the signed thinking with
// its signature, the redacted thinking's data, the text and the tool call with its arguments byte
// for byte, the result, the answer; and the model, API and thinking level the conversation last
// ran at, from its model and thinking-level changes.
func TestReadRebuildsTheConversationOhMyPiResumes(t *testing.T) {
	f := newFixture()
	f.add(`{"type":"model_change","model":"anthropic/claude-opus-5-5"}`)
	f.add(`{"type":"thinking_level_change","thinkingLevel":"high","configured":null}`)
	f.add(userEntry("Read note.txt."))
	f.add(`{"type":"custom","customType":"system-prompt-digest","data":"284852018acfe3b4"}`)
	f.add(claudeEntry("toolUse", `[{"type":"thinking","thinking":"I should read it.","thinkingSignature":"c2ln"},{"type":"redactedThinking","data":"cmVk"},{"type":"text","text":"Reading."},{"type":"toolCall","id":"toolu_01","name":"read","arguments":{"path": "note.txt"}}]`))
	f.add(`{"type":"custom","customType":"tool_execution_start","data":{"toolCallId":"toolu_01"}}`)
	f.add(toolResultEntry("toolu_01", "read", "plum", false))
	f.add(claudeEntry("stop", `[{"type":"thinking","thinking":"It says plum.","thinkingSignature":"c2lnMg=="},{"type":"text","text":"Plum."}]`))
	f.add(`{"type":"custom","customType":"session_exit","data":{"reason":"dispose"}}`)
	s := read(t, f.bytes())

	assertMessages(t, s.Messages, []conversation.Message{
		user("Read note.txt."),
		claude(thinking("I should read it.", "c2ln"), conversation.Block{Type: conversation.RedactedThinking, Data: "cmVk"}, text("Reading."), call("toolu_01", "read", `{"path": "note.txt"}`)),
		result("toolu_01", "read", "plum", false),
		claude(thinking("It says plum.", "c2lnMg=="), text("Plum.")),
	})
	if s.Model != "anthropic/claude-opus-5-5" || s.API != conversation.AnthropicMessages || s.ThinkingLevel != "high" {
		t.Errorf("model %q, API %q, thinking level %q", s.Model, s.API, s.ThinkingLevel)
	}
	if s.ID != "0001" || s.Leaf != f.last || s.Torn || s.Size != int64(len(f.bytes())) {
		t.Errorf("session %q, leaf %q, torn %t, size %d", s.ID, s.Leaf, s.Torn, s.Size)
	}
}

// A compaction replaces everything before the entry it keeps from with its summary, in Oh My Pi's
// template, ahead of the kept entries and everything after it; a handoff compaction's summary is in
// the handoff template. Opt-in Anthropic replay replaces the text with a native block.
// A /clear boundary after the compaction drops it and all before.
func TestReadReplacesCompactedHistoryWithItsSummary(t *testing.T) {
	build := func(method, preserve string, reset bool) []byte {
		f := newFixture()
		f.add(userEntry("first request"))
		f.add(claudeEntry("stop", `[{"type":"text","text":"first answer"}]`))
		kept := f.add(userEntry("second request"))
		f.add(claudeEntry("stop", `[{"type":"text","text":"second answer"}]`))
		f.add(`{"type":"compaction","summary":"Did the first thing.\n\n\n\nThen more.  ","firstKeptEntryId":"` + kept + `","tokensBefore":100` + method + preserve + `}`)
		if reset {
			f.add(`{"type":"reset_boundary"}`)
		}
		f.add(userEntry("third request"))
		f.add(claudeEntry("stop", `[{"type":"text","text":"third answer"}]`))
		return f.bytes()
	}
	summary := "Prior model work/tool state available.\nMUST build on prior work; NEVER duplicate prior work.\n\n<summary>\nDid the first thing.\nThen more.\n</summary>"
	tail := []conversation.Message{user("second request"), claude(text("second answer")), user("third request"), claude(text("third answer"))}

	s := read(t, build("", "", false))
	assertMessages(t, s.Messages, append([]conversation.Message{user(summary)}, tail...))

	s = read(t, build(`,"method":"handoff"`, "", false))
	if got := s.Messages[0].Content[0].Text; !strings.HasPrefix(got, "Context replaced. The <handoff> below") || !strings.HasSuffix(got, "<handoff>\nDid the first thing.\nThen more.\n</handoff>") {
		t.Errorf("handoff summary = %q", got)
	}

	payload := `{"provider":"anthropic","content":"API summary","signature":"c2ln"}`
	content := build("", `,"preserveData":{"anthropicCompaction":`+payload+`}`, false)
	assertMessages(t, read(t, content).Messages, append([]conversation.Message{user(summary)}, tail...))
	s, err := Read(content, Options{ReplayAnthropicCompaction: true})
	if err != nil {
		t.Fatal(err)
	}
	assertMessages(t, s.Messages, append([]conversation.Message{{
		Role: conversation.Assistant, Native: &conversation.Native{
			Kind: conversation.AnthropicCompaction, Provider: "anthropic", Payload: json.RawMessage(payload),
		},
	}}, tail...))

	s = read(t, build("", "", true))
	assertMessages(t, s.Messages, []conversation.Message{user("third request"), claude(text("third answer"))})
}

// Only the resolved branch is replayed, including its branch summary in the pin's template.
func TestReadFollowsTheActiveBranch(t *testing.T) {
	f := newFixture()
	root := f.add(userEntry("request"))
	f.add(claudeEntry("stop", `[{"type":"text","text":"abandoned answer"}]`))
	f.addAfter(root, `{"type":"branch_summary","fromId":"x","summary":"Tried another way."}`)
	f.add(claudeEntry("stop", `[{"type":"text","text":"kept answer"}]`))
	s := read(t, f.bytes())
	assertMessages(t, s.Messages, []conversation.Message{
		user("request"),
		user("Branch-return summary:\n\n<summary>\nTried another way.\n</summary>"),
		claude(text("kept answer")),
	})
}

func TestReadResolvesCapturedLeafAndAppendsAfterIt(t *testing.T) {
	for _, tc := range []struct {
		name, capture string
		empty         bool
	}{
		{"late usage", `"e0000005"`, false},
		{"stale captured leaf", `"missing"`, false},
		{"null captured leaf", `null`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			f.add(`{"type":"model_change","model":"anthropic/claude-opus-5-5"}`)
			f.add(userEntry("first"))
			one := f.add(claudeEntry("stop", `[{"type":"text","text":"one"}]`))
			f.add(userEntry("second"))
			two := f.add(claudeEntry("stop", `[{"type":"text","text":"two"}]`))
			f.addAfter(one, `{"type":"model_usage","activeLeafId":`+quote(two)+`}`)
			f.addAfter(one, `{"type":"model_usage","activeLeafId":`+tc.capture+`}`)
			s := read(t, f.bytes())
			wantLeaf := two
			var wantParent any = two
			want := []conversation.Message{user("first"), claude(text("one")), user("second"), claude(text("two"))}
			if tc.empty {
				wantLeaf, wantParent, want = "", nil, nil
				if s.Model != "" || s.API != "" || s.ThinkingLevel != "off" {
					t.Errorf("cleared session: model %q, API %q, thinking level %q; want empty model and API and thinking off", s.Model, s.API, s.ThinkingLevel)
				}
			}
			if s.Leaf != wantLeaf {
				t.Errorf("Leaf = %q, want %q", s.Leaf, wantLeaf)
			}
			assertMessages(t, s.Messages, want)
			appended, err := s.Encode(claudeAnswer(), appendedAt)
			if err != nil {
				t.Fatal(err)
			}
			first, _, _ := bytes.Cut(appended, []byte("\n"))
			var record map[string]any
			if err := json.Unmarshal(first, &record); err != nil {
				t.Fatal(err)
			}
			if parent, ok := record["parentId"]; !ok || !reflect.DeepEqual(parent, wantParent) {
				t.Errorf("first appended entry parentId = %v (present: %t), want %v", parent, ok, wantParent)
			}
		})
	}
}

func TestReadCarriesDerivedOpenAICompactionPayload(t *testing.T) {
	f := newFixture()
	f.add(userEntry("request"))
	f.add(assistantEntry("openai", conversation.OpenAIResponses, "gpt-6.1", "stop", `[{"type":"text","text":"answer"}]`))
	first := `{"type":"compaction","encrypted_content":"first"}`
	second := `{"type":"compaction_summary","encrypted_content":"second"}`
	items := `[` + first + `,` + second + `]`
	f.add(`{"type":"compaction","summary":"Compacted.","firstKeptEntryId":"","preserveData":{"openaiRemoteCompaction":{"provider":"openai","replacementHistory":` + items + `}}}`)
	s := read(t, f.bytes())
	payload := `{"type":"openaiResponsesHistory","provider":"openai","items":` + items + `}`
	if len(s.Messages) != 1 || s.Messages[0].Native == nil {
		t.Fatalf("messages = %+v, want a summary carrying native replay", s.Messages)
	}
	native := s.Messages[0].Native
	if native.Kind != conversation.OpenAIResponsesHistory || native.Provider != "openai" || native.Incremental || string(native.Payload) != payload {
		t.Errorf("native = %+v, Payload = %s, want %s", native, native.Payload, payload)
	}
	if len(native.Items) != 2 || string(native.Items[0]) != first || string(native.Items[1]) != second {
		t.Errorf("native replay items = %q, want %s in order", native.Items, items)
	}
}

// entry-patches.ts:145–183,202–237,298–343: content patches replace
// message and custom-message content in file order, even before their target.
func TestReadAppliesEntryPatchContent(t *testing.T) {
	f := newFixture()
	f.add(entryPatchRecord(`{"entryId":"e0000002","message":{"content":"new developer text"}}`))
	f.add(`{"type":"message","message":{"role":"developer","content":"old developer text","timestamp":1}}`)
	request := f.add(userEntry("old request"))
	custom := f.add(`{"type":"custom_message","customType":"note","content":"old note"}`)
	answer := f.add(claudeEntry("stop", `[{"type":"text","text":"old answer"}]`))
	f.add(entryPatchRecord(`{"entryId":` + quote(request) + `,"message":{"content":"intermediate request"}}`))
	f.add(entryPatchRecord(
		`{"entryId":`+quote(request)+`,"message":{"content":"latest request"}}`,
		`{"entryId":`+quote(custom)+`,"entry":{"content":[{"type":"text","text":"new note"}]}}`,
		`{"entryId":`+quote(answer)+`,"message":{"content":[{"type":"text","text":"new answer"}]}}`,
	))
	assertMessages(t, read(t, f.bytes()).Messages, []conversation.Message{
		user("new developer text"), user("latest request"), systemText("new note"), claude(text("new answer")),
	})
}

// entry-patches.ts:180,225 and agent/src/compaction/messages.ts:92–110:
// prunedAt makes a tool result replay as one combined text block.
func TestReadAppliesEntryPatchPrunedAt(t *testing.T) {
	f := newFixture()
	f.add(userEntry("request"))
	f.add(claudeEntry("toolUse", `[{"type":"toolCall","id":"call","name":"read","arguments":{}}]`))
	id := f.add(`{"type":"message","message":{"role":"toolResult","toolCallId":"call","toolName":"read","content":[{"type":"text","text":"part one"},{"type":"text","text":" and two"}],"isError":false,"timestamp":3}}`)
	f.add(entryPatchRecord(`{"entryId":` + quote(id) + `,"message":{"prunedAt":4}}`))
	assertMessages(t, read(t, f.bytes()).Messages, []conversation.Message{
		user("request"), claude(call("call", "read", `{}`)), result("call", "read", "part one and two", false),
	})
}

// entry-patches.ts:161–169,202–208: preserveData replaces the compaction's
// whole object, rather than merging keys from the previous revision.
func TestReadAppliesEntryPatchPreserveData(t *testing.T) {
	f := newFixture()
	f.add(claudeEntry("stop", `[{"type":"text","text":"earlier"}]`))
	id := f.add(`{"type":"compaction","summary":"summary","firstKeptEntryId":"","preserveData":{"snapcompact":{}}}`)
	payload := `{"provider":"anthropic","content":"native summary","signature":"sig"}`
	f.add(entryPatchRecord(`{"entryId":` + quote(id) + `,"entry":{"preserveData":{"anthropicCompaction":` + payload + `}}}`))
	s, err := Read(f.bytes(), Options{ReplayAnthropicCompaction: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 1 || s.Messages[0].Role != conversation.Assistant ||
		s.Messages[0].Native == nil || string(s.Messages[0].Native.Payload) != payload {
		t.Fatalf("messages = %+v, want only the summary with the replacement payload", s.Messages)
	}
}

// entry-patches.ts:161–183,247–280,298–327: malformed warning or tokensAfter
// skips the full record, but a valid field on a non-compaction skips only its
// patch. These fields are checked even though the conversation does not emit them.
func TestReadValidatesEntryPatchCompactionMetadata(t *testing.T) {
	for _, tc := range []struct{ field, valid, malformed string }{
		{"warning", `"rescue warning"`, `123`},
		{"tokensAfter", `15`, `"15"`},
	} {
		t.Run(tc.field, func(t *testing.T) {
			f := newFixture()
			f.add(userEntry("cut"))
			first := f.add(userEntry("old text"))
			second := f.add(userEntry("other text"))
			f.add(claudeEntry("stop", `[{"type":"text","text":"answer"}]`))
			compaction := f.add(`{"type":"compaction","summary":"summary","firstKeptEntryId":` + quote(first) + `}`)
			meta := fmt.Sprintf(`{"entryId":%q,"entry":{%q:%s}}`, compaction, tc.field, tc.valid)
			f.add(entryPatchRecord(
				meta,
				`{"entryId":`+quote(first)+`,"message":{"content":"new text"}}`,
				fmt.Sprintf(`{"entryId":%q,"entry":{%q:%s},"message":{"content":"wrong target"}}`, second, tc.field, tc.valid),
			))
			f.add(entryPatchRecord(
				`{"entryId":`+quote(first)+`,"message":{"content":"malformed record"}}`,
				fmt.Sprintf(`{"entryId":%q,"entry":{%q:%s}}`, compaction, tc.field, tc.malformed),
			))
			s := read(t, f.bytes())
			if len(s.Messages) != 4 {
				t.Fatalf("messages = %+v, want summary and three kept messages", s.Messages)
			}
			assertMessages(t, s.Messages[1:], []conversation.Message{user("new text"), user("other text"), claude(text("answer"))})
		})
	}
}

func entryPatchRecord(patches ...string) string {
	return `{"type":"custom","customType":"entry_patch","data":{"patches":[` + strings.Join(patches, ",") + `]}}`
}

// session-context.ts:407–416: an imported synthetic API error does not
// replace the last real model selected by legacy assistant inference.
func TestReadDoesNotInferASyntheticAssistantModel(t *testing.T) {
	f := newFixture()
	f.add(userEntry("request"))
	f.add(claudeEntry("stop", `[{"type":"text","text":"answer"}]`))
	f.add(assistantEntry("anthropic", conversation.AnthropicMessages, "<synthetic>", "error", `[]`))
	s := read(t, f.bytes())
	if s.Model != "anthropic/claude-opus-5-5" || s.API != conversation.AnthropicMessages {
		t.Errorf("selected %s on %s, want the last real assistant's model", s.Model, s.API)
	}
	assertMessages(t, s.Messages, []conversation.Message{user("request"), claude(text("answer"))})
}

// session-context.ts:69–84,657–673: a user-attributed skill prompt or a parent's IRC
// message can be the last request restored before the kept tail.
func TestReadRestoresCustomRolloverRequests(t *testing.T) {
	for _, tc := range []struct {
		name, record string
		request      conversation.Message
	}{
		{"user skill prompt", `{"type":"custom_message","customType":"skill-prompt","attribution":"user","content":"skill request"}`, user("skill request")},
		{"user collaborator prompt", `{"type":"custom_message","customType":"collab-prompt","attribution":"user","content":"collaborator request"}`,
			user(render(steeringTemplate, "{{message}}", "collaborator request"))},
		{"agent skill prompt", `{"type":"custom_message","customType":"skill-prompt","attribution":"agent","content":"skill request"}`, user("plain request")},
		{"parent IRC", `{"type":"custom_message","customType":"irc:incoming","details":{"fromParent":true},"content":"parent request"}`, systemText("parent request")},
		{"other IRC", `{"type":"custom_message","customType":"irc:incoming","details":{"fromParent":false},"content":"another IRC message"}`, user("plain request")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			f.add(userEntry("plain request"))
			f.add(tc.record)
			kept := f.add(claudeEntry("stop", `[{"type":"text","text":"kept answer"}]`))
			f.add(`{"type":"compaction","summary":"summary","firstKeptEntryId":` + quote(kept) +
				`,"details":{"kind":"experimental-context-rollover"}}`)
			messages := read(t, f.bytes()).Messages
			if len(messages) != 3 {
				t.Fatalf("messages = %+v, want summary, restored request, kept answer", messages)
			}
			assertMessages(t, messages[1:], []conversation.Message{tc.request, claude(text("kept answer"))})
		})
	}
}

// sdk.ts:5547–5562 and agent/src/compaction/compaction.ts:1429–1435:
// native Responses history belongs to its provider and needs a Responses API.
// A model that cannot read it resumes from the previous readable compaction.
func TestReadUsesTheLastCompactionItsModelCanReplay(t *testing.T) {
	for _, tc := range []struct {
		name, provider, api, model, preserve string
		placeholder                          bool
	}{
		{"different provider", "openai", conversation.OpenAIResponses, "gpt-test", `{"provider":"other","replacementHistory":[{"type":"compaction","encrypted_content":"encoded"}]}`, false},
		{"different API", "anthropic", conversation.AnthropicMessages, "claude-test", `{"provider":"anthropic","replacementHistory":[{"type":"compaction","encrypted_content":"encoded"}]}`, false},
		{"missing provider on classic compaction", "openai", conversation.OpenAIResponses, "gpt-test", `{"replacementHistory":[{"type":"compaction","encrypted_content":"encoded"}],"compactionItem":{"type":"compaction","encrypted_content":"encoded"}}`, false},
		{"missing provider on compaction summary", "openai", conversation.OpenAIResponses, "gpt-test", `{"replacementHistory":[{"type":"compaction_summary"}],"compactionItem":{"type":"compaction_summary"}}`, false},
		{"classic item without encrypted content", "openai", conversation.OpenAIResponses, "gpt-test", `{"replacementHistory":[{"type":"compaction"}],"compactionItem":{"type":"compaction"}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			f.add(userEntry("cut"))
			f.add(`{"type":"compaction","summary":"Readable summary.","firstKeptEntryId":""}`)
			f.add(userEntry("keep"))
			f.add(`{"type":"compaction","summary":"Replacement placeholder.","firstKeptEntryId":"","preserveData":{"openaiRemoteCompaction":` + tc.preserve + `}}`)
			f.add(assistantEntry(tc.provider, tc.api, tc.model, "stop", `[{"type":"text","text":"answer"}]`))
			s := read(t, f.bytes())
			if tc.placeholder {
				if len(s.Messages) != 2 || s.Messages[0].Native != nil ||
					!strings.Contains(s.Messages[0].Content[0].Text, "Replacement placeholder.") ||
					s.Messages[1].Content[0].Text != "answer" {
					t.Fatalf("messages = %+v, want the placeholder compaction and later answer", s.Messages)
				}
				return
			}
			if len(s.Messages) != 3 || s.Messages[0].Native != nil ||
				!strings.Contains(s.Messages[0].Content[0].Text, "Readable summary.") {
				t.Fatalf("messages = %+v, want the readable summary, kept request, and later answer", s.Messages)
			}
			if s.Messages[1].Content[0].Text != "keep" || s.Messages[2].Content[0].Text != "answer" {
				t.Errorf("kept turns = %+v", s.Messages[1:])
			}
		})
	}
}

// agent/src/compaction/compaction.ts:1509–1521: keep firstKeptEntryId itself
// or the successor of the native replay-through entry. session-entries.ts:437–446
// visits each ID once, so the reader's forward lookup and the pin's backward lookup
// select the same entry even if the links form a cycle.
func TestReadRespectsCompactionTailBoundaries(t *testing.T) {
	for _, tc := range []struct{ name, boundary string }{
		{"first kept", `"firstKeptEntryId":"retained"`},
		{"native replay through", `"firstKeptEntryId":"","providerReplayThroughEntryId":"covered","preserveData":{"anthropicCompaction":{"provider":"anthropic","content":"Native summary."}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			f.lines = append(f.lines,
				`{"id":"covered","parentId":"retained",`+userEntry("do not replay this")[1:],
				`{"id":"retained","parentId":"covered",`+claudeEntry("stop", `[{"type":"text","text":"kept turn"}]`)[1:],
			)
			f.addAfter("retained", `{"type":"compaction","summary":"summary",`+tc.boundary+`}`)
			s := read(t, f.bytes())
			if len(s.Messages) != 2 {
				t.Fatalf("messages = %+v, want summary and only the retained turn", s.Messages)
			}
			assertMessages(t, s.Messages[1:], []conversation.Message{claude(text("kept turn"))})
		})
	}
}

// agent/src/compaction/anthropic.ts:80–89: a present toolChanges must be an
// array of objects, or only the text summary can be replayed.
func TestReadRejectsMalformedNativeToolChanges(t *testing.T) {
	for _, tc := range []struct{ name, raw string }{
		{"null", `null`},
		{"object", `{}`},
		{"non-object item", `[true]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			f.add(claudeEntry("stop", `[{"type":"text","text":"earlier"}]`))
			f.add(`{"type":"compaction","summary":"Readable text.","firstKeptEntryId":"","preserveData":{"anthropicCompaction":{"provider":"anthropic","content":"Signed block","signature":"sig","toolChanges":` + tc.raw + `}}}`)
			s, err := Read(f.bytes(), Options{ReplayAnthropicCompaction: true})
			if err != nil {
				t.Fatal(err)
			}
			if len(s.Messages) != 1 || s.Messages[0].Native != nil ||
				!strings.Contains(s.Messages[0].Content[0].Text, "Readable text.") {
				t.Fatalf("messages = %+v, want just the text summary", s.Messages)
			}
		})
	}
}

// ai/src/providers/transform-messages.ts:1110–1128,1159–1164,1215–1219:
// a failed turn drops only its trailing thinking signature. An abandoned
// tool-use turn strips every signature except on the latest replay from the
// same Anthropic provider.
func TestReadReplaysOnlyTrustedFailedTurnSignatures(t *testing.T) {
	t.Run("Anthropic continued abort", func(t *testing.T) {
		f := newFixture()
		f.add(userEntry("request"))
		f.add(withMessageFields(claudeEntry("aborted", `[{"type":"thinking","thinking":"earlier","thinkingSignature":"c2ln"},{"type":"toolCall","id":"call","name":"read","arguments":{}},{"type":"thinking","thinking":"trailing","thinkingSignature":"dGFpbA=="}]`), `,"continued":true`))
		f.add(toolResultEntry("call", "read", "plum", false))
		f.add(claudeEntry("stop", `[{"type":"text","text":"answer"}]`))
		assertMessages(t, read(t, f.bytes()).Messages, []conversation.Message{
			user("request"), claude(thinking("earlier", "c2ln"), call("call", "read", `{}`)),
			result("call", "read", "plum", false), claude(text("answer")),
		})
	})

	responsesEntry := func(stop, content string) string {
		return assistantEntry("openai", conversation.OpenAIResponses, "gpt-6.1", stop, content)
	}
	responses := func(blocks ...conversation.Block) conversation.Message {
		return conversation.Message{Role: conversation.Assistant, Provider: "openai", API: conversation.OpenAIResponses, Model: "gpt-6.1", Content: blocks}
	}
	t.Run("OpenAI Responses continued error", func(t *testing.T) {
		f := newFixture()
		f.add(userEntry("request"))
		f.add(withMessageFields(responsesEntry("error", `[{"type":"thinking","thinking":"earlier","thinkingSignature":"{\"type\":\"reasoning\",\"id\":\"rs_complete\"}"},{"type":"text","text":"partial answer"},{"type":"thinking","thinking":"trailing","thinkingSignature":"{\"type\":\"reasoning\",\"id\":\"rs_incomplete\"}"}]`), `,"continued":true`))
		f.add(continueReminderEntry())
		f.add(responsesEntry("stop", `[{"type":"text","text":"answer"}]`))
		assertMessages(t, read(t, f.bytes()).Messages, []conversation.Message{
			user("request"),
			responses(text("partial answer")),
			user("Pick up where you stopped."), responses(text("answer")),
		})
	})
	t.Run("OpenAI Responses abandoned tool use", func(t *testing.T) {
		f := newFixture()
		f.add(userEntry("request"))
		f.add(responsesEntry("stop", `[{"type":"thinking","thinking":"earlier","thinkingSignature":"{\"type\":\"reasoning\",\"id\":\"rs_stale\"}"},{"type":"toolCall","id":"call","name":"read","arguments":{}},{"type":"thinking","thinking":"","thinkingSignature":"{\"type\":\"reasoning\",\"id\":\"rs_stale_empty\"}"}]`))
		f.add(toolResultEntry("call", "read", "tool not run", true))
		assertMessages(t, read(t, f.bytes()).Messages, []conversation.Message{
			user("request"), responses(call("call", "read", `{}`)),
			result("call", "read", "tool not run", true),
		})
	})
	t.Run("latest Anthropic abandoned tool use", func(t *testing.T) {
		f := newFixture()
		f.add(userEntry("request"))
		f.add(claudeEntry("stop", `[{"type":"thinking","thinking":"plan","thinkingSignature":"c2ln"},{"type":"toolCall","id":"call","name":"read","arguments":{}}]`))
		f.add(toolResultEntry("call", "read", "tool not run", true))
		assertMessages(t, read(t, f.bytes()).Messages, []conversation.Message{
			user("request"), claude(thinking("plan", "c2ln"), call("call", "read", `{}`)),
			result("call", "read", "tool not run", true),
		})
	})
}

// A tool call interrupted before its result is not replayed: the call is dropped from its turn and
// the turn's signatures with it, so its now-unsigned thinking is dropped on Anthropic, as its
// redacted thinking is. A turn that failed or was aborted is dropped with its tool results, a turn
// that failed empty and a retry-recovery turn are skipped, and a cut-off thinking-only turn is not
// replayed.
func TestReadDropsInterruptedAndFailedTurns(t *testing.T) {
	f := newFixture()
	f.add(userEntry("request"))
	f.add(claudeEntry("toolUse", `[{"type":"thinking","thinking":"try","thinkingSignature":"c2ln"},{"type":"toolCall","id":"toolu_failed","name":"bash","arguments":{}}]`))
	f.add(toolResultEntry("toolu_failed", "bash", "aborted", true))
	f.add(claudeEntry("error", `[{"type":"text","text":"half an answer"},{"type":"toolCall","id":"toolu_err","name":"bash","arguments":{}}]`))
	f.add(toolResultEntry("toolu_err", "bash", "aborted", true))
	f.add(claudeEntry("error", `[{"type":"text","text":"  "}]`))
	f.add(`{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"recovering"}],"api":"anthropic-messages","provider":"anthropic","model":"claude-opus-5-5","stopReason":"stop","retryRecovery":true,"timestamp":2}}`)
	f.add(claudeEntry("length", `[{"type":"thinking","thinking":"cut off","thinkingSignature":"c2ln"}]`))
	f.add(userEntry("again"))
	f.add(claudeEntry("toolUse", `[{"type":"thinking","thinking":"I will look.","thinkingSignature":"c2lnMw=="},{"type":"redactedThinking","data":"cmVk"},{"type":"text","text":"Looking."},{"type":"toolCall","id":"toolu_dangling","name":"read","arguments":{"path":"x"}}]`))
	s := read(t, f.bytes())
	assertMessages(t, s.Messages, []conversation.Message{
		user("request"),
		claude(thinking("try", "c2ln"), call("toolu_failed", "bash", `{}`)),
		result("toolu_failed", "bash", "aborted", true),
		user("again"),
		claude(text("Looking.")),
	})
}

// An uncontinued turn with truthy retryRecovery is skipped, whether the field is true or an object.
func TestReadSkipsARetryRecoveryTurnOfEitherShape(t *testing.T) {
	for _, testCase := range []struct {
		name, retryRecovery string
		kept                bool
	}{
		{"true", `true`, false},
		{"an object", `{"kind":"auto-retry","status":"recovered","attempt":1,"recoveredAt":"2026-11-25T11:56:19.129Z","recovery":"plain","note":"error; retried","supersededBy":{"timestamp":1795607764754,"responseId":"resp_x","provider":"openai","model":"gpt-6.1-sol"}}`, false},
		{"false", `false`, true},
		{"null", `null`, true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			f := newFixture()
			f.add(userEntry("request"))
			f.add(`{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"recovering"}],"api":"anthropic-messages","provider":"anthropic","model":"claude-opus-5-5","stopReason":"stop","retryRecovery":` + testCase.retryRecovery + `,"timestamp":2}}`)
			f.add(userEntry("again"))
			want := []conversation.Message{user("request"), user("again")}
			if testCase.kept {
				want = []conversation.Message{user("request"), claude(text("recovering")), user("again")}
			}
			assertMessages(t, read(t, f.bytes()).Messages, want)
		})
	}
}

func TestReadKeepsContinuedFailedTurnsAndTheirReminders(t *testing.T) {
	for _, stop := range []string{"error", "aborted"} {
		t.Run(stop, func(t *testing.T) {
			f := newFixture()
			f.add(userEntry("request"))
			f.add(withMessageFields(claudeEntry(stop, `[{"type":"text","text":"partial"}]`), `,"continued":true,"retryRecovery":{}`))
			f.add(continueReminderEntry())
			f.add(claudeEntry("stop", `[{"type":"text","text":"done"}]`))
			assertMessages(t, read(t, f.bytes()).Messages, []conversation.Message{
				user("request"), claude(text("partial")), user("Pick up where you stopped."), claude(text("done")),
			})
		})
	}
}

func TestReadDropsContinueRemindersWithoutTheirContinuedTurn(t *testing.T) {
	const partial = `[{"type":"text","text":"partial"}]`
	for _, tc := range []struct {
		name, stop, content, fields string
		beforeTurn, afterTurn       string
		kept                        []conversation.Message
	}{
		{name: "retry recovery even after an earlier continued turn", stop: "error", content: partial, fields: `,"retryRecovery":{}`},
		{name: "continued must be a boolean", stop: "stop", content: partial, fields: `,"retryRecovery":true,"continued":"true"`},
		{name: "empty error even if continued", stop: "error", content: `[{"type":"text","text":"  "}]`, fields: `,"continued":true`},
		{name: "empty fallback error even if continued", stop: "error", content: `[{"type":"fallback"}]`, fields: `,"continued":true`},
		{name: "failed turn without continuation", stop: "aborted", content: partial},
		{name: "successful turn without continuation", stop: "stop", content: partial, kept: []conversation.Message{user("Continue."), claude(text("partial"))}},
		{name: "its tool call was stripped", stop: "error", content: `[{"type":"toolCall","id":"missing","name":"read","arguments":{}}]`, fields: `,"continued":true`,
			beforeTurn: userEntry("new request"), kept: []conversation.Message{user("new request")}},
		{name: "another user turn intervenes", stop: "error", content: partial, fields: `,"continued":true`,
			afterTurn: userEntry("new request"), kept: []conversation.Message{user("Continue."), claude(text("partial")), user("new request")}},
		{name: "another custom turn intervenes", stop: "error", content: partial, fields: `,"continued":true`,
			afterTurn: `{"type":"custom_message","customType":"other","content":"other note"}`, kept: []conversation.Message{user("Continue."), claude(text("partial")), user("other note")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			f.add(userEntry("request"))
			f.add(withMessageFields(claudeEntry("error", `[{"type":"text","text":"earlier continued turn"}]`), `,"continued":true`))
			if tc.beforeTurn != "" {
				f.add(tc.beforeTurn)
			}
			f.add(withMessageFields(claudeEntry(tc.stop, tc.content), tc.fields))
			if tc.afterTurn != "" {
				f.add(tc.afterTurn)
			}
			f.add(continueReminderEntry())
			want := append([]conversation.Message{user("request"), claude(text("earlier continued turn"))}, tc.kept...)
			assertMessages(t, read(t, f.bytes()).Messages, want)
		})
	}
}

func TestReadKeepsAReminderAcrossToolResultsAndContextNotes(t *testing.T) {
	f := newFixture()
	f.add(userEntry("request"))
	kept := f.add(withMessageFields(claudeEntry("aborted", `[{"type":"text","text":"partial"},{"type":"toolCall","id":"call","name":"read","arguments":{}}]`), `,"continued":true`))
	f.add(toolResultEntry("call", "read", "plum", false))
	f.add(continueReminderEntry())
	f.add(`{"type":"compaction","summary":"Earlier context.","firstKeptEntryId":` + quote(kept) +
		`,"preserveData":{"anthropicCompaction":{"provider":"anthropic","content":"Summary"}}}`)
	f.add(`{"type":"custom","customType":"experimental_context_notes","data":{"version":1,"text":"Keep the note."}}`)
	messages := read(t, f.bytes()).Messages
	summary := render(compactionSummaryTemplate, "{{summary}}", "Earlier context.")
	if len(messages) != 5 || messages[3].Role != conversation.User ||
		!strings.Contains(messages[3].Content[0].Text, "\nLatest notebook revision:\nKeep the note.\n") {
		t.Fatalf("messages = %+v, want summary, continued turn, tool result, context notes, reminder", messages)
	}
	assertMessages(t, messages[:1], []conversation.Message{user(summary)})
	assertMessages(t, messages[1:3], []conversation.Message{claude(text("partial"), call("call", "read", `{}`)), result("call", "read", "plum", false)})
	assertMessages(t, messages[4:], []conversation.Message{user("Pick up where you stopped.")})
}

// context-notes.ts:22–43,54–63: count UTF-8 bytes and ignore an invalid revision
// without hiding the last valid one.
func TestReadLimitsContextNotesByUTF8Bytes(t *testing.T) {
	const previous = "Previous notes."
	atLimit := strings.Repeat("é", 8_192)
	for _, tc := range []struct{ name, latest, want string }{
		{"at 16384 bytes", atLimit, atLimit},
		{"over 16384 bytes", atLimit + "x", previous},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			f.add(userEntry("request"))
			f.add(claudeEntry("stop", `[{"type":"text","text":"answer"}]`))
			for _, note := range []string{previous, tc.latest} {
				f.add(`{"type":"custom","customType":"experimental_context_notes","data":{"version":1,"text":` + quote(note) + `}}`)
			}
			messages := read(t, f.bytes()).Messages
			if len(messages) != 3 || len(messages[0].Content) != 1 {
				t.Fatalf("want one context notes block, a request, and an answer, got %d messages", len(messages))
			}
			_, got, ok := strings.Cut(messages[0].Content[0].Text, "\nLatest notebook revision:\n")
			if !ok || got != tc.want+"\n</experimental-context-notes>" {
				t.Errorf("wrong notebook revision for %d-byte entry: want the %d-byte revision", len(tc.latest), len(tc.want))
			}
		})
	}
}

func TestReadDropsAReminderWhoseTurnCompactionCut(t *testing.T) {
	f := newFixture()
	f.add(userEntry("request"))
	f.add(withMessageFields(claudeEntry("error", `[{"type":"text","text":"partial"}]`), `,"continued":true`))
	kept := f.add(continueReminderEntry())
	f.add(`{"type":"compaction","summary":"Earlier context.","firstKeptEntryId":` + quote(kept) + `}`)
	s := read(t, f.bytes())
	if len(s.Messages) != 1 || !strings.Contains(s.Messages[0].Content[0].Text, "Earlier context.") {
		t.Errorf("messages = %+v, want just the compaction summary", s.Messages)
	}
}

func TestReadKeepsInterruptedThinkingContinuity(t *testing.T) {
	f := newFixture()
	f.add(userEntry("request"))
	f.add(claudeEntry("aborted", `[{"type":"text","text":"partial"},{"type":"thinking","thinking":"unfinished"},{"type":"text","text":" "}]`))
	f.add(`{"type":"custom_message","customType":"interrupted-thinking","content":"Continuity note."}`)
	s, err := Read(f.bytes(), Options{ReplayUnsignedThinking: true})
	if err != nil {
		t.Fatal(err)
	}
	assertMessages(t, s.Messages, []conversation.Message{
		user("request"), claude(text("partial")), user("Continuity note."),
	})
}

func TestReadDropsOnlyOrphanedSyntheticResults(t *testing.T) {
	f := newFixture()
	f.add(userEntry("request"))
	f.add(withMessageFields(toolResultEntry("orphan", "read", "did not run", true), `,"details":{"__synthetic":true,"executed":false}`))
	f.add(toolResultEntry("real", "read", "a real result", false))
	f.add(claudeEntry("toolUse", `[{"type":"toolCall","id":"paired","name":"read","arguments":{}}]`))
	f.add(withMessageFields(toolResultEntry("paired", "read", "did not run", true), `,"details":{"__synthetic":true,"executed":false}`))
	assertMessages(t, read(t, f.bytes()).Messages, []conversation.Message{
		user("request"), user("<stale-tool-result tool=\"read\" id=\"real\">\na real result\n</stale-tool-result>"),
		claude(call("paired", "read", `{}`)), result("paired", "read", "did not run", true),
	})
}

func TestReadSuppressesBranchSummaryDuplicatedByRewindReport(t *testing.T) {
	f := newFixture()
	f.add(userEntry("request"))
	f.add(`{"type":"branch_summary","summary":"Report in transcript."}`)
	f.add(`{"type":"custom_message","customType":"rewind-report","content":"Report for the model."}`)
	f.add(claudeEntry("stop", `[{"type":"text","text":"done"}]`))
	assertMessages(t, read(t, f.bytes()).Messages, []conversation.Message{
		user("request"), systemText("Report for the model."), claude(text("done")),
	})
}

func continueReminderEntry() string {
	return `{"type":"custom_message","customType":"continue-reminder","content":"Pick up where you stopped.","attribution":"agent","display":false}`
}

func withMessageFields(record, fields string) string {
	return strings.TrimSuffix(record, "}}") + fields + "}}"
}

// Tool call ids reach Anthropic as Oh My Pi's normalizeToolCallId leaves them: an OpenAI
// "call_…|fc_…" id with every character outside [a-zA-Z0-9_-] replaced, cut to 64; the Responses
// API's call_id is the id's part before the '|'. Each result follows its call, pulled forward past
// a message between them.
func TestReadCarriesToolCallIDsAsTheTargetRequestDoes(t *testing.T) {
	build := func(final string) []byte {
		f := newFixture()
		f.add(userEntry("request"))
		f.add(assistantEntry("openai", conversation.OpenAIResponses, "gpt-6.1", "toolUse", `[{"type":"toolCall","id":"call_A|fc_0123456789012345678901234567890123456789012345678901234567890123","name":"read","arguments":{}}]`))
		f.add(`{"type":"custom_message","customType":"mid-run-todo-nudge","content":"Keep going.","display":false}`)
		f.add(toolResultEntry("call_A|fc_0123456789012345678901234567890123456789012345678901234567890123", "read", "out", false))
		f.add(final)
		return f.bytes()
	}
	s := read(t, build(claudeEntry("stop", `[{"type":"text","text":"done"}]`)))
	anthropicID := ("call_A_fc_" + "0123456789012345678901234567890123456789012345678901234567890123")[:64]
	if got := s.Messages[1].Content[0].ToolCallID; got != anthropicID {
		t.Errorf("Anthropic tool call id = %q, want %q", got, anthropicID)
	}
	if got := s.Messages[2]; !reflect.DeepEqual(got, result(anthropicID, "read", "out", false)) {
		t.Errorf("message after the call = %+v, want the real result under %q", got, anthropicID)
	}
	if got := s.Messages[3]; !reflect.DeepEqual(got, systemText("Keep going.")) {
		t.Errorf("message after the result = %+v, want the custom message as a system turn", got)
	}

	s = read(t, build(assistantEntry("openai", conversation.OpenAIResponses, "gpt-6.1", "stop", `[{"type":"text","text":"done"}]`)))
	if s.Messages[1].Content[0].ToolCallID != "call_A" || s.Messages[2].Content[0].ToolCallID != "call_A" {
		t.Errorf("Responses ids = %q and %q, want call_A", s.Messages[1].Content[0].ToolCallID, s.Messages[2].Content[0].ToolCallID)
	}

	t.Run("Anthropic endpoint policy is independent of signing", func(t *testing.T) {
		for _, tc := range []struct {
			name, id, want string
			opts           Options
		}{
			{"official valid ID", "toolu_Abc-123", "toolu_Abc-123", Options{}},
			{"official opaque ID", "call:opaque", "call_opaque", Options{}},
			{"official unsigned policy", "call:opaque", "call_opaque", Options{ReplayUnsignedThinking: true}},
			{"nonofficial signing proxy", "call:opaque", "call:opaque", Options{NonOfficialAnthropicEndpoint: true}},
			{"nonofficial unsigned endpoint", "call:opaque", "call:opaque", Options{NonOfficialAnthropicEndpoint: true, ReplayUnsignedThinking: true}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f := newFixture()
				f.add(claudeEntry("toolUse", `[{"type":"toolCall","id":`+quote(tc.id)+`,"name":"read","arguments":{}}]`))
				f.add(toolResultEntry(tc.id, "read", "out", false))
				s, err := Read(f.bytes(), tc.opts)
				if err != nil {
					t.Fatal(err)
				}
				assertMessages(t, s.Messages, []conversation.Message{claude(call(tc.want, "read", `{}`)), result(tc.want, "read", "out", false)})
			})
		}
	})
}

// OpenAI Responses preserves native reasoning and tool items on the same model. A Claude target
// drops a foreign reasoning summary. An OpenAI target demotes foreign reasoning to text.
func TestReadCarriesOpenAIReasoningNativeAndForeignReasoningAsText(t *testing.T) {
	items := `[{"id":"rs_01","type":"reasoning","content":[],"encrypted_content":"gAAAAABq3krn+/=","summary":[{"type":"summary_text","text":"Weighing it."}]},` +
		`{"id":"fc_01","type":"function_call","status":"completed","arguments":"{\"path\":\"x\"}","call_id":"call_01","name":"read"}]`
	const payloadPrefix = `{"type":"openaiResponsesHistory","dt":true,"items":`
	payload := payloadPrefix + items + `}`
	openaiTurn := `{"type":"message","message":{"role":"assistant","content":[{"type":"thinking","thinking":"Weighing it.","thinkingSignature":"{\"type\":\"reasoning\",\"id\":\"rs_01\",\"encrypted_content\":\"gAAAAABq3krn+/=\"}","itemId":"rs_01","summary":true},{"type":"toolCall","id":"call_01|fc_01","name":"read","arguments":{"path":"x"}}],` +
		`"api":"openai-responses","provider":"openai","model":"gpt-6.1","stopReason":"toolUse","timestamp":2,"providerPayload":` + payload + `}}`

	f := newFixture()
	f.add(userEntry("request"))
	f.add(openaiTurn)
	f.add(toolResultEntry("call_01|fc_01", "read", "out", false))
	s := read(t, f.bytes())
	native := s.Messages[1].Native
	if native == nil || native.Kind != conversation.OpenAIResponsesHistory || native.Provider != "openai" || !native.Incremental || len(native.Items) != 2 {
		t.Fatalf("native = %+v, want the turn's two output items", native)
	}
	if !bytes.Equal(native.Payload, []byte(payload)) {
		t.Errorf("payload = %s, want byte for byte %s", native.Payload, payload)
	}
	var reasoning map[string]any
	if err := json.Unmarshal(native.Items[0], &reasoning); err != nil || reasoning["encrypted_content"] != "gAAAAABq3krn+/=" {
		t.Errorf("native reasoning = %s, error = %v", native.Items[0], err)
	}
	if got := s.Messages[1].Content[0]; got.Type != conversation.Thinking || got.Text != "Weighing it." {
		t.Errorf("thinking = %+v", got)
	}
	if s.API != conversation.OpenAIResponses || s.Model != "openai/gpt-6.1" {
		t.Errorf("API %q, model %q", s.API, s.Model)
	}

	f.add(`{"type":"model_change","model":"anthropic/claude-opus-5-5"}`)
	f.add(claudeEntry("stop", `[{"type":"thinking","thinking":"Claude thinks.","thinkingSignature":"c2ln"},{"type":"redactedThinking","data":"cmVk"},{"type":"text","text":"Claude answers."}]`))
	s = read(t, f.bytes())
	assertMessages(t, s.Messages[1:2], []conversation.Message{{
		Role: conversation.Assistant, Provider: "openai", API: conversation.OpenAIResponses, Model: "gpt-6.1",
		Content: []conversation.Block{call("call_01_fc_01", "read", `{"path":"x"}`)},
	}})

	f.add(`{"type":"model_change","model":"openai/gpt-6.1"}`)
	s = read(t, f.bytes())
	assertMessages(t, s.Messages[3:4], []conversation.Message{claude(text("<think>\nClaude thinks.\n</think>"), text("Claude answers."))})
	if s.Messages[1].Native == nil {
		t.Error("back on gpt-6.1 the OpenAI turn carries no native items")
	}
}

// Inline images reach the request as their media type and data.
func TestReadCarriesImages(t *testing.T) {
	f := newFixture()
	f.add(`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"look"},{"type":"image","data":"` + attachmentFixturePNG + `","mimeType":"image/png"}],"timestamp":1}}`)
	f.add(claudeEntry("stop", `[{"type":"text","text":"seen"}]`))
	s := read(t, f.bytes())
	if got := s.Messages[0].Content; len(got) != 2 || !reflect.DeepEqual(got[1], conversation.Block{Type: conversation.Image, MediaType: "image/png", ImageData: attachmentFixturePNG}) {
		t.Errorf("user content = %+v", got)
	}
}

// A record that is not JSON, as a torn last record is, is skipped as Oh My Pi skips it, and the
// session is marked torn when its content does not end in a newline.
func TestReadSkipsATornRecord(t *testing.T) {
	f := newFixture()
	f.add(userEntry("request"))
	f.add(claudeEntry("stop", `[{"type":"text","text":"answer"}]`))
	content := append(f.bytes(), []byte(`{"type":"message","id":"torn","parentId":"`+f.last+`","message":{"role":"user","content":[{"type":"te`)...)
	s := read(t, content)
	assertMessages(t, s.Messages, []conversation.Message{user("request"), claude(text("answer"))})
	if !s.Torn || s.Leaf != f.last {
		t.Errorf("torn %t, leaf %q; want torn, leaf %q", s.Torn, s.Leaf, f.last)
	}
}

func TestReadSupportedTargets(t *testing.T) {
	for _, tc := range []struct {
		provider, api, model string
		refused, mixed       bool
	}{
		{"openai", conversation.OpenAIResponses, "gpt-oss-120b", true, false},
		{"openai", conversation.OpenAIResponses, "gpt-alias/gpt-oss-120b", true, false},
		{"openai", conversation.OpenAIResponses, "gpt-alias/kimi-k3.1-pro", true, false},
		{"nanogpt", conversation.AnthropicMessages, "claude-opus-5-5", true, false},
		{"muse-code", conversation.OpenAIResponses, "gpt-6.2-sol", true, false},
		{"model-gateway", "openai-completions", "kimi-k3.1-pro", true, false},
		{"anthropic", conversation.AnthropicMessages, "gpt-6.2-sol", true, false},
		{"anthropic", conversation.AnthropicMessages, "claude-alias/kimi-k3.1-pro", true, false},
		{"anthropic", conversation.AnthropicMessages, "claude-opus-5-5", false, false},
		{"openai", conversation.OpenAIResponses, "gpt-6.2-sol", false, true},
	} {
		t.Run(tc.provider+"/"+tc.model, func(t *testing.T) {
			f := newFixture()
			f.add(userEntry("request"))
			want := []conversation.Message{user("request")}
			if tc.mixed {
				const provider, api, model = "model-gateway", "google-generative-ai", "gemini-4-neon"
				f.add(assistantEntry(provider, api, model, "stop", `[{"type":"thinking","thinking":"Earlier reasoning."},{"type":"text","text":"Earlier answer."}]`))
				f.add(userEntry("follow-up"))
				want = append(want, conversation.Message{Role: conversation.Assistant, Provider: provider, API: api, Model: model,
					Content: []conversation.Block{text("<think>\nEarlier reasoning.\n</think>"), text("Earlier answer.")}}, user("follow-up"))
			}
			f.add(assistantEntry(tc.provider, tc.api, tc.model, "stop", `[{"type":"text","text":"answer"}]`))
			s, err := Read(f.bytes(), Options{})
			if tc.refused {
				if !errors.Is(err, conversation.ErrUnknownFormat) {
					t.Fatalf("Read = %v, want ErrUnknownFormat", err)
				}
				for _, field := range []string{tc.provider, tc.api, tc.model} {
					if !strings.Contains(err.Error(), quote(field)) {
						t.Errorf("error %v does not name %q", err, field)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if s.Model != tc.provider+"/"+tc.model || s.API != tc.api {
				t.Errorf("model %q, API %q; want %s/%s, %s", s.Model, s.API, tc.provider, tc.model, tc.api)
			}
			want = append(want, conversation.Message{Role: conversation.Assistant, Provider: tc.provider, API: tc.api, Model: tc.model,
				Content: []conversation.Block{text("answer")}})
			assertMessages(t, s.Messages, want)
		})
	}
}

// A well-formed record the pinned Oh My Pi does not write refuses the session: an entry type, a
// message role, a content block type, a header version, a provider payload type; so does content a
// reader cannot rebuild as Oh My Pi would.
func TestReadRefusesWhatItDoesNotKnow(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		content string
	}{
		{"an entry type", fixtureHeader + "\n" + `{"type":"bookmark","id":"a","parentId":null}` + "\n"},
		{"a header version", `{"type":"session","version":4,"id":"0001","cwd":"/work"}` + "\n"},
		{"an older header version", `{"type":"session","version":2,"id":"0001","cwd":"/work"}` + "\n"},
		{"no header", `{"type":"message","id":"a","parentId":null,"message":{"role":"user","content":"hi"}}` + "\n"},
		{"a message role", fixtureHeader + "\n" + `{"type":"message","id":"a","parentId":null,"message":{"role":"narrator","content":"hi"}}` + "\n"},
		{"a content block type", fixtureHeader + "\n" + `{"type":"message","id":"a","parentId":null,"message":{"role":"user","content":[{"type":"audio","data":"x"}]}}` + "\n"},
		{"an assistant block type", fixtureHeader + "\n" + `{"type":"message","id":"a","parentId":null,"message":{"role":"assistant","api":"anthropic-messages","provider":"anthropic","model":"claude-x","stopReason":"stop","content":[{"type":"audio","data":"x"}]}}` + "\n"},
		{"a server tool without its native block", fixtureHeader + "\n" + `{"type":"message","id":"a","parentId":null,"message":{"role":"assistant","api":"anthropic-messages","provider":"anthropic","model":"claude-x","stopReason":"stop","content":[{"type":"anthropicServerTool"}]}}` + "\n"},
		{"a provider payload type", fixtureHeader + "\n" + `{"type":"message","id":"a","parentId":null,"message":{"role":"assistant","api":"anthropic-messages","provider":"anthropic","model":"claude-x","stopReason":"stop","content":[{"type":"text","text":"hi"}],"providerPayload":{"type":"geminiHistory"}}}` + "\n"},
		{"a provider API", fixtureHeader + "\n" + `{"type":"message","id":"a","parentId":null,"message":{"role":"assistant","api":"google-generative-ai","provider":"google","model":"gemini-3","stopReason":"stop","content":[{"type":"text","text":"hi"}]}}` + "\n"},
		{"a mistyped field", fixtureHeader + "\n" + `{"type":"compaction","id":"a","parentId":null,"summary":7,"firstKeptEntryId":"x"}` + "\n"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := Read([]byte(testCase.content), Options{})
			if !errors.Is(err, conversation.ErrUnknownFormat) {
				t.Fatalf("Read = %v, want ErrUnknownFormat", err)
			}
			var formatError *conversation.FormatError
			if !errors.As(err, &formatError) || formatError.What == "" {
				t.Errorf("Read = %v, want a FormatError naming what it did not know", err)
			}
		})
	}
	for _, tc := range []struct{ name, entry, what string }{
		{"a snapcompact archive", `{"type":"compaction","summary":"s","firstKeptEntryId":"","preserveData":{"snapcompact":{}}}`, "the newest compaction is a snapcompact archive, which the reader does not rebuild (dispatch://INFRA-6438)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			f.add(claudeEntry("stop", `[{"type":"text","text":"supported model"}]`))
			f.add(tc.entry)
			_, err := Read(f.bytes(), Options{})
			var formatError *conversation.FormatError
			if !errors.As(err, &formatError) || formatError.What != tc.what {
				t.Fatalf("Read = %v, want FormatError.What = %q", err, tc.what)
			}
		})
	}
}

// Custom messages reach the model as Oh My Pi converts them: an extension's message as a user
// turn, its images in a user turn of their own; a prewalk plan not at all; a collaborator's prompt
// in the user-interjection envelope. A custom entry, such as the answer marker an append writes,
// never reaches it, nor does a tool result a prune record blanked keep its output.
func TestReadConvertsCustomMessagesAndPrunes(t *testing.T) {
	f := newFixture()
	f.add(userEntry("request"))
	f.add(`{"type":"custom_message","customType":"envoy-message","content":"A message arrived.","display":true}`)
	f.add(`{"type":"custom_message","customType":"prewalk-plan","content":"plan","display":false}`)
	f.add(`{"type":"custom_message","customType":"screenshot","content":[{"type":"text","text":"Screen:"},{"type":"image","data":"` + attachmentFixturePNG + `","mimeType":"image/png"}],"display":true}`)
	f.add(`{"type":"custom_message","customType":"collab-prompt","content":"Stop   \n\n\n\nnow","display":true,"attribution":"user"}`)
	f.add(claudeEntry("toolUse", `[{"type":"toolCall","id":"toolu_01","name":"xd://codegraph","arguments":{}}]`))
	result := f.add(`{"type":"message","message":{"role":"toolResult","toolCallId":"toolu_01","toolName":"xd://codegraph","content":[{"type":"text","text":"a long output"},{"type":"text","text":" continued"}],"isError":false,"timestamp":3}}`)
	f.add(`{"type":"custom","customType":"tool_result_prune","data":{"results":[{"entryId":"` + result + `","notice":"[Uneventful result elided]","prunedAt":5}]}}`)
	f.add(claudeEntry("stop", `[{"type":"text","text":"done"}]`))
	f.add(`{"type":"custom","customType":"legion-answer","data":{"questionId":"q1"}}`)
	ctx := contextBeforeRequest(t, f)
	for _, m := range ctx.messages {
		if m.role == "toolResult" && m.toolCallID == "toolu_01" {
			if m.rewriteAt == nil || *m.rewriteAt != 5 {
				t.Errorf("the prune's rewrite timestamp was not carried: %v, want 5", m.rewriteAt)
			}
		}
	}
	s := read(t, f.bytes())
	assertMessages(t, s.Messages, []conversation.Message{
		user("request"),
		user("A message arrived."),
		user("Screen:"),
		{Role: conversation.User, Content: []conversation.Block{text("Images attached to screenshot."), {Type: conversation.Image, MediaType: "image/png", ImageData: attachmentFixturePNG}}},
		user("<system-notice>\nUser interjection during work: priority; supersedes conflicting prior instructions. Re-read; ensure current work reflects user intent.\n</system-notice>\nStop\nnow"),
		claude(call("toolu_01", "codegraph", `{}`)),
		{Role: conversation.Tool, Content: []conversation.Block{{Type: conversation.ToolResult, ToolCallID: "toolu_01", ToolName: "codegraph", Content: []conversation.Block{text("[Uneventful result elided]")}}}},
		claude(text("done")),
	})
}

// Oh My Pi's post-render format: trailing white space trimmed, a run of blank lines removed whole,
// one blank line kept between text, table rows compacted outside code fences, blank lines dropped
// before a closing tag and at either end.
func TestFormatPromptIsOhMyPisPostRenderFormat(t *testing.T) {
	for _, testCase := range []struct{ in, want string }{
		{"a  \nb\t\n", "a\nb"},
		{"a\n\nb", "a\n\nb"},
		{"a\n\n\nb", "a\nb"},
		{"\n\na\n", "a"},
		{"<summary>\nx\n\n</summary>\n", "<summary>\nx\n</summary>"},
		{"| a |  b |\n|:--|--:|", "|a|b|\n|:---|---:|"},
		{"```\n| a |\n\n\n```", "```\n| a |\n\n\n```"},
	} {
		if got := formatPrompt(testCase.in); got != testCase.want {
			t.Errorf("formatPrompt(%q) = %q, want %q", testCase.in, got, testCase.want)
		}
	}
}
