package textcheck

//ci:reads docs/kubernetes.md reclaimGuide reads it under ../../../..
//ci:reads packages/daemon/internal/runtime/sandbox/reclaim_doc_extract.txt reclaimExtractLists reads the committed copy it holds the guide's paragraph to (reclaimExtractLists)

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testdoc"
)

// The reclaim paragraph of docs/kubernetes.md is an operator's whole remedy for an issue volume
// near full, and internal/runtime/sandbox holds every directory the renderer puts on that volume
// to its two lists. That comparison is in two halves, because a test that reads the guide puts the
// guide in its job's filter — a declared read applies to every compiled source file of a run — and
// a documentation-only edit would then start the whole daemon Go lane. The product half compares
// the renderer with internal/runtime/sandbox/reclaim_doc_extract.txt, a copy of the two lists
// committed beside it; this half holds that copy to the paragraph itself, and checks the one thing
// only the lists can say, that no delete-list path swallows a path the same paragraph exempts.
// Neither half alone is the guarantee: together they are the one the single test used to give.
//
// The two lists are read as lists: a delete-list path is a bullet's whole content, an exempt path
// is the one a bullet opens with, and a path named later inside a bullet's prose is on neither
// list. That is not fastidiousness. An earlier version of this check asked only whether the
// paragraph mentioned a path anywhere between its markers, and it passed with /legion/uv/cache
// struck from the delete list, because an exemption's reason happens to mention it — the check
// could not fail where it mattered, which is the defect it was written against.
//
// The markers bound the search to that paragraph. A rendered path mentioned anywhere else in a
// 4000-line document would otherwise satisfy the check while the remedy itself stays silent about
// it, which is the same miss wearing a pass.
//
// Both tests hold the paragraph as it is written and as it renders (visibleMarkdown,
// bothReadings): an operator who follows the rendered guide never sees a marker or a bullet inside
// an HTML comment, and one who reads the file sees every bullet a comment holds, so a comment in
// the paragraph's lists fails both tests rather than deciding them.
const (
	reclaimParagraphStart = "The remedy for a volume the trimmer cannot bring down is manual"
	reclaimParagraphEnd   = "As for the rest:"
	reclaimDeleteHeading  = "Delete list"
	reclaimExemptHeading  = "Exempt, each for a reason"
	// The two lines of the extract that open a list, as the file's own header describes them.
	reclaimDeleteList = "delete:"
	reclaimExemptList = "exempt:"
	// The committed copy, read from the package it is about, and the guide, four directories above
	// this one.
	reclaimExtract  = "../runtime/sandbox/reclaim_doc_extract.txt"
	kubernetesGuide = "../../../../docs/kubernetes.md"
	// treeRoot is the prefix every path of the two lists carries: where this layout mounts the
	// issue's volume, sandbox.TreeRoot in the runtime. It is written out rather than imported
	// because this file reaches no product package: importing internal/runtime/sandbox would put
	// the Kubernetes runtime, and every file that package's own tests read, back in this job's
	// filter, which is what splitting the check took out. A root that moved cannot pass silently
	// either way: the paragraph and the extract are both written in it, so a value this no longer
	// matches makes the parse below name no path at all, which fails loudly.
	treeRoot = "/legion"
)

// The committed copy of the reclaim paragraph's two lists is the paragraph's own, path for path.
// An edit to either side alone fails here: a path added to the guide and not to the copy would
// otherwise never reach the renderer's own check, which reads the copy and not the guide.
func TestTheReclaimExtractIsTheGuidesReclaimParagraph(t *testing.T) {
	extractDeleted, extractExempt := reclaimExtractLists(t)
	err := reclaimParagraphIsTheExtract(reclaimGuide(t), extractDeleted, extractExempt)
	if err != nil {
		t.Errorf("docs/kubernetes.md's reclaim paragraph: %v\nthe extract is the copy internal/runtime/sandbox "+
			"checks the renderer against, so the guide answers for the runtime only while the two are equal; "+
			"change them in one commit", err)
	}
}

