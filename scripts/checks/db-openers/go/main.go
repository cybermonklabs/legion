//go:build ignore

// Db-openers is the Go half of scripts/checks/db-openers.ts, which runs it once for each Go module
// of the repository: the list of every call in the module's non-test Go files that opens or parses
// a Postgres connection string, each judged by what it passes.
//
//	go run scripts/checks/db-openers/go/*.go <module directory> [<package directory to plant in>]
//
// It reads every package of the module from source: every non-test Go file below the module's
// go.mod (nested modules, testdata and vendor apart), whatever its build constraints. The host's
// own build reads most; each file it leaves out is read in a build of a context that compiles it
// (another GOOS and GOARCH, or a build tag), and each build is type-checked on its own (load.go).
// The calls of every build are listed once each, the strictest verdict any build gives a call
// standing. The packages an opener comes from (database/sql, jackc/pgx and its pgconn, pgxpool
// and stdlib, lib/pq, and pgsetting, the one formatter) are read from the export data `go list
// -export` names; every other package outside the module is an empty one, so a module whose
// dependencies are large (an infrastructure SDK) is read without building them, and a call into
// such a package is never an opener.
//
// An opener form is a function of those packages that takes a connection string (a parameter
// named connString, dataSourceName or dsn) or a parsed configuration (pgx.ConnConfig,
// pgconn.Config, pgxpool.Config): the forms are read from the packages' own signatures, not from
// a list (forms.go). A call of a form, or of a wrapper of one, is judged by the argument in that
// place (judge.go):
//   - read by the formatter: the value pgsetting.ParseConfig or ParsePoolConfig answered, or a
//     value one of those parses guards: the parse's error ends the function (`if _, err :=
//     <parse>; err != nil { … return }`, or its error tested so right after it), it runs on every
//     path to the opener (a statement of a block that holds the opener, before it, in the same
//     function literal), and the opener passes the very variable, or field of one, the parse read,
//     which is the function's own, no pointer reaches, and no line writes between the two (a
//     closure's write, or one later in a loop that holds the opener and not the parse, counts as
//     between). pgsetting.Refuse guards nothing: it refuses only the shapes pgx reads apart, and a
//     value it admits can still be one pgx cannot parse, whose parse error, the opener's own, quotes
//     the user part;
//   - a constant, or a zero value: it holds no setting;
//   - the configuration another listed parse answered, or the configuration of a pool or a
//     connection that is already open, whose own opener is listed and judged;
//   - a parameter of the function the call is in: that function is then a wrapper, and each call
//     of it is listed and judged in its turn;
//   - anything else is unjudged: a field, a package variable, a value read from the environment
//     or a flag, the answer of another function. scripts/checks/db-openers.judged.ts says why each
//     such call cannot carry a misread setting, or the check fails.
//
// A form or a wrapper named as a value and not called is listed too, unjudged, since what it is
// later handed is not read. A wrapper that no file read calls (its callers are tests) is listed
// as one.
//
// It prints one JSON line for each form ({"form": …}), each call ({"call": …}), what the planted
// shapes gave when any was not as marked ({"blind": […]}), and a summary ({"summary": …}). A
// call's argument is its whole source text, its white space folded, so that a judgement names the
// call whole. The planted shapes (planted.go) are more files of the package the second argument
// names, read with it: a line marked `planted: unjudged` must be listed unjudged, one marked
// `planted: judged` must be listed and judged, and a line marked neither must not be listed.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

func main() {
	if len(os.Args) < 2 || len(os.Args) > 3 {
		fmt.Fprintln(os.Stderr, "usage: go run scripts/checks/db-openers/go/*.go <module directory> [<package directory to plant in>]")
		os.Exit(2)
	}
	root, err := os.Getwd()
	if err != nil {
		fail(err)
	}
	plantDir := ""
	if len(os.Args) == 3 {
		plantDir = os.Args[2]
	}
	read := load(root, os.Args[1], plantDir)
	forms := map[string]form{}
	type listed struct {
		rec    record
		offset int
	}
	merged := map[string]*listed{}
	var order []string
	typeErrors := 0
	var builds []string
	for _, m := range read.builds {
		a := newAnalysis(m)
		a.run()
		builds = append(builds, m.build)
		typeErrors += m.typeErrors
		for _, f := range a.formList() {
			forms[f.Callee] = f
		}
		for _, c := range a.calls {
			rec := a.record(c)
			offset := m.fset.Position(c.at).Offset
			key := fmt.Sprintf("%s\x00%d\x00%s\x00%s\x00%s", rec.File, offset, rec.Function, rec.Callee, rec.Argument)
			before, seen := merged[key]
			if !seen {
				merged[key] = &listed{rec, offset}
				order = append(order, key)
			} else if strictness(rec) > strictness(before.rec) {
				before.rec = rec
			}
		}
	}
	sort.SliceStable(order, func(i, j int) bool {
		x, y := merged[order[i]], merged[order[j]]
		if x.rec.File != y.rec.File {
			return x.rec.File < y.rec.File
		}
		return x.offset < y.offset
	})
	encoder := json.NewEncoder(os.Stdout)
	var formNames []string
	for name := range forms {
		formNames = append(formNames, name)
	}
	sort.Strings(formNames)
	for _, name := range formNames {
		must(encoder.Encode(map[string]any{"form": forms[name]}))
	}
	planted := map[string]map[int]string{}
	plantedArguments := map[string]map[int][]string{}
	plantedNames := map[string]bool{}
	for _, f := range plantedFiles {
		plantedNames[f.name] = true
	}
	counts := map[string]int{}
	for _, key := range order {
		rec := merged[key].rec
		counts[rec.Verdict]++
		if plantedNames[rec.File] {
			if planted[rec.File] == nil {
				planted[rec.File], plantedArguments[rec.File] = map[int]string{}, map[int][]string{}
			}
			// A line listed twice reads as both verdicts, which matches no mark.
			verdict := rec.Verdict
			if before, twice := planted[rec.File][rec.Line]; twice {
				verdict = before + " and " + verdict
			}
			planted[rec.File][rec.Line] = verdict
			plantedArguments[rec.File][rec.Line] = append(plantedArguments[rec.File][rec.Line], rec.Argument)
		}
		must(encoder.Encode(map[string]any{"call": rec}))
	}
	var blind []string
	if plantDir != "" {
		blind = plantedBlind(planted, plantedArguments)
	}
	if read.files == 0 {
		blind = append(blind, fmt.Sprintf("no Go file of the module %s was read", os.Args[1]))
	}
	if len(blind) > 0 {
		must(encoder.Encode(map[string]any{"blind": blind}))
	}
	must(encoder.Encode(map[string]any{"summary": map[string]any{
		"module": os.Args[1], "files": read.files, "constrained": read.constrained, "builds": builds,
		"forms": len(forms), "calls": len(order), "judged": counts["judged"], "wrapper": counts["wrapper"],
		"unjudged": counts["unjudged"], "typeErrors": typeErrors,
	}}))
}

// strictness orders the verdicts two builds can give one call: a call unjudged in any build is
// unjudged; a wrapper's call that one build calls and another does not is a wrapper's, judged at
// each call; and a call judged only where every build judges it.
func strictness(r record) int {
	switch {
	case r.Verdict == "unjudged" && r.Kind != "uncalled":
		return 3
	case r.Verdict == "wrapper":
		return 2
	case r.Kind == "uncalled":
		return 1
	}
	return 0
}

func must(err error) {
	if err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "db-openers:", err)
	os.Exit(1)
}
