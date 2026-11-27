// Command spec-check judges a directory of spec files, a checkout's .dispatch/spec, by the rules
// of package specgraph, lists what changed between two such directories, compares one with an
// approved spec and prints the approved text into the files, checks that no CI file of a
// repository runs that comparison, derives the states of a scenario, names each program check that
// CI refuses at a commit, and lists the files that changed between two commits. It reads the
// directory as the Dispatch server's importer reads .dispatch/architecture: notes.ReadSet takes
// each specgraph.IsFile entry, a *.yaml file directly in it, within the size bound, and passes
// over every other entry. Every rule of the spec files and of a scenario is specgraph's (Parse,
// its schema, Validate, Compare, Layout.Place, ReadScenario, Derive, WaitCycle and Citations); the
// command states none of its own. The rule of guard is the command's (cifiles.go), since it reads
// CI files and no spec file.
//
// Usage:
//
//	spec-check tree --dir <dir> [--allow-empty] [--retired <file>]
//	spec-check changes --base <dir> --dir <dir>
//	spec-check checks --dir <dir> --job <job>
//	spec-check compare --dir <dir> --approved <approved.json> [--allow-empty]
//	spec-check print --approved <approved.json> --id <id> [--id <id>...] --dir <dir>
//	spec-check guard --root <dir>
//	spec-check state --scenario <dir> [--json]
//	  (each subject of <dir>/subjects.json: commit, branch or deploy, and graph_tx with a deploy;
//	  a check of production or of the graph is judged only at a deploy, and one of the graph reads
//	  unknown there when no result read the subject's graph_tx; a run of a check of the code counts
//	  only at the commit that it read, and a failing example at the commit that it counts from, its
//	  planted_on for a program check of the code; <dir>/ties.json holds each check's tie by commit,
//	  and a failing example that the test caught, or a run of the test of a program check of
//	  production or of the graph, counts at another commit only when the check's tie is known at both
//	  and no path that changed between them reaches the tie at either (a path reaches a tie that
//	  covers it or holds a path under it); one that the test missed never counts at another commit)
//	spec-check citations --dir <dir> --repo <root> --workflow <file> (--changed <file> | --all-changed)
//	    --reports <dir> --needs <file> [--json <file>] [--allow-empty]
//	spec-check changed --repo <root> --base <commit> [--head <commit>]
//
// tree judges one directory. It exits 0 for a valid tree, with the count line "spec-check: <dir>:
// 3 statements, 4 checks, 2 components, 1 interface, 1 term; the tree is valid" on standard output.
// It exits 1 for a refused tree, with one line for each problem on standard error, "spec-check:
// <dir>/<file>:<line>: <node>: <detail>", where the detail begins with the rule, and the count line
// ending "; the tree is refused". A file above the size bound gives the line "spec-check: file
// too large: <dir>/<file> is <n> bytes (limit 1048576)" in place of the problems, and the counts
// are 0: no file is parsed. With --allow-empty, a directory that does not exist or holds no
// spec file is the empty tree, exit 0, ending "; the tree is empty"; without it, exit 2, with a
// line that names --allow-empty. --retired names a file of the ids the approved spec removed, one
// id a line, "#" lines and blank lines passed over; without it, a line on standard error says that
// no id was checked against them.
//
// changes prints one line for each statement that only one of the two directories holds, or whose
// text, parent, relations or checks differ between them, "spec-check: <id>: text, checks", or
// "spec-check: no statement changed", and exits 0; a statement that only moved to another file is
// no change. checks prints the program checks that name the job as one line of JSON,
// [{"id","runner","selector","job","procedure_sha256"}], [] when none, and exits 0. Both exit 2,
// with the problems on standard error, when a tree they read has any problem. Both read a
// directory that does not exist as the empty tree, with a line on standard error that says so,
// since git keeps no empty directory: the first change that adds a spec file lists each of its
// statements as added, and a checkout with no spec file has no program check.
//
// compare compares the files of one directory with an approved spec in the form of approved.json
// (specgraph.ReadApproved). It exits 1 when the files hold a node in a text that the approved spec
// never held, with one line for each such node on standard error, "spec-check:
// <dir>/<file>:<line>: <node>: <detail>", and "spec-check: <dir>: the comparison fails on <n>
// nodes" on standard output. It exits 0 otherwise, with "spec-check: <dir>: the approved spec held
// every text that the files hold; 5 of 7 approved statements are in the files at their current
// text", which counts statements only, the verb and its pronoun in the singular for 1 held and
// the noun for 1 of 1. Then come one line for each other current approved node, of each kind,
// "spec-check: <kind> <id>: not in the files" or "spec-check: <kind> <id>: the files hold approved
// version 1, and version 2 is current", and one line for each node that the files hold and the
// approved spec removed, "spec-check: <kind> <id>: the files hold it, and the approved spec removed
// it" (R28). The kind is statement, check, component, interface or term. It exits 2, and makes no
// comparison, for an approved.json that ReadApproved refuses (JSON that is not UTF-8, or that
// gives a member that the format does not name in its own letters or one member two times, is one
// of them), and for files with a problem of Parse that can lose a node or change one (its Effect
// is specgraph.LosesNode), such as a document that the YAML library does not read, a key that the
// node does not take, or a file that is not as print writes it, which two YAML libraries can read
// as two texts, since the comparison would not see that text; it names only those problems, and the
// line of a file that is not printed names spec-check print, the way out. A directory that does
// not exist or holds no spec file is exit 2 too, with a line that names --allow-empty. With
// --allow-empty, compare reads it as the empty tree, with a line on standard error that says so,
// since git keeps no empty directory and a revert of the last spec file leaves none. The
// comparison does not judge the tree, which is tree's work: a problem of Parse that leaves each
// node as a printed file writes it (specgraph.KeepsEachNode), a value that the schema refuses and
// the node keeps or an empty file, is no reason for compare to refuse.
// R2 gives the comparison at each push to Dispatch, and no CI job runs compare. So compare exits
// 2, before it reads an argument, when GITHUB_ACTIONS is "true", with the one line "spec-check:
// compare does not run in a CI job (GITHUB_ACTIONS=true): R2 gives the comparison of the files with
// the approved spec at each push to Dispatch". GitHub sets GITHUB_ACTIONS=true in each step of each
// job. CI=true alone, as an agent's shell sets it, is no CI job, and compare runs there. The
// refusal covers each route that passes the runner's environment on to the command. A route that
// drops it, such as sudo or a container that docker run starts, does not pass GITHUB_ACTIONS, and
// the command compares there: only guard names such a line, in the files that it reads, when the
// command's name and compare stand next to each other there.
//
// print writes the current approved document of each node it is named, and of each current check
// that serves a statement it is named, into the spec files of --dir, each node at its place by
// specgraph.Layout.Place with the bound of one file (1048576 bytes): a node that the files hold is
// replaced where it stands, a check goes after its statement, a statement at the end of its
// parent's file or of <id>.yaml, a component or an interface at the end of components.yaml, a term
// at the end of terms.yaml, and a block that does not fit moves to the next file of its file's
// series (<stem>-2.yaml and on). The run places the components and interfaces, then the terms,
// then each statement with its checks, parents first. It holds an exclusive lock on --dir for the
// whole run, and waits up to 10 seconds for another run that holds it. It writes nothing, and
// exits 2 with one line for each refusal on standard error: when another run holds the lock after
// the wait; when --dir holds a file whose documents the reader cannot say for certain
// ("spec-check: print would write into <dir>, and the reader cannot say which documents
// <dir>/<file> holds: <reason>…"); when Place refuses a node ("spec-check: print would write …",
// for a check whose statement or a statement whose parent the files do not hold, a block larger
// than one file, or a node that two places hold); when the run would write a name that is no spec
// file of --dir, such as a directory of that name; when the files that the run would leave would
// not hold a node as the files held it, or a node of the run as its approved document
// ("spec-check: print would lose or change node …"); or when the judgment of tree finds a problem
// in the files that the run would leave ("spec-check: print would leave <dir>/<file>:<line>:
// <node>: <detail>", which for a not-printed node that the run does not name ends with each node
// of that file to name, by its whole id). It exits 2 too when the approved spec does not hold an
// id or removed it, and when the system refuses to exchange --dir with the new directory that it
// builds beside it. Else it changes the files that change in one step, also for a kill, prints
// "spec-check: wrote <id> to <dir>/<file>:<line>" for each node, and exits 0, or 1 when it kept
// beside --dir an entry that another program made or changed there, or whose move back or removal
// the system refused, with a line that names what it kept, where, and each cause.
//
// guard reads every regular file under <dir>/.github/workflows, <dir>/.github/actions,
// <dir>/.github/scripts and <dir>/scripts as text, and names each line that names the command that
// compares the files (ciFilesThatCompare) on standard error, "spec-check: <dir>/<path>:<line>:
// this line names the command that compares the files with the approved spec, and no CI job runs
// that command (R2). If the line only mentions the command, for example in a comment, a quoted
// text, a pattern or a name, reword it.", with "spec-check: <dir>: <n> lines of CI files name the
// command that compares the files" on standard output, exit 1. It exits 1 with "spec-check: <dir>:
// read no workflow" when <dir>/.github/workflows holds no file, since an instrument that read
// nothing has not passed, and 0 otherwise, with "spec-check: <dir>: no line of <k> CI files names
// the command that compares the files". The guard names each place of a CI file where the
// command's name and then the word compare follow each other with only white space, commas, ( or [
// between them, and with quote characters (also a $ before one), backslashes, and line breaks with
// the indentation after them ignored. So it also names a comment or a quoted text that no shell
// runs, and the author rewords that line. The name is spec-check, under any path, as a word of its
// own: a word that only holds it, such as envoy-spec-check or spec-checker, is another name, and
// compare is a word of its own too. Each bound reads the file's own bytes. Right before the name,
// a letter, a digit, _ or - makes the name part of another word. Right after compare, a letter, a
// digit, _, -, . or / makes compare part of another word or a path, such as compare-all,
// compare.sh or compare/. Each other byte ends the word: white space, a quote, ;, |, &, (, ), <, >,
// a comma, a bracket, and the start or the end of the file. A backslash right after compare is an
// escape, and the byte after it takes its place. A quote there ends the word only when no letter,
// digit or _ follows the quote, so "compare"d and compare\"d are other words. After a backslash, n,
// t and r stand for white space, so \n, \t and \r end the word on each side, as a backslash before
// a space or a line break does after compare. So a name right after \7 or \- is not named, nor
// compare before \x20 or \x64.
// The rule reads the file's bytes and runs no shell expansion, so it does not find a command that
// an expansion, an escape or an alias builds, one with another word between the two, such as a
// redirection, a here-document or an image (docker run --entrypoint spec-check <image> compare),
// or a run: | block whose two line continuations need the indentation after one kept and after the
// other removed. Before the name, only \n, \t and \r of the escapes stand for white space; each
// other escape or expansion of one language that builds the boundary is outside the claim (\x20,
// \x0a, \012, \u000a, PowerShell's `n, a batch file's %~dp0, $1), and so are \0 right before the
// name or right after compare, a NUL byte that Bash skips or stops at, \t between the two words,
// and a letter, a digit or _ right after the quote that ends compare, which Bash reads as more of
// the word and another language can read as its next token, such as Python's f or r prefix
// ("compare"f" --dir"). It is no defence against an author who hides the command: compare itself
// refuses to run in a CI job. A step can run the comparison from a file it does not read, such as a
// package script or a make target: compare's refusal covers that route when the step passes the
// runner's environment on, and nothing covers it when the step does not.
// The guard follows no symbolic link at or under the four directories (the path to them, --root
// and .github included, resolves as the system resolves it), and it reads each file whole, with
// no bound on its size.
//
// state reads a scenario of The Spec in the format of specgraph.ReadScenario (the directories of
// internal/dispatch/specgraph/testdata/scenarios; packages/envoy/docs/spec-check-state.md is its
// guide for a user) and derives the state of each check and each statement at each subject of it.
// For each subject it prints "spec-check: subject <commit>, the
// head of <branch>", "spec-check: subject <commit>, the commit that production runs at deploy
// <deploy>", or with the graph_tx of the subject "…, the commit that production runs at deploy
// <deploy>, and the graph at transaction <graph_tx>", then each current approved statement in
// preorder, "statement <id>: met", "met, partly checked", "not built (…)", "not met (…)" or
// "unknown (…)", each followed by its own checks that read the subject, "  check <id>: true
// (passed, counted from <x>)", "false (failed, counted from <x>)" or "unknown (<reason>)", where
// <x> is the commit, the deploy or the transaction of the graph that the counted result read; then
// each check whose statement the approved spec removed, "check <id> (serves no current statement):
// <state>", its state in the same words. With units.json, at the head of main, it prints "wait of
// <unit> on <statement>: ends" or "holds (<own state>)" for each wait, and after the last subject
// "spec-check: cycle of waits: <unit> -> <statement> -> … -> <unit>" or "spec-check: cycle of
// waits: none". --json prints the states as one line of JSON instead, []State, or with units.json
// {"states":[…],"cycle":[…]}. It exits 0 for a scenario it reads, and 2, with one line on standard
// error naming the file and the fault, for one it does not: a file or a field that the format does
// not hold (JSON that is not UTF-8, a member written in other letters or given two times, or a
// value of another type, named with the form of the file and its place, too), a missing file, a
// member that the derivation reads and that a file does not set (such as the at of a result), spec
// files that do not parse or that the rules of tree refuse, or a loop of parent and support
// relations in the approved spec. A check of production or of the graph is judged only
// at a subject that names a deploy, and there a check of the graph counts only the results at the
// subject's graph_tx, so it reads unknown when the subject names no graph_tx or one that no result
// read; a result of a graph check at another transaction reads as a subject that moved. A subject
// names graph_tx only with a deploy, each name of a subject has a letter or a digit, and a result
// names one subject, so the reader refuses graph_tx on a branch, a commit, branch, deploy or
// graph_tx of no letter and no digit, and a result that names two of commit, deploy and graph_tx.
// ties.json holds the tie of each check at each commit. A run of a check of the code counts only at
// the commit that it read, and a failing example at the commit that it counts from, its planted_on
// for a program check of the code; a failing example that its test caught, or a run of the test of
// a program check of production or of the graph, counts at another commit only when the check's tie
// is known at both commits and no path that changed between them reaches the tie at either commit
// (a path reaches a tie that covers it or holds a path under it), and one that its test missed
// never counts at another commit; the reader refuses a commit of ties.json of no letter and no
// digit, as it refuses such a commit of a subject, and names the form that ties.json needs when a
// value in it is of another type, such as ties by check alone.
//
// citations names each program check that CI must refuse at the commit of --repo
// (specgraph.Citations, R48): its test is not in the code, its selector picks two tests, its job is
// not in the workflow or the citations job does not wait for it, or code that its test reads
// changed and the job's report does not show the test run. A report counts a run only after the
// run's package ends, and a run that go test replayed from its cache did not run. changed writes
// the line "base <commit>" and then each path that differs between two commits, the list that
// citations --changed reads. Both refuse a --repo that is not the top of its git clone. citations
// runs go list and .github/scripts/go-test-reads.sh in <root>, and for a tie at the base the
// base's own script in a temporary checkout; changed runs git. runCitations and runChanged give
// each exit status.
//
// Every subcommand exits 2 for a directory it cannot read, a file it cannot read or arguments it
// does not take, with one line naming the case, or the usage, and for a file or a device on
// standard output that does not take all of its text, its product, such as a full disk, with the
// line "spec-check: <subcommand>: cannot write standard output: <error>" on standard error. A pipe
// whose reader has gone ends the command by SIGPIPE instead, with no line. A standard output that
// is closed at the start reads as /dev/null, since the Go runtime opens /dev/null on it before the
// program starts, so the text is lost as with >/dev/null. Each entry of a directory that is
// not a spec file is a warning on standard error, since the importer passes over it in silence.
// Paths are printed absolute. Every line but the usage, the JSON of checks and of state, and the
// statement, check and wait lines of state is written by runnerlog.Say with the prefix
// "spec-check: ", so no text a file name, a file or an argument holds starts a line of the
// runner's log or names a workflow command. Each statement, check and wait line starts with its
// fixed word and passes through runnerlog.Printable for the same reason. A JSON line holds no raw
// line break, as encoding/json escapes each control character, and checks and state write the "["
// of each "##[" in it as \u005b, which a JSON reader reads back as "[". A line of tree, compare,
// print and state cuts a long value of the input, except a value that the line tells the reader to
// type or to open (a path, an argument, an id of a way out): a node id, a value or a line of a
// file, a name of code or a value of a scenario is cut (specgraph.Cut), whole up to 100 bytes, else
// its first and its last 50 bytes with … between them, so no line of them grows with the files. A
// line of citations prints its check id, selector and job whole, as they are what a person types to
// find the test; and a JSON line holds each value whole, as
// data for a program. print writes each document into the spec files of its directory itself, as
// the approved spec holds it, byte for byte; no CI job runs print.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/sjawhar/envoy/internal/dispatch/architecture/notes"
	"github.com/sjawhar/envoy/internal/dispatch/specgraph"
	"github.com/sjawhar/envoy/internal/runnerlog"
)

