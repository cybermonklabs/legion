package refs

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// The block types the typed-node pass reads: a decision and a reading, whose blocks are nodes of
// their own type's supertag, and an ask, whose block's shaped
// hangs on the ask's node and whose answer is the decision a reading in it reads.
const (
	decisionType = "decision"
	readingType  = "reading"
	askType      = "ask"
)

// DecisionSupertag and ReadingSupertag are the supertags of a decision's and a reading's block
// node, their types' own, where every other block's node carries BlockSupertag
// (TypedFacts.supertag).
const (
	DecisionSupertag = decisionType
	ReadingSupertag  = readingType
)

// The ties a decision's node holds and the values it carries, each under its attribute's store
// name (schemanodes.StoreName), beside graphstore.TupleDecidedBy: shaped points at each block of
// the issue the decision shaped, from_ask at the ask it settles, and uncertainty,
// uncertainty_reason and replaced are values no table projects. An ask block's shaped is the
// same kind, stated on the ask's node.
const (
	TupleShaped            = "shaped"
	TupleFromAsk           = "from_ask"
	ValueUncertainty       = "uncertainty"
	ValueUncertaintyReason = "uncertainty_reason"
	ValueReplaced          = "replaced"
)

// The two appends of a person's Keep on a decision's node, each under
// its attribute's store name: kept_by ties the decision to the person who kept it, and
// kept_text_sum holds the decision's text sum at the Keep, a value of its own because a tuple
// holds one far end and nothing beside it. decision_keeps is their projection.
const (
	TupleKeptBy      = "kept_by"
	ValueKeptTextSum = "kept_text_sum"
)

// typedKinds are the kinds of tie the typed-node pass states, whose block_refs rows it reconciles
// on its own above the deferral bound (PlanDecisionNodes).
var typedKinds = []string{graphstore.TupleDecidedBy, TupleFromAsk, TupleShaped}

// decisionValues are the values a decision's node holds, in the order decisionFact holds them.
var decisionValues = [...]string{ValueUncertainty, ValueUncertaintyReason, ValueReplaced}

// TypedFacts is what the typed-node pass reads of one version of a document (DecisionFacts): its
// decision, reading and ask blocks, read off the tree the version was rendered from, and what the
// store and the asks rows hold of them. PlanBlocks takes it for the supertag of each node it
// writes, the part_of of each reading and the ties of each decision and ask block, and for the
// typed nodes, their values and their authors it states beside them (indexedFacts), and
// PlanDecisionNodes writes those from it on its own above the deferral bound, so the two passes
// read one version once and agree on what they write. The zero value is a version that holds no
// such block or whose tree did not read back, of which neither pass states a typed fact.
type TypedFacts struct {
	// read is whether the version holds a decision, a reading or an ask block and its tree was
	// read: the zero facts write every decision and reading as its type's node and no tie.
	read bool
	// bare is a version whose tree was read and holds none of those, and unread its blocks, which
	// facts that read nothing else keep for the typed-node pass above the deferral bound: each
	// decision and reading node the store places in the document is one the version no longer
	// holds as one (PlanDecisionNodes). document is what the version's row says of the pass,
	// which bare facts carry too.
	bare     bool
	unread   []model.ArtifactBlock
	document IndexedVersion
	// nodes and fits are the version's blocks as PlanBlocks reads them (uniqueBlocks) and
	// whether each one's address fits the store's key, which the pass places and pairs above the
	// deferral bound.
	nodes []model.ArtifactBlock
	fits  []bool
	// supertags is each block of the version that is a node of its own type's supertag, by id:
	// every decision, and every reading but one standing in an ask a person has not answered,
	// which settlement flags (docs' flagReadingsWithoutAnswers) and which is a node of the
	// supertag block until the ask holds an answer. typed is the same blocks in document order.
	supertags map[string]string
	typed     []string
	decisions []decisionFact
	readings  []string
	// askParents is each reading standing in an answered ask whose row names its node, with that
	// ask's id: the reading's part_of is the ask's node (planBlockParents). The ask is the one
	// its ask block shows (TypedSources.Shown): the block's own row's, or for a copy of another
	// document's ask, the source's.
	askParents map[string]string
	// askBlocks is every ask block of the version, by id, with the id of the ask whose row names
	// its node, "" for a block no such row holds yet: the shaped of a block with none waits for
	// the row settlement opens.
	askBlocks map[string]string
	// shaped and ties are the decisions' and the ask blocks' shaped values and the decisions'
	// ties their writer states (a from_ask), as the pointers the block index pairs
	// (pointerPairs); foreign is each of them naming another issue than the document's, of which
	// no pass states a tie.
	shaped  []Pointer
	ties    []Pointer
	foreign map[tieKey]struct{}
	// serverDecided is whether the schema marks a decision's decided_by `server`, the server's to
	// derive. decidedBy is then each decision's author: the far end of the newest decided_by tuple
	// its node holds, or for a node holding none, the person or session who introduced the block
	// (TypedSources.Authors). authored is each decision and reading whose author this pass states,
	// with that author: a decision's decided_by and a reading's authored_by.
	serverDecided bool
	decidedBy     map[string]Target
	authored      map[string]model.Actor
	// held is what the store holds now of each typed node, by address and then tuple kind: the
	// newest tuple of each of its values, its decided_by and its authored_by. nodes is the
	// supertag and state of each typed node the store holds, by address, which tells the pass
	// which node it has to write (indexedFacts, PlanDecisionNodes).
	held      map[string]map[string]heldTie
	heldNodes map[string]heldNode
}

