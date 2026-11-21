package asof

import "strings"

// shadow is the definition of one table at a cut. Definition gives a statement that answers
// every column of Table, by name and type, as the table stood at the cut whose timestamptz
// literal it is given. Inside a definition every base table has its schema (public.events), so
// no shadow reads another shadow of the same WITH list by its name.
type shadow struct {
	Table      string
	Definition func(cut string) string
}

// shadows are the shadowed tables, in the order Shadows writes them. Each one is the plan's
// decision 2 for its table. No definition reads another shadow: each reads public.<table>, and
// what it needs of another table at the cut it reads from public.events itself. It is an array,
// so that Bind marks the tables a statement names in an array of its length, with no allocation.
var shadows = [...]shadow{
	// An event exists at the cut when the transaction that wrote it began at or before the cut:
	// created_at is that transaction's start. The index events_created_at (created_at, id) holds
	// the events at the cut in its first key.
	{Table: "events", Definition: createdByTheCut("events")},
	{Table: "issues", Definition: issuesShadow},
	{Table: "issue_links", Definition: issueLinksShadow},
	{Table: "issue_external_links", Definition: issueExternalLinksShadow},
	{Table: "issue_components", Definition: issueComponentsShadow},
	{Table: "priority_tuples", Definition: priorityTuplesShadow},
	{Table: "projects", Definition: projectsShadow},
	{Table: "repo_projects", Definition: repoProjectsShadow},
	{Table: "asks", Definition: asksShadow},
	{Table: "ask_followers", Definition: askFollowersShadow},
	{Table: "comments", Definition: commentsShadow},
	{Table: "messages", Definition: messagesShadow},
	{Table: "comment_deliveries", Definition: commentDeliveriesShadow},
	{Table: "message_deliveries", Definition: messageDeliveriesShadow},
	{Table: "artifacts", Definition: artifactsShadow},
	{Table: "artifact_versions", Definition: createdByTheCut("artifact_versions")},
	{Table: "artifact_reviews", Definition: createdByTheCut("artifact_reviews")},
	{Table: "check_results", Definition: createdByTheCut("check_results")},
	{Table: "check_result_retractions", Definition: createdByTheCut("check_result_retractions")},
}

// ShadowedTables are the names of the shadowed tables, in the order Shadows writes them.
func ShadowedTables() []string {
	tables := make([]string, len(shadows))
	for index := range shadows {
		tables[index] = shadows[index].Table
	}
	return tables
}

// createdByTheCut is the shadow of an append-only table: each row whose insert began at or before
// the cut, as the row stands. created_at is the inserting transaction's now(), the clock events
// keep. Every column of the table is the row's own, so a column added later is answered with no
// change here. A writer changes such a row after its insert only where the row as it stands is
// right at the cut too:
//   - artifact_reviews.event_seq: the transaction of the review's insert writes it (docs.QueueReview).
//   - artifact_reviews.content_sha256: the boot backfill (docs.FillReviewContentHashes) writes the
//     hash of the text of the reviewed version, which no write changes, so the hash is the one
//     that text had at the cut.
//   - artifact_versions.accepted_from: the transaction that inserts the version writes it, the
//     accept of a suggestion (api/comment_actions.go).
//   - artifact_versions.summary and renamed_by_delete: the delete of a comment renames the version
//     that its accepted suggestion named (renameAcceptedVersion, api/comment_delete.go). A post
//     deleted after the cut reads at the cut as deleted, with no part of its body (decision 14),
//     so the version reads renamed at the cut too, as the present read gives it.
func createdByTheCut(table string) func(cut string) string {
	return func(cut string) string {
		return "select * from public." + table + " where created_at <= " + cut
	}
}

// sqlList is values as a SQL list of string literals, such as ('a', 'b'). Each value is a
// constant of this package, never the text of a request.
func sqlList(values []string) string {
	quoted := make([]string, len(values))
	for index, value := range values {
		quoted[index] = "'" + strings.ReplaceAll(value, "'", "''") + "'"
	}
	return "(" + strings.Join(quoted, ", ") + ")"
}

// jsonArray is the JSON value expr when it is an array, else an empty array: a payload of an
// older shape that lacks the key, or holds null, reads as no element.
func jsonArray(expr string) string {
	return "(case jsonb_typeof(" + expr + ") when 'array' then " + expr + " else '[]'::jsonb end)"
}

// textArray is the JSON array expr holds as text[], '{}' when it holds none.
func textArray(expr string) string {
	return "array(select jsonb_array_elements_text(" + jsonArray(expr) + "))"
}

// jsonValue is the JSON value expr as SQL null when the key is absent or holds JSON null, which
// is what the column holds for a field the writer stored as null.
func jsonValue(expr string) string {
	return "nullif(" + expr + ", 'null'::jsonb)"
}

