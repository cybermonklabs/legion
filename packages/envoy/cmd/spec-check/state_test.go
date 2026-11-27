package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/specgraph"
)

// Each scenario of specgraph's testdata/scenarios, each with the answers that the spec gives it in
// want.txt: state prints want.txt byte for byte, exit 0 and nothing on stderr. The four of The Spec
// (carry, one-commit-two-answers, earlier-text, wait-cycle) are among them, and the test fails when
// one is missing; the others hold more shapes of the rules. No flag of the test writes want.txt, and
// a scenario with no want.txt fails the test, which would read nothing. The control runs carry with
// no tie, so that c1's caught failing example counts only at c1, where its fault was planted: the
// comparison can fail.
func TestScenarios(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	entries, err := os.ReadDir(fixture(t, "scenarios"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	for _, ofTheSpec := range []string{"carry", "one-commit-two-answers", "earlier-text", "wait-cycle"} {
		if !slices.Contains(names, ofTheSpec) {
			t.Errorf("the scenarios %q do not hold %s", names, ofTheSpec)
		}
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := fixture(t, "scenarios", name)
			expect(t, runBinary(t, binary, "", "state", "--scenario", dir), result{stdout: wantOf(t, dir)})
		})
	}
	t.Run("control: carry with no tie counts c1's failing example at neither c2 nor m2", func(t *testing.T) {
		t.Parallel()
		dir := copyScenario(t, "carry")
		writeFile(t, filepath.Join(dir, "ties.json"), []byte("{}\n"))
		got := runBinary(t, binary, "", "state", "--scenario", dir)
		if got.code != 0 || got.stderr != "" {
			t.Fatalf("exit %d, stderr %q: want exit 0 and nothing on stderr", got.code, got.stderr)
		}
		if got.stdout == wantOf(t, fixture(t, "scenarios", "carry")) {
			t.Errorf("with no tie, the state is carry's want.txt")
		}
		unplaced := "  check a-restart-loses-no-message: unknown (no_failing_example)"
		for _, subject := range []string{"c2, the head of listener-restart", "m2, the head of main"} {
			if lines := subjectLines(got.stdout, subject); !slices.Contains(lines, unplaced) {
				t.Errorf("at subject %s: %q, want the line %q", subject, lines, unplaced)
			}
		}
	})
	t.Run("--json prints the state at each subject", func(t *testing.T) {
		t.Parallel()
		var states []specgraph.State
		decodeStrict(t, runBinary(t, binary, "", "state", "--scenario", fixture(t, "scenarios", "carry"), "--json"), &states)
		var subjects []specgraph.Point
		for _, s := range states {
			subjects = append(subjects, s.Subject)
		}
		want := []specgraph.Point{{Commit: "c2", Branch: "listener-restart"}, {Commit: "m2", Branch: "main"}, {Commit: "c3", Branch: "contract-fixture"}, {Commit: "c4", Branch: "listener-change"}}
		if !slices.Equal(subjects, want) {
			t.Fatalf("the subjects of the states: %+v, want %+v", subjects, want)
		}
		if want := []specgraph.CheckState{{ID: "a-restart-loses-no-message", State: "true", Reason: "passed", From: "m2"}}; !slices.Equal(states[1].Checks, want) {
			t.Errorf("the checks at m2: %+v, want %+v", states[1].Checks, want)
		}
		want3 := []specgraph.StatementState{
			{ID: "delivery-is-fast", State: "met", Reason: "checks_true", Own: "met", HeldVersion: 1},
			{ID: "messages-reach-sessions", State: "unknown", Reason: "check_unknown", Detail: "a-restart-loses-no-message", Own: "met", HeldVersion: 1},
			{ID: "a-stored-message-survives-a-restart", State: "unknown", Reason: "check_unknown", Detail: "a-restart-loses-no-message", Own: "unknown", HeldVersion: 1},
		}
		if !slices.Equal(states[2].Statements, want3) {
			t.Errorf("the statements at c3:\n got %+v\nwant %+v", states[2].Statements, want3)
		}
	})
	// R48: a refusal of CI names the rule that refused the check. The text of the scenario refusal
	// prints its check at c2 as unknown (uncited), and --json prints the rule as the check's detail.
	t.Run("--json prints the rule of a refusal of CI as the detail of the check", func(t *testing.T) {
		t.Parallel()
		var states []specgraph.State
		decodeStrict(t, runBinary(t, binary, "", "state", "--scenario", fixture(t, "scenarios", "refusal"), "--json"), &states)
		want := []specgraph.CheckState{{ID: "a-restart-loses-no-message", State: "unknown", Reason: "uncited", Detail: "test-not-in-code"}}
		if len(states) == 0 || states[0].Subject != (specgraph.Point{Commit: "c2", Branch: "listener-restart"}) || !slices.Equal(states[0].Checks, want) {
			t.Errorf("the states: %+v\nwant first the state at c2, with the checks %+v", states, want)
		}
	})
	t.Run("--json with units.json prints the states and the cycle", func(t *testing.T) {
		t.Parallel()
		var out struct {
			States []specgraph.State `json:"states"`
			Cycle  []string          `json:"cycle"`
		}
		decodeStrict(t, runBinary(t, binary, "", "state", "--scenario", fixture(t, "scenarios", "wait-cycle"), "--json"), &out)
		if want := []string{"u-a", "a-message-reaches-a-late-session", "u-b", "a-message-survives-a-restart", "u-a"}; !slices.Equal(out.Cycle, want) {
			t.Errorf("cycle %q, want %q", out.Cycle, want)
		}
		if len(out.States) != 1 || !out.States[0].WaitEnds("a-message-reaches-a-late-session") || out.States[0].WaitEnds("a-message-survives-a-restart") {
			t.Errorf("states %+v: want one state at m1, where the wait on a-message-reaches-a-late-session ends and the wait on a-message-survives-a-restart holds", out.States)
		}

		dir := copyScenario(t, "wait-cycle")
		writeFile(t, filepath.Join(dir, "units.json"), []byte(`[{"id":"u-a","serves":["a-message-survives-a-restart"],"needs":[],"waits_on":[]}]`))
		got := runBinary(t, binary, "", "state", "--scenario", dir, "--json")
		if got.code != 0 || !strings.HasSuffix(got.stdout, `,"cycle":[]}`+"\n") {
			t.Errorf("with no cycle: exit %d, stdout %q, want exit 0 and \"cycle\":[]", got.code, got.stdout)
		}
	})
}

