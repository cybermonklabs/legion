package delivery

// The line that ends each reconcile pass, with the time the whole pass took (revision 21, review
// 20 B1).

import (
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/events"
)

// reconcilePassLine is the message of the line that ends each reconcile pass. headPassLine is the
// message of the head pass's line (reconcilePullRequestHeads).
const (
	reconcilePassLine = "dispatch delivery: reconcile pass"
	headPassLine      = "dispatch delivery: pull request attribution"
)

// A pass logs its time once, where it ends (revision 21, review 20 B1). The first pass has no
// delivery settings, so it returns at the settings arm. The second pass has the settings and the
// fake GitHub, so it runs each step. Want one reconcilePassLine in each pass, with took set, and no
// line of that pass after it. Want no took on the head pass's line. A pass that logs no such line,
// or logs its time at the head pass, fails.
func TestAPassLogsItsTimeOnceWhereItEnds(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	fake := newFakeGitHub(t)
	fake.handle("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, searchResponseJSON(0, nil, false, ""))
	})
	noRuns(t, fake)
	reconcile := NewReconcile(pool, fake.newTestClient(), events.NewBroker())
	logs := captureLogs(t)

	reconcile.runOnce(ctx)
	first := logs()
	wantOnePassLine(t, "the pass with no delivery settings", first)

	seedDeliverySettings(t, ctx, pool)
	reconcile.runOnce(ctx)
	wantOnePassLine(t, "the pass with the delivery settings", logs()[len(first):])
	settings, err := GetSettings(ctx, pool)
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if settings.LastError != nil || settings.LastReconcileAt == nil {
		lastError := "none"
		if settings.LastError != nil {
			lastError = *settings.LastError
		}
		t.Errorf("the pass with the delivery settings recorded last_error %q and last_reconcile_at %v, want a pass that ran each step",
			lastError, settings.LastReconcileAt)
	}
}

// wantOnePassLine fails the test unless records, the lines of one pass, hold one reconcilePassLine
// as their last line, with took above 0. They must also hold one headPassLine, with no took.
func wantOnePassLine(t *testing.T, pass string, records []map[string]any) {
	t.Helper()
	var passLines []int
	var heads int
	for i, record := range records {
		switch record["msg"] {
		case reconcilePassLine:
			passLines = append(passLines, i)
		case headPassLine:
			heads++
			if took, found := record["took"]; found {
				t.Errorf("%s logged msg=%q with took %v, want no took: the line of the whole pass holds the time", pass, headPassLine, took)
			}
		}
	}
	if heads != 1 {
		t.Errorf("%s logged %d lines msg=%q, want 1", pass, heads, headPassLine)
	}
	if len(passLines) != 1 {
		t.Errorf("%s logged %d lines msg=%q, want 1", pass, len(passLines), reconcilePassLine)
		return
	}
	at := passLines[0]
	if took, ok := records[at]["took"].(float64); !ok || took <= 0 {
		t.Errorf("%s logged msg=%q with took %v, want a time above 0", pass, reconcilePassLine, records[at]["took"])
	}
	if after := records[at+1:]; len(after) != 0 {
		t.Errorf("%s logged %d lines after msg=%q, want none: the first is msg=%q", pass, len(after), reconcilePassLine, after[0]["msg"])
	}
}
