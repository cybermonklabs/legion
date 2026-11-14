package main

import (
	"fmt"
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

// gitCommit is the argument list that `legion git` hands git for a commit, keep, the file whose
// descriptor that list names in place of a message or template git could no longer read, and
// stdin, the file that is to be git's standard input in place of the one the wrapper read
// (commitStash). keep must stay open until the exec, and stdin goes onto descriptor 0 before it
// (asStandardInput).
type gitCommit struct {
	args  []string
	keep  *os.File
	stdin *os.File
}

// commitWithTrailers is the argument list for `git <globals> commit <args>`, read with git, the
// helper, whose globals are the caller's global options. script is whether the git that runs the
// commit is a script. It adds a `--trailer '<key>: <value>'` for each line of gitTrailers that the
// message lacks, and only when it has shown that git alone would accept the message and that git's
// trailer step would change nothing but add those lines (commitBuild.lines). It puts them after
// every option of the caller: right before the `--` or `--end-of-options` that ends the options,
// or at the end when no word ends them. git applies --trailer options in order, and its default
// (trailer.ifExists addIfDifferentNeighbor) compares each with the line before it, so a line put
// before the caller's own would change which of the caller's lines git keeps. git commit reads an
// option after a path (parse_options with no PARSE_OPT_STOP_AT_NON_OPTION), so the end is a
// place for an option. It sets no trailer.* configuration, so each trailer of the caller's keeps
// git's behaviour, a repeated one too.
//
// It adds none and leaves every argument as written when no line is valid, when -h or --help is
// among the options, when an option matches no option of git commit, when git would make no
// commit (a dry run), when the helper's version is below 2.32 (the first git with
// `commit --trailer`), and when it cannot show those two things. A message or template that it has
// read and that git can no longer read goes to git as a file of its own, trailers or not
// (commitStash). The error is a stash it could not write, and then it hands git nothing.
func commitWithTrailers(git gitReader, script bool, args []string, env map[string]string) (gitCommit, error) {
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
	b := commitBuild{git: git, script: script, opts: opts, plan: plan}
	lines, stash, err := b.lines(want)
	if err != nil {
		return gitCommit{}, err
	}
	commit := gitCommit{args: slices.Clone(args)}
	switch {
	case stash == nil:
	case stash.stdin:
		commit.stdin = stash.file
	default:
		word := commit.args[stash.arg]
		commit.args[stash.arg] = word[:stash.at] + "/dev/fd/" + strconv.Itoa(int(stash.file.Fd()))
		commit.keep = stash.file
	}
	added := make([]string, 0, 2*len(lines))
	for _, line := range lines {
		added = append(added, "--trailer", line.String())
	}
	commit.args = slices.Concat(git.globals, []string{"commit"}, commit.args[:opts.end], added, commit.args[opts.end:])
	return commit, nil
}

// commitStash is a message or template that the wrapper read, that git reads once, and that git
// can no longer read: standard input for -F -, or a -F path, or a -t path that template_untouched
// alone reads, that is no regular file (a FIFO, a process substitution) or that names a descriptor
// (namesADescriptor). It is in file, an unlinked regular file under TMPDIR, at offset 0: git reads
// it from its start through the descriptor it gets, also on macOS, where git's open of
// /dev/fd/<n> shares the descriptor's offset (fd(4)).
//
// Standard input (stdin) becomes git's standard input again: the file goes onto descriptor 0
// before the exec (asStandardInput), and `-F -` stays as written in every spelling. Descriptors 0
// to 2 reach git through any host wrapper, and a wrapper that closes the ones above 2 (Python's
// subprocess, Node's spawn, sudo) leaves them open.
//
// Any other source keeps its descriptor open across the exec, and git gets /dev/fd/<that
// descriptor> in place of the value at args[arg][at:]. The descriptor is the one the open
// returned, the lowest free one, so it takes the place of none that the caller passed. Only a git
// that is no script gets such a stash: a host's wrapper may close every descriptor above 2, so for
// a script the wrapper leaves such a source unread and adds no line (commitBuild.readFile).
type commitStash struct {
	file  *os.File
	stdin bool
	arg   int
	at    int
}

// namesADescriptor is whether path, a -F or -t value as git opens it, names a descriptor of the
// process that opens it: /dev/stdin, /dev/stdout, /dev/stderr, or a path under /dev/fd/ or
// /proc/self/fd/. It reads path cleaned, each symbolic link it leads to in turn, and path with
// every link resolved (filepath.EvalSymlinks), where each can be read. On macOS an open of such a
// path shares the descriptor's offset (fd(4)), so the wrapper's read of a regular file there would
// leave git's own read at its end. So the wrapper reads such a path once, as it reads a source that
// is no regular file (readFile), on every system.
func namesADescriptor(path string) bool {
	names := func(path string) bool {
		path = filepath.Clean(path)
		return path == "/dev/stdin" || path == "/dev/stdout" || path == "/dev/stderr" ||
			strings.HasPrefix(path, "/dev/fd/") || strings.HasPrefix(path, "/proc/self/fd/")
	}
	// 40 is the number of links Linux follows in one path before it fails with ELOOP.
	link := path
	for range 40 {
		if names(link) {
			return true
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
	return err == nil && names(resolved)
}

// asStandardInput makes file, a stash of standard input (commitStash), descriptor 0 of this
// process, so git reads it as the standard input it was. A dup onto 0 leaves the new descriptor
// open across the exec, and file's own descriptor closes there. A file that is already descriptor
// 0 (this process started with none) has its close-on-exec flag cleared.
func asStandardInput(file *os.File) error {
	fd := int(file.Fd())
	if fd == 0 {
		_, err := unix.FcntlInt(0, unix.F_SETFD, 0)
		return err
	}
	if err := dupOnto(fd, 0); err != nil {
		return fmt.Errorf("hand git the standard input kept for it: %w", err)
	}
	return nil
}
