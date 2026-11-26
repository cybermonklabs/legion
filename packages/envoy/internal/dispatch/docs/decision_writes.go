package docs

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// writeDecisionsDocument writes the decisions document: it takes out of it each ask block the
// restore moves (shape (b), and a copy of an ask of the main shape that the document holds) and
// each section the pass converts, as this pass reads the document's sections (planSections), and
// writes its version. The same write takes every attribute a version can store off each ask block
// the document keeps that has no ask row and is no copy of an ask (withoutAsklessAskAttributes),
// so its version holds none of the eight there (R10): the phase runs on a fleet of one build alone
// (restoreIssue). It records whether the document still holds an ask block once those are taken
// out (decisionsHoldAsks), which keeps it from its retirement. Where the pass holds a person's
// removal from the document's room (roomRemovals), it writes the version even where it takes no
// block out. That version states each such removal on the ask's node (removalsOf), so a conversion
// that then retires the document never retires it with the deletion unstated. It reports each such
// ask declined, to the person and at that version. A pass that holds such a removal and writes no
// version fails, so no pass commits the removal unstated. It answers the event of that version,
// NoEvent where it wrote none.
//
// kept for one release: decisions-step
func (r *issueRestore) writeDecisionsDocument(ctx context.Context) (int64, error) {
	var drop map[string]bool
	dropped := func(node *pmdoc.Node) bool {
		id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
		return drop[id]
	}
	err := r.service.applyLive(ctx, r.decisions.id, DecisionStepActor, func(doc *crdt.Doc, current pmdoc.ValidDocument, _ func(func(*crdt.Transaction))) error {
		tree := current.Node()
		if tree == nil {
			return fmt.Errorf("the decisions document of %s is outside the Proof schema, so the decisions step cannot read its sections", r.issue)
		}
		var err error
		if drop, err = r.planSections(ctx, tree); err != nil {
			return err
		}
		r.decisionsDropped = drop
		left, _ := withoutBlocks(tree, dropped)
		r.decisionsHoldAsks = pmdoc.HoldsAskBlock(left)
		if len(drop) == 0 {
			return nil
		}
		// kept for one release: stored-ask-state
		withAsk, err := askBlocksWithAnAsk(ctx, r.tx, r.decisions.id, tree)
		if err != nil {
			return err
		}
		_, err = rewriteLive(doc, nil, func(live *pmdoc.Node) bool {
			next, took := withoutBlocks(live, dropped)
			if !took {
				return false
			}
			live.Children = next.Children
			withoutAsklessAskAttributes(live, withAsk)
			return true
		})
		return err
	})
	if errors.Is(err, websocket.ErrNoChanges) || err == nil && len(drop) == 0 {
		if len(r.roomRemovals) == 0 {
			return graphstore.NoEvent, nil
		}
	} else if err != nil {
		return graphstore.NoEvent, err
	}
	event, err := r.writeVersion(ctx, *r.decisions, &r.result.DecisionsVersion)
	if err != nil || len(r.roomRemovals) == 0 {
		return event, err
	}
	if event == graphstore.NoEvent {
		return graphstore.NoEvent, fmt.Errorf("the decisions step holds a person's removal of an answered ask from the decisions document of %s, and wrote no version of that document to state it", r.issue)
	}
	for _, ask := range slices.Sorted(maps.Keys(r.roomRemovals)) {
		r.result.Declined = append(r.result.Declined, DeclinedAsk{Ask: ask, Person: r.roomRemovals[ask], Version: r.result.DecisionsVersion})
	}
	return event, nil
}

