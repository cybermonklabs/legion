package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/specgraph"
)

// lineBound is the most bytes that one line of these runs may hold. A line of a problem holds its
// place, its node and its detail, and at most a few values of the input, each cut to its first and
// its last 50 bytes; 1,024 bytes is room for that and the path of a temporary directory, and a line
// that held one value of 262,144 bytes whole would be 256 times as long.
const lineBound = 1024

// longValue is a value of the input of size bytes: start, then "y", then end. With ASCII letters,
// digits and single hyphens it has the shape of an id.
func longValue(size int, start, end string) string {
	return start + strings.Repeat("y", size-len(start)-len(end)) + end
}

// longRow is one row of the table of the lines that print a long value of the input: the kind of
// problem that its files make (its format and Effect, as specgraph.ProblemKinds gives them), the
// spec files, an edit of carry's approved spec (nil keeps it), the retired ids, the parts of the
// value that the kind's line must show, and where that line is: "tree" for a problem of the files,
// which tree and state print and compare prints for an Effect of LosesNode; "compare" for a problem
// of Compare; "approved" for a refusal of the approved spec, which compare and state print.
type longRow struct {
	name     string
	kind     string
	effect   specgraph.Effect
	files    map[string]string
	approved func(nodes []specgraph.ApprovedNode) []specgraph.ApprovedNode
	retired  []string
	shows    []string
	in       string
}

