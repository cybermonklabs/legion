package daemon

import (
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/testdoc"
)

// docs/kubernetes.md states two facts that are this package's constants: how often the daemon
// copies a running pod's workspace (runningCopyPeriod) and the line a copy that was not stored is
// logged with (runningCopyUnsavedLine). An operator reads the period to know how much work an end
// the cluster made can cost, and searches the log for the line. So the check reads both from the
// constants themselves: a period or a line changed here is a document that must change with it,
// and nobody has to remember to edit this file. The sandbox runtime's own doc test
// (internal/runtime/sandbox's workspace_copy_doc_test.go) checks the rest of the same paragraphs
// through the same reader, internal/testdoc.
//
// Each check reads one region of the document, bounded by its first and last words, and fails by
// naming the marker it could not find: a phrase found anywhere in a document of thousands of lines
// would answer a check about a paragraph that says the opposite. Where the fact belongs to one
// bullet of a list, it is held to that bullet.
//
// What these checks cannot do: they read what the document says, never whether it is true of the
// daemon's behaviour. That the cadence really waits runningCopyPeriod is this package's other
// tests' to say.
const (
	copyCadenceStart = "The daemon keeps a copy of each issue workspace's unpushed work outside the pod while the pod runs"
	copyCadenceEnd   = "and the next period tries again."

	endsApartStart = "Three ends differ in what reaches the issue's next pod"
	endsApartEnd   = "and each role's session row."
)

// The cadence paragraphs state the period from runningCopyPeriod and the line from
// runningCopyUnsavedLine, and the bullet for an end the cluster made states how far behind the
// restored copy can be, which is the same period.
func TestKubernetesDocStatesTheRunningCopysConstants(t *testing.T) {
	period := docMinutes(t, runningCopyPeriod)

	const cadence = `docs/kubernetes.md's cadence paragraphs of "The copy of a running pod's workspace"`
	_, region := testdoc.Region(t, "docs/kubernetes.md", cadence, copyCadenceStart, copyCadenceEnd)
	testdoc.RequireDocFacts(t, cadence, region, []testdoc.Fact{
		{Fact: "the second trigger and its bound, runningCopyPeriod", Phrase: "every " + period + " while a turn of that issue runs"},
		{Fact: "the line a copy that was not stored is logged with, runningCopyUnsavedLine", Phrase: runningCopyUnsavedLine},
	})

	const ends = `docs/kubernetes.md's "What each kind of end loses and keeps" paragraph`
	lines, _ := testdoc.Region(t, "docs/kubernetes.md", ends, endsApartStart, endsApartEnd)
	if bullet, ok := testdoc.BulletNaming(t, ends, testdoc.RegionBullets(lines), "an end the cluster made"); ok {
		testdoc.RequireDocFacts(t, ends+", its bullet for an end the cluster made", bullet, []testdoc.Fact{
			{Fact: "how much work the restored copy can be behind, runningCopyPeriod", Phrase: period},
		})
	}
}

// docMinutes is d as the document writes a period, "5 minutes". A period that is no whole number
// of minutes fails here by name: the document has no way to write it that this check could find,
// and a check that cannot say what it looks for has stopped checking.
func docMinutes(t *testing.T, d time.Duration) string {
	t.Helper()
	if d <= 0 || d%time.Minute != 0 {
		t.Fatalf("the period %s is no whole number of minutes, which is how docs/kubernetes.md states it: write the "+
			"document's figure and this check's together", d)
	}
	if d == time.Minute {
		return "1 minute"
	}
	return fmt.Sprintf("%d minutes", d/time.Minute)
}
