package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// c-query-drain's own helpers.

// markEventWords is a mark's greatest_event as a failure names it: the event id, or null.
func markEventWords(event *int64) string {
	if event == nil {
		return "null"
	}
	return strconv.FormatInt(*event, 10)
}

// String is a mark as a failure prints it, its greatest_event by value.
func (mark queryMarkRow) String() string {
	return fmt.Sprintf("{greatest_event:%s greatest_seq:%d writer:%s %s}", markEventWords(mark.event), mark.seq, mark.writer.Kind, mark.writer.ID)
}

// digestEventsOf is every artifact.digest the log holds of version number of the document
// artifactID, in the log's order.
func digestEventsOf(t *testing.T, database *store.Store, artifactID string, number int) []hashGateEvent {
	t.Helper()
	var held []hashGateEvent
	for _, event := range loggedEvents(t, database, model.ArtifactDigestEventType, artifactID) {
		if version, _ := event.payload["version"].(float64); int(version) == number {
			held = append(held, event)
		}
	}
	return held
}

// requireDigestAfterVersion fails unless the log holds one artifact.digest of version number of
// the document artifactID, appended after that version's artifact.version, carrying the record
// digest the version's row holds, and answers that event's payload, nil where the log holds none.
func requireDigestAfterVersion(t *testing.T, database *store.Store, what, artifactID string, number int) map[string]any {
	t.Helper()
	versionEvent, _, held := documentEventOf(t, database, "artifact.version", artifactID, number)
	digests := digestEventsOf(t, database, artifactID, number)
	ids := make([]int64, len(digests))
	for index, digest := range digests {
		ids[index] = digest.id
	}
	if !held || len(digests) != 1 || digests[0].id <= versionEvent {
		t.Errorf("%s: version %d of %s holds the artifact.version %d (held %t) and the artifact.digest events %v in the log, want one artifact.digest appended after its artifact.version",
			what, number, refKeyOf(t, database, artifactID), versionEvent, held, ids)
	}
	if len(digests) == 0 {
		return nil
	}
	row := versionFillRowOf(t, database, artifactID, number)
	if got, _ := digests[0].payload["content_sha256"].(string); row.digest == nil || got != *row.digest {
		t.Errorf("%s: the artifact.digest of version %d of %s carries content_sha256 %q, want the record digest its row holds, %s",
			what, number, refKeyOf(t, database, artifactID), got, versionFillWords(row.digest))
	}
	return digests[0].payload
}

// approvalStandStatement reads the newest review of the document $1 and what a gate compares of
// it, as docs' newestReviewStand reads them for the fill's verdict (readDigestVerdict).
const approvalStandStatement = `select ` + docs.ReviewReadColumns + `, ` + docs.ReviewStandColumns + `
	from artifact_reviews` + docs.ReviewStandJoin + `
	where artifact_reviews.artifact_id = $1
	order by ` + model.ReviewOrder + ` limit 1`

// requireDigestPayload fails unless payload, the artifact.digest of version number of artifactID
// whose fill recorded digest, carries its seven members as the fill reads them, approval_current as
// ApprovalStand.Approves answers it of the newest review (none where no approval stands).
func requireDigestPayload(t *testing.T, database *store.Store, what string, payload map[string]any, artifactID string, number int, digest string) {
	t.Helper()
	if payload == nil {
		return
	}
	ref := refKeyOf(t, database, artifactID)
	if payload["artifact_id"] != artifactID || payload["name"] != documentName(t, database, artifactID) || payload["version"] != float64(number) || payload["content_sha256"] != digest {
		t.Errorf("%s: the artifact.digest of %s@v%d carries %v, want artifact_id %s, name %s, version %d and content_sha256 %s",
			what, ref, number, payload, artifactID, documentName(t, database, artifactID), number, digest)
	}
	current, held := payload["approval_current"].(bool)
	if !held {
		t.Errorf("%s: the artifact.digest of %s@v%d carries no approval_current: %v", what, ref, number, payload)
	}
	review, stand, err := docs.ScanReviewStand(database.Pool.QueryRow(context.Background(), approvalStandStatement, artifactID))
	if errors.Is(err, pgx.ErrNoRows) || err == nil && review.State != "approved" {
		_, version := payload["approved_version"]
		_, sha := payload["approved_sha256"]
		if current || version || sha {
			t.Errorf("%s: the artifact.digest of %s@v%d carries %v, want approval_current false and no approved_version or approved_sha256: no approval stands", what, ref, number, payload)
		}
		return
	}
	if err != nil {
		t.Fatalf("%s: read the newest review of %s and its stand: %v", what, ref, err)
	}
	stand.Current = &digest
	if review.Version == number {
		stand.DifferentDefinition = false
	}
	pin := cmp.Or(stand.Pin, review.ContentSHA256)
	if want := stand.Approves(review.Version, number); current != want || payload["approved_version"] != float64(review.Version) || pin == nil || payload["approved_sha256"] != *pin {
		t.Errorf("%s: the artifact.digest of %s@v%d carries %v, want approval_current %t as ApprovalStand.Approves reads %s's approval of version %d, approved_version %d and approved_sha256 %s",
			what, ref, number, payload, want, review.Actor.ID, review.Version, review.Version, versionFillWords(pin))
	}
}