// writeSpec writes into the spec what the pass restores and converts, in one live write and one
// version: each restored ask block, in answer-time order, then each converted section's decision,
// in the spec's `## Decisions` part, in place of its one pointer line to the decisions document -
// the part's first paragraph whose one content is the citation of the document itself
// (isPointerLine), every other paragraph kept, one citing the document among other words or
// citing a section of it among them - or after the part's last block where it holds none, and
// under a `## Decisions` heading the write adds where the spec holds no such part; and, in each
// answered ask block it holds already, the shaped pointers and the readings the conversion gives
// it. It records the pointer line it took out (pointerLine) and whether it added the heading
// (headingAdded), which its version names as the step's own. On a fleet of one build - which the
// phase runs on alone - the same write cuts every ask block of the spec to the attributes a
// version keeps (writePresent), as the first accepted write of the document does (R10), a block
// that has no ask row and is no copy of an ask to none of them, and gives each restored answered
// ask's block those of its row (keptAskState). It answers the event of the version it wrote,
// NoEvent where the spec did not change.
//
// kept for one release: decisions-step
func (r *issueRestore) writeSpec(ctx context.Context) (int64, error) {
	// A restored ask's block keeps the state its node holds (keptAskState, onItsNode).
	// kept for one release: stored-ask-state
	var ids []string
	for _, restore := range r.restores {
		if !restore.joins && !restore.rowless {
			ids = append(ids, restore.row.ID)
		}
	}
	nodes, err := askNodeAnswers(ctx, r.tx, ids)
	if err != nil {
		return graphstore.NoEvent, err
	}
	var added []*pmdoc.Node
	for _, restore := range r.restores {
		if restore.joins {
			// A copy of an ask block the spec holds: nothing to write (planDocumentMoves).
			continue
		}
		var block *pmdoc.Node
		if restore.rowless {
			block, err = r.movedBlockOf(ctx, restore)
		} else {
			block, err = r.restoredAskBlock(ctx, restore, nodes)
		}
		if err != nil {
			return graphstore.NoEvent, err
		}
		added = append(added, block)
		r.result.Restored = append(r.result.Restored, RestoredAsk{
			Ask: restore.row.ID, Shape: restore.work.Shape, From: restore.from,
			To: restore.address(r.spec), Read: restore.read, NewID: restore.id != restore.work.BlockID,
		})
	}
	for _, converted := range r.sections {
		if converted.fate == fateDecision {
			added = append(added, converted.decision)
		}
	}
	pointers := map[string]bool{}
	if r.decisions != nil {
		pointers[r.decisions.refKey] = true
	}
	var refused error
	var pointerLine string
	var headingAdded bool
	err = r.service.applyLive(ctx, r.spec.id, DecisionStepActor, func(doc *crdt.Doc, current pmdoc.ValidDocument, _ func(func(*crdt.Transaction))) error {
		if current.Node() == nil {
			return fmt.Errorf("the spec of %s is outside the Proof schema, so the decisions step cannot write into it", r.issue)
		}
		// kept for one release: stored-ask-state
		withAsk, err := askBlocksWithAnAsk(ctx, r.tx, r.spec.id, current.Node())
		if err != nil {
			return err
		}
		_, err = rewriteLive(doc, nil, func(live *pmdoc.Node) bool {
			for _, block := range added {
				id, _ := block.Attrs[pmdoc.BlockIDAttr].(string)
				if blockIDHeld(live, id) {
					refused = fmt.Errorf("the spec of %s holds block %s already, which the decisions step writes a block under", r.issue, id)
					return false
				}
			}
			changed := r.writePresent(r.spec.id, live, r.present[r.spec.id], withAsk)
			pointerLine, headingAdded = "", false
			if len(added) > 0 {
				headingAdded = !hasDecisionsPart(live)
				var line *pmdoc.Node
				live.Children, line = withDecisionsPartBlocks(live.Children, added, func(node *pmdoc.Node) bool {
					return isPointerLine(node, pointers, r.service.serverURL)
				})
				if line != nil {
					markdown, err := blockMarkdown(line)
					if err != nil {
						refused = fmt.Errorf("render the pointer line the decisions step takes out of the spec of %s: %w", r.issue, err)
						return false
					}
					pointerLine = markdown
				}
				changed = true
			}
			return changed
		})
		return err
	})
	if refused != nil {
		return graphstore.NoEvent, refused
	}
	if errors.Is(err, websocket.ErrNoChanges) {
		return graphstore.NoEvent, nil
	}
	if err != nil {
		return graphstore.NoEvent, err
	}
	r.pointerLine, r.headingAdded = pointerLine, headingAdded
	r.creditSectionAuthors(ctx, r.spec.id)
	return r.writeVersion(ctx, r.spec, &r.result.SpecVersion)
}

