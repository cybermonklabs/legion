package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"unsafe"

	legionclaim "github.com/sjawhar/legion/daemon/internal/claim" // main_test.go's `claim` helper holds the bare name
	"github.com/sjawhar/legion/daemon/internal/lookpath"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
)

// This file holds the step's reads (jjStep, jj.go): a read of commits with jjCommitTemplate (read,
// readEach) and of commit ids (ids, eachID), each with --ignore-working-copy (jjStep.jj), the
// comparison of git's HEAD and branches with what jj last imported (gitMoved), and the
// revsets of a list of commit or change ids in as few words as keep each under jjWordBound bytes
// (jjWords, jjChanges, jjCommitIDs), with the bytes Linux counts against jjArgMax (jjSize); and
// how the step and the expansion run a jj command, name its failure and print what it printed on
// standard error (jj, output, command, runChild, jjErrorLine, jjOnceOnly).

// jjCommitTemplate prints, for each commit, its change id, its commit id, whether it is hidden,
// whether it is the working-copy commit of the workspace the command runs in
// (current_working_copy, which jj reads from the operation, --ignore-working-copy or not), its
// author's email and the value of its first Legion-Role trailer, each ended by NUL, which none of
// them holds.
const jjCommitTemplate = `change_id ++ "\0" ++ commit_id ++ "\0" ++ hidden ++ "\0" ++ current_working_copy ++ "\0" ++ author.email() ++ "\0" ++ trailers.filter(|t| t.key() == "Legion-Role").map(|t| t.value()).join("\n") ++ "\0"`

// jjCommitRead is one commit as jjCommitTemplate prints it.
type jjCommitRead struct {
	change, commit  string
	hidden, working bool
	email           string
	role            legionclaim.Role
}

// readEach is each commit of the revsets of words, read with jjCommitTemplate (read), one command
// for each, and each commit once, in the order the reads print them (jjEachOnce).
func (s *jjStep) readEach(words []string) ([]jjCommitRead, error) {
	return jjEachOnce(words, s.read, func(c jjCommitRead) string { return c.commit })
}

// read is each commit of revset, read with jjCommitTemplate.
func (s *jjStep) read(revset string) ([]jjCommitRead, error) {
	out, err := s.jj(s.env, "--color=never", "log", "--no-graph", "-r", revset, "-T", jjCommitTemplate)
	if err != nil {
		return nil, err
	}
	fields := strings.Split(out, "\x00")
	if len(fields)%6 != 1 || fields[len(fields)-1] != "" {
		return nil, fmt.Errorf("jj log -r %s printed %q, which is not one record of six fields for each commit", revset, out)
	}
	commits := make([]jjCommitRead, 0, len(fields)/6)
	for i := 0; i+6 < len(fields); i += 6 {
		role, _, _ := strings.Cut(fields[i+5], "\n")
		commits = append(commits, jjCommitRead{
			change: fields[i], commit: fields[i+1], hidden: fields[i+2] == "true", working: fields[i+3] == "true",
			email: fields[i+4], role: legionclaim.Role(role),
		})
	}
	return commits, nil
}

// ids reads the commits of revset in one command: each one's full id, once, in jj's order, and
// the id of the one that is the working-copy commit of the workspace the command runs in
// (current_working_copy, which jj reads from the operation, --ignore-working-copy or not), or ""
// where none is. A read jj refuses, or one that cannot start, is a jjStepFailure, its one error.
func (s *jjStep) ids(revset string) (ids []string, working string, err error) {
	out, err := s.jj(s.env, "--color=never", "log", "--no-graph", "-r", revset, "-T", `commit_id ++ if(current_working_copy, " @") ++ "\n"`)
	if err != nil {
		return nil, "", err
	}
	for line := range strings.Lines(out) {
		id, mark, _ := strings.Cut(strings.TrimSuffix(line, "\n"), " ")
		if id == "" {
			continue
		}
		ids = append(ids, id)
		if mark == "@" {
			working = id
		}
	}
	return ids, working, nil
}

// eachID is the full id of each commit of revsets, read with one command for each (ids), each
// once, in the order the reads print them (jjEachOnce).
func (s *jjStep) eachID(revsets []string) ([]string, error) {
	return jjEachOnce(revsets, func(revset string) ([]string, error) {
		ids, _, err := s.ids(revset)
		return ids, err
	}, func(id string) string { return id })
}

