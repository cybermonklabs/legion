package omp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/conversation"
)

// responsesItem is one output item of a canned OpenAI Responses stream: the item as
// response.output_item.added announces it and as response.output_item.done completes it.
type responsesItem struct {
	added, done string
}

func reasoningItem(id, encrypted, summary string) responsesItem {
	return responsesItem{
		added: `{"id":"` + id + `","type":"reasoning","summary":[]}`,
		done:  `{"id":"` + id + `","type":"reasoning","encrypted_content":` + quote(encrypted) + `,"summary":[{"type":"summary_text","text":` + quote(summary) + `}]}`,
	}
}

func functionCallItem(id, callID, name, arguments string) responsesItem {
	return responsesItem{
		added: `{"id":"` + id + `","type":"function_call","status":"in_progress","call_id":"` + callID + `","name":"` + name + `","arguments":""}`,
		done:  `{"id":"` + id + `","type":"function_call","status":"completed","call_id":"` + callID + `","name":"` + name + `","arguments":` + quote(arguments) + `}`,
	}
}

func messageItem(id, text string) responsesItem {
	return responsesItem{
		added: `{"id":"` + id + `","type":"message","role":"assistant","status":"in_progress","content":[]}`,
		done:  `{"id":"` + id + `","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":` + quote(text) + `,"annotations":[]}]}`,
	}
}

// responsesStream is an OpenAI Responses streaming response of items.
func responsesStream(id string, items ...responsesItem) string {
	var b strings.Builder
	sequence := 0
	event := func(name, data string) {
		fmt.Fprintf(&b, "event: %s\ndata: {\"type\":%q,\"sequence_number\":%d,%s}\n\n", name, name, sequence, data)
		sequence++
	}
	response := func(status string) string {
		return `"response":{"id":"` + id + `","object":"response","created_at":1,"status":"` + status + `","model":"` + standInGPT +
			`","output":[],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":2}}}`
	}
	event("response.created", response("in_progress"))
	for i, item := range items {
		event("response.output_item.added", fmt.Sprintf(`"output_index":%d,"item":%s`, i, item.added))
		event("response.output_item.done", fmt.Sprintf(`"output_index":%d,"item":%s`, i, item.done))
	}
	event("response.completed", response("completed"))
	return b.String()
}

func newOpenAIStandIn(t *testing.T, replies ...string) *standIn {
	t.Helper()
	return newStandIn(t, responsesStream("resp_default", reasoningItem("rs_default", "gAAAAAresumed==", "Resuming."), messageItem("msg_default", "Resumed.")), replies...)
}

// The first run's two model calls on OpenAI Responses: reasoning and a read call, then reasoning
// and the answer.
func firstOpenAIRunReplies() []string {
	return []string{
		responsesStream("resp_1", reasoningItem("rs_1", "gAAAAENC+/one==", "Need the note."), functionCallItem("fc_1", "call_1", "read", `{"path":"note.txt"}`)),
		responsesStream("resp_2", reasoningItem("rs_2", "gAAAAENC+/two==", "It says plum."), messageItem("msg_2", "The note says plum.")),
	}
}

// requestInput returns the Responses input items without the session's journaled reminders.
func requestInput(t *testing.T, body, content []byte) []map[string]any {
	t.Helper()
	var request struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatalf("decode the request: %v\n%s", err, body)
	}
	journal := readReminderJournal(t, content)
	var kept []map[string]any
	for _, item := range request.Input {
		parts, _ := item["content"].([]any)
		if len(parts) > 0 {
			first, _ := parts[0].(map[string]any)
			text, _ := first["text"].(string)
			if item["role"] == "developer" && first["type"] == "input_text" && len(parts) == 1 && journal.takeControl(text) {
				continue
			}
			if item["role"] == "user" && first["type"] == "input_text" {
				if rest, ok := journal.cutPrefix(text, "\n\n"); ok {
					if rest != "" {
						first["text"] = rest
					} else {
						item["content"] = parts[1:]
					}
				}
			}
		}
		kept = append(kept, item)
	}
	return kept
}

// readerInput is the reader's conversation as the input items of a Responses request: a turn's
// native output items as Oh My Pi replays them, without their item id and status (its replay
// sanitizer drops both; every other field, encrypted_content included, goes as stored).
func readerInput(t *testing.T, messages []conversation.Message) []map[string]any {
	t.Helper()
	var input []map[string]any
	for _, m := range messages {
		switch m.Role {
		case conversation.User:
			var content []any
			for _, b := range m.Content {
				content = append(content, map[string]any{"type": "input_text", "text": b.Text})
			}
			input = append(input, map[string]any{"role": "user", "content": content})
		case conversation.Assistant:
			if m.Native == nil {
				t.Fatalf("an assistant turn of the model with no native items: %+v", m)
			}
			for _, raw := range m.Native.Items {
				var item map[string]any
				if err := json.Unmarshal(raw, &item); err != nil {
					t.Fatal(err)
				}
				delete(item, "id")
				delete(item, "status")
				input = append(input, item)
			}
		case conversation.Tool:
			result := m.Content[0]
			var texts []string
			for _, part := range result.Content {
				texts = append(texts, part.Text)
			}
			input = append(input, map[string]any{"type": "function_call_output", "call_id": result.ToolCallID, "output": strings.Join(texts, "\n")})
		}
	}
	return input
}

