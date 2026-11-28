package graphstore

import (
	"cmp"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/pgmigrate"
	"github.com/sjawhar/envoy/internal/sqlscan"
)

// storeMigrations is the directory of the store's migrations, from the repository's root.
const storeMigrations = "packages/envoy/internal/dispatch/store/migrations"

// TestNoStatementDeletesANodeOrATuple holds the store to append-only (R1). Since the erase left,
// nothing in the tree takes a row out of graph_nodes or graph_tuples: a relation that ends is a
// second tuple with Asserted false, appended after the one that stated it, so every fact the
// store was told stays and a read as of any moment answers what was true then. A statement that
// deleted a row would rewrite that history.
//
// The test reads the source, never a database: every Go file under packages/ but the tests, and
// every .sql file under it - the store's migrations and every other SQL a Go file embeds. It fails
// on each delete from, truncate or drop table that names graph_nodes or graph_tuples
// (storeRowRemoval), naming the file and the line. A statement whose table name is built at run
// time counts too, where the scan can see the table the name holds: the strings a Go file builds
// with +, with its package's constants, with the exported constants of the packages of the tree
// it imports, with a function's local variables and with fmt's formatting functions are each read
// as the statement they make (sqlscan.ReadGo). Tests and the e2e drivers delete the fixture rows
// they wrote, so no _test.go file, no fixture under testdata and no e2e script is read. A drop
// trigger … on graph_nodes drops a trigger and no row, so it is no such statement:
// 20261109051232_graph_supertags drops two before it makes them again.
func TestNoStatementDeletesANodeOrATuple(t *testing.T) {
	requireTheScanFindsWhatItShould(t)

	root := sqlscan.Root(t)
	found := map[storeRemoval]bool{}
	tested := 0
	record := func(file string, value sqlscan.Value) {
		tested++
		for _, removal := range storeRemovalsIn(value) {
			removal.path = file
			found[removal] = true
		}
	}
	goFiles := sqlscan.ReadGo(t, root, "packages", record)
	migrations := 0
	sqlFiles := sqlscan.ReadSQL(t, root, "packages", func(file string, value sqlscan.Value) {
		if strings.HasPrefix(file, storeMigrations+"/") {
			migrations++
		}
		record(file, value)
	})

	removals := sortedRemovals(found)
	for _, removal := range removals {
		t.Errorf("%s:%d: %s: the store is append-only, so no statement takes a node or a tuple out of it; a relation that ends is a tuple with Asserted false appended after the one that stated it",
			removal.path, removal.line, removal.clause)
	}
	if goFiles < 1000 || migrations < 100 {
		t.Errorf("the scan read %d Go files and %d migrations under %s, too few to be the tree: it is reading the wrong place, and a scan of nothing proves nothing",
			goFiles, migrations, root)
	}
	t.Logf("the scan read %d Go files and %d SQL files under packages/, %d of them migrations under %s, and tested %d statements, each string a Go file holds or builds and each SQL file's text: %d delete a node or a tuple",
		goFiles, sqlFiles, migrations, storeMigrations, tested, len(removals))
}

