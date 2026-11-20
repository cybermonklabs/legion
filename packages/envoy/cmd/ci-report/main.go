// Command ci-report is the last step of a CI job: it writes one result to Dispatch for each check
// of the spec files whose test part names the job and whose test ran in the job's go test report.
// A check whose test did not run gets no result (R12: nobody writes a result for work that it did
// not do). The composite action .github/actions/dispatch-results runs it.
//
// Usage:
//
//	ci-report --dir <dir> --job <job> --report <file> --project <key> [--dispatch-cli <argv>]
//
// It reads the commit with `git rev-parse HEAD` in its working directory, the checkout, and never
// from GITHUB_SHA, which it prints beside it. It reads the check nodes of the spec files in the
// directory at that commit, through git from the commit's tree (filesAt), and never the files of
// the working tree, so a step before it that changes a spec file and commits nothing changes no
// check (R12). It reads them as Dispatch's results route reads the files at the run's commit: an
// entry is a spec file as notes.IsNote decides of the tree's entry, a directory that the commit
// does not hold is the empty tree, and then specgraph.Judge with no retired list, so a tree has
// one answer in both places. It reads the report with specgraph.ReadGoTestJSON. planReports
// decides the outcome of each check of the job. For each pass, fail and could_not_run it runs
//
//	<dispatch-cli> result --project <key> --check <id> --verdict <outcome> --commit <sha> --observed-file -
//
// with what the test wrote, as specgraph.TestOutcome.Output holds it, whole on the command's
// standard input, so no text of a test reaches the argument list, where one that starts with "-"
// would read as a flag. The reporter cuts nothing: the dispatch command is the one cut, which
// keeps the start and the end of a text longer than a result keeps, with a note between them, and
// sends a NUL as U+FFFD. <dispatch-cli> is split at white space, with no quoting, and is
// "dispatch" when the flag is left out. A skip, a test the report does not hold and a test that
// ran twice get no call. Each run writes its results again: a second run of the step at one
// commit stores a second result of each check, and Dispatch keeps each and decides the state from
// all of them. At one commit, a pass and a failure of one check make its state unknown, and a
// later pass does not end the report of the failure (R7).
//
// It prints one JSON line on standard output for each check of the job, in the tree's order:
// {"check","test","outcome","exit","result_id","why"}. exit is the command's exit status, -1 when
// it could not start or a signal ended it, and null for a check with no call. result_id is the id
// of the stored result, the one part of a stored call's output that it reads and prints, since
// Dispatch answers the row itself (`dispatch results --project`). A write that landed with its
// answer lost, which the command prints as {"landed_without_answer":"<code>"} with exit 0, is a
// stored result whose id is unknown: its result_id is empty and why says so. why says why a check
// got no call, and holds what the command printed on both streams when a call failed: the command
// prints a refusal on standard output. A refused call stored nothing. A write whose outcome is
// unknown, which the command prints as {"write_outcome_unknown":"<code>"} with exit 1 when the
// write went out and no answer of Dispatch's said whether it stored the result (a 2xx that
// names no key back among them, whatever it holds), is a failed call whose why names the code,
// and it is never read as a stored result: it may be stored, and a second run of the step is
// safe, since Dispatch keeps each result. The command itself waits at most 60 s for a broker that
// answers 429 or 5xx and for Dispatch's check of its token, so the step adds no retry of its own.
// The line holds no raw line break, as encoding/json escapes each control character,
// and the "[" of each "##[" in it is written as \u005b, which a JSON reader reads back as "[", so
// the line starts no workflow command of a runner's log. Every other line goes to standard error
// through runnerlog.Say with the prefix
// "ci-report: ", ending with "<n> checks of <dir> at <sha> name job <job>; <n> results written,
// <n> calls failed".
//
// It exits as help names: 0 when each call stored its result, 1 when a call failed, and 2, with
// no call, for arguments it does not take, a commit it cannot read, a spec directory or a report
// it cannot read, and a tree that Judge refuses or that is over notes.CheckSet's or
// notes.MaxFileSize's bound, which never reaches Judge. For a refused tree it writes each problem
// with its file and line, the first one first, as Dispatch names that first one when it refuses a
// result of the tree.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/architecture/notes"
	"github.com/sjawhar/envoy/internal/dispatch/specgraph"
	"github.com/sjawhar/envoy/internal/runnerlog"
)

const usage = `usage: ci-report --dir <dir> --job <job> --report <file> --project <key> [--dispatch-cli <argv>]`

// help is what -h prints after the usage, from a blank line on.
const help = `

ci-report writes one result to Dispatch, with <argv> result --project, for each check of the
spec files in <dir> whose job is <job> and whose test ran in the go test -json report <file>.
It reads the spec files at the commit of the checkout (git rev-parse HEAD), through git, and
never the files of the working tree. A second run writes each result again, and Dispatch keeps
each result and decides the state from all of them.
At one commit, a pass and a failure of one check make its state unknown,
and a later pass does not end the report of the failure (R7).

Exit status:
  0  each call stored its result, or no check of the job had a test that ran
  1  a call of the dispatch command failed
  2  no call was made: an argument it does not take, or a commit, a spec directory, a report
     or a tree of spec files that it cannot read, that is over a bound of the spec set, or
     that the graph logic refuses`