// The approved side is read by what Dispatch refuses at a change, not by the rules of the files:
// the person approves a level before the checks of its statements exist, so the approved spec can
// hold a leaf with no check. The files do not hold it, and it reads not built (R10).
func TestAnApprovedLeafWithNoCheckReadsNotBuilt(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	dir := copyScenario(t, "carry")
	leaf := specgraph.Statement{ID: "a-session-reads-old-messages", Kind: specgraph.KindBehavior,
		Text: "A session reads each message that Envoy stored before the session connected."}
	addApproved(t, dir, "a-session-reads-old-messages", leaf)
	got := runBinary(t, binary, "", "state", "--scenario", dir)
	if got.code != 0 || got.stderr != "" {
		t.Fatalf("exit %d, stderr %q: want exit 0 and nothing on stderr", got.code, got.stderr)
	}
	line := "statement a-session-reads-old-messages: not built (the files do not hold its approved text)"
	for _, subject := range []string{"c2, the head of listener-restart", "m2, the head of main", "c3, the head of contract-fixture", "c4, the head of listener-change"} {
		if lines := subjectLines(got.stdout, subject); len(lines) == 0 || lines[0] != line {
			t.Errorf("at subject %s: %q, want the first line %q", subject, lines, line)
		}
	}
}

// A relation names a node of the approved spec of its kind, kept or removed (R22: "Dispatch refuses
// the approval of a change request that puts a statement under a parent that the approved spec does
// not hold"). A node that the approved spec removed is one that it holds, so a relation to it is
// taken, and the derivation reads the current tree, which leaves the removed node out: in
// wait-cycle with envoy-delivers-messages removed, the two statements under it read at the top of
// the tree, and the check that serves it is taken and judged. --json reports it among the checks,
// and the text prints it after the statements, as a check that serves no current statement, so the
// text names each check that --json reports. The wait of u-c on it is dropped, since a unit waits
// on a current statement.
// The relations that name no node, or a node of another kind, are rows of TestScenarioRefusals.
func TestARelationToANodeThatTheApprovedSpecRemovedIsTaken(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	dir := copyScenario(t, "wait-cycle")
	editApproved(t, filepath.Join(dir, "approved.json"), func(nodes []specgraph.ApprovedNode) []specgraph.ApprovedNode {
		approvedNode(t, nodes, "envoy-delivers-messages").Removed = true
		return nodes
	})
	replaceIn(t, filepath.Join(dir, "units.json"), `"waits_on": ["envoy-delivers-messages"]`, `"waits_on": []`)
	text := runBinary(t, binary, "", "state", "--scenario", dir)
	expect(t, text, result{stdout: `spec-check: subject m1, the head of main
statement a-message-reaches-a-late-session: met
  check a-late-session-gets-the-message: true (passed, counted from m1)
statement a-message-survives-a-restart: not met (check a-restart-keeps-the-message is false)
  check a-restart-keeps-the-message: false (failed, counted from m1)
check a-sent-message-reaches-a-listener (serves no current statement): true (passed, counted from m1)
wait of u-a on a-message-reaches-a-late-session: ends
wait of u-b on a-message-survives-a-restart: holds (not met)
spec-check: cycle of waits: u-a -> a-message-reaches-a-late-session -> u-b -> a-message-survives-a-restart -> u-a
`})
	var out struct {
		States []specgraph.State `json:"states"`
		Cycle  []string          `json:"cycle"`
	}
	decodeStrict(t, runBinary(t, binary, "", "state", "--scenario", dir, "--json"), &out)
	served := specgraph.CheckState{ID: "a-sent-message-reaches-a-listener", State: "true", Reason: "passed", From: "m1"}
	if len(out.States) != 1 || !slices.Contains(out.States[0].Checks, served) {
		t.Fatalf("--json: states %+v; want one state, whose checks hold %+v", out.States, served)
	}
	for _, c := range out.States[0].Checks {
		if !strings.Contains(text.stdout, "check "+c.ID+":") && !strings.Contains(text.stdout, "check "+c.ID+" (serves no current statement):") {
			t.Errorf("--json reports the check %s, and the text names it on no line", c.ID)
		}
	}
}

