package graphstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

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
// names its lens, `lens/<uuid>#<block id>`, as a comment's body is blocks of the comment, and
// KindLensBlock is its address form: the from_kind graph_edges writes for it and the key the read
// registry knows it by (refs' nodeRegistry).
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
// to the lens whose finding invoked it. TupleConstrainedBy points from a requirement block to a
// constraint it is held to, and TuplePreventedBy from a risk to each check block that covers it, a
// check of any issue of the risk's own project. graph_edges answers each of them from the tuples
// alone, with no projection row behind it (migration 20261117020856), and refs.TupleKinds lists
// them.
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
// lens, `lens/<uuid>#<block id>`, and is addressed KindLensBlock, as the registry reads an item's
// block by its item's block kind; a block of a document, and every other node, is its own kind.
func AddressKind(kind, id string) string {
	if kind == blockKind && strings.HasPrefix(id, KindLens+"/") {
		return KindLensBlock
	}
	return kind
}

// AddressKindSQL is AddressKind over a node whose kind and id are SQL expressions of a read's own
// query.
func AddressKindSQL(kindExpr, idExpr string) string {
	return `(case when ` + kindExpr + ` = '` + blockKind + `' and ` + idExpr + ` like '` + KindLens + `/%' then '` +
		KindLensBlock + `' else ` + kindExpr + ` end)`
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
	// another refuses a tuple whose far end is its own near end.
	another bool
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
}

