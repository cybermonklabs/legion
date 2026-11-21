package api

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// referenceTargetsRequest reads what references point at, as alice, with etag, when set, as
// If-None-Match (listRead). Each reference rides as its own ?ref=, in the order given
// (referenceTargetsOf).
func referenceTargetsRequest(t *testing.T, handler http.Handler, references []string, etag string) *httptest.ResponseRecorder {
	t.Helper()
	return listRead(t, handler, referenceTargetsOf(references...), listReadOptions{etag: etag})
}

// referenceTargets is the answer of a read that must succeed.
func referenceTargets(t *testing.T, handler http.Handler, references []string) ([]model.ReferenceTarget, string) {
	t.Helper()
	response := referenceTargetsRequest(t, handler, references, "")
	if response.Code != http.StatusOK {
		t.Fatalf("read %d references: status=%d body=%s", len(references), response.Code, response.Body.String())
	}
	etag := response.Header().Get("ETag")
	if etag == "" {
		t.Fatalf("the read carries no ETag: %s", response.Body.String())
	}
	return decodeBody[model.ReferenceTargets](t, response).Targets, etag
}

// One request resolves a whole set: a block that exists, a block a later version deleted, an
// issue of its own, and that same issue written with whitespace around it. Each member carries
// the kind its reference names, the words a reader is shown for its target, and whether that
// target is gone — the deleted block answered, not refused, since a chip whose target is gone
// still has to draw — and each echoes its reference as the request wrote it, so a caller that
// matches the answer against the strings it sent finds every one of them.
//
// A second request names items and blocks by ids written short, which are read as GET
// /api/v1/references reads them: a comment, a block, an ask, a message and a block pinned to a
// version are each answered with the kind and the name of the one item of its owner that
// carries the prefix, and each echoes the short reference as the request wrote it. The
// comment's prefix under an issue that holds no comment names nothing, and is answered gone. A
// prefix of a document's id is answered gone too, though it resolves: ResolvePrefixes finds the
// document that carries it (artifactShortIDCandidates), and wholeReferences writes the document's
// uuid into the reference's ID, where a document's reference holds its slug. The graph keys a
// document by its slug (text.TargetID), so TargetNames finds nothing under that uuid and the
// member is answered gone. GET /api/v1/references reads such a reference the same way
// (wholeReference), as the design of the new issue page decided.
func TestReferenceTargetsResolveABatchInOneRequest(t *testing.T) {
	handler, _ := blockIndexHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "First paragraph.\n\nSecond paragraph.\n")
	other := createReferenceAPIIssue(t, handler, "CORE")
	ids := blockIDsByText(t, handler, issue.PrimaryArtifactID)
	specRef := "dispatch://" + issue.Key + "/spec"
	held := specRef + "#b-" + text.EncodeBlockID(ids["First paragraph."])
	gone := specRef + "#b-" + text.EncodeBlockID(ids["Second paragraph."])
	if ids["First paragraph."] == "" || ids["Second paragraph."] == "" {
		t.Fatalf("the spec's blocks are %v, want both paragraphs", ids)
	}
	edited := editSpec(t, handler, issue.Key, []map[string]any{{"op": "delete", "find": "Second paragraph."}})

	padded := "  dispatch://" + other.Key + "  "
	targets, _ := referenceTargets(t, handler, []string{held, gone, "dispatch://" + other.Key, padded})
	if len(targets) != 4 {
		t.Fatalf("the read answered %#v, want one member per reference", targets)
	}
	if want := (model.ReferenceTarget{Ref: held, Kind: "block", Name: "First paragraph."}); !reflect.DeepEqual(targets[0], want) {
		t.Errorf("the block that exists answered %#v, want %#v", targets[0], want)
	}
	if want := (model.ReferenceTarget{Ref: gone, Kind: "block", Gone: true}); !reflect.DeepEqual(targets[1], want) {
		t.Errorf("the deleted block answered %#v, want it gone and unnamed", targets[1])
	}
	if want := (model.ReferenceTarget{Ref: "dispatch://" + other.Key, Kind: "issue", Name: other.Title}); !reflect.DeepEqual(targets[2], want) {
		t.Errorf("the other issue answered %#v, want %#v", targets[2], want)
	}
	if want := (model.ReferenceTarget{Ref: padded, Kind: "issue", Name: other.Title}); !reflect.DeepEqual(targets[3], want) {
		t.Errorf("the padded reference answered %#v, want it resolved and echoed as it was written: %#v", targets[3], want)
	}

	comment := createReferenceAPIComment(t, handler, other.Key, "A comment named short.")
	ask := createIssueAsk(t, handler, other.Key, "An ask named short?")
	message := createIssueMessage(t, handler, other.Key, map[string]any{"body": "A message named short."}, "alice")
	notes := uploadDocument(t, handler, "/api/v1/issues/"+other.Key, "notes.md", "A note.\n")
	shortBlock := specRef + "#b-" + text.EncodeBlockID(ids["First paragraph."][:8])
	wanted := []model.ReferenceTarget{
		{Ref: "dispatch://" + other.Key + "/comment/" + comment.ID[:8], Kind: "comment", Name: "A comment named short."},
		{Ref: shortBlock, Kind: "block", Name: "First paragraph."},
		{Ref: "dispatch://" + issue.Key + "/comment/" + comment.ID[:8], Kind: "comment", Gone: true},
		{Ref: "dispatch://" + other.Key + "/ask/" + ask[:8], Kind: "ask", Name: "An ask named short?"},
		{Ref: "dispatch://" + other.Key + "/message/" + message.ID[:8], Kind: "message", Name: "A message named short."},
		{Ref: shortBlock + "@v" + strconv.Itoa(edited.Number), Kind: "block", Name: "First paragraph."},
		// Resolved to the document, then written under its uuid where its reference holds a
		// slug, so TargetNames finds nothing and the member is gone (the doc comment above).
		{Ref: "dispatch://" + other.Key + "/artifact/" + notes.Artifact.ID[:8], Kind: "artifact", Gone: true},
	}
	references := make([]string, len(wanted))
	for index, want := range wanted {
		references[index] = want.Ref
	}
	short, _ := referenceTargets(t, handler, references)
	if len(short) != len(wanted) {
		t.Fatalf("the read of short ids answered %#v, want one member per reference", short)
	}
	for index, want := range wanted {
		if !reflect.DeepEqual(short[index], want) {
			t.Errorf("%s answered %#v, want %#v", want.Ref, short[index], want)
		}
	}
}