// R5: "The parent and support relations taken together form no cycle, and Dispatch refuses a change
// that makes one." A change of the approved spec is a new version of a node, and a loop that only
// an older version made is no loop of the approved spec now: wait-cycle, where version 1 of
// a-message-survives-a-restart made it its own parent and version 2, its text as written, is the one
// that the files hold, reads as wait-cycle (want.txt). The loop in version 2 is a row of
// TestScenarioRefusals.
func TestALoopThatOnlyAnOlderVersionOfTheApprovedSpecMadeIsTaken(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	dir := copyScenario(t, "wait-cycle")
	withSecondVersion(t, dir, "a-message-survives-a-restart", "parent: envoy-delivers-messages\n", "parent: a-message-survives-a-restart\n", 1)
	want, err := os.ReadFile(filepath.Join(dir, "want.txt"))
	if err != nil {
		t.Fatal(err)
	}
	expect(t, runBinary(t, binary, "", "state", "--scenario", dir), result{stdout: string(want)})
}

// withSecondVersion gives the node id of approved.json of dir a version 2 at the time of its
// version 1, with the block of version 1, and replaces old with new in the block of version
// changed. The test fails when that block does not hold old.
func withSecondVersion(t *testing.T, dir, id, old, new string, changed int) {
	t.Helper()
	editApproved(t, filepath.Join(dir, "approved.json"), func(nodes []specgraph.ApprovedNode) []specgraph.ApprovedNode {
		n := approvedNode(t, nodes, id)
		second := n.Versions[0]
		second.Version = 2
		n.Versions = append(n.Versions, second)
		v := &n.Versions[changed-1]
		if !strings.Contains(v.Document, old) {
			t.Fatalf("version %d of %s does not hold %q", changed, id, old)
		}
		v.Document = strings.Replace(v.Document, old, new, 1)
		return nodes
	})
}

