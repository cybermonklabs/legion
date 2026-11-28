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
		`{"type":"thinking","thinking":"new","thinkingSignature":"bmV3"},` +
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
		`{"type":"thinking","thinking":"new","signature":"bmV3"},`+serverTool+`,`+toolResult+`,{"type":"text","text":"Native reply."}]`), &wantNative); err != nil {
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
		{Type: conversation.Thinking, Text: "new", Signature: "bmV3"},
		{Type: conversation.AnthropicServerTool, NativeBlock: json.RawMessage(serverTool)},
		{Type: conversation.AnthropicServerTool, NativeBlock: json.RawMessage(toolResult)},
		text("Native reply."),
	}
	if !reflect.DeepEqual(last.Content, wantBlocks) {
		t.Fatalf("reader assistant blocks = %v, want %v", last.Content, wantBlocks)
	}
}
