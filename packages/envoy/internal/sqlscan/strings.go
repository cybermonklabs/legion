package sqlscan

import (
	"cmp"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Piece is one piece of a string the scan read, with where the source holds it.
type Piece struct {
	Text string
	Line int
	// Multiline is whether Text's line breaks are the source's own, as a raw string's are, so a
	// piece after one is on a later line than Line.
	Multiline bool
	// Borrowed is whether Text is what a constant, a variable or a formatted argument holds,
	// which the scan reads on its own where that is written: a statement wholly inside it is
	// found there, and is not found a second time where it is used.
	Borrowed bool
}

// Value is one string the scan read, as the pieces it is built from.
type Value []Piece

// Text is the string itself.
func (v Value) Text() string {
	var text strings.Builder
	for _, piece := range v {
		text.WriteString(piece.Text)
	}
	return text.String()
}

// Slice is the part of piece from byte from to byte to, on the line that part starts on.
func (piece Piece) Slice(from, to int) Piece {
	part := piece
	part.Text = piece.Text[from:to]
	if piece.Multiline {
		part.Line += strings.Count(piece.Text[:from], "\n")
	}
	return part
}

// PlaceholderPattern is a regular expression matching the placeholder the scan writes for a name
// whose value it cannot see (placeholder), so a rule over the scan's strings can read it as a name
// that stands for anything.
const PlaceholderPattern = `«[^«»]*»`

// ReadGo calls visit with every string the Go files under root's directory under hold or build -
// each string literal, each + and each call of a formatting function of fmt - each read as the
// string it makes (evaluation.value), with the file holding it from root, and answers how many
// files it read. A name in such a string holds what its package's constant, an exported constant
// of another package of the tree the file imports, or a function's local variable may hold. A
// part read into a larger string is not read again on its own, so a statement is visited once, in
// the whole it is part of. A file that does not parse fails the test, as a build of its package
// would, and is not read.
//
// The constants of every package are read first, and of each file only its constant declarations
// are kept; then each directory is parsed whole, its files read, and dropped: the tree's syntax is
// never held at once.
func ReadGo(t testing.TB, root, under string, visit func(file string, value Value)) int {
	t.Helper()
	paths, err := GoFiles(filepath.Join(root, under))
	if err != nil {
		t.Fatalf("walk the Go files under %s: %v", filepath.Join(root, under), err)
	}
	byDirectory := map[string][]goFile{}
	for _, path := range paths {
		directory := filepath.Dir(path)
		byDirectory[directory] = append(byDirectory[directory], goFile{name: Path(t, root, path), path: path})
	}
	modules := map[string]string{}
	var packages []goPackage
	for _, directory := range slices.Sorted(maps.Keys(byDirectory)) {
		importPath, _ := moduleImportPath(modules, root, directory)
		packages = append(packages, goPackage{importPath: importPath, files: byDirectory[directory]})
	}
	read, err := newScan().read(packages, visit)
	if err != nil {
		var failures parseFailures
		if !errors.As(err, &failures) {
			t.Fatal(err)
		}
		for _, failure := range failures {
			t.Errorf("%s does not parse, so no build of its package does either: %v", failure.file, failure.err)
		}
	}
	return read
}

// Source is one Go file ReadSources reads from memory: the import path of its package, the name a
// failure gives it, and its text.
type Source struct {
	ImportPath, Name, Text string
}

// ReadSources reads sources as ReadGo reads the tree, the files of one import path being one
// package, and calls visit with each string they hold or build. A file that does not parse is an
// error, and the files that do are read.
func ReadSources(sources []Source, visit func(file string, value Value)) error {
	var packages []goPackage
	for _, source := range sources {
		at := slices.IndexFunc(packages, func(p goPackage) bool { return p.importPath == source.ImportPath })
		if at < 0 {
			at = len(packages)
			packages = append(packages, goPackage{importPath: source.ImportPath})
		}
		packages[at].files = append(packages[at].files, goFile{name: source.Name, text: []byte(source.Text)})
	}
	_, err := newScan().read(packages, visit)
	return err
}

// ReadSQL calls visit with the text of every .sql file under root's directory under, as one
// string on its file's first line, with the file from root, and answers how many it read.
func ReadSQL(t testing.TB, root, under string, visit func(file string, value Value)) int {
	t.Helper()
	paths, err := SQLFiles(filepath.Join(root, under))
	if err != nil {
		t.Fatalf("walk the SQL files under %s: %v", filepath.Join(root, under), err)
	}
	for _, path := range paths {
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		visit(Path(t, root, path), Value{{Text: string(source), Line: 1, Multiline: true}})
	}
	return len(paths)
}

// goPackage is the Go files of one directory, with the import path a file of another package
// names it by, empty where it is in no module.
type goPackage struct {
	importPath string
	files      []goFile
}

// goFile is one Go file: the name a failure gives it, and its text, or the path it is read from.
type goFile struct {
	name, path string
	text       []byte
}

// source is the file's text.
func (f goFile) source() ([]byte, error) {
	if f.text != nil {
		return f.text, nil
	}
	return os.ReadFile(f.path)
}

// parseFailure is one file that does not parse, and why; parseFailures is every such file of a
// read.
type parseFailure struct {
	file string
	err  error
}

type parseFailures []parseFailure

func (failures parseFailures) Error() string {
	messages := make([]string, len(failures))
	for index, failure := range failures {
		messages[index] = fmt.Sprintf("%s does not parse: %v", failure.file, failure.err)
	}
	return strings.Join(messages, "\n")
}

// moduleImportPath is the import path of directory, under root: its module's path, read from the
// go.mod of the nearest directory holding one, joined with directory's path below that one.
// modules keeps the module path of each directory looked at, empty for one holding no go.mod. It
// answers false for a directory in no module under root.
func moduleImportPath(modules map[string]string, root, directory string) (string, bool) {
	var below []string
	for current := directory; ; {
		module, known := modules[current]
		if !known {
			module = modulePath(filepath.Join(current, "go.mod"))
			modules[current] = module
		}
		if module != "" {
			slices.Reverse(below)
			return strings.Join(append([]string{module}, below...), "/"), true
		}
		parent := filepath.Dir(current)
		if current == root || parent == current {
			return "", false
		}
		below = append(below, filepath.Base(current))
		current = parent
	}
}

// modulePath is the module path the go.mod at path declares, empty where there is none.
func modulePath(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for line := range strings.Lines(string(data)) {
		if fields := strings.Fields(line); len(fields) == 2 && fields[0] == "module" {
			if unquoted, err := strconv.Unquote(fields[1]); err == nil {
				return unquoted
			}
			return fields[1]
		}
	}
	return ""
}

// valueLimit is how many strings the scan follows for one expression. A name that may hold more,
// and a + whose two sides would make more, are read as a placeholder past it, as a name the scan
// cannot see is, so the scan's work never grows as the product of every name in a statement.
const valueLimit = 256

// scan reads Go files for the strings they hold or build.
type scan struct {
	files *token.FileSet
	// constants and locals are what each constant and each local variable may hold, read once;
	// a value still being read is one that names itself, which the scan cannot see.
	constants map[constantRef]*readValue
	locals    map[*ast.Object]*readValue
	// imported is the constants every package declares at its own level, by its import path and
	// then by its name, which a file of another package qualifies them with (evaluation.imported).
	imported map[string]map[string]*scannedPackage
}

// readValue is what one name may hold, and whether the scan is still reading it.
type readValue struct {
	values  []Value
	reading bool
}

func newScan() *scan {
	return &scan{
		files:     token.NewFileSet(),
		constants: map[constantRef]*readValue{},
		locals:    map[*ast.Object]*readValue{},
		imported:  map[string]map[string]*scannedPackage{},
	}
}

// read reads packages: the constants of each first (readConstants), then each package parsed
// whole, its files read for their strings, each given to visit, and dropped. It answers how many
// files it read, and the files that do not parse as parseFailures.
func (s *scan) read(packages []goPackage, visit func(file string, value Value)) (int, error) {
	if err := s.readConstants(packages); err != nil {
		return 0, err
	}
	var failures parseFailures
	read := 0
	for _, pkg := range packages {
		byName := map[string]*scannedPackage{}
		var files []*scannedFile
		for _, file := range pkg.files {
			source, err := file.source()
			if err != nil {
				return read, fmt.Errorf("read %s: %w", file.name, err)
			}
			syntax, err := parser.ParseFile(s.files, file.name, source, 0)
			if err != nil {
				failures = append(failures, parseFailure{file: file.name, err: err})
				continue
			}
			scanned := byName[syntax.Name.Name]
			if scanned == nil {
				scanned = &scannedPackage{constants: map[string]constantRef{}}
				byName[syntax.Name.Name] = scanned
			}
			files = append(files, newScannedFile(file.name, syntax, scanned))
		}
		for _, file := range files {
			s.scanFile(file, visit)
		}
		read += len(files)
	}
	if len(failures) > 0 {
		return read, failures
	}
	return read, nil
}

// readConstants records the constants every package of packages declares at its own level, by its
// import path, so a file of another package that imports it reads what a constant of it it names
// holds. Of each file only its constant declarations and its imports are kept. A file that does
// not parse is left for read to report.
func (s *scan) readConstants(packages []goPackage) error {
	for _, pkg := range packages {
		if pkg.importPath == "" {
			continue
		}
		byName := map[string]*scannedPackage{}
		for _, file := range pkg.files {
			source, err := file.source()
			if err != nil {
				return fmt.Errorf("read %s: %w", file.name, err)
			}
			syntax, err := parser.ParseFile(s.files, file.name, source, 0)
			if err != nil {
				continue
			}
			var constants []ast.Decl
			for _, declaration := range syntax.Decls {
				if general, ok := declaration.(*ast.GenDecl); ok && general.Tok == token.CONST {
					constants = append(constants, general)
				}
			}
			scanned := byName[syntax.Name.Name]
			if scanned == nil {
				scanned = &scannedPackage{constants: map[string]constantRef{}}
				byName[syntax.Name.Name] = scanned
			}
			newScannedFile(file.name, &ast.File{Name: syntax.Name, Imports: syntax.Imports, Decls: constants}, scanned)
		}
		s.imported[pkg.importPath] = byName
	}
	return nil
}

// scannedPackage is one Go package, as the scan reads its names.
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
	// imports are the packages the file imports, each with the name it gives it, empty where it
	// gives none and names the package by its own.
	imports []importedPackage
	// constValues is the value list of each constant declaration, the one before it repeated
	// for a declaration that states none, as Go repeats it.
	constValues map[*ast.ValueSpec][]ast.Expr
	// packageVars are the variables the file declares at the package's level, which any file of
	// the package may assign, so the scan does not see what they hold.
	packageVars map[*ast.Object]bool
	// assigned is what each local variable of the file is given, read the first time it is asked.
	assigned map[*ast.Object]*localSources
}