// TypedSources is what the typed-node pass reads of a version from the document service, which
// holds what no row of the store does: who introduced each block, and which ask an ask block shows.
type TypedSources struct {
	// Authors is, of the blocks ids names, each one's author: the actor of the write that
	// introduced the block - a joined write's own for the blocks it added, the one browser an
	// update came from for one a browser introduced - as the document service records it for a
	// settlement to read (docs' askBlockSources.author). A block no write is recorded as
	// introducing, or one several browsers' update did, is absent, and its node takes no author:
	// a decision's author is never guessed from who else wrote the version.
	Authors func(ids []string) (map[string]model.Actor, error)
	// Shown is, of the ask blocks ids names, each holding a reading, the ask each one shows: its
	// own row's, or for a copy of an ask of another document of its owner, that source's (docs'
	// readingAnswers). A block that shows none - a new block no row holds yet, an unreadable one -
	// is absent, and a reading in it is a block like any other.
	Shown func(ids []string) (map[string]ShownAsk, error)
}

// ShownAsk is the ask an ask block shows: its id, whether its row names its node, and whether
// that node holds an answer.
type ShownAsk struct {
	ID              string
	Named, Answered bool
}

// heldNode is what the store holds of one node: its supertag and its state.
type heldNode struct {
	supertag, state string
}

// decisionFact is one decision block of the version: its id, and the values its attributes hold
// in decisionValues' order, nil where the attribute is absent - replaced is held only while it is
// true.
type decisionFact struct {
	id     string
	values [len(decisionValues)]*string
}

// tieKey is one tie a typed block holds: the block's id, the tie's kind and its target.
type tieKey struct {
	block, kind string
	target      Target
}

// heldTie is the newest tuple of one kind a node holds: its far end, and whether it asserts.
type heldTie struct {
	toKind, toID string
	asserted     bool
}

// DecisionFacts reads version record of document artifactID for the typed-node pass, through q,
// in the read phase of the write that holds the document's owner lock: record is what the
// version's row says of the pass (IndexedVersion), handed over as the block index takes it, so a
// version whose row is only queued has its writer and its issue all the same; blocks are the
// version's rendered blocks, tree the tree they were rendered from, pointers the version's
// `block[]` values (pmdoc.BlockPointers), of which it keeps the decisions' and the ask blocks'
// shaped, and sources what the document service says of the version's blocks. A version holding
// no decision, reading or ask block, and one with no tree, reads nothing and answers the zero
// facts, bare ones beside the record for the first.
//
// It reads the ask row of each ask block, the ask each ask block holding a reading shows
// (TypedSources.Shown), and, of each decision and reading node, its supertag and state and the
// newest tuple of each value it holds and of its decided_by and its authored_by.
//
// A reading in an ask block stands under the ask the block shows, a person's decision once that
// ask holds an answer: its node is a reading's, its part_of that ask's node. A reading in an ask
// block that shows no ask, or one whose ask holds no answer, which settlement flags (docs'
// flagReadingsWithoutAnswers), is a block like any other until the ask holds an answer.
//
// Whose a decision's tie is, the schema says (pmdoc.BlockTypeAttributes): a relation attribute
// its writer writes - from_ask, of kind `ask` - is read off the tree, and decided_by, an `actor`
// the schema marks `server`, is the server's and never a value from the tree, which a browser
// can put anything in. Its author is read off its node, because markdown never carries it (the
// attribute is never rendered, `markdown: false`): the stored tuple is the one record of who
// decided. A node that holds none takes the author of the write that introduced its block
// (TypedSources.Authors), and only that one, so a later write by another writer, the browser's
// included, moves nothing of it, and a version several people wrote names nobody it cannot tell
// introduced the block. A reading's authored_by follows the same rule.
func DecisionFacts(ctx context.Context, q Queryer, artifactID string, record IndexedVersion, blocks []model.ArtifactBlock, tree *pmdoc.Node, pointers []Pointer, sources TypedSources) (TypedFacts, error) {
	if tree == nil {
		return TypedFacts{}, nil
	}
	if !HoldsTypedBlocks(blocks) {
		return TypedFacts{bare: true, unread: blocks, document: record}, nil
	}
	nodes, _, _ := uniqueBlocks(blocks)
	fits, _ := blockFits(record.RefKey, nodes)
	facts := TypedFacts{
		read: true, document: record, nodes: nodes, fits: fits,
		supertags: map[string]string{}, askBlocks: map[string]string{},
	}
	types := make(map[string]string, len(nodes))
	for _, node := range nodes {
		types[node.ID] = node.Type
	}
	decisionAttributes, _ := pmdoc.BlockTypeAttributes(decisionType)
	facts.serverDecided = decisionAttributes.Get(graphstore.TupleDecidedBy).Server
	// The readings of each holder, in document order, before the asks say which of them stand in
	// an answered ask.
	type heldReading struct{ id, holder, holderType string }
	var readings []heldReading
	var readingAsks []string
	seen := map[string]struct{}{}
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type != decisionType && node.Type != askType {
			return true
		}
		id := TypedBlockID(node)
		if _, repeated := seen[id]; repeated || id == "" || types[id] != node.Type {
			return true
		}
		seen[id] = struct{}{}
		if node.Type == decisionType {
			facts.decisions = append(facts.decisions, decisionOf(id, node))
			facts.ties = append(facts.ties, writtenTies(id, node, decisionAttributes)...)
		} else {
			facts.askBlocks[id] = ""
		}
		_, admitted := pmdoc.TypedContent(node)
		holds := false
		for _, child := range admitted {
			reading := TypedBlockID(child)
			if _, repeated := seen[reading]; child.Type != readingType || repeated || reading == "" || types[reading] != readingType {
				continue
			}
			seen[reading] = struct{}{}
			readings = append(readings, heldReading{id: reading, holder: id, holderType: node.Type})
			holds = true
		}
		if holds && node.Type == askType {
			readingAsks = append(readingAsks, id)
		}
		return true
	})
	for _, pointer := range pointers {
		if pointer.Kind != TupleShaped || (pointer.BlockType != decisionType && pointer.BlockType != askType) {
			continue
		}
		facts.shaped = append(facts.shaped, pointer)
	}
	named, err := namedAskRows(ctx, q, artifactID, facts.askBlocks)
	if err != nil {
		return TypedFacts{}, err
	}
	maps.Copy(facts.askBlocks, named)
	var shown map[string]ShownAsk
	if len(readingAsks) > 0 && sources.Shown != nil {
		if shown, err = sources.Shown(readingAsks); err != nil {
			return TypedFacts{}, err
		}
	}
	for _, decision := range facts.decisions {
		facts.supertags[decision.id] = decisionType
	}
	for _, reading := range readings {
		if reading.holderType == askType {
			ask, shows := shown[reading.holder]
			if !shows || !ask.Answered {
				continue
			}
			if ask.Named {
				if facts.askParents == nil {
					facts.askParents = map[string]string{}
				}
				facts.askParents[reading.id] = ask.ID
			}
		}
		facts.supertags[reading.id] = readingType
		facts.readings = append(facts.readings, reading.id)
	}
	for _, node := range nodes {
		if _, typed := facts.supertags[node.ID]; typed {
			facts.typed = append(facts.typed, node.ID)
		}
	}
	facts.foreign = foreignTies(record.Issue, facts.shaped, facts.ties)
	if err := facts.readHeld(ctx, q, sources); err != nil {
		return TypedFacts{}, err
	}
	return facts, nil
}

