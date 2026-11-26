package daemon

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// promptInputsReadRecord is one read call observed in a session transcript before the first
// non-read tool call, with its target path and result byte size.
type promptInputsReadRecord struct {
	Path  string `json:"path"`
	Bytes int    `json:"bytes"`
	Tool  string `json:"tool"`
}

// promptInputsTranscriptReads parses an Oh My Pi or Claude Code JSONL session transcript and
// returns every read call (`read`, `Read`, `Skill`, or Anthropic-prefixed `_read` etc.) made before
// the first non-read tool call, with the byte length of each call's tool result content.
//
// Oh My Pi session JSONL entries have `type: "message"` with `message.role == "assistant"` (content
// blocks `type: "toolCall"` or `"tool_use"`, `id`, `name`, `arguments`/`input`) and
// `message.role == "toolResult"` (`toolCallId`, `content`, `isError`).
// Claude Code session JSONL entries have `type: "assistant"` (`message.content` blocks
// `type: "tool_use"`, `id`, `name`, `input`) and `type: "user"` (`message.content` blocks
// `type: "tool_result"`, `tool_use_id`, `content`, `is_error`).
func promptInputsTranscriptReads(jsonl []byte) ([]promptInputsReadRecord, string, error) {
	type pendingCall struct {
		id   string
		tool string
		path string
	}
	var ordered []pendingCall
	byID := map[string]bool{}
	results := map[string]int{}
	stoppedAt := ""
	stopped := false

	scanner := bufio.NewScanner(bytes.NewReader(jsonl))
	// Transcript lines can carry large file reads (several MiB).
	scanner.Buffer(make([]byte, 64*1024), 32*1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var env struct {
			Type    string          `json:"type"`
			Message json.RawMessage `json:"message"`
		}
		if err := json.Unmarshal(line, &env); err != nil {
			return nil, "", fmt.Errorf("transcript line %d: %w", lineNo, err)
		}
		if len(env.Message) == 0 {
			continue
		}
		var msg struct {
			Role       string          `json:"role"`
			ToolCallID string          `json:"toolCallId"`
			ToolName   string          `json:"toolName"`
			IsError    bool            `json:"isError"`
			Content    json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(env.Message, &msg); err != nil {
			continue
		}

		switch msg.Role {
		case "assistant":
			if stopped {
				continue
			}
			var blocks []struct {
				Type      string          `json:"type"`
				ID        string          `json:"id"`
				Name      string          `json:"name"`
				Input     json.RawMessage `json:"input"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal(msg.Content, &blocks); err != nil {
				continue
			}
			for _, b := range blocks {
				if b.Type != "toolCall" && b.Type != "tool_use" {
					continue
				}
				argsRaw := b.Input
				if len(argsRaw) == 0 {
					argsRaw = b.Arguments
				}
				if !promptInputsIsReadTool(b.Name) {
					stopped = true
					stoppedAt = b.Name
					break
				}
				path := promptInputsReadToolTarget(b.Name, argsRaw)
				if path == "" || b.ID == "" {
					return nil, "", fmt.Errorf("transcript line %d: %s call has no target or id", lineNo, b.Name)
				}
				byID[b.ID] = true
				ordered = append(ordered, pendingCall{id: b.ID, tool: b.Name, path: path})
			}

		case "toolResult":
			// Oh My Pi tool result entry: top-level message with toolCallId.
			if msg.ToolCallID == "" || msg.IsError {
				continue
			}
			if _, ok := byID[msg.ToolCallID]; ok {
				results[msg.ToolCallID] = promptInputsContentTextBytes(msg.Content)
			}

		case "user":
			// Claude Code tool_result blocks live inside a user message's content array.
			var blocks []struct {
				Type      string          `json:"type"`
				ToolUseID string          `json:"tool_use_id"`
				IsError   bool            `json:"is_error"`
				Content   json.RawMessage `json:"content"`
			}
			if err := json.Unmarshal(msg.Content, &blocks); err != nil {
				continue
			}
			for _, b := range blocks {
				if b.Type != "tool_result" || b.IsError {
					continue
				}
				if _, ok := byID[b.ToolUseID]; ok {
					results[b.ToolUseID] = promptInputsContentTextBytes(b.Content)
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, "", fmt.Errorf("scan transcript: %w", err)
	}

	out := make([]promptInputsReadRecord, 0, len(ordered))
	for _, call := range ordered {
		n, ok := results[call.id]
		if !ok {
			continue // A failed or incomplete read did not put a file into context.
		}
		out = append(out, promptInputsReadRecord{
			Path:  call.path,
			Bytes: n,
			Tool:  call.tool,
		})
	}
	return out, stoppedAt, nil
}

// promptInputsIsReadTool reports whether toolName is a file/skill read call counted toward
// required reads before work begins.
func promptInputsIsReadTool(toolName string) bool {
	trimmed := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(toolName)), "_")
	switch trimmed {
	case "read", "skill":
		return true
	default:
		return false
	}
}

// promptInputsReadToolTarget extracts the path or skill name from a read/Read/Skill tool call's
// JSON arguments.
func promptInputsReadToolTarget(toolName string, argsRaw json.RawMessage) string {
	var args map[string]any
	if err := json.Unmarshal(argsRaw, &args); err != nil {
		return ""
	}
	for _, key := range []string{"path", "file_path", "skill", "name"} {
		if v, ok := args[key].(string); ok && v != "" {
			if strings.EqualFold(strings.TrimPrefix(toolName, "_"), "skill") && !strings.HasPrefix(v, "skill://") {
				return "skill://" + promptInputsNormalizeSkillName(v)
			}
			return v
		}
	}
	return ""
}

// promptInputsContentTextBytes sums the byte lengths of text content in a tool result (either a
// JSON string or an array of content blocks).
func promptInputsContentTextBytes(raw json.RawMessage) int {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return 0
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err == nil {
			return len(s)
		}
		return 0
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(trimmed, &blocks); err != nil {
		return 0
	}
	total := 0
	for _, b := range blocks {
		total += len(b.Text)
	}
	return total
}

// promptInputsToReads converts transcript read records into the evidence row's promptInputsRead slice.
func promptInputsToReads(records []promptInputsReadRecord) []promptInputsRead {
	if len(records) == 0 {
		return nil
	}
	out := make([]promptInputsRead, len(records))
	for i, r := range records {
		out[i] = promptInputsRead{Path: r.Path, Bytes: r.Bytes}
	}
	return out
}

func TestPromptInputsRecordsReadsBeforeWork(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		jsonl string
		want  []promptInputsReadRecord
		stop  string
	}{
		{
			name: "omp",
			jsonl: `{"type":"session"}
{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","id":"1","name":"read","arguments":{"path":"skill://entry"}}]}}
{"type":"message","message":{"role":"toolResult","toolCallId":"1","content":[{"type":"text","text":"Résumé\n"}]}}
{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","id":"2","name":"read","arguments":{"path":"README.md"}},{"type":"toolCall","id":"3","name":"bash","arguments":{"command":"true"}},{"type":"toolCall","id":"4","name":"read","arguments":{"path":"after.md"}}]}}
{"type":"message","message":{"role":"toolResult","toolCallId":"4","content":[{"type":"text","text":"excluded"}]}}
{"type":"message","message":{"role":"toolResult","toolCallId":"2","content":[{"type":"text","text":"file\n"}]}}`,
			want: []promptInputsReadRecord{{Path: "skill://entry", Bytes: len("Résumé\n"), Tool: "read"}, {Path: "README.md", Bytes: 5, Tool: "read"}},
			stop: "bash",
		},
		{
			name: "claude",
			jsonl: `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"1","name":"Skill","input":{"skill":"legion:entry"}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"1","content":[{"type":"text","text":"entry"}]}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"2","name":"Read","input":{"file_path":"missing.md"}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"2","is_error":true,"content":"file not found"}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"3","name":"Bash","input":{"command":"true"}}]}}`,
			want: []promptInputsReadRecord{{Path: "skill://entry", Bytes: 5, Tool: "Skill"}},
			stop: "Bash",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, stop, err := promptInputsTranscriptReads([]byte(tc.jsonl))
			if err != nil || stop != tc.stop || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("reads = %#v, stop = %q, error = %v; want %#v, stop = %q", got, stop, err, tc.want, tc.stop)
			}
		})
	}
}

func TestPromptInputsRecordsCapturedReads(t *testing.T) {
	t.Parallel()
	data := promptInputsOpenCapture(t, "testdata/prompt-inputs/f5/reads-controller-interactive-omp.jsonl.gz")
	got, stop, err := promptInputsTranscriptReads(data)
	if err != nil {
		t.Fatal(err)
	}
	if stop != "bash" {
		t.Fatalf("first non-read call %q, want bash", stop)
	}
	for i := range got {
		if filepath.IsAbs(got[i].Path) {
			got[i].Path = filepath.Base(got[i].Path)
		}
	}
	want := []promptInputsReadRecord{
		{Path: "skill://legion-controller", Bytes: 7929, Tool: "read"},
		{Path: "README.md", Bytes: 17578, Tool: "read"},
		{Path: "skill://legion-controller/references/session.md", Bytes: 4847, Tool: "read"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reads = %#v, want %#v", got, want)
	}
}
