package graphstore

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The kinds of node that record how a piece of work was reasoned about, as the graph addresses
// each: the one spelling of them that every writer of one of these nodes, and every read that
// names its kind, takes. An interaction is one run of the command bar, read by the person whose
// run it was. A lens is one analysis invoked on a node and logged under it, of the kind its
// lens_kind node names. A risk sits under its lens, and a constraint is what a requirement is
// held to. Each is addressed `dispatch://<kind>/<uuid>`, and a lens kind
// `dispatch://lens_kind/<slug>` (text.ParseRef), with no issue key in the address: where a node
// stands is its TuplePartOf tuple, so a move changes no address.
//
// A finding of a lens is no kind of its own. It is a node of kind and supertag block whose id
// names its lens, `lens/<uuid>#<block id>` (FindingID), as a comment's body is blocks of the
// comment, and KindLensBlock is its address form: the from_kind graph_edges writes for it and the
// key the read registry knows it by (refs' nodeRegistry).
//
// A read answers a node of these kinds through the loader, the predicate and the backlink arms
// refs' registry holds for its kind, as it answers nothing of a kind the registry does not know.
const (
	KindInteraction = "interaction"
	KindLens        = "lens"
	KindLensKind    = "lens_kind"
	KindConstraint  = "constraint"
	KindRisk        = "risk"
	KindLensBlock   = "lens_block"
)

// The tuple kinds those nodes are tied by, beside the TuplePartOf that places each under the node
// it stands under and the TupleAuthoredBy from each to whoever wrote it, where its credential
// proved an author.
//
// TupleChangedByRun points from each node a write made for a run created or changed to that run's
// interaction. TupleHasKind points from a lens to its lens_kind, and TupleTriggeredBy from a lens
// to the lens whose finding invoked it, which was invoked before it (InvokedSeqSQL).
// TupleConstrainedBy points from a requirement block to a constraint it is held to, and
// TuplePreventedBy from a risk to each check block that covers it, a check of any issue of the
// risk's own project in a document that is not retired. graph_edges answers each of them from
// the tuples alone, with no projection row behind it (migration 20261117020856), and
// refs.TupleKinds lists them.
const (
	TupleChangedByRun  = "changed_by_run"
	TupleHasKind       = "has_kind"
	TupleTriggeredBy   = "triggered_by"
	TupleConstrainedBy = "constrained_by"
	TuplePreventedBy   = "prevented_by"
)

// LensKindPreMortem is the id of the lens_kind node of a pre-mortem (`dispatch://lens_kind/pre-mortem`):
// the one kind of lens a risk stands under, and a kind a lens keeps once it holds it, whether or
// not a risk stands under it (ErrLensKindKept).
const LensKindPreMortem = "pre-mortem"

// The attributes the reads of those nodes answer as each node's text, as the tuple that states
// each value spells it. A lens's verdict and a risk's heading are attributes of their types
// (migration 20261116123354 declares both), carried on the node's row as the catalogue's words
// are, and so are the early warning a risk is seen coming by and the prevention that keeps it
// from happening, which the same migration declares on risk. A finding's text is a value tuple
// of the finding's block node and no attribute of any type: the block supertag's attributes
// mirror pmdoc/schema/blocks.json field by field, so a new one would be a schema bump.
const (
	AttributeVerdict      = "verdict"
	AttributeHeading      = "heading"
	AttributeEarlyWarning = "early_warning"
	AttributePrevention   = "prevention"
	AttributeText         = "text"
)

// AttributeArtifactID is the row a lens on a document, or on a block of one, records: the
// artifacts.id of the row the artifacts table holds at the document's address as the write that
// places the lens there runs - not the row the document's node was written for - a value of the
// lens (migration 20261116123354 declares it on lens, beside verdict), since a row's id is the
// address of no node. A document's address, `<issue key>/<slug>`, is a name a later document
// takes again, and a row's id is never reused, so a lens whose recorded row no longer stands at
// its document's address stands on a document that is no more (refs.StaleSQL). The store's write
// of a lens's part_of states it (queueLensArtifactIDs), whichever writer places the lens, the
// invoke and a move alike, and retracts it where a write moves the lens off a document.
const AttributeArtifactID = "artifact_id"

// blockKind is the kind the store addresses a block by, which refs.BlockNodeKind spells for the
// packages that may import refs, and this one may not. Every block carries the supertag block
// alone, whatever its type, so a rule that names a block type - a requirement, a check - reads
// that type off the block's document_blocks row, the projection of a document's block.
// BlockTypeCheck is the type of a check, the one block a risk's prevented_by may point at
// (pointerRules), which a route that names a check reads off that row too.
const (
	blockKind            = "block"
	blockTypeRequirement = "requirement"
	BlockTypeCheck       = "check"
)

// AddressKind is the kind the graph addresses a node of the store's kind and id by: the key the
// read registry knows the node by, and the kind a node reserved for its author is admitted or
// refused under (AdmitAuthorOnly). A finding of a lens is a node of kind block whose id names its
// lens (ParseFindingID), and is addressed KindLensBlock, as the registry reads an item's block by
// its item's block kind; a block of a document, and every other node, is its own kind.
func AddressKind(kind, id string) string {
	if kind != blockKind {
		return kind
	}
	if _, _, finding := ParseFindingID(id); finding {
		return KindLensBlock
	}
	return kind
}

// AddressKindSQL is AddressKind over a node whose kind and id are SQL expressions of a read's own
// query.
func AddressKindSQL(kindExpr, idExpr string) string {
	return `(case when ` + kindExpr + ` = '` + blockKind + `' and ` + FindingIDSQL(idExpr) + ` then '` +
		KindLensBlock + `' else ` + kindExpr + ` end)`
}

// FindingID is the store id of the finding blockID of the lens whose uuid is lens: the id of a
// node of kind block that names its lens, `lens/<uuid>#<block id>`. It is the one rule of that
// id: ParseFindingID reads it back, and FindingIDSQL tells it in SQL, so the author-only
// admission, the erase, the census and every read of a finding tell a finding from any other
// block by the same test. Migration 20261117020856 spells the same test in its own text.
func FindingID(lens, blockID string) string { return KindLens + "/" + lens + "#" + blockID }

// ParseFindingID is the uuid of the lens and the block id that the id of a block node names
// where it is a finding's (FindingID), and false for the id of any other block. A block of a
// document is `<ref_key>#<block id>`, and a ref_key begins with an issue's or a project's key,
// which is upper case, or with `agent/` (ArtifactRefKey), so no block of a document reads as a
// finding; nor does a block of an ask, a comment or a message, whose id begins with its own kind.
func ParseFindingID(id string) (lens, blockID string, ok bool) {
	named, ok := strings.CutPrefix(id, KindLens+"/")
	if !ok {
		return "", "", false
	}
	lens, blockID, _ = strings.Cut(named, "#")
	return lens, blockID, true
}

// FindingIDSQL is true of the id of a block node, the SQL expression idExpr, that is a finding's,
// as ParseFindingID reads one: a node's own id column, or the from_id of a tuple out of it.
func FindingIDSQL(idExpr string) string {
	return `(` + idExpr + ` like '` + KindLens + `/%')`
}

// pointerEnd is one type of node an end of a pointer tuple may be: a kind of node, or a block of
// one block type.
type pointerEnd struct{ kind, blockType string }

// word is the end as a refusal names it: "lens_kind", "requirement block".
func (e pointerEnd) word() string {
	if e.blockType != "" {
		return e.blockType + " block"
	}
	return e.kind
}

