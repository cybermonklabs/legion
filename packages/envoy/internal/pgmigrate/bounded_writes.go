package pgmigrate

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// boundedWritesRule is what a refusal on size says, after the size, of the one route a migration
// that writes rows of a table above the limit has (boundedWrites), before the words that say what
// the migration does besides.
var boundedWritesRule = fmt.Sprintf("a migration that writes rows of such a table may touch it only with bounded writes (an insert of at most %s literal rows, or an update of at most %s rows by their primary key)",
	Thousands(BoundedWriteRows), Thousands(BoundedWriteRows))

// writeReading is what Census reads of one pending migration to admit a table above the limit
// that the migration names for bounded writes (boundedWrites).
type writeReading struct {
	// c reads the catalog: in the census's transaction (c.census), by the oid the census found the
	// table by, for the table's keys, rules, row-level security, child tables and triggers
	// (Catalog read 8), and in the migration's catalog (c.catalog), nil where Census gives the
	// migration none, for the type a value casts to and the function it calls (defaultPartsOf).
	c *classifier
	// statements are the migration's statements as Postgres's parser reads them, nil where it does
	// not parse, and touched the tables each one touches, read the first time a table asks.
	statements []statement
	touched    [][]string
	// voided says why no table of the migration is admitted, in words that follow "and"; "" where
	// nothing stops every admission of the migration.
	voided string
	// changedBy names, by table, the first earlier pending migration that touches the table with
	// anything but bounded writes (Census keeps it).
	changedBy map[string]string
	// reviewed are the enabled triggers a table may be admitted beside
	// (CensusOptions.reviewedTriggers).
	reviewed []reviewedTrigger
}

// boundedVerdict is what boundedWrites answers for one table: where it admits the table, the
// number of the migration's statements that touch it, each a bounded write, and the table's
// enabled triggers; where it does not, why, in words that follow "and".
type boundedVerdict struct {
	statements int
	triggers   []string
	why        string
}

