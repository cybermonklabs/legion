package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/schemanodes"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

type askBlock struct {
	node     *pmdoc.Node
	id       string
	question string
	options  []model.AskOption
	multiple bool
	urgency  string
}

// settlementReconciliation is what one settlement's pass over the ask blocks and the check blocks
// found. Everything that names a version number - a repair, an invalid block, the retraction of
// an ask whose block left - is left for nameVersion, since settlement knows the number only once
// it has rendered the reconciled tree: a settlement whose markdown repeats the latest version
// writes no version and leaves the document at the one it found.
type settlementReconciliation struct {
	repairs   []blockRepair
	events    []model.Event
	retracted []model.Ask
	// answeredGone is whether an answered ask of the document has no block in the tree reconciled
	// - a row no block matched - which alone can make the version a person's removal of its block
	// (holdRoomRemovals), so a settlement whose tree holds every answered ask's block reads no
	// removal.
	answeredGone bool
	// owedCopies are the other documents showing a copy of an ask this settlement retracts, or of
	// a decided ask whose wording it restores (restoreDocumentDecidedWordings), whose settlement
	// reconcileAskBlocks marked owed (owedCopiesOf); the settlement arms each once it commits.
	owedCopies []string
	// copied are the blocks this settlement wrote a copy's attributes into, as a copy of another
	// document's ask (copiedAskSources), which it logs once it commits (logCopiedBlocks).
	copied []copiedBlock
	// indexedAskBlocks are the blocks this settlement indexed or found indexed, whose author the
	// room forgets as it commits (consumeAskAuthorsLocked) and whose durable author its transaction
	// deletes (deleteSettledAskAuthors). A copy is neither: its author is kept for the ask it opens
	// once its source is retracted. Nor is a new block a closed issue's settlement leaves with no
	// row: its author is kept for the ask it opens once the issue reopens (reconcileAskBlocks).
	indexedAskBlocks []string
	// invalid is the reason each ask whose block settlement could not read fails the strict read
	// for (readAskBlockStrictly), by ask id, where the block has an ask row; readable is each ask
	// whose block reads again. stateInvalidReasons writes both on the asks' nodes (G1).
	invalid  map[string]string
	readable []string
	// follows are the follows of the asks this settlement opened, each beside the ask it follows:
	// a session author's node is written before the ask.opened that records the ask exists, and
	// learns that event right after the settlement appends it (causedByEvent).
	follows []openedFollow
	// answers are the answer states this settlement wrote through the one writer of an ask's
	// answer state (asks.PlanSettle) - a retraction of an ask whose block left, the restoration of
	// one whose block came back - each beside the event that records it, whose facts on the ask's
	// node wait for that event (causedByEvent).
	answers []settledAnswer
	// copiedFrom is, for every ask block of the tree settlement read, the ask its block is a copy
	// of (copiedAskSources), or "" for a block that is no copy, which claimCopies states as the
	// copied_from of the block's node once the version's blocks are indexed.
	copiedFrom map[string]string
	// opened is each ask block whose ask this settlement opened, with that ask's id: a block whose
	// ask no row held when its version was indexed has its shaped ties written once the row is
	// (writeOpenedAskTies). openedTies is what that write stated of each such ask, by ask id, held
	// for its ask.opened (causedByEvent).
	opened     map[string]string
	openedTies map[string]graphstore.Uncaused
	// sources is who introduced each credited block of the tree this settlement reconciled: the
	// room's record and the durable one this settlement read (askBlockSources), which the typed
	// nodes of the version it writes take their authors from (typedSources).
	sources askBlockSources
	// inStep are the ask blocks this settlement wrote an answer's words into while another build
	// serves (rollWindowAskAttributes), in document order, which withholdAnswersWithoutRoom weighs.
	// kept for one release: roll-window-attributes
	inStep []answerInStep
}

// answerInStep is an ask block settlement wrote an answer's words into (repairAskInStep): the
// block in the tree settlement reconciled, its attributes as settlement found them, the write with
// the words and the one without them (askReadableWriteWithheld), and the indexes of the repair and
// of the block.repaired the write made, -1 for none.
//
// kept for one release: roll-window-attributes
type answerInStep struct {
	node            *pmdoc.Node
	found           pmdoc.Attrs
	write, withheld func(*pmdoc.Node) bool
	repair, event   int
}

// writeOpenedAskTies plans, in the settlement's transaction that opened them, the shaped ties of
// the ask blocks whose asks this settlement opened, in one reconcile over all of them
// (refs.PlanOpenedAskTies), against what the document's block index holds (heldBlockIndex): their
// block_refs rows go through write, after the asks' own queued inserts (createAskBlock), and their
// tuples are held for the ask.opened that records each ask (causedByEvent). The targets whose
// counts each ask's rows moved are named on that ask.opened, beside what its question and parts
// moved, as a version event names what its block index moved (refs.CountedChanges, read once for
// every opened ask), so a page reading backlink counts off the event refreshes them. tree is the
// tree the settlement settled, rendered as offsets, of version `version` of document artifactID.
// It runs before the events are appended, since an appended event is never rewritten. A
// settlement that writes a version runs it after that version's block index, which reads the asks
// table, where the rows this settlement queued are not written yet, so it stated none of these
// ties; this plans against what that index leaves (heldBlockIndex).
func (r *settlementReconciliation) writeOpenedAskTies(ctx context.Context, tx pgx.Tx, artifactID string, version int, tree *pmdoc.Node, offsets []pmdoc.BlockOffset) error {
	if len(r.opened) == 0 {
		return nil
	}
	blocks := offsetBlocks(offsets)
	record, err := indexedVersionOf(ctx, tx, artifactID, version)
	if err != nil {
		return err
	}
	facts, err := refs.DecisionFacts(ctx, tx, artifactID, record, blocks, tree, blockPointers(blocks, tree),
		typedSources(ctx, tx, artifactID, tree, &r.sources))
	if err != nil {
		return err
	}
	index, err := heldBlockIndex(ctx, tx, artifactID)
	if err != nil {
		return err
	}
	ties, err := refs.PlanOpenedAskTies(ctx, tx, index, artifactID, version, time.Now().UTC(), facts, r.opened)
	if err != nil {
		return fmt.Errorf("write the ties of the ask blocks settlement opened: %w", err)
	}
	recordBlockIndex(ctx, tx, artifactID, ties.Held())
	if err := write(ctx, tx, ties.Queue); err != nil {
		return fmt.Errorf("write the ties of the ask blocks settlement opened: %w", err)
	}
	// Each tuple hangs on its ask's node, so it is the ask's to cause. The tuples are the plan's
	// facts alone, with no node write of their own, so each ask's share of them is a write of its
	// own.
	r.openedTies = make(map[string]graphstore.Uncaused, len(r.opened))
	for _, tie := range ties.Stated().Tuples {
		held := r.openedTies[tie.FromID]
		held.Tuples = append(held.Tuples, tie)
		r.openedTies[tie.FromID] = held
	}
	moved := ties.Moved()
	var all []refs.Target
	for _, targets := range moved {
		all = append(all, targets...)
	}
	if len(all) == 0 {
		return nil
	}
	counted, err := refs.CountedChanges(ctx, tx, all)
	if err != nil {
		return fmt.Errorf("count the targets the ties of the ask blocks settlement opened moved: %w", err)
	}
	for block, targets := range moved {
		ask := r.opened[block]
		own := model.ReferenceChanges{TruncatedKinds: counted.TruncatedKinds}
		for _, changed := range counted.Targets {
			if slices.Contains(targets, refs.Target{Kind: changed.Kind, ID: changed.ID}) {
				own.Targets = append(own.Targets, changed)
			}
		}
		for index, event := range r.events {
			payload, isAsk := event.Payload.(model.AskEventPayload)
			if !isAsk || event.Type != "ask.opened" || payload.ID != ask {
				continue
			}
			named := model.ReferenceChanges{Targets: payload.ReferencesChanged, TruncatedKinds: payload.ReferencesChangedTruncatedKinds}
			payload.ReferenceChangesPayload = model.NewReferenceChangesPayload(refs.MergeChanges(named, own))
			r.events[index].Payload = payload
		}
	}
	return nil
}

// settledAnswer is one answer state this settlement wrote: the ask, the type of the event the
// settlement appends for it, and what the write stated on its node (asks.Settled.Held), held for
// that event.
type settledAnswer struct {
	ask       string
	eventType string
	held      graphstore.Uncaused
}

// openedFollow is one ask this settlement opened: the write of the ask's own node, queued before
// its row (createAskBlock), and its follow by its author, both held for the ask.opened that
// records it. followed is the node the follow holds for that event, which the follow's own write
// step fills when the write phase runs it, before the step that appends the event.
type openedFollow struct {
	ask      string
	node     *graphstore.Uncaused
	followed *graphstore.Uncaused
}

