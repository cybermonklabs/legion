package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/specgraph"
)

// A scenario that the format does not take exits 2, with one line on stderr that names the file
// and the fault, and nothing on stdout.
func TestScenarioRefusals(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	write := func(file, content string) func(t *testing.T, dir string) {
		return func(t *testing.T, dir string) { writeFile(t, filepath.Join(dir, file), []byte(content)) }
	}
	result1 := func(version, kind, outcome string) func(t *testing.T, dir string) {
		return write("results.json", `[{"check":"a-restart-loses-no-message","check_version":`+version+`,"kind":"`+kind+`","outcome":"`+outcome+`","commit":"c1","leg":""}]`)
	}
	replace := func(file, old, new string) func(t *testing.T, dir string) {
		return func(t *testing.T, dir string) { replaceIn(t, filepath.Join(dir, file), old, new) }
	}
	const failingExample = "Failing example: A listener that deletes its stored messages when it starts.\n"
	const firstOutcome = `"outcome": "pass",`
	serving := ":::requirement{#messages-reach-sessions kind=\"behavior\"}"
	loop := ":::requirement{#messages-reach-sessions kind=\"behavior\" serves=\"a-stored-message-survives-a-restart\"}"
	loopDetail := "loop: a-stored-message-survives-a-restart -[parent]-> messages-reach-sessions -[serves]-> a-stored-message-survives-a-restart"
	const resultForm = `{"check":"<check id>","check_version":<whole number from 1>,"kind":"<run or failing_example>","outcome":"<pass, fail or could_not_run>","commit":"<commit>","deploy":"<deploy>","graph_tx":"<transaction>","job":"<job>","leg":"<leg>","attempt":<whole number>,"at":"` + timeForm + `","seq":<whole number from 1>,"fault_author":"<author>","test_authors":["<author>", …],"test_commit":"<commit>"}`
	const unitForm = `{"id":"<unit>","serves":["<statement id>", …],"needs":["<unit>", …],"waits_on":["<statement id>", …]}`
	const historyForm = `{"commits":[{"id":"<commit>","parent":"<commit>","files":{"<repository path>":"<content label>"}}, …]}`
	const jobRunForm = `{"job":"<job>","leg":"<leg>","commit":"<commit>","ended":<true or false>}`
	const uncitedForm = `{"<commit>":{"<check id>":"<R48 rule>"}}`
	const mainAtC4 = `"packages/envoy/cmd/listener/main.go": "2"` // the one path of carry's history.json at c4 with label 2
	const repoPathRule = `, whose parts "/" separates and none of which is empty, "." or ".."`
	for _, row := range []struct {
		name, scenario string
		change         func(t *testing.T, dir string)
		file, fault    string // the line is "spec-check: <dir>/<file>: <fault>"
	}{
		{"an unknown field in results.json", "carry",
			write("results.json", `[{"check":"a-restart-loses-no-message","check_version":1,"kind":"run","outcome":"pass","commit":"c1","bogus":true}]`),
			"results.json", `result 1: member "/bogus" is not in the format`},
		// R9: "No setting makes the failing example optional." The files of each commit pass the rules
		// of spec-check tree, or the scenario is refused with the first problem.
		{"a check with no failing example", "carry", func(t *testing.T, dir string) {
			editApproved(t, filepath.Join(dir, "approved.json"), func(nodes []specgraph.ApprovedNode) []specgraph.ApprovedNode {
				v := &approvedNode(t, nodes, "a-restart-loses-no-message").Versions[0]
				v.Block = strings.Replace(v.Block, failingExample, "", 1)
				return nodes
			})
			editSpecFiles(t, dir, failingExample, "")
		}, filepath.Join("files", "c2", "spec.md") + ":20", "a-restart-loses-no-message: no-failing-example: the check has no failing example"},
		// R6: "Each leaf statement has one or more checks."
		{"a leaf with no check", "carry", func(t *testing.T, dir string) { removeCheck(t, dir, "production-delivery-takes-under-a-second") },
			filepath.Join("files", "c2", "spec.md") + ":1", "delivery-is-fast: leaf-without-check: no statement is under it and no check serves it"},
		// R5: "The parent and support relations taken together form no cycle, and Dispatch refuses a
		// change that makes one."
		{"a loop of parent and support relations in the files", "carry", func(t *testing.T, dir string) { editSpecFiles(t, dir, serving, loop) },
			filepath.Join("files", "c2", "spec.md") + ":16", "a-stored-message-survives-a-restart: " + loopDetail},
		{"a loop of parent and support relations in the approved spec", "carry", func(t *testing.T, dir string) {
			looping := specgraph.Statement{ID: "messages-reach-sessions", Kind: specgraph.KindBehavior,
				Serves: []string{"a-stored-message-survives-a-restart"}, Text: "Envoy delivers each message to the session that it is for."}
			block, err := looping.Print()
			if err != nil {
				t.Fatal(err)
			}
			editApproved(t, filepath.Join(dir, "approved.json"), func(nodes []specgraph.ApprovedNode) []specgraph.ApprovedNode {
				approvedNode(t, nodes, "messages-reach-sessions").Versions[0].Block = block
				return nodes
			})
		}, "approved.json", loopDetail},
		// The one strict JSON reader: RFC 8259 "JSON text … MUST be encoded using UTF-8", and a member
		// is the format's, in its own letters, one time in its object.
		{"a field name in capitals", "carry", replace("results.json", firstOutcome, `"OUTCOME": "pass",`),
			"results.json", `result 1: member "/OUTCOME" is not in the format`},
		{"one field two times, in two cases", "carry", replace("results.json", firstOutcome, `"outcome": "fail", "Outcome": "pass",`),
			"results.json", `result 1: member "/Outcome" is not in the format`},
		{"one field two times", "carry", replace("results.json", firstOutcome, `"outcome": "fail", "outcome": "pass",`),
			"results.json", `member "/0/outcome" is in one object two times`},
		{"a byte that is not UTF-8", "carry", replace("subjects.json", `"branch": "main"`, "\"branch\": \"ma\xffin\""),
			"subjects.json", "it is not UTF-8"},
		{"one tie two times", "carry", write("ties.json", `{"c1":{"a-restart-loses-no-message":["packages/envoy/go.mod"],"a-restart-loses-no-message":["docs/x.md"]}}`),
			"ties.json", `member "/c1/a-restart-loses-no-message" is in one object two times`},
		{"a tie glob", "carry", write("ties.json", `{"c1":{"a-restart-loses-no-message":["p/*.go"]}}`),
			"ties.json", `commit "c1": check "a-restart-loses-no-message": specgraph: tie entry "p/*.go" is not a repository path, <dir>/* or <dir>/**`},
		// A tie is a fact of a commit (R18: "At each commit, Dispatch derives four ties"), and a commit
		// of ties.json is taken by the rule that the reader takes the commit of a subject with.
		{"a commit of ties.json that holds only blanks", "carry", write("ties.json", `{" \t ":{"a-restart-loses-no-message":["go.work"]}}`),
			"ties.json", `commit " \t ": a commit of no letter and no digit names no commit`},
		{"ties by check, with no commit", "carry", write("ties.json", `{"a-restart-loses-no-message":["go.work"]}`),
			"ties.json", `it is not in the form {"<commit>":{"<check id>":["<path or pattern>", …]}}: the value at "/a-restart-loses-no-message" is of another type`},
		{"a history whose commits are a string", "carry", write("history.json", `{"commits":"c1"}`),
			"history.json", `specgraph: history: it is not in the form {"commits":[{"id":"<commit>","parent":"<commit>","files":{"<repository path>":"<content label>"}}, …]}: the value at "/commits" is of another type`},
		{"a result whose check_version is a string", "carry", result1(`"1"`, "run", "pass"),
			"results.json", `result 1: it is not in the form ` + resultForm + `: the value at "/check_version" is of another type`},
		{"a result whose time does not parse", "carry",
			write("results.json", `[{"check":"a-restart-loses-no-message","check_version":1,"kind":"run","outcome":"pass","commit":"c1","at":"yesterday"}]`),
			"results.json", `result 1: it is not in the form ` + resultForm + `: the value at "/at" is of another type`},
		{"a job run whose ended is a string", "carry", write("jobs.json", `[{"job":"go-checks","leg":"","commit":"c1","ended":"yes"}]`),
			"jobs.json", `job run 1: it is not in the form ` + jobRunForm + `: the value at "/ended" is of another type`},
		{"a unit whose serves is a string", "wait-cycle", write("units.json", `[{"id":"u-a","serves":"a-message-survives-a-restart","needs":[],"waits_on":[]}]`),
			"units.json", `unit 1: it is not in the form {"id":"<unit>","serves":["<statement id>", …],"needs":["<unit>", …],"waits_on":["<statement id>", …]}: the value at "/serves" is of another type`},
		{"subjects.json that is no array", "carry", write("subjects.json", `{"commit":"c2"}`),
			"subjects.json", `it is not in the form [{"commit":"<commit>","branch":"<branch>","deploy":"<deploy>","graph_tx":"<transaction>","now":"` + timeForm + `"}, …]`},
		{"an uncited check whose rule is a list", "carry", write("uncited.json", `{"c1":{"a-restart-loses-no-message":["x"]}}`),
			"uncited.json", `it is not in the form ` + uncitedForm + `: the value at "/c1/a-restart-loses-no-message" is of another type`},
		// A JSON null says nothing at its place, so the reader refuses it at each place of each file,
		// with its pointer: a content label of null would read as the empty label, so a changed file
		// would look unchanged and a result would carry (R8); a jobs.json of null would read as no run
		// of a job, which hides a leg that ended with no result (R9); an uncited.json of null, or a map
		// of one commit of null, would read as no refusal (R48); and a list of null as no list.
		{"a content label of the history that is null", "carry",
			replace("history.json", `"packages/envoy/cmd/listener/main.go": "2"`, `"packages/envoy/cmd/listener/main.go": null`),
			"history.json", `specgraph: history: it is not in the form ` + historyForm + `: the value at "/commits/5/files/packages~1envoy~1cmd~1listener~1main.go" is null`},
		{"a jobs.json that is null", "carry", write("jobs.json", `null`), "jobs.json", `it is not in the form [` + jobRunForm + `, …]: it is null`},
		{"a member of a job run that is null", "carry", write("jobs.json", `[{"job":"go-checks","leg":null,"commit":"c1","ended":true}]`),
			"jobs.json", `it is not in the form [` + jobRunForm + `, …]: the value at "/0/leg" is null`},
		{"an uncited.json that is null", "carry", write("uncited.json", `null`), "uncited.json", `it is not in the form ` + uncitedForm + `: it is null`},
		{"a map of one commit of uncited.json that is null", "carry", write("uncited.json", `{"c2":null}`),
			"uncited.json", `it is not in the form ` + uncitedForm + `: the value at "/c2" is null`},
		{"an approved spec with a list of nodes that is null", "carry", write("approved.json", `{"nodes":null}`),
			"approved.json", `specgraph: approved spec: it is not in the form ` + approvedForm + `: the value at "/nodes" is null`},
		// R48: CI refuses a check at a commit; a scenario says which it refused, {} for none, as
		// jobs.json says which jobs ran, [] for none. One that does not hold uncited.json says nothing
		// of the refusals, which would read as none.
		{"no uncited.json", "carry", func(t *testing.T, dir string) { removeAll(t, filepath.Join(dir, "uncited.json")) },
			"uncited.json", "does not exist"},
		{"a subject with a branch and a deploy", "carry",
			write("subjects.json", `[{"commit":"c2","branch":"listener-restart","deploy":"d1","now":"2026-11-14T00:00:00Z"}]`),
			"subjects.json", "subject 1: it sets both branch and deploy; a subject sets exactly one of them"},
		{"a subject with no branch and no deploy", "carry", write("subjects.json", `[{"commit":"c2","now":"2026-11-14T00:00:00Z"}]`),
			"subjects.json", "subject 1: it sets neither branch nor deploy; a subject sets exactly one of them"},
		// The head of a branch reads no graph: a subject names the transaction of the graph only
		// with a deploy.
		{"a subject of a branch with a transaction of the graph", "carry",
			write("subjects.json", `[{"commit":"c2","branch":"listener-restart","graph_tx":"tx1","now":"2026-11-14T00:00:00Z"}]`),
			"subjects.json", "subject 1: it sets graph_tx with no deploy; the commit that production runs reads the graph, and the head of a branch does not"},
		// A graph_tx of no letter and no digit, by the test that the reader takes a missing text with,
		// names no transaction (TestAGraphCheckIsJudgedAtTheTransactionOfItsSubject takes tx1).
		{"a subject whose graph_tx holds only blanks", "carry",
			replace("subjects.json", `"branch": "listener-change"`, `"deploy": "d1", "graph_tx": " \t "`),
			"subjects.json", "subject 4: it sets a graph_tx of no letter and no digit, which names no transaction of the graph"},
		{"a subject whose commit holds only blanks", "carry", func(t *testing.T, dir string) {
			replaceIn(t, filepath.Join(dir, "subjects.json"), `"commit": "c4"`, `"commit": " \t "`)
			if err := os.Rename(filepath.Join(dir, "files", "c4"), filepath.Join(dir, "files", " \t ")); err != nil {
				t.Fatal(err)
			}
		}, "subjects.json", "subject 4: it sets a commit of no letter and no digit, which names no commit"},
		{"a subject whose branch holds only blanks", "carry", replace("subjects.json", `"branch": "listener-change"`, `"branch": " \t "`),
			"subjects.json", "subject 4: it sets a branch of no letter and no digit, which names no branch"},
		{"a subject whose deploy holds only blanks", "carry", replace("subjects.json", `"branch": "listener-change"`, `"deploy": " \t "`),
			"subjects.json", "subject 4: it sets a deploy of no letter and no digit, which names no deploy"},
		{"a subject with no time", "carry", write("subjects.json", `[{"commit":"c2","branch":"listener-restart"}]`),
			"subjects.json", "subject 1: it sets no now"},
		{"no subject", "carry", write("subjects.json", `[]`), "subjects.json", "it holds no subject"},
		{"a file that the format does not hold", "carry", write("notes.txt", "A note.\n"),
			"notes.txt", "the format of a scenario holds no such file"},
		{"no subjects.json", "carry", func(t *testing.T, dir string) { removeAll(t, filepath.Join(dir, "subjects.json")) },
			"subjects.json", "does not exist"},
		{"a spec file that does not parse", "carry", write(filepath.Join("files", "c2", "spec.md"), "A line outside a block.\n"),
			filepath.Join("files", "c2", "spec.md") + ":1", "-: text-outside-block: a spec file holds only blocks, headings and blank lines"},
		{"a subject whose commit has no spec files", "carry", func(t *testing.T, dir string) { removeAll(t, filepath.Join(dir, "files", "c4")) },
			filepath.Join("files", "c4"), "does not exist, and subject 4 reads the spec files of its commit there"},
		{"spec files that no subject reads", "carry", func(t *testing.T, dir string) {
			if err := os.CopyFS(filepath.Join(dir, "files", "c9"), os.DirFS(filepath.Join(dir, "files", "c2"))); err != nil {
				t.Fatal(err)
			}
		}, filepath.Join("files", "c9"), "no subject reads the spec files of this commit"},
		{"a result of a version that the approved spec does not hold", "carry", result1("2", "run", "pass"),
			"results.json", `result 1: the approved spec holds no version 2 of check "a-restart-loses-no-message"`},
		{"a result of another kind", "carry", result1("1", "rerun", "pass"),
			"results.json", `result 1: kind "rerun" is not run or failing_example`},
		{"a result of another outcome", "carry", result1("1", "run", "skip"),
			"results.json", `result 1: outcome "skip" is not pass, fail or could_not_run`},
		// A result names the one subject that it read: a commit, a deploy or a transaction of the graph.
		{"a result that names two subjects", "carry",
			write("results.json", `[{"check":"a-restart-loses-no-message","check_version":1,"kind":"run","outcome":"pass","commit":"c1","graph_tx":"tx1","leg":""}]`),
			"results.json", "result 1: it names the subjects commit and graph_tx; a result reads one subject"},
		// R12: "Each leg of a matrix reports its own result, and no leg replaces the result of another."
		// A result or a job run that leaves out leg would read as the leg of no matrix, "", which a
		// result of the job from before its matrix reports: a leg that ended with no result would read
		// as reported. So each sets leg, "" for a job with no matrix.
		{"a result with no leg", "carry",
			write("results.json", `[{"check":"a-restart-loses-no-message","check_version":1,"kind":"run","outcome":"pass","commit":"c1","job":"go-checks","attempt":1,"at":"2026-11-12T10:00:00Z","seq":1}]`),
			"results.json", `result 1: it sets no leg; a result of no matrix sets ""`},
		{"a job run with no leg", "carry", write("jobs.json", `[{"job":"go-checks","commit":"c1","ended":true}]`),
			"jobs.json", `job run 1: it sets no leg; a run of no matrix sets ""`},
		// A member that a result does not set would be its zero value, and the derivation would read
		// that value as a real one: a failure with no at as older than every approval, one with no
		// subject, no job or no test_commit as one that counts nowhere, and of two results at one at and
		// one seq the one listed first as the later. Each hides a failure (R7: "A pass and a failure on
		// one such subject and one procedure make the state unknown"), so the reader refuses it.
		{"a result with no at", "carry",
			write("results.json", `[{"check":"a-restart-loses-no-message","check_version":1,"kind":"run","outcome":"fail","commit":"c1","job":"go-checks","leg":"","seq":5}]`),
			"results.json", "result 1: it sets no at"},
		{"a run of a check of the code with no commit", "carry",
			write("results.json", `[{"check":"a-restart-loses-no-message","check_version":1,"kind":"run","outcome":"fail","job":"go-checks","leg":"","at":"2026-11-12T11:00:00Z","seq":5}]`),
			"results.json", "result 1: it sets no commit, the subject that a check of the code reads"},
		{"a failing example of a check of the code with no commit", "carry",
			write("results.json", `[{"check":"a-restart-loses-no-message","check_version":1,"kind":"failing_example","outcome":"pass","job":"go-checks","leg":"","at":"2026-11-12T11:00:00Z","seq":5}]`),
			"results.json", "result 1: it sets no commit, the subject that a check of the code reads"},
		{"a run of a check of production with no deploy", "carry",
			write("results.json", `[{"check":"production-delivery-takes-under-a-second","check_version":1,"kind":"run","outcome":"fail","leg":"","at":"2026-11-12T12:00:00Z","seq":4}]`),
			"results.json", "result 1: it sets no deploy, the subject that a check of production reads"},
		{"a run of a program check with no job", "carry",
			write("results.json", `[{"check":"a-restart-loses-no-message","check_version":1,"kind":"run","outcome":"fail","commit":"c1","leg":"","at":"2026-11-12T11:00:00Z","seq":5}]`),
			"results.json", "result 1: it sets no job, and only the reports of the job of its check count"},
		{"two results of a check at one at and one seq", "carry", write("results.json", `[`+
			`{"check":"a-restart-loses-no-message","check_version":1,"kind":"run","outcome":"pass","commit":"c1","job":"go-checks","leg":"","at":"2026-11-12T10:00:00Z","seq":1},`+
			`{"check":"a-restart-loses-no-message","check_version":1,"kind":"run","outcome":"fail","commit":"c1","job":"go-checks","leg":"","at":"2026-11-12T10:00:00Z","seq":1}]`),
			"results.json", "result 2: it has the at and the seq of result 1, so neither of the two is the later"},
		// The order of record is at, then seq, which Dispatch numbers from 1: a result with no seq
		// would read as seq 0, before each result of its at, so a later failure would be hidden.
		{"a result with no seq", "carry",
			write("results.json", `[{"check":"a-restart-loses-no-message","check_version":1,"kind":"run","outcome":"fail","commit":"c1","job":"go-checks","leg":"","at":"2026-11-12T11:00:00Z"}]`),
			"results.json", "result 1: it sets no seq"},
		{"a result with seq 0", "carry",
			write("results.json", `[{"check":"a-restart-loses-no-message","check_version":1,"kind":"run","outcome":"fail","commit":"c1","job":"go-checks","leg":"","at":"2026-11-12T11:00:00Z","seq":0}]`),
			"results.json", "result 1: it sets no seq"},
		// A job run with no job or no commit is a run of no job, or at no commit, and one that does not
		// set ended would read as one that did not end: each hides a leg that ended with no result (R9:
		// "When a check has no result that counts after a run of its job ends, it reads unknown"). A
		// run that has not ended sets "ended": false.
		{"a job run with no ended", "carry", write("jobs.json", `[{"job":"go-checks","leg":"","commit":"c1"}]`),
			"jobs.json", "job run 1: it sets no ended; a run that has not ended sets false"},
		{"a job run with no job", "carry", write("jobs.json", `[{"leg":"","commit":"c1","ended":true}]`),
			"jobs.json", "job run 1: it sets no job"},
		{"a job run with no commit", "carry", write("jobs.json", `[{"job":"go-checks","leg":"","ended":true}]`),
			"jobs.json", "job run 1: it sets no commit"},
		// A commit of the history with no files would read as an empty tree, and two such trees as
		// one, so a result would carry between them (R8); so would a tree with no path, {}, since a
		// test reads at least one file, and a content label "" at two commits, since a label names
		// the content of a file. A history.json that does not set commits would read as no commit,
		// which its writer may mean: it sets [] for that.
		{"a commit of the history with no files", "carry", write("history.json", `{"commits":[{"id":"m1"}]}`),
			"history.json", `specgraph: history: commit "m1": it sets no files`},
		{"a commit of the history whose tree holds no path", "carry",
			write("history.json", `{"commits":[{"id":"m1","files":{"docs/x.md":"1"}},{"id":"c1","parent":"m1","files":{}}]}`),
			"history.json", `specgraph: history: commit "c1": its files hold no path, and the tree of a commit holds at least one file`},
		{"a content label of the history that is empty", "carry",
			replace("history.json", `"packages/envoy/cmd/listener/main.go": "2"`, `"packages/envoy/cmd/listener/main.go": ""`),
			"history.json", `specgraph: history: commit "c4": the content label of "packages/envoy/cmd/listener/main.go" is empty, and a label has at least one character`},
		// R48: a refusal of CI names the rule that refused the check; an empty rule names none.
		{"a rule of uncited.json that is empty", "carry", write("uncited.json", `{"c2":{"a-restart-loses-no-message":""}}`),
			"uncited.json", `commit "c2": check "a-restart-loses-no-message": its rule is empty, and a refusal of CI names the rule that refused the check`},
		{"a history that does not set commits", "carry", write("history.json", `{}`),
			"history.json", "specgraph: history: it sets no commits; a history of no commit sets an empty list"},
		{"a history whose commits are null", "carry", write("history.json", `{"commits":null}`),
			"history.json", `specgraph: history: it is not in the form ` + historyForm + `: the value at "/commits" is null`},
		// A unit with no serves, needs or waits_on would read as one with none, which hides a cycle of
		// waits (R29); [] is none.
		{"a unit with no serves", "wait-cycle", write("units.json", `[{"id":"u-a","needs":[],"waits_on":[]}]`),
			"units.json", `unit "u-a": it sets no serves; a unit with none sets an empty list`},
		{"a unit with no needs", "wait-cycle", write("units.json", `[{"id":"u-a","serves":[],"waits_on":[]}]`),
			"units.json", `unit "u-a": it sets no needs; a unit with none sets an empty list`},
		{"a unit whose waits_on is null", "wait-cycle", write("units.json", `[{"id":"u-a","serves":[],"needs":[],"waits_on":null}]`),
			"units.json", `it is not in the form [` + unitForm + `, …]: the value at "/0/waits_on" is null`},
		{"a unit with no id", "wait-cycle", write("units.json", `[{"serves":[],"needs":[],"waits_on":[]}]`),
			"units.json", "unit 1 has no id"},
		{"a wait on no statement", "wait-cycle", write("units.json", `[{"id":"u-a","serves":[],"needs":[],"waits_on":["no-such-statement"]}]`),
			"units.json", `unit "u-a": waits_on names "no-such-statement", which is no current statement of the approved spec`},
		{"a need of no unit", "wait-cycle", write("units.json", `[{"id":"u-a","serves":[],"needs":["u-x"],"waits_on":[]}]`),
			"units.json", `unit "u-a": needs names "u-x", which is no unit of the file`},
		// A path of the history is a repository path, as a tie entry is (isRepoPath): no tie covers
		// "./a", "/a", "a/", "a//b", "" or ".", so a change at it would carry a result (R8).
		{"a path of the history that starts with ./", "carry", replace("history.json", mainAtC4, `"./packages/envoy/cmd/listener/main.go": "2"`),
			"history.json", `specgraph: history: commit "c4": the path "./packages/envoy/cmd/listener/main.go" is no repository path` + repoPathRule},
		{"a path of the history that starts with /", "carry", replace("history.json", mainAtC4, `"/packages/envoy/cmd/listener/main.go": "2"`),
			"history.json", `specgraph: history: commit "c4": the path "/packages/envoy/cmd/listener/main.go" is no repository path` + repoPathRule},
		{"a path of the history that ends with /", "carry", replace("history.json", mainAtC4, `"packages/envoy/cmd/listener/main.go/": "2"`),
			"history.json", `specgraph: history: commit "c4": the path "packages/envoy/cmd/listener/main.go/" is no repository path` + repoPathRule},
		{"a path of the history with a doubled /", "carry", replace("history.json", mainAtC4, `"packages//envoy/cmd/listener/main.go": "2"`),
			"history.json", `specgraph: history: commit "c4": the path "packages//envoy/cmd/listener/main.go" is no repository path` + repoPathRule},
		{"a path of the history that is empty", "carry", replace("history.json", mainAtC4, `"": "2"`),
			"history.json", `specgraph: history: commit "c4": the path "" is no repository path` + repoPathRule},
		{"a path of the history that is .", "carry", replace("history.json", mainAtC4, `".": "2"`),
			"history.json", `specgraph: history: commit "c4": the path "." is no repository path` + repoPathRule},
		{"a path of the history that is /", "carry", replace("history.json", mainAtC4, `"/": "2"`),
			"history.json", `specgraph: history: commit "c4": the path "/" is no repository path` + repoPathRule},
		{"a commit of the history whose id holds only blanks", "carry", write("history.json", `{"commits":[{"id":" ","files":{"go.work":"1"}}]}`),
			"history.json", `specgraph: history: commit " ": its id has no letter and no digit, so it names no commit`},
		{"a commit of the history whose parent holds only blanks", "carry", write("history.json", `{"commits":[{"id":"c1","parent":" ","files":{"go.work":"1"}}]}`),
			"history.json", `specgraph: history: commit "c1": it sets a parent of no letter and no digit, which names no commit`},
		// A commit or a check that a key of uncited.json or of ties.json names has a letter or a
		// digit, as a commit of a subject has: a refusal or a tie under a key of none counts nowhere.
		{"a commit of uncited.json that is empty", "carry", write("uncited.json", `{"":{"a-restart-loses-no-message":"test-not-in-code"}}`),
			"uncited.json", `commit "": a commit of no letter and no digit names no commit`},
		{"a commit of uncited.json that holds only blanks", "carry", write("uncited.json", `{" ":{"a-restart-loses-no-message":"test-not-in-code"}}`),
			"uncited.json", `commit " ": a commit of no letter and no digit names no commit`},
		{"a check of uncited.json that is empty", "carry", write("uncited.json", `{"c2":{"":"test-not-in-code"}}`),
			"uncited.json", `commit "c2": check "": a check of no letter and no digit names no check`},
		{"a check of uncited.json that holds only blanks", "carry", write("uncited.json", `{"c2":{" ":"test-not-in-code"}}`),
			"uncited.json", `commit "c2": check " ": a check of no letter and no digit names no check`},
		{"a check of ties.json that is empty", "carry", write("ties.json", `{"c1":{"":["go.work"]}}`),
			"ties.json", `commit "c1": check "": a check of no letter and no digit names no check`},
		{"a check of ties.json that holds only blanks", "carry", write("ties.json", `{"c1":{" \t ":["go.work"]}}`),
			"ties.json", `commit "c1": check " \t ": a check of no letter and no digit names no check`},
		// A job, a leg, a commit, a deploy or a test commit that a result or a job run names has a
		// letter or a digit: a run of job " " is a run of no job of the check, so a leg that ended
		// with no result would be hidden, and a failure at deploy " " or test commit " " would count
		// nowhere.
		{"a job run whose job holds only blanks", "carry", write("jobs.json", `[{"job":" ","leg":"","commit":"c1","ended":true}]`),
			"jobs.json", "job run 1: it sets a job of no letter and no digit, which names no job"},
		{"a job run whose leg holds only blanks", "carry", write("jobs.json", `[{"job":"go-checks","leg":" ","commit":"c1","ended":true}]`),
			"jobs.json", "job run 1: it sets a leg of no letter and no digit, which names no leg"},
		{"a job run whose commit holds only blanks", "carry", write("jobs.json", `[{"job":"go-checks","leg":"","commit":" ","ended":true}]`),
			"jobs.json", "job run 1: it sets a commit of no letter and no digit, which names no commit"},
		{"a run whose job holds only blanks", "carry",
			write("results.json", `[{"check":"a-restart-loses-no-message","check_version":1,"kind":"run","outcome":"fail","commit":"c1","job":" ","leg":"","at":"2026-11-12T11:00:00Z","seq":5}]`),
			"results.json", "result 1: it sets a job of no letter and no digit, which names no job"},
		{"a run whose commit holds only blanks", "carry",
			write("results.json", `[{"check":"a-restart-loses-no-message","check_version":1,"kind":"run","outcome":"fail","commit":" ","job":"go-checks","leg":"","at":"2026-11-12T11:00:00Z","seq":5}]`),
			"results.json", "result 1: it sets a commit of no letter and no digit, which names no commit"},
		{"a run of a check of production whose deploy holds only blanks", "carry",
			write("results.json", `[{"check":"production-delivery-takes-under-a-second","check_version":1,"kind":"run","outcome":"fail","deploy":" ","leg":"","at":"2026-11-12T12:00:00Z","seq":4}]`),
			"results.json", "result 1: it sets a deploy of no letter and no digit, which names no deploy"},
		{"a unit whose id holds only blanks", "wait-cycle", write("units.json", `[{"id":" ","serves":[],"needs":[],"waits_on":[]}]`),
			"units.json", "unit 1: its id has no letter and no digit, so it names no unit"},
		// R12: "Each leg of a matrix reports its own result, and no leg replaces the result of
		// another." One job at one commit has legs or it has none, "" being the leg of no matrix: a
		// run of no matrix beside a run of leg linux, or a run result of no matrix where the job's runs
		// there have legs, would be answered by an older run result of the leg "" that counts there
		// (TestARunResultOfNoMatrixAnswersNoRunWhereItsJobHadLegs).
		{`runs of one job at one commit in the leg "" and in a named leg`, "silent-leg", replace("jobs.json", `"leg": "arm"`, `"leg": ""`),
			"jobs.json", `job run 2: it sets the leg "", and job run 1 of its job "go-checks" at the commit "m2" sets the leg "linux"; one job at one commit has legs or it has none, and "" is the leg of no matrix`},
		{`a run result in the leg "" where the runs of its job at its commit name legs`, "carry", write("jobs.json", `[{"job":"go-checks","leg":"linux","commit":"c1","ended":true}]`),
			"results.json", `result 1: it sets the leg "", and job run 1 of its job "go-checks" at the commit "c1" sets the leg "linux"; one job at one commit has legs or it has none, and "" is the leg of no matrix`},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			dir := copyScenario(t, row.scenario)
			row.change(t, dir)
			expect(t, runBinary(t, binary, "", "state", "--scenario", dir), result{code: 2, stderr: "spec-check: " + filepath.Join(dir, row.file) + ": " + row.fault + "\n"})
		})
	}
	t.Run("a scenario directory that does not exist", func(t *testing.T) {
		t.Parallel()
		missing := filepath.Join(t.TempDir(), "missing")
		expect(t, runBinary(t, binary, "", "state", "--scenario", missing), result{code: 2, stderr: "spec-check: " + missing + ": does not exist\n"})
	})
}

