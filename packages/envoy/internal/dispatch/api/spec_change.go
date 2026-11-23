package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/specchange"
	"github.com/sjawhar/envoy/internal/dispatch/specstore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The approved spec of a repository is written by a person's approval of a change request (R22).
// A project names its spec sources (spec_sources); an issue holds a change request when its
// repository (Issue.ResolvedRepo) is an enabled spec source of its project (D1). An approval in a
// person's name that names such an issue's spec - her review on either review route, and her
// answer to its approval request, which both reach queueApprovalReviews - writes the request into
// the approved spec of that repository, in the approval's own transaction and after its reviews
// (applySpecChange, specstore.Apply), or is refused, writing nothing. A request for changes and the
// carry of an approval to a new version write nothing. An issue of a repository no project names a
// spec source keeps the approval it always had.

// specRequestStaleEvent is the event that tells the owner of a refused change request that the
// approved spec changed it after it was written (R22: "Dispatch tells its owner of the refusal").
const specRequestStaleEvent = "spec.request_stale"

// specApprovedWrittenEvent is the event an act that writes the approved spec of a repository
// appends on its issue, in its own transaction: a person's approval of a change request
// (applySpecChange) and the import of an approved level (importLevel). Every other issue of that
// repository reads its change request against the approved spec, so what such an act wrote moves
// their reads while their documents stand; this event is how a reader of those requests, the
// daemon's design gate, hears of it.
const specApprovedWrittenEvent = "spec.approved_written"

// specChangeRouteWords is what the routes that write a person's approval say of an approval of a
// change request (routes_table.go).
const specChangeRouteWords = "An approval of the spec of an issue whose repository is a spec source of its project writes the change request it covers - each statement, check, component and interface the issue's blocks add, change or remove against the approved version their base names - into that repository's approved spec in the same transaction (GET /api/v1/projects/{key}/spec/{owner}/{repo}/nodes/{id}), appends spec.approved_written on the issue {issue, repo, act, nodes} when it wrote a node, and is refused, writing nothing, with 409 SPEC_REQUEST_STALE (a node whose base is not the current approved version; spec.request_stale then tells the issue's owner, naming each node and its versions), SPEC_NODE_INVALID (a node the approved spec cannot hold), SPEC_CHECKS_NOT_INCLUDED (a check change whose request names no Verification document), SPEC_PARENT_MISSING (a statement under a parent neither the approved spec nor the request holds), SPEC_LOOP (a loop across parent and supports) or APPROVAL_REQUEST_REQUIRED (no open approval request, or a text that moved after the request named it). A request for changes writes nothing into it."

// specChangeRefusal is a refusal of the approved spec (specstore.Refusal), with the issue and the
// approval request it refused, which a stale refusal's event names once the approval's
// transaction has rolled back (recordStaleRequest).
type specChangeRefusal struct {
	refusal *specstore.Refusal
	issue   string
	ask     *string
}

func (r *specChangeRefusal) Error() string { return r.refusal.Error() }
func (r *specChangeRefusal) Unwrap() error { return r.refusal }

// specChangeError is the 409 body of a refusal of the approved spec: its code and words, each
// stale node of a stale request, and each fault of the others.
type specChangeError struct {
	Code       string                       `json:"code"`
	Error      string                       `json:"error"`
	Statements []model.SpecRequestStaleNode `json:"statements,omitempty"`
	Problems   []specChangeProblem          `json:"problems,omitempty"`
}

// specChangeProblem is one fault of a refused change request: the node, its rule and its words.
type specChangeProblem struct {
	Node   string `json:"node"`
	Rule   string `json:"rule"`
	Detail string `json:"detail"`
}

// specChangeBody is the 409 body of refusal.
func specChangeBody(refusal *specstore.Refusal) specChangeError {
	body := specChangeError{Code: refusal.Code, Error: refusal.Detail}
	for _, node := range refusal.Stale {
		body.Statements = append(body.Statements, model.SpecRequestStaleNode{ID: node.ID, Base: node.Base, Current: node.Current, Removed: node.RemovedNode})
	}
	for _, problem := range refusal.Problems {
		body.Problems = append(body.Problems, specChangeProblem{Node: problem.Node, Rule: problem.Rule, Detail: problem.Detail})
	}
	return body
}

