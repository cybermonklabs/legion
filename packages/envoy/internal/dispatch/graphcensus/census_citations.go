package graphcensus

import (
	"context"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// citationProjections are the census's kinds of the citations of a block, a moved one among them
// (projections). It holds none.
var citationProjections []projection

// citationBackfills are the backfill's passes over the citations of a block (backfillPasses). It
// holds none.
var citationBackfills []backfillPass

// UnresolvedCitation is one block citation whose address resolves to no node: the node whose text
// cites - a document (its uuid), an ask, a comment or a message, as danglingRead names it - and the
// block address it cites.
type UnresolvedCitation struct {
	SourceKind, SourceID, Target string
}

// UnresolvedCitationReport is what the counter of unresolved citations read (UnresolvedCitations):
// each citation, in the order the read gave them.
type UnresolvedCitationReport struct {
	Citations []UnresolvedCitation
}

// Count is how many citations the report holds.
func (r UnresolvedCitationReport) Count() int64 {
	return int64(len(r.Citations))
}

// citedDocumentOwed is true of the block address the SQL expression address names while its
// document still owes its block index by either mark, a null version or store_owed_at
// (documentOwed), or while the issue room that document's fragment stands in owes one (roomOwed):
// a block of a room is indexed by a reconciliation of the whole room. Such an address may resolve
// to no node until that index is written. The document is the one whose ref_key is the part of
// the address before the first '#', since a ref_key holds none, which artifacts' ref_key index
// finds.
func citedDocumentOwed(address string) string {
	return `exists (
			select 1 from artifacts document
			left join document_rooms joined on joined.artifact_id = document.id
			where document.ref_key = split_part(` + address + `, '#', 1)
			  and (` + roomOwed("document.id") + ` or (joined.room is not null and ` + roomOwed("joined.room") + `)))`
}

// unresolvedRead is every block citation whose address resolves to no node (this issue's spec
// R13), in one statement. A citation is the node whose text cites an address, read off the five
// tables a citation of a block is stored in, as danglingRead reads them: refs, by its source, and
// block_refs, ask_block_refs, comment_block_refs and message_block_refs, by the document, ask,
// comment or message that holds the citing block. A node that cites one address from its own text
// and from one of its blocks, or from two blocks, is one citation.
//
// An address resolves as every resolver of an address reads it, through moved_to_reach alone
// (migration 20261118165901), as danglingRead and graphstore.MovedTo read it: its moved_to tuples,
// then its document's issue room, then the address itself, to a block a document_blocks row holds
// or to an ask whose row stands. So a block a move took to a new address resolves through the
// moved_to the move wrote, and a move adds no citation here. An address of a document that still
// owes its block index, or whose room owes one, is left out (citedDocumentOwed). Every other
// address is counted: a `#b-` citation in prose of a block no version holds, and one of a block an
// edit deleted, resolve to none, so the count is not zero over a healthy instance.
var unresolvedRead = `
	with cited (kind, id, address) as (
		select r.from_kind, r.from_id, r.to_id from refs r
		where r.to_kind = '` + refs.BlockNodeKind + `'
		union
		select '` + graphstore.KindArtifact + `', b.artifact_id::text, b.to_id from block_refs b
		where b.to_kind = '` + refs.BlockNodeKind + `'
		union
		select '` + graphstore.KindAsk + `', b.ask_id::text, b.to_id from ask_block_refs b
		where b.to_kind = '` + refs.BlockNodeKind + `'
		union
		select '` + graphstore.KindComment + `', b.comment_id::text, b.to_id from comment_block_refs b
		where b.to_kind = '` + refs.BlockNodeKind + `'
		union
		select '` + graphstore.KindMessage + `', b.message_id::text, b.to_id from message_block_refs b
		where b.to_kind = '` + refs.BlockNodeKind + `'
	),
	unresolved (address) as (
		select target.address from (select distinct cited.address from cited) target
		where not ` + citedDocumentOwed("target.address") + `
		  and (select reach.kind from moved_to_reach(target.address) reach) is null
	)
	select cited.kind, cited.id, cited.address
	from cited join unresolved on unresolved.address = cited.address
	order by cited.address, cited.kind, cited.id`

// UnresolvedCitations is the counter of unresolved citations (unresolvedRead), read in q: the
// `unresolved_citations n=<n>` line of the store census. It refuses nothing, since an instance
// already holds prose citations that resolve to no node; a driver reads it before and after a move
// and holds the two counts equal.
func UnresolvedCitations(ctx context.Context, q graphstore.Queryer) (UnresolvedCitationReport, error) {
	rows, err := q.Query(ctx, unresolvedRead)
	if err != nil {
		return UnresolvedCitationReport{}, fmt.Errorf("count the unresolved citations: %w", err)
	}
	citations, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (UnresolvedCitation, error) {
		var citation UnresolvedCitation
		err := row.Scan(&citation.SourceKind, &citation.SourceID, &citation.Target)
		return citation, err
	})
	if err != nil {
		return UnresolvedCitationReport{}, fmt.Errorf("count the unresolved citations: %w", err)
	}
	return UnresolvedCitationReport{Citations: citations}, nil
}

// writeUnresolvedCitations prints the counter's line, and a line for each citation it counted when
// verbose. Each line prints a count, node ids and block addresses alone.
func writeUnresolvedCitations(w io.Writer, unresolved UnresolvedCitationReport, verbose bool) {
	fmt.Fprintf(w, "census: unresolved_citations n=%s\n", pgmigrate.Thousands(unresolved.Count()))
	if verbose {
		for _, citation := range unresolved.Citations {
			fmt.Fprintf(w, "census: unresolved_citation from=%s:%s to=%s\n", citation.SourceKind, citation.SourceID, citation.Target)
		}
	}
}
