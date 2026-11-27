package ompsessions

import (
	"cmp"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testtypes"
)

// helperName is the one function of this package that opens a transaction: inTransaction, whose
// transaction pins search_path, and the rest, before anything else runs in it.
const helperName = "inTransaction"

// pinnedName is the type of the transaction the helper hands its function: only the helper builds
// one, and only its methods run a statement on it.
const pinnedName = "pinned"

// connName is the type Connect and ConnectURL answer, the connection a caller holds: only the
// constructor builds one, and only the helper, the constructor and its connMethods reach the
// connection it holds, so no other package can hand it to code of its own.
const connName = "Conn"

// constructorName is the one function that opens a connection and builds a Conn.
const constructorName = "ConnectURL"

// connMethods are the methods of Conn that may reach its connection: they close it, and say
// whether it is closed, and run no statement.
var connMethods = []string{"Close", "IsClosed"}

// driverPackage reports whether path is a package whose methods and functions the check watches:
// pgx and every package under it (pgconn, pgxpool, pgtype, pgproto3), and database/sql with its
// driver package. Every method or function of these packages runs a statement, or may, unless
// allowedMethods names it, or constructorFunctions names it in the constructor's body: the check
// denies by default, so a method pgx adds, or one it has and nobody listed (LoadType, Prepare,
// ExecStatement, StartPipeline, Hijack, a large object's Close), counts as a statement.
func driverPackage(path string) bool {
	return path == "github.com/jackc/pgx/v5" || strings.HasPrefix(path, "github.com/jackc/pgx/v5/") ||
		path == "database/sql" || path == "database/sql/driver"
}

// allowedMethods are the methods of those packages, each by the type that declares it, that send
// no statement the server resolves a name in, and so may be called anywhere: ending a handle
// (closing a connection, releasing a pooled one, ending a transaction with COMMIT or ROLLBACK,
// which name nothing, closing a result set or a result reader), saying whether a connection is
// closed, handing out the PgConn under a connection (a statement on it is a method of its own,
// which the check reads), and reading a result a statement already returned. A method of the same
// name on another type is not allowed: a large object's Close runs select lo_close, and a PgConn's
// Conn is the raw socket.
var allowedMethods = []string{
	"github.com/jackc/pgx/v5.Conn.Close",
	"github.com/jackc/pgx/v5.Conn.IsClosed",
	"github.com/jackc/pgx/v5.Conn.PgConn",
	"github.com/jackc/pgx/v5.Tx.Commit",
	"github.com/jackc/pgx/v5.Tx.Rollback",
	"github.com/jackc/pgx/v5.Row.Scan",
	"github.com/jackc/pgx/v5.Rows.Next",
	"github.com/jackc/pgx/v5.Rows.Scan",
	"github.com/jackc/pgx/v5.Rows.Close",
	"github.com/jackc/pgx/v5.Rows.Err",
	"github.com/jackc/pgx/v5/pgconn.CommandTag.RowsAffected",
	"github.com/jackc/pgx/v5/pgconn.MultiResultReader.Close",
	"github.com/jackc/pgx/v5/pgconn.ResultReader.Close",
	"github.com/jackc/pgx/v5/pgxpool.Conn.Release",
	"database/sql.Tx.Commit",
	"database/sql.Tx.Rollback",
	"database/sql.Row.Scan",
	"database/sql.Rows.Close",
}

// constructorFunctions are the package functions of those packages that run no statement, and
// that the constructor alone may call: pgx's Connect, ConnectConfig and ParseConfig, which read a
// URL and open a connection with its startup packet alone, and database/sql's Open, which opens
// nothing until a statement runs. Each answers a raw connection, so a call of one anywhere else is
// a connection no Conn holds, which the package's code could hand to another package's.
var constructorFunctions = []string{
	"github.com/jackc/pgx/v5.Connect",
	"github.com/jackc/pgx/v5.ConnectConfig",
	"github.com/jackc/pgx/v5.ParseConfig",
	"database/sql.Open",
}

