package cireconcile

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
)

// unfinished is a run GitHub has not finished, as its listing answers it: completed_at stays null
// until a run ends, so the run is timed by its start.
func unfinished(run object) object {
	delete(run, "completed_at")
	return run
}

// unansweringWriter is a CI record writer whose writes that carry one named check fail, as a
// write fails while the bus does not answer, until failures of them have failed. Every other call
// goes to the writer it wraps, which is the listener's own bucket.
type unansweringWriter struct {
	Writer
	check string

	mu       sync.Mutex
	failures int
}

func (w *unansweringWriter) RecordRuns(ctx context.Context, observations []contracts.CIObservation) (bool, error) {
	w.mu.Lock()
	failing := w.failures > 0 && slices.ContainsFunc(observations, func(observation contracts.CIObservation) bool {
		return observation.CheckName == w.check
	})
	if failing {
		w.failures--
	}
	w.mu.Unlock()
	if failing {
		return false, errors.New("cistore: the KV call did not answer: context deadline exceeded")
	}
	return w.Writer.RecordRuns(ctx, observations)
}

// A pass whose write of a head's failed run did not land publishes no settlement. GitHub holds two
// finished runs that no webhook carried, one passed and one failed, and the bus does not answer
// the write that carries the failed one. Had the passed run landed alone, the record would hold
// one finished check and nothing else, and the listener would publish a settlement claiming the
// head green. The head's runs are one write, so neither lands; the next pass, with the bus
// answering, writes both and the head settles with its failure.
func TestAPassWhoseWriteOfOneFinishedRunFailedSettlesNothing(t *testing.T) {
	// Not parallel: it holds a 2 s quiet window (noSettlement), which load could make it miss.
	h := newHarness(t, reviewerAppID)
	h.seedProjectDefault(rowRepository)
	h.listHead()
	h.github.setCheckRuns(gitHubRepository, headSHA,
		checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-22T00:04:00Z"),
		checkRun(2, "lint", "completed", "failure", otherApp, "2026-11-22T00:04:10Z"),
	)
	h.answerNoRunWriteCarrying("lint", 1)
	settlements := h.subscribe()

	h.pass()

	h.noSettlement(settlements, "after a pass that could not write the failed run")
	if state, _, ok := h.record(); ok && len(state.Checks) != 0 {
		t.Fatalf("the record holds %+v; a write that did not land wrote part of the head", state.Checks)
	}

	h.pass()

	settled := h.settlement(settlements, "after the pass whose write landed")
	if settled.Passed.Count != 1 || settled.Failed.Count != 1 {
		t.Fatalf("settlement = %+v, want ci-ok passed and lint failed", settled)
	}
}

// answerNoRunWriteCarrying makes the pass bind a writer whose first failures writes of runs that
// carry check fail (unansweringWriter). Every pass binds its writer through Open, so the wrapping
// goes around Open.
func (h *harness) answerNoRunWriteCarrying(check string, failures int) {
	h.t.Helper()
	unanswering := &unansweringWriter{check: check, failures: failures}
	bind := h.reconciler.Open
	h.reconciler.Open = func() (Writer, error) {
		bound, err := bind()
		if err != nil {
			return nil, err
		}
		unanswering.Writer = bound
		return unanswering, nil
	}
}

// A pass whose write of a head's runs failed writes nothing more of that head. The head settled
// on its first run; GitHub then holds a queued run no webhook carried, and a suite of another App
// that has completed. The bus does not answer the write of the runs. Had the pass gone on to write
// the completed suite, that write would re-arm the settled record, which holds the first run
// alone, and the listener would settle the head again while GitHub runs the queued one. The next
// pass, with the bus answering, writes the queued run, and the head waits for it.
func TestAPassWhoseRunWriteFailedWritesNothingMoreOfTheHead(t *testing.T) {
	// Not parallel: it holds two 2 s quiet windows (noSettlement), which load could make it miss.
	h := newHarness(t, reviewerAppID)
	h.seedProjectDefault(rowRepository)
	h.listHead()
	settlements := h.subscribe()
	changes := checkRun(1, "changes", "completed", "success", otherApp, "2026-11-22T00:02:05Z")
	h.post("check_run", webhookBody("check_run", changes), "d1")
	if settled := h.settlement(settlements, "on the webhook"); settled.Passed.Count != 1 {
		t.Fatalf("settlement = %+v, want the head settled on changes", settled)
	}
	h.settledRecord()

	h.github.setCheckRuns(gitHubRepository, headSHA,
		changes,
		unfinished(checkRun(2, "test", "queued", "", otherApp, "2026-11-22T00:02:06Z")),
	)
	h.github.setCheckSuites(gitHubRepository, headSHA,
		checkSuite(901, "completed", "success", 4321, "2026-11-22T00:02:07Z"),
	)
	h.answerNoRunWriteCarrying("test", 1)

	h.pass()

	h.noSettlement(settlements, "after a pass whose write of the runs failed")
	state, _, ok := h.record()
	if !ok {
		t.Fatal("the head's record is gone after the pass")
	}
	if _, held := state.Suites["901"]; held {
		t.Fatalf("the record holds the suite %+v; the pass wrote more of a head whose runs it could not write", state.Suites)
	}

	h.pass()

	h.noSettlement(settlements, "while GitHub runs test")
	state, _, ok = h.record()
	if !ok {
		t.Fatal("the head's record is gone after the second pass")
	}
	if got := state.Checks["test"]; got.CheckRunID != 2 || got.Status != "queued" {
		t.Fatalf("the record holds test as %+v, want run 2 queued", got)
	}
}

