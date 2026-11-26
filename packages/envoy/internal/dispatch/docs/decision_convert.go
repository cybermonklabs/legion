package docs

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/decisionread"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// planSections reads tree, the decisions document as its live write holds it, into what this
// pass does with it, and answers the block ids the document loses: each ask block the restore
// moves into the spec, and every block of each section the pass converts.
//
// kept for one release: decisions-step
func (r *issueRestore) planSections(ctx context.Context, tree *pmdoc.Node) (map[string]bool, error) {
	drop := map[string]bool{}
	movedBy := map[string]*restoredBlock{}
	for _, restore := range r.restores {
		if restore.work.Shape == graphcensus.RestoreReturns && r.ownedByDecisions(restore.work.BlockID) {
			// The block the document holds under that id is an ask of its own, which its own
			// work item moves; it is no copy of this one.
			continue
		}
		pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
			id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
			if node.Type != "ask" || id != restore.work.BlockID || restore.moved != nil {
				return true
			}
			// The ask's own block of the second shape, or a copy of an ask of the main shape the
			// document holds under that ask's block id: either moves into the spec.
			restore.moved = node
			if restore.from == "" {
				restore.from = refs.BlockAddress(r.decisions.refKey, id)
			}
			movedBy[id] = restore
			drop[id] = true
			return false
		})
	}
	// A block no row names moves only where the live tree still holds it: nothing else holds what
	// to write.
	r.restores = slices.DeleteFunc(r.restores, func(restore *restoredBlock) bool {
		return restore.rowless && restore.moved == nil
	})
	sections := decisionSectionsOf(tree)
	ties, err := readSectionTies(ctx, r.tx, r.decisions.id, r.spec.id)
	if err != nil {
		return nil, err
	}
	if r.indexedBlocks, err = refs.BlockChanges(ctx, r.tx, r.decisions.id); err != nil {
		return nil, err
	}
	var indexed *int
	if err := r.tx.QueryRow(ctx, `select version from document_block_versions where artifact_id = $1::uuid`,
		r.decisions.id).Scan(&indexed); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("read the version the block index of the decisions document of %s stands at: %w", r.issue, err)
	}
	if indexed != nil {
		r.indexedVersion = *indexed
	}
	authors, err := r.sectionAuthors(ctx, sections)
	if err != nil {
		return nil, err
	}
	keeps, err := r.sectionKeeps(ctx, tree, sections)
	if err != nil {
		return nil, err
	}
	for index, section := range sections {
		converted := &convertedSection{section: section, author: authors[index], keep: keeps[section.headingID()]}
		r.sections = append(r.sections, converted)
		if err := r.planSection(ctx, converted, ties, movedBy); err != nil {
			return nil, err
		}
		switch converted.fate {
		case fateUntouched, fateLeft:
			r.sectionsLeft++
			if converted.fate == fateLeft {
				r.result.Left++
			}
			continue
		case fateDecision:
			r.result.Decisions++
		case fateReading:
			r.result.Readings++
		case fateDropped:
			r.result.Dropped++
		case fateRemovedAsk:
			r.result.Removed++
		}
		for _, id := range section.blockIDs() {
			drop[id] = true
		}
	}
	return drop, nil
}