// The ETag covers the whole answer: the same set, unchanged, is answered 304 under the same tag,
// and a set one of whose members was renamed is answered the new name under a new tag. Nothing
// else in the set moved, so a tag over the references alone would tell a page holding it 304 for
// an answer that now names the issue differently.
func TestTheReferenceTargetsETagAnswers304UntilAMemberChanges(t *testing.T) {
	handler, _ := blockIndexHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "First paragraph.\n")
	other := createReferenceAPIIssue(t, handler, "CORE")
	ids := blockIDsByText(t, handler, issue.PrimaryArtifactID)
	set := []string{
		"dispatch://" + issue.Key + "/spec#b-" + text.EncodeBlockID(ids["First paragraph."]),
		"dispatch://" + other.Key,
	}

	_, etag := referenceTargets(t, handler, set)
	unchanged := referenceTargetsRequest(t, handler, set, etag)
	if unchanged.Code != http.StatusNotModified || unchanged.Body.Len() != 0 || unchanged.Header().Get("ETag") != etag {
		t.Fatalf("the unchanged set answered status=%d etag=%q body=%q, want 304 under the same tag",
			unchanged.Code, unchanged.Header().Get("ETag"), unchanged.Body.String())
	}

	renamed := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+other.Key, map[string]any{
		"title": "Reference issue, renamed",
	}, "alice")
	if renamed.Code != http.StatusOK {
		t.Fatalf("retitle the other issue: status=%d body=%s", renamed.Code, renamed.Body.String())
	}
	changed := referenceTargetsRequest(t, handler, set, etag)
	if changed.Code != http.StatusOK {
		t.Fatalf("the changed set answered status=%d body=%s, want the new answer", changed.Code, changed.Body.String())
	}
	if tag := changed.Header().Get("ETag"); tag == etag {
		t.Errorf("the changed set kept the ETag %s", tag)
	}
	targets := decodeBody[model.ReferenceTargets](t, changed).Targets
	if len(targets) != 2 || targets[1].Name != "Reference issue, renamed" {
		t.Errorf("the changed set answered %#v, want the issue's new title", targets)
	}
}

