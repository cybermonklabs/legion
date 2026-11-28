package omp

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/conversation"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
)

func TestThePinnedOhMyPiReplaysSavedNativeBlocksAndImages(t *testing.T) {
	const model = "claude-opus-5-5"
	endpoint := newAnthropicStandIn(t)
	rig := newOMPRigForModel(t, conversation.AnthropicMessages, model, endpoint)
	agent := filepath.Join(rig.home, ".omp", "agent")
	setStandInModalities(t, rig, false)
	appendRaw(t, filepath.Join(agent, "models.yml"), "        compat:\n          bedrockMessagesApi: true\n          supportsServerCompaction: true\n")
	rig.run(t, "", "Initial request.")
	path := rig.sessionFile(t)
	initial, err := ReadFile(path, standInEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	png, err := base64.StdEncoding.DecodeString(attachmentFixturePNG)
	if err != nil {
		t.Fatal(err)
	}
	hash := ompsessions.Summarize(png).SHA256
	blobDir := filepath.Join(agent, "blobs")
	if err := os.MkdirAll(blobDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blobDir, hash), png, 0o600); err != nil {
		t.Fatal(err)
	}

	const serverTool = `{"type":"server_tool_use","id":"srvtoolu_one","name":"web_search","input":{"query":"plum"}}`
	const toolResult = `{"type":"web_search_tool_result","tool_use_id":"srvtoolu_one","content":[]}`
	const blocks = `[{"type":"thinking","thinking":"old","thinkingSignature":"b2xk"},` +
		`{"type":"fallback","from":{"model":"claude-a"},"to":{"model":"claude-b"}},` +
		`{"type":"thinking","thinking":""},` +
		`{"type":"anthropicServerTool","block":` + serverTool + `},{"type":"anthropicServerTool","block":` + toolResult + `},` +
		`{"type":"text","text":"Native reply."}]`
	f := &fixture{last: initial.Leaf}
	f.add(`{"type":"message","message":{"role":"bashExecution","command":"printf plum","output":"plum","timestamp":1,"images":[` +
		`{"type":"image","mimeType":"image/png","data":` + quote(blobPrefix+hash) + `},` +
		`{"type":"image","url":"https://example.invalid/p.png","data":"","mimeType":"image/png"},` +
		`{"type":"image","providerFile":{"provider":"anthropic","id":"file_123"},"data":"","mimeType":"image/png"}]}}`)
	const firstCompaction = `{"type":"anthropicCompaction","provider":"anthropic","content":"Older context.","signature":"b2xk"}`
	const secondCompaction = `{"type":"anthropicCompaction","provider":"anthropic","content":"Prior context.","signature":"c2ln"}`
	assistant := assistantEntry("anthropic", conversation.AnthropicMessages, model, "stop", `[]`)
	f.add(strings.TrimSuffix(assistant, "}}") + `,"providerPayload":` + firstCompaction + `}}`)
	assistant = assistantEntry("anthropic", conversation.AnthropicMessages, model, "stop", blocks)
	f.add(strings.TrimSuffix(assistant, "}}") + `,"providerPayload":` + secondCompaction + `}}`)
	appendRaw(t, path, string(f.bytes()))
	opts := standInEndpoint
	opts.ReplayAnthropicCompaction = true
	opts.BlobResolver = func(hash string) ([]byte, error) { return os.ReadFile(filepath.Join(blobDir, hash)) }
	rebuilt, err := ReadFile(path, opts)
	if err != nil {
		t.Fatal(err)
	}

	// Reduce only the documented wire keys, preserving exact types and order.
	// The local endpoint must receive the URL and ID as references, not fetch them.
	rig.run(t, path, "Follow-up.")
	var images, native []map[string]any
	for _, turn := range endpoint.lastAnthropic(t).Messages {
		var wire []map[string]any
		if json.Unmarshal(turn.Content, &wire) != nil {
			continue
		}
		for _, b := range wire {
			if turn.Role == "user" && b["type"] == "image" {
				source, ok := b["source"].(map[string]any)
				if !ok {
					t.Fatalf("pinned image has no source: %v", b)
				}
				images = append(images, source)
			}
			if turn.Role == "assistant" && b["text"] == "Native reply." {
				native = wire
			}
		}
	}
	wantImages := []map[string]any{
		{"type": "base64", "media_type": "image/png", "data": attachmentFixturePNG},
		{"type": "url", "url": "https://example.invalid/p.png"},
		{"type": "file", "file_id": "file_123"},
	}
	if !reflect.DeepEqual(images, wantImages) {
		t.Fatalf("pinned image sources = %v, want %v", images, wantImages)
	}
	var wantNative []map[string]any
	if err := json.Unmarshal([]byte(`[{"type":"compaction","content":"Older context.","signature":"b2xk"},`+
		`{"type":"compaction","content":"Prior context.","signature":"c2ln"},`+
		`{"type":"thinking","thinking":"old","signature":"b2xk"},`+serverTool+`,`+toolResult+`,{"type":"text","text":"Native reply."}]`), &wantNative); err != nil {
		t.Fatal(err)
	}
	// Cache control is a request marker, not a saved block field.
	for _, b := range native {
		delete(b, "cache_control")
	}
	if !reflect.DeepEqual(native, wantNative) {
		t.Fatalf("pinned assistant blocks = %v, want %v", native, wantNative)
	}
	readImages := rebuilt.Messages[len(rebuilt.Messages)-2].Content
	wantBlocks := []conversation.Block{
		text("Ran `printf plum`\n```\nplum\n```"),
		{Type: conversation.Image, MediaType: "image/png", ImageData: attachmentFixturePNG},
		{Type: conversation.Image, ImageURL: "https://example.invalid/p.png"},
		{Type: conversation.Image, ImageFileID: "file_123"},
	}
	if !reflect.DeepEqual(readImages, wantBlocks) {
		t.Fatalf("reader input = %v, want %v", readImages, wantBlocks)
	}
	last := rebuilt.Messages[len(rebuilt.Messages)-1]
	if last.Native == nil || last.Native.Kind != conversation.AnthropicCompaction {
		t.Fatalf("reader lost assistant compaction: %+v", last)
	}
	if string(last.Native.Payload) != firstCompaction || len(last.Native.Items) != 1 ||
		string(last.Native.Items[0]) != secondCompaction {
		t.Fatalf("reader compaction payloads = %s, %s; want both in order", last.Native.Payload, last.Native.Items)
	}
	wantBlocks = []conversation.Block{
		{Type: conversation.Thinking, Text: "old", Signature: "b2xk"},
		{Type: conversation.AnthropicServerTool, NativeBlock: json.RawMessage(serverTool)},
		{Type: conversation.AnthropicServerTool, NativeBlock: json.RawMessage(toolResult)},
		text("Native reply."),
	}
	if !reflect.DeepEqual(last.Content, wantBlocks) {
		t.Fatalf("reader assistant blocks = %v, want %v", last.Content, wantBlocks)
	}
}

