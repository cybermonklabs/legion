package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// c-query-drain's case of the fill (R15, R14): a version write keeps main's window, so its batch
// inserts the version's version_renders row with the cut it takes in SQL and its record's digest
// owed, and the fill records that digest after the COMMIT, in a transaction of its own, before the
// server publishes the write's events.

// versionFillRow is the version_renders row of one version, as the fill case reads it on a
// connection of its own: its cut, its record's digest, whether that digest is owed, and whether
// the row is marked tree_mismatch.
type versionFillRow struct {
	at       *time.Time
	readSeq  *int64
	digest   *string
	owed     bool
	mismatch bool
}

// versionFillRowOf reads the version_renders row of version number of artifactID.
func versionFillRowOf(t *testing.T, database *store.Store, artifactID string, number int) versionFillRow {
	t.Helper()
	var row versionFillRow
	if err := database.Pool.QueryRow(context.Background(), `
		select at, read_seq, record_sha256, digest_owed, tree_mismatch from version_renders where artifact_id = $1::uuid and number = $2`,
		artifactID, number).Scan(&row.at, &row.readSeq, &row.digest, &row.owed, &row.mismatch); err != nil {
		t.Fatalf("read the version_renders row of version %d of %s: %v", number, artifactID, err)
	}
	return row
}

// versionFillDigest is what artifact_version_text_sha256 answers for version number of artifactID.
func versionFillDigest(t *testing.T, database *store.Store, artifactID string, number int) string {
	t.Helper()
	var digest string
	if err := database.Pool.QueryRow(context.Background(), `select artifact_version_text_sha256($1::uuid, $2)`, artifactID, number).Scan(&digest); err != nil {
		t.Fatalf("read artifact_version_text_sha256 of version %d of %s: %v", number, artifactID, err)
	}
	return digest
}

// versionFillWords is a digest as a failure names it: the digest, or "none" where none is held.
func versionFillWords(digest *string) string {
	if digest == nil {
		return "none"
	}
	return *digest
}

// versionFillEvents takes every event published so far off published, without waiting, and
// answers how many of each type the issue key's events hold. A check of two types counts both off
// one take, so the take of one type never drops an event the check of the other counts.
func versionFillEvents(published <-chan model.Event, key string) map[string]int {
	taken := map[string]int{}
	for {
		select {
		case event := <-published:
			if event.IssueKey != nil && *event.IssueKey == key {
				taken[event.Type]++
			}
		default:
			return taken
		}
	}
}

// versionFillHold is the hold of one fill of an issue's record digests at its start
// (versionFillPause): held receives once that fill is held, and release lets it go on. While
// ticking is set every fill of the issue passes the hold, so a block-index tick the test runs
// meanwhile (versionFiller.fill) fills what the held fill would have.
type versionFillHold struct {
	held    <-chan struct{}
	release func()
	ticking atomic.Bool
}

// versionFillPause holds the next fill of the issue key at its start (docs.Service.HoldFills), a
// fill of the tick's excepted (versionFillHold.ticking). Its release runs at the end of t too, so a
// failure while the fill is held leaves no request waiting.
func versionFillPause(t *testing.T, service *docs.Service, key string) *versionFillHold {
	t.Helper()
	held, release := make(chan struct{}), make(chan struct{})
	hold := &versionFillHold{held: held}
	var once, released sync.Once
	service.HoldFills(func(issueKey string) {
		if issueKey != key || hold.ticking.Load() {
			return
		}
		once.Do(func() {
			close(held)
			<-release
		})
	})
	hold.release = func() { released.Do(func() { close(release) }) }
	t.Cleanup(hold.release)
	return hold
}

// versionFillEdit sends, in a goroutine of its own, the edit of the document artifactID that
// replaces find with with, and answers the channel its response arrives on.
func versionFillEdit(t *testing.T, handler http.Handler, artifactID, find, with string) <-chan *httptest.ResponseRecorder {
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", map[string]any{
			"ops":   []map[string]string{{"op": "replace", "find": find, "with": with}},
			"actor": sessionActor(),
		})
	}()
	return done
}

