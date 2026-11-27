package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// renameArtifactInput is the body of PATCH /api/v1/artifacts/{id} and of PATCH
// /api/v2/artifacts/{id} (renameArtifactV2).
type renameArtifactInput struct {
	Name string `json:"name" required:"true"`
}

// renameArtifact corrects a document's title. It writes artifacts.name and nothing else: the
// slug, the reference key that slug gives and the role the document plays are its address and
// its part, which the first title fixed. A correction moves none of them, so every
// `dispatch://KEY/artifact/<slug>` reference, every anchored comment and the issue's plan or
// Verification document still name the same row. It needs the person scope (its route's OAuthScopes)
// because a title is what every reader of the issue sees the document by, so only a credential
// acting as its person corrects one.
//
// The owner's lock is taken before the owner's names are read, as an upload's is
// (storeArtifact). Two corrections of one owner at once would otherwise each miss the other and
// leave two documents that one name finds. The name write and the artifact.renamed event commit
// in one transaction, so no failure leaves a corrected title without its event.
func (s *server) renameArtifact(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	var input renameArtifactInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	renamed, ok := s.renameArtifactTo(w, r, actor, input)
	if !ok {
		return
	}
	WriteJSON(w, http.StatusOK, renamed)
}

// renameArtifactTo corrects the title of the document the path names as input asks, for actor,
// once the route has read its body, and answers the document as it now is, with its versions and
// its approval: the work of PATCH /api/v1/artifacts/{id} (renameArtifact) and PATCH
// /api/v2/artifacts/{id} (renameArtifactV2), each of which answers it in its own shape. It writes
// a refusal itself and answers false.
func (s *server) renameArtifactTo(w http.ResponseWriter, r *http.Request, actor model.Actor, input renameArtifactInput) (model.Artifact, bool) {
	// The name is trimmed of spaces before every rule, as an upload's name is
	// (jsonArtifactUpload), so the rules read what would be stored.
	name := strings.TrimSpace(input.Name)
	tx, artifact, target, ok := s.beginArtifactWrite(w, r)
	if !ok {
		return model.Artifact{}, false
	}
	defer tx.Rollback(r.Context())
	if artifact.Kind != "doc" {
		s.writeHandlerError(w, r, artifactIsNoDocumentRefusal(artifact))
		return model.Artifact{}, false
	}
	if isSpecDocument(artifact) {
		s.writeHandlerError(w, r, specNameRefusal())
		return model.Artifact{}, false
	}
	if name == "" {
		writeError(w, r, apicode.InvalidArtifact, "artifact name is required")
		return model.Artifact{}, false
	}
	if refusal := newArtifactNameRefusal(name); refusal != nil {
		s.writeHandlerError(w, r, refusal)
		return model.Artifact{}, false
	}
	// The owner's names are read under its lock. The document the name or the slug finds is a
	// refusal, unless it is this document itself: correcting the letter case of a title finds
	// the row being corrected and goes on.
	held, err := lookupArtifactByNameOrSlug(r.Context(), tx, target, name)
	switch {
	case err == nil && held.ID != artifact.ID:
		s.writeHandlerError(w, r, artifactNameTakenRefusal(name, held))
		return model.Artifact{}, false
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		s.writeHandlerError(w, r, err)
		return model.Artifact{}, false
	}
	// The slug the new name would give decides, as it decides the role a new document takes
	// (documentRole), so every spelling that gives a role's slug reads the same refusal. The
	// document that already plays that role keeps it: the issue's plan document is corrected to
	// `plan.md` as any other title is.
	slug := model.ArtifactSlug(name)
	if role := documentRole(target, artifact.Kind, slug); role != nil && !documentPlays(artifact, *role) {
		s.writeHandlerError(w, r, renameRoleNameRefusal(name, slug, *role))
		return model.Artifact{}, false
	}
	renamed, err := scanArtifact(tx.QueryRow(r.Context(), `
		update artifacts set name = $2 where id = $1
		returning `+artifactColumns, artifact.ID, name))
	if err != nil {
		s.writeHandlerError(w, r, err)
		return model.Artifact{}, false
	}
	// The row as it now is, with the versions and the approval every artifact read carries, is
	// what GET /api/v1/artifacts/{id} answers and what PATCH /api/v1/artifacts/{id} answers too;
	// PATCH /api/v2/artifacts/{id} answers it as its artifact row (renameArtifactV2).
	if err := loadArtifactDetails(r.Context(), tx, []*model.Artifact{&renamed}); err != nil {
		s.writeHandlerError(w, r, err)
		return model.Artifact{}, false
	}
	event, err := s.appendEvent(r.Context(), tx, ownerForArtifact(artifact).event(
		"artifact.renamed", actor, map[string]any{"artifact": renamed, "previous_name": artifact.Name},
	))
	if err != nil {
		s.writeHandlerError(w, r, err)
		return model.Artifact{}, false
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, r, err)
		return model.Artifact{}, false
	}
	s.publish(r.Context(), event)
	return renamed, true
}

// artifactNameTakenRefusal is the 409 refusing a correction to name, which already names
// artifact, another of this owner's artifacts, by its name or by its slug. A name reaches one
// document of an owner, so the correction would leave a reference naming two.
func artifactNameTakenRefusal(name string, artifact model.Artifact) error {
	return errorf(apicode.ArtifactNameTaken,
		"%q names artifact %s (%s) of this owner; a document's name is its own here: give it another name",
		name, artifact.Slug, artifact.Name)
}

// renameRoleNameRefusal is the 400 refusing a correction whose new name gives slug, the slug of
// one of the documents Dispatch makes itself (model.RoleDocumentSlugs), on a document that does
// not play that role. The slug is read, not the title, so every spelling that gives it reads this
// refusal.
func renameRoleNameRefusal(name, slug, role string) error {
	return errorf(apicode.ArtifactRoleName,
		"%q gives the slug %s, the slug of the issue's %s document, which this document is not; give it another name",
		name, slug, role)
}

// specNameRefusal is the 400 refusing a correction of the issue's primary document, its spec.
// The spec's name is part of how the spec is read and written whole, so it does not change.
func specNameRefusal() error {
	return errorf(apicode.ArtifactPrimary,
		"this document is the issue's spec, and its name is how the spec is written whole; the spec's name does not change")
}

// artifactIsNoDocumentRefusal is the 400 refusing a correction of an artifact that is no
// document: a file or an image is stored under the name it was uploaded with, and is replaced by
// another upload rather than renamed.
func artifactIsNoDocumentRefusal(artifact model.Artifact) error {
	return errorf(apicode.ArtifactKindMismatch,
		"artifact %s (%s) is a %s; only a document's name changes",
		artifact.Slug, artifact.Name, artifact.Kind)
}
