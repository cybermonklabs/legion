// Command architecture-check judges one directory of Dispatch architecture notes, a checkout's
// .dispatch/architecture, by the rules the Dispatch server's importer applies to that directory
// on GitHub. Every rule is the notes package's: which entries are notes (notes.FileKind and
// notes.IsNote), how large a note may be (notes.CheckSize) and whether the set is valid
// (notes.Parse). The command states none of its own.
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
//     or the usage line, and no count line is printed.
//
// Each entry of the directory that is not a note is a warning on standard error, since the
// importer passes over it in silence and its author expects it to count; a warning changes no
// exit status. With --allow-empty, a directory that does not exist or holds no note is the empty
// set, which the parser accepts: exit 0 with a count of 0, for a repository with no note yet.
//
// Every line the command prints, the usage aside, is written by say, which writes the fixed text
// "architecture-check: " itself and then the rest of the line escaped as printable does. A GitHub
// runner ends a line of the log at \n and at \r, and reads a workflow command in two forms: a line
// that starts with "::" once its leading white space is trimmed, and a "##[" anywhere in a line.
// Every line but the usage starts with that fixed text, and printable writes each control
// character as an escape, so no line break reaches the log and no text the directory or an
// argument holds starts a line of its own. It writes the "[" of every "##[" as \x5b, and only a
// literal "##[" names a command, as printable's doc says, so no line holds a "##[" command. It also
// writes each format character, the line separator U+2028, the paragraph separator U+2029 and each
// byte that is not UTF-8 as an escape, for a reader of the log: a format character is invisible or
// reorders text, and can make one name read as another, and a log viewer can show a line break at
// either separator. A problem whose text spans several lines, as yaml.v3's answer for an unknown
// key does, prints on one line with each line break as \n.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/dispatch/architecture/notes"
)

const usage = "usage: architecture-check [--allow-empty] <directory>"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run judges the directory args name and answers the exit status: 0 for a valid set, 1 for a
// refused one, 2 for a directory it cannot judge or arguments that do not name one directory.
func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("architecture-check", flag.ContinueOnError)
	// The usage line and the flag list PrintDefaults writes are this program's own fixed text,
	// the one output that does not pass through say.
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), usage)
		flags.PrintDefaults()
	}
	allowEmpty := flags.Bool("allow-empty", false, "read a directory that does not exist or holds no note as the empty set, which is valid")
	// The flag package prints a parse error with the argument in it as it is, so Parse prints
	// nothing and run prints the error through say, then the usage as Parse would.
	flags.SetOutput(io.Discard)
	err := flags.Parse(args)
	flags.SetOutput(stderr)
	switch {
	case errors.Is(err, flag.ErrHelp):
		flags.Usage()
		return 0
	case err != nil:
		say(stderr, "%s", err)
		flags.Usage()
		return 2
	}
	if flags.NArg() != 1 {
		say(stderr, "want one directory argument, got %d", flags.NArg())
		flags.Usage()
		return 2
	}
	dir, err := filepath.Abs(flags.Arg(0))
	if err != nil {
		say(stderr, "%s: cannot be read: %s", flags.Arg(0), err)
		return 2
	}

	files, rejects, err := buildSet(os.DirFS(dir), dir)
	for _, entry := range rejects {
		say(stderr, "warning: %s: not a note (%s); the importer passes over it", entry.path, entry.kind)
	}
	var refused *sizeRefusal
	switch {
	case errors.As(err, &refused):
		return refuse(stdout, stderr, dir, refused.held, refused.problems)
	case errors.Is(err, errNoDirectory) && *allowEmpty:
		// Git keeps no empty directory, so a repository with no note yet has no directory at all,
		// and the server reads a missing directory as the empty model.
	case err != nil:
		say(stderr, "%s: %s", dir, err)
		return 2
	case len(files) == 0 && !*allowEmpty:
		say(stderr, "%s: holds no note", dir)
		return 2
	}
	if _, err := notes.Parse(files); err != nil {
		return refuse(stdout, stderr, dir, len(files), problems(err))
	}
	say(stdout, "%s: %s, the set is valid", dir, count(len(files)))
	return 0
}

