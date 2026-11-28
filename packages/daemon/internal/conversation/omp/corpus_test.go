//go:build ompcorpus

package omp

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/conversation"
)

const corpusFollowUp = "Reply with the word corpus-check-6161."

var (
	corpusIndex = regexp.MustCompile(`^[0-9]+$`)
	corpusID    = regexp.MustCompile(`^[0-9a-f]{8}-(?:[0-9a-f]{4}-){3}[0-9a-f]{12}$`)
	corpusModel = regexp.MustCompile(`^(claude|gpt)-[a-z0-9.-]+$`)
)

// TestPinnedOMPCorpus takes a private directory with selected.tsv and sessions/<index>.jsonl.
// Set LEGION_TEST_OMP to the verified pin and LEGION_OMP_CORPUS_RESULTS to a private TSV path.
// Neither the test nor its child processes print transcript text, even when replay fails.
func TestPinnedOMPCorpus(t *testing.T) {
	dir := os.Getenv("LEGION_OMP_CORPUS_DIR")
	if dir == "" {
		t.Skip("set LEGION_OMP_CORPUS_DIR to the private corpus directory")
	}
	resultPath := os.Getenv("LEGION_OMP_CORPUS_RESULTS")
	if resultPath == "" || os.Getenv("LEGION_TEST_OMP") == "" {
		t.Fatal("set LEGION_OMP_CORPUS_RESULTS and LEGION_TEST_OMP")
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatal("corpus directory must have mode 0700")
	}
	manifest, err := os.Open(filepath.Join(dir, "selected.tsv"))
	if err != nil {
		t.Fatal("cannot open the corpus manifest")
	}
	defer manifest.Close()
	input := csv.NewReader(manifest)
	input.Comma, input.FieldsPerRecord = '\t', 4
	rows, err := input.ReadAll()
	if err != nil || len(rows) == 0 {
		t.Fatal("the corpus manifest must contain four columns and at least one row")
	}
	seen := make(map[string]bool, len(rows))
	for i, row := range rows {
		if !corpusIndex.MatchString(row[0]) || seen[row[0]] || (row[1] != "claude" && row[1] != "gpt") {
			t.Fatalf("invalid corpus manifest row %d", i+1)
		}
		seen[row[0]] = true
	}
	out, err := os.OpenFile(resultPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal("cannot open the results TSV")
	}
	defer out.Close()
	var read, compacted, equal, refused, different, failed int
	for _, row := range rows {
		index, family, id := row[0], row[1], "-"
		status, compact := "", "no"
		t.Run(index, func(t *testing.T) {
			status = "error: replay did not complete"
			content, err := os.ReadFile(filepath.Join(dir, "sessions", index+".jsonl"))
			if err != nil {
				status = "error: cannot read session"
				t.Fatal(status)
			}
			read++
			target, sessionID, hasCompaction := corpusTarget(content, family)
			id = sessionID
			if hasCompaction {
				compacted++
				compact = "yes"
			}
			status = compareCorpusSession(t, target, content)
			if status != "equal" {
				t.Error(status)
			}
		})
		if status == "" { // The -run filter excluded this subtest.
			continue
		}
		switch {
		case status == "equal":
			equal++
		case strings.HasPrefix(status, "refused:"):
			refused++
		case strings.HasPrefix(status, "different:"):
			different++
		default:
			failed++
		}
		if _, err := fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\n", index, id, family, compact, status); err != nil {
			t.Fatal("cannot write the results TSV")
		}
	}
	if err := out.Close(); err != nil {
		t.Fatal("cannot close the results TSV")
	}
	t.Logf("read=%d compacted=%d equal=%d refused=%d different=%d errors=%d", read, compacted, equal, refused, different, failed)
}

