package workflow

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// RecordedFact is a fact the daemon builds from its own record, for a run no live event brought
// (admission's promotion of a stranded child, Adopt's entry of a queued root's todo members). It
// copies the stored priority, so the record it writes keeps the issue's own priority rather than
// reading as none until the next Dispatch observation.
func TestARecordedFactKeepsTheIssuesOwnPriority(t *testing.T) {
	parent := "LEGION-1"
	for _, priority := range []record.Priority{record.P0, record.P3, record.NoPriority} {
		issue := record.Issue{Key: "LEGION-2", Tree: parent, Parent: &parent, Status: "todo", Title: "a child", Rank: "B",
			LastDispatchSeq: 4, HandedOver: true, Repo: "acme/widgets", Priority: priority}
		if got := RecordedFact(issue).Priority; got != priority {
			t.Errorf("RecordedFact of an issue recorded at %v carries %v, want the record's own %v", priority, got, priority)
		}
	}
}
