package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/specgraph"
)

// The Detail of each rule of specgraph.Compare, as the command prints it.
const (
	unapprovedID   = "unapproved-id: the approved spec never held this id"
	unapprovedText = "unapproved-text: the approved spec never held this text"
	removedID      = "removed-id: the approved spec removed this id and never held this text for it"
)

// ciRefusal is what compare writes, and all it writes, when GITHUB_ACTIONS is true.
const ciRefusal = "spec-check: compare does not run in a CI job (GITHUB_ACTIONS=true): R2 gives the comparison of the files with the approved spec at each push to Dispatch\n"

// timeForm is a time as the forms of the JSON files name its type: Z or an offset, and an
// upper-case T.
const timeForm = `<RFC 3339 time with an upper-case T, and Z or an offset such as +01:00>`

// approvedForm is the form of approved.json, as compare and print name it before the place of a
// value of another type.
const approvedForm = `{"nodes":[{"id":"<id>","removed":<true or false>,"partly_checked":<true or false>,"versions":[{"version":<whole number from 1>,"at":"` + timeForm + `","document":"<document>"}, …]}, …]}`

// testCompare runs compare over each fixture of specgraph's comparison tests (testdata/compare),
// each the approved spec and the files of one change: a node in a text that the approved spec
// never held fails the comparison, and the command names it with its file and line; the files
// that hold only approved texts pass, and the command names each approved node that the files do
// not hold at its current text, of each kind.
func testCompare(t *testing.T, binary string) {
	covered := map[string]bool{}
	for _, row := range []struct {
		fixture, file string
		line          int
		node, detail  string
	}{
		{"one-word", "orders.yaml", 7, "an-order-survives-a-restart", unapprovedText},
		{"unapproved-statement", "orders.yaml", 51, "each-order-gets-one-bill", unapprovedID},
		{"check-text", "orders.yaml", 14, "a-restart-keeps-each-order", unapprovedText},
		{"component-purpose", "components.yaml", 8, "orders", unapprovedText},
		{"interface-joins", "components.yaml", 15, "orders-to-billing", unapprovedText},
		{"term-definition", "components.yaml", 32, "order", unapprovedText},
		{"removed-id-reused", "orders.yaml", 51, "an-order-is-paid-once", removedID},
		{"one-of-four", "orders.yaml", 51, "each-order-gets-one-bill", unapprovedText},
	} {
		covered[row.fixture] = true
		t.Run(row.fixture+" fails on one node", func(t *testing.T) {
			t.Parallel()
			dir, approved := fixture(t, "compare", row.fixture, "spec"), fixture(t, "compare", row.fixture, "approved.json")
			expect(t, runBinary(t, binary, "", "compare", "--dir", dir, "--approved", approved), result{
				code:   1,
				stdout: "spec-check: " + dir + ": the comparison fails on 1 node\n",
				stderr: "spec-check: " + filepath.Join(dir, row.file) + ":" + strconv.Itoa(row.line) + ": " + row.node + ": " + row.detail + "\n",
			})
		})
	}
	for _, row := range []struct {
		fixture, held string
		others        []string
	}{
		{"approved-text", "3 of 3", nil},
		{"two-checks", "3 of 3", nil},
		// R28: the files hold a statement and its check that the approved spec removed, in a text
		// that it held. The comparison passes (R2) and names each of them.
		{"removed-kept", "3 of 3", []string{
			"statement an-order-is-paid-once: the files hold it, and the approved spec removed it",
			"check a-second-payment-is-refused: the files hold it, and the approved spec removed it",
		}},
		{"earlier-text", "2 of 3", []string{"statement an-order-survives-a-restart: the files hold approved version 1, and version 2 is current"}},
		// R2: the change that lands a statement writes the statement and its checks into the files.
		// The files hold neither, and a line names each.
		{"not-in-files", "2 of 3", []string{
			"statement orders-reach-billing-only-through-the-interface: not in the files",
			"check no-order-skips-the-interface: not in the files",
		}},
	} {
		covered[row.fixture] = true
		t.Run(row.fixture+" passes", func(t *testing.T) {
			t.Parallel()
			dir, approved := fixture(t, "compare", row.fixture, "spec"), fixture(t, "compare", row.fixture, "approved.json")
			want := passLine(dir, row.held)
			for _, line := range row.others {
				want += "spec-check: " + line + "\n"
			}
			expect(t, runBinary(t, binary, "", "compare", "--dir", dir, "--approved", approved), result{stdout: want})
		})
	}
	entries, err := os.ReadDir(fixture(t, "compare"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !covered[entry.Name()] {
			t.Errorf("testdata/compare/%s is in no row", entry.Name())
		}
	}

	t.Run("compare names a removed component, interface and term that the files hold", func(t *testing.T) {
		t.Parallel()
		removed := 0
		approved := approvedCopy(t, "approved-text", func(n *specgraph.ApprovedNode) {
			if n.ID == "shipping" || n.ID == "orders-to-billing" || n.ID == "order" {
				n.Removed = new(true)
				removed++
			}
		})
		if removed != 3 {
			t.Fatalf("the fixture holds %d of the three nodes, want 3", removed)
		}
		dir := fixture(t, "compare", "approved-text", "spec")
		expect(t, runBinary(t, binary, "", "compare", "--dir", dir, "--approved", approved), result{stdout: passLine(dir, "3 of 3") +
			"spec-check: interface orders-to-billing: the files hold it, and the approved spec removed it\n" +
			"spec-check: component shipping: the files hold it, and the approved spec removed it\n" +
			"spec-check: term order: the files hold it, and the approved spec removed it\n"})
	})
	// The files hold a removed statement in two files, which tree refuses as a doubled id: the line
	// that names it comes once.
	t.Run("compare names a removed node that the files hold two times once", func(t *testing.T) {
		t.Parallel()
		dir := copyOf(t, fixture(t, "compare", "removed-kept", "spec"))
		approved := fixture(t, "compare", "removed-kept", "approved.json")
		removed := approvedOf(t, "removed-kept").Nodes["an-order-is-paid-once"]
		if !*removed.Removed {
			t.Fatal("the fixture did not remove an-order-is-paid-once")
		}
		writeFile(t, filepath.Join(dir, "again.yaml"), []byte(removed.Versions[len(removed.Versions)-1].Document))
		expect(t, runBinary(t, binary, "", "compare", "--dir", dir, "--approved", approved), result{stdout: passLine(dir, "3 of 3") +
			"spec-check: statement an-order-is-paid-once: the files hold it, and the approved spec removed it\n" +
			"spec-check: check a-second-payment-is-refused: the files hold it, and the approved spec removed it\n"})
	})
	// R2: "The change that lands the code of a statement writes that statement and its checks into
	// the files". A change that leaves out a check, a component, an interface or a term gets a line
	// for each, as a statement does, and the first line still counts statements.
	t.Run("compare names each approved check, component, interface and term that the files do not hold", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		for name, ids := range map[string][]string{
			"orders.yaml":     {"a-restart-keeps-each-order"},
			"components.yaml": {"shipping", "orders-to-billing", "order"},
		} {
			content, err := os.ReadFile(fixture(t, "compare", "approved-text", "spec", name))
			if err != nil {
				t.Fatal(err)
			}
			text := string(content)
			for _, id := range ids {
				text = withoutDocument(t, text, id)
			}
			writeFile(t, filepath.Join(dir, name), []byte(text))
		}
		expect(t, runBinary(t, binary, "", "compare", "--dir", dir, "--approved", fixture(t, "compare", "approved-text", "approved.json")), result{stdout: passLine(dir, "3 of 3") +
			"spec-check: check a-restart-keeps-each-order: not in the files\n" +
			"spec-check: interface orders-to-billing: not in the files\n" +
			"spec-check: component shipping: not in the files\n" +
			"spec-check: term order: not in the files\n"})
	})
	t.Run("compare names each approved check, component, interface and term that the files hold at an earlier version", func(t *testing.T) {
		t.Parallel()
		changed := 0
		approved := approvedCopy(t, "approved-text", func(n *specgraph.ApprovedNode) {
			if n.ID != "a-restart-keeps-each-order" && n.ID != "shipping" && n.ID != "orders-to-billing" && n.ID != "order" {
				return
			}
			// Version 2 adds one word to the first line of the text of version 1 that ends a
			// sentence.
			n.Versions = append(n.Versions, specgraph.ApprovedVersion{
				Version:  2,
				At:       time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC),
				Document: strings.Replace(n.Versions[0].Document, ".\n", " now.\n", 1),
			})
			changed++
		})
		if changed != 4 {
			t.Fatalf("the fixture holds %d of the four nodes, want 4", changed)
		}
		dir := fixture(t, "compare", "approved-text", "spec")
		expect(t, runBinary(t, binary, "", "compare", "--dir", dir, "--approved", approved), result{stdout: passLine(dir, "3 of 3") +
			"spec-check: check a-restart-keeps-each-order: the files hold approved version 1, and version 2 is current\n" +
			"spec-check: interface orders-to-billing: the files hold approved version 1, and version 2 is current\n" +
			"spec-check: component shipping: the files hold approved version 1, and version 2 is current\n" +
			"spec-check: term order: the files hold approved version 1, and version 2 is current\n"})
	})
	// The verb and its pronoun take the number of the statements that the files hold at their
	// current text, and the noun is in the singular only for 1 of 1, so a verb in the plural never
	// follows a noun in the singular.
	t.Run("the first line of compare gives its noun and its verb one number", func(t *testing.T) {
		t.Parallel()
		only := func(keep func(id, document string) bool) string {
			return approvedCopy(t, "approved-text", func(n *specgraph.ApprovedNode) {
				if !keep(n.ID, n.Versions[len(n.Versions)-1].Document) {
					n.Removed = new(true)
				}
			})
		}
		oneStatement := only(func(id, _ string) bool { return id == "acme-keeps-each-order" })
		threeStatements := only(func(_, document string) bool { return strings.HasPrefix(document, "---\ntype: requirement\n") })
		empty, one := t.TempDir(), t.TempDir()
		writeFile(t, filepath.Join(one, "spec.yaml"), []byte("---\ntype: requirement\nid: acme-keeps-each-order\nkind: behavior\ntext:\n  - ACME keeps each order that a customer sends.\n"))
		held := ": the approved spec held every text that the files hold; "
		expect(t, runBinary(t, binary, "", "compare", "--allow-empty", "--dir", empty, "--approved", oneStatement), result{
			stdout: "spec-check: " + empty + held + "0 of 1 approved statements are in the files at their current text\n" +
				"spec-check: statement acme-keeps-each-order: not in the files\n",
			stderr: "spec-check: " + empty + ": holds no spec file, so compare reads it as the empty tree\n",
		})
		expect(t, runBinary(t, binary, "", "compare", "--dir", one, "--approved", oneStatement), result{
			stdout: "spec-check: " + one + held + "1 of 1 approved statement is in the files at its current text\n",
		})
		expect(t, runBinary(t, binary, "", "compare", "--dir", one, "--approved", threeStatements), result{
			stdout: "spec-check: " + one + held + "1 of 3 approved statements is in the files at its current text\n" +
				"spec-check: statement an-order-survives-a-restart: not in the files\n" +
				"spec-check: statement orders-reach-billing-only-through-the-interface: not in the files\n",
		})
	})

	t.Run("two nodes in texts never approved are two lines, in file and line order", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		for _, name := range []string{"orders.yaml", "components.yaml"} {
			content, err := os.ReadFile(fixture(t, "compare", "approved-text", "spec", name))
			if err != nil {
				t.Fatal(err)
			}
			text := strings.Replace(string(content), "It keeps the orders that customers send.", "It keeps the orders that customers send and cancel.", 1)
			writeFile(t, filepath.Join(dir, name), []byte(strings.Replace(text, "order service starts again.", "order service starts again fast.", 1)))
		}
		expect(t, runBinary(t, binary, "", "compare", "--dir", dir, "--approved", fixture(t, "compare", "approved-text", "approved.json")), result{
			code:   1,
			stdout: "spec-check: " + dir + ": the comparison fails on 2 nodes\n",
			stderr: "spec-check: " + filepath.Join(dir, "components.yaml") + ":8: orders: " + unapprovedText + "\n" +
				"spec-check: " + filepath.Join(dir, "orders.yaml") + ":7: an-order-survives-a-restart: " + unapprovedText + "\n",
		})
	})
	t.Run("compare makes no comparison of files that do not read", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "a.yaml"), []byte("A line that is no node.\n"))
		expect(t, runBinary(t, binary, "", "compare", "--dir", dir, "--approved", fixture(t, "compare", "approved-text", "approved.json")), result{
			code: 2,
			stderr: "spec-check: " + filepath.Join(dir, "a.yaml") + ":1: -: schema: node: got string, want object\n" +
				"spec-check: " + dir + noComparison,
		})
	})
	// A file in other quotes is not in its printed form, so compare makes no comparison of it, and
	// its line names the way out (TestCompareMakesNoComparisonOfAFileThatIsNotPrinted). A problem
	// that leaves each node as a printed file writes it (specgraph.KeepsEachNode), a value that the
	// schema refuses and the node keeps, is no reason of a refusal: beside a file whose reading
	// stops, which can hide a node (specgraph.LosesNode), only the fault of that file is named.
	t.Run("compare makes no comparison of files with a spelling that print does not write", func(t *testing.T) {
		t.Parallel()
		dir := copyOf(t, fixture(t, "compare", "approved-text", "spec"))
		orders := filepath.Join(dir, "orders.yaml")
		replaceIn(t, orders, "kind: behavior\n", "kind: 'behavior'\n")
		approved := fixture(t, "compare", "approved-text", "approved.json")
		expect(t, runBinary(t, binary, "", "compare", "--dir", dir, "--approved", approved), result{
			code: 2,
			stderr: "spec-check: " + orders + ":4: acme-keeps-each-order: not-printed: line 4 is \"kind: 'behavior'\\n\", and print writes \"kind: behavior\\n\" there (other quotes); spec-check print writes each node in that form\n" +
				"spec-check: " + dir + noComparison,
		})

		replaceIn(t, orders, "kind: 'behavior'\n", "kind: behavior\n")
		replaceIn(t, orders, "subject: code\n", "subject: the logs\n")
		writeFile(t, filepath.Join(dir, "broken.yaml"), []byte("---\ntype: [\n"))
		expect(t, runBinary(t, binary, "", "compare", "--dir", dir, "--approved", approved), result{
			code: 2,
			stderr: "spec-check: " + filepath.Join(dir, "broken.yaml") + ":2: -: bad-yaml: line 2: did not find expected node content\n" +
				"spec-check: " + dir + noComparison,
		})
	})
	// compare compares files whose only problems of Parse keep each node (specgraph.KeepsEachNode),
	// each file in its printed form: a value that the schema refuses and the node keeps, and an empty
	// file. Beside each, a text that the approved spec never held still fails the comparison (R2), so
	// a compare that passed whenever such a problem stands fails here.
	t.Run("compare fails on a text never approved beside a problem of Parse that keeps each node", func(t *testing.T) {
		t.Parallel()
		approved := fixture(t, "compare", "approved-text", "approved.json")
		const statement = "ACME keeps each order that a customer sends."
		for _, row := range []struct {
			name, old, new, extra string
		}{
			{"a subject that the schema refuses", "subject: code\n", "subject: the logs\n", ""},
			{"an empty file beside the files", "", "", "empty.yaml"},
		} {
			t.Run(row.name, func(t *testing.T) {
				t.Parallel()
				dir := copyOf(t, fixture(t, "compare", "approved-text", "spec"))
				orders := filepath.Join(dir, "orders.yaml")
				content, err := os.ReadFile(orders)
				if err != nil {
					t.Fatal(err)
				}
				planted := bytes.Replace(content, []byte(row.old), []byte(row.new), 1)
				planted = bytes.Replace(planted, []byte(statement), []byte("ACME keeps each order that a client sends."), 1)
				if row.old != "" && bytes.Equal(planted, bytes.Replace(content, []byte(statement), []byte("ACME keeps each order that a client sends."), 1)) {
					t.Fatal("the plant changed nothing")
				}
				writeFile(t, orders, planted)
				if row.extra != "" {
					writeFile(t, filepath.Join(dir, row.extra), nil)
				}
				got := runBinary(t, binary, "", "compare", "--dir", dir, "--approved", approved)
				if got.code != 1 || !strings.Contains(got.stderr, "acme-keeps-each-order: unapproved-text") {
					t.Errorf("exit %d, stdout %q, stderr %q; want exit 1 and a line naming acme-keeps-each-order: unapproved-text", got.code, got.stdout, got.stderr)
				}
			})
		}
	})
	t.Run("compare refuses a directory that holds no spec file, and names --allow-empty", func(t *testing.T) {
		t.Parallel()
		empty, missing := t.TempDir(), filepath.Join(t.TempDir(), "missing")
		approved := fixture(t, "compare", "approved-text", "approved.json")
		expect(t, runBinary(t, binary, "", "compare", "--dir", empty, "--approved", approved), result{
			code:   2,
			stderr: "spec-check: " + empty + ": holds no spec file, and compare reads it as the empty tree only with --allow-empty\n",
		})
		expect(t, runBinary(t, binary, "", "compare", "--dir", missing, "--approved", approved), result{
			code:   2,
			stderr: "spec-check: " + missing + ": does not exist, and compare reads it as the empty tree only with --allow-empty\n",
		})
	})
	// R2: "A revert of a change that added a statement takes it out of the files." A revert of the
	// change that added the only spec file leaves no directory, since git keeps no empty directory,
	// and a checkout can hold an empty one. With --allow-empty, compare reads each as the empty tree:
	// each approved node is not in the files.
	t.Run("compare --allow-empty reads the directory that a revert of the last spec file leaves as the empty tree", func(t *testing.T) {
		t.Parallel()
		empty, missing := t.TempDir(), filepath.Join(t.TempDir(), ".dispatch", "spec")
		approved := fixture(t, "compare", "approved-text", "approved.json")
		notInFiles := ""
		for _, node := range []string{
			"statement acme-keeps-each-order", "statement an-order-survives-a-restart", "statement orders-reach-billing-only-through-the-interface",
			"check a-restart-keeps-each-order", "check no-order-skips-the-interface",
			"component billing", "component orders", "interface orders-to-billing", "component shipping", "term order",
		} {
			notInFiles += "spec-check: " + node + ": not in the files\n"
		}
		held := ": the approved spec held every text that the files hold; 0 of 3 approved statements are in the files at their current text\n"
		expect(t, runBinary(t, binary, "", "compare", "--allow-empty", "--dir", missing, "--approved", approved), result{
			stdout: "spec-check: " + missing + held + notInFiles,
			stderr: "spec-check: " + missing + ": does not exist, so compare reads it as the empty tree\n",
		})
		expect(t, runBinary(t, binary, "", "compare", "--allow-empty", "--dir", empty, "--approved", approved), result{
			stdout: "spec-check: " + empty + held + notInFiles,
			stderr: "spec-check: " + empty + ": holds no spec file, so compare reads it as the empty tree\n",
		})
		// Over files, --allow-empty changes nothing.
		dir := fixture(t, "compare", "approved-text", "spec")
		expect(t, runBinary(t, binary, "", "compare", "--allow-empty", "--dir", dir, "--approved", approved), result{stdout: passLine(dir, "3 of 3")})
	})
	t.Run("compare refuses an approved spec it cannot read", func(t *testing.T) {
		t.Parallel()
		dir := fixture(t, "compare", "approved-text", "spec")
		missing := filepath.Join(t.TempDir(), "approved.json")
		expect(t, runBinary(t, binary, "", "compare", "--dir", dir, "--approved", missing), result{
			code:   2,
			stderr: "spec-check: " + missing + ": cannot be read: open " + missing + ": no such file or directory\n",
		})
		bad := filepath.Join(t.TempDir(), "approved.json")
		writeFile(t, bad, []byte(`{"nodes":[{"id":"order","versions":[]}]}`))
		expect(t, runBinary(t, binary, "", "compare", "--dir", dir, "--approved", bad), result{
			code:   2,
			stderr: "spec-check: " + bad + `: specgraph: approved node "order": it has no version` + "\n",
		})
		// A value of another type than the form needs is named by its place and the form, in the
		// words of the package, for compare and print alike.
		wrong := filepath.Join(t.TempDir(), "approved.json")
		writeFile(t, wrong, []byte(`{"nodes":[{"id":"order","versions":[{"version":"1","at":"2026-11-10T00:00:00Z","document":""}]}]}`))
		wrongType := "spec-check: " + wrong + ": specgraph: approved spec: it is not in the form " + approvedForm + `: the value at "/nodes/0/versions/0/version" is of another type` + "\n"
		expect(t, runBinary(t, binary, "", "compare", "--dir", dir, "--approved", wrong), result{code: 2, stderr: wrongType})
		expect(t, runBinary(t, binary, "", "print", "--approved", wrong, "--id", "order", "--dir", t.TempDir()), result{code: 2, stderr: wrongType})
	})
	// Each CI job of this repository is a GitHub Actions job, and GitHub sets GITHUB_ACTIONS=true in
	// each step. An agent's shell sets CI=true, and an agent's step is no CI job (R12), so CI alone
	// compares.
	t.Run("compare does not run in a CI job", func(t *testing.T) {
		t.Parallel()
		dir, approved := fixture(t, "compare", "approved-text", "spec"), fixture(t, "compare", "approved-text", "approved.json")
		for _, env := range [][]string{{"GITHUB_ACTIONS=true"}, {"GITHUB_ACTIONS=true", "CI=true"}} {
			expect(t, runBinaryIn(t, binary, "", env, "compare", "--dir", dir, "--approved", approved), result{code: 2, stderr: ciRefusal})
		}
		// Only GITHUB_ACTIONS=true names a CI job: the comparison runs with CI=true alone, and where
		// GITHUB_ACTIONS holds another value.
		for _, env := range [][]string{{"CI=true"}, {"GITHUB_ACTIONS=false", "CI=true"}} {
			expect(t, runBinaryIn(t, binary, "", env, "compare", "--dir", dir, "--approved", approved), result{stdout: passLine(dir, "3 of 3")})
		}
	})
	// The refusal is the first thing compare does, so a CI job gets it, and nothing more, whatever
	// the arguments: none, -h, a flag it does not take, files that do not exist, or an argument
	// besides the flags.
	t.Run("compare refuses in a CI job before it reads an argument", func(t *testing.T) {
		t.Parallel()
		missing := filepath.Join(t.TempDir(), "missing")
		for _, args := range [][]string{
			{"compare"},
			{"compare", "-h"},
			{"compare", "--bogus"},
			{"compare", "--dir", missing, "--approved", missing},
			{"compare", "--dir", fixture(t, "compare", "approved-text", "spec"), "--approved", missing, "extra"},
		} {
			expect(t, runBinaryIn(t, binary, "", []string{"GITHUB_ACTIONS=true"}, args...), result{code: 2, stderr: ciRefusal})
		}
	})
	// The refusal reads the command's own environment, so a step that starts the command behind a
	// boundary that drops the runner's environment compares. sudo -n with its default env_reset and
	// a container that docker run starts without -e GITHUB_ACTIONS both give the command no
	// GITHUB_ACTIONS. This row holds that fact with env -i, the same boundary, since a test cannot
	// count on docker, an image or sudo: the step's own environment holds GITHUB_ACTIONS=true, and
	// env -i starts the command with no variable. A change that makes the command refuse behind env
	// -i changes this row; a refusal keyed to sudo or to a container alone would not, so the row
	// pins env -i and not those two.
	t.Run("compare compares behind a boundary that drops the environment", func(t *testing.T) {
		t.Parallel()
		envPath, err := exec.LookPath("env")
		if err != nil {
			t.Fatalf("no env on PATH: %v", err)
		}
		dir, approved := fixture(t, "compare", "approved-text", "spec"), fixture(t, "compare", "approved-text", "approved.json")
		expect(t, runBinaryIn(t, envPath, "", []string{"GITHUB_ACTIONS=true", "CI=true"}, "-i", binary, "compare", "--dir", dir, "--approved", approved), result{
			stdout: passLine(dir, "3 of 3"),
		})
		// The same step without the boundary is refused, so the pass above is the boundary's.
		got := runBinaryIn(t, envPath, "", []string{"GITHUB_ACTIONS=true", "CI=true"}, binary, "compare", "--dir", dir, "--approved", approved)
		if got.code != 2 || !strings.Contains(got.stderr, "compare does not run in a CI job") {
			t.Errorf("compare in the step without env -i: exit %d, stderr %q; want the refusal", got.code, got.stderr)
		}
	})
	t.Run("every other subcommand runs in a CI job", func(t *testing.T) {
		t.Parallel()
		ci := []string{"GITHUB_ACTIONS=true", "CI=true"}
		approved := fixture(t, "compare", "approved-text", "approved.json")
		if got := runBinaryIn(t, binary, "", ci, "print", "--approved", approved, "--id", "order", "--dir", t.TempDir()); got.code != 0 || got.stderr != "" {
			t.Errorf("print in a CI job: exit %d, stderr %q; want exit 0", got.code, got.stderr)
		}
		clean := ciFiles(t, "clean")
		expect(t, runBinaryIn(t, binary, "", ci, "guard", "--root", clean), result{stdout: "spec-check: " + clean + ": no line of 4 CI files names the command that compares the files\n"})
		valid := fixture(t, "trees", "valid")
		if got := runBinaryIn(t, binary, "", ci, "tree", "--dir", valid); got.code != 0 {
			t.Errorf("tree in a CI job: exit %d, stderr %q; want exit 0", got.code, got.stderr)
		}
	})
}

