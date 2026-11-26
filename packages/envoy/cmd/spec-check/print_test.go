package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/architecture/notes"
	"github.com/sjawhar/envoy/internal/dispatch/specgraph"
	"gopkg.in/yaml.v3"
)

// testPrint runs print over the fixtures of testdata/compare: it writes the current approved
// document of each node it is named, and of each check of a statement it is named, into the files
// of a directory, byte for byte as the files of the fixture hold them. A print of each current
// node passes the comparison, and the same files with one word more fail it on that statement
// (v-two-specs: "A change copies the text that Dispatch prints for an approved statement and its
// checks, and it passes the comparison. The same text with one word changed fails it").
func testPrint(t *testing.T, binary string) {
	t.Run("print writes a statement and its two checks as the files hold them", func(t *testing.T) {
		t.Parallel()
		approved := fixture(t, "compare", "two-checks", "approved.json")
		fixed, err := os.ReadFile(fixture(t, "compare", "two-checks", "spec", "orders.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		top := splitDocuments(string(fixed))[0]
		writeFile(t, filepath.Join(dir, "acme-keeps-each-order.yaml"), []byte(top))
		file := filepath.Join(dir, "acme-keeps-each-order.yaml")
		expect(t, runBinary(t, binary, "", "print", "--approved", approved, "--id", "an-order-survives-a-restart", "--dir", dir), result{
			stdout: "spec-check: wrote an-order-survives-a-restart to " + file + ":7\n" +
				"spec-check: wrote a-crash-keeps-each-order to " + file + ":14\n" +
				"spec-check: wrote a-restart-keeps-each-order to " + file + ":30\n",
		})
		printed, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.HasPrefix(fixed, printed) || len(printed) <= len(top) {
			t.Errorf("the files of two-checks do not start with the printed text %q", printed)
		}
	})
	t.Run("print writes components, an interface and a term, each node once", func(t *testing.T) {
		t.Parallel()
		approved := fixture(t, "compare", "approved-text", "approved.json")
		dir := t.TempDir()
		components, terms := filepath.Join(dir, "components.yaml"), filepath.Join(dir, "terms.yaml")
		expect(t, runBinary(t, binary, "", "print", "--approved", approved, "--id", "orders-to-billing", "--id", "order", "--id", "billing", "--id", "orders", "--id", "order", "--dir", dir), result{
			stdout: "spec-check: wrote billing to " + components + ":1\n" +
				"spec-check: wrote orders to " + components + ":8\n" +
				"spec-check: wrote orders-to-billing to " + components + ":15\n" +
				"spec-check: wrote order to " + terms + ":1\n",
		})
		fixed, err := os.ReadFile(fixture(t, "compare", "approved-text", "spec", "components.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		documents := splitDocuments(string(fixed))
		want := map[string]string{"components.yaml": strings.Join(documents[:3], ""), "terms.yaml": documents[4]}
		if got := readTexts(t, dir); !maps.Equal(got, want) {
			t.Errorf("print wrote %q, want %q", got, want)
		}
	})
	t.Run("print exits 2 on an id the approved spec does not hold or removed", func(t *testing.T) {
		t.Parallel()
		approved := fixture(t, "compare", "removed-kept", "approved.json")
		dir := t.TempDir()
		expect(t, runBinary(t, binary, "", "print", "--approved", approved, "--id", "acme-keeps-each-order", "--id", "an-order-is-paid-once", "--id", "each-order-gets-one-bill", "--dir", dir), result{
			code: 2,
			stderr: "spec-check: " + approved + `: the approved spec removed node "an-order-is-paid-once", so it has no current text` + "\n" +
				"spec-check: " + approved + `: the approved spec holds no node "each-order-gets-one-bill"` + "\n",
		})
		if got := readTexts(t, dir); len(got) != 0 {
			t.Errorf("print wrote %q, want nothing", got)
		}
	})

	entries, err := os.ReadDir(fixture(t, "compare"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		t.Run("a print of each current node of "+entry.Name()+" passes compare, and one word more fails it", func(t *testing.T) {
			t.Parallel()
			approved := fixture(t, "compare", entry.Name(), "approved.json")
			a := readApprovedFile(t, approved)
			current, _ := a.Current()
			args := []string{"print", "--approved", approved}
			for _, id := range slices.Sorted(maps.Keys(a.Nodes)) {
				if !a.Nodes[id].Removed {
					args = append(args, "--id", id)
				}
			}
			dir := t.TempDir()
			if printed := runBinary(t, binary, "", append(args, "--dir", dir)...); printed.code != 0 || printed.stderr != "" {
				t.Fatalf("print: exit %d, stderr %q", printed.code, printed.stderr)
			}
			n := strconv.Itoa(len(current.Statements))
			expect(t, runBinary(t, binary, "", "compare", "--dir", dir, "--approved", approved), result{stdout: passLine(dir, n+" of "+n)})

			tree, problems := specgraph.Parse(readDir(t, dir))
			if len(problems) != 0 || len(tree.Statements) == 0 {
				t.Fatalf("the printed files: %d statements, problems %+v", len(tree.Statements), problems)
			}
			planted := tree.Statements[0]
			path := filepath.Join(dir, planted.Pos.File)
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			line := "\n  - " + strings.Split(planted.Text, "\n")[0] + "\n"
			if !strings.Contains(string(content), line) {
				t.Fatalf("%s holds no line %q", path, line)
			}
			writeFile(t, path, []byte(strings.Replace(string(content), line, "\n  - planted "+line[len("\n  - "):], 1)))
			expect(t, runBinary(t, binary, "", "compare", "--dir", dir, "--approved", approved), result{
				code:   1,
				stdout: "spec-check: " + dir + ": the comparison fails on 1 node\n",
				stderr: "spec-check: " + path + ":" + strconv.Itoa(planted.Pos.Line) + ": " + planted.ID + ": " + unapprovedText + "\n",
			})
		})
	}
}

// R3: print writes each node into the files, and tree and compare accept the files of a print of
// each current node (v-print-reads-back). The first control prints the statements only, with their
// checks: the interface that a structure statement is about is not in the files, so print writes
// nothing, and tree refuses the files that the run would leave with the same problem. The second
// control changes one word of a statement in the files: compare refuses it, and tree still takes it.
func TestPrintedFilesPassTheComparison(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	approved := fixture(t, "compare", "one-word", "approved.json")
	a := readApprovedFile(t, approved)
	var all, statements []string
	for _, id := range slices.Sorted(maps.Keys(a.Nodes)) {
		if a.Nodes[id].Removed {
			continue
		}
		all = append(all, "--id", id)
		if strings.HasPrefix(a.Nodes[id].Versions[len(a.Nodes[id].Versions)-1].Document, "---\ntype: requirement\n") {
			statements = append(statements, id)
		}
	}
	if len(all) != 2*10 || len(statements) != 3 {
		t.Fatalf("one-word holds %d current nodes and %d statements, want 10 and 3", len(all)/2, len(statements))
	}

	dir := t.TempDir()
	if got := runBinary(t, binary, "", append([]string{"print", "--approved", approved, "--dir", dir}, all...)...); got.code != 0 || got.stderr != "" {
		t.Fatalf("print of each current node: exit %d, stderr %q", got.code, got.stderr)
	}
	expect(t, runBinary(t, binary, "", "tree", "--dir", dir), result{
		stdout: "spec-check: " + dir + ": 3 statements, 2 checks, 3 components, 1 interface, 1 term; the tree is valid\n",
		stderr: "spec-check: " + dir + noList,
	})
	expect(t, runBinary(t, binary, "", "compare", "--dir", dir, "--approved", approved), result{stdout: passLine(dir, "3 of 3")})

	// The first control: the statements only.
	only := t.TempDir()
	args := []string{"print", "--approved", approved, "--dir", only}
	for _, id := range statements {
		args = append(args, "--id", id)
	}
	missing := "orders-reach-billing-only-through-the-interface: missing-target: about names \"orders-to-billing\", which the files do not hold"
	got := runBinary(t, binary, "", args...)
	if want := "spec-check: print would leave " + filepath.Join(only, "acme-keeps-each-order.yaml") + ":30: " + missing + "\n"; got.code != 2 || !strings.Contains(got.stderr, want) {
		t.Errorf("print of the statements only: exit %d, stderr %q; want exit 2 and %q", got.code, got.stderr, want)
	}
	if texts := readTexts(t, only); len(texts) != 0 {
		t.Fatalf("print wrote %q, want nothing", texts)
	}
	after, _, refusals := planPrint(only, notes.Set{Files: map[string][]byte{}}, a, statements, notes.MaxFileSize)
	if len(refusals) == 0 || after == nil {
		t.Fatalf("planPrint of the statements only: refusals %q, after %v; want the refusal and the files", refusals, after)
	}
	for name, content := range after {
		writeFile(t, filepath.Join(only, name), content)
	}
	expect(t, runBinary(t, binary, "", "tree", "--dir", only), result{
		code:   1,
		stdout: "spec-check: " + only + ": 3 statements, 2 checks, 0 components, 0 interfaces, 0 terms; the tree is refused\n",
		stderr: "spec-check: " + only + noList + "spec-check: " + filepath.Join(only, "acme-keeps-each-order.yaml") + ":30: " + missing + "\n",
	})

	// The second control: one word more in the text of a statement.
	file := filepath.Join(dir, "acme-keeps-each-order.yaml")
	replaceIn(t, file, "  - ACME keeps each stored order after the order service starts again.\n", "  - ACME keeps each stored paid order after the order service starts again.\n")
	expect(t, runBinary(t, binary, "", "compare", "--dir", dir, "--approved", approved), result{
		code:   1,
		stdout: "spec-check: " + dir + ": the comparison fails on 1 node\n",
		stderr: "spec-check: " + file + ":7: an-order-survives-a-restart: " + unapprovedText + "\n",
	})
	expect(t, runBinary(t, binary, "", "tree", "--dir", dir), result{
		stdout: "spec-check: " + dir + ": 3 statements, 2 checks, 3 components, 1 interface, 1 term; the tree is valid\n",
		stderr: "spec-check: " + dir + noList,
	})
}

// A check alone, into a directory that holds no statement: tree would refuse the check
// (check-serves), so print writes nothing and names the statement that the check needs. The
// control prints the check with its statement and that statement's parent, and writes them.
func TestPrintRefusesACheckWithoutItsStatement(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	approved := fixture(t, "compare", "one-word", "approved.json")
	dir := t.TempDir()
	expect(t, runBinary(t, binary, "", "print", "--approved", approved, "--id", "a-restart-keeps-each-order", "--dir", dir), result{
		code: 2,
		stderr: "spec-check: print would write check \"a-restart-keeps-each-order\", whose statement \"an-order-survives-a-restart\" " + dir +
			" does not hold: check-serves: a check serves exactly one statement of the files\n",
	})
	if texts := readTexts(t, dir); len(texts) != 0 {
		t.Fatalf("print wrote %q, want nothing", texts)
	}

	got := runBinary(t, binary, "", "print", "--approved", approved, "--id", "a-restart-keeps-each-order", "--id", "acme-keeps-each-order", "--id", "an-order-survives-a-restart", "--dir", dir)
	if got.code != 0 || got.stderr != "" {
		t.Fatalf("the control: exit %d, stderr %q; want exit 0", got.code, got.stderr)
	}
	texts := readTexts(t, dir)
	documents := splitDocuments(texts["acme-keeps-each-order.yaml"])
	var order []string
	for _, d := range documents {
		order = append(order, strings.TrimPrefix(strings.Split(d, "\n")[2], "id: "))
	}
	if len(texts) != 1 || !slices.Equal(order, []string{"acme-keeps-each-order", "an-order-survives-a-restart", "a-restart-keeps-each-order"}) {
		t.Errorf("print wrote %q, want acme-keeps-each-order.yaml with the three documents in that order", texts)
	}
	expect(t, runBinary(t, binary, "", "tree", "--dir", dir), result{
		stdout: "spec-check: " + dir + ": 2 statements, 1 check, 0 components, 0 interfaces, 0 terms; the tree is valid\n",
		stderr: "spec-check: " + dir + noList,
	})
}

// A node that two files hold has no single home: print writes nothing and names both places, and
// the directory stays byte for byte. The control, with one of the two files taken out, replaces
// the statement where it stands and writes its two checks after it.
func TestPrintRefusesANodeHeldTwoTimes(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	approved := fixture(t, "compare", "two-checks", "approved.json")
	fixed, err := os.ReadFile(fixture(t, "compare", "two-checks", "spec", "orders.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	documents := splitDocuments(string(fixed))
	top, statement := documents[0], documents[1]
	earlier := strings.Replace(statement, "starts again.", "starts again soon.", 1)
	if earlier == statement {
		t.Fatal("the fixture holds no statement text to change")
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.yaml"), []byte(top+earlier))
	writeFile(t, filepath.Join(dir, "b.yaml"), []byte(statement))
	before := readTexts(t, dir)
	expect(t, runBinary(t, binary, "", "print", "--approved", approved, "--id", "an-order-survives-a-restart", "--dir", dir), result{
		code: 2,
		stderr: "spec-check: print would write node \"an-order-survives-a-restart\", which " + filepath.Join(dir, "a.yaml") + ":7 and " +
			filepath.Join(dir, "b.yaml") + ":1 both hold\n",
	})
	if got := readTexts(t, dir); !maps.Equal(got, before) {
		t.Errorf("the directory changed: %q, want %q", got, before)
	}

	if err := os.Remove(filepath.Join(dir, "b.yaml")); err != nil {
		t.Fatal(err)
	}
	if got := runBinary(t, binary, "", "print", "--approved", approved, "--id", "an-order-survives-a-restart", "--dir", dir); got.code != 0 {
		t.Fatalf("the control: exit %d, stderr %q; want exit 0", got.code, got.stderr)
	}
	if got, want := readTexts(t, dir)["a.yaml"], strings.Join(documents[:4], ""); got != want {
		t.Errorf("a.yaml is %q, want the statement replaced and its two checks after it, %q", got, want)
	}
}

// print never exits 0 with a node lost or changed that the run does not name, whatever line breaks
// a file holds: the two files of the red team of print (three terms with CR line ends, and three
// terms whose first name holds 30 U+2028 in single quotes), then each other line break that yaml.v3
// reads as one (U+0085, U+2028, U+2029, CR LF and CR) in a quoted key, after a key, in a quoted
// text, in a block text and between two documents, and a CR at the end of the file. A run of print
// of the third term, and one of the first, each either exits 2 with the file as it was, or exits 0
// with each other term as yaml.v3 read it before the run. No refusal names a node that the run
// would lose, since the reader cuts each file where yaml.v3 reads its documents. LF alone is the
// control: print writes it.
func TestPrintLosesNoNodeOfAFileWithOtherLineBreaks(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	term := func(id string) string {
		return "---\ntype: term\nid: " + id + "\nname: " + id + "\ndefinition:\n  - A term is a unit of meaning.\n"
	}
	first, rest := term("first-term"), term("second-term")+term("third-term")
	approved := writeApproved(t, map[string]string{"first-term": first, "second-term": term("second-term"), "third-term": term("third-term")})
	rows := map[string]string{
		"LF alone":                      first + rest,
		"carriage returns as line ends": strings.ReplaceAll(first+rest, "\n", "\r"),
		"U+2028 in a quoted name":       strings.Replace(first, "name: first-term", "name: 'A"+strings.Repeat("\u2028  B", 30)+"'", 1) + rest,
		"a CR at the end of the file":   first + rest + "\r",
	}
	for name, lineBreak := range map[string]string{"U+0085": "\u0085", "U+2028": "\u2028", "U+2029": "\u2029", "CR LF": "\r\n", "CR": "\r"} {
		rows[name+" in a quoted key"] = strings.Replace(first, "definition:\n", "'a"+lineBreak+"  key': x\ndefinition:\n", 1) + rest
		rows[name+" after a key"] = strings.Replace(first, "name: first-term", "name:"+lineBreak+"  first-term", 1) + rest
		rows[name+" in a quoted text"] = strings.Replace(first, "name: first-term", "name: 'first"+lineBreak+"  term'", 1) + rest
		rows[name+" in a block text"] = strings.Replace(first, "  - A term is a unit of meaning.\n", "  - |-\n    A term"+lineBreak+"    is a unit of meaning.\n", 1) + rest
		rows[name+" between documents"] = first + lineBreak + rest
	}
	for name, text := range rows {
		for _, id := range []string{"third-term", "first-term"} {
			t.Run(name+", print of "+id, func(t *testing.T) {
				t.Parallel()
				dir := t.TempDir()
				file := filepath.Join(dir, "terms.yaml")
				writeFile(t, file, []byte(text))
				before := yamlNodes(t, map[string][]byte{"terms.yaml": []byte(text)})
				got := runBinary(t, binary, "", "print", "--approved", approved, "--id", id, "--dir", dir)
				if strings.Contains(got.stderr, "print would lose or change node") {
					t.Errorf("the plan of the run loses a node, so the reader cut the file in another way than yaml.v3: %s", got.stderr)
				}
				after := readDir(t, dir)
				switch got.code {
				case 2:
					if name == "LF alone" {
						t.Fatalf("print of the printed file: exit 2, stderr %q", got.stderr)
					}
					if !bytes.Equal(after["terms.yaml"], []byte(text)) || len(after) != 1 {
						t.Errorf("a refused run changed the directory: %q", after)
					}
				case 0:
					want := maps.Clone(before)
					want[id] = yamlNodes(t, map[string][]byte{"run.yaml": []byte(term(id))})[id]
					if nodes := yamlNodes(t, after); !maps.EqualFunc(nodes, want, slices.Equal) {
						t.Errorf("print exits 0, and the files hold %q; want %q", nodes, want)
					}
				default:
					t.Fatalf("print: exit %d, stderr %q", got.code, got.stderr)
				}
			})
		}
	}
}

// A file whose documents the reader cannot say for certain is refused by its name, and print
// writes nothing: a file whose reading stops at a fault of the YAML, beside a valid file that the
// run would write into, and a file that starts with the directive %YAML 1.2, which yaml.v3 does
// not read (F1 of the second red team). R1 gives the files to a standard YAML library, and R2 has
// the tree checker refuse each document that is not as the printer writes it, so print takes no
// such file; the refusal names the file, the library's own words for the fault, and the way out.
func TestPrintRefusesAFileItCannotCut(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	term := "---\ntype: term\nid: first-term\nname: first term\ndefinition:\n  - A term is a unit of meaning.\n"
	approved := writeApproved(t, map[string]string{"first-term": term})
	for _, row := range []struct {
		name, file, text, why string
	}{
		{"a fault after the documents", "broken.yaml", term + "---\ntype: [\n", "the YAML library stops at line 8 (line 8: did not find expected node content)"},
		{"the directive %YAML 1.2", "versioned.yaml", "%YAML 1.2\n" + term, "the YAML library stops at line 1 (found incompatible YAML document)"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "terms.yaml"), []byte(term))
			writeFile(t, filepath.Join(dir, row.file), []byte(row.text))
			before := readDir(t, dir)
			expect(t, runBinary(t, binary, "", "print", "--approved", approved, "--id", "first-term", "--dir", dir), result{
				code: 2,
				stderr: "spec-check: print would write into " + dir + ", and the reader cannot say which documents " + filepath.Join(dir, row.file) +
					" holds: " + row.why + ", so the reader does not know the documents after that line. Correct that file, then run print again\n",
			})
			if got := readDir(t, dir); !maps.EqualFunc(got, before, bytes.Equal) {
				t.Errorf("the directory changed: %q", got)
			}
		})
	}
}

// A print of some of the nodes of a file changes only those nodes, so it cannot write in the
// printed form a node that it is not named: a comment before the first node, or a folded text in
// it (F2 of the second red team). R2 has the tree checker refuse a document that is not as the
// printer writes it, and print writes nothing that tree refuses, so the run is refused; the line
// names the file, the cause and the way out, each node of the file by its id. A run that names
// each node of the file writes it, and tree takes it.
func TestPrintNamesTheWayOutOfAFileThatIsNotPrinted(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	term := func(id string) string {
		return "---\ntype: term\nid: " + id + "\nname: " + strings.ReplaceAll(id, "-", " ") + "\ndefinition:\n  - A term is a unit of meaning.\n"
	}
	ids := []string{"first-term", "second-term", "third-term"}
	approved := writeApproved(t, map[string]string{ids[0]: term(ids[0]), ids[1]: term(ids[1]), ids[2]: term(ids[2])})
	printed := term(ids[0]) + term(ids[1]) + term(ids[2])
	for _, row := range []struct {
		name, text string
		line       int
		got, want  string
		difference string
	}{
		{"a comment before the first node", "# Terms used by the service.\n" + printed, 1, "# Terms used by the service.\n", "---\n", "a comment"},
		{"a folded text", strings.Replace(printed, "  - A term is a unit of meaning.\n", "  - >-\n    A term is\n    a unit of meaning.\n", 1), 6, "  - >-\n", "  - A term is a unit of meaning.\n", "another spelling"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			file := filepath.Join(dir, "terms.yaml")
			writeFile(t, file, []byte(row.text))
			expect(t, runBinary(t, binary, "", "print", "--approved", approved, "--id", "third-term", "--dir", dir), result{
				code: 2,
				stderr: "spec-check: print would leave " + file + ":" + strconv.Itoa(row.line) + ": first-term: " + notPrinted(row.line, row.got, row.want, row.difference) +
					"; print changes only the nodes that the run names, and this run does not name first-term, so to write " + file +
					" in that form, name each node of it: --id first-term --id second-term --id third-term\n",
			})
			if got := readTexts(t, dir); got["terms.yaml"] != row.text || len(got) != 1 {
				t.Fatalf("the refused run changed the directory: %q", got)
			}
			if got := runBinary(t, binary, "", "print", "--approved", approved, "--id", "first-term", "--id", "second-term", "--id", "third-term", "--dir", dir); got.code != 0 {
				t.Fatalf("print of each node of the file: exit %d, stderr %q", got.code, got.stderr)
			}
			if got := readTexts(t, dir); got["terms.yaml"] != printed {
				t.Errorf("print of each node of the file wrote %q, want %q", got["terms.yaml"], printed)
			}
			if got := runBinary(t, binary, "", "tree", "--dir", dir); got.code != 0 {
				t.Errorf("tree after the print of each node: exit %d, stderr %q", got.code, got.stderr)
			}
		})
	}
}

// A run of print changes all of its files or none (B2). The case of the review: a directory named
// components.yaml beside the file of a statement, and a run that changes the statement and adds
// a component, which goes into components.yaml. print refuses before it writes anything, and the
// directory is byte for byte as it was, so tree still takes it.
func TestPrintWritesNothingWhenOneFileCannotBeWritten(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	statement := "---\ntype: requirement\nid: a-claim\nkind: behavior\ntext:\n  - The daemon keeps each claim.\n"
	check := "---\ntype: check\nid: claim-check\nserves: a-claim\nsubject: graph\nprocedure: query\nclaim: The graph holds each claim.\n" +
		"procedure_text:\n  - Read each claim.\nfailing_example:\n  - A graph with no claims.\n"
	approved := writeApproved(t, map[string]string{
		"a-claim":     "---\ntype: requirement\nid: a-claim\nkind: structure\nabout:\n  - z-component\ntext:\n  - The daemon keeps each claim.\n",
		"claim-check": check,
		"z-component": "---\ntype: component\nid: z-component\nname: A component\nkind: component\npurpose:\n  - It keeps each claim.\n",
	})
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.yaml"), []byte(statement+check))
	components := filepath.Join(dir, "components.yaml")
	if err := os.Mkdir(components, 0o755); err != nil {
		t.Fatal(err)
	}
	// entries is each entry of dir by name: a file's text, or "directory".
	entries := func() map[string]string {
		all := map[string]string{}
		list, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range list {
			all[entry.Name()] = "directory"
			if !entry.IsDir() {
				content, err := os.ReadFile(filepath.Join(dir, entry.Name()))
				if err != nil {
					t.Fatal(err)
				}
				all[entry.Name()] = string(content)
			}
		}
		return all
	}
	before := entries()
	expect(t, runBinary(t, binary, "", "print", "--approved", approved, "--id", "a-claim", "--id", "z-component", "--dir", dir), result{
		code: 2,
		stderr: "spec-check: warning: " + components + ": not a spec file (directory); the importer passes over it\n" +
			"spec-check: print would write " + components + ", where " + dir + " holds a directory, not a spec file: print replaces a spec file only\n",
	})
	if got := entries(); !maps.Equal(got, before) {
		t.Errorf("the directory changed: %q, want %q", got, before)
	}
	if got := runBinary(t, binary, "", "tree", "--dir", dir); got.code != 0 {
		t.Errorf("tree after the refused run: exit %d, stderr %q", got.code, got.stderr)
	}
}

// Two runs of print at one time into one directory each write their node (N1): print holds the
// lock of the directory for its whole run, so the second run waits for the first and reads the
// files that the first wrote. Five trials of four runs, each with another term of about 12 KB, into
// a directory that does not exist yet; after each trial the directory holds the four terms. A run
// that cannot take the lock within its wait refuses by name and writes nothing.
func TestPrintRunsOneAtATimeOnADirectory(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	documents := map[string]string{}
	var ids []string
	for i := range 20 {
		id := fmt.Sprintf("term-%c%c", 'a'+i/10, 'a'+i%10)
		var b strings.Builder
		b.WriteString("---\ntype: term\nid: " + id + "\nname: term " + id + "\ndefinition:\n")
		for line := range 500 {
			fmt.Fprintf(&b, "  - Line %d of the term.\n", line)
		}
		documents[id] = b.String()
		ids = append(ids, id)
	}
	approved := writeApproved(t, documents)
	for trial := range 5 {
		dir := filepath.Join(t.TempDir(), "spec")
		var wg sync.WaitGroup
		results := make([]result, 4)
		for i := range 4 {
			wg.Go(func() {
				results[i] = runBinary(t, binary, "", "print", "--approved", approved, "--id", ids[4*trial+i], "--dir", dir)
			})
		}
		wg.Wait()
		for i, got := range results {
			if got.code != 0 {
				t.Errorf("trial %d, run %d: exit %d, stderr %q", trial, i, got.code, got.stderr)
			}
		}
		held := yamlNodes(t, readDir(t, dir))
		for _, id := range ids[4*trial : 4*trial+4] {
			if len(held[id]) != 1 {
				t.Errorf("trial %d: the directory holds %s %d times, want 1: a run wrote over the files of another", trial, id, len(held[id]))
			}
		}
	}

	t.Run("a run that waits too long refuses by name", func(t *testing.T) {
		dir := t.TempDir()
		lock, err := lockSpecDir(osFileSystem{}, dir, 0)
		if err != nil {
			t.Fatal(err)
		}
		a := readApprovedFile(t, approved)
		var stderr strings.Builder
		if code := printApproved(io.Discard, &stderr, osFileSystem{}, approved, a, ids[:1], dir, printBounds{limit: notes.MaxFileSize, wait: 100 * time.Millisecond}); code != 2 ||
			stderr.String() != "spec-check: "+dir+": another run of print holds the lock of the directory, and this run waited 100ms for it; run print again when that run ends\n" {
			t.Errorf("print while another run holds the lock: exit %d, stderr %q", code, stderr.String())
		}
		if got := readDir(t, dir); len(got) != 0 {
			t.Errorf("print wrote %q while another run held the lock", got)
		}
		lock.release(true)
		stderr.Reset()
		if code := printApproved(io.Discard, &stderr, osFileSystem{}, approved, a, ids[:1], dir, printBounds{limit: notes.MaxFileSize, wait: 100 * time.Millisecond}); code != 0 {
			t.Errorf("print after the lock is released: exit %d, stderr %q", code, stderr.String())
		}
	})
}

// The invariant of print: after each run, each file is within the bound, each node stands in
// exactly one file, no file is empty, the judgment of tree finds no problem, the files hold each
// node that they held before the run as yaml.v3 reads it, each node of the run as its approved
// document, and no other node, each check that the run wrote is in the block of its statement, and
// a second run changes no byte; a refused run writes nothing. The test runs print's own path after
// its arguments (printApproved, with a bound of 4,096 bytes) over random approved sets: printSeeds
// seeds, each up to 40 runs of 1 to 5 nodes on one directory, with a new approved version of a
// node between runs. Before one run in four it plants a fault in one file (plantFault): the same
// nodes with other line breaks, a term in front whose name holds a line break of YAML 1.1, or a
// fault of the YAML after the documents. The plan of no run loses a node, and print refuses a run
// over a file whose reading stops, by its name. It replays each run with Layout.Place to name the
// answer that each node got, and requires each answer that print can give to occur: W (written at
// home), M (moved to the next file of its series) and T (refused, its block over the bound) for
// each kind of node, new and replaced, and S and P (refused, its statement or its parent not in
// the files).
//
// The proof of the rule itself is TestPlace, a row for each cell of its table; this test proves
// that print follows the rule. Over the seeds 1 to 2,000 of the generator before it planted faults,
// the last answer to occur first occurred at seed 92 (term replaced T), so the default is five times
// that, 460 seeds, and the test requires each answer at that count. SPEC_CHECK_PRINT_SEEDS raises
// the count, for example to 2000 for a longer run.
func TestPrintKeepsTheInvariant(t *testing.T) {
	t.Parallel()
	const limit = 4096
	seeds := printSeeds(t)
	var mu sync.Mutex
	seen := map[string]bool{}
	// The seeds are spread over one group for each core, so the test takes the time of the seeds
	// divided by the cores.
	groups := min(max(runtime.GOMAXPROCS(0), 1), 64)
	t.Run("seeds", func(t *testing.T) {
		for group := range groups {
			t.Run(strconv.Itoa(group), func(t *testing.T) {
				t.Parallel()
				for seed := uint64(group + 1); seed <= seeds; seed += uint64(groups) {
					answers := printSeed(t, seed, limit)
					mu.Lock()
					for answer := range answers {
						seen[answer] = true
					}
					mu.Unlock()
					if t.Failed() {
						return
					}
				}
			})
		}
	})
	var want []string
	for _, kind := range []string{"statement", "check", "component", "term"} {
		for _, age := range []string{"new", "replaced"} {
			for _, answer := range []string{"W", "M", "T"} {
				want = append(want, kind+" "+age+" "+answer)
			}
		}
	}
	want = append(want, "S", "P")
	for _, answer := range want {
		if !seen[answer] {
			t.Errorf("no run gave the answer %q, so the generator reaches no such case", answer)
		}
	}
}

// defaultPrintSeeds is the count of seeds of TestPrintKeepsTheInvariant: five times the seed at
// which the last of its answers first occurs (92), and at least 100.
const defaultPrintSeeds = 460

// printSeeds is the count of seeds of TestPrintKeepsTheInvariant: SPEC_CHECK_PRINT_SEEDS when it is
// set, a whole number of at least defaultPrintSeeds, else defaultPrintSeeds.
func printSeeds(t *testing.T) uint64 {
	t.Helper()
	value, set := os.LookupEnv("SPEC_CHECK_PRINT_SEEDS")
	if !set {
		return defaultPrintSeeds
	}
	seeds, err := strconv.ParseUint(value, 10, 64)
	if err != nil || seeds < defaultPrintSeeds {
		t.Fatalf("SPEC_CHECK_PRINT_SEEDS is %q; want a whole number of at least %d, which raises the count of seeds", value, defaultPrintSeeds)
	}
	return seeds
}

// printSeed runs the printer over the random approved set of seed, and answers each answer that
// a node got. After a run that print refuses, it puts a file with a planted fault back.
func printSeed(t *testing.T, seed uint64, limit int64) map[string]bool {
	r := rand.New(rand.NewPCG(seed, seed))
	g := newPrintSet(r)
	dir := t.TempDir()
	answers := map[string]bool{}
	bounds := printBounds{limit: limit, wait: printLockWait}
	for range 1 + r.IntN(40) {
		g.change(r)
		a := g.approved()
		var ids []string
		for range 1 + r.IntN(5) {
			ids = append(ids, g.ids[r.IntN(len(g.ids))])
		}
		clean := readDir(t, dir)
		files, planted, stops := clean, "", false
		if len(clean) > 0 && r.IntN(4) == 0 {
			planted = slices.Sorted(maps.Keys(clean))[r.IntN(len(clean))]
			files = maps.Clone(clean)
			files[planted], stops = plantFault(r, clean[planted])
			writeFile(t, filepath.Join(dir, planted), files[planted])
		}
		current, _ := a.Current()
		run := printRun(current, ids)
		if !stops {
			replay, _ := specgraph.ReadLayout(files)
			for _, node := range run {
				answer, ok := placeAnswer(&replay, node, limit)
				answers[answer] = true
				if !ok {
					break
				}
			}
		}
		var stderr strings.Builder
		code := printApproved(io.Discard, &stderr, osFileSystem{}, "approved.json", a, ids, dir, bounds)
		for _, lost := range []string{"print would lose or change node", "print would not leave node", "which neither the files nor the run held"} {
			if strings.Contains(stderr.String(), lost) {
				t.Fatalf("seed %d: the plan of a run loses or changes a node, so the reader cut a file in another way than yaml.v3 reads it: %s", seed, stderr.String())
			}
		}
		if stops && (code != 2 || !strings.Contains(stderr.String(), "the reader cannot say which documents "+filepath.Join(dir, planted)+" holds")) {
			t.Fatalf("seed %d: print over %s, whose reading stops at a fault, did not refuse it by name: exit %d: %s", seed, planted, code, stderr.String())
		}
		switch code {
		case 2:
			if got := readDir(t, dir); !maps.EqualFunc(got, files, bytes.Equal) {
				t.Fatalf("seed %d: a refused run changed the files: %s", seed, stderr.String())
			}
			if planted != "" {
				writeFile(t, filepath.Join(dir, planted), clean[planted])
			}
			continue
		case 0:
		default:
			t.Fatalf("seed %d: print exit %d: %s", seed, code, stderr.String())
		}
		holdsTheInvariant(t, seed, dir, limit, files, run)
		written := readDir(t, dir)
		stderr.Reset()
		if code := printApproved(io.Discard, &stderr, osFileSystem{}, "approved.json", a, ids, dir, bounds); code != 0 || !maps.EqualFunc(readDir(t, dir), written, bytes.Equal) {
			t.Fatalf("seed %d: a second run with the same arguments exits %d or changes the files: %s", seed, code, stderr.String())
		}
	}
	return answers
}

// plantFault is content, a file that print wrote, as an editor or another tool can leave it, and
// whether yaml.v3 stops reading it: the same documents with a carriage return or CR LF as each
// line end; a term in front whose name holds U+0085, U+2028 or U+2029 as yaml.v3 writes it, a
// line break in a single-quoted scalar; or a fault of the YAML after the documents.
func plantFault(r *rand.Rand, content []byte) ([]byte, bool) {
	switch r.IntN(4) {
	case 0:
		return bytes.ReplaceAll(content, []byte("\n"), []byte("\r")), false
	case 1:
		return bytes.ReplaceAll(content, []byte("\n"), []byte("\r\n")), false
	case 2:
		lineBreak := []string{"\u0085", "\u2028", "\u2029"}[r.IntN(3)]
		term := "---\ntype: term\nid: planted-term\nname: 'planted" + strings.Repeat(lineBreak+"  term", 1+r.IntN(20)) + "'\ndefinition:\n  - A planted term is a word.\n"
		return append([]byte(term), content...), false
	}
	return append(slices.Clone(content), "---\ntype: [\n"...), true
}

// placeAnswer places node into l and names its answer: "<kind> new|replaced W|M|T", or S or P;
// ok is false for a refusal, after which print places no more nodes.
func placeAnswer(l *specgraph.Layout, node specgraph.Node, limit int64) (string, bool) {
	id := node.NodeID()
	home, replaced := "", false
	for _, name := range slices.Sorted(maps.Keys(l.Files)) {
		for _, d := range l.Files[name] {
			if d.Node != nil && d.Node.NodeID() == id {
				home, replaced = name, true
			}
		}
	}
	statementFile := func(id string) string {
		for _, name := range slices.Sorted(maps.Keys(l.Files)) {
			for _, d := range l.Files[name] {
				if s, ok := d.Node.(specgraph.Statement); ok && s.ID == id {
					return name
				}
			}
		}
		return ""
	}
	kind, age := "term", "new"
	if replaced {
		age = "replaced"
	}
	switch n := node.(type) {
	case specgraph.Statement:
		kind = "statement"
		if !replaced {
			home = n.ID + ".yaml"
			if n.Parent != "" {
				home = statementFile(n.Parent)
			}
		}
	case specgraph.Check:
		kind = "check"
		if !replaced {
			home = statementFile(n.Serves)
		}
	case specgraph.Component:
		kind = "component"
		if !replaced {
			home = "components.yaml"
		}
	default:
		if !replaced {
			home = "terms.yaml"
		}
	}
	file, err := l.Place(node, limit)
	switch {
	case errors.Is(err, specgraph.ErrNoStatement):
		return "S", false
	case errors.Is(err, specgraph.ErrNoParent):
		return "P", false
	case errors.Is(err, specgraph.ErrTooLarge):
		return kind + " " + age + " T", false
	case err != nil:
		return "error " + err.Error(), false
	case file == home:
		return kind + " " + age + " W", true
	default:
		return kind + " " + age + " M", true
	}
}

// holdsTheInvariant fails the test when the files of dir break the invariant of print after a run
// that wrote the nodes of run into the files before.
func holdsTheInvariant(t *testing.T, seed uint64, dir string, limit int64, before map[string][]byte, run []specgraph.Node) {
	t.Helper()
	files := readDir(t, dir)
	for name := range files {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() > limit || info.Size() == 0 {
			t.Fatalf("seed %d: %s is %d bytes, want 1 to %d", seed, name, info.Size(), limit)
		}
	}
	tree, problems := specgraph.Parse(files)
	if problems = append(problems, specgraph.Validate(tree, specgraph.Options{})...); len(problems) > 0 {
		t.Fatalf("seed %d: tree refuses the files that print wrote: %+v", seed, problems)
	}
	// The nodes as yaml.v3 reads them: each node of the files before the run, with each node of
	// the run in place of its id's.
	want := yamlNodes(t, before)
	for id, data := range yamlNodes(t, map[string][]byte{"run.yaml": printed(t, run...)}) {
		want[id] = data
	}
	got := yamlNodes(t, files)
	for id := range got {
		if _, held := want[id]; !held {
			want[id] = nil
		}
	}
	for _, id := range slices.Sorted(maps.Keys(want)) {
		if !slices.Equal(got[id], want[id]) {
			t.Fatalf("seed %d: node %s: the files hold %q, and want %q", seed, id, got[id], want[id])
		}
	}
	// The nodes of each file in their order, from the line of each.
	type held struct {
		line int
		node specgraph.Node
	}
	byFile := map[string][]held{}
	count := map[string]int{}
	for _, node := range specgraph.Nodes(tree) {
		pos := node.NodePos()
		byFile[pos.File] = append(byFile[pos.File], held{pos.Line, node})
		count[node.NodeID()]++
	}
	for name, nodes := range byFile {
		slices.SortFunc(nodes, func(a, b held) int { return a.line - b.line })
		for i, h := range nodes {
			c, ok := h.node.(specgraph.Check)
			if !ok || !slices.ContainsFunc(run, func(n specgraph.Node) bool { return n.NodeID() == c.ID }) {
				continue
			}
			j := i - 1
			for j >= 0 {
				prev, isCheck := nodes[j].node.(specgraph.Check)
				if !isCheck || prev.Serves != c.Serves {
					break
				}
				j--
			}
			if s, ok := nodes[max(j, 0)].node.(specgraph.Statement); j < 0 || !ok || s.ID != c.Serves {
				t.Fatalf("seed %d: check %s in %s is not in the block of its statement %s", seed, c.ID, name, c.Serves)
			}
		}
	}
	for id, n := range count {
		if n != 1 {
			t.Fatalf("seed %d: node %s stands in %d places", seed, id, n)
		}
	}
}

// printSet is a random approved set: a forest of statements, the checks of each, components with
// parents, an interface, terms, and the versions of each node.
type printSet struct {
	ids      []string
	versions map[string][]specgraph.Node
	text     func(r *rand.Rand) string
	// spec is the approved spec of versions, written again only after a change, since each run
	// reads it and each version prints once.
	spec specgraph.Approved
}

// printWords are the words of the texts of printSet: plain words, so that no rule of the texts
// refuses a node.
var printWords = strings.Fields("a an the order message claim session listener store keeps each reads after starts again before " +
	"customer service billing page shows sends gets only through part holds every that it of to")

func newPrintSet(r *rand.Rand) *printSet {
	g := &printSet{versions: map[string][]specgraph.Node{}, spec: specgraph.Approved{Nodes: map[string]specgraph.ApprovedNode{}}}
	g.text = func(r *rand.Rand) string {
		lines, width := 1+r.IntN(30), 20+r.IntN(100)
		if r.IntN(100) < 2 {
			lines, width = 50, 100
		}
		var out []string
		for range lines {
			var line []string
			for n := 0; n < width-10; {
				w := printWords[r.IntN(len(printWords))]
				line = append(line, w)
				n += len(w) + 1
			}
			out = append(out, strings.Join(line, " "))
		}
		return strings.Join(out, "\n")
	}
	add := func(node specgraph.Node) {
		id := node.NodeID()
		g.ids = append(g.ids, id)
		g.versions[id] = []specgraph.Node{node}
		g.approve(id, node)
	}
	var plain []string
	for i := range r.IntN(5) {
		c := specgraph.Component{ID: fmt.Sprintf("part-%c", 'a'+i), Name: fmt.Sprintf("Part %c", 'a'+i), Purpose: g.text(r)}
		if len(plain) > 0 && r.IntN(2) == 0 {
			c.Parent = plain[r.IntN(len(plain))]
		}
		if len(plain) >= 2 && r.IntN(3) == 0 {
			c.Parent, c.Interface, c.Joins = "", true, []string{plain[0], plain[1]}
		} else {
			plain = append(plain, c.ID)
		}
		add(c)
	}
	for i := range r.IntN(4) {
		add(specgraph.Term{ID: fmt.Sprintf("word-%c", 'a'+i), Name: fmt.Sprintf("word %c", 'a'+i), Definition: g.text(r)})
	}
	statements := 1 + r.IntN(12)
	parents := make([]string, statements)
	children := map[string]bool{}
	for i := range statements {
		id := fmt.Sprintf("claim-%c", 'a'+i)
		if i > 0 && r.IntN(3) > 0 {
			parents[i] = fmt.Sprintf("claim-%c", 'a'+r.IntN(i))
			children[parents[i]] = true
		}
		add(specgraph.Statement{ID: id, Kind: specgraph.KindBehavior, Parent: parents[i], Text: g.text(r)})
	}
	for i := range statements {
		statement := fmt.Sprintf("claim-%c", 'a'+i)
		checks := r.IntN(4)
		if !children[statement] && checks == 0 {
			checks = 1
		}
		for k := range checks {
			add(specgraph.Check{
				ID: fmt.Sprintf("test-%c-%c", 'a'+i, 'a'+k), Serves: statement, Subject: specgraph.SubjectCode, Procedure: specgraph.ProcedureAgent,
				Claim: "an agent reads the store", ProcedureText: g.text(r), FailingExample: "a store with no order",
			})
		}
	}
	return g
}

// change gives each node a new version with probability 15 in 100: a new text.
func (g *printSet) change(r *rand.Rand) {
	for _, id := range g.ids {
		if r.IntN(100) >= 15 {
			continue
		}
		versions := g.versions[id]
		switch n := versions[len(versions)-1].(type) {
		case specgraph.Statement:
			n.Text = g.text(r)
			g.versions[id] = append(versions, n)
			g.approve(id, n)
		case specgraph.Check:
			n.ProcedureText = g.text(r)
			g.versions[id] = append(versions, n)
			g.approve(id, n)
		case specgraph.Component:
			n.Purpose = g.text(r)
			g.versions[id] = append(versions, n)
			g.approve(id, n)
		case specgraph.Term:
			n.Definition = g.text(r)
			g.versions[id] = append(versions, n)
			g.approve(id, n)
		}
	}
}

// approve adds node, the next version of id, to the approved spec of g, its document as Print
// writes it.
func (g *printSet) approve(id string, node specgraph.Node) {
	document, err := node.Print()
	if err != nil {
		panic(err)
	}
	n, held := g.spec.Nodes[id]
	if !held {
		n = specgraph.ApprovedNode{ID: id}
	}
	at := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC).Add(time.Duration(len(n.Versions)) * time.Hour)
	n.Versions = append(slices.Clone(n.Versions), specgraph.ApprovedVersion{Version: len(n.Versions) + 1, At: at, Document: document})
	nodes := maps.Clone(g.spec.Nodes)
	nodes[id] = n
	g.spec = specgraph.Approved{Nodes: nodes}
}

// approved is the approved spec of g: each version of each node.
func (g *printSet) approved() specgraph.Approved {
	return g.spec
}

// readApprovedFile reads the approved spec at path.
func readApprovedFile(t *testing.T, path string) specgraph.Approved {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	a, err := specgraph.ReadApproved(f)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// readTexts is the text of each file directly in dir, by name.
func readTexts(t *testing.T, dir string) map[string]string {
	t.Helper()
	texts := map[string]string{}
	for name, content := range readDir(t, dir) {
		texts[name] = string(content)
	}
	return texts
}

// splitDocuments is each document of text, a spec file: the text from each line that is exactly
// "---" to the next.
func splitDocuments(text string) []string {
	var documents []string
	for line := range strings.Lines(text) {
		if line == "---\n" || len(documents) == 0 {
			documents = append(documents, "")
		}
		documents[len(documents)-1] += line
	}
	return documents
}

// printed is the spec file of nodes: the document that Print writes for each, in order.
func printed(t *testing.T, nodes ...specgraph.Node) []byte {
	t.Helper()
	var b strings.Builder
	for _, node := range nodes {
		document, err := node.Print()
		if err != nil {
			t.Fatal(err)
		}
		b.WriteString(document)
	}
	return []byte(b.String())
}

// yamlNodes is the data of each document of files, in name order, as yaml.v3 reads it and as JSON
// writes it, by the id that it holds: the nodes of a directory as the YAML library counts them,
// with no reader of specgraph. A file whose reading stops at a fault gives the documents before it.
func yamlNodes(t *testing.T, files map[string][]byte) map[string][]string {
	t.Helper()
	nodes := map[string][]string{}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		decoder := yaml.NewDecoder(bytes.NewReader(files[name]))
		for {
			var data any
			if decoder.Decode(&data) != nil {
				break // the end of the file, or a fault that stops the reading
			}
			js, err := json.Marshal(data)
			if err != nil {
				t.Fatalf("%s: a document with no JSON form: %v", name, err)
			}
			object, _ := data.(map[string]any)
			id, _ := object["id"].(string)
			nodes[id] = append(nodes[id], string(js))
		}
	}
	return nodes
}
