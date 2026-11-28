package omp

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/conversation"
)

func TestResponsesHistoryShaping(t *testing.T) {
	const api = conversation.OpenAIResponses
	model := target{api: api, provider: "openai", model: "gpt-test"}
	image := block{Type: "image", MimeType: "image/png", Data: "eA=="}
	neutralImage := conversation.Block{Type: conversation.Image, MediaType: "image/png", ImageData: "eA=="}
	textBlock := func(s string) block { return block{Type: "text", Text: s} }
	var prompt = llmMessage{role: "user", blocks: []block{image, textBlock("  "), textBlock("first"), image, textBlock("last")}}
	var tool = llmMessage{role: "assistant", api: api, provider: model.provider, model: model.model, stopReason: "toolUse", blocks: []block{{Type: "toolCall", ID: "opaque|fc_a", Name: "read"}}}
	var output = llmMessage{role: "toolResult", toolCallID: "opaque|fc_a", toolName: "read", blocks: []block{textBlock("a"), image, textBlock(""), textBlock("b")}}
	promptWith := func(blocks ...conversation.Block) conversation.Message {
		return conversation.Message{Role: conversation.User, Content: blocks}
	}
	run := func(t *testing.T, target target, in ...llmMessage) []conversation.Message {
		t.Helper()
		got, err := toRequest(in, target)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	t.Run("vision prompt order and empty user", func(t *testing.T) {
		got := run(t, model, llmMessage{role: "user", blocks: []block{textBlock(" \t")}}, prompt)
		assertMessages(t, got, []conversation.Message{promptWith(text("first"), text("last"), neutralImage, neutralImage)})
	})
	t.Run("text only replaces images in place", func(t *testing.T) {
		textOnly := model
		textOnly.textOnly = true
		got := run(t, textOnly, prompt, tool, output)
		wantCall := call("opaque", "read", "null")
		wantCall.ItemID = "fc_a"
		assertMessages(t, got, []conversation.Message{
			promptWith(text(historyImageOmission), text("first"), text(historyImageOmission), text("last")),
			{Role: conversation.Assistant, API: api, Provider: model.provider, Model: model.model, Content: []conversation.Block{wantCall}},
			result("opaque", "read", "a\n"+historyImageOmission+"\n\nb", false),
		})
	})
	t.Run("vision output retains interleaving", func(t *testing.T) {
		got := run(t, model, tool, output)[1].Content[0].Content
		want := []conversation.Block{text("a"), neutralImage, text(""), text("b")}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("result parts = %+v, want %+v", got, want)
		}
	})
	t.Run("plain results join all text", func(t *testing.T) {
		output := output
		output.blocks = []block{textBlock("a"), textBlock(""), textBlock("b")}
		got := run(t, model, tool, output)
		assertMessages(t, got[1:], []conversation.Message{result("opaque", "read", "a\n\nb", false)})
		output.blocks = nil
		got = run(t, model, tool, output)
		assertMessages(t, got[1:], []conversation.Message{result("opaque", "read", "", false)})
	})
	t.Run("reasoning separates tool batches", func(t *testing.T) {
		tool := tool
		const signature = `{"type":"reasoning","id":"rs_fixture","encrypted_content":"opaque"}`
		tool.blocks = append(tool.blocks[:1:1], block{Type: "thinking", ThinkingSignature: signature}, textBlock("after reasoning"))
		got := run(t, model, tool, output)[0].Content
		wantCall := call("opaque", "read", "null")
		wantCall.ItemID = "fc_a"
		want := []conversation.Block{wantCall, thinking("", signature), text("after reasoning")}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("assistant blocks = %+v, want %+v", got, want)
		}
	})
	t.Run("phase survives only on the same Responses model", func(t *testing.T) {
		answer := llmMessage{role: "assistant", api: api, provider: model.provider, model: model.model, stopReason: "stop", blocks: []block{{Type: "text", Text: "answer", TextSignature: `{"v":1,"id":"msg_fixture","phase":"final_answer"}`}}}
		if got := run(t, model, answer)[0].Content[0].Phase; got != "final_answer" {
			t.Errorf("phase = %q, want final_answer", got)
		}
		other := model
		other.model = "gpt-other"
		if got := run(t, other, answer)[0].Content[0].Phase; got != "" {
			t.Errorf("foreign Responses phase = %q, want empty", got)
		}
		other = target{api: conversation.AnthropicMessages, provider: "anthropic", model: "claude-test"}
		answer.api, answer.provider, answer.model = other.api, other.provider, other.model
		if got := run(t, other, answer)[0].Content[0]; got.Phase != "" || got.ItemID != "" {
			t.Errorf("Anthropic text phase = %q, item ID = %q, want both empty", got.Phase, got.ItemID)
		}
	})
}

