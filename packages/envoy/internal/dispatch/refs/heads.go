package refs

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// The heads of stored text that Dispatch sends a reader to draw with its references: a first
// line, an item's excerpt and a block's excerpt. Each is cut where no reference crosses the cut
// (text.HeadRefs), so a mention, an address, a key or a `#id` is never half of itself where it is
// drawn: `CORE-12` of `CORE-1234` would be a working link to another issue. heads_test.go holds
// every other cut of Dispatch text to the reason it is none of these.

// firstLineRunes bounds a first line, its ellipsis included: the bound contracts.OneLineSummary
// cuts a line to.
const firstLineRunes = 160

// excerptRunes bounds the head of an item's text shown beside an edge.
const excerptRunes = 240

// BlockExcerptRunes bounds the containing block a document mention's excerpt shows.
const BlockExcerptRunes = 480

// FirstLine is the line a person reads to tell one text from another: its first line that holds
// more than whitespace (contracts.FirstNonEmptyLine), cut to firstLineRunes runes with `…` as
// contracts.OneLineSummary cuts it, but where no reference crosses the cut; serverURL is the
// deployment whose dashboard URLs are references (text.HeadRefs). It is an Inbox row's subject
// and its reply's first line, and an id prefix candidate's first line.
func FirstLine(body, serverURL string) string {
	return FirstLineWithin(body, serverURL, firstLineRunes)
}

// FirstLineWithin is FirstLine cut to runes runes in place of firstLineRunes, for a surface whose
// own design gives its first line more room: a person's page shows 240 (api/person_nodes.go).
func FirstLineWithin(body, serverURL string, runes int) string {
	return text.HeadRefs(contracts.FirstNonEmptyLine(body), runes, "…", serverURL)
}

// itemExcerpt is the head of an item's text shown beside an edge (Edges): its first excerptRunes
// runes, cut where no reference crosses the cut.
func itemExcerpt(body, serverURL string) string {
	return text.HeadRefs(body, excerptRunes, "", serverURL)
}

// BlockExcerpt is a block's markdown as a mention it holds shows it and as the block's row stores
// it: its first BlockExcerptRunes runes, cut where no reference crosses the cut.
func BlockExcerpt(markdown, serverURL string) string {
	return text.HeadRefs(markdown, BlockExcerptRunes, "", serverURL)
}

// The head read of an issue's tree reads the issue's rows of the head projection
// (issue_head_nodes, graphstore.HeadRowsSQL) in one range of its primary key, where the issue is
// marked whole (issue_head_marks), and answers from them what the walk's reads answer of each node
// at the head: its place among its parent's children (TreeChild), its values and relations
// (TreeFacts), its row (TreeNodeRow), and for a document block what the block loader answers of
// it for the reader and its document_blocks row (BlockNodesAndRows). A read at a version walks the
// tuples.

// HeadRow is one row of the head projection: a node of the issue's tree as the store held it when
// a writer, or the backfill, last wrote the row.
type HeadRow struct {
	Node   Target
	Parent Target
	// Rank and Text are the node's rank and text values, nil where it holds none.
	Rank, Text *string
	// Stored says the store holds the node's graph_nodes row, whose supertag, state, visibility and
	// made_at follow.
	Stored                      bool
	Supertag, State, Visibility string
	MadeAt                      time.Time
	// Authors is the who of each authored_by the node states now, in seq order, nil where none.
	Authors []byte
	// FirstAuthors and FirstAt are, for a document block, the authors and the time of the version
	// that first held it where Dispatch knows the block was made there.
	FirstAuthors []byte
	FirstAt      *time.Time
	// Block is the document block's document_blocks row, its slug and band read off its document as
	// the head read runs; nil for any other node and for a block no row stands for.
	Block *BlockRow
	// IssueKey and Project are the block's document's issue and project.
	IssueKey *string
	// Seen says the reader may see the node, by its graph_nodes row's visibility and author, as the
	// block loader's predicate reads it.
	Seen  bool
	Facts TreeFacts
	// Item is what the row of a comment, or of a current block of one, holds of its conversation
	// row; nil for every other node.
	Item *HeadItem
	// stored is, for a row that holds an Item, the graph_nodes row's state and visibility as a
	// loader selects them (StoredRowOf), each nil where the store holds no row of the node.
	stored StoredNode
	// zeroMaker says the node's graph_nodes.made_by is the zero actor; treeBlock says the block's
	// document_blocks row belongs to an artifact of kind tree (readHeadDocuments).
	zeroMaker bool
	treeBlock bool
}

