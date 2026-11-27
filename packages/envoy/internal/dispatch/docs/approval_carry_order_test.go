package docs

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/rank"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// Every carried approval is written before its version (R14): a carry's artifact.approved comes
// before the version's artifact.version, in event id and in seq, which the deployed Legion daemon
// reads a restore and a carry by; the carried review names its own event; and a restore of a
// version that holds no record digest carries once, in the write's batch.

// The decision document the readings and upload carries are written over: a decision with no
// reading, and the same decision with the agent's reading under it, the whole change the reading
// case carries an approval across (readingCase).
const (
	orderDecision        = "Context\n\n:::decision{#d1}\nWe ship on Friday.\n:::\n"
	orderDecisionReading = "Context\n\n::::decision{#d1}\nWe ship on Friday.\n\n:::reading{#d1-r1}\nThe agent reads it as Friday.\n:::\n::::\n"
)

// orderAgent is the session that writes the versions the carries go to.
var orderAgent = model.Actor{Kind: "session", ID: "session-0123456789abcdef"}

// queueVersionEvent queues on the write phase of the ledger ctx joins, after every write queued so
// far, the artifact.version of version of artifactID, a document of issue, in who's name, and what
// the version's write stated, caused by that event (graphstore.LatestEvent), as api's
// commitArtifactVersionEvent queues a route's version event once the version's write - its carry
// among it - has queued its own.
func queueVersionEvent(ctx context.Context, service *Service, issue, artifactID string, version model.Version, stated graphstore.Uncaused, who model.Actor) error {
	event := model.Event{
		IssueKey: &issue, Type: "artifact.version", Actor: who,
		Payload: ArtifactVersionEventPayload(artifactID, "document.md", version, nil, model.ReferenceChanges{}),
	}
	sink := eventSink(ctx)
	return ledgerFrom(ctx).Queue(func(b *store.WriteBatch) error {
		if err := service.events.QueueAppend(b, event, sink); err != nil {
			return err
		}
		graphstore.QueueCaused(b, stated, graphstore.LatestEvent)
		graphstore.QueueHeads(b, graphstore.HeldHeads(stated))
		return nil
	})
}

// namedVersionWithItsEvent names the live document artifactID of issue as a version, as the agent,
// with its artifact.version queued after the write (queueVersionEvent), as the named-version route
// writes one, and answers the version it wrote.
func namedVersionWithItsEvent(t *testing.T, service *Service, issue, artifactID, summary string) int {
	t.Helper()
	var number int
	if err := joinedWrite(service, func(ctx context.Context) error {
		result, err := service.NamedVersion(ctx, artifactID, summary, orderAgent)
		if err != nil {
			return err
		}
		if !result.Wrote {
			return fmt.Errorf("the named version of %s wrote nothing", artifactID)
		}
		number = result.Version.Number
		return queueVersionEvent(ctx, service, issue, artifactID, result.Version, result.Blocks, orderAgent)
	}); err != nil {
		t.Fatalf("name %q: %v", summary, err)
	}
	return number
}

