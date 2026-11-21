package api

import (
	"fmt"
	"go/ast"
	"go/printer"
	"go/token"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// versionRowInsert matches a statement that writes an artifact_versions row, wherever in the
// statement it sits, a data-modifying common table expression's included, in either spelling a
// row reaches the table by: an insert, or a COPY sent as text.
var versionRowInsert = regexp.MustCompile(`\b(?:insert into|copy) (?:public\.)?artifact_versions\b`)

// versionRowWriter is one entry of the census: whether the writer stores the version's insert tree
// (artifact_versions.insert_tree) in the write that stores its row, and why, or why it stores none.
type versionRowWriter struct {
	fills bool
	why   string
}

// versionTreeRule is what a writer the census does not name, or one that stopped filling the
// column, is told.
const versionTreeRule = "Every write of a document version stores, in the same write, the tree its markdown was\n" +
	"rendered from when that tree holds an insert suggestion's mark (docs.InsertTree into\n" +
	"artifact_versions.insert_tree): the markdown carries no anchor mark, so a version stored without it\n" +
	"gives the words of a pending insert whose comment was deleted to every read (docs.MaskedVersionText,\n" +
	"dispatch://INFRA-1523 R3). Name insert_tree in the write, then add the function below with how it\n" +
	"fills the column - or, for a row that holds no tree at all, why it stores none."

// versionRowWriters is the census: every function of this module that writes an artifact_versions
// row, with how it fills insert_tree. A writer the scan finds and this map does not name reds the
// test; so does an entry marked fills whose write names no insert_tree, and an entry no writer
// answers, since a census that keeps a function it no longer covers stops saying anything.
//
// Every document version a server writes goes through versionInsert: queueVersionInsert's one
// insert, behind docs' writeVersionTx - a settlement (docs/settlement.go), a named version
// (Service.NamedVersion: the version route's, an accept's), a comment anchor's snapshot
// (Service.SnapshotVersion) and a rebuild (docs/rebuild.go) - and behind Ledger.QueueVersion, an
// upload's (api/artifacts.go) and an issue's first spec (api/issue_create.go). A live edit, a
// reject and a restore write the document through applyLive and reach a version through those.
// That one statement names insert_tree for every one of them, so what each stores in it is the
// stamp it passes, which the census's second half reads (versionStamps).
var versionRowWriters = map[string]map[string]versionRowWriter{
	"cmd/dispatch-seed/write_docs.go": {
		"writeDocument": {fills: true, why: "the seed's copy of every version of a document: the latest " +
			"version's tree is the one renderVersion rendered, through docs.InsertTree; an earlier version " +
			"is generated markdown no tree was read from, which holds no span a parse reads as a mark"},
	},
	"internal/dispatch/api/artifacts.go": {
		"queueFileVersion": {why: "a file's or an image's version, which stores bytes and no markdown: " +
			"there is no tree, so no insert suggestion's words"},
	},
	"internal/dispatch/docs/mutation.go": {
		"versionInsert": {fills: true, why: "every document version a server writes, its stamp's tree " +
			"(RenderStamp.InsertTree), each stamp on versionStamps"},
	},
	"internal/dispatch/textversions/textversionstest/textversionstest.go": {
		"insertVersion": {why: "a test's version of an issue's primary document for the keyword fill's tests: " +
			"its markdown is a test string no tree was rendered from, so it holds no insert suggestion's words"},
	},
}

// versionStampRule is what a stamp the census does not name, or one that stopped filling the
// column, is told.
const versionStampRule = "What versionInsert stores in insert_tree is the docs.RenderStamp its caller passes\n" +
	"(RenderStamp.InsertTree): a statement that names the column stores null for a stamp made without the\n" +
	"tree, as the version of an upload that changed nothing did (dispatch://INFRA-1523, census v20, V.1).\n" +
	"Fill InsertTree from the tree the version's markdown renders from (renderStampOf, unchangedStampOf),\n" +
	"then add the site below with how it fills the column - or why the stamp it makes holds no tree."

// versionStamps is the census's second half: every site of this module that makes or passes on a
// docs.RenderStamp (stampSites) - each return of a function answering one, and each call of such a
// function, RenderStamp literal and `var` of that type elsewhere - by its file, its function and
// its source text, with how the stamp fills insert_tree. The first half reads the statement, and
// versionInsert's names the column for every writer behind it, so it answered fills for all of
// them alike; but the value is the stamp the caller passes, and an upload that changed nothing
// passed the empty stamp (Ledger.RenderStampOf, its write having recorded no tree) through that
// statement, storing null (dispatch://INFRA-1523, census v20, V.1, V.4, V.6, V.11, V.13). A site
// the scan finds and this map does not name reds the test; so does an entry marked fills whose
// stamp is a literal setting no InsertTree, and an entry no site answers - the last is what reds
// when a path that filled the column stops passing its stamp, as that path's own site goes.
var versionStamps = map[string]map[string]map[string]versionRowWriter{
	"internal/dispatch/docs/ledger.go": {
		"QueueVersion": {
			"l.RenderStampOf(artifactID, markdown)": {fills: true, why: "an upload's version and an issue's " +
				"first spec: the stamp of the tree the transaction's write left, or found where it changed nothing"},
		},
	},
	"internal/dispatch/docs/mutation.go": {
		"writeVersionTx": {
			"renderStampOf(artifactID, tree)": {fills: true, why: "every version writeVersionTx writes - a " +
				"settlement, a named version, a snapshot, the delete's settle, a rebuild - from the tree its " +
				"markdown was rendered from, which each of its callers renders"},
		},
		"ReplaceText": {
			"unchangedStampOf(artifactID, unchangedTree)": {fills: true, why: "an upload that changes nothing: it " +
				"records no operation, so its transaction holds no tree it wrote, and the stamp of the live tree " +
				"it found rendering as the replacement is kept for its version (Ledger.wroteUnchanged)"},
		},
	},
	"internal/dispatch/docs/render_stamp.go": {
		"renderStampOf": {
			"RenderStamp{}": {why: "a nil tree, which holds no mark; no version write passes one, since " +
				"writeVersionTx's callers each render a tree and RenderStampOf passes the one it holds"},
			"var stamp RenderStamp": {fills: true, why: "the stamp of a tree, whose insert tree the function " +
				"sets (insertTreeOf) beside its token and renderer version"},
			"stamp": {fills: true, why: "the tree's token, renderer version and insert tree (insertTreeOf)"},
		},
		"unchangedStampOf": {
			"RenderStamp{InsertTree: insertTreeOf(artifactID, tree)}": {fills: true, why: "the insert tree " +
				"of the live tree an upload that changed nothing read, and no token, since the head its " +
				"version's insert reads need not be that tree's state"},
		},
		"RenderStampOf": {
			"renderStampOf(artifactID, rendered.tree)": {fills: true, why: "the tree the transaction's write " +
				"left, where it renders as the version's markdown: a changed upload's, a seed's"},
			"kept.stamp": {fills: true, why: "an upload that changed nothing: the stamp its write kept of the " +
				"live tree it found rendering as the version's markdown (unchangedStampOf)"},
			"RenderStamp{}": {why: "markdown that no tree the transaction wrote or found renders as; an upload " +
				"reaches it only where the fork its write left renders otherwise than its replacement, which " +
				"IndexDocumentReferences logs, and the anchors it carries over render nothing (pmdoc.Render)"},
		},
	},
}

// TestEveryWriterOfADocumentVersionStoresItsInsertTree holds the write half of the version mask
// (docs.MaskedVersionText): every write of an artifact_versions row is on the census, and each that
// stores a document version names insert_tree in the write that stores its row, so no version
// holding a pending insert's words is stored without the marks the mask finds them by; and every
// stamp such a write can store is on the census too (versionStamps), each that fills the column
// made from its version's tree.
//
// Its reach is the statement as sqlStatementsByFunction assembles it (module_source_test.go) - a
// literal, or a chain of literals and package-level constants - and a pgx CopyFrom whose table is
// a pgx.Identifier literal and whose columns are a []string literal (copiedVersionColumns). A copy
// whose columns are no literal is read as naming none, so it reds until its entry says why. The
// stamps are read off the syntax (stampSites): a function answers one by its declared result, and
// a site is named by its text, so a renamed variable reds until its entry follows. What the census
// does not read is whether the value a stamp carries is the tree: the rows of
// comment_delete_version_insert_test.go hold that, by reading what a write stored.
func TestEveryWriterOfADocumentVersionStoresItsInsertTree(t *testing.T) {
	sources := moduleSources(t)
	constants, err := stringConstants(sources)
	if err != nil {
		t.Fatal(err)
	}
	var unlisted, unfilled []string
	answered := map[string]map[string]bool{}
	account := func(path, function, write string, namesTree bool) {
		file, writer, listed := censusEntryIn(versionRowWriters, path, function)
		if !listed {
			unlisted = append(unlisted, fmt.Sprintf("%s %s: %s", path, function, write))
			return
		}
		if answered[file] == nil {
			answered[file] = map[string]bool{}
		}
		answered[file][function] = true
		if writer.fills && !namesTree {
			unfilled = append(unfilled, fmt.Sprintf("%s %s: %s", path, function, write))
		}
	}
	for _, source := range sources {
		path := filepath.ToSlash(source.path)
		for _, located := range sqlStatementsByFunction(source.file, constants) {
			// A statement outside any function is a declaration, not a writer: the function that
			// runs it resolves the constant and is found here.
			if located.function == "" || !versionRowInsert.MatchString(located.statement) {
				continue
			}
			account(path, located.function, located.statement, strings.Contains(located.statement, "insert_tree"))
		}
		for _, copied := range copiedVersionColumns(source.file) {
			account(path, copied.function, "CopyFrom artifact_versions ("+strings.Join(copied.columns, ", ")+")",
				slices.Contains(copied.columns, "insert_tree"))
		}
	}
	slices.Sort(unlisted)
	if unlisted = slices.Compact(unlisted); len(unlisted) > 0 {
		t.Errorf("a writer of a document version is on no census entry (versionRowWriters):\n\t%s\n%s",
			strings.Join(unlisted, "\n\t"), versionTreeRule)
	}
	slices.Sort(unfilled)
	if unfilled = slices.Compact(unfilled); len(unfilled) > 0 {
		t.Errorf("a writer of a document version stores no insert tree in its write:\n\t%s\n%s",
			strings.Join(unfilled, "\n\t"), versionTreeRule)
	}
	var stale []string
	for file, functions := range versionRowWriters {
		for function := range functions {
			if !answered[file][function] {
				stale = append(stale, file+" "+function)
			}
		}
	}
	slices.Sort(stale)
	if len(stale) > 0 {
		t.Errorf("a census entry answers no writer of a document version, so it says nothing:\n\t%s\n"+
			"Delete it, or name the function that writes the row now.", strings.Join(stale, "\n\t"))
	}
	checkVersionStamps(t, sources)
}

// checkVersionStamps is the census's second half (versionStamps): every stamp site of sources is
// on it, each entry marked fills is no literal that sets no InsertTree, and each entry answers a
// site.
func checkVersionStamps(t *testing.T, sources []moduleSource) {
	t.Helper()
	stampers := stampFunctions(sources)
	var unlisted, unfilled []string
	answered := map[[3]string]bool{}
	for _, source := range sources {
		path := filepath.ToSlash(source.path)
		for _, site := range stampSites(source, stampers) {
			file, entries, listed := censusEntryIn(versionStamps, path, site.function)
			stamp, named := entries[site.text]
			if !listed || !named {
				unlisted = append(unlisted, fmt.Sprintf("%s %s: %s", path, site.function, site.text))
				continue
			}
			answered[[3]string{file, site.function, site.text}] = true
			if stamp.fills && !stampLiteralSetsInsertTree(site.expression) {
				unfilled = append(unfilled, fmt.Sprintf("%s %s: %s", path, site.function, site.text))
			}
		}
	}
	slices.Sort(unlisted)
	if unlisted = slices.Compact(unlisted); len(unlisted) > 0 {
		t.Errorf("a stamp a document version can store is on no census entry (versionStamps):\n\t%s\n%s",
			strings.Join(unlisted, "\n\t"), versionStampRule)
	}
	slices.Sort(unfilled)
	if unfilled = slices.Compact(unfilled); len(unfilled) > 0 {
		t.Errorf("a stamp the census says fills insert_tree is a literal setting no InsertTree:\n\t%s\n%s",
			strings.Join(unfilled, "\n\t"), versionStampRule)
	}
	var stale []string
	for file, functions := range versionStamps {
		for function, sites := range functions {
			for text := range sites {
				if !answered[[3]string{file, function, text}] {
					stale = append(stale, file+" "+function+": "+text)
				}
			}
		}
	}
	slices.Sort(stale)
	if len(stale) > 0 {
		t.Errorf("a census entry answers no stamp a document version can store, so it says nothing:\n\t%s\n"+
			"Delete it, or name the site that makes or passes the stamp now: a path whose site went no longer\n"+
			"passes its stamp to its version.\n%s", strings.Join(stale, "\n\t"), versionStampRule)
	}
}

// versionStampSite is one site stampSites finds: the top-level function it is in, its source text,
// and the expression it is, nil for a declaration.
type versionStampSite struct {
	function   string
	text       string
	expression ast.Expr
}

// namesRenderStamp reports whether a type expression names docs.RenderStamp: RenderStamp in its
// own package, docs.RenderStamp in another.
func namesRenderStamp(expression ast.Expr) bool {
	switch typed := expression.(type) {
	case *ast.Ident:
		return typed.Name == "RenderStamp"
	case *ast.SelectorExpr:
		return typed.Sel.Name == "RenderStamp"
	}
	return false
}

// stampFunctions is the name of every function and method of sources whose one result is a
// docs.RenderStamp.
func stampFunctions(sources []moduleSource) map[string]bool {
	functions := map[string]bool{}
	for _, source := range sources {
		for _, declared := range source.file.Decls {
			fn, ok := declared.(*ast.FuncDecl)
			if !ok || fn.Type.Results == nil || len(fn.Type.Results.List) != 1 || len(fn.Type.Results.List[0].Names) > 1 {
				continue
			}
			if namesRenderStamp(fn.Type.Results.List[0].Type) {
				functions[fn.Name.Name] = true
			}
		}
	}
	return functions
}

// stampSites is every site of source's functions that makes or passes on a RenderStamp: each
// return of a function functions names, outside the function literals in it, and, among the
// rest, each call of such a function, each RenderStamp literal and each `var` of that type, which
// is the zero stamp until its function sets it.
func stampSites(source moduleSource, functions map[string]bool) []versionStampSite {
	var sites []versionStampSite
	site := func(function, prefix string, node ast.Node) {
		var text strings.Builder
		text.WriteString(prefix)
		if err := printer.Fprint(&text, source.positions, node); err != nil {
			text.WriteString("unprintable: " + err.Error())
		}
		expression, _ := node.(ast.Expr)
		sites = append(sites, versionStampSite{function: function, text: text.String(), expression: expression})
	}
	for _, declared := range source.file.Decls {
		fn, ok := declared.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		returned := map[ast.Node]bool{}
		if functions[fn.Name.Name] {
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				switch node := node.(type) {
				case *ast.FuncLit:
					return false
				case *ast.ReturnStmt:
					if len(node.Results) == 1 {
						returned[node.Results[0]] = true
						site(fn.Name.Name, "", node.Results[0])
					}
				}
				return true
			})
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			if returned[node] {
				return true
			}
			switch node := node.(type) {
			case *ast.CallExpr:
				name := ""
				switch called := node.Fun.(type) {
				case *ast.Ident:
					name = called.Name
				case *ast.SelectorExpr:
					name = called.Sel.Name
				}
				if functions[name] {
					site(fn.Name.Name, "", node)
				}
			case *ast.CompositeLit:
				if namesRenderStamp(node.Type) {
					site(fn.Name.Name, "", node)
				}
			case *ast.ValueSpec:
				if node.Type != nil && namesRenderStamp(node.Type) {
					site(fn.Name.Name, "var ", node)
				}
			}
			return true
		})
	}
	return sites
}

