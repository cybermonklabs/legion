package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// keptDocument is what a reader of a document's stored state sees beside its text: every block's
// id in document order, where each anchor's mark stands (the block holding it and the text it
// covers, "" where the text holds no mark of it), and every margin record.
type keptDocument struct {
	blocks  []string
	anchors map[string]string
	margin  map[string]string
}

// readKept reads artifactID's stored state as keptDocument, its anchors those of marks.
func readKept(t *testing.T, database *store.Store, artifactID string, marks map[string]string) keptDocument {
	t.Helper()
	loaded, err := docs.NewPgVersioned(database).Load(context.Background(), artifactID)
	if err != nil {
		t.Fatal(err)
	}
	doc := crdt.New()
	if err := crdt.ApplyUpdateV1(doc, loaded.Update, nil); err != nil {
		t.Fatal(err)
	}
	tree, err := pmdoc.Read(doc.GetXmlFragment("prosemirror"))
	if err != nil {
		t.Fatal(err)
	}
	kept := keptDocument{anchors: map[string]string{}, margin: map[string]string{}}
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if id, ok := node.Attrs[pmdoc.BlockIDAttr].(string); ok {
			kept.blocks = append(kept.blocks, id)
		}
		return true
	})
	for markID, markType := range marks {
		r, quote, found := pmdoc.FindMark(tree, markType, markID)
		if !found {
			kept.anchors[markID] = ""
			continue
		}
		block, err := pmdoc.BlockIDForRange(tree, r)
		if err != nil {
			t.Fatal(err)
		}
		kept.anchors[markID] = block + ": " + quote
	}
	doc.GetMap("marks").ForEach(func(key string, value any) {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		kept.margin[key] = string(encoded)
	})
	return kept
}