// jjGitBookmarkTemplate prints, for each @git bookmark that `jj bookmark list --all-remotes`
// lists, the git branch as jj last imported it from git or exported it to git: its name as a JSON
// string (json), a space, and the full id of the commit it points at, or nothing where it points
// at no one commit, as a conflicted one does, then a newline. It prints nothing for a local
// bookmark or another remote's. A template that prints the name itself gets it quoted where it is
// no plain revset symbol (`"a@b"`), and json() prints each name whole: measured with jj 0.38.0,
// 0.45.1-sami and 0.46.0, in a colocated repository with the git branches `main`, `legion/K-1`,
// `a@b`, `q"x`, `x,y`, `p(q)`, `with|bar`, `a+b`, `n=1` and `é`, each release prints one line for
// each branch, with the name git's `for-each-ref` prints, once decoded.
const jjGitBookmarkTemplate = `if(remote == "git", json(name) ++ " " ++ if(normal_target, normal_target.commit_id()) ++ "\n")`

// gitMoved is whether git has moved HEAD or a branch of the workspace the push runs in since jj
// last imported them or exported them, which jj does in each of its commands: a commit that git
// made, or a branch that git moved, which no jj command has imported yet. rewrite asks it, and
// acts on its answer as rewrite says. Its reads import nothing: each jj read runs with
// --ignore-working-copy (jj), and git's reads write nothing.
//
// It reads the root of that workspace (`jj workspace root`, which jj 0.38.0 and 0.45.1-sami alike
// have). A root that holds no .git has no git working tree whose HEAD jj sets, such as an issue
// workspace under jj 0.38, which colocates none, and compares nothing: it answers false, whatever
// repository git would find in a directory above it. Otherwise it compares git's HEAD (`git
// rev-parse --verify --quiet HEAD`) with @'s first parent (first_parent(@)), to which jj sets
// git's HEAD after each of its commands, an unborn HEAD being equal to the root commit alone; and
// then git's branches (`git for-each-ref refs/heads`) with jj's @git bookmarks
// (jjGitBookmarkTemplate), as two sets of a name and a commit id, where a conflicted @git bookmark
// is a difference. jj 0.45 has no read of the HEAD it last imported, and first_parent(@) is the
// read jj 0.38 recommends in place of git_head(). Each git read runs the first git on the PATH of
// the step's environment, which gitMoved looks up once (lookpath.On): the image's git wrapper in
// a pod and a tmux pane's bin/git, each of which passes a read through to the real git. It runs
// as a gitReader with -C and that root as its globals, in that environment.
//
// A jj read that fails is its jjStepFailure, as for every other read of the step. Each of these
// answers true: a .git that cannot be read (an Lstat error but for its absence), a git read that
// cannot run (a PATH with no git among them) or that fails (but for an unborn HEAD), and a line of
// jj's whose name does not decode. Each leaves the step unable to rule out a move of git's, and
// the snapshot that follows is the one the push takes anyway, so a read that fails adds no refusal
// and still lets the step see what git did.
func (s *jjStep) gitMoved() (bool, error) {
	root, err := s.jj(s.env, "--color=never", "workspace", "root")
	if err != nil {
		return false, err
	}
	root = strings.TrimSuffix(root, "\n")
	if _, err := os.Lstat(filepath.Join(root, ".git")); err != nil {
		return !errors.Is(err, os.ErrNotExist), nil
	}
	parent, err := s.jj(s.env, "--color=never", "log", "--no-graph", "-r", "first_parent(@)", "-T", `if(root, "", commit_id) ++ "\n"`)
	if err != nil {
		return false, err
	}
	git, found := lookpath.On(envValue(s.env, "PATH"), "git")
	if !found {
		return true, nil
	}
	reader := gitReader{path: git, globals: []string{"-C", root}, env: s.env}
	// An unborn HEAD names no commit: rev-parse --verify --quiet exits 1 and prints nothing, as
	// the read of @'s first parent prints nothing but its newline for the root commit.
	head, err := reader.output("", "rev-parse", "--verify", "--quiet", "HEAD")
	unborn := procgroup.Ended(err).ExitedWith(1) && head == ""
	if (err != nil && !unborn) || strings.TrimSuffix(head, "\n") != strings.TrimSuffix(parent, "\n") {
		return true, nil
	}
	branches, err := reader.output("", "for-each-ref", "--format=%(refname:strip=2) %(objectname)", "refs/heads")
	if err != nil {
		return true, nil
	}
	bookmarks, err := s.jj(s.env, "--color=never", "bookmark", "list", "--all-remotes", "-T", jjGitBookmarkTemplate)
	if err != nil {
		return false, err
	}
	// Each line is `<JSON name> <id>\n`; an id holds no quote, so the name ends at the last `" `.
	imported := slices.Collect(strings.Lines(bookmarks))
	for i, line := range imported {
		var name string
		end := strings.LastIndex(line, `" `)
		if end < 0 || json.Unmarshal([]byte(line[:end+1]), &name) != nil {
			return true, nil
		}
		imported[i] = name + line[end+1:]
	}
	slices.Sort(imported)
	return !slices.Equal(imported, slices.Sorted(strings.Lines(branches))), nil
}

