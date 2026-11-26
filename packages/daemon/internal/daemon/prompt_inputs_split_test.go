package daemon

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// promptInputsDispatchFirstOpen and promptInputsDispatchFirstClose are the markers pi-envoy and
// claude-envoy wrap the injected dispatch-first skill in (packages/envoy-client/src/dispatch-first.ts).
const (
	promptInputsDispatchFirstOpen  = "<dispatch-first-skill>"
	promptInputsDispatchFirstClose = "</dispatch-first-skill>"
)

// promptInputsSplitResult separates exact wire sizes from the content sizes used in the row.
// System, tools and messages divide the request body with FramingBytes, which includes JSON
// punctuation and the request options. The decoded system text separates the Legion prompt, the
// harness's own text, and any skill list. Some harnesses send the skill list in a message instead.
type promptInputsSplitResult struct {
	// Raw HTTP body length in bytes.
	TotalRequestBytes int `json:"total_request_bytes"`
	// Wire (JSON-encoded) byte counts of top-level sections, summing with FramingBytes to TotalRequestBytes.
	WireSystemBytes   int `json:"wire_system_bytes"`
	WireToolsBytes    int `json:"wire_tools_bytes"`
	WireMessagesBytes int `json:"wire_messages_bytes"`
	FramingBytes      int `json:"framing_bytes"`

	// Decoded text byte counts inside the system prompt and messages.
	SystemTextBytes      int `json:"system_text_bytes"`
	LegionPromptBytes    int `json:"legion_prompt_bytes"`
	HarnessSystemBytes   int `json:"harness_system_bytes"`
	SkillListBytes       int `json:"skill_list_bytes"`
	ToolDefinitionsBytes int `json:"tool_definitions_bytes"`
	DispatchFirstBytes   int `json:"dispatch_first_bytes"`
	FirstMessageBytes    int `json:"first_message_bytes"`
	OtherMessagesBytes   int `json:"other_messages_bytes"`

	// DecodedSkills lists the skill names discovered in the request's skill list, sorted.
	Skills []string `json:"skills"`
	// LegionPromptFound is whether the launched prompt file's bytes appeared in the system text.
	LegionPromptFound bool `json:"legion_prompt_found"`
}

// promptInputsToRequest converts a split result into the evidence row's promptInputsRequest.
func (s promptInputsSplitResult) toRequest() promptInputsRequest {
	return promptInputsRequest{
		SystemTextBytes:      s.SystemTextBytes,
		ToolDefinitionsBytes: s.ToolDefinitionsBytes,
		SkillListBytes:       s.SkillListBytes,
		DispatchFirstBytes:   s.DispatchFirstBytes,
		FirstMessageBytes:    s.FirstMessageBytes,
	}
}

