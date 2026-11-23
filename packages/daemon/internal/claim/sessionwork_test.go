package claim

import (
	"testing"
	"time"
)

// A report counts only when it was read at or after the caller's moment (ReadSessionWork): a
// session with no report, and one whose newest report was read before its last turn, has said
// nothing about that turn, whatever background work the old report named. A report read at the
// same instant as the turn counts, and a session with no turn the caller knows of counts every
// report it sent. Each row runs with and without background work, which decides only between
// the two values a counted report reads as.
func TestSessionWorkReadsAReportOnlyAtOrAfterTheLastTurn(t *testing.T) {
	lastTurn := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	rows := []struct {
		name       string
		reportedAt time.Time
		lastTurn   time.Time
		counts     bool
	}{
		{name: "no report", reportedAt: time.Time{}, lastTurn: lastTurn, counts: false},
		{name: "a report one second before the last turn", reportedAt: lastTurn.Add(-time.Second), lastTurn: lastTurn, counts: false},
		{name: "a report at the instant of the last turn", reportedAt: lastTurn, lastTurn: lastTurn, counts: true},
		{name: "a report after the last turn", reportedAt: lastTurn.Add(time.Second), lastTurn: lastTurn, counts: true},
		{name: "a report with no last turn", reportedAt: lastTurn, lastTurn: time.Time{}, counts: true},
	}
	for _, row := range rows {
		for _, background := range []bool{false, true} {
			want := WorkUnreported
			switch {
			case row.counts && background:
				want = WorkRunning
			case row.counts:
				want = WorkSettled
			}
			if got := ReadSessionWork(row.reportedAt, row.lastTurn, background); got != want {
				t.Errorf("%s, background %t: ReadSessionWork = %d, want %d", row.name, background, got, want)
			}
		}
	}
}
