package refs

import (
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// answeredState is the answer_state value of an answered ask: asks.StateAnswered, which refs does
// not import. asks reaches pglisten through pmdoc/schemanodes, and pglisten's tests reach refs
// through delivery, so an import of asks here is a cycle. TestAnsweredStateIsTheAsksAnsweredState
// holds the two to one value.
const answeredState = "answered"

// TypeSupertag is the supertag the node of a block of type typ carries (R26 of
// the issue-tree spec): its type, for a typed block of the block schema the store's
// supertag nodes install (pmdoc.IsTypedBlock: callout, ask, requirement, plan_task, check,
// criterion, decision, reading), and BlockSupertag for a block of any other kind, a paragraph, a
// heading, a list or a code block, whose kind is no supertag but the kind of its own text. A
// reading the typed-node pass does not take for one - in an ask nobody has answered - carries
// BlockSupertag instead (TypedFacts.supertag), which only the reading's holder says.
func TypeSupertag(typ string) string {
	if pmdoc.IsTypedBlock(typ) {
		return typ
	}
	return BlockSupertag
}

// retaggedBlock is a block of a version whose words the version left as they stand and whose node
// carries another supertag than its type gives, with that supertag (PlanBlocks): its id, and the
// supertag its node is restated with.
type retaggedBlock struct {
	id, supertag string
}

// ReadingHolder is the block a reading of a version stands in, as the typed-node pass reads it
// (DecisionFacts): its id, and whether it is an ask block, whose reading is a reading only while
// the ask it shows holds an answer, rather than a decision.
type ReadingHolder struct {
	ID  string
	Ask bool
}

// ReadingHolders is, of the readings tree holds - the tree a version was rendered from, or its
// stored markdown read back (pmdoc.ParseRendering) - each that stands in a decision or an ask block,
// by its id, with that holder: the readings the typed-node pass reads (DecisionFacts), each once,
// in the first decision or ask holding a block of its id. A reading in no decision or ask is no
// reading the pass takes for one, and is absent. The backfill's block batch, which reads no
// parent of a row a build before the store wrote, reads each reading's holder here, as the pass
// does, to give the reading's node the supertag the pass gives it (graphcensus).
func ReadingHolders(tree *pmdoc.Node) map[string]ReadingHolder {
	holders := map[string]ReadingHolder{}
	seen := map[string]struct{}{}
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type != decisionType && node.Type != askType {
			return true
		}
		id := TypedBlockID(node)
		if _, repeated := seen[id]; repeated || id == "" {
			return true
		}
		seen[id] = struct{}{}
		_, admitted := pmdoc.TypedContent(node)
		for _, child := range admitted {
			reading := TypedBlockID(child)
			if _, repeated := seen[reading]; child.Type != readingType || repeated || reading == "" {
				continue
			}
			seen[reading] = struct{}{}
			holders[reading] = ReadingHolder{ID: id, Ask: node.Type == askType}
		}
		return true
	})
	return holders
}

