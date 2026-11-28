package omp

import (
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/conversation"
)

func TestAnthropicPrefixBinding(t *testing.T) {
	for _, tc := range []struct {
		id     string
		prefix bool
	}{
		{"claude-opus-5-5", true},
		{"claude-sonnet-5.6", true},
		{"claude-haiku-5-5", true},
		{"claude-fable-5-1", true},
		{"claude-mythos-6-1", true},
		{"claude-sonnet-5-4", false},
		{"claude-opus-6-0", false},
		{"claude-opus-55", false},
		{"claude-fable-5-0", false},
		{"claude-opus-5-5b", false},
		{"claude-test", false},
	} {
		t.Run(tc.id, func(t *testing.T) {
			if prefix := anthropicPrefixBinding(tc.id); prefix != tc.prefix {
				t.Errorf("prefix binding = %t; want %t", prefix, tc.prefix)
			}
		})
	}
}

func TestReadDropsThinkingAfterAPrecedingRewrite(t *testing.T) {
	const signed = `[{"type":"thinking","thinking":"plan","thinkingSignature":"c2ln"},{"type":"redactedThinking","data":"cmVk"},{"type":"text","text":"answer"}]`
	for _, tc := range []struct {
		name, marker, assistantTime string
		wantThinking                bool
	}{
		{"prune at the turn time", `{"type":"message","message":{"role":"toolResult","toolCallId":"compacted-call","toolName":"read","content":[{"type":"text","text":"pruned"}],"prunedAt":10,"timestamp":2}}`, "10", false},
		{"branch at the turn time", `{"type":"branch_summary","summary":"The earlier branch ended.","timestamp":"1970-01-01T00:00:00.010Z"}`, "10", false},
		{"user rewrite at the turn time", `{"type":"message","message":{"role":"user","content":"new prefix","historyRewriteAt":10,"timestamp":2}}`, "10", false},
		{"assistant newer than prune", `{"type":"message","message":{"role":"toolResult","toolCallId":"compacted-call","toolName":"read","content":[{"type":"text","text":"pruned"}],"prunedAt":10,"timestamp":2}}`, "11", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			f.add(userEntry("request"))
			f.add(tc.marker)
			// Timestamps compare by maximum, not by the most recent marker's position.
			f.add(`{"type":"message","message":{"role":"user","content":"next","historyRewriteAt":1,"timestamp":2}}`)
			f.add(strings.Replace(claudeEntry("stop", signed), `"timestamp":2`, `"timestamp":`+tc.assistantTime, 1))
			// A marker later in message order does not invalidate the preceding turn.
			f.add(`{"type":"message","message":{"role":"user","content":"later marker","historyRewriteAt":100,"timestamp":100}}`)
			messages := read(t, f.bytes()).Messages
			want := claude(text("answer"))
			if tc.wantThinking {
				want = claude(thinking("plan", "c2ln"), conversation.Block{Type: conversation.RedactedThinking, Data: "cmVk"}, text("answer"))
			}
			assertMessages(t, messages[len(messages)-2:], []conversation.Message{want, user("later marker")})
		})
	}
}