// jjEachOnce is each item of revsets, read with one command for each (read), and each item once
// by its commit id, as id reads it, in the order the reads print them.
func jjEachOnce[T any](revsets []string, read func(string) ([]T, error), id func(T) string) ([]T, error) {
	var items []T
	seen := map[string]bool{}
	for _, revset := range revsets {
		got, err := read(revset)
		if err != nil {
			return nil, err
		}
		for _, item := range got {
			if key := id(item); !seen[key] {
				seen[key] = true
				items = append(items, item)
			}
		}
	}
	return items, nil
}

// jjChanges is the revsets of the commits of each of changes, by change id: `change_id(<change>)`
// for each, joined by ` | ` into as few revsets as keep each under jjWordBound bytes (jjWords).
func jjChanges(changes []string) []string {
	named := make([]string, len(changes))
	for i, change := range changes {
		named[i] = "change_id(" + change + ")"
	}
	return jjWords("", named, "")
}

// jjCommitIDs is the term `commit_id(<id>)` of each of ids, full commit ids, each of which
// commit_id() names exactly, for a read of them in as few revsets as keep each under jjWordBound
// bytes (jjWords).
func jjCommitIDs(ids []string) []string {
	named := make([]string, len(ids))
	for i, id := range ids {
		named[i] = "commit_id(" + id + ")"
	}
	return named
}

// jjWordBound is the bound, in bytes, under which the step keeps each word it builds of a list of
// commit or change ids: the revset of one of its reads (jjWords), and a word of a revision argument
// that gives the push its commits as their ids or as its primary form (revisionGiven). Linux
// refuses one argv word over 131,072 bytes, and such a list grows with the commits it names, which
// a long history makes thousands. It is a variable so that this package's test binary, run as
// legion, can lower it (TestMain); nothing in the legion binary changes it.
var jjWordBound = 100_000

// jjArgMax is the bound, in bytes, under which the step keeps each command of its own that grows
// with the commits it names, the push's (checkArgMax) and each metaedit's (jjStep.metaedit): each
// string of its arguments and of its environment with the NUL that ends it, and the pointer to each
// (jjSize), which Linux refuses past 2,097,152 bytes, a quarter of the usual 8 MiB stack
// (execve(2)). It is a variable so that this package's test binary, run as legion, can lower it
// (TestMain); nothing in the legion binary changes it.
var jjArgMax = 2_097_152

// jjSize is the bytes that Linux counts against jjArgMax (execve(2)) for words, strings of a
// command's arguments or environment: each with the NUL that ends it, and the pointer to it.
func jjSize(words ...string) int {
	size := 0
	for _, word := range words {
		size += len(word) + 1 + int(unsafe.Sizeof(uintptr(0)))
	}
	return size
}

// jjWords is terms in as few revsets as keep each under jjWordBound bytes: each prefix, then terms
// joined by ` | `, then suffix, the terms in order. A term that no revset of its own keeps under
// the bound is a revset alone. A read of each, merged (readEach), is one read of them all where
// prefix and suffix take the union of the terms' commits to the union of what they take of each,
// as `::`, `&`, `~` and `|` do.
func jjWords(prefix string, terms []string, suffix string) []string {
	var words []string
	var word strings.Builder
	for _, term := range terms {
		if word.Len() > 0 && word.Len()+len(" | ")+len(term)+len(suffix) >= jjWordBound {
			words = append(words, word.String()+suffix)
			word.Reset()
		}
		if word.Len() == 0 {
			word.WriteString(prefix)
		} else {
			word.WriteString(" | ")
		}
		word.WriteString(term)
	}
	if word.Len() > 0 {
		words = append(words, word.String()+suffix)
	}
	return words
}

