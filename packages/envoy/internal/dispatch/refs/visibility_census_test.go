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
//
// Last, it holds the kinds a writer may reserve for its author to what each read of a node answers
// (requireEachAuthorKindAnswersItsAuthorAlone): a node of each at `project` is read by a second
// person and an author-only one is not, a lens kind takes no author-only node, and the registry's
// author flag is on for exactly those kinds.
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
			// The column every loader selects to answer a node's stored visibility, beside its
			// stored state (StoredRowOf), reads `visibility` once and tests nothing, and the
			// registry opens it with a marker of its own; every other `visibility` a statement
			// reads is a test. A marker is subtracted only where it opens that column, named
			// stored_visibility, so a marker set beside a test excuses nothing.
			columns := len(storedVisibilityColumn.FindAllStringIndex(statement, -1))
			if markers := strings.Count(statement, StoredNodeMarker); markers != columns {
				t.Errorf("statement %d of the %s read opens %d marker of a stored state and visibility and %d of them opens a stored_visibility column: the marker opens that column alone\n%s",
					index+1, read.call, markers, columns, statement)
			}
			statementTests := strings.Count(statement, ".visibility") - columns
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
	requireEachAuthorKindAnswersItsAuthorAlone(t, ctx, database)
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
	treeWalk = "the walk of an issue's tree (docs.WalkIssueTree) is what the record a person approves is built from, and what " +
		"every gate compares a review's digest with, so it reads the store as the server holds it, one answer for every reader; " +
		"a render of the tree for a reader binds that reader's predicate elsewhere"
)