// pointerRule is the type rule of one tuple kind: the types of node it may point from and at.
type pointerRule struct {
	kind     string
	from, to []pointerEnd
	// placement holds a tuple of the kind to the rule only where it starts at one of from, and
	// lets one out of any other node through: part_of places every node there is, and only a
	// risk's place is ruled.
	placement bool
	// earlier holds the far end, a lens, to one invoked before the near end, in the order the
	// lens read answers (InvokedSeqSQL): never the near end itself, nor a lens invoked at the same
	// moment or later, so no tuple of the kind closes a cycle.
	earlier bool
	// single holds a node to one asserted tuple of the kind at a time: a second assertion is
	// refused, and a retraction is taken only beside the assertion that replaces it.
	single bool
	// underKind is the lens_kind the far end, a lens, holds through its has_kind tuple.
	underKind string
	// ownProject holds the far end to the project of the near end: a risk is prevented by a check
	// of any issue of its own project, and of no other. The near end's project is the one its
	// node holds and a block's the one its document holds; a node no project holds - none a route
	// writes, since every reasoning node is written in its issue's project - is held to the types
	// alone.
	ownProject bool
	// unretired holds the far end, a block, to a document that is not retired: nobody runs a
	// retired document's checks.
	unretired bool
}

// pointerRules are the type rules of the four pointer tuples and of a risk's place, the one table
// of them (spec requirement r-constraint-risk). Every write of this store holds each
// relation it appends whose kind is here to its rule, in the write's own transaction, before it
// appends anything (pointerCheck, from QueueWrite and QueueCaused), so no route keeps a type rule
// of its own and a move through cybermonklabs/legion#4180's `/parent` meets the same one; the
// pointer route reads the kinds it writes from here too (PointerKinds). A route keeps only what no
// type rule says: api's pointerEnd answers a constraint of another issue as no far end of this
// one, a 404, and a write that asserts a prevented_by takes the owner lock (store.LockOwner) of its
// check's document before it reads a node (api's lockReasoningOwners). A kind that is not here is
// let through to any node.
//
// The table rules the types, the place of a triggered_by's far end in the order lenses were
// invoked, and the project and the document of a risk's prevented_by: a check of any issue of the
// risk's own project, as the pre-mortem conversion writes one
// (spec requirement r-premortem-nodes), in a document that is not retired, so the
// pointer route, the risk route and the conversion are held to one rule. That the far end exists,
// is not gone and may be seen by the caller is its route's to check, through the read registry's
// loader for the far end's kind (an issue or a document is read off its own row), so that a far
// end the caller may not see answers as one that does not exist and no refusal here tells that it
// is there: a check of another project or of a retired document, and a lens invoked later, reach
// this rule only once its route has read them for the caller.
var pointerRules = [...]pointerRule{
	{kind: TupleHasKind, from: []pointerEnd{{kind: KindLens}}, to: []pointerEnd{{kind: KindLensKind}}, single: true},
	{kind: TupleTriggeredBy, from: []pointerEnd{{kind: KindLens}}, to: []pointerEnd{{kind: KindLens}}, earlier: true},
	{kind: TupleConstrainedBy,
		from: []pointerEnd{{kind: blockKind, blockType: blockTypeRequirement}, {kind: KindLensKind}},
		to:   []pointerEnd{{kind: KindConstraint}}},
	{kind: TuplePreventedBy, from: []pointerEnd{{kind: KindRisk}}, to: []pointerEnd{{kind: blockKind, blockType: BlockTypeCheck}},
		ownProject: true, unretired: true},
	{kind: TuplePartOf, from: []pointerEnd{{kind: KindRisk}}, to: []pointerEnd{{kind: KindLens}},
		placement: true, underKind: LensKindPreMortem},
}

// PointerKinds is the kind of each pointer tuple pointerRules holds, in the table's order:
// has_kind, triggered_by, constrained_by and prevented_by, the tuples the pointer route appends,
// retracts and retargets out of a node. A part_of is a node's place, which the table rules for a
// risk and no pointer route moves, so it is none of them. Each call answers a slice of its own.
func PointerKinds() []string {
	kinds := make([]string, 0, len(pointerRules))
	for _, rule := range pointerRules {
		if !rule.placement {
			kinds = append(kinds, rule.kind)
		}
	}
	return kinds
}

// InvokedSeqSQL is where the lens whose id the SQL expression idExpr names stands in the order
// lenses were invoked: the seq of its first part_of tuple (graph_tuples.seq, one order across
// every node), asserted or since retracted, so neither a later write of its row nor a move to
// another parent, which appends a later part_of, moves it; null for a lens the store places
// nowhere. The lens read answers a node's lenses in this order (api's lensesOnNodeQuery), and the
// type rule of triggered_by holds a lens to one invoked before it in the same order
// (pointerRules), so the two read one order. It reads graph_tuples_current, the index of a read
// out of a node.
func InvokedSeqSQL(idExpr string) string {
	return `(select min(invoked.seq) from graph_tuples invoked
		where invoked.from_kind = '` + KindLens + `' and invoked.from_id = ` + idExpr + `
			and invoked.kind = '` + TuplePartOf + `' and invoked.to_kind <> '` + KindValue + `')`
}

// pointerRuleOf is the rule a relation of kind out of a node of fromKind is held to, or nil for
// one the table holds to none.
func pointerRuleOf(kind, fromKind string) *pointerRule {
	for index := range pointerRules {
		rule := &pointerRules[index]
		if rule.kind != kind {
			continue
		}
		if !rule.placement || slices.ContainsFunc(rule.from, func(end pointerEnd) bool { return end.kind == fromKind }) {
			return rule
		}
	}
	return nil
}

// allowed is what the rule lets through, in words: "has_kind points from a lens at a lens_kind".
func (r *pointerRule) allowed() string {
	if r.underKind != "" {
		return fmt.Sprintf("%s belongs under %s of kind %s", namedEnds(r.from), namedEnds(r.to), r.underKind)
	}
	far := namedEnds(r.to)
	if r.earlier {
		far = WithArticle(r.to[0].word()) + " invoked before it"
	}
	return fmt.Sprintf("%s points from %s at %s", r.kind, namedEnds(r.from), far)
}

// typesBlocks is whether one of ends names a block type, which a block at that end of a tuple is
// read for.
func typesBlocks(ends []pointerEnd) bool {
	return slices.ContainsFunc(ends, func(end pointerEnd) bool { return end.blockType != "" })
}

func namedEnds(ends []pointerEnd) string {
	words := make([]string, len(ends))
	for index, end := range ends {
		words[index] = WithArticle(end.word())
	}
	return strings.Join(words, " or ")
}

// WithArticle is a kind or a type with the article it is read with: "a lens", "an issue".
func WithArticle(word string) string {
	if word != "" && strings.ContainsRune("aeiou", rune(word[0])) {
		return "an " + word
	}
	return "a " + word
}

// ErrInvalidPointer is a write that appends a relation at a node its type rule does not allow
// (pointerRules): a far end or a near end of another type, a check of another project than its
// risk or of a retired document, a lens triggered by itself or by a lens not invoked before it, a
// risk under a lens of any kind but pre-mortem, a lens left with no kind. It is a sentinel so a
// route can answer it as the caller's refusal, 400 INVALID_POINTER; what a write is refused with is
// an InvalidPointerError, which wraps it and names the tuple.
var ErrInvalidPointer = errors.New("a pointer tuple is at a node its type rule does not allow")

// InvalidPointerError is ErrInvalidPointer for one tuple: its kind, its two ends as the refusal
// names them, and what its rule lets through, so the words say what the caller pointed at and
// what it may point at instead. Retracted marks the retraction of a lens's one has_kind tuple with
// none asserted in its place, which leaves the lens no kind. keyed are the ends From and To name
// by the id the store keys each by, which a route names by address instead (NamedAt): the far end
// of a tuple refused for standing in another project than its near end (pointerRule's ownProject)
// or in a retired document (unretired), and both lenses of a triggered_by whose far end was not
// invoked before its near end (earlier); it holds none on every other refusal.
type InvalidPointerError struct {
	Kind, From, To, Rule string
	Retracted            bool
	keyed                []keyedEnd
}

