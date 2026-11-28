package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// c-render-at-version's case of a PATCH that changes the head render and no byte of an earlier
// version (R7, R15), which TestTheDecisionsQueryVersionsRenderTheirAnswerAsOfTheirRow runs on its own
// server.

// renderAtVersionPatchSpec is the spec of the PATCH case's issue: two criteria, k1 and k2, and two
// top-level paragraphs, the second of which the case moves under the first. The requirement under k1
// is written after the create (renderAtVersionPatchRequirement), once the issue's key is known.
const renderAtVersionPatchSpec = ":::criterion{#k1}\nThe first criterion of the move.\n:::\n\n" +
	":::criterion{#k2}\nThe second criterion of the move.\n:::\n\n" +
	renderAtVersionPatchFirst + "\n\n" + renderAtVersionPatchSecond + "\n"

// The two room paragraphs of renderAtVersionPatchSpec.
const (
	renderAtVersionPatchFirst  = "The first room paragraph."
	renderAtVersionPatchSecond = "The second room paragraph."
)

// renderAtVersionPatchRequirement is the requirement r1 whose parent attribute names parent.
func renderAtVersionPatchRequirement(parent string) string {
	return ":::requirement{#r1 parent=\"" + parent + "\"}\nThe requirement the move takes.\n:::\n"
}

// renderAtVersionTextDigest is artifact_version_text_sha256 of version number of artifactID: the
// digest an approval of that version pins, its record's where its row holds one.
func renderAtVersionTextDigest(t *testing.T, database *store.Store, artifactID string, number int) string {
	t.Helper()
	var digest *string
	if err := database.Pool.QueryRow(context.Background(), `select artifact_version_text_sha256($1::uuid, $2)`, artifactID, number).Scan(&digest); err != nil {
		t.Fatalf("read artifact_version_text_sha256 of version %d of %s: %v", number, artifactID, err)
	}
	if digest == nil {
		t.Fatalf("artifact_version_text_sha256 answers null for version %d of %s", number, artifactID)
	}
	return *digest
}

// renderAtVersionTextFacts is what the store holds of the text of the block node address: its text
// value, its text_parent and its text_rank, as a failure names them.
func renderAtVersionTextFacts(t *testing.T, database *store.Store, address string) string {
	t.Helper()
	ctx := context.Background()
	read := func(attribute string) string {
		value, held, err := graphstore.CurrentValue(ctx, database.Pool, refs.BlockNodeKind, address, attribute)
		if err != nil {
			t.Fatalf("read the %s of %s: %v", attribute, address, err)
		}
		if !held {
			return "none"
		}
		return value
	}
	return "text " + read(graphstore.AttributeText) + ", text_parent " + textParentOf(t, database, address) + ", text_rank " + read(refs.AttributeTextRank)
}