// requireTheScanFindsWhatItShould runs the scan over statements and Go sources whose answers
// are known, so a scan that stopped seeing a removal fails here rather than passing the tree.
func requireTheScanFindsWhatItShould(t *testing.T) {
	t.Helper()
	for _, statement := range []struct {
		text, removes string
	}{
		{"delete from graph_nodes where kind = $1", "delete from graph_nodes"},
		{"DELETE FROM ONLY public.graph_tuples", "DELETE FROM ONLY public.graph_tuples"},
		{`truncate table only graph_events, "graph_tuples" restart identity`, `truncate table only graph_events, "graph_tuples"`},
		{"drop table if exists retired, graph_nodes cascade", "drop table if exists retired, graph_nodes"},
		{"delete /* the store */ from -- a note\n graph_tuples t using gone g", "delete from graph_tuples"},
		{"drop trigger if exists graph_nodes_block_schema_written on graph_nodes", ""},
		{"delete from graph_nodes_x", ""},
		{"do $$ begin execute 'delete from graph_nodes where kind = ''gone'''; end $$", "delete from graph_nodes"},
		{"insert into graph_tuples (kind) values ('a')", ""},
		{"update graph_nodes set state = 'gone'", ""},
		{"-- delete from graph_nodes", ""},
		// A -- comment ends at a carriage return, as Postgres's scan.l ends it, so the delete after
		// one is a statement.
		{"select 1 -- c\rdelete from graph_nodes", "delete from graph_nodes"},
		// A dollar quote's tag takes bytes 0x80-0xff, so the -- between its two tags is inside its
		// body and comments out nothing after the closing tag.
		{"select $é$ -- $é$; delete from graph_tuples", "delete from graph_tuples"},
		{`delete from "Graph_Nodes"`, ""},
	} {
		var clauses []string
		for _, removal := range storeRemovalsIn(sqlscan.Value{{Text: statement.text, Line: 1, Multiline: true}}) {
			clauses = append(clauses, removal.clause)
		}
		var want []string
		if statement.removes != "" {
			want = []string{statement.removes}
		}
		if !slices.Equal(clauses, want) {
			t.Errorf("the scan reads %q as the removals %q, want %q", statement.text, clauses, want)
		}
	}

	for _, sample := range []struct {
		sources []sqlscan.Source
		want    []storeRemoval
	}{{
		sources: []sqlscan.Source{{ImportPath: "example.com/sample", Name: "sample.go", Text: removalSample}},
		want: []storeRemoval{
			{"sample.go", 7, "delete from graph_tuples"},
			{"sample.go", 11, "truncate graph_nodes"},
			{"sample.go", 12, "delete from graph_tuples"},
			{"sample.go", 15, "drop table graph_nodes"},
		},
	}, {
		sources: removalPackages,
		want: []storeRemoval{
			{"tables.go", 5, "delete from graph_nodes"},
			{"writer.go", 8, "delete from graph_tuples"},
			{"writer.go", 11, "truncate graph_tuples"},
		},
	}} {
		found := map[storeRemoval]bool{}
		if err := sqlscan.ReadSources(sample.sources, func(file string, value sqlscan.Value) {
			for _, removal := range storeRemovalsIn(value) {
				removal.path = file
				found[removal] = true
			}
		}); err != nil {
			t.Fatalf("parse the sample: %v", err)
		}
		if removals := sortedRemovals(found); !slices.Equal(removals, sample.want) {
			t.Errorf("the scan finds the removals %v in the sample, want %v:\n%v", removals, sample.want, sample.sources)
		}
	}
}

// removalSample is a Go source whose removals are known: a constant's, a local variable's, a
// formatted statement's and a ranged name's, each built from a table name the scan can see. A
// parameter it cannot see, a drop trigger and an update remove nothing, and a constant used
// again is found where its statement is written, not a second time where it is used.
const removalSample = `package sample

import "fmt"

const tuples = "graph_tuples"

const removal = "delete from " + tuples

func run(kind string) {
	table := "graph_nodes"
	_ = "truncate " + table
	_ = fmt.Sprintf("delete from %s where kind = $1", tuples)
	_ = "delete from " + kind
	for _, name := range []string{"comments", "graph_nodes"} {
		_ = "drop table " + name
	}
	_ = "drop trigger if exists graph_nodes_written on graph_nodes"
	_ = "update graph_nodes set state = 'gone'"
	_ = removal + " where seq = 0"
}
`

// removalPackages are the Go sources of two packages whose removals are known: the second builds
// statements from the exported constants of the first, which it imports under the first's own
// name and under a name of its own, an exported constant's removal and one built from such a
// constant. A name the first does not declare is one the scan cannot see, and the first's
// removal used again is found where it is written, not a second time where it is used.
var removalPackages = []sqlscan.Source{{
	ImportPath: "example.com/tables", Name: "tables.go", Text: `package tables

const Tuples = "graph_tuples"

const Removal = "delete from graph_nodes"
`,
}, {
	ImportPath: "example.com/writer", Name: "writer.go", Text: `package writer

import (
	"example.com/tables"
	stored "example.com/tables"
)

const Clear = "delete from " + tables.Tuples

func run() {
	_ = "truncate " + stored.Tuples
	_ = "drop table " + tables.Nodes
	_ = tables.Removal + " where kind = $1"
}
`,
}}