// HeadItem is what a head row holds of a comment's row (issue_head_nodes' item_ columns), as the
// comment loader reads it (loadCommentNodes), or of a current block of a comment, as the comment
// block loader and BlockTexts read it (itemBlockNodes): the comment's issue and author; the
// comment's body, or the block's text at its newest revision, nil where that revision holds none;
// when the comment was written and last edited, or the block made and changed; the comment it
// replies to (its thread's head), the comment it answers and its ask, which name its parent
// (commentReplyParent); the document its row names (artifact_id), and the document and version it
// is written on; of a block its changed revision (Version), its id, its excerpt and its position;
// and of both the comment's marks, when its author last edited it (EditedAt) and when and by whom
// it was deleted (DeletedAt, DeletedBy), as model.DeletedPostMark reads them.
type HeadItem struct {
	IssueKey                *string
	Author                  []byte
	Text                    *string
	MadeAt, ChangedAt       *time.Time
	ReplyTo, Answers, AskID *string
	ArtifactID, DocumentID  *string
	Version                 *int
	BlockID, Excerpt        *string
	Position                *int
	EditedAt, DeletedAt     *time.Time
	DeletedBy               []byte
}

// headRowsStatement is the rows of the issue $1 the head projection holds while a mark names the
// issue whole, each with whether the reader whose credential $2 binds may see it (the block
// loader's predicate, VisibleKindRow over the row's own graph_nodes columns), its stored state and
// visibility selected as a loader selects a node's (StoredRowOf, null where the store holds no row
// of it): the rows rows selects, read once and sent as they are read. The documents a document
// block's row names are read after them, by theirs (headDocumentsStatement), so no row is held
// aside to find them: holding aside the 3,000 rows of an issue of the largest plan cost the read a
// third of its time. It is built at each read, as the block loader's is (blockNodesStatement): the
// registry the predicate reads is filled by the package's init, after its variables.
//
// It sends each row only what a read answers of it. Of the prose of a role document
// (headProseSQL) it sends the row's place, its type, its state, its graph_nodes row's state and
// visibility and its document, and none of its text, its authors, its document_blocks row or its
// other values and relations. Of every other row it sends all of them, but the text sum and the
// changed version of a block only where a check may type it (headCheckSQL), and the authors of the
// version that first held a block only where the block states no authored_by of its own
// (HeadRow.BlockNode's fallback). It also sends a boolean for a block node's zero maker.
// Of a comment and of a current block of one it sends the conversation columns (headItemSQL).
//
// The test is a column of the rows, computed once a row in a subquery Postgres keeps (`offset
// 0`): as a lateral it was inlined into each column that reads it, and run fourteen times a row.
func headRowsStatement(rows string) string {
	whole := func(column string) string { return `case when not heads.prose then ` + column + ` end` }
	checked := func(column string) string {
		return `case when not heads.prose and ` + headCheckSQL + ` then ` + column + ` end`
	}
	state := `'` + graphstore.AttributeState + `'`
	return `
	with head_roles as (` + headRolesSQL + `)
	select heads.node_kind, heads.node_address, heads.parent_kind, heads.parent_address, heads.rank, ` + whole(`heads.text`) + `,
	       heads.made_at is not null, coalesce(heads.supertag, ''), ` + StoredRowOf("heads") + `,
	       heads.made_at, ` + whole(`heads.authors`) + `,
	       case when not heads.prose and heads.authors is null then heads.first_authors end, ` + whole(`heads.first_at`) + `,
	       heads.document_id::text, ` + whole(`heads.block_type`) + `, ` + checked(`heads.text_sha256`) + `, ` + checked(`heads.changed_version`) + `,
	       heads.made_at is null or ` + VisibleKindRow(BlockNodeKind, "heads", "$2") + `,
	       case when not heads.prose and heads.document_id is not null then ` + zeroActorSQL("heads.made_by") + ` else false end,
	       case when not heads.prose then heads.value_kinds when ` + state + ` = any(heads.value_kinds) then array[` + state + `] else '{}' end,
	       case when not heads.prose then heads.value_texts
	            when ` + state + ` = any(heads.value_kinds) then array[heads.value_texts[array_position(heads.value_kinds, ` + state + `)]]
	            else '{}' end,
	       case when not heads.prose then heads.relation_kinds else '{}' end,
	       case when not heads.prose then heads.relation_to_kinds else '{}' end,
	       case when not heads.prose then heads.relation_to_ids else '{}' end,
	       ` + headItemSQL + `
	from (select heads.*, coalesce(` + headProseSQL() + `, false) as prose from (` + rows + `) heads offset 0) heads
	where exists (select 1 from issue_head_marks mark where mark.issue_key = $1)`
}

// headItemSQL is a head row's conversation columns (issue_head_nodes' item_ columns) as one record,
// in the order headItemOf reads them, the author as its text, for the row of a comment and of a
// comment's block, and null for every other: seventeen columns null on each of the 3,000 other rows
// of an issue of the largest plan would be sent as 204 kB of null lengths. A record is sent in
// binary with its fields as Postgres keeps them: a jsonb array of the same columns cost the 409
// rows of an issue of the most comments 2 ms to build, a record a third of that.
const headItemSQL = `case when heads.node_kind in ('` + graphstore.KindComment + `', '` + graphstore.KindCommentBlock + `') then row(
	       heads.item_issue_key, heads.item_author::text, heads.item_text, heads.item_made_at, heads.item_changed_at,
	       heads.item_reply_to, heads.item_answers, heads.item_ask_id, heads.item_artifact_id::text, heads.item_document_id::text,
	       heads.item_version, heads.item_block_id, heads.item_excerpt, heads.item_position,
	       heads.item_edited_at, heads.item_deleted_at, heads.item_deleted_by::text) end`

