package docs

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A carried approval is written before its version (R14): a carry's artifact.approved comes before
// the version's artifact.version, in event id and in seq, which the deployed Legion daemon reads a
// carry by; and the one carry a write of a converted query document still holds, the cutover's,
// is written once, in the write's batch, and pinned to its record's digest by the fill.

// orderDecisionReading is a decision document with the agent's reading under its decision.
const orderDecisionReading = "Context\n\n::::decision{#d1}\nWe ship on Friday.\n\n:::reading{#d1-r1}\nThe agent reads it as Friday.\n:::\n::::\n"

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
		Payload: ArtifactVersionEventPayload(artifactID, "document.md", version, nil, model.ReferenceChanges{}, VersionDigest{}),
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

// TestAPreCutoverRestoreCarriesOnce restores the approved text of a version that holds no
// version_renders row, and so no record digest (R14): PRE-1's spec, a query version's document,
// stores version 1 by SQL, as a release before the cutover wrote it, and alice approves it, pinning
// its stored markdown's sha256. Version 2 changes the text through the real version write, and
// version 3 restores version 1's text. PRE-1 has not converted, so its write carries alice's
// approval by its text, once, in its batch: exactly one carried review, which pins the sha256 of
// version 3's stored markdown, with one artifact.approved before the version's artifact.version;
// the write's own pass carries nothing again. The fill after the COMMIT pins the record's digest
// on that carry's own row of artifact_review_carries and appends one artifact.digest, with no
// second review and no second artifact.approved, leaving the review's content_sha256 as it was.
// The case reads the review, its pin and its row at the start of the fill (HoldFills), on a
// connection of its own, and again once the fill has run.
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

	// before is what a connection of its own reads of the carry at the start of the fill, once the
	// write has committed: the carry's review, the digest it pins and the digest every gate reads
	// it standing on (reviewPin), its row of artifact_review_carries, and what the log holds.
	type reading struct {
		review, pinned, standing, rowRecord *string
		reviews, approvals, digests         int
		owed                                bool
	}
	read := func(number int) reading {
		var got reading
		if err := database.Pool.QueryRow(ctx, `
			select (select id::text from artifact_reviews where artifact_id = $1 and version = $2 order by created_at desc, id desc limit 1),
			       (select content_sha256 from artifact_reviews where artifact_id = $1 and version = $2 order by created_at desc, id desc limit 1),
			       (select `+reviewPin+` from artifact_reviews where artifact_id = $1 and version = $2 order by created_at desc, id desc limit 1),
			       (select c.record_sha256 from artifact_review_carries c join artifact_reviews r on r.id = c.review_id where r.artifact_id = $1 and r.version = $2),
			       (select count(*) from artifact_reviews where artifact_id = $1 and version = $2),
			       (select count(*) from events where type = 'artifact.approved' and payload->>'artifact_id' = $1::text and (payload->>'version')::int = $2),
			       (select count(*) from events where type = 'artifact.digest' and payload->>'artifact_id' = $1::text and (payload->>'version')::int = $2),
			       coalesce((select r.digest_owed from version_renders r where r.artifact_id = $1 and r.number = $2), false)`,
			spec, number).Scan(&got.review, &got.pinned, &got.standing, &got.rowRecord, &got.reviews, &got.approvals, &got.digests, &got.owed); err != nil {
			t.Fatalf("read the carry to version %d: %v", number, err)
		}
		return got
	}
	const restored = 3
	var before reading
	service.HoldFills(func(issueKey string) {
		if issueKey == "PRE-1" {
			before = read(restored)
		}
	})
	defer service.HoldFills(nil)
	if number := namedVersionWithItsEvent(t, service, "PRE-1", spec, "The approved text again"); number != restored || storedVersionMarkdown(t, database, spec, number) != approvedText {
		t.Fatalf("the restore wrote version %d storing %q, want version %d storing %q", number, storedVersionMarkdown(t, database, spec, number), restored, approvedText)
	}

	if before.reviews != 1 || before.approvals != 1 || before.digests != 0 || !before.owed {
		t.Fatalf("at the start of the fill version %d holds %d reviews, %d artifact.approved events and %d artifact.digest events, owed %t, want the one carry its write's batch made, no digest event yet, and its digest owed: the write's own pass carried again",
			restored, before.reviews, before.approvals, before.digests, before.owed)
	}
	if before.pinned == nil || *before.pinned != reviewTextHash(approvedText) || before.standing == nil || *before.standing != reviewTextHash(approvedText) || before.rowRecord != nil {
		t.Fatalf("at the start of the fill the carry pins %v, stands on %v and holds record_sha256 %v on its row, want the sha256 of its stored markdown, %s, twice, and none",
			before.pinned, before.standing, before.rowRecord, reviewTextHash(approvedText))
	}
	order := readCarriedOrder(t, database, "the restore's carry", spec, restored)
	requireApprovalFirst(t, "restore", order)
	if order.review != *before.review {
		t.Fatalf("the carry to version %d is the review %s after the fill, want %s: the fill wrote another review", restored, order.review, *before.review)
	}

	after := read(restored)
	var record *string
	if err := database.Pool.QueryRow(ctx, `select record_sha256 from version_renders where artifact_id = $1 and number = $2`, spec, restored).Scan(&record); err != nil {
		t.Fatalf("read the row of version %d: %v", restored, err)
	}
	switch {
	case record == nil || after.owed:
		t.Fatalf("version %d's row holds record digest %v, owed %t, after its write committed: want the fill to have recorded it", restored, record, after.owed)
	case *record == reviewTextHash(approvedText):
		t.Fatalf("version %d's record digest is the sha256 of its stored markdown, %s, want its record's", restored, *record)
	case after.rowRecord == nil || *after.rowRecord != *record:
		t.Errorf("the carry %s holds record_sha256 %v on its row of artifact_review_carries, want version %d's record digest %s: the fill did not pin it",
			order.review, after.rowRecord, restored, *record)
	case after.standing == nil || *after.standing != *record:
		t.Errorf("the carry %s stands on %v after the fill, want version %d's record digest %s: every gate reads its row's digest", order.review, after.standing, restored, *record)
	}
	if after.review == nil || *after.review != *before.review || after.pinned == nil || *after.pinned != *before.pinned {
		t.Errorf("after the fill the carry to version %d is %v pinning %v, want %s still pinning %v: the fill changed the immutable review", restored, after.review, after.pinned, *before.review, *before.pinned)
	}
	if after.reviews != 1 || after.approvals != 1 {
		t.Errorf("after the fill version %d holds %d reviews and %d artifact.approved events, want the one carry's: the fill added a review or an approval", restored, after.reviews, after.approvals)
	}
	if after.digests != 1 {
		t.Fatalf("after the fill version %d holds %d artifact.digest events, want the fill's one", restored, after.digests)
	}
	var current bool
	var digest, approvedDigest *string
	var approvedVersion *int
	if err := database.Pool.QueryRow(ctx, `
		select (payload->>'approval_current')::boolean, payload->>'content_sha256', payload->>'approved_sha256', (payload->>'approved_version')::int
		from events where type = 'artifact.digest' and payload->>'artifact_id' = $1::text and (payload->>'version')::int = $2`,
		spec, restored).Scan(&current, &digest, &approvedDigest, &approvedVersion); err != nil {
		t.Fatalf("read the artifact.digest of version %d: %v", restored, err)
	}
	if !current || digest == nil || record == nil || *digest != *record || approvedVersion == nil || *approvedVersion != restored || approvedDigest == nil || *approvedDigest != *record {
		t.Errorf("the artifact.digest of version %d reads approval_current %t, content_sha256 %v, approved_version %v and approved_sha256 %v, want true, the record's %v, %d and the record's",
			restored, current, digest, approvedVersion, approvedDigest, record, restored)
	}
}
