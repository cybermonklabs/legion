// Package testdoc holds the reading of a guide's region that the daemon's doc tests share. A doc
// test holds one region of a guide, bounded by its first and last words, to the facts that region
// must state, and the bound is the whole structure: a phrase found anywhere in a document of
// thousands of lines would answer a check about a paragraph that says the opposite.
//
// A doc test reads no guide. It reads its package's extract, a copy of the regions it checks in
// the guide's own bytes, committed in the test's own directory, and internal/textcheck holds each
// region of that copy to the guide's lines between the same two markers. A test that read the
// guide would put the guide in its job's filter, since a declared read applies to every compiled
// source file of a run, and a documentation-only edit would then start the whole daemon Go lane;
// a read that walked up to the repository root is one the filter check cannot see, so CI would
// stop running the test on a guide-only edit at all.
//
// Region finds the region an extract names by its two markers and fails by naming what it cannot
// place; ExtractRegions reads an extract into all its regions, which Region and
// internal/textcheck's comparison with the guide both read it by; FlattenKeepingLines and
// MarkedRun find the run of a document's lines two markers bound, which Region and that
// comparison's search of the guide both bound a region by; RequireDocFacts holds a region, or one
// bullet of it, to its facts; RegionBullets and BulletNaming read a region that is a list as the
// list it is, so each fact is held to the bullet that carries it. Which facts a region must state
// stays each test's own, and so does a list whose shape is its own, such as the reclaim
// paragraph's two lists under their headings.
package testdoc

