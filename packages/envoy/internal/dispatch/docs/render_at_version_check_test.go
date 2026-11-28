package docs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The helpers c-render-at-version's steps of TestTheTreeRendersAPostCutoverVersionFromItsTuples
// share beside their own files (version_render_test.go, version_render_deferred_test.go,
// version_render_mismatch_test.go), and its case of an unchanged upload over a deferred version of
// a converted query document (renderStepUnchangedUploadCarriesNothing).

// renderAtVersionRequirement is a requirement block of id whose words are words: a node the spec
// query takes (queryDefinitions), so a spec whose text holds one has a record of at least that
// node, and the digest of a version's record tells that record from an empty one. A spec of plain
// paragraphs and list items holds no node the spec query takes, and its record is zero bytes, the
// same for every such version.
func renderAtVersionRequirement(id, words string) string {
	return ":::requirement{#" + id + "}\n" + words + "\n:::\n"
}

// renderAtVersionEmptyDigest is the sha256 of no bytes: the digest of a record that takes no node.
var renderAtVersionEmptyDigest = func() string {
	sum := sha256.Sum256(nil)
	return hex.EncodeToString(sum[:])
}()

// requireRecordOfNodes fails unless the version_renders row of version number of the document
// artifactID, whose ref_key is refKey and which is a document of issue whose query is query, holds a
// time, a seq and the digest of a record that takes at least one node: the query's record read at
// the row's cut (ReadQueryRecord) takes nodes, is not empty, and digests to the digest the row
// holds. It answers that digest, so a digest the step compares with it is a real record's.
func requireRecordOfNodes(t *testing.T, database *store.Store, issue, query, artifactID, refKey string, number int) string {
	t.Helper()
	ctx := context.Background()
	var digest *string
	var at *time.Time
	var readSeq *int64
	if err := database.Pool.QueryRow(ctx, `
		select record_sha256, at, read_seq from version_renders where artifact_id = $1 and number = $2`,
		artifactID, number).Scan(&digest, &at, &readSeq); err != nil {
		t.Fatalf("read the version_renders row of %s@v%d: %v", refKey, number, err)
	}
	if digest == nil || at == nil || readSeq == nil {
		t.Fatalf("the version_renders row of %s@v%d holds %v, %v and %v, want a time, a seq and its record's digest", refKey, number, at, readSeq, digest)
	}
	record, err := ReadQueryRecord(ctx, database.Pool, issue, query, refs.TreeCut{At: *at, ReadSeq: *readSeq})
	if err != nil {
		t.Fatalf("read the %s record of %s at the cut of %s@v%d: %v", query, issue, refKey, number, err)
	}
	switch {
	case len(record.Nodes) == 0 || len(record.Record) == 0 || record.Digest == renderAtVersionEmptyDigest:
		t.Errorf("the %s record of %s at the cut of %s@v%d takes %d nodes in %d bytes, digest %s: the digest of the version's record would compare an empty record, want the version's nodes in it",
			query, issue, refKey, number, len(record.Nodes), len(record.Record), record.Digest)
	case *digest != record.Digest:
		t.Errorf("the version_renders row of %s@v%d holds the digest %s, want its %s record's, %s (%d nodes)", refKey, number, *digest, query, record.Digest, len(record.Nodes))
	}
	t.Logf("the %s record of %s at the cut of %s@v%d takes %d nodes in %d bytes, digest %s; the row holds %s",
		query, issue, refKey, number, len(record.Nodes), len(record.Record), record.Digest, *digest)
	return *digest
}

