package omp

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/conversation"
)

func TestReadRestoresBranchSettings(t *testing.T) {
	for _, tc := range []struct{ name, role, wantModel string }{
		{"last named role", `"small"`, "anthropic/claude-small"},
		{"retry fallback", `"fallback"`, "anthropic/claude-opus-5-5"},
		{"implicit default role", `null`, "anthropic/claude-small"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			f.add(`{"type":"model_change","model":"anthropic/claude-opus-5-5"}`)
			f.add(claudeEntry("stop", `[{"type":"text","text":"default"}]`))
			f.add(`{"type":"model_change","model":"anthropic/claude-small","role":` + tc.role + `}`)
			f.add(assistantEntry("anthropic", conversation.AnthropicMessages, "claude-small", "stop", `[{"type":"text","text":"small"}]`))
			f.add(assistantEntry("other", conversation.OpenAIResponses, "temporary", "stop", `[{"type":"text","text":"temporary"}]`))
			if tc.role == "null" {
				f.add(`{"type":"model_change","model":"other/temporary","role":"fallback"}`)
			}
			f.add(`{"type":"thinking_level_change","thinkingLevel":"high"}`)
			f.add(`{"type":"reset_boundary"}`)
			f.add(`{"type":"thinking_level_change","thinkingLevel":null,"configured":"auto"}`)
			leaf := f.add(userEntry("after reset"))
			f.addAfter(leaf, `{"type":"thinking_level_change","thinkingLevel":"low","activeLeafId":`+quote(leaf)+`}`)
			s := read(t, f.bytes())
			if s.Model != tc.wantModel || s.API != conversation.AnthropicMessages || s.ThinkingLevel != "off" {
				t.Fatalf("model %q, API %q, thinking %q; want %q, Anthropic Messages, off", s.Model, s.API, s.ThinkingLevel, tc.wantModel)
			}
			assertMessages(t, s.Messages, []conversation.Message{user("after reset")})
		})
	}
}

func TestReadReplaysRemoteTailAfterReplayThroughEntry(t *testing.T) {
	for _, boundary := range []string{"request", "missing"} {
		t.Run(boundary, func(t *testing.T) {
			f := newFixture()
			request := f.add(userEntry("covered"))
			answer := f.add(assistantEntry("openai", conversation.OpenAIResponses, "gpt-test", "stop", `[{"type":"text","text":"tail"}]`))
			through := boundary
			if boundary == "request" {
				through = request
			}
			f.add(`{"type":"compaction","summary":"Placeholder.","firstKeptEntryId":` + quote(answer) +
				`,"providerReplayThroughEntryId":` + quote(through) +
				`,"preserveData":{"openaiRemoteCompaction":{"provider":"openai","replacementHistory":[{"type":"compaction","encrypted_content":"encoded"}]}}}`)
			s := read(t, f.bytes())
			wantLen := 1
			if boundary == "request" {
				wantLen++
			}
			if len(s.Messages) != wantLen || s.Messages[0].Native == nil ||
				wantLen == 2 && s.Messages[1].Content[0].Text != "tail" {
				t.Fatalf("messages = %+v, want a native summary and %d retained turns", s.Messages, wantLen-1)
			}
		})
	}
}

func TestReadConvertsContinuedUserInterrupts(t *testing.T) {
	const unfinished = `{"type":"thinking","thinking":"unfinished"}`
	for _, tc := range []struct {
		name, fields, content string
		wantTurns             int
		wantText              string
	}{
		{"structured flag", `,"errorId":67108864`, `[{"type":"text","text":"partial"},` + unfinished + `]`, 2, "partial"},
		{"legacy message", `,"errorMessage":"Interrupted by user"`, `[{"type":"text","text":"partial"},` + unfinished + `]`, 2, "partial"},
		{"empty after stripping", `,"errorId":67108864`, `[` + unfinished + `]`, 1, ""},
		{"recorded controls preserve the turn", `,"errorId":67108864,"requestControls":{"messageIndex":1}`, `[` + unfinished + `]`, 2, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			f.add(userEntry("request"))
			f.add(withMessageFields(claudeEntry("aborted", tc.content), `,"continued":true`+tc.fields))
			ctx := contextBeforeRequest(t, f)
			if len(ctx.messages) != tc.wantTurns {
				t.Fatalf("context has %d messages, want %d", len(ctx.messages), tc.wantTurns)
			}
			if tc.wantTurns == 2 {
				blocks := ctx.messages[1].blocks
				if tc.wantText == "" && len(blocks) != 0 ||
					tc.wantText != "" && (len(blocks) != 1 || blocks[0].Type != "text" || blocks[0].Text != tc.wantText) {
					t.Fatalf("interrupted assistant blocks = %+v, want only %q (or no blocks for empty text)", blocks, tc.wantText)
				}
			}
		})
	}
}

