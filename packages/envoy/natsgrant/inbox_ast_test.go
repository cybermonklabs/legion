package natsgrant_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// natsImport is nats.go's import path, whose package function NewInbox always makes an _INBOX.
// subject, whatever reply prefix the connection set (nats.go v1.53.1 nats.go:4844-4851).
const natsImport = "github.com/nats-io/nats.go"

// packageNewInboxCalls walks the non-test Go files under roots, testdata and node_modules below a
// root left out, and returns each call of nats.go's package function NewInbox as "<path>:<line>",
// whatever name the file imports nats.go under; a dot import of nats.go is returned as
// "<path>:<line> import .", since its NewInbox cannot be told from a function of the file's own.
func packageNewInboxCalls(roots ...string) ([]string, error) {
	fileSet := token.NewFileSet()
	var calls []string
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if path != root && (entry.Name() == "testdata" || entry.Name() == "node_modules") {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(fileSet, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			names := map[string]bool{}
			for _, spec := range file.Imports {
				if importPath, _ := strconv.Unquote(spec.Path.Value); importPath != natsImport {
					continue
				}
				name := "nats"
				if spec.Name != nil {
					name = spec.Name.Name
				}
				if name == "." {
					calls = append(calls, fmt.Sprintf("%s:%d import .", filepath.ToSlash(path), fileSet.Position(spec.Pos()).Line))
				}
				names[name] = true
			}
			ast.Inspect(file, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || selector.Sel.Name != "NewInbox" {
					return true
				}
				if pkg, ok := selector.X.(*ast.Ident); ok && names[pkg.Name] {
					calls = append(calls, fmt.Sprintf("%s:%d", filepath.ToSlash(path), fileSet.Position(call.Pos()).Line))
				}
				return true
			})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return calls, nil
}

// No program of the envoy or daemon module makes an inbox with nats.go's package function
// NewInbox, which always answers an _INBOX. subject: a connection whose credential names a
// service sets its account's own reply prefix (natscred.InboxPrefix) and the auth service grants
// it no other inbox, so an inbox only the connection's own NewInbox makes is one it can subscribe.
// The control is a fixture that calls it under another import name, which the walk names.
func TestNoPackageNewInbox(t *testing.T) {
	t.Run("control: a fixture calling it", func(t *testing.T) {
		calls, err := packageNewInboxCalls(filepath.Join("testdata", "newinbox"))
		require.NoError(t, err)
		require.Equal(t, []string{"testdata/newinbox/fixture.go:9"}, calls)
	})
	calls, err := packageNewInboxCalls("..", filepath.Join("..", "..", "daemon"))
	require.NoError(t, err)
	require.Empty(t, calls, "nats.NewInbox makes an _INBOX. subject whatever the connection's prefix; use the connection's own NewInbox")
}