// boundedWrites says whether table, a table above the limit that the migration names, found by
// oid, is admitted for bounded writes: whether every statement of the migration that touches it,
// as the census reads touches (CensusTables, over each statement's own text), is a bounded write
// of it, as Postgres's own parser reads the statement:
//
//   - an insert into the table of a VALUES list of at most BoundedWriteRows rows, each value a
//     literal, with no WITH, no RETURNING and no OVERRIDING, and either no ON CONFLICT or ON
//     CONFLICT … DO NOTHING, with any conflict target or none (boundedInsert);
//   - an update of the table, under its own name or an alias, that sets columns to literals and
//     names at most BoundedWriteRows rows by their primary key, compared with literals the key's
//     index is searched by (keyRows, keyLiteral), with no WITH, no FROM and no RETURNING
//     (boundedUpdate).
//
// A literal is what Postgres stores as a column's default in place of a rewrite (defaultPartsOf),
// none of which reads a table: a constant, a negative number or NULL among them, an ARRAY[…] of
// constants, a cast of one of those to a type admittedType admits ('{}'::jsonb), a SQL value
// function (current_timestamp), or a call of no argument of a pg_catalog function admittedCall
// admits (now()). A cast and a call need the migration's catalog, so with none only the others are
// literals. An operator, a subquery, a call with arguments (jsonb_build_object(…)), a function
// outside pg_catalog and a column reference are not. Nor is DEFAULT, as a value or as a set value:
// a default is code the catalog holds, and the census would have to judge it as catalogOnlyColumn
// judges an added column's before it could show that it reads no row, so it is left out. A column
// an insert leaves out takes its default all the same, and that default runs for each inserted row
// as a trigger does: the census reads neither.
//
// Such a write takes ROW EXCLUSIVE on the table, which holds off no read and no write of another
// row, and it reads and writes only the rows it names: an insert probes each unique index once a
// row and adds an entry to every index, and an update finds each row through its primary key's
// index. What it costs grows with its rows, not with the table, and BoundedWriteRows says what
// that is at the bound. The table is held to more, since each of these can make a write of one row
// read the whole table:
//
//   - no rule (pg_rewrite): a rule can run any statement in place of the write or beside it;
//   - no row-level security (relrowsecurity): a policy's expression can read any table for each
//     row the write meets;
//   - for an update without ONLY, no child table (pg_inherits): Postgres updates every child too,
//     whose size and keys the census does not read;
//   - the census reads the table's keys, rules and triggers as they stand before any pending
//     migration applies, so no earlier pending migration may touch the table with anything but
//     bounded writes (changedBy), and neither the migration nor an earlier pending one may hold a
//     statement whose effect the census cannot read (voided): a statement off the keep-on list, a
//     DO block among them, or one that names a relation of pg_catalog (takesLaterExemptions). Nor
//     may the migration hold a statement after which its names can resolve to another table than
//     the one the census read (catalogReadsOff), a set of search_path among them.
//
// An enabled trigger of the table runs for each row the writes write, and it can read any table,
// while the census does not read its body, as it reads no function's. So the table is admitted
// only where each of its enabled triggers is on reviewedTriggers, whose entry says why the
// trigger's body is bounded, and the report names each one beside the admission (Report.Write).
// A foreign key of the table checks each written row against the table it references, by that
// table's key, and the census reads that table as one the key reaches. A statement that only reads
// the table, a select of it or an insert into another table from it, is no touch (TouchedTables),
// so it is not judged here: it holds the writes' row locks for as long as it reads, as it holds
// the locks of any table a migration the census admits touches.
//
// why is "" where no statement that touches the table is an insert or an update: a migration that
// writes none of the table's rows took no bounded route, and its refusal is on the size alone, as
// it was before this rule. So is the refusal of a migration that does not parse, since the census
// cannot read what it writes.
func (w *writeReading) boundedWrites(ctx context.Context, table string, oid uint32) (boundedVerdict, error) {
	if w.statements == nil {
		return boundedVerdict{}, nil
	}
	if w.touched == nil {
		w.touched = make([][]string, len(w.statements))
		for i, s := range w.statements {
			text := readMigration(s.text)
			tables, err := censusTables(ctx, w.c.census, text, matchedTouches(text.code), nil)
			if err != nil {
				return boundedVerdict{}, err
			}
			for _, touched := range tables {
				w.touched[i] = append(w.touched[i], touched.Name)
			}
		}
	}
	var touching []statement
	writes := false
	for i, s := range w.statements {
		if slices.Contains(w.touched[i], table) {
			touching = append(touching, s)
			writes = writes || s.node == "InsertStmt" || s.node == "UpdateStmt"
		}
	}
	switch {
	case !writes:
		return boundedVerdict{}, nil
	case w.voided != "":
		return boundedVerdict{why: w.voided}, nil
	case w.changedBy[table] != "":
		return boundedVerdict{why: fmt.Sprintf("the earlier pending migration %s touches %s with more than bounded writes, while the census reads the table's keys, rules and triggers as they stand before any pending migration applies", w.changedBy[table], table)}, nil
	}
	var facts boundedTable
	found, err := w.c.read(w.c.census, boundedTableQuery, oid, &facts.primaryKey, &facts.keyTypes, &facts.referenced, &facts.ruled, &facts.rowSecurity, &facts.children, &facts.triggers)
	if err != nil {
		return boundedVerdict{}, err
	}
	if !found {
		return boundedVerdict{why: "the census could not read the table's keys, rules and row-level security"}, nil
	}
	for _, s := range touching {
		var why string
		switch s.node {
		case "InsertStmt":
			why, err = w.boundedInsert(s.body, table)
		case "UpdateStmt":
			why, err = w.boundedUpdate(s.body, table, facts)
		default:
			why = statementKind(s.node) + " is not one"
		}
		if err != nil || why != "" {
			return boundedVerdict{why: why}, err
		}
	}
	switch {
	case facts.ruled:
		return boundedVerdict{why: table + " has a rule, which can run any statement in place of a write or beside it"}, nil
	case facts.rowSecurity:
		return boundedVerdict{why: table + " has row-level security, whose policies can read any table for each row a write meets"}, nil
	}
	if why := unreviewedTriggers(table, facts.triggers, w.reviewed); why != "" {
		return boundedVerdict{why: why}, nil
	}
	return boundedVerdict{statements: len(touching), triggers: facts.triggers}, nil
}

