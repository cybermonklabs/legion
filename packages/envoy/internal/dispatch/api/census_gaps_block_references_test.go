package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestABlocksCountsNameNoHiddenCommentOrAsk (R2-R4): a comment and an ask of PRIV's issue whose
// stored anchor names a block of a PUB document - the row a release could keep after what owns
// them went to a project its reader does not see; no route writes it, since an anchor must name one
// of its owner's documents (loadAnchorArtifact) - count on GET /api/v1/artifacts/{id}/blocks, for
// every caller that does not see PRIV, as no row: each block answers the counts it answered before
// the two rows named it. carol, who sees PRIV, reads one comment and one ask on the block, so the
// row counts live anchors. With PRIV public, no project is private, and alice reads the two as
// before (R9).
func TestABlocksCountsNameNoHiddenCommentOrAsk(t *testing.T) {
	t.Parallel()
	seed := seedPrivateProject(t)
	document := decodeBody[artifactUploadResponse](t, seed.mustRequest(t, callerCarol, http.MethodPost,
		"/api/v1/projects/"+seedPublic+"/artifacts",
		map[string]any{"name": "Runbook.md", "content": "# Runbook\n\nThe runbook steps.\n"}, http.StatusCreated)).Artifact.ID
	counts := func(t *testing.T, caller privateCaller) map[string]model.BlockReferences {
		t.Helper()
		blocks := decodeBody[[]model.ArtifactBlock](t, seed.mustRequest(t, caller, http.MethodGet,
			"/api/v1/artifacts/"+document+"/blocks", nil, http.StatusOK))
		counted := make(map[string]model.BlockReferences, len(blocks))
		for _, block := range blocks {
			counted[block.ID] = block.References
		}
		return counted
	}
	before := map[privateCaller]map[string]model.BlockReferences{}
	for _, caller := range censusGapsPublicReaders {
		before[caller] = counts(t, caller)
	}
	var paragraph string
	for _, block := range decodeBody[[]model.ArtifactBlock](t, seed.mustRequest(t, callerCarol, http.MethodGet,
		"/api/v1/artifacts/"+document+"/blocks", nil, http.StatusOK)) {
		if block.Type == "paragraph" {
			paragraph = block.ID
		}
	}
	if paragraph == "" {
		t.Fatalf("the PUB document holds no paragraph block")
	}
	private := seed.issue[seedPrivate]
	comment := createdID(t, seed.request(t, callerCarol, http.MethodPost, "/api/v1/issues/"+private+"/comments",
		map[string]any{"body": "The private plan's step."}))
	ask := createdID(t, seed.request(t, callerCarol, http.MethodPost, "/api/v1/issues/"+private+"/asks", map[string]any{
		"question": "Which step?", "options": []map[string]string{{"label": "This"}, {"label": "That"}},
	}))
	for _, stored := range []struct{ table, id string }{{"comments", comment}, {"asks", ask}} {
		tag, err := seed.database.Pool.Exec(context.Background(),
			`update `+stored.table+` set anchor = jsonb_build_object('artifact_id', $2::text, 'block_id', $3::text) where id = $1`,
			stored.id, document, paragraph)
		if err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("store the anchor of %s %s: %v (%d rows)", stored.table, stored.id, err, tag.RowsAffected())
		}
	}
	named := model.BlockReferences{Comments: 1, Asks: 1}
	t.Run("PRIV private", func(t *testing.T) {
		if got := counts(t, callerCarol)[paragraph]; got != named {
			t.Fatalf("carol, who sees PRIV, reads the paragraph's counts %+v, want %+v: the row counts nothing", got, named)
		}
		for _, caller := range censusGapsPublicReaders {
			got := counts(t, caller)
			for block, want := range before[caller] {
				if got[block] != want {
					t.Errorf("%s: block %s counts %+v, want %+v, as before PRIV's comment and ask named it", caller, block, got[block], want)
				}
			}
		}
	})
	t.Run("PRIV public: the counts are read as before", func(t *testing.T) {
		seed.setVisibility(t, seedPrivate, model.ProjectPublic)
		if got := counts(t, callerAlice)[paragraph]; got != named {
			t.Errorf("alice, with no project private, reads the paragraph's counts %+v, want %+v", got, named)
		}
	})
}