// A set at the cap is read, one past it is refused naming the cap, and a reference the grammar
// cannot read - an external URL, or a reference to a person, which it reads none of - is refused
// naming it. The cap is counted before anything is parsed, so a set past it costs no
// reference's parse. A block id written short that two blocks of the document carry refuses
// nothing: that reference is answered as one member, gone and ambiguous, with no name, and the
// other member of the set is answered with its name. The other member's answer carries no key
// ambiguous at all. The tag covers ambiguous: once no block carries the prefix, the reference is
// gone and not ambiguous, every other field as it was, and the ambiguous answer's tag sent back
// is answered with the new answer under a new tag, never 304.
func TestReferenceTargetsRefuseASetPastTheCapAndAReferenceItCannotRead(t *testing.T) {
	handler, _ := blockIndexHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createReferenceAPIIssue(t, handler, "CORE")
	reference := "dispatch://" + issue.Key

	atCap := make([]string, referenceTargetsMax)
	for index := range atCap {
		atCap[index] = reference
	}
	targets, _ := referenceTargets(t, handler, atCap)
	if len(targets) != referenceTargetsMax || targets[referenceTargetsMax-1].Name != issue.Title {
		t.Fatalf("a set of %d references answered %d members, want one per reference, each the issue",
			referenceTargetsMax, len(targets))
	}

	past := referenceTargetsRequest(t, handler, append(atCap, reference), "")
	if past.Code != http.StatusBadRequest ||
		!strings.Contains(past.Body.String(), `"code":"CAP_EXCEEDED"`) ||
		!strings.Contains(past.Body.String(), "ref is 1 over the 256-item limit (257/256)") {
		t.Fatalf("a set one past the cap answered status=%d body=%s, want 400 CAP_EXCEEDED naming the cap",
			past.Code, past.Body.String())
	}

	unreadable := referenceTargetsRequest(t, handler, []string{reference, "https://example.com/notes"}, "")
	if unreadable.Code != http.StatusBadRequest ||
		!strings.Contains(unreadable.Body.String(), `"code":"INVALID_REFERENCE"`) ||
		!strings.Contains(unreadable.Body.String(), "https://example.com/notes") {
		t.Fatalf("a reference the grammar cannot read answered status=%d body=%s, want 400 INVALID_REFERENCE naming it",
			unreadable.Code, unreadable.Body.String())
	}

	const person = "dispatch://person/alice@example.com"
	personal := referenceTargetsRequest(t, handler, []string{reference, person}, "")
	if personal.Code != http.StatusBadRequest ||
		!strings.Contains(personal.Body.String(), `"code":"INVALID_REFERENCE"`) ||
		!strings.Contains(personal.Body.String(), person) {
		t.Fatalf("a reference to a person answered status=%d body=%s, want 400 INVALID_REFERENCE naming it",
			personal.Code, personal.Body.String())
	}

	none := referenceTargetsRequest(t, handler, nil, "")
	if none.Code != http.StatusBadRequest || !strings.Contains(none.Body.String(), `"code":"INVALID_REFERENCE"`) {
		t.Fatalf("a read naming no reference answered status=%d body=%s, want 400 INVALID_REFERENCE",
			none.Code, none.Body.String())
	}

	shared := createSpecIssue(t, handler, "CORE", ":::requirement{#deadbeef-one}\nThe first requirement.\n:::\n\n"+
		":::requirement{#deadbeef-two}\nThe second requirement.\n:::\n")
	short := "dispatch://" + shared.Key + "/spec#b-deadbeef"
	answered := referenceTargetsRequest(t, handler, []string{short, reference}, "")
	if answered.Code != http.StatusOK {
		t.Fatalf("a set that holds a block prefix two blocks carry answered status=%d body=%s, want 200",
			answered.Code, answered.Body.String())
	}
	members := decodeBody[model.ReferenceTargets](t, answered).Targets
	wanted := []model.ReferenceTarget{
		{Ref: short, Kind: "block", Name: "", Gone: true, Ambiguous: true},
		{Ref: reference, Kind: "issue", Name: issue.Title},
	}
	if !reflect.DeepEqual(members, wanted) {
		t.Fatalf("a set that holds a block prefix two blocks carry answered %#v, want %#v", members, wanted)
	}
	keys := decodeBody[struct {
		Targets []map[string]any `json:"targets"`
	}](t, answered).Targets
	if keys[0]["ambiguous"] != true {
		t.Errorf("the ambiguous member is %v, want the key ambiguous true", keys[0])
	}
	if _, carried := keys[1]["ambiguous"]; carried {
		t.Errorf("the other member is %v, want no key ambiguous", keys[1])
	}

	ambiguousTag := answered.Header().Get("ETag")
	uploadDocument(t, handler, "/api/v1/issues/"+shared.Key, "spec.md", ":::requirement{#cafe-one}\nThe only requirement.\n:::\n")
	cleared := referenceTargetsRequest(t, handler, []string{short, reference}, ambiguousTag)
	if cleared.Code != http.StatusOK || cleared.Header().Get("ETag") == ambiguousTag {
		t.Fatalf("once no block carries the prefix, the ambiguous answer's tag answered status=%d etag=%q (was %q), want the new answer under a new tag",
			cleared.Code, cleared.Header().Get("ETag"), ambiguousTag)
	}
	wanted[0].Ambiguous = false
	if members := decodeBody[model.ReferenceTargets](t, cleared).Targets; !reflect.DeepEqual(members, wanted) {
		t.Fatalf("once no block carries the prefix, the set answered %#v, want %#v", members, wanted)
	}
}

