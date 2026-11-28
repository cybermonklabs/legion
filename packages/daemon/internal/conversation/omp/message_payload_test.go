package omp

import (
	"encoding/json"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/conversation"
)

func TestReadPlacesAnthropicDeveloperMessages(t *testing.T) {
	system := func(blocks ...conversation.Block) conversation.Message {
		return conversation.Message{Role: conversation.System, Content: blocks}
	}
	scoped := system(text("Instruction"))
	scoped.Native = anthropicMessageControls(conversation.AnthropicControls{ClearAt: "next_user_message"})
	effort := system()
	effort.Native = anthropicMessageControls(conversation.AnthropicControls{Effort: "high"})
	add := conversation.Block{Type: conversation.ToolAddition, ToolName: "read"}
	remove := conversation.Block{Type: conversation.ToolRemoval, ToolName: "list"}
	const tools = `"toolChanges":[{"type":"tool_addition","name":"read"},{"type":"tool_removal","name":"list"}]`
	const clear = `"clearAt":"next_user_message"`
	const instructions = `[{"type":"text","text":"Instruction"}]`
	for _, tc := range []struct {
		name, model, attribution, content, payload string
		noPrecedingUser                            bool
		want                                       []conversation.Message
	}{
		{name: "blank controls survive", attribution: "user", content: `"  "`, payload: clear, noPrecedingUser: true, want: []conversation.Message{user()}},
		{name: "blank without controls drops", content: `[]`},
		{name: "blank changes survive", attribution: "user", content: `[]`, payload: tools, want: []conversation.Message{system(add, remove)}},
		{name: "empty changes are not controls", content: `[]`, payload: `"toolChanges":[]`},
		{name: "tool changes follow text", content: `"Instruction"`, payload: tools, want: []conversation.Message{system(text("Instruction"), add, remove)}},
		{name: "unsupported tools stay off", model: "claude-opus-4-7", content: instructions, payload: tools, want: []conversation.Message{user("Instruction")}},
		{name: "clear at on user attributed developer", attribution: "user", content: instructions, payload: clear, want: []conversation.Message{scoped}},
		{name: "attributed user without controls stays user", attribution: "user", content: instructions, want: []conversation.Message{user("Instruction")}},
		{name: "no attribution eligible", content: instructions, want: []conversation.Message{system(text("Instruction"))}},
		{name: "no preceding user stays user", content: instructions, payload: clear, noPrecedingUser: true, want: []conversation.Message{user("Instruction")}},
		{name: "effort only bypasses placement", attribution: "user", content: `[]`, payload: `"effort":"high"`, noPrecedingUser: true, want: []conversation.Message{effort}},
		{name: "split scoped text and tools", content: instructions, payload: clear + "," + tools, want: []conversation.Message{scoped, system(add, remove)}},
		{name: "split scoped text and effort", content: instructions, payload: clear + `,"effort":"high"`, want: []conversation.Message{scoped, effort}},
		{name: "effort on a string does not split", content: `"Instruction"`, payload: clear + `,"effort":"high"`, want: []conversation.Message{{
			Role: conversation.System, Content: []conversation.Block{text("Instruction")}, Native: effort.Native,
		}}},
		{name: "unsupported effort leaves scope", model: "claude-opus-4-8", content: instructions, payload: clear + `,"effort":"high"`, want: []conversation.Message{scoped}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			var want []conversation.Message
			if !tc.noPrecedingUser {
				f.add(userEntry("request"))
				want = append(want, user("request"))
			}
			record := `{"type":"message","message":{"role":"developer","content":` + tc.content
			if tc.attribution != "" {
				record += `,"attribution":` + quote(tc.attribution)
			}
			if tc.payload != "" {
				record += `,"providerPayload":{"type":"anthropicMessage",` + tc.payload + `}`
			}
			f.add(record + `}}`)
			model := tc.model
			if model == "" {
				model = "claude-opus-5-5"
			}
			f.add(assistantEntry("anthropic", conversation.AnthropicMessages, model, "stop", `[{"type":"text","text":"answer"}]`))
			answer := claude(text("answer"))
			answer.Model = model
			want = append(append(want, tc.want...), answer)
			assertMessages(t, read(t, f.bytes()).Messages, want)
		})
	}
}

func TestReadCarriesResponsesMessagePayload(t *testing.T) {
	const first = `{"type":"compaction","encrypted_content":"first"}`
	const second = `{"type":"message","role":"user","content":[{"type":"input_text","text":"native text"}]}`
	for _, tc := range []struct {
		role, provider, fallback string
		eligible                 bool
	}{
		{"user", `,"provider":"openai"`, `"fallback"`, true},
		{"developer", "", `"fallback"`, true},
		{"developer", `,"provider":"openai"`, `[]`, true},
		{"user", `,"provider":"other"`, `"fallback"`, false},
	} {
		name := tc.role + " native"
		if tc.fallback == `[]` {
			name += " without fallback"
		} else if !tc.eligible {
			name += " from different provider"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture()
			raw := `{"type":"openaiResponsesHistory"` + tc.provider + `,"items":[` + first + `,` + second + `]}`
			f.add(`{"type":"message","message":{"role":` + quote(tc.role) + `,"content":` + tc.fallback + `,"providerPayload":` + raw + `}}`)
			f.add(assistantEntry("openai", conversation.OpenAIResponses, "gpt-test", "stop", `[{"type":"text","text":"answer"}]`))
			want := user("fallback")
			if tc.fallback == `[]` {
				want.Content = nil
			}
			if tc.eligible {
				want.Native = &conversation.Native{
					Kind: conversation.OpenAIResponsesHistory, Provider: "openai", Payload: json.RawMessage(raw),
					Items: []json.RawMessage{json.RawMessage(first), json.RawMessage(second)},
				}
			}
			assertMessages(t, read(t, f.bytes()).Messages, []conversation.Message{want, {
				Role: conversation.Assistant, Provider: "openai", API: conversation.OpenAIResponses, Model: "gpt-test", Content: []conversation.Block{text("answer")},
			}})
		})
	}
}
