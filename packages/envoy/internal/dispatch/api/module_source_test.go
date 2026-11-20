// The toolkit the package's source scans share: the module's parsed source, the string
// constants a statement is assembled from, and the text of such a statement. It carries no
// build tag and declares no test of its own, and each of those is deliberate.
//
// No tag, because its callers are on both sides of one. The eight static guards, and
// one_expression_guard_test.go beside them, carry `//go:build !race` for what that saves the
// race job (lock_level_test.go's header says what), while two tests of this package use its
// declarations untagged and run in the race job: ask_asked_of_test.go:480 normalises a
// migration's SQL with sqlWhitespace, and item_writer_census_test.go's
// TestEveryWriterOfAnItemRowIsOnTheOwnerLockCensus, which the text-check job does not run, walks
// the module (moduleSources, sqlStatementsByFunction). A tagged toolkit drops both from the race
// build and the package's test binary stops compiling.
//
// No test, because a test declared here would run wherever this file compiles - the race job
// included - and the eighteen tests of those nine files are the text-check job's to run, once
// each. What an untagged file of declarations costs the race build is its compile and the one
// walk the census asks of it: nothing here walks or parses anything until a test calls it.

package api

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// sqlWhitespace collapses the whitespace of a statement, so a clause written across several
// lines of a raw string reads as one line.
var sqlWhitespace = regexp.MustCompile(`\s+`)

// moduleSource is one non-test Go file of the module with the tree both scans below read. The
// file is parsed once here rather than again in each scan: the constant map and the statement
// walk want the same trees, and parsing the module twice doubled this test's wall time. positions
// is the file set it was parsed into, which turns a node's position into a line.
type moduleSource struct {
	path      string
	file      *ast.File
	positions *token.FileSet
}

// moduleRoot is the envoy module's root, from this package.
const moduleRoot = "../../.."

// sharedModuleSources is the module's parsed source, walked once per test binary: five of this
// package's eight guards and one_expression_guard_test.go read the same files, and the text-check
// job's step runs all six in one go test. In the race build its one caller is the item-writer
// census, which carries no tag. A failed walk's error is kept and reported by each test that asks
// for the sources, as the copy-lock guard's graph is.
var sharedModuleSources = sync.OnceValues(func() ([]moduleSource, error) { return scanModuleSources(moduleRoot) })

// moduleSources reads and parses every non-test Go file of the envoy module, not just the
// Dispatch server: the broker's owner locks live in internal/events, and a future caller could
// lock one of these rows from anywhere. Test sources hold these strings deliberately - the
// upload regressions and the sabotage copies a red proof makes - so they are skipped.
func moduleSources(t *testing.T) []moduleSource {
	t.Helper()
	sources, err := sharedModuleSources()
	if err != nil {
		t.Fatalf("scan dispatch sources: %v", err)
	}
	return sources
}

func scanModuleSources(root string) ([]moduleSource, error) {
	fileSet := token.NewFileSet()
	var sources []moduleSource
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			return err
		}
		sources = append(sources, moduleSource{path: path, file: file, positions: fileSet})
		return nil
	}); err != nil {
		return nil, err
	}
	return sources, nil
}

// stringConstants maps every package-level string constant of the module - and every `var` bound
// to the same shapes - to its value, keyed `<package>.<name>`, so a statement assembled from a
// shared row shape (`select ` + askRowColumns + askRowFrom + ...) is scanned with those pieces in
// place. The key carries the package because two packages may declare the same name over
// different SQL, and welding their values together would fabricate a statement that exists in
// neither.
//
// A constant is routinely written from another one - api's askRowColumns is docs.AskColumns plus
// its own columns - so the map is resolved to a fixpoint rather than in one pass: every
// declaration is re-read against the previous pass's map until a pass changes nothing. Resolving
// once would leave the inner constant a gap and drop its text from every statement built on the
// outer one. Dropping an ambiguous key can unresolve a chain that named it, so a pass takes keys
// away as well as adding them and the iteration is not monotone; it is bounded by the number of
// declarations and fails rather than spinning. It returns its error rather than failing a test, so
// the copy-lock guard's graph, built once for the test binary, can keep it.
func stringConstants(sources []moduleSource) (map[string]string, error) {
	type declaration struct {
		pkg   string
		key   string
		value ast.Expr
	}
	var declarations []declaration
	for _, source := range sources {
		file := source.file
		for _, declared := range file.Decls {
			general, ok := declared.(*ast.GenDecl)
			if !ok || (general.Tok != token.CONST && general.Tok != token.VAR) {
				continue
			}
			for _, spec := range general.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok || len(value.Names) != len(value.Values) {
					continue
				}
				for index, name := range value.Names {
					declarations = append(declarations, declaration{
						pkg:   file.Name.Name,
						key:   file.Name.Name + "." + name.Name,
						value: value.Values[index],
					})
				}
			}
		}
	}
	constants := map[string]string{}
	for pass := 0; pass <= len(declarations); pass++ {
		resolved := map[string]string{}
		dropped := map[string]struct{}{}
		for _, declared := range declarations {
			text, ok := literalText(declared.value, declared.pkg, constants)
			if !ok {
				continue
			}
			if existing, clash := resolved[declared.key]; clash && existing != text {
				// Several directories declare a package of the same name (main,
				// config, store), so this key is not unique. Two different values
				// under it mean the name cannot be resolved: drop it and let it
				// render as a gap.
				dropped[declared.key] = struct{}{}
				delete(resolved, declared.key)
				continue
			}
			if _, gone := dropped[declared.key]; gone {
				continue
			}
			resolved[declared.key] = text
		}
		if maps.Equal(resolved, constants) {
			return constants, nil
		}
		constants = resolved
	}
	return nil, fmt.Errorf("string constants did not settle in %d passes", len(declarations)+1)
}