// planSection decides what becomes of one section (R9), and records the spec blocks it links as
// the `shaped` pointers of the ask it restates. It is the one place that orders R9's rules.
//
// Every ask block the section holds, wherever in its body, moves into the spec first (the second
// shape of the restore, which moves every ask block of the document: planDocumentMoves), and the
// rest of the section converts by the rules below. The asks a section is about are the asks whose
// blocks it held and the answered asks it cites. A section that cites one answered ask a person
// removed from its spec (removed: its node holds an asserted removed_by, or this pass declined to
// restore it) is deleted by R9's rule for it (fateRemovedAsk): the person removed the decision it
// reads, so no block takes its links or its words, which stay in the decisions document's
// versions, and its heading moves to the ask itself (writeFacts). A section under `## Replaced`
// otherwise becomes a decision with replaced="true" whichever ask it is about (planReplaced): that
// rule comes before the reading rule. A section about one answered ask restates it
// (planRestating): that ask's block - the one the restore writes into the spec, or the one a
// document of the issue holds already (askBlockOf) - takes the section's links as `shaped`
// pointers, and the section's body becomes a reading in it; a body that is the ask's answer and
// nothing more, or nothing at all, is dropped, and writes no reading (the plain-answer rule). A
// section about no answered ask becomes a decision, and one that held one ask block holding no
// answer and nothing else is dropped, its heading moving to that block.
//
// R9's further kinds each become a decision that waits for a person's review (planUnplaced): a
// section about more than one answered ask, under `## Replaced` or not, one whose answered ask has
// its block in no document of the issue (an ask of the issue no block holds), and one whose body a
// reading cannot hold (a reading holds paragraphs alone, and only in an answered ask: a reading
// reads an answer).
//
// A section whose body, the blocks that move taken out, still holds an ask block - one the
// document's block index does not hold yet, which no move takes - is left where it is: a
// conversion of it would take that block out of the document and write it nowhere (R6, R8). The
// next pass, once the index holds the block, moves it and converts the section.
//
// A restore alone converts only a section that holds a block it moves, so no section is left
// holding nothing but its heading, and it writes no decision, since only the conversion's case
// carries an approval across one (conversionCase); it still gives a restored answered ask the
// links of every section that restates it, as R6 says. It converts a section into a reading only
// of an ask it restores, whose block takes the section's links beside it: a section about an ask a
// document holds already is left, with its links, for the conversion, which gives them to that
// block (planRestating).
//
// kept for one release: decisions-step
func (r *issueRestore) planSection(ctx context.Context, converted *convertedSection, ties sectionTies, movedBy map[string]*restoredBlock) error {
	section := converted.section
	moving := func(node *pmdoc.Node) bool {
		id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
		_, moves := movedBy[id]
		return moves && node.Type == "ask"
	}
	var heldBlocks []*restoredBlock
	var held []string
	for _, block := range section.body {
		pmdoc.Walk(block, func(node *pmdoc.Node) bool {
			if moving(node) {
				id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
				heldBlocks = append(heldBlocks, movedBy[id])
				if !movedBy[id].rowless {
					held = append(held, movedBy[id].row.ID)
				}
			}
			return true
		})
	}
	body := section.body
	if len(heldBlocks) > 0 {
		left, _ := withoutBlocks(&pmdoc.Node{Type: "doc", Children: section.body}, moving)
		body = left.Children
	}
	if slices.ContainsFunc(body, pmdoc.HoldsAskBlock) {
		converted.fate = r.leftUnlessRestoring(len(heldBlocks) > 0)
		return nil
	}
	converted.links = tiesOf(section, ties.spec)
	about := slices.Clone(held)
	for _, ask := range tiesOf(section, ties.asks) {
		if row, found := r.rows[ask]; found && row.State == asks.StateAnswered {
			about = appendUnique(about, ask)
		}
	}
	var answered []string
	for _, ask := range about {
		if r.rows[ask].State == asks.StateAnswered {
			answered = append(answered, ask)
		}
	}
	unplaced := unplacedSection{asks: about, held: len(heldBlocks) > 0, mentions: ties.asks}
	switch {
	case len(answered) > 1:
		unplaced.cause = unplacedAsks
	case len(answered) == 1 && !slices.Contains(held, answered[0]) && r.removed[answered[0]]:
		converted.fate = r.leftUnlessRestoring(unplaced.held)
		if r.convert {
			converted.fate, converted.ask = fateRemovedAsk, answered[0]
		}
		return nil
	case section.replaced:
		return r.planReplaced(ctx, converted, answered, body, unplaced.held)
	case len(answered) == 1:
		return r.planRestating(ctx, converted, answered[0], body, unplaced)
	case len(heldBlocks) == 0:
		if !r.convert {
			return nil
		}
		return r.planDecision(ctx, converted, body, nil)
	case len(heldBlocks) == 1 && len(body) == 0:
		converted.fate, converted.ask = fateDropped, heldBlocks[0].row.ID
		converted.askDocument, converted.askBlock = r.spec, heldBlocks[0].id
		return nil
	default:
		unplaced.cause = unplacedBody
	}
	return r.planUnplaced(ctx, converted, body, unplaced)
}

