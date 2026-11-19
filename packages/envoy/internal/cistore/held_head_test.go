package cistore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
)

const (
	// heldLine and publishedLine are the messages of the summary loop's two lines.
	heldLine      = "checks held a head"
	publishedLine = "checks settlement published"
	// heldBound is how long after its last event a held record is named: the spec's ten minutes.
	heldBound = 10 * time.Minute
	// heldDebounce is the summarizer's debounce in the held tests, well inside the bound.
	heldDebounce = 20 * time.Millisecond
)

// suiteObservation is a check_suite observation of h as the webhook maps it, with its App and the
// run count the event gave (nil for none).
func suiteObservation(h head, id, appID, status, conclusion, observedAt string, count *int) contracts.CIObservation {
	return contracts.CIObservation{
		Owner: h.owner, Repo: h.repo, Number: h.number, SHA: h.sha,
		SuiteID: id, AppID: appID, Status: status, Conclusion: conclusion, ObservedAt: observedAt,
		CheckRunCount: count,
	}
}

// cachedAsStored waits until the cache holds h's record as the bucket does, and returns it.
func cachedAsStored(t *testing.T, s *Store, h head) State {
	t.Helper()
	stored := getState(t, s, h.owner, h.repo, h.number, h.sha)
	waitCached(t, s, h.key(), func(st State) bool {
		return st.Hash() == stored.Hash() && st.LastEventAt == stored.LastEventAt &&
			st.Generation == stored.Generation && st.Overflowed == stored.Overflowed &&
			(st.Claim == nil) == (stored.Claim == nil)
	})
	return stored
}

// heldRig is a store over the test NATS, its summarizer, and the logger its passes write to.
type heldRig struct {
	t      *testing.T
	store  *Store
	z      *summarizer
	pub    *recPub
	logs   *bytes.Buffer
	logger *logging.Logger
}

func newHeldRig(t *testing.T) *heldRig {
	t.Helper()
	conn, cleanup := connectNATS(t)
	t.Cleanup(cleanup)
	store := openStore(t, conn)
	logs := &bytes.Buffer{}
	return &heldRig{
		t:      t,
		store:  store,
		z:      testSummarizer(store, heldDebounce),
		pub:    &recPub{},
		logs:   logs,
		logger: logging.NewWithWriter("test", logs),
	}
}

// record records each observation on the rig's store, failing the test on an error.
func (r *heldRig) record(observations ...contracts.CIObservation) {
	r.t.Helper()
	for _, observation := range observations {
		if err := record(r.store, observation); err != nil {
			r.t.Fatalf("record %+v: %v", observation, err)
		}
	}
}

// pass runs one summarizer pass at now and returns how many records it held back.
func (r *heldRig) pass(now int64) int {
	held, _, _ := r.z.pass(r.pub, r.logger, now)
	return held
}

// heldLines is every held line the rig's passes logged.
func (r *heldRig) heldLines() []map[string]any {
	return warnings(r.logs, heldLine)
}

// wantHeldLines fails unless the rig's passes logged n held lines in all.
func (r *heldRig) wantHeldLines(n int, what string) []map[string]any {
	r.t.Helper()
	lines := r.heldLines()
	if len(lines) != n {
		r.t.Fatalf("%s: %d held lines, want %d:\n%s", what, len(lines), n, r.logs.String())
	}
	return lines
}

// assertHeldLine fails unless line is at level and names h, st's generation and each of fields.
func assertHeldLine(t *testing.T, line map[string]any, level string, h head, st State, fields map[string]any) {
	t.Helper()
	want := map[string]any{
		"level": level, "owner": h.owner, "repo": h.repo, "number": h.number, "sha": h.sha,
		"generation": float64(st.Generation),
	}
	maps.Copy(want, fields)
	for field, value := range want {
		if !reflect.DeepEqual(line[field], value) {
			t.Fatalf("the held line has %s %#v, want %#v: %v", field, line[field], value, line)
		}
	}
}