// promptInputsSplitRequest splits one Anthropic Messages API request body (as Oh My Pi or Claude
// Code sends it) into its named parts, accounting for every byte of the raw body and splitting the
// system text into the Legion prompt (launchedPrompt), the harness's own system text, and the
// skill list.
func promptInputsSplitRequest(raw []byte, launchedPrompt string) (promptInputsSplitResult, error) {
	if len(raw) == 0 {
		return promptInputsSplitResult{}, fmt.Errorf("empty request body")
	}
	top, err := promptInputsTopLevelSpans(raw)
	if err != nil {
		return promptInputsSplitResult{}, fmt.Errorf("parse top-level JSON spans: %w", err)
	}

	res := promptInputsSplitResult{TotalRequestBytes: len(raw)}
	var systemRaw, toolsRaw, messagesRaw []byte
	if span, ok := top["system"]; ok {
		res.WireSystemBytes = span.end - span.start
		systemRaw = raw[span.start:span.end]
	}
	if span, ok := top["tools"]; ok {
		res.WireToolsBytes = span.end - span.start
		toolsRaw = raw[span.start:span.end]
	}
	if span, ok := top["messages"]; ok {
		res.WireMessagesBytes = span.end - span.start
		messagesRaw = raw[span.start:span.end]
	}
	res.FramingBytes = res.TotalRequestBytes - res.WireSystemBytes - res.WireToolsBytes - res.WireMessagesBytes
	if res.FramingBytes < 0 {
		return promptInputsSplitResult{}, fmt.Errorf("wire spans (%d+%d+%d) exceed request length %d",
			res.WireSystemBytes, res.WireToolsBytes, res.WireMessagesBytes, res.TotalRequestBytes)
	}

	// Tool definitions: raw JSON array bytes of the `tools` value (exact wire size of definitions).
	res.ToolDefinitionsBytes = res.WireToolsBytes

	// Decode system blocks/string.
	var systemTexts []string
	if len(systemRaw) > 0 {
		systemTexts, err = promptInputsDecodeSystem(systemRaw)
		if err != nil {
			return promptInputsSplitResult{}, fmt.Errorf("decode system field: %w", err)
		}
	}
	fullSystem := strings.Join(systemTexts, "")

	// Extract skill list from system text (Oh My Pi: `<skills>...</skills>` or `<available_skills>...</available_skills>`).
	skillListBytes, skills, systemWithoutSkills := promptInputsExtractSkillsFromText(fullSystem)

	// Claude Code also lists skills inside the Skill tool description or inside a user message
	// (`<system-reminder>` containing `The following skills are available`). If system held no
	// skill list, inspect tools and messages after decoding messages.
	var messages []promptInputsWireMessage
	if len(messagesRaw) > 0 {
		if err := json.Unmarshal(messagesRaw, &messages); err != nil {
			return promptInputsSplitResult{}, fmt.Errorf("decode messages: %w", err)
		}
	}

	// Split messages into dispatch-first injection, first message, and any other messages/reminders.
	var dispatchFirstBytes, firstMsgBytes, otherMsgBytes int
	var skillListFromMessages int
	var messageSkills []string
	firstRealMessageSeen := false
	for _, msg := range messages {
		blocks := msg.textBlocks()
		for _, text := range blocks {
			rem := text
			// Extract <dispatch-first-skill>...</dispatch-first-skill> anywhere it appears
			// (Oh My Pi injects a dedicated user message; Claude Code wraps it in a SessionStart
			// hook system-reminder block inside the user message).
			for {
				dfBytes, stripped, found := promptInputsCutTag(rem, promptInputsDispatchFirstOpen, promptInputsDispatchFirstClose)
				if !found {
					break
				}
				dispatchFirstBytes += dfBytes
				rem = stripped
			}
			// Extract Claude Code skill listing reminder if system held no skill list.
			if skillListBytes == 0 {
				sb, sk, stripped := promptInputsExtractClaudeSkillReminder(rem)
				if sb > 0 {
					skillListFromMessages += sb
					messageSkills = append(messageSkills, sk...)
					rem = stripped
				}
			}
			if len(rem) == 0 {
				continue
			}
			if !firstRealMessageSeen && msg.Role == "user" && !promptInputsIsSystemReminderOnly(rem) {
				firstRealMessageSeen = true
				firstMsgBytes += len(rem)
			} else if !firstRealMessageSeen && msg.Role == "user" {
				// System-reminder blocks (e.g. date, hook wrapper) that precede the real prompt.
				otherMsgBytes += len(rem)
			} else {
				otherMsgBytes += len(rem)
			}
		}
	}

	if skillListBytes == 0 && skillListFromMessages > 0 {
		skillListBytes = skillListFromMessages
		skills = messageSkills
	}
	if skillListBytes == 0 && len(toolsRaw) > 0 {
		sb, sk := promptInputsExtractSkillsFromTools(toolsRaw)
		if sb > 0 {
			skillListBytes = sb
			skills = sk
			// Subtract the encoded string fragment from the tool array. The skill is counted
			// once, as decoded text, just as it would be in the system or messages fields.
			res.ToolDefinitionsBytes -= sb
		}
	}

	// Account for the Legion prompt inside systemWithoutSkills.
	var legionBytes int
	if launchedPrompt != "" && strings.Contains(systemWithoutSkills, launchedPrompt) {
		res.LegionPromptFound = true
		legionBytes = len(launchedPrompt)
	} else if trimmed := strings.TrimSpace(launchedPrompt); trimmed != "" && strings.Contains(systemWithoutSkills, trimmed) {
		res.LegionPromptFound = true
		legionBytes = len(trimmed)
	} else if compact := promptInputsCompactTables(strings.TrimSpace(launchedPrompt)); compact != "" && strings.Contains(systemWithoutSkills, compact) {
		res.LegionPromptFound = true
		legionBytes = len(compact)
	}

	res.SkillListBytes = skillListBytes
	res.SystemTextBytes = len(systemWithoutSkills)
	res.LegionPromptBytes = legionBytes
	res.HarnessSystemBytes = res.SystemTextBytes - res.LegionPromptBytes
	res.DispatchFirstBytes = dispatchFirstBytes
	res.FirstMessageBytes = firstMsgBytes
	res.OtherMessagesBytes = otherMsgBytes
	res.Skills = promptInputsUniqueSorted(skills)
	return res, nil
}

