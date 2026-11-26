package outbox

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// An entry of the held-copy record that no pass has found waiting for fallbackClaim - its hold
// moved to another process - leaves the gauges at once and leaves the record at the next pass
// that holds a copy, with no scrape of the gauges: reading them changes nothing, and the record
// keeps only what this process's passes last found waiting.
func TestAHandedOverHeldCopyLeavesTheRecordWithoutAScrape(t *testing.T) {
	fake := clocktest.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	held := newHeldCopies(fake)
	issue := "A-1"
	event := model.Event{IssueKey: &issue}
	stale := fallbackCopy{commentID: "stale", destination: "notifications.agent.s1"}
	held.hold(stale, event, model.MentionCopyHolder{EventID: 1}, time.Hour)
	long := fake.Now().Add(-2 * fallbackClaim)
	held.mu.Lock()
	held.rows[heldCopyKey{comment: stale.commentID, destination: stale.destination}].seen = long
	held.swept = long
	held.mu.Unlock()
	entries := func() []string {
		held.mu.Lock()
		defer held.mu.Unlock()
		var comments []string
		for key := range held.rows {
			comments = append(comments, key.comment)
		}
		return comments
	}

	if count, _ := held.samples(fake.Now()); count != 0 {
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
}
