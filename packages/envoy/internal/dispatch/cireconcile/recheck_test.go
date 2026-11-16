package cireconcile

import (
	"testing"
	"time"
)

// An answer that the App is not installed on a repository is held for AbsentAppRecheck and no
// longer, so a repository the App is installed on later is read. The pass before the answer
// lapses asks GitHub nothing about the repository and reads no head of it; the first pass
// after it asks again, finds the App and settles the head. A pass that held the answer for
// good would leave such a repository unread until the process restarted.
func TestARepositoryTheAppIsInstalledOnLaterIsReadOnceTheAnswerLapses(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	// The clock is this test's own, read by the pass on the test's goroutine: the answer is
	// held for an hour, which no test waits out.
	now := time.Now()
	h.reconciler.now = func() time.Time { return now }
	h.issueInRepository(rowRepository)

	h.pass()

	line := h.passLine()
	if got := number(t, line, "repositories_without_app"); got != 1 {
		t.Fatalf("the first pass reports %d repositories without the App, want 1: %v", got, line)
	}
	if asked := h.github.countPath(installationPath(rowRepository)); asked != 1 {
		t.Fatalf("GitHub was asked %d times whether the App is installed on %s, want once", asked, rowRepository)
	}

	// A person installs the App on the repository, which has an open pull request.
	h.listHead()
	h.github.setCheckRuns(gitHubRepository, headSHA,
		checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-05T03:01:00Z"),
	)
	settlements := h.subscribe()
	now = now.Add(AbsentAppRecheck - time.Second)

	h.pass()

	line = h.passLine()
	if asked := h.github.countPath(installationPath(rowRepository)); asked != 1 {
		t.Fatalf("GitHub was asked %d times about %s before the answer lapsed, want the one time the first pass asked", asked, rowRepository)
	}
	if got := number(t, line, "repositories_without_app"); got != 1 {
		t.Errorf("the pass before the answer lapsed reports %d repositories without the App, want 1: %v", got, line)
	}
	if got := number(t, line, "heads"); got != 0 {
		t.Errorf("the pass before the answer lapsed read %d heads, want none: %v", got, line)
	}

	now = now.Add(time.Second)

	h.pass()

	settled := h.settlement(settlements, "after the pass that asked GitHub again")
	if settled.SHA != headSHA || settled.Passed.Count != 1 {
		t.Fatalf("settlement = %+v, want the head settled on its one passing check", settled)
	}
	line = h.passLine()
	if asked := h.github.countPath(installationPath(rowRepository)); asked != 2 {
		t.Errorf("GitHub was asked %d times about %s, want twice: once a held answer", asked, rowRepository)
	}
	if got := number(t, line, "repositories_without_app"); got != 0 {
		t.Errorf("the pass that found the App reports %d repositories without it, want none: %v", got, line)
	}
	if got := number(t, line, "heads"); got != 1 {
		t.Errorf("the pass that found the App read %d heads, want the repository's one: %v", got, line)
	}
	if held := len(h.reconciler.absent); held != 0 {
		t.Errorf("the Reconciler still holds %d answers that the App is not installed, want none: %v", held, h.reconciler.absent)
	}
}

// The answer held about a repository is forgotten with the cycle that no longer covers it, as
// the ETags of a head the order has left are: what the Reconciler keeps follows the
// repositories mappings and issues name now, not every one an issue has ever named.
func TestTheAnswerHeldAboutARepositoryTheOrderHasLeftIsForgotten(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.issueInRepository(uninstalledRow)
	h.listHead()

	h.pass()

	if _, held := h.reconciler.absent[uninstalledRow]; !held {
		t.Fatalf("the Reconciler holds no answer about %s after the pass that asked: %v", uninstalledRow, h.reconciler.absent)
	}

	// The issue names another repository, so nothing names this one any longer.
	if _, err := h.pool.Exec(h.ctx, `update issues set repo = $2 where repo = $1`, uninstalledRow, rowRepository); err != nil {
		t.Fatalf("move the issue off %s: %v", uninstalledRow, err)
	}

	h.pass()

	if held := len(h.reconciler.absent); held != 0 {
		t.Fatalf("the Reconciler holds %d answers after the cycle that no longer covers %s, want none: %v", held, uninstalledRow, h.reconciler.absent)
	}
	if got := number(t, h.passLine(), "repositories_without_app"); got != 0 {
		t.Errorf("the pass reports %d repositories without the App, want none: %v", got, h.passLine())
	}
}
