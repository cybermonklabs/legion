package graphstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
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
// the tuples alone, with no projection row behind it (migration 20261126050002), and
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
// (migration 20261123174710 declares both), carried on the node's row as the catalogue's words
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
// admission, the census and every read of a finding tell a finding from any other block by the
// same test. Migration 20261126050002 spells the same test in its own text.
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
	// refused, and a retraction is taken only beside the assertion that replaces it, or in a write
	// that states the node gone (R7's delete, which retracts every tuple out of what it takes).
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

// PlacementKinds is the kind of each placement pointerRules holds, in the table's order: part_of,
// a risk's place under a pre-mortem lens, which the table rules for a risk and lets through out of
// any other node. A placement is a node's place, which no pointer route moves, so PointerKinds
// lists none of them. Each call answers a slice of its own.
func PlacementKinds() []string {
	kinds := make([]string, 0, len(pointerRules))
	for _, rule := range pointerRules {
		if rule.placement {
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
		if tuple := &tuples[index]; readInWritePhase(tuple.Kind, tuple.FromKind, tuple.ToKind, tuple.ToID) {
			return fmt.Errorf("%w: %s out of %s", errPointerInArms, tuple.Kind, tuple.FromKind)
		}
	}
	for index := range claims {
		if claim := &claims[index]; readInWritePhase(claim.Kind, claim.FromKind, claim.ToKind, claim.ToID) {
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