// HoldsTypedBlocks reports whether blocks hold a block the typed-node pass reads: a decision, a
// reading or an ask.
func HoldsTypedBlocks(blocks []model.ArtifactBlock) bool {
	for _, block := range blocks {
		switch block.Type {
		case decisionType, readingType, askType:
			return true
		}
	}
	return false
}

// DecisionWords is a decision's own words, the text its text sum covers: decision, a decision
// block node, rendered alone with the readings it closes with left out
// (pmdoc.TypedContent). A reading never changes the decision it reads, so the decision's words,
// and the sum a Keep records over them, are the same whether readings hang under it or not. The
// attributes a markdown rendering never carries - decided_by, kept_by, kept_text_sum - are none
// of its words.
func DecisionWords(decision *pmdoc.Node) (string, error) {
	content, _ := pmdoc.TypedContent(decision)
	own := *decision
	own.Children = content
	return pmdoc.Render(&pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{&own}})
}

// TypedBlockID is a typed block's id as the block index keys it (storedBlockID).
func TypedBlockID(node *pmdoc.Node) string {
	id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
	return storedBlockID(id)
}

// decisionOf is decision block node as decisionFact holds it.
func decisionOf(id string, node *pmdoc.Node) decisionFact {
	fact := decisionFact{id: id}
	for index, name := range decisionValues {
		switch value := node.Attrs[name].(type) {
		case string:
			if value != "" {
				fact.values[index] = &value
			}
		case bool:
			if value {
				held := "true"
				fact.values[index] = &held
			}
		}
	}
	return fact
}

// writtenTies is the ties a decision block node's writer states, read off its attributes: the
// value of each relation attribute its type declares and does not mark `server`, as a pointer of
// the attribute's kind - an `ask` attribute's ask (a from_ask), and an `actor` attribute's
// person. A `server` attribute's value is the server's to derive and is never read off the tree
// (decidedBy). A tree inside the schema holds only an ask of an issue written whole (pmdoc's
// validateAskReference).
func writtenTies(id string, node *pmdoc.Node, attributes pmdoc.BlockAttributes) []Pointer {
	var ties []Pointer
	for _, attribute := range attributes {
		written, _ := node.Attrs[attribute.Name].(string)
		if attribute.Server || written == "" {
			continue
		}
		switch attribute.Kind {
		case pmdoc.BlockAttributeAsk:
			if ref, ok := text.ParseRef(written, ""); ok && ref.Kind == askType {
				ties = append(ties, Pointer{Block: id, BlockType: decisionType, Kind: attribute.Name, Target: ref})
			}
		case pmdoc.BlockAttributeActor:
			ties = append(ties, Pointer{Block: id, BlockType: decisionType, Kind: attribute.Name,
				Target: text.Ref{Kind: graphstore.KindPerson, ID: model.CanonicalLogin(written)}})
		}
	}
	return ties
}

// foreignTies is each of ties naming another issue than issue, the document's, by the issue its
// reference spells (pmdoc.ReferencedIssue), none for a document no issue holds: a write that
// takes markdown refuses such a tie (pmdoc.CheckIssueTies), and a browser's, which settlement
// never refuses, is stored with no tie stated, so the store holds no relation the editor and the
// server both forbid.
func foreignTies(issue string, ties ...[]Pointer) map[tieKey]struct{} {
	if issue == "" {
		return nil
	}
	var foreign map[tieKey]struct{}
	for _, held := range ties {
		for _, tie := range held {
			named := tie.Target.IssueKey
			if named == "" {
				named = tie.Target.Project
			}
			if named == "" || named == issue {
				continue
			}
			target, _ := TargetOf(tie.Target)
			if foreign == nil {
				foreign = map[tieKey]struct{}{}
			}
			foreign[tieKey{block: tie.Block, kind: tie.Kind, target: target}] = struct{}{}
		}
	}
	return foreign
}

// namedAskRows is, of blocks, ask blocks of document artifactID, each whose row names the ask's
// node, with that ask's id, read in one statement: the node an ask block's shaped hangs on
// (TypedFacts.pairs). A block no row holds, or whose row names no node yet, is absent.
func namedAskRows(ctx context.Context, q Queryer, artifactID string, blocks map[string]string) (map[string]string, error) {
	if len(blocks) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(blocks))
	for id := range blocks {
		ids = append(ids, id)
	}
	rows, err := q.Query(ctx, `
		select k.block_id, k.id::text from asks k
		where k.block_artifact_id = $1 and k.block_id = any($2::text[])
		  and k.node_kind is not null and k.node_address is not null
	`, artifactID, ids)
	if err != nil {
		return nil, fmt.Errorf("read the asks of the ask blocks of %s: %w", artifactID, err)
	}
	defer rows.Close()
	found := make(map[string]string, len(ids))
	for rows.Next() {
		var block, ask string
		if err := rows.Scan(&block, &ask); err != nil {
			return nil, fmt.Errorf("scan the ask of an ask block of %s: %w", artifactID, err)
		}
		found[block] = ask
	}
	return found, rows.Err()
}

