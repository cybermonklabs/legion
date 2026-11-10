package harness

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
)

// RepoMarker is the file whose presence names the Legion repository's root. The
// experiments that quote Legion's own code read it from there, so they need the root
// rather than the scratch directory they write to.
const RepoMarker = "packages/daemon/internal/workflow/table.go"

// RepoRoot is the root of the Legion repository: the -repo-root the command line gave,
// or the first directory at or above the working directory that holds RepoMarker. It
// returns an error rather than a guess, because an experiment that quotes the wrong
// file measures the wrong thing.
func RepoRoot(configured string) (string, error) {
	if configured != "" {
		if _, err := os.Stat(filepath.Join(configured, RepoMarker)); err != nil {
			return "", fmt.Errorf("-repo-root %q holds no %s: %w", configured, RepoMarker, err)
		}
		return configured, nil
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("working directory: %w", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, RepoMarker)); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no directory at or above the working directory holds %s; pass -repo-root", RepoMarker)
		}
		dir = parent
	}
}

// FileLines is how many lines of text a file holds, counting a last line that ends
// without a newline.
func FileLines(path string) (int, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}
	lines := bytes.Count(content, []byte("\n"))
	if len(content) > 0 && !bytes.HasSuffix(content, []byte("\n")) {
		lines++
	}
	return lines, nil
}

// FuncLines is the source file a function was compiled from and how many lines its
// declaration takes there, its signature and closing brace included. It reads the
// source the running binary was built from, so an experiment that reports "the workflow
// took N lines of Go" reports the function it actually ran, not a number typed by hand.
func FuncLines(fn any) (string, int, error) {
	value := reflect.ValueOf(fn)
	if value.Kind() != reflect.Func {
		return "", 0, fmt.Errorf("FuncLines wants a function, got %T", fn)
	}
	entry := runtime.FuncForPC(value.Pointer())
	if entry == nil {
		return "", 0, fmt.Errorf("no compiled function behind %T", fn)
	}
	file, line := entry.FileLine(value.Pointer())
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
	if err != nil {
		return "", 0, fmt.Errorf("parse %s: %w", file, err)
	}
	for _, decl := range parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		from, to := fset.Position(fn.Pos()).Line, fset.Position(fn.End()).Line
		if line >= from && line <= to {
			return file, to - from + 1, nil
		}
	}
	return "", 0, fmt.Errorf("no function declaration around %s:%d", file, line)
}