// issueSpecSource is the repository of issue when it is an enabled spec source of the issue's
// project, and whether it is one.
func issueSpecSource(ctx context.Context, q queryer, issue model.Issue) (string, bool, error) {
	if issue.ResolvedRepo == nil || *issue.ResolvedRepo == "" {
		return "", false, nil
	}
	repo := strings.ToLower(strings.TrimSpace(*issue.ResolvedRepo))
	var held bool
	if err := q.QueryRow(ctx, `select exists(select 1 from spec_sources where project_key = $1 and repo = $2 and enabled)`,
		issue.Project, repo).Scan(&held); err != nil {
		return "", false, fmt.Errorf("read whether %s is a spec source of %s: %w", repo, issue.Project, err)
	}
	return repo, held, nil
}

// versionDigest is the digest a person's approval of version of the document artifactID covers.
func versionDigest(ctx context.Context, q queryer, artifactID string, version int) (string, error) {
	var digest *string
	if err := q.QueryRow(ctx, `select artifact_version_text_sha256($1::uuid, $2)`, artifactID, version).Scan(&digest); err != nil {
		return "", fmt.Errorf("read the digest of version %d of %s: %w", version, artifactID, err)
	}
	if digest == nil {
		return "", nil
	}
	return *digest, nil
}

// applySpecChange writes the change request an approval of documents covers into the approved
// spec (specstore.Apply), queued on ledger after what the approval queued before it, or refuses
// the approval. It covers an approval only where documents name the spec of an issue of a spec
// source; any other writes nothing here. A covered approval answers an open approval request
// (APPROVAL_REQUEST_REQUIRED otherwise) and counts only for the text that request names: a
// document whose text moved after the request handed it to the person (Version past
// RequestedVersion) is refused the same way. The request is read from the versions it names, as
// they are stored (specchange.FromVersions): the statements and components from the spec's, the
// checks from the Verification document's where the request names it, else from that document's
// latest settled version, so a request that changes a check its approval request leaves out is
// refused (specstore.RefusalChecksNotIncluded).
//
// An approval that writes a node also queues spec.approved_written on the issue, after the write,
// and answers that event, which Commit fills in for the caller to publish; one that writes nothing
// answers none.
func (s *server) applySpecChange(ctx context.Context, tx pgx.Tx, ledger *docs.Ledger, request *model.AskApproval,
	documents []model.AskApprovalDocument, actor model.Actor, askID *string) (*model.Event, error) {
	var spec, verification *model.Artifact
	var specVersion, verificationVersion int
	for _, document := range documents {
		artifact, err := artifactMetadata(ctx, tx, document.ArtifactID)
		if err != nil {
			return nil, err
		}
		switch {
		case isSpecDocument(artifact):
			spec, specVersion = &artifact, document.Version
		case artifact.Kind == "doc" && artifact.IssueKey != nil && documentPlays(artifact, model.ArtifactRoleVerification):
			verification, verificationVersion = &artifact, document.Version
		}
	}
	if spec == nil {
		return nil, nil
	}
	issue, err := s.loadIssue(ctx, tx, *spec.IssueKey)
	if err != nil {
		return nil, err
	}
	repo, held, err := issueSpecSource(ctx, tx, issue)
	if err != nil || !held {
		return nil, err
	}
	if request == nil {
		return nil, errorf(http.StatusConflict, "APPROVAL_REQUEST_REQUIRED",
			"%s holds a change request of %s's approved spec, so its approval answers an open approval request; open one on its spec and approve that", issue.Key, repo)
	}
	for _, document := range documents {
		if document.RequestedVersion != 0 && document.RequestedVersion != document.Version {
			return nil, errorf(http.StatusConflict, "APPROVAL_REQUEST_REQUIRED",
				"the approval request names version %d of %s, and the document moved to version %d after it: an approval counts only for the text its request names; open the request again on the new version",
				document.RequestedVersion, document.Name, document.Version)
		}
	}
	statements, err := specVersionText(ctx, tx, *spec, specVersion)
	if err != nil {
		return nil, err
	}
	var checks *specchange.VersionText
	switch {
	case verification != nil:
		text, err := specVersionText(ctx, tx, *verification, verificationVersion)
		if err != nil {
			return nil, err
		}
		checks = &text
	default:
		unnamed, held, err := s.verificationDocument(ctx, tx, issue.Key, "")
		if err != nil {
			return nil, err
		}
		if held {
			latest, err := settledVersionNumber(ctx, tx, unnamed.ID)
			if err != nil {
				return nil, err
			}
			if latest > 0 {
				text, err := specVersionText(ctx, tx, unnamed, latest)
				if err != nil {
					return nil, err
				}
				checks = &text
			}
		}
	}
	changeRequest, err := specchange.FromVersions(issue.Key, statements, checks)
	if err != nil {
		return nil, err
	}
	provenance := specstore.Provenance{Act: specstore.ActApproval, Issue: issue.Key, Ask: askID, Person: actor}
	if provenance.Spec, err = specDocumentOf(ctx, tx, *spec, specVersion); err != nil {
		return nil, err
	}
	if verification != nil {
		if provenance.Verification, err = specDocumentOf(ctx, tx, *verification, verificationVersion); err != nil {
			return nil, err
		}
	}
	applied, err := specstore.Apply(ctx, tx, specstore.Input{
		Project: issue.Project, Repo: repo, Issue: issue.Key, Request: changeRequest,
		ChecksIncluded: verification != nil, Provenance: provenance, Who: actor,
	}, func(step func(*store.WriteBatch) error) error { return ledger.Queue(step) })
	var refusal *specstore.Refusal
	if errors.As(err, &refusal) {
		return nil, &specChangeRefusal{refusal: refusal, issue: issue.Key, ask: askID}
	}
	if err != nil || len(applied.Written) == 0 {
		return nil, err
	}
	written := new(model.Event)
	event := approvedWrittenEvent(issue.Key, repo, specstore.ActApproval, actor, applied.Written)
	if err := ledger.Queue(func(b *store.WriteBatch) error {
		return s.deps.Events.QueueAppend(b, event, func(appended model.Event) { *written = appended })
	}); err != nil {
		return nil, err
	}
	return written, nil
}