// causedByEvent is, for each of events that records a write of this reconciliation held for it,
// the writes that event causes, by the event's place in events, which the write phase states under
// it right after its append (appendEvents): under the ask.opened of each ask this reconciliation
// opened, the ask's own node and its author's follow, and the ties of its block
// (writeOpenedAskTies); and under the event recording each answer state it wrote (settledAnswer) -
// the ask.resolved of a retraction, the ask.opened or ask.answered of a restoration - what that
// write stated on the ask's node. It matches by the type and the ask its payload names rather than
// by a position recorded when the write was, because events is edited after the writes are
// recorded: withholdAnswersWithoutRoom drops the block.repaired of a repair it takes back, which
// moves every event after it. A write whose event is not among events is a settlement that lost
// the event recording a write it made, and fails here naming the ask.
func (r *settlementReconciliation) causedByEvent(events []model.Event) (map[int][]*graphstore.Uncaused, error) {
	if len(r.follows) == 0 && len(r.openedTies) == 0 && len(r.answers) == 0 {
		return nil, nil
	}
	type recorded struct{ ask, eventType string }
	places := make(map[recorded]int, len(events))
	for index, event := range events {
		if payload, ok := event.Payload.(model.AskEventPayload); ok {
			places[recorded{payload.ID, event.Type}] = index
		}
	}
	caused := make(map[int][]*graphstore.Uncaused, len(r.follows)+len(r.answers))
	for _, follow := range r.follows {
		index, found := places[recorded{follow.ask, "ask.opened"}]
		if !found {
			return nil, fmt.Errorf("settlement opened ask %s and appended no ask.opened for it", follow.ask)
		}
		caused[index] = append(caused[index], follow.node, follow.followed)
	}
	for _, ask := range slices.Sorted(maps.Keys(r.openedTies)) {
		index, found := places[recorded{ask, "ask.opened"}]
		if !found {
			return nil, fmt.Errorf("settlement opened ask %s and appended no ask.opened for it", ask)
		}
		ties := r.openedTies[ask]
		caused[index] = append(caused[index], &ties)
	}
	for position := range r.answers {
		answer := &r.answers[position]
		index, found := places[recorded{answer.ask, answer.eventType}]
		if !found {
			return nil, fmt.Errorf("settlement wrote the answer state of ask %s and appended no %s for it", answer.ask, answer.eventType)
		}
		caused[index] = append(caused[index], &answer.held)
	}
	return caused, nil
}

// claimCopies plans, for each ask block of the tree settlement read whose block node the
// document's block index holds once this settlement's own indexing is written (heldBlockIndex,
// refs.BlockIndex.NodeOf), the copied_from of that node: the source ask where copiedAskSources
// resolved one, and none where it resolved none, so a block that stopped being a copy - its source
// retracted, its wording changed, its own ask opened - retracts the copied_from it held. It
// answers the step that states them under cause, nil where there is none to state. Each is a
// claim (graphstore.WriteArms), which appends only where the node holds another far end or none,
// so a settlement that moved no copy appends nothing. It runs once the version's blocks are
// planned, so a copy pasted in this version has its node, and the caller queues the step under
// that version's event, right after it (graphstore.LatestEvent), or under graphstore.NoEvent for
// a settlement that writes no version. The copy's source document is not stated here: a reader
// takes it from the source ask's own part_of (graphstore.AskFacts).
func (r *settlementReconciliation) claimCopies(ctx context.Context, tx pgx.Tx, artifactID string, who model.Actor, at time.Time) (func(b *store.WriteBatch, cause int64) error, error) {
	if len(r.copiedFrom) == 0 {
		return nil, nil
	}
	index, err := heldBlockIndex(ctx, tx, artifactID)
	if err != nil {
		return nil, fmt.Errorf("read the nodes of the ask blocks of %s: %w", artifactID, err)
	}
	var claims []graphstore.Claim
	for _, blockID := range slices.Sorted(maps.Keys(r.copiedFrom)) {
		node, named := index.NodeOf(blockID)
		if !named {
			continue
		}
		claims = append(claims, graphstore.Claim{
			FromKind: refs.BlockNodeKind, FromID: node, Kind: graphstore.TupleCopiedFrom,
			ToKind: graphstore.KindAsk, ToID: r.copiedFrom[blockID],
			At: at, Who: who,
		})
	}
	if len(claims) == 0 {
		return nil, nil
	}
	return func(b *store.WriteBatch, cause int64) error {
		for index := range claims {
			claims[index].SourceEvent = cause
		}
		arms, params, err := graphstore.WriteArms(1, nil, nil, claims)
		if err != nil {
			return err
		}
		b.Queue(fmt.Sprintf("state the copies of %s", artifactID), "with "+arms+"\nselect 1", params...)
		return nil
	}, nil
}

// stateInvalidReasons plans what settlement writes on the node of each ask it read the
// readability of the block of (G1): the reason each block that fails the strict read fails it for,
// where the block has an ask row (invalid), as the node's `invalid` value; and, for each ask whose
// block reads again (readable), the retraction of the value its node holds. A node already holding
// the reason, and one holding none whose block reads, is written nothing, so a settlement that
// moved no block's readability appends nothing. Only an ask whose row names its node has one
// (asks.node_kind): a row the release before wrote is stated on the store by the boot step's
// backfill. It answers the step that states them under cause, nil where there is none to state,
// which the caller queues where it queues claimCopies', in who's name at at.
//
// The same write retracts each removed_by an ask whose block reads (readable) holds asserted: an
// ask block of that ask stands in the document its row names again, so the person's removal no
// longer stands, and a later removal is judged afresh (graphstore.TupleRemovedBy).
func (r *settlementReconciliation) stateInvalidReasons(ctx context.Context, tx pgx.Tx, who model.Actor, at time.Time) (func(b *store.WriteBatch, cause int64) error, error) {
	if len(r.invalid) == 0 && len(r.readable) == 0 {
		return nil, nil
	}
	kind := schemanodes.StoreName("ask", "invalid")
	invalid := slices.Sorted(maps.Keys(r.invalid))
	rows, err := tx.Query(ctx, `
		select k.id::text, coalesce(held.to_id, ''), coalesce(held.asserted, false)
		from asks k
		left join lateral (
			select t.to_id, t.asserted from graph_tuples t
			where t.from_kind = $2 and t.from_id = k.id::text and t.kind = $3 and t.to_kind = $4
			order by t.seq desc limit 1
		) held on true
		where k.id = any($1::uuid[]) and k.node_kind is not null
		order by k.id
	`, slices.Concat(invalid, r.readable), graphstore.KindAsk, kind, graphstore.KindValue)
	if err != nil {
		return nil, fmt.Errorf("read the readability the ask nodes hold: %w", err)
	}
	var tuples []graphstore.Tuple
	for rows.Next() {
		var ask, current string
		var holds bool
		if err := rows.Scan(&ask, &current, &holds); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read the readability an ask node holds: %w", err)
		}
		tuple := graphstore.Tuple{
			FromKind: graphstore.KindAsk, FromID: ask, Kind: kind, ToKind: graphstore.KindValue,
			At: at, Who: who,
		}
		reason, flagged := r.invalid[ask]
		switch {
		case flagged && (!holds || current != reason):
			tuple.ToID, tuple.Asserted = reason, true
		case !flagged && holds:
			tuple.ToID = current
		default:
			continue
		}
		tuples = append(tuples, tuple)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the readability the ask nodes hold: %w", err)
	}
	if len(r.readable) > 0 {
		removals, err := tx.Query(ctx, `
			select held.from_id, held.to_id from (
				`+graphstore.CurrentTupleRead(`t.from_id, t.to_id`, `t.from_id, t.to_id, t.asserted`,
			`t.from_kind = $2 and t.from_id = any($1::text[]) and t.kind = $3 and t.to_kind = $4`)+`
			) held where held.asserted
			order by held.from_id, held.to_id
		`, r.readable, graphstore.KindAsk, graphstore.TupleRemovedBy, graphstore.KindPerson)
		if err != nil {
			return nil, fmt.Errorf("read the removals the ask nodes hold: %w", err)
		}
		for removals.Next() {
			var ask, person string
			if err := removals.Scan(&ask, &person); err != nil {
				removals.Close()
				return nil, fmt.Errorf("read a removal an ask node holds: %w", err)
			}
			tuples = append(tuples, graphstore.Tuple{
				FromKind: graphstore.KindAsk, FromID: ask, Kind: graphstore.TupleRemovedBy,
				ToKind: graphstore.KindPerson, ToID: person, At: at, Who: who,
			})
		}
		removals.Close()
		if err := removals.Err(); err != nil {
			return nil, fmt.Errorf("read the removals the ask nodes hold: %w", err)
		}
	}
	if len(tuples) == 0 {
		return nil, nil
	}
	return func(b *store.WriteBatch, cause int64) error {
		for index := range tuples {
			tuples[index].SourceEvent = cause
		}
		graphstore.QueueWrite(b, nil, tuples)
		return nil
	}, nil
}

// blockRepair is one attribute repair settlement made to the block of type blockType carrying
// blockID in the tree it read, which set makes again on that block in the live document
// (repairLive): a server-owned attribute of an ask, or the ci_check of a check that breaks a rule
// (reconcileCICheckBlocks).
type blockRepair struct {
	blockID   string
	blockType string
	set       func(*pmdoc.Node) bool
}

// askRepair is the repair set makes to the ask block carrying blockID.
func askRepair(blockID string, set func(*pmdoc.Node) bool) blockRepair {
	return blockRepair{blockID: blockID, blockType: "ask", set: set}
}

// copiedBlock is one block settlement wrote a copy's attributes into: its id and its source.
type copiedBlock struct {
	blockID string
	source  copiedSource
}

// logCopiedBlocks logs, once its settlement of artifactID has committed, each block that settlement
// wrote a copy's attributes into - a block becoming a copy, or a copy whose source's state changed
// - naming the copy, its source and the state it shows, never the question. A block taken for a
// copy opens no ask and reaches nobody's Inbox, so this line is how one taken for a copy that was
// meant as a new question is found. A copy whose attributes already agree is written nothing and
// logged nothing, so an unchanged copy's later settlements add no line.
func (r *settlementReconciliation) logCopiedBlocks(artifactID string) {
	for _, block := range r.copied {
		slog.Info("dispatch: settled an ask block as a copy of another document's ask",
			"artifact", artifactID, "block", block.blockID, "source_ask", block.source.ask.ID,
			"source_document", block.source.document, "source_state", block.source.ask.State)
	}
}

