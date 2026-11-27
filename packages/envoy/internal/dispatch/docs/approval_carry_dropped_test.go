package docs

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The texts the dropped-carry test writes the spec of DRP-1 in: a criterion, then the criterion
// with another sentence, which the restore takes back.
const (
	droppedCarryApproved = ":::criterion{#k1}\nI want every write held to its writer.\n:::\n"
	droppedCarryChanged  = ":::criterion{#k1}\nI want every write held to its writer, and each refusal named.\n:::\n"
)

// droppedCarryRestore is what the restore of the approved text wrote over a record: the spec, the
// request the restore moved, the digest the approval pinned, the version the restore wrote, the
// ledger of its transaction and the types of the events its commit published, in the order it
// published them.
type droppedCarryRestore struct {
	database  *store.Store
	issue     string
	spec      string
	request   string
	pin       string
	restored  int
	ledger    *Ledger
	published []string
}

// restoreOverTheRecord writes, on a database of its own, the spec of DRP-1, a document whose
// versions are a query's (writeQueryNode), through the real version write: version 2 holds a
// criterion, which alice approves, so her review pins the digest of that version's record; version
// 3 words the criterion again, and the agent opens an approval request over it; and version 4
// restores version 2's text, which is the restore carryRestored carries alice's approval to. Where
// moved is set the criterion's node is written overtaken before version 4, so version 4's record is
// not the one alice approved though its text is. It subscribes to the document service's events
// before version 4, which the restore's transaction publishes once it has committed.
//
// The spec's record at the head is read three times: at version 2, where it is alice's pin; at
// version 3, where it is not, since the criterion's words are in it (a block's text value is its
// whole slice of the version's markdown, refs' AttributeText, and the spec takes the criterion);
// and at version 4, where it is her pin again unless the criterion's state moved. The last is what
// the carry is weighed by, so where it is not as moved says, the test proves nothing.
func restoreOverTheRecord(t *testing.T, moved bool) droppedCarryRestore {
	t.Helper()
	ctx := context.Background()
	database := storetest.Open(t)
	service := newTestServiceOn(t, database)
	service.settle = time.Hour
	const issue = "DRP-1"
	var spec string
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('DRP', 'Dropped carry', 'alice');
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('DRP-1', 'DRP', 1, 'A dropped carry', '{"kind":"user","id":"alice"}', 'U')`); err != nil {
		t.Fatalf("seed project DRP: %v", err)
	}
	writeQueryNode(t, database, "DRP", issue, "spec", graphstore.QuerySpec)
	if err := database.Pool.QueryRow(ctx, `
		with spec as (
			insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
			values ('DRP-1', 'DRP', 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"alice"}')
			returning id
		)
		insert into artifact_versions (artifact_id, number, markdown, authors)
		select id, 1, 'Before the criterion.' || chr(10), '[{"kind":"user","id":"alice"}]' from spec
		returning artifact_id::text`).Scan(&spec); err != nil {
		t.Fatalf("write the spec of DRP-1: %v", err)
	}
	refKey := refKeyOf(t, database, spec)
	criterion := refs.BlockNodeKind + ":" + refKey + "#k1"
	alice := model.Actor{Kind: "user", ID: "alice"}

	seedServiceText(t, service, spec, droppedCarryApproved)
	if number := namedVersionWithItsEvent(t, service, issue, spec, "The approved version"); number != 2 {
		t.Fatalf("the approved text is version %d of the spec, want 2", number)
	}
	requireStatedRow(t, database, issue, graphstore.QuerySpec, spec, 2)
	approved := storedVersionMarkdown(t, database, spec, 2)
	approveStoredVersion(t, database, spec, 2, "alice")
	var pin string
	if err := database.Pool.QueryRow(ctx, `select content_sha256 from artifact_reviews where artifact_id = $1 and version = 2`, spec).Scan(&pin); err != nil {
		t.Fatalf("read the digest alice's approval pins: %v", err)
	}
	requireHeadRecord := func(what string, want bool) {
		t.Helper()
		record, err := ReadQueryRecord(ctx, database.Pool, issue, graphstore.QuerySpec, refs.TreeCut{})
		if err != nil {
			t.Fatalf("read the spec's record %s: %v", what, err)
		}
		if !slices.ContainsFunc(record.Nodes, func(node TreeNode) bool { return node.Address() == criterion }) {
			t.Fatalf("the spec's record %s holds %v, want it to hold the criterion %s: writing its state would move nothing, and this test proves nothing",
				what, walkNodeAddresses(record.Nodes), criterion)
		}
		if (record.Digest == pin) != want {
			t.Fatalf("the spec's record %s digests to %s and alice's approval pins %s, want the two %s: this test proves nothing", what, record.Digest, pin, map[bool]string{true: "equal", false: "different"}[want])
		}
	}
	requireHeadRecord("as approved", true)

	if _, err := joinedReplaceText(service, spec, droppedCarryChanged, alice); err != nil {
		t.Fatalf("word the criterion again: %v", err)
	}
	if number := namedVersionWithItsEvent(t, service, issue, spec, "The criterion worded again"); number != 3 {
		t.Fatalf("the criterion worded again is version %d of the spec, want 3", number)
	}
	request := openDroppedCarryRequest(t, database, issue, spec, 3)
	if moved {
		if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
			node, held, err := graphstore.ReadNode(ctx, tx, refs.BlockNodeKind, refKey+"#k1")
			if err != nil || !held {
				return fmt.Errorf("the criterion's node is not in the store (%t, %v)", held, err)
			}
			_, err = graphstore.WriteState(ctx, tx, []graphstore.Node{node}, graphstore.StateOvertaken, time.Now().UTC(), alice)
			return err
		}); err != nil {
			t.Fatalf("write the criterion overtaken: %v", err)
		}
	}
	requireHeadRecord("once version 3 words the criterion again", false)

	if _, err := joinedReplaceText(service, spec, approved, alice); err != nil {
		t.Fatalf("write the approved text again: %v", err)
	}
	published, unsubscribe := service.events.Subscribe()
	defer unsubscribe()
	restore := droppedCarryRestore{database: database, issue: issue, spec: spec, request: request, pin: pin}
	if err := joinedWrite(service, func(ctx context.Context) error {
		restore.ledger = ledgerFrom(ctx)
		result, err := service.NamedVersion(ctx, spec, "The approved text again", orderAgent)
		if err != nil {
			return err
		}
		if !result.Wrote {
			return fmt.Errorf("the named version of %s wrote nothing", spec)
		}
		restore.restored = result.Version.Number
		return queueVersionEvent(ctx, service, issue, spec, result.Version, result.Blocks, orderAgent)
	}); err != nil {
		t.Fatalf("write the restore of the approved text: %v", err)
	}
	if restore.restored != 4 || storedVersionMarkdown(t, database, spec, 4) != approved {
		t.Fatalf("the restore wrote version %d storing %q, want version 4 storing the approved %q", restore.restored, storedVersionMarkdown(t, database, spec, 4), approved)
	}
	for drained := false; !drained; {
		select {
		case event := <-published:
			restore.published = append(restore.published, event.Type)
		default:
			drained = true
		}
	}
	requireHeadRecord("once version 4 restores the approved text over the criterion's state moved or stood", !moved)
	return restore
}

// walkNodeAddresses is the store address of each of nodes, for a failure line.
func walkNodeAddresses(nodes []TreeNode) []string {
	addresses := make([]string, len(nodes))
	for index, node := range nodes {
		addresses[index] = node.Address()
	}
	return addresses
}

// openDroppedCarryRequest stores an open approval request of issue over version of the document
// artifactID, waiting on the person, as the approval route writes one, and answers its id.
func openDroppedCarryRequest(t *testing.T, database *store.Store, issue, artifactID string, version int) string {
	t.Helper()
	documents := []model.AskApprovalDocument{{ArtifactID: artifactID, Name: "spec.md", Version: version, RequestedVersion: version}}
	approval, err := EncodeApproval(&model.AskApproval{Documents: documents})
	if err != nil {
		t.Fatal(err)
	}
	var id string
	if err := database.Pool.QueryRow(context.Background(), `
		insert into asks (issue_key, author, question, options, urgency, kind, approval)
		values ($1, '{"kind":"session","id":"asker"}', $2, '[{"label":"Approve"},{"label":"Request changes"}]', 'high', 'approval', $3)
		returning id::text
	`, issue, ApprovalQuestion(documents, "Ships it."), approval).Scan(&id); err != nil {
		t.Fatalf("store the approval request: %v", err)
	}
	return id
}

// requestOf is the state of the approval request id, the version its document is moved to and the
// version it was requested at.
func requestOf(t *testing.T, database *store.Store, id string) (state string, version, requested int) {
	t.Helper()
	if err := database.Pool.QueryRow(context.Background(), `
		select state, (approval -> 'documents' -> 0 ->> 'version')::int, (approval -> 'documents' -> 0 ->> 'requested_version')::int
		from asks where id = $1
	`, id).Scan(&state, &version, &requested); err != nil {
		t.Fatalf("read the approval request %s: %v", id, err)
	}
	return state, version, requested
}

// TestADroppedCarryPublishesNothingAndLeavesTheRequestOpen holds R14's rollback to the ledger it
// keeps: the restore of the approved text carries alice's approval behind a savepoint, and where
// the version's record is not the one she approved - the criterion the approval covers was written
// overtaken since - the write phase's fill finds another digest than the carry pinned, rolls the
// transaction back to the savepoint and writes the version with no carry (Ledger.writeRestored).
// The carry then publishes no event - no artifact.approved and no ask.answered, though the
// version's own artifact.version and the move of the request's ask.edited stand - writes no review
// and no row of artifact_review_carries, and leaves the approval request open, moved to the version
// the restore wrote and still requested at the version the agent opened it over, as the write
// phase's own report of the request (Ledger.WrittenApproval) and of the review (WrittenReview) say.
// The version's row records its own record's digest, not the one alice's approval pinned.
//
// The control is the same restore over the record alice approved: the carry stands, its
// artifact.approved published before the version's artifact.version, and the request answered. It
// is what makes the first case read a drop and not a restore that never carried.
func TestADroppedCarryPublishesNothingAndLeavesTheRequestOpen(t *testing.T) {
	t.Parallel()
	t.Run("a record that moved since the approval", func(t *testing.T) {
		restore := restoreOverTheRecord(t, true)
		ctx := context.Background()
		for _, kind := range []string{"artifact.approved", "ask.answered"} {
			if slices.Contains(restore.published, kind) {
				t.Errorf("the dropped carry's transaction published %s, want no event of the carry: published %v", kind, restore.published)
			}
		}
		for _, kind := range []string{"artifact.version", "ask.edited"} {
			if !slices.Contains(restore.published, kind) {
				t.Errorf("the restore's transaction published %v, want the %s its version and the move of its request stand on", restore.published, kind)
			}
		}
		var reviews, carries, approvals, answers int
		if err := restore.database.Pool.QueryRow(ctx, `
			select (select count(*) from artifact_reviews where artifact_id = $1 and version = $2),
			       (select count(*) from artifact_review_carries c join artifact_reviews r on r.id = c.review_id where r.artifact_id = $1),
			       (select count(*) from events where type = 'artifact.approved' and payload->>'artifact_id' = $1::text),
			       (select count(*) from events where type = 'ask.answered')
		`, restore.spec, restore.restored).Scan(&reviews, &carries, &approvals, &answers); err != nil {
			t.Fatalf("count what the dropped carry left: %v", err)
		}
		if reviews != 0 || carries != 0 || approvals != 0 || answers != 0 {
			t.Errorf("the dropped carry left %d review(s) of version %d, %d carry row(s), %d artifact.approved event(s) and %d ask.answered event(s), want none of them",
				reviews, restore.restored, carries, approvals, answers)
		}
		if state, version, requested := requestOf(t, restore.database, restore.request); state != "open" || version != restore.restored || requested != 3 {
			t.Errorf("the approval request is %s at version %d, requested at %d, want it open, moved to version %d and still requested at version 3",
				state, version, requested, restore.restored)
		}
		if review, wrote := restore.ledger.WrittenReview(restore.spec); wrote {
			t.Errorf("the write phase reports the review %+v of version %d, want none: the dropped carry's review is off the ledger", review, restore.restored)
		}
		ask, wrote := restore.ledger.WrittenApproval(restore.spec)
		document := (*model.AskApprovalDocument)(nil)
		if ask.Approval != nil {
			document = ask.Approval.Document(restore.spec)
		}
		if !wrote || ask.ID != restore.request || ask.State != "open" || document == nil || document.Version != restore.restored || document.RequestedVersion != 3 {
			t.Errorf("the write phase reports the request %+v (written %t), want %s open at version %d, requested at 3: the dropped carry's answer is off the ledger",
				ask, wrote, restore.request, restore.restored)
		}
		requireStatedRow(t, restore.database, restore.issue, graphstore.QuerySpec, restore.spec, restore.restored)
		var digest string
		if err := restore.database.Pool.QueryRow(ctx, `
			select record_sha256 from version_renders where artifact_id = $1 and number = $2
		`, restore.spec, restore.restored).Scan(&digest); err != nil {
			t.Fatalf("read the row of version %d: %v", restore.restored, err)
		}
		if digest == restore.pin {
			t.Errorf("version %d records the digest %s alice's approval pinned, want the digest of its own record, which holds the criterion overtaken", restore.restored, digest)
		}
	})

	t.Run("the record the person approved", func(t *testing.T) {
		restore := restoreOverTheRecord(t, false)
		approved, version := slices.Index(restore.published, "artifact.approved"), slices.Index(restore.published, "artifact.version")
		if approved < 0 || version < 0 || approved > version {
			t.Errorf("the restore published %v, want artifact.approved before artifact.version", restore.published)
		}
		if !slices.Contains(restore.published, "ask.answered") {
			t.Errorf("the restore published %v, want the ask.answered of the request its carry answers", restore.published)
		}
		var review *string
		if err := restore.database.Pool.QueryRow(context.Background(), `
			select content_sha256 from artifact_reviews where artifact_id = $1 and version = $2
		`, restore.spec, restore.restored).Scan(&review); err != nil || review == nil || *review != restore.pin {
			t.Errorf("version %d holds a carried review pinning %v (%v), want the digest alice's approval pinned, %s", restore.restored, review, err, restore.pin)
		}
		if state, _, _ := requestOf(t, restore.database, restore.request); state != "answered" {
			t.Errorf("the approval request is %s, want it answered by the carry that stood", state)
		}
		if _, wrote := restore.ledger.WrittenReview(restore.spec); !wrote {
			t.Errorf("the write phase reports no review of version %d, want the carry's", restore.restored)
		}
	})
}

// askRecordSpec is recordSpec with an open ask after its paragraph: the spec query takes the
// criterion and the requirement, and no ask. Bob answers the ask once alice has approved the spec,
// and the version his answer settles into differs from the approved one in the ask block's
// server-owned attributes alone (attributeRemovalCase).
const askRecordSpec = recordSpec + "\n:::ask{#ask-1 urgency=\"med\" multiple=\"false\"}\nShip it?\n\n- Yes: Now.\n- No: Later.\n:::\n"

// treeCriterion is a criterion written into the tree artifact's own fragment of an issue room, as
// the edits route on the tree writes one: the spec query takes it wherever the room holds it, so it
// moves the spec's record while the spec's own text holds none of it.
const treeCriterion = ":::criterion{#k2}\nI want the urgent list quickly.\n:::\n"

// writeTreeCriterion writes treeCriterion into the tree artifact of written's room, with its
// version and the version's event, in one transaction in alice's name, as the edits route writes an
// edit of the tree (api's TestACriterionTheEditRouteWritesStalesTheSpecsApproval): the version's
// pass over the room states the criterion and marks the issue's queries owed (markRoomQueries), and
// the commit drains the mark (Ledger.Commit), whose version of the spec keeps the spec's bytes and
// records the moved record's digest. It answers the address of the criterion's node.
func writeTreeCriterion(t *testing.T, written recordRoom) string {
	t.Helper()
	service, room := written.service, written.room
	alice := model.Actor{Kind: "user", ID: "alice"}
	base := liveDocumentToken(t, service, room.tree)
	if err := joinedWrite(service, func(ctx context.Context) error {
		if _, err := service.ReplaceText(ctx, room.tree, treeCriterion, alice, base); err != nil {
			return err
		}
		result, err := service.SnapshotVersion(ctx, room.tree, alice)
		if err != nil {
			return err
		}
		if !result.Wrote {
			return fmt.Errorf("the snapshot of the tree %s wrote no version", room.tree)
		}
		return queueVersionEvent(ctx, service, room.issue, room.tree, result.Version, result.Blocks, alice)
	}); err != nil {
		t.Fatalf("write the criterion into the tree of %s: %v", room.issue, err)
	}
	return refs.BlockNodeKind + ":" + refKeyOf(t, written.database, room.tree) + "#k2"
}

// answerAskAsBob has bob answer the ask of the spec's block ask-1 in written's room as the answer
// route writes it (settleAskAs): the ask is read under the room's tree, which its row is keyed by,
// and its block is the spec's, the document AskColumns names for it. The room's settlement then
// writes the version his answer settles into, whose ask block holds the state, answerer and time a
// version keeps (keptAskState), and nothing else new.
func answerAskAsBob(t *testing.T, written recordRoom) {
	t.Helper()
	service, room := written.service, written.room
	ask := blockAskOn(t, service, room.tree, "ask-1")
	bob := model.Actor{Kind: "user", ID: "bob"}
	at := time.Now().UTC()
	change := asks.Answered(model.AskAnswer{User: "bob", Selected: []string{"Yes"}, At: at})
	change.Who, change.At = bob, at
	settleAskAs(t, service, room.spec, "ask-1", ask.ID, bob, change)
	settleCurrentGeneration(t, service, room.tree)
}

// requireAskAttributesAlone fails unless version number of the spec of written differs from the
// version alice approved in the opener of ask-1 alone, which shows bob's answer: a difference
// attributeRemovalCase carries across on its own.
func requireAskAttributesAlone(t *testing.T, written recordRoom, number int) {
	t.Helper()
	approved := storedVersionMarkdown(t, written.database, written.room.spec, written.approved)
	version := storedVersionMarkdown(t, written.database, written.room.spec, number)
	opener := askOpeningLine(version, "ask-1")
	if !strings.HasPrefix(opener, `:::ask{#ask-1 urgency="med" multiple="false" state="answered" answered_by="bob" answered_at="`) ||
		strings.Replace(version, opener, askOpeningLine(approved, "ask-1"), 1) != approved {
		t.Fatalf("version %d of the spec of %s reads\n%s\nwant version %d's text with bob's answer in the opener of ask-1 alone:\n%s",
			number, written.room.issue, version, written.approved, approved)
	}
}

// specApprovedAt is what the gate reads of the spec of written at version number: whether its
// newest review stands approved (NamedDocumentsApproved), which for a query document of a converted
// issue compares the digest that review stands on with the current query's.
func specApprovedAt(t *testing.T, written recordRoom, number int) bool {
	t.Helper()
	approved, err := NamedDocumentsApproved(context.Background(), written.database.Pool,
		[]model.AskApprovalDocument{{ArtifactID: written.room.spec, Version: number}}, "", false)
	if err != nil {
		t.Fatalf("read whether the spec of %s stands approved at version %d: %v", written.room.issue, number, err)
	}
	return approved
}

// TestAnAskAttributeVersionAfterAMovedRecordCarriesNoApproval holds R14's carry to the record the
// person approved: an approval carries only where that record is unmoved. Alice approves the spec
// of a converted issue holding a criterion, a requirement, a paragraph and an open ask. An agent
// then writes a criterion into the tree artifact of the issue's room, which the spec query takes:
// the drain writes the next version of the spec, holding the approved bytes and the moved record's
// digest, and carries nothing. Bob then answers the ask, and the room's settlement writes the
// version after that, which differs from the approved one in the ask block's server-owned
// attributes alone - a difference attributeRemovalCase carries on its own - and whose record is
// the moved one. That version carries nothing either: no review stands at it, the newest review is
// still alice's own approval of the version she approved, and the gate reads the spec as changed
// since. The attribute case compared the version with the approved one by its text alone, so it
// carried her approval to it, and the fill repinned that carry to the moved record: the gate read
// approved a criterion nobody approved.
//
// The control is the same answer over the record alice approved: the settlement's version carries
// her approval by the attribute case, its artifact.approved before the version's artifact.version,
// and the gate reads the spec approved.
func TestAnAskAttributeVersionAfterAMovedRecordCarriesNoApproval(t *testing.T) {
	t.Parallel()
	t.Run("a record that moved since the approval", func(t *testing.T) {
		written := approvedRecordRoomOf(t, 10, askRecordSpec)
		database, spec := written.database, written.room.spec
		approval, err := NewestReview(context.Background(), database.Pool, spec)
		if err != nil || approval == nil {
			t.Fatalf("read alice's approval of the spec of %s: %+v, %v", written.room.issue, approval, err)
		}
		pin := recordVersionDigest(t, database, spec, written.approved)

		criterion := writeTreeCriterion(t, written)
		record, err := ReadQueryRecord(context.Background(), database.Pool, written.room.issue, graphstore.QuerySpec, refs.TreeCut{})
		if err != nil {
			t.Fatalf("read the spec's record of %s: %v", written.room.issue, err)
		}
		if !slices.ContainsFunc(record.Nodes, func(node TreeNode) bool { return node.Address() == criterion }) || record.Digest == pin {
			t.Fatalf("the spec's record of %s holds %v at the digest %s once the tree holds %s, and alice's approval pins %s: want the criterion in it and the digest moved, or this test proves nothing",
				written.room.issue, walkNodeAddresses(record.Nodes), record.Digest, criterion, pin)
		}
		moved := savedVersion(t, database, spec)
		if moved != written.approved+1 || storedVersionMarkdown(t, database, spec, moved) != storedVersionMarkdown(t, database, spec, written.approved) ||
			recordVersionDigest(t, database, spec, moved) != record.Digest {
			t.Fatalf("the spec of %s holds version %d newest after the criterion's write, digesting to %s, want the drain's version %d holding the approved bytes and the moved record's digest %s",
				written.room.issue, moved, recordVersionDigest(t, database, spec, moved), written.approved+1, record.Digest)
		}
		if count := carriedReviewCount(t, database, spec, moved); count != 0 {
			t.Fatalf("the drain's version %d of the spec holds %d carried reviews, want none: its record moved", moved, count)
		}

		answerAskAsBob(t, written)
		latest := savedVersion(t, database, spec)
		if latest != moved+1 {
			t.Fatalf("the settlement of bob's answer left version %d of the spec newest, want %d", latest, moved+1)
		}
		requireAskAttributesAlone(t, written, latest)
		if digest := recordVersionDigest(t, database, spec, latest); digest != record.Digest {
			t.Fatalf("version %d of the spec digests to %s, want the moved record's %s: bob's answer moves no record of the spec", latest, digest, record.Digest)
		}

		if count := carriedReviewCount(t, database, spec, latest); count != 0 {
			carry := readRecordCarry(t, database, spec, latest)
			t.Errorf("version %d of the spec holds %d carried reviews, the newest reading %q carried from version %d, want none: its record holds the criterion %s, which alice never approved",
				latest, count, carry.reason, carry.from, criterion)
		}
		if newest, err := NewestReview(context.Background(), database.Pool, spec); err != nil || newest == nil || newest.ID != approval.ID || newest.Version != written.approved {
			version := 0
			if newest != nil {
				version = newest.Version
			}
			t.Errorf("the newest review of the spec is a review of version %d (%v), want alice's own approval %s of version %d: the approval stays at the version she approved",
				version, err, approval.ID, written.approved)
		}
		if specApprovedAt(t, written, latest) {
			t.Errorf("the gate reads the spec of %s approved at version %d, want it changed since alice's approval of version %d: its record holds the criterion %s",
				written.room.issue, latest, written.approved, criterion)
		}
	})

	t.Run("the record the person approved", func(t *testing.T) {
		written := approvedRecordRoomOf(t, 11, askRecordSpec)
		database, spec := written.database, written.room.spec
		answerAskAsBob(t, written)
		latest := savedVersion(t, database, spec)
		if latest != written.approved+1 {
			t.Fatalf("the settlement of bob's answer left version %d of the spec newest, want %d", latest, written.approved+1)
		}
		requireAskAttributesAlone(t, written, latest)
		if got, want := recordVersionDigest(t, database, spec, latest), recordVersionDigest(t, database, spec, written.approved); got != want {
			t.Fatalf("version %d of the spec digests to %s, want the approved version's %s: bob's answer moves no record of the spec", latest, got, want)
		}
		if carry := readRecordCarry(t, database, spec, latest); carry.reason != attributeRemovalReason(written.approved) || carry.from != written.approved {
			t.Errorf("the review carried to version %d reads %q carried from version %d, want %q carried from version %d",
				latest, carry.reason, carry.from, attributeRemovalReason(written.approved), written.approved)
		}
		requireApprovalFirst(t, "the attribute carry", readCarriedOrder(t, database, "the attribute carry", spec, latest))
		if !specApprovedAt(t, written, latest) {
			t.Errorf("the gate reads the spec of %s changed since at version %d, want it approved: its record is the one alice approved", written.room.issue, latest)
		}
	})
}

// closeRecordRoom closes the issue of written by its row alone, a minute ahead, as the row reads
// to a process that has not heard the close: the issue reads closed from now on under every owner
// lock, and the store's close margin still admits a browser's update that the room takes before
// then (observedBeforeClose), the one window in which a closed issue's room takes an edit. The
// settlement of that edit writes its versions as a closed issue's, with no carry
// (versionWrite.closedIssue), and marks and drains the issue's queries as any settlement does.
func closeRecordRoom(t *testing.T, written recordRoom) {
	t.Helper()
	if _, err := written.database.Pool.Exec(context.Background(), `update issues set closed_at = now() + interval '1 minute' where key = $1`, written.room.issue); err != nil {
		t.Fatalf("close %s: %v", written.room.issue, err)
	}
}

// reopenRecordRoom reopens the issue of written as the API's reopen does (reopenTestIssue, for
// the issue of written): in one transaction joined with the service, it clears the issue's
// closed_at, marks owing a settlement each document whose ask blocks wait for the reopen
// (MarkReopenOwedAskDocuments) and follows the versions written while the issue was closed
// (FollowClosedIssueVersions), which carries a standing approval to each document's latest version
// on carryNewestApprovalNaming's conditions (carryToLatest). The reopen is then recorded on the
// service's rooms, as room sync records it.
func reopenRecordRoom(t *testing.T, written recordRoom) {
	t.Helper()
	service, issue := written.service, written.room.issue
	if err := joinedWrite(service, func(ctx context.Context) error {
		tx, _ := txFromContext(ctx)
		if _, err := tx.Exec(ctx, `update issues set closed_at = null where key = $1`, issue); err != nil {
			return fmt.Errorf("clear the closed_at of %s: %w", issue, err)
		}
		if err := service.MarkReopenOwedAskDocuments(ctx, issue); err != nil {
			return err
		}
		return service.FollowClosedIssueVersions(ctx, issue)
	}); err != nil {
		t.Fatalf("reopen %s and follow its closed versions: %v", issue, err)
	}
	service.issueOpenChanged(context.Background(), issue)
}

// TestAReopenCarriesNoApprovalOntoARecordThatMovedWhileTheIssueWasClosed holds R14's carry to the
// record the person approved at an issue's reopen. Alice approves the spec of a converted issue,
// and the issue closes. Within the close margin a browser of the issue's tree writes a criterion
// into the tree artifact's own fragment, which the spec query takes: the closed issue's settlement
// writes the tree's version with no carry, and the drain writes the next version of the spec,
// holding the approved bytes and the moved record's digest, and carries nothing. The reopen then
// follows that version (followLatestVersion), whose text is the approved text: it carries nothing
// either, since its record is not the one alice approved. No review stands at the version, the
// newest review is still alice's own approval of the version she approved, and the gate reads the
// spec as changed since.
//
// The control is a reopen over the approved text a closed settlement wrote back, with the record
// alice approved: a word of the requirement moved while the issue was open, and the browser wrote
// it back once the issue had closed. The reopen carries her approval to that version by the
// restore, and the gate reads the spec approved.
func TestAReopenCarriesNoApprovalOntoARecordThatMovedWhileTheIssueWasClosed(t *testing.T) {
	t.Parallel()
	t.Run("a record that moved while the issue was closed", func(t *testing.T) {
		ctx := context.Background()
		written := approvedRecordRoomOf(t, 12, recordSpec)
		database, spec := written.database, written.room.spec
		approval, err := NewestReview(ctx, database.Pool, spec)
		if err != nil || approval == nil {
			t.Fatalf("read alice's approval of the spec of %s: %+v, %v", written.room.issue, approval, err)
		}
		pin := recordVersionDigest(t, database, spec, written.approved)
		browser := connectPeer(t, written.serverURL, written.room.tree, "alice")
		if err := browser.WaitForText(fragmentMarkdown(written.room.plan), planSeed, roomWait(t)); err != nil {
			t.Fatalf("a browser's copy of %s's room through its tree: %v", written.room.issue, err)
		}
		criterionTree, err := pmdoc.Parse(treeCriterion)
		if err != nil {
			t.Fatalf("parse the criterion: %v", err)
		}

		closeRecordRoom(t, written)
		treeVersion := savedVersion(t, database, written.room.tree)
		written.settleEdit(t, ctx, browser, fragmentName, func(*pmdoc.Node) (*pmdoc.Node, error) { return criterionTree, nil })
		if number := savedVersion(t, database, written.room.tree); number != treeVersion+1 {
			t.Fatalf("the closed issue's settlement of the criterion left version %d of the tree of %s newest, want %d", number, written.room.issue, treeVersion+1)
		}
		criterion := refs.BlockNodeKind + ":" + refKeyOf(t, database, written.room.tree) + "#k2"
		record, err := ReadQueryRecord(ctx, database.Pool, written.room.issue, graphstore.QuerySpec, refs.TreeCut{})
		if err != nil {
			t.Fatalf("read the spec's record of %s: %v", written.room.issue, err)
		}
		if !slices.ContainsFunc(record.Nodes, func(node TreeNode) bool { return node.Address() == criterion }) || record.Digest == pin {
			t.Fatalf("the spec's record of %s holds %v at the digest %s once the tree holds %s, and alice's approval pins %s: want the criterion in it and the digest moved, or this test proves nothing",
				written.room.issue, walkNodeAddresses(record.Nodes), record.Digest, criterion, pin)
		}
		moved := savedVersion(t, database, spec)
		if moved != written.approved+1 || storedVersionMarkdown(t, database, spec, moved) != storedVersionMarkdown(t, database, spec, written.approved) ||
			recordVersionDigest(t, database, spec, moved) != record.Digest {
			t.Fatalf("the spec of %s holds version %d newest after the closed issue's criterion, digesting to %s, want the drain's version %d holding the approved bytes and the moved record's digest %s",
				written.room.issue, moved, recordVersionDigest(t, database, spec, moved), written.approved+1, record.Digest)
		}
		if count := carriedReviewCount(t, database, spec, moved); count != 0 {
			t.Fatalf("the drain's version %d of the spec holds %d carried reviews before the reopen, want none: its record moved, and the issue is closed", moved, count)
		}

		reopenRecordRoom(t, written)
		if latest := savedVersion(t, database, spec); latest != moved {
			t.Fatalf("the reopen left version %d of the spec newest, want the drain's %d: the reopen writes no version of it", latest, moved)
		}
		if count := carriedReviewCount(t, database, spec, moved); count != 0 {
			carry := readRecordCarry(t, database, spec, moved)
			t.Errorf("version %d of the spec holds %d carried reviews after the reopen, the newest reading %q carried from version %d, want none: its record holds the criterion %s, which alice never approved",
				moved, count, carry.reason, carry.from, criterion)
		}
		if newest, err := NewestReview(ctx, database.Pool, spec); err != nil || newest == nil || newest.ID != approval.ID || newest.Version != written.approved {
			version := 0
			if newest != nil {
				version = newest.Version
			}
			t.Errorf("the newest review of the spec after the reopen is a review of version %d (%v), want alice's own approval %s of version %d: the approval stays at the version she approved",
				version, err, approval.ID, written.approved)
		}
		if specApprovedAt(t, written, moved) {
			t.Errorf("the gate reads the spec of %s approved at version %d after the reopen, want it changed since alice's approval of version %d: its record holds the criterion %s",
				written.room.issue, moved, written.approved, criterion)
		}
	})

	t.Run("the approved text a closed settlement wrote back", func(t *testing.T) {
		ctx := context.Background()
		written := approvedRecordRoomOf(t, 13, recordSpec)
		database, spec := written.database, written.room.spec
		pin := recordVersionDigest(t, database, spec, written.approved)
		approvedText := storedVersionMarkdown(t, database, spec, written.approved)
		written.settleSpecEdit(t, ctx, func(tree *pmdoc.Node) (*pmdoc.Node, error) {
			return tree, replaceWords(tree, "r1", "An urgent ask comes last.")
		})
		edited := savedVersion(t, database, spec)
		if edited != written.approved+1 || recordVersionDigest(t, database, spec, edited) == pin || carriedReviewCount(t, database, spec, edited) != 0 {
			t.Fatalf("the requirement's edit left version %d of the spec newest, digesting to %s with %d carried reviews, want version %d with a moved record and no carry",
				edited, recordVersionDigest(t, database, spec, edited), carriedReviewCount(t, database, spec, edited), written.approved+1)
		}

		closeRecordRoom(t, written)
		written.settleSpecEdit(t, ctx, func(tree *pmdoc.Node) (*pmdoc.Node, error) {
			return tree, replaceWords(tree, "r1", "An urgent ask comes first.")
		})
		back := savedVersion(t, database, spec)
		if back != edited+1 || storedVersionMarkdown(t, database, spec, back) != approvedText || recordVersionDigest(t, database, spec, back) != pin {
			t.Fatalf("the closed issue's settlement left version %d of the spec newest, holding\n%s\ndigesting to %s, want version %d holding the approved text\n%s\nat the approved digest %s",
				back, storedVersionMarkdown(t, database, spec, back), recordVersionDigest(t, database, spec, back), edited+1, approvedText, pin)
		}
		if count := carriedReviewCount(t, database, spec, back); count != 0 {
			t.Fatalf("version %d of the spec holds %d carried reviews before the reopen, want none: a closed issue's settlement carries nothing", back, count)
		}

		reopenRecordRoom(t, written)
		if carry := readRecordCarry(t, database, spec, back); carry.reason != restoredReason(written.approved, written.approved) || carry.from != written.approved {
			t.Errorf("the review the reopen carried to version %d reads %q carried from version %d, want %q carried from version %d",
				back, carry.reason, carry.from, restoredReason(written.approved, written.approved), written.approved)
		}
		if !specApprovedAt(t, written, back) {
			t.Errorf("the gate reads the spec of %s changed since at version %d after the reopen, want it approved: it holds the approved text and record", written.room.issue, back)
		}
	})
}
