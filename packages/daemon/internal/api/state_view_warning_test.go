package api

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The state route authenticates nobody and the values of `view` are the caller's, so the warning
// for a request that names a view other than `observed` is logged at most once a minute in all,
// whoever asks. A request that is not logged is counted, and the next line, a minute after the
// last, names that count (`skipped`). Every such request is still served the whole state. A request
// with no `view` and a request for the observed view log nothing and are not counted.
func TestTheStateRouteLogsTheOtherViewWarningAtMostOnceAMinuteAndNamesTheCountOfTheRest(t *testing.T) {
	var logged strings.Builder
	clock := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	start := clock
	server := NewServer("127.0.0.1", 8437, Options{State: &observingSource{observed: observedFixture()},
		StateTransactions: &fakeStateTransactions{tx: &fakeTx{}}, Log: slog.New(slog.NewJSONHandler(&logged, nil)),
		Now: func() time.Time { return clock }})
	serve := func(at time.Duration, query string) string {
		t.Helper()
		clock = start.Add(at)
		recorder := httptest.NewRecorder()
		server.Handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/legion/v1/state"+query, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET state%s at +%s = %d %s, want 200", query, at, recorder.Code, recorder.Body)
		}
		return recorder.Body.String()
	}
	warnings := func() []map[string]any {
		t.Helper()
		return warnRecords(t, logged.String())
	}
	// wantLines requires that the log holds exactly want lines, and that the last one names views
	// and the count of the requests that were not logged before it.
	wantLines := func(at time.Duration, want int, views []any, skipped float64) {
		t.Helper()
		found := warnings()
		if len(found) != want {
			t.Fatalf("at +%s the log holds %d warnings %v, want %d", at, len(found), found, want)
		}
		last := found[len(found)-1]
		if !reflect.DeepEqual(last["views"], views) || last["skipped"] != skipped {
			t.Fatalf("at +%s the newest warning names views %v and skipped %v, want views %v and skipped %v: %v",
				at, last["views"], last["skipped"], views, skipped, last)
		}
	}

	whole := serve(0, "")
	if got := warnings(); len(got) != 0 {
		t.Fatalf("a request with no view logged %v, want nothing", got)
	}

	// The first such request logs, and counts none before it.
	if body := serve(0, "?view=full"); body != whole {
		t.Fatalf("view=full served %s, want the whole state %s", body, whole)
	}
	wantLines(0, 1, []any{"full"}, 0)

	// Three such requests, and two that are not, inside the minute log nothing.
	for _, step := range []struct {
		after time.Duration
		query string
		// whole is whether the request is served the whole state.
		whole bool
	}{
		{20 * time.Second, "?view=", true},
		{30 * time.Second, "", true},
		{40 * time.Second, "?view=observed", false},
		{50 * time.Second, "?view=observed&view=full", true},
		{59 * time.Second, "?view=Observed", true},
	} {
		body := serve(step.after, step.query)
		if step.whole && body != whole {
			t.Fatalf("GET state%s at +%s served %s, want the whole state %s", step.query, step.after, body, whole)
		}
		wantLines(step.after, 1, []any{"full"}, 0)
	}

	// The request a minute after the last line logs, and names the three that were not logged: the
	// ones that named a view other than observed, and not the two that did not name one.
	if body := serve(time.Minute, "?view=latest"); body != whole {
		t.Fatalf("view=latest served %s, want the whole state %s", body, whole)
	}
	wantLines(time.Minute, 2, []any{"latest"}, 3)

	// The count starts again at that line: the next minute's first line names the one request
	// between.
	serve(time.Minute+time.Second, "?view=full")
	wantLines(time.Minute+time.Second, 2, []any{"latest"}, 3)
	serve(2*time.Minute, "?view=final")
	wantLines(2*time.Minute, 3, []any{"final"}, 1)
}
