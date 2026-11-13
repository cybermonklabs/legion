package docs

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// This file is the write of a new artifact's row and of a markdown document's whole text, which
// an upload (api's storeArtifact) makes over a request and the eval registry
// (internal/dispatch/evals) makes with none: a project's `evals-md` document is created by the
// first eval write that needs it. Both packages call these functions, so a document either of
// them creates is stored by the same statements. They live here, beside the document service
// whose transaction they join, because api's eval routes call the registry, so the registry
// cannot import api.

// ArtifactColumns is the canonical artifact column list ScanArtifact reads. role is read through
// coalesce because model.Artifact carries it as "" when the document plays no role.
const ArtifactColumns = `id::text, issue_key, project_key, session_id, ref_key, slug, name, kind, is_primary, coalesce(role, ''), created_by, created_at`

// ScanArtifact reads ArtifactColumns. A session-owned artifact's project is "".
func ScanArtifact(row pgx.Row) (model.Artifact, error) {
	var artifact model.Artifact
	var project *string
	var createdBy []byte
	if err := row.Scan(
		&artifact.ID, &artifact.IssueKey, &project, &artifact.SessionID, &artifact.RefKey, &artifact.Slug,
		&artifact.Name, &artifact.Kind, &artifact.Primary, &artifact.Role, &createdBy, &artifact.CreatedAt,
	); err != nil {
		return model.Artifact{}, err
	}
	if project != nil {
		artifact.Project = *project
	}
	if err := json.Unmarshal(createdBy, &artifact.CreatedBy); err != nil {
		return model.Artifact{}, fmt.Errorf("decode artifact author: %w", err)
	}
	return artifact, nil
}

// NewArtifact is the row a write that creates an artifact inserts: its owner (an issue with its
// project, a project alone, or an agent's conversation alone), the slug the write took for it
// within that owner, its name, its kind (doc, image or file), the role its slug gives it (nil
// for none) and who created it. The row is never primary: an issue's spec is created with the
// issue, by the issue's own write.
type NewArtifact struct {
	IssueKey  *string
	Project   *string
	Session   *string
	Slug      string
	Name      string
	Kind      string
	Role      *string
	CreatedBy model.Actor
}

// LockProjectArtifacts serialises the creation of one project's own documents (not its issues')
// for the rest of tx, as api's lockAgentArtifacts does a conversation's. A project's upload
// otherwise locks no row before it looks its artifact up by name or slug and takes a new slug, so
// two uploads at once - `plan.md` and `plan-md` - could each miss the other and leave two
// documents that one reference names; the eval registry's create of `evals-md` takes the same
// lock, so it and an upload of `evals.md` cannot both create one. Only those writes take it. An
// upload takes it before any row or room, and the registry takes it after its own eval rows and
// before any artifact row or room, which no holder of this lock waits on, so it joins no lock
// cycle.
func LockProjectArtifacts(ctx context.Context, tx pgx.Tx, project string) error {
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtext('project-artifacts:' || $1))`, project); err != nil {
		return fmt.Errorf("lock the project's documents: %w", err)
	}
	return nil
}

// InsertArtifact inserts row in tx and answers the artifact it stored, read back through
// ArtifactColumns. The caller has taken the slug (no other artifact of the owner holds it) under
// the lock that serialises the owner's new artifacts.
func InsertArtifact(ctx context.Context, tx pgx.Tx, row NewArtifact) (model.Artifact, error) {
	createdBy, err := json.Marshal(row.CreatedBy)
	if err != nil {
		return model.Artifact{}, fmt.Errorf("encode JSON: %w", err)
	}
	return ScanArtifact(tx.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, session_id, slug, name, kind, is_primary, role, created_by)
		values ($1, $2, $3, $4, $5, $6, false, $7, $8)
		returning `+ArtifactColumns,
		row.IssueKey, row.Project, row.Session, row.Slug, row.Name, row.Kind, row.Role, createdBy))
}

// WrittenText is what WriteDocumentText stored: the document's markdown as the store renders it,
// the version that holds it, that rendering read back by its own parse (Ledger.ReadBackOf, nil
// when the write left none), and what the version moved in the reference graph.
type WrittenText struct {
	Markdown string
	Version  model.Version
	ReadBack *pmdoc.Node
	Changes  model.ReferenceChanges
}

