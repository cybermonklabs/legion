package refs

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestEveryNodeKindAnswersAVisibilityPredicate is the census of the visibility registry. It
// holds the registry to the node kinds the store itself resolves, in both directions at once,
// and holds every read of the graph to predicates the registry rendered.
//
// The kinds it compares against are read from the store and from no list in Go: the kinds
// `graph_nodes` holds, and the node kinds the `graph_edges` view states as literals, read out of
// the view's own definition in the catalogue. A node kind a later migration adds to either is a
// kind this test asks the registry for the day that migration lands, and one the registry holds
// that neither names is a predicate nothing can bind.
func TestEveryNodeKindAnswersAVisibilityPredicate(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seedStoreOwnNodes(t, ctx, database)

	resolved := storeNodeKinds(t, ctx, database)
	declared := VisibilityKinds()
	var problems []string
	for _, kind := range resolved {
		if !slices.Contains(declared, kind) {
			problems = append(problems, fmt.Sprintf(
				"the store resolves the node kind %q and the registry answers it no predicate", kind))
		}
	}
	for _, kind := range declared {
		if !slices.Contains(resolved, kind) {
			problems = append(problems, fmt.Sprintf(
				"the registry declares a predicate for %q, a node kind the store does not resolve", kind))
		}
	}
	// Both directions in one failure, so an implementer sees every gap at once rather than one
	// per run.
	if len(problems) > 0 {
		t.Fatalf("the registry and the store disagree about the node kinds:\n  %s\n  the store resolves %v\n  the registry declares %v",
			strings.Join(problems, "\n  "), resolved, declared)
	}

	for name, statement := range graphReadStatements(t, ctx, database) {
		marks := strings.Count(statement, VisibilityMarker)
		tests := strings.Count(statement, ".visibility")
		if marks != tests {
			t.Errorf("the %s read makes %d test of `visibility` and binds %d predicate of the registry's: a read states no visibility rule of its own\n%s",
				name, tests, marks, statement)
		}
		for _, kind := range markedKinds(statement) {
			if kind == "every kind" || kind == "the row's own kind" {
				continue
			}
			if !slices.Contains(declared, kind) {
				t.Errorf("the %s read binds a predicate for %q, which the registry does not hold", name, kind)
			}
		}
	}
}

// seedStoreOwnNodes writes one node of each kind the store holds of people and agents. A
// database nobody has written to holds none of them - a person's node is made by the write that
// first records them (graphstore.Authored) and a role's and a page's by the publisher's record -
// so the census would otherwise read a store that resolves four kinds fewer than a running
// deployment's. They are made here through the store's own constructors and its own writer, the
// way those writes make them.
func seedStoreOwnNodes(t *testing.T, ctx context.Context, database *store.Store) {
	t.Helper()
	at := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	who := model.Actor{Kind: "user", ID: "alice"}
	nodes := []graphstore.Node{
		graphstore.PersonNode("alice", who, at, graphstore.NoEvent),
		graphstore.SessionNode("census-session", who, at, graphstore.NoEvent),
		graphstore.RoleNode("census-role", who, at, graphstore.NoEvent),
		graphstore.PageNode(graphstore.RolePageID("census-role"), who, at, graphstore.NoEvent),
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the census seed: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := graphstore.Write(ctx, tx, nodes, nil); err != nil {
		t.Fatalf("seed the store's own node kinds: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the census seed: %v", err)
	}
}

// viewNodeKind matches a node kind the graph_edges view states as a literal, as the catalogue
// prints the view back: each arm names its from_kind and its to_kind, and an arm whose kind is a
// column of its table names neither.
var viewNodeKind = regexp.MustCompile(`'([a-z_]+)'::text AS (?:from_kind|to_kind)`)

// storeNodeKinds is the node kinds the store resolves, read from the store: every kind
// `graph_nodes` holds a row of, and every kind the `graph_edges` view names as the from or the
// to end of one of its arms.
func storeNodeKinds(t *testing.T, ctx context.Context, database *store.Store) []string {
	t.Helper()
	held := map[string]struct{}{}
	rows, err := database.Pool.Query(ctx, `select distinct kind from graph_nodes`)
	if err != nil {
		t.Fatalf("read the kinds graph_nodes holds: %v", err)
	}
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			rows.Close()
			t.Fatalf("scan a kind graph_nodes holds: %v", err)
		}
		held[kind] = struct{}{}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("read the kinds graph_nodes holds: %v", err)
	}
	var definition string
	if err := database.Pool.QueryRow(ctx,
		`select pg_get_viewdef('graph_edges'::regclass, true)`).Scan(&definition); err != nil {
		t.Fatalf("read the graph_edges definition: %v", err)
	}
	matches := viewNodeKind.FindAllStringSubmatch(definition, -1)
	if len(matches) == 0 {
		t.Fatalf("the graph_edges definition names no node kind this test can read:\n%s", definition)
	}
	for _, match := range matches {
		held[match[1]] = struct{}{}
	}
	kinds := make([]string, 0, len(held))
	for kind := range held {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	return kinds
}