func TestReadNotebookRevisionVisibility(t *testing.T) {
	for _, tc := range []struct{ name, next, want string }{
		{"new revision", `{"type":"custom","customType":"experimental_context_notes","data":{"version":1,"text":"newer"}}`, "newer"},
		{"wrong version", `{"type":"custom","customType":"experimental_context_notes","data":{"version":2,"text":"newer"}}`, "original"},
		{"extra field", `{"type":"custom","customType":"experimental_context_notes","data":{"version":1,"text":"newer","extra":true}}`, "original"},
		{"explicitly empty", `{"type":"custom","customType":"experimental_context_notes","data":{"version":1,"text":""}}`, ""},
		{"reset", `{"type":"reset_boundary"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			f.add(claudeEntry("stop", `[{"type":"text","text":"old answer"}]`))
			f.add(`{"type":"custom","customType":"experimental_context_notes","data":{"version":1,"text":"original"}}`)
			f.add(tc.next)
			f.add(userEntry("current request"))
			var gotNotes []string
			for _, m := range read(t, f.bytes()).Messages {
				for _, b := range m.Content {
					if strings.HasPrefix(b.Text, "<experimental-context-notes>") {
						gotNotes = append(gotNotes, b.Text)
					}
				}
			}
			if tc.want != "" {
				if len(gotNotes) != 1 || !strings.Contains(gotNotes[0], "\nLatest notebook revision:\n"+tc.want+"\n") {
					t.Fatalf("notes = %q, want one revision with %q", gotNotes, tc.want)
				}
			} else if len(gotNotes) != 0 {
				t.Fatalf("notes = %q, want no revision", gotNotes)
			}
		})
	}
}

func TestReadSkipsNonContextEntries(t *testing.T) {
	f := newFixture()
	f.add(userEntry("request"))
	f.add(`{"type":"branch_summary","summary":""}`)
	f.add(`{"type":"custom_message","customType":"vibe-mode-context","content":"vibe"}`)
	f.add(`{"type":"custom_message","customType":"another","content":{"type":"text","text":"not an array"}}`)
	for _, role := range []string{"bashExecution", "pythonExecution"} {
		f.add(`{"type":"message","message":{"role":` + quote(role) + `,"excludeFromContext":true}}`)
	}
	f.add(claudeEntry("stop", `[{"type":"text","text":"answer"}]`))
	assertMessages(t, read(t, f.bytes()).Messages, []conversation.Message{user("request"), claude(text("answer"))})
}

func TestContextDropsOrphanedSyntheticResult(t *testing.T) {
	messages := []llmMessage{
		{role: "assistant", blocks: []block{{Type: "toolCall", ID: "paired"}}},
		{role: "toolResult", toolCallID: "paired", synthetic: true},
		{role: "toolResult", toolCallID: "orphan", synthetic: true},
		{role: "toolResult", toolCallID: "real"},
	}
	got := dropOrphanedSyntheticResults(messages)
	if len(got) != 3 || got[1].toolCallID != "paired" || got[2].toolCallID != "real" {
		t.Fatalf("context = %+v, want paired synthetic and real results only", got)
	}
}

func TestContextDropsRedactedThinkingWhenStrippingADanglingCall(t *testing.T) {
	messages := []llmMessage{{role: "assistant", blocks: []block{
		{Type: "redactedThinking", Data: "cmVk"},
		{Type: "text", Text: "visible"},
		{Type: "toolCall", ID: "dangling"},
	}}}
	got := stripDanglingToolCalls(messages)
	if len(got) != 1 || len(got[0].blocks) != 1 || got[0].blocks[0].Type != "text" {
		t.Fatalf("context = %+v, want the visible text without the protected block or tool call", got)
	}
}

func TestContextPlacesNotesAfterAllRetainedToolResults(t *testing.T) {
	f := newFixture()
	f.add(userEntry("request"))
	kept := f.add(claudeEntry("toolUse", `[{"type":"toolCall","id":"a","name":"read","arguments":{}},{"type":"toolCall","id":"b","name":"read","arguments":{}}]`))
	f.add(toolResultEntry("a", "read", "plum", false))
	f.add(toolResultEntry("b", "read", "green", false))
	f.add(`{"type":"compaction","summary":"Earlier context.","firstKeptEntryId":` + quote(kept) +
		`,"preserveData":{"anthropicCompaction":{"provider":"anthropic","content":"Summary"}}}`)
	f.add(`{"type":"custom","customType":"experimental_context_notes","data":{"version":1,"text":"Keep the note."}}`)
	got := contextBeforeRequest(t, f).messages
	if len(got) != 5 || got[2].toolCallID != "a" || got[3].toolCallID != "b" || got[4].customType != contextNotesType {
		t.Fatalf("context = %+v, want summary, assistant, both tool results, then notes", got)
	}
}

func contextBeforeRequest(t *testing.T, f *fixture) *sessionContext {
	t.Helper()
	parsed, err := parseSession(f.bytes())
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := buildContext(parsed.entries)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func TestReadReplaysUserSteeringAndKeepsSkillImagesTogether(t *testing.T) {
	f := newFixture()
	f.add(`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"Please stop."},{"type":"image","data":"iVBO","mimeType":"image/png"}],"steering":true}}`)
	f.add(`{"type":"custom_message","customType":"skill-prompt","attribution":"user","content":[{"type":"text","text":"Read this."},{"type":"image","data":"iVBO","mimeType":"image/png"}]}`)
	f.add(`{"type":"custom_message","content":[{"type":"image","data":"iVBO","mimeType":"image/png"}]}`)
	f.add(claudeEntry("stop", `[{"type":"text","text":"answer"}]`))
	image := conversation.Block{Type: conversation.Image, MediaType: "image/png", ImageData: "iVBO"}
	assertMessages(t, read(t, f.bytes()).Messages, []conversation.Message{
		{Role: conversation.User, Content: []conversation.Block{text("<system-notice>\nUser interjection during work: priority; supersedes conflicting prior instructions. Re-read; ensure current work reflects user intent.\n</system-notice>\nPlease stop."), image}},
		{Role: conversation.User, Content: []conversation.Block{text("Read this."), image}},
		{Role: conversation.User, Content: []conversation.Block{text("Images attached to custom."), image}},
		claude(text("answer")),
	})
}

func TestPrunedResultInsertsTruncationNoticeAndKeepsImages(t *testing.T) {
	image := block{Type: "image", MimeType: "image/png", Data: "iVBO"}
	for _, tc := range []struct {
		name      string
		in        []block
		wantFirst string
	}{
		{"no text block", []block{image}, "text"},
		{"empty text block", []block{image, {Type: "text"}}, "image"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := prunedContent(tc.in)
			if len(got) != 2 || got[0].Type != tc.wantFirst ||
				got[0].Text+got[1].Text != "[Output truncated]" || got[0].Data+got[1].Data != image.Data {
				t.Fatalf("pruned content = %+v, want truncation notice and image (first type %q)", got, tc.wantFirst)
			}
		})
	}
}

func TestReadAppliesRetryRecoveryPatch(t *testing.T) {
	f := newFixture()
	f.add(userEntry("request"))
	id := f.add(claudeEntry("stop", `[{"type":"text","text":"superseded"}]`))
	f.add(entryPatchRecord(`{"entryId":` + quote(id) + `,"message":{"retryRecovery":{"kind":"auto-retry","status":"recovered"}}}`))
	f.add(claudeEntry("stop", `[{"type":"text","text":"answer"}]`))
	assertMessages(t, read(t, f.bytes()).Messages, []conversation.Message{user("request"), claude(text("answer"))})
}

func TestReadValidatesPruneRecordsAndAppliesPatchesAfterThem(t *testing.T) {
	for _, tc := range []struct {
		name, tail, want string
	}{
		{"prune before target", "", "pruned"},
		{"malformed record is atomic", `{"type":"custom","customType":"tool_result_prune","data":{"results":[{"entryId":"e0000004","notice":"wrong","prunedAt":5},{"entryId":"missing","notice":"not valid without prunedAt"}]}}`, "pruned"},
		{"patch wins after prune", entryPatchRecord(`{"entryId":"e0000004","message":{"content":[{"type":"text","text":"patched"}]}}`), "patched"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			f.add(`{"type":"custom","customType":"tool_result_prune","data":{"results":[{"entryId":"e0000004","notice":"pruned","prunedAt":5}]}}`)
			f.add(userEntry("request"))
			f.add(claudeEntry("toolUse", `[{"type":"toolCall","id":"call","name":"read","arguments":{}}]`))
			f.add(toolResultEntry("call", "read", "original", false))
			if tc.tail != "" {
				f.add(tc.tail)
			}
			assertMessages(t, read(t, f.bytes()).Messages, []conversation.Message{
				user("request"), claude(call("call", "read", "{}")), result("call", "read", tc.want, false),
			})
		})
	}
}
