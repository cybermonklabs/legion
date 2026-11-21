package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/testwait"
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
// leave the entry to a bare `git worktree prune`. No `locked` file is written. Every later
// provision of the workspace fails the same way and nothing repairs the entry, so the error says
// what does, as restoreGitWorktree's refusal of a pointer outside the clone says it.
func TestLockGitWorktreeRefusesItsEntryWhenItsGitdirCannotBeRead(t *testing.T) {
	located, entry, want := unreadableOwnEntry(t)
	want += "; provisioning cannot lock it. Remove the workspace so the next provisioning adds it again"
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
// The answer is waited for with no span of its own (testwait.Answer), so a read that waits on the
// fifo fails the row by name at the test binary's deadline, and a slow provision passes.
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
	err = testwait.Answer(t, "restoreGitWorktree over the fifo "+pointer, done)
	if err == nil {
		t.Fatalf("restoreGitWorktree answered nil for a pointer that is a fifo, want the reason it holds no pointer git wrote")
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
// the lock. A removal that waits on the fifo fails the row by name at the test binary's deadline
// (testwait.Answer).
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
	err = testwait.Answer(t, "restoreGitWorktree with "+resolved+" swapped for a fifo", done)
	if err == nil || !strings.Contains(err.Error(), "read-tree") {
		t.Fatalf("restoreGitWorktree answered %v, want the failure of its git read-tree", err)
	}
}

// gitDirAway is a runner for restoreGitWorktree alone: jj answers a working-copy parent, after
// swapping the shared clone's .git for a symlink to away when away is set, as a role working the
// clone can while jj runs; `git read-tree` succeeds and writes nothing.
type gitDirAway struct {
	clone, away string
}

func (gitDirAway) Timeout() time.Duration { return testTimeout }

func (r gitDirAway) Run(_ context.Context, command Command) (Result, error) {
	switch {
	case command.Argv[0] == "jj":
		if r.away != "" {
			gitDir := filepath.Join(r.clone, ".git")
			if err := os.Rename(gitDir, gitDir+".aside"); err != nil {
				return Result{}, err
			}
			if err := os.Symlink(r.away, gitDir); err != nil {
				return Result{}, err
			}
		}
		return Result{Stdout: strings.Repeat("a", 40) + "\n"}, nil
	case slices.Contains(command.Argv, "read-tree"):
		return Result{}, nil
	}
	return Result{}, errors.New("gitDirAway runs jj log and git read-tree alone")
}

// A restore makes a directory and three files under the shared clone's .git/worktrees, through
// .git, by path, with the store lock held under the tmux runtime. A role can make that .git a
// symlink to a directory of its choosing, before the restore or while its jj runs, and the restore
// must then refuse, naming the link, and make nothing where it points.
func TestRestoreGitWorktreeRefusesAGitDirectorySymlinkedAway(t *testing.T) {
	for _, row := range []struct {
		name    string
		whileJJ bool
	}{
		{name: "planted before the restore"},
		{name: "swapped in while jj runs", whileJJ: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			located, err := Location(t.TempDir(), ghrepo.MustParse("acme/widgets"), "infra-1")
			if err != nil {
				t.Fatal(err)
			}
			gitDir := filepath.Join(located.Clone, ".git")
			away := filepath.Join(t.TempDir(), "away")
			for _, dir := range []string{filepath.Join(gitDir, "worktrees"), away, located.Dir} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(located.Dir, ".git"), []byte("gitdir: "+filepath.Join(gitDir, "worktrees", "infra-1")+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			run := gitDirAway{clone: located.Clone}
			if row.whileJJ {
				run.away = away
			} else {
				if err := os.Mkdir(filepath.Join(away, "worktrees"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(gitDir, gitDir+".aside"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(away, gitDir); err != nil {
					t.Fatal(err)
				}
			}
			err = restoreGitWorktree(context.Background(), run, located, func(string) {})
			if want := gitDir + " is a symlink"; err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("restoreGitWorktree answered %v, want a refusal saying %q", err, want)
			}
			// What was under away before the restore: away itself and, when the link was planted
			// first, the worktrees directory it holds. Anything else the restore made there.
			planted := []string{away}
			if !row.whileJJ {
				planted = append(planted, filepath.Join(away, "worktrees"))
			}
			var made []string
			if walkErr := filepath.WalkDir(away, func(path string, _ os.DirEntry, err error) error {
				if err == nil && !slices.Contains(planted, path) {
					made = append(made, path)
				}
				return err
			}); walkErr != nil {
				t.Fatal(walkErr)
			}
			if len(made) != 0 {
				t.Fatalf("the restore made %v under %s, where the clone's .git was linked", made, away)
			}
		})
	}
}

// excludeClone lays out the git directory of a shared clone down to .git/info, which
// excludeCodegraphDirectory writes under, and answers the clone and its exclude file.
func excludeClone(t *testing.T) (clone, exclude string) {
	t.Helper()
	clone = filepath.Join(t.TempDir(), "widgets")
	if err := os.MkdirAll(filepath.Join(clone, ".git", "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	return clone, filepath.Join(clone, ".git", "info", "exclude")
}

// excludeWithin runs excludeCodegraphDirectory on clone and answers its error, waited for with no
// span of its own (testwait.Answer): the call holds the repository's store lock under the tmux
// runtime, so a call that waits on a fifo holds every later provision of the repository, and
// fails the row by name at the test binary's deadline, while a call that is merely slow passes.
func excludeWithin(t *testing.T, clone string) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- excludeCodegraphDirectory(clone) }()
	return testwait.Answer(t, "excludeCodegraphDirectory("+clone+")", done)
}

// A clone's .git/info/exclude that is a fifo is refused, naming it, rather than waited on: in the
// open of a read when nobody holds it, in the read when a writer holds it open and writes nothing,
// and in the open of the write after. The fifo is left as it was.
func TestExcludeCodegraphDirectoryRefusesAFifo(t *testing.T) {
	for _, row := range []struct {
		name   string
		writer bool
	}{
		{name: "nobody holds it"},
		{name: "a writer holds it open", writer: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			clone, exclude := excludeClone(t)
			if err := syscall.Mkfifo(exclude, 0o600); err != nil {
				t.Fatal(err)
			}
			var held *os.File
			if row.writer {
				var err error
				if held, err = os.OpenFile(exclude, os.O_RDWR, 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = held.Close() })
			}
			err := excludeWithin(t, clone)
			if want := exclude + " is a fifo"; err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("excludeCodegraphDirectory answered %v, want a refusal saying %q", err, want)
			}
			if info, err := os.Lstat(exclude); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
				t.Fatalf("the fifo %s is no longer there as it was (%v, %v)", exclude, info, err)
			}
		})
	}
}