// AskBlockAnsweredSQL is true while the ask an ask block shows holds an answer, as the typed-node
// pass reads it (TypedSources.Shown, ShownAsk.Answered): the ask whose row the block's document
// keys it by (asks.block_artifact_id, the SQL text artifact, and block_id, the SQL text block,
// through asks_block_id_unique), or, for a copy of an ask of another document, the ask its node's
// current copied_from names (graphstore.TupleCopiedFrom, read from the node at the address SQL text
// address); and of either, whether its node holds an answer (askAnsweredSQL). Each probe stands
// behind an offset 0, so a statement over many blocks probes each one's ask by its key.
func AskBlockAnsweredSQL(artifact, block, address string) string {
	return `(exists (
			select 1 from asks shown
			where shown.block_artifact_id = ` + artifact + ` and shown.block_id = ` + block + `
			  and ` + askAnsweredSQL("shown.id::text") + `
			offset 0)
		or exists (
			select 1 from (` + graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_id, t.asserted`,
		`t.from_kind = '`+BlockNodeKind+`' and t.from_id = `+address+` and t.kind = '`+graphstore.TupleCopiedFrom+`'
				  and t.to_kind = '`+graphstore.KindAsk+`'`) + `) copied
			where copied.asserted and ` + askAnsweredSQL("copied.to_id") + `
			offset 0))`
}

// askAnsweredSQL is true while the node of the ask whose id is the SQL text ask holds an answer:
// its newest answer_state value is answered, as docs' askAnswers reads it, where a node that holds
// none reads as open.
func askAnsweredSQL(ask string) string {
	return `coalesce((select answer.asserted and answer.to_id = ` + store.QuoteLiteral(answeredState) + ` from graph_tuples answer
			where answer.from_kind = '` + graphstore.KindAsk + `' and answer.from_id = ` + ask + `
			  and answer.kind = '` + graphstore.TupleAnswerState + `' and answer.to_kind = '` + graphstore.KindValue + `'
			order by answer.seq desc limit 1), false)`
}

// copiedAskShownSQL is true while the reading whose document_blocks row is aliased row stands in
// the ask block its parent_block_id names (the SQL text holder) as a copy of an answered ask of
// another document, before a settlement states the block's copied_from (AskBlockAnsweredSQL). The
// typed-node pass matched the block to that ask in the version's own transaction (docs'
// readCopiedAskSources, through TypedSources.Shown), on the block's wording, which only the
// version's tree holds, and it records the match as the reading's part_of to the ask's node
// (askParents). So the census takes the ask that the reading's current part_of names, the marked
// row of that node, and holds it to the rest of that read: no ask of the reading's own document
// indexes the holder, since the pass looks for a source only then; the ask's block is the holder's
// id; and the ask is keyed by another document than the artifact the row is keyed by (the
// document, or its issue room, whose asks are its own: refs.DocumentRoomSQL), of that artifact's
// owner (docs' ownerDocuments: the same issue, or, for an artifact of no issue, a document of the
// same project that is of no issue and no agent conversation). A part_of to an ask of another
// issue or project, or to one under another id, is none a copy shows and reads as no answer.
// It holds while that ask's node holds an answer (askAnsweredSQL).
func copiedAskShownSQL(row, holder string) string {
	return `exists (
			select 1 from (` + graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_id, t.asserted`,
		`t.from_kind = '`+BlockNodeKind+`' and t.from_id = `+row+`.node_address and t.kind = '`+graphstore.TuplePartOf+`'
				  and t.to_kind = '`+graphstore.KindAsk+`'`) + `) placed
			join asks source on source.node_kind = '` + graphstore.KindAsk + `' and source.node_address = placed.to_id
			join artifacts source_document on source_document.id = source.block_artifact_id
			join artifacts copy_document on copy_document.id = ` + row + `.artifact_id
			where placed.asserted and source.block_id = ` + holder + ` and source_document.id <> copy_document.id
			  and case when copy_document.issue_key is not null then source_document.issue_key = copy_document.issue_key
				else source_document.issue_key is null and source_document.session_id is null
				  and source_document.project_key = coalesce(copy_document.project_key, '') end
			  and not exists (
				select 1 from asks own_ask
				where own_ask.block_artifact_id = ` + row + `.artifact_id and own_ask.block_id = ` + holder + `
				offset 0)
			  and ` + askAnsweredSQL("source.id::text") + `
			offset 0)`
}

// BlockTypeSupertagSQL is the supertag the node of the document_blocks row aliased row carries
// (R26 of the issue-tree spec), as SQL text: its type, for a typed block of the block
// schema - a type whose supertag node the store holds present and marks a block type, the nodes the
// installed schema is read from (pmdoc.IsTypedBlock, TypeSupertag), probed by graph_nodes' primary
// key - and BlockSupertag for every other block; but for a reading, which carries its type only
// where the typed-node pass takes it for one - in a decision, or in an ask block whose ask holds an
// answer - read off the block its row's parent_block_id names, which for a reading in an answered
// ask is still the ask block holding it, and BlockSupertag otherwise. The ask an ask block shows
// is its own document's, else, for a copy, the source a settlement claimed (AskBlockAnsweredSQL),
// or, before any settlement claims it, the source the typed-node pass matched it to
// (copiedAskShownSQL). It is the census's reading of the supertag a row's node must carry
// (graphcensus's block_type).
func BlockTypeSupertagSQL(row string) string {
	holder := row + `.parent_block_id`
	holderAddress := `left(` + row + `.node_address, length(` + row + `.node_address) - length(` + row + `.block_id)) || ` + holder
	return `case
		when ` + row + `.type = '` + readingType + `' then
			case when ` + holder + ` is not null and (exists (
					select 1 from document_blocks reading_holder
					where reading_holder.artifact_id = ` + row + `.artifact_id and reading_holder.block_id = ` + holder + `
					  and reading_holder.type = '` + decisionType + `'
					offset 0)
				or exists (
					select 1 from document_blocks reading_holder
					where reading_holder.artifact_id = ` + row + `.artifact_id and reading_holder.block_id = ` + holder + `
					  and reading_holder.type = '` + askType + `'
					offset 0)
				and (` + AskBlockAnsweredSQL(row+`.artifact_id`, holder, holderAddress) + ` or ` + copiedAskShownSQL(row, holder) + `))
			then '` + readingType + `' else '` + BlockSupertag + `' end
		when exists (
			select 1 from graph_nodes typed
			where typed.kind = 'supertag' and typed.id = ` + row + `.type and typed.state <> '` + graphstore.StateGone + `'
			  and coalesce((typed.attributes->>'block_type')::boolean, false)
			offset 0) then ` + row + `.type
		else '` + BlockSupertag + `' end`
}