// Select the recorded model independently of Read, so a reader refusal does not prevent resume.
// Prefer the active branch's selection. Recover the target from records if the reader's parser
// refuses the session. The corpus distinguishes provider families in its manifest.
func corpusTarget(content []byte, family string) (target, string, bool) {
	route := target{api: conversation.OpenAIResponses, provider: "openai"}
	if family == "claude" {
		route.api, route.provider = conversation.AnthropicMessages, "anthropic"
	}
	setModel := func(model string) {
		if corpusModel.MatchString(model) && supportedTarget(route.provider, route.api, model) {
			route.model = model
		}
	}
	compacted := false
	id := "-"
	for line := range bytes.Lines(content) {
		var e entry
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		switch e.Type {
		case "session":
			// A subagent's filename names its parent, not the subagent's own session.
			var h header
			if id == "-" && json.Unmarshal(line, &h) == nil && corpusID.MatchString(h.ID) {
				id = h.ID
			}
		case "compaction":
			compacted = true
		case "model_change":
			if model, ok := strings.CutPrefix(e.Model, route.provider+"/"); ok {
				setModel(model)
			}
		case "message":
			if m := e.Message; m != nil && m.Role == "assistant" && m.Provider == route.provider && m.API == route.api {
				setModel(m.Model)
			}
		}
	}
	if p, err := parseSession(content); err == nil {
		if built, err := buildContext(p.entries); err == nil {
			if model, ok := strings.CutPrefix(built.model, route.provider+"/"); ok && built.api == route.api {
				setModel(model)
			}
		}
	}
	return route, id, compacted
}

func compareCorpusSession(t *testing.T, route target, content []byte) string {
	t.Helper()
	if route.model == "" {
		return "error: no supported replay target"
	}
	// The proxy only rejects requests. The only model route in the fresh HOME uses loopback
	// and a dummy key. Disable tools and external prompt discovery in the child process.
	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer deny.Close()
	var model *standIn
	opts := Options{}
	if route.api == conversation.AnthropicMessages {
		model = newAnthropicStandIn(t)
		// Official signing and tool ID rules, with native compaction over loopback.
		// The pin accepts a Bedrock Messages proxy for native replay at its configured URL.
		// Permit legacy replay too: the pin includes its replay edit for unsigned cached state.
		opts.ReplayAnthropicCompaction = true
		opts.ReplayLegacyAnthropicCompaction = true
	} else {
		model = newOpenAIStandIn(t)
	}
	// Both recorded families accept images. Neither request enables credential redaction.
	rig := newOMPRigForProviderModel(t, route.api, route.provider, route.model, model,
		"HTTP_PROXY="+deny.URL, "HTTPS_PROXY="+deny.URL, "ALL_PROXY="+deny.URL, "NO_PROXY=127.0.0.1,localhost")
	setStandInModalities(t, rig, false)
	agent := filepath.Join(rig.home, ".omp", "agent")
	if route.api == conversation.AnthropicMessages {
		appendRaw(t, filepath.Join(agent, "models.yml"), `        compat:
          officialEndpoint: true
          signingEndpoint: true
          replayUnsignedThinking: false
          supportsMidConversationSystem: true
          supportsTurnScopedSystem: true
          supportsPerMessageEffort: true
          bedrockMessagesApi: true
          supportsServerCompaction: true
`)
	}
	// Replay existing compaction rather than asking the stand-in to summarize again.
	appendRaw(t, filepath.Join(agent, "config.yml"), "compaction:\n  enabled: false\n")
	if err := os.MkdirAll(rig.sessions, 0o700); err != nil {
		return "error: cannot create private session directory"
	}
	file := filepath.Join(rig.sessions, "resume.jsonl")
	if err := os.WriteFile(file, content, 0o600); err != nil {
		return "error: cannot copy session for resume"
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, rig.omp, "-p", "--session-dir", rig.sessions, "--no-title",
		"--no-extensions", "--no-skills", "--no-rules", "--no-context-files", "--no-prewalk", "--no-tools", "--no-lsp",
		"--provider", route.provider, "--model", route.model, "--resume", file, corpusFollowUp)
	cmd.Dir, cmd.Env = rig.work, append(rig.env, "TMPDIR="+rig.work)
	// Do not use ompRig.run: it prints the CLI output when resume fails.
	runErr := cmd.Run()
	if runErr != nil {
		if ctx.Err() != nil {
			return "error: pinned resume timed out"
		}
		return "error: pinned resume failed"
	}
	model.mu.Lock()
	defer model.mu.Unlock()
	if len(model.requests) == 0 {
		return "error: pinned resume sent no request"
	}
	before, err := Read(content, opts)
	if err != nil {
		return corpusReadError(err)
	}
	resumedBytes, err := os.ReadFile(file)
	if err != nil {
		return "error: cannot read resumed session"
	}
	// Like assertResumeMatches, cut at the newly written follow-up's entry ID. Any
	// prune records the pin wrote before this point belong to the compared history.
	prefix, ok := corpusResumePrefix(before, resumedBytes)
	if !ok {
		return "error: no journaled follow-up"
	}
	resumed, err := Read(prefix, opts)
	if err != nil {
		return corpusReadError(err)
	}
	if resumed.Model != route.provider+"/"+route.model || resumed.API != route.api {
		return "error: restored target differs from replay target"
	}
	journal, ok := corpusReminders(resumedBytes)
	if !ok {
		return "error: invalid reminder journal"
	}
	// A retry can make more than one request. Use the last resume request, not a
	// connection probe. The stand-in supplies only an answer and cannot call tools.
	got, ok := corpusRequest(model.requests[len(model.requests)-1], route, journal)
	if !ok {
		return "error: cannot reduce captured request"
	}
	want, ok := corpusReaderInput(resumed.Messages, route.api)
	if !ok {
		return "error: cannot reduce rebuilt messages"
	}
	if difference := corpusDifference(got, want); difference != "" {
		return difference
	}
	// As in assertBlankedOnly, do not accept a change to the rebuilt history
	// between the original session and the resume other than pruned tool output.
	for i := range max(len(before.Messages), len(resumed.Messages)) {
		if i >= len(before.Messages) {
			return corpusDifferenceAt(i, nil, corpusObject{"role": string(resumed.Messages[i].Role)})
		}
		if i >= len(resumed.Messages) {
			return corpusDifferenceAt(i, corpusObject{"role": string(before.Messages[i].Role)}, nil)
		}
		was, now := before.Messages[i], resumed.Messages[i]
		if _, prune := blankedResult(was, now); !prune && !reflect.DeepEqual(was, now) {
			return corpusDifferenceAt(i, corpusObject{"role": string(was.Role)}, corpusObject{"role": string(now.Role)})
		}
	}
	return "equal"
}

