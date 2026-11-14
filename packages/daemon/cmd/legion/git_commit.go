package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	legionclaim "github.com/sjawhar/legion/daemon/internal/claim"
)

// This file is the commit half of `legion git`: the two trailer lines, the argument list that adds
// them, and the stash that hands git a message it can no longer read. The git it mirrors is
// 2.47.3, the worker image's. git commit's options as git reads them are in
// git_commit_options.go, the message as git builds, cleans and refuses it and git's trailer step
// in git_commit_message.go, and the helper git's reads in git_reader.go. The functions each part
// names are in builtin/commit.c, sequencer.c, parse-options.c, parse-options-cb.c, trailer.c,
// strbuf.c, commit.c and pretty.c of that release.

// gitTrailer is one trailer line that `legion git` adds to a commit: `<key>: <value>`.
type gitTrailer struct{ key, value string }

// String is the line as git writes it, `<key>: <value>`, and the value of its --trailer option.
func (l gitTrailer) String() string { return l.key + ": " + l.value }

// sameAs is whether held, a line `interpret-trailers --parse` printed, is this line: the same key
// in any case, and the same value.
func (l gitTrailer) sameAs(held string) bool {
	key, value, ok := strings.Cut(held, ": ")
	return ok && strings.EqualFold(key, l.key) && value == l.value
}

// sessionIDRule is the rule a session id must meet to reach a trailer. It is the jj overlay
// writer's rule, SESSION_ID_PATH_SEGMENT (packages/envoy-client/src/dispatch-session-directory.ts),
// so an id that holds a line break gives no line.
var sessionIDRule = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// gitTrailers is the lines a commit gets, in order, each only where its value is valid.
//
// Omp-Session holds the session id that the dispatch command acts as (runDispatchCli,
// packages/envoy-client/src/dispatch-cli.ts). An empty variable counts as unset. The host is
// DISPATCH_HOST, or claude when DISPATCH_HOST is unset and CLAUDE_CODE_CHILD_SESSION=1. Host omp
// takes DISPATCH_SESSION_ID. Host claude takes ENVOY_SESSION_ID, else CLAUDE_CODE_SESSION_ID. Any
// other host, or no host, gives no line.
//
// Legion-Role holds LEGION_ROLE when it is one of the roles a claim can be on, or the
// controller's role.
func gitTrailers(env map[string]string) []gitTrailer {
	var lines []gitTrailer
	host := env["DISPATCH_HOST"]
	if host == "" && env["CLAUDE_CODE_CHILD_SESSION"] == "1" {
		host = "claude"
	}
	id := ""
	switch host {
	case "omp":
		id = env["DISPATCH_SESSION_ID"]
	case "claude":
		if id = env["ENVOY_SESSION_ID"]; id == "" {
			id = env["CLAUDE_CODE_SESSION_ID"]
		}
	}
	if sessionIDRule.MatchString(id) {
		lines = append(lines, gitTrailer{"Omp-Session", id})
	}
	if role := legionclaim.Role(env["LEGION_ROLE"]); legionclaim.IsRole(role) || role == legionclaim.RoleController {
		lines = append(lines, gitTrailer{"Legion-Role", string(role)})
	}
	return lines
}

// gitCommit is the argument list that `legion git` hands git for a commit, and the stash, if any,
// that it hands git in place of a message or template git could no longer read (commitStash).
// keep is a stash whose own descriptor that list names: it must stay open until the exec
// (execGit). stdin is a stash that is to be git's standard input: it goes onto descriptor 0 before
// the exec (asStandardInput). At most one of the two is set.
type gitCommit struct {
	args  []string
	keep  *os.File
	stdin *os.File
}