// A node of approved.json sets removed and partly_checked: one that sets neither would read as
// false, so a check that the approved spec removed would be judged, and could make its statement
// met where it reads unknown, and a statement would lose its "partly checked". Each row takes the
// member out of the node of carry's check a-restart-loses-no-message (node 5 of the file) and runs
// state on the scenario, and compare on its files at c2 with its approved.json: each exits 2 with
// nothing on stdout and one line on stderr that names the file, the node and the member, in the
// words in which the entry of the library refuses the same node.
func TestAnApprovedNodeSetsRemovedAndPartlyChecked(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	for _, member := range []string{"removed", "partly_checked"} {
		t.Run("a node that sets no "+member, func(t *testing.T) {
			t.Parallel()
			dir := copyScenario(t, "carry")
			approved := filepath.Join(dir, "approved.json")
			var file struct {
				Nodes []map[string]any `json:"nodes"`
			}
			readJSONFile(t, approved, &file)
			if file.Nodes[4]["id"] != "a-restart-loses-no-message" {
				t.Fatalf("node 5 of %s is %v, not a-restart-loses-no-message", approved, file.Nodes[4]["id"])
			}
			delete(file.Nodes[4], member)
			writeJSONFile(t, approved, file)
			explicit := map[string]string{"removed": "a node that the approved spec keeps sets false", "partly_checked": "a statement whose checks decide all of it sets false"}
			want := result{code: 2, stderr: "spec-check: " + approved + `: specgraph: approved node "a-restart-loses-no-message": it sets no ` + member + "; " + explicit[member] + "\n"}
			expect(t, runBinary(t, binary, "", "state", "--scenario", dir), want)
			expect(t, runBinary(t, binary, "", "compare", "--dir", filepath.Join(dir, "files", "c2"), "--approved", approved), want)
		})
	}
}

