package pgmigrate

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// RehearsalStep is what a rehearsal of a change at a later boot does with one pending migration:
// the pre-deploy census of Dispatch rehearses the boot's move of the priority store and the
// workflow store (internal/dispatch/storesmove, dispatch://INFRA-1098's R32) inside one
// transaction that it rolls back, over a database whose pending migrations have not run. It runs
// the migrations the move needs in that transaction (RehearsalApplies), leaves out the ones that
// change nothing the move reads (RehearsalSkips), and refuses to rehearse over any other
// (RehearsalRefuses), since it cannot tell what the move would meet once that one ran.
type RehearsalStep int

const (
	RehearsalApplies RehearsalStep = iota + 1
	RehearsalSkips
	RehearsalRefuses
)

func (s RehearsalStep) String() string {
	switch s {
	case RehearsalApplies:
		return "applies"
	case RehearsalSkips:
		return "skips"
	case RehearsalRefuses:
		return "refuses"
	}
	return fmt.Sprintf("RehearsalStep(%d)", int(s))
}

// RehearseOwn says what a rehearsal does with migration, a pending migration of the tree whose
// change it rehearses, and why, in words that follow its name; tables are the tables that change
// reads and writes. As Postgres's own parser reads its statements (parseStatements), it applies a
// transactional migration made of these alone:
//
//   - create table, of a table that is neither temporary nor joined to another (INHERITS,
//     PARTITION OF), which holds no row;
//   - create index, not concurrently, on a table an earlier create table of the migration made,
//     that create table written without IF NOT EXISTS, so the build reads that new table's rows
//     alone, which are none;
//   - an insert of a VALUES list of literal rows the census admits as a bounded write
//     (boundedInsert, read with no catalog, so a cast or a call is no literal).
//
// It skips a concurrent index build (Migration.NonTransactional), which no transaction may run, a
// migration of extended statistics statements alone (create statistics and alter statistics … set
// statistics), which change no row the move reads and populate nothing until an ANALYZE, and a
// migration of view replacements (create or replace view), which would hold access exclusive on
// its view against the serving release's reads, where each of its other statements is an insert,
// an update or a delete of a table none of tables (TouchedTables), whose rows the change reads
// nothing of. It refuses every other migration, naming its first statement of another kind, and
// one that mixes what it applies with what it skips, since it can neither run such a migration
// whole nor leave all of it out.
func RehearseOwn(migration Migration, tables []string) (RehearsalStep, string) {
	if migration.NonTransactional {
		return RehearsalSkips, "a concurrent index build, which no transaction may run"
	}
	statements, ok := parseStatements(migration.SQL)
	if !ok || len(statements) == 0 {
		return RehearsalRefuses, "Postgres's parser reads no statement of it"
	}
	// A literal is judged without the catalog (defaultPartsOf), so a value that casts or calls is
	// none, which only refuses more.
	reading := &writeReading{c: &classifier{ctx: context.Background()}}
	var created []rangeVar
	applies, replaces, elsewhere, stats := 0, 0, 0, 0
	for _, s := range statements {
		var why string
		switch s.node {
		case "CreateStmt":
			why = createsTable(s.body, &created)
			applies++
		case "IndexStmt":
			why = indexesCreatedTable(s.body, created)
			applies++
		case "ViewStmt":
			var view struct {
				Replace bool `json:"replace"`
			}
			if json.Unmarshal(s.body, &view) != nil || !view.Replace {
				why = "a create view that replaces no view"
			}
			replaces++
		case "CreateStatsStmt", "AlterStatsStmt":
			stats++
		case "InsertStmt", "UpdateStmt", "DeleteStmt":
			if s.node == "InsertStmt" && insertsBounded(reading, s.body) == "" {
				applies++
				break
			}
			if touched := touchedOf(s.text, tables); len(touched) > 0 {
				why = map[string]string{"InsertStmt": "an insert", "UpdateStmt": "an update", "DeleteStmt": "a delete"}[s.node] +
					" of " + strings.Join(touched, ", ") + " that is no bounded insert"
			}
			elsewhere++
		default:
			why = statementKind(s.node)
		}
		if why != "" {
			return RehearsalRefuses, "it holds " + why + ", which the rehearsal neither runs nor leaves out"
		}
	}
	switch {
	case stats > 0 && (applies > 0 || replaces > 0 || elsewhere > 0):
		return RehearsalRefuses, "it holds both extended statistics statements it leaves out and other statements, so the rehearsal can neither run it whole nor leave it out"
	case stats > 0:
		return RehearsalSkips, "extended statistics statements, which change no row the rehearsal reads"
	case replaces > 0 && applies > 0:
		return RehearsalRefuses, "it holds both statements the rehearsal runs and view replacements it leaves out, so the rehearsal can neither run it whole nor leave it out"
	case replaces > 0:
		return RehearsalSkips, "view replacements, beside writes of no table the rehearsal reads"
	case elsewhere > 0:
		return RehearsalRefuses, "it holds a write that is no bounded insert, with no view replacement, which the rehearsal neither runs nor leaves out"
	}
	return RehearsalApplies, "transactional create table, create index on a table it creates and bounded inserts alone"
}

