package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// owner identifies the issue, unlinked artifact, or project that owns collaboration.
type owner struct {
	IssueKey   *string
	ArtifactID *string
	ProjectKey *string
}

func issueOwner(key string) owner {
	return owner{IssueKey: new(key)}
}

func documentOwner(artifactID string) owner {
	return owner{ArtifactID: new(artifactID)}
}

func projectOwner(key string) owner {
	return owner{ProjectKey: new(key)}
}

func ownerOf(issueKey, artifactID *string) owner {
	return owner{IssueKey: issueKey, ArtifactID: artifactID}
}

func (o owner) event(eventType string, actor model.Actor, payload any) model.Event {
	return model.Event{
		IssueKey: o.IssueKey, ArtifactID: o.ArtifactID, ProjectKey: o.ProjectKey,
		Type: eventType, Actor: actor, Payload: payload,
	}
}

func ownerForArtifact(artifact model.Artifact) owner {
	if artifact.IssueKey != nil {
		return issueOwner(*artifact.IssueKey)
	}
	return documentOwner(artifact.ID)
}

func (s *server) documentOwnerFromRequest(ctx context.Context, q queryer, who model.Actor, r *http.Request) (model.Artifact, owner, error) {
	artifact, err := s.loadArtifact(ctx, q, who, r.PathValue("id"))
	if err != nil {
		return model.Artifact{}, owner{}, err
	}
	if artifact.IssueKey != nil {
		return model.Artifact{}, owner{}, errorf(
			http.StatusBadRequest,
			"ARTIFACT_LINKED",
			"artifact belongs to issue %s; use /api/v1/issues/%s/...",
			*artifact.IssueKey,
			*artifact.IssueKey,
		)
	}
	if artifact.SessionID != nil {
		return model.Artifact{}, owner{}, errorf(
			http.StatusBadRequest,
			"ARTIFACT_AGENT_OWNED",
			"artifact belongs to agent %s's conversation, which holds files and images only: it has no comments, asks, events or subscribers, which belong to an issue's or a project's documents",
			*artifact.SessionID,
		)
	}
	return artifact, documentOwner(artifact.ID), nil
}

// loadProject reads the project row, every column model.Project carries but the open-ask count,
// which only the project list reads. A project.updated event carries what this answers, so its
// payload is the whole row a reader of the project list sees. An unknown key is 404
// PROJECT_NOT_FOUND.
func (s *server) loadProject(ctx context.Context, q queryer, who model.Actor, key string) (model.Project, error) {
	var project model.Project
	if err := q.QueryRow(ctx, `
		select key, name, gated_steps, default_repo, proven_sessions, created_at from projects where key = $1
	`, key).Scan(
		&project.Key,
		&project.Name,
		&project.GatedSteps,
		&project.DefaultRepo,
		&project.ProvenSessions,
		&project.CreatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.Project{}, errorf(http.StatusNotFound, "PROJECT_NOT_FOUND", "project not found")
		}
		return model.Project{}, err
	}
	return project, nil
}

// projectSeenBy reports whether who sees project key (access.ProjectClause): a stored answer of a
// request key replayed to a caller is held to it (request_keys.go). A reader that sees every
// object, as every open viewer of a deployment with no private project does, sees every project and
// reads no statement for it (R9).
func projectSeenBy(ctx context.Context, q queryer, who model.Actor, key string) (bool, error) {
	if seesEveryObject(who) {
		return true, nil
	}
	var seen bool
	err := q.QueryRow(ctx, "select exists (select 1 from projects where key = $1 and "+
		access.ProjectClause("key", "$2")+")", key, refs.VisibilityArg(who)).Scan(&seen)
	return seen, err
}

// requireOpenOwner locks the owner before its collaboration rows are changed, as who reads it.
func (s *server) requireOpenOwner(ctx context.Context, tx pgx.Tx, who model.Actor, owner owner) error {
	_, err := s.requireOpenOwnerStatus(ctx, tx, who, owner)
	return err
}

// requireOpenOwnerStatus also returns the lifecycle status for an issue owner.
// Project documents have no issue status and return nil. It takes the owner's lock first and
// reads the owner in the next statement, never in the same one: a statement's snapshot is taken
// before it waits, so a read combined with the lock would miss a close committed while it waited
// (store.LockOwner), and the write it guards would land on a closed owner. The two go in one round
// trip (store.LockOwnerAndRead).
func (s *server) requireOpenOwnerStatus(ctx context.Context, tx pgx.Tx, who model.Actor, owner owner) (*string, error) {
	status, open, err := ownerStatus(ctx, who, owner, lockedOwnerRead(tx))
	if err != nil {
		return nil, err
	}
	if err := refuseClosed(open, refusedWhenClosed); err != nil {
		return nil, err
	}
	return status, nil
}

