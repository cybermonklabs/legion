package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// wholeBlockTail is the sentence the typed block of each test below ends on, and newerBlockTail
// the one an edit puts in its place. Each stands past the 480 runes a block's stored excerpt
// stops at (refs.BlockExcerptRunes), so only a tree read from the block's whole content holds
// it.
const (
	wholeBlockTail = "Last of all, rehearse the rollback."
	newerBlockTail = "Last of all, rehearse the restore."
)

// wholeBlockContent is what the typed block of each test below holds between its opening line
// and its closing one: a paragraph with a link and a dispatch:// reference, and a second
// paragraph long enough to carry tail past the block's stored excerpt. blockWords drops the
// link's text and the reference from the first and joins the second onto it, which is the
// `text` each read has always answered; the tree is the two paragraphs as they stand.
func wholeBlockContent(issueKey, tail string) string {
	return "Read [the deploy guide](https://example.com/guide) and dispatch://" + issueKey + "/spec#b-r2 first.\n\n" +
		strings.Repeat("Then **ship** it. ", 30) + tail
}

// wordsOfWholeBlock is blockWords of wholeBlockContent ending on tail: the link's text without
// its target (R10 of dispatch://INFRA-4071/spec), and no reference.
func wordsOfWholeBlock(tail string) string {
	return "Read the deploy guide and first. " + strings.Repeat("Then ship it. ", 30) + tail
}

// requireWholeBlockTree fails unless tree is the display tree of wholeBlockContent ending on
// tail - the tree a reader of that markdown standing alone is shown - and holds the link's text,
// the link's address and the dispatch:// reference, which are what the read's `text` leaves out,
// and tail, which the block's stored excerpt leaves out. No word of a typed block's opening line
// is in it: the tree is of what the block says, not of its directive.
func requireWholeBlockTree(t *testing.T, what string, tree json.RawMessage, issueKey, tail string) {
	t.Helper()
	content := wholeBlockContent(issueKey, tail)
	if before := len([]rune(strings.TrimSuffix(content, tail))); before <= refs.BlockExcerptRunes {
		t.Fatalf("the content before its tail is %d runes, too few to carry the tail past the stored excerpt", before)
	}
	want := sameShape(t, pmdoc.DisplayJSON(content, pmdoc.DisplayLatest))
	if got := sameShape(t, tree); !bytes.Equal(got, want) {
		t.Errorf("%s is\n\t%s\nwant the tree of its whole content %q\n\t%s", what, got, content, want)
	}
	for _, kept := range []string{"the deploy guide", "https://example.com/guide", "dispatch://" + issueKey + "/spec#b-r2", "ship", tail} {
		if !strings.Contains(string(tree), kept) {
			t.Errorf("%s holds no %q:\n\t%s", what, kept, tree)
		}
	}
	for _, opening := range []string{"requirement{", "check{", "plan_task{", "serves", "surface"} {
		if strings.Contains(string(tree), opening) {
			t.Errorf("%s holds %q, a word of a typed block's opening line:\n\t%s", what, opening, tree)
		}
	}
}

// treeAt is the tree standing at path of a read made with `Dispatch-Trees: 1`, after the same
// read without the header answered none there.
func treeAt(t *testing.T, handler http.Handler, target, path string) json.RawMessage {
	t.Helper()
	if _, held := valueAt(t, treesRequest(t, handler, target, false), path); held {
		t.Errorf("GET %s without the header carries %s", target, path)
	}
	tree, held := valueAt(t, treesRequest(t, handler, target, true), path)
	if !held {
		t.Fatalf("GET %s with the header carries no %s", target, path)
	}
	return tree
}

// storedBlockText is the latest text document_block_texts records for block id of a document
// (refs.CurrentBlockText): the block's slice of the document's rendering, which is the text GET
// /api/v1/reference-targets reads the block's tree from (refs.BlockTexts).
func storedBlockText(t *testing.T, database *store.Store, artifactID, id string) string {
	t.Helper()
	stored, err := refs.CurrentBlockText(context.Background(), database.Pool, artifactID, id)
	if err != nil {
		t.Fatalf("read the stored text of %s of document %s: %v", id, artifactID, err)
	}
	if stored == "" {
		t.Fatalf("the block index records no text for %s of document %s: it is deleted or was never recorded", id, artifactID)
	}
	return stored
}

