package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// AskedText is a document as the text routes serve it (TextWithAsks): its canonical markdown, which
// holds no server-owned attribute on any ask block; its whole-document token; its blocks, whose
// byte ranges index that markdown, for a read that asked for them; and its asks, one entry per ask
// block of that markdown, in document order, nil where the markdown holds no ask block, so the
// text routes answer such a document with no asks member, as the release before answered it.
type AskedText struct {
	Markdown string
	Token    string
	Blocks   []model.ArtifactBlock
	Asks     []model.TextAsk
}

// TextWithAsks is the document an agent reads through the text routes (GET
// /api/v1/artifacts/{id}/text and its issue and project slug variants, and so `dispatch read` and
// `doc-read`): its canonical markdown and its whole-document token, with its blocks beside them
// when withBlocks, rendered from its tree with the server-owned attributes of every ask block
// (askServerAttributeNames) taken off first, whatever the tree holds: the roll window keeps them
// in step with the asks rows while another build serves (rollWindowAskAttributes), and a document
// no write has touched since the release before holds them as that release wrote them. So the text
// an agent reads uploads back unchanged, and a ?blocks=true read's byte ranges index the markdown
// it serves. The token is the document's own, since no token weighs a server-owned attribute. A
// person's answer is held once, on the ask's node, and the text routes
// answer it beside the markdown, one entry per ask block (textAsks). A version's text, which the
// versions route serves, is the stored bytes a person approved, and none of this reads it.
//
// The tree is the read's (readDocument): the calling transaction's fork's or the resident room's,
// or the tree a cold read folded, which the read cache keeps beside its rendering for a document
// holding an ask block (documentRead.tree), so a read of a stored state the cache holds decodes
// nothing, and the cached rendering is served as it is where no ask block of it holds a
// server-owned attribute. A document a room of this process holds at its latest version is served
// that version's text without a rendering (storedText) where it holds no ask block; one that holds
// an ask block is read through the room, and where the history the room stands behind does not
// decode, the version's text is read back into a tree and served from it (storedAskedText) rather
// than failing the read. A document converted into an issue room is read from its fragment of that
// room, as TextWithToken reads it (documentReadRoom), since its own latest version no longer says
// what its room holds.
func (s *Service) TextWithAsks(ctx context.Context, artifactID string, withBlocks bool) (AskedText, error) {
	room, stamp, err := s.documentReadRoom(ctx, artifactID)
	if err != nil {
		return AskedText{}, err
	}
	var stored *AskedText
	if !withBlocks && room == artifactID && stamp == nil {
		markdown, token, served, err := s.storedText(ctx, artifactID)
		if err != nil {
			return AskedText{}, err
		}
		if served {
			if !strings.Contains(markdown, "ask{") {
				return AskedText{Markdown: markdown, Token: token}, nil
			}
			stored = &AskedText{Markdown: markdown, Token: token}
		}
	}
	read, tree, err := s.readDocumentIn(ctx, room, artifactID, stamp)
	if err != nil {
		if stored != nil && errors.Is(err, ErrDocumentUnloadable) {
			return s.storedAskedText(ctx, artifactID, *stored, err)
		}
		return AskedText{}, err
	}
	// text is the rendering of tree where the read holds one: the cache's, whose blocks are every
	// reader's.
	var text *renderedDocument
	switch {
	case read != nil:
		text = &renderedDocument{markdown: read.markdown, token: read.token, blocks: read.blocks}
		tree = read.tree
	case tree == nil:
		return AskedText{}, nil
	}
	var answer AskedText
	if pmdoc.HoldsAskBlock(tree) {
		if stripped, took := withoutAskServerAttributes(tree); took {
			tree, text = stripped, nil
		}
		if answer.Asks, err = s.textAsks(ctx, artifactID, tree); err != nil {
			return AskedText{}, err
		}
	}
	switch {
	case text == nil && withBlocks:
		rendered, err := renderDocumentTree(tree)
		if err == nil {
			err = rendered.tokenErr
		}
		if err != nil {
			return AskedText{}, err
		}
		answer.Markdown, answer.Token, answer.Blocks = rendered.markdown, rendered.token, rendered.blocks
	case text == nil:
		if answer.Markdown, answer.Token, err = renderTokenTree(tree); err != nil {
			return AskedText{}, err
		}
	case withBlocks:
		// The cached rendering's blocks are every reader's; the caller writes into its own.
		answer.Markdown, answer.Token, answer.Blocks = text.markdown, text.token, slices.Clone(text.blocks)
	default:
		answer.Markdown, answer.Token = text.markdown, text.token
	}
	return answer, nil
}