// reviewedTrigger is one enabled trigger boundedWrites admits a table above the limit beside: the
// table, the trigger's name, and why its body reads and writes a bounded number of rows for each
// row a bounded write writes.
type reviewedTrigger struct {
	table, trigger, bounded string
}

// reviewedTriggers are the enabled triggers boundedWrites admits a table above the limit beside.
// A trigger runs for each row a write writes and can read any table, and the census does not read
// its body, so a trigger is admitted only once a person has read that body and written here why
// it is bounded: what it reads, by which key, and what it writes, for each row. A table with an
// enabled trigger that is not here is refused, and its trigger is reviewed and added before the
// migration can pass. An entry names a trigger as the table holds it when the entry is written:
// a later migration that replaces the trigger's function changes the body the entry reviewed, so
// that migration reads the entry again.
//
// graph_nodes' two are Dispatch's: the first type migration written after the store's backfill
// (20261123080000_graph_supertags_plan_task_steps) writes graph_nodes by bounded writes alone, and
// the census admits it above the limit only beside them. Both run block_schema_changed, as
// 20261109051232_graph_supertags created it and no later migration replaced it.
var reviewedTriggers = []reviewedTrigger{
	{table: "graph_nodes", trigger: "graph_nodes_block_schema_written",
		bounded: "it runs, after an insert or update of a supertag or attribute node alone (its when clause), block_schema_changed, which reads no table and calls pg_notify('dispatch_block_schema', ''); Postgres folds the notifications of one channel and payload into one per transaction"},
	{table: "graph_nodes", trigger: "graph_nodes_block_schema_removed",
		bounded: "it runs, after a delete of a supertag or attribute node alone (its when clause), block_schema_changed, which reads no table and calls pg_notify('dispatch_block_schema', ''); Postgres folds the notifications of one channel and payload into one per transaction"},
}

// ReviewedTriggerNames is the names of the enabled triggers of table that reviewedTriggers holds,
// in its order: the triggers a store's own test holds to the definitions their entries reviewed
// (dispatch store's TestTheReviewedTriggersAreTheOnesTheStoreHolds), since this list names a
// trigger and not its body.
func ReviewedTriggerNames(table string) []string {
	var names []string
	for _, reviewed := range reviewedTriggers {
		if reviewed.table == table {
			names = append(names, reviewed.trigger)
		}
	}
	return names
}

// unreviewedTriggers says why table, whose enabled triggers are triggers, is refused for the
// triggers that are not on reviewed, the census's list (CensusOptions.reviewedTriggers), in words
// that follow "and"; "" where every one is.
func unreviewedTriggers(table string, triggers []string, reviewed []reviewedTrigger) string {
	var unreviewed []string
	for _, trigger := range triggers {
		if !slices.ContainsFunc(reviewed, func(entry reviewedTrigger) bool {
			return entry.table == table && entry.trigger == trigger
		}) {
			unreviewed = append(unreviewed, trigger)
		}
	}
	switch len(unreviewed) {
	case 0:
		return ""
	case 1:
		return fmt.Sprintf("%s has the enabled trigger %s, which is not on the census's reviewed list (reviewedTriggers): a trigger runs for each row a write writes and can read any table, and the census does not read its body", table, unreviewed[0])
	}
	return fmt.Sprintf("%s has the enabled triggers %s, which are not on the census's reviewed list (reviewedTriggers): a trigger runs for each row a write writes and can read any table, and the census does not read its body", table, strings.Join(unreviewed, ", "))
}