// approvedWrittenEvent is the spec.approved_written event of act (specstore.ActApproval or
// ActImport) of the issue key, which wrote each of written into the approved spec of repo, by
// actor.
func approvedWrittenEvent(key, repo, act string, actor model.Actor, written []specstore.Written) model.Event {
	payload := model.SpecApprovedWrittenEventPayload{Issue: key, Repo: repo, Act: act, Nodes: make([]model.SpecApprovedWrittenNode, 0, len(written))}
	for _, fact := range written {
		payload.Nodes = append(payload.Nodes, model.SpecApprovedWrittenNode{ID: fact.ID, Kind: fact.Kind, Version: fact.Version, Removed: fact.Removed})
	}
	return issueOwner(key).event(specApprovedWrittenEvent, actor, payload)
}

// specVersionText is version of artifact as specchange reads a change request from it.
func specVersionText(ctx context.Context, q queryer, artifact model.Artifact, version int) (specchange.VersionText, error) {
	markdown, err := storedVersionText(ctx, q, artifact.ID, version)
	if err != nil {
		return specchange.VersionText{}, err
	}
	return specchange.VersionText{Document: artifact.RefKey, Version: version, Markdown: markdown}, nil
}

// specDocumentOf is version of artifact as an act of the approved spec names it, with its digest.
func specDocumentOf(ctx context.Context, q queryer, artifact model.Artifact, version int) (*specstore.Document, error) {
	digest, err := versionDigest(ctx, q, artifact.ID, version)
	if err != nil {
		return nil, err
	}
	return &specstore.Document{ArtifactID: artifact.ID, Name: artifact.Name, Version: version, Digest: digest}, nil
}

