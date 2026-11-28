package store

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// The migrations of this child c-migration-size names, each by its file.
const (
	migrationSizeBlockNodeListed     = "20261125150000_issue_tree_block_node_listed.up.sql"
	migrationSizeEdgesBlockNodeList  = "20261126050009_issue_tree_graph_edges_block_node_listed.up.sql"
	migrationSizeAfterStoreClassLast = "20261126050008_issue_tree_graph_edges_after_store_class_last.up.sql"
	migrationSizeHead                = "20261127120803_issue_tree_head.up.sql"
	migrationSizeIssueChanges        = "20261124060000_issue_tree_issue_changes.up.sql"
	migrationSizeUnvoicedComments    = "20261124060100_issue_tree_unvoiced_comments.up.sql"
)

// migrationSizeName is a name as Postgres's parser gives it in a list: a String node.
type migrationSizeName struct {
	String struct {
		Value string `json:"sval"`
	} `json:"String"`
}

// migrationSizeNames are the values of a list of names, in order.
func migrationSizeNames(names []migrationSizeName) []string {
	values := make([]string, len(names))
	for index, name := range names {
		values[index] = name.String.Value
	}
	return values
}

// migrationSizeWords is a statement's text as the checks below compare it: the comment lines
// before it left out, lowercased, and its words one space apart (sourceText).
func migrationSizeWords(text string) string {
	return strings.ToLower(sourceText(text))
}

// migrationSizeTrigger is one of the seven triggers of the head migration as R8 defines it: its
// table, the events it fires on (insert, update, delete), the columns an update fires it for (none:
// any update), the function it calls, and its WHEN condition as the migration writes it, words one
// space apart. Every one is an AFTER ROW trigger.
type migrationSizeTrigger struct {
	table    string
	events   []string
	columns  []string
	function string
	when     string
}

// migrationSizeHeadRowsKept is the WHEN condition every head trigger holds: it skips a statement
// whose transaction claims dispatch.head_rows=kept.
const migrationSizeHeadRowsKept = "current_setting('dispatch.head_rows', true) is distinct from 'kept'"

// migrationSizeCommentFields are the comment fields comments_head_marks_update fires on, as its
// update columns name them, and as its WHEN compares them, old against new: anchor by the two
// values a head row reads of it.
var (
	migrationSizeCommentColumns = []string{"issue_key", "author", "body", "created_at", "edited_at", "deleted_at", "deleted_by",
		"reply_to", "replies_to_block", "ask_id", "artifact_id", "anchor", "written_version"}
	migrationSizeCommentValues = []string{"issue_key", "author", "body", "created_at", "edited_at", "deleted_at", "deleted_by",
		"reply_to", "replies_to_block", "ask_id", "artifact_id", "anchor->>'artifact_id'", "anchor->>'version'", "written_version"}
)

// migrationSizeChanged is the condition that a projected value changes: the old row's values are
// distinct from the new row's.
func migrationSizeChanged() string {
	side := func(row string) string {
		values := make([]string, len(migrationSizeCommentValues))
		for index, value := range migrationSizeCommentValues {
			values[index] = row + "." + value
		}
		return "(" + strings.Join(values, ", ") + ")"
	}
	return side("old") + " is distinct from " + side("new")
}

// migrationSizeHeadTriggers are the seven definitions R8 gives the head migration's triggers.
var migrationSizeHeadTriggers = map[string]migrationSizeTrigger{
	"graph_tuples_head_marks": {table: "graph_tuples", events: []string{"insert", "delete"},
		function: "issue_head_marks_drop", when: "(" + migrationSizeHeadRowsKept + ")"},
	"graph_nodes_head_marks": {table: "graph_nodes", events: []string{"insert", "update", "delete"},
		function: "issue_head_marks_drop", when: "(" + migrationSizeHeadRowsKept + ")"},
	"document_blocks_head_marks": {table: "document_blocks", events: []string{"insert", "update", "delete"},
		columns:  []string{"node_id", "artifact_id", "block_id", "type", "text_sha256", "changed_version"},
		function: "issue_head_marks_drop", when: "(" + migrationSizeHeadRowsKept + ")"},
	"comments_head_marks": {table: "comments", events: []string{"insert", "delete"},
		function: "issue_head_items_drop", when: "(" + migrationSizeHeadRowsKept + ")"},
	"comments_head_marks_update": {table: "comments", events: []string{"update"}, columns: migrationSizeCommentColumns,
		function: "issue_head_items_drop", when: "(" + migrationSizeHeadRowsKept + " and " + migrationSizeChanged() + ")"},
	"comment_blocks_head_marks": {table: "comment_blocks", events: []string{"insert", "update", "delete"},
		columns:  []string{"comment_id", "block_id", "node_id", "position", "excerpt", "made_at", "changed_revision", "changed_at"},
		function: "issue_head_items_drop", when: "(" + migrationSizeHeadRowsKept + ")"},
	"comment_block_texts_head_marks": {table: "comment_block_texts", events: []string{"insert", "update", "delete"},
		columns:  []string{"comment_id", "block_id", "revision", "text"},
		function: "issue_head_items_drop", when: "(" + migrationSizeHeadRowsKept + ")"},
}