// removalName is one name a statement writes: a SQL identifier (sqlIdentifier), or the
// placeholder the scan writes for a Go name whose value it cannot see.
const removalName = `(?:` + sqlIdentifier + `|` + sqlscan.PlaceholderPattern + `)`

// removalTable is a table as a statement names it, under its schema and its database where it
// names them, with a space or none round each dot.
const removalTable = removalName + `(?:\s*\.\s*` + removalName + `){0,2}`

// storeRowRemoval matches one statement that takes rows out of the store, in any case and over
// any whitespace: a delete from graph_nodes or graph_tuples, and a truncate or a drop table whose
// list of tables holds one of them. A table is named bare, quoted or under its schema; a quoted
// name is matched in its own case, as Postgres reads it. The name must end where the match does,
// so graph_nodes_x, and graph_nodes as the schema of another table, are not it. Group 1 is the
// clause, from its verb to the store's table. A drop trigger on graph_nodes, an insert into
// graph_tuples and an update of graph_nodes take no row out and are not matched.
var storeRowRemoval = regexp.MustCompile(`(?i)\b(` +
	`(?:delete\s+from\s+(?:only\s+)?` +
	`|truncate\s+(?:table\s+)?(?:(?:only\s+)?` + removalTable + `(?:\s*\*)?\s*,\s*)*(?:only\s+)?` +
	`|drop\s+table\s+(?:if\s+exists\s+)?(?:` + removalTable + `\s*,\s*)*)` +
	`(?:` + removalName + `\s*\.\s*){0,2}(?:graph_(?:nodes|tuples)|(?-i:"graph_(?:nodes|tuples)"))` +
	`)(?:\s*(?:[^a-z0-9_$.\s]|\z)|\s+[^.\s])`)

// storeRemoval is one statement found that takes rows out of the store: the file, from the
// repository's root, the line its verb is on, and the clause as the scan read it.
type storeRemoval struct {
	path   string
	line   int
	clause string
}

// sortedRemovals is every statement of found, in the order of their files and lines.
func sortedRemovals(found map[storeRemoval]bool) []storeRemoval {
	return slices.SortedFunc(maps.Keys(found), func(a, b storeRemoval) int {
		return cmp.Or(strings.Compare(a.path, b.path), cmp.Compare(a.line, b.line), strings.Compare(a.clause, b.clause))
	})
}

// storeRemovalsIn is every statement of value that takes rows out of the store, once its SQL
// comments are gone, each on the line of the piece its verb is in. A statement wholly inside a
// borrowed piece is left to the scan of the place that piece is written.
func storeRemovalsIn(value sqlscan.Value) []storeRemoval {
	starts := make([]int, len(value))
	length := 0
	for i, piece := range value {
		starts[i] = length
		length += len(piece.Text)
	}
	// pgmigrate.WithoutComments blanks each comment byte for byte, a line break kept, so an offset
	// into statement is one into the pieces, and reads a dollar quote's body as SQL too.
	statement := pgmigrate.WithoutComments(value.Text())
	var removals []storeRemoval
	for _, match := range storeRowRemoval.FindAllStringSubmatchIndex(statement, -1) {
		start, end := match[2], match[3]
		// The pieces lie in order, so the first that ends past start holds it.
		at := 0
		for start >= starts[at]+len(value[at].Text) {
			at++
		}
		piece := value[at]
		if piece.Borrowed && end <= starts[at]+len(piece.Text) {
			continue
		}
		removals = append(removals, storeRemoval{
			line:   piece.Slice(start-starts[at], start-starts[at]).Line,
			clause: strings.Join(strings.Fields(statement[start:end]), " "),
		})
	}
	return removals
}
