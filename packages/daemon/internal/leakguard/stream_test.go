package leakguard

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// aPush is a push of a repository's one commit to a ref its destination does not hold.
type aPush struct {
	t   *testing.T
	req PushRequest
}

// through is the guard's answer to the push, under the test's bound.
func (p aPush) through(guard *Guard) error {
	return guard.Push(bound(p.t), p.req)
}

// aPushOf is a repository whose one commit holds files (path to content), and its push.
func aPushOf(t *testing.T, files map[string]string) aPush {
	t.Helper()
	dir := t.TempDir()
	fixture := fixturegit.Isolated(t, t.TempDir())
	fixture.Run(dir, fixturegit.InitWorkTree("", "main")...)
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fixture.Run(dir, "git", "add", "-A")
	fixture.Run(dir, "git", "commit", "--quiet", "-m", "a commit of the test's files")
	head := fixture.Run(dir, "git", "rev-parse", "HEAD")
	return aPush{t: t, req: PushRequest{
		Dir:     dir,
		Remote:  "origin",
		URL:     "https://github.com/invented-org/invented-repo.git",
		Updates: []Update{{LocalRef: "refs/heads/main", LocalSHA: head, RemoteRef: "refs/heads/main", RemoteSHA: noSHA}},
	}}
}

// gitFirst is a guard whose git, first on its PATH, is script: a stand-in that may run the real
// git, as fixturegit.CompiledGit names it, by the path script holds.
func gitFirst(t *testing.T, script string) *Guard {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\n"+script), 0o700); err != nil {
		t.Fatal(err)
	}
	return guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public,
		"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// A record is scanned when it arrives, not when the pass ends: once a record that names a host is
// added, the pass's report holds its line before end is called. A pass that held its records
// and scanned them at its end would hold the whole push.
func TestTheRulePassScansARecordAsItArrives(t *testing.T) {
	guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
	pass, err := guard.rulePass(bound(t), "the push")
	if err != nil {
		t.Fatal(err)
	}
	if err := pass.add(Record{Label: "notes.md", Text: "the runner answers at " + theHost}); err != nil {
		t.Fatal(err)
	}
	want := []string{"notes.md: " + theClass}
	if !slices.Equal(pass.report.lines, want) {
		t.Fatalf("after the record was added the report is %q, want %q", pass.report.lines, want)
	}
	if err := pass.add(Record{Label: "notes.md", Text: "a line of nothing in particular"}); err != nil {
		t.Fatal(err)
	}
	if got := refusal(t, pass.end("the push")).Lines; !slices.Equal(got, want) {
		t.Fatalf("the refusal is %q, want %q", got, want)
	}
}

// The rule pass reads a push's diff as git writes it and scans each line as it arrives. The
// stand-in git writes the diff's first added line, which names a host, and then waits for a file
// that the test makes once the pass's report holds that line; only then does it write the second
// line and exit. A read that waited for the whole diff, or a pass that scanned at its end, would
// leave the file unmade, and the stand-in gives up after 10 s and exits 7.
func TestTheRulePassReadsTheDiffAsGitWritesIt(t *testing.T) {
	seen := filepath.Join(t.TempDir(), "the first line was scanned")
	guard := gitFirst(t, "printf 'diff --git a/f.md b/f.md\\n--- /dev/null\\n+++ b/f.md\\n@@ -0,0 +1,2 @@\\n"+
		"+the runner answers at "+theHost+"\\n'\n"+
		"i=0\nwhile [ ! -e '"+seen+"' ]; do\n  i=$((i+1))\n  [ \"$i\" -gt 100 ] && exit 7\n  sleep 0.1\ndone\n"+
		"printf '+a second line of nothing\\n'\n")
	pass, err := guard.rulePass(bound(t), "the push")
	if err != nil {
		t.Fatal(err)
	}
	made := false
	emit := func(record Record) error {
		if err := pass.add(record); err != nil {
			return err
		}
		if len(pass.report.lines) > 0 && !made {
			made = true
			return os.WriteFile(seen, nil, 0o600)
		}
		return nil
	}
	if err := guard.addedLines(bound(t), t.TempDir(), "HEAD", 1, emit); err != nil {
		t.Fatalf("the read of the diff: %v", err)
	}
	want := []string{"f.md: " + theClass}
	if got := refusal(t, pass.end("the push")).Lines; !slices.Equal(got, want) {
		t.Fatalf("the refusal is %q, want %q", got, want)
	}
}

