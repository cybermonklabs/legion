package grapherase

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// appendEvent writes one event of issue with payload, as the log holds it, and answers its id.
func appendEvent(t *testing.T, database *store.Store, issue, kind, payload string) int64 {
	t.Helper()
	var id int64
	if err := database.Pool.QueryRow(context.Background(), `
		insert into events (issue_key, seq, type, actor, payload, notify)
		values ($1, coalesce((select max(seq) from events where issue_key = $1), 0) + 1, $2,
		        '{"kind":"user","id":"alice"}', $3::jsonb, false)
		returning id`, issue, kind, payload).Scan(&id); err != nil {
		t.Fatalf("append a %s event of %s: %v", kind, issue, err)
	}
	return id
}

// writeVersion writes version 1 of document holding markdown.
func writeVersion(t *testing.T, database *store.Store, document quotingDocument, markdown string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1::uuid, 1, $2, '[{"kind":"user","id":"alice"}]')`, document.artifactID, markdown); err != nil {
		t.Fatalf("write version 1 of %s: %v", document.refKey, err)
	}
}

// rowsAsWritten reads each of the rows a test planted for another node, whole, so a read after the
// erase can say whether any byte of one moved: the versions of the given documents and the
// payloads of the given events.
func rowsAsWritten(t *testing.T, database *store.Store, documents []string, events []int64) string {
	t.Helper()
	var rows string
	if err := database.Pool.QueryRow(context.Background(), `
		select coalesce((select string_agg(v::text, ';' order by v.artifact_id, v.number)
		                 from artifact_versions v where v.artifact_id = any($1::uuid[])), '') || '|' ||
		       coalesce((select string_agg(e.payload::text, ';' order by e.id) from events e where e.id = any($2::bigint[])), '')`,
		documents, events).Scan(&rows); err != nil {
		t.Fatalf("read the planted rows: %v", err)
	}
	return rows
}

// eventPayload reads the payload of event id, decoded.
func eventPayload(t *testing.T, database *store.Store, id int64) (map[string]any, string) {
	t.Helper()
	var raw string
	if err := database.Pool.QueryRow(context.Background(), `select payload::text from events where id = $1`, id).Scan(&raw); err != nil {
		t.Fatalf("read the payload of event %d: %v", id, err)
	}
	decoder := json.NewDecoder(bytes.NewReader([]byte(raw)))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		t.Fatalf("decode the payload of event %d: %v", id, err)
	}
	return payload, raw
}

// R5: the erase changes only the rows that derive from the node. The comment erased reads `Yes`.
// The same three bytes stand in the spec of another issue, ACME-1, in the only version of a
// document of a third issue, BETA-1, and in the comment.created payload of a comment another
// person wrote. Each is another node's text: the run lists each as a row it leaves, and after it
// every one of them reads byte for byte as it did, while the comment's own event reads the marker
// where its body was.
func TestTheEraseLeavesAnotherNodesTextAndListsIt(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seeded := seedAgreeingStore(t, database)
	if _, err := database.Pool.Exec(ctx, `update comments set body = 'Yes' where id = $1::uuid`, seeded.comment); err != nil {
		t.Fatalf("write the comment's body: %v", err)
	}
	own := appendEvent(t, database, "CORE-1", "comment.created",
		fmt.Sprintf(`{"id": %q, "body": "Yes", "author": {"kind": "user", "id": "alice"}}`, seeded.comment))
	spec := seedQuotingDocument(t, database, "ACME")
	if _, err := database.Pool.Exec(ctx, `update artifacts set is_primary = true where id = $1::uuid`, spec.artifactID); err != nil {
		t.Fatalf("make %s ACME-1's spec: %v", spec.refKey, err)
	}
	writeVersion(t, database, spec, "Yes, this is what ACME-1 ships.")
	other := seedQuotingDocument(t, database, "BETA")
	writeVersion(t, database, other, "Yes")
	bob := appendEvent(t, database, "ACME-1", "comment.created",
		`{"id": "00000000-0000-4000-8000-0000000000b0", "body": "Yes, I agree.", "author": {"kind": "user", "id": "bob"}}`)
	planted := []string{spec.artifactID, other.artifactID}
	before := rowsAsWritten(t, database, planted, []int64{bob})

	listing := dryRun(t, database, commentNodeKind, seeded.comment)
	for _, left := range [][]string{
		{"leave public.artifact_versions", "[" + spec.refKey + " version 1 of ACME-1]", "markdown"},
		{"leave public.artifact_versions", "[" + other.refKey + " version 1 of BETA-1]", "markdown"},
		{"leave public.events", fmt.Sprintf("(id)=(%d)", bob), "payload"},
		{fmt.Sprintf("events %d comment.created", own)},
	} {
		if !listed(listing, left...) {
			t.Errorf("the listing holds no line with %q:\n%s", left, strings.Join(listing.Listing, "\n"))
		}
	}
	if len(listing.Versions) != 0 || len(listing.Reviews) != 0 || len(listing.Documents) != 0 {
		t.Errorf("the listing ends the versions %+v and the reviews %+v and replaces the histories %+v, want none: a comment has no document of its own",
			listing.Versions, listing.Reviews, listing.Documents)
	}

	erasure, err := Erase(ctx, database.Pool.Config().ConnString(), commentNodeKind, seeded.comment, listing.Digest, time.Millisecond)
	if err != nil {
		t.Fatalf("erase --confirm %s: %v", listing.Digest, err)
	}
	requireTheCommentErased(t, database, erasure, seeded.comment)
	if after := rowsAsWritten(t, database, planted, []int64{bob}); after != before {
		t.Errorf("the erase changed rows of other nodes that held the same bytes:\nbefore %s\nafter  %s", before, after)
	}
	if payload, _ := eventPayload(t, database, own); payload["body"] != erasure.marker() {
		t.Errorf("the comment's own event reads the body %q after the erase, want the marker %q", payload["body"], erasure.marker())
	}
}

// R5: a run without --confirm takes no write lock, lists every row with each event row by its id,
// and prints a digest; --confirm erases only when the listing the run reads under its own locks
// has that digest. The test holds the comment's owner row, its node row and its comments row while
// the run without --confirm reads them all, which it does without waiting. A digest the listing
// does not have is refused with both digests and removes nothing; so is the digest of a listing
// that changed since, here by a second event about the comment; the digest of the listing read
// again erases it, redacting both events.
func TestTheRunWithoutConfirmTakesNoLockAndTheConfirmErasesOnlyItsListing(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seeded := seedAgreeingStore(t, database)
	body := fmt.Sprintf(`{"id": %q, "body": "A remark on the spec."}`, seeded.comment)
	created := appendEvent(t, database, "CORE-1", "comment.created", body)

	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the holder: %v", err)
	}
	defer holder.Rollback(ctx)
	for _, hold := range []struct{ statement, id string }{
		{`select 1 from issues where key = $1 for update`, "CORE-1"},
		{`select 1 from graph_nodes where kind = 'comment' and id = $1 for update`, seeded.comment},
		{`select 1 from comments where id = $1::uuid for update`, seeded.comment},
	} {
		if tag, err := holder.Exec(ctx, hold.statement, hold.id); err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("hold %q: %d rows, %v", hold.statement, tag.RowsAffected(), err)
		}
	}
	// The comment's owner is serialised on its advisory lock, which every writer of the issue and
	// the confirmed erase take first (store.LockOwner); the holder takes it too.
	if err := store.LockOwner(ctx, holder, store.IssueOwner("CORE-1")); err != nil {
		t.Fatalf("hold issue CORE-1's owner lock: %v", err)
	}
	type answer struct {
		erasure *Erasure
		err     error
	}
	answered := make(chan answer, 1)
	go func() {
		erasure, err := Erase(ctx, database.Pool.Config().ConnString(), commentNodeKind, seeded.comment, "", 0)
		answered <- answer{erasure, err}
	}()
	var listing *Erasure
	select {
	case got := <-answered:
		if got.err != nil {
			t.Fatalf("erase without --confirm: %v", got.err)
		}
		listing = got.erasure
	case <-time.After(20 * time.Second):
		t.Fatalf("the run without --confirm waited 20 s behind the comment's owner lock and row, node row and comments row, which another transaction held: it takes a lock")
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("let go of the held rows: %v", err)
	}
	if listing.Applied || listing.Digest == "" || !listed(listing, fmt.Sprintf("events %d comment.created", created)) {
		t.Fatalf("the run without --confirm answered applied %t, digest %q and the listing:\n%s\nwant nothing removed, a digest and event %d listed",
			listing.Applied, listing.Digest, strings.Join(listing.Listing, "\n"), created)
	}

	url := database.Pool.Config().ConnString()
	wrong := strings.Repeat("0", len(listing.Digest))
	refused, err := Erase(ctx, url, commentNodeKind, seeded.comment, wrong, time.Millisecond)
	if err != nil {
		t.Fatalf("erase --confirm %s: %v", wrong, err)
	}
	var report bytes.Buffer
	refused.Write(&report)
	if refused.Applied || !refused.Mismatch() || refused.Digest != listing.Digest ||
		!strings.Contains(report.String(), wrong) || !strings.Contains(report.String(), listing.Digest) {
		t.Errorf("erase --confirm with a digest no listing has answered applied %t, mismatch %t, digest %s, want it refused naming both digests:\n%s",
			refused.Applied, refused.Mismatch(), refused.Digest, report.String())
	}

	edited := appendEvent(t, database, "CORE-1", "comment.edited", body)
	stale, err := Erase(ctx, url, commentNodeKind, seeded.comment, listing.Digest, time.Millisecond)
	if err != nil {
		t.Fatalf("erase --confirm %s after the listing changed: %v", listing.Digest, err)
	}
	if stale.Applied || !stale.Mismatch() || stale.Digest == listing.Digest {
		t.Errorf("erase --confirm with the digest of a listing that changed since answered applied %t, mismatch %t, digest %s, want it refused",
			stale.Applied, stale.Mismatch(), stale.Digest)
	}
	for _, event := range []int64{created, edited} {
		if payload, _ := eventPayload(t, database, event); payload["body"] != "A remark on the spec." {
			t.Errorf("a refused run rewrote event %d: its body reads %q", event, payload["body"])
		}
	}
	var comments int
	if err := database.Pool.QueryRow(ctx, `select count(*) from comments where id = $1::uuid`, seeded.comment).Scan(&comments); err != nil || comments != 1 {
		t.Errorf("after the refused runs %d comments row(s) stand (err %v), want the comment's one", comments, err)
	}

	erasure, err := confirmedErase(t, database, commentNodeKind, seeded.comment, time.Millisecond)
	if err != nil {
		t.Fatalf("erase --confirm with the listing's own digest: %v", err)
	}
	requireTheCommentErased(t, database, erasure, seeded.comment)
	for _, event := range []int64{created, edited} {
		if payload, _ := eventPayload(t, database, event); payload["body"] != erasure.marker() {
			t.Errorf("event %d reads the body %q after the erase, want the marker", event, payload["body"])
		}
	}
}

// R5: a redaction replaces bytes inside a JSON string value and never in a key, and never in a
// value under a structural key either. The comment erased reads `kind`, which every actor of its
// own event names as a key. After the erase every key of that payload stands, the author is still
// of kind `user`, the payload's own kind still reads `kind`, each value that held the text as what
// somebody wrote holds the marker where it was, and a number the payload holds keeps its digits.
func TestTheEventRedactionRewritesValuesAndNoKey(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seeded := seedAgreeingStore(t, database)
	if _, err := database.Pool.Exec(ctx, `update comments set body = 'kind' where id = $1::uuid`, seeded.comment); err != nil {
		t.Fatalf("write the comment's body: %v", err)
	}
	event := appendEvent(t, database, "CORE-1", "comment.created", fmt.Sprintf(`{"id": %q, "body": "kind", "kind": "kind", "ask_question": "kind",
		"note": "a kind remark", "author": {"kind": "user", "id": "alice"}, "list": ["kind", 12345678901234567890]}`, seeded.comment))
	beforePayload, _ := eventPayload(t, database, event)

	erasure, err := confirmedErase(t, database, commentNodeKind, seeded.comment, time.Millisecond)
	if err != nil {
		t.Fatalf("erase %s: %v", seeded.comment, err)
	}
	requireTheCommentErased(t, database, erasure, seeded.comment)
	payload, raw := eventPayload(t, database, event)
	for key := range beforePayload {
		if _, held := payload[key]; !held {
			t.Errorf("the redacted payload lost its key %q: %s", key, raw)
		}
	}
	marker := erasure.marker()
	author, _ := payload["author"].(map[string]any)
	list, _ := payload["list"].([]any)
	switch {
	case author == nil || author["kind"] != "user":
		t.Errorf("the redacted payload's author reads %v, want its kind key standing and reading user: %s", payload["author"], raw)
	case payload["kind"] != "kind":
		t.Errorf("the redacted payload's kind reads %v, want the structural value as written, kind: %s", payload["kind"], raw)
	case payload["body"] != marker || payload["ask_question"] != marker || payload["note"] != "a "+marker+" remark":
		t.Errorf("the redacted payload reads %s, want the marker in its body, its ask_question and its note", raw)
	case len(list) != 2 || list[0] != marker || fmt.Sprint(list[1]) != "12345678901234567890":
		t.Errorf("the redacted payload's list reads %v, want the marker and the number as written: %s", payload["list"], raw)
	}
}

// requireCensusZero runs the store census and fails the test on any difference it reads.
func requireCensusZero(t *testing.T, database *store.Store, when string) {
	t.Helper()
	report, err := graphcensus.StoreCensus(context.Background(), database.Pool.Config().ConnString(), false)
	if err != nil {
		t.Fatalf("the store census %s: %v", when, err)
	}
	if report.Differences() != 0 {
		var written bytes.Buffer
		report.Write(&written)
		t.Fatalf("the store census %s reads %d difference(s):\n%s", when, report.Differences(), written.String())
	}
}

// requireRefused runs the erase of kind:id as an operator does and holds it to a refusal naming
// stating, with the node, its tuples and its projection rows standing as they did.
func requireRefused(t *testing.T, database *store.Store, kind, id, stating string) {
	t.Helper()
	count := func() int64 {
		var held int64
		if err := database.Pool.QueryRow(context.Background(), `
			select (select count(*) from graph_nodes where kind = $1 and id = $2)
			     + (select count(*) from graph_tuples where (from_kind = $1 and from_id = $2) or (to_kind = $1 and to_id = $2))
			     + (select count(*) from document_blocks where node_kind = $1 and node_address = $2)
			     + (select count(*) from comments where node_kind = $1 and node_address = $2)`, kind, id).Scan(&held); err != nil {
			t.Fatalf("count what the store holds of %s:%s: %v", kind, id, err)
		}
		return held
	}
	before := count()
	erasure, err := confirmedErase(t, database, kind, id, time.Millisecond)
	if err != nil {
		t.Fatalf("erase %s:%s: %v", kind, id, err)
	}
	named := false
	for _, blocker := range erasure.Blockers {
		named = named || strings.HasPrefix(blocker, stating+" ")
	}
	if erasure.Applied || !named {
		t.Errorf("the erase of %s:%s answered applied %t and the blockers %q, want it refused naming %s", kind, id, erasure.Applied, erasure.Blockers, stating)
	}
	if after := count(); after != before {
		t.Errorf("the refused erase of %s:%s left %d rows of it, want the %d it held", kind, id, after, before)
	}
}

// requireErased runs the erase of kind:id as an operator does and holds it to having removed it.
func requireErased(t *testing.T, database *store.Store, kind, id string) {
	t.Helper()
	erasure, err := confirmedErase(t, database, kind, id, time.Millisecond)
	if err != nil {
		t.Fatalf("erase %s:%s: %v", kind, id, err)
	}
	if !erasure.Applied {
		t.Fatalf("the erase of %s:%s removed nothing: %q", kind, id, erasure.Blockers)
	}
}

// R5, R19: the census reads zero after every erase that runs. A block that holds a block is the
// parent its child's part_of tuple and its parent_block_id name; an erase that went on would
// delete the tuple and leave the column. So the erase of the outer block refuses, naming the inner
// block, and changes nothing; once the inner block is erased the outer one is, and the census
// reads zero.
func TestTheEraseRefusesABlockThatHoldsBlocksUntilTheyGo(t *testing.T) {
	database := storetest.Open(t)
	seeded := seedAgreeingStore(t, database)
	requireCensusZero(t, database, "before the erase")
	requireRefused(t, database, refs.BlockNodeKind, seeded.outer, refs.BlockNodeKind+":"+seeded.inner+" states "+graphstore.TuplePartOf)
	requireErased(t, database, refs.BlockNodeKind, seeded.inner)
	requireErased(t, database, refs.BlockNodeKind, seeded.outer)
	requireCensusZero(t, database, "after the erases")
}

// A block another block cites is the far end of the citing block's mentions tuple and of its
// block_refs row. The erase of the cited block refuses, naming the citing one, and changes
// nothing; once the citing block is erased the cited one is, and the census reads zero.
func TestTheEraseRefusesABlockOtherBlocksCiteUntilTheyGo(t *testing.T) {
	database := storetest.Open(t)
	seeded := seedCitingStore(t, database)
	requireCensusZero(t, database, "before the erase")
	requireRefused(t, database, refs.BlockNodeKind, seeded.inner, refs.BlockNodeKind+":"+seeded.cite+" states mentions")
	requireErased(t, database, refs.BlockNodeKind, seeded.cite)
	requireErased(t, database, refs.BlockNodeKind, seeded.inner)
	requireCensusZero(t, database, "after the erases")
}

// A person is who her comment's authored_by tuple and its author name. The erase of person:alice
// refuses, naming the comment, and changes nothing; once the comment is erased she is, and the
// census reads zero.
func TestTheEraseRefusesAPersonACommentNamesUntilItGoes(t *testing.T) {
	database := storetest.Open(t)
	seeded := seedAgreeingStore(t, database)
	requireCensusZero(t, database, "before the erase")
	requireRefused(t, database, graphstore.KindPerson, "alice", commentNodeKind+":"+seeded.comment+" states "+graphstore.TupleAuthoredBy)
	requireErased(t, database, commentNodeKind, seeded.comment)
	requireErased(t, database, graphstore.KindPerson, "alice")
	requireCensusZero(t, database, "after the erases")
}

// R5: a block that holds another holds the other's text inside its own, and the erase of the
// inner block redacts the outer one's text only at a version the inner block stood in. At version
// 1 the outer list item is alone and reads `Ship it on Friday.` as its own words; version 2 nests
// the inner block inside it, holding the same words, under a line of the outer block's own. The
// erase of the inner block redacts the outer block's text at version 2, where those words are the
// inner block's, and its excerpt cut from that version, and leaves its text at version 1, where
// they were the outer block's own. The listing names version 2's text as one the run redacts and
// version 1's as a row it leaves.
func TestTheEraseLeavesAnAncestorsTextFromBeforeTheBlockExisted(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seeded := seedAgreeingStore(t, database)
	const words = "Ship it on Friday."
	nested := "Plan for the week:\n  - " + words
	artifact := seeded.document.artifactID
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1::uuid, 2, $2, '[{"kind":"user","id":"alice"}]')`, artifact, "- "+nested); err != nil {
		t.Fatalf("write version 2 of the document: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into document_block_texts (artifact_id, block_id, version, text)
		values ($1::uuid, 'outer', 1, $2), ($1::uuid, 'outer', 2, $3), ($1::uuid, 'inner', 2, $2)`,
		artifact, words, nested); err != nil {
		t.Fatalf("write the blocks' texts at versions 1 and 2: %v", err)
	}
	if tag, err := database.Pool.Exec(ctx, `
		update document_blocks set changed_version = 2, excerpt = case block_id when 'outer' then $2 else $3 end
		where artifact_id = $1::uuid and block_id in ('outer', 'inner')`, artifact, nested, words); err != nil || tag.RowsAffected() != 2 {
		t.Fatalf("write the blocks' excerpts at version 2: %d rows, %v", tag.RowsAffected(), err)
	}

	erasure, err := confirmedErase(t, database, refs.BlockNodeKind, seeded.inner, time.Millisecond)
	if err != nil {
		t.Fatalf("erase %s: %v", seeded.inner, err)
	}
	if !erasure.Applied {
		t.Fatalf("the erase of %s removed nothing: %q", seeded.inner, erasure.Blockers)
	}
	outerText := func(version int) string {
		var text string
		if err := database.Pool.QueryRow(ctx, `
			select text from document_block_texts where artifact_id = $1::uuid and block_id = 'outer' and version = $2`,
			artifact, version).Scan(&text); err != nil {
			t.Fatalf("read the outer block's text at version %d: %v", version, err)
		}
		return text
	}
	if got := outerText(1); got != words {
		t.Errorf("after the erase of %s the outer block's text at version 1 reads %q, want it as written, %q: the inner block did not stand in version 1, so those words were the outer block's own",
			seeded.inner, got, words)
	}
	if got, want := outerText(2), "Plan for the week:\n  - "+erasure.marker(); got != want {
		t.Errorf("after the erase of %s the outer block's text at version 2 reads %q, want %q: the inner block stood in version 2 holding those words",
			seeded.inner, got, want)
	}
	var excerpt string
	if err := database.Pool.QueryRow(ctx, `select excerpt from document_blocks where artifact_id = $1::uuid and block_id = 'outer'`,
		artifact).Scan(&excerpt); err != nil {
		t.Fatalf("read the outer block's excerpt: %v", err)
	}
	if strings.Contains(excerpt, words) {
		t.Errorf("after the erase of %s the outer block's excerpt reads %q: it was cut from version 2, where the words are the inner block's, and holds them still",
			seeded.inner, excerpt)
	}
	outer := seeded.document.refKey + "#outer"
	if !listed(erasure, "redact document_block_texts "+outer+" version 2") {
		t.Errorf("the listing does not name the outer block's text at version 2 as one the run redacts:\n%s", strings.Join(erasure.Listing, "\n"))
	}
	if listed(erasure, "redact document_block_texts "+outer+" version 1") {
		t.Errorf("the listing names the outer block's text at version 1 as one the run redacts, a version the inner block did not stand in:\n%s",
			strings.Join(erasure.Listing, "\n"))
	}
	if !listed(erasure, "leave public.document_block_texts", "["+outer+" version 1]", "text") {
		t.Errorf("the listing does not name the outer block's text at version 1 as a row the run leaves holding the words:\n%s",
			strings.Join(erasure.Listing, "\n"))
	}
}

// R5: the listing names every row the run leaves holding an erased text, the excerpt of a block
// holding the node's block among them. At version 2 the outer list item holds the inner block,
// which reads `Ship it on Friday.`, under a line of the outer block's own; version 3 drops the
// inner block, whose row there holds no text, and the outer item reads the same words as its own.
// The erase of the inner block redacts the outer block's text at version 2, where those words are
// the inner block's, and leaves the outer block's excerpt, cut from version 3, where they are the
// outer block's own. The listing names that excerpt's document_blocks row as a row the run leaves
// holding the words, although the outer block is one whose text the run rewrites.
func TestTheEraseListsAnAncestorsExcerptItLeaves(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seeded := seedAgreeingStore(t, database)
	const words = "Ship it on Friday."
	nested := "Plan for the week:\n  - " + words
	artifact := seeded.document.artifactID
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1::uuid, 2, $2, '[{"kind":"user","id":"alice"}]'), ($1::uuid, 3, $3, '[{"kind":"user","id":"alice"}]')`,
		artifact, "- "+nested, "- "+words); err != nil {
		t.Fatalf("write versions 2 and 3 of the document: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into document_block_texts (artifact_id, block_id, version, text)
		values ($1::uuid, 'outer', 2, $2), ($1::uuid, 'inner', 2, $3), ($1::uuid, 'outer', 3, $3), ($1::uuid, 'inner', 3, null)`,
		artifact, nested, words); err != nil {
		t.Fatalf("write the blocks' texts at versions 2 and 3: %v", err)
	}
	if tag, err := database.Pool.Exec(ctx, `
		update document_blocks set changed_version = case block_id when 'outer' then 3 else 2 end, excerpt = $2
		where artifact_id = $1::uuid and block_id in ('outer', 'inner')`, artifact, words); err != nil || tag.RowsAffected() != 2 {
		t.Fatalf("write the blocks' excerpts: %d rows, %v", tag.RowsAffected(), err)
	}

	erasure, err := confirmedErase(t, database, refs.BlockNodeKind, seeded.inner, time.Millisecond)
	if err != nil {
		t.Fatalf("erase %s: %v", seeded.inner, err)
	}
	if !erasure.Applied {
		t.Fatalf("the erase of %s removed nothing: %q", seeded.inner, erasure.Blockers)
	}
	outer := seeded.document.refKey + "#outer"
	if !listed(erasure, "redact document_block_texts "+outer+" version 2") {
		t.Fatalf("the listing does not name the outer block's text at version 2 as one the run redacts, so the run rewrites nothing of the outer block and this test holds nothing:\n%s",
			strings.Join(erasure.Listing, "\n"))
	}
	var excerpt string
	if err := database.Pool.QueryRow(ctx, `select excerpt from document_blocks where artifact_id = $1::uuid and block_id = 'outer'`,
		artifact).Scan(&excerpt); err != nil {
		t.Fatalf("read the outer block's excerpt: %v", err)
	}
	if !strings.Contains(excerpt, words) {
		t.Errorf("after the erase of %s the outer block's excerpt reads %q, want it as written, %q: it was cut from version 3, which the inner block did not stand in, so those words are the outer block's own",
			seeded.inner, excerpt, words)
	}
	if !listed(erasure, "leave public.document_blocks", "[block:"+outer+"]", "excerpt") {
		t.Errorf("the listing of the erase of %s names no row left for the excerpt of %s, which still holds the erased text:\n%s",
			seeded.inner, outer, strings.Join(erasure.Listing, "\n"))
	}
}
