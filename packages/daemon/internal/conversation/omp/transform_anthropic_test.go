package omp

import (
	"encoding/json"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/conversation"
)

func TestAnthropicHistoryShaping(t *testing.T) {
	target := ompAnthropicTarget()
	image := block{Type: "image", MimeType: " Image/JPG ", Data: "eA=="}
	jpeg := conversation.Block{Type: conversation.Image, MediaType: "image/jpeg", ImageData: image.Data}
	png := conversation.Block{Type: conversation.Image, MediaType: "image/png", ImageData: "eQ=="}
	textBlock := func(s string) block { return block{Type: "text", Text: s} }
	prompt := func(parts ...block) llmMessage { return llmMessage{role: "user", blocks: parts} }
	assistant := func(parts ...block) llmMessage {
		return llmMessage{role: "assistant", api: target.api, provider: target.provider, model: target.model, stopReason: "toolUse", blocks: parts}
	}
	turn := func(source llmMessage, blocks ...conversation.Block) conversation.Message {
		return conversation.Message{Role: conversation.Assistant, API: source.api, Provider: source.provider, Model: source.model, Content: blocks}
	}
	callBlock := func(id string) block { return block{Type: "toolCall", ID: id, Name: "read"} }
	toolResult := func(id string, isError bool, parts ...block) llmMessage {
		return llmMessage{role: "toolResult", toolCallID: id, toolName: "read", isError: isError, blocks: parts}
	}
	userBlocks := func(parts ...conversation.Block) conversation.Message {
		return conversation.Message{Role: conversation.User, Content: parts}
	}
	check := func(t *testing.T, input []llmMessage, want ...conversation.Message) {
		t.Helper()
		got, err := toRequest(input, target)
		if err != nil {
			t.Fatal(err)
		}
		assertMessages(t, got, want)
	}
	t.Run("partition stable only on Anthropic", func(t *testing.T) {
		source := assistant(textBlock("Before."), callBlock("x"), textBlock("After."), callBlock("y"))
		source.api = conversation.OpenAIResponses
		input := []llmMessage{source, toolResult("x", false, textBlock("x")), toolResult("y", false, textBlock("y"))}
		x, y := call("x", "read", `{}`), call("y", "read", `{}`)
		xResult, yResult := result("x", "read", "x", false), result("y", "read", "y", false)
		check(t, input, turn(source, text("Before."), text("After."), x, y), xResult, yResult)
		responses := target
		responses.api = conversation.OpenAIResponses
		got, err := toRequest(input, responses)
		if err != nil {
			t.Fatal(err)
		}
		assertMessages(t, got, []conversation.Message{turn(source, text("Before."), x, text("After."), y), xResult, yResult})
	})
	t.Run("thinking alone does not trigger partition", func(t *testing.T) {
		source := assistant(callBlock("x"), block{Type: "thinking", Thinking: "plan", ThinkingSignature: "c2ln"}, block{Type: "redactedThinking", Data: "cmVk"})
		check(t, []llmMessage{source, toolResult("x", false, textBlock("ok"))},
			turn(source, call("x", "read", `{}`), thinking("plan", "c2ln"), conversation.Block{Type: conversation.RedactedThinking, Data: "cmVk"}),
			result("x", "read", "ok", false))
	})
	t.Run("assistant made empty by encoder drops", func(t *testing.T) {
		empty := assistant(textBlock(" \t"))
		check(t, []llmMessage{prompt(textBlock("first")), empty, prompt(textBlock("next"))}, user("first"), user("next"))
	})
	t.Run("blank user drops between assistants", func(t *testing.T) {
		one, two := assistant(textBlock("one")), assistant(textBlock("two"))
		check(t, []llmMessage{one, prompt(textBlock(" \t")), two}, turn(one, text("one")), user("Continue."), turn(two, text("two")))
	})
	t.Run("inline image type normalization", func(t *testing.T) {
		check(t, []llmMessage{prompt(textBlock("look"), image)}, userBlocks(text("look"), jpeg))
	})
	t.Run("image only input has leading text", func(t *testing.T) {
		check(t, []llmMessage{prompt(textBlock("\t"), image)}, userBlocks(text("(see attached image)"), jpeg))
	})
	t.Run("unsupported historical media becomes text", func(t *testing.T) {
		pdf := block{Type: "image", MimeType: "application/pdf", Data: "eA=="}
		check(t, []llmMessage{prompt(pdf, image)}, userBlocks(text("(see attached image)"), text("[unsupported image: application/pdf]"), jpeg))
	})
	t.Run("text only option replaces each image and joins text", func(t *testing.T) {
		f := newFixture()
		f.add(claudeEntry("stop", `[{"type":"text","text":"answer"}]`))
		f.add(`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"first"},{"type":"image","mimeType":"image/png","data":"eA=="},{"type":"image","mimeType":"image/png","data":"eQ=="},{"type":"text","text":"last"}]}}`)
		s, err := Read(f.bytes(), Options{AnthropicTextOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		assertMessages(t, s.Messages, []conversation.Message{
			claude(text("answer")), user("first\n[image omitted: model does not support vision]\n[image omitted: model does not support vision]\nlast"),
		})
	})
	t.Run("error images follow the whole tool result run", func(t *testing.T) {
		pngBlock := block{Type: "image", MimeType: png.MediaType, Data: png.ImageData}
		source := assistant(callBlock("a"), callBlock("b"), callBlock("c"))
		check(t, []llmMessage{source, toolResult("a", true, image), toolResult("b", false, textBlock("ok")),
			toolResult("c", true, textBlock("failed"), pngBlock), prompt(textBlock("next"))},
			turn(source, call("a", "read", `{}`), call("b", "read", `{}`), call("c", "read", `{}`)),
			result("a", "read", "(see attached image)", true), result("b", "read", "ok", false), result("c", "read", "failed", true),
			userBlocks(text("Attached image(s) from the tool result(s) above:"), jpeg, png), user("next"))
	})
	t.Run("result blank text drops", func(t *testing.T) {
		source := assistant(callBlock("x"))
		check(t, []llmMessage{source, toolResult("x", false, textBlock("one"), textBlock("\t"), textBlock("two"))},
			turn(source, call("x", "read", `{}`)), conversation.Message{Role: conversation.Tool, Content: []conversation.Block{
				{Type: conversation.ToolResult, ToolCallID: "x", ToolName: "read", Content: []conversation.Block{text("one"), text("two")}},
			}})
	})
	t.Run("empty error result supplies text", func(t *testing.T) {
		source := assistant(callBlock("a"), callBlock("b"))
		check(t, []llmMessage{source, toolResult("a", true, textBlock(" ")), toolResult("b", false)},
			turn(source, call("a", "read", `{}`), call("b", "read", `{}`)), result("a", "read", "Tool failed with no output.", true),
			conversation.Message{Role: conversation.Tool, Content: []conversation.Block{{Type: conversation.ToolResult, ToolCallID: "b", ToolName: "read"}}})
	})
	t.Run("tool argument strings replace lone surrogates", func(t *testing.T) {
		c := callBlock("x")
		c.Arguments = json.RawMessage(`{"\ud800": ["a\ud800b", "\udfff", "\ud83d\ude00", "\\ud800", {"s": "\udabc"}]}`)
		source := assistant(c)
		const want = `{"\ud800": ["a�b", "�", "\ud83d\ude00", "\\ud800", {"s": "�"}]}`
		check(t, []llmMessage{source, toolResult("x", false, textBlock("ok"))}, turn(source, call("x", "read", want)), result("x", "read", "ok", false))
	})
	t.Run("null arguments become an empty object", func(t *testing.T) {
		c := callBlock("x")
		c.Arguments = json.RawMessage(`null`)
		source := assistant(c)
		check(t, []llmMessage{source, toolResult("x", false, textBlock("ok"))}, turn(source, call("x", "read", `{}`)), result("x", "read", "ok", false))
	})
	t.Run("mixed signed and unsigned thinking demotes the unsigned part", func(t *testing.T) {
		source := assistant(block{Type: "thinking", ThinkingSignature: "c2ln"}, block{Type: "thinking", Thinking: "unsigned"},
			block{Type: "thinking", ThinkingSignature: " "}, textBlock("answer"))
		check(t, []llmMessage{source}, turn(source, thinking("", "c2ln"), text("<think>\nunsigned\n</think>"), text("answer")))
	})
	t.Run("missing signed thinking normalizes a blank signature", func(t *testing.T) {
		source := assistant(block{Type: "thinking", Thinking: "unsigned", ThinkingSignature: " \t"})
		check(t, []llmMessage{source}, turn(source, thinking("unsigned", "")))
	})
}