// readHeld reads what the store holds of each typed node that fits its key, in one statement:
// the node's supertag and state (heldNodes), and the newest tuple of each value it holds and of
// its decided_by and its authored_by (held). It then names the author of each decision and
// reading that holds none from sources (authored), and each decision's decided_by (decidedBy).
func (f *TypedFacts) readHeld(ctx context.Context, q Queryer, sources TypedSources) error {
	refKey := f.document.RefKey
	addresses := make([]string, 0, len(f.typed))
	for _, id := range f.typed {
		if blockIDFits(refKey, id) {
			addresses = append(addresses, blockNodeAddress(refKey, id))
		}
	}
	f.held = make(map[string]map[string]heldTie, len(addresses))
	f.heldNodes = make(map[string]heldNode, len(addresses))
	if len(addresses) > 0 {
		kinds := append([]string{graphstore.TupleDecidedBy, graphstore.TupleAuthoredBy}, decisionValues[:]...)
		rows, err := q.Query(ctx, `
			select typed.address, n.supertag, n.state, t.kind, t.to_kind, t.to_id, t.asserted
			from unnest($2::text[]) as typed (address)
			left join graph_nodes n on n.kind = $1 and n.id = typed.address
			left join lateral (
				select distinct on (h.kind) h.kind, h.to_kind, h.to_id, h.asserted
				from graph_tuples h
				where h.from_kind = $1 and h.from_id = typed.address and h.kind = any($3::text[])
				order by h.kind, h.seq desc
			) t on true
		`, BlockNodeKind, addresses, kinds)
		if err != nil {
			return fmt.Errorf("read what the store holds of the decisions and readings of %s: %w", refKey, err)
		}
		defer rows.Close()
		for rows.Next() {
			var address string
			var supertag, state, kind, toKind, toID *string
			var asserted *bool
			if err := rows.Scan(&address, &supertag, &state, &kind, &toKind, &toID, &asserted); err != nil {
				return fmt.Errorf("scan what the store holds of a decision or a reading of %s: %w", refKey, err)
			}
			if supertag != nil && state != nil {
				f.heldNodes[address] = heldNode{supertag: *supertag, state: *state}
			}
			if kind == nil {
				continue
			}
			if f.held[address] == nil {
				f.held[address] = map[string]heldTie{}
			}
			f.held[address][*kind] = heldTie{toKind: *toKind, toID: *toID, asserted: *asserted}
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read what the store holds of the decisions and readings of %s: %w", refKey, err)
		}
	}
	// A node holding its author keeps it: the author a block takes is the one of the write that
	// introduced it, and only a node that holds none yet is given one.
	var unauthored []string
	for _, decision := range f.decisions {
		if !f.serverDecided || !blockIDFits(refKey, decision.id) {
			continue
		}
		if held, stated := f.held[blockNodeAddress(refKey, decision.id)][graphstore.TupleDecidedBy]; stated {
			f.setDecidedBy(decision.id, Target{Kind: held.toKind, ID: held.toID})
		} else {
			unauthored = append(unauthored, decision.id)
		}
	}
	for _, reading := range f.readings {
		if !blockIDFits(refKey, reading) {
			continue
		}
		if _, stated := f.held[blockNodeAddress(refKey, reading)][graphstore.TupleAuthoredBy]; !stated {
			unauthored = append(unauthored, reading)
		}
	}
	if len(unauthored) == 0 || sources.Authors == nil {
		return nil
	}
	authors, err := sources.Authors(unauthored)
	if err != nil {
		return fmt.Errorf("read who introduced the decisions and readings of %s: %w", refKey, err)
	}
	for _, id := range unauthored {
		actor, named := authors[id]
		kind, authorID, proved := graphstore.AuthorOf(actor)
		if !named || !proved || !graphstore.AddressFits(authorID) {
			continue
		}
		if f.authored == nil {
			f.authored = map[string]model.Actor{}
		}
		f.authored[id] = actor
		if f.supertags[id] == decisionType {
			f.setDecidedBy(id, Target{Kind: kind, ID: authorID})
		}
	}
	return nil
}

func (f *TypedFacts) setDecidedBy(id string, author Target) {
	if f.decidedBy == nil {
		f.decidedBy = map[string]Target{}
	}
	f.decidedBy[id] = author
}

// supertag is the supertag of the node of block id, of type typ: its own type's for a decision
// and for a reading the facts hold to be one, and BlockSupertag for every other block. Facts that
// read nothing tag every decision and reading by its type.
func (f TypedFacts) supertag(id, typ string) string {
	if f.read {
		if supertag, typed := f.supertags[id]; typed {
			return supertag
		}
		return BlockSupertag
	}
	switch typ {
	case decisionType, readingType:
		return typ
	}
	return BlockSupertag
}

// withTies is pointers with each decision's written ties beside them (a from_ask), which the
// block index pairs as it pairs a pointer: a from_ask value is written in its decision's opening
// line, so the mention the extraction finds there is the tie's and no mention of its own
// (pointerPairs).
func (f TypedFacts) withTies(pointers []Pointer) []Pointer {
	if len(f.ties) == 0 {
		return pointers
	}
	return slices.Concat(pointers, f.ties)
}

