package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestARowWhoseTestDoesNotCompileIsNamedByTheCompilersError: go test -json gives a build's
// errors as build-output events, not as lines of their own, so a row whose test does not compile -
// a leak row's test copied onto a base that lacks a declaration it uses, as control 19 runs it -
// is named by the compiler's line read from those events, in the run's note and in the reason the
// gate prints. Before, both were empty ("the mutant does not compile: ").
func TestARowWhoseTestDoesNotCompileIsNamedByTheCompilersError(t *testing.T) {
	copyDir := t.TempDir()
	write := func(path, text string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	module := filepath.Join(copyDir, "packages", "envoy")
	write(filepath.Join(module, "go.mod"), "module example.com/probe\n\ngo 1.27\n")
	write(filepath.Join(module, "broken", "broken_test.go"),
		"package broken\n\nimport \"testing\"\n\nfunc TestUsesAMissingDeclaration(t *testing.T) { declaredNowhere() }\n")
	g := &gate{rowTimeout: time.Minute}
	r := &row{ID: "probe", Package: "./broken", Test: "TestUsesAMissingDeclaration", Capture: "probe"}
	res := g.runTest(copyDir, r)
	if !res.compileError {
		t.Fatalf("the run of a test that does not compile is not a compile error: %+v", res)
	}
	const want = "broken_test.go:5:"
	if !strings.Contains(res.note, want) || !strings.Contains(res.note, "undefined: declaredNowhere") {
		t.Fatalf("the run's note is %q, want the compiler's line (%s ... undefined: declaredNowhere)", res.note, want)
	}
	if why := res.evidence(r); !strings.Contains(why, "undefined: declaredNowhere") {
		t.Fatalf("the gate's reason is %q, want it to name the compiler's error", why)
	}
}
