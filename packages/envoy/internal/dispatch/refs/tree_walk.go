package refs

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The reads a walk of an issue's tree makes (docs' WalkIssueTree), each as of one cut of the tuple
// log: of each relation or value it takes the newest tuple the cut holds by its time and then its
// seq, kept while it asserts, as graphstore.AsOfOut and AsOfIn read a node at a time. A record's
// cut holds every tuple whose seq is at or below its own, so the head - the newest seq the log
// holds - and every version a record was taken at are one read at two cuts; a render's cut holds
// every tuple dated at or before its time (TreeCut.Dated). Each statement reaches graph_tuples
// through graph_tuples_as_of_out, keyed on the node and the kind, or graph_tuples_as_of_in, keyed
// on the far end and the kind, and refs' TestEveryAsOfReadOfThisChildUsesAnIndexOfTheStore holds
// each to that.

// TreeCut is the one moment a walk of an issue's tree reads the store at. A record's cut holds
// each tuple whose seq is at or below ReadSeq, the highest graph_tuples.seq the record read,
// whatever its time. A render's cut, Dated, holds each tuple whose time is at or before At,
// whatever its seq: a render of a version reads, of each node, the newest text value, part_of
// and rank dated at or before the version's time, with every retraction dated before it applied
// (R15), so a tuple a later write dates after that time changes nothing of it. Either takes the
// issue's asks, comments and messages as of At, since the conversation test reads their rows by
// their times (ConversationNodes). The zero TreeCut is no cut: a walk given it reads the head's
// (HeadCut).
type TreeCut struct {
	At      time.Time
	ReadSeq int64
	Dated   bool
}

// bounds are the highest seq and the latest time a read of the cut takes a tuple at, which every
// statement here binds beside each other (cutBound): a record's cut leaves the time open, and a
// render's the seq.
func (cut TreeCut) bounds() (int64, pgtype.Timestamptz) {
	if cut.Dated {
		return math.MaxInt64, pgtype.Timestamptz{Time: cut.At, Valid: true}
	}
	return cut.ReadSeq, pgtype.Timestamptz{InfinityModifier: pgtype.Infinity, Valid: true}
}

// String names the cut in an error: the seq of a record's, the time of a render's.
func (cut TreeCut) String() string {
	if cut.Dated {
		return "as of " + cut.At.UTC().Format(time.RFC3339Nano)
	}
	return fmt.Sprintf("at seq %d", cut.ReadSeq)
}

// cutBound is the predicate a tuple t of a read meets at the cut whose seq and time bounds the SQL
// seq and at name (TreeCut.bounds).
func cutBound(seq, at string) string { return `t.seq <= ` + seq + ` and t.at <= ` + at }

// headCutStatement is the head's cut: the newest seq the tuple log holds, read off its primary
// key, and the database's clock as the statement runs. The clock is clock_timestamp() and not the
// transaction's now(), which is its beginning: a version a pass writes in that transaction is
// written after the pass dated its tuples (refs.IndexedVersion.At), and a render as of the cut's
// time must take them.
const headCutStatement = `select coalesce((select seq from graph_tuples order by seq desc limit 1), 0), clock_timestamp()`

// HeadCut is the cut of the store as q sees it now: the newest seq of the tuple log and the
// database's clock. A version written in q's transaction records both, as the read_seq its record
// was read at and the time it renders as of.
func HeadCut(ctx context.Context, q Queryer) (TreeCut, error) {
	var cut TreeCut
	if err := q.QueryRow(ctx, headCutStatement).Scan(&cut.ReadSeq, &cut.At); err != nil {
		return TreeCut{}, fmt.Errorf("read the head's cut of the tuple log: %w", err)
	}
	return cut, nil
}

// jitOffStatement turns Postgres' JIT compiler off for the rest of the transaction it is sent in,
// and is allowed in a read-only one. The walk's reads probe the tuple table once a node, and
// Postgres costs a custom plan of such a read over a tree of thousands of nodes in the millions -
// it costs the relations read's probe of graph_tuples_as_of_out at about 8,650 a node - far above
// jit_above_cost, so with JIT on it compiled 70 to 460 ms of code for 50 to 130 ms of work, and a
// new connection plans its first executions of a statement that way.
const jitOffStatement = `set local jit = off`

// TurnJITOff sends jitOffStatement through q: the first statement of each transaction whose reads
// walk an issue's tree - the tree read, the coverage read over the tuples and the readers of a
// query's record. Sent outside a transaction it changes nothing, as Postgres warns.
func TurnJITOff(ctx context.Context, q Queryer) error {
	rows, err := q.Query(ctx, jitOffStatement)
	if err != nil {
		return fmt.Errorf("turn the JIT compiler off for the walk of a tree: %w", err)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("turn the JIT compiler off for the walk of a tree: %w", err)
	}
	return nil
}