// R29: "That wait ends when main holds the statement's approved text and its own checks read true
// there." R10: "Only the checks that read the subject count." At the head of main an own check of
// production decides nothing, so the wait reads the own checks that read main. It ends beside a
// production check with no result, with only a production check and with no own check, and it
// holds while an own check that reads main is false: u-b's wait, in each row.
func TestAWaitReadsTheOwnChecksThatReadTheSubject(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	production := specgraph.Check{ID: "a-late-session-gets-messages-in-production", Serves: "a-message-reaches-a-late-session",
		Subject: specgraph.SubjectProduction, Procedure: specgraph.ProcedureQuery,
		Claim:          "Each session of the last hour got each message that was written before it connected.",
		ProcedureText:  "The query reads the messages and the sessions of the last hour.\nIt returns each message that a session did not get.",
		FailingExample: "A copy of the records with one message that a late session did not get."}
	addProduction := func(t *testing.T, dir string) {
		addApproved(t, dir, production.ID, production)
		block, err := production.Print()
		if err != nil {
			t.Fatal(err)
		}
		spec := filepath.Join(dir, "files", "m1", "spec.yaml")
		content, err := os.ReadFile(spec)
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, spec, append(content, block...))
	}
	holds := "wait of u-b on a-message-survives-a-restart: holds (not met)"
	for _, row := range []struct {
		name   string
		change func(t *testing.T, dir string)
		want   []string
	}{
		{"a code check that is true beside a production check with no result", addProduction,
			[]string{"statement a-message-reaches-a-late-session: met", "wait of u-a on a-message-reaches-a-late-session: ends", holds}},
		{"a production check alone", func(t *testing.T, dir string) {
			addProduction(t, dir)
			removeCheck(t, dir, "a-late-session-gets-the-message")
		}, []string{"statement a-message-reaches-a-late-session: met", "wait of u-a on a-message-reaches-a-late-session: ends", holds}},
		{"no own check", func(t *testing.T, dir string) { removeCheck(t, dir, "a-sent-message-reaches-a-listener") },
			[]string{"wait of u-c on envoy-delivers-messages: ends", holds}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			dir := copyScenario(t, "wait-cycle")
			row.change(t, dir)
			got := runBinary(t, binary, "", "state", "--scenario", dir)
			if got.code != 0 || got.stderr != "" {
				t.Fatalf("exit %d, stderr %q: want exit 0 and nothing on stderr", got.code, got.stderr)
			}
			lines := strings.Split(got.stdout, "\n")
			for _, want := range row.want {
				if !slices.Contains(lines, want) {
					t.Errorf("no line %q in:\n%s", want, got.stdout)
				}
			}
		})
	}
}

