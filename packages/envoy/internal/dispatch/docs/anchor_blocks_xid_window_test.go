package docs

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The anchor-block backfill's own write transaction under storetest's xid rig (INFRA-636). The
// command is a one-off an operator runs, and it writes comment and ask rows the server is
// serving, so it is a write route for this issue's purposes: a backfill nobody counted is the
// writer nobody measured.
//
// It writes one row per transaction (backfillAnchorBlock), and the whole of what that
// transaction holds its transaction id for is the update that pins the row's block. The window
// is declared below as two statements and each is named: everything before the update - the
// row's owner lock, the re-read of its anchor under that lock, and the document work that
// resolves the quote - runs with no transaction id held, the lock because it is advisory and
// the reads because a read takes none.
func TestXidWindowAnchorBlockBackfill(t *testing.T) {
	service, database, tracer := xidService(t)
	spec, quotes := storetest.XidSpec("DOC", 1)
	artifactID := createIssueDocument(t, database, 1, spec)
	// The live text the quote is resolved against: the backfill reads the document through the
	// room, and the reads that load it run in the row's own transaction (Service.Join).
	seedServiceText(t, service, artifactID, spec)
	quote := quotes[len(quotes)/2]
	askID := insertAnchoredAsk(t, service, artifactID, quote)
	commentID := insertAnchoredComment(t, service, artifactID, quote)
	t.Logf("xid fixture: an ask and a comment anchored to a quote of a document of %d bytes, neither pinned to a block", len(spec))

	var backfill AnchorBlockBackfill
	report := tracer.Measure(t, database.Pool, func() {
		var err error
		backfill, err = service.BackfillAnchorBlocks(storetest.MarkXid(context.Background()))
		if err != nil {
			t.Errorf("backfill anchor blocks: %v", err)
		}
	})
	if backfill.Asks != 1 || backfill.Comments != 1 || backfill.Skipped != 0 {
		t.Fatalf("XID RIG PROBLEM (not a finding about the code): the backfill = %#v, want the ask and the comment pinned and nothing skipped", backfill)
	}
	wantBlockID, err := service.BlockForQuote(context.Background(), artifactID, quote)
	if err != nil {
		t.Fatalf("resolve the quote's block: %v", err)
	}
	for _, row := range []struct {
		table string
		id    string
	}{{"asks", askID}, {"comments", commentID}} {
		var encoded []byte
		if err := service.store.Pool.QueryRow(context.Background(),
			"select anchor from "+row.table+" where id = $1", row.id).Scan(&encoded); err != nil {
			t.Fatalf("load the %s anchor: %v", row.table, err)
		}
		var anchor model.Anchor
		if err := json.Unmarshal(encoded, &anchor); err != nil {
			t.Fatalf("decode the %s anchor: %v", row.table, err)
		}
		if anchor.BlockID == nil || *anchor.BlockID != wantBlockID {
			t.Fatalf("XID RIG PROBLEM (not a finding about the code): the %s row's block_id = %v, want %q: the measured transaction wrote nothing to judge",
				row.table, anchor.BlockID, wantBlockID)
		}
	}

	storetest.AssertXidWindows(t, report, storetest.XidWindowOf(2,
		"the one update that pins the row's block and its COMMIT"))
}