// requireRowNoEarlierThanItsTexts fails unless at, the time the version_renders row of the version
// what names holds, is no earlier than the time of every text value, text_parent tuple and
// text_rank value written of the blocks keyed under nodeKey, the node key of that version's
// document (or of its room): every one the pass that stated the version wrote among them.
func requireRowNoEarlierThanItsTexts(t *testing.T, database *store.Store, nodeKey, what string, at time.Time) {
	t.Helper()
	var latest time.Time
	var tuples int
	if err := database.Pool.QueryRow(context.Background(), `
		select coalesce(max(t.at), 'epoch'::timestamptz), count(*) from graph_tuples t
		where t.from_kind = $1 and t.from_id like $2 and t.kind = any($3)`,
		refs.BlockNodeKind, nodeKey+"#%", []string{graphstore.AttributeText, refs.TupleTextParent, refs.AttributeTextRank}).Scan(&latest, &tuples); err != nil {
		t.Fatalf("read the newest text value of %s: %v", nodeKey, err)
	}
	if tuples == 0 {
		t.Fatalf("the store holds no text value, text_parent or text_rank of a block of %s, so the time of %s is compared with nothing", nodeKey, what)
	}
	if at.Before(latest) {
		t.Errorf("the row of %s holds %s, before the text value its pass wrote at %s", what, at, latest)
	}
}

// renderAtVersionDigestEvent is what the artifact.digest events of one version of a document say:
// how many there are, and of the newest, approval_current, content_sha256 and approved_version.
type renderAtVersionDigestEvent struct {
	count           int
	current         *bool
	digest          *string
	approvedVersion *int
}

// readDigestEvents reads the artifact.digest events appended for version number of artifactID.
func readDigestEvents(t *testing.T, database *store.Store, artifactID string, number int) renderAtVersionDigestEvent {
	t.Helper()
	var read renderAtVersionDigestEvent
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) over (), (payload->>'approval_current')::boolean, payload->>'content_sha256', (payload->>'approved_version')::int
		from events where type = $1 and payload->>'artifact_id' = $2 and (payload->>'version')::int = $3
		order by id desc limit 1`, model.ArtifactDigestEventType, artifactID, number).Scan(
		&read.count, &read.current, &read.digest, &read.approvedVersion); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("read the artifact.digest of version %d of %s: %v", number, artifactID, err)
	}
	return read
}

// words are the event's approval_current, content_sha256 and approved_version as a failure names
// them, "null" for each it holds none of.
func (e renderAtVersionDigestEvent) words() (current, digest, approved string) {
	current, digest, approved = "null", "null", "null"
	if e.current != nil {
		current = strconv.FormatBool(*e.current)
	}
	if e.digest != nil {
		digest = *e.digest
	}
	if e.approvedVersion != nil {
		approved = strconv.Itoa(*e.approvedVersion)
	}
	return current, digest, approved
}

// reviewsAt is how many reviews of artifactID name version number.
func reviewsAt(t *testing.T, database *store.Store, artifactID string, number int) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from artifact_reviews where artifact_id = $1 and version = $2`, artifactID, number).Scan(&count); err != nil {
		t.Fatalf("read the reviews of version %d of %s: %v", number, artifactID, err)
	}
	return count
}

// approvedEvents is how many artifact.approved events name artifactID.
func approvedEvents(t *testing.T, database *store.Store, artifactID string) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from events where type = 'artifact.approved' and payload->>'artifact_id' = $1`, artifactID).Scan(&count); err != nil {
		t.Fatalf("read the artifact.approved events of %s: %v", artifactID, err)
	}
	return count
}

// convertWithoutARoom gives issue an issue_trees row naming a tree artifact of its own, as a
// conversion records an issue's tree, and writes no document_rooms row: the issue has converted,
// and each of its documents is still its own room, so an unchanged upload of one leaves its blocks
// to the reconciliation rather than indexing them in the upload's own transaction as a room
// document's is (R15).
func convertWithoutARoom(t *testing.T, database *store.Store, project, issue string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		with tree as (
			insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
			values ($1, $2, 'tree', 'tree', 'tree', false, '{"kind":"user","id":"alice"}')
			returning id
		)
		insert into issue_trees (issue_key, artifact_id) select $1, id from tree`, issue, project); err != nil {
		t.Fatalf("record the tree of %s: %v", issue, err)
	}
}

// renderAtVersionCarryWords are the words of the spec of the case of an unchanged upload, in a
// requirement, so its record takes a node.
const renderAtVersionCarryWords = "The words the person approved, unchanged."