// creditSectionAuthors credits each reading and each decision this pass writes into document to
// the author of the section it came from (sectionAuthors), on the live write that wrote it, before
// its version is written: the typed-node pass gives a reading's node, and a decision's, the author
// of the write that introduced its block (refs.DecisionFacts, blockAuthors), and this write is the
// step's, which proves nobody, so the block carries its section's author forward as a stamp's
// rename carries an ask block's (liveWrite.carriedAskAuthors). A section whose author is unknown
// credits nobody, and its node takes no author.
//
// kept for one release: decisions-step
func (r *issueRestore) creditSectionAuthors(ctx context.Context, document string) {
	write := ledgerFrom(ctx).liveWriteFor(document)
	if write == nil {
		return
	}
	authors := map[string]model.Actor{}
	for _, converted := range r.sections {
		if converted.author.Kind == "" && converted.author.ID == "" {
			continue
		}
		switch {
		case converted.fate == fateDecision && document == r.spec.id:
			authors[converted.decisionID] = converted.author
		case converted.fate == fateReading && converted.askDocument.id == document:
			id, _ := converted.reading.Attrs[pmdoc.BlockIDAttr].(string)
			authors[id] = converted.author
		}
	}
	mergeInto(&write.carriedAskAuthors, authors)
}

// writePresent writes edits - the shaped pointers and the readings the conversion gives the
// answered ask blocks a document holds already - into live, the tree of document as its live
// write holds it, and takes the answer's words and selection, which no version holds on a fleet
// of one build - the one the phase runs on - off every ask block of it that has an ask (withAsk,
// askBlocksWithAnAsk: withoutUnkeptAskAttributes), leaving the state, answerer, time and a copy's
// provenance settlement keeps in step with each ask, and every attribute a version can store off
// each other ask block, which has no ask row and is no copy of an ask and keeps none of the eight
// (R10, withoutStoredAskAttributes). It records each pointer it gave a block that
// the block did not hold (shapedAdded), which the version names as the step's own, and each
// reading it wrote (readingsWritten), and reports whether it changed the tree. It visits every
// ask block of the tree: pmdoc.Walk stops at the first node its visit answers false for, so the
// visit answers true at an ask block too, and the blocks after the first are written as well.
//
// kept for one release: decisions-step
func (r *issueRestore) writePresent(document string, live *pmdoc.Node, edits *presentEdits, withAsk map[string]bool) bool {
	changed := false
	added := map[string][]string{}
	written := map[string]bool{}
	pmdoc.Walk(live, func(node *pmdoc.Node) bool {
		if node.Type != "ask" {
			return true
		}
		id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
		if edits != nil {
			if shaped := edits.shaped[id]; len(shaped) > 0 {
				held := shapedPointersOf(node)
				var given []string
				for _, pointer := range r.specPointers(shaped) {
					if !slices.Contains(held, pointer) {
						given = append(given, pointer)
					}
				}
				if len(given) > 0 {
					node.Attrs[shapedAttribute] = append(held, given...)
					added[id] = given
					changed = true
				}
			}
			if readings := edits.readings[id]; len(readings) > 0 {
				node.Children = append(node.Children, readings...)
				for _, reading := range readings {
					readingID, _ := reading.Attrs[pmdoc.BlockIDAttr].(string)
					written[readingID] = true
				}
				changed = true
			}
		}
		// The strip of a block that has no ask row and is no copy of an ask is R10's and stays;
		// the lighter strip of a block that has an ask goes with the form a version keeps.
		strip := withoutStoredAskAttributes
		// kept for one release: stored-ask-state
		if withAsk[id] {
			strip = withoutUnkeptAskAttributes
		}
		if strip(node) {
			changed = true
		}
		return true
	})
	r.shapedAdded[document] = added
	r.readingsWritten[document] = written
	return changed
}

