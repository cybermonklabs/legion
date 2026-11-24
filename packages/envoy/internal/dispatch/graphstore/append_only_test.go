package graphstore

import (
	"cmp"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// storeMigrations is the directory of the store's migrations, from the repository's root.
const storeMigrations = "packages/envoy/internal/dispatch/store/migrations"

// TestNoStatementDeletesANodeOrATuple holds the store to append-only (R1). Since the erase left,
// nothing in the tree takes a row out of graph_nodes or graph_tuples: a relation that ends is a
// second tuple with Asserted false, appended after the one that stated it, so every fact the
// store was told stays and a read as of any moment answers what was true then. A statement that
// deleted a row would rewrite that history.
//
// The test reads the source, never a database: every Go file under packages/ but the tests, and
// every migration of the store. It fails on each delete from, truncate or drop table that names
// graph_nodes or graph_tuples (storeRowRemoval), naming the file and the line. A statement whose
// table name is built at run time counts too, where the scan can see the table the name holds:
// the strings a Go file builds with +, with its package's constants, with a function's local
// variables and with fmt's formatting functions are each read as the statement they make
// (removalScan). Tests and the e2e drivers delete the fixture rows they wrote, so no _test.go
// file and no e2e script is read. A drop trigger … on graph_nodes drops a trigger and no row, so
// it is no such statement: 20261109051232_graph_supertags drops two before it makes them again.
func TestNoStatementDeletesANodeOrATuple(t *testing.T) {
	requireTheScanFindsWhatItShould(t)

	root := goWorkRoot(t)
	scan := newRemovalScan()
	byDirectory := map[string][]string{}
	err := filepath.WalkDir(filepath.Join(root, "packages"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if name := entry.Name(); name == "node_modules" || name == "testdata" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			byDirectory[filepath.Dir(path)] = append(byDirectory[filepath.Dir(path)], path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the Go files under %s: %v", filepath.Join(root, "packages"), err)
	}
	// A constant is read from every file of its package, so a directory is parsed whole before
	// its files are scanned, and dropped after: the tree's syntax is never held at once.
	goFiles := 0
	for _, directory := range slices.Sorted(maps.Keys(byDirectory)) {
		goFiles += scan.readDirectory(t, root, byDirectory[directory])
	}
	migrations := scan.readMigrations(t, root)

	removals := scan.removals()
	for _, removal := range removals {
		t.Errorf("%s:%d: %s: the store is append-only, so no statement takes a node or a tuple out of it; a relation that ends is a tuple with Asserted false appended after the one that stated it",
			removal.path, removal.line, removal.clause)
	}
	if goFiles < 1000 || migrations < 100 {
		t.Errorf("the scan read %d Go files and %d migrations under %s, too few to be the tree: it is reading the wrong place, and a scan of nothing proves nothing",
			goFiles, migrations, root)
	}
	t.Logf("the scan read %d Go files under packages/ and %d migrations under %s, and tested %d statements, each string a Go file holds or builds and each migration's text: %d delete a node or a tuple",
		goFiles, migrations, storeMigrations, scan.tested, len(removals))
}

// requireTheScanFindsWhatItShould runs the scan over statements and a Go source whose answers
// are known, so a scan that stopped seeing a removal fails here rather than passing the tree.
func requireTheScanFindsWhatItShould(t *testing.T) {
	t.Helper()
	for _, statement := range []struct {
		text, removes string
	}{
		{"delete from graph_nodes where kind = $1", "delete from graph_nodes"},
		{"DELETE FROM ONLY public.graph_tuples", "DELETE FROM ONLY public.graph_tuples"},
		{`truncate table only graph_events, "graph_tuples" restart identity`, `truncate table only graph_events, "graph_tuples"`},
		{"drop table if exists retired, graph_nodes cascade", "drop table if exists retired, graph_nodes"},
		{"delete /* the store */ from -- a note\n graph_tuples t using gone g", "delete from graph_tuples"},
		{"drop trigger if exists graph_nodes_block_schema_written on graph_nodes", ""},
		{"delete from graph_nodes_x", ""},
		{"do $$ begin execute 'delete from graph_nodes where kind = ''gone'''; end $$", "delete from graph_nodes"},
		{"insert into graph_tuples (kind) values ('a')", ""},
		{"update graph_nodes set state = 'gone'", ""},
		{"-- delete from graph_nodes", ""},
		{`delete from "Graph_Nodes"`, ""},
	} {
		var clauses []string
		for _, removal := range storeRemovalsIn(built{{text: statement.text, line: 1, multiline: true}}) {
			clauses = append(clauses, removal.clause)
		}
		var want []string
		if statement.removes != "" {
			want = []string{statement.removes}
		}
		if !slices.Equal(clauses, want) {
			t.Errorf("the scan reads %q as the removals %q, want %q", statement.text, clauses, want)
		}
	}

	scan := newRemovalScan()
	syntax, err := parser.ParseFile(scan.files, "sample.go", removalSample, 0)
	if err != nil {
		t.Fatalf("parse the sample: %v", err)
	}
	scan.scanFile(newScannedFile("sample.go", syntax, &scannedPackage{constants: map[string]constantRef{}}))
	want := []storeRemoval{
		{"sample.go", 7, "delete from graph_tuples"},
		{"sample.go", 11, "truncate graph_nodes"},
		{"sample.go", 12, "delete from graph_tuples"},
		{"sample.go", 15, "drop table graph_nodes"},
	}
	if found := scan.removals(); !slices.Equal(found, want) {
		t.Errorf("the scan finds the removals %v in the sample, want %v:\n%s", found, want, removalSample)
	}
}

// removalSample is a Go source whose removals are known: a constant's, a local variable's, a
// formatted statement's and a ranged name's, each built from a table name the scan can see. A
// parameter it cannot see, a drop trigger and an update remove nothing, and a constant used
// again is found where its statement is written, not a second time where it is used.
const removalSample = `package sample

import "fmt"

const tuples = "graph_tuples"

const removal = "delete from " + tuples

func run(kind string) {
	table := "graph_nodes"
	_ = "truncate " + table
	_ = fmt.Sprintf("delete from %s where kind = $1", tuples)
	_ = "delete from " + kind
	for _, name := range []string{"comments", "graph_nodes"} {
		_ = "drop table " + name
	}
	_ = "drop trigger if exists graph_nodes_written on graph_nodes"
	_ = "update graph_nodes set state = 'gone'"
	_ = removal + " where seq = 0"
}
`

// removalName is one name a statement writes: a SQL identifier (sqlIdentifier), or the
// placeholder the scan writes for a Go name whose value it cannot see.
const removalName = `(?:` + sqlIdentifier + `|«[^«»]*»)`

// removalTable is a table as a statement names it, under its schema and its database where it
// names them, with a space or none round each dot.
const removalTable = removalName + `(?:\s*\.\s*` + removalName + `){0,2}`

// storeRowRemoval matches one statement that takes rows out of the store, in any case and over
// any whitespace: a delete from graph_nodes or graph_tuples, and a truncate or a drop table whose
// list of tables holds one of them. A table is named bare, quoted or under its schema; a quoted
// name is matched in its own case, as Postgres reads it. The name must end where the match does,
// so graph_nodes_x, and graph_nodes as the schema of another table, are not it. Group 1 is the
// clause, from its verb to the store's table. A drop trigger on graph_nodes, an insert into
// graph_tuples and an update of graph_nodes take no row out and are not matched.
var storeRowRemoval = regexp.MustCompile(`(?i)\b(` +
	`(?:delete\s+from\s+(?:only\s+)?` +
	`|truncate\s+(?:table\s+)?(?:(?:only\s+)?` + removalTable + `(?:\s*\*)?\s*,\s*)*(?:only\s+)?` +
	`|drop\s+table\s+(?:if\s+exists\s+)?(?:` + removalTable + `\s*,\s*)*)` +
	`(?:` + removalName + `\s*\.\s*){0,2}(?:graph_(?:nodes|tuples)|(?-i:"graph_(?:nodes|tuples)"))` +
	`)(?:\s*(?:[^a-z0-9_$.\s]|\z)|\s+[^.\s])`)

// withoutSQLComments is text with each -- and /* */ comment blanked out, a block comment nested
// in another included, and no comment found inside a quoted string or name. A line break stays
// and every other byte of a comment becomes a space, so an offset into the answer is the same
// offset into text, and a comment between two words still parts them, as it does for Postgres.
func withoutSQLComments(text string) string {
	out := []byte(text)
	var quote byte
	for i := 0; i < len(out); i++ {
		switch c := out[i]; {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '-' && i+1 < len(out) && out[i+1] == '-':
			for ; i < len(out) && out[i] != '\n'; i++ {
				out[i] = ' '
			}
		case c == '/' && i+1 < len(out) && out[i+1] == '*':
			for depth := 0; i < len(out); i++ {
				switch {
				case out[i] == '/' && i+1 < len(out) && out[i+1] == '*':
					depth++
					out[i], out[i+1] = ' ', ' '
					i++
				case out[i] == '*' && i+1 < len(out) && out[i+1] == '/':
					depth--
					out[i], out[i+1] = ' ', ' '
					i++
				case out[i] != '\n':
					out[i] = ' '
				}
				if depth == 0 {
					break
				}
			}
		}
	}
	return string(out)
}

// storeRemoval is one statement found that takes rows out of the store: the file, from the
// repository's root, the line its verb is on, and the clause as the scan read it.
type storeRemoval struct {
	path   string
	line   int
	clause string
}

// segment is one piece of a string the scan read, with where the source holds it.
type segment struct {
	text string
	line int
	// multiline is whether text's line breaks are the source's own, as a raw string's are, so
	// a piece after one is on a later line than line.
	multiline bool
	// borrowed is whether text is what a constant, a variable or a formatted argument holds,
	// which the scan reads on its own where that is written: a statement wholly inside it is
	// found there, and is not found a second time where it is used.
	borrowed bool
}

// built is one string the scan read, as the pieces it is built from.
type built []segment

// text is the string itself.
func (b built) text() string {
	var text strings.Builder
	for _, piece := range b {
		text.WriteString(piece.text)
	}
	return text.String()
}

// slice is the part of piece from byte from to byte to, on the line that part starts on.
func (piece segment) slice(from, to int) segment {
	part := piece
	part.text = piece.text[from:to]
	if piece.multiline {
		part.line += strings.Count(piece.text[:from], "\n")
	}
	return part
}

// storeRemovalsIn is every statement of value that takes rows out of the store, once its SQL
// comments are gone, each on the line of the piece its verb is in. A statement wholly inside a
// borrowed piece is left to the scan of the place that piece is written.
func storeRemovalsIn(value built) []storeRemoval {
	starts := make([]int, len(value))
	length := 0
	for i, piece := range value {
		starts[i] = length
		length += len(piece.text)
	}
	statement := withoutSQLComments(value.text())
	var removals []storeRemoval
	for _, match := range storeRowRemoval.FindAllStringSubmatchIndex(statement, -1) {
		start, end := match[2], match[3]
		// The pieces lie in order, so the first that ends past start holds it.
		at := 0
		for start >= starts[at]+len(value[at].text) {
			at++
		}
		piece := value[at]
		if piece.borrowed && end <= starts[at]+len(piece.text) {
			continue
		}
		removals = append(removals, storeRemoval{
			line:   piece.slice(start-starts[at], start-starts[at]).line,
			clause: strings.Join(strings.Fields(statement[start:end]), " "),
		})
	}
	return removals
}

// valueLimit is how many strings the scan follows for one expression. A name that may hold more,
// and a + whose two sides would make more, are read as a placeholder past it, as a name the scan
// cannot see is, so the scan's work never grows as the product of every name in a statement.
const valueLimit = 256

// removalScan reads Go files and migrations for statements that take rows out of the store.
type removalScan struct {
	files *token.FileSet
	// constants and locals are what each constant and each local variable may hold, read once;
	// a value still being read is one that names itself, which the scan cannot see.
	constants map[constantRef]*readValue
	locals    map[*ast.Object]*readValue
	found     map[storeRemoval]bool
	tested    int
}

// readValue is what one name may hold, and whether the scan is still reading it.
type readValue struct {
	values  []built
	reading bool
}

func newRemovalScan() *removalScan {
	return &removalScan{
		files:     token.NewFileSet(),
		constants: map[constantRef]*readValue{},
		locals:    map[*ast.Object]*readValue{},
		found:     map[storeRemoval]bool{},
	}
}

// removals is every statement the scan found, in the order of their files and lines.
func (s *removalScan) removals() []storeRemoval {
	return slices.SortedFunc(maps.Keys(s.found), func(a, b storeRemoval) int {
		return cmp.Or(strings.Compare(a.path, b.path), cmp.Compare(a.line, b.line), strings.Compare(a.clause, b.clause))
	})
}

// scannedPackage is one Go package of the tree, as the scan reads its names.
type scannedPackage struct {
	// constants is each constant the package declares at its own level, by name, since a file
	// names one another file declares as a bare name the parser leaves unresolved.
	constants map[string]constantRef
}

// constantRef is one constant: the declaration it is in and its place there.
type constantRef struct {
	file  *scannedFile
	spec  *ast.ValueSpec
	index int
}

// scannedFile is one parsed Go file and what the scan reads of its names.
type scannedFile struct {
	path   string
	syntax *ast.File
	pkg    *scannedPackage
	// fmtNames are the names the file imports fmt under.
	fmtNames map[string]bool
	// constValues is the value list of each constant declaration, the one before it repeated
	// for a declaration that states none, as Go repeats it.
	constValues map[*ast.ValueSpec][]ast.Expr
	// packageVars are the variables the file declares at the package's level, which any file of
	// the package may assign, so the scan does not see what they hold.
	packageVars map[*ast.Object]bool
	// assigned is what each local variable of the file is given, read the first time it is asked.
	assigned map[*ast.Object]*localSources
}

// localSources is what one local variable is given: each expression it is assigned or declared
// with, whether its zero string is one of its values, and whether it is given anything the scan
// cannot read as an expression, such as a parameter's argument or one result of a call.
type localSources struct {
	values []ast.Expr
	zero   bool
	unseen bool
}

func newScannedFile(path string, syntax *ast.File, pkg *scannedPackage) *scannedFile {
	file := &scannedFile{
		path: path, syntax: syntax, pkg: pkg, fmtNames: map[string]bool{},
		constValues: map[*ast.ValueSpec][]ast.Expr{}, packageVars: map[*ast.Object]bool{},
	}
	for _, imported := range syntax.Imports {
		if imported.Path.Value == `"fmt"` {
			name := "fmt"
			if imported.Name != nil {
				name = imported.Name.Name
			}
			file.fmtNames[name] = true
		}
	}
	ast.Inspect(syntax, func(node ast.Node) bool {
		if declaration, ok := node.(*ast.GenDecl); ok && declaration.Tok == token.CONST {
			var values []ast.Expr
			for _, spec := range declaration.Specs {
				value := spec.(*ast.ValueSpec)
				if len(value.Values) > 0 {
					values = value.Values
				}
				file.constValues[value] = values
			}
		}
		return true
	})
	for _, declaration := range syntax.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range general.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for index, name := range value.Names {
				switch {
				case general.Tok == token.CONST:
					pkg.constants[name.Name] = constantRef{file: file, spec: value, index: index}
				case name.Obj != nil:
					file.packageVars[name.Obj] = true
				}
			}
		}
	}
	return file
}

// readDirectory parses the Go files of one directory, the package or packages it holds, and
// scans each file, answering how many it read. A file that does not parse fails the test, as a
// build of its package would, and is not scanned.
func (s *removalScan) readDirectory(t *testing.T, root string, paths []string) int {
	t.Helper()
	packages := map[string]*scannedPackage{}
	var files []*scannedFile
	for _, path := range paths {
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		relative := repositoryPath(t, root, path)
		syntax, err := parser.ParseFile(s.files, relative, source, 0)
		if err != nil {
			t.Errorf("%s does not parse, so no build of its package does either: %v", relative, err)
			continue
		}
		pkg := packages[syntax.Name.Name]
		if pkg == nil {
			pkg = &scannedPackage{constants: map[string]constantRef{}}
			packages[syntax.Name.Name] = pkg
		}
		files = append(files, newScannedFile(relative, syntax, pkg))
	}
	for _, file := range files {
		s.scanFile(file)
	}
	return len(files)
}

// readMigrations scans the text of every migration of the store, answering how many it read.
func (s *removalScan) readMigrations(t *testing.T, root string) int {
	t.Helper()
	read := 0
	directory := filepath.Join(root, filepath.FromSlash(storeMigrations))
	err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".sql") {
			return err
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		read++
		s.tested++
		relative := repositoryPath(t, root, path)
		for _, removal := range storeRemovalsIn(built{{text: string(source), line: 1, multiline: true}}) {
			removal.path = relative
			s.found[removal] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read the migrations under %s: %v", directory, err)
	}
	return read
}

// scanFile tests every string the file holds or builds: each string literal, each + and each
// call of a formatting function of fmt. A part read into a larger string is not read again on
// its own, so a statement is found once, in the whole it is part of.
func (s *removalScan) scanFile(file *scannedFile) {
	consumed := map[ast.Node]bool{}
	ast.Inspect(file.syntax, func(node ast.Node) bool {
		expression, ok := node.(ast.Expr)
		if !ok || consumed[node] || !file.buildsAString(expression) {
			return true
		}
		reading := evaluation{scan: s, file: file, consumed: consumed}
		for _, value := range reading.value(expression) {
			s.tested++
			for _, removal := range storeRemovalsIn(value) {
				removal.path = file.path
				s.found[removal] = true
			}
		}
		return true
	})
}

// buildsAString is whether expression is one the scan reads a string from as a whole.
func (f *scannedFile) buildsAString(expression ast.Expr) bool {
	switch x := expression.(type) {
	case *ast.BasicLit:
		return x.Kind == token.STRING
	case *ast.BinaryExpr:
		return x.Op == token.ADD
	case *ast.CallExpr:
		_, formats := f.formatAt(x)
		return formats
	}
	return false
}

// printfFormats is the argument each formatting function of fmt takes its format at.
var printfFormats = map[string]int{"Sprintf": 0, "Errorf": 0, "Printf": 0, "Fprintf": 1, "Appendf": 1}

// formatAt is the argument call takes its format at, when call is one of printfFormats.
func (f *scannedFile) formatAt(call *ast.CallExpr) (int, bool) {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return 0, false
	}
	// An imported package's name is one the parser leaves unresolved; a local variable named
	// fmt is resolved to its declaration, and its methods are not fmt's.
	if pkg, ok := selector.X.(*ast.Ident); !ok || pkg.Obj != nil || !f.fmtNames[pkg.Name] {
		return 0, false
	}
	at, ok := printfFormats[selector.Sel.Name]
	return at, ok && at < len(call.Args)
}