// A key of ties.json, history.json and uncited.json is the writer's own text, and a refusal of a
// value of another type names the place of the value, so the place holds the key. Each row: a key
// of 262,144 bytes whose value is of another type. The command exits 2 with nothing on stdout and
// one line on stderr that names the file, the form and the place, and the place as it prints it
// holds at most placeBound bytes: the bound of decodeJSON, which prints a place whole up to 100
// bytes, else its first and its last 50 bytes with … between them. The line names the place by its
// start and its end, and grows with no byte of the key.
func TestARefusalNamesALongKeyOfTheWriterInPart(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	const placeBound = 100 + len("…")
	start := "a-name-of-the-writer-"
	key := start + strings.Repeat("y", 262144-len(start)-len("-end")) + "-end"
	for _, row := range []struct {
		name, file, content string
		form                string // the line is "spec-check: <dir>/<file>: <form>: the value at "<place>" is of another type"
		place               string // the place whole
	}{
		{"a check of ties.json whose name is long and whose tie is a number", "ties.json", `{"c1":{"` + key + `":7}}`,
			`it is not in the form {"<commit>":{"<check id>":["<path or pattern>", …]}}`, "/c1/" + key},
		{"a path of history.json whose name is long and whose label is a number", "history.json",
			`{"commits":[{"id":"c1","files":{"` + key + `":7}}]}`,
			`specgraph: history: it is not in the form {"commits":[{"id":"<commit>","parent":"<commit>","files":{"<repository path>":"<content label>"}}, …]}`,
			"/commits/0/files/" + key},
		{"a check of uncited.json whose name is long and whose rule is a list", "uncited.json", `{"c1":{"` + key + `":["x"]}}`,
			`it is not in the form {"<commit>":{"<check id>":"<R48 rule>"}}`, "/c1/" + key},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			dir := copyScenario(t, "carry")
			writeFile(t, filepath.Join(dir, row.file), []byte(row.content))
			got := runBinary(t, binary, "", "state", "--scenario", dir)
			before := "spec-check: " + filepath.Join(dir, row.file) + ": " + row.form + `: the value at "`
			const after = `" is of another type` + "\n"
			if got.code != 2 || got.stdout != "" || strings.Count(got.stderr, "\n") != 1 ||
				!strings.HasPrefix(got.stderr, before) || !strings.HasSuffix(got.stderr, after) {
				t.Fatalf("exit %d, stdout %d bytes, stderr %d bytes starting %q\nwant exit 2, nothing on stdout and one line %q<place>%q",
					got.code, len(got.stdout), len(got.stderr), got.stderr[:min(len(got.stderr), 300)], before, after)
			}
			place := got.stderr[len(before) : len(got.stderr)-len(after)]
			if len(place) > placeBound || !strings.HasPrefix(place, row.place[:40]) || !strings.HasSuffix(place, row.place[len(row.place)-40:]) {
				t.Errorf("the line is %d bytes, and its place %d bytes, starting %q and ending %q\nwant a place of at most %d bytes that starts %q and ends %q",
					len(got.stderr), len(place), place[:min(len(place), 60)], place[max(0, len(place)-60):], placeBound, row.place[:40], row.place[len(row.place)-40:])
			}
		})
	}
}