// asOfValue is a lateral's body reading, at the cut the SQL predicate bound names (cutBound), the
// value of attribute out of the node whose kind and id the SQL kind and id name: the newest value
// tuple of that name, by its time and then its seq, as held where it asserts and null where it
// retracts. The opening of a text value is its first line alone, which says the type of a typed
// block (TreeNodeType).
func asOfValue(kind, id, attribute, bound string, opening bool) string {
	held := `t.to_id`
	if opening {
		held = `split_part(t.to_id, chr(10), 1)`
	}
	return `select case when t.asserted then ` + held + ` end as held
		from graph_tuples t
		where t.from_kind = ` + kind + ` and t.from_id = ` + id + ` and t.kind = '` + attribute + `'
		  and t.to_kind = '` + graphstore.KindValue + `' and ` + bound + `
		order by t.at desc, t.seq desc
		limit 1`
}

// asOfPlacement is a lateral's body reading, at the cut the SQL predicate bound names, the parent
// of the node whose kind and id the SQL kind and id name: of its part_of tuples, the newest of
// each far end kept while it asserts, and of those the newest, so a node stands under one parent
// at a time.
func asOfPlacement(kind, id, bound string) string {
	return `select held.to_kind, held.to_id from (
			select distinct on (t.to_kind, t.to_id) t.to_kind, t.to_id, t.asserted, t.at, t.seq
			from graph_tuples t
			where t.from_kind = ` + kind + ` and t.from_id = ` + id + ` and t.kind = '` + graphstore.TuplePartOf + `'
			  and t.to_kind <> '` + graphstore.KindValue + `' and ` + bound + `
			order by t.to_kind, t.to_id, t.at desc, t.seq desc
		) held
		where held.asserted
		order by held.at desc, held.seq desc
		limit 1`
}

// treeNodeColumns are the columns of a node the children read and the placed-above read answer
// beside its parent and its address: its rank, its supertag, when it was made and the opening of
// its text, read off the laterals and the row asOfNodeJoins names.
const treeNodeColumns = `rank.held, coalesce(node.supertag, ''), node.made_at, coalesce(opening.held, '')`

// asOfNodeJoins are the joins of the node whose kind and id the SQL kind and id name that
// treeNodeColumns reads, at the cut the SQL predicate bound names: its rank, its state, which a
// read keeps out where it is gone, the opening of its text, and its row.
func asOfNodeJoins(kind, id, bound string) string {
	return `left join lateral (` + asOfValue(kind, id, AttributeRank, bound, false) + `) rank on true
		left join lateral (` + asOfValue(kind, id, graphstore.AttributeState, bound, false) + `) state on true
		left join lateral (` + asOfValue(kind, id, graphstore.AttributeText, bound, true) + `) opening on true
		left join graph_nodes node on node.kind = ` + kind + ` and node.id = ` + id
}

// workflowKinds are the kinds of the nodes that record how an issue's work ran: a run of a step,
// the plan a run ran from, and the handoff a run reported, each part_of its issue (the kinds
// workflowstore writes and names). They are records of the workflow and not parts of the
// discussion, so the children read answers none of them, and a walk reaches nothing under one
// either. They are spelled here rather than imported, so the walk, and every package that walks
// a tree, depends on no workflow writer.
var workflowKinds = []string{"step", "plan", "handoff"}

// treeChildJoins are the joins of the children read under the parent whose kind and id the SQL kind
// and id name, at the cut whose bounds $3 and $4 are: each node whose part_of names the parent
// there - read into the parent through graph_tuples_as_of_in - unless its kind is one of
// workflowKinds, as child, with its newest standing part_of, read through graph_tuples_as_of_out,
// as placed, and the columns treeNodeColumns reads (asOfNodeJoins). treeChildStands keeps the
// children that stand under the parent there, and treeChildOrder orders one parent's.
func treeChildJoins(kind, id string) string {
	return `cross join lateral (
		select distinct on (t.from_kind, t.from_id) t.from_kind, t.from_id, t.asserted
		from graph_tuples t
		where t.to_kind = ` + kind + ` and t.to_id = ` + id + ` and t.kind = '` + graphstore.TuplePartOf + `'
		  and t.from_kind not in ('` + strings.Join(workflowKinds, `', '`) + `')
		  and t.to_kind <> '` + graphstore.KindValue + `' and ` + cutBound(`$3`, `$4`) + `
		order by t.from_kind, t.from_id, t.at desc, t.seq desc
	) child
	cross join lateral (` + asOfPlacement(`child.from_kind`, `child.from_id`, cutBound(`$3`, `$4`)) + `) placed
	` + asOfNodeJoins(`child.from_kind`, `child.from_id`, cutBound(`$3`, `$4`))
}