// ownedBy is true of an event row whose owner is the issue or the document that owns row: an
// event of a comment names its owner as the comment does (one of issue_key and artifact_id,
// events_one_owner). It lets a per-row read of a comment's events that no index of events keys by
// the comment take the owner's index: its delivery receipts, which name the comment as
// comment_id. A read of an ask's, a comment's or a message's own events reads them by the id in
// their payload alone, through the partial index of its types (events_ask_state,
// events_comment_state, events_message_state, events_ask_follower): the id is the item's, and
// with the owner beside it the planner reads every event of the owner to intersect them with the
// few events of the item.
func ownedBy(event, row string) string {
	return "(" + event + ".issue_key = " + row + ".issue_key or " + event + ".artifact_id = " + row + ".artifact_id)"
}

// postHistoryMigration is INFRA-1523's migration 20261110224508. From the time schema_migrations
// records for it, every edit and delete of a comment or a message writes a post_body_history row
// in its own transaction.
const postHistoryMigration = "20261110224508"

// PostHistoryBegan is the time post_body_history began, as a SQL expression. The keyword table's
// fill reads the same time (textversions), so the two pair a post's history rows with its edits
// alike.
const PostHistoryBegan = `(select m.applied_at from public.schema_migrations m where m.version = ` + postHistoryMigration + `)`

// historyEventsCount is the number of the edit and delete events (types, a SQL list) of the post
// whose row the alias post names that began at or after post_body_history began and that bound
// holds (a further condition on e, or empty), read from events (public.events, or events, the
// events shadow under a cut) by the post's id in their payload (events_comment_state,
// events_message_state). The transaction of each such event wrote one history row, and the rows
// of a post in edited_at, id order are the rows of those events in their order. An edit's
// transaction begins before it takes the post's lock, so a row's edited_at does not pair it with
// its event: two edits can overlap, and the first then writes its row after the second began.
func historyEventsCount(events, post, types, bound string) string {
	return `(select count(*) from ` + events + ` e
		where e.payload->>'id' = ` + post + `.id::text and e.type in ` + types + `
			and e.created_at >= ` + PostHistoryBegan + bound + `)`
}

// postBodyAtCut is the body a comment or a message had at the cut, by decision 2's one clock,
// the events'. post is the row's alias and column post_body_history's column that names it;
// newest is the alias of its newest state event at the cut and later the alias of its first
// edit or delete event after the cut (each with no row when there is none); types are the post's
// edit and delete types (a SQL list). A post deleted at any time reads with no body (decision
// 14), as the present read gives it.
//
//   - No edit or delete after the cut: the row's body.
//   - The first such event began at or after post_body_history did: the body that event
//     replaced, the history row its transaction wrote. The post's edits and deletes at the cut
//     since the history began wrote the rows before it (historyEventsCount), so it is the row
//     after as many rows as those events.
//   - An edit made before that history began (no row holds what it replaced): the body of the
//     newest event at the cut, unless that event cut it at 32,000 UTF-16 units
//     (body_truncated), which the answer names absent (reason T).
func postBodyAtCut(post, column, newest, later, types, cut string) string {
	return `case
			when ` + post + `.deleted_at is not null then ''
			when ` + later + `.id is null then ` + post + `.body
			when ` + later + `.created_at >= ` + PostHistoryBegan + ` then coalesce((
				select h.body from public.post_body_history h
				where h.` + column + ` = ` + post + `.id
				order by h.edited_at, h.id
				offset ` + historyEventsCount("public.events", post, types, " and e.created_at <= "+cut) + ` limit 1), '')
			when coalesce((` + newest + `.payload->>'body_truncated')::boolean, false) then ''
			else coalesce(` + newest + `.payload->>'body', '')
		end`
}

// Shadows is the body of a WITH list that holds one member for each shadowed table that tables
// names, `<table> as not materialized (<definition>)`, in the order of shadows, the cut written in
// each as a timestamptz literal. A name that is not a shadowed table adds no member, so with no
// shadowed table named the body is empty. Bind puts the shadows of the tables a statement names
// in front of it. NOT MATERIALIZED lets PostgreSQL push the statement's own filters into each
// definition, as it does for the table, and drop each column of a definition that the statement
// does not read. It stays so where a statement names a table many times (the issue listing names
// issues in eleven places): a materialized shadow is read once, but for every row of the table
// and with every column, a search vector and a claim included. On a copy of production's event
// log the issues shadow materialized made each statement measured slower: the issue listing 6.7 s
// against 8.5 s, the issue read's row 0.15 s against 7.5 s, an issue's asks 0.4 s against 11 s.
func Shadows(cut Cut, tables ...string) string {
	var named [len(shadows)]bool
	for _, table := range tables {
		if index := shadowNamed(table, true); index >= 0 {
			named[index] = true
		}
	}
	return shadowList(cut, &named)
}

