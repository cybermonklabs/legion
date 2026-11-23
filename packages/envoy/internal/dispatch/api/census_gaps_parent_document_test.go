package api

import (
	"context"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// censusGapsPublicReaders are the callers of verification v16 that read PUB and do not see PRIV:
// alice, carol's token that lists no project and the shared token.
var censusGapsPublicReaders = []privateCaller{callerAlice, callerCarolToken, callerSharedToken}

// parentDocumentSeed is verification v16's seed (private_seed_test.go) with a comment and an ask on
// a PUB issue whose stored anchor names a document of PRIV's issue - the row a release could keep
// after the document it names went to a project its reader does not see - and a twin of each whose
// anchor names a uuid no document holds.
type parentDocumentSeed struct {
	privateSeed
	// issue is the PUB issue the four items are on.
	issue string
	// document is PRIV's document, as the artifacts row holds it.
	document, documentSlug string
	// comment and ask name document; missingComment and missingAsk name a uuid no document holds.
	comment, ask, missingComment, missingAsk string
}

// seedParentDocuments writes the seed: each item through its route as carol, anchored on the PUB
// issue's own spec, and then, where no route writes it (an anchor must name one of its owner's
// documents, loadAnchorArtifact), the stored anchor's artifact_id rewritten.
func seedParentDocuments(t *testing.T) parentDocumentSeed {
	t.Helper()
	seed := parentDocumentSeed{privateSeed: seedPrivateProject(t)}
	seed.issue = decodeBody[struct {
		Key string `json:"key"`
	}](t, seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": seedPublic, "title": "Anchored work", "spec": "Ship the build tonight.", "force": true,
	}, http.StatusCreated)).Key
	uploaded := decodeBody[artifactUploadResponse](t, seed.mustRequest(t, callerCarol, http.MethodPost,
		"/api/v1/issues/"+seed.privateSeed.issue[seedPrivate]+"/artifacts",
		map[string]any{"name": "hidden-notes.md", "content": "# Hidden notes\n\nThe private plan.\n"}, http.StatusCreated))
	seed.document, seed.documentSlug = uploaded.Artifact.ID, uploaded.Artifact.Slug
	anchor := map[string]any{"artifact": "spec", "quote": "build"}
	comment := func() string {
		return createdID(t, seed.request(t, callerCarol, http.MethodPost, "/api/v1/issues/"+seed.issue+"/comments",
			map[string]any{"body": "Why tonight?", "anchor": anchor}))
	}
	ask := func() string {
		return createdID(t, seed.request(t, callerCarol, http.MethodPost, "/api/v1/issues/"+seed.issue+"/asks", map[string]any{
			"question": "Why tonight?", "options": []map[string]string{{"label": "Yes"}, {"label": "No"}}, "anchor": anchor,
		}))
	}
	seed.comment, seed.missingComment, seed.ask, seed.missingAsk = comment(), comment(), ask(), ask()
	nobody := uuid.NewString()
	for _, stored := range []struct{ table, id, document string }{
		{"comments", seed.comment, seed.document}, {"comments", seed.missingComment, nobody},
		{"asks", seed.ask, seed.document}, {"asks", seed.missingAsk, nobody},
	} {
		tag, err := seed.database.Pool.Exec(context.Background(),
			`update `+stored.table+` set anchor = jsonb_set(anchor, '{artifact_id}', to_jsonb($2::text)) where id = $1`, stored.id, stored.document)
		if err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("store the anchor of %s %s: %v (%d rows)", stored.table, stored.id, err, tag.RowsAffected())
		}
	}
	return seed
}

// parentRead is one read of an item's graph node: the node, and the body it came in.
type parentRead struct {
	node model.GraphNode
	body string
}