// treeChildStands is the condition a node treeChildJoins reads stands under the parent whose kind
// and id the SQL kind and id name: its part_of into the parent asserts, its newest standing part_of
// is that one, and its state is not gone.
func treeChildStands(kind, id string) string {
	return `child.asserted and placed.to_kind = ` + kind + ` and placed.to_id = ` + id + `
	  and state.held is distinct from '` + graphstore.StateGone + `'`
}

// treeChildOrder is the order of one parent's children, over the SQL rank and the child's kind and
// id: the ranked ones first by rank and then the unranked, a tie by the lower address,
// `<kind>:<id>`, each compared byte for byte (collate "C"), since a rank is a base-62 string of
// mixed case.
func treeChildOrder(rank, kind, id string) string {
	return rank + ` is null, ` + rank + ` collate "C", (` + kind + ` || ':' || ` + id + `) collate "C"`
}

// treeChildrenStatement is the children of each parent $1 and $2 name, kinds and ids, at the cut
// whose bounds $3 and $4 are (treeChildJoins, treeChildStands), by the parent's position and then
// in each parent's order (treeChildOrder).
var treeChildrenStatement = `
	select parent.position, child.from_kind, child.from_id, ` + treeNodeColumns + `
	from unnest($1::text[], $2::text[]) with ordinality as parent (kind, id, position)
	` + treeChildJoins(`parent.kind`, `parent.id`) + `
	where ` + treeChildStands(`parent.kind`, `parent.id`) + `
	order by parent.position, ` + treeChildOrder(`rank.held`, `child.from_kind`, `child.from_id`)

// treeSubtreesStatement is the children of each parent $1 and $2 name, kinds and ids, at the cut
// whose bounds $3 and $4 are, as treeChildrenStatement reads them, and the children of each child
// the walk of the tree of the issue $5 enters (TreeOwns, treeOwnsSQL), level by level down to the
// leaves, in one recursive statement: each row a parent and one of its children, in each parent's
// order (treeChildOrder). A union keeps each row once, so a node reached twice - a node of the
// walk's conversation that part_of also places under another node - is read under once, and a
// cycle of part_of tuples, which no writer leaves, ends rather than recursing for ever.
var treeSubtreesStatement = `
	with recursive under (parent_kind, parent_id, kind, id, held_rank, supertag, made_at, opening) as (
		select parent.kind, parent.id, child.from_kind, child.from_id, ` + treeNodeColumns + `
		from unnest($1::text[], $2::text[]) as parent (kind, id)
		` + treeChildJoins(`parent.kind`, `parent.id`) + `
		where ` + treeChildStands(`parent.kind`, `parent.id`) + `
		union
		select entered.kind, entered.id, child.from_kind, child.from_id, ` + treeNodeColumns + `
		from under entered
		` + treeChildJoins(`entered.kind`, `entered.id`) + `
		where ` + treeOwnsSQL(`$5::text`, `entered.kind`, `entered.id`) + `
		  and ` + treeChildStands(`entered.kind`, `entered.id`) + `
	)
	select parent_kind, parent_id, kind, id, held_rank, supertag, made_at, opening from under
	order by ` + treeChildOrder(`held_rank`, `kind`, `id`)

// TreeOwns reports whether node is a node of the tree of the issue owner as its address tells,
// which the walk of that tree (docs' WalkIssueTree) takes and enters: an issue's node is never one,
// and a document's, a version's or a document block's is one where the issue's key, owner, is the
// first part of its address, its document's ref_key (graphstore.ArtifactRefKey). A finding of a lens
// (graphstore.ParseFindingID) and every node whose address names no issue - an ask, a comment, a
// message, a lens, a risk, a result - is one where the walk reaches it. treeOwnsSQL is the same
// rule in SQL, which the subtrees read enters by (TreeSubtrees).
func TreeOwns(owner string, node Target) bool {
	switch node.Kind {
	case graphstore.KindIssue:
		return false
	case graphstore.KindArtifact, graphstore.KindVersion, BlockNodeKind:
		if _, _, finding := graphstore.ParseFindingID(node.ID); finding {
			return true
		}
		key, _, _ := strings.Cut(node.ID, "/")
		return key == owner
	}
	return true
}

