package omp

import (
	"encoding/json"
	"strings"
)

type responsesHistoryItem struct {
	raw    json.RawMessage
	fields map[string]json.RawMessage
}

func (i responsesHistoryItem) stringField(key string) string {
	var value string
	_ = json.Unmarshal(i.fields[key], &value)
	return value
}

func (i responsesHistoryItem) clientBoundary() bool {
	switch i.stringField("type") {
	case "message", "additional_tools":
		return i.stringField("role") != "assistant"
	case "":
		return i.stringField("role") != "" && i.stringField("role") != "assistant"
	case "tool_search_output":
		return i.stringField("execution") != "server"
	case "input_text", "input_image", "input_file", "input_audio", "function_call_output", "custom_tool_call_output", "computer_call_output", "local_shell_call_output", "shell_call_output", "apply_patch_call_output", "mcp_approval_response", "compaction", "compaction_summary", "compaction_trigger", "item_reference":
		return true
	default:
		return false
	}
}

func (i responsesHistoryItem) emptyAssistantMessage() bool {
	if i.stringField("type") != "message" || i.stringField("role") != "assistant" {
		return false
	}
	var text string
	if json.Unmarshal(i.fields["content"], &text) == nil {
		return blank(text)
	}
	var parts []struct {
		Type, Text, Refusal string
	}
	_ = json.Unmarshal(i.fields["content"], &parts)
	for _, part := range parts {
		if (part.Type == "output_text" && !blank(part.Text)) || (part.Type == "refusal" && !blank(part.Refusal)) {
			return false
		}
	}
	return true
}

// shapeNativeResponsesItems makes request-independent changes to the native
// items. It leaves wire metadata and the saved Payload alone.
func shapeNativeResponsesItems(raw []json.RawMessage, assistant bool) []json.RawMessage {
	items := make([]responsesHistoryItem, 0, len(raw))
	type callKey struct{ kind, id string }
	var queues map[callKey][]bool
	for _, r := range raw {
		item := responsesHistoryItem{raw: r}
		if json.Unmarshal(r, &item.fields) != nil || item.fields == nil {
			continue
		}
		kind := item.stringField("type")
		switch kind {
		case "function_call", "custom_tool_call":
			name := item.stringField("name")
			bad := len(name) > 128 || !validToolCallName.MatchString(name)
			if queues == nil {
				queues = make(map[callKey][]bool)
			}
			key := callKey{kind, item.stringField("call_id")}
			queues[key] = append(queues[key], bad)
			if bad {
				continue
			}
		case "function_call_output", "custom_tool_call_output":
			key := callKey{strings.TrimSuffix(kind, "_output"), item.stringField("call_id")}
			if queue := queues[key]; len(queue) > 0 {
				queues[key] = queue[1:]
				if queue[0] {
					continue
				}
			}
		case "computer_call_output", "local_shell_call_output", "shell_call_output", "apply_patch_call_output":
		default:
			if item.clientBoundary() {
				clear(queues)
			}
		}
		switch kind {
		case "item_reference":
			continue
		case "image_generation_call":
			var id *string
			_ = json.Unmarshal(item.fields["id"], &id)
			if id == nil || item.stringField("result") == "" {
				continue
			}
		case "function_call":
			args := item.stringField("arguments")
			if blank(args) {
				item.fields["arguments"] = json.RawMessage(`"{}"`)
				item.raw, _ = json.Marshal(item.fields)
			}
		}
		items = append(items, item)
	}

	hasOutput, hasBlank := !assistant, false
	if assistant {
		for _, item := range items {
			if item.stringField("type") == "reasoning" {
				continue
			}
			if item.emptyAssistantMessage() {
				hasBlank = true
			} else {
				hasOutput = true
			}
		}
	}
	if !hasOutput {
		return nil
	}
	out := make([]json.RawMessage, 0, len(items))
	if !hasBlank {
		for _, item := range items {
			out = append(out, item.raw)
		}
		return out
	}
	var pending []json.RawMessage
	removed := false
	for _, item := range items {
		switch {
		case item.clientBoundary():
			pending = pending[:0]
			removed = false
			out = append(out, item.raw)
		case item.stringField("type") == "reasoning":
			if removed {
				pending = pending[:0]
			}
			removed = false
			pending = append(pending, item.raw)
		case item.emptyAssistantMessage():
			removed = true
		default:
			out = append(out, pending...)
			pending = pending[:0]
			removed = false
			out = append(out, item.raw)
		}
	}
	return out
}