// migrationSizeHeadFunctions are the two PL/pgSQL trigger functions the head migration creates.
var migrationSizeHeadFunctions = []string{"issue_head_marks_drop", "issue_head_items_drop"}

// The trigger bits of a CreateTrigStmt's timing and events (Postgres's pg_trigger.h).
const (
	migrationSizeTriggerBefore  = 1 << 1
	migrationSizeTriggerInsert  = 1 << 2
	migrationSizeTriggerDelete  = 1 << 3
	migrationSizeTriggerUpdate  = 1 << 4
	migrationSizeTriggerInstead = 1 << 6
)

// migrationSizeTriggerStatement is what the checks read of a create trigger's parse node.
type migrationSizeTriggerStatement struct {
	relationOf
	Name         string              `json:"trigname"`
	Function     []migrationSizeName `json:"funcname"`
	Row          bool                `json:"row"`
	Timing       int                 `json:"timing"`
	Events       int                 `json:"events"`
	Columns      []migrationSizeName `json:"columns"`
	When         json.RawMessage     `json:"whenClause"`
	Replace      bool                `json:"replace"`
	IsConstraint bool                `json:"isconstraint"`
}

// events names the events a trigger fires on, in the order insert, update, delete.
func (s migrationSizeTriggerStatement) events() []string {
	var events []string
	for _, event := range []struct {
		bit  int
		name string
	}{{migrationSizeTriggerInsert, "insert"}, {migrationSizeTriggerUpdate, "update"}, {migrationSizeTriggerDelete, "delete"}} {
		if s.Events&event.bit != 0 {
			events = append(events, event.name)
		}
	}
	if rest := s.Events &^ (migrationSizeTriggerInsert | migrationSizeTriggerUpdate | migrationSizeTriggerDelete); rest != 0 {
		events = append(events, fmt.Sprintf("events %d", rest))
	}
	return events
}

// migrationSizeWhen is the WHEN condition of a create trigger as its text writes it, words one
// space apart: what stands between `for each row when` and `execute`.
func migrationSizeWhen(words string) string {
	_, after, found := strings.Cut(words, " for each row when ")
	if !found {
		return ""
	}
	when, _, _ := strings.Cut(after, " execute ")
	return when
}

// migrationSizeFunctionStatement is what the checks read of a create function's parse node.
type migrationSizeFunctionStatement struct {
	Replace    bool                `json:"replace"`
	Name       []migrationSizeName `json:"funcname"`
	ReturnType struct {
		Names []migrationSizeName `json:"names"`
	} `json:"returnType"`
	Options []struct {
		DefElem struct {
			Name string `json:"defname"`
			Arg  struct {
				String struct {
					Value string `json:"sval"`
				} `json:"String"`
			} `json:"arg"`
		} `json:"DefElem"`
	} `json:"options"`
}

// language is the language a create function names.
func (s migrationSizeFunctionStatement) language() string {
	for _, option := range s.Options {
		if option.DefElem.Name == "language" {
			return option.DefElem.Arg.String.Value
		}
	}
	return ""
}

// migrationSizeIndexStatement is what the checks read of a create index's parse node.
type migrationSizeIndexStatement struct {
	relationOf
	Name   string `json:"idxname"`
	Params []struct {
		Element struct {
			Name string          `json:"name"`
			Expr json.RawMessage `json:"expr"`
		} `json:"IndexElem"`
	} `json:"indexParams"`
	Where       json.RawMessage `json:"whereClause"`
	Unique      bool            `json:"unique"`
	Concurrent  bool            `json:"concurrent"`
	IfNotExists bool            `json:"if_not_exists"`
}

// columns names the plain columns the index keys, "" for an expression.
func (s migrationSizeIndexStatement) columns() []string {
	columns := make([]string, len(s.Params))
	for index, param := range s.Params {
		columns[index] = param.Element.Name
	}
	return columns
}

