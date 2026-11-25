package refs

import (
	"context"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
)

// staleLensesOn reads the row of each stale lens that is not gone (StaleSQL) whose current part_of
// stands on the document node at the address $1 (StaleLensesOnSQL), in id order, as
// graphstore.ScanNode reads a node row. A document any release removes holds one empty version,
// whose index holds no block, so no lens stands on a block of it. The statement is built on its
// first read, since StaleSQL reads standingKinds, which this package's init fills and no
// initializer of the package may read.
var staleLensesOn = sync.OnceValue(func() string {
	return StaleLensesOnSQL(graphstore.NodeColumnsOf(`stale`), `$1`) + `
	order by stale.id`
})

// StaleLensesOnSQL is the select of columns over each stale lens that is not gone (StaleSQL),
// aliased stale, whose current part_of stands on the document node at the address the SQL document
// names, read into that node through graph_tuples_as_of_in: the lenses on one document that its
// removal reads (StaleLensesOn), and that the tick's read of whether its events owe the object pass
// reads at each address an event names (graphcensus's EventsOweObjectPass).
func StaleLensesOnSQL(columns, document string) string {
	return `
	select ` + columns + `
	from (
		` + graphstore.CurrentTupleRead(`t.from_id`, `t.from_id, t.asserted`,
		`t.to_kind = '`+graphstore.KindArtifact+`' and t.to_id = `+document+` and t.kind = '`+graphstore.TuplePartOf+`'
			and t.from_kind = '`+graphstore.KindLens+`' and t.to_kind <> '`+graphstore.KindValue+`'`) + `
	) placed
	join graph_nodes stale on stale.kind = '` + graphstore.KindLens + `' and stale.id = placed.from_id
	where placed.asserted and stale.state <> '` + graphstore.StateGone + `'
	  and ` + StaleSQL(`'`+graphstore.KindLens+`'`, `stale.id`)
}

// StaleLensesOn answers the row of each stale lens that is not gone on the document node at the
// address document, in id order (staleLensesOn): a lens whose recorded row
// (graphstore.AttributeArtifactID) no longer stands at that address. A document's removal reads it
// after the delete of the document's row, and takes each lens it answers gone with all that stands
// beneath it (api's removeArtifact). It takes no lock of its own: the removal reads it under the
// owner lock of the issue the address names. The api's reasoning writers take the same lock
// first (lockIssueOwner, or lockReasoningOwners for a write that asserts a prevented_by and
// for the pre-mortem conversion).
func StaleLensesOn(ctx context.Context, q Queryer, document string) ([]graphstore.Node, error) {
	rows, err := q.Query(ctx, staleLensesOn(), document)
	if err != nil {
		return nil, fmt.Errorf("read the stale lenses on %s: %w", document, err)
	}
	lenses, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (graphstore.Node, error) { return graphstore.ScanNode(row) })
	if err != nil {
		return nil, fmt.Errorf("read the stale lenses on %s: %w", document, err)
	}
	return lenses, nil
}