// rejected is an entry of the directory that notes.IsNote rejected, which the command warns of:
// its absolute path and its kind.
type rejected struct {
	path string
	kind notes.Kind
}

// errNoDirectory is the set builder's answer for a directory that does not exist.
var errNoDirectory = errors.New("does not exist")

// sizeRefusal is the bound's refusal of a set: one notes.CheckSize error for each note above
// notes.MaxFileSize, each naming the note's absolute path. It is exit 1, a refused set, as a
// notes.Parse problem is; held is the number of notes the set holds, for the count line, and no
// note of it was parsed.
type sizeRefusal struct {
	held     int
	problems []error
}

func (r *sizeRefusal) Error() string   { return errors.Join(r.problems...).Error() }
func (r *sizeRefusal) Unwrap() []error { return r.problems }

// buildSet reads the note set of the directory fsys holds, whose absolute path is dir: each entry
// notes.FileKind and notes.IsNote accept, by name, with its content, and each entry they reject,
// in name order. It checks the size of every note with notes.CheckSize before it reads any, as the
// server's GitHub reader refuses a listing before any blob read, and the length of each note it
// read again, as that reader checks each blob; either refusal is a *sizeRefusal and answers no
// files. A directory that does not exist is errNoDirectory, and any other failure to read it or a
// note is cannotRead's "cannot be read" error.
func buildSet(fsys fs.FS, dir string) (map[string][]byte, []rejected, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, errNoDirectory
	}
	if err != nil {
		return nil, nil, cannotRead(dir, err)
	}
	var rejects []rejected
	var names []string
	var refusals []error
	for _, entry := range entries {
		kind := notes.FileKind(entry.Type())
		if !notes.IsNote(entry.Name(), kind) {
			rejects = append(rejects, rejected{path: filepath.Join(dir, entry.Name()), kind: kind})
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, rejects, cannotRead(dir, err)
		}
		if err := notes.CheckSize(filepath.Join(dir, entry.Name()), info.Size()); err != nil {
			refusals = append(refusals, err)
		}
		names = append(names, entry.Name())
	}
	if len(refusals) > 0 {
		return nil, rejects, &sizeRefusal{held: len(names), problems: refusals}
	}
	files := make(map[string][]byte, len(names))
	for _, name := range names {
		content, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, rejects, cannotRead(dir, err)
		}
		if err := notes.CheckSize(filepath.Join(dir, name), int64(len(content))); err != nil {
			refusals = append(refusals, err)
		}
		files[name] = content
	}
	if len(refusals) > 0 {
		return nil, rejects, &sizeRefusal{held: len(names), problems: refusals}
	}
	return files, rejects, nil
}

// cannotRead is the set builder's answer for a directory or a note it could not read, exit 2: err
// wrapped as "cannot be read". os.DirFS names a path in its error relative to the directory, so
// cannotRead first makes it absolute, as every path the command prints is.
func cannotRead(dir string, err error) error {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) && !filepath.IsAbs(pathErr.Path) {
		pathErr.Path = filepath.Join(dir, filepath.FromSlash(pathErr.Path))
	}
	return fmt.Errorf("cannot be read: %w", err)
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
// the count line on standard output, and answers exit 1. say keeps each problem on one line: a
// line break in it, in yaml.v3's answer for an unknown key or in a note's name, prints as \n.
func refuse(stdout, stderr io.Writer, dir string, held int, problems []error) int {
	for _, problem := range problems {
		say(stderr, "%s: %s", dir, problem)
	}
	say(stdout, "%s: %s, the set is refused", dir, count(held))
	return 1
}

