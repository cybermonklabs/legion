package docs

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The steps of TestTheTreeRendersAPostCutoverVersionFromItsTuples that hold R15's version whose
// tree does not render as its stored markdown (tree_mismatch) and R14's carry of the pass that
// states a version first. Their issues are of a project of their own, MISS, so no other step's
// issue shares their documents.

// mismatchSeed is the text the mismatched version's write seeds its document with: what the
// document's tree renders as, and what its version does not store.
const mismatchSeed = "Seeded words.\n"

// renderRow is a version's version_renders row as the steps read it.
type renderRow struct {
	at         *time.Time
	digest     *string
	readSeq    *int64
	mismatch   bool
	repairedAt *time.Time
}

// readRenderRow reads the version_renders row of version number of artifactID.
func readRenderRow(t *testing.T, database *store.Store, artifactID string, number int) renderRow {
	t.Helper()
	var row renderRow
	if err := database.Pool.QueryRow(context.Background(), `
		select at, record_sha256, read_seq, tree_mismatch, tree_mismatch_repaired_at
		from version_renders where artifact_id = $1 and number = $2`, artifactID, number).Scan(
		&row.at, &row.digest, &row.readSeq, &row.mismatch, &row.repairedAt); err != nil {
		t.Fatalf("read the version_renders row of version %d of %s: %v", number, artifactID, err)
	}
	return row
}

// renderCensusLines is the lines `census --store` prints of census, a census of the rendered
// versions, as the command prints a store report that holds it (graphcensus.StoreReport.Write).
func renderCensusLines(census graphcensus.VersionRenderCensus) []string {
	var printed bytes.Buffer
	(&graphcensus.StoreReport{VersionRenders: &census}).Write(&printed)
	return strings.Split(printed.String(), "\n")
}

// markLine is the line the census of the rendered versions prints of a version whose
// tree_mismatch mark stands, at address, `version <ref_key>@v<n>`.
func markLine(address string) string { return "census: versions tree_mismatch at " + address }

// differenceAt is the cause of the difference census reads at address, "" for none.
func differenceAt(census graphcensus.VersionRenderCensus, address string) string {
	for _, difference := range census.Reported {
		if difference.At == address {
			return difference.Cause
		}
	}
	return ""
}

