package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
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
// a file holds: three terms with CR line ends, and three terms whose first name holds 30 U+2028 in
// single quotes, then each other line break that yaml.v3
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
// not read. R1 gives the files to a standard YAML library, and R2 has
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
// it. R2 has the tree checker refuse a document that is not as the
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

// TestTheWayOutOfPrintNamesAWholeID holds that a line of spec-check does not cut an id of the way
// out of print, a value that the line tells the reader to type: print refuses a run over a file
// that holds a node that is not in its printed form and a node whose id is longer than the cut of
// 100 bytes, and a run of print with exactly the --id words of that line exits 0.
func TestTheWayOutOfPrintNamesAWholeID(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	longID := "w-" + strings.Repeat("a", 150) + "-z"
	check := func(id, serves, claim string) string {
		return "---\ntype: check\nid: " + id + "\nserves: " + serves + "\nsubject: code\nprocedure: program\ntest:\n  runner: go\n" +
			"  selector: example.com/acme/orders TestEachOrderIsKept\n  job: go-checks\nclaim: " + claim + "\n" +
			"procedure_text:\n  - The program stores one order and reads the orders.\n  - It must find the order.\n" +
			"failing_example:\n  - An order service that keeps no order.\n"
	}
	documents := map[string]string{
		"acme-keeps-each-order":     "---\ntype: requirement\nid: acme-keeps-each-order\nkind: behavior\ntext:\n  - ACME keeps each order that a customer sends.\n",
		"each-order-is-kept":        check("each-order-is-kept", "acme-keeps-each-order", "The order service keeps each order."),
		longID:                      "---\ntype: requirement\nid: " + longID + "\nkind: behavior\nparent: acme-keeps-each-order\ntext:\n  - ACME keeps the way-out order.\n",
		"the-way-out-order-is-kept": check("the-way-out-order-is-kept", longID, "The order service keeps the way-out order."),
	}
	approved := writeApproved(t, documents)
	spec := filepath.Join(t.TempDir(), "repo", ".dispatch", "spec")
	printAll := []string{"print", "--approved", approved}
	for _, id := range []string{"acme-keeps-each-order", "each-order-is-kept", longID, "the-way-out-order-is-kept"} {
		printAll = append(printAll, "--id", id)
	}
	if got := runBinary(t, binary, "", append(printAll, "--dir", spec)...); got.code != 0 {
		t.Fatalf("SETUP: print of the tree: exit %d, stderr %q", got.code, got.stderr)
	}
	replaceIn(t, filepath.Join(spec, "acme-keeps-each-order.yaml"), "kind: behavior\n", "kind: behavior \n")
	refused := runBinary(t, binary, "", "print", "--approved", approved, "--id", "each-order-is-kept", "--dir", spec)
	const marker = "name each node of it: "
	_, after, found := strings.Cut(refused.stderr, marker)
	if refused.code != 2 || !found {
		t.Fatalf("SETUP: print must refuse and name the way out: exit %d: %s", refused.code, refused.stderr)
	}
	line, _, _ := strings.Cut(after, "\n")
	words := strings.Fields(line)
	again := []string{"print", "--approved", approved}
	var ids []string
	for i := 0; i+1 < len(words); i++ {
		if words[i] == "--id" {
			ids = append(ids, words[i+1])
			again = append(again, "--id", words[i+1])
		}
	}
	t.Logf("the line names: %q", ids)
	if got := runBinary(t, binary, "", append(again, "--dir", spec)...); got.code != 0 {
		t.Errorf("print refuses the ids that its own way out names (a valid id longer than the cut): print with exactly those ids: exit %d: %s", got.code, got.stdout+got.stderr)
	}
}