// migrationSizeViewStatement is what the checks read of a create view's parse node.
type migrationSizeViewStatement struct {
	View struct {
		Schema string `json:"schemaname"`
		Name   string `json:"relname"`
	} `json:"view"`
	Replace bool `json:"replace"`
}

// migrationSizeCreateStatement is what the checks read of a create table's parse node: its columns
// and its table constraints.
type migrationSizeCreateStatement struct {
	relationOf
	Elements []struct {
		Column *struct {
			Name string `json:"colname"`
		} `json:"ColumnDef"`
		Constraint *struct {
			Type string              `json:"contype"`
			Keys []migrationSizeName `json:"keys"`
		} `json:"Constraint"`
	} `json:"tableElts"`
}

// columns names the table's columns, in order.
func (s migrationSizeCreateStatement) columns() []string {
	var columns []string
	for _, element := range s.Elements {
		if element.Column != nil {
			columns = append(columns, element.Column.Name)
		}
	}
	return columns
}

// primaryKey names the columns of the table's primary key where a table constraint declares it.
func (s migrationSizeCreateStatement) primaryKey() []string {
	for _, element := range s.Elements {
		if element.Constraint != nil && element.Constraint.Type == "CONSTR_PRIMARY" {
			return migrationSizeNames(element.Constraint.Keys)
		}
	}
	return nil
}

// migrationSizeReading is what migrationSizeRequireTheNamedMigrations reads of the set: each
// migration by its file, each one's census entry under the one-byte limit, and the findings.
type migrationSizeReading struct {
	t          *testing.T
	migrations map[string]pgmigrate.Migration
	census     map[string]pgmigrate.MigrationCensus
	findings   []string
}

func (r *migrationSizeReading) finding(format string, args ...any) {
	r.findings = append(r.findings, fmt.Sprintf(format, args...))
}

// statements are the statements of the migration name as Postgres's parser reads them, and false
// where the set holds no such migration or the parser cannot read it, each a finding.
func (r *migrationSizeReading) statements(name, why string) ([]sourceStatement, bool) {
	migration, held := r.migrations[name]
	if !held {
		r.finding("no migration of this child is %s: %s", name, why)
		return nil, false
	}
	statements, ok := parsedStatements(migration.SQL)
	if !ok {
		r.finding("%s does not parse as Postgres's parser reads a migration: %s", name, why)
		return nil, false
	}
	return statements, true
}

// decode reads statement's parse node into into, a finding naming the migration and the statement
// where it cannot.
func (r *migrationSizeReading) decode(name string, statement sourceStatement, into any) bool {
	if err := json.Unmarshal(statement.body, into); err != nil {
		r.finding("%s: the statement %q reads as no %s: %v", name, sourceText(statement.text), statement.node, err)
		return false
	}
	return true
}

// migrationSizeRequireTheNamedMigrations holds the migrations of this child that c-migration-size
// names to what it says of each, reading each statement as Postgres's parser reads it and report,
// the census of the set under the one-byte table limit, for what the census decides of each.
// Each finding names the migration and the statement.
//
//   - The migration that creates block_node_listed (R26) holds that create function alone, and
//     the migration that restates graph_edges to call it holds that create or replace view alone.
//   - 20261126050008_issue_tree_graph_edges_after_store_class_last holds one create or replace
//     view of graph_edges, which touches no table as TouchedTables reads a migration (R19).
//   - 20261127120803_issue_tree_head (R8, R19) creates issue_head_nodes with its primary key,
//     issue_head_nodes_node and issue_head_marks, exactly the two PL/pgSQL trigger functions
//     issue_head_marks_drop and issue_head_items_drop, and the seven AFTER ROW triggers on six tables
//     R8 defines, each with its events, its update columns, its function and its WHEN, which skips a
//     statement that claims dispatch.head_rows=kept, and each on pgmigrate's reviewed list
//     (pgmigrate.ReviewedTriggerNames); it writes no row, and the census exempts it as create
//     function and create trigger statements (catalogOnlyStatements).
//   - On comments the set's statements are the two create trigger statements of the head
//     migration, the alter table that adds node_anchor as nullable jsonb with no default, which the
//     census exempts as an alter table (catalogOnlyAlterTable), and its marked concurrent indexes,
//     each named; the set touches comments and changes its shape (TOUCHED: comments, SHAPE: comments).
//   - 20261124060000_issue_tree_issue_changes and 20261124060100_issue_tree_unvoiced_comments are
//     marked pgmigrate:non-transactional, each holds one create index concurrently if not exists of
//     the index named, and the census reads each a ConcurrentIndex; the set's one statement on
//     events is that index, and the set touches events (TOUCHED: events).
//   - issue_query_marks, document_rooms and version_renders are created new by a migration of the
//     set (R7, R19), and version_renders.digest_owed stands in version_renders' create table (R15),
//     each printed beside the migration that declares it.
func migrationSizeRequireTheNamedMigrations(t *testing.T, ours []pgmigrate.Migration, reading issueTreeReading, report *pgmigrate.Report) {
	t.Helper()
	r := &migrationSizeReading{t: t, migrations: map[string]pgmigrate.Migration{}, census: map[string]pgmigrate.MigrationCensus{}}
	for _, migration := range ours {
		r.migrations[migration.Name] = migration
	}
	for _, entry := range report.Pending {
		r.census[entry.Migration.Name] = entry
	}
	r.requireBlockNodeListed()
	r.requireAfterStoreClassLast()
	r.requireTheHead()
	r.requireComments(ours, reading)
	r.requireTheConcurrentIndexes(ours, reading)
	r.requireTheInventory(reading)
	for _, finding := range r.findings {
		t.Error(finding)
	}
}

