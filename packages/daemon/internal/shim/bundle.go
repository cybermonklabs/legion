package shim

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The release's save: the shim of one role builds the git bundle of the unpushed work of the
// issue's shared clone — the issue's own working copy, the one every role of its issue starts in,
// and the working copy of each task beside it — and the daemon stores it until the pod that
// replaces the released one has restored it (internal/launcher). Only a shim can build it — it
// runs the jj of the workspace it already works in, and it dies with its role's process.

// bundleKeptRevset is every commit a release keeps: every visible commit the remote does not
// already hold, on any head, and the working copy's own change. bundleTipsRevset is the heads of
// that set, the commits the bundle is built from; the bases it is built against are the remote
// heads a new pod's workspace fetch already holds.
//
// Tags are subtracted beside remote bookmarks because a clone of this repository holds thousands
// of them, jj makes each tagged commit a visible head, and the release commits they point at are
// reachable from no remote bookmark. Measured in an issue workspace of sjawhar/legion: the set
// without the tags is 536 commits on 16 heads, with them 1640 commits on 773, and the bundle
// grows from 3,993,189 to 6,030,760 bytes. Every one of those extra commits is already on the
// remote, so saving it keeps nothing, and restoring it would make 757 heads visible in a
// workspace whose agent made none of them.
const (
	bundleKeptRevset = `(::visible_heads() | ::@) ~ ::(remote_bookmarks() | tags())`
	bundleTipsRevset = "heads(" + bundleKeptRevset + ")"
)

// bundleRefPrefix is where the save's temporary refs live while `git bundle create` reads them.
// git bundles refs, not bare commit ids ("Refusing to create empty bundle"), and a saved head
// usually has no ref of its own; the prefix is outside refs/heads, refs/tags and refs/remotes, so
// jj imports none of them, and every one is removed once the bundle is written.
const bundleRefPrefix = "refs/legion-bundle/"

// rootCommit is jj's own virtual root, which no git object store holds: a repository with no
// trunk resolves trunk() to it, and it is never a base a bundle can be built against.
const rootCommit = "0000000000000000000000000000000000000000"

// workspaceSave is what one bundle run produced: the saved work itself, in the one shape the
// daemon, the launcher and the wire all read it as (shimwire.WorkspaceBundle), and its size,
// which is reported even when the bundle is refused for being too large and so has no bytes.
type workspaceSave struct {
	bundle shimwire.WorkspaceBundle
	size   int
}

// bundleWorkspace answers `bundle-workspace`: the release's save of the unpushed work of the
// workspace this shim runs in and of each task's working copy beside it. A request the shim cannot
// act on is answered with the reason when it names an id to answer, and a bundle over the
// request's bound is refused with its size, so the daemon can stop the release and record why. The
// result crosses the lane as an rpc_chunk sequence when it is larger than one line.
func (s *shim) bundleWorkspace(request shimwire.BundleWorkspace) {
	result := shimwire.BundleWorkspaceResult{ID: request.ID, OK: true, At: time.Now().UTC()}
	if err := request.Validate(); err != nil {
		if request.ID == "" {
			s.log.Printf("[worker-shim] refusing a bundle-workspace frame: %v", err)
			return
		}
		result.OK, result.Error = false, err.Error()
	} else {
		saved, err := s.runBundle(request)
		result.SizeBytes = saved.size
		switch {
		case err != nil:
			result.OK, result.Error = false, err.Error()
		default:
			b := saved.bundle
			result.Bundle, result.Head, result.Tips, result.Bookmarks, result.WorkingCopies, result.At = b.Bundle, b.Head, b.Tips, b.Bookmarks, b.WorkingCopies, b.At
		}
	}
	s.toDaemonChunked(result)
}