const usage = `usage: spec-check tree --dir <dir> [--allow-empty] [--retired <file>]
       spec-check changes --base <dir> --dir <dir>
       spec-check checks --dir <dir> --job <job>
       spec-check compare --dir <dir> --approved <approved.json> [--allow-empty]
       spec-check print --approved <approved.json> --id <id> [--id <id>...] --dir <dir>
       spec-check guard --root <dir>
       spec-check state --scenario <dir> [--json]
         (each subject of <dir>/subjects.json: commit, branch or deploy, and graph_tx with a deploy;
         a check of production or of the graph is judged only at a deploy, and one of the graph reads
         unknown there when no result read the subject's graph_tx; a run of a check of the code counts
         only at the commit that it read, and a failing example at the commit that it counts from, its
         planted_on for a program check of the code; <dir>/ties.json holds each check's tie by commit,
         and a failing example that the test caught, or a run of the test of a program check of
         production or of the graph, counts at another commit only when the check's tie is known at both
         and no path that changed between them reaches the tie at either (a path reaches a tie that
         covers it or holds a path under it); one that the test missed never counts at another commit)
       spec-check citations --dir <dir> --repo <root> --workflow <file> (--changed <file> | --all-changed)
           --reports <dir> --needs <file> [--json <file>] [--allow-empty]
       spec-check changed --repo <root> --base <commit> [--head <commit>]`