// refusedImports are the packages the package's code may not import: reflect, which builds a
// value of any type and sets one whole from a value of its shape, and unsafe, which reads and
// writes any memory. Through either, code builds a pinned or a Conn, or sets the field of one,
// with no composite literal, conversion or selector the check reads.
var refusedImports = []string{"reflect", "unsafe"}

// place is where statementsOutside found a statement can run outside the helper, and what runs.
type place struct {
	pos  token.Pos
	what string
}

// statementsOutside is every place in files, which info type-checked as pkg, where a statement
// can run outside the helper, in source order, and how many places it found where one runs inside
// it. A place is:
//
//   - a call of, or a method value taken from, a method or a function of a driver package
//     (driverPackage) outside the helper's own body and the bodies of pinned's methods, unless
//     allowedMethods names the method, or constructorFunctions the function and it is in the
//     constructor's body;
//   - a pinned built, by a composite literal or a conversion, outside the helper's body, since
//     only the transaction the helper pinned may be one, and a Conn built outside the
//     constructor's body, since only the connection the constructor opened may be one. What is
//     built is a value of a type whose values are a pinned or a Conn, or point at one (isOf): the
//     type under any alias of it, a struct type of its shape written out, which is assignable to
//     it, and a type parameter whose constraint names types (namesTypes) and which it satisfies;
//     so an element of a composite literal of pointers, whose &T the literal elides, a pointer
//     conversion, and a generic literal or conversion each build one too. A conversion of nil
//     builds none;
//   - a value of the struct type pinned or Conn is declared with, written out with no name of its
//     own, outside the body that may build one: it becomes one with no conversion wherever one is
//     wanted, returned as one, handed to a function as one or assigned whole to one. A composite
//     literal and a conversion are the rule above's to name, and the value a field is selected
//     from the rule below's;
//   - a field of pinned, or of a type whose values are a pinned, reached outside pinned's methods,
//     and one of Conn reached outside the helper's body, the constructor's and Conn's
//     connMethods: how a pinned or a Conn made from its zero value, by new, or as a struct of its
//     shape would be given a transaction or a connection of another's, and how a raw connection
//     would leave the package's own code;
//   - a field of Conn that is exported, or embedded, whose methods Conn promotes even where its
//     name is unexported: code outside the package reaches the connection through it, where the
//     rule above cannot see;
//   - an interface, declared or written as a constraint anywhere in the files, that a type of a
//     driver package implements: a driver handle held in one runs its statements through methods
//     no driver package declares, which the first rule cannot see;
//   - an import of reflect or unsafe (refusedImports), through which code builds a pinned or a
//     Conn, or sets the field of one, with no composite literal, conversion or selector the rules
//     above read.
//
// So the only raw connections in the package's code are the constructor's and the helper's, and a
// caller holds a Conn no other package's interface or function takes. Inside is a watched call in
// the helper's body or a method of pinned. A method of pinned or of Conn is one whose receiver is
// of it (isOf), under whatever alias it declares. With no helper every place is outside. What the
// check does not follow is a driver handle a caller hands the package's code as an argument, which
// is the caller's own, and nothing in the package takes one; nor what the helper's body, pinned's
// methods and the constructor do with the handles they hold, which is the code a reader of the
// package checks by eye: each is a few lines.
func statementsOutside(fset *token.FileSet, files []*ast.File, info *types.Info, pkg *types.Package) (outside []string, inside int) {
	helper := helperOf(pkg)
	lookupType := func(name string) *types.TypeName {
		typeName, _ := pkg.Scope().Lookup(name).(*types.TypeName)
		return typeName
	}
	pinnedType, connType := lookupType(pinnedName), lookupType(connName)
	constructor, _ := pkg.Scope().Lookup(constructorName).(*types.Func)
	var helperBody, constructorBody *ast.BlockStmt
	var bodies, pinnedMethods, connMethodBodies []*ast.BlockStmt
	// isOf reports whether a value of typ is a value of typeName, or points at one: typ, or the type
	// it points at, is assignable to typeName's type, as it is under any alias of either and as a
	// struct type of its shape written out, or is a type parameter whose constraint names types and
	// which typeName's type, or a pointer to it, satisfies.
	isOf := func(typ types.Type, typeName *types.TypeName) bool {
		if typ == nil || typeName == nil {
			return false
		}
		if pointer, ok := typ.Underlying().(*types.Pointer); ok {
			typ = pointer.Elem()
		}
		if parameter, ok := types.Unalias(typ).(*types.TypeParam); ok {
			constraint, ok := parameter.Constraint().Underlying().(*types.Interface)
			return ok && namesTypes(constraint) &&
				(types.Satisfies(typeName.Type(), constraint) || types.Satisfies(types.NewPointer(typeName.Type()), constraint))
		}
		return types.AssignableTo(typ, typeName.Type())
	}
	// receiverOf is the type a method declares its receiver as, nil for a function.
	receiverOf := func(function *ast.FuncDecl) types.Type {
		if function.Recv == nil || len(function.Recv.List) != 1 {
			return nil
		}
		return info.Types[function.Recv.List[0].Type].Type
	}
	for _, file := range files {
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			switch receiver := receiverOf(function); {
			case receiver == nil && helper != nil && info.Defs[function.Name] == helper:
				helperBody = function.Body
				bodies = append(bodies, function.Body)
			case receiver == nil && constructor != nil && info.Defs[function.Name] == constructor:
				constructorBody = function.Body
			case isOf(receiver, pinnedType):
				bodies = append(bodies, function.Body)
				pinnedMethods = append(pinnedMethods, function.Body)
			case isOf(receiver, connType) && slices.Contains(connMethods, function.Name.Name):
				connMethodBodies = append(connMethodBodies, function.Body)
			}
		}
	}
	within := func(body *ast.BlockStmt, pos token.Pos) bool {
		return body != nil && body.Pos() <= pos && pos < body.End()
	}
	inAny := func(list []*ast.BlockStmt, pos token.Pos) bool {
		return slices.ContainsFunc(list, func(body *ast.BlockStmt) bool { return within(body, pos) })
	}
	fieldsOf := func(typeName *types.TypeName) []*types.Var {
		var fields []*types.Var
		if typeName != nil {
			if structure, ok := typeName.Type().Underlying().(*types.Struct); ok {
				for i := range structure.NumFields() {
					fields = append(fields, structure.Field(i))
				}
			}
		}
		return fields
	}
	pinnedFields, connFields := fieldsOf(pinnedType), fieldsOf(connType)
	var places []place
	for _, field := range connFields {
		if field.Exported() || field.Embedded() {
			places = append(places, place{field.Pos(), fmt.Sprintf("%s.%s (an exported or embedded field of %s, which code outside the package reaches)",
				connName, field.Name(), connName)})
		}
	}
	// holderOf is the type whose own field selection selects: the type of the value it selects
	// from, or of the embedded field it selects through.
	holderOf := func(selection *types.Selection) types.Type {
		holder, path := selection.Recv(), selection.Index()
		for _, i := range path[:len(path)-1] {
			if pointer, ok := holder.Underlying().(*types.Pointer); ok {
				holder = pointer.Elem()
			}
			structure, ok := holder.Underlying().(*types.Struct)
			if !ok {
				return nil
			}
			holder = structure.Field(i).Type()
		}
		return holder
	}
	for selector, selection := range info.Selections {
		if field, ok := selection.Obj().(*types.Var); ok {
			holder := holderOf(selection)
			switch {
			case (slices.Contains(pinnedFields, field) || isOf(holder, pinnedType)) && !inAny(pinnedMethods, selector.Pos()):
				places = append(places, place{selector.Pos(), fmt.Sprintf("%s (the transaction a %s holds, reached outside %s's methods)",
					types.ExprString(selector), pinnedName, pinnedName)})
			case (slices.Contains(connFields, field) || isOf(holder, connType)) && !within(helperBody, selector.Pos()) &&
				!within(constructorBody, selector.Pos()) &&
				!inAny(connMethodBodies, selector.Pos()):
				places = append(places, place{selector.Pos(), fmt.Sprintf("%s (the connection a %s holds, reached outside %s, %s and %s's %s)",
					types.ExprString(selector), connName, helperName, constructorName, connName, strings.Join(connMethods, " and "))})
			}
			continue
		}
		method, ok := selection.Obj().(*types.Func)
		if !ok || method.Pkg() == nil || !driverPackage(method.Pkg().Path()) {
			continue
		}
		key, name := declaringType(method, selection)
		if slices.Contains(allowedMethods, key+"."+method.Name()) {
			continue
		}
		if inAny(bodies, selector.Pos()) {
			inside++
			continue
		}
		places = append(places, place{selector.Sel.Pos(), fmt.Sprintf("%s (a method of %s)", types.ExprString(selector), name)})
	}
	for ident, object := range info.Uses {
		function, ok := object.(*types.Func)
		if !ok || function.Pkg() == nil || !driverPackage(function.Pkg().Path()) || function.Type().(*types.Signature).Recv() != nil {
			continue
		}
		if within(constructorBody, ident.Pos()) && slices.Contains(constructorFunctions, function.Pkg().Path()+"."+function.Name()) {
			continue
		}
		if inAny(bodies, ident.Pos()) {
			inside++
			continue
		}
		places = append(places, place{ident.Pos(), fmt.Sprintf("%s.%s (a function of %s)", function.Pkg().Name(), function.Name(), function.Pkg().Name())})
	}
	// qualifier writes a type of pkg by its name alone, and one of another package by its package's
	// name, as the package's code writes it.
	qualifier := func(other *types.Package) string {
		if other == pkg {
			return ""
		}
		return other.Name()
	}
	// built names a value of pinned or Conn that expression, a composite literal or a conversion,
	// builds outside the one body that may.
	built := func(expression ast.Expr) {
		typ := info.Types[expression].Type
		written := types.ExprString(expression)
		if literal, ok := expression.(*ast.CompositeLit); ok && literal.Type == nil {
			// An element of a composite literal, whose type the literal elides, with a pointer's &:
			// written whole, as a reader writes it.
			elements := "{}"
			if len(literal.Elts) > 0 {
				elements = "{…}"
			}
			written = types.TypeString(typ, qualifier) + elements
			if pointer, ok := types.Unalias(typ).(*types.Pointer); ok {
				written = "&" + types.TypeString(pointer.Elem(), qualifier) + elements
			}
		}
		switch {
		case isOf(typ, pinnedType) && !within(helperBody, expression.Pos()):
			places = append(places, place{expression.Pos(), fmt.Sprintf("%s (a %s transaction built outside %s)", written, pinnedName, helperName)})
		case isOf(typ, connType) && !within(constructorBody, expression.Pos()):
			places = append(places, place{expression.Pos(), fmt.Sprintf("%s (a %s built outside %s)", written, connName, constructorName)})
		}
	}
	// shapeOf is the struct type typeName is declared with, nil where there is none.
	shapeOf := func(typeName *types.TypeName) *types.Struct {
		if typeName == nil {
			return nil
		}
		structure, _ := typeName.Type().Underlying().(*types.Struct)
		return structure
	}
	pinnedShape, connShape := shapeOf(pinnedType), shapeOf(connType)
	// selected is every expression a field or a method is selected from: the field rule names a
	// field reached, and the value it is reached on becomes no pinned and no Conn there.
	selected := map[ast.Expr]bool{}
	for selector := range info.Selections {
		selected[ast.Unparen(selector.X)] = true
	}
	// shaped names a value expression is of pinned's or Conn's struct type, written out with no
	// name of its own, outside the one body that may build one.
	shaped := func(expression ast.Expr) {
		value, ok := info.Types[expression]
		if !ok || !value.IsValue() || selected[expression] {
			return
		}
		structure, ok := types.Unalias(value.Type).(*types.Struct)
		switch {
		case !ok:
		case pinnedShape != nil && types.Identical(structure, pinnedShape) && !within(helperBody, expression.Pos()):
			places = append(places, place{expression.Pos(), fmt.Sprintf("%s (a value of %s's struct type, which becomes a %s with no conversion, outside %s)",
				types.ExprString(expression), pinnedName, pinnedName, helperName)})
		case connShape != nil && types.Identical(structure, connShape) && !within(constructorBody, expression.Pos()):
			places = append(places, place{expression.Pos(), fmt.Sprintf("%s (a value of %s's struct type, which becomes a %s with no conversion, outside %s)",
				types.ExprString(expression), connName, connName, constructorName)})
		}
	}
	handles := driverTypes(pkg)
	for _, file := range files {
		for _, spec := range file.Imports {
			if path, _ := strconv.Unquote(spec.Path.Value); slices.Contains(refusedImports, path) {
				places = append(places, place{spec.Path.Pos(), fmt.Sprintf("the import of %s (through which code builds a %s or a %s, or reaches a field of one, where the check cannot see)",
					path, pinnedName, connName)})
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.CompositeLit:
				built(node)
			case *ast.CallExpr:
				switch {
				case !info.Types[node.Fun].IsType():
					shaped(node)
				case len(node.Args) == 1 && info.Types[node.Args[0]].IsNil():
					// A conversion of nil is a nil pointer, which holds nothing.
				default:
					built(node)
				}
			case *ast.InterfaceType:
				iface, ok := info.Types[node].Type.Underlying().(*types.Interface)
				if !ok || iface.NumMethods() == 0 {
					return true
				}
				if slices.ContainsFunc(handles, func(handle types.Type) bool { return types.Implements(handle, iface) }) {
					places = append(places, place{node.Pos(), "an interface a driver handle implements"})
				}
			case *ast.ParenExpr:
				// The expression it holds is read for itself.
			case ast.Expr:
				shaped(node)
			}
			return true
		})
	}
	slices.SortFunc(places, func(a, b place) int { return cmp.Compare(a.pos, b.pos) })
	for _, p := range places {
		outside = append(outside, describePlace(fset.Position(p.pos), p.what, helperName))
	}
	return outside, inside
}

