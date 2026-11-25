package docs

// person_kept.go is the one rule that R32 of the issue tree's spec reads on every path that
// writes a person's decision: who is a person (CallerIsPerson), which node of the store is a
// person's decision (PersonDecisionOf), and what a text write does to one (typedBlockChanges),
// which the edit routes and the upload refuse to an agent (refuseDroppedAnsweredAsks). An agent
// cannot change, move or remove a person's decision, and it can move, replace or delete any other
// node a person wrote. A person's decision is an answered ask - its own node, `ask/<uuid>`, and the
// `:::ask` block that holds it - and a `decision` block whose current decided_by names a person. A
// person's Keep of an agent's decision (kept_by) makes it no person's decision: its decided_by
// still names the session that wrote it.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// CallerIsPerson reports whether the caller whose credential is proving is a person, for R32 and
// for R7: its credential holds the person scope (model.ScopePerson). A browser's cookie holds it,
// and so does a token that holds it: one minted with no scopes named, which takes the scopes of
// the credential that minted it, where that is the browser or a personal token that holds person
// and approve; one minted with person among its named scopes, or changed to hold it; and one the
// secrets broker scoped to its person. An agent that runs under such a token acts as that person.
// Every other caller is an agent, the deployment's shared token among them. A document write reads
// the same test off its context (writtenByPerson), which the route holding the credential marks
// (WithPersonScope).
func CallerIsPerson(proving model.Actor) bool {
	return proving.Credential.Holds(model.ScopePerson)
}

// AskOfBlockSQL answers the id of the ask that holds the question's state for the `:::ask` block
// whose node the store keys as $1 and $2. It looks up document_blocks by
// document_blocks_node_address (migration 20261111043000) and joins the block's own ask by
// document and block id, matching the block id keys of asks_block_id_unique (migration 0127). Where
// the block has no ask of its own, it reads the source ask from the block's current asserted
// copied_from tuple (graphstore.CurrentTupleRead). It answers null for an ask block with neither and
// no row for any other node. The state route reads it for the ask that holds a block's state (api's
// askHoldsState), and PersonDecisionOf for whether that ask is answered.
var AskOfBlockSQL = `
	select coalesce(k.id::text, copied.to_id)
	from document_blocks b
	left join asks k on k.block_artifact_id = b.artifact_id
		and block_id_key(k.block_id) = block_id_key(b.block_id) and k.block_id = b.block_id
	left join lateral (
		select c.to_id
		from (` + graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_id, t.asserted, t.seq`,
	`t.from_kind = b.node_kind and t.from_id = b.node_address and t.kind = '`+graphstore.TupleCopiedFrom+`'
			and t.to_kind = '`+graphstore.KindAsk+`'`) + `) c
		where k.id is null and c.asserted
		order by c.seq desc limit 1
	) copied on true
	where b.node_kind = $1 and b.node_address = $2 and b.type = 'ask'`

// decidedByPersonSQL is true of the node whose stored kind and id are the SQL expressions kind and
// id while its current decided_by names a person: the newest tuple to each far end, kept while it
// asserts (graphstore.CurrentTupleRead), of which one runs to a person's node.
func decidedByPersonSQL(kind, id string) string {
	return `exists (select 1 from (` + graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_kind, t.asserted`,
		`t.from_kind = `+kind+` and t.from_id = `+id+` and t.kind = '`+graphstore.TupleDecidedBy+`' and t.to_kind <> '`+graphstore.KindValue+`'`) +
		`) held where held.asserted and held.to_kind = '` + graphstore.KindPerson + `')`
}

// blockTextSQL is the newest text value of the block node whose stored kind and id are the SQL
// expressions kind and id, null where it holds none or its newest retracts it: the block's slice of
// the version its index last read (refs' AttributeText).
func blockTextSQL(kind, id string) string {
	return `(select case when t.asserted then t.to_id end from graph_tuples t
		 where t.from_kind = ` + kind + ` and t.from_id = ` + id + ` and t.kind = '` + graphstore.AttributeText + `' and t.to_kind = '` + graphstore.KindValue + `'
		 order by t.seq desc limit 1)`
}