// prefix is the fixed text that starts every line the command prints but the usage, the JSON of
// checks and of state, the blocks of print, and the statement, check and wait lines of state.
const prefix = "spec-check: "

// subcommands is each subcommand by its name: it takes the arguments after the name and answers
// the exit status.
var subcommands = map[string]func(args []string, stdout, stderr io.Writer) int{
	"tree":      runTree,
	"changes":   runChanges,
	"checks":    runChecks,
	"compare":   runCompare,
	"print":     runPrint,
	"guard":     runGuard,
	"state":     runState,
	"citations": runCitations,
	"changed":   runChanged,
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run runs the subcommand args name and answers its exit status, or 2 with the usage for no
// subcommand or one it does not know. The subcommand writes its standard output through output,
// so a write to stdout that fails, or takes part of its text, is one line on stderr and exit 2. A
// write to a pipe whose reader has gone does not return: SIGPIPE ends the process.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	subcommand, ok := subcommands[args[0]]
	if !ok {
		say(stderr, "unknown subcommand %q", args[0])
		fmt.Fprintln(stderr, usage)
		return 2
	}
	out := &output{w: stdout}
	code := subcommand(args[1:], out, stderr)
	if out.err != nil {
		say(stderr, "%s: cannot write standard output: %s", args[0], out.err)
		return 2
	}
	return code
}