// keyedEnd is one end of a refused tuple that its refusal names by the id the store keys the end's
// node by - a block by its document's ref_key and its block id, a lens by its uuid: its kind and
// id, whether To names it (the far end) or From (the near end), and the words its name stands
// between there, "the check block " and " of project FAR".
type keyedEnd struct {
	kind, id      string
	far           bool
	before, after string
}

// named is the end as its refusal names it, by name.
func (end keyedEnd) named(name string) string { return end.before + name + end.after }

func (e *InvalidPointerError) Error() string {
	if e.Retracted {
		return fmt.Sprintf("the %s tuple from %s is retracted with none asserted in its place, which breaks its type rule: %s",
			e.Kind, e.From, e.Rule)
	}
	return fmt.Sprintf("the %s tuple from %s at %s breaks its type rule: %s", e.Kind, e.From, e.To, e.Rule)
}

func (e *InvalidPointerError) Unwrap() error { return ErrInvalidPointer }

// NamedAt is the refusal naming the node the store keys by kind and id by address, where the
// refusal names that node by the id the store keys it by (keyedEnd): the store names a block by
// its document's ref_key and its block id and a lens by its uuid, and a route that read the node
// at its dispatch:// address names it as its caller does. A lens that triggers itself is both ends
// of its refusal, and both are named. named is false, and the refusal nil, for a node the refusal
// names by no id, for every other refusal and for an empty address.
func (e *InvalidPointerError) NamedAt(kind, id, address string) (refusal *InvalidPointerError, named bool) {
	if address == "" {
		return nil, false
	}
	at := *e
	for _, end := range e.keyed {
		if end.kind != kind || end.id != id {
			continue
		}
		if end.far {
			at.To = end.named(address)
		} else {
			at.From = end.named(address)
		}
		named = true
	}
	if !named {
		return nil, false
	}
	return &at, true
}

// ErrPointerAlreadyAsserted is a write that asserts a pointer its node holds one of already and
// keeps: a second has_kind tuple of a lens, or a retarget of a pre-mortem's away from pre-mortem.
// A route answers it 409 POINTER_ALREADY_ASSERTED, with the words of the PointerAlreadyAssertedError
// that wraps it.
var ErrPointerAlreadyAsserted = errors.New("the node holds that pointer already, and its type rule keeps it")

// PointerAlreadyAssertedError is ErrPointerAlreadyAsserted for one reason. Its words name no node
// and nothing that hangs under one, so a refusal reads the same whatever a reader may not see.
type PointerAlreadyAssertedError struct{ Reason string }

func (e *PointerAlreadyAssertedError) Error() string { return e.Reason }

func (e *PointerAlreadyAssertedError) Unwrap() error { return ErrPointerAlreadyAsserted }

// The two refusals ErrPointerAlreadyAsserted wraps. ErrLensKindKept is one body whatever hangs
// under the lens, so it tells nothing of a risk a reader may not see.
var (
	ErrLensKindAsserted = &PointerAlreadyAssertedError{Reason: "this lens holds a has_kind tuple already, and a lens " +
		"holds exactly one kind: retarget that tuple rather than assert a second"}
	ErrLensKindKept = &PointerAlreadyAssertedError{Reason: "a lens of kind pre-mortem keeps that kind: its has_kind " +
		"tuple is never retargeted away from pre-mortem"}
)

// ErrPointerNotHeld is a retarget of a pointer its node does not hold: there is no far end to move
// it from (Retarget).
var ErrPointerNotHeld = errors.New("the node holds no such pointer to retarget")

// errPointerInArms refuses a relation pointerRules rules handed to WriteArms, a part_of at a node
// of a kind that takes a node reserved for its author (pointerCheck), and a part_of out of a lens,
// whose write records the row of the document it places the lens on (queueLensArtifactIDs): an arm
// spliced into a caller's statement runs with no read of the nodes its rule names, so such a
// tuple is appended only through QueueWrite or QueueCaused, which check and record it in their
// write phase.
var errPointerInArms = errors.New("a pointer tuple, a part_of at a kind that takes a node reserved for its author, " +
	"and a lens's part_of are checked against their rule in the write's own transaction, by the store's Write or Caused, " +
	"and never in the arms of a caller's statement")

// refusePointersInArms is errPointerInArms for the first ruled relation or claim a WriteArms call
// holds.
func refusePointersInArms(tuples []Tuple, claims []Claim) error {
	for index := range tuples {
		if tuple := &tuples[index]; tuple.ToKind != KindValue && (pointerRuleOf(tuple.Kind, tuple.FromKind) != nil ||
			placesUnderAuthorOnly(tuple.Kind, tuple.ToKind, tuple.ToID) || placesLens(tuple.Kind, tuple.FromKind)) {
			return fmt.Errorf("%w: %s out of %s", errPointerInArms, tuple.Kind, tuple.FromKind)
		}
	}
	for index := range claims {
		if claim := &claims[index]; claim.ToKind != KindValue && (pointerRuleOf(claim.Kind, claim.FromKind) != nil ||
			placesUnderAuthorOnly(claim.Kind, claim.ToKind, claim.ToID) || placesLens(claim.Kind, claim.FromKind)) {
			return fmt.Errorf("%w: %s out of %s", errPointerInArms, claim.Kind, claim.FromKind)
		}
	}
	return nil
}

// ErrVisibilityWiderThanParent is a write that places a node every reader of its project may see
// under or on a node reserved for its author: a part_of tuple out of a node that is not reserved
// for its author at one that is. Such a node would tell each reader who may not see its parent
// that the parent exists - a lens on a node they cannot open, a risk under a lens they cannot list
// - so the store refuses it in the write of that tuple, whichever writer appends it, a move to a
// new parent among them (spec requirement r-node-visibility). A pointer of any other kind - a
// triggered_by, a constrained_by, a prevented_by - at a node a reader may not see is no such
// place: each read leaves it out at both ends. It is a sentinel so a route can answer it as the
// caller's refusal, 400 VISIBILITY_WIDER_THAN_PARENT; what a write is refused with is a
// VisibilityWiderThanParentError, which wraps it and names both nodes.
var ErrVisibilityWiderThanParent = errors.New("a node every reader may see never stands under or on a node reserved for its author")

// VisibilityWiderThanParentError is ErrVisibilityWiderThanParent for one part_of tuple: the node
// it goes out of, which is not reserved for its author, and the node it points at, which is, each
// by the kind the graph addresses it by (AddressKind) and the id the store keys it by.
type VisibilityWiderThanParentError struct {
	ChildKind, ChildID, ParentKind, ParentID string
}

func (e *VisibilityWiderThanParentError) Error() string {
	return e.Named("the "+e.ChildKind+" "+e.ChildID, "the "+e.ParentKind+" "+e.ParentID)
}

func (e *VisibilityWiderThanParentError) Unwrap() error { return ErrVisibilityWiderThanParent }

// Named is the refusal's words with its two nodes named child and parent: by kind and store id,
// as Error names them, or by the dispatch:// address a route names each by. The writer read the
// parent for its caller before it placed anything there, so the words name nothing that caller
// may not see.
func (e *VisibilityWiderThanParentError) Named(child, parent string) string {
	return child + " is visible to every reader of its project and cannot stand under or on " + parent +
		", which is reserved for its author: write it with visibility author, or with none, which takes its parent's"
}

