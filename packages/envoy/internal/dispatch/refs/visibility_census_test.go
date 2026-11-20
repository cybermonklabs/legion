package refs

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
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
//
// A read that takes no acting person is out of that reach by construction, since the predicate
// binds the acting credential and such a read has none to bind. So the test also lists every
// exported read of the store that takes none, from the same source, and holds each one to
// readsWithoutAViewer, which names it with the reason it answers no reader. A read in neither
// place fails here, named, and so does an entry of that table that names no such read.
func TestEveryNodeKindAnswersAVisibilityPredicate(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	if SeedStoreThroughItsWriters == nil {
		t.Fatal("no seed of the store's own node kinds is registered: visibility_census_seed_test.go sets SeedStoreThroughItsWriters")
	}
	SeedStoreThroughItsWriters(t, ctx, database)

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

	// Every entry states its project and party rule (nodeVisibility.scope). A kind whose scope
	// nobody stated reads as visible to the server alone, which is safe and is still a rule no one
	// wrote, so it fails here, named.
	for _, kind := range declared {
		scope := nodeRegistry[kind].visibility.scope
		if scope.name == "" {
			t.Errorf("the registry holds the node kind %q with no stated scope: give its nodeVisibility the project and "+
				"party rule its nodes take", kind)
			continue
		}
		t.Logf("the node kind %s takes the %s scope", kind, scope.name)
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
			// A project's own visibility, the public arm of the access rule a person's scope
			// binds (access.PersonSeesProjectSQL, `<project>.visibility = 'public'`), and the
			// private-project SessionClause and RoleClause gates (`<project>.visibility =
			// 'private'`), are tests of a project and not of a node, so neither is a registry
			// predicate of the statement's own.
			statementTests := strings.Count(statement, ".visibility") - strings.Count(statement, ".visibility = 'public'") -
				strings.Count(statement, ".visibility = 'private'")
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
		// BlockLinks lists its links out of block_refs under its two documents' predicates
		// alone and keeps only those whose two blocks the block loader, which binds the block
		// predicate, resolves for the viewer.
		if marks == 0 || tests == 0 {
			t.Errorf("the %s read binds %d predicate of the registry's and makes %d test of `visibility` in all of its %d statements: a read of the graph that binds no visibility rule answers every node to everyone\n%s",
				read.call, marks, tests, len(read.statements), strings.Join(read.statements, "\n----\n"))
		}
	}
	viewing, without := storeReads(t)
	t.Logf("the package's reads of the graph that take a viewer are %v", viewing)
	for _, function := range viewing {
		if !recorded[function] {
			t.Errorf("%s reads the graph for a viewer and the census records no call of it, so no statement it issues was checked", function)
		}
	}
	t.Logf("the package's reads of the store that take no acting person are %v", without)
	for _, function := range without {
		if _, named := readsWithoutAViewer[function]; !named {
			t.Errorf("%s reads the store and takes no acting person, so it can bind no visibility predicate: "+
				"give it the acting person and bind the registry's predicate of each kind it answers, "+
				"or name it in readsWithoutAViewer with the reason it answers no reader", function)
		}
	}
	for _, function := range slices.Sorted(maps.Keys(readsWithoutAViewer)) {
		if !slices.Contains(without, function) {
			t.Errorf("readsWithoutAViewer names %s, which is no exported read of this package that takes no acting person: "+
				"a read that takes one is held to the predicates above, and a read that is gone needs no reason", function)
		}
	}
}

// Why each exported read of the store that takes no acting person binds no visibility
// predicate. Each reason is one of the few that hold: the read is the server's own work and
// answers no reader, or it answers no node at all, or it reads one node its caller has already
// read through a read that binds that node's predicate and answers nothing of any other node. A
// read whose answer reaches a reader and names a node is none of these: it takes the acting
// person instead, and the census above holds it to the registry.
const (
	serverWork = "the server's own work: a pass or a write calls it, and its answer reaches no reader"
	noNode     = "it answers no node: a fact about one row the caller names, never a node's id or text"
	nodeDetail = "it reads a detail of one node its caller has already read through a read that binds that node's predicate " +
		"(Node, RemovedItemBlockNode, or the caller's own registry predicate), and answers nothing of any other node"
)