// headItemOf is the item a head row's headItemSQL column holds, raw as Postgres sent it in binary,
// scanned by plan (a record's, typeMap.PlanScan of pgtype.CompositeFields); nil for a null one.
func headItemOf(plan pgtype.ScanPlan, raw []byte) (*HeadItem, error) {
	if raw == nil {
		return nil, nil
	}
	var item HeadItem
	var author, deletedBy *string
	fields := pgtype.CompositeFields{&item.IssueKey, &author, &item.Text, &item.MadeAt, &item.ChangedAt, &item.ReplyTo,
		&item.Answers, &item.AskID, &item.ArtifactID, &item.DocumentID, &item.Version, &item.BlockID, &item.Excerpt, &item.Position,
		&item.EditedAt, &item.DeletedAt, &deletedBy}
	if err := plan.Scan(raw, fields); err != nil {
		return nil, fmt.Errorf("decode the conversation columns of a head row: %w", err)
	}
	if author != nil {
		item.Author = []byte(*author)
	}
	if deletedBy != nil {
		item.DeletedBy = []byte(*deletedBy)
	}
	return &item, nil
}

// headProseSQL is the test of a head row that a read answers nothing of but its place (R2, R16):
// a block whose document_blocks row names a role document of the issue $1 - its spec, its
// Verification document, its plan (head_roles) - whose supertag names no typed block the installed
// schema declares, and whose text holds no `:::`, so its opening line opens no typed block
// (TreeNodeType): its prose, which no query of the tree answers (the tree read's excluded, which
// reads the band of the block's document). A read takes its place among its parent's children,
// its type, which is its supertag, and its state, which a gone block is left out by. Of the 3,013
// rows of an issue of the largest plan, half are such prose, and their texts, authors, values and
// relations were half of the 2.0 MB its read was sent. It is built at each read: the schema can be
// installed again.
func headProseSQL() string {
	names := pmdoc.TypedBlockNames()
	typed := make([]string, len(names))
	for index, name := range names {
		typed[index] = `'` + strings.ReplaceAll(name, `'`, `''`) + `'`
	}
	notTyped := `true`
	if len(typed) > 0 {
		notTyped = `coalesce(heads.supertag, '') not in (` + strings.Join(typed, ", ") + `)`
	}
	return `heads.node_kind = '` + BlockNodeKind + `'
		and heads.document_id = any((select head_roles.ids from head_roles)::uuid[])
		and ` + notTyped + `
		and position(':::' in coalesce(heads.text, '')) = 0`
}

// headRolesSQL is the role documents of the issue $1 headProseSQL reads, by their id: the documents
// whose band (DocumentBandSQL) is the spec's, the Verification document's or the plan's.
var headRolesSQL = `select coalesce(array_agg(a.id), '{}') as ids
		from artifacts a
		where a.issue_key = $1 and ` + DocumentBandSQL("a") + ` in ('` +
	string(RankBandSpec) + `', '` + string(RankBandVerification) + `', '` + string(RankBandPlan) + `')`

// headCheckSQL is the test of a head row a check may type, whose text sum and changed version a
// read answers (a check's): its supertag is check, or its text opens a check somewhere. Every other
// block's 64-byte sum goes unread.
const headCheckSQL = `(heads.supertag = '` + graphstore.BlockTypeCheck + `' or position(':::` + graphstore.BlockTypeCheck + `' in coalesce(heads.text, '')) > 0)`

// headDocumentsStatement is, of each document $1 names, its slug, its issue and its band, and what a
// conversation item's node reads of its document (documentColumns: its project, its session, its
// kind and whether it is primary), plus whether its blocks are in a tree artifact. It also reads
// the project of each issue $2 names. The documents and issues come from the head rows
// (HeadItem), and the issues those items belong to, each read by its key once. The two were two
// statements, the second sent by the read of the comment page (ConversationPageNodesOfHeads).
var headDocumentsStatement = `
	select 'artifact', a.id::text, a.slug, a.issue_key, ` + DocumentBandSQL("a") + `, a.project_key, a.session_id, a.kind, a.is_primary,
	       a.kind = 'tree' or exists (select 1 from document_rooms r where r.artifact_id = a.id)
	from artifacts a
	where a.id = any($1::uuid[])
	union all
	select 'issue', i.key, null, null, null, i.project_key, null, null, null, false
	from issues i
	where i.key = any($2::text[])`

// headRowsWhole is every row of the issue $1, one range of issue_head_nodes' primary key: what a
// view reads.
const headRowsWhole = `select * from issue_head_nodes whole where whole.issue_key = $1`

