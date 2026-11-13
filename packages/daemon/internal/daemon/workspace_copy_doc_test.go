package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// docs/kubernetes.md states two facts that are this package's constants: how often the daemon
// copies a running pod's workspace (runningCopyPeriod) and the line a copy that was not stored is
// logged with (runningCopyUnsavedLine). An operator reads the period to know how much work an end
// the cluster made can cost, and searches the log for the line. So the check reads both from the
// constants themselves: a period or a line changed here is a document that must change with it,
// and nobody has to remember to edit this file. The sandbox runtime's own doc test
// (internal/runtime/sandbox's workspace_copy_doc_test.go) checks the rest of the same paragraphs,
// in the same shape.
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

// docFact is one thing a region of docs/kubernetes.md must state: the fact a reader would lose,
// and the words that say it.
type docFact struct {
	fact   string
	phrase string
}

// The cadence paragraphs state the period from runningCopyPeriod and the line from
// runningCopyUnsavedLine, and the bullet for an end the cluster made states how far behind the
// restored copy can be, which is the same period.
func TestKubernetesDocStatesTheRunningCopysConstants(t *testing.T) {
	period := docMinutes(t, runningCopyPeriod)

	const cadence = `docs/kubernetes.md's cadence paragraphs of "The copy of a running pod's workspace"`
	_, region := kubernetesDocRegion(t, cadence, copyCadenceStart, copyCadenceEnd)
	requireDocFacts(t, cadence, region, []docFact{
		{"the second trigger and its bound, runningCopyPeriod", "every " + period + " while a turn of that issue runs"},
		{"the line a copy that was not stored is logged with, runningCopyUnsavedLine", runningCopyUnsavedLine},
	})

	const ends = `docs/kubernetes.md's "What each kind of end loses and keeps" paragraph`
	lines, _ := kubernetesDocRegion(t, ends, endsApartStart, endsApartEnd)
	if bullet, ok := bulletNaming(t, ends, regionBullets(lines), "an end the cluster made"); ok {
		requireDocFacts(t, ends+", its bullet for an end the cluster made", bullet, []docFact{
			{"how much work the restored copy can be behind, runningCopyPeriod", period},
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

// kubernetesDocRegion is the part of docs/kubernetes.md between the first and the last words of
// one region, as its lines and as one flattened string. It fails by name rather than skipping or
// passing: a marker that cannot be found is a region that was reworded, and a check that cannot
// find what it bounds reads exactly like a pass.
//
// The markers are prose and the document is hard-wrapped, so they straddle line ends: they are
// found in a flattened copy and carried back to the lines they cover, which is what a list read as
// a list needs (regionBullets).
func kubernetesDocRegion(t *testing.T, what, start, end string) ([]string, string) {
	t.Helper()
	// The document is at the repository root, four directories above this package.
	doc, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "docs", "kubernetes.md"))
	if err != nil {
		t.Fatalf("read docs/kubernetes.md: %v", err)
	}
	lines := strings.Split(string(doc), "\n")
	flat, lineOf := flattenKeepingLines(lines)
	startOffset := strings.Index(flat, start)
	if startOffset < 0 {
		t.Fatalf("docs/kubernetes.md has no %q, so %s cannot be found: the region was reworded or removed, and this "+
			"check stops checking the moment it cannot find it", start, what)
	}
	if strings.Contains(flat[startOffset+len(start):], start) {
		t.Fatalf("docs/kubernetes.md has %q more than once, so it no longer says where %s starts: the check would read "+
			"whichever comes first and cannot tell that it read the wrong one", start, what)
	}
	endOffset := strings.Index(flat[startOffset:], end)
	if endOffset < 0 {
		t.Fatalf("docs/kubernetes.md has no %q after %q, so the end of %s moved: an unbounded search would pass on a "+
			"sentence anywhere in the document", end, start, what)
	}
	last := startOffset + endOffset + len(end) - 1
	return lines[lineOf[startOffset] : lineOf[last]+1], flat[startOffset : last+1]
}

// flattenKeepingLines collapses the document's whitespace so prose that was hard-wrapped reads as
// one line, and records for every byte of the result the line it came from.
func flattenKeepingLines(lines []string) (string, []int) {
	var flat strings.Builder
	lineOf := make([]int, 0, len(lines)*8)
	for i, line := range lines {
		for _, field := range strings.Fields(line) {
			if flat.Len() > 0 {
				flat.WriteByte(' ')
				lineOf = append(lineOf, i)
			}
			flat.WriteString(field)
			for range len(field) {
				lineOf = append(lineOf, i)
			}
		}
	}
	return flat.String(), lineOf
}

// requireDocFacts holds text, one region or one bullet of it, to every fact it must state. Each
// miss is its own failure naming the fact, since a reader who has to fix this knows what to write
// from the fact and not from the string.
func requireDocFacts(t *testing.T, what, text string, facts []docFact) {
	t.Helper()
	for _, f := range facts {
		if !strings.Contains(text, f.phrase) {
			t.Errorf("%s does not state %s: it carries no %q. A reader of that part of the document is left to guess, "+
				"and a sentence elsewhere in the document is no answer, since this is the part they read", what, f.fact, f.phrase)
		}
	}
}

// regionBullets reads the markdown bullets of a region, each as its whole content: the `- ` line
// and the indented lines under it, joined.
func regionBullets(region []string) []string {
	var bullets []string
	// The loop advances i itself over a wrapped bullet's continuation lines, so it stays explicit.
	for i := 0; i < len(region); i++ {
		line := strings.TrimRight(region[i], " ")
		if !strings.HasPrefix(line, "- ") {
			continue
		}
		bullet := strings.TrimSpace(strings.TrimPrefix(line, "- "))
		for i+1 < len(region) && strings.HasPrefix(region[i+1], " ") && strings.TrimSpace(region[i+1]) != "" {
			i++
			bullet += " " + strings.TrimSpace(region[i])
		}
		bullets = append(bullets, bullet)
	}
	return bullets
}

// bulletNaming is the one bullet of a region that names what the check is about. Two bullets
// naming it, or none, is a failure of its own.
func bulletNaming(t *testing.T, what string, bullets []string, name string) (string, bool) {
	t.Helper()
	var found []string
	for _, bullet := range bullets {
		if strings.Contains(bullet, name) {
			found = append(found, bullet)
		}
	}
	if len(found) != 1 {
		t.Errorf("%s has %d bullets naming %q, want exactly 1: the facts a reader takes from that bullet have no one "+
			"bullet to be read against. The bullets are: %q", what, len(found), name, bullets)
		return "", false
	}
	return found[0], true
}
