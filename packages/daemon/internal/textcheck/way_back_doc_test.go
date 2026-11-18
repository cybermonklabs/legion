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
// The way back of contract 146 stands the same way, in the headers of its four migrations and in
// a fenced block of the guide of its own, and is held the same way: internal/daemon's
// TestTheArchitectEntryUpgradesWayBackBlockIsOneTextInItsHeadersAndTheGuide compares the four
// headers with internal/daemon/upgrade_architect_entry_way_back_extract.txt, and this file holds
// that copy to the guide's block.
//
// Each block is found by the one line only that rollback's block holds rather than by its place
// in the document, which every edit above it moves: the guide carries a psql heredoc for several
// contracts' rollbacks, and the block of any other would pass for the one meant if the first
// fence were taken.
const (
	// The committed copies, each read from the package it is about, and the guide, read under
	// ../../../.. by shippedText.
	wayBackExtract               = "../daemon/upgrade_way_back_extract.txt"
	architectEntryWayBackExtract = "../daemon/upgrade_architect_entry_way_back_extract.txt"
	wayBackGuide                 = "docs/kubernetes.md"
	// wayBackMark is the line that deletes the migration's version row, which no other rollback's
	// block carries. The version is internal/daemon's phaseEntryMigration, written out because
	// this file reaches no product package: importing internal/daemon would put the daemon, and
	// every file that package's own tests read, back in this job's filter, which is what
	// splitting the check took out. A version that moved cannot pass silently either way: the
	// header, the guide and the extract all carry it, so a guide whose block names another
	// version holds no fence this mark finds, which fails loudly.
	wayBackMark = "delete from schema_version where version = 20261111225552;"
	// architectEntryWayBackMark is the line that deletes the version row of
	// 20261114061900_phase_entry_verbatim, the newest of contract 146's four migrations and the
	// first that block reverses, which no other rollback's block carries. It is written out for
	// wayBackMark's reason.
	architectEntryWayBackMark = "delete from schema_version where version = 20261114061900;"
)

// The committed copy of the guide's way back is the guide's own block, line for line. An edit to
// either side alone fails here: a line changed in the guide and not in the copy would otherwise
// never reach the migration's own check, which reads the copy and not the guide.
func TestTheWayBackExtractIsTheGuidesWayBackBlock(t *testing.T) {
	assertTheExtractIsTheGuidesBlock(t, wayBackExtract, wayBackMark)
}

// The committed copy of contract 146's way back is the guide's own block, line for line, for the
// same reason: the four migrations' own check reads the copy and not the guide.
func TestTheArchitectEntryWayBackExtractIsTheGuidesWayBackBlock(t *testing.T) {
	assertTheExtractIsTheGuidesBlock(t, architectEntryWayBackExtract, architectEntryWayBackMark)
}

// assertTheExtractIsTheGuidesBlock holds the body of extract, a committed copy, to the fenced
// block of the guide holding mark, and names each line the two disagree on.
func assertTheExtractIsTheGuidesBlock(t *testing.T, extract, mark string) {
	t.Helper()
	copied := quotedBody(t, extract)
	if copied == "" {
		t.Fatalf("%s carries nothing under its `#` header: internal/daemon's check of the migration "+
			"headers has no copy of the guide's block to hold them to", extract)
	}
	guide := wayBackBlockOfTheGuide(t, mark, extract)
	if copied == guide {
		return
	}
	t.Errorf("%s and %s's way back disagree:\n%s"+
		"the extract is the copy internal/daemon checks the migration headers against, so the guide "+
		"answers for the migrations only while the two are equal; change them in one commit",
		extract, wayBackGuide, wayBackBlockDifference(copied, guide))
}

// wayBackBlockOfTheGuide is a way back as docs/kubernetes.md carries it: the content of the
// fenced block holding mark, without the fence lines. extract is the committed copy of that
// block, which a failure names. A guide with no such block stops the test rather than skipping
// it: an absence here would read exactly like a pass, and what this check guards is the copy an
// operator runs at a rollback.
func wayBackBlockOfTheGuide(t *testing.T, mark, extract string) string {
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
			if slices.Contains(block, mark) {
				return strings.Join(block, "\n")
			}
			inside = false
		}
	}
	t.Fatalf("%s carries no fenced block holding %q: that block is the way back an operator runs, "+
		"and %s is its copy", wayBackGuide, mark, extract)
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
