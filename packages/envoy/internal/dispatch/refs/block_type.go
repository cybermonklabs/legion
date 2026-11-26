package refs

import (
	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

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
// address); and of either, whether its node's newest answer_state value is answered, as docs'
// askAnswers reads it, where a node that holds none reads as open. Each probe stands behind an
// offset 0, so a statement over many blocks probes each one's ask by its key.
func AskBlockAnsweredSQL(artifact, block, address string) string {
	answered := func(ask string) string {
		return `coalesce((select answer.asserted and answer.to_id = ` + store.QuoteLiteral(asks.StateAnswered) + ` from graph_tuples answer
			where answer.from_kind = '` + graphstore.KindAsk + `' and answer.from_id = ` + ask + `
			  and answer.kind = '` + graphstore.TupleAnswerState + `' and answer.to_kind = '` + graphstore.KindValue + `'
			order by answer.seq desc limit 1), false)`
	}
	return `(exists (
			select 1 from asks shown
			where shown.block_artifact_id = ` + artifact + ` and shown.block_id = ` + block + `
			  and ` + answered("shown.id::text") + `
			offset 0)
		or exists (
			select 1 from (` + graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_id, t.asserted`,
		`t.from_kind = '`+BlockNodeKind+`' and t.from_id = `+address+` and t.kind = '`+graphstore.TupleCopiedFrom+`'
				  and t.to_kind = '`+graphstore.KindAsk+`'`) + `) copied
			where copied.asserted and ` + answered("copied.to_id") + `
			offset 0))`
}

// BlockTypeSupertagSQL is the supertag the node of the document_blocks row aliased row carries
// (R26 of the issue-tree spec), as SQL text: its type, for a typed block of the block
// schema - a type whose supertag node the store holds present and marks a block type, the nodes the
// installed schema is read from (pmdoc.IsTypedBlock, TypeSupertag), probed by graph_nodes' primary
// key - and BlockSupertag for every other block; but for a reading, which carries its type only
// where the typed-node pass takes it for one - in a decision, or in an ask block whose ask holds an
// answer (AskBlockAnsweredSQL) - read off the block its row's parent_block_id names, which for a
// reading in an answered ask is still the ask block holding it, and BlockSupertag otherwise. It is
// the census's reading of the supertag a row's node must carry (graphcensus's block_type).
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
				and ` + AskBlockAnsweredSQL(row+`.artifact_id`, holder, holderAddress) + `)
			then '` + readingType + `' else '` + BlockSupertag + `' end
		when exists (
			select 1 from graph_nodes typed
			where typed.kind = 'supertag' and typed.id = ` + row + `.type and typed.state <> '` + graphstore.StateGone + `'
			  and coalesce((typed.attributes->>'block_type')::boolean, false)
			offset 0) then ` + row + `.type
		else '` + BlockSupertag + `' end`
}