// requireOneView holds the migration name to one statement, a create or replace view of
// graph_edges that names calls where it is not empty, which touches no table as TouchedTables reads
// a migration.
func (r *migrationSizeReading) requireOneView(name, why, calls string) {
	statements, ok := r.statements(name, why)
	if !ok {
		return
	}
	if touched := pgmigrate.TouchedTables(r.migrations[name].SQL); len(touched) != 0 {
		r.finding("%s touches %v as TouchedTables reads a migration, want no table: %s", name, touched, why)
	}
	views := 0
	for _, statement := range statements {
		var view migrationSizeViewStatement
		if statement.node != "ViewStmt" || !r.decode(name, statement, &view) || !view.Replace || view.View.Name != "graph_edges" ||
			(view.View.Schema != "" && view.View.Schema != "public") {
			r.finding("%s holds the statement %q (%s): %s", name, sourceText(statement.text), statement.node, why)
			continue
		}
		if calls != "" && !strings.Contains(migrationSizeWords(statement.text), calls+"(") {
			r.finding("%s restates graph_edges in the statement %q, which calls no %s: %s", name, sourceText(statement.text), calls, why)
		}
		views++
	}
	if views != 1 {
		r.finding("%s holds %d create or replace view statements of graph_edges, want one: %s", name, views, why)
	}
	r.t.Logf("%s: one create or replace view of graph_edges, touching no table", name)
}

// requireBlockNodeListed holds the migration that creates block_node_listed to that create
// function alone, and the migration that restates graph_edges to call it to that view alone (R26).
func (r *migrationSizeReading) requireBlockNodeListed() {
	const why = "the migration that creates block_node_listed holds its create function alone, and the view of graph_edges that calls it is restated by a migration of its own (R26)"
	creates := 0
	for _, name := range slices.Sorted(maps.Keys(r.migrations)) {
		statements, ok := parsedStatements(r.migrations[name].SQL)
		if !ok {
			continue
		}
		for _, statement := range statements {
			var function migrationSizeFunctionStatement
			if statement.node == "CreateFunctionStmt" && json.Unmarshal(statement.body, &function) == nil &&
				slices.Equal(migrationSizeNames(function.Name), []string{"block_node_listed"}) && name != migrationSizeBlockNodeListed {
				r.finding("%s creates block_node_listed in the statement %q: %s, which is %s", name, sourceText(statement.text), why, migrationSizeBlockNodeListed)
			}
		}
	}
	if statements, ok := r.statements(migrationSizeBlockNodeListed, why); ok {
		for _, statement := range statements {
			var function migrationSizeFunctionStatement
			if statement.node != "CreateFunctionStmt" || !r.decode(migrationSizeBlockNodeListed, statement, &function) ||
				!slices.Equal(migrationSizeNames(function.Name), []string{"block_node_listed"}) || function.language() != "sql" {
				r.finding("%s holds the statement %q (%s): %s", migrationSizeBlockNodeListed, sourceText(statement.text), statement.node, why)
				continue
			}
			creates++
		}
		if creates != 1 {
			r.finding("%s holds %d create function statements of block_node_listed, want one: %s", migrationSizeBlockNodeListed, creates, why)
		}
		if touched := pgmigrate.TouchedTables(r.migrations[migrationSizeBlockNodeListed].SQL); len(touched) != 0 {
			r.finding("%s touches %v as TouchedTables reads a migration, want no table: %s", migrationSizeBlockNodeListed, touched, why)
		}
		r.t.Logf("%s: one create function of block_node_listed (R26)", migrationSizeBlockNodeListed)
	}
	r.requireOneView(migrationSizeEdgesBlockNodeList, why, "block_node_listed")
}

