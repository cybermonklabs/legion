package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

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

// approvedForm is the form of approved.json, as compare and print name it before the place of a
// value of another type.
const approvedForm = `{"nodes":[{"id":"<id>","removed":<true or false>,"partly_checked":<true or false>,"versions":[{"version":<whole number from 1>,"at":"<RFC 3339 time with an upper-case T and Z>","block":"<block>"}, …]}, …]}`

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
		{"one-word", "orders.md", 7, "an-order-survives-a-restart", unapprovedText},
		{"unapproved-statement", "orders.md", 29, "each-order-gets-one-bill", unapprovedID},
		{"check-text", "orders.md", 11, "a-restart-keeps-each-order", unapprovedText},
		{"component-purpose", "components.md", 7, "orders", unapprovedText},
		{"interface-joins", "components.md", 11, "orders-to-billing", unapprovedText},
		{"term-definition", "components.md", 19, "order", unapprovedText},
		{"removed-id-reused", "orders.md", 29, "an-order-is-paid-once", removedID},
		{"one-of-four", "orders.md", 29, "each-order-gets-one-bill", unapprovedText},
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
				n.Removed = true
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
		if !removed.Removed {
			t.Fatal("the fixture did not remove an-order-is-paid-once")
		}
		writeFile(t, filepath.Join(dir, "again.md"), []byte(removed.Versions[len(removed.Versions)-1].Block))
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
			"orders.md":     {"a-restart-keeps-each-order"},
			"components.md": {"shipping", "orders-to-billing", "order"},
		} {
			content, err := os.ReadFile(fixture(t, "compare", "approved-text", "spec", name))
			if err != nil {
				t.Fatal(err)
			}
			text := string(content)
			for _, id := range ids {
				text = withoutBlock(t, text, id)
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
			// Version 2 adds one word to the first line of the text of version 1.
			n.Versions = append(n.Versions, specgraph.ApprovedVersion{
				Version: 2,
				At:      time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC),
				Block:   strings.Replace(n.Versions[0].Block, ".\n", " now.\n", 1),
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
		only := func(keep func(id, block string) bool) string {
			return approvedCopy(t, "approved-text", func(n *specgraph.ApprovedNode) {
				if !keep(n.ID, n.Versions[len(n.Versions)-1].Block) {
					n.Removed = true
				}
			})
		}
		oneStatement := only(func(id, _ string) bool { return id == "acme-keeps-each-order" })
		threeStatements := only(func(_, block string) bool { return strings.HasPrefix(block, ":::requirement{") })
		empty, one := t.TempDir(), t.TempDir()
		writeFile(t, filepath.Join(empty, "spec.md"), nil)
		writeFile(t, filepath.Join(one, "spec.md"), []byte(":::requirement{#acme-keeps-each-order kind=\"behavior\"}\nACME keeps each order that a customer sends.\n:::\n"))
		held := ": the approved spec held every text that the files hold; "
		expect(t, runBinary(t, binary, "", "compare", "--dir", empty, "--approved", oneStatement), result{
			stdout: "spec-check: " + empty + held + "0 of 1 approved statements are in the files at their current text\n" +
				"spec-check: statement acme-keeps-each-order: not in the files\n",
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
		for _, name := range []string{"orders.md", "components.md"} {
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
			stderr: "spec-check: " + filepath.Join(dir, "components.md") + ":7: orders: " + unapprovedText + "\n" +
				"spec-check: " + filepath.Join(dir, "orders.md") + ":7: an-order-survives-a-restart: " + unapprovedText + "\n",
		})
	})
	t.Run("compare makes no comparison of files that do not parse", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "a.md"), []byte("A line outside a block.\n"))
		expect(t, runBinary(t, binary, "", "compare", "--dir", dir, "--approved", fixture(t, "compare", "approved-text", "approved.json")), result{
			code: 2,
			stderr: "spec-check: " + filepath.Join(dir, "a.md") + ":1: -: text-outside-block: a spec file holds only blocks, headings and blank lines\n" +
				"spec-check: " + dir + ": the files do not parse, so no comparison was made\n",
		})
	})
	// A heading that names code breaks a rule of form, which tree judges, and leaves every node in
	// the tree (specgraph.KeepsEachNode), so compare compares the files and gives its own answer.
	// Beside a line that no block holds, the heading is no reason of the refusal, and only that line
	// is named.
	t.Run("compare compares files whose heading names code", func(t *testing.T) {
		t.Parallel()
		dir := copyOf(t, fixture(t, "compare", "approved-text", "spec"))
		orders := filepath.Join(dir, "orders.md")
		content, err := os.ReadFile(orders)
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, orders, append([]byte("# Orders and I/O\n\n"), content...))
		approved := fixture(t, "compare", "approved-text", "approved.json")
		expect(t, runBinary(t, binary, "", "compare", "--dir", dir, "--approved", approved), result{stdout: passLine(dir, "3 of 3")})

		lost := t.TempDir()
		writeFile(t, filepath.Join(lost, "a.md"), []byte("# Orders and I/O\n\nA line outside a block.\n"))
		expect(t, runBinary(t, binary, "", "compare", "--dir", lost, "--approved", approved), result{
			code: 2,
			stderr: "spec-check: " + filepath.Join(lost, "a.md") + ":3: -: text-outside-block: a spec file holds only blocks, headings and blank lines\n" +
				"spec-check: " + lost + ": the files do not parse, so no comparison was made\n",
		})
	})
	// compare compares files whose only problems of Parse keep each node (specgraph.KeepsEachNode):
	// a byte that Dispatch cannot store, a heading that names code, a claim on more than one line and
	// a claim of more than one sentence. Beside each, a text that the approved spec never held still
	// fails the comparison (R2), so a compare that passed whenever such a problem stands fails here.
	t.Run("compare fails on a text never approved beside a problem of Parse that keeps each node", func(t *testing.T) {
		t.Parallel()
		approved := fixture(t, "compare", "approved-text", "approved.json")
		const statement = "ACME keeps each order that a customer sends."
		const claim = "Claim: The order service reads each stored order after it starts again."
		for _, row := range []struct {
			name, old, new, named string
		}{
			{"bad-encoding in a heading, one word changed in a statement", "# Orders\n", "# Orders \xff\n", "acme-keeps-each-order: unapproved-text"},
			{"a heading that names code, one word changed in a statement", "# Orders\n", "# Orders and I/O\n", "acme-keeps-each-order: unapproved-text"},
			{"a claim on two lines", claim + "\n", claim + "\nIt also deletes each order that it reads.\n", "a-restart-keeps-each-order: unapproved-text"},
			{"a claim of two sentences", claim, claim + " It then deletes it.", "a-restart-keeps-each-order: unapproved-text"},
		} {
			t.Run(row.name, func(t *testing.T) {
				t.Parallel()
				dir := copyOf(t, fixture(t, "compare", "approved-text", "spec"))
				orders := filepath.Join(dir, "orders.md")
				content, err := os.ReadFile(orders)
				if err != nil {
					t.Fatal(err)
				}
				planted := bytes.Replace(content, []byte(row.old), []byte(row.new), 1)
				if strings.HasSuffix(row.name, "in a statement") {
					planted = bytes.Replace(planted, []byte(statement), []byte("ACME keeps each order that a client sends."), 1)
				}
				if bytes.Equal(planted, content) {
					t.Fatal("the plant changed nothing")
				}
				writeFile(t, orders, planted)
				got := runBinary(t, binary, "", "compare", "--dir", dir, "--approved", approved)
				if got.code != 1 || !strings.Contains(got.stderr, row.named) {
					t.Errorf("exit %d, stdout %q, stderr %q; want exit 1 and a line naming %q", got.code, got.stdout, got.stderr, row.named)
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
		writeFile(t, wrong, []byte(`{"nodes":[{"id":"order","versions":[{"version":"1","at":"2026-11-10T00:00:00Z","block":""}]}]}`))
		wrongType := "spec-check: " + wrong + ": specgraph: approved spec: it is not in the form " + approvedForm + `: the value at "/nodes/0/versions/0/version" is of another type` + "\n"
		expect(t, runBinary(t, binary, "", "compare", "--dir", dir, "--approved", wrong), result{code: 2, stderr: wrongType})
		expect(t, runBinary(t, binary, "", "print", "--approved", wrong, "--id", "order"), result{code: 2, stderr: wrongType})
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
		if got := runBinaryIn(t, binary, "", ci, "print", "--approved", approved, "--id", "order"); got.code != 0 || got.stderr != "" {
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
			problem := specgraph.Problem{Pos: specgraph.Position{File: "orders.md", Line: 3}, Node: "-", Rule: "text-outside-block", Detail: "text-outside-block: a line outside a block", Effect: row.effect}
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

// withoutBlock is text with the block of the node id taken out, from its opening line to its
// closing ":::" line.
func withoutBlock(t *testing.T, text, id string) string {
	t.Helper()
	lines := strings.SplitAfter(text, "\n")
	start := slices.IndexFunc(lines, func(line string) bool {
		return strings.HasPrefix(line, ":::") && (strings.Contains(line, "{#"+id+" ") || strings.Contains(line, "{#"+id+"}"))
	})
	if start < 0 {
		t.Fatalf("the text holds no block of %s", id)
	}
	end := slices.IndexFunc(lines[start+1:], func(line string) bool { return strings.TrimRight(line, "\r\n") == ":::" })
	if end < 0 {
		t.Fatalf("the block of %s has no closing line", id)
	}
	return strings.Join(slices.Delete(lines, start, start+end+2), "")
}