// Oh My Pi compacts Markdown table cells in the appended prompt. Match the compacted
// text exactly and count its bytes, without normalizing any other part of the prompt.
func promptInputsCompactTables(prompt string) string {
	lines := strings.Split(prompt, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "|") && strings.HasSuffix(line, "|") {
			cells := strings.Split(line, "|")
			for j := range cells {
				cells[j] = strings.TrimSpace(cells[j])
			}
			lines[i] = strings.Join(cells, "|")
		}
	}
	return strings.Join(lines, "\n")
}

type promptInputsByteSpan struct {
	start int
	end   int
}

// promptInputsTopLevelSpans returns the byte slice [start, end) within raw of each top-level JSON
// object key's value, preserving exact wire bytes so WireSystemBytes + WireToolsBytes +
// WireMessagesBytes + FramingBytes == len(raw).
func promptInputsTopLevelSpans(raw []byte) (map[string]promptInputsByteSpan, error) {
	if !json.Valid(raw) {
		return nil, fmt.Errorf("not a single JSON value")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, fmt.Errorf("expected top-level JSON object, got %v", tok)
	}
	spans := map[string]promptInputsByteSpan{}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, fmt.Errorf("expected string key, got %T", keyTok)
		}
		if _, exists := spans[key]; exists {
			return nil, fmt.Errorf("duplicate top-level key %q", key)
		}
		// Advance past whitespace and ':' to the start of the value in raw.
		pos := int(dec.InputOffset())
		for pos < len(raw) && (raw[pos] == ' ' || raw[pos] == '\t' || raw[pos] == '\r' || raw[pos] == '\n' || raw[pos] == ':') {
			pos++
		}
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return nil, fmt.Errorf("decode value of %q: %w", key, err)
		}
		end := int(dec.InputOffset())
		// Trim trailing whitespace from [pos, end) if any.
		for end > pos && (raw[end-1] == ' ' || raw[end-1] == '\t' || raw[end-1] == '\r' || raw[end-1] == '\n') {
			end--
		}
		spans[key] = promptInputsByteSpan{start: pos, end: end}
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return spans, nil
}

// promptInputsDecodeSystem decodes Anthropic's `system` field, which may be a JSON string or an
// array of `{type: "text", text: "..."}` blocks.
func promptInputsDecodeSystem(raw []byte) ([]string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return nil, err
		}
		return []string{s}, nil
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(trimmed, &blocks); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Text != "" {
			out = append(out, b.Text)
		}
	}
	return out, nil
}

type promptInputsWireMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func (m promptInputsWireMessage) textBlocks() []string {
	trimmed := bytes.TrimSpace(m.Content)
	if len(trimmed) == 0 {
		return nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err == nil {
			return []string{s}
		}
		return nil
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(trimmed, &blocks); err != nil {
		return nil
	}
	var out []string
	for _, b := range blocks {
		if b.Type == "text" {
			out = append(out, b.Text)
		}
	}
	return out
}

func promptInputsCutTag(text, openTag, closeTag string) (cutBytes int, remainder string, found bool) {
	start := strings.Index(text, openTag)
	if start < 0 {
		return 0, text, false
	}
	rest := text[start+len(openTag):]
	endRel := strings.Index(rest, closeTag)
	if endRel < 0 {
		return 0, text, false
	}
	end := start + len(openTag) + endRel + len(closeTag)
	return end - start, text[:start] + text[end:], true
}

var (
	promptInputsSkillBulletRe = regexp.MustCompile(`(?m)^-[ \t]+([a-z0-9][a-z0-9._-]*(?::[a-z0-9][a-z0-9._-]*)?)(?::[ \t]|[ \t]*$)`)
	promptInputsSkillXMLRe    = regexp.MustCompile(`<name>\s*([a-z0-9][a-z0-9._:-]*)\s*</name>`)
)

// promptInputsExtractSkillsFromText finds a `<skills>...</skills>` or `<available_skills>...</available_skills>`
// block in system text, returning its byte length, the skill names inside it, and the text with
// that block removed.
func promptInputsExtractSkillsFromText(system string) (skillBytes int, skills []string, remainder string) {
	for _, pair := range [][2]string{
		{"<skills>", "</skills>"},
		{"<available_skills>", "</available_skills>"},
	} {
		start := strings.Index(system, pair[0])
		if start < 0 {
			continue
		}
		rest := system[start+len(pair[0]):]
		endRel := strings.Index(rest, pair[1])
		if endRel < 0 {
			continue
		}
		end := start + len(pair[0]) + endRel + len(pair[1])
		block := system[start:end]
		return len(block), promptInputsParseSkillNames(block), system[:start] + system[end:]
	}
	return 0, nil, system
}

func promptInputsParseSkillNames(block string) []string {
	var names []string
	for _, m := range promptInputsSkillXMLRe.FindAllStringSubmatch(block, -1) {
		names = append(names, m[1])
	}
	for _, m := range promptInputsSkillBulletRe.FindAllStringSubmatch(block, -1) {
		names = append(names, m[1])
	}
	return promptInputsUniqueSorted(names)
}

// promptInputsExtractClaudeSkillReminder extracts the skill list Claude Code injects in a
// `<system-reminder>` block ("The following skills are available for use with the Skill tool:").
func promptInputsExtractClaudeSkillReminder(text string) (skillBytes int, skills []string, remainder string) {
	const needle = "The following skills are available"
	idx := strings.Index(text, needle)
	if idx < 0 {
		return 0, nil, text
	}
	// If wrapped in <system-reminder>...</system-reminder>, take the enclosing reminder block.
	openIdx := strings.LastIndex(text[:idx], "<system-reminder>")
	if openIdx >= 0 {
		closeRel := strings.Index(text[idx:], "</system-reminder>")
		if closeRel >= 0 {
			end := idx + closeRel + len("</system-reminder>")
			block := text[openIdx:end]
			return len(block), promptInputsParseSkillNames(block), text[:openIdx] + text[end:]
		}
	}
	block := text[idx:]
	return len(block), promptInputsParseSkillNames(block), text[:idx]
}

// promptInputsExtractSkillsFromTools looks for a `Skill` tool definition in `tools` whose
// description carries the available skill list (Claude Code's alternate placement).
func promptInputsExtractSkillsFromTools(toolsRaw []byte) (int, []string) {
	var tools []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(toolsRaw, &tools); err != nil {
		return 0, nil
	}
	for _, tool := range tools {
		if tool.Name != "Skill" {
			continue
		}
		if b, sk, _ := promptInputsExtractSkillsFromText(tool.Description); b > 0 {
			return b, sk
		}
		if b, sk, _ := promptInputsExtractClaudeSkillReminder(tool.Description); b > 0 {
			return b, sk
		}
	}
	return 0, nil
}

func promptInputsIsSystemReminderOnly(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return true
	}
	rem := trimmed
	for {
		_, stripped, found := promptInputsCutTag(rem, "<system-reminder>", "</system-reminder>")
		if !found {
			break
		}
		rem = strings.TrimSpace(stripped)
	}
	return rem == ""
}