// assertResponsesResumeMatches checks the Responses request the pinned Oh My Pi sent on resuming
// a session as assertResumeMatches checks an Anthropic one, and returns the request's input items.
func assertResponsesResumeMatches(t *testing.T, before *Session, content, body []byte, followUp string) []map[string]any {
	t.Helper()
	resumed := resumedSession(t, before, content, Options{}, followUp)
	got := requestInput(t, body, content)
	want := append(readerInput(t, resumed.Messages), map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": followUp}}})
	observed, _ := json.Marshal(got)
	t.Logf("Oh My Pi's resume request input: %s", observed)
	wantJSON, _ := json.Marshal(want)
	var wantDecoded []map[string]any
	if err := json.Unmarshal(wantJSON, &wantDecoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, wantDecoded) {
		t.Errorf("Oh My Pi's resume request input:\n%s\nthe reader's conversation and the follow-up:\n%s", observed, wantJSON)
	}
	if resumed.Model != standInProvider+"/"+standInGPT || resumed.API != conversation.OpenAIResponses {
		t.Errorf("the reader's model %q, API %q", resumed.Model, resumed.API)
	}
	assertBlankedOnly(t, before, resumed)
	return got
}

// The pinned Oh My Pi on OpenAI Responses, resuming a session of reasoning and a tool call, sends
// each turn's output items as the session holds them, the reasoning items' encrypted content byte
// for byte, which is what the reader carries as the turns' native items.
func TestThePinnedOhMyPiReplaysTheOpenAIItemsTheReaderCarries(t *testing.T) {
	model := newOpenAIStandIn(t, firstOpenAIRunReplies()...)
	rig := newOMPRig(t, conversation.OpenAIResponses, model)
	rig.run(t, "", "Read note.txt and tell me what it says.")
	file := rig.sessionFile(t)
	before, err := ReadFile(file, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var encrypted []string
	for _, m := range before.Messages {
		if m.Native == nil {
			continue
		}
		for _, item := range m.Native.Items {
			var reasoning struct {
				Type             string `json:"type"`
				EncryptedContent string `json:"encrypted_content"`
			}
			if json.Unmarshal(item, &reasoning) == nil && reasoning.Type == "reasoning" {
				encrypted = append(encrypted, reasoning.EncryptedContent)
			}
		}
	}
	if strings.Join(encrypted, " ") != "gAAAAENC+/one== gAAAAENC+/two==" {
		t.Errorf("the reader's reasoning items carry %q, want the model's encrypted content as it came", encrypted)
	}

	rig.run(t, file, "What colour is the fruit?")
	assertResponsesResumeMatches(t, before, sessionContent(t, file), model.last(t), "What colour is the fruit?")
}

// itemCarriedOnce is the index in input of the last item match accepts, -1 when none is, and
// marks the test failed, naming the item as name, unless exactly one is: each part of an
// appended answer reaches the model once.
func itemCarriedOnce(t *testing.T, input []map[string]any, match func(map[string]any) bool, name string) int {
	t.Helper()
	index, count := -1, 0
	for i, item := range input {
		if match(item) {
			index = i
			count++
		}
	}
	if count != 1 {
		t.Errorf("the request carries %s %d times, want once", name, count)
	}
	return index
}

// userText is the text of a user message item of one part, and "" for any other item.
func userText(item map[string]any) string {
	parts, _ := item["content"].([]any)
	if item["role"] != "user" || len(parts) != 1 {
		return ""
	}
	text, _ := parts[0].(map[string]any)["text"].(string)
	return text
}

// messageText is the text of a message item of one part, the model's reply, and "" for any
// other item.
func messageText(item map[string]any) string {
	content, _ := item["content"].([]any)
	if item["type"] != "message" || len(content) != 1 {
		return ""
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	return text
}

// An answer appended on OpenAI Responses reaches the model when the pinned Oh My Pi resumes: its
// next request carries the question, the reasoning items with their encrypted content as the
// append wrote them, the call, its output and the answer, once each and in order.
func TestThePinnedOhMyPiResumesAnAppendedOpenAIAnswer(t *testing.T) {
	model := newOpenAIStandIn(t, firstOpenAIRunReplies()...)
	rig := newOMPRig(t, conversation.OpenAIResponses, model)
	rig.run(t, "", "Read note.txt and tell me what it says.")
	file := rig.sessionFile(t)
	before, err := ReadFile(file, Options{})
	if err != nil {
		t.Fatal(err)
	}
	item := func(raw string) json.RawMessage { return json.RawMessage(raw) }
	answer := conversation.Append{
		QuestionID: "q-1", Question: "Which fruit does the note name?",
		Provider: standInProvider, API: conversation.OpenAIResponses, Model: standInGPT,
		Turns: []conversation.Turn{
			{
				Content: []conversation.Block{thinking("Check the note.", ""), call("call_append_1", "read", `{"path":"note.txt"}`)},
				Native: []json.RawMessage{
					item(reasoningItem("rs_append_1", "gAAAAAPPEND+/1==", "Check the note.").done),
					item(functionCallItem("fc_append_1", "call_append_1", "read", `{"path":"note.txt"}`).done),
				},
				Results: []conversation.Block{{Type: conversation.ToolResult, ToolCallID: "call_append_1", ToolName: "read", Content: []conversation.Block{text("plum")}}},
				Usage:   conversation.Usage{InputTokens: 12, OutputTokens: 8, ReasoningTokens: 3},
			},
			{
				Content: []conversation.Block{thinking("It names plum.", ""), text("The note names plum.")},
				Native: []json.RawMessage{
					item(reasoningItem("rs_append_2", "gAAAAAPPEND+/2==", "It names plum.").done),
					item(messageItem("msg_append_2", "The note names plum.").done),
				},
				Usage: conversation.Usage{InputTokens: 20, OutputTokens: 5},
			},
		},
	}
	if err := AppendFile(file, before, answer, time.Now()); err != nil {
		t.Fatal(err)
	}
	appended, err := ReadFile(file, Options{})
	if err != nil {
		t.Fatal(err)
	}
	rig.run(t, file, "Thanks.")
	input := assertResponsesResumeMatches(t, appended, sessionContent(t, file), model.last(t), "Thanks.")

	question := itemCarriedOnce(t, input, func(item map[string]any) bool { return userText(item) == answer.Question }, "the question")
	firstReasoning := itemCarriedOnce(t, input, func(item map[string]any) bool { return item["encrypted_content"] == "gAAAAAPPEND+/1==" }, "the first reasoning item")
	toolCall := itemCarriedOnce(t, input, func(item map[string]any) bool {
		return item["type"] == "function_call" && item["call_id"] == "call_append_1"
	}, "the call")
	output := itemCarriedOnce(t, input, func(item map[string]any) bool {
		return item["type"] == "function_call_output" && item["call_id"] == "call_append_1" && item["output"] == "plum"
	}, "the call's output")
	secondReasoning := itemCarriedOnce(t, input, func(item map[string]any) bool { return item["encrypted_content"] == "gAAAAAPPEND+/2==" }, "the second reasoning item")
	finalAnswer := itemCarriedOnce(t, input, func(item map[string]any) bool { return messageText(item) == "The note names plum." }, "the answer")
	order := []int{question, firstReasoning, toolCall, output, secondReasoning, finalAnswer, len(input) - 1}
	for i := 1; i < len(order); i++ {
		if order[i-1] >= order[i] {
			t.Errorf("the question, reasoning, call, output, reasoning, answer and follow-up at %v of %d items; want them in that order", order, len(input))
			break
		}
	}
}

// After resuming an appended exchange, Oh My Pi sends the question, the first reply's reasoning
// and message items, the retry prompt, the second reply's items and the follow-up in that order.
// Each appears once.
func TestThePinnedOhMyPiResumesAnOpenAIAnswerAskedForAgain(t *testing.T) {
	model := newOpenAIStandIn(t, firstOpenAIRunReplies()...)
	rig := newOMPRig(t, conversation.OpenAIResponses, model)
	rig.run(t, "", "Read note.txt and tell me what it says.")
	file := rig.sessionFile(t)
	before, err := ReadFile(file, Options{})
	if err != nil {
		t.Fatal(err)
	}
	item := func(raw string) json.RawMessage { return json.RawMessage(raw) }
	answer := conversation.Append{
		QuestionID: "q-1", Question: "Which fruit does the note name?",
		Provider: standInProvider, API: conversation.OpenAIResponses, Model: standInGPT,
		Turns: []conversation.Turn{
			{
				Content: []conversation.Block{thinking("I cannot tell from here.", ""), text(refusedReply)},
				Native: []json.RawMessage{
					item(reasoningItem("rs_refused", "gAAAARFSD+/1==", "I cannot tell from here.").done),
					item(messageItem("msg_refused", refusedReply).done),
				},
				AskedAgain: askedAgain,
				Usage:      conversation.Usage{InputTokens: 12, OutputTokens: 8, ReasoningTokens: 3},
			},
			{
				Content: []conversation.Block{thinking("Name what I need.", ""), text(replyAgain)},
				Native: []json.RawMessage{
					item(reasoningItem("rs_again", "gAAAAAGAIN+/2==", "Name what I need.").done),
					item(messageItem("msg_again", replyAgain).done),
				},
				Usage: conversation.Usage{InputTokens: 20, OutputTokens: 5},
			},
		},
	}
	if err := AppendFile(file, before, answer, time.Now()); err != nil {
		t.Fatal(err)
	}
	appended, err := ReadFile(file, Options{})
	if err != nil {
		t.Fatal(err)
	}
	rig.run(t, file, "Thanks.")
	input := assertResponsesResumeMatches(t, appended, sessionContent(t, file), model.last(t), "Thanks.")

	// The reader rebuilds from the same appended session Oh My Pi resumes, so a re-ask the
	// appender dropped or wrote out of place would leave the two agreeing; these checks hold the
	// request to the order the run made.
	question := itemCarriedOnce(t, input, func(item map[string]any) bool { return userText(item) == answer.Question }, "the question")
	refusedReasoning := itemCarriedOnce(t, input, func(item map[string]any) bool { return item["encrypted_content"] == "gAAAARFSD+/1==" }, "the refused reply's reasoning item")
	refused := itemCarriedOnce(t, input, func(item map[string]any) bool { return messageText(item) == refusedReply }, "the refused reply")
	reAsk := itemCarriedOnce(t, input, func(item map[string]any) bool { return userText(item) == askedAgain }, "the message the loop asked again with")
	againReasoning := itemCarriedOnce(t, input, func(item map[string]any) bool { return item["encrypted_content"] == "gAAAAAGAIN+/2==" }, "the reply's reasoning item")
	accepted := itemCarriedOnce(t, input, func(item map[string]any) bool { return messageText(item) == replyAgain }, "the reply after it")
	order := []int{question, refusedReasoning, refused, reAsk, againReasoning, accepted, len(input) - 1}
	for i := 1; i < len(order); i++ {
		if order[i] != order[i-1]+1 {
			t.Errorf("the question, the refused reply's reasoning and message, the message it was asked again with, the reply's reasoning and message and the follow-up at %v of %d items; want them one after another",
				order, len(input))
			break
		}
	}
}

func TestThePinnedOhMyPiShapesHandWrittenResponsesHistory(t *testing.T) {
	model := newOpenAIStandIn(t)
	rig := newOMPRig(t, conversation.OpenAIResponses, model)
	rig.run(t, "", "Initial request.")
	file := rig.sessionFile(t)
	before, err := ReadFile(file, Options{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	stamp := conversation.ISOTime(now)
	appendRaw(t, file, fmt.Sprintf(
		`{"type":"message","id":"d6000001","parentId":%q,"timestamp":%q,"message":{"role":"assistant","api":%q,"provider":%q,"model":%q,"stopReason":"toolUse","usage":%s,"timestamp":%d,"content":[{"type":"text","text":"Before the call.","textSignature":"{\"v\":1,\"id\":\"msg_before\",\"phase\":\"commentary\"}"},{"type":"toolCall","id":"call_fixture|fc_fixture","name":"read","arguments":null},{"type":"text","text":" "},{"type":"text","text":"After the call."}]}}`+"\n"+
			`{"type":"message","id":"d6000002","parentId":"d6000001","timestamp":%q,"message":{"role":"toolResult","toolCallId":"call_fixture|fc_fixture","toolName":"read","content":[{"type":"text","text":"plum"}],"timestamp":%d}}`+"\n"+
			`{"type":"message","id":"d6000003","parentId":"d6000002","timestamp":%q,"message":{"role":"assistant","api":%q,"provider":%q,"model":%q,"stopReason":"stop","usage":%s,"timestamp":%d,"content":[{"type":"text","text":"Fallback answer."}],"providerPayload":{"type":"openaiResponsesHistory","provider":%q,"dt":true,"items":[{"type":"item_reference","id":"msg_stale"},{"type":"function_call","call_id":"call_bad","name":"bad name","arguments":"{}"},{"type":"function_call_output","call_id":"call_bad","output":"must not replay"},{"type":"image_generation_call","id":"ig_incomplete"},{"type":"function_call","call_id":"call_native","name":"read","arguments":" "},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Native answer.","annotations":[]}],"status":"completed"}]}}}`+"\n"+
			`{"type":"message","id":"d6000004","parentId":"d6000003","timestamp":%q,"message":{"role":"assistant","api":%q,"provider":%q,"model":%q,"stopReason":"stop","usage":%s,"timestamp":%d,"content":[{"type":"thinking","thinking":"Do not replay.","thinkingSignature":"{\"type\":\"reasoning\",\"id\":\"rs_hidden\",\"encrypted_content\":\"hidden\"}"},{"type":"text","text":"\t"},{"type":"text","text":"Use visible fallback."}],"providerPayload":{"type":"openaiResponsesHistory","provider":%q,"dt":true,"items":[{"type":"reasoning","id":"rs_hidden","encrypted_content":"hidden","summary":[]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"\t","annotations":[]}],"status":"completed"}]}}}`+"\n",
		before.Leaf, stamp, conversation.OpenAIResponses, standInProvider, standInGPT, handWrittenUsage, now.UnixMilli(),
		stamp, now.UnixMilli(), stamp, conversation.OpenAIResponses, standInProvider, standInGPT, handWrittenUsage, now.UnixMilli(), standInProvider,
		stamp, conversation.OpenAIResponses, standInProvider, standInGPT, handWrittenUsage, now.UnixMilli(), standInProvider))
	rig.run(t, file, "Follow-up.")
	body := model.last(t)
	input := requestInput(t, body, sessionContent(t, file))
	formatted, _ := json.Marshal(input)
	t.Logf("Pinned Responses input: %s", formatted)
	after := itemCarriedOnce(t, input, func(item map[string]any) bool { return messageText(item) == "After the call." }, "trailing assistant text")
	phase := itemCarriedOnce(t, input, func(item map[string]any) bool {
		return messageText(item) == "Before the call." && item["phase"] == "commentary"
	}, "assistant text with a recorded phase")
	blankText := itemCarriedOnce(t, input, func(item map[string]any) bool { return messageText(item) == " " }, "blank assistant text")
	if phase >= blankText || blankText >= after {
		t.Errorf("want phased, blank, and trailing text in order; got indexes %d, %d, %d", phase, blankText, after)
	}
	rebuilt := resumedSession(t, before, sessionContent(t, file), Options{}, "Follow-up.")
	var replay []map[string]any
	for _, msg := range rebuilt.Messages {
		if msg.Native == nil {
			continue
		}
		for _, raw := range msg.Native.Items {
			var item map[string]any
			if err := json.Unmarshal(raw, &item); err != nil {
				t.Fatal(err)
			}
			replay = append(replay, item)
		}
	}
	foundFallback := false
	for _, msg := range rebuilt.Messages {
		for _, b := range msg.Content {
			if b.Type == conversation.Text && b.Text == "Use visible fallback." {
				foundFallback = true
				if msg.Native != nil || !reflect.DeepEqual(msg.Content, []conversation.Block{text("Use visible fallback.")}) {
					t.Errorf("hidden-empty history did not fall back to visible text only: %+v", msg)
				}
			}
		}
	}
	if !foundFallback {
		t.Error("reader did not retain the visible fallback")
	}
	itemCarriedOnce(t, replay, func(item map[string]any) bool {
		return item["type"] == "function_call" && item["call_id"] == "call_native" && item["arguments"] == "{}"
	}, "reader's native call with repaired arguments")
	for _, item := range replay {
		if item["type"] == "item_reference" {
			t.Errorf("reader kept a native item_reference: %v", item)
		}
	}
	found := false
	for _, msg := range rebuilt.Messages {
		if len(msg.Content) == 0 || msg.Content[0].Text != "Before the call." {
			continue
		}
		found = true
		want := []conversation.Block{
			{Type: conversation.Text, Text: "Before the call.", Phase: "commentary", ItemID: "msg_before"},
			text(" "), text("After the call."),
			{Type: conversation.ToolCall, ToolCallID: "call_fixture", ToolName: "read", ItemID: "fc_fixture", Arguments: json.RawMessage("null")},
		}
		if !reflect.DeepEqual(msg.Content, want) {
			t.Errorf("reader blocks = %+v, want %+v", msg.Content, want)
		}
	}
	if !found {
		t.Error("reader dropped the hand-written tool turn")
	}
	call := itemCarriedOnce(t, input, func(item map[string]any) bool {
		return item["type"] == "function_call" && item["call_id"] == "call_fixture" && item["arguments"] == "null"
	}, "rebuilt call with null arguments")
	output := itemCarriedOnce(t, input, func(item map[string]any) bool {
		return item["type"] == "function_call_output" && item["call_id"] == "call_fixture"
	}, "rebuilt call output")
	if after >= call || call >= output {
		t.Errorf("want hoisted text, call, output in order; got indexes %d, %d, %d", after, call, output)
	}
	itemCarriedOnce(t, input, func(item map[string]any) bool {
		return item["type"] == "function_call" && item["call_id"] == "call_native" && item["arguments"] == "{}"
	}, "native call with repaired arguments")
	itemCarriedOnce(t, input, func(item map[string]any) bool {
		return item["type"] == "function_call_output" && item["call_id"] == "call_native" &&
			item["output"] == "[No tool output recorded: the tool call was interrupted before it produced a result.]"
	}, "native call's inserted result")
	itemCarriedOnce(t, input, func(item map[string]any) bool {
		return messageText(item) == "Use visible fallback."
	}, "visible fallback for a hidden-empty payload")
	for _, item := range input {
		if item["type"] == "item_reference" || item["type"] == "image_generation_call" ||
			item["call_id"] == "call_bad" || item["encrypted_content"] == "hidden" || messageText(item) == "\t" {
			t.Errorf("the pin replayed an item that should drop: %v", item)
		}
	}
}

const openAIStandInModel = "gpt-6.2-sol"

func TestThePinnedOhMyPiReplaysSavedResponsesBlockItemIDs(t *testing.T) {
	model := newOpenAIStandIn(t)
	rig := newOMPRigForProviderModel(t, conversation.OpenAIResponses, "openai", openAIStandInModel, model)
	rig.run(t, "", "Initial request.")
	file := rig.sessionFile(t)
	before, err := ReadFile(file, Options{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	stamp := conversation.ISOTime(now)
	const signature = `{"type":"reasoning","id":"rs_keep","summary":[],"encrypted_content":"opaque"}`
	appendRaw(t, file, fmt.Sprintf(
		`{"type":"message","id":"d6100001","parentId":%q,"timestamp":%q,"message":{"role":"assistant","api":%q,"provider":%q,"model":%q,"stopReason":"toolUse","usage":%s,"timestamp":%d,"content":[{"type":"thinking","thinking":"","thinkingSignature":%s},{"type":"text","text":"Calling.","textSignature":"{\"v\":1,\"id\":\"msg_keep\"}"},{"type":"toolCall","id":"call_k|fc_keep","name":"read","arguments":{}}]}}`+"\n"+
			`{"type":"message","id":"d6100002","parentId":"d6100001","timestamp":%q,"message":{"role":"toolResult","toolCallId":"call_k|fc_keep","toolName":"read","content":[{"type":"text","text":"plum"}],"timestamp":%d}}`+"\n",
		before.Leaf, stamp, conversation.OpenAIResponses, "openai", openAIStandInModel, handWrittenUsage, now.UnixMilli(), quote(signature), stamp, now.UnixMilli()))
	rig.run(t, file, "Follow-up.")
	content := sessionContent(t, file)
	input := requestInput(t, model.last(t), content)
	reason := itemCarriedOnce(t, input, func(item map[string]any) bool {
		return item["type"] == "reasoning" && item["id"] == "rs_keep"
	}, "saved reasoning item")
	msg := itemCarriedOnce(t, input, func(item map[string]any) bool {
		return messageText(item) == "Calling." && item["id"] == "msg_keep"
	}, "saved message item")
	tool := itemCarriedOnce(t, input, func(item map[string]any) bool {
		return item["type"] == "function_call" && item["call_id"] == "call_k" && item["id"] == "fc_keep"
	}, "saved tool item")
	if reason >= msg || msg >= tool {
		t.Errorf("reasoning, text and call appeared at indexes %d, %d, %d", reason, msg, tool)
	}
	resumed := resumedSession(t, before, content, Options{}, "Follow-up.")
	for _, m := range resumed.Messages {
		if m.Role != conversation.Assistant || len(m.Content) == 0 || m.Content[0].Signature != signature {
			continue
		}
		want := []conversation.Block{
			thinking("", signature),
			{Type: conversation.Text, Text: "Calling.", ItemID: "msg_keep"},
			{Type: conversation.ToolCall, ToolCallID: "call_k", ToolName: "read", ItemID: "fc_keep", Arguments: json.RawMessage("{}")},
		}
		if m.Native != nil || !reflect.DeepEqual(m.Content, want) {
			t.Fatalf("reader assistant = %+v, want block replay with %+v", m, want)
		}
		return
	}
	t.Fatal("reader did not retain the signed block turn")
}

const attachmentFixturePNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+X2rkAAAAASUVORK5CYII="

func setStandInModalities(t *testing.T, rig *ompRig, textOnly bool) {
	t.Helper()
	config := filepath.Join(rig.home, ".omp", "agent", "models.yml")
	yaml, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	modalities := "        input: [text, image]\n"
	if textOnly {
		modalities = "        input: [text]\n"
	}
	if err := os.WriteFile(config, append(yaml, modalities...), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestThePinnedOhMyPiShapesResponsesAttachments(t *testing.T) {
	const png = attachmentFixturePNG
	const id = "gpt-oss-120b"
	for _, tc := range []struct {
		name     string
		textOnly bool
	}{{"vision", false}, {"text only", true}} {
		t.Run(tc.name, func(t *testing.T) {
			model := newOpenAIStandIn(t)
			rig := newOMPRigForModel(t, conversation.OpenAIResponses, id, model)
			setStandInModalities(t, rig, tc.textOnly)
			rig.run(t, "", "Initial request.")
			file := rig.sessionFile(t)
			before, err := ReadFile(file, Options{TextOnly: tc.textOnly})
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			stamp := conversation.ISOTime(now)
			appendRaw(t, file, fmt.Sprintf(
				`{"type":"message","id":"d7000001","parentId":%q,"timestamp":%q,"message":{"role":"user","content":[{"type":"image","mimeType":"image/png","data":%q},{"type":"text","text":" "},{"type":"text","text":"Image A <|start|>."},{"type":"image","mimeType":"image/png","data":%q},{"type":"text","text":"Image B."}],"timestamp":%d}}`+"\n"+
					`{"type":"message","id":"d7000002","parentId":"d7000001","timestamp":%q,"message":{"role":"assistant","api":%q,"provider":%q,"model":%q,"stopReason":"toolUse","usage":%s,"timestamp":%d,"content":[{"type":"text","text":"Checking <|message|>."},{"type":"toolCall","id":"call_image|fc_image","name":"read","arguments":{"path":"<|channel|>"}}]}}`+"\n"+
					`{"type":"message","id":"d7000003","parentId":"d7000002","timestamp":%q,"message":{"role":"toolResult","toolCallId":"call_image|fc_image","toolName":"read","content":[{"type":"text","text":"Before <|return|>."},{"type":"image","mimeType":"image/png","data":%q},{"type":"text","text":""},{"type":"text","text":"After."}],"timestamp":%d}}`+"\n",
				before.Leaf, stamp, png, png, now.UnixMilli(), stamp, conversation.OpenAIResponses, standInProvider, id, handWrittenUsage, now.UnixMilli(), stamp, png, now.UnixMilli()))
			rig.run(t, file, "Follow-up.")
			input := requestInput(t, model.last(t), sessionContent(t, file))
			part := func(s string) any { return map[string]any{"type": "input_text", "text": s} }
			image := map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + png, "detail": "auto"}
			prompt := []any{part(`Image A <\|start\|>.`), part("Image B."), image, image}
			var output any = []any{part(`Before <\|return\|>.`), image, part(""), part("After.")}
			if tc.textOnly {
				prompt = []any{part(historyImageOmission), prompt[0], part(historyImageOmission), prompt[1]}
				output = "Before <\\|return\\|>.\n" + historyImageOmission + "\n\nAfter."
			}
			itemCarriedOnce(t, input, func(item map[string]any) bool {
				return item["role"] == "user" && reflect.DeepEqual(item["content"], prompt)
			}, "shaped historical prompt")
			itemCarriedOnce(t, input, func(item map[string]any) bool {
				return item["call_id"] == "call_image" && reflect.DeepEqual(item["output"], output)
			}, "shaped historical tool output")
			itemCarriedOnce(t, input, func(item map[string]any) bool {
				return item["call_id"] == "call_image" && item["arguments"] == `{"path":"<\\|channel\\|>"}`
			}, "escaped function arguments")
			itemCarriedOnce(t, input, func(item map[string]any) bool {
				return messageText(item) == `Checking <\|message\|>.`
			}, "escaped assistant text")
			if t.Failed() {
				wire, _ := json.Marshal(input)
				t.Logf("Pinned Responses input: %s", wire)
			}
		})
	}
}

func TestThePinnedOhMyPiCarriesRecordedResponsesImageDetails(t *testing.T) {
	model := newOpenAIStandIn(t)
	rig := newOMPRigForProviderModel(t, conversation.OpenAIResponses, "openai", openAIStandInModel, model)
	setStandInModalities(t, rig, false)
	rig.run(t, "", "Initial request.")
	file := rig.sessionFile(t)
	before, err := ReadFile(file, Options{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	stamp := conversation.ISOTime(now)
	const png = attachmentFixturePNG
	appendRaw(t, file, fmt.Sprintf(
		`{"type":"message","id":"d7100001","parentId":%q,"timestamp":%q,"message":{"role":"user","content":[{"type":"image","mimeType":"image/png","data":%q,"detail":"low"},{"type":"image","mimeType":"image/png","data":%q}],"timestamp":%d}}`+"\n"+
			`{"type":"message","id":"d7100002","parentId":"d7100001","timestamp":%q,"message":{"role":"assistant","api":%q,"provider":"openai","model":%q,"stopReason":"toolUse","usage":%s,"timestamp":%d,"content":[{"type":"toolCall","id":"call_detail|fc_detail","name":"read","arguments":{"path":"note.txt"}}]}}`+"\n"+
			`{"type":"message","id":"d7100003","parentId":"d7100002","timestamp":%q,"message":{"role":"toolResult","toolCallId":"call_detail|fc_detail","toolName":"read","content":[{"type":"image","mimeType":"image/png","data":%q,"detail":"high"}],"timestamp":%d}}`+"\n",
		before.Leaf, stamp, png, png, now.UnixMilli(), stamp, conversation.OpenAIResponses, openAIStandInModel, handWrittenUsage, now.UnixMilli(), stamp, png, now.UnixMilli()))
	before, err = ReadFile(file, Options{})
	if err != nil {
		t.Fatal(err)
	}
	rig.run(t, file, "Follow-up.")
	content := sessionContent(t, file)
	input := requestInput(t, model.last(t), content)
	image := func(detail string) map[string]any {
		return map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + png, "detail": detail}
	}
	itemCarriedOnce(t, input, func(item map[string]any) bool {
		return item["role"] == "user" && reflect.DeepEqual(item["content"], []any{image("low"), image("auto")})
	}, "prompt images with low and default detail")
	itemCarriedOnce(t, input, func(item map[string]any) bool {
		return item["type"] == "function_call_output" && item["call_id"] == "call_detail" &&
			reflect.DeepEqual(item["output"], []any{image("high")})
	}, "tool image with high detail")
	resumed := resumedSession(t, before, content, Options{}, "Follow-up.")
	var promptDetails, resultDetails []string
	for _, msg := range resumed.Messages {
		for _, b := range msg.Content {
			switch {
			case msg.Role == conversation.User && b.Type == conversation.Image:
				promptDetails = append(promptDetails, b.Detail)
			case b.Type == conversation.ToolResult && b.ToolCallID == "call_detail":
				for _, part := range b.Content {
					if part.Type == conversation.Image {
						resultDetails = append(resultDetails, part.Detail)
					}
				}
			}
		}
	}
	if !reflect.DeepEqual(promptDetails, []string{"low", ""}) || !reflect.DeepEqual(resultDetails, []string{"high"}) {
		t.Errorf("reader prompt image details = %q, result image details = %q; want [low, unrecorded] and [high]", promptDetails, resultDetails)
	}
}

func TestThePinnedOhMyPiReplacesAnthropicHistoryAttachmentsBeforeEncoding(t *testing.T) {
	model := newAnthropicStandIn(t)
	rig := newOMPRig(t, conversation.AnthropicMessages, model)
	setStandInModalities(t, rig, true)
	opts := Options{ReplayUnsignedThinking: true, TextOnly: true}
	rig.run(t, "", "Initial request.")
	file := rig.sessionFile(t)
	before, err := ReadFile(file, opts)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	stamp := conversation.ISOTime(now)
	image := `{"type":"image","mimeType":"image/png","data":` + quote(attachmentFixturePNG) + `}`
	appendRaw(t, file, fmt.Sprintf(
		`{"type":"message","id":"d8000001","parentId":%q,"timestamp":%q,"message":{"role":"user","content":[{"type":"text","text":"First."},%s,%s,{"type":"text","text":"Last."}],"timestamp":%d}}`+"\n"+
			`{"type":"message","id":"d8000002","parentId":"d8000001","timestamp":%q,"message":{"role":"assistant","api":%q,"provider":%q,"model":%q,"stopReason":"toolUse","usage":%s,"timestamp":%d,"content":[{"type":"toolCall","id":"toolu_image","name":"read","arguments":{"path":"note.txt"}}]}}`+"\n"+
			`{"type":"message","id":"d8000003","parentId":"d8000002","timestamp":%q,"message":{"role":"toolResult","toolCallId":"toolu_image","toolName":"read","content":[{"type":"text","text":"Before."},%s,{"type":"text","text":"After."}],"timestamp":%d}}`+"\n",
		before.Leaf, stamp, image, image, now.UnixMilli(), stamp, conversation.AnthropicMessages, standInProvider, standInModel, handWrittenUsage, now.UnixMilli(), stamp, image, now.UnixMilli()))
	before, err = ReadFile(file, opts)
	if err != nil {
		t.Fatal(err)
	}
	const followUp = "Follow-up."
	rig.run(t, file, followUp)
	saved := sessionContent(t, file)
	rebuilt := resumedSession(t, before, saved, opts, followUp)
	got := model.lastAnthropic(t).turns(t, saved)
	want := append(readerTurns(t, rebuilt.Messages), wireTurn{Role: "user", Blocks: []wireBlock{{Type: "text", Text: followUp}}})
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pinned request = %+v\nreader and follow-up = %+v", got, want)
	}
	carriedOnce(t, got, func(turn wireTurn, b wireBlock) bool {
		return turn.Role == "user" && b.Type == "text" && b.Text == "First.\n"+historyImageOmission+"\nLast."
	}, "coalesced image omission between prompt text")
	carriedOnce(t, got, func(turn wireTurn, b wireBlock) bool {
		return turn.Role == "user" && b.Type == "tool_result" && b.Result == "Before.\n"+historyImageOmission+"\nAfter."
	}, "image omission between tool result text")
}
