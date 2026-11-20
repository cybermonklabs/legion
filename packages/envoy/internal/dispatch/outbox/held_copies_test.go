package outbox

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/metrics"
)

// An entry of the held-copy record that no pass has found waiting for fallbackClaim - its hold
// moved to another process - leaves the gauges at once and leaves the record at the next pass
// that holds a copy, with no scrape of the gauges: reading them changes nothing, and the record
// keeps only what this process's passes last found waiting. The record reads a clock the test
// moves (heldClock), and the stale entry ages by that clock alone, so a read of the wall clock in
// the sweep leaves the entry in the record and fails the row. The row runs from two starts, one
// before the wall clock and one after it, since each of the sweep's three reads of the time
// shows from one of them: its check that a sweep is due and its check of each entry, read off the
// wall clock, find the entry fresh from a start after it; the sweep's time, stored off the wall
// clock, keeps the next sweep from coming from a start before it.
func TestAHandedOverHeldCopyLeavesTheRecordWithoutAScrape(t *testing.T) {
	for _, start := range []struct {
		name string
		at   time.Time
	}{
		{name: "before the wall clock", at: time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)},
		{name: "after the wall clock", at: time.Now().AddDate(1, 0, 0)},
	} {
		t.Run(start.name, func(t *testing.T) {
			clock := &heldClock{at: start.at}
			held := newHeldCopies(clock.now)
			issue := "A-1"
			event := model.Event{IssueKey: &issue}
			stale := fallbackCopy{commentID: "stale", destination: "notifications.agent.s1"}
			held.hold(stale, event, model.MentionCopyHolder{EventID: 1}, time.Hour)
			clock.advance(2 * fallbackClaim)
			entries := func() []string {
				held.mu.Lock()
				defer held.mu.Unlock()
				var comments []string
				for key := range held.rows {
					comments = append(comments, key.comment)
				}
				return comments
			}

			if count, _ := held.samples(clock.now()); count != 0 {
				t.Fatalf("the gauges read %d, want nothing of a copy no pass found waiting for %s", count, fallbackClaim)
			}
			if got := entries(); len(got) != 1 {
				t.Fatalf("reading the gauges left the record %v, want it unchanged: the gauges only read", got)
			}
			held.hold(fallbackCopy{commentID: "fresh", destination: "notifications.agent.s1"}, event,
				model.MentionCopyHolder{EventID: 2}, time.Hour)
			got := entries()
			t.Logf("the record after the next pass held a copy: %v", got)
			if len(got) != 1 || got[0] != "fresh" {
				t.Fatalf("the record holds %v, want the fresh copy alone: the pass drops the stale one", got)
			}
		})
	}
}

// The outbox New makes - cmd/dispatch's - gives its held-copy record the wall clock: a pass that
// holds a copy through it records the moment it found the copy waiting as a wall-clock time, one
// between the test's own reads of the wall clock before and after the hold, and the record reads
// the copy's age from that moment. The record is made before the first of those reads, so a
// clock that has stood still since the record was made gives a moment before it, as do one at
// the zero time and one behind the wall clock; one ahead of it by more than the hold took gives a
// moment after the second read. The age is read 30 s after the recorded moment, inside the minute
// in which the record counts a copy (fallbackClaim), so however long the test waits between the
// hold and its reads, the copy is counted and its age is 30 s.
func TestTheOutboxGivesItsHeldCopyRecordTheWallClock(t *testing.T) {
	issue := "A-1"
	row := fallbackCopy{commentID: "c1", destination: "notifications.agent.s1"}
	held := New(Deps{}).deps.held
	before := time.Now()
	held.hold(row, model.Event{IssueKey: &issue}, model.MentionCopyHolder{EventID: 1}, time.Hour)
	after := time.Now()
	held.mu.Lock()
	entry, found := held.rows[heldCopyKey{comment: row.commentID, destination: row.destination}]
	var seen time.Time
	if found {
		seen = entry.seen
	}
	held.mu.Unlock()
	if !found || seen.Before(before) || seen.After(after) {
		t.Fatalf("the record holds the copy %v, found waiting at %v; want it held, found waiting between the wall clock's %v before the hold and its %v after",
			found, seen, before, after)
	}
	const later = 30 * time.Second
	if count, oldest := held.samples(seen.Add(later)); count != 1 || oldest != later {
		t.Fatalf("read %v after the moment the hold recorded, the record counts %d copies, the oldest held %v; want one, held %v",
			later, count, oldest, later)
	}
}

// One scrape reads the record's two gauges from one sample: the count of the held copies and the
// oldest one's age. The clock the record reads forgets the copy at its second read, as a pass
// that settles the copy during a scrape does, so a scrape that read the record twice - two gauge
// functions, one after the other - would count the copy and read the age of none. The age is
// exact: the clock stands 2 s past the hold.
func TestAScrapeReadsTheHeldCopyPairFromOneSample(t *testing.T) {
	clock := &heldClock{at: time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)}
	row := fallbackCopy{commentID: "c1", destination: "notifications.agent.s1"}
	var reads atomic.Int64
	var held *heldCopies
	held = newHeldCopies(func() time.Time {
		if reads.Add(1) == 2 {
			held.forget(row)
		}
		return clock.now()
	})
	issue := "A-1"
	held.hold(row, model.Event{IssueKey: &issue}, model.MentionCopyHolder{EventID: 1}, time.Hour)
	clock.advance(2 * time.Second)
	registry := metrics.New()
	held.register(registry)
	reads.Store(0)
	body := heldGauges(t, registry)
	if !strings.Contains(body, "envoy_dispatch_outbox_fallback_copies_held 1\n") ||
		!strings.Contains(body, "envoy_dispatch_outbox_fallback_copy_held_oldest_seconds 2\n") || reads.Load() != 1 {
		t.Fatalf("one scrape read the record's clock %d times and the gauges %q; want one read, and one copy held for 2 s",
			reads.Load(), body)
	}
}