// R13: a query "reads the graph at one transaction". R7: "A result counts only for the procedure
// that ran it and for the subject that it read." R10: "A check whose subject is production decides
// nothing for the head of a branch", and the graph is production's data. Through the command, a
// check of the graph is judged at the commit that production runs, at the graph_tx of the subject,
// which the subject line names, whichever transaction is newer or was recorded last, and reads
// unknown at a subject that names no graph_tx or one that no result read (R10: "Unknown: every
// other case"). R13: "A query that cannot read its data, or that passes a time or row limit, gives
// unknown": a run that could not run names the transaction that it tried to read, the subject's,
// and counts there alone, as a run that passed or failed names the one that it read, or the reader
// refuses it. Each row: carry at m2 alone, as the commit that production runs at deploy d1, with a
// query of the graph as the one check of its leaf (carry's code check taken out), the row's runs of
// it, and a caught failing example.
func TestAGraphCheckIsJudgedAtTheTransactionOfItsSubject(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	graph := specgraph.Check{ID: "the-graph-holds-a-delivery-of-each-message", Serves: "a-stored-message-survives-a-restart",
		Subject: specgraph.SubjectGraph, Procedure: specgraph.ProcedureQuery,
		Claim:          "Each message record of the graph has a delivery record.",
		ProcedureText:  "The query reads each message record of the graph.\nIt returns each message record that has no delivery record.",
		FailingExample: "A copy of the graph with one message record that has no delivery record."}
	run := func(outcome, tx, at string, seq int) any {
		return map[string]any{"check": graph.ID, "check_version": 1, "kind": "run", "outcome": outcome, "graph_tx": tx, "leg": "", "at": at, "seq": seq}
	}
	twoRuns := []any{run("fail", "tx2", "2026-11-13T12:00:00Z", 21), run("pass", "tx1", "2026-11-13T13:00:00Z", 22)}
	noTx := func(outcome string) any {
		return map[string]any{"check": graph.ID, "check_version": 1, "kind": "run", "outcome": outcome, "leg": "", "at": "2026-11-13T13:00:00Z", "seq": 22}
	}
	deploy := "spec-check: subject m2, the commit that production runs at deploy d1"
	leaf, check := "statement a-stored-message-survives-a-restart: ", "  check "+graph.ID+": "
	unknown := []string{leaf + "unknown (check " + graph.ID + ": subject_moved)", check + "unknown (subject_moved)"}
	for _, row := range []struct {
		name, graphTx string
		runs          []any
		subjectLine   string
		want          []string // lines that stdout holds
		fault         string   // the refusal of results.json; "" for none
	}{
		{"a failure at tx2, then a pass at tx1; graph_tx tx2", "tx2", twoRuns, deploy + ", and the graph at transaction tx2",
			[]string{leaf + "not met (check " + graph.ID + " is false)", check + "false (failed, counted from tx2)"}, ""},
		{"a failure at tx2, then a pass at tx1; graph_tx tx1", "tx1", twoRuns, deploy + ", and the graph at transaction tx1",
			[]string{leaf + "met", check + "true (passed, counted from tx1)"}, ""},
		{"a failure at tx2, then a pass at tx1; graph_tx tx3", "tx3", twoRuns, deploy + ", and the graph at transaction tx3", unknown, ""},
		{"a failure at tx2, then a pass at tx1; no graph_tx", "", twoRuns, deploy, unknown, ""},
		{"a failure at tx1; no graph_tx", "", []any{run("fail", "tx1", "2026-11-13T12:00:00Z", 21)}, deploy, unknown, ""},
		{"a pass at tx1; no graph_tx", "", []any{run("pass", "tx1", "2026-11-13T12:00:00Z", 21)}, deploy, unknown, ""},
		{"a failure at tx1, then a query at tx2 that could not run; graph_tx tx1", "tx1",
			[]any{run("fail", "tx1", "2026-11-13T12:00:00Z", 21), run("could_not_run", "tx2", "2026-11-13T13:00:00Z", 22)}, deploy + ", and the graph at transaction tx1",
			[]string{leaf + "not met (check " + graph.ID + " is false)", check + "false (failed, counted from tx1)"}, ""},
		{"a failure at tx1, then a query that could not run and names no transaction; graph_tx tx1", "tx1",
			[]any{run("fail", "tx1", "2026-11-13T12:00:00Z", 21), noTx("could_not_run")}, "", nil,
			"result 4: it sets no graph_tx, the subject that a check of the graph reads"},
		{"a pass at tx1, then a failure that names no transaction; graph_tx tx1", "tx1",
			[]any{run("pass", "tx1", "2026-11-13T12:00:00Z", 21), noTx("fail")}, "", nil,
			"result 4: it sets no graph_tx, the subject that a check of the graph reads"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			dir := copyScenario(t, "carry")
			for _, commit := range []string{"c2", "c3", "c4"} {
				removeAll(t, filepath.Join(dir, "files", commit))
			}
			removeCheck(t, dir, "a-restart-loses-no-message")
			subject := map[string]any{"commit": "m2", "deploy": "d1", "now": "2026-11-14T00:00:00Z"}
			if row.graphTx != "" {
				subject["graph_tx"] = row.graphTx
			}
			writeJSONFile(t, filepath.Join(dir, "subjects.json"), []any{subject})
			addApproved(t, dir, graph.ID, graph)
			block, err := graph.Print()
			if err != nil {
				t.Fatal(err)
			}
			spec := filepath.Join(dir, "files", "m2", "spec.yaml")
			content, err := os.ReadFile(spec)
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, spec, append(content, block...))
			var results []any
			readJSONFile(t, filepath.Join(dir, "results.json"), &results)
			fault := map[string]any{"check": graph.ID, "check_version": 1, "kind": "failing_example", "outcome": "fail", "leg": "", "at": "2026-11-13T11:00:00Z", "seq": 20}
			results = append(append(results, fault), row.runs...)
			writeJSONFile(t, filepath.Join(dir, "results.json"), results)
			got := runBinary(t, binary, "", "state", "--scenario", dir)
			if row.fault != "" {
				expect(t, got, result{code: 2, stderr: "spec-check: " + filepath.Join(dir, "results.json") + ": " + row.fault + "\n"})
				return
			}
			if got.code != 0 || got.stderr != "" {
				t.Fatalf("exit %d, stderr %q: want exit 0 and nothing on stderr", got.code, got.stderr)
			}
			lines := strings.Split(got.stdout, "\n")
			if lines[0] != row.subjectLine {
				t.Errorf("the subject line %q, want %q", lines[0], row.subjectLine)
			}
			for _, want := range row.want {
				if !slices.Contains(lines, want) {
					t.Errorf("no line %q in:\n%s", want, got.stdout)
				}
			}
		})
	}
}