// headRowsSkeleton is the rows of the issue $1's skeleton (HeadSkeleton), each a range of
// issue_head_nodes' primary key, which sorts byte by byte: every row of a kind other than a
// block, a comment and a comment's block; the blocks whose address $3 and $4 bound, a pair for each
// document of the skeleton and one for the addresses below the issue's own, and the blocks whose
// address is $6 or above; and the comments $5 names, with their blocks. Each pair of bounds, and
// each comment of the page, is a range of its own read through the key (a lateral subquery Postgres
// keeps, `offset 0`): as a join, Postgres reckons each pair to keep a ninth of the issue's blocks,
// and over an issue of 46,000 blocks it read every row of the table rather than the 1,460 the
// pairs bound. The blocks at $6 or above are bounded by a subquery of $6, which Postgres reads as
// a value it does not know when it plans: given $6 itself it reckoned, off the addresses of the
// 9,252 comment blocks of an issue of the most comments, which all sort above it, that the range
// kept 11,875 of its 6 blocks, and read the whole table (45 ms of a 51 ms statement).
var headRowsSkeleton = `
		select * from issue_head_nodes kinds where kinds.issue_key = $1 and kinds.node_kind < '` + BlockNodeKind + `'
		union all
		select * from issue_head_nodes kinds where kinds.issue_key = $1 and kinds.node_kind > '` + BlockNodeKind + `' and kinds.node_kind < '` + string(ItemComment) + `'
		union all
		select * from issue_head_nodes kinds where kinds.issue_key = $1 and kinds.node_kind > '` + ItemComment.BlockKind() + `'
		union all
		select blocks.* from unnest($3::text[], $4::text[]) bounds(low, high)
		cross join lateral (
			select * from issue_head_nodes blocks where blocks.issue_key = $1 and blocks.node_kind = '` + BlockNodeKind + `'
			  and blocks.node_address >= bounds.low and blocks.node_address < bounds.high
			offset 0) blocks
		union all
		select * from issue_head_nodes blocks where blocks.issue_key = $1 and blocks.node_kind = '` + BlockNodeKind + `' and blocks.node_address >= (select $6::text)
		union all
		select comments.* from issue_head_nodes comments
		where comments.issue_key = $1 and comments.node_kind = '` + string(ItemComment) + `' and comments.node_address = any($5::text[])
		union all
		select blocks.* from unnest($5::text[]) page(id)
		cross join lateral (
			select * from issue_head_nodes blocks where blocks.issue_key = $1 and blocks.node_kind = '` + ItemComment.BlockKind() + `'
			  and blocks.node_address >= '` + string(ItemComment) + `/' || page.id || '#' and blocks.node_address < '` + string(ItemComment) + `/' || page.id || '$'
			offset 0) blocks`

// HeadSkeleton is the part of an issue's rows the read with no view reads. Of the blocks whose
// address begins with the issue's key and `/`, the blocks of the issue's own documents, it reads
// the blocks of Documents alone, each a document's ref_key; it reads every other block, a finding
// of a lens (graphstore.FindingID) and a block of another issue's document placed under a node of
// the issue among them. Of the comments it reads those of the comment page, Comments, alone, each
// with its blocks. It reads every row of every other kind.
type HeadSkeleton struct {
	Documents []string
	Comments  []string
}

// bounds is the low and the high end of each range of addresses of the blocks the skeleton reads
// below the high end of its last range, above, which begins at the lowest address past the
// issue's own: the addresses below `<key>/`, and `<ref_key>#` up to `<ref_key>$` for each of
// Documents; every high end is its range's prefix with its last byte one higher, past which no
// address that begins with the prefix sorts.
func (skeleton HeadSkeleton) bounds(issueKey string) (low, high []string, above string) {
	low, high = make([]string, 0, len(skeleton.Documents)+1), make([]string, 0, len(skeleton.Documents)+1)
	low, high = append(low, ""), append(high, issueKey+"/")
	for _, document := range skeleton.Documents {
		low, high = append(low, document+"#"), append(high, document+"$")
	}
	return low, high, issueKey + "0"
}

// HeadTree is the rows of one issue's tree the head projection holds, keyed by the node, and the
// children each parent holds among them, as a children read answers them (Subtrees).
type HeadTree struct {
	Rows     map[Target]*HeadRow
	children map[Target][]TreeChild
	// owners is, by its kind and its key, each document and each issue the conversation items of
	// the rows name (HeadItem), as a comment's or a comment block's node reads them
	// (headDocumentsStatement).
	owners map[[2]string]headItemOwner
}