// repair runs set on node, the block carrying blockID in the tree settlement read (an ask, or a
// check whose ci_check breaks a rule), and records it for repairLive, under node's block type,
// when it changed the block.
func (r *settlementReconciliation) repair(node *pmdoc.Node, blockID string, set func(*pmdoc.Node) bool) bool {
	if !set(node) {
		return false
	}
	r.repairs = append(r.repairs, blockRepair{blockID: blockID, blockType: node.Type, set: set})
	return true
}

// repairAsk makes set, settlement's write of the server attributes of an ask block
// (askReadableWrite), on node, the ask block carrying blockID in the tree settlement read,
// records the repair for repairLive and, when event is not nil, the block.repaired it emits, and
// reports whether it changed the block.
func (r *settlementReconciliation) repairAsk(node *pmdoc.Node, blockID string, set func(*pmdoc.Node) bool, event *model.Event) bool {
	if !r.repair(node, blockID, set) {
		return false
	}
	if event != nil {
		r.events = append(r.events, *event)
	}
	return true
}

// repairAskInStep is repairAsk with askReadableWrite, settlement's write to a block of ask, or
// of a copy of ask on the document copied, that records the block when the write gave it an
// answer's words while another build serves (wroteAnswerWords), for withholdAnswersWithoutRoom to
// weigh. A block that already held the words its write leaves - one whose repair cleared its
// `invalid` or moved a copy's provenance - keeps them: they are in the document settlement read
// and add nothing to it, and the release before weighed only a repair that wrote an answer. The
// write stays, settlement's one write to a block of an ask (G1); the record of the words it wrote
// goes with the roll window.
func (r *settlementReconciliation) repairAskInStep(node *pmdoc.Node, blockID string, twoBuilds bool, ask model.Ask, copied string, event *model.Event) bool {
	// kept for one release: roll-window-attributes
	found := maps.Clone(node.Attrs)
	// kept for one release: roll-window-attributes
	repair, eventAt := len(r.repairs), len(r.events)
	if event == nil {
		eventAt = -1
	}
	write := askReadableWrite(twoBuilds, ask, copied)
	if !r.repairAsk(node, blockID, write, event) {
		return false
	}
	// kept for one release: roll-window-attributes
	if twoBuilds && wroteAnswerWords(found, node) {
		r.inStep = append(r.inStep, answerInStep{
			node: node, found: found, write: write, withheld: askReadableWriteWithheld(ask, copied),
			repair: repair, event: eventAt,
		})
	}
	return true
}

// wroteAnswerWords reports whether node, an ask block a write left, holds an answer's words
// (answerWords) that found, its attributes before the write, did not: a selection or words found
// held none of, or held another value of.
//
// kept for one release: roll-window-attributes
func wroteAnswerWords(found pmdoc.Attrs, node *pmdoc.Node) bool {
	for _, name := range answerWords {
		value, held := node.Attrs[name]
		if !held {
			continue
		}
		if was, had := found[name]; !had || !sameAskAttribute(was, value) {
			return true
		}
	}
	return false
}

// withholdAnswersWithoutRoom keeps out of the document each answer this settlement wrote into its
// block while another build serves (inStep) that the document has no room for: one that would
// leave it past what one upload may hold and bigger than before, the document's rendering as
// settlement read it (weigh, which weighs a caller's write the same way). A block that leaves the
// document and returns gets its answer from the ask, so a returning block is the answer's text
// arriving without the answer route that weighs it (KeepAskBlockInStep): forty answers of 32,000
// characters, each taken against a document their deleted blocks had left small, come back in one
// edit. Where the answers do not all fit, every one is taken out of tree, and each is given back
// in document order while the document still has room for it. A block whose answer stays out is
// repaired without it (askReadableWriteWithheld), so it still says who answered and when, as the
// release before wrote such a block, and the ask keeps the answer; a later settlement of a
// document with room for it writes it back. A repair that then changes nothing is dropped with
// its block.repaired. A document that does not render cannot be weighed, so its answers all stay
// out, and the settlement that renders it next fails as it would have.
//
// kept for one release: roll-window-attributes
func (r *settlementReconciliation) withholdAnswersWithoutRoom(artifactID string, tree *pmdoc.Node, before string) {
	if len(r.inStep) == 0 {
		return
	}
	after, err := renderTree(tree)
	rendered := err == nil
	if rendered {
		if err = weighRendering(before, after); err == nil {
			return
		}
	}
	// changes says whether each block repaired without its answer still changes from what
	// settlement found.
	changes := make([]bool, len(r.inStep))
	for index, answer := range r.inStep {
		answer.node.Attrs = maps.Clone(answer.found)
		changes[index] = answer.withheld(answer.node)
	}
	returned := make([]bool, len(r.inStep))
	if rendered {
		r.returnAnswersWithRoom(tree, before, returned)
	}
	withheld := 0
	droppedRepairs, droppedEvents := map[int]bool{}, map[int]bool{}
	for index, answer := range r.inStep {
		switch {
		case returned[index]:
			continue
		case changes[index]:
			r.repairs[answer.repair].set = answer.withheld
		default:
			droppedRepairs[answer.repair] = true
			if answer.event >= 0 {
				droppedEvents[answer.event] = true
			}
		}
		withheld++
	}
	slog.Warn("dispatch: settlement withholds restored answers from their blocks", "room", artifactID,
		"withheld", withheld, "answers", len(r.inStep), "reason", err)
	r.repairs = withoutIndexes(r.repairs, droppedRepairs)
	r.events = withoutIndexes(r.events, droppedEvents)
	r.inStep = nil
}

// returnAnswersWithRoom gives back, in document order, the answer of each block of inStep, all of
// them left out of tree, while the document still has room for it, and marks in returned which it
// gave back. The document is rendered once without them, and each answer is weighed by what it
// adds to its block's own rendering (grownBy).
//
// kept for one release: roll-window-attributes
func (r *settlementReconciliation) returnAnswersWithRoom(tree *pmdoc.Node, before string, returned []bool) {
	left, err := renderTree(tree)
	if err != nil {
		return
	}
	document, was := renderingOf(left), renderingOf(before)
	for index, answer := range r.inStep {
		withheld := *answer.node
		withheld.Attrs = maps.Clone(answer.node.Attrs)
		answer.write(answer.node)
		grown, err := grownBy(document, &withheld, answer.node)
		if err == nil {
			err = weigh(was, grown)
		}
		if err != nil {
			answer.node.Attrs = withheld.Attrs
			continue
		}
		document, returned[index] = grown, true
	}
}

// changedTree reports whether settlement changed the tree it reconciled. Every change
// reconcileAskBlocks and reconcileCICheckBlocks make in place is recorded in repairs - by repair,
// which the deletion of a broken ci_check goes through too, by repairAsk, and by the write of a
// decided ask's wording - so changedTree is true whenever the tree was touched.
func (r *settlementReconciliation) changedTree() bool {
	return len(r.repairs) > 0
}

// repairLive makes the reconciliation's repairs on live, the document as it stands when settlement
// writes them, which a peer may have edited since settlement read the tree it reconciled, and
// reports whether any changed it. A repair lands on the first block carrying its id - the holder
// EnsureBlockIDs keeps the id for - while that block has the type the repair was made on: one
// removed or retyped since takes none, and the settlement its removal scheduled reconciles it.
func (r *settlementReconciliation) repairLive(live *pmdoc.Node) bool {
	holders := make(map[string]*pmdoc.Node, len(r.repairs))
	for _, repair := range r.repairs {
		holders[repair.blockID] = nil
	}
	unfound := len(holders)
	pmdoc.Walk(live, func(node *pmdoc.Node) bool {
		id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
		if holder, wanted := holders[id]; id != "" && wanted && holder == nil {
			holders[id] = node
			unfound--
		}
		return unfound > 0
	})
	changed := false
	for _, repair := range r.repairs {
		if holder := holders[repair.blockID]; holder != nil && holder.Type == repair.blockType && repair.set(holder) {
			changed = true
		}
	}
	return changed
}

// writeLive writes the reconciliation's repairs into doc as it stands (rewriteLive, repairLive).
func (r *settlementReconciliation) writeLive(doc *crdt.Doc, origin any) error {
	_, err := rewriteLive(doc, origin, r.repairLive)
	return err
}

// nameVersion completes the reconciliation at the version the settled document is at, writing
// the retractions whose reason names it and stamping it into every event that carries one.
func (r *settlementReconciliation) nameVersion(
	ctx context.Context,
	tx pgx.Tx,
	artifactID string,
	owner artifactOwner,
	version int,
) error {
	for index, event := range r.events {
		switch payload := event.Payload.(type) {
		case model.BlockRepairedEventPayload:
			payload.Version = version
			r.events[index].Payload = payload
		case model.BlockInvalidEventPayload:
			payload.Version = version
			r.events[index].Payload = payload
		}
	}
	for _, ask := range r.retracted {
		// The retraction is queued through the one writer of an ask's answer state
		// (WriteAskResolution), and what it states on the ask's node waits for its ask.resolved
		// (causedByEvent).
		var held graphstore.Uncaused
		retracted, err := WriteAskResolution(ctx, tx, ask, "retracted", fmt.Sprintf("%s %d", SettlementRetractionReason, version), SettlementActor, &held)
		if err != nil {
			return fmt.Errorf("retract deleted ask block: %w", err)
		}
		recordAskBlock(ctx, tx, artifactID, retracted)
		r.answers = append(r.answers, settledAnswer{ask: ask.ID, eventType: "ask.resolved", held: held})
		// Its thread_logins are read when settlement appends it (settlementEventLogins).
		r.events = append(r.events, documentAskEvent(
			owner, artifactID, "ask.resolved", SettlementActor, model.NewAskEventPayload(retracted, model.ReferenceChanges{}),
		))
	}
	return nil
}