// sources is what each local variable of the file is given, read from the whole file once:
// each assignment and declaration of it, each value a range over a composite literal gives it,
// and whether it is ever given anything else, or has its address taken and may be written
// through it.
func (f *scannedFile) sources() map[*ast.Object]*localSources {
	if f.assigned != nil {
		return f.assigned
	}
	f.assigned = map[*ast.Object]*localSources{}
	of := func(name ast.Expr) *localSources {
		ident, ok := name.(*ast.Ident)
		if !ok || ident.Obj == nil || ident.Obj.Kind != ast.Var {
			return nil
		}
		held := f.assigned[ident.Obj]
		if held == nil {
			held = &localSources{}
			f.assigned[ident.Obj] = held
		}
		return held
	}
	ast.Inspect(f.syntax, func(node ast.Node) bool {
		switch x := node.(type) {
		case *ast.AssignStmt:
			for i, target := range x.Lhs {
				if held := of(target); held != nil {
					if (x.Tok == token.DEFINE || x.Tok == token.ASSIGN) && len(x.Rhs) == len(x.Lhs) {
						held.values = append(held.values, x.Rhs[i])
					} else {
						held.unseen = true
					}
				}
			}
		case *ast.RangeStmt:
			if held := of(x.Key); held != nil {
				held.unseen = true
			}
			if held := of(x.Value); held != nil {
				literal, ok := ast.Unparen(x.X).(*ast.CompositeLit)
				if !ok {
					held.unseen = true
					break
				}
				for _, element := range literal.Elts {
					if pair, ok := element.(*ast.KeyValueExpr); ok {
						element = pair.Value
					}
					held.values = append(held.values, element)
				}
			}
		case *ast.ValueSpec:
			for i, name := range x.Names {
				if held := of(name); held != nil {
					switch typeName, _ := x.Type.(*ast.Ident); {
					case len(x.Values) == len(x.Names):
						held.values = append(held.values, x.Values[i])
					case len(x.Values) == 0 && typeName != nil && typeName.Name == "string":
						held.zero = true
					default:
						held.unseen = true
					}
				}
			}
		case *ast.UnaryExpr:
			if held := of(x.X); x.Op == token.AND && held != nil {
				held.unseen = true
			}
		}
		return true
	})
	return f.assigned
}

