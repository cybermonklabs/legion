package omp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/legion/daemon/internal/conversation"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
)

func TestReadForeignGeminiNativeContent(t *testing.T) {
	for _, targetAPI := range []string{conversation.AnthropicMessages, conversation.OpenAIResponses} {
		t.Run(targetAPI, func(t *testing.T) {
			f := newFixture()
			foreign := assistantEntry("factory-droid", "google-generative-ai", "gemini-3", "stop", `[{"type":"text","text":"Earlier text"}]`)
			f.add(strings.TrimSuffix(foreign, "}}") + `,"providerPayload":{"type":"geminiNativeContent","digest":"saved","content":{"role":"model","parts":[{"text":"opaque"}]}}}}`)
			f.add(userEntry("next"))
			provider, model := "anthropic", "claude-test"
			if targetAPI == conversation.OpenAIResponses {
				provider, model = "openai", "gpt-test"
			}
			f.add(assistantEntry(provider, targetAPI, model, "stop", `[{"type":"text","text":"answer"}]`))
			s := read(t, f.bytes())
			assertMessages(t, s.Messages[:1], []conversation.Message{{Role: conversation.Assistant, Provider: "factory-droid", API: "google-generative-ai", Model: "gemini-3", Content: []conversation.Block{text("Earlier text")}}})
		})
	}
}

func TestReadExecutionMessages(t *testing.T) {
	for _, tc := range []struct {
		name, message, want string
		image               bool
	}{
		{"bash", `{"role":"bashExecution","command":"printf plum","output":"plum","exitCode":2,"images":[{"type":"image","mimeType":"image/png","data":"cGx1bQ=="}],"meta":{"truncation":{"direction":"tail","truncatedBy":"bytes","totalLines":4,"outputLines":1,"maxBytes":1280,"shownRange":{"start":4,"end":4},"artifactId":"a1"}}}`, "Ran `printf plum`\n```\nplum\n```\n\nCommand exited with code 2\n\n[Showing lines 4-4 of 4 (1.3KB limit). Read artifact://a1 for full output]", true},
		{"bash cancelled", `{"role":"bashExecution","command":"printf plum","output":"","exitCode":2,"cancelled":true}`, "Ran `printf plum`\n(no output)\n\n(command cancelled)", false},
		{"python", `{"role":"pythonExecution","code":"print('plum')","output":"plum","exitCode":3}`, "Ran Python:\n```python\nprint('plum')\n```\nOutput:\n```\nplum\n```\n\nExecution failed with code 3", false},
		{"python cancelled", `{"role":"pythonExecution","code":"print('plum')","output":"","exitCode":3,"cancelled":true}`, "Ran Python:\n```python\nprint('plum')\n```\n(no output)\n\n(execution cancelled)", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			f.add(claudeEntry("stop", `[{"type":"text","text":"start"}]`))
			f.add(`{"type":"message","message":` + tc.message + `}`)
			f.add(`{"type":"message","message":{"role":"pythonExecution","code":"hidden","excludeFromContext":true}}`)
			want := user(tc.want)
			if tc.image {
				want.Content = append(want.Content, conversation.Block{Type: conversation.Image, MediaType: "image/png", ImageData: "cGx1bQ=="})
			}
			assertMessages(t, read(t, f.bytes()).Messages, []conversation.Message{claude(text("start")), want})
		})
	}
}

