package textcheck

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The waiting line ships in three kinds of text outside packages/daemon: the contract a caller
// is told the place arrives at, the two words a root's architect reads about the wait, and the
// `legion state` sample an operator reads the count in. Three tests of the daemon hold those
// texts to the daemon's own constants, and none of them reads a text any more: each reads a copy
// committed beside it - internal/api/waiting_line_contract_extract.txt,
// internal/workflow/waiting_line_words_extract.txt and cmd/legion/awaiting_summary_extract.txt -
// because a test that reads a shipped text puts that text in its job's filter, a declared read
// applying to every compiled source file of a run, and an edit of AGENTS.md or a guide page
// would then start the whole 20-minute daemon Go lane. This file is the other half, in the job
// whose subject is a file's words, where a documentation edit is the run that should start: it
// holds each copy to the text it was taken from, and carries the one check no copy can carry.
// Neither half alone is the guarantee: together they are the one the three tests used to give.
// (INFRA-1413.)
//
// What these four tests prove: that the api package's copy is every window of the seven texts
// that names the field's contract, in their own bytes; that every quotation of the workflow
// package's copy is still in the file it names, raw or collapsed as the section says, and that
// no file of either list has lost its section; that the guide still carries the sample line
// cmd/legion renders; and that no shipped text of either list claims a position in the line,
// which is a whole-file negative that no excerpt can prove and so could not stay with the three
// product tests.
//
// What they do not prove: that a sentence around a quoted name is true, which stays a reader's
// to judge; that the copies say what the daemon declares, which is the product half's - a
// renamed constant fails there and not here; and anything of a text these lists do not name.
//
// This file imports no product package of packages/daemon and writes out as literals the
// strings it needs from one. That is the rule the split is made of: importing internal/api or
// internal/workflow would put those packages' own inputs back in this job's filter, which is
// exactly what the split took out (reclaim_doc_test.go keeps the same rule for the same reason).
const (
	// The committed copies, each read from the package it was committed beside, and the window
	// rule the api package's test reads a text by.
	waitingLineContractExtract = "../api/waiting_line_contract_extract.txt"
	waitingLineWordsExtract    = "../workflow/waiting_line_words_extract.txt"
	awaitingSummaryExtract     = "../../cmd/legion/awaiting_summary_extract.txt"
	runningLegionGuide         = "docs/site/src/content/docs/legion/running-legion.md"
	// The window internal/api/waiting_line_test.go takes around a number it finds, and the
	// pattern it finds one by. The two rules are one rule in two files: a change to either alone
	// leaves this half computing windows the api scan then reads differently, and the comparison
	// below is what reports it - loudly, since every section would move at once.
	waitingLineNamed  = `(?:contract|below) (\d+)`
	waitingLineWindow = 220
	// api.WaitingLineField, written out because this file reaches no product package. What the
	// literal cannot catch: a rename of that constant, which would leave this file comparing the
	// texts against a word the daemon no longer serves. internal/api's
	// TestTheShippedTextsNameTheWaitingLinesOwnContract catches that one - it reads the constant,
	// and a renamed field puts it at no window of the daemon's own sources, which its
	// `checked == 0` guard fails on.
	waitingLineField = "waitingLine"
	// workflow.WaitingForSlotNoticeKind and record.WaitingLineBand, written out for the same
	// reason. A rename of either is internal/workflow's
	// TestTheShippedWordsNameTheWaitingLineNotice to catch: it reads both constants and holds the
	// files of packages/daemon to them directly, so the rename fails there while the quotations
	// here stay the texts' own words.
	waitingForSlotKind = "waiting-for-slot"
	waitingLineBand    = "among the issues whose work came back, oldest first"
)

// The seven shipped texts outside packages/daemon that internal/api's contract test used to read
// itself, in its own order: every window of these is what the committed copy must hold. A text
// that yields no window has no section in the copy, and troubleshooting.md is that text today,
// which is why it stays on this list - a window it ever gains is one the copy must gain with it.
var waitingLineContractTexts = []string{
	"packages/contracts/src/legion-api.ts",
	"packages/pi-legion/AGENTS.md",
	runningLegionGuide,
	"docs/site/src/content/docs/legion/troubleshooting.md",
	"packages/contracts/.changes/unreleased/pr2989-Added-20261108-074056.yaml",
	"packages/pi-legion/.changes/unreleased/pr2989-Changed-20261108-074108.yaml",
	"packages/claude-envoy/.changes/unreleased/pr2989-Changed-20261108-074111.yaml",
}

