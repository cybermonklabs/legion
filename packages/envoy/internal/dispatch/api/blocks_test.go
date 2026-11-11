package api

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func TestArtifactBlocksReferenceCanonicalMarkdownByteRanges(t *testing.T) {
	handler := newTestHandler(t)
	issue := createArtifactIssue(t, handler)
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
		"name": "blocks.md", "content": "# Heading\n\nSecond paragraph\n",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create document: status=%d body=%s", created.Code, created.Body.String())
	}
	artifact := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, created).Artifact
	text := decodeBody[struct {
		Markdown string `json:"markdown"`
		Token    string `json:"token"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifact.ID+"/text", nil, "alice"))
	blocksResponse := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifact.ID+"/blocks", nil, "alice")
	if blocksResponse.Code != http.StatusOK {
		t.Fatalf("get blocks: status=%d body=%s", blocksResponse.Code, blocksResponse.Body.String())
	}
	blocks := decodeBody[[]model.ArtifactBlock](t, blocksResponse)
	if len(blocks) != 2 {
		t.Fatalf("blocks = %#v, want heading and paragraph", blocks)
	}
	seen := map[string]bool{}
	for _, block := range blocks {
		if block.ID == "" || block.Token == "" || seen[block.ID] || block.From < 0 || block.To < block.From || block.To > len(text.Markdown) {
			t.Fatalf("invalid block %#v for markdown %q", block, text.Markdown)
		}
		seen[block.ID] = true
	}
	if text.Token == "" {
		t.Fatal("document text omitted its optimistic-concurrency token")
	}
	if got := text.Markdown[blocks[0].From:blocks[0].To]; got != "# Heading" || blocks[0].Type != "heading" {
		t.Fatalf("heading block = %q (%s), want # Heading", got, blocks[0].Type)
	}
	if got := text.Markdown[blocks[1].From:blocks[1].To]; got != "Second paragraph" || blocks[1].Type != "paragraph" {
		t.Fatalf("paragraph block = %q (%s), want Second paragraph", got, blocks[1].Type)
	}
	bySlug := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/artifacts/blocks-md/blocks", nil, "alice")
	if bySlug.Code != http.StatusOK {
		t.Fatalf("get blocks by issue slug: status=%d body=%s", bySlug.Code, bySlug.Body.String())
	}
}

// Every blocks route says where meaning search stands on each block it lists: pending while a
// window covering the block waits for its vector, embedded once every one of them holds a vector
// of the text the window has now, and none for a block no window covers - what every block of an
// item reads while its windows are not written, as an item a build before them wrote has none
// until the block-index tick reaches it.
func TestTheBlocksRoutesSayWhereMeaningSearchStands(t *testing.T) {
	embedder := &fakeEmbedder{vectors: map[string][]float32{}}
	handler, database, _ := newTestServer(t, testServerOptions{embedder: embedder})
	issue := createInteractionIssue(t, handler, "EMBD", "Block embedding", "# Heading\n\nSecond paragraph\n")
	comment := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Alpha.\n\nBeta."}, "alice")
	message := createIssueMessage(t, handler, issue.Key, map[string]any{"body": "One.\n\nTwo."}, "alice")
	documentBlocks := func(step string) []model.ArtifactBlock {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("%s: get the document's blocks: status=%d body=%s", step, response.Code, response.Body.String())
		}
		return decodeBody[[]model.ArtifactBlock](t, response)
	}
	itemBlocks := func(step, path string) []model.ItemBlock {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, path, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("%s: GET %s: status=%d body=%s", step, path, response.Code, response.Body.String())
		}
		return decodeBody[model.ItemBlocks](t, response).Blocks
	}
	requireEvery := func(step, what string, states []string, want string) {
		t.Helper()
		if len(states) < 2 {
			t.Fatalf("%s: the %s lists %d blocks, want at least two", step, what, len(states))
		}
		for index, state := range states {
			if state != want {
				t.Fatalf("%s: the %s's block %d reads embedding %q, want %q", step, what, index, state, want)
			}
		}
	}
	requireAll := func(step, want string) {
		t.Helper()
		document := make([]string, 0, 2)
		for _, block := range documentBlocks(step) {
			document = append(document, block.Embedding)
		}
		requireEvery(step, "document", document, want)
		for what, path := range map[string]string{
			"comment": "/api/v1/comments/" + comment.ID + "/blocks",
			"message": "/api/v1/messages/" + message.ID + "/blocks",
		} {
			states := make([]string, 0, 2)
			for _, block := range itemBlocks(step, path) {
				states = append(states, block.Embedding)
			}
			requireEvery(step, what, states, want)
		}
	}

	requireAll("before the queue runs", model.BlockEmbeddingPending)
	processAllPending(t, database, embedder)
	requireAll("once the queue drained", model.BlockEmbedded)

	// An edit leaves the comment's own window waiting again, where the message's, untouched,
	// stays current.
	requireEdit(t, "reword", editCommentAs(t, handler, comment.ID, "Alpha reworded.\n\nBeta.", nil, nil))
	for index, block := range itemBlocks("after an edit", "/api/v1/comments/"+comment.ID+"/blocks") {
		if block.Embedding != model.BlockEmbeddingPending {
			t.Fatalf("after an edit: the comment's block %d reads embedding %q, want %q", index, block.Embedding, model.BlockEmbeddingPending)
		}
	}

	// An item whose windows are not written - what a build before them left behind - reads none
	// for every block, and no read writes one.
	if _, err := database.Pool.Exec(context.Background(),
		`delete from embeddings where kind = 'message_block' and split_part(id, '#', 1) = 'message/' || $1`, message.ID); err != nil {
		t.Fatalf("drop the message's windows: %v", err)
	}
	for index, block := range itemBlocks("with no window", "/api/v1/messages/"+message.ID+"/blocks") {
		if block.Embedding != model.BlockNotEmbedded {
			t.Fatalf("with no window: the message's block %d reads embedding %q, want %q", index, block.Embedding, model.BlockNotEmbedded)
		}
	}
}

// A listing is one answer of the server. The three text routes carry the document's blocks when
// the request asks for them with ?blocks=true, beside the markdown, the token and the version they
// already carry, and every part of that answer is rendered from the one tree: a write by another
// session cannot leave a listing's byte ranges indexing markdown they were not computed over.
// Each block is what GET /api/v1/artifacts/{id}/blocks reports, references and embedding included,
// and that route still answers the bare array a client older than the server reads.
func TestTheTextRoutesCarryTheBlocksTheirMarkdownHolds(t *testing.T) {
	handler := newTestHandler(t)
	issue := createArtifactIssue(t, handler)
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
		"name": "listing.md", "content": "# Heading\n\nSecond paragraph\n",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create document: status=%d body=%s", created.Code, created.Body.String())
	}
	artifact := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, created).Artifact
	// A comment anchored to the heading: the text route's blocks must count it as the blocks
	// route does, so the two answers are the same block rows and not two renderings.
	onBlocks := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifact.ID+"/blocks", nil, "alice")
	if onBlocks.Code != http.StatusOK {
		t.Fatalf("get blocks: status=%d body=%s", onBlocks.Code, onBlocks.Body.String())
	}
	listed := decodeBody[[]model.ArtifactBlock](t, onBlocks)
	if len(listed) != 2 {
		t.Fatalf("the blocks route lists %#v, want a heading and a paragraph", listed)
	}
	if body := strings.TrimSpace(onBlocks.Body.String()); !strings.HasPrefix(body, "[") {
		t.Fatalf("the blocks route answers %s, want the bare array every older client reads", body)
	}
	comment := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body":   "Which heading?",
		"anchor": map[string]string{"artifact": "listing-md", "quote": "# Heading"},
	}, "alice")
	if comment.Code != http.StatusCreated {
		t.Fatalf("comment on the heading: status=%d body=%s", comment.Code, comment.Body.String())
	}
	withComment := decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, handler, http.MethodGet,
		"/api/v1/artifacts/"+artifact.ID+"/blocks", nil, "alice"))

	for _, route := range []struct{ name, path string }{
		{"by id", "/api/v1/artifacts/" + artifact.ID + "/text"},
		{"by issue slug", "/api/v1/issues/" + issue.Key + "/artifacts/listing-md/text"},
		{"by project slug", "/api/v1/projects/TEST/artifacts/" + createProjectDocument(t, handler, "TEST", "listing.md", "# Heading\n\nSecond paragraph\n").Slug + "/text"},
	} {
		t.Run(route.name, func(t *testing.T) {
			plain := dispatchRequest(t, handler, http.MethodGet, route.path, nil, "alice")
			if plain.Code != http.StatusOK {
				t.Fatalf("read the text: status=%d body=%s", plain.Code, plain.Body.String())
			}
			if strings.Contains(plain.Body.String(), `"blocks"`) {
				t.Fatalf("a request that asked for no blocks got %s", plain.Body.String())
			}
			response := dispatchRequest(t, handler, http.MethodGet, route.path+"?blocks=true", nil, "alice")
			if response.Code != http.StatusOK {
				t.Fatalf("read the text with its blocks: status=%d body=%s", response.Code, response.Body.String())
			}
			answer := decodeBody[struct {
				Markdown string                `json:"markdown"`
				Token    string                `json:"token"`
				Version  *int                  `json:"version"`
				Blocks   []model.ArtifactBlock `json:"blocks"`
			}](t, response)
			if answer.Token == "" || answer.Version == nil || *answer.Version != 1 {
				t.Fatalf("answer token=%q version=%v, want a token and version 1", answer.Token, answer.Version)
			}
			if len(answer.Blocks) != 2 {
				t.Fatalf("the text route lists %#v, want a heading and a paragraph", answer.Blocks)
			}
			for _, want := range []struct {
				index int
				text  string
				kind  string
			}{{0, "# Heading", "heading"}, {1, "Second paragraph", "paragraph"}} {
				block := answer.Blocks[want.index]
				if block.From < 0 || block.To > len(answer.Markdown) || block.To < block.From {
					t.Fatalf("block %#v does not index the markdown it came with (%q)", block, answer.Markdown)
				}
				if got := answer.Markdown[block.From:block.To]; got != want.text || block.Type != want.kind {
					t.Fatalf("block %d = %q (%s), want %q (%s)", want.index, got, block.Type, want.text, want.kind)
				}
			}
		})
	}

	// The document the comment is on: the text route's blocks carry the same counts and the same
	// embedding states as the blocks route's, since both answers describe the blocks the same way.
	withText := decodeBody[struct {
		Blocks []model.ArtifactBlock `json:"blocks"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifact.ID+"/text?blocks=true", nil, "alice")).Blocks
	if !reflect.DeepEqual(withText, withComment) {
		t.Fatalf("the text route's blocks are\n%#v\nand the blocks route's are\n%#v", withText, withComment)
	}
	if withComment[0].References.Comments != 1 {
		t.Fatalf("the heading block carries %#v, want one comment", withComment[0].References)
	}

	// A value that is not `true` asks for no blocks, as every other boolean parameter reads.
	for _, query := range []string{"?blocks=false", "?blocks=1", "?blocks="} {
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifact.ID+"/text"+query, nil, "alice")
		if response.Code != http.StatusOK || strings.Contains(response.Body.String(), `"blocks"`) {
			t.Fatalf("GET /text%s: status=%d body=%s, want the text alone", query, response.Code, response.Body.String())
		}
	}
}
