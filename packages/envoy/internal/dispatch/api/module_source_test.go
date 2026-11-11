// The toolkit the package's source scans share: the module's parsed source, the string
// constants a statement is assembled from, and the text of such a statement. It carries no
// build tag and declares no test of its own, and each of those is deliberate.
//
// No tag, because its callers are on both sides of one. The six static guards, and
// one_expression_guard_test.go beside them, carry `//go:build !race` for what that saves the
// race job (lock_level_test.go's header says what), while one test main added to this package
// uses a declaration of this file untagged and runs in the race job: ask_asked_of_test.go:480
// normalises a migration's SQL with sqlWhitespace. A tagged toolkit drops that one from the
// race build and the package's test binary stops compiling.
//
// No test, because a test declared here would run wherever this file compiles - the race job
// included - and the twelve tests of those seven files are the text-check job's to run, once
// each. What an untagged file of declarations costs the race build is this one compile:
// nothing here walks or parses anything until a test calls it, and under `-race` that one
// read of sqlWhitespace is the only call any of it gets.

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

// sharedModuleSources is the module's parsed source, walked once per test binary: four of this
// package's six guards and one_expression_guard_test.go read the same files, and the text-check
// job's step runs all five in one go test. In the race build it has no caller at all, since
// every one of those five carries the tag; there it is a declaration the test binary compiles
// and nothing runs. A failed walk's error is kept and reported by each test that asks for the
// sources, as the copy-lock guard's graph is.
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
	var statements []string
	ast.Inspect(file, func(node ast.Node) bool {
		expression, ok := node.(ast.Expr)
		if !ok {
			return true
		}
		text, ok := literalText(expression, file.Name.Name, constants)
		if !ok {
			return true
		}
		statements = append(statements, sqlWhitespace.ReplaceAllString(strings.ToLower(text), " "))
		// The whole concatenation is one statement; its parts are not statements of their own.
		return false
	})
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