// scenarioApproval is when the approved nodes of the scenarios were approved.
var scenarioApproval = time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)

// addApproved adds node, version 1 of id approved when the scenarios' nodes were, to approved.json
// of dir.
func addApproved(t *testing.T, dir, id string, node interface{ Print() (string, error) }) {
	t.Helper()
	block, err := node.Print()
	if err != nil {
		t.Fatal(err)
	}
	editApproved(t, filepath.Join(dir, "approved.json"), func(nodes []specgraph.ApprovedNode) []specgraph.ApprovedNode {
		return append(nodes, specgraph.ApprovedNode{ID: id, Versions: []specgraph.ApprovedVersion{{Version: 1, At: scenarioApproval, Document: block}}})
	})
}

// approvedNode is the node id of nodes; the test fails when nodes holds none.
func approvedNode(t *testing.T, nodes []specgraph.ApprovedNode, id string) *specgraph.ApprovedNode {
	t.Helper()
	i := slices.IndexFunc(nodes, func(n specgraph.ApprovedNode) bool { return n.ID == id })
	if i < 0 {
		t.Fatalf("the approved spec holds no node %q", id)
	}
	return &nodes[i]
}

// removeCheck takes the check id out of the scenario at dir: its node of approved.json, its block
// from each spec file (the block of its version 1, which the files hold), its results and its tie
// at each commit. The results and the ties are edited as JSON values, since no type of specgraph
// holds the form of results.json, and a row of a test takes a member out of a result.
func removeCheck(t *testing.T, dir, id string) {
	t.Helper()
	var block string
	editApproved(t, filepath.Join(dir, "approved.json"), func(nodes []specgraph.ApprovedNode) []specgraph.ApprovedNode {
		block = approvedNode(t, nodes, id).Versions[0].Document
		return slices.DeleteFunc(nodes, func(n specgraph.ApprovedNode) bool { return n.ID == id })
	})
	paths, err := filepath.Glob(filepath.Join(dir, "files", "*", "spec.yaml"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no spec file under %s: %v", dir, err)
	}
	for _, path := range paths {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(content)
		switch {
		case text == block:
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		case strings.Contains(text, block):
			text = strings.Replace(text, block, "", 1)
			writeFile(t, path, []byte(text))
		default:
			t.Fatalf("%s does not hold the document of %s", path, id)
		}
	}
	var results []map[string]any
	readJSONFile(t, filepath.Join(dir, "results.json"), &results)
	results = slices.DeleteFunc(results, func(r map[string]any) bool { return r["check"] == id })
	writeJSONFile(t, filepath.Join(dir, "results.json"), results)
	var ties map[string]map[string]any
	readJSONFile(t, filepath.Join(dir, "ties.json"), &ties)
	for _, commit := range ties {
		delete(commit, id)
	}
	writeJSONFile(t, filepath.Join(dir, "ties.json"), ties)
}

// readJSONFile decodes the JSON file at path into v.
func readJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(content, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

// writeJSONFile writes v as JSON to path.
func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, out)
}