// compare refuses the files for each problem of Parse whose Effect is not specgraph.KeepsEachNode
// (refusedProblems): a problem that loses a node, and a problem with no Effect, which no kind of
// Parse gives today, so such a problem fails closed. A problem that keeps each node is tree's to
// judge.
func TestCompareRefusesAProblemWithNoEffect(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name    string
		effect  specgraph.Effect
		refused bool
	}{
		{"a problem with no Effect", 0, true},
		{"a problem that loses a node", specgraph.LosesNode, true},
		{"a problem that keeps each node", specgraph.KeepsEachNode, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			problem := specgraph.Problem{Pos: specgraph.Position{File: "orders.yaml", Line: 3}, Node: "-", Rule: "bad-yaml", Detail: "bad-yaml: yaml: line 3: mapping values are not allowed in this context", Effect: row.effect}
			var want []specgraph.Problem
			if row.refused {
				want = []specgraph.Problem{problem}
			}
			if got := refusedProblems([]specgraph.Problem{problem}); !slices.Equal(got, want) {
				t.Errorf("refusedProblems([%+v]) = %+v, want %+v", problem, got, want)
			}
		})
	}
}

// passLine is the first line of a comparison that passes over dir, with held "<m> of <n>": the
// approved spec held every text that the files hold, which may be an earlier text or a text of a
// removed node, so the line does not say that each text is approved now.
func passLine(dir, held string) string {
	return "spec-check: " + dir + ": the approved spec held every text that the files hold; " + held + " approved statements are in the files at their current text\n"
}