// Never emit FormatError.What verbatim: several branches include untrusted fields.
func corpusReadError(err error) string {
	var unknown *conversation.FormatError
	if !errors.As(err, &unknown) {
		return "error: reader failed"
	}
	for _, reason := range []string{
		"snapcompact compaction", "session entry type", "session header version", "message role",
		"an image held in Oh My Pi's blob store", "an image held at a URL or in a provider's file store",
		"a native Anthropic server tool block", "an Anthropic server-side fallback boundary",
		"an assistant turn carrying an Anthropic compaction block", "providerPayload type",
		"target provider", "the session names no model", "no Oh My Pi session header",
	} {
		if strings.HasPrefix(unknown.What, reason) {
			return "refused: " + reason
		}
	}
	if strings.Contains(unknown.What, " content block type ") {
		return "refused: unsupported content block type"
	}
	return "refused: unknown session format"
}

func corpusResumePrefix(before *Session, content []byte) ([]byte, bool) {
	offset := 0
	for line := range bytes.Lines(content) {
		var e entry
		if json.Unmarshal(line, &e) == nil && e.Type == "message" && e.Message != nil &&
			e.Message.Role == "user" && !before.ids[e.ID] {
			blocks, err := parseBlocks(e.Message.Content, inputBlocks, "follow-up")
			if err == nil && len(blocks) == 1 && blocks[0].Type == "text" && blocks[0].Text == corpusFollowUp {
				return content[:offset], true
			}
			return nil, false
		}
		offset += len(line)
	}
	return nil, false
}