func TestTransformNativeAnthropicBlocks(t *testing.T) {
	for _, tc := range []struct {
		name, kind, source, sourceAPI, provider, api, model string
		refused                                             bool
	}{
		{"server tool across providers", "anthropicServerTool", "other", conversation.AnthropicMessages, "anthropic", conversation.AnthropicMessages, "claude-test", false},
		{"server tool across APIs", "anthropicServerTool", "anthropic", conversation.OpenAIResponses, "anthropic", conversation.AnthropicMessages, "claude-test", false},
		{"server tool same provider", "anthropicServerTool", "anthropic", conversation.AnthropicMessages, "anthropic", conversation.AnthropicMessages, "claude-test", true},
		{"fallback on Anthropic", "fallback", "anthropic", conversation.AnthropicMessages, "anthropic", conversation.AnthropicMessages, "claude-test", true},
		{"fallback on Responses", "fallback", "anthropic", conversation.AnthropicMessages, "openai", conversation.OpenAIResponses, "gpt-test", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			f.add(assistantEntry(tc.source, tc.sourceAPI, "claude-test", "stop", `[{"type":`+quote(tc.kind)+`},{"type":"text","text":"answer"}]`))
			f.add(userEntry("next"))
			f.add(assistantEntry(tc.provider, tc.api, tc.model, "stop", `[{"type":"text","text":"next answer"}]`))
			s, err := Read(f.bytes(), Options{})
			if tc.refused {
				if !errors.Is(err, conversation.ErrUnknownFormat) {
					t.Fatalf("Read = %v, want ErrUnknownFormat", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			assertMessages(t, s.Messages[:1], []conversation.Message{{Role: conversation.Assistant, Provider: tc.source,
				API: tc.sourceAPI, Model: "claude-test", Content: []conversation.Block{text("answer")}}})
		})
	}
}

func TestTransformToolResultRules(t *testing.T) {
	const model = "gpt-test"
	const api = conversation.OpenAIResponses
	defaultTarget := target{provider: "openai", api: api, model: model}
	turn := func(stop string, blocks ...block) llmMessage {
		return llmMessage{role: "assistant", provider: "openai", api: api, model: model, stopReason: stop, blocks: blocks}
	}
	callBlock := func(id string) block { return block{Type: "toolCall", ID: id, Name: "read", Arguments: []byte(`{}`)} }
	out := func(id, text string) llmMessage {
		return llmMessage{role: "toolResult", toolCallID: id, toolName: "read", blocks: []block{{Type: "text", Text: text}}}
	}
	usr := llmMessage{role: "user", blocks: []block{{Type: "text", Text: "next"}}}
	assistant := func(blocks ...conversation.Block) conversation.Message {
		return conversation.Message{Role: conversation.Assistant, Provider: "openai", API: api, Model: model, Content: blocks}
	}
	run := func(t *testing.T, input []llmMessage, want ...conversation.Message) {
		t.Helper()
		got, err := toRequest(input, defaultTarget)
		if err != nil {
			t.Fatal(err)
		}
		assertMessages(t, got, want)
	}
	t.Run("parallel results and duplicate calls", func(t *testing.T) {
		long := strings.Repeat("a", 64)
		dup := long[:59] + "_dup1"
		first, second := call(long, "read", `{}`), call(dup, "read", `{}`)
		const signature = `{"type":"reasoning","id":"rs_duplicate","encrypted_content":"opaque"}`
		first.ItemID, second.ItemID = "fc_first", "fc_second_dup1"
		run(t, []llmMessage{turn("toolUse", block{Type: "thinking", ThinkingSignature: signature}, callBlock(long+"|fc_first"), callBlock(long+"|fc_second"), callBlock("peer")),
			out("peer", "peer first"), out(long+"|fc_result", "first"), out(long, "second"), out(long, "duplicate output")},
			assistant(thinking("", signature), first, second, call("peer", "read", `{}`)),
			result("peer", "read", "peer first", false), result(long, "read", "first", false), result(dup, "read", "second", false))
	})
	t.Run("generated suffix skips a previously used ID", func(t *testing.T) {
		run(t, []llmMessage{turn("toolUse", callBlock("x_dup1"), callBlock("x"), callBlock("x")), out("x_dup1", "original"), out("x", "first"), out("x", "second")},
			assistant(call("x_dup1", "read", `{}`), call("x", "read", `{}`), call("x_dup2", "read", `{}`)),
			result("x_dup1", "read", "original", false), result("x", "read", "first", false), result("x_dup2", "read", "second", false))
	})
	t.Run("flush duplicate at end", func(t *testing.T) {
		run(t, []llmMessage{turn("toolUse", callBlock("x"), callBlock("x")), out("x", "real")},
			assistant(call("x", "read", `{}`), call("x_dup1", "read", `{}`)), result("x", "read", "real", false),
			result("x_dup1", "read", "No result provided", true))
	})
	t.Run("flush duplicate before next assistant", func(t *testing.T) {
		run(t, []llmMessage{turn("toolUse", callBlock("x"), callBlock("x")), out("x", "real"), turn("toolUse", callBlock("y")), out("y", "next")},
			assistant(call("x", "read", `{}`), call("x_dup1", "read", `{}`)), result("x", "read", "real", false),
			result("x_dup1", "read", "No result provided", true), assistant(call("y", "read", `{}`)), result("y", "read", "next", false))
	})
	t.Run("new turn takes ownership of recurring id", func(t *testing.T) {
		run(t, []llmMessage{turn("toolUse", callBlock("call"), callBlock("call")), out("call", "old"), usr,
			turn("toolUse", callBlock("call")), out("call", "new")},
			assistant(call("call", "read", `{}`), call("call_dup1", "read", `{}`)), result("call", "read", "old", false),
			result("call_dup1", "read", "No result provided", true), user("next"),
			assistant(call("call_dup2", "read", `{}`)), result("call_dup2", "read", "new", false))
	})
	t.Run("failed turn prefers a later real result", func(t *testing.T) {
		run(t, []llmMessage{out("late", "too early"), turn("aborted", callBlock("missing"), callBlock("late")), out("orphan", "omit"), usr, out("late", "real")},
			assistant(call("missing", "read", `{}`), call("late", "read", `{}`)), result("missing", "read", "aborted", true),
			result("late", "read", "real", false), user("next"))
	})
	t.Run("malformed call result pairing is positional", func(t *testing.T) {
		bad := callBlock("same")
		bad.Name = "read.invalid"
		blankID := callBlock("   ")
		tooLong := callBlock("long")
		tooLong.Name = strings.Repeat("a", 129)
		run(t, []llmMessage{turn("toolUse", blankID, tooLong, bad, callBlock("same")), out("   ", "blank"), out("long", "long"),
			out("same", "discard"), out("same", "keep"), turn("toolUse", bad), usr, out("same", "late duplicate")},
			assistant(call("same", "read", `{}`)), result("same", "read", "keep", false), user("next"))
	})
	t.Run("malformed queue ends at user and assistant boundaries", func(t *testing.T) {
		bad := callBlock("x")
		bad.Name = "read.invalid"
		emptied := turn("toolUse", bad)
		emptied.payload = []byte(`{"type":"openaiResponsesHistory","items":[{"type":"message","role":"assistant","content":"not carried"}]}`)
		visible := block{Type: "text", Text: "answer"}
		stale := user("<stale-tool-result tool=\"read\" id=\"x\">\nreal output\n</stale-tool-result>")
		for _, boundary := range []struct {
			name string
			in   llmMessage
			want conversation.Message
		}{{"user", usr, user("next")}, {"assistant", turn("stop", visible), assistant(text("answer"))}} {
			t.Run(boundary.name, func(t *testing.T) {
				run(t, []llmMessage{emptied, boundary.in, out("x", "real output")}, boundary.want, stale)
			})
		}
	})
	t.Run("name length cap", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			target target
		}{
			{"Responses", defaultTarget},
			{"Messages", ompAnthropicTarget()},
		} {
			t.Run(tc.name, func(t *testing.T) {
				atLimit, tooLong := callBlock("x"), callBlock("x")
				atLimit.Name = strings.Repeat("a", 128)
				tooLong.Name = atLimit.Name + "b"
				got, err := toRequest([]llmMessage{
					turn("toolUse", tooLong, atLimit),
					out("x", "discard"), out("x", "keep"),
				}, tc.target)
				if err != nil {
					t.Fatal(err)
				}
				assertMessages(t, got, []conversation.Message{
					assistant(call("x", atLimit.Name, `{}`)), result("x", "read", "keep", false),
				})
			})
		}
	})
	t.Run("orphan note preserves nonblank output and error", func(t *testing.T) {
		synthetic := out("synthetic", "did not run")
		synthetic.notExecuted = true
		orphan := out("orphan", "actual output")
		orphan.blocks = append(orphan.blocks, block{Type: "text", Text: "\t"}, block{Type: "text", Text: "second"})
		orphan.isError = true
		run(t, []llmMessage{synthetic, orphan}, user("<stale-tool-result tool=\"read\" id=\"orphan\" is-error=\"true\">\nactual output\nsecond\n</stale-tool-result>"))
	})
	t.Run("only Responses origin can pair by call component", func(t *testing.T) {
		messages := []conversation.Message{
			assistant(call("call|native", "read", `{}`)),
			assistant(call("call|opaque", "read", `{}`)),
		}
		key := toolPairingKey([]llmMessage{{api: api}, {api: "openai-completions"}}, messages)
		for in, want := range map[string]string{"call|different": "call", "call|opaque": "call|opaque", "unrelated|opaque": "unrelated|opaque", "|empty": "|empty"} {
			if got := key(in); got != want {
				t.Errorf("key(%q) = %q, want %q", in, got, want)
			}
		}
	})
	t.Run("cross API Responses IDs use the Bun hash", func(t *testing.T) {
		for _, tc := range []struct{ id, want string }{
			{"call:abc", "call_abc_29nxm66dspohs"},
			{"|", "call_347lcsqfrgkgp"},
		} {
			foreign := turn("toolUse", callBlock(tc.id))
			foreign.api = conversation.AnthropicMessages
			run(t, []llmMessage{foreign, out(tc.id, "real")},
				conversation.Message{Role: conversation.Assistant, Provider: "openai", API: conversation.AnthropicMessages, Model: model,
					Content: []conversation.Block{call(tc.want, "read", `{}`)}}, result(tc.want, "read", "real", false))
		}
	})
	t.Run("different Responses item half after Anthropic normalization", func(t *testing.T) {
		source := turn("toolUse", callBlock("call|fc_first"))
		got, err := toRequest([]llmMessage{source, usr, out("call|fc_other", "real")}, ompAnthropicTarget())
		if err != nil {
			t.Fatal(err)
		}
		assertMessages(t, got, []conversation.Message{
			assistant(call("call_fc_first", "read", `{}`)),
			result("call_fc_first", "read", "real", false), user("next"),
		})
	})
}