// withoutAsklessAskAttributes takes every attribute a version can store off each ask block of tree
// that withAsk does not name (askBlocksWithAnAsk) - one that has no ask row and is no copy of an
// ask, which keeps none of the eight (R10) - and reports whether it changed one. Its one caller is
// the decisions step's write of the decisions document (writeDecisionsDocument), so it goes with
// the step; the blocks withAsk names are passed over only while a version keeps a form of each
// ask, and that goes earlier.
//
// kept for one release: decisions-step
func withoutAsklessAskAttributes(tree *pmdoc.Node, withAsk map[string]bool) bool {
	changed := false
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type != "ask" {
			return true
		}
		// kept for one release: stored-ask-state
		if id, _ := node.Attrs[pmdoc.BlockIDAttr].(string); withAsk[id] {
			return true
		}
		if withoutStoredAskAttributes(node) {
			changed = true
		}
		return true
	})
	return changed
}

// refuseUnwrittenReadings fails the pass where a section it converts into a reading of an ask
// block a document held already has that reading in no block the pass wrote (readingsWritten):
// the decisions document's write has taken the section out by then, and the moved_to the pass
// would state from its heading names a block that holds none of its words. The failure rolls the
// issue's transaction back, so the section stays where it stood, and no moved_to names it. A
// reading of an ask the pass restores is in the block restoredAskBlock builds, which holds it.
//
// kept for one release: decisions-step
func (r *issueRestore) refuseUnwrittenReadings() error {
	for _, converted := range r.sections {
		if converted.fate != fateReading || r.restoreOf(converted.ask) != nil {
			continue
		}
		id, _ := converted.reading.Attrs[pmdoc.BlockIDAttr].(string)
		if !r.readingsWritten[converted.askDocument.id][id] {
			return fmt.Errorf("the decisions step wrote the reading of the section %q of %s into no block: %s holds no ask block %s to take it, so it converts nothing of %s this pass",
				converted.section.title, r.decisions.address, converted.askDocument.address, converted.askBlock, r.issue)
		}
	}
	return nil
}

// writePresentDocuments writes, into each document of the issue other than the spec that holds an
// answered ask block a converted section restated, the shaped pointers and the readings the
// conversion gives that block (writePresent), and its version, in document id order. R9 gives a
// section's links and reading to the ask's present block wherever it stands in the issue.
//
// kept for one release: decisions-step
func (r *issueRestore) writePresentDocuments(ctx context.Context) error {
	for _, id := range slices.Sorted(maps.Keys(r.present)) {
		if id == r.spec.id {
			continue
		}
		document := r.documents[id]
		edits := r.present[id]
		err := r.service.applyLive(ctx, id, DecisionStepActor, func(doc *crdt.Doc, current pmdoc.ValidDocument, _ func(func(*crdt.Transaction))) error {
			if current.Node() == nil {
				return fmt.Errorf("%s is outside the Proof schema, so the decisions step cannot write into it", document.address)
			}
			// kept for one release: stored-ask-state
			withAsk, err := askBlocksWithAnAsk(ctx, r.tx, id, current.Node())
			if err != nil {
				return err
			}
			_, err = rewriteLive(doc, nil, func(live *pmdoc.Node) bool {
				return r.writePresent(id, live, edits, withAsk)
			})
			return err
		})
		if errors.Is(err, websocket.ErrNoChanges) {
			continue
		}
		if err != nil {
			return err
		}
		var number int
		r.creditSectionAuthors(ctx, id)
		if _, err := r.writeVersion(ctx, document, &number); err != nil {
			return err
		}
		if number > 0 {
			r.result.OtherVersions++
		}
	}
	return nil
}

// blockIDHeld reports whether tree holds a block under id.
//
// kept for one release: decisions-step
func blockIDHeld(tree *pmdoc.Node, id string) bool {
	held := false
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if blockID, _ := node.Attrs[pmdoc.BlockIDAttr].(string); blockID == id {
			held = true
		}
		return !held
	})
	return held
}

