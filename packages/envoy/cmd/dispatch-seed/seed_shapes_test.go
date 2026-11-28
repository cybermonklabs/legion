package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// shapeCount is one shape of a seeded data set and the rows the seed wrote of it.
type shapeCount struct {
	name  string
	count int
}

// seedShapes is the census of a seeded database: one count for each shape a test of the seed
// can lose by seeding at a smaller scale, sorted by name. The shapes are
//
//   - each table's rows (rowCountsOf), as "table <name>";
//   - each arm of graph_edges, as assertEveryGraphArmHoldsRows splits the view's own text into
//     them (graphEdgeArms), as "graph arm <position> <kind>-><to_kind>", the pair being the
//     arm's literals where it states them;
//   - each markdown size band's versions as the run counted them (run.bands), as
//     "size band <band>";
//   - the comments and the asks of each anchor shape the plan hangs them on (anchorShapes), as
//     "anchor <shape> comments" and "anchor <shape> asks".
//
// Every name is fixed by the schema and the code, never by the draw, so the census of a seed at
// one scale and the census of a seed at another under the same schema name the same shapes. A
// smaller scale is a faster test only where it holds a row of every shape the base seed held: a
// shape that falls to zero is a part of the seed the test no longer exercises.
func seedShapes(t *testing.T, database *store.Store, run *seedRun) []shapeCount {
	t.Helper()
	ctx := context.Background()
	var shapes []shapeCount
	counts, err := rowCountsOf(database)
	if err != nil {
		t.Fatal(err)
	}
	for table, count := range counts {
		shapes = append(shapes, shapeCount{"table " + table, count})
	}

	for index, arm := range graphEdgeArms(t, database) {
		name := fmt.Sprintf("graph arm %02d", index+1)
		kind := armKindLiteral.FindStringSubmatch(arm)
		toKind := armToKindLiteral.FindStringSubmatch(arm)
		if kind != nil && toKind != nil {
			name += " " + kind[1] + "->" + toKind[1]
		}
		var rows int
		if err := database.Pool.QueryRow(ctx, `select count(*) from (`+arm+`) arm`).Scan(&rows); err != nil {
			t.Fatalf("count arm %d of graph_edges: %v\n%s", index+1, err, arm)
		}
		shapes = append(shapes, shapeCount{name, rows})
	}

	bands := slices.Clone(sizeBands)
	for band := range run.bands {
		if !slices.Contains(bands, band) {
			bands = append(bands, band)
		}
	}
	for _, band := range bands {
		shapes = append(shapes, shapeCount{"size band " + band, run.bands[band]})
	}

	anchored := run.plan.documents[run.plan.anchored].ID
	for _, shape := range anchorShapes {
		block := firstParagraphBlock
		if shape.tableRow {
			block = run.tableRow
		}
		for _, table := range []string{"comments", "asks"} {
			var count int
			if err := database.Pool.QueryRow(ctx, `
				select count(*) from `+table+`
				where anchor->>'block_id' = $1 and (anchor->>'artifact_id' = $2) = $3
			`, block, anchored, shape.anchoredDocument).Scan(&count); err != nil {
				t.Fatalf("count the %s anchored on %s: %v", table, shape.name, err)
			}
			shapes = append(shapes, shapeCount{"anchor " + shape.name + " " + table, count})
		}
	}

	slices.SortFunc(shapes, func(a, b shapeCount) int { return strings.Compare(a.name, b.name) })
	return shapes
}

// assertSeedKeepsItsShapes holds the census of test's seed (seedShapes) to a row of every shape
// its seed at testScale held: each name testdata/seed-shapes-<test>.txt lists, the census of that
// seed at the merge base, is a shape of shapes with a count above zero. A name the census no
// longer holds fails as well, since the file then names a part of the seed that nothing reads; a
// change of the schema or the seed that drops one captures the file again, as its head says. On a
// failure the whole census is logged, so the reader sees what the smaller data set does hold.
func assertSeedKeepsItsShapes(t *testing.T, test string, shapes []shapeCount) {
	t.Helper()
	path := filepath.Join("testdata", "seed-shapes-"+test+".txt")
	base, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the census of %s's seed at testScale: %v", test, err)
	}
	counts := make(map[string]int, len(shapes))
	for _, shape := range shapes {
		counts[shape.name] = shape.count
	}
	names, lost := 0, false
	for line := range strings.Lines(string(base)) {
		line = strings.TrimSuffix(line, "\n")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		names++
		name, _, _ := strings.Cut(line, "\t")
		count, named := counts[name]
		switch {
		case !named:
			t.Errorf("%s's data set lost a shape its base seed held: its census no longer names %q (%s)",
				test, name, path)
			lost = true
		case count == 0:
			t.Errorf("%s's data set lost a shape its base seed held: %q holds no rows (%s)", test, name, path)
			lost = true
		}
	}
	// A file that names no shape would hold the seed to nothing and pass whatever it seeded.
	if names == 0 {
		t.Fatalf("%s names no shape, so it holds %s's seed to nothing", path, test)
	}
	if lost {
		t.Logf("the census of %s's seed:\n%s", test, formatShapes(shapes))
	}
}

// formatShapes prints a census one "name<TAB>count" line per shape, sorted by name.
func formatShapes(shapes []shapeCount) string {
	sorted := slices.Clone(shapes)
	slices.SortFunc(sorted, func(a, b shapeCount) int { return strings.Compare(a.name, b.name) })
	var out strings.Builder
	for _, shape := range sorted {
		fmt.Fprintf(&out, "%s\t%d\n", shape.name, shape.count)
	}
	return out.String()
}

// graphEdgeArms is each arm of graph_edges as its own query, in the view's order, as
// assertEveryGraphArmHoldsRows and seedShapes run them. The arms come from pg_get_viewdef, the
// view's own text, so they are those of the schema the database holds and not a list kept here.
func graphEdgeArms(t *testing.T, database *store.Store) []string {
	t.Helper()
	var definition string
	if err := database.Pool.QueryRow(context.Background(),
		`select pg_get_viewdef('graph_edges', true)`).Scan(&definition); err != nil {
		t.Fatalf("read the graph_edges definition: %v", err)
	}
	arms := strings.Split(definition, "UNION ALL")
	for index, arm := range arms {
		arms[index] = strings.TrimSuffix(strings.TrimSpace(arm), ";")
	}
	return arms
}

// sizeBands are the markdown size bands versionBytes draws a version's size in, by the names it
// counts them under in run.bands. They are listed so that a band no version of a run fell in is
// counted as zero rather than left out of the census.
var sizeBands = []string{"1-5 KB", "5-10 KB", "10-20 KB", "100-660 KB"}

// anchorShapes are the anchors the plan hangs an anchored comment or ask on (planAsks,
// planComments): the first paragraph of the latest version (firstParagraphBlock) or the first
// table's body row (TableRow, run.tableRow), in the anchored document the anchor reads are
// measured on or in another document (documentForAnchor).
var anchorShapes = []struct {
	name             string
	tableRow         bool
	anchoredDocument bool
}{
	{"paragraph in the anchored document", false, true},
	{"table row in the anchored document", true, true},
	{"paragraph in another document", false, false},
	{"table row in another document", true, false},
}
