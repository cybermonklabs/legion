package textcheck

//ci:reads docs/kubernetes.md TestTheKubernetesDocExtractsAreTheGuidesOwnLines reads it under ../../../.. (shippedText)
//ci:reads packages/daemon/internal/runtime/sandbox/eviction_doc_extract.txt TestTheKubernetesDocExtractsAreTheGuidesOwnLines reads it as ../runtime/sandbox/eviction_doc_extract.txt, the committed copy it holds the guide's regions to (readExtract)
//ci:reads packages/daemon/internal/launcher/testpg_disk_doc_extract.txt TestTheKubernetesDocExtractsAreTheGuidesOwnLines reads it as ../launcher/testpg_disk_doc_extract.txt, the committed copy it holds the guide's region to (readExtract)

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testdoc"
)

// Two doc tests hold regions of docs/kubernetes.md to the code those regions document.
// internal/runtime/sandbox's eviction_doc_test.go holds the extended-run-time annotation's bullet
// and the guide's three other statements of that rule, the AWS paragraph and the paragraphs on an
// end from outside the pod to the runtime's annotations and pod-end reasons; and
// internal/launcher's testpg_disk_doc_test.go holds the line a reset of the test database writes,
// and the command that shows it, to the program's name. That comparison is in two halves, because
// a test that reads the guide puts the guide in its job's filter — a declared read applies to every
// compiled source file of a run — and a documentation-only edit would then start the whole daemon
// Go lane. The product halves read a copy of their regions committed beside each of the two
// packages (eviction_doc_extract.txt in internal/runtime/sandbox, and testpg_disk_doc_extract.txt
// in internal/launcher) through internal/testdoc; this half holds each region of every copy to the
// guide itself. Neither half alone is the guarantee: together they hold each region to the guide
// and to the code.
//
// What it proves: each region of any copy is the run of the guide's lines, byte for byte, that the
// region's own two markers bound, and those are the markers its product test reads it by, since
// internal/testdoc fails a test whose markers name no region of its copy. What it does not: that a
// region states a fact or a constant, which is the product halves', and anything of a region no
// copy carries.
const (
	// The committed copies, each read from the package whose test reads it, and the guide, read
	// under ../../../.. by shippedText.
	sandboxEvictionExtract    = "../runtime/sandbox/eviction_doc_extract.txt"
	launcherTestPGDiskExtract = "../launcher/testpg_disk_doc_extract.txt"
	extractsGuide             = "docs/kubernetes.md"
)

// Each region of the committed copies is the guide's own lines between the region's two
// markers. An edit to either side alone fails here: a sentence reworded in the guide and not in a
// copy would otherwise never reach the product test, which reads the copy and not the guide; and a
// region copied from another place, or a marker the guide no longer bounds that region by, would
// hold that test's facts to lines the guide does not carry where an operator reads them. The
// failure prints the guide's lines, so the fix is a paste.
func TestTheKubernetesDocExtractsAreTheGuidesOwnLines(t *testing.T) {
	guide := strings.Split(shippedText(t, extractsGuide), "\n")
	flat, lineOf := testdoc.FlattenKeepingLines(guide)
	for _, extract := range []string{sandboxEvictionExtract, launcherTestPGDiskExtract} {
		for _, region := range kubernetesExtractRegions(t, extract) {
			first, last, ok := kubernetesGuideRun(t, flat, lineOf, extract, region)
			if !ok {
				continue
			}
			bounded := guide[first : last+1]
			if slices.Equal(region.Lines, bounded) {
				continue
			}
			paste := strings.Join(bounded, "\n")
			if at := kubernetesGuideRunAt(guide, region.Lines); at >= 0 {
				t.Errorf("%s's region from %q to %q is lines %d-%d of %s, but the guide's lines between those "+
					"two markers are %d-%d: the doc test beside the copy reads the region by those markers, so "+
					"its facts would be held to lines the guide carries elsewhere. Replace the region with the "+
					"lines the markers bound:\n\n%s",
					extract, region.Start, region.End, at+1, at+len(region.Lines), extractsGuide,
					first+1, last+1, paste)
				continue
			}
			t.Errorf("%s carries no run of lines that is %s's region from %q to %q, byte for byte; %s. The doc "+
				"test beside the copy reads the region and not the guide, so the two answer for the guide only "+
				"while they are equal. Repaste the region from the guide's lines %d-%d, in the same commit as "+
				"the edit that changed them:\n\n%s",
				extractsGuide, extract, region.Start, region.End,
				kubernetesFirstDifference(region.Lines, bounded), first+1, last+1, paste)
		}
	}
}

