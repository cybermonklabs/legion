package api

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// indexedEvent is one artifact.indexed as the event log stores it.
type indexedEvent struct {
	id      int64
	actor   model.Actor
	notify  bool
	payload model.ArtifactIndexedEventPayload
}

// indexedEvents reads every artifact.indexed of the issue issueKey, oldest first.
func indexedEvents(t *testing.T, database *store.Store, issueKey string) []indexedEvent {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select id, actor, notify, payload from events
		where issue_key = $1 and type = 'artifact.indexed'
		order by id
	`, issueKey)
	if err != nil {
		t.Fatalf("read the artifact.indexed events of %s: %v", issueKey, err)
	}
	defer rows.Close()
	var read []indexedEvent
	for rows.Next() {
		var event indexedEvent
		var actor, payload []byte
		if err := rows.Scan(&event.id, &actor, &event.notify, &payload); err != nil {
			t.Fatalf("scan an artifact.indexed of %s: %v", issueKey, err)
		}
		if err := json.Unmarshal(actor, &event.actor); err != nil {
			t.Fatalf("decode the actor of an artifact.indexed of %s: %v", issueKey, err)
		}
		if err := json.Unmarshal(payload, &event.payload); err != nil {
			t.Fatalf("decode the payload of an artifact.indexed of %s: %v", issueKey, err)
		}
		read = append(read, event)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the artifact.indexed events of %s: %v", issueKey, err)
	}
	return read
}

// versionEventOf is the id of the newest artifact.version of artifactID on the issue issueKey and
// the version number it names.
func versionEventOf(t *testing.T, database *store.Store, issueKey, artifactID string) (int64, int) {
	t.Helper()
	var id int64
	var number int
	if err := database.Pool.QueryRow(context.Background(), `
		select id, (payload->'version'->>'number')::int from events
		where issue_key = $1 and type = 'artifact.version' and payload->>'artifact_id' = $2
		order by id desc limit 1
	`, issueKey, artifactID).Scan(&id, &number); err != nil {
		t.Fatalf("read the newest artifact.version of %s on %s: %v", artifactID, issueKey, err)
	}
	return id, number
}

// TestAnIndexThatClearsAnOwedDocumentAppendsOneQuietIndexedEvent: the block-index
// reconciliation's pass that clears the block index a version past the bound (docs'
// deferBlockIndexBlocks) left owed appends one artifact.indexed on the document's issue, naming
// the document, in the pass's own transaction, so it is in the event log once that commits and in
// no log while the document owes its index. It is quiet, written by the reconciliation's own
// actor (docs.BlockIndexActor), after the version's own artifact.version and with no
// artifact.version of its own; the event feed carries it, and the Broker publishes it once the
// pass has committed. A version its own write indexes owes nothing and appends none, and a pass
// that clears nothing appends none (spec requirement r-owed-coverage: the page that drew the owed
// line reads the coverage again on this event).
func TestAnIndexThatClearsAnOwedDocumentAppendsOneQuietIndexedEvent(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{settle: time.Hour}, nil)
	handler, database, deps := rig.handler, rig.database, rig.served.deps
	createReferenceAPIProject(t, handler, "INDX")
	issue := createSpecIssue(t, handler, "INDX", owedRequirement)
	key, spec := issue.Key, issue.PrimaryArtifactID
	owner := "/api/v1/issues/" + key
	reconcileUntilNothingOwed(t, handler, deps, key)
	before := len(indexedEvents(t, database, key))

	// A version under the bound is indexed by its own write: it owes nothing, and neither the
	// write nor a pass after it appends an artifact.indexed.
	uploadDocument(t, handler, owner, "spec.md", owedRequirement+"\nA paragraph its own write indexes.\n")
	requireNothingOwed(t, handler, key, "after a version its own write indexed")
	reconcileBlockIndexPass(t, deps)
	if got := indexedEvents(t, database, key); len(got) != before {
		t.Fatalf("a version its own write indexed left %d artifact.indexed events on %s, want the %d before it: %+v", len(got), key, before, got)
	}

	// A version past the bound owes its index, and nothing says it was indexed while it does.
	written := uploadDocument(t, handler, owner, "spec.md", owedRequirement+"\n"+deferralList(itemsOverTheBlockBound, "end"))
	version := written.Version.Number
	requireOwedNamed(t, handler, key, spec, version, "the upload of spec.md past the bound")
	if got := indexedEvents(t, database, key); len(got) != before {
		t.Fatalf("while spec.md owes its index at version %d, %s holds %d artifact.indexed events, want the %d before it: %+v", version, key, len(got), before, got)
	}
	versionEvent, named := versionEventOf(t, database, key, spec)
	if named != version {
		t.Fatalf("the newest artifact.version of spec.md on %s names version %d, want the upload's %d", key, named, version)
	}

	client := rig.dial(t, withBearer("agent-token"))
	subscribeEvents(t, client, "events", new(eventHead(t, database)))
	published, unsubscribe := deps.Events.Subscribe()
	defer unsubscribe()

	tick := reconcileBlockIndexPass(t, deps)
	requireNothingOwed(t, handler, key, "once one pass indexed spec.md")
	got := indexedEvents(t, database, key)
	if len(got) != before+1 {
		t.Fatalf("the pass that indexed spec.md at version %d (%+v) left %d artifact.indexed events on %s, want one more than the %d before it: %+v", version, tick, len(got), key, before, got)
	}
	indexed := got[len(got)-1]
	if !slices.Contains(indexed.payload.ArtifactIDs, spec) || !slices.IsSorted(indexed.payload.ArtifactIDs) {
		t.Errorf("the artifact.indexed of %s names %v, want spec.md %s among them, sorted", key, indexed.payload.ArtifactIDs, spec)
	}
	if indexed.notify || indexed.actor.Kind != docs.BlockIndexActor.Kind || indexed.actor.ID != docs.BlockIndexActor.ID {
		t.Errorf("the artifact.indexed of %s notifies=%v, written by %+v, want a quiet event written by %+v", key, indexed.notify, indexed.actor, docs.BlockIndexActor)
	}
	if indexed.id < versionEvent {
		t.Errorf("the artifact.indexed of %s is event %d, before the artifact.version %d of the version it indexed", key, indexed.id, versionEvent)
	}
	if latest, number := versionEventOf(t, database, key, spec); latest != versionEvent || number != version {
		t.Errorf("once indexed, the newest artifact.version of spec.md on %s is event %d naming version %d, want the upload's own, event %d naming %d: the pass wrote no version", key, latest, number, versionEvent, version)
	}

	// The feed carries the committed event as the log stores it.
	for read := 0; ; read++ {
		if read == 20 {
			t.Fatalf("the event feed carried no artifact.indexed %d within %d events", indexed.id, read)
		}
		frame := eventFeedEvent(t, client, "events")
		var event struct {
			ID      int64                             `json:"id"`
			Type    string                            `json:"type"`
			Notify  bool                              `json:"notify"`
			Payload model.ArtifactIndexedEventPayload `json:"payload"`
		}
		if err := json.Unmarshal(frame.Event, &event); err != nil {
			t.Fatalf("decode event frame %+v: %v", frame, err)
		}
		if event.ID != indexed.id {
			continue
		}
		if event.Type != "artifact.indexed" || event.Notify || !slices.Equal(event.Payload.ArtifactIDs, indexed.payload.ArtifactIDs) {
			t.Errorf("the event feed carried event %d as %+v, want the stored artifact.indexed %+v", event.ID, event, indexed)
		}
		break
	}

	// The Broker publishes it once the pass's transaction has committed.
	for {
		select {
		case event := <-published:
			if event.Type != "artifact.indexed" {
				continue
			}
			if event.ID != indexed.id {
				t.Errorf("the Broker published the artifact.indexed %d, want the stored %d", event.ID, indexed.id)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("the Broker published no artifact.indexed within 5 s of the pass that appended %d", indexed.id)
		}
		break
	}

	// A pass that finds nothing owed clears nothing and appends nothing.
	reconcileBlockIndexPass(t, deps)
	if again := indexedEvents(t, database, key); len(again) != len(got) {
		t.Errorf("a pass with nothing owed left %d artifact.indexed events on %s, want the %d it found: %+v", len(again), key, len(got), again)
	}
}