// constant is every string the constant at spec's index may hold, read once.
func (s *removalScan) constant(ref constantRef) []built {
	if held := s.constants[ref]; held != nil {
		if held.reading {
			return placeholder(ref.spec.Names[ref.index].Name)
		}
		return held.values
	}
	held := &readValue{reading: true}
	s.constants[ref] = held
	values := ref.file.constValues[ref.spec]
	held.values = placeholder(ref.spec.Names[ref.index].Name)
	if ref.index < len(values) {
		held.values = evaluation{scan: s, file: ref.file, constant: true}.value(values[ref.index])
	}
	held.reading = false
	return held.values
}

// local is every string the local variable object of file may hold: what each expression it is
// given may hold, its zero string where it is declared without one, and a placeholder where it
// is also given something the scan cannot see, read once.
func (s *removalScan) local(file *scannedFile, object *ast.Object) []built {
	if held := s.locals[object]; held != nil {
		if held.reading {
			return placeholder(object.Name)
		}
		return held.values
	}
	held := &readValue{reading: true}
	s.locals[object] = held
	sources := file.sources()[object]
	_, parameter := object.Decl.(*ast.Field)
	var values []built
	unseen := sources == nil || sources.unseen || parameter
	if sources != nil {
		if sources.zero {
			values = append(values, built{})
		}
		reading := evaluation{scan: s, file: file}
		for _, source := range sources.values {
			values = append(values, reading.value(source)...)
		}
	}
	if unseen || len(values) == 0 {
		values = append(values, placeholder(object.Name)...)
	}
	if values = distinct(values); len(values) > valueLimit {
		values = placeholder(object.Name)
	}
	held.values, held.reading = values, false
	return values
}

