// Package shimtest is the fixture of the tests that run the real shim's bundler against a real
// workspace: one remote, a pod's colocated clone of it and the jj issue workspace provisioning adds
// there, work in that workspace that no remote holds, and the shim itself (shim.Run) asked for the
// bundle of that work over a worker stream of the test's own. The launcher's tests restore what it
// bundles into the pods that replace the released one (internal/launcher), and the daemon's drive
// the copies its cadence takes of a running pod's workspace through it (internal/daemon), so the
// two share this one body rather than each keeping a copy.
//
// It is an ordinary package rather than a _test.go file of one of them because a test file's
// declarations belong to its own package's tests, and no other package can import them. Only
// tests import it. Every package whose tests do runs real git and jj, in the fixture and in the
// shim's bundler, so its TestMain guards its process (fixtureenv.MustGuard, docs/testing.md).
package shimtest

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/shim"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// Workspace is one remote, the pod that works on Issue and the workspace it works in: real git
// and real jj throughout, every command naming the directory it runs in (docs/testing.md).
type Workspace struct {
	// Issue is the issue the workspace belongs to. The remote holds its branch `legion/<Issue>`
	// beside main, and the shim builds its bundle against both (Bundle).
	Issue string
	// Root is the test's own directory that everything of the fixture is made under.
	Root string
	// Remote is the bare repository every pod clones.
	Remote string
	// Clone is the pod's colocated clone of Remote.
	Clone string
	// Dir is the issue's workspace in Clone, the one the shim bundles.
	Dir string
	// Env is the whole environment of every git and jj the fixture runs, and of the shim.
	Env []string
	// Commits is the commit ids the fixture and its caller named, by name: "one", the remote's
	// first commit, and each commit Work made.
	Commits map[string]string

	tb testing.TB
}

// New is a remote whose main and `legion/<issue>` both hold two commits, and one pod's clone of it
// with the issue's workspace in it (Pod). It skips the test where jj or git is not on PATH.
func New(tb testing.TB, issue string) *Workspace {
	tb.Helper()
	for _, tool := range []string{"jj", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			tb.Skipf("%s is required: %v", tool, err)
		}
	}
	root := tb.TempDir()
	// Neither the workspace, the configuration, the remote nor the credential of the process that
	// runs the tests reaches the fixture's git and jj: its environment is fixtureenv.Isolated of
	// a home of this test's own, which holds none of the host's jj and git configuration — its
	// user, its trailers, its filters — and the identity below is the fixture's own
	// (docs/testing.md).
	home := filepath.Join(root, "home")
	w := &Workspace{
		Issue: issue, Root: root, Remote: filepath.Join(root, "remote"), Clone: filepath.Join(root, "clone"), Dir: filepath.Join(root, "ws"),
		Commits: map[string]string{},
		Env: append(fixtureenv.Isolated(home),
			"JJ_USER=tester", "JJ_EMAIL=tester@example.test",
			"GIT_AUTHOR_NAME=tester", "GIT_AUTHOR_EMAIL=tester@example.test",
			"GIT_COMMITTER_NAME=tester", "GIT_COMMITTER_EMAIL=tester@example.test"),
		tb: tb,
	}
	seed := filepath.Join(root, "seed")
	for _, dir := range []string{w.Remote, seed, home} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			tb.Fatal(err)
		}
	}
	w.Run(w.Remote, fixturegit.InitBare(w.Remote, "")...)
	w.Run(w.Remote, "git", "--git-dir="+w.Remote, "symbolic-ref", "HEAD", "refs/heads/main")
	w.Run(seed, fixturegit.InitWorkTree(seed, "main")...)
	w.Write(filepath.Join(seed, "f.txt"), "one\n")
	w.Run(seed, "git", "add", "-A")
	w.Run(seed, "git", "commit", "--quiet", "-m", "one")
	w.Commits["one"] = w.Run(seed, "git", "rev-parse", "HEAD")
	w.Run(seed, "git", "remote", "add", "origin", w.Remote)
	w.Run(seed, "git", "push", "--quiet", "-u", "origin", "main")
	w.Write(filepath.Join(seed, "f.txt"), "two\n")
	w.Run(seed, "git", "commit", "--quiet", "-am", "two")
	w.Run(seed, "git", "push", "--quiet", "origin", "main")
	w.Run(seed, "git", "push", "--quiet", "origin", "main:refs/heads/legion/"+issue)
	w.Pod(w.Clone, w.Dir)
	return w
}