// editSpecFiles replaces old by new in each files/<commit>/spec.yaml of dir, and fails when one of
// them does not hold old, so that no row passes on a change that changed nothing.
func editSpecFiles(t *testing.T, dir, old, new string) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "files", "*", "spec.yaml"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no spec file under %s: %v", dir, err)
	}
	for _, path := range paths {
		replaceIn(t, path, old, new)
	}
}

// replaceIn replaces the first old by new in the file at path, and fails when the file does not
// hold old.
func replaceIn(t *testing.T, path, old, new string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), old) {
		t.Fatalf("%s does not hold %q", path, old)
	}
	writeFile(t, path, []byte(strings.Replace(string(content), old, new, 1)))
}

// wantOf is the want.txt of the scenario at dir; the test fails, naming dir, when it has none or
// an empty one, since a comparison with nothing reads nothing.
func wantOf(t *testing.T, dir string) string {
	t.Helper()
	want, err := os.ReadFile(filepath.Join(dir, "want.txt"))
	if err != nil || len(want) == 0 {
		t.Fatalf("%s: no want.txt to compare with (%v), so the test reads nothing", dir, err)
	}
	return string(want)
}

// subjectLines is the lines that state printed after the line "spec-check: subject <subject>" and
// before the next line that starts with "spec-check: ".
func subjectLines(stdout, subject string) []string {
	var lines []string
	in := false
	for _, line := range strings.Split(stdout, "\n") {
		switch {
		case line == "spec-check: subject "+subject:
			in = true
		case strings.HasPrefix(line, "spec-check: "):
			in = false
		case in:
			lines = append(lines, line)
		}
	}
	return lines
}

// decodeStrict decodes the one line of JSON that a run of state --json printed into v, refusing a
// field that v does not hold, and fails the test unless the run exited 0 with nothing on stderr.
func decodeStrict(t *testing.T, got result, v any) {
	t.Helper()
	if got.code != 0 || got.stderr != "" || strings.Count(got.stdout, "\n") != 1 {
		t.Fatalf("exit %d, stdout %q, stderr %q: want exit 0, one line of JSON and nothing on stderr", got.code, got.stdout, got.stderr)
	}
	dec := json.NewDecoder(strings.NewReader(got.stdout))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Fatalf("%s: %v", got.stdout, err)
	}
}

// copyScenario is a copy of the scenario name in a directory of the test's own.
func copyScenario(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.CopyFS(dir, os.DirFS(fixture(t, "scenarios", name))); err != nil {
		t.Fatal(err)
	}
	return dir
}

// removeAll removes path and everything under it.
func removeAll(t *testing.T, path string) {
	t.Helper()
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
}
