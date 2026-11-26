package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// versionBlocksAnswer is what a version read with ?blocks=true answers, as a reader of the plan
// at a version reads it: the version's markdown and the blocks whose byte ranges index it.
type versionBlocksAnswer struct {
	Markdown string                 `json:"markdown"`
	Number   int                    `json:"number"`
	SHA256   *string                `json:"sha256"`
	Size     *int                   `json:"size"`
	Blocks   *[]model.ArtifactBlock `json:"blocks"`
}

// A version read with ?blocks=true carries the blocks of that version's own text, not the live
// document's: each typed block under the id its opening line writes, with its attributes and the
// byte range of its rendering in the markdown the same answer carries. Two versions of a plan
// task therefore compare by the id of its block, the comparison a plan's later version rests on.
func TestAVersionReadCarriesThatVersionsBlocks(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createArtifactIssue(t, handler)

	plan := createIssueDocument(t, handler, issue.Key, "plan.md", ":::plan_task{#a steps=\"implementer reviewer\"}\nDo the change.\n:::\n")
	editToVersion(t, handler, plan.ID, 2, map[string]any{
		"op": "replace", "find": "Do the change.", "with": ":::plan_task{#a steps=\"planner implementer\"}\nDo it again.\n:::",
	})
	v1 := readVersionBlocks(t, handler, "/api/v1/artifacts/"+plan.ID+"/versions/1?blocks=true")
	v2 := readVersionBlocks(t, handler, "/api/v1/artifacts/"+plan.ID+"/versions/2?blocks=true")
	assertPlanTask(t, v1, "a", "implementer reviewer", "Do the change.")
	assertPlanTask(t, v2, "a", "planner implementer", "Do it again.")
	bySlug := readVersionBlocks(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts/plan-md/versions/1?blocks=true")
	assertPlanTask(t, bySlug, "a", "implementer reviewer", "Do the change.")
	// Only the typed block is listed: the paragraph inside it writes no id into the stored text, so
	// a read of the version would give it a new id each time.
	for _, answer := range []versionBlocksAnswer{v1, v2} {
		if len(*answer.Blocks) != 1 {
			t.Fatalf("version %d lists %+v, want its one plan task alone", answer.Number, *answer.Blocks)
		}
	}

	// A plan task written with no id gets one when it is first stored, and keeps it in each later
	// version: the stored markdown names it, so every read of every version finds the same one.
	unnamed := createIssueDocument(t, handler, issue.Key, "unnamed.md", ":::plan_task{steps=\"implementer\"}\nX\n:::\n")
	first := readVersionBlocks(t, handler, "/api/v1/artifacts/"+unnamed.ID+"/versions/1?blocks=true")
	minted := onlyPlanTask(t, first).ID
	if minted == "" {
		t.Fatalf("version 1 of a plan task written with no id lists it with none: %+v", first)
	}
	if again := onlyPlanTask(t, readVersionBlocks(t, handler, "/api/v1/artifacts/"+unnamed.ID+"/versions/1?blocks=true")).ID; again != minted {
		t.Fatalf("version 1 read twice names its plan task %q, then %q", minted, again)
	}
	editToVersion(t, handler, unnamed.ID, 2, map[string]any{
		"op": "retype", "block": minted, "type": "plan_task", "attributes": map[string]any{"steps": "implementer reviewer"},
	})
	second := readVersionBlocks(t, handler, "/api/v1/artifacts/"+unnamed.ID+"/versions/2?blocks=true")
	if got := onlyPlanTask(t, second).ID; got != minted {
		t.Fatalf("a task block changed its id between versions: %q in version 1, %q in version 2\nversion 2's markdown: %q", minted, got, second.Markdown)
	}
	assertPlanTask(t, first, minted, "implementer", "X")
	assertPlanTask(t, second, minted, "implementer reviewer", "X")

	// Control: a read that asks for no blocks has no "blocks" key, whatever else it sends, and the
	// live text's blocks are version 2's, so the version reads above did not read the live tree.
	for _, query := range []string{"", "?blocks=false", "?blocks=1", "?blocks="} {
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+plan.ID+"/versions/1"+query, nil, "alice")
		if response.Code != http.StatusOK || strings.Contains(response.Body.String(), `"blocks"`) {
			t.Fatalf("GET version 1%s: status=%d body=%s, want the version with no blocks key", query, response.Code, response.Body.String())
		}
	}
	live := decodeBody[versionBlocksAnswer](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+plan.ID+"/text?blocks=true", nil, "alice"))
	assertPlanTask(t, live, "a", "planner implementer", "Do it again.")
}

