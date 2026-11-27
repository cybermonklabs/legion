package main

//ci:reads packages/envoy/internal/dispatch/specgraph/testdata/** fixture joins ../../internal/dispatch/specgraph/testdata, which TestTheBuiltBinary lists and its built binary reads

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/specgraph"
)

func TestMain(m *testing.M) { os.Exit(cmdtest.Main(m)) }

// noList is the line tree writes after the directory when it reads no --retired list: the
// instrument says what it did not read.
const noList = ": no --retired list, so no id is checked against the ids the approved spec removed\n"

// plantedFaults is each fixture tree of specgraph's TestEachPlantedFaultIsNamed with the one
// problem the command must name, and the counts of the blocks it holds. The counts come from the
// opening lines of each fixture, not from the command. The retired-id tree is its own row of
// TestTheBuiltBinary, since it needs --retired.
var plantedFaults = []struct {
	dir, file    string
	line         int
	node, detail string
	counts       string
}{
	{"about-a-missing-component", "spec.md", 1, "a-claim-survives-a-restart", `missing-target: about names "legion", which the files do not hold`, "1 statement, 1 check, 0 components, 0 interfaces, 0 terms"},
	{"bad-id", "spec.md", 1, "step-3", `bad-id: "step-3" is not an id: words of lower-case ASCII letters and digits joined by single hyphens, with no word that states a position`, "1 statement, 1 check, 0 components, 0 interfaces, 0 terms"},
	{"check-serves-two-statements", "spec.md", 21, "each-restart-keeps-the-claim", "check-serves: a check serves exactly one statement of the files", "2 statements, 3 checks, 0 components, 0 interfaces, 0 terms"},
	{"check-subject-not-allowed", "spec.md", 5, "a-restart-keeps-the-claim", `bad-subject: "the logs" is not code, production or graph`, "1 statement, 1 check, 0 components, 0 interfaces, 0 terms"},
	{"check-without-failing-example", "spec.md", 5, "a-restart-keeps-the-claim", "no-failing-example: the check has no failing example", "1 statement, 1 check, 0 components, 0 interfaces, 0 terms"},
	{"claim-names-a-path", "spec.md", 5, "a-restart-keeps-the-claim", "code-in-text: claim names code: cmd/listener/main.go", "1 statement, 1 check, 0 components, 0 interfaces, 0 terms"},
	{"component-names-a-package", "spec.md", 1, "dispatch", "code-in-text: purpose names code: internal/dispatch/pmdoc", "0 statements, 0 checks, 1 component, 0 interfaces, 0 terms"},
	{"component-no-purpose", "spec.md", 1, "dispatch", "bad-component: a component has a name and one sentence of purpose", "0 statements, 0 checks, 1 component, 0 interfaces, 0 terms"},
	{"doubled-id", "b.md", 1, "a-claim-survives-a-restart", "doubled-id: also at a.md:1", "2 statements, 2 checks, 0 components, 0 interfaces, 0 terms"},
	{"go-selector-two-words-missing", "spec.md", 5, "a-restart-keeps-the-claim", `bad-selector: a go selector is "<import path> <TopLevelTestName>"`, "1 statement, 1 check, 0 components, 0 interfaces, 0 terms"},
	{"interface-joins-a-missing-component", "spec.md", 5, "legion-to-dispatch", `bad-interface: joins "legion", which is no component`, "0 statements, 0 checks, 1 component, 1 interface, 0 terms"},
	{"interface-joins-one", "spec.md", 9, "legion-to-dispatch", "bad-interface: an interface joins two or more components", "0 statements, 0 checks, 2 components, 1 interface, 0 terms"},
	{"leaf-without-check", "spec.md", 1, "a-claim-survives-a-restart", "leaf-without-check: no statement is under it and no check serves it", "1 statement, 0 checks, 0 components, 0 interfaces, 0 terms"},
	{"missing-parent", "spec.md", 1, "a-claim-survives-a-restart", `no-parent: the files hold no statement "legion-keeps-each-claim"`, "1 statement, 1 check, 0 components, 0 interfaces, 0 terms"},
	{"parent-and-serves-loop", "spec.md", 1, "a-claim-survives-a-restart", "loop: a-claim-survives-a-restart -[serves]-> legion-keeps-each-claim -[parent]-> a-claim-survives-a-restart", "2 statements, 1 check, 0 components, 0 interfaces, 0 terms"},
	{"parent-loop", "spec.md", 1, "a-claim-survives-a-restart", "loop: a-claim-survives-a-restart -[parent]-> legion-keeps-each-claim -[parent]-> a-claim-survives-a-restart", "2 statements, 0 checks, 0 components, 0 interfaces, 0 terms"},
	{"program-check-without-job", "spec.md", 5, "a-restart-keeps-the-claim", "no-job: a program check names the job that runs its test", "1 statement, 1 check, 0 components, 0 interfaces, 0 terms"},
	{"program-check-without-test-part", "spec.md", 5, "a-restart-keeps-the-claim", "no-test: a program check names its runner and its selector", "1 statement, 1 check, 0 components, 0 interfaces, 0 terms"},
	{"program-procedure-names-a-test", "spec.md", 5, "a-restart-keeps-the-claim", "code-in-text: procedure names code: TestARestart_LosesNoClaim", "1 statement, 1 check, 0 components, 0 interfaces, 0 terms"},
	{"query-check-with-test-part", "spec.md", 5, "each-claim-is-in-the-graph", "test-on-non-program: a query check names no runner, selector or job", "1 statement, 1 check, 0 components, 0 interfaces, 0 terms"},
	{"statement-names-a-package", "spec.md", 1, "a-document-keeps-its-blocks", "code-in-text: text names code: internal/dispatch/pmdoc", "1 statement, 1 check, 0 components, 0 interfaces, 0 terms"},
	{"statement-without-kind", "spec.md", 1, "a-claim-survives-a-restart", `bad-kind: "" is not behavior, structure or property`, "1 statement, 1 check, 0 components, 0 interfaces, 0 terms"},
	{"structure-statement-without-about", "spec.md", 1, "legion-reads-issues-only-through-dispatch", "no-about: a statement of kind structure names its components or interfaces", "1 statement, 1 check, 0 components, 0 interfaces, 0 terms"},
}

