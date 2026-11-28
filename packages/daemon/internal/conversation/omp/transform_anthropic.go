package omp

import (
	"bytes"
	"cmp"
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/conversation"
)

// selectAnthropicCompaction chooses the wire shape before file notes and
// developer placement. The native payload retains the opaque signed state.
func selectAnthropicCompaction(m *conversation.Message, t target) {
	if m.Native == nil || m.Native.Kind != conversation.AnthropicCompaction {
		return
	}
	var state struct {
		Signature        json.RawMessage `json:"signature"`
		EncryptedContent json.RawMessage `json:"encryptedContent"`
	}
	if t.anthropicReplay && m.Native.Provider == t.provider &&
		json.Unmarshal(m.Native.Payload, &state) == nil &&
		(compactionString(state.Signature) != "" || compactionString(state.EncryptedContent) != "") {
		m.Role, m.Content = conversation.Assistant, nil
		return
	}
	m.Native = nil
}

func compactionString(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

// An assistant can carry the compaction block before its other output.
func assistantAnthropicCompaction(raw json.RawMessage, t target) *conversation.Native {
	if t.api != conversation.AnthropicMessages || !t.anthropicReplay {
		return nil
	}
	var state struct {
		Provider         string  `json:"provider"`
		Content          string  `json:"content"`
		Signature        *string `json:"signature"`
		EncryptedContent *string `json:"encryptedContent"`
	}
	if json.Unmarshal(raw, &state) != nil || state.Provider != t.provider || state.Content == "" ||
		(state.Signature == nil && state.EncryptedContent == nil) {
		return nil
	}
	return &conversation.Native{Kind: conversation.AnthropicCompaction, Provider: state.Provider, Payload: raw}
}

// The encoder keeps reasoning from the last fallback segment with reasoning.
// The input already reflects the common replay filter.
func collapseFallbackReasoning(blocks []*conversation.Block) {
	segment, lastReasoning := 0, 0
	for _, b := range blocks {
		if b == nil {
			continue
		}
		switch b.Type {
		case conversation.AnthropicFallback:
			segment++
		case conversation.Thinking, conversation.RedactedThinking:
			lastReasoning = segment
		}
	}
	if segment == 0 {
		return
	}
	segment = 0
	for i, b := range blocks {
		if b == nil {
			continue
		}
		switch b.Type {
		case conversation.AnthropicFallback:
			segment++
			blocks[i] = nil
		case conversation.Thinking, conversation.RedactedThinking:
			if segment != lastReasoning {
				blocks[i] = nil
			}
		}
	}
}

type compactionFileNote struct {
	text  string
	after float64
}

type anthropicCompactionFiles struct {
	exact  []compactionFileNote
	legacy string
}

// beforeTurn runs after pairing any open tool call. A tool-result run never
// advances the exact-tail clock. A native summary queues notes instead.
func (f *anthropicCompactionFiles) beforeTurn(out []conversation.Message, m conversation.Message, source llmMessage) []conversation.Message {
	if f == nil {
		return out
	}
	if source.role != "assistant" && m.Native != nil && m.Native.Kind == conversation.AnthropicCompaction {
		var state struct {
			FilesText     json.RawMessage `json:"filesText"`
			RetainedFiles json.RawMessage `json:"retainedFiles"`
		}
		_ = json.Unmarshal(m.Native.Payload, &state)
		text := compactionString(state.FilesText)
		f.exact = f.exact[:0]
		if source.exactCompactionTail {
			var retained []json.RawMessage
			if json.Unmarshal(state.RetainedFiles, &retained) == nil {
				for _, raw := range retained {
					var note struct {
						Text  *string  `json:"text"`
						After *float64 `json:"after"`
					}
					if json.Unmarshal(raw, &note) == nil && note.Text != nil && note.After != nil {
						f.exact = append(f.exact, compactionFileNote{*note.Text, *note.After})
					}
				}
			}
			if text != "" && source.timestamp != nil {
				f.exact = append(f.exact, compactionFileNote{text, *source.timestamp})
			}
			slices.SortStableFunc(f.exact, func(a, b compactionFileNote) int {
				return cmp.Compare(a.after, b.after)
			})
		} else {
			f.legacy = text
		}
		return out
	}
	if source.timestamp != nil {
		out = f.flushExact(out, *source.timestamp)
	}
	if m.Role == conversation.User || m.Role == anthropicDeveloper {
		out = f.flushLegacy(out)
	}
	return out
}

func (f *anthropicCompactionFiles) flushExact(out []conversation.Message, before float64) []conversation.Message {
	for len(f.exact) > 0 && f.exact[0].after < before {
		out = appendCompactionFileNote(out, f.exact[0].text)
		f.exact = f.exact[1:]
	}
	return out
}

func (f *anthropicCompactionFiles) flushLegacy(out []conversation.Message) []conversation.Message {
	if f == nil || f.legacy == "" {
		return out
	}
	out = appendCompactionFileNote(out, f.legacy)
	f.legacy = ""
	return out
}

func (f *anthropicCompactionFiles) finish(out []conversation.Message) []conversation.Message {
	if f == nil {
		return out
	}
	return f.flushLegacy(f.flushExact(out, math.Inf(1)))
}

func appendCompactionFileNote(out []conversation.Message, text string) []conversation.Message {
	if blank(text) {
		return out
	}
	return append(out, anthropicUserText(text))
}

// shapeAnthropicHistory applies the encoder after tool pairing. The neutral model
// keeps individual tool results; the sender encodes each adjacent run as one user turn.
func shapeAnthropicHistory(messages []conversation.Message, t target) []conversation.Message {
	var out []conversation.Message
	var attachments []conversation.Block
	for i, m := range messages {
		switch m.Role {
		case conversation.Assistant:
			m.Content = anthropicToolUsesLast(m.Content)
		case conversation.Tool:
			result := &m.Content[0]
			result.Content = anthropicInput(result.Content, t.textOnly)
			if result.IsError {
				kept := result.Content[:0]
				for _, b := range result.Content {
					if b.Type == conversation.Image {
						attachments = append(attachments, b)
					} else {
						kept = append(kept, b)
					}
				}
				result.Content = kept
				if len(result.Content) == 0 {
					result.Content = []conversation.Block{{Type: conversation.Text, Text: "Tool failed with no output."}}
				}
			}
		}
		out = append(out, m)
		if len(attachments) > 0 && (i+1 == len(messages) || messages[i+1].Role != conversation.Tool) {
			withImages := anthropicUserText("Attached image(s) from the tool result(s) above:")
			withImages.Content = append(withImages.Content, attachments...)
			out = append(out, withImages)
			attachments = attachments[:0]
		}
	}
	out = placeAnthropicDeveloperTurns(out, t)
	return mergeAndSeparateAnthropicAssistants(out)
}

// The pin places developer messages before folding a compaction or inserting
// a Continue turn. Neither repair can give a developer system authority.
func mergeAndSeparateAnthropicAssistants(messages []conversation.Message) []conversation.Message {
	for i := len(messages) - 2; i >= 0; i-- {
		current, next := &messages[i], &messages[i+1]
		if current.Role == conversation.Assistant && next.Role == conversation.Assistant &&
			current.Native != nil && current.Native.Kind == conversation.AnthropicCompaction &&
			len(current.Content) == 0 && len(current.Native.Items) == 0 {
			if next.Native == nil {
				next.Native = current.Native
			} else {
				// The pin folds a standalone compaction into the next assistant,
				// including when that turn already opens with a compaction block.
				folded := *current.Native
				folded.Items = append([]json.RawMessage{next.Native.Payload}, next.Native.Items...)
				next.Native = &folded
			}
			messages = slices.Delete(messages, i, i+1)
		}
	}
	pads := 0
	for i := 1; i < len(messages); i++ {
		if messages[i-1].Role == conversation.Assistant && messages[i].Role == conversation.Assistant {
			pads++
		}
	}
	if pads == 0 {
		return messages
	}
	out := slices.Grow(messages, pads)[:len(messages)+pads]
	// Walk backwards so the new slots cannot overwrite an unread turn.
	at := len(out) - 1
	for i := len(messages) - 1; i >= 0; i-- {
		out[at] = messages[i]
		at--
		if i > 0 && messages[i-1].Role == conversation.Assistant && messages[i].Role == conversation.Assistant {
			out[at] = anthropicUserText("Continue.")
			at--
		}
	}
	return out
}

// anthropicDeveloper is an internal marker for a user projection eligible for
// system placement. It never leaves shapeAnthropicHistory.
const anthropicDeveloper conversation.Role = "anthropic-developer"

// anthropicControlSupport is the catalog's direct Anthropic policy
// (classes/anthropic.kdl:283–357). These models enable system turns, tool
// changes, and turn scoping together. Per-message effort starts separately.
func anthropicControlSupport(model string) (system, effort bool) {
	family, major, minor := claudeFamilyRevision(model)
	switch family {
	case "fable", "mythos":
		return major >= 5, major > 5 || major == 5 && minor >= 1
	case "sonnet":
		supported := major > 5 || major == 5 && minor >= 5
		return supported, supported
	case "haiku":
		supported := major == 5 && minor >= 5
		return supported, supported
	case "opus":
		return major > 4 || major == 4 && minor >= 8, major >= 5
	}
	return false, false
}

func markAnthropicDeveloper(msg *conversation.Message, attribution string, raw json.RawMessage, stringInput bool, t target) {
	var payload struct {
		Type        string  `json:"type"`
		ClearAt     *string `json:"clearAt"`
		Effort      *string `json:"effort"`
		ToolChanges []struct {
			Type conversation.BlockType `json:"type"`
			Name string                 `json:"name"`
		} `json:"toolChanges"`
	}
	hasControls := len(raw) != 0 && json.Unmarshal(raw, &payload) == nil && payload.Type == "anthropicMessage" &&
		(payload.ClearAt != nil || payload.Effort != nil || len(payload.ToolChanges) > 0)
	if attribution != "user" || hasControls {
		msg.Role = anthropicDeveloper
	}
	if !hasControls {
		return
	}
	var controls conversation.AnthropicControls
	if payload.Effort != nil {
		controls.Effort = *payload.Effort
	}
	if payload.ClearAt != nil && *payload.ClearAt == "next_user_message" {
		controls.ClearAt = *payload.ClearAt
		if (stringInput || t.textOnly) && t.anthropicEffort && payload.Effort != nil && payload.ToolChanges == nil {
			// The pin cannot split a string into scoped text and controls.
			controls.ClearAt = ""
		}
	}
	// Keep control-only turns through pairing, including where the target does
	// not support the individual fields. Placement resolves their final shape.
	msg.Native = anthropicMessageControls(controls)
	if t.anthropicSystem {
		for _, change := range payload.ToolChanges {
			if change.Type == conversation.ToolAddition || change.Type == conversation.ToolRemoval {
				msg.Content = append(msg.Content, conversation.Block{Type: change.Type, ToolName: change.Name})
			}
		}
	}
}

func anthropicMessageControls(controls conversation.AnthropicControls) *conversation.Native {
	return &conversation.Native{Kind: conversation.AnthropicMessageControls, Provider: "anthropic", Controls: &controls}
}

// placeAnthropicDeveloperTurns walks backwards: an upgraded next turn prevents
// the current candidate from also becoming a system turn. Tool result runs
// count as a user turn, as they do after sender-side grouping.
// Read rebuilds history for a new user prompt, so a trailing developer turn
// has a user, not the end of the request, as its next neighbour.
func placeAnthropicDeveloperTurns(out []conversation.Message, t target) []conversation.Message {
	for i := len(out) - 1; i >= 0; i-- {
		msg := &out[i]
		if msg.Role != anthropicDeveloper {
			continue
		}
		msg.Role = conversation.User
		var controls conversation.AnthropicControls
		if msg.Native != nil && msg.Native.Kind == conversation.AnthropicMessageControls {
			controls = *msg.Native.Controls
			msg.Native = nil
		}
		if !t.anthropicSystem {
			continue
		}
		followsUser := i > 0 && (out[i-1].Role == conversation.User || out[i-1].Role == anthropicDeveloper || out[i-1].Role == conversation.Tool)
		beforeAssistant := i+1 < len(out) && out[i+1].Role == conversation.Assistant
		effortOnly := controls.Effort != "" && len(msg.Content) == 0
		if !effortOnly && (!followsUser || !beforeAssistant || slices.ContainsFunc(msg.Content, func(b conversation.Block) bool {
			return b.Type == conversation.Image
		})) {
			continue
		}
		msg.Role = conversation.System
		if !t.anthropicEffort {
			controls.Effort = ""
		}
		toolStart := len(msg.Content)
		for toolStart > 0 && msg.Content[toolStart-1].Type != conversation.Text {
			toolStart--
		}
		if controls.ClearAt != "" && (controls.Effort != "" || toolStart < len(msg.Content)) {
			if toolStart > 0 {
				change := conversation.Message{Role: conversation.System}
				if toolStart < len(msg.Content) {
					change.Content = msg.Content[toolStart:]
				}
				msg.Content = msg.Content[:toolStart:toolStart]
				msg.Native = anthropicMessageControls(conversation.AnthropicControls{ClearAt: controls.ClearAt})
				if controls.Effort != "" {
					change.Native = anthropicMessageControls(conversation.AnthropicControls{Effort: controls.Effort})
				}
				out = slices.Insert(out, i+1, change)
				continue
			}
			controls.ClearAt = ""
		}
		if controls.ClearAt != "" || controls.Effort != "" {
			msg.Native = anthropicMessageControls(controls)
		}
	}
	return out
}

func anthropicUserText(text string) conversation.Message {
	return conversation.Message{Role: conversation.User, Content: []conversation.Block{{Type: conversation.Text, Text: text}}}
}

// anthropicToolUsesLast moves every call to the end only when a later block is
// neither kind of thinking. Both partitions keep their original order.
func anthropicToolUsesLast(blocks []conversation.Block) []conversation.Block {
	seenCall := false
	for _, b := range blocks {
		switch b.Type {
		case conversation.ToolCall:
			seenCall = true
		case conversation.Thinking, conversation.RedactedThinking:
		default:
			if seenCall {
				ordered := make([]conversation.Block, 0, len(blocks))
				for _, b := range blocks {
					if b.Type != conversation.ToolCall {
						ordered = append(ordered, b)
					}
				}
				for _, b := range blocks {
					if b.Type == conversation.ToolCall {
						ordered = append(ordered, b)
					}
				}
				return ordered
			}
		}
	}
	return blocks
}

// anthropicInput shapes saved prompt and tool-result blocks. A new user prompt
// is not part of the saved history; its sender validates any new attachments.
func anthropicInput(blocks []conversation.Block, textOnly bool) []conversation.Block {
	kept := blocks[:0]
	var sawText, sawImage bool
	for _, b := range blocks {
		if b.Type == conversation.Text {
			if blank(b.Text) {
				continue
			}
			sawText = true
		} else if b.ImageFileID != "" || b.ImageURL != "" {
			sawImage = true
		} else {
			mime := strings.ToLower(strings.TrimFunc(b.MediaType, isJSSpace))
			if mime == "image/jpg" {
				mime = "image/jpeg"
			}
			switch mime {
			case "image/jpeg", "image/png", "image/gif", "image/webp":
				b.MediaType = mime
				sawImage = true
			default:
				b = conversation.Block{Type: conversation.Text, Text: "[unsupported image: " + b.MediaType + "]"}
			}
		}
		kept = append(kept, b)
	}
	if len(kept) == 0 {
		return nil
	}
	if textOnly && len(kept) > 1 {
		var joined strings.Builder
		joined.WriteString(kept[0].Text)
		for _, b := range kept[1:] {
			joined.WriteByte('\n')
			joined.WriteString(b.Text)
		}
		return []conversation.Block{{Type: conversation.Text, Text: joined.String()}}
	}
	if sawImage && !sawText {
		return append([]conversation.Block{{Type: conversation.Text, Text: "(see attached image)"}}, kept...)
	}
	return kept
}

// wellFormedAnthropicArguments replaces just lone surrogate escapes in value
// strings. The encoder leaves keys and the bytes of well-formed inputs alone.
func wellFormedAnthropicArguments(raw json.RawMessage) json.RawMessage {
	if !bytes.Contains(raw, []byte(`\u`)) {
		return raw
	}
	var out json.RawMessage
	copied := 0
	for next := 0; next < len(raw); {
		offset := bytes.IndexByte(raw[next:], '"')
		if offset < 0 {
			break
		}
		start := next + offset + 1
		end := start
		for end < len(raw) && raw[end] != '"' {
			if raw[end] == '\\' {
				end++
			}
			end++
		}
		if end >= len(raw) {
			break
		}
		next = end + 1
		after := bytes.TrimLeft(raw[next:], " \t\r\n")
		if len(after) > 0 && after[0] == ':' {
			continue
		}
		for i := start; i < end; {
			if raw[i] != '\\' {
				i++
				continue
			}
			unit := surrogateEscape(raw[i:end])
			if unit == 0 {
				i += 2
				continue
			}
			if unit < 0xDC00 && surrogateEscape(raw[i+6:end]) >= 0xDC00 {
				i += 12
				continue
			}
			out = append(out, raw[copied:i]...)
			out = append(out, "�"...)
			i += 6
			copied = i
		}
	}
	if copied == 0 {
		return raw
	}
	return append(out, raw[copied:]...)
}

func surrogateEscape(raw []byte) uint64 {
	if len(raw) < 6 || raw[0] != '\\' || raw[1] != 'u' {
		return 0
	}
	unit, err := strconv.ParseUint(string(raw[2:6]), 16, 16)
	if err != nil || unit < 0xD800 || unit > 0xDFFF {
		return 0
	}
	return unit
}
