package daemon

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"
)

// promptInputsDispatchFirstOpen and promptInputsDispatchFirstClose are the markers pi-envoy and
// claude-envoy wrap the injected dispatch-first skill in (packages/envoy-client/src/dispatch-first.ts).
const (
	promptInputsDispatchFirstOpen  = "<dispatch-first-skill>"
	promptInputsDispatchFirstClose = "</dispatch-first-skill>"
)

// The lines the two recorders answer a request with when it is not the fixture task's: the
// stand-in (prompt_inputs_live_test.go) and the gateway recorder that --reads uses
// (prompt_inputs_gateway_live_test.go). None of them is text of a skill.
const (
	promptInputsStandinWait  = "WAITING on the next fixture task."
	promptInputsGatewayWait  = "WAITING on the fixture task."
	promptInputsGatewayEnded = "WAITING: the fixture measurement has ended."
)

var promptInputsPlaceholders = []string{promptInputsStandinWait, promptInputsGatewayWait, promptInputsGatewayEnded}

// promptInputsDescriptionAsk is the start of the pinned Oh My Pi's skill-description request: the
// instruction of packages/coding-agent/src/prompts/skills/compress-description.md, then the line
// that names the skill. compressSkill (src/extensibility/skill-descriptions.ts) renders it with
// the skill's name and description and sends it as the one user message of a request with no
// tools. Oh My Pi stores the answer as the skill's description, and the skill list of every later
// session under the same agent directory gives that answer.
const promptInputsDescriptionAsk = "Compress into one routing hint of at most 12 words: WHEN to use; WHEN NOT only if it prevents confusion. " +
	"Retain recognizable trigger words. Do not restate the skill name or market it. Output one line without bullets or quotes.\n\nSkill: "

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
	// SkillDescriptions gives each listed skill's description as its entry gives it, "" where
	// the entry gives none.
	SkillDescriptions map[string]string `json:"skill_descriptions"`
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
		OtherMessagesBytes:   s.OtherMessagesBytes,
	}
}

// placeholderSkills returns, sorted, each listed skill whose description holds a line a recorder
// answers with. Such an entry measures a recorder's answer, not the skill.
func (s promptInputsSplitResult) placeholderSkills() []string {
	var held []string
	for name, description := range s.SkillDescriptions {
		if slices.ContainsFunc(promptInputsPlaceholders, func(line string) bool { return strings.Contains(description, line) }) {
			held = append(held, name)
		}
	}
	slices.Sort(held)
	return held
}

// promptInputsDescriptionRequest returns the skill name and description that raw, one Anthropic
// Messages request body, asks Oh My Pi's fast model to shorten. ok is false for every other
// request: one that offers a tool, holds more than one message, or whose message is not that ask.
// A role's request always offers tools, so it is never taken for a description request.
func promptInputsDescriptionRequest(raw []byte) (name, description string, ok bool) {
	var call struct {
		Tools    []json.RawMessage         `json:"tools"`
		Messages []promptInputsWireMessage `json:"messages"`
	}
	if json.Unmarshal(raw, &call) != nil || len(call.Tools) != 0 || len(call.Messages) != 1 || call.Messages[0].Role != "user" {
		return "", "", false
	}
	blocks := call.Messages[0].textBlocks()
	if len(blocks) != 1 {
		return "", "", false
	}
	rest, ok := strings.CutPrefix(blocks[0], promptInputsDescriptionAsk)
	if !ok {
		return "", "", false
	}
	name, description, ok = strings.Cut(rest, "\nDescription: ")
	if !ok || name == "" || strings.Contains(name, "\n") {
		return "", "", false
	}
	return name, description, true
}