// uploadWithItsEvent writes markdown over artifactID, a document of issue, as api's storeArtifact
// writes an upload over a document: the text's replacement with the token of the version it
// replaces (ReplaceText), the version (Ledger.QueueVersion), its index (IndexDocumentReferences), its
// node (WriteVersionNode), the upload's carry (CarryUploadedApproval, weighing the facts the index
// and the node stated) and then the version's event with what the write stated, in one transaction
// joined to the service. It answers the version.
func uploadWithItsEvent(t *testing.T, service *Service, issue, artifactID, markdown string) int {
	t.Helper()
	ctx := context.Background()
	alice := model.Actor{Kind: "user", ID: "alice"}
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the upload over %s: %v", artifactID, err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	precondition, err := replacementPrecondition(joined, service, artifactID)
	if err != nil {
		t.Fatalf("read the token of %s: %v", artifactID, err)
	}
	stored, err := service.ReplaceText(joined, artifactID, markdown, alice, precondition)
	if err != nil {
		t.Fatalf("replace the text of %s: %v", artifactID, err)
	}
	version, err := ledger.QueueVersion(ctx, artifactID, stored, []model.Actor{alice}, nil)
	if err != nil {
		t.Fatalf("queue the upload's version of %s: %v", artifactID, err)
	}
	ledger.WroteVersion(artifactID, version, stored)
	_, stated, err := service.IndexDocumentReferences(joined, artifactID, version.Number, stored)
	if err != nil {
		t.Fatalf("index the upload's version of %s: %v", artifactID, err)
	}
	owner, err := ReadVersionOwner(joined, tx, artifactID)
	if err != nil {
		t.Fatalf("read the owner of the upload's version of %s: %v", artifactID, err)
	}
	node, err := WriteVersionNode(joined, tx, owner, version.Number, alice, version.CreatedAt)
	if err != nil {
		t.Fatalf("write the node of the upload's version of %s: %v", artifactID, err)
	}
	if err := CarryUploadedApproval(joined, tx, service.events, artifactID, version.Number, stored, nil, StatedWhole(stated, node), service.serverURL); err != nil {
		t.Fatalf("carry the approval of %s to the upload's version: %v", artifactID, err)
	}
	if err := queueVersionEvent(joined, service, issue, artifactID, version, graphstore.Joined(stated, node), alice); err != nil {
		t.Fatalf("queue the upload's version event of %s: %v", artifactID, err)
	}
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit the upload over %s: %v", artifactID, err)
	}
	return version.Number
}

// addDecisionReading is the live-tree edit of a browser that writes the agent's reading under the
// decision d1 (orderDecisionReading), the whole change between the two texts.
func addDecisionReading(t *testing.T) func(*pmdoc.Node) *pmdoc.Node {
	t.Helper()
	read, err := pmdoc.Parse(orderDecisionReading)
	if err != nil {
		t.Fatalf("parse the decision with its reading: %v", err)
	}
	decisionOf := func(tree *pmdoc.Node) *pmdoc.Node {
		for _, child := range tree.Children {
			if child.Type == decisionType {
				return child
			}
		}
		t.Fatalf("the tree holds no decision: %+v", tree)
		return nil
	}
	written := decisionOf(read)
	reading := written.Children[len(written.Children)-1]
	return func(tree *pmdoc.Node) *pmdoc.Node {
		decision := decisionOf(tree)
		decision.Children = append(decision.Children, reading)
		return tree
	}
}

// approvedDecisionDocument seeds orderDecision into a new document of DOC-1, has settlement write
// its version, and records alice's approval of it; it answers the service, the document and the
// version alice approved.
func approvedDecisionDocument(t *testing.T) (*Service, string, int) {
	t.Helper()
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, orderDecision)
	settleCurrentGeneration(t, service, artifactID)
	approved := latestVersionNumber(t, service, artifactID)
	approveStoredVersion(t, service.store, artifactID, approved, "alice")
	return service, artifactID, approved
}

// carriedOrder is what a carry to a version wrote, as the order tests read it: the carried
// review, its event_seq and its content_sha256; the carry's artifact.approved, its id, its seq and
// the content_sha256 its payload carries; and the version's artifact.version, its id, its seq and
// whether its payload holds a content_sha256 at all.
type carriedOrder struct {
	review         string
	reviewSeq      *int64
	reviewDigest   *string
	approvedID     int64
	approvedSeq    int64
	approvedDigest *string
	versionID      int64
	versionSeq     int64
	versionDigest  bool
}

