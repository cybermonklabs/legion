package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	ygws "github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

func TestApplyOpsEditsLiveDocumentAndSettlesVersion(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "one two one")
	actor := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	first := 0
	applied, err := joinedApplyOps(service, artifactID, []model.EditOp{
		{Op: "replace", Find: "two", With: "TWO"},
		{Op: "insert", Markdown: "!", After: "end"},
		{Op: "delete", Find: "one ", Occurrence: &first},
	}, actor, nil)

	if err != nil || applied.Applied != 3 || !applied.Changed {
		t.Fatalf("applied = %#v, %v", applied, err)
	}
	waitForDocumentText(t, service, artifactID, "TWO one\n\n!\n")
	version := waitForDocumentVersion(t, service.store, artifactID, 2)
	if len(version.Authors) != 1 || version.Authors[0] != actor {
		t.Fatalf("authors = %#v", version.Authors)
	}
}

// An edit that leaves the canonical markdown as it was is not a failure and not a change: the
// route mints no version for it, so ApplyOps carries the batch's own verdict out.
func TestApplyOpsReportsABatchThatWroteNoUpdatesAsUnchanged(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "before")
	result, err := joinedApplyOps(service, artifactID, []model.EditOp{
		{Op: "replace", Find: "before", With: "before"},
	}, model.Actor{Kind: "session", ID: "session-0123456789abcdef"}, nil)

	if err != nil {
		t.Fatalf("no-op edit: %v", err)
	}
	if result.Applied != 1 || result.Changed || !reflect.DeepEqual(result.Unchanged, []int{0}) {
		t.Fatalf("no-op edit = %#v, want 1 applied, unchanged, naming operation 0", result)
	}
	waitForDocumentText(t, service, artifactID, "before\n")
}

// The other no-op shape: operations that do write Yjs updates (here a fresh block id) and still
// leave the canonical markdown byte-identical. It has to report exactly like the first.
func TestApplyOpsReportsABatchThatWroteUpdatesAndNoMarkdownAsUnchanged(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "before")
	blocksBefore, err := service.Blocks(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("read blocks: %v", err)
	}
	first := 0
	result, err := joinedApplyOps(service, artifactID, []model.EditOp{
		{Op: "insert", Markdown: "before", After: "end"},
		{Op: "delete", Find: "before", Occurrence: &first},
	}, model.Actor{Kind: "session", ID: "session-0123456789abcdef"}, nil)

	if err != nil {
		t.Fatalf("round-trip edit: %v", err)
	}
	if result.Applied != 2 || result.Changed {
		t.Fatalf("round-trip edit = %#v, want 2 applied and unchanged", result)
	}
	waitForDocumentText(t, service, artifactID, "before\n")
	blocksAfter, err := service.Blocks(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("read blocks: %v", err)
	}
	if len(blocksBefore) != 1 || len(blocksAfter) != 1 || blocksBefore[0].ID == blocksAfter[0].ID {
		t.Fatalf("block ids %v -> %v; the round trip must write a new block, not nothing", blocksBefore, blocksAfter)
	}
}

// Canonical markdown renders no anchor mark, so a `replace` whose `with` equals its `find` reads
// as identical text while the human comment anchor it covered is gone - a replace that runs past
// an anchor's edge writes its text outside that anchor. That is a change, and the version that
// records it must still be minted (Deep1326).
func TestApplyOpsReportsAnEditThatOnlyDropsAnAnchorMarkAsChanged(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "Keep anchored words here.")
	if _, err := joinedMarkQuote(service, artifactID, MarkSpec{
		Kind: MarkComment, ID: "comment-1", By: model.Actor{Kind: "user", ID: "alice"},
	}, "anchored words", nil); err != nil {
		t.Fatalf("anchor a comment: %v", err)
	}
	result, err := joinedApplyOps(service, artifactID, []model.EditOp{
		{Op: "replace", Find: "Keep anchored words", With: "Keep anchored words"},
	}, model.Actor{Kind: "session", ID: "session-0123456789abcdef"}, nil)

	if err != nil {
		t.Fatalf("replace over the anchor: %v", err)
	}
	if !result.Changed || len(result.Unchanged) != 0 {
		t.Fatalf("edit that dropped the anchor = %#v, want changed with no unchanged operation", result)
	}
	if _, _, ok := pmdoc.FindMark(liveTree(t, service, artifactID), "proofComment", "comment-1"); ok {
		t.Fatal("the replace kept the anchor, so this no longer exercises the verdict")
	}
}

func TestVerifyTableAnchorSnapshotsRejectsMarkAddedAfterPrevalidation(t *testing.T) {
	tree, err := parseInput("| Key | Value |\n| --- | --- |\n| delete | row |\n| retain | row |\n")
	if err != nil {
		t.Fatalf("parse table: %v", err)
	}
	tableID, _ := tree.Children[0].Attrs[pmdoc.BlockIDAttr].(string)
	op := model.EditOp{Op: "delete_row", Block: tableID, Index: []byte("1")}
	snapshot, marks, table, err := tableAnchorCheck(newBatchTree(tree), op)
	if err != nil || !table || len(marks) != 0 {
		t.Fatalf("table snapshot = %#v marks=%#v err=%v", snapshot, marks, err)
	}
	var markText func(*pmdoc.Node) bool
	markText = func(node *pmdoc.Node) bool {
		if node.Type == "text" && node.Text == "delete" {
			node.Marks = append(node.Marks, pmdoc.Mark{
				Type: string(MarkComment), Attrs: pmdoc.Attrs{"id": "comment-1", "by": "user:alice"},
			})
			return true
		}
		for _, child := range node.Children {
			if markText(child) {
				return true
			}
		}
		return false
	}
	if !markText(tree) {
		t.Fatal("delete cell text missing")
	}
	err = verifyTableAnchorSnapshots(tree, []model.EditOp{op}, []tableAnchorSnapshot{snapshot}, "")
	var invalid *ErrInvalidOp
	if !errors.As(err, &invalid) || invalid.Field != "index" || !strings.Contains(invalid.Reason, "changed during validation") {
		t.Fatalf("table snapshot validation error = %v", err)
	}
}

func TestLockTableAnchorRowsRejectsCommentReopenedAfterPrevalidation(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "| Key | Value |\n| --- | --- |\n| delete | row |\n| retain | row |\n")
	const commentID = "00000000-0000-4000-8000-000000000009"
	anchored, err := joinedMarkQuote(service, artifactID, MarkSpec{
		Kind: MarkComment, ID: commentID, By: model.Actor{Kind: "user", ID: "alice"},
	}, "delete", nil)

	if err != nil {
		t.Fatalf("mark table cell: %v", err)
	}
	var blockID *string
	if anchored.BlockID != "" {
		blockID = &anchored.BlockID
	}
	anchorJSON, err := json.Marshal(model.Anchor{ArtifactID: artifactID, MarkID: commentID, Version: 1, Quote: anchored.Quote, BlockID: blockID})
	if err != nil {
		t.Fatalf("encode anchor: %v", err)
	}
	if _, err := service.store.Pool.Exec(context.Background(), `
		insert into comments (id, issue_key, author, body, anchor, resolved)
		values ($1, 'DOC-1', '{"kind":"user","id":"alice"}', 'Table discussion', $2, true)
	`, commentID, anchorJSON); err != nil {
		t.Fatalf("create resolved table comment: %v", err)
	}
	tree := liveTree(t, service, artifactID)
	tableID, _ := tree.Children[0].Attrs[pmdoc.BlockIDAttr].(string)
	ops := []model.EditOp{{Op: "delete_row", Block: tableID, Index: []byte("1")}}
	snapshots, err := service.prevalidateOperations(context.Background(), artifactID, tree, ops)
	if err != nil {
		t.Fatalf("prevalidate resolved comment: %v", err)
	}
	if _, err := service.store.Pool.Exec(context.Background(), `update comments set resolved = false where id = $1`, commentID); err != nil {
		t.Fatalf("reopen table comment: %v", err)
	}
	tx, err := service.store.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin edit transaction: %v", err)
	}
	defer tx.Rollback(context.Background())
	err = lockTableAnchorRows(context.Background(), tx, artifactID, snapshots)
	var invalid *ErrInvalidOp
	if !errors.As(err, &invalid) || invalid.Field != "index" || !strings.Contains(invalid.Reason, "active anchors") {
		t.Fatalf("reopened table comment lock error = %v", err)
	}

	updateDone := make(chan error, 1)
	go func() {
		_, err := service.store.Pool.Exec(context.Background(), `update comments set body = 'Updated discussion' where id = $1`, commentID)
		updateDone <- err
	}()
	waitForLockWait(t, context.Background(), service.store, "%update comments%", updateDone)
	if err := tx.Rollback(context.Background()); err != nil {
		t.Fatalf("release table share lock: %v", err)
	}
	select {
	case err := <-updateDone:
		if err != nil {
			t.Fatalf("comment body update: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("comment body update remained blocked after the edit transaction released its share lock")
	}
}

func TestConditionalEditAdmissionBoundsQueuedRequests(t *testing.T) {
	service, artifactID := newTestService(t)
	release, err := service.AcquireConditionalEdit(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("acquire first conditional edit: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var queued sync.WaitGroup
	for range maxQueuedConditionalEdits - 1 {
		queued.Add(1)
		go func() {
			defer queued.Done()
			_, _ = service.AcquireConditionalEdit(ctx, artifactID)
		}()
	}
	gateValue, ok := service.conditionalGates.Load(artifactID)
	if !ok {
		t.Fatal("conditional edit gate not registered")
	}
	gate := gateValue.(*conditionalEditGate)
	// Each admission is a short transaction under the queue's advisory lock (admitConditionalEdit),
	// so 31 of them queue one after another; the wait is the room wait, not a fixed second.
	deadline := time.Now().Add(roomWait(t))
	for queueDepth(gate) != maxQueuedConditionalEdits && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := queueDepth(gate); got != maxQueuedConditionalEdits {
		t.Fatalf("queued conditional edits = %d, want %d", got, maxQueuedConditionalEdits)
	}
	if _, err := service.AcquireConditionalEdit(context.Background(), artifactID); !errors.Is(err, ErrPreconditionBusy) {
		t.Fatalf("overflow conditional edit = %v, want ErrPreconditionBusy", err)
	}
	cancel()
	queued.Wait()
	release()
	if _, exists := service.conditionalGates.Load(artifactID); exists {
		t.Fatal("idle conditional edit gate was retained")
	}

}
func queueDepth(gate *conditionalEditGate) int {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	return gate.queued
}

func TestApplyOperationRetypesAParagraphInPlace(t *testing.T) {
	pmdoc.SetBlockIDGenerator(func() string { return "generated-body" })
	t.Cleanup(func() { pmdoc.SetBlockIDGenerator(nil) })
	tree, err := pmdoc.Parse("Which transport should we expose?\n")
	if err != nil {
		t.Fatalf("parse source paragraph: %v", err)
	}
	blockID, _ := tree.Children[0].Attrs[pmdoc.BlockIDAttr].(string)

	retyped, err := applyAlone(tree, model.EditOp{
		Op:         "retype",
		Block:      blockID,
		Type:       "ask",
		Attributes: map[string]any{"multiple": true, "urgency": "high"},
	})
	if err != nil {
		t.Fatalf("retype paragraph: %v", err)
	}
	got, err := pmdoc.Render(retyped)
	if err != nil {
		t.Fatalf("render retyped paragraph: %v", err)
	}
	want := ":::ask{#" + blockID + " urgency=\"high\" multiple=\"true\" state=\"open\"}\nWhich transport should we expose?\n:::\n"
	if got != want {
		t.Fatalf("retyped markdown = %q, want %q", got, want)
	}
}

func TestApplyOperationLabelsInvalidRetypeFields(t *testing.T) {
	tree, err := pmdoc.Parse("Which transport should we expose?\n")
	if err != nil {
		t.Fatalf("parse source paragraph: %v", err)
	}
	blockID, _ := tree.Children[0].Attrs[pmdoc.BlockIDAttr].(string)
	for _, test := range []struct {
		name  string
		op    model.EditOp
		field string
	}{
		{name: "unknown type", op: model.EditOp{Op: "retype", Block: blockID, Type: "missing"}, field: "type"},
		{name: "invalid attributes", op: model.EditOp{Op: "retype", Block: blockID, Type: "ask", Attributes: map[string]any{"urgency": "now"}}, field: "attributes"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := applyAlone(tree, test.op)
			var invalid *ErrInvalidOp
			if !errors.As(err, &invalid) || invalid.Field != test.field {
				t.Fatalf("retype error = %v, want invalid %s", err, test.field)
			}
		})
	}
}

func TestSetBlockAttributesWritesTypedBlockState(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, ":::ask{#ask-1 multiple=\"false\" state=\"open\" urgency=\"med\"}\nShip it?\n:::\n")
	actor := model.Actor{Kind: "user", ID: "alice"}

	err := joinedSetBlockAttributes(service, artifactID, "ask-1", map[string]any{
		"answer":      "Yes.",
		"answered_at": "2026-09-12T13:20:00Z",
		"answered_by": "alice",
		"selected":    []string{"Yes"},
		"state":       "answered",
	}, actor)

	if err != nil {
		t.Fatalf("write ask state: %v", err)
	}
	waitForDocumentText(t, service, artifactID, ":::ask{#ask-1 urgency=\"med\" multiple=\"false\" state=\"answered\" answered_by=\"alice\" answered_at=\"2026-09-12T13:20:00Z\" selected=\"[&#x22;Yes&#x22;]\" answer=\"Yes.\"}\nShip it?\n:::\n")
}

func TestApplyOpsRejectsAmbiguousTargetWithoutChangingDocument(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "same same")
	_, err := joinedApplyOps(service, artifactID, []model.EditOp{{Op: "replace", Find: "same", With: "changed"}}, model.Actor{Kind: "session", ID: "session-0123456789abcdef"}, nil)
	var ambiguous *ErrAnchorAmbiguous
	if !errors.As(err, &ambiguous) || ambiguous.Kind != "quote" || ambiguous.Target != "same" {
		t.Fatalf("ambiguous edit error = %v, want the quote named", err)
	}
	if len(ambiguous.Candidates) != 2 {
		t.Fatalf("ambiguous candidates = %#v, want two candidates", ambiguous.Candidates)
	}
	waitForDocumentText(t, service, artifactID, "same same\n")
}

func TestApplyOpsResolvesAgainstDocumentInsideApply(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "# First")
	if _, err := joinedReplaceText(service, artifactID, "base", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("prepare document: %v", err)
	}
	// The write resolves inside the Apply of the room a browser edits; a write to a document no
	// room holds loads none (publishLiveWrite), so the room is loaded here.
	if err := service.warmLiveDocument(context.Background(), artifactID); err != nil {
		t.Fatalf("load the document's room: %v", err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	service.srv.OnInject = func(ctx context.Context, info ygws.InjectInfo) error {
		select {
		case <-entered:
		default:
			close(entered)
			<-release
		}
		return service.allowInject(ctx, info)
	}

	result := make(chan error, 1)
	go func() {
		_, err := joinedApplyOps(service, artifactID, []model.EditOp{{Op: "insert", Markdown: "!", After: "end"}}, model.Actor{Kind: "session", ID: "session-0123456789abcdef"}, nil)
		result <- err
	}()
	<-entered
	editLiveTree(t, service, artifactID, replaceRun("base", "base browser"))
	close(release)
	if err := <-result; err != nil {
		t.Fatalf("apply operation: %v", err)
	}
	waitForDocumentText(t, service, artifactID, "base browser\n\n!\n")
}

func TestApplyOpsInsertsAtHeadingsAndEdgesAndReplacesInlineText(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "# Title\n\nBody text.\n")
	_, err := joinedApplyOps(service, artifactID, []model.EditOp{
		{Op: "insert", Markdown: "Intro.", After: "heading:Title"},
		{Op: "replace", Find: "text.", With: "text. more"},
		{Op: "insert", Markdown: "- item", Before: "start"},
	}, model.Actor{Kind: "session", ID: "session-0123456789abcdef"}, nil)

	if err != nil {
		t.Fatal(err)
	}
	waitForDocumentText(t, service, artifactID, "- item\n\n# Title\n\nIntro.\n\nBody text. more\n")
}

// A replacement the document cannot hold where the match sits is an INVALID_OP naming the field
// the caller wrote, through the route, and the live document is left as it was: a list written
// over a heading's text has nowhere to stand (INFRA-1520).
func TestApplyOpsRejectsAReplaceTheDocumentCannotHold(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "# keep")
	_, err := joinedApplyOps(service, artifactID, []model.EditOp{{Op: "replace", Find: "keep", With: "- a\n- b"}}, model.Actor{Kind: "session", ID: "session-0123456789abcdef"}, nil)
	var invalid *ErrInvalidOp
	if !errors.As(err, &invalid) || invalid.Field != "with" || !strings.Contains(invalid.Reason, "which holds one line of text") {
		t.Fatalf("err = %v", err)
	}
	waitForDocumentText(t, service, artifactID, "# keep\n")
}

// Table rows inserted before a table's header cell, which the table cannot hold there, are an
// invalid insert naming its markdown, never an error the handler answers 500.
func TestApplyOperationRefusesTableRowsBeforeAHeaderCell(t *testing.T) {
	for _, test := range []struct{ spec, markdown string }{
		{"| a | b |\n| --- | --- |\n| c | d |\n", "| x | y |"},
		{"| a |\n| --- |\n| c |\n", "| a |\n| - |\n| b |"},
	} {
		tree, err := parseInput(test.spec)
		if err != nil {
			t.Fatal(err)
		}
		_, err = applyAlone(tree, model.EditOp{Op: "insert", Before: "a", Markdown: test.markdown})
		var invalid *ErrInvalidOp
		if !errors.As(err, &invalid) || invalid.Field != "markdown" {
			t.Fatalf("insert %q before a header cell: %v, want an invalid op on markdown", test.markdown, err)
		}
	}
}

func TestApplyOperationInsertsParagraphAfterTableContainingCellAnchor(t *testing.T) {
	tree, err := parseInput("| Key | Value |\n| --- | --- |\n| A10 | old |\n\nAfter.\n")
	if err != nil {
		t.Fatal(err)
	}
	next, err := applyAlone(tree, model.EditOp{
		Op:       "insert",
		After:    "A10",
		Markdown: "Inserted paragraph",
	})
	if err != nil {
		t.Fatal(err)
	}
	markdown, err := renderTree(next)
	if err != nil {
		t.Fatal(err)
	}
	const want = "| Key | Value |\n| --- | --- |\n| A10 | old |\n\nInserted paragraph\n\nAfter.\n"
	if markdown != want {
		t.Fatalf("paragraph after cell anchor = %q, want %q", markdown, want)
	}
}

// An insert reads the document back before and after it (pmdoc.RefuseMisreadWrite) once for each
// insert of a batch, so it reads the tables' spans unwritten: with a budget of span cells for each
// read-back, four inserts beside a table whose spans take the whole budget allocated some 1,580 MiB.
func TestApplyOperationsInsertsBesideASpannedTableSpendNoSpanBudgetEach(t *testing.T) {
	tree := &pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{
		{Type: "paragraph", Children: []*pmdoc.Node{{Type: "text", Text: "Before."}}},
		wholeBudgetTable(),
	}}
	pmdoc.EnsureBlockIDs(tree)
	ops := make([]model.EditOp, 4)
	for index := range ops {
		ops[index] = model.EditOp{Op: "insert", After: "end", Markdown: "New."}
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := applyOperations(tree, ops)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 64<<20 {
		t.Fatalf("%d inserts allocated %d MiB, want at most 64", len(ops), allocated>>20)
	}
}

// wholeBudgetTable is a table of 125 rows, each one cell holding `x` and spanning 801 columns, so
// its spans add all the empty cells one render writes for spans (100,000) and every row is as wide
// as the next.
func wholeBudgetTable() *pmdoc.Node {
	table := &pmdoc.Node{Type: "table"}
	for index := range 125 {
		kind, row := "table_cell", "table_row"
		if index == 0 {
			kind, row = "table_header", "table_header_row"
		}
		table.Children = append(table.Children, &pmdoc.Node{Type: row, Children: []*pmdoc.Node{{
			Type:     kind,
			Attrs:    pmdoc.Attrs{"alignment": nil, "colspan": 801, "colwidth": nil, "rowspan": 1},
			Children: []*pmdoc.Node{{Type: "paragraph", Children: []*pmdoc.Node{{Type: "text", Text: "x"}}}},
		}}})
	}
	return table
}

// An insert reads the document back before and after it with the tables' spans unwritten, so a
// stored row whose cell spans columns is written short and padded on each read-back. Those cells
// are the stored document's, not the batch's, so they spend none of the batch's budget: inserts
// beside a stored table spanning 5,940 cells, or 1,000 cells after six paragraph inserts, are
// stored, a small table among them included.
func TestApplyOperationsInsertsBesideAStoredSpanTableSpendNoBatchBudget(t *testing.T) {
	for _, test := range []struct {
		name              string
		width, rows       int
		paragraphsInserts int
	}{
		{name: "a header of 100 cells over 60 rows spanning it", width: 100, rows: 60, paragraphsInserts: 1},
		{name: "a header of 11 cells over 100 rows spanning it", width: 11, rows: 100, paragraphsInserts: 6},
	} {
		t.Run(test.name, func(t *testing.T) {
			tree := &pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{
				{Type: "paragraph", Children: []*pmdoc.Node{{Type: "text", Text: "Before."}}},
				spannedTable(test.width, test.rows),
			}}
			pmdoc.EnsureBlockIDs(tree)
			var ops []model.EditOp
			for range test.paragraphsInserts {
				ops = append(ops, model.EditOp{Op: "insert", After: "end", Markdown: "New."})
			}
			ops = append(ops, model.EditOp{Op: "insert", After: "end", Markdown: "| a | b |\n| - | - |\n| 1 | 2 |"})
			if _, err := applyOperations(tree, ops); err != nil {
				t.Fatalf("inserts beside the stored span table: %v, want them stored", err)
			}
		})
	}
}