// ErrInvalidAskBlock refuses a write that would leave an ask it writes or changes unreadable: an
// upload (a spec at issue creation, a new document or version) or an edit whose ask breaks the
// content rule paragraph+ bullet_list?, or an edit whose ask settlement cannot read.
type ErrInvalidAskBlock struct {
	Reason error
}

func (e *ErrInvalidAskBlock) Error() string { return e.Reason.Error() }

func (e *ErrInvalidAskBlock) Unwrap() error { return e.Reason }

// SettlementActor writes what a document decides on its own rather than any one person: the
// retraction of an ask whose block left the document, and an approval request's move when the
// version credits no writer or several. Crediting the room's last editor instead would put a
// change nobody made in their name. It is also the actor of each repair event a settlement of a
// closed issue's document appends (planRepairs): a system actor's event notifies nobody, and such
// a settlement wakes nobody.
var SettlementActor = model.Actor{Kind: "system", ID: "document-settlement"}

// SettlementRetractionReason opens the reason of every retraction settlement writes, followed by
// the version the block left in.
const SettlementRetractionReason = "removed from the document in version"

// settlementRetracted reports whether ask's retraction is one settlement wrote when its block
// left the document, which returning the block undoes. A retraction a person or a session wrote
// is their decision and stands, however the document moves. Retractions written before
// settlement began naming itself carry only its reason, so that prefix still counts - which is
// why resolveAsk refuses a caller reason that begins with it. It applies only to block asks,
// the rows loadAskBlocks reads, which is why an approval ask retracted as SettlementActor is
// never restored.
func settlementRetracted(ask model.Ask) bool {
	return ask.State == "resolved" && ask.Resolution != nil && settlementRetraction(*ask.Resolution)
}

// settlementRetraction reports whether resolution is a retraction settlement wrote when its ask's
// block left the document (settlementRetracted). reopenOwedAskDocuments holds this rule in SQL,
// for the reopen's mark, so a change to one is a change to the other.
func settlementRetraction(resolution model.AskResolution) bool {
	return resolution.Kind == "retracted" &&
		(resolution.Actor.SameAs(SettlementActor) || strings.HasPrefix(resolution.Reason, SettlementRetractionReason))
}

// closeWithdrawn reports whether ask is resolved as its issue's close withdrew it
// (model.AskResolutionWithdrawn). That resolution is no decision on the ask's question: the
// issue's reopen opens the ask again (api.prepareRestoration).
func closeWithdrawn(ask model.Ask) bool {
	return ask.State == "resolved" && ask.Resolution != nil && ask.Resolution.Kind == model.AskResolutionWithdrawn
}

