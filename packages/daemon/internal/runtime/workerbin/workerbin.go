// Package workerbin installs the directory at the head of a host-run Legion agent's PATH, under a
// root directory: bin. Each script in bin execs the legion that installed it — the daemon's, for a
// tmux daemon's panes, and the operator's, for the controller `legion controller start` runs. Its
// legion launcher runs that legion, so an agent under root never reaches another `legion` on the
// PATH it inherits. Its gh runs that legion's `legion gh`, which runs the real gh (LEGION_GH_PATH)
// with the gh directory of the owner the command names (internal/ghconfig's bundle). Its git runs
// that legion's `legion git --after <bin>`, which adds a commit's Omp-Session and Legion-Role
// trailers and runs the next git on PATH after bin, a host's own git wrapper included. Install
// writes no git where the first git on the inherited PATH is the image's own git wrapper, which
// hands a commit to `legion git` itself. An agent's jj is the first jj on its PATH. A pod installs
// nothing: the image carries its own gh and git wrappers in /opt/legion/bin, which its PATH names
// first (packages/daemon/docker/gh, the script Install writes for the image's legion, and
// packages/daemon/docker/git).
package workerbin

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
)

// imageGit is the image's git wrapper, packages/daemon/docker/git, the first git on a pod's PATH.
// A test may point it at a stand-in.
var imageGit = "/opt/legion/bin/git"

// LauncherDir is root's bin: the directory of the legion launcher, the gh wrapper and the git
// wrapper that Install writes.
func LauncherDir(root string) string { return filepath.Join(root, "bin") }

// Install installs bin's scripts. Each execs legionExecutable, its path absolute and single-quoted
// (shellprefix.Literal): the legion launcher runs it as it is, gh runs its `legion gh`, and git
// runs its `legion git --after <bin>`, with bin's absolute path quoted the same way. path is the
// PATH that the agents under root inherit. Install writes git unless the first executable git on
// path, leaving out bin itself, is the image's git wrapper (imageGit). That wrapper already hands
// a commit to `legion git`, and the image's jj wrapper refuses a push whose git is not it. Where
// the image's wrapper is first, Install removes a git that an earlier run wrote. Where path holds
// no git at all, Install writes git still, and that git then fails when it runs, naming the git it
// lacks (exit 127). Its two callers pass their own executable and their own PATH: a tmux daemon's
// boot (internal/daemon) and `legion controller start` on the operator's machine (cmd/legion).
func Install(root, legionExecutable, path string) error {
	if !filepath.IsAbs(legionExecutable) {
		return fmt.Errorf("legion launcher target %q is not an absolute path", legionExecutable)
	}
	bin, err := filepath.Abs(LauncherDir(root))
	if err != nil {
		return fmt.Errorf("resolve %s: %w", LauncherDir(root), err)
	}
	legion := shellprefix.Literal(legionExecutable)
	if err := installScript(bin, "legion", "#!/bin/sh\nexec "+legion+" \"$@\"\n"); err != nil {
		return err
	}
	if err := installScript(bin, "gh", "#!/bin/sh\nexec "+legion+" gh \"$@\"\n"); err != nil {
		return err
	}
	if isImageGit(firstGit(path, bin)) {
		if err := os.Remove(filepath.Join(bin, "git")); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove the git script of an earlier install: %w", err)
		}
		return nil
	}
	return installScript(bin, "git", "#!/bin/sh\nexec "+legion+" git --after "+shellprefix.Literal(bin)+" -- \"$@\"\n")
}

// firstGit is the first executable git on path, leaving out each entry that is bin (the same
// cleaned path, or the same directory by os.SameFile), or "" when there is none.
func firstGit(path, bin string) string {
	binInfo, binErr := os.Stat(bin)
	for _, entry := range filepath.SplitList(path) {
		if entry == "" || filepath.Clean(entry) == bin {
			continue
		}
		if binErr == nil {
			if info, err := os.Stat(entry); err == nil && os.SameFile(info, binInfo) {
				continue
			}
		}
		git := filepath.Join(entry, "git")
		if info, err := os.Stat(git); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return git
		}
	}
	return ""
}

// isImageGit is whether git is the image's git wrapper (imageGit), by os.SameFile, so a link to
// it counts too.
func isImageGit(git string) bool {
	if git == "" {
		return false
	}
	found, err := os.Stat(git)
	if err != nil {
		return false
	}
	image, err := os.Stat(imageGit)
	return err == nil && os.SameFile(found, image)
}

// Path makes root's bin the first entry of path, exactly once. A daemon started from an agent's
// shell can inherit its predecessor's directory; removing every occurrence keeps PATH from growing
// while keeping every other entry verbatim.
func Path(path, root string) string {
	launcher := LauncherDir(root)
	entries := make([]string, 0, len(filepath.SplitList(path))+1)
	entries = append(entries, launcher)
	for _, entry := range filepath.SplitList(path) {
		if entry != "" && entry != launcher {
			entries = append(entries, entry)
		}
	}
	return strings.Join(entries, string(filepath.ListSeparator))
}

// installScript writes one 0700 script into a 0700 directory, replacing any earlier one atomically.
func installScript(dir, name, contents string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("chmod %s: %w", dir, err)
	}
	temporary, err := os.CreateTemp(dir, "."+name+"-")
	if err != nil {
		return fmt.Errorf("create %s script: %w", name, err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if _, err := temporary.WriteString(contents); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write %s script: %w", name, err)
	}
	if err := temporary.Chmod(0o700); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("chmod %s script: %w", name, err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close %s script: %w", name, err)
	}
	if err := os.Rename(temporaryPath, filepath.Join(dir, name)); err != nil {
		return fmt.Errorf("install %s script: %w", name, err)
	}
	return nil
}
