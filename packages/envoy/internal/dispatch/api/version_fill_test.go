package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
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
// connection of its own: its cut, its record's digest and whether that digest is owed.
type versionFillRow struct {
	at      *time.Time
	readSeq *int64
	digest  *string
	owed    bool
}

// versionFillRowOf reads the version_renders row of version number of artifactID.
func versionFillRowOf(t *testing.T, database *store.Store, artifactID string, number int) versionFillRow {
	t.Helper()
	var row versionFillRow
	if err := database.Pool.QueryRow(context.Background(), `
		select at, read_seq, record_sha256, digest_owed from version_renders where artifact_id = $1::uuid and number = $2`,
		artifactID, number).Scan(&row.at, &row.readSeq, &row.digest, &row.owed); err != nil {
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

// versionFillPause holds the next fill of the issue key at its start (docs.Service.HoldFills): it
// answers a channel that receives once that fill is held, and a release that lets it go on.
func versionFillPause(t *testing.T, service *docs.Service, key string) (<-chan struct{}, func()) {
	t.Helper()
	held, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	service.HoldFills(func(issueKey string) {
		if issueKey != key {
			return
		}
		once.Do(func() {
			close(held)
			<-release
		})
	})
	return held, func() { close(release) }
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

// TestVersionRecordDigestFillsAfterCommit writes a version of the spec of a converted issue through
// the edit route, a version a query's record covers, and holds the fill after the write's COMMIT
// and before the fill begins. The committed version_renders row then holds its time and its
// read_seq, no digest and digest_owed true, and artifact_version_text_sha256 answers the sha256 of
// the version's stored markdown; the server has published no artifact.version of it yet. The case
// then moves the head record of the spec query, appending a placement value to the spec's
// requirement by SQL, and lets the fill go: the fill records the digest of the record at the cut
// the row saved, not the head's, clears digest_owed, and the function answers that digest from
// then on; and only then does the server publish the version's artifact.version. That ordinary
// fill appends no event. A second edit is held the same way, and alice approves its version while
// the fill is held, which pins the digest of its stored markdown: the fill then carries her
// approval at that version, and appends the carry's artifact.approved and no other event; the
// carried review names the version itself.
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
	awaitSettled(t, database, spec)
	requirement := blockAddressOf(t, database, issue.Key, "r1")
	service := deps.Docs.(*docs.Service)
	published, unsubscribe := deps.Events.Subscribe()
	defer unsubscribe()
	defer service.HoldFills(nil)

	// The ordinary fill.
	before := renderAtVersionNewest(t, database, spec)
	held, release := versionFillPause(t, service, issue.Key)
	done := versionFillEdit(t, handler, spec, "The opening words.", "The first words.")
	testwait.Receive(t, "the fill of the edit's version", held)
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
	if early := versionFillEvents(published, issue.Key)["artifact.version"]; early > 0 {
		t.Errorf("the server published %d artifact.version events of %s before the fill, want none", early, issue.Key)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
		values ($1, $2, $3, $4, $5, now(), '{"kind":"user","id":"alice"}'::jsonb, 0, true)`,
		refs.BlockNodeKind, requirement, refs.AttributePlacement, graphstore.KindValue, refs.PlacementStored); err != nil {
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
	release()
	if edited := testwait.Receive(t, "the edit's answer", done); edited.Code != http.StatusOK {
		t.Fatalf("the edit answered %d %s, want 200", edited.Code, edited.Body.String())
	}
	if filled := versionFillRowOf(t, database, spec, number); filled.digest == nil || *filled.digest != saved.Digest || filled.owed {
		t.Errorf("after the fill the row of version %d holds record_sha256 %v and digest_owed %t, want the record's digest at the saved cut %s and false",
			number, filled.digest, filled.owed, saved.Digest)
	}
	if got := versionFillDigest(t, database, spec, number); got != saved.Digest {
		t.Errorf("artifact_version_text_sha256 answers %s for version %d after the fill, want the record's digest %s", got, number, saved.Digest)
	}
	events := versionFillEvents(published, issue.Key)
	if events["artifact.version"] == 0 {
		t.Errorf("the server published no artifact.version of %s once the edit answered", issue.Key)
	}
	if approved := events["artifact.approved"]; approved > 0 {
		t.Errorf("the ordinary fill of version %d appended %d artifact.approved events, want none", number, approved)
	}

	// The fill that carries an approval given between the COMMIT and the fill.
	held, release = versionFillPause(t, service, issue.Key)
	done = versionFillEdit(t, handler, spec, "The first words.", "The second words.")
	testwait.Receive(t, "the fill of the second edit's version", held)
	second := renderAtVersionNewest(t, database, spec)
	approval := reviewDocument(t, handler, spec, "alice", "approved", "")
	if want := contentHash(storedVersionMarkdown(t, database, spec, second)); approval.Version != second || approval.ContentSHA256 == nil || *approval.ContentSHA256 != want {
		t.Errorf("alice's approval given before the fill is of version %d pinning %v, want version %d pinning the stored markdown's %s",
			approval.Version, approval.ContentSHA256, second, want)
	}
	versionFillEvents(published, issue.Key)
	release()
	if edited := testwait.Receive(t, "the second edit's answer", done); edited.Code != http.StatusOK {
		t.Fatalf("the second edit answered %d %s, want 200", edited.Code, edited.Body.String())
	}
	if approved := versionFillEvents(published, issue.Key)["artifact.approved"]; approved != 1 {
		t.Errorf("the fill that carries alice's approval appended %d artifact.approved events, want the carry's one", approved)
	}
	carried := requireReviewCarriedWith(t, handler, spec, second, second, restoreSentence(second), nil)
	var pinned string
	if err := database.Pool.QueryRow(ctx, `select coalesce(content_sha256, '') from artifact_reviews where id = $1::uuid`, carried.ID).Scan(&pinned); err != nil {
		t.Fatalf("read the carried review's digest: %v", err)
	}
	if filled := versionFillRowOf(t, database, spec, second); filled.digest == nil || pinned != *filled.digest {
		t.Errorf("the carried review of version %d pins %q, want the record's digest the fill recorded, %v", second, pinned, filled.digest)
	}
}