// carriedOrderStatement reads carriedOrder of the newest carried review of version $2 of the
// document $1: one that holds its row of artifact_review_carries, with the newest artifact.approved
// and the newest artifact.version naming that document and that version.
const carriedOrderStatement = `
	select r.id::text, r.event_seq, r.content_sha256,
	       approved.id, approved.seq, approved.payload->>'content_sha256',
	       versioned.id, versioned.seq, versioned.payload ? 'content_sha256'
	from artifact_reviews r
	join artifact_review_carries c on c.review_id = r.id
	join lateral (
		select e.id, e.seq, e.payload from events e
		where e.type = 'artifact.approved' and e.payload->>'artifact_id' = r.artifact_id::text
		  and (e.payload->>'version')::int = r.version
		order by e.id desc limit 1
	) approved on true
	join lateral (
		select e.id, e.seq, e.payload from events e
		where e.type = 'artifact.version' and e.payload->>'artifact_id' = r.artifact_id::text
		  and (case jsonb_typeof(e.payload->'version') when 'object' then e.payload->'version'->>'number' else e.payload->>'version' end)::int = r.version
		order by e.id desc limit 1
	) versioned on true
	where r.artifact_id = $1 and r.version = $2
	order by r.created_at desc, r.id desc
	limit 1`

// readCarriedOrder reads what the carry to version number of artifactID wrote (carriedOrderStatement),
// failing the test, naming what, where no carried review of it and its two events stand.
func readCarriedOrder(t *testing.T, database *store.Store, what, artifactID string, number int) carriedOrder {
	t.Helper()
	var order carriedOrder
	if err := database.Pool.QueryRow(context.Background(), carriedOrderStatement, artifactID, number).Scan(
		&order.review, &order.reviewSeq, &order.reviewDigest,
		&order.approvedID, &order.approvedSeq, &order.approvedDigest,
		&order.versionID, &order.versionSeq, &order.versionDigest); err != nil {
		t.Fatalf("%s: read the carry to version %d of %s and its events: %v; want a carried review, its artifact.approved and the version's artifact.version",
			what, number, artifactID, err)
	}
	return order
}

// requireApprovalFirst fails the test, naming the row, unless the carry's artifact.approved comes
// before the version's artifact.version in event id and in seq.
func requireApprovalFirst(t *testing.T, row string, order carriedOrder) {
	t.Helper()
	if order.approvedID >= order.versionID || order.approvedSeq >= order.versionSeq {
		t.Errorf("the %s carry appended artifact.approved at id %d, seq %d and the version's artifact.version at id %d, seq %d: want the approval first in both",
			row, order.approvedID, order.approvedSeq, order.versionID, order.versionSeq)
	}
}

// recordResult records a result of the check c1 of the issue room room with the mark of the room
// issue's queries (leaveResultOwed), and drains that mark (DrainOwedQueries), whose version of the
// Verification document carries an approval across the appended result.
func recordResult(t *testing.T, database *store.Store, service *Service, room issueRoom) {
	t.Helper()
	leaveResultOwed(t, database, room)
	service.DrainOwedQueries(context.Background(), room.issue)
}

