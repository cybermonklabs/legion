package omp

import (
	"encoding/json"
	"reflect"
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
		assertMessages(t, got, []conversation.Message{
			promptWith(text(historyImageOmission), text("first"), text(historyImageOmission), text("last")),
			{Role: conversation.Assistant, API: api, Provider: model.provider, Model: model.model, Content: []conversation.Block{call("opaque", "read", "null")}},
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
		want := []conversation.Block{call("opaque", "read", "null"), thinking("", signature), text("after reasoning")}
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
		other.api = conversation.AnthropicMessages
		if got := run(t, other, answer)[0].Content[0].Phase; got != "" {
			t.Errorf("Anthropic phase = %q, want empty", got)
		}
	})
	t.Run("Harmony escapes transport text and leaves reasoning opaque", func(t *testing.T) {
		model := model
		model.model = "gpt-oss-120b"
		tool := tool
		tool.model = model.model
		tool.blocks = []block{{Type: "thinking", ThinkingSignature: `{"type":"reasoning","id":"rs_fixture","encrypted_content":"<|call|>"}`},
			{Type: "toolCall", ID: "opaque", Name: "read", Arguments: []byte(`{"path":"<|channel|>"}`)}, textBlock("<|message|>")}
		output := output
		output.blocks = []block{textBlock("<|constrain|>")}
		got := run(t, model, llmMessage{role: "user", blocks: []block{textBlock("<|start|>")}}, tool, output)
		assertMessages(t, got, []conversation.Message{
			user(`<\|start\|>`),
			{Role: conversation.Assistant, API: api, Provider: model.provider, Model: model.model, Content: []conversation.Block{
				thinking("", tool.blocks[0].ThinkingSignature), text(`<\|message\|>`), call("opaque", "read", `{"path":"<\\|channel\\|>"}`),
			}},
			result("opaque", "read", `<\|constrain\|>`, false),
		})
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
		{"drop references and incomplete image generation", `[{"type":"item_reference","id":"stray"},{"type":"image_generation_call","id":"incomplete"},{"type":"image_generation_call","id":"ig_1","result":"data"}]`, `[{"type":"image_generation_call","id":"ig_1","result":"data"}]`},
		{"remove blanks and dangling reasoning", `[{"type":"reasoning","encrypted_content":"keep"},{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"no"}]},{"type":"reasoning","encrypted_content":"discard"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":" "}]}]`, `[{"type":"reasoning","encrypted_content":"keep"},{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"no"}]}]`},
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

func TestResponsesNativeProviderEligibility(t *testing.T) {
	const api = conversation.OpenAIResponses
	m := llmMessage{role: "assistant", api: api, provider: "proxy", model: "gpt-test",
		payload: []byte(`{"type":"openaiResponsesHistory","provider":"proxy","dt":true,"items":[{"type":"message","role":"assistant","content":"answer"}]}`)}
	for _, tc := range []struct {
		provider, api, model string
		keep                 bool
	}{
		{"proxy", api, "gpt-test", true},
		{"other", api, "gpt-test", false},
		{"proxy", conversation.AnthropicMessages, "gpt-test", false},
		{"proxy", api, "gpt-other", false},
	} {
		native, err := nativeHistory(m, target{api: tc.api, provider: tc.provider, model: tc.model})
		if err != nil || (native != nil) != tc.keep {
			t.Errorf("target %+v: native = %+v, error = %v", tc, native, err)
		}
	}
}