// placeholder is the string the scan reads a name it cannot see as: one that stands for any
// table, and is neither graph_nodes nor graph_tuples.
func placeholder(name string) []built {
	return []built{{{text: "«" + name + "»"}}}
}

// distinct is values without a string read twice from the same pieces.
func distinct(values []built) []built {
	seen := map[string]bool{}
	var answer []built
	for _, value := range values {
		var key strings.Builder
		for _, piece := range value {
			fmt.Fprintf(&key, "%d %t %t %q;", piece.line, piece.multiline, piece.borrowed, piece.text)
		}
		if !seen[key.String()] {
			seen[key.String()] = true
			answer = append(answer, value)
		}
	}
	return answer
}

// joined is every string a + b may be: each of a's followed by each of b's, with their pieces.
// Past valueLimit of them, b is read as fallback after each of a's, and each of b's is read
// alone besides, so a statement written wholly inside b is still found.
func joined(a, b, fallback []built) []built {
	var alone []built
	if len(b) > 1 && len(a)*len(b) > valueLimit {
		b, alone = fallback, b
	}
	answer := make([]built, 0, len(a)*len(b)+len(alone))
	for _, left := range a {
		for _, right := range b {
			answer = append(answer, append(slices.Clip(left), right...))
		}
	}
	return append(answer, alone...)
}