// Every added line of a push is read whole as git writes it, a line longer than the reader's
// buffer included: a match at the end of a line of 192 KiB is reported, and so is each match among
// many short lines, one line of the report for each, in the order git wrote them.
func TestEveryAddedLineIsReadWholeAsGitWritesIt(t *testing.T) {
	var many strings.Builder
	for i := range 2000 {
		if i%500 == 7 {
			fmt.Fprintf(&many, "line %d names %s\n", i, theHost)
			continue
		}
		fmt.Fprintf(&many, "line %d of a file of many lines\n", i)
	}
	long := strings.Repeat("a long line of nothing ", 192<<10/23) + theHost
	push := aPushOf(t, map[string]string{"many.md": many.String(), "long.md": "before\n" + long + "\nafter\n"})
	err := push.through(guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public))
	want := append([]string{"long.md: " + theClass}, slices.Repeat([]string{"many.md: " + theClass}, 4)...)
	if got := refusal(t, err).Lines; !slices.Equal(got, want) {
		t.Fatalf("the report is %q, want %q", got, want)
	}
}

// holdUntilEnd is a shell loop that runs while a file the test made is there and the test binary
// is alive, and the test removes that file when it ends: a process that outlives the git that
// started it, for as long as the test runs, and goes once it ends. The loop waits on the file's
// presence, not on a file's arrival, so either removal ends it: the cleanup's, or the RemoveAll of
// the test's temporary directory, which a cleanup registered earlier runs after it. A run that
// ends with no cleanups at all, a timeout or an interrupt, ends it through the test binary's
// exit, which `kill -0` of the binary's process id sees.
func holdUntilEnd(t *testing.T) string {
	t.Helper()
	held := filepath.Join(t.TempDir(), "held")
	if err := os.WriteFile(held, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(held) })
	return "while [ -e '" + held + "' ] && kill -0 " + strconv.Itoa(os.Getpid()) + " 2>/dev/null; do sleep 0.1; done"
}

// No holder outlives its test: five loops of holdUntilEnd, each started by a subtest, have all
// ended before the test binary's deadline (testwait.Receive). The loops are this test's own
// children, so it waits for them, and kills every one still running when it fails.
func TestNoHolderOutlivesItsTest(t *testing.T) {
	var loops []*exec.Cmd
	t.Cleanup(func() {
		for _, loop := range loops {
			_ = loop.Process.Kill()
		}
	})
	for i := range 5 {
		t.Run(fmt.Sprintf("holder %d", i), func(t *testing.T) {
			loop := exec.Command("sh", "-c", holdUntilEnd(t))
			if err := loop.Start(); err != nil {
				t.Fatal(err)
			}
			loops = append(loops, loop)
		})
	}
	ended := make(chan struct{})
	go func() {
		for _, loop := range loops {
			_ = loop.Wait()
		}
		close(ended)
	}()
	testwait.Receive(t, "the holders to end with their subtests", ended)
}

// waits records, through a guard's onWait seam (record), what Wait answered for the diff's git
// (`--unified=0`): diff is that answer, and saw is whether the diff's git ran.
type waits struct {
	diff error
	saw  bool
}

func (w *waits) record(argv []string, err error) {
	if slices.Contains(argv, "--unified=0") {
		w.diff, w.saw = err, true
	}
}

