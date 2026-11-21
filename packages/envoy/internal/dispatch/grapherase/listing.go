package grapherase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// listing is every row a run would delete, rewrite, redact or leave, by section, each a line
// naming the row by its table and its key or by the address a reader knows it by. It names no
// erased text: a value tuple is listed without its value and a row by its key alone.
type listing struct {
	tuples, rows, versions, reviews, embeddings, search, excerpts, events, documents, left, blockers []string
}

// lines renders the listing as Write prints it and as its digest is taken: the node first, then
// each section under a line counting it.
func (l listing) lines(e *Erasure) []string {
	lines := []string{fmt.Sprintf("  node: %s:%s (supertag %s)", e.Kind, e.ID, e.Supertag)}
	section := func(head string, items []string) {
		lines = append(lines, fmt.Sprintf("  %s: %d", head, len(items)))
		for _, item := range items {
			lines = append(lines, "    "+item)
		}
	}
	lines = append(lines, fmt.Sprintf("  tuples: %d out of the node, %d into it", e.Tuples, e.Inbound))
	for _, item := range l.tuples {
		lines = append(lines, "    "+item)
	}
	section("rows of the node", l.rows)
	section("versions holding the erased text", l.versions)
	section("reviews over those bytes", l.reviews)
	section("embedding rows", l.embeddings)
	section("search rows", l.search)
	section("excerpts quoting the node", l.excerpts)
	section("event rows about the node holding the erased text, redacted in place", l.events)
	section("live documents whose history is replaced", l.documents)
	lines = append(lines, "  display trees: the text_trees row of every text above the run removes or rewrites goes; "+
		"a tree of any other text is that text's, keyed by its digest, and is not listed")
	section("rows that hold the erased bytes and derive from another node, left as they are", l.left)
	for _, blocker := range l.blockers {
		lines = append(lines, "  blocked: "+blocker)
	}
	return lines
}

// digest is the SHA-256 of a listing's lines, in hex, which --confirm names.
func digest(lines []string) string {
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

// readEmbeddings reads the rows of the embedding store that derive from the node and that the run
// deletes: the node's own whole-text row, which an older build wrote under its kind and id; for a
// comment every window of its blocks, under the window kind and owner key refs names them by
// (refs.ItemKind.WindowKind, WindowOwnerKey), with the window state kept beside them; and for a
// block each window of its document that covers it (its covers column names the blocks a
// window's text came from, so a block a window holds in part is reached as well as one it holds
// whole) or holds an erased text, its document's whole-text row, and the issue's own row where
// the document is the issue's spec, each that holds an erased text. A window of another document
// or of another item is that node's, and stays. A block's document here is the one it stands in
// (erasedRead.versioned), which writes the windows of a block of an issue room.
func (e *Erasure) readEmbeddings(ctx context.Context, tx pgx.Tx, read *erasedRead) error {
	windowKind, ownerKey := e.itemWindows()
	document := ""
	if read.versioned != nil {
		document = read.versioned.artifact
	}
	rows, err := tx.Query(ctx, `
		select held.kind, held.id from embeddings held
		where (held.kind = $1 and held.id = $2)
		   or ($3 <> '' and held.kind = $3 and split_part(held.id, '#', 1) = $4)
		   or ($5 <> '' and held.kind = $6 and split_part(held.id, '#', 1) = $5
		       and ($7 = any(held.covers)
		            or exists (select 1 from unnest($8::text[]) erased (text) where position(erased.text in held.text_snapshot) > 0)))
		   or ($5 <> '' and held.kind = 'document' and held.id = $5
		       and exists (select 1 from unnest($8::text[]) erased (text) where position(erased.text in held.text_snapshot) > 0))
		   or ($9 <> '' and held.kind = 'issue' and held.id = $9
		       and exists (select 1 from unnest($8::text[]) erased (text) where position(erased.text in held.text_snapshot) > 0))
		order by 1, 2`,
		e.Kind, e.ID, windowKind, ownerKey, document, refs.DocumentWindowKind, read.blockID, read.texts, read.versioned.specIssue())
	if err != nil {
		return fmt.Errorf("read the embedding rows of %s:%s: %w", e.Kind, e.ID, err)
	}
	if read.embedded, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (embeddingRow, error) {
		var held embeddingRow
		err := row.Scan(&held.kind, &held.id)
		return held, err
	}); err != nil {
		return fmt.Errorf("read the embedding rows of %s:%s: %w", e.Kind, e.ID, err)
	}
	for _, held := range read.embedded {
		verb := "delete"
		if held.kind == "issue" {
			verb = "rewrite"
		}
		read.listing.embeddings = append(read.listing.embeddings, fmt.Sprintf("%s embeddings %s %s", verb, held.kind, held.id))
	}
	if windowKind != "" {
		var states int64
		if err := tx.QueryRow(ctx, `select count(*) from embedding_window_states where kind = $1 and owner_key = $2`,
			windowKind, ownerKey).Scan(&states); err != nil {
			return fmt.Errorf("read the window state of %s:%s: %w", e.Kind, e.ID, err)
		}
		if states > 0 {
			read.listing.embeddings = append(read.listing.embeddings,
				fmt.Sprintf("delete embedding_window_states %s %s", windowKind, ownerKey))
		}
	}
	return nil
}