// TestTheBuiltBinary builds the command once and runs it as a process over specgraph's fixture
// trees, each row asserting the exit status and both streams byte for byte.
func TestTheBuiltBinary(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	valid := fixture(t, "trees", "valid")
	validLine := func(dir string) string {
		return "spec-check: " + dir + ": 3 statements, 4 checks, 2 components, 1 interface, 1 term; the tree is valid\n"
	}

	t.Run("tree names each planted fault with its file, line, node and rule", func(t *testing.T) {
		t.Parallel()
		entries, err := os.ReadDir(fixture(t, "trees"))
		if err != nil {
			t.Fatal(err)
		}
		covered := map[string]bool{"valid": true, "program-check-test-part": true, "retired-id": true}
		for _, row := range plantedFaults {
			covered[row.dir] = true
			t.Run(row.dir, func(t *testing.T) {
				t.Parallel()
				dir := fixture(t, "trees", row.dir)
				expect(t, runBinary(t, binary, "", "tree", "--dir", dir), result{
					code:   1,
					stdout: "spec-check: " + dir + ": " + row.counts + "; the tree is refused\n",
					stderr: "spec-check: " + dir + noList + "spec-check: " + filepath.Join(dir, row.file) + ":" + strconv.Itoa(row.line) + ": " + row.node + ": " + row.detail + "\n",
				})
			})
		}
		for _, entry := range entries {
			if entry.IsDir() && !covered[entry.Name()] {
				t.Errorf("testdata/trees/%s is in no row", entry.Name())
			}
		}
	})
	t.Run("tree over the valid tree exits 0", func(t *testing.T) {
		t.Parallel()
		expect(t, runBinary(t, binary, "", "tree", "--dir", valid), result{stdout: validLine(valid), stderr: "spec-check: " + valid + noList})
	})
	t.Run("tree over a program check with its test part exits 0", func(t *testing.T) {
		t.Parallel()
		dir := fixture(t, "trees", "program-check-test-part")
		expect(t, runBinary(t, binary, "", "tree", "--dir", dir), result{
			stdout: "spec-check: " + dir + ": 1 statement, 1 check, 0 components, 0 interfaces, 0 terms; the tree is valid\n",
			stderr: "spec-check: " + dir + noList,
		})
	})
	t.Run("tree over a CRLF copy of the valid tree prints the same count line", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		for _, name := range []string{"claims.md", "interfaces.md"} {
			content, err := os.ReadFile(filepath.Join(valid, name))
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(dir, name), []byte(strings.ReplaceAll(string(content), "\n", "\r\n")))
		}
		expect(t, runBinary(t, binary, "", "tree", "--dir", dir), result{stdout: validLine(dir), stderr: "spec-check: " + dir + noList})
	})
	t.Run("tree refuses a file that is not UTF-8 or holds a NUL byte, naming the file and the line", func(t *testing.T) {
		t.Parallel()
		for name, content := range map[string]string{
			"Latin-1": ":::term{#cafe-term name=\"cafe\"}\nA caf\xe9 is a place where a session waits.\n:::\n",
			"NUL":     ":::term{#nul-term name=\"nul term\"}\nA nul\x00term is a word.\n:::\n",
		} {
			dir := copyOf(t, valid)
			writeFile(t, filepath.Join(dir, "bytes.md"), []byte(content))
			got := runBinary(t, binary, "", "tree", "--dir", dir)
			if want := (result{
				code:   1,
				stdout: "spec-check: " + dir + ": 3 statements, 4 checks, 2 components, 1 interface, 2 terms; the tree is refused\n",
				stderr: "spec-check: " + dir + noList + "spec-check: " + filepath.Join(dir, "bytes.md") + ":2: -: bad-encoding: a spec file is UTF-8 text with no NUL byte\n",
			}); got != want {
				t.Errorf("%s: exit %d, stdout %q, stderr %q\nwant exit %d, stdout %q, stderr %q", name, got.code, got.stdout, got.stderr, want.code, want.stdout, want.stderr)
			}
		}
	})
	t.Run("tree with --retired names a retired id", func(t *testing.T) {
		t.Parallel()
		dir := fixture(t, "trees", "retired-id")
		retired := filepath.Join(t.TempDir(), "retired.txt")
		writeFile(t, retired, []byte("# The ids the approved spec removed.\n\n  gone-statement  \nanother-gone-statement\n"))
		expect(t, runBinary(t, binary, "", "tree", "--dir", dir, "--retired", retired), result{
			code:   1,
			stdout: "spec-check: " + dir + ": 1 statement, 1 check, 0 components, 0 interfaces, 0 terms; the tree is refused\n",
			stderr: "spec-check: " + filepath.Join(dir, "spec.md") + ":1: gone-statement: retired-id: the approved spec removed this id\n",
		})
	})
	t.Run("tree without --retired says so and checks no id", func(t *testing.T) {
		t.Parallel()
		dir := fixture(t, "trees", "retired-id")
		expect(t, runBinary(t, binary, "", "tree", "--dir", dir), result{
			stdout: "spec-check: " + dir + ": 1 statement, 1 check, 0 components, 0 interfaces, 0 terms; the tree is valid\n",
			stderr: "spec-check: " + dir + noList,
		})
	})
	t.Run("tree with a --retired file it cannot read exits 2", func(t *testing.T) {
		t.Parallel()
		missing := filepath.Join(t.TempDir(), "retired.txt")
		got := runBinary(t, binary, "", "tree", "--dir", valid, "--retired", missing)
		want := "spec-check: " + missing + ": cannot be read: open " + missing + ": no such file or directory\n"
		expect(t, got, result{code: 2, stderr: want})
	})
	t.Run("tree over an empty directory exits 2 without --allow-empty, and names --allow-empty", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		expect(t, runBinary(t, binary, "", "tree", "--dir", dir), result{
			code:   2,
			stderr: "spec-check: " + dir + ": holds no spec file, and tree reads it as the empty tree only with --allow-empty\n",
		})
	})
	t.Run("tree over an empty directory with --allow-empty is the empty tree", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		expect(t, runBinary(t, binary, "", "tree", "--allow-empty", "--dir", dir), result{
			stdout: "spec-check: " + dir + ": 0 statements, 0 checks, 0 components, 0 interfaces, 0 terms; the tree is empty\n",
			stderr: "spec-check: " + dir + noList,
		})
	})
	t.Run("tree over a missing directory exits 2 without --allow-empty, and names --allow-empty", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(t.TempDir(), "missing")
		expect(t, runBinary(t, binary, "", "tree", "--dir", dir), result{
			code:   2,
			stderr: "spec-check: " + dir + ": does not exist, and tree reads it as the empty tree only with --allow-empty\n",
		})
	})
	t.Run("tree over a missing directory with --allow-empty is the empty tree", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(t.TempDir(), "missing")
		expect(t, runBinary(t, binary, "", "tree", "--allow-empty", "--dir", dir), result{
			stdout: "spec-check: " + dir + ": 0 statements, 0 checks, 0 components, 0 interfaces, 0 terms; the tree is empty\n",
			stderr: "spec-check: " + dir + noList,
		})
	})
	t.Run("tree warns of each entry that is not a spec file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		for _, name := range []string{"claims.md", "interfaces.md"} {
			content, err := os.ReadFile(filepath.Join(valid, name))
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(dir, name), content)
		}
		writeFile(t, filepath.Join(dir, "notes.txt"), []byte("Not a spec file.\n"))
		if err := os.Mkdir(filepath.Join(dir, "nested.md"), 0o755); err != nil {
			t.Fatal(err)
		}
		expect(t, runBinary(t, binary, "", "tree", "--dir", dir), result{
			stdout: validLine(dir),
			stderr: "spec-check: warning: " + filepath.Join(dir, "nested.md") + ": not a spec file (directory); the importer passes over it\n" +
				"spec-check: warning: " + filepath.Join(dir, "notes.txt") + ": not a spec file (file); the importer passes over it\n" +
				"spec-check: " + dir + noList,
		})
	})
	t.Run("tree names a relative directory by its absolute path", func(t *testing.T) {
		t.Parallel()
		parent := filepath.Dir(valid)
		expect(t, runBinary(t, binary, parent, "tree", "--dir", "valid"), result{stdout: validLine(valid), stderr: "spec-check: " + valid + noList})
	})
	t.Run("tree refuses a parse problem and a rule problem together, in file and line order", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "a.md"), []byte(":::requirement{#a-claim-survives-a-restart kind=\"behavior\"}\nLegion keeps each claim.\n:::\nA line outside a block.\n"))
		expect(t, runBinary(t, binary, "", "tree", "--dir", dir), result{
			code:   1,
			stdout: "spec-check: " + dir + ": 1 statement, 0 checks, 0 components, 0 interfaces, 0 terms; the tree is refused\n",
			stderr: "spec-check: " + dir + noList +
				"spec-check: " + filepath.Join(dir, "a.md") + ":1: a-claim-survives-a-restart: leaf-without-check: no statement is under it and no check serves it\n" +
				"spec-check: " + filepath.Join(dir, "a.md") + ":4: -: text-outside-block: a spec file holds only blocks, headings and blank lines\n",
		})
	})

	t.Run("changes lists no statement for a move to another file", func(t *testing.T) {
		t.Parallel()
		expect(t, runBinary(t, binary, "", "changes", "--base", fixture(t, "changes", "move", "base"), "--dir", fixture(t, "changes", "move", "head")),
			result{stdout: "spec-check: no statement changed\n"})
	})
	t.Run("changes lists the statement whose text gained one word", func(t *testing.T) {
		t.Parallel()
		expect(t, runBinary(t, binary, "", "changes", "--base", fixture(t, "changes", "one-word", "base"), "--dir", fixture(t, "changes", "one-word", "head")),
			result{stdout: "spec-check: a-claim-survives-a-restart: text\n"})
	})
	t.Run("changes lists each part that changed", func(t *testing.T) {
		t.Parallel()
		expect(t, runBinary(t, binary, "", "changes", "--base", fixture(t, "changes", "relations", "base"), "--dir", fixture(t, "changes", "relations", "head")),
			result{stdout: "spec-check: a-claim-survives-a-restart: relations\nspec-check: a-lost-claim-is-reported: text, parent\n"})
	})
	// Git keeps no empty directory, so a version with no spec file has no .dispatch/spec, and the
	// first change that adds a spec file, or the change that removes the last one, compares a tree
	// with a directory that does not exist.
	t.Run("changes reads a base that does not exist as the empty tree", func(t *testing.T) {
		t.Parallel()
		base := filepath.Join(t.TempDir(), "missing")
		expect(t, runBinary(t, binary, "", "changes", "--base", base, "--dir", valid), result{
			stdout: "spec-check: a-claim-survives-a-restart: added\nspec-check: legion-keeps-each-claim-in-dispatch: added\nspec-check: legion-reads-issues-only-through-dispatch: added\n",
			stderr: "spec-check: " + base + ": does not exist, so it is read as the empty tree\n",
		})
	})
	t.Run("changes reads a directory that does not exist as the empty tree", func(t *testing.T) {
		t.Parallel()
		head := filepath.Join(t.TempDir(), "missing")
		expect(t, runBinary(t, binary, "", "changes", "--base", valid, "--dir", head), result{
			stdout: "spec-check: a-claim-survives-a-restart: removed\nspec-check: legion-keeps-each-claim-in-dispatch: removed\nspec-check: legion-reads-issues-only-through-dispatch: removed\n",
			stderr: "spec-check: " + head + ": does not exist, so it is read as the empty tree\n",
		})
	})
	t.Run("changes reads an empty base directory as the empty tree", func(t *testing.T) {
		t.Parallel()
		expect(t, runBinary(t, binary, "", "changes", "--base", t.TempDir(), "--dir", valid), result{
			stdout: "spec-check: a-claim-survives-a-restart: added\nspec-check: legion-keeps-each-claim-in-dispatch: added\nspec-check: legion-reads-issues-only-through-dispatch: added\n",
		})
	})
	t.Run("changes exits 2 when a tree has a problem", func(t *testing.T) {
		t.Parallel()
		base := fixture(t, "trees", "leaf-without-check")
		expect(t, runBinary(t, binary, "", "changes", "--base", base, "--dir", valid), result{
			code:   2,
			stderr: "spec-check: " + filepath.Join(base, "spec.md") + ":1: a-claim-survives-a-restart: leaf-without-check: no statement is under it and no check serves it\n",
		})
	})

	t.Run("checks prints the program checks of a job", func(t *testing.T) {
		t.Parallel()
		tree, problems := specgraph.Parse(readDir(t, valid))
		if len(problems) != 0 {
			t.Fatal(problems)
		}
		atTheLimit, _ := tree.CheckByID("a-claim-at-the-time-limit-stays")
		restart, _ := tree.CheckByID("a-restart-keeps-the-claim")
		want := `[{"id":"a-claim-at-the-time-limit-stays","runner":"go","selector":"example.com/acme/daemon TestAClaimAtTheTimeLimitStays","job":"go-checks","procedure_sha256":"` + atTheLimit.Digest() + `"},` +
			`{"id":"a-restart-keeps-the-claim","runner":"go","selector":"example.com/acme/daemon TestARestartKeepsTheClaim","job":"go-checks","procedure_sha256":"` + restart.Digest() + `"}]` + "\n"
		expect(t, runBinary(t, binary, "", "checks", "--dir", valid, "--job", "go-checks"), result{stdout: want})
	})
	t.Run("checks prints [] for a job that no check names", func(t *testing.T) {
		t.Parallel()
		expect(t, runBinary(t, binary, "", "checks", "--dir", valid, "--job", "other"), result{stdout: "[]\n"})
	})
	t.Run("checks reads a directory that does not exist as the empty tree", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(t.TempDir(), "missing")
		expect(t, runBinary(t, binary, "", "checks", "--dir", dir, "--job", "go-checks"), result{
			stdout: "[]\n",
			stderr: "spec-check: " + dir + ": does not exist, so it is read as the empty tree\n",
		})
	})
	t.Run("checks exits 2 when the tree has a problem", func(t *testing.T) {
		t.Parallel()
		dir := fixture(t, "trees", "program-check-without-job")
		expect(t, runBinary(t, binary, "", "checks", "--dir", dir, "--job", "go-checks"), result{
			code:   2,
			stderr: "spec-check: " + filepath.Join(dir, "spec.md") + ":5: a-restart-keeps-the-claim: no-job: a program check names the job that runs its test\n",
		})
	})
	t.Run("checks writes no workflow command a selector holds", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "spec.md"), []byte(":::requirement{#a-claim-survives-a-restart kind=\"behavior\"}\nLegion keeps the claim of an issue after the daemon starts again.\n:::\n\n"+
			":::check{#a-restart-keeps-the-claim serves=\"a-claim-survives-a-restart\" subject=\"code\" procedure=\"program\" runner=\"bun\" selector=\"daemon.test.ts ##[error]forged\" job=\"bun-checks\"}\n"+
			"Claim: The daemon reads the claim of an issue after it starts again.\nProcedure: The program starts the daemon again and reads the claims.\nFailing example: A daemon that keeps its claims in memory only.\n:::\n"))
		got := runBinary(t, binary, "", "checks", "--dir", dir, "--job", "bun-checks")
		if got.code != 0 || got.stderr != "" || strings.Contains(got.stdout, "##[") {
			t.Fatalf("exit %d, stdout %q, stderr %q: want exit 0 and no \"##[\"", got.code, got.stdout, got.stderr)
		}
		var entries []struct{ ID, Selector string }
		if err := json.Unmarshal([]byte(got.stdout), &entries); err != nil || len(entries) != 1 || entries[0].Selector != "daemon.test.ts ##[error]forged" {
			t.Errorf("stdout %q reads as %+v, %v; want the selector as written", got.stdout, entries, err)
		}
	})

	t.Run("compare", func(t *testing.T) {
		t.Parallel()
		testCompare(t, binary)
	})
	t.Run("print", func(t *testing.T) {
		t.Parallel()
		testPrint(t, binary)
	})
	t.Run("guard", func(t *testing.T) {
		t.Parallel()
		testGuard(t, binary)
	})

	t.Run("a bad argument exits 2 with the line that names it and the usage", func(t *testing.T) {
		t.Parallel()
		approved := fixture(t, "compare", "approved-text", "approved.json")
		for _, row := range []struct {
			args []string
			line string
		}{
			{[]string{"tree"}, "tree: --dir is required"},
			{[]string{"tree", "--dir", valid, "extra"}, `tree: takes no argument but its flags, got "extra"`},
			{[]string{"tree", "--bogus"}, "tree: flag provided but not defined: -bogus"},
			{[]string{"changes", "--dir", valid}, "changes: --base is required"},
			{[]string{"checks", "--dir", valid}, "checks: --job is required"},
			{[]string{"compare", "--dir", valid}, "compare: --approved is required"},
			{[]string{"compare", "--approved", approved}, "compare: --dir is required"},
			{[]string{"print", "--approved", approved}, "print: --id is required"},
			{[]string{"print", "--id", "order"}, "print: --approved is required"},
			{[]string{"guard"}, "guard: --root is required"},
			{[]string{"guard", "--root", valid, "extra"}, `guard: takes no argument but its flags, got "extra"`},
			{[]string{"state"}, "state: --scenario is required"},
			{[]string{"state", "--scenario", valid, "extra"}, `state: takes no argument but its flags, got "extra"`},
		} {
			expect(t, runBinary(t, binary, "", row.args...), result{code: 2, stderr: "spec-check: " + row.line + "\n" + usage + "\n"})
		}
	})
}

