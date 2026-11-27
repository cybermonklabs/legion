package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// big.md holds moveKeepsIDsParagraphs paragraphs, each with a text of its own. Version 2 moves
// paragraph moveKeepsIDsMoved to after paragraph moveKeepsIDsAfter, so it passes every paragraph
// from the one after it to that one: 1,490 paragraphs, paragraphs 1000 and 1200 among them.
const (
	moveKeepsIDsParagraphs = 2_000
	moveKeepsIDsMoved      = 250
	moveKeepsIDsAfter      = 1_740
	moveKeepsIDsCitedFirst = 1_000
	moveKeepsIDsAnchored   = 1_200
)

// moveKeepsIDsParagraph is the text of paragraph n of big.md, which no other paragraph holds.
func moveKeepsIDsParagraph(n int) string {
	return fmt.Sprintf("Paragraph %04d of the big document says a thing of its own.", n)
}

// moveKeepsIDsText is big.md with its paragraphs in order, the heading above them where heading
// is set: 2,000 blocks, which the upload indexes itself, or 2,001, more than docs'
// deferBlockIndexBlocks, which the block-index reconciliation indexes.
func moveKeepsIDsText(heading bool, order []int) string {
	var text strings.Builder
	if heading {
		text.WriteString("# The big document\n\n")
	}
	for index, n := range order {
		if index > 0 {
			text.WriteString("\n")
		}
		text.WriteString(moveKeepsIDsParagraph(n) + "\n")
	}
	return text.String()
}

// moveKeepsIDsOrder is the paragraphs of big.md in the order of version 1, or of version 2 where
// moved is set: paragraph 250 taken out and put back right after paragraph 1740.
func moveKeepsIDsOrder(moved bool) []int {
	order := make([]int, 0, moveKeepsIDsParagraphs)
	for n := 1; n <= moveKeepsIDsParagraphs; n++ {
		if !moved || n != moveKeepsIDsMoved {
			order = append(order, n)
		}
		if moved && n == moveKeepsIDsAfter {
			order = append(order, moveKeepsIDsMoved)
		}
	}
	return order
}

// moveKeepsIDsByParagraph is the id GET /api/v1/artifacts/{id}/blocks answers for each paragraph
// of big.md, found by its text (blockIDsByText), indexed by the paragraph's number. It fails t
// where a paragraph is not listed.
func moveKeepsIDsByParagraph(t *testing.T, handler http.Handler, artifactID string) []string {
	t.Helper()
	byText := map[string]string{}
	for text, id := range blockIDsByText(t, handler, artifactID) {
		byText[strings.TrimSpace(text)] = id
	}
	ids := make([]string, moveKeepsIDsParagraphs+1)
	for n := 1; n <= moveKeepsIDsParagraphs; n++ {
		id, listed := byText[moveKeepsIDsParagraph(n)]
		if !listed || id == "" {
			t.Fatalf("GET /api/v1/artifacts/%s/blocks lists no block holding paragraph %d", artifactID, n)
		}
		ids[n] = id
	}
	return ids
}

// moveKeepsIDsReconcile runs passes of the block-index reconciliation (docs.Service.ReconcileBlockIndex)
// until one indexes nothing.
func moveKeepsIDsReconcile(t *testing.T, deps Deps) {
	t.Helper()
	for range 8 {
		if tick := reconcileBlockIndexPass(t, deps); tick.Documents == 0 && tick.Texts == 0 {
			return
		}
	}
	t.Fatal("eight passes of the block-index reconciliation each still indexed something")
}

// moveKeepsIDsStore is what the store holds before the move: how many nodes of kind block, and
// the newest tuple's seq, after which every tuple the move appends stands.
type moveKeepsIDsStore struct {
	blockNodes int
	lastSeq    int64
}

func readMoveKeepsIDsStore(t *testing.T, database *store.Store) moveKeepsIDsStore {
	t.Helper()
	var held moveKeepsIDsStore
	if err := database.Pool.QueryRow(context.Background(), `
		select (select count(*)::int from graph_nodes where kind = 'block'),
		       (select coalesce(max(seq), 0) from graph_tuples)
	`).Scan(&held.blockNodes, &held.lastSeq); err != nil {
		t.Fatalf("read the store before the move: %v", err)
	}
	return held
}