// readSearch lists the search rows that stand on a version the run deletes: the search row of the
// document the block stands in (erasedRead.versioned), and the issue's search vector where that
// document is its spec (restateSearch). document_search is read only where it stands
// (documentSearchStands), since the boot step that retires it may have dropped it.
func (e *Erasure) readSearch(ctx context.Context, tx pgx.Tx, read *erasedRead) error {
	if len(read.artifacts) == 0 {
		return nil
	}
	stands, err := documentSearchStands(ctx, tx)
	if err != nil {
		return err
	}
	var searched bool
	if stands {
		if err := tx.QueryRow(ctx, `select exists (select 1 from document_search where artifact_id = $1::uuid)`,
			read.versioned.artifact).Scan(&searched); err != nil {
			return fmt.Errorf("read the search row of %s: %w", read.versioned.refKey, err)
		}
	}
	if searched {
		read.listing.search = append(read.listing.search, "rewrite document_search of "+read.versioned.refKey)
	}
	if issue := read.versioned.specIssue(); issue != "" {
		read.listing.search = append(read.listing.search, "rewrite issues.search of "+issue)
	}
	return nil
}

// readExcerpts lists the rows of other blocks that quote the node's text and that the run
// rewrites: the text, the text value tuples and the excerpt the redaction moves in every block
// holding the node's block (readAncestors, written back by redactBlocks), and the excerpt each
// mention of the node's document shows when it was cut from the node's block or from one of those
// (refs.RecordExcerpts), which clearExcerpts takes away for the reconciliation to cut again from
// the latest version left. A mention's excerpt is the first refs.BlockExcerptRunes runes of its
// block, so it is taken whether or not it holds a whole erased text. A value tuple is listed by
// its seq and its block and without its value, as readTuples lists one.
func (e *Erasure) readExcerpts(ctx context.Context, tx pgx.Tx, read *erasedRead) error {
	if read.document == nil || len(read.texts) == 0 {
		return nil
	}
	for _, text := range read.blockTexts {
		read.listing.excerpts = append(read.listing.excerpts,
			fmt.Sprintf("redact document_block_texts %s#%s version %d", read.document.refKey, text.blockID, text.version))
	}
	for _, value := range read.blockValues {
		read.listing.excerpts = append(read.listing.excerpts,
			fmt.Sprintf("redact graph_tuples %d: block:%s#%s text", value.seq, read.document.refKey, value.blockID))
	}
	for _, excerpt := range read.excerpts {
		read.listing.excerpts = append(read.listing.excerpts, "redact document_blocks "+excerpt.nodeID+" excerpt")
	}
	rows, err := tx.Query(ctx, `
		select 'rewrite refs ' || $3::text || ' -> ' || r.to_kind || ':' || r.to_id || ' excerpt' from refs r
		where r.from_kind = 'artifact' and r.from_id = $1::uuid::text and r.excerpt_block_id = any($2::text[])
		  and r.excerpt_text is not null
		order by 1`,
		read.document.artifact, append([]string{read.blockID}, read.ancestors...), read.document.refKey)
	if err != nil {
		return fmt.Errorf("read the excerpts quoting %s:%s: %w", e.Kind, e.ID, err)
	}
	mentions, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("read the excerpts quoting %s:%s: %w", e.Kind, e.ID, err)
	}
	read.listing.excerpts = append(read.listing.excerpts, mentions...)
	slices.Sort(read.listing.excerpts)
	return nil
}