// referenceTargetOf is the one member of a batched reference read of reference made with
// `Dispatch-Trees: 1`.
func referenceTargetOf(t *testing.T, handler http.Handler, reference string) model.ReferenceTarget {
	t.Helper()
	members := decodeBody[model.ReferenceTargets](t, treesRequest(t, handler, referenceTargetsOf(reference), true)).Targets
	if len(members) != 1 {
		t.Fatalf("the read of %s answered %d members, want 1", reference, len(members))
	}
	return members[0]
}

// The coverage read answers each requirement's whole content as `text_tree`, for a caller that
// asked for trees: the link and the dispatch:// reference `text` drops are both in it, and so
// is the tail past the stored excerpt, and `text` is what it was. The tree is the very bytes the
// batched reference read answers for the same block, since both read it from the block's stored
// text by one rule: a requirement nested in a quote, whose stored text does not read back as one
// block alone, answers the tree of that whole text, opening line included, in both. A requirement
// in a list nested two levels deep and one in a footnote definition answer the tree of their
// content, as a top-level one does, in both reads, with the same bytes.
func TestTheCoverageReadCarriesARequirementsContentWhole(t *testing.T) {
	handler, database := blockIndexHandler(t)
	const deep, footnoted = "Deep in a nested list.", "In a footnote."
	issue := createInteractionIssue(t, handler, "TBT", "Whole requirements",
		":::requirement{#r1}\n"+wholeBlockContent("TBT-1", wholeBlockTail)+"\n:::\n\n"+
			":::requirement{#r2}\nAn answered ask leaves the list.\n:::\n\n"+
			"> :::requirement{#r3}\n> Nested in a quote.\n> :::\n\n"+
			"- a\n  - b\n\n    :::requirement{#r4}\n    "+deep+"\n    :::\n\n"+
			"See the note.[^1]\n\n[^1]: A note.\n\n    :::requirement{#r5}\n    "+footnoted+"\n    :::\n")
	if issue.Key != "TBT-1" {
		t.Fatalf("the issue is %s, want TBT-1, which the requirement's reference names", issue.Key)
	}
	target := "/api/v1/issues/" + issue.Key + "/coverage"

	requireWholeBlockTree(t, "r1's text_tree", treeAt(t, handler, target, "requirements.0.text_tree"),
		issue.Key, wholeBlockTail)
	const plain = "An answered ask leaves the list."
	if got, want := sameShape(t, treeAt(t, handler, target, "requirements.1.text_tree")),
		sameShape(t, pmdoc.DisplayJSON(plain, pmdoc.DisplayLatest)); !bytes.Equal(got, want) {
		t.Errorf("r2's text_tree is\n\t%s\nwant the tree of %q\n\t%s", got, plain, want)
	}
	quoted := storedBlockText(t, database, issue.PrimaryArtifactID, "r3")
	if !strings.HasPrefix(quoted, ":::requirement{#r3}") {
		t.Fatalf("r3's stored text is %q, want its slice from its opening line", quoted)
	}
	if got, want := sameShape(t, treeAt(t, handler, target, "requirements.2.text_tree")),
		sameShape(t, pmdoc.DisplayJSON(quoted, pmdoc.DisplayLatest)); !bytes.Equal(got, want) {
		t.Errorf("r3's text_tree is\n\t%s\nwant the tree of its whole stored text %q\n\t%s", got, quoted, want)
	}
	for index, content := range map[int]string{3: deep, 4: footnoted} {
		path := "requirements." + strconv.Itoa(index) + ".text_tree"
		if got, want := sameShape(t, treeAt(t, handler, target, path)),
			sameShape(t, pmdoc.DisplayJSON(content, pmdoc.DisplayLatest)); !bytes.Equal(got, want) {
			t.Errorf("%s is\n\t%s\nwant the tree of its content %q\n\t%s", path, got, content, want)
		}
	}

	coverage := decodeBody[model.IssueCoverage](t, treesRequest(t, handler, target, true))
	if got := requirementOf(t, coverage, "r1").Text; got != wordsOfWholeBlock(wholeBlockTail) {
		t.Errorf("r1's text = %q, want its words, the link's text kept and its target dropped, %q", got, wordsOfWholeBlock(wholeBlockTail))
	}
	for _, id := range []string{"r1", "r3", "r4", "r5"} {
		member := referenceTargetOf(t, handler, "dispatch://"+issue.Key+"/spec#b-"+id)
		if got := requirementOf(t, coverage, id).TextTree; member.TextTree == nil || !bytes.Equal(got, member.TextTree) {
			t.Errorf("%s's text_tree is\n\t%s\nin the coverage read and\n\t%s\nin the batched reference read, want the same bytes",
				id, got, member.TextTree)
		}
	}
}