// reconcileAskBlocks makes the asks rows and the ask blocks of tree, the document settlement read,
// agree. A new block is authored by the update that introduced it (askBlockSources.author), as the
// room recorded it or, failing that, as the durable record does, which this reads in tx with the
// rows tx has queued itself (transactionRecordedAskAuthors): a block a stamp of this very
// transaction renamed is found under its minted id there. One whose update observer has not run
// is left for the settlement that observer arms. The durable author of each block an ask row now
// names, and of each block tree no longer holds, is deleted in tx.
// A block copied from another document's ask opens none and shows that ask's state instead
// (copiedAskSources). A decided ask of this document that a task of an earlier release reworded
// gets its decided wording back first (restoreDocumentDecidedWordings), so the block is written
// back to it, and its copies settle again.
//
// Each ask block is written as the fleet stands at this write (anotherBuildServes): while another
// build serves, the attributes the release before reads from the block are kept in step with its
// ask (rollWindowAskAttributes); once the fleet is one build, the ones a version keeps are kept in
// step with it and the rest of the seven a version can store taken out (keptAskState). A block
// settlement cannot read keeps whatever a browser wrote onto it - a forged state or answerer
// among them - so once the fleet is one build it is given its row's kept state where a row
// indexes it, and none of the seven where none does. `invalid` is written into the live tree
// either way (G1): onto a block that fails the strict read, and off one that reads again, and
// where the block has an ask row its node holds the reason too, until the block reads again
// (stateInvalidReasons).
//
// issueClosed is whether the issue row, as the settlement's transaction locked it, reads closed
// (settlement.issueClosed), and on a closed issue no ask changes: the close withdrew every ask of
// the issue, so an ask opened, restored or edited now would contradict the close. A new block
// opens no ask and is indexed by no row. While another build serves, it is written
// state="resolved", as the close's own sweep writes such a block (resolveUnindexedAskBlock), and
// once the fleet is one build it keeps none of the seven attributes a version can store, as a block
// with no ask row and no source keeps none (unindexedAskWrite). Its author is recorded in tx
// (recordAskAuthors, the first registration winning): this settlement deletes the document's
// pending row, whose latest edit source would otherwise name the author. The reopen marks the
// document owed (MarkReopenOwedAskDocuments), and the settlement after it opens the block's
// ask, credited to that author. A block whose observer has not run is left as it is here too. An
// ask a settlement retracted stays retracted when its block comes back, and its block shows it
// resolved: the reopen marks its document owed too, and the settlement after the reopen restores
// it, as a returning block's ask is restored on an open issue. No open ask whose block left is
// retracted, and no open ask takes its block's new wording; a copy shows its source's state as on
// an open issue. An ask the close withdrew whose block left stays withdrawn: the reopen restores
// it and marks its block document owed, and the settlement after the reopen retracts it. Nor is
// the close's withdrawal a decision whose wording settlement writes back (writeDecidedWording):
// the block of an ask the close withdrew keeps the wording a person gave it while the issue was
// closed, the row keeps the question it was asked, and the settlement after the reopen takes the
// block's wording into the restored ask, as it takes an open ask's.
func (s *Service) reconcileAskBlocks(
	ctx context.Context,
	tx pgx.Tx,
	artifactID string,
	owner artifactOwner,
	tree *pmdoc.Node,
	credit settlementVersionCredit,
	issueClosed bool,
) (settlementReconciliation, error) {
	blocks, invalidBlocks, err := collectAskBlocksForSettlement(tree)
	if err != nil {
		return settlementReconciliation{}, err
	}
	// Settlement reads an ask as askReadability does (readAskBlockStrictly): one that parses but
	// breaks its content rule (pmdoc.AskContentError) is flagged as one the parse refuses is, and
	// its row left as it was. A decided ask flagged so keeps its decided row and skips
	// writeDecidedWording until repaired. The collector reads such an ask leniently, so the
	// ask-edit route finds it (askBlockOf) to refuse it, quoting its blocks (refuseAskContentEdit).
	readable := blocks[:0]
	for _, block := range blocks {
		if err := readAskBlockStrictly(block.node); err != nil {
			invalidBlocks = append(invalidBlocks, invalidAskBlock{node: block.node, id: block.id, reason: err})
			continue
		}
		readable = append(readable, block)
	}
	blocks = readable
	rows, err := loadAskBlocks(ctx, tx, artifactID)
	if err != nil {
		return settlementReconciliation{}, err
	}
	credit.askSources.recorded, err = transactionRecordedAskAuthors(ctx, tx, artifactID)
	if err != nil {
		return settlementReconciliation{}, err
	}
	restoredDecided, err := s.restoreDocumentDecidedWordings(ctx, tx, rows)
	if err != nil {
		return settlementReconciliation{}, err
	}
	sources, err := copiedAskSources(ctx, tx, artifactID, owner, blocks, rows)
	if err != nil {
		return settlementReconciliation{}, err
	}
	// A reading stands only in an ask a person answered. Every write that takes markdown refuses
	// one in any other ask (refuseReadingsWithoutAnswers); a browser's write is never refused, so
	// settlement flags such an ask block with that reason, as it flags one the strict read refuses.
	// Whether the ask holds an answer is decided once the block's ask is resolved
	// (readingAnswers): a copy shows its source's (sources), and a block bringing back an ask
	// settlement retracted shows the state the restoration below gives it. So the flag is read
	// after the rows and the sources, and before the loop that restores.
	blocks, invalidBlocks, err = flagReadingsWithoutAnswers(ctx, tx, blocks, invalidBlocks, rows, sources)
	if err != nil {
		return settlementReconciliation{}, err
	}

	actor := credit.actor
	// The fleet is read at each write that holds an ask block, never cached: the attributes stay in
	// step while another build serves, and the first settlement after its last process left takes
	// them out.
	// kept for one release: roll-window-attributes
	// kept for one release: stored-ask-state
	twoBuilds := false
	// kept for one release: roll-window-attributes
	// kept for one release: stored-ask-state
	if len(blocks)+len(invalidBlocks) > 0 {
		if twoBuilds, err = anotherBuildServes(ctx, tx); err != nil {
			return settlementReconciliation{}, err
		}
	}
	// Once the fleet is one build, the attributes a version keeps are written from each ask's node,
	// each one its node holds no value of taken off, or from its row where the node is behind it,
	// before the decisions step's catch-up (keptAskState, keptFromItsNode); an ask whose state this
	// settlement writes - one it restores - is written as it states it.
	// kept for one release: stored-ask-state
	var nodes map[string]askNodeAnswer
	// kept for one release: stored-ask-state
	if !twoBuilds && len(rows)+len(sources) > 0 {
		ids := make([]string, 0, len(rows)+len(sources))
		for _, ask := range rows {
			ids = append(ids, ask.ID)
		}
		for _, source := range sources {
			ids = append(ids, source.ask.ID)
		}
		if nodes, err = askNodeAnswers(ctx, tx, ids); err != nil {
			return settlementReconciliation{}, err
		}
	}
	// kept for one release: stored-ask-state
	shown := func(ask model.Ask) model.Ask {
		if twoBuilds {
			return ask
		}
		return keptFromItsNode(ask, nodes)
	}
	// The sources, with the durable record read above, are what the version this settlement writes
	// reads the authors of its decisions and readings from (typedSources).
	reconciled := settlementReconciliation{sources: credit.askSources}
	// unindexed are the authors of the new blocks a closed issue's settlement leaves with no row,
	// where the durable record holds none yet, recorded once the blocks are reconciled.
	var unindexed map[string]model.Actor
	for _, invalid := range invalidBlocks {
		reason := invalid.reason.Error()
		ask, indexed := rows[invalid.id]
		if indexed {
			if reconciled.invalid == nil {
				reconciled.invalid = map[string]string{}
			}
			reconciled.invalid[ask.ID] = reason
		}
		delete(rows, invalid.id)
		if reconciled.repair(invalid.node, invalid.id, askInvalidAttribute(reason)) {
			reconciled.events = append(reconciled.events, documentAskEvent(
				owner,
				artifactID,
				"block.invalid",
				actor,
				model.BlockInvalidEventPayload{
					BlockID:     invalid.id,
					Reason:      reason,
					DisturbedBy: actor,
				},
			))
		}
		// Once the fleet is one build, an invalid block keeps none of the attributes a version can
		// store where it has no ask row (R10), which stays, and an indexed one what a version keeps
		// of its ask, which goes with that form; the roll window's branch goes with it.
		// kept for one release: roll-window-attributes
		if twoBuilds {
			continue
		}
		unread := withoutStoredAskAttributes
		// kept for one release: stored-ask-state
		if indexed {
			unread = keptAskState(shown(ask), "")
		}
		reconciled.repair(invalid.node, invalid.id, unread)
	}
	// Every ask block of the tree, readable or not, names the ask it is a copy of, or none, for
	// claimCopies once the version's blocks are indexed.
	reconciled.copiedFrom = make(map[string]string, len(blocks)+len(invalidBlocks))
	for _, invalid := range invalidBlocks {
		reconciled.copiedFrom[invalid.id] = ""
	}
	for _, block := range blocks {
		reconciled.copiedFrom[block.id] = ""
		if source, copied := sources[block.id]; copied {
			reconciled.copiedFrom[block.id] = source.ask.ID
		}
	}
	for _, block := range blocks {
		ask, exists := rows[block.id]
		if !exists {
			// A copy is read-only here: its source's state is written into the block, and
			// nothing this settlement does writes the source's row.
			if source, copied := sources[block.id]; copied {
				if reconciled.repairAskInStep(block.node, block.id, twoBuilds, shown(source.ask), source.document, nil) {
					reconciled.copied = append(reconciled.copied, copiedBlock{blockID: block.id, source: source})
				}
				continue
			}
		}
		if !exists {
			blockActor, known := credit.askSources.author(block.id, actor)
			if !known {
				continue
			}
			if issueClosed {
				// No ask on a closed issue: the block reads resolved, as the close left the
				// issue's asks (unindexedAskWrite), and it keeps its author for the ask the
				// reopen's settlement opens. An author the durable record holds already stays
				// (askAuthorsStatement keeps it).
				reconciled.repair(block.node, block.id, unindexedAskWrite(twoBuilds))
				if _, stored := credit.askSources.recorded[block.id]; !stored {
					if unindexed == nil {
						unindexed = make(map[string]model.Actor)
					}
					unindexed[block.id] = blockActor
				}
				continue
			}
			var opening openedFollow
			ask, opening, err = createAskBlock(ctx, tx, artifactID, owner, block, blockActor)
			if err != nil {
				return settlementReconciliation{}, err
			}
			reconciled.indexedAskBlocks = append(reconciled.indexedAskBlocks, block.id)
			// The question and the parts through one reader: the ask's parts hold its question,
			// so the database answers once per kind for the write (refs.NewReaderInScope). The
			// ask's row is queued (createAskBlock), so the document its `#id`s resolve against -
			// this very document, which holds the question blocks they name - is named by the
			// fields that row will hold rather than read from the row, and read only if the
			// question names a block (refs.PendingAskDocument).
			reader := refs.NewReaderInScope(ctx, tx, ask.Author, "ask", ask.ID, s.serverURL, refs.PendingAskDocument(ask))
			changes, index, err := planAskReferences(ctx, tx, reader, ask.ID, ask.Question)
			if err != nil {
				return settlementReconciliation{}, fmt.Errorf("index new ask block: %w", err)
			}
			if err := write(ctx, tx, index); err != nil {
				return settlementReconciliation{}, fmt.Errorf("index new ask block: %w", err)
			}
			// The new ask's parts, beside its own references: both named by the ask.opened, and
			// read through the one reader. The parts are planned here and written after the
			// row's own insert, which this transaction has queued (createAskBlock), since every
			// part row names its ask, and after the question's reconciliation just queued
			// (questionReconciled).
			changes, parts, err := planAskPartsCounted(ctx, tx, reader, ask, ask.CreatedAt, true, changes)
			if err != nil {
				return settlementReconciliation{}, fmt.Errorf("index the parts of new ask block %s: %w", ask.ID, err)
			}
			if err := write(ctx, tx, parts); err != nil {
				return settlementReconciliation{}, fmt.Errorf("index the parts of new ask block %s: %w", ask.ID, err)
			}
			reconciled.follows = append(reconciled.follows, opening)
			if reconciled.opened == nil {
				reconciled.opened = map[string]string{}
			}
			reconciled.opened[block.id] = ask.ID
			reconciled.events = append(reconciled.events, documentAskEvent(
				owner, artifactID, "ask.opened", blockActor, model.NewAskEventPayload(ask, changes),
			))
			reconciled.repairAsk(block.node, block.id, askReadableWrite(twoBuilds, ask, ""), nil)
			continue
		}
		reconciled.indexedAskBlocks = append(reconciled.indexedAskBlocks, block.id)
		reconciled.readable = append(reconciled.readable, ask.ID)
		delete(rows, block.id)

		stated := false
		if settlementRetracted(ask) && !issueClosed {
			var held graphstore.Uncaused
			restored, err := restoreRetractedAsk(ctx, tx, ask, actor, &held)
			if err != nil {
				return settlementReconciliation{}, err
			}
			ask, stated = restored, true
			recordAskBlock(ctx, tx, artifactID, ask)
			eventType := "ask.opened"
			if ask.State == "answered" {
				eventType = "ask.answered"
			}
			reconciled.answers = append(reconciled.answers, settledAnswer{ask: ask.ID, eventType: eventType, held: held})
			// The restoration writes state, answer and resolution back: the answer it brings
			// back is a part again, and the event names what that moved. The parts are written
			// after the restoring update, which this transaction has queued. The restoration holds
			// the parts alone, so their step reconciles the question's references after them too
			// and settles the forms queue there (questionReconciled false).
			restoredAt := refs.AskPartsAt(ask)
			changes, parts, err := planAskPartsCounted(ctx, tx, refs.NewReader(ctx, tx, ask.Author, "ask", ask.ID, s.serverURL),
				ask, restoredAt, false, model.ReferenceChanges{})
			if err != nil {
				return settlementReconciliation{}, fmt.Errorf("index the parts of restored ask %s: %w", ask.ID, err)
			}
			if err := write(ctx, tx, parts); err != nil {
				return settlementReconciliation{}, fmt.Errorf("index the parts of restored ask %s: %w", ask.ID, err)
			}
			payload := model.NewAskEventPayload(ask, changes)
			payload.Restored = true
			reconciled.events = append(reconciled.events, documentAskEvent(owner, artifactID, eventType, actor, payload))
		}

		reworded := false
		// The close's withdrawal decides nothing (closeWithdrawn): on a closed issue the block of
		// an ask the close withdrew keeps the wording a person gave it, which the settlement after
		// the reopen, left owed by the reopen's ask-state write, takes into the restored ask.
		decided := ask.State != "open" && !(issueClosed && closeWithdrawn(ask))
		if previous := model.NewAskEditPrevious(ask); !block.asks(previous) && decided {
			// A decided ask's wording is fixed, as the edit route holds it (ASK_NOT_OPEN): its
			// answer or resolution decides the question it was given on, so the block gets that
			// wording back rather than the row taking the block's, which would show the decision on
			// a question nobody decided. A person who wants another question writes a new ask block.
			changed, err := writeDecidedWording(block.node, ask)
			switch {
			case err != nil:
				slog.Error("dispatch: cannot write a decided ask's wording back into its block",
					"ask", ask.ID, "artifact", artifactID, "block", block.id, "error", err)
			case changed:
				reconciled.repairs = append(reconciled.repairs, askRepair(block.id, decidedWording(ask)))
				reworded = true
				slog.Info("dispatch: wrote a decided ask's wording back into its block",
					"ask", ask.ID, "artifact", artifactID, "block", block.id, "state", ask.State)
			}
		} else if !block.asks(previous) && !issueClosed {
			options, err := json.Marshal(block.options)
			if err != nil {
				return settlementReconciliation{}, fmt.Errorf("encode reconciled ask options: %w", err)
			}
			editedAt, err := transactionNow(ctx, tx)
			if err != nil {
				return settlementReconciliation{}, err
			}
			if err := writeExec(ctx, tx, "update reconciled ask", `
				update asks
				set question = $2, options = $3, multiple = $4, urgency = $5, edited_at = $6
				where id = $1
			`, ask.ID, block.question, options, block.multiple, block.urgency, editedAt); err != nil {
				return settlementReconciliation{}, err
			}
			ask.Question = block.question
			ask.Options = block.options
			ask.Multiple = block.multiple
			ask.Urgency = block.urgency
			ask.EditedAt = askTimestamp(&editedAt)
			recordAskBlock(ctx, tx, artifactID, ask)
			// The question and the parts through one reader (refs.NewReader).
			reader := refs.NewReader(ctx, tx, ask.Author, "ask", ask.ID, s.serverURL)
			changes, index, err := planEditedAskReferences(ctx, tx, reader, ask.ID, previous.Question, ask.Question)
			if err != nil {
				return settlementReconciliation{}, fmt.Errorf("index reconciled ask: %w", err)
			}
			if err := write(ctx, tx, index); err != nil {
				return settlementReconciliation{}, fmt.Errorf("index reconciled ask: %w", err)
			}
			// The block's wording is now the row's: its parts are that wording too, read through
			// the one reader, and the ask.edited names what they moved beside the ask's own
			// references. The parts are written after the queued update that gives the row that
			// wording, which the row this plans from carries already (edited_at), and after the
			// question's reconciliation queued above (questionReconciled).
			changes, parts, err := planAskPartsCounted(ctx, tx, reader, ask, editedAt, true, changes)
			if err != nil {
				return settlementReconciliation{}, fmt.Errorf("index the parts of reconciled ask %s: %w", ask.ID, err)
			}
			if err := write(ctx, tx, parts); err != nil {
				return settlementReconciliation{}, fmt.Errorf("index the parts of reconciled ask %s: %w", ask.ID, err)
			}
			reconciled.events = append(reconciled.events, documentAskEvent(
				owner,
				artifactID,
				"ask.edited",
				actor,
				model.NewAskEditEventPayload(ask, previous, actor, changes),
			))
		}
		repaired := documentAskEvent(
			owner,
			artifactID,
			"block.repaired",
			actor,
			model.BlockRepairedEventPayload{BlockID: block.id, DisturbedBy: actor},
		)
		kept := ask
		// kept for one release: stored-ask-state
		if !stated {
			kept = shown(ask)
		}
		if reworded {
			// One block.repaired names both repairs: the wording's, and the server attributes'
			// when they changed too.
			reconciled.events = append(reconciled.events, repaired)
			reconciled.repairAskInStep(block.node, block.id, twoBuilds, kept, "", nil)
		} else {
			reconciled.repairAskInStep(block.node, block.id, twoBuilds, kept, "", &repaired)
		}
	}
	if err := recordAskAuthors(ctx, tx, artifactID, unindexed); err != nil {
		return settlementReconciliation{}, err
	}

	for _, ask := range rows {
		reconciled.answeredGone = reconciled.answeredGone || ask.State == "answered"
		// Only an open ask is retracted: an answered or resolved one is already closed, its
		// answer or resolution is the record, and the asks table forbids a resolved row that
		// still carries an answer. A closed issue's settlement retracts none.
		if ask.State != "open" || issueClosed {
			continue
		}
		reconciled.retracted = append(reconciled.retracted, ask)
	}
	// The copies of what this settlement retracts, and of the decided asks whose wording it
	// restored, owe a settlement of their own (owedCopiesOf), marked here, under the project's copy
	// lock, before this settlement's repairs append its update and lock its own pending row: the
	// copy lock and every other document's pending row come before this document's own pending row
	// and before any event.
	reconciled.owedCopies, err = owedCopiesOf(ctx, tx, documentAsks{
		artifactID: artifactID, owner: owner, asks: slices.Concat(reconciled.retracted, restoredDecided),
	})
	if err != nil {
		return settlementReconciliation{}, err
	}
	// The settlement holds the document's advisory lock, so the rows it read are every row there is:
	// none read, none to delete.
	if len(credit.askSources.recorded) > 0 {
		if err := deleteSettledAskAuthors(ctx, tx, artifactID, reconciled.indexedAskBlocks, askBlockIDs(tree)); err != nil {
			return settlementReconciliation{}, err
		}
	}
	return reconciled, nil
}