// placesUnderAuthorOnly is whether a relation of kind at the node toKind and toID name places a
// node under one whose kind takes a node reserved for its author (AdmitAuthorOnly): a part_of,
// which places a lens on the node it was invoked on and a finding or a risk under its lens, at a
// lens, a finding, a risk, a constraint or an interaction. A pointerCheck holds each such tuple
// to the visibility of both its ends; a part_of at a node of any other kind - a block under
// its document, a comment's block under its comment - is at a node no write reserves for its
// author, and is read for nothing.
func placesUnderAuthorOnly(kind, toKind, toID string) bool {
	if kind != TuplePartOf {
		return false
	}
	_, admitted := authorOnlyKinds[AddressKind(toKind, toID)]
	return admitted
}

// placesLens is whether a relation of kind out of a node of fromKind places a lens: its part_of,
// whose write records the row of the document it places the lens on (queueLensArtifactIDs).
func placesLens(kind, fromKind string) bool {
	return kind == TuplePartOf && fromKind == KindLens
}

// ErrNoDocumentRow is a write that places a lens on a document, or on a block of one, at whose
// address the artifacts table holds no row as the write runs: the document is gone, a removal took
// its row while the write queued behind it, so the write would record no row and the lens would
// stand on nothing. It is a sentinel so a route can answer it as a write on a gone node, 409
// NODE_GONE; what a write is refused with is a NoDocumentRowError, which wraps it and names the
// lens and the node.
var ErrNoDocumentRow = errors.New("no document row stands where the write places a lens")

// NoDocumentRowError is ErrNoDocumentRow for one lens: its id and the node, as the part_of names it
// by kind and id, the write places it on.
type NoDocumentRowError struct{ Lens, ToKind, ToID string }

func (e *NoDocumentRowError) Error() string {
	return fmt.Sprintf("the lens %s is placed on the %s %s, at whose address no document row stands", e.Lens, AddressKind(e.ToKind, e.ToID), e.ToID)
}

func (e *NoDocumentRowError) Unwrap() error { return ErrNoDocumentRow }

// queueLensArtifactIDs queues on b, between the node write and the append, beside the reads of the
// pointer check (pointerCheck.queue), the one statement that records on each lens a part_of of
// tuples places the artifacts.id of the row of the document it places the lens on
// (AttributeArtifactID): every placement of a lens records it, the write that invokes a lens and a
// move under a new parent alike, whichever writer appends the part_of, so no lens stands on a
// document with the row of another (lensArtifactIDsStatement). A lens two part_of tuples of one
// write place is placed by the last. A placement on a document no row stands at is refused, the
// statement's result (NoDocumentRowError). A write that places no lens queues nothing. Its
// arguments build at Send, as the append's do, so a placement that cannot be encoded fails Send.
func queueLensArtifactIDs(b *store.WriteBatch, tuples []Tuple) {
	last := map[string]int{}
	for index := range tuples {
		if tuple := &tuples[index]; tuple.Asserted && tuple.ToKind != KindValue && placesLens(tuple.Kind, tuple.FromKind) {
			last[tuple.FromID] = index
		}
	}
	if len(last) == 0 {
		return
	}
	placements := make([]Tuple, 0, len(last))
	for _, lens := range slices.Sorted(maps.Keys(last)) {
		placements = append(placements, tuples[last[lens]])
	}
	b.QueueBuilt("record the artifact_id of each lens the write places", lensArtifactIDsStatement, func() ([]any, error) {
		return tupleParams(placements)
	}).Query(func(rows pgx.Rows) error {
		if !rows.Next() {
			return nil
		}
		refused := &NoDocumentRowError{}
		if err := rows.Scan(&refused.Lens, &refused.ToKind, &refused.ToID); err != nil {
			return err
		}
		return refused
	})
}

// lensArtifactIDsStatement is queueLensArtifactIDs' statement, over the placements it binds as a
// tuple append binds its tuples (tupleParams), one for each lens, in ascending id. A lens placed on
// a document, or on a block of one - a block whose id is no finding's (FindingIDSQL) and no block of
// an ask, a comment or a message - records the row the artifacts table holds, as the write runs, at
// the address of that document: the node's id for a document, and for a block the address before
// the `#` of the block's id, which is its own address, `<ref_key>#<block id>`. That row is the one
// whose ref_key, the trigger's `<issue key>/<slug>`, `<project key>/<slug>` or
// `agent/<session>/<slug>`, is that address (one probe of its unique index). It reads neither the
// row a node at the address was written for, which may be a removed one's, nor the node a block
// stands under through its own part_of. Where the lens's row holds another value than that one, the
// statement moves the row's attribute to it and appends its value tuple, at the placement's time,
// writer and cause: the row's id where the lens stands on a document, and where it does not - a lens
// moved off a document onto any other node, a finding of another lens, a block of an ask - the
// retraction of the value it held, so the lens records none. A lens whose value is the one it holds
// already states nothing. A lens placed on a document no row stands at states nothing either, and
// the statement answers it, which refuses the write. It takes no row lock of its own and reads each
// lens's row and the artifacts row plainly: every writer that places a lens takes the issue's owner
// lock first (api's lockIssueOwner, which the reasoning routes' write and the pre-mortem conversion
// take before they write), and that lock serializes the placements of a lens, so no other placement
// moves a lens's row between this statement's read of it and its update.
var lensArtifactIDsStatement = `
	with placed as (
		select placement.from_id as lens, placement.to_kind, placement.to_id, placement.at, placement.who,
		       ` + causeOf("placement.source_event") + ` as source_event,
		       (placement.to_kind = '` + KindArtifact + `'
		        or placement.to_kind = '` + blockKind + `'
		           and not ` + FindingIDSQL("placement.to_id") + `
		           and split_part(placement.to_id, '/', 1) not in ('` + KindAsk + `', '` + KindComment + `', '` + KindMessage + `')) as on_document
		from unnest(` + tupleArrays(1) + `) as placement (` + tupleWriteColumns + `)
	),
	placements as (
		select placed.lens, placed.to_kind, placed.to_id, placed.at, placed.who, placed.source_event, placed.on_document,
		       held.attributes->>'` + AttributeArtifactID + `' as held_row,
		       case when placed.on_document then
		           (select document.id::text from artifacts document where document.ref_key = split_part(placed.to_id, '#', 1))
		       end as standing_row
		from placed
		join graph_nodes held on held.kind = '` + KindLens + `' and held.id = placed.lens
	),
	changed as (
		select * from placements
		where placements.standing_row is distinct from placements.held_row
		  and not (placements.on_document and placements.standing_row is null)
	),
	moved as (
		update graph_nodes held
		set attributes = case when changed.standing_row is null then held.attributes - '` + AttributeArtifactID + `'
			else held.attributes || jsonb_build_object('` + AttributeArtifactID + `', changed.standing_row) end
		from changed
		where held.kind = '` + KindLens + `' and held.id = changed.lens
	),
	appended as (
		insert into graph_tuples (` + tupleWriteColumns + `)
		select '` + KindLens + `', changed.lens, '` + AttributeArtifactID + `', '` + KindValue + `',
		       coalesce(changed.standing_row, changed.held_row), changed.at, changed.who, changed.source_event,
		       null, changed.standing_row is not null
		from changed
		order by changed.lens
		on conflict do nothing
	)
	select placements.lens, placements.to_kind, placements.to_id from placements
	where placements.on_document and placements.standing_row is null
	order by placements.lens`

// storedKindOf is the kind graph_nodes keys the node a tuple names by kind and id under: the kind
// itself, but for a finding of a lens, which a tuple may name lens_block, the kind the graph
// addresses it by, and the store keys as a block (AddressKind).
func storedKindOf(kind string) string {
	if kind == KindLensBlock {
		return blockKind
	}
	return kind
}