func ownerInvalid() error {
	return errorf(http.StatusBadRequest, "OWNER_INVALID", "owner requires exactly one issue or artifact")
}

// ownerRead makes the read an owner check decides on: the statement, the arguments it takes and
// the destinations its one row is scanned into, for the owner named. The check that guards a
// write takes that owner's lock ahead of it (lockedOwnerRead); the one that only spares work
// takes no lock (pooledOwnerRead).
type ownerRead func(ctx context.Context, owner store.Owner, sql string, args []any, dest ...any) error

// lockedOwnerRead reads under the owner's lock in tx: the lock first and the read in the next
// statement, never in the same one, the two in one round trip (store.LockOwnerAndRead).
func lockedOwnerRead(tx pgx.Tx) ownerRead {
	return func(ctx context.Context, owner store.Owner, sql string, args []any, dest ...any) error {
		return store.LockOwnerAndRead(ctx, tx, owner, sql, args, dest...)
	}
}

// pooledOwnerRead reads through q, the pool, for a check made before any transaction opens. It
// locks nothing, so a write on a busy owner waits for that lock once rather than twice.
func pooledOwnerRead(q queryer) ownerRead {
	return func(ctx context.Context, _ store.Owner, sql string, args []any, dest ...any) error {
		return q.QueryRow(ctx, sql, args...).Scan(dest...)
	}
}

// ownerStatus is the refusal table an owner gets, spelled once for both the check that decides
// and the one that only spares work: an owner naming neither an issue nor a document, or a
// document that does not exist. It answers an issue owner's status and whether that issue is
// open - a project document is always open - and leaves refusing a closed issue to each write
// that does. The check that decides reads under the owner's lock (lockedOwnerRead), the one made
// before any transaction opens reads through the pool (pooledOwnerRead); the two differ in read
// alone, so both refuse exactly the same.
//
// The owner is read as who reads it - the writer, with the viewer of its request (readerOf) - so
// an owner of a project the writer does not see is the missing owner, and the write answers it
// as it answers an owner no row holds.
func ownerStatus(ctx context.Context, who model.Actor, owner owner, read ownerRead) (*string, bool, error) {
	if (owner.IssueKey == nil) == (owner.ArtifactID == nil) {
		return nil, false, ownerInvalid()
	}
	if owner.IssueKey != nil {
		status, open, err := issueStatus(ctx, who, *owner.IssueKey, read)
		if err != nil {
			return nil, false, err
		}
		return &status, open, nil
	}
	if err := refuseMissingArtifact(read(ctx, store.ArtifactOwner(*owner.ArtifactID), unlinkedArtifactRead,
		[]any{*owner.ArtifactID, refs.VisibilityArg(who)}, new(bool))); err != nil {
		return nil, false, err
	}
	return nil, true, nil
}

// unlinkedArtifactRead reads a row for a document no issue owns, as the viewer $2 binds reads it
// (access.DocumentClause): a document of a project that viewer does not see is no row, as an id
// no document holds is.
var unlinkedArtifactRead = `select true from artifacts where id = $1 and issue_key is null and ` +
	access.DocumentClause("artifacts", "$2")

// refuseMissingArtifact is the refusal of unlinkedArtifactRead's answer err: a document that does
// not exist is ARTIFACT_NOT_FOUND, any other failure err itself.
func refuseMissingArtifact(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return errorf(http.StatusNotFound, "ARTIFACT_NOT_FOUND", "artifact not found")
	}
	return err
}

// refuseOwnerBeforeResolving answers, from the pool and locking nothing, the refusal the locked
// owner check will make (lockCommentOwner, requireOpenOwnerStatus). A comment that mentions
// anyone resolves every mention against the Envoy listener before its transaction opens, and an
// owner that does not exist - or, for a write a closed issue refuses, is already closed - refuses
// the write whatever the listener says, so a request that cannot be written costs no listener
// call at all; an upload to an issue is refused so before its body is spooled
// (refuseUploadToAClosedIssue). The locked check still decides; this one only spares the work,
// and takes no lock of its own so a write on a busy owner waits for that lock once rather than
// twice.
func (s *server) refuseOwnerBeforeResolving(ctx context.Context, who model.Actor, owner owner, write closedWrite) error {
	if s.deps.Store == nil || s.deps.Store.Pool == nil {
		return errorf(http.StatusServiceUnavailable, "DATABASE_UNAVAILABLE", "database unavailable")
	}
	_, open, err := ownerStatus(ctx, who, owner, pooledOwnerRead(s.deps.Store.Pool))
	if err != nil {
		return err
	}
	return refuseClosed(open, write)
}
