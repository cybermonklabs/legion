package legionfacts

import (
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
)

// TestTheBoardsTwoReadsJoinedKeepEachIssuesRowWrittenLast: Join answers every row of the board's
// two reads, one row for each issue, and where an issue stands in both, because it closed or
// opened again between the open read (ReadOpen) and the read of the closed rows (ReadShownClosed),
// the row with the newer updated_at, the one written last, whichever read found it. Each row of
// the table is one issue: one in the open read alone, one in the closed read alone, one that
// closed between the reads, one that opened again between them, and one both reads found at one
// write, which keeps the open read's row.
func TestTheBoardsTwoReadsJoinedKeepEachIssuesRowWrittenLast(t *testing.T) {
	t.Parallel()
	written := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	row := func(issue string, closed bool, at time.Time) Row {
		stored := Row{Issue: issue, UpdatedAt: at}
		if closed {
			stored.ClosedFacts = metrics.ClosedFacts{IssueFacts: metrics.IssueFacts{ClosedAt: &written}}
		}
		return stored
	}
	issues := []struct {
		name         string
		open, closed *Row
		want         Row
	}{
		{"open alone", new(row("INFRA-1", false, written)), nil, row("INFRA-1", false, written)},
		{"closed alone", nil, new(row("INFRA-2", true, written)), row("INFRA-2", true, written)},
		{"closed between the reads", new(row("INFRA-3", false, written)), new(row("INFRA-3", true, written.Add(time.Second))), row("INFRA-3", true, written.Add(time.Second))},
		{"opened again between the reads", new(row("INFRA-4", false, written.Add(time.Second))), new(row("INFRA-4", true, written)), row("INFRA-4", false, written.Add(time.Second))},
		{"one write in both reads", new(row("INFRA-5", false, written)), new(row("INFRA-5", true, written)), row("INFRA-5", false, written)},
	}
	open, closed := map[string]Row{}, map[string]Row{}
	for _, issue := range issues {
		if issue.open != nil {
			open[issue.open.Issue] = *issue.open
		}
		if issue.closed != nil {
			closed[issue.closed.Issue] = *issue.closed
		}
	}
	joined := Join(open, closed)
	if len(joined) != len(issues) {
		t.Errorf("Join answers %d rows %v, want one for each of the %d issues", len(joined), slices.Sorted(maps.Keys(joined)), len(issues))
	}
	for _, issue := range issues {
		got, held := joined[issue.want.Issue]
		switch {
		case !held:
			t.Errorf("%s: Join answers no row of %s", issue.name, issue.want.Issue)
		case (got.ClosedAt != nil) != (issue.want.ClosedAt != nil) || !got.UpdatedAt.Equal(issue.want.UpdatedAt):
			t.Errorf("%s: Join answers %s's row written at %s, closed %v; want the row written at %s, closed %v",
				issue.name, issue.want.Issue, got.UpdatedAt, got.ClosedAt != nil, issue.want.UpdatedAt, issue.want.ClosedAt != nil)
		}
	}
}
