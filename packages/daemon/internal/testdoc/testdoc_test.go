package testdoc

import (
	"fmt"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// A region whose end marker wraps runs through the line the marker finishes on. Ending on the line
// the marker starts on, as one copy of this reader did, drops the wrapped tail of the last bullet:
// the facts that tail carries then read as missing from a document that states them.
func TestRegionRunsThroughTheLineItsEndMarkerFinishesOn(t *testing.T) {
	const doc = "Before the region.\n" +
		"The region starts\n" +
		"here and lists:\n" +
		"\n" +
		"- one bullet\n" +
		"  that wraps.\n" +
		"\n" +
		"- the last bullet, which ends the\n" +
		"  region here.\n" +
		"After the region.\n"
	lines, flat := region(t, "guide.md", doc, "the region", "The region starts here", "ends the region here.")

	if want := strings.Split(doc, "\n")[1:9]; !slices.Equal(lines, want) {
		t.Errorf("the region's lines are %q, want %q: from the line the start marker opens on through the line the end marker finishes on", lines, want)
	}
	if want := "The region starts here and lists: - one bullet that wraps. - the last bullet, which ends the region here."; flat != want {
		t.Errorf("the region's flattened text is %q, want %q", flat, want)
	}
	if bullets, want := RegionBullets(lines), []string{"one bullet that wraps.", "the last bullet, which ends the region here."}; !slices.Equal(bullets, want) {
		t.Errorf("the region's bullets are %q, want %q: each bullet whole, its indented lines joined to it", bullets, want)
	}
}

// A marker Region cannot place fails the test, naming the marker and the region. Each of these is
// a region a check would otherwise read wrong while passing: a start marker gone, one found twice
// so the first is read whichever was meant, and an end marker found only before the start.
func TestRegionFailsNamingTheMarkerItCannotPlace(t *testing.T) {
	const doc = "A first paragraph ends here.\nA second paragraph ends here.\n"
	for _, row := range []struct {
		name, start, end string
		want             []string
	}{
		{"a start marker gone", "A third paragraph", "ends here.", []string{`guide.md has no "A third paragraph"`, "the region"}},
		{"a start marker found twice", "paragraph", "ends here.", []string{`guide.md has "paragraph" more than once`, "the region"}},
		{"an end marker only before the start", "A second paragraph", "A first", []string{`guide.md has no "A first" after "A second paragraph"`, "the region"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			r := &recorder{TB: t}
			done := make(chan struct{})
			go func() {
				defer close(done)
				region(r, "guide.md", doc, "the region", row.start, row.end)
			}()
			<-done
			if r.fatal == "" {
				t.Fatalf("Region between %q and %q returned a region of %q instead of failing", row.start, row.end, doc)
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
