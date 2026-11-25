package graphstore

import (
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// typesBlocks is whether one of ends names a block type, which a block at that end of a tuple is
// read for.
func typesBlocks(ends []pointerEnd) bool {
	return slices.ContainsFunc(ends, func(end pointerEnd) bool { return end.blockType != "" })
}

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

// RoomBlockRowSQL is the document that the issue_room_blocks row of block block (SQL text) under
// the tree artifact tree (SQL text answering a uuid) names, as SQL text answering a uuid, null
// where no row names the block: the document whose fragment of an issue room holds a block whose
// document_blocks row the room's tree artifact keys. The table is keyed by block_id_key(block_id)
// (migration 20261118230000_issue_tree_room_blocks_key), which holds an id of any length, so the
// row is read through that key, and the id itself is compared beside it. It is the one reading of
// that record: refs reads the document of a document_blocks row (refs.BlockDocumentSQL) and of a
// room's block (refs.RoomBlockDocumentSQL) through it, and the pointer check reads a check's
// document through it (readBlocks). It stands here, below refs, so that both read the one record.
func RoomBlockRowSQL(tree, block string) string {
	return `(select room_block.document_id from issue_room_blocks room_block
		where room_block.tree_artifact_id = ` + tree + ` and block_id_key(room_block.block_id) = block_id_key(` + block + `)
		  and room_block.block_id = ` + block + `)`
}

// readBlocks reads the type of each block of the check that a document holds now, the project of
// that document and whether it is retired (artifacts.retired_at, docs.RetireDocument), read off
// its document_blocks row by node_id, the block's address that every row carries, through that
// column's hash index (document_blocks_node_id, migration 0121), and the primary key of the
// artifacts row of the document the row is a block of, as refs.BlockDocumentSQL reads it: the
// document whose fragment holds the block for a row of an issue room, which the room's tree
// artifact keys (RoomBlockRowSQL), and the row's own artifact for every other row. So a check of a
// converted document is judged by that document, which its retirement retires, and never by the
// tree, which stands. It is not read by node_kind and node_address: a row written before the
// store's backfill, one a pod of the previous release writes during a roll, and one past the
// store's address bound leave both null, and its block is still the requirement or the check it
// is. A block no document holds - a finding of a lens, a block a version removed - answers none,
// and no rule that names a block type lets it through. A document no project holds, an agent
// conversation's file, answers no project. A retired document keeps its document_blocks rows, and
// a retired document of an issue room keeps its blocks' issue_room_blocks rows too, since the
// room's passes keep those blocks as its retirement left them, so its checks are read, and
// refused, for what they are. A write that asserts a prevented_by reads them holding the owner
// lock (store.LockOwner) of each check's document (its race with a retirement or an index pass:
// api's lockReasoningOwners).
func (c *pointerCheck) readBlocks() pointerRead {
	return pointerRead{
		what: "read the types of the blocks a pointer names",
		sql: `select d.node_id, d.type, a.project_key, a.retired_at is not null
			from document_blocks d
			join artifacts a on a.id = coalesce(` + RoomBlockRowSQL("d.artifact_id", "d.block_id") + `, d.artifact_id)
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
