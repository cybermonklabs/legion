package docs

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// askReading is one block an ask block closes with (pmdoc.TypedContent): a reading, which reads
// the decision a person's answer made. ask is the ask block's id, reading the block's own, and
// kind its type, which a refusal names it by.
type askReading struct {
	ask     string
	reading string
	kind    string
}

// askReadings is every block tree's ask blocks close with, in document order.
func askReadings(tree *pmdoc.Node) []askReading {
	var found []askReading
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type == "ask" {
			found = append(found, closingReadings(node)...)
		}
		return true
	})
	return found
}

// closingReadings is the blocks ask, an ask block, closes with, in order.
func closingReadings(ask *pmdoc.Node) []askReading {
	_, admitted := pmdoc.TypedContent(ask)
	if len(admitted) == 0 {
		return nil
	}
	id, _ := ask.Attrs[pmdoc.BlockIDAttr].(string)
	readings := make([]askReading, len(admitted))
	for index, reading := range admitted {
		readingID, _ := reading.Attrs[pmdoc.BlockIDAttr].(string)
		readings[index] = askReading{ask: id, reading: readingID, kind: reading.Type}
	}
	return readings
}

// askAnswer is what the store holds of the ask one ask block shows: the ask's id, its
// `dispatch://` address, which a refusal names ("" for an ask no address names, one on an agent
// conversation's document), whether its row names its node, and whether a person answered it.
type askAnswer struct {
	id       string
	address  string
	named    bool
	answered bool
}

