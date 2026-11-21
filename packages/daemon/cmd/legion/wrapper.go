package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/sjawhar/legion/daemon/internal/lookpath"
)

// This file is what `legion git` (git.go) and `legion jj` (jj.go) share: the two forms of their
// arguments and the signals at their start (startWrapper), the program each runs and its loop
// guard (pickAfter, wrapperChain), and the exec that ends each (execWrapped).

// startWrapper is the start of `legion <name>`, the wrapper command of the program name (git, jj),
// whose help is usage: it answers whether the command runs on, and the status it ends with where
// it does not. Help asked for in the first argument prints usage and ends it with 0. Arguments of
// neither form, `--real <name> -- …` nor `--after <directory> -- …`, print what to give and usage
// and end it with 2.
//
// main's signal.NotifyContext catches SIGINT and SIGTERM for the whole process. A caught signal
// would not stop a read that waits (`git commit -F -` from a pipe that stays open, a FIFO with no
// writer), and a cancelled commit or push would still be made. So startWrapper puts back the
// actions those two had at the process start (signal.Reset): the Go runtime's own, which ends the
// process as the signal's default action does, or for SIGINT an ignore that the caller set. So an
// interrupt or a terminate ends the command where it stands. A signal that came before that ends
// the command with the status a shell reads a death by that signal as (endedBySignal), before it
// reads anything or runs anything.
//
// This has a limit. The Go runtime puts its own handler over an ignore that the caller set for
// each signal it handles, except SIGHUP and SIGINT (sigInstallGoHandler, runtime/signal_unix.go).
// It does not handle SIGTSTP, SIGTTIN or SIGTTOU (_SigDefault, runtime/sigtab_linux_generic.go).
// The exec sets each handled signal back to its default action. So where the caller ignores a
// signal that the runtime handles, such as SIGTERM, SIGPIPE or SIGQUIT, the program alone lives
// through that signal, but the wrapper command and the program it runs die of it. An ignored
// SIGHUP, SIGINT, SIGTSTP, SIGTTIN or SIGTTOU stays ignored.
func startWrapper(ctx context.Context, name, usage string, args []string, stderr io.Writer) (status int, runs bool) {
	if len(args) > 0 && helpRequested(args[0]) {
		fmt.Fprint(stderr, usage)
		return 0, false
	}
	if len(args) < 3 || (args[0] != "--real" && args[0] != "--after") || args[1] == "" || args[2] != "--" {
		fmt.Fprintf(stderr, "legion %s: give --real <%s> or --after <directory>, then --, then the %s arguments\n%s", name, name, name, usage)
		return 2, false
	}
	signal.Reset(os.Interrupt, syscall.SIGTERM)
	if status, ok := signalled(ctx); ok {
		return status, false
	}
	return 0, true
}

// wrapperChain is a loop guard's value (wrappedGitAt, wrappedJJAt): reentries, the number of runs
// in the chain that came back, and ran, the path of each program the chain ran, in order.
type wrapperChain struct {
	reentries int
	ran       []string
}

// String is the chain as the variable holds it: `<reentries>:<path>:<path>…`.
func (c wrapperChain) String() string {
	return strconv.Itoa(c.reentries) + ":" + strings.Join(c.ran, ":")
}

// parseWrapperChain reads a loop guard's value, and answers what os.Stat says of each listed path
// that is there. A value of another form is no chain.
func parseWrapperChain(value string) (chain wrapperChain, listed []os.FileInfo) {
	count, rest, ok := strings.Cut(value, ":")
	n, err := strconv.ParseUint(count, 10, 31)
	if !ok || err != nil {
		return wrapperChain{}, nil
	}
	chain.reentries = int(n)
	for _, path := range strings.Split(rest, ":") {
		if path == "" {
			continue
		}
		chain.ran = append(chain.ran, path)
		if info, err := os.Stat(path); err == nil {
			listed = append(listed, info)
		}
	}
	return chain, listed
}

// withVariable is env with name set to value, in place of any value it held. It changes env's own
// array, so a caller that keeps env gives it a copy (slices.Clone).
func withVariable(env []string, name, value string) []string {
	env = slices.DeleteFunc(env, func(entry string) bool {
		return strings.HasPrefix(entry, name+"=")
	})
	return append(env, name+"="+value)
}

// wrapperPick is the program that `legion git --after` or `legion jj --after` runs, and what it
// knows of the programs of that name on PATH.
type wrapperPick struct {
	// run is the program it replaces itself with, and chain the loop guard's value run gets.
	run   string
	chain wrapperChain
	// reentry is whether this run came back through a program that an earlier run of the chain
	// chose, a host's wrapper among them. `legion git` adds nothing to such a run's commit: the
	// first run of the chain added its trailers. `legion jj` does not read it for its step, since a
	// jj that a jj command starts is a re-entry too (wrappedJJStepped decides that step).
	reentry bool
	// after is each program of the name on PATH after the first entry that is the wrapper's own
	// directory, in PATH order: the ones it may run, and the ones the helper is chosen from.
	after []wrapperCandidate
}