func TestNativeResponsesHistoryShaping(t *testing.T) {
	readItems := func(s string) []json.RawMessage {
		t.Helper()
		var items []json.RawMessage
		if err := json.Unmarshal([]byte(s), &items); err != nil {
			t.Fatal(err)
		}
		return items
	}
	for _, tc := range []struct{ name, input, want string }{
		{"malformed call pairs positionally", `[{"type":"function_call","call_id":"id","name":"bad name"},{"type":"function_call","call_id":"id","name":"read","arguments":"{}"},{"type":"function_call_output","call_id":"id","output":"discard"},{"type":"function_call_output","call_id":"id","output":"keep"}]`, `[{"type":"function_call","call_id":"id","name":"read","arguments":"{}"},{"type":"function_call_output","call_id":"id","output":"keep"}]`},
		{"only string IDs enter or consume a queue",
			`[{"type":"function_call","name":"bad call"},{"type":"function_call","name":"bad call","call_id":42},{"type":"function_call_output","call_id":"","output":"keep empty ID"},{"type":"function_call","name":"bad call","call_id":""},{"type":"function_call_output","call_id":null,"output":"keep null ID"},{"type":"function_call_output","output":"keep absent ID"},{"type":"function_call_output","call_id":"","output":"drop paired output"}]`,
			`[{"type":"function_call_output","call_id":"","output":"keep empty ID"},{"type":"function_call_output","call_id":null,"output":"keep null ID"},{"type":"function_call_output","output":"keep absent ID"}]`},
		{"129 character call name", `[{"type":"function_call","call_id":"id","name":"` + strings.Repeat("a", 129) + `"},{"type":"function_call_output","call_id":"id","output":"discard"},{"type":"message","role":"assistant","content":"keep"}]`, `[{"type":"message","role":"assistant","content":"keep"}]`},
		{"call kinds share an ID", `[{"type":"function_call","call_id":"shared","name":"bad name"},{"type":"custom_tool_call","call_id":"shared","name":"edit","input":"patch"},{"type":"custom_tool_call_output","call_id":"shared","output":"keep"},{"type":"function_call_output","call_id":"shared","output":"discard"}]`, `[{"type":"custom_tool_call","call_id":"shared","name":"edit","input":"patch"},{"type":"custom_tool_call_output","call_id":"shared","output":"keep"}]`},
		{"malformed custom call shares a function call ID",
			`[{"type":"custom_tool_call","call_id":"shared","name":"bad name","input":"patch"},{"type":"function_call","call_id":"shared","name":"read","arguments":"{}"},{"type":"custom_tool_call_output","call_id":"shared","output":"discard"},{"type":"function_call_output","call_id":"shared","output":"keep"}]`,
			`[{"type":"function_call","call_id":"shared","name":"read","arguments":"{}"},{"type":"function_call_output","call_id":"shared","output":"keep"}]`},
		{"user message resets call window", `[{"type":"function_call","call_id":"id","name":"bad name"},{"type":"message","role":"user","content":"next"},{"type":"function_call","call_id":"id","name":"read","arguments":"{}"},{"type":"function_call_output","call_id":"id","output":"keep"}]`, `[{"type":"message","role":"user","content":"next"},{"type":"function_call","call_id":"id","name":"read","arguments":"{}"},{"type":"function_call_output","call_id":"id","output":"keep"}]`},
		{"drop references and incomplete image generation", `[{"type":"item_reference","id":"stray"},{"type":"image_generation_call","id":"incomplete"},{"type":"image_generation_call","id":"ig_1","result":"data","status":"in_progress"}]`, `[{"id":"ig_1","result":"data","status":"completed","type":"image_generation_call"}]`},
		{"remove blanks and dangling reasoning", `[{"type":"reasoning","encrypted_content":"keep"},{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"no"}]},{"type":"reasoning","encrypted_content":"discard"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":" "}]}]`, `[{"type":"reasoning","encrypted_content":"keep"},{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"no"}]}]`},
		{"new reasoning after a blank message", `[{"type":"reasoning","summary":[{"text":"A"}]},{"type":"message","role":"assistant","content":" "},{"type":"reasoning","summary":[{"text":"B"}]},{"type":"message","role":"assistant","content":"keep"}]`, `[{"type":"reasoning","summary":[{"text":"B"}]},{"type":"message","role":"assistant","content":"keep"}]`},
		{"user boundary drops reasoning left by a blank assistant message",
			`[{"type":"reasoning","encrypted_content":"discard"},{"type":"message","role":"assistant","content":" "},{"type":"message","role":"user","content":"next"},{"type":"message","role":"assistant","content":"keep"}]`,
			`[{"type":"message","role":"user","content":"next"},{"type":"message","role":"assistant","content":"keep"}]`},
		{"hidden only", `[{"type":"reasoning","encrypted_content":"discard"},{"type":"message","role":"assistant","content":" "}]`, `null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := shapeNativeResponsesItems(readItems(tc.input), true)
			if want := readItems(tc.want); !reflect.DeepEqual(got, want) {
				t.Fatalf("replay items = %s, want %s", got, want)
			}
		})
	}
}

func TestReadRepairsNativeResponsesItemsWithoutChangingPayload(t *testing.T) {
	const placeholder = `,"output":"[No tool output recorded: the tool call was interrupted before it produced a result.]"}`
	const api = conversation.OpenAIResponses
	readNative := func(t *testing.T, items string) *conversation.Native {
		t.Helper()
		// A journal record occupies one line. Keep the spaces inside its saved payload.
		items = strings.ReplaceAll(items, "\n", "")
		payload := `{"type":"openaiResponsesHistory","provider":"openai","items":` + items + `}`
		f := newFixture()
		f.add(`{"type":"message","message":{"role":"assistant","api":"` + api +
			`","provider":"openai","model":"gpt-test","stopReason":"stop","content":[{"type":"text","text":"visible"}],"providerPayload":` + payload + `}}`)
		s := read(t, f.bytes())
		native := s.Messages[0].Native
		if native == nil || string(native.Payload) != payload {
			t.Fatalf("native = %+v, want unchanged payload %s", native, payload)
		}
		return native
	}
	assertItems := func(t *testing.T, got *conversation.Native, wantJSON string) {
		t.Helper()
		var want, items []map[string]any
		for _, item := range got.Items {
			var fields map[string]any
			if err := json.Unmarshal(item, &fields); err != nil {
				t.Fatal(err)
			}
			items = append(items, fields)
		}
		if err := json.Unmarshal([]byte(wantJSON), &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(items, want) {
			t.Fatalf("native items = %s, want %s", got.Items, wantJSON)
		}
	}
	t.Run("malformed arguments become a bounded diagnostic", func(t *testing.T) {
		prefix := `{"path":"`
		malformed := prefix + strings.Repeat("é", 600)
		bounded := prefix + strings.Repeat("é", 512-len(prefix)) + "… [truncated 97 chars]"
		native := readNative(t, `[{"type":"function_call","name":"read","call_id":"bad","arguments":`+quote(malformed)+`},`+
			`{"type":"function_call_output","call_id":"bad","output":"still paired"},`+
			`{"type":"function_call","name":"read","call_id":"good","arguments":" {\"path\":\"x\"} "}]`)
		assertItems(t, native, `[{"type":"function_call","name":"read","call_id":"bad","arguments":`+
			quote(`{"__parseError":"invalid JSON","__rawJson":`+quote(bounded)+`}`)+`},`+
			`{"type":"function_call_output","call_id":"bad","output":"still paired"},`+
			`{"type":"function_call","name":"read","call_id":"good","arguments":" {\"path\":\"x\"} "},`+
			`{"type":"function_call_output","call_id":"good"`+placeholder+`]`)
	})
	t.Run("remove output-only fields but keep replay content", func(t *testing.T) {
		native := readNative(t, `[
			{"type":"reasoning","id":"wrong_summary","summary":null,"content":[],"encrypted_content":"ciphertext","created_by":null,"status":"completed"},
			{"type":"reasoning","id":7,"summary":[],"content":{},"encrypted_content":false,"unknown":"ignored"},
			{"type":"reasoning","id":"rs_computer","encrypted_content":null},
			{"type":"computer_call","id":"cu_keep","call_id":"computer","status":"completed","actions":[],"created_by":"backend"},
			{"type":"shell_call_output","id":"unused","call_id":"shell","created_by":"backend","output":[null,{"stdout":"keep","created_by":null}]},
			{"type":"reasoning","id":"ordinary","encrypted_content":"second"},
			{"type":"function_call","id":"fc_remove","call_id":"keep","name":"read","arguments":"{}","status":"completed","created_by":null},
			{"type":"message","id":"msg_remove","role":"assistant","content":"answer","status":"completed","internal_chat_message_metadata_passthrough":{}}
		]`)
		assertItems(t, native, `[
			{"type":"reasoning","id":"wrong_summary","content":[],"encrypted_content":"ciphertext"},
			{"type":"reasoning","summary":[]},
			{"type":"reasoning","id":"rs_computer","encrypted_content":null},
			{"type":"computer_call","id":"cu_keep","call_id":"computer","status":"completed","actions":[]},
			{"type":"shell_call_output","call_id":"shell","output":[null,{"stdout":"keep"}]},
			{"type":"reasoning","encrypted_content":"second"},
			{"type":"function_call","call_id":"keep","name":"read","arguments":"{}"},
			{"type":"function_call_output","call_id":"keep"`+placeholder+`,
			{"type":"message","role":"assistant","content":"answer"}
		]`)
	})
}

func TestReadRepairsResponsesCallsWithoutRecordedOutput(t *testing.T) {
	const api = conversation.OpenAIResponses
	const native = `{"type":"openaiResponsesHistory","provider":"openai","dt":true,"items":[` +
		`{"type":"function_call","name":"read","call_id":"call_a","arguments":"{}"},` +
		`{"type":"custom_tool_call","name":"edit","call_id":"call_b","input":"fixture"},` +
		`{"type":"function_call","name":"read","call_id":"call_c","arguments":"{}"}]}`
	f := newFixture()
	f.add(userEntry("Check the note."))
	f.add(`{"type":"message","message":{"role":"assistant","api":"` + api +
		`","provider":"openai","model":"gpt-test","stopReason":"toolUse","content":[` +
		`{"type":"toolCall","id":"call_c","name":"read","arguments":{}}],"providerPayload":` + native + `}}`)
	f.add(toolResultEntry("call_c", "read", "A note.", false))
	messages := read(t, f.bytes()).Messages
	got := messages[1].Native
	if got == nil || string(got.Payload) != native || !got.Incremental {
		t.Fatalf("reader did not preserve the native payload: %+v", got)
	}
	var items []map[string]any
	raw, err := json.Marshal(got.Items)
	if err != nil || json.Unmarshal(raw, &items) != nil {
		t.Fatalf("cannot decode replay items: %v", err)
	}
	const interrupted = "[No tool output recorded: the tool call was interrupted before it produced a result.]"
	want := []map[string]any{
		{"type": "function_call", "name": "read", "call_id": "call_a", "arguments": "{}"},
		{"type": "function_call_output", "call_id": "call_a", "output": interrupted},
		{"type": "custom_tool_call", "name": "edit", "call_id": "call_b", "input": "fixture"},
		{"type": "custom_tool_call_output", "call_id": "call_b", "output": interrupted},
		{"type": "function_call", "name": "read", "call_id": "call_c", "arguments": "{}"},
	}
	if !reflect.DeepEqual(items, want) {
		t.Fatalf("native replay items = %v, want %v", items, want)
	}
	assertMessages(t, messages[2:], []conversation.Message{result("call_c", "read", "A note.", false)})
}

func TestResponsesUserCompactionPayloadShaping(t *testing.T) {
	item := json.RawMessage(`{"type":"compaction_summary","summary":"keep"}`)
	m := llmMessage{
		role:   "user",
		blocks: []block{{Type: "text", Text: "Summary."}},
		native: &conversation.Native{
			Kind: conversation.OpenAIResponsesHistory, Provider: "openai",
			Items: []json.RawMessage{
				item,
				json.RawMessage(`{"type":"function_call","call_id":"id","name":"bad name"}`),
				json.RawMessage(`{"type":"function_call_output","call_id":"id","output":"discard"}`),
			},
		},
	}
	got, err := toRequest([]llmMessage{m}, target{api: conversation.OpenAIResponses, provider: "openai", model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Native == nil || !reflect.DeepEqual(got[0].Native.Items, []json.RawMessage{item}) {
		t.Fatalf("user replay = %+v, want only compaction summary in Native.Items", got)
	}
}

func TestResponsesNativeProviderEligibility(t *testing.T) {
	for _, tc := range []struct {
		name, sourceAPI, sourceModel, sourceProvider, payloadProvider string
		keep                                                          bool
	}{
		{"same target", conversation.OpenAIResponses, "gpt-test", "openai", "openai", true},
		{"different payload provider", conversation.OpenAIResponses, "gpt-test", "openai", "proxy", false},
		{"different source API", "azure-openai-responses", "gpt-test", "openai", "openai", false},
		{"different source model", conversation.OpenAIResponses, "gpt-other", "openai", "openai", false},
		{"missing provider uses same source", conversation.OpenAIResponses, "gpt-test", "openai", "", true},
		{"missing provider uses foreign source", conversation.OpenAIResponses, "gpt-test", "proxy", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			providerField := ""
			if tc.payloadProvider != "" {
				providerField = `"provider":` + quote(tc.payloadProvider) + `,`
			}
			m := llmMessage{role: "assistant", api: tc.sourceAPI, provider: tc.sourceProvider, model: tc.sourceModel,
				payload: []byte(`{"type":"openaiResponsesHistory",` + providerField + `"dt":true,"items":[{"type":"message","role":"assistant","content":"answer"}]}`)}
			native, err := nativeHistory(m, target{api: conversation.OpenAIResponses, provider: "openai", model: "gpt-test"})
			if err != nil || (native != nil) != tc.keep {
				t.Errorf("native = %+v, error = %v; want native = %t without error", native, err, tc.keep)
			}
		})
	}
}