// approvedOf is the approved spec of the compare fixture name, as compare's readApproved reads it.
func approvedOf(t *testing.T, name string) specgraph.Approved {
	t.Helper()
	var stderr strings.Builder
	_, a, ok := readApproved(&stderr, fixture(t, "compare", name, "approved.json"))
	if !ok {
		t.Fatal(stderr.String())
	}
	return a
}

// approvedCopy is the path of a copy of the approved.json of the compare fixture name, with edit
// applied to each node before specgraph.WriteApproved writes the copy.
func approvedCopy(t *testing.T, name string, edit func(n *specgraph.ApprovedNode)) string {
	t.Helper()
	a := approvedOf(t, name)
	for id, n := range a.Nodes {
		edit(&n)
		a.Nodes[id] = n
	}
	var changed bytes.Buffer
	if err := specgraph.WriteApproved(&changed, a); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "approved.json")
	writeFile(t, path, changed.Bytes())
	return path
}

// editApproved decodes the approved.json at path as the approved nodes of specgraph, lets edit
// change, add or take out nodes, and writes back the nodes that edit returns. A test of a file that
// is not valid edits its bytes instead (replaceIn).
func editApproved(t *testing.T, path string, edit func(nodes []specgraph.ApprovedNode) []specgraph.ApprovedNode) {
	t.Helper()
	var file specgraph.ApprovedFile
	readJSONFile(t, path, &file)
	file.Nodes = edit(file.Nodes)
	writeJSONFile(t, path, file)
}