// statementKinds name, for a refusal, the kinds of statement that touch a table and are no bounded
// write, by their parse node; statementKind names any other by its node.
var statementKinds = map[string]string{
	"AlterTableStmt": "alter table",
	"CopyStmt":       "copy",
	"CreateStmt":     "create table",
	"CreateTrigStmt": "create trigger",
	"DeleteStmt":     "delete",
	"DoStmt":         "a DO block",
	"DropStmt":       "a drop",
	"IndexStmt":      "create index",
	"LockStmt":       "lock table",
	"MergeStmt":      "merge",
	"RenameStmt":     "a rename",
	"SelectStmt":     "select",
	"TruncateStmt":   "truncate",
}

func statementKind(node string) string {
	if kind, ok := statementKinds[node]; ok {
		return kind
	}
	return "a statement Postgres parses as " + node
}

// boundedTable is what Catalog read 8 finds of a table above the limit, by the oid the census found
// it by (boundedWrites).
type boundedTable struct {
	// primaryKey names the columns of its primary key, in the key's order, and keyTypes the type of
	// each; both empty where it has none, or where the key's index is not valid, since Postgres
	// does not search an index that is not.
	primaryKey []string
	keyTypes   []uint32
	// referenced names each of its columns that a foreign key references, its own or another
	// table's: an update that changes such a column checks the referencing table for rows that
	// still hold the old value, by whatever index that table has on its key, or by a scan.
	referenced []string
	// ruled, rowSecurity and children say whether it has a rule, row-level security and a child
	// table; triggers names its enabled triggers, sorted, without those a foreign key makes
	// (tgisinternal), whose checks CensusTables reads as the tables the key reaches.
	ruled, rowSecurity, children bool
	triggers                     []string
}

// boundedTableQuery, Catalog read 8, reads a table's boundedTable by its oid. It resolves no name.
const boundedTableQuery = `with key as (
		select a.attname::text as name, a.atttypid as type, k.ord
		from pg_constraint p
		join pg_index i on i.indexrelid = p.conindid and i.indisvalid
		cross join lateral unnest(p.conkey) with ordinality k(attnum, ord)
		join pg_attribute a on a.attrelid = p.conrelid and a.attnum = k.attnum
		where p.conrelid = $1::oid and p.contype = 'p'
	)
	select coalesce((select array_agg(name order by ord) from key), '{}'),
		coalesce((select array_agg(type order by ord) from key), '{}'),
		coalesce((select array_agg(distinct a.attname::text) from pg_constraint f
			cross join lateral unnest(f.confkey) k(attnum)
			join pg_attribute a on a.attrelid = f.confrelid and a.attnum = k.attnum
			where f.confrelid = $1::oid and f.contype = 'f'), '{}'),
		exists (select from pg_rewrite w where w.ev_class = r.oid),
		r.relrowsecurity,
		exists (select from pg_inherits h where h.inhparent = r.oid),
		coalesce((select array_agg(t.tgname::text order by t.tgname) from pg_trigger t
			where t.tgrelid = r.oid and not t.tgisinternal and t.tgenabled <> 'D'), '{}')
	from pg_class r where r.oid = $1::oid`

// writtenRelation reads the RangeVar of the table a bounded write writes: ok where it names table
// as the census reads a touched name (matchedNames folds it to lower case and takes public. off,
// so a name the statement quotes in another case is another table), inh where Postgres writes the
// table's children too (no ONLY), and the name a column reference may give the table by, its alias
// where it has one, since Postgres then refuses the table's own name.
func writtenRelation(raw json.RawMessage, table string) (inh bool, qualifier string, ok bool) {
	fields, ok := fieldsOnly(raw, "catalogname", "schemaname", "relname", "inh", "relpersistence", "alias", "location")
	var relation struct {
		rangeVar
		Inh   bool `json:"inh"`
		Alias *struct {
			Aliasname string `json:"aliasname"`
		} `json:"alias"`
	}
	if !ok || json.Unmarshal(raw, &relation) != nil || relation.Catalogname != "" {
		return false, "", false
	}
	named := relation.Relname
	if relation.Schemaname != "" && relation.Schemaname != "public" {
		named = relation.Schemaname + "." + named
	}
	if named != table {
		return false, "", false
	}
	qualifier = relation.Relname
	if relation.Alias != nil {
		if _, ok := fieldsOnly(fields["alias"], "aliasname"); !ok {
			return false, "", false
		}
		qualifier = relation.Alias.Aliasname
	}
	return relation.Inh, qualifier, true
}

