package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/inboxwrite"
	"github.com/sjawhar/envoy/internal/dispatch/model"
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

func (s *server) documentOwnerFromRequest(ctx context.Context, q queryer, r *http.Request) (model.Artifact, owner, error) {
	artifact, err := s.loadArtifact(ctx, q, r.PathValue("id"))
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
// payload is the whole row a reader of the project list sees, and a stored list of gated steps is
// read without the retired `decisions` step here as the list reads it (scanListedProject). An
// unknown key is 404 PROJECT_NOT_FOUND (errProjectNotFound).
func (s *server) loadProject(ctx context.Context, q queryer, key string) (model.Project, error) {
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
			return model.Project{}, errProjectNotFound()
		}
		return model.Project{}, err
	}
	// A stored list may hold the retired `decisions` step until the decisions step rewrites it,
	// and the database's check still admits it.
	// kept for one release: check-narrowings
	project.GatedSteps = gatedStepsWithout(project.GatedSteps)
	return project, nil
}

// errProjectNotFound is the 404 PROJECT_NOT_FOUND of a project key that no project holds, which
// the project routes answer (loadProject). The going POST answers it too, to a caller whose
// credential names one project, for a private project that the credential does not name
// (createGoingRequest), so the two answers are one.
func errProjectNotFound() error {
	return errorf(http.StatusNotFound, "PROJECT_NOT_FOUND", "project not found")
}

// requireOpenOwner locks the owner before its collaboration rows are changed.
func (s *server) requireOpenOwner(ctx context.Context, tx pgx.Tx, owner owner) error {
	_, err := s.requireOpenOwnerStatus(ctx, tx, owner)
	return err
}

// requireOpenOwnerStatus also returns the lifecycle status for an issue owner.
// Project documents have no issue status and return nil. It takes the owner's lock first and
// reads the owner in the next statement, never in the same one: a statement's snapshot is taken
// before it waits, so a read combined with the lock would miss a close committed while it waited
// (store.LockOwner), and the write it guards would land on a closed owner. The two go in one round
// trip (store.LockOwnerAndRead).
func (s *server) requireOpenOwnerStatus(ctx context.Context, tx pgx.Tx, owner owner) (*string, error) {
	status, open, err := ownerStatus(ctx, owner, lockedOwnerRead(tx))
	if err != nil {
		return nil, err
	}
	if err := refuseClosed(open, refusedWhenClosed); err != nil {
		return nil, err
	}
	return status, nil
}

// requireOpenOwnerStatusHoldingDefault is requireOpenOwnerStatus for a write that opens an ask
// the one writer of its thread delivers (createAskFor, requestArtifactApproval). The owner's
// locked read also answers the project whose default assignee an ask on that owner falls through
// to - an issue's while nobody holds it, a project document's - by the rule inboxwrite.HoldDefault
// reads, and that project's owner lock is then taken shared (inboxwrite.HoldProjectDefault),
// after the owner's and before anything reads who the ask waits on. An owner whose asks fall
// through to no default takes no second lock, and the check costs the owner check's round trip
// alone.
func (s *server) requireOpenOwnerStatusHoldingDefault(ctx context.Context, tx pgx.Tx, owner owner) (*string, error) {
	if (owner.IssueKey == nil) == (owner.ArtifactID == nil) {
		return nil, ownerInvalid()
	}
	var status *string
	var project string
	open := true
	if owner.IssueKey != nil {
		status = new(string)
		if err := store.LockOwnerAndRead(ctx, tx, store.IssueOwner(*owner.IssueKey), issueStatusDefaultRead,
			[]any{*owner.IssueKey}, status, &open, &project); err != nil {
			return nil, err
		}
	} else if err := refuseMissingArtifact(store.LockOwnerAndRead(ctx, tx, store.ArtifactOwner(*owner.ArtifactID),
		unlinkedArtifactDefaultRead, []any{*owner.ArtifactID}, &project)); err != nil {
		return nil, err
	}
	if err := refuseClosed(open, refusedWhenClosed); err != nil {
		return nil, err
	}
	if err := inboxwrite.HoldProjectDefault(ctx, tx, project); err != nil {
		return nil, err
	}
	return status, nil
}

// issueStatusDefaultRead is issueStatusRead with the project an ask on the issue falls through
// to: the issue's project while nobody holds the issue, and none while somebody does.
const issueStatusDefaultRead = `select status, closed_at is null, case when assignee is null then coalesce(project_key, '') else '' end from issues where key = $1`

// unlinkedArtifactDefaultRead is unlinkedArtifactRead answering the document's project, whose
// default an ask on it falls through to.
const unlinkedArtifactDefaultRead = `select coalesce(project_key, '') from artifacts where id = $1 and issue_key is null`

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

// ownerStatus is the refusal table an owner gets, for both the check that decides and the one that
// only spares work: an owner naming neither an issue nor a document, or a document that does not
// exist. It answers an issue owner's status and whether that issue is open - a project document
// is always open - and leaves refusing a closed issue to each write that does. The check that
// decides reads under the owner's lock (lockedOwnerRead), the one made before any transaction
// opens reads through the pool (pooledOwnerRead); the two differ in read alone, so both refuse
// exactly the same. requireOpenOwnerStatusHoldingDefault refuses the same owners over two
// statements of its own, which also read the project an ask on the owner falls through to.
func ownerStatus(ctx context.Context, owner owner, read ownerRead) (*string, bool, error) {
	if (owner.IssueKey == nil) == (owner.ArtifactID == nil) {
		return nil, false, ownerInvalid()
	}
	if owner.IssueKey != nil {
		status, open, err := issueStatus(ctx, *owner.IssueKey, read)
		if err != nil {
			return nil, false, err
		}
		return &status, open, nil
	}
	if err := refuseMissingArtifact(read(ctx, store.ArtifactOwner(*owner.ArtifactID), unlinkedArtifactRead, []any{*owner.ArtifactID}, new(bool))); err != nil {
		return nil, false, err
	}
	return nil, true, nil
}

// unlinkedArtifactRead reads a row for a document no issue owns.
const unlinkedArtifactRead = `select true from artifacts where id = $1 and issue_key is null`

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
func (s *server) refuseOwnerBeforeResolving(ctx context.Context, owner owner, write closedWrite) error {
	if s.deps.Store == nil || s.deps.Store.Pool == nil {
		return errorf(http.StatusServiceUnavailable, "DATABASE_UNAVAILABLE", "database unavailable")
	}
	_, open, err := ownerStatus(ctx, owner, pooledOwnerRead(s.deps.Store.Pool))
	if err != nil {
		return err
	}
	return refuseClosed(open, write)
}
