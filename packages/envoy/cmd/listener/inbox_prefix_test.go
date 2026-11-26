package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/testgo"
)

// natsModule is the import path of nats.go, whose package function NewInbox names an inbox under
// the default prefix whatever the connection it is used on.
const natsModule = "github.com/nats-io/nats.go"

// envoyModule is this module's import path: the packages whose sources the walk below reads.
const envoyModule = "github.com/sjawhar/envoy"

// packageInboxCalls lists every call of nats.go's package function NewInbox in the non-test
// sources of every package of this module the listener's binary links, as file:line from
// packages/envoy. An edge listener's connections name their inboxes under a prefix of their own
// (natsgrant.EdgeInboxPrefix), and its grant subscribes that prefix alone, so an inbox the package
// function names is one the server refuses the listener a subscription to: every inbox is named by
// the connection it is used on (nats.Conn.NewInbox), which applies the connection's prefix.
func packageInboxCalls(t *testing.T) []string {
	t.Helper()
	list := exec.Command("go", "list", "-deps", "-f", `{{.ImportPath}}{{"\t"}}{{.Dir}}{{"\t"}}{{join .GoFiles " "}}`, ".")
	out, err := testgo.Output(list)
	if err != nil {
		t.Fatalf("go list -deps .: %v", err)
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	read := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 3 || (fields[0] != envoyModule && !strings.HasPrefix(fields[0], envoyModule+"/")) {
			continue
		}
		for _, name := range strings.Fields(fields[2]) {
			path := filepath.Join(fields[1], name)
			read++
			for _, at := range packageInboxCallsIn(t, path) {
				rel, err := filepath.Rel(root, path)
				if err != nil {
					rel = path
				}
				calls = append(calls, rel+":"+strconv.Itoa(at))
			}
		}
	}
	// A walk that read no source of this module found nothing because it looked at nothing.
	if read == 0 {
		t.Fatalf("go list -deps . named no source of %s:\n%s", envoyModule, out)
	}
	return calls
}

// packageInboxCallsIn is the line of every call of nats.go's NewInbox in the Go file at path,
// under whatever name the file imports nats.go.
func packageInboxCallsIn(t *testing.T, path string) []int {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	names := map[string]bool{}
	for _, spec := range file.Imports {
		if imported, _ := strconv.Unquote(spec.Path.Value); imported == natsModule {
			name := "nats"
			if spec.Name != nil {
				name = spec.Name.Name
			}
			names[name] = true
		}
	}
	if len(names) == 0 {
		return nil
	}
	var lines []int
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "NewInbox" {
			return true
		}
		if ident, ok := selector.X.(*ast.Ident); ok && names[ident.Name] {
			lines = append(lines, fset.Position(call.Pos()).Line)
		}
		return true
	})
	return lines
}

// No package the listener links names an inbox with nats.go's package function: each inbox is
// named by the connection it is used on, so an edge listener's inboxes carry its own prefix, the
// one its grant subscribes. The match finds a call however the file names nats.go, and a call of a
// connection's NewInbox is no finding.
func TestTheListenerNamesEveryInboxByItsConnection(t *testing.T) {
	t.Parallel()
	t.Run("the match", func(t *testing.T) {
		dir := t.TempDir()
		for source, want := range map[string]int{
			"package a\nimport \"github.com/nats-io/nats.go\"\nvar _ = nats.NewInbox()\n":                1,
			"package a\nimport natsgo \"github.com/nats-io/nats.go\"\nvar _ = natsgo.NewInbox()\n":       1,
			"package a\nimport \"github.com/nats-io/nats.go\"\nvar c *nats.Conn\nvar _ = c.NewInbox()\n": 0,
			"package a\nvar nats struct{ NewInbox func() string }\nvar _ = nats.NewInbox()\n":            0,
		} {
			path := filepath.Join(dir, "a.go")
			if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := len(packageInboxCallsIn(t, path)); got != want {
				t.Errorf("found %d calls in %q, want %d", got, source, want)
			}
		}
	})
	t.Run("the listener's packages", func(t *testing.T) {
		for _, call := range packageInboxCalls(t) {
			t.Errorf("%s names an inbox with nats.NewInbox, which ignores its connection's inbox prefix: use the connection's NewInbox", call)
		}
	})
}