// stampLiteralSetsInsertTree reports whether expression, a stamp site, sets InsertTree where it is
// a RenderStamp literal; a site that is no literal - a call, a variable, a declaration - is read
// as setting it, and its own sites are on the census.
func stampLiteralSetsInsertTree(expression ast.Expr) bool {
	literal, ok := expression.(*ast.CompositeLit)
	if !ok {
		return true
	}
	for _, element := range literal.Elts {
		if pair, ok := element.(*ast.KeyValueExpr); ok {
			if key, ok := pair.Key.(*ast.Ident); ok && key.Name == "InsertTree" {
				return true
			}
		}
	}
	return false
}

// copiedRows is one CopyFrom into artifact_versions: the top-level function it is in and the
// columns it names.
type copiedRows struct {
	function string
	columns  []string
}

// copiedVersionColumns is every CopyFrom call of file whose table, its second argument, is a
// pgx.Identifier literal ending in artifact_versions, with the columns its third argument names
// when that is a []string literal of strings, and none otherwise.
func copiedVersionColumns(file *ast.File) []copiedRows {
	var copies []copiedRows
	for _, declared := range file.Decls {
		fn, ok := declared.(*ast.FuncDecl)
		if !ok {
			continue
		}
		ast.Inspect(fn, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) < 3 {
				return true
			}
			if selector, ok := call.Fun.(*ast.SelectorExpr); !ok || selector.Sel.Name != "CopyFrom" {
				return true
			}
			table := stringLiterals(call.Args[1])
			if len(table) == 0 || table[len(table)-1] != "artifact_versions" {
				return true
			}
			copies = append(copies, copiedRows{function: fn.Name.Name, columns: stringLiterals(call.Args[2])})
			return true
		})
	}
	return copies
}

// stringLiterals is the strings of a composite literal every element of which is a string literal
// (pgx.Identifier{"artifact_versions"}, []string{"id", ...}), and nil for any other expression.
func stringLiterals(expression ast.Expr) []string {
	composite, ok := expression.(*ast.CompositeLit)
	if !ok {
		return nil
	}
	values := make([]string, 0, len(composite.Elts))
	for _, element := range composite.Elts {
		literal, ok := element.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return nil
		}
		value, err := strconv.Unquote(literal.Value)
		if err != nil {
			return nil
		}
		values = append(values, value)
	}
	return values
}