// No path the delete list names is an ancestor of, or equal to, a path the exemptions hold back.
// This is a property of the two lists rather than of a rendered directory, so it is its own test:
// the renderer's check walks what is put on the volume and would never visit the pair that
// collides. An operator follows the remedy literally — `rm -rf` on each delete-list bullet — so a
// bullet written one segment too coarse removes the exemption underneath it while the same
// paragraph is still telling the reader to keep it, and the accounting check sees only that every
// rendered path is covered.
//
// The walk goes all the way up to treeRoot, unlike the renderer's namesPathOrAncestor, which stops
// short of it: treeRoot is no coverage for a rendered path, but a delete list that names it would
// still have the operator remove every exemption in the paragraph, and that is the harm this test
// is about. It reads the guide rather than the extract, since the paragraph is what an operator
// follows.
func TestTheReclaimParagraphNeverDeletesWhatItExempts(t *testing.T) {
	if err := reclaimParagraphKeepsWhatItExempts(reclaimGuide(t)); err != nil {
		t.Errorf("docs/kubernetes.md's reclaim paragraph: %v", err)
	}
}

// The reclaim paragraph is read where an operator who follows it sees it. The paragraph inside an
// HTML comment, a bullet inside one, a bullet the extract lacks inside one, and a comment opened on
// a line of its own and never closed before the paragraph each leave the rendered guide and the
// file disagreeing on the lists, and fail for the comment; so does a comment after a delete-list
// bullet, which makes it a bare path, one segment too coarse, only on the rendered page. A "<!--"
// inside a fenced block before the paragraph hides nothing, and nor does a "<!--" inside a line
// that its paragraph never closes, which the page shows as text. The paragraph has no fenced block
// of its own: its markers are prose and its lists are bullets. Each case is a paragraph as the
// guide writes one (reclaimParagraphText).
func TestTheReclaimParagraphIsReadWhereAnOperatorSeesIt(t *testing.T) {
	const deleteBullet, exemptBullet = "- `/legion/scratch/cache`", "- `/legion/scratch/state/<role>` — per-role, and small."
	extractDeleted, extractExempt := map[string]bool{"/legion/scratch/cache": true}, map[string]bool{"/legion/scratch/state/<role>": true}
	for _, guide := range []struct {
		shape, text   string
		isTheExtract  error
		keepsExempted error
	}{
		{"a paragraph a reader sees",
			reclaimParagraphText(deleteBullet, exemptBullet), nil, nil},
		{"the paragraph inside a comment",
			"<!--\n" + reclaimParagraphText(deleteBullet, exemptBullet) + "-->\n", errCommented, errCommented},
		{"a delete-list bullet inside a comment",
			reclaimParagraphText("<!--\n"+deleteBullet+"\n-->", exemptBullet), errCommented, errCommented},
		{"a bullet the extract lacks inside a comment",
			reclaimParagraphText(deleteBullet+"\n<!--\n- `/legion/scratch/other`\n-->", exemptBullet), errCommented, nil},
		{"a comment never closed before the paragraph",
			"An earlier note.\n<!-- a note\n\n" + reclaimParagraphText(deleteBullet, exemptBullet), errCommented, errCommented},
		{"a \"<!--\" never closed inside a line before the paragraph",
			"An earlier note. <!-- a note\n\n" + reclaimParagraphText(deleteBullet, exemptBullet), nil, nil},
		{"a comment that makes a delete-list bullet a bare path one segment too coarse",
			reclaimParagraphText(deleteBullet+"\n- `/legion/scratch` <!-- the whole of it -->", exemptBullet), errCommented,
			errCommented},
		{"a comment opened inside a fenced block before the paragraph",
			"```html\n<!--\n```\n\n" + reclaimParagraphText(deleteBullet, exemptBullet), nil, nil},
		{"a delete-list bullet one segment too coarse",
			reclaimParagraphText(deleteBullet+"\n- `/legion/scratch`", exemptBullet), errNotTheReclaimExtract,
			errReclaimDeletesAnExemption},
	} {
		err := reclaimParagraphIsTheExtract(guide.text, extractDeleted, extractExempt)
		if !errors.Is(err, guide.isTheExtract) {
			t.Errorf("%s: holding the lists to the extract answers the error %v, want %v: %q", guide.shape, err,
				guide.isTheExtract, guide.text)
		}
		if err := reclaimParagraphKeepsWhatItExempts(guide.text); !errors.Is(err, guide.keepsExempted) {
			t.Errorf("%s: holding the delete list off the exemptions answers the error %v, want %v: %q", guide.shape,
				err, guide.keepsExempted, guide.text)
		}
	}
}

