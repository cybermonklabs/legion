package docs

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The record carry (R14): a version whose record digest equals the approved version's carries the
// approval, decided in the write's read phase (recordUnmoved), and the fill holds a record carry to
// its claim (fillOwedDigests).

// recordSpec is the spec the record carry's tests write into the spec's fragment of an issue room:
// a criterion and a requirement, which the spec query takes, then a paragraph it does not take, in
// that order of the text and of the tree. The criterion is the issue's first child, and no callout,
// so the spec has no memo.
const recordSpec = ":::criterion{#k1}\nI want the urgent list.\n:::\n\n:::requirement{#r1}\nAn urgent ask comes first.\n:::\n\nA paragraph after it.\n"

// recordParagraph is the opening of the paragraph's text value in recordSpec.
const recordParagraph = "A paragraph after it."

// withRecordCarryForced marks ctx so the record carry of a write answers that its version's record
// is the approved one without weighing the write's facts (recordCarryForcedKey): the test's way to
// make the fill's backstop fire. No route can put it on a context.
func withRecordCarryForced(ctx context.Context) context.Context {
	return context.WithValue(ctx, recordCarryForcedKey{}, true)
}

// recordRoom is an issue room whose spec holds recordSpec, written by a peer's edit and settled, at
// version approved, which alice approved once the settlement's fill recorded its record's digest
// (approveStoredVersion pins the digest artifact_version_text_sha256 answers).
type recordRoom struct {
	database *store.Store
	service  *Service
	peer     *docstest.Peer
	room     issueRoom
	approved int
}

// approvedRecordRoom writes the issue DOC-<number>'s room by SQL (createIssueRoom), its query nodes
// (writeRoomQueryNodes), recordSpec into its spec through a peer's edit, settled, and alice's
// approval of the version that settlement wrote, whose row holds the record's digest
// (requireStatedRow).
func approvedRecordRoom(t *testing.T, number int) recordRoom {
	t.Helper()
	database := storetest.Open(t)
	service, server := newTestServiceInstance(t, database)
	room := createIssueRoom(t, database, number)
	writeRoomQueryNodes(t, database, room)
	peer := connectPeer(t, server.URL, room.spec, "alice")
	if err := peer.WaitForText(fragmentMarkdown(room.plan), planSeed, roomWait(t)); err != nil {
		t.Fatalf("a peer's copy of %s's room: %v", room.issue, err)
	}
	written := recordRoom{database: database, service: service, peer: peer, room: room}
	spec, err := pmdoc.Parse(recordSpec)
	if err != nil {
		t.Fatalf("parse the spec: %v", err)
	}
	written.settleSpecEdit(t, context.Background(), func(*pmdoc.Node) (*pmdoc.Node, error) { return spec, nil })
	written.approved = savedVersion(t, database, room.spec)
	requireStatedRow(t, database, room.issue, graphstore.QuerySpec, room.spec, written.approved)
	approveStoredVersion(t, database, room.spec, written.approved, "alice")
	return written
}

// settleSpecEdit has the peer write into the spec's fragment the tree edit makes of the one the
// fragment holds, in one transaction of the peer's, waits for the room to store it, and settles the
// room under ctx (settleLandedWrite), whose settlement writes the spec's version, carries an
// approval to it, and fills its owed digest once it has committed. A fragment that holds nothing
// yet, which reads no tree, hands edit nil.
func (r recordRoom) settleSpecEdit(t *testing.T, ctx context.Context, edit func(*pmdoc.Node) (*pmdoc.Node, error)) {
	t.Helper()
	stored := storedUpdateCount(t, r.database, r.room.tree)
	fragment := r.peer.Doc.GetXmlFragment(r.room.spec)
	var want string
	var failed error
	if _, err := r.peer.Send(func(txn *crdt.Transaction) {
		var tree *pmdoc.Node
		if fragment.Len() > 0 {
			if tree, failed = pmdoc.ReadInTransaction(txn, fragment); failed != nil {
				return
			}
		}
		tree, err := edit(tree)
		if err == nil {
			want, err = pmdoc.Render(tree)
		}
		if err == nil {
			err = pmdoc.Update(txn, fragment, tree)
		}
		failed = err
	}); err != nil || failed != nil {
		t.Fatalf("write the spec of %s: %v, %v", r.room.issue, err, failed)
	}
	if err := r.peer.WaitForText(fragmentMarkdown(r.room.spec), want, roomWait(t)); err != nil {
		t.Fatalf("a peer's copy of %s's spec: %v", r.room.issue, err)
	}
	settleLandedWrite(t, ctx, r.database, r.service, r.room, stored)
}

// moveParagraphBeforeRequirement is recordSpec's paragraph moved in the text before the
// requirement: the criterion, the paragraph, the requirement. The paragraph moves forward, so it
// takes the new text_rank and the requirement keeps its own (R1), and the spec's record does not
// move (R14).
func moveParagraphBeforeRequirement(tree *pmdoc.Node) (*pmdoc.Node, error) {
	if len(tree.Children) != 3 || tree.Children[2].Type != "paragraph" {
		return nil, fmt.Errorf("the spec holds %d top-level blocks, want the criterion, the requirement and the paragraph", len(tree.Children))
	}
	tree.Children[1], tree.Children[2] = tree.Children[2], tree.Children[1]
	return tree, nil
}

