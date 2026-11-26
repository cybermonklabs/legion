package graphstore

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// headWriteFunctions are the head writes (heads.go): the calls a writer of a node, or of a value or
// a relation out of a node, of an issue's tree names beside its store write, so the node's rows of
// issue_head_nodes are read again from the store in the writer's transaction.
var headWriteFunctions = []string{"QueueHeads", "WriteHeads"}

// headNoTree are the store writers (storeWriteCallSites) that write no node of an issue's tree,
// each beside why: every node they write, and the near end of every tuple they append, is a node no
// issue's tree holds - a person, a session, a role and its page, a run of the command bar and its
// frames, a component, a node of the lens catalogue. headWrappers are this package's own writers
// that run another writer for their caller, which writes the head rows (Write) or whose caller names
// the head write itself (QueueAuthored). Both lists are exact: an entry the source no longer holds
// as a store writer fails, and so does an entry that names a head write after all.
var headNoTree = map[string]string{
	"graphstore/people.go#QueueReached":                           "writes a session's node alone",
	"graphstore/people.go#QueueSessionRecorded":                   "writes a session's node alone",
	"graphstore/interaction.go#WriteRun":                          "writes a run's interaction node, its frames and its author's node",
	"graphstore/lens_catalogue.go#AddLensKind":                    "writes a lens kind and its constraint, nodes of the catalogue",
	"graphcensus/backfill_roles.go#writeRoleBatch":                "writes role and page nodes",
	"graphcensus/backfill_sessions.go#writeSessionBatch":          "writes session nodes",
	"graphcensus/backfill_runs.go#writeOwedRun":                   "writes a run's interaction node through WriteRun",
	"graphcensus/backfill_runs.go#writeGoneRun":                   "states a run's interaction node gone",
	"graphcensus/backfill_runs.go#writeRunFrames":                 "appends a run's frame values",
	"api/going.go#appendGoingEvent":                               "appends a run's frame value",
	"api/going.go#lockGoingRun":                                   "writes a run's interaction node through WriteRun",
	"api/going.go#recordGoingRequest":                             "writes a run's interaction node and its author's node",
	"api/going.go#goRunGone":                                      "states a run's interaction node gone",
	"architecture/importer.go#stateComponents":                    "writes component nodes and their part_of and depends_on",
	"architecture/importer.go#RetireComponents":                   "states component nodes gone and retracts their relations",
	"graphcensus/census_components.go#writeComponentBatch":        "writes component nodes and their part_of",
	"graphcensus/census_components.go#writeComponentDependsBatch": "appends the depends_on of component nodes",
	"api/comment_delivery.go#completeCommentDelivery":             "records the session a delivery attempt reached (QueueReached)",
	"api/message_delivery.go#completeMessageDelivery":             "records the session a delivery attempt reached (QueueReached)",
	"cmd/dispatch-seed/write_items.go#writeFollowerNodes":         "writes the session nodes of the followers the seed copies",
	"docs/removed_by.go#queueRemovals":                            "writes the node of the person who removed an answered ask's block; the removed_by it appends rides in the version's block index, whose cause sites write its heads",
	"asks/followers.go#queueFollow":                               "writes the following session's node alone; its callers state the ask's followed_by and write the ask's heads",
	"cmd/dispatch-seed/write_core.go#writeArchitecture":           "writes component nodes and their part_of and depends_on, as an import does",
	"seedcorpus/agents.go#writeRoles":                             "writes role and page nodes, each page's part_of its role, a role's held_by and its holder's part_of the role",
	"seedcorpus/claims.go#EndClaim":                               "retracts a role's held_by",
	"seedcorpus/claims.go#RestoreClaim":                           "appends a role's held_by",
	"seedcorpus/nobody.go#writeNobodysRun":                        "writes a run's interaction node through WriteRun",
}

// Write and Caused are on none of the lists: each queues the head rows of what it wrote in its own
// batch, after its statements (QueueHeads), since each statement of the write claims those rows
// kept (KeptHeadRows), so the batch that makes the claim writes them and the scan reads each as a
// writer that names a head write. WriteState and Retarget write through Write.
var headWrappers = map[string]string{
	"graphstore/write.go#WriteState":     "states its caller's nodes through Write, which writes their head rows",
	"graphstore/people.go#QueueAuthored": "queues the authored_by of its caller's node, and its caller names the head write",
	"graphstore/reasoning.go#Retarget":   "moves its caller's pointer through Write, which writes its head row",
}

