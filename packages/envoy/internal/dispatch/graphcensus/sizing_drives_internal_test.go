package graphcensus

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/architecture"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/githubtest"
)

// The drives of the sites of the register of the store's writers that this package's own passes,
// the architecture importer, graphstore's pointer writer and the rebuild of the references hold,
// and that no drive before them runs (internalDrives).
const (
	versionBatchDrive          = "graphcensus/census_versions.go#writeVersionBatch"
	missingVersionsDrive       = "graphcensus/census_versions.go#Queue, a block-index drain's"
	issueLinkBatchDrive        = "graphcensus/census_issues.go#writeIssueLinkBatch"
	referenceBatchDrive        = "graphcensus/census_refs.go#writeReferenceBatch"
	anchorBatchDrive           = "graphcensus/census_anchors.go#writeAnchorBatch"
	askedOnBatchDrive          = "graphcensus/census_citations_asks.go#writeAskedOnBatch"
	keepSumBatchDrive          = "graphcensus/census_keeps.go#writeKeepSumBatch"
	approvesBatchDrive         = "graphcensus/census_approvals.go#writeApprovesBatch"
	resultBatchDrive           = "graphcensus/census_results.go#writeResultBatch"
	resultRetractionBatchDrive = "graphcensus/census_results.go#writeResultRetractionBatch"
	componentBatchDrive        = "graphcensus/census_components.go#writeComponentBatch"
	componentDependsBatchDrive = "graphcensus/census_components.go#writeComponentDependsBatch"
	memberBatchDrive           = "graphcensus/census_components.go#writeIssueComponentMemberBatch"
	retargetDrive              = "graphstore/reasoning.go#Retarget"
	rebuildDrive               = "refs/rebuild.go#replaceInTx"
	importDrive                = "architecture/importer.go#stateComponents"
	retireDrive                = "architecture/importer.go#RetireComponents"
)

// drivenProject is the project every row the passes' drives stand up belongs to, drivenIssue the
// issue each of those rows stands under, and drivenTarget the issue a link and a reference name.
// Both issues are this build's, each its node and a row naming it, so no pass but the one a drive
// runs reads a row of theirs.
const (
	drivenProject = "DRIVE"
	drivenIssue   = "DRIVE-1"
	drivenTarget  = "DRIVE-2"
)

// drivenAuthor is who wrote every row the drives stand up, as a build before the store wrote it:
// alice, whose person node the database holds before the first drive, so a drive's authored_by
// names her and appends no node of hers.
var drivenAuthor = model.Actor{Kind: "user", ID: "alice"}

// internalDrives are the drives of the register's sites the drives before them leave: the
// backfill's passes over what a build before the store wrote (passDrives), a retarget of a lens's
// has_kind and the rebuild of a document's references, over a database of their own, and the
// architecture import and a source's removal over another (archDrives).
func internalDrives(t *testing.T) []StoreDrive {
	t.Helper()
	return slices.Concat(passDrives(t), archDrives(t))
}

// drivenPass runs one pass of the backfill over database, as Backfill runs it, and fails unless
// the pass wrote a row and left none of its kind.
func drivenPass(database *store.Store, pass func(context.Context, *store.Store) (KindBackfill, error)) func(*testing.T) {
	return func(t *testing.T) {
		t.Helper()
		done, err := pass(context.Background(), database)
		if err != nil {
			t.Fatalf("run the backfill's %s pass: %v", done.Kind, err)
		}
		if done.Rows == 0 || !done.Complete {
			t.Fatalf("the backfill's %s pass reports %+v, want the rows its drive stood up written and none left", done.Kind, done)
		}
	}
}