// The shipped texts outside packages/daemon of internal/workflow's two lists: `naming`, the
// files that name the notice and the field, and `band`, the files that state where the line puts
// an issue whose work came back. The lists are written out here rather than read from that test,
// which this file may not import, and they are what the committed copy is held to: a section
// deleted there would otherwise take that file's coverage with it, since the product half reads
// the sections and no longer the files. They are also what the ban below reads, over the whole
// of each file.
var (
	waitingLineNamingTexts = []string{
		"skills/legion-architect/SKILL.md",
		"skills/legion-architect/references/wake-routing.md",
		"skills/legion-architect/references/design-gate.md",
		"skills/legion-architect/references/acceptance.md",
		runningLegionGuide,
		"docs/site/src/content/docs/legion/troubleshooting.md",
		"docs/site/src/content/docs/legion/concepts.md",
	}
	waitingLineBandTexts = []string{
		"AGENTS.md",
		runningLegionGuide,
		"docs/site/src/content/docs/legion/troubleshooting.md",
		"packages/contracts/src/legion-api.ts",
		"skills/legion-controller/references/admission.md",
	}
)

// quotedText is one section of a committed copy: the repository-relative path it was taken from
// and its body, the bytes under the section's own `#` line.
type quotedText struct {
	path, body string
}

// The api package's copy is every window the seven texts yield, in their bytes and their order.
// An edit to a text alone fails here: the window moves, the copy stays, and the number the api
// package scans is then one no text carries any more - a renumber correcting a guide and not the
// copy would otherwise pass both halves. The failure prints what the texts yield, so the fix is
// a paste rather than a hunt.
func TestTheWaitingLineContractExtractIsEveryWindowOfTheShippedTexts(t *testing.T) {
	want := waitingLineContractWindows(t)
	if len(want) == 0 {
		t.Fatalf("no shipped text names a contract beside %s: the copy %s answers for nothing, and "+
			"internal/api's check of it proves nothing", waitingLineField, waitingLineContractExtract)
	}
	got := quotedSections(t, waitingLineContractExtract, func(marker string) (quotedText, bool) {
		if !slices.Contains(waitingLineContractTexts, marker) {
			return quotedText{}, false
		}
		return quotedText{path: marker}, true
	})
	if slices.Equal(got, want) {
		return
	}
	differs := waitingLineContractExtract
	for i := range max(len(got), len(want)) {
		if i >= len(got) || i >= len(want) || got[i] != want[i] {
			if i < len(want) {
				differs = want[i].path
			} else {
				differs = got[i].path
			}
			break
		}
	}
	t.Errorf("%s is not what the shipped texts say, from %s on: the windows a text yields are read "+
		"by internal/api's TestTheShippedTextsNameTheWaitingLinesOwnContract out of this copy and "+
		"never out of the text, so the two answer for the texts only while they are equal. Replace "+
		"everything under that file's `#` header with:\n\n%s",
		waitingLineContractExtract, differs, renderQuotedTexts(want))
}

// waitingLineContractWindows is every window the seven texts yield under the rule internal/api's
// test reads a text by: a `contract <N>` or a bare `below <N>` whose 220 bytes on either side
// name the field and name neither the retired stranded state nor the stranded report, which is
// the other contract these same texts discuss. The parse is that test's own, written again here
// rather than shared, because a copy held to its texts by the reader that reads the copy can
// drift with it and still agree - and because the two live in packages this file may not import.
func waitingLineContractWindows(t *testing.T) []quotedText {
	t.Helper()
	named := regexp.MustCompile(waitingLineNamed)
	var windows []quotedText
	for _, name := range waitingLineContractTexts {
		text := shippedText(t, name)
		for _, at := range named.FindAllStringSubmatchIndex(text, -1) {
			near := text[max(0, at[0]-waitingLineWindow):min(len(text), at[1]+waitingLineWindow)]
			if !strings.Contains(near, waitingLineField) ||
				strings.Contains(near, "retired") || strings.Contains(near, "stranded") {
				continue
			}
			windows = append(windows, quotedText{name, strings.TrimRight(near, "\n")})
		}
	}
	return windows
}

