//go:build linux

package launcher

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/testwait"
	"golang.org/x/sys/unix"
)

// The guards' cases. The file builds only under Linux, as roleinit_test.go does: the last case
// runs real processes in the namespaces a role runs in, with roleNamespaceAttr's own clone flags
// and uid mapping, and reads them through the container's own /proc — the pod this trimmer was
// written for, and the one thing a fixture /proc cannot prove. The fixture cases keep company
// with it because they are the same guards read a cheaper way.

// The two re-entrant helpers the pod case runs as processes of its own: the child cloned into
// the role's namespaces, and each of the three processes it starts inside them. Each is the test
// binary re-executed with its own LEGION_ marker, which fixtureenv.Guard would take out, so
// TestMain runs no Guard in such a child (helperChildVariables, main_test.go); this file is
// Linux-only, so it registers its markers itself rather than leaving them in that list.
const (
	volumeGuardNamespaceEnv = "LEGION_LAUNCHER_VOLUME_GUARD_NAMESPACE"
	volumeGuardProcessEnv   = "LEGION_LAUNCHER_VOLUME_GUARD_PROCESS"
)

func init() {
	helperChildVariables = append(helperChildVariables, volumeGuardNamespaceEnv, volumeGuardProcessEnv)
}

// fixtureProcess is one process of a /proc a case builds: only the entries that case needs, so a
// reader that found the path through another entry fails the case that asked for this one.
type fixtureProcess struct {
	pid     int
	cwd     string
	exe     string
	root    string
	fds     []string
	maps    []string
	threads map[int]string
	// fdRefused stages a refusal: the fd directory is made unreadable (mode 000), which refuses
	// only a reader that is not root. fdIsFile stages the same branch for any reader, with a
	// regular file where the fd directory belongs.
	fdRefused bool
	fdIsFile  bool
}

