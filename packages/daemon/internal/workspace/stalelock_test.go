package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// The init container's sweep (RemoveStaleGitLocks) reads the clone through the same walk as the
// launcher's (GitLocksOf), so an entry whose gitdir it cannot read is decided the same way: a
// sibling's is passed over without a line and fails nothing, even one it may not open at all,
// and the workspace's own — the entry its .git names — is a line naming the entry and the cause,
// its locks left, while every other lock of the clone is removed and the provisioning goes on.
func TestRemoveStaleGitLocksSweepsPastAnUnreadableWorktreeEntry(t *testing.T) {
	located, err := Location(t.TempDir(), ghrepo.MustParse("acme/widgets"), "infra-1")
	if err != nil {
		t.Fatal(err)
	}
	gitDir := filepath.Join(located.Clone, ".git")
	own := filepath.Join(gitDir, "worktrees", "infra-1")
	sibling := filepath.Join(gitDir, "worktrees", "infra-2")
	for _, dir := range []string{own, sibling, located.Dir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(located.Dir, ".git"), "gitdir: "+own+"\n")
	gitdir := filepath.Join(own, "gitdir")
	if err := syscall.Mkfifo(gitdir, 0o600); err != nil {
		t.Fatal(err)
	}
	siblingGitdir := filepath.Join(sibling, "gitdir")
	write(siblingGitdir, filepath.Join(filepath.Dir(located.Dir), "infra-2", ".git")+"\n")
	if err := os.Chmod(siblingGitdir, 0); err != nil {
		t.Fatal(err)
	}
	if file, err := os.Open(siblingGitdir); err == nil {
		_ = file.Close()
		t.Fatalf("%s is readable at mode 000, so this row would prove nothing (is the test running as root?)", siblingGitdir)
	}
	stale := filepath.Join(gitDir, "index.lock")
	entryLock := filepath.Join(own, "index.lock")
	write(stale, "stale")
	write(entryLock, "stale")

	var lines []string
	if err := RemoveStaleGitLocks(located, time.Now(), func(line string) { lines = append(lines, line) }); err != nil {
		t.Fatalf("the sweep failed the provisioning: %v", err)
	}
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Fatalf("the sweep left %s (%v); its lines:\n%s", stale, err, strings.Join(lines, "\n"))
	}
	if _, err := os.Lstat(entryLock); err != nil {
		t.Fatalf("the sweep removed %s, under an entry it could not read (%v); its lines:\n%s", entryLock, err, strings.Join(lines, "\n"))
	}
	want := "left the git locks of the workspace " + located.Dir + "'s own git worktree entry where they are: the git worktree entry " +
		own + ", which " + filepath.Join(located.Dir, ".git") + " names, cannot be read: " + gitdir + " is a fifo"
	if len(lines) != 2 || lines[1] != want {
		t.Fatalf("the sweep wrote:\n%s\nwant a removal line, then:\n%s", strings.Join(lines, "\n"), want)
	}
	if strings.Contains(strings.Join(lines, "\n"), sibling) {
		t.Fatalf("the sweep wrote a line naming the sibling entry it skips:\n%s", strings.Join(lines, "\n"))
	}
}