func TestReadLegacyHookAndFileMention(t *testing.T) {
	const image = `{"type":"image","mimeType":"image/png","data":"cGx1bQ=="}`
	inline := conversation.Block{Type: conversation.Image, MediaType: "image/png", ImageData: "cGx1bQ=="}
	for _, tc := range []struct {
		name, record string
		want         []conversation.Message
	}{
		{"hook", `{"role":"hookMessage","customType":"collab-prompt","attribution":"user","content":[{"type":"text","text":"a note"},` + image + `]}`, []conversation.Message{
			user("a note"), {Role: conversation.User, Content: []conversation.Block{text("Images attached to collab-prompt."), inline}},
		}},
		{"hook label", `{"role":"hookMessage","customType":"continue-reminder","content":"This is a hook, not a continuation marker."}`, []conversation.Message{
			user("This is a hook, not a continuation marker."),
		}},
		{"files", `{"role":"fileMention","files":[{"path":"plum.png","image":` + image + `},{"path":"plum.txt","content":"plum"}]}`, []conversation.Message{
			user("<file path=\"plum.txt\">\nplum\n</file>"), {Role: conversation.User, Content: []conversation.Block{text("<file path=\"plum.png\">\n</file>"), inline}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			f.add(claudeEntry("stop", `[{"type":"text","text":"start"}]`))
			f.add(`{"type":"message","message":` + tc.record + `}`)
			assertMessages(t, read(t, f.bytes()).Messages, append([]conversation.Message{claude(text("start"))}, tc.want...))
		})
	}
}

func TestReadAnthropicFallbackSegments(t *testing.T) {
	const first = `{"type":"thinking","thinking":"old thought","thinkingSignature":"b2xk"}`
	const second = `{"type":"thinking","thinking":"new thought","thinkingSignature":"bmV3"}`
	const boundary = `{"type":"fallback","from":{"model":"claude-one"},"to":{"model":"claude-two"}}`
	f := newFixture()
	f.add(claudeEntry("stop", `[`+first+`,`+boundary+`,`+second+`,{"type":"text","text":"answer"}]`))
	old := conversation.Block{Type: conversation.Thinking, Text: "old thought", Signature: "b2xk"}
	fresh := conversation.Block{Type: conversation.Thinking, Text: "new thought", Signature: "bmV3"}
	for _, tc := range []struct {
		name string
		opts Options
		want []conversation.Block
	}{
		{"collapse", Options{}, []conversation.Block{fresh, text("answer")}},
		{"nonofficial", Options{AnthropicServerSideFallback: true, NonOfficialAnthropicEndpoint: true}, []conversation.Block{fresh, text("answer")}},
		{"enabled", Options{AnthropicServerSideFallback: true}, []conversation.Block{old, {Type: conversation.AnthropicFallback, NativeBlock: json.RawMessage(boundary)}, fresh, text("answer")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := Read(f.bytes(), tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			assertMessages(t, s.Messages, []conversation.Message{claude(tc.want...)})
		})
	}
}

func TestReadAssistantAnthropicCompaction(t *testing.T) {
	for _, tc := range []struct {
		name, sourceProvider, replayProvider, state string
		enabled, want                               bool
	}{
		{"signed", "anthropic", "anthropic", `,"signature":""`, true, true},
		{"legacy", "anthropic", "anthropic", `,"encryptedContent":"opaque"`, true, true},
		{"off", "anthropic", "anthropic", `,"signature":"signed"`, false, false},
		{"foreign", "anthropic", "other", `,"signature":"signed"`, true, false},
		{"no state", "anthropic", "anthropic", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			assistant := assistantEntry(tc.sourceProvider, conversation.AnthropicMessages, "claude-test", "stop", `[{"type":"text","text":"answer"}]`)
			payload := `{"type":"anthropicCompaction","provider":` + quote(tc.replayProvider) + `,"content":"prior context"` + tc.state + `}`
			f.add(strings.TrimSuffix(assistant, "}}") + `,"providerPayload":` + payload + `}}`)
			s, err := Read(f.bytes(), Options{ReplayAnthropicCompaction: tc.enabled})
			if err != nil {
				t.Fatal(err)
			}
			want := claude(text("answer"))
			want.Model = "claude-test"
			if tc.want {
				want.Native = &conversation.Native{Kind: conversation.AnthropicCompaction, Provider: "anthropic", Payload: json.RawMessage(payload)}
			}
			assertMessages(t, s.Messages, []conversation.Message{want})
		})
	}
}

func TestReadLegacyFileNoteAfterAssistantCompaction(t *testing.T) {
	const summary = `{"provider":"anthropic","content":"Archived.","encryptedContent":"old","filesText":"Retained file."}`
	const payload = `{"type":"anthropicCompaction","provider":"anthropic","content":"More archive.","encryptedContent":"new"}`
	messages := []llmMessage{
		{role: "user", native: &conversation.Native{Kind: conversation.AnthropicCompaction, Provider: "anthropic", Payload: json.RawMessage(summary)}},
		{role: "assistant", provider: "anthropic", api: conversation.AnthropicMessages, model: "claude-test", stopReason: "stop",
			payload: json.RawMessage(payload), blocks: []block{{Type: "text", Text: "First reply."}}},
		{role: "assistant", provider: "anthropic", api: conversation.AnthropicMessages, model: "claude-test", stopReason: "stop",
			blocks: []block{{Type: "text", Text: "Second reply."}}},
	}
	got, err := toRequest(messages, target{provider: "anthropic", api: conversation.AnthropicMessages, model: "claude-test", anthropicReplay: true})
	if err != nil {
		t.Fatal(err)
	}
	first, second := claude(text("First reply.")), claude(text("Second reply."))
	first.Model, second.Model = "claude-test", "claude-test"
	first.Native = &conversation.Native{Kind: conversation.AnthropicCompaction, Provider: "anthropic", Payload: json.RawMessage(summary), Items: []json.RawMessage{json.RawMessage(payload)}}
	assertMessages(t, got, []conversation.Message{first, user("Retained file."), second})
}

func TestReadImageSourcePrecedence(t *testing.T) {
	const inputs = `[{"type":"text","text":"images"},` +
		`{"type":"image","url":"https://example.invalid/p.png","mimeType":"image/bmp","data":"b25l","providerFile":{"provider":"anthropic","id":"file_a"}},` +
		`{"type":"image","url":"https://example.invalid/q.png","mimeType":"image/png","data":"dHdv","providerFile":{"provider":"openai","id":"file_o"}},` +
		`{"type":"image","mimeType":"image/png","data":"dGhyZWU=","providerFile":{"provider":"google","id":"file_g"}}]`
	for _, tc := range []struct {
		provider, api, model string
		want                 []conversation.Block
	}{
		{"anthropic", conversation.AnthropicMessages, "claude-test", []conversation.Block{{Type: conversation.Image, ImageFileID: "file_a"}, {Type: conversation.Image, ImageURL: "https://example.invalid/q.png"}}},
		{"openai", conversation.OpenAIResponses, "gpt-test", []conversation.Block{{Type: conversation.Image, ImageURL: "https://example.invalid/p.png"}, {Type: conversation.Image, ImageFileID: "file_o"}}},
	} {
		t.Run(tc.api, func(t *testing.T) {
			f := newFixture()
			f.add(`{"type":"message","message":{"role":"user","content":` + inputs + `}}`)
			f.add(assistantEntry(tc.provider, tc.api, tc.model, "stop", `[{"type":"text","text":"seen"}]`))
			want := user("images")
			want.Content = append(want.Content, tc.want...)
			want.Content = append(want.Content, conversation.Block{Type: conversation.Image, MediaType: "image/png", ImageData: "dGhyZWU="})
			assertMessages(t, read(t, f.bytes()).Messages[:1], []conversation.Message{want})
		})
	}
}

func TestReadResolvesImageBlob(t *testing.T) {
	const hash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, tc := range []struct {
		name, data, want string
		resolve          bool
	}{
		{"resolved", blobPrefix + hash, "cGx1bQ==", true},
		{"missing", blobPrefix + hash, blobPrefix + hash, true},
		{"no resolver", blobPrefix + hash, blobPrefix + hash, false},
		{"invalid", blobPrefix + "../plum", blobPrefix + "../plum", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			f.add(`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"image"},{"type":"image","mimeType":"image/png","data":` + quote(tc.data) + `}]}}`)
			f.add(claudeEntry("stop", `[{"type":"text","text":"seen"}]`))
			opts := Options{}
			if tc.resolve {
				opts.BlobResolver = func(key string) ([]byte, error) {
					if key != hash {
						t.Fatalf("resolver received %q, want canonical hash", key)
					}
					if tc.name == "missing" {
						return nil, nil
					}
					return []byte("plum"), nil
				}
			}
			s, err := Read(f.bytes(), opts)
			if err != nil {
				t.Fatal(err)
			}
			want := user("image")
			want.Content = append(want.Content, conversation.Block{Type: conversation.Image, MediaType: "image/png", ImageData: tc.want})
			assertMessages(t, s.Messages[:1], []conversation.Message{want})
		})
	}
}