// readingAnswers decides, for each of blockIDs, ask blocks of one document, whether the ask the
// block shows holds an answer, by block id. It is the one decision a reading inside an ask block
// depends on, on settlement (flagReadingsWithoutAnswers) and on every write that takes markdown
// (refuseReadingsWithoutAnswers) alike, and it is made once the block's ask is resolved as
// settlement resolves it (reconcileAskBlocks), so a reading meets the ask it stands in once the
// document settles:
//
//   - A block an ask of its own document indexes (rows) shows that ask. One settlement retracted
//     when its block left (settlementRetracted) is restored when the block comes back, before
//     anything reads it again, so it shows the state the restoration gives it
//     (stateBeforeRetraction): an ask retracted while it held an answer holds that answer again.
//   - A block no ask of its document indexes, copied from an ask of another document of its owner
//     (sources, copiedAskSources), opens none and shows its source's state, so it holds an answer
//     where its source does.
//   - Any other block opens an ask of its own once settled, which holds no answer, and has none
//     here.
//
// Whether the ask a block shows holds an answer otherwise is what its node says (askAnswers).
func readingAnswers(ctx context.Context, tx pgx.Tx, blockIDs []string, rows map[string]model.Ask, sources map[string]copiedSource) (map[string]askAnswer, error) {
	shown := make(map[string]string, len(blockIDs))
	restored := map[string]bool{}
	ids := make([]string, 0, len(blockIDs))
	for _, blockID := range blockIDs {
		if ask, own := rows[blockID]; own {
			if settlementRetracted(ask) {
				prior, err := stateBeforeRetraction(ctx, tx, ask.ID)
				if err != nil {
					return nil, err
				}
				restored[blockID] = prior.State == asks.StateAnswered
			}
			shown[blockID] = ask.ID
		} else if source, copied := sources[blockID]; copied {
			shown[blockID] = source.ask.ID
		} else {
			continue
		}
		ids = append(ids, shown[blockID])
	}
	if len(ids) == 0 {
		return nil, nil
	}
	held, err := askAnswers(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	answers := make(map[string]askAnswer, len(shown))
	for blockID, id := range shown {
		answer, found := held[id]
		if !found {
			continue
		}
		if answered, retracted := restored[blockID]; retracted {
			answer.answered = answered
		}
		answers[blockID] = answer
	}
	return answers, nil
}

// askAnswers reads what the store holds now of each ask ids names, by ask id: its address,
// whether its row names its node, and whether it holds an answer, which it reads as every ask read
// lays the answer state over the row (asks.NodeStateOf), decoded as every ask read decodes it
// (asks.NodeAnswer.Read): the newest answer_state value of the ask's node (asks.NodeValueOf),
// which asks.Settle appends, or open where a node the row names holds none; and the row's own
// state where the row names no node - an ask a process of the release before wrote, before the
// boot step marks it, and one whose relations are past the store's bound (graphstore.AskFacts) -
// whose answer Settle writes on the row alone. The asks row names the ask and its document's
// address beside it.
func askAnswers(ctx context.Context, tx pgx.Tx, ids []string) (map[string]askAnswer, error) {
	rows, err := tx.Query(ctx, `
		select k.id::text, `+asks.NodeStateOf("k")+`,
		       k.issue_key, a.project_key, coalesce(a.slug, ''),
		       k.node_kind is not null and k.node_address is not null
		from asks k join artifacts a on a.id = `+refs.AskBlockDocumentSQL("k")+`
		where k.id = any($1::uuid[])
	`, ids)
	if err != nil {
		return nil, fmt.Errorf("read the answers of the asks readings stand in: %w", err)
	}
	defer rows.Close()
	answers := make(map[string]askAnswer, len(ids))
	for rows.Next() {
		var id, state, slug string
		var issueKey, project *string
		var named bool
		if err := rows.Scan(&id, &state, &issueKey, &project, &slug, &named); err != nil {
			return nil, fmt.Errorf("scan the answer of an ask a reading stands in: %w", err)
		}
		var node asks.NodeAnswer
		if err := node.Read(graphstore.TupleAnswerState, state); err != nil {
			return nil, fmt.Errorf("read the answer state of ask %s: %w", id, err)
		}
		ref := text.Ref{ID: id, Document: slug}
		if issueKey != nil {
			ref.IssueKey = *issueKey
		} else if project != nil {
			ref.Project = *project
		}
		answers[id] = askAnswer{id: id, address: refs.AskRef(ref), named: named, answered: state == asks.StateAnswered}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the answers of the asks readings stand in: %w", err)
	}
	return answers, nil
}

// asksOfBlocks reads the asks of artifactID's ask blocks under blockIDs, by block id, as
// loadAskBlocks reads a document's asks, but with no document owner's lock held for them, which
// loadAskBlocks's caller holds: a write reads them to decide whether to refuse it, and writes none
// of them.
func asksOfBlocks(ctx context.Context, tx pgx.Tx, artifactID string, blockIDs []string) (map[string]model.Ask, error) {
	rows, err := tx.Query(ctx, `
		select `+AskColumns+`
		from asks a where `+refs.AsksOfFragmentBlocksSQL("a", "$1::uuid", "$2::text[]")+`
	`, artifactID, blockIDs)
	if err != nil {
		return nil, fmt.Errorf("read the asks of document %s: %w", artifactID, err)
	}
	defer rows.Close()
	found := make(map[string]model.Ask, len(blockIDs))
	for rows.Next() {
		ask, err := ScanAsk(rows)
		if err != nil {
			return nil, fmt.Errorf("scan an ask of document %s: %w", artifactID, err)
		}
		found[*ask.BlockID] = ask
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the asks of document %s: %w", artifactID, err)
	}
	return found, nil
}

// readingWithoutAnswer is why reading may not stand in its ask, which holds no answer yet, or
// nil where the ask holds one. A reading reads a decision, and a person's ask is one only once
// they answer it, so the refusal names the reading and the ask and says what is missing. An ask
// block that opens no ask yet - one this very write adds - is named by its block id.
func readingWithoutAnswer(reading askReading, answers map[string]askAnswer) error {
	answer, opened := answers[reading.ask]
	if answer.answered {
		return nil
	}
	ask := fmt.Sprintf("the ask of ask block %q, which no ask row holds yet", reading.ask)
	if opened && answer.address != "" {
		ask = "the ask " + answer.address
	}
	return fmt.Errorf("%s %q stands in %s, and that ask has no answer yet: a %s reads a decision, and an ask is one only once a person answers it, so write the %s after the answer", reading.kind, reading.reading, ask, reading.kind, reading.kind)
}

// refuseReadingsWithoutAnswers refuses tree, the tree an agent's or API write to artifactID
// leaves, when a reading it introduces stands in an ask that holds no answer
// (readingWithoutAnswer), naming the first such reading in document order (ErrInvalidAskBlock).
// A reading is introduced when prior, the tree before the write, holds no reading under its id in
// the same ask block; a nil prior (a new document, or one outside the schema) holds none. So an
// edit elsewhere in the document never meets a reading another writer left. A browser's write
// never comes here: its editor refuses such a reading, and settlement flags one it meets anyway
// (flagReadingsWithoutAnswers).
//
// Whether the ask holds an answer is decided as settlement decides it (readingAnswers), over the
// asks this document's blocks index and, for a block none indexes, the ask of another document of
// its owner it was copied from (readCopiedAskSources). That read takes no copy lock
// (lockProjectCopies): the lock keeps two settlements from each opening an ask for copies of one
// ask, and this write opens none.
func refuseReadingsWithoutAnswers(ctx context.Context, tx pgx.Tx, artifactID string, tree, prior *pmdoc.Node) error {
	held := map[askReading]struct{}{}
	for _, reading := range askReadings(prior) {
		held[reading] = struct{}{}
	}
	var introduced []askReading
	var blockIDs []string
	var blocks []askBlock
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type != "ask" {
			return true
		}
		before := len(introduced)
		for _, reading := range closingReadings(node) {
			if _, kept := held[reading]; !kept {
				introduced = append(introduced, reading)
			}
		}
		if len(introduced) == before {
			return true
		}
		id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
		blockIDs = append(blockIDs, id)
		// A block the parse cannot read is a copy of nothing (askBlock.asks): it shows its own
		// row's ask or none.
		if block, err := parseAskBlock(node); err == nil {
			blocks = append(blocks, block)
		}
		return true
	})
	if len(introduced) == 0 {
		return nil
	}
	answers, err := shownAskAnswers(ctx, tx, artifactID, blockIDs, blocks)
	if err != nil {
		return err
	}
	for _, reading := range introduced {
		if reason := readingWithoutAnswer(reading, answers); reason != nil {
			return &ErrInvalidAskBlock{Reason: reason}
		}
	}
	return nil
}