// pairs is a version's pairs (blockPairs) as planBlockRefs reconciles them: a tie naming
// another issue left out (foreign), an ask block's shaped marked with its ask's node or, where no
// row names that node yet, left out, and each decision's decided_by added, its far end the author
// decidedBy names. The typed-node pass hands the same function the pairs of its own ties, so the
// rows it writes above the deferral bound are the rows the block index writes for one version.
func (f TypedFacts) pairs(pairs []blockPair) []blockPair {
	if !f.read {
		return pairs
	}
	kept := make([]blockPair, 0, len(pairs)+len(f.decidedBy))
	for _, pair := range pairs {
		if _, foreign := f.foreign[tieKey{block: pair.blockID, kind: pair.kind, target: pair.target}]; foreign {
			continue
		}
		if pair.kind == TupleShaped {
			if ask, onAsk := f.askBlocks[pair.blockID]; onAsk {
				if ask == "" {
					continue
				}
				pair.nodeKind, pair.nodeAddress = graphstore.KindAsk, ask
			}
		}
		kept = append(kept, pair)
	}
	for _, decision := range f.decisions {
		if author, decided := f.decidedBy[decision.id]; decided {
			kept = append(kept, blockPair{blockID: decision.id, kind: graphstore.TupleDecidedBy, target: author})
		}
	}
	return kept
}

// typedPairs is the pairs of the facts' own ties: each decision's and ask block's shaped and each
// decision's from_ask, paired as the block index pairs a pointer (pointerPairs), and its
// decided_by (pairs).
func (f TypedFacts) typedPairs() []blockPair {
	paired, _ := pointerPairs(f.nodes, slices.Concat(f.shaped, f.ties))
	return f.pairs(paired)
}

// typedScope is the rows of block_refs the facts' own ties are: those of typedKinds held by the
// version's decisions and ask blocks.
func (f TypedFacts) typedScope() *pairScope {
	blocks := make([]string, 0, len(f.decisions)+len(f.askBlocks))
	for _, decision := range f.decisions {
		blocks = append(blocks, decision.id)
	}
	for id := range f.askBlocks {
		blocks = append(blocks, id)
	}
	slices.Sort(blocks)
	return &pairScope{kinds: typedKinds, blocks: blocks}
}

// indexedFacts is what the typed-node pass states of the version beside the block index that
// indexes it in the same write (PlanBlocks), which writes the node of every block whose text the
// version moved, states each typed node's part_of, reconciles every block_refs row of the
// document with the facts' ties among them, and states gone each block the version no longer
// holds. So the pass adds only the node of a decision or a reading the store holds otherwise than
// the version leaves it - under another supertag (a block retyped into a decision, a reading
// whose ask now holds an answer) or gone - with its type's supertag, the same node the block index
// writes (blockNode); a block the store holds no node of is one whose text this version wrote,
// and the block index writes its node. So a version that moves one word of a document of many
// decisions writes no node of its own. Beside those it states each author and each decision's
// values (typedValues). written is the nodes the block index writes, none of which the pass
// writes again: one write of the store takes each node once.
func (f TypedFacts) indexedFacts(versionAt time.Time, written []graphstore.Node) ([]graphstore.Node, []graphstore.Tuple) {
	if !f.read {
		return nil, nil
	}
	refKey := f.document.RefKey
	writes := make(map[string]struct{}, len(written))
	for _, node := range written {
		writes[node.ID] = struct{}{}
	}
	var nodes []graphstore.Node
	for _, id := range f.typed {
		if !blockIDFits(refKey, id) {
			continue
		}
		address := blockNodeAddress(refKey, id)
		held, holds := f.heldNodes[address]
		if !holds || held.supertag == f.supertags[id] && held.state != graphstore.StateGone {
			continue
		}
		if _, indexed := writes[address]; indexed {
			continue
		}
		// The block index marks the blocks whose words the version moved changed (blockNode's
		// changed); this pass moves a node's type or brings it back, never its words.
		nodes = append(nodes, blockNode(address, f.document, versionAt, graphstore.StateLive, f.supertags[id], false))
	}
	authors, tuples := f.typedValues(versionAt)
	return append(nodes, authors...), tuples
}

// typedValues is what the typed-node pass states of the version's decisions and readings beyond
// their nodes: of a decision, uncertainty, uncertainty_reason and replaced as values where they
// moved, and a retraction of one the decision no longer holds, so a decision whose author recorded
// neither level nor reason holds neither; and of a decision or a reading that holds no author, the
// node of the person or session who introduced the block (TypedFacts.authored, through
// graphstore.AuthorFacts) and the aliases the server proved of it, the far end of a decision's
// decided_by and a reading's authored_by. A decision's tie itself is its block_refs row's, stated
// by the reconcile that writes the row (planBlockRefs), and a reading's authored_by is stated
// here, both once. It answers the author nodes and the tuples.
func (f TypedFacts) typedValues(versionAt time.Time) ([]graphstore.Node, []graphstore.Tuple) {
	refKey, writer := f.document.RefKey, f.document.Writer
	var nodes []graphstore.Node
	var tuples []graphstore.Tuple
	// Each author's node and the aliases the server proved of it are stated once however many
	// blocks name it, since one write of the store takes each node once.
	authors := map[Target]struct{}{}
	author := func(address string, actor model.Actor) []graphstore.Tuple {
		authorNodes, stated, named := graphstore.AuthorFacts(BlockNodeKind, address, actor, versionAt, graphstore.NoEvent)
		if !named {
			return nil
		}
		key := Target{Kind: authorNodes[0].Kind, ID: authorNodes[0].ID}
		if _, written := authors[key]; !written {
			authors[key] = struct{}{}
			nodes = append(nodes, authorNodes...)
			tuples = append(tuples, stated[1:]...)
		}
		return stated[:1]
	}
	for _, decision := range f.decisions {
		if !blockIDFits(refKey, decision.id) {
			continue
		}
		address := blockNodeAddress(refKey, decision.id)
		if actor, authored := f.authored[decision.id]; authored {
			author(address, actor)
		}
		held := f.held[address]
		for index, kind := range decisionValues {
			stored, value := held[kind], decision.values[index]
			switch {
			case value != nil && (!stored.asserted || stored.toID != *value):
				tuples = append(tuples, valueTuple(address, kind, *value, versionAt, writer, true))
			case value == nil && stored.asserted:
				tuples = append(tuples, valueTuple(address, kind, stored.toID, versionAt, writer, false))
			}
		}
	}
	for _, reading := range f.readings {
		if actor, authored := f.authored[reading]; authored && blockIDFits(refKey, reading) {
			tuples = append(tuples, author(blockNodeAddress(refKey, reading), actor)...)
		}
	}
	return nodes, tuples
}