// say writes one line through runnerlog.Say with the command's prefix.
func say(w io.Writer, format string, args ...any) {
	runnerlog.Say(w, prefix, format, args...)
}

// counts is the count line's "3 statements, 4 checks, 2 components, 1 interface, 1 term": the
// nodes the tree holds, where components counts the components other than interfaces and
// interfaces the components of kind interface, each noun in the singular for one.
func counts(statements, checks, components, interfaces, terms int) string {
	return fmt.Sprintf("%s, %s, %s, %s, %s", count(statements, "statement"), count(checks, "check"),
		count(components, "component"), count(interfaces, "interface"), count(terms, "term"))
}

// count is "<n> <noun>s", with the noun alone for one.
func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// parse reads args into flags, the flags of one subcommand, and answers whether the subcommand
// goes on and, when it does not, its exit status: 0 for -h, and 2 for a flag it does not take, an
// argument besides the flags or a required flag left empty, each with one line naming the case,
// then the usage. The flag package would print a parse error with the argument in it as it is, so
// parse prints nothing through it and writes the error through runnerlog.Say.
func parse(flags *flag.FlagSet, args []string, stderr io.Writer, required ...string) (int, bool) {
	flags.SetOutput(io.Discard)
	err := flags.Parse(args)
	switch {
	case errors.Is(err, flag.ErrHelp):
		fmt.Fprintln(stderr, usage)
		return 0, false
	case err != nil:
		say(stderr, "%s: %s", flags.Name(), err)
	case flags.NArg() > 0:
		say(stderr, "%s: takes no argument but its flags, got %q", flags.Name(), flags.Arg(0))
	default:
		missing := slices.IndexFunc(required, func(name string) bool { return flags.Lookup(name).Value.String() == "" })
		if missing < 0 {
			return 0, true
		}
		say(stderr, "%s: --%s is required", flags.Name(), required[missing])
	}
	fmt.Fprintln(stderr, usage)
	return 2, false
}

