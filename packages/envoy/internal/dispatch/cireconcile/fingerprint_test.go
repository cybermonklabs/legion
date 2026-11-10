package cireconcile

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cistore"
	"github.com/sjawhar/envoy/internal/contracts"
)

// countingWriter counts what a pass costs the CI record store and hands every call to the
// writer it wraps, which is the listener's own bucket. The two record calls are what the
// fingerprint exists to save: each one is a read of the head's record from NATS, an unmarshal
// of it and a hash of what it holds, and a pass of 833 heads of 39 checks made some 30,000 of
// them whether or not GitHub had anything new. HasRecord is the one existence read the record
// guard makes of a head the pass holds something for (Reconciler.kept).
type countingWriter struct {
	Writer

	mu sync.Mutex
	// updates is every Record and RecordSuite the pass made, and exists every HasRecord.
	updates int
	exists  int
}

func (w *countingWriter) Record(ctx context.Context, observation contracts.CIObservation) (bool, error) {
	w.count(&w.updates)
	return w.Writer.Record(ctx, observation)
}

func (w *countingWriter) RecordSuite(ctx context.Context, observation contracts.CIObservation) (bool, error) {
	w.count(&w.updates)
	return w.Writer.RecordSuite(ctx, observation)
}

func (w *countingWriter) HasRecord(owner, repo, number, sha string) (bool, error) {
	w.count(&w.exists)
	return w.Writer.HasRecord(owner, repo, number, sha)
}

func (w *countingWriter) count(of *int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	*of++
}

// calls is what the bucket has been asked for since the last forget.
func (w *countingWriter) calls() (updates, exists int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.updates, w.exists
}

// forget empties the counts, so what a test reads afterwards is what the pass after it cost and
// nothing else, as the stand-in's own forgetRequests does for the requests it was sent.
func (w *countingWriter) forget() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.updates, w.exists = 0, 0
}

// countWrites makes the pass bind a writer that counts its calls, as failWrites makes one whose
// writes fail: every pass binds through Open, so the wrapping goes around Open.
func (h *harness) countWrites() *countingWriter {
	h.t.Helper()
	counted := &countingWriter{}
	bind := h.reconciler.Open
	h.reconciler.Open = func() (Writer, error) {
		bound, err := bind()
		if err != nil {
			return nil, err
		}
		counted.Writer = bound
		return counted, nil
	}
	return counted
}

// writesPerHead is what one head of unchangingHeads costs a pass that writes it: its two
// completed check runs and the completed check suite they hang under.
const writesPerHead = 3

// unchangingHeads is count open pull requests of the mapped repository, each at a head commit of
// its own with two completed check runs under one completed check suite, and nothing one query
// cannot answer whole. Nothing about them changes between two passes, so a pass over them after
// the first has nothing to write.
func unchangingHeads(h *harness, count int64) []fakePull {
	h.t.Helper()
	pulls := heads(count)
	h.listHeads(pulls...)
	for _, pull := range pulls {
		h.github.setCheckSuites(gitHubRepository, pull.sha,
			atHead(gitHubRepository, pull.sha, checkSuite(900, "completed", "success", otherApp, "2026-11-10T05:53:02Z")),
		)
		h.github.setCheckRuns(gitHubRepository, pull.sha,
			atHead(gitHubRepository, pull.sha, checkRun(100+pull.number, "ci-ok", "completed", "success", otherApp, "2026-11-10T05:53:00Z")),
			atHead(gitHubRepository, pull.sha, checkRun(200+pull.number, "build", "completed", "success", otherApp, "2026-11-10T05:53:01Z")),
		)
	}
	return pulls
}

// dropRecord deletes one head's record from the bucket, which is what the bucket's seven-day
// TTL does to the record of a head nothing has written for a week, and what a person deleting a
// key does.
func (h *harness) dropRecord(pull fakePull) {
	h.t.Helper()
	kv, err := h.bus.JS().KeyValue(bus.CIStateBucket)
	if err != nil {
		h.t.Fatalf("open the CI bucket: %v", err)
	}
	key := cistore.Key(gitHubOwner, gitHubName, strconv.FormatInt(pull.number, 10), pull.sha)
	if err := kv.Delete(key); err != nil {
		h.t.Fatalf("delete the head's record: %v", err)
	}
}