import (
	"errors"
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

// The two lines that open a region of an extract. regionFrom and the region's start marker make
// the first, regionTo and its end marker the line under it, and every line under those two, to
// the next regionFrom line, is the region in the guide's own bytes; above the first region an
// extract carries its `#` header. internal/textcheck reads the extracts through ExtractRegions
// too, and finds the guide's lines by the markers they name.
const (
	regionFrom = "from: "
	regionTo   = "to: "
)

// Region is the region of extract, a file in the calling test's own package directory, that the
// extract names by the markers start and end, as its lines and as one flattened string; what
// names the region in every failure. Every check reads a region and never the whole document,
// which is the miss these checks exist against.
//
// It fails by name rather than skipping or passing. An extract that cannot be read, a region it
// does not name by both markers, or a marker that region does not carry, is a region that was
// moved or reworded, and a check that cannot find what it bounds is a check that stopped
// checking, which reads exactly like a pass. The region is named by the test's own two markers
// because internal/textcheck finds the guide's lines by them: a marker changed in the test alone
// would otherwise hold the facts to one bound and the guide to another. A start marker found
// twice in the region fails as well, since the check would read whichever comes first.
//
// The markers are prose and may straddle line ends, so they are found in a flattened copy and
// carried back to the lines they cover, which is what a list read as a list needs
// (RegionBullets). The lines run from the one the start marker opens on through the one the end
// marker finishes on, so a marker that wraps takes its whole last line with it; the flattened
// string runs from the start marker's first word through the end marker's last.
func Region(t testing.TB, extract, what, start, end string) ([]string, string) {
	t.Helper()
	text, err := os.ReadFile(filepath.FromSlash(extract))
	if err != nil {
		t.Fatalf("read %s: %v", extract, err)
	}
	return regionOfExtract(t, extract, string(text), what, start, end)
}

// regionOfExtract is Region of an extract already read, text being the content of extract.
func regionOfExtract(t testing.TB, extract, text, what, start, end string) ([]string, string) {
	t.Helper()
	var named []string
	for _, r := range ExtractRegions(t, extract, text) {
		if r.Start == start && r.End == end {
			named = append(named, strings.Join(r.Lines, "\n"))
		}
	}
	if len(named) != 1 {
		t.Fatalf("%s has %d regions from %q to %q, want exactly 1, so %s cannot be read: the extract names "+
			"each region by the two markers its test bounds it by, and internal/textcheck holds the guide's "+
			"lines between those markers to it, so change a marker in the test, the extract and the guide "+
			"together", extract, len(named), start, end, what)
	}
	return region(t, extract, named[0], what, start, end)
}

// ExtractRegion is one region of an extract: the two markers its opening lines name, and the
// guide's lines under them.
type ExtractRegion struct {
	Start, End string
	Lines      []string
}

// ExtractRegions reads text, the content of extract, into its regions. Above the first regionFrom
// line a line is the `#` header or blank; a regionFrom line opens a region, and the line under it
// must be that region's regionTo line; every other line is the open region's own, and the blank
// lines that end a region are dropped, since a region ends on the line holding its end marker. An
// extract whose shape cannot be read that way fails the test by name, as Region does. An extract
// that holds no region at all is none of those failures, so a caller that reads every region, as
// internal/textcheck does, fails that itself.
//
// Region and internal/textcheck read the extracts by this one parse, and that does not let the
// two drift together: textcheck holds each region the parse answers to the guide's own lines
// between the region's markers, found in the guide by a search of its own, so a parse that
// dropped, added or moved a line would hand it a region the guide does not carry there, and it
// fails.
func ExtractRegions(t testing.TB, extract, text string) []ExtractRegion {
	t.Helper()
	var regions []ExtractRegion
	for i, line := range strings.Split(text, "\n") {
		open := len(regions) - 1
		switch {
		case strings.HasPrefix(line, regionFrom):
			start := strings.TrimPrefix(line, regionFrom)
			if start == "" {
				t.Fatalf("%s:%d: %q names no start marker, so the region it opens cannot be found", extract, i+1, line)
			}
			regions = append(regions, ExtractRegion{Start: start})
		case open < 0:
			if line != "" && !strings.HasPrefix(line, "#") {
				t.Fatalf("%s:%d: %q stands above the first %q line, where only the `#` header is, so which "+
					"region it belongs to cannot be read", extract, i+1, line, regionFrom)
			}
		case regions[open].End == "":
			end, ok := strings.CutPrefix(line, regionTo)
			if !ok || end == "" {
				t.Fatalf("%s:%d: the region from %q has no %q line naming its end marker under its %q line, so "+
					"where it ends cannot be read", extract, i+1, regions[open].Start, regionTo, regionFrom)
			}
			regions[open].End = end
		default:
			regions[open].Lines = append(regions[open].Lines, line)
		}
	}
	for i, r := range regions {
		for len(r.Lines) > 0 && r.Lines[len(r.Lines)-1] == "" {
			r.Lines = r.Lines[:len(r.Lines)-1]
		}
		if r.End == "" || len(r.Lines) == 0 {
			t.Fatalf("%s's region from %q carries no %q line or no line of the guide, so it holds nothing a "+
				"check could read", extract, r.Start, regionTo)
		}
		regions[i].Lines = r.Lines
	}
	return regions
}

// region is the part of text, the content of doc, between the first and the last words of one
// region, as Region answers it.
func region(t testing.TB, doc, text, what, start, end string) ([]string, string) {
	t.Helper()
	lines := strings.Split(text, "\n")
	flat, lineOf := FlattenKeepingLines(lines)
	first, last, bounded, err := MarkedRun(flat, lineOf, start, end)
	switch {
	case errors.Is(err, ErrNoStartMarker):
		t.Fatalf("%s has no %q, so %s cannot be found: the region was reworded or removed, and this "+
			"check stops checking the moment it cannot find it", doc, start, what)
	case errors.Is(err, ErrStartMarkerTwice):
		t.Fatalf("%s has %q more than once, so it no longer says where %s starts: the check would read "+
			"whichever comes first and cannot tell that it read the wrong one", doc, start, what)
	case errors.Is(err, ErrNoEndMarker):
		t.Fatalf("%s has no %q after %q, so the end of %s moved: an unbounded search would pass on a "+
			"sentence anywhere in the document", doc, end, start, what)
	}
	return lines[first : last+1], bounded
}

// The ways two markers fail to bound a run of a document (MarkedRun). Each caller words the
// failure for its own reader, so these name the failure and nothing of what to do about it.
var (
	ErrNoStartMarker    = errors.New("the document has no start marker")
	ErrStartMarkerTwice = errors.New("the document has the start marker more than once")
	ErrNoEndMarker      = errors.New("the document has no end marker after its start marker")
)

// MarkedRun is the run of a document's lines that the markers start and end bound, found in flat
// and lineOf, the document as FlattenKeepingLines answers it: the indexes of the run's first and
// last lines, from the line the start marker opens on through the line the end marker finishes
// on, and the flattened text from the start marker's first word through the end marker's last. A
// start marker the document does not carry, one it carries more than once, and an end marker it
// does not carry after the start marker answer ErrNoStartMarker, ErrStartMarkerTwice and
// ErrNoEndMarker, since a run bounded by whichever start comes first, or by no end, would hold a
// check to lines nobody chose.
func MarkedRun(flat string, lineOf []int, start, end string) (first, last int, text string, err error) {
	startOffset := strings.Index(flat, start)
	if startOffset < 0 {
		return 0, 0, "", ErrNoStartMarker
	}
	if strings.Contains(flat[startOffset+len(start):], start) {
		return 0, 0, "", ErrStartMarkerTwice
	}
	endOffset := strings.Index(flat[startOffset:], end)
	if endOffset < 0 {
		return 0, 0, "", ErrNoEndMarker
	}
	lastOffset := startOffset + endOffset + len(end) - 1
	return lineOf[startOffset], lineOf[lastOffset], flat[startOffset : lastOffset+1], nil
}

// FlattenKeepingLines collapses the document's whitespace so prose that was wrapped reads as one
// line, and records for every byte of the result the line it came from, which is what turns a
// marker found in the flat text back into a bound on the lines a list lives in.
func FlattenKeepingLines(lines []string) (string, []int) {
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