// pointerRules are the type rules of the four pointer tuples and of a risk's place, the one table
// of them (dispatch://INFRA-1950/spec#b-r-constraint-risk). Every write of this store holds each
// relation it appends whose kind is here to its rule, in the write's own transaction, before it
// appends anything (queuePointerChecks, in QueueWrite and QueueCaused), so no route keeps its own
// and a move through dispatch://INFRA-1253's `/parent` meets the same one. A kind that is not here
// is let through to any node.
//
// The table rules the types, and the project of a risk's prevented_by: a check of any issue of
// the risk's own project, as the pre-mortem conversion writes one
// (dispatch://INFRA-1950/spec#b-r-premortem-nodes), so the pointer route, the risk route and the
// conversion are held to one rule. That the far end exists, is not gone and may be seen by the
// caller is its route's to check, through the read registry's loader for the far end's kind (an
// issue or a document is read off its own row), so that a far end the caller may not see answers
// as one that does not exist and no refusal here tells that it is there: a check of another
// project reaches this rule only once its route has read it for the caller.
var pointerRules = [...]pointerRule{
	{kind: TupleHasKind, from: []pointerEnd{{kind: KindLens}}, to: []pointerEnd{{kind: KindLensKind}}, single: true},
	{kind: TupleTriggeredBy, from: []pointerEnd{{kind: KindLens}}, to: []pointerEnd{{kind: KindLens}}, another: true},
	{kind: TupleConstrainedBy,
		from: []pointerEnd{{kind: blockKind, blockType: blockTypeRequirement}, {kind: KindLensKind}},
		to:   []pointerEnd{{kind: KindConstraint}}},
	{kind: TuplePreventedBy, from: []pointerEnd{{kind: KindRisk}}, to: []pointerEnd{{kind: blockKind, blockType: BlockTypeCheck}},
		ownProject: true},
	{kind: TuplePartOf, from: []pointerEnd{{kind: KindRisk}}, to: []pointerEnd{{kind: KindLens}},
		placement: true, underKind: LensKindPreMortem},
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
	if r.another {
		far = "another " + r.to[0].word()
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
// risk, a lens triggered by itself, a risk under a lens of any kind but pre-mortem, a lens left
// with no kind. It is a sentinel so a route can answer it as the caller's refusal, 400
// INVALID_POINTER; what a write is refused with is an InvalidPointerError, which wraps it and
// names the tuple.
var ErrInvalidPointer = errors.New("a pointer tuple is at a node its type rule does not allow")

// InvalidPointerError is ErrInvalidPointer for one tuple: its kind, its two ends as the refusal
// names them, and what its rule lets through, so the words say what the caller pointed at and
// what it may point at instead. Retracted marks the retraction of a lens's one has_kind tuple with
// none asserted in its place, which leaves the lens no kind. foreign is the far end of a tuple
// refused for standing in another project than its near end (pointerRule's ownProject), which To
// names; it is the zero foreignEnd on every other refusal.
type InvalidPointerError struct {
	Kind, From, To, Rule string
	Retracted            bool
	foreign              foreignEnd
}

// foreignEnd is a far end of another project than its tuple's near end: its kind and id as the
// store keys it, its type as a refusal names it ("check block") and the project it stands in.
type foreignEnd struct{ kind, id, word, project string }

// named is the far end as its refusal's To names it, by name: "the check block <name> of project
// <project>".
func (end foreignEnd) named(name string) string {
	return fmt.Sprintf("the %s %s of project %s", end.word, name, end.project)
}

func (e *InvalidPointerError) Error() string {
	if e.Retracted {
		return fmt.Sprintf("the %s tuple from %s is retracted with none asserted in its place, which breaks its type rule: %s",
			e.Kind, e.From, e.Rule)
	}
	return fmt.Sprintf("the %s tuple from %s at %s breaks its type rule: %s", e.Kind, e.From, e.To, e.Rule)
}

func (e *InvalidPointerError) Unwrap() error { return ErrInvalidPointer }

// NamedAt is the refusal naming its far end by address, where it refuses a tuple at the node the
// store keys by kind and id for standing in another project than its near end: the store names
// that node by the id it keys it by - a block by its document's ref_key and its block id - and a
// route that read it at its dispatch:// address names it as its caller does. named is false, and
// the refusal nil, for every other refusal and for an empty address.
func (e *InvalidPointerError) NamedAt(kind, id, address string) (refusal *InvalidPointerError, named bool) {
	if e.foreign.id == "" || e.foreign.kind != kind || e.foreign.id != id || address == "" {
		return nil, false
	}
	at := *e
	at.To = e.foreign.named(address)
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

// errPointerInArms refuses a relation pointerRules rules handed to WriteArms: an arm spliced into
// a caller's statement runs with no read of the nodes its rule names, so a ruled tuple is
// appended only through QueueWrite or QueueCaused (Write, Caused), which check it in their
// transaction.
var errPointerInArms = errors.New("a pointer tuple is checked against its type rule in the write's own transaction, " +
	"by the store's Write or Caused, and never in the arms of a caller's statement")

// refusePointersInArms is errPointerInArms for the first ruled relation or claim a WriteArms call
// holds.
func refusePointersInArms(tuples []Tuple, claims []Claim) error {
	for index := range tuples {
		if tuple := &tuples[index]; tuple.ToKind != KindValue && pointerRuleOf(tuple.Kind, tuple.FromKind) != nil {
			return fmt.Errorf("%w: %s out of %s", errPointerInArms, tuple.Kind, tuple.FromKind)
		}
	}
	for index := range claims {
		if claim := &claims[index]; claim.ToKind != KindValue && pointerRuleOf(claim.Kind, claim.FromKind) != nil {
			return fmt.Errorf("%w: %s out of %s", errPointerInArms, claim.Kind, claim.FromKind)
		}
	}
	return nil
}

// pointerTarget is one far end of a pointer, as a lens's has_kind tuples hold it.
type pointerTarget struct{ kind, id string }

// pointerEnds is what queuePointerChecks reads of the nodes a write's ruled tuples name: the
// block type of each block an end of one is ruled by and the project of the document that holds
// it, the project of each near end an ownProject rule compares with its far end's, and the kinds
// each lens holds now.
type pointerEnds struct {
	blockTypes    map[string]string
	blockProjects map[string]string
	nodeProjects  map[pointerTarget]string
	lensKinds     map[string][]pointerTarget
}

// queuePointerChecks holds every relation of tuples that pointerRules rules to its rule, in the
// write's transaction, before the write appends a tuple, and refuses the first breach: an
// InvalidPointerError, or ErrLensKindAsserted or ErrLensKindKept. nodes are the nodes the write
// writes (QueueWrite) or wrote (QueueCaused), whose rows it holds already. What the rules ask of
// the store is read by statements queued here (pointerReads), and the last one's callback checks
// the tuples once every read has answered, so a breach fails the batch as Send reads it; a write
// whose rules read nothing is checked at once, before anything more is queued. A write no rule
// holds a tuple of - every write of this store but those of the reasoning kinds above - queues no
// statement here and makes nothing. A refusal ends the caller's transaction, which rolls the nodes
// it wrote back with it, as ErrProjectMoved does.
//
// An assertion is held to the kinds of both its ends, and a risk's prevented_by to the project of
// its risk: the check stands in a document of that project, read off the document that holds it
// as its type is. A retraction is held to nothing but the has_kind rule: it ends what a write
// already took, and a block the index pass removes has no document_blocks row left to read its
// type off when its pointers are retracted.
//
// A lens holds one kind at a time, which is read off the store and then off this write's own
// has_kind tuples in their order, so a retarget - the retraction of one far end and the assertion
// of another - is taken, and a second assertion is refused. Two writes asserting a kind of one
// lens must not both read it kindless, so a write that asserts or retracts a has_kind tuple out of
// a lens it does not itself write first takes that lens's graph_nodes row `for no key update`,
// after the rows the write took already and before it appends, as the lock order on
// api/inbox_read.go's markInboxReadThreads says. A risk's part_of is read against the kinds its
// lens holds once the whole write is read, so a write that makes a pre-mortem and its risks
// together may state them in any order.
func queuePointerChecks(b *store.WriteBatch, nodes []Node, tuples []Tuple) error {
	var ruled []int
	for index := range tuples {
		if tuple := &tuples[index]; tuple.ToKind != KindValue && pointerRuleOf(tuple.Kind, tuple.FromKind) != nil {
			ruled = append(ruled, index)
		}
	}
	if len(ruled) == 0 {
		return nil
	}
	ends := &pointerEnds{}
	reads := pointerReads(ends, nodes, tuples, ruled)
	check := func() error { return ends.check(tuples, ruled) }
	if len(reads) == 0 {
		return check()
	}
	for index, read := range reads {
		then := func() error { return nil }
		if index == len(reads)-1 {
			then = check
		}
		read(b, then)
	}
	return nil
}

// pointerRead queues one of the reads queuePointerChecks makes, whose callback fills what it read
// into the ends and then runs then: the check, for the last read, which runs once every read
// before it has filled its part, since Send reads the results in statement order.
type pointerRead func(b *store.WriteBatch, then func() error)

// pointerReads are the reads the ruled tuples' rules ask of their ends, in order: one statement
// for the type of every block an end is ruled by and the project of its document, one for the
// project of each near end of an ownProject rule that no node of the write states, the lock of
// every lens whose has_kind tuples the write states and does not write the node of, and one for
// the has_kind tuples each named lens holds now, which a lens the write's own insert made holds
// none of. What the write's own nodes state is filled into ends here, with no read.
func pointerReads(ends *pointerEnds, nodes []Node, tuples []Tuple, ruled []int) []pointerRead {
	var blocks, lenses, locked []string
	var placed []pointerTarget
	for _, index := range ruled {
		tuple := &tuples[index]
		rule := pointerRuleOf(tuple.Kind, tuple.FromKind)
		if tuple.Asserted && tuple.FromKind == blockKind && typesBlocks(rule.from) {
			blocks = append(blocks, tuple.FromID)
		}
		if tuple.Asserted && tuple.ToKind == blockKind && typesBlocks(rule.to) {
			blocks = append(blocks, tuple.ToID)
		}
		if tuple.Asserted && rule.ownProject {
			placed = append(placed, pointerTarget{kind: tuple.FromKind, id: tuple.FromID})
		}
		switch {
		case rule.single && tuple.FromKind == KindLens:
			lenses = append(lenses, tuple.FromID)
			if !slices.ContainsFunc(nodes, func(node Node) bool { return node.Kind == KindLens && node.ID == tuple.FromID }) {
				locked = append(locked, tuple.FromID)
			}
		case rule.underKind != "" && tuple.Asserted && tuple.ToKind == KindLens:
			lenses = append(lenses, tuple.ToID)
		}
	}
	var reads []pointerRead
	if len(blocks) > 0 {
		reads = append(reads, readBlocks(ends, blocks))
	}
	if len(placed) > 0 {
		if read := readNodeProjects(ends, nodes, placed); read != nil {
			reads = append(reads, read)
		}
	}
	if len(locked) > 0 {
		slices.Sort(locked)
		locked = slices.Compact(locked)
		reads = append(reads, func(b *store.WriteBatch, then func() error) {
			b.Queue("lock the lenses whose kind the write states", `
				select 1 from graph_nodes held
				where held.kind = $1 and held.id = any($2::text[])
				order by held.id
				for no key update of held
			`, KindLens, locked).Exec(func(pgconn.CommandTag) error { return then() })
		})
	}
	if len(lenses) > 0 {
		slices.Sort(lenses)
		reads = append(reads, readLensKinds(ends, slices.Compact(lenses)))
	}
	return reads
}

// readLensKinds reads into ends the has_kind tuples each of lenses holds now, in one statement,
// each lens's in the order CurrentOut answers them (by far end). A lens the write's own insert
// made holds none, the read being queued after that insert.
func readLensKinds(ends *pointerEnds, lenses []string) pointerRead {
	ends.lensKinds = make(map[string][]pointerTarget, len(lenses))
	return func(b *store.WriteBatch, then func() error) {
		b.Queue("read the kinds of the lenses a pointer names", `
			select newest.from_id, newest.to_kind, newest.to_id from (
				`+CurrentTupleRead(`t.from_id, t.kind, t.to_kind, t.to_id`, `t.from_id, t.to_kind, t.to_id, t.asserted`,
			`t.from_kind = $1 and t.from_id = any($2::text[]) and t.kind = $3 and t.to_kind <> '`+KindValue+`'`)+`
			) newest
			where newest.asserted
			order by newest.from_id, newest.to_kind, newest.to_id
		`, KindLens, lenses, TupleHasKind).Query(func(rows pgx.Rows) error {
			for rows.Next() {
				var lens string
				var target pointerTarget
				if err := rows.Scan(&lens, &target.kind, &target.id); err != nil {
					return fmt.Errorf("read the kinds of the lenses a pointer names: %w", err)
				}
				ends.lensKinds[lens] = append(ends.lensKinds[lens], target)
			}
			if err := rows.Err(); err != nil {
				return fmt.Errorf("read the kinds of the lenses a pointer names: %w", err)
			}
			return then()
		})
	}
}

// readBlocks reads into ends the type of each block of ids that a document holds now, and the
// project of that document, read off its document_blocks row by node_id, the block's address that
// every row carries, through that column's hash index (document_blocks_node_id, migration 0121),
// and the primary key of the artifacts row it names. It is not read by node_kind and
// node_address: a row written before the store's backfill, one a pod of the previous release
// writes during a roll, and one past the store's address bound leave both null, and its block is
// still the requirement or the check it is. A block no document holds - a finding of a lens, a
// block a version removed - answers none, and no rule that names a block type lets it through. A
// document no project holds, an agent conversation's file, answers no project.
func readBlocks(ends *pointerEnds, ids []string) pointerRead {
	ends.blockTypes, ends.blockProjects = make(map[string]string, len(ids)), make(map[string]string, len(ids))
	return func(b *store.WriteBatch, then func() error) {
		b.Queue("read the types of the blocks a pointer names", `
			select d.node_id, d.type, a.project_key
			from document_blocks d join artifacts a on a.id = d.artifact_id
			where d.node_id = any($1::text[])
		`, ids).Query(func(rows pgx.Rows) error {
			for rows.Next() {
				var id, blockType string
				var project *string
				if err := rows.Scan(&id, &blockType, &project); err != nil {
					return fmt.Errorf("read the types of the blocks a pointer names: %w", err)
				}
				ends.blockTypes[id] = blockType
				if project != nil {
					ends.blockProjects[id] = *project
				}
			}
			if err := rows.Err(); err != nil {
				return fmt.Errorf("read the types of the blocks a pointer names: %w", err)
			}
			return then()
		})
	}
}

// readNodeProjects fills into ends the project of each of placed, the near ends of the tuples an
// ownProject rule holds: the one a node of the write states, which its row holds already (a node's
// project never moves, ErrProjectMoved), and otherwise the one its graph_nodes row holds, read by
// the one statement it answers, or nil where the write's nodes state every one. A node whose row
// holds no project answers none.
func readNodeProjects(ends *pointerEnds, nodes []Node, placed []pointerTarget) pointerRead {
	ends.nodeProjects = make(map[pointerTarget]string, len(placed))
	var kinds, ids []string
	for _, end := range placed {
		if _, stated := ends.nodeProjects[end]; stated {
			continue
		}
		if at := slices.IndexFunc(nodes, func(node Node) bool {
			return node.Kind == end.kind && node.ID == end.id && node.Project != ""
		}); at >= 0 {
			ends.nodeProjects[end] = nodes[at].Project
			continue
		}
		kinds, ids = append(kinds, end.kind), append(ids, end.id)
	}
	if len(ids) == 0 {
		return nil
	}
	return func(b *store.WriteBatch, then func() error) {
		b.Queue("read the projects of the nodes a pointer starts at", `
			select held.kind, held.id, held.project_key from graph_nodes held
			join unnest($1::text[], $2::text[]) as named(kind, id) on named.kind = held.kind and named.id = held.id
			where held.project_key is not null
		`, kinds, ids).Query(func(rows pgx.Rows) error {
			for rows.Next() {
				var end pointerTarget
				var project string
				if err := rows.Scan(&end.kind, &end.id, &project); err != nil {
					return fmt.Errorf("read the projects of the nodes a pointer starts at: %w", err)
				}
				ends.nodeProjects[end] = project
			}
			if err := rows.Err(); err != nil {
				return fmt.Errorf("read the projects of the nodes a pointer starts at: %w", err)
			}
			return then()
		})
	}
}

// check holds each ruled tuple to its rule in the write's order, the places of risks last
// (queuePointerChecks).
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
		if rule.another && tuple.ToKind == tuple.FromKind && tuple.ToID == tuple.FromID {
			return e.breach(rule, tuple, "that same "+tuple.ToKind)
		}
		if rule.ownProject {
			// The far end passed its types, so it is a block, whose project is its document's.
			near, far := e.nodeProjects[pointerTarget{kind: tuple.FromKind, id: tuple.FromID}], e.blockProjects[tuple.ToID]
			if near != "" && far != "" && near != far {
				return e.acrossProjects(rule, tuple, near, far)
			}
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
	foreign := foreignEnd{kind: tuple.ToKind, id: tuple.ToID, word: e.word(rule.to, tuple.ToKind, tuple.ToID), project: far}
	return &InvalidPointerError{
		Kind: tuple.Kind, From: e.named(rule.from, tuple.FromKind, tuple.FromID) + " of project " + near,
		To: foreign.named(tuple.ToID), Rule: rule.allowed() + " of the same project", foreign: foreign,
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
// refuses to move one: a node moves to another parent through dispatch://INFRA-1253's `/parent`.
//
// It first takes the node's graph_nodes row `for no key update`, the one node row it takes, so two
// retargets of one node's pointers queue there and the second reads what the first moved. A node
// that does not hold pointer is ErrPointerNotHeld, and a move to the far end it holds already
// writes nothing. That both far ends exist, are not gone and may be seen by the caller is the
// route's to check before it calls this (pointerRules).
func Retarget(ctx context.Context, tx pgx.Tx, pointer Tuple, toKind, toID string) error {
	if pointer.Kind == TuplePartOf {
		return errors.New("a part_of tuple is the node's place, which its parent route moves: it is never retargeted")
	}
	if _, err := tx.Exec(ctx, NodeRowLockSQL, pointer.FromKind, pointer.FromID); err != nil {
		return fmt.Errorf("lock the node whose %s tuple moves: %w", pointer.Kind, err)
	}
	held, err := CurrentOut(ctx, tx, pointer.FromKind, pointer.FromID, []string{pointer.Kind})
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(held, func(tuple Tuple) bool { return tuple.ToKind == pointer.ToKind && tuple.ToID == pointer.ToID }) {
		return ErrPointerNotHeld
	}
	if pointer.ToKind == toKind && pointer.ToID == toID {
		return nil
	}
	retraction := pointer
	retraction.Seq, retraction.LegacySourceSeq, retraction.Asserted = 0, nil, false
	assertion := retraction
	assertion.ToKind, assertion.ToID, assertion.Asserted = toKind, toID, true
	return Write(ctx, tx, nil, []Tuple{retraction, assertion})
}