// pointerTarget is one far end of a pointer, as a lens's has_kind tuples hold it.
type pointerTarget struct{ kind, id string }

// pointerEnds is what a pointerCheck reads of the nodes a write's ruled tuples name: the block type
// of each block an end of one is ruled by, the project of the document that holds it and whether
// that document is retired, the project of each near end an ownProject rule compares with its far
// end's, the kinds each lens holds now, and where each lens at either end of a triggered_by stands
// in the order lenses were invoked.
type pointerEnds struct {
	blockTypes    map[string]string
	blockProjects map[string]string
	blockRetired  map[string]bool
	nodeProjects  map[pointerTarget]string
	lensKinds     map[string][]pointerTarget
	invoked       map[string]invocation
}

// invocation is where a lens stands in the order lenses were invoked (InvokedSeqSQL): at seq, the
// seq of the first part_of tuple the store holds of it; or, for a lens the store places nowhere
// (seq 0), after every lens it does place - at written, the position of the lens's first part_of
// among the tuples of the write that is checked, which are appended after every tuple the store
// holds and in their order, and last of all where that write places it nowhere either (written
// -1). So the lens a write invokes, its part_of beside its triggered_by, stands after every lens
// already invoked.
type invocation struct {
	seq     int64
	written int
}

// before is whether a lens at a was invoked before a lens at b; a lens at the same place as
// another - the lens itself, or two lenses neither the store nor the write places (seq 0, written
// -1) - was not, so before answers false both ways.
func (a invocation) before(b invocation) bool {
	if a.seq > 0 || b.seq > 0 {
		return a.seq > 0 && (b.seq == 0 || a.seq < b.seq)
	}
	return a.written >= 0 && (b.written < 0 || a.written < b.written)
}

// pointerCheck is the check of one write's ruled relations (pointerRules) and of the part_of
// tuples it places at a node of a kind that takes a node reserved for its author
// (placesUnderAuthorOnly), planned in Go off the write's own nodes and tuples before anything of
// the write is queued (planPointerCheck): what the rules ask of the store, read in statements of
// the write phase queued between the node write and the append (queue), and what the write
// decides itself. The last of those statements' callbacks decides (decide), so the refusal is the
// result Send answers for the write; a check that reads nothing is decided before anything is
// queued, so its refusal sends nothing.
type pointerCheck struct {
	tuples        []Tuple
	ruled, placed []int
	ends          pointerEnds
	// reserved holds, for the ends of the placed part_of tuples, whether each is reserved for its
	// author: as the write writes it, where the write decides it, and then as the store holds it.
	reserved map[pointerTarget]bool
	// The reads the rules ask for: the blocks whose type an end is ruled by, the lenses at either
	// end of a triggered_by, the near ends of an ownProject rule no node of the write states the
	// project of, the lenses whose has_kind tuples the write states and whose row it does not
	// write, which it locks, the lenses whose kinds a rule reads, and the ends of a placed part_of
	// whose visibility the write does not decide.
	blocks, invoked, locked, lenses []string
	projectKinds, projectIDs        []string
	visibilityKinds, visibilityIDs  []string
}

// planPointerCheck plans the check of every relation of tuples that pointerRules rules and every
// part_of tuples asserts at a node of a kind that takes a node reserved for its author: an
// InvalidPointerError, ErrLensKindAsserted or ErrLensKindKept for the first breach of a rule, and
// a VisibilityWiderThanParentError for a part_of out of a node that is not reserved for its author
// at one that is. nodes are the nodes the write writes (QueueWrite) or wrote (QueueCaused), whose
// rows the write holds before the check reads, and written says the write's own node write runs
// in the same append, so the visibility of each node it writes and does not restate is the one
// the node carries, decided in Go; for a write held for its cause (QueueCaused) every end is
// read. A write no rule holds a tuple of - every write of this store but those of the reasoning
// kinds and their places - answers nil and plans nothing, and one whose every rule the write
// decides itself is decided here, before anything is queued: it answers the refusal, or nil.
// A refusal ends the caller's transaction, which rolls the nodes it wrote back with it, as
// ErrProjectMoved does.
//
// An assertion is held to the kinds of both its ends, a risk's prevented_by to the project of its
// risk and to a document that is not retired - the check stands in a document of that project,
// read off the document that holds it as its type is - and a lens's triggered_by to a lens invoked
// before it, each read off the seq of its first part_of and, for a lens the store places nowhere
// yet, off this write's own part_of tuples (invocation). A retraction is held to nothing but the
// has_kind rule: it ends what a write already took, and a block the index pass removes has no
// document_blocks row left to read its type off when its pointers are retracted.
//
// A lens holds one kind at a time, which is read off the store and then off this write's own
// has_kind tuples in their order, so a retarget - the retraction of one far end and the assertion
// of another - is taken, and a second assertion is refused. Two writes asserting a kind of one
// lens must not both read it kindless, so a write that asserts or retracts a has_kind tuple out of
// a lens it does not itself write first takes that lens's graph_nodes row `for no key update`,
// after the rows the write took already and before it reads the lens's kinds and appends, as the
// lock order on api/inbox_read.go's markInboxReadThreads says. A risk's part_of is read against
// the kinds its lens holds once the whole write is read, so a write that makes a pre-mortem and
// its risks together may state them in any order.
//
// A placed part_of is decided off the visibility of its two ends as the write leaves their rows: a
// child reserved for its author passes whatever its parent is, and every other end the write does
// not decide is read in one statement. An end the store holds no row of is visible to every
// reader, as every read answers one (refs.VisibleKind). The read takes no lock: no writer of these
// kinds writes a node again at another visibility (a write of a node's state keeps the row's own),
// so the visibility it reads is the one each end keeps.
func planPointerCheck(nodes []Node, tuples []Tuple, written bool) (*pointerCheck, error) {
	var ruled, placed []int
	for index := range tuples {
		tuple := &tuples[index]
		if tuple.ToKind == KindValue {
			continue
		}
		if pointerRuleOf(tuple.Kind, tuple.FromKind) != nil {
			ruled = append(ruled, index)
		}
		if tuple.Asserted && placesUnderAuthorOnly(tuple.Kind, tuple.ToKind, tuple.ToID) {
			placed = append(placed, index)
		}
	}
	if len(ruled) == 0 && len(placed) == 0 {
		return nil, nil
	}
	check := &pointerCheck{tuples: tuples, ruled: ruled, placed: placed}
	check.planEnds(nodes)
	check.planPlaces(nodes, written)
	if len(check.blocks)+len(check.invoked)+len(check.projectIDs)+len(check.locked)+len(check.lenses)+len(check.visibilityIDs) == 0 {
		return nil, check.decide()
	}
	return check, nil
}