// commitWithTrailers is the argument list for `git <globals> commit <args>`, read with git, the
// helper, whose globals are the caller's global options. route is the way a stash reaches the git
// that runs the commit (stashRoute). It adds a `--trailer '<key>: <value>'` for each line of
// gitTrailers that the message lacks, and only when it has shown that git alone would accept the
// message and that git's trailer step would change nothing but add those lines
// (commitBuild.lines). It puts them after every option of the caller: right before the `--` or
// `--end-of-options` that ends the options, or at the end when no word ends them. git applies
// --trailer options in order, and its default (trailer.ifExists addIfDifferentNeighbor) compares
// each with the line before it, so a line put before the caller's own would change which of the
// caller's lines git keeps. git commit reads an option after a path (parse_options with no
// PARSE_OPT_STOP_AT_NON_OPTION), so the end is a place for an option. It sets no trailer.*
// configuration, so each trailer of the caller's keeps git's behaviour, a repeated one too.
//
// It adds none and leaves every argument as written when no line is valid, when -h or --help is
// among the options, when an option matches no option of git commit, when git would make no
// commit (a dry run), when the helper's version is below 2.32 (the first git with
// `commit --trailer`), and when it cannot show those two things. A message or template that it has
// read and that git can no longer read goes to git as a file of its own, trailers or not
// (commitStash). The stash says whether that file goes onto descriptor 0 or stays open on its own
// descriptor, and which path, if any, takes the place of the value in the argument list. The error
// is a stash it could not write, and then it hands git nothing.
func commitWithTrailers(git gitReader, route stashRoute, args []string, env map[string]string) (gitCommit, error) {
	written := gitCommit{args: slices.Concat(git.globals, []string{"commit"}, args)}
	want := gitTrailers(env)
	if len(want) == 0 {
		return written, nil
	}
	opts, ok := readCommitOptions(args)
	if !ok || opts.help || opts.dryRun || opts.statusFormat || opts.null {
		return written, nil
	}
	plan, ok := opts.plan()
	if !ok {
		return written, nil
	}
	if !git.atLeast(2, 32) {
		return written, nil
	}
	b := commitBuild{git: git, route: route, opts: opts, plan: plan}
	lines, stash, err := b.lines(want)
	if err != nil {
		return gitCommit{}, err
	}
	commit := gitCommit{args: slices.Clone(args)}
	if stash != nil {
		if stash.gitPath != "" {
			word := commit.args[stash.arg]
			commit.args[stash.arg] = word[:stash.at] + stash.gitPath
		}
		if stash.stdin {
			commit.stdin = stash.file
		} else {
			commit.keep = stash.file
		}
	}
	added := make([]string, 0, 2*len(lines))
	for _, line := range lines {
		added = append(added, "--trailer", line.String())
	}
	commit.args = slices.Concat(git.globals, []string{"commit"}, commit.args[:opts.end], added, commit.args[opts.end:])
	return commit, nil
}

// stashRoute is the way a stash (commitStash) of a source that is not on descriptor 0 reaches the
// git that runs the commit. runGit picks it. A source on descriptor 0 goes to git on descriptor 0
// on every route.
type stashRoute int

const (
	// stashOnStandardInput hands git the stash on descriptor 0, with /dev/stdin in place of the
	// value. It is the route of `legion git --after`.
	stashOnStandardInput stashRoute = iota
	// stashThroughDescriptor hands git the stash on its own descriptor, with /dev/fd/<that
	// descriptor> in place of the value. It is the route of `legion git --real` with a git that is
	// no script.
	stashThroughDescriptor
	// stashNone makes no stash: the wrapper leaves the source unread for git and adds no line. It
	// is the route of `legion git --real` with a script.
	stashNone
)

// commitStash is a message or template that the wrapper read, that git reads once, and that git
// can no longer read: standard input for -F -, or a -F path, or a -t path that template_untouched
// alone reads, that is no regular file (a FIFO, a process substitution) or that names a descriptor
// (namesADescriptor). It is in file, an unlinked regular file under TMPDIR, at offset 0
// (stashSource). stdin is whether file goes to git on descriptor 0. gitPath is the path that git
// gets in place of the value at args[arg][at:], or "" when the argument stays as written.
//
// Standard input, and a path that names descriptor 0, go to git on descriptor 0 on every route
// (stashRoute): the file goes onto descriptor 0 before the exec (asStandardInput), and the
// argument stays as written: `-F -` in every spelling, or the path. Descriptors 0 to 2 reach git
// through any host wrapper, a script too. A wrapper that closes the ones above 2 (Python's
// subprocess, Node's spawn, sudo) leaves them open.
//
// For a path that names descriptor 0, the file gives git the text that git alone reads, on Linux
// and on macOS. The wrapper reads the path as git alone opens it. On Linux that open opens the
// source on descriptor 0 again, a regular file at offset 0. On macOS it is a dup of descriptor 0
// that shares its offset (fd(4)). So the wrapper's read starts where git's read would start, and it
// reads what git would read. Then git's own open of the path reads the file from its start. On
// Linux that open opens the file again at offset 0. On macOS it shares the offset of descriptor 0,
// now the file's offset, which is 0.
//
// Any other source goes to git as the route says. On stashOnStandardInput, the route of
// `legion git --after`, it goes to git on descriptor 0 too, and git gets /dev/stdin in place of
// the value. So a host's wrapper that closes every descriptor above 2 still hands git the text.
// git's open of /dev/stdin reads the text from its start, on Linux and on macOS, because
// descriptor 0 holds the file, a regular file at offset 0. On Linux that open opens the file
// again at offset 0. On macOS it shares the offset of descriptor 0, which is 0. Under --after with
// -e, the editor that git starts gets the file as its standard input, as it gets the file of -F -.
//
// On stashThroughDescriptor, the route of `legion git --real` with a git that is no script, the
// source keeps its descriptor open across the exec, and git gets /dev/fd/<that descriptor> in
// place of the value. The descriptor is the one the open returned, the lowest free one, so it
// takes the place of none that the caller passed. On stashNone, the route of `legion git --real`
// with a script, the wrapper leaves such a source unread and adds no line (commitBuild.readFile):
// a script may close every descriptor above 2 before it runs git.
type commitStash struct {
	file    *os.File
	stdin   bool
	gitPath string
	arg     int
	at      int
}