// The read's end after git exits is chosen from what Wait answered: one WaitDelay where it
// answered nil, so a process git started that holds the pipe is waited for as exec waits for
// output it copies itself, and none where it answered an error. An error is either that wait run
// out already, exec.ErrWaitDelay for a git that exited 0, which holds a process holding git's
// standard error too, or a git that failed, *exec.ExitError, whatever exec waited, which the
// guard refuses whatever its output.
func TestTheReadEndsAtOnceAfterAnErrorOfWait(t *testing.T) {
	const exitDelay = 7 * time.Second
	failed := exec.Command("sh", "-c", "exit 3").Run()
	var exitErr *exec.ExitError
	if !errors.As(failed, &exitErr) {
		t.Fatalf("the failing command answered %v, want an *exec.ExitError", failed)
	}
	for _, tc := range []struct {
		name    string
		waitErr error
		want    time.Duration
	}{
		{name: "git exited 0", waitErr: nil, want: exitDelay},
		{name: "exec's wait for the standard error ran out", waitErr: exec.ErrWaitDelay, want: 0},
		{name: "git failed", waitErr: exitErr, want: 0},
	} {
		if got := readEndDelay(tc.waitErr, exitDelay); got != tc.want {
			t.Errorf("%s: the read ends %s after Wait, want %s", tc.name, got, tc.want)
		}
	}
}

// A push with no deadline at all, as git's pre-push hook runs one, is answered though a process
// git started holds git's standard output, the diff's pipe, until the test ends: Wait answers nil,
// as git exited 0 and exec copies nothing the process holds, and the read ends one WaitDelay
// after it, the wait exec gives such a process when it copies a child's output itself. With no
// deadline nothing stops git's process group, so the process need not leave it, and the test
// needs nothing a system may lack. The answer is the one a push with no such process gets.
func TestAReadEndsAfterGitExitsThoughAProcessHoldsItsOutput(t *testing.T) {
	push := aPushOf(t, map[string]string{"notes.md": "a line of nothing\nthe runner answers at " + theHost + "\n"})
	want := refusal(t, push.through(guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public))).Lines
	git := fixturegit.CompiledGit(t)
	guard := gitFirst(t, "case \" $* \" in\n"+
		"*\" --unified=0 \"*) ("+holdUntilEnd(t)+") 2>/dev/null &\n  exec '"+git+"' \"$@\" ;;\n"+
		"*) exec '"+git+"' \"$@\" ;;\nesac\n")
	var seen waits
	guard.onWait = seen.record
	var err error
	answered(t, "the push whose diff a process that outlived git holds", func() { err = guard.Push(context.Background(), push.req) })
	if got := refusal(t, err).Lines; !slices.Equal(got, want) {
		t.Fatalf("the report is %q, want the one with no holding process, %q", got, want)
	}
	if !seen.saw || seen.diff != nil {
		t.Errorf("the diff's Wait answered %v (ran: %t), want nil", seen.diff, seen.saw)
	}
}