// compare and print read approved.json through the one strict JSON reader, as state does: a member
// written in other letters or given two times, and a byte that is not UTF-8, are exit 2 with one
// line that names the file.
func TestTheApprovedSpecIsReadStrictly(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	const first = `"removed": false,`
	for _, row := range []struct{ name, old, new, fault string }{
		{"a field name in capitals", first, `"REMOVED": false,`, `member "/nodes/0/REMOVED" is not in the format`},
		{"one field two times, in two cases", first, `"removed": false, "Removed": true,`, `member "/nodes/0/Removed" is not in the format`},
		{"one field two times", first, `"removed": false, "removed": true,`, `member "/nodes/0/removed" is in one object two times`},
		{"a byte that is not UTF-8", `"id": "acme-keeps-each-order"`, "\"id\": \"acme-keeps-each-\xfforder\"", "it is not UTF-8"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			fixtureDir := copyOf(t, fixture(t, "compare", "approved-text"))
			approved := filepath.Join(fixtureDir, "approved.json")
			replaceIn(t, approved, row.old, row.new)
			want := result{code: 2, stderr: "spec-check: " + approved + ": specgraph: approved spec: " + row.fault + "\n"}
			expect(t, runBinary(t, binary, "", "compare", "--dir", filepath.Join(fixtureDir, "spec"), "--approved", approved), want)
			expect(t, runBinary(t, binary, "", "print", "--approved", approved, "--id", "acme-keeps-each-order", "--dir", t.TempDir()), want)
		})
	}
}