// renderAtVersionPatchCase: a PATCH can change the head render without changing the bytes of an
// earlier version (R7, R15). On an issue born converted, the spec's version that holds the
// requirement r1 with parent= naming k1 (written by an edit) renders from the tree as its stored
// bytes; the case saves those bytes and its digest (artifact_version_text_sha256). It moves r1 under
// k2 through PATCH /api/v1/issues/{key}/nodes/{id} with the If-Match of r1's current placement_seq:
// the tree places r1 under k2, the head and the version the move wrote each hold r1's parent= naming
// k2 and not k1, and the head differs from the saved bytes. The saved version still renders its
// saved bytes from the tree, through docs.RenderVersion and the version route alike, and its digest
// is the saved one. Then it moves the second room paragraph under the first: the tree places it
// under the first paragraph, the head's markdown is the flat text it was, no version is written,
// and the paragraph's text value, text_parent and text_rank are the ones it held before.
func renderAtVersionPatchCase(t *testing.T, handler http.Handler, database *store.Store, project string) {
	issue, tree := createRoomIssue(t, handler, database, project, "A move the PATCH takes", renderAtVersionPatchSpec, "")
	if tree == "" {
		t.Fatalf("issue %s holds no tree, want it born converted", issue.Key)
	}
	key := issue.Key
	specRef := "dispatch://" + key + "/spec"
	k1Ref, k2Ref := refs.BlockRef(specRef, "k1"), refs.BlockRef(specRef, "k2")
	editSpec(t, handler, key, []map[string]any{{"op": "insert", "after": "end", "markdown": renderAtVersionPatchRequirement(k1Ref)}})
	renderAtVersionQuiet(t, database, key, issue.PrimaryArtifactID)
	spec := readDecisionDocument(t, database, key, "spec")
	r1, k1, k2 := spec.address("r1"), spec.address("k1"), spec.address("k2")

	// The version that holds r1 with parent= naming k1: its render and its digest, saved.
	saved := renderAtVersionNewest(t, database, spec.id)
	savedBytes := storedVersionMarkdown(t, database, spec.id, saved)
	if line := lineWith(savedBytes, "requirement{#r1"); !strings.Contains(line, `parent="`+k1Ref+`"`) {
		t.Fatalf("version %d of %s holds %q for r1, want parent= naming %s", saved, spec.refKey, line, k1Ref)
	}
	if placed, _ := placementOf(t, database, refs.BlockNodeKind, r1); placed.ToID != k1 {
		t.Fatalf("before the move %s stands under %s %s, want %s", r1, placed.ToKind, placed.ToID, k1)
	}
	requireRenderAtVersionFromTree(t, database, spec.refKey, spec.id, saved, savedBytes)
	savedDigest := renderAtVersionTextDigest(t, database, spec.id, saved)

	// The move of r1 under k2, with the current If-Match.
	_, tag := placementOf(t, database, refs.BlockNodeKind, r1)
	if moved := patchNode(t, handler, key, r1, tag, map[string]any{"parent": k2}, "alice"); moved.Code != http.StatusOK {
		t.Fatalf("the move of %s under %s answered %d %s, want 200", r1, k2, moved.Code, moved.Body.String())
	}
	renderAtVersionQuiet(t, database, key, issue.PrimaryArtifactID)
	if placed, _ := placementOf(t, database, refs.BlockNodeKind, r1); placed.ToID != k2 {
		t.Errorf("the move of %s under %s left it under %s %s", r1, k2, placed.ToKind, placed.ToID)
	}
	movedTo := renderAtVersionNewest(t, database, spec.id)
	if movedTo <= saved {
		t.Fatalf("the move of %s under %s leaves %s at version %d, the version before it, want a version that holds the changed parent=", r1, k2, spec.refKey, movedTo)
	}
	head := artifactTextOf(t, handler, spec.id)
	for _, read := range []struct{ what, markdown string }{
		{"the head of " + spec.refKey, head},
		{"version " + graphstore.VersionID(spec.refKey, movedTo), storedVersionMarkdown(t, database, spec.id, movedTo)},
	} {
		if line := lineWith(read.markdown, "requirement{#r1"); !strings.Contains(line, `parent="`+k2Ref+`"`) || strings.Contains(line, k1Ref) {
			t.Errorf("%s holds %q for r1 after its move under k2, want parent= naming %s and not %s", read.what, line, k2Ref, k1Ref)
		}
	}
	if head == savedBytes {
		t.Errorf("the head of %s after the move of %s reads the bytes of version %d, want the changed parent=", spec.refKey, r1, saved)
	}

	// The earlier version still renders its saved bytes, and its digest is the one saved.
	requireRenderAtVersionFromTree(t, database, spec.refKey, spec.id, saved, savedBytes)
	if routed := versionRender(t, handler, spec.id, saved); routed != savedBytes {
		t.Errorf("the version route answers version %d of %s with %q after the move, want its saved bytes %q", saved, spec.refKey, routed, savedBytes)
	}
	if digest := renderAtVersionTextDigest(t, database, spec.id, saved); digest != savedDigest {
		t.Errorf("version %d of %s digests to %s after the move, want the %s saved before it", saved, spec.refKey, digest, savedDigest)
	}

	// The move of a room paragraph under another paragraph: the tree places it, and neither the
	// markdown nor the paragraph's text facts move.
	first, _ := textBlockOf(t, database, spec.id, "paragraph", renderAtVersionPatchFirst)
	second, _ := textBlockOf(t, database, spec.id, "paragraph", renderAtVersionPatchSecond)
	facts, flat, versioned := renderAtVersionTextFacts(t, database, second), artifactTextOf(t, handler, spec.id), renderAtVersionNewest(t, database, spec.id)
	_, tag = placementOf(t, database, refs.BlockNodeKind, second)
	if moved := patchNode(t, handler, key, second, tag, map[string]any{"parent": first}, "alice"); moved.Code != http.StatusOK {
		t.Fatalf("the move of the paragraph %s under the paragraph %s answered %d %s, want 200", second, first, moved.Code, moved.Body.String())
	}
	renderAtVersionQuiet(t, database, key, issue.PrimaryArtifactID)
	if placed, _ := placementOf(t, database, refs.BlockNodeKind, second); placed.ToID != first {
		t.Errorf("the move of the paragraph %s under the paragraph %s left it under %s %s", second, first, placed.ToKind, placed.ToID)
	}
	if after := artifactTextOf(t, handler, spec.id); after != flat {
		t.Errorf("the move of the paragraph %s under the paragraph %s changed the head of %s from %q to %q, want its markdown flat as it stood", second, first, spec.refKey, flat, after)
	}
	if after := renderAtVersionNewest(t, database, spec.id); after != versioned {
		t.Errorf("the move of the paragraph %s under the paragraph %s wrote version %d of %s, want none: its markdown stays flat", second, first, after, spec.refKey)
	}
	if after := renderAtVersionTextFacts(t, database, second); after != facts {
		t.Errorf("the move of the paragraph %s under the paragraph %s changed its text facts from %s to %s, want them unchanged", second, first, facts, after)
	}
}