// DecisionIndexing is the typed-node pass over a version whose blocks its write leaves to the
// block-index reconciliation (PlanDecisionNodes): planned in the write's read phase, against what
// the document's block index holds, and written by Queue in its write phase.
type DecisionIndexing struct {
	artifactID string
	version    int
	// blocks is the pass's own share of a block index: the parents of its typed blocks and the
	// scoped reconcile of their ties (planBlockParents, planBlockRefs), which Queue queues as the
	// block index queues its own. A pass that reconciles nothing holds no scope.
	blocks BlockIndexing
	held   BlockIndex
	stated graphstore.Uncaused
}

// PlanDecisionNodes plans the typed-node pass over version `version` of document artifactID,
// written at versionAt, above the deferral bound (docs.IndexDocumentReferences), where no block
// index runs in the request: facts are what DecisionFacts read of the version, and index what the
// document's block index holds, one ReadBlockIndex read or the Held of the transaction's own
// earlier indexing of the document that is not written yet. The plan reads through q, in the read
// phase of the write that holds the document's owner lock, and Queue writes it in that write's
// write phase. beside is what the caller states under the same event (PlanBlocks' beside): its
// nodes go in the pass's own node write and its tuples among the pass's facts.
//
// It writes the node of a decision or a reading wherever the store does not hold it as the
// version leaves it - under another supertag, gone, or not at all - with its type's supertag, the
// same node PlanBlocks writes (blockNode), its values and its author (typedValues), and does for
// its own blocks alone what the block index would: it states the part_of of each decision and
// reading where the store places it elsewhere (planBlockParents), reconciles the rows of its own
// kinds held by its own blocks (typedScope), deleting no other row of the document, and states
// each decision and reading node the store places in the document that the version no longer
// holds as one (unheldTypedNodes): gone where the version holds no such block, and a node of the
// supertag block where it holds the block under another type. So a decision a deferred version
// removes leaves the Decisions read in that version's transaction. The block-index reconciliation
// indexes the document later with the same facts, and its pairs hold these same ties, so it moves
// none of them (TestTheReconcilerMovesNoRowTheTypedPassWrote). Bare facts, of a version holding no
// typed block, state only the nodes it no longer holds as one.
func PlanDecisionNodes(ctx context.Context, q Queryer, index BlockIndex, artifactID string, version int, versionAt time.Time, facts TypedFacts, beside graphstore.Uncaused) (DecisionIndexing, error) {
	plan := DecisionIndexing{artifactID: artifactID, version: version, held: index}
	var nodes []graphstore.Node
	var tuples []graphstore.Tuple
	switch {
	case facts.read:
		document := facts.document
		refKey := document.RefKey
		for _, id := range facts.typed {
			if !blockIDFits(refKey, id) {
				continue
			}
			address := blockNodeAddress(refKey, id)
			if held, holds := facts.heldNodes[address]; holds && held.supertag == facts.supertags[id] && held.state != graphstore.StateGone {
				continue
			}
			// The block index marks the blocks whose words the version moved changed (blockNode's
			// changed); this pass moves a node's type or brings it back, never its words.
			nodes = append(nodes, blockNode(address, document, versionAt, graphstore.StateLive, facts.supertags[id], false))
		}
		unheld, err := unheldTypedNodes(ctx, q, artifactID, document, versionAt, facts.nodes, facts.supertags)
		if err != nil {
			return DecisionIndexing{}, err
		}
		authors, values := facts.typedValues(versionAt)
		nodes, tuples = slices.Concat(nodes, unheld, authors), values
	case facts.bare:
		blocks, _, _ := uniqueBlocks(facts.unread)
		unheld, err := unheldTypedNodes(ctx, q, artifactID, facts.document, versionAt, blocks, nil)
		if err != nil {
			return DecisionIndexing{}, err
		}
		nodes = unheld
	}
	plan.stated = graphstore.PlanUncaused(append(nodes, unwritten(beside.Nodes, nodes)...), tuples)
	if facts.read {
		if err := plan.planTies(ctx, q, index, facts, versionAt); err != nil {
			return DecisionIndexing{}, err
		}
	}
	plan.stated.Tuples = append(plan.stated.Tuples, beside.Tuples...)
	return plan, nil
}

// planTies plans the pass's own share of a block index (DecisionIndexing.blocks): the parents of
// the version's decisions and readings and the reconcile of the rows of their ties, against index,
// with what each states appended to the pass's facts.
func (d *DecisionIndexing) planTies(ctx context.Context, q Queryer, index BlockIndex, facts TypedFacts, versionAt time.Time) error {
	writer := facts.document.Writer
	d.blocks = BlockIndexing{
		artifactID: d.artifactID, refKey: facts.document.RefKey, version: d.version, versionAt: versionAt,
		pairs: facts.typedPairs(), scope: facts.typedScope(),
	}
	only := make(map[string]struct{}, len(facts.typed))
	for _, id := range facts.typed {
		only[id] = struct{}{}
	}
	parents, placed, err := d.blocks.planBlockParents(ctx, q, index, facts.nodes, facts.fits, facts.askParents, only, writer)
	if err != nil {
		return err
	}
	d.stated.Tuples = append(d.stated.Tuples, parents...)
	links, held, err := d.blocks.planBlockRefs(ctx, q, index, writer)
	if err != nil {
		return err
	}
	d.stated.Tuples = append(d.stated.Tuples, links...)
	// The parents' projection names the node of each row it places, which the index then holds.
	blocks := index.blocks
	if len(d.blocks.placedIDs) > 0 {
		blocks = maps.Clone(index.blocks)
		for _, id := range d.blocks.placedIDs {
			if row, stored := blocks[id]; stored {
				row.node = blockNodeAddress(d.blocks.refKey, id)
				blocks[id] = row
			}
		}
	}
	d.held = BlockIndex{blocks: blocks, links: held, stand: index.stand, placed: placed}
	return nil
}