// requireAfterStoreClassLast holds 20261126050008_issue_tree_graph_edges_after_store_class_last to
// one create or replace view of graph_edges, which touches no table (R19).
func (r *migrationSizeReading) requireAfterStoreClassLast() {
	r.requireOneView(migrationSizeAfterStoreClassLast,
		"it holds one create or replace view of graph_edges, which touches no table as TouchedTables reads a migration (R19)", "")
}

// requireTheHead holds 20261127120803_issue_tree_head to what R8 and R19 say it creates, and to
// writing no row.
func (r *migrationSizeReading) requireTheHead() {
	const name = migrationSizeHead
	const why = "the head migration creates issue_head_nodes with its primary key and issue_head_nodes_node, issue_head_marks, " +
		"the two PL/pgSQL trigger functions issue_head_marks_drop and issue_head_items_drop and the seven AFTER ROW triggers on six tables R8 defines, " +
		"and writes no row (R8, R19)"
	statements, ok := r.statements(name, why)
	if !ok {
		return
	}
	tables := map[string]int{}
	var indexes, functions []string
	triggers := map[string]bool{}
	triggerTables := map[string]bool{}
	for _, statement := range statements {
		text := sourceText(statement.text)
		switch statement.node {
		case "CreateStmt":
			var create migrationSizeCreateStatement
			if !r.decode(name, statement, &create) {
				continue
			}
			switch {
			case create.is("issue_head_nodes"):
				if key := create.primaryKey(); !slices.Equal(key, []string{"issue_key", "node_kind", "node_address"}) {
					r.finding("%s creates issue_head_nodes with the primary key %v in the statement %q, want (issue_key, node_kind, node_address): %s", name, key, text, why)
				}
				tables["issue_head_nodes"]++
			case create.is("issue_head_marks"):
				tables["issue_head_marks"]++
			default:
				r.finding("%s creates the table %s in the statement %q: %s", name, create.Relation.Name, text, why)
			}
		case "IndexStmt":
			var index migrationSizeIndexStatement
			if !r.decode(name, statement, &index) {
				continue
			}
			if index.Name != "issue_head_nodes_node" || !index.is("issue_head_nodes") || index.Unique || index.Concurrent ||
				!slices.Equal(index.columns(), []string{"node_kind", "node_address"}) || len(index.Where) != 0 {
				r.finding("%s holds the index statement %q: %s, its one index issue_head_nodes_node on issue_head_nodes (node_kind, node_address)", name, text, why)
				continue
			}
			indexes = append(indexes, index.Name)
		case "CreateFunctionStmt":
			var function migrationSizeFunctionStatement
			if !r.decode(name, statement, &function) {
				continue
			}
			named := strings.Join(migrationSizeNames(function.Name), ".")
			if !slices.Contains(migrationSizeHeadFunctions, named) || function.Replace || function.language() != "plpgsql" ||
				!slices.Equal(migrationSizeNames(function.ReturnType.Names), []string{"trigger"}) || slices.Contains(functions, named) {
				r.finding("%s holds the function statement %q (%s, language %q, returns %v): %s", name, text, named, function.language(),
					migrationSizeNames(function.ReturnType.Names), why)
				continue
			}
			functions = append(functions, named)
		case "CreateTrigStmt":
			var trigger migrationSizeTriggerStatement
			if !r.decode(name, statement, &trigger) {
				continue
			}
			r.requireHeadTrigger(trigger, migrationSizeWords(statement.text), text)
			triggers[trigger.Name] = true
			triggerTables[trigger.Relation.Name] = true
		default:
			r.finding("%s holds the statement %q (%s): %s", name, text, statement.node, why)
		}
	}
	if tables["issue_head_nodes"] != 1 || tables["issue_head_marks"] != 1 {
		r.finding("%s creates issue_head_nodes %d time(s) and issue_head_marks %d time(s), want each once: %s", name, tables["issue_head_nodes"], tables["issue_head_marks"], why)
	}
	if len(indexes) != 1 {
		r.finding("%s creates the indexes %v, want issue_head_nodes_node alone: %s", name, indexes, why)
	}
	if slices.Sort(functions); !slices.Equal(functions, slices.Sorted(slices.Values(migrationSizeHeadFunctions))) {
		r.finding("%s creates the trigger functions %v, want exactly %v: %s", name, functions, migrationSizeHeadFunctions, why)
	}
	for _, trigger := range slices.Sorted(maps.Keys(migrationSizeHeadTriggers)) {
		if !triggers[trigger] {
			r.finding("%s holds no trigger %s on %s, which R8 defines: %s", name, trigger, migrationSizeHeadTriggers[trigger].table, why)
		}
	}
	if len(triggers) != 7 || len(triggerTables) != 6 {
		r.finding("%s creates %d trigger(s) on %d table(s), want seven on six: %s", name, len(triggers), len(triggerTables), why)
	}
	entry, held := r.census[name]
	switch {
	case !held:
		r.finding("the census under a %d byte table limit holds no entry of %s: %s", issueTreeTableLimit, name, why)
	case !strings.Contains(entry.TableSizeExemption, "create function") || !strings.Contains(entry.TableSizeExemption, "create trigger"):
		r.finding("the census under a %d byte table limit exempts %s as %q, want its create function and create trigger statements admitted by catalogOnlyStatements: %s",
			issueTreeTableLimit, name, entry.TableSizeExemption, why)
	}
	r.t.Logf("%s: issue_head_nodes with its primary key, %v, issue_head_marks, the functions %v and the triggers %v on %v; the census exempts it as %s",
		name, indexes, functions, slices.Sorted(maps.Keys(triggers)), slices.Sorted(maps.Keys(triggerTables)), entry.TableSizeExemption)
}