// The blocks of a version index the markdown its answer carries. A version an earlier renderer
// stored can render otherwise today; its answer then carries the rendering the blocks were read
// from, and the digest and size of that text, so a reader slicing the markdown at a block's range
// reads that block. Only a version written before the cutover holds such a rendering: every
// version this build writes, an upload's among them, holds a version_renders row with a time and
// renders from the tree (R15: "A version of a document that no query record covers ... The row
// holds a time, so the version renders from the tree"), and "A version with no row was written
// before the cutover", which answers its stored bytes. So the fixture makes the upload's version 1
// one written before the cutover: it stores the rendering an earlier renderer wrote and takes away
// the version_renders row the upload wrote, as c-render-at-version deletes a row and reads the
// stored markdown answered.
func TestAVersionsBlocksIndexTheMarkdownItsAnswerCarries(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	issue := createArtifactIssue(t, handler)
	plan := createIssueDocument(t, handler, issue.Key, "notes.md", "Intro.\n\n:::plan_task{#a steps=\"implementer\"}\nBody.\n:::\n")
	// The stored text as a renderer that kept the blank lines between blocks would have written it.
	stored := "Intro.\n\n\n\n:::plan_task{#a steps=\"implementer\"}\nBody.\n:::\n"
	if _, err := database.Pool.Exec(context.Background(), `update artifact_versions set markdown = $2 where artifact_id = $1 and number = 1`, plan.ID, stored); err != nil {
		t.Fatalf("store version 1's older rendering: %v", err)
	}
	if taken, err := database.Pool.Exec(context.Background(), `delete from version_renders where artifact_id = $1 and number = 1`, plan.ID); err != nil || taken.RowsAffected() != 1 {
		t.Fatalf("take away the version_renders row the upload wrote of version 1: %d rows (%v), want the one row", taken.RowsAffected(), err)
	}
	plain := decodeBody[versionBlocksAnswer](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+plan.ID+"/versions/1", nil, "alice"))
	if plain.Markdown != stored {
		t.Fatalf("version 1 without blocks = %q, want the stored %q", plain.Markdown, stored)
	}
	answer := readVersionBlocks(t, handler, "/api/v1/artifacts/"+plan.ID+"/versions/1?blocks=true")
	if answer.Markdown == stored {
		t.Fatalf("version 1 with blocks answers the stored %q, which no block indexes", stored)
	}
	assertPlanTask(t, answer, "a", "implementer", "Body.")
	digest := sha256.Sum256([]byte(answer.Markdown))
	if answer.SHA256 == nil || *answer.SHA256 != hex.EncodeToString(digest[:]) || answer.Size == nil || *answer.Size != len(answer.Markdown) {
		t.Fatalf("version 1 with blocks: sha256=%v size=%v, want those of the markdown it carries (%q)", answer.SHA256, answer.Size, answer.Markdown)
	}
}

// createIssueDocument uploads a markdown document onto an issue as version 1.
func createIssueDocument(t *testing.T, handler http.Handler, issueKey, name, content string) model.Artifact {
	t.Helper()
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/artifacts", map[string]string{
		"name": name, "content": content,
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create %s: status=%d body=%s", name, created.Code, created.Body.String())
	}
	return decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, created).Artifact
}

// editToVersion applies one edit operation to a document and requires that it wrote version.
func editToVersion(t *testing.T, handler http.Handler, artifactID string, version int, op map[string]any) {
	t.Helper()
	if edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", map[string]any{
		"ops": []map[string]any{op},
	}, "alice"); edited.Code != http.StatusOK {
		t.Fatalf("edit %+v: status=%d body=%s", op, edited.Code, edited.Body.String())
	}
	written := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/versions/"+strconv.Itoa(version), nil, "alice")
	if written.Code != http.StatusOK {
		t.Fatalf("the edit %+v wrote no version %d: status=%d body=%s", op, version, written.Code, written.Body.String())
	}
}

// readVersionBlocks reads a version with its blocks and requires the key.
func readVersionBlocks(t *testing.T, handler http.Handler, target string) versionBlocksAnswer {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s: status=%d body=%s", target, response.Code, response.Body.String())
	}
	answer := decodeBody[versionBlocksAnswer](t, response)
	if answer.Blocks == nil {
		t.Fatalf("GET %s answers no blocks key: %s", target, response.Body.String())
	}
	return answer
}

// onlyPlanTask is the one plan_task block an answer lists.
func onlyPlanTask(t *testing.T, answer versionBlocksAnswer) model.ArtifactBlock {
	t.Helper()
	var found []model.ArtifactBlock
	for _, block := range *answer.Blocks {
		if block.Type == "plan_task" {
			found = append(found, block)
		}
	}
	if len(found) != 1 {
		t.Fatalf("blocks %+v hold %d plan tasks, want one", *answer.Blocks, len(found))
	}
	return found[0]
}

// assertPlanTask requires the answer to list the plan_task block id with that steps attribute,
// its range to be its rendering in the answer's markdown (the opening line, the body, the closing
// fence), and its body to be body.
func assertPlanTask(t *testing.T, answer versionBlocksAnswer, id, steps, body string) {
	t.Helper()
	for _, block := range *answer.Blocks {
		if block.ID != id {
			continue
		}
		if block.Type != "plan_task" {
			t.Fatalf("block %s is a %s, want a plan_task", id, block.Type)
		}
		attributes := map[string]string{}
		for _, attribute := range block.Attributes {
			attributes[attribute.Name] = attribute.Value
		}
		if attributes["steps"] != steps {
			t.Fatalf("block %s carries attributes %+v, want steps %q", id, block.Attributes, steps)
		}
		if block.From < 0 || block.To > len(answer.Markdown) || block.To < block.From {
			t.Fatalf("block %+v does not index the markdown it came with (%q)", block, answer.Markdown)
		}
		rendering := answer.Markdown[block.From:block.To]
		opening, rest, cut := strings.Cut(rendering, "\n")
		closing := strings.LastIndex(rest, "\n")
		if !cut || !strings.HasPrefix(opening, ":::plan_task{") || closing < 0 || rest[closing+1:] != ":::" {
			t.Fatalf("block %s spans %q, want its opening line, its body and its closing fence", id, rendering)
		}
		if got := rest[:closing]; got != body {
			t.Fatalf("block %s's body = %q, want %q", id, got, body)
		}
		return
	}
	t.Fatalf("no block %s among %+v (markdown %q)", id, *answer.Blocks, answer.Markdown)
}