// Moved is the targets whose counts the ties the pass reconciles move: those of the rows it
// inserts or deletes and of those whose block types it changes.
func (d DecisionIndexing) Moved() []Target {
	return d.blocks.moved
}

// Stated is what the pass states in the node and tuple store, held for the event that records the
// version: the caller hands it to graphstore.QueueCaused right after that event, in the same write
// phase.
func (d DecisionIndexing) Stated() graphstore.Uncaused {
	return d.stated
}

// Held is what the document's block index holds once the pass is written.
func (d DecisionIndexing) Held() BlockIndex {
	return d.held
}

// Queue queues the pass's writes on a write phase's batch, which its caller runs in the
// transaction holding the document's owner lock: the nodes it states (graphstore.QueueNodes),
// then the projection of its blocks' parents and the reconcile of its ties' rows, each of which
// names a node that write wrote or the store holds.
func (d DecisionIndexing) Queue(batch *store.WriteBatch) error {
	graphstore.QueueNodes(batch, d.stated)
	if d.blocks.scope == nil {
		return nil
	}
	d.blocks.queueBlockParents(batch)
	d.blocks.queueBlockRefs(batch)
	return nil
}

// unheldTypedNodes is the node of each decision and reading the store holds present in document
// artifactID, at any state but gone (the spec's R12: a proposed, settled or overtaken
// node is as present as a live one), that the version's blocks no longer hold as one, written at
// versionAt: stated gone, with the supertag it holds, where the version holds no block of its id,
// and restated with the supertag block where it holds the block under another type, as
// PlanBlocks writes the node of a retyped block, which keeps the state it holds (blockNode).
// supertags is each block the version holds as a decision or a reading (TypedFacts.supertags),
// whose node the typed-node pass writes itself.
//
// It reads only what the store places in the document, each by its own key: the decisions and
// readings its document_blocks rows hold, the block nodes whose current part_of is the document's
// own node (graph_tuples_as_of_in) and those its block_refs rows name, which is where the typed-node
// pass above the deferral bound places them; and the readings whose current part_of is one of
// those decisions or an ask whose block the document holds. A node outside the document - the
// reading of a copy of one of its asks on another document - is no node of it. The read is the
// plan's, in its write's read phase.
func unheldTypedNodes(ctx context.Context, q Queryer, artifactID string, document IndexedVersion, versionAt time.Time, blocks []model.ArtifactBlock, supertags map[string]string) ([]graphstore.Node, error) {
	refKey := document.RefKey
	rows, err := q.Query(ctx, `
		with candidates as (
			select d.node_address as id from document_blocks d
			where d.artifact_id = $1::uuid and d.node_kind = $3 and d.type = any($4::text[])
			union
			select part.from_id from (`+graphstore.CurrentTupleRead(`t.kind, t.from_kind, t.from_id`, `t.from_kind, t.from_id, t.asserted`,
		`t.to_kind = 'artifact' and t.to_id = $2 and t.kind = '`+graphstore.TuplePartOf+`'`)+`) part
			where part.asserted and part.from_kind = $3
			union
			select r.node_address from block_refs r where r.artifact_id = $1::uuid and r.node_kind = $3
		),
		typed as (
			select node.id, node.supertag from candidates c
			cross join lateral (
				select g.id, g.supertag from graph_nodes g
				where g.kind = $3 and g.id = c.id and g.state <> $5 and g.supertag = any($4::text[])
				offset 0
			) node
		),
		holders as (
			select $3::text as kind, id from typed where supertag = $6
			union all
			select $7::text, q.id::text from asks q where q.block_artifact_id = $1::uuid
		)
		select id, supertag from typed
		union
		select node.id, node.supertag from holders h
		cross join lateral (`+graphstore.CurrentTupleRead(`t.kind, t.from_kind, t.from_id`, `t.from_kind, t.from_id, t.asserted`,
		`t.to_kind = h.kind and t.to_id = h.id and t.kind = '`+graphstore.TuplePartOf+`' and t.to_kind <> '`+graphstore.KindValue+`'`)+`) part
		cross join lateral (
			select g.id, g.supertag from graph_nodes g
			where g.kind = $3 and g.id = part.from_id and g.state <> $5 and g.supertag = $8
			offset 0
		) node
		where part.asserted and part.from_kind = $3
	`, artifactID, refKey, BlockNodeKind, []string{DecisionSupertag, ReadingSupertag}, graphstore.StateGone,
		DecisionSupertag, graphstore.KindAsk, ReadingSupertag)
	if err != nil {
		return nil, fmt.Errorf("read the decisions and readings the store places in %s: %w", refKey, err)
	}
	held := map[string]string{}
	for rows.Next() {
		var address, supertag string
		if err := rows.Scan(&address, &supertag); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan a decision or a reading the store places in %s: %w", refKey, err)
		}
		if strings.HasPrefix(address, refKey+"#") {
			held[address] = supertag
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the decisions and readings the store places in %s: %w", refKey, err)
	}
	if len(held) == 0 {
		return nil, nil
	}
	present := make(map[string]string, len(blocks))
	for _, block := range blocks {
		if blockIDFits(refKey, block.ID) {
			present[blockNodeAddress(refKey, block.ID)] = block.ID
		}
	}
	// The block index marks the blocks a version dropped or whose words it moved changed
	// (blockNode's changed); this pass states only what the store placed here and the version no
	// longer holds as a decision or a reading, so it marks none.
	var nodes []graphstore.Node
	for _, address := range slices.Sorted(maps.Keys(held)) {
		id, holds := present[address]
		switch {
		case !holds:
			nodes = append(nodes, blockNode(address, document, versionAt, graphstore.StateGone, held[address], false))
		case supertags[id] == "":
			nodes = append(nodes, blockNode(address, document, versionAt, graphstore.StateLive, BlockSupertag, false))
		}
	}
	return nodes, nil
}