// PersonDecision is a person's decision as a refusal of R32 names it: What, the words that name
// its kind ("the ask", "the :::ask block" or "the decision"), and Words, the ask's question or the
// decision's words (decisionQuote), which the refusal quotes.
type PersonDecision struct {
	What, Words string
}

// PersonDecisionOf reads whether the node the store keys as kind and id is a person's decision
// (R32), and which: an answered ask's own node, `ask/<uuid>`, whose id is the ask's, or a block of
// a document that is one (PersonDecisionOfBlock), read with its type off its document_blocks row.
// An ask is answered as its row holds it, as every read that decides a write reads it. Any other
// node is no person's decision, and a kind that holds none - neither an ask nor a block of a
// document, a finding of a lens among them (graphstore.AddressKind) - is answered so with no
// statement. q is the write's transaction, so the answer is the one the write is made against.
func PersonDecisionOf(ctx context.Context, q Queryer, kind, id string) (PersonDecision, bool, error) {
	switch graphstore.AddressKind(kind, id) {
	case graphstore.KindAsk:
		var question string
		err := q.QueryRow(ctx, `select coalesce(question, '') from asks where id = $1::uuid and state = '`+asks.StateAnswered+`'`, id).Scan(&question)
		if errors.Is(err, pgx.ErrNoRows) {
			return PersonDecision{}, false, nil
		}
		if err != nil {
			return PersonDecision{}, false, fmt.Errorf("read whether the ask %s is answered: %w", id, err)
		}
		return PersonDecision{What: "the ask", Words: question}, true, nil
	case refs.BlockNodeKind:
	default:
		return PersonDecision{}, false, nil
	}
	var typ string
	err := q.QueryRow(ctx, `select type from document_blocks where node_kind = $1 and node_address = $2 limit 1`, kind, id).Scan(&typ)
	if errors.Is(err, pgx.ErrNoRows) {
		return PersonDecision{}, false, nil
	}
	if err != nil {
		return PersonDecision{}, false, fmt.Errorf("read the type of %s: %w", id, err)
	}
	return PersonDecisionOfBlock(ctx, q, kind, id, typ)
}

// PersonDecisionOfBlock reads whether the block of a document whose node the store keys as kind
// and id, and whose document_blocks row holds the type typ, is a person's decision (R32): an
// `:::ask` block whose ask - the block's own, or for a copy its source (AskOfBlockSQL) - is
// answered, named "the :::ask block" and quoted by its question; or a decision whose current
// decided_by names a person, named "the decision" and quoted by its words, the text its node holds
// (decisionQuote). A block of any other type is none, with no statement, and so is an ask block
// whose ask is open or that no ask holds. A caller that read the block's type already, as the node
// routes do, asks here with it.
func PersonDecisionOfBlock(ctx context.Context, q Queryer, kind, id, typ string) (PersonDecision, bool, error) {
	switch typ {
	case "ask":
		var question string
		err := q.QueryRow(ctx, `
			select coalesce(k.question, '') from (`+AskOfBlockSQL+`) block (ask)
			join asks k on k.id::text = block.ask
			where k.state = '`+asks.StateAnswered+`'`, kind, id).Scan(&question)
		if errors.Is(err, pgx.ErrNoRows) {
			return PersonDecision{}, false, nil
		}
		if err != nil {
			return PersonDecision{}, false, fmt.Errorf("read the ask of the block %s: %w", id, err)
		}
		return PersonDecision{What: "the :::ask block", Words: question}, true, nil
	case "decision":
		var decided bool
		var words *string
		if err := q.QueryRow(ctx, `select `+decidedByPersonSQL("$1", "$2")+`, `+blockTextSQL("$1", "$2"), kind, id).Scan(&decided, &words); err != nil {
			return PersonDecision{}, false, fmt.Errorf("read who decided %s: %w", id, err)
		}
		if !decided {
			return PersonDecision{}, false, nil
		}
		var quoted string
		if words != nil {
			quoted = decisionQuote(*words)
		}
		return PersonDecision{What: "the decision", Words: quoted}, true, nil
	}
	return PersonDecision{}, false, nil
}