// planEnds plans what the ruled tuples' rules ask of their ends: the type of every block an end is
// ruled by, the project of its document and whether that document is retired, the project of each
// near end of an ownProject rule - the one a node of the write states, which its row holds already
// (a node's project never moves, ErrProjectMoved), and otherwise the one its graph_nodes row holds
// - where each lens at either end of a triggered_by stands in the order lenses were invoked, the
// lock of every lens whose has_kind tuples the write states and does not write the node of, and
// the has_kind tuples each named lens holds now, which a lens the write makes holds none of.
func (c *pointerCheck) planEnds(nodes []Node) {
	if len(c.ruled) == 0 {
		return
	}
	c.ends.lensKinds = map[string][]pointerTarget{}
	var near []pointerTarget
	for _, index := range c.ruled {
		tuple := &c.tuples[index]
		rule := pointerRuleOf(tuple.Kind, tuple.FromKind)
		if tuple.Asserted && tuple.FromKind == blockKind && typesBlocks(rule.from) {
			c.blocks = append(c.blocks, tuple.FromID)
		}
		if tuple.Asserted && tuple.ToKind == blockKind && typesBlocks(rule.to) {
			c.blocks = append(c.blocks, tuple.ToID)
		}
		if tuple.Asserted && rule.ownProject {
			near = append(near, pointerTarget{kind: tuple.FromKind, id: tuple.FromID})
		}
		switch {
		case rule.single && tuple.FromKind == KindLens:
			c.lenses = append(c.lenses, tuple.FromID)
			if !slices.ContainsFunc(nodes, func(node Node) bool { return node.Kind == KindLens && node.ID == tuple.FromID }) {
				c.locked = append(c.locked, tuple.FromID)
			}
		case rule.underKind != "" && tuple.Asserted && tuple.ToKind == KindLens:
			c.lenses = append(c.lenses, tuple.ToID)
		case rule.earlier && tuple.Asserted && tuple.FromKind == KindLens && tuple.ToKind == KindLens:
			c.invoked = append(c.invoked, tuple.FromID, tuple.ToID)
		}
	}
	if len(c.blocks) > 0 {
		c.ends.blockTypes, c.ends.blockProjects, c.ends.blockRetired = map[string]string{}, map[string]string{}, map[string]bool{}
	}
	if len(near) > 0 {
		c.ends.nodeProjects = make(map[pointerTarget]string, len(near))
		for _, end := range near {
			if _, stated := c.ends.nodeProjects[end]; stated {
				continue
			}
			if at := slices.IndexFunc(nodes, func(node Node) bool {
				return node.Kind == end.kind && node.ID == end.id && node.Project != ""
			}); at >= 0 {
				c.ends.nodeProjects[end] = nodes[at].Project
				continue
			}
			c.projectKinds, c.projectIDs = append(c.projectKinds, end.kind), append(c.projectIDs, end.id)
		}
	}
	for _, list := range []*[]string{&c.invoked, &c.locked, &c.lenses} {
		slices.Sort(*list)
		*list = slices.Compact(*list)
	}
}

// planPlaces plans what the placed part_of tuples ask of their ends: the visibility of each end
// the write does not decide. Where written says so, an end the write writes and does not restate
// holds the visibility the write gives it, so it is decided here and never read: a child written
// reserved for its author passes whatever its parent is, and a tuple both of whose ends the write
// writes so is decided off the two.
func (c *pointerCheck) planPlaces(nodes []Node, written bool) {
	if len(c.placed) == 0 {
		return
	}
	c.reserved = map[pointerTarget]bool{}
	if written {
		for index := range nodes {
			if node := &nodes[index]; !node.Restates {
				c.reserved[pointerTarget{kind: storedKindOf(node.Kind), id: node.ID}] = node.Visibility == VisibilityAuthor
			}
		}
	}
	for _, index := range c.placed {
		tuple := &c.tuples[index]
		child := pointerTarget{kind: storedKindOf(tuple.FromKind), id: tuple.FromID}
		if c.reserved[child] {
			continue
		}
		for _, end := range [...]pointerTarget{child, {kind: storedKindOf(tuple.ToKind), id: tuple.ToID}} {
			if _, decided := c.reserved[end]; !decided {
				c.visibilityKinds, c.visibilityIDs = append(c.visibilityKinds, end.kind), append(c.visibilityIDs, end.id)
			}
		}
	}
}

// pointerRead is one statement of a pointerCheck: what it reads, as a failure of it is named, and
// the callback that reads its rows into the check.
type pointerRead struct {
	what, sql string
	args      []any
	fill      func(pgx.Rows) error
}

// queue queues on b the statements that read what the check asks of the store, in the order
// readBlocks, the invocations, the projects, the lock, the lens kinds and the visibility read
// them, after the node write and before the append that QueueWrite and QueueCaused queue around
// it. Each statement's callback reads its rows into the check, and the last one's decides it, so
// the refusal is that statement's result. A nil check - a write planPointerCheck decided, or one
// no rule holds a tuple of - queues nothing.
func (c *pointerCheck) queue(b *store.WriteBatch) {
	if c == nil {
		return
	}
	var reads []pointerRead
	if len(c.blocks) > 0 {
		reads = append(reads, c.readBlocks())
	}
	if len(c.invoked) > 0 {
		reads = append(reads, c.readInvocations())
	}
	if len(c.projectIDs) > 0 {
		reads = append(reads, c.readNodeProjects())
	}
	if len(c.locked) > 0 {
		reads = append(reads, pointerRead{
			what: "lock the lenses whose kind the write states",
			sql: `select 1 from graph_nodes held
				where held.kind = $1 and held.id = any($2::text[])
				order by held.id
				for no key update of held`,
			args: []any{KindLens, c.locked},
			fill: func(pgx.Rows) error { return nil },
		})
	}
	if len(c.lenses) > 0 {
		reads = append(reads, c.readLensKinds())
	}
	if len(c.visibilityIDs) > 0 {
		reads = append(reads, c.readVisibility())
	}
	for index, read := range reads {
		last := index == len(reads)-1
		b.Queue(read.what, read.sql, read.args...).Query(func(rows pgx.Rows) error {
			if err := read.fill(rows); err != nil {
				return fmt.Errorf("%s: %w", read.what, err)
			}
			if last {
				return c.decide()
			}
			return nil
		})
	}
}

// decide holds each ruled tuple to its rule (pointerEnds.check), and then each placed part_of to
// the visibility of its two ends, off what the write decided and the store answered.
func (c *pointerCheck) decide() error {
	if len(c.ruled) > 0 {
		if err := c.ends.check(c.tuples, c.ruled); err != nil {
			return err
		}
	}
	for _, index := range c.placed {
		tuple := &c.tuples[index]
		if c.reserved[pointerTarget{kind: storedKindOf(tuple.ToKind), id: tuple.ToID}] &&
			!c.reserved[pointerTarget{kind: storedKindOf(tuple.FromKind), id: tuple.FromID}] {
			return &VisibilityWiderThanParentError{
				ChildKind: AddressKind(tuple.FromKind, tuple.FromID), ChildID: tuple.FromID,
				ParentKind: AddressKind(tuple.ToKind, tuple.ToID), ParentID: tuple.ToID,
			}
		}
	}
	return nil
}

// readLensKinds reads the has_kind tuples each lens of the check holds now, in one statement,
// each lens's in the order CurrentOut answers them (by far end).
func (c *pointerCheck) readLensKinds() pointerRead {
	return pointerRead{
		what: "read the kinds of the lenses a pointer names",
		sql: `select newest.from_id, newest.to_kind, newest.to_id from (
			` + CurrentTupleRead(`t.from_id, t.kind, t.to_kind, t.to_id`, `t.from_id, t.to_kind, t.to_id, t.asserted`,
			`t.from_kind = $1 and t.from_id = any($2::text[]) and t.kind = $3 and t.to_kind <> '`+KindValue+`'`) + `
			) newest
			where newest.asserted
			order by newest.from_id, newest.to_kind, newest.to_id`,
		args: []any{KindLens, c.lenses, TupleHasKind},
		fill: func(rows pgx.Rows) error {
			for rows.Next() {
				var lens string
				var target pointerTarget
				if err := rows.Scan(&lens, &target.kind, &target.id); err != nil {
					return err
				}
				c.ends.lensKinds[lens] = append(c.ends.lensKinds[lens], target)
			}
			return nil
		},
	}
}