// A member naming a comment or an issue message, or the first block of one (#b-b1), carries its
// post's marks as every read of the post does, with trees and without. Before any correction no
// member carries a mark, each is named by its post's words, and each block's tree, for a caller
// that asked for trees, holds them. After the author's edit, which leaves the first block as it
// was, each member carries the edit's time, the block's name and tree as they were. After the
// delete no member gives a word of either post, with trees or without: a member naming the post
// keeps its place, not gone, named "" and carrying the delete's mark, and a member naming its
// block is gone, with no name and no tree and the same mark, as a block read of it is refused
// naming the deletion. Each still carries the time of the edit before the delete, whether the
// request names the post and its block (the whole set) or the blocks alone. The tag of a set
// naming the two blocks alone, which the edit leaves as they were, changes at the edit and again
// at the delete: a holder of the older tag is answered the new answer, never 304.
func TestReferenceTargetsMarkAnEditedPostAndGiveNoWordOfADeletedOne(t *testing.T) {
	handler, _ := blockIndexHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createReferenceAPIIssue(t, handler, "CORE")
	const commentWords, messageWords = "zarquoncommentwords", "zarquonmessagewords"
	comment := createReferenceAPIComment(t, handler, issue.Key, commentWords+" first block.\n\nSecond block.")
	message := createIssueMessage(t, handler, issue.Key, map[string]any{"body": messageWords + " first block.\n\nSecond block."}, "alice")
	commentRef := "dispatch://" + issue.Key + "/comment/" + comment.ID
	messageRef := "dispatch://" + issue.Key + "/message/" + message.ID
	set := []string{commentRef, commentRef + "#b-b1", messageRef, messageRef + "#b-b1"}
	words := []string{commentWords, commentWords, messageWords, messageWords}
	blocks := []string{set[1], set[3]}

	// read is the answer to the whole set, as a caller given trees or given none is answered it,
	// and that answer's body.
	read := func(trees bool) ([]model.ReferenceTarget, string) {
		t.Helper()
		response := listRead(t, handler, referenceTargetsOf(set...), listReadOptions{trees: trees})
		if response.Code != http.StatusOK {
			t.Fatalf("read the set (trees %t): status=%d body=%s", trees, response.Code, response.Body.String())
		}
		members := decodeBody[model.ReferenceTargets](t, response).Targets
		if len(members) != len(set) {
			t.Fatalf("the set (trees %t) answered %#v, want one member per reference", trees, members)
		}
		return members, response.Body.String()
	}
	// changedBlocks reads the two blocks holding etag, and fails t unless it is answered the new
	// answer under a new tag, which it answers.
	changedBlocks := func(what, etag string) ([]model.ReferenceTarget, string) {
		t.Helper()
		response := referenceTargetsRequest(t, handler, blocks, etag)
		if response.Code != http.StatusOK || response.Header().Get("ETag") == etag {
			t.Fatalf("the blocks %s, read holding the tag before it, answered status=%d etag=%q (was %q), want the new answer under a new tag",
				what, response.Code, response.Header().Get("ETag"), etag)
		}
		return decodeBody[model.ReferenceTargets](t, response).Targets, response.Header().Get("ETag")
	}

	// standingTrees is each block member's tree before any correction, which the edit leaves as
	// it was.
	standingTrees := make([]string, len(set))
	for _, trees := range []bool{false, true} {
		members, _ := read(trees)
		for index, member := range members {
			if member.Gone || !strings.Contains(member.Name, words[index]) {
				t.Errorf("before any correction (trees %t) %s answered %#v, want its post's words", trees, set[index], member)
			}
			if member.EditedAt != nil || member.DeletedAt != nil || member.DeletedBy != nil {
				t.Errorf("before any correction (trees %t) %s carries a mark: %#v", trees, set[index], member)
			}
			if block := member.Kind == "block"; trees && block != strings.Contains(string(member.TextTree), words[index]) {
				t.Errorf("before any correction (trees %t) %s answered the tree %s, want its block's words in a block's tree alone", trees, set[index], member.TextTree)
			}
			if trees {
				standingTrees[index] = string(member.TextTree)
			}
		}
	}
	standingBlocks, standingTag := referenceTargets(t, handler, blocks)

	commentEdited := editThreadComment(t, handler, comment.ID, commentWords+" first block.\n\nSecond block, corrected.", "alice")
	messageEdited := editIssueMessage(t, handler, message.ID, messageWords+" first block.\n\nSecond block, corrected.", "alice").EditedAt
	if messageEdited == nil {
		t.Fatalf("the edit of the message answered no edited_at")
	}
	editedAt := []time.Time{commentEdited, commentEdited, *messageEdited, *messageEdited}
	for _, trees := range []bool{false, true} {
		members, _ := read(trees)
		for index, member := range members {
			if member.Gone || !strings.Contains(member.Name, words[index]) || member.DeletedAt != nil || member.DeletedBy != nil {
				t.Errorf("after the edit (trees %t) %s answered %#v, want its post's words and no delete mark", trees, set[index], member)
			}
			if member.EditedAt == nil || !member.EditedAt.Equal(editedAt[index]) {
				t.Errorf("after the edit (trees %t) %s carries edited_at %v, want the edit's time %s", trees, set[index], member.EditedAt, editedAt[index])
			}
			if trees && member.Kind == "block" && string(member.TextTree) != standingTrees[index] {
				t.Errorf("after the edit (trees %t) %s answered the tree %s, want the tree the edit left as it was, %s", trees, set[index], member.TextTree, standingTrees[index])
			}
		}
	}
	editedBlocks, editedTag := changedBlocks("after the edit", standingTag)
	for index, member := range editedBlocks {
		if member.Name != standingBlocks[index].Name {
			t.Errorf("after the edit %s is named %q, want the name the edit left as it was, %q", blocks[index], member.Name, standingBlocks[index].Name)
		}
	}

	commentDeleted := deleteThreadComment(t, handler, comment.ID, "alice").DeletedAt
	messageDeleted := deleteIssueMessage(t, handler, message.ID, "alice").DeletedAt
	if commentDeleted == nil || messageDeleted == nil {
		t.Fatalf("the deletes answered no mark: comment %v, message %v", commentDeleted, messageDeleted)
	}
	deletedAt := []time.Time{*commentDeleted, *commentDeleted, *messageDeleted, *messageDeleted}
	// deleteMarked fails t unless member, answered for ref after the delete in the read what names,
	// carries the edit before the delete at edited and alice's delete at deleted.
	deleteMarked := func(what, ref string, member model.ReferenceTarget, edited, deleted time.Time) {
		t.Helper()
		if member.EditedAt == nil || !member.EditedAt.Equal(edited) {
			t.Errorf("after the delete (%s) %s carries edited_at %v, want the edit's time %s", what, ref, member.EditedAt, edited)
		}
		if member.DeletedAt == nil || !member.DeletedAt.Equal(deleted) ||
			member.DeletedBy == nil || member.DeletedBy.Kind != "user" || member.DeletedBy.ID != "alice" {
			t.Errorf("after the delete (%s) %s carries the mark %v by %#v, want alice's delete at %s", what, ref, member.DeletedAt, member.DeletedBy, deleted)
		}
	}
	for _, trees := range []bool{false, true} {
		members, body := read(trees)
		for _, word := range []string{commentWords, messageWords} {
			if strings.Contains(body, word) {
				t.Errorf("after the delete (trees %t) the read gives the deleted words %q: %s", trees, word, body)
			}
		}
		for index, member := range members {
			if block := member.Kind == "block"; member.Gone != block || member.Name != "" || len(member.TextTree) != 0 {
				t.Errorf("after the delete (trees %t) %s answered %#v, want no name and no tree, the post in its place and its block gone", trees, set[index], member)
			}
			deleteMarked("the post and its block, trees "+strconv.FormatBool(trees), set[index], member, editedAt[index], deletedAt[index])
		}
	}
	deletedBlocks, _ := changedBlocks("after the delete", editedTag)
	if len(deletedBlocks) != len(blocks) {
		t.Fatalf("after the delete the blocks alone answered %#v, want one member per block", deletedBlocks)
	}
	for index, member := range deletedBlocks {
		if !member.Gone || member.Name != "" {
			t.Errorf("after the delete (the blocks alone) %s answered %#v, want it gone with no name", blocks[index], member)
		}
		deleteMarked("the blocks alone", blocks[index], member, editedAt[2*index+1], deletedAt[2*index+1])
	}
}