// absolute returns path as an absolute path, as the command prints every path, and whether it
// could; when it could not, it has written the line that names the path.
func absolute(stderr io.Writer, path string) (string, bool) {
	abs, err := filepath.Abs(path)
	if err != nil {
		say(stderr, "%s: cannot be read: %s", path, err)
		return "", false
	}
	return abs, true
}

// isDirectory is whether path is a directory; when not, it has written the line that says why.
func isDirectory(stderr io.Writer, path string) bool {
	switch info, err := os.Stat(path); {
	case errors.Is(err, fs.ErrNotExist):
		say(stderr, "%s: does not exist", path)
	case err != nil:
		say(stderr, "%s: cannot be read: %s", path, err)
	case !info.IsDir():
		say(stderr, "%s: is not a directory", path)
	default:
		return true
	}
	return false
}

// judged is one directory of spec files, read and judged: its tree, every problem of Parse and,
// for judge, of Validate in one list, sorted by file, line, node, rule and detail, how many files
// it held, and for judgeFiles each file above the size bound, in place of the problems.
type judged struct {
	dir      string
	tree     specgraph.Tree
	problems []specgraph.Problem
	files    int
	oversize []error
}

// judge reads dir with readFiles and judges its files with judgeFiles.
func judge(stderr io.Writer, dir string, o specgraph.Options) (judged, error) {
	files, err := readFiles(stderr, dir)
	if err != nil {
		return judged{dir: dir}, err
	}
	return judgeFiles(dir, files, o), nil
}

