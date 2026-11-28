package testdoc

import (
	"fmt"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// Region answers the region its extract names by both markers, from the line the start marker
// opens on through the line the end marker finishes on. Ending on the line the marker starts on
// drops the wrapped tail of the last bullet, and the facts that tail carries then read as missing
// from a guide that states them. Reading another region of the extract than the one both markers
// name, or taking the blank line that closes a region into it, would hold a test's facts to lines
// the guide does not carry there.
func TestRegionReadsTheRegionItsExtractNamesThroughTheLineItsEndMarkerFinishesOn(t *testing.T) {
	const extract = "# A copy of two regions of a guide.\n" +
		"\n" +
		"from: A first region\n" +
		"to: ends the region here.\n" +
		"A first region ends the region here.\n" +
		"\n" +
		"from: The region starts here\n" +
		"to: ends the region here.\n" +
		"Before it. The region starts\n" +
		"here and lists:\n" +
		"\n" +
		"- one bullet\n" +
		"  that wraps.\n" +
		"\n" +
		"- the last bullet, which ends the\n" +
		"  region here. After it.\n" +
		"\n"
	lines, flat := regionOfExtract(t, "guide_extract.txt", extract, "the region", "The region starts here", "ends the region here.")

	if want := strings.Split(extract, "\n")[8:16]; !slices.Equal(lines, want) {
		t.Errorf("the region's lines are %q, want %q: the region both markers name, from the line the start marker opens on through the line the end marker finishes on", lines, want)
	}
	if want := "The region starts here and lists: - one bullet that wraps. - the last bullet, which ends the region here."; flat != want {
		t.Errorf("the region's flattened text is %q, want %q: from the start marker's first word through the end marker's last", flat, want)
	}
	if bullets, want := RegionBullets(lines), []string{"one bullet that wraps.", "the last bullet, which ends the region here. After it."}; !slices.Equal(bullets, want) {
		t.Errorf("the region's bullets are %q, want %q: each bullet whole, its indented lines joined to it", bullets, want)
	}
}

// What Region cannot place fails the test, naming it, the extract and the region. Each of these is
// a region a check would otherwise read wrong while passing: a region no longer named by the
// test's markers, or named twice, so the guide's lines textcheck holds and the lines read here can
// part; a start marker gone from the region, one found twice so the first is read whichever was
// meant, and an end marker found only before the start; and an extract whose shape says no longer
// where a region begins or ends.
func TestRegionFailsNamingWhatItCannotPlace(t *testing.T) {
	const twoParagraphs = "from: A first paragraph\n" +
		"to: ends here.\n" +
		"A first paragraph ends here.\n" +
		"A second paragraph ends here.\n"
	for _, row := range []struct {
		name, extract, start, end string
		want                      []string
	}{
		{"no region named by both markers", twoParagraphs, "A first paragraph", "ends there.",
			[]string{`guide_extract.txt has 0 regions from "A first paragraph" to "ends there."`, "the region"}},
		{"two regions named by both markers", twoParagraphs + "\n" + twoParagraphs, "A first paragraph", "ends here.",
			[]string{`guide_extract.txt has 2 regions from "A first paragraph" to "ends here."`, "the region"}},
		{"a start marker its region does not carry", "from: A third paragraph\nto: ends here.\nA first paragraph ends here.\n",
			"A third paragraph", "ends here.", []string{`guide_extract.txt has no "A third paragraph"`, "the region"}},
		{"a start marker found twice in its region", "from: paragraph\nto: ends here.\nA first paragraph ends here.\nA second paragraph ends here.\n",
			"paragraph", "ends here.", []string{`guide_extract.txt has "paragraph" more than once`, "the region"}},
		{"an end marker only before the start", "from: A second paragraph\nto: A first\nA first paragraph ends here.\nA second paragraph ends here.\n",
			"A second paragraph", "A first", []string{`guide_extract.txt has no "A first" after "A second paragraph"`, "the region"}},
		{"a region with no end marker line", "from: A first paragraph\nA first paragraph ends here.\n", "A first paragraph", "ends here.",
			[]string{`guide_extract.txt:2: the region from "A first paragraph" has no "to: " line`}},
		{"a line above the first region", "A first paragraph ends here.\n" + twoParagraphs, "A first paragraph", "ends here.",
			[]string{`guide_extract.txt:1: "A first paragraph ends here." stands above the first "from: " line`}},
		{"a region with no line of the guide", "from: A first paragraph\nto: ends here.\n\n", "A first paragraph", "ends here.",
			[]string{`guide_extract.txt's region from "A first paragraph" carries no "to: " line or no line of the guide`}},
	} {
		t.Run(row.name, func(t *testing.T) {
			r := &recorder{TB: t}
			done := make(chan struct{})
			go func() {
				defer close(done)
				regionOfExtract(r, "guide_extract.txt", row.extract, "the region", row.start, row.end)
			}()
			<-done
			if r.fatal == "" {
				t.Fatalf("Region from %q to %q returned a region of %q instead of failing", row.start, row.end, row.extract)
			}
			for _, want := range row.want {
				if !strings.Contains(r.fatal, want) {
					t.Errorf("Region failed with %q, want it to say %q", r.fatal, want)
				}
			}
		})
	}
}

// BulletNaming answers the one bullet naming what a check is about. Two bullets naming it, or
// none, fail the test and answer false, so the caller reads no facts against a bullet nobody
// chose: answering the first of two would hold one kind of end to the bullet written for another.
func TestBulletNamingWantsExactlyOneBullet(t *testing.T) {
	bullets := []string{"the cluster ends the pod", "the close ends the pod", "an idle release"}
	for _, row := range []struct {
		name, naming string
		want         string
		ok           bool
	}{
		{"one bullet naming it", "idle", "an idle release", true},
		{"two bullets naming it", "ends the pod", "", false},
		{"no bullet naming it", "a node drain", "", false},
	} {
		t.Run(row.name, func(t *testing.T) {
			r := &recorder{TB: t}
			bullet, ok := BulletNaming(r, "the list", bullets, row.naming)
			if bullet != row.want || ok != row.ok {
				t.Errorf("BulletNaming of %q answered %q, %t, want %q, %t", row.naming, bullet, ok, row.want, row.ok)
			}
			if failed := len(r.errors) > 0; failed == row.ok {
				t.Errorf("BulletNaming of %q reported %q, want a failure exactly when no one bullet names it", row.naming, r.errors)
			}
		})
	}
}

// recorder is t with its failures recorded rather than failing the running test. Fatalf ends the
// goroutine, as t.Fatalf does, so a row that expects one drives the helper from a goroutine of its
// own.
type recorder struct {
	testing.TB
	errors []string
	fatal  string
}

func (r *recorder) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func (r *recorder) Fatalf(format string, args ...any) {
	r.fatal = fmt.Sprintf(format, args...)
	runtime.Goexit()
}
