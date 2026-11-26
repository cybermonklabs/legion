package store

import (
	"context"
	"io/fs"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// graphEdgesViewMigrations is every migration that states graph_edges from GraphEdgesView, by its
// file, with the arm set it states. A migration stating the view from this set on is one of them:
// the first of them is where the function became the view's one source.
var graphEdgesViewMigrations = map[string]GraphEdgesArms{
	"20261117140000_issue_tree_relations.up.sql":                          GraphEdgesIssueRelations,
	"20261118023219_issue_tree_components.up.sql":                         GraphEdgesComponents,
	"20261119100000_issue_tree_artifact_refs.up.sql":                      GraphEdgesArtifactRefs,
	"20261121090000_issue_tree_graph_edges_after_tuple_arm_filter.up.sql": GraphEdgesTupleArmFilter,
	"20261123180000_issue_tree_graph_edges_after_store_class_last.up.sql": GraphEdgesTupleArmFilter,
	"20261125150001_issue_tree_graph_edges_block_node_listed.up.sql":      GraphEdgesBlockNodeListed,
}

// graphEdgesSiblingRestatements are the migrations of the sibling branch this branch merges that
// state graph_edges by hand after the first of graphEdgesViewMigrations, each from the arms the
// sibling's own view has: 20261120122700 filters the tuple arm of 20261109215947's arms,
// 20261121034242 restates 20261116094101's arms with that filter, and 20261121104002 restates
// 20261121034242's arms with 20261117020856's class `store` and `lens_block` arm. None holds this
// branch's arms, and a database runs each version once, so the newest of
// graphEdgesViewMigrations, dated after all three, states the view again from GraphEdgesView with
// every arm of both branches.
var graphEdgesSiblingRestatements = map[string]bool{
	"20261120122700_graph_edges_tuple_arm_filter.up.sql":          true,
	"20261121034242_graph_edges_asks_and_tuple_arm_filter.up.sql": true,
	"20261121104002_graph_edges_store_class_last.up.sql":          true,
}

// graphEdgesStatement is the head of the statement that states the view, as GraphEdgesView writes
// it and as every migration that states it writes it.
const graphEdgesStatement = "create or replace view graph_edges "

// TestEveryGraphEdgesMigrationHoldsTheViewItsSourceGives holds graph_edges to one source. A
// migration is an embedded file that calls no Go, so each migration that states the view from
// the issue-tree spec's split of the issue relations on holds the text GraphEdgesView gives for
// its arm set, byte for byte after its header of comments, and nothing else. Every migration
// that states the view and sorts after the first of them must be one of them, so a migration that
// states the view by hand fails here by its name. Last, a database migrated to this head holds the
// view the newest set states: stating GraphEdgesView of that set over it leaves the catalogue's
// definition as it was.
func TestEveryGraphEdgesMigrationHoldsTheViewItsSourceGives(t *testing.T) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		t.Fatalf("read the migrations: %v", err)
	}
	first := slices.Min(slices.Collect(maps.Keys(graphEdgesViewMigrations)))
	var newest string
	var newestArms GraphEdgesArms
	held := 0
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		body, err := fs.ReadFile(migrationFiles, path.Join("migrations", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		arms, listed := graphEdgesViewMigrations[name]
		states := strings.Contains(string(body), graphEdgesStatement)
		switch {
		case !listed && states && name > first && !graphEdgesSiblingRestatements[name]:
			t.Errorf("%s states graph_edges and is not stated from GraphEdgesView: write its statement with that function's text for its arm set, and list it in graphEdgesViewMigrations", name)
			continue
		case !listed:
			continue
		}
		held++
		statement := afterHeader(string(body))
		if want := GraphEdgesView(arms); statement != want {
			t.Errorf("%s holds a statement that is not GraphEdgesView(%d):\n%s", name, arms, firstDifference(statement, want))
		}
		if name > newest {
			newest, newestArms = name, arms
		}
	}
	if held != len(graphEdgesViewMigrations) {
		t.Fatalf("found %d of the %d migrations graphEdgesViewMigrations lists", held, len(graphEdgesViewMigrations))
	}

	database := openEmptyTestStore(t)
	ctx := context.Background()
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	read := func(when string) string {
		t.Helper()
		var definition string
		if err := database.Pool.QueryRow(ctx, `select pg_get_viewdef('graph_edges'::regclass, true)`).Scan(&definition); err != nil {
			t.Fatalf("read graph_edges %s: %v", when, err)
		}
		return definition
	}
	migrated := read("as the migrations left it")
	if _, err := database.Pool.Exec(ctx, GraphEdgesView(newestArms)); err != nil {
		t.Fatalf("state GraphEdgesView(%d), the set of %s: %v", newestArms, newest, err)
	}
	if stated := read("once GraphEdgesView states it"); stated != migrated {
		t.Errorf("the database the migrations left holds another graph_edges than GraphEdgesView(%d), the set of %s, states:\n%s",
			newestArms, newest, firstDifference(migrated, stated))
	}
}

// afterHeader is a migration's text after its header: every line before its first statement
// that is a comment or blank.
func afterHeader(body string) string {
	rest := body
	for rest != "" {
		line, tail, _ := strings.Cut(rest, "\n")
		if trimmed := strings.TrimSpace(line); trimmed != "" && !strings.HasPrefix(trimmed, "--") {
			return rest
		}
		rest = tail
	}
	return rest
}

// firstDifference is where got and want first part, with a line of each around it, so a failure
// names the place rather than printing two whole views.
func firstDifference(got, want string) string {
	gotLines, wantLines := strings.Split(got, "\n"), strings.Split(want, "\n")
	for index := range max(len(gotLines), len(wantLines)) {
		var have, should string
		if index < len(gotLines) {
			have = gotLines[index]
		}
		if index < len(wantLines) {
			should = wantLines[index]
		}
		if have != should {
			return "line " + strconv.Itoa(index+1) + ":\n  got:  " + have + "\n  want: " + should
		}
	}
	return "they differ in nothing a line holds"
}