// reclaimParagraphText is a reclaim paragraph as the guide writes one: the sentence opening it, the
// delete list's heading and deleted, its bullets, the exemptions' heading and exempt, its bullets,
// and the sentence its end marker opens.
func reclaimParagraphText(deleted, exempt string) string {
	return reclaimParagraphStart + ", because nothing prunes, and it accounts for every directory:\n\n" +
		reclaimDeleteHeading + " — remove these:\n\n" + deleted + "\n\n" +
		reclaimExemptHeading + " the list above does not hold:\n\n" + exempt + "\n" +
		reclaimParagraphEnd + " a role mid-build rebuilds what it loses.\n"
}

// errNotTheReclaimExtract is why reclaimListsAreTheExtracts refuses a text: its lists are not the
// extract's.
var errNotTheReclaimExtract = errors.New("its lists are not the extract's")

// reclaimParagraphIsTheExtract holds the two lists of guide's reclaim paragraph to the extract's in
// both readings of the guide (bothReadings): as written, and as it renders (visibleMarkdown).
func reclaimParagraphIsTheExtract(guide string, extractDeleted, extractExempt map[string]bool) error {
	return bothReadings(reclaimListsAreTheExtracts(guide, extractDeleted, extractExempt),
		reclaimListsAreTheExtracts(visibleMarkdown(guide), extractDeleted, extractExempt))
}

// reclaimListsAreTheExtracts holds the two lists of the reclaim paragraph of text, one reading of
// the guide (reclaimListsIn), to the extract's, path for path, and names each list the two
// disagree on.
func reclaimListsAreTheExtracts(text string, extractDeleted, extractExempt map[string]bool) error {
	documentedDeleted, documentedExempt, err := reclaimListsIn(text)
	if err != nil {
		return err
	}
	var disagree []error
	for _, list := range []struct {
		name           string
		extract, guide map[string]bool
	}{
		{reclaimDeleteHeading, extractDeleted, documentedDeleted},
		{reclaimExemptHeading, extractExempt, documentedExempt},
	} {
		got, want := slices.Sorted(maps.Keys(list.extract)), slices.Sorted(maps.Keys(list.guide))
		if !slices.Equal(got, want) {
			disagree = append(disagree, fmt.Errorf("%s and its %q disagree, %w:\n"+
				"  the extract: %s\n"+
				"  the guide:   %s", reclaimExtract, list.name, errNotTheReclaimExtract,
				strings.Join(got, ", "), strings.Join(want, ", ")))
		}
	}
	return errors.Join(disagree...)
}

// errReclaimDeletesAnExemption is why reclaimListsKeepTheExemptions refuses a text: a delete-list
// path holds a path the same paragraph exempts.
var errReclaimDeletesAnExemption = errors.New("a delete-list bullet is a segment too coarse")

// reclaimParagraphKeepsWhatItExempts holds the delete list of guide's reclaim paragraph off its
// exemptions in both readings of the guide (bothReadings): as written, and as it renders
// (visibleMarkdown).
func reclaimParagraphKeepsWhatItExempts(guide string) error {
	return bothReadings(reclaimListsKeepTheExemptions(guide), reclaimListsKeepTheExemptions(visibleMarkdown(guide)))
}