// Every quotation of the workflow package's copy is still in the file it names, in the form the
// section declares, and every file of either list still has a section. The product half reads
// this copy and not the files, so a sentence reworded in a text and not here leaves that half
// holding words nobody ships, and a section deleted here leaves it holding nothing at all for
// that file - both are failures of this test and of nothing else.
func TestTheWaitingLineWordsExtractIsWhatTheShippedTextsSay(t *testing.T) {
	sections := waitingLineWordsSections(t)
	for _, section := range sections {
		path, list, form, _ := wordsSectionParts(section.path)
		text := shippedText(t, path)
		if form == "collapsed" {
			text = onOneLine(text)
		}
		if !strings.Contains(text, section.body) {
			t.Errorf("%s no longer carries its %s quotation in %s form, which %s is holding "+
				"internal/workflow's check to:\n  %s\nquote the file as it reads now, in the same "+
				"commit as the edit that moved it", path, list, form, waitingLineWordsExtract, section.body)
		}
	}
	for _, list := range []struct {
		name  string
		files []string
	}{{"naming", waitingLineNamingTexts}, {"band", waitingLineBandTexts}} {
		for _, file := range list.files {
			if slices.ContainsFunc(sections, func(section quotedText) bool {
				path, named, _, _ := wordsSectionParts(section.path)
				return path == file && named == list.name
			}) {
				continue
			}
			t.Errorf("%s has no `%s` section for %s: internal/workflow's check reads this copy and "+
				"not that file, so the file is held to nothing until a section quotes it again",
				waitingLineWordsExtract, list.name, file)
		}
	}
}

// waitingLineWordsSections reads the workflow package's copy, each section keyed by its whole
// `<path> <list> <form>` marker. The marker is kept whole and split by wordsSectionParts, so a
// marker this file does not understand fails loudly rather than standing for a file nobody
// checks.
func waitingLineWordsSections(t *testing.T) []quotedText {
	t.Helper()
	return quotedSections(t, waitingLineWordsExtract, func(marker string) (quotedText, bool) {
		path, list, form, ok := wordsSectionParts(marker)
		if !ok {
			return quotedText{}, false
		}
		if !slices.Contains(waitingLineNamingTexts, path) && !slices.Contains(waitingLineBandTexts, path) {
			t.Fatalf("%s quotes %s, which neither list of internal/workflow's check names: a section "+
				"no list reads is held to that file by nothing", waitingLineWordsExtract, path)
		}
		if (list == "naming") != (form == "raw") {
			t.Fatalf("%s quotes %s as `%s %s`: a `naming` section is the file's raw bytes and a "+
				"`band` section its collapsed form, as the product half reads each list",
				waitingLineWordsExtract, path, list, form)
		}
		return quotedText{path: marker}, true
	})
}

