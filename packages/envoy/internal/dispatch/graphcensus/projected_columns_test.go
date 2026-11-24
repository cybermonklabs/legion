package graphcensus

import (
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus/censustest"
)

// projectedTables are the tables migration 20261109022248 made projections of the node and
// tuple store: the tables censustest.ProjectedColumns lists, in order, so a table added there is
// one this test reads the census's statements for.
var projectedTables = slices.Sorted(maps.Keys(censustest.ProjectedColumns))

// censusTableRead matches where a statement reads a projected table (projectedTables), under the
// schema it may be qualified by, with the word after it: the alias the statement gives the table,
// or a word of the clause that follows a table given none (unaliasedTableWords).
var censusTableRead = regexp.MustCompile(`(?i)\b(?:from|join)\s+(?:only\s+)?(?:\w+\.)?(` + literalAlternation(projectedTables) +
	`)\b(?:\s+(?:as\s+)?([a-z_][a-z0-9_]*))?`)

// literalAlternation is a regular expression matching any one of words, each read literally.
func literalAlternation(words []string) string {
	quoted := make([]string, len(words))
	for index, word := range words {
		quoted[index] = regexp.QuoteMeta(word)
	}
	return strings.Join(quoted, "|")
}

// unaliasedTableWords are the words that may follow a table in a from list where the statement
// gives it no alias.
var unaliasedTableWords = []string{
	"cross", "except", "fetch", "for", "full", "group", "having", "inner", "intersect", "join", "lateral",
	"left", "limit", "natural", "offset", "on", "order", "right", "union", "using", "where", "window",
}

// TestEveryColumnTheCensusComparesIsOneTheProjectionScanHolds holds censustest.ProjectedColumns,
// the list of the columns the store states that api's scan of the projected tables' writers
// (TestNoProjectionRowIsWrittenWithoutTheStore) holds to the store writer's call path, to the
// census. A kind's row side is read through its rows, the names it binds (projection.names) and
// its past-bound count, and those statements are the census's only reads of the tables that
// project the store, so every column of a projected table one of them reads is a column the
// census holds against the store, and so one the store states, which the list must name for that
// table. A column the census compares that the list leaves out fails here, rather than letting an
// update of it outside the store writer's path pass the scan as one that sets nothing a tuple
// projects. It fails too on a projected table the census reads no column of, since a reading that
// finds nothing proves nothing. It reads the census's statements as text, and no database.
func TestEveryColumnTheCensusComparesIsOneTheProjectionScanHolds(t *testing.T) {
	// compared is each column the census reads, by its table, with the kinds whose row sides read it.
	compared := map[string]map[string][]string{}
	statements := 0
	for _, held := range projections {
		for _, statement := range [...]string{held.rows, held.names, held.pastBound} {
			if statement == "" {
				continue
			}
			statements++
			for table, columns := range censusColumnsOf(t, held.name, statement) {
				if compared[table] == nil {
					compared[table] = map[string][]string{}
				}
				for _, column := range columns {
					if !slices.Contains(compared[table][column], held.name) {
						compared[table][column] = append(compared[table][column], held.name)
					}
				}
			}
		}
	}
	if statements == 0 {
		t.Fatal("no projection of the census has a row side statement, so no column the census compares was read")
	}
	if len(projectedTables) == 0 {
		t.Fatal("censustest.ProjectedColumns lists no table, so no table that projects the store is held to the census")
	}
	for _, table := range projectedTables {
		if len(compared[table]) == 0 {
			t.Errorf("the census's row sides read no column of %s, a table that projects the store: either the census no longer holds it against the store, or this reading of its statements broke", table)
			continue
		}
		columns := slices.Sorted(maps.Keys(compared[table]))
		for _, column := range columns {
			if _, listed := censustest.ProjectedColumns[table][column]; !listed {
				t.Errorf("the census compares %s.%s (the row side of %s), which censustest.ProjectedColumns does not list for %s: a column the census holds against the store is one the store states, so an update of it outside the store writer's path must fail api's TestNoProjectionRowIsWrittenWithoutTheStore",
					table, column, strings.Join(compared[table][column], ", "), table)
			}
		}
		t.Logf("the census compares %s.%s", table, strings.Join(columns, ", "+table+"."))
	}
}

// censusColumnsOf answers the columns of each projected table one statement of the census's kind
// reads, each qualified by the alias the statement gives the table or by the table's own name. A
// read of a projected table under no alias fails the test, and so does an alias the statement
// gives two tables: the columns such a statement reads could be any of its tables'.
func censusColumnsOf(t *testing.T, kind, statement string) map[string][]string {
	t.Helper()
	aliases := map[string]string{}
	for _, read := range censusTableRead.FindAllStringSubmatch(statement, -1) {
		table, alias := strings.ToLower(read[1]), strings.ToLower(read[2])
		aliases[table] = table
		if alias == "" || slices.Contains(unaliasedTableWords, alias) {
			t.Errorf("the row side of %s reads %s under no alias, so this test cannot read which of its columns the census compares: give it one in the census's statement", kind, table)
			continue
		}
		if bound, taken := aliases[alias]; taken && bound != table {
			t.Errorf("the row side of %s gives %s and %s the one alias %s, so this test cannot read which of the two a column is of", kind, bound, table, alias)
		}
		aliases[alias] = table
	}
	columns := map[string][]string{}
	for _, alias := range slices.Sorted(maps.Keys(aliases)) {
		qualified := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(alias) + `\.([a-z_][a-z0-9_]*)\b`)
		for _, column := range qualified.FindAllStringSubmatch(statement, -1) {
			columns[aliases[alias]] = append(columns[aliases[alias]], strings.ToLower(column[1]))
		}
	}
	return columns
}