// boundedInsert says why an InsertStmt is no bounded write of table, in words that follow "and",
// or "" where it is one (boundedWrites lists what one is).
func (w *writeReading) boundedInsert(body json.RawMessage, table string) (string, error) {
	const unread = "an insert the census cannot read is not one"
	insert, ok := fieldsOnly(body, "relation", "cols", "selectStmt", "onConflictClause", "override", "returningList", "withClause")
	if !ok {
		return unread, nil
	}
	if _, _, ok := writtenRelation(insert["relation"], table); !ok {
		return "an insert that names its table otherwise than the census reads " + table + " is not one", nil
	}
	switch {
	case insert["withClause"] != nil:
		return "an insert with a WITH is not one", nil
	case insert["returningList"] != nil:
		return "an insert with RETURNING is not one", nil
	case string(insert["override"]) != `"OVERRIDING_NOT_SET"`:
		return "an insert with OVERRIDING is not one", nil
	case insert["selectStmt"] == nil:
		return "an insert of DEFAULT VALUES is not one", nil
	}
	// A column list names whole columns: an array element or a field of one is refused.
	var cols []json.RawMessage
	if raw := insert["cols"]; raw != nil && json.Unmarshal(raw, &cols) != nil {
		return unread, nil
	}
	for _, col := range cols {
		kind, target, ok := nodeOf(col)
		if _, only := fieldsOnly(target, "name", "location"); !ok || kind != "ResTarget" || !only {
			return unread, nil
		}
	}
	// A conflict target, index elements with a predicate or a constraint's name, only names the
	// arbiter index: Postgres matches it against the indexes' own expressions and predicates when
	// it plans the insert and evaluates none of it on a row, and it refuses a subquery in either
	// part, as in an index's own expression. So a target of any expression, md5(to_id) and
	// where to_kind = 'value' among them, is read as no target is.
	if raw := insert["onConflictClause"]; raw != nil {
		conflict, ok := fieldsOnly(raw, "action", "infer", "location")
		if !ok || string(conflict["action"]) != `"ONCONFLICT_NOTHING"` {
			return "an insert whose ON CONFLICT does anything but DO NOTHING is not one", nil
		}
		if infer := conflict["infer"]; infer != nil {
			if _, ok := fieldsOnly(infer, "indexElems", "whereClause", "conname", "location"); !ok {
				return unread, nil
			}
		}
	}
	kind, query, ok := nodeOf(insert["selectStmt"])
	values, only := fieldsOnly(query, "valuesLists", "limitOption", "op")
	if !ok || kind != "SelectStmt" || !only || values["valuesLists"] == nil ||
		string(values["limitOption"]) != `"LIMIT_OPTION_DEFAULT"` || string(values["op"]) != `"SETOP_NONE"` {
		return "an insert whose rows come from a query, not from a VALUES list, is not one", nil
	}
	var rows []json.RawMessage
	if json.Unmarshal(values["valuesLists"], &rows) != nil {
		return unread, nil
	}
	if len(rows) > BoundedWriteRows {
		return fmt.Sprintf("an insert of %s rows, above the %s a bounded write may hold, is not one", Thousands(int64(len(rows))), Thousands(BoundedWriteRows)), nil
	}
	for _, row := range rows {
		var list struct {
			Items []json.RawMessage `json:"items"`
		}
		kind, body, ok := nodeOf(row)
		if _, only := fieldsOnly(body, "items"); !ok || kind != "List" || !only || json.Unmarshal(body, &list) != nil {
			return unread, nil
		}
		for _, item := range list.Items {
			if why, err := w.literal(item, "an insert whose values hold"); err != nil || why != "" {
				return why, err
			}
		}
	}
	return "", nil
}