// textColumn is one column of a table a text can be written into, with the table's primary key,
// which names a row of it on the listing.
type textColumn struct {
	schema, table, name, dataType, udtName string
	key                                    []string
}

// derivedRows is one set of rows of a table that derive from the node, as a predicate over the
// row (named held) and the values leftBound binds: rows the run deletes or rewrites, which the
// search for the rows it leaves passes over. columns limits it to those columns, all where empty.
type derivedRows struct {
	columns []string
	where   string
}

// derivedTables is every table holding rows that derive from a node and may hold its text, by
// name, each with the rows of it that do. It is the same derivation apply writes by, row set for
// row set: the node's own rows, its tuples, its projections, its own document's rows, its
// embedding rows, the events about it and its project's term statistics, which the run counts
// again. The rows that cannot exist without a projection row are added by leftDerived, from the
// foreign keys into the projections. A run of the command bar's own rows (runTables) are here
// too, by the run's uuid, which is its node's id (runDerived): its going_requests row holds the
// words readTexts reads off its node, and its going_events rows its answer, and apply deletes
// both with the node. A block's rows are keyed by bound.document, its room's tree for a block of
// an issue room, and its versions, search rows and windows by bound.versioned, the document it
// stands in (erasedRead.versioned).
var derivedTables = runDerived(map[string][]derivedRows{
	"graph_nodes": {{where: `held.kind = bound.kind and held.id = bound.id`}},
	"graph_tuples": {
		{where: `(held.from_kind = bound.kind and held.from_id = bound.id) or (held.to_kind = bound.kind and held.to_id = bound.id)`},
		// The text values of the blocks holding the node's block, which the redaction rewrites
		// beside their document_block_texts rows (redactBlocks).
		{where: `bound.document_ref <> '' and held.from_kind = '` + refs.BlockNodeKind + `'
			and held.from_id = any(array(select bound.document_ref || '#' || ancestor from unnest(bound.ancestors) ancestor))
			and held.kind = '` + graphstore.AttributeText + `' and held.to_kind = '` + graphstore.KindValue + `'`},
	},
	"comments": {{where: `held.node_kind = bound.kind and held.node_address = bound.id`}},
	"document_blocks": {
		{where: `held.node_kind = bound.kind and held.node_address = bound.id`},
		{where: `held.artifact_id = nullif(bound.document, '')::uuid and held.block_id = any(bound.ancestors)`},
	},
	"block_refs":           {{where: `held.node_kind = bound.kind and held.node_address = bound.id`}},
	"document_block_texts": {{where: `held.artifact_id = nullif(bound.document, '')::uuid and (held.block_id = bound.block or held.block_id = any(bound.ancestors))`}},
	"artifact_versions":    {{where: `held.artifact_id = nullif(bound.versioned, '')::uuid and held.number = any(bound.numbers)`}},
	"document_search":      {{where: `held.artifact_id = nullif(bound.versioned, '')::uuid and cardinality(bound.numbers) > 0`}},
	"issues":               {{columns: []string{"search"}, where: `bound.spec_issue <> '' and held.key = bound.spec_issue and cardinality(bound.numbers) > 0`}},
	"embeddings": {{where: `(held.kind = bound.kind and held.id = bound.id)
		or (bound.window_kind <> '' and held.kind = bound.window_kind and split_part(held.id, '#', 1) = bound.owner_key)
		or (bound.versioned <> '' and held.kind in ('document', '` + refs.DocumentWindowKind + `') and split_part(held.id, '#', 1) = bound.versioned)
		or (bound.spec_issue <> '' and held.kind = 'issue' and held.id = bound.spec_issue)`}},
	"doc_updates":       {{where: `held.artifact_id::text = any(bound.histories)`}},
	"doc_checkpoints":   {{where: `held.artifact_id::text = any(bound.histories)`}},
	"doc_snapshots":     {{where: `held.artifact_id::text = any(bound.histories)`}},
	"refs":              {{where: `bound.document <> '' and held.from_kind = 'artifact' and held.from_id = bound.document and (held.excerpt_block_id = bound.block or held.excerpt_block_id = any(bound.ancestors))`}},
	"events":            {{where: `held.id = any(bound.events)`}},
	"search_term_stats": {{where: `bound.project <> '' and held.project_key = bound.project`}},
})