// requireHeadTrigger holds one create trigger of the head migration, words its text lowercased
// with its words one space apart and text as a finding quotes it, to its definition in R8.
func (r *migrationSizeReading) requireHeadTrigger(trigger migrationSizeTriggerStatement, words, text string) {
	const name = migrationSizeHead
	want, defined := migrationSizeHeadTriggers[trigger.Name]
	if !defined {
		r.finding("%s creates the trigger %s on %s in the statement %q, which R8 does not define", name, trigger.Name, trigger.Relation.Name, text)
		return
	}
	var differs []string
	if !trigger.is(want.table) {
		differs = append(differs, fmt.Sprintf("it is on %s, want %s", trigger.Relation.Name, want.table))
	}
	if !trigger.Row || trigger.Timing&(migrationSizeTriggerBefore|migrationSizeTriggerInstead) != 0 || trigger.Replace || trigger.IsConstraint {
		differs = append(differs, "it is no plain AFTER ROW trigger")
	}
	if events := trigger.events(); !slices.Equal(events, want.events) {
		differs = append(differs, fmt.Sprintf("it fires on %v, want %v", events, want.events))
	}
	if columns := migrationSizeNames(trigger.Columns); !slices.Equal(columns, want.columns) {
		differs = append(differs, fmt.Sprintf("an update fires it for the columns %v, want %v", columns, want.columns))
	}
	if function := migrationSizeNames(trigger.Function); !slices.Equal(function, []string{want.function}) {
		differs = append(differs, fmt.Sprintf("it calls %v, want %s", function, want.function))
	}
	if when := migrationSizeWhen(words); when != want.when {
		differs = append(differs, fmt.Sprintf("its WHEN reads %q, want %q, which skips a statement that claims dispatch.head_rows=kept", when, want.when))
	}
	if !slices.Contains(pgmigrate.ReviewedTriggerNames(want.table), trigger.Name) {
		differs = append(differs, fmt.Sprintf("pgmigrate's reviewed list (bounded_writes.go) names no trigger %s on %s", trigger.Name, want.table))
	}
	for _, difference := range differs {
		r.finding("%s: the trigger %s on table %s in the statement %q differs from its definition in R8: %s", name, trigger.Name, trigger.Relation.Name, text, difference)
	}
}

// migrationSizeCommentIndexes are the marked concurrent indexes the set builds on comments, each
// by its migration.
var migrationSizeCommentIndexes = map[string]string{
	"comments_issue_unvoiced":    migrationSizeUnvoicedComments,
	"comments_node_anchor_node":  "20261125140001_issue_tree_comments_node_anchor_node.up.sql",
	"comments_issue_node_anchor": "20261125140002_issue_tree_comments_node_anchor_issue.up.sql",
}

// migrationSizeNodeAnchor is the column the set adds to comments (R31): node_anchor, nullable jsonb
// with no default.
var migrationSizeNodeAnchor = columnAdd{what: "the nullable jsonb column node_anchor", columns: map[string][]string{"node_anchor": {"jsonb"}}}