// recordStaleRequest tells the owner of a change request whose approval err refused as stale: it
// appends spec.request_stale on the request's issue, naming each stale node and the reason, in a
// transaction of its own, since the approval's own rolled back, and publishes it. Any other err
// records nothing. A failure to record is logged: the approval stays refused.
func (s *server) recordStaleRequest(ctx context.Context, err error, actor model.Actor) {
	var refusal *specChangeRefusal
	if !errors.As(err, &refusal) || refusal.refusal.Code != specstore.RefusalStale {
		return
	}
	payload := model.SpecRequestStaleEventPayload{Issue: refusal.issue, ApprovalRequest: refusal.ask, Reason: refusal.refusal.Detail}
	for _, node := range refusal.refusal.Stale {
		payload.Statements = append(payload.Statements, model.SpecRequestStaleNode{ID: node.ID, Base: node.Base, Current: node.Current, Removed: node.RemovedNode})
	}
	record := func() (model.Event, error) {
		tx, err := s.begin(ctx)
		if err != nil {
			return model.Event{}, err
		}
		defer tx.Rollback(ctx)
		event, err := s.appendEvent(ctx, tx, issueOwner(refusal.issue).event(specRequestStaleEvent, actor, payload))
		if err != nil {
			return model.Event{}, err
		}
		return event, tx.Commit(ctx)
	}
	event, recordErr := record()
	if recordErr != nil {
		slog.Error("dispatch: the owner of a stale change request was not told of its refusal", "issue", refusal.issue, "error", recordErr)
		return
	}
	s.publish(ctx, event)
}

// specNodeRead is the read of the history of one node of the approved spec, with the review row of
// each approval's provenance found by its approval request and its spec document.
func (s *server) specNodeRead(ctx context.Context, q queryer, project, repo string, history *specstore.History) (model.SpecNodeRead, error) {
	read := model.SpecNodeRead{Project: project, Repo: repo, ID: history.ID, Removed: history.Removed(),
		Versions: []model.SpecNodeVersion{}, Removals: []model.SpecNodeRemoval{}}
	for _, version := range history.Versions {
		block, err := version.Block()
		if err != nil {
			return model.SpecNodeRead{}, err
		}
		provenance, err := s.specProvenanceRead(ctx, q, version.Provenance)
		if err != nil {
			return model.SpecNodeRead{}, err
		}
		read.Kind = version.Kind
		read.Versions = append(read.Versions, model.SpecNodeVersion{Version: version.Version, At: version.At, Block: block, Provenance: provenance})
	}
	for _, removal := range history.Removals {
		provenance, err := s.specProvenanceRead(ctx, q, removal.Provenance)
		if err != nil {
			return model.SpecNodeRead{}, err
		}
		read.Removals = append(read.Removals, model.SpecNodeRemoval{Version: removal.Version, At: removal.At, Provenance: provenance})
	}
	return read, nil
}

// specProvenanceRead is provenance as a read answers it. An approval's review row is the person's
// approval of the spec its request and its spec document name, whose id the approval's write phase
// chose, so the read finds it by those two.
func (s *server) specProvenanceRead(ctx context.Context, q queryer, provenance specstore.Provenance) (model.SpecProvenance, error) {
	read := model.SpecProvenance{Act: provenance.Act, Issue: provenance.Issue, Ask: provenance.Ask, Review: provenance.Review,
		Person: provenance.Person, BroughtBackWith: provenance.BroughtBackWith,
		Spec: specDocumentRead(provenance.Spec), Verification: specDocumentRead(provenance.Verification), Notes: specDocumentRead(provenance.Notes)}
	if read.Review == nil && provenance.Ask != nil && provenance.Spec != nil {
		var review string
		err := q.QueryRow(ctx, `
			select id::text from artifact_reviews
			where ask_id = $1::uuid and artifact_id = $2::uuid and version = $3 and state = 'approved'
			order by `+docs.ReviewOrder+` limit 1
		`, *provenance.Ask, provenance.Spec.ArtifactID, provenance.Spec.Version).Scan(&review)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return model.SpecProvenance{}, fmt.Errorf("read the review of the approval answering %s: %w", *provenance.Ask, err)
		default:
			read.Review = &review
		}
	}
	return read, nil
}

func specDocumentRead(document *specstore.Document) *model.SpecDocumentRead {
	if document == nil {
		return nil
	}
	return &model.SpecDocumentRead{ArtifactID: document.ArtifactID, Name: document.Name, Version: document.Version, Digest: document.Digest,
		Ask: document.Ask, Review: document.Review}
}

