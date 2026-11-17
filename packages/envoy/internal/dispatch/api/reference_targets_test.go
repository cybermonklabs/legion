package api

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

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
