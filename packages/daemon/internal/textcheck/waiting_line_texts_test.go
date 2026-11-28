package textcheck

//ci:reads packages/daemon/internal/api/waiting_line_contract_extract.txt readExtract reads it as ../api/waiting_line_contract_extract.txt
//ci:reads packages/daemon/internal/workflow/waiting_line_words_extract.txt readExtract reads it as ../workflow/waiting_line_words_extract.txt
//ci:reads packages/daemon/cmd/legion/awaiting_summary_extract.txt readExtract reads it as ../../cmd/legion/awaiting_summary_extract.txt
//ci:reads packages/daemon/cmd/legion/waiting_line_block_extract.txt readExtract reads it as ../../cmd/legion/waiting_line_block_extract.txt
//ci:reads docs/site/src/content/docs/legion/concepts.md shippedText reads it under ../../../..
//ci:reads docs/site/src/content/docs/legion/running-legion.md shippedText reads it under ../../../..
//ci:reads docs/site/src/content/docs/legion/troubleshooting.md shippedText reads it under ../../../..
//ci:reads packages/contracts/src/legion-api.ts shippedText reads it under ../../../..
//ci:reads packages/pi-legion/AGENTS.md shippedText reads it under ../../../..
//ci:reads packages/contracts/.changes/unreleased/pr2989-Added-20261108-074056.yaml shippedText reads that one fragment under ../../../..
//ci:reads packages/pi-legion/.changes/unreleased/pr2989-Changed-20261108-074108.yaml shippedText reads that one fragment under ../../../..
//ci:reads packages/claude-envoy/.changes/unreleased/pr2989-Changed-20261108-074111.yaml shippedText reads that one fragment under ../../../..