// evaluation reads the strings an expression of one file may hold, without type-checking it.
type evaluation struct {
	scan *removalScan
	file *scannedFile
	// consumed is where an expression read in place marks each part it reads, so the scan does
	// not read that part again on its own; nil for one read where it is borrowed.
	consumed map[ast.Node]bool
	// constant is whether the expression is a constant's value, where a call of one argument
	// is a conversion.
	constant bool
}

// value is every string expression may hold. A string literal holds its own; a + holds each of
// its left's followed by each of its right's; a name holds what its constant or its local
// variable may (named); a call of a formatting function of fmt holds its format with its
// arguments in it (formatted). Anything else is a placeholder.
func (e evaluation) value(expression ast.Expr) []built {
	switch x := expression.(type) {
	case *ast.BasicLit:
		e.consume(x)
		text, err := strconv.Unquote(x.Value)
		if x.Kind != token.STRING || err != nil {
			return e.unseen(x)
		}
		return []built{{{text: text, line: e.line(x), multiline: strings.HasPrefix(x.Value, "`")}}}
	case *ast.ParenExpr:
		return e.value(x.X)
	case *ast.BinaryExpr:
		if x.Op != token.ADD {
			return e.unseen(x)
		}
		e.consume(x)
		return joined(e.value(x.X), e.value(x.Y), e.unseen(x.Y))
	case *ast.CallExpr:
		if at, ok := e.file.formatAt(x); ok {
			e.consume(x)
			return e.formatted(x, at)
		}
		if len(x.Args) == 1 && !x.Ellipsis.IsValid() && e.conversion(x.Fun) {
			return e.value(x.Args[0])
		}
	case *ast.Ident:
		return e.named(x)
	}
	return e.unseen(expression)
}