// readInvocations reads where each lens of either end of a triggered_by stands in the order lenses
// were invoked (invocation), in one statement: the seq of the first part_of tuple the store holds
// of it (InvokedSeqSQL), and, for a lens the store places nowhere, the position of its first
// asserted part_of among the write's own tuples. The order is fixed once a lens is placed, so it
// takes no lock: a lens another transaction places later stands later, and a lens no committed
// write placed is no far end any route reads.
func (c *pointerCheck) readInvocations() pointerRead {
	return pointerRead{
		what: "read where the lenses a triggered_by names were invoked",
		sql: `select named.id, coalesce(` + InvokedSeqSQL(`named.id`) + `, 0)
			from unnest($1::text[]) as named(id)`,
		args: []any{c.invoked},
		fill: func(rows pgx.Rows) error {
			c.ends.invoked = make(map[string]invocation, len(c.invoked))
			for rows.Next() {
				var lens string
				at := invocation{written: -1}
				if err := rows.Scan(&lens, &at.seq); err != nil {
					return err
				}
				if at.seq == 0 {
					at.written = slices.IndexFunc(c.tuples, func(tuple Tuple) bool {
						return tuple.Asserted && tuple.FromKind == KindLens && tuple.FromID == lens &&
							tuple.Kind == TuplePartOf && tuple.ToKind != KindValue
					})
				}
				c.ends.invoked[lens] = at
			}
			return nil
		},
	}
}

// readBlocks reads the type of each block of the check that a document holds now, the project of
// that document and whether it is retired (artifacts.retired_at, docs.RetireDocument), read off
// its document_blocks row by node_id, the block's address that every row carries, through that
// column's hash index (document_blocks_node_id, migration 0121), and the primary key of the
// artifacts row it names. It is not read by node_kind and node_address: a row written before the
// store's backfill, one a pod of the previous release writes during a roll, and one past the
// store's address bound leave both null, and its block is still the requirement or the check it
// is. A block no document holds - a finding of a lens, a block a version removed - answers none,
// and no rule that names a block type lets it through. A document no project holds, an agent
// conversation's file, answers no project. A retired document keeps its document_blocks rows, so
// its checks are read, and refused, for what they are. A write that asserts a prevented_by reads
// them holding the owner lock (store.LockOwner) of each check's document (its race with a
// retirement or an index pass: api's lockReasoningOwners).
func (c *pointerCheck) readBlocks() pointerRead {
	return pointerRead{
		what: "read the types of the blocks a pointer names",
		sql: `select d.node_id, d.type, a.project_key, a.retired_at is not null
			from document_blocks d join artifacts a on a.id = d.artifact_id
			where d.node_id = any($1::text[])`,
		args: []any{c.blocks},
		fill: func(rows pgx.Rows) error {
			for rows.Next() {
				var id, blockType string
				var project *string
				var retired bool
				if err := rows.Scan(&id, &blockType, &project, &retired); err != nil {
					return err
				}
				c.ends.blockTypes[id] = blockType
				if project != nil {
					c.ends.blockProjects[id] = *project
				}
				if retired {
					c.ends.blockRetired[id] = true
				}
			}
			return nil
		},
	}
}

// readNodeProjects reads the project the graph_nodes row of each near end of an ownProject rule
// that no node of the write states holds, in one statement. A node whose row holds no project
// answers none.
func (c *pointerCheck) readNodeProjects() pointerRead {
	return pointerRead{
		what: "read the projects of the nodes a pointer starts at",
		sql: `select held.kind, held.id, held.project_key from graph_nodes held
			join unnest($1::text[], $2::text[]) as named(kind, id) on named.kind = held.kind and named.id = held.id
			where held.project_key is not null`,
		args: []any{c.projectKinds, c.projectIDs},
		fill: func(rows pgx.Rows) error {
			for rows.Next() {
				var end pointerTarget
				var project string
				if err := rows.Scan(&end.kind, &end.id, &project); err != nil {
					return err
				}
				c.ends.nodeProjects[end] = project
			}
			return nil
		},
	}
}

// readVisibility reads which ends of the placed part_of tuples the write does not decide the store
// holds reserved for their author, in one statement, once the write's own nodes are written.
func (c *pointerCheck) readVisibility() pointerRead {
	return pointerRead{
		what: "read the visibility of the nodes a part_of places and its parents",
		sql: `select distinct held.kind, held.id from graph_nodes held
			join unnest($1::text[], $2::text[]) as named(kind, id) on named.kind = held.kind and named.id = held.id
			where held.visibility = $3`,
		args: []any{c.visibilityKinds, c.visibilityIDs, VisibilityAuthor},
		fill: func(rows pgx.Rows) error {
			for rows.Next() {
				var end pointerTarget
				if err := rows.Scan(&end.kind, &end.id); err != nil {
					return err
				}
				c.reserved[end] = true
			}
			return nil
		},
	}
}

// check holds each ruled tuple to its rule in the write's order, the places of risks last
// (pointerCheck's decide).
func (e pointerEnds) check(tuples []Tuple, ruled []int) error {
	var emptied []string
	for _, index := range ruled {
		tuple := &tuples[index]
		rule := pointerRuleOf(tuple.Kind, tuple.FromKind)
		if rule.underKind != "" {
			continue
		}
		if !tuple.Asserted {
			if !rule.single || tuple.FromKind != KindLens {
				continue
			}
			kinds := e.lensKinds[tuple.FromID]
			at := slices.Index(kinds, pointerTarget{kind: tuple.ToKind, id: tuple.ToID})
			if at < 0 {
				continue
			}
			if tuple.ToKind == KindLensKind && tuple.ToID == LensKindPreMortem {
				return ErrLensKindKept
			}
			e.lensKinds[tuple.FromID] = slices.Delete(kinds, at, at+1)
			emptied = append(emptied, tuple.FromID)
			continue
		}
		if !e.allows(rule.from, tuple.FromKind, tuple.FromID) || !e.allows(rule.to, tuple.ToKind, tuple.ToID) {
			return e.breach(rule, tuple, e.named(rule.to, tuple.ToKind, tuple.ToID))
		}
		if rule.earlier && !e.invoked[tuple.ToID].before(e.invoked[tuple.FromID]) {
			return notInvokedBefore(rule, tuple)
		}
		if rule.ownProject {
			// The far end passed its types, so it is a block, whose project is its document's.
			near, far := e.nodeProjects[pointerTarget{kind: tuple.FromKind, id: tuple.FromID}], e.blockProjects[tuple.ToID]
			if near != "" && far != "" && near != far {
				return e.acrossProjects(rule, tuple, near, far)
			}
		}
		if rule.unretired && e.blockRetired[tuple.ToID] {
			return e.inRetiredDocument(rule, tuple)
		}
		if rule.single {
			kinds := e.lensKinds[tuple.FromID]
			if len(kinds) > 0 {
				return ErrLensKindAsserted
			}
			e.lensKinds[tuple.FromID] = append(kinds, pointerTarget{kind: tuple.ToKind, id: tuple.ToID})
		}
	}
	for _, lens := range emptied {
		if len(e.lensKinds[lens]) == 0 {
			return &InvalidPointerError{Kind: TupleHasKind, From: WithArticle(KindLens), Retracted: true,
				Rule: "a lens holds exactly one kind, so its has_kind tuple is retargeted, never retracted alone"}
		}
	}
	for _, index := range ruled {
		tuple := &tuples[index]
		rule := pointerRuleOf(tuple.Kind, tuple.FromKind)
		if rule.underKind == "" || !tuple.Asserted {
			continue
		}
		if !e.allows(rule.to, tuple.ToKind, tuple.ToID) {
			return e.breach(rule, tuple, e.named(rule.to, tuple.ToKind, tuple.ToID))
		}
		kinds := e.lensKinds[tuple.ToID]
		if !slices.Contains(kinds, pointerTarget{kind: KindLensKind, id: rule.underKind}) {
			return e.breach(rule, tuple, WithArticle(KindLens)+" of "+lensKindWords(kinds))
		}
	}
	return nil
}