// Pod is a pod's shared clone and the issue workspace in it, as provisioning makes them: a
// colocated clone of the remote with every remote branch tracked, one `jj workspace add`, and the
// record of when the workspace was created, which a restore reads to tell a workspace provisioned
// since the save from the one the save came out of (workspace.RecordCreated, the launcher's
// restoreWorkspace).
func (w *Workspace) Pod(clone, dir string) {
	w.tb.Helper()
	// --no-local: a local clone hardlinks the whole object store, unreachable objects included,
	// where a pod clones over the network and gets only what the remote's refs reach.
	w.Run(w.Root, "git", "clone", "--quiet", "--no-local", w.Remote, clone)
	w.Run(clone, fixturegit.JJInit(clone, true)...)
	w.Run(clone, "jj", "bookmark", "track", "main@origin", "legion/"+w.Issue+"@origin")
	w.Run(clone, "jj", "workspace", "add", "--name", filepath.Base(dir), dir)
	if err := workspace.RecordCreated(dir, time.Now()); err != nil {
		w.tb.Fatal(err)
	}
}

// Work commits work no remote holds in the workspace, as a role's turn does: a file of its own,
// name.txt, in a commit described as name, with a new change started on top of it, so the commit
// is the working copy's parent. It records the commit in Commits under name and answers its id.
func (w *Workspace) Work(name string) string {
	w.tb.Helper()
	w.Write(filepath.Join(w.Dir, name+".txt"), name+"\n")
	w.Run(w.Dir, "jj", "describe", "-R", w.Dir, "-m", name)
	commit := w.WorkingCopy()
	w.Run(w.Dir, "jj", "new", "-R", w.Dir)
	w.Commits[name] = commit
	return commit
}

// WorkingCopy is the workspace's working-copy commit, after the snapshot the read itself makes.
func (w *Workspace) WorkingCopy() string {
	w.tb.Helper()
	return w.Run(w.Dir, "jj", "log", "-R", w.Dir, "-r", "@", "--no-graph", "-T", "commit_id")
}

// bundleBudget bounds one Bundle: the shim's dial, its build of the bundle and its answer.
const bundleBudget = 2 * time.Minute

// Bundle runs the real shim against the workspace and answers the bundle it built: the daemon's
// end of the worker stream, one bundle-workspace frame bounded at maxBytes, and the result the
// shim answers, reassembled from its chunk lane as the daemon's reader reassembles it. The bundle
// is the shim's whole answer, its own time of the reading (At) included; a caller that stands in
// for the daemon's reader leaves out what that reader leaves out (stream.Conn.BundleWorkspace).
//
// It fails no test, because the daemon's cadence asks for a bundle from a goroutine of its own,
// where no test may be failed: every way the exchange comes to nothing, the shim's refusal among
// them, is its error. The shim runs only while Bundle does. It is stopped, and its wrapped process
// with it, before Bundle returns, and the end of ctx ends the exchange.
func (w *Workspace) Bundle(ctx context.Context, maxBytes int) (shimwire.WorkspaceBundle, error) {
	ctx, cancel := context.WithTimeout(ctx, bundleBudget)
	defer cancel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return shimwire.WorkspaceBundle{}, fmt.Errorf("listen for the shim: %w", err)
	}
	defer listener.Close()
	// A shim that never dials would leave the accept waiting past the end of ctx.
	context.AfterFunc(ctx, func() { _ = listener.Close() })
	run, stop := context.WithCancel(ctx)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		// The wrapped process is a plain `cat`: the shim bridges its stdio and answers the
		// bundle frame itself, so nothing of Oh My Pi is needed to prove the save.
		_, _ = shim.Run(run, shim.Config{
			Network: "tcp", Address: listener.Addr().String(), BootToken: "boot",
			Argv: []string{"/bin/sh", "-c", "exec cat"},
			Env:  slices.Concat(w.Env, []string{"LEGION_WORKSPACE=" + w.Dir}),
		})
	}()
	saved, err := w.ask(ctx, listener, maxBytes)
	stop()
	select {
	case <-exited:
	case <-time.After(20 * time.Second):
		err = errors.Join(err, errors.New("the shim did not leave after its context ended"))
	}
	return saved, err
}