// sqlStatements returns every string expression in a parsed Go file - a literal or a
// concatenation of literals and known constants - lowercased with its whitespace collapsed, so
// a clause split across lines or constants reads as one statement.
func sqlStatements(file *ast.File, constants map[string]string) []string {
	located := sqlStatementsByFunction(file, constants)
	statements := make([]string, len(located))
	for index, statement := range located {
		statements[index] = statement.statement
	}
	return statements
}

// locatedStatement is one statement sqlStatements finds, with where it is declared. function is
// the name of the function declared at the top level of the file that holds it, its function
// literals included, and empty for one outside any function, such as a constant's value. value is
// the name of the package-level constant or variable whose declaration holds one outside any
// function - the names of a declaration of several, comma-joined, where its values are not one per
// name (`a, b = f()`) - and empty for one in a function or in no value (an import path, a struct
// tag).
type locatedStatement struct {
	function  string
	value     string
	statement string
}

// sqlStatementsByFunction is sqlStatements with the function, or the package-level value, each
// statement is in.
func sqlStatementsByFunction(file *ast.File, constants map[string]string) []locatedStatement {
	var statements []locatedStatement
	// locate adds the statements under node, which is in function or, outside any function, in
	// the package-level value named value.
	locate := func(node ast.Node, function, value string) {
		ast.Inspect(node, func(node ast.Node) bool {
			expression, ok := node.(ast.Expr)
			if !ok {
				return true
			}
			text, ok := literalText(expression, file.Name.Name, constants)
			if !ok {
				return true
			}
			statements = append(statements, locatedStatement{
				function:  function,
				value:     value,
				statement: sqlWhitespace.ReplaceAllString(strings.ToLower(text), " "),
			})
			// The whole concatenation is one statement; its parts are not statements of their own.
			return false
		})
	}
	for _, declared := range file.Decls {
		general, ok := declared.(*ast.GenDecl)
		if !ok {
			function := ""
			if fn, ok := declared.(*ast.FuncDecl); ok {
				function = fn.Name.Name
			}
			locate(declared, function, "")
			continue
		}
		for _, spec := range general.Specs {
			values, ok := spec.(*ast.ValueSpec)
			if !ok {
				locate(spec, "", "")
				continue
			}
			// Each part of the spec is walked in the order ast.Walk takes it, so the statements
			// come out as a walk of the whole spec finds them: the names, the type, the values.
			// A name and, one per name, its value are in that name; the rest is in all of them.
			names := make([]string, len(values.Names))
			for index, name := range values.Names {
				names[index] = name.Name
				locate(name, "", name.Name)
			}
			every := strings.Join(names, ", ")
			if values.Type != nil {
				locate(values.Type, "", every)
			}
			for index, value := range values.Values {
				if len(values.Values) == len(values.Names) {
					locate(value, "", names[index])
				} else {
					locate(value, "", every)
				}
			}
		}
	}
	return statements
}