// versionFiller is what fills the record digest an edit's version owes while the edit's own fill is
// held (versionFillPause): the edit's request, once released, or the block-index tick
// (docs.Service.ReconcileBlockIndex), run while the request's fill is still held, as c-query-drain's
// repeats with the request fill suppressed have it. The request's fill, released once the tick has
// returned, finds nothing left owed.
type versionFiller struct {
	name string
	tick bool
}

// versionFillers are the two fillers each case of TestVersionRecordDigestFillsAfterCommit runs by.
var versionFillers = []versionFiller{{name: "the request"}, {name: "the tick", tick: true}}

// fill has by fill the digest version number of the document artifactID owes while hold holds its
// edit's fill, and answers the edit's response from done. A tick must have filled the version's row
// once it returns, while the request's fill is still held.
func (by versionFiller) fill(t *testing.T, service *docs.Service, database *store.Store, hold *versionFillHold, done <-chan *httptest.ResponseRecorder, artifactID string, number int) *httptest.ResponseRecorder {
	t.Helper()
	if by.tick {
		hold.ticking.Store(true)
		_, err := service.ReconcileBlockIndex(context.Background(), docs.BlockIndexCursors{}, nil)
		hold.ticking.Store(false)
		if err != nil {
			t.Fatalf("a tick of the block-index reconciliation: %v", err)
		}
		if row := versionFillRowOf(t, database, artifactID, number); row.owed || row.digest == nil {
			t.Errorf("digest_owed still true after the tick: version %d of %s holds record_sha256 %s and digest_owed %t while its request's fill is held",
				number, artifactID, versionFillWords(row.digest), row.owed)
		}
	}
	hold.release()
	return testwait.Receive(t, "the edit's answer", done)
}

// versionFillCarry is the newest review of a document as the batched carry's case reads it on a
// connection of its own: its id, version and state, the digest it pins (content_sha256), and its
// row of artifact_review_carries, the version it carries from and the record digest the fill
// repins it to, nil where it holds none.
type versionFillCarry struct {
	id      string
	version int
	state   string
	pinned  string
	from    *int
	record  *string
}

// newestCarryOf reads the newest review of the document artifactID (docs.NewestReview) and its row
// of artifact_review_carries.
func newestCarryOf(t *testing.T, database *store.Store, artifactID string) versionFillCarry {
	t.Helper()
	ctx := context.Background()
	newest, err := docs.NewestReview(ctx, database.Pool, artifactID)
	if err != nil || newest == nil {
		t.Fatalf("read the newest review of %s: %+v, %v", artifactID, newest, err)
	}
	carry := versionFillCarry{id: newest.ID, version: newest.Version, state: newest.State}
	if err := database.Pool.QueryRow(ctx, `
		select coalesce(r.content_sha256, ''), c.carried_from_version, c.record_sha256
		from artifact_reviews r left join artifact_review_carries c on c.review_id = r.id
		where r.id = $1::uuid`, newest.ID).Scan(&carry.pinned, &carry.from, &carry.record); err != nil {
		t.Fatalf("read the review %s of %s and its carry row: %v", newest.ID, artifactID, err)
	}
	return carry
}

// versionFillReviewCounts is how many reviews the document artifactID holds and how many
// artifact.approved events name its version number.
func versionFillReviewCounts(t *testing.T, database *store.Store, artifactID string, number int) (reviews, approvals int) {
	t.Helper()
	if err := database.Pool.QueryRow(context.Background(), `
		select (select count(*) from artifact_reviews where artifact_id = $1::uuid),
		       (select count(*) from events where type = 'artifact.approved' and payload->>'artifact_id' = $1::text and (payload->>'version')::int = $2)`,
		artifactID, number).Scan(&reviews, &approvals); err != nil {
		t.Fatalf("count the reviews of %s and the artifact.approved events of its version %d: %v", artifactID, number, err)
	}
	return reviews, approvals
}