// namesTypes reports whether constraint names a type: a union, or an embedded type that is no
// interface, in it or in a constraint it embeds. A type parameter whose constraint names none
// (any, comparable, a set of methods) converts only a value of its own type and has no composite
// literal, so it builds no pinned and no Conn, though each satisfies it.
func namesTypes(constraint *types.Interface) bool {
	for i := range constraint.NumEmbeddeds() {
		embedded, ok := constraint.EmbeddedType(i).Underlying().(*types.Interface)
		if !ok || namesTypes(embedded) {
			return true
		}
	}
	return false
}

// driverTypes is every named type the driver packages pkg imports, directly or not, declare, and
// a pointer to each that is no interface: what a driver handle can be.
func driverTypes(pkg *types.Package) []types.Type {
	var handles []types.Type
	seen := map[*types.Package]bool{}
	var walk func(*types.Package)
	walk = func(imported *types.Package) {
		if seen[imported] {
			return
		}
		seen[imported] = true
		if driverPackage(imported.Path()) {
			scope := imported.Scope()
			for _, name := range scope.Names() {
				typeName, ok := scope.Lookup(name).(*types.TypeName)
				if !ok || typeName.IsAlias() {
					continue
				}
				named, ok := typeName.Type().(*types.Named)
				if !ok || named.TypeParams().Len() > 0 {
					continue
				}
				handles = append(handles, named)
				if !types.IsInterface(named) {
					handles = append(handles, types.NewPointer(named))
				}
			}
		}
		for _, next := range imported.Imports() {
			walk(next)
		}
	}
	walk(pkg)
	return handles
}

