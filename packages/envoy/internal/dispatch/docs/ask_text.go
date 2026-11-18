package docs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// TextWithAnswers is the document an agent reads through the text routes (GET
// /api/v1/artifacts/{id}/text and its issue and project slug variants, and so `dispatch read` and
// `doc-read`): its canonical markdown and its whole-document token, with its blocks beside them
// when withBlocks, as TextWithToken and TextWithTokenAndBlocks answer them, but with each ask block
// carrying its ask's answer state and answer, rendered on read from the ask (askAnswersShown). A
// person's answer is held once, on the ask's node (dispatch://INFRA-1150 R3), and the stored bytes
// hold none of it, so an agent reads what was decided where it reads the question. The markdown
// and the blocks come from one rendering, so the blocks' byte ranges index the markdown answered;
// the tokens are the document's own, since no token weighs a server-owned attribute. A document
// holding no ask block is answered as those two read it.
//
// The answers are rendered on the tree of the read that served the text (readDocument): the tree
// of the calling transaction's fork or of the resident room, or the tree a cold read folded, which
// the read cache keeps beside its rendering for a document holding an ask block
// (documentRead.tree). So an agent's read of a document no room holds folds its stored history
// once a stored state, as every other read of it does, and a read of a stored state the cache
// holds decodes nothing. A document a room of this process holds at its latest version is served
// that version's text without a rendering (storedText) where it holds no ask block; one that holds
// an ask block is read through the room, and where the history the room stands behind does not
// decode, the version's text is served without the answers rather than failing the read. A
// document converted into an issue room is read from its fragment of that room, as TextWithToken
// reads it (documentReadRoom), since its own latest version no longer says what its room holds.
//
// What the text routes answer is what an upload of it sends back, so it is held to what one upload
// may hold as every write is (growth.go): an answer is caller text of up to
// contracts.DispatchEventBodyMax units, and a document's answers rendered beside its text could
// pass the bound its own text keeps. Where they would, each answer is given in document order
// while the document has room for it, and a block whose answer has no room says who answered and
// when, without the words or the options chosen (answersWithRoom), as the release before wrote
// such a block; the ask keeps the answer whole.
func (s *Service) TextWithAnswers(ctx context.Context, artifactID string, withBlocks bool) (string, string, []model.ArtifactBlock, error) {
	room, stamp, err := s.documentReadRoom(ctx, artifactID)
	if err != nil {
		return "", "", nil, err
	}
	var stored *renderedDocument
	if !withBlocks && room == artifactID && stamp == nil {
		markdown, token, served, err := s.storedText(ctx, artifactID)
		if err != nil {
			return "", "", nil, err
		}
		if served {
			if !strings.Contains(markdown, "ask{") {
				return markdown, token, nil, nil
			}
			stored = &renderedDocument{markdown: markdown, token: token}
		}
	}
	read, tree, err := s.readDocumentIn(ctx, room, artifactID, stamp)
	if err != nil {
		if stored != nil && errors.Is(err, ErrDocumentUnloadable) {
			return stored.markdown, stored.token, nil, nil
		}
		return "", "", nil, err
	}
	var text renderedDocument
	switch {
	case read != nil:
		text = renderedDocument{markdown: read.markdown, token: read.token, blocks: read.blocks}
		if tree == nil {
			tree = read.tree
		}
	case tree == nil:
		return "", "", nil, nil
	case withBlocks:
		if text, err = renderDocumentTree(tree); err == nil {
			err = text.tokenErr
		}
	default:
		text.markdown, text.token, err = renderTokenTree(tree)
	}
	if err != nil {
		return "", "", nil, err
	}
	if tree != nil && strings.Contains(text.markdown, "ask{") {
		answered, err := s.answersRendered(ctx, artifactID, tree, text.markdown)
		if err != nil {
			return "", "", nil, err
		}
		if answered != nil {
			text = *answered
			read = nil
		}
	}
	switch {
	case !withBlocks:
		return text.markdown, text.token, nil, nil
	case read != nil:
		// The cached rendering's blocks are every reader's; the caller writes into its own.
		return text.markdown, text.token, slices.Clone(read.blocks), nil
	}
	return text.markdown, text.token, text.blocks, nil
}

// answersRendered is tree, the document whose canonical markdown is markdown, rendered with each
// ask block carrying its ask's answer state and answer (askAnswersShown), each answer while the
// document has room for it (answersWithRoom); nil where no block of it changes.
func (s *Service) answersRendered(ctx context.Context, artifactID string, tree *pmdoc.Node, markdown string) (*renderedDocument, error) {
	shown, answered, err := s.askAnswersShown(ctx, artifactID, tree)
	if err != nil || shown == tree {
		return nil, err
	}
	rendered, err := renderDocumentTree(shown)
	if err == nil && len(answered) > 0 && weighRendering(markdown, rendered.markdown) != nil {
		rendered, err = answersWithRoom(markdown, shown, answered)
	}
	if err != nil {
		return nil, err
	}
	if rendered.tokenErr != nil {
		return nil, rendered.tokenErr
	}
	return &rendered, nil
}