// requireDrainCarriesNoApproval fails unless the drain that wrote version number of artifactID past
// approval, a person's approval of an earlier version, wrote no carried review and no
// artifact.approved, the approval reads stale, and the version's artifact.digest reads
// approval_current false.
func requireDrainCarriesNoApproval(t *testing.T, handler http.Handler, database *store.Store, what, artifactID string, number int, approval model.ArtifactReview) {
	t.Helper()
	ref := refKeyOf(t, database, artifactID)
	if reviews := listReviews(t, handler, artifactID); len(reviews) != 1 || reviews[0].ID != approval.ID {
		t.Errorf("%s: %s holds %d reviews, the newest %+v, want %s's approval %s alone: the drain writes no carried review", what, ref, len(reviews), reviews, approval.Actor.ID, approval.ID)
	}
	if approved := len(loggedEvents(t, database, "artifact.approved", artifactID)); approved != 1 {
		t.Errorf("%s: %s holds %d artifact.approved events, want the person's own approval's one", what, ref, approved)
	}
	if read := readApproval(t, handler, artifactID).Approval; read == nil || read.State != "stale" || read.Version == nil || *read.Version != approval.Version || read.LatestVersion != number {
		state, version, latest := "none", "none", 0
		if read != nil {
			state, latest = read.State, read.LatestVersion
			if read.Version != nil {
				version = strconv.Itoa(*read.Version)
			}
		}
		t.Errorf("%s: the approval of %s reads %s of version %s at the latest version %d, want %s's approval of version %d changed since (stale) at the drained version %d",
			what, ref, state, version, latest, approval.Actor.ID, approval.Version, number)
	}
	payload := requireDigestAfterVersion(t, database, what, artifactID, number)
	if current, _ := payload["approval_current"].(bool); payload != nil && (current || payload["approved_version"] != float64(approval.Version)) {
		t.Errorf("%s: the artifact.digest of %s@v%d carries approval_current %v beside approved_version %v, want false beside %d: the drained record moved off the approval",
			what, ref, number, payload["approval_current"], payload["approved_version"], approval.Version)
	}
}

// String is a review and its carry row as a failure prints them, by value.
func (carry versionFillCarry) String() string {
	from := "none"
	if carry.from != nil {
		from = strconv.Itoa(*carry.from)
	}
	return fmt.Sprintf("{review:%s version:%d state:%s pinned:%s carried_from:%s record_sha256:%s}", carry.id, carry.version, carry.state, carry.pinned, from, versionFillWords(carry.record))
}

// loggedEventID is the id of the newest event the log holds.
func loggedEventID(t *testing.T, database *store.Store) int64 {
	t.Helper()
	var id int64
	if err := database.Pool.QueryRow(context.Background(), `select coalesce(max(id), 0) from events`).Scan(&id); err != nil {
		t.Fatalf("read the newest event id: %v", err)
	}
	return id
}