// Use the contract tests' journal matching rules without printing invalid records.
func corpusReminders(content []byte) (reminderJournal, bool) {
	j := reminderJournal{prefixes: map[string]int{}, controls: map[string]int{}}
	p, err := parseSession(content)
	if err != nil {
		return j, false
	}
	byID := make(map[string]*entry, len(p.entries))
	for _, e := range p.entries {
		byID[e.ID] = e
	}
	for _, e := range activePath(resolveLeaf(p.entries, byID), byID) {
		if e.Type == "reset_boundary" {
			clear(j.prefixes)
			clear(j.controls)
		}
		if e.Type == "custom" && e.CustomType == "date-cwd-reminder" {
			var d struct {
				Reminder  string `json:"reminder"`
				Placement string `json:"placement"`
			}
			if json.Unmarshal(e.Data, &d) != nil || d.Reminder == "" {
				return j, false
			}
			switch d.Placement {
			case "prefix":
				j.prefixes[d.Reminder]++
			case "control":
				j.controls[d.Reminder]++
			default:
				return j, false
			}
		}
	}
	return j, true
}

type corpusObject = map[string]any

func corpusText(kind, text string) corpusObject { return corpusObject{"type": kind, "text": text} }

func corpusTurn(role string, parts ...any) corpusObject {
	return corpusObject{"role": role, "content": parts}
}

// corpusRequest removes request-only fields, journaled controls, and the new prompt.
// Keep historical developer messages, tool I/O, and native compaction and reasoning.
func corpusRequest(body []byte, route target, journal reminderJournal) ([]corpusObject, bool) {
	var request struct {
		Model    string         `json:"model"`
		Messages []corpusObject `json:"messages"`
		Input    []corpusObject `json:"input"`
	}
	if json.Unmarshal(body, &request) != nil || request.Model != route.model {
		return nil, false
	}
	items, textType, separator := request.Input, "input_text", "\n\n"
	controlRole := "developer"
	if route.api == conversation.AnthropicMessages {
		items, textType, separator, controlRole = request.Messages, "text", "\n", "user"
	}
	var kept []corpusObject
	for _, item := range items {
		if text, ok := item["content"].(string); ok {
			item["content"] = []any{corpusText(textType, text)}
		}
		if parts, _ := item["content"].([]any); len(parts) > 0 {
			first, _ := parts[0].(corpusObject)
			text, _ := first["text"].(string)
			if first["type"] == textType {
				if len(parts) == 1 && item["role"] == controlRole && journal.takeControl(text) {
					continue
				}
				if item["role"] == "user" {
					if rest, ok := journal.cutPrefix(text, separator); ok {
						if rest != "" {
							first["text"] = rest
						} else if len(parts) == 1 {
							continue
						} else {
							item["content"] = parts[1:]
						}
					}
				}
			}
		}
		kept = append(kept, item)
	}
	// The new prompt is normally its own turn. Do not remove a historical turn
	// just because its text contains the follow-up, or accept a request without it.
	if len(kept) == 0 {
		return nil, false
	}
	last := kept[len(kept)-1]
	parts, _ := last["content"].([]any)
	if last["role"] != "user" || len(parts) == 0 {
		return nil, false
	}
	prompt, _ := parts[len(parts)-1].(corpusObject)
	if prompt["type"] != textType || prompt["text"] != corpusFollowUp {
		return nil, false
	}
	if len(parts) == 1 {
		return kept[:len(kept)-1], true
	}
	last["content"] = parts[:len(parts)-1]
	return kept, true
}

func corpusReaderInput(messages []conversation.Message, api string) ([]corpusObject, bool) {
	if api == conversation.AnthropicMessages {
		return corpusAnthropicInput(messages)
	}
	return corpusResponsesInput(messages)
}

