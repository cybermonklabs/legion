//go:build !race

// An eighth static check beside the seven source guards, under the tag they carry and for the same
// saving: it reads text - the api package's parsed declarations, and the bytes of every non-test
// .go file of the module - and opens no database and starts no goroutine, so the race detector
// has nothing here to watch. Behind the tag the race build neither compiles it nor walks
// packages/envoy a second time to run it (lock_level_test.go's header weighs that walk).
//
// The tag is also what puts this check in the job its subject asks for. R15 of
// dispatch://INFRA-1066's spec has a test whose subject is a file's words run in a text-check job
// with a filter of its own, never inside a product suite; this one holds one SQL rule to one
// spelling in the module's source, which is words. It arrived from main inside who_waits_test.go,
// a behaviour file of this package's Postgres-backed tests, as a subtest of
// TestOneExpressionDecidesWhoWaits - the one place a module-wide walk ran in the race build. Its
// job there was envoy-go-race-api, whose filter does not name packages/envoy/** and must not: an
// edit under packages/envoy/cmd or packages/envoy/infra reaches what this check reads while
// selecting no job that ran it, so the red landed on whatever pull request next touched that
// job's own closure, or on none. Here it is the text-check job's to run by name
// (ci_job_reads.py's TEXT_CHECK_TESTS, which also keeps it out of envoy-go's plain step), once
// per run of the router, under a filter that names the whole module - the read
// ci_job_reads.py's MODULE_SOURCES declares for this file.
//
// What it holds is one of the two tests the Go half of dispatch://INFRA-1576's
// c-one-expression-decides is written as: TestOneExpressionDecidesWhoWaitsInOneSpelling here,
// which holds that no read keeps a copy of the rule for which person an ask waits on, and
// TestOneExpressionDecidesWhoWaits in who_waits_test.go, which reads the three answers for the
// same ask and the same person. That check's own `run` names the shorter name and anchors
// nothing, so one `-run TestOneExpressionDecidesWhoWaits` matches both tests; that is why the
// test here is spelled as a longer form of that name. The `!race` tag above is what puts the two
// in different jobs: the race build does not compile this file, so the text-check job runs this
// test by name while the behaviour test runs with the package's Postgres-backed tests.

package api

import (
	"fmt"
	"go/ast"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// askedOfPersonExpressionName is the identifier the three reads must name instead of spelling
// the rule again.
const askedOfPersonExpressionName = "askedOfPersonExpression"

// namesTheExpression reports whether a declaration or a function body mentions that identifier.
func namesTheExpression(node ast.Node) bool {
	named := false
	ast.Inspect(node, func(inner ast.Node) bool {
		if identifier, ok := inner.(*ast.Ident); ok && identifier.Name == askedOfPersonExpressionName {
			named = true
		}
		return !named
	})
	return named
}

// TestOneExpressionDecidesWhoWaitsInOneSpelling holds the inbox read, the author's open-asks
// read and the replies read to the one constant: each of the three names it, and the SQL it
// holds is spelled once in the module. The precedent for reading the source rather than the
// behaviour is api/lock_level_test.go.
func TestOneExpressionDecidesWhoWaitsInOneSpelling(t *testing.T) {
	sources := moduleSources(t)
	declarations := map[string]ast.Node{}
	for _, source := range sources {
		if filepath.Base(filepath.Dir(source.path)) != "api" {
			continue
		}
		for _, declared := range source.file.Decls {
			switch node := declared.(type) {
			case *ast.FuncDecl:
				declarations[node.Name.Name+"'s query"] = node
			case *ast.GenDecl:
				for _, spec := range node.Specs {
					value, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for _, name := range value.Names {
						declarations[name.Name] = value
					}
				}
			}
		}
	}
	for _, read := range []string{"askReadColumns", "listOpenAsks's query", "myRepliesOwner"} {
		node, declared := declarations[read]
		if !declared {
			t.Fatalf("%s is no longer declared in the api package; the reads this check holds have moved", read)
		}
		if !namesTheExpression(node) {
			t.Fatalf("%s does not name %s: a read with its own copy of the rule answers a different person",
				read, askedOfPersonExpressionName)
		}
	}

	copies := []string{}
	for _, source := range sources {
		text, err := os.ReadFile(source.path)
		if err != nil {
			t.Fatalf("read %s: %v", source.path, err)
		}
		if spelled := strings.Count(string(text), "coalesce(a.asked_of"); spelled > 0 {
			copies = append(copies, fmt.Sprintf("%s (%d)", filepath.ToSlash(source.path), spelled))
		}
	}
	slices.Sort(copies)
	if want := []string{"../../../internal/dispatch/api/ask_rows.go (1)"}; !slices.Equal(copies, want) {
		t.Fatalf("the rule is spelled in %v, want it written once, in %v", copies, want)
	}
}

// INFRA-1413 selection probe, never merged.