// ask is the daemon's end of one exchange with the shim that dials listener: the shim's hello and
// its ack, the bundle-workspace frame, and the lines the shim sends until its result is whole.
func (w *Workspace) ask(ctx context.Context, listener net.Listener, maxBytes int) (shimwire.WorkspaceBundle, error) {
	conn, err := listener.Accept()
	if err != nil {
		return shimwire.WorkspaceBundle{}, fmt.Errorf("the shim never dialled: %w", err)
	}
	defer conn.Close()
	// The deadline ends a read the shim leaves waiting once the budget is spent, and a cancel of
	// ctx closes the connection, which ends it sooner.
	context.AfterFunc(ctx, func() { _ = conn.Close() })
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return shimwire.WorkspaceBundle{}, err
	}
	reader := shimwire.NewReader(bufio.NewReader(conn))
	if line, err := reader.ReadLine(); err != nil {
		return shimwire.WorkspaceBundle{}, fmt.Errorf("the shim's hello: %w", err)
	} else if frame, err := shimwire.Decode(line); err != nil || frame.FrameType() != shimwire.TypeHello2 {
		return shimwire.WorkspaceBundle{}, fmt.Errorf("the shim opened with %v (%v)", frame, err)
	}
	writer := shimwire.NewWriter(conn)
	if err := writer.WriteFrame(shimwire.HelloAck{}); err != nil {
		return shimwire.WorkspaceBundle{}, fmt.Errorf("ack the shim's hello: %w", err)
	}
	request := shimwire.BundleWorkspace{ID: "bundle-1", Issue: w.Issue, MaxBytes: maxBytes, TimeoutMs: int(bundleBudget.Milliseconds())}
	if err := writer.WriteFrame(request); err != nil {
		return shimwire.WorkspaceBundle{}, fmt.Errorf("ask the shim for the bundle: %w", err)
	}
	var chunks shimwire.Reassembler
	for {
		line, err := reader.ReadLine()
		if err != nil {
			return shimwire.WorkspaceBundle{}, fmt.Errorf("read the shim's answer: %w", err)
		}
		frame, err := shimwire.Decode(line)
		if err != nil {
			continue
		}
		if chunk, ok := frame.(shimwire.RPCChunk); ok {
			whole, err := chunks.Push(chunk)
			if err != nil {
				return shimwire.WorkspaceBundle{}, fmt.Errorf("reassemble the shim's answer: %w", err)
			}
			if whole == nil {
				continue
			}
			if frame, err = shimwire.Decode(whole); err != nil {
				return shimwire.WorkspaceBundle{}, fmt.Errorf("decode the shim's reassembled answer: %w", err)
			}
		}
		result, ok := frame.(shimwire.BundleWorkspaceResult)
		if !ok {
			continue
		}
		if !result.OK {
			return shimwire.WorkspaceBundle{}, fmt.Errorf("the shim refused the save: %s", result.Error)
		}
		if err := result.Validate(); err != nil {
			return shimwire.WorkspaceBundle{}, err
		}
		return shimwire.WorkspaceBundle{
			Bundle: result.Bundle, Head: result.Head, Tips: result.Tips, Bookmarks: result.Bookmarks, At: result.At,
		}, nil
	}
}

// GitDir is the git directory behind a workspace, as the restore resolves it.
func (w *Workspace) GitDir(dir string) string {
	w.tb.Helper()
	gitDir, err := workspace.GitDir(dir)
	if err != nil {
		w.tb.Fatal(err)
	}
	return gitDir
}

// Write writes content to the file at path, failing the test where it cannot.
func (w *Workspace) Write(path, content string) {
	w.tb.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		w.tb.Fatal(err)
	}
}

// Run runs one jj or git command in dir, which is the directory it reads its repository from: jj
// searches upward from the directory it runs in for the closest .jj, so a command with no Dir
// would open the workspace the test process stands in (docs/testing.md).
func (w *Workspace) Run(dir string, args ...string) string {
	w.tb.Helper()
	return fixturegit.Runner{TB: w.tb, Env: w.Env}.Run(dir, args...)
}