// eventsNamingVersionAfter is the type of each event past the event id after that names version
// number of the document artifactID, in the log's order.
func eventsNamingVersionAfter(t *testing.T, database *store.Store, artifactID string, number int, after int64) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select type from events where id > $1 and payload->>'artifact_id' = $2
		  and coalesce((payload->'version'->>'number')::int, (payload->>'version')::int) = $3
		order by id`, after, artifactID, number)
	if err != nil {
		t.Fatalf("read the events of version %d of %s past event %d: %v", number, artifactID, after, err)
	}
	types, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the events of version %d of %s past event %d: %v", number, artifactID, after, err)
	}
	return types
}

// fillBy fills the digests of artifactID's version number while hold holds its write's fill: by the
// tick first where by is the tick's, then by the write's own fill once released.
func fillBy(t *testing.T, by versionFiller, service *docs.Service, database *store.Store, hold *versionFillHold, artifactID string, number int) {
	t.Helper()
	if by.tick {
		hold.ticking.Store(true)
		_, err := service.ReconcileBlockIndex(context.Background(), docs.BlockIndexCursors{}, nil)
		hold.ticking.Store(false)
		if err != nil {
			t.Fatalf("a tick of the block-index reconciliation: %v", err)
		}
		if row := versionFillRowOf(t, database, artifactID, number); row.owed || row.digest == nil {
			t.Errorf("digest_owed still true after the tick: version %d of %s holds record_sha256 %s and digest_owed %t while its write's fill is held",
				number, refKeyOf(t, database, artifactID), versionFillWords(row.digest), row.owed)
		}
	}
	hold.release()
}

// requireCutoverCarryFilled is the cutover carry of R14, filled by by: alice approves the spec of an
// unconverted issue, and its conversion (docs.Service.ConvertIssue) carries the approval to the
// version it writes, pinned to its stored markdown, its record digest owed. At the pause before the
// fill the carry row holds no record digest; after it the same review's
// artifact_review_carries.record_sha256 holds it, the pin and alice's review are unchanged, no
// review or artifact.approved is added, the fill's one event is its artifact.digest, and the
// approval stands on the carry row's digest.
func requireCutoverCarryFilled(t *testing.T, handler http.Handler, database *store.Store, service *docs.Service, project string, by versionFiller) {
	t.Helper()
	issue := createDigestIssue(t, handler, database, project, "A cutover carry filled by "+by.name, ":::requirement{#r1}\nThe carried list comes first.\n:::\n", true)
	spec := issue.PrimaryArtifactID
	original := reviewDocument(t, handler, spec, "alice", "approved", "")
	settleIssue(t, service, issue.Key)
	reconcileUntilNothingOwed(t, handler, Deps{Docs: service}, issue.Key)
	hold := versionFillPause(t, service, issue.Key)
	converted := make(chan error, 1)
	go func() {
		result, err := service.ConvertIssue(context.Background(), issue.Key, docs.IssueRoomGate, false)
		if err == nil && result.Outcome != docs.IssueConverted {
			err = fmt.Errorf("the conversion answered %+v", result)
		}
		converted <- err
	}()
	testwait.Receive(t, "the fill of the conversion's version, held", hold.held)
	number := renderAtVersionNewest(t, database, spec)
	stored := contentHash(storedVersionMarkdown(t, database, spec, number))
	carry, row := newestCarryOf(t, database, spec), versionFillRowOf(t, database, spec, number)
	if number <= original.Version || !row.owed || carry.id == original.ID || carry.version != number || carry.state != "approved" ||
		carry.from == nil || *carry.from != original.Version || carry.record != nil || carry.pinned != stored {
		t.Fatalf("at the pause after the conversion's COMMIT version %d of %s owes its digest %t and its newest review is %+v (record %s), want alice's approval %s carried from version %d to version %d, pinned to its stored markdown's digest %s, with no record digest on its carry row yet",
			number, refKeyOf(t, database, spec), row.owed, carry, versionFillWords(carry.record), original.ID, original.Version, number, stored)
	}
	reviews, approvals := versionFillReviewCounts(t, database, spec, number)
	paused := loggedEventID(t, database)
	fillBy(t, by, service, database, hold, spec, number)
	if err := testwait.Receive(t, "the conversion's answer", converted); err != nil {
		t.Fatalf("the conversion of %s: %v", issue.Key, err)
	}
	filled := versionFillRowOf(t, database, spec, number)
	after := newestCarryOf(t, database, spec)
	if filled.owed || filled.digest == nil || after.id != carry.id || after.record == nil || *after.record != *filled.digest || after.pinned != carry.pinned {
		t.Errorf("after the fill by %s version %d holds record_sha256 %s (owed %t) and the newest review of %s is %+v (record %s), want the same review %s, pinning %s still, with the record's digest on its artifact_review_carries.record_sha256",
			by.name, number, versionFillWords(filled.digest), filled.owed, refKeyOf(t, database, spec), after, versionFillWords(after.record), carry.id, carry.pinned)
	}
	for _, review := range listReviews(t, handler, spec) {
		if review.ID == original.ID && (review.Version != original.Version || review.State != "approved" || versionFillWords(review.ContentSHA256) != versionFillWords(original.ContentSHA256)) {
			t.Errorf("alice's original approval reads version %d, %s, pinning %s after the fill, want version %d, approved, pinning %s",
				review.Version, review.State, versionFillWords(review.ContentSHA256), original.Version, versionFillWords(original.ContentSHA256))
		}
	}
	if gotReviews, gotApprovals := versionFillReviewCounts(t, database, spec, number); gotReviews != reviews || gotApprovals != approvals {
		t.Errorf("the fill by %s left %s holding %d reviews and %d artifact.approved events of version %d, want the %d and %d at the pause: no second review and no artifact.approved",
			by.name, refKeyOf(t, database, spec), gotReviews, gotApprovals, number, reviews, approvals)
	}
	if types := eventsNamingVersionAfter(t, database, spec, number, paused); len(types) != 1 || types[0] != model.ArtifactDigestEventType {
		t.Errorf("the fill by %s appended %v of version %d of %s, want its one artifact.digest", by.name, types, number, refKeyOf(t, database, spec))
	}
	if digests := digestEventsOf(t, database, spec, number); len(digests) == 1 && filled.digest != nil {
		if current, _ := digests[0].payload["approval_current"].(bool); !current {
			t.Errorf("the artifact.digest of %s@v%d carries approval_current false, want true: the carried approval stands on the carry row's digest", refKeyOf(t, database, spec), number)
		}
		requireDigestPayload(t, database, "the fill of the cutover carry by "+by.name, digests[0].payload, spec, number, *filled.digest)
	}
	if read := readApproval(t, handler, spec).Approval; read == nil || read.State != "approved" || read.Version == nil || *read.Version != number {
		t.Errorf("the approval of %s reads %+v after the fill, want approved at the converted version %d", refKeyOf(t, database, spec), read, number)
	}
	current := "none"
	if read := artifactDigestApproval(t, handler, spec).Approval; read != nil {
		current = versionFillWords(read.CurrentSHA256)
	}
	if after.record == nil || current != *after.record || current == carry.pinned {
		t.Errorf("the artifact read of %s answers current_sha256 %s after the fill, want the carry row's record digest %s, not the review's pin %s",
			refKeyOf(t, database, spec), current, versionFillWords(after.record), carry.pinned)
	}
}

// requireTheTickRepairsALostFill: an unchanged upload of the spec of key writes a version owing its
// digest whose room pass states nothing, so it marks no query; its request's fill held, only the
// tick's repair of lost fills (docs' fillLostDigests) fills it, at its saved cut, with one
// artifact.digest.
func requireTheTickRepairsALostFill(t *testing.T, handler http.Handler, database *store.Store, service *docs.Service, key, spec string) {
	t.Helper()
	ctx := context.Background()
	path, name := "/api/v1/issues/"+key+"/artifacts", documentName(t, database, spec)
	live, _ := documentTexts(t, handler, spec)
	before := renderAtVersionNewest(t, database, spec)
	body := map[string]any{"name": name, "content": live, "precondition": uploadBaseNamed(t, handler, path, name)}
	hold := versionFillPause(t, service, key)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- dispatchRequest(t, handler, http.MethodPost, path, body, "alice") }()
	testwait.Receive(t, "the fill of the unchanged upload's version, held", hold.held)
	number := renderAtVersionNewest(t, database, spec)
	row := versionFillRowOf(t, database, spec, number)
	if mark, marked := readQueryMarkRow(t, database, key); number != before+1 || !row.owed || row.at == nil || row.readSeq == nil || marked {
		t.Fatalf("the unchanged upload left %s at version %d (want %d) owing its digest %t, with a mark %s standing %t: want a version owing its digest and no mark, so that only the tick's repair of lost fills fills it, or this case proves nothing",
			refKeyOf(t, database, spec), number, before+1, row.owed, mark, marked)
	}
	saved, err := docs.ReadQueryRecord(ctx, database.Pool, key, graphstore.QuerySpec, refs.TreeCut{At: *row.at, ReadSeq: *row.readSeq})
	if err != nil {
		t.Fatalf("read the spec query's record at the row's cut: %v", err)
	}
	fillBy(t, versionFiller{name: "the tick", tick: true}, service, database, hold, spec, number)
	if filled := versionFillRowOf(t, database, spec, number); filled.digest == nil || *filled.digest != saved.Digest {
		t.Errorf("after the tick version %d of %s holds record_sha256 %s, want the record's digest at the saved cut, %s", number, refKeyOf(t, database, spec), versionFillWords(filled.digest), saved.Digest)
	}
	if digests := digestEventsOf(t, database, spec, number); len(digests) != 1 {
		t.Errorf("no artifact.digest for %s@v%d after its fill: the log holds %d, want the tick's one", refKeyOf(t, database, spec), number, len(digests))
	} else {
		requireDigestPayload(t, database, "the tick's repair of a lost fill", digests[0].payload, spec, number, saved.Digest)
	}
	if answer := testwait.Receive(t, "the unchanged upload's answer", done); answer.Code != http.StatusCreated {
		t.Errorf("the unchanged upload of %s answered %d %s, want 201", refKeyOf(t, database, spec), answer.Code, answer.Body.String())
	}
}