// markedKinds is the kind each predicate in a statement names, read off the marker the registry
// opens every one of them with.
func markedKinds(statement string) []string {
	var kinds []string
	for _, part := range strings.Split(statement, VisibilityMarker)[1:] {
		name, _, _ := strings.Cut(part, " */")
		kinds = append(kinds, name)
	}
	return kinds
}

// graphReadStatements is the SQL every read of the graph issues, named by the read that issued
// it: the reads are run against the database through a queryer that records their statements,
// so the test reads what Postgres was asked rather than a copy of it.
func graphReadStatements(t *testing.T, ctx context.Context, database *store.Store) map[string]string {
	t.Helper()
	recorder := &recordingQueryer{inner: database.Pool}
	who := model.Actor{Kind: "user", ID: "alice"}
	for _, query := range []Query{
		{Direction: "in", Kind: "issue", ID: "CORE-1"},
		{Direction: "out", Kind: "issue", ID: "CORE-1"},
		{Direction: "in", Kind: "artifact", ID: "CORE-1/spec"},
		{Direction: "in", Kind: "artifact", ID: "CORE-1/spec", WholeDocuments: true},
		{Direction: "out", Kind: "artifact", ID: "00000000-0000-4000-8000-000000000001"},
		{Direction: "in", Kind: "comment", ID: "00000000-0000-4000-8000-000000000002"},
		{Direction: "out", Kind: "comment", ID: "00000000-0000-4000-8000-000000000002"},
		{Direction: "in", Kind: "ask", ID: "00000000-0000-4000-8000-000000000003"},
		{Direction: "out", Kind: "message", ID: "00000000-0000-4000-8000-000000000004"},
	} {
		if _, err := Edges(ctx, recorder, who, query); err != nil {
			t.Fatalf("read the %s edges of %s %s: %v", query.Direction, query.Kind, query.ID, err)
		}
	}
	for _, kind := range nodeKinds {
		if _, _, err := Node(ctx, recorder, who, kind, "no-such-node", true); err != nil {
			t.Fatalf("read a %s node: %v", kind, err)
		}
		if _, err := BacklinkCounts(ctx, recorder, who, kind, []string{"no-such-node"}, nil); err != nil {
			t.Fatalf("count %s backlinks: %v", kind, err)
		}
	}
	statements := make(map[string]string, len(recorder.statements))
	for index, statement := range recorder.statements {
		statements[fmt.Sprintf("graph read %d", index+1)] = statement
	}
	return statements
}

// recordingQueryer is the Queryer a read runs on while this test reads the statements it issues.
type recordingQueryer struct {
	inner      Queryer
	statements []string
}

func (r *recordingQueryer) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	r.statements = append(r.statements, sql)
	return r.inner.Query(ctx, sql, args...)
}

func (r *recordingQueryer) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	r.statements = append(r.statements, sql)
	return r.inner.QueryRow(ctx, sql, args...)
}