// describePlace is how statementsOutside names a place where a statement runs outside helper:
// its file and line, what runs there, and the helper's name.
func describePlace(position token.Position, what, helper string) string {
	return fmt.Sprintf("%s:%d: %s runs a statement outside %s", filepath.Base(position.Filename), position.Line, what, helper)
}

// declaringType is the named type that declares method, as a key of allowedMethods and as a
// reader writes it (*pgx.Conn, pgx.Tx). A method an interface declares without naming it is read
// from the selection's receiver.
func declaringType(method *types.Func, selection *types.Selection) (key, name string) {
	receiver := method.Type().(*types.Signature).Recv().Type()
	if _, ok := types.Unalias(receiver).(*types.Interface); ok {
		receiver = selection.Recv()
	}
	pointer := ""
	if p, ok := types.Unalias(receiver).(*types.Pointer); ok {
		receiver, pointer = p.Elem(), "*"
	}
	named, ok := types.Unalias(receiver).(*types.Named)
	if !ok || named.Obj().Pkg() == nil {
		return "", ""
	}
	object := named.Obj()
	return object.Pkg().Path() + "." + object.Name(), pointer + object.Pkg().Name() + "." + object.Name()
}

// typeCheck parses files and type-checks them as one package, reading what they import from
// where the compiler left it for each of patterns (testtypes.CompiledDependencies).
func typeCheck(t *testing.T, path string, files []string, patterns ...string) (*token.FileSet, []*ast.File, *types.Info, *types.Package) {
	t.Helper()
	var lookups []importer.Lookup
	for _, pattern := range patterns {
		lookup, err := testtypes.CompiledDependencies(pattern)
		if err != nil {
			t.Fatal(err)
		}
		lookups = append(lookups, lookup)
	}
	lookup := func(path string) (io.ReadCloser, error) {
		var errs []string
		for _, lookup := range lookups {
			opened, err := lookup(path)
			if err == nil {
				return opened, nil
			}
			errs = append(errs, err.Error())
		}
		return nil, fmt.Errorf("no compiled types for %s: %s", path, strings.Join(errs, "; "))
	}
	fset := token.NewFileSet()
	var parsed []*ast.File
	for _, file := range files {
		syntax, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		parsed = append(parsed, syntax)
	}
	info := &types.Info{
		Types: map[ast.Expr]types.TypeAndValue{}, Defs: map[*ast.Ident]types.Object{},
		Uses: map[*ast.Ident]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{},
	}
	config := types.Config{Importer: importer.ForCompiler(fset, "gc", lookup)}
	pkg, err := config.Check(path, fset, parsed, info)
	if err != nil {
		t.Fatalf("type-check %s: %v", strings.Join(files, ", "), err)
	}
	return fset, parsed, info, pkg
}

