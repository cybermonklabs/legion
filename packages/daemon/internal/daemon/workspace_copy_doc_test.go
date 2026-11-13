package daemon

import (
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/testdoc"
)

// docs/kubernetes.md's cadence paragraphs and its paragraph on what each kind of pod end loses and
// keeps are checked here, because two of their facts are this package's constants: how often the
// daemon copies a running pod's workspace (runningCopyPeriod) and the line a copy that was not
// stored is logged with (runningCopyUnsavedLine). An operator reads the period to know how much
// work an end the cluster made can cost, and searches the log for the line. So the check reads
// both from the constants themselves: a period or a line changed here is a document that must
// change with it, and nobody has to remember to edit this file. The two regions' markers are
// written here alone, so a reworded paragraph is one marker to change. The sandbox runtime's own
// doc test (internal/runtime/sandbox's workspace_copy_doc_test.go) checks the document's other
// paragraphs on the copy and on pod ends through the same reader, internal/testdoc.
//
// Each check reads one region of the document, bounded by its first and last words, and fails by
// naming the marker it could not find: a phrase found anywhere in a document of thousands of lines
// would answer a check about a paragraph that says the opposite. Where the fact belongs to one
// bullet of a list, it is held to that bullet: the three kinds of end are three bullets, and a fact
// proved from the region's flat text alone would let the close's bullet stand on a sentence
// written for the release.
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

// The copy's cadence: its first trigger and where its period runs from, one copy of an issue in
// flight, no turn waiting for one, the role it is asked of, where it is stored, and what a copy that
// fails costs. Its 5-minute bound and the line a copy that was not stored writes are this package's
// constants, checked against them (TestKubernetesDocStatesTheRunningCopysConstants).
func TestKubernetesDocStatesTheWorkspaceCopyCadence(t *testing.T) {
	const what = `docs/kubernetes.md's cadence paragraphs of "The copy of a running pod's workspace"`
	_, region := testdoc.Region(t, "docs/kubernetes.md", what, copyCadenceStart, copyCadenceEnd)
	testdoc.RequireDocFacts(t, what, region, []testdoc.Fact{
		{Fact: "the first trigger, a turn's end", Phrase: "the end of each turn of the issue"},
		{Fact: "that the period runs from the end of the copy before it", Phrase: "from the end of the copy before it"},
		{Fact: "that one copy of an issue is in flight at a time", Phrase: "One copy of an issue is in flight at a time"},
		{Fact: "that no turn waits for a copy", Phrase: "No turn waits for a copy"},
		{Fact: "the role the copy is asked of", Phrase: "the first live role of the issue that has a running process"},
		{Fact: "the call it is taken through, which an idle release makes too", Phrase: "`BundleWorkspace`"},
		{Fact: "the table it is stored in", Phrase: "workspace_running_copies"},
		{Fact: "that it is never stored in the idle release's row", Phrase: "never in the `workspace_bundles` row"},
		{Fact: "that the line carries the reason by name", Phrase: "with the reason by name"},
		{Fact: "that a failed copy leaves the issue the copy it had", Phrase: "the issue keeps the copy it had"},
	})
}

// What a pod loses and what it keeps at each kind of end, read as the three bullets it is: a
// reader tells an end the cluster made from an idle release and from an issue's close by them.
func TestKubernetesDocStatesWhatEachKindOfPodEndLosesAndKeeps(t *testing.T) {
	const what = `docs/kubernetes.md's "What each kind of end loses and keeps" paragraph`
	lines, _ := testdoc.Region(t, "docs/kubernetes.md", what, endsApartStart, endsApartEnd)
	bullets := testdoc.RegionBullets(lines)
	if len(bullets) != 3 {
		t.Fatalf("%s lists %d kinds of end, want the 3 a reader tells apart: %q", what, len(bullets), bullets)
	}
	for _, end := range []struct {
		name  string
		facts []testdoc.Fact
	}{
		{"an end the cluster made", []testdoc.Fact{
			{Fact: "that the disk goes with the pod", Phrase: "The disk goes with the pod"},
			{Fact: "that the newer saved copy comes back", Phrase: "the newer of the issue's two saved copies"},
			{Fact: "that the roles keep their sessions and are charged nothing", Phrase: "charged no death"},
			{Fact: "what no copy holds", Phrase: "What no copy holds goes with the disk"},
		}},
		{"an idle release", []testdoc.Fact{
			{Fact: "that the bundle is saved before any role stops", Phrase: "saves the release bundle first"},
			{Fact: "that the workspace comes back whole", Phrase: "comes back whole at the next wake"},
			{Fact: "what each role keeps", Phrase: "suspended `released` until a wake"},
			{Fact: "that the issue keeps its slot", Phrase: "The issue keeps its admission slot"},
		}},
		{"an issue's close", []testdoc.Fact{
			{Fact: "that both saved copies are deleted", Phrase: "both saved copies of the workspace are deleted"},
			{Fact: "that nothing of the disk is kept", Phrase: "nothing of that disk is kept"},
			{Fact: "what the issue keeps", Phrase: "What the issue keeps is what it pushed"},
		}},
	} {
		if bullet, ok := testdoc.BulletNaming(t, what, bullets, end.name); ok {
			testdoc.RequireDocFacts(t, what+", its bullet for "+end.name, bullet, end.facts)
		}
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
