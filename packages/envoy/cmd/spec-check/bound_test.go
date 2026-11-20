package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/architecture/notes"
)

// The bound refuses a directory that holds a spec file of notes.MaxFileSize + 1 bytes before any
// file is parsed, as the importer refuses the set: the file beside it opens a block that it never
// closes, and no subcommand names that problem. tree prints the bound's one refusal and a count
// line of no block, exit 1; changes and checks print the refusal alone, exit 2.
func TestTheBoundRefusesTheTreeBeforeAnyFileIsParsed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	big := filepath.Join(dir, "big.md")
	writeFile(t, big, bytes.Repeat([]byte("a"), notes.MaxFileSize+1))
	writeFile(t, filepath.Join(dir, "unclosed.md"), []byte(":::requirement{#a-claim-survives-a-restart kind=\"behavior\"}\nLegion keeps each claim.\n"))
	refusal := "spec-check: " + notes.CheckSize(big, notes.MaxFileSize+1).Error() + "\n"
	for _, row := range []struct {
		args []string
		want result
	}{
		{[]string{"tree", "--dir", dir}, result{
			code:   1,
			stdout: "spec-check: " + dir + ": 0 statements, 0 checks, 0 components, 0 interfaces, 0 terms; the tree is refused\n",
			stderr: "spec-check: " + dir + noList + refusal,
		}},
		{[]string{"changes", "--base", t.TempDir(), "--dir", dir}, result{code: 2, stderr: refusal}},
		{[]string{"checks", "--dir", dir, "--job", "go-checks"}, result{code: 2, stderr: refusal}},
	} {
		var stdout, stderr strings.Builder
		got := result{code: run(row.args, &stdout, &stderr)}
		got.stdout, got.stderr = stdout.String(), stderr.String()
		if got != row.want {
			t.Errorf("%q: exit %d, stdout %q, stderr %q\nwant exit %d, stdout %q, stderr %q",
				row.args, got.code, got.stdout, got.stderr, row.want.code, row.want.stdout, row.want.stderr)
		}
	}
}