// TestEachCarryKindIsWrittenBeforeItsVersion holds each kind of carry to R14's order, one row for
// each: readings, a named version whose one change from the approved version is the agent's
// reading under a decision (writeVersionTx's carry); answer, bob's answer to an ask block, which
// settlement writes as a version whose block shows the answer (the settlement's carry); upload,
// an upload whose one change is that reading (CarryUploadedApproval); result, a result
// recorded on a check, whose drain writes a Verification version that differs from the approved
// one by the appended result alone (carryAppendedResults); record, a version whose record digest
// equals the approved one, as a paragraph moved in the text before a requirement makes it, which
// the settlement of a peer's move writes (the record case, recordUnmoved); and drain, a version the
// drain writes whose rendering alone moved, its record digest the approved one (the drain's record
// case, knownRecord). Each row reads the carry written - a carried review with its row of
// artifact_review_carries - and its artifact.approved before the version's artifact.version, in
// event id and in seq; the two record rows read the carry's reason and the version it names, and
// the version's digest, the approved one's.
func TestEachCarryKindIsWrittenBeforeItsVersion(t *testing.T) {
	t.Parallel()
	t.Run("readings", func(t *testing.T) {
		service, artifactID, approved := approvedDecisionDocument(t)
		editLiveTree(t, service, artifactID, addDecisionReading(t))
		number := namedVersionWithItsEvent(t, service, "DOC-1", artifactID, "The agent's reading")
		if number != approved+1 {
			t.Fatalf("the named version is version %d, want %d", number, approved+1)
		}
		requireApprovalFirst(t, "readings", readCarriedOrder(t, service.store, "the readings carry", artifactID, number))
	})
	t.Run("answer", func(t *testing.T) {
		service, artifactID := newTestService(t)
		service.settle = time.Hour
		seedServiceText(t, service, artifactID, carrySpec)
		settleCurrentGeneration(t, service, artifactID)
		approved := latestVersionNumber(t, service, artifactID)
		approveStoredVersion(t, service.store, artifactID, approved, "alice")
		answerBlockAs(t, service, artifactID, "ask-1", "bob", []string{"Yes"})
		settleCurrentGeneration(t, service, artifactID)
		number := latestVersionNumber(t, service, artifactID)
		if number != approved+1 {
			t.Fatalf("the latest version is %d after bob's answer settled, want the one version settlement wrote of it, %d", number, approved+1)
		}
		requireApprovalFirst(t, "answer", readCarriedOrder(t, service.store, "the answer carry", artifactID, number))
	})
	t.Run("upload", func(t *testing.T) {
		service, artifactID, approved := approvedDecisionDocument(t)
		number := uploadWithItsEvent(t, service, "DOC-1", artifactID, orderDecisionReading)
		if number != approved+1 {
			t.Fatalf("the upload wrote version %d, want %d", number, approved+1)
		}
		requireApprovalFirst(t, "upload", readCarriedOrder(t, service.store, "the upload carry", artifactID, number))
	})
	t.Run("result", func(t *testing.T) {
		ctx := context.Background()
		database := storetest.Open(t)
		service, server := newTestServiceInstance(t, database)
		room := createIssueRoom(t, database, 4)
		writeRoomQueryNodes(t, database, room)
		settleVerification(t, ctx, database, service, server.URL, room)
		approved := savedVersion(t, database, room.verification)
		requireStatedRow(t, database, room.issue, graphstore.QueryVerification, room.verification, approved)
		approveStoredVersion(t, database, room.verification, approved, "alice")
		recordResult(t, database, service, room)
		number := savedVersion(t, database, room.verification)
		if number != approved+1 {
			t.Fatalf("the drain of the result wrote version %d of DOC-4's Verification document, want %d", number, approved+1)
		}
		requireApprovalFirst(t, "result", readCarriedOrder(t, database, "the result carry", room.verification, number))
	})
	t.Run("record", func(t *testing.T) {
		written := approvedRecordRoom(t, 5)
		written.settleSpecEdit(t, context.Background(), moveParagraphBeforeRequirement)
		number := savedVersion(t, written.database, written.room.spec)
		if number != written.approved+1 {
			t.Fatalf("the settlement of the paragraph's move wrote version %d of DOC-5's spec, want %d", number, written.approved+1)
		}
		if count := carriedReviewCount(t, written.database, written.room.spec, number); count != 1 {
			t.Fatalf("version %d of DOC-5's spec holds %d carried reviews, want the record carry alone", number, count)
		}
		requireRecordCarry(t, written.database, "record", written.room.spec, written.approved, number)
	})
	t.Run("drain", func(t *testing.T) {
		written := approvedRecordRoom(t, 6)
		criterion, requirement := blockAddressByText(t, written.database, ":::criterion{#k1}"), blockAddressByText(t, written.database, ":::requirement{#r1}")
		statePlacedParagraph(t, written, rank.Between(textRankOf(t, written.database, criterion), textRankOf(t, written.database, requirement)))
		written.service.DrainOwedQueries(context.Background(), written.room.issue)
		number := savedVersion(t, written.database, written.room.spec)
		if number != written.approved+1 {
			t.Fatalf("the drain of the paragraph's text_rank wrote version %d of DOC-6's spec, want %d: its rendering moved", number, written.approved+1)
		}
		if stored := storedVersionMarkdown(t, written.database, written.room.spec, number); strings.Index(stored, recordParagraph) > strings.Index(stored, "An urgent ask") {
			t.Fatalf("the drain's version %d of DOC-6's spec reads\n%s\nwant the paragraph before the requirement", number, stored)
		}
		requireRecordCarry(t, written.database, "drain", written.room.spec, written.approved, number)
	})
}