// requireComments holds the set's statements on comments to the two create trigger statements of
// the head migration, the add of node_anchor, which the census exempts as an alter table, and the
// marked concurrent indexes of migrationSizeCommentIndexes.
func (r *migrationSizeReading) requireComments(ours []pgmigrate.Migration, reading issueTreeReading) {
	const why = "the statements of this child on comments are the two create trigger statements of the head migration, " +
		"the alter table that adds node_anchor as nullable jsonb with no default (R31) and its marked concurrent indexes"
	alters, indexes := map[string]string{}, map[string]string{}
	triggers := map[string]int{}
	for _, migration := range ours {
		statements, ok := parsedStatements(migration.SQL)
		if !ok {
			continue
		}
		for _, statement := range statements {
			if !actsOn(statement.text, "comments") {
				continue
			}
			text := sourceText(statement.text)
			switch statement.node {
			case "AlterTableStmt":
				if !addsColumns(statement.text, "comments", migrationSizeNodeAnchor) {
					r.finding("%s alters comments in the statement %q, which is no add of node_anchor as nullable jsonb with no default: %s", migration.Name, text, why)
					continue
				}
				alters[migration.Name] = text
			case "CreateTrigStmt":
				var trigger migrationSizeTriggerStatement
				if !r.decode(migration.Name, statement, &trigger) {
					continue
				}
				if migration.Name != migrationSizeHead || (trigger.Name != "comments_head_marks" && trigger.Name != "comments_head_marks_update") {
					r.finding("%s creates the trigger %s on comments in the statement %q: %s", migration.Name, trigger.Name, text, why)
					continue
				}
				triggers[trigger.Name]++
			case "IndexStmt":
				var index migrationSizeIndexStatement
				if !r.decode(migration.Name, statement, &index) {
					continue
				}
				entry := r.census[migration.Name]
				if migrationSizeCommentIndexes[index.Name] != migration.Name || !index.Concurrent || !migration.NonTransactional || !entry.ConcurrentIndex {
					r.finding("%s builds the index %s on comments in the statement %q, which is not one of its marked concurrent indexes %v that the census reads a ConcurrentIndex (ConcurrentIndex %t): %s",
						migration.Name, index.Name, text, slices.Sorted(maps.Keys(migrationSizeCommentIndexes)), entry.ConcurrentIndex, why)
					continue
				}
				indexes[index.Name] = migration.Name
			default:
				r.finding("%s touches comments in the statement %q (%s): %s", migration.Name, text, statement.node, why)
			}
		}
	}
	if triggers["comments_head_marks"] != 1 || triggers["comments_head_marks_update"] != 1 {
		r.finding("the set creates comments_head_marks %d time(s) and comments_head_marks_update %d time(s), want each once: %s",
			triggers["comments_head_marks"], triggers["comments_head_marks_update"], why)
	}
	if len(alters) != 1 {
		r.finding("the set adds node_anchor to comments in %d migration(s) %v, want one alter table: %s", len(alters), slices.Sorted(maps.Keys(alters)), why)
	}
	for migration, text := range alters {
		entry, held := r.census[migration]
		if !held || !strings.Contains(entry.TableSizeExemption, "alter table") {
			r.finding("%s: the census under a %d byte table limit does not exempt the statement %q as an alter table (catalogOnlyAlterTable): it exempts the migration as %q",
				migration, issueTreeTableLimit, text, entry.TableSizeExemption)
		}
		r.t.Logf("comments: %s adds node_anchor, nullable jsonb with no default, exempt as %s (R31)", migration, entry.TableSizeExemption)
	}
	for index, migration := range migrationSizeCommentIndexes {
		if indexes[index] != migration {
			r.finding("%s builds no marked concurrent index %s on comments: %s", migration, index, why)
		}
	}
	if !slices.Contains(reading.touched, "comments") || !slices.Contains(reading.shaped, "comments") {
		r.finding("the set's TOUCHED lines hold comments %t and its SHAPE lines %t, want both: %s",
			slices.Contains(reading.touched, "comments"), slices.Contains(reading.shaped, "comments"), why)
	}
	r.t.Logf("comments: the triggers %v, the add of node_anchor by %v and the marked concurrent indexes %v",
		slices.Sorted(maps.Keys(triggers)), slices.Sorted(maps.Keys(alters)), indexes)
}

// migrationSizeConcurrentIndex is one of the two marked concurrent index migrations c-migration-size
// requires: the index, its table, its columns and the predicate its where writes, words one space
// apart.
type migrationSizeConcurrentIndex struct {
	index, table string
	columns      []string
	where        string
}

