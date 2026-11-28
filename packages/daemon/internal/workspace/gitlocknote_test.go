package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// What a role reads when a git lock fails its command (StaleGitLockNote, jjgit.go). The note is
// one text in two implementations — this package's staleGitLockNoteText, which is the data file
// stale-git-lock-note.txt, and the Legion tools' own, which
// packages/pi-shared/src/git-lock.test.ts holds to that same file by reading it — and the
// rows here are what that text must say. The numbers in it belong to internal/launcher, whose
// TestStaleGitLockNoteCarriesTheLaunchersSweepNumbers renders this note for a planted lock and
// asserts gitLockGuard and gitLockInterval in it: this package cannot read them, since the
// launcher imports it.
//
// Every sentence has a role's failure behind it. The wording before INFRA-3098 said no process
// removes such a lock while the pod runs, told the role not to remove it, and offered replacing
// the issue's pod — which costs the issue its pod and its disk — as the remedy; two roles read it
// on 2026-11-11 and stopped to ask a person what to do. The launcher of an issue pod's roles
// container now sweeps those locks, so the note names the sweep, tells the role to wait and run
// its command again, and ends with the one thing left to do when the lock outlives the sweep.

// noteAge is how long ago each planted lock here was last written, and ageWords is how the note
// prints it: a Go duration rounded to the second.
const (
	noteAge  = 31*time.Minute + 4*time.Second
	ageWords = "31m4s"
)

// plantedLock is an empty git lock file at name under dir, last written noteAge before now, which
// is what makes it a lock StaleGitLockNote reports: a lock the note names must still be on disk.
func plantedLock(t *testing.T, dir, name string, now time.Time) string {
	t.Helper()
	lock := filepath.Join(dir, name)
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	written := now.Add(-noteAge)
	if err := os.Chtimes(lock, written, written); err != nil {
		t.Fatal(err)
	}
	return lock
}

// gitLockFailure is how git's own lockfile API names a lock it could not take, and jjLockFailure
// how jj's git backend does: the two the note is read out of (gitLockReports).
func gitLockFailure(lock string) string {
	return fmt.Sprintf("error: cannot lock ref 'refs/heads/main': Unable to create '%s': File exists.", lock)
}

func jjLockFailure(lock string) string {
	return fmt.Sprintf("2: The lock for resource '%s' could not be obtained after 1.00s after 16 attempt(s). The lockfile at '%s' might need manual deletion.",
		strings.TrimSuffix(lock, ".lock"), lock)
}

// noteForOneLock is the note for a single planted lock, with that lock's path: what most rows
// here read.
func noteForOneLock(t *testing.T, now time.Time) (string, string) {
	t.Helper()
	lock := plantedLock(t, t.TempDir(), "index.lock", now)
	return StaleGitLockNote(gitLockFailure(lock), now), lock
}

// wantNoteNaming fails t unless note is one line for each of locks, in order, each naming its lock
// and its age (noteAge).
func wantNoteNaming(t *testing.T, note string, locks ...string) {
	t.Helper()
	lines := strings.Split(note, "\n")
	if len(lines) != len(locks) {
		t.Fatalf("the note is %q, want one line for each of %q", lines, locks)
	}
	for i, lock := range locks {
		if !strings.HasPrefix(lines[i], "git lock "+lock+" was last written "+ageWords+" ago") {
			t.Errorf("line %d is %q, want it to name %s and its age", i, lines[i], lock)
		}
	}
}