// The results read answers each check's whole content as `text_tree`, as the coverage read does
// a requirement's.
func TestTheResultsReadCarriesAChecksContentWhole(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TBT", "Whole checks", twoRequirements)
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md",
		":::check{#k1 surface=\"cli\" run=\"go test ./...\"}\n"+wholeBlockContent(issue.Key, wholeBlockTail)+"\n:::\n")
	target := "/api/v1/issues/" + issue.Key + "/results"

	requireWholeBlockTree(t, "k1's text_tree", treeAt(t, handler, target, "checks.0.text_tree"),
		issue.Key, wholeBlockTail)

	read := decodeBody[issueResults](t, treesRequest(t, handler, target, true))
	if got := checkOf(t, read, "k1").Text; got != wordsOfWholeBlock(wholeBlockTail) {
		t.Errorf("k1's text = %q, want its words, the link's text kept and its target dropped, %q", got, wordsOfWholeBlock(wholeBlockTail))
	}
}

// referenceTargetsOf is the address of one batched reference read of references, in order.
func referenceTargetsOf(references ...string) string {
	query := url.Values{}
	for _, reference := range references {
		query.Add("ref", reference)
	}
	return "/api/v1/reference-targets?" + query.Encode()
}

// The batched reference read answers each block's whole text as `text_tree`, for a caller that
// asked for trees, which is where a plan task's words are read whole: its name is the block's
// stored excerpt, which holds the directive line and stops at 480 runes. A typed block's tree is
// of its own content, as the coverage and the results reads answer one, and a reference pinned
// to an older version of its document answers the newest version's tree, as it answers the
// newest name. A typed block nested in a quote, whose stored text does not read back as one
// block alone, answers the tree of that whole text, while one in a list item answers its
// content, and so do one in a list nested two levels deep, one in a footnote definition and one
// after the marker `10. `, whose lines stand four columns in: its content reads as paragraphs,
// not as an indented code block. A block of no declared type, of a comment, of a message and of
// an ask answers the tree of its own whole text, and a comment's typed block that holds another,
// neither written with an id, answers its content as the comment wrote it: no read gives the
// inner block an id, so two reads answer one tree under one ETag, and that tag sent back is
// answered 304. A comment's typed block whose content opens with a rule `---` and holds a second
// one answers both rules and both paragraphs: as the opening of a document of its own, that `---`
// would start front matter that swallows the content up to the second rule. A comment's typed
// block whose closing fence stands two spaces in, or three, while its line of content stands at
// column 0, answers that line: the block's lines start at column 0, where such a fence closes it.
// A comment's typed block whose fence and lines stand two spaces in, with a line four spaces in
// between them, answers that line as the comment's own parse reads it: a code block, not a heading.
// A comment's and a message's typed block whose opening line stands two spaces in, after a
// paragraph, answers its content with those spaces taken off it and off its lines, as the item's
// own parse reads them, and no word of its opening line.
// A member that names no block carries no tree, and neither does any member of a read that asked
// for none. The names are the same either way.
//
// The texts cost one statement however many blocks of a document the set names: a set of 20
// blocks of one document makes as many statements as a set of 2, and one of them reads the
// blocks' texts out of document_block_texts. The same blocks named by ids written short cost one
// statement more, which resolves every prefix among the document's blocks, so a set of 20 of them
// again makes as many statements as a set of 2.
func TestTheReferenceTargetsReadCarriesATypedBlocksContentWhole(t *testing.T) {
	database := storetest.Open(t)
	counter := &roundTrips{}
	traced := &store.Store{Pool: storetest.Traced(t, database.Pool, counter, nil)}
	handler, _, _, served := newRegisteredTestServer(t, testServerOptions{
		store: traced,
		// The caller's identity is resolved on the untraced pool, so a count is the read's own
		// statements and not the sign-in lookup every request makes.
		identity: headerIdentity(database),
		// As blockIndexHandler's: no document settles during the test, so every version a read
		// answers is one its own requests wrote, and no settlement runs on the traced pool.
		settle: time.Hour,
	})
	createReferenceAPIProject(t, handler, "TBT")
	issue := createSpecIssue(t, handler, "TBT", twoRequirements)
	plan := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "plan.md",
		"A paragraph of the plan.\n\n"+
			":::plan_task{#t1 serves=\"dispatch://"+issue.Key+"/spec#b-r1\"}\n"+wholeBlockContent(issue.Key, wholeBlockTail)+"\n:::\n")
	planRef := "dispatch://" + issue.Key + "/artifact/" + plan.Artifact.Slug
	paragraphID := blockIDsByText(t, handler, plan.Artifact.ID)["A paragraph of the plan."]
	if paragraphID == "" {
		t.Fatalf("the plan lists no block for its paragraph")
	}
	notes := make([]string, 20)
	for index := range notes {
		notes[index] = "Note " + strconv.Itoa(index+1) + " of the twenty."
	}
	const listed = "Nested in a list."
	const deep, footnoted = "Deep in a nested list.", "In a footnote."
	// After the marker `10. ` an item's content stands four columns in, so each line of the
	// requirement's stored text but its first carries four spaces.
	const numbered = "Nested in item 10.\n\nIts second paragraph."
	notesDocument := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "notes.md",
		"> :::requirement{#q1}\n> Nested in a quote.\n> :::\n\n"+
			"- An item:\n\n  :::requirement{#l1}\n  "+listed+"\n  :::\n\n"+
			"10. An item:\n\n    :::requirement{#l10}\n    "+strings.ReplaceAll(numbered, "\n\n", "\n\n    ")+"\n    :::\n\n"+
			"- a\n  - b\n\n    :::requirement{#r2}\n    "+deep+"\n    :::\n\n"+
			"See the note.[^1]\n\n[^1]: A note.\n\n    :::requirement{#f1}\n    "+footnoted+"\n    :::\n\n"+
			strings.Join(notes, "\n\n")+"\n")
	notesRef := "dispatch://" + issue.Key + "/artifact/" + notesDocument.Artifact.Slug
	quoted := storedBlockText(t, database, notesDocument.Artifact.ID, "q1")
	if !strings.HasPrefix(quoted, ":::requirement{#q1}") {
		t.Fatalf("the quoted requirement's stored text is %q, want its slice from its opening line", quoted)
	}
	if stored := storedBlockText(t, database, notesDocument.Artifact.ID, "l1"); !strings.Contains(stored, "requirement{") {
		t.Fatalf("the listed requirement's stored text is %q, want its opening line in it", stored)
	}
	if stored := storedBlockText(t, database, notesDocument.Artifact.ID, "l10"); !strings.Contains(stored, "\n    Nested in item 10.") {
		t.Fatalf("the requirement in item 10 has the stored text %q, want its lines four spaces in", stored)
	}
	for id, content := range map[string]string{"r2": deep, "f1": footnoted} {
		if stored := storedBlockText(t, database, notesDocument.Artifact.ID, id); !strings.Contains(stored, "requirement{") ||
			!strings.Contains(stored, "\n    "+content) {
			t.Fatalf("the requirement %s has the stored text %q, want its opening line and its lines four spaces in", id, stored)
		}
	}
	const commentBlock = "Its second block, with [a link](https://example.com/second)."
	comment := createReferenceAPIComment(t, handler, issue.Key, "A comment's first block.\n\n"+commentBlock)
	// A comment's typed block that holds another, neither written with an id: each parse gives
	// the inner block a new random id, which a rendering of the content would write into its
	// opening line.
	const nestedCallout = "::::callout{}\n:::callout{}\nInner words.\n:::\n::::"
	nested := createReferenceAPIComment(t, handler, issue.Key, nestedCallout)
	// A typed block whose content opens with a rule and holds a second one: cut out of the block,
	// the first `---` would open front matter that runs to the second.
	const ruledCallout = ":::callout{}\n---\n\nAfter a rule.\n\n---\n\nTail.\n:::"
	ruled := createReferenceAPIComment(t, handler, issue.Key, ruledCallout)
	// A typed block whose closing fence stands two spaces in and whose content does not: its
	// lines start at column 0, where a fence fewer than four columns in closes the block.
	const indentedFence = ":::callout{}\nText of the callout.\n  :::"
	fenced := createReferenceAPIComment(t, handler, issue.Key, indentedFence)
	// Three spaces is the most a fence can stand in and still close a block whose lines start
	// at column 0, so the cut takes the lines as written there too.
	const threeSpaceFence = ":::callout{}\nText of the callout, its fence three spaces in.\n   :::"
	fencedThree := createReferenceAPIComment(t, handler, issue.Key, threeSpaceFence)
	// A typed block whose fence and lines stand two spaces in, with a line four spaces in: the
	// comment's own parse reads that line as an indented code block, so its content is its lines
	// as written, and with the fence's two spaces taken out the line would read as a heading.
	const notAHeading = "  Intro\n\n    # Not a heading"
	codeLine := createReferenceAPIComment(t, handler, issue.Key, ":::callout{}\n"+notAHeading+"\n  :::")
	const messageBlock = "A message's second block, with dispatch://TBT-1 in it."
	message := createIssueMessage(t, handler, issue.Key, map[string]any{"body": "A message's first block.\n\n" + messageBlock}, "alice")
	// A comment's and a message's second block, a typed block whose opening line stands two spaces
	// in: the block is stored from the start of its line, those spaces included, and the item's own
	// parse reads it from past them.
	const indentedOpener = "Indented opener."
	indentedBody := "Intro.\n\n  :::callout{}\n  " + indentedOpener + "\n  :::"
	indentedComment := createReferenceAPIComment(t, handler, issue.Key, indentedBody)
	indentedMessage := createIssueMessage(t, handler, issue.Key, map[string]any{"body": indentedBody}, "alice")
	ask := readPartAsk(t, handler, createPartAsk(t, handler, issue.Key, "Which day do we ship?",
		map[string]string{"label": "Monday", "description": "Ship at the start of the week."},
		map[string]string{"label": "Tuesday"}).ID)
	if len(ask.Options) == 0 || ask.Options[0].ID == "" {
		t.Fatalf("the ask answers options %#v, want a part id on its first", ask.Options)
	}
	part := askPartText(t, database, ask.ID, ask.Options[0].ID, ask.Revision)
	if part == nil || !strings.Contains(*part, "Ship at the start of the week.") {
		t.Fatalf("the ask's first option part holds %v, want its label and description", part)
	}

	references := []string{
		planRef + "#b-t1",
		planRef + "#b-" + text.EncodeBlockID(paragraphID),
		"dispatch://" + issue.Key + "/comment/" + comment.ID + "#b-b2",
		"dispatch://" + issue.Key + "/message/" + message.ID + "#b-b2",
		"dispatch://" + issue.Key,
		askPartRef(issue.Key, ask.ID, ask.Options[0].ID),
		notesRef + "#b-q1",
		notesRef + "#b-l1",
		notesRef + "#b-l10",
		"dispatch://" + issue.Key + "/comment/" + nested.ID + "#b-b1",
		"dispatch://" + issue.Key + "/comment/" + ruled.ID + "#b-b1",
		notesRef + "#b-r2",
		notesRef + "#b-f1",
		"dispatch://" + issue.Key + "/comment/" + fenced.ID + "#b-b1",
		"dispatch://" + issue.Key + "/comment/" + fencedThree.ID + "#b-b1",
		"dispatch://" + issue.Key + "/comment/" + codeLine.ID + "#b-b1",
		"dispatch://" + issue.Key + "/comment/" + indentedComment.ID + "#b-b2",
		"dispatch://" + issue.Key + "/message/" + indentedMessage.ID + "#b-b2",
	}
	target := referenceTargetsOf(references...)
	plain := treesRequest(t, handler, target, false)
	bare := decodeBody[model.ReferenceTargets](t, plain).Targets
	read := decodeBody[model.ReferenceTargets](t, treesRequest(t, handler, target, true)).Targets
	if len(bare) != len(references) || len(read) != len(references) {
		t.Fatalf("the reads answered %d and %d members, want %d each", len(bare), len(read), len(references))
	}
	for _, member := range decodeBody[struct {
		Targets []map[string]json.RawMessage `json:"targets"`
	}](t, plain).Targets {
		if tree, held := member["text_tree"]; held {
			t.Errorf("without the header %s carries the key text_tree: %s", member["ref"], tree)
		}
	}
	for index, member := range bare {
		if member.Gone || member.Name != read[index].Name || member.Kind != read[index].Kind {
			t.Errorf("%s answers %#v without the header and %#v with it, want one name and one kind, and neither gone",
				member.Ref, member, read[index])
		}
	}

	task := read[0]
	if !strings.Contains(task.Name, "plan_task") {
		t.Errorf("the plan task's name = %q, want the stored excerpt, which opens with its directive line", task.Name)
	}
	if runes := len([]rune(task.Name)); runes > refs.BlockExcerptRunes || strings.Contains(task.Name, wholeBlockTail) {
		t.Errorf("the plan task's name is %d runes and reads %q, want at most %d without the tail",
			runes, task.Name, refs.BlockExcerptRunes)
	}
	requireWholeBlockTree(t, "the plan task's text_tree", json.RawMessage(task.TextTree), issue.Key, wholeBlockTail)
	for index, content := range map[int]string{
		1: "A paragraph of the plan.", 2: commentBlock, 3: messageBlock, 5: *part, 6: quoted, 7: listed,
		8: numbered, 9: ":::callout{}\nInner words.\n:::", 10: "***\n\nAfter a rule.\n\n---\n\nTail.",
		11: deep, 12: footnoted, 13: "Text of the callout.", 14: "Text of the callout, its fence three spaces in.",
		15: notAHeading,
	} {
		want := sameShape(t, pmdoc.DisplayJSON(content, pmdoc.DisplayLatest))
		if read[index].TextTree == nil {
			t.Errorf("%s carries no text_tree, want the tree of %q", read[index].Ref, content)
			continue
		}
		if got := sameShape(t, json.RawMessage(read[index].TextTree)); !bytes.Equal(got, want) {
			t.Errorf("%s: text_tree is\n\t%s\nwant the tree of %q\n\t%s", read[index].Ref, got, content, want)
		}
	}
	if read[4].Kind != "issue" || read[4].TextTree != nil {
		t.Errorf("the issue answers %#v, want no text_tree: only a block carries one", read[4])
	}
	if read[5].Kind != "block" {
		t.Errorf("the ask's part answers the kind %q, want block", read[5].Kind)
	}
	if strings.Contains(string(read[7].TextTree), "requirement{") {
		t.Errorf("the listed requirement's text_tree holds its opening line:\n\t%s", read[7].TextTree)
	}
	if tree := string(read[8].TextTree); strings.Contains(tree, "code_block") || strings.Count(tree, `"paragraph"`) != 2 {
		t.Errorf("the requirement in item 10 answers the text_tree\n\t%s\nwant its content as two paragraphs, not a code block", tree)
	}
	if tree := string(read[15].TextTree); !strings.Contains(tree, `"code_block"`) || strings.Contains(tree, `"heading"`) {
		t.Errorf("the comment's block with a line four spaces in answers the text_tree\n\t%s\nwant that line as a code block, not a heading", tree)
	}
	for _, member := range read[16:] {
		if member.TextTree == nil {
			t.Errorf("%s carries no text_tree, want the tree of %q", member.Ref, indentedOpener)
			continue
		}
		want := sameShape(t, pmdoc.DisplayJSON(indentedOpener, pmdoc.DisplayLatest))
		if got := sameShape(t, json.RawMessage(member.TextTree)); !bytes.Equal(got, want) {
			t.Errorf("%s: text_tree is\n\t%s\nwant the tree of %q\n\t%s", member.Ref, got, indentedOpener, want)
		}
	}

	nestedTarget := referenceTargetsOf(references[9])
	firstRead, secondRead := treesRequest(t, handler, nestedTarget, true), treesRequest(t, handler, nestedTarget, true)
	first := decodeBody[model.ReferenceTargets](t, firstRead).Targets
	second := decodeBody[model.ReferenceTargets](t, secondRead).Targets
	if len(first) != 1 || len(second) != 1 || first[0].TextTree == nil {
		t.Fatalf("the reads of the nested callout answered %#v and %#v, want one member with a text_tree each", first, second)
	}
	if !bytes.Equal(first[0].TextTree, second[0].TextTree) {
		t.Errorf("two reads of the nested callout answered the text_trees\n\t%s\nand\n\t%s\nwant the same bytes",
			first[0].TextTree, second[0].TextTree)
	}
	if strings.Contains(string(first[0].TextTree), pmdoc.BlockIDAttr) {
		t.Errorf("the nested callout's text_tree holds a block id the comment never wrote:\n\t%s", first[0].TextTree)
	}
	etag := firstRead.Header().Get("ETag")
	if etag == "" || secondRead.Header().Get("ETag") != etag {
		t.Errorf("two reads of the nested callout carry the ETags %q and %q, want one tag", etag, secondRead.Header().Get("ETag"))
	}
	if unchanged := listRead(t, handler, nestedTarget, listReadOptions{trees: true, etag: etag}); unchanged.Code != http.StatusNotModified {
		t.Errorf("the nested callout's first tag sent back answered status=%d body=%q, want 304", unchanged.Code, unchanged.Body.String())
	}

	ids := blockIDsByText(t, handler, notesDocument.Artifact.ID)
	twenty := make([]string, len(notes))
	for index, note := range notes {
		if ids[note] == "" {
			t.Fatalf("the notes list no block for %q", note)
		}
		twenty[index] = notesRef + "#b-" + text.EncodeBlockID(ids[note])
	}
	// Each set is read once before it is counted, and that read's store of the trees it built
	// is waited out: the store runs beside the request (displayTreeReader.storeOf) and is no
	// statement of the read's own.
	statementsOf := func(set []string) []string {
		t.Helper()
		target := referenceTargetsOf(set...)
		treesRequest(t, handler, target, true)
		awaitBackgroundWrites(t, served)
		counter.take()
		countedTreesRead(t, handler, target)
		return counter.take()
	}
	counts := map[int][]string{2: statementsOf(twenty[:2]), 20: statementsOf(twenty)}
	if len(counts[2]) != len(counts[20]) {
		t.Errorf("a set of 2 blocks of one document made %d statements and a set of 20 made %d, want as many:\n%s\n\n%s",
			len(counts[2]), len(counts[20]), strings.Join(roundTripLines(counts[2]), "\n"), strings.Join(roundTripLines(counts[20]), "\n"))
	}
	// The names' statement reads document_block_texts as well, for the version that first held
	// each block (refs.TargetNames); the texts' own statement is the one selecting each block's
	// text from it (refs.BlockTexts).
	for size, counted := range counts {
		texts := 0
		for _, statement := range counted {
			if strings.Contains(statement, "document_block_texts") && strings.Contains(statement, "held.text") {
				texts++
			}
		}
		if texts != 1 {
			t.Errorf("a set of %d blocks of one document read their texts from document_block_texts in %d statements, want 1:\n%s",
				size, texts, strings.Join(roundTripLines(counted), "\n"))
		}
	}
	short := make([]string, len(notes))
	for index, note := range notes {
		short[index] = notesRef + "#b-" + text.EncodeBlockID(ids[note][:8])
	}
	shortCounts := map[int][]string{2: statementsOf(short[:2]), 20: statementsOf(short)}
	if len(shortCounts[2]) != len(shortCounts[20]) {
		t.Errorf("a set of 2 short block references of one document made %d statements and a set of 20 made %d, want as many:\n%s\n\n%s",
			len(shortCounts[2]), len(shortCounts[20]), strings.Join(roundTripLines(shortCounts[2]), "\n"), strings.Join(roundTripLines(shortCounts[20]), "\n"))
	}
	if len(shortCounts[20]) != len(counts[20])+1 {
		t.Errorf("a set of 20 short block references made %d statements and the same set written whole made %d, want one more, the one that resolves the prefixes:\n%s",
			len(shortCounts[20]), len(counts[20]), strings.Join(roundTripLines(shortCounts[20]), "\n"))
	}

	edited := editIssueDocument(t, handler, issue.Key, plan.Artifact.Slug, []map[string]any{
		{"op": "replace", "find": wholeBlockTail, "with": newerBlockTail},
	})
	if edited.Number <= plan.Version.Number {
		t.Fatalf("the edit wrote version %d, want one after the upload's %d", edited.Number, plan.Version.Number)
	}
	pinned := referenceTargetOf(t, handler, planRef+"#b-t1@v"+strconv.Itoa(plan.Version.Number))
	if pinned.Gone || pinned.Kind != "block" {
		t.Fatalf("the plan task pinned to version %d answers %#v, want the block", plan.Version.Number, pinned)
	}
	requireWholeBlockTree(t, "the pinned plan task's text_tree", json.RawMessage(pinned.TextTree), issue.Key, newerBlockTail)
	if strings.Contains(string(pinned.TextTree), wholeBlockTail) {
		t.Errorf("the pinned plan task's text_tree holds the tail of the version it is pinned to:\n\t%s", pinned.TextTree)
	}
}