// jj runs one jj command of the step that takes no snapshot and answers what it printed on
// standard output (output): next, with the caller's options (jjPush.options), then
// --ignore-working-copy, then args, in env. It prints nothing of what the command printed on
// standard error: a read before the step has taken a warning jj prints once and printed it (under
// tmux the read of the aliases or jjStep.readConfiguration, in a pod the pod's wrapper's reads),
// and the real push prints the warnings jj prints on every command itself.
func (s *jjStep) jj(env []string, args ...string) (string, error) {
	stdout, _, err := s.output(env, slices.Concat(s.push.options, []string{"--ignore-working-copy"}, args))
	return stdout, err
}

// output runs next with args in env (command) and answers what it printed on standard output and
// on standard error. A command that cannot start or exits non-zero is a jjStepFailure, with what
// it printed on standard error.
func (s *jjStep) output(env, args []string) (stdout, stderr string, err error) {
	stdout, stderr, code, err := s.command(env, args)
	if err != nil {
		return "", "", err
	}
	if code != 0 {
		return "", "", jjStepFailure{code: code, output: stderr}
	}
	return stdout, stderr, nil
}

// command runs next with args in env and waits for it (runChild). A command that cannot start
// is a jjStepFailure whose status and output are execWrapped's for the same failure
// (wrapperStartFailure): 127 where next does not exist and 126 otherwise, and the line `legion jj:
// run <next>: <error>`.
func (s *jjStep) command(env, args []string) (stdout, stderr string, code int, err error) {
	stdout, stderr, code, err = runChild(s.next, env, args)
	if err == nil {
		return stdout, stderr, code, nil
	}
	line, status := wrapperStartFailure("legion jj", s.next, err)
	return "", "", 0, jjStepFailure{code: status, output: line}
}

// runChild runs next with args in env and waits for it, as a child in this process's group,
// with no standard input: each command of the step (jjStep.command) and the read of the aliases
// (readJJAliases). It answers what the command printed and its status, a death by a signal being
// 128 and the signal's number, as a shell reads it. A file the kernel refuses to run runs with
// /bin/sh, as execWrapped runs it. A command that cannot start is its start error alone, which
// each caller names in its own words (jjStep.command, readJJAliases).
func runChild(next string, env, args []string) (stdout, stderr string, code int, err error) {
	var out, errOut bytes.Buffer
	run := func(path string, argv []string) error {
		out.Reset()
		errOut.Reset()
		cmd := &exec.Cmd{Path: path, Args: argv, Env: env, Stdout: &out, Stderr: &errOut}
		return cmd.Run()
	}
	err = run(next, append([]string{next}, args...))
	if errors.Is(err, syscall.ENOEXEC) {
		err = run("/bin/sh", append([]string{"/bin/sh", next}, args...))
	}
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		code = exit.ExitCode()
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			code = 128 + int(status.Signal())
		}
		return out.String(), errOut.String(), code, nil
	case err != nil:
		return "", "", 0, err
	}
	return out.String(), errOut.String(), 0, nil
}

// jjErrorLine is the line that names the failure of a jj command that exited code, command being
// what stands for the command: the first line of output, its standard error, that starts with
// `Error: `, jj's error, which may follow a notice such as the `Creating bookmark push-<change> for
// revision <change>` of `jj git push -c`; else the first line that is not blank, or a line that
// says it printed nothing. The step names it where it cannot read a dry run, and the expansion
// where its read of the aliases fails (readJJAliases).
func jjErrorLine(output, command string, code int) string {
	first := ""
	for line := range strings.SplitSeq(output, "\n") {
		if strings.HasPrefix(line, "Error: ") {
			return strings.TrimSpace(line)
		}
		if first == "" {
			first = strings.TrimSpace(line)
		}
	}
	if first != "" {
		return first
	}
	return fmt.Sprintf("%s exited %d and printed nothing", command, code)
}