// TestUsage runs the command with no subcommand and with one it does not know, each of which exits
// 2 with the usage, and with -h after a subcommand, which exits 0 with the usage. The usage names
// compare's --allow-empty, the flag a caller needs after a revert of the last spec file.
func TestUsage(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	expect(t, runBinary(t, binary, ""), result{code: 2, stderr: usage + "\n"})
	expect(t, runBinary(t, binary, "", "nope"), result{code: 2, stderr: "spec-check: unknown subcommand \"nope\"\n" + usage + "\n"})
	expect(t, runBinary(t, binary, "", "tree", "-h"), result{stderr: usage + "\n"})
	help := runBinary(t, binary, "", "compare", "-h")
	expect(t, help, result{stderr: usage + "\n"})
	line := "spec-check compare --dir <dir> --approved <approved.json> [--allow-empty]"
	if !slices.ContainsFunc(strings.Split(help.stderr, "\n"), func(l string) bool { return strings.TrimSpace(l) == line }) {
		t.Errorf("the usage %q holds no line %q", help.stderr, line)
	}
}

// result is what one run of the binary did.
type result struct {
	code           int
	stdout, stderr string
}

// runBinary runs binary with args in dir, the test's own directory when dir is empty, in the
// environment of childEnv with no variable added, and answers its exit status and output.
func runBinary(t *testing.T, binary, dir string, args ...string) result {
	t.Helper()
	return runBinaryIn(t, binary, dir, nil, args...)
}