// conversion is whether a call of fun with one argument is a conversion, which holds what its
// argument does: string(...) anywhere, and any call but a builtin's in a constant's value, where
// no other function may be called.
func (e evaluation) conversion(fun ast.Expr) bool {
	switch x := ast.Unparen(fun).(type) {
	case *ast.Ident:
		if x.Obj == nil && x.Name == "string" {
			return true
		}
		return e.constant && x.Name != "len" && x.Name != "cap" && x.Name != "real" && x.Name != "imag"
	case *ast.SelectorExpr:
		return e.constant
	}
	return false
}

// named is every string the name may hold: the value of the constant it names, of the package
// or of its function, or what the local variable it names may hold. A variable of the package's
// level may be assigned in any of its files, and anything else is no string, so either is a
// placeholder.
func (e evaluation) named(name *ast.Ident) []built {
	object := name.Obj
	switch {
	case object == nil:
		if ref, ok := e.file.pkg.constants[name.Name]; ok {
			return e.borrowed(name, e.scan.constant(ref))
		}
	case object.Kind == ast.Con:
		if spec, ok := object.Decl.(*ast.ValueSpec); ok {
			index := slices.IndexFunc(spec.Names, func(declared *ast.Ident) bool { return declared.Name == name.Name })
			if index >= 0 {
				return e.borrowed(name, e.scan.constant(constantRef{file: e.file, spec: spec, index: index}))
			}
		}
	case object.Kind == ast.Var && !e.file.packageVars[object]:
		return e.borrowed(name, e.scan.local(e.file, object))
	}
	return e.unseen(name)
}