// A pass over a head whose webhooks the listener lost publishes no settlement while GitHub still
// runs checks at that head. The listener took the head's first run and settled it. Then its
// intake failed for a minute (a NATS move: every record answered GitHub 503), and in that minute
// GitHub created the jobs that waited on the first run: one it skipped, one queued and one
// running. No webhook of theirs reached the record. The pass reads GitHub's listing of the head
// and writes all three. A pass that wrote only the skipped run would leave the record with two
// finished checks and nothing unfinished, and the listener would publish a settlement claiming
// the head's CI done while two of its jobs had not started. Once GitHub finishes them, the next
// pass records the ends and the head settles again, superseding the first settlement.
func TestAPassOverRunsNoWebhookCarriedSettlesNothingWhileGitHubStillRunsSome(t *testing.T) {
	// Not parallel: it holds a 2 s quiet window (noSettlement), which load could make it miss.
	h := newHarness(t, reviewerAppID)
	h.seedProjectDefault(rowRepository)
	h.listHead()
	settlements := h.subscribe()

	changes := checkRun(1, "changes", "completed", "success", otherApp, "2026-11-22T00:02:05Z")
	h.post("check_run", webhookBody("check_run", changes), "d1")
	if settled := h.settlement(settlements, "on the webhook before the gap"); settled.Passed.Count != 1 {
		t.Fatalf("settlement = %+v, want the head settled on changes", settled)
	}
	h.settledRecord()

	skipped := checkRun(2, "envoy-go", "completed", "skipped", otherApp, "2026-11-22T00:02:06Z")
	h.github.setCheckRuns(gitHubRepository, headSHA,
		changes,
		skipped,
		unfinished(checkRun(3, "test", "queued", "", otherApp, "2026-11-22T00:02:06Z")),
		unfinished(checkRun(4, "lint", "in_progress", "", otherApp, "2026-11-22T00:02:07Z")),
	)
	h.github.setCheckSuites(gitHubRepository, headSHA,
		checkSuite(900, "in_progress", "", otherApp, "2026-11-22T00:02:07Z"),
	)

	h.pass()

	h.noSettlement(settlements, "while GitHub still runs test and lint at the head")
	state, _, ok := h.record()
	if !ok {
		t.Fatal("the head has no record after the pass")
	}
	if got := state.Checks["test"]; got.CheckRunID != 3 || got.Status != "queued" {
		t.Fatalf("the record holds test as %+v, want run 3 queued: %+v", got, state.Checks)
	}
	if got := state.Checks["lint"]; got.CheckRunID != 4 || got.Status != "in_progress" {
		t.Fatalf("the record holds lint as %+v, want run 4 in_progress: %+v", got, state.Checks)
	}
	if got := state.Checks["envoy-go"]; got.CheckRunID != 2 || got.Conclusion != "skipped" {
		t.Fatalf("the record holds envoy-go as %+v, want run 2 skipped: %+v", got, state.Checks)
	}
	if _, held := state.Suites["900"]; held {
		t.Fatalf("the record holds the running suite %+v; a suite is recorded once it completes", state.Suites)
	}

	h.github.setCheckRuns(gitHubRepository, headSHA,
		changes,
		skipped,
		checkRun(3, "test", "completed", "success", otherApp, "2026-11-22T00:09:00Z"),
		checkRun(4, "lint", "completed", "failure", otherApp, "2026-11-22T00:09:30Z"),
	)
	h.github.setCheckSuites(gitHubRepository, headSHA,
		checkSuite(900, "completed", "failure", otherApp, "2026-11-22T00:09:30Z"),
	)

	h.pass()

	settled := h.settlement(settlements, "once GitHub finished test and lint")
	if settled.Passed.Count != 2 || settled.Failed.Count != 1 || settled.Skipped.Count != 1 ||
		settled.Running.Count != 0 || settled.Queued.Count != 0 {
		t.Fatalf("settlement = %+v, want changes and test passed, lint failed, envoy-go skipped", settled)
	}
	if settled.SupersededSettlement != "true" {
		t.Fatalf("settlement = %+v, want it to supersede the settlement before the gap", settled)
	}
}