// reclaimListsKeepTheExemptions holds the delete list of the reclaim paragraph of text, one reading
// of the guide (reclaimListsIn), off its exemptions: no delete-list path, up to treeRoot, is a path
// an exemption holds back or one of its ancestors. The exemptions are walked in order, so that a
// reading's failure is the same text every time it is the same failure.
func reclaimListsKeepTheExemptions(text string) error {
	deleted, exempt, err := reclaimListsIn(text)
	if err != nil {
		return err
	}
	var swallowed []error
	for _, keep := range slices.Sorted(maps.Keys(exempt)) {
		for p := keep; strings.HasPrefix(p, treeRoot); p = path.Dir(p) {
			if !deleted[p] {
				continue
			}
			swallowed = append(swallowed, fmt.Errorf("it deletes %s, which holds %s that the same paragraph "+
				"exempts: an operator removing the delete-list bullet takes the exemption with it, so %w",
				p, keep, errReclaimDeletesAnExemption))
		}
	}
	return errors.Join(swallowed...)
}

// reclaimExtractLists reads the committed copy's two lists. A line ending in a colon opens a list,
// every other line is one path, and the blank lines and the `#` header are passed over; the file is
// written to be read that way. The parse is deliberately not reclaimParagraphLists below: a copy of
// a document held to it by the same reader that reads the document can drift with it and still
// agree.
//
// internal/runtime/sandbox keeps a parse of the same shape over the same file, and the copy
// stands rather than being shared: this file may not import that package (see treeRoot above),
// and a package both could import would be a new package of this module carrying nine lines.
//
// The two do not read the extract identically. That one refuses a line outside TreeRoot, because
// a path outside the tree answers for no rendered directory there; this one takes the line, and
// the comparison with the guide is what refuses it, naming both lists rather than the line alone,
// since the guide's own bullets carry no path outside treeRoot (bulletPath).
func reclaimExtractLists(t *testing.T) (deleted, exempt map[string]bool) {
	t.Helper()
	text, err := os.ReadFile(filepath.FromSlash(reclaimExtract))
	if err != nil {
		t.Fatalf("read %s: %v", reclaimExtract, err)
	}
	deleted, exempt = map[string]bool{}, map[string]bool{}
	var into map[string]bool
	for i, line := range strings.Split(string(text), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		switch line {
		case reclaimDeleteList:
			into = deleted
			continue
		case reclaimExemptList:
			into = exempt
			continue
		}
		if into == nil {
			t.Fatalf("%s:%d: %q stands under neither %q nor %q, so which of the two lists it is on "+
				"cannot be read", reclaimExtract, i+1, line, reclaimDeleteList, reclaimExemptList)
		}
		into[line] = true
	}
	// An empty list is this check failing open: the file is still there, the parse matched nothing,
	// and the guide's list would then be held to a list nobody is reading.
	if len(deleted) == 0 || len(exempt) == 0 {
		t.Fatalf("%s names no path under %q or none under %q: its copy of the paragraph's lists is "+
			"gone, and nothing is being held to it", reclaimExtract, reclaimDeleteList, reclaimExemptList)
	}
	return deleted, exempt
}

// reclaimGuide is docs/kubernetes.md, four directories above this package. A guide that cannot be
// read stops the test: a skip here would be an absence that reads exactly like a pass, and this
// check exists because a silent absence is what the paragraph kept shipping.
func reclaimGuide(t *testing.T) string {
	t.Helper()
	doc, err := os.ReadFile(filepath.FromSlash(kubernetesGuide))
	if err != nil {
		t.Fatalf("read docs/kubernetes.md: %v", err)
	}
	return string(doc)
}