// withDecisionsPartBlocks is children, a document's top-level blocks, with blocks put in its
// `## Decisions` part in place of the part's first pointer line (pointer), which it answers, every
// other block of the part kept, and after its last block where it holds none; and, where the
// document holds no such part, at its end under a `## Decisions` heading added for them. It
// answers nil for the pointer line where it took none out.
//
// kept for one release: decisions-step
func withDecisionsPartBlocks(children, blocks []*pmdoc.Node, pointer func(*pmdoc.Node) bool) ([]*pmdoc.Node, *pmdoc.Node) {
	start, end := decisionsPart(&pmdoc.Node{Type: "doc", Children: children})
	if start < 0 {
		heading := &pmdoc.Node{Type: "heading", Attrs: pmdoc.Attrs{"level": decisionsPartLevel},
			Children: []*pmdoc.Node{{Type: "text", Text: decisionsPartTitle}}}
		return append(append(slices.Clone(children), heading), blocks...), nil
	}
	next := slices.Clone(children[:start+1])
	var replaced *pmdoc.Node
	for _, child := range children[start+1 : end] {
		if replaced == nil && pointer(child) {
			replaced = child
			next = append(next, blocks...)
			continue
		}
		next = append(next, child)
	}
	if replaced == nil {
		next = append(next, blocks...)
	}
	return append(next, children[end:]...), replaced
}

// restoredAskBlock is the block restore writes into the spec for an ask its row names. It is the
// block it moves, byte for byte, with every link, code span and sub-list its question and options
// hold - for the main shape the block as the spec's last version holding it held it (held), for
// the second shape, or where the spec never held it, the block as the decisions document holds it
// (moved) - where that block reads back as the wording the ask's row holds (askBlock.asks), so
// the restore returns what the person read (R7); and otherwise the ask's question and options as
// its row holds them. Either way it takes urgency and multiple from the row and shows no answer,
// which its node holds (R6): its opening line keeps the row's state, answerer and time, as every
// version on a fleet of one build - the one the phase runs on - keeps them for the release before's
// readers (keptAskState), and none of the other server-owned attributes. Its `shaped` pointers are
// the block's own and the spec blocks the sections that restate it link (restoredPointers), and it
// holds the readings the block of the decisions document held and the ones the conversion writes.
// It reads back as the row's wording, or is refused.
//
// kept for one release: decisions-step
func (r *issueRestore) restoredAskBlock(ctx context.Context, restore *restoredBlock, nodes map[string]askNodeAnswer) (*pmdoc.Node, error) {
	decided := model.NewAskEditPrevious(restore.row)
	attrs := pmdoc.Attrs{pmdoc.BlockIDAttr: restore.id, "urgency": decided.Urgency, "multiple": decided.Multiple}
	shaped, shape, err := r.restoredPointers(ctx, restore)
	if err != nil {
		return nil, err
	}
	restore.shape = shape
	if len(shaped) > 0 {
		attrs[shapedAttribute] = shaped
	}
	var readings []*pmdoc.Node
	if restore.moved != nil {
		_, readings = pmdoc.TypedContent(restore.moved)
	}
	readings = append(slices.Clone(readings), restore.readings...)
	kept := func(block *pmdoc.Node) *pmdoc.Node {
		// kept for one release: stored-ask-state
		keptAskState(onItsNode(restore.row, nodes), "")(block)
		return block
	}
	source := restore.held
	if restore.work.Shape == graphcensus.RestoreMoves || source == nil {
		source = restore.moved
	}
	if source != nil {
		content, _ := pmdoc.TypedContent(source)
		// A block whose bytes the parse refuses, or that asks other words than the row's, is
		// built from the row below instead.
		if block, asks, err := askBlockFrom(attrs, content, readings, decided); err == nil && asks {
			return kept(block), nil
		}
	}
	budget := pmdoc.NewWriteBudget()
	content, err := questionBlocks(normalizeAskQuestion(decided.Question), budget)
	if err != nil {
		return nil, fmt.Errorf("write the question of ask %s: %w", restore.row.ID, err)
	}
	if list, err := editedOptionList(nil, normalizeAskOptions(decided.Options), budget); err != nil {
		return nil, fmt.Errorf("write the options of ask %s: %w", restore.row.ID, err)
	} else if list != nil {
		content = append(content, list)
	}
	block, asks, err := askBlockFrom(attrs, content, readings, decided)
	if err != nil {
		return nil, fmt.Errorf("write the block of ask %s: %w", restore.row.ID, err)
	}
	if !asks {
		return nil, fmt.Errorf("the restored block of ask %s does not read back as the wording its row holds", restore.row.ID)
	}
	return kept(block), nil
}