func corpusAnthropicInput(messages []conversation.Message) ([]corpusObject, bool) {
	var items []corpusObject
	previousTool := false
	for _, m := range messages {
		var parts []any
		if m.Native != nil && m.Native.Kind == conversation.AnthropicCompaction {
			var payload corpusObject
			if json.Unmarshal(m.Native.Payload, &payload) != nil {
				return nil, false
			}
			part := corpusObject{"type": "compaction", "content": payload["content"]}
			for recorded, wire := range map[string]string{"signature": "signature", "encryptedContent": "encrypted_content", "toolChanges": "tool_changes"} {
				if field, ok := payload[recorded]; ok {
					part[wire] = field
				}
			}
			parts = append(parts, part)
		}
		for _, b := range m.Content {
			switch b.Type {
			case conversation.Text, conversation.Image:
				parts = append(parts, corpusInputPart(b, false))
			case conversation.Thinking:
				part := corpusObject{"type": "thinking", "thinking": b.Text}
				if b.Signature != "" {
					part["signature"] = b.Signature
				}
				parts = append(parts, part)
			case conversation.RedactedThinking:
				parts = append(parts, corpusObject{"type": "redacted_thinking", "data": b.Data})
			case conversation.ToolCall:
				var input any
				if json.Unmarshal(b.Arguments, &input) != nil {
					return nil, false
				}
				parts = append(parts, corpusObject{"type": "tool_use", "id": b.ToolCallID, "name": b.ToolName, "input": input})
			case conversation.ToolResult:
				content := corpusToolOutput(b.Content, false)
				if text, ok := content.(string); b.IsError && ok && strings.TrimSpace(text) == "" {
					content = "Tool failed with no output."
				}
				part := corpusObject{"type": "tool_result", "tool_use_id": b.ToolCallID, "content": content}
				if b.IsError {
					part["is_error"] = true
				}
				parts = append(parts, part)
			default:
				return nil, false
			}
		}
		if m.Role == conversation.Tool && previousTool {
			last := items[len(items)-1]
			last["content"] = append(last["content"].([]any), parts...)
			continue
		}
		role := string(m.Role)
		previousTool = m.Role == conversation.Tool
		if previousTool {
			role = "user"
		}
		item := corpusTurn(role, parts...)
		if m.Native != nil && m.Native.Controls != nil {
			if c := m.Native.Controls; c.ClearAt != "" {
				item["clear_at"] = c.ClearAt
			}
			if c := m.Native.Controls; c.Effort != "" {
				item["output_config"] = corpusObject{"effort": c.Effort}
			}
		}
		items = append(items, item)
	}
	return items, true
}

func corpusResponsesInput(messages []conversation.Message) ([]corpusObject, bool) {
	var items []corpusObject
	customCalls := make(map[string]bool)
	for _, m := range messages {
		if m.Native != nil && m.Native.Kind == conversation.OpenAIResponsesHistory && len(m.Native.Items) != 0 {
			for _, raw := range m.Native.Items {
				var item corpusObject
				if json.Unmarshal(raw, &item) != nil || item == nil {
					return nil, false
				}
				items = append(items, item)
				// The reader preserves the call kind in native history. The sender
				// uses it to choose the wire type for the corresponding tool result.
				if id, ok := item["call_id"].(string); ok && item["type"] == "custom_tool_call" {
					customCalls[id] = true
				}
			}
			continue
		}
		switch m.Role {
		case conversation.User, conversation.System, conversation.Role("developer"):
			var parts []any
			for _, b := range m.Content {
				parts = append(parts, corpusInputPart(b, true))
			}
			items = append(items, corpusTurn(string(m.Role), parts...))
		case conversation.Assistant:
			for _, b := range m.Content {
				switch b.Type {
				case conversation.Text:
					item := corpusTurn("assistant", corpusObject{"type": "output_text", "text": b.Text, "annotations": []any{}})
					item["type"] = "message"
					if b.Phase != "" {
						item["phase"] = b.Phase
					}
					items = append(items, item)
				case conversation.Thinking:
					var item corpusObject
					if json.Unmarshal([]byte(b.Signature), &item) != nil || item == nil {
						return nil, false
					}
					items = append(items, item)
				case conversation.ToolCall:
					name := b.ToolName
					if b.CustomWireName != "" {
						name = b.CustomWireName
					}
					item := corpusObject{"type": "function_call", "call_id": b.ToolCallID, "name": name, "arguments": string(b.Arguments)}
					if b.Namespace != "" {
						item["namespace"] = b.Namespace
					}
					items = append(items, item)
				default:
					return nil, false
				}
			}
		case conversation.Tool:
			for _, b := range m.Content {
				kind := "function_call_output"
				if customCalls[b.ToolCallID] {
					kind = "custom_tool_call_output"
				}
				items = append(items, corpusObject{"type": kind, "call_id": b.ToolCallID, "output": corpusToolOutput(b.Content, true)})
			}
		default:
			return nil, false
		}
	}
	return items, true
}