// reclaimListsIn reads the two markdown lists of text's reclaim paragraph: the paths the delete
// list tells the operator to remove, and the paths the exemptions hold back. A delete-list path is
// a bullet's entire content and an exempt path is the one its bullet opens with, so the paths an
// exemption's reason goes on to mention — other halves, neighbouring caches — land in neither set
// and cannot stand in for a bullet that is missing.
func reclaimListsIn(text string) (deleted, exempt map[string]bool, err error) {
	lines := strings.Split(text, "\n")
	// The markers are prose and the document is hard-wrapped, so they straddle line ends: find them
	// in a flattened copy and carry the match back to the line it starts on. The lists themselves are
	// read from the lines, because a bullet is a shape of a line and flattening would erase it.
	flat, lineOf := testdoc.FlattenKeepingLines(lines)
	startOffset := strings.Index(flat, reclaimParagraphStart)
	if startOffset < 0 {
		return nil, nil, fmt.Errorf("it has no %q: the reclaim paragraph was reworded, and this check "+
			"stops checking the moment it cannot find it", reclaimParagraphStart)
	}
	endOffset := strings.Index(flat[startOffset:], reclaimParagraphEnd)
	if endOffset < 0 {
		return nil, nil, fmt.Errorf("it has no %q after %q: the reclaim paragraph's end moved, and an "+
			"unbounded search would pass on a mention anywhere in the document",
			reclaimParagraphEnd, reclaimParagraphStart)
	}
	region := lines[lineOf[startOffset] : lineOf[startOffset+endOffset]+1]

	deleted, exempt = map[string]bool{}, map[string]bool{}
	var into map[string]bool
	var wholeBullet bool
	// The loop advances i itself over a wrapped bullet's continuation lines, so it stays explicit.
	for i := 0; i < len(region); i++ {
		line := strings.TrimRight(region[i], " ")
		switch {
		case strings.HasPrefix(line, reclaimDeleteHeading):
			into, wholeBullet = deleted, true
			continue
		case strings.HasPrefix(line, reclaimExemptHeading):
			into, wholeBullet = exempt, false
			continue
		}
		if into == nil || !strings.HasPrefix(line, "- ") {
			continue
		}
		// A wrapped bullet continues on the indented lines under it, and they are part of its content:
		// a delete-list bullet that carries prose is not a bare path, whatever its first line looks
		// like.
		bullet := strings.TrimSpace(strings.TrimPrefix(line, "- "))
		for i+1 < len(region) && strings.HasPrefix(region[i+1], " ") && strings.TrimSpace(region[i+1]) != "" {
			i++
			bullet += " " + strings.TrimSpace(region[i])
		}
		if named, ok := bulletPath(bullet, wholeBullet); ok {
			into[named] = true
		}
	}

	if len(deleted)+len(exempt) == 0 {
		return nil, nil, fmt.Errorf("the reclaim paragraph between %q and %q names no path at all, so the "+
			"markers are no longer bounding the paragraph they were written for", reclaimParagraphStart,
			reclaimParagraphEnd)
	}
	// An empty list is this check failing open: the heading is still there, the parse matched nothing,
	// and the extract would then be held to a list that is not being read.
	if len(deleted) == 0 {
		return nil, nil, fmt.Errorf("the reclaim paragraph's %q list yields no path: its bullets are no "+
			"longer one backticked absolute path each, and nothing is being held to it", reclaimDeleteHeading)
	}
	if len(exempt) == 0 {
		return nil, nil, fmt.Errorf("the reclaim paragraph's %q list yields no path: its bullets no longer "+
			"open with a backticked absolute path, and nothing is being held to it", reclaimExemptHeading)
	}
	return deleted, exempt, nil
}

// bulletPath is the path a bullet puts on its list. A delete-list bullet is the path and nothing
// else, since the operator removes what the bullet says; an exemption opens with the path it holds
// back and then argues for it, so only the opening code span counts. treeRoot itself is a path a
// bullet can name: it covers no rendered directory, but a delete list naming it swallows every
// exemption, and that is a reading the lists must be held to.
func bulletPath(bullet string, whole bool) (string, bool) {
	if !strings.HasPrefix(bullet, "`") {
		return "", false
	}
	named, rest, closed := strings.Cut(bullet[1:], "`")
	if !closed || !strings.HasPrefix(named, treeRoot+"/") && named != treeRoot {
		return "", false
	}
	if whole && rest != "" {
		return "", false
	}
	return named, true
}