// treeOwnsSQL is TreeOwns over the node whose kind and id the SQL kind and id name, owner the SQL
// issue key.
func treeOwnsSQL(owner, kind, id string) string {
	return `(` + kind + ` <> '` + graphstore.KindIssue + `' and (` + kind + ` not in ('` + graphstore.KindArtifact + `', '` +
		graphstore.KindVersion + `', '` + BlockNodeKind + `') or ` + graphstore.FindingIDSQL(id) + ` or split_part(` + id + `, '/', 1) = ` + owner + `))`
}

// TreeChild is one node a walk's read answers under a parent at a cut: the parent and the node;
// its rank, and whether it holds one; its supertag and when it was made, off its row (the zero
// time where the store holds none); and its type (TreeNodeType).
type TreeChild struct {
	Parent, Node Target
	Rank         string
	Ranked       bool
	Supertag     string
	MadeAt       time.Time
	Type         string
}

// TreeChildren is the children of each of parents at cut, each parent's in the order
// treeChildrenStatement answers them, the parents' in the order given.
func TreeChildren(ctx context.Context, q Queryer, parents []Target, cut TreeCut) ([]TreeChild, error) {
	if len(parents) == 0 {
		return nil, nil
	}
	kinds, ids := targetColumns(parents)
	seq, at := cut.bounds()
	rows, err := q.Query(ctx, treeChildrenStatement, kinds, ids, seq, at)
	if err != nil {
		return nil, fmt.Errorf("read the children of %d nodes %s: %w", len(parents), cut, err)
	}
	defer rows.Close()
	var children []TreeChild
	var position int
	var child TreeChild
	var row treeNodeRow
	targets := append([]any{&position, &child.Node.Kind, &child.Node.ID}, row.targets()...)
	for rows.Next() {
		child, row = TreeChild{}, treeNodeRow{}
		if err := rows.Scan(targets...); err != nil {
			return nil, fmt.Errorf("read a child %s: %w", cut, err)
		}
		child.Parent = parents[position-1]
		row.fill(&child)
		children = append(children, child)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the children of %d nodes %s: %w", len(parents), cut, err)
	}
	return children, nil
}

// TreeSubtrees is the children of each of parents at cut, and of each node under them the walk of
// the tree of the issue owner enters (TreeOwns), down to the leaves, keyed by the parent, each
// parent's in the order TreeChildren answers them: the reads TreeChildren makes level by level,
// read in one statement (treeSubtreesStatement), so the walk's statements do not grow with the
// tree's depth. A child the walk does not enter - an issue's node, a node of another issue - is
// answered under its parent, as TreeChildren answers it, and nothing under it is read.
func TreeSubtrees(ctx context.Context, q Queryer, parents []Target, owner string, cut TreeCut) (map[Target][]TreeChild, error) {
	under := map[Target][]TreeChild{}
	if len(parents) == 0 {
		return under, nil
	}
	kinds, ids := targetColumns(parents)
	seq, at := cut.bounds()
	rows, err := q.Query(ctx, treeSubtreesStatement, kinds, ids, seq, at, owner)
	if err != nil {
		return nil, fmt.Errorf("read the subtrees of %d nodes %s: %w", len(parents), cut, err)
	}
	defer rows.Close()
	var child TreeChild
	var row treeNodeRow
	targets := append([]any{&child.Parent.Kind, &child.Parent.ID, &child.Node.Kind, &child.Node.ID}, row.targets()...)
	for rows.Next() {
		child, row = TreeChild{}, treeNodeRow{}
		if err := rows.Scan(targets...); err != nil {
			return nil, fmt.Errorf("read a child %s: %w", cut, err)
		}
		row.fill(&child)
		under[child.Parent] = append(under[child.Parent], child)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the subtrees of %d nodes %s: %w", len(parents), cut, err)
	}
	return under, nil
}

// treeNodeRow is what treeNodeColumns answers of one node, as scanned.
type treeNodeRow struct {
	rank     *string
	supertag string
	madeAt   *time.Time
	opening  string
}

// targets are where a scan of treeNodeColumns writes, in their order.
func (row *treeNodeRow) targets() []any {
	return []any{&row.rank, &row.supertag, &row.madeAt, &row.opening}
}

// fill gives child what row read of it.
func (row treeNodeRow) fill(child *TreeChild) {
	if row.rank != nil {
		child.Rank, child.Ranked = *row.rank, true
	}
	if row.madeAt != nil {
		child.MadeAt = *row.madeAt
	}
	child.Supertag = row.supertag
	child.Type = TreeNodeType(child.Node.Kind, row.supertag, row.opening)
}

