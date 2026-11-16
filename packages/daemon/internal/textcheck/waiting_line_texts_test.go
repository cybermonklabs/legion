package textcheck

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The waiting line ships in four kinds of text outside packages/daemon: the contract a caller
// is told the place arrives at, the fragment that states where the line puts an issue whose
// work came back, the `legion state` sample an operator reads the count in, and the guide's
// words for the waiting-line block that sample prints. Four tests of the daemon hold those
// texts to the daemon's own constants, and none of them reads a text any
// more: each reads a copy committed beside it - internal/api/waiting_line_contract_extract.txt,
// internal/workflow/waiting_line_words_extract.txt, cmd/legion/awaiting_summary_extract.txt and
// cmd/legion/waiting_line_block_extract.txt -
// because a test that reads a shipped text puts that text in its job's filter, a declared read
// applying to every compiled source file of a run, and an edit of AGENTS.md or a guide page
// would then start the whole 20-minute daemon Go lane. This file is the other half, in the job
// whose subject is a file's words, where a documentation edit is the run that should start: it
// holds each copy to the text it was taken from, and carries the one check no copy can carry.
// Neither half alone is the guarantee: together they are the one the four tests used to give.
// (INFRA-1413.)
//
// What these tests prove: that the api package's copy is every window of the seven texts
// that names the field's contract, in their own bytes; that every quotation of the workflow
// package's copy is still in the file it names, in the one-line form that copy quotes it in,
// and that no file of its list has lost its section; that the guide still carries the sample
// line cmd/legion renders and both passages its waiting-line block is held to; and that no
// shipped text of that list claims a position in the line,
// which is a whole-file negative that no excerpt can prove and so could not stay with the
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
	waitingLineBlockExtract    = "../../cmd/legion/waiting_line_block_extract.txt"
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
	// record.WaitingLineBand, written out for the same reason. A rename of it is
	// internal/workflow's TestTheShippedWordsNameTheWaitingLineBand to catch: it reads the
	// constant, holds the files of packages/daemon to it directly and holds every quotation of
	// the copy to it too, so the rename fails there while the quotations here stay the texts'
	// own words.
	waitingLineBand = "among the issues whose work came back, oldest first"
	// The one form word a section marker of the workflow package's copy carries, and the only
	// one either reader of that copy knows: a body in that form is the quoted file's prose on
	// one line. TestEveryWaitingLineWordsExtractSectionIsCollapsed below is what holds the copy
	// to it.
	collapsedForm = "collapsed"
)

// The seven shipped texts outside packages/daemon that internal/api's contract test used to read
// itself, in its own order: every window of these is what the committed copy must hold. A text
// that yields no window has no section in the copy, and whether a text is in that state is this
// check's to answer on every run rather than the list's to record: troubleshooting.md yielded
// none when this list was written and has a section today, which is why a text stays on the
// list either way - a window it gains is one the copy must gain with it.
var waitingLineContractTexts = []string{
	"packages/contracts/src/legion-api.ts",
	"packages/pi-legion/AGENTS.md",
	runningLegionGuide,
	"docs/site/src/content/docs/legion/troubleshooting.md",
	"packages/contracts/.changes/unreleased/pr2989-Added-20261108-074056.yaml",
	"packages/pi-legion/.changes/unreleased/pr2989-Changed-20261108-074108.yaml",
	"packages/claude-envoy/.changes/unreleased/pr2989-Changed-20261108-074111.yaml",
}

// The shipped texts outside packages/daemon that internal/workflow's band list names: the files
// that state where the line puts an issue whose work came back. The list is written out here
// rather than read from that test, which this file may not import, and it is what the committed
// copy is held to: a section deleted there would otherwise take that file's coverage with it,
// since the product half reads the sections and no longer the files. It is also what the ban
// below reads, over the whole of each file.
var waitingLineBandTexts = []string{
	"AGENTS.md",
	runningLegionGuide,
	"docs/site/src/content/docs/legion/troubleshooting.md",
	"packages/contracts/src/legion-api.ts",
	"skills/legion-controller/references/admission.md",
	"docs/site/src/content/docs/legion/concepts.md",
	"skills/legion-architect/references/acceptance.md",
	"skills/legion-controller/references/wakes.md",
}

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

// Every quotation of the workflow package's copy is still in the file it names, in the one-line
// form the copy quotes it in, and every file of the band list still has a section. The product
// half reads this copy and not the files, so a sentence reworded in a text and not here leaves
// that half holding words nobody ships, and a section deleted here leaves it holding nothing at
// all for that file - both are failures of this test and of nothing else.
func TestTheWaitingLineWordsExtractIsWhatTheShippedTextsSay(t *testing.T) {
	sections := waitingLineWordsSections(t)
	for _, section := range sections {
		if !strings.Contains(onOneLine(shippedText(t, section.path)), section.body) {
			t.Errorf("%s no longer carries the quotation %s is holding internal/workflow's check "+
				"to:\n  %s\nquote the file as it reads now, on one line, in the same commit as the "+
				"edit that moved it", section.path, waitingLineWordsExtract, section.body)
		}
	}
	for _, file := range waitingLineBandTexts {
		if slices.ContainsFunc(sections, func(section quotedText) bool { return section.path == file }) {
			continue
		}
		t.Errorf("%s has no section for %s: internal/workflow's check reads this copy and not that "+
			"file, so the file is held to nothing until a section quotes it again",
			waitingLineWordsExtract, file)
	}
}