// spannedTable is a table under a header of width cells, each of its rows one cell spanning the
// header.
func spannedTable(width, rows int) *pmdoc.Node {
	cell := func(kind string, colspan int) *pmdoc.Node {
		return &pmdoc.Node{
			Type:     kind,
			Attrs:    pmdoc.Attrs{"alignment": nil, "colspan": colspan, "colwidth": nil, "rowspan": 1},
			Children: []*pmdoc.Node{{Type: "paragraph", Children: []*pmdoc.Node{{Type: "text", Text: "x"}}}},
		}
	}
	header := &pmdoc.Node{Type: "table_header_row"}
	for range width {
		header.Children = append(header.Children, cell("table_header", 1))
	}
	table := &pmdoc.Node{Type: "table", Children: []*pmdoc.Node{header}}
	for range rows {
		table.Children = append(table.Children, &pmdoc.Node{Type: "table_row", Children: []*pmdoc.Node{cell("table_cell", width)}})
	}
	return table
}

// A table-row insert is the caller's markdown too, its rows padded to the table's width, so the
// rows each insert of a batch writes share the batch's budget: two inserts of 60 one-cell rows into
// a 100-column table, 5,940 cells each, are refused at the second, naming the limit.
func TestApplyOperationsChargesEveryTableRowInsertToTheBatch(t *testing.T) {
	tree, err := parseInput(strings.Repeat("| h ", 100) + "|\n" + strings.Repeat("| - ", 100) + "|\n| A10 " + strings.Repeat("| x ", 99) + "|\n")
	if err != nil {
		t.Fatal(err)
	}
	rows := strings.Repeat("| y |\n", 60)
	_, err = applyOperations(tree, []model.EditOp{
		{Op: "insert", After: "A10", Markdown: rows},
		{Op: "insert", After: "A10", Markdown: rows},
	})
	var invalid *ErrInvalidOp
	if !errors.As(err, &invalid) || invalid.Field != "markdown" || !strings.Contains(err.Error(), "operation 1") || !strings.Contains(err.Error(), "limit 10000") {
		t.Fatalf("two table-row inserts padding 11,880 cells: %v, want operation 1 refused on markdown naming the limit", err)
	}
}

func TestApplyOperationInsertsParagraphAfterParagraphContainingAnchor(t *testing.T) {
	tree, err := parseInput("Before anchor after.\n\nNext.\n")
	if err != nil {
		t.Fatal(err)
	}
	next, err := applyAlone(tree, model.EditOp{
		Op:       "insert",
		After:    "anchor",
		Markdown: "Inserted paragraph",
	})
	if err != nil {
		t.Fatal(err)
	}
	markdown, err := renderTree(next)
	if err != nil {
		t.Fatal(err)
	}
	const want = "Before anchor after.\n\nInserted paragraph\n\nNext.\n"
	if markdown != want {
		t.Fatalf("paragraph after in-paragraph anchor = %q, want %q", markdown, want)
	}
}

func TestApplyOperationInsertsInlineMarkdownAsOwnParagraph(t *testing.T) {
	tree, err := parseInput("Before anchor after.\n")
	if err != nil {
		t.Fatal(err)
	}
	next, err := applyAlone(tree, model.EditOp{
		Op:       "insert",
		After:    "anchor",
		Markdown: " **bold**",
	})
	if err != nil {
		t.Fatal(err)
	}
	markdown, err := renderTree(next)
	if err != nil {
		t.Fatal(err)
	}
	const want = "Before anchor after.\n\n**bold**\n"
	if markdown != want {
		t.Fatalf("inline markdown insert = %q, want %q", markdown, want)
	}
}

func TestApplyOperationExtendsTableAfterCellAnchor(t *testing.T) {
	tree, err := parseInput("| Key | Value |\n| --- | --- |\n| A10 | old |\n")
	if err != nil {
		t.Fatal(err)
	}
	next, err := applyAlone(tree, model.EditOp{
		Op:       "insert",
		After:    "A10",
		Markdown: "| A11 | new |",
	})
	if err != nil {
		t.Fatal(err)
	}
	markdown, err := renderTree(next)
	if err != nil {
		t.Fatal(err)
	}
	const want = "| Key | Value |\n| --- | --- |\n| A10 | old |\n| A11 | new |\n"
	if markdown != want {
		t.Fatalf("table-row insertion = %q, want %q", markdown, want)
	}
}

func TestApplyOperationRejectsFindTargetSpanningTextblocks(t *testing.T) {
	tree, err := parseInput("one\n\ntwo\n")
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range []model.EditOp{
		{Op: "replace", Find: "one two", With: "changed"},
		{Op: "delete", Find: "one two"},
	} {
		_, err := applyAlone(tree, op)
		if !errors.Is(err, pmdoc.ErrTargetSpansBlocks) {
			t.Fatalf("%s spanning textblocks error = %v, want ErrTargetSpansBlocks", op.Op, err)
		}
	}
}

func TestApplyOperationMatchesPlainTextInsideInlineCodeAndLinks(t *testing.T) {
	tests := []struct {
		name     string
		markdown string
		anchor   string
		want     string
	}{
		{
			name:     "inline code",
			markdown: "before `code anchor` after\n",
			anchor:   "code anchor",
			want:     "before updated after\n",
		},
		{
			name:     "link",
			markdown: "before [link anchor](https://example.com) after\n",
			anchor:   "link anchor",
			want:     "before updated after\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tree, err := parseInput(test.markdown)
			if err != nil {
				t.Fatal(err)
			}
			next, err := applyAlone(tree, model.EditOp{
				Op:   "replace",
				Find: test.anchor,
				With: "updated",
			})
			if err != nil {
				t.Fatal(err)
			}
			markdown, err := renderTree(next)
			if err != nil {
				t.Fatal(err)
			}
			if markdown != test.want {
				t.Fatalf("replace %q = %q, want %q", test.anchor, markdown, test.want)
			}
		})
	}
}

func TestApplyOperationExplainsRenderedQuoteMatching(t *testing.T) {
	tree, err := parseInput("Use `config` with care.\n\nUse it sparingly.\n\nUnrelated paragraph.\n\nAnother one.\n")
	if err != nil {
		t.Fatal(err)
	}
	_, err = applyOperations(tree, []model.EditOp{{
		Op:   "replace",
		Find: "Use `missing`",
		With: "updated",
	}})
	var missing *ErrQuoteNotFound
	if !errors.As(err, &missing) {
		t.Fatalf("rendered quote error = %v, want *ErrQuoteNotFound", err)
	}
	want := []string{"Use config with care.", "Use it sparingly.", "Unrelated paragraph."}
	if missing.Quote != "Use `missing`" || !reflect.DeepEqual(missing.Nearest, want) {
		t.Fatalf("rendered quote miss = %q, nearest %q; want the quote and %q", missing.Quote, missing.Nearest, want)
	}
}

const askFixture = ":::ask{#decision urgency=\"high\" multiple=\"false\" state=\"open\"}\nWhich transport?\n:::\n"

func TestApplyOperationDeletesABlockByID(t *testing.T) {
	tree, err := parseInput("# Title\n\n" + askFixture + "\n1. only\n\nAfter.\n")
	if err != nil {
		t.Fatal(err)
	}
	listID, _ := tree.Children[2].Attrs[pmdoc.BlockIDAttr].(string)
	next, err := applyOperations(tree, []model.EditOp{
		{Op: "delete", Block: "decision"},
		{Op: "delete", Block: listID},
	})
	if err != nil {
		t.Fatal(err)
	}
	markdown, err := renderTree(next.tree)
	if err != nil {
		t.Fatal(err)
	}
	if markdown != "# Title\n\nAfter.\n" {
		t.Fatalf("after block deletes = %q", markdown)
	}
	if _, err := applyAlone(tree, model.EditOp{Op: "delete", Block: "missing"}); !errors.Is(err, pmdoc.ErrTargetNotFound) {
		t.Fatalf("unknown block error = %v, want ErrTargetNotFound", err)
	}
	_, err = applyAlone(tree, model.EditOp{Op: "delete"})
	var invalid *ErrInvalidOp
	if !errors.As(err, &invalid) || invalid.Field != "find or block" {
		t.Fatalf("delete without a target error = %v, want invalid find or block", err)
	}
}

func TestApplyOperationsExplainsBlockCascadeWithinBatch(t *testing.T) {
	tree, err := parseInput("- Parent\n  - Child\n")
	if err != nil {
		t.Fatal(err)
	}
	parentID, _ := tree.Children[0].Attrs[pmdoc.BlockIDAttr].(string)
	childID, _ := tree.Children[0].Children[0].Attrs[pmdoc.BlockIDAttr].(string)
	valid, err := pmdoc.ValidateDocument(tree, "BlockDescendantIDs")
	if err != nil {
		t.Fatal(err)
	}
	removed, err := valid.BlockDescendantIDs(parentID)
	if err != nil {
		t.Fatalf("find delete cascade: %v", err)
	}
	foundChild := false
	for _, blockID := range removed {
		if blockID == childID {
			foundChild = true
			break
		}
	}
	if !foundChild {
		t.Fatalf("delete cascade targets = %q, want child %q", removed, childID)
	}
	_, err = applyOperations(tree, []model.EditOp{
		{Op: "delete", Block: parentID},
		{Op: "delete", Block: childID},
	})
	var invalid *ErrInvalidOp
	if !errors.As(err, &invalid) || invalid.Field != "block" ||
		!strings.Contains(invalid.Reason, `block "`+childID+`" was removed by operation 0 as a cascade of delete {block:"`+parentID+`"}`) {
		t.Fatalf("cascade batch error = %v", err)
	}
}

// LEGION-140: a delete whose match is a textblock's entire text removes the block; a list
// emptied of every item disappears; a partial match keeps the block with its remaining text.
func TestApplyOperationDeletingABlocksWholeTextRemovesTheBlock(t *testing.T) {
	const list = "# Acceptance\n\n- first criterion\n- second criterion\n- third criterion\n\n## Next\n"
	for _, test := range []struct {
		name     string
		markdown string
		ops      []model.EditOp
		want     string
	}{
		{
			name:     "whole item text removes the item",
			markdown: list,
			ops:      []model.EditOp{{Op: "delete", Find: "second criterion"}},
			want:     "# Acceptance\n\n- first criterion\n- third criterion\n\n## Next\n",
		},
		{
			name:     "every item removed removes the list",
			markdown: list,
			ops: []model.EditOp{
				{Op: "delete", Find: "first criterion"},
				{Op: "delete", Find: "second criterion"},
				{Op: "delete", Find: "third criterion"},
			},
			want: "# Acceptance\n\n## Next\n",
		},
		{
			name:     "partial text keeps the item",
			markdown: list,
			ops:      []model.EditOp{{Op: "delete", Find: " criterion", Occurrence: new(0)}},
			want:     "# Acceptance\n\n- first\n- second criterion\n- third criterion\n\n## Next\n",
		},
		{
			name:     "whole paragraph text removes the paragraph",
			markdown: "Keep.\n\nRemove me.\n\nAlso keep.\n",
			ops:      []model.EditOp{{Op: "delete", Find: "Remove me."}},
			want:     "Keep.\n\nAlso keep.\n",
		},
		{
			name:     "whole heading text removes the heading",
			markdown: "# Keep\n\n## Remove\n\nBody.\n",
			ops:      []model.EditOp{{Op: "delete", Find: "## Remove"}},
			want:     "# Keep\n\nBody.\n",
		},
		{
			name:     "table cell text is removed but the cell stays",
			markdown: "| Key | Value |\n| --- | --- |\n| A10 | old |\n",
			ops:      []model.EditOp{{Op: "delete", Find: "old"}},
			want:     "| Key | Value |\n| --- | --- |\n| A10 |  |\n",
		},
		{
			name:     "a parent bullet's nested list is hoisted into its place",
			markdown: "- Parent\n  - child one\n  - child two\n- Sibling\n",
			ops:      []model.EditOp{{Op: "delete", Find: "Parent"}},
			want:     "- child one\n- child two\n- Sibling\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			tree, err := parseInput(test.markdown)
			if err != nil {
				t.Fatal(err)
			}
			next, err := applyOperations(tree, test.ops)
			if err != nil {
				t.Fatal(err)
			}
			markdown, err := renderTree(next.tree)
			if err != nil {
				t.Fatal(err)
			}
			if markdown != test.want {
				t.Fatalf("after deletes = %q, want %q", markdown, test.want)
			}
		})
	}
}

func TestApplyOperationDeleteThatEmptiesATypedBlockNamesTheSchemaRule(t *testing.T) {
	tree, err := parseInput(askFixture)
	if err != nil {
		t.Fatal(err)
	}
	_, err = applyAlone(tree, model.EditOp{Op: "delete", Find: "Which transport?"})
	var invalid *ErrInvalidOp
	if !errors.As(err, &invalid) || invalid.Field != "find" || !strings.Contains(invalid.Reason, "typed block \"ask\"") {
		t.Fatalf("delete of an ask's only paragraph error = %v, want invalid find naming the ask content rule", err)
	}
}

func TestApplyOperationDeleteOfABulletWithOtherContentNamesTheItemBlock(t *testing.T) {
	tree, err := parseInput("- Parent\n\n  Extra paragraph.\n\n- Sibling\n")
	if err != nil {
		t.Fatal(err)
	}
	itemID, _ := tree.Children[0].Children[0].Attrs[pmdoc.BlockIDAttr].(string)
	_, err = applyAlone(tree, model.EditOp{Op: "delete", Find: "Parent"})
	var invalid *ErrInvalidOp
	if !errors.As(err, &invalid) || invalid.Field != "find" || !strings.Contains(invalid.Reason, `delete {block:"`+itemID+`"}`) {
		t.Fatalf("delete of a bullet with other content error = %v, want invalid find naming delete {block:%q}", err, itemID)
	}
	// The named escape hatch removes the item with its content.
	next, err := applyAlone(tree, model.EditOp{Op: "delete", Block: itemID})
	if err != nil {
		t.Fatal(err)
	}
	markdown, err := renderTree(next)
	if err != nil {
		t.Fatal(err)
	}
	if markdown != "- Sibling\n" {
		t.Fatalf("after deleting the item by id = %q", markdown)
	}
}

func TestApplyOperationMovesABlockToAnAnchor(t *testing.T) {
	const contextMarkdown = "# Context\n\nThe context ends with no drift.\n\n## 4\\. Design\n\nDesign body.\n"
	tree, err := parseInput(askFixture + "\n" + contextMarkdown)
	if err != nil {
		t.Fatal(err)
	}
	designID, _ := tree.Children[3].Attrs[pmdoc.BlockIDAttr].(string)
	for _, test := range []struct {
		name string
		op   model.EditOp
		want string
	}{
		{
			name: "after a quote",
			op:   model.EditOp{Op: "move", Block: "decision", After: "no drift."},
			want: "# Context\n\nThe context ends with no drift.\n\n" + askFixture + "\n## 4\\. Design\n\nDesign body.\n",
		},
		{
			name: "before a heading",
			op:   model.EditOp{Op: "move", Block: "decision", Before: "heading:4. Design"},
			want: "# Context\n\nThe context ends with no drift.\n\n" + askFixture + "\n## 4\\. Design\n\nDesign body.\n",
		},
		{
			name: "after a block id",
			op:   model.EditOp{Op: "move", Block: "decision", After: "block:" + designID},
			want: "# Context\n\nThe context ends with no drift.\n\n## 4\\. Design\n\n" + askFixture + "\nDesign body.\n",
		},
		{
			name: "to the end",
			op:   model.EditOp{Op: "move", Block: "decision", After: "end"},
			want: contextMarkdown + "\n" + askFixture,
		},
		{
			name: "insert also accepts a block id anchor",
			op:   model.EditOp{Op: "insert", Markdown: "Inserted.", Before: "block:" + designID},
			want: askFixture + "\n# Context\n\nThe context ends with no drift.\n\nInserted.\n\n## 4\\. Design\n\nDesign body.\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			next, err := applyAlone(tree, test.op)
			if err != nil {
				t.Fatal(err)
			}
			markdown, err := renderTree(next)
			if err != nil {
				t.Fatal(err)
			}
			if markdown != test.want {
				t.Fatalf("after %s = %q, want %q", test.name, markdown, test.want)
			}
		})
	}
}

func TestApplyOperationMoveRejectsMalformedAndSelfAnchoredMoves(t *testing.T) {
	tree, err := parseInput(askFixture + "\nAfter.\n")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		op    model.EditOp
		field string
	}{
		{name: "no block", op: model.EditOp{Op: "move", After: "After."}, field: "block"},
		{name: "no anchor", op: model.EditOp{Op: "move", Block: "decision"}, field: "after or before"},
		{name: "both anchors", op: model.EditOp{Op: "move", Block: "decision", After: "After.", Before: "start"}, field: "after or before"},
		{name: "quote inside the moved block", op: model.EditOp{Op: "move", Block: "decision", After: "transport"}, field: "after"},
		{name: "anchored to itself", op: model.EditOp{Op: "move", Block: "decision", Before: "block:decision"}, field: "before"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := applyAlone(tree, test.op)
			var invalid *ErrInvalidOp
			if !errors.As(err, &invalid) || invalid.Field != test.field {
				t.Fatalf("move error = %v, want invalid %s", err, test.field)
			}
		})
	}
	if _, err := applyAlone(tree, model.EditOp{Op: "move", Block: "missing", After: "After."}); !errors.Is(err, pmdoc.ErrTargetNotFound) {
		t.Fatalf("unknown block error = %v, want ErrTargetNotFound", err)
	}
}

func TestApplyOperationRetypesATypedBlock(t *testing.T) {
	tree, err := parseInput(askFixture)
	if err != nil {
		t.Fatal(err)
	}
	next, err := applyAlone(tree, model.EditOp{Op: "retype", Block: "decision", Type: "callout", Attributes: map[string]any{"kind": "warning"}})
	if err != nil {
		t.Fatalf("retype ask block: %v", err)
	}
	markdown, err := renderTree(next)
	if err != nil {
		t.Fatal(err)
	}
	if markdown != ":::callout{#decision kind=\"warning\" title=\"\"}\nWhich transport?\n:::\n" {
		t.Fatalf("retyped ask = %q", markdown)
	}

	heading, err := parseInput("# Heading\n")
	if err != nil {
		t.Fatal(err)
	}
	headingID, _ := heading.Children[0].Attrs[pmdoc.BlockIDAttr].(string)
	_, err = applyAlone(heading, model.EditOp{Op: "retype", Block: headingID, Type: "callout"})
	var invalid *ErrInvalidOp
	if !errors.As(err, &invalid) || invalid.Field != "block" {
		t.Fatalf("retype heading error = %v, want invalid block", err)
	}
}

// A `replace` writes the blocks its text names (INFRA-1520): `with` is read by the block parser
// an insert's markdown is read by, and the result is fitted into the match by the rules the
// editor a person reads the document in applies. A list written over a paragraph's whole text
// stands in that paragraph's place; one written under a line of text inside a list item stands
// inside that item, and the items around it keep their numbers.
func TestApplyOperationReplaceWritesTheBlocksItsTextNames(t *testing.T) {
	for _, test := range []struct{ name, markdown, find, with, want string }{
		{
			name:     "a paragraph and a list over a paragraph",
			markdown: "Intro.\n\nBody.\n\nAfter.\n",
			find:     "Body.",
			with:     "Here is the plan:\n\n- first\n- second",
			want:     "Intro.\n\nHere is the plan:\n\n- first\n- second\n\nAfter.\n",
		},
		{
			name:     "a list in the paragraph's place",
			markdown: "Intro.\n\nBody.\n\nAfter.\n",
			find:     "Body.",
			with:     "- first\n- second",
			want:     "Intro.\n\n- first\n- second\n\nAfter.\n",
		},
		{
			name:     "a list inside the item the text names",
			markdown: "- a\n\n  - x\n",
			find:     "x",
			with:     "x\n- y",
			want:     "- a\n\n  - x\n    - y\n",
		},
		{
			name:     "a list inside the first item of an ordered list",
			markdown: "1. one\n2. two\n",
			find:     "one",
			with:     "one\n- y",
			want:     "1. one\n   - y\n2. two\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			tree, err := parseInput(test.markdown)
			if err != nil {
				t.Fatal(err)
			}
			next, err := applyAlone(tree, model.EditOp{Op: "replace", Find: test.find, With: test.with})
			if err != nil {
				t.Fatalf("replace with %q: %v", test.with, err)
			}
			markdown, err := renderTree(next)
			if err != nil {
				t.Fatal(err)
			}
			if markdown != test.want {
				t.Fatalf("replace with %q = %q, want %q", test.with, markdown, test.want)
			}
		})
	}
}

