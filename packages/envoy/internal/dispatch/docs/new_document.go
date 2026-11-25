package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
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
// (Ledger.ReadBackOf, nil when the write left none), what the version moves in the reference
// graph, and what its block index stated in the node and tuple store (Blocks). The index runs
// before the event that records the version exists, and no tuple may be updated afterwards, so
// the caller appends Blocks under that event, in the same transaction (graphstore.QueueCaused),
// as every other writer of a version does (VersionResult.Blocks).
//
// OwesSettlement is whether the write left the document owing a settlement, which its caller
// schedules once the transaction has committed (Service.ScheduleSettlement). Every write does but
// the seed of a document CreateDocument creates whose tree a settlement has nothing to write into
// (seedOwesSettlement), which records nothing owed (SeedText).
type WrittenText struct {
	Markdown       string
	Version        model.Version
	ReadBack       *pmdoc.Node
	Changes        model.ReferenceChanges
	Blocks         graphstore.Uncaused
	OwesSettlement bool
}

// WriteDocumentText writes markdown as the whole text of the document artifactID and queues it as
// the document's next version (Ledger.QueueVersion, which numbers it), crediting actor alone,
// with summary naming the version (nil for an unnamed one). created says the transaction created
// the document, which holds no text yet, so its text is seeded (SeedText); otherwise the text
// replaces the document's (ReplaceText), which holds the replacement to precondition, the token
// of the version the caller believes it replaces: nil for a new document, and for a replacement
// that names no token, which ReplaceText takes unchecked (the upload route counts those,
// INFRA-4980). It then records the version over the write (Ledger.WroteVersion), which clears the
// credit of every edit the write read when it changed the document, queues the count of the
// document's task items after the version's insert, so the count names that version (LEGION-542),
// and indexes the version's references. ctx is the context Join returned: every read runs in the
// transaction it joined, and every write is queued on its ledger. It answers whether the write
// left the document owing a settlement (WrittenText.OwesSettlement): a replacement does, and so
// does a seed, unless CreateDocument made it and its tree needs no settlement (SeedText).
//
// It appends no event, and so states none of Blocks: an upload appends its own event, after the
// approval moves a replacement makes, and the eval registry appends the creation's, and each
// appends Blocks under it.
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
	changes, blocks, err := documents.IndexDocumentReferences(ctx, artifactID, version.Number, stored)
	if err != nil {
		return WrittenText{}, err
	}
	owes := !created || ledger.seedOwes(artifactID)
	return WrittenText{Markdown: stored, Version: version, ReadBack: readBack, Changes: changes, Blocks: blocks, OwesSettlement: owes}, nil
}

// CreateDocument creates a markdown document with no request behind it: it queues row's insert
// (whose Kind it sets to doc) and writes markdown as the document's first version, as an upload
// of a new document does (QueueNewArtifact, then WriteDocumentText). ctx is the context Join
// returned, and the caller queues the document's `artifact.created` event
// (ArtifactCreatedEventPayload) after them, commits through the ledger and then schedules the
// document's settlement where the answer says the document owes one (WrittenText.OwesSettlement).
//
// The document's seed owes a settlement only where its tree needs one (withVersionedSeed,
// seedOwesSettlement). Its first version is the one written here, at the head the seed leaves, so
// a settlement of a tree it has nothing to write into would store nothing but the delete of the
// pending-settlement row the seed left, and would load the document's room to do it, a room that
// then stays resident until the idle sweep takes it: on every project create, whose `evals-md`
// this creates (internal/dispatch/evals). Such a seed records nothing owed (SeedText), so no
// settlement runs for it: none is scheduled, and neither the resumption, nor a load of the room,
// nor Shutdown finds one owed.
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
	written, err := WriteDocumentText(withVersionedSeed(ctx), documents, artifact.ID, markdown, row.CreatedBy, nil, true, nil)
	if err != nil {
		return model.Artifact{}, WrittenText{}, err
	}
	artifact.Versions = []model.Version{written.Version}
	return artifact, written, nil
}

// versionedSeedContextKey marks a seed whose first version its caller writes at the seed's head
// (withVersionedSeed).
type versionedSeedContextKey struct{}

// withVersionedSeed marks the seed made with ctx (SeedText) as CreateDocument's: its first version
// is queued in the seed's own transaction, at the head the seed leaves (WriteDocumentText), so a
// settlement of it writes no version, and the seed owes one only where its tree needs one
// (seedOwesSettlement). A seed made without the mark owes a settlement, as every seed did before
// the mark: a test's, which no caller versions, and a new issue's spec and an upload's (api's
// createIssue and storeArtifact), which schedule the settlement of every document they write.
func withVersionedSeed(ctx context.Context) context.Context {
	return context.WithValue(ctx, versionedSeedContextKey{}, true)
}

// isVersionedSeed is whether ctx marks its seed as CreateDocument's (withVersionedSeed).
func isVersionedSeed(ctx context.Context) bool {
	versioned, _ := ctx.Value(versionedSeedContextKey{}).(bool)
	return versioned
}

// seedOwesSettlement is whether a seed of tree that its caller versions at the seed's head
// (withVersionedSeed) still owes its document a settlement: whether a settlement could write
// anything into the tree. It errs toward owing, reading the tree as each stage of a settlement
// that writes reads it: a block lacking an id, or repeating one, which the stamp gives a new one
// (pmdoc.BlockIDRepairCount, applyStamp; the parse gives every block of a written tree an id, so
// none should); any ask block, with an id or without, which the reconciliation indexes, repairs
// or flags (reconcileAskBlocks); and any check block carrying `ci_check`, which it holds to the
// four rules (reconcileCICheckBlocks), though the seed's own refusal has held it to them already
// (refuseCICheckBlocks). The rest of a settlement writes nothing for such a seed: the version is
// the caller's, at the seed's head, so no content follows it (contentChangedSinceVersion); the
// parse took out every anchor mark (asUploaded), so the sweep finds none (sweepUnrecordedMarks);
// and a new document has no ask row, pending author or check result to reconcile.
func seedOwesSettlement(tree *pmdoc.Node) bool {
	return pmdoc.BlockIDRepairCount(tree) > 0 || pmdoc.HoldsAskBlock(tree) ||
		slices.ContainsFunc(checkBlockNodes(tree), func(node *pmdoc.Node) bool {
			return CarriesCICheck(CheckAttributesOf(node))
		})
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