// storedAskedText is TextWithAsks for stored, the markdown and the token of the document's latest
// version, which a room of this process holds, where the history the room stands behind does not
// decode (unloadable, the read's error): the version's markdown read back into a tree
// (pmdoc.ParseRendering), rendered again only where an ask block of it held a server-owned
// attribute, with the version's own token, and the asks of that tree. A markdown that does not
// read back answers unloadable.
func (s *Service) storedAskedText(ctx context.Context, artifactID string, stored AskedText, unloadable error) (AskedText, error) {
	tree, err := pmdoc.ParseRendering(stored.Markdown)
	if err != nil {
		return AskedText{}, unloadable
	}
	if stripped, took := withoutAskServerAttributes(tree); took {
		tree = stripped
		if stored.Markdown, err = documentMarkdown(tree); err != nil {
			return AskedText{}, err
		}
	}
	if stored.Asks, err = s.textAsks(ctx, artifactID, tree); err != nil {
		return AskedText{}, err
	}
	return stored, nil
}

// textAsks is the asks member of tree, a tree of document artifactID with no server-owned
// attribute on any ask block: one entry per ask block, in document order, its answer read off the
// node of the ask that answers for it (askNodeAnswers, textAskOf). A block an ask of the document
// indexes reads that ask; a copy of an ask of another document of its owner
// (readCopiedAskSources) reads its source, and names it and its document; on a closed issue's
// document, a readable block that neither has an ask nor copies one - a block no settlement has
// indexed yet, which opens no ask while the issue is closed - reads resolved, since the close
// withdrew every ask of its issue; and every other block reads no state, a question nobody has
// opened yet, which the text routes showed with no state before they took the attributes off, and
// so does every block of a tree that repeats an ask block's id, which settlement reads no block
// under (askNodes). The asks rows say which ask indexes which block, and the node holds the
// answer, as the Decisions read's entries read it (R3): while a process of the release before
// answers an ask whose node the decisions step's catch-up has not stated that answer on yet, both
// read the ask as its node holds it. Everything is read in a transaction of the read's own that
// writes nothing and locks no row.
func (s *Service) textAsks(ctx context.Context, artifactID string, tree *pmdoc.Node) ([]model.TextAsk, error) {
	var blocks []*pmdoc.Node
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type == "ask" {
			blocks = append(blocks, node)
		}
		return true
	})
	if len(blocks) == 0 {
		return nil, nil
	}
	entries := make([]model.TextAsk, len(blocks))
	for index, node := range blocks {
		id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
		entries[index] = model.TextAsk{BlockID: id}
	}
	nodes, readable := askNodes(tree)
	if !readable || len(nodes) == 0 {
		return entries, nil
	}
	err := pgx.BeginTxFunc(ctx, s.store.Pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		held := make([]string, 0, len(nodes))
		for id := range nodes {
			held = append(held, id)
		}
		rows, err := tx.Query(ctx, `
			select `+AskColumns+`
			from asks a where `+refs.AsksOfFragmentBlocksSQL("a", "$1::uuid", "$2::text[]")+`
		`, artifactID, held)
		if err != nil {
			return fmt.Errorf("read the asks of the document's blocks: %w", err)
		}
		indexed := map[string]model.Ask{}
		for rows.Next() {
			ask, err := ScanAsk(rows)
			if err != nil {
				rows.Close()
				return fmt.Errorf("read an ask of the document's blocks: %w", err)
			}
			if ask.BlockID != nil {
				indexed[*ask.BlockID] = ask
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read the asks of the document's blocks: %w", err)
		}
		var unindexed []askBlock
		var ids []string
		for id, node := range nodes {
			if _, held := indexed[id]; held {
				continue
			}
			if block, err := parseAskBlock(node); err == nil {
				unindexed, ids = append(unindexed, block), append(ids, id)
			}
		}
		var sources map[string]copiedSource
		closed := false
		if len(unindexed) > 0 {
			owner, err := documentOwner(ctx, tx, artifactID)
			if err != nil {
				return err
			}
			if sources, err = readCopiedAskSources(ctx, tx, artifactID, owner, unindexed, ids); err != nil {
				return err
			}
			if closed, err = ownerIssueClosed(ctx, tx, owner); err != nil {
				return err
			}
		}
		shown := make([]string, 0, len(entries))
		for _, entry := range entries {
			if ask, held := indexed[entry.BlockID]; held {
				shown = append(shown, ask.ID)
			} else if source, copied := sources[entry.BlockID]; copied {
				shown = append(shown, source.ask.ID)
			}
		}
		answers, err := askNodeAnswers(ctx, tx, shown)
		if err != nil {
			return err
		}
		parsed := make(map[string]bool, len(ids))
		for _, id := range ids {
			parsed[id] = true
		}
		for index := range entries {
			id := entries[index].BlockID
			if ask, held := indexed[id]; held {
				entries[index] = textAskOf(id, answers[ask.ID], "", "")
			} else if source, copied := sources[id]; copied {
				entries[index] = textAskOf(id, answers[source.ask.ID], source.ask.ID, source.document)
			} else if closed && parsed[id] {
				// A readable block no ask indexes on a closed issue's document opens no ask until
				// the issue reopens (reconcileAskBlocks), and the close withdrew every ask of the
				// issue, so it reads resolved, as the close's sweep writes such a block while
				// another build serves (unindexedAskResolution).
				entries[index].State = asks.StateResolved
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// askNodeAnswer is what an ask's node holds of its answer: its answer state (answer_state), and
// for an answer who gave it (the answered_by relation's person), when (answered_at), the options
// chosen (selected) and the words (answer), each "" or nil where the node holds none. planned is
// whether it is what a change the reading transaction plans leaves on the node
// (plannedAskNodeAnswer), which the transaction may not have written to the node or the row yet.
type askNodeAnswer struct {
	state, answeredBy, answeredAt string
	selected                      []string
	answer                        *string
	// kept for one release: catch-up
	planned bool
}

// askNodeAnswerKinds are the tuples of an ask's node askNodeAnswers reads.
var askNodeAnswerKinds = []string{
	graphstore.TupleAnswerState, graphstore.TupleAnswer, graphstore.TupleSelected,
	graphstore.TupleAnsweredAt, graphstore.TupleAnsweredBy,
}

// askNodeAnswers reads, in tx, what the node of each ask ids names holds of its answer, by ask
// id, from the node's tuples as they stand now (graphstore.CurrentTupleRead), as the Decisions
// read's entries read them (api's decisionEntriesRead). An ask whose node holds no answer_state -
// one a process of the release before wrote, before the decisions step states it - is absent, and
// reads open (textAskOf), as an entry does. An ask whose answer state the ledger that queues tx's
// writes has planned a change of (Ledger.PlanSettle) - a reopen's restore, planned before the
// reopen writes its asks' blocks (WriteRestoredAsks, WriteIssueAskStates) - reads as that change
// leaves its node (plannedAskNodeAnswer): a block written in the same transaction shows the node
// as the commit leaves it.
func askNodeAnswers(ctx context.Context, tx pgx.Tx, ids []string) (map[string]askNodeAnswer, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `
		select k.id, f.kind, f.to_id
		from unnest($1::text[]) k (id)
		cross join lateral (`+graphstore.CurrentTupleRead(
		`t.kind, t.to_kind, case when t.to_kind = '`+graphstore.KindValue+`' then '' else t.to_id end`,
		`t.kind, t.to_kind, t.to_id, t.asserted`,
		`t.from_kind = '`+graphstore.KindAsk+`' and t.from_id = k.id and t.kind = any($2::text[])`)+`
		) f
		where f.asserted
	`, ids, askNodeAnswerKinds)
	if err != nil {
		return nil, fmt.Errorf("read the answers of the document's asks off their nodes: %w", err)
	}
	defer rows.Close()
	held := map[string]*askNodeAnswer{}
	for rows.Next() {
		var id, kind, value string
		if err := rows.Scan(&id, &kind, &value); err != nil {
			return nil, fmt.Errorf("read an answer of the document's asks off its node: %w", err)
		}
		answer := held[id]
		if answer == nil {
			answer = &askNodeAnswer{}
			held[id] = answer
		}
		switch kind {
		case graphstore.TupleAnswerState:
			answer.state = value
		case graphstore.TupleAnswer:
			answer.answer = &value
		case graphstore.TupleSelected:
			if err := json.Unmarshal([]byte(value), &answer.selected); err != nil {
				return nil, fmt.Errorf("the node of ask %s holds selected %q, which is no list of labels: %w", id, value, err)
			}
		case graphstore.TupleAnsweredAt:
			answer.answeredAt = value
		case graphstore.TupleAnsweredBy:
			answer.answeredBy = value
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the answers of the document's asks off their nodes: %w", err)
	}
	answers := make(map[string]askNodeAnswer, len(held))
	for id, answer := range held {
		if answer.state != "" {
			answers[id] = *answer
		}
	}
	if ledger := queueFor(ctx, tx); ledger != nil {
		for _, id := range ids {
			if settled, planned := ledger.settledAsks[id]; planned {
				answers[id] = plannedAskNodeAnswer(settled.Ask)
			}
		}
	}
	return answers, nil
}

// plannedAskNodeAnswer is what the node of an ask holds of its answer once a planned change of its
// answer state is written, from ask, the ask as that change leaves it (asks.Settled.Ask): the
// state, and for an answer the person, the time, the options and the words it states on the node
// (asks' answerFacts).
func plannedAskNodeAnswer(ask model.Ask) askNodeAnswer {
	held := askNodeAnswer{state: ask.State}
	// kept for one release: catch-up
	held.planned = true
	if ask.Answer != nil {
		held.answeredBy = asks.AnswererOf(*ask.Answer)
		held.answeredAt = asks.AnsweredAtValue(ask.Answer.At)
		held.selected = append([]string{}, ask.Answer.Selected...)
		held.answer = ask.Answer.Text
	}
	return held
}

// textAskOf is the entry of the ask block blockID whose ask's node holds answer, or, for a copy,
// whose source, the ask source on the document whose address is copied, holds it: under the
// names the roll window writes into the block while another build serves (rollWindowAskWrite) -
// the state, open where the node holds none; who answered, when, the options chosen (never a nil
// list) and the words, for an answered ask; and for a copy, the source ask and its document.
func textAskOf(blockID string, answer askNodeAnswer, source, copied string) model.TextAsk {
	entry := model.TextAsk{BlockID: blockID, State: answer.state}
	if entry.State == "" {
		entry.State = asks.StateOpen
	}
	if copied != "" {
		entry.CopiedFrom, entry.CopiedFromDocument = source, copied
	}
	if entry.State == asks.StateAnswered {
		selected := append([]string{}, answer.selected...)
		entry.AnsweredBy, entry.AnsweredAt = answer.answeredBy, answer.answeredAt
		entry.Selected, entry.Answer = &selected, answer.answer
	}
	return entry
}

// ownerIssueClosed reports whether owner is an issue whose row reads closed, read in tx without a
// lock; a project document's owner is never closed.
func ownerIssueClosed(ctx context.Context, tx pgx.Tx, owner artifactOwner) (bool, error) {
	if owner.IssueKey == nil {
		return false, nil
	}
	var closed bool
	if err := tx.QueryRow(ctx, `select closed_at is not null from issues where key = $1`, *owner.IssueKey).Scan(&closed); err != nil {
		return false, fmt.Errorf("read whether the document's issue is closed: %w", err)
	}
	return closed, nil
}

// withAskBlocks is a copy of tree in which each ask block is the node show answers for it, where
// show says it changed one: each container on the way to such a block is a fresh node, and every
// other node is the tree's own. A tree none of whose ask blocks show changes is answered as it is.
func withAskBlocks(tree *pmdoc.Node, show func(*pmdoc.Node) (*pmdoc.Node, bool)) *pmdoc.Node {
	next, _ := withAskBlocksChanged(tree, show)
	return next
}

func withAskBlocksChanged(tree *pmdoc.Node, show func(*pmdoc.Node) (*pmdoc.Node, bool)) (*pmdoc.Node, bool) {
	node, changed := tree, false
	if tree.Type == "ask" {
		node, changed = show(tree)
	}
	if tree.Type == "text" || len(tree.Children) == 0 {
		return node, changed
	}
	var children []*pmdoc.Node
	for index, child := range tree.Children {
		next, took := withAskBlocksChanged(child, show)
		if !took {
			continue
		}
		if children == nil {
			children = append([]*pmdoc.Node(nil), tree.Children...)
		}
		children[index] = next
	}
	if children == nil {
		return node, changed
	}
	if !changed {
		copied := *tree
		node = &copied
	}
	node.Children = children
	return node, true
}