// HeadRows is the rows of the issue issueKey's tree the head projection holds, read for who, and
// whether the issue is marked whole: an issue no mark names answers nil and false, and its read
// walks the tuples. Every marked issue holds at least its own node's row. skeleton, where it is
// set, reads the skeleton's rows alone (HeadSkeleton); nil reads every row.
func HeadRows(ctx context.Context, q Queryer, who model.Actor, issueKey string, skeleton *HeadSkeleton) (*HeadTree, bool, error) {
	statement, args := headRowsStatement(headRowsWhole), []any{issueKey, VisibilityArg(who)}
	if skeleton != nil {
		low, high, above := skeleton.bounds(issueKey)
		comments := skeleton.Comments
		if comments == nil {
			comments = []string{}
		}
		statement, args = headRowsStatement(headRowsSkeleton), append(args, low, high, comments, above)
	}
	rows, err := q.Query(ctx, statement, append([]any{headArraysAsText}, args...)...)
	if err != nil {
		return nil, false, fmt.Errorf("read the head rows of %s: %w", issueKey, err)
	}
	fields, held, err := drainHeadRows(rows)
	if err != nil {
		return nil, false, fmt.Errorf("read the head rows of %s: %w", issueKey, err)
	}
	if len(held) == 0 {
		return nil, false, nil
	}
	typeMap := pgtype.NewMap()
	if conn := rows.Conn(); conn != nil {
		typeMap = conn.TypeMap()
	}
	decode := headTextsText
	if fields[headFirstArray].Format == pgx.BinaryFormatCode {
		decode = headTexts
	}
	read := make([]HeadRow, 0, len(held))
	var blocks []headBlock
	var documents, issues []string
	named := map[string]bool{}
	var plans []pgtype.ScanPlan
	var itemPlan pgtype.ScanPlan
	itemColumn := headFirstArray + 5
	for _, raw := range held {
		var head HeadRow
		var parentKind, parentID, document, blockType, textSHA256, state, visibility pgtype.Text
		var madeAt *time.Time
		var changedVersion pgtype.Int4
		var valueKinds, valueTexts, relationKinds, relationToKinds, relationToIDs []string
		destinations := []any{&head.Node.Kind, &head.Node.ID, &parentKind, &parentID, &head.Rank, &head.Text,
			&head.Stored, &head.Supertag, &state, &visibility, &madeAt, &head.Authors, &head.FirstAuthors, &head.FirstAt,
			&document, &blockType, &textSHA256, &changedVersion, &head.Seen, &head.zeroMaker}
		if plans == nil {
			plans = make([]pgtype.ScanPlan, len(destinations))
			for index, destination := range destinations {
				plans[index] = typeMap.PlanScan(fields[index].DataTypeOID, fields[index].Format, destination)
			}
		}
		for index, destination := range destinations {
			if err := plans[index].Scan(raw[index], destination); err != nil {
				return nil, false, fmt.Errorf("read the column %s of a head row of %s: %w", fields[index].Name, issueKey, err)
			}
		}
		for index, array := range []*[]string{&valueKinds, &valueTexts, &relationKinds, &relationToKinds, &relationToIDs} {
			if *array, err = decode(raw[headFirstArray+index]); err != nil {
				return nil, false, fmt.Errorf("read a head row of %s: %w", issueKey, err)
			}
		}
		if itemPlan == nil {
			itemPlan = typeMap.PlanScan(fields[itemColumn].DataTypeOID, fields[itemColumn].Format, pgtype.CompositeFields{})
		}
		if head.Item, err = headItemOf(itemPlan, raw[itemColumn]); err != nil {
			return nil, false, fmt.Errorf("read the column %s of a head row of %s: %w", fields[itemColumn].Name, issueKey, err)
		}
		head.State, head.Visibility = state.String, visibility.String
		if head.Item != nil {
			if state.Valid {
				held := state.String
				head.stored.State = &held
			}
			if visibility.Valid {
				held := visibility.String
				head.stored.Visibility = &held
			}
		}
		if parentKind.Valid && parentID.Valid {
			head.Parent = Target{Kind: parentKind.String, ID: parentID.String}
		}
		if madeAt != nil {
			head.MadeAt = *madeAt
		}
		if document.Valid {
			_, blockID, _ := strings.Cut(head.Node.ID, "#")
			blocks = append(blocks, headBlock{read: len(read), row: BlockRow{BlockID: blockID, Document: document.String,
				Type: blockType.String, TextSHA256: textSHA256.String, ChangedVersion: int(changedVersion.Int32)}})
			if !named[document.String] {
				named[document.String] = true
				documents = append(documents, document.String)
			}
		}
		if item := head.Item; item != nil {
			if item.IssueKey != nil && !named["issue "+*item.IssueKey] {
				named["issue "+*item.IssueKey] = true
				issues = append(issues, *item.IssueKey)
			}
			for _, document := range []*string{item.ArtifactID, item.DocumentID} {
				if document != nil && !named[*document] {
					named[*document] = true
					documents = append(documents, *document)
				}
			}
		}
		head.Facts = headFacts(head.Rank, head.Text, valueKinds, valueTexts, relationKinds, relationToKinds, relationToIDs)
		read = append(read, head)
	}
	owners, err := readHeadDocuments(ctx, q, issueKey, read, blocks, documents, issues)
	if err != nil {
		return nil, false, err
	}
	heads := make(map[Target]*HeadRow, len(read))
	for index := range read {
		heads[read[index].Node] = &read[index]
	}
	tree := newHeadTree(heads, read)
	tree.owners = owners
	return tree, true, nil
}