// runBinaryIn is runBinary with the variables extra, each "NAME=value", added to the environment.
// In dir, PWD names dir made absolute, as a shell's cd sets it, and as os/exec sets it for a command
// with no Env of its own: the command prints a relative path made absolute against its working
// directory as PWD names it, and a PWD that named the test's own directory would make it print the
// path with each symbolic link of dir resolved, so that a row would pass or fail by how the
// checkout was reached.
func runBinaryIn(t *testing.T, binary, dir string, extra []string, args ...string) result {
	t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Dir = dir
	cmd.Env = childEnv(extra...)
	if dir != "" {
		pwd, err := filepath.Abs(dir)
		if err != nil {
			t.Fatal(err)
		}
		cmd.Env = append(slices.DeleteFunc(cmd.Env, func(v string) bool { return strings.HasPrefix(v, "PWD=") }), "PWD="+pwd)
	}
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatal(err)
	}
	return result{code: cmd.ProcessState.ExitCode(), stdout: stdout.String(), stderr: stderr.String()}
}

// childEnv is this process's environment without GITHUB_ACTIONS, then GOMAXPROCS=1, then extra.
// These tests run in a GitHub Actions job, where compare refuses to run, so every child starts
// outside one unless a row sets GITHUB_ACTIONS itself. CI stays as this process has it: GitHub and
// an agent's shell both set CI=true, and compare runs under it. GOMAXPROCS=1 runs each child on
// one P, except the one row that starts it behind env -i to drop the whole environment: the
// parallel rows keep dozens of children alive at once, and a child left to size GOMAXPROCS to the
// host starts a thread for each P its collector's idle mark workers take, about 80 on a 288-CPU
// host.
func childEnv(extra ...string) []string {
	env := slices.DeleteFunc(os.Environ(), func(v string) bool { return strings.HasPrefix(v, "GITHUB_ACTIONS=") })
	return append(append(env, "GOMAXPROCS=1"), extra...)
}

// expect fails the test unless got is want.
func expect(t *testing.T, got, want result) {
	t.Helper()
	if got != want {
		t.Errorf("exit %d, stdout %q, stderr %q\nwant exit %d, stdout %q, stderr %q", got.code, got.stdout, got.stderr, want.code, want.stdout, want.stderr)
	}
}

// fixture is the absolute path of internal/dispatch/specgraph/testdata/<parts>, the fixture trees
// specgraph's own tests read.
func fixture(t *testing.T, parts ...string) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join(append([]string{"..", "..", "internal", "dispatch", "specgraph", "testdata"}, parts...)...))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// readDir reads each file directly in dir, by name, as specgraph.Parse takes them.
func readDir(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for _, entry := range entries {
		content, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		files[entry.Name()] = content
	}
	return files
}

// writeFile writes content to path.
func writeFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

// copyOf is a copy of the files of dir in a new temporary directory.
func copyOf(t *testing.T, dir string) string {
	t.Helper()
	copied := filepath.Join(t.TempDir(), filepath.Base(dir))
	if err := os.CopyFS(copied, os.DirFS(dir)); err != nil {
		t.Fatal(err)
	}
	return copied
}