// TreeNodeType is the type a node of the tree is taken by. A block's is the typed block its text
// opens (pmdoc's grammar of a typed block's opening line), read off its text value as of the walk's
// cut, whatever supertag its row holds: the store keeps no history of a node's supertag -
// graphstore.Write moves the column of its row in place, and no tuple states it - so the text value
// at the cut is the one record of a block's type at that cut, and a version is typed as it stood,
// before a later supertag change and after one alike. A block whose text opens no typed block the
// schema declares - a paragraph, a finding of a lens - is taken by its supertag, and every other
// node by its supertag too: a result, an ask, a comment. A node the store holds no row of is taken
// by its kind.
func TreeNodeType(kind, supertag, opening string) string {
	if kind == BlockNodeKind {
		if name, _, ok := pmdoc.ParseTypedDirectiveOpen(strings.TrimRight(trimContainerMarkers(opening), "\r")); ok && pmdoc.IsTypedBlock(name) {
			return name
		}
	}
	if supertag == "" {
		return kind
	}
	return supertag
}

// trimContainerMarkers drops the indentation, quote markers (`>`), list markers (`-`, `*`, `+`, or
// a number and `.` or `)`, each followed by a space or a tab) and footnote labels (`[^label]:` with
// a label that is not blank), each with the spaces and tabs after it, that a block nested in a
// quote, a list item or a footnote is written after. The renderer writes the first block of a list
// item or of a footnote's definition on the marker's or the label's line, so a typed block's
// opening line can start with them.
func trimContainerMarkers(line string) string {
	for {
		line = strings.TrimLeft(line, " \t")
		switch {
		case strings.HasPrefix(line, ">"):
			line = line[1:]
		case len(line) > 1 && (line[0] == '-' || line[0] == '*' || line[0] == '+') && (line[1] == ' ' || line[1] == '\t'):
			line = line[2:]
		case strings.HasPrefix(line, "[^"):
			label, rest, closed := strings.Cut(line[2:], "]")
			if !closed || !strings.HasPrefix(rest, ":") || strings.TrimSpace(label) == "" {
				return line
			}
			line = rest[1:]
		default:
			digits := 0
			for digits < len(line) && line[digits] >= '0' && line[digits] <= '9' {
				digits++
			}
			if digits == 0 || digits+1 >= len(line) || (line[digits] != '.' && line[digits] != ')') || (line[digits+1] != ' ' && line[digits+1] != '\t') {
				return line
			}
			line = line[digits+2:]
		}
	}
}

// documentMembers is a lateral's body reading, at the cut the SQL predicate bound names, the
// blocks whose in_document names the document whose ref_key the SQL refKey names: of each block's
// in_document tuples into it, read into the document through graph_tuples_as_of_in, the newest,
// answered as member.from_id and member.asserted; a caller keeps those that assert.
func documentMembers(refKey, bound string) string {
	return `select distinct on (t.from_id) t.from_id, t.asserted
			from graph_tuples t
			where t.to_kind = '` + graphstore.KindArtifact + `' and t.to_id = ` + refKey + ` and t.kind = '` + TupleInDocument + `'
			  and t.from_kind = '` + BlockNodeKind + `' and t.to_kind <> '` + graphstore.KindValue + `' and ` + bound + `
			order by t.from_id, t.at desc, t.seq desc`
}