// headLeftTo are the store writers that write a node of an issue's tree, or hold its facts, and
// leave its head row to the sites that append those facts under their cause later in the same
// transaction, each beside those cause sites: a node row written before the projection row that
// names it and the event that causes its tuples (graphstore.PlanUncaused, QueueNodes), whose tuples
// QueueCaused appends at the cause site, which then writes the heads. Each named cause site names a
// head write, or the test fails naming both.
var headLeftTo = map[string][]string{
	"refs/blocks.go#Queue": {"api/artifacts.go#storeArtifact", "api/artifacts.go#commitArtifactVersionEvent",
		"api/issue_create.go#createIssue", "docs/block_index.go#indexOwedDocument", "docs/block_index.go#causeRoomFacts",
		"docs/settlement.go#appendEvents", "cmd/dispatch-seed/write_docs.go#writeDocument"},
	"refs/decision_nodes.go#Queue": {"docs/block_index.go#indexOwedDocument", "docs/block_index.go#causeRoomFacts",
		"docs/settlement.go#appendEvents"},
	"docs/mutation.go#WriteVersionNode": {"api/artifacts.go#storeArtifact", "api/artifacts.go#commitArtifactVersionEvent",
		"api/issue_create.go#createIssue", "docs/settlement.go#appendEvents"},
	"api/artifacts.go#createIssueTree":           {"api/issue_create.go#createIssue"},
	"api/artifacts.go#queueNewArtifact":          {"api/artifacts.go#storeArtifact"},
	"api/comment_create.go#queueCommentRow":      {"api/comment_create.go#queueCommentCreated"},
	"api/issue_create.go#createDecisionsQuery":   {"api/issue_create.go#createIssue"},
	"api/broadcasts.go#writeBroadcast":           {"api/server.go#appendCausingEvent"},
	"api/message_delivery.go#insertSessionReply": {"api/server.go#appendCausingEvent"},
	"docs/retire.go#writeRetiredNode":            {"docs/retire.go#RetireDocument"},
	"docs/ask_blocks.go#createAskBlock":          {"docs/settlement.go#appendEvents"},
	"api/issue_relations.go#queueIssueRelations": {"api/issue_create.go#createIssue", "api/issue_patch.go#writeIssue"},
	"cmd/dispatch-seed/write_items.go#writeItemNodes": {"cmd/dispatch-seed/write_items.go#writeAsks",
		"cmd/dispatch-seed/write_items.go#writeMessages"},
}

// headVerdicts are the failures of the scan's verdict over found, every store writer by its site
// and the calls it makes, and heads, every site that names a head write: each writer of a tree node
// that names no head write and is on none of the lists, each writer whose named cause site writes
// none, and each list entry the scan does not hold or that names a head write after all. passing
// counts the writers that name a head write.
func headVerdicts(found map[string][]string, heads map[string]bool) (failures []string, passing int) {
	for _, site := range slices.Sorted(maps.Keys(found)) {
		_, noTree := headNoTree[site]
		_, wrapper := headWrappers[site]
		causes, leftTo := headLeftTo[site]
		switch {
		case (noTree || wrapper || leftTo) && heads[site]:
			failures = append(failures, site+" names a head write, and a list of this test names it as a writer that writes none: take it off the list")
		case noTree || wrapper:
		case leftTo:
			for _, cause := range causes {
				if !heads[cause] {
					failures = append(failures, site+" leaves its head row to "+cause+", which writes none")
				}
			}
		case !heads[site]:
			failures = append(failures, site+" writes a value or a relation out of a tree node and no head row")
		default:
			passing++
		}
	}
	for _, list := range []map[string]bool{keysOf(headNoTree), keysOf(headWrappers), keysOf(headLeftTo)} {
		for _, site := range slices.Sorted(maps.Keys(list)) {
			if _, ok := found[site]; !ok {
				failures = append(failures, "a list of this test names "+site+", and no file of the module holds a store writer there any more: take it off the list")
			}
		}
	}
	return failures, passing
}

// keysOf is the keys of a list.
func keysOf[V any](list map[string]V) map[string]bool {
	keys := make(map[string]bool, len(list))
	for key := range list {
		keys[key] = true
	}
	return keys
}

