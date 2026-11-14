package graphstore

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// moduleRoot is the packages/envoy module, read from this package's own directory.
const moduleRoot = "../../.."

// graphstoreImportPath is the path a file outside this package names the store by.
const graphstoreImportPath = "github.com/sjawhar/envoy/internal/dispatch/graphstore"

// graphstoreDirectory is this package's own directory under moduleRoot, where its writers are
// named without a package.
const graphstoreDirectory = "internal/dispatch/graphstore"

// storeWriteFunctions are the calls a write of graph_nodes and graph_tuples from outside this
// package goes through, the same six docs' register of the tree's passes scans for
// (storeWriterCall): Write, WriteStating and Caused in a caller's transaction, WriteArms spliced
// into a caller's statement, and Authored and Reached, which run WriteArms for the writers that
// record an author and a reached session. A write that holds a statement of its own instead is
// found by rawStoreWrite.
var storeWriteFunctions = []string{"Write", "WriteStating", "WriteArms", "Caused", "Authored", "Reached"}

// rawStoreWrite matches a statement that writes graph_nodes or graph_tuples itself rather than
// through one of storeWriteFunctions: an insert, an update or a delete, in any case, over any
// whitespace, and under the schema's name as well as bare.
var rawStoreWrite = regexp.MustCompile(`(?is)\b(?:insert\s+into|update|delete\s+from)\s+(?:public\s*\.\s*)?graph_(?:nodes|tuples)\b`)

// storeWriteCallSites is every function outside the tests that calls one of storeWriteFunctions,
// or that outside this package holds a statement rawStoreWrite matches, named as docs' register
// names a pass: the file, from internal/dispatch where it lives under it, and the function,
// without its receiver. TestEveryStoreWriteTakesOneLockOrder proves the lock order on this
// package's own writers, and each site listed here has its lock steps proven in its own package
// against that order; a site that is not listed has had nobody prove its steps, so it fails here
// until it joins this list.
var storeWriteCallSites = []string{
	// The comment path: the comment's node before the comments row that names it, then its
	// tuples and its cause under the event appended after both, and who wrote it.
	"api/comment_create.go#caused",
	"api/comment_create.go#createCommentFor",
	"api/comment_create.go#writeNode",
	// A delivery's reply and the settle of a delivery attempt, which records the session the
	// attempt reached.
	"api/comment_delivery.go#completeCommentDelivery",
	"api/comment_delivery.go#replyComment",
	"api/message_delivery.go#completeMessageDelivery",
	// The seed's comments, with their author's person node and each comment's authored_by
	// tuple, which its one batch write states as a seeded database's own writer.
	"cmd/dispatch-seed/write_items.go#writeComments",
	// The backfill command's four batches: the blocks, the block references and the comments
	// written before the store existed, and the role and page nodes of the roles the census
	// holds to one (addressedRoles).
	"graphcensus/backfill.go#writeBlockBatch",
	"graphcensus/backfill.go#writeBlockRefBatch",
	"graphcensus/backfill.go#writeCommentBatch",
	"graphcensus/backfill.go#writeRoleBatch",
	// The erase, the one writer that deletes from the store, with statements of its own rather
	// than through a writer: the tuples on both sides of the node, then the node's row. It takes
	// the owner rows it writes under before that row, and its projection rows after it
	// (graphcensus's TestTheEraseTakesTheStoreLockOrder).
	"graphcensus/erase.go#apply",
	// This package's own writers that run another of them: Write is WriteStating and the
	// append of what it states, and Authored and Reached are arms of one statement.
	"graphstore/people.go#Authored",
	"graphstore/people.go#Reached",
	"graphstore/write.go#Write",
	// The publisher's record of a batch, which splices the store's arms into its statement.
	"outbox/publisher.go#recordBatch",
	// The block index: its nodes inside the revision fence, and its tuples and their cause
	// under the event that recorded the version.
	"refs/blocks.go#Caused",
	"refs/blocks.go#IndexBlocks",
}