// treePlacedAboveStatement is, at the cut whose bounds $3 and $4 are, each block of the issue $1
// whose parent is a node of an issue of $2, the issues above it: the blocks whose in_document
// names a document whose part_of names the issue (documentMembers), each read into its far end
// through graph_tuples_as_of_in, of which those whose newest standing part_of, read through
// graph_tuples_as_of_out, names a block of a document of one of $2 - its address begins with that
// issue's key and a slash - or an ask, a comment or a message of one, or a block of one, unless
// its state there is gone. It answers them by the parent's address, then rank, then the node's
// address, each compared byte for byte, with the columns the children read answers.
var treePlacedAboveStatement = `
	with documents as (
		select placed.from_id as ref_key
		from (
			select distinct on (t.from_id) t.from_id, t.asserted
			from graph_tuples t
			where t.to_kind = '` + graphstore.KindIssue + `' and t.to_id = $1 and t.kind = '` + graphstore.TuplePartOf + `'
			  and t.from_kind = '` + graphstore.KindArtifact + `' and t.to_kind <> '` + graphstore.KindValue + `' and ` + cutBound(`$3`, `$4`) + `
			order by t.from_id, t.at desc, t.seq desc
		) placed
		where placed.asserted
	), blocks as (
		select distinct member.from_id as id
		from documents
		cross join lateral (` + documentMembers(`documents.ref_key`, cutBound(`$3`, `$4`)) + `) member
		where member.asserted
	)
	select placed.to_kind, placed.to_id, '` + BlockNodeKind + `', blocks.id, ` + treeNodeColumns + `
	from blocks
	cross join lateral (` + asOfPlacement(`'`+BlockNodeKind+`'`, `blocks.id`, cutBound(`$3`, `$4`)) + `) placed
	` + asOfNodeJoins(`'`+BlockNodeKind+`'`, `blocks.id`, cutBound(`$3`, `$4`)) + `
	where state.held is distinct from '` + graphstore.StateGone + `'
	  and (placed.to_kind = '` + BlockNodeKind + `' and split_part(placed.to_id, '/', 1) = any($2::text[])
	    or ` + itemOfIssuesSQL(ItemAsk, "asks", "$2") + `
	    or ` + itemOfIssuesSQL(ItemComment, "comments", "$2") + `
	    or ` + itemOfIssuesSQL(ItemMessage, "messages", "$2") + `)
	order by (placed.to_kind || ':' || placed.to_id) collate "C", rank.held is null, rank.held collate "C",
	         ('` + BlockNodeKind + `:' || blocks.id) collate "C"`

// itemOfIssuesSQL is true of a far end, placed.to_kind and placed.to_id, that is an item of kind,
// whose rows table holds, or a block of one (`<kind>/<uuid>#<block id>`, ParseItemBlockNode), of
// one of the issues the SQL text array issues names, read off the item's row by its key.
func itemOfIssuesSQL(kind ItemKind, table, issues string) string {
	id := `case when placed.to_kind = '` + string(kind) + `' then placed.to_id
		else split_part(split_part(placed.to_id, '#', 1), '/', 2) end`
	return `placed.to_kind in ('` + string(kind) + `', '` + kind.BlockKind() + `') and exists (
		select 1 from ` + table + ` item where item.id = ` + store.CastableUUID(id) + ` and item.issue_key = any(` + issues + `::text[]))`
}

// TreePlacedAbove is, at cut, each block of the issue issueKey that stands under a node of one of
// ancestors, the issues above it (IssueAncestors), with that node as its Parent, in the order
// treePlacedAboveStatement answers them. An issue with no issue above holds none.
func TreePlacedAbove(ctx context.Context, q Queryer, issueKey string, ancestors []string, cut TreeCut) ([]TreeChild, error) {
	if len(ancestors) == 0 {
		return nil, nil
	}
	seq, at := cut.bounds()
	rows, err := q.Query(ctx, treePlacedAboveStatement, issueKey, ancestors, seq, at)
	if err != nil {
		return nil, fmt.Errorf("read the nodes of %s under an issue above %s: %w", issueKey, cut, err)
	}
	defer rows.Close()
	var placed []TreeChild
	var child TreeChild
	var row treeNodeRow
	targets := append([]any{&child.Parent.Kind, &child.Parent.ID, &child.Node.Kind, &child.Node.ID}, row.targets()...)
	for rows.Next() {
		child, row = TreeChild{}, treeNodeRow{}
		if err := rows.Scan(targets...); err != nil {
			return nil, fmt.Errorf("read a node of %s under an issue above: %w", issueKey, err)
		}
		row.fill(&child)
		placed = append(placed, child)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the nodes of %s under an issue above %s: %w", issueKey, cut, err)
	}
	return placed, nil
}

// treeMembersStatement is, at the cut whose bounds $2 and $3 are, every block whose in_document
// names the document whose ref_key $1 is (documentMembers), by its address.
var treeMembersStatement = `
	select member.from_id
	from (` + documentMembers(`$1`, cutBound(`$2`, `$3`)) + `) member
	where member.asserted
	order by member.from_id`