// prefix is the fixed text that starts every line the command prints but the usage and the JSON
// lines.
const prefix = "ci-report: "

// commitSHA is the form of the commit a result is for, as Dispatch takes it: 40 lowercase hex.
var commitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// checkLine is the JSON line printed for one check.
type checkLine struct {
	Check    string `json:"check"`
	Test     string `json:"test"`
	Outcome  string `json:"outcome"`
	Exit     *int   `json:"exit"`
	ResultID string `json:"result_id"`
	Why      string `json:"why"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is the command with args, answering its exit status.
func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("ci-report", flag.ContinueOnError)
	dirFlag := flags.String("dir", "", "the directory of spec files")
	job := flags.String("job", "", "the job whose checks to report")
	reportFlag := flags.String("report", "", "the job's go test -json report")
	project := flags.String("project", "", "the key of the Dispatch project the results are for")
	cliFlag := flags.String("dispatch-cli", "dispatch", "the dispatch command, split at white space")
	if code, ok := parse(flags, args, stderr, "dir", "job", "report", "project"); !ok {
		return code
	}
	cli := strings.Fields(*cliFlag)
	if len(cli) == 0 {
		say(stderr, "--dispatch-cli names no command")
		fmt.Fprintln(stderr, usage)
		return 2
	}
	dir, err := filepath.Abs(*dirFlag)
	if err != nil {
		say(stderr, "%s: cannot be read: %s", *dirFlag, err)
		return 2
	}
	commit, err := headCommit()
	if err != nil {
		say(stderr, "%s", err)
		return 2
	}
	githubSHA, set := os.LookupEnv("GITHUB_SHA")
	if !set {
		githubSHA = "not set"
	}
	say(stderr, "the commit is %s, which git rev-parse HEAD names; GITHUB_SHA is %s", commit, githubSHA)
	tree, ok := readTree(stderr, dir, commit)
	if !ok {
		return 2
	}
	ran, err := loadReport(*reportFlag)
	if err != nil {
		say(stderr, "%s", err)
		return 2
	}

	planned := planReports(tree, *job, ran)
	written, failed := 0, 0
	for _, p := range planned {
		l := checkLine{Check: p.Check, Test: p.Test, Outcome: p.Outcome, Why: p.Why}
		if recorded(p.Outcome) {
			code, id, why, stored := record(cli, *project, commit, p)
			l.Exit, l.ResultID = &code, id
			if why != "" {
				l.Why = why
			}
			if stored {
				written++
			} else {
				failed++
			}
		}
		if err := writeLine(stdout, l); err != nil {
			say(stderr, "%s: %s", p.Check, err)
			return 2
		}
	}
	say(stderr, "%d checks of %s at %s name job %s; %d results written, %d calls failed", len(planned), dir, commit, *job, written, failed)
	if failed > 0 {
		return 1
	}
	return 0
}

// say writes one line through runnerlog.Say with the command's prefix.
func say(w io.Writer, format string, args ...any) {
	runnerlog.Say(w, prefix, format, args...)
}

// parse reads args into flags and answers whether the command goes on and, when it does not, its
// exit status: 0 for -h, and 2 for a flag it does not take, an argument besides the flags or a
// required flag left empty, each with one line naming the case, then the usage. The flag package
// would print a parse error with the argument in it as it is, so parse prints nothing through it
// and writes the error through runnerlog.Say.
func parse(flags *flag.FlagSet, args []string, stderr io.Writer, required ...string) (int, bool) {
	flags.SetOutput(io.Discard)
	err := flags.Parse(args)
	switch {
	case errors.Is(err, flag.ErrHelp):
		fmt.Fprintln(stderr, usage+help)
		return 0, false
	case err != nil:
		say(stderr, "%s", err)
	case flags.NArg() > 0:
		say(stderr, "takes no argument but its flags, got %q", flags.Arg(0))
	default:
		missing := slices.IndexFunc(required, func(name string) bool { return flags.Lookup(name).Value.String() == "" })
		if missing < 0 {
			return 0, true
		}
		say(stderr, "--%s is required", required[missing])
	}
	fmt.Fprintln(stderr, usage)
	return 2, false
}

// headCommit is the commit `git rev-parse HEAD` names in the working directory.
func headCommit() (string, error) {
	out, err := runGit(nil, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	commit := strings.TrimSpace(string(out))
	if !commitSHA.MatchString(commit) {
		return "", fmt.Errorf("git rev-parse HEAD printed %q, not a commit of 40 lowercase hex characters", commit)
	}
	return commit, nil
}

// readTree reads the spec files of dir at commit (filesAt), and answers their tree and whether it
// is one to report on: a tree that specgraph.Judge takes, or the empty tree of a directory that
// the commit does not hold. It warns of each entry that is not a spec file, and writes each
// problem of a refused tree, in Judge's order, and why it could not read one, on stderr. A set
// over a bound of notes.CheckSet, or a file over the size bound, refuses the tree before Judge
// reads any file.
func readTree(stderr io.Writer, dir, commit string) (specgraph.Tree, bool) {
	set, err := filesAt(dir, commit)
	for _, entry := range set.Rejected {
		say(stderr, "warning: %s: not a spec file (%s); the importer passes over it", entry.Path, entry.Kind)
	}
	var refused *notes.SizeRefusal
	switch {
	case errors.Is(err, notes.ErrNoDirectory):
		// Git keeps no empty directory, so a commit with no spec file has no directory, and the
		// importer reads a missing directory as the empty set: it has no check.
		say(stderr, "%s: does not exist at %s, so it is read as the empty tree", dir, commit)
		return specgraph.Tree{}, true
	case errors.As(err, &refused):
		for _, problem := range refused.Problems {
			say(stderr, "%s", problem)
		}
		say(stderr, "%s: the tree is refused, so no result is written", dir)
		return specgraph.Tree{}, false
	case err != nil:
		say(stderr, "%s: %s", dir, err)
		return specgraph.Tree{}, false
	}
	tree, problems := specgraph.Judge(set.Files, specgraph.Options{})
	if len(problems) == 0 {
		return tree, true
	}
	for _, p := range problems {
		say(stderr, "%s:%d: %s: %s", filepath.Join(dir, p.Pos.File), p.Pos.Line, p.Node, p.Detail)
	}
	say(stderr, "%s: the tree is refused, so no result is written", dir)
	return specgraph.Tree{}, false
}

// loadReport is the go test -json report at path, as specgraph.ReadGoTestJSON reads it. A report
// that holds anything but events is an error, so an unreadable report never reads as a run of no
// tests.
func loadReport(path string) ([]specgraph.TestOutcome, error) {
	report, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer report.Close()
	ran, err := specgraph.ReadGoTestJSON(report)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return ran, nil
}

// record runs the dispatch command cli once for the check of p, and answers its exit status, the
// id of the stored result, what its line says of the call, and whether the call stored the
// result. A refused call stored nothing, and its words are what it wrote on both streams: the
// command prints a refusal of Dispatch or of the broker on standard output (`print` in
// packages/envoy-client/src/dispatch-cli.ts), and its warnings on standard error. A write whose
// outcome is unknown, {"write_outcome_unknown":"<code>"}, is a failed call that names the code,
// whatever the exit status: it may be stored, and a second run of the step is safe, since
// Dispatch keeps each result. Of a call that
// stored its result it reads only the stored row's id, and it never prints the rest. A write that
// landed with its answer lost, {"landed_without_answer":"<code>"} with exit 0, stored the result,
// and its id is unknown.
func record(cli []string, project, commit string, p Planned) (code int, id, why string, stored bool) {
	args := append(slices.Clone(cli[1:]), "result", "--project", project, "--check", p.Check, "--verdict", p.Outcome,
		"--commit", commit, "--observed-file", "-")
	cmd := exec.Command(cli[0], args...)
	cmd.Stdin = strings.NewReader(p.Observed)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exitError *exec.ExitError
	if err != nil && !errors.As(err, &exitError) {
		return -1, "", err.Error(), false
	}
	code = cmd.ProcessState.ExitCode()
	// The command's first line on standard output: a stored row, a landed write, or a write whose
	// outcome is unknown; a refusal is "<CODE>: <message>", which is no JSON.
	var line struct {
		ID      string `json:"id"`
		Landed  string `json:"landed_without_answer"`
		Unknown string `json:"write_outcome_unknown"`
	}
	decoded := json.NewDecoder(bytes.NewReader(stdout.Bytes())).Decode(&line) == nil
	switch {
	case decoded && line.Unknown != "":
		// The write went out and no answer of Dispatch's said whether it stored the result (a 2xx
		// that names no key back among them): it may be stored, and it is never read as stored.
		return code, "", fmt.Sprintf("the write went out and no answer said whether Dispatch stored it (%s): a second run of the step is safe, since Dispatch keeps each result", line.Unknown), false
	case code != 0:
		words := strings.TrimSpace(strings.TrimSpace(stderr.String()) + "\n" + strings.TrimSpace(stdout.String()))
		if words == "" {
			words = fmt.Sprintf("the dispatch command ended with %s and printed nothing", cmd.ProcessState)
		}
		return code, "", words, false
	case decoded && line.ID != "":
		return 0, line.ID, "", true
	case decoded && line.Landed != "":
		return 0, "", fmt.Sprintf("the result was stored, and its answer was lost on the way back (%s), so its id is unknown", line.Landed), true
	}
	return 0, "", "the dispatch command exited 0 and printed no stored result with an id", false
}

// writeLine writes l to w as one JSON line. encoding/json writes each control character of a
// value as an escape, so no line break reaches the log, and the line starts with "{", so it starts
// no workflow command; the "[" of each "##[", which can only be in a value, is written as \u005b.
func writeLine(w io.Writer, l checkLine) error {
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(l); err != nil {
		return err
	}
	_, err := io.WriteString(w, strings.ReplaceAll(encoded.String(), "##[", `##\u005b`))
	return err
}