// The markdown of `with` means what markdown means, so each of these kinds of block is written
// where today it is text or a refusal. Front matter opens the document's start alone, as it does
// for an insert: the same three lines over the first paragraph's text write front matter, and
// over a later paragraph's text a rule and the heading its second line underlines.
func TestApplyOperationReplaceWritesEachKindOfBlockItsMarkdownNames(t *testing.T) {
	const document = "Intro.\n\nBody.\n\nAfter.\n"
	for _, test := range []struct{ name, markdown, find, with, want string }{
		{name: "a rule", markdown: document, find: "Body.", with: "---", want: "Intro.\n\n---\n\nAfter.\n"},
		{name: "a heading", markdown: document, find: "Body.", with: "# Title", want: "Intro.\n\n# Title\n\nAfter.\n"},
		{name: "a quotation", markdown: document, find: "Body.", with: "> quoted", want: "Intro.\n\n> quoted\n\nAfter.\n"},
		{
			name:     "a table",
			markdown: document,
			find:     "Body.",
			with:     "| a | b |\n| --- | --- |\n| c | d |",
			want:     "Intro.\n\n| a | b |\n| --- | --- |\n| c | d |\n\nAfter.\n",
		},
		{
			name:     "a code block, from a line indented four spaces",
			markdown: document,
			find:     "Body.",
			with:     "    - Not a bullet",
			want:     "Intro.\n\n```\n- Not a bullet\n```\n\nAfter.\n",
		},
		{
			name:     "a heading of level 2, from the line of dashes under its text",
			markdown: document,
			find:     "Body.",
			with:     "Title\n---",
			want:     "Intro.\n\n## Title\n\nAfter.\n",
		},
		{
			name:     "front matter over the first paragraph's text",
			markdown: "Intro.\n\nBody.\n",
			find:     "Intro.",
			with:     "---\ntitle: T\n---",
			want:     "---\ntitle: T\n---\n\nBody.\n",
		},
		{
			name:     "the same three lines past the first paragraph",
			markdown: "Intro.\n\nBody.\n",
			find:     "Body.",
			with:     "---\ntitle: T\n---",
			want:     "Intro.\n\n---\n\n## title: T\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			tree, err := parseInput(test.markdown)
			if err != nil {
				t.Fatal(err)
			}
			next, err := applyAlone(tree, model.EditOp{Op: "replace", Find: test.find, With: test.with})
			if err != nil {
				t.Fatalf("replace with %q: %v", test.with, err)
			}
			markdown, err := renderTree(next)
			if err != nil {
				t.Fatal(err)
			}
			if markdown != test.want {
				t.Fatalf("replace with %q = %q, want %q", test.with, markdown, test.want)
			}
		})
	}
}

// A `with` that parses to one paragraph names no block of its own and keeps the inline path,
// where it continues the text around the match: marked text is marked, a soft break renders as a
// space, and prose written with a backslash before a marker is still the characters.
func TestApplyOperationReplaceOfOneParagraphStaysInline(t *testing.T) {
	const document = "Intro.\n\nBody.\n\nAfter.\n"
	for _, test := range []struct{ name, markdown, find, with, want string }{
		{name: "a bold run", markdown: document, find: "Body.", with: "**bold**", want: "Intro.\n\n**bold**\n\nAfter.\n"},
		{
			name:     "an escaped bullet marker",
			markdown: document,
			find:     "Body.",
			with:     "\\- not a bullet",
			want:     "Intro.\n\n\\- not a bullet\n\nAfter.\n",
		},
		{
			name:     "an escaped heading marker",
			markdown: document,
			find:     "Body.",
			with:     "\\# not a heading",
			want:     "Intro.\n\n\\# not a heading\n\nAfter.\n",
		},
		{name: "inline markup stays markup", markdown: "Body.\n", find: "Body.", with: "**bold** `code`", want: "**bold** `code`\n"},
		{name: "a soft break joins as a space", markdown: "Body.\n", find: "Body.", with: "one\ntwo", want: "one two\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			tree, err := parseInput(test.markdown)
			if err != nil {
				t.Fatal(err)
			}
			next, err := applyAlone(tree, model.EditOp{Op: "replace", Find: test.find, With: test.with})
			if err != nil {
				t.Fatalf("replace with %q: %v", test.with, err)
			}
			markdown, err := renderTree(next)
			if err != nil {
				t.Fatal(err)
			}
			if markdown != test.want {
				t.Fatalf("replace with %q = %q, want %q", test.with, markdown, test.want)
			}
		})
	}
}

// A `with` the caller wrote that renders to nothing is refused, since splicing nothing over the
// match would delete their text and report the batch applied. Whitespace alone is the whole of
// that text now: a line indented four spaces or a tab is a code block, which the replacement
// writes. An empty `with` is the only one that deletes on purpose. This is LEGION-280.
func TestApplyOperationReplaceRefusesAWithThatRendersToNoText(t *testing.T) {
	for _, test := range []struct{ name, with string }{
		{name: "one space", with: " "},
		{name: "one tab", with: "\t"},
		{name: "one newline", with: "\n"},
		{name: "mixed whitespace", with: "  \t  "},
	} {
		t.Run(test.name, func(t *testing.T) {
			tree, err := parseInput("Body.\n")
			if err != nil {
				t.Fatal(err)
			}
			_, err = applyAlone(tree, model.EditOp{Op: "replace", Find: "Body.", With: test.with})
			var invalid *ErrInvalidOp
			if !errors.As(err, &invalid) || invalid.Field != "with" {
				t.Fatalf("replace with %q = %v, want invalid with rather than a silent deletion", test.with, err)
			}
			t.Logf("refusal: %s", invalid.Reason)
			if !strings.Contains(invalid.Reason, "whitespace alone") {
				t.Fatalf("reason = %q, want it to name the whitespace", invalid.Reason)
			}
			if markdown, err := renderTree(tree); err != nil || markdown != "Body.\n" {
				t.Fatalf("the document reads %q (%v), want the matched text kept", markdown, err)
			}
		})
	}
}

// A line after a hard break opens a block only where the parser reads a block there: an ordered
// item may interrupt a paragraph only when it is numbered a lone `1`, as the browser editor's
// parser reads it, so `2024. was a year` after a break stays prose, and so do a zero-padded one
// (`01.`, `001)`, `000000001.`), a zero-padded two (`02.`) and a `1` the digit run continues past
// (`10.`); and marked text opens with its mark's delimiter, not the marker character. Each of
// these stays one paragraph, and its canonical markdown still reads back as the document it was
// rendered from.
func TestApplyOperationReplaceKeepsAHardBreakThatOpensNoBlock(t *testing.T) {
	for _, test := range []struct{ name, with string }{
		{name: "a hard break into plain text", with: "Body.  \ntwo"},
		{name: "a hard break into an ordered marker that is not one", with: "Body.  \n4. was a year"},
		{name: "a hard break into a zero-padded two", with: "Body.  \n02. was a year"},
		{name: "a hard break into a ten", with: "Body.  \n10. items"},
		{name: "a hard break into a zero-padded one", with: "Body.  \n01. item"},
		{name: "a hard break into a twice-padded paren", with: "Body.  \n001) x"},
		{name: "a hard break into the longest zero-padded one", with: "Body.  \n000000001. item"},
		{name: "a hard break into a zero run past the digit cap", with: "Body.  \n0000000001. items"},
		{name: "a hard break into marked text", with: "Body.  \n**- bold**"},
	} {
		t.Run(test.name, func(t *testing.T) {
			tree, err := parseInput("Body.\n")
			if err != nil {
				t.Fatal(err)
			}
			next, err := applyAlone(tree, model.EditOp{Op: "replace", Find: "Body.", With: test.with})
			if err != nil {
				t.Fatalf("replace with %q: %v", test.with, err)
			}
			if len(next.Children) != 1 || next.Children[0].Type != "paragraph" {
				t.Fatalf("replace with %q wrote %v, want the one paragraph the markdown names", test.with, blockTypes(next))
			}
			markdown, err := renderTree(next)
			if err != nil {
				t.Fatal(err)
			}
			reparsed, err := parseInput(markdown)
			if err != nil {
				t.Fatalf("canonical markdown %q does not parse back: %v", markdown, err)
			}
			if !reparsed.Equal(next) {
				t.Fatalf("canonical markdown %q reads back as a different document: the %q replacement changed the document's shape", markdown, test.with)
			}
			settled, err := renderTree(reparsed)
			if err != nil {
				t.Fatal(err)
			}
			again, err := parseInput(settled)
			if err != nil {
				t.Fatalf("settled markdown %q does not parse back: %v", settled, err)
			}
			if !again.Equal(reparsed) {
				t.Fatalf("markdown %q is not a fixed point: it reads back as a different document", settled)
			}
		})
	}
}

// An empty `with` still deletes the matched span on purpose.
func TestApplyOperationReplaceWithNothingStillDeletesTheMatch(t *testing.T) {
	tree, err := parseInput("Keep this.\n")
	if err != nil {
		t.Fatal(err)
	}
	next, err := applyAlone(tree, model.EditOp{Op: "replace", Find: "this", With: ""})
	if err != nil {
		t.Fatalf("empty replacement: %v", err)
	}
	markdown, err := renderTree(next)
	if err != nil {
		t.Fatal(err)
	}
	if markdown != "Keep .\n" {
		t.Fatalf("empty replacement = %q, want %q", markdown, "Keep .\n")
	}
}

// blockTypes is every non-text node type in document order, the shape a round trip must preserve.
func blockTypes(tree *pmdoc.Node) []string {
	var types []string
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type != "text" {
			types = append(types, node.Type)
		}
		return true
	})
	return types
}

// TESTER-AUTHORED (Accept1326, LEGION-260 acceptance item 3): "a heading: anchor, quote anchor or
// target that doesn't resolve returns an error naming the operation's index, the anchor text, and
// the nearest existing headings ... or blocks. It never names an internal package."
// resolveEditQuote only dresses pmdoc.ErrQuoteNotFound; a quote that resolves ambiguously
// (ErrTargetAmbiguous) or across two textblocks (ErrTargetSpansBlocks) falls through raw, so an
// agent is told "pmdoc: target is ambiguous" with no operation index and no quote to act on.
func TestApplyOperationsUnresolvedQuoteErrorsNameTheQuoteNotThePackage(t *testing.T) {
	for _, test := range []struct {
		name, markdown, find string
	}{
		{name: "quote matching two blocks", markdown: "Shared line.\n\nShared line.\n", find: "Shared line."},
		{name: "quote spanning two blocks", markdown: "## Alpha\n\nBody.\n", find: "Alpha Body."},
	} {
		t.Run(test.name, func(t *testing.T) {
			tree, err := parseInput(test.markdown)
			if err != nil {
				t.Fatal(err)
			}
			_, err = applyOperations(tree, []model.EditOp{{Op: "replace", Find: test.find, With: "z"}})
			if err == nil {
				t.Fatalf("replace find=%q resolved, want a refusal", test.find)
			}
			message := err.Error()
			if strings.Contains(message, "pmdoc") {
				t.Errorf("error %q names the internal package", message)
			}
			if !strings.Contains(message, "operation 0") {
				t.Errorf("error %q does not name the operation index", message)
			}
			if !strings.Contains(message, test.find) {
				t.Errorf("error %q does not name the quote %q", message, test.find)
			}
		})
	}
}

// Whether HTML opens a block depends on where the replacement lands, so the refusal is decided on
// the document the replace produces: a replacement whose markdown the parser then refuses is
// refused. These land where HTML starts a block - the whole of a paragraph, a list item's start,
// the line after a hard break.
func TestApplyOperationReplaceRefusesHTMLThatOpensABlockWhereItLands(t *testing.T) {
	for _, test := range []struct{ document, find, with string }{
		{document: "Intro.\n\nBody.\n", find: "Body.", with: "<div>x</div>"},
		{document: "Intro.\n\nBody.\n", find: "Body.", with: "<!-- note -->"},
		{document: "Intro.\n\nBody.\n", find: "Body.", with: "<br>"},
		{document: "- Body.\n", find: "Body.", with: "<div>x</div>"},
		{document: "Intro.\n\nfoo Body. bar\n", find: "Body.", with: "x  \n<div>y</div>"},
	} {
		t.Run(test.document+test.with, func(t *testing.T) {
			tree, err := parseInput(test.document)
			if err != nil {
				t.Fatal(err)
			}
			_, err = applyAlone(tree, model.EditOp{Op: "replace", Find: test.find, With: test.with})
			var invalid *ErrInvalidOp
			if !errors.As(err, &invalid) || invalid.Field != "with" {
				t.Fatalf("replace = %v, want invalid with", err)
			}
			if !strings.Contains(invalid.Reason, "block HTML") {
				t.Fatalf("refusal = %q, want it to name the parser's reason", invalid.Reason)
			}
		})
	}
}

// The positive control for that refusal: the same HTML where it opens no block - inside a line, in
// a table cell, in a heading - is kept, and the document reads back as written.
func TestApplyOperationReplaceKeepsHTMLThatOpensNoBlockWhereItLands(t *testing.T) {
	const paragraph, cell, heading = "Intro.\n\nfoo Body. bar\n", "| h |\n| --- |\n| Body. |\n", "# Body.\n"
	for _, test := range []struct{ document, with string }{
		{document: "Intro.\n\nBody.\n", with: "before <b>x</b> after"},
		{document: "Intro.\n\nBody.\n", with: `<span class="x">text</span>`},
		{document: "Intro.\n\nBody.\n", with: "<br>after"},
		{document: paragraph, with: "<div>x</div>"},
		{document: paragraph, with: "<br>"},
		{document: paragraph, with: "<br/>"},
		{document: paragraph, with: "<img src=x>"},
		{document: paragraph, with: `<img src="i.png" width="16">`},
		{document: paragraph, with: "<!-- c --> tail"},
		{document: paragraph, with: "<b>x</b>\n<div>y</div>"},
		{document: cell, with: "<ul><li>a</li><li>b</li></ul>"},
		{document: cell, with: "<br>"},
		{document: cell, with: "<img src=x>"},
		{document: cell, with: "<div>x</div>"},
		{document: cell, with: "<!-- c --> tail"},
		{document: heading, with: "<br>"},
		{document: heading, with: "<img src=x>"},
		{document: heading, with: "<div>x</div>"},
		{document: heading, with: "<!-- c --> tail"},
	} {
		t.Run(test.document+test.with, func(t *testing.T) {
			tree, err := parseInput(test.document)
			if err != nil {
				t.Fatal(err)
			}
			tree, err = applyAlone(tree, model.EditOp{Op: "replace", Find: "Body.", With: test.with})
			if err != nil {
				t.Fatalf("replace: %v", err)
			}
			markdown, err := renderTree(tree)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(test.with, "\n") && !strings.Contains(markdown, test.with) {
				t.Fatalf("%q does not hold the HTML as written, %q", markdown, test.with)
			}
			back, err := parseInput(markdown)
			if err != nil {
				t.Fatalf("%q no longer parses: %v", markdown, err)
			}
			if again, err := renderTree(back); err != nil || again != markdown {
				t.Fatalf("%q reads back as %q (%v)", markdown, again, err)
			}
		})
	}
}

// The id stays the caller's to keep when the block that had it goes first: a delete earlier in
// the batch frees it, so a typed block can be put back in its own place under its own id.
func TestApplyOperationsInsertKeepsAnIDADeleteEarlierInTheBatchFreed(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	tree, err := parseInput("Before.\n\n:::ask{#" + id + "}\nQuestion?\n:::\n\nAfter.\n")
	if err != nil {
		t.Fatal(err)
	}
	batch, err := applyOperations(tree, []model.EditOp{
		{Op: "delete", Block: id},
		{Op: "insert", Markdown: ":::ask{#" + id + "}\nReworded?\n:::", After: "Before."},
	})
	if err != nil {
		t.Fatalf("delete then insert: %v", err)
	}
	carriers := 0
	for _, child := range batch.tree.Children {
		if blockID, _ := child.Attrs[pmdoc.BlockIDAttr].(string); blockID == id {
			carriers++
		}
	}
	if carriers != 1 {
		t.Fatalf("%d blocks carry %q, want the reinserted one", carriers, id)
	}
}

// A `with` of more than one paragraph is written whole. Before INFRA-1520 the inline parser
// could not hold it and the operation was refused, since writing what the parser read would have
// dropped the indented code block after the first paragraph, and every paragraph after it, while
// the batch reported itself changed. Each paragraph and each code block is now written.
func TestApplyOperationReplaceWritesEveryParagraphOfItsWith(t *testing.T) {
	for _, test := range []struct{ with, want string }{
		{with: "Keep this.\n\n    dropped code", want: "Intro.\n\nKeep this.\n\n```\ndropped code\n```\n"},
		{with: "one\n\n    code line\n\ntwo", want: "Intro.\n\none\n\n```\ncode line\n```\n\ntwo\n"},
		// A skipped line of whitespace the parser keeps is the code block's own text.
		{with: "one\n\n    \u00a0\n\ntwo", want: "Intro.\n\none\n\n```\n\u00a0\n```\n\ntwo\n"},
		{with: "one\n\n    \f\n\ntwo", want: "Intro.\n\none\n\n```\n\f\n```\n\ntwo\n"},
	} {
		t.Run(test.with, func(t *testing.T) {
			tree, err := parseInput("Intro.\n\nBody.\n")
			if err != nil {
				t.Fatal(err)
			}
			next, err := applyAlone(tree, model.EditOp{Op: "replace", Find: "Body.", With: test.with})
			if err != nil {
				t.Fatalf("replace with %q: %v", test.with, err)
			}
			markdown, err := renderTree(next)
			if err != nil {
				t.Fatal(err)
			}
			if markdown != test.want {
				t.Fatalf("replace with %q = %q, want %q", test.with, markdown, test.want)
			}
		})
	}
}

// A replace continues the text around it with the spaces and tabs at the edges of its `with`,
// which parsing strips: once, outside any mark, and without the line breaks, which an inline
// replacement cannot carry. Getting any of these wrong writes something else - a bold that no
// longer reads as bold, a code span or link whose text gained the space, a no-break space written
// twice, a blank line that split the paragraph.
func TestApplyOperationReplaceKeepsItsEdgeWhitespaceOnceOutsideTheMarks(t *testing.T) {
	for _, test := range []struct{ with, want string }{
		{with: " **x**", want: "foo  **x** bar"},
		{with: "**x** ", want: "foo **x**  bar"},
		{with: " `c` ", want: "foo  `c`  bar"},
		{with: " [l](https://x.test) ", want: "foo  [l](https://x.test)  bar"},
		{with: " <b>x</b>", want: "foo  <b>x</b> bar"},
		{with: " ![a](u)", want: "foo  ![a](u) bar"},
		{with: "\u00a0x", want: "foo \u00a0x bar"},
		{with: "x\u3000", want: "foo x\u3000 bar"},
		{with: "x\n\n", want: "foo x bar"},
		{with: "\n x", want: "foo  x bar"},
	} {
		t.Run(test.with, func(t *testing.T) {
			tree, err := parseInput("Intro.\n\nfoo Body. bar\n")
			if err != nil {
				t.Fatal(err)
			}
			out, err := applyAlone(tree, model.EditOp{Op: "replace", Find: "Body.", With: test.with})
			if err != nil {
				t.Fatal(err)
			}
			markdown, err := pmdoc.Render(out)
			if err != nil {
				t.Fatal(err)
			}
			if want := "Intro.\n\n" + test.want + "\n"; markdown != want {
				t.Fatalf("replace with %q wrote %q, want %q", test.with, markdown, want)
			}
		})
	}
}

// A heading and a table cell are each written on one line, so a hard break replaced into one, or a
// line break inside a code span or inline HTML, ends the block there: the heading reads back as a
// heading and a paragraph, the cell's row as two rows. In a paragraph or a list item they are kept.
func TestApplyOperationReplaceRefusesAHardBreakInAHeadingOrTableCell(t *testing.T) {
	for _, test := range []struct {
		document string
		refused  bool
	}{
		{document: "# Body.\n", refused: true},
		{document: "| h |\n| --- |\n| Body. |\n", refused: true},
		{document: "| Body. |\n| --- |\n| v |\n", refused: true},
		{document: "Intro.\n\nBody.\n", refused: false},
		{document: "- Body.\n", refused: false},
		{document: "> Body.\n", refused: false},
	} {
		for _, with := range []string{"x  \ny", "x\\\ny", "`x\ny`", "<span\nclass=\"x\">y</span>"} {
			t.Run(test.document+with, func(t *testing.T) {
				tree, err := parseInput(test.document)
				if err != nil {
					t.Fatal(err)
				}
				_, err = applyAlone(tree, model.EditOp{Op: "replace", Find: "Body.", With: with})
				var invalid *ErrInvalidOp
				refused := errors.As(err, &invalid) && invalid.Field == "with"
				if refused != test.refused || (!refused && err != nil) {
					t.Fatalf("replace = %v, want refused %v", err, test.refused)
				}
			})
		}
	}
}