// TriggerFunction is one trigger of a table and the name of the function it runs, the function
// pg_trigger.tgfoid names.
type TriggerFunction struct{ Table, Function string }

// RehearseOther says what a rehearsal does with migration, a pending migration of another tree
// than the one whose change it rehearses, and why, in words that follow its name: it skips one
// that touches none of tables, the tables the rehearsed change reads and writes, and refuses one
// that touches any of them, since it does not run such a migration and cannot tell what the change
// would meet once it ran. A migration touches each table TouchedTables names for it, and each table
// of triggers whose function it creates or replaces, by the function's name: TouchedTables reads
// no function's body and no create function, so a migration that only replaces the body of a
// trigger's function changes what each write of that table runs and touches no table it names. The
// rule reads the trigger's own function alone, not a function that one calls. A migration whose
// statements Postgres's parser does not read is refused, since its functions cannot be read.
func RehearseOther(migration Migration, tables []string, triggers []TriggerFunction) (RehearsalStep, string) {
	touched := touchedOf(migration.SQL, tables)
	statements, ok := parseStatements(migration.SQL)
	if !ok {
		return RehearsalRefuses, "Postgres's parser does not read it, so the rehearsal cannot read which functions it replaces"
	}
	var replaced []string
	for _, s := range statements {
		if s.node != "CreateFunctionStmt" {
			continue
		}
		var create struct {
			Funcname nameList `json:"funcname"`
		}
		if json.Unmarshal(s.body, &create) != nil || len(create.Funcname) == 0 {
			return RehearsalRefuses, "it holds a create function the rehearsal cannot read"
		}
		name := create.Funcname[len(create.Funcname)-1].String.Sval
		for _, trigger := range triggers {
			if trigger.Function == name && slices.Contains(tables, trigger.Table) && !slices.Contains(touched, trigger.Table) {
				touched = append(touched, trigger.Table)
				replaced = append(replaced, name+" of a trigger on "+trigger.Table)
			}
		}
	}
	switch {
	case len(replaced) > 0:
		return RehearsalRefuses, "it touches " + strings.Join(touched, ", ") + ", which the rehearsal reads and writes, and replaces the function " + strings.Join(replaced, ", ")
	case len(touched) > 0:
		return RehearsalRefuses, "it touches " + strings.Join(touched, ", ") + ", which the rehearsal reads and writes"
	}
	return RehearsalSkips, "it touches none of " + strings.Join(tables, ", ")
}

// touchedOf is the tables of tables that TouchedTables names for sql, in TouchedTables' order.
func touchedOf(sql string, tables []string) []string {
	var touched []string
	for _, table := range TouchedTables(sql) {
		if slices.Contains(tables, table) {
			touched = append(touched, table)
		}
	}
	return touched
}

// createsTable says why a CreateStmt is no create table the rehearsal runs, or "" where it is one,
// and records the table it creates, written without IF NOT EXISTS, in created (catalogOnlyIndex
// keeps the same record).
func createsTable(body json.RawMessage, created *[]rangeVar) string {
	var create struct {
		Relation struct {
			rangeVar
			Relpersistence string `json:"relpersistence"`
		} `json:"relation"`
		InhRelations json.RawMessage `json:"inhRelations"`
		Partbound    json.RawMessage `json:"partbound"`
		IfNotExists  bool            `json:"if_not_exists"`
	}
	switch {
	case json.Unmarshal(body, &create) != nil || create.Relation.Relname == "":
		return "a create table it cannot read"
	case create.InhRelations != nil || create.Partbound != nil:
		return "a create table that joins its table to another (INHERITS, PARTITION OF)"
	case create.Relation.Relpersistence == "t":
		return "a create of a temporary table"
	}
	if !create.IfNotExists {
		*created = append(*created, create.Relation.rangeVar)
	}
	return ""
}

// indexesCreatedTable says why an IndexStmt is no index the rehearsal builds, or "" where it is one
// on a table an earlier create table of the same migration made (created).
func indexesCreatedTable(body json.RawMessage, created []rangeVar) string {
	var index struct {
		Relation   rangeVar `json:"relation"`
		Concurrent bool     `json:"concurrent"`
	}
	switch {
	case json.Unmarshal(body, &index) != nil:
		return "a create index it cannot read"
	case index.Concurrent:
		return "a concurrent index build inside a transaction"
	case !slices.Contains(created, index.Relation):
		return "a create index on " + index.Relation.sanitized() + ", which the migration does not create"
	}
	return ""
}

// insertsBounded says why an InsertStmt is no bounded write of the table it names, or "" where it
// is one (boundedInsert).
func insertsBounded(reading *writeReading, body json.RawMessage) string {
	var insert struct {
		Relation rangeVar `json:"relation"`
	}
	if json.Unmarshal(body, &insert) != nil || insert.Relation.Relname == "" || insert.Relation.Catalogname != "" {
		return "an insert it cannot read"
	}
	table := insert.Relation.Relname
	if schema := insert.Relation.Schemaname; schema != "" && schema != "public" {
		table = schema + "." + table
	}
	why, err := reading.boundedInsert(body, table)
	switch {
	case err != nil:
		return "an insert into " + table + " it cannot read"
	case why != "":
		return "an insert into " + table + " that is no bounded write: " + why
	}
	return ""
}
