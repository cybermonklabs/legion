package docs

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// ReadContext carries the rows this ledger has queued to the reads made under it. A route hands
// the helpers that index what it writes whatever context it holds - the request's - so each of
// them derives its reading context from the ledger it was handed, and that context is the only
// thing that shows a read the rows this transaction has queued and not written yet
// (refs.Pending). The same read under the context the route holds answers from the stored rows
// alone, which is what a reader that trusted the context it was passed would see.
func TestLedgerReadContextResolvesARowTheLedgerHasOnlyQueued(t *testing.T) {
	service, _ := newTestService(t)
	tx, ledger := joinProbe(t, service)
	// The insert of the issues row is queued with the transaction's other writes, so no read of
	// the issues table answers for it yet (RegisterPendingIssue).
	ledger.RegisterPendingIssue("LEDGER-7")
	mentions := func(ctx context.Context) []text.Located {
		reader := refs.NewReader(ctx, tx, "comment", "4a7f3b22-0f1c-4a6b-9a1e-2b7c8d9e0f11", service.serverURL)
		return reader.Mentions("LEDGER-7 is where this lands")
	}
	if located := mentions(context.Background()); len(located) != 0 {
		t.Fatalf("a read under the route's own context resolved %+v, want nothing: the row is only queued", located)
	}
	located := mentions(ledger.ReadContext(context.Background()))
	want := text.Ref{Kind: "issue", IssueKey: "LEDGER-7", ID: "LEDGER-7"}
	if len(located) != 1 || located[0].Ref != want {
		t.Fatalf("a read under ReadContext resolved %+v, want the queued issue %+v", located, want)
	}
}