// drainHeadRows reads every row of the head rows statement off the connection as it arrives, each
// row's values copied into one allocation of its own, and closes the statement: its fields, and
// its rows' values, nil for a null. It decodes nothing, so Postgres sends the rows as fast as it
// writes them and the statement ends there; HeadRows decodes them after. Decoding each row as it
// arrived held the statement open while the client scanned it: of the 26 ms Postgres counted for
// the 3,013 rows of an issue of the largest plan, it spent 14 writing them and the rest waiting
// on the client to read the next.
func drainHeadRows(rows pgx.Rows) ([]pgconn.FieldDescription, [][][]byte, error) {
	defer rows.Close()
	var fields []pgconn.FieldDescription
	var held [][][]byte
	for rows.Next() {
		values := rows.RawValues()
		if fields == nil {
			fields = slices.Clone(rows.FieldDescriptions())
		}
		size := 0
		for _, value := range values {
			size += len(value)
		}
		bytes := make([]byte, size)
		row := make([][]byte, len(values))
		at := 0
		for index, value := range values {
			if value == nil {
				continue
			}
			row[index] = bytes[at : at+len(value) : at+len(value)]
			copy(row[index], value)
			at += len(value)
		}
		held = append(held, row)
	}
	rows.Close()
	return fields, held, rows.Err()
}

// headFirstArray is the column of headRowsStatement's first text[], value_kinds, which the four
// others follow: value_texts, relation_kinds, relation_to_kinds and relation_to_ids.
const headFirstArray = 20

// headArraysAsText asks Postgres for the head rows' text arrays in its text form, and every column
// of another type that a client reads apart from its text - timestamps, booleans, integers, jsonb
// and the conversation record (headItemSQL), which can be read in binary alone - in binary; pgx asks
// for every type the map does not name in text, and a text column is the same bytes either way. A
// text[] in binary carries a 20-byte header and a 4-byte length for each element, so the five
// arrays of the 3,013 rows of an issue of the largest plan are 1,114 kB in binary and 655 kB as
// text, of the 2,472 kB the rows are sent in, at about the same cost to Postgres to write: sent in
// binary with the rest, they cost that read 1.1 ms of database time. The other columns in binary
// spare Postgres formatting them, about 1 ms of that read's statement and 0.5 ms of the 409 rows of
// an issue of the most comments.
var headArraysAsText = pgx.QueryResultFormatsByOID{
	pgtype.TextArrayOID:   pgx.TextFormatCode,
	pgtype.TimestamptzOID: pgx.BinaryFormatCode,
	pgtype.BoolOID:        pgx.BinaryFormatCode,
	pgtype.Int4OID:        pgx.BinaryFormatCode,
	pgtype.JSONBOID:       pgx.BinaryFormatCode,
	pgtype.RecordOID:      pgx.BinaryFormatCode,
}

// headTextsText is a text[] of a head row in Postgres's text form (array_out): `{a,b,…}`, an
// element quoted, with `"` and `\` escaped by a backslash, where it is empty, is NULL in any case,
// or holds a brace, a comma, a quote, a backslash or white space, and a lower bound other than one
// written before it as `[lower:upper]=`. The unquoted elements are cut out of one string of the
// whole value. An empty array is the empty slice; a null one, or an unquoted NULL element, which
// no head row holds, is an error.
func headTextsText(raw []byte) ([]string, error) {
	if raw == nil {
		return nil, errors.New("a text array of a head row is null")
	}
	whole := string(raw)
	if strings.HasPrefix(whole, "[") {
		_, after, cut := strings.Cut(whole, "=")
		if !cut {
			return nil, fmt.Errorf("a text array of a head row reads %q, with no = after its bounds", whole)
		}
		whole = after
	}
	if len(whole) < 2 || whole[0] != '{' || whole[len(whole)-1] != '}' {
		return nil, fmt.Errorf("a text array of a head row reads %q, not one {…}", whole)
	}
	body := whole[1 : len(whole)-1]
	texts := make([]string, 0, strings.Count(body, ",")+1)
	if body == "" {
		return texts, nil
	}
	for at := 0; ; {
		var text string
		if body[at] == '"' {
			var element strings.Builder
			at++
			for ; at < len(body) && body[at] != '"'; at++ {
				if body[at] == '\\' && at+1 < len(body) {
					at++
				}
				element.WriteByte(body[at])
			}
			if at >= len(body) {
				return nil, fmt.Errorf("a text array of a head row reads %q, with a quote it does not close", whole)
			}
			at++
			text = element.String()
		} else {
			end := strings.IndexByte(body[at:], ',')
			if end < 0 {
				end = len(body) - at
			}
			text = body[at : at+end]
			if strings.EqualFold(text, "NULL") {
				return nil, errors.New("a text array of a head row holds a null element")
			}
			at += end
		}
		texts = append(texts, text)
		if at == len(body) {
			return texts, nil
		}
		if body[at] != ',' {
			return nil, fmt.Errorf("a text array of a head row reads %q, with %q after an element", whole, body[at])
		}
		at++
	}
}