type invalidAskBlock struct {
	node   *pmdoc.Node
	id     string
	reason error
}

// validateEditedAskBlocks refuses an edit that leaves an ask unreadable - holding what settlement
// cannot read, breaking its content rule, or past the bound its wording shares - when the edit
// wrote or changed it. It reads an ask as settlement reads it (readAskBlockStrictly), so the
// refusal is the reason settlement would have flagged the block with.
func validateEditedAskBlocks(before, after *pmdoc.Node) error {
	return refuseChangedAsks(before, after, readAskBlockStrictly, nodeToken)
}

// refuseChangedAsks is the first reason check gives against an ask in after that before does not
// hold as it is - an ask after wrote or changed - or nil. Two asks are the same when fingerprint
// gives both the same value. An ask a browser edit already left unreadable,
// which the write carries through unchanged, is not the write's to refuse: refusing it would refuse
// every write to the document until someone repairs that ask in the browser, and settlement flags
// it `invalid` meanwhile.
func refuseChangedAsks(before, after *pmdoc.Node, check func(*pmdoc.Node) error, fingerprint func(*pmdoc.Node) (string, error)) error {
	var held map[string]string
	var refusal, walkErr error
	pmdoc.Walk(after, func(node *pmdoc.Node) bool {
		if refusal != nil || walkErr != nil {
			return false
		}
		if node.Type != "ask" {
			return true
		}
		reason := check(node)
		if reason == nil {
			return true
		}
		if held == nil {
			if held, walkErr = askFingerprints(before, fingerprint); walkErr != nil {
				return false
			}
		}
		value, err := fingerprint(node)
		if err != nil {
			walkErr = err
			return false
		}
		if id, _ := node.Attrs[pmdoc.BlockIDAttr].(string); held[id] == value {
			return true
		}
		refusal = reason
		return false
	})
	if walkErr != nil {
		return walkErr
	}
	return refusal
}

// askFingerprints is each ask in tree by its block id, as fingerprint gives it.
func askFingerprints(tree *pmdoc.Node, fingerprint func(*pmdoc.Node) (string, error)) (map[string]string, error) {
	held := make(map[string]string)
	var err error
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if err != nil {
			return false
		}
		if node.Type != "ask" {
			return true
		}
		id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
		held[id], err = fingerprint(node)
		return true
	})
	return held, err
}

// addedAskBlockIDs is the id of every credited block in after that before does not hold as the
// same type: a readable ask block one an edit inserted, retyped another block into, or repaired
// after a browser left it unreadable, wherever it lands - a blockquote, a list item - as the parser
// reads it, so an opener quoted in code adds none; and a decision or a reading the edit inserted or
// retyped another block into, whose node a version's index gives this write's author
// (refs.DecisionFacts). One the edit moved, reworded or left alone keeps its id and is not among
// them. A nil before - a write with no readable starting tree, a repair - holds nothing, so every
// readable ask and every decision and reading in after counts.
func addedAskBlockIDs(before, after *pmdoc.Node) map[string]struct{} {
	held := readableAskBlockIDs(before)
	typed := map[string]string{}
	pmdoc.Walk(before, func(node *pmdoc.Node) bool {
		if node.Type == "decision" || node.Type == "reading" {
			id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
			typed[id] = node.Type
		}
		return true
	})
	added := map[string]struct{}{}
	pmdoc.Walk(after, func(node *pmdoc.Node) bool {
		id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
		switch node.Type {
		case "ask":
			if _, err := parseAskBlock(node); err == nil {
				if _, kept := held[id]; !kept {
					added[id] = struct{}{}
				}
			}
		case "decision", "reading":
			if id != "" && typed[id] != node.Type {
				added[id] = struct{}{}
			}
		}
		return true
	})
	return added
}

// readableAskBlockIDs is the block id of every ask block in tree the parser reads; none for a nil
// tree.
func readableAskBlockIDs(tree *pmdoc.Node) map[string]struct{} {
	ids := map[string]struct{}{}
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type == "ask" {
			if _, err := parseAskBlock(node); err == nil {
				id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
				ids[id] = struct{}{}
			}
		}
		return true
	})
	return ids
}

// addedAskBlocks is each readable ask block in after whose block id no readable ask block in
// before carries, in after's document order, with its question: one an edit inserted, copied from
// another document, retyped another block into, or repaired after a browser left it unreadable,
// wherever it lands - a blockquote, a list item - as the parser reads it, so an opener quoted in
// code adds none. One the edit moved, reworded or left alone keeps its id and is not among them. An
// id two blocks repeat is named once, for the first.
func addedAskBlocks(before, after *pmdoc.Node) []AddedAskBlock {
	// An id once named joins held, so a later block repeating it is not named again.
	held := readableAskBlockIDs(before)
	added := []AddedAskBlock{}
	pmdoc.Walk(after, func(node *pmdoc.Node) bool {
		if node.Type != "ask" {
			return true
		}
		block, err := parseAskBlock(node)
		if err != nil {
			return true
		}
		if _, kept := held[block.id]; !kept {
			added = append(added, AddedAskBlock{ID: block.id, Question: block.question})
			held[block.id] = struct{}{}
		}
		return true
	})
	return added
}