// askBlockFrom is the ask block of attrs holding content and then readings, as the document's own
// markdown pipeline makes it (parsedTypedBlock), and whether it asks what decided does
// (askBlock.asks).
//
// kept for one release: decisions-step
func askBlockFrom(attrs pmdoc.Attrs, content, readings []*pmdoc.Node, decided model.AskEditPrevious) (*pmdoc.Node, bool, error) {
	children := append(slices.Clone(content), readings...)
	block, err := parsedTypedBlock(&pmdoc.Node{Type: "ask", Attrs: maps.Clone(attrs), Children: children}, "ask")
	if err != nil {
		return nil, false, err
	}
	read, err := parseAskBlock(block)
	return block, err == nil && read.asks(decided), nil
}

// restoredPointers is the `shaped` value of restore's block, R6's "its `shaped` tuples stay
// intact": the pointers the block held - as the spec last held it (held) and as the decisions
// document holds it (moved) - each as it reads once the pass is written (movedPointer), then the
// spec blocks the sections that restate its ask link (specPointers), each once; and what the pass
// did to the pointers the spec last held the block with (restoredShape) - each it rewrote, and
// each other pointer it gives the block - which restoreCase leaves out of its comparison with the
// block the person approved.
//
// kept for one release: decisions-step
func (r *issueRestore) restoredPointers(ctx context.Context, restore *restoredBlock) ([]string, restoredShape, error) {
	var pointers []string
	var shape restoredShape
	if restore.held != nil {
		for _, pointer := range shapedPointersOf(restore.held) {
			to, stands, err := r.movedPointer(ctx, pointer)
			if err != nil {
				return nil, restoredShape{}, err
			}
			if !stands {
				to = ""
			}
			if to != pointer {
				if shape.moved == nil {
					shape.moved = map[string]string{}
				}
				shape.moved[pointer] = to
			}
			if to != "" {
				pointers = appendUnique(pointers, to)
			}
		}
	}
	own := len(pointers)
	if restore.moved != nil {
		moved, err := r.movedPointers(ctx, shapedPointersOf(restore.moved))
		if err != nil {
			return nil, restoredShape{}, err
		}
		pointers = appendUnique(pointers, moved...)
	}
	pointers = appendUnique(pointers, r.specPointers(restore.shaped)...)
	shape.added = pointers[own:len(pointers):len(pointers)]
	return pointers, shape, nil
}

// movedPointers is pointers, the `shaped` values of a block the restore moves, each as it reads
// once the pass is written (movedPointer), without each that then names no block.
//
// kept for one release: decisions-step
func (r *issueRestore) movedPointers(ctx context.Context, pointers []string) ([]string, error) {
	moved := make([]string, 0, len(pointers))
	for _, pointer := range pointers {
		to, stands, err := r.movedPointer(ctx, pointer)
		if err != nil {
			return nil, err
		}
		if stands {
			moved = appendUnique(moved, to)
		}
	}
	return moved, nil
}

// movedPointer is pointer, a `shaped` value of a block the restore moves, as it reads once the
// pass is written, and whether it names a block then. A pointer to a block of the decisions
// document that this pass moves names the block it moves to: the ask block the restore writes
// into the spec, or the decision or the ask block a section it converts becomes, as writeFacts'
// moved_to tuples state it (passMove). One to any other block this pass takes out of that
// document names none, nor does one whose section's moved_to reaches an ask, which no `shaped`
// value names. One to a block the document no longer holds names the block its moved_to tuples
// reach (graphstore.MovedTo), and none where they reach no block. Every other pointer, one to a
// block the decisions document still holds among them, reads as it is.
//
// kept for one release: decisions-step
func (r *issueRestore) movedPointer(ctx context.Context, pointer string) (string, bool, error) {
	if r.decisions == nil {
		return pointer, true, nil
	}
	cited := text.Extract(pointer, r.service.serverURL)
	if len(cited) != 1 || cited[0].Kind != "block" {
		return pointer, true, nil
	}
	address := text.TargetID(cited[0])
	id, ofDecisions := strings.CutPrefix(address, r.decisions.refKey+"#")
	if !ofDecisions {
		return pointer, true, nil
	}
	if to, taken := r.passMove(address, id); taken {
		return to, to != "", nil
	}
	reached, ok, err := graphstore.MovedTo(ctx, r.tx, address)
	if err != nil || !ok || reached.Ask != "" {
		return "", false, err
	}
	if reached.Address == address {
		return pointer, true, nil
	}
	to, stands := r.blockPointer(reached.Address)
	return to, stands, nil
}