// literal says why value, a value an insert gives or a value an update sets, is no literal, in
// words that follow what, or "" where it is one (boundedWrites says what one is).
func (w *writeReading) literal(value json.RawMessage, what string) (string, error) {
	if kind, _, ok := nodeOf(value); ok && kind == "SetToDefault" {
		return what + " DEFAULT is not one", nil
	}
	_, ok, err := w.c.defaultPartsOf(value)
	if err != nil {
		return "", err
	}
	if !ok {
		return what + " something other than a literal (a constant, a cast of one, a SQL value function or a call of no argument of a pg_catalog function) is not one", nil
	}
	return "", nil
}

// boundedUpdate says why an UpdateStmt is no bounded write of table, whose facts Catalog read 8
// read, in words that follow "and", or "" where it is one (boundedWrites lists what one is). Its
// set list assigns literals to columns that are neither of the primary key nor referenced by a
// foreign key, since a change of such a key checks the table that references it, and its WHERE
// clause names each column of the primary key once and nothing else (keyRows).
func (w *writeReading) boundedUpdate(body json.RawMessage, table string, facts boundedTable) (string, error) {
	const unread = "an update the census cannot read is not one"
	update, ok := fieldsOnly(body, "relation", "targetList", "whereClause", "fromClause", "returningList", "withClause")
	if !ok {
		return unread, nil
	}
	inh, qualifier, ok := writtenRelation(update["relation"], table)
	switch {
	case !ok:
		return "an update that names its table otherwise than the census reads " + table + " is not one", nil
	case update["withClause"] != nil:
		return "an update with a WITH is not one", nil
	case update["fromClause"] != nil:
		return "an update with a FROM is not one", nil
	case update["returningList"] != nil:
		return "an update with RETURNING is not one", nil
	case update["whereClause"] == nil:
		return "an update with no WHERE clause is not one", nil
	case len(facts.primaryKey) == 0:
		return "an update of a table with no primary key whose index Postgres searches is not one", nil
	case inh && facts.children:
		return "an update of a table with child tables, which it updates too, is not one", nil
	}
	key := strings.Join(facts.primaryKey, ", ")
	var targets []json.RawMessage
	if json.Unmarshal(update["targetList"], &targets) != nil {
		return unread, nil
	}
	for _, target := range targets {
		var column string
		kind, body, ok := nodeOf(target)
		set, only := fieldsOnly(body, "name", "val", "location")
		if !ok || kind != "ResTarget" || !only || json.Unmarshal(set["name"], &column) != nil {
			return "an update that sets part of a column (an array element or a field) is not one", nil
		}
		switch {
		case slices.Contains(facts.primaryKey, column):
			return fmt.Sprintf("an update that sets %s, a column of its primary key (%s), is not one", column, key), nil
		case slices.Contains(facts.referenced, column):
			return fmt.Sprintf("an update that sets %s, which a foreign key references, is not one", column), nil
		}
		if why, err := w.literal(set["val"], "an update that sets a column to"); err != nil || why != "" {
			return why, err
		}
	}
	columns, rows, ok := keyRows(update["whereClause"], qualifier)
	if !ok {
		return fmt.Sprintf("an update whose WHERE clause is not its primary key (%s) compared with = or IN to literals is not one", key), nil
	}
	if !sameColumns(columns, facts.primaryKey) {
		return fmt.Sprintf("an update by %s, which is not its primary key (%s), is not one", strings.Join(columns, ", "), key), nil
	}
	if len(rows) > BoundedWriteRows {
		return fmt.Sprintf("an update of %s rows, above the %s a bounded write may hold, is not one", Thousands(int64(len(rows))), Thousands(BoundedWriteRows)), nil
	}
	for _, row := range rows {
		for i, value := range row {
			ok, err := w.keyLiteral(value, facts.keyTypes[slices.Index(facts.primaryKey, columns[i])])
			if err != nil {
				return "", err
			}
			if !ok {
				return "an update whose key values are not literals its primary key's index is searched by (a string constant, an integer constant for an integer column, or a cast to the column's own type) is not one", nil
			}
		}
	}
	return "", nil
}

