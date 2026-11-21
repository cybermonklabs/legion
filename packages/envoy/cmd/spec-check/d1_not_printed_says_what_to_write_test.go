package main

// Red test D1 of the acceptance of t-format at cc00ab616a7849364d8ee9651947f85655f1d903. Copy it into
// packages/envoy/cmd/spec-check/ and run:
//
//	go test -count=1 -run TestNotPrintedSaysWhatToWrite ./cmd/spec-check/
//
// The tree checker refuses a file that an editor or a tool changed without a person's intent: one
// node has one form. Then its line must say what to do. This test asks that the line names the text
// that print writes at the line it names, quoted as strconv.Quote quotes it with its line end, so an
// invisible change (a space at the end, a carriage return, a byte order mark) shows too.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestNotPrintedSaysWhatToWrite(t *testing.T) {
	approved := filepath.Join("..", "..", "internal", "dispatch", "specgraph", "testdata", "compare", "one-of-four", "approved.json")
	content, err := os.ReadFile(approved)
	if err != nil {
		t.Fatal(err)
	}
	var a struct {
		Nodes []struct {
			ID      string `json:"id"`
			Removed bool   `json:"removed"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(content, &a); err != nil {
		t.Fatal(err)
	}
	printed := filepath.Join(t.TempDir(), "spec")
	args := []string{"print", "--approved", approved, "--dir", printed}
	for _, n := range a.Nodes {
		if !n.Removed {
			args = append(args, "--id", n.ID)
		}
	}
	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("print: exit %d: %s", code, stderr.String())
	}
	const name = "acme-keeps-each-order.yaml"
	original, err := os.ReadFile(filepath.Join(printed, name))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(original), "\n")
	edits := []struct {
		name string
		edit func(string) string
	}{
		{"a space at the end of a line", func(s string) string { return strings.Replace(s, "kind: behavior\n", "kind: behavior \n", 1) }},
		{"CRLF line ends", func(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }},
		{"a byte order mark", func(s string) string { return "\ufeff" + s }},
		{"another quote style", func(s string) string { return strings.Replace(s, "kind: behavior\n", "kind: 'behavior'\n", 1) }},
		{"a reordered key", func(s string) string {
			return strings.Replace(s, "id: acme-keeps-each-order\nkind: behavior\n", "kind: behavior\nid: acme-keeps-each-order\n", 1)
		}},
		{"a comment", func(s string) string {
			return strings.Replace(s, "kind: behavior\n", "kind: behavior\n# the customer asked for this\n", 1)
		}},
		{"a tab", func(s string) string { return strings.Replace(s, "kind: behavior\n", "kind:\tbehavior\n", 1) }},
		{"a final line with no line break", func(s string) string { return strings.TrimSuffix(s, "\n") }},
	}
	at := regexp.MustCompile(regexp.QuoteMeta(name) + `:([0-9]+): `)
	for _, e := range edits {
		t.Run(e.name, func(t *testing.T) {
			edited := e.edit(string(original))
			if edited == string(original) {
				t.Fatal("the edit changed nothing")
			}
			dir := t.TempDir()
			entries, err := os.ReadDir(printed)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				body, err := os.ReadFile(filepath.Join(printed, entry.Name()))
				if err != nil {
					t.Fatal(err)
				}
				if entry.Name() == name {
					body = []byte(edited)
				}
				if err := os.WriteFile(filepath.Join(dir, entry.Name()), body, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			var stdout, stderr bytes.Buffer
			if code := run([]string{"tree", "--dir", dir}, &stdout, &stderr); code != 1 {
				t.Fatalf("tree: exit %d, want 1 (refused): %s", code, stderr.String())
			}
			var refusal string
			for _, line := range strings.Split(stderr.String(), "\n") {
				if at.MatchString(line) {
					refusal = line
					break
				}
			}
			if refusal == "" {
				t.Fatalf("no line names %s and a line: %s", name, stderr.String())
			}
			n, _ := strconv.Atoi(at.FindStringSubmatch(refusal)[1])
			if n < 1 || n > len(lines) {
				t.Fatalf("the line %q names line %d, and print wrote %d lines", refusal, n, len(lines))
			}
			if want := strconv.Quote(lines[n-1]); !strings.Contains(refusal, want) {
				t.Errorf("the refusal %q does not say what to write: the text print writes at line %d, %s", refusal, n, want)
			}
		})
	}
}
