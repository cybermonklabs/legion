package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/specgraph"
)

// programCheck is one entry of the JSON that checks prints, in the field order Results from CI
// reads: the check's id, its test part, and its digest, which each result of the check carries.
type programCheck struct {
	ID              string `json:"id"`
	Runner          string `json:"runner"`
	Selector        string `json:"selector"`
	Job             string `json:"job"`
	ProcedureSHA256 string `json:"procedure_sha256"`
}

// runChecks is "spec-check checks --dir <dir> --job <job>": it prints the program checks whose
// test part names the job as one line of JSON, [] when none, and answers 0; 2 when the tree has a
// problem, with the problems on stderr, or for a directory it cannot read or arguments it does not
// take. A directory that does not exist is the empty tree (judgeOrEmpty), so it prints [].
//
// The line does not pass through runnerlog.Say, whose prefix and escapes would make it no JSON.
// encoding/json writes each control character of a value as an escape, so no line break of a
// value reaches the log, and the line starts with "[", so it starts no workflow command; the "[" of
// each "##[", which can only be in a value, is written as \u005b, which a JSON reader reads back as
// "[".
func runChecks(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("checks", flag.ContinueOnError)
	dirFlag := flags.String("dir", "", "the directory of spec files")
	job := flags.String("job", "", "the job whose program checks to print")
	if code, ok := parse(flags, args, stderr, "dir", "job"); !ok {
		return code
	}
	dir, ok := absolute(stderr, *dirFlag)
	if !ok {
		return 2
	}
	j, err := judgeOrEmpty(stderr, dir)
	if err != nil {
		return cannotJudge(stderr, dir, err)
	}
	if len(j.problems) > 0 {
		j.sayProblems(stderr)
		return 2
	}
	checks := []programCheck{}
	for _, c := range j.tree.Checks {
		if c.Procedure == specgraph.ProcedureProgram && c.Test != nil && c.Test.Job == *job {
			checks = append(checks, programCheck{ID: c.ID, Runner: c.Test.Runner, Selector: c.Test.Selector, Job: c.Test.Job, ProcedureSHA256: c.Digest()})
		}
	}
	line, err := json.Marshal(checks)
	if err != nil {
		say(stderr, "%s: %s", dir, err)
		return 2
	}
	fmt.Fprintln(stdout, strings.ReplaceAll(string(line), "##[", `##\u005b`))
	return 0
}
