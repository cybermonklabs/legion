package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// unreadableOwnEntry lays out a workspace whose .git names its own worktree entry, and that entry
// with a gitdir naming the workspace that this process may not open: case C of
// gitWorktreeEntries' table, whose cause is the open's own permission denied. It answers the
// workspace, the entry, and the error lockGitWorktree and removeGitWorktree answer for it.
func unreadableOwnEntry(t *testing.T) (located Workspace, entry, want string) {
	t.Helper()
	located, err := Location(t.TempDir(), ghrepo.MustParse("acme/widgets"), "infra-1")
	if err != nil {
		t.Fatal(err)
	}
	entry = filepath.Join(located.Clone, ".git", "worktrees", "infra-1")
	for _, dir := range []string{entry, located.Dir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	pointer := filepath.Join(located.Dir, ".git")
	if err := os.WriteFile(pointer, []byte("gitdir: "+entry+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitdir := filepath.Join(entry, "gitdir")
	if err := os.WriteFile(gitdir, []byte(pointer+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(gitdir, 0); err != nil {
		t.Fatal(err)
	}
	if file, err := os.Open(gitdir); err == nil {
		_ = file.Close()
		t.Fatalf("%s is readable at mode 000, so this row would prove nothing (is the test running as root?)", gitdir)
	}
	return located, entry, "the git worktree entry " + entry + ", which " + pointer + " names, cannot be read: open " + gitdir + ": permission denied"
}

// The tmux runtime's provisioning locks the workspace's git worktree entry with no stale-lock
// sweep before it to say anything about that entry, so an entry the workspace's .git names whose
// gitdir cannot be read (case C) is the lock's error: locking nothing and saying nothing would
// leave the entry to a bare `git worktree prune`. No `locked` file is written.
func TestLockGitWorktreeRefusesItsEntryWhenItsGitdirCannotBeRead(t *testing.T) {
	located, entry, want := unreadableOwnEntry(t)
	err := lockGitWorktree(located.Clone, located.Dir)
	if err == nil || err.Error() != want {
		t.Fatalf("lockGitWorktree answered %v, want %q", err, want)
	}
	if _, err := os.Lstat(filepath.Join(entry, "locked")); !os.IsNotExist(err) {
		t.Fatalf("lockGitWorktree wrote a lock into an entry it could not read (%v)", err)
	}
}

// removeGitWorktree answers the same entry with the same error, and leaves the entry where it is:
// it is the workspace's, and removing nothing must not read as removing it.
func TestRemoveGitWorktreeRefusesItsEntryWhenItsGitdirCannotBeRead(t *testing.T) {
	located, entry, want := unreadableOwnEntry(t)
	err := removeGitWorktree(located.Clone, located.Dir)
	if err == nil || err.Error() != want {
		t.Fatalf("removeGitWorktree answered %v, want %q", err, want)
	}
	if _, err := os.Lstat(entry); err != nil {
		t.Fatalf("removeGitWorktree removed an entry it could not read (%v)", err)
	}
}

// The provision of a workspace that already exists reads the workspace's .git, the pointer to its
// worktree entry, before it restores a missing entry (restoreGitWorktree). Every process working
// the workspace can write that file, and under the tmux runtime the daemon makes this read while
// it holds the repository's store lock, so a pointer that is a fifo nobody writes must be answered,
// not waited on: a blocking open there stalls every later provision of the repository's issues.
func TestRestoreGitWorktreeAnswersAPointerThatIsAFifo(t *testing.T) {
	located, err := Location(t.TempDir(), ghrepo.MustParse("acme/widgets"), "infra-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{filepath.Join(located.Clone, ".git", "worktrees"), located.Dir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	pointer := filepath.Join(located.Dir, ".git")
	if err := syscall.Mkfifo(pointer, 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- restoreGitWorktree(context.Background(), NewRunner(testTimeout, testTools(t)), located, func(string) {})
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("restoreGitWorktree answered nil for a pointer that is a fifo, want the reason it holds no pointer git wrote")
		}
	case <-time.After(5 * time.Second):
		// Open the fifo for writing so the blocked read returns and the test's goroutine ends.
		if writer, openErr := os.OpenFile(pointer, os.O_WRONLY, 0); openErr == nil {
			_ = writer.Close()
		}
		<-done
		t.Fatalf("restoreGitWorktree was still reading %s after 5s: a pointer that is a fifo blocks the provision", pointer)
	}
}

// removeTree answers a path whose parent is a fifo rather than waiting on it: os.RemoveAll opens
// the parent of a path it cannot unlink outright with a plain open, which a fifo nobody writes
// holds for ever, and every caller of removeTree removes something under the shared clone's
// .git/worktrees, which a role can swap for a fifo.
func TestRemoveTreeAnswersAParentThatIsAFifo(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "worktrees")
	if err := syscall.Mkfifo(parent, 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- removeTree(filepath.Join(parent, "infra-1")) }()
	select {
	case err := <-done:
		if !errors.Is(err, syscall.ENOTDIR) {
			t.Fatalf("removeTree under a fifo answered %v, want ENOTDIR", err)
		}
	case <-time.After(5 * time.Second):
		// Open the fifo for writing so the blocked open returns and the test's goroutine ends.
		if writer, openErr := os.OpenFile(parent, os.O_WRONLY, 0); openErr == nil {
			_ = writer.Close()
		}
		<-done
		t.Fatalf("removeTree was still opening %s after 5s: a parent that is a fifo blocks the removal", parent)
	}
	if info, err := os.Lstat(parent); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("the fifo %s is no longer there as it was (%v, %v)", parent, info, err)
	}
}

// worktreesToFifo is a runner for restoreGitWorktree alone: jj answers a working-copy parent, and
// `git read-tree` swaps the shared clone's .git/worktrees for a fifo, as a role working the clone
// can, and fails, which sends the restore down its cleanup of the temporary entry it built there.
type worktreesToFifo struct {
	worktrees string
}

func (worktreesToFifo) Timeout() time.Duration { return testTimeout }

func (r worktreesToFifo) Run(_ context.Context, command Command) (Result, error) {
	if command.Argv[0] == "jj" {
		return Result{Stdout: strings.Repeat("a", 40) + "\n"}, nil
	}
	if !slices.Contains(command.Argv, "read-tree") {
		return Result{}, errors.New("worktreesToFifo runs jj log and git read-tree alone")
	}
	if err := os.Rename(r.worktrees, r.worktrees+".aside"); err != nil {
		return Result{}, err
	}
	if err := syscall.Mkfifo(r.worktrees, 0o644); err != nil {
		return Result{}, err
	}
	return Result{ExitCode: 1, Stderr: "fatal: not a git repository"}, nil
}

// A restore that fails after it built its temporary entry removes that entry, under the clone's
// .git/worktrees, with the store lock still held under the tmux runtime. The window is a whole
// `git read-tree`, and a role that swaps .git/worktrees for a fifo inside it must have the restore
// answer its failure, not wait on the fifo while every later provision of the repository waits on
// the lock.
func TestRestoreGitWorktreeAnswersWorktreesSwappedForAFifo(t *testing.T) {
	located, err := Location(t.TempDir(), ghrepo.MustParse("acme/widgets"), "infra-1")
	if err != nil {
		t.Fatal(err)
	}
	worktrees := filepath.Join(located.Clone, ".git", "worktrees")
	for _, dir := range []string{worktrees, located.Dir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(located.Dir, ".git"), []byte("gitdir: "+filepath.Join(worktrees, "infra-1")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(worktrees)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- restoreGitWorktree(context.Background(), worktreesToFifo{worktrees: resolved}, located, func(string) {})
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "read-tree") {
			t.Fatalf("restoreGitWorktree answered %v, want the failure of its git read-tree", err)
		}
	case <-time.After(5 * time.Second):
		// Open the fifo for writing so the blocked open returns and the test's goroutine ends.
		if writer, openErr := os.OpenFile(resolved, os.O_WRONLY, 0); openErr == nil {
			_ = writer.Close()
		}
		<-done
		t.Fatalf("restoreGitWorktree was still removing its temporary entry under %s after 5s: a .git/worktrees swapped for a fifo blocks the provision", resolved)
	}
}