// A replace's `with` is read as a paragraph's text, where `\|` in code is the code's own text, and a
// delete can cut code down to a backslash before a `|`; in a table cell no markdown writes such code
// (pmdoc.TableCodePipes), so the write is refused, naming the operation's field, the table, the
// cell's row and column, the header above it, and what to write instead. Code holding a `|` with no
// backslash or two before it, and text holding a backslash before a `|`, are stored as written. A
// table a browser edit already left such code in still takes writes, the one that rewrites that
// code among them, but not one that adds such code beside it, the same code included.
func TestApplyOperationRefusesCodeWithAPipeAfterABackslashInATableCell(t *testing.T) {
	const table = "| Cmd | Note |\n| --- | --- |\n| run | x |\n"
	replace := func(find, with string) model.EditOp { return model.EditOp{Op: "replace", Find: find, With: with} }
	for _, test := range []struct {
		name, document string
		// browser puts code holding `a\|b` in the body row's first cell first, as a browser edit can.
		browser bool
		op      model.EditOp
		// field is the field a refusal names, "" when the write is stored; says is what the refusal
		// says after naming the table, and stored a line of the markdown stored.
		field, says, stored string
	}{
		{name: "code holding a backslash before a pipe replaced into a body cell", document: table, op: replace("x", "`git log \\| wc -l`"),
			field: "with", says: `holds code in row 1, column 1 (under the header "Note") whose text has a | right after a backslash, "git log \| wc -l"`},
		{name: "code holding a backslash before a pipe replaced into a header cell", document: table, op: replace("Note", "`a\\|b`"),
			field: "with", says: `holds code in row 0, column 1 (the header row)`},
		{name: "a delete that leaves code holding a backslash before a pipe", document: "| Cmd |\n| --- |\n| `a\\\\\\|b` |\n", op: model.EditOp{Op: "delete", Find: "a\\"},
			field: "find", says: `holds code in row 1, column 0 (under the header "Cmd") whose text has a | right after a backslash, "\|b"`},
		{name: "code holding a pipe", document: table, op: replace("x", "`a|b`"), stored: "| run | `a\\|b` |"},
		{name: "code holding a pipe after two backslashes", document: table, op: replace("x", "`a\\\\|b`"), stored: "| run | `a\\\\\\|b` |"},
		{name: "text holding a backslash before a pipe", document: table, op: replace("x", "a\\\\\\|b"), stored: "| run | a\\\\\\|b |"},
		{name: "a replace beside code a browser edit left", document: table, browser: true, op: replace("x", "y"), stored: "| `a\\`\\|`b` | y |"},
		{name: "the rewrite of code a browser edit left", document: table, browser: true, op: replace("a\\|b", "`a|b`"), stored: "| `a\\|b` | x |"},
		{name: "a code pipe replaced in beside code a browser edit left", document: table, browser: true, op: replace("x", "`a\\|b`"),
			field: "with", says: `holds code in row 1, column 1 (under the header "Note") whose text has a | right after a backslash, "a\|b"`},
		{name: "a delete that leaves a code pipe beside code a browser edit left", document: "| Cmd | Note |\n| --- | --- |\n| run | `c\\\\\\|d` |\n", browser: true,
			op: model.EditOp{Op: "delete", Find: "c\\"}, field: "find", says: `holds code in row 1, column 1 (under the header "Note") whose text has a | right after a backslash, "\|d"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			tree, err := parseInput(test.document)
			if err != nil {
				t.Fatal(err)
			}
			if test.browser {
				tree.Children[0].Children[1].Children[0].Children[0].Children = []*pmdoc.Node{{Type: "text", Text: "a\\|b", Marks: []pmdoc.Mark{{Type: "inlineCode"}}}}
			}
			out, err := applyAlone(tree, test.op)
			if test.field == "" {
				if err != nil {
					t.Fatalf("%s = %v, want it stored", test.op.Op, err)
				}
				markdown, err := renderTree(out)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(markdown, test.stored+"\n") {
					t.Fatalf("%s stored %q, want the line %q", test.op.Op, markdown, test.stored)
				}
				return
			}
			var invalid *ErrInvalidOp
			if !errors.As(err, &invalid) || invalid.Field != test.field {
				t.Fatalf("%s = %v, want an invalid op on %s", test.op.Op, err, test.field)
			}
			for _, says := range []string{fmt.Sprintf("table %q %s", blockID(tree.Children[0]), test.says), "drop the backslash before the |, or end the code before the | and write the | as text"} {
				if !strings.Contains(invalid.Reason, says) {
					t.Errorf("%s refused with %q, want it to say %q", test.op.Op, invalid.Reason, says)
				}
			}
		})
	}
}

// A browser edit can leave code holding a `|` after a backslash in a table cell, which no markdown of
// a cell writes and a caller's write is refused for
// (TestApplyOperationRefusesCodeWithAPipeAfterABackslashInATableCell). Settlement versions a browser
// edit as its rendering, as it does every tree whose markdown reads back otherwise, so the `|` is
// written as text after the code: the version reads back as the table it was, each row as wide and
// every character in its cell, never as the `\\|` that ends the cell between the backslash and the
// `|`.
func TestSettlementVersionsCodeWithAPipeAfterABackslashInATableCellWithThePipeAfterTheCode(t *testing.T) {
	for _, test := range []struct{ name, code, row string }{
		{"one backslash", "a\\|b", "| run | `a\\`\\|`b` |"},
		{"a grep alternation's code", "grep -n 'one\\|two' notes.txt", "| run | `grep -n 'one\\`\\|`two' notes.txt` |"},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			service.settle = time.Hour
			seedServiceText(t, service, artifactID, "| Cmd | Count |\n| --- | --- |\n| run | n |\n")
			editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
				tree.Children[0].Children[1].Children[1].Children[0].Children = []*pmdoc.Node{{Type: "text", Text: test.code, Marks: []pmdoc.Mark{{Type: "inlineCode"}}}}
				return tree
			})
			settleCurrentGeneration(t, service, artifactID)
			markdown, _ := latestVersionOf(t, service.store, artifactID)
			if want := "| Cmd | Count |\n| --- | --- |\n" + test.row + "\n"; markdown != want {
				t.Fatalf("the settled version is %q, want %q", markdown, want)
			}
			back, err := pmdoc.Parse(markdown)
			if err != nil {
				t.Fatal(err)
			}
			for index, row := range back.Children[0].Children {
				if len(row.Children) != 2 {
					t.Fatalf("the version reads back with row %d holding %d cells, want 2", index, len(row.Children))
				}
			}
			if got := pmdoc.TextContent(back.Children[0].Children[1].Children[1]); got != test.code {
				t.Fatalf("the version's cell reads back holding %q, want %q", got, test.code)
			}
		})
	}
}

// Whitespace at the edges of a replacement continues the text around it; at the start or the end
// of the textblock it lands in there is no text to continue, and whitespace kept there would be
// stripped on the next read - or, after a footnote's marker, open indented code.
func TestApplyOperationReplaceDropsWhitespaceAtATextblocksEdges(t *testing.T) {
	for _, test := range []struct{ document, with, want string }{
		{document: "Intro.\n\nBody.\n", with: "   x", want: "Intro.\n\nx\n"},
		{document: "Intro.\n\nBody.\n", with: "x   ", want: "Intro.\n\nx\n"},
		{document: "x[^1]\n\n[^1]: Body.\n", with: "   x", want: "x[^1]\n\n[^1]: x\n"},
		{document: "Intro.\n\nBody. tail\n", with: "  x ", want: "Intro.\n\nx  tail\n"},
		{document: "Intro.\n\nhead Body.\n", with: " x  ", want: "Intro.\n\nhead  x\n"},
	} {
		t.Run(test.document+test.with, func(t *testing.T) {
			tree, err := parseInput(test.document)
			if err != nil {
				t.Fatal(err)
			}
			out, err := applyAlone(tree, model.EditOp{Op: "replace", Find: "Body.", With: test.with})
			if err != nil {
				t.Fatal(err)
			}
			markdown, err := pmdoc.Render(out)
			if err != nil {
				t.Fatal(err)
			}
			if markdown != test.want {
				t.Fatalf("replace with %q wrote %q, want %q", test.with, markdown, test.want)
			}
		})
	}
}

// A `with` whose first line ends in a backslash keeps its hard break: the backslash form of the
// break would read back as an escaped backslash and a soft break, so the text the caller wrote as
// two lines would come back as one.
func TestApplyOperationReplaceKeepsAHardBreakAfterATrailingBackslash(t *testing.T) {
	tree, err := parseInput("Intro.\n\nBody.\n\nAfter.\n")
	if err != nil {
		t.Fatal(err)
	}
	tree, err = applyAlone(tree, model.EditOp{
		Op: "replace", Find: "Body.", With: "Ends in a backslash \\\\  \nnext line",
	})
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	markdown, err := renderTree(tree)
	if err != nil {
		t.Fatal(err)
	}
	back, err := parseInput(markdown)
	if err != nil {
		t.Fatalf("parse %q: %v", markdown, err)
	}
	var kinds []string
	for _, child := range back.Children[1].Children {
		kinds = append(kinds, child.Type)
	}
	if strings.Join(kinds, ",") != "text,hardbreak,text" {
		t.Fatalf("the rewritten paragraph is %v, rendered %q", kinds, markdown)
	}
}

// A document whose block ends in a backslash renders exactly as it was written, so nothing about
// it drifts: the canonical markdown a version records is the caller's own bytes.
func TestADocumentEndingInABackslashRendersAsItWasStored(t *testing.T) {
	service, artifactID := newTestService(t)
	const stored = `The path is C:\` + "\n"
	seedServiceText(t, service, artifactID, stored)

	markdown, err := renderTree(liveTree(t, service, artifactID))
	if err != nil {
		t.Fatal(err)
	}
	if markdown != stored {
		t.Fatalf("the document renders %q, where it is stored as %q", markdown, stored)
	}
}

// A link whose text carries a `]` keeps its link: the escaped bracket `replace` accepts comes
// back as a literal, where an unescaped one closes the label early and the link is gone.
func TestApplyOperationReplaceKeepsALinkWhoseTextHasABracket(t *testing.T) {
	tree, err := parseInput("Intro.\n\nBody.\n")
	if err != nil {
		t.Fatal(err)
	}
	tree, err = applyAlone(tree, model.EditOp{
		Op: "replace", Find: "Body.", With: `[a\]b](https://x.test)`,
	})
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	markdown, err := renderTree(tree)
	if err != nil {
		t.Fatal(err)
	}
	back, err := parseInput(markdown)
	if err != nil {
		t.Fatalf("parse %q: %v", markdown, err)
	}
	first := back.Children[1].Children[0]
	if first.Text != `a]b` || len(first.Marks) != 1 || first.Marks[0].Type != "link" {
		t.Fatalf("the link is gone: %q (%#v)", markdown, first)
	}
}

// Bold text ending in a backslash keeps its bold: the closing `**` would otherwise be escaped by
// that backslash, and the document would read back as plain text with a stray marker.
func TestApplyOperationReplaceKeepsAMarkAroundTextEndingInABackslash(t *testing.T) {
	tree, err := parseInput("Intro.\n\nBody.\n")
	if err != nil {
		t.Fatal(err)
	}
	tree, err = applyAlone(tree, model.EditOp{Op: "replace", Find: "Body.", With: `**a\\** tail`})
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	markdown, err := renderTree(tree)
	if err != nil {
		t.Fatal(err)
	}
	back, err := parseInput(markdown)
	if err != nil {
		t.Fatalf("parse %q: %v", markdown, err)
	}
	first := back.Children[1].Children[0]
	if len(first.Marks) != 1 || first.Marks[0].Type != "strong" {
		t.Fatalf("the bold is gone: %q (%#v)", markdown, first)
	}
}

// The refusal's heading advice is worth following only if it keeps what deleting the block loses.
// Replacing the paragraph with the new text's paragraph and inserting the heading beside it keeps
// that paragraph's block id; inserting the heading and deleting the old paragraph gives the same
// markdown and a new id. The id is what this saves - a rewrite drops the anchor marks on the text
// it rewrites either way (TestApplyOpsReportsAnEditThatOnlyDropsAnAnchorMarkAsChanged), and the
// surviving block is what an orphaned comment stays pinned to.
func TestReplacePlusInsertKeepsTheParagraphsBlockID(t *testing.T) {
	tree, err := parseInput("Intro.\n\nBody.\n\nAfter.\n")
	if err != nil {
		t.Fatal(err)
	}
	before := blockIDOfText(t, tree, "Body.")

	batch, err := applyOperations(tree, []model.EditOp{
		{Op: "replace", Find: "Body.", With: "Body text."},
		{Op: "insert", Markdown: "## New", Before: "Body text."},
	})
	if err != nil {
		t.Fatalf("replace then insert: %v", err)
	}
	tree = batch.tree
	markdown, err := pmdoc.Render(tree)
	if err != nil {
		t.Fatal(err)
	}
	if markdown != "Intro.\n\n## New\n\nBody text.\n\nAfter.\n" {
		t.Fatalf("markdown = %q", markdown)
	}
	if after := blockIDOfText(t, tree, "Body text."); after != before {
		t.Fatalf("block id = %q, want the paragraph's own %q", after, before)
	}
}

// blockIDOfText resolves a block the way the edit path does: the quote, then the block that
// holds it. Parsing mints the ids, so nothing stamps them here.
func blockIDOfText(t *testing.T, tree *pmdoc.Node, text string) string {
	t.Helper()
	r, err := pmdoc.FindQuote(tree, text, nil, nil)
	if err != nil {
		t.Fatalf("find %q: %v", text, err)
	}
	id, err := pmdoc.BlockIDForRange(tree, r)
	if err != nil {
		t.Fatalf("block id for %q: %v", text, err)
	}
	if id == "" {
		t.Fatalf("the block holding %q has no id", text)
	}
	return id
}

// A `with` carrying the marker its own block already renders would write that marker twice
// (`## ##`, `7. 7\.`, `4. 4\.`, `-    - `). The heading rename is the one shape that keeps
// working, because `find` carried the marker through the match.
func TestApplyOperationReplaceKeepsOneHeadingMarkerOnARename(t *testing.T) {
	tree, err := parseInput("## New since we talked (2026-09-24)\n\nBody.\n")
	if err != nil {
		t.Fatal(err)
	}
	next, err := applyAlone(tree, model.EditOp{
		Op:   "replace",
		Find: "## New since we talked (2026-09-24)",
		With: "## New since we talked (2026-09-24) - SUPERSEDED, kept as record",
	})
	if err != nil {
		t.Fatalf("rename heading: %v", err)
	}
	markdown, err := renderTree(next)
	if err != nil {
		t.Fatal(err)
	}
	const want = "## New since we talked (2026-09-24) - SUPERSEDED, kept as record\n\nBody.\n"
	if markdown != want {
		t.Fatalf("renamed heading = %q, want %q", markdown, want)
	}
}

// A `with` opening with a marker of the matched block's own kind would write that marker twice,
// so it is refused, and the refusal names the two ways out: the backslash escape for prose that
// merely looks like a marker, and the first line for the block's own new text, which leaves the
// lines under it to write the blocks the caller meant.
func TestApplyOperationReplaceRefusesAWithThatRepeatsTheBlocksOwnMarker(t *testing.T) {
	for _, test := range []struct {
		name, markdown, find, with, escaped string
	}{
		{name: "heading marker find never carried", markdown: "## Design\n", find: "Design", with: "## Design notes", escaped: "## \\## Design notes\n"},
		{name: "a heading marker on a heading whose find carried none", markdown: "## Design\n", find: "Design", with: "# Design", escaped: "## \\# Design\n"},
		{name: "ordered marker on an ordered item", markdown: "1. Launcher contract\n", find: "Launcher contract", with: "1. Launcher contract, ruled", escaped: "1. 1\\. Launcher contract, ruled\n"},
		{name: "prose that merely looks like an ordered marker", markdown: "1. Launcher contract\n", find: "Launcher contract", with: "1999. was a year", escaped: "1. 1999\\. was a year\n"},
		{name: "bullet marker on a bullet item", markdown: "- Retracted\n", find: "Retracted", with: "- Retracted later", escaped: "- \\- Retracted later\n"},
		// The leading spaces land at the item's text start, where there is no text for them to
		// continue, so they are dropped: kept, `-    \- …` reads back as `- \- …` anyway.
		{name: "an indented bullet marker on a bullet item", markdown: "- Retracted\n", find: "Retracted", with: "   - Retracted later", escaped: "- \\- Retracted later\n"},
		{name: "a tab inside the marker", markdown: "- Retracted\n", find: "Retracted", with: "+\t3 degrees", escaped: "- \\+\t3 degrees\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			tree, err := parseInput(test.markdown)
			if err != nil {
				t.Fatal(err)
			}
			_, err = applyAlone(tree, model.EditOp{Op: "replace", Find: test.find, With: test.with})
			var invalid *ErrInvalidOp
			if !errors.As(err, &invalid) || invalid.Field != "with" {
				t.Fatalf("replace with %q = %v, want invalid with", test.with, err)
			}
			t.Logf("refusal: %s", invalid.Reason)
			if !strings.Contains(invalid.Reason, "omit the marker") {
				t.Fatalf("reason = %q, want the omit-the-marker guidance", invalid.Reason)
			}
			if !strings.Contains(invalid.Reason, "write the block's new text on the first line and the new blocks on the lines under it") {
				t.Fatalf("reason = %q, want it to name the first line as the block's own text", invalid.Reason)
			}
			// The escape the refusal offers is its one actionable remedy, so it has to be the
			// caller's own text: accepted when fed back, and rendering the prose they wrote.
			suggestion := backtickedSuggestion(t, invalid.Reason)
			next, err := applyAlone(tree, model.EditOp{Op: "replace", Find: test.find, With: suggestion})
			if err != nil {
				t.Fatalf("the suggested escape %q was refused: %v", suggestion, err)
			}
			markdown, err := renderTree(next)
			if err != nil {
				t.Fatal(err)
			}
			if markdown != test.escaped {
				t.Fatalf("the suggested escape %q rendered %q, want %q", suggestion, markdown, test.escaped)
			}
		})
	}
}

// A list item's whole text is the one match where the first line of `with` carries a rule of its
// own: a marker of the item's own kind is refused, and the item's new text goes on that line
// with the new blocks under it. A `find` that selected the item by its marker has that marker
// taken off the first line, as it always had, and a task item's box is still the item's own.
func TestApplyOperationReplaceOverAListItemsWholeText(t *testing.T) {
	for _, test := range []struct{ name, markdown, find, with, want, refusal string }{
		{
			name:     "a list opening with the item's own marker",
			markdown: "- a\n- b\n",
			find:     "a",
			with:     "- p\n- q",
			refusal:  "write the block's new text on the first line and the new blocks on the lines under it",
		},
		{
			name:     "the item's new text on the first line and the list under it",
			markdown: "- a\n- b\n",
			find:     "a",
			with:     "p\n- q",
			want:     "- p\n  - q\n- b\n",
		},
		{
			name:     "a find that selects the item by its marker",
			markdown: "- a\n- b\n",
			find:     "- a",
			with:     "- p\n- q",
			want:     "- p\n  - q\n- b\n",
		},
		{
			name:     "a task box, the item's text and a list under it",
			markdown: "- [ ] todo\n",
			find:     "todo",
			with:     "[x] done\n\n- a",
			want:     "- [x] done\n  - a\n",
		},
		{
			name:     "whitespace alone",
			markdown: "- a\n- b\n",
			find:     "a",
			with:     "   ",
			refusal:  "whitespace alone",
		},
		{
			name:     "an empty with deletes the item's text",
			markdown: "- a\n- b\n",
			find:     "a",
			with:     "",
			want:     "- \n- b\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			tree, err := parseInput(test.markdown)
			if err != nil {
				t.Fatal(err)
			}
			next, err := applyAlone(tree, model.EditOp{Op: "replace", Find: test.find, With: test.with})
			if test.refusal != "" {
				var invalid *ErrInvalidOp
				if !errors.As(err, &invalid) || invalid.Field != "with" {
					t.Fatalf("replace with %q = %v, want invalid with", test.with, err)
				}
				t.Logf("refusal: %s", invalid.Reason)
				if !strings.Contains(invalid.Reason, test.refusal) {
					t.Fatalf("reason = %q, want it to say %q", invalid.Reason, test.refusal)
				}
				if markdown, err := renderTree(tree); err != nil || markdown != test.markdown {
					t.Fatalf("the document reads %q (%v), want it unchanged", markdown, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("replace with %q: %v", test.with, err)
			}
			markdown, err := renderTree(next)
			if err != nil {
				t.Fatal(err)
			}
			if markdown != test.want {
				t.Fatalf("replace with %q = %q, want %q", test.with, markdown, test.want)
			}
		})
	}
}

// Three places hold no block of their own, and a replacement that names blocks is refused in
// each with the document unchanged: a heading, which the fit would take away; any cell of a
// table, which the fit lifts the blocks out of; and a typed block whose content rule leaves them
// outside it. Each refusal says what that place can hold, so the caller writes the block beside
// it with an insert.
func TestApplyOperationReplaceRefusesBlocksWhereNoBlockCanStand(t *testing.T) {
	const table = "| a | b |\n| --- | --- |\n| c | d |\n"
	const requirement = ":::requirement{#r1}\n**R1.** The rule.\n:::\n"
	for _, test := range []struct{ name, markdown, find, with, says string }{
		{
			name:     "a heading",
			markdown: "## Design\n\nBody.\n",
			find:     "Design",
			with:     "- a\n- b",
			says:     "a heading, which holds one line of text and no block of its own",
		},
		{
			name:     "the first header cell",
			markdown: table,
			find:     "a",
			with:     "- x\n- y",
			says:     "a table cell, which holds one paragraph of text",
		},
		{
			name:     "a body cell",
			markdown: table,
			find:     "c",
			with:     "- x\n- y",
			says:     "a table cell, which holds one paragraph of text",
		},
		{
			name:     "a one-paragraph requirement",
			markdown: requirement,
			find:     "The rule.",
			with:     "- a\n- b",
			says:     "the requirement holding the match cannot hold them",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			tree, err := parseInput(test.markdown)
			if err != nil {
				t.Fatal(err)
			}
			_, err = applyAlone(tree, model.EditOp{Op: "replace", Find: test.find, With: test.with})
			var invalid *ErrInvalidOp
			if !errors.As(err, &invalid) || invalid.Field != "with" {
				t.Fatalf("replace with %q = %v, want invalid with", test.with, err)
			}
			t.Logf("refusal: %s", invalid.Reason)
			if !strings.Contains(invalid.Reason, test.says) {
				t.Fatalf("reason = %q, want it to say %q", invalid.Reason, test.says)
			}
			if !strings.Contains(invalid.Reason, "insert the blocks") {
				t.Fatalf("reason = %q, want it to name the insert that writes the blocks beside it", invalid.Reason)
			}
			if markdown, err := renderTree(tree); err != nil || markdown != test.markdown {
				t.Fatalf("the document reads %q (%v), want it unchanged", markdown, err)
			}
		})
	}
}