// renderStepUnchangedUploadCarriesNothing: an unchanged upload of the approved text over a deferred
// version of a converted query document carries nothing, and the approval stands by its record's
// digest (R14). MISS-6 converts with no document_rooms row (convertWithoutARoom), so its spec,
// written as treeDocument writes one with its node its query's (createSpecOf), is a query node of a
// converted issue (queryDocumentStatement). Version 1, a requirement, is written through the real
// version write, which indexes it in its own transaction: its row holds its time and the digest of
// a record of the requirement's node. alice approves version 1 through the approval route, pinning
// that digest. Version 2 is an upload of version 1's text (uploadVersion): the replacement changes
// nothing, so the upload writes no tree and leaves version 2's blocks to the reconciliation, its
// row with no time, and CarryUploadedApproval carries nothing (textCarries): no review at version
// 2 and no artifact.approved. The reconciliation's pass states version 2, whose render is its stored
// bytes, records its time and its record's digest, which is version 1's, writes no carried review,
// since the newest review approves version 1, and appends one artifact.digest for version 2 with
// approval_current true and approved_version 1.
func renderStepUnchangedUploadCarriesNothing(t *testing.T, rig *renderRig, service *Service) {
	ctx := context.Background()
	database := rig.database
	seedMismatchIssues(t, database)
	const issue = "MISS-6"
	spec, refKey := createSpecOf(t, database, issue)
	convertWithoutARoom(t, database, "MISS", issue)
	var query bool
	if err := database.Pool.QueryRow(ctx, queryDocumentStatement, spec).Scan(&query); err != nil || !query {
		t.Fatalf("%s reads as a query document of a converted issue %t (%v), want true: the case is of a converted query document", refKey, query, err)
	}
	seedServiceText(t, service, spec, renderAtVersionRequirement("r1", renderAtVersionCarryWords))
	writeVersionThroughTheRealPath(t, service, spec, "The first version")
	settle(t, database, spec)
	first := storedVersionMarkdown(t, database, spec, 1)
	if !strings.Contains(first, renderAtVersionCarryWords) {
		t.Fatalf("version 1 of %s stores %q, want the requirement written", refKey, first)
	}
	digest := requireRecordOfNodes(t, database, issue, graphstore.QuerySpec, spec, refKey, 1)
	approval := approveThroughTheRoute(t, service, spec, 1, "alice")
	if approval.ContentSHA256 == nil || *approval.ContentSHA256 != digest {
		t.Fatalf("alice's approval of %s@v1 through the approval route = %+v; want it pinning the record's %s", refKey, approval, digest)
	}
	approved := approvedEvents(t, database, spec)

	number, stored := uploadVersion(t, database, service, spec, first)
	if number != 2 || stored != first {
		t.Fatalf("the upload wrote version %d of %s storing %q, want version 2 storing version 1's %q", number, refKey, stored, first)
	}
	if row := readRenderRow(t, database, spec, 2); row.at != nil {
		t.Errorf("version 2 of %s holds a time before its pass", refKey)
	}
	if newest, err := NewestReview(ctx, database.Pool, spec); err != nil || newest == nil || newest.ID != approval.ID || reviewsAt(t, database, spec, 2) != 0 {
		reason := "nothing"
		if newest != nil && newest.Reason != nil {
			reason = *newest.Reason
		}
		t.Errorf("the upload of version 2 of %s carried %q (the newest review %+v, %v; %d reviews at version 2), want no carry", refKey, reason, newest, err, reviewsAt(t, database, spec, 2))
	}
	if after := approvedEvents(t, database, spec); after != approved {
		t.Errorf("the upload of version 2 of %s appended %d artifact.approved, want none", refKey, after-approved)
	}

	drainOwed(t, database, service)
	row := readRenderRow(t, database, spec, 2)
	if row.at == nil || row.digest == nil || row.mismatch {
		t.Fatalf("the pass left version 2 of %s with its row holding %v and %v, tree_mismatch %t, want its time and its record's digest", refKey, row.at, row.digest, row.mismatch)
	}
	rig.requireRenderedAs(t, spec, refKey, 2, RenderedFromTree)
	if second := requireRecordOfNodes(t, database, issue, graphstore.QuerySpec, spec, refKey, 2); second != digest {
		t.Errorf("version 2 of %s records the digest %s, want version 1's %s: its text and its record are version 1's", refKey, second, digest)
	}
	if newest, err := NewestReview(ctx, database.Pool, spec); err != nil || newest == nil || newest.ID != approval.ID || reviewsAt(t, database, spec, 2) != 0 {
		t.Errorf("the pass over version 2 of %s wrote a carried review, want none: the newest review is %+v (%v), %d reviews at version 2, want alice's %s of version 1",
			refKey, newest, err, reviewsAt(t, database, spec, 2), approval.ID)
	}
	event := readDigestEvents(t, database, spec, 2)
	current, eventDigest, approvedVersion := event.words()
	if current != "true" {
		t.Errorf("the artifact.digest of version 2 of %s carries approval_current %s, want true", refKey, current)
	}
	if event.count != 1 || eventDigest != *row.digest || approvedVersion != "1" {
		t.Errorf("version 2 of %s holds %d artifact.digest events, the newest with content_sha256 %s and approved_version %s, want one, with the record's %s and approved_version 1: alice's approval of version 1 stands on it",
			refKey, event.count, eventDigest, approvedVersion, *row.digest)
	}
}