// helperOf is pkg's helper, nil where pkg declares none.
func helperOf(pkg *types.Package) *types.Func {
	helper, _ := pkg.Scope().Lookup(helperName).(*types.Func)
	return helper
}

// wantComment is a fixture's mark of a line the check must name: `// want "<what>"`, what being
// the place as statementsOutside describes it.
var wantComment = regexp.MustCompile(`^// want "(.*)"$`)

// wanted is what a fixture's want comments say the check names, each with its file and line, as
// statementsOutside writes a place.
func wanted(fset *token.FileSet, file *ast.File) []string {
	var want []string
	for _, group := range file.Comments {
		for _, comment := range group.List {
			if match := wantComment.FindStringSubmatch(comment.Text); match != nil {
				want = append(want, describePlace(fset.Position(comment.Pos()), match[1], helperName))
			}
		}
	}
	return want
}

// Every statement the package runs runs inside inTransaction, whose transaction pins search_path,
// and the rest, before anything else runs in it (TestEveryTransactionPinsBeforeItsWork holds that
// order, and TestEveryPoolPinsItsSearchPath reads the pin), so the pin covers every statement.
// The helper hands its function a pinned, the one type whose methods run a statement, and only the
// helper builds one; Connect and ConnectURL hand their caller a Conn, whose connection only the
// helper, the constructor and Conn's Close and IsClosed reach, and only the constructor builds
// one. So outside the helper's body and pinned's methods no method or function of pgx's or
// database/sql's packages is called, or taken as a value, but the few allowedMethods, each named
// with its type, and constructorFunctions in the constructor's body; no pinned or Conn is built
// elsewhere, by a literal, a conversion or a value of its struct type written out, and no field of
// either is reached elsewhere; Conn exports and embeds no field; no interface the package declares
// or writes is one a driver handle implements; and the package imports neither reflect nor unsafe
// (statementsOutside). The check type-checks the package's own files, test files aside, since
// those seed fixtures. Its controls are the fixtures of testdata/outside, each type-checked alone
// with a helper and a pinned of its own: a pgx call outside the helper, a database/sql call, a
// pool's connection acquired and run, the other ways a transaction the helper did not open runs a
// statement, the Go shapes that carry one into the helper's function or a driver handle past the
// check, the ways a Conn's connection reaches other code, a pinned or a Conn built, or its methods
// declared, through an alias of it, with it declared as an alias, where no composite literal or
// conversion names its type, or from a value of its struct type written out with no conversion at
// all, and reflect and unsafe imported; and conversions that build nothing, of a type parameter
// whose constraint names no type and of nil. The check must name exactly the lines their want
// comments mark and none of the calls inside their helper, and it must find statements inside the
// helper in each, or it read nothing.
func TestNoStatementRunsOutsideTheHelper(t *testing.T) {
	t.Run("this package", func(t *testing.T) {
		entries, err := os.ReadDir(".")
		if err != nil {
			t.Fatal(err)
		}
		var files []string
		for _, entry := range entries {
			if name := entry.Name(); strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
				files = append(files, name)
			}
		}
		if !slices.Contains(files, "ompsessions.go") {
			t.Fatalf("UNKNOWN: the check read %v, which holds no ompsessions.go: it runs in the package's own directory", files)
		}
		const path = "github.com/sjawhar/legion/daemon/internal/ompsessions"
		fset, parsed, info, pkg := typeCheck(t, path, files, path)
		if helperOf(pkg) == nil {
			t.Errorf("the package declares no function %s, the helper every statement runs through", helperName)
		}
		if _, ok := pkg.Scope().Lookup(pinnedName).(*types.TypeName); !ok {
			t.Errorf("the package declares no type %s, the transaction %s hands its function", pinnedName, helperName)
		}
		if _, ok := pkg.Scope().Lookup(connName).(*types.TypeName); !ok {
			t.Errorf("the package declares no type %s, the connection only %s builds and only %s unwraps", connName, constructorName, helperName)
		}
		if _, ok := pkg.Scope().Lookup(constructorName).(*types.Func); !ok {
			t.Errorf("the package declares no function %s, the one place a connection is opened", constructorName)
		}
		outside, inside := statementsOutside(fset, parsed, info, pkg)
		for _, place := range outside {
			t.Error(place)
		}
		if inside == 0 {
			t.Errorf("UNKNOWN: the check found no statement inside %s or %s's methods in %v, so it read none of the package's statements", helperName, pinnedName, files)
		}
		t.Logf("%d statements inside %s and %s's methods in %v", inside, helperName, pinnedName, files)
	})

	// Each shape that would carry the package's connection to another package's code, round 3's
	// and round 4's: held as that package's interface, assigned to it as part of a tuple, returned
	// as it as part of one, stored in a struct field of its type, and handed to its function. Each
	// is a package outside this one that opens the connection with ConnectURL, type-checked against
	// the package as the compiler left it, and each must be refused, since the connection
	// ConnectURL answers is a Conn no other package's interface can hold. The control is each same
	// shape with the connection pgx.Connect answers, which must type-check, so the refusal is the
	// Conn's and no fault of the shape.
	t.Run("the connection cannot leave the package", func(t *testing.T) {
		shapes := map[string]string{
			"held as another package's interface": `conn, err := OPEN(ctx, dsn)
	if err != nil {
		return nil, err
	}
	var q store.TxPool = conn
	return q, nil`,
			"assigned to it as part of a tuple": `var q store.TxPool
	var err error
	q, err = OPEN(ctx, dsn)
	return q, err`,
			"returned as it as part of a tuple": `return OPEN(ctx, dsn)`,
			"stored in a struct field of its type": `conn, err := OPEN(ctx, dsn)
	if err != nil {
		return nil, err
	}
	return carrier{q: conn}.q, nil`,
			"handed to its function": `conn, err := OPEN(ctx, dsn)
	if err != nil {
		return nil, err
	}
	err = store.Transact(ctx, conn, func(context.Context, pgx.Tx) error { return nil })
	return nil, err`,
		}
		const refusal = "does not implement store.TxPool (missing method BeginTx)"
		for _, name := range slices.Sorted(maps.Keys(shapes)) {
			for _, opener := range []string{"pgx.Connect", "ompsessions.ConnectURL"} {
				err := typeCheckSource(t, strings.Replace(outsider, "BODY", strings.ReplaceAll(shapes[name], "OPEN", opener), 1),
					"github.com/sjawhar/legion/daemon/internal/ompsessions", "github.com/sjawhar/legion/daemon/internal/store")
				switch {
				case opener == "pgx.Connect" && err != nil:
					t.Errorf("control: %s, with pgx.Connect's connection, does not type-check: %v", name, err)
				case opener == "ompsessions.ConnectURL" && (err == nil || !strings.Contains(err.Error(), refusal)):
					t.Errorf("%s, with ConnectURL's connection: the type check answered %v, want it refused: %s", name, err, refusal)
				default:
					t.Logf("%s, with %s's connection: %v", name, opener, err)
				}
			}
		}
	})

	fixtures, err := filepath.Glob(filepath.Join("testdata", "outside", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	if len(fixtures) == 0 {
		t.Fatal("UNKNOWN: testdata/outside holds no fixture, so no control ran")
	}
	for _, fixture := range fixtures {
		t.Run("control "+filepath.Base(fixture), func(t *testing.T) {
			fset, parsed, info, pkg := typeCheck(t, "fixture", []string{fixture},
				"github.com/jackc/pgx/v5/pgxpool", "database/sql", "github.com/sjawhar/legion/daemon/internal/store")
			if helperOf(pkg) == nil {
				t.Fatalf("the fixture declares no function %s", helperName)
			}
			want := wanted(fset, parsed[0])
			if len(want) == 0 {
				t.Fatal("the fixture carries no want comment, so it controls nothing")
			}
			outside, inside := statementsOutside(fset, parsed, info, pkg)
			if !slices.Equal(outside, want) {
				t.Errorf("the check named\n\t%s\nwant\n\t%s", strings.Join(outside, "\n\t"), strings.Join(want, "\n\t"))
			}
			if inside == 0 {
				t.Errorf("the check found no statement inside the fixture's %s, which runs some", helperName)
			}
		})
	}
}

// outsider is a package outside this one whose function shape opens a connection and carries it
// toward store's code, which opens a transaction on any pool it is handed (store.TxPool): BODY is
// the shape, written with OPEN for the function that opens it.
const outsider = `package outsider

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/store"
)

var _, _ = pgx.Connect, ompsessions.ConnectURL

type carrier struct{ q store.TxPool }

func shape(ctx context.Context, dsn string) (store.TxPool, error) {
	BODY
}
`

// typeCheckSource type-checks source as a package of its own, reading what it imports from where
// the compiler left it for each of patterns, and answers every error the type check found, joined,
// nil for none. A parse error fails the test: the source is the test's own.
func typeCheckSource(t *testing.T, source string, patterns ...string) error {
	t.Helper()
	var lookups []importer.Lookup
	for _, pattern := range patterns {
		lookup, err := testtypes.CompiledDependencies(pattern)
		if err != nil {
			t.Fatal(err)
		}
		lookups = append(lookups, lookup)
	}
	lookup := func(path string) (io.ReadCloser, error) {
		var errs []string
		for _, lookup := range lookups {
			opened, err := lookup(path)
			if err == nil {
				return opened, nil
			}
			errs = append(errs, err.Error())
		}
		return nil, fmt.Errorf("no compiled types for %s: %s", path, strings.Join(errs, "; "))
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "outsider.go", source, 0)
	if err != nil {
		t.Fatalf("parse the outside package: %v\n%s", err, source)
	}
	var found []error
	config := types.Config{Importer: importer.ForCompiler(fset, "gc", lookup), Error: func(err error) { found = append(found, err) }}
	_, _ = config.Check("outsider", fset, []*ast.File{file}, nil)
	return errors.Join(found...)
}