// judgeFiles is the judgment of tree over files in memory: notes.CheckSize of each file, Parse,
// and Validate with o, its problems sorted. A file above the bound is in oversize, and then no file
// is parsed, as the importer refuses a set before it parses any. print judges the files that a run
// would leave with it, so print and tree judge by one rule.
func judgeFiles(dir string, files map[string][]byte, o specgraph.Options) judged {
	j := judged{dir: dir, files: len(files)}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		if err := notes.CheckSize(filepath.Join(dir, name), int64(len(files[name]))); err != nil {
			j.oversize = append(j.oversize, err)
		}
	}
	if len(j.oversize) > 0 {
		return j
	}
	j.tree, j.problems = specgraph.Parse(files)
	j.problems = append(j.problems, specgraph.Validate(j.tree, o)...)
	specgraph.SortProblems(j.problems)
	return j
}

// read reads dir with readFiles and parses the files it read, with no rule of Validate.
func read(stderr io.Writer, dir string) (judged, error) {
	files, err := readFiles(stderr, dir)
	if err != nil {
		return judged{dir: dir}, err
	}
	tree, problems := specgraph.Parse(files)
	return judged{dir: dir, tree: tree, problems: problems, files: len(files)}, nil
}

// readFiles reads the spec files of dir with readSet, and answers them, by name.
func readFiles(stderr io.Writer, dir string) (map[string][]byte, error) {
	set, err := readSet(stderr, dir)
	return set.Files, err
}

