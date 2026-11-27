package specgraph

import (
	"slices"
	"testing"
	"time"
)

// The failing example of a program check of the code counts only when each path of its fault is a
// source file and no file of a test (deriveChecks, step 7). A source file ends in .go, .ts or .tsx.
// A file of a test has a name that ends in _test.go, .test.ts, .test.tsx or .e2e.ts, or a directory
// in its path that starts with test, ends with test or tests, or is __tests__ or e2e. Each row is one
// part of the rule, or a path beside it that the part does not take.
func TestAFaultPathIsASourceFileAndNoFileOfATest(t *testing.T) {
	t.Parallel()
	for path, want := range map[string]bool{
		// a source file
		"packages/envoy/cmd/listener/main.go":     true,
		"packages/dispatch/web/src/client.ts":     true,
		"packages/dispatch/web/src/view.tsx":      true,
		"packages/dispatch/web/src/types.d.ts":    true,
		"main.go":                                 true,
		"packages/envoy/cmd/listener/testing.go":  true, // a name, not a directory, that starts with test
		"packages/envoy/cmd/listener/contest.go":  true, // a name, not a directory, that ends with test
		"packages/envoy/cmd/contested/main.go":    true, // a directory with test inside its name
		"packages/envoy/cmd/e2eutil/main.go":      true, // a directory that is not e2e
		"packages/dispatch/web/__tests_/view.tsx": true, // a directory that is not __tests__
		// no source file
		"packages/contracts/fixtures/dispatch-targeted-delivery.json": false,
		"docs/x.md":                            false,
		"Makefile":                             false,
		"packages/envoy/cmd/listener/go":       false,
		"packages/dispatch/web/src/a.tsx.orig": false,
		// a file of a test by its name
		"packages/envoy/cmd/listener/restart_test.go": false,
		"packages/dispatch/web/src/client.test.ts":    false,
		"packages/dispatch/web/src/view.test.tsx":     false,
		"packages/dispatch/web/flows/sign-in.e2e.ts":  false,
		// a file of a test by a directory of its path
		"packages/envoy/cmd/listener/testdata/fault.go": false,
		"packages/envoy/internal/testutil/clock.go":     false,
		"packages/envoy/cmd/loadtest/main.go":           false,
		"packages/envoy/cmd/listener/unittests/a.go":    false,
		"packages/dispatch/web/__tests__/view.tsx":      false,
		"packages/dispatch/e2e/sign-in.ts":              false,
		"test/main.go":                                  false,
		"tests/main.go":                                 false,
	} {
		if got := isFaultPath(path); got != want {
			t.Errorf("isFaultPath(%q) = %t, want %t", path, got, want)
		}
	}
}

// R9: "For a check that a program runs, the failing example is a fault that a step plants in the
// code." A failing example of a program check of the code counts only when each path of its fault
// is a source file, one that ends in .go, .ts or .tsx, and no file of a test (isFaultPath), and the
// check's tie at the commit that the fault was planted on, where that tie is known, covers it; a
// path of only white space is no path, so a record of such paths alone reads as one with none, and
// one beside a real path has a reason of its own. Each row is TestCheckState's base, whose caught
// failing example planted on c1 is the newest, with the row's fault paths, at the subject c1 and at
// c2, whose tree is c1's with docs/x.md changed, a file that no tie covers, and which holds a pass
// of C; sender is a source file outside C's tie, which a row adds to the tie at the commits that it
// names, so the tie at c1 and the tie at c2 can differ.
func TestAFaultPathIsASourceFileThatTheTieAtPlantedOnCovers(t *testing.T) {
	t.Parallel()
	const sender = "packages/envoy/cmd/sender/main.go"
	none := CheckState{ID: checkStateCheck.ID, State: "unknown", Reason: "no_failing_example"}
	at := func(reason string) [2]CheckState {
		state := CheckState{ID: checkStateCheck.ID, State: "unknown", Reason: reason}
		return [2]CheckState{state, state}
	}
	counts := [2]CheckState{{ID: checkStateCheck.ID, State: "true", Reason: "passed", From: "c1"}, {ID: checkStateCheck.ID, State: "true", Reason: "passed", From: "c2"}}
	for _, row := range []struct {
		name   string
		paths  []string
		tiedAt []string      // the commits whose tie of C also covers packages/envoy/cmd/sender/*
		want   [2]CheckState // at c1 and at c2
	}{
		{"control: the fault path is a source file of the tie", []string{checkStateFaultPath}, nil, counts},
		{"the fault path is the test's own _test.go file", []string{"packages/envoy/cmd/listener/restart_test.go"}, nil, [2]CheckState{none, none}},
		{"the fault path is a source file under testdata, a directory of the tie that starts with test", []string{"packages/envoy/cmd/listener/testdata/fault.go"}, nil, [2]CheckState{none, none}},
		{"the fault path is a source file outside the tie", []string{sender}, nil, [2]CheckState{none, none}},
		{"the fault path is a file of the tie that is no source file", []string{"packages/contracts/fixtures/dispatch-targeted-delivery.json"}, nil, [2]CheckState{none, none}},
		{"one of two fault paths is the test's own _test.go file", []string{checkStateFaultPath, "packages/envoy/cmd/listener/restart_test.go"}, nil, [2]CheckState{none, none}},
		{"the one fault path is a space, which is no path", []string{" "}, nil, at("no_fault_paths")},
		{"one of two fault paths is a tab, which is no path", []string{checkStateFaultPath, "\t"}, nil, at("blank_fault_path")},
		{"control: the fault path is a source file of the tie with a space in its name", []string{"packages/envoy/cmd/listener/main two.go"}, nil, counts},
		{"the fault path is a .tsx source file of the tie", []string{"packages/envoy/cmd/listener/view.tsx"}, nil, counts},
		{"the fault path is a .ts source file of the tie", []string{"packages/envoy/cmd/listener/client.ts"}, nil, counts},
		{"two fault paths, each a source file of the tie", []string{checkStateFaultPath, "packages/envoy/cmd/listener/store.go"}, nil, counts},
		{"the tie at c1, where the fault was planted, does not cover its path, and the tie at c2 does", []string{sender}, []string{"c2"}, [2]CheckState{none, none}},
		{"control: the ties at c1 and at c2 cover the fault's path", []string{sender}, []string{"c1", "c2"}, counts},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			wide, err := NewTie(append(slices.Clone(checkStateTie), "packages/envoy/cmd/sender/*"))
			if err != nil {
				t.Fatal(err)
			}
			for i, subject := range []string{"c1", "c2"} {
				s := newCheckCase(t)
				s.commit("c2", "c1", "docs/x.md")
				for _, commit := range row.tiedAt {
					s.Ties[commit][s.check] = wide
				}
				s.Results[1].FaultPaths = row.paths
				if subject == "c2" {
					s.Results = append(s.Results, checkRun("c2", OutcomePass, checkStateRun.Add(time.Hour)))
				}
				s.Subject.Commit = subject
				if states := checkStates(t, s.input()); !slices.Contains(states, row.want[i]) {
					t.Errorf("at %s: deriveChecks = %+v\nwant %+v among them", subject, states, row.want[i])
				}
			}
		})
	}
}