// headTexts is a text[] of a head row as Postgres sends it in binary, where a caller's connection
// asks for it so: its dimensions, whether it holds a null and its element type, the length and
// lower bound of its one dimension, and each element's length and bytes. The elements are cut out
// of one string of the whole value, so the array costs two allocations, where a scan through its
// codec costs one an element and more: the five arrays of each of 3,000 rows are what a client
// spends most on while Postgres waits to send the next rows. An empty array is the empty slice; a
// null one, or a null element, which no head row holds, is an error.
func headTexts(raw []byte) ([]string, error) {
	if raw == nil {
		return nil, errors.New("a text array of a head row is null")
	}
	if len(raw) < 12 {
		return nil, fmt.Errorf("a text array of a head row is %d bytes, short of its header", len(raw))
	}
	dimensions := int32(binary.BigEndian.Uint32(raw))
	if dimensions == 0 {
		return []string{}, nil
	}
	if dimensions != 1 || len(raw) < 20 {
		return nil, fmt.Errorf("a text array of a head row has %d dimensions in %d bytes, not one", dimensions, len(raw))
	}
	count := int32(binary.BigEndian.Uint32(raw[12:]))
	if count < 0 {
		return nil, fmt.Errorf("a text array of a head row holds %d elements", count)
	}
	whole := string(raw)
	texts := make([]string, count)
	at := 20
	for index := range texts {
		if at+4 > len(whole) {
			return nil, errors.New("a text array of a head row ends inside an element's length")
		}
		length := int(int32(binary.BigEndian.Uint32(raw[at:])))
		at += 4
		if length < 0 {
			return nil, errors.New("a text array of a head row holds a null element")
		}
		if at+length > len(whole) {
			return nil, errors.New("a text array of a head row ends inside an element")
		}
		texts[index] = whole[at : at+length]
		at += length
	}
	return texts, nil
}

// headBlock is the document_blocks row a head row of a document block holds, read, the row's index
// among the rows read, until its document's slug and band are read (readHeadDocuments).
type headBlock struct {
	read int
	row  BlockRow
}

// readHeadDocuments reads each of documents and of issues, the documents and the issues the head
// rows read name (headDocumentsStatement), gives each row of blocks its document's issue, and its
// document_blocks row with the document's slug and band, where the document stands - a row whose
// document no artifact stands for holds neither - and answers each document and each issue that
// stands as a conversation item's node reads it (headItemOwner), by its kind and its key.
func readHeadDocuments(ctx context.Context, q Queryer, issueKey string, read []HeadRow, blocks []headBlock, documents, issues []string) (map[[2]string]headItemOwner, error) {
	owners := map[[2]string]headItemOwner{}
	if len(documents) == 0 && len(issues) == 0 {
		return owners, nil
	}
	rows, err := q.Query(ctx, headDocumentsStatement, documents, issues)
	if err != nil {
		return nil, fmt.Errorf("read the documents of the head rows of %s: %w", issueKey, err)
	}
	bands := make(map[string]string, len(documents))
	inTree := make(map[string]bool, len(documents))
	for rows.Next() {
		var kind, id string
		var band *string
		var owner headItemOwner
		var treeDoc bool
		if err := rows.Scan(&kind, &id, &owner.slug, &owner.issueKey, &band, &owner.project, &owner.session, &owner.kind, &owner.primary, &treeDoc); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read a document of the head rows of %s: %w", issueKey, err)
		}
		owners[[2]string{kind, id}] = owner
		if band != nil {
			bands[id] = *band
		}
		if treeDoc {
			inTree[id] = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the documents of the head rows of %s: %w", issueKey, err)
	}
	for _, block := range blocks {
		document, held := owners[[2]string{"artifact", block.row.Document}]
		if !held {
			continue
		}
		head := &read[block.read]
		head.IssueKey = document.issueKey
		row := block.row
		if document.slug != nil {
			row.Slug = *document.slug
		}
		if band := bands[block.row.Document]; band != "" {
			row.Band = band[0]
		}
		head.Block = &row
		head.treeBlock = inTree[block.row.Document]
	}
	return owners, nil
}

// newHeadTree is rows, read, with the children of each parent among them, as the children read
// answers them under it (treeChildJoins, treeChildStands, treeChildOrder): each node whose
// standing part_of names the parent, unless its kind is one of workflowKinds or its state is gone,
// in the tree's order of siblings (CompareSiblings).
func newHeadTree(rows map[Target]*HeadRow, read []HeadRow) *HeadTree {
	tree := &HeadTree{Rows: rows, children: map[Target][]TreeChild{}}
	for index := range read {
		head := &read[index]
		if head.Parent == (Target{}) || slices.Contains(workflowKinds, head.Node.Kind) || head.Gone() {
			continue
		}
		tree.children[head.Parent] = append(tree.children[head.Parent], head.Child())
	}
	for _, children := range tree.children {
		slices.SortFunc(children, CompareSiblings)
	}
	return tree
}

