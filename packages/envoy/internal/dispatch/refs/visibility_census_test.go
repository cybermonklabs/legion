package refs

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
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
	"github.com/sjawhar/envoy/internal/dispatch/text"
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
//
// The reads it holds to the registry are every exported function of this package that reads
// the graph for a viewer, and the test reads that list out of the package's own source, so a
// read added later and left out of the census fails here, named. Each read is judged as a
// whole: it fails, named, when no statement it issues binds a predicate of the registry's.
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

	reads := graphReads(t, ctx, database)
	if len(reads) == 0 {
		t.Fatal("no read of the graph was recorded, so no statement was checked")
	}
	recorded := map[string]bool{}
	for _, read := range reads {
		recorded[read.function] = true
		var marks, tests int
		for index, statement := range read.statements {
			statementMarks := strings.Count(statement, VisibilityMarker)
			statementTests := strings.Count(statement, ".visibility")
			marks, tests = marks+statementMarks, tests+statementTests
			// A statement may bind no predicate where another statement of its read filters
			// its rows, but a test of `visibility` that no marker opens is a rule the
			// statement wrote for itself rather than took from the registry.
			if statementMarks != statementTests {
				t.Errorf("statement %d of the %s read makes %d test of `visibility` and binds %d predicate of the registry's: a read states no visibility rule of its own\n%s",
					index+1, read.call, statementTests, statementMarks, statement)
			}
			for _, kind := range markedKinds(statement) {
				if kind == "every kind" || kind == "the row's own kind" {
					continue
				}
				if !slices.Contains(declared, kind) {
					t.Errorf("statement %d of the %s read binds a predicate for %q, which the registry does not hold", index+1, read.call, kind)
				}
			}
		}
		t.Logf("the %s read binds %d predicate of the registry's and makes %d test of `visibility` over %d statements",
			read.call, marks, tests, len(read.statements))
		// Equal counts alone prove nothing: a read that binds no visibility rule at all makes
		// zero of each, and zero equals zero, which is how such a read passed this check on
		// 2026-11-12. Every read of the graph answers only what the acting person may see, so
		// every read binds at least one predicate. The rule is the read's and not each
		// statement's, because a read may filter one statement's rows by another's:
		// BlockLinks lists its links out of block_refs with no predicate and keeps only those
		// whose two blocks the block loader, which binds the block predicate, resolves for the
		// viewer.
		if marks == 0 || tests == 0 {
			t.Errorf("the %s read binds %d predicate of the registry's and makes %d test of `visibility` in all of its %d statements: a read of the graph that binds no visibility rule answers every node to everyone\n%s",
				read.call, marks, tests, len(read.statements), strings.Join(read.statements, "\n----\n"))
		}
	}
	viewing := viewerReads(t)
	t.Logf("the package's reads of the graph that take a viewer are %v", viewing)
	for _, function := range viewing {
		if !recorded[function] {
			t.Errorf("%s reads the graph for a viewer and the census records no call of it, so no statement it issues was checked", function)
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

// graphRead is one call of a read of the graph: the exported function that made it, the call
// as a failure names it, and every statement the call issued, in order.
type graphRead struct {
	function   string
	call       string
	statements []string
}

// graphReads calls every read of the graph that takes a viewer and records the SQL each call
// issues: the reads are run against the database through a queryer that records their
// statements, so the test reads what Postgres was asked rather than a copy of it.
func graphReads(t *testing.T, ctx context.Context, database *store.Store) []graphRead {
	t.Helper()
	recorder := &recordingQueryer{inner: database.Pool}
	who := model.Actor{Kind: "user", ID: "alice"}
	var reads []graphRead
	record := func(function, call string, run func() error) {
		t.Helper()
		start := len(recorder.statements)
		if err := run(); err != nil {
			t.Fatalf("run the %s read: %v", call, err)
		}
		reads = append(reads, graphRead{function: function, call: call, statements: slices.Clone(recorder.statements[start:])})
	}
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
		record("Edges", fmt.Sprintf("Edges %s of %s %s (whole documents %t)", query.Direction, query.Kind, query.ID, query.WholeDocuments), func() error {
			_, err := Edges(ctx, recorder, who, query)
			return err
		})
	}
	for _, kind := range nodeKinds {
		record("Node", "Node of kind "+kind, func() error {
			_, _, err := Node(ctx, recorder, who, kind, "no-such-node", true)
			return err
		})
		record("BacklinkCounts", "BacklinkCounts of kind "+kind, func() error {
			_, err := BacklinkCounts(ctx, recorder, who, kind, []string{"no-such-node"}, nil)
			return err
		})
		// The batched half queues its statement rather than issuing it on a queryer: the
		// statement recorded is the one queued, and the batch is sent so Postgres runs it.
		record("QueueBacklinkCounts", "QueueBacklinkCounts of kind "+kind, func() error {
			batch := &pgx.Batch{}
			if err := QueueBacklinkCounts(batch, who, kind, []string{"no-such-node"}, nil, map[string]int{}); err != nil {
				return err
			}
			for _, queued := range batch.QueuedQueries {
				recorder.statements = append(recorder.statements, queued.SQL)
			}
			return database.Pool.SendBatch(ctx, batch).Close()
		})
	}
	record("ReferencedBy", "ReferencedBy of CORE-1/spec", func() error {
		_, err := ReferencedBy(ctx, recorder, who, "CORE-1/spec")
		return err
	})
	// A message's blocks record no revisions, so no block of one is ever removed and
	// RemovedItemBlockNode refuses that kind; the two kinds it reads are read here.
	for _, kind := range []ItemKind{ItemComment, ItemAsk} {
		record("RemovedItemBlockNode", "RemovedItemBlockNode of a removed "+string(kind)+" block", func() error {
			_, _, err := RemovedItemBlockNode(ctx, recorder, who, kind, "no-such-node", 2)
			return err
		})
	}
	linked := seedBlockLink(t, ctx, database)
	record("BlockLinks", "BlockLinks within the linked document", func() error {
		links, err := BlockLinks(ctx, recorder, who, linked, linked)
		if err == nil && len(links) != 1 {
			err = fmt.Errorf("the linked document lists %d block links, want its one: with no link to list the read never reaches the loader that binds its predicate", len(links))
		}
		return err
	})
	return reads
}

// seedBlockLink indexes one document whose first block links to its second, and answers the
// document's id. BlockLinks reads its links out of block_refs, a statement that binds no
// predicate, and resolves the two blocks of each through the block loader, which binds the
// block predicate: with no link to list it never reaches the loader, and the census would read
// only the statement that binds none.
func seedBlockLink(t *testing.T, ctx context.Context, database *store.Store) string {
	t.Helper()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the block link seed: %v", err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, "LINK")
	artifact, _ := createReferenceArtifact(t, tx, "", "LINK", "links-md")
	const source = "See dispatch://LINK/artifact/links-md#b-target.\n"
	markdown := source + "The target.\n"
	blocks := []model.ArtifactBlock{
		{ID: "source", Type: "paragraph", From: 0, To: len(source)},
		{ID: "target", Type: "paragraph", From: len(source), To: len(markdown)},
	}
	located := text.ExtractAt(markdown, "https://dispatch.example")
	_, _, write, err := IndexBlocks(ctx, tx, artifact, 1, time.Now(), markdown, blocks, located, nil)
	if err != nil {
		t.Fatalf("index the linked document: %v", err)
	}
	if err := write.Caused(ctx, tx, graphstore.NoEvent); err != nil {
		t.Fatalf("append what the linked document's index stated: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the block link seed: %v", err)
	}
	return artifact
}

// viewerReads is every exported function of this package that reads the graph for a viewer,
// read out of the package's own source rather than kept as a list: one that takes a
// model.Actor beside the Queryer it reads on, or beside the *pgx.Batch it queues its statement
// in. A method is named by its receiver's type and its own name.
func viewerReads(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list the package's source: %v", err)
	}
	fileSet := token.NewFileSet()
	var reads []string
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fileSet, file, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || !function.Name.IsExported() {
				continue
			}
			var viewer, queries bool
			for _, parameter := range function.Type.Params.List {
				switch types.ExprString(parameter.Type) {
				case "model.Actor":
					viewer = true
				case "Queryer", "*pgx.Batch":
					queries = true
				}
			}
			if !viewer || !queries {
				continue
			}
			name := function.Name.Name
			if function.Recv != nil {
				name = strings.TrimPrefix(types.ExprString(function.Recv.List[0].Type), "*") + "." + name
			}
			reads = append(reads, name)
		}
	}
	if len(reads) == 0 {
		t.Fatal("the package's source holds no read of the graph that takes a viewer, so the census could check none")
	}
	sort.Strings(reads)
	return reads
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