// literalText renders a string literal, or a `+` chain of literals and constants, as its text.
// A bare identifier resolves in pkg, its own package; a qualified one (docs.AskColumns) resolves
// in the package it names. Inside a `+` chain an operand that resolves to nothing renders as a
// gap, so a query built around a runtime value is still scanned for the parts that are known;
// on its own it is not text at all, so the expression holding it keeps being walked.
func literalText(expression ast.Expr, pkg string, constants map[string]string) (string, bool) {
	switch node := expression.(type) {
	case *ast.BasicLit:
		if node.Kind != token.STRING {
			return "", false
		}
		text, err := strconv.Unquote(node.Value)
		if err != nil {
			return "", false
		}
		return text, true
	case *ast.Ident:
		text, known := constants[pkg+"."+node.Name]
		return text, known
	case *ast.SelectorExpr:
		qualifier, ok := node.X.(*ast.Ident)
		if !ok {
			// A method call on an expression (tx.QueryRow(...).Scan) is not text, and
			// reporting it as text would prune the call holding the statement.
			return "", false
		}
		text, known := constants[qualifier.Name+"."+node.Sel.Name]
		return text, known
	case *ast.BinaryExpr:
		if node.Op != token.ADD {
			return "", false
		}
		left, leftOK := literalText(node.X, pkg, constants)
		right, rightOK := literalText(node.Y, pkg, constants)
		if !leftOK && !rightOK {
			return "", false
		}
		// An operand that resolves to nothing - a call, a runtime value, a constant whose
		// name is ambiguous - contributes its own string literals instead, joined by a
		// space, so a clause they spell is still part of the statement rather than a gap
		// that hides it: strings.Join([]string{"for update"}, "") reds, and so does
		// []string{"for", "update"}, which no single literal spells.
		// A spliced gap is padded on both sides, so a fragment can never weld itself to the
		// operand beyond it. Nothing is inserted when both operands resolve, so a keyword
		// split across a concatenation ("for upd" + "ate") stays one word and is refused.
		if !leftOK {
			left = " " + literalsWithin(node.X) + " "
		}
		if !rightOK {
			right = " " + literalsWithin(node.Y) + " "
		}
		return left + right, true
	}
	return "", false
}

// literalsWithin renders every string literal inside an expression the scan could not resolve,
// so the words of a clause built at runtime still reach the statement. It is deliberately loose,
// and that looseness is the second false-positive shape the lock-level check's own doc names: a
// literal that never reaches the query still lands in the scanned text, so it can supply either
// half of a match - the keyword or an owner table's name. Such a red is told apart the way that
// test's doc describes it: it reads as a word the statement did not spell.
func literalsWithin(expression ast.Expr) string {
	var parts []string
	ast.Inspect(expression, func(node ast.Node) bool {
		literal, ok := node.(*ast.BasicLit)
		if ok && literal.Kind == token.STRING {
			if text, err := strconv.Unquote(literal.Value); err == nil {
				parts = append(parts, text)
			}
		}
		return true
	})
	return strings.Join(parts, " ")
}

// sqlTemplate is one SQL value a source passes to a database sink. statement is the text the
// parser reads, with each run-time fragment standing as `null` so the statement keeps its shape;
// fragments is every string literal inside those fragments, which a reader scans for the words a
// draw needs and which the value's digest covers, so an edit to a fragment moves that digest.
// complete reports whether every byte came from a literal or a package-level constant;
// unresolved reports a sink whose SQL the scan could not read at all.
type sqlTemplate struct {
	locatedStatement
	fragments  string
	complete   bool
	unresolved bool
}

// sqlTemplatesByFunction reads every value a function hands to a pgx or write-batch sink: the
// query of Exec, Query, QueryRow and Prepare, and both argument shapes of Queue, since
// pgx.Batch.Queue takes its SQL first and store.WriteBatch.Queue takes a description first. A
// value bound once to a local is followed to that binding, whatever the local is called; a value
// the scan cannot read at all is reported unresolved rather than dropped.
// sqlSinkParameters names every function whose parameter reaches a database sink as its SQL, with
// the positions of those parameters. A function that hands its caller's string to pgx is a sink
// of its own, so the caller's call of it is read where the caller stands. A name is matched
// without its package, which can only widen what is read.
func sqlSinkParameters(files []*ast.File) map[string][]int {
	forwarded := map[string][]int{}
	for _, file := range files {
		for _, declared := range file.Decls {
			function, ok := declared.(*ast.FuncDecl)
			if !ok || function.Body == nil || function.Type.Params == nil {
				continue
			}
			positions := map[string]int{}
			position := 0
			for _, field := range function.Type.Params.List {
				for _, name := range field.Names {
					positions[name.Name] = position
					position++
				}
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				for _, candidate := range queryArguments(call) {
					identifier, ok := candidate.(*ast.Ident)
					if !ok {
						continue
					}
					index, ok := positions[identifier.Name]
					if !ok || slices.Contains(forwarded[function.Name.Name], index) {
						continue
					}
					forwarded[function.Name.Name] = append(forwarded[function.Name.Name], index)
				}
				return true
			})
		}
	}
	return forwarded
}