// TestACarriedApprovalNamesItsOwnEvent holds the carried review to its own artifact.approved (R14):
// a named version whose one change from alice's approved version is a reading carries her approval,
// and the carried review's event_seq is that artifact.approved's seq, the event's payload carries
// the carried review's content_sha256, and the version's artifact.version, appended after it,
// carries no review digest at all.
func TestACarriedApprovalNamesItsOwnEvent(t *testing.T) {
	t.Parallel()
	service, artifactID, _ := approvedDecisionDocument(t)
	editLiveTree(t, service, artifactID, addDecisionReading(t))
	number := namedVersionWithItsEvent(t, service, "DOC-1", artifactID, "The agent's reading")
	order := readCarriedOrder(t, service.store, "the carry", artifactID, number)
	requireApprovalFirst(t, "readings", order)
	if order.reviewSeq == nil || *order.reviewSeq != order.approvedSeq {
		t.Errorf("the carried review %s holds event_seq %v, want the seq of its own artifact.approved, %d: the statement that sets event_seq named another event",
			order.review, order.reviewSeq, order.approvedSeq)
	}
	if order.reviewDigest == nil || order.approvedDigest == nil || *order.approvedDigest != *order.reviewDigest {
		t.Errorf("the artifact.approved of the carried review %s carries content_sha256 %v, want the review's %v: the statement that completes the payload named another event",
			order.review, order.approvedDigest, order.reviewDigest)
	}
	if order.versionDigest {
		t.Errorf("the artifact.version of version %d carries a content_sha256, want no review digest on the version event", number)
	}
}

