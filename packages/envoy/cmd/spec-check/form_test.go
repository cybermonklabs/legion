package main

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/architecture/notes"
	"github.com/sjawhar/envoy/internal/dispatch/specgraph"
)

// testForm runs the built command over the YAML form of the spec files: tree refuses each file
// that is not exactly as print writes its nodes, a file that print wrote reads back, and print keeps
// each file within the bound of one file as it fills a directory.
func testForm(t *testing.T, binary string) {
	valid := fixture(t, "trees", "valid")
	const term = "---\ntype: term\nid: fault\nname: fault term\ndefinition:\n  - A fault term is a word of a fault.\n"
	// v-yaml-tree: five files, one at a time, each beside the valid tree and each with one fault
	// that the YAML library reads past: an alias, a tag and a comment read as the same data as the
	// valid term, so only the printed form refuses them; yaml.v3 refuses a key written two times;
	// and the schema refuses a key that a term does not take.
	for _, row := range []struct {
		name, text, line, counts string
	}{
		{"an alias", strings.Replace(term, "name: fault term\ndefinition:\n  - A fault term is a word of a fault.\n", "name: &n fault term\ndefinition:\n  - *n\n", 1),
			"fault.yaml:4: fault: " + notPrinted(4, "name: &n fault term\n", "name: fault term\n", "another spelling"), "2 terms"},
		{"a tag", strings.Replace(term, "name: fault term", "name: !!str fault term", 1),
			"fault.yaml:4: fault: " + notPrinted(4, "name: !!str fault term\n", "name: fault term\n", "another spelling"), "2 terms"},
		{"a comment", strings.Replace(term, "---\n", "---\n# The term of a fault.\n", 1),
			"fault.yaml:2: fault: " + notPrinted(2, "# The term of a fault.\n", "type: term\n", "a comment"), "2 terms"},
		{"a key written two times", strings.Replace(term, "name: fault term\n", "name: fault term\nname: fault term\n", 1),
			`fault.yaml:5: -: bad-yaml: line 5: mapping key "name" already defined at line 4`, "1 term"},
		{"a key that a term does not take", term + "surprise: true\n",
			"fault.yaml:1: fault: schema: node: additional properties 'surprise' not allowed", "2 terms"},
	} {
		t.Run("tree refuses a file with "+row.name, func(t *testing.T) {
			t.Parallel()
			dir := copyOf(t, valid)
			writeFile(t, filepath.Join(dir, "fault.yaml"), []byte(row.text))
			expect(t, runBinary(t, binary, "", "tree", "--dir", dir), result{
				code:   1,
				stdout: "spec-check: " + dir + ": 3 statements, 4 checks, 2 components, 1 interface, " + row.counts + "; the tree is refused\n",
				stderr: "spec-check: " + dir + noList + "spec-check: " + filepath.Join(dir, row.line) + "\n",
			})
		})
	}

	// v-printed-form: print writes the term of testdata/nodes/quoted.yaml, whose lines YAML must
	// quote, and tree reads the file it wrote. The same file with one change, one at a time, reads
	// as the same data in the YAML library, and the printed form refuses it.
	t.Run("tree reads a file that print wrote, and refuses it with one change", func(t *testing.T) {
		t.Parallel()
		quoted, err := os.ReadFile(fixture(t, "nodes", "quoted.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		approved := writeApproved(t, map[string]string{"quoted": string(quoted)})
		dir := t.TempDir()
		terms := filepath.Join(dir, "terms.yaml")
		expect(t, runBinary(t, binary, "", "print", "--approved", approved, "--id", "quoted", "--dir", dir), result{stdout: "spec-check: wrote quoted to " + terms + ":1\n"})
		if got, err := os.ReadFile(terms); err != nil || string(got) != string(quoted) {
			t.Fatalf("print wrote %q, %v; want the text of quoted.yaml", got, err)
		}
		counts := ": 0 statements, 0 checks, 0 components, 0 interfaces, 1 term; the tree "
		expect(t, runBinary(t, binary, "", "tree", "--dir", dir), result{stdout: "spec-check: " + dir + counts + "is valid\n", stderr: "spec-check: " + dir + noList})
		for _, change := range []struct {
			name, text      string
			line            int
			got, difference string
		}{
			{"a space at the end of a line", strings.Replace(string(quoted), "name: quoted term\n", "name: quoted term \n", 1), 4, "name: quoted term \n", "a space at the end of the line"},
			{"quotes around a plain value", strings.Replace(string(quoted), "name: quoted term\n", "name: \"quoted term\"\n", 1), 4, "name: \"quoted term\"\n", "other quotes"},
			{"a carriage return", strings.Replace(string(quoted), "type: term\n", "type: term\r\n", 1), 2, "type: term\r\n", "a carriage return: print ends each line with a line feed only"},
			{"a byte order mark", "\ufeff" + string(quoted), 1, "\ufeff---\n", "a byte order mark"},
		} {
			changed := t.TempDir()
			writeFile(t, filepath.Join(changed, "terms.yaml"), []byte(change.text))
			want := strings.SplitAfter(string(quoted), "\n")[change.line-1]
			expect(t, runBinary(t, binary, "", "tree", "--dir", changed), result{
				code:   1,
				stdout: "spec-check: " + changed + counts + "is refused\n",
				stderr: "spec-check: " + changed + noList + "spec-check: " + filepath.Join(changed, "terms.yaml") + ":" + fmt.Sprint(change.line) + ": quoted: " +
					notPrinted(change.line, change.got, want, change.difference) + "\n",
			})
		}
	})

	// v-print-reads-back, the size control: print fills a directory with 40 terms of about 30 KB,
	// one call each in a random order. After each call each file is within the bound of one file and
	// tree takes the directory, and the terms that do not fit go on to terms-2.yaml.
	t.Run("print keeps each file within the bound as it fills a directory", func(t *testing.T) {
		t.Parallel()
		documents := map[string]string{}
		var ids []string
		line := strings.TrimSpace(strings.Repeat("a term is a word of a node ", 4))[:99]
		for i := range 40 {
			id := fmt.Sprintf("large-term-%c%c", 'a'+i/26, 'a'+i%26)
			var b strings.Builder
			b.WriteString("---\ntype: term\nid: " + id + "\nname: large term " + id[len("large-term-"):] + "\ndefinition:\n")
			for range 300 {
				b.WriteString("  - " + line + "\n")
			}
			documents[id] = b.String()
			ids = append(ids, id)
		}
		approved := writeApproved(t, documents)
		r := rand.New(rand.NewPCG(1, 1))
		r.Shuffle(len(ids), func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })
		dir := t.TempDir()
		for _, id := range ids {
			if got := runBinary(t, binary, "", "print", "--approved", approved, "--id", id, "--dir", dir); got.code != 0 {
				t.Fatalf("print %s: exit %d, stderr %q", id, got.code, got.stderr)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				info, err := entry.Info()
				if err != nil {
					t.Fatal(err)
				}
				if info.Size() > notes.MaxFileSize {
					t.Fatalf("after print %s, %s is %d bytes (limit %d)", id, entry.Name(), info.Size(), notes.MaxFileSize)
				}
			}
			if got := runBinary(t, binary, "", "tree", "--dir", dir); got.code != 0 {
				t.Fatalf("tree after print %s: exit %d, stderr %q", id, got.code, got.stderr)
			}
		}
		if _, err := os.Stat(filepath.Join(dir, "terms-2.yaml")); err != nil {
			t.Errorf("print wrote no terms-2.yaml: %v", err)
		}
	})
}

// writeApproved writes an approved.json (specgraph.ApprovedFile) of one version of each node, its
// document, and answers its path.
func writeApproved(t *testing.T, documents map[string]string) string {
	t.Helper()
	file := specgraph.ApprovedFile{Nodes: []specgraph.ApprovedNode{}}
	for id, document := range documents {
		file.Nodes = append(file.Nodes, specgraph.ApprovedNode{ID: id, Removed: new(false), PartlyChecked: new(false),
			Versions: []specgraph.ApprovedVersion{{Version: 1, At: time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC), Document: document}}})
	}
	path := filepath.Join(t.TempDir(), "approved.json")
	writeJSONFile(t, path, file)
	return path
}

// notPrinted is the detail of the not-printed problem at line n, whose text is got in the file and
// want in the documents that print writes, and which differs in the kind that difference names.
func notPrinted(n int, got, want, difference string) string {
	return fmt.Sprintf("not-printed: line %d is %q, and print writes %q there (%s); spec-check print writes each node in that form", n, got, want, difference)
}