// promptInputsDescriptionAnswer is what both recorders answer a skill-description request with:
// the preview the pinned Oh My Pi itself lists for a skill while no answer is stored
// (previewSkillDescription, src/extensibility/skill-descriptions.ts), computed as that function
// computes it, in UTF-16 code units. It depends on the description alone, so every request for one
// skill gets the same answer, and a skill list gives the same entry whether or not the answer was
// stored before the list was rendered. Oh My Pi stores it as the shortened description because of
// validCompression in the same file: it takes an answer that, trimmed, is not empty, holds no line
// break and is at most MAX_COMPRESSED_CHARS (160) code units long. This answer is one line of at
// most MAX_PREVIEW_CHARS (100) code units, and it is empty only for an empty description.
func promptInputsDescriptionAnswer(description string) string {
	const maxChars, minCut = 100, 40 // MAX_PREVIEW_CHARS and MIN_PREVIEW_CUT
	// description.replace(/\s+/g, " ").trim()
	text := strings.Join(strings.FieldsFunc(description, promptInputsJSSpace), " ")
	units := utf16.Encode([]rune(text))
	if len(units) <= maxChars {
		return text
	}
	boundary := units[:maxChars-1]
	// boundary.match(/^.*?[.!?](?=\s|$)/): the first stop that ends the boundary or comes before a
	// space. The collapse above left no other white space.
	for i, unit := range boundary {
		if (unit == '.' || unit == '!' || unit == '?') && (i == len(boundary)-1 || boundary[i+1] == ' ') {
			if i+1 >= minCut {
				return string(utf16.Decode(boundary[:i+1]))
			}
			break
		}
	}
	// boundary.lastIndexOf(" "), taken where it is at least minCut. The collapse left no white
	// space before a space, so trimEnd() changes nothing.
	cut := boundary
	for space := len(boundary) - 1; space >= minCut; space-- {
		if boundary[space] == ' ' {
			cut = boundary[:space]
			break
		}
	}
	return string(utf16.Decode(cut)) + "…"
}

// promptInputsJSSpace is JavaScript's \s, the white space previewSkillDescription collapses.
func promptInputsJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
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
	messageSkills := map[string]string{}
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
					maps.Copy(messageSkills, sk)
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
	res.Skills = slices.Sorted(maps.Keys(skills))
	res.SkillDescriptions = skills
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
	promptInputsSkillBulletRe   = regexp.MustCompile(`(?m)^-[ \t]+([a-z0-9][a-z0-9._-]*(?::[a-z0-9][a-z0-9._-]*)?)(?::[ \t]+(.*)|[ \t]*)$`)
	promptInputsSkillXMLRe      = regexp.MustCompile(`<name>\s*([a-z0-9][a-z0-9._:-]*)\s*</name>`)
	promptInputsSkillXMLEntryRe = regexp.MustCompile(`(?s)<name>\s*([a-z0-9][a-z0-9._:-]*)\s*</name>\s*<description>\s*(.*?)\s*</description>`)
)

// promptInputsExtractSkillsFromText finds a `<skills>...</skills>` or `<available_skills>...</available_skills>`
// block in system text, returning its byte length, the skills inside it with their descriptions,
// and the text with that block removed.
func promptInputsExtractSkillsFromText(system string) (skillBytes int, skills map[string]string, remainder string) {
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
		return len(block), promptInputsParseSkills(block), system[:start] + system[end:]
	}
	return 0, nil, system
}

// promptInputsParseSkills returns each skill a skill list names, with the description its entry
// gives: a `- <name>: <description>` line, as Oh My Pi and Claude Code write it, or a `<name>`
// element and the `<description>` element after it. A name with no description maps to "".
func promptInputsParseSkills(block string) map[string]string {
	skills := map[string]string{}
	for _, m := range promptInputsSkillXMLRe.FindAllStringSubmatch(block, -1) {
		skills[m[1]] = ""
	}
	for _, m := range promptInputsSkillXMLEntryRe.FindAllStringSubmatch(block, -1) {
		skills[m[1]] = m[2]
	}
	for _, m := range promptInputsSkillBulletRe.FindAllStringSubmatch(block, -1) {
		skills[m[1]] = strings.TrimRight(m[2], " \t")
	}
	return skills
}