// The other side of that rule: a block whose content rule holds the blocks takes them inside it,
// and a match in a code block is still the literal text of `with`, line by line, since a code
// block's text is not parsed at all.
func TestApplyOperationReplaceWritesBlocksInsideTheBlockThatHoldsThem(t *testing.T) {
	const callout = "Intro.\n\n:::callout{#c1 kind=\"note\" title=\"T\"}\nBody.\n:::\n"
	const requirement = ":::requirement{#r1}\n**R1.** The rule.\n:::\n"
	const ask = ":::ask{#a1 urgency=\"med\"}\nWhich one?\n:::\n"
	for _, test := range []struct{ name, markdown, find, with, want string }{
		{
			name:     "a list inside a callout",
			markdown: callout,
			find:     "Body.",
			with:     "- a\n- b",
			want:     "Intro.\n\n:::callout{#c1 kind=\"note\" title=\"T\"}\n- a\n- b\n:::\n",
		},
		{
			name:     "two paragraphs inside a requirement",
			markdown: requirement,
			find:     "The rule.",
			with:     "one\n\ntwo",
			want:     ":::requirement{#r1}\n**R1.**\n\none\n\ntwo\n:::\n",
		},
		{
			name:     "a list as an ask's options, under its question",
			markdown: ask,
			find:     "Which one?",
			with:     "Which one?\n\n- one\n- two",
			want:     ":::ask{#a1 urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich one?\n\n- one\n- two\n:::\n",
		},
		{
			name:     "the lines of a list inside a code block",
			markdown: "Intro.\n\n```yaml\nitems:\n  placeholder\n```\n",
			find:     "  placeholder",
			with:     "- name: x\n- name: y",
			want:     "Intro.\n\n```yaml\nitems:\n- name: x\n- name: y\n```\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			tree, err := parseInput(test.markdown)
			if err != nil {
				t.Fatal(err)
			}
			next, err := applyAlone(tree, model.EditOp{Op: "replace", Find: test.find, With: test.with})
			if err != nil {
				t.Fatalf("replace with %q: %v", test.with, err)
			}
			markdown, err := renderTree(next)
			if err != nil {
				t.Fatal(err)
			}
			if markdown != test.want {
				t.Fatalf("replace with %q = %q, want %q", test.with, markdown, test.want)
			}
		})
	}
}

// A `replace` that writes blocks settles what it wrote before anything stores it (INFRA-1520):
// the whitespace the split left at the inner edge of each half of the textblock the match lay in
// goes, since no text is left there to continue, and the blocks hold what their own markdown
// reads back as - an empty half the renderer does not write is dropped, and a list and its items
// take the spread they are written with. Each case prints the markdown it read.
func TestApplyOperationReplaceSettlesTheBlocksItWrote(t *testing.T) {
	const requirement = ":::requirement{#r1}\n**R1.** The rule.\n:::\n"
	const callout = "Intro.\n\n:::callout{#c1 kind=\"note\" title=\"T\"}\nBody.\n:::\n"
	for _, test := range []struct{ name, markdown, find, with, want string }{
		{
			// The trim alone, which is what repairs the spaces: the splice leaves the halves
			// ending and opening with the space around the match, stored as `Alpha&#32;` and
			// `&#32;gamma.`, and a reader of the document reads neither word as it was written.
			name:     "the spaces the split left at the match's own edges",
			markdown: "Alpha beta gamma.\n",
			find:     "beta",
			with:     "beta\n\n- a\n- b",
			want:     "Alpha\n\nbeta\n\n- a\n- b\n\ngamma.\n",
		},
		{
			name:     "the space a requirement's marker left where it ran into the match",
			markdown: requirement,
			find:     "The rule.",
			with:     "one\n\ntwo",
			want:     ":::requirement{#r1}\n**R1.**\n\none\n\ntwo\n:::\n",
		},
		{
			name:     "no empty half where the blocks take a paragraph's whole text",
			markdown: "Intro.\n\nBody.\n\nAfter.\n",
			find:     "Body.",
			with:     "- first\n- second",
			want:     "Intro.\n\n- first\n- second\n\nAfter.\n",
		},
		{
			name:     "no empty half where the blocks take a callout's whole text",
			markdown: callout,
			find:     "Body.",
			with:     "- a\n- b",
			want:     "Intro.\n\n:::callout{#c1 kind=\"note\" title=\"T\"}\n- a\n- b\n:::\n",
		},
		{
			// The item the write splits holds blocks with a blank line between them, so its
			// list is written loose, and the list and its items take that spread rather than
			// the tight one the splice left them with.
			name:     "the spread a list written inside a list item reads back with",
			markdown: "- foo one bar\n- two\n",
			find:     "one",
			with:     "- a\n- b",
			want:     "- foo\n\n  - a\n  - b\n\n  bar\n- two\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			tree, err := parseInput(test.markdown)
			if err != nil {
				t.Fatal(err)
			}
			next, err := applyAlone(tree, model.EditOp{Op: "replace", Find: test.find, With: test.with})
			if err != nil {
				t.Fatalf("replace with %q: %v", test.with, err)
			}
			markdown, err := renderTree(next)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("replace with %q wrote %q", test.with, markdown)
			if markdown != test.want {
				t.Fatalf("replace with %q = %q, want %q", test.with, markdown, test.want)
			}
			// No markdown writes an empty half, so the text above says nothing about one: the
			// tree is where it would stand, taking an id and reaching the next write.
			if empty := emptyParagraphCount(next); empty > 0 {
				t.Fatalf("replace with %q left %d empty paragraphs behind %q, want none", test.with, empty, markdown)
			}
		})
	}
}

// emptyParagraphCount is how many paragraphs of tree hold nothing.
func emptyParagraphCount(tree *pmdoc.Node) (empty int) {
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type == "paragraph" && len(node.Children) == 0 {
			empty++
		}
		return true
	})
	return empty
}

// The block ids a `replace` that writes blocks carries (INFRA-1520): the id of the paragraph the
// match lay in goes to the first block, in document order, that still holds that paragraph's
// text, so an ask, a comment or a reference keyed on it still names the block its reader sees.
// Every other block the write wrote or split off carries an id of its own, so no id names two
// blocks, and where no block holds that text any more the paragraph is gone and its id with it.
// Each case prints the ids it read before and after.
func TestApplyOperationReplaceCarriesTheBlockIDOfTheParagraphItWroteOver(t *testing.T) {
	for _, test := range []struct {
		name, markdown, find, with string
		// holds is the index among the result's document-level blocks of the one that keeps the
		// paragraph's id, or -1 where no block keeps it.
		holds int
	}{
		{
			name:     "the block holding the text before a match inside the paragraph",
			markdown: "Alpha beta gamma.\n",
			find:     "beta",
			with:     "beta\n\n- a\n- b",
			holds:    0,
		},
		{
			name:     "the block holding the text after a match at the first character",
			markdown: "Alpha beta gamma.\n",
			find:     "Alpha ",
			with:     "- a\n- b",
			holds:    1,
		},
		{
			name:     "the paragraph written first over the paragraph's whole text",
			markdown: "Here is the plan.\n",
			find:     "Here is the plan.",
			with:     "Here is the plan:\n\n- first\n- second",
			holds:    0,
		},
		{
			name:     "no block where a list alone took the paragraph's whole text",
			markdown: "Here is the plan.\n",
			find:     "Here is the plan.",
			with:     "- first\n- second",
			holds:    -1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			tree, err := parseInput(test.markdown)
			if err != nil {
				t.Fatal(err)
			}
			held := blockID(tree.Children[0])
			next, err := applyAlone(tree, model.EditOp{Op: "replace", Find: test.find, With: test.with})
			if err != nil {
				t.Fatalf("replace with %q: %v", test.with, err)
			}
			t.Logf("the document held %v; replace with %q left %v", writtenBlockIDs(tree), test.with, writtenBlockIDs(next))
			if test.holds >= 0 {
				if keeper := blockID(next.Children[test.holds]); keeper != held {
					t.Fatalf("block %d of %v carries %q, want the paragraph's %q", test.holds, writtenBlockIDs(next), keeper, held)
				}
			}
			carriers, minted := 0, map[string]bool{}
			pmdoc.Walk(next, func(node *pmdoc.Node) bool {
				if node.Type == "doc" || node.Type == "text" || isInlineLeaf(node) {
					return true
				}
				id := blockID(node)
				if id == "" {
					t.Fatalf("a %s of %v carries no id, want every block the write left to carry one", node.Type, writtenBlockIDs(next))
				}
				if minted[id] {
					t.Fatalf("%v names %q twice, want every block under an id of its own", writtenBlockIDs(next), id)
				}
				minted[id] = true
				if id == held {
					carriers++
				}
				return true
			})
			want := 1
			if test.holds < 0 {
				want = 0
			}
			if carriers != want {
				t.Fatalf("%d blocks of %v carry the paragraph's %q, want %d", carriers, writtenBlockIDs(next), held, want)
			}
		})
	}
}

// writtenBlockIDs is the kind and the id of each block of tree, in document order.
func writtenBlockIDs(tree *pmdoc.Node) []string {
	var ids []string
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type == "doc" || node.Type == "text" || isInlineLeaf(node) {
			return true
		}
		ids = append(ids, node.Type+":"+blockID(node))
		return true
	})
	return ids
}

// A comment pinned to a paragraph by its block id still points at that block after a replacement
// that writes blocks inside it: the half holding the text before the match keeps the paragraph's
// own id, and no other block takes it, so the row still names one block, the one holding the
// words the comment was written about.
func TestApplyOperationReplaceKeepsTheBlockIDAPinnedCommentNames(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "Alpha beta gamma.\n")
	commentID := insertAnchoredComment(t, service, artifactID, "Alpha")
	if _, err := service.BackfillAnchorBlocks(context.Background()); err != nil {
		t.Fatalf("pin the comment to its block: %v", err)
	}
	var encoded []byte
	if err := service.store.Pool.QueryRow(context.Background(),
		"select anchor from comments where id = $1", commentID).Scan(&encoded); err != nil {
		t.Fatalf("load the comment's anchor: %v", err)
	}
	var anchor model.Anchor
	if err := json.Unmarshal(encoded, &anchor); err != nil {
		t.Fatalf("decode the comment's anchor: %v", err)
	}
	if anchor.BlockID == nil || *anchor.BlockID == "" {
		t.Fatalf("the comment's anchor is %+v, want it pinned to a block", anchor)
	}
	pinned := *anchor.BlockID

	if _, err := joinedApplyOps(service, artifactID, []model.EditOp{
		{Op: "replace", Find: "beta", With: "beta\n\n- a\n- b"},
	}, model.Actor{Kind: "session", ID: "session-0123456789abcdef"}, nil); err != nil {
		t.Fatalf("write the blocks over the match: %v", err)
	}
	waitForDocumentText(t, service, artifactID, "Alpha\n\nbeta\n\n- a\n- b\n\ngamma.\n")

	live := liveTree(t, service, artifactID)
	t.Logf("the comment is pinned to %q; the document holds %v", pinned, writtenBlockIDs(live))
	var holders []string
	pmdoc.Walk(live, func(node *pmdoc.Node) bool {
		if blockID(node) == pinned {
			holders = append(holders, pmdoc.TextContent(node))
		}
		return true
	})
	if len(holders) != 1 || holders[0] != "Alpha" {
		t.Fatalf("the blocks under %q read %q, want the one holding %q", pinned, holders, "Alpha")
	}
}

// A `with` that writes blocks is judged by the two guards an insert's markdown is judged by
// (INFRA-1520). The id a typed block's markdown names must name one block of the document, so a
// `with` naming an id the document holds outside the text being replaced is refused as the same
// markdown through an insert is, and one naming an id twice is refused by the parse of the
// fragment, before it reaches the document. A `with` that rewrites the typed block the match
// lands in under that block's own id names one block still, and is written. Each case prints what
// it read.
func TestApplyOperationReplaceRefusesABlockIDThatWouldNameTwoBlocks(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	const callout = ":::callout{#" + id + " kind=\"note\" title=\"T\"}\n"
	for _, test := range []struct {
		name, markdown, find, with, want, refusal string
		// throughInsert asks for the same markdown inserted at the document's end, which must be
		// refused for the same reason on the field that carried it.
		throughInsert bool
	}{
		{
			name:          "an id the document holds outside the match",
			markdown:      "Intro.\n\nBody.\n\n" + callout + "Held.\n:::\n",
			find:          "Body.",
			with:          callout + "New.\n:::",
			refusal:       `block id "` + id + `" would name two blocks; give one of them another id, or omit {#` + id + `} to have one minted`,
			throughInsert: true,
		},
		{
			name:     "an id the markdown names twice",
			markdown: "Intro.\n\nBody.\n",
			find:     "Body.",
			with:     callout + "One.\n:::\n\n" + callout + "Two.\n:::",
			refusal:  `markdown is not a Proof document: outside Proof schema: block id "` + id + `" would name two blocks`,
		},
		{
			name:     "the typed block the match lands in, rewritten under its own id",
			markdown: "Intro.\n\n" + callout + "Body.\n:::\n",
			find:     "Body.",
			with:     callout + "New.\n\nMore.\n:::",
			want:     "Intro.\n\n" + callout + "New.\n\nMore.\n:::\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			tree, err := parseInput(test.markdown)
			if err != nil {
				t.Fatal(err)
			}
			next, err := applyAlone(tree, model.EditOp{Op: "replace", Find: test.find, With: test.with})
			if test.refusal == "" {
				if err != nil {
					t.Fatalf("replace with %q: %v", test.with, err)
				}
				markdown, err := renderTree(next)
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("replace with %q wrote %q", test.with, markdown)
				if markdown != test.want {
					t.Fatalf("replace with %q = %q, want %q", test.with, markdown, test.want)
				}
				return
			}
			var invalid *ErrInvalidOp
			if !errors.As(err, &invalid) || invalid.Field != "with" || !strings.Contains(invalid.Reason, test.refusal) {
				t.Fatalf("replace with %q = %v, want with refused saying %q", test.with, err, test.refusal)
			}
			t.Logf("replace with %q was refused: %v", test.with, err)
			if !test.throughInsert {
				return
			}
			inserted, err := parseInput(test.markdown)
			if err != nil {
				t.Fatal(err)
			}
			_, err = applyAlone(inserted, model.EditOp{Op: "insert", After: "end", Markdown: test.with})
			var insertRefusal *ErrInvalidOp
			if !errors.As(err, &insertRefusal) || insertRefusal.Field != "markdown" {
				t.Fatalf("insert of %q = %v, want markdown refused", test.with, err)
			}
			t.Logf("the same markdown inserted was refused: %v", err)
			if insertRefusal.Reason != invalid.Reason {
				t.Fatalf("insert says %q, want what the replace says, %q", insertRefusal.Reason, invalid.Reason)
			}
		})
	}
}

// What a replace stores is the document's rendering, so one whose rendering reads back otherwise
// is refused with the document unchanged (pmdoc.RefuseMisreadWrite), as an insert's markdown is.
// A write beside a stored table whose spans would pad more cells than one read-back may is such a
// write: the read-backs of each block the replace changed read that table nowhere, and the document
// read back as a whole cannot be read at all. Measured on 2026-11-11, the same replace beside a
// table whose spans pad the whole budget (wholeBudgetTable, 100,000 cells) is written, and no
// other shape tried reached this refusal: the renderer writes a list beside a list with the kind's
// other marker, a rule opening the document as `***`, and block syntax inside a line escaped, so
// each of those reads back as it was written.
func TestApplyOperationReplaceRefusesAWriteTheDocumentReadsBackOtherwise(t *testing.T) {
	tree := &pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{
		{Type: "paragraph", Children: []*pmdoc.Node{{Type: "text", Text: "Before."}}},
		spannedTable(1000, 101),
	}}
	pmdoc.EnsureBlockIDs(tree)
	_, err := applyAlone(tree, model.EditOp{Op: "replace", Find: "Before.", With: "- first\n- second"})
	var invalid *ErrInvalidOp
	if !errors.As(err, &invalid) || invalid.Field != "with" || !strings.Contains(invalid.Reason, "reads back otherwise") {
		t.Fatalf("a list written beside a table spanning 100,899 cells = %v, want with refused as reading back otherwise", err)
	}
	t.Logf("the write was refused: %v", err)
	if !strings.Contains(invalid.Reason, "limit 100000") {
		t.Fatalf("the refusal says %q, want it to name the read-back limit", invalid.Reason)
	}
}

// A replace that writes blocks under an open comment or ask carries that anchor onto one block of
// what it writes (INFRA-1520): the first paragraph or heading holding a run of text, which for a
// list is its first item's paragraph. The anchor quotes the words being rewritten, so it holds a
// line of the new text, and no other block of the replacement carries the mark - not a code block
// and not a table cell, which an anchor's quote cannot be read from. Each case prints what it
// wrote and what the anchor covers.
func TestApplyOperationReplaceKeepsAnOpenAnchorOnOneBlockOfWhatItWrote(t *testing.T) {
	for _, test := range []struct{ name, with, want, carries string }{
		{
			name:    "a list, on its first item's paragraph",
			with:    "- first\n- second",
			want:    "Intro.\n\n- first\n- second\n\nAfter.\n",
			carries: "paragraph:first",
		},
		{
			name:    "a paragraph and a list, on the paragraph",
			with:    "Plan:\n\n- first\n- second",
			want:    "Intro.\n\nPlan:\n\n- first\n- second\n\nAfter.\n",
			carries: "paragraph:Plan:",
		},
		{
			name:    "a paragraph and a code block, never on the code",
			with:    "Plan:\n\n```\ncode\n```",
			want:    "Intro.\n\nPlan:\n\n```\ncode\n```\n\nAfter.\n",
			carries: "paragraph:Plan:",
		},
		{
			name:    "a table and a paragraph, never on a cell",
			with:    "| a | b |\n| --- | --- |\n| c | d |\n\nTail.",
			want:    "Intro.\n\n| a | b |\n| --- | --- |\n| c | d |\n\nTail.\n\nAfter.\n",
			carries: "paragraph:Tail.",
		},
		{
			name:    "a heading and a list, on the heading",
			with:    "# Title\n\n- first",
			want:    "Intro.\n\n# Title\n\n- first\n\nAfter.\n",
			carries: "heading:Title",
		},
		{
			// A heading with no text holds no run for the mark to cover, so the anchor would be
			// lost there; the list item under it holds the first run of text the write makes.
			name:    "a heading with no text and a list, on the item's paragraph",
			with:    "#\n\n- first",
			want:    "Intro.\n\n# \n\n- first\n\nAfter.\n",
			carries: "paragraph:first",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			tree := anchoredDocument(t, "Intro.\n\nBody.\n\nAfter.\n", "Body.")
			next, err := applyAlone(tree, model.EditOp{Op: "replace", Find: "Body.", With: test.with})
			if err != nil {
				t.Fatalf("replace with %q: %v", test.with, err)
			}
			markdown, err := renderTree(next)
			if err != nil {
				t.Fatal(err)
			}
			carrying := blocksCarryingComment(next, anchoredCommentID)
			t.Logf("replace with %q wrote %q; the comment covers %v", test.with, markdown, carrying)
			if markdown != test.want {
				t.Fatalf("replace with %q = %q, want %q", test.with, markdown, test.want)
			}
			if len(carrying) != 1 || carrying[0] != test.carries {
				t.Fatalf("the comment covers %v, want the one block %q", carrying, test.carries)
			}
		})
	}
}

