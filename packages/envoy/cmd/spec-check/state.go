package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/specgraph"
	"github.com/sjawhar/envoy/internal/runnerlog"
)

// runState is "spec-check state --scenario <dir> [--json]": it reads a scenario in the format of
// specgraph.ReadScenario, derives the state of each check and each statement at each of its
// subjects (specgraph.Derive), and prints them; with units.json, it also prints whether each wait
// on a statement ends at the head of main and the cycle of waits (specgraph.WaitCycle). It answers
// 0 for a scenario it reads, and 2, with one line on stderr naming the file and the fault, for one
// it does not, or for arguments it does not take. The line of a fault of a file of the scenario is
// "spec-check: <dir>/<file>: <fault>", with ":<line>" after the file for a line of a spec file.
func runState(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("state", flag.ContinueOnError)
	scenarioFlag := flags.String("scenario", "", "the directory of a scenario")
	asJSON := flags.Bool("json", false, "print the states as JSON")
	if code, ok := parse(flags, args, stderr, "scenario"); !ok {
		return code
	}
	dir, ok := absolute(stderr, *scenarioFlag)
	if !ok {
		return 2
	}
	switch info, err := os.Stat(dir); {
	case errors.Is(err, fs.ErrNotExist):
		say(stderr, "%s: does not exist", dir)
		return 2
	case err != nil:
		say(stderr, "%s: cannot be read: %s", dir, err)
		return 2
	case !info.IsDir():
		say(stderr, "%s: is not a directory", dir)
		return 2
	}
	scenario, err := specgraph.ReadScenario(os.DirFS(dir))
	if err != nil {
		var fault *specgraph.ScenarioError
		if !errors.As(err, &fault) {
			say(stderr, "%s: %s", dir, err)
			return 2
		}
		where := filepath.Join(dir, fault.File)
		if fault.Line > 0 {
			where = fmt.Sprintf("%s:%d", where, fault.Line)
		}
		say(stderr, "%s: %s", where, fault.Err)
		return 2
	}

	states := make([]specgraph.State, len(scenario.Subjects))
	for i := range scenario.Subjects {
		if states[i], err = specgraph.Derive(scenario.Input(i)); err != nil {
			say(stderr, "%s: %s", dir, err)
			return 2
		}
	}
	var cycle []string
	if scenario.Units != nil {
		if cycle, err = specgraph.WaitCycle(scenario.Units); err != nil {
			say(stderr, "%s: %s", dir, err)
			return 2
		}
	}
	if *asJSON {
		return printStatesJSON(stdout, stderr, dir, states, scenario.Units != nil, cycle)
	}
	printStates(stdout, scenario, states, cycle)
	return 0
}

// printStates writes the states as text: for each subject, its line, then each statement in the
// order of Derive with each of its own checks that CheckStates judged after it, then, at the head of
// main, each wait of each unit; and, with units, the cycle of waits. A statement, check or wait line
// starts with fixed text and passes through runnerlog.Printable, so no text of the scenario starts
// a line of the runner's log or names a workflow command. Each value of the scenario that a line
// names, an id, a commit, a branch, a deploy or a transaction, is cut (specgraph.Cut); the states
// themselves are keyed by the whole ids.
func printStates(stdout io.Writer, scenario specgraph.Scenario, states []specgraph.State, cycle []string) {
	current, _ := scenario.Approved.Current() // ReadApproved refused every document that would be a problem here
	serves := make(map[string]string, len(current.Checks))
	for _, c := range current.Checks {
		serves[c.ID] = c.Serves
	}
	units := slices.Clone(scenario.Units)
	slices.SortFunc(units, func(a, b specgraph.Unit) int { return strings.Compare(a.ID, b.ID) })

	for i, state := range states {
		subject := scenario.Subjects[i]
		switch {
		case subject.Branch != "":
			say(stdout, "subject %s, the head of %s", specgraph.Cut(subject.Commit), specgraph.Cut(subject.Branch))
		case subject.GraphTx != "":
			say(stdout, "subject %s, the commit that production runs at deploy %s, and the graph at transaction %s",
				specgraph.Cut(subject.Commit), specgraph.Cut(subject.Deploy), specgraph.Cut(subject.GraphTx))
		default:
			say(stdout, "subject %s, the commit that production runs at deploy %s", specgraph.Cut(subject.Commit), specgraph.Cut(subject.Deploy))
		}
		checks := make(map[string]specgraph.CheckState, len(state.Checks))
		checksOf := map[string][]specgraph.CheckState{} // by the statement each serves, in the order of state.Checks
		for _, c := range state.Checks {
			checks[c.ID] = c
			checksOf[serves[c.ID]] = append(checksOf[serves[c.ID]], c)
		}
		own := make(map[string]string, len(state.Statements))
		for _, s := range state.Statements {
			own[s.ID] = s.Own
			printLine(stdout, "statement %s: %s", specgraph.Cut(s.ID), statementText(s, checks, scenario.Approved))
			for _, c := range checksOf[s.ID] {
				printLine(stdout, "  check %s: %s", specgraph.Cut(c.ID), checkText(c))
			}
		}
		if scenario.Units == nil || subject.Branch != "main" {
			continue
		}
		for _, u := range units {
			for _, statement := range slices.Sorted(slices.Values(u.WaitsOn)) {
				if state.WaitEnds(statement) {
					printLine(stdout, "wait of %s on %s: ends", specgraph.Cut(u.ID), specgraph.Cut(statement))
				} else {
					printLine(stdout, "wait of %s on %s: holds (%s)", specgraph.Cut(u.ID), specgraph.Cut(statement), stateWords(own[statement]))
				}
			}
		}
	}
	if scenario.Units == nil {
		return
	}
	if cycle == nil {
		say(stdout, "cycle of waits: none")
		return
	}
	named := make([]string, len(cycle))
	for i, id := range cycle {
		named[i] = specgraph.Cut(id)
	}
	say(stdout, "cycle of waits: %s", strings.Join(named, " -> "))
}