// Where a process git started holds both git's outputs until the test ends, exec waits one
// WaitDelay for the standard error it copies, and Wait answers an error: exec.ErrWaitDelay for a
// git that exited 0, which the guard reads as that exit and answers as a push with no such
// process, and git's *exec.ExitError for a git that failed, which reaches the refusal. Either way
// the read ends at once (TestTheReadEndsAtOnceAfterAnErrorOfWait), with no deadline.
func TestWaitAnswersAnErrorWhereAProcessHoldsBothOfGitsOutputs(t *testing.T) {
	git := fixturegit.CompiledGit(t)
	for _, tc := range []struct {
		name string
		// then is what the diff's stand-in does once the real diff ran.
		then  string
		check func(t *testing.T, push aPush, err error, waitErr error)
	}{
		{
			name: "git exits 0",
			then: "exit 0",
			check: func(t *testing.T, push aPush, err error, waitErr error) {
				want := refusal(t, push.through(guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public))).Lines
				if got := refusal(t, err).Lines; !slices.Equal(got, want) {
					t.Errorf("the report is %q, want the one with no holding process, %q", got, want)
				}
				if !errors.Is(waitErr, exec.ErrWaitDelay) {
					t.Errorf("the diff's Wait answered %v, want exec.ErrWaitDelay", waitErr)
				}
			},
		},
		{
			name: "git exits 3",
			then: "exit 3",
			check: func(t *testing.T, _ aPush, err error, waitErr error) {
				says(t, setupFailure(t, err), "the push's commits could not be read: git diff-tree exited 3")
				var exitErr *exec.ExitError
				if !errors.As(waitErr, &exitErr) || exitErr.ExitCode() != 3 {
					t.Errorf("the diff's Wait answered %v, want git's *exec.ExitError of status 3", waitErr)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			push := aPushOf(t, map[string]string{"notes.md": "a line of nothing\nthe runner answers at " + theHost + "\n"})
			guard := gitFirst(t, "case \" $* \" in\n"+
				"*\" --unified=0 \"*) ("+holdUntilEnd(t)+") &\n  '"+git+"' \"$@\" || exit\n  "+tc.then+" ;;\n"+
				"*) exec '"+git+"' \"$@\" ;;\nesac\n")
			var seen waits
			guard.onWait = seen.record
			var err error
			answered(t, "the push whose diff a process that outlived git holds", func() { err = guard.Push(context.Background(), push.req) })
			if !seen.saw {
				t.Fatal("the diff's git was not run")
			}
			tc.check(t, push, err, seen.diff)
		})
	}
}

// The guard closes its own copy of the pipe's write end once the child has started, so the read
// ends at the pipe's end as soon as the child exits and nothing else holds it, without waiting
// for the end after the child's exit. That end is an hour here, and the context's bound, what is
// left of the test binary's deadline (testwait.Remaining), is only a bound on the failure: a guard that kept its copy open would read until the context ended
// and answer that stop.
func TestAReadEndsAtThePipesEndOnceTheChildExits(t *testing.T) {
	guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
	guard.waitDelay = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), testwait.Remaining(t))
	defer cancel()
	var read []byte
	status, err := guard.readOutput(ctx, child{argv: []string{"sh", "-c", "printf 'a line\\n'"}}, func(r io.Reader) error {
		var err error
		read, err = io.ReadAll(r)
		return err
	})
	if err != nil || status != 0 || string(read) != "a line\n" {
		t.Fatalf("the read answered status %d, %v, and %q, want 0, no error and the child's line", status, err, read)
	}
}

// Once the child has exited and its wait has run out, a read takes what the pipe held at that
// first read and no more, though the pipe's write end is still open and more is written to it
// after: 1000 bytes of A are in the pipe when the reader is marked past the child's exit, one
// read takes some of them, 1000 bytes of B follow, and the reader answers the rest of the A and
// then the end of its output. A pipe the test owns stands for the child's output and the process
// that outlived the child, so nothing here depends on how fast either runs.
func TestAReadAfterTheChildExitedTakesWhatThePipeHeldAndNoMore(t *testing.T) {
	out, in, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	defer in.Close()
	if _, err := in.Write(bytes.Repeat([]byte("A"), 1000)); err != nil {
		t.Fatal(err)
	}
	reader := &pipeReader{ctx: context.Background(), file: out}
	reader.exited.Store(true)
	first := make([]byte, 100)
	n, err := reader.Read(first)
	if err != nil {
		t.Fatalf("the first read: %v", err)
	}
	if _, err := in.Write(bytes.Repeat([]byte("B"), 1000)); err != nil {
		t.Fatal(err)
	}
	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("the rest of the read: %v", err)
	}
	got := string(append(first[:n], rest...))
	if got != strings.Repeat("A", 1000) {
		t.Fatalf("read %d bytes (%d of them B), want the 1000 of A alone", len(got), strings.Count(got, "B"))
	}
}

