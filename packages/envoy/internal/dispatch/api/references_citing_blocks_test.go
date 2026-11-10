package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// paragraphBlockIDs are the ids of the document's paragraph blocks, sorted as the references
// read sorts the blocks it names.
func paragraphBlockIDs(t *testing.T, handler http.Handler, artifactID string) []string {
	t.Helper()
	ids := []string{}
	for _, block := range decodeBody[[]model.ArtifactBlock](t, dispatchRequest(
		t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/blocks", nil, "alice")) {
		if block.Type == "paragraph" {
			ids = append(ids, block.ID)
		}
	}
	slices.Sort(ids)
	return ids
}

// The references read names the blocks citing each document it answers. The answer keeps one
// member per document, so a document two blocks of one citer cite is answered once, naming both
// blocks; a document no block cites — here one whose citer the block index has not reached, the
// state every document was in before migration 0121 — is answered with none rather than an empty
// name.
func TestIssueReferencesNameTheBlocksCitingEachDocument(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createReferenceAPIIssue(t, handler, "CORE")
	twiceCited := createProjectDocument(t, handler, "CORE", "Twice cited", "# Twice cited\n")
	onceCited := createProjectDocument(t, handler, "CORE", "Once cited", "# Once cited\n")
	citer := createProjectDocument(t, handler, "CORE", "Citing notes",
		"# Citing notes\n\nFirst, see dispatch://CORE/artifact/twice-cited\n\nAgain, see dispatch://CORE/artifact/twice-cited\n")
	unindexed := createProjectDocument(t, handler, "CORE", "Unindexed notes",
		"# Unindexed notes\n\nSee dispatch://CORE/artifact/once-cited\n")
	if _, err := database.Pool.Exec(context.Background(),
		`delete from block_refs where artifact_id = $1`, unindexed.ID); err != nil {
		t.Fatalf("leave the citing document's blocks unindexed: %v", err)
	}
	createReferenceAPIComment(t, handler, issue.Key,
		"See dispatch://CORE/artifact/citing-notes and dispatch://CORE/artifact/unindexed-notes")

	response := issueReferencesRequest(t, handler, issue.Key, "")
	if response.Code != http.StatusOK {
		t.Fatalf("read the issue's references: status=%d body=%s", response.Code, response.Body.String())
	}
	answer := decodeBody[model.IssueReferences](t, response)
	members := map[string]model.ReferenceMember{}
	for _, member := range answer.Members {
		members[member.Artifact.ID] = member
	}
	if answer.Truncated || len(members) != 4 {
		t.Fatalf("the read answered %#v; want the two citers and the two documents they cite", answer)
	}

	twice := members[twiceCited.ID]
	wantBlocks := paragraphBlockIDs(t, handler, citer.ID)
	if len(wantBlocks) != 2 {
		t.Fatalf("the citing document holds %d paragraphs, want the two that cite: %v", len(wantBlocks), wantBlocks)
	}
	if twice.Via != (model.ReferenceVia{Kind: "artifact", ID: citer.ID}) {
		t.Fatalf("the twice-cited document's via = %#v, want the citing document", twice.Via)
	}
	if !slices.Equal(twice.ViaBlocks, wantBlocks) {
		t.Errorf("the twice-cited document's via_blocks = %v, want both citing blocks %v", twice.ViaBlocks, wantBlocks)
	}

	once := members[onceCited.ID]
	if once.Via != (model.ReferenceVia{Kind: "artifact", ID: unindexed.ID}) {
		t.Fatalf("the once-cited document's via = %#v, want the unindexed document", once.Via)
	}
	if once.ViaBlocks != nil {
		t.Errorf("the once-cited document's via_blocks = %v, want none: no block of its citer holds the citation", once.ViaBlocks)
	}
	if got := strings.Count(response.Body.String(), `"via_blocks":null`); got != 1 {
		t.Errorf("the answer carries %d null via_blocks, want the one member no block cites:\n%s", got, response.Body.String())
	}
}

// The ETag covers the blocks citing a member: a member no block cites, one a block cites and one
// the citation moved to another block of the same document hash to three different tags. Without
// that, a client holding the tag from before the move would be told 304 for an answer naming the
// other block, since neither the issue's last_seq nor any member's version need have moved with
// a citation that stayed inside one document.
func TestTheReferencesETagCoversTheBlocksCitingAMember(t *testing.T) {
	member := model.ReferenceMember{
		Artifact: model.Artifact{
			ID:       "4a2b4e8c-6d0a-4f44-9a7e-2f6c6d9a1b23",
			Versions: []model.Version{{Number: 3}},
		},
		Depth: 2,
		Via:   model.ReferenceVia{Kind: "artifact", ID: "9f1c8b7a-2d34-4c55-8e66-7a8b9c0d1e2f"},
	}
	tag := func(blocks ...string) string {
		member.ViaBlocks = blocks
		return referencesETag(7, []model.ReferenceMember{member}, false, false, false)
	}
	cited, atOneBlock, atAnother := tag(), tag("b2"), tag("b3")
	if cited == atOneBlock || cited == atAnother || atOneBlock == atAnother {
		t.Fatalf("the ETags of no block (%s), b2 (%s) and b3 (%s) are not three distinct tags",
			cited, atOneBlock, atAnother)
	}
}

// The ETag names the shape of the answer it stands for: the format of the hash itself, the
// ?versions variant and the ?own variant. Two answers of one issue that differ in any of those
// are two different bodies, and a tag they shared would tell a client holding one 304 for the
// other. The format is in the tag because the fields the hash covers have grown: the build
// before via_blocks hashed a member no block cites to the very same bytes, so over a rollout
// the two builds answered one tag for two bodies, the older of which names no via_blocks at
// all, and a conditional read that crossed builds was answered 304 for a body the page did not
// hold.
func TestTheReferencesETagNamesTheShapeOfItsAnswer(t *testing.T) {
	members := []model.ReferenceMember{{
		Artifact: model.Artifact{
			ID:       "4a2b4e8c-6d0a-4f44-9a7e-2f6c6d9a1b23",
			Versions: []model.Version{{Number: 3}},
		},
		Depth: 2,
		Via:   model.ReferenceVia{Kind: "artifact", ID: "9f1c8b7a-2d34-4c55-8e66-7a8b9c0d1e2f"},
	}}
	// legacy is referencesETag as the build before via_blocks wrote it, for these members:
	// last_seq and truncated, `versions=latest` on the trimmed answer alone, and a line per
	// member of its document and its latest version.
	legacy := func(latestOnly bool) string {
		hash := sha256.New()
		_, _ = fmt.Fprintf(hash, "%d\n%t\n", 7, false)
		if latestOnly {
			_, _ = io.WriteString(hash, "versions=latest\n")
		}
		for _, member := range members {
			_, _ = fmt.Fprintf(hash, "%s@%d\n", member.Artifact.ID, member.Artifact.Versions[0].Number)
		}
		return `"` + hex.EncodeToString(hash.Sum(nil)) + `"`
	}

	shapes := []struct {
		name       string
		latestOnly bool
		own        bool
	}{
		{"versions=all own=0", false, false},
		{"versions=all own=1", false, true},
		{"versions=latest own=0", true, false},
		{"versions=latest own=1", true, true},
	}
	tags := map[string]string{}
	for _, shape := range shapes {
		tag := referencesETag(7, members, false, shape.latestOnly, shape.own)
		if older := legacy(shape.latestOnly); tag == older {
			t.Errorf("%s hashes to %s, the tag the build before via_blocks wrote for a body "+
				"carrying none, so a conditional read across a rollout is answered 304 for "+
				"the other build's body", shape.name, tag)
		}
		if shared, found := tags[tag]; found {
			t.Errorf("%s and %s share the ETag %s, so a client asking for one is told 304 for "+
				"the other", shape.name, shared, tag)
		}
		tags[tag] = shape.name
	}
}

// createIssueDocument uploads one document to an issue and answers it.
func createIssueDocument(t *testing.T, handler http.Handler, issueKey, name, content string) model.Artifact {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/artifacts", map[string]any{
		"name": name, "content": content,
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create issue document %q: status=%d body=%s", name, response.Code, response.Body.String())
	}
	return decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, response).Artifact
}