func sqlTemplatesByFunction(file *ast.File, constants map[string]string) []sqlTemplate {
	return sqlTemplatesReaching(file, constants, nil)
}

// sqlTemplatesReaching is sqlTemplatesByFunction with the module's forwarding functions, so a
// string a caller hands to such a function is read at the call.
func sqlTemplatesReaching(file *ast.File, constants map[string]string, forwarded map[string][]int) []sqlTemplate {
	var statements []sqlTemplate
	readAt := func(expression ast.Expr, name string, bindings map[string][]ast.Expr) (sqlTemplate, bool) {
		text, fragments, found, complete := sqlTemplateText(expression, file.Name.Name, constants, bindings, map[string]bool{})
		if !found {
			return sqlTemplate{}, false
		}
		return sqlTemplate{
			locatedStatement: locatedStatement{function: name, statement: strings.TrimSpace(text)},
			fragments:        strings.TrimSpace(fragments),
			complete:         complete,
		}, true
	}
	// A package-level constant or variable holds SQL as often as a function body does, and the
	// two id guards read the same module, so both are walked.
	for _, declared := range file.Decls {
		general, ok := declared.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range general.Specs {
			values, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for index, value := range values.Values {
				name := "<package>"
				if index < len(values.Names) {
					name = values.Names[index].Name
				}
				ast.Inspect(value, func(node ast.Node) bool {
					expression, ok := node.(ast.Expr)
					if !ok {
						return true
					}
					template, ok := readAt(expression, name, nil)
					if !ok {
						return true
					}
					statements = append(statements, template)
					return false
				})
			}
		}
	}
	for _, declared := range file.Decls {
		function, ok := declared.(*ast.FuncDecl)
		if !ok || function.Body == nil {
			continue
		}
		bindings := localBindings(function.Body)
		readTemplate := func(expression ast.Expr) (sqlTemplate, bool) {
			text, fragments, found, complete := sqlTemplateText(expression, file.Name.Name, constants, bindings, map[string]bool{})
			if !found {
				return sqlTemplate{}, false
			}
			return sqlTemplate{
				locatedStatement: locatedStatement{function: function.Name.Name, statement: strings.TrimSpace(text)},
				fragments:        strings.TrimSpace(fragments),
				complete:         complete,
			}, true
		}
		// Every expression of the body, not only the argument of a database sink: a statement
		// this scan cannot follow to its sink, one held in a struct field, a map or another
		// function's return, is still written here, and is read where it is written. The
		// arguments a forwarding function hands on to a sink are read for the same reason.
		ast.Inspect(function.Body, func(node ast.Node) bool {
			expression, ok := node.(ast.Expr)
			if !ok {
				return true
			}
			template, ok := readTemplate(expression)
			if !ok {
				return true
			}
			statements = append(statements, template)
			return false
		})
		for _, candidate := range forwardedSinkArguments(function.Body, forwarded) {
			if template, ok := readTemplate(candidate); ok {
				statements = append(statements, template)
			}
		}
	}
	return statements
}

// forwardedSinkArguments is every argument a body hands to a function that passes it on to a
// database sink, so a string built at such a call is read as SQL.
func forwardedSinkArguments(body *ast.BlockStmt, forwarded map[string][]int) []ast.Expr {
	var arguments []ast.Expr
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		arguments = append(arguments, forwardedArguments(call, forwarded)...)
		return true
	})
	return arguments
}

// forwardedArguments is every argument of call that a forwarding function hands to a sink.
func forwardedArguments(call *ast.CallExpr, forwarded map[string][]int) []ast.Expr {
	var name string
	switch function := call.Fun.(type) {
	case *ast.Ident:
		name = function.Name
	case *ast.SelectorExpr:
		name = function.Sel.Name
	default:
		return nil
	}
	var arguments []ast.Expr
	for _, index := range forwarded[name] {
		if index < len(call.Args) {
			arguments = append(arguments, call.Args[index])
		}
	}
	return arguments
}

