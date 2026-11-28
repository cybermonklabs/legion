package omp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/conversation"
)

func TestReadCarriesResponsesBlockToolMetadata(t *testing.T) {
	const callMetadata = `{"type":"computer","providerItemId":"cu_desktop","actions":[{"type":"screenshot"}],"pendingSafetyChecks":[{"id":"check_1","code":"example","message":"Check the screen."}]}`
	const resultMetadata = `{"type":"computer","screenshot":{"type":"computer_screenshot","image_url":"data:image/png;base64,eA=="},"acknowledgedSafetyChecks":[{"id":"check_1","code":"example","message":"Check the screen."}]}`
	f := newFixture()
	f.add(userEntry("Inspect the workspace."))
	f.add(assistantEntry("openai", conversation.OpenAIResponses, "gpt-6.1", "toolUse", `[`+
		`{"type":"toolCall","id":"call_patch|ctc_patch","name":"patch","arguments":{"input":"freeform patch"},"customWireName":"apply_patch","namespace":"workspace"},`+
		`{"type":"toolCall","id":"call_desktop|cu_desktop","name":"computer","arguments":{},"providerMetadata":`+callMetadata+`}]`))
	f.add(toolResultEntry("call_patch|ctc_patch", "patch", "done", false))
	f.add(withMessageFields(toolResultEntry("call_desktop|cu_desktop", "computer", "captured", false), `,"providerMetadata":`+resultMetadata))

	messages := read(t, f.bytes()).Messages
	if len(messages) != 4 || messages[1].Native != nil || len(messages[1].Content) != 2 || len(messages[3].Content) != 1 {
		t.Fatalf("history = %+v, want two block calls and their two results", messages)
	}
	patch, computer, result := messages[1].Content[0], messages[1].Content[1], messages[3].Content[0]
	t.Run("custom wire name", func(t *testing.T) {
		if patch.CustomWireName != "apply_patch" {
			t.Fatalf("custom wire name = %q, want apply_patch", patch.CustomWireName)
		}
	})
	t.Run("namespace", func(t *testing.T) {
		if patch.Namespace != "workspace" {
			t.Fatalf("namespace = %q, want workspace", patch.Namespace)
		}
	})
	t.Run("computer call metadata", func(t *testing.T) {
		if computer.Type != conversation.ToolCall || string(computer.ProviderMetadata) != callMetadata {
			t.Fatalf("computer call metadata = %s, want %s", computer.ProviderMetadata, callMetadata)
		}
	})
	t.Run("computer result metadata", func(t *testing.T) {
		if result.Type != conversation.ToolResult || result.ToolCallID != computer.ToolCallID || string(result.ProviderMetadata) != resultMetadata {
			t.Fatalf("computer result = %+v, want paired result with metadata %s", result, resultMetadata)
		}
	})
}

func TestReadPlacesAnthropicCompactionFileNotes(t *testing.T) {
	commit := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC).UnixMilli()
	const callID = "call_read"
	f := newFixture()
	f.add(userEntry("Earlier request"))
	kept := f.add(strings.Replace(claudeEntry("toolUse", `[{"type":"toolCall","id":"call_read","name":"read","arguments":{}}]`), `"timestamp":2`, fmt.Sprintf(`"timestamp":%d`, commit-2), 1))
	f.add(strings.Replace(toolResultEntry(callID, "read", "plum", false), `"timestamp":3`, fmt.Sprintf(`"timestamp":%d`, commit+1), 1))
	token := "ghp_" + strings.Repeat("aB", 18)
	payload := fmt.Sprintf(`{"provider":"anthropic","content":"Native summary.","signature":"sig","exactTail":true,"filesText":%q,"retainedFiles":[{"text":"Earlier note two.","after":%d},{"text":"Earlier note one.","after":%d},{"text":"   ","after":%d},{"text":"missing time"},{"text":"Final file note.","after":%d}]}`,
		"Read "+token+".", commit-3, commit-4, commit-5, commit+9)
	f.add(`{"type":"compaction","summary":"Earlier work.","firstKeptEntryId":` + quote(kept) + `,"preserveData":{"anthropicCompaction":` + payload + `}}`)
	f.add(strings.Replace(userEntry("At the commit time."), `"timestamp":1`, fmt.Sprintf(`"timestamp":%d`, commit), 1))
	f.add(strings.Replace(userEntry("After the commit time."), `"timestamp":1`, fmt.Sprintf(`"timestamp":%d`, commit+2), 1))
	native := conversation.Message{Role: conversation.Assistant, Native: &conversation.Native{
		Kind: conversation.AnthropicCompaction, Provider: "anthropic", Payload: json.RawMessage(payload),
	}}
	t.Run("resume", func(t *testing.T) {
		s, err := Read(f.bytes(), Options{ReplayAnthropicCompaction: true})
		if err != nil {
			t.Fatal(err)
		}
		assertMessages(t, s.Messages, []conversation.Message{
			native, user("Earlier note one."), user("Earlier note two."), claude(call(callID, "read", `{}`)),
			result(callID, "read", "plum", false), user("At the commit time."), user("Read " + token + "."),
			user("After the commit time."), user("Final file note."),
		})
	})
}