// ?own= decides whether the issue's own documents are members. Absent, the read answers what
// the issue reaches outside itself, which is the answer every caller before the parameter got;
// with 1 it answers the issue's own documents too, each with the blocks of its citer, which is
// what a reader drawing a document under the block citing it needs and what spares it a read of
// every document's stored text. Any other value is refused rather than read as one of the two,
// an own given with no value among them: the two spellings the server takes are the two the
// OpenAPI document declares (issueReferencesQuery.Own), so a client built from it can write
// nothing this refuses and ask nothing this answers narrowly by surprise.
func TestIssueReferencesAnswerTheIssuesOwnDocumentsOnlyWhenAsked(t *testing.T) {
	handler, _ := newTestHandlerWithStore(t)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createReferenceAPIIssue(t, handler, "CORE")
	outside := createProjectDocument(t, handler, "CORE", "Outside notes", "# Outside notes\n")
	cited := createIssueDocument(t, handler, issue.Key, "Cited notes", "# Cited notes\n")
	citer := createIssueDocument(t, handler, issue.Key, "Citing notes",
		"# Citing notes\n\nThe measurements are in dispatch://"+issue.Key+"/artifact/cited-notes\n")
	createReferenceAPIComment(t, handler, issue.Key, "See dispatch://CORE/artifact/outside-notes")

	narrow := issueReferencesRequest(t, handler, issue.Key, "")
	if narrow.Code != http.StatusOK {
		t.Fatalf("read the issue's references: status=%d body=%s", narrow.Code, narrow.Body.String())
	}
	answered := decodeBody[model.IssueReferences](t, narrow)
	if len(answered.Members) != 1 || answered.Members[0].Artifact.ID != outside.ID {
		t.Fatalf("the read without ?own answered %#v, want the one document outside the issue", answered.Members)
	}

	wide := issueReferencesQueryRequest(t, handler, issue.Key, "own=1", "")
	if wide.Code != http.StatusOK {
		t.Fatalf("read the issue's references with ?own=1: status=%d body=%s", wide.Code, wide.Body.String())
	}
	members := map[string]model.ReferenceMember{}
	for _, member := range decodeBody[model.IssueReferences](t, wide).Members {
		members[member.Artifact.ID] = member
	}
	if len(members) != 2 {
		t.Fatalf("the read with ?own=1 answered %#v, want the outside document and the issue's own cited one", members)
	}
	own, found := members[cited.ID]
	if !found {
		t.Fatalf("the read with ?own=1 answered %#v, naming no member for the issue's own cited document", members)
	}
	if own.Via != (model.ReferenceVia{Kind: "artifact", ID: citer.ID}) {
		t.Fatalf("the cited document's via = %#v, want the issue's own citing document", own.Via)
	}
	if want := paragraphBlockIDs(t, handler, citer.ID); !slices.Equal(own.ViaBlocks, want) {
		t.Errorf("the cited document's via_blocks = %v, want the citing paragraph %v", own.ViaBlocks, want)
	}
	if narrow.Header().Get("ETag") == wide.Header().Get("ETag") {
		t.Errorf("the narrow and the wide answer share the ETag %q, so a client asking for one would be told 304 for the other", wide.Header().Get("ETag"))
	}

	for _, value := range []string{"", "2", "true", "yes"} {
		refused := issueReferencesQueryRequest(t, handler, issue.Key, "own="+value, "")
		if refused.Code != http.StatusBadRequest || !strings.Contains(refused.Body.String(), `"code":"INVALID_QUERY"`) {
			t.Errorf("?own=%s: status=%d body=%s, want 400 INVALID_QUERY", value, refused.Code, refused.Body.String())
		}
	}
	twice := issueReferencesQueryRequest(t, handler, issue.Key, "own=1&own=1", "")
	if twice.Code != http.StatusBadRequest {
		t.Errorf("?own given twice: status=%d body=%s, want 400", twice.Code, twice.Body.String())
	}
}