// A `with` that names blocks holding no line of text - a rule alone, a table alone, a heading with
// no text - is refused where an open anchor covers the match, since the anchor quotes the words
// being rewritten and nothing the write leaves could hold that quote. The same `with` where no
// anchor covers the match writes what it names, as it did before. Each case prints what it read.
func TestApplyOperationReplaceRefusesBlocksAnOpenAnchorCannotQuote(t *testing.T) {
	const document = "Intro.\n\nBody.\n\nAfter.\n"
	for _, test := range []struct{ name, with, want string }{
		{name: "a rule alone", with: "---", want: "Intro.\n\n---\n\nAfter.\n"},
		{
			name: "a table alone",
			with: "| a | b |\n| --- | --- |\n| c | d |",
			want: "Intro.\n\n| a | b |\n| --- | --- |\n| c | d |\n\nAfter.\n",
		},
		{name: "a heading with no text", with: "#", want: "Intro.\n\n# \n\nAfter.\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			op := model.EditOp{Op: "replace", Find: "Body.", With: test.with}
			_, err := applyAlone(anchoredDocument(t, document, "Body."), op)
			var invalid *ErrInvalidOp
			if !errors.As(err, &invalid) || invalid.Field != "with" {
				t.Fatalf("replace with %q under an anchor = %v, want with refused", test.with, err)
			}
			t.Logf("under an open comment, replace with %q was refused: %v", test.with, err)
			for _, says := range []string{"open comment or ask", "no line of text"} {
				if !strings.Contains(invalid.Reason, says) {
					t.Fatalf("the refusal says %q, want it to say %q", invalid.Reason, says)
				}
			}
			plain, err := parseInput(document)
			if err != nil {
				t.Fatal(err)
			}
			next, err := applyAlone(plain, op)
			if err != nil {
				t.Fatalf("replace with %q where no anchor covers the match: %v", test.with, err)
			}
			markdown, err := renderTree(next)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("with no anchor over the match, replace with %q wrote %q", test.with, markdown)
			if markdown != test.want {
				t.Fatalf("replace with %q = %q, want %q", test.with, markdown, test.want)
			}
		})
	}
}

// anchoredCommentID is the comment the documents of the anchor tests above are marked with.
const anchoredCommentID = "comment-anchor-1"

// anchoredDocument parses markdown and marks quote, a whole paragraph's text, as an open comment's
// anchor covers it: one mark over every run of that text, which is what a comment written through
// the API leaves (MarkQuote).
func anchoredDocument(t *testing.T, markdown, quote string) *pmdoc.Node {
	t.Helper()
	tree, err := parseInput(markdown)
	if err != nil {
		t.Fatal(err)
	}
	marked := false
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type == "text" && node.Text == quote {
			node.Marks = append(node.Marks, pmdoc.Mark{
				Type: string(MarkComment), Attrs: pmdoc.Attrs{"id": anchoredCommentID, "by": "user:alice"},
			})
			marked = true
		}
		return true
	})
	if !marked {
		t.Fatalf("%q holds no text run reading %q to anchor a comment on", markdown, quote)
	}
	return tree
}

// blocksCarryingComment is the kind and the text of each block of tree holding a run that carries
// the comment mark id, in document order and each block once.
func blocksCarryingComment(tree *pmdoc.Node, id string) []string {
	var carrying []string
	seen := map[*pmdoc.Node]bool{}
	var visit func(node, block *pmdoc.Node)
	visit = func(node, block *pmdoc.Node) {
		if node.Type == "text" {
			for _, mark := range node.Marks {
				if mark.Type == string(MarkComment) && mark.Attrs["id"] == id && !seen[block] {
					seen[block] = true
					carrying = append(carrying, block.Type+":"+pmdoc.TextContent(block))
				}
			}
			return
		}
		if node.Type != "doc" && !isInlineLeaf(node) {
			block = node
		}
		for _, child := range node.Children {
			visit(child, block)
		}
	}
	visit(tree, tree)
	return carrying
}

// A comment anchored on a whole paragraph's text still quotes the document after a replace that
// writes a list in that paragraph's place: the anchor holds the first item's text, which is the
// line a reader sees where the words it was written about stood, rather than being orphaned.
func TestApplyOpsReplaceWithAListKeepsAnOpenCommentsQuote(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "Intro.\n\nBody text here.\n")
	commentID := insertAnchoredComment(t, service, artifactID, "Body text here.")

	if _, err := joinedApplyOps(service, artifactID, []model.EditOp{
		{Op: "replace", Find: "Body text here.", With: "- first\n- second"},
	}, model.Actor{Kind: "user", ID: "alice"}, nil); err != nil {
		t.Fatalf("write the list over the paragraph's text: %v", err)
	}

	live := liveTree(t, service, artifactID)
	_, quote, found := pmdoc.FindMark(live, string(MarkComment), commentID)
	stored := loadCommentAnchor(t, service, commentID)
	markdown, err := renderTree(live)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("the document reads %q; the comment covers %q (found=%t) and its row quotes %q", markdown, quote, found, stored.Quote)
	if !found || quote != "first" {
		t.Fatalf("the comment covers %q found=%t, want the first item's %q", quote, found, "first")
	}
	if stored.Orphaned || stored.Quote != "first" {
		t.Fatalf("the comment's anchor = %#v, want it holding %q", stored, "first")
	}
}

// backtickedSuggestion is the single backtick-quoted example a refusal carries.
func backtickedSuggestion(t *testing.T, reason string) string {
	t.Helper()
	open := strings.Index(reason, "(`")
	if open < 0 {
		t.Fatalf("reason = %q, want a backticked escape example", reason)
	}
	rest := reason[open+2:]
	close := strings.Index(rest, "`)")
	if close < 0 {
		t.Fatalf("reason = %q, want a closed backticked escape example", reason)
	}
	return rest[:close]
}

// A rename whose `find` names the heading's actual level may also carry a different level in
// `with`: the caller has shown they know the current one, so that is how they say "and make it
// that level". A level-blind `find` — the `# ` selector both descriptions teach — renames the
// text and keeps the level it selected, rather than silently flattening the section (Deep1326).
func TestApplyOperationReplaceSetsAHeadingLevelOnlyFromALevelNamingFind(t *testing.T) {
	for _, test := range []struct {
		name, markdown, find, with, want string
	}{
		{name: "deeper", markdown: "## Design\n\nBody.\n", find: "## Design", with: "### Design", want: "### Design\n\nBody.\n"},
		{name: "shallower, with a rename", markdown: "### Design\n", find: "### Design", with: "# Design notes", want: "# Design notes\n"},
		{name: "a level-blind find renames and keeps the level", markdown: "## Design\n", find: "# Design", with: "## Design notes", want: "## Design notes\n"},
		{name: "a level-blind find never flattens", markdown: "## Design\n", find: "# Design", with: "# Design notes", want: "## Design notes\n"},
		{name: "a level-blind find never flattens a deep heading", markdown: "#### Deep\n", find: "# Deep", with: "# Deeper", want: "#### Deeper\n"},
		{name: "a level-blind find that only renames", markdown: "## Design\n", find: "# Design", with: "# Design", want: "## Design\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			tree, err := parseInput(test.markdown)
			if err != nil {
				t.Fatal(err)
			}
			next, err := applyAlone(tree, model.EditOp{Op: "replace", Find: test.find, With: test.with})
			if err != nil {
				t.Fatalf("retitle heading: %v", err)
			}
			markdown, err := renderTree(next)
			if err != nil {
				t.Fatal(err)
			}
			if markdown != test.want {
				t.Fatalf("replace with %q = %q, want %q", test.with, markdown, test.want)
			}
		})
	}
}

// The refusal quotes the marker the reader will see in the document, not a stand-in: telling a
// reader whose item is `7.` that the block renders `1.` sends them looking for a different
// bullet. A list beside a list of its kind is written with the kind's other marker.
func TestApplyOperationReplaceRefusalQuotesTheBlocksRealMarker(t *testing.T) {
	for _, test := range []struct{ markdown, find, with, want string }{
		{markdown: "7. Launcher contract\n8. Acceptance\n", find: "Launcher contract", with: "9. Launcher contract", want: `"7. "`},
		{markdown: "7. Launcher contract\n8. Acceptance\n", find: "Acceptance", with: "9. Acceptance", want: `"8. "`},
		{markdown: "0. Launcher contract\n1. Acceptance\n", find: "Launcher contract", with: "9. Launcher contract", want: `"0. "`},
		{markdown: "- a\n\n* b\n", find: "b", with: "* c", want: `"* "`},
		{markdown: "1. a\n\n1) b\n2) c\n", find: "c", with: "2) d", want: `"2) "`},
		{markdown: "- outer\n  - x\n  * y\n", find: "y", with: "- z", want: `"* "`},
	} {
		tree, err := parseInput(test.markdown)
		if err != nil {
			t.Fatal(err)
		}
		_, err = applyAlone(tree, model.EditOp{Op: "replace", Find: test.find, With: test.with})
		var invalid *ErrInvalidOp
		if !errors.As(err, &invalid) {
			t.Fatalf("replace %q in %q = %v, want invalid with", test.find, test.markdown, err)
		}
		if !strings.Contains(invalid.Reason, test.want) {
			t.Fatalf("reason = %q, want it to quote %s", invalid.Reason, test.want)
		}
	}
}

// A refusal is rendered from the typed error, not the wrapped chain, so the operation index has
// to survive on the error itself or a multi-op batch never says which operation was refused.
func TestApplyOperationsRefusalsCarryTheirOperationIndex(t *testing.T) {
	tree, err := parseInput("Body.\n\n- Retracted\n")
	if err != nil {
		t.Fatal(err)
	}
	_, err = applyOperations(tree, []model.EditOp{
		{Op: "replace", Find: "Body.", With: "Body!"},
		{Op: "replace", Find: "Retracted", With: "- Retracted later"},
	})
	var invalid *ErrInvalidOp
	if !errors.As(err, &invalid) || invalid.Field != "with" {
		t.Fatalf("batch error = %v, want invalid with", err)
	}
	if !strings.Contains(invalid.Error(), "operation 1") {
		t.Fatalf("refusal = %q, want it to name operation 1", invalid.Error())
	}
}

// An anchor that is a prefix of a longer heading misses, and the miss names the anchor and the
// nearest headings rather than only `pmdoc: target not found`.
func TestApplyOperationHeadingAnchorMissNamesTheAnchorAndNearestHeadings(t *testing.T) {
	tree, err := parseInput("## New since we talked (2026-09-24) - SUPERSEDED, kept as record\n\nBody.\n\n## Acceptance\n")
	if err != nil {
		t.Fatal(err)
	}
	_, err = applyAlone(tree, model.EditOp{
		Op:       "insert",
		Markdown: "Intro.",
		After:    "heading:New since we talked (2026-09-24)",
	})
	if !errors.Is(err, pmdoc.ErrTargetNotFound) {
		t.Fatalf("heading anchor miss = %v, want ErrTargetNotFound", err)
	}
	message := err.Error()
	for _, want := range []string{
		`"New since we talked (2026-09-24)"`,
		"New since we talked (2026-09-24) - SUPERSEDED, kept as record",
		"Acceptance",
		"nearest headings",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("heading anchor miss = %q, want it to name %q", message, want)
		}
	}
	if strings.Contains(message, "pmdoc") {
		t.Fatalf("heading anchor miss = %q, want no internal package name", message)
	}
}

func TestApplyOperationsQuoteMissNamesTheOperationTheQuoteAndNearestBlocks(t *testing.T) {
	tree, err := parseInput("Alpha block.\n\nBeta block.\n")
	if err != nil {
		t.Fatal(err)
	}
	_, err = applyOperations(tree, []model.EditOp{
		{Op: "replace", Find: "Alpha block.", With: "Alpha block!"},
		{Op: "replace", Find: "Gamma block.", With: "Gamma block!"},
	})
	if !errors.Is(err, pmdoc.ErrTargetNotFound) {
		t.Fatalf("quote miss = %v, want ErrTargetNotFound", err)
	}
	message := err.Error()
	for _, want := range []string{"operation 1", `"Gamma block."`, "nearest blocks"} {
		if !strings.Contains(message, want) {
			t.Fatalf("quote miss = %q, want it to name %q", message, want)
		}
	}
	if strings.Contains(message, "pmdoc") {
		t.Fatalf("quote miss = %q, want no internal package name", message)
	}
}

// A quote cut before its closing `**` can never match rendered text, and "quote not found"
// sends the agent looking for the wrong mistake.
func TestApplyOperationRefusesAFindWithAnUnbalancedInlineMark(t *testing.T) {
	tree, err := parseInput("1. **SUPERSEDED by the ruling** and the rest.\n")
	if err != nil {
		t.Fatal(err)
	}
	for _, find := range []string{"**SUPERSEDED by the", "and the `rest"} {
		_, err = applyAlone(tree, model.EditOp{Op: "replace", Find: find, With: "x"})
		var invalid *ErrInvalidOp
		if !errors.As(err, &invalid) || invalid.Field != "find" {
			t.Fatalf("unbalanced find %q = %v, want invalid find", find, err)
		}
		if !strings.Contains(invalid.Reason, "balanced") {
			t.Fatalf("unbalanced find %q reason = %q, want the balance rule", find, invalid.Reason)
		}
	}
}

// The unbalanced-mark verdict may be wrong about which mistake it names — a quote that misses and
// merely contains an odd delimiter — so the nearest blocks that would fix an ordinary miss stay
// in the message (Deep1326).
func TestApplyOperationUnbalancedMarkKeepsNearestBlocks(t *testing.T) {
	tree, err := parseInput("Closest rendered block.\n\nAnother block.\n")
	if err != nil {
		t.Fatal(err)
	}
	_, err = applyAlone(tree, model.EditOp{Op: "replace", Find: "Closest **unmatched", With: "x"})
	var invalid *ErrInvalidOp
	if !errors.As(err, &invalid) || invalid.Field != "find" {
		t.Fatalf("unbalanced find = %v, want invalid find", err)
	}
	if !strings.Contains(invalid.Reason, "nearest blocks") || !strings.Contains(invalid.Reason, "Closest rendered block.") {
		t.Fatalf("reason = %q, want the nearest blocks list", invalid.Reason)
	}
}

// An ambiguous `heading:` anchor is the same shape as an ambiguous quote: it names the operation
// and the anchor, with candidates to disambiguate, and offers the occurrence advice a heading
// anchor can actually use (Rev1326).
func TestApplyOperationsAmbiguousHeadingAnchorNamesTheOperationAndAnchor(t *testing.T) {
	tree, err := parseInput("Body.\n\n# Dup\n\nMore.\n\n# Dup\n")
	if err != nil {
		t.Fatal(err)
	}
	_, err = applyOperations(tree, []model.EditOp{
		{Op: "replace", Find: "Body.", With: "Body!"},
		{Op: "insert", Markdown: "Intro.", After: "heading:Dup"},
	})
	var ambiguous *ErrAnchorAmbiguous
	if !errors.As(err, &ambiguous) || ambiguous.Kind != "heading anchor" || ambiguous.Target != "Dup" || len(ambiguous.Candidates) != 2 {
		t.Fatalf("heading anchor ambiguity = %v, want it dressed with the anchor and two candidates", err)
	}
	if !strings.Contains(ambiguous.Error(), "operation 1") {
		t.Fatalf("heading anchor ambiguity = %q, want it to name operation 1", ambiguous.Error())
	}
	if strings.Contains(ambiguous.Error(), "surrounding text") {
		t.Fatalf("heading anchor ambiguity = %q, a heading anchor has no surrounding text to quote", ambiguous.Error())
	}
}

// An insert anchored on a quote spanning two blocks is the same failure as a replace or delete
// with that quote, and reads the same way (Rev1326).
func TestApplyOperationInsertWithACrossBlockQuoteAnchorNamesTheQuote(t *testing.T) {
	tree, err := parseInput("## Alpha\n\nBody.\n")
	if err != nil {
		t.Fatal(err)
	}
	_, err = applyAlone(tree, model.EditOp{Op: "insert", Markdown: "x", After: "Alpha Body."})
	var spans *ErrQuoteSpansBlocks
	if !errors.As(err, &spans) || spans.Quote != "Alpha Body." {
		t.Fatalf("cross-block insert anchor = %v, want the anchor named", err)
	}
}

// A quote that runs across table cells - a row as markdown writes it, with " | " between the
// cells - matches no text, because " | " is the boundary between two cells and neither cell holds
// it. The miss says so and names the row's cells to quote, for a replace, a delete, an insert and a
// move anchor alike, rather than ranking blocks against a quote that opens with a pipe. The same
// row quoted with the pipes left out reaches from one cell into the next, and that refusal names
// both cells too.
func TestApplyOperationQuoteAcrossTableCellsNamesTheCellsToQuote(t *testing.T) {
	tree, err := parseInput("Intro.\n\n| Item | Owner |\n| --- | --- |\n| Cut freezes run as build-pool pods | MACH; worker PoolFreeze |\n| Literal | x \\| y |\n| same | same |\n")
	if err != nil {
		t.Fatal(err)
	}
	intro, _ := tree.Children[0].Attrs[pmdoc.BlockIDAttr].(string)
	const cells = `"Cut freezes run as build-pool pods", "MACH; worker PoolFreeze"`
	const findAdvice = "one operation per cell in the same batch"
	const anchorAdvice = "anchor on one cell's own text"
	for name, test := range map[string]struct {
		op          model.EditOp
		cells, says string
	}{
		"a replace": {
			op:    model.EditOp{Op: "replace", Find: "Cut freezes run as build-pool pods | MACH; worker PoolFreeze", With: "x | y"},
			cells: cells, says: findAdvice,
		},
		"a replace with the row's own pipes": {
			op:    model.EditOp{Op: "replace", Find: "| Cut freezes run as build-pool pods | MACH; worker PoolFreeze |", With: "x"},
			cells: cells, says: findAdvice,
		},
		"a delete of part of each cell": {
			op:    model.EditOp{Op: "delete", Find: "build-pool pods | MACH; worker"},
			cells: cells, says: findAdvice,
		},
		"a row whose cells hold the same text": {
			op:    model.EditOp{Op: "replace", Find: "same | same", With: "x"},
			cells: `"same", "same"`, says: findAdvice,
		},
		"an insert anchor": {
			op:    model.EditOp{Op: "insert", Markdown: "x", After: "Cut freezes run as build-pool pods | MACH; worker PoolFreeze"},
			cells: cells, says: anchorAdvice,
		},
		"a move anchor": {
			op:    model.EditOp{Op: "move", Block: intro, Before: "Item | Owner"},
			cells: `"Item", "Owner"`, says: anchorAdvice,
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := applyAlone(tree, test.op)
			var missing *ErrQuoteNotFound
			if !errors.As(err, &missing) {
				t.Fatalf("cross-cell quote = %v, want *ErrQuoteNotFound", err)
			}
			message := missing.Error()
			for _, want := range []string{"one table cell", `" | " is the boundary`, test.cells, test.says} {
				if !strings.Contains(message, want) {
					t.Fatalf("cross-cell quote = %q, want it to contain %q", message, want)
				}
			}
		})
	}

	for name, test := range map[string]struct {
		op   model.EditOp
		says string
	}{
		"a replace":      {op: model.EditOp{Op: "replace", Find: "Item Owner", With: "x"}, says: findAdvice},
		"an insert":      {op: model.EditOp{Op: "insert", Markdown: "x", After: "Item Owner"}, says: anchorAdvice},
		"a move":         {op: model.EditOp{Op: "move", Block: intro, After: "Item Owner"}, says: anchorAdvice},
		"a move, before": {op: model.EditOp{Op: "move", Block: intro, Before: "Item Owner"}, says: anchorAdvice},
	} {
		t.Run(name+" reaching from one cell into the next", func(t *testing.T) {
			_, err := applyAlone(tree, test.op)
			var spans *ErrQuoteSpansBlocks
			if !errors.As(err, &spans) {
				t.Fatalf("quote reaching across two cells = %v, want *ErrQuoteSpansBlocks", err)
			}
			message := spans.Error()
			for _, want := range []string{"one table cell", `"Item", "Owner"`, test.says} {
				if !strings.Contains(message, want) {
					t.Fatalf("quote reaching across two cells = %q, want it to contain %q", message, want)
				}
			}
		})
	}

	// A quote reaching from a paragraph into a cell names no cells: only one end is in a table.
	_, err = applyAlone(tree, model.EditOp{Op: "replace", Find: "Intro. Item", With: "x"})
	var spans *ErrQuoteSpansBlocks
	if !errors.As(err, &spans) || strings.Contains(spans.Error(), "table cell") {
		t.Fatalf("quote from a paragraph into a cell = %v, want the ordinary cross-block refusal", err)
	}
	// A cell that holds " | " itself is quoted with it, as before.
	if _, err := applyAlone(tree, model.EditOp{Op: "replace", Find: "x | y", With: "x or y"}); err != nil {
		t.Fatalf("quote of a cell holding a pipe = %v, want it to match", err)
	}
	// A quote holding a pipe whose parts no run of one row's cells holds is an ordinary miss: one
	// part a cell holds is not a row.
	for _, find := range []string{"Intro | Outro", "Item | typo", "Item | Cut freezes run as build-pool pods"} {
		_, err = applyAlone(tree, model.EditOp{Op: "replace", Find: find, With: "x"})
		var missing *ErrQuoteNotFound
		if !errors.As(err, &missing) || strings.Contains(missing.Error(), "table cell") || !strings.Contains(missing.Error(), "nearest blocks") {
			t.Fatalf("pipe quote %q with no row = %v, want the ordinary miss with its nearest blocks", find, err)
		}
	}
	// An unclosed inline mark is refused as one before the quote is read as a row.
	_, err = applyAlone(tree, model.EditOp{Op: "replace", Find: "**Item | Owner", With: "x"})
	var invalid *ErrInvalidOp
	if !errors.As(err, &invalid) || !strings.Contains(invalid.Error(), "must be balanced") {
		t.Fatalf("unbalanced row quote = %v, want the unbalanced-mark refusal", err)
	}
}