// wrapperCandidate is a program that pickAfter found on PATH, and script, whether it is a script
// (isScript), which pickAfter reads once.
type wrapperCandidate struct {
	lookpath.Found
	script bool
}

// pickAfter chooses the program named name for `legion <name> --after own`, from path (PATH),
// marker (the loop guard's variable) and the kind of each candidate after own, which it reads once
// (isScript), and nothing else. A candidate is each program of the name on path outside own
// (lookpath.Others). It runs the first candidate after the first entry that is own, or the first
// candidate when own is on no entry. A script is a candidate too, so a host's own wrapper still
// runs.
//
// A marker that lists the same file as a candidate is a re-entry, one more than the chain counted.
// It runs the first candidate after own that the marker does not list, and the helper when the
// marker lists every one. When the count passes the number of candidates, or the marker lists
// every candidate and there is no helper, the programs on PATH have run one another and come back:
// the chain is a loop, and pickAfter refuses it. A marker that lists no candidate is stale: the run
// starts a new chain. Either way the chain's value for the program it runs lists that program too.
// The error is the loop, or no program of the name after own on a run that is no re-entry.
func pickAfter(name, own, path, marker string) (wrapperPick, error) {
	found, ownAt := lookpath.Others(path, name, own)
	var pick wrapperPick
	for _, f := range found {
		if f.Index > ownAt {
			pick.after = append(pick.after, wrapperCandidate{Found: f, script: isScript(f.Path)})
		}
	}
	chain, listed := parseWrapperChain(marker)
	isListed := func(f lookpath.Found) bool {
		return slices.ContainsFunc(listed, func(info os.FileInfo) bool { return os.SameFile(info, f.Info) })
	}
	pick.reentry = slices.ContainsFunc(found, isListed)
	switch {
	case !pick.reentry:
		if len(pick.after) == 0 {
			return wrapperPick{}, fmt.Errorf("no %s on PATH after %s", name, own)
		}
		chain = wrapperChain{}
		pick.run = pick.after[0].Path
	case chain.reentries < len(found):
		chain.reentries++
		if i := slices.IndexFunc(pick.after, func(c wrapperCandidate) bool { return !isListed(c.Found) }); i >= 0 {
			pick.run = pick.after[i].Path
		} else {
			pick.run = pick.helper()
		}
	}
	if pick.run == "" {
		return wrapperPick{}, fmt.Errorf("the %s programs on PATH after %s run one another in a loop", name, own)
	}
	chain.ran = append(chain.ran, pick.run)
	pick.chain = chain
	return pick, nil
}

// helper is the first program after own that is no script: the run of a re-entry whose marker
// lists every candidate, and for `legion git` the git that answers a commit's reads, so a host's
// script never answers them. It is "" when there is none: such a re-entry is a loop (pickAfter),
// and `legion git` adds no trailer.
func (p wrapperPick) helper() string {
	for _, c := range p.after {
		if !c.script {
			return c.Path
		}
	}
	return ""
}

// isScript is whether path is no compiled program (lookpath.Kind): a script with a `#!` line,
// or a file with neither that line nor a compiled program's magic number, which the kernel refuses
// and a shell runs with /bin/sh. Either one is a host's wrapper, not the real git or jj. A file
// this process cannot read counts as a script, since nothing shows that it is a real program.
func isScript(path string) bool {
	_, compiled, err := lookpath.Kind(path)
	return err != nil || !compiled
}

// execWrapped replaces this process, the wrapper command (`legion git`, `legion jj`), with the
// program at path, argv[0] being path. keep is the file whose descriptor args name in place of a
// message git could no longer read (commitStash): it stays open across the exec, since its
// close-on-exec flag is clear. A file the kernel refuses to run (ENOEXEC), a script with no `#!`
// line, runs as execvp runs it: /bin/sh with argv `/bin/sh <path> <args…>` (glibc's
// maybe_script_execute). execWrapped returns only when the exec fails, having said so in command's
// name: 127 when path does not exist, as a shell answers, and 126 otherwise.
func execWrapped(command, path string, args, env []string, keep *os.File, stderr io.Writer) int {
	err := syscall.Exec(path, append([]string{path}, args...), env)
	if errors.Is(err, syscall.ENOEXEC) {
		err = syscall.Exec("/bin/sh", append([]string{"/bin/sh", path}, args...), env)
	}
	runtime.KeepAlive(keep)
	fmt.Fprintf(stderr, "%s: run %s: %v\n", command, path, err)
	if errors.Is(err, syscall.ENOENT) {
		return 127
	}
	return 126
}