// allows is whether one of ends is the node kind and id name, a block by the type it was read at.
func (e pointerEnds) allows(ends []pointerEnd, kind, id string) bool {
	return slices.ContainsFunc(ends, func(end pointerEnd) bool {
		return end.kind == kind && (end.blockType == "" || e.blockTypes[id] == end.blockType)
	})
}

// named is the node kind and id name as a refusal names it, with its article (word).
func (e pointerEnds) named(ends []pointerEnd, kind, id string) string {
	return WithArticle(e.word(ends, kind, id))
}

// word is the node kind and id name as a refusal names it: a block by its type where the rule's
// ends were read for one and a document holds it, and anything else by its kind.
func (e pointerEnds) word(ends []pointerEnd, kind, id string) string {
	if kind == blockKind && typesBlocks(ends) && e.blockTypes[id] != "" {
		return e.blockTypes[id] + " block"
	}
	return kind
}

func (e pointerEnds) breach(rule *pointerRule, tuple *Tuple, far string) error {
	return &InvalidPointerError{
		Kind: tuple.Kind, From: e.named(rule.from, tuple.FromKind, tuple.FromID), To: far, Rule: rule.allowed(),
	}
}

// acrossProjects is the refusal of tuple, whose rule holds its far end to the project of its near
// end, at a far end of another project: it names the near end with its project, near, and the far
// end itself with its project, far - by the id the store keys it by, which the route that read it
// at an address names it by instead (InvalidPointerError.NamedAt). That route read the far end for
// its caller before the write, so the refusal names nothing the caller may not see.
func (e pointerEnds) acrossProjects(rule *pointerRule, tuple *Tuple, near, far string) error {
	foreign := e.keyedFar(rule, tuple, " of project "+far)
	return &InvalidPointerError{
		Kind: tuple.Kind, From: e.named(rule.from, tuple.FromKind, tuple.FromID) + " of project " + near,
		To: foreign.named(tuple.ToID), Rule: rule.allowed() + " of the same project", keyed: []keyedEnd{foreign},
	}
}

// inRetiredDocument is the refusal of tuple, whose rule holds its far end, a block, to a document
// that is not retired, at a block of a retired document, whose checks nobody runs: it names the far
// end by the id the store keys it by, which the route that read it at an address names it by
// instead (InvalidPointerError.NamedAt), as acrossProjects does.
func (e pointerEnds) inRetiredDocument(rule *pointerRule, tuple *Tuple) error {
	retired := e.keyedFar(rule, tuple, " of a retired document")
	return &InvalidPointerError{
		Kind: tuple.Kind, From: e.named(rule.from, tuple.FromKind, tuple.FromID), To: retired.named(tuple.ToID),
		Rule:  rule.allowed() + " of a document that is not retired, since nobody runs a retired document's checks",
		keyed: []keyedEnd{retired},
	}
}

// keyedFar is the far end of tuple as a refusal under rule names it, by the id the store keys it
// by: "the ", the far end's word (word), the id, and then after - " of project FAR" for
// acrossProjects, " of a retired document" for inRetiredDocument.
func (e pointerEnds) keyedFar(rule *pointerRule, tuple *Tuple, after string) keyedEnd {
	return keyedEnd{kind: tuple.ToKind, id: tuple.ToID, far: true,
		before: "the " + e.word(rule.to, tuple.ToKind, tuple.ToID) + " ", after: after}
}

// notInvokedBefore is the refusal of tuple, whose rule holds its far end to a lens invoked before
// its near end (pointerRule's earlier), at a lens that was not: the lens itself, or one invoked at
// the same moment or later. It names both lenses, the one lens twice for a lens that triggers
// itself, by the ids the store keys them by, which the route that read them at their addresses
// names them by instead (InvalidPointerError.NamedAt). The route read both for its caller before
// the write, so the refusal names nothing the caller may not see.
func notInvokedBefore(rule *pointerRule, tuple *Tuple) error {
	near := keyedEnd{kind: tuple.FromKind, id: tuple.FromID, before: "the " + KindLens + " "}
	far := keyedEnd{kind: tuple.ToKind, id: tuple.ToID, far: true, before: "the " + KindLens + " ", after: ", which was not invoked before it,"}
	if tuple.ToID == tuple.FromID {
		far.before, far.after = "that same "+KindLens+" ", ""
	}
	return &InvalidPointerError{
		Kind: tuple.Kind, From: near.named(tuple.FromID), To: far.named(tuple.ToID), Rule: rule.allowed(),
		keyed: []keyedEnd{near, far},
	}
}

// lensKindWords names the kinds a lens holds as a refusal reads them: "kind inversion", or "no
// kind".
func lensKindWords(kinds []pointerTarget) string {
	if len(kinds) == 0 {
		return "no kind"
	}
	words := make([]string, len(kinds))
	for index, kind := range kinds {
		words[index] = kind.id
	}
	return "kind " + strings.Join(words, ", ")
}

// Retarget moves one pointer out of a node from the far end it holds to another, in the caller's
// transaction: pointer is the asserted tuple as the node holds it - its near end, its kind and
// the far end it is at - dated and signed by the write that moves it, and toKind and toID the far
// end it moves to. It appends the retraction of the old far end and then the assertion of the new
// one, both through Write, so the type rule of the pointer's kind holds the move as it holds any
// other write: a lens's has_kind moves from one kind to another and stays one, and a pre-mortem's
// is refused with ErrLensKindKept whatever hangs under it. It touches no part_of tuple, and
// refuses to move one: a node moves to another parent through cybermonklabs/legion#4180's `/parent`.
//
// It reads first (planRetarget), taking the node's row before it reads the pointers the node
// holds, and then writes the two tuples that read answers in one Write.
func Retarget(ctx context.Context, tx pgx.Tx, pointer Tuple, toKind, toID string) error {
	moved, err := planRetarget(ctx, tx, pointer, toKind, toID)
	if err != nil || len(moved) == 0 {
		return err
	}
	return Write(ctx, tx, nil, moved)
}

// planRetarget is the read phase of Retarget, which writes nothing: it first takes the node's
// graph_nodes row `for no key update`, the one node row a retarget takes, so two retargets of one
// node's pointers queue there and the second reads what the first moved, and then reads the
// pointers the node holds. A node that does not hold pointer is ErrPointerNotHeld, and a move to
// the far end it holds already answers no tuple. Otherwise it answers the retraction of the old
// far end and the assertion of the new one, in that order, for the write that moves it. That both
// far ends exist, are not gone and may be seen by the caller is the route's to check before it
// calls this (pointerRules).
func planRetarget(ctx context.Context, tx pgx.Tx, pointer Tuple, toKind, toID string) ([]Tuple, error) {
	if pointer.Kind == TuplePartOf {
		return nil, errors.New("a part_of tuple is the node's place, which its parent route moves: it is never retargeted")
	}
	if _, err := tx.Exec(ctx, NodeRowLockSQL, pointer.FromKind, pointer.FromID); err != nil {
		return nil, fmt.Errorf("lock the node whose %s tuple moves: %w", pointer.Kind, err)
	}
	held, err := CurrentOut(ctx, tx, pointer.FromKind, pointer.FromID, []string{pointer.Kind})
	if err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(held, func(tuple Tuple) bool { return tuple.ToKind == pointer.ToKind && tuple.ToID == pointer.ToID }) {
		return nil, ErrPointerNotHeld
	}
	if pointer.ToKind == toKind && pointer.ToID == toID {
		return nil, nil
	}
	retraction := pointer
	retraction.Seq, retraction.LegacySourceSeq, retraction.Asserted = 0, nil, false
	assertion := retraction
	assertion.ToKind, assertion.ToID, assertion.Asserted = toKind, toID, true
	return []Tuple{retraction, assertion}, nil
}
