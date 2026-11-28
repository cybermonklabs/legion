package omp

import (
	"encoding/json"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/conversation"
)

// responsesInputHistory retains the eligible replay alternative beside the
// fallback user blocks (openai-shared.ts:2332–2357). Without an explicit
// provider, getOpenAIResponsesHistoryItems uses the current provider.
func responsesInputHistory(raw json.RawMessage, provider string) *conversation.Native {
	if len(raw) == 0 {
		return nil
	}
	var payload struct {
		Type     conversation.NativeKind
		Provider *string
		Items    []json.RawMessage
	}
	if json.Unmarshal(raw, &payload) != nil || payload.Type != conversation.OpenAIResponsesHistory ||
		payload.Items == nil || payload.Provider != nil && *payload.Provider != provider {
		return nil
	}
	return &conversation.Native{Kind: payload.Type, Provider: provider, Items: payload.Items, Payload: raw}
}

func responsesTextSignature(signature string) (id, phase string) {
	if !strings.HasPrefix(signature, "{") {
		return signature, ""
	}
	var parsed map[string]json.RawMessage
	var version float64
	var savedID *string
	if json.Unmarshal([]byte(signature), &parsed) != nil ||
		json.Unmarshal(parsed["v"], &version) != nil || version != 1 ||
		json.Unmarshal(parsed["id"], &savedID) != nil || savedID == nil {
		return signature, ""
	}
	_ = json.Unmarshal(parsed["phase"], &phase)
	if phase != "commentary" && phase != "final_answer" {
		phase = ""
	}
	return *savedID, phase
}

func responsesReasoningSignature(signature string) bool {
	var item map[string]json.RawMessage
	var kind string
	var id *string
	return signature != "" && json.Unmarshal([]byte(signature), &item) == nil &&
		json.Unmarshal(item["type"], &kind) == nil && kind == "reasoning" &&
		json.Unmarshal(item["id"], &id) == nil && id != nil
}

// Responses hoists assistant text out of a tool batch. Replayable reasoning
// items separate batches, but thinking retained only for synthesis does not.
func hoistResponsesText(blocks []conversation.Block) []conversation.Block {
	firstCall, move := -1, false
	for i, b := range blocks {
		switch b.Type {
		case conversation.Thinking:
			if b.Signature != "" {
				firstCall, move = -1, false
			}
		case conversation.ToolCall:
			if firstCall < 0 {
				firstCall = i
			}
		case conversation.Text:
			move = move || firstCall >= 0
		}
	}
	if !move {
		return blocks
	}
	ordered := make([]conversation.Block, firstCall, len(blocks))
	copy(ordered, blocks[:firstCall])
	for _, b := range blocks[firstCall:] {
		if b.Type != conversation.ToolCall {
			ordered = append(ordered, b)
		}
	}
	for _, b := range blocks[firstCall:] {
		if b.Type == conversation.ToolCall {
			ordered = append(ordered, b)
		}
	}
	return ordered
}

func responsesUserContent(blocks []conversation.Block) []conversation.Block {
	firstImage, reorder := -1, false
	for i, b := range blocks {
		if b.Type == conversation.Image {
			if firstImage < 0 {
				firstImage = i
			}
		} else if firstImage >= 0 {
			reorder = true
		}
	}
	if firstImage < 0 || !reorder {
		return blocks
	}
	ordered := make([]conversation.Block, 0, len(blocks))
	for _, b := range blocks {
		if b.Type == conversation.Text {
			ordered = append(ordered, b)
		}
	}
	for _, b := range blocks[firstImage:] {
		if b.Type == conversation.Image {
			ordered = append(ordered, b)
		}
	}
	return ordered
}

func responsesResultContent(blocks []conversation.Block) []conversation.Block {
	for _, b := range blocks {
		if b.Type == conversation.Image {
			return blocks
		}
	}
	if len(blocks) == 1 {
		return blocks
	}
	var output strings.Builder
	for i, b := range blocks {
		if i != 0 {
			output.WriteByte('\n')
		}
		output.WriteString(b.Text)
	}
	return []conversation.Block{{Type: conversation.Text, Text: output.String()}}
}

func shapeResponsesHistory(messages []conversation.Message) []conversation.Message {
	for i := range messages {
		m := &messages[i]
		switch m.Role {
		case conversation.User:
			m.Content = responsesUserContent(m.Content)
		case conversation.Assistant:
			m.Content = hoistResponsesText(m.Content)
		}
		for i := range m.Content {
			b := &m.Content[i]
			switch b.Type {
			case conversation.ToolCall:
				b.ToolCallID = responsesCallID(b.ToolCallID)
			case conversation.ToolResult:
				b.ToolCallID = responsesCallID(b.ToolCallID)
				b.Content = responsesResultContent(b.Content)
			}
		}
	}
	return messages
}