// writeProcFixture builds a /proc of the test's own and answers its path.
func writeProcFixture(t *testing.T, processes ...fixtureProcess) string {
	t.Helper()
	root := t.TempDir()
	for _, process := range processes {
		dir := filepath.Join(root, strconv.Itoa(process.pid))
		mkdirFixture(t, dir)
		for name, target := range map[string]string{"cwd": process.cwd, "exe": process.exe, "root": process.root} {
			if target == "" {
				continue
			}
			// The target need not exist: /proc's entries are the kernel's answer about a path,
			// and a fixture that made each one would be making the very directories it asks the
			// guard about.
			if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
				t.Fatal(err)
			}
		}
		switch {
		case process.fdIsFile:
			if err := os.WriteFile(filepath.Join(dir, "fd"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		case process.fdRefused:
			fd := filepath.Join(dir, "fd")
			mkdirFixture(t, fd)
			if err := os.Chmod(fd, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(fd, 0o700) })
		case len(process.fds) > 0:
			fd := filepath.Join(dir, "fd")
			mkdirFixture(t, fd)
			for at, target := range process.fds {
				if err := os.Symlink(target, filepath.Join(fd, strconv.Itoa(3+at))); err != nil {
					t.Fatal(err)
				}
			}
		}
		if len(process.maps) > 0 {
			var body strings.Builder
			for _, mapped := range process.maps {
				// One mapping as /proc writes it: the address range, the permissions, the
				// offset, the device and the inode, then the path.
				fmt.Fprintf(&body, "7f0b2c000000-7f0b2c021000 r-xp 00000000 fd:01 131074 %s\n", mapped)
			}
			// The two kinds of line that name no file, which every real maps file carries.
			body.WriteString("7f0b2c021000-7f0b2c022000 rw-p 00000000 00:00 0 \n")
			body.WriteString("7ffd4c3f0000-7ffd4c411000 rw-p 00000000 00:00 0                          [stack]\n")
			if err := os.WriteFile(filepath.Join(dir, "maps"), []byte(body.String()), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		for tid, cwd := range process.threads {
			task := filepath.Join(dir, "task", strconv.Itoa(tid))
			mkdirFixture(t, task)
			if err := os.Symlink(cwd, filepath.Join(task, "cwd")); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

func mkdirFixture(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

// Every one of the six /proc entries is read, driven one path at a time: each case builds a
// /proc holding one process that names the path through that entry alone, so a reader that
// covered only open file descriptors fails five of the six. An open descriptor is not enough on
// its own — a running program holds none for its own binary or for the libraries it mapped,
// which is the Chromium case `exe` and `maps` cover — and a thread may sit in a directory the
// process's own cwd does not name.
//
// Each case asks twice: for the entry a process holds something under, and for the held path
// itself, because a `flock` is held on a directory and not on a file in it, so an entry that is
// itself in use counts as used. The negative control is a sibling path no process names, so a
// pass is not a reader answering yes to everything.
func TestTheUsedPathSetReadsEveryProcEntry(t *testing.T) {
	const (
		entry = "/legion/scratch/cache/ms-playwright"
		held  = entry + "/chromium-1200/chrome-linux/chrome"
		never = "/legion/scratch/cache/some-tool"
	)
	sources := []struct {
		name  string
		place func(process *fixtureProcess, path string)
	}{
		{"an open file descriptor", func(p *fixtureProcess, path string) { p.fds = []string{path} }},
		{"the working directory", func(p *fixtureProcess, path string) { p.cwd = path }},
		{"the executable", func(p *fixtureProcess, path string) { p.exe = path }},
		{"the root", func(p *fixtureProcess, path string) { p.root = path }},
		{"a mapped file", func(p *fixtureProcess, path string) { p.maps = []string{path} }},
		{"a thread's working directory", func(p *fixtureProcess, path string) { p.threads = map[int]string{p.pid + 1: path} }},
	}
	for _, source := range sources {
		t.Run(source.name, func(t *testing.T) {
			process := fixtureProcess{pid: 101}
			source.place(&process, held)
			used := readUsedPaths(writeProcFixture(t, process), os.Getpid())
			if len(used.Refused) > 0 {
				t.Fatalf("the fixture was refused: %v", used.Refused)
			}
			if len(used.paths) == 0 {
				t.Fatalf("the reader found no path at all through %s", source.name)
			}
			if !used.Covers(entry) {
				t.Errorf("Covers(%s) = false through %s, want an entry a process holds something under to count as used; the reader found %v",
					entry, source.name, used.paths)
			}
			if !used.Covers(held) {
				t.Errorf("Covers(%s) = false through %s, want a path on the entry itself to count as much as one under it", held, source.name)
			}
			if used.Covers(never) {
				t.Errorf("Covers(%s) = true through %s, want nothing: no process of the fixture names it", never, source.name)
			}
		})
	}
}

// The reader leaves its own process out. Listing a root and removing an entry both hold a handle
// under a root, so a reader that counted the trimmer's own handles would have the sweep skip
// everything it was about to delete. The control is another process holding a path the same way,
// which must count: without it a reader that found nothing at all would pass.
func TestTheUsedPathSetLeavesTheReadersOwnProcessOut(t *testing.T) {
	const (
		own   = "/legion/go/build/ab/abcdefabcdefabcdefabcdefabcdefab-a"
		other = "/legion/go/build/cd/cdefabcdefabcdefabcdefabcdefabcd-d"
	)
	used := readUsedPaths(writeProcFixture(t,
		fixtureProcess{pid: os.Getpid(), fds: []string{own}, cwd: "/legion/go/build"},
		fixtureProcess{pid: os.Getpid() + 1, fds: []string{other}},
	), os.Getpid())
	if len(used.Refused) > 0 {
		t.Fatalf("the fixture was refused: %v", used.Refused)
	}
	if used.Covers(own) {
		t.Errorf("Covers(%s) = true, want the reader's own process left out: the trimmer holds a handle on every root it lists", own)
	}
	if !used.Covers(other) {
		t.Errorf("Covers(%s) = false, want another process's handle to count; the reader found %v", other, used.paths)
	}
}

// A /proc read the kernel refuses names the process it could not read and is carried back, since
// the sweep decides on it: nothing is deleted when the first read of a sweep was refused. A
// process that exited under the pass is no refusal — every read of it answers ENOENT and its
// paths went with it — and a refused process still contributes what was read before the refusal.
//
// Both stagings are the same branch of the reader. The mode-000 directory is the one a pod
// stages, and it refuses only a reader that is not root; the file where the fd directory belongs
// refuses every reader, and is what this case runs when the test process is root.
func TestTheUsedPathSetNamesEveryProcessWhoseProcWasRefused(t *testing.T) {
	const (
		stillRead = "/legion/scratch/tmp/go-build2026/one"
		gone      = "/legion/scratch/tmp/go-build1999/two"
	)
	stagings := []struct {
		name    string
		process fixtureProcess
	}{
		{"a file where the fd directory belongs", fixtureProcess{pid: 301, cwd: stillRead, fdIsFile: true}},
	}
	if os.Geteuid() == 0 {
		t.Log("the test process is root, which reads a mode-000 directory, so only the file-where-a-directory-belongs staging runs")
	} else {
		stagings = append(stagings, struct {
			name    string
			process fixtureProcess
		}{"a directory the kernel refuses", fixtureProcess{pid: 302, cwd: stillRead, fdRefused: true}})
	}
	for _, staging := range stagings {
		t.Run(staging.name, func(t *testing.T) {
			// The third process has no entry at all, which is what the kernel shows of a process
			// that exited while the pass read it.
			used := readUsedPaths(writeProcFixture(t, staging.process, fixtureProcess{pid: 400, fds: []string{gone}}, fixtureProcess{pid: 500}), os.Getpid())
			if len(used.Refused) != 1 {
				t.Fatalf("Refused = %v, want exactly the one process this case made unreadable", used.Refused)
			}
			if pid := strconv.Itoa(staging.process.pid); !strings.Contains(used.Refused[0], pid) {
				t.Errorf("Refused[0] = %q, want the refused process %s named in it", used.Refused[0], pid)
			}
			if !used.Covers(stillRead) {
				t.Errorf("Covers(%s) = false, want what was read of the refused process before the refusal to count", stillRead)
			}
			if !used.Covers(gone) {
				t.Errorf("Covers(%s) = false, want the readable process's own handle", gone)
			}
		})
	}
}

// The age guard reads the newest modification time anywhere in an entry. A directory's own time
// changes only when its entries are added or removed, so a live build writing inside a
// `go-build` tree leaves the tree's own time as old as its creation; a guard that read that time
// would delete the cache the build is writing.
func TestTheAgeGuardReadsTheNewestTimeAnywhereInAnEntry(t *testing.T) {
	old := time.Now().Add(-48 * time.Hour)
	fresh := time.Now().Add(-time.Minute)
	dir := t.TempDir()

	tree := filepath.Join(dir, "go-build2026")
	deep := filepath.Join(tree, "ab", "cd")
	mkdirFixture(t, deep)
	inside := filepath.Join(deep, "out")
	writeAged(t, inside, fresh)
	// The directories are aged after the file is written, since writing it set each parent's own
	// time to now.
	for _, aged := range []string{deep, filepath.Join(tree, "ab"), tree} {
		if err := os.Chtimes(aged, old, old); err != nil {
			t.Fatal(err)
		}
	}
	own, err := os.Lstat(tree)
	if err != nil {
		t.Fatal(err)
	}
	if own.ModTime().After(fresh.Add(-time.Second)) {
		t.Fatalf("the tree's own time is %s, want the old one this case set: the case would prove nothing", own.ModTime())
	}
	newest, err := newestModTime(tree)
	if err != nil {
		t.Fatalf("newestModTime(%s): %v", tree, err)
	}
	if newest.Sub(fresh).Abs() > time.Second {
		t.Errorf("newestModTime(%s) = %s, want the time %s of the one fresh file deep inside it, not the tree's own %s",
			tree, newest, fresh, own.ModTime())
	}

	// A file is its own time.
	file := filepath.Join(dir, "lone")
	writeAged(t, file, fresh)
	if newest, err := newestModTime(file); err != nil || newest.Sub(fresh).Abs() > time.Second {
		t.Errorf("newestModTime(%s) = %s, %v, want the file's own time %s", file, newest, err, fresh)
	}

	// newerThan stops at the first file newer than the cutoff. The tree below holds a fresh file
	// under the first name the walk reads and a directory the walk may not read under a later
	// one, so a full walk of it fails where the early stop never reaches it. A root reader is
	// refused by nothing, which is the one case this cannot stage (claims_test.go skips the same
	// way).
	stop := filepath.Join(dir, "go-build1999")
	late := filepath.Join(stop, "9-late")
	mkdirFixture(t, late)
	writeAged(t, filepath.Join(stop, "0-early"), fresh)
	if err := os.Chtimes(stop, old, old); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().Add(-time.Hour)
	if os.Geteuid() == 0 {
		t.Log("the test process is root, which reads a mode-000 directory, so the early stop runs over a readable tree alone")
	} else {
		if err := os.Chmod(late, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(late, 0o700) })
		if _, err := newestModTime(stop); err == nil {
			t.Error("a full walk of the tree read the directory this case made unreadable, so the early stop below proves nothing")
		}
	}
	switch newer, err := newerThan(stop, cutoff); {
	case err != nil:
		t.Errorf("newerThan(%s) walked past the first fresh file: %v", stop, err)
	case !newer:
		t.Errorf("newerThan(%s, %s) = false, want the fresh file under its first name to stop the walk", stop, cutoff)
	}

	// Nothing newer than the cutoff is the answer that lets a sweep delete.
	if newer, err := newerThan(tree, time.Now()); err != nil || newer {
		t.Errorf("newerThan(%s, now) = %v, %v, want false: nothing in it was written in this second", tree, newer, err)
	}
}

// writeAged writes an empty file and gives it the time this case wants it to have.
func writeAged(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

// The kind guard reads every entry with lstat and follows no symbolic link, so what the trimmer
// deletes is what it looked at: a regular file where a cache directory belongs — what an
// interrupted `flock` leaves — is passed over and reported, and so is a link into a workspace,
// which a guard that followed links would call a directory and delete through. Each refusal
// carries the reason, since a cache root filling with such entries is why a pod stays over its
// mark.
func TestTheKindGuardReadsEveryEntryWithLstat(t *testing.T) {
	dir := t.TempDir()
	workspace := filepath.Join(dir, "workspaces", "cybermonklabs", "legion", "infra-2009")
	mkdirFixture(t, workspace)
	entries := map[string]string{}
	for _, name := range []string{"a-directory", "ms-playwright"} {
		entries[name] = filepath.Join(dir, name)
		mkdirFixture(t, entries[name])
	}
	entries["a-file"] = filepath.Join(dir, "a-file")
	writeAged(t, entries["a-file"], time.Now())
	entries["a-link"] = filepath.Join(dir, "a-link")
	if err := os.Symlink(workspace, entries["a-link"]); err != nil {
		t.Fatal(err)
	}
	entries["a-pipe"] = filepath.Join(dir, "a-pipe")
	if err := unix.Mkfifo(entries["a-pipe"], 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		entry string
		rule  EntryRule
		ok    bool
		why   []string
	}{
		{"a-directory", GoBuildTrees, true, nil},
		{"a-file", GoBuildTrees, false, []string{"go-build tree", "a directory", "a regular file"}},
		{"a-link", GoBuildTrees, false, []string{"a symbolic link", "never follows"}},
		{"a-file", GoCacheEntries, true, nil},
		{"a-directory", GoCacheEntries, true, nil},
		{"a-link", GoCacheEntries, false, []string{"a symbolic link", "never follows"}},
		{"a-pipe", GoCacheEntries, false, []string{"go build cache entry", "a named pipe"}},
		{"a-directory", ImmediateChildren, true, nil},
		{"a-file", ImmediateChildren, false, []string{"immediate child", "a directory", "a regular file"}},
		{"a-link", ImmediateChildren, false, []string{"a symbolic link", "never follows"}},
	} {
		path := entries[tc.entry]
		ok, why, err := entryKindOK(path, tc.rule)
		switch {
		case err != nil:
			t.Errorf("entryKindOK(%s, %s): %v", tc.entry, tc.rule, err)
		case ok != tc.ok:
			t.Errorf("entryKindOK(%s, %s) = %t, %q, want %t", tc.entry, tc.rule, ok, why, tc.ok)
		case ok && why != "":
			t.Errorf("entryKindOK(%s, %s) passed the entry and gave the reason %q, want none", tc.entry, tc.rule, why)
		}
		for _, want := range tc.why {
			if !strings.Contains(why, want) {
				t.Errorf("entryKindOK(%s, %s) gave the reason %q, want %q in it", tc.entry, tc.rule, why, want)
			}
		}
	}
	// The link was read and not followed: what it points at is still there, and the guard called
	// it a link although its target is a directory.
	if _, err := os.Stat(workspace); err != nil {
		t.Errorf("the workspace the link points into: %v", err)
	}
	// An entry that is not there at all is the caller's error to carry, not a reason.
	if ok, why, err := entryKindOK(filepath.Join(dir, "absent"), ImmediateChildren); err == nil || ok || why != "" {
		t.Errorf("entryKindOK over an absent entry = %t, %q, %v, want the read's own error", ok, why, err)
	}
}

// guardTree is the layout the pod case builds and the paths its processes use, one path per
// /proc entry, with one more no process touches.
type guardTree struct {
	Self   string // the test binary, which the namespace child re-enters for its processes
	Copy   string // that binary copied into the tree: the one process's exe and mapping
	Open   string // the file one process holds open
	Cwd    string // the directory one process and its threads work in
	Root   string // the directory one process chroots into
	Unused string // the negative control: a path of the tree no process of it uses
	Ready  string // where each process writes its marker once it is holding what it holds
}

// guardProcess is one process of that tree: what it does before it reports itself ready.
type guardProcess struct {
	Kind   string
	Open   string
	Cwd    string
	Root   string
	Marker string
}

// guardHold is how long a process of the tree stays alive unasked. The case kills the namespace
// child, and the kernel ends its PID namespace with it, so the hold matters only to a run that
// died without its cleanup: it is a bound on what such a run leaks, never a wait the case takes.
const guardHold = 5 * time.Minute

// guardHeldFile is the file the opener holds open for as long as it runs. It is a package
// variable because a local one is unreachable while the process sleeps, and a finaliser closing
// it would take the descriptor this case is about out of /proc.
var guardHeldFile *os.File

// A pod's processes are visible to the trimmer through every /proc entry it reads. The case
// clones a child with roleNamespaceAttr's own flags and uid mapping — the user, mount, PID and
// IPC namespaces every role generation runs in — and that child starts three processes inside
// them: one holding a file open from a working directory of the tree, one that is the test
// binary copied into the tree and executed from there, and one that chroots into a directory of
// the tree. The test then reads its own container's /proc, which is what the trimmer reads, and
// must find each of their paths: a role's agent runs in exactly those namespaces, and a guard
// that could not see into them would delete the browser a sibling role is running.
//
// The negative control is a path of the same tree no process uses: a reader that answered yes to
// everything would pass every case above and fail this one.
func TestThePodsProcessesAreVisibleThroughEveryProcEntry(t *testing.T) {
	requireRoleNamespaces(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	tree := guardTree{
		Self:   self,
		Copy:   filepath.Join(dir, "bin", "legion-volume-guard-probe"),
		Open:   filepath.Join(dir, "open", "held.bin"),
		Cwd:    filepath.Join(dir, "cwd"),
		Root:   filepath.Join(dir, "root"),
		Unused: filepath.Join(dir, "unused"),
		Ready:  filepath.Join(dir, "ready"),
	}
	for _, made := range []string{filepath.Dir(tree.Copy), filepath.Dir(tree.Open), tree.Cwd, tree.Root, tree.Unused, tree.Ready} {
		mkdirFixture(t, made)
	}
	if err := os.WriteFile(tree.Open, []byte("held"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := copyExecutable(self, tree.Copy); err != nil {
		t.Fatal(err)
	}
	plan, err := json.Marshal(tree)
	if err != nil {
		t.Fatal(err)
	}
	attr, err := roleNamespaceAttr()
	if err != nil {
		t.Fatal(err)
	}
	output := &lockedBuffer{}
	child := exec.Command(self, "-test.run=^TestVolumeGuardNamespaceHelper$")
	child.Dir = "/"
	child.Env = append(os.Environ(), volumeGuardNamespaceEnv+"="+string(plan))
	child.SysProcAttr = attr
	child.Stdout, child.Stderr = output, output
	if err := child.Start(); err != nil {
		t.Fatalf("the child in the role's namespaces: %v", err)
	}
	ended := make(chan error, 1)
	go func() { ended <- child.Wait() }()
	t.Cleanup(func() {
		_ = child.Process.Kill()
		<-ended
	})

	// Each process reports itself ready by a marker of its own; the chrooted one writes its
	// marker inside the root it took, which is that same directory from here.
	markers := []string{filepath.Join(tree.Ready, "opener"), filepath.Join(tree.Ready, "runner"), filepath.Join(tree.Root, "chrooted")}
	deadline := time.Now().Add(testwait.Bounded(t, 30*time.Second, time.Second))
	for ready := false; !ready; {
		ready = true
		for _, marker := range markers {
			if _, err := os.Stat(marker); err != nil {
				ready = false
			}
		}
		if ready {
			break
		}
		select {
		case err := <-ended:
			t.Fatalf("the child in the role's namespaces ended before its processes were ready: %v\n%s", err, output.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the three processes were not ready within the bound\n%s", output.String())
		}
		time.Sleep(testwait.Poll)
	}

	used := readUsedPaths("/proc", os.Getpid())
	if len(used.paths) == 0 {
		t.Fatalf("the reader found no path at all in this container's /proc; refused %v", used.Refused)
	}
	t.Logf("the container's /proc gave %d paths, and refused %d processes: %v", len(used.paths), len(used.Refused), used.Refused)
	for _, tc := range []struct {
		entries string
		path    string
	}{
		{"fd", tree.Open},
		{"cwd and task/<tid>/cwd", tree.Cwd},
		{"exe and maps", tree.Copy},
		{"root", tree.Root},
	} {
		if !used.Covers(tc.path) {
			t.Errorf("Covers(%s) = false, want this container's own /proc to name it through %s; refused %v", tc.path, tc.entries, used.Refused)
		}
	}
	// The thread entry is a source of its own in this pod and not a fiction of the fixtures
	// above: a task/<tid>/cwd link of this container's /proc names the directory the opener works
	// in. Go's threads share one working directory, so no process here can hold a thread cwd its
	// own cwd does not name; what this holds is that the entry exists and the kernel answers it.
	if !threadCwdNames(t, tree.Cwd) {
		t.Errorf("no task/<tid>/cwd of this container's /proc names %s, so the reader's thread entry read nothing here", tree.Cwd)
	}
	if used.Covers(tree.Unused) {
		t.Errorf("Covers(%s) = true, want nothing: no process of the tree uses it, and a reader that answers yes to everything guards nothing", tree.Unused)
	}
}

// threadCwdNames is whether any thread of any process of this container has cwd as its working
// directory, read straight from /proc rather than through the guard.
func threadCwdNames(t *testing.T, cwd string) bool {
	t.Helper()
	processes, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	for _, process := range processes {
		if _, err := strconv.Atoi(process.Name()); err != nil {
			continue
		}
		threads, err := os.ReadDir(filepath.Join("/proc", process.Name(), "task"))
		if err != nil {
			continue
		}
		for _, thread := range threads {
			if target, err := os.Readlink(filepath.Join("/proc", process.Name(), "task", thread.Name(), "cwd")); err == nil && target == cwd {
				return true
			}
		}
	}
	return false
}

// TestVolumeGuardNamespaceHelper is the child the case clones into a role's namespaces: it
// starts the three processes the case looks for and then waits to be killed with its namespace.
func TestVolumeGuardNamespaceHelper(t *testing.T) {
	encoded := os.Getenv(volumeGuardNamespaceEnv)
	if encoded == "" {
		return
	}
	var tree guardTree
	if err := json.Unmarshal([]byte(encoded), &tree); err != nil {
		fmt.Fprintln(os.Stderr, "volume-guard namespace child:", err)
		os.Exit(2)
	}
	for _, process := range []guardProcess{
		{Kind: "opener", Open: tree.Open, Cwd: tree.Cwd, Marker: filepath.Join(tree.Ready, "opener")},
		{Kind: "runner", Marker: filepath.Join(tree.Ready, "runner")},
		// The marker is written after the chroot, so its path is the one the process sees: the
		// root it took is the case's directory from outside.
		{Kind: "chrooter", Root: tree.Root, Marker: "/chrooted"},
	} {
		plan, err := json.Marshal(process)
		if err != nil {
			fmt.Fprintln(os.Stderr, "volume-guard namespace child:", err)
			os.Exit(2)
		}
		// The runner is the binary copied into the tree, so its exe and its mappings name a path
		// of the tree; the other two are the test binary where it already is.
		argv0 := tree.Self
		if process.Kind == "runner" {
			argv0 = tree.Copy
		}
		started := exec.Command(argv0, "-test.run=^TestVolumeGuardProcessHelper$")
		started.Dir = "/"
		started.Env = append(os.Environ(), volumeGuardProcessEnv+"="+string(plan))
		started.Stdout, started.Stderr = os.Stdout, os.Stderr
		if err := started.Start(); err != nil {
			fmt.Fprintln(os.Stderr, "volume-guard namespace child:", process.Kind, err)
			os.Exit(2)
		}
	}
	time.Sleep(guardHold)
	os.Exit(0)
}

// TestVolumeGuardProcessHelper is one process inside those namespaces: it takes hold of the one
// thing its kind is for, reports itself ready and waits.
func TestVolumeGuardProcessHelper(t *testing.T) {
	encoded := os.Getenv(volumeGuardProcessEnv)
	if encoded == "" {
		return
	}
	var plan guardProcess
	if err := json.Unmarshal([]byte(encoded), &plan); err != nil {
		fmt.Fprintln(os.Stderr, "volume-guard process:", err)
		os.Exit(2)
	}
	fail := func(err error) {
		fmt.Fprintln(os.Stderr, "volume-guard process:", plan.Kind, err)
		os.Exit(2)
	}
	switch plan.Kind {
	case "opener":
		if err := os.Chdir(plan.Cwd); err != nil {
			fail(err)
		}
		file, err := os.Open(plan.Open)
		if err != nil {
			fail(err)
		}
		guardHeldFile = file
	case "runner":
		// Being this process is the whole of it: this binary is the copy under the tree, so the
		// kernel's exe link and its file mappings name a path of the tree, and it holds no
		// descriptor on that path at all — the Chromium case.
	case "chrooter":
		// The namespaces are the role's, and this process is root in their user namespace, which
		// is what lets it chroot at all.
		if err := unix.Chroot(plan.Root); err != nil {
			fail(err)
		}
	default:
		fail(fmt.Errorf("no kind named %q", plan.Kind))
	}
	if err := os.WriteFile(plan.Marker, nil, 0o644); err != nil {
		fail(err)
	}
	time.Sleep(guardHold)
	os.Exit(0)
}