// Every rebuild keeps the document's block ids, its open anchors and its margin records: the
// dashboard's (a history near the decode cap, rebuilt from its own text), one from supplied
// markdown over that history, and one of a history that does not load, rebuilt from its latest
// version. The document holds two anchored comments, each with a reply, an anchored ask and a
// decision block (an ask block), and after each rebuild it reads the same block ids - but for the
// paragraph the supplied markdown rewrote, and, of a history that does not load, the list items no
// row records - the same marks in the same blocks, and the same margin records. Before, the
// dashboard's rebuild left the marks map empty, and the two from markdown gave every untyped block
// a fresh id and dropped every anchor mark and the margin with it.
func TestEveryRebuildPathKeepsTheDocumentsBlockIDsAnchorsAndMarginRecords(t *testing.T) {
	var broken *invalidLoadOnceStore
	handler, database, _ := newTestServer(t, testServerOptions{
		testHooks: true,
		settle:    time.Hour,
		persistence: func(database *store.Store) docs.VersionedStore {
			broken = &invalidLoadOnceStore{VersionedStore: docs.NewPgVersioned(database), invalid: make(map[string]bool)}
			return broken
		},
	})
	const spec = "# Kept\n\nThe first paragraph holds a comment.\n\nThe second paragraph holds an ask.\n\n" +
		":::ask{urgency=\"med\"}\nWhich transport should we expose?\n\n- REST: the platform's own\n- gRPC: streaming\n:::\n\n" +
		"The third paragraph holds another comment.\n\nThe last paragraph is rewritten.\n"
	type anchored struct {
		ID     string `json:"id"`
		Anchor struct {
			MarkID string `json:"mark_id"`
		} `json:"anchor"`
	}
	for index, path := range []string{"the dashboard's, from the history's own text", "from supplied markdown", "of a history that does not load"} {
		t.Run(path, func(t *testing.T) {
			issue := createInteractionIssue(t, handler, fmt.Sprintf("KEEP%d", index), "Kept anchors", spec)
			artifact := issue.PrimaryArtifactID
			marks := map[string]string{}
			for _, quote := range []string{"first paragraph holds a comment", "third paragraph holds another comment"} {
				comment := createThreadComment(t, handler, issue.Key, map[string]any{
					"body": "A comment on " + quote + ".", "anchor": map[string]any{"artifact": "spec", "quote": quote},
				}, "alice")
				marks[comment.Anchor.MarkID] = string(docs.MarkComment)
				createThreadComment(t, handler, issue.Key, map[string]any{"body": "A reply.", "reply_to": comment.ID}, "bob")
			}
			ask := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
				"anchor": map[string]string{"artifact": "spec", "quote": "second paragraph holds an ask"}, "question": "Is this complete?",
			}, "alice")
			if ask.Code != http.StatusCreated {
				t.Fatalf("create ask: status=%d body=%s", ask.Code, ask.Body.String())
			}
			marks[decodeBody[anchored](t, ask).Anchor.MarkID] = string(docs.MarkAsk)
			if quiesced := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/_test/quiesce", nil, "alice"); quiesced.Code != http.StatusOK {
				t.Fatalf("quiesce: status=%d body=%s", quiesced.Code, quiesced.Body.String())
			}
			before := readKept(t, database, artifact, marks)
			if len(before.margin) != 2 || len(before.blocks) < 6 {
				t.Fatalf("the document before its rebuild holds %d blocks and %d margin records, want its blocks and both comments' records", len(before.blocks), len(before.margin))
			}
			for markID, at := range before.anchors {
				if at == "" {
					t.Fatalf("the document before its rebuild holds no mark %s", markID)
				}
			}
			body := map[string]any{}
			rewritten := -1
			// The ids a rebuild must keep: every block's, but for a history that does not load,
			// whose own ids only it held, the ids the rows record of it - the block index's, which
			// lists every block a reference or an anchor can name, and no list item.
			var recorded map[string]bool
			switch index {
			case 0, 1:
				if near := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifact+"/_test/near-decode-cap", nil, "alice"); near.Code != http.StatusNoContent {
					t.Fatalf("near-decode-cap hook: status=%d body=%.300s", near.Code, near.Body.String())
				}
				if index == 1 {
					text := decodeBody[artifactText](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifact+"/text", nil, "alice"))
					body["markdown"] = strings.Replace(text.Markdown, "The last paragraph is rewritten.", "The last paragraph was rewritten by the rebuild.", 1)
					rewritten = len(before.blocks) - 1
				}
			case 2:
				recorded = map[string]bool{}
				rows, err := database.Pool.Query(context.Background(), `select block_id from document_blocks where artifact_id = $1`, artifact)
				if err != nil {
					t.Fatal(err)
				}
				for rows.Next() {
					var id string
					if err := rows.Scan(&id); err != nil {
						t.Fatal(err)
					}
					recorded[id] = true
				}
				rows.Close()
				broken.failNextLoad(artifact)
			}
			rebuilt := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifact+"/rebuild", body, "alice")
			if rebuilt.Code != http.StatusOK {
				t.Fatalf("rebuild: status=%d body=%s", rebuilt.Code, rebuilt.Body.String())
			}
			after := readKept(t, database, artifact, marks)
			lost, unrecorded := 0, 0
			for i, id := range before.blocks {
				if i == rewritten {
					continue
				}
				if recorded != nil && !recorded[id] {
					unrecorded++
					continue
				}
				if i >= len(after.blocks) || after.blocks[i] != id {
					lost++
					t.Logf("block %d was %s and is %v", i, id, after.blocks[min(i, len(after.blocks)-1)])
				}
			}
			droppedMarks, droppedRecords := 0, 0
			for markID, at := range before.anchors {
				if after.anchors[markID] != at {
					droppedMarks++
					t.Logf("mark %s stood at %q and stands at %q", markID, at, after.anchors[markID])
				}
			}
			for key, record := range before.margin {
				if after.margin[key] != record {
					droppedRecords++
					t.Logf("margin record %s was %s and is %q", key, record, after.margin[key])
				}
			}
			t.Logf("%d of %d block ids (%d no row records), %d of %d anchor marks and %d of %d margin records not kept", lost, len(before.blocks), unrecorded, droppedMarks, len(before.anchors), droppedRecords, len(before.margin))
			if lost+droppedMarks+droppedRecords > 0 || len(after.blocks) != len(before.blocks) {
				t.Fatalf("the rebuild %s left %d of %d block ids, %d of %d anchor marks and %d of %d margin records unkept", path, lost, len(before.blocks), droppedMarks, len(before.anchors), droppedRecords, len(before.margin))
			}
		})
	}
}