// OpenedAskTies is the shaped ties of the ask blocks whose asks a settlement opened, when it
// writes no version of its own to index (PlanOpenedAskTies): planned in the settlement's read
// phase and written by Queue in its write phase.
type OpenedAskTies struct {
	// blocks is the reconcile of those blocks' shaped rows (planBlockRefs), which Queue queues as
	// the block index queues its own. A plan that reconciles nothing holds no scope.
	blocks BlockIndexing
	held   BlockIndex
	moved  map[string][]Target
	stated graphstore.Uncaused
}

// PlanOpenedAskTies plans the shaped ties of the ask blocks of version `version` of document
// artifactID whose asks the calling transaction opened, in the read phase of the write that holds
// the document's owner lock: facts are what DecisionFacts read of the version, index what the
// document's block index holds (one ReadBlockIndex read, or the Held of the transaction's own
// earlier indexing of it), and opened each such block with the id of the ask opened for it, whose
// node the transaction writes before these rows. An ask block's shaped hangs on the ask's node
// (TypedFacts.pairs), so a block whose ask no row held when its version was indexed had none of
// its ties written then; the settlement that opens the asks writes them here, beside their rows
// (docs' writeOpenedAskTies). The ask a block's ties hang on is the one opened names, since the
// row that names it may be one this transaction has queued and not written, which no read of the
// asks answers yet. It reconciles the shaped rows of those blocks alone, in one reconcile, from
// the same pairs the block index reconciles, so the document's next index moves none of them.
//
// Moved answers, by block, the targets whose counts that block's rows moved, which the
// ask.opened that records its ask names: an opened block held no shaped row before, since its
// pairs had no ask's node to hang on, so each moved target is one its own pairs reach. Stated
// answers what it states, held for those events.
func PlanOpenedAskTies(ctx context.Context, q Queryer, index BlockIndex, artifactID string, version int, at time.Time, facts TypedFacts, opened map[string]string) (OpenedAskTies, error) {
	plan := OpenedAskTies{held: index}
	if !facts.read || len(opened) == 0 {
		return plan, nil
	}
	facts.askBlocks = maps.Clone(facts.askBlocks)
	own := make(map[string]struct{}, len(opened))
	for block, ask := range opened {
		if _, onAsk := facts.askBlocks[block]; onAsk {
			facts.askBlocks[block] = ask
			own[block] = struct{}{}
		}
	}
	if len(own) == 0 {
		return plan, nil
	}
	var pairs []blockPair
	for _, pair := range facts.typedPairs() {
		if _, mine := own[pair.blockID]; mine && pair.kind == TupleShaped {
			pairs = append(pairs, pair)
		}
	}
	plan.blocks = BlockIndexing{
		artifactID: artifactID, refKey: facts.document.RefKey, version: version, versionAt: at,
		pairs: pairs, scope: &pairScope{kinds: []string{TupleShaped}, blocks: slices.Sorted(maps.Keys(own))},
	}
	tuples, held, err := plan.blocks.planBlockRefs(ctx, q, index, facts.document.Writer)
	if err != nil {
		return OpenedAskTies{}, err
	}
	plan.stated = graphstore.Uncaused{Tuples: tuples}
	plan.held = BlockIndex{blocks: index.blocks, links: held, stand: index.stand, placed: index.placed}
	reached := make(map[Target]struct{}, len(plan.blocks.moved))
	for _, target := range plan.blocks.moved {
		reached[target] = struct{}{}
	}
	plan.moved = map[string][]Target{}
	for _, pair := range pairs {
		if _, movedIt := reached[pair.target]; movedIt && !slices.Contains(plan.moved[pair.blockID], pair.target) {
			plan.moved[pair.blockID] = append(plan.moved[pair.blockID], pair.target)
		}
	}
	return plan, nil
}

// Moved is, by opened block, the targets whose counts that block's rows moved.
func (o OpenedAskTies) Moved() map[string][]Target {
	return o.moved
}

// Stated is what the ties state in the node and tuple store, tuples alone, each on its ask's node
// and held for the ask.opened that records that ask.
func (o OpenedAskTies) Stated() graphstore.Uncaused {
	return o.stated
}

// Held is what the document's block index holds once the ties are written.
func (o OpenedAskTies) Held() BlockIndex {
	return o.held
}

// Queue queues the reconcile of the opened blocks' shaped rows on a write phase's batch, after
// the statements that write the asks' nodes, which those rows name.
func (o OpenedAskTies) Queue(batch *store.WriteBatch) error {
	if o.blocks.scope == nil {
		return nil
	}
	o.blocks.queueBlockRefs(batch)
	return nil
}

// valueTuple is one value of a decision's node: asserted where the decision holds it, and a
// retraction of the value the node held where the decision holds none.
func valueTuple(address, kind, value string, at time.Time, writer model.Actor, asserted bool) graphstore.Tuple {
	return graphstore.Tuple{
		FromKind: BlockNodeKind, FromID: address, Kind: kind,
		ToKind: graphstore.KindValue, ToID: value,
		At: at, Who: writer, Asserted: asserted,
	}
}
