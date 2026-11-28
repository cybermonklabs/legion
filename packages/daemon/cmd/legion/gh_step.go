package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sjawhar/legion/daemon/internal/leakguard"
)

// ghStepFileVar names the file a caller of `legion gh` asks it to write the step it is in to: the
// capability report's `github` row (packages/pi-legion's checkGitHub), which reads the file when
// the call misses the row's deadline and names that step in the row. It is diagnostics alone: no
// value of it changes what the call does or the status it exits with, and gh is never given it
// (ghChildKeeps names it not).
const ghStepFileVar = "LEGION_GH_STEP_FILE"

// The steps of a `legion gh` call, in their order, beside the guard's own three (leakguard's
// StepCheck, StepVisibility and StepRules, which come between help and gh): config is the gh
// directory's shape and the git configuration's reads, from the start of runGh; help is the
// command's routing and the reads of gh's help it makes (ghroute.TargetOf); gh is the real gh.
const (
	ghStepConfig leakguard.Step = "config"
	ghStepHelp   leakguard.Step = "help"
	ghStepGh     leakguard.Step = "gh"
)

// ghSteps writes each step a call starts to the step file, as the one line `<step> <milliseconds
// since started>`. A nil *ghSteps writes nothing, which is every call whose caller named no file
// it may write.
type ghSteps struct {
	// dir is the file's directory, open, and name the file's name in it: every write goes through
	// the descriptor, so the directory checked is the directory written, whatever its path names
	// after the check.
	dir     *os.File
	name    string
	started time.Time
}

// openGhSteps is the writer of the step file at path for a call that started at started, or nil
// where path is not one this command writes: path must be absolute and clean, and its directory a
// directory, not a link, owned by this process's user with no permission for its group or others
// (ghPrivate, read off the open descriptor and then off the path with Lstat, by ghOpenedAs, since
// under gVisor an open with O_NOFOLLOW follows a link to a directory). Anything else, the empty
// value of an unset variable among them, is nil and says nothing: the file is diagnostics, and a
// call that cannot write it runs as one with none.
func openGhSteps(path string, started time.Time) *ghSteps {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return nil
	}
	dir := filepath.Dir(path)
	f, err := ghOpenDir(dir)
	if err != nil {
		return nil
	}
	if !ghOpenedAs(f, dir, fs.ModeDir, os.Getuid()) {
		f.Close()
		return nil
	}
	return &ghSteps{dir: f, name: filepath.Base(path), started: started}
}

// write replaces the step file with step's line: into a file of mode 0600 made in the same
// directory and renamed over it, so the reader, which may read at any moment, finds the line
// before or the line after and never part of one. A step that cannot be written leaves the line
// before, and nothing is said of it.
func (s *ghSteps) write(step leakguard.Step) {
	if s == nil {
		return
	}
	line := string(step) + " " + strconv.FormatInt(time.Since(s.started).Milliseconds(), 10) + "\n"
	dir := int(s.dir.Fd())
	// The name is this process's own, so no other writer's file is in the way; one left by a
	// process of the same id that was killed mid-write is removed first.
	tmp := "." + s.name + "." + strconv.Itoa(os.Getpid()) + ".tmp"
	_ = unix.Unlinkat(dir, tmp, 0)
	fd, err := unix.Openat(dir, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return
	}
	file := os.NewFile(uintptr(fd), tmp)
	_, err = file.WriteString(line)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = unix.Renameat(dir, tmp, dir, s.name)
	}
	if err != nil {
		_ = unix.Unlinkat(dir, tmp, 0)
	}
}

// close closes the file's directory.
func (s *ghSteps) close() {
	if s != nil {
		s.dir.Close()
	}
}