// The batched read's ETag covers each block's tree. A block's name is its stored excerpt, which
// stops at 480 runes, so a change past them moves the tree and leaves every name as it was: a
// tag over the names alone would answer 304 for a tree the page no longer holds. The same set
// unchanged is answered 304, and a read that asked for no tree never shares a tag with one that
// did: the tag of a read without trees, sent by a caller asking for them, is answered the trees.
func TestTheReferenceTargetsETagCoversABlocksWholeText(t *testing.T) {
	handler, _ := blockIndexHandler(t)
	createReferenceAPIProject(t, handler, "TBT")
	issue := createSpecIssue(t, handler, "TBT", twoRequirements)
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "plan.md",
		":::plan_task{#t1 serves=\"dispatch://"+issue.Key+"/spec#b-r1\"}\n"+wholeBlockContent(issue.Key, wholeBlockTail)+"\n:::\n")
	target := referenceTargetsOf("dispatch://" + issue.Key + "/artifact/plan-md#b-t1")
	conditional := func(etag string) *httptest.ResponseRecorder {
		t.Helper()
		return listRead(t, handler, target, listReadOptions{trees: true, etag: etag})
	}

	first := treesRequest(t, handler, target, true)
	etag := first.Header().Get("ETag")
	before := decodeBody[model.ReferenceTargets](t, first).Targets
	if len(before) != 1 || before[0].Gone {
		t.Fatalf("the read answered %#v, want the plan task", before)
	}
	requireWholeBlockTree(t, "the plan task's text_tree", json.RawMessage(before[0].TextTree), issue.Key, wholeBlockTail)
	if strings.Contains(before[0].Name, wholeBlockTail) {
		t.Fatalf("the task's name %q holds its tail, so the fixture decides nothing about a change past the excerpt", before[0].Name)
	}
	bare := treesRequest(t, handler, target, false).Header().Get("ETag")
	if bare == "" || bare == etag {
		t.Errorf("the read without trees carries the ETag %q and the read with them %q, want two tags for two bodies", bare, etag)
	}
	if crossed := conditional(bare); crossed.Code != http.StatusOK || crossed.Body.Len() == 0 {
		t.Errorf("the tag of the read without trees, sent with the header, answered status=%d body=%q, want 200 with a body",
			crossed.Code, crossed.Body.String())
	}
	if unchanged := conditional(etag); unchanged.Code != http.StatusNotModified || unchanged.Body.Len() != 0 {
		t.Fatalf("the unchanged set answered status=%d body=%q, want 304", unchanged.Code, unchanged.Body.String())
	}

	editIssueDocument(t, handler, issue.Key, "plan-md", []map[string]any{
		{"op": "replace", "find": wholeBlockTail, "with": newerBlockTail},
	})
	changed := conditional(etag)
	if changed.Code != http.StatusOK || changed.Header().Get("ETag") == etag {
		t.Fatalf("the set whose block changed past its excerpt answered status=%d etag=%q (was %q), want the new answer under a new tag",
			changed.Code, changed.Header().Get("ETag"), etag)
	}
	after := decodeBody[model.ReferenceTargets](t, changed).Targets
	if len(after) != 1 || after[0].Name != before[0].Name {
		t.Fatalf("the changed set answered %#v, want the name %q unchanged", after, before[0].Name)
	}
	requireWholeBlockTree(t, "the changed block's text_tree", json.RawMessage(after[0].TextTree), issue.Key, newerBlockTail)
	if strings.Contains(string(after[0].TextTree), wholeBlockTail) {
		t.Errorf("the changed block's text_tree is\n\t%s\nwant its new tail alone", after[0].TextTree)
	}
}