// waitingLineWordsSections reads the workflow package's copy, each section keyed by the path its
// marker names. A `# ` line is a marker only in the copy's own shape, `<path> collapsed`, so a
// quotation that itself opens with a hash stays the body it is, and a marker naming a file the
// list does not read fails loudly rather than standing for a file nobody checks.
func waitingLineWordsSections(t *testing.T) []quotedText {
	t.Helper()
	return quotedSections(t, waitingLineWordsExtract, func(marker string) (quotedText, bool) {
		path, ok := wordsSectionPath(marker)
		if !ok {
			return quotedText{}, false
		}
		if !slices.Contains(waitingLineBandTexts, path) {
			t.Fatalf("%s quotes %s, which internal/workflow's band list does not name: a section no "+
				"list reads is held to that file by nothing", waitingLineWordsExtract, path)
		}
		return quotedText{path: path}, true
	})
}

// wordsSectionPath reads a section marker of the workflow package's copy, `<path> collapsed`:
// the path the section quotes, and the form word that says its body is that file's prose on one
// line, which is the form the product half reads a wrapped fragment in and the only form a
// quotation of one can be in.
func wordsSectionPath(marker string) (string, bool) {
	parts := strings.Fields(marker)
	if len(parts) != 2 || parts[1] != collapsedForm {
		return "", false
	}
	return parts[0], true
}

// Every section marker of the workflow package's copy is in the one form both of its readers
// know. internal/workflow's parse of that copy and this file's each open a section on a
// `# <path> collapsed` line and on nothing else, so a marker written in any other form is a
// section to neither: both fold it into the quotation above, and that quotation then stands for
// words its file never carried, reported against the wrong file. This rule refuses it where it
// is written, naming the line.
//
// It is keyed on the band list's own paths, and so stays narrow: a quoted body line that happens
// to open with a hash and two words is not a marker to either reader, and is one this test
// passes over unless the first of those words is a file the band list names.
func TestEveryWaitingLineWordsExtractSectionIsCollapsed(t *testing.T) {
	for i, line := range strings.Split(readExtract(t, waitingLineWordsExtract), "\n") {
		marker, hashed := strings.CutPrefix(line, "# ")
		parts := strings.Fields(marker)
		if !hashed || len(parts) != 2 || parts[1] == collapsedForm ||
			!slices.Contains(waitingLineBandTexts, parts[0]) {
			continue
		}
		t.Errorf("%s:%d quotes %s as `%s`: `%s` is the only form either reader of this copy knows "+
			"- internal/workflow's TestTheShippedWordsNameTheWaitingLineBand and this file's "+
			"TestTheWaitingLineWordsExtractIsWhatTheShippedTextsSay - and a line in any other form "+
			"opens no section for them, so its body joins the quotation above and that quotation "+
			"is then reported against a file it never came from",
			waitingLineWordsExtract, i+1, parts[0], parts[1], collapsedForm)
	}
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

// The guide still carries both passages cmd/legion's waiting-line block is held to: the block of
// its `legion state` sample, and the sentence naming the two lines the block takes instead.
// cmd/legion's TestTheShippedWordsNameTheWaitingLineBlock renders those four lines and finds
// them in the committed copy; what keeps the copy the guide's is this. The copy quotes two
// passages the guide keeps thirty lines apart, so it carries a section per passage and each is
// held to the guide on its own.
func TestTheWaitingLineBlockExtractIsTheGuidesWaitingLineBlock(t *testing.T) {
	guide := shippedText(t, runningLegionGuide)
	for _, passage := range quotedSections(t, waitingLineBlockExtract, func(marker string) (quotedText, bool) {
		if marker != runningLegionGuide {
			return quotedText{}, false
		}
		return quotedText{path: marker}, true
	}) {
		if !strings.Contains(guide, passage.body) {
			t.Errorf("%s is no longer in %s, which cmd/legion's TestTheShippedWordsNameTheWaitingLineBlock "+
				"reads it for:\n%s\nquote the guide's passage as it reads now, in the same commit as the "+
				"edit that changed it", waitingLineBlockExtract, runningLegionGuide, passage.body)
		}
	}
}

// No shipped text of internal/workflow's band list claims a position in the waiting line. This
// is the half of that test no copy can carry: a ban is a whole-file negative, and an excerpt
// proves nothing about the sentence beside it - AGENTS.md once carried the placement word for
// word two lines below a sentence claiming a position, and the presence check passed on the
// neighbouring sentence's words. The rule those texts keep is that a text states the placement,
// "among the issues whose work came back, oldest first", with a tree root's own first phase
// placed at the moment its gate opened, and never the position that placement gives an issue,
// which depends on what else waits.
//
// What it proves: no file of the list carries one of the four wordings that claim a position.
// What it does not: whether what a file says instead is true, which stays a reader's to judge,
// and a wrong wording nobody has written yet, since the list holds the phrases these texts used
// and no more. internal/workflow keeps the same ban over the files of packages/daemon, which
// are its own job's inputs either way.
func TestNoShippedTextClaimsAPositionInTheWaitingLine(t *testing.T) {
	// Lower case, since the text is lower-cased before the comparison: a sentence that opens on
	// one of these is banned as the one that carries it mid-sentence is.
	banned := []string{
		"front of the waiting line",
		"front of the line",
		"front of the whole line",
		"never first in",
	}
	for _, file := range waitingLineBandTexts {
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