// passMove is where this pass moves the block of the decisions document at address, the store's
// address of its block id, named as a `shaped` value names it ("" for a section's heading whose
// moved_to reaches an ask, or a block of a converted section that no moved_to leaves), and whether
// this pass takes that block out of the document at all (decisionsDropped).
//
// kept for one release: decisions-step
func (r *issueRestore) passMove(address, id string) (string, bool) {
	for _, restore := range r.restores {
		if restore.from == address {
			return refs.BlockRef(r.spec.address, restore.id), true
		}
	}
	for _, converted := range r.sections {
		if refs.BlockAddress(r.decisions.refKey, converted.section.headingID()) != address {
			continue
		}
		switch converted.fate {
		case fateDecision:
			return refs.BlockRef(r.spec.address, converted.decisionID), true
		case fateReading, fateDropped:
			return refs.BlockRef(converted.askDocument.address, converted.askBlock), true
		}
	}
	return "", r.decisionsDropped[id]
}

// blockPointer is the `shaped` value naming the block at address, a store address
// (refs.BlockAddress), and whether address is one of a document of the issue that stands.
//
// kept for one release: decisions-step
func (r *issueRestore) blockPointer(address string) (string, bool) {
	refKey, id, found := strings.Cut(address, "#")
	if !found {
		return "", false
	}
	for _, document := range r.documents {
		if document.refKey == refKey {
			return refs.BlockRef(document.address, id), true
		}
	}
	return "", false
}

// movedBlockOf is the block the restore writes into the spec for restore, a block of the decisions
// document no row names: the block as that document's live tree holds it, under the id it takes,
// its `shaped` pointers each as it reads once the pass is written (movedPointers), read back as
// the document's own markdown pipeline makes it. It holds what a version of the spec on a fleet of
// one build - the one the phase runs on - stores of it, as the spec's settlement and a version of
// the spec read the ask of the block once it stands there (askBlockAsks). A copy of an ask another
// document of the issue holds takes the form a version keeps of its source - the state where that
// is not open, the answerer and time, and copied_from and copied_from_document - as the source's
// node holds it when the step runs (keptFromItsNode), and holds no answer's words or selection and
// no open state (keptAskState). The copy's own attributes are not that form: they show the source
// as the copy's last settlement read it, and an answer or a resolution of the source arms the
// copy's next settlement for after its commit (SettleCopiesOf), which the step can run before. A
// block the spec's own row names loses the answer's words and selection and an open state
// (withoutUnkeptAskAttributes). A block that is neither loses every attribute a version can store
// (withoutStoredAskAttributes, R10): no ask's state stands behind them, and the settlement after
// the write opens its ask and writes that state. No row holds its question to build it from, and
// none names it to rebind.
//
// kept for one release: decisions-step
func (r *issueRestore) movedBlockOf(ctx context.Context, restore *restoredBlock) (*pmdoc.Node, error) {
	node := &pmdoc.Node{Type: "ask", Attrs: maps.Clone(restore.moved.Attrs), Children: restore.moved.Children}
	node.Attrs[pmdoc.BlockIDAttr] = restore.id
	if held := shapedPointersOf(restore.moved); len(held) > 0 {
		pointers, err := r.movedPointers(ctx, held)
		if err != nil {
			return nil, err
		}
		delete(node.Attrs, shapedAttribute)
		if len(pointers) > 0 {
			node.Attrs[shapedAttribute] = pointers
		}
		// The spec held no block of it: every pointer it carries in is the pass's own.
		restore.shape = restoredShape{added: pointers}
	}
	// The strip of a block that is no copy is R10's and stays; the source's form a copy takes goes
	// with the form a version keeps.
	// kept for one release: stored-ask-state
	rows, sources, err := askBlockAsks(ctx, r.tx, r.spec.id, &pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{node}})
	if err != nil {
		return nil, err
	}
	_, indexed := rows[restore.id]
	source, copied := sources[restore.id]
	switch {
	// kept for one release: stored-ask-state
	case copied:
		nodes, err := askNodeAnswers(ctx, r.tx, []string{source.ask.ID})
		if err != nil {
			return nil, err
		}
		keptAskState(keptFromItsNode(source.ask, nodes), source.document)(node)
	// kept for one release: stored-ask-state
	case indexed:
		withoutUnkeptAskAttributes(node)
	default:
		withoutStoredAskAttributes(node)
	}
	return movedAskBlock(node, restore)
}