// A second pass over heads nothing has changed at writes nothing at all. A query carries no
// ETag, so the pass keeps a fingerprint of the objects it mapped for each head instead, and a
// head whose next answer fingerprints the same costs the one existence read the record guard
// makes and no update: in production that is 833 heads of about 39 checks each, some 30,000
// reads, unmarshals and hashes of records nothing had changed, inside a five-minute bound that
// already holds about 178 seconds of GitHub's own time. It is k6.
func TestASecondPassOverUnchangedHeadsWritesNothing(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	pulls := unchangingHeads(h, 5)
	counted := h.countWrites()

	h.pass()

	updates, exists := counted.calls()
	if want := len(pulls) * writesPerHead; updates != want {
		t.Fatalf("the first pass made %d updates, want the %d observations of its %d heads", updates, want, len(pulls))
	}
	if exists != 0 {
		t.Errorf("the first pass made %d existence reads, want none: it held nothing of any of these heads", exists)
	}
	counted.forget()

	h.pass()

	updates, exists = counted.calls()
	if updates != 0 {
		t.Errorf("the second pass made %d updates, want none: GitHub answered the very objects the first pass recorded", updates)
	}
	if exists > len(pulls) {
		t.Errorf("the second pass made %d existence reads, want at most the one per head the record guard makes, %d", exists, len(pulls))
	}
	line := h.passLine()
	if got, want := number(t, line, "heads_unchanged"), len(pulls); got != want {
		t.Errorf("the second pass's heads_unchanged = %d, want all %d of them: %v", got, want, line)
	}
	if got, want := number(t, line, "heads"), len(pulls); got != want {
		t.Errorf("the second pass's heads = %d, want the %d it read: a head written nothing is still a head read: %v", got, want, line)
	}
	if got := number(t, line, "records"); got != 0 {
		t.Errorf("the second pass changed %d records, want none: %v", got, line)
	}
	// What the pass wrote once is still there to be read, which is what makes writing it again
	// needless rather than skipped.
	for _, pull := range pulls {
		state := h.recordOf(pull, pull.number)
		if state == nil {
			t.Fatalf("pull request %d has no record after the second pass", pull.number)
		}
		for _, check := range []string{"ci-ok", "build"} {
			if _, held := state.Checks[check]; !held {
				t.Errorf("pull request %d's record holds %v, want its %s run", pull.number, state.Checks, check)
			}
		}
	}
}

// News of GitHub's own goes through the fingerprint: a head the pass wrote at one pass and
// GitHub has something more for at the next -- a run that has finished since, and the suite
// finished with it -- fingerprints differently and is written. A fingerprint that hid this
// would be worse than no fingerprint at all: the head would keep the record of a pass before
// the checks it is waiting on ended, which is the very failure this pass exists to mend.
func TestAHeadGitHubHasMoreForIsWrittenThroughTheFingerprint(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.listHead()
	h.github.setCheckSuites(gitHubRepository, headSHA,
		checkSuite(900, "in_progress", "", otherApp, "2026-11-10T05:53:01Z"),
	)
	h.github.setCheckRuns(gitHubRepository, headSHA,
		checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-10T05:53:00Z"),
	)
	counted := h.countWrites()

	h.pass()

	counted.forget()
	// The second run ends, and the suite ends with it.
	h.github.setCheckSuites(gitHubRepository, headSHA,
		checkSuite(900, "completed", "success", otherApp, "2026-11-10T05:55:01Z"),
	)
	h.github.setCheckRuns(gitHubRepository, headSHA,
		checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-10T05:53:00Z"),
		checkRun(2, "build", "completed", "success", otherApp, "2026-11-10T05:55:00Z"),
	)

	h.pass()

	if updates, _ := counted.calls(); updates != writesPerHead {
		t.Errorf("the pass over the head GitHub had more for made %d updates, want the %d observations it now holds", updates, writesPerHead)
	}
	state, _, ok := h.record()
	if !ok {
		t.Fatal("the head has no record after the second pass")
	}
	if _, held := state.Checks["build"]; !held {
		t.Fatalf("the record holds %v, want the run that finished between the passes", state.Checks)
	}
	if len(state.Suites) != 1 {
		t.Errorf("the record holds %v, want the suite that finished between the passes", state.Suites)
	}
	line := h.passLine()
	if got := number(t, line, "heads_unchanged"); got != 0 {
		t.Errorf("the second pass counted %d unchanged heads, want none: GitHub answered a run and a suite more: %v", got, line)
	}
	if got := number(t, line, "records"); got != 1 {
		t.Errorf("the second pass changed %d records, want the one head's: %v", got, line)
	}
}