import (
	"errors"
	"fmt"
	"os"
	"path"
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
// Each Markdown text is held twice: as written, which an agent or an editor reads, and as the
// rendered page shows it (visibleMarkdown), which a person reads. A quotation, a sample line or a
// window only an HTML comment holds is then one no reader of the page sees, and fails rather than
// passing; a banned phrase fails in either reading, whether a comment holds it or splits it. A
// text outside Markdown has the one reading, since an HTML comment hides nothing there.
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
	//
	// The word and its number stand apart by spaces, or by the one line break a wrap puts between
	// them, with the indent and the one `//`, `*` or `#` comment marker the next line opens with:
	// onOneLine's rule for a wrapped line, below. The repository's semantic line breaks let a
	// sentence break there, in Markdown and YAML as in a Go or TypeScript comment, and a pattern
	// that read only a space passed every wrapped mention over without a word (INFRA-4004). A
	// blank line between the two is a paragraph break, and no mention.
	waitingLineNamed  = `(?:contract|below)(?:[ \t]+|[ \t]*\n[ \t]*(?:(?://|\*|#)[ \t]*)?)(\d+)`
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
	waitingLineBand = "within its priority, among the issues whose work came back, oldest first"
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
	runningLegionGuide,
	"docs/site/src/content/docs/legion/troubleshooting.md",
	"packages/contracts/src/legion-api.ts",
	"docs/site/src/content/docs/legion/concepts.md",
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
	if !slices.Equal(got, want) {
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
	named := regexp.MustCompile(waitingLineNamed)
	for _, name := range waitingLineContractTexts {
		if err := contractWindowsShownAsWritten(named, name, shippedText(t, name)); err != nil {
			t.Errorf("%s: %v\nthe copy %s holds its windows as written, which is where internal/api's "+
				"TestTheShippedTextsNameTheWaitingLinesOwnContract reads them, so a window the rendered page "+
				"does not show as written tells its reader nothing the check holds", name, err,
				waitingLineContractExtract)
		}
	}
}

// waitingLineContractWindows is every window the seven texts yield under the rule internal/api's
// test reads a text by: a `contract <N>` or a bare `below <N>`, on one line or wrapped onto the
// next, whose 220 bytes on either side name the field and name neither the retired stranded
// state nor the stranded report, which is the other contract these same texts discuss. The parse
// is that test's own, written again here rather than shared, because a copy held to its texts by
// the reader that reads the copy can drift with it and still agree - and because the two live in
// packages this file may not import.
func waitingLineContractWindows(t *testing.T) []quotedText {
	t.Helper()
	named := regexp.MustCompile(waitingLineNamed)
	var windows []quotedText
	for _, name := range waitingLineContractTexts {
		windows = append(windows, waitingLineContractWindowsIn(named, name, shippedText(t, name))...)
	}
	return windows
}

// contractWindowsShownAsWritten holds the windows text, the file called name, yields
// (waitingLineContractWindowsIn, named being waitingLineNamed compiled) as the rendered page shows
// it (visibleMarkdown) to the ones it yields as written, which are the copy's. A mention only an
// HTML comment holds yields a window as written and none on the page, and a comment within a
// window's bytes makes it one the page shows otherwise: either is refused for the comment
// (errCommented), naming the first window that differs. A text outside Markdown has the one
// reading.
func contractWindowsShownAsWritten(named *regexp.Regexp, name, text string) error {
	if path.Ext(name) != ".md" {
		return nil
	}
	written, shown := waitingLineContractWindowsIn(named, name, text), waitingLineContractWindowsIn(named, name, visibleMarkdown(text))
	for i := range max(len(written), len(shown)) {
		if i < len(written) && i < len(shown) && written[i] == shown[i] {
			continue
		}
		if i < len(written) {
			return fmt.Errorf("the window as written\n%s\nis not one the rendered page shows, its HTML comments "+
				"hidden: %w", written[i].body, errCommented)
		}
		return fmt.Errorf("the rendered page shows the window\n%s\nwhich is none as written: %w", shown[i].body,
			errCommented)
	}
	return nil
}

// Every quotation of the workflow package's copy is still in the file it names, in the one-line
// form the copy quotes it in, and every file of the band list still has a section. The product
// half reads this copy and not the files, so a sentence reworded in a text and not here leaves
// that half holding words nobody ships, and a section deleted here leaves it holding nothing at
// all for that file - both are failures of this test and of nothing else.
func TestTheWaitingLineWordsExtractIsWhatTheShippedTextsSay(t *testing.T) {
	sections := waitingLineWordsSections(t)
	for _, section := range sections {
		if err := passageHeld(section.path, shippedText(t, section.path), section.body, onOneLine); err != nil {
			t.Errorf("%s no longer carries the quotation %s is holding internal/workflow's check "+
				"to (%v):\n  %s\nquote the file as it reads now, on one line, or take out the comment the "+
				"failure names, in the same commit as the edit that moved it", section.path,
				waitingLineWordsExtract, err, section.body)
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
	if err := passageHeld(runningLegionGuide, shippedText(t, runningLegionGuide), sample, verbatim); err != nil {
		t.Errorf("%s is no longer in %s (%v), which cmd/legion's TestTheShippedWordsNameTheWaitingLineCount "+
			"reads it for:\n%s\nquote the guide's sample as it prints now, or take out the comment the "+
			"failure names, in the same commit as the edit that changed it", awaitingSummaryExtract,
			runningLegionGuide, err, sample)
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
		if err := passageHeld(runningLegionGuide, guide, passage.body, verbatim); err != nil {
			t.Errorf("%s is no longer in %s (%v), which cmd/legion's TestTheShippedWordsNameTheWaitingLineBlock "+
				"reads it for:\n%s\nquote the guide's passage as it reads now, or take out the comment the "+
				"failure names, in the same commit as the edit that changed it", waitingLineBlockExtract,
				runningLegionGuide, err, passage.body)
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
// What it proves: no file of the list carries one of the four wordings that claim a position
// (positionClaims), as written or, for a Markdown file, as its rendered page shows it.
// What it does not: whether what a file says instead is true, which stays a reader's to judge,
// and a wrong wording nobody has written yet, since the list holds the phrases these texts used
// and no more. internal/workflow keeps the same ban over the files of packages/daemon, which
// are its own job's inputs either way.
func TestNoShippedTextClaimsAPositionInTheWaitingLine(t *testing.T) {
	for _, file := range waitingLineBandTexts {
		if err := positionClaimsIn(file, shippedText(t, file)); err != nil {
			t.Errorf("%s: %v; no text the waiting line's checks list may say one: such a text states where "+
				"the line puts an issue - %q, a tree root's own first phase placed at the moment its gate "+
				"opened - and never the position that gives it, which depends on what else waits.", file, err,
				waitingLineBand)
		}
	}
}

// positionClaims are the wordings that claim a position in the waiting line, lower case, since a
// text is lower-cased before the comparison: a sentence that opens on one of these is banned as
// the one that carries it mid-sentence is.
var positionClaims = []string{
	"front of the waiting line",
	"front of the line",
	"front of the whole line",
	"never first in",
}

// errClaimsAPosition is why positionClaimsIn refuses a text: it says one of positionClaims.
var errClaimsAPosition = errors.New("a wording that claims a position in the waiting line")

// positionClaimsIn refuses text, the file called name, for each of positionClaims it says on one
// line (onOneLine), in both its readings for a Markdown file (bothReadings): as written, where an
// agent or an editor reads a phrase an HTML comment holds, and as the rendered page shows it
// (visibleMarkdown), where a person reads a phrase a comment splits as whole.
func positionClaimsIn(name, text string) error {
	says := func(reading string) error {
		lower := strings.ToLower(onOneLine(reading))
		var said []error
		for _, phrase := range positionClaims {
			if strings.Contains(lower, phrase) {
				said = append(said, fmt.Errorf("it says %q, %w", phrase, errClaimsAPosition))
			}
		}
		return errors.Join(said...)
	}
	written := says(text)
	if path.Ext(name) != ".md" {
		return written
	}
	return bothReadings(written, says(visibleMarkdown(text)))
}

// errPassageMissing is why passageHeld refuses a text: the passage does not stand in it.
var errPassageMissing = errors.New("the passage does not stand in it")

// passageHeld holds text, the file called name, to carry passage in form: its prose on one line
// (onOneLine), or the text as it is (verbatim). A Markdown file is held in both its readings
// (bothReadings), as written and as the rendered page shows it (visibleMarkdown), so that a
// passage only an HTML comment holds, fenced or not, fails for the comment; any other file is held
// as written, since an HTML comment hides nothing there.
func passageHeld(name, text, passage string, form func(string) string) error {
	carries := func(reading string) error {
		if strings.Contains(form(reading), passage) {
			return nil
		}
		return errPassageMissing
	}
	written := carries(text)
	if path.Ext(name) != ".md" {
		return written
	}
	return bothReadings(written, carries(visibleMarkdown(text)))
}

// verbatim is a text as it is: the form passageHeld holds a passage quoted byte for byte in.
func verbatim(text string) string { return text }

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

// waitingLineContractWindowsIn is the windows one text yields under the rule
// waitingLineContractWindows reads the seven texts by, named being waitingLineNamed compiled: the
// text is handed in rather than read, so that a case can hold the rule to a shape of text the
// shipped files do not carry today.
func waitingLineContractWindowsIn(named *regexp.Regexp, name, text string) []quotedText {
	var windows []quotedText
	for _, at := range named.FindAllStringSubmatchIndex(text, -1) {
		near := text[max(0, at[0]-waitingLineWindow):min(len(text), at[1]+waitingLineWindow)]
		if !strings.Contains(near, waitingLineField) ||
			strings.Contains(near, "retired") || strings.Contains(near, "stranded") {
			continue
		}
		windows = append(windows, quotedText{name, strings.TrimRight(near, "\n")})
	}
	return windows
}

// A number wrapped onto the line after its word yields its window as one on the same line does.
// cybermonklabs/legion#3659 wrapped packages/pi-legion/AGENTS.md's "after contract 85's" across
// two lines, and the scan, which read only a space between the two, found no window there; that
// run failed only because the copy had been taken from the text before the wrap, and a window
// first written wrapped would never have reached the copy at all (INFRA-4004). Each case is a
// shape the seven texts are written in, and each wrapped mention in it must yield its window; a
// blank line between the word and a number is a paragraph break, and yields none.
//
// What it proves: that the rule finds a wrapped number in Markdown, in a Go or TypeScript
// comment and in a YAML scalar. What it does not: that internal/api's scan reads the copy by the
// same rule, which its own case of a wrapped number holds.
func TestAContractNumberWrappedOntoTheNextLineYieldsItsWindow(t *testing.T) {
	named := regexp.MustCompile(waitingLineNamed)
	for _, wrapped := range []struct {
		shape, text string
		windows     int
	}{
		{"a Markdown sentence", "`handoff_complete` takes no `ready`), after contract\n85's `why` and the\n`waitingLine` fields beside it\n", 1},
		{"a Go line comment", "// A caller below\n// 58 is served no waitingLine.\n", 1},
		{"a TypeScript block comment", "  /** Where the issue stands in `admission.waiting` (contract\n   * 58): a daemon before contract\n   * 58 writes none. */\n  waitingLine: legionWaitingLineView.optional(),\n", 2},
		{"a YAML scalar", "body: 'Declares daemon API contract\n  58, which adds `waitingLine` to each issue the line names.'\n", 1},
		{"a paragraph break", "The `waitingLine` passage ends on the word contract\n\n58 opens the next paragraph.\n", 0},
	} {
		got := waitingLineContractWindowsIn(named, wrapped.shape, wrapped.text)
		if len(got) != wrapped.windows {
			t.Errorf("%s yields %d windows under %s, want %d: %q", wrapped.shape, len(got), waitingLineNamed,
				wrapped.windows, wrapped.text)
		}
	}
}

// Each read of a waiting-line text holds it where a reader sees it. A quotation or a fenced sample
// only an HTML comment holds, a contract mention only a comment holds, a comment within a window's
// bytes, and a comment that splits a quotation each leave the text as written and the rendered
// page disagreeing, and fail for the comment; a banned phrase fails both when a comment holds it,
// which an agent reading the file still reads, and when a comment splits it, which the page shows
// whole. A "<!--" inside a fenced block hides nothing, and outside Markdown, in TypeScript or a
// YAML fragment, a "<!--" is no comment at all. The quotation is prose, the sample a fenced block,
// the mention prose: each case is in the shape the shipped texts write it in.
func TestAWaitingLineTextIsHeldWhereAReaderSeesIt(t *testing.T) {
	named := regexp.MustCompile(waitingLineNamed)
	const sample = "  waiting line: 2\n    WIDGETS-14 1 of 2, returned: Its work came back."
	const mention = "A daemon older than contract 58 serves no `waitingLine`, so that count reads 0."
	filler := strings.Repeat("A sentence that names nothing. ", 10)
	quotation := func(name, text string) error { return passageHeld(name, text, waitingLineBand, onOneLine) }
	fencedSample := func(name, text string) error { return passageHeld(name, text, sample, verbatim) }
	windows := func(name, text string) error { return contractWindowsShownAsWritten(named, name, text) }
	for _, text := range []struct {
		shape, name, text string
		check             func(name, text string) error
		err               error
	}{
		{"a quotation a reader sees", "guide.md",
			"It stands within its priority,\namong the issues whose work came back,\noldest first, until it resumes.", quotation, nil},
		{"a quotation inside a comment", "guide.md",
			"It stands <!--\nwithin its priority, among the issues whose work came back,\noldest first, --> until it resumes.", quotation,
			errCommented},
		{"a comment inside a quotation", "guide.md",
			"It stands within its priority, among the issues <!-- a note --> whose work came back, oldest first.", quotation, errCommented},
		{"a quotation of TypeScript inside a \"<!--\"", "legion-api.ts",
			"/** It stands <!-- within its priority, among the issues whose work came back, oldest first --> */", quotation, nil},
		{"no quotation", "guide.md", "It stands among the issues, oldest first.", quotation, errPassageMissing},
		{"a fenced sample a reader sees", "guide.md",
			"- **What is it doing?**\n\n  ```text\n  legion WIDGETS\n" + sample + "\n  ```\n", fencedSample, nil},
		{"a comment around the fenced sample", "guide.md",
			"- **What is it doing?**\n\n  <!--\n  ```text\n  legion WIDGETS\n" + sample + "\n  ```\n  -->\n", fencedSample,
			errCommented},
		{"a fenced sample after a fenced block holding a comment's opening", "guide.md",
			"```html\n<!--\n```\n\n  ```text\n" + sample + "\n  ```\n", fencedSample, nil},
		{"a mention a reader sees", "guide.md", filler + mention + filler, windows, nil},
		{"a mention inside a comment beyond its window", "guide.md",
			"<!--\n" + filler + mention + filler + "\n-->\n", windows, errCommented},
		{"a comment within a mention's window", "guide.md", mention + " <!-- a note -->", windows, errCommented},
		{"a mention of YAML inside a \"<!--\"", "change.yaml", "body: '<!-- " + mention + " -->'\n", windows, nil},
		{"no claim of a position", "guide.md", "It stands among the issues whose work came back.", positionClaimsIn, nil},
		{"a claim a reader sees", "guide.md", "It goes to the front of the line.", positionClaimsIn, errClaimsAPosition},
		{"a claim inside a comment", "guide.md", "<!-- It goes to the front of the line. -->", positionClaimsIn,
			errCommented},
		{"a comment that splits a claim", "guide.md", "It goes to the front of <!-- the whole --> the line.",
			positionClaimsIn, errCommented},
		{"a claim of TypeScript split by a \"<!--\"", "legion-api.ts", "/** front of <!-- a --> the line */",
			positionClaimsIn, nil},
	} {
		if err := text.check(text.name, text.text); !errors.Is(err, text.err) {
			t.Errorf("%s: the check answers the error %v, want %v: %q", text.shape, err, text.err, text.text)
		}
	}
}