// replaceWords sets the text of the first text run inside the block of tree whose id is id to
// words, and fails where tree holds no such block.
func replaceWords(tree *pmdoc.Node, id, words string) error {
	block := blockByID(tree, id)
	if block == nil {
		return fmt.Errorf("the tree holds no block %s", id)
	}
	if run := firstTextRun(block); run != nil {
		run.Text = words
		return nil
	}
	return fmt.Errorf("the block %s holds no text", id)
}

// firstTextRun is the first text node under node, nil where none is.
func firstTextRun(node *pmdoc.Node) *pmdoc.Node {
	if node.Type == "text" {
		return node
	}
	for _, child := range node.Children {
		if run := firstTextRun(child); run != nil {
			return run
		}
	}
	return nil
}

// recordVersionDigest is the digest artifact_version_text_sha256 answers for version number of
// artifactID: the record's digest where the version's row holds one.
func recordVersionDigest(t *testing.T, database *store.Store, artifactID string, number int) string {
	t.Helper()
	var digest *string
	if err := database.Pool.QueryRow(context.Background(), `select artifact_version_text_sha256($1::uuid, $2)`, artifactID, number).Scan(&digest); err != nil || digest == nil {
		t.Fatalf("read the digest version %d of %s answers (%v): %v", number, artifactID, digest, err)
	}
	return *digest
}

// heldRecordCarry is a carried review of a version as readRecordCarry reads it: its reason, the
// version its row of artifact_review_carries names, and the record digest that row holds, nil where
// the fill repinned none.
type heldRecordCarry struct {
	reason string
	from   int
	record *string
}

// readRecordCarry reads the newest carried review of version number of artifactID, failing where
// none stands.
func readRecordCarry(t *testing.T, database *store.Store, artifactID string, number int) heldRecordCarry {
	t.Helper()
	var carry heldRecordCarry
	var reason *string
	if err := database.Pool.QueryRow(context.Background(), `
		select r.reason, c.carried_from_version, c.record_sha256
		from artifact_reviews r join artifact_review_carries c on c.review_id = r.id
		where r.artifact_id = $1 and r.version = $2
		order by r.created_at desc, r.id desc limit 1`, artifactID, number).Scan(&reason, &carry.from, &carry.record); err != nil {
		t.Fatalf("read the carried review of version %d of %s: %v", number, artifactID, err)
	}
	if reason != nil {
		carry.reason = *reason
	}
	return carry
}

