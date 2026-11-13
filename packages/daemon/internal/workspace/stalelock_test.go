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
	want := "swept only the git directory of the workspace " + located.Dir + "'s clone: the git worktree entry " +
		own + ", which " + filepath.Join(located.Dir, ".git") + " names, cannot be read: " + gitdir + " is a fifo"
	if len(lines) != 2 || lines[1] != want {
		t.Fatalf("the sweep wrote:\n%s\nwant a removal line, then:\n%s", strings.Join(lines, "\n"), want)
	}
	if strings.Contains(strings.Join(lines, "\n"), sibling) {
		t.Fatalf("the sweep wrote a line naming the sibling entry it skips:\n%s", strings.Join(lines, "\n"))
	}
}

// When no entry's gitdir names the workspace and its .git names no entry it can take (case B of
// gitWorktreeEntries' table), or names a sibling's entry whose gitdir names that sibling (case D),
// the init container's sweep removes the git directory's locks, keeps every entry's, writes one
// line saying why and fails nothing. The .git only chooses that line: the sibling it names keeps
// its lock.
func TestRemoveStaleGitLocksSaysWhyNoEntryIsTheWorkspaces(t *testing.T) {
	for _, row := range []struct {
		name    string
		pointer func(sibling string) string
		why     func(dir, sibling string) string
	}{
		{
			name:    "a .git that names no entry (B)",
			pointer: func(string) string { return "ref: refs/heads/main\n" },
			why: func(dir, _ string) string {
				return "no git worktree entry's gitdir names the workspace " + dir + ", and its .git, which would name its entry, names none: " +
					dir + "/.git names no git worktree: \"ref: refs/heads/main\\n\""
			},
		},
		{
			name:    "a .git that names a sibling's entry (D)",
			pointer: func(sibling string) string { return "gitdir: " + sibling + "\n" },
			why: func(dir, sibling string) string {
				return "no git worktree entry's gitdir names the workspace " + dir + ", not even that of the entry " + sibling + ", which its .git names"
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
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
			// The workspace's own entry has no gitdir, so no entry's names the workspace.
			write(filepath.Join(sibling, "gitdir"), filepath.Join(filepath.Dir(located.Dir), "infra-2", ".git")+"\n")
			write(filepath.Join(located.Dir, ".git"), row.pointer(sibling))
			stale := filepath.Join(gitDir, "index.lock")
			kept := []string{filepath.Join(own, "index.lock"), filepath.Join(sibling, "index.lock")}
			for _, path := range append([]string{stale}, kept...) {
				write(path, "stale")
			}

			var lines []string
			if err := RemoveStaleGitLocks(located, time.Now(), func(line string) { lines = append(lines, line) }); err != nil {
				t.Fatalf("the sweep failed the provisioning: %v", err)
			}
			if _, err := os.Lstat(stale); !os.IsNotExist(err) {
				t.Fatalf("the sweep left %s (%v); its lines:\n%s", stale, err, strings.Join(lines, "\n"))
			}
			for _, path := range kept {
				if _, err := os.Lstat(path); err != nil {
					t.Fatalf("the sweep removed %s, under an entry whose gitdir names no workspace of its own (%v); its lines:\n%s", path, err, strings.Join(lines, "\n"))
				}
			}
			want := "swept only the git directory of the workspace " + located.Dir + "'s clone: " + row.why(located.Dir, sibling)
			if len(lines) != 2 || lines[1] != want {
				t.Fatalf("the sweep wrote:\n%s\nwant a removal line, then:\n%s", strings.Join(lines, "\n"), want)
			}
		})
	}
}

// The init container sweeps before it provisions anything, so on a fresh disk there is no clone
// and no workspace: the last row of gitWorktreeEntries' table, which has no locks and writes no
// line, rather than a line every new pod would write about a workspace not yet made.
func TestRemoveStaleGitLocksSaysNothingOfADiskWithNoClone(t *testing.T) {
	located, err := Location(t.TempDir(), ghrepo.MustParse("acme/widgets"), "infra-1")
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	if err := RemoveStaleGitLocks(located, time.Now(), func(line string) { lines = append(lines, line) }); err != nil {
		t.Fatalf("the sweep failed the provisioning: %v", err)
	}
	if len(lines) != 0 {
		t.Fatalf("the sweep of a disk with no clone wrote:\n%s", strings.Join(lines, "\n"))
	}
}