// readsWithoutAViewer names each exported read of the store that takes no acting person, with
// the reason it binds no predicate (storeReads lists them from the package's source).
var readsWithoutAViewer = map[string]string{
	"AskPartIDs":          nodeDetail,
	"BlockChanges":        nodeDetail,
	"BlockIndexing.Cited": serverWork + " - a version write reads the blocks that cite the blocks its index changes, to count what it moves",
	"BlockTargets":        serverWork + " - the write of a document checks the block pointers it holds against the blocks they name",
	"BlockTextAt":         nodeDetail,
	"BlockTexts": "it reads the text of each block GET /api/v1/reference-targets has already named through TargetNames, " +
		"which binds each block's predicate, as CurrentBlockText and CurrentItemBlockText read one, and answers nothing of any other node",
	"CanonicalRepo":        noNode,
	"CountedChanges":       serverWork + " - a write records what its own text cites, as the addresses that text already spells",
	"CurrentBlockText":     nodeDetail,
	"CurrentItemBlockText": nodeDetail,
	"DocumentIsCited":      noNode,
	"DocumentsOwedWindows": serverWork,
	"DriftedExcerpts":      serverWork,
	"ExcerptsMoved":        serverWork + " - a write's read phase works out which of its document's excerpts the update it queues moves",
	"HeldTargets":          serverWork + " - a write's read phase reads the edges of the source its reconciliation replaces",
	"IssueAncestors": "every caller is a rule of a write - a reparent's cycle refusal, an issue event's ancestors, the near-copy " +
		"gate's exclusion, a block pointer's ancestor rule and the scope a result's check resolves in - and a rule that " +
		"skipped an issue it may not see would let a cycle through that issue or route an event short of its root",
	"IssueRepository":  noNode,
	"ItemBlockAt":      nodeDetail,
	"ItemsOwedWindows": serverWork,
	"ListOwed":         serverWork,
	"NewReader":        serverWork + " - the index-time resolver acts for no person, and every edge it lets a write store is held to the predicates of both its ends when it is read",
	"NewReaderInScope": serverWork + " - the index-time resolver of a source whose row is only queued acts for no person, as NewReader's does",
	"NextExcerptRetry": serverWork,
	"OwedItemBlocks":   serverWork,
	"PendingDocument.Scope": serverWork + " - the document a source whose row is only queued belongs to, which the index-time " +
		"resolver reads for no person",
	"PlanBlocks":         serverWork + " - a version write's read phase plans its block index against what the store holds of the document",
	"PlanItemIfOwed":     serverWork + " - a write's read phase plans the index an item it writes beside is owed",
	"PlanReplace":        serverWork + " - a write's read phase plans the reconciliation of a source's references against its edges",
	"QueueAskPartIDs":    nodeDetail,
	"ReadBlockIndex":     serverWork + " - a version write's read phase reads the document's block index it plans against",
	"ReadIndexedVersion": serverWork + " - a version write's read phase reads the document's address and the version's writer",
	"ReadItemIndex":      serverWork + " - a write's read phase reads the item's block index it plans against",
	"RecordExcerpts":     serverWork,
	"RemovedBlockNode":   nodeDetail,
	"ScopeSources":       serverWork,
	"SourceText":         serverWork,
}