// versionFillApprovalsAfter is how many artifact.approved events naming version number of
// artifactID the log holds past the event id after, and the id of the newest of them, after where
// it holds none.
func versionFillApprovalsAfter(t *testing.T, database *store.Store, artifactID string, number int, after int64) (approvals int, newest int64) {
	t.Helper()
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*), coalesce(max(id), $3) from events
		where type = 'artifact.approved' and payload->>'artifact_id' = $1::text and (payload->>'version')::int = $2 and id > $3`,
		artifactID, number, after).Scan(&approvals, &newest); err != nil {
		t.Fatalf("count the artifact.approved events of version %d of %s past event %d: %v", number, artifactID, after, err)
	}
	return approvals, newest
}

// TestVersionRecordDigestFillsAfterCommit writes versions of the spec of a converted issue through
// the edit route, versions a query's record covers, and holds each version's fill after the
// write's COMMIT and before the fill begins (versionFillPause). Each case runs twice: filled by the
// request's own fill once released, and again filled by the block-index tick
// (docs.Service.ReconcileBlockIndex) while the request's fill is still held (versionFiller), which
// must have filled the row by the time it returns; the request's fill then finds nothing owed.
//
//   - An ordinary fill: the committed version_renders row holds its time and its read_seq, no
//     digest and digest_owed true, and artifact_version_text_sha256 answers the sha256 of the
//     version's stored markdown; GET /api/v1/artifacts/{id} answers digest_owed true and the
//     markdown's digest as current_sha256; the server has published no artifact.version of it yet.
//     The case then moves the head record of the spec query, stating a placement value on the
//     spec's requirement by SQL, and lets the fill go: the fill records the digest of the record at
//     the cut the row saved, not the head's, clears digest_owed, and the function and the artifact
//     read answer that digest from then on; only then does the server publish the version's
//     artifact.version, and the fill appends one artifact.digest and no artifact.approved.
//   - An approval given while the fill is held: alice approves the version while its fill is held.
//     The review route fills the owed digest first, so the row owes nothing before the request's
//     fill runs and her review pins the record's digest, never the stored markdown's; nothing
//     carries, so the only artifact.approved of the version is hers, and the version holds one
//     artifact.digest.
//   - A write under an approval carries nothing: with alice's approval standing, an edit of the
//     paragraph the spec query's record does not take has written, at the pause after its COMMIT,
//     no review, no carry row and no artifact.approved; its fill appends one artifact.digest
//     reading approval_current true, and the approval still reads approved at the version she
//     approved. Each artifact.digest a fill appends carries its seven members as the fill reads
//     them (requireDigestPayload).
//   - The cutover carry of R14, by the request and by the tick (requireCutoverCarryFilled).
//   - A fill the request lost on a version that marks no query, which the tick's repair of lost
//     fills alone fills (requireTheTickRepairsALostFill).
func TestVersionRecordDigestFillsAfterCommit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	handler, database, deps := newTestServer(t, testServerOptions{})
	const project = "FILLDIGEST"
	createRoomProject(t, handler, project)
	issue, tree := createRoomIssue(t, handler, database, project, "A digest filled after its commit",
		":::requirement{#r1}\nThe gateway answers in a second.\n:::\n\nThe opening words.\n", "")
	if tree == "" {
		t.Fatalf("issue %s holds no tree, want it born converted", issue.Key)
	}
	spec := issue.PrimaryArtifactID
	service := deps.Docs.(*docs.Service)
	settleIssue(t, service, issue.Key)
	awaitSettled(t, database, spec)
	requirement := blockAddressOf(t, database, issue.Key, "r1")
	published, unsubscribe := deps.Events.Subscribe()
	defer unsubscribe()
	defer service.HoldFills(nil)

	// words is the paragraph the spec holds after the requirement, which each edit rewords.
	words := "The opening words."
	edit := func(t *testing.T, with string) (*versionFillHold, <-chan *httptest.ResponseRecorder) {
		t.Helper()
		versionFillEvents(published, issue.Key)
		hold := versionFillPause(t, service, issue.Key)
		done := versionFillEdit(t, handler, spec, words, with)
		words = with
		testwait.Receive(t, "the fill of the edit's version", hold.held)
		return hold, done
	}

	// Each ordinary fill moves the head record by a placement value of its own.
	placements := []string{refs.PlacementStored, refs.PlacementAuthored}
	for index, by := range versionFillers {
		t.Run("an ordinary fill by "+by.name, func(t *testing.T) {
			before := renderAtVersionNewest(t, database, spec)
			hold, done := edit(t, "The words of the ordinary fill by "+by.name+".")
			number := renderAtVersionNewest(t, database, spec)
			if number != before+1 {
				t.Errorf("the edit left the spec at version %d, want version %d", number, before+1)
			}
			row := versionFillRowOf(t, database, spec, number)
			if row.at == nil || row.readSeq == nil || row.digest != nil || !row.owed {
				t.Fatalf("the committed version_renders row of version %d holds at %v, read_seq %v, record_sha256 %v and digest_owed %t, want a time, a seq, no digest and digest_owed true",
					number, row.at, row.readSeq, row.digest, row.owed)
			}
			stored := storedVersionMarkdown(t, database, spec, number)
			if got, want := versionFillDigest(t, database, spec, number), contentHash(stored); got != want {
				t.Errorf("artifact_version_text_sha256 answers %s for version %d before the fill, want the stored markdown's %s", got, number, want)
			}
			if read := artifactDigestApproval(t, handler, spec).Approval; read == nil || read.DigestOwed == nil || !*read.DigestOwed || read.CurrentSHA256 == nil || *read.CurrentSHA256 != contentHash(stored) {
				t.Errorf("GET /api/v1/artifacts/%s answers %+v while version %d's fill is held, want digest_owed true and current_sha256 the stored markdown's, %s", spec, read, number, contentHash(stored))
			}
			if early := versionFillEvents(published, issue.Key)["artifact.version"]; early > 0 {
				t.Errorf("artifact.version published locally while its row still owes its digest: version %d of %s, %d events of %s before the fill, want none", number, refKeyOf(t, database, spec), early, issue.Key)
			}
			if _, err := database.Pool.Exec(ctx, `
				insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
				values ($1, $2, $3, $4, $5, now(), '{"kind":"user","id":"alice"}'::jsonb, 0, true)`,
				refs.BlockNodeKind, requirement, refs.AttributePlacement, graphstore.KindValue, placements[index]); err != nil {
				t.Fatalf("move the head record of the spec query: %v", err)
			}
			saved, err := docs.ReadQueryRecord(ctx, database.Pool, issue.Key, graphstore.QuerySpec, refs.TreeCut{At: *row.at, ReadSeq: *row.readSeq})
			if err != nil {
				t.Fatalf("read the spec query's record at the row's cut: %v", err)
			}
			head, err := docs.ReadQueryRecord(ctx, database.Pool, issue.Key, graphstore.QuerySpec, refs.TreeCut{})
			if err != nil {
				t.Fatalf("read the spec query's head record: %v", err)
			}
			if head.Digest == saved.Digest {
				t.Fatalf("the head record of the spec query digests to %s as its record at the row's cut does, so this case proves nothing", head.Digest)
			}
			if edited := by.fill(t, service, database, hold, done, spec, number); edited.Code != http.StatusOK {
				t.Fatalf("the edit answered %d %s, want 200", edited.Code, edited.Body.String())
			}
			if filled := versionFillRowOf(t, database, spec, number); filled.digest == nil || *filled.digest != saved.Digest || filled.owed {
				t.Errorf("after the fill the row of version %d holds record_sha256 %s and digest_owed %t, want the record's digest at the saved cut %s and false",
					number, versionFillWords(filled.digest), filled.owed, saved.Digest)
			}
			if got := versionFillDigest(t, database, spec, number); got != saved.Digest {
				t.Errorf("artifact_version_text_sha256 answers %s for version %d after the fill, want the record's digest %s", got, number, saved.Digest)
			}
			latest := renderAtVersionNewest(t, database, spec)
			current := versionFillDigest(t, database, spec, latest)
			if read := artifactDigestApproval(t, handler, spec).Approval; read == nil || read.LatestVersion != latest || read.DigestOwed == nil || *read.DigestOwed || read.CurrentSHA256 == nil || *read.CurrentSHA256 != current {
				t.Errorf("GET /api/v1/artifacts/%s answers %+v after the fill, want digest_owed false and current_sha256 %s of latest version %d", spec, read, current, latest)
			}
			events := map[string]int{}
			for _, event := range publishedSince(published) {
				if event.IssueKey == nil || *event.IssueKey != issue.Key {
					continue
				}
				if digest, ok := event.Payload.(model.ArtifactDigestEventPayload); ok && (digest.ArtifactID != spec || digest.Version != number) {
					continue
				}
				events[event.Type]++
			}
			if events["artifact.version"] == 0 {
				t.Errorf("the server published no artifact.version of %s once the edit answered", issue.Key)
			}
			if approved := events["artifact.approved"]; approved > 0 {
				t.Errorf("the ordinary fill of version %d appended %d artifact.approved events, want none", number, approved)
			}
			digests := 0
			for _, held := range loggedEvents(t, database, "artifact.digest", spec) {
				if got, _ := held.payload["version"].(float64); int(got) == number {
					digests++
					if digest, _ := held.payload["content_sha256"].(string); digest != saved.Digest {
						t.Errorf("the artifact.digest of %s@v%d carries content_sha256 %q, want the record's digest at the saved cut, %s", issue.Key, number, digest, saved.Digest)
					}
					requireDigestPayload(t, database, "the ordinary fill by "+by.name, held.payload, spec, number, saved.Digest)
				}
			}
			if digests != 1 || events["artifact.digest"] != 1 {
				t.Errorf("no artifact.digest for %s@v%d after its fill: the log holds %d and the server published %d, want the fill's one", refKeyOf(t, database, spec), number, digests, events["artifact.digest"])
			}
		})
	}

	for _, by := range versionFillers {
		t.Run("an approval given while the fill is held, by "+by.name, func(t *testing.T) {
			hold, done := edit(t, "The words alice approves while the fill is held by "+by.name+".")
			number := renderAtVersionNewest(t, database, spec)
			approvedBefore, _ := versionFillApprovalsAfter(t, database, spec, number, 0)
			approval := reviewDocument(t, handler, spec, "alice", "approved", "")
			row := versionFillRowOf(t, database, spec, number)
			if row.owed || row.digest == nil {
				t.Fatalf("version %d holds digest_owed %t and record %s after alice's review, want the review's own fill to have recorded it before the request's fill ran", number, row.owed, versionFillWords(row.digest))
			}
			stored := contentHash(storedVersionMarkdown(t, database, spec, number))
			if approval.Version != number || approval.ContentSHA256 == nil || *approval.ContentSHA256 == stored || *approval.ContentSHA256 != *row.digest {
				t.Errorf("approval of %s@v%d pinned the markdown digest while its digest was owed: alice's review is of version %d pinning %s, want version %d pinning the record's digest %s, not the stored markdown's %s",
					refKeyOf(t, database, spec), number, approval.Version, versionFillWords(approval.ContentSHA256), number, *row.digest, stored)
			}
			if approval.CarriedFromVersion != nil {
				t.Errorf("alice's review of version %d is carried from version %d, want her own", number, *approval.CarriedFromVersion)
			}
			if edited := by.fill(t, service, database, hold, done, spec, number); edited.Code != http.StatusOK {
				t.Fatalf("the edit answered %d %s, want 200", edited.Code, edited.Body.String())
			}
			if approved, _ := versionFillApprovalsAfter(t, database, spec, number, 0); approved != approvedBefore+1 {
				t.Errorf("version %d holds %d artifact.approved events after both fills, want alice's one beyond the %d before it: no carry", number, approved, approvedBefore)
			}
			if carried := newestCarryOf(t, database, spec); carried.id != approval.ID || carried.from != nil {
				t.Errorf("the newest review of the spec is %+v, want alice's own %s with no carry row: nothing carries to the version", carried, approval.ID)
			}
			if read := readApproval(t, handler, spec).Approval; read == nil || read.State != "approved" || read.Version == nil || *read.Version != number {
				t.Errorf("the approval of the spec reads %+v, want approved at version %d", read, number)
			}
		})
	}

	for _, by := range versionFillers {
		t.Run("a write under an approval carries nothing, by "+by.name, func(t *testing.T) {
			reviewDocument(t, handler, spec, "alice", "approved", "")
			if standing := newestCarryOf(t, database, spec); standing.state != "approved" || standing.version != renderAtVersionNewest(t, database, spec) {
				t.Fatalf("the newest review of the spec is %+v, want an approval of its newest version for the edit to leave standing", standing)
			}
			standing := newestCarryOf(t, database, spec)
			reviews, approvals := versionFillReviewCounts(t, database, spec, renderAtVersionNewest(t, database, spec)+1)
			hold, done := edit(t, "The words the edit under an approval writes, by "+by.name+".")
			number := renderAtVersionNewest(t, database, spec)
			if after := newestCarryOf(t, database, spec); after.id != standing.id || after.version != standing.version {
				t.Fatalf("at the pause after the edit's COMMIT the newest review of the spec is %+v, want alice's %s of version %d untouched: the write carried an approval", after, standing.id, standing.version)
			}
			if gotReviews, gotApprovals := versionFillReviewCounts(t, database, spec, number); gotReviews != reviews || gotApprovals != approvals {
				t.Fatalf("at the pause the spec holds %d reviews and %d artifact.approved events of version %d, want the %d and %d before the edit: the write's batch wrote a synthetic carry", gotReviews, gotApprovals, number, reviews, approvals)
			}
			versionFillEvents(published, issue.Key)
			if edited := by.fill(t, service, database, hold, done, spec, number); edited.Code != http.StatusOK {
				t.Fatalf("the edit answered %d %s, want 200", edited.Code, edited.Body.String())
			}
			if after := newestCarryOf(t, database, spec); after.id != standing.id || after.from != nil {
				t.Errorf("after the fill the newest review of the spec is %+v, want alice's %s still: the fill wrote a review", after, standing.id)
			}
			digests := digestEventsOf(t, database, spec, number)
			if len(digests) != 1 {
				t.Fatalf("no artifact.digest for %s@v%d after its fill: the log holds %d, want the fill's one", refKeyOf(t, database, spec), number, len(digests))
			}
			if current, _ := digests[0].payload["approval_current"].(bool); !current {
				t.Errorf("the artifact.digest of %s@v%d carries approval_current false, want true: a prose edit leaves the spec query's record where alice approved it", refKeyOf(t, database, spec), number)
			}
			requireDigestPayload(t, database, "the fill of a write under an approval, by "+by.name, digests[0].payload, spec, number,
				versionFillWords(versionFillRowOf(t, database, spec, number).digest))
			if read := readApproval(t, handler, spec).Approval; read == nil || read.State != "approved" || read.Version == nil || *read.Version != standing.version {
				t.Errorf("the approval of the spec reads %+v after the fill, want approved at version %d, the one alice approved", read, standing.version)
			}
		})
	}

	for _, by := range versionFillers {
		t.Run("the cutover carry of R14, by "+by.name, func(t *testing.T) {
			requireCutoverCarryFilled(t, handler, database, service, project, by)
		})
	}

	t.Run("a fill the request lost, which the tick repairs", func(t *testing.T) {
		requireTheTickRepairsALostFill(t, handler, database, service, issue.Key, spec)
	})

	t.Run("rows that hold no record digest", func(t *testing.T) {
		// A version of a document no query takes: its row holds its cut and no record digest, and
		// owes none.
		uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "notes.md", "Notes no query takes.\n")
		notes := readDecisionDocument(t, database, issue.Key, "notes-md")
		noted := renderAtVersionNewest(t, database, notes.id)
		if row := versionFillRowOf(t, database, notes.id, noted); row.at == nil || row.digest != nil || row.owed || row.mismatch {
			t.Errorf("the row of version %d of notes.md, a document no query takes, holds at %v, record_sha256 %s, digest_owed %t and tree_mismatch %t, want a time, no record digest, digest_owed false and no mismatch",
				noted, row.at, versionFillWords(row.digest), row.owed, row.mismatch)
		}
		// A deferred version: no time and no mismatch, which a fill of the issue's owed digests
		// passes over.
		_, text := documentTexts(t, handler, spec)
		uploadAs(t, handler, issue.Key, documentName(t, database, spec), text+"\n"+strings.Repeat("- an item\n", 2001), "alice")
		deferred := renderAtVersionNewest(t, database, spec)
		requireDeferred := func(when string) {
			t.Helper()
			if row := versionFillRowOf(t, database, spec, deferred); row.at != nil || row.readSeq != nil || row.digest != nil || row.mismatch {
				t.Errorf("%s the row of the deferred version %d of the spec holds at %v, read_seq %v, record_sha256 %s and tree_mismatch %t, want no time, no seq, no digest and no mismatch",
					when, deferred, row.at, row.readSeq, versionFillWords(row.digest), row.mismatch)
			}
		}
		requireDeferred("once its upload answered")
		service.DrainOwedQueries(ctx, issue.Key)
		requireDeferred("after a fill of the issue's owed digests (docs.Service.DrainOwedQueries)")
	})
}