// keyRows reads an update's WHERE clause as the keys it names, ok false where it is none of four
// forms, each comparing a column of the table the update writes with =: column = value; column IN
// (value, …); (column, …) IN ((value, …), …); and column = value AND column = value …. columns
// are the columns the clause names, in its order, and rows the values each key gives them, in the
// same order. Postgres reads an IN of values as an = ANY of an array, and an IN of rows as an OR of
// the rows' equalities, which it searches the primary key's index for, one probe a key: on the
// measurement BoundedWriteRows holds, an index scan of graph_tuples_pkey and a BitmapOr of
// graph_nodes_pkey scans. NOT IN, <> and any other operator are refused.
func keyRows(where json.RawMessage, qualifier string) (columns []string, rows [][]json.RawMessage, ok bool) {
	kind, body, ok := nodeOf(where)
	if !ok {
		return nil, nil, false
	}
	if kind == "BoolExpr" {
		var and struct {
			Boolop string            `json:"boolop"`
			Args   []json.RawMessage `json:"args"`
		}
		if _, only := fieldsOnly(body, "boolop", "args", "location"); !only || json.Unmarshal(body, &and) != nil || and.Boolop != "AND_EXPR" {
			return nil, nil, false
		}
		var row []json.RawMessage
		for _, arg := range and.Args {
			kind, body, ok := nodeOf(arg)
			compare, equal := equality(body)
			if !ok || kind != "A_Expr" || !equal || compare.Kind != "AEXPR_OP" {
				return nil, nil, false
			}
			column, ok := columnOf(compare.Lexpr, qualifier)
			if !ok {
				return nil, nil, false
			}
			columns, row = append(columns, column), append(row, compare.Rexpr)
		}
		return columns, [][]json.RawMessage{row}, true
	}
	compare, equal := equality(body)
	if kind != "A_Expr" || !equal {
		return nil, nil, false
	}
	switch compare.Kind {
	case "AEXPR_OP":
		column, ok := columnOf(compare.Lexpr, qualifier)
		return []string{column}, [][]json.RawMessage{{compare.Rexpr}}, ok
	case "AEXPR_IN":
		items, ok := nodeFields(compare.Rexpr, "List", "items")
		if !ok {
			return nil, nil, false
		}
		if column, ok := columnOf(compare.Lexpr, qualifier); ok {
			for _, item := range items {
				rows = append(rows, []json.RawMessage{item})
			}
			return []string{column}, rows, true
		}
		args, ok := nodeFields(compare.Lexpr, "RowExpr", "args", "row_format")
		if !ok {
			return nil, nil, false
		}
		for _, arg := range args {
			column, ok := columnOf(arg, qualifier)
			if !ok {
				return nil, nil, false
			}
			columns = append(columns, column)
		}
		for _, item := range items {
			values, ok := nodeFields(item, "RowExpr", "args", "row_format")
			if !ok || len(values) != len(columns) {
				return nil, nil, false
			}
			rows = append(rows, values)
		}
		return columns, rows, true
	}
	return nil, nil, false
}

// comparison is an A_Expr of keyRows: its kind, and the two sides of its operator.
type comparison struct {
	Kind  string          `json:"kind"`
	Lexpr json.RawMessage `json:"lexpr"`
	Rexpr json.RawMessage `json:"rexpr"`
}

