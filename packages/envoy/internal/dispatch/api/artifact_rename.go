package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// renameArtifactInput is the body of PATCH /api/v1/artifacts/{id}.
type renameArtifactInput struct {
	Name string `json:"name" required:"true"`
}

// renameArtifact corrects a document's title. It writes artifacts.name and nothing else: the
// slug, the reference key that slug gives and the role the document plays are its address and
// its part, which the first title fixed. A correction moves none of them, so every
// `dispatch://KEY/artifact/<slug>` reference, every anchored comment and the issue's plan or
// Verification document still name the same row. It needs the person scope (routeScopes)
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
		s.writeHandlerError(w, err)
		return
	}
	// The name is trimmed of spaces before every rule, as an upload's name is
	// (jsonArtifactUpload), so the rules read what would be stored.
	name := strings.TrimSpace(input.Name)
	tx, artifact, target, ok := s.beginArtifactWrite(w, r)
	if !ok {
		return
	}
	defer tx.Rollback(r.Context())
	if artifact.Kind != "doc" {
		s.writeHandlerError(w, artifactIsNoDocumentRefusal(artifact))
		return
	}
	if isSpecDocument(artifact) {
		s.writeHandlerError(w, specNameRefusal())
		return
	}
	if name == "" {
		writeError(w, "INVALID_ARTIFACT", http.StatusBadRequest, "artifact name is required")
		return
	}
	if refusal := newArtifactNameRefusal(name); refusal != nil {
		s.writeHandlerError(w, refusal)
		return
	}
	// The owner's names are read under its lock. The document the name or the slug finds is a
	// refusal, unless it is this document itself: correcting the letter case of a title finds
	// the row being corrected and goes on.
	held, err := lookupArtifactByNameOrSlug(r.Context(), tx, target, name)
	switch {
	case err == nil && held.ID != artifact.ID:
		s.writeHandlerError(w, artifactNameTakenRefusal(name, held))
		return
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		s.writeHandlerError(w, err)
		return
	}
	// The slug the new name would give decides, as it decides the role a new document takes
	// (documentRole), so every spelling that gives a role's slug reads the same refusal. The
	// document that already plays that role keeps it: the issue's plan document is corrected to
	// `plan.md` as any other title is.
	slug := artifactSlug(name)
	if role := documentRole(target, artifact.Kind, slug); role != nil && !documentPlays(artifact, *role) {
		s.writeHandlerError(w, renameRoleNameRefusal(name, slug, *role))
		return
	}
	renamed, err := scanArtifact(tx.QueryRow(r.Context(), `
		update artifacts set name = $2 where id = $1
		returning `+artifactColumns, artifact.ID, name))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The row as it now is, with the versions and the approval every artifact read carries, is
	// what GET /api/v1/artifacts/{id} answers and what this route answers too.
	if err := loadArtifactDetails(r.Context(), tx, []*model.Artifact{&renamed}); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.appendEvent(r.Context(), tx, ownerForArtifact(artifact).event(
		"artifact.renamed", actor, map[string]any{"artifact": renamed, "previous_name": artifact.Name},
	))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(event)
	WriteJSON(w, http.StatusOK, renamed)
}

// artifactNameTakenRefusal is the 409 refusing a correction to name, which already names
// artifact, another of this owner's artifacts, by its name or by its slug. A name reaches one
// document of an owner, so the correction would leave a reference naming two.
func artifactNameTakenRefusal(name string, artifact model.Artifact) error {
	return errorf(http.StatusConflict, "ARTIFACT_NAME_TAKEN",
		"%q names artifact %s (%s) of this owner; a document's name is its own here: give it another name",
		name, artifact.Slug, artifact.Name)
}

// renameRoleNameRefusal is the 400 refusing a correction whose new name gives slug, the slug of
// one of the documents Dispatch makes itself (roleSlugs), on a document that does not play that
// role. The slug is read, not the title, so every spelling that gives it reads this refusal.
func renameRoleNameRefusal(name, slug, role string) error {
	return errorf(http.StatusBadRequest, "ARTIFACT_ROLE_NAME",
		"%q gives the slug %s, the slug of the issue's %s document, which this document is not; give it another name",
		name, slug, role)
}

// specNameRefusal is the 400 refusing a correction of the issue's primary document, its spec.
// The spec's name is part of how the spec is read and written whole, so it does not change.
func specNameRefusal() error {
	return errorf(http.StatusBadRequest, "ARTIFACT_PRIMARY",
		"this document is the issue's spec, and its name is how the spec is written whole; the spec's name does not change")
}

// artifactIsNoDocumentRefusal is the 400 refusing a correction of an artifact that is no
// document: a file or an image is stored under the name it was uploaded with, and is replaced by
// another upload rather than renamed.
func artifactIsNoDocumentRefusal(artifact model.Artifact) error {
	return errorf(http.StatusBadRequest, "ARTIFACT_KIND_MISMATCH",
		"artifact %s (%s) is a %s; only a document's name changes",
		artifact.Slug, artifact.Name, artifact.Kind)
}
