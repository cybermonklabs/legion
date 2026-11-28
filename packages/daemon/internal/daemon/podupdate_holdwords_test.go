package daemon

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// Every source whose suspension holds (supervise.SuspendSource.Holds) has exactly one row of
// holdWords, and no other source has one. A holding source with no row would drop its claims out
// of the late-round notice (holdSentences) and the log line (holdNames) while those claims kept the
// round past its bound, and a second row would name them twice. The sources are read from
// supervise's own source rather than listed here, so a third holding source declared there and
// given no row fails this test instead of passing it.
//
// The negative control is holdWords without its controller row: the controller's source then
// holds with no row.
func TestEverySuspendSourceThatHoldsHasOneHoldWordsRow(t *testing.T) {
	t.Parallel()
	declared := declaredSuspendSources(t)
	holding := 0
	for _, by := range declared {
		rows := 0
		for _, row := range holdWords {
			if row.by == by {
				rows++
			}
		}
		switch {
		case by.Holds() && rows != 1:
			t.Errorf("source %q holds, and holdWords has %d rows of it, want one", by, rows)
		case !by.Holds() && rows != 0:
			t.Errorf("source %q does not hold, and holdWords has %d rows of it, want none", by, rows)
		}
		if by.Holds() {
			holding++
		}
	}
	if holding == 0 {
		t.Fatalf("no source of %v holds, want the operator's and the controller's", declared)
	}
	for _, row := range holdWords {
		if !slices.Contains(declared, row.by) {
			t.Errorf("holdWords has a row of %q, which supervise declares no SuspendSource for", row.by)
		}
	}
}

// declaredSuspendSources is the value of every SuspendSource constant supervise's own source
// declares: a constant typed SuspendSource, or one whose value converts a string to it. A value of
// any other shape fails the test, so no source is skipped for how it is spelled.
func declaredSuspendSources(t *testing.T) []supervise.SuspendSource {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "supervise", "*.go"))
	if err != nil {
		t.Fatalf("list internal/supervise: %v", err)
	}
	var sources []supervise.SuspendSource
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, decl := range parsed.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				value := spec.(*ast.ValueSpec)
				typed := isSuspendSource(value.Type)
				for i, expr := range value.Values {
					isSource := typed
					if call, ok := expr.(*ast.CallExpr); ok && isSuspendSource(call.Fun) && len(call.Args) == 1 {
						expr, isSource = call.Args[0], true
					}
					if !isSource {
						continue
					}
					literal, ok := expr.(*ast.BasicLit)
					if !ok || literal.Kind != token.STRING {
						t.Fatalf("%s declares the SuspendSource %s as no string literal", file, value.Names[i].Name)
					}
					text, err := strconv.Unquote(literal.Value)
					if err != nil {
						t.Fatalf("unquote the SuspendSource %s of %s: %v", value.Names[i].Name, file, err)
					}
					sources = append(sources, supervise.SuspendSource(text))
				}
			}
		}
	}
	if len(sources) == 0 {
		t.Fatal("read no SuspendSource from internal/supervise's source")
	}
	return sources
}

// isSuspendSource is whether expr names the type SuspendSource.
func isSuspendSource(expr ast.Expr) bool {
	ident, ok := expr.(*ast.Ident)
	return ok && ident.Name == "SuspendSource"
}