// A version that moves one paragraph keeps the id of that paragraph and of every paragraph it
// passes (R13 of the issue-tree spec): carryBlockIDs carries ids by an alignment of the old and
// the new block sequences, not only to an unchanged block at its paired place. So a block_refs
// pointer and an ask anchor into a passed paragraph still resolve, and the move writes no block
// node and no tuple but the moved paragraph's and the new version's. The test runs twice: over
// 2,000 paragraphs, which the upload indexes itself, and over a heading and 2,000 paragraphs,
// 2,001 blocks, more than docs' deferBlockIndexBlocks, which the block-index reconciliation
// indexes.
func TestAMoveOfOneParagraphKeepsEveryBlockID(t *testing.T) {
	t.Parallel()
	for _, arm := range []struct {
		name    string
		heading bool
	}{
		{name: "2000 paragraphs the upload indexes", heading: false},
		{name: "2001 blocks the reconciliation indexes", heading: true},
	} {
		t.Run(arm.name, func(t *testing.T) {
			moveKeepsIDsArm(t, arm.heading)
		})
	}
}

func moveKeepsIDsArm(t *testing.T, heading bool) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	ctx := context.Background()
	issue := createInteractionIssue(t, handler, "MOVE", "The big document's issue", "The issue that holds big.md.\n")
	citing := createProjectIssue(t, handler, "MOVE", "The citing issue", "The issue whose document cites big.md.\n")
	owner := "/api/v1/issues/" + issue.Key

	big := uploadDocument(t, handler, owner, "big.md", moveKeepsIDsText(heading, moveKeepsIDsOrder(false))).Artifact
	if big.Slug != "big-md" {
		t.Fatalf("big.md was stored under the slug %q, want big-md", big.Slug)
	}
	if deferred := blockIndexVersion(t, database, big.ID) == nil; deferred != heading {
		t.Fatalf("version 1 of big.md owes its block index: %t, want %t (a document of more than 2,000 blocks leaves its index to the reconciliation)", deferred, heading)
	}
	moveKeepsIDsReconcile(t, deps)
	before := moveKeepsIDsByParagraph(t, handler, big.ID)
	paragraphOf := map[string]int{}
	for n := 1; n <= moveKeepsIDsParagraphs; n++ {
		paragraphOf[before[n]] = n
	}
	var refKey string
	if err := database.Pool.QueryRow(ctx, `select ref_key from artifacts where id = $1::uuid`, big.ID).Scan(&refKey); err != nil {
		t.Fatalf("read the ref_key of big.md: %v", err)
	}

	cites := uploadDocument(t, handler, "/api/v1/issues/"+citing, "cites.md", fmt.Sprintf(
		"Paragraph %d is dispatch://%s/artifact/big-md#b-%s here.\n\nParagraph %d is dispatch://%s/artifact/big-md#b-%s here.\n",
		moveKeepsIDsCitedFirst, issue.Key, before[moveKeepsIDsCitedFirst], moveKeepsIDsMoved, issue.Key, before[moveKeepsIDsMoved])).Artifact
	var pointers []string
	for _, row := range readBlockRefs(t, database, cites.ID) {
		if row.ToKind == "block" && strings.HasPrefix(row.ToID, refKey+"#") {
			pointers = append(pointers, strings.TrimPrefix(row.ToID, refKey+"#"))
		}
	}
	slices.Sort(pointers)
	if want := []string{before[moveKeepsIDsCitedFirst], before[moveKeepsIDsMoved]}; !slices.Equal(pointers, slices.Sorted(slices.Values(want))) {
		t.Fatalf("cites.md wrote block_refs rows naming %v of big.md, want one for paragraph %d and one for paragraph %d: %v",
			pointers, moveKeepsIDsCitedFirst, moveKeepsIDsMoved, want)
	}

	asked := dispatchRequest(t, handler, http.MethodPost, owner+"/asks", map[string]any{
		"question": "Does paragraph 1200 say enough?",
		"anchor":   map[string]string{"artifact": "big-md", "quote": moveKeepsIDsParagraph(moveKeepsIDsAnchored)},
	}, "alice")
	if asked.Code != http.StatusCreated {
		t.Fatalf("open the ask anchored on paragraph %d: status=%d body=%s", moveKeepsIDsAnchored, asked.Code, asked.Body.String())
	}
	ask := decodeBody[model.Ask](t, asked)

	awaitSettledDocuments(t, database)
	held := readMoveKeepsIDsStore(t, database)
	blocks := moveKeepsIDsParagraphs
	if heading {
		blocks++
	}
	started := time.Now()
	moved := uploadDocument(t, handler, owner, "big.md", moveKeepsIDsText(heading, moveKeepsIDsOrder(true)))
	t.Logf("the request that uploads version 2 of big.md (%d blocks) took %s", blocks, time.Since(started))
	if moved.Artifact.ID != big.ID || moved.Version.Number != 2 {
		t.Fatalf("the move wrote version %d of %s, want version 2 of %s", moved.Version.Number, moved.Artifact.ID, big.ID)
	}
	if deferred := blockIndexVersion(t, database, big.ID) == nil; deferred != heading {
		t.Fatalf("version 2 of big.md owes its block index: %t, want %t", deferred, heading)
	}
	moveKeepsIDsReconcile(t, deps)
	awaitSettledDocuments(t, database)

	after := moveKeepsIDsByParagraph(t, handler, big.ID)
	firstPassed, passedChanged := 0, 0
	for n := moveKeepsIDsMoved + 1; n <= moveKeepsIDsAfter; n++ {
		if after[n] == before[n] {
			continue
		}
		passedChanged++
		if firstPassed == 0 {
			firstPassed = n
			t.Errorf("passed paragraph %d changed id %s to %s", n, before[n], after[n])
		}
	}
	if passedChanged > 0 {
		t.Errorf("%d of the %d passed paragraphs changed id", passedChanged, moveKeepsIDsAfter-moveKeepsIDsMoved)
	}
	if after[moveKeepsIDsMoved] != before[moveKeepsIDsMoved] {
		t.Errorf("moved paragraph %d changed id %s to %s", moveKeepsIDsMoved, before[moveKeepsIDsMoved], after[moveKeepsIDsMoved])
	}
	for n := 1; n <= moveKeepsIDsParagraphs; n++ {
		if (n < moveKeepsIDsMoved || n > moveKeepsIDsAfter) && after[n] != before[n] {
			t.Errorf("paragraph %d, which the move neither moves nor passes, changed id %s to %s", n, before[n], after[n])
		}
	}

	for _, id := range pointers {
		n := paragraphOf[id]
		read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+big.ID+"/blocks/"+id, nil, "alice")
		if read.Code != http.StatusOK {
			var refusal struct {
				Code string `json:"code"`
			}
			_ = json.Unmarshal(read.Body.Bytes(), &refusal)
			t.Errorf("the block_refs pointer at paragraph %d answers %d %s", n, read.Code, refusal.Code)
			continue
		}
		if path := decodeBody[model.BlockPath](t, read); path.ID != after[n] {
			t.Errorf("the block_refs pointer at paragraph %d answers block %s, want paragraph %d's block %s", n, path.ID, n, after[n])
		}
	}
	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+ask.ID, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("read the ask anchored on paragraph %d: status=%d body=%s", moveKeepsIDsAnchored, read.Code, read.Body.String())
	}
	anchored := decodeBody[struct {
		Ask struct {
			AnchorBlock *model.BlockPath `json:"anchor_block"`
		} `json:"ask"`
	}](t, read).Ask.AnchorBlock
	switch {
	case anchored == nil:
		t.Errorf("the ask anchored on paragraph %d answers no anchor_block", moveKeepsIDsAnchored)
	case anchored.ID != after[moveKeepsIDsAnchored]:
		t.Errorf("the ask anchored on paragraph %d answers anchor_block %s, want paragraph %d's block %s",
			moveKeepsIDsAnchored, anchored.ID, moveKeepsIDsAnchored, after[moveKeepsIDsAnchored])
	}

	moveKeepsIDsRequireWritten(t, database, held, refKey+"#"+before[moveKeepsIDsMoved], refKey+"@v2")
}