// A read waiting on the pipe ends when its context ends, with os.ErrDeadlineExceeded, whatever
// the child does: here no child and no Wait, only a pipe the test owns whose write end stays open
// and holds nothing, and a reader made as readOutput makes its own (newPipeReader), so nothing but
// the context's end can end the read. A read with no such end would wait until the test closed
// the pipe; the test binary's deadline only bounds that failure (testwait.Receive).
func TestAReadEndsWhenItsContextEnds(t *testing.T) {
	out, in, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	defer in.Close()
	ctx, cancel := context.WithCancel(context.Background())
	reader, stop := newPipeReader(ctx, out)
	defer stop()
	read := make(chan error, 1)
	go func() {
		_, err := reader.Read(make([]byte, 16))
		read <- err
	}()
	cancel()
	if err := testwait.Receive(t, "the read to end when its context ended (the pipe's read deadline)", read); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("the read answered %v, want os.ErrDeadlineExceeded", err)
	}
}

// readOutput hands what its child's Wait answered to the read's end after the exit: a child that
// fails while a process it started holds its standard output makes Wait answer an
// *exec.ExitError, and the read then ends at once (readEndDelay), not after the hour the guard's
// WaitDelay is here. The holder keeps the pipe open, so the read cannot end at the pipe's end
// either. The read takes what the child wrote, and readOutput answers the child's status.
func TestReadOutputEndsAtOnceAfterAFailedChildThoughAProcessHoldsItsOutput(t *testing.T) {
	guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
	guard.waitDelay = time.Hour
	argv := []string{"sh", "-c", "(" + holdUntilEnd(t) + ") 2>/dev/null &\nprintf 'a line\\n'\nexit 3"}
	var read []byte
	var status int
	var err error
	answered(t, "the read whose output a process that outlived the child holds", func() {
		status, err = guard.readOutput(context.Background(), child{argv: argv}, func(r io.Reader) error {
			var err error
			read, err = io.ReadAll(r)
			return err
		})
	})
	if err != nil || status != 3 || string(read) != "a line\n" {
		t.Fatalf("the read answered status %d, %v, and %q, want 3, no error and the child's line", status, err, read)
	}
}

