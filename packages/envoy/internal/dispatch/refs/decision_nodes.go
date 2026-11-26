package refs

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

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
// node, their types' own, as every typed block's node carries its type's (TypeSupertag), but for
// a reading in an ask nobody has answered, whose node carries BlockSupertag (TypedFacts.supertag).
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
	// versions is the version of each document the facts were read of (DecisionFacts): the one
	// document's, or each fragment's document's of an issue room, by id; artifactID is the
	// document indexed, the room's tree artifact; and documents is, by block id, the document
	// whose fragment of the room holds each block of another document than artifactID. Each
	// typed fact of a block carries the version of that block's own document (versionOf), as the
	// block index's do.
	versions   Versions
	artifactID string
	documents  map[string]string
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
	// settles is each decision whose from_ask names an ask of the document's issue whose row
	// names its node, with that ask's id (settledAsks): the decision's part_of is the ask's node,
	// whatever its placement (planBlockParents), as a reading's in an answered ask is.
	settles map[string]string
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

// versionOf is the version block id's typed facts carry: that of the document whose fragment of
// an issue room holds it (documents), where versions covers that document, else the indexed
// document's own, else the newest the facts were read of.
func (facts TypedFacts) versionOf(id string) Version {
	if held, ok := facts.versions[facts.documents[id]]; ok {
		return held
	}
	if held, ok := facts.versions[facts.artifactID]; ok {
		return held
	}
	_, newest := facts.versions.Newest(facts.artifactID)
	return newest
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

// DecisionFacts reads the versions versions holds of document artifactID - the one version of a
// document, or, for an issue room indexed under its tree artifact artifactID, the version of each
// fragment's document - for the typed-node pass, through q, in the read phase of the write that
// holds the document's owner lock: record is what the newest of those versions' row says of the
// pass (IndexedVersion), handed over as the block index takes it, so a version whose row is only
// queued has its writer and its issue all the same; blocks are the version's rendered blocks,
// tree the tree they were rendered from, pointers the version's `block[]` values
// (pmdoc.BlockPointers), of which it keeps the decisions' and the ask blocks' shaped, and sources
// what the document service says of the version's blocks. A version holding no decision, reading
// or ask block, and one with no tree, reads nothing and answers the zero facts, bare ones beside
// the record and the versions for the first. Each fact of a block is dated at the version of that
// block's own document (TypedFacts.versionOf).
//
// It reads the ask row of each ask block, the ask each ask block holding a reading shows
// (TypedSources.Shown), the asks row of each ask a decision's from_ask names (settledAsks), and,
// of each decision and reading node, its supertag and state and the newest tuple of each value it
// holds and of its decided_by and its authored_by.
//
// A reading in an ask block stands under the ask the block shows, a person's decision once that
// ask holds an answer: its node is a reading's, its part_of that ask's node. A reading in an ask
// block that shows no ask, or one whose ask holds no answer, which settlement flags (docs'
// flagReadingsWithoutAnswers), is a block like any other until the ask holds an answer.
//
// A decision whose from_ask names an ask of the document's issue stands under that ask's node,
// "a decision under the ask it answers" (R2 of the issue-tree spec): an issue-level ask, which no
// block holds, or the ask of an ask block of any document of the issue. Its from_ask stays a tie
// of its own, which says what the decision means, and its part_of says where it stands. Where the
// issue holds no node of that ask, the decision stands where its text puts it.
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
func DecisionFacts(ctx context.Context, q Queryer, artifactID string, record IndexedVersion, versions Versions, blocks []model.ArtifactBlock, tree *pmdoc.Node, pointers []Pointer, sources TypedSources) (TypedFacts, error) {
	if tree == nil {
		return TypedFacts{}, nil
	}
	if !HoldsTypedBlocks(blocks) {
		return TypedFacts{bare: true, unread: blocks, document: record, versions: versions, artifactID: artifactID}, nil
	}
	nodes, _, _ := uniqueBlocks(blocks)
	fits, _ := blockFits(record.RefKey, nodes)
	facts := TypedFacts{
		read: true, document: record, nodes: nodes, fits: fits,
		versions: versions, artifactID: artifactID, documents: map[string]string{},
		supertags: map[string]string{}, askBlocks: map[string]string{},
	}
	for _, node := range nodes {
		if node.Document != "" && node.Document != artifactID {
			facts.documents[node.ID] = node.Document
		}
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
	if facts.settles, err = settledAsks(ctx, q, record.Issue, facts.ties, facts.foreign); err != nil {
		return TypedFacts{}, err
	}
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
// (pmdoc.TypedContent), and with neither its `shaped` attribute nor any attribute its type marks
// `server`. A reading never changes the decision it reads, so the decision's words, and the sum a
// Keep records over them, are the same whether readings hang under it or not. `shaped` is a
// relation out of the decision to the spec nodes it shaped, and no word of it (R32 of
// the issue-tree spec), so a write of it alone stales no Keep. A server attribute -
// decided_by, kept_by, kept_text_sum - is the server's record of the decision and no word of it
// either; markdown carries none of them today, and this leaves them out whatever the schema says.
// It is the one function every sum of a decision's words goes through: the reads of a decision
// (decisionread), the Keep, which compares with those reads, and the decisions step's conversion,
// which writes a converted Keep's sum (docs' decision_restore.go).
func DecisionWords(decision *pmdoc.Node) (string, error) {
	content, _ := pmdoc.TypedContent(decision)
	attributes, _ := pmdoc.BlockTypeAttributes(decision.Type)
	own := *decision
	own.Children = content
	own.Attrs = make(pmdoc.Attrs, len(decision.Attrs))
	for name, value := range decision.Attrs {
		if name == shapedAttribute || attributes.Get(name).Server {
			continue
		}
		own.Attrs[name] = value
	}
	return pmdoc.Render(&pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{&own}})
}

// AskWords is an ask's own words, the text its sum covers and a person's answer decides: the
// blocks of its content, its question and its options, rendered alone, with no attribute of its
// block - neither the server's state and answer nor its `shaped` pointers nor its urgency - and none
// of the readings it closes with (pmdoc.TypedContent). It is the one rendering of an ask's words:
// the reads of a decision (decisionread) and R32's refusal of an agent's write that changes an
// answered ask's words (docs' refuseDroppedAnsweredAsks) both read it.
func AskWords(ask *pmdoc.Node) (string, error) {
	content, _ := pmdoc.TypedContent(ask)
	return pmdoc.Render(&pmdoc.Node{Type: "doc", Children: content})
}

// shapedAttribute is the name of a decision's `shaped` attribute, whose store name is
// TupleShaped's.
const shapedAttribute = "shaped"

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

// settledAsks is, by decision id, the ask each decision's from_ask among ties names, where that ask
// is one of issue, the document's, and its row names its node, read in one statement: the ask the
// decision settles, under whose node it stands (R2 of the issue-tree spec). A tie naming another
// issue (foreign) settles none, since no pass states it, and neither does one naming an ask no
// row of the issue holds, or whose row names no node yet: such a decision stands where its text
// puts it. The ask is matched by the id its row's node is keyed by, spelled as the store spells
// it, which is the far end the decision's from_ask tuple names. A document no issue holds settles
// none.
func settledAsks(ctx context.Context, q Queryer, issue string, ties []Pointer, foreign map[tieKey]struct{}) (map[string]string, error) {
	if issue == "" {
		return nil, nil
	}
	settling := map[string][]string{}
	var asks []string
	for _, tie := range ties {
		if tie.Kind != TupleFromAsk || tie.Target.Kind != askType {
			continue
		}
		target, _ := TargetOf(tie.Target)
		if _, other := foreign[tieKey{block: tie.Block, kind: tie.Kind, target: target}]; other {
			continue
		}
		if canonical, ok := canonicalUUID(target.ID); !ok || canonical != target.ID {
			continue
		}
		if _, seen := settling[target.ID]; !seen {
			asks = append(asks, target.ID)
		}
		settling[target.ID] = append(settling[target.ID], tie.Block)
	}
	if len(asks) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, `
		select k.id::text from asks k
		where k.id = any($1::uuid[]) and k.issue_key = $2
		  and k.node_kind is not null and k.node_address is not null
	`, asks, issue)
	if err != nil {
		return nil, fmt.Errorf("read the asks the decisions of %s settle: %w", issue, err)
	}
	defer rows.Close()
	var settles map[string]string
	for rows.Next() {
		var ask string
		if err := rows.Scan(&ask); err != nil {
			return nil, fmt.Errorf("scan an ask a decision of %s settles: %w", issue, err)
		}
		for _, decision := range settling[ask] {
			if settles == nil {
				settles = map[string]string{}
			}
			settles[decision] = ask
		}
	}
	return settles, rows.Err()
}

// DecisionFromAsk is whether the decision block node at address holds a current from_ask, and the
// name of the ask it holds one to as who may read it: the ask's dispatch:// address where the ask's
// marked row names its issue, and its store address, ask/<id>, where that row names none. An ask
// no marked row names, one the store holds gone and one the ask's predicate does not let who see
// (VisibleKind) answer the one empty name, so what reaches who says nothing of an ask who may not
// read, nor of whether one exists. Whether the decision holds one reads no visibility: such a
// decision stands under the ask it settles whatever a move would store (DecisionFacts), so the
// node route moves none, for every caller who reaches the decision (R7 of the issue-tree spec,
// api's NODE_NOT_MOVABLE). It reads through q.
func DecisionFromAsk(ctx context.Context, q Queryer, who model.Actor, address string) (string, bool, error) {
	var ask *string
	err := q.QueryRow(ctx, `
		select case
		         when k.id is not null and `+VisibleKind(graphstore.KindAsk, `held.to_id`, `$4`)+`
		          and not exists (select 1 from graph_nodes gone_ask
		            where gone_ask.kind = '`+graphstore.KindAsk+`' and gone_ask.id = held.to_id and gone_ask.state = '`+graphstore.StateGone+`')
		         then coalesce('dispatch://' || k.issue_key || '/ask/' || held.to_id, '`+graphstore.KindAsk+`/' || held.to_id)
		       end
		from (`+graphstore.CurrentTupleRead(`t.to_id`, `t.to_id, t.asserted`,
		`t.from_kind = $1 and t.from_id = $2 and t.kind = $3 and t.to_kind = '`+graphstore.KindAsk+`'`)+`) held
		left join asks k on k.node_kind = '`+graphstore.KindAsk+`' and k.node_address = held.to_id
		where held.asserted
		order by held.to_id limit 1
	`, BlockNodeKind, address, TupleFromAsk, VisibilityArg(who)).Scan(&ask)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read the from_ask of the decision %s: %w", address, err)
	}
	if ask == nil {
		return "", true, nil
	}
	return *ask, true, nil
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

// supertag is the supertag of the node of block id, of type typ (R26 of the issue-tree
// spec): a decision's and a reading's are the facts' to decide - each decision, and each reading but
// one in an ask a person has not answered or in no decision or ask, takes its type's, and every
// other one BlockSupertag - and every other block's is its type's (TypeSupertag): its type for a
// typed block of the block schema, BlockSupertag for a paragraph, a heading, a list or any other
// kind. Facts that read nothing tag every decision and reading by its type. So every writer of a
// block node - the block index, the typed-node pass, the requirements, checks and plan tasks a
// deferred version's write writes (PlanAuthored) - gives one block one supertag.
func (f TypedFacts) supertag(id, typ string) string {
	if f.read && (typ == decisionType || typ == readingType) {
		if supertag, typed := f.supertags[id]; typed {
			return supertag
		}
		return BlockSupertag
	}
	return TypeSupertag(typ)
}

// decides says the facts decide the supertag of a block of type typ: a decision's and a reading's
// where they read the version's tree, and every other type's always (supertag). Facts that read
// nothing - a version whose tree did not read back - tag a decision and a reading by its type,
// which a reading in an ask nobody answered is not, so the block index leaves the node of an
// unchanged decision or reading as it stands under such facts.
func (f TypedFacts) decides(typ string) bool {
	return f.read || f.bare || (typ != decisionType && typ != readingType)
}

// asks is the blocks of the version that stand under an ask's node, as planBlockParents places
// them: each reading in an answered ask (askParents) and each decision that settles an ask of its
// issue (settles).
func (f TypedFacts) asks() askPlacements {
	return askPlacements{readings: f.askParents, decisions: f.settles}
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
// decided_by (pairs), each pointer's far end resolved, through q, to the node it resolves to now,
// as PlanBlocks resolves the pairs it reconciles (resolvePointerPairs): a `dispatch://KEY/spec#b-…`
// tie names the block of the spec's issue room once the spec is a fragment of that room (T17 of
// this branch's plan), and the blocks the facts were read of, at their addresses under the
// document artifactID the pass indexes, are the ones a pass writes.
func (f TypedFacts) typedPairs(ctx context.Context, q Queryer, artifactID string) ([]blockPair, error) {
	paired, _ := pointerPairs(f.nodes, slices.Concat(f.shaped, f.ties))
	written := make([]string, 0, len(f.nodes))
	for position, node := range f.nodes {
		if f.fits[position] {
			written = append(written, blockNodeAddress(f.document.RefKey, node.ID))
		}
	}
	return resolvePointerPairs(ctx, q, artifactID, written, f.pairs(paired))
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
// and the block index writes its node. A node the store holds at any state but gone - proposed,
// settled or overtaken as much as live - is present, and the pass leaves it. So a version that
// moves one word of a document of many decisions writes no node of its own. Beside those it
// states each author and each decision's values (typedValues). Each fact of a block is dated at
// the version of that block's own document (TypedFacts.versionOf): the one version of a document
// that is its own room, or the version of the document whose fragment of an issue room holds the
// block. written is the nodes the block index writes, none of which the pass writes again: one
// write of the store takes each node once.
func (f TypedFacts) indexedFacts(written []graphstore.Node) ([]graphstore.Node, []graphstore.Tuple) {
	if !f.read {
		return nil, nil
	}
	refKey := f.document.RefKey
	writes := make(map[Target]struct{}, len(written))
	for _, node := range written {
		writes[Target{Kind: node.Kind, ID: node.ID}] = struct{}{}
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
		if _, indexed := writes[Target{Kind: BlockNodeKind, ID: address}]; indexed {
			continue
		}
		// The typed pass moves a node's supertag or state and no block's words, so it marks no
		// node changed (blockNode): the block index marks each block whose words the version
		// moved.
		nodes = append(nodes, blockNode(address, f.document, f.document.Writer, f.versionOf(id).At, graphstore.StateLive, f.supertags[id], false))
	}
	// An author the block index states already (blockAuthors' nodes among written) is no node
	// this pass writes again.
	authors, tuples := f.typedValues()
	for _, author := range authors {
		if _, indexed := writes[Target{Kind: author.Kind, ID: author.ID}]; !indexed {
			nodes = append(nodes, author)
		}
	}
	return nodes, tuples
}

// typedValues is what the typed-node pass states of the version's decisions and readings beyond
// their nodes: of a decision, uncertainty, uncertainty_reason and replaced as values where they
// moved, and a retraction of one the decision no longer holds, so a decision whose author recorded
// neither level nor reason holds neither; and of a decision or a reading that holds no author, the
// node of the person or session who introduced the block (TypedFacts.authored, through
// graphstore.AuthorFacts) and the aliases the server proved of it, the far end of a decision's
// decided_by and a reading's authored_by. A decision's tie itself is its block_refs row's, stated
// by the reconcile that writes the row (planBlockRefs), and a reading's authored_by is stated
// here, both once. Each is dated at the version of its block's own document (versionOf). It
// answers the author nodes and the tuples.
func (f TypedFacts) typedValues() ([]graphstore.Node, []graphstore.Tuple) {
	refKey, writer := f.document.RefKey, f.document.Writer
	var nodes []graphstore.Node
	var tuples []graphstore.Tuple
	// Each author's node and the aliases the server proved of it are stated once however many
	// blocks name it, since one write of the store takes each node once.
	authors := map[Target]struct{}{}
	author := func(address string, actor model.Actor, at time.Time) []graphstore.Tuple {
		authorNodes, stated, named := graphstore.AuthorFacts(BlockNodeKind, address, actor, at, graphstore.NoEvent)
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
		at := f.versionOf(decision.id).At
		if actor, authored := f.authored[decision.id]; authored {
			author(address, actor, at)
		}
		held := f.held[address]
		for index, kind := range decisionValues {
			stored, value := held[kind], decision.values[index]
			switch {
			case value != nil && (!stored.asserted || stored.toID != *value):
				tuples = append(tuples, valueTuple(address, kind, *value, at, writer, true))
			case value == nil && stored.asserted:
				tuples = append(tuples, valueTuple(address, kind, stored.toID, at, writer, false))
			}
		}
	}
	for _, reading := range f.readings {
		if actor, authored := f.authored[reading]; authored && blockIDFits(refKey, reading) {
			tuples = append(tuples, author(blockNodeAddress(refKey, reading), actor, f.versionOf(reading).At)...)
		}
	}
	return nodes, tuples
}

// DecisionIndexing is the typed-node pass over a version whose blocks its write leaves to the
// block-index reconciliation (PlanDecisionNodes), with the requirements, checks and plan tasks
// the same write writes beside it (PlanAuthored): planned in the write's read phase, against what
// the document's block index holds, and written by Queue in its write phase.
type DecisionIndexing struct {
	artifactID string
	version    int
	// blocks is the pass's own share of a block index: the parents of its typed blocks and the
	// scoped reconcile of their ties (planBlockParents, planBlockRefs), which Queue queues as the
	// block index queues its own. A pass that reconciles nothing holds no scope.
	blocks BlockIndexing
	// dropped is the typed blocks the version removes, whose nodes the pass states gone: their
	// rows, which document_blocks loses, their texts recorded as none, and every block_refs row they
	// hold, each link retracted (planGone). A pass that removes none holds no scope.
	dropped BlockIndexing
	// authored is the version's requirements, checks and plan tasks (PlanAuthored): their rows and
	// texts, their parents and the reconcile of their serves rows. A pass that writes none holds
	// no scope.
	authored BlockIndexing
	// roomIDs and roomDocuments are, over an issue room, the decisions, readings, requirements,
	// checks and plan tasks the pass places and the document whose fragment holds each
	// (planRoomTypedBlocks, PlanAuthored).
	roomIDs, roomDocuments []string
	held                   BlockIndex
	stated                 graphstore.Uncaused
}

// PlanDecisionNodes plans the typed-node pass over the versions facts were read of
// (DecisionFacts) of document artifactID, above the deferral bound (docs.IndexDocumentReferences)
// or in a room left owed its index (docs' indexRoomLeftOwed), where no block index runs in the
// request: each fact of a block is dated at the version of that block's own document
// (TypedFacts.versionOf), the one version of a document that is its own room, or the version of
// the document whose fragment of an issue room holds the block, and index is what the document's
// block index holds, one ReadBlockIndex read or the Held of the transaction's own earlier indexing
// of the document that is not written yet. The plan reads through q, in the read phase of the
// write that holds the document's owner lock, and Queue writes it in that write's write phase.
// beside is what the caller states under the same event (PlanBlocks' beside): its nodes go in the
// pass's own node write and its tuples among the pass's facts. fragment, where it is not "", is the
// one document of the issue room artifactID whose fragment the facts were read of
// (unheldTypedNodes).
//
// It writes the node of a decision or a reading wherever the store does not hold it as the
// version leaves it - under another supertag, gone, or not at all - with its type's supertag, the
// same node PlanBlocks writes (blockNode), its values and its author (typedValues), and does for
// its own blocks alone what the block index would: it states the part_of of each decision and
// reading where the store places it elsewhere (planBlockParents), reconciles the rows of its own
// kinds held by its own blocks (typedScope), deleting no other row of the document, and states
// each typed block's node the store places in the document that the version no longer holds as
// one (unheldTypedNodes): gone where the version holds no such block, and a node of the supertag
// block where it holds a decision or a reading under another type. A typed block it states gone
// loses its rows in the same write, as a block the block index drops does (planGone). So a
// decision, a requirement or a check a deferred version removes leaves the Decisions read, the
// coverage read and each view in that version's transaction. The block-index reconciliation
// indexes the document later with the same facts, and its pairs hold these same ties, so it moves
// none of them (TestTheReconcilerMovesNoRowTheTypedPassWrote). Over an issue room it also records
// the document whose fragment holds each decision and reading it places (planRoomTypedBlocks), as
// an own-room deferral places them in its document by its own key. Bare facts, of a version
// holding no decision, reading or ask block, state only the nodes it no longer holds as one.
func PlanDecisionNodes(ctx context.Context, q Queryer, index BlockIndex, artifactID string, facts TypedFacts, beside graphstore.Uncaused, fragment string) (DecisionIndexing, error) {
	_, newest := facts.versions.Newest(artifactID)
	plan := DecisionIndexing{artifactID: artifactID, version: newest.Number, held: index}
	var nodes []graphstore.Node
	var tuples []graphstore.Tuple
	var gone []string
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
			// The typed pass moves a node's supertag or state and no block's words, so it marks
			// no node changed (blockNode): the block index marks each block whose words the
			// version moved.
			nodes = append(nodes, blockNode(address, document, document.Writer, facts.versionOf(id).At, graphstore.StateLive, facts.supertags[id], false))
		}
		unheld, dropped, err := unheldTypedNodes(ctx, q, artifactID, document, newest.At, facts.nodes, facts.supertags, fragment)
		if err != nil {
			return DecisionIndexing{}, err
		}
		authors, values := facts.typedValues()
		nodes, tuples, gone = slices.Concat(nodes, unheld, authors), values, dropped
	case facts.bare:
		blocks, _, _ := uniqueBlocks(facts.unread)
		unheld, dropped, err := unheldTypedNodes(ctx, q, artifactID, facts.document, newest.At, blocks, nil, fragment)
		if err != nil {
			return DecisionIndexing{}, err
		}
		nodes, gone = unheld, dropped
	}
	plan.stated = graphstore.PlanUncaused(append(nodes, unwritten(beside.Nodes, nodes)...), tuples)
	if facts.read {
		if err := plan.planTies(ctx, q, index, facts); err != nil {
			return DecisionIndexing{}, err
		}
	}
	if len(gone) > 0 {
		if err := plan.planGone(ctx, q, facts.document, facts.versions, newest.At, gone); err != nil {
			return DecisionIndexing{}, err
		}
	}
	plan.stated.Tuples = append(plan.stated.Tuples, beside.Tuples...)
	return plan, nil
}

// planGone plans what the write drops of the typed blocks gone, whose nodes the pass states gone
// (unheldTypedNodes), as the block index drops a block a version no longer holds (PlanBlocks,
// BlockIndexing.Queue): each one's document_blocks row, which the delete takes; its text recorded
// as none at the version of its document the pass covers, after which the edges at it stop
// resolving until a version holds its id again; every block_refs row it holds, of any kind, each
// one's tuple retracted at that version's time (planBlockRefs over a scope of the whole blocks);
// and its pointer tuples (retractGonePointers): each prevented_by of a risk it covered as a check,
// and each constrained_by it held as a requirement. The reads are the plan's, against the index
// the pass's ties leave (DecisionIndexing.held), in its write's read phase.
func (d *DecisionIndexing) planGone(ctx context.Context, q Queryer, document IndexedVersion, versions Versions, versionAt time.Time, gone []string) error {
	index := d.held
	// unheldTypedNodes answers gone in its addresses' order, which is its ids' under one ref_key.
	d.dropped = BlockIndexing{
		artifactID: d.artifactID, refKey: document.RefKey, version: d.version, versionAt: versionAt,
		versions: versions, scope: &pairScope{whole: gone},
	}
	addresses := make([]string, 0, len(gone))
	for _, id := range gone {
		if stored, held := index.blocks[id]; held {
			if stored.document != "" && stored.document != d.artifactID {
				if d.dropped.documentOf == nil {
					d.dropped.documentOf = map[string]string{}
				}
				d.dropped.documentOf[id] = stored.document
			}
			d.dropped.deleted = append(d.dropped.deleted, id)
		}
		addresses = append(addresses, blockNodeAddress(document.RefKey, id))
	}
	d.dropped.textIDs = d.dropped.deleted
	d.dropped.texts = make([]*string, len(d.dropped.deleted))
	d.dropped.textVersions = make([]int32, len(d.dropped.deleted))
	for position, id := range d.dropped.deleted {
		d.dropped.textVersions[position] = int32(d.dropped.versionOf(id).Number)
	}
	links, held, err := d.dropped.planBlockRefs(ctx, q, index, document.Writer)
	if err != nil {
		return err
	}
	d.stated.Tuples = append(d.stated.Tuples, links...)
	retracted, err := retractGonePointers(ctx, q, addresses, versionAt, document.Writer)
	if err != nil {
		return fmt.Errorf("retract the pointers at the typed blocks version %d of %s no longer holds: %w", d.version, d.artifactID, err)
	}
	d.stated.Tuples = append(d.stated.Tuples, retracted...)
	// The index holds the dropped blocks' rows no more, nor their links or their placements.
	blocks, placed := index.blocks, index.placed
	if len(d.dropped.deleted) > 0 {
		blocks = maps.Clone(index.blocks)
		for _, id := range d.dropped.deleted {
			delete(blocks, id)
		}
	}
	if placed != nil {
		placed = maps.Clone(placed)
		for _, id := range gone {
			delete(placed, id)
		}
	}
	d.held.blocks, d.held.links, d.held.placed = blocks, held, placed
	return nil
}

// planTies plans the pass's own share of a block index (DecisionIndexing.blocks): the parents of
// the version's decisions and readings and the reconcile of the rows of their ties, against index,
// with what each states appended to the pass's facts, and over an issue room the room's record of
// the document of each of those blocks. Each row records the version of its block's own document
// (TypedFacts.versionOf), and each placement is dated at the pass's own time (IndexedVersion.At),
// read here where the record carries none, as PlanBlocks reads it.
func (d *DecisionIndexing) planTies(ctx context.Context, q Queryer, index BlockIndex, facts TypedFacts) error {
	document := facts.document
	if document.At.IsZero() {
		if err := q.QueryRow(ctx, `select clock_timestamp()`).Scan(&document.At); err != nil {
			return fmt.Errorf("read the time of the decisions and readings of %s: %w", d.artifactID, err)
		}
	}
	writer := document.Writer
	_, newest := facts.versions.Newest(d.artifactID)
	pairs, err := facts.typedPairs(ctx, q, d.artifactID)
	if err != nil {
		return err
	}
	d.blocks = BlockIndexing{
		artifactID: d.artifactID, refKey: document.RefKey, version: d.version, versionAt: newest.At,
		versions: facts.versions, documentOf: facts.documents,
		pairs: pairs, scope: facts.typedScope(),
	}
	only := make(map[string]struct{}, len(facts.typed))
	for _, id := range facts.typed {
		only[id] = struct{}{}
	}
	fragments, err := fragmentDocuments(ctx, q, d.artifactID, facts.nodes)
	if err != nil {
		return err
	}
	_, parents, placed, err := d.blocks.planBlockParents(ctx, q, index, document, facts.nodes, facts.fits, fragments, facts.asks(), only, authoredSources{})
	if err != nil {
		return err
	}
	d.stated.Tuples = append(d.stated.Tuples, parents...)
	d.planRoomTypedBlocks(document, facts, fragments)
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
	d.held = index
	d.held.blocks, d.held.links, d.held.placed, d.held.rooms = blocks, held, placed, d.heldRooms(index)
	return nil
}

// Moved is the targets whose counts the rows the pass reconciles move: those of the rows of its
// ties, of the typed blocks it drops and of its requirements', checks' and plan tasks' serves that
// it inserts or deletes, and of those whose block types it changes.
func (d DecisionIndexing) Moved() []Target {
	if len(d.dropped.moved) == 0 && len(d.authored.moved) == 0 {
		return d.blocks.moved
	}
	return slices.Concat(d.blocks.moved, d.dropped.moved, d.authored.moved)
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
// transaction holding the document's owner lock: the nodes it states (graphstore.QueueNodes), in
// one node write, then the projection of its typed blocks' parents, an issue room's record of the
// documents of the blocks it places (queueRoomTypedBlocks) and the reconcile of its ties' rows;
// then the rows of the typed blocks it drops and their links (planGone); then the rows and texts
// of the requirements, checks and plan tasks it writes, the projection of their parents and the
// reconcile of their serves rows (PlanAuthored). Each row names a node that write wrote or the
// store holds.
func (d DecisionIndexing) Queue(batch *store.WriteBatch) error {
	graphstore.QueueNodes(batch, d.stated)
	if d.blocks.scope != nil {
		d.blocks.queueBlockParents(batch)
	}
	d.queueRoomTypedBlocks(batch)
	if d.blocks.scope != nil {
		d.blocks.queueBlockRefs(batch)
	}
	if d.dropped.scope != nil {
		d.dropped.queueRows(batch)
		d.dropped.queueBlockRefs(batch)
	}
	if d.authored.scope != nil {
		d.authored.queueRows(batch)
		d.authored.queueBlockParents(batch)
		d.authored.queueBlockRefs(batch)
	}
	return nil
}

// planRoomTypedBlocks plans the record in issue_room_blocks, the issue room's one record of the
// document whose fragment holds each of its blocks (planRoomBlocks), of the document of each
// decision and reading the typed-node pass above the deferral bound places in the room
// artifactID, whose tree artifact keys their nodes and their ties' block_refs rows: a document that
// is its own room places them under its own key, so every reader of a block's document
// (BlockDocumentSQL), the Decisions read of a document that owes its block index among them, reads
// that document, and a room's would read the tree but for this row. A block of the tree's own
// fragment names the tree. The pass states no in_document - the block-index reconciliation states
// it, and the row it projects, when it indexes the room whole - so a block the room has a row of
// already keeps it (queueRoomTypedBlocks): a row moved here would project an in_document no tuple
// states. A row whose block went before the reconciliation indexed the room goes with that pass
// (planRoomBlocks). A document that is its own room records nothing.
func (d *DecisionIndexing) planRoomTypedBlocks(document IndexedVersion, facts TypedFacts, fragments map[string]fragmentDocument) {
	if document.Band != RankBandTree || len(facts.typed) == 0 {
		return
	}
	for _, id := range facts.typed {
		if !blockIDFits(document.RefKey, id) {
			continue
		}
		owner := d.artifactID
		if _, read := fragments[facts.documents[id]]; read {
			owner = facts.documents[id]
		}
		d.roomIDs, d.roomDocuments = append(d.roomIDs, id), append(d.roomDocuments, owner)
	}
}

// queueRoomTypedBlocks queues the room's record planRoomTypedBlocks planned: the document of each
// block the room holds no row of yet.
func (d DecisionIndexing) queueRoomTypedBlocks(batch *store.WriteBatch) {
	if len(d.roomIDs) == 0 {
		return
	}
	batch.Queue(fmt.Sprintf("record the documents of the typed blocks of room %s", d.artifactID), `
		insert into issue_room_blocks (tree_artifact_id, block_id, document_id)
		select $1::uuid, w.block_id, w.document_id::uuid from unnest($2::text[], $3::text[]) as w (block_id, document_id)
		on conflict (tree_artifact_id, block_id_key(block_id)) do nothing
	`, d.artifactID, d.roomIDs, d.roomDocuments)
}

// heldRooms is the room's record as index holds it once the pass is written: where index knows
// the record (BlockIndex.rooms), each block the pass records that the record holds no row of joins
// it; where index does not, it stays unknown, and a later indexing of the room reads the table.
func (d DecisionIndexing) heldRooms(index BlockIndex) map[string]string {
	if index.rooms == nil || len(d.roomIDs) == 0 {
		return index.rooms
	}
	rooms := maps.Clone(index.rooms)
	for position, id := range d.roomIDs {
		if _, held := rooms[id]; !held {
			rooms[id] = d.roomDocuments[position]
		}
	}
	return rooms
}

// unheldTypedNodes is the node of each typed block the store holds present in document
// artifactID, at any state but gone (the spec's R12: a proposed, settled or overtaken
// node is as present as a live one), that the version's blocks no longer hold as one, written at
// versionAt: stated gone, with the supertag it holds, where the version holds no block of its id,
// and restated with the supertag of the type the version holds the block under where that is
// another than the one it carries (TypeSupertag; BlockSupertag for a decision or a reading the
// facts do not decide), as PlanBlocks writes the node of a retyped block, which keeps the
// state it holds (blockNode). supertags is each block the version holds as a decision or a reading
// (TypedFacts.supertags), whose node the typed-node pass writes itself. It answers beside the
// nodes the id of each block it states gone, in the nodes' order, whose rows the pass deletes
// (DecisionIndexing.planGone). A node it states gone is one the version changed (blockNode's
// changed), as a block a version's index drops is.
//
// A typed block is each type of the block schema (pmdoc.TypedBlockNames): a decision, a reading,
// a callout, an ask block, a requirement, a check, a criterion and a plan task, whose node carries
// its type's supertag (R26 of the issue-tree spec) and whose document_blocks row
// carries the type. It reads only what the store places in the
// document, each by its own key: the typed blocks its document_blocks rows hold, the block nodes
// whose current part_of is the document's own node (graph_tuples_as_of_in) and those its block_refs
// rows name, which is where the typed-node pass above the deferral bound places a decision or a
// reading; and the readings whose current part_of is one of those decisions or an ask whose block
// the document holds. A node outside the document - the reading of a copy of one of its asks on
// another document - is no node of it, and neither is a block of a retired document of an issue
// room, nor an ask of one, which the room's passes keep as the retirement left them
// (BlockIndexing.retained). fragment, where it is not "", is the one document of the issue room
// artifactID whose fragment the version's blocks are (docs' room left owed): a block another
// fragment holds is none of the version's, and is left as it stands. The read is the plan's, in
// its write's read phase.
func unheldTypedNodes(ctx context.Context, q Queryer, artifactID string, document IndexedVersion, versionAt time.Time, blocks []model.ArtifactBlock, supertags map[string]string, fragment string) ([]graphstore.Node, []string, error) {
	refKey := document.RefKey
	var only *string
	if fragment != "" {
		only = &fragment
	}
	rows, err := q.Query(ctx, `
		with candidates as (
			select d.node_address as id, true as typed_row from document_blocks d
			where d.artifact_id = $1::uuid and d.node_kind = $3 and d.type = any($9::text[])
			  and not `+retiredDocumentSQL(graphstore.RoomBlockRowSQL("d.artifact_id", "d.block_id"))+`
			union
			select part.from_id, false from (`+graphstore.CurrentTupleRead(`t.kind, t.from_kind, t.from_id`, `t.from_kind, t.from_id, t.asserted`,
		`t.to_kind = 'artifact' and t.to_id = $2 and t.kind = '`+graphstore.TuplePartOf+`'`)+`) part
			where part.asserted and part.from_kind = $3
			union
			select r.node_address, false from block_refs r where r.artifact_id = $1::uuid and r.node_kind = $3
			  and not `+retiredDocumentSQL(graphstore.RoomBlockRowSQL("r.artifact_id", "r.block_id"))+`
		),
		typed as (
			select node.id, node.supertag from candidates c
			cross join lateral (
				select g.id, g.supertag from graph_nodes g
				where g.kind = $3 and g.id = c.id and g.state <> $5 and (c.typed_row or g.supertag = any($4::text[]))
				offset 0
			) node
		),
		holders as (
			select $3::text as kind, id from typed where supertag = $6
			union all
			select $7::text, q.id::text from asks q where q.block_artifact_id = $1::uuid
			  and not `+retiredDocumentSQL(RoomBlockDocumentSQL("q.block_artifact_id", "q.block_id"))+`
		)
		select found.id, found.supertag from (
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
		) found
		where $10::uuid is null or `+RoomBlockDocumentSQL("$1::uuid", "substr(found.id, length($2) + 2)")+` = $10::uuid
	`, artifactID, refKey, BlockNodeKind, []string{DecisionSupertag, ReadingSupertag}, graphstore.StateGone,
		DecisionSupertag, graphstore.KindAsk, ReadingSupertag, pmdoc.TypedBlockNames(), only)
	if err != nil {
		return nil, nil, fmt.Errorf("read the typed blocks the store places in %s: %w", refKey, err)
	}
	held := map[string]string{}
	for rows.Next() {
		var address, supertag string
		if err := rows.Scan(&address, &supertag); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("scan a typed block the store places in %s: %w", refKey, err)
		}
		if strings.HasPrefix(address, refKey+"#") {
			held[address] = supertag
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("read the typed blocks the store places in %s: %w", refKey, err)
	}
	if len(held) == 0 {
		return nil, nil, nil
	}
	present := make(map[string]model.ArtifactBlock, len(blocks))
	for _, block := range blocks {
		if blockIDFits(refKey, block.ID) {
			present[blockNodeAddress(refKey, block.ID)] = block
		}
	}
	// The block index marks the blocks a version dropped or whose words it moved changed
	// (blockNode's changed). This pass deletes the row of each typed block the version dropped,
	// so no later index drops it again, and marks it changed here; a node it restates under the
	// supertag of the block's type now keeps its words, and is not marked.
	var nodes []graphstore.Node
	var gone []string
	for _, address := range slices.Sorted(maps.Keys(held)) {
		block, holds := present[address]
		if !holds {
			nodes = append(nodes, blockNode(address, document, document.Writer, versionAt, graphstore.StateGone, held[address], true))
			gone = append(gone, strings.TrimPrefix(address, refKey+"#"))
			continue
		}
		if supertags[block.ID] != "" {
			continue
		}
		// A block the version holds as no decision or reading the facts decide carries the
		// supertag of its type now (TypeSupertag): a decision or a reading retyped into a check
		// takes check, and a reading that is no reading yet - in an ask nobody answered - and a
		// paragraph take block.
		want := BlockSupertag
		if block.Type != decisionType && block.Type != readingType {
			want = TypeSupertag(block.Type)
		}
		if held[address] != want {
			nodes = append(nodes, blockNode(address, document, document.Writer, versionAt, graphstore.StateLive, want, false))
		}
	}
	return nodes, gone, nil
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

// PlanOpenedAskTies plans the shaped ties of the ask blocks of the versions facts were read of
// (DecisionFacts) of document artifactID whose asks the calling transaction opened, in the read
// phase of the write that holds the document's owner lock: facts are what DecisionFacts read of
// the versions, index what the document's block index holds (one ReadBlockIndex read, or the Held
// of the transaction's own earlier indexing of it), and opened each such block with the id of the
// ask opened for it, whose node the transaction writes before these rows. Each row it writes
// records the version of its block's own document (TypedFacts.versionOf) and is dated at. An ask
// block's shaped hangs on the ask's node (TypedFacts.pairs), so a block whose ask no row held when
// its version was indexed had none of its ties written then; the settlement that opens the asks
// writes them here, beside their rows (docs' writeOpenedAskTies). The ask a block's ties hang on
// is the one opened names, since the row that names it may be one this transaction has queued and
// not written, which no read of the asks answers yet. It reconciles the shaped rows of those
// blocks alone, in one reconcile, from the same pairs the block index reconciles, so the
// document's next index moves none of them.
//
// Moved answers, by block, the targets whose counts that block's rows moved, which the
// ask.opened that records its ask names: an opened block held no shaped row before, since its
// pairs had no ask's node to hang on, so each moved target is one its own pairs reach. Stated
// answers what it states, held for those events.
func PlanOpenedAskTies(ctx context.Context, q Queryer, index BlockIndex, artifactID string, at time.Time, facts TypedFacts, opened map[string]string) (OpenedAskTies, error) {
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
	typed, err := facts.typedPairs(ctx, q, artifactID)
	if err != nil {
		return OpenedAskTies{}, err
	}
	var pairs []blockPair
	for _, pair := range typed {
		if _, mine := own[pair.blockID]; mine && pair.kind == TupleShaped {
			pairs = append(pairs, pair)
		}
	}
	// Each row records the version of its block's own document, dated at.
	versions := make(Versions, len(facts.versions))
	for document, version := range facts.versions {
		versions[document] = Version{Number: version.Number, At: at}
	}
	_, newest := versions.Newest(artifactID)
	plan.blocks = BlockIndexing{
		artifactID: artifactID, refKey: facts.document.RefKey, version: newest.Number, versionAt: at,
		versions: versions, documentOf: facts.documents,
		pairs: pairs, scope: &pairScope{kinds: []string{TupleShaped}, blocks: slices.Sorted(maps.Keys(own))},
	}
	tuples, held, err := plan.blocks.planBlockRefs(ctx, q, index, facts.document.Writer)
	if err != nil {
		return OpenedAskTies{}, err
	}
	plan.stated = graphstore.Uncaused{Tuples: tuples}
	plan.held = index
	plan.held.links = held
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
