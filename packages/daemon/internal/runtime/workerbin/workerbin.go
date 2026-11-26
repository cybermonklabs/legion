// Package workerbin installs the directory at the head of a host-run Legion agent's PATH, under a
// root directory: bin. Each script in bin execs the legion that installed it — the daemon's, for a
// tmux daemon's panes, and the operator's, for the controller `legion controller start` runs. Its
// legion launcher runs that legion, so an agent under root never reaches another `legion` on the
// PATH it inherits. Its gh runs that legion's `legion gh`, which runs the real gh (LEGION_GH_PATH)
// with the gh directory of the owner the command names (internal/ghconfig's bundle). Its git runs
// that legion's `legion git --after <bin>`, which adds a commit's Omp-Session and Legion-Role
// trailers and runs the next git on PATH after bin, a host's own git wrapper included. An agent's
// jj is bin's jj, which runs that legion's `legion jj --after <bin>`: it expands a command as jj
// does, through jj's aliases and ui.default-command, and before a `jj git push`, or a command that
// jj expands into one, it runs the push step, which sets the author of a commit the push sends from
// the commit's Legion-Role trailer, where its rules let it; then it runs the next jj on PATH after
// bin, a host's own jj wrapper included. Install writes no git where the first git on the inherited
// PATH is the image's own git wrapper, which hands a commit to `legion git` itself, and no jj where
// the next jj is the image's own jj wrapper, which expands each command itself and hands a push to
// `legion jj`.
// A pod installs nothing: the image carries its own gh, git and jj wrappers in /opt/legion/bin,
// which its PATH names first (packages/daemon/docker/gh, the script Install writes for the image's
// legion, packages/daemon/docker/git and packages/daemon/docker/jj).
package workerbin

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/lookpath"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
)

// ImageGit is the image's git wrapper, packages/daemon/docker/git, the first git on a pod's PATH.
const ImageGit = "/opt/legion/bin/git"

// imageGit is ImageGit, or the stand-in a test points it at.
var imageGit = ImageGit

// ImageJJ is the image's jj wrapper, packages/daemon/docker/jj, the first jj on a pod's PATH.
const ImageJJ = "/opt/legion/bin/jj"

// imageJJ is ImageJJ, or the stand-in a test points it at.
var imageJJ = ImageJJ

// LauncherDir is root's bin: the directory of the legion launcher and of the gh, git and jj
// wrappers that Install writes.
func LauncherDir(root string) string { return filepath.Join(root, "bin") }

// Install installs bin's scripts. Each execs legionExecutable, its path absolute and single-quoted
// (shellprefix.Literal): the legion launcher runs it as it is, gh runs its `legion gh`, git runs
// its `legion git --after <bin>` and jj its `legion jj --after <bin>`, with bin's absolute path
// quoted the same way. path is the PATH that the agents under root inherit. Install writes git
// unless the first executable git on path, leaving out bin itself, is the image's git wrapper
// (imageGit). That wrapper already hands a commit to `legion git`, and the image's jj wrapper
// refuses a push whose git is not it. Install writes jj unless the first executable jj on path,
// leaving out bin itself, is the image's jj wrapper (imageJJ), which already hands a push to
// `legion jj`, so the push step runs once. Where the image's wrapper is first, Install removes
// the script of that name an earlier run wrote. Where path holds no git, or no jj, at all, Install
// writes that script still, and it then fails when it runs, naming the program it lacks (exit
// 127). Its two callers pass their own executable and their own PATH: a tmux daemon's boot
// (internal/daemon) and `legion controller start` on the operator's machine (cmd/legion). A
// test's boot of the daemon may pass another legion in place of its own (overrides.paneLegion).
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
	if err := installWrapper(bin, legion, path, "git", imageGit); err != nil {
		return err
	}
	return installWrapper(bin, legion, path, "jj", imageJJ)
}

// installWrapper writes bin's script name (git, jj), which execs legion, quoted, as
// `legion <name> --after <bin>`, unless the first executable program of that name on path,
// leaving out bin itself, is the image's wrapper at image (imageGit, imageJJ): it then removes the
// script of that name an earlier Install wrote.
func installWrapper(bin, legion, path, name, image string) error {
	if isImage(firstOnPath(path, name, bin), image) {
		return removeScript(bin, name)
	}
	return installScript(bin, name, "#!/bin/sh\nexec "+legion+" "+name+" --after "+shellprefix.Literal(bin)+" -- \"$@\"\n")
}

// firstOnPath is the first executable program named name (git, jj) on path, leaving out each
// entry that is bin, or "" when there is none. It is the first of the programs that
// `legion <name> --after <bin>` chooses among (lookpath.Others), so Install reads the program
// that a pane's wrapper of that name would run.
func firstOnPath(path, name, bin string) string {
	if found, _ := lookpath.Others(path, name, bin); len(found) > 0 {
		return found[0].Path
	}
	return ""
}

// isImage is whether program is the image's wrapper at image (imageGit, imageJJ), by
// os.SameFile, so a link to it counts too.
func isImage(program, image string) bool {
	if program == "" {
		return false
	}
	found, err := os.Stat(program)
	if err != nil {
		return false
	}
	wrapper, err := os.Stat(image)
	return err == nil && os.SameFile(found, wrapper)
}

// removeScript removes bin's script name that an earlier Install wrote, where there is one.
func removeScript(bin, name string) error {
	if err := os.Remove(filepath.Join(bin, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove the %s script of an earlier install: %w", name, err)
	}
	return nil
}

// Path makes root's bin the first entry of path, exactly once. A daemon started from an agent's
// shell can inherit its predecessor's directory; removing every occurrence keeps PATH from growing
// while keeping every other entry verbatim.
func Path(path, root string) string {
	launcher := LauncherDir(root)
	parts := filepath.SplitList(path)
	entries := []string{launcher}
	for _, entry := range parts {
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