// Subtrees is what TreeSubtrees answers of the tree at the head, read from its rows: the children
// of each of parents, and of each node under them the walk of the tree of the issue owner enters
// (TreeOwns), down to the leaves, keyed by the parent, each parent's in the children read's order.
// A child the walk does not enter - an issue's node, a node of another issue - is answered under
// its parent, and nothing under it is read.
func (tree *HeadTree) Subtrees(parents []Target, owner string) map[Target][]TreeChild {
	under := map[Target][]TreeChild{}
	read := map[Target]bool{}
	for len(parents) > 0 {
		var next []Target
		for _, parent := range parents {
			if read[parent] {
				continue
			}
			read[parent] = true
			children := tree.children[parent]
			if len(children) == 0 {
				continue
			}
			under[parent] = children
			for _, child := range children {
				if TreeOwns(owner, child.Node) {
					next = append(next, child.Node)
				}
			}
		}
		parents = next
	}
	return under
}

// Under is the children of each parent the rows hold children of that match holds, each parent's
// in the children read's order: what a walk reads of a parent it takes no node to reach, as a walk
// of an issue's skeleton reads the blocks of a document it does not enter.
func (tree *HeadTree) Under(match func(Target) bool) map[Target][]TreeChild {
	under := map[Target][]TreeChild{}
	for parent, children := range tree.children {
		if match(parent) {
			under[parent] = children
		}
	}
	return under
}

// headFacts is the values and relations a head row holds, as TreeFactsOf answers them: every value
// by its attribute, the rank and the text among them, and every relation in the row's order, which
// is the facts read's. A node that states nothing answers the zero TreeFacts.
func headFacts(rank, text *string, valueKinds, valueTexts, relationKinds, relationToKinds, relationToIDs []string) TreeFacts {
	var facts TreeFacts
	if rank != nil || text != nil || len(valueKinds) > 0 {
		facts.Values = make(map[string]string, len(valueKinds)+2)
		for index, kind := range valueKinds {
			facts.Values[kind] = valueTexts[index]
		}
		if rank != nil {
			facts.Values[AttributeRank] = *rank
		}
		if text != nil {
			facts.Values[graphstore.AttributeText] = *text
		}
	}
	if len(relationKinds) > 0 {
		facts.Relations = make([]TreeRelation, len(relationKinds))
		for index, kind := range relationKinds {
			facts.Relations[index] = TreeRelation{Kind: kind, To: Target{Kind: relationToKinds[index], ID: relationToIDs[index]}}
		}
	}
	return facts
}

// Child is the node as a children read answers it under its parent (TreeChild): its rank, its
// supertag and when it was made off its row, and its type off its supertag and the opening line of
// its text (TreeNodeType).
func (head *HeadRow) Child() TreeChild {
	child := TreeChild{Parent: head.Parent, Node: head.Node, Supertag: head.Supertag, MadeAt: head.MadeAt}
	if head.Rank != nil {
		child.Rank, child.Ranked = *head.Rank, true
	}
	opening := ""
	if head.Text != nil {
		opening, _, _ = strings.Cut(*head.Text, "\n")
	}
	child.Type = TreeNodeType(head.Node.Kind, head.Supertag, opening)
	return child
}

// Row is the node's row as TreeNodeRows answers it: the zero row where the store holds none.
func (head *HeadRow) Row() TreeNodeRow {
	if !head.Stored {
		return TreeNodeRow{}
	}
	return TreeNodeRow{Supertag: head.Supertag, MadeAt: head.MadeAt}
}

// Gone says the node's state value is gone, which a children read keeps out (treeChildStands).
func (head *HeadRow) Gone() bool {
	return head.Facts.Values[graphstore.AttributeState] == graphstore.StateGone
}

// BlockNode is the document block as BlockNodesAndRows answers it for the reader the head read was
// read for: the node the block loader builds, with the members a tree read answers of it - its
// author, when it was made, its stored state and visibility, its document's issue - and whether
// the reader may see it; ok is false for a node that is no document block with a row.
func (head *HeadRow) BlockNode() (node ExcerptedNode, seen, ok bool, err error) {
	if head.Block == nil || head.Node.Kind != BlockNodeKind {
		return ExcerptedNode{}, false, false, nil
	}
	built := model.GraphNode{Kind: BlockNodeKind, ID: head.Node.ID, BlockID: head.Block.BlockID, IssueKey: head.IssueKey,
		ChangedVersion: head.Block.ChangedVersion}
	first := blockFirst{authors: head.FirstAuthors, createdAt: head.FirstAt}
	known := head.FirstAt != nil || head.FirstAuthors != nil
	first.known = &known
	var nodeMadeAt *time.Time
	if head.Stored {
		madeAt := head.MadeAt
		nodeMadeAt = &madeAt
	}
	if built.MadeBy, built.MadeAt, err = blockMaker(head.treeBlock, nodeMadeAt, head.zeroMaker, head.Authors, first); err != nil {
		return ExcerptedNode{}, false, false, fmt.Errorf("decode the maker of block %s: %w", head.Node.ID, err)
	}
	stored := StoredNode{}
	if head.Stored {
		stored = StoredNode{State: &head.State, Visibility: &head.Visibility}
	}
	setStored(&built, stored)
	return ExcerptedNode{Node: built}, head.Seen, true, nil
}