// A write that did not land is not hidden behind a fingerprint, exactly as it is not hidden
// behind an ETag: the pass whose write of a run failed keeps no fingerprint for that head, so
// the next pass writes every observation of it again and the run reaches the record. Were the
// fingerprint kept, the next answer would fingerprint the same for ever and the run the record
// lacks would never reach it.
func TestAFailedWriteDropsTheHeadsFingerprint(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.listHead()
	h.github.setCheckRuns(gitHubRepository, headSHA,
		checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-10T05:53:00Z"),
	)
	// The pass's first write of a check run fails, which is NATS refusing it or the connection
	// going while the pass ran.
	h.failWrites(1)
	counted := h.countWrites()

	h.pass()

	if !h.logged("ci reconcile did not record a check run") {
		t.Fatalf("the failed write was not named at WARN:\n%s", h.logs.String())
	}
	if state, _, ok := h.record(); ok {
		if _, held := state.Checks["ci-ok"]; held {
			t.Fatalf("the record holds the run whose write failed: %v", state.Checks)
		}
	}
	counted.forget()

	h.pass()

	updates, _ := counted.calls()
	if updates == 0 {
		t.Fatal("the pass after a failed write wrote nothing: the head kept a fingerprint of observations the record does not hold")
	}
	state, _, ok := h.record()
	if !ok {
		t.Fatal("the head has no record after the pass that wrote it again")
	}
	if _, held := state.Checks["ci-ok"]; !held {
		t.Fatalf("the record holds %v, want the run the failed write left out", state.Checks)
	}
	line := h.passLine()
	if got := number(t, line, "heads_unchanged"); got != 0 {
		t.Errorf("the pass after a failed write counted %d unchanged heads, want none: %v", got, line)
	}
}

// A head whose record has gone is written again, fingerprint or no fingerprint: the bucket's
// seven-day TTL drops the record of a head nothing has written for a week, and a fingerprint
// standing for observations no record holds would leave that head with no record at all for as
// long as GitHub's answer about it does not change -- which, for a head whose checks have all
// finished, is for ever.
func TestAHeadWhoseRecordHasGoneIsWrittenAgain(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.listHead()
	h.github.setCheckRuns(gitHubRepository, headSHA,
		checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-10T05:53:00Z"),
	)
	counted := h.countWrites()

	h.pass()

	// The settlement's own writes of the record are over before the record is dropped, so what
	// the next pass finds is a head with no record rather than one being written.
	h.settledRecord()
	h.dropRecord(fakePull{number: 7, sha: headSHA})
	counted.forget()

	h.pass()

	updates, exists := counted.calls()
	if exists != 1 {
		t.Errorf("the pass made %d existence reads of the one head it holds a fingerprint for, want 1", exists)
	}
	if updates == 0 {
		t.Fatal("the pass over a head whose record had gone wrote nothing: the fingerprint stood for a record that is not there")
	}
	state, _, ok := h.record()
	if !ok {
		t.Fatal("the head has no record after the pass that found it gone")
	}
	if _, held := state.Checks["ci-ok"]; !held {
		t.Fatalf("the record written again holds %v, want the head's run", state.Checks)
	}
}