// heldFields is what a held line names beyond the head and its generation: its checks, each check
// not terminal and each suite not completed, the four hold flags unset, and each field named in
// flags set to true (a hold flag, or every_check_finished).
func heldFields(checks int, running, suites []any, flags ...string) map[string]any {
	fields := map[string]any{
		"checks": float64(checks), "running": running, "running_count": float64(len(running)), "suites": suites,
		"overflowed": false, "no_run_id": false, "legacy": false, "claimed": false,
	}
	for _, flag := range flags {
		fields[flag] = true
	}
	return fields
}

// finishedChecks records three completed check runs of h.
func finishedChecks(h head) []contracts.CIObservation {
	return []contracts.CIObservation{
		h.check("build", 9101, "completed", "success", "2026-11-19T15:50:00Z"),
		h.check("lint", 9102, "completed", "success", "2026-11-19T15:51:00Z"),
		h.check("test", 9103, "completed", "failure", "2026-11-19T15:52:00Z"),
	}
}

// actionsSuites records three completed suites of h's from GitHub Actions (App 15368), whose event
// gave a count that a completed suite does not keep.
func actionsSuites(h head) []contracts.CIObservation {
	return []contracts.CIObservation{
		suiteObservation(h, "120620605801", "15368", "completed", "success", "2026-11-19T15:53:00Z", new(48)),
		suiteObservation(h, "120620605802", "15368", "completed", "success", "2026-11-19T15:53:00Z", new(2)),
		suiteObservation(h, "120620605803", "15368", "completed", "failure", "2026-11-19T15:53:00Z", new(1)),
	}
}

