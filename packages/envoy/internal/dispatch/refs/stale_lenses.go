package refs

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// staleLenses reads the row of each stale lens that is not gone (StaleSQL), as
// graphstore.ScanNode reads a node row: each lens that records a document's row
// (graphstore.AttributeArtifactID) whose row no longer stands at the address of the document it
// stands on, with the key of the issue that address names, `<issue key>/<slug>`, whose owner lock
// every writer under the lens takes (api's lockIssueOwner). Only a lens that records a row can be
// stale itself, so the read keeps those before it walks; every node under one is stale with it and
// is taken with it as NodesBeneath reads it. It reads the lenses of the store, through the primary
// key of graph_nodes. It is built on its first read, as staleLensesOn is, since StaleSQL reads
// standingKinds, which this package's init fills and no initializer of the package may read.
var staleLenses = sync.OnceValue(func() string {
	return `
	select ` + graphstore.NodeColumnsOf(`stale`) + `, split_part(split_part(placed.to_id, '#', 1), '/', 1)
	from graph_nodes stale
	cross join lateral (
		` + CurrentFarEndSQL(`newest.to_kind, newest.to_id`, `'`+graphstore.KindLens+`'`, `stale.id`, graphstore.TuplePartOf) + `
	) placed
	where stale.kind = '` + graphstore.KindLens + `' and stale.state <> '` + graphstore.StateGone + `'
	  and stale.attributes->>'` + graphstore.AttributeArtifactID + `' is not null
	  and ` + StaleSQL(`'`+graphstore.KindLens+`'`, `stale.id`) + `
	order by stale.id`
})

// staleLensesOn is staleLenses over the lenses whose current part_of stands on the document node
// at the address $1 alone (StaleLensesOnSQL). A document any release removes holds one empty
// version, whose index holds no block, so no lens stands on a block of it.
var staleLensesOn = sync.OnceValue(func() string {
	return StaleLensesOnSQL(graphstore.NodeColumnsOf(`stale`)+`, split_part($1, '/', 1)`, `$1`) + `
	order by stale.id`
})

// StaleLensesOnSQL is the select of columns over each stale lens that is not gone (StaleSQL),
// aliased stale, whose current part_of stands on the document node at the address the SQL document
// names, read into that node through graph_tuples_as_of_in: the lenses on one document that its
// removal reads (StateStaleLensesGone), and that the tick's read of whether its events owe the
// object pass reads at each address an event names (graphcensus's EventsOweObjectPass).
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

// readStaleLenses answers the row of each stale lens staleLenses reads, or staleLensesOn for the
// document at document where it is not empty, each with the issue its document's address names.
func readStaleLenses(ctx context.Context, tx pgx.Tx, document string) (lenses []graphstore.Node, issues []string, err error) {
	statement, args := staleLenses(), []any(nil)
	if document != "" {
		statement, args = staleLensesOn(), []any{document}
	}
	rows, err := tx.Query(ctx, statement, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("read the stale lenses: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var issue string
		lens, err := graphstore.ScanNode(rows, &issue)
		if err != nil {
			return nil, nil, fmt.Errorf("read the stale lenses: %w", err)
		}
		lenses, issues = append(lenses, lens), append(issues, issue)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("read the stale lenses: %w", err)
	}
	return lenses, issues, nil
}

// StateStaleLensesGone states gone, in the caller's transaction tx, opening none of its own, every
// stale lens the store holds that is not gone (StaleSQL): a lens on a document, or on a block of
// one, whose recorded row (graphstore.AttributeArtifactID) no longer stands at the document's
// address, because the row is gone or another row stands there. Each goes with its whole part_of
// subtree, at every depth, its findings, its risks and the lenses on those, whoever may see each
// node (graphstore.NodesBeneath), each written again as the store holds it but for its state,
// keeping its cause, with one state tuple at the sentinel signed by who at at
// (graphstore.AsStateWrite), all in one write. It reads a lens stale whatever the state of the node
// at its document's address - live behind no row, gone behind no row, or live for the row a later
// create made there - since the recorded row, and not the node, is what tells which document the
// lens was written on. It answers how many nodes it stated gone.
//
// Where document is empty it reads every stale lens of the store: the block-index tick runs it so
// after its orphan sweep, in a transaction of its own, by graphcensus.SweepActor at the database's
// clock (docs' writeObjectNodesAnotherBuildWrote), so a lens a removal by an earlier build left
// stale does not stay live in the store. Where document names a document's address it reads the
// stale lenses on that document's node alone: the removal of the document runs it so, in the
// transaction that deletes the row and after the delete, by the person at the removal's time (api's
// removeArtifact). An at of the zero time is the database's clock, read once the locks are held.
//
// It takes the store's lock order. Where document is empty it reads the stale lenses with no
// lock, and where there are none it takes no lock and writes nothing; otherwise it takes the owner
// lock of each issue those lenses' documents name (events.LockOwnerRows), which every writer of a
// lens, a finding, a risk or a node's state under the issue takes first (api's lockIssueOwner),
// and reads the stale lenses again under them, keeping those of an issue it locked. Where document
// names a document's address, its caller holds the owner lock of the issue that address names
// already, the removal's first lock, so it reads the stale lenses once, under that lock, and takes
// none. Each read answers the row of each stale lens, so no lens is read again; it then reads what
// stands beneath each, and writes the nodes, in ascending (kind, id), and their state tuples, in
// one graphstore.Write.
func StateStaleLensesGone(ctx context.Context, tx pgx.Tx, document string, at time.Time, who model.Actor) (int, error) {
	locked := map[string]bool{}
	if document == "" {
		_, issues, err := readStaleLenses(ctx, tx, "")
		if err != nil || len(issues) == 0 {
			return 0, err
		}
		owners := events.OwnerRows{}
		for _, issue := range issues {
			owners.Add(issue, "", "the issue of a stale lens")
			locked[issue] = true
		}
		if err := events.LockOwnerRows(ctx, tx, owners); err != nil {
			return 0, err
		}
	}
	lenses, issues, err := readStaleLenses(ctx, tx, document)
	if err != nil {
		return 0, err
	}
	if at.IsZero() {
		if err := tx.QueryRow(ctx, `select clock_timestamp()`).Scan(&at); err != nil {
			return 0, fmt.Errorf("read the time of the stale lenses' gone: %w", err)
		}
	}
	var gone []graphstore.Node
	taken := map[[2]string]bool{}
	take := func(node graphstore.Node) {
		if key := [2]string{node.Kind, node.ID}; !taken[key] {
			taken[key] = true
			gone = append(gone, node)
		}
	}
	for index, lens := range lenses {
		if document == "" && !locked[issues[index]] {
			continue
		}
		take(lens)
		beneath, err := graphstore.NodesBeneath(ctx, tx, lens.Kind, lens.ID)
		if err != nil {
			return 0, err
		}
		for _, node := range beneath {
			take(node)
		}
	}
	if len(gone) == 0 {
		return 0, nil
	}
	graphstore.AsStateWrite(gone, graphstore.StateGone, at.UTC(), who)
	if err := graphstore.Write(ctx, tx, gone, nil); err != nil {
		return 0, fmt.Errorf("state %d nodes of stale lenses gone: %w", len(gone), err)
	}
	return len(gone), nil
}