func TestReadFlushesAnthropicFileNotesBeforeAStaleResultNote(t *testing.T) {
	f := newFixture()
	f.add(claudeEntry("stop", `[{"type":"text","text":"Summarized."}]`))
	payload := `{"provider":"anthropic","content":"Native summary.","signature":"sig","exactTail":true,"filesText":"File note."}`
	f.add(`{"type":"compaction","summary":"Readable.","firstKeptEntryId":"","preserveData":{"anthropicCompaction":` + payload + `}}`)
	f.add(strings.Replace(toolResultEntry("orphan", "read", "orphan output", false), `"timestamp":3`, `"timestamp":1792972800001`, 1))
	f.add(userEntry("Next request."))
	s, err := Read(f.bytes(), Options{ReplayAnthropicCompaction: true})
	if err != nil {
		t.Fatal(err)
	}
	assertMessages(t, s.Messages, []conversation.Message{
		{Role: conversation.Assistant, Native: &conversation.Native{Kind: conversation.AnthropicCompaction, Provider: "anthropic", Payload: json.RawMessage(payload)}},
		user("File note."),
		user("<stale-tool-result tool=\"read\" id=\"orphan\">\norphan output\n</stale-tool-result>"),
		user("Next request."),
	})
}

func TestReadReplaysLegacyAnthropicCompaction(t *testing.T) {
	for _, tc := range []struct {
		name, provider, state string
		native                bool
	}{
		{name: "legacy enabled", provider: "anthropic", state: `,"encryptedContent":"ciphertext"`, native: true},
		{name: "different provider", provider: "other", state: `,"signature":"signed"`, native: false},
		{name: "empty signature", provider: "anthropic", state: `,"signature":""`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			f.add(userEntry("Earlier request"))
			kept := f.add(claudeEntry("toolUse", `[{"type":"toolCall","id":"first","name":"read","arguments":{}},{"type":"toolCall","id":"second","name":"read","arguments":{}}]`))
			f.add(toolResultEntry("first", "read", "one", false))
			f.add(toolResultEntry("second", "read", "two", false))
			payload := `{"provider":` + quote(tc.provider) + `,"content":"Native summary.","filesText":"File note"` + tc.state + `}`
			f.add(`{"type":"compaction","summary":"Readable.","firstKeptEntryId":` + quote(kept) + `,"preserveData":{"anthropicCompaction":` + payload + `}}`)
			f.add(claudeEntry("stop", `[{"type":"text","text":"Done."}]`))
			s, err := Read(f.bytes(), Options{ReplayAnthropicCompaction: true})
			if err != nil {
				t.Fatal(err)
			}
			calls := claude(call("first", "read", `{}`), call("second", "read", `{}`))
			results := []conversation.Message{result("first", "read", "one", false), result("second", "read", "two", false)}
			var want []conversation.Message
			if tc.native {
				calls.Native = &conversation.Native{Kind: conversation.AnthropicCompaction, Provider: tc.provider, Payload: json.RawMessage(payload)}
				want = append([]conversation.Message{calls}, results...)
				want = append(want, user("File note"))
			} else {
				summary := render(compactionSummaryTemplate, "{{summary}}", "Readable.")
				want = append([]conversation.Message{user(summary), calls}, results...)
			}
			assertMessages(t, s.Messages, append(want, claude(text("Done."))))
		})
	}
}