func TestReadSQLResolvesImageBlobs(t *testing.T) {
	ctx := context.Background()
	dsn := testdb.PlainDatabase(t, "legion_omp_blob_test")
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(ctx) })
	conn, err := ompsessions.ConnectURL(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(ctx) })
	if _, err := admin.Exec(ctx, `CREATE TABLE public.omp_session_files_blobs (sha256 text PRIMARY KEY, content bytea NOT NULL, byte_len bigint NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	insert := func(hash string, body []byte, size int) {
		t.Helper()
		if _, err := admin.Exec(ctx, `INSERT INTO public.omp_session_files_blobs (sha256, content, byte_len) VALUES ($1, $2, $3)`, hash, body, size); err != nil {
			t.Fatal(err)
		}
	}
	a, b := bytes.Repeat([]byte{0x61}, 96), bytes.Repeat([]byte{0x62}, 95)
	whole := bytes.Join([][]byte{b, a, b}, nil)
	ka, kb, key := ompsessions.Summarize(a).SHA256, ompsessions.Summarize(b).SHA256, ompsessions.Summarize(whole).SHA256
	insert(ka, a, len(a))
	insert(kb, b, len(b))
	insert(key, []byte(kb+ka+kb), len(whole))
	f := newFixture()
	f.add(fmt.Sprintf(`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"images"},{"type":"image","mimeType":"image/png","data":%q},{"type":"image","mimeType":"image/png","data":%q}]}}`, blobPrefix+ka, blobPrefix+key))
	f.add(claudeEntry("stop", `[{"type":"text","text":"seen"}]`))
	const path = "/fixtures/image.jsonl"
	if _, err := ompsessions.Import(ctx, conn, path, f.bytes(), time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	s, err := ReadSQL(ctx, conn, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	assertMessages(t, s.Messages[:1], []conversation.Message{{Role: conversation.User, Content: []conversation.Block{
		text("images"),
		{Type: conversation.Image, MediaType: "image/png", ImageData: base64.StdEncoding.EncodeToString(a)},
		{Type: conversation.Image, MediaType: "image/png", ImageData: base64.StdEncoding.EncodeToString(whole)},
	}}})
}
