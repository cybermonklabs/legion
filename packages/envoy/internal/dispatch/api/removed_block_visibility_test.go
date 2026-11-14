package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// R19: a block reserved for its author answers a pinned read of its own address, for anyone
// else, exactly what an address nobody holds answers. refs.Node answers such a block to another
// person as it answers a block no current version holds, so a pinned read reaches the branch that
// shows a removed block's recorded text, and that text is the block's own.
//
// Two private blocks of one public document carry recorded text at version 1: `private`, which
// the document still holds, and `gone`, which version 2 deleted. Each address, pinned to version
// 1 and not, is read by the second person once before either block exists, when nothing holds
// it, and again after both are written: the two answers are the same bytes. The author reads
// each pinned address and is answered its text, the current block as unchanged and the deleted
// one as removed at version 2.
//
// No route writes an author-private node yet (TestNoReadAnswersANodeTheActingPersonMayNotSee says
// why), so the blocks' rows and nodes are written the way that test writes its own: the rows by
// hand, the nodes through the store's own writer.
func TestAPinnedReadOfABlockHiddenFromTheReaderAnswersWhatNobodyHolds(t *testing.T) {
	handler, database := blockIndexHandler(t)
	ctx := context.Background()
	issue := createInteractionIssue(t, handler, "PRIV", "Private blocks with a recorded text", "# Spec\n\nA public passage.\n")
	editSpec(t, handler, issue.Key, []map[string]any{{"op": "insert", "after": "end", "markdown": "A second public passage."}})
	requireIndexedAt(t, database, issue.PrimaryArtifactID, 2)
	var refKey string
	if err := database.Pool.QueryRow(ctx,
		`select ref_key from artifacts where id = $1::uuid`, issue.PrimaryArtifactID).Scan(&refKey); err != nil {
		t.Fatalf("read the spec's address: %v", err)
	}
	read := func(reference, login string) *httptest.ResponseRecorder {
		return dispatchRequest(t, handler, http.MethodGet, "/api/v1/references?to="+url.QueryEscape(reference), nil, login)
	}
	const privateText, goneText = "A private passage about wolfram tangerine.", "A deleted private passage."
	privateRef := refs.BlockRef("dispatch://"+refKey, "private")
	goneRef := refs.BlockRef("dispatch://"+refKey, "gone")
	addresses := []string{privateRef, privateRef + "@v1", goneRef, goneRef + "@v1"}

	nobody := map[string]*httptest.ResponseRecorder{}
	for _, address := range addresses {
		nobody[address] = read(address, "bob")
	}
	// The pinned answer for an address nobody holds is the 404 of a version that did not hold the
	// block, which needs version 1 on record: a version with no record of its blocks answers
	// another 404, the same for every block, and would compare nothing here.
	if answer, want := nobody[privateRef+"@v1"], `no block \"private\" in dispatch://`+refKey+` at version 1`; answer.Code != http.StatusNotFound ||
		!strings.Contains(answer.Body.String(), want) {
		t.Fatalf("a pinned read of an address nobody holds answers %d %s, want the 404 of a version that did not hold the block, %s",
			answer.Code, answer.Body.String(), want)
	}

	author := model.Actor{Kind: "user", ID: "alice"}
	visibility, err := graphstore.VisibilityFor(graphstore.VisibilityAuthor, author)
	if err != nil {
		t.Fatalf("reserve the blocks for their author: %v", err)
	}
	seedExec(t, database, "the private block's row", `
		insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at)
		values ($1::uuid, 'private', $2, 'paragraph', '', $3, 1, now())
	`, issue.PrimaryArtifactID, refKey+"#private", privateText)
	seedExec(t, database, "the blocks' recorded texts", `
		insert into document_block_texts (artifact_id, block_id, version, text) values
			($1::uuid, 'private', 1, $2), ($1::uuid, 'gone', 1, $3), ($1::uuid, 'gone', 2, null)
	`, issue.PrimaryArtifactID, privateText, goneText)
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the private nodes' write: %v", err)
	}
	defer tx.Rollback(ctx)
	madeAt := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	if err := graphstore.Write(ctx, tx, []graphstore.Node{
		{Kind: "block", ID: refKey + "#gone", Supertag: "block", Visibility: visibility, State: graphstore.StateGone,
			MadeAt: madeAt, MadeBy: author, SourceEvent: graphstore.NoEvent},
		{Kind: "block", ID: refKey + "#private", Supertag: "block", Visibility: visibility, State: graphstore.StateLive,
			MadeAt: madeAt, MadeBy: author, SourceEvent: graphstore.NoEvent},
	}, nil); err != nil {
		t.Fatalf("write the private nodes: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the private nodes' write: %v", err)
	}

	for _, address := range addresses {
		private := read(address, "bob")
		if private.Code != nobody[address].Code || private.Body.String() != nobody[address].Body.String() {
			t.Errorf("the second person's read of %s answers %d\n%s\nand the same address held by nobody answered %d\n%s",
				address, private.Code, private.Body.String(), nobody[address].Code, nobody[address].Body.String())
		}
	}

	for _, want := range []struct {
		address string
		pin     model.BlockPin
	}{
		{privateRef + "@v1", model.BlockPin{Version: 1, Ref: privateRef + "@v1", Text: privateText, State: "unchanged"}},
		{goneRef + "@v1", model.BlockPin{Version: 1, Ref: goneRef + "@v1", Text: goneText, State: "removed", RemovedVersion: 2}},
	} {
		response := read(want.address, author.ID)
		if response.Code != http.StatusOK {
			t.Errorf("the author's read of %s answers %d %s, want the block as version 1 recorded it",
				want.address, response.Code, response.Body.String())
			continue
		}
		pin := decodeBody[model.GraphReferences](t, response).Node.Pin
		if pin == nil || pin.Version != want.pin.Version || pin.Ref != want.pin.Ref || pin.Text != want.pin.Text ||
			pin.State != want.pin.State || pin.RemovedVersion != want.pin.RemovedVersion {
			t.Errorf("the author's read of %s pins %#v, want %#v", want.address, pin, want.pin)
		}
	}
}