// TreeMembers is every block of the document whose ref_key is refKey at cut: the nodes a render
// of that document takes (D6 of this branch's plan), by their address.
func TreeMembers(ctx context.Context, q Queryer, refKey string, cut TreeCut) ([]Target, error) {
	seq, at := cut.bounds()
	rows, err := q.Query(ctx, treeMembersStatement, refKey, seq, at)
	if err != nil {
		return nil, fmt.Errorf("read the blocks of %s %s: %w", refKey, cut, err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the blocks of %s %s: %w", refKey, cut, err)
	}
	members := make([]Target, len(ids))
	for index, id := range ids {
		members[index] = Target{Kind: BlockNodeKind, ID: id}
	}
	return members, nil
}

// treeFactFarEnd is what tells two tuples of one kind out of a node apart in the facts read
// (treeFactsStatement): the far end, its kind and its id, of a relation, and nothing of a value,
// whose newest tuple of its attribute is its value, compared byte for byte.
const treeFactFarEnd = `t.to_kind collate "C", (case when t.to_kind = '` + graphstore.KindValue + `' then '' else t.to_id end) collate "C"`

// treeFactsStatement is, at the cut whose bounds $3 and $4 are, every value and every relation out
// of each node $1 and $2 name, in one read of the tuples out of each node through
// graph_tuples_as_of_out: the newest tuple of each attribute's values and of each relation, its
// kind and its far end (treeFactFarEnd), kept while it asserts, a value marked as one. The read of
// one node groups its tuples byte for byte, which makes the groups the database's own collation
// makes at less cost, so the answer is ordered by the node's position and then by kind, far end
// kind and far end id in the database's collation: the order the relations of a node have always
// been answered in, which a read that keeps the last part_of in order relies on.
var treeFactsStatement = `
	select node.position, newest.to_kind = '` + graphstore.KindValue + `', newest.kind, newest.to_kind, newest.to_id
	from unnest($1::text[], $2::text[]) with ordinality as node (kind, id, position)
	cross join lateral (
		select distinct on (t.kind, ` + treeFactFarEnd + `) t.kind, t.to_kind, t.to_id, t.asserted
		from graph_tuples t
		where t.from_kind = node.kind and t.from_id = node.id and ` + cutBound(`$3`, `$4`) + `
		order by t.kind, ` + treeFactFarEnd + `, t.at desc, t.seq desc
	) newest
	where newest.asserted
	order by node.position, newest.kind, newest.to_kind, newest.to_id`

// TreeRelation is one relation out of a node: its kind and its far end.
type TreeRelation struct {
	Kind string
	To   Target
}

// TreeFacts is what a node states at a cut: each attribute's value, by its name, and each
// relation, by its kind and then its far end's kind and id, in the database's collation.
type TreeFacts struct {
	Values    map[string]string
	Relations []TreeRelation
}

// TreeFactsOf is what each of nodes states at cut, keyed by the node, read in one statement
// (treeFactsStatement): a node that states nothing has no entry. The statement is planned with its
// values on every call (QueryExecModeDescribeExec), never under the generic plan Postgres takes
// for a statement a connection has prepared from its sixth execution on: that plan, which knows
// neither the cut's seq nor the nodes, reads graph_tuples by its primary key over every seq at or
// below the cut once a node, and read the 2,009 nodes of the read-cost corpus's tree in 31.5 s
// against 144 ms for the plan made with the values, through graph_tuples_as_of_out.
func TreeFactsOf(ctx context.Context, q Queryer, nodes []Target, cut TreeCut) (map[Target]TreeFacts, error) {
	facts := make(map[Target]TreeFacts, len(nodes))
	if len(nodes) == 0 {
		return facts, nil
	}
	kinds, ids := targetColumns(nodes)
	seq, at := cut.bounds()
	rows, err := q.Query(ctx, treeFactsStatement, pgx.QueryExecModeDescribeExec, kinds, ids, seq, at)
	if err != nil {
		return nil, fmt.Errorf("read the values and relations of %d nodes %s: %w", len(nodes), cut, err)
	}
	defer rows.Close()
	for rows.Next() {
		var position int
		var valued bool
		var relation TreeRelation
		if err := rows.Scan(&position, &valued, &relation.Kind, &relation.To.Kind, &relation.To.ID); err != nil {
			return nil, fmt.Errorf("read a value or a relation %s: %w", cut, err)
		}
		node := nodes[position-1]
		held := facts[node]
		switch {
		case !valued:
			held.Relations = append(held.Relations, relation)
		case held.Values == nil:
			held.Values = map[string]string{relation.Kind: relation.To.ID}
		default:
			held.Values[relation.Kind] = relation.To.ID
		}
		facts[node] = held
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the values and relations of %d nodes %s: %w", len(nodes), cut, err)
	}
	return facts, nil
}

// treeNodesStatement is the row of each node $1 and $2 name that the store holds: its supertag and
// when it was made, read off graph_nodes' primary key.
var treeNodesStatement = `
	select node.position, held.supertag, held.made_at
	from unnest($1::text[], $2::text[]) with ordinality as node (kind, id, position)
	join graph_nodes held on held.kind = node.kind and held.id = node.id`

// TreeNodeRow is what a node's row holds that a walk reads: its supertag and when it was made.
type TreeNodeRow struct {
	Supertag string
	MadeAt   time.Time
}

// TreeNodeRows is the row of each of nodes the store holds, keyed by the node.
func TreeNodeRows(ctx context.Context, q Queryer, nodes []Target) (map[Target]TreeNodeRow, error) {
	held := make(map[Target]TreeNodeRow, len(nodes))
	if len(nodes) == 0 {
		return held, nil
	}
	kinds, ids := targetColumns(nodes)
	rows, err := q.Query(ctx, treeNodesStatement, kinds, ids)
	if err != nil {
		return nil, fmt.Errorf("read the rows of %d nodes: %w", len(nodes), err)
	}
	defer rows.Close()
	for rows.Next() {
		var position int
		var row TreeNodeRow
		if err := rows.Scan(&position, &row.Supertag, &row.MadeAt); err != nil {
			return nil, fmt.Errorf("read a node's row: %w", err)
		}
		held[nodes[position-1]] = row
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the rows of %d nodes: %w", len(nodes), err)
	}
	return held, nil
}

// treeMovedToStatement follows, at the cut whose bounds $2 and $3 are, the moved_to tuples from
// each block address of $1: from an address, the newest standing moved_to to a block or an ask,
// read through graph_tuples_as_of_out, and on from a block it reaches, at most $4 steps, as
// block_address_walk follows them now (migration 20261118165901); a step that ends at an ask ends
// the walk where it stands. It answers each address, the address its walk reaches, and the same
// block id in the room the reached address's document is edited in (block_room_address), null
// where its document has no room.
var treeMovedToStatement = `
	with recursive walk (start, address, hop) as (
		select start.address, start.address, 0 from unnest($1::text[]) as start (address)
		union all
		select walk.start, moved.to_id, walk.hop + 1
		from walk
		cross join lateral (
			select held.to_kind, held.to_id from (
				select distinct on (t.to_kind, t.to_id) t.to_kind, t.to_id, t.asserted, t.at, t.seq
				from graph_tuples t
				where t.from_kind = '` + BlockNodeKind + `' and t.from_id = walk.address and t.kind = '` + graphstore.TupleMovedTo + `'
				  and t.to_kind <> '` + graphstore.KindValue + `' and ` + cutBound(`$2`, `$3`) + `
				order by t.to_kind, t.to_id, t.at desc, t.seq desc
			) held
			where held.asserted and held.to_kind in ('` + BlockNodeKind + `', '` + graphstore.KindAsk + `')
			order by held.at desc, held.seq desc
			limit 1
		) moved
		where moved.to_kind = '` + BlockNodeKind + `' and walk.hop < $4
	)
	select distinct on (walk.start) walk.start, walk.address, block_room_address(walk.address)
	from walk
	order by walk.start, walk.hop desc`

// TreeMovedTo is, for each of addresses, the block addresses it resolves to at cut, in the order
// a resolve takes them: the same block id in the room of the document the address its moved_to
// tuples reach is in, where that document has a room, and then that reached address
// (graphstore.MovedTo's order, read as of the cut).
func TreeMovedTo(ctx context.Context, q Queryer, addresses []string, cut TreeCut) (map[string][]string, error) {
	reached := make(map[string][]string, len(addresses))
	if len(addresses) == 0 {
		return reached, nil
	}
	seq, at := cut.bounds()
	rows, err := q.Query(ctx, treeMovedToStatement, addresses, seq, at, graphstore.MovedToHops)
	if err != nil {
		return nil, fmt.Errorf("follow the moves of %d addresses %s: %w", len(addresses), cut, err)
	}
	defer rows.Close()
	for rows.Next() {
		var start, address string
		var room *string
		if err := rows.Scan(&start, &address, &room); err != nil {
			return nil, fmt.Errorf("follow the moves of an address %s: %w", cut, err)
		}
		if room != nil {
			reached[start] = append(reached[start], *room)
		}
		reached[start] = append(reached[start], address)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("follow the moves of %d addresses %s: %w", len(addresses), cut, err)
	}
	return reached, nil
}

// targetColumns is targets as the two arrays a read binds them as: their kinds and their ids.
func targetColumns(targets []Target) (kinds, ids []string) {
	kinds, ids = make([]string, len(targets)), make([]string, len(targets))
	for index, target := range targets {
		kinds[index], ids[index] = target.Kind, target.ID
	}
	return kinds, ids
}
