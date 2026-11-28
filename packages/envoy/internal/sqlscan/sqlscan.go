// Package sqlscan reads the SQL a source tree holds, for the tests that hold every statement of
// the tree to a rule by reading the source rather than a database. It walks the tree (GoFiles,
// SQLFiles, Root), reads SQL text (WithoutComments, Statements), reads the strings of Go source
// (StringLiteral, LiteralLine, Concatenation, PrintfVerb, NextArgument), and reads every string a
// Go file holds or builds as the statement it makes (ReadGo, ReadSQL, ReadSources; strings.go).
// graphstore's append-only and index-set tests and api's projection, migration and source-guard
// tests each read the tree through it, so a fix to how the tree's SQL is read reaches every one
// of them.
//
// It is an ordinary package rather than a _test.go file because Go cannot share test-only code
// across packages (internal/explaintest's reasoning).
package sqlscan

import (
	"go/ast"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// skippedDirectories are the directories no walk of the tree enters: installed and vendored
// dependencies, fixtures, version control and build output.
var skippedDirectories = map[string]bool{
	"node_modules": true, "vendor": true, "testdata": true, ".git": true, ".jj": true, "dist": true, "build": true,
}

// GoFiles is every Go file under root but the tests, in lexical order, outside the directories no
// walk of the tree enters.
func GoFiles(root string) ([]string, error) {
	return files(root, func(name string) bool {
		return strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go")
	})
}

// SQLFiles is every .sql file under root, in lexical order, outside the directories no walk of
// the tree enters.
func SQLFiles(root string) ([]string, error) {
	return files(root, func(name string) bool { return strings.HasSuffix(name, ".sql") })
}

// files is every file under root whose name wanted takes, in lexical order, entering no directory
// skippedDirectories names.
func files(root string, wanted func(name string) bool) ([]string, error) {
	var found []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if skippedDirectories[entry.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if wanted(entry.Name()) {
			found = append(found, path)
		}
		return nil
	})
	return found, err
}

// Root is the repository's root: the directory holding go.work, found by climbing out of the
// directory the test runs in, its package's.
func Root(t testing.TB) string {
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

// Path is path from the repository's root, with forward slashes, as a failure names it.
func Path(t testing.TB, root, path string) string {
	t.Helper()
	relative, err := filepath.Rel(root, path)
	if err != nil {
		t.Fatalf("name %s under %s: %v", path, root, err)
	}
	return filepath.ToSlash(relative)
}

// StringLiteral answers a string literal and its text, and false for any other expression. A
// parsed literal always unquotes; its quoted form is its text if one ever did not.
func StringLiteral(expression ast.Expr) (*ast.BasicLit, string, bool) {
	literal, ok := expression.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return nil, "", false
	}
	text, err := strconv.Unquote(literal.Value)
	if err != nil {
		text = literal.Value
	}
	return literal, text, true
}

// LiteralLine is the line of the text at offset in literal, whose text is text: a raw literal
// spans lines, so a statement in it is named by the line its head is on; an interpreted literal
// is one line.
func LiteralLine(fset *token.FileSet, literal *ast.BasicLit, text string, offset int) int {
	line := fset.Position(literal.Pos()).Line
	if strings.HasPrefix(literal.Value, "`") {
		line += strings.Count(text[:offset], "\n")
	}
	return line
}

// Concatenation flattens the + chain at expression into its parts, left to right, calling inner,
// when it is not nil, with each + of the chain, expression's own included, so a caller that walks
// the file reads the chain once, from its outermost +. Any expression but a + is a chain of one
// part. A parenthesised + is a part of its own.
func Concatenation(expression ast.Expr, inner func(*ast.BinaryExpr)) []ast.Expr {
	binary, ok := expression.(*ast.BinaryExpr)
	if !ok || binary.Op != token.ADD {
		return []ast.Expr{expression}
	}
	if inner != nil {
		inner(binary)
	}
	return append(Concatenation(binary.X, inner), Concatenation(binary.Y, inner)...)
}

// PrintfVerb reads the directive of a format that starts at text[at], a '%': the verb that ends
// it, where the text after it starts, how many arguments a * of its width and precision take,
// and the argument an explicit [n] makes it take, -1 where it names none.
func PrintfVerb(text string, at int) (verb rune, end, stars, explicit int) {
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

// NextArgument is the argument, counted from 0 after the format, that a verb written at the end of
// format takes: each directive of format (PrintfVerb) takes one more for each * of its width and
// precision, an explicit [n] moves to the n-th, and its verb takes the one it is at, except %%,
// which takes none.
func NextArgument(format string) int {
	next := 0
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			continue
		}
		verb, end, stars, explicit := PrintfVerb(format, i)
		next += stars
		if explicit >= 0 {
			next = explicit
		}
		if verb != '%' {
			next++
		}
		i = end - 1
	}
	return next
}