// Each line of tree, compare and state that names a value of the input prints a value of 262,144
// bytes as its first and its last 50 bytes with … between them, so the line stays under lineBound
// and names the start and the end of the value. The table has a row for each kind of problem that
// specgraph gives (ProblemKinds: the reader's, Validate's, Compare's and bad-approved-document), so
// a new kind fails the test until it has a row; a row runs the three subcommands over its files and
// its approved spec, and requires each line of each run under the bound, and the kind's line where
// the row says, holding each part of shows. The rows of no-node and of a value that JSON cannot hold
// name no value of the input: they hold only the bound. Then a valid id longer than the bound: state
// and print match it whole, and each line shows it cut.
func TestEachLineCutsALongValueOfTheInput(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	carry := fixture(t, "scenarios", "carry")
	baseBytes, err := os.ReadFile(filepath.Join(carry, "files", "c2", "spec.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	base := string(baseBytes)
	v := longValue(1<<18, "value-start-", "-value-end")
	other := longValue(1<<18, "other-start-", "-other-end")
	shows := []string{"value-start-", "-value-end"}
	// badID is a long text that is not in the shape of an id, as the upper-case V makes it.
	badID, badIDShows := longValue(1<<18, "Value-start-", "-value-end"), []string{"Value-start-", "-value-end"}
	both := []string{"value-start-", "-value-end", "other-start-", "-other-end"}
	term := func(id, name, definition string) string {
		return "---\ntype: term\nid: " + id + "\nname: " + name + "\ndefinition:\n  - " + definition + "\n"
	}
	requirement := func(id, relations, text string) string {
		return "---\ntype: requirement\nid: " + id + "\nkind: behavior\n" + relations + "text:\n  - " + text + "\n"
	}
	agentCheck := func(id, serves, timeLimit, claim, procedure, failing string) string {
		return "---\ntype: check\nid: " + id + "\nserves: " + serves + "\nsubject: code\nprocedure: agent\n" + timeLimit +
			"claim: " + claim + "\nprocedure_text:\n  - " + procedure + "\nfailing_example:\n  - " + failing + "\n"
	}
	checkOf := func(id, serves string) string {
		return agentCheck(id, serves, "", "The page shows it.", "An agent opens the page.", "A page that shows nothing.")
	}
	programCheck := func(id, runner, selector, job string) string {
		return "---\ntype: check\nid: " + id + "\nserves: messages-reach-sessions\nsubject: code\nprocedure: program\ntest:\n  runner: " + runner +
			"\n  selector: " + selector + "\n  job: " + job + "\nclaim: The page shows it.\nprocedure_text:\n  - It opens the page.\nfailing_example:\n  - A page that shows nothing.\n"
	}
	component := func(id, kind, relations, purpose string) string {
		return "---\ntype: component\nid: " + id + "\nname: A part\nkind: " + kind + "\n" + relations + "purpose:\n  - " + purpose + "\n"
	}
	files := func(extra ...string) map[string]string {
		return map[string]string{"spec.yaml": base + strings.Join(extra, "")}
	}
	workUnit := term("work-unit", "work unit", "A work unit is work.")
	addNode := func(id string, removed bool, documents ...string) func(nodes []specgraph.ApprovedNode) []specgraph.ApprovedNode {
		return func(nodes []specgraph.ApprovedNode) []specgraph.ApprovedNode {
			n := specgraph.ApprovedNode{ID: id, Removed: new(removed), PartlyChecked: new(false)}
			for i, document := range documents {
				n.Versions = append(n.Versions, specgraph.ApprovedVersion{Version: i + 1, At: scenarioApproval, Document: document})
			}
			return append(nodes, n)
		}
	}
	const yaml, schema = "bad-yaml: %s", "schema: %s"
	rows := []longRow{
		{name: "an alias of no anchor", kind: yaml, effect: specgraph.LosesNode, files: files(term("work-unit", "*"+v, "A work unit is work.")), shows: shows},
		{name: "a key written two times", kind: yaml, effect: specgraph.LosesNode, files: files("---\ntype: term\nid: work-unit\n? " + v + "\n: 1\n? " + v + "\n: 2\nname: work unit\ndefinition:\n  - A work unit is work.\n"), shows: shows},
		{name: "a list as a key", kind: yaml, effect: specgraph.LosesNode, files: files("---\ntype: term\nid: work-unit\n? [" + v + "]\n: 1\nname: work unit\ndefinition:\n  - A work unit is work.\n"), shows: shows},
		{name: "an anchor whose value holds its alias", kind: yaml, effect: specgraph.LosesNode, files: files("---\ntype: term\nid: work-unit\nname: work unit\ndefinition: &" + v + "\n  - *" + v + "\n"), shows: shows},
		{name: "a tag that the value does not take", kind: yaml, effect: specgraph.LosesNode, files: files(term("work-unit", "!!int "+v, "A work unit is work.")), shows: shows},
		{name: "a key that JSON cannot hold", kind: "bad-yaml: the document holds a value that JSON cannot hold", effect: specgraph.LosesNode, files: files("---\n1: " + v + "\n")},
		{name: "a null under a long key", kind: "bad-yaml: %s is null", effect: specgraph.LosesNode, files: files(workUnit + "? " + v + "\n: null\n"), shows: shows},
		{name: "a file of a comment only", kind: "no-node: a spec file holds one or more nodes", effect: specgraph.KeepsEachNode, files: map[string]string{"spec.yaml": base, "empty.yaml": "# " + v + "\n"}},
		{name: "a key that the node does not take", kind: schema, effect: specgraph.LosesNode, files: files(workUnit + "? " + v + "\n: x\n"), shows: shows},
		{name: "an id that breaks the pattern", kind: schema, effect: specgraph.KeepsEachNode, files: files(requirement(badID, "", "Envoy keeps it.")), shows: badIDShows},
		{name: "a time limit that is no duration", kind: `bad-time-limit: %q is not a positive Go duration`, effect: specgraph.LosesNode,
			files: files(agentCheck("a-check-of-time", "messages-reach-sessions", "time_limit: "+v+"\n", "The page shows it.", "An agent opens the page.", "A page that shows nothing.")), shows: shows},
		{name: "a space at the end of a line", kind: "not-printed: %s", effect: specgraph.LosesNode, files: files(term("work-unit", "work unit", "A work unit is "+v+". ")), shows: shows},
		{name: "an anchor", kind: "not-printed: %s", effect: specgraph.LosesNode, files: files(term("work-unit", "&"+v+" work unit", "A work unit is work.")), shows: shows},

		{name: "one id two times", kind: "doubled-id: also at %s:%d", files: files(term(v, "work unit", "A work unit is work."), term(v, "work unit", "A work unit is work.")), shows: shows},
		{name: "an id with a word that states a position", kind: ruleFormat(t, "bad-id: "), files: files(term(longValue(1<<18, "value-start-s3-", "-value-end"), "work unit", "A work unit is work.")), shows: []string{"value-start-s3-", "-value-end"}},
		{name: "an id that the approved spec removed", kind: "retired-id: the approved spec removed this id", files: files(term(v, "work unit", "A work unit is work.")), retired: []string{v}, shows: shows, in: "retired"},
		{name: "a text of no word", kind: "no-text: a statement has a text", files: files(requirement(v, "", "…"), checkOf("a-check-of-it", v)), shows: shows},
		{name: "a parent that the files do not hold", kind: ruleFormat(t, "no-parent: "), files: files(requirement("a-page-shows-it", "parent: "+v+"\n", "The page shows it."), checkOf("a-check-of-it", "a-page-shows-it")), shows: shows},
		{name: "a statement that it serves and the files do not hold", kind: ruleFormat(t, "missing-target: ", "which the files do not hold"),
			files: files(requirement("a-page-shows-it", "serves:\n  - "+v+"\n", "The page shows it."), checkOf("a-check-of-it", "a-page-shows-it")), shows: shows},
		{name: "a node that it is about of another kind", kind: ruleFormat(t, "missing-target: ", "not a %s"),
			files: files(term(v, "work unit", "A work unit is work."), "---\ntype: requirement\nid: a-page-shows-it\nkind: structure\nabout:\n  - "+v+"\ntext:\n  - The page shows it.\n", checkOf("a-check-of-it", "a-page-shows-it")), shows: shows},
		{name: "a leaf with no check", kind: "leaf-without-check: no statement is under it and no check serves it", files: files(requirement(v, "", "Envoy keeps it.")), shows: shows},
		{name: "a statement that others serve with no check", kind: "supported-without-check: other statements serve it and no check serves it",
			files: files(requirement(v, "", "Envoy keeps it."), requirement("a-part-of-it", "parent: "+v+"\n", "Envoy keeps a part."), checkOf("a-check-of-the-part", "a-part-of-it"),
				requirement("a-server-of-it", "serves:\n  - "+v+"\n", "Envoy serves it."), checkOf("a-check-of-the-server", "a-server-of-it")), shows: shows},
		{name: "two statements that are each the parent of the other", kind: ruleFormat(t, "loop: "),
			files: map[string]string{"spec.yaml": base + requirement(v, "parent: "+other+"\n", "Envoy keeps it.") + checkOf("a-check-of-it", v),
				"other.yaml": requirement(other, "parent: "+v+"\n", "Envoy keeps the other.") + checkOf("a-check-of-the-other", other)}, shows: both},
		{name: "a check of no statement of the files", kind: "check-serves: a check serves exactly one statement of the files", files: files(checkOf(v, "no-statement-of-it")), shows: shows},
		{name: "a claim of no word", kind: "no-claim: the check has no claim", files: files(agentCheck(v, "messages-reach-sessions", "", "…", "An agent opens the page.", "A page that shows nothing.")), shows: shows},
		{name: "a procedure of no word", kind: "no-procedure: the check has no procedure", files: files(agentCheck(v, "messages-reach-sessions", "", "The page shows it.", "…", "A page that shows nothing.")), shows: shows},
		{name: "a failing example of no word", kind: "no-failing-example: the check has no failing example", files: files(agentCheck(v, "messages-reach-sessions", "", "The page shows it.", "An agent opens the page.", "…")), shows: shows},
		{name: "a claim of two sentences", kind: "bad-claim: a claim is one sentence", files: files(agentCheck(v, "messages-reach-sessions", "", "The page shows it. It shows all.", "An agent opens the page.", "A page that shows nothing.")), shows: shows},
		{name: "a test with no selector", kind: "no-test: a program check names its runner and its selector", files: files(programCheck(v, "go", "…", "go-checks")), shows: shows},
		{name: "a selector of no test", kind: `bad-selector: a go selector is "<import path> <TopLevelTestName>"`, files: files(programCheck(v, "go", "no test", "go-checks")), shows: shows},
		{name: "a test with no job", kind: "no-job: a program check names the job that runs its test", files: files(programCheck(v, "go", "github.com/sjawhar/envoy/cmd/listener TestARestartLosesNoMessage", "…")), shows: shows},
		{name: "a purpose of no word", kind: "bad-component: a component has a name and one sentence of purpose", files: files(component(v, "component", "", "…")), shows: shows},
		{name: "a parent that is no component", kind: ruleFormat(t, "bad-component: parent "), files: files(component("a-part", "component", "parent: "+v+"\n", "It holds a part.")), shows: shows},
		{name: "an interface that joins no component", kind: ruleFormat(t, "bad-interface: "),
			files: files(component("a-joint", "interface", "joins:\n  - "+v+"\n  - zz-no-component\n", "It joins two parts.")), shows: shows},
		{name: "a definition of no word", kind: "bad-term: a term has a name and a definition", files: files(term(v, "work unit", "…")), shows: shows},
		{name: "a name of code in a text", kind: ruleFormat(t, "code-in-text: "), files: files(requirement("a-page-shows-it", "", "It reads `"+v+"`."), checkOf("a-check-of-it", "a-page-shows-it")), shows: shows},

		{name: "an id that the approved spec never held", kind: "unapproved-id: the approved spec never held this id", files: files(term(v, "work unit", "A work unit is work.")), shows: shows, in: "compare"},
		{name: "a text that the approved spec never held", kind: "unapproved-text: the approved spec never held this text", files: files(term(v, "work unit", "A work unit is work.")),
			approved: addNode(v, false, term(v, "work unit", "A work unit is a task.")), shows: shows, in: "compare"},
		{name: "an id that the approved spec removed, in another text", kind: "removed-id: the approved spec removed this id and never held this text for it", files: files(term(v, "work unit", "A work unit is work.")),
			approved: addNode(v, true, term(v, "work unit", "A work unit is a task.")), shows: shows, in: "compare"},
		{name: "an approved document of two nodes", kind: ruleFormat(t, "bad-approved-document: the document holds"), files: files(),
			approved: addNode(v, false, term(v, "work unit", "A work unit is work.")+workUnit), shows: shows, in: "approved"},
		{name: "an approved document of another node", kind: ruleFormat(t, "bad-approved-document: the document is of"), files: files(),
			approved: addNode(v, false, workUnit), shows: shows, in: "approved"},
		{name: "an approved document whose parent is no id", kind: ruleFormat(t, "bad-approved-document: ", "is no id"), files: files(),
			approved: addNode("a-page-shows-it", false, requirement("a-page-shows-it", "parent: "+badID+"\n", "The page shows it.")), shows: badIDShows, in: "approved"},
		{name: "an approved document that print does not write", kind: "bad-approved-document: the document is not as Print writes it", files: files(),
			approved: addNode(v, false, term(v, "work unit", "A work unit is work.")+"surprise: true\n"), shows: shows, in: "approved"},
	}
	t.Run("complete", func(t *testing.T) {
		t.Parallel()
		type kind struct {
			format string
			effect specgraph.Effect
		}
		need := map[kind]int{}
		for _, k := range specgraph.ProblemKinds() {
			need[kind{k.Format, k.Effect}]++
		}
		have := map[kind]int{}
		for _, row := range rows {
			have[kind{row.kind, row.effect}]++
			if need[kind{row.kind, row.effect}] == 0 {
				t.Errorf("the row %q names the kind %q with the Effect %d, which specgraph.ProblemKinds does not give", row.name, row.kind, row.effect)
			}
		}
		for k, n := range need {
			if have[k] < n {
				t.Errorf("specgraph gives %d kinds %q with the Effect %d, and the table has %d rows of it: give each kind a row", n, k.format, k.effect, have[k])
			}
		}
	})
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			for name, content := range row.files {
				writeFile(t, filepath.Join(dir, name), []byte(content))
			}
			retired := filepath.Join(t.TempDir(), "retired")
			writeFile(t, retired, []byte(strings.Join(row.retired, "\n")))
			scenario := copyOf(t, carry)
			removeAll(t, filepath.Join(scenario, "files", "c2"))
			if err := os.CopyFS(filepath.Join(scenario, "files", "c2"), os.DirFS(dir)); err != nil {
				t.Fatal(err)
			}
			approved := filepath.Join(scenario, "approved.json")
			if row.approved != nil {
				editApproved(t, approved, row.approved)
			}
			lines := func(run string, args ...string) []string {
				r := runBinary(t, binary, "", args...)
				all := strings.Split(strings.TrimSuffix(r.stdout+r.stderr, "\n"), "\n")
				for _, line := range all {
					if len(line) > lineBound {
						t.Errorf("%s: a line of %d bytes, more than %d: %.150q … %q", run, len(line), lineBound, line, line[len(line)-150:])
					}
				}
				return all
			}
			tree := lines("tree", "tree", "--dir", dir, "--retired", retired)
			compare := lines("compare", "compare", "--dir", dir, "--approved", approved)
			state := lines("state", "state", "--scenario", scenario)
			want := map[string][]string{}
			switch row.in {
			case "", "retired":
				want["tree"] = tree
				if row.in == "" {
					want["state"] = state
				}
				if row.effect == specgraph.LosesNode {
					want["compare"] = compare
				}
			case "compare":
				want["compare"] = compare
			case "approved":
				want["compare"], want["state"] = compare, state
			}
			start, _, _ := strings.Cut(row.kind, "%")
			for run, output := range want {
				if !slices.ContainsFunc(output, func(line string) bool {
					return strings.Contains(line, start) && !slices.ContainsFunc(row.shows, func(part string) bool { return !strings.Contains(line, part) })
				}) {
					t.Errorf("%s prints no line of %q that shows %q of the value; it prints %.600q", run, start, row.shows, output)
				}
			}
		})
	}
	t.Run("a valid id longer than the bound", func(t *testing.T) {
		t.Parallel()
		testALongIDMatchesAndPrintsCut(t, binary, carry, longValue(1<<16, "value-start-", "-value-end"), longValue(1<<16, "check-start-", "-check-end"))
	})
}