// roomJoinedRendering is the room's joined rendering of the issue room room, laid out as layout:
// every fragment of the room's own Yjs document read in the room's order and rendered as one
// document (renderRoomForUpdate), as the room renders itself. Nothing else writes the room while
// the step reads it.
func roomJoinedRendering(t *testing.T, service *Service, room string, layout roomLayout) string {
	t.Helper()
	var markdown string
	err := service.srv.Apply(context.Background(), room, func(doc *crdt.Doc, _ func(func(*crdt.Transaction))) {
		rendered, _, _, err := renderRoomForUpdate(doc, layout)
		if err != nil {
			t.Errorf("render the room %s: %v", room, err)
			return
		}
		markdown = rendered
	})
	if err != nil && !errors.Is(err, ygws.ErrNoChanges) {
		t.Fatalf("read the room %s: %v", room, err)
	}
	return markdown
}

// roomOwed is each document of the issue room room, laid out as layout, that owes its block index,
// by either mark the reconciliation lists a document by (refs.OwedBlockIndexDebts), and each that
// does not.
func roomOwed(t *testing.T, database *store.Store, room string, layout roomLayout) (owed, indexed []string) {
	t.Helper()
	for _, document := range layout.documents(room) {
		var held bool
		if err := database.Pool.QueryRow(context.Background(), `
			select exists (select 1 from document_block_versions b
			               where b.artifact_id = $2::uuid and (b.version is null or `+refs.OwedMarkListed("b", "$1::integer")+`))`,
			refs.BlockIndexMarkingRule, document).Scan(&held); err != nil {
			t.Fatalf("read whether %s owes its block index: %v", document, err)
		}
		if held {
			owed = append(owed, document)
		} else {
			indexed = append(indexed, document)
		}
	}
	return owed, indexed
}

// requireDurableAppendsLanded waits until every durable append of room, which what names, has
// landed (waitForDurableAppends), at most roomWait, as a race case does once it releases the room's
// lock and before it runs the room's settlement.
func requireDurableAppendsLanded(t *testing.T, service *Service, room, what string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), roomWait(t))
	defer cancel()
	if err := service.waitForDurableAppends(ctx, room); err != nil {
		t.Fatalf("wait for the durable appends of %s: %v", what, err)
	}
}