// readSet reads the spec files of dir with notes.ReadSet and specgraph.IsFile, and warns on stderr
// of each entry that is not a spec file; for a file, the warning says what a spec file is. It opens
// dir one time (os.OpenRoot) and reads each file through it, so a read while print exchanges dir
// reads the files as they were or as the run leaves them, never a mix. The error is ReadSet's:
// notes.ErrNoDirectory for a directory that does not exist, a *notes.SizeRefusal for a file above
// the bound, and an error naming the path for any other failure to read.
func readSet(stderr io.Writer, dir string) (notes.Set, error) {
	root, err := os.OpenRoot(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return notes.Set{}, notes.ErrNoDirectory
	case err != nil:
		return notes.Set{}, fmt.Errorf("cannot be read: %w", err)
	}
	defer root.Close()
	set, err := notes.ReadSet(root.FS(), dir, specgraph.IsFile)
	for _, entry := range set.Rejected {
		if entry.Kind == notes.KindFile {
			say(stderr, "warning: %s: not a spec file (file): a spec file is a *.yaml file, which spec-check print writes; the importer passes over it", entry.Path)
			continue
		}
		say(stderr, "warning: %s: not a spec file (%s); the importer passes over it", entry.Path, entry.Kind)
	}
	return set, err
}

// sayProblems writes one line of stderr for each problem: "<dir>/<file>:<line>: <node>: <detail>",
// each value of the input in it cut (Problem.Text).
func (j judged) sayProblems(stderr io.Writer) {
	for _, p := range j.problems {
		say(stderr, "%s:%d: %s", filepath.Join(j.dir, p.Pos.File), p.Pos.Line, p.Text())
	}
}

// counts is the count line's counts of the nodes the tree holds.
func (j judged) counts() string {
	interfaces := 0
	for _, c := range j.tree.Components {
		if c.Interface {
			interfaces++
		}
	}
	return counts(len(j.tree.Statements), len(j.tree.Checks), len(j.tree.Components)-interfaces, interfaces, len(j.tree.Terms))
}

// cannotJudge writes why judge could not read dir, one line for each file a size refusal names
// and one line naming the directory for any other error, and answers exit 2.
func cannotJudge(stderr io.Writer, dir string, err error) int {
	var refused *notes.SizeRefusal
	if errors.As(err, &refused) {
		for _, problem := range refused.Problems {
			say(stderr, "%s", problem)
		}
		return 2
	}
	say(stderr, "%s: %s", dir, err)
	return 2
}

// noSpecFile reports whether read, which answered j and err, found no spec file in its directory,
// and the words that name the case: "does not exist" for notes.ErrNoDirectory, and "holds no spec
// file" for a directory with none. tree, compare and citations refuse such a directory without
// --allow-empty, and read it as the empty tree with it.
func noSpecFile(j judged, err error) (string, bool) {
	switch {
	case errors.Is(err, notes.ErrNoDirectory):
		return "does not exist", true
	case err == nil && j.files == 0:
		return "holds no spec file", true
	}
	return "", false
}