// planRestating decides what becomes of converted, a section about one answered ask, ask, that no
// rule before the reading rule took (planSection), and whose body, the blocks that move taken out,
// is body.
//
// kept for one release: decisions-step
func (r *issueRestore) planRestating(ctx context.Context, converted *convertedSection, ask string, body []*pmdoc.Node, unplaced unplacedSection) error {
	document, blockID, stands := r.askBlockOf(ask)
	if !stands {
		unplaced.cause = unplacedNoBlock
		return r.planUnplaced(ctx, converted, body, unplaced)
	}
	answered := r.answered[ask]
	fate := fateReading
	switch {
	case len(body) == 0 || answered && plainAnswer(body, r.rows[ask]):
		fate = fateDropped
	case !answered || !allParagraphs(body):
		unplaced.cause = unplacedBody
		return r.planUnplaced(ctx, converted, body, unplaced)
	}
	// An ask that holds no answer takes no pointer: what an answer shaped is the answer's.
	restore := r.restoreOf(ask)
	switch {
	case !answered:
	case restore != nil:
		restore.shaped = appendUnique(restore.shaped, converted.links...)
	case r.convert:
		edits := r.presentIn(document.id)
		edits.shaped[blockID] = appendUnique(edits.shaped[blockID], converted.links...)
	}
	// A restore alone gives a section's links to no block it does not write: they are the
	// conversion's to give to the ask's present block, so a section about an ask this pass does not
	// restore keeps its links and its words where they stand for the conversion, even where it held
	// a block that moves.
	if !r.convert && restore == nil {
		converted.fate = r.leftUnlessRestoring(unplaced.held)
		return nil
	}
	if !r.convert && !unplaced.held {
		return nil
	}
	converted.fate, converted.ask = fate, ask
	converted.askDocument, converted.askBlock = document, blockID
	if fate != fateReading {
		return nil
	}
	reading, err := readingOf(body)
	if err != nil {
		return err
	}
	converted.reading = reading
	if restore != nil {
		restore.readings = append(restore.readings, reading)
	} else {
		edits := r.presentIn(document.id)
		edits.readings[blockID] = append(edits.readings[blockID], reading)
	}
	return nil
}

// planReplaced makes converted, a section under `## Replaced` about at most one answered ask
// (answered), a decision with replaced="true" (planDecision), whatever its body holds: R9's rule
// for such a section comes before its reading rule. Its decision keeps the section's heading block
// id and takes its links as `shaped`, as every section that becomes a decision does, and settles
// the one answered ask it is about, where it is about one, as its from_ask. It writes no reading
// under that ask and gives the ask none of the section's links, and its heading moves to the
// decision, not to the ask (writeFacts). A restore alone writes no decision, and leaves the section
// to the conversion.
//
// kept for one release: decisions-step
func (r *issueRestore) planReplaced(ctx context.Context, converted *convertedSection, answered []string, body []*pmdoc.Node, held bool) error {
	if !r.convert {
		converted.fate = r.leftUnlessRestoring(held)
		return nil
	}
	if len(answered) == 1 {
		converted.ask = answered[0]
	}
	return r.planDecision(ctx, converted, body, nil)
}

// unplacedSection is a section the conversion could not place under one ask: why (cause), the
// asks it is about - the asks whose blocks it held and the answered asks it cites - whether it held
// an ask block, which moved into the spec, and the asks each block of the decisions document cites
// (sectionTies.asks).
//
// kept for one release: decisions-step
type unplacedSection struct {
	cause    string
	asks     []string
	held     bool
	mentions map[string][]string
}

// Why the conversion could not place a section under one ask (unplacedReason).
//
// kept for one release: decisions-step
const (
	unplacedAsks    = "it cites more than one answered ask"
	unplacedNoBlock = "the answered ask it cites has its block in no document of the issue"
	unplacedBody    = "its body is no reading one answered ask can hold"
)

// unplacedReason is the uncertainty_reason of the decision a section becomes that the conversion
// could not place under one ask for cause.
//
// kept for one release: decisions-step
func unplacedReason(cause string) string {
	return "The conversion of the decisions document could not place this section under one ask: " + cause +
		". It waits for a person's review."
}