func TestStaleGitLockNoteNamesTheLockAndTellsTheRoleToRunTheCommandAgain(t *testing.T) {
	t.Parallel()
	now := time.Now()
	note, lock := noteForOneLock(t, now)
	opening := "git lock " + lock + " was last written " + ageWords + " ago and blocks this command."
	if !strings.HasPrefix(note, opening) {
		t.Errorf("the note is %q, want it to open %q", note, opening)
	}
	if want := "Run the command again in about 16 minutes."; !strings.Contains(note, want) {
		t.Errorf("the note is %q, want it to say %q", note, want)
	}

	t.Run("one line for each lock the output names, each named once", func(t *testing.T) {
		dir := t.TempDir()
		index := plantedLock(t, dir, "index.lock", now)
		ref := plantedLock(t, dir, "main.lock", now)
		// jj's backend and the git it ran both report the index lock: one line all the same.
		output := strings.Join([]string{jjLockFailure(index), gitLockFailure(index), gitLockFailure(ref)}, "\n")
		wantNoteNaming(t, StaleGitLockNote(output, now), index, ref)
	})

	t.Run("nothing for output that names no lock, or names one already gone", func(t *testing.T) {
		gone := filepath.Join(t.TempDir(), "index.lock")
		if note := StaleGitLockNote(`Error: There is no jj repo in "."`, now); note != "" {
			t.Errorf("the note for output naming no lock is %q, want none", note)
		}
		if note := StaleGitLockNote(gitLockFailure(gone), now); note != "" {
			t.Errorf("the note for a lock already gone is %q, want none", note)
		}
	})

	// git and jj print the lock's path between single quotes and do not escape it, so a path that
	// holds a quote of its own (a TMPDIR or a checkout named so) still names its lock. Two rules
	// read it, and pi-shared's git-lock.test.ts holds its copy to the same rows: the path ends at
	// the first closing text after its opening, so two locks one report names on one line are two
	// paths, not one path running from the first opening to the last closing; and a path stays on
	// its line, so an opening cut off at the end of a line takes nothing from the next line's report.
	t.Run("a lock whose path holds a single quote", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "tmp-sq'x", "it's here")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		index := plantedLock(t, dir, "index.lock", now)
		ref := plantedLock(t, dir, "main.lock", now)
		for _, tc := range []struct {
			name   string
			output string
			locks  []string
		}{
			{"git's report", gitLockFailure(index), []string{index}},
			{"jj's report", jjLockFailure(index), []string{index}},
			{"both reports of one lock, two lines", jjLockFailure(index) + "\n" + gitLockFailure(index), []string{index}},
			{"two locks git names on one line", gitLockFailure(index) + " " + gitLockFailure(ref), []string{index, ref}},
			{"two locks jj names on one line", jjLockFailure(index) + " " + jjLockFailure(ref), []string{index, ref}},
			{"a cut-off git opening before git's report", "fatal: Unable to create '/cut off\n" + gitLockFailure(index), []string{index}},
			{"a cut-off jj opening before jj's report", "Error: The lockfile at '/cut off\n" + jjLockFailure(index), []string{index}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				wantNoteNaming(t, StaleGitLockNote(tc.output, now), tc.locks...)
			})
		}
	})
}

func TestStaleGitLockNoteNamesHowLongTheSweepTakes(t *testing.T) {
	t.Parallel()
	note, _ := noteForOneLock(t, time.Now())
	for _, want := range []string{
		"The launcher of the issue's pod removes a git lock that no process of the pod holds open and that is more than 15 minutes old, and it looks again every minute.",
		"Run the command again in about 16 minutes.",
	} {
		if !strings.Contains(note, want) {
			t.Errorf("the note is %q, want it to say %q", note, want)
		}
	}
}

func TestStaleGitLockNoteEndsWithTellingTheArchitect(t *testing.T) {
	t.Parallel()
	now := time.Now()
	// The last sentence is what keeps the note true in a pod whose image predates the launcher's
	// sweep, where nothing removes the lock and the wait the note names passes with it still
	// there: the role has one thing left to do, and it is not to remove the file.
	last := "If the lock is still there after that time, tell the issue's architect."
	note, _ := noteForOneLock(t, now)
	if !strings.HasSuffix(note, last) {
		t.Errorf("the note is %q, want it to end %q", note, last)
	}

	t.Run("every line of a note naming more than one lock", func(t *testing.T) {
		dir := t.TempDir()
		output := gitLockFailure(plantedLock(t, dir, "index.lock", now)) + "\n" +
			gitLockFailure(plantedLock(t, dir, "main.lock", now))
		for i, line := range strings.Split(StaleGitLockNote(output, now), "\n") {
			if !strings.HasSuffix(line, last) {
				t.Errorf("line %d is %q, want it to end %q", i, line, last)
			}
		}
	})
}

func TestStaleGitLockNoteOffersNoPodReplacement(t *testing.T) {
	t.Parallel()
	note, _ := noteForOneLock(t, time.Now())
	// Replacing the issue's pod takes the pod's disk with it, and every role in it: a remedy the
	// note must never offer now that the launcher sweeps the lock. Nor may it tell the role to
	// keep the lock where it is: the role's own answer is to run the command again.
	for _, forbidden := range []string{
		"replacing the issue's pod",
		"replace the pod",
		"replace the issue's pod",
		"until the pod is replaced",
		"a new pod",
		"do not remove it",
		"leave it where it is",
	} {
		if strings.Contains(strings.ToLower(note), forbidden) {
			t.Errorf("the note is %q, want nothing of %q in it", note, forbidden)
		}
	}
}

func TestStaleGitLockNoteKeepsItsTmuxSentence(t *testing.T) {
	t.Parallel()
	note, _ := noteForOneLock(t, time.Now())
	// Under tmux there is no pod and no launcher, so this sentence is the whole of what a lock
	// costs a host: it is the one the rewrite kept word for word.
	tmux := "Under the tmux runtime nothing removes it: remove it by hand only once no git or jj process on the host is using the repository."
	if !strings.Contains(note, tmux) {
		t.Errorf("the note is %q, want it to keep %q", note, tmux)
	}
}