// promptInputsExtractClaudeSkillReminder extracts the skill list Claude Code injects in a
// `<system-reminder>` block ("The following skills are available for use with the Skill tool:").
func promptInputsExtractClaudeSkillReminder(text string) (skillBytes int, skills map[string]string, remainder string) {
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
			return len(block), promptInputsParseSkills(block), text[:openIdx] + text[end:]
		}
	}
	block := text[idx:]
	return len(block), promptInputsParseSkills(block), text[:idx]
}

// promptInputsExtractSkillsFromTools looks for a `Skill` tool definition in `tools` whose
// description carries the available skill list (Claude Code's alternate placement).
func promptInputsExtractSkillsFromTools(toolsRaw []byte) (int, map[string]string) {
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

// Text in a message other than the dispatch-first skill and the first message reaches the row as
// its own part, and the row's total counts it. Here that is an earlier answer and the system-role
// message Claude Code sends after the first message. The wire framing stays out of the total.
func TestPromptInputsRowCountsTheOtherMessages(t *testing.T) {
	t.Parallel()
	const system = "System text."
	const tools = `[{"name":"read"}]`
	const injected = "<dispatch-first-skill>Search first.</dispatch-first-skill>"
	const task = "Read README.md."
	const answer = "An earlier answer."
	const hook = "SessionStart:startup hook success: a summary of 16,131 characters in the real capture."
	messages, _ := json.Marshal([]map[string]any{
		{"role": "user", "content": injected},
		{"role": "user", "content": []map[string]string{{"type": "text", "text": task}}},
		{"role": "assistant", "content": answer},
		{"role": "system", "content": []map[string]string{{"type": "text", "text": hook}}},
	})
	raw := fmt.Appendf(nil, `{"model":"fixture","system":%q,"messages":%s,"tools":%s}`, system, messages, tools)
	split, err := promptInputsSplitRequest(raw, "")
	if err != nil {
		t.Fatal(err)
	}
	row := promptInputsRow{FirstRequest: split.toRequest(), RequiredReads: []promptInputsRead{{Path: "README.md", Bytes: 50}}}
	if row.FirstRequest.OtherMessagesBytes != len(answer)+len(hook) {
		t.Fatalf("the row's other messages are %d bytes, want %d: %+v", row.FirstRequest.OtherMessagesBytes, len(answer)+len(hook), row.FirstRequest)
	}
	want := len(system) + len(tools) + len(injected) + len(task) + len(answer) + len(hook) + 50
	if total := promptInputsTotal(row); total != want || total >= len(raw)+50 {
		t.Fatalf("total %d bytes, want %d, the decoded text and the read without the %d bytes of the request's wire", total, want, len(raw))
	}
	encoded, err := json.Marshal(row.FirstRequest)
	if err != nil || !bytes.Contains(encoded, fmt.Appendf(nil, `"other_messages_bytes":%d`, len(answer)+len(hook))) {
		t.Fatalf("the first request encodes as %s (%v), without its other messages", encoded, err)
	}
}

// A skill entry that gives a recorder's line as its description is found, by skill, whether the
// harness lists the skills in the system text (Oh My Pi) or in a message (Claude Code). An entry
// that only says WAITING in its own words is no recorder's line.
func TestPromptInputsFindsARecordersLineInTheSkillList(t *testing.T) {
	t.Parallel()
	for _, line := range promptInputsPlaceholders {
		list := "- alpha: Use when a reply ends with a WAITING line.\n- beta: " + line + "\n- gamma\n"
		system, _ := json.Marshal("System text.<skills>\n" + list + "</skills>")
		omp := fmt.Appendf(nil, `{"system":%s,"messages":[{"role":"user","content":"Task."}],"tools":[{"name":"read"}]}`, system)
		reminder, _ := json.Marshal("<system-reminder>\nThe following skills are available for use with the Skill tool:\n\n" + list + "</system-reminder>\nTask.")
		claude := fmt.Appendf(nil, `{"system":"System text.","messages":[{"role":"user","content":%s}],"tools":[{"name":"Read"}]}`, reminder)
		for harness, raw := range map[string][]byte{"omp": omp, "claude": claude} {
			split, err := promptInputsSplitRequest(raw, "")
			if err != nil {
				t.Fatal(err)
			}
			if held := split.placeholderSkills(); !slices.Equal(held, []string{"beta"}) || !slices.Equal(split.Skills, []string{"alpha", "beta", "gamma"}) {
				t.Fatalf("%s, %q: skills %v, recorder lines in %v, want beta alone", harness, line, split.Skills, held)
			}
		}
	}
}

// promptInputsOhMyPiTakes is validCompression of the pinned Oh My Pi
// (src/extensibility/skill-descriptions.ts), the rule an answer must meet to be stored as a
// skill's shortened description, and the stored text is the answer trimmed.
func promptInputsOhMyPiTakes(answer string) (string, bool) {
	line := strings.TrimFunc(answer, promptInputsJSSpace)
	return line, line != "" && !strings.ContainsAny(line, "\r\n") && len(utf16.Encode([]rune(line))) <= 160
}

// The answer to a description request is Oh My Pi's own preview of the description: white space
// collapsed, kept whole up to 100 UTF-16 code units, else cut at the first sentence end of at
// least 40 units, else at the last space at or past unit 40, else at unit 99, and marked with an
// ellipsis where it is cut. Each expectation is what previewSkillDescription of the pinned Oh My Pi
// returned for the same input under bun. Oh My Pi stores every answer as it is.
func TestPromptInputsDescriptionAnswerIsOhMyPisPreview(t *testing.T) {
	t.Parallel()
	hundred := strings.Repeat("abcd ", 19) + "abcde"
	for _, tc := range []struct{ description, want string }{
		{"  Use\twhen\u00a0a  run\n needs it.\u2003 ", "Use when a run needs it."},
		{hundred, hundred},
		{"Use when the proof reads every launch of the table, and when each part of the first request is counted.",
			"Use when the proof reads every launch of the table, and when each part of the first request is…"},
		{"Use when a proof measures what each launch puts before the model. Never use it for the real run or for anything else.",
			"Use when a proof measures what each launch puts before the model."},
		{"Use it now. Then read every file the launch names before the first act, and keep each byte count for the row.",
			"Use it now. Then read every file the launch names before the first act, and keep each byte count…"},
		{strings.Repeat("x", 112), strings.Repeat("x", 99) + "…"},
		{"Use e.g.for long text that keeps going and going past the forty unit mark! More words follow here to pass one hundred units.",
			"Use e.g.for long text that keeps going and going past the forty unit mark!"},
		{"Use 🙂🙂🙂🙂🙂 when the text holds pictures that count twice, and keep going until the cut falls near the end here",
			"Use 🙂🙂🙂🙂🙂 when the text holds pictures that count twice, and keep going until the cut falls…"},
	} {
		got := promptInputsDescriptionAnswer(tc.description)
		if got != tc.want {
			t.Errorf("answer to %q = %q, want %q", tc.description, got, tc.want)
		}
		if stored, ok := promptInputsOhMyPiTakes(got); !ok || stored != got {
			t.Errorf("Oh My Pi does not store %q as it is", got)
		}
	}
}

// promptInputsCapturedDescriptions reads fixture F1d (testdata/prompt-inputs/descriptions): the
// skill-description requests the stand-in answered in the run that captured F1, one request body
// a line. It returns each skill's description, and fails on a line that is no description request.
func promptInputsCapturedDescriptions(t *testing.T) map[string]string {
	t.Helper()
	descriptions := map[string]string{}
	for line := range bytes.Lines(promptInputsOpenCapture(t, "testdata/prompt-inputs/descriptions/requests.jsonl.gz")) {
		name, description, ok := promptInputsDescriptionRequest(line)
		if !ok {
			t.Fatalf("a captured request is no description request: %.300s", line)
		}
		descriptions[name] = description
	}
	if len(descriptions) == 0 {
		t.Fatal("no captured description request")
	}
	return descriptions
}

// Each skill-description request the pinned Oh My Pi sent is taken for one, and its answer is
// one Oh My Pi stores. A role's first request is never taken for one: none of F1 and F1c, nor the
// description ask with a tool offered. A description that holds the fixture task's words is still
// a description request, so a recorder that answers description requests first never keeps it as
// the role's request.
func TestPromptInputsRecognizesDescriptionRequests(t *testing.T) {
	t.Parallel()
	for name, description := range promptInputsCapturedDescriptions(t) {
		if stored, ok := promptInputsOhMyPiTakes(promptInputsDescriptionAnswer(description)); !ok || stored != promptInputsDescriptionAnswer(description) {
			t.Errorf("%s: Oh My Pi does not store the answer %q as it is", name, promptInputsDescriptionAnswer(description))
		}
	}
	paths, err := filepath.Glob("testdata/prompt-inputs/f1*/*.json.gz")
	if err != nil || len(paths) == 0 {
		t.Fatalf("find captured first requests: %v (paths %v)", err, paths)
	}
	for _, path := range paths {
		if _, _, ok := promptInputsDescriptionRequest(promptInputsOpenCapture(t, path)); ok {
			t.Errorf("%s: a role's first request is taken for a description request", path)
		}
	}
	// The words the fixture task starts with (promptInputsTask, in the e2e driver).
	const task = "Prompt inputs proof: gather the public context required before work."
	ask := func(tools string) []byte {
		message, _ := json.Marshal(promptInputsDescriptionAsk + "fixture-skill\nDescription: " + task)
		return fmt.Appendf(nil, `{"model":"m","max_tokens":1024,"messages":[{"role":"user","content":[{"type":"text","text":%s}]}]%s}`, message, tools)
	}
	if name, description, ok := promptInputsDescriptionRequest(ask("")); !ok || name != "fixture-skill" || description != task {
		t.Errorf("the ask for a description holding the task reads as %q, %q, %t", name, description, ok)
	}
	if _, _, ok := promptInputsDescriptionRequest(ask(`,"tools":[{"name":"read"}]`)); ok {
		t.Error("a request that offers a tool is taken for a description request")
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
	descriptions := promptInputsCapturedDescriptions(t)
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
			// The row accounts for all the decoded text of the request: the system text, the tool
			// definitions as sent, and every text block of every message.
			text := len(wire.Tools)
			systemTexts, err := promptInputsDecodeSystem(wire.System)
			if err != nil {
				t.Fatal(err)
			}
			for _, block := range systemTexts {
				text += len(block)
			}
			var messages []promptInputsWireMessage
			if err := json.Unmarshal(wire.Messages, &messages); err != nil {
				t.Fatal(err)
			}
			for _, message := range messages {
				for _, block := range message.textBlocks() {
					text += len(block)
				}
			}
			if total := promptInputsTotal(promptInputsRow{FirstRequest: got.toRequest()}); total != text {
				t.Fatalf("the row's total is %d bytes, and the request's text is %d bytes: %+v", total, text, got.toRequest())
			}
			if held := got.placeholderSkills(); len(held) != 0 {
				t.Fatalf("the skill list gives a recorder's line as the description of %v", held)
			}
			if filepath.Base(filepath.Dir(path)) != "f1" {
				return
			}
			// Oh My Pi lists each skill by its stored answer, or by its own preview where no answer
			// is stored: both are the stand-in's answer to that skill's description request.
			for name, entry := range got.SkillDescriptions {
				description, ok := descriptions[name]
				if !ok {
					t.Errorf("%s: no captured description request", name)
				} else if want := promptInputsDescriptionAnswer(description); entry != want {
					t.Errorf("%s: listed as %q, want the answer %q", name, entry, want)
				}
			}
		})
	}
}