// A cycle that no longer lists a head forgets its fingerprint, as it forgets its ETags: a head
// that is listed again later -- a pull request reopened at the same commit -- is written again
// rather than passed over on the strength of what a pass read before it closed, and the
// fingerprints the Reconciler holds follow the open pull requests there are.
func TestACycleThatNoLongerListsAHeadForgetsItsFingerprint(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	pulls := unchangingHeads(h, 2)
	counted := h.countWrites()

	h.pass()

	// The second pull request is closed, so the cycle this pass walks never lists its head and
	// closes having forgotten it.
	h.listHeads(pulls[0])
	counted.forget()

	h.pass()

	if updates, _ := counted.calls(); updates != 0 {
		t.Fatalf("the pass over the one head left made %d updates, want none: nothing changed at it", updates)
	}
	// It is reopened at the same commit, which is a head GitHub answers exactly what it
	// answered before for.
	h.listHeads(pulls...)
	counted.forget()

	h.pass()

	updates, _ := counted.calls()
	if updates != writesPerHead {
		t.Errorf("the pass made %d updates, want the %d observations of the head the cycle had forgotten: the head still listed must not be written and the one listed again must",
			updates, writesPerHead)
	}
	line := h.passLine()
	if got := number(t, line, "heads_unchanged"); got != 1 {
		t.Errorf("the pass counted %d unchanged heads, want the one it had held a fingerprint for throughout: %v", got, line)
	}
}

// A head a pass's own bound cut the cycle off before keeps its fingerprint, as it keeps its
// ETags: a cut is the pass ending rather than a head that failed. The pass that takes the cycle
// up reads the pages the cut pass did not reach and closes the cycle, and the heads of the page
// the cut pass did read are still in the cycle's own set, so the cycle keeps what it holds of
// them and the pass after it writes nothing for any of the heads.
func TestACutPassKeepsTheFingerprintsOfTheHeadsItRead(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	// Twenty-five open pull requests are two pages of a query: twenty and five.
	pulls := unchangingHeads(h, 25)
	h.reconciler.PassTimeout = 2 * time.Second
	// GitHub never answers the second page within what the pass has left, so the pass's own
	// bound ends it there, after the twenty heads of the first page.
	h.github.stallQuery(gitHubRepository, 1, time.Minute)
	counted := h.countWrites()

	h.pass()

	line := h.passLine()
	if line["deadline_reached"] != true {
		t.Fatalf("the pass reports deadline_reached = %v, want true: its own bound ended it at the stalled page: %v", line["deadline_reached"], line)
	}
	if got, want := number(t, line, "heads"), pullsPerQuery; got != want {
		t.Fatalf("the cut pass read %d heads, want the %d of the page it got: %v", got, want, line)
	}
	// GitHub answers again, and the pass that takes the cycle up has as long as it needs.
	h.github.stallQuery(gitHubRepository, 1, 0)
	h.reconciler.PassTimeout = 0
	counted.forget()

	h.pass()

	line = h.passLine()
	if line["cycle_complete"] != true {
		t.Fatalf("the pass that took the cycle up reports cycle_complete = %v, want true: %v", line["cycle_complete"], line)
	}
	if updates, _ := counted.calls(); updates != (len(pulls)-pullsPerQuery)*writesPerHead {
		t.Fatalf("the pass that took the cycle up made %d updates, want the %d observations of the %d heads the cut pass never reached",
			updates, (len(pulls)-pullsPerQuery)*writesPerHead, len(pulls)-pullsPerQuery)
	}
	counted.forget()

	h.pass()

	updates, _ := counted.calls()
	if updates != 0 {
		t.Errorf("the pass after the cycle made %d updates, want none: the heads the cut pass read kept their fingerprints through the cut and through the cycle closing", updates)
	}
	line = h.passLine()
	if got := number(t, line, "heads_unchanged"); got != len(pulls) {
		t.Errorf("the pass counted %d unchanged heads, want all %d: %v", got, len(pulls), line)
	}
}