// SeedStoreThroughItsWriters writes the people and agents a running deployment's store holds, as
// the writers of that store make them. A database nobody has written to holds none of them, so
// the census would otherwise read a store that resolves four kinds fewer than a running
// deployment's. It names no kind: it writes the rows that address people and agents and runs the
// writer that states the store from those rows, and the census reads which kinds that made from
// `graph_nodes` (R10).
//
// It is set by the external test package (visibility_census_seed_test.go) rather than written
// here, because that writer, graphcensus's backfill, imports this package, which a test of this
// package cannot import back.
var SeedStoreThroughItsWriters func(t *testing.T, ctx context.Context, database *store.Store)

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
// statements, so the test reads what Postgres was asked rather than a copy of it. A read whose
// answer depends on the kind it is asked for is called once for each kind it answers, and each
// call is held to the registry on its own, so none of them can bind no predicate.
func graphReads(t *testing.T, ctx context.Context, database *store.Store) []graphRead {
	t.Helper()
	recorder := &recordingQueryer{inner: database.Pool}
	// A viewer that sees every project, so each read reaches every statement it issues: what the
	// statements bind is the same text for every viewer, which is a parameter of each.
	who := model.Actor{Kind: "user", ID: "alice", Viewer: model.ServerViewer}
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
		record("TargetNames", "TargetNames of kind "+kind, func() error {
			_, err := TargetNames(ctx, recorder, who, []Target{{Kind: kind, ID: "no-such-node"}})
			return err
		})
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
	record("Closure", "Closure of CORE-1", func() error {
		_, _, err := Closure(ctx, recorder, who, "CORE-1")
		return err
	})
	record("Outgoing", "Outgoing of a document", func() error {
		_, err := Outgoing(ctx, recorder, who, "00000000-0000-4000-8000-000000000001")
		return err
	})
	// Each kind ResolvePrefix answers is its own statement, a pinned block's included; a prefix
	// nothing carries is its one no-match signal and no failure of the read.
	for _, asked := range []struct {
		kind  string
		scope Scope
	}{
		{"ask", Scope{}}, {"comment", Scope{IssueKey: "CORE-1"}}, {"message", Scope{}}, {"artifact", Scope{}},
		{"block", Scope{Document: "CORE-1/spec"}}, {"block", Scope{Document: "CORE-1/spec", Version: 1}},
	} {
		record("ResolvePrefix", fmt.Sprintf("ResolvePrefix of kind %s in %+v", asked.kind, asked.scope), func() error {
			_, err := ResolvePrefix(ctx, recorder, who, asked.kind, asked.scope, "00000000", Viewer{Human: true})
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		})
	}
	// ResolvePrefixes reads one statement per kind of item it is asked, a pinned block's
	// included, each asked on its own. A message id is resolved within an issue alone, so its
	// scope names one.
	for _, asked := range []ShortID{
		{Kind: "ask", Prefix: "00000000"}, {Kind: "comment", Scope: Scope{IssueKey: "CORE-1"}, Prefix: "00000000"},
		{Kind: "message", Scope: Scope{IssueKey: "CORE-1"}, Prefix: "00000000"}, {Kind: "artifact", Prefix: "00000000"},
		{Kind: "block", Scope: Scope{Document: "CORE-1/spec"}, Prefix: "00000000"},
		{Kind: "block", Scope: Scope{Document: "CORE-1/spec", Version: 1}, Prefix: "00000000"},
	} {
		record("ResolvePrefixes", fmt.Sprintf("ResolvePrefixes of kind %s in %+v", asked.Kind, asked.Scope), func() error {
			_, _, err := ResolvePrefixes(ctx, recorder, &who, []ShortID{asked})
			return err
		})
	}
	// A document's own scope is its id, answered with no statement; the three kinds whose scope
	// is read are read here.
	for _, kind := range []string{"ask", "comment", "message"} {
		record("ScopeOf", "ScopeOf of kind "+kind, func() error {
			_, err := ScopeOf(ctx, recorder, who, kind, "00000000-0000-4000-8000-000000000002")
			return err
		})
	}
	record("QuestionMap", "QuestionMap of a document", func() error {
		_, err := QuestionMap(ctx, recorder, who, "00000000-0000-4000-8000-000000000001")
		return err
	})
	// A document's windows are keyed by its uuid, an item's by `<kind>/<uuid>`; each binds the
	// predicate of its owner's kind.
	for _, owner := range []struct{ kind, key string }{
		{DocumentWindowKind, "00000000-0000-4000-8000-000000000001"},
		{ItemComment.WindowKind(), ItemComment.WindowOwnerKey("00000000-0000-4000-8000-000000000002")},
		{ItemAsk.WindowKind(), ItemAsk.WindowOwnerKey("00000000-0000-4000-8000-000000000003")},
		{ItemMessage.WindowKind(), ItemMessage.WindowOwnerKey("00000000-0000-4000-8000-000000000004")},
	} {
		record("BlockEmbeddingStates", "BlockEmbeddingStates of "+owner.key, func() error {
			_, err := BlockEmbeddingStates(ctx, recorder, who, owner.kind, owner.key)
			return err
		})
	}
	record("StoredExcerpts", "StoredExcerpts of CORE-1", func() error {
		_, err := StoredExcerpts(ctx, recorder, who, "issue", "CORE-1")
		return err
	})
	for _, kind := range []ItemKind{ItemComment, ItemAsk, ItemMessage} {
		record("ReplyBlocks", "ReplyBlocks of kind "+string(kind), func() error {
			_, err := ReplyBlocks(ctx, recorder, who, kind, []string{"00000000-0000-4000-8000-000000000002"})
			return err
		})
		record("CurrentItemBlock", "CurrentItemBlock of kind "+string(kind), func() error {
			_, _, _, err := CurrentItemBlock(ctx, recorder, who, kind, "00000000-0000-4000-8000-000000000002", "b1")
			return err
		})
	}
	record("IssueDescendants", "IssueDescendants of CORE-1", func() error {
		_, err := IssueDescendants(ctx, recorder, who, "CORE-1")
		return err
	})
	record("IssueAncestorsAsSeen", "IssueAncestorsAsSeen of CORE-1", func() error {
		_, err := IssueAncestorsAsSeen(ctx, recorder, who, "CORE-1")
		return err
	})
	return reads
}