// planUnplaced makes converted, a section the conversion could not place under one ask, a decision
// that waits for a person's review (planDecision). A restore alone writes no decision, and leaves
// the section to the conversion.
//
// kept for one release: decisions-step
func (r *issueRestore) planUnplaced(ctx context.Context, converted *convertedSection, body []*pmdoc.Node, unplaced unplacedSection) error {
	if !r.convert {
		converted.fate = r.leftUnlessRestoring(unplaced.held)
		return nil
	}
	return r.planDecision(ctx, converted, body, &unplaced)
}

// leftUnlessRestoring is fateLeft for a section the conversion reads, and fateUntouched for one a
// restore alone reads and that held no block it moves.
//
// kept for one release: decisions-step
func (r *issueRestore) leftUnlessRestoring(holds bool) sectionFate {
	if !r.convert && !holds {
		return fateUntouched
	}
	return fateLeft
}

// planDecision makes converted a decision block of the spec's `## Decisions` part: its words are
// the section's body (body) with its heading as the first paragraph; a section under
// `## Replaced` is replaced="true", and from_ask names the answered ask its decision settles
// (converted.ask, planReplaced), where it settles one; its links are its shaped pointers. It keeps
// the section's heading block id where the spec holds no block under it. A section that cites no
// answered ask, or one under `## Replaced` (unplaced nil), takes its `Unsure:` paragraph as
// uncertainty="high", with the rest of it as uncertainty_reason. One the conversion could not
// place under one ask takes uncertainty="high" with the reason why (unplacedReason), keeps any
// `Unsure:` paragraph among its words, and closes with a paragraph citing each ask it is about
// that its words do not cite (uncitedAsks); it takes no from_ask, since it settles no one ask.
//
// A block of the body a decision cannot hold is left out of it (holdableWords), and the decision
// then ends with a citation of the section in the decisions document's last version that held it
// (sectionVersion), whose text stays in that version once the document retires. An ask block the
// section held is no such part: it stands in the spec. Where no version holds the section yet, the
// section is left for the next pass.
//
// kept for one release: decisions-step
func (r *issueRestore) planDecision(ctx context.Context, converted *convertedSection, body []*pmdoc.Node, unplaced *unplacedSection) error {
	section := converted.section
	id := section.headingID()
	if id == "" || r.specBlocks[id] || r.restoreHolds(id) {
		var err error
		if id, err = r.freshBlockID(ctx); err != nil {
			return err
		}
	}
	attrs := pmdoc.Attrs{pmdoc.BlockIDAttr: id}
	words := body
	if unplaced != nil {
		attrs["uncertainty"], attrs["uncertainty_reason"] = "high", unplacedReason(unplaced.cause)
	} else if unsure, reason := section.unsure(); unsure != nil {
		words = slices.DeleteFunc(slices.Clone(body), func(block *pmdoc.Node) bool { return block == unsure })
		attrs["uncertainty"] = "high"
		if reason != "" {
			attrs["uncertainty_reason"] = reason
		}
	}
	if section.replaced {
		attrs["replaced"] = true
	}
	if converted.ask != "" {
		attrs["from_ask"] = refs.AskRef(text.Ref{IssueKey: r.issue, ID: converted.ask})
	}
	if len(converted.links) > 0 {
		attrs[shapedAttribute] = r.specPointers(converted.links)
	}
	heading := &pmdoc.Node{Type: "paragraph", Attrs: pmdoc.Attrs{}, Children: section.heading.Children}
	decision, err := parsedTypedBlock(decisionNode(attrs, heading, words), decisionType)
	leftOut := false
	if err != nil {
		words, leftOut = holdableWords(attrs, heading, words)
	}
	var closing []*pmdoc.Node
	if unplaced != nil {
		if uncited := r.uncitedAsks(section, words, *unplaced); len(uncited) > 0 {
			closing = append(closing, textParagraph("The asks this section cites: "+strings.Join(uncited, ", ")+"."))
		}
	}
	if leftOut {
		version := r.sectionVersion(section)
		if version == 0 {
			converted.fate = fateLeft
			return nil
		}
		closing = append(closing, textParagraph("The section as the decisions document held it: "+
			refs.BlockRef(r.decisions.address, section.headingID())+"@v"+strconv.Itoa(version)+"."))
	}
	if len(closing) > 0 {
		decision, err = parsedTypedBlock(decisionNode(attrs, heading, append(slices.Clone(words), closing...)), decisionType)
	}
	if err != nil {
		return fmt.Errorf("write the section %q of the decisions document of %s as a decision: %w", section.title, r.issue, err)
	}
	converted.fate, converted.decision, converted.decisionID = fateDecision, decision, id
	return nil
}