var migrationSizeConcurrentIndexes = map[string]migrationSizeConcurrentIndex{
	migrationSizeIssueChanges: {index: "events_issue_changes", table: "events", columns: []string{"issue_key", "seq"},
		where: "where type = any('{issue.created,issue.updated,issue.closed}')"},
	migrationSizeUnvoicedComments: {index: "comments_issue_unvoiced", table: "comments", columns: []string{"issue_key", "created_at"},
		where: "where (author->>'kind' = 'user') is not true"},
}

// requireTheConcurrentIndexes holds the two marked concurrent index migrations to one create index
// concurrently if not exists each, of the index named, which the census reads a ConcurrentIndex,
// and the set's statements on events to that one index.
func (r *migrationSizeReading) requireTheConcurrentIndexes(ours []pgmigrate.Migration, reading issueTreeReading) {
	for _, name := range slices.Sorted(maps.Keys(migrationSizeConcurrentIndexes)) {
		want := migrationSizeConcurrentIndexes[name]
		why := fmt.Sprintf("%s is marked pgmigrate:non-transactional and holds one create index concurrently if not exists of %s on %s (%s) %s",
			name, want.index, want.table, strings.Join(want.columns, ", "), want.where)
		statements, ok := r.statements(name, why)
		if !ok {
			continue
		}
		if !r.migrations[name].NonTransactional {
			r.finding("%s is not marked pgmigrate:non-transactional: %s", name, why)
		}
		for _, statement := range statements {
			var index migrationSizeIndexStatement
			if statement.node != "IndexStmt" || !r.decode(name, statement, &index) || index.Name != want.index || !index.is(want.table) ||
				!index.Concurrent || !index.IfNotExists || index.Unique || !slices.Equal(index.columns(), want.columns) ||
				!strings.Contains(migrationSizeWords(statement.text), want.where) {
				r.finding("%s holds the statement %q (%s): %s", name, sourceText(statement.text), statement.node, why)
			}
		}
		if len(statements) != 1 {
			r.finding("%s holds %d statements: %s", name, len(statements), why)
		}
		if entry := r.census[name]; !entry.ConcurrentIndex {
			r.finding("the census under a %d byte table limit reads %s no ConcurrentIndex: %s", issueTreeTableLimit, name, why)
		}
		r.t.Logf("%s: the marked concurrent index %s on %s (%s), a ConcurrentIndex to the census", name, want.index, want.table, strings.Join(want.columns, ", "))
	}
	const why = "the one statement of this child on events is the marked concurrent index events_issue_changes"
	for _, migration := range ours {
		for _, statement := range pgmigrate.CensusStatements(migration.SQL) {
			if actsOn(statement, "events") && migration.Name != migrationSizeIssueChanges {
				r.finding("%s touches events in the statement %q: %s", migration.Name, statement, why)
			}
		}
	}
	if !slices.Contains(reading.touched, "events") {
		r.finding("the set's TOUCHED lines hold no events: %s", why)
	}
}

// requireTheInventory holds the set to creating issue_query_marks (R7), document_rooms and
// version_renders (R19) new, and to declaring version_renders.digest_owed in version_renders'
// create table (R15), and prints each beside the migration that declares it.
func (r *migrationSizeReading) requireTheInventory(reading issueTreeReading) {
	for _, table := range []string{"issue_query_marks", "document_rooms", "version_renders"} {
		migration, created := reading.created[table]
		if !created {
			r.finding("no migration of this child creates %s new, by a create table of its own that holds no row: the migration inventory holds it as a new table (R7, R19)", table)
			continue
		}
		r.t.Logf("inventory: %s, a new table, created by %s", table, migration)
	}
	name, created := reading.created["version_renders"]
	if !created {
		return
	}
	const why = "version_renders.digest_owed stands in the definition of version_renders (R15)"
	statements, ok := r.statements(name, why)
	if !ok {
		return
	}
	for _, statement := range statements {
		var create migrationSizeCreateStatement
		if statement.node != "CreateStmt" || json.Unmarshal(statement.body, &create) != nil || !create.is("version_renders") {
			continue
		}
		if !slices.Contains(create.columns(), "digest_owed") {
			r.finding("%s creates version_renders in the statement %q with the columns %v, which hold no digest_owed: %s", name, sourceText(statement.text), create.columns(), why)
			return
		}
		r.t.Logf("inventory: version_renders.digest_owed, declared in the create table of version_renders by %s", name)
		return
	}
	r.finding("%s holds no create table of version_renders: %s", name, why)
}