// moveKeepsIDsRequireWritten fails t unless the upload of version 2 and the reconciliation after
// it, which appended every tuple past held's newest seq, added no node of kind block, appended out
// of a block node only tuples out of movedNode, the moved paragraph's (its rank and its text_rank,
// R1 of the issue-tree spec), and every other tuple out of version, the new version's node.
func moveKeepsIDsRequireWritten(t *testing.T, database *store.Store, held moveKeepsIDsStore, movedNode, version string) {
	t.Helper()
	ctx := context.Background()
	if now := readMoveKeepsIDsStore(t, database); now.blockNodes != held.blockNodes {
		t.Errorf("the move added %d nodes of kind block", now.blockNodes-held.blockNodes)
	}
	rows, err := database.Pool.Query(ctx, `
		select from_kind, from_id, count(*)::int from graph_tuples where seq > $1
		group by from_kind, from_id order by from_kind, from_id
	`, held.lastSeq)
	if err != nil {
		t.Fatalf("read the tuples the move appended: %v", err)
	}
	defer rows.Close()
	otherBlocks, otherBlockTuples := 0, 0
	for rows.Next() {
		var fromKind, fromID string
		var count int
		if err := rows.Scan(&fromKind, &fromID, &count); err != nil {
			t.Fatalf("scan a tuple the move appended: %v", err)
		}
		switch {
		case fromKind == "block" && fromID == movedNode:
		case fromKind == "block":
			otherBlocks++
			otherBlockTuples += count
		case fromKind == "version" && fromID == version:
		default:
			t.Errorf("the move appended %d tuples out of the %s node %s, which is not the new version's node %s", count, fromKind, fromID, version)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the tuples the move appended: %v", err)
	}
	if otherBlocks > 0 {
		t.Errorf("the move appended %d tuples out of %d blocks other than the moved one", otherBlockTuples, otherBlocks)
	}
}
