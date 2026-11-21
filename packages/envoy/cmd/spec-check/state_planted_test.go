package main

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/specgraph"
)

// R9: "For a check that a program runs, the failing example is a fault that a step plants in the
// code." R8: "A failing example that the check's test caught counts at another commit only while no
// file that the test reads changed." The failing example of a program check of the code counts from
// the commit that the step planted its fault on (planted_on), and never at a subject at the fault's
// own commit, which CI tested (commit), whatever planted_on names, and only when each path of the
// fault (fault_paths) is a source file, one that ends in .go, .ts or .tsx, and no file of a test,
// and the tie of the check at planted_on, where it is known, covers it. A record with no planted_on,
// no path, a path of only white space, which is no path, or its own commit as planted_on counts
// nowhere, and as the newest failing example it leaves the check unknown, with the reason that
// names what it lacks. In carry the step planted the fault on c1, in
// packages/envoy/cmd/listener/main.go, a file of the tie, and CI tested it at f1: the fault makes
// the check true at c1, and at c2 and at m2, whose files of the tie equal those of c1, and c3's
// change of a file that the test reads ends it. Each row: carry, with a subject at c1 itself, whose
// spec files are m2's (withSubjectAtC1), and the row's change, read back as the command's reader
// reads it, so that a change that does nothing fails its row by name; the check reads the row's
// line at each commit of the row's map. A build that counts the failing example at f1, the commit
// that CI tested, reads the check unknown at c1, c2 and m2, and fails the control.
func TestAFailingExampleCountsFromTheCommitThatItsFaultWasPlantedOn(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	const check, sender = "a-restart-loses-no-message", "packages/envoy/cmd/sender/main.go"
	const fromC1, none = "true (passed, counted from c1)", "unknown (no_failing_example)"
	subjects := map[string]string{"c1": "c1, the head of c1-head", "c2": "c2, the head of listener-restart", "m2": "m2, the head of main",
		"c3": "c3, the head of contract-fixture", "f1": "f1, the head of spec-fault"}
	atEach := func(line string) map[string]string { return map[string]string{"c1": line, "c2": line, "m2": line} }
	// results changes carry's results.json by change; fault sets the members of set on carry's
	// failing example, result 2, and takes out each member whose value is nil; paths sets its
	// fault_paths, and each path row reads those paths back from the row itself; later adds a second
	// failing example of the check, planted on c1 in a fault f2 of its own after carry's caught one,
	// on which the test missed its fault, with the members of set. fault and later read back each
	// member that they write.
	results := func(change func(r []map[string]any) []map[string]any) func(t *testing.T, dir string) {
		return func(t *testing.T, dir string) {
			var r []map[string]any
			readJSONFile(t, filepath.Join(dir, "results.json"), &r)
			if r[1]["kind"] != "failing_example" || r[1]["commit"] != "f1" || r[1]["planted_on"] != "c1" {
				t.Fatalf("result 2 of carry is no failing example of the fault f1 planted on c1: %v", r[1])
			}
			writeJSONFile(t, filepath.Join(dir, "results.json"), change(r))
		}
	}
	setMembers := func(r, set map[string]any) {
		for member, v := range set {
			if v == nil {
				delete(r, member)
			} else {
				r[member] = v
			}
		}
	}
	fault := func(set map[string]any) func(t *testing.T, dir string) {
		edit := results(func(r []map[string]any) []map[string]any {
			setMembers(r[1], set)
			return r
		})
		return func(t *testing.T, dir string) {
			edit(t, dir)
			readsBack(t, dir, 1, set)
		}
	}
	paths := func(p ...string) func(t *testing.T, dir string) {
		return fault(map[string]any{"fault_paths": append([]string{}, p...)})
	}
	later := func(set map[string]any) func(t *testing.T, dir string) {
		written := map[string]any{"commit": "f2", "outcome": "pass"}
		maps.Copy(written, set)
		edit := results(func(r []map[string]any) []map[string]any {
			missed := withMembers(r[1], map[string]any{"at": "2026-11-12T10:06:00Z", "seq": 5})
			setMembers(missed, written)
			return append(r, missed)
		})
		return func(t *testing.T, dir string) {
			edit(t, dir)
			readsBack(t, dir, 4, written)
		}
	}
	// atF1 adds f1, the fault's own commit, which CI tested, to the history, with the tree of c1,
	// where the fault was planted, the ties of c1 and the spec files of m2, and a subject at f1.
	atF1 := func(t *testing.T, dir string) {
		var history struct {
			Commits []map[string]any `json:"commits"`
		}
		readJSONFile(t, filepath.Join(dir, "history.json"), &history)
		c1 := history.Commits[slices.IndexFunc(history.Commits, func(c map[string]any) bool { return c["id"] == "c1" })]
		history.Commits = append(history.Commits, map[string]any{"id": "f1", "parent": "c1", "files": c1["files"]})
		writeJSONFile(t, filepath.Join(dir, "history.json"), history)
		var ties map[string]any
		readJSONFile(t, filepath.Join(dir, "ties.json"), &ties)
		ties["f1"] = ties["c1"]
		writeJSONFile(t, filepath.Join(dir, "ties.json"), ties)
		if err := os.CopyFS(filepath.Join(dir, "files", "f1"), os.DirFS(filepath.Join(dir, "files", "m2"))); err != nil {
			t.Fatal(err)
		}
		var s []any
		readJSONFile(t, filepath.Join(dir, "subjects.json"), &s)
		writeJSONFile(t, filepath.Join(dir, "subjects.json"), append(s, map[string]any{"commit": "f1", "branch": "spec-fault", "now": "2026-11-14T00:00:00Z"}))
		read := readScenario(t, dir)
		changed, ok, err := read.History.Changed("c1", "f1")
		if !slices.ContainsFunc(read.Subjects, func(s specgraph.ScenarioSubject) bool { return s.Commit == "f1" }) || !read.Ties["f1"][check].Known || !ok || err != nil || len(changed) != 0 {
			t.Fatalf("the reader reads no subject at f1 with the tree and the tie of c1: changed %q, %t, %v", changed, ok, err)
		}
	}
	// tiedAt adds packages/envoy/cmd/sender/* to the tie of the check at each of commits, and gives
	// carry's failing example the fault path packages/envoy/cmd/sender/main.go.
	tiedAt := func(commits ...string) func(t *testing.T, dir string) {
		return func(t *testing.T, dir string) {
			var ties map[string]map[string][]string
			readJSONFile(t, filepath.Join(dir, "ties.json"), &ties)
			for _, c := range commits {
				ties[c][check] = append(slices.Clone(ties[c][check]), "packages/envoy/cmd/sender/*")
			}
			writeJSONFile(t, filepath.Join(dir, "ties.json"), ties)
			paths(sender)(t, dir)
			readsBack(t, dir, 1, map[string]any{"fault_paths": []string{sender}})
			read := readScenario(t, dir)
			for _, c := range []string{"c1", "c2"} {
				if tie := read.Ties[c][check]; !tie.Known || tie.Covers(sender) != slices.Contains(commits, c) {
					t.Fatalf("the tie at %s as the reader reads it: known %t, covers %s %t", c, tie.Known, sender, tie.Covers(sender))
				}
			}
		}
	}
	// refusedAt adds CI's refusal of the check at commit and reads it back as the command's reader
	// reads it.
	refusedAt := func(commit string) func(t *testing.T, dir string) {
		return func(t *testing.T, dir string) {
			var uncited map[string]map[string]string
			readJSONFile(t, filepath.Join(dir, "uncited.json"), &uncited)
			if uncited[commit] == nil {
				uncited[commit] = map[string]string{}
			}
			uncited[commit][check] = "test-not-in-code"
			writeJSONFile(t, filepath.Join(dir, "uncited.json"), uncited)
			if got := readScenario(t, dir).Uncited[commit][check]; got != "test-not-in-code" {
				t.Fatalf("the reader reads CI's refusal of %s at %s as %q", check, commit, got)
			}
		}
	}
	// then applies each of changes in order, and each must apply: it fails the row by name when the
	// scenario, as the command's reader reads it, is the same after a change as before it, and when
	// it applied fewer changes than it was given, so that a row whose refusal is dropped, which reads
	// the same state without it, fails.
	then := func(changes ...func(t *testing.T, dir string)) func(t *testing.T, dir string) {
		return func(t *testing.T, dir string) {
			applied := 0
			for i, change := range changes {
				before := readScenario(t, dir)
				change(t, dir)
				if reflect.DeepEqual(readScenario(t, dir), before) {
					t.Fatalf("change %d of %d left the scenario as the reader reads it", i+1, len(changes))
				}
				applied++
			}
			if applied != len(changes) {
				t.Fatalf("then applied %d of its %d changes", applied, len(changes))
			}
		}
	}
	run := func(t *testing.T, change func(t *testing.T, dir string), want map[string]string) {
		t.Helper()
		dir := copyScenario(t, "carry")
		withSubjectAtC1(t, dir)
		if change != nil {
			change(t, dir)
		}
		got := runBinary(t, binary, "", "state", "--scenario", dir)
		if got.code != 0 || got.stderr != "" {
			t.Fatalf("exit %d, stderr %q: want exit 0 and nothing on stderr", got.code, got.stderr)
		}
		for _, commit := range slices.Sorted(maps.Keys(want)) {
			line := "  check " + check + ": " + want[commit]
			if lines := subjectLines(got.stdout, subjects[commit]); !slices.Contains(lines, line) {
				t.Errorf("at subject %s: %q, want the line %q", subjects[commit], lines, line)
			}
		}
	}
	for _, row := range []struct {
		name   string
		change func(t *testing.T, dir string)
		want   map[string]string // the line of the check at the subject of each commit
	}{
		{"control: carry, whose fault the step planted on c1 and CI tested at f1", nil,
			map[string]string{"c1": fromC1, "c2": fromC1, "m2": fromC1, "c3": none}},
		{"a pass and a failure at one commit read unknown, and not true", results(func(r []map[string]any) []map[string]any {
			return append(r, withMembers(r[0], map[string]any{"outcome": "fail", "at": "2026-11-12T10:10:00Z", "seq": 5}))
		}), atEach("unknown (disagree)")},
		{"the record leaves fault_paths out", fault(map[string]any{"fault_paths": nil}), atEach("unknown (no_fault_paths)")},
		{"the record names no planted_on", fault(map[string]any{"planted_on": nil}), atEach("unknown (no_planted_on)")},
		{"the record names no planted_on, and c1 as its commit", fault(map[string]any{"planted_on": nil, "commit": "c1"}), atEach("unknown (no_planted_on)")},
		{"the record names c1, where the fault was planted, as its commit, the commit that CI tested", fault(map[string]any{"commit": "c1"}), atEach("unknown (planted_on_tested_commit)")},
		{"the record names m2 as its commit and as its planted_on", fault(map[string]any{"commit": "m2", "planted_on": "m2"}), atEach("unknown (planted_on_tested_commit)")},
		{"the subject is f1, the commit that CI tested, and the history gives f1 the tree of c1", atF1,
			map[string]string{"f1": none, "c1": fromC1, "c2": fromC1, "m2": fromC1}},
		{"the tie at c1, where the fault was planted, does not cover its path, and the tie at c2 does", tiedAt("c2"), map[string]string{"c1": none, "c2": none}},
		{"control: the ties at c1 and at c2 cover the fault's path", tiedAt("c1", "c2"), map[string]string{"c1": fromC1, "c2": fromC1}},
		{"control: a later failing example planted on c1 that missed its fault", later(nil), atEach("unknown (failing_example_missed)")},
		{"a later failing example planted on c1 that missed its fault names no planted_on", later(map[string]any{"planted_on": nil}), atEach("unknown (no_planted_on)")},
		{"a later failing example planted on c1 that missed its fault leaves fault_paths out", later(map[string]any{"fault_paths": nil}), atEach("unknown (no_fault_paths)")},
		{"a later failing example planted on c1 that missed its fault names one fault path, a space", later(map[string]any{"fault_paths": []string{" "}}), atEach("unknown (no_fault_paths)")},
		{"a later failing example planted on c1 that missed its fault names its fault path and a space", later(map[string]any{"fault_paths": []string{"packages/envoy/cmd/listener/main.go", " "}}), atEach("unknown (blank_fault_path)")},
		{"a later failing example planted on c1 that missed its fault names c1 as its commit", later(map[string]any{"commit": "c1"}), atEach("unknown (planted_on_tested_commit)")},
		{"a later failing example with no fault path, planted on c3 where CI refused the check", then(later(map[string]any{"planted_on": "c3", "commit": "f-c3", "fault_paths": nil}), refusedAt("c3")), atEach("unknown (no_fault_paths)")},
		{"a later failing example planted on c3, its own commit, where CI refused the check", then(later(map[string]any{"planted_on": "c3", "commit": "c3"}), refusedAt("c3")), atEach("unknown (planted_on_tested_commit)")},
		{"a later failing example with no planted_on, at f2 where CI refused the check", then(later(map[string]any{"planted_on": nil}), refusedAt("f2")), atEach("unknown (no_planted_on)")},
		{"a later failing example with no fault path, at f2 where CI refused the check", then(later(map[string]any{"fault_paths": nil}), refusedAt("f2")), atEach("unknown (no_fault_paths)")},
		{"a later failing example with a blank fault path, at f2 where CI refused the check", then(later(map[string]any{"fault_paths": []string{"packages/envoy/cmd/listener/main.go", " "}}), refusedAt("f2")), atEach("unknown (blank_fault_path)")},
		{"a later failing example planted on f2, its own commit, where CI refused the check", then(later(map[string]any{"planted_on": "f2"}), refusedAt("f2")), atEach("unknown (planted_on_tested_commit)")},
		{"control: CI's refusal at f2 does not take out a later failing example planted on c1", then(later(nil), refusedAt("f2")), atEach("unknown (failing_example_missed)")},
		{"a later failing example that missed its fault planted on c9, no commit of the history", later(map[string]any{"planted_on": "c9"}), atEach("unknown (planted_on_outside_history c9)")},
		{"a later failing example that missed its fault planted on f1, no commit of the history", later(map[string]any{"planted_on": "f1"}), atEach("unknown (planted_on_outside_history f1)")},
		{"a later failing example planted on c9, no commit of the history, where CI refused the check", then(later(map[string]any{"planted_on": "c9"}), refusedAt("c9")), atEach("unknown (planted_on_outside_history c9)")},
		{"a later failing example planted on f1, no commit of the history, where CI refused the check", then(later(map[string]any{"planted_on": "f1"}), refusedAt("f1")), atEach("unknown (planted_on_outside_history f1)")},
		{"a later failing example planted on c9, no commit of the history, at f2 where CI refused the check", then(later(map[string]any{"planted_on": "c9"}), refusedAt("f2")), atEach("unknown (planted_on_outside_history c9)")},
		{"control: CI's refusal at c2 takes out a later failing example planted on c2, which carries to c1 and m2", then(later(map[string]any{"planted_on": "c2", "commit": "f-c2"}), refusedAt("c2")), map[string]string{"c1": fromC1, "m2": fromC1}},
		{"control: a later failing example that missed its fault planted on c3, which does not carry to c1, c2 or m2", later(map[string]any{"planted_on": "c3", "commit": "f-c3"}), atEach(fromC1)},
		{"a second fault, planted on c3 and tested at f3, makes the check true at c3", results(func(r []map[string]any) []map[string]any {
			return append(r, withMembers(r[1], map[string]any{"commit": "f3", "planted_on": "c3", "at": "2026-11-13T10:05:00Z", "seq": 5}))
		}), map[string]string{"c1": fromC1, "c2": fromC1, "m2": fromC1, "c3": "true (passed, counted from c3)"}},
		{"the tie of the check at c1 is not known: the fault counts at c1 alone, where no tie is read", func(t *testing.T, dir string) {
			var ties map[string]map[string]any
			readJSONFile(t, filepath.Join(dir, "ties.json"), &ties)
			delete(ties["c1"], check)
			writeJSONFile(t, filepath.Join(dir, "ties.json"), ties)
		}, map[string]string{"c1": fromC1, "c2": "unknown (subject_moved)", "m2": "unknown (subject_moved)"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			run(t, row.change, row.want)
		})
	}
	for _, row := range []struct {
		name  string
		paths []string
		want  map[string]string
	}{
		{"the fault path is the test's own _test.go file", []string{"packages/envoy/cmd/listener/restart_test.go"}, atEach(none)},
		{"the fault path is a source file under testdata, a directory of the tie that starts with test", []string{"packages/envoy/cmd/listener/testdata/fault.go"}, atEach(none)},
		{"the fault path is a source file outside the tie", []string{sender}, atEach(none)},
		{"the fault path is a file of the tie that is no source file", []string{"packages/contracts/fixtures/dispatch-targeted-delivery.json"}, atEach(none)},
		{"one of two fault paths is the test's own _test.go file", []string{"packages/envoy/cmd/listener/main.go", "packages/envoy/cmd/listener/restart_test.go"}, atEach(none)},
		{"the record names no fault path", []string{}, atEach("unknown (no_fault_paths)")},
		{"the one fault path of the record is a space, which is no path", []string{" "}, atEach("unknown (no_fault_paths)")},
		{"one of two fault paths is a tab, which is no path", []string{"packages/envoy/cmd/listener/main.go", "\t"}, atEach("unknown (blank_fault_path)")},
		{"control: the fault path is a source file of the tie with a space in its name", []string{"packages/envoy/cmd/listener/main two.go"}, atEach(fromC1)},
		{"the fault path is a .tsx source file of the tie", []string{"packages/envoy/cmd/listener/view.tsx"}, atEach(fromC1)},
		{"the fault path is a .ts source file of the tie", []string{"packages/envoy/cmd/listener/client.ts"}, atEach(fromC1)},
		{"two fault paths, each a source file of the tie", []string{"packages/envoy/cmd/listener/main.go", "packages/envoy/cmd/listener/store.go"}, atEach(fromC1)},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			run(t, func(t *testing.T, dir string) {
				paths(row.paths...)(t, dir)
				readsBack(t, dir, 1, map[string]any{"fault_paths": row.paths})
			}, row.want)
		})
	}
}

// readsBack fails the test unless result i of the scenario at dir, as the command's reader reads
// it, holds each member of set: its commit, planted_on, fault_paths or outcome, nil for a member
// taken out, which reads as its zero.
func readsBack(t *testing.T, dir string, i int, set map[string]any) {
	t.Helper()
	r := readScenario(t, dir).Results[i]
	read := map[string]any{"commit": r.Commit, "planted_on": r.PlantedOn, "fault_paths": r.FaultPaths, "outcome": string(r.Outcome)}
	zero := map[string]any{"commit": "", "planted_on": "", "fault_paths": []string(nil), "outcome": ""}
	for _, member := range slices.Sorted(maps.Keys(set)) {
		got, known := read[member]
		want := set[member]
		if want == nil {
			want = zero[member]
		}
		if !known || fmt.Sprintf("%q", got) != fmt.Sprintf("%q", want) {
			t.Fatalf("result %d as the reader reads it: %s %q, where the row wrote %q", i+1, member, got, want)
		}
	}
}