// ruleFormat is the one format of specgraph.ProblemKinds that starts with start and holds each of
// parts; the test fails when not exactly one does.
func ruleFormat(t *testing.T, start string, parts ...string) string {
	t.Helper()
	var found []string
	for _, k := range specgraph.ProblemKinds() {
		if strings.HasPrefix(k.Format, start) && !slices.ContainsFunc(parts, func(p string) bool { return !strings.Contains(k.Format, p) }) {
			found = append(found, k.Format)
		}
	}
	if len(found) != 1 {
		t.Fatalf("specgraph.ProblemKinds gives %q for %q and %q, want one format", found, start, parts)
	}
	return found[0]
}

// testALongIDMatchesAndPrintsCut takes carry with the id of its statement
// a-stored-message-survives-a-restart replaced by statement, and the id of its check
// a-restart-loses-no-message by check, each a valid id longer than the bound, in each file: 65,536
// bytes, since print takes the id as one argument, and Linux takes no argument of more than 131,072
// bytes (MAX_ARG_STRLEN). state derives each state that carry's want.txt holds, and names the
// statement and the check by their cut forms; print writes the statement's approved document, the
// whole id in the file, and names it cut. state --json and checks hold each id whole, since a JSON
// line is data for a program: the one kind of line of the command that holds a long value whole.
func testALongIDMatchesAndPrintsCut(t *testing.T, binary, carry, statement, check string) {
	const statementID, checkID = "a-stored-message-survives-a-restart", "a-restart-loses-no-message"
	cut := func(id string) string { return id[:50] + "…" + id[len(id)-50:] }
	scenario := copyOf(t, carry)
	err := filepath.WalkDir(scenario, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(path, []byte(strings.NewReplacer(statementID, statement, checkID, check).Replace(string(content))), 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	wantBytes, err := os.ReadFile(filepath.Join(carry, "want.txt"))
	if err != nil {
		t.Fatal(err)
	}
	expect(t, runBinary(t, binary, "", "state", "--scenario", scenario),
		result{stdout: strings.NewReplacer(statementID, cut(statement), checkID, cut(check)).Replace(string(wantBytes))})

	r := runBinary(t, binary, "", "state", "--scenario", scenario, "--json")
	if r.code != 0 || !strings.Contains(r.stdout, `"`+statement+`"`) || !strings.Contains(r.stdout, `"`+check+`"`) {
		t.Errorf("state --json: exit %d, stdout of %d bytes, stderr %q; want exit 0 and each id whole in the JSON", r.code, len(r.stdout), r.stderr)
	}
	dir := filepath.Join(scenario, "files", "c2")
	r = runBinary(t, binary, "", "checks", "--dir", dir, "--job", "go-checks")
	if r.code != 0 || !strings.Contains(r.stdout, `"id":"`+check+`"`) {
		t.Errorf("checks: exit %d, stdout of %d bytes, stderr %q; want exit 0 and the check's id whole in the JSON", r.code, len(r.stdout), r.stderr)
	}

	before, err := os.ReadFile(filepath.Join(dir, "spec.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	r = runBinary(t, binary, "", "print", "--approved", filepath.Join(scenario, "approved.json"), "--id", statement, "--dir", dir)
	printed := strings.Split(strings.TrimSuffix(r.stdout, "\n"), "\n")
	if r.code != 0 || printed[0] != "spec-check: wrote "+cut(statement)+" to "+filepath.Join(dir, "spec.yaml")+":25" || slices.ContainsFunc(printed, func(l string) bool { return len(l) > lineBound }) {
		t.Errorf("print --id <the long id>: exit %d, stdout %.300q, stderr %.300q; want exit 0 and a first line that names the id cut", r.code, r.stdout, r.stderr)
	}
	after, err := os.ReadFile(filepath.Join(dir, "spec.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) || !strings.Contains(string(after), "id: "+statement+"\n") {
		t.Errorf("print changed spec.yaml, or it does not hold the whole id: %d bytes before, %d after", len(before), len(after))
	}
}
