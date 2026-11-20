// Command spec-check judges a directory of spec files, a checkout's .dispatch/spec, by the rules
// of package specgraph, lists what changed between two such directories, compares one with an
// approved spec and prints the approved text, and checks that no CI file of a repository runs that
// comparison. It reads the directory as the Dispatch server's importer reads
// .dispatch/architecture: notes.ReadSet takes each .md file directly in it, within the size
// bound, and passes over every other entry. Every rule of the spec files is specgraph's (Parse,
// Validate and Compare); the command states none of its own. The rule of guard is the command's
// (cifiles.go), since it reads CI files and no spec file.
//
// Usage:
//
//	spec-check tree --dir <dir> [--allow-empty] [--retired <file>]
//	spec-check changes --base <dir> --dir <dir>
//	spec-check checks --dir <dir> --job <job>
//	spec-check compare --dir <dir> --approved <approved.json> [--allow-empty]
//	spec-check print --approved <approved.json> --id <id> [--id <id>...]
//	spec-check guard --root <dir>
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
// (specgraph.ApprovedFile). It exits 1 when the files hold a node in a text that the approved spec
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
// comparison, for files with a problem of Parse that can lose a node or change one (its Effect is
// specgraph.LosesNode), such as a block whose opening line the grammar refuses or a line outside a
// block, since the comparison would not see that text; it names only those problems. A directory
// that does not exist or holds no spec file is exit 2 too, with a line that names --allow-empty.
// With --allow-empty, compare reads it as the empty tree, with a line on standard error that says
// so, since git keeps no empty directory and a revert of the last spec file leaves none. A problem
// of Parse that keeps each node (specgraph.KeepsEachNode), such as a heading that names code, is
// tree's to judge, and compare compares those files: each node holds the text that the files
// write, except a raw 0xA0 or 0x85 byte where its opening line's attributes take white space (after
// {, between two attributes or before }), which it drops as white space. A heading is no node, so
// compare never compares it. R2 gives the comparison at each push to
// Dispatch, and no CI job runs compare. So compare exits 2, before it reads an argument, when
// GITHUB_ACTIONS is "true", with the one line "spec-check: compare does not run in a CI job
// (GITHUB_ACTIONS=true): R2 gives the comparison of the files with the approved spec at each push
// to Dispatch". GitHub sets GITHUB_ACTIONS=true in each step of each job. CI=true alone, as an
// agent's shell sets it, is no CI job, and compare runs there. The refusal covers each route that
// passes the runner's environment on to the command. A route that drops it, such as sudo or a
// container that docker run starts, does not pass GITHUB_ACTIONS, and the command compares there:
// only guard names such a line, in the files that it reads, when the command's name and compare
// stand next to each other there.
//
// print writes the current approved block of each node it is named, in the order named, and after
// a statement the block of each current check that serves it, each block once and the blocks
// separated by one blank line, so that a change copies them into the files. It exits 2, and
// prints nothing, when the approved spec does not hold an id or removed it.
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
// Every subcommand exits 2 for a directory it cannot read, a file it cannot read or arguments it
// does not take, with one line naming the case, or the usage, and for a file or a device on
// standard output that does not take all of its text, its product, such as a full disk, with the
// line "spec-check: <subcommand>: cannot write standard output: <error>" on standard error. A pipe
// whose reader has gone ends the command by SIGPIPE instead, with no line. A standard output that
// is closed at the start reads as /dev/null, since the Go runtime opens /dev/null on it before the
// program starts, so the text is lost as with >/dev/null. Each entry of a directory that is
// not a spec file is a warning on standard error, since the importer passes over it in silence.
// Paths are printed absolute. Every line but the usage, the JSON of checks and the blocks of print
// is written by runnerlog.Say with the prefix "spec-check: ", so no text a file name, a file or an
// argument holds starts a line of the runner's log or names a workflow command. The JSON line holds
// no raw line break, as encoding/json escapes each control character, and checks writes the "[" of
// each "##[" in it as \u005b, which a JSON reader reads back as "[". print writes each block as the
// approved spec holds it, since a change copies it byte for byte, so the output of print must not
// reach a CI log: a line of a block that starts with "::" would be a workflow command there.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
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
       spec-check print --approved <approved.json> --id <id> [--id <id>...]
       spec-check guard --root <dir>`

// prefix is the fixed text that starts every line the command prints but the usage, the JSON of
// checks and the blocks of print.
const prefix = "spec-check: "

// subcommands is each subcommand by its name: it takes the arguments after the name and answers
// the exit status.
var subcommands = map[string]func(args []string, stdout, stderr io.Writer) int{
	"tree":    runTree,
	"changes": runChanges,
	"checks":  runChecks,
	"compare": runCompare,
	"print":   runPrint,
	"guard":   runGuard,
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
// blocks the tree holds, where components counts the components other than interfaces and
// interfaces the components with kind="interface", each noun in the singular for one.
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

// judged is one directory of spec files, read and judged: its tree, every problem of Parse and,
// for judge, of Validate in one list, sorted by file, line, node, rule and detail, and how many
// files it held.
type judged struct {
	dir      string
	tree     specgraph.Tree
	problems []specgraph.Problem
	files    int
}

// judge reads dir as readSet does, and judges the files it read with o as every reader of spec
// files does (specgraph.Judge).
func judge(stderr io.Writer, dir string, o specgraph.Options) (judged, error) {
	set, err := readSet(stderr, dir)
	if err != nil {
		return judged{dir: dir}, err
	}
	tree, problems := specgraph.Judge(set.Files, o)
	return judged{dir: dir, tree: tree, problems: problems, files: len(set.Files)}, nil
}

// read reads dir as readSet does, and parses the files it read.
func read(stderr io.Writer, dir string) (judged, error) {
	set, err := readSet(stderr, dir)
	if err != nil {
		return judged{dir: dir}, err
	}
	tree, problems := specgraph.Parse(set.Files)
	specgraph.SortProblems(problems)
	return judged{dir: dir, tree: tree, problems: problems, files: len(set.Files)}, nil
}

// readSet reads dir with notes.ReadSet, and warns on stderr of each entry that is not a spec
// file. The error is ReadSet's: notes.ErrNoDirectory for a directory that does not exist, a
// *notes.SizeRefusal for a file above the bound, and an error naming the path for any other
// failure to read.
func readSet(stderr io.Writer, dir string) (notes.Set, error) {
	set, err := notes.ReadSet(os.DirFS(dir), dir)
	for _, entry := range set.Rejected {
		say(stderr, "warning: %s: not a spec file (%s); the importer passes over it", entry.Path, entry.Kind)
	}
	return set, err
}

// sayProblems writes one line of stderr for each problem: "<dir>/<file>:<line>: <node>: <detail>".
func (j judged) sayProblems(stderr io.Writer) {
	for _, p := range j.problems {
		say(stderr, "%s:%d: %s: %s", filepath.Join(j.dir, p.Pos.File), p.Pos.Line, p.Node, p.Detail)
	}
}

// counts is the count line's counts of the blocks the tree holds.
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
// file" for a directory with none. tree and compare refuse such a directory without --allow-empty,
// and read it as the empty tree with it.
func noSpecFile(j judged, err error) (string, bool) {
	switch {
	case errors.Is(err, notes.ErrNoDirectory):
		return "does not exist", true
	case err == nil && j.files == 0:
		return "holds no spec file", true
	}
	return "", false
}
