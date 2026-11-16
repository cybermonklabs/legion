package textcheck

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// The way back of store migration 20261111225552 stands in the migration's header, which ships
// it with the migration, and in a fenced block of docs/kubernetes.md, which is where an operator
// reads it at a rollback; internal/daemon's
// TestTheUpgradesWayBackBlockIsOneTextInTheHeaderAndTheGuide holds the two to one text. That
// comparison is in two halves, because a test that reads the guide puts the guide in its job's
// filter — a declared read applies to every compiled source file of a run — and a
// documentation-only edit would then start the whole daemon Go lane. The product half compares
// the migration's header with internal/daemon/upgrade_way_back_extract.txt, a copy of the guide's
// block committed beside it; this half holds that copy to the block the guide carries, byte for
// byte. Neither half alone is the guarantee: together they are the one the single test used to
// give.
//
// The block is found by the one line only this rollback's block holds rather than by its place
// in the document, which every edit above it moves: the guide carries a psql heredoc for several
// contracts' rollbacks, and the block of any other would pass for this one if the first fence
// were taken.
const (
	// The committed copy, read from the package it is about, and the guide, read under
	// ../../../.. by shippedText.
	wayBackExtract = "../daemon/upgrade_way_back_extract.txt"
	wayBackGuide   = "docs/kubernetes.md"
	// wayBackMark is the line that deletes the migration's version row, which no other rollback's
	// block carries. The version is internal/daemon's phaseEntryMigration, written out because
	// this file reaches no product package: importing internal/daemon would put the daemon, and
	// every file that package's own tests read, back in this job's filter, which is what
	// splitting the check took out. A version that moved cannot pass silently either way: the
	// header, the guide and the extract all carry it, so a guide whose block names another
	// version holds no fence this mark finds, which fails loudly.
	wayBackMark = "delete from schema_version where version = 20261111225552;"
)

// The committed copy of the guide's way back is the guide's own block, line for line. An edit to
// either side alone fails here: a line changed in the guide and not in the copy would otherwise
// never reach the migration's own check, which reads the copy and not the guide.
func TestTheWayBackExtractIsTheGuidesWayBackBlock(t *testing.T) {
	extract := quotedBody(t, wayBackExtract)
	if extract == "" {
		t.Fatalf("%s carries nothing under its `#` header: internal/daemon's check of the migration's "+
			"header has no copy of the guide's block to hold it to", wayBackExtract)
	}
	guide := wayBackBlockOfTheGuide(t)
	if extract == guide {
		return
	}
	t.Errorf("%s and %s's way back disagree:\n%s"+
		"the extract is the copy internal/daemon checks the migration's header against, so the guide "+
		"answers for the migration only while the two are equal; change them in one commit",
		wayBackExtract, wayBackGuide, wayBackBlockDifference(extract, guide))
}

// wayBackBlockOfTheGuide is the way back as docs/kubernetes.md carries it: the content of the
// fenced block holding wayBackMark, without the fence lines. A guide with no such block stops
// the test rather than skipping it: an absence here would read exactly like a pass, and what this
// check guards is the copy an operator runs at a rollback.
func wayBackBlockOfTheGuide(t *testing.T) string {
	t.Helper()
	var block []string
	inside := false
	for _, line := range strings.Split(shippedText(t, wayBackGuide), "\n") {
		switch {
		case !strings.HasPrefix(line, "```"):
			if inside {
				block = append(block, line)
			}
		case !inside:
			inside, block = true, nil
		default:
			if slices.Contains(block, wayBackMark) {
				return strings.Join(block, "\n")
			}
			inside = false
		}
	}
	t.Fatalf("%s carries no fenced block holding %q: that block is the way back an operator runs, "+
		"and %s is its copy", wayBackGuide, wayBackMark, wayBackExtract)
	return ""
}

// wayBackBlockDifference renders the lines the two copies disagree on, each with its number
// inside the block, so a failing run names the edit to make rather than two blocks to compare by
// eye. internal/daemon keeps a rendering of the same shape for the header and the copy; this file
// may not import that package (see wayBackMark above).
func wayBackBlockDifference(extract, guide string) string {
	extractLines, guideLines := strings.Split(extract, "\n"), strings.Split(guide, "\n")
	var difference strings.Builder
	for i := range max(len(extractLines), len(guideLines)) {
		copied, held := "", ""
		if i < len(extractLines) {
			copied = extractLines[i]
		}
		if i < len(guideLines) {
			held = guideLines[i]
		}
		if copied == held {
			continue
		}
		fmt.Fprintf(&difference, "line %d:\n\tthe extract: %q\n\tthe guide:   %q\n", i+1, copied, held)
	}
	return difference.String()
}
