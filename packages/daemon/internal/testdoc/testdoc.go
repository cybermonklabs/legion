// Package testdoc holds the reading of a repository document that the daemon's doc tests share.
// A doc test holds one region of a guide, bounded by its first and last words, to the facts that
// region must state, and the bound is the whole structure: a phrase found anywhere in a document
// of thousands of lines would answer a check about a paragraph that says the opposite. Region
// finds the bounded region and fails by naming a marker it cannot place; RequireDocFacts holds a
// region, or one bullet of it, to its facts; RegionBullets and BulletNaming read a region that is
// a list as the list it is, so each fact is held to the bullet that carries it. Which facts a
// region must state stays each test's own, and so does a list whose shape is its own, such as the
// reclaim paragraph's two lists under their headings.
package testdoc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fact is one thing a region of a document must state: the fact a reader would lose, and the
// phrase that says it. The fact is what a failure reports, because a reader who meets the check
// has to know what to write, not only which string is missing.
type Fact struct {
	Fact   string
	Phrase string
}

// Region is the part of doc, a slash-separated path from the repository root, between the first
// and the last words of one region, as its lines and as one flattened string; what names the
// region in every failure. Every check reads a region and never the whole document, which is the
// miss these checks exist against.
//
// It fails by name rather than skipping or passing. A document that cannot be read, or a marker
// that cannot be found, is a region that was moved or reworded, and a check that cannot find what
// it bounds is a check that stopped checking, which reads exactly like a pass. A start marker
// found twice fails as well, since the check would read whichever comes first.
//
// The markers are prose and may straddle line ends, so they are found in a flattened copy and
// carried back to the lines they cover, which is what a list read as a list needs
// (RegionBullets). The last line is the one the end marker finishes on, so a marker that wraps
// takes its whole last line with it.
func Region(t testing.TB, doc, what, start, end string) ([]string, string) {
	t.Helper()
	text, err := os.ReadFile(filepath.Join(repositoryRoot(t), filepath.FromSlash(doc)))
	if err != nil {
		t.Fatalf("read %s: %v", doc, err)
	}
	return region(t, doc, string(text), what, start, end)
}

// region is Region of a document already read, text being the content of doc.
func region(t testing.TB, doc, text, what, start, end string) ([]string, string) {
	t.Helper()
	lines := strings.Split(text, "\n")
	flat, lineOf := flattenKeepingLines(lines)
	startOffset := strings.Index(flat, start)
	if startOffset < 0 {
		t.Fatalf("%s has no %q, so %s cannot be found: the region was reworded or removed, and this "+
			"check stops checking the moment it cannot find it", doc, start, what)
	}
	if strings.Contains(flat[startOffset+len(start):], start) {
		t.Fatalf("%s has %q more than once, so it no longer says where %s starts: the check would read "+
			"whichever comes first and cannot tell that it read the wrong one", doc, start, what)
	}
	endOffset := strings.Index(flat[startOffset:], end)
	if endOffset < 0 {
		t.Fatalf("%s has no %q after %q, so the end of %s moved: an unbounded search would pass on a "+
			"sentence anywhere in the document", doc, end, start, what)
	}
	last := startOffset + endOffset + len(end) - 1
	return lines[lineOf[startOffset] : lineOf[last]+1], flat[startOffset : last+1]
}

// repositoryRoot is the checkout that holds this module, found by walking up from the directory
// the test runs in, which is its package's own, to the one holding packages/daemon/go.mod. A test
// of any package of the module finds the same root, however deep the package sits.
func repositoryRoot(t testing.TB) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("read the working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "packages", "daemon", "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no packages/daemon/go.mod above the working directory, so the repository root that " +
				"holds the documents cannot be found")
		}
		dir = parent
	}
}

// flattenKeepingLines collapses the document's whitespace so prose that was wrapped reads as one
// line, and records for every byte of the result the line it came from, which is what turns a
// marker found in the flat text back into a bound on the lines a list lives in.
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

// RequireDocFacts holds text, which is one region or one bullet of it, to every fact it must
// state; what names that text in each failure. Each miss is its own failure naming the fact,
// since a reader who has to fix this knows what to write from the fact and not from the string.
func RequireDocFacts(t testing.TB, what, text string, facts []Fact) {
	t.Helper()
	for _, f := range facts {
		if !strings.Contains(text, f.Phrase) {
			t.Errorf("%s does not state %s: it carries no %q. A reader of that part of the document is left to guess, "+
				"and a sentence elsewhere in the document is no answer, since this is the part they read", what, f.Fact, f.Phrase)
		}
	}
}

// RegionBullets reads the markdown bullets of a region, each as its whole content: the `- ` line
// and the indented lines under it, joined. A fact is then held to the bullet that carries it,
// which is what stops the sentence written for one item of a list from standing in for another's.
func RegionBullets(region []string) []string {
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

// BulletNaming is the one bullet of a region that names what the check is about; what names the
// region in the failure. Two bullets naming it, or none, is a failure of its own, and the answer
// is then false: the facts below it would otherwise be read against a bullet nobody chose.
func BulletNaming(t testing.TB, what string, bullets []string, name string) (string, bool) {
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
