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
// one text in two implementations — this package's staleGitLockNoteText and the Legion tools'
// own, which packages/pi-shared/src/git-lock.test.ts holds to this source by reading it — and the
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

func TestStaleGitLockNoteNamesTheLockAndTellsTheRoleToRunTheCommandAgain(t *testing.T) {
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
		lines := strings.Split(StaleGitLockNote(output, now), "\n")
		if len(lines) != 2 {
			t.Fatalf("the note is %q, want one line for each of the two locks", lines)
		}
		for i, lock := range []string{index, ref} {
			if !strings.HasPrefix(lines[i], "git lock "+lock+" was last written "+ageWords+" ago") {
				t.Errorf("line %d is %q, want it to name %s and its age", i, lines[i], lock)
			}
		}
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
}

func TestStaleGitLockNoteNamesHowLongTheSweepTakes(t *testing.T) {
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
	note, _ := noteForOneLock(t, time.Now())
	// Under tmux there is no pod and no launcher, so this sentence is the whole of what a lock
	// costs a host: it is the one the rewrite kept word for word.
	tmux := "Under the tmux runtime nothing removes it: remove it by hand only once no git or jj process on the host is using the repository."
	if !strings.Contains(note, tmux) {
		t.Errorf("the note is %q, want it to keep %q", note, tmux)
	}
}
