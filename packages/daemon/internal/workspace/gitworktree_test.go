package workspace

import (
	"os"
	"path/filepath"
	"testing"

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