// reads are the reads that answer the graph node of item id, of kind, to caller: GET
// /api/v1/references?to= its reference, and, for a comment, GET /api/v1/comments/{id}/blocks,
// whose item is that node. Each is keyed by what it reads.
func (s parentDocumentSeed) reads(t *testing.T, caller privateCaller, kind, id string) map[string]parentRead {
	t.Helper()
	reference := "dispatch://" + s.issue + "/" + kind + "/" + id
	references := s.request(t, caller, http.MethodGet, "/api/v1/references?"+url.Values{"to": {reference}}.Encode(), nil)
	if references.Code != http.StatusOK {
		t.Fatalf("%s: ?to=%s = %d %s, want 200", caller, reference, references.Code, references.Body.String())
	}
	reads := map[string]parentRead{
		"?to= the " + kind: {node: decodeBody[model.GraphReferences](t, references).Node, body: references.Body.String()},
	}
	if kind == "comment" {
		blocks := s.request(t, caller, http.MethodGet, "/api/v1/comments/"+id+"/blocks", nil)
		if blocks.Code != http.StatusOK {
			t.Fatalf("%s: the blocks of comment %s = %d %s, want 200", caller, id, blocks.Code, blocks.Body.String())
		}
		reads["the comment's blocks"] = parentRead{node: decodeBody[model.ItemBlocks](t, blocks).Item, body: blocks.Body.String()}
	}
	return reads
}

// TestACommentOrAnAskNamesNoHiddenDocumentAsItsParent (R2-R4): a comment or an ask on a PUB issue
// whose stored anchor names a document of PRIV answers, to every caller that does not see PRIV,
// the parent an anchor naming a uuid no document holds answers - the issue, at no version - and
// names neither the document's id nor its slug, on GET /api/v1/references?to= and, for a comment,
// on GET /api/v1/comments/{id}/blocks. carol, who sees PRIV, reads PRIV's document as the parent,
// so the row reads a live anchor. With PRIV public, no project is private, and alice reads the
// document as the parent, as before (R9).
func TestACommentOrAnAskNamesNoHiddenDocumentAsItsParent(t *testing.T) {
	t.Parallel()
	seed := seedParentDocuments(t)
	parentIsTheDocument := func(t *testing.T, caller privateCaller, kind, id string) {
		t.Helper()
		for what, read := range seed.reads(t, caller, kind, id) {
			if read.node.Parent == nil || read.node.Parent.Kind != "artifact" || read.node.Parent.ID != seed.document {
				t.Errorf("%s: %s reads the parent %+v, want PRIV's document %s", caller, what, read.node.Parent, seed.document)
			}
		}
	}
	items := []struct{ kind, hidden, missing string }{
		{"comment", seed.comment, seed.missingComment},
		{"ask", seed.ask, seed.missingAsk},
	}
	t.Run("PRIV private", func(t *testing.T) {
		for _, item := range items {
			parentIsTheDocument(t, callerCarol, item.kind, item.hidden)
			for _, caller := range censusGapsPublicReaders {
				missing := seed.reads(t, caller, item.kind, item.missing)
				for what, read := range seed.reads(t, caller, item.kind, item.hidden) {
					if !reflect.DeepEqual(read.node.Parent, missing[what].node.Parent) || !reflect.DeepEqual(read.node.ParentVersion, missing[what].node.ParentVersion) {
						t.Errorf("%s: %s reads %s; an anchor naming no document reads %s",
							caller, what, factsOf(read.node), factsOf(missing[what].node))
					}
					if strings.Contains(read.body, seed.document) || strings.Contains(read.body, seed.documentSlug) {
						t.Errorf("%s: %s names PRIV's document %s (%s): %s", caller, what, seed.document, seed.documentSlug, read.body)
					}
				}
			}
		}
	})
	t.Run("PRIV public: the parent is read as before", func(t *testing.T) {
		seed.setVisibility(t, seedPrivate, model.ProjectPublic)
		for _, item := range items {
			parentIsTheDocument(t, callerAlice, item.kind, item.hidden)
		}
	})
}