func promptInputsUniqueSorted(items []string) []string {
	if len(items) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	sort.Strings(out)
	return out
}

// promptInputsNormalizeSkillName strips a Claude Code plugin namespace prefix (`legion:`,
// `claude-envoy:`) so the skill set can be compared directly with scripts/pi-plugin-prepack.sh.
func promptInputsNormalizeSkillName(name string) string {
	if _, after, ok := strings.Cut(name, ":"); ok {
		return after
	}
	return name
}

func TestPromptInputsSplitsRequestBytes(t *testing.T) {
	t.Parallel()
	const legion = "Prompt: café\nInstructions."
	const harness = "Harness."
	const skillList = "<available_skills><skill><name>entry</name></skill></available_skills>"
	const injected = "<dispatch-first-skill>Search first.</dispatch-first-skill>"
	const task = "Read README.md."
	const tools = `[{"name":"read","input_schema":{"type":"object"}}]`
	system, _ := json.Marshal([]map[string]string{{"type": "text", "text": harness}, {"type": "text", "text": legion + skillList}})
	messages, _ := json.Marshal([]map[string]string{{"role": "user", "content": injected}, {"role": "user", "content": task}})
	const format = " {\n\"model\": \"fixture\", \"system\" : %s, \"messages\": %s, \"tools\": %s  } \n"
	framing := fmt.Sprintf(format, "", "", "")
	raw := fmt.Appendf(nil, format, system, messages, tools)
	got, err := promptInputsSplitRequest(raw, legion)
	if err != nil {
		t.Fatal(err)
	}
	if got.WireSystemBytes != len(system) || got.WireMessagesBytes != len(messages) ||
		got.WireToolsBytes != len(tools) || got.FramingBytes != len(framing) || got.TotalRequestBytes != len(raw) {
		t.Fatalf("incorrect wire partition: %+v", got)
	}
	if got.SystemTextBytes != len(legion)+len(harness) || got.LegionPromptBytes != len(legion) ||
		got.HarnessSystemBytes != len(harness) || !got.LegionPromptFound ||
		got.SkillListBytes != len(skillList) || len(got.Skills) != 1 || got.Skills[0] != "entry" ||
		got.DispatchFirstBytes != len(injected) || got.FirstMessageBytes != len(task) || got.OtherMessagesBytes != 0 {
		t.Fatalf("incorrect input partition: %+v", got)
	}
}

func promptInputsOpenCapture(t *testing.T, path string) []byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	z, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	data, err := io.ReadAll(z)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestPromptInputsSplitsCapturedRequests(t *testing.T) {
	t.Parallel()
	paths, err := filepath.Glob("testdata/prompt-inputs/f1*/*.json.gz")
	if err != nil || len(paths) == 0 {
		t.Fatalf("find captured first requests: %v (paths %v)", err, paths)
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw := promptInputsOpenCapture(t, path)
			prompt := promptInputsOpenCapture(t, strings.TrimSuffix(path, ".json.gz")+".md.gz")
			got, err := promptInputsSplitRequest(raw, string(prompt))
			if err != nil {
				t.Fatal(err)
			}
			var wire struct {
				System, Tools, Messages json.RawMessage
			}
			if err := json.Unmarshal(raw, &wire); err != nil {
				t.Fatal(err)
			}
			if len(wire.System) != got.WireSystemBytes || len(wire.Tools) != got.WireToolsBytes || len(wire.Messages) != got.WireMessagesBytes ||
				got.WireSystemBytes+got.WireToolsBytes+got.WireMessagesBytes+got.FramingBytes != len(raw) {
				t.Fatalf("wire partitions do not account for the %d-byte request: %+v", len(raw), got)
			}
			if !got.LegionPromptFound || got.DispatchFirstBytes == 0 || got.FirstMessageBytes == 0 || got.SkillListBytes == 0 ||
				got.ToolDefinitionsBytes == 0 || got.HarnessSystemBytes+got.LegionPromptBytes != got.SystemTextBytes {
				t.Fatalf("missing or overlapping prompt content: %+v", got)
			}
		})
	}
}