// seedMismatchIssues writes the project MISS and its issues MISS-1 to MISS-6, none of which has
// converted: no issue_trees row and no document_rooms row.
func seedMismatchIssues(t *testing.T, database *store.Store) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into projects (key, name, default_assignee) values ('MISS', 'Mismatches', 'alice') on conflict (key) do nothing;
		insert into issues (key, project_key, number, title, created_by, rank)
		select 'MISS-' || n, 'MISS', n, 'Mismatch ' || n, '{"kind":"user","id":"alice"}', 'U' || n
		from generate_series(1, 6) n
		on conflict (key) do nothing`); err != nil {
		t.Fatalf("seed project MISS: %v", err)
	}
}

// mismatchedMarkdown is the renderer's own rendering of a document that holds one paragraph,
// "Other words." (documentMarkdownWithOffsets): the markdown the mismatched version stores, which
// the repair restates only because it reads back and renders as its own bytes.
func mismatchedMarkdown(t *testing.T) string {
	t.Helper()
	tree, err := pmdoc.Parse("Other words.\n")
	if err != nil {
		t.Fatalf("parse the mismatched paragraph: %v", err)
	}
	markdown, _, err := documentMarkdownWithOffsets(tree)
	if err != nil {
		t.Fatalf("render the mismatched paragraph: %v", err)
	}
	read, err := pmdoc.ParseRendering(markdown)
	if err != nil {
		t.Fatalf("the mismatched markdown does not render back as itself: %v", err)
	}
	if again, _, err := documentMarkdownWithOffsets(read); err != nil || again != markdown || markdown == mismatchSeed {
		t.Fatalf("the mismatched markdown does not render back as itself: %q renders as %q (%v)", markdown, again, err)
	}
	return markdown
}

// createSpecOf creates the spec of issue, with primary set as treeDocument creates one, whose node
// is its query's, named on its row (writeQueryNode), so its versions have a record of R14 and the
// reconciliation's pass over the rows another build wrote with no node
// (writeObjectNodesAnotherBuildWrote) leaves the node a query node. It answers the spec and its
// ref_key.
func createSpecOf(t *testing.T, database *store.Store, issue string) (string, string) {
	t.Helper()
	var id, refKey string
	if err := database.Pool.QueryRow(context.Background(), `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ($1, 'MISS', 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"alice"}')
		returning id::text, ref_key
	`, issue).Scan(&id, &refKey); err != nil {
		t.Fatalf("create the spec of %s: %v", issue, err)
	}
	writeQueryNode(t, database, "MISS", issue, "spec", graphstore.QuerySpec)
	return id, refKey
}

// writeMismatchedSpec creates the spec of issue (createSpecOf) and writes its version 1 as the
// upload route writes a version (writeFirstVersion), with the one input the product never gives:
// markdown other than the text the seed left. It answers the spec and its ref_key.
func writeMismatchedSpec(t *testing.T, database *store.Store, service *Service, issue, other string) (string, string) {
	t.Helper()
	id, refKey := createSpecOf(t, database, issue)
	writeFirstVersion(t, database, service, id, refKey, mismatchSeed, func(string) string { return other })
	return id, refKey
}

// writeFirstVersion writes version 1 of the document id, whose ref_key is refKey, as the upload
// route and the issue create write a version (api's storeArtifact, createIssue): in one
// transaction joined to the service it seeds the document's text with seed (SeedText) and queues
// version 1 with markdown(the text the seed left) (Ledger.QueueVersion), which records the
// version's doc_update_version at the head the seed left and inserts the version's
// version_renders row; it indexes the version (IndexDocumentReferences), which leaves the blocks
// of a version of more than deferBlockIndexBlocks blocks, or of a tree that does not render as the
// version's markdown, to the reconciliation, writes the version's node (WriteVersionNode) and
// commits.
func writeFirstVersion(t *testing.T, database *store.Store, service *Service, id, refKey, seed string, markdown func(seeded string) string) {
	t.Helper()
	ctx := context.Background()
	alice := model.Actor{Kind: "user", ID: "alice"}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the write of %s: %v", refKey, err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	seeded, err := service.SeedText(joined, id, seed, alice)
	if err != nil {
		t.Fatalf("seed %s: %v", refKey, err)
	}
	stored := markdown(seeded)
	version, err := ledger.QueueVersion(ctx, id, stored, []model.Actor{alice}, nil)
	if err != nil {
		t.Fatalf("queue version 1 of %s: %v", refKey, err)
	}
	ledger.WroteVersion(id, version, stored)
	_, stated, err := service.IndexDocumentReferences(joined, id, version.Number, stored)
	if err != nil {
		t.Fatalf("index version 1 of %s: %v", refKey, err)
	}
	owner, err := ReadVersionOwner(joined, tx, id)
	if err != nil {
		t.Fatalf("read the owner of version 1 of %s: %v", refKey, err)
	}
	node, err := WriteVersionNode(joined, tx, owner, version.Number, alice, version.CreatedAt)
	if err != nil {
		t.Fatalf("write the node of version 1 of %s: %v", refKey, err)
	}
	if err := causeInWritePhase(joined, tx, graphstore.Joined(stated, node), graphstore.NoEvent); err != nil {
		t.Fatalf("append what the write of %s stated: %v", refKey, err)
	}
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit the write of %s: %v", refKey, err)
	}
}

// renderStepTreeMismatch: a version whose tree does not render as its stored markdown keeps its
// stored bytes, and the census refuses on it, until a later quiet tick repairs it; and a mark a
// later version's pass has moved past is closed (R14, R15, R16).
//
// MISS-1's spec stores version 1 as mismatchedMarkdown over a tree seeded with mismatchSeed
// (writeMismatchedSpec). alice approves version 1, pinning its stored markdown's digest. The seed's
// pending row is settled as the product settles it (settleOwedNow): no content changed since
// version 1, so no version is written and the row goes. One tick then states the document's tree
// at version 1 and marks the row tree_mismatch: no time, no digest, no seq. The digest fill, as a
// write's caller runs it for the issue (fillOwedDigests) and as the tick repairs the fills requests
// lost (fillLostDigests), leaves that row with no time and its mark: the fill consumes no row with
// no time. The version answers
// its stored bytes, the census counts the row tree_mismatch, prints it on a mark line of its own,
// and counts it neither deferred nor a difference, the census alone
// refuses, and alice's approval stands with nothing carried. The next tick, with no update in
// flight, no pending row, no pass owed and the blocks still at version 1, restates version 1 from
// its stored bytes: its row holds its time, seq and record digest, its mark is cleared and
// tree_mismatch_repaired_at set, the version renders from the tree as its stored bytes, the census
// moves the row from tree_mismatch to tree_mismatch_repaired, prints no mark line at it and reads
// no difference at it, and the pass carries alice's
// approval to version 1 with the restore's reason, pinning the record's digest.
//
// MISS-2's spec is marked the same way, then version 2 is written through the real version write,
// which stands the blocks at version 2; the next tick closes version 1's mark: no time, no digest
// and no seq, the mark cleared and repaired_at set, version 1 answering its stored bytes, and the
// census reading tree_mismatch down by 1, tree_mismatch_repaired and deferred each up by 1, with
// no mark line at version 1.
func renderStepTreeMismatch(t *testing.T, rig *renderRig, service *Service) {
	ctx := context.Background()
	database := rig.database
	seedMismatchIssues(t, database)
	other := mismatchedMarkdown(t)

	spec, refKey := writeMismatchedSpec(t, database, service, "MISS-1", other)
	address := "version " + refKey + "@v1"
	approval := approveThroughTheRoute(t, service, spec, 1, "alice")
	if approval.ContentSHA256 == nil || *approval.ContentSHA256 != reviewTextHash(other) {
		t.Fatalf("alice's approval of %s@v1 through the approval route = %+v; want it pinning the stored markdown's %s", refKey, approval, reviewTextHash(other))
	}
	if err := service.settleOwedNow(ctx, spec, time.Now().Add(roomWait(t))); err != nil {
		t.Fatalf("settle %s: %v", refKey, err)
	}
	reconcileBlockIndex(t, service, "")
	if row := readRenderRow(t, database, spec, 1); row.at != nil || row.digest != nil || row.readSeq != nil || !row.mismatch {
		t.Errorf("after the tick the row of %s@v1 holds %v, %v and %v, tree_mismatch %t; want no time, no digest, no seq and tree_mismatch",
			refKey, row.at, row.digest, row.readSeq, row.mismatch)
	}
	// The digest fill consumes no row with no time (R15): the fill a write's caller runs for the
	// issue (fillOwedDigests), and the tick's repair of the fills requests lost (fillLostDigests),
	// which runs that fill for each issue that owes a digest, each leave the mismatch row with no
	// time and its tree_mismatch mark.
	if _, err := service.fillOwedDigests(ctx, "MISS-1", 0); err != nil {
		t.Fatalf("fill the record digests MISS-1 owes: %v", err)
	}
	if err := service.fillLostDigests(ctx); err != nil {
		t.Fatalf("the tick's repair of the record digests requests left owed: %v", err)
	}
	if row := readRenderRow(t, database, spec, 1); row.at != nil || !row.mismatch {
		t.Errorf("after the digest fill the tree_mismatch row of %s@v1 holds at %v and tree_mismatch %t; want no time and tree_mismatch: the fill consumes no row with no at",
			refKey, row.at, row.mismatch)
	}
	if got := rig.renderVersion(t, spec, 1); got.Source != StoredBytes || got.Markdown != other {
		t.Errorf("version 1 of %s renders %d bytes from %s, want its %d stored bytes from %s", refKey, len(got.Markdown), got.Source, len(other), StoredBytes)
	}
	marked := rig.readRenderCensus(t)
	if slices.Contains(marked.DeferredAt, address) {
		t.Errorf("the census counts version 1 of %s as deferred, want tree_mismatch", refKey)
	}
	if marked.TreeMismatch < 1 || !slices.Contains(marked.TreeMismatchAt, address) {
		t.Errorf("the census reads tree_mismatch=%d at %v, want 1 at version 1 of %s", marked.TreeMismatch, marked.TreeMismatchAt, refKey)
	}
	if !slices.Contains(renderCensusLines(marked), markLine(address)) {
		t.Errorf("the census prints no line %q while version 1 of %s holds tree_mismatch:\n%s",
			markLine(address), refKey, strings.Join(renderCensusLines(marked), "\n"))
	}
	if cause := differenceAt(marked, address); cause != "" {
		t.Errorf("the census reads a difference at %s: %s, want tree_mismatch alone", address, cause)
	}
	if !(&graphcensus.StoreReport{VersionRenders: &marked}).Refused() {
		t.Errorf("the census holds tree_mismatch=%d and is not refused", marked.TreeMismatch)
	}
	if newest, err := NewestReview(ctx, database.Pool, spec); err != nil || newest == nil || newest.ID != approval.ID {
		t.Errorf("the approval of %s at version 1 reads stale after a pass whose tree does not render as its stored bytes: the newest review is %+v (%v), want alice's own %s",
			refKey, newest, err, approval.ID)
	}

	reconcileBlockIndex(t, service, "")
	row := readRenderRow(t, database, spec, 1)
	if row.mismatch || row.repairedAt == nil || row.at == nil || row.digest == nil || row.readSeq == nil {
		t.Fatalf("version 1 of %s still holds tree_mismatch after a quiet tick: its row holds %v, %v and %v, tree_mismatch %t, repaired at %v",
			refKey, row.at, row.digest, row.readSeq, row.mismatch, row.repairedAt)
	}
	requireStatedRow(t, database, "MISS-1", graphstore.QuerySpec, spec, 1)
	if got := rig.renderVersion(t, spec, 1); got.Source != RenderedFromTree || got.Markdown != other {
		t.Errorf("version 1 of %s renders %d bytes from %s, want its %d stored bytes from %s", refKey, len(got.Markdown), got.Source, len(other), RenderedFromTree)
	}
	repaired := rig.readRenderCensus(t)
	if repaired.TreeMismatch != marked.TreeMismatch-1 || repaired.TreeMismatchRepaired != marked.TreeMismatchRepaired+1 {
		t.Errorf("the census reads tree_mismatch=%d tree_mismatch_repaired=%d after the repair, want %d and %d",
			repaired.TreeMismatch, repaired.TreeMismatchRepaired, marked.TreeMismatch-1, marked.TreeMismatchRepaired+1)
	}
	if cause := differenceAt(repaired, address); cause != "" {
		t.Errorf("the census reads a difference at %s after the repair: %s", address, cause)
	}
	if slices.Contains(repaired.TreeMismatchAt, address) || slices.Contains(renderCensusLines(repaired), markLine(address)) {
		t.Errorf("the census reads a tree_mismatch line at version 1 of %s after the repair restated it: %v", refKey, repaired.TreeMismatchAt)
	}
	carried, err := NewestReview(ctx, database.Pool, spec)
	switch {
	case err != nil || carried == nil:
		t.Errorf("the newest review of %s = %+v, %v; want a review carried to version 1", refKey, carried, err)
	case carried.ID == approval.ID || carried.State != "approved" || carried.Version != 1 || carried.Reason == nil ||
		*carried.Reason != restoredReason(1, 1) || carried.ContentSHA256 == nil || *carried.ContentSHA256 != *row.digest:
		t.Errorf("the approval of %s at version 1 reads stale after the repair restated it: the newest review is %+v, want a carry to version 1 reading %q and pinning the record's %s",
			refKey, carried, restoredReason(1, 1), *row.digest)
	}

	closed, closedRef := writeMismatchedSpec(t, database, service, "MISS-2", other)
	if err := service.settleOwedNow(ctx, closed, time.Now().Add(roomWait(t))); err != nil {
		t.Fatalf("settle %s: %v", closedRef, err)
	}
	reconcileBlockIndex(t, service, "")
	if row := readRenderRow(t, database, closed, 1); !row.mismatch {
		t.Fatalf("after the tick the row of %s@v1 holds tree_mismatch %t, want it marked", closedRef, row.mismatch)
	}
	if _, err := joinedReplaceText(service, closed, "Later words.\n", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("write the next text of %s: %v", closedRef, err)
	}
	if result, _ := writeVersionThroughTheRealPath(t, service, closed, "The next version"); result.Version.Number != 2 {
		t.Fatalf("the real version write wrote version %d of %s, want 2", result.Version.Number, closedRef)
	}
	settle(t, database, closed)
	before := rig.readRenderCensus(t)
	reconcileBlockIndex(t, service, "")
	row = readRenderRow(t, database, closed, 1)
	if row.mismatch || row.repairedAt == nil || row.at != nil || row.digest != nil || row.readSeq != nil {
		t.Errorf("version 1 of %s still holds tree_mismatch after a quiet tick: its row holds %v, %v and %v, tree_mismatch %t, repaired at %v; want the mark closed with no time, digest or seq",
			closedRef, row.at, row.digest, row.readSeq, row.mismatch, row.repairedAt)
	}
	if got := rig.renderVersion(t, closed, 1); got.Source != StoredBytes || got.Markdown != other {
		t.Errorf("version 1 of %s renders %d bytes from %s, want its %d stored bytes from %s", closedRef, len(got.Markdown), got.Source, len(other), StoredBytes)
	}
	after := rig.readRenderCensus(t)
	if after.TreeMismatch != before.TreeMismatch-1 || after.TreeMismatchRepaired != before.TreeMismatchRepaired+1 || after.Deferred != before.Deferred+1 {
		t.Errorf("the census reads tree_mismatch=%d tree_mismatch_repaired=%d deferred=%d after the repair, want %d %d %d",
			after.TreeMismatch, after.TreeMismatchRepaired, after.Deferred, before.TreeMismatch-1, before.TreeMismatchRepaired+1, before.Deferred+1)
	}
	if closedAt := "version " + closedRef + "@v1"; slices.Contains(after.TreeMismatchAt, closedAt) || slices.Contains(renderCensusLines(after), markLine(closedAt)) {
		t.Errorf("the census reads a tree_mismatch line at version 1 of %s after the repair closed it: %v", closedRef, after.TreeMismatchAt)
	}
}

// renderStepCarryOfACarry: the reason a carry of a carry writes (R14). MISS-3's spec, a query
// version's document, writes version 1 through the real version write, which indexes it in its own
// transaction, and alice approves it. Version 2 is an upload of version 1's text, written as
// storeArtifact writes one (uploadVersion): the replacement changes nothing, so the upload
// writes no tree and leaves version 2's blocks to the reconciliation, its row with no time, and
// CarryUploadedApproval carries alice's approval to version 2 by the restore, pinning the stored
// markdown's digest. The reconciliation's pass then states version 2, whose render is its stored
// bytes, records its time and its record's digest, and carries again, to version 2, since the
// newest review approves version 2: its reason names the version alice approved and the version
// whose text it holds (restoreCarriedReason), and it pins the record's digest.
func renderStepCarryOfACarry(t *testing.T, rig *renderRig, service *Service) {
	ctx := context.Background()
	database := rig.database
	seedMismatchIssues(t, database)
	spec, refKey := createSpecOf(t, database, "MISS-3")
	seedServiceText(t, service, spec, "The words the person approved.\n")
	writeVersionThroughTheRealPath(t, service, spec, "The first version")
	settle(t, database, spec)
	first := storedVersionMarkdown(t, database, spec, 1)
	approveStoredVersion(t, database, spec, 1, "alice")

	number, stored := uploadVersion(t, database, service, spec, first)
	if number != 2 || stored != first {
		t.Fatalf("the upload wrote version %d of %s storing %q, want version 2 storing version 1's %q", number, refKey, stored, first)
	}
	if row := readRenderRow(t, database, spec, 2); row.at != nil {
		t.Fatalf("version 2 of %s holds a time before its pass", refKey)
	}
	uploaded, err := NewestReview(ctx, database.Pool, spec)
	if err != nil || uploaded == nil || uploaded.Version != 2 || uploaded.Reason == nil || *uploaded.Reason != restoredReason(1, 1) ||
		uploaded.ContentSHA256 == nil || *uploaded.ContentSHA256 != reviewTextHash(first) {
		t.Fatalf("the upload of version 2 of %s carried %+v (%v), want %q pinning the stored markdown's %s", refKey, uploaded, err, restoredReason(1, 1), reviewTextHash(first))
	}

	drainOwed(t, database, service)
	row := readRenderRow(t, database, spec, 2)
	if row.at == nil || row.digest == nil {
		t.Fatalf("the pass left version 2 of %s with its row holding %v and %v, want its time and its record's digest", refKey, row.at, row.digest)
	}
	carried, err := NewestReview(ctx, database.Pool, spec)
	want := fmt.Sprintf(restoreCarriedReason, 1, 2)
	switch {
	case err != nil || carried == nil || carried.ID == uploaded.ID || carried.Version != 2 || carried.Reason == nil:
		t.Errorf("the carry to version 2 of %s gives the reason %+v (%v), want %q", refKey, carried, err, want)
	case *carried.Reason != want:
		t.Errorf("the carry to version 2 of %s gives the reason %q, want %q", refKey, *carried.Reason, want)
	case carried.ContentSHA256 == nil || *carried.ContentSHA256 != *row.digest:
		t.Errorf("the carry to version 2 of %s pins %v, want the record's %s", refKey, carried.ContentSHA256, *row.digest)
	}
}