func corpusInputPart(b conversation.Block, responses bool) corpusObject {
	if b.Type == conversation.Image {
		if responses {
			detail := b.Detail
			if detail == "" {
				detail = "auto"
			}
			return corpusObject{"type": "input_image", "image_url": "data:" + b.MediaType + ";base64," + b.ImageData, "detail": detail}
		}
		return corpusObject{"type": "image", "source": corpusObject{"type": "base64", "media_type": b.MediaType, "data": b.ImageData}}
	}
	if responses {
		return corpusText("input_text", b.Text)
	}
	return corpusText("text", b.Text)
}

func corpusToolOutput(blocks []conversation.Block, responses bool) any {
	var texts []string
	images := false
	for _, b := range blocks {
		if b.Type == conversation.Image {
			images = true
		} else {
			texts = append(texts, b.Text)
		}
	}
	if !images {
		return strings.Join(texts, "\n")
	}
	parts := make([]any, 0, len(blocks))
	for _, b := range blocks {
		parts = append(parts, corpusInputPart(b, responses))
	}
	return parts
}

// Both sides use the same reduction. Ignore cache marks and Responses item envelope IDs
// and status, as the contract tests do. Keep call IDs and every content field.
func corpusNormalize(item corpusObject) {
	delete(item, "id") // item envelope, not a nested Anthropic tool ID
	delete(item, "status")
	if item["type"] == "message" {
		delete(item, "type") // optional for Responses user and developer items
	}
	if item["type"] == "function_call" {
		if args, ok := item["arguments"].(string); ok {
			var value any
			if json.Unmarshal([]byte(args), &value) == nil {
				item["arguments"] = value
			}
		}
	}
	corpusNormalizeParts(item)
}

func corpusNormalizeParts(obj corpusObject) {
	delete(obj, "cache_control")
	if obj["is_error"] == false {
		delete(obj, "is_error")
	}
	if obj["type"] == "thinking" && obj["signature"] == "" {
		delete(obj, "signature")
	}
	if parts, ok := obj["content"].([]any); ok {
		var texts []string
		allText := true
		for _, raw := range parts {
			part, _ := raw.(corpusObject)
			corpusNormalizeParts(part)
			if text, ok := part["text"].(string); part["type"] == "text" && ok {
				texts = append(texts, text)
			} else {
				allText = false
			}
		}
		if obj["type"] == "tool_result" && allText {
			obj["content"] = strings.Join(texts, "\n")
		}
	}
}

func corpusDifference(got, want []corpusObject) string {
	for i := range min(len(got), len(want)) {
		corpusNormalize(got[i])
		corpusNormalize(want[i])
		if !reflect.DeepEqual(got[i], want[i]) {
			return corpusDifferenceAt(i, got[i], want[i])
		}
	}
	if len(got) > len(want) {
		return corpusDifferenceAt(len(want), got[len(want)], nil)
	}
	if len(want) > len(got) {
		return corpusDifferenceAt(len(got), nil, want[len(got)])
	}
	return ""
}

func corpusDifferenceAt(i int, got, want corpusObject) string {
	return fmt.Sprintf("different: index %d %s/%s", i, corpusKind(got), corpusKind(want))
}

func corpusKind(item corpusObject) string {
	if item == nil {
		return "missing"
	}
	for _, field := range []string{"role", "type"} {
		switch item[field] {
		case "user", "assistant", "system", "developer", "tool", "message", "reasoning", "compaction",
			"compaction_summary", "function_call", "function_call_output", "custom_tool_call", "custom_tool_call_output",
			"computer_call", "computer_call_output", "local_shell_call", "local_shell_call_output", "web_search_call":
			return item[field].(string)
		}
	}
	return "other"
}