// runDerived is tables with the rows of a run of the command bar in each of runTables: those whose
// Column names the run, for a node that is an interaction.
func runDerived(tables map[string][]derivedRows) map[string][]derivedRows {
	for _, run := range runTables {
		tables[run.Table] = []derivedRows{{
			where: `bound.kind = '` + graphstore.KindInteraction + `' and held.` + run.Column + `::text = bound.id`,
		}}
	}
	return tables
}

// leftBound is the one row of values every statement of readLeft reads its derivation from, so
// each binds the same fifteen parameters whichever of them its table's predicate names.
const leftBound = `with bound as (select $1::text as kind, $2::text as id, $3::text[] as texts, $4::text as document,
	$5::text as block, $6::text[] as ancestors, $7::bigint[] as events, $8::int[] as numbers, $9::text as window_kind,
	$10::text as owner_key, $11::text as spec_issue, $12::text as project, $13::text[] as histories,
	$14::text as document_ref, $15::text as versioned)`

// readLeft searches every column of every table a text can be written into for the erased texts,
// and lists each row it finds that does not derive from the node - by its table, its primary key
// and the columns holding them - as a row the run leaves as it is. Those rows are another node's
// text, which an operator erases by a run of that node, and the run writes none of them. It lists
// as well an event about the node whose payload holds the texts in its structural values alone:
// the redaction rewrites no such value (structuralKey), so the run leaves that row whole, and the
// listing says so.
//
// The search names no store, so a table a later build adds is listed without anyone adding it
// here. A bytea is searched for the texts' UTF-8 bytes, since its text form is hex, and a json
// column for each text as json escapes it as well as raw. Two tables are not searched. Each row
// of text_trees is a cache of one text, keyed by that text's digest, which the run deletes for
// every text it removes or rewrites (forgetTrees) and which belongs to whatever text it was read
// from otherwise. graph_erasures is the erase's own record, which holds no erased text, and which
// the run reads nothing of before it writes its row last.
func (e *Erasure) readLeft(ctx context.Context, tx pgx.Tx, read *erasedRead) error {
	if len(read.texts) == 0 {
		return nil
	}
	derived, err := leftDerived(ctx, tx)
	if err != nil {
		return err
	}
	columns, err := textColumns(ctx, tx)
	if err != nil {
		return err
	}
	windowKind, ownerKey := e.itemWindows()
	document, documentRef, versioned := "", "", ""
	if read.document != nil {
		document, documentRef, versioned = read.document.artifact, read.document.refKey, read.versioned.artifact
	}
	eventIDs := make([]int64, len(read.events))
	for index, event := range read.events {
		eventIDs[index] = event.id
	}
	histories := make([]string, 0, len(read.margins)+1)
	for _, replaced := range read.replacedHistories() {
		histories = append(histories, replaced.document.artifact)
	}
	args := []any{e.Kind, e.ID, read.texts, document, read.blockID, read.ancestors, eventIDs, read.numbers,
		windowKind, ownerKey, read.versioned.specIssue(), read.project, histories, documentRef, versioned}
	held := map[string][]string{}
	for _, column := range columns {
		if column.schema == "public" && (column.table == "text_trees" || column.table == "graph_erasures") {
			continue
		}
		var arms []string
		if column.schema == "public" {
			for _, rows := range derived[column.table] {
				if len(rows.columns) == 0 || slices.Contains(rows.columns, column.name) {
					arms = append(arms, "("+rows.where+")")
				}
			}
		}
		passed := "false"
		if len(arms) > 0 {
			passed = "coalesce(" + strings.Join(arms, " or ") + ", false)"
		}
		key := rowKey(column)
		if column.schema == "public" && rowNames[column.table] != "" {
			key += ` || coalesce(' [' || (` + rowNames[column.table] + `) || ']', '')`
		}
		rows, err := tx.Query(ctx, leftBound+` select `+key+` from `+
			pgx.Identifier{column.schema, column.table}.Sanitize()+` held, bound where `+holdsText(column)+
			` and not `+passed, args...)
		if err != nil {
			return fmt.Errorf("search %s.%s.%s for the erased text: %w", column.schema, column.table, column.name, err)
		}
		keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return fmt.Errorf("search %s.%s.%s for the erased text: %w", column.schema, column.table, column.name, err)
		}
		for _, key := range keys {
			place := column.schema + "." + column.table + " " + key
			held[place] = append(held[place], column.name)
		}
	}
	for _, place := range slices.Sorted(maps.Keys(held)) {
		read.listing.left = append(read.listing.left, "leave "+place+" "+strings.Join(held[place], ", "))
	}
	margins, err := e.readKeptMargins(ctx, tx, read)
	if err != nil {
		return err
	}
	read.listing.left = append(read.listing.left, margins...)
	return nil
}