// shadowList is the body of a WITH list that holds the shadow of each table that named marks, by
// its place in shadows.
func shadowList(cut Cut, named *[len(shadows)]bool) string {
	literal := cut.Literal()
	var list strings.Builder
	for index := range shadows {
		if !named[index] {
			continue
		}
		if list.Len() > 0 {
			list.WriteString(",\n")
		}
		list.WriteString(shadows[index].Table)
		list.WriteString(" as not materialized (")
		list.WriteString(shadows[index].Definition(literal))
		list.WriteString(")")
	}
	return list.String()
}

// shadowNamed is the place in shadows of the table that identifier names, or -1 when it names no
// shadowed table. PostgreSQL folds a word that is not quoted to lower case, so such a word names a
// table in any case of its ASCII letters. A quoted identifier names a table only in the table's
// own case.
func shadowNamed(identifier string, quoted bool) int {
	for index := range shadows {
		table := shadows[index].Table
		if quoted && identifier == table || !quoted && equalFoldASCII(identifier, table) {
			return index
		}
	}
	return -1
}

// equalFoldASCII reports whether word is name with any of name's lower-case ASCII letters in
// upper case, which is how PostgreSQL folds a word that is not quoted.
func equalFoldASCII(word, name string) bool {
	if len(word) != len(name) {
		return false
	}
	for index := range len(word) {
		c := word[index]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != name[index] {
			return false
		}
	}
	return true
}

// MachineryTables are the tables that a past-time read reads as they are now, each with its
// reason. Such a table holds no Dispatch content. It holds only the state of Dispatch's own
// machinery, which decides how a read runs. The relation inventory test of the shadows admits
// these tables with no shadow.
var MachineryTables = map[string]string{
	"embeddings_project_key_backfill": "It records how far the fill of embeddings.project_key went. " +
		"That progress selects the statement of a meaning search (api/search.go, meaningStatementFor). " +
		"It holds no Dispatch content.",
	"search_text_versions_fill": "It holds the cursor of the keyword table's fill and the time the fill reached. " +
		"That reach decides whether a search at a cut is answered or refused (store.TextVersionsAt). " +
		"It holds no Dispatch content.",
}

// CutReadTables are the tables that a past-time read reads with no shadow, under a predicate of
// its own that holds the read to the cut. Each value is that predicate. The relation inventory
// test of the shadows admits these tables with no shadow; each read of one of them at a cut
// must apply its predicate.
var CutReadTables = map[string]string{
	"ask_block_state": "Only the revision of an ask that did not change after the cut (AsksChangedAfterStatement), " +
		"which the read of its ask_blocks reads with it; an answer leaves out the revision of every other ask.",
	"ask_block_texts": "Only the texts of the parts that a meaning hit covers, of an ask whose text did not change after the cut.",
	"ask_blocks": "Only the parts of an ask that did not change after the cut (AsksChangedAfterStatement); " +
		"an answer leaves out the part ids and the revision of every other ask.",
	"comment_block_texts": "Only the texts of the blocks that a meaning hit covers, of a comment whose text did not change after the cut.",
	"comment_blocks": "Only the blocks of a comment that did not change after the cut (ItemsChangedAfterStatement), " +
		"or of a comment deleted at any time, whose blocks read removed with no text as the comment reads deleted at the cut.",
	"comment_mentions": "Only the mentions of a comment that the comments shadow gives at the cut: " +
		"the transaction that creates a comment writes its mentions, and no write changes them after it.",
	"message_blocks": "Only the blocks of a message that did not change after the cut (ItemsChangedAfterStatement), " +
		"or of a message deleted at any time, whose blocks read removed with no text as the message reads deleted at the cut.",
	"document_blocks": "Only the nodes of a document whose newest version at the cut is its present newest version: " +
		"a search at a cut leaves out the href of an ask hit whose typed block is in a document that changed after the cut.",
	"document_block_texts": "Only the rows of the document's version at the cut, " +
		"for a version that document_indexed_versions holds.",
	"document_indexed_versions": "Only the rows of the document's version at the cut.",
	"document_search":           "Only the documents whose newest version at the cut is their present newest version.",
	"embeddings": "Only the windows of items that did not change after the cut, " +
		"embedded at or before the cut.",
	"post_body_history": "Only the body that the first comment.edited, comment.deleted, message.edited or message.deleted event " +
		"of the post after the cut replaced, for an event at or after the time schema_migrations records for 20261110224508: " +
		"the row of the post, in edited_at and id order, after as many rows as the post has such events at the cut at or after that time. " +
		"In the history read of one post, only the rows that the post's edit and delete events at the cut wrote: " +
		"the first rows in edited_at and id order, as many as the post has such events at or after that time " +
		"(CommentHistoryRowsAtCut, MessageHistoryRowsAtCut).",
	"search_text_versions": "Only through store.TextVersions.SQL(), the version of each item that held at the cut, " +
		"which store.TextVersionsAt gives only for a cut the keyword table's fill has reached.",
}
