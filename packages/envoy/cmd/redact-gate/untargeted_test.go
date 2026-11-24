package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// answerRead is a production file's raw read of an answer's body, which the census reads only
// type-checked: in a file no build compiles, it is UNKNOWN.
const answerRead = `

import (
	"io"
	"net/http"
)

func read(resp *http.Response) ([]byte, error) { return io.ReadAll(resp.Body) }
`

// writeTree writes files, each relative to root.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, text := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestTheGoCensusReadsAFileInADirectoryNoBuildLists: go list names no package, and no error, for
// a directory whose every file the build excludes, so the census must find such a file by walking
// the module: a //go:build ignore file and a Windows-only one that read an answer's body are
// UNKNOWN, and an ignored file that reads none is listed as untargeted. A file go's ./... pattern
// does not walk (testdata, a name starting with "." or "_", a nested module) is not read.
func TestTheGoCensusReadsAFileInADirectoryNoBuildLists(t *testing.T) {
	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "")
	root := t.TempDir()
	script, err := os.ReadFile(filepath.Join("..", "..", "..", "..", ".github", "scripts", "go_build_tags.py"))
	if err != nil {
		t.Fatal(err)
	}
	writeTree(t, root, map[string]string{
		".github/scripts/go_build_tags.py": string(script),
		"m/go.mod":                         "module example.com/m\n\ngo 1.27\n",
		"m/a/a.go":                         "package a\n\n// A is a package every build lists.\nfunc A() int { return 1 }\n",
		"m/a/a_test.go":                    "package a" + answerRead,
		"m/a/_draft.go":                    "package a" + answerRead,
		"m/a/.draft.go":                    "package a" + answerRead,
		"m/b/b.go":                         "//go:build ignore\n\npackage main" + answerRead,
		"m/c/c_windows.go":                 "//go:build windows\n\npackage c" + answerRead,
		"m/d/d.go":                         "//go:build ignore\n\npackage main\n\nimport \"crypto/tls\"\n\nvar _ = tls.VersionTLS13\n",
		"m/testdata/t.go":                  "package t" + answerRead,
		"m/_under/u.go":                    "package u" + answerRead,
		"m/.hidden/h.go":                   "package h" + answerRead,
		"m/nested/go.mod":                  "module example.com/nested\n\ngo 1.27\n",
		"m/nested/n.go":                    "//go:build ignore\n\npackage n" + answerRead,
	})
	g := &gate{}
	var c census
	g.goModuleCensus(&c, root, "m", nil, nil, nil)
	// Control: the module was listed and its one built package read, so a file left out below is
	// left out by the census, not by a list that failed.
	if len(c.Targets) == 0 || !strings.Contains(strings.Join(c.Targets, "\n"), "1 packages") {
		t.Fatalf("control: the census listed no build of the module's package a: targets %q, unknown %q", c.Targets, c.Unknown)
	}
	unknown := strings.Join(c.Unknown, "\n")
	for _, want := range []string{
		"m/b/b.go: no CI build compiles it, and it imports net/http, selects .Body",
		"m/c/c_windows.go: no CI build compiles it, and it imports net/http, selects .Body",
	} {
		if !strings.Contains(unknown, want) {
			t.Errorf("the census does not name %q as UNKNOWN; its UNKNOWN lines:\n%s", want, unknown)
		}
	}
	if !slices.Contains(c.Untargeted, "m/d/d.go") {
		t.Errorf("the census does not list m/d/d.go, which no build compiles, as untargeted: %q", c.Untargeted)
	}
	for _, skipped := range []string{"a/a.go", "a_test.go", "_draft.go", ".draft.go", "testdata/", "_under/", ".hidden/", "nested/", "did not find"} {
		if strings.Contains(unknown, skipped) || slices.ContainsFunc(c.Untargeted, func(f string) bool { return strings.Contains(f, skipped) }) {
			t.Errorf("the census reads %q, which no build of ./... names or which a build compiles: unknown %q, untargeted %q", skipped, c.Unknown, c.Untargeted)
		}
	}
}
