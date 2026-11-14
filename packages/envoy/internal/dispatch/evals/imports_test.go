package evals

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fetchers are the import paths through which Go code reaches another server: the standard
// library's HTTP, RPC and mail clients, and this module's own clients of other services. The
// registry stores a link and never fetches, mirrors or checks its far side (R9), so its package
// imports none of them, not even net/http for a type.
var fetchers = []string{
	"net/http",
	"net/rpc",
	"net/smtp",
	"golang.org/x/net",
	"github.com/gorilla/websocket",
	"google.golang.org/grpc",
	"github.com/sjawhar/envoy/internal/dispatch/envoy",
	"github.com/sjawhar/envoy/internal/dispatch/githubapi",
	"github.com/sjawhar/envoy/internal/dispatch/githubapp",
	"github.com/sjawhar/envoy/internal/dispatch/embed",
	"github.com/sjawhar/envoy/internal/dispatch/agentsecrets",
}

// TestThePackageImportsNoClientThatFetches reads the import list of every file of the package
// that is not a test and fails on any path in fetchers or below one.
func TestThePackageImportsNoClientThatFetches(t *testing.T) {
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list the package's files: %v", err)
	}
	files, imports := 0, 0
	for _, source := range sources {
		if strings.HasSuffix(source, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), source, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", source, err)
		}
		files++
		for _, spec := range parsed.Imports {
			imports++
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("%s imports %s, which is no quoted path: %v", source, spec.Path.Value, err)
			}
			for _, fetcher := range fetchers {
				if path == fetcher || strings.HasPrefix(path, fetcher+"/") {
					t.Errorf("%s imports %s, a client that reaches another server", source, path)
				}
			}
		}
	}
	t.Logf("read %d imports of %d files", imports, files)
	if files == 0 || imports == 0 {
		t.Fatalf("read %d imports of %d files, and a check that read none proves nothing", imports, files)
	}
}