// A quote that opens with a list item's marker (`5. `, `- `) selects the item whose text starts
// with the rest, as a quote's `# ` selects a heading: the marker is no text the item holds, so the
// quote matched nothing unless it also carried inline markup. Its number is not matched, a `with`
// carrying the same kind of marker replaces the item's text, and a miss says what the marker means
// and what to quote instead.
func TestApplyOperationQuoteOpeningWithAListMarkerSelectsTheItem(t *testing.T) {
	tree, err := parseInput("Plan:\n\n1. First point.\n2. Second point.\n3. A fresh reviewer checks points 1 to 4.\n\nThen:\n\n- bullet one\n- bullet two\n")
	if err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		op   model.EditOp
		want string
	}{
		"a replace by the item's own marker": {
			op:   model.EditOp{Op: "replace", Find: "3. A fresh reviewer checks points 1 to 4.", With: "3. A fresh reviewer checks points 1 to 5."},
			want: "3. A fresh reviewer checks points 1 to 5.\n",
		},
		"a replace under another number": {
			op:   model.EditOp{Op: "replace", Find: "5. A fresh reviewer", With: "One reviewer"},
			want: "3. One reviewer checks points 1 to 4.\n",
		},
		"a replace of a bullet item": {
			op:   model.EditOp{Op: "replace", Find: "- bullet two", With: "- bullet 2"},
			want: "- bullet 2\n",
		},
		"an insert anchor": {
			op:   model.EditOp{Op: "insert", Markdown: "Done.", After: "3. A fresh reviewer checks points 1 to 4."},
			want: "\n\nDone.\n",
		},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := applyAlone(tree, test.op)
			if err != nil {
				t.Fatalf("list-marker quote = %v, want it to select the item", err)
			}
			markdown, err := pmdoc.Render(out)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(markdown, test.want) {
				t.Fatalf("list-marker quote wrote %q, want it to hold %q", markdown, test.want)
			}
		})
	}
	out, err := applyAlone(tree, model.EditOp{Op: "delete", Find: "- bullet one"})
	if err != nil {
		t.Fatalf("list-marker delete = %v, want it to select the item", err)
	}
	if markdown, _ := pmdoc.Render(out); strings.Contains(markdown, "bullet one") || !strings.Contains(markdown, "- bullet two") {
		t.Fatalf("list-marker delete wrote %q, want the item gone and its sibling kept", markdown)
	}

	// The marker selects an item of its own kind whose text starts with the rest: a bullet does not
	// select an ordered item, and text inside an item is not where its marker stands.
	for find, kind := range map[string]string{"- A fresh reviewer": "an item of a bullet list", "2. point.": "an item of an ordered list"} {
		_, err := applyAlone(tree, model.EditOp{Op: "replace", Find: find, With: "x"})
		var missing *ErrQuoteNotFound
		if !errors.As(err, &missing) {
			t.Fatalf("list-marker quote %q = %v, want *ErrQuoteNotFound", find, err)
		}
		for _, want := range []string{"a leading list marker is no text the item holds", kind, "without the marker"} {
			if !strings.Contains(missing.Error(), want) {
				t.Fatalf("list-marker miss = %q, want it to contain %q", missing.Error(), want)
			}
		}
	}
}

// A task box after a list marker selects only a task item in that state, and a with carrying the
// marker and the other box ticks the item it selected.
func TestApplyOperationListMarkerQuoteMatchesTheTaskBox(t *testing.T) {
	tree, err := parseInput("- [ ] deploy\n- [x] ship\n")
	if err != nil {
		t.Fatal(err)
	}
	out, err := applyAlone(tree, model.EditOp{Op: "replace", Find: "- [ ] deploy", With: "- [x] deploy"})
	if err != nil {
		t.Fatalf("task-box quote = %v, want it to select the unchecked item", err)
	}
	if markdown, _ := pmdoc.Render(out); !strings.Contains(markdown, "- [x] deploy") {
		t.Fatalf("task-box replace wrote %q, want the item ticked", markdown)
	}
	_, err = applyAlone(tree, model.EditOp{Op: "replace", Find: "- [x] deploy", With: "x"})
	var missing *ErrQuoteNotFound
	if !errors.As(err, &missing) || !strings.Contains(missing.Error(), "a checked task item") || !strings.Contains(missing.Error(), `without the marker ("deploy")`) {
		t.Fatalf("checked box on an unchecked item = %v, want a miss naming the box and the text to quote", err)
	}
}

// A list marker selects an item only for a document edit's find or anchor: a comment's or an ask's
// quote, and an upload re-anchoring one, read the marker as the text it was, so a cached quote
// `5. deploy` never lands on an item `1. deploy`.
func TestAListMarkerSelectsAnItemForADocumentEditAlone(t *testing.T) {
	tree, err := parseInput("1. deploy\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pmdoc.FindQuote(tree, "5. deploy", nil, nil); !errors.Is(err, pmdoc.ErrTargetNotFound) {
		t.Fatalf("anchor quote with a list marker = %v, want no match", err)
	}
	if _, err := pmdoc.NewQuoteFinder(tree).NearestFirst("5. deploy", 0); !errors.Is(err, pmdoc.ErrTargetNotFound) {
		t.Fatalf("re-anchoring a quote with a list marker = %v, want no match", err)
	}
	if _, err := applyAlone(tree, model.EditOp{Op: "replace", Find: "5. deploy", With: "ship"}); err != nil {
		t.Fatalf("document edit with a list marker = %v, want it to select the item", err)
	}
}

// The selection reads each step of the matcher on its own, item text that opens with punctuation
// is no marker text, and a find that matched as markup elsewhere selected nothing by its marker.
func TestAListMarkerSelectionEdgeCases(t *testing.T) {
	render := func(t *testing.T, out *pmdoc.Node) string {
		t.Helper()
		markdown, err := pmdoc.Render(out)
		if err != nil {
			t.Fatal(err)
		}
		return markdown
	}
	// An exact match in a paragraph does not hide an item the text matches with its whitespace
	// normalized.
	tree, err := parseInput("ship now\n\n- ship  now\n")
	if err != nil {
		t.Fatal(err)
	}
	out, err := applyAlone(tree, model.EditOp{Op: "replace", Find: "- ship now", With: "- shipped"})
	if err != nil || !strings.Contains(render(t, out), "- shipped") || !strings.Contains(render(t, out), "ship now\n") {
		t.Fatalf("list-marker find beside an exact paragraph match = %v, want the item replaced", err)
	}
	// Item text that opens with punctuation like a marker's is the item's text.
	for find, with := range map[string]string{"- -2 degrees": "- -3 degrees", "1. 1.2 release": "1. 1.3 release"} {
		tree, err := parseInput("- -2 degrees\n\n1. 1.2 release\n")
		if err != nil {
			t.Fatal(err)
		}
		out, err := applyAlone(tree, model.EditOp{Op: "replace", Find: find, With: with})
		if err != nil || !strings.Contains(render(t, out), with) {
			t.Fatalf("replace %q with %q = %v, want the item's text replaced", find, with, err)
		}
	}
	// A find whose markup matched an unchecked item's text selected nothing by its checked box,
	// so a with carrying a marker is refused as it always was.
	tree, err = parseInput("- [ ] **deploy**\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applyAlone(tree, model.EditOp{Op: "replace", Find: "- [x] **deploy**", With: "- [ ] shipped"}); err == nil {
		t.Fatal("a checked box matched by markup on an unchecked item = applied, want it refused")
	}
}

// A with that is only the selected item's marker, and its box, would empty the item with no word
// of it, so it is refused; an empty with deletes the text on purpose.
func TestAMarkerOnlyWithForASelectedItemIsRefused(t *testing.T) {
	for doc, op := range map[string]model.EditOp{
		"- deploy\n":     {Op: "replace", Find: "- deploy", With: "- "},
		"1. deploy\n":    {Op: "replace", Find: "1. deploy", With: "1. "},
		"- [ ] deploy\n": {Op: "replace", Find: "- [ ] deploy", With: "- [x] "},
	} {
		tree, err := parseInput(doc)
		if err != nil {
			t.Fatal(err)
		}
		_, err = applyAlone(tree, op)
		var invalid *ErrInvalidOp
		if !errors.As(err, &invalid) || !strings.Contains(invalid.Error(), "marker with no text after it") {
			t.Fatalf("marker-only with %q on %q = %v, want it refused", op.With, doc, err)
		}
	}
	// On a plain item a box is text, so a with of the marker and a box gives the item that text.
	for doc, op := range map[string]model.EditOp{
		"- deploy\n":  {Op: "replace", Find: "- deploy", With: "- [x] "},
		"1. deploy\n": {Op: "replace", Find: "1. deploy", With: "1. [ ] "},
	} {
		tree, err := parseInput(doc)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := applyAlone(tree, op); err != nil {
			t.Fatalf("with %q on the plain item %q = %v, want the box written as text", op.With, doc, err)
		}
	}
}

// A row quote takes off only the escape of a pipe a cell holds, leaving every other backslash to
// the cell's inline parse, as the table parser does: a cell shown as `x\|z` is quoted `x\\\|z`.
func TestARowQuoteDecodesACellsEscapedPipeOnce(t *testing.T) {
	tree, err := parseInput("| A | B |\n| --- | --- |\n| x\\\\\\|z | y |\n")
	if err != nil {
		t.Fatal(err)
	}
	_, err = applyAlone(tree, model.EditOp{Op: "replace", Find: `x\\\|z | y`, With: "w"})
	var missing *ErrQuoteNotFound
	if !errors.As(err, &missing) || !strings.Contains(missing.Error(), `the cells are "x\\|z", "y"`) {
		t.Fatalf("row quote of a cell holding a backslash and a pipe = %v, want both cells named", err)
	}
}

// A row quote keeps an empty cell between two pipes, one part alone is no row, and a part written
// with inline markdown matches the text its cell shows.
func TestARowQuoteReadsEmptyAndFormattedCells(t *testing.T) {
	tree, err := parseInput("| A | Gap | B |\n| --- | --- | --- |\n| A |  | B |\n| Item | Owner | x |\n")
	if err != nil {
		t.Fatal(err)
	}
	for find, cells := range map[string]string{
		"A | | B":                          `the cells are "A", "", "B"`,
		"*Item* | [Owner](https://x.test)": `the cells are "Item", "Owner"`,
	} {
		_, err := applyAlone(tree, model.EditOp{Op: "replace", Find: find, With: "x"})
		var missing *ErrQuoteNotFound
		if !errors.As(err, &missing) || !strings.Contains(missing.Error(), cells) {
			t.Fatalf("row quote %q = %v, want it to name %s", find, err, cells)
		}
	}
	_, err = applyAlone(tree, model.EditOp{Op: "replace", Find: "| Gap |", With: "x"})
	var missing *ErrQuoteNotFound
	if !errors.As(err, &missing) || strings.Contains(missing.Error(), "table cell") {
		t.Fatalf("one cell between pipes = %v, want the ordinary miss", err)
	}
}

// A row quote is split as a markdown row is split: a pipe after an odd run of backslashes is the
// pipe a cell holds, and one after an even run ends a cell.
func TestApplyOperationRowQuoteSplitsAtPipesAsARowDoes(t *testing.T) {
	tree, err := parseInput("| Item | Owner |\n| --- | --- |\n| a\\\\ | b |\n")
	if err != nil {
		t.Fatal(err)
	}
	_, err = applyAlone(tree, model.EditOp{Op: "replace", Find: `Item \| Owner`, With: "x"})
	var missing *ErrQuoteNotFound
	if !errors.As(err, &missing) || strings.Contains(missing.Error(), "table cell") {
		t.Fatalf("quote with an escaped pipe = %v, want the ordinary miss", err)
	}
	// `a\\|` is a cell holding `a\` and the boundary after it.
	_, err = applyAlone(tree, model.EditOp{Op: "replace", Find: `a\\| b`, With: "x"})
	if !errors.As(err, &missing) || !strings.Contains(missing.Error(), `the cells are "a\\", "b"`) {
		t.Fatalf("quote with a pipe after two backslashes = %v, want it read as the row's two cells", err)
	}
}

// An out-of-range occurrence is a miss like any other: it names the quote, how many places it
// did match, and the nearest blocks — not a bare "target not found" (Rev1326).
func TestApplyOperationOutOfRangeOccurrenceNamesTheQuoteAndMatchCount(t *testing.T) {
	tree, err := parseInput("Alpha block.\n\nAlpha block.\n")
	if err != nil {
		t.Fatal(err)
	}
	occurrence := 5
	_, err = applyAlone(tree, model.EditOp{Op: "replace", Find: "Alpha block.", With: "x", Occurrence: &occurrence})
	var missing *ErrQuoteNotFound
	if !errors.As(err, &missing) || missing.Quote != "Alpha block." || missing.Matches != 2 {
		t.Fatalf("out-of-range occurrence = %v, want the quote and match count named", err)
	}
	if !strings.Contains(missing.Error(), "matches 2 places") || !strings.Contains(missing.Error(), "nearest blocks") {
		t.Fatalf("out-of-range occurrence = %q, want the match count and nearest blocks", missing.Error())
	}
}

// A too-wide table row is the same class of refusal as any other: it reaches the caller without
// the service's internal "apply live document operations:" prose (Quality1326).
func TestApplyOperationsTableWidthRefusalHasNoServiceProse(t *testing.T) {
	tree, err := parseInput("| Key | Value |\n| --- | --- |\n| a | b |\n")
	if err != nil {
		t.Fatal(err)
	}
	_, err = applyOperations(tree, []model.EditOp{
		{Op: "insert", Markdown: "| too | many | cells |", After: "b"},
	})
	if !errors.Is(err, pmdoc.ErrTableWidth) {
		t.Fatalf("table width error = %v, want ErrTableWidth", err)
	}
	if !isEditRefusal(err) {
		t.Fatalf("table width error = %v, want isEditRefusal true", err)
	}
}

// A fragment that passes the line check is parsed as rows of the table it lands in, as a
// whole-document write of the same rows is, whatever one- or two-hyphen rows it holds, and what that
// parse refuses is refused; a table the fragment makes itself keeps its own refusal, and a fragment
// that parse does not read as one table is the block path's, read as a document of its own.
func TestApplyOperationsJudgesTableRowsAgainstTheirTable(t *testing.T) {
	const three = "| K | V | W |\n| --- | --- | --- |\n| a | b | c |\n"
	const two = "| K | V |\n| --- | --- |\n| a | c |\n"
	for _, test := range []struct {
		name, table, markdown string
		// code is the error code the insert answers, or "" where it stores the rows as the
		// whole-document write of the table and the markdown does.
		code string
	}{
		{"dash row under three columns", three, "| A11 | x |\n| - | - |\n| A12 | y | z |", ""},
		{"dash row of one cell under three columns", three, "| A11 |\n| - |\n| A12 | y |", ""},
		{"dash row above a wide row", two, "| A11 | x |\n| - | - |\n| A12 | y | z |", "TABLE_WIDTH"},
		{"wide row", two, "| A11 | x |\n| A12 | y | z |", "TABLE_WIDTH"},
		{"wide row in a list's own table", two, "- | h |\n  | - |\n  | a | b |", "INVALID_OP"},
		{"wide row in a table after a heading", two, "# h | x\n| a |\n| - |\n| b | c |", "INVALID_OP"},
		// U+00A0 is a cell's text to goldmark's row trim, so `|` before one is no lone `|`: the line
		// is a row, and a dash cell beside one is no delimiter row.
		{"lone pipe and U+00A0 after a wide row", two, "| A11 | x | y |\n|\u00a0", "TABLE_WIDTH"},
		{"lone pipe and U+00A0", two, "| A11 | x |\n|\u00a0", ""},
		{"dash cell and U+00A0", two, "| A11 | x |\n| --- |\u00a0", ""},
		// The rows parse refuses before it asks whether it read one table, so a heading after a wide
		// row is answered for the row, where the block path alone would store a paragraph and a heading.
		{"wide row then a heading", two, "| a | b | c |\n# h | x", "TABLE_WIDTH"},
		// The only case the block path answers: the heading after the rows makes it read the fragment
		// as a document of its own, whose dash row makes a table of its first row. Refused at that
		// table's width, where main stored it with `z` dropped and an upload stores the rows.
		{"dash row and a heading under three columns", three, "| A11 | x |\n| - | - |\n| A12 | y | z |\n# h | q", "INVALID_OP"},
	} {
		t.Run(test.name, func(t *testing.T) {
			tree, err := parseInput(test.table)
			if err != nil {
				t.Fatal(err)
			}
			batch, err := applyOperations(tree, []model.EditOp{{Op: "insert", Markdown: test.markdown, After: "c"}})
			switch test.code {
			case "INVALID_OP":
				var invalid *ErrInvalidOp
				if !errors.As(err, &invalid) || errors.Is(err, pmdoc.ErrTableWidth) {
					t.Fatalf("insert = %v, want INVALID_OP and not TABLE_WIDTH", err)
				}
			case "TABLE_WIDTH":
				var invalid *ErrInvalidOp
				if !errors.Is(err, pmdoc.ErrTableWidth) || errors.As(err, &invalid) {
					t.Fatalf("insert = %v, want TABLE_WIDTH and not INVALID_OP", err)
				}
			default:
				if err != nil {
					t.Fatalf("insert = %v, want the rows stored", err)
				}
				written, err := pmdoc.ParseForWrite(test.table+test.markdown+"\n", nil)
				if err != nil {
					t.Fatal(err)
				}
				got, err := pmdoc.Render(batch.tree)
				if err != nil {
					t.Fatal(err)
				}
				want, err := pmdoc.Render(written)
				if err != nil {
					t.Fatal(err)
				}
				if got != want {
					t.Fatalf("insert stored %q, the document write %q", got, want)
				}
			}
		})
	}
}

// blockAskHarness seeds a document with the ask fixture, settles it, and returns the ask's
// id plus helpers that settle the room and count the events on that ask.
func blockAskHarness(t *testing.T) (*Service, string, string, func(), func() int) {
	t.Helper()
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, askFixture+"\nContext ends with no drift.\n")
	service.settleRoom(artifactID, 0)
	var askID string
	if err := service.store.Pool.QueryRow(context.Background(), `
		select id::text from asks where block_artifact_id = $1 and block_id = 'decision'
	`, artifactID).Scan(&askID); err != nil {
		t.Fatalf("read indexed ask: %v", err)
	}
	settle := func() { settleCurrentGeneration(t, service, artifactID) }
	askEvents := func() int {
		var count int
		if err := service.store.Pool.QueryRow(context.Background(), `
			select count(*) from events where type like 'ask.%' and payload->>'id' = $1
		`, askID).Scan(&count); err != nil {
			t.Fatalf("count ask events: %v", err)
		}
		return count
	}
	if got := askEvents(); got != 1 {
		t.Fatalf("ask events after indexing = %d, want the opened event only", got)
	}
	return service, artifactID, askID, settle, askEvents
}

func askRowState(t *testing.T, service *Service, askID string) (state string, resolutionKind *string) {
	t.Helper()
	if err := service.store.Pool.QueryRow(context.Background(), `
		select state, resolution->>'kind' from asks where id = $1
	`, askID).Scan(&state, &resolutionKind); err != nil {
		t.Fatalf("read ask row: %v", err)
	}
	return state, resolutionKind
}

var editingSession = model.Actor{Kind: "session", ID: "session-0123456789abcdef"}

func TestApplyOpsMovingAnOpenAskBlockKeepsItsAsk(t *testing.T) {
	service, artifactID, askID, settle, askEvents := blockAskHarness(t)
	if _, err := joinedApplyOps(service, artifactID, []model.EditOp{{Op: "move", Block: "decision", After: "no drift."}}, editingSession, nil); err != nil {
		t.Fatalf("move ask block: %v", err)
	}
	waitForDocumentText(t, service, artifactID, "Context ends with no drift.\n\n"+askFixture)
	settle()
	if state, _ := askRowState(t, service, askID); state != "open" {
		t.Fatalf("moved ask state = %q, want open", state)
	}
	if got := askEvents(); got != 1 {
		t.Fatalf("ask events after move = %d, want no retract or reopen", got)
	}
}

func TestApplyOpsMovingAnAnsweredAskBlockKeepsItsAnswer(t *testing.T) {
	service, artifactID, askID, settle, askEvents := blockAskHarness(t)
	// Answer the ask the way the answer handler does: the row first, then the block's
	// server-owned attributes.
	answeredAt := time.Date(2026, 9, 15, 17, 37, 34, 0, time.UTC)
	answerJSON, err := json.Marshal(model.AskAnswer{User: "alice", Selected: []string{"REST"}, At: answeredAt})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.store.Pool.Exec(context.Background(), `update asks set state = 'answered', answer = $2 where id = $1`, askID, answerJSON); err != nil {
		t.Fatalf("answer indexed ask: %v", err)
	}
	if err := joinedSetBlockAttributes(service, artifactID, "decision", map[string]any{
		"state": "answered", "answered_by": "alice", "answered_at": answeredAt.Format(time.RFC3339Nano), "selected": []string{"REST"},
	}, model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("write answered attributes: %v", err)
	}
	const answeredAsk = ":::ask{#decision urgency=\"high\" multiple=\"false\" state=\"answered\" answered_by=\"alice\" answered_at=\"2026-09-15T17:37:34Z\" selected=\"[&#x22;REST&#x22;]\"}\nWhich transport?\n:::\n"
	waitForDocumentText(t, service, artifactID, answeredAsk+"\nContext ends with no drift.\n")
	settle()

	if _, err := joinedApplyOps(service, artifactID, []model.EditOp{{Op: "move", Block: "decision", After: "no drift."}}, editingSession, nil); err != nil {
		t.Fatalf("move ask block: %v", err)
	}
	waitForDocumentText(t, service, artifactID, "Context ends with no drift.\n\n"+answeredAsk)
	settle()
	if state, _ := askRowState(t, service, askID); state != "answered" {
		t.Fatalf("moved ask state = %q, want answered", state)
	}
	if got := askEvents(); got != 1 {
		t.Fatalf("ask events after move = %d, want no retract, reopen, or repair", got)
	}

	// Deleting the answered block leaves the answered row as the record; a resolved row
	// carrying an answer is what the asks table forbids, and settlement must not fail on it.
	if _, err := joinedApplyOps(service, artifactID, []model.EditOp{{Op: "delete", Block: "decision"}}, editingSession, nil); err != nil {
		t.Fatalf("delete answered ask block: %v", err)
	}
	waitForDocumentText(t, service, artifactID, "Context ends with no drift.\n")
	settle()
	if state, resolution := askRowState(t, service, askID); state != "answered" || resolution != nil {
		t.Fatalf("deleted answered ask state=%q resolution=%v, want answered without a resolution", state, resolution)
	}
	if got := askEvents(); got != 1 {
		t.Fatalf("ask events after deleting an answered ask = %d, want none", got)
	}
}