// decisionQuote is a decision block's words as R32's refusal quotes them, read off markdown that
// renders the block: its text without its opening and closing fence lines, each line trimmed and
// the lines that hold anything joined by spaces.
func decisionQuote(markdown string) string {
	lines := strings.Split(strings.TrimSpace(markdown), "\n")
	if len(lines) > 0 && strings.HasPrefix(strings.TrimSpace(lines[0]), ":::") {
		lines = lines[1:]
	}
	if len(lines) > 0 && strings.HasPrefix(strings.TrimSpace(lines[len(lines)-1]), ":::") {
		lines = lines[:len(lines)-1]
	}
	for index, line := range lines {
		lines[index] = strings.TrimSpace(line)
	}
	return strings.Join(slices.DeleteFunc(lines, func(line string) bool { return line == "" }), " ")
}

// typedChange is what a text write does to one ask or decision block of the document it starts
// from: it takes the block out of the document where it neither retypes nor rewords it, which is
// also the one way a text write moves the block out of the spec or the project document holding
// it; retypedTo is the type it gives the block instead; and reworded is a write that keeps the
// block's type and changes its words (typedWords). words is a decision's words before the write,
// as the refusal quotes them (decisionQuote); an ask is quoted by its row's question instead.
type typedChange struct {
	retypedTo string
	reworded  bool
	words     string
}

// typedChanges is what a text write does to the asks and the decisions of the document it starts
// from, by block id: an ask by the id its ask row keys its block by, the block's own, and a
// decision by the id the block index keys it by (refs.TypedBlockID). A block the write leaves as it
// was is in neither.
type typedChanges struct {
	asks, decisions map[string]typedChange
}

// typedBlockChanges is what the write from before to after does to each ask and decision block
// before holds (typedChange), the first block of each id. A block after holds at the same id and
// type, wherever it stands - moved among its siblings, into a callout or a list item or out of one
// - is changed only where its words are: a reading it closes with is no word of it, nor is a
// decision's `shaped`, so a reading inserted into it, or a retype that writes `shaped` and keeps
// its words, changes nothing. A block after holds as before held it, attribute for attribute and
// child for child (pmdoc's EqualWithBlockIDs), is unchanged with no rendering. A nil before holds no
// block.
func typedBlockChanges(before, after *pmdoc.Node) (typedChanges, error) {
	var changes typedChanges
	held := map[string]*pmdoc.Node{}
	pmdoc.Walk(before, func(node *pmdoc.Node) bool {
		if node.Type != "ask" && node.Type != "decision" {
			return true
		}
		if id := ownBlockID(node); id != "" {
			if _, repeated := held[id]; !repeated {
				held[id] = node
			}
		}
		return true
	})
	if len(held) == 0 {
		return changes, nil
	}
	written := make(map[string]*pmdoc.Node, len(held))
	pmdoc.Walk(after, func(node *pmdoc.Node) bool {
		id := ownBlockID(node)
		if _, wanted := held[id]; wanted {
			if _, seen := written[id]; !seen {
				written[id] = node
			}
		}
		return true
	})
	for id, was := range held {
		now := written[id]
		var change typedChange
		var words string
		switch {
		case now == nil:
		case now.Type != was.Type:
			change.retypedTo = now.Type
		case now.EqualWithBlockIDs(was):
			continue
		default:
			var err error
			if words, err = typedWords(was); err != nil {
				return typedChanges{}, err
			}
			rewritten, err := typedWords(now)
			if err != nil {
				return typedChanges{}, err
			}
			if rewritten == words {
				continue
			}
			change.reworded = true
		}
		if was.Type == "ask" {
			if changes.asks == nil {
				changes.asks = map[string]typedChange{}
			}
			changes.asks[id] = change
			continue
		}
		if words == "" {
			var err error
			if words, err = typedWords(was); err != nil {
				return typedChanges{}, err
			}
		}
		change.words = decisionQuote(words)
		if changes.decisions == nil {
			changes.decisions = map[string]typedChange{}
		}
		changes.decisions[refs.TypedBlockID(was)] = change
	}
	return changes, nil
}

