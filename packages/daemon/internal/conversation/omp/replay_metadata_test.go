package omp

import (
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

func TestReadCarriesAnthropicCompactionFileNoteTimes(t *testing.T) {
	// The fixture entry time is midnight. The inner message times are distinct
	// and do not monotonically increase across the compaction summary.
	commit := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC).UnixMilli()
	f := newFixture()
	f.add(userEntry("Earlier request"))
	kept := f.add(strings.Replace(claudeEntry("toolUse", `[{"type":"toolCall","id":"call_read","name":"read","arguments":{}}]`), `"timestamp":2`, fmt.Sprintf(`"timestamp":%d`, commit-2), 1))
	f.add(strings.Replace(toolResultEntry("call_read", "read", "plum", false), `"timestamp":3`, fmt.Sprintf(`"timestamp":%d`, commit+1), 1))
	payload := fmt.Sprintf(`{"provider":"anthropic","content":"Native summary.","exactTail":true,"filesText":"Read the note.","retainedFiles":[{"text":"Read an earlier note.","after":%d}]}`, commit-3)
	f.add(`{"type":"compaction","summary":"Earlier work.","firstKeptEntryId":` + quote(kept) + `,"preserveData":{"anthropicCompaction":` + payload + `}}`)
	f.add(strings.Replace(userEntry("At the commit time."), `"timestamp":1`, fmt.Sprintf(`"timestamp":%d`, commit), 1))
	f.add(strings.Replace(userEntry("After the commit time."), `"timestamp":1`, fmt.Sprintf(`"timestamp":%d`, commit+2), 1))
	messages := read(t, f.bytes()).Messages
	if len(messages) != 5 || messages[0].Native == nil || messages[2].Role != conversation.Tool {
		t.Fatalf("history = %+v, want a summary, a retained call and result, then two prompts", messages)
	}
	if got := string(messages[0].Native.Payload); got != payload {
		t.Fatalf("native payload = %s, want %s", got, payload)
	}
	t.Run("compaction commit", func(t *testing.T) {
		if stamp := messages[0].Native.Timestamp; stamp == nil || *stamp != float64(commit) {
			t.Fatalf("native timestamp = %v, want compaction commit %d", stamp, commit)
		}
	})
	t.Run("saved turns", func(t *testing.T) {
		for i, want := range map[int]int64{1: commit - 2, 3: commit, 4: commit + 2} {
			if stamp := messages[i].Content[0].Timestamp; stamp == nil || *stamp != float64(want) {
				t.Fatalf("message %d timestamp = %v, want %d", i, stamp, want)
			}
		}
		if messages[2].Content[0].Timestamp != nil {
			t.Fatal("a tool result must not act as a file note boundary")
		}
	})
}