// formatted is every string a call of a formatting function of fmt may make: its format, with
// each %s and %v replaced by what the argument it takes may hold, each %q by that quoted, and
// each other verb by a placeholder, since a number or an error names no table. An argument is
// borrowed: the scan reads it where it is written, as the string it holds alone.
func (e evaluation) formatted(call *ast.CallExpr, at int) []built {
	arguments := call.Args[at+1:]
	if call.Ellipsis.IsValid() {
		arguments = nil
	}
	argument := func(index int) []built {
		if index < 0 || index >= len(arguments) {
			return e.unseen(call)
		}
		return e.borrowed(arguments[index], evaluation{scan: e.scan, file: e.file}.value(arguments[index]))
	}
	var answer []built
	for _, format := range e.value(call.Args[at]) {
		made := []built{nil}
		next := 0
		for _, piece := range format {
			from := 0
			for i := 0; i < len(piece.text); i++ {
				if piece.text[i] != '%' {
					continue
				}
				verb, end, stars, explicit := printfVerb(piece.text, i)
				made = joined(made, []built{{piece.slice(from, i)}}, nil)
				next += stars
				if explicit >= 0 {
					next = explicit
				}
				var substituted []built
				switch verb {
				case '%':
					substituted = []built{{piece.slice(i+1, end)}}
				case 's', 'v':
					substituted = argument(next)
					next++
				case 'q':
					for _, value := range argument(next) {
						substituted = append(substituted, built{{text: strconv.Quote(value.text()), line: e.line(call), borrowed: true}})
					}
					next++
				default:
					substituted = e.unseen(call)
					next++
				}
				made = joined(made, substituted, e.unseen(call))
				from, i = end, end-1
			}
			made = joined(made, []built{{piece.slice(from, len(piece.text))}}, nil)
		}
		answer = append(answer, made...)
	}
	return distinct(answer)
}