// readOutput's read ends when its context ends, though its child has exited 0 and Wait has
// answered nil, so nothing stops the child's process group and the read's end after the exit is
// the hour the guard's WaitDelay is here, and a process the child started holds the pipe open: the
// context's end (newPipeReader) is the only end left, and readOutput answers it as the stop it is.
// The child writes its own process id and exits; the reader reads that line, waits until the
// process is gone, which is once Wait has reaped it, and only then ends the context. The wait for
// the reap lasts at most a minute, and ends 5 s before answered gives up on the read 30 s before
// the test binary's deadline (testwait.Remaining), or after 2 s where less is left, so a child not
// reaped fails with its own message.
func TestReadOutputEndsWhenItsContextEndsAfterTheChildExited(t *testing.T) {
	guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
	guard.waitDelay = time.Hour
	argv := []string{"sh", "-c", "(" + holdUntilEnd(t) + ") 2>/dev/null &\necho $$"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reap := max(min(time.Minute, testwait.Remaining(t)-5*time.Second), 2*time.Second)
	var err error
	answered(t, "the read whose child has exited and whose context then ended", func() {
		_, err = guard.readOutput(ctx, child{argv: argv}, func(r io.Reader) error {
			line, err := bufio.NewReader(r).ReadString('\n')
			if err != nil {
				return fmt.Errorf("the child's process id: %w", err)
			}
			pid, err := strconv.Atoi(strings.TrimSpace(line))
			if err != nil {
				return fmt.Errorf("the child's process id %q: %w", line, err)
			}
			if !testwait.Holds(reap, testwait.Poll, func() bool { return syscall.Kill(pid, 0) != nil }) {
				return fmt.Errorf("the child, process %d, was not reaped within %s", pid, reap)
			}
			cancel()
			_, err = io.ReadAll(r)
			return err
		})
	})
	var stop *stoppedError
	if !errors.As(err, &stop) {
		t.Fatalf("readOutput answered %v, want the stop of its context's end", err)
	}
}

// A read after the child's exit that finds the pipe empty, or at its end, before it has read the
// bytes it counted fails rather than ending as if it had read them all, so the push is refused.
// One reader of the pipe cannot cause it: here the test takes some of the counted bytes from the
// pipe itself, with the write end open (the pipe empty) and closed (the pipe at its end).
func TestAReadThatFindsFewerBytesThanItCountedFails(t *testing.T) {
	for _, closed := range []bool{false, true} {
		t.Run(fmt.Sprintf("the write end closed: %t", closed), func(t *testing.T) {
			out, in, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer out.Close()
			defer in.Close()
			if _, err := in.Write(bytes.Repeat([]byte("A"), 1000)); err != nil {
				t.Fatal(err)
			}
			if closed {
				_ = in.Close()
			}
			reader := &pipeReader{ctx: context.Background(), file: out}
			reader.exited.Store(true)
			if _, err := reader.Read(make([]byte, 100)); err != nil {
				t.Fatalf("the first read: %v", err)
			}
			if n, err := io.ReadFull(out, make([]byte, 500)); err != nil {
				t.Fatalf("the test's own read of %d bytes: %v", n, err)
			}
			if _, err := io.ReadAll(reader); !errors.Is(err, errShortPipe) {
				t.Fatalf("the read of the rest answered %v, want errShortPipe", err)
			}
		})
	}
}

// A read of a push's diff ends with the push's bound while git's diff has not exited, and a
// process git started has left git's process group (setsid) and holds the diff's pipe until the
// test ends: the read is refused as a setup failure naming the bound. The stand-in runs every
// other git at once and, for the diff, runs the real git and then stays in its foreground until
// the bound stops it. The escaped process holds standard output alone: one that held standard
// error too would keep exec's copy of it running.
func TestADiffReadEndsWithItsBoundThoughAnEscapedProcessHoldsThePipe(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("no setsid on this machine")
	}
	push := aPushOf(t, map[string]string{"notes.md": "a line of nothing in particular\n"})
	git := fixturegit.CompiledGit(t)
	hold := holdUntilEnd(t)
	guard := gitFirst(t, "case \" $* \" in\n"+
		"*\" --unified=0 \"*) setsid sh -c \""+strings.ReplaceAll(hold, "\"", "\\\"")+"\" 2>/dev/null &\n  '"+git+"' \"$@\"\n  "+hold+" ;;\n"+
		"*) exec '"+git+"' \"$@\" ;;\nesac\n")
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	var err error
	answered(t, "the push whose diff an escaped process holds", func() { err = guard.Push(ctx, push.req) })
	says(t, setupFailure(t, err), "was stopped by its bound")
}

// A file renamed to a path the rules refuse is read as its new path, which is the path the
// destination learns, and not as the path it was at. The rename moves the file whole, so the
// path is the whole of what the commit names.
func TestARenamedPathIsReadAsItsNewPath(t *testing.T) {
	push := aPushOf(t, map[string]string{"old.md": "a line of nothing in particular\n"})
	fixture := fixturegit.Isolated(t, t.TempDir())
	base := push.req.Updates[0].LocalSHA
	// Built out of two pieces for the reason theHost is (TestTheReportMasksAPathComponent).
	path := "packages/" + "agen" + "t-c/notes.md"
	if err := os.MkdirAll(filepath.Join(push.req.Dir, filepath.Dir(path)), 0o700); err != nil {
		t.Fatal(err)
	}
	fixture.Run(push.req.Dir, "git", "mv", "old.md", path)
	fixture.Run(push.req.Dir, "git", "commit", "--quiet", "-m", "a rename")
	push.req.Updates = []Update{{LocalRef: "refs/heads/main", LocalSHA: fixture.Run(push.req.Dir, "git", "rev-parse", "HEAD"), RemoteRef: "refs/heads/main", RemoteSHA: base}}
	const class = "names the private deployment repository or its Dispatch project key"
	want := []string{"packages/<name>/notes.md: " + class, "packages/<name>/notes.md: " + class + " in its path"}
	if got := refusal(t, push.through(guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public))).Lines; !slices.Equal(got, want) {
		t.Fatalf("the report is %q, want %q", got, want)
	}
}