// runBundle builds the bundle with the jj and git of PATH — the ones the pane's or pod's own
// shell runs, since no runtime pins either on a pane or a pod — in the workspace the shim's
// environment names in LEGION_WORKSPACE, which the runtime sets on every role (openWorkspace). It
// reads the working copy's commit first, then the working-copy commit of each task's working copy
// beside it (taskWorkingCopies): those reads are the jj snapshots the save makes, one for each
// working copy; every read after them is --ignore-working-copy, so the whole save describes one
// state of the clone. The bundle is written under the process's temporary directory, never in the
// workspace, and removed. The budget is a bound on the shim's command context (s.commands) rather
// than a context of its own, so the shim's own end ends the save's jj and git at once and a
// command that outlives the budget is still reported as a timeout.
func (s *shim) runBundle(request shimwire.BundleWorkspace) (workspaceSave, error) {
	opened, done, err := s.openWorkspace("bundle", s.cfg.Env, time.Duration(request.TimeoutMs)*time.Millisecond)
	if err != nil {
		return workspaceSave{}, err
	}
	defer done()
	dir, jj, run := opened.dir, opened.jj, opened.run
	git, err := exec.LookPath("git")
	if err != nil {
		return workspaceSave{}, fmt.Errorf("worker-shim: git is not on PATH (%s); no git to bundle the workspace with", os.Getenv("PATH"))
	}
	gitDir, err := workspace.GitDir(dir)
	if err != nil {
		return workspaceSave{}, fmt.Errorf("worker-shim: %w", err)
	}
	read := func(revset string) ([]string, error) {
		out, err := run(nil, jj, "log", "-R", dir, "--ignore-working-copy", "-r", revset, "--no-graph", "-T", `commit_id ++ "\n"`)
		if err != nil {
			return nil, err
		}
		return workspace.OutputLines(out), nil
	}

	head, err := run(nil, jj, "log", "-R", dir, "-r", "@", "--no-graph", "-T", "commit_id")
	if err != nil {
		return workspaceSave{}, err
	}
	copies, err := taskWorkingCopies(run, jj, dir, request.Issue)
	if err != nil {
		return workspaceSave{}, err
	}
	tips, err := read(bundleTipsRevset)
	if err != nil {
		return workspaceSave{}, err
	}
	if len(tips) == 0 {
		return workspaceSave{}, errors.New("worker-shim: the workspace holds no commit outside the remote's branches, not even its working copy")
	}
	kept, err := read(bundleKeptRevset)
	if err != nil {
		return workspaceSave{}, err
	}
	// The bases are the remote heads a new pod's workspace fetch already holds. A repository with
	// no trunk resolves trunk() to the root commit, which is jj's own and no object git knows, so
	// it is dropped: a bundle with no base is a whole one, which is what such a repository needs.
	bases, err := read(`trunk() | remote_bookmarks(exact:"legion/` + request.Issue + `")`)
	if err != nil {
		return workspaceSave{}, err
	}
	bases = slices.DeleteFunc(bases, func(commit string) bool { return commit == rootCommit })
	listed, err := run(nil, jj, "bookmark", "list", "-R", dir, "--ignore-working-copy",
		"-T", `if(remote, "", name ++ " " ++ normal_target.commit_id() ++ "\n")`)
	if err != nil {
		return workspaceSave{}, err
	}
	bookmarks := bookmarksInto(listed, kept)
	// A task's working copy on a commit the remote already holds keeps nothing unpushed: the
	// adoption of that task's next step makes it again from the task's branch.
	maps.DeleteFunc(copies, func(_, commit string) bool { return !slices.Contains(kept, commit) })

	file, err := os.CreateTemp("", "legion-workspace-*.bundle")
	if err != nil {
		return workspaceSave{}, fmt.Errorf("worker-shim: make the bundle's file: %w", err)
	}
	path := file.Name()
	_ = file.Close()
	defer os.Remove(path)
	if _, err := run(workspace.RefUpdates("create", bundleRefPrefix, tips), git, "--git-dir="+gitDir, "update-ref", "--stdin"); err != nil {
		return workspaceSave{}, err
	}
	create := []string{git, "--git-dir=" + gitDir, "bundle", "create", path, "--glob=" + bundleRefPrefix}
	if len(bases) > 0 {
		create = append(create, append([]string{"--not"}, bases...)...)
	}
	_, bundleErr := run(nil, create...)
	if _, err := run(workspace.RefUpdates("delete", bundleRefPrefix, tips), git, "--git-dir="+gitDir, "update-ref", "--stdin"); err != nil && bundleErr == nil {
		bundleErr = err
	}
	if bundleErr != nil {
		return workspaceSave{}, bundleErr
	}
	// The bundle is measured before it is read: a workspace that bundles to hundreds of megabytes
	// is refused on its size alone, so the bytes over the bound never enter the pod's memory.
	info, err := os.Stat(path)
	if err != nil {
		return workspaceSave{}, fmt.Errorf("worker-shim: read the bundle: %w", err)
	}
	if info.Size() > int64(request.MaxBytes) {
		return workspaceSave{size: int(info.Size())},
			fmt.Errorf("worker-shim: the workspace's unpushed work is %d bytes, over the %d the release carries", info.Size(), request.MaxBytes)
	}
	bundle, err := os.ReadFile(path)
	if err != nil {
		return workspaceSave{}, fmt.Errorf("worker-shim: read the bundle: %w", err)
	}
	return workspaceSave{
		bundle: shimwire.WorkspaceBundle{Bundle: bundle, Head: head, Tips: tips, Bookmarks: bookmarks, WorkingCopies: copies, At: time.Now().UTC()},
		size:   len(bundle),
	}, nil
}