// equality reads an A_Expr's fields, ok false where it holds a field outside its kind, its
// operator, its two sides and its location, or where its operator is anything but an unqualified =.
func equality(body json.RawMessage) (compare comparison, ok bool) {
	var operator struct {
		Name nameList `json:"name"`
	}
	if _, only := fieldsOnly(body, "kind", "name", "lexpr", "rexpr", "location"); !only ||
		json.Unmarshal(body, &compare) != nil || json.Unmarshal(body, &operator) != nil ||
		len(operator.Name) != 1 || operator.Name[0].String.Sval != "=" {
		return comparison{}, false
	}
	return compare, true
}

// nodeFields reads a node of kind whose one list field is list, beside its location and the
// fields others names, as the list's items; ok false for any other node.
func nodeFields(raw json.RawMessage, kind, list string, others ...string) ([]json.RawMessage, bool) {
	node, body, ok := nodeOf(raw)
	fields, only := fieldsOnly(body, append([]string{list, "location"}, others...)...)
	var items []json.RawMessage
	if !ok || node != kind || !only || json.Unmarshal(fields[list], &items) != nil {
		return nil, false
	}
	return items, true
}

// columnOf reads a ColumnRef as the column it names: one part, or two whose first is qualifier,
// the name writtenRelation reads the table by. A star, a column of another relation and a name of
// three parts are refused.
func columnOf(raw json.RawMessage, qualifier string) (string, bool) {
	kind, body, ok := nodeOf(raw)
	fields, only := fieldsOnly(body, "fields", "location")
	var parts nameList
	if !ok || kind != "ColumnRef" || !only || json.Unmarshal(fields["fields"], &parts) != nil {
		return "", false
	}
	name := parts.identifier()
	if slices.Contains(name, "") {
		return "", false
	}
	switch {
	case len(name) == 1:
		return name[0], true
	case len(name) == 2 && name[0] == qualifier:
		return name[1], true
	}
	return "", false
}

// sameColumns says whether columns names each column of key once and no other.
func sameColumns(columns, key []string) bool {
	if len(columns) != len(key) {
		return false
	}
	for _, column := range key {
		if i := slices.Index(columns, column); i < 0 || slices.Contains(columns[i+1:], column) {
			return false
		}
	}
	return true
}

// keyLiteral says whether value, compared with = to a primary key column of type typ, is a literal
// that Postgres searches the key's index by. The key's index takes its columns' default btree
// operator class, as every primary key's does, and Postgres searches it only where the comparison's
// operator is of that class's family with the column bare: a comparison that casts the column
// instead (seq = 1.5 casts a bigint seq to numeric) scans the table. So a value is admitted where
// the operator is certain to be the column's own: a string constant or NULL, whose type is
// unknown until Postgres gives it the column's type; an integer constant for a column of type
// smallint, integer or bigint, whose family holds every comparison of the three; and a cast of a
// constant to the column's own type, which needs the migration's catalog (admittedType). A numeric
// or a boolean constant, a cast to any other type, and every other literal are refused.
func (w *writeReading) keyLiteral(value json.RawMessage, typ uint32) (bool, error) {
	kind, body, ok := nodeOf(value)
	if !ok {
		return false, nil
	}
	switch kind {
	case "A_Const":
		fields, ok := fieldsOnly(body, "ival", "fval", "sval", "bsval", "boolval", "isnull", "location")
		switch {
		case !ok:
			return false, nil
		case fields["sval"] != nil, fields["isnull"] != nil:
			return true, nil
		case fields["ival"] != nil:
			// int8, int2 and int4, in pg_type.
			return slices.Contains([]uint32{20, 21, 23}, typ), nil
		}
	case "TypeCast":
		cast, ok := fieldsOnly(body, "arg", "typeName", "location")
		if arg, _, read := nodeOf(cast["arg"]); !ok || !read || arg != "A_Const" {
			return false, nil
		}
		if _, ok := constant(cast["arg"]); !ok {
			return false, nil
		}
		t, ok, err := w.c.admittedType(cast["typeName"])
		return ok && t.oid == typ, err
	}
	return false, nil
}