// newAskMarkdown is what an uploaded version can say of an ask, for one refuseChangedAsks call: its
// rendering alone, taken as an upload is (asUploaded) - without anchor marks, and with its
// server-owned attributes (`state`, the answer, `invalid`) at their defaults, since an upload's are
// discarded for the ask row's. The attributes a reader's browser derives are never rendered. A new
// version is markdown, so this is how a version says it carries an ask unchanged.
//
// An ask can hold a table. A document's render writes its tables' spans out under one budget for
// the whole document (pmdoc.Render), and a version uploaded from that markdown holds them as the
// cells they were written as. So the asks one call renders share one budget (pmdoc.SpanBudget),
// spent in document order, and each ask is written as the stored markdown holds it wherever the
// document's render still had the budget for its table. A budget for each ask would write every
// ask past the first budget's end with cells the document never wrote, and spend a budget for
// every ask the document holds. Past the point a budget runs out, a table is written with fewer
// span cells than its spans cover. Where only the asks' tables have spent the budget before an
// ask, the fingerprint and the stored markdown write its table alike, and the ask reads as changed,
// refusing an upload of the document's own markdown, when its table as written holds a body row
// shorter than its widest, which the parser pads when it reads the upload: a body cell spanning
// columns under a wider header, a rowspan, or rows the budget ran out partway through. The header
// is written as wide as the widest row, so it never counts, and a table whose rows each hold as
// many cells as written is kept. Where a table outside the asks spent some of it first, the
// document can have written an ask's table with fewer span cells than the fingerprint writes, and
// the ask reads as changed unless padding its stored rows gives the fingerprint's cells; one the
// document still had the budget for is written alike and judged as above. An ask over a
// span-free table with a short body row reads as changed the same way.
func newAskMarkdown() func(ask *pmdoc.Node) (string, error) {
	budget := pmdoc.NewSpanBudget()
	return func(ask *pmdoc.Node) (string, error) {
		return budget.Render(asUploaded(&pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{ask}}))
	}
}

func collectAskBlocksForSettlement(tree *pmdoc.Node) ([]askBlock, []invalidAskBlock, error) {
	blocks := []askBlock{}
	invalidBlocks := []invalidAskBlock{}
	seen := map[string]struct{}{}
	var collectErr error
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type != "ask" {
			return true
		}
		id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
		if _, duplicate := seen[id]; duplicate {
			collectErr = fmt.Errorf("duplicate ask block id %q", id)
			return false
		}
		seen[id] = struct{}{}
		block, err := parseAskBlock(node)
		if err != nil {
			invalidBlocks = append(invalidBlocks, invalidAskBlock{node: node, id: id, reason: err})
			return true
		}
		blocks = append(blocks, block)
		return true
	})
	if collectErr != nil {
		return nil, nil, collectErr
	}
	return blocks, invalidBlocks, nil
}

// askReadability visits each ask block in document order with the reason it cannot be read, nil
// when it can (readAskBlockStrictly). An ask repeating an earlier ask's id is unreadable as a
// duplicate, so every ask gets an answer where collectAskBlocksForSettlement stops at the first
// repeat.
func askReadability(tree *pmdoc.Node, visit func(id string, reason error) bool) {
	seen := map[string]struct{}{}
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type != "ask" {
			return true
		}
		id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
		var reason error
		if _, duplicate := seen[id]; duplicate {
			reason = fmt.Errorf("duplicate ask block id %q", id)
		} else {
			reason = readAskBlockStrictly(node)
		}
		seen[id] = struct{}{}
		return visit(id, reason)
	})
}

// readAskBlockStrictly is why node is unreadable as settlement reads it, nil when it is readable:
// parseAskBlock, the content rule the browser editor holds an ask to (pmdoc.AskContentError),
// then the bound its question and options share (askWordingTooLong). It is the one reading of an
// ask block, in one order: settlement calls it on each ask its collector
// (collectAskBlocksForSettlement) reads, and every write of this package is held to it for each
// ask the write wrote or changed (validateEditedAskBlocks; the version path in mutation.go holds
// its asks to readWrittenAskBlockStrictly, the same reading with the content rule for markdown
// written in whole), so a write is refused with the reason settlement would have flagged the
// block with, and a document carries no ask settlement could not read.
//
// An ask breaking the rule - which concurrent edits can leave, and the browser keeps - is
// unreadable even where the parse reads it: the browser's card takes its question from its first
// child and its options from its last, where the parse joins them all, so settlement marks it
// `invalid` and leaves its row as it was.
//
// The wording bound is read here for the same reason: a browser edit reaches a document through
// the Yjs room and settlement, where no route's check runs, so without it a person could open an
// ask whose wording every ask route refuses. Its row would then take no PATCH at all, an
// urgency-only edit included, because the ask routes hold the merged ask to that bound
// (api.validateAskWording). Settlement marks such a block `invalid` with a block.invalid event
// naming the bound, and leaves its row as it was, until an edit shortens the wording. A decision
// block opens an ask like any other, whose ask.answered event carries its wording, so a wording
// that cannot be answered must not be stored.
func readAskBlockStrictly(node *pmdoc.Node) error {
	block, err := parseAskBlock(node)
	if err != nil {
		return err
	}
	if err := pmdoc.AskContentError(node); err != nil {
		return err
	}
	return askWordingTooLong(block)
}

// readWrittenAskBlockStrictly is readAskBlockStrictly for an ask that markdown written in whole
// carries: the same parse and the same wording bound, with the content rule written markdown is
// held to (pmdoc.AskWrittenContentError), so an ask written with its options and no question is
// refused at that door as well, which the tree rule no longer catches once both parsers read the
// empty paragraph the schema fills in ahead of the options. That rule is read first, since it
// names what the author wrote wrongly, where the parse would name the empty question alone.
func readWrittenAskBlockStrictly(node *pmdoc.Node) error {
	if err := pmdoc.AskWrittenContentError(node); err != nil {
		return err
	}
	block, err := parseAskBlock(node)
	if err != nil {
		return err
	}
	return askWordingTooLong(block)
}

// parseAskBlock is the ask settlement reads in node, or why it cannot be read: a block no
// paragraph of which holds text asks nothing, and settlement leaves its row as it was. The
// readings an ask closes with (pmdoc.TypedContent) are no part of what it asks, and the parse
// passes over them.
func parseAskBlock(node *pmdoc.Node) (askBlock, error) {
	block, err := parseAskBlockLeniently(node)
	if err != nil {
		return askBlock{}, err
	}
	if block.question == "" {
		return askBlock{}, fmt.Errorf("ask block %q has an empty question", block.id)
	}
	return block, nil
}

// parseAskBlockLeniently is parseAskBlock with its one refusal of an empty question left off: the
// block comes back carrying the question its paragraphs hold, "" where none of them holds text.
// Every other refusal of the parse is given here, so a block read this way and one read strictly
// differ in their question alone. The ask-edit route reads a block leniently to repair it, since
// an ask edit carrying a question writes one into the block (SetAskBlockText).
func parseAskBlockLeniently(node *pmdoc.Node) (askBlock, error) {
	blockID, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
	urgency, _ := node.Attrs["urgency"].(string)
	multiple, _ := node.Attrs["multiple"].(bool)
	if blockID == "" || urgency == "" {
		return askBlock{}, fmt.Errorf("ask block is missing required attributes")
	}
	questionParts := []string{}
	// An ask block without a bullet list is a free-text decision; its options are an empty
	// list on the wire (never JSON null), the same shape every other ask carries.
	options := []model.AskOption{}
	content, _ := pmdoc.TypedContent(node)
	for _, child := range content {
		switch child.Type {
		case "paragraph":
			questionParts = append(questionParts, strings.TrimSpace(pmdoc.TextContent(child)))
		case "bullet_list":
			for _, item := range child.Children {
				if len(item.Children) == 0 {
					return askBlock{}, fmt.Errorf("ask block %q has an empty option", blockID)
				}
				option := optionOfItem(item)
				if option.Label == "" {
					return askBlock{}, fmt.Errorf("ask block %q has an option without a label", blockID)
				}
				options = append(options, option)
			}
		default:
			return askBlock{}, fmt.Errorf("ask block %q has unsupported body node %q", blockID, child.Type)
		}
	}
	question := strings.TrimSpace(strings.Join(questionParts, "\n\n"))
	return askBlock{node: node, id: blockID, question: question, options: options, multiple: multiple, urgency: urgency}, nil
}

// loadAskBlocks reads every ask anchored to a block of this document, as the transaction's queued
// writes leave them (recordAskBlock). It takes no row lock: its caller holds the document owner's
// lock, which every writer of a block ask takes first - settlement's reconciliation, the ask
// routes' answers, edits and resolutions, a version's approval carry - so the rows hold still
// until the caller's transaction ends.
func loadAskBlocks(ctx context.Context, tx pgx.Tx, artifactID string) (map[string]model.Ask, error) {
	rows, err := tx.Query(ctx, `
		select `+AskColumns+`
		from asks a where a.block_artifact_id = $1 and a.block_id is not null
	`, artifactID)
	if err != nil {
		return nil, fmt.Errorf("load ask blocks: %w", err)
	}
	defer rows.Close()
	asks := map[string]model.Ask{}
	for rows.Next() {
		ask, err := ScanAsk(rows)
		if err != nil {
			return nil, fmt.Errorf("scan ask block: %w", err)
		}
		if ask.BlockID == nil {
			return nil, fmt.Errorf("loaded ask block without a block id")
		}
		asks[*ask.BlockID] = ask
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate ask blocks: %w", err)
	}
	if ledger := queueFor(ctx, tx); ledger != nil {
		for blockID, ask := range ledger.askBlocks[artifactID] {
			asks[blockID] = ask
		}
	}
	return asks, nil
}

// recordAskBlock records ask, a block ask of artifactID whose row the caller has queued a write of,
// as the row will stand, so a later read of the document's block asks in the same transaction
// (loadAskBlocks: the version write's approval carry) reads it so.
func recordAskBlock(ctx context.Context, tx pgx.Tx, artifactID string, ask model.Ask) {
	ledger := queueFor(ctx, tx)
	if ledger == nil || ask.BlockID == nil {
		return
	}
	if ledger.askBlocks == nil {
		ledger.askBlocks = make(map[string]map[string]model.Ask)
	}
	if ledger.askBlocks[artifactID] == nil {
		ledger.askBlocks[artifactID] = make(map[string]model.Ask)
	}
	ledger.askBlocks[artifactID][*ask.BlockID] = ask
}