// Each other value of a scenario's files that a refusal of the reader prints is the writer's own
// text too: a commit of a subject, a check, a kind and an outcome of a result, a commit and a path of
// the history, a commit and a check of uncited.json, a commit, a check and an entry of a tie, and a
// unit and the ids that it names. Each row: a value of 262,144 bytes where the reader refuses it.
// The command exits 2 with nothing on stdout and one line on stderr that names the file and the
// value by its first and its last bytes, and that holds at most 700 bytes beside the path of the
// scenario, since each value prints as decodeJSON prints a place, whole up to 100 bytes, else its
// first and its last 50 bytes with … between them.
func TestARefusalNamesALongValueOfTheWriterInPart(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	long := func(start string) string { return start + strings.Repeat("y", 262144-len(start)-len("-end")) + "-end" }
	commit, check, unit, blanks := long("a-commit-"), long("a-check-"), long("a-unit-"), strings.Repeat(" ", 262144)
	const now = `"now":"2026-11-14T00:00:00Z"`
	result := func(check, kind, outcome string) string {
		return `[{"check":"` + check + `","check_version":1,"kind":"` + kind + `","outcome":"` + outcome + `","commit":"c1","job":"go-checks","leg":"","at":"2026-11-12T10:00:00Z","seq":1}]`
	}
	for _, row := range []struct {
		name, scenario, file, content string
		value                         string // the long value that the line names
	}{
		{"a commit of a subject that names no directory", "carry", "subjects.json", `[{"commit":"a/` + commit + `","branch":"b",` + now + `}]`, "a/" + commit},
		{"a commit of a subject too long for a directory", "carry", "subjects.json", `[{"commit":"` + commit + `","branch":"b",` + now + `}]`, commit},
		{"a check of a result", "carry", "results.json", result(check, "run", "pass"), check},
		{"a kind of a result", "carry", "results.json", result("a-restart-loses-no-message", check, "pass"), check},
		{"an outcome of a result", "carry", "results.json", result("a-restart-loses-no-message", "run", check), check},
		{"a commit of the history two times", "carry", "history.json", `{"commits":[{"id":"` + commit + `","files":{}},{"id":"` + commit + `","files":{}}]}`, commit},
		{"a commit of the history with no files", "carry", "history.json", `{"commits":[{"id":"` + commit + `"}]}`, commit},
		{"a commit of the history whose tree holds no path", "carry", "history.json", `{"commits":[{"id":"` + commit + `","files":{}}]}`, commit},
		{"a path of the history whose content label is empty", "carry", "history.json", `{"commits":[{"id":"c1","files":{"` + check + `":""}}]}`, check},
		{"a commit of uncited.json whose rule is empty", "carry", "uncited.json", `{"` + commit + `":{"a-restart-loses-no-message":""}}`, commit},
		{"a check of uncited.json whose rule is empty", "carry", "uncited.json", `{"c1":{"` + check + `":""}}`, check},
		{"a commit of ties.json of no letter and no digit", "carry", "ties.json", `{"` + blanks + `":{}}`, blanks},
		{"a check of ties.json", "carry", "ties.json", `{"c1":{"` + check + `":["p/*.go"]}}`, check},
		{"an entry of a tie", "carry", "ties.json", `{"c1":{"a-restart-loses-no-message":["` + check + `*"]}}`, check + "*"},
		{"a unit two times", "wait-cycle", "units.json", `[{"id":"` + unit + `","serves":[],"needs":[],"waits_on":[]},{"id":"` + unit + `","serves":[],"needs":[],"waits_on":[]}]`, unit},
		{"a unit that serves no statement", "wait-cycle", "units.json", `[{"id":"` + unit + `","serves":["` + check + `"],"needs":[],"waits_on":[]}]`, check},
		{"a unit that needs no unit", "wait-cycle", "units.json", `[{"id":"` + unit + `","serves":[],"needs":["` + commit + `"],"waits_on":[]}]`, commit},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			dir := copyScenario(t, row.scenario)
			writeFile(t, filepath.Join(dir, row.file), []byte(row.content))
			got := runBinary(t, binary, "", "state", "--scenario", dir)
			start := "spec-check: " + filepath.Join(dir, row.file) + ": "
			if got.code != 2 || got.stdout != "" || strings.Count(got.stderr, "\n") != 1 || !strings.HasPrefix(got.stderr, start) {
				t.Fatalf("exit %d, stdout %d bytes, stderr %d bytes starting %q\nwant exit 2, nothing on stdout and one line that starts %q",
					got.code, len(got.stdout), len(got.stderr), got.stderr[:min(len(got.stderr), 300)], start)
			}
			if len(got.stderr)-len(dir) > 700 || !strings.Contains(got.stderr, row.value[:40]) || !strings.Contains(got.stderr, row.value[len(row.value)-40:]) {
				t.Errorf("the line is %d bytes beside the path of the scenario, starting %q\nwant at most 700 that name the value by its first 40 and its last 40 bytes",
					len(got.stderr)-len(dir), got.stderr[:min(len(got.stderr), 400)])
			}
		})
	}
}