// requireWalkedBefore fails unless the walk of the tree of issue at the head (WalkIssueTree), the
// tree's own order, takes the node first before the member of the document refKey whose text value
// is text: the order the tree holds, which the render of a version does not print in.
func requireWalkedBefore(t *testing.T, database *store.Store, issue, refKey, first, text string) {
	t.Helper()
	ctx := context.Background()
	cut, err := refs.HeadCut(ctx, database.Pool)
	if err != nil {
		t.Fatalf("read the head's cut: %v", err)
	}
	members, err := refs.TreeMembers(ctx, database.Pool, refKey, cut)
	if err != nil {
		t.Fatalf("read the members of %s: %v", refKey, err)
	}
	facts, err := refs.TreeFactsOf(ctx, database.Pool, members, cut)
	if err != nil {
		t.Fatalf("read the members of %s: %v", refKey, err)
	}
	second := ""
	for _, member := range members {
		if facts[member].Values[graphstore.AttributeText] == text {
			second = member.ID
		}
	}
	if second == "" {
		t.Fatalf("no member of %s holds the text %q, so the tree's order of %s and it is read of nothing", refKey, text, first)
	}
	tree, err := WalkIssueTree(ctx, database.Pool, issue, cut)
	if err != nil {
		t.Fatalf("walk the tree of %s: %v", issue, err)
	}
	at := func(address string) int {
		for index, node := range tree.Nodes {
			if node.Kind == refs.BlockNodeKind && node.ID == address {
				return index
			}
		}
		return -1
	}
	if was, then := at(first), at(second); was < 0 || then < 0 || was > then {
		t.Errorf("the tree of %s takes %s at %d and %s, which holds %q, at %d in its walk, want %s before it",
			issue, first, was, second, text, then, first)
	}
}

// requireJoinedRenderingOfFragments fails unless the room's joined rendering of the issue room room
// (roomJoinedRendering) differs from its fragments' own renderings where the fragments step says
// it does: the newest version of the plan opens with a `-` list and that of the spec with a `***`
// rule, each as its own fragment renders it, and the joined rendering holds the Verification
// document's newest version as it stores it, then the plan's list written with the other marker,
// `*`, after the Verification document's list (otherListMarkers), then the spec opening with a
// `---` rule.
func requireJoinedRenderingOfFragments(t *testing.T, service *Service, database *store.Store, room issueRoom) {
	t.Helper()
	_, verification := newestStoredVersion(t, database, room.verification)
	_, plan := newestStoredVersion(t, database, room.plan)
	_, spec := newestStoredVersion(t, database, room.spec)
	if !strings.HasPrefix(plan, "- ") || !strings.HasPrefix(spec, "***\n") || !strings.Contains(verification, "\n- ") {
		t.Fatalf("the newest versions store %q, %q and %q, want the Verification document ending with a `-` list, the plan opening with one and the spec opening with `***`", verification, plan, spec)
	}
	lines := strings.SplitAfter(plan, "\n")
	for index, line := range lines {
		if strings.HasPrefix(line, "- ") {
			lines[index] = "* " + strings.TrimPrefix(line, "- ")
		}
	}
	joinedPlan, joinedSpec := strings.Join(lines, ""), "---"+strings.TrimPrefix(spec, "***")
	layout, err := readRoomLayout(context.Background(), database.Pool, room.tree)
	if err != nil {
		t.Fatalf("read the layout of the room of %s: %v", room.issue, err)
	}
	joined := roomJoinedRendering(t, service, room.tree, layout)
	if want := verification + "\n" + joinedPlan + "\n" + joinedSpec; !strings.Contains(joined, want) {
		t.Errorf("the room's joined rendering of %s reads %q, want in it the Verification document's own bytes, the plan's list with the other marker and the spec opening with `---`: %q",
			room.issue, joined, want)
	}
}

// renderAtVersionSettledSpec is the text a peer writes into the spec of DOC-1, whose version a real
// settlement writes: a paragraph and a requirement, a node the spec query takes, so the digest the
// settlement's room pass records is a real record's. It is its own rendering.
var renderAtVersionSettledSpec = "Spec words.\n\n" + renderAtVersionRequirement("rs", "The settled spec's own requirement.")