// localBindings is every expression each local name of the function body is bound to. A name
// bound more than once is followed nowhere: the scan cannot say which binding a sink reads.
func localBindings(body *ast.BlockStmt) map[string][]ast.Expr {
	bindings := map[string][]ast.Expr{}
	ast.Inspect(body, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.AssignStmt:
			if len(node.Lhs) != len(node.Rhs) {
				return true
			}
			for index, name := range node.Lhs {
				if identifier, ok := name.(*ast.Ident); ok {
					bindings[identifier.Name] = append(bindings[identifier.Name], node.Rhs[index])
				}
			}
		case *ast.DeclStmt:
			declaration, ok := node.Decl.(*ast.GenDecl)
			if !ok {
				return true
			}
			for _, spec := range declaration.Specs {
				values, ok := spec.(*ast.ValueSpec)
				if !ok || len(values.Names) != len(values.Values) {
					continue
				}
				for index, name := range values.Names {
					bindings[name.Name] = append(bindings[name.Name], values.Values[index])
				}
			}
		}
		return true
	})
	return bindings
}

// queryArguments is every argument of call that carries SQL. pgx.Batch.Queue's SQL is its first
// argument and store.WriteBatch.Queue's is its second, and the two cannot be told apart without
// types, so a Queue of two arguments or more gives both: a description reads as SQL that parses
// as nothing and holds no event-id construct.
func queryArguments(call *ast.CallExpr) []ast.Expr {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil
	}
	switch selector.Sel.Name {
	case "Exec", "Query", "QueryRow":
		if len(call.Args) > 1 {
			return []ast.Expr{call.Args[1]}
		}
	case "Prepare":
		if len(call.Args) > 2 {
			return []ast.Expr{call.Args[2]}
		}
	case "Queue":
		switch len(call.Args) {
		case 0:
			return nil
		case 1:
			return []ast.Expr{call.Args[0]}
		default:
			return []ast.Expr{call.Args[0], call.Args[1]}
		}
	}
	return nil
}

// sqlTemplateText renders a SQL value two ways: text, the statement with each run-time fragment
// standing as `null`, which keeps the statement parseable, and fragments, the string literals
// inside those run-time expressions (literalsWithin), which a reader scans for the words a draw
// needs. complete is false wherever a fragment stood.
func sqlTemplateText(expression ast.Expr, pkg string, constants map[string]string, bindings map[string][]ast.Expr, resolving map[string]bool) (text, fragments string, found, complete bool) {
	switch node := expression.(type) {
	case *ast.BasicLit:
		if node.Kind != token.STRING {
			return "", "", false, false
		}
		text, err := strconv.Unquote(node.Value)
		return text, "", err == nil, err == nil
	case *ast.Ident:
		if values := bindings[node.Name]; len(values) == 1 && !resolving[node.Name] {
			resolving[node.Name] = true
			text, fragments, found, complete = sqlTemplateText(values[0], pkg, constants, bindings, resolving)
			delete(resolving, node.Name)
			return text, fragments, found, complete
		}
		text, found = constants[pkg+"."+node.Name]
		return text, "", found, found
	case *ast.SelectorExpr:
		qualifier, ok := node.X.(*ast.Ident)
		if !ok {
			return "", "", false, false
		}
		text, found = constants[qualifier.Name+"."+node.Sel.Name]
		return text, "", found, found
	case *ast.CallExpr:
		selector, ok := node.Fun.(*ast.SelectorExpr)
		if !ok || len(node.Args) == 0 || selector.Sel.Name != "Sprintf" {
			return "", "", false, false
		}
		qualifier, ok := selector.X.(*ast.Ident)
		if !ok || qualifier.Name != "fmt" {
			return "", "", false, false
		}
		format, formatFragments, found, _ := sqlTemplateText(node.Args[0], pkg, constants, bindings, resolving)
		if !found {
			return "", "", false, false
		}
		for _, argument := range node.Args[1:] {
			formatFragments += " " + literalsWithin(argument)
		}
		return format, strings.TrimSpace(formatFragments), true, false
	case *ast.BinaryExpr:
		if node.Op != token.ADD {
			return "", "", false, false
		}
		left, leftFragments, leftFound, leftComplete := sqlTemplateText(node.X, pkg, constants, bindings, resolving)
		right, rightFragments, rightFound, rightComplete := sqlTemplateText(node.Y, pkg, constants, bindings, resolving)
		if !leftFound && !rightFound {
			return "", "", false, false
		}
		if !leftFound {
			left = " null "
			leftFragments = literalsWithin(node.X)
		}
		if !rightFound {
			right = " null "
			rightFragments = literalsWithin(node.Y)
		}
		return left + right, strings.TrimSpace(leftFragments + " " + rightFragments), true, leftFound && rightFound && leftComplete && rightComplete
	}
	return "", "", false, false
}