func TestTransformAssistantReplayRules(t *testing.T) {
	const id = "claude-test"
	const api = conversation.AnthropicMessages
	signed := block{Type: "thinking", Thinking: "plan", ThinkingSignature: "c2ln"}
	redacted := block{Type: "redactedThinking", Data: "cmVk"}
	redactedBlock := conversation.Block{Type: conversation.RedactedThinking, Data: "cmVk"}
	answer := block{Type: "text", Text: "answer"}
	out := func(provider, model string, blocks ...conversation.Block) conversation.Message {
		return conversation.Message{Role: conversation.Assistant, API: api, Provider: provider, Model: model, Content: blocks}
	}
	for _, tc := range []struct {
		name, provider, model, stop string
		blocks                      []block
		signing, later              bool
		want                        []conversation.Block
	}{
		{"same model preserves signatures", "anthropic", id, "stop", []block{signed, redacted, answer}, true, true, []conversation.Block{thinking("plan", "c2ln"), redactedBlock, text("answer")}},
		{"signature enforcement removes visible and redacted thinking", "anthropic", id, "stop", []block{{Type: "thinking", Thinking: "unsigned"}, redacted, answer}, true, false, []conversation.Block{text("answer")}},
		{"compatible endpoint replays unsigned thinking", "anthropic", id, "stop", []block{{Type: "thinking", Thinking: "unsigned"}, redacted, answer}, false, false, []conversation.Block{thinking("unsigned", ""), redactedBlock, text("answer")}},
		{"compatible endpoint drops unanchored empty thinking", "anthropic", id, "stop", []block{{Type: "thinking", Thinking: " \t"}, answer}, false, false, []conversation.Block{text("answer")}},
		{"assistant images and blank blocks drop", "anthropic", id, "stop", []block{{Type: "image", MimeType: "image/png", Data: "eA=="}, {Type: "text", Text: " "}, {Type: "redactedThinking", Data: " "}, answer}, true, false, []conversation.Block{text("answer")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := []llmMessage{{role: "assistant", api: api, provider: tc.provider, model: tc.model, stopReason: tc.stop, blocks: tc.blocks}}
			want := []conversation.Message{out(tc.provider, tc.model, tc.want...)}
			if tc.later {
				in = append(in, llmMessage{role: "assistant", api: api, provider: "anthropic", model: id, stopReason: "stop", blocks: []block{answer}})
				want = append(want, user("Continue."), out("anthropic", id, text("answer")))
			}
			got, err := toRequest(in, target{api: api, provider: "anthropic", model: id, signing: tc.signing})
			if err != nil {
				t.Fatal(err)
			}
			assertMessages(t, got, want)
		})
	}
	t.Run("truncated thinking-only turn is not the latest", func(t *testing.T) {
		first := llmMessage{role: "assistant", api: api, provider: "anthropic", model: id, stopReason: "stop",
			blocks: []block{signed, {Type: "toolCall", ID: "tool", Name: "read"}}}
		short := llmMessage{role: "assistant", api: api, provider: "anthropic", model: id, stopReason: "length", blocks: []block{signed}}
		got, err := toRequest([]llmMessage{first, short}, target{api: api, provider: "anthropic", model: id, signing: true})
		if err != nil {
			t.Fatal(err)
		}
		assertMessages(t, got, []conversation.Message{
			out("anthropic", id, thinking("plan", "c2ln"), call("tool", "read", `{}`)),
			result("tool", "read", "No result provided", true),
		})
	})
	t.Run("malformed-only turn is not the latest", func(t *testing.T) {
		first := llmMessage{role: "assistant", api: api, provider: "anthropic", model: id, stopReason: "stop",
			blocks: []block{signed, {Type: "toolCall", ID: "tool", Name: "read"}}}
		malformed := llmMessage{role: "assistant", api: api, provider: "anthropic", model: id, stopReason: "stop",
			blocks: []block{{Type: "toolCall", ID: "malformed", Name: "read.invalid"}}}
		got, err := toRequest([]llmMessage{first, malformed}, target{api: api, provider: "anthropic", model: id, signing: true})
		if err != nil {
			t.Fatal(err)
		}
		assertMessages(t, got, []conversation.Message{
			out("anthropic", id, thinking("plan", "c2ln"), call("tool", "read", `{}`)),
			result("tool", "read", "No result provided", true),
		})
	})
	t.Run("same-model Responses encrypted reasoning has no text", func(t *testing.T) {
		const id = "gpt-test"
		const responses = conversation.OpenAIResponses
		const signature = `{"type":"reasoning","id":"rs_1","encrypted_content":"encrypted"}`
		m := llmMessage{role: "assistant", api: responses, provider: "openai", model: id, stopReason: "stop",
			blocks: []block{
				{Type: "thinking", ThinkingSignature: signature},
				{Type: "thinking", Thinking: "unsigned"}, {Type: "thinking", ThinkingSignature: "invalid"},
				{Type: "thinking", ThinkingSignature: `{"type":"message","id":"rs_not_reasoning"}`},
				{Type: "thinking", ThinkingSignature: `{"type":"reasoning","id":null}`},
				answer,
			}}
		got, err := toRequest([]llmMessage{m}, target{api: responses, provider: "openai", model: id})
		if err != nil {
			t.Fatal(err)
		}
		assertMessages(t, got, []conversation.Message{{Role: conversation.Assistant, API: responses, Provider: "openai", Model: id,
			Content: []conversation.Block{thinking("", signature), text("answer")}}})
	})
	t.Run("Responses native turn drops empty unanchored thinking blocks", func(t *testing.T) {
		const responses = conversation.OpenAIResponses
		m := llmMessage{role: "assistant", api: responses, provider: "openai", model: "gpt-test", stopReason: "stop",
			blocks:  []block{{Type: "thinking", Thinking: "\t"}, answer},
			payload: []byte(`{"type":"openaiResponsesHistory","items":[{"type":"message","role":"assistant","content":"answer"}]}`)}
		got, err := toRequest([]llmMessage{m}, target{api: responses, provider: "openai", model: "gpt-test"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Native == nil || len(got[0].Content) != 1 || got[0].Content[0].Text != "answer" {
			t.Fatalf("history = %+v, want native turn with only the visible text block", got)
		}
	})
	t.Run("Responses ignores display images and independent redacted thinking", func(t *testing.T) {
		const responses = conversation.OpenAIResponses
		m := llmMessage{role: "assistant", api: responses, provider: "openai", model: "gpt-test", stopReason: "stop",
			blocks: []block{{Type: "image", MimeType: "image/png", Data: "eA=="}, redacted, answer}}
		got, err := toRequest([]llmMessage{m}, target{api: responses, provider: "openai", model: "gpt-test"})
		if err != nil {
			t.Fatal(err)
		}
		assertMessages(t, got, []conversation.Message{{
			Role: conversation.Assistant, API: responses, Provider: "openai", Model: "gpt-test", Content: []conversation.Block{text("answer")},
		}})
	})
}

func ompAnthropicTarget() target {
	return target{api: conversation.AnthropicMessages, provider: "anthropic", model: "claude-test"}
}

func TestDemotedThinkingUsesPinnedDialect(t *testing.T) {
	for _, tc := range []struct{ id, in, want string }{
		{"anthropic/alias", "reasoning", "reasoning"},
		{"gemini-3", "reasoning", "```thinking\nreasoning\n```"},
		{"google/gemma-4", "reasoning", "<think>\nreasoning\n</think>"},
		{"gpt-6.1", "reasoning", "<think>\nreasoning\n</think>"},
		{"o4-mini", "reasoning", "<think>\nreasoning\n</think>"},
		{"deepseek-v4", "reasoning", "<think>\nreasoning\n</think>"},
		{"qwen3", "reasoning", "<think>\nreasoning\n</think>"},
		{"glm-5", "reasoning", "<think>\nreasoning\n</think>"},
		{"moonshotai/k3", "reasoning", "<think>\nreasoning\n</think>"},
		{"custom-model", " <thinking><thinking>reasoning</thinking></thinking> <thinking>next</thinking> ", "<thinking>\nreasoning\nnext\n</thinking>"},
		{"minimax-m2", "Prose <thinking>reasoning</thinking>", "<thinking>\nProse <thinking>reasoning</thinking>\n</thinking>"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			if got := demotedThinking(replayModelClass(tc.id), tc.in); got != tc.want {
				t.Errorf("thinking = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReadExactNativeCompactionPreservesBoundThinking(t *testing.T) {
	f := newFixture()
	f.add(userEntry("request"))
	kept := f.add(claudeEntry("stop", `[{"type":"thinking","thinking":"plan","thinkingSignature":"c2ln"},{"type":"text","text":"answer"}]`))
	f.add(`{"type":"compaction","summary":"Earlier work.","firstKeptEntryId":` + quote(kept) +
		`,"preserveData":{"anthropicCompaction":{"provider":"anthropic","content":"Native summary.","exactTail":true}}}`)
	messages := read(t, f.bytes()).Messages
	if len(messages) != 2 || messages[0].Native == nil {
		t.Fatalf("want native summary and retained turn, got %+v", messages)
	}
	assertMessages(t, messages[1:], []conversation.Message{claude(thinking("plan", "c2ln"), text("answer"))})
}