// typedWords is an ask's or a decision's own words: refs.AskWords for an ask, refs.DecisionWords
// for a decision, the words their sums cover.
func typedWords(block *pmdoc.Node) (string, error) {
	if block.Type == "ask" {
		return refs.AskWords(block)
	}
	return refs.DecisionWords(block)
}

// standingChange is what a write that leaves after does to the block of type typ the block index
// holds at id, for a repair, whose document reads no tree to say what it held before (a nil
// before, refuseDroppedAnsweredAsks): its removal where after holds no block of that id, read by
// idOf, its retype where after holds one of another type, and nothing where it holds one of that
// type, whose words the repair has no tree to compare with. blocks is after's blocks by idOf, read
// once by the caller (blocksByID).
func standingChange(blocks map[string]*pmdoc.Node, id, typ string) (typedChange, bool) {
	now, held := blocks[id]
	switch {
	case !held:
		return typedChange{}, true
	case now.Type != typ:
		return typedChange{retypedTo: now.Type}, true
	}
	return typedChange{}, false
}

// blocksByID is each block of tree by the id idOf reads off it, the first block of each id; a
// block with no id is left out.
func blocksByID(tree *pmdoc.Node, idOf func(*pmdoc.Node) string) map[string]*pmdoc.Node {
	blocks := map[string]*pmdoc.Node{}
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if id := idOf(node); id != "" {
			if _, repeated := blocks[id]; !repeated {
				blocks[id] = node
			}
		}
		return true
	})
	return blocks
}

// ownBlockID is the id a block carries in its tree, as an ask row keys the block of its ask.
func ownBlockID(node *pmdoc.Node) string {
	id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
	return id
}