// headWriteSitesIn is every function of one file that names a head write, by its site key.
func headWriteSitesIn(relative string, files *token.FileSet, file *ast.File) map[string]bool {
	bare := filepath.ToSlash(filepath.Dir(relative)) == graphstoreDirectory && file.Name.Name == "graphstore"
	qualifier := ""
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil || path != graphstoreImportPath {
			continue
		}
		switch {
		case spec.Name == nil:
			qualifier = "graphstore"
		case spec.Name.Name == ".":
			bare = true
		case spec.Name.Name != "_":
			qualifier = spec.Name.Name
		}
	}
	sites := map[string]bool{}
	if !bare && qualifier == "" {
		return sites
	}
	for _, declaration := range file.Decls {
		decl, ok := declaration.(*ast.FuncDecl)
		if !ok || decl.Body == nil {
			continue
		}
		site := siteKey(relative, decl.Name.Name)
		var visit func(node ast.Node) bool
		visit = func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.SelectorExpr:
				if x, ok := node.X.(*ast.Ident); ok && qualifier != "" && x.Name == qualifier && slices.Contains(headWriteFunctions, node.Sel.Name) {
					sites[site] = true
					return false
				}
				ast.Inspect(node.X, visit)
				return false
			case *ast.Ident:
				if bare && slices.Contains(headWriteFunctions, node.Name) {
					sites[site] = true
				}
			}
			return true
		}
		ast.Inspect(decl.Body, visit)
	}
	return sites
}

// headScan is what the scan read of the module's source: every store writer by its site with the
// calls it makes, every site that names a head write, and each file's own reading, by the file, so a
// copy of one file can be read in its place.
type headScan struct {
	found map[string][]string
	heads map[string]bool
	files map[string]headFileReading
}

// headFileReading is one file's store writers and head writers.
type headFileReading struct {
	found map[string][]string
	heads map[string]bool
}

// readHeadFile reads one file's source as the scan reads it.
func readHeadFile(t *testing.T, relative string, source []byte) headFileReading {
	t.Helper()
	files := token.NewFileSet()
	file, err := parser.ParseFile(files, relative, source, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", relative, err)
	}
	return headFileReading{found: storeWriteSitesIn(relative, files, file), heads: headWriteSitesIn(relative, files, file)}
}

// readHeadScan reads the module's source, every Go file but the tests that names graphstore.
func readHeadScan(t *testing.T) headScan {
	t.Helper()
	scan := headScan{files: map[string]headFileReading{}}
	err := filepath.WalkDir(moduleRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != moduleRoot && skippedDirectory(path, entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Contains(source, []byte("graphstore")) {
			return nil
		}
		relative, err := filepath.Rel(moduleRoot, path)
		if err != nil {
			return err
		}
		scan.files[filepath.ToSlash(relative)] = readHeadFile(t, filepath.ToSlash(relative), source)
		return nil
	})
	if err != nil {
		t.Fatalf("read the module's source under %s: %v", moduleRoot, err)
	}
	return scan.with("", headFileReading{})
}

// with is the scan with relative's reading replaced by reading, its writers and head writers
// gathered again; relative "" replaces nothing.
func (scan headScan) with(relative string, reading headFileReading) headScan {
	gathered := headScan{found: map[string][]string{}, heads: map[string]bool{}, files: scan.files}
	for file, own := range scan.files {
		if file == relative {
			own = reading
		}
		for site, calls := range own.found {
			gathered.found[site] = append(gathered.found[site], calls...)
		}
		maps.Copy(gathered.heads, own.heads)
	}
	return gathered
}