// decisionNode is a decision block of attrs holding heading and then words.
//
// kept for one release: decisions-step
func decisionNode(attrs pmdoc.Attrs, heading *pmdoc.Node, words []*pmdoc.Node) *pmdoc.Node {
	return &pmdoc.Node{Type: decisionType, Attrs: attrs, Children: append([]*pmdoc.Node{heading}, words...)}
}

// holdableWords is words without each block a decision block of attrs cannot hold after heading,
// as the document's own markdown pipeline reads one (parsedTypedBlock), and whether it left one
// out.
//
// kept for one release: decisions-step
func holdableWords(attrs pmdoc.Attrs, heading *pmdoc.Node, words []*pmdoc.Node) ([]*pmdoc.Node, bool) {
	var kept []*pmdoc.Node
	for _, block := range words {
		if _, err := parsedTypedBlock(decisionNode(attrs, heading, []*pmdoc.Node{block}), decisionType); err == nil {
			kept = append(kept, block)
		}
	}
	return kept, len(kept) < len(words)
}

// textParagraph is a paragraph holding words as plain text.
//
// kept for one release: decisions-step
func textParagraph(words string) *pmdoc.Node {
	return &pmdoc.Node{Type: "paragraph", Attrs: pmdoc.Attrs{}, Children: []*pmdoc.Node{{Type: "text", Text: words}}}
}

// uncitedAsks is the address (dispatch://KEY/ask/<id>) of each ask unplaced is about that the
// section's heading and the blocks of words do not cite, as the decisions document's block index
// read their citations (unplacedSection.mentions): an ask whose block the section held, and one
// cited only in a block the decision leaves out.
//
// kept for one release: decisions-step
func (r *issueRestore) uncitedAsks(section decisionSection, words []*pmdoc.Node, unplaced unplacedSection) []string {
	cited := map[string]bool{}
	cite := func(id string) {
		for _, ask := range unplaced.mentions[id] {
			cited[ask] = true
		}
	}
	cite(section.headingID())
	for _, block := range words {
		pmdoc.Walk(block, func(node *pmdoc.Node) bool {
			if id, _ := node.Attrs[pmdoc.BlockIDAttr].(string); id != "" {
				cite(id)
			}
			return true
		})
	}
	var addresses []string
	for _, ask := range unplaced.asks {
		if !cited[ask] {
			addresses = append(addresses, refs.AskRef(text.Ref{IssueKey: r.issue, ID: ask}))
		}
	}
	return addresses
}

// sectionVersion is the decisions document's last version that holds section, as its block index
// reads it: the version the index stands at where it holds the section's heading, and 0 where the
// index holds no such version.
//
// kept for one release: decisions-step
func (r *issueRestore) sectionVersion(section decisionSection) int {
	if _, held := r.indexedBlocks[section.headingID()]; !held || section.headingID() == "" {
		return 0
	}
	return r.indexedVersion
}

// restoreHolds reports whether a block this pass restores takes id.
//
// kept for one release: decisions-step
func (r *issueRestore) restoreHolds(id string) bool {
	for _, restore := range r.restores {
		if restore.id == id {
			return true
		}
	}
	return false
}

// specPointers is the `shaped` value naming each spec block of ids, as a `block[]` attribute
// writes it (pmdoc.ParseBlockPointerValue): canonical block references, each once.
//
// kept for one release: decisions-step
func (r *issueRestore) specPointers(ids []string) []string {
	pointers := make([]string, 0, len(ids))
	for _, id := range ids {
		pointers = appendUnique(pointers, refs.BlockRef(r.spec.address, id))
	}
	return pointers
}

// appendUnique appends each of values to list that it does not hold yet.
//
// kept for one release: decisions-step
func appendUnique(list []string, values ...string) []string {
	for _, value := range values {
		if !slices.Contains(list, value) {
			list = append(list, value)
		}
	}
	return list
}