// taskWorkingCopies is the working-copy commit of each task's working copy beside the issue's
// working copy dir, by the task's branch. A task's working copy is a workspace of the issue's
// clone named for the branch without legion/, lower-cased as its directory is (workspace.Name,
// workspace.TaskDir): <key>.<part>, the part read whole, a later give's `<slug>.<n>` included. A
// workspace of any other name, of no task's branch, or whose directory is gone, is no task's
// working copy and is not read. Each read runs without --ignore-working-copy, so it snapshots the
// edit the task's agent left there, as the read of the issue's own @ does.
func taskWorkingCopies(run func(stdin []byte, args ...string) (string, error), jj, dir, issue string) (map[string]string, error) {
	listed, err := run(nil, jj, "workspace", "list", "-R", dir, "--ignore-working-copy", "-T", `name ++ "\n"`)
	if err != nil {
		return nil, err
	}
	prefix := strings.ToLower(issue) + "."
	copies := map[string]string{}
	for _, name := range workspace.OutputLines(listed) {
		part, ok := strings.CutPrefix(name, prefix)
		if !ok {
			continue
		}
		branch := workspace.Bookmark(issue) + "." + part
		taskDir, err := workspace.TaskDir(dir, branch)
		if err != nil || filepath.Base(taskDir) != name {
			continue
		}
		if _, err := os.Lstat(taskDir); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, fmt.Errorf("worker-shim: the working copy of %s: %w", branch, err)
		}
		commit, err := run(nil, jj, "log", "-R", taskDir, "-r", "@", "--no-graph", "-T", "commit_id")
		if err != nil {
			return nil, err
		}
		copies[branch] = commit
	}
	return copies, nil
}

// bookmarksInto is every local bookmark of listed that points at a commit the bundle keeps, by
// name. A bookmark on a commit the remote already holds — the default branch's, above all — is
// not the workspace's unpushed work and is left where the new pod's fetch will put it.
func bookmarksInto(listed string, kept []string) map[string]string {
	into := map[string]string{}
	for _, line := range workspace.OutputLines(listed) {
		name, commit, found := strings.Cut(line, " ")
		if !found || !slices.Contains(kept, commit) {
			continue
		}
		into[name] = commit
	}
	return into
}
