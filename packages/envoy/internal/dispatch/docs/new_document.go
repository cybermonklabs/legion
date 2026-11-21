package docs

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// This file is the write of a new artifact's row and of a markdown document's whole text, which
// an upload (api's storeArtifact) makes over a request and the eval registry
// (internal/dispatch/evals) makes with none: a project's `evals-md` document is created by the
// first eval write that needs it. Both packages call these functions, so a document either of
// them creates is stored by the same statements. They live here, beside the document service
// whose transaction they join, because api's eval routes call the registry, so the registry
// cannot import api. Each of them runs in the read phase of a transaction joined with the
// document service (Join) and queues its writes on that transaction's ledger, which its Commit
// sends as the write phase's one batch (Ledger).

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
// project, a project alone, or an agent's conversation alone), what the owner's artifacts'
// ref_key holds before `/<slug>` (RefPrefix: the issue's key, the project's, or the
// conversation's owner key), the slug the write took for it within that owner, its name, its
// kind (doc, image or file), the role its slug gives it (nil for none) and who created it. The
// row is never primary: an issue's spec is created with the issue, by the issue's own write.
type NewArtifact struct {
	IssueKey  *string
	Project   *string
	Session   *string
	RefPrefix string
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
// lock, so it and an upload of `evals.md` cannot both create one, and api's title correction and
// removal of a project document take it before they read the row again (beginArtifactWrite).
// Only those writes take it. Each takes it before any row, room or the document's owner lock,
// which comes after it in every taker, and the registry takes it after the eval's own lock
// (evals' lockEval), which no holder of this lock waits on, so it joins no lock cycle.
func LockProjectArtifacts(ctx context.Context, tx pgx.Tx, project string) error {
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtext('project-artifacts:' || $1))`, project); err != nil {
		return fmt.Errorf("lock the project's documents: %w", err)
	}
	return nil
}

// QueueNewArtifact queues, on ledger, the insert of row, an artifact the transaction creates, and
// returns the row as the insert will leave it. Its id, which the document's own writes name before
// the row is written, is taken here (gen_random_uuid, which assigns no transaction id), in the
// statement that reads its created_at, the column's default, the transaction's now(), which the
// insert writes explicitly; its ref_key is the one the 0071 trigger fills in, the owner's
// RefPrefix and the slug. The caller has taken the slug (no other artifact of the owner holds it)
// under the lock that serialises the owner's new artifacts, and row.Role is the part the new
// document plays, which its slug decided: the row the insert leaves carries it, so the reads this
// transaction makes of the artifact it just created see it as every later reader will.
func QueueNewArtifact(ctx context.Context, ledger *Ledger, row NewArtifact) (model.Artifact, error) {
	if ledger == nil || ledger.tx == nil {
		return model.Artifact{}, errUnjoined
	}
	createdBy, err := json.Marshal(row.CreatedBy)
	if err != nil {
		return model.Artifact{}, fmt.Errorf("encode JSON: %w", err)
	}
	artifact := model.Artifact{
		IssueKey: row.IssueKey, Project: stringOf(row.Project), SessionID: row.Session,
		RefKey: row.RefPrefix + "/" + row.Slug, Slug: row.Slug, Name: row.Name, Kind: row.Kind, Role: stringOf(row.Role),
	}
	if err := json.Unmarshal(createdBy, &artifact.CreatedBy); err != nil {
		return model.Artifact{}, fmt.Errorf("decode artifact author: %w", err)
	}
	if err := ledger.tx.QueryRow(ctx, `select gen_random_uuid()::text, now()`).Scan(&artifact.ID, &artifact.CreatedAt); err != nil {
		return model.Artifact{}, err
	}
	// The document work that follows runs before the insert below, so the address that insert
	// gives the row - its key, the issue holding it, none for a project's or a session's
	// document, and its own dispatch:// address - is recorded for the reads of this
	// transaction that need it: a block pointer of a new document is held to the document's key
	// and its issue (addressOfDocument), and text this transaction indexes that cites the new
	// document resolves to that row (refs.Pending). The issue already has its row, so the
	// address names no pending parent (refs.DocumentAddress).
	ledger.RegisterPendingArtifact(artifact.ID, refs.DocumentAddress{
		RefKey: artifact.RefKey, IssueKey: stringOf(row.IssueKey),
		Ref: refs.ArtifactRef(artifact.IssueKey, artifact.Project, artifact.SessionID, artifact.Slug, artifact.Kind, artifact.Primary),
	})
	return artifact, ledger.Queue(func(b *store.WriteBatch) error {
		b.Queue("", `
			insert into artifacts (id, issue_key, project_key, session_id, slug, name, kind, is_primary, role, created_by, created_at)
			values ($1, $2, $3, $4, $5, $6, $7, false, $8, $9, $10)
		`, artifact.ID, row.IssueKey, row.Project, row.Session, row.Slug, row.Name, row.Kind, row.Role, createdBy, artifact.CreatedAt)
		return nil
	})
}

// stringOf is the string a nullable column holds, or "".
func stringOf(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// WrittenText is what WriteDocumentText wrote: the document's markdown as the store renders it,
// the version that holds it as its insert will leave it, that rendering read back by its own parse
// (Ledger.ReadBackOf, nil when the write left none), and what the version moves in the reference
// graph.
type WrittenText struct {
	Markdown string
	Version  model.Version
	ReadBack *pmdoc.Node
	Changes  model.ReferenceChanges
}

// WriteDocumentText writes markdown as the whole text of the document artifactID and queues it as
// the document's next version (Ledger.QueueVersion, which numbers it), crediting actor alone,
// with summary naming the version (nil for an unnamed one). created says the transaction created
// the document, which holds no text yet, so its text is seeded (SeedText); otherwise the text
// replaces the document's (ReplaceText), which holds the replacement to precondition, the token
// of the version the caller believes it replaces (nil for a new document). It then records the version over the write
// (Ledger.WroteVersion), which clears the credit of every edit the write read when it changed the
// document, queues the count of the document's task items after the version's insert, so the
// count names that version (LEGION-542), and indexes the version's references. ctx is the context
// Join returned: every read runs in the transaction it joined, and every write is queued on its
// ledger.
//
// It appends no event: an upload appends its own, after the approval moves a replacement makes,
// and the eval registry appends the creation's.
func WriteDocumentText(ctx context.Context, documents API, artifactID, markdown string, actor model.Actor, summary *string, created bool, precondition *model.EditPrecondition) (WrittenText, error) {
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
		stored, err = documents.ReplaceText(ctx, artifactID, markdown, actor, precondition)
	}
	if err != nil {
		return WrittenText{}, err
	}
	version, err := ledger.QueueVersion(ctx, artifactID, stored, []model.Actor{actor}, summary)
	if err != nil {
		return WrittenText{}, err
	}
	// The version credits the writer alone, and holds the write to the document, which clears the
	// credit of every edit that write read when it changed the document. It carries the markdown
	// it stored, whose display tree the commit stores (Ledger.storeVersionTrees), since this write
	// queues the row itself.
	ledger.WroteVersion(artifactID, version, stored)
	readBack := ledger.ReadBackOf(artifactID, stored)
	if err := RecordWrittenTaskProgress(ctx, tx, artifactID, readBack, stored); err != nil {
		return WrittenText{}, err
	}
	changes, err := documents.IndexDocumentReferences(ctx, artifactID, version.Number, stored)
	if err != nil {
		return WrittenText{}, err
	}
	return WrittenText{Markdown: stored, Version: version, ReadBack: readBack, Changes: changes}, nil
}

// CreateDocument creates a markdown document with no request behind it: it queues row's insert
// (whose Kind it sets to doc) and writes markdown as the document's first version, as an upload
// of a new document does (QueueNewArtifact, then WriteDocumentText). ctx is the context Join
// returned, and the caller queues the document's `artifact.created` event
// (ArtifactCreatedEventPayload) after them, commits through the ledger and then schedules the
// document's settlement, as an upload does.
func CreateDocument(ctx context.Context, documents API, row NewArtifact, markdown string) (model.Artifact, WrittenText, error) {
	ledger := ledgerFrom(ctx)
	if _, joined := txFromContext(ctx); !joined {
		return model.Artifact{}, WrittenText{}, errUnjoined
	}
	row.Kind = "doc"
	artifact, err := QueueNewArtifact(ctx, ledger, row)
	if err != nil {
		return model.Artifact{}, WrittenText{}, err
	}
	written, err := WriteDocumentText(ctx, documents, artifact.ID, markdown, row.CreatedBy, nil, true, nil)
	if err != nil {
		return model.Artifact{}, WrittenText{}, err
	}
	artifact.Versions = []model.Version{written.Version}
	return artifact, written, nil
}

// RecordWrittenTaskProgress records the task count of the document a write just stored, queued on
// the ledger ctx carries after the write's own version. readBack is the rendering that write
// stored, read back by its own parse (Ledger.ReadBackOf): the count reads that tree rather than
// parsing the same markdown again, and parses markdown itself when the write left none.
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