// shownAskAnswers is, of blockIDs, ask blocks of document artifactID read as blocks (the readable
// ones among them; a block the parse cannot read is a copy of nothing), the ask each one shows and
// what its node holds (readingAnswers): its own row's, else, for a block no ask of its document
// indexes, the ask of another document of its owner it was copied from (readCopiedAskSources). The
// read takes no copy lock (lockProjectCopies): it opens no ask.
func shownAskAnswers(ctx context.Context, tx pgx.Tx, artifactID string, blockIDs []string, blocks []askBlock) (map[string]askAnswer, error) {
	rows, err := asksOfBlocks(ctx, tx, artifactID, blockIDs)
	if err != nil {
		return nil, err
	}
	var sources map[string]copiedSource
	if unindexed := unindexedAskBlockIDs(blocks, rows); len(unindexed) > 0 {
		owner, err := documentOwner(ctx, tx, artifactID)
		if err != nil {
			return nil, err
		}
		if sources, err = readCopiedAskSources(ctx, tx, artifactID, owner, blocks, unindexed); err != nil {
			return nil, err
		}
	}
	return readingAnswers(ctx, tx, blockIDs, rows, sources)
}

// readingShownAsks is shownAskAnswers for the ask blocks of tree under ids, which close with a
// reading, as the typed-node pass reads them (refs.TypedSources.Shown): the ask each one shows,
// whether that ask's row names its node, and whether that node holds an answer. A block that shows
// no ask is absent.
func readingShownAsks(ctx context.Context, tx pgx.Tx, artifactID string, tree *pmdoc.Node, ids []string) (map[string]refs.ShownAsk, error) {
	wanted := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		wanted[id] = struct{}{}
	}
	var blocks []askBlock
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type != "ask" {
			return true
		}
		id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
		if _, want := wanted[id]; want {
			if block, err := parseAskBlock(node); err == nil {
				blocks = append(blocks, block)
			}
		}
		return true
	})
	answers, err := shownAskAnswers(ctx, tx, artifactID, ids, blocks)
	if err != nil {
		return nil, err
	}
	shown := make(map[string]refs.ShownAsk, len(answers))
	for block, answer := range answers {
		shown[block] = refs.ShownAsk{ID: answer.id, Named: answer.named, Answered: answer.answered}
	}
	return shown, nil
}

// flagReadingsWithoutAnswers is blocks, the readable ask blocks settlement collected from a
// document, less each one that closes with a reading while the ask it shows holds no answer
// (readingAnswers, over rows, the asks of the document, and sources, the asks its copies were
// copied from), which joins invalid with that reason (readingWithoutAnswer): settlement flags it
// `invalid` as it flags any ask block the strict read refuses, and leaves its row as it was. A
// browser's write is never refused, so this is where one that left such a reading meets the rule
// a markdown write is held to (refuseReadingsWithoutAnswers).
func flagReadingsWithoutAnswers(ctx context.Context, tx pgx.Tx, blocks []askBlock, invalid []invalidAskBlock, rows map[string]model.Ask, sources map[string]copiedSource) ([]askBlock, []invalidAskBlock, error) {
	closing := map[string][]askReading{}
	var blockIDs []string
	for _, block := range blocks {
		readings := closingReadings(block.node)
		if len(readings) == 0 {
			continue
		}
		closing[block.id] = readings
		blockIDs = append(blockIDs, block.id)
	}
	if len(blockIDs) == 0 {
		return blocks, invalid, nil
	}
	answers, err := readingAnswers(ctx, tx, blockIDs, rows, sources)
	if err != nil {
		return nil, nil, err
	}
	readable := make([]askBlock, 0, len(blocks))
	for _, block := range blocks {
		var reason error
		for _, reading := range closing[block.id] {
			if reason = readingWithoutAnswer(reading, answers); reason != nil {
				break
			}
		}
		if reason != nil {
			invalid = append(invalid, invalidAskBlock{node: block.node, id: block.id, reason: reason})
			continue
		}
		readable = append(readable, block)
	}
	return readable, invalid, nil
}