// A record the summary loop does not settle - one not ready, or a head-gated listener's leftover
// it holds back - is named in one line once it has had no event for ten minutes: WARN where every
// recorded check run is terminal and INFO where one still runs, naming the head, its generation,
// its check count, each check that is not terminal, each suite that is not completed with its App
// and run count, and the hold the snapshot does not take. The line is logged at the bound and not
// a millisecond before, once for each snapshot and hold: a pass over a day later, or a pass that
// files the record again unchanged, logs nothing more, while a new event and a second bound, or a
// change of the hold alone, logs it once more. A record that settles logs none.
func TestAHeldHeadNoRunLogsOneLineNamingWhatHoldsIt(t *testing.T) {
	const day = int64(24 * time.Hour / time.Millisecond)
	queuedApp := "120620537894 app=5789269 status=queued runs=0"
	for _, tc := range []struct {
		name   string
		h      head
		record func(r *heldRig, h head)
		level  string
		held   int
		fields map[string]any
	}{
		{
			name: "every check finished and an App's suite queued with no run",
			h:    head{"example-org", "example-repo", "42", "5e1d000000000000000000000000000000000001"},
			record: func(r *heldRig, h head) {
				r.record(finishedChecks(h)...)
				r.record(actionsSuites(h)...)
				r.record(suiteObservation(h, "120620537894", "5789269", "queued", "", "2026-11-19T15:56:07Z", new(0)))
			},
			level:  "WARN",
			fields: heldFields(3, []any{}, []any{queuedApp}),
		},
		{
			name: "two Apps' suites not completed name both in one line",
			h:    head{"example-org", "example-repo", "42", "5e1d000000000000000000000000000000000002"},
			record: func(r *heldRig, h head) {
				r.record(finishedChecks(h)...)
				r.record(actionsSuites(h)...)
				r.record(suiteObservation(h, "120620537894", "5789269", "queued", "", "2026-11-19T15:56:07Z", new(0)))
				r.record(suiteObservation(h, "120620537895", "424242", "requested", "", "2026-11-19T15:56:08Z", new(0)))
			},
			level:  "WARN",
			fields: heldFields(3, []any{}, []any{queuedApp, "120620537895 app=424242 status=requested runs=0"}),
		},
		{
			name: "a check still in progress",
			h:    head{"example-org", "example-repo", "43", "5e1d000000000000000000000000000000000003"},
			record: func(r *heldRig, h head) {
				r.record(h.check("build", 9201, "completed", "success", "2026-11-19T15:50:00Z"))
				r.record(h.check("deploy", 9202, "in_progress", "", "2026-11-19T15:51:00Z"))
				r.record(actionsSuites(h)[0])
			},
			level:  "INFO",
			fields: heldFields(2, []any{"deploy:in_progress"}, []any{}),
		},
		{
			name: "no check with a run id",
			h:    head{"example-org", "example-repo", "44", "5e1d000000000000000000000000000000000004"},
			record: func(r *heldRig, h head) {
				r.record(h.check("build", 0, "completed", "success", "2026-11-19T15:50:00Z"))
			},
			level:  "WARN",
			fields: heldFields(1, []any{}, []any{}, "no_run_id"),
		},
		{
			name: "a head-gated listener's ready record held back past its handover band",
			h:    head{"example-org", "example-repo", "45", "5e1d000000000000000000000000000000000005"},
			record: func(r *heldRig, h head) {
				putHeadGatedRecord(r.t, r.store, h, time.Now().UnixMilli(), nil)
			},
			level:  "WARN",
			held:   1,
			fields: heldFields(1, []any{}, []any{}, "legacy"),
		},
		{
			name: "a suite recorded with no run count",
			h:    head{"example-org", "example-repo", "46", "5e1d000000000000000000000000000000000006"},
			record: func(r *heldRig, h head) {
				r.record(h.check("build", 9601, "completed", "success", "2026-11-19T15:50:00Z"))
				r.record(suiteObservation(h, "120620537896", "5789269", "queued", "", "2026-11-19T15:56:07Z", nil))
			},
			level:  "WARN",
			fields: heldFields(1, []any{}, []any{"120620537896 app=5789269 status=queued runs=unknown"}),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newHeldRig(t)
			tc.record(r, tc.h)
			st := cachedAsStored(t, r.store, tc.h)
			bound := st.LastEventAt + heldBound.Milliseconds()

			if held := r.pass(bound - 1); held != tc.held {
				t.Fatalf("the pass before the bound held back %d records, want %d", held, tc.held)
			}
			r.wantHeldLines(0, "a millisecond before the bound")
			if held := r.pass(bound); held != tc.held {
				t.Fatalf("the pass at the bound held back %d records, want %d", held, tc.held)
			}
			fields := maps.Clone(tc.fields)
			fields["every_check_finished"] = tc.level == "WARN"
			assertHeldLine(t, r.wantHeldLines(1, "at the bound")[0], tc.level, tc.h, st, fields)

			r.pass(bound)
			for i := int64(1); i <= 100; i++ {
				if held := r.pass(bound + i*day/100); held != tc.held {
					t.Fatalf("pass %d over the day held back %d records, want %d", i, held, tc.held)
				}
			}
			// A write that moves neither the snapshot nor the hold - its last event moved back an
			// hour - is a change the pass files the record again for, with no line.
			setLastEventAt(t, r.store, tc.h.owner, tc.h.repo, tc.h.number, tc.h.sha, st.LastEventAt-time.Hour.Milliseconds())
			r.pass(bound + day + 1)
			r.wantHeldLines(1, "a second pass, a hundred over a day and a record filed again unchanged")
			if got := r.pub.count(); got != 0 {
				t.Fatalf("the held record published %d settlements, want none", got)
			}
		})
	}

	t.Run("a new event and a second bound log it again", func(t *testing.T) {
		r := newHeldRig(t)
		h := head{"example-org", "example-repo", "47", "5e1d000000000000000000000000000000000007"}
		r.record(finishedChecks(h)...)
		r.record(suiteObservation(h, "120620537894", "5789269", "queued", "", "2026-11-19T15:56:07Z", new(0)))
		first := cachedAsStored(t, r.store, h)
		bound := first.LastEventAt + heldBound.Milliseconds()
		r.pass(bound - 1)
		r.pass(bound)
		r.wantHeldLines(1, "at the first bound")

		r.record(suiteObservation(h, "120620537894", "5789269", "in_progress", "", "2026-11-19T16:10:00Z", new(0)))
		second := cachedAsStored(t, r.store, h)
		next := second.LastEventAt + heldBound.Milliseconds()
		r.pass(next - 1)
		r.wantHeldLines(1, "a millisecond before the second bound")
		r.pass(next)
		lines := r.wantHeldLines(2, "at the second bound")
		assertHeldLine(t, lines[1], "WARN", h, second, heldFields(3, []any{}, []any{"120620537894 app=5789269 status=in_progress runs=0"}, "every_check_finished"))
		r.pass(next + day)
		r.wantHeldLines(2, "a day after the second bound")
	})

	t.Run("an overflowed record, and its claim taken later", func(t *testing.T) {
		r := newHeldRig(t)
		h := head{"example-org", "example-repo", "48", "5e1d000000000000000000000000000000000008"}
		r.record(finishedChecks(h)...)
		r.record(actionsSuites(h)...)
		cachedAsStored(t, r.store, h)
		if err := r.store.markOverflowed(h.key()); err != nil {
			t.Fatalf("mark the record overflowed: %v", err)
		}
		st := cachedAsStored(t, r.store, h)
		bound := st.LastEventAt + heldBound.Milliseconds()
		r.pass(bound - 1)
		r.wantHeldLines(0, "a millisecond before the bound")
		r.pass(bound)
		fields := heldFields(3, []any{}, []any{}, "overflowed", "every_check_finished")
		assertHeldLine(t, r.wantHeldLines(1, "at the bound")[0], "WARN", h, st, fields)

		setClaim(t, r.store, h.owner, h.repo, h.number, h.sha, &SettlementClaim{Hash: st.Hash(), Generation: st.Generation, ClaimedAt: time.Now().UnixMilli()})
		claimed := cachedAsStored(t, r.store, h)
		if claimed.Hash() != st.Hash() {
			t.Fatalf("the claim moved the snapshot from %s to %s; the row needs a hold change alone", st.Hash(), claimed.Hash())
		}
		r.pass(bound + 1)
		fields["claimed"] = true
		assertHeldLine(t, r.wantHeldLines(2, "after the claim was taken")[1], "WARN", h, claimed, fields)
		r.pass(bound + day)
		r.wantHeldLines(2, "a day after the claim was taken")
	})

	t.Run("a record that settles logs none", func(t *testing.T) {
		r := newHeldRig(t)
		settles := head{"example-org", "example-repo", "49", "5e1d000000000000000000000000000000000009"}
		r.record(finishedChecks(settles)...)
		r.record(actionsSuites(settles)...)
		st := cachedAsStored(t, r.store, settles)
		r.pass(st.LastEventAt + heldDebounce.Milliseconds())
		if got := r.pub.count(); got != 1 {
			t.Fatalf("the ready record published %d settlements, want 1", got)
		}
		cached := cachedAsStored(t, r.store, settles)
		if !cached.SettledEmitted {
			t.Fatalf("the ready record was not marked settled: %+v", cached)
		}
		bound := st.LastEventAt + heldBound.Milliseconds()
		r.pass(bound)
		r.pass(bound + day)
		r.wantHeldLines(0, "a settled record at the bound and a day after it")

		// A record named while held that then settles drops what was logged of it.
		later := head{"example-org", "example-repo", "50", "5e1d00000000000000000000000000000000000a"}
		r.record(finishedChecks(later)...)
		r.record(suiteObservation(later, "120620537894", "5789269", "queued", "", "2026-11-19T15:56:07Z", new(0)))
		held := cachedAsStored(t, r.store, later)
		heldAt := max(held.LastEventAt+heldBound.Milliseconds(), bound+day+1)
		r.pass(heldAt)
		r.wantHeldLines(1, "the held record at its bound")
		r.record(suiteObservation(later, "120620537894", "5789269", "completed", "success", "2026-11-19T16:20:00Z", new(0)))
		cachedAsStored(t, r.store, later)
		r.pass(heldAt + 1)
		if got := r.pub.count(); got != 2 {
			t.Fatalf("the record that stopped being held published %d settlements in all, want 2", got)
		}
		cachedAsStored(t, r.store, later)
		r.pass(heldAt + 2)
		r.z.mu.Lock()
		remembered := len(r.z.stuckLogged)
		r.z.mu.Unlock()
		if remembered != 0 {
			t.Fatalf("the summarizer remembers %d held lines after the records settled, want none", remembered)
		}
		r.wantHeldLines(1, "after the held record settled")
	})
}