// GET /api/v1/projects/{key}/spec/{owner}/{repo}/nodes/{id}
func (s *server) getSpecNode(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	project := r.PathValue("key")
	owner, name, err := sourcePathRepo(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	repo, id := owner+"/"+name, r.PathValue("id")
	pool, err := s.readyPool()
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	history, err := specstore.ReadNode(r.Context(), pool, project, repo, id)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if history == nil {
		writeError(w, "SPEC_NODE_NOT_FOUND", http.StatusNotFound, fmt.Sprintf("the approved spec of %s in %s holds no node %s", repo, project, id))
		return
	}
	read, err := s.specNodeRead(r.Context(), pool, project, repo, history)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, read)
}

// putSpecSourceInput is the body of PUT /api/v1/projects/{key}/spec-sources/{owner}/{repo}.
type putSpecSourceInput struct {
	Branch string       `json:"branch" required:"true"`
	Actor  *model.Actor `json:"actor"`
}

// specSourceColumns selects one spec_sources row in the order scanSpecSource reads it.
const specSourceColumns = `project_key, repo, branch, enabled, created_by, created_at, updated_at`

func scanSpecSource(row pgx.Row) (model.SpecSource, error) {
	var source model.SpecSource
	var createdBy []byte
	if err := row.Scan(&source.Project, &source.Repo, &source.Branch, &source.Enabled, &createdBy, &source.CreatedAt, &source.UpdatedAt); err != nil {
		return model.SpecSource{}, err
	}
	if err := json.Unmarshal(createdBy, &source.CreatedBy); err != nil {
		return model.SpecSource{}, fmt.Errorf("decode the writer of a spec source: %w", err)
	}
	return source, nil
}

// putSpecSource names the repository in the path a spec source of the project, or changes the
// branch of the one it is, and turns it on. Its issues hold change requests from then on.
func (s *server) putSpecSource(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	var input putSpecSourceInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	owner, name, err := sourcePathRepo(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	branch := strings.TrimSpace(input.Branch)
	if branch == "" {
		writeError(w, "SOURCE_INPUT", http.StatusBadRequest, "branch is required")
		return
	}
	actorJSON, err := encodeJSON(actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	if err := tx.QueryRow(r.Context(), "select key from projects where key = $1", key).Scan(new(string)); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	source, err := scanSpecSource(tx.QueryRow(r.Context(), `
		insert into spec_sources (project_key, repo, branch, enabled, created_by)
		values ($1, $2, $3, true, $4)
		on conflict (project_key, repo) do update set branch = excluded.branch, enabled = true, updated_at = now()
		returning `+specSourceColumns, key, owner+"/"+name, branch, actorJSON))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, source)
}

// listProjectSpecSources answers the project's spec sources by repository; [] when it names none,
// and 404 for a project that does not exist.
func (s *server) listProjectSpecSources(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	key := r.PathValue("key")
	pool, err := s.readyPool()
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := pool.QueryRow(r.Context(), "select key from projects where key = $1", key).Scan(new(string)); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	rows, err := pool.Query(r.Context(), `select `+specSourceColumns+` from spec_sources where project_key = $1 order by repo`, key)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	sources, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.SpecSource, error) { return scanSpecSource(row) })
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if sources == nil {
		sources = []model.SpecSource{}
	}
	WriteJSON(w, http.StatusOK, sources)
}

// deleteSpecSource removes the repository in the path from the project's spec sources. Its
// approved spec stays: an issue of it holds no change request from then on, and a later source of
// the same repository reads the same approved spec again.
func (s *server) deleteSpecSource(w http.ResponseWriter, r *http.Request) {
	var input actorInput
	if !s.decodeOptionalJSON(w, r, &input) {
		return
	}
	if _, ok := s.requireActor(w, r, input.Actor); !ok {
		return
	}
	owner, name, err := sourcePathRepo(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	pool, err := s.readyPool()
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	tag, err := pool.Exec(r.Context(), `delete from spec_sources where project_key = $1 and repo = $2`, r.PathValue("key"), owner+"/"+name)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, "SOURCE_NOT_FOUND", http.StatusNotFound, "no spec source from "+owner+"/"+name+" configured for "+r.PathValue("key"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