// wordsSectionParts reads a section marker of the workflow package's copy: the path it quotes,
// which of the two lists it answers for, and whether its body is the file's raw bytes or its
// collapsed form.
func wordsSectionParts(marker string) (path, list, form string, ok bool) {
	parts := strings.Fields(marker)
	if len(parts) != 3 || !slices.Contains([]string{"naming", "band"}, parts[1]) ||
		!slices.Contains([]string{"raw", "collapsed"}, parts[2]) {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

// The guide still prints the `legion state` sample cmd/legion's awaitingSummary is held to. That
// test finds its rendered line in the committed copy; what keeps the copy the guide's is this.
func TestTheAwaitingSummaryExtractIsTheGuidesWaitingLineSample(t *testing.T) {
	sample := quotedBody(t, awaitingSummaryExtract)
	if sample == "" {
		t.Fatalf("%s carries nothing under its `#` header: cmd/legion's check of the sample has "+
			"nothing to find its line in", awaitingSummaryExtract)
	}
	if !strings.Contains(shippedText(t, runningLegionGuide), sample) {
		t.Errorf("%s is no longer in %s, which cmd/legion's TestTheShippedWordsNameTheWaitingLineCount "+
			"reads it for:\n%s\nquote the guide's sample as it prints now, in the same commit as the "+
			"edit that changed it", awaitingSummaryExtract, runningLegionGuide, sample)
	}
}

// No shipped text of either of internal/workflow's lists claims a position in the waiting line.
// This is the half of that test no copy can carry: a ban is a whole-file negative, and an
// excerpt proves nothing about the sentence beside it - AGENTS.md once carried the placement
// word for word two lines below a sentence claiming a position, and the presence check passed on
// the neighbouring sentence's words. The rule those texts keep is that a text states the
// placement, "among the issues whose work came back, oldest first", with a tree root's own first
// phase placed at the moment its gate opened, and never the position that placement gives an
// issue, which depends on what else waits.
//
// What it proves: no file of either list carries one of the four wordings that claim a position.
// What it does not: whether what a file says instead is true, which stays a reader's to judge,
// and a wrong wording nobody has written yet, since the list holds the phrases these texts used
// and no more. internal/workflow keeps the same ban over the files of packages/daemon, which are
// its own job's inputs either way.
func TestNoShippedTextClaimsAPositionInTheWaitingLine(t *testing.T) {
	// Lower case, since the text is lower-cased before the comparison: a sentence that opens on
	// one of these is banned as the one that carries it mid-sentence is.
	banned := []string{
		"front of the waiting line",
		"front of the line",
		"front of the whole line",
		"never first in",
	}
	read := map[string]bool{}
	for _, list := range [][]string{waitingLineNamingTexts, waitingLineBandTexts} {
		for _, file := range list {
			if read[file] {
				continue
			}
			read[file] = true
			text := strings.ToLower(onOneLine(shippedText(t, file)))
			for _, phrase := range banned {
				if !strings.Contains(text, phrase) {
					continue
				}
				t.Errorf("%s says %q, which no text the waiting line's checks list may say: such a "+
					"text states where the line puts an issue - %q, a tree root's own first phase "+
					"placed at the moment its gate opened - and never the position that gives it, "+
					"which depends on what else waits.", file, phrase, waitingLineBand)
			}
		}
	}
}

// quotedSections reads a committed copy into its sections. A line that opens with `# ` is a
// section marker when section says it is one, and every line under it, to the next marker, is
// that section's body; the lines before the first marker are the file's `#` header. A `# ` line
// inside a section that no marker rule claims is that section's own byte, since a body is the
// quoted file's bytes and a quotation may open a line with a hash.
func quotedSections(t *testing.T, file string, section func(marker string) (quotedText, bool)) []quotedText {
	t.Helper()
	var sections []quotedText
	at := -1
	for _, line := range strings.Split(readExtract(t, file), "\n") {
		if marker, hashed := strings.CutPrefix(line, "# "); hashed {
			if opened, ok := section(strings.TrimSpace(marker)); ok {
				sections, at = append(sections, opened), len(sections)
				continue
			}
		}
		if at < 0 {
			continue
		}
		sections[at].body += line + "\n"
	}
	for i := range sections {
		sections[i].body = strings.TrimRight(sections[i].body, "\n")
	}
	if len(sections) == 0 {
		t.Fatalf("%s holds no section: its copy of the shipped texts is gone, and the daemon's own "+
			"check that reads it is being held to nothing", file)
	}
	return sections
}

// quotedBody is a one-section copy: everything under the run of `#` lines the file opens with.
func quotedBody(t *testing.T, file string) string {
	t.Helper()
	body := readExtract(t, file)
	for strings.HasPrefix(body, "#") {
		_, body, _ = strings.Cut(body, "\n")
	}
	return strings.TrimRight(strings.TrimLeft(body, "\n"), "\n")
}

// readExtract reads a committed copy, which stands beside the package whose test is held to it.
func readExtract(t *testing.T, file string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.FromSlash(file))
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	return string(body)
}

// shippedText reads a text of the repository, four directories above this package.
func shippedText(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "..", "..", filepath.FromSlash(name)))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(body)
}

// onOneLine puts a file's prose on a single line, the way internal/workflow's check reads a
// wrapped fragment: every line loses a leading run of whitespace and one `//`, `*` or `#`
// comment marker, and the words are joined by single spaces. A fragment wrapped inside a comment
// is the same wording, and a read that left the markers in would see `work came // back` and
// report a true text as false. It is that test's rule written again, for the same reason its
// parse of the copy is: this file may not import the package that keeps it.
func onOneLine(text string) string {
	var words []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimLeft(line, " \t")
		for _, marker := range []string{"//", "*", "#"} {
			if rest, found := strings.CutPrefix(line, marker); found {
				line = rest
				break
			}
		}
		words = append(words, strings.Fields(line)...)
	}
	return strings.Join(words, " ")
}

// renderQuotedTexts writes sections the way a copy carries them, so a failure above prints what
// the texts yield and the fix is a paste.
func renderQuotedTexts(sections []quotedText) string {
	var out strings.Builder
	for _, section := range sections {
		out.WriteString("# " + section.path + "\n" + section.body + "\n\n")
	}
	return out.String()
}