// readKeptMargins lists every margin record that holds an erased text and that the seed of a
// replaced history keeps (docs.KeptMarginQuoting): a suggestion whose replacement, or a comment
// whose body, quotes the erased text. Each is that comment's text, another node's, which the run
// leaves (R5), and it stands in that document's doc_updates after the run, which derive from the
// node otherwise. So the listing names each such record by its document, its mark id and its
// comment, and a search of that history after the run can tell those bytes from the node's own.
// A replaced history of an issue room is the tree's, and a comment's anchor names the document
// of the room it is written on, so a record's comment is found by its anchor's room
// (refs.DocumentRoomSQL), which is its own document for a document of a one-document room.
//
// It reads the stored history without taking the document's owner row or its room lock, as the
// rest of the listing reads, so a run without --confirm lists them in its read-only transaction;
// a confirmed run reads it again under its locks with the rest of its listing.
func (e *Erasure) readKeptMargins(ctx context.Context, tx pgx.Tx, read *erasedRead) ([]string, error) {
	var lines []string
	for _, replaced := range read.replacedHistories() {
		scope := replaced.scope
		scope.Texts = read.texts
		marks, err := docs.KeptMarginQuoting(ctx, tx, replaced.document.artifact, scope)
		if err != nil {
			return nil, fmt.Errorf("read the margin of %s: %w", replaced.document.refKey, err)
		}
		if len(marks) == 0 {
			continue
		}
		rows, err := tx.Query(ctx, `
			select marks.mark, coalesce((select 'comment:' || c.id::text from comments c
			                             where c.anchor->>'mark_id' = marks.mark
			                               and `+refs.DocumentRoomSQL(`(c.anchor->>'artifact_id')::uuid`)+` = $1::uuid
			                             order by c.created_at limit 1), 'no comment row')
			from unnest($2::text[]) as marks (mark)
			order by 1`, replaced.document.artifact, marks)
		if err != nil {
			return nil, fmt.Errorf("read the comments of the margin of %s: %w", replaced.document.refKey, err)
		}
		named, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (string, error) {
			var mark, comment string
			err := row.Scan(&mark, &comment)
			return "leave the margin record " + mark + " of " + replaced.document.refKey + " [" + comment + "] in its kept history", err
		})
		if err != nil {
			return nil, fmt.Errorf("read the comments of the margin of %s: %w", replaced.document.refKey, err)
		}
		lines = append(lines, named...)
	}
	return lines, nil
}

// rowNames names a row of the tables a reader knows the rows of by an address, beside the key the
// listing gives every row: a version by its document and its number, a comment and a block by
// their node, a block's text and a document's history by the document, an event by its type.
var rowNames = map[string]string{
	"artifact_versions": `select a.ref_key || ' version ' || held.number || coalesce(' of ' || a.issue_key, '')
		from artifacts a where a.id = held.artifact_id`,
	"comments":             `'comment:' || held.id::text`,
	"document_blocks":      `'block:' || held.node_id`,
	"document_block_texts": `select a.ref_key || '#' || held.block_id || ' version ' || held.version from artifacts a where a.id = held.artifact_id`,
	"doc_updates":          `select a.ref_key from artifacts a where a.id = held.artifact_id`,
	"doc_checkpoints":      `select a.ref_key from artifacts a where a.id = held.artifact_id`,
	"doc_snapshots":        `select a.ref_key from artifacts a where a.id = held.artifact_id`,
	"events":               `held.type`,
}

// leftDerived is derivedTables with the rows that cannot exist without a projection row of the
// node: every row of a table whose required single-column reference names one, which apply
// deletes with the projection row. A row pointing at one through a nullable reference is another
// node's and refuses the run (readBlockers).
func leftDerived(ctx context.Context, tx pgx.Tx) (map[string][]derivedRows, error) {
	derived := maps.Clone(derivedTables)
	for _, held := range projectedTables {
		references, err := referencesTo(ctx, tx, held.Table)
		if err != nil {
			return nil, err
		}
		for _, reference := range references {
			if !reference.Required {
				continue
			}
			derived[reference.Table] = append(slices.Clone(derived[reference.Table]), derivedRows{
				where: `held.` + reference.Column + ` in (select projected.` + reference.Referenced + ` from ` + held.Table +
					` projected where projected.node_kind = bound.kind and projected.node_address = bound.id)`,
			})
		}
	}
	return derived, nil
}