// seedBlockLink indexes one document whose first block links to its second, and answers the
// document's id. BlockLinks reads its links out of block_refs under the predicates of its two
// documents, and resolves the two blocks of each through the block loader, which binds the block
// predicate: with no link to list it never reaches the loader, and the census would read only the
// statement that binds the documents' predicates.
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
	if err := graphstore.Caused(ctx, tx, write, graphstore.NoEvent); err != nil {
		t.Fatalf("append what the linked document's index stated: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the block link seed: %v", err)
	}
	return artifact
}

// storeReads is every exported function of this package that reads the store, read out of the
// package's own source rather than kept as a list: one that takes the Queryer it reads on, or
// the *pgx.Batch it queues its statement in. viewing is those that take a model.Actor beside it,
// or a *model.Actor where nil is no person (ResolvePrefixes), the reads of the graph for a
// viewer, and without is the rest. A method is named by its receiver's type and its own name.
func storeReads(t *testing.T) (viewing, without []string) {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list the package's source: %v", err)
	}
	fileSet := token.NewFileSet()
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
				case "model.Actor", "*model.Actor":
					viewer = true
				case "Queryer", "*pgx.Batch":
					queries = true
				}
			}
			if !queries {
				continue
			}
			name := function.Name.Name
			if function.Recv != nil {
				name = strings.TrimPrefix(types.ExprString(function.Recv.List[0].Type), "*") + "." + name
			}
			if viewer {
				viewing = append(viewing, name)
			} else {
				without = append(without, name)
			}
		}
	}
	if len(viewing) == 0 {
		t.Fatal("the package's source holds no read of the graph that takes a viewer, so the census could check none")
	}
	sort.Strings(viewing)
	sort.Strings(without)
	return viewing, without
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