// noComparison is the last line of compare for files that it does not compare, after the directory.
const noComparison = ": the files do not parse, or are not as print writes them, so no comparison was made\n"

// compare makes no comparison of a file that is not in its printed form (B3 and B4 of the review):
// the printed form is the one form that each YAML library reads alike, because the printer writes
// it, and two libraries can read another form as two texts, so a comparison of the tree that
// yaml.v3 reads from it could pass a text that the approved spec never held. The rows are the files
// of the review, each with the approved text that yaml.v3 reads from it as Print writes it: a plain
// date, as a name and as a line of a definition; a !!binary; a merge key; an alias; and each of
// U+2028, U+2029 and U+0085, which a YAML 1.1 library such as yaml.v3 reads as a line break and a
// YAML 1.2 library as a character, in a text in double quotes, in single quotes and in a block; and
// a text in other quotes, which the libraries read alike. tree refuses each file, as before, and
// compare exits 2, and its line names the way out, spec-check print. The control is a file in its
// printed form with a text that the approved spec never held: compare compares it.
func TestCompareMakesNoComparisonOfAFileThatIsNotPrinted(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	term := func(name, definition string) string {
		return "---\ntype: term\nid: a-day\n" + name + "\ndefinition:\n" + definition + "\n"
	}
	const day = "  - A day is a unit of time."
	// readsAs is the document that Print writes for the term that yaml.v3 reads from file.
	readsAs := func(t *testing.T, file string) string {
		t.Helper()
		tree, _ := specgraph.Parse(map[string][]byte{"terms.yaml": []byte(file)})
		if len(tree.Terms) != 1 {
			t.Fatalf("yaml.v3 reads %d terms from %q, want 1", len(tree.Terms), file)
		}
		document, err := tree.Terms[0].Print()
		if err != nil {
			t.Fatalf("Print of the term that yaml.v3 reads from %q: %v", file, err)
		}
		if document == file {
			t.Fatalf("%q is in its printed form, so the row is no file that print does not write", file)
		}
		return document
	}
	// row is one file and its approved text ("" for yaml.v3's reading of it, readsAs), and the
	// not-printed line that compare and tree print for it: line is got in the file, print writes
	// want there, and why names what differs. A row with no got takes any not-printed line of the
	// term that names the way out.
	type row struct {
		name, approved, file string
		line                 int
		got, want, why       string
	}
	rows := []row{
		{"a plain date as a name", term(`name: "2026-11-16T00:00:00Z"`, day), term("name: 2026-11-16", day), 4, "name: 2026-11-16\n", "name: \"2026-11-16T00:00:00Z\"\n", "another spelling"},
		{"a plain date as a line of a definition", term("name: day", `  - "2026-11-16T00:00:00Z"`), term("name: day", "  - 2026-11-16"), 6, "  - 2026-11-16\n", "  - \"2026-11-16T00:00:00Z\"\n", "another spelling"},
		{"a binary", term("name: work unit", day), term("name: !!binary d29yayB1bml0", day), 4, "name: !!binary d29yayB1bml0\n", "name: work unit\n", "another spelling"},
		{"a merge key", term("name: day", day), term("<<: {name: day}", day), 4, "<<: {name: day}\n", "name: day\n", "another order of keys"},
		{"an alias", term("name: day", "  - day"), term("name: &n day", "  - *n"), 4, "name: &n day\n", "name: day\n", "another spelling"},
		{"a text in other quotes", term("name: day", day), term("name: 'day'", day), 4, "name: 'day'\n", "name: day\n", "other quotes"},
	}
	for _, c := range []struct{ name, char string }{{"U+2028", "\u2028"}, {"U+2029", "\u2029"}, {"U+0085", "\u0085"}} {
		for _, style := range []struct{ name, file string }{
			{"double quotes", term(`name: "a`+c.char+`  day"`, day)},
			{"single quotes", term(`name: 'a`+c.char+`  day'`, day)},
			{"a block", term("name: day", "  - |-\n    A day"+c.char+"    is a unit of time.")},
		} {
			rows = append(rows, row{name: c.name + " in a text in " + style.name, file: style.file})
		}
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			if row.approved == "" {
				row.approved = readsAs(t, row.file)
			}
			approved := writeApproved(t, map[string]string{"a-day": row.approved})
			dir := t.TempDir()
			file := filepath.Join(dir, "terms.yaml")
			writeFile(t, file, []byte(row.file))
			compare := runBinary(t, binary, "", "compare", "--dir", dir, "--approved", approved)
			var problem string
			if row.got != "" {
				problem = "spec-check: " + file + ":" + strconv.Itoa(row.line) + ": a-day: " +
					fmt.Sprintf("not-printed: line %d is %q, and print writes %q there (%s); spec-check print writes each node in that form\n", row.line, row.got, row.want, row.why)
				expect(t, compare, result{code: 2, stderr: problem + "spec-check: " + dir + noComparison})
			} else {
				refusal := regexp.MustCompile(`^spec-check: ` + regexp.QuoteMeta(file) + `:[0-9]+: a-day: not-printed: line [0-9]+ is .*; spec-check print writes each node in that form\n`)
				problem = refusal.FindString(compare.stderr)
				if compare.code != 2 || problem == "" || compare.stderr != problem+"spec-check: "+dir+noComparison || compare.stdout != "" {
					t.Errorf("compare: exit %d, stdout %q, stderr %q; want exit 2, a not-printed line that names spec-check print, and %q", compare.code, compare.stdout, compare.stderr, dir+noComparison)
				}
			}
			if got := runBinary(t, binary, "", "tree", "--dir", dir); got.code != 1 || problem == "" || !strings.Contains(got.stderr, problem) {
				t.Errorf("tree: exit %d, stderr %q, want exit 1 and the line %q", got.code, got.stderr, problem)
			}
		})
	}
	t.Run("the control: a file in its printed form, and compare compares it", func(t *testing.T) {
		t.Parallel()
		approved := writeApproved(t, map[string]string{"a-day": term("name: day", day)})
		dir := t.TempDir()
		file := filepath.Join(dir, "terms.yaml")
		writeFile(t, file, []byte(term("name: night", day)))
		expect(t, runBinary(t, binary, "", "compare", "--dir", dir, "--approved", approved), result{
			code:   1,
			stdout: "spec-check: " + dir + ": the comparison fails on 1 node\n",
			stderr: "spec-check: " + file + ":1: a-day: " + unapprovedText + "\n",
		})
	})
}

// withoutDocument is text, a spec file, with the document of the node id taken out, from its
// "---" line to the next.
func withoutDocument(t *testing.T, text, id string) string {
	t.Helper()
	var kept []string
	documents := splitDocuments(text)
	for _, document := range documents {
		if !strings.Contains(document, "\nid: "+id+"\n") {
			kept = append(kept, document)
		}
	}
	if len(kept) == len(documents) {
		t.Fatalf("the text holds no document of %s", id)
	}
	return strings.Join(kept, "")
}
