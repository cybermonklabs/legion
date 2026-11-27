package testgolden

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The first line that differs is named with the file's side and Go's, counted from 1, and a file
// one side ends before the other names the empty line past its end.
func TestFirstDifferenceNamesTheFirstLineThatDiffers(t *testing.T) {
	for _, tc := range []struct {
		name           string
		held, written  string
		line           int
		inFile, fromGo string
	}{
		{name: "a changed line", held: "{\n  \"a\": 1,\n  \"b\": 2\n}\n", written: "{\n  \"a\": 1,\n  \"b\": 3\n}\n", line: 3, inFile: `  "b": 2`, fromGo: `  "b": 3`},
		{name: "a line Go adds", held: "{\n}\n", written: "{\n  \"a\": 1\n}\n", line: 2, inFile: "}", fromGo: `  "a": 1`},
		{name: "the file ends first", held: "a\n", written: "a\nb\n", line: 2, inFile: "", fromGo: "b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line, inFile, fromGo := FirstDifference([]byte(tc.held), []byte(tc.written))
			if line != tc.line || inFile != tc.inFile || fromGo != tc.fromGo {
				t.Errorf("FirstDifference = %d %q %q, want %d %q %q", line, inFile, fromGo, tc.line, tc.inFile, tc.fromGo)
			}
		})
	}
}

// Check writes the value's indented JSON with a newline, makes the directory it needs, and then
// passes over the file it wrote.
func TestCheckWritesTheFileItThenPasses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "value.json")
	Check(t, path, map[string]int{"b": 2, "a": 1}, true, "go test -update")
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := "{\n  \"a\": 1,\n  \"b\": 2\n}\n"; string(written) != want {
		t.Fatalf("Check wrote %q, want %q", written, want)
	}
	Check(t, path, map[string]int{"a": 1, "b": 2}, false, "go test -update")
}

// failures is a testing.TB that records what Check reports instead of failing the test that runs
// it. Its Fatalf stops the call as the standard library's does (runtime.Goexit), so Check runs in
// a goroutine of its own (checked).
type failures struct {
	testing.TB
	reported []string
}

func (f *failures) Helper() {}

func (f *failures) Errorf(format string, args ...any) {
	f.reported = append(f.reported, fmt.Sprintf(format, args...))
}

func (f *failures) Fatalf(format string, args ...any) {
	f.Errorf(format, args...)
	runtime.Goexit()
}

// checked is what Check reports over path and value, read without update, once its goroutine has
// returned or stopped.
func checked(t *testing.T, path string, value any, rewrite string) []string {
	t.Helper()
	recorder := &failures{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		Check(recorder, path, value, false, rewrite)
	}()
	<-done
	return recorder.reported
}

// Check fails where the file differs from the value, naming the file, the first line that differs
// on each side and the command that writes the file again. Over a file that matches, it reports
// nothing, which shows the recorder can stay quiet.
func TestCheckFailsOnADifferenceNamingItsFirstLineAndTheRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "value.json")
	if err := os.WriteFile(path, []byte("{\n  \"a\": 1,\n  \"b\": 2\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const rewrite = "go test ./internal/example/ -update"
	if reported := checked(t, path, map[string]int{"a": 1, "b": 2}, rewrite); len(reported) != 0 {
		t.Fatalf("Check over the file that matches reported %q, want nothing", reported)
	}
	reported := checked(t, path, map[string]int{"a": 1, "b": 3}, rewrite)
	if len(reported) != 1 {
		t.Fatalf("Check over a file that differs reported %q, want one failure", reported)
	}
	for _, want := range []string{path, "line 3", `"  \"b\": 2"`, `"  \"b\": 3"`, "`" + rewrite + "`"} {
		if !strings.Contains(reported[0], want) {
			t.Errorf("Check reported %q, want it to name %s", reported[0], want)
		}
	}
}