// TestEveryStoreWriteCallSiteIsListed reads the module's source, every Go file but the tests,
// and finds each function that names one of storeWriteFunctions, called or passed on as a value,
// and each function outside this package whose string literals hold a statement writing
// graph_nodes or graph_tuples (rawStoreWrite). It fails on a site storeWriteCallSites does not
// list, naming the site and the call or the statement, so a new writer of the store cannot appear
// without its lock steps being proven, and on a listed site the source no longer holds, so the
// list never names a writer that is gone. It parses the source rather than matching its text, so
// a call written over two lines, a comment that names a writer or spells a statement, and a
// method of another type that shares a writer's name are each read for what they are.
func TestEveryStoreWriteCallSiteIsListed(t *testing.T) {
	found := map[string][]string{}
	parsed := 0
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
		// A file that names the store names it by its import path or is the store's own
		// package, and both spell the word; a file that writes the store's tables itself spells
		// one of their names, in whatever case its statement is written in. The rest of the
		// module is never parsed.
		lowered := bytes.ToLower(source)
		if !bytes.Contains(source, []byte("graphstore")) &&
			!bytes.Contains(lowered, []byte("graph_nodes")) && !bytes.Contains(lowered, []byte("graph_tuples")) {
			return nil
		}
		relative, err := filepath.Rel(moduleRoot, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		files := token.NewFileSet()
		file, err := parser.ParseFile(files, relative, source, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", relative, err)
		}
		parsed++
		for _, sites := range []map[string][]string{
			storeWriteSitesIn(relative, files, file),
			rawStoreWritesIn(relative, files, file),
		} {
			for site, calls := range sites {
				found[site] = append(found[site], calls...)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read the module's source under %s: %v", moduleRoot, err)
	}
	if parsed == 0 || len(found) == 0 {
		t.Fatalf("read %d files naming graphstore or its tables and found %d store writers: the walk read nothing worth checking", parsed, len(found))
	}
	t.Logf("read %d files naming graphstore or its tables under %s and found %d store writer sites", parsed, moduleRoot, len(found))

	sites := make([]string, 0, len(found))
	for site := range found {
		sites = append(sites, site)
	}
	slices.Sort(sites)
	for _, site := range sites {
		t.Logf("store writer %s (%s)", site, strings.Join(found[site], ", "))
		if !slices.Contains(storeWriteCallSites, site) {
			t.Errorf("%s writes the store (%s) and is not in storeWriteCallSites: a new writer of graph_nodes and graph_tuples takes the lock order written on api/inbox_read.go's markInboxReadThreads, so its steps are proven in its own package and then it is listed here",
				site, strings.Join(found[site], ", "))
		}
	}
	for _, site := range storeWriteCallSites {
		if _, ok := found[site]; !ok {
			t.Errorf("storeWriteCallSites lists %s, and no file of the module holds a call of a store writer or a statement writing the store there any more: take it off the list, since the proofs of the writers rest on it naming the ones there are", site)
		}
	}
}

// skippedDirectory is a directory the walk does not enter: one Go itself ignores (testdata, a
// name starting with a dot or an underscore), a vendored or installed tree, and another module.
func skippedDirectory(path, name string) bool {
	if name == "testdata" || name == "vendor" || name == "node_modules" ||
		strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
		return true
	}
	_, err := os.Stat(filepath.Join(path, "go.mod"))
	return err == nil
}

// storeWriteSitesIn is every site one file names a store writer at, each with the calls it makes
// as "graphstore.<writer> at <file>:<line>". In the store's own package the writers are named
// bare; anywhere else through the name the file imports graphstore under.
func storeWriteSitesIn(relative string, files *token.FileSet, file *ast.File) map[string][]string {
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
	if !bare && qualifier == "" {
		return nil
	}

	sites := map[string][]string{}
	for _, declaration := range file.Decls {
		site := siteKey(relative, "package-level declaration")
		var body ast.Node = declaration
		if decl, ok := declaration.(*ast.FuncDecl); ok {
			site = siteKey(relative, decl.Name.Name)
			if decl.Body == nil {
				continue
			}
			body = decl.Body
		}
		record := func(name *ast.Ident) {
			line := files.Position(name.Pos()).Line
			sites[site] = append(sites[site], "graphstore."+name.Name+" at "+relative+":"+strconv.Itoa(line))
		}
		var visit func(node ast.Node) bool
		visit = func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.SelectorExpr:
				if x, ok := node.X.(*ast.Ident); ok && qualifier != "" && x.Name == qualifier &&
					slices.Contains(storeWriteFunctions, node.Sel.Name) {
					record(node.Sel)
					return false
				}
				// The name after a dot is a field or a method of something else, never the
				// store's own function, so only the expression before it is read on.
				ast.Inspect(node.X, visit)
				return false
			case *ast.Ident:
				// Bare, a writer is named only in the store's own package or in a file that
				// dot-imports it.
				if bare && slices.Contains(storeWriteFunctions, node.Name) {
					record(node)
				}
			}
			return true
		}
		ast.Inspect(body, visit)
	}
	return sites
}

// rawStoreWritesIn is every site one file writes the store at with a statement of its own, each
// with the statements it holds as "<statement> at <file>:<line>", read from the file's string
// literals alone, so a statement a comment spells is none. The store's own package writes its
// tables that way by design, through the writers storeWriteFunctions names, so its files are
// not read here.
func rawStoreWritesIn(relative string, files *token.FileSet, file *ast.File) map[string][]string {
	if strings.HasPrefix(relative, graphstoreDirectory+"/") {
		return nil
	}
	sites := map[string][]string{}
	for _, declaration := range file.Decls {
		site := siteKey(relative, "package-level declaration")
		if decl, ok := declaration.(*ast.FuncDecl); ok {
			site = siteKey(relative, decl.Name.Name)
		}
		ast.Inspect(declaration, func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			text, err := strconv.Unquote(literal.Value)
			if err != nil {
				return true
			}
			for _, match := range rawStoreWrite.FindAllStringIndex(text, -1) {
				// A raw string holds its lines as the source does, so a statement inside one is
				// on the literal's line plus the line breaks before it.
				line := files.Position(literal.Pos()).Line
				if strings.HasPrefix(literal.Value, "`") {
					line += strings.Count(text[:match[0]], "\n")
				}
				statement := strings.ToLower(strings.Join(strings.Fields(text[match[0]:match[1]]), " "))
				sites[site] = append(sites[site], statement+" at "+relative+":"+strconv.Itoa(line))
			}
			return true
		})
	}
	return sites
}

// siteKey names a site as docs' register of the tree's passes names one (passKey): the file,
// from internal/dispatch where it lives under it and from the module otherwise, and the function.
func siteKey(relative, function string) string {
	return strings.TrimPrefix(relative, "internal/dispatch/") + "#" + function
}
