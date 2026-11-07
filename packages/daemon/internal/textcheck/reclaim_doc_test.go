package textcheck

import (
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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
const (
	reclaimParagraphStart = "The remedy for a volume near full is manual"
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
	// because this package imports no product code — a check whose subject is text should not pull
	// the Kubernetes runtime into its build — and a root that moved cannot pass silently either
	// way: the paragraph and the extract are both written in it, so a value this no longer matches
	// makes the parse below name no path at all, which fails loudly.
	treeRoot = "/legion"
)

// The committed copy of the reclaim paragraph's two lists is the paragraph's own, path for path.
// An edit to either side alone fails here: a path added to the guide and not to the copy would
// otherwise never reach the renderer's own check, which reads the copy and not the guide.
func TestTheReclaimExtractIsTheGuidesReclaimParagraph(t *testing.T) {
	documentedDeleted, documentedExempt := reclaimParagraphLists(t)
	extractDeleted, extractExempt := reclaimExtractLists(t)
	for _, list := range []struct {
		name           string
		extract, guide map[string]bool
	}{
		{reclaimDeleteHeading, extractDeleted, documentedDeleted},
		{reclaimExemptHeading, extractExempt, documentedExempt},
	} {
		got, want := slices.Sorted(maps.Keys(list.extract)), slices.Sorted(maps.Keys(list.guide))
		if !slices.Equal(got, want) {
			t.Errorf("%s and docs/kubernetes.md's %q disagree:\n"+
				"  the extract: %s\n"+
				"  the guide:   %s\n"+
				"the extract is the copy internal/runtime/sandbox checks the renderer against, so the "+
				"guide answers for the runtime only while the two are equal; change them in one commit",
				reclaimExtract, list.name, strings.Join(got, ", "), strings.Join(want, ", "))
		}
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
	deleted, exempt := reclaimParagraphLists(t)
	for keep := range exempt {
		for p := keep; strings.HasPrefix(p, treeRoot); p = path.Dir(p) {
			if !deleted[p] {
				continue
			}
			t.Errorf("docs/kubernetes.md's reclaim paragraph deletes %s, which holds %s that the same "+
				"paragraph exempts: an operator removing the delete-list bullet takes the exemption with "+
				"it, so the bullet is a segment too coarse", p, keep)
		}
	}
}

// reclaimExtractLists reads the committed copy's two lists. A line ending in a colon opens a list,
// every other line is one path, and the blank lines and the `#` header are passed over; the file is
// written to be read that way. The parse is deliberately not the one below: a copy of a document
// held to it by the same reader that reads the document can drift with it and still agree.
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

// reclaimParagraphLists reads the guide's two markdown lists: the paths the delete list tells the
// operator to remove, and the paths the exemptions hold back. A delete-list path is a bullet's
// entire content and an exempt path is the one its bullet opens with, so the paths an exemption's
// reason goes on to mention — other halves, neighbouring caches — land in neither set and cannot
// stand in for a bullet that is missing.
func reclaimParagraphLists(t *testing.T) (deleted, exempt map[string]bool) {
	t.Helper()
	// The document is at the repository root, four directories above this package. Read it, and fail
	// when it cannot be read: a skip here would be an absence that reads exactly like a pass, and
	// this check exists because a silent absence is what the paragraph kept shipping.
	doc, err := os.ReadFile(filepath.FromSlash(kubernetesGuide))
	if err != nil {
		t.Fatalf("read docs/kubernetes.md: %v", err)
	}
	lines := strings.Split(string(doc), "\n")
	// The markers are prose and the document is hard-wrapped, so they straddle line ends: find them
	// in a flattened copy and carry the match back to the line it starts on. The lists themselves are
	// read from the lines, because a bullet is a shape of a line and flattening would erase it.
	flat, lineOf := flattenKeepingLines(lines)
	startOffset := strings.Index(flat, reclaimParagraphStart)
	if startOffset < 0 {
		t.Fatalf("docs/kubernetes.md has no %q: the reclaim paragraph was reworded, and this check "+
			"stops checking the moment it cannot find it", reclaimParagraphStart)
	}
	endOffset := strings.Index(flat[startOffset:], reclaimParagraphEnd)
	if endOffset < 0 {
		t.Fatalf("docs/kubernetes.md has no %q after %q: the reclaim paragraph's end moved, and an "+
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
		t.Fatalf("the reclaim paragraph between %q and %q names no path at all, so the markers are no "+
			"longer bounding the paragraph they were written for", reclaimParagraphStart, reclaimParagraphEnd)
	}
	// An empty list is this check failing open: the heading is still there, the parse matched nothing,
	// and the extract would then be held to a list that is not being read.
	if len(deleted) == 0 {
		t.Fatalf("the reclaim paragraph's %q list yields no path: its bullets are no longer one "+
			"backticked absolute path each, and nothing is being held to it", reclaimDeleteHeading)
	}
	if len(exempt) == 0 {
		t.Fatalf("the reclaim paragraph's %q list yields no path: its bullets no longer open with a "+
			"backticked absolute path, and nothing is being held to it", reclaimExemptHeading)
	}
	return deleted, exempt
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

// flattenKeepingLines collapses the document's whitespace so prose that was hard-wrapped reads as
// one line, and records for every byte of the result the line it came from, which is what turns a
// marker found in the flat text back into a bound on the lines the lists live in.
func flattenKeepingLines(lines []string) (string, []int) {
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