// Each settlement the summary loop publishes logs one INFO line naming its head, generation and
// snapshot, and whether it superseded an earlier settlement of the record, after the publication
// succeeded. A publication NATS refuses, or one that fails, logs no such line; a failed one
// retried logs it once it succeeds.
func TestASettlementPublishedLogsOneLine(t *testing.T) {
	// settlementOf decodes pub's last settlement.
	settlementOf := func(t *testing.T, pub *recPub) Summary {
		t.Helper()
		var sum Summary
		if err := json.Unmarshal([]byte(pub.last().Payload), &sum); err != nil {
			t.Fatalf("decode the settlement: %v", err)
		}
		return sum
	}
	assertPublished := func(t *testing.T, line map[string]any, h head, sum Summary, superseded bool) {
		t.Helper()
		want := map[string]any{
			"level": "INFO", "owner": h.owner, "repo": h.repo, "number": h.number, "sha": h.sha,
			"generation": float64(sum.Generation), "snapshot": sum.Snapshot, "superseded": superseded,
		}
		for field, value := range want {
			if line[field] != value {
				t.Fatalf("the published line has %s %#v, want %#v: %v", field, line[field], value, line)
			}
		}
	}

	t.Run("a published settlement and its re-settlement", func(t *testing.T) {
		r := newHeldRig(t)
		h := head{"example-org", "example-repo", "51", "5e7713000000000000000000000000000000000b"}
		r.record(h.check("build", 9701, "completed", "success", "2026-11-19T15:50:00Z"))
		waitCacheChecks(t, r.store, h.owner, h.repo, h.number, h.sha, 1)
		runSummaryTick(r.store, r.pub, heldDebounce, r.logger)
		time.Sleep(2 * heldDebounce)
		runSummaryTick(r.store, r.pub, heldDebounce, r.logger)
		if got := r.pub.count(); got != 1 {
			t.Fatalf("the ready record published %d settlements, want 1", got)
		}
		first := settlementOf(t, r.pub)
		lines := warnings(r.logs, publishedLine)
		if len(lines) != 1 {
			t.Fatalf("%d published lines, want one:\n%s", len(lines), r.logs.String())
		}
		assertPublished(t, lines[0], h, first, false)
		runSummaryTick(r.store, r.pub, heldDebounce, r.logger)
		if lines := warnings(r.logs, publishedLine); len(lines) != 1 {
			t.Fatalf("%d published lines after a tick over the settled record, want still one", len(lines))
		}

		r.record(h.check("lint", 9702, "completed", "success", "2026-11-19T15:55:00Z"))
		waitCacheChecks(t, r.store, h.owner, h.repo, h.number, h.sha, 2)
		time.Sleep(2 * heldDebounce)
		runSummaryTick(r.store, r.pub, heldDebounce, r.logger)
		if got := r.pub.count(); got != 2 {
			t.Fatalf("the re-armed record published %d settlements in all, want 2", got)
		}
		lines = warnings(r.logs, publishedLine)
		if len(lines) != 2 {
			t.Fatalf("%d published lines after the re-settlement, want two:\n%s", len(lines), r.logs.String())
		}
		assertPublished(t, lines[1], h, settlementOf(t, r.pub), true)
	})

	t.Run("a publication the bus refuses", func(t *testing.T) {
		r := newHeldRig(t)
		h := head{"example-org", "example-repo", "52", "5e7713000000000000000000000000000000000c"}
		r.pub.setErr(bus.ErrTooLarge)
		r.record(h.check("build", 9801, "completed", "success", "2026-11-19T15:50:00Z"))
		waitCacheChecks(t, r.store, h.owner, h.repo, h.number, h.sha, 1)
		time.Sleep(2 * heldDebounce)
		runSummaryTick(r.store, r.pub, heldDebounce, r.logger)
		if refused := warnings(r.logs, "checks settlement refused"); len(refused) != 1 {
			t.Fatalf("%d refusal lines, want one: the bus never refused the settlement\n%s", len(refused), r.logs.String())
		}
		if lines := warnings(r.logs, publishedLine); len(lines) != 0 {
			t.Fatalf("a refused settlement logged %d published lines, want none:\n%s", len(lines), r.logs.String())
		}
	})

	t.Run("a publication that fails, and its retry", func(t *testing.T) {
		r := newHeldRig(t)
		h := head{"example-org", "example-repo", "53", "5e7713000000000000000000000000000000000d"}
		r.pub.setErr(errors.New("nats unavailable"))
		r.record(h.check("build", 9901, "completed", "success", "2026-11-19T15:50:00Z"))
		waitCacheChecks(t, r.store, h.owner, h.repo, h.number, h.sha, 1)
		time.Sleep(2 * heldDebounce)
		runSummaryTick(r.store, r.pub, heldDebounce, r.logger)
		if failed := warnings(r.logs, "checks publish failed"); len(failed) != 1 {
			t.Fatalf("%d publish-failed lines, want one: the publication never failed\n%s", len(failed), r.logs.String())
		}
		if lines := warnings(r.logs, publishedLine); len(lines) != 0 {
			t.Fatalf("a failed publication logged %d published lines, want none:\n%s", len(lines), r.logs.String())
		}

		r.pub.setErr(nil)
		deadline := time.Now().Add(5 * time.Second)
		for r.pub.count() == 0 {
			if time.Now().After(deadline) {
				t.Fatal("the failed settlement was never published on retry")
			}
			time.Sleep(2 * heldDebounce)
			runSummaryTick(r.store, r.pub, heldDebounce, r.logger)
		}
		lines := warnings(r.logs, publishedLine)
		if len(lines) != 1 {
			t.Fatalf("%d published lines after the retry, want one:\n%s", len(lines), r.logs.String())
		}
		assertPublished(t, lines[0], h, settlementOf(t, r.pub), false)
	})
}