// importedPackage is one import of a file: the name the file gives the package, empty where it
// names it by its own, and its path.
type importedPackage struct {
	name, path string
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
		path, err := strconv.Unquote(imported.Path.Value)
		if err != nil {
			continue
		}
		name := ""
		if imported.Name != nil {
			name = imported.Name.Name
		}
		file.imports = append(file.imports, importedPackage{name: name, path: path})
		if path == "fmt" {
			file.fmtNames[cmp.Or(name, "fmt")] = true
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

// scanFile calls visit with every string the file holds or builds: each string literal, each +
// and each call of a formatting function of fmt. A part read into a larger string is not read
// again on its own, so a statement is found once, in the whole it is part of.
func (s *scan) scanFile(file *scannedFile, visit func(file string, value Value)) {
	consumed := map[ast.Node]bool{}
	ast.Inspect(file.syntax, func(node ast.Node) bool {
		expression, ok := node.(ast.Expr)
		if !ok || consumed[node] || !file.buildsAString(expression) {
			return true
		}
		reading := evaluation{scan: s, file: file, consumed: consumed}
		for _, value := range reading.value(expression) {
			visit(file.path, value)
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
func (s *scan) constant(ref constantRef) []Value {
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
func (s *scan) local(file *scannedFile, object *ast.Object) []Value {
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
	var values []Value
	unseen := sources == nil || sources.unseen || parameter
	if sources != nil {
		if sources.zero {
			values = append(values, Value{})
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

// placeholder is the string the scan reads a name it cannot see as: one that stands for anything
// (PlaceholderPattern), so it names no table a rule looks for.
func placeholder(name string) []Value {
	return []Value{{{Text: "«" + name + "»"}}}
}

// distinct is values without a string read twice from the same pieces.
func distinct(values []Value) []Value {
	seen := map[string]bool{}
	var answer []Value
	for _, value := range values {
		var key strings.Builder
		for _, piece := range value {
			fmt.Fprintf(&key, "%d %t %t %q;", piece.Line, piece.Multiline, piece.Borrowed, piece.Text)
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
func joined(a, b, fallback []Value) []Value {
	var alone []Value
	if len(b) > 1 && len(a)*len(b) > valueLimit {
		b, alone = fallback, b
	}
	answer := make([]Value, 0, len(a)*len(b)+len(alone))
	for _, left := range a {
		for _, right := range b {
			answer = append(answer, append(slices.Clip(left), right...))
		}
	}
	return append(answer, alone...)
}

// evaluation reads the strings an expression of one file may hold, without type-checking it.
type evaluation struct {
	scan *scan
	file *scannedFile
	// consumed is where an expression read in place marks each part it reads, so the scan does
	// not read that part again on its own; nil for one read where it is borrowed.
	consumed map[ast.Node]bool
	// constant is whether the expression is a constant's value, where a call of one argument
	// is a conversion.
	constant bool
}

// value is every string expression may hold. A string literal holds its own; a + holds each of
// its first part's followed by each of the next part's, and so on along its chain
// (Concatenation); a name holds what its constant or its local variable may (named), and a name
// qualified with an imported package's what that package's constant does (imported); a call of a
// formatting function of fmt holds its format with its arguments in it (formatted). Anything else
// is a placeholder.
func (e evaluation) value(expression ast.Expr) []Value {
	switch x := expression.(type) {
	case *ast.BasicLit:
		e.consume(x)
		_, text, ok := StringLiteral(x)
		if !ok {
			return e.unseen(x)
		}
		return []Value{{{Text: text, Line: e.line(x), Multiline: strings.HasPrefix(x.Value, "`")}}}
	case *ast.ParenExpr:
		return e.value(x.X)
	case *ast.BinaryExpr:
		if x.Op != token.ADD {
			return e.unseen(x)
		}
		parts := Concatenation(x, func(inner *ast.BinaryExpr) { e.consume(inner) })
		values := e.value(parts[0])
		for _, part := range parts[1:] {
			values = joined(values, e.value(part), e.unseen(part))
		}
		return values
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
	case *ast.SelectorExpr:
		if ref, ok := e.imported(x); ok {
			return e.borrowed(x, e.scan.constant(ref))
		}
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
func (e evaluation) named(name *ast.Ident) []Value {
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

// imported is the constant selector names where it qualifies an exported name with the name the
// file gives a package of the tree it imports (pkg.Name): that package's constant of the name, as
// readConstants recorded it. The package is the one of its directory that the file names by its
// own name, or, under a name the file gives it, the one package of its directory but a main. A
// local variable, which the parser resolves, qualifies nothing.
func (e evaluation) imported(selector *ast.SelectorExpr) (constantRef, bool) {
	qualifier, ok := selector.X.(*ast.Ident)
	if !ok || qualifier.Obj != nil || !token.IsExported(selector.Sel.Name) {
		return constantRef{}, false
	}
	for _, imported := range e.file.imports {
		packages := e.scan.imported[imported.path]
		var pkg *scannedPackage
		switch imported.name {
		case "":
			pkg = packages[qualifier.Name]
		case qualifier.Name:
			for name, candidate := range packages {
				if name == "main" {
					continue
				}
				if pkg != nil {
					pkg = nil
					break
				}
				pkg = candidate
			}
		}
		if pkg == nil {
			continue
		}
		if ref, ok := pkg.constants[selector.Sel.Name]; ok {
			return ref, true
		}
	}
	return constantRef{}, false
}

// formatted is every string a call of a formatting function of fmt may make: its format, with
// each %s and %v replaced by what the argument it takes may hold, each %q by that quoted, and
// each other verb by a placeholder, since a number or an error names no table. An argument is
// borrowed: the scan reads it where it is written, as the string it holds alone.
func (e evaluation) formatted(call *ast.CallExpr, at int) []Value {
	arguments := call.Args[at+1:]
	if call.Ellipsis.IsValid() {
		arguments = nil
	}
	argument := func(index int) []Value {
		if index < 0 || index >= len(arguments) {
			return e.unseen(call)
		}
		return e.borrowed(arguments[index], evaluation{scan: e.scan, file: e.file}.value(arguments[index]))
	}
	var answer []Value
	for _, format := range e.value(call.Args[at]) {
		made := []Value{nil}
		next := 0
		for _, piece := range format {
			from := 0
			for i := 0; i < len(piece.Text); i++ {
				if piece.Text[i] != '%' {
					continue
				}
				verb, end, stars, explicit := PrintfVerb(piece.Text, i)
				made = joined(made, []Value{{piece.Slice(from, i)}}, nil)
				next += stars
				if explicit >= 0 {
					next = explicit
				}
				var substituted []Value
				switch verb {
				case '%':
					substituted = []Value{{piece.Slice(i+1, end)}}
				case 's', 'v':
					substituted = argument(next)
					next++
				case 'q':
					for _, value := range argument(next) {
						substituted = append(substituted, Value{{Text: strconv.Quote(value.Text()), Line: e.line(call), Borrowed: true}})
					}
					next++
				default:
					substituted = e.unseen(call)
					next++
				}
				made = joined(made, substituted, e.unseen(call))
				from, i = end, end-1
			}
			made = joined(made, []Value{{piece.Slice(from, len(piece.Text))}}, nil)
		}
		answer = append(answer, made...)
	}
	return distinct(answer)
}

// borrowed is values, each read as one borrowed piece on the line of at, where it is used.
func (e evaluation) borrowed(at ast.Expr, values []Value) []Value {
	answer := make([]Value, 0, len(values))
	for _, value := range values {
		answer = append(answer, Value{{Text: value.Text(), Line: e.line(at), Borrowed: true}})
	}
	return distinct(answer)
}

// unseen is the placeholder for expression, named by it where it is a name or a selector of
// names, on its line.
func (e evaluation) unseen(expression ast.Expr) []Value {
	answer := placeholder(unseenName(expression))
	answer[0][0].Line = e.line(expression)
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