// plainAnswer reports whether body is ask's answer and nothing more (R9's plain-answer rule): its
// text, and the answer's, are the same once whitespace is collapsed and case folded. The answer is
// the person's words, or, where they wrote none, the options they chose.
//
// kept for one release: decisions-step
func plainAnswer(body []*pmdoc.Node, ask model.Ask) bool {
	if ask.Answer == nil {
		return false
	}
	answer := strings.Join(ask.Answer.Selected, ", ")
	if ask.Answer.Text != nil && strings.TrimSpace(*ask.Answer.Text) != "" {
		answer = *ask.Answer.Text
	}
	parts := make([]string, len(body))
	for index, block := range body {
		parts[index] = pmdoc.TextContent(block)
	}
	normalized := normalizedWords(strings.Join(parts, " "))
	return normalized != "" && normalized == normalizedWords(answer)
}

// normalizedWords is value with its whitespace collapsed to single spaces, trimmed, and its case
// folded.
//
// kept for one release: decisions-step
func normalizedWords(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}

// allParagraphs reports whether every block of body is a paragraph, which is all a reading holds.
//
// kept for one release: decisions-step
func allParagraphs(body []*pmdoc.Node) bool {
	for _, block := range body {
		if block.Type != "paragraph" {
			return false
		}
	}
	return true
}

// readingOf is a reading block holding body's paragraphs, under a fresh block id, as the document's
// own markdown pipeline makes it. A reading stands only at the end of a decision or an ask, so it is
// parsed at the end of a decision that holds it and nothing else of weight, and taken out of it.
//
// kept for one release: decisions-step
func readingOf(body []*pmdoc.Node) (*pmdoc.Node, error) {
	reading := &pmdoc.Node{
		Type: readingType, Attrs: pmdoc.Attrs{pmdoc.BlockIDAttr: uuid.NewString()}, Children: slices.Clone(body),
	}
	holder, err := parsedTypedBlock(&pmdoc.Node{
		Type: decisionType, Attrs: pmdoc.Attrs{pmdoc.BlockIDAttr: uuid.NewString()},
		Children: []*pmdoc.Node{{Type: "paragraph", Attrs: pmdoc.Attrs{}, Children: []*pmdoc.Node{{Type: "text", Text: "holder"}}}, reading},
	}, decisionType)
	if err != nil {
		return nil, err
	}
	_, readings := pmdoc.TypedContent(holder)
	if len(readings) != 1 || readings[0].Type != readingType {
		return nil, errors.New("the reading does not read back as one reading block")
	}
	return readings[0], nil
}

// readingType is the type of an agent's reading block.
//
// kept for one release: decisions-step
const readingType = "reading"

// parsedTypedBlock is node, a typed block of typ built here, as the document's own markdown
// pipeline makes it (parsedBlockBody): rendered and read back, so it carries the attributes and
// the inner block ids a parse gives rather than ones a hand-built node invents, with its own id,
// which its markdown writes, kept.
//
// kept for one release: decisions-step
func parsedTypedBlock(node *pmdoc.Node, typ string) (*pmdoc.Node, error) {
	parsed, refusal, err := parsedBlockBody([]*pmdoc.Node{node}, pmdoc.NewWriteBudget())
	if err != nil {
		return nil, err
	}
	if refusal != "" {
		return nil, errors.New(refusal)
	}
	if len(parsed) != 1 || parsed[0].Type != typ {
		return nil, fmt.Errorf("the %s block does not read back as one %s block", typ, typ)
	}
	return parsed[0], nil
}