// keptAsksOf is each answered ask of the document artifactID whose block a write changes, in the
// order of their block ids, as ErrAnsweredAskKept names it: the asks of the blocks changed names,
// or for a standing read, a repair's, each answered ask whose block the block index holds as an
// ask block, which the write changes where standingChange says it does over after. The asks are
// read with the address of the document holding each block (refs.AskBlockDocumentSQL), in the one
// statement, so the refusal names each ask's node and its block's address and quotes its question.
func keptAsksOf(ctx context.Context, q Queryer, artifactID string, changed map[string]typedChange, standing bool, after *pmdoc.Node) ([]KeptAsk, error) {
	rows, err := q.Query(ctx, `
		select k.id::text, k.block_id, k.question, k.issue_key,
		       d.issue_key, coalesce(d.project_key, ''), d.session_id, d.slug, d.kind, d.is_primary
		from asks k
		join artifacts d on d.id = `+refs.AskBlockDocumentSQL("k")+`
		where `+documentAsksOf(artifactID).predicate("k", "$1::uuid")+` and k.state = '`+asks.StateAnswered+`' and k.block_id is not null
		  and (($2 and exists (
				select 1 from document_blocks b
				where b.artifact_id = k.block_artifact_id and b.block_id = k.block_id and b.type = 'ask'))
		    or (not $2 and k.block_id = any($3::text[])))
		order by k.block_id
	`, artifactID, standing, slices.Collect(maps.Keys(changed)))
	if err != nil {
		return nil, fmt.Errorf("read answered ask blocks: %w", err)
	}
	defer rows.Close()
	var blocks map[string]*pmdoc.Node
	if standing {
		blocks = blocksByID(after, ownBlockID)
	}
	var kept []KeptAsk
	for rows.Next() {
		var askID, blockID, question, project, slug, kind string
		var askIssue, documentIssue, session *string
		var primary bool
		if err := rows.Scan(&askID, &blockID, &question, &askIssue, &documentIssue, &project, &session, &slug, &kind, &primary); err != nil {
			return nil, fmt.Errorf("scan answered ask block: %w", err)
		}
		change, changes := changed[blockID]
		if standing {
			change, changes = standingChange(blocks, blockID, "ask")
		}
		if !changes {
			continue
		}
		ask := text.Ref{Project: project, Document: slug, ID: askID}
		if askIssue != nil {
			ask.IssueKey = *askIssue
		}
		kept = append(kept, KeptAsk{
			Ask:       refs.AskRef(ask),
			Question:  question,
			Block:     refs.BlockRef(refs.ArtifactRef(documentIssue, project, session, slug, kind, primary), blockID),
			RetypedTo: change.retypedTo,
			Reworded:  change.reworded,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read answered ask blocks: %w", err)
	}
	return kept, nil
}

// keptDecisionsOf is each decision of the document artifactID that a person decided - whose
// block's node's current decided_by names a person (decidedByPersonSQL) - that a write changes, in
// the order of their block ids, as ErrAnsweredAskKept names it: the decisions changed names, by the
// id the block index keys them by, or for a standing read, a repair's, each such decision the
// block index holds, which the write changes where standingChange says it does over after, quoted
// by the text its node holds, since no tree says what its words were. The document's blocks are
// read where the block index keys them (refs.DocumentRoomBlocksSQL), its room's rows for a document
// of an issue room, with the document's address, so the refusal names each decision's block.
func keptDecisionsOf(ctx context.Context, q Queryer, artifactID string, changed map[string]typedChange, standing bool, after *pmdoc.Node) ([]KeptDecision, error) {
	rows, err := q.Query(ctx, `
		select b.block_id, case when $2 then `+blockTextSQL("b.node_kind", "b.node_address")+` end,
		       d.issue_key, coalesce(d.project_key, ''), d.session_id, d.slug, d.kind, d.is_primary
		from document_blocks b
		join artifacts d on d.id = $1::uuid
		where `+refs.DocumentRoomBlocksSQL("b", "$1::uuid")+` and b.type = 'decision' and b.node_address is not null
		  and ($2 or b.block_id = any($3::text[]))
		  and `+decidedByPersonSQL("b.node_kind", "b.node_address")+`
		order by b.block_id
	`, artifactID, standing, slices.Collect(maps.Keys(changed)))
	if err != nil {
		return nil, fmt.Errorf("read the decisions a person decided: %w", err)
	}
	defer rows.Close()
	var blocks map[string]*pmdoc.Node
	if standing {
		blocks = blocksByID(after, refs.TypedBlockID)
	}
	var kept []KeptDecision
	for rows.Next() {
		var blockID, project, slug, kind string
		var held, documentIssue, session *string
		var primary bool
		if err := rows.Scan(&blockID, &held, &documentIssue, &project, &session, &slug, &kind, &primary); err != nil {
			return nil, fmt.Errorf("scan a decision a person decided: %w", err)
		}
		change, changes := changed[blockID]
		if standing {
			change, changes = standingChange(blocks, blockID, "decision")
			if held != nil {
				change.words = decisionQuote(*held)
			}
		}
		if !changes {
			continue
		}
		kept = append(kept, KeptDecision{
			Block:     refs.BlockRef(refs.ArtifactRef(documentIssue, project, session, slug, kind, primary), blockID),
			Words:     change.words,
			RetypedTo: change.retypedTo,
			Reworded:  change.reworded,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the decisions a person decided: %w", err)
	}
	return kept, nil
}