// createAskBlock opens the ask of block, a new ask block of artifactID: its id and its created_at,
// the transaction's now() its column's default would give it, are taken where it is called, and
// its node, its insert and its author's follow go through write, so the ask it returns is the row
// as the insert leaves it before the insert has run. The node goes in first, in the project of the
// document that holds the block: the owner's where the caller read it with the document's own row
// (lockArtifactOwner), so the node costs no read of its own, and otherwise read off the document
// here (graphstore.ScopeOf). The node and its relations are dated by that created_at, as
// graph_edges' old arm dates the row (graphstore.AskFacts), by the database's clock, never a
// host's. The ask.opened that records this ask is appended with the settlement's other events,
// after this, so the ask's node and the author's node are made at the sentinel and learn it then
// (causedByEvent).
func createAskBlock(
	ctx context.Context,
	tx pgx.Tx,
	artifactID string,
	owner artifactOwner,
	block askBlock,
	author model.Actor,
) (model.Ask, openedFollow, error) {
	options, err := json.Marshal(block.options)
	if err != nil {
		return model.Ask{}, openedFollow{}, fmt.Errorf("encode new ask block options: %w", err)
	}
	authorJSON, err := json.Marshal(author)
	if err != nil {
		return model.Ask{}, openedFollow{}, fmt.Errorf("encode new ask block author: %w", err)
	}
	var ownerArtifactID *string
	if owner.IssueKey == nil {
		ownerArtifactID = new(artifactID)
	}
	var ask model.Ask
	if err := tx.QueryRow(ctx, `select gen_random_uuid()::text`).Scan(&ask.ID); err != nil {
		return model.Ask{}, openedFollow{}, fmt.Errorf("mint ask block id: %w", err)
	}
	if ask.CreatedAt, err = transactionNow(ctx, tx); err != nil {
		return model.Ask{}, openedFollow{}, err
	}
	id, createdAt := ask.ID, ask.CreatedAt
	project, parties := owner.Project, []string(nil)
	if project == "" {
		if project, parties, err = graphstore.ScopeOf(ctx, tx, owner.IssueKey, &artifactID); err != nil {
			return model.Ask{}, openedFollow{}, err
		}
	}
	// The ask is anchored to and part of the document holding its block, and belongs to that
	// document where it is on no issue (graphstore.AskFacts).
	places, err := graphstore.ReadAskPlaces(ctx, tx, &artifactID, ownerArtifactID, &artifactID)
	if err != nil {
		return model.Ask{}, openedFollow{}, err
	}
	nodes, facts, marked := graphstore.AskFacts(id, project, parties, author, createdAt, graphstore.NoEvent, places)
	nodeKind, nodeAddress := graphstore.AskRowNode(id, marked)
	// The author's follow of the ask this transaction creates is planned with no read: its node is
	// queued before the follow's own statement below, and its follower row is new
	// (asks.PlanFollowNewAsk), dated by the ask's created_at.
	follow := asks.PlanFollowNewAsk(id, author, createdAt)
	opening := openedFollow{ask: id, followed: new(graphstore.Uncaused)}
	node := graphstore.PlanUncaused(nodes, facts)
	opening.node = &node
	if err := write(ctx, tx, func(b *store.WriteBatch) error {
		graphstore.QueueNodes(b, node)
		b.Queue("create ask block row", `
			insert into asks (id, issue_key, artifact_id, block_id, block_artifact_id, author, question, options, multiple, urgency, kind, created_at, node_kind, node_address)
			values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'question', $11, $12, $13)
		`, id, owner.IssueKey, ownerArtifactID, block.id, artifactID, authorJSON, block.question, options, block.multiple, block.urgency, createdAt, nodeKind, nodeAddress)
		var err error
		*opening.followed, err = follow.Queue(b, graphstore.NoEvent)
		return err
	}); err != nil {
		return model.Ask{}, openedFollow{}, err
	}
	ask.IssueKey = owner.IssueKey
	ask.ArtifactID = ownerArtifactID
	ask.BlockID = new(block.id)
	ask.BlockArtifactID = new(artifactID)
	ask.Author = author
	ask.Question = block.question
	ask.Options = block.options
	ask.Multiple = block.multiple
	ask.Urgency = block.urgency
	ask.State = "open"
	ask.Kind = "question"
	recordAskBlock(ctx, tx, artifactID, ask)
	return ask, opening, nil
}

// restoreRetractedAsk gives an ask settlement retracted, whose block came back, the state and the
// answer it held before the retraction (stateBeforeRetraction) and takes the retraction off,
// through the one writer of an ask's answer state in who's name: planned where it is called
// (asks.PlanSettle) and queued through write. What that write states on the ask's node waits in
// held for the event the settlement appends for the restoration (causedByEvent).
func restoreRetractedAsk(ctx context.Context, tx pgx.Tx, ask model.Ask, who model.Actor, held *graphstore.Uncaused) (model.Ask, error) {
	prior, err := stateBeforeRetraction(ctx, tx, ask.ID)
	if err != nil {
		return model.Ask{}, err
	}
	change := asks.Restored(prior.State, prior.Answer)
	change.Who, change.At, change.Held = who, time.Now().UTC(), held
	settled, err := planSettle(ctx, tx, ask.ID, change)
	if err != nil {
		return model.Ask{}, fmt.Errorf("restore retracted ask: %w", err)
	}
	if err := write(ctx, tx, settled.Queue); err != nil {
		return model.Ask{}, fmt.Errorf("restore retracted ask: %w", err)
	}
	ask.State, ask.Answer, ask.Resolution = settled.Ask.State, settled.Ask.Answer, settled.Ask.Resolution
	return ask, nil
}

// stateBeforeRetraction is the ask askID as it stood before settlement retracted it, read from
// the newest ask.opened, ask.edited or ask.answered that recorded it: open, or answered with its
// answer. It is the state the restoration gives the ask back (restoreRetractedAsk), and the one a
// reading inside its returning block is held to (readingAnswers).
func stateBeforeRetraction(ctx context.Context, tx pgx.Tx, askID string) (model.Ask, error) {
	var payload []byte
	if err := tx.QueryRow(ctx, `
		select payload from events
		where type in ('ask.opened', 'ask.edited', 'ask.answered') and payload->>'id' = $1
		order by id desc limit 1
	`, askID).Scan(&payload); err != nil {
		return model.Ask{}, fmt.Errorf("load ask state before retraction: %w", err)
	}
	var prior model.Ask
	if err := json.Unmarshal(payload, &prior); err != nil {
		return model.Ask{}, fmt.Errorf("decode ask state before retraction: %w", err)
	}
	if prior.State != asks.StateOpen && prior.State != asks.StateAnswered {
		return model.Ask{}, fmt.Errorf("ask %q state before retraction is %q", askID, prior.State)
	}
	return prior, nil
}

func setAskInvalidAttribute(node *pmdoc.Node, reason string) bool {
	if current, present := node.Attrs["invalid"]; present && current == reason {
		return false
	}
	node.Attrs["invalid"] = reason
	return true
}

// askInvalidAttribute is setAskInvalidAttribute for reason, for a repair to make again on another
// tree (settlementReconciliation.repair).
func askInvalidAttribute(reason string) func(*pmdoc.Node) bool {
	return func(node *pmdoc.Node) bool { return setAskInvalidAttribute(node, reason) }
}

// clearAskInvalidAttribute takes `invalid` off node, an ask block settlement reads again, and
// reports whether it held one, so a repaired block shows no old reason (G1).
func clearAskInvalidAttribute(node *pmdoc.Node) bool {
	if _, present := node.Attrs["invalid"]; !present {
		return false
	}
	delete(node.Attrs, "invalid")
	return true
}

// unindexedAskWrite is settlement's write to a readable ask block of a closed issue's document
// that no row indexes: resolved while another build serves (resolveUnindexedAskBlock), and none
// of the seven attributes a version can store once the fleet is one build, as the close's sweep
// leaves such a block (unindexedAskResolution). The block reads, so it carries no `invalid`
// either way. The one-build write is R10's and stays; the roll window's branch is
// unindexedAskResolution's and goes with it.
func unindexedAskWrite(twoBuilds bool) func(*pmdoc.Node) bool {
	resolve := unindexedAskResolution(twoBuilds)
	return func(node *pmdoc.Node) bool {
		changed := clearAskInvalidAttribute(node)
		return resolve(node) || changed
	}
}

func documentAskEvent(owner artifactOwner, artifactID, eventType string, actor model.Actor, payload any) model.Event {
	event := model.Event{IssueKey: owner.IssueKey, Type: eventType, Actor: actor, Payload: payload}
	if owner.IssueKey == nil {
		event.ArtifactID = &artifactID
	}
	return event
}

// settlementEventLogins is the thread_logins an event settlement appends names: an ask.opened,
// ask.answered or ask.resolved carrying its ask's row (a new ask block's, a restored one's, a
// retracted one's) names its ask's thread's, which the event's insert reads after the ask's queued
// writes and the user_threads triggers they fire (events.ThreadLoginsOf). Every other event names
// none.
func settlementEventLogins(event model.Event) *events.Logins {
	switch event.Type {
	case "ask.opened", "ask.answered", "ask.resolved":
		if payload, ok := event.Payload.(model.AskEventPayload); ok {
			return events.ThreadLoginsOf(payload.ID)
		}
	}
	return nil
}