// answerWords are the attributes of an ask block that carry an answer's caller text: the options
// it selects, whose labels are the asker's text, and its words. Who answered and when are the
// server's, and stay on a block whose answer has no room.
var answerWords = [...]string{"selected", "answer"}

// answersWithRoom is shown, the document whose own canonical markdown is markdown with its ask
// blocks answered (askAnswersShown), rendered with each answer of answered - the shown blocks
// carrying one, in document order, each a node of shown's own - given back while the document
// still has room for it, weighed as a write is (weigh): all are taken out, the document is
// rendered once without them, and each is weighed by what it adds to its block's own rendering,
// since an answer lengthens its block's directive line and makes no element. A block whose answer
// has no room keeps its state, its answerer and its time.
func answersWithRoom(markdown string, shown *pmdoc.Node, answered []*pmdoc.Node) (renderedDocument, error) {
	held := make([][len(answerWords)]any, len(answered))
	for index, node := range answered {
		for word, name := range answerWords {
			held[index][word] = node.Attrs[name]
			delete(node.Attrs, name)
		}
	}
	left, err := renderTree(shown)
	if err != nil {
		return renderedDocument{}, err
	}
	document, was := renderingOf(left), renderingOf(markdown)
	for index, node := range answered {
		alone := &pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{node}}
		withheld, err := renderTree(alone)
		if err != nil {
			return renderedDocument{}, err
		}
		for word, name := range answerWords {
			if value := held[index][word]; value != nil {
				node.Attrs[name] = value
			}
		}
		given, err := renderTree(alone)
		if err != nil {
			return renderedDocument{}, err
		}
		grown := rendering{bytes: document.bytes + len(given) - len(withheld), measured: document.measured}
		if weigh(was, grown) != nil {
			for _, name := range answerWords {
				delete(node.Attrs, name)
			}
			continue
		}
		document = grown
	}
	return renderDocumentTree(shown)
}

// askAnswersShown is a copy of tree, a tree of document artifactID, in which each ask block an ask
// of the document indexes, and each copy of an ask of another document of its owner
// (readCopiedAskSources), carries that ask's answer state and answer as the release before wrote
// them into the block (rollWindowAskAttributes): its state, who answered, when, what they chose
// and wrote, and a copy's source and the source's document. On a closed issue's document, each
// readable block that neither has an ask nor copies one - a block no settlement has indexed yet,
// which opens no ask while the issue is closed - reads state="resolved", as the close's sweep
// wrote such a block before the state left the document. The rows are read in a transaction of
// the read's own that writes nothing and locks no row; the asks row is the projection of its node,
// written from the node's tuples in the transaction that appends them (asks.Settle). A tree none
// of whose blocks changes is answered as it is. Beside the copy it answers the blocks of the copy
// it gave an answer's words or selection (answerWords), each a fresh node of the copy's own, in
// document order.
func (s *Service) askAnswersShown(ctx context.Context, artifactID string, tree *pmdoc.Node) (*pmdoc.Node, []*pmdoc.Node, error) {
	nodes, readable := askNodes(tree)
	if !readable || len(nodes) == 0 {
		return tree, nil, nil
	}
	var shown *pmdoc.Node
	var answered []*pmdoc.Node
	err := pgx.BeginTxFunc(ctx, s.store.Pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			select `+AskColumns+`
			from asks a where a.block_artifact_id = $1 and a.block_id is not null
		`, artifactID)
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
		parsed := make(map[string]bool, len(ids))
		for _, id := range ids {
			parsed[id] = true
		}
		shown = withAskBlocks(tree, func(node *pmdoc.Node) (*pmdoc.Node, bool) {
			id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
			if nodes[id] != node {
				return node, false
			}
			write := func(*pmdoc.Node) bool { return false }
			if ask, held := indexed[id]; held {
				write = rollWindowAskAttributes(true, ask, "")
			} else if source, copied := sources[id]; copied {
				write = rollWindowAskAttributes(true, source.ask, source.document)
			} else if closed && parsed[id] {
				// A readable block no ask indexes on a closed issue's document opens no ask until
				// the issue reopens (reconcileAskBlocks), and the close withdrew every ask of the
				// issue, so it reads resolved and nothing else, as the close's sweep writes such a
				// block while another build serves (resolveUnindexedAskBlock).
				write = rollWindowAskAttributes(true, model.Ask{State: "resolved"}, "")
			}
			written := attributesOf(node)
			if !write(written) {
				return node, false
			}
			copied := *node
			copied.Attrs = written.Attrs
			if holdsAnswerWords(&copied) {
				answered = append(answered, &copied)
			}
			return &copied, true
		})
		return nil
	})
	return shown, answered, err
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