// statementText is the state of a statement as a line of text says it, each id cut.
func statementText(s specgraph.StatementState, checks map[string]specgraph.CheckState, approved specgraph.Approved) string {
	detail := specgraph.Cut(s.Detail)
	switch s.Reason {
	case "checks_true":
		if s.PartlyChecked {
			return "met, partly checked"
		}
		return "met"
	case "not_built":
		switch {
		case s.Detail != s.ID:
			return fmt.Sprintf("not built (the files do not hold the approved text of %s)", detail)
		case s.EarlierText:
			versions := approved.Nodes[s.ID].Versions
			return fmt.Sprintf("not built (the files hold approved version %d, and version %d is current)", s.HeldVersion, versions[len(versions)-1].Version)
		}
		return "not built (the files do not hold its approved text)"
	case "check_false":
		return fmt.Sprintf("not met (check %s is false)", detail)
	case "supporter_not_met":
		return fmt.Sprintf("not met (supporter %s is not met)", detail)
	case "check_unknown":
		return fmt.Sprintf("unknown (check %s: %s)", detail, checks[s.Detail].Reason)
	case "supporter_unknown":
		return fmt.Sprintf("unknown (supporter %s: unknown)", detail)
	case "supporter_not_built":
		return fmt.Sprintf("unknown (supporter %s: not built)", detail)
	case "no_check":
		if s.Detail == s.ID {
			return "unknown (no check serves it)"
		}
		return fmt.Sprintf("unknown (statement %s has no check)", detail)
	}
	return fmt.Sprintf("%s (%s %s)", stateWords(s.State), s.Reason, detail)
}

// checkText is the state of a check as a line of text says it, the commit cut.
func checkText(c specgraph.CheckState) string {
	switch c.State {
	case "true":
		return fmt.Sprintf("true (passed, counted from %s)", specgraph.Cut(c.From))
	case "false":
		return fmt.Sprintf("false (failed, counted from %s)", specgraph.Cut(c.From))
	}
	return fmt.Sprintf("%s (%s)", c.State, c.Reason)
}

// stateWords is a state of a statement in words: "not_built" is "not built".
func stateWords(state string) string {
	return strings.ReplaceAll(state, "_", " ")
}

// printLine writes one line that starts with the fixed text of format, escaped by
// runnerlog.Printable.
func printLine(w io.Writer, format string, args ...any) {
	fmt.Fprintln(w, runnerlog.Printable(fmt.Sprintf(format, args...)))
}

// printStatesJSON writes the states as one line of JSON with printJSON, []State, or with units
// {"states":[…],"cycle":[…]}, "cycle" [] when there is none.
func printStatesJSON(stdout, stderr io.Writer, dir string, states []specgraph.State, units bool, cycle []string) int {
	var value any = states
	if units {
		if cycle == nil {
			cycle = []string{}
		}
		value = struct {
			States []specgraph.State `json:"states"`
			Cycle  []string          `json:"cycle"`
		}{states, cycle}
	}
	return printJSON(stdout, stderr, dir, value)
}