// A document edit may not take a block an open ask points at out of the document: the batch is
// refused and the ask is left open (R5 of dispatch://INFRA-1520, refuseBatchDroppedAskBlocks). A
// person's editor is what removes such a block, and settlement retracts the ask of the block
// that left.
func TestAnEditCannotDropAnOpenAskBlockAndTheEditorsRemovalRetractsIt(t *testing.T) {
	service, artifactID, askID, settle, askEvents := blockAskHarness(t)
	var invalidAsk *ErrInvalidAskBlock
	_, err := joinedApplyOps(service, artifactID, []model.EditOp{{Op: "delete", Block: "decision"}}, editingSession, nil)
	if !errors.As(err, &invalidAsk) || !strings.Contains(err.Error(), "decision") {
		t.Fatalf("delete of the open ask block = %v, want it refused for the block it drops", err)
	}
	if state, _ := askRowState(t, service, askID); state != "open" {
		t.Fatalf("the refused delete left the ask %q, want it open", state)
	}

	editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
		kept := make([]*pmdoc.Node, 0, len(tree.Children))
		for _, child := range tree.Children {
			if id, _ := child.Attrs[pmdoc.BlockIDAttr].(string); id == "decision" {
				continue
			}
			kept = append(kept, child)
		}
		tree.Children = kept
		return tree
	})
	waitForDocumentText(t, service, artifactID, "Context ends with no drift.\n")
	settle()
	state, resolution := askRowState(t, service, askID)
	if state != "resolved" || resolution == nil || *resolution != "retracted" {
		t.Fatalf("the removed block's ask state=%q resolution=%v, want retracted", state, resolution)
	}
	if got := askEvents(); got != 2 {
		t.Fatalf("ask events after the removal = %d, want opened and resolved", got)
	}
}

// An edit is refused for the open asks of the blocks it drops itself, and for no others: a
// document can carry an open ask row whose block left it before this batch ran, and a batch that
// did not touch that block is not the write that lost it. The refusal names the batch's own block
// alone, and a batch that drops a block of its own whose ask is not that row is taken.
func TestAnEditIsRefusedForTheOpenAsksOfTheBlocksItDropsItself(t *testing.T) {
	const second = ":::ask{#second urgency=\"high\" multiple=\"false\" state=\"open\"}\nWhich store?\n:::\n"
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, askFixture+"\n"+second+"\nContext ends with no drift.\n")
	service.settleRoom(artifactID, 0)
	// The row of the second ask is made an orphan: it stays open, pointing at a block the
	// document does not hold, which is what a block removed in a person's editor leaves until
	// settlement retracts its ask.
	if _, err := service.store.Pool.Exec(context.Background(), `
		update asks set block_id = 'gone' where block_artifact_id = $1 and block_id = 'second'
	`, artifactID); err != nil {
		t.Fatalf("point the second ask at a block the document does not hold: %v", err)
	}

	var invalidAsk *ErrInvalidAskBlock
	_, err := joinedApplyOps(service, artifactID, []model.EditOp{{Op: "delete", Block: "decision"}}, editingSession, nil)
	if !errors.As(err, &invalidAsk) || !strings.Contains(err.Error(), "the open decision block decision,") {
		t.Fatalf("delete of the open ask block = %v, want it refused for the block it drops", err)
	}
	if strings.Contains(err.Error(), "gone") {
		t.Fatalf("the refusal = %v, want it silent about the orphan row's block, which this batch never touched", err)
	}

	// The batch below drops a block of its own, and the only open ask row left points at no
	// block of the document, so this batch loses no question anyone can answer.
	if _, err := joinedApplyOps(service, artifactID, []model.EditOp{{Op: "delete", Block: "second"}}, editingSession, nil); err != nil {
		t.Fatalf("delete of a block whose ask row is not open = %v, want it taken", err)
	}
	waitForDocumentText(t, service, artifactID, askFixture+"\nContext ends with no drift.\n")
}

// A `with` is parsed twice on the way in: once to say whether it names blocks of its own, and
// once by the path that writes the tree the document keeps. Only the second parse is charged to
// the write's elements, which are counted across every parse of one write
// (pmdoc.MaxDocumentElements), so a `with` one parse fits under the limit is applied. Measured
// 2026-11-11: 60,000 bytes of `)_` make 45,003 elements, and charging both parses refused this
// replace with "document too large to store".
func TestApplyOperationReplaceChargesTheWriteForOneParseOfItsWith(t *testing.T) {
	dense := strings.Repeat(")_", 30_000)
	measured := pmdoc.MeasureDocument(dense)
	if measured.TooHeavy() || 2*measured.Elements <= pmdoc.MaxDocumentElements {
		t.Fatalf("the with makes %d elements, want one parse of it under %d and two over", measured.Elements, pmdoc.MaxDocumentElements)
	}
	tree, err := parseInput("Intro.\n")
	if err != nil {
		t.Fatal(err)
	}
	next, err := applyAlone(tree, model.EditOp{Op: "replace", Find: "Intro.", With: dense})
	if err != nil {
		t.Fatalf("replace with %d dense bytes = %v, want it applied", len(dense), err)
	}
	// The `_` runs of the with pair into emphasis, so the text the document holds is the text
	// one parse of the with makes, not the with's own bytes.
	inline, err := pmdoc.ParseInline(dense, pmdoc.NewWriteBudget())
	if err != nil {
		t.Fatalf("one parse of the with = %v, want it under the element limit", err)
	}
	want := pmdoc.TextContent(&pmdoc.Node{Type: "paragraph", Children: inline})
	if got := pmdoc.TextContent(next); got != want {
		t.Fatalf("the replaced text is %d bytes, want the with's %d", len(got), len(want))
	}
}

// An empty with deletes the matched text on purpose. Where it empties a whole paragraph the block
// and its id stay, holding nothing, whatever blocks are around it; where the text it leaves would
// read as block syntax at a line start, it is stored escaped and reads back as the characters.
func TestApplyOperationReplaceWithNothingEmptiesTheParagraph(t *testing.T) {
	for _, test := range []struct{ name, markdown string }{
		{"before a list", "Intro.\n\nBody.\n\n- a\n"},
		{"after a list", "- a\n\nBody.\n\nAfter.\n"},
		{"before a fence", "Intro.\n\nBody.\n\n```\ncode\n```\n"},
		{"after a fence", "```\ncode\n```\n\nBody.\n"},
		{"before a heading", "Intro.\n\nBody.\n\n# Title\n"},
		{"after a heading", "# Title\n\nBody.\n"},
		{"before a table", "Intro.\n\nBody.\n\n| h |\n| --- |\n| c |\n"},
		{"before indented code", "Intro.\n\nBody.\n\n    code\n"},
		{"before a rule", "Intro.\n\nBody.\n\n***\n"},
		{"between paragraphs", "Intro.\n\nBody.\n\nAfter.\n"},
		{"a blockquote's paragraph before its list", "Intro.\n\n> Body.\n>\n> - a\n"},
		{"an ask's question", "Intro.\n\n:::ask{#a1 urgency=\"med\" multiple=\"false\" state=\"open\"}\nBody.\n\n- A\n- B\n:::\n"},
		{"a footnote definition's first paragraph of two", "x[^1]\n\n[^1]: Body.\n\n    More.\n"},
		{"a table body cell", "Intro.\n\n| h |\n| --- |\n| Body. |\n"},
		{"a footnote definition's only paragraph", "x[^1]\n\n[^1]: Body.\n"},
		{"a table header cell", "Intro.\n\n| Body. |\n| --- |\n| c |\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			tree, err := parseInput(test.markdown)
			if err != nil {
				t.Fatal(err)
			}
			pmdoc.EnsureBlockIDs(tree)
			var id string
			pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
				if node.Type == "paragraph" && pmdoc.TextContent(node) == "Body." {
					id = blockID(node)
				}
				return true
			})
			next, err := applyAlone(tree, model.EditOp{Op: "replace", Find: "Body.", With: ""})
			if err != nil {
				t.Fatalf("replace with nothing = %v, want the paragraph emptied", err)
			}
			var emptied *pmdoc.Node
			pmdoc.Walk(next, func(node *pmdoc.Node) bool {
				if blockID(node) == id {
					emptied = node
				}
				return true
			})
			if emptied == nil || emptied.Type != "paragraph" || pmdoc.TextContent(emptied) != "" {
				t.Fatalf("after replacing with nothing, block %q = %+v, want an empty paragraph", id, emptied)
			}
		})
	}
	tree, err := parseInput("Intro.\n\n--- x\n\nAfter.\n")
	if err != nil {
		t.Fatal(err)
	}
	next, err := applyAlone(tree, model.EditOp{Op: "replace", Find: " x", With: ""})
	if err != nil {
		t.Fatalf("replace leaving \"---\" = %v, want the text stored escaped", err)
	}
	markdown, err := renderTree(next)
	if err != nil {
		t.Fatal(err)
	}
	if want := "Intro.\n\n\\---\n\nAfter.\n"; markdown != want {
		t.Fatalf("replace leaving \"---\" stored %q, want %q", markdown, want)
	}
	if back, err := parseInput(markdown); err != nil || !back.Equal(next) {
		t.Fatalf("replace leaving \"---\" stored %q, which does not read back as written (%v)", markdown, err)
	}
}

// Emptying the paragraph a callout holds in a footnote definition is taken: the callout reads back
// holding one empty paragraph, as the browser editor's parser reads it, so the definition keeps
// its callout and its reference stays a footnote reference rather than literal text.
func TestApplyOperationEmptyingACalloutInAFootnoteKeepsTheCallout(t *testing.T) {
	tree, err := parseInput("x[^1]\n\n[^1]: :::callout{#c1 kind=\"note\" title=\"T\"}\n    Body.\n    :::\n")
	if err != nil {
		t.Fatal(err)
	}
	pmdoc.EnsureBlockIDs(tree)
	next, err := applyAlone(tree, model.EditOp{Op: "replace", Find: "Body.", With: ""})
	if err != nil {
		t.Fatalf("emptying the callout's paragraph = %v, want it taken", err)
	}
	markdown, err := renderTree(next)
	if err != nil {
		t.Fatal(err)
	}
	back, err := parseInput(markdown)
	if err != nil || !back.Equal(next) {
		t.Fatalf("after emptying, %q does not read back as written (%v)", markdown, err)
	}
	var references, callouts int
	pmdoc.Walk(back, func(node *pmdoc.Node) bool {
		switch node.Type {
		case "footnote_reference":
			references++
		case "callout":
			callouts++
		}
		return true
	})
	if references != 1 || callouts != 1 {
		t.Fatalf("after emptying, %q holds %d footnote references and %d callouts, want 1 and 1", markdown, references, callouts)
	}
}

// A code span keeps the whitespace that starts its next line, as the browser editor's parser reads
// it; only the prefix of the containers around it is not the code's. A replace writing one stores
// that code and reads it back.
func TestApplyOperationReplaceKeepsACodeSpansLineIndent(t *testing.T) {
	for _, test := range []struct{ name, markdown, with, code string }{
		{"after a line feed", "Intro.\n\nBody.\n\nAfter.\n", "x `a\n  b` y", "a\n  b"},
	} {
		t.Run(test.name, func(t *testing.T) {
			tree, err := parseInput(test.markdown)
			if err != nil {
				t.Fatal(err)
			}
			pmdoc.EnsureBlockIDs(tree)
			next, err := applyAlone(tree, model.EditOp{Op: "replace", Find: "Body.", With: test.with})
			if err != nil {
				t.Fatalf("replace with %q = %v, want it taken", test.with, err)
			}
			markdown, err := renderTree(next)
			if err != nil {
				t.Fatal(err)
			}
			back, err := parseInput(markdown)
			if err != nil || !back.Equal(next) {
				t.Fatalf("replace with %q stored %q, which does not read back as written (%v)", test.with, markdown, err)
			}
			var code []string
			pmdoc.Walk(back, func(node *pmdoc.Node) bool {
				for _, mark := range node.Marks {
					if node.Type == "text" && mark.Type == "inlineCode" {
						code = append(code, node.Text)
					}
				}
				return true
			})
			if len(code) != 1 || code[0] != test.code {
				t.Fatalf("replace with %q stored %q, whose code reads back %q, want %q", test.with, markdown, code, test.code)
			}
		})
	}
}

// A replace writes the blocks its text names, and text that names no block is written as text:
// a line of colons that opens no typed block reads back as the characters, in a paragraph, a
// callout and a footnote definition alike, stored escaped. The browser editor's parser reads
// those escaped lines as text too (the escaped-block-markers fixture).
func TestApplyOperationReplaceStoresBlockSyntaxEscaped(t *testing.T) {
	const footnote = "x[^1]\n\n[^1]: Body.\n"
	const callout = "Intro.\n\n:::callout{#c1 kind=\"note\" title=\"T\"}\nBody.\n:::\n"
	for _, test := range []struct{ name, markdown, with, stored string }{
		{"a callout, with colons", callout, ":::", "Intro.\n\n:::callout{#c1 kind=\"note\" title=\"T\"}\n\\:::\n:::\n"},
		{"a paragraph, a colon line then text", "Intro.\n\nBody.\n\nAfter.\n", ":::\nb", "Intro.\n\n\\::: b\n\nAfter.\n"},
		{"a footnote definition, a colon line then text", footnote, ":::\nb", "x[^1]\n\n[^1]: \\::: b\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			tree, err := parseInput(test.markdown)
			if err != nil {
				t.Fatal(err)
			}
			pmdoc.EnsureBlockIDs(tree)
			next, err := applyAlone(tree, model.EditOp{Op: "replace", Find: "Body.", With: test.with})
			if err != nil {
				t.Fatalf("replace with %q = %v, want it stored escaped", test.with, err)
			}
			markdown, err := renderTree(next)
			if err != nil {
				t.Fatal(err)
			}
			if markdown != test.stored {
				t.Fatalf("replace with %q stored %q, want %q", test.with, markdown, test.stored)
			}
			back, err := parseInput(markdown)
			if err != nil || !back.Equal(next) {
				t.Fatalf("replace with %q stored %q, which does not read back as written (%v)", test.with, markdown, err)
			}
		})
	}
}

// An insert's markdown is written into the document, so a leading `---` line is a rule, as `***`
// is, and never the front matter that would swallow it. Only the document's start can hold front
// matter, so an insert landing there, at `start` or before the first block, still opens the
// document with a closed front-matter block.
func TestApplyOperationInsertReadsFrontMatterOnlyAtTheStart(t *testing.T) {
	for _, test := range []struct {
		name string
		op   model.EditOp
		want string
	}{
		{"dashes after a paragraph", model.EditOp{Op: "insert", After: "Body.", Markdown: "---"}, "Intro.\n\nBody.\n\n---\n\nAfter.\n"},
		{"dashes at the end", model.EditOp{Op: "insert", After: "end", Markdown: "---"}, "Intro.\n\nBody.\n\nAfter.\n\n---\n"},
		{"a delimited block after a paragraph", model.EditOp{Op: "insert", After: "Body.", Markdown: "---\nx\n---"}, "Intro.\n\nBody.\n\n---\n\n## x\n\nAfter.\n"},
		{"front matter at the start", model.EditOp{Op: "insert", Before: "start", Markdown: "---\ntitle: T\n---"}, "---\ntitle: T\n---\n\nIntro.\n\nBody.\n\nAfter.\n"},
		{"front matter before the first block", model.EditOp{Op: "insert", Before: "Intro.", Markdown: "---\ntitle: T\n---"}, "---\ntitle: T\n---\n\nIntro.\n\nBody.\n\nAfter.\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			tree, err := parseInput("Intro.\n\nBody.\n\nAfter.\n")
			if err != nil {
				t.Fatal(err)
			}
			pmdoc.EnsureBlockIDs(tree)
			next, err := applyAlone(tree, test.op)
			if err != nil {
				t.Fatalf("insert %q = %v", test.op.Markdown, err)
			}
			if markdown, err := renderTree(next); err != nil || markdown != test.want {
				t.Fatalf("after inserting %q = %q (%v), want %q", test.op.Markdown, markdown, err, test.want)
			}
		})
	}
	// Only a closed block is front matter: an unclosed `---` at the start is a rule, as `***` is.
	opening := map[string]string{}
	for _, markdown := range []string{"---", "***"} {
		tree, err := parseInput("Intro.\n\nBody.\n\nAfter.\n")
		if err != nil {
			t.Fatal(err)
		}
		pmdoc.EnsureBlockIDs(tree)
		next, err := applyAlone(tree, model.EditOp{Op: "insert", Before: "start", Markdown: markdown})
		if err != nil {
			t.Fatalf("insert %q at the start = %v", markdown, err)
		}
		if opening[markdown], err = renderTree(next); err != nil {
			t.Fatal(err)
		}
	}
	if opening["---"] != opening["***"] {
		t.Fatalf("inserting `---` at the start = %q, want what `***` writes, %q", opening["---"], opening["***"])
	}
}

// A document with nothing in it holds one empty paragraph, as the primary document of an issue
// created without a spec does. The first insert takes that paragraph's place wherever it is
// anchored, rather than leaving an empty line beside what it writes, and so lands at the
// document's start, where a closed front-matter block is front matter.
func TestApplyOperationInsertIntoAnEmptyDocumentTakesItsPlace(t *testing.T) {
	for _, markdown := range []string{
		"The problem.",
		"## Problem\n\nAgents retry forever.\n",
		"- one\n- two\n",
		"---\ntitle: x\n---\n\nBody text.\n",
	} {
		for _, op := range []model.EditOp{
			{Op: "insert", After: "start", Markdown: markdown},
			{Op: "insert", Before: "start", Markdown: markdown},
			{Op: "insert", After: "end", Markdown: markdown},
			{Op: "insert", Before: "end", Markdown: markdown},
		} {
			tree, err := parseInput("")
			if err != nil {
				t.Fatal(err)
			}
			pmdoc.EnsureBlockIDs(tree)
			next, err := applyAlone(tree, op)
			if err != nil {
				t.Fatalf("insert %#v into an empty document: %v", op, err)
			}
			got, err := renderTree(next)
			if err != nil {
				t.Fatal(err)
			}
			written, err := parseInput(markdown)
			if err != nil {
				t.Fatal(err)
			}
			want, err := renderTree(written)
			if err != nil {
				t.Fatal(err)
			}
			if got != want || len(next.Children) != len(written.Children) {
				t.Fatalf("insert %#v into an empty document = %q with %d blocks, want %q alone", op, got, len(next.Children), want)
			}
		}
	}
}

// A panic in the check a write's changed blocks go through is this package's bug, never the
// caller's text, so replacementBroke passes it on as its error, answered as an internal one:
// read as a verdict before the write it would wave the write through, and after it it would
// refuse the caller's text with the panic's message.
func TestReplacementBrokePassesAPanicOnAsAnError(t *testing.T) {
	before, err := pmdoc.Parse("Intro.\n\nBody.\n")
	if err != nil {
		t.Fatal(err)
	}
	after, err := pmdoc.Parse("Intro.\n\nChanged.\n")
	if err != nil {
		t.Fatal(err)
	}
	panicked := fmt.Errorf("%w: reading markdown: boom", pmdoc.ErrPanic)
	// "Body." is the second paragraph's text, from position 9.
	match := pmdoc.Range{From: 9, To: 14}
	for _, test := range []struct {
		name  string
		check func(*pmdoc.Node) error
	}{
		{"before the write", func(*pmdoc.Node) error { return panicked }},
		{"after the write", func(block *pmdoc.Node) error {
			if block == after.Children[1] {
				return panicked
			}
			return nil
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			valid, err := pmdoc.ValidateDocument(before, "BlockIndexes")
			if err != nil {
				t.Fatal(err)
			}
			found, err := matchedBlocks(valid, match)
			if err != nil {
				t.Fatal(err)
			}
			block, broke, err := replacementBroke(before, after, found, test.check)
			if block != -1 || broke != nil || !errors.Is(err, pmdoc.ErrPanic) {
				t.Fatalf("replacementBroke = (%d, %v, %v), want an ErrPanic error and no verdict", block, broke, err)
			}
		})
	}
}

// applyAlone applies op to tree as a write of its own, on a budget of its own.
func applyAlone(tree *pmdoc.Node, op model.EditOp) (*pmdoc.Node, error) {
	return applyOperation(newBatchTree(tree), op, pmdoc.NewWriteBudget())
}