// TestAPreCutoverRestoreCarriesOnce restores the approved text of a version that holds no
// version_renders row, and so no record digest (R14): PRE-1's spec, a query version's document,
// stores version 1 by SQL, as a release before the cutover wrote it, and alice approves it, pinning
// its stored markdown's sha256. Version 2 changes the text through the real version write, and
// version 3 restores version 1's text. The approval stands on no record's digest (recordPin), so
// version 3's write carries it as one plain carry, in its batch: exactly one carried review, which
// pins the sha256 of version 3's stored markdown, with one artifact.approved before the version's
// artifact.version; the write's own pass carries nothing again; and the fill after the COMMIT
// repins that carry on its own row of artifact_review_carries to version 3's record digest, with no
// second review and no second event, leaving the review's content_sha256 as it was.
func TestAPreCutoverRestoreCarriesOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := storetest.Open(t)
	service := newTestServiceOn(t, database)
	service.settle = time.Hour
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('PRE', 'Before the cutover', 'alice');
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('PRE-1', 'PRE', 1, 'Before the cutover', '{"kind":"user","id":"alice"}', 'U')`); err != nil {
		t.Fatalf("seed project PRE: %v", err)
	}
	writeQueryNode(t, database, "PRE", "PRE-1", "spec", graphstore.QuerySpec)
	approvedTree, err := pmdoc.Parse("The words alice approved before the cutover.\n")
	if err != nil {
		t.Fatal(err)
	}
	approvedText, err := pmdoc.Render(approvedTree)
	if err != nil {
		t.Fatal(err)
	}
	var spec string
	if err := database.Pool.QueryRow(ctx, `
		with spec as (
			insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
			values ('PRE-1', 'PRE', 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"alice"}')
			returning id
		)
		insert into artifact_versions (artifact_id, number, markdown, authors)
		select id, 1, $1, '[{"kind":"user","id":"alice"}]' from spec
		returning artifact_id::text`, approvedText).Scan(&spec); err != nil {
		t.Fatalf("write the spec of PRE-1 as the release before wrote it: %v", err)
	}
	approveStoredVersion(t, database, spec, 1, "alice")
	alice := model.Actor{Kind: "user", ID: "alice"}
	seedServiceText(t, service, spec, "Words written after the cutover.\n")
	if number := namedVersionWithItsEvent(t, service, "PRE-1", spec, "A change"); number != 2 {
		t.Fatalf("the change wrote version %d of the spec, want 2", number)
	}
	if _, err := joinedReplaceText(service, spec, approvedText, alice); err != nil {
		t.Fatalf("write the approved text again: %v", err)
	}
	restored := namedVersionWithItsEvent(t, service, "PRE-1", spec, "The approved text again")
	if restored != 3 || storedVersionMarkdown(t, database, spec, restored) != approvedText {
		t.Fatalf("the restore wrote version %d storing %q, want version 3 storing %q", restored, storedVersionMarkdown(t, database, spec, restored), approvedText)
	}

	var reviews, approvals int
	if err := database.Pool.QueryRow(ctx, `
		select (select count(*) from artifact_reviews where artifact_id = $1 and version = $2),
		       (select count(*) from events where type = 'artifact.approved' and payload->>'artifact_id' = $1::text and (payload->>'version')::int = $2)`,
		spec, restored).Scan(&reviews, &approvals); err != nil {
		t.Fatalf("count the reviews of version %d: %v", restored, err)
	}
	if reviews != 1 || approvals != 1 {
		t.Fatalf("version %d holds %d reviews and %d artifact.approved events, want the one carry its write's batch made: the write's own pass carried again", restored, reviews, approvals)
	}
	order := readCarriedOrder(t, database, "the restore's carry", spec, restored)
	requireApprovalFirst(t, "restore", order)
	if order.reviewDigest == nil || *order.reviewDigest != reviewTextHash(approvedText) {
		t.Errorf("the carry to version %d pins %v, want the sha256 of its stored markdown, %s", restored, order.reviewDigest, reviewTextHash(approvedText))
	}
	var record, repinned *string
	var owed bool
	if err := database.Pool.QueryRow(ctx, `
		select r.record_sha256, r.digest_owed, (select c.record_sha256 from artifact_review_carries c where c.review_id = $3::uuid)
		from version_renders r where r.artifact_id = $1 and r.number = $2`, spec, restored, order.review).Scan(&record, &owed, &repinned); err != nil {
		t.Fatalf("read the row of version %d and its carry's: %v", restored, err)
	}
	switch {
	case record == nil || owed:
		t.Errorf("version %d's row holds record digest %v, owed %t, after its write committed: want the fill to have recorded it", restored, record, owed)
	case repinned == nil || *repinned != *record:
		t.Errorf("the carry %s holds record_sha256 %v on its row of artifact_review_carries, want version %d's record digest %s: the fill did not repin it",
			order.review, repinned, restored, *record)
	case *record == reviewTextHash(approvedText):
		t.Errorf("version %d's record digest is the sha256 of its stored markdown, %s, want its record's", restored, *record)
	}
	if after := readCarriedOrder(t, database, "the restore's carry after the fill", spec, restored); after.review != order.review ||
		after.reviewDigest == nil || *after.reviewDigest != reviewTextHash(approvedText) {
		t.Errorf("after the fill the carry to version %d is %s pinning %v, want %s still pinning %s", restored, after.review, after.reviewDigest, order.review, reviewTextHash(approvedText))
	}
}