// WriteDocumentText stores markdown as the whole text of the document artifactID and writes it as
// the document's version number, crediting actor alone, with summary naming the version (nil
// for an unnamed one). created says the transaction created the document, which holds no text
// yet, so its text is seeded (SeedText); otherwise the text replaces the document's
// (ReplaceText). It then records the version over the write (Ledger.WroteVersion), which clears
// the credit of every edit the write read, counts the document's task items against that
// version and indexes the version's references. ctx is the context Join returned, and every
// statement runs in the transaction it joined.
//
// It writes no event: an upload appends its own, after the approval moves a replacement makes,
// and the eval registry appends the creation's.
func WriteDocumentText(ctx context.Context, documents API, artifactID string, number int, markdown string, actor model.Actor, summary *string, created bool) (WrittenText, error) {
	ledger := ledgerFrom(ctx)
	tx, joined := txFromContext(ctx)
	if !joined {
		return WrittenText{}, errUnjoined
	}
	var stored string
	var err error
	if created {
		stored, err = documents.SeedText(ctx, artifactID, markdown, actor)
	} else {
		stored, err = documents.ReplaceText(ctx, artifactID, markdown, actor)
	}
	if err != nil {
		return WrittenText{}, err
	}
	version, err := WriteDocumentVersion(ctx, tx, artifactID, number, stored, []model.Actor{actor}, summary)
	if err != nil {
		return WrittenText{}, err
	}
	// The version credits the writer alone, and holds the write to the document, which clears the
	// credit of every edit that write read when it changed the document.
	ledger.WroteVersion(artifactID, version)
	readBack := ledger.ReadBackOf(artifactID, stored)
	// The version row exists now, so the count names it (LEGION-542).
	if err := RecordWrittenTaskProgress(ctx, tx, artifactID, readBack, stored); err != nil {
		return WrittenText{}, err
	}
	changes, err := documents.IndexDocumentReferences(ctx, artifactID, version.Number, stored)
	if err != nil {
		return WrittenText{}, err
	}
	return WrittenText{Markdown: stored, Version: version, ReadBack: readBack, Changes: changes}, nil
}

// CreateDocument creates a markdown document with no request behind it: it inserts row (whose
// Kind it sets to doc) and writes markdown as the document's first version, as an upload of a new
// document does (InsertArtifact, then WriteDocumentText). ctx is the context Join returned, and
// the caller appends the document's `artifact.created` event (ArtifactCreatedEventPayload),
// commits through the ledger and then schedules the document's settlement, as an upload does.
func CreateDocument(ctx context.Context, documents API, row NewArtifact, markdown string) (model.Artifact, WrittenText, error) {
	tx, joined := txFromContext(ctx)
	if !joined {
		return model.Artifact{}, WrittenText{}, errUnjoined
	}
	row.Kind = "doc"
	artifact, err := InsertArtifact(ctx, tx, row)
	if err != nil {
		return model.Artifact{}, WrittenText{}, err
	}
	written, err := WriteDocumentText(ctx, documents, artifact.ID, 1, markdown, row.CreatedBy, nil, true)
	if err != nil {
		return model.Artifact{}, WrittenText{}, err
	}
	artifact.Versions = []model.Version{written.Version}
	return artifact, written, nil
}

// WriteDocumentVersion stores the document-version row an upload writes directly, returning the
// same decoded version the upload route returns to its caller. Uploads keep this separate from
// writeVersionTx because they own their artifact event and approval move.
func WriteDocumentVersion(ctx context.Context, tx pgx.Tx, artifactID string, number int, markdown string, authors []model.Actor, summary *string) (model.Version, error) {
	encodedAuthors, err := json.Marshal(authors)
	if err != nil {
		return model.Version{}, fmt.Errorf("encode document version authors: %w", err)
	}
	var version model.Version
	var authorsRaw []byte
	if err := tx.QueryRow(ctx, `
		insert into artifact_versions as v (artifact_id, number, markdown, authors, named, summary, doc_update_version)
		values ($1, $2, $3, $4, $5, $6, coalesce((select max(version) from doc_updates where artifact_id = $1), 0))
		returning number, named, summary, authors, created_at, `+model.VersionSize+`
	`, artifactID, number, markdown, encodedAuthors, summary != nil, summary).Scan(
		&version.Number, &version.Named, &version.Summary, &authorsRaw, &version.CreatedAt, &version.Size,
	); err != nil {
		return model.Version{}, fmt.Errorf("write document version: %w", err)
	}
	if err := json.Unmarshal(authorsRaw, &version.Authors); err != nil {
		return model.Version{}, fmt.Errorf("decode document version authors: %w", err)
	}
	return version, nil
}

// RecordWrittenTaskProgress records the task count of the document a write just stored.
// readBack is the rendering that write stored, read back by its own parse (Ledger.ReadBackOf):
// the count reads that tree rather than parsing the same markdown again, and parses markdown
// itself when the write left none.
func RecordWrittenTaskProgress(ctx context.Context, tx pgx.Tx, artifactID string, readBack *pmdoc.Node, markdown string) error {
	if readBack != nil {
		return RecordTaskProgress(ctx, tx, artifactID, readBack)
	}
	return RecordTaskProgressMarkdown(ctx, tx, artifactID, markdown)
}

// ArtifactCreatedEventPayload is the payload of an `artifact.created` event. A version event is
// built by ArtifactVersionEventPayload wherever it is appended; a document's creation is the one
// other artifact payload, and it states what the write moved through the same helper, so both
// shapes spell the reference keys once.
func ArtifactCreatedEventPayload(artifact model.Artifact, changes model.ReferenceChanges) map[string]any {
	payload := map[string]any{"artifact": artifact}
	model.NameReferenceChanges(payload, changes)
	return payload
}
