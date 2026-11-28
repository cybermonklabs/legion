package omp

import (
	"encoding/json"
	"fmt"
	"strings"
)

type responsesHistoryItem struct {
	raw             json.RawMessage
	fields          map[string]json.RawMessage
	kind            string
	keepReasoningID bool
}

func (i responsesHistoryItem) stringField(key string) string {
	value, _ := i.stringValue(key)
	return value
}

func (i responsesHistoryItem) stringValue(key string) (string, bool) {
	raw := i.fields[key]
	if !startsJSON(raw, '"') {
		return "", false
	}
	var value string
	err := json.Unmarshal(raw, &value)
	return value, err == nil
}

func startsJSON(raw json.RawMessage, token byte) bool {
	return len(raw) > 0 && raw[0] == token
}

func (i responsesHistoryItem) clientBoundary() bool {
	switch i.kind {
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
	if i.kind != "message" || i.stringField("role") != "assistant" {
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

// cleanReplayFields modifies only the decoded replay copy, not the raw journal
// payload. A reasoning ID may be kept for a computer call, subject to the
// sender's computer policy and final output pairing.
func (i *responsesHistoryItem) cleanReplayFields() {
	count := len(i.fields)
	changed := false
	switch i.kind {
	case "reasoning":
		for key, field := range i.fields {
			switch key {
			case "type":
				continue
			case "id":
				if i.keepReasoningID && startsJSON(field, '"') {
					continue
				}
			case "summary", "content":
				if startsJSON(field, '[') {
					continue
				}
			case "encrypted_content":
				if startsJSON(field, '"') || string(field) == "null" {
					continue
				}
			}
			delete(i.fields, key)
		}
	case "image_generation_call":
		id := i.stringField("id")
		if utf16Length(id) > 64 {
			i.fields["id"], _ = json.Marshal("ig_" + bunHash(id))
		}
		for key := range i.fields {
			switch key {
			case "id", "type", "result", "status":
			default:
				delete(i.fields, key)
			}
		}
		i.fields["status"] = json.RawMessage(`"completed"`)
		changed = true
	default:
		delete(i.fields, "internal_chat_message_metadata_passthrough")
		delete(i.fields, "created_by")
		if i.kind != "computer_call" || !startsJSON(i.fields["id"], '"') {
			delete(i.fields, "id")
		}
		switch i.kind {
		case "message", "function_call", "custom_tool_call", "compaction", "compaction_summary":
			delete(i.fields, "status")
		}
		switch i.kind {
		case "function_call":
			args := i.stringField("arguments")
			if replay := replayFunctionArguments(args); replay != args {
				i.fields["arguments"], _ = json.Marshal(replay)
				changed = true
			}
		case "shell_call_output":
			changed = i.cleanShellOutput()
		}
	}
	if changed || len(i.fields) != count {
		i.raw, _ = json.Marshal(i.fields)
	}
}

func (i *responsesHistoryItem) cleanShellOutput() bool {
	var entries []json.RawMessage
	if json.Unmarshal(i.fields["output"], &entries) != nil {
		return false
	}
	changed := false
	for index, entry := range entries {
		var fields map[string]json.RawMessage
		if json.Unmarshal(entry, &fields) != nil || fields["created_by"] == nil {
			continue
		}
		delete(fields, "created_by")
		entries[index], _ = json.Marshal(fields)
		changed = true
	}
	if changed {
		i.fields["output"], _ = json.Marshal(entries)
	}
	return changed
}

func replayFunctionArguments(raw string) string {
	if blank(raw) {
		return "{}"
	}
	if json.Valid([]byte(raw)) {
		return raw
	}
	const limit = 512
	if length := utf16Length(raw); length > limit {
		cut := 0
		for index, r := range raw {
			cut++
			if r > 0xFFFF {
				cut++
			}
			if cut > limit {
				raw = fmt.Sprintf("%s… [truncated %d chars]", raw[:index], length-limit)
				break
			}
		}
	}
	diagnostic, _ := json.Marshal(struct {
		Error string `json:"__parseError"`
		Raw   string `json:"__rawJson"`
	}{"invalid JSON", raw})
	return string(diagnostic)
}

// shapeNativeResponsesItems repairs and filters the replay copy. It leaves the
// saved Payload alone.
func shapeNativeResponsesItems(raw []json.RawMessage, assistant bool) []json.RawMessage {
	items := make([]responsesHistoryItem, 0, len(raw))
	type callKey struct{ kind, id string }
	var queues map[callKey][]bool
	// Track which reasoning items can be paired with a native computer call.
	// Only the sender knows whether the pair survives the final request.
	var reasoning []int
	for _, r := range raw {
		item := responsesHistoryItem{raw: r}
		if json.Unmarshal(r, &item.fields) != nil || item.fields == nil {
			continue
		}
		kind := item.stringField("type")
		item.kind = kind
		switch kind {
		case "function_call", "custom_tool_call":
			name := item.stringField("name")
			bad := len(name) > 128 || !validToolCallName.MatchString(name)
			if id, ok := item.stringValue("call_id"); ok {
				if queues == nil {
					queues = make(map[callKey][]bool)
				}
				key := callKey{kind, id}
				queues[key] = append(queues[key], bad)
			}
			if bad {
				continue
			}
		case "function_call_output", "custom_tool_call_output":
			if id, ok := item.stringValue("call_id"); ok {
				key := callKey{strings.TrimSuffix(kind, "_output"), id}
				if queue := queues[key]; len(queue) > 0 {
					queues[key] = queue[1:]
					if queue[0] {
						continue
					}
				}
			}
		case "computer_call_output", "local_shell_call_output", "shell_call_output", "apply_patch_call_output":
		default:
			if item.clientBoundary() {
				clear(queues)
			}
		}
		switch {
		case item.clientBoundary():
			reasoning = reasoning[:0]
		case kind == "reasoning":
			reasoning = append(reasoning, len(items))
		case kind == "computer_call" && startsJSON(item.fields["id"], '"') && len(reasoning) != 0:
			for _, index := range reasoning {
				items[index].keepReasoningID = true
			}
			reasoning = reasoning[:0]
		}
		switch kind {
		case "item_reference":
			continue
		case "image_generation_call":
			if !startsJSON(item.fields["id"], '"') || item.stringField("result") == "" {
				continue
			}
		}
		items = append(items, item)
	}
	for index := range items {
		items[index].cleanReplayFields()
	}

	hasOutput, hasBlank := !assistant, false
	if assistant {
		for _, item := range items {
			if item.kind == "reasoning" {
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
		case item.kind == "reasoning":
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