// sectionAuthors is the author of each section's newest version: the first author
// artifact_versions.authors names on the newest version of the decisions document that changed
// the section's text, which is the greatest version the block index records as changing a block of
// the section (indexedBlocks, refs.BlockChanges), as the Decisions read of the release before dated
// a section. A section whose blocks the index records no change of, or whose version names no
// author, has none.
//
// kept for one release: decisions-step
func (r *issueRestore) sectionAuthors(ctx context.Context, sections []decisionSection) ([]model.Actor, error) {
	changes := r.indexedBlocks
	versions := make([]int, len(sections))
	wanted := map[int]bool{}
	for index, section := range sections {
		versions[index] = sectionChangedVersion(changes, section.blockIDs())
		if versions[index] > 0 {
			wanted[versions[index]] = true
		}
	}
	authors := make([]model.Actor, len(sections))
	if len(wanted) == 0 {
		return authors, nil
	}
	rows, err := r.tx.Query(ctx, `
		select number, authors from artifact_versions where artifact_id = $1::uuid and number = any($2::int[])
	`, r.decisions.id, slices.Sorted(maps.Keys(wanted)))
	if err != nil {
		return nil, fmt.Errorf("read the authors of the decisions document of %s: %w", r.issue, err)
	}
	firsts := map[int]model.Actor{}
	for rows.Next() {
		var number int
		var listed []model.Actor
		if err := rows.Scan(&number, &listed); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read the authors of the decisions document of %s: %w", r.issue, err)
		}
		if len(listed) > 0 {
			firsts[number] = listed[0]
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the authors of the decisions document of %s: %w", r.issue, err)
	}
	for index := range sections {
		authors[index] = firsts[versions[index]]
	}
	return authors, nil
}

// sectionKeeps is each standing Keep a person recorded on a heading of the decisions document
// (decision_keeps, which Keep writes for such a heading until the document is retired, F3), by
// heading block id, with whether the sum it recorded is the section's text sum as it stands: the
// sum the release before's Decisions read served, over the section's markdown from its heading to
// its last block (decisionread.TextSum), which the Keep posts back. A Keep recorded before a
// renderer change sums the section in its renderer's spelling, so a Keep whose sum is not the
// section's as it stands matches too where the version it names holds that text in another
// renderer version's spelling (keptAcrossRenderers). Each carries the actor of the newest
// decision.kept the issue holds for its heading, the event Keep appended in the transaction that
// wrote the row (sectionKeep.keeper).
//
// kept for one release: decisions-step
func (r *issueRestore) sectionKeeps(ctx context.Context, tree *pmdoc.Node, sections []decisionSection) (map[string]*sectionKeep, error) {
	rows, err := r.tx.Query(ctx, `
		select k.block_id, k.text_sha256, k.changed_version, k.kept_by, k.kept_at, kept.actor
		from decision_keeps k
		left join lateral (
			select e.actor from events e
			where e.issue_key = $2 and e.type = 'decision.kept'
			  and e.payload->>'artifact_id' = k.artifact_id::text and e.payload->>'block_id' = k.block_id
			order by e.id desc
			limit 1
		) kept on true
		where k.artifact_id = $1::uuid
	`, r.decisions.id, r.issue)
	if err != nil {
		return nil, fmt.Errorf("read the Keeps of the decisions document of %s: %w", r.issue, err)
	}
	type recorded struct {
		sum, keptBy string
		version     int
		keeper      model.Actor
		keptAt      time.Time
	}
	held := map[string]recorded{}
	for rows.Next() {
		var block string
		var keep recorded
		var actor *model.Actor
		if err := rows.Scan(&block, &keep.sum, &keep.version, &keep.keptBy, &keep.keptAt, &actor); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read a Keep of the decisions document of %s: %w", r.issue, err)
		}
		keep.keeper = model.Actor{Kind: "user", ID: model.CanonicalLogin(keep.keptBy)}
		if actor != nil && actor.Kind == "user" && model.CanonicalLogin(actor.ID) == keep.keeper.ID {
			keep.keeper = *actor
		}
		held[block] = keep
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the Keeps of the decisions document of %s: %w", r.issue, err)
	}
	keeps := map[string]*sectionKeep{}
	if len(held) == 0 {
		return keeps, nil
	}
	markdown, offsets, err := renderTreeWithOffsets(tree)
	if err != nil {
		return nil, err
	}
	ranges := blockRanges(offsets)
	var waiting []keptElsewhere
	for _, section := range sections {
		keep, kept := held[section.headingID()]
		if !kept {
			continue
		}
		text, found := sectionText(markdown, ranges, section.blockIDs())
		matches := found && decisionread.TextSum(text) == keep.sum
		converted := &sectionKeep{keptBy: keep.keptBy, keeper: keep.keeper, keptAt: keep.keptAt, matches: matches}
		keeps[section.headingID()] = converted
		if found && !matches {
			waiting = append(waiting, keptElsewhere{
				keep: converted, start: ranges[section.headingID()][0], text: text, sum: keep.sum, version: keep.version,
			})
		}
	}
	if err := r.keptAcrossRenderers(ctx, markdown, waiting); err != nil {
		return nil, err
	}
	return keeps, nil
}