// movedAskBlock is node, the block movedBlockOf builds for restore, read back as the document's
// own markdown pipeline makes it.
//
// kept for one release: decisions-step
func movedAskBlock(node *pmdoc.Node, restore *restoredBlock) (*pmdoc.Node, error) {
	block, err := parsedTypedBlock(node, "ask")
	if err != nil {
		return nil, fmt.Errorf("write the ask block %s of the decisions document, which no row names, into the spec: %w", restore.work.BlockID, err)
	}
	return block, nil
}

// newestApproval is the spec's newest review where it is an approval, of whatever version, or nil:
// the approval the restore's version can carry (carryNewestApproval reads the same review), which
// an agent's move of an ask out of the spec left behind its own version, and which the restore
// carries back or supersedes (R7).
func newestApproval(ctx context.Context, tx pgx.Tx, artifactID string) (*model.SupersededApproval, error) {
	review, err := NewestReview(ctx, tx, artifactID)
	if err != nil || review == nil || review.State != "approved" {
		return nil, err
	}
	return &model.SupersededApproval{ReviewID: review.ID, ApprovedVersion: review.Version, ApprovedBy: review.Actor, Kind: review.Kind}, nil
}

// reportApprovals records what the pass did to each document's approval: the spec's approval -
// its newest review where that is an approval (newestApproval) - carried to the version the pass
// wrote, or, where the version does not carry it, superseded, with the asks whose restore wrote
// that version, each as its node and its block; and the decisions document's approval, which
// stood at its latest version and which the version the pass wrote of it superseded, with the
// asks it moved out.
//
// kept for one release: decisions-step
func (r *issueRestore) reportApprovals(ctx context.Context, spec, decisions *model.SupersededApproval) error {
	askRefs := make([]string, 0, len(r.restores))
	nodes := make([]string, 0, len(r.restores))
	var movedRefs, movedNodes []string
	for _, restore := range r.restores {
		if restore.rowless {
			continue
		}
		ask := refs.AskRef(text.Ref{IssueKey: r.issue, ID: restore.row.ID})
		askRefs = append(askRefs, ask)
		nodes = append(nodes, refs.BlockRef(r.spec.address, restore.id))
		if restore.from != "" {
			movedRefs = append(movedRefs, ask)
			movedNodes = append(movedNodes, refs.BlockRef(r.decisions.address, restore.work.BlockID))
		}
	}
	if r.result.SpecVersion > 0 {
		newest, err := NewestReview(ctx, r.tx, r.spec.id)
		if err != nil {
			return err
		}
		switch {
		case newest != nil && newest.State == "approved" && newest.Version == r.result.SpecVersion:
			r.result.SpecApproval = "carried"
		case spec != nil:
			r.result.SpecApproval = "superseded"
			r.result.Superseded = append(r.result.Superseded, DecisionSupersededApproval{
				Document: r.spec.address, Approval: *spec, Asks: askRefs, Nodes: nodes,
			})
		}
	}
	if r.result.DecisionsVersion > 0 && decisions != nil {
		r.result.Superseded = append(r.result.Superseded, DecisionSupersededApproval{
			Document: r.decisions.address, Approval: *decisions, Asks: movedRefs, Nodes: movedNodes,
		})
	}
	return nil
}