func TestThePinnedOhMyPiOmitsUnreadableInlineImages(t *testing.T) {
	endpoint := newAnthropicStandIn(t)
	rig := newOMPRigForModel(t, conversation.AnthropicMessages, "claude-opus-5-5", endpoint)
	setStandInModalities(t, rig, false)
	rig.run(t, "", "Initial request.")
	path := rig.sessionFile(t)
	initial, err := ReadFile(path, standInEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{last: initial.Leaf}
	f.add(`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"Look."},` +
		`{"type":"image","mimeType":"image/png","data":"blob:sha256:` + strings.Repeat("a", 64) + `"},` +
		`{"type":"image","mimeType":"image/png","data":"cGx1bQ=="},` +
		`{"type":"image","mimeType":"image/jpeg","data":` + quote(jpegWithThumbnail(t, true)) + `}],"timestamp":1}}`)
	appendRaw(t, path, string(f.bytes()))
	before, err := ReadFile(path, standInEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	const followUp = "Follow-up."
	rig.run(t, path, followUp)
	content := sessionContent(t, path)
	got := endpoint.lastAnthropic(t).turns(t, content)
	resumed := resumedSession(t, before, content, standInEndpoint, followUp)
	want := append(readerTurns(t, resumed.Messages), wireTurn{Role: "user", Blocks: []wireBlock{{Type: "text", Text: followUp}}})
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pinned request = %+v\nreader and follow-up = %+v", got, want)
	}
}

func TestThePinnedOhMyPiOmitsUnreadableResponsesNativeImages(t *testing.T) {
	const provider, model = "openai", "gpt-test"
	endpoint := newOpenAIStandIn(t)
	rig := newOMPRigForProviderModel(t, conversation.OpenAIResponses, provider, model, endpoint)
	setStandInModalities(t, rig, false)
	rig.run(t, "", "Initial request.")
	path := rig.sessionFile(t)
	initial, err := ReadFile(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	const badPart = `{"type":"input_image","image_url":"data:image/png;base64,@@","detail":"low"}`
	const badEscape = `{"type":"input_image","image_url":"data:image/png,%GG"}`
	const valid = `{"type":"input_image","image_url":"data:image/png;base64,` + attachmentFixturePNG + `","detail":"auto"}`
	large := `{"type":"input_image","image_url":"data:image/png;base64,` + largeFixturePNG(t) + `","detail":"auto"}`
	f := &fixture{last: initial.Leaf}
	f.add(`{"type":"message","message":{"role":"user","content":"Fallback only.","timestamp":1,"providerPayload":` +
		`{"type":"openaiResponsesHistory","provider":"openai","items":[` + badPart + `,` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"Look."},` + badEscape + `,` + valid + `,` + large + `]}]}}}`)
	appendRaw(t, path, string(f.bytes()))
	before, err := ReadFile(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	native := before.Messages[len(before.Messages)-1].Native
	if native == nil {
		t.Fatal("reader lost the native user payload")
	}
	var replay []map[string]any
	raw, err := json.Marshal(native.Items)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &replay); err != nil {
		t.Fatal(err)
	}
	rig.run(t, path, "Follow-up.")
	input := requestInput(t, endpoint.last(t), sessionContent(t, path))
	wantJSON := `[{"type":"input_text","text":"[image omitted: undecodable image/png data (invalid base64 image data)]"},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"Look."},` +
		`{"type":"input_text","text":"[image omitted: undecodable application/octet-stream data (empty image data)]"},` + valid + `,` + large + `]}]`
	var wanted []map[string]any
	if err := json.Unmarshal([]byte(wantJSON), &wanted); err != nil {
		t.Fatal(err)
	}
	for _, item := range wanted {
		equal := func(got map[string]any) bool { return reflect.DeepEqual(got, item) }
		itemCarriedOnce(t, input, equal, "pinned replay with unreadable image replaced")
		itemCarriedOnce(t, replay, equal, "reader replay with unreadable image replaced")
	}
}

func TestThePinnedOhMyPiFallsBackFromUnreadableComputerScreenshot(t *testing.T) {
	const provider, model = "openai", "gpt-test"
	const note = "Screenshot attempted."
	endpoint := newOpenAIStandIn(t)
	rig := newOMPRigForProviderModel(t, conversation.OpenAIResponses, provider, model, endpoint)
	setStandInModalities(t, rig, false)
	rig.run(t, "", "Initial request.")
	path := rig.sessionFile(t)
	initial, err := ReadFile(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{last: initial.Leaf}
	f.add(withMessageFields(assistantEntry(provider, conversation.OpenAIResponses, model, "toolUse",
		`[{"type":"toolCall","id":"call_screen|cu_screen","name":"computer","arguments":{}}]`),
		`,"providerPayload":{"type":"openaiResponsesHistory","provider":"openai","dt":true,"items":[{"type":"computer_call","id":"cu_screen","call_id":"call_screen","actions":[{"type":"screenshot"}],"status":"completed","pending_safety_checks":[]}]}`))
	f.add(withMessageFields(toolResultEntry("call_screen|cu_screen", "computer", note, false),
		`,"providerMetadata":{"type":"computer","screenshot":{"type":"computer_screenshot","image_url":"data:image/png,%GG"},"acknowledgedSafetyChecks":[]}`))
	appendRaw(t, path, string(f.bytes()))
	before, err := ReadFile(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	last := before.Messages[len(before.Messages)-1]
	if last.Role != conversation.Tool || len(last.Content) != 1 || last.Content[0].ProviderMetadata != nil ||
		!reflect.DeepEqual(last.Content[0].Content, []conversation.Block{text(note)}) {
		t.Fatalf("reader result = %+v, want tool result without screenshot metadata", last)
	}
	rig.run(t, path, "Follow-up.")
	input := requestInput(t, endpoint.last(t), sessionContent(t, path))
	itemCarriedOnce(t, input, func(item map[string]any) bool {
		content, _ := item["content"].(string)
		return item["type"] == "message" && item["role"] == "assistant" &&
			strings.HasPrefix(content, "[Previous computer call; call_id=call_screen]: ")
	}, "pinned computer call note on a model without native computer use")
	itemCarriedOnce(t, input, func(item map[string]any) bool {
		return item["type"] == "message" && item["role"] == "assistant" &&
			item["content"] == "[Orphan tool result; call_id=call_screen]: "+note
	}, "pinned computer-result fallback")
}

func TestThePinnedOhMyPiPrefersSignedAnthropicCompaction(t *testing.T) {
	const model = "claude-opus-5-5"
	const followUp = "Follow-up."
	const encrypted = `{"provider":"anthropic","content":"Legacy context.","encryptedContent":"ciphertext"}`
	const signed = `{"type":"anthropicCompaction","provider":"anthropic","content":"Signed context.","signature":"c2ln"}`
	for _, tc := range []string{"assistant", "summary"} {
		t.Run(tc, func(t *testing.T) {
			endpoint := newAnthropicStandIn(t)
			rig := newOMPRigForModel(t, conversation.AnthropicMessages, model, endpoint)
			appendRaw(t, filepath.Join(rig.home, ".omp", "agent", "models.yml"), "        compat:\n          bedrockMessagesApi: true\n          supportsServerCompaction: true\n")
			rig.run(t, "", "Initial request.")
			path := rig.sessionFile(t)
			initial, err := ReadFile(path, standInEndpoint)
			if err != nil {
				t.Fatal(err)
			}
			f := &fixture{last: initial.Leaf}
			assistant := func(content, payload string) string {
				return withMessageFields(assistantEntry("anthropic", conversation.AnthropicMessages, model, "stop", content), `,"providerPayload":`+payload)
			}
			if tc == "summary" {
				f.add(`{"type":"compaction","summary":"Readable legacy summary.","firstKeptEntryId":"","tokensBefore":1000,"preserveData":{"anthropicCompaction":` + encrypted + `}}`)
			} else {
				f.add(assistant(`[{"type":"text","text":"First reply."}]`, `{"type":"anthropicCompaction",`+encrypted[1:]))
				f.add(userEntry("Next request."))
			}
			f.add(assistant(`[{"type":"text","text":"Signed reply."}]`, signed))
			appendRaw(t, path, string(f.bytes()))
			opts := standInEndpoint
			opts.ReplayAnthropicCompaction = true
			before, err := ReadFile(path, opts)
			if err != nil {
				t.Fatal(err)
			}
			rig.run(t, path, followUp)
			content := sessionContent(t, path)
			got := endpoint.lastAnthropic(t).turns(t, content)
			resumed := resumedSession(t, before, content, opts, followUp)
			want := append(readerTurns(t, resumed.Messages), wireTurn{Role: "user", Blocks: []wireBlock{{Type: "text", Text: followUp}}})
			if !reflect.DeepEqual(got, want) {
				t.Errorf("pinned request = %+v\nreader and follow-up = %+v", got, want)
			}
			var compactions []wireBlock
			for _, turn := range got {
				for _, b := range turn.Blocks {
					if b.Type == "compaction" {
						compactions = append(compactions, b)
					}
				}
			}
			if !reflect.DeepEqual(compactions, []wireBlock{{Type: "compaction", Text: "Signed context.", Signature: "c2ln"}}) {
				t.Fatalf("pinned compactions = %+v, want only the signed block", compactions)
			}
		})
	}
}

func TestThePinnedOhMyPiRestoresResponsesImageBlobs(t *testing.T) {
	const provider, model = "openai", "gpt-test"
	const url = "data:image/png;base64," + attachmentFixturePNG
	for _, tc := range []string{"generation", "replacement history"} {
		t.Run(tc, func(t *testing.T) {
			endpoint := newOpenAIStandIn(t)
			rig := newOMPRigForProviderModel(t, conversation.OpenAIResponses, provider, model, endpoint)
			setStandInModalities(t, rig, false)
			rig.run(t, "", "Initial request.")
			path := rig.sessionFile(t)
			initial, err := ReadFile(path, Options{})
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(rig.home, ".omp", "agent", "blobs")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			body, err := base64.StdEncoding.DecodeString(attachmentFixturePNG)
			if err != nil {
				t.Fatal(err)
			}
			if tc == "replacement history" {
				body = []byte(url)
			}
			hash := ompsessions.Summarize(body).SHA256
			if err := os.WriteFile(filepath.Join(dir, hash), body, 0o600); err != nil {
				t.Fatal(err)
			}
			ref := quote(blobPrefix + hash)
			f := &fixture{last: initial.Leaf}
			var wantedItem map[string]any
			if tc == "generation" {
				f.add(withMessageFields(assistantEntry(provider, conversation.OpenAIResponses, model, "stop", `[{"type":"text","text":"Generated an image."}]`),
					`,"providerPayload":{"type":"openaiResponsesHistory","provider":"openai","dt":true,"items":[{"type":"image_generation_call","id":"ig_fixture","result":`+ref+`,"status":"completed"}]}`))
				wantedItem = map[string]any{"type": "image_generation_call", "id": "ig_fixture", "result": attachmentFixturePNG, "status": "completed"}
			} else {
				f.add(`{"type":"compaction","summary":"Compacted.","firstKeptEntryId":"","tokensBefore":1000,"preserveData":{"openaiRemoteCompaction":{"provider":"openai","replacementHistory":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":` + ref + `,"detail":"auto"}]}]}}}`)
				wantedItem = map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": url, "detail": "auto"}}}
			}
			appendRaw(t, path, string(f.bytes()))
			before, err := ReadFile(path, Options{BlobResolver: func(key string) ([]byte, error) {
				return os.ReadFile(filepath.Join(dir, key))
			}})
			if err != nil {
				t.Fatal(err)
			}
			var rebuilt []map[string]any
			for _, m := range before.Messages {
				if m.Native != nil {
					for _, raw := range m.Native.Items {
						var item map[string]any
						if err := json.Unmarshal(raw, &item); err != nil {
							t.Fatal(err)
						}
						rebuilt = append(rebuilt, item)
					}
				}
			}
			rig.run(t, path, "Follow-up.")
			input := requestInput(t, endpoint.last(t), sessionContent(t, path))
			equal := func(item map[string]any) bool { return reflect.DeepEqual(item, wantedItem) }
			itemCarriedOnce(t, input, equal, "pinned image item with restored data")
			itemCarriedOnce(t, rebuilt, equal, "reader image item with restored data")
		})
	}
}
