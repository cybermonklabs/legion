// Package testgolden holds the one helper every daemon test that writes a daemon API fixture
// (packages/contracts/fixtures/daemon-api) pins its fixture with. The fixtures are the
// cross-language pin: Go writes each wire shape, and `packages/contracts/src/legion-api.test.ts`
// parses it through the zod schema its readers parse with and holds the parse equal to the file,
// so a Go field with no zod line is named there. Go is the source of truth; the fixture is how the
// schema is held to it. Each writing package keeps its own `-update` flag and names its own command
// for rewriting what it writes, which is all that differs between them.
package testgolden

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Check pins value, encoded as indented JSON with a final newline, to the daemon API fixture name.
// With update it writes the fixture; otherwise it fails t where the file differs from what Go now
// writes, naming the first line that differs and command, the run that rewrites the fixture. The
// fixture is the file at Path, which is relative to the directory a package's tests run in, so
// every caller runs in packages/daemon/internal/<package>.
func Check(t testing.TB, name string, value any, update bool, command string) {
	t.Helper()
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("marshal %s: %v", name, err)
	}
	encoded = append(encoded, '\n')
	path := Path(name)
	if update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create fixture directory: %v", err)
		}
		if err := os.WriteFile(path, encoded, 0o644); err != nil {
			t.Fatalf("write golden %s: %v", name, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (run `%s`): %v", name, command, err)
	}
	if !bytes.Equal(encoded, want) {
		t.Fatalf("%s is stale, first at %s\n(run `%s`)", name, firstDifferentLine(encoded, want), command)
	}
}

// firstDifferentLine names the first line at which got and want differ, with both versions of it.
func firstDifferentLine(got, want []byte) string {
	gotLines, wantLines := bytes.Split(got, []byte("\n")), bytes.Split(want, []byte("\n"))
	for i := range max(len(gotLines), len(wantLines)) {
		var gotLine, wantLine []byte
		if i < len(gotLines) {
			gotLine = gotLines[i]
		}
		if i < len(wantLines) {
			wantLine = wantLines[i]
		}
		if !bytes.Equal(gotLine, wantLine) {
			return fmt.Sprintf("line %d:\n got: %s\nwant: %s", i+1, gotLine, wantLine)
		}
	}
	return "no line"
}

// Path is the path of the daemon API fixture name, relative to the directory a package's tests run
// in, which for every caller is packages/daemon/internal/<package>. A test that reads a fixture
// it does not pin whole with Check reads it here.
func Path(name string) string {
	return filepath.Join("..", "..", "..", "contracts", "fixtures", "daemon-api", name)
}