// drivenTx runs write in a transaction of its own over database and commits it, failing the test
// on what names.
func drivenTx(t *testing.T, database *store.Store, what string, write func(pgx.Tx) error) {
	t.Helper()
	if err := pgx.BeginFunc(context.Background(), database.Pool, write); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// drivenDocument writes a document of drivenIssue named slug as this build's upload writes one:
// its row naming its node, the node in drivenProject and its part_of the issue. It answers the
// document's id and its address.
func drivenDocument(t *testing.T, database *store.Store, slug string) (id, refKey string) {
	t.Helper()
	ctx := context.Background()
	drivenTx(t, database, "write the document "+slug, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
			values ($1, $2, $3, $3, 'doc', '{"kind":"user","id":"alice"}')
			returning id::text, ref_key
		`, drivenIssue, drivenProject, slug).Scan(&id, &refKey); err != nil {
			return err
		}
		now := time.Now().UTC()
		if err := graphstore.Write(ctx, tx, []graphstore.Node{graphstore.ArtifactNode(refKey, drivenProject, "", drivenAuthor, now)},
			[]graphstore.Tuple{graphstore.ArtifactPartOf(refKey, drivenIssue, drivenAuthor, now)}); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `update artifacts set node_kind = $2, node_address = ref_key where id = $1::uuid`, id, graphstore.KindArtifact)
		return err
	})
	return id, refKey
}

// versionNodeCounted is what a version's node write appends (graphstore.VersionFacts): its base
// attributes, its part_of its document and its number and created_at values, each counted by
// term.
func versionNodeCounted(term string) map[StoredKind][]string {
	counted := map[StoredKind][]string{}
	for _, kind := range []string{graphstore.AttributeVisibility, graphstore.AttributeState, graphstore.AttributeProject,
		graphstore.TuplePartOf, graphstore.AttributeNumber, graphstore.AttributeCreatedAt} {
		counted[StoredKind{graphstore.KindVersion, kind}] = []string{term}
	}
	return counted
}

// resultKindsCounted is what a record of a result appends of the result's node
// (graphstore.ResultFacts): its base attributes, its values, its part_of and its result_of its
// check, and its authored_by, each counted by the term result.
func resultKindsCounted() map[StoredKind][]string {
	counted := map[StoredKind][]string{}
	for _, kind := range []string{graphstore.AttributeVisibility, graphstore.AttributeState, graphstore.AttributeProject,
		graphstore.AttributeVerdict, graphstore.AttributeSurface, graphstore.AttributeRun, graphstore.AttributeObserved,
		graphstore.AttributeCommit, graphstore.AttributeRole, graphstore.AttributeFailed, graphstore.AttributeSkipped,
		graphstore.AttributeExcluded, graphstore.AttributeNegativeControl, graphstore.AttributeCheckVersion,
		graphstore.AttributeCheckSHA256, graphstore.TuplePartOf, graphstore.TupleResultOf, graphstore.TupleAuthoredBy} {
		counted[StoredKind{graphstore.KindResult, kind}] = []string{"result"}
	}
	return counted
}

// VersionNodeCounted and ResultKindsCounted are versionNodeCounted and resultKindsCounted for this
// package's external test (imported_drives_test.go, sizing_drives_external_test.go).
var (
	VersionNodeCounted = versionNodeCounted
	ResultKindsCounted = resultKindsCounted
)

// passDrives are the drives of the backfill's passes this package holds, each over the rows a
// build before the store wrote and a drive's Ready stands up by SQL, and of the pointer and the
// rebuild writers, over one database of their own. That database holds project DRIVE, its two
// issues, each its node and a row naming it, and alice's person node. Each pass
// reads every row of its kind its marker does not mark, so the drives run in an order in which no
// pass reads a row a drive before it stood up: the rebuild, which reads every document, comes
// after every drive but the Keep pass's, whose document's pointer it would read as a reference, and
// none of the passes reads a row of another's table.
func passDrives(t *testing.T) []StoreDrive {
	t.Helper()
	ctx := context.Background()
	database := storetest.Open(t)
	drivenTx(t, database, "write project DRIVE, its two issues and alice's node", func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `insert into projects (key, name, default_assignee) values ($1, 'Drives', 'alice')`, drivenProject); err != nil {
			return err
		}
		now := time.Now().UTC()
		if err := graphstore.Write(ctx, tx, []graphstore.Node{
			graphstore.IssueNode(drivenIssue, drivenProject, drivenAuthor, now), graphstore.IssueNode(drivenTarget, drivenProject, drivenAuthor, now),
			graphstore.PersonNode(drivenAuthor.ID, drivenAuthor, now, graphstore.NoEvent),
		}, nil); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			insert into issues (key, project_key, number, title, created_by, rank, node_kind, node_address) values
				($1, $3, 1, 'What the drives stand under', '{"kind":"user","id":"alice"}', 'U', 'issue', $1),
				($2, $3, 2, 'What the drives name', '{"kind":"user","id":"alice"}', 'V', 'issue', $2)
		`, drivenIssue, drivenTarget, drivenProject)
		return err
	})
	var drained, result string
	return []StoreDrive{{
		// The version pass (backfillVersions) over the one version a build before the store wrote
		// of a document of DRIVE-1: the version's node and its facts, which the term version
		// counts.
		Name: versionBatchDrive, Database: database,
		Sites: []string{versionBatchDrive},
		Ready: func(t *testing.T) {
			document, _ := drivenDocument(t, database, "versioned-md")
			censusVersion(t, database, document, 1, "Written by a build before the store.", drivenAuthor)
		},
		Drive:  drivenPass(database, backfillVersions),
		Counts: versionNodeCounted(graphstore.KindVersion),
	}, {
		// A block-index pass's drain of a document's versions that hold no node, as docs'
		// indexRenderedVersion drains it: under the document's owner lock, the read of the
		// versions owed (PlanMissingVersionNodes) and the queued write of their nodes
		// (MissingVersionNodes.Queue) on the pass's write phase. The one version a build before
		// the store wrote gets its node and its facts, which the term version counts.
		Name: missingVersionsDrive, Database: database,
		Sites: []string{"graphcensus/census_versions.go#Queue"},
		Ready: func(t *testing.T) {
			drained, _ = drivenDocument(t, database, "drained-md")
			censusVersion(t, database, drained, 1, "Drained by a block-index pass.", drivenAuthor)
		},
		Drive: func(t *testing.T) {
			drivenTx(t, database, "drain the versions of drained-md that hold no node", func(tx pgx.Tx) error {
				if err := store.LockOwners(ctx, tx, store.IssueOwner(drivenIssue)); err != nil {
					return err
				}
				missing, whole, err := PlanMissingVersionNodes(ctx, tx, []string{drained}, nil, 0)
				if err != nil {
					return err
				}
				if !whole || len(missing.nodes) != 1 {
					return fmt.Errorf("the drain planned %d version node(s), whole %t, want drained-md's one", len(missing.nodes), whole)
				}
				return store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
					missing.Queue(b)
					return nil
				})
			})
		},
		Counts: versionNodeCounted(graphstore.KindVersion),
	}, {
		// The issue link pass (backfillIssueLinks) over a wait a build before the store wrote,
		// DRIVE-1 blocked by DRIVE-2, naming no node: DRIVE-1's needs, which the term
		// issue_relation counts.
		Name: issueLinkBatchDrive, Database: database,
		Sites: []string{issueLinkBatchDrive},
		Ready: func(t *testing.T) {
			if _, err := database.Pool.Exec(ctx, `
				insert into issue_links (issue_key, kind, target_key, created_at) values ($1, 'blocked_by', $2, now() - interval '3 days')
			`, drivenIssue, drivenTarget); err != nil {
				t.Fatalf("write the link a build before the store wrote: %v", err)
			}
		},
		Drive:  drivenPass(database, backfillIssueLinks),
		Counts: map[StoredKind][]string{{graphstore.KindIssue, graphstore.TupleNeeds}: {"issue_relation"}},
	}, {
		// The reference pass (backfillReferences) over a refs row a build before the store wrote
		// out of a document of DRIVE-1 to DRIVE-2, which no tuple states: the document's
		// mentions, which the term document_ref counts.
		Name: referenceBatchDrive, Database: database,
		Sites: []string{referenceBatchDrive},
		Ready: func(t *testing.T) {
			document, _ := drivenDocument(t, database, "citing-md")
			if _, err := database.Pool.Exec(ctx, `
				insert into refs (from_kind, from_id, to_kind, to_id, created_at, source_seq)
				values ('artifact', $1, 'issue', $2, now() - interval '2 days', 41)
			`, document, drivenTarget); err != nil {
				t.Fatalf("write the reference a build before the store wrote: %v", err)
			}
		},
		Drive:  drivenPass(database, backfillReferences),
		Counts: map[StoredKind][]string{{graphstore.KindArtifact, graphstore.TupleMentions}: {"document_ref"}},
	}, {
		// The anchor pass (backfillAnchoredIn) over a comment this build wrote with its node,
		// anchored on a block of a document of DRIVE-1, whose node states no anchored_in, as a
		// process of the release before leaves the row it anchors: the comment's anchored_in and,
		// since it answers nothing, its part_of to the same block (graphstore.AnchorRelation), which
		// the term anchor_block counts.
		Name: anchorBatchDrive, Database: database,
		Sites: []string{anchorBatchDrive},
		Ready: func(t *testing.T) {
			document, refKey := drivenDocument(t, database, "anchored-md")
			seedAnchoredComment(t, database, drivenProject, drivenIssue, document, refKey,
				"d0d0d0d0-0000-4000-8000-000000000001", time.Now().UTC().Add(-48*time.Hour))
		},
		Drive: drivenPass(database, backfillAnchoredIn),
		Counts: map[StoredKind][]string{
			{graphstore.KindComment, graphstore.TupleAnchoredIn}: {"anchor_block"},
			{graphstore.KindComment, graphstore.TuplePartOf}:     {"anchor_block"},
		},
	}, {
		Name: askedOnBatchDrive, Database: database,
		Sites: []string{askedOnBatchDrive},
		Ready: func(t *testing.T) {
			document, refKey := drivenDocument(t, database, "asked-on-md")
			seedAskWithoutBlockTuple(t, database, drivenProject, drivenIssue, document, refKey)
		},
		Drive:  drivenPass(database, backfillAskedOn),
		Counts: map[StoredKind][]string{{graphstore.KindAsk, graphstore.TupleAskedOn}: {"ask_opened"}},
	}, {
		// The result pass (backfillResults) over a failed result a release before this one
		// recorded, by alice, of check c1 of DRIVE-1's Verification document, its row and no
		// node: the result's node with its base attributes, its values, its part_of, its result_of
		// and her authored_by, which the term result counts.
		Name: resultBatchDrive, Database: database,
		Sites: []string{resultBatchDrive},
		Ready: func(t *testing.T) {
			document, _ := drivenDocument(t, database, "verification-md")
			if err := database.Pool.QueryRow(ctx, `
				insert into check_results (issue_key, check_issue_key, check_artifact_id, check_block_id, check_version,
					check_sha256, role, commit_sha, verdict, failed, actor, created_at)
				values ($1, $1, $2::uuid, 'c1', 1, 'drive-check-sum', 'implementer', '1111111111111111111111111111111111111111',
				        'fail', 1, '{"kind":"user","id":"alice"}', now() - interval '3 days')
				returning id::text
			`, drivenIssue, document).Scan(&result); err != nil {
				t.Fatalf("write the result a release before this one recorded: %v", err)
			}
		},
		Drive:  drivenPass(database, backfillResults),
		Counts: resultKindsCounted(),
	}, {
		// The retraction pass (backfillResultRetractions) over alice's retraction of that result,
		// which a release before this one wrote: the result's retracted value, which the term
		// result_retraction counts.
		Name: resultRetractionBatchDrive, Database: database,
		Sites: []string{resultRetractionBatchDrive},
		Ready: func(t *testing.T) {
			if _, err := database.Pool.Exec(ctx, `
				insert into check_result_retractions (result_id, reason, actor, created_at)
				values ($1::uuid, 'The run measured the wrong build.', '{"kind":"user","id":"alice"}', now() - interval '1 day')
			`, result); err != nil {
				t.Fatalf("write the retraction a release before this one wrote: %v", err)
			}
		},
		Drive:  drivenPass(database, backfillResultRetractions),
		Counts: map[StoredKind][]string{{graphstore.KindResult, graphstore.AttributeRetracted}: {"result_retraction"}},
	}, {
		// The component pass (backfillComponents) over the two components a process of the release
		// before imported from DRIVE's source, naming no node, census under store: each one's node
		// with its visibility, state and project, which the term component counts, and census's
		// part_of store, which component_relation counts.
		Name: componentBatchDrive, Database: database,
		Sites: []string{componentBatchDrive, "graphstore/write.go#Write"},
		Ready: func(t *testing.T) {
			drivenTx(t, database, "write DRIVE's source, its snapshot and its two components", func(tx pgx.Tx) error {
				var source, snapshot int64
				if err := tx.QueryRow(ctx, `
					insert into architecture_sources (project_key, repo, branch, installation_id, created_by)
					values ($1, 'drive/arch', 'main', 1, '{"kind":"user","id":"alice"}') returning id
				`, drivenProject).Scan(&source); err != nil {
					return err
				}
				if err := tx.QueryRow(ctx, `
					insert into architecture_snapshots (project_key, commit, imported_at, files)
					values ($1, 'one', now() - interval '2 days', '{}') returning id
				`, drivenProject).Scan(&snapshot); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, `
					insert into components (project_key, id, title, prose, parent, snapshot_id, source_id) values
						($1, 'store', 'Store', 'The store.', null, $2, $3),
						($1, 'census', 'Census', 'The census.', 'store', $2, $3)
				`, drivenProject, snapshot, source)
				return err
			})
		},
		Drive: drivenPass(database, backfillComponents),
		Counts: map[StoredKind][]string{
			{graphstore.KindComponent, graphstore.AttributeVisibility}: {graphstore.KindComponent},
			{graphstore.KindComponent, graphstore.AttributeState}:      {graphstore.KindComponent},
			{graphstore.KindComponent, graphstore.AttributeProject}:    {graphstore.KindComponent},
			{graphstore.KindComponent, graphstore.TuplePartOf}:         {"component_relation"},
		},
	}, {
		// The dependency pass (backfillComponentDepends) over census's dependency on store, which
		// the release before wrote naming no node: census's depends_on, which the term
		// component_relation counts.
		Name: componentDependsBatchDrive, Database: database,
		Sites: []string{componentDependsBatchDrive, "graphstore/write.go#Write"},
		Ready: func(t *testing.T) {
			if _, err := database.Pool.Exec(ctx, `
				insert into component_depends (project_key, from_id, to_id) values ($1, 'census', 'store')
			`, drivenProject); err != nil {
				t.Fatalf("write the dependency the release before wrote: %v", err)
			}
		},
		Drive:  drivenPass(database, backfillComponentDepends),
		Counts: map[StoredKind][]string{{graphstore.KindComponent, graphstore.TupleDependsOn}: {"component_relation"}},
	}, {
		// The issue component pass (backfillIssueComponentMembers) over DRIVE-1's components the
		// release before set to census, naming no node: DRIVE-1's affects, which the term
		// component_relation counts.
		Name: memberBatchDrive, Database: database,
		Sites: []string{memberBatchDrive},
		Ready: func(t *testing.T) {
			drivenTx(t, database, "write DRIVE-1's components as the release before set them", func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `
					insert into issue_components (issue_key, mode, updated_at) values ($1, 'explicit', now() - interval '1 day')
				`, drivenIssue); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, `
					insert into issue_component_members (issue_key, project_key, component_id) values ($1, $2, 'census')
				`, drivenIssue, drivenProject)
				return err
			})
		},
		Drive:  drivenPass(database, backfillIssueComponentMembers),
		Counts: map[StoredKind][]string{{graphstore.KindIssue, graphstore.TupleAffects}: {"component_relation"}},
	}, {
		// A retarget of a lens's has_kind from trade-off to inversion, two kinds the catalogue's
		// seed holds, as the pointer route makes one (api's writeReasoningPointer, through
		// graphstore.Retarget): the retraction of trade-off and the assertion of inversion, which
		// the term pointer_retarget counts.
		Name: retargetDrive, Database: database,
		Sites: []string{retargetDrive, "graphstore/write.go#Write"},
		Ready: func(t *testing.T) {
			drivenTx(t, database, "write a trade-off lens", func(tx pgx.Tx) error {
				now := time.Now().UTC()
				return graphstore.Write(ctx, tx, []graphstore.Node{{
					Kind: graphstore.KindLens, ID: "driven-lens", Supertag: graphstore.KindLens,
					Visibility: graphstore.VisibilityProject, State: graphstore.StateLive, Project: drivenProject,
					MadeAt: now, MadeBy: drivenAuthor, SourceEvent: graphstore.NoEvent,
				}}, []graphstore.Tuple{{
					FromKind: graphstore.KindLens, FromID: "driven-lens", Kind: graphstore.TupleHasKind,
					ToKind: graphstore.KindLensKind, ToID: "trade-off",
					At: now, Who: drivenAuthor, SourceEvent: graphstore.NoEvent, Asserted: true,
				}})
			})
		},
		Drive: func(t *testing.T) {
			drivenTx(t, database, "retarget the lens's has_kind at inversion", func(tx pgx.Tx) error {
				return graphstore.Retarget(ctx, tx, graphstore.Tuple{
					FromKind: graphstore.KindLens, FromID: "driven-lens", Kind: graphstore.TupleHasKind,
					ToKind: graphstore.KindLensKind, ToID: "trade-off",
					At: time.Now().UTC(), Who: drivenAuthor, SourceEvent: graphstore.NoEvent, Asserted: true,
				}, graphstore.KindLensKind, "inversion")
			})
		},
		Counts: map[StoredKind][]string{{graphstore.KindLens, graphstore.TupleHasKind}: {"pointer_retarget"}},
	}, {
		// The rebuild of every reference source (refs.RebuildAll), as `envoy-dispatch
		// rebuild-refs` runs it, over a document of DRIVE-1 whose one version cites DRIVE-2 and
		// which no refs row and no tuple holds yet: its reconcile (replaceInTx) states the
		// document's mentions, which the term document_ref counts. Every other source the database
		// holds cites nothing and holds no reference, so its reconcile appends nothing.
		Name: rebuildDrive, Database: database,
		Sites: []string{rebuildDrive},
		Ready: func(t *testing.T) {
			document, _ := drivenDocument(t, database, "rebuilt-md")
			censusVersion(t, database, document, 1, "Cites "+drivenTarget+".", drivenAuthor)
		},
		Drive: func(t *testing.T) {
			rebuilt, err := refs.RebuildAll(ctx, database.Pool, sizedServerURL)
			if err != nil {
				t.Fatalf("rebuild the references: %v", err)
			}
			if rebuilt.Documents == 0 {
				t.Fatalf("the rebuild reports %+v, want rebuilt-md's references rebuilt", rebuilt)
			}
		},
		Counts: map[StoredKind][]string{{graphstore.KindArtifact, graphstore.TupleMentions}: {"document_ref"}},
	}, {
		// The Keep pass (backfillKeepSums) over a Keep a release before this one made, by alice, of a
		// decision of a document of DRIVE-1 that carries shaped, summed over its words with shaped:
		// the decision's kept_text_sum, rewritten to the sum of its words without shaped, which the
		// term keep counts. It comes after the rebuild, which would otherwise read the document's
		// pointer as a reference of its own.
		Name: keepSumBatchDrive, Database: database,
		Sites: []string{keepSumBatchDrive},
		Ready: func(t *testing.T) {
			document, _ := drivenDocument(t, database, "kept-md")
			issue := drivenIssue
			pointer := refs.BlockRef(refs.ArtifactRef(&issue, drivenProject, nil, "kept-md", "doc", false), "r1")
			kept := seedShapedDecisions(t, database, document, pointer, "We keep the node store.")[0]
			keepAsTheReleaseBefore(t, database, document, kept, kept.shaped)
		},
		Drive:  drivenPass(database, backfillKeepSums),
		Counts: map[StoredKind][]string{{refs.BlockNodeKind, refs.ValueKeptTextSum}: {"keep"}},
	}, {
		// The approves pass (backfillApproves) over an approval request this build wrote with its
		// node, naming version 1 of a document of DRIVE-1 in its row, whose node states no approves
		// tuple, as a process of the release before leaves a request it moves: the one approves
		// tuple to the version node, which the term ask_opened counts.
		Name: approvesBatchDrive, Database: database,
		Sites: []string{approvesBatchDrive},
		Ready: func(t *testing.T) {
			document, _ := drivenDocument(t, database, "approved-md")
			seedApprovalAsk(t, database, drivenProject, drivenIssue, document, "approved-md",
				"d0d0d0d0-0000-4000-8000-0000000000a1", time.Now().UTC().Add(-48*time.Hour))
		},
		Drive:  drivenPass(database, backfillApproves),
		Counts: map[StoredKind][]string{{graphstore.KindAsk, TupleApproves}: {"ask_opened"}},
	}}
}