// printfVerb reads the directive of a format that starts at text[at], a '%': the verb that ends
// it, where the text after it starts, how many arguments a * of its width and precision take,
// and the argument an explicit [n] makes it take, -1 where it names none.
func printfVerb(text string, at int) (verb rune, end, stars, explicit int) {
	explicit = -1
	i := at + 1
	index := func() {
		if i < len(text) && text[i] == '[' {
			if closing := strings.IndexByte(text[i:], ']'); closing > 0 {
				if n, err := strconv.Atoi(text[i+1 : i+closing]); err == nil {
					explicit = n - 1
				}
				i += closing + 1
			}
		}
	}
	number := func() {
		if i < len(text) && text[i] == '*' {
			stars++
			i++
			return
		}
		for i < len(text) && '0' <= text[i] && text[i] <= '9' {
			i++
		}
	}
	for i < len(text) && strings.IndexByte("+-# 0", text[i]) >= 0 {
		i++
	}
	index()
	number()
	if i < len(text) && text[i] == '.' {
		i++
		index()
		number()
	}
	index()
	if i >= len(text) {
		return 0, i, stars, explicit
	}
	verb, size := utf8.DecodeRuneInString(text[i:])
	return verb, i + size, stars, explicit
}

// borrowed is values, each read as one borrowed piece on the line of at, where it is used.
func (e evaluation) borrowed(at ast.Expr, values []built) []built {
	answer := make([]built, 0, len(values))
	for _, value := range values {
		answer = append(answer, built{{text: value.text(), line: e.line(at), borrowed: true}})
	}
	return distinct(answer)
}

// unseen is the placeholder for expression, named by it where it is a name or a selector of
// names, on its line.
func (e evaluation) unseen(expression ast.Expr) []built {
	answer := placeholder(unseenName(expression))
	answer[0][0].line = e.line(expression)
	return answer
}

// unseenName is the name a placeholder carries: the expression's own where it is a name or a
// selector of names, which is what a failure reads best, and ? for any other.
func unseenName(expression ast.Expr) string {
	switch x := expression.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		if base := unseenName(x.X); base != "?" {
			return base + "." + x.Sel.Name
		}
	case *ast.ParenExpr:
		return unseenName(x.X)
	}
	return "?"
}

// consume marks node as read into the expression being read in place.
func (e evaluation) consume(node ast.Node) {
	if e.consumed != nil {
		e.consumed[node] = true
	}
}

func (e evaluation) line(node ast.Node) int {
	return e.scan.files.Position(node.Pos()).Line
}

// goWorkRoot is the repository's root: the directory holding go.work, found by climbing out of
// this package's directory, where go test runs the test.
func goWorkRoot(t *testing.T) string {
	t.Helper()
	here, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("read the test's own directory: %v", err)
	}
	for directory := here; ; {
		if _, err := os.Stat(filepath.Join(directory, "go.work")); err == nil {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			t.Fatalf("no directory above %s holds go.work", here)
		}
		directory = parent
	}
}

// repositoryPath is path from the repository's root, with forward slashes, as a failure names it.
func repositoryPath(t *testing.T, root, path string) string {
	t.Helper()
	relative, err := filepath.Rel(root, path)
	if err != nil {
		t.Fatalf("name %s under %s: %v", path, root, err)
	}
	return filepath.ToSlash(relative)
}