// stashSource keeps a source that git can read once in a new stash for the file option f
// (commitStash), and answers the text it read. It reads standard input when readStdin is true,
// and path otherwise. onStandardInput is whether the source is on descriptor 0: standard input, or
// a path that names descriptor 0. route is the way the stash of any other source reaches git
// (stashRoute), and readFile makes no such stash on stashNone. It makes the file before it reads,
// so a source that it cannot keep is left unread for git (readDeclined). A read that fails gives
// readFailed, as git's own read fails.
//
// After it writes the text, it rewinds the file to offset 0, so git reads the text from its start
// through the descriptor it gets. On Linux git's open of /dev/fd/<n> or /dev/stdin opens the file
// again at offset 0. On macOS that open shares the descriptor's offset (fd(4)), which the rewind
// puts at 0. A stash that git gets through /dev/fd/<n> has its close-on-exec flag cleared, so its
// descriptor stays open across the exec. The error is a write, a rewind or a flag change that
// failed after the read, and then it returns no stash.
func stashSource(f commitFile, path string, readStdin, onStandardInput bool, route stashRoute) (string, commitReadStatus, *commitStash, error) {
	file, err := os.CreateTemp("", "legion-git-message-*")
	if err != nil {
		return "", readDeclined, nil, nil
	}
	if err := os.Remove(file.Name()); err != nil {
		file.Close()
		return "", readDeclined, nil, nil
	}
	var read []byte
	if readStdin {
		read, err = io.ReadAll(os.Stdin)
	} else {
		read, err = os.ReadFile(path)
	}
	if err != nil {
		file.Close()
		return "", readFailed, nil, nil
	}
	if _, err := file.Write(read); err != nil {
		file.Close()
		return "", readDeclined, nil, fmt.Errorf("keep the text git can no longer read: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		file.Close()
		return "", readDeclined, nil, fmt.Errorf("rewind the text kept for git: %w", err)
	}
	stash := &commitStash{file: file, stdin: true, arg: f.arg, at: f.at}
	switch {
	case onStandardInput:
	case route == stashOnStandardInput:
		stash.gitPath = "/dev/stdin"
	default: // stashThroughDescriptor: readFile makes no such stash on stashNone.
		if _, err := unix.FcntlInt(file.Fd(), unix.F_SETFD, 0); err != nil {
			file.Close()
			return "", readDeclined, nil, fmt.Errorf("keep the text git can no longer read open for git: %w", err)
		}
		stash.stdin, stash.gitPath = false, "/dev/fd/"+strconv.Itoa(int(file.Fd()))
	}
	return string(read), readDone, stash, nil
}

// namesADescriptor is whether path, a -F or -t value as git opens it, names a descriptor of the
// process that opens it: /dev/stdin, /dev/stdout, /dev/stderr, or a path under /dev/fd/ or
// /proc/self/fd/. standardInput is whether that descriptor is 0: /dev/stdin, /dev/fd/0 or
// /proc/self/fd/0. It reads path cleaned, each symbolic link it leads to in turn, and path with
// every link resolved (filepath.EvalSymlinks), where each can be read, and the first of them that
// names a descriptor gives both answers. On macOS an open of such a path shares the descriptor's
// offset (fd(4)), so the wrapper's read of a regular file there would leave git's own read at its
// end. So the wrapper reads such a path once, as it reads a source that is no regular file
// (readFile), on every system.
func namesADescriptor(path string) (names, standardInput bool) {
	named := func(path string) (bool, bool) {
		path = filepath.Clean(path)
		zero := path == "/dev/stdin" || path == "/dev/fd/0" || path == "/proc/self/fd/0"
		return zero || path == "/dev/stdout" || path == "/dev/stderr" ||
			strings.HasPrefix(path, "/dev/fd/") || strings.HasPrefix(path, "/proc/self/fd/"), zero
	}
	// 40 is the number of links Linux follows in one path before it fails with ELOOP.
	link := path
	for range 40 {
		if names, zero := named(link); names {
			return true, zero
		}
		target, err := os.Readlink(link)
		if err != nil {
			break
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(link), target)
		}
		link = target
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false, false
	}
	return named(resolved)
}

// asStandardInput makes file, a stash that goes to git on descriptor 0 (commitStash), descriptor 0
// of this process, so git reads it as its standard input. A dup onto 0 leaves the new descriptor
// open across the exec, and file's own descriptor closes there.
func asStandardInput(file *os.File) error {
	if err := dupOnto(int(file.Fd()), 0); err != nil {
		return fmt.Errorf("hand git the standard input kept for it: %w", err)
	}
	return nil
}