// jjEveryCommand is the first line of each warning that jj prints on every command it runs, from
// cli/src/cli_util.rs and cli/src/config.rs at jj v0.38.0, v0.45.0 and v0.46.0: an alias that has
// the name of a built-in command (`Cannot define an alias that overrides the built-in command
// '<name>'`, with a full stop in 0.45 and 0.46 and none in 0.38), an alias of an alias table that
// does not load (`Failed to load `<table>.<name>`: …`), a setting jj migrates (`Deprecated <source>
// config: …`), aliases that -R, --config or --config-file would define (`Command aliases cannot be
// loaded from -R/--repository path or --config/--config-file arguments.`), and a trunk() that does
// not resolve (`Failed to resolve `revset-aliases.trunk()`: …`), with its hint (`Hint: Use `jj
// config edit --repo` to adjust the `trunk()` alias.`).
var jjEveryCommand = regexp.MustCompile("^(?:Warning: (?:" +
	"Cannot define an alias that overrides the built-in command '" +
	"|Failed to load `(?:[a-z]+-)?aliases\\." +
	"|Deprecated [A-Za-z-]+ config: " +
	"|Command aliases cannot be loaded from -R/--repository path or --config/--config-file arguments" +
	"|Failed to resolve `revset-aliases\\.trunk\\(\\)`: )" +
	"|Hint: Use `jj config edit --repo` to adjust the `trunk\\(\\)` alias)")

// jjEveryCommandMore is a line that jj prints after the first line of such a warning
// (jjEveryCommand) as a part of it, with no heading of its own: a line of the parse error of an
// alias, which is blank or starts with a space or a digit (`  |`, `1 | foo(`), the causes jj 0.46
// prints after an alias that does not load (`Caused by:`, `1: …`), the second line of the
// warning of trunk() (`The `trunk()` alias is temporarily set to `root()`.`), and the last two
// lines of jj 0.38.0's deprecation of git.auto-local-bookmark and git.push-new-bookmarks
// (`Example: jj config set …`, `For details, see: …`, cli/src/config.rs at v0.38.0). Any other
// line after such a warning is no part of it, such as the snapshot's `Rebased 1 descendant
// commits onto updated working copy`, which jj prints right after the warning of an alias that
// has the name of a built-in command.
var jjEveryCommandMore = regexp.MustCompile("^(?:$|[ 0-9]|Caused by:|The `trunk\\(\\)` alias is temporarily set to |Example: jj config set |For details, see: )")

// jjErrorHeading is the first line of jj's error, which jj prints last, its causes and hints after
// it: `Error: `, `Config error: ` or `Internal error: ` (cli/src/command_error.rs).
var jjErrorHeading = regexp.MustCompile(`^(?:Error|Config error|Internal error): `)

// jjUnsetKey is jj's note of a key that a read names and no configuration sets, which jj prints
// and then exits 0: `No matching config key for: <key>` in jj 0.45.1-sami and 0.46.0, with no colon
// in jj 0.38.0 (cli/src/commands/config/list.rs).
var jjUnsetKey = regexp.MustCompile("^Warning: No matching config key for[: ]")

// jjSGR is an escape sequence of jj's colours, which a line carries where ui.color is always.
var jjSGR = regexp.MustCompile("\x1b\\[[0-9;]*m")

// jjOnceOnly is stderr, what a jj command of legion jj's own printed on standard error, as legion
// jj prints it: each line but the warnings jj prints on every command, each with the lines jj
// prints as its parts (jjEveryCommand, jjEveryCommandMore), and jj's note of a key no
// configuration sets (jjUnsetKey), each line read with its colours left out. The command that
// legion jj runs after its own prints those warnings itself, so each prints as often as jj alone
// prints it for that command. Only a read that names a key prints the note, which none of legion
// jj's own does; it is left out so that this filter and the pod's wrapper's
// (packages/daemon/docker/jj's once_only) hold one rule. A warning that jj prints once and then
// never again, such as `Your repo appears to have been copied …` (lib/src/secure_config.rs), is
// printed: the first command in the state that makes jj print it is the one that prints it, and
// that may be a command of legion jj's own. jj's error, which jj prints last, and every line after
// it (jjErrorHeading) are left out too: jj prints it only for a command that fails, whose failure
// the caller reports itself, readJJAliases by the line of its jjUnexpandable, and passOn, which
// filters only what a command that exited 0 printed, by the jjStepFailure that runJJ prints whole.
func jjOnceOnly(stderr string) string {
	var kept strings.Builder
	part := false
	for line := range strings.SplitAfterSeq(stderr, "\n") {
		plain := jjSGR.ReplaceAllString(strings.TrimSuffix(line, "\n"), "")
		switch {
		case jjErrorHeading.MatchString(plain):
			return kept.String()
		case jjEveryCommand.MatchString(plain):
			part = true
		case part && jjEveryCommandMore.MatchString(plain):
		case jjUnsetKey.MatchString(plain):
			part = false
		default:
			part = false
			kept.WriteString(line)
		}
	}
	return kept.String()
}