// TestEveryWriterOfAHeadFactWritesTheHeadRow reads the module's source as
// TestEveryStoreWriteCallSiteIsListed reads it, every Go file but the tests, and finds each
// function that names one of the store's writers (storeWriteFunctions): each site that writes a node
// row, or appends a value or a relation tuple out of a node. The head row of a node of an issue's tree
// holds the node's row and every value and relation out of it (issue_head_nodes, HeadRowsSQL), so
// each such site names a head write (headWriteFunctions) in the same function, after its writes. It
// fails on each that names none, printing `<site> writes a value or a relation out of a tree node and
// no head row`, but for the writers of no tree node (headNoTree) and this package's wrappers
// (headWrappers), each printed with why, and the node writes whose cause sites write their heads
// (headLeftTo), each printed with those sites, of which each names a head write or the test fails
// printing `<site> leaves its head row to <cause site>, which writes none`. A list entry the source
// holds no store writer at, or that names a head write after all, fails too.
//
// The same verdict over two copies goes into the log: one file with a writer planted that writes no
// head row, which the verdict finds, and one named cause site with its head write taken out, which
// the verdict names beside the writer that leaves its head row to it. A writer that names no head
// write leaves a node's rows behind the store under the mark its own build keeps, since every
// statement of this build that writes the store sets dispatch.head_rows, and the head read would
// answer the stale rows.
func TestEveryWriterOfAHeadFactWritesTheHeadRow(t *testing.T) {
	scan := readHeadScan(t)
	if len(scan.found) == 0 {
		t.Fatalf("found no store writer under %s: the walk read nothing worth checking", moduleRoot)
	}
	for _, site := range slices.Sorted(maps.Keys(headNoTree)) {
		t.Logf("store writer %s writes no node of an issue's tree: %s", site, headNoTree[site])
	}
	for _, site := range slices.Sorted(maps.Keys(headWrappers)) {
		t.Logf("store writer %s is a writer of this package: %s", site, headWrappers[site])
	}
	for _, site := range slices.Sorted(maps.Keys(headLeftTo)) {
		t.Logf("store writer %s leaves its head row to %s", site, strings.Join(headLeftTo[site], ", "))
	}
	failures, passing := headVerdicts(scan.found, scan.heads)
	for _, failure := range failures {
		t.Error(failure)
	}
	t.Logf("read %d store writers: %d name a head write, %d write no tree node, %d are wrappers, %d leave their head row to a cause site",
		len(scan.found), passing, len(headNoTree), len(headWrappers), len(headLeftTo))

	// A writer planted in a copy of one file, which writes a node and no head row.
	const plantedFile = "internal/dispatch/api/results_retract.go"
	source, err := os.ReadFile(filepath.Join(moduleRoot, plantedFile))
	if err != nil {
		t.Fatalf("read %s: %v", plantedFile, err)
	}
	planted := append(slices.Clone(source), []byte("\nfunc plantedHeadlessWriter(ctx context.Context, tx pgx.Tx) error {\n\treturn graphstore.Write(ctx, tx, nil, nil)\n}\n")...)
	plantedScan := scan.with(plantedFile, readHeadFile(t, plantedFile, planted))
	plantedFailures, _ := headVerdicts(plantedScan.found, plantedScan.heads)
	want := "api/results_retract.go#plantedHeadlessWriter writes a value or a relation out of a tree node and no head row"
	t.Logf("the copy of %s with a writer planted reads: %s", plantedFile, strings.Join(plantedFailures, "; "))
	if !slices.Contains(plantedFailures, want) {
		t.Errorf("the copy of %s with a writer planted that writes no head row reads %q, want %q among it", plantedFile, plantedFailures, want)
	}

	// The head write taken out of a copy of one named cause site.
	const causeFile = "internal/dispatch/api/server.go"
	source, err = os.ReadFile(filepath.Join(moduleRoot, causeFile))
	if err != nil {
		t.Fatalf("read %s: %v", causeFile, err)
	}
	headWrite := []byte("\t\tgraphstore.QueueHeads(b, graphstore.HeldHeads(stated))\n")
	if !bytes.Contains(source, headWrite) {
		t.Fatalf("%s holds no line %q to take out", causeFile, headWrite)
	}
	causeScan := scan.with(causeFile, readHeadFile(t, causeFile, bytes.Replace(source, headWrite, nil, 1)))
	causeFailures, _ := headVerdicts(causeScan.found, causeScan.heads)
	want = "api/broadcasts.go#writeBroadcast leaves its head row to api/server.go#appendCausingEvent, which writes none"
	t.Logf("the copy of %s with the head write of appendCausingEvent taken out reads: %s", causeFile, strings.Join(causeFailures, "; "))
	if !slices.Contains(causeFailures, want) {
		t.Errorf("the copy of %s with the head write taken out of appendCausingEvent reads %q, want %q among it", causeFile, causeFailures, want)
	}
}
