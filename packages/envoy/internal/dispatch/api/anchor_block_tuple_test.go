package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// anchorOrphanStatement is orphanCommentAnchor's update of the comment's anchor, which marks it
// orphaned in place (jsonb_set).
const anchorOrphanStatement = "update comments set anchor = jsonb_set(anchor, '{orphaned}', to_jsonb(true)) where id = $1"

// commentAnchorOrphaning is an accept that finds a suggestion's mark gone (orphanCommentAnchor),
// through its route: the issue's owner lock (lockOwnerOfComment), under which it reads the comment
// (lockedComment), the update that writes its anchor orphaned, and the retraction of its
// anchored_in, appended at the sentinel since the orphaning appends no event. Each run anchors a
// suggestion of its own on a paragraph written for it into the rig's spec, deletes that paragraph
// through the edit route, and stands the suggestion's anchor and its anchored_in up again as a row
// the refresh did not reach, so the accept is what orphans it. The tuple's key is the comment's,
// drawn in the run, so the tuple step is held as the table it is appended to.
func (r *storeWriterRig) commentAnchorOrphaning() storeWriter {
	return storeWriter{
		name: "orphanCommentAnchor under an accept",
		steps: []storeStep{
			ownerLockStep(store.IssueOwner(r.issue)),
			{
				name: "the comment's anchor", order: projectionStep,
				hold: holdTableWrites("comments"), at: anchorOrphanStatement,
				takes: []tableLock{{"comments", rowExclusiveMode}},
			},
			{
				name: "the anchored_in's retraction", order: tupleStep,
				hold: holdTableWrites("graph_tuples"), at: tupleOnlyAppendStatement,
				takes: []tableLock{{"graph_tuples", rowExclusiveMode}},
			},
		},
		ready: func(t *testing.T, ctx context.Context) storeWriterRun {
			words := r.fresh("Words the suggestion orphans")
			spec := decodeBody[struct {
				PrimaryArtifactID string `json:"primary_artifact_id"`
			}](t, dispatchRequest(t, r.handler, http.MethodGet, "/api/v1/issues/"+r.issue, nil, "alice")).PrimaryArtifactID
			if edited := dispatchRequest(t, r.handler, http.MethodPost, "/api/v1/artifacts/"+spec+"/edits", map[string]any{
				"ops": []map[string]any{{"op": "insert", "after": "end", "markdown": words + ".\n"}},
			}, "alice"); edited.Code != http.StatusOK {
				t.Fatalf("write the paragraph the suggestion anchors on: status=%d body=%s", edited.Code, edited.Body.String())
			}
			suggested := dispatchRequest(t, r.handler, http.MethodPost, "/api/v1/issues/"+r.issue+"/comments", map[string]any{
				"body": "Other words.", "anchor": map[string]any{"artifact": spec, "quote": words},
				"suggestion": map[string]string{"replace_with": "Other words"},
			}, "alice")
			if suggested.Code != http.StatusCreated {
				t.Fatalf("anchor the suggestion: status=%d body=%s", suggested.Code, suggested.Body.String())
			}
			comment := decodeBody[model.Comment](t, suggested)
			if comment.Anchor == nil || comment.Anchor.BlockID == nil {
				t.Fatalf("the suggestion answers anchor %+v, want one naming its block", comment.Anchor)
			}
			block := anchoredBlockAddress(t, r.database, spec, *comment.Anchor.BlockID)
			if edited := dispatchRequest(t, r.handler, http.MethodPost, "/api/v1/artifacts/"+spec+"/edits", map[string]any{
				"ops": []map[string]any{{"op": "delete", "find": words + "."}},
			}, "alice"); edited.Code != http.StatusOK {
				t.Fatalf("delete the suggestion's paragraph: status=%d body=%s", edited.Code, edited.Body.String())
			}
			if _, err := r.database.Pool.Exec(ctx, `update comments set anchor = jsonb_set(anchor, '{orphaned}', 'false') where id = $1::uuid`, comment.ID); err != nil {
				t.Fatalf("stand the suggestion's anchor up again: %v", err)
			}
			writeAnchoredIn(t, r.database, comment.ID, block, true)
			return storeWriterRun{
				made: graphstore.Node{Kind: graphstore.KindComment, ID: comment.ID},
				write: func() error {
					response := dispatchRequest(t, r.call, http.MethodPost, "/api/v1/comments/"+comment.ID+"/accept", map[string]any{}, "alice")
					if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"ANCHOR_ORPHANED"`) {
						return fmt.Errorf("status=%d body=%s, want 409 ANCHOR_ORPHANED", response.Code, response.Body.String())
					}
					held, err := graphstore.CurrentOut(context.Background(), r.database.Pool, graphstore.KindComment, comment.ID, []string{graphstore.TupleAnchoredIn})
					if err != nil {
						return err
					}
					if len(held) != 0 {
						return fmt.Errorf("the orphaned suggestion states anchored_in %+v, want none", held)
					}
					return nil
				},
			}
		},
	}
}

// anchoredBlockAddress is the address of block of the document documentID as the block index
// addresses it: the ref_key of the room the document is edited in - its issue's tree for a
// document of an issue room - '#' and the block id.
func anchoredBlockAddress(t *testing.T, database *store.Store, documentID, block string) string {
	t.Helper()
	var address string
	if err := database.Pool.QueryRow(context.Background(),
		`select `+graphstore.RoomRefKeySQL("$1::uuid")+` || '#' || $2::text`, documentID, block).Scan(&address); err != nil {
		t.Fatalf("read the address of block %s of %s: %v", block, documentID, err)
	}
	return address
}

// indexedBlockNode is the node_id of block's document_blocks row in the document documentID, keyed
// under the room the document is edited in, which is the block index's own address of the block.
func indexedBlockNode(t *testing.T, database *store.Store, documentID, block string) string {
	t.Helper()
	var node string
	if err := database.Pool.QueryRow(context.Background(), `
		select d.node_id from document_blocks d
		where d.artifact_id = `+refs.DocumentRoomSQL("$1::uuid")+`
		  and d.block_id = $2
	`, documentID, block).Scan(&node); err != nil {
		t.Fatalf("read the indexed node of block %s of %s: %v", block, documentID, err)
	}
	return node
}

// storedCommentAnchor is the block id and the orphan flag comments.anchor holds for comment.
func storedCommentAnchor(t *testing.T, database *store.Store, comment string) (string, bool) {
	t.Helper()
	var block string
	var orphaned bool
	if err := database.Pool.QueryRow(context.Background(), `
		select coalesce(anchor->>'block_id', ''), coalesce((anchor->>'orphaned')::boolean, false)
		from comments where id = $1::uuid
	`, comment).Scan(&block, &orphaned); err != nil {
		t.Fatalf("read the anchor of comment %s: %v", comment, err)
	}
	return block, orphaned
}

// requireAnchoredInAgrees holds the anchored_in comment's node states now to the block its stored
// anchor names: the one block's node (anchoredBlockAddress) while the anchor names a block and is
// not orphaned, and none otherwise. It answers what the node states.
func requireAnchoredInAgrees(t *testing.T, database *store.Store, documentID, comment, what string) []graphstore.Tuple {
	t.Helper()
	held, err := graphstore.CurrentOut(context.Background(), database.Pool, graphstore.KindComment, comment, []string{graphstore.TupleAnchoredIn})
	if err != nil {
		t.Fatalf("%s: read the anchored_in of comment %s: %v", what, comment, err)
	}
	block, orphaned := storedCommentAnchor(t, database, comment)
	if block == "" || orphaned {
		if len(held) != 0 {
			t.Errorf("%s: comment %s's anchor names block %q orphaned=%t, and its node states anchored_in %+v, want none", what, comment, block, orphaned, held)
		}
		return held
	}
	want := anchoredBlockAddress(t, database, documentID, block)
	if len(held) != 1 || held[0].ToKind != refs.BlockNodeKind || held[0].ToID != want {
		t.Errorf("%s: comment %s's anchor names block %q, and its node states anchored_in %+v, want the one to %s:%s", what, comment, block, held, refs.BlockNodeKind, want)
	}
	return held
}

// writeAnchoredIn states, or retracts where asserted is false, the anchored_in of comment to block
// through the store's writer, as a build that wrote the anchor's row without it left the store.
func writeAnchoredIn(t *testing.T, database *store.Store, comment, block string, asserted bool) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the anchored_in's write: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := graphstore.Write(ctx, tx, nil, []graphstore.Tuple{
		graphstore.AnchoredIn(comment, block, asserted, model.Actor{Kind: "user", ID: "alice"}, time.Now().UTC(), graphstore.NoEvent),
	}); err != nil {
		t.Fatalf("write the anchored_in of comment %s: %v", comment, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the anchored_in's write: %v", err)
	}
}

// newestCommentEvent is the id of the newest event of type that names comment.
func newestCommentEvent(t *testing.T, database *store.Store, comment, eventType string) int64 {
	t.Helper()
	var id int64
	if err := database.Pool.QueryRow(context.Background(), `
		select id from events where type = $1 and payload->>'id' = $2 order by id desc limit 1
	`, eventType, comment).Scan(&id); err != nil {
		t.Fatalf("read the newest %s of comment %s: %v", eventType, comment, err)
	}
	return id
}

// createAnchoredComment writes, as alice, a comment of issue anchored as anchor names, and answers
// it.
func createBlockAnchoredComment(t *testing.T, handler http.Handler, issue string, anchor map[string]any) model.Comment {
	t.Helper()
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/comments", map[string]any{
		"body": "A remark on these words.", "anchor": anchor,
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("anchor a comment at %v: status=%d body=%s", anchor, created.Code, created.Body.String())
	}
	comment := decodeBody[model.Comment](t, created)
	if comment.Anchor == nil || comment.Anchor.BlockID == nil || *comment.Anchor.BlockID == "" {
		t.Fatalf("the comment anchored at %v answers anchor %+v, want one naming its block", anchor, comment.Anchor)
	}
	return comment
}

// A comment's anchor block, comments.anchor's block_id, is the projection of an anchored_in tuple
// from the comment's node to the block's node (T17b of this branch's plan), and every write that
// sets or moves it keeps the two level in its own transaction. Over a spec of an issue born
// converted: the create states the anchored_in to the block's node as the block index addresses
// it, under the comment.created that stored it; the anchor block backfill, over a row whose anchor
// names no block and whose node states no anchored_in, pins the block and states it; the refresh,
// once an upload removes a suggestion's quote, orphans its anchor and retracts it; an accept that
// finds the suggestion's mark gone from a row the refresh had not orphaned (its row and its tuple
// put back as such a row stands) orphans the anchor and retracts it; and the refresh that finds a
// stale orphaned anchor's mark again states its block once more, under that comment's own
// comment.anchor_refreshed. After each, the anchored_in names the block comments.anchor names, or
// none.
func TestAnAnchorBlockIsATupleEveryAnchorWriteKeeps(t *testing.T) {
	ctx := context.Background()
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	documentService := deps.Docs.(*docs.Service)
	alice := model.Actor{Kind: "user", ID: "alice"}
	issue := createInteractionIssue(t, handler, "TEST", "Anchor blocks", "The quick brown fox.\n\nA lazy dog sleeps.\n\nThe moon is up.\n")
	spec := issue.PrimaryArtifactID

	fox := createBlockAnchoredComment(t, handler, issue.Key, map[string]any{"artifact": "spec", "quote": "brown fox"})
	held := requireAnchoredInAgrees(t, database, spec, fox.ID, "the create")
	if len(held) == 1 {
		if indexed := indexedBlockNode(t, database, spec, *fox.Anchor.BlockID); held[0].ToID != indexed {
			t.Errorf("the create states anchored_in to %s, want the block index's own node of the block, %s", held[0].ToID, indexed)
		}
		if created := newestCommentEvent(t, database, fox.ID, "comment.created"); held[0].SourceEvent != created {
			t.Errorf("the create's anchored_in is caused by event %d, want the comment.created %d that stored the comment", held[0].SourceEvent, created)
		}
	}

	dog := createBlockAnchoredComment(t, handler, issue.Key, map[string]any{"artifact": "spec", "quote": "lazy dog"})
	dogBlock := anchoredBlockAddress(t, database, spec, *dog.Anchor.BlockID)
	if _, err := database.Pool.Exec(ctx, `update comments set anchor = anchor - 'block_id' where id = $1::uuid`, dog.ID); err != nil {
		t.Fatalf("make the second comment's anchor one the release before wrote with no block: %v", err)
	}
	writeAnchoredIn(t, database, dog.ID, dogBlock, false)
	requireAnchoredInAgrees(t, database, spec, dog.ID, "an anchor written with no block")
	backfilled, err := documentService.BackfillAnchorBlocks(ctx)
	if err != nil {
		t.Fatalf("backfill the anchor blocks: %v", err)
	}
	if backfilled.Comments != 1 {
		t.Fatalf("the anchor block backfill pinned %d comments, want the one written with no block", backfilled.Comments)
	}
	if held := requireAnchoredInAgrees(t, database, spec, dog.ID, "the anchor block backfill"); len(held) != 1 {
		t.Fatalf("the anchor block backfill left comment %s with anchored_in %+v, want its block", dog.ID, held)
	}

	suggested := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "It has set.", "anchor": map[string]any{"artifact": "spec", "quote": "moon is up"},
		"suggestion": map[string]string{"replace_with": "moon has set"}, "actor": sessionActor(),
	})
	if suggested.Code != http.StatusCreated {
		t.Fatalf("create the suggestion: status=%d body=%s", suggested.Code, suggested.Body.String())
	}
	moon := decodeBody[model.Comment](t, suggested)
	moonHeld := requireAnchoredInAgrees(t, database, spec, moon.ID, "the suggestion's create")
	if len(moonHeld) != 1 {
		t.Fatalf("the suggestion states anchored_in %+v, want its block", moonHeld)
	}
	moonBlock := moonHeld[0].ToID

	if _, err := replaceDocumentText(database, deps.Docs, spec, "The quick brown fox.\n\nA lazy dog sleeps.\n", alice); err != nil {
		t.Fatalf("upload the spec without the suggestion's quote: %v", err)
	}
	if _, orphaned := storedCommentAnchor(t, database, moon.ID); !orphaned {
		t.Fatalf("the upload left the suggestion's anchor standing, want the refresh to orphan it")
	}
	requireAnchoredInAgrees(t, database, spec, moon.ID, "the refresh's orphaning")

	if _, err := database.Pool.Exec(ctx, `update comments set anchor = jsonb_set(anchor, '{orphaned}', 'false') where id = $1::uuid`, moon.ID); err != nil {
		t.Fatalf("stand the suggestion's anchor up again as a row the refresh did not reach: %v", err)
	}
	writeAnchoredIn(t, database, moon.ID, moonBlock, true)
	if held := requireAnchoredInAgrees(t, database, spec, moon.ID, "a standing anchor whose mark is gone"); len(held) != 1 {
		t.Fatalf("the put-back suggestion states anchored_in %+v, want its block", held)
	}
	accepted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+moon.ID+"/accept", map[string]any{}, "alice")
	if accepted.Code != http.StatusConflict || !strings.Contains(accepted.Body.String(), `"code":"ANCHOR_ORPHANED"`) {
		t.Fatalf("accept the suggestion whose mark is gone: status=%d body=%s, want 409 ANCHOR_ORPHANED", accepted.Code, accepted.Body.String())
	}
	if _, orphaned := storedCommentAnchor(t, database, moon.ID); !orphaned {
		t.Fatalf("the accept left the suggestion's anchor standing, want it orphaned")
	}
	requireAnchoredInAgrees(t, database, spec, moon.ID, "the re-anchor's orphaning")

	foxBlock := anchoredBlockAddress(t, database, spec, *fox.Anchor.BlockID)
	if _, err := database.Pool.Exec(ctx, `update comments set anchor = jsonb_set(anchor, '{orphaned}', 'true') where id = $1::uuid`, fox.ID); err != nil {
		t.Fatalf("make the first comment's anchor a stale orphaned one: %v", err)
	}
	writeAnchoredIn(t, database, fox.ID, foxBlock, false)
	requireAnchoredInAgrees(t, database, spec, fox.ID, "a stale orphaned anchor")
	if _, err := replaceDocumentText(database, deps.Docs, spec, "The quick brown fox.\n\nA lazy dog sleeps.\n\nStars come out.\n", alice); err != nil {
		t.Fatalf("upload the spec with a new paragraph: %v", err)
	}
	if _, orphaned := storedCommentAnchor(t, database, fox.ID); orphaned {
		t.Fatalf("the upload left the first comment's anchor orphaned, want the refresh to find its mark again")
	}
	if held := requireAnchoredInAgrees(t, database, spec, fox.ID, "the refresh's return of an anchor"); len(held) == 1 {
		if refreshed := newestCommentEvent(t, database, fox.ID, "comment.anchor_refreshed"); held[0].SourceEvent != refreshed {
			t.Errorf("the refresh's anchored_in is caused by event %d, want the comment.anchor_refreshed %d of the comment", held[0].SourceEvent, refreshed)
		}
	} else {
		t.Errorf("the refresh that found the first comment's mark again states anchored_in %+v, want its block", held)
	}
	for _, comment := range []string{fox.ID, dog.ID, moon.ID} {
		requireAnchoredInAgrees(t, database, spec, comment, "the end")
	}
}

// commentCount is how many comments issue holds.
func commentCount(t *testing.T, database *store.Store, issue string) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from comments where issue_key = $1`, issue).Scan(&count); err != nil {
		t.Fatalf("count the comments of %s: %v", issue, err)
	}
	return count
}

// anchoredTop is the id of the top-level block of the path the single comment read answers for
// comment's anchor block, "" where it answers none.
func anchoredTop(t *testing.T, handler http.Handler, comment string) string {
	t.Helper()
	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/comments/"+comment, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("read comment %s: status=%d body=%s", comment, read.Code, read.Body.String())
	}
	stored := decodeBody[struct {
		Comment model.Comment `json:"comment"`
	}](t, read).Comment
	if stored.AnchorBlock == nil || len(stored.AnchorBlock.Path) == 0 {
		return ""
	}
	return stored.AnchorBlock.Path[0].ID
}

// blockScopedSpec holds "turns at noon" once above the callout tide and once inside it, "tide
// turns" once above it and twice inside it, and a callout calm that holds neither.
const blockScopedSpec = "The tide turns at noon.\n\n:::callout{#tide}\nThe tide turns at noon, and the tide turns at dusk.\n:::\n\n" +
	":::callout{#calm}\nThe sea is calm.\n:::\n"

// An anchor's block_id has the server find the quote inside that block alone and count occurrence
// within it (R8 of the issue-tree spec, T17b of its plan). Over a document that is its own room,
// a quote written once above the callout tide and once inside it anchors inside it by block_id with
// no occurrence, and the comment's anchored_in names the block comments.anchor stores; a quote
// written twice inside the block is ambiguous there with no occurrence, and an occurrence past the
// block's own two matches is a miss though the document holds a third; and a block_id of no block
// of the document, of a block that does not hold the quote, or sent beside a mark_id is refused 400
// INVALID_ANCHOR naming block_id, with no comment written for any of them. On an issue born
// converted, an anchor naming the spec's artifact id, a quote and a block_id is written into the
// spec's fragment of the issue room, where the spec's own mark read finds it, inside that block,
// and its anchored_in names the block's node under the issue's tree.
func TestAnAnchorByBlockFindsTheQuoteInThatBlockAlone(t *testing.T) {
	t.Run("on a document that is its own room", func(t *testing.T) {
		handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
		holdIssuesUnconverted(t, database)
		issue := createInteractionIssue(t, handler, "TEST", "Anchors by block", blockScopedSpec)

		inside := createBlockAnchoredComment(t, handler, issue.Key, map[string]any{"artifact": "spec", "quote": "turns at noon", "block_id": "tide"})
		if top := anchoredTop(t, handler, inside.ID); top != "tide" {
			t.Errorf("the anchor by block_id tide stands under top-level block %q, want tide", top)
		}
		if inside.Anchor.Quote != "turns at noon" {
			t.Errorf("the anchor by block_id quotes %q, want turns at noon", inside.Anchor.Quote)
		}
		requireAnchoredInAgrees(t, database, issue.PrimaryArtifactID, inside.ID, "the anchor by block_id")
		written := commentCount(t, database, issue.Key)

		for _, refused := range []struct {
			name   string
			anchor map[string]any
			status int
			code   string
			names  string
		}{
			{"a quote twice inside the block, with no occurrence", map[string]any{"artifact": "spec", "quote": "tide turns", "block_id": "tide"},
				http.StatusConflict, "TARGET_AMBIGUOUS", ""},
			{"an occurrence past the block's own matches", map[string]any{"artifact": "spec", "quote": "tide turns", "block_id": "tide", "occurrence": 2},
				http.StatusNotFound, "TARGET_NOT_FOUND", ""},
			{"a block_id of no block of the document", map[string]any{"artifact": "spec", "quote": "turns at noon", "block_id": "nowhere"},
				http.StatusBadRequest, "INVALID_ANCHOR", "block_id"},
			{"a block_id of a block without the quote", map[string]any{"artifact": "spec", "quote": "turns at noon", "block_id": "calm"},
				http.StatusBadRequest, "INVALID_ANCHOR", "block_id"},
			{"a block_id beside a mark_id", map[string]any{"artifact": "spec", "mark_id": "m-1", "block_id": "tide"},
				http.StatusBadRequest, "INVALID_ANCHOR", "block_id"},
		} {
			response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
				"body": "A remark on these words.", "anchor": refused.anchor,
			}, "alice")
			if response.Code != refused.status || !strings.Contains(response.Body.String(), `"code":"`+refused.code+`"`) ||
				!strings.Contains(response.Body.String(), refused.names) {
				t.Errorf("%s: status=%d body=%s, want %d %s naming %q", refused.name, response.Code, response.Body.String(), refused.status, refused.code, refused.names)
			}
			if count := commentCount(t, database, issue.Key); count != written {
				t.Errorf("%s: the issue holds %d comments after the refusal, want the %d it held", refused.name, count, written)
			}
		}

		second := createBlockAnchoredComment(t, handler, issue.Key, map[string]any{"artifact": "spec", "quote": "tide turns", "block_id": "tide", "occurrence": 1})
		if top := anchoredTop(t, handler, second.ID); top != "tide" {
			t.Errorf("the second match inside tide stands under top-level block %q, want tide", top)
		}
		requireAnchoredInAgrees(t, database, issue.PrimaryArtifactID, second.ID, "the second match inside the block")
	})

	t.Run("on a converted spec, by its artifact id", func(t *testing.T) {
		ctx := context.Background()
		handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
		issue := createInteractionIssue(t, handler, "TEST", "Anchors by block in a room", blockScopedSpec)
		tree, err := issueTreeOf(ctx, database.Pool, issue.Key)
		if err != nil || tree == "" {
			t.Fatalf("issue %s holds tree %q (%v), want it born converted", issue.Key, tree, err)
		}
		spec := issue.PrimaryArtifactID

		comment := createBlockAnchoredComment(t, handler, issue.Key, map[string]any{"artifact": spec, "quote": "turns at noon", "block_id": "tide"})
		if comment.Anchor.ArtifactID != spec {
			t.Errorf("the anchor names artifact %s, want the spec %s it was asked on", comment.Anchor.ArtifactID, spec)
		}
		anchored, err := deps.Docs.(*docs.Service).VerifyMark(ctx, spec, docs.MarkComment, comment.Anchor.MarkID)
		if err != nil || anchored.Quote != "turns at noon" || anchored.BlockID != *comment.Anchor.BlockID {
			t.Errorf("the spec's fragment of the room holds mark %s as %+v (%v), want it on turns at noon in block %s",
				comment.Anchor.MarkID, anchored, err, *comment.Anchor.BlockID)
		}
		if top := anchoredTop(t, handler, comment.ID); top != "tide" {
			t.Errorf("the anchor by block_id tide stands under top-level block %q, want tide", top)
		}
		held := requireAnchoredInAgrees(t, database, spec, comment.ID, "the anchor on the converted spec")
		var treeKey string
		if err := database.Pool.QueryRow(ctx, `select ref_key from artifacts where id = $1::uuid`, tree).Scan(&treeKey); err != nil {
			t.Fatalf("read the tree's ref_key: %v", err)
		}
		if len(held) != 1 || held[0].ToID != treeKey+"#"+*comment.Anchor.BlockID {
			t.Errorf("the anchor on the converted spec states anchored_in %+v, want the block's node under the issue's tree, %s#%s",
				held, treeKey, *comment.Anchor.BlockID)
		} else if indexed := indexedBlockNode(t, database, spec, *comment.Anchor.BlockID); held[0].ToID != indexed {
			t.Errorf("the anchor on the converted spec states anchored_in to %s, want the block index's own node of the block, %s", held[0].ToID, indexed)
		}
	})
}
