// Package testgolden holds the one golden-file check the daemon's tests share: Go writes a value's
// indented JSON to a file with the calling package's `-update` flag, and a test fails where the
// file differs from what Go writes now, naming the first line that differs and the command that
// writes the file again. The comparison is byte for byte: nothing is normalised, so a fixture
// another language's test reads is exactly what Go wrote.
package testgolden

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Check pins value at path: with update it writes the value there, and otherwise it fails t where
// the file differs from the value, naming the first line that differs. rewrite is the command that
// writes the file again, which the failure names.
func Check(t testing.TB, path string, value any, update bool, rewrite string) {
	t.Helper()
	encoded := Encode(t, path, value)
	if update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create the directory of %s: %v", path, err)
		}
		if err := os.WriteFile(path, encoded, 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		return
	}
	held, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s (run `%s`): %v", path, rewrite, err)
	}
	if !bytes.Equal(encoded, held) {
		line, file, written := FirstDifference(held, encoded)
		t.Fatalf("%s is stale (run `%s`): line %d of the file holds %q, and Go writes %q", path, rewrite, line, file, written)
	}
}

// Encode is value as Check writes it: indented by two spaces and ending in a newline.
func Encode(t testing.TB, name string, value any) []byte {
	t.Helper()
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("marshal %s: %v", name, err)
	}
	return append(encoded, '\n')
}

// FirstDifference is the first line, counted from 1, at which held and written differ, with that
// line of each ("" past the end of one of them).
func FirstDifference(held, written []byte) (int, string, string) {
	heldLines, writtenLines := bytes.Split(held, []byte("\n")), bytes.Split(written, []byte("\n"))
	for i := 0; ; i++ {
		var h, w []byte
		if i < len(heldLines) {
			h = heldLines[i]
		}
		if i < len(writtenLines) {
			w = writtenLines[i]
		}
		if !bytes.Equal(h, w) || i >= len(heldLines) || i >= len(writtenLines) {
			return i + 1, string(h), string(w)
		}
	}
}