// carriedReviewCount is how many carried reviews version number of artifactID holds.
func carriedReviewCount(t *testing.T, database *store.Store, artifactID string, number int) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from artifact_reviews r join artifact_review_carries c on c.review_id = r.id
		where r.artifact_id = $1 and r.version = $2`, artifactID, number).Scan(&count); err != nil {
		t.Fatalf("count the carried reviews of version %d of %s: %v", number, artifactID, err)
	}
	return count
}

// requireRecordCarry fails, naming the row, unless version number of artifactID holds a record
// carry of the approval of version approved (RecordCarryReason, its row of artifact_review_carries
// naming approved) written before the version, and the version's digest is the approved one's.
func requireRecordCarry(t *testing.T, database *store.Store, row, artifactID string, approved, number int) {
	t.Helper()
	if got, want := recordVersionDigest(t, database, artifactID, number), recordVersionDigest(t, database, artifactID, approved); got != want {
		t.Fatalf("the %s row's version %d of %s answers the digest %s, want the approved version %d's, %s", row, number, artifactID, got, approved, want)
	}
	carry := readRecordCarry(t, database, artifactID, number)
	if carry.reason != RecordCarryReason(approved) || carry.from != approved {
		t.Errorf("the %s row's carried review of version %d reads the reason %q carried from version %d, want %q carried from version %d",
			row, number, carry.reason, carry.from, RecordCarryReason(approved), approved)
	}
	requireApprovalFirst(t, row, readCarriedOrder(t, database, "the "+row+" carry", artifactID, number))
}

// blockAddressByText is the address of the block whose newest text value opens with opening.
func blockAddressByText(t *testing.T, database *store.Store, opening string) string {
	t.Helper()
	var address string
	if err := database.Pool.QueryRow(context.Background(), `
		select t.from_id from graph_tuples t
		where t.from_kind = $1 and t.kind = $2 and t.to_kind = $3 and t.asserted and starts_with(t.to_id, $4)
		order by t.seq desc limit 1`, refs.BlockNodeKind, graphstore.AttributeText, graphstore.KindValue, opening).Scan(&address); err != nil {
		t.Fatalf("read the block whose text opens %q: %v", opening, err)
	}
	return address
}

// TestTheRecordCarryBacksAnUnmovedRecordAndItsFillHoldsItToTheClaim holds the record carry's
// backstop (R14): the fill repins a record carry only where the version's record digest is the one
// its approval stood on (fillOwedDigests). The forced rule (withRecordCarryForced) carries alice's
// approval of DOC-7's spec across a settlement that edits a word of its requirement, which moves
// the record. The carried review stands at the new version with the record carry's reason, its row
// of artifact_review_carries holds no record digest once the fill has run, the fill logs an error
// naming the document, the version and both digests, and the approval reads stale at the new
// version (NamedDocumentsApproved). Without the backstop the fill would repin the carry and the
// approval would read approved over a record nobody approved. It captures the default logger, so
// it runs alone.
func TestTheRecordCarryBacksAnUnmovedRecordAndItsFillHoldsItToTheClaim(t *testing.T) {
	logs := captureLogs(t)
	written := approvedRecordRoom(t, 7)
	spec := written.room.spec
	written.settleSpecEdit(t, withRecordCarryForced(context.Background()), func(tree *pmdoc.Node) (*pmdoc.Node, error) {
		return tree, replaceWords(tree, "r1", "An urgent ask comes last.")
	})
	number := savedVersion(t, written.database, spec)
	if number != written.approved+1 {
		t.Fatalf("the settlement of the requirement's edit left version %d newest, want %d", number, written.approved+1)
	}
	approvedDigest, digest := recordVersionDigest(t, written.database, spec, written.approved), recordVersionDigest(t, written.database, spec, number)
	if digest == approvedDigest {
		t.Fatalf("version %d, whose requirement's word moved, answers the approved digest %s: the test needs a moved record", number, digest)
	}
	carry := readRecordCarry(t, written.database, spec, number)
	if carry.reason != RecordCarryReason(written.approved) || carry.from != written.approved {
		t.Fatalf("the forced carry to version %d reads the reason %q from version %d, want %q from version %d",
			number, carry.reason, carry.from, RecordCarryReason(written.approved), written.approved)
	}
	if carry.record != nil {
		t.Errorf("the fill repinned the record carry to version %d on %s, want its row of artifact_review_carries left with no record digest: its record is not the approved one", number, *carry.record)
	}
	logged := logs.String()
	for _, want := range []string{"a record carry's version does not digest to the record its approval stood on", spec, "version=" + strconv.Itoa(number), digest, approvedDigest} {
		if !strings.Contains(logged, want) {
			t.Errorf("the fill's log holds no %q:\n%s", want, logged)
		}
	}
	approved, err := NamedDocumentsApproved(context.Background(), written.database.Pool,
		[]model.AskApprovalDocument{{ArtifactID: spec, Version: number}}, "", false)
	if err != nil || approved {
		t.Errorf("NamedDocumentsApproved reads version %d of %s approved %t (%v), want stale: the carry stands on the stored markdown's digest, and the version answers its record's", number, spec, approved, err)
	}
}

// statePlacedParagraph gives the spec's paragraph of written a text_rank between the criterion's
// and the requirement's, straight into the store, as no version write states it, and marks the
// issue's queries owed in the same transaction (QueueQueryMark): the spec's rendering moves and its
// record does not, since the spec query takes no paragraph.
func statePlacedParagraph(t *testing.T, written recordRoom, rank string) {
	t.Helper()
	ctx := context.Background()
	paragraph := blockAddressByText(t, written.database, recordParagraph)
	if err := pgx.BeginFunc(ctx, written.database.Pool, func(tx pgx.Tx) error {
		var at time.Time
		if err := tx.QueryRow(ctx, `select clock_timestamp()`).Scan(&at); err != nil {
			return err
		}
		if err := graphstore.Write(ctx, tx, nil, []graphstore.Tuple{{
			FromKind: refs.BlockNodeKind, FromID: paragraph, Kind: refs.AttributeTextRank, ToKind: graphstore.KindValue, ToID: rank,
			At: at, Who: model.Actor{Kind: "user", ID: "alice"}, SourceEvent: graphstore.NoEvent, Asserted: true,
		}}); err != nil {
			return err
		}
		var marked bool
		return store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
			QueueQueryMark(b, written.room.issue, graphstore.NoEvent, &marked)
			return nil
		})
	}); err != nil {
		t.Fatalf("place the paragraph of %s's spec at the text_rank %s: %v", written.room.issue, rank, err)
	}
}

// textRankOf is the newest text_rank value the block at address holds.
func textRankOf(t *testing.T, database *store.Store, address string) string {
	t.Helper()
	var rank string
	if err := database.Pool.QueryRow(context.Background(), `
		select t.to_id from graph_tuples t
		where t.from_kind = $1 and t.from_id = $2 and t.kind = $3 and t.to_kind = $4 and t.asserted
		order by t.at desc, t.seq desc limit 1`, refs.BlockNodeKind, address, refs.AttributeTextRank, graphstore.KindValue).Scan(&rank); err != nil {
		t.Fatalf("read the text_rank of %s: %v", address, err)
	}
	return rank
}