// kubernetesExtractRegions reads a committed copy into its regions by the one parse the product
// halves read it by (testdoc.ExtractRegions), which fails the test by name for a copy whose shape
// cannot be read. One parse for both halves lets no misreading pass: each region it answers is
// held here to the guide's own lines, found by this file's own search of the guide
// (kubernetesGuideRun), so a region the parse read wrong is one the guide does not carry between
// its markers.
//
// A copy that holds no region is the one failure the parse leaves to its caller, and it stops
// the test here by name: the file is still there, the parse matched nothing, and the product
// halves would then be held to regions nobody is reading, which reads exactly like a pass.
func kubernetesExtractRegions(t *testing.T, extract string) []testdoc.ExtractRegion {
	t.Helper()
	regions := testdoc.ExtractRegions(t, extract, readExtract(t, extract))
	if len(regions) == 0 {
		t.Fatalf("%s holds no region: its copy of the guide is gone, and the doc test beside it is held to "+
			"nothing", extract)
	}
	return regions
}

// kubernetesGuideRun is the run of the guide's lines that region's two markers bound, as the
// indexes of its first and last lines: from the line the start marker opens on through the line
// the end marker finishes on, the bound internal/testdoc reads the region by in the copy. The
// markers are prose and the guide is wrapped, so they are found in the flattened guide by
// internal/testdoc's own search (testdoc.FlattenKeepingLines, testdoc.MarkedRun) and carried back
// to its lines. A marker the guide does not carry, or carries where it no longer bounds one run,
// fails naming it and answers false, so no region is compared with lines nobody chose.
func kubernetesGuideRun(t *testing.T, flat string, lineOf []int, extract string, region testdoc.ExtractRegion) (first, last int, ok bool) {
	t.Helper()
	first, last, _, err := testdoc.MarkedRun(flat, lineOf, region.Start, region.End)
	switch {
	case err == nil:
		return first, last, true
	case errors.Is(err, testdoc.ErrNoStartMarker):
		t.Errorf("%s has no %q, the start marker of %s's region: the guide's paragraph was reworded or moved, "+
			"and the doc test beside the copy reads words the guide no longer ships. Repaste the region and "+
			"its markers from the guide as it reads now, and change that test's marker with them",
			extractsGuide, region.Start, extract)
	case errors.Is(err, testdoc.ErrStartMarkerTwice):
		t.Errorf("%s has %q more than once, so it no longer says where %s's region starts: the region could "+
			"be held to whichever comes first. Choose a start marker the guide carries once, in the copy and "+
			"in the doc test beside it", extractsGuide, region.Start, extract)
	case errors.Is(err, testdoc.ErrNoEndMarker):
		t.Errorf("%s has no %q after %q, so the end of %s's region moved: an unbounded region would hold the "+
			"doc test's facts to whatever the guide carries after it", extractsGuide, region.End, region.Start,
			extract)
	}
	return 0, 0, false
}

// kubernetesGuideRunAt is the index of the first guide line of a run equal to lines, or -1 when
// the guide carries no such run: where a region that is not the one its markers bound was copied
// from.
func kubernetesGuideRunAt(guide, lines []string) int {
	for at := 0; at+len(lines) <= len(guide); at++ {
		if slices.Equal(guide[at:at+len(lines)], lines) {
			return at
		}
	}
	return -1
}

// kubernetesFirstDifference names the first line a region and the guide's run disagree on, so a
// failing run points at the edit rather than at two blocks to compare by eye.
func kubernetesFirstDifference(region, guide []string) string {
	line := func(lines []string, i int) string {
		if i < len(lines) {
			return fmt.Sprintf("%q", lines[i])
		}
		return "no line"
	}
	for i := range max(len(region), len(guide)) {
		if i >= len(region) || i >= len(guide) || region[i] != guide[i] {
			return fmt.Sprintf("its line %d is %s in the copy and %s in the guide", i+1, line(region, i), line(guide, i))
		}
	}
	return "the two are equal"
}