// keptElsewhere is a Keep of a section whose recorded sum is not the section's as it stands: the
// Keep the conversion carries (keep), where the section's heading starts in this build's
// rendering of the document, the section's text there, and the sum and version the Keep recorded
// (decision_keeps.text_sha256 and changed_version).
//
// kept for one release: decisions-step
type keptElsewhere struct {
	keep    *sectionKeep
	start   int
	text    string
	sum     string
	version int
}

// keptAcrossRenderers marks matched each of waiting whose Keep agreed with its section's text in
// another renderer version's spelling, by the rule the release before's Decisions read kept such
// a decision by in main's #4369 (markKeptAcrossRenderers). A Keep holds the sum of the text it
// agreed with and the version the decision then stood at, not the text, and that version's
// stored markdown holds the text. A renderer change (pmdoc.RendererVersion) leaves every version
// stored before it in its renderer's spelling of the document, so a section kept before the
// change sums otherwise in this build's spelling though nobody changed it. Such a Keep matches
// where another renderer version stored the version it names, or none stamped it
// (pmdoc.OtherRenderer), that version's text of the section (decisionCuts, named by its
// sectionKey) has the Keep's sum, which makes it the text the Keep agreed with, that text stood
// alone there and the section's text stands alone now (decisionCut.alone), and this build reads
// that text as the section's text now, both alone (pmdoc.FragmentReadsAs). markdown is this
// build's rendering of the document, which the section's key and whether it stands alone now are
// read from by the same cut; a section that has no cut there matches by its sum alone. A version
// the parser refuses, one that holds no such text - the heading changed since, or a block left the
// section after that version - a section that does not stand alone then or now, kept text that
// reads otherwise alone, and kept text or text now that holds `[^` leave the Keep unmatched, as a
// version this renderer version stored does. The versions are read in one query, which reads
// none this renderer version stored, and closed before any is cut.
//
// kept for one release: decisions-step
func (r *issueRestore) keptAcrossRenderers(ctx context.Context, markdown string, waiting []keptElsewhere) error {
	if len(waiting) == 0 {
		return nil
	}
	current, read := decisionCuts(markdown)
	if !read {
		return nil
	}
	keyAt := make(map[int]sectionKey, len(current))
	for key, cut := range current {
		keyAt[cut.start] = key
	}
	keys := make([]sectionKey, len(waiting))
	standing := make([]bool, len(waiting))
	numbers := map[int]bool{}
	for index, keep := range waiting {
		key, found := keyAt[keep.start]
		if !found || !current[key].alone {
			continue
		}
		keys[index], standing[index] = key, true
		numbers[keep.version] = true
	}
	if len(numbers) == 0 {
		return nil
	}
	rows, err := r.tx.Query(ctx, `
		select number, coalesce(markdown, '') from artifact_versions
		where artifact_id = $1::uuid and number = any($2::int[]) and renderer_version is distinct from $3
	`, r.decisions.id, slices.Sorted(maps.Keys(numbers)), pmdoc.RendererVersion)
	if err != nil {
		return fmt.Errorf("read the versions the Keeps of the decisions document of %s name: %w", r.issue, err)
	}
	type storedVersion struct {
		number   int
		markdown string
	}
	stored, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (storedVersion, error) {
		var version storedVersion
		err := row.Scan(&version.number, &version.markdown)
		return version, err
	})
	if err != nil {
		return fmt.Errorf("read the versions the Keeps of the decisions document of %s name: %w", r.issue, err)
	}
	for _, version := range stored {
		cuts, read := decisionCuts(version.markdown)
		if !read {
			continue
		}
		for index, keep := range waiting {
			if !standing[index] || keep.version != version.number {
				continue
			}
			kept, held := cuts[keys[index]]
			if held && kept.alone && kept.sum == keep.sum && pmdoc.FragmentReadsAs(kept.text, keep.text) {
				keep.keep.matches = true
			}
		}
	}
	return nil
}