// A path that holds a space is reported as itself. git ends the "+++" line that names such a path
// with a tab, which is no part of the path; where the path also holds a byte git quotes, a tab of
// its own here, git writes it C-quoted with its b/ prefix inside the quotes, and the report names
// the path itself, not that quoted form.
func TestAPathWithASpaceIsReportedAsItself(t *testing.T) {
	push := aPushOf(t, map[string]string{
		"a named file.md":     "the runner answers at " + theHost + "\n",
		"a named\tand tab.md": "the runner answers at " + theHost + "\n",
	})
	err := push.through(guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public))
	// git lists the paths in byte order, and a tab sorts before a space.
	want := []string{"a named\tand tab.md: " + theClass, "a named file.md: " + theClass}
	if got := refusal(t, err).Lines; !slices.Equal(got, want) {
		t.Fatalf("the report is %q, want %q", got, want)
	}
}

// A refusal names at most refusalLines lines and counts the rest in one last line of its own, so a
// push of a file of many lines at a path a path rule refuses, each line refused for the path, is a
// report of bounded size. A refusal of no more lines than the bound is every line, as it was.
func TestARefusalNamesAtMostItsBoundOfLines(t *testing.T) {
	// Built out of two pieces for the reason theHost is (TestTheReportMasksAPathComponent).
	path := "packages/" + "agen" + "t-c/notes.md"
	const class = "names the private deployment repository or its Dispatch project key"
	for _, tc := range []struct {
		name string
		// lines is how many lines the file adds; the report of each is one line, and the path's
		// own record is two more, one of the content rule and one of the path rule.
		lines int
		want  func(today []string) []string
	}{
		{name: "fewer lines than the bound", lines: 3, want: func(today []string) []string { return today }},
		{name: "the bound's own number of lines", lines: refusalLines - 2, want: func(today []string) []string { return today }},
		{
			name:  "one line more than the bound",
			lines: refusalLines - 1,
			want: func(today []string) []string {
				return append(today[:refusalLines:refusalLines], fmt.Sprintf("(1 more line left out: this report names the first %d)", refusalLines))
			},
		},
		{
			name:  "more lines than the bound",
			lines: refusalLines + 5,
			want: func(today []string) []string {
				return append(today[:refusalLines:refusalLines], fmt.Sprintf("(7 more lines left out: this report names the first %d)", refusalLines))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var content strings.Builder
			for i := range tc.lines {
				fmt.Fprintf(&content, "line %d of nothing in particular\n", i)
			}
			push := aPushOf(t, map[string]string{path: content.String()})
			got := refusal(t, push.through(guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public))).Lines
			today := []string{"packages/<name>/notes.md: " + class, "packages/<name>/notes.md: " + class + " in its path"}
			today = append(today, slices.Repeat([]string{"packages/<name>/notes.md: " + class + " in its path"}, tc.lines)...)
			want := tc.want(today)
			if !slices.Equal(got, want) {
				t.Fatalf("the report is %d lines ending %q, want %d lines ending %q", len(got), got[max(0, len(got)-2):], len(want), want[max(0, len(want)-2):])
			}
		})
	}
}