// say writes one line to w: the fixed text "architecture-check: ", then format filled with args
// and escaped by printable. Every line the command prints but the usage is written here, so every
// such line starts with that fixed text whatever the format, no print site can leave an argument
// unescaped, and a "##[" that an argument and the format text make together is escaped too. The
// fixed text ends in ": ", so it makes no "##[" with the text after it, and printable keeps the
// rest of the line on it, so no text of an argument or of the directory starts a line.
func say(w io.Writer, format string, args ...any) {
	fmt.Fprintln(w, "architecture-check: "+printable(fmt.Sprintf(format, args...)))
}

// printable is s as the command prints it after a line's fixed text. A file name or a front-matter
// value in a pull request is its author's text, and a GitHub runner reads workflow commands from
// the log: it ends a line at \n and at \r alike, and reads a command in two forms, a line that
// starts with "::" once its leading white space is trimmed, and a "##[" anywhere in a line.
//
// printable writes each control character (Unicode category Cc: C0, DEL and C1) as a Go string
// literal writes it, `\n`, `\r` or `\x1b`, so no line break of s reaches the log and no text of s
// starts a line of its own: every line say writes starts with its fixed text.
//
// Then it writes the "[" of every "##[" as `\x5b`, so "###[" prints as `###\x5b` and "##[##[" as
// `##\x5b##\x5b`. No escape holds a "#" or a "[", so no "##[" is left, and that keeps out every
// command of the second form, since only a literal "##[" names one. The runner finds "##[" with a
// culture-sensitive search, which can pass over an ignorable character such as U+034F or U+FE0F
// as if it were not there. But it then reads the command name a fixed three characters after the
// start of the match, and accepts only a name of its registered commands, compared without regard
// to case. So in a match through an ignorable character the name starts inside the match, at a
// "#", a "[" or that character, and no command the runner defines has that name. In actions/runner
// at commit 8cfa1049f86781d141056082d73802d56cd2b433, ActionCommand.TryParse reads the name at
// cmdIndex = prefixIndex + Prefix.Length, and ActionCommandManager keeps the registered commands
// in _registeredCommands, a set built with StringComparer.OrdinalIgnoreCase.
//
// printable also writes each format character (Cf: the bidi controls, the zero-width joiner and
// non-joiner, the soft hyphen, the byte order mark) as `\u202e`, `\u200d`, `\u00ad` or `\ufeff`,
// the line separator U+2028 (Zl) and the paragraph separator U+2029 (Zp) as `\u2028` and
// `\u2029`, and each byte that is not UTF-8 as `\xNN`. That is for a reader of the log, not for
// the runner: a format character is invisible or reorders the text around it, so it can make one
// name read as another, and the runner does not end a line at U+2028 or U+2029 (it reads lines
// with StreamReader.ReadLine), but a log viewer can show a line break there. Text that is valid
// UTF-8 and holds none of these is s itself.
func printable(s string) string {
	if !utf8.ValidString(s) || strings.ContainsFunc(s, escaped) {
		var b strings.Builder
		for i := 0; i < len(s); {
			r, size := utf8.DecodeRuneInString(s[i:])
			if (r == utf8.RuneError && size == 1) || escaped(r) {
				// strconv quotes a lone byte that is not UTF-8 as \xNN, and r as its escape.
				var quoted [12]byte // `"\U0010ffff"`, the longest a rune quotes to
				escape := strconv.AppendQuote(quoted[:0], s[i:i+size])
				b.Write(escape[1 : len(escape)-1])
			} else {
				b.WriteString(s[i : i+size])
			}
			i += size
		}
		s = b.String()
	}
	return strings.ReplaceAll(s, "##[", `##\x5b`)
}

// escaped reports whether printable writes r as an escape: a control character (Unicode category
// Cc), such as a line break or a terminal escape, a format character (Cf), which is invisible or
// reorders text, or the line separator U+2028 (Zl) or the paragraph separator U+2029 (Zp), at
// which a log viewer can show a line break.
func escaped(r rune) bool {
	return unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp)
}

// count is the count line's "<n> notes", with "note" for one.
func count(n int) string {
	if n == 1 {
		return "1 note"
	}
	return fmt.Sprintf("%d notes", n)
}