// A clone's .git/info/exclude that is a symlink, or an info directory that is one, is refused,
// naming the link, rather than followed: a role that can write the clone could otherwise have
// provisioning append `.codegraph/` to any file the daemon can write. The file the link names is
// left as it was.
func TestExcludeCodegraphDirectoryRefusesASymlink(t *testing.T) {
	for _, row := range []struct {
		name string
		link func(t *testing.T, clone, outside string) string
	}{
		{name: "the exclude file", link: func(t *testing.T, clone, outside string) string {
			link := filepath.Join(clone, ".git", "info", "exclude")
			if err := os.Symlink(filepath.Join(outside, "exclude"), link); err != nil {
				t.Fatal(err)
			}
			return link
		}},
		{name: "its info directory", link: func(t *testing.T, clone, outside string) string {
			link := filepath.Join(clone, ".git", "info")
			if err := os.Remove(link); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, link); err != nil {
				t.Fatal(err)
			}
			return link
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			clone, _ := excludeClone(t)
			outside := t.TempDir()
			target := filepath.Join(outside, "exclude")
			const before = "# a file outside the clone\n"
			if err := os.WriteFile(target, []byte(before), 0o600); err != nil {
				t.Fatal(err)
			}
			link := row.link(t, clone, outside)
			err := excludeWithin(t, clone)
			if want := link + " is a symlink"; err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("excludeCodegraphDirectory answered %v, want a refusal saying %q", err, want)
			}
			if body, err := os.ReadFile(target); err != nil || string(body) != before {
				t.Fatalf("%s, outside the clone, now holds %q (%v), want %q", target, body, err, before)
			}
		})
	}
}

// A clone's .git/info/exclude larger than excludeLimit is refused, naming it and the bound,
// rather than read whole, and is left as it was.
func TestExcludeCodegraphDirectoryRefusesAFileLargerThanItsBound(t *testing.T) {
	clone, exclude := excludeClone(t)
	if err := os.WriteFile(exclude, []byte(strings.Repeat("a", excludeLimit+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	err := excludeWithin(t, clone)
	if want := fmt.Sprintf("%s is larger than %d bytes", exclude, excludeLimit); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("excludeCodegraphDirectory answered %v, want a refusal saying %q", err, want)
	}
	if info, err := os.Stat(exclude); err != nil || info.Size() != excludeLimit+1 {
		t.Fatalf("%s changed (%v, %v)", exclude, info, err)
	}
}

// An ordinary exclude file is handled as it always was: made at 0600 when missing, in an info
// directory made at 0700 when that is missing too; `.codegraph/` appended on a line of its own,
// after a newline the file lacked; and nothing written when a line already is `.codegraph/`.
func TestExcludeCodegraphDirectoryAppendsToAnOrdinaryFile(t *testing.T) {
	for _, row := range []struct {
		name    string
		plant   func(t *testing.T, exclude string)
		want    string
		infoNew bool
	}{
		{name: "no info directory", plant: func(t *testing.T, exclude string) {
			if err := os.Remove(filepath.Dir(exclude)); err != nil {
				t.Fatal(err)
			}
		}, want: ".codegraph/\n", infoNew: true},
		{name: "no exclude file", plant: func(*testing.T, string) {}, want: ".codegraph/\n"},
		{name: "a last line with no newline", plant: func(t *testing.T, exclude string) {
			if err := os.WriteFile(exclude, []byte("*.log\nbuild"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, want: "*.log\nbuild\n.codegraph/\n"},
		{name: "the line already there", plant: func(t *testing.T, exclude string) {
			if err := os.WriteFile(exclude, []byte("*.log\n.codegraph/\nbuild\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, want: "*.log\n.codegraph/\nbuild\n"},
	} {
		t.Run(row.name, func(t *testing.T) {
			clone, exclude := excludeClone(t)
			row.plant(t, exclude)
			for range 2 {
				if err := excludeWithin(t, clone); err != nil {
					t.Fatalf("excludeCodegraphDirectory: %v", err)
				}
			}
			body, err := os.ReadFile(exclude)
			if err != nil || string(body) != row.want {
				t.Fatalf("%s holds %q (%v), want %q", exclude, body, err, row.want)
			}
			if row.name == "no exclude file" || row.infoNew {
				if info, err := os.Stat(exclude); err != nil || info.Mode().Perm() != 0o600 {
					t.Errorf("%s was made as %v (%v), want mode 0600", exclude, info, err)
				}
			}
			if row.infoNew {
				if info, err := os.Stat(filepath.Dir(exclude)); err != nil || info.Mode().Perm() != 0o700 {
					t.Errorf("%s was made as %v (%v), want mode 0700", filepath.Dir(exclude), info, err)
				}
			}
		})
	}
}