// textColumns reads every column of every table a text can be written into, as graph-erase.sh's
// held_anywhere reads them: a column of a type that holds no text - a number, a time, a uuid - is
// left out. Each comes with the key its table names a row by: its primary key, or where it has
// none its first unique index over columns alone with no predicate, as artifact_versions'
// (artifact_id, number) is.
func textColumns(ctx context.Context, tx pgx.Tx) ([]textColumn, error) {
	rows, err := tx.Query(ctx, `
		select c.table_schema, c.table_name, c.column_name, c.data_type, c.udt_name,
		       coalesce((select array_agg(a.attname::text order by k.n)
		                 from (select u.indrelid, u.indkey from pg_index u
		                       where u.indrelid = format('%I.%I', c.table_schema, c.table_name)::regclass
		                         and u.indisunique and u.indpred is null and u.indexprs is null
		                       order by u.indisprimary desc, u.indexrelid limit 1) i
		                 cross join lateral unnest(i.indkey) with ordinality as k (attnum, n)
		                 join pg_attribute a on a.attrelid = i.indrelid and a.attnum = k.attnum), '{}')
		from information_schema.columns c
		join information_schema.tables t on t.table_schema = c.table_schema and t.table_name = c.table_name
		where t.table_type = 'BASE TABLE'
		  and c.table_schema not in ('pg_catalog', 'information_schema') and c.table_schema not like 'pg_temp%'
		  and c.data_type not in ('smallint', 'integer', 'bigint', 'numeric', 'real', 'double precision',
		    'boolean', 'uuid', 'date', 'interval', 'time with time zone', 'time without time zone',
		    'timestamp with time zone', 'timestamp without time zone', 'inet', 'cidr', 'macaddr',
		    'macaddr8', 'oid', 'xid', 'xid8', 'pg_lsn', 'money', 'bit', 'bit varying')
		order by 1, 2, 3`)
	if err != nil {
		return nil, fmt.Errorf("list the columns of the database: %w", err)
	}
	columns, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (textColumn, error) {
		var column textColumn
		err := row.Scan(&column.schema, &column.table, &column.name, &column.dataType, &column.udtName, &column.key)
		return column, err
	})
	if err != nil {
		return nil, fmt.Errorf("list the columns of the database: %w", err)
	}
	return columns, nil
}

// holdsText is the test of one column of a row named held for any of the erased texts bound.texts
// holds.
func holdsText(column textColumn) string {
	name := "held." + pgx.Identifier{column.name}.Sanitize()
	switch {
	case column.dataType == "bytea":
		return `exists (select 1 from unnest(bound.texts) erased (text) where position(convert_to(erased.text, 'UTF8') in ` + name + `) > 0)`
	case column.udtName == "_bytea":
		return `exists (select 1 from unnest(` + name + `) element cross join unnest(bound.texts) erased (text)
			where position(convert_to(erased.text, 'UTF8') in element) > 0)`
	case column.dataType == "json" || column.dataType == "jsonb":
		return `exists (select 1 from unnest(bound.texts) erased (text)
			where position(erased.text in ` + name + `::text) > 0
			   or position(substring(to_jsonb(erased.text)::text from 2 for length(to_jsonb(erased.text)::text) - 2) in ` + name + `::text) > 0)`
	}
	return `exists (select 1 from unnest(bound.texts) erased (text) where position(erased.text in ` + name + `::text) > 0)`
}

// rowKey names a row of a column's table, named held, by its primary key - `(id)=(<uuid>)` - or
// by its ctid where the table has none.
func rowKey(column textColumn) string {
	if len(column.key) == 0 {
		return `'(ctid)=(' || held.ctid::text || ')'`
	}
	quoted := make([]string, len(column.key))
	for index, name := range column.key {
		quoted[index] = "held." + pgx.Identifier{name}.Sanitize()
	}
	names := strings.ReplaceAll(strings.Join(column.key, ", "), "'", "''")
	return `'(` + names + `)=' || row(` + strings.Join(quoted, ", ") + `)::text`
}