// A suite's run count is kept, as the check_suite event gave it, while the suite is not completed:
// a count of 0 reads back from the bucket as 0, not as no count, and a completed suite keeps none.
// The same event delivered again changes nothing, its count compared by value, so it neither
// writes the record nor moves its last event. The count is display only: the record's snapshot
// and readiness are the same with it as without it.
func TestARecordedSuiteKeepsItsRunCountUntilItCompletes(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)
	h := head{"example-org", "example-repo", "54", "5e1d00000000000000000000000000000000000e"}
	recordOne := func(observation contracts.CIObservation) bool {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		changed, err := s.recordSuite(ctx, observation)
		if err != nil {
			t.Fatalf("record %+v: %v", observation, err)
		}
		return changed
	}
	// storedRunCounts reads each suite's run_count from the bucket's bytes, absent ones left out.
	storedRunCounts := func() (map[string]any, uint64) {
		t.Helper()
		entry, err := s.watcher.KV().Get(h.key())
		if err != nil {
			t.Fatalf("kv get: %v", err)
		}
		var record struct {
			Suites map[string]map[string]any `json:"suites"`
		}
		if err := json.Unmarshal(entry.Value(), &record); err != nil {
			t.Fatalf("decode the stored record: %v", err)
		}
		counts := map[string]any{}
		for id, suite := range record.Suites {
			if count, ok := suite["run_count"]; ok {
				counts[id] = count
			}
		}
		return counts, entry.Revision()
	}
	withoutCounts := func(st State) State {
		st.Suites = maps.Clone(st.Suites)
		for id, suite := range st.Suites {
			suite.RunCount = nil
			st.Suites[id] = suite
		}
		return st
	}

	if !recordOne(suiteObservation(h, "9301", "5789269", "requested", "", "2026-11-19T15:56:07Z", new(0))) {
		t.Fatal("the requested suite changed nothing")
	}
	if !recordOne(suiteObservation(h, "9302", "424242", "requested", "", "2026-11-19T15:56:08Z", new(2))) {
		t.Fatal("the second requested suite changed nothing")
	}
	counts, revision := storedRunCounts()
	if want := map[string]any{"9301": float64(0), "9302": float64(2)}; !maps.Equal(counts, want) {
		t.Fatalf("the bucket holds run counts %v, want %v", counts, want)
	}
	st := getState(t, s, h.owner, h.repo, h.number, h.sha)
	if c := st.Suites["9301"].RunCount; c == nil || *c != 0 {
		t.Fatalf("suite 9301 reads back run count %v, want 0", c)
	}
	if c := st.Suites["9302"].RunCount; c == nil || *c != 2 {
		t.Fatalf("suite 9302 reads back run count %v, want 2", c)
	}
	if bare := withoutCounts(st); bare.Hash() != st.Hash() || settlementReady(bare) != settlementReady(st) {
		t.Fatalf("the not-ready record without its counts hashes %s ready %v, with them %s ready %v",
			bare.Hash(), settlementReady(bare), st.Hash(), settlementReady(st))
	}

	// The same event again, its count a pointer of its own.
	if recordOne(suiteObservation(h, "9301", "5789269", "requested", "", "2026-11-19T15:56:07Z", new(0))) {
		t.Fatal("the requested suite delivered again changed the record")
	}
	if _, again := storedRunCounts(); again != revision {
		t.Fatalf("the requested suite delivered again wrote the record (revision %d -> %d)", revision, again)
	}
	if again := getState(t, s, h.owner, h.repo, h.number, h.sha); again.LastEventAt != st.LastEventAt {
		t.Fatalf("the requested suite delivered again moved the last event from %d to %d", st.LastEventAt, again.LastEventAt)
	}

	if !recordOne(suiteObservation(h, "9301", "5789269", "completed", "success", "2026-11-19T16:20:00Z", new(3))) {
		t.Fatal("the completed suite changed nothing")
	}
	if !recordOne(suiteObservation(h, "9302", "424242", "completed", "success", "2026-11-19T16:20:01Z", new(2))) {
		t.Fatal("the second completed suite changed nothing")
	}
	if counts, _ := storedRunCounts(); len(counts) != 0 {
		t.Fatalf("the completed suites keep run counts %v, want none", counts)
	}
	if err := record(s, h.check("build", 9303, "completed", "success", "2026-11-19T16:21:00Z")); err != nil {
		t.Fatalf("record the check run: %v", err)
	}
	ready := getState(t, s, h.owner, h.repo, h.number, h.sha)
	if !settlementReady(ready) {
		t.Fatalf("the record of completed suites and a completed run is not ready: %+v", ready)
	}
	counted := withoutCounts(ready)
	suite := counted.Suites["9301"]
	suite.RunCount = new(3)
	counted.Suites["9301"] = suite
	if counted.Hash() != ready.Hash() || !settlementReady(counted) {
		t.Fatalf("the ready record with a count hashes %s ready %v, without it %s ready true",
			counted.Hash(), settlementReady(counted), ready.Hash())
	}
}