// readsWithoutAViewer names each exported read of the store that takes no acting person, with
// the reason it binds no predicate (storeReads lists them from the package's source).
var readsWithoutAViewer = map[string]string{
	"AskPartIDs":           nodeDetail,
	"BlockChanges":         nodeDetail,
	"BlockEmbeddingStates": nodeDetail,
	"BlockIndexing.Cited":  serverWork + " - a version write reads the blocks that cite the blocks its index changes, to count what it moves",
	"BlockRefKinds": serverWork + " - a version write's block index reads the names of the block[] attributes the " +
		"store's type nodes declare, which only a migration writes, to bind them as a parameter of its own read of the store's tuples",
	"BlockTargets": serverWork + " - the write of a document checks the block pointers it holds against the blocks they name",
	"BlockTextAt":  nodeDetail,
	"CommentsNamingNoNode": serverWork + " - the block-index tick's write of the comments the events after its cursor name " +
		"reads, under those comments' owner locks, which of their rows name no node",
	"CommentsOffPage":      treeWalk + " - which comments a walk of the skeleton's loaders name as a parent are of another page of the issue's conversation (WalkIssueSkeleton); it answers ids of comments the walk was given, and no comment is a node the store takes reserved for its author (graphstore.AdmitAuthorOnly), so every reader the issue's read admits is answered the same",
	"CountedChanges":       serverWork + " - a write records what its own text cites, as the addresses that text already spells",
	"CurrentBlockText":     nodeDetail,
	"CurrentItemBlockText": nodeDetail,
	"DecisionFacts":        serverWork + " - a version write's read phase reads what the store and the asks rows hold of the version's decisions, readings and ask blocks",
	"DecisionIndexing.PlanAuthored": serverWork + " - a write that leaves a version's blocks to the reconciliation reads what the store " +
		"holds of the requirements, checks and plan tasks it writes at once, and of the blocks above them",
	"DocumentIsCited":      noNode,
	"DocumentMentioning":   serverWork + " - rebuild-refs reads the document whose references it reconciles, to sign the mentions it states",
	"DocumentsOwedWindows": serverWork,
	"DriftedExcerpts":      serverWork,
	"ExcerptsMoved":        serverWork + " - a write's read phase works out which of its document's excerpts the update it queues moves",
	"HeadCut":              treeWalk + " - the cut of the store every read of the walk is made at",
	"HeldTargets":          serverWork + " - a write's read phase reads the edges of the source its reconciliation replaces",
	"IsConversationNode": noNode + " - R2's test of the one node its caller names, which decides what kind of node it is and " +
		"not what a reader sees: the nodes of a conversation reach a reader through nodeRegistry's loaders, which bind the " +
		"reader's predicate (ConversationNodes)",
	"IssueAncestors": "every caller is a rule of a write - a reparent's cycle refusal, an issue event's ancestors, the near-copy " +
		"gate's exclusion, a block pointer's ancestor rule and the scope a result's check resolves in - and a rule that " +
		"skipped an issue it may not see would let a cycle through that issue or route an event short of its root",
	"IssueRepository":  noNode,
	"ItemBlockAt":      nodeDetail,
	"ItemsOwedWindows": serverWork,
	"ListOwed":         serverWork,
	"NamedBlock": "it answers a node id and nothing of any node: the pinned read of a block names the node an address " +
		"names, which it then reads through Node and holds to the block's predicate at that node, so a block who may not " +
		"see answers what an address nobody holds answers",
	"NextExcerptRetry": serverWork,
	"OwedItemBlocks":   serverWork,
	"PartOfChains": serverWork + " - the block index climbs the part_of chain of each block an authored node may hang under, " +
		"to keep a cycle out of where it places that node (placeAuthored), and the chain reaches no reader",
	"PendingDocument.Scope": serverWork + " - the document a source whose row is only queued belongs to, which the index-time " +
		"resolver reads for no person",
	"PinnedParent": nodeDetail + " - the lens read names the lenses on a document, a block of one, a block of a comment or a part of an ask " +
		"it read through Node by the address a lens names that node at, its own pinned to a version of its document or a revision of its item",
	"PlanBlocks":        serverWork + " - a version write's read phase plans its block index against what the store holds of the document",
	"PlanDecisionNodes": serverWork + " - a deferred version write's read phase plans its typed-node pass against what the store holds of the document",
	"PlanItemIfOwed":    serverWork + " - a write's read phase plans the index an item it writes beside is owed",
	"PlanOpenedAskTies": serverWork + " - a settlement's read phase plans the ties of the ask blocks it opened against the document's block index",
	"PlanReplace":       serverWork + " - a write's read phase plans the reconciliation of a source's references against its edges",
	"PreventionRetractions": serverWork + " - the index pass, a retirement and the backfill read the prevented_by tuples at the checks " +
		"they end, under those checks' owner lock, to retract each; its writer signs the retractions and views nothing",
	"QueueAskPartIDs":    nodeDetail,
	"ReadBlockIndex":     serverWork + " - a version write's read phase reads the document's block index it plans against",
	"ReadCommentPage":    treeWalk + " - the page of the issue's conversation comments a walk of the skeleton takes (WalkIssueSkeleton), and the position of its oldest; no comment is a node the store takes reserved for its author (graphstore.AdmitAuthorOnly), so every reader the issue's read admits may see each comment of the page, and each is answered the same page",
	"ReadIndexedVersion": serverWork + " - a version write's read phase reads the document's address and the version's writer",
	"ReadItemIndex":      serverWork + " - a write's read phase reads the item's block index it plans against",
	"RecordExcerpts":     serverWork,
	"RemovedBlockNode":   nodeDetail,
	"Replacement.ArtifactMentions": serverWork + " - a document's reconcile reads whether the document's row names its node, " +
		"to state the mentions it moves out of that node",
	"ResolveFarEnds": "it answers node ids and nothing of any node: the block index resolves the far ends of the pointers it " +
		"places a node by, and the pre-mortem conversion the checks its map names, whose nodes it then reads through Nodes, " +
		"which binds each one's predicate, so a node who may not see is absent from what a reader is shown",
	"ScopeSources": serverWork,
	"SourceText":   serverWork,
	"StaleLensesOn": serverWork + " - a document's removal reads the stale lenses on the removed document to take each " +
		"gone with all that stands beneath it, those its caller may not see included",
	"TreeChildren": treeWalk + " - the children of each node the walk reaches",
	"TreeFactsOf":  treeWalk + " - what each node the walk reaches states at its cut",
	"TreeMovedTo":  treeWalk + " - the nodes the address a comment's anchor names resolves to at the walk's cut",
	"TreeMembers": treeWalk + " - the blocks a document holds at a version's time, which the render of that version " +
		"(docs.RenderAt) takes: a version renders as one set of bytes for every reader, as its stored markdown does",
	"TreeNodeRows":    treeWalk + " - the row of each node the walk reaches",
	"TreePlacedAbove": treeWalk + " - the nodes of the issue that stand under a node of an issue above it",
	"TreeSubtrees":    treeWalk + " - every level of the children under the walk's frontier, read in one statement",
	"TurnJITOff":      treeWalk + " - it answers nothing: it turns JIT off for the rest of the transaction the walk reads in",
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

// storedVisibilityColumn is the one read of `visibility` StoredNodeMarker opens: the column a
// loader selects to answer a node's stored visibility, named stored_visibility beside the column
// stored_state (StoredRowOf).
var storedVisibilityColumn = regexp.MustCompile(regexp.QuoteMeta(StoredNodeMarker) + `\w+\.visibility as stored_visibility\b`)

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
	// Nodes is Node over a batch, ExcerptedNodes is Nodes with each node's excerpt, and
	// TargetNames is Nodes with each node's name: one target of every kind, so each kind's loader
	// issues its statement in the one call. BlockTexts reads each kind of block among them in a
	// statement of its own and passes every other kind over. A finding's statement binds the
	// predicate of its kind over the row it reads; a document's block and an item's bind none,
	// since a caller reads those only once TargetNames has answered them to the reader. So that
	// read is held to the registry as a whole.
	everyKind := make([]Target, len(nodeKinds))
	for index, kind := range nodeKinds {
		everyKind[index] = Target{Kind: kind, ID: "no-such-node"}
	}
	record("Nodes", "Nodes of every kind at once", func() error {
		_, err := Nodes(ctx, recorder, who, everyKind)
		return err
	})
	record("ExcerptedNodes", "ExcerptedNodes of every kind at once", func() error {
		_, err := ExcerptedNodes(ctx, recorder, who, everyKind)
		return err
	})
	record("BlockNodesAndRows", "BlockNodesAndRows of a block", func() error {
		_, _, err := BlockNodesAndRows(ctx, recorder, who, []string{"no-such-node"})
		return err
	})
	record("HeadRows", "HeadRows of CORE-1", func() error {
		_, _, err := HeadRows(ctx, recorder, who, "CORE-1", nil)
		return err
	})
	record("HeadRows", "HeadRows of the skeleton of CORE-1", func() error {
		_, _, err := HeadRows(ctx, recorder, who, "CORE-1", &HeadSkeleton{Documents: []string{"CORE-1/spec"}})
		return err
	})
	record("TargetNames", "TargetNames of every kind at once", func() error {
		_, err := TargetNames(ctx, recorder, who, everyKind)
		return err
	})
	record("BlockTexts", "BlockTexts of every kind at once", func() error {
		_, err := BlockTexts(ctx, recorder, who, everyKind)
		return err
	})
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
	record("ReadableFarEnds", "ReadableFarEnds of a block address", func() error {
		_, err := ReadableFarEnds(ctx, recorder, who, []string{"CORE-1/spec#b1"})
		return err
	})
	record("Closure", "Closure of CORE-1", func() error {
		_, err := Closure(ctx, recorder, who, "CORE-1")
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
	// ResolvePrefixes reads each kind its ids name in one statement, and the blocks of a document in
	// two: those of its latest version and those of a version a reference pins, when one call asks
	// for every kind at once.
	record("ResolvePrefixes", "ResolvePrefixes of every kind", func() error {
		_, _, err := ResolvePrefixes(ctx, recorder, &who, []ShortID{
			{Kind: "ask", Prefix: "00000000"}, {Kind: "comment", Scope: Scope{IssueKey: "CORE-1"}, Prefix: "00000000"},
			{Kind: "message", Scope: Scope{IssueKey: "CORE-1"}, Prefix: "00000000"}, {Kind: "artifact", Prefix: "00000000"},
			{Kind: "block", Scope: Scope{Document: "CORE-1/spec"}, Prefix: "00000000"},
			{Kind: "block", Scope: Scope{Document: "CORE-1/spec", Version: 1}, Prefix: "00000000"},
		})
		return err
	})
	record("TargetNames", "TargetNames of an issue and a document", func() error {
		_, err := TargetNames(ctx, recorder, who, []Target{{Kind: "issue", ID: "CORE-1"}, {Kind: "artifact", ID: "CORE-1/spec"}})
		return err
	})
	// The index-time resolver resolves the short ids a text writes for that text's writer
	// (Reader.Items, through ResolvePrefix), so each constructor's reader is asked one: the
	// statement that resolves it is the read of the graph the census holds it to.
	const shortComment = "See dispatch://CORE-1/comment/00000000."
	record("NewReader", "NewReader's resolution of a short comment id", func() error {
		NewReader(ctx, recorder, who, "comment", "00000000-0000-4000-8000-000000000002", "https://dispatch.example").Mentions(shortComment)
		return nil
	})
	record("NewReaderInScope", "NewReaderInScope's resolution of a short comment id", func() error {
		NewReaderInScope(ctx, recorder, who, "comment", "00000000-0000-4000-8000-000000000002", "https://dispatch.example", nil).Mentions(shortComment)
		return nil
	})
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
	record("IssueDescendantRows", "IssueDescendantRows of CORE-1", func() error {
		_, err := IssueDescendantRows(ctx, recorder, who, "CORE-1")
		return err
	})
	record("LensCatalogue", "LensCatalogue", func() error {
		_, err := LensCatalogue(ctx, recorder, who)
		return err
	})
	conversed := seedConversation(t, ctx, database)
	record("ConversationNodes", "ConversationNodes of "+conversed, func() error {
		nodes, err := ConversationNodes(ctx, recorder, who, conversed, time.Now())
		if err == nil && len(nodes) == 0 {
			err = fmt.Errorf("the conversation of %s lists no node: with no item that passes R2's test the read never reaches the loaders that bind their predicates", conversed)
		}
		return err
	})
	// The page is read off the pool rather than the recorder: it binds no predicate
	// (readsWithoutAViewer), and the census judges the statements ConversationPageNodes sends.
	record("ConversationPageNodes", "ConversationPageNodes of "+conversed, func() error {
		page, err := ReadCommentPage(ctx, database.Pool, conversed, time.Now(), nil, 200)
		if err != nil {
			return err
		}
		nodes, err := ConversationPageNodes(ctx, recorder, who, conversed, time.Now(), page)
		if err == nil && len(nodes) == 0 {
			err = fmt.Errorf("the conversation page of %s lists no node: with no item that passes R2's test the read never reaches the loaders that bind their predicates", conversed)
		}
		return err
	})
	// The same page read off head rows that hold none of its comments: the asks and the messages
	// go through the loaders, which bind their predicates, and the comments' nodes are built from
	// their rows, which bind none.
	record("ConversationPageNodesOfHeads", "ConversationPageNodesOfHeads of "+conversed, func() error {
		page, err := ReadCommentPage(ctx, database.Pool, conversed, time.Now(), nil, 200)
		if err != nil {
			return err
		}
		nodes, held, err := ConversationPageNodesOfHeads(ctx, recorder, who, conversed, time.Now(), page, newHeadTree(map[Target]*HeadRow{}, nil))
		switch {
		case err != nil:
		case !held:
			err = fmt.Errorf("the conversation page of %s, which holds no comment, is not read off the head rows", conversed)
		case len(nodes) == 0:
			err = fmt.Errorf("the conversation page of %s lists no node: with no item that passes R2's test the read never reaches the loaders that bind their predicates", conversed)
		}
		return err
	})
	// The tree is reserved for its author at its root issue: the author reads its plan node and
	// that node's needs, and another reader reads no node of it.
	planned := seedPlanOrder(t, ctx, database)
	for _, reader := range []model.Actor{who, privateReader} {
		record("PlanOrder", "PlanOrder of "+planned+" as "+reader.ID, func() error {
			order, err := PlanOrder(ctx, recorder, reader, planned)
			switch {
			case err != nil:
			case reader.ID == who.ID && len(order.Order) < 5:
				err = fmt.Errorf("the tree of %s answers its author the order %+v, want both issues' halves and the plan node: with no plan node the read never reaches its read of the needs", planned, order.Order)
			case reader.ID != who.ID && len(order.Order) != 0:
				err = fmt.Errorf("the tree of %s, reserved for its author at its root, answers another reader the order %+v, want none", planned, order.Order)
			}
			return err
		})
	}
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
	_, _, write, _, err := IndexBlocks(ctx, tx, artifact, OneVersion(artifact, 1, time.Now()), markdown, blocks, located, nil, TypedFacts{})
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

// seedConversation writes one issue whose conversation holds a person's ask, which R2's test
// always passes, with its parts indexed as a write of this build indexes them, and answers the
// issue's key. ConversationNodes lists an issue's items and reads R2's test over them in
// statements that bind no predicate, and loads what passes through nodeRegistry's loaders, which
// bind the ask's and the ask block's: with no item that passes it never reaches the loaders, and
// the census would read only the statements that bind none.
func seedConversation(t *testing.T, ctx context.Context, database *store.Store) string {
	t.Helper()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the conversation seed: %v", err)
	}
	defer tx.Rollback(ctx)
	const issue = "CONV-1"
	createReferenceProject(t, tx, "CONV")
	createReferenceIssue(t, tx, issue, "CONV")
	conversationItem(t, tx, "asks", issue, `{"kind":"user","id":"alice"}`, "Which order first?", time.Now(), "")
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the conversation seed: %v", err)
	}
	return issue
}

// seedPlanOrder writes one issue tree with a plan, and answers its root's key: PLAN-1, reserved
// for its author (reserveForAuthor), with its child PLAN-2, and a plan node of PLAN-1 that needs
// PLAN-2, indexed as a write of this build indexes it. PlanOrder reads the needs of the nodes its
// walk found: with no plan node the census would read only the walk.
func seedPlanOrder(t *testing.T, ctx context.Context, database *store.Store) string {
	t.Helper()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the plan order seed: %v", err)
	}
	defer tx.Rollback(ctx)
	const root = "PLAN-1"
	createReferenceProject(t, tx, "PLAN")
	createReferenceIssue(t, tx, root, "PLAN")
	if _, err := tx.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank, parent_key)
		values ('PLAN-2', 'PLAN', 2, 'The child', '{"kind":"user","id":"alice"}', 'U', $1)
	`, root); err != nil {
		t.Fatalf("create the child of %s: %v", root, err)
	}
	reserveForAuthor(t, tx, graphstore.KindIssue, root)
	artifact, _ := createReferenceArtifact(t, tx, root, "PLAN", "spec")
	markdown, blocks := documentOf([]part{{"t1", planTaskType, "After the child."}})
	at := writeAuthoredVersion(t, tx, artifact, 1, markdown, privateAuthor)
	pointers := []Pointer{{Block: "t1", BlockType: planTaskType, Kind: "needs",
		Target: text.Ref{Kind: graphstore.KindIssue, IssueKey: "PLAN-2", ID: "PLAN-2"}, Targets: []string{planTaskType}}}
	_, _, write, _, err := IndexBlocks(ctx, tx, artifact, OneVersion(artifact, 1, at), markdown, blocks, nil, pointers, TypedFacts{})
	if err != nil {
		t.Fatalf("index the plan of %s: %v", root, err)
	}
	if err := graphstore.Caused(ctx, tx, write, graphstore.NoEvent); err != nil {
		t.Fatalf("append what the plan of %s stated: %v", root, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the plan order seed: %v", err)
	}
	return root
}

// writerSigned names each exported read of the store whose model.Actor is the writer that signs
// the facts it answers for a write to append, not a viewer: PreventionRetractions answers the
// retractions a writer appends, each signed by that writer, and answers them to no reader.
// storeReads counts such a read among the reads that take no acting person, so
// readsWithoutAViewer must name it with its reason.
var writerSigned = []string{"PreventionRetractions"}

// storeReads is every exported function of this package that reads the store, read out of the
// package's own source rather than kept as a list: one that takes the Queryer it reads on, or
// the *pgx.Batch it queues its statement in. viewing is those that take a model.Actor beside it,
// or a *model.Actor where nil is no person (ResolvePrefixes), the reads of the graph for a
// viewer, and without is the rest, a read of writerSigned among them. A method is named by its
// receiver's type and its own name.
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
				case "Queryer", "graphstore.Queryer", "*pgx.Batch":
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
			if viewer && !slices.Contains(writerSigned, name) {
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
