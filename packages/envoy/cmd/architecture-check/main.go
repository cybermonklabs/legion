// Command architecture-check judges one directory of Dispatch architecture notes, a checkout's
// .dispatch/architecture, by the rules the Dispatch server's importer applies to that directory
// on GitHub. Every rule is the notes package's: notes.ReadSet reads the directory by them, which
// entries are notes (notes.FileKind and notes.IsNote) and how large a note may be
// (notes.CheckSize), and notes.Parse decides whether the set is valid. The command states none of
// its own.
//
// Usage:
//
//	architecture-check [--allow-empty] <directory>
//
// It names the directory by its absolute path and answers in one of three ways, whose lines the
// composite action .github/actions/architecture-check reads:
//
//   - Exit 0, the set is valid: standard output holds one line,
//     "architecture-check: <directory>: <n> notes, the set is valid".
//   - Exit 1, the set is refused: standard error holds one line for each problem, each naming its
//     note, and standard output the line "architecture-check: <directory>: <n> notes, the set is
//     refused".
//   - Exit 2, the directory does not exist, cannot be read or holds no note, or the arguments are
//     not one directory: standard error holds one line naming the directory and which case it is,
//     or the usage line, and no count line is printed. The line for a directory that does not
//     exist or holds no note also names the way out: it ends ", and architecture-check reads it
//     as the empty set only with --allow-empty".
//
// Each entry of the directory that is not a note is a warning on standard error, since the
// importer passes over it in silence and its author expects it to count; a warning changes no
// exit status. With --allow-empty, a directory that does not exist or holds no note is the empty
// set, which the parser accepts: exit 0 with a count of 0, for a repository with no note yet.
//
// Every line the command prints, the usage aside, is written by runnerlog.Say, which writes the
// fixed text "architecture-check: " and then the rest of the line, all of it escaped as
// runnerlog.Printable does. A GitHub runner ends a line of the log at \n and at \r, and reads a
// workflow command in two forms: a line that starts with "::" once its leading white space is
// trimmed, and a "##[" anywhere in a line. Every line but the usage starts with that fixed text,
// and runnerlog.Printable writes each control character as an escape, so no line break reaches
// the log and no text the directory or an argument holds starts a line of its own. It writes the
// "[" of every "##[" as \x5b, and only a literal "##[" names a command, as its doc says, so no
// line holds a "##[" command. It also writes each format character, the line separator U+2028,
// the paragraph separator U+2029 and each byte that is not UTF-8 as an escape, for a reader of
// the log: a format character is invisible or reorders text, and can make one name read as
// another, and a log viewer can show a line break at either separator. A problem whose text spans
// several lines, as yaml.v3's answer for an unknown key does, prints on one line with each line
// break as \n.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/sjawhar/envoy/internal/dispatch/architecture/notes"
	"github.com/sjawhar/envoy/internal/runnerlog"
)

const usage = "usage: architecture-check [--allow-empty] <directory>"

// prefix is the fixed text that starts every line the command prints but the usage.
const prefix = "architecture-check: "

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run judges the directory args name and answers the exit status: 0 for a valid set, 1 for a
// refused one, 2 for a directory it cannot judge or arguments that do not name one directory.
func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("architecture-check", flag.ContinueOnError)
	// The usage line and the flag list PrintDefaults writes are this program's own fixed text,
	// the one output that does not pass through runnerlog.Say.
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), usage)
		flags.PrintDefaults()
	}
	allowEmpty := flags.Bool("allow-empty", false, "read a directory that does not exist or holds no note as the empty set, which is valid")
	// The flag package prints a parse error with the argument in it as it is, so Parse prints
	// nothing and run prints the error through runnerlog.Say, then the usage as Parse would.
	flags.SetOutput(io.Discard)
	err := flags.Parse(args)
	flags.SetOutput(stderr)
	switch {
	case errors.Is(err, flag.ErrHelp):
		flags.Usage()
		return 0
	case err != nil:
		runnerlog.Say(stderr, prefix, "%s", err)
		flags.Usage()
		return 2
	}
	if flags.NArg() != 1 {
		runnerlog.Say(stderr, prefix, "want one directory argument, got %d", flags.NArg())
		flags.Usage()
		return 2
	}
	dir, err := filepath.Abs(flags.Arg(0))
	if err != nil {
		runnerlog.Say(stderr, prefix, "%s: cannot be read: %s", flags.Arg(0), err)
		return 2
	}

	set, err := notes.ReadSet(os.DirFS(dir), dir)
	for _, entry := range set.Rejected {
		runnerlog.Say(stderr, prefix, "warning: %s: not a note (%s); the importer passes over it", entry.Path, entry.Kind)
	}
	var refused *notes.SizeRefusal
	switch {
	case errors.As(err, &refused):
		return refuse(stdout, stderr, dir, refused.Held, refused.Problems)
	case errors.Is(err, notes.ErrNoDirectory) && *allowEmpty:
		// Git keeps no empty directory, so a repository with no note yet has no directory at all,
		// and the server reads a missing directory as the empty model.
	case errors.Is(err, notes.ErrNoDirectory):
		runnerlog.Say(stderr, prefix, "%s: %s, and architecture-check reads it as the empty set only with --allow-empty", dir, err)
		return 2
	case err != nil:
		runnerlog.Say(stderr, prefix, "%s: %s", dir, err)
		return 2
	case len(set.Files) == 0 && !*allowEmpty:
		runnerlog.Say(stderr, prefix, "%s: holds no note, and architecture-check reads it as the empty set only with --allow-empty", dir)
		return 2
	}
	if _, err := notes.Parse(set.Files); err != nil {
		return refuse(stdout, stderr, dir, len(set.Files), problems(err))
	}
	runnerlog.Say(stdout, prefix, "%s: %s, the set is valid", dir, count(len(set.Files)))
	return 0
}

// problems splits notes.Parse's refusal into its problems: the errors.Join it answers holds one
// error for each.
func problems(err error) []error {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		return joined.Unwrap()
	}
	return []error{err}
}

// refuse prints a refused set of held notes, each problem as one line of standard error and then
// the count line on standard output, and answers exit 1. runnerlog.Say keeps each problem on one
// line: a line break in it, in yaml.v3's answer for an unknown key or in a note's name, prints as
// \n.
func refuse(stdout, stderr io.Writer, dir string, held int, problems []error) int {
	for _, problem := range problems {
		runnerlog.Say(stderr, prefix, "%s: %s", dir, problem)
	}
	runnerlog.Say(stdout, prefix, "%s: %s, the set is refused", dir, count(held))
	return 1
}

// count is the count line's "<n> notes", with "note" for one.
func count(n int) string {
	if n == 1 {
		return "1 note"
	}
	return fmt.Sprintf("%d notes", n)
}