// archProject is the project the architecture drives import a model into, archRepo the
// repository of its one source, and archCommit that repository's head.
const (
	archProject = "ARCH"
	archRepo    = "drive/arch"
	archCommit  = "1111111111111111111111111111111111111111"
)

// archFiles is the model at archCommit: the server, its API under it, which depends on the
// store, and the store.
var archFiles = map[string]string{
	"server.md": "---\ntitle: Server\n---\nThe server.\n",
	"api.md":    "---\ntitle: HTTP API\nparent: server\ndepends_on: [store]\npaths: [internal/api]\n---\nThe API.\n",
	"store.md":  "---\ntitle: Store\nexternal: true\n---\nThe store.\n",
}

// archDrives are the drives of the architecture importer's two writers of the store, over a
// database of their own holding project ARCH and its one source: the first import of the source's
// model (architecture.Importer.Sync, which states the components in stateComponents), and then the
// source's removal, which states every component it imported gone (RetireComponents) before it
// deletes the source's row, as api's deleteArchitectureSource does.
func archDrives(t *testing.T) []StoreDrive {
	t.Helper()
	ctx := context.Background()
	database := storetest.Open(t)
	const installation = 11
	drivenTx(t, database, "write project ARCH and its source", func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `insert into projects (key, name, default_assignee) values ($1, 'Architecture', 'alice')`, archProject); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			insert into architecture_sources (project_key, repo, branch, installation_id, created_by)
			values ($1, $2, 'main', $3, '{"kind":"user","id":"alice"}')
		`, archProject, archRepo, installation)
		return err
	})
	importer := architecture.NewImporter(database, archGitHub(t, installation), events.NewBroker())
	t.Cleanup(importer.Close)
	return []StoreDrive{{
		// The first import of ARCH's model: each component's node with its visibility, state and
		// project, which the term component counts, and the API's part_of the server and its
		// depends_on the store, which component_relation counts.
		Name: importDrive, Database: database,
		Sites: []string{importDrive, "graphstore/write.go#Write"},
		Drive: func(t *testing.T) {
			if _, err := importer.Sync(ctx, archProject, archRepo); err != nil {
				t.Fatalf("import ARCH's model: %v", err)
			}
		},
		Counts: map[StoredKind][]string{
			{graphstore.KindComponent, graphstore.AttributeVisibility}: {graphstore.KindComponent},
			{graphstore.KindComponent, graphstore.AttributeState}:      {graphstore.KindComponent},
			{graphstore.KindComponent, graphstore.AttributeProject}:    {graphstore.KindComponent},
			{graphstore.KindComponent, graphstore.TuplePartOf}:         {"component_relation"},
			{graphstore.KindComponent, graphstore.TupleDependsOn}:      {"component_relation"},
		},
	}, {
		// The removal of ARCH's source: under its row, every component it imported stated gone
		// and the API's part_of and depends_on retracted, which the term component_retired counts,
		// and then the row deleted, which takes the components' rows with it.
		Name: retireDrive, Database: database,
		Sites: []string{retireDrive, "graphstore/write.go#Write"},
		Drive: func(t *testing.T) {
			drivenTx(t, database, "remove ARCH's source", func(tx pgx.Tx) error {
				var source int64
				if err := tx.QueryRow(ctx, `
					select id from architecture_sources where project_key = $1 and repo = $2 for update
				`, archProject, archRepo).Scan(&source); err != nil {
					return err
				}
				rows, err := tx.Query(ctx, `select id from components where project_key = $1 and source_id = $2 order by id`, archProject, source)
				if err != nil {
					return err
				}
				ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
				if err != nil {
					return err
				}
				if len(ids) != len(archFiles) {
					return fmt.Errorf("the source holds the components %v, want the %d the import wrote", ids, len(archFiles))
				}
				if err := architecture.RetireComponents(ctx, tx, archProject, ids, drivenAuthor, time.Now().UTC()); err != nil {
					return err
				}
				_, err = tx.Exec(ctx, `delete from architecture_sources where id = $1`, source)
				return err
			})
		},
		Counts: map[StoredKind][]string{
			{graphstore.KindComponent, graphstore.AttributeState}: {"component_retired"},
			{graphstore.KindComponent, graphstore.TuplePartOf}:    {"component_retired"},
			{graphstore.KindComponent, graphstore.TupleDependsOn}: {"component_retired"},
		},
	}}
}

// archGitHub serves the GitHub App surface an import of archRepo walks - the installation, a
// token, the branch's head, the architecture directory's subtree and each file's blob - with
// archFiles at archCommit, and answers an App client pointed at it.
func archGitHub(t *testing.T, installation int64) *githubapp.Client {
	t.Helper()
	encode := func(w http.ResponseWriter, value any) {
		if err := json.NewEncoder(w).Encode(value); err != nil {
			t.Errorf("encode a fake GitHub answer: %v", err)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, _ *http.Request) {
		encode(w, map[string]any{"id": installation, "app_slug": "dispatch-test", "permissions": map[string]string{"contents": "read"}})
	})
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		encode(w, map[string]any{"token": "ghs_driven", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)})
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{branch}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") == "application/vnd.github.sha" {
			w.Header().Set("Content-Type", "application/vnd.github.sha")
			fmt.Fprint(w, archCommit)
			return
		}
		encode(w, map[string]any{"sha": archCommit})
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/git/trees/{ref}", func(w http.ResponseWriter, _ *http.Request) {
		names := slices.Sorted(maps.Keys(archFiles))
		entries := make([]map[string]any, len(names))
		for index, name := range names {
			entries[index] = map[string]any{"path": name, "mode": "100644", "type": "blob", "sha": "blob-" + name, "size": len(archFiles[name])}
		}
		encode(w, map[string]any{"sha": "tree-driven", "truncated": false, "tree": entries})
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/git/blobs/{sha}", func(w http.ResponseWriter, r *http.Request) {
		content, ok := archFiles[strings.TrimPrefix(r.PathValue("sha"), "blob-")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		encode(w, map[string]any{"content": base64.StdEncoding.EncodeToString([]byte(content)), "encoding": "base64"})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { githubtest.Unexpected(t, w, r) })
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate the fake App's key: %v", err)
	}
	client, err := githubapp.New(&auth.AppConfig{
		ClientID: "Iv1.test", PEM: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})),
	}, githubtest.Server(t, githubtest.Spec{Handle: mux.ServeHTTP}))
	if err != nil {
		t.Fatalf("make the App client of the fake GitHub: %v", err)
	}
	return client
}