// A pass writes an unfinished run through the record's own ordering, so it never takes a head
// back from what a webhook recorded later than GitHub's listing: a queued run never replaces the
// same run completed, and never a newer run of its name. A newer run of a name that GitHub has
// not finished re-arms the head, which settles again once that run ends and supersedes the
// settlement before it.
func TestAPassWritesAnUnfinishedRunOnlyWhereItIsGitHubsNewest(t *testing.T) {
	// Not parallel: each row holds a 2 s quiet window (noSettlement), which load could make it miss.
	t.Run("a queued run never replaces the same run completed", func(t *testing.T) {
		h := newHarness(t, reviewerAppID)
		h.seedProjectDefault(rowRepository)
		h.listHead()
		settlements := h.subscribe()
		done := checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-22T00:05:00Z")
		h.post("check_run", webhookBody("check_run", done), "d1")
		h.settlement(settlements, "on the completion webhook")
		_, before := h.settledRecord()

		// A listing older than the webhook: GitHub's answer still shows the run queued.
		h.github.setCheckRuns(gitHubRepository, headSHA,
			unfinished(checkRun(1, "ci-ok", "queued", "", otherApp, "2026-11-22T00:04:00Z")),
		)

		h.pass()

		h.noSettlement(settlements, "after a pass over a stale listing")
		state, after, ok := h.record()
		if !ok {
			t.Fatal("the record is gone after the pass")
		}
		if after != before {
			t.Fatalf("the record's revision moved from %d to %d: the queued run was written over the completed one", before, after)
		}
		if got := state.Checks["ci-ok"]; got.Status != "completed" || !state.SettledEmitted {
			t.Fatalf("the record holds ci-ok as %+v, settled %v; want the completed run and the head settled", got, state.SettledEmitted)
		}
	})

	t.Run("a queued run never replaces a newer run of its name", func(t *testing.T) {
		h := newHarness(t, reviewerAppID)
		h.seedProjectDefault(rowRepository)
		h.listHead()
		settlements := h.subscribe()
		newer := checkRun(5, "ci-ok", "completed", "success", otherApp, "2026-11-22T00:05:00Z")
		h.post("check_run", webhookBody("check_run", newer), "d1")
		h.settlement(settlements, "on the newer run's webhook")
		_, before := h.settledRecord()

		// Every attempt of the name: an earlier one GitHub left queued, and the newer one.
		h.github.setCheckRuns(gitHubRepository, headSHA,
			unfinished(checkRun(3, "ci-ok", "queued", "", otherApp, "2026-11-22T00:01:00Z")),
			newer,
		)

		h.pass()

		h.noSettlement(settlements, "after a pass over an older attempt left queued")
		state, after, ok := h.record()
		if !ok {
			t.Fatal("the record is gone after the pass")
		}
		if after != before {
			t.Fatalf("the record's revision moved from %d to %d: the older attempt was written over the newer run", before, after)
		}
		if got := state.Checks["ci-ok"]; got.CheckRunID != 5 || got.Status != "completed" {
			t.Fatalf("the record holds ci-ok as %+v, want run 5 completed", got)
		}
	})

	t.Run("a re-run GitHub has not finished re-arms the head and it settles again superseding", func(t *testing.T) {
		h := newHarness(t, reviewerAppID)
		h.seedProjectDefault(rowRepository)
		h.listHead()
		settlements := h.subscribe()
		first := checkRun(1, "ci-ok", "completed", "failure", otherApp, "2026-11-22T00:05:00Z")
		h.post("check_run", webhookBody("check_run", first), "d1")
		if settled := h.settlement(settlements, "on the first run"); settled.Failed.Count != 1 {
			t.Fatalf("settlement = %+v, want ci-ok failed", settled)
		}
		h.settledRecord()

		// GitHub re-runs ci-ok; the re-run's webhooks never reached the listener.
		h.github.setCheckRuns(gitHubRepository, headSHA,
			first,
			unfinished(checkRun(7, "ci-ok", "in_progress", "", otherApp, "2026-11-22T00:06:00Z")),
		)

		h.pass()

		h.noSettlement(settlements, "while the re-run runs")
		state, _, ok := h.record()
		if !ok {
			t.Fatal("the record is gone after the pass")
		}
		if got := state.Checks["ci-ok"]; got.CheckRunID != 7 || got.Status != "in_progress" || state.SettledEmitted {
			t.Fatalf("the record holds ci-ok as %+v, settled %v; want run 7 in_progress and the head re-armed", got, state.SettledEmitted)
		}

		h.github.setCheckRuns(gitHubRepository, headSHA,
			first,
			checkRun(7, "ci-ok", "completed", "success", otherApp, "2026-11-22T00:08:00Z"),
		)

		h.pass()

		settled := h.settlement(settlements, "once the re-run passed")
		if settled.Passed.Count != 1 || settled.Failed.Count != 0 || settled.SupersededSettlement != "true" {
			t.Fatalf("settlement = %+v, want ci-ok passed, superseding the failed settlement", settled)
		}
		if len(settled.CheckRuns) != 1 || settled.CheckRuns[0].ID != 7 {
			t.Fatalf("settlement names %+v, want the re-run, run 7", settled.CheckRuns)
		}
	})
}
