package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/files/filestest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// documentEventAncestorsFixture is where TestDocumentEventsNameTheirIssueAncestors commits the
// payloads it stored, from this package's directory: the contracts' Bun tests parse each one under
// its event type's schema, so the schemas are held to what the server stores rather than to a
// payload written by hand.
var documentEventAncestorsFixture = filepath.Join("..", "..", "..", "..", "contracts", "fixtures", "document-event-ancestors.json")

// documentEventAncestors is that fixture: the keys of the four issues the test writes, by their
// place in the tree, and one stored payload for each event type and each kind of owner, with the
// version events of the conversion and of the drain.
type documentEventAncestors struct {
	Issues map[string]string             `json:"issues"`
	Events []documentEventAncestorsEvent `json:"events"`
}

// documentEventAncestorsEvent is one stored payload of the fixture: whose document it is
// (grandchild, converted grandchild, root or project), its event type, and the payload as the
// events table holds it.
type documentEventAncestorsEvent struct {
	Owner   string          `json:"owner"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// storedDocumentEvent is one row of the events table as it is stored: its type, the owner it is
// stored under - an issue's key, or the id of a document no issue holds (events_one_owner) - and
// its payload's JSON.
type storedDocumentEvent struct {
	eventType  string
	issueKey   *string
	artifactID *string
	payload    json.RawMessage
}

// readStoredDocumentEvent reads the stored event id.
func readStoredDocumentEvent(t *testing.T, database *store.Store, id int64) storedDocumentEvent {
	t.Helper()
	var event storedDocumentEvent
	if err := database.Pool.QueryRow(context.Background(), `
		select type, issue_key, artifact_id::text, payload from events where id = $1
	`, id).Scan(&event.eventType, &event.issueKey, &event.artifactID, &event.payload); err != nil {
		t.Fatalf("read the stored event %d: %v", id, err)
	}
	return event
}

// members is the event's payload decoded one level down, each member's JSON as stored, so a test
// tells a member that is absent from one that holds null.
func (e storedDocumentEvent) members(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	var members map[string]json.RawMessage
	if err := json.Unmarshal(e.payload, &members); err != nil {
		t.Fatalf("decode the stored %s payload %s: %v", e.eventType, e.payload, err)
	}
	return members
}

// fixture is the event as the fixture holds it, for owner.
func (e storedDocumentEvent) fixture(owner string) documentEventAncestorsEvent {
	return documentEventAncestorsEvent{Owner: owner, Type: e.eventType, Payload: e.payload}
}

// requireIssueAncestors fails t unless event, the stored event of what, is stored under the issue
// issueKey and its payload's ancestors are chain, the ancestors of that issue's own issue.created:
// the one walk names both. The stored JSON is compared, not only a decoded list, so a root's chain
// must be stored as [] and never as null.
func requireIssueAncestors(t *testing.T, what string, event storedDocumentEvent, issueKey string, chain []string) {
	t.Helper()
	if deref(event.issueKey) != issueKey || event.artifactID != nil {
		t.Errorf("the %s of %s is stored under issue %q and document %q, want under the issue %s alone",
			event.eventType, what, deref(event.issueKey), deref(event.artifactID), issueKey)
	}
	want, err := json.Marshal(chain)
	if err != nil {
		t.Fatalf("encode the chain %#v: %v", chain, err)
	}
	raw, held := event.members(t)["ancestors"]
	if !held {
		t.Errorf("the stored %s payload of %s has no ancestors, want %s, the ancestors of %s's own issue.created: %s",
			event.eventType, what, want, issueKey, event.payload)
		return
	}
	var stored bytes.Buffer
	if err := json.Compact(&stored, raw); err != nil || !bytes.Equal(stored.Bytes(), want) {
		t.Errorf("the stored %s payload of %s holds ancestors %s, want %s, the ancestors of %s's own issue.created",
			event.eventType, what, raw, want, issueKey)
	}
}

// requireNoAncestors fails t unless event, the stored event of what, is stored under the document
// artifactID, which no issue holds (documentOwner), and its payload holds no ancestors member at
// all: an artifact of no issue has no chain, and its payload stays as it was before the member.
func requireNoAncestors(t *testing.T, what string, event storedDocumentEvent, artifactID string) {
	t.Helper()
	if event.issueKey != nil || deref(event.artifactID) != artifactID {
		t.Errorf("the %s of %s is stored under issue %q and document %q, want under the document %s alone",
			event.eventType, what, deref(event.issueKey), deref(event.artifactID), artifactID)
	}
	if raw, held := event.members(t)["ancestors"]; held {
		t.Errorf("the stored %s payload of %s holds ancestors %s, want no ancestors member: the document belongs to no issue: %s",
			event.eventType, what, raw, event.payload)
	}
}

// Each artifact.created, artifact.version and artifact.removed event of an artifact that belongs
// to an issue names the issue's ancestors, parent first, as the issue's own events do: a page open
// on an ancestor then reaches its tree and coverage from the event alone, whatever it holds of the
// issue. A root's events name [] and an artifact of no issue's name none, so a reader tells "no
// ancestors" from "no chain". The test writes a root, a child and two grandchildren of project
// TEST, the second created unconverted and converted later, and reads the stored payload of the
// event of each path that appends one, one subtest for each:
//
//   - W1 and W2, an upload that creates a document and one that versions it (storeArtifact);
//   - W3, a named version, whose route is one of the callers of commitArtifactVersionEvent;
//   - W4, the settlement of a document in a room of its own, which no document_rooms row joins to
//     the issue's room, and W5, the settlement of a document of the issue's room: the two writes of
//     a settlement;
//   - W6, the decisions step's restore and conversion (issueRestore.writeVersion);
//   - W7, the removal of an untouched document;
//   - W8, W1 and W2 on the root's document, whose chain is [];
//   - W9, W1 and W2 on a project's document, which no issue holds, its events stored under the
//     document itself (documentOwner): no ancestors member;
//   - W10, W1 and W2 on a picture of the grandchild: an issue's file names the chain as its
//     documents do;
//   - W11, the conversion of the second grandchild, which versions its spec as it takes it into
//     the issue's room (issueConversion.finish);
//   - W12, the drain's revision of the second grandchild's Verification document, whose record a
//     recorded result moved (drainQueryMark's finishRevision).
//
// The chain of every issue-owned row is compared with the ancestors of that issue's own
// issue.created. Under -update the test writes packages/contracts/fixtures/document-event-ancestors.json
// from the payloads it stored, and without it fails where they differ from that file.
func TestDocumentEventsNameTheirIssueAncestors(t *testing.T) {
	ctx := context.Background()
	// Settlement waits an hour, so the only settlements are the two rows' own (docs.SettleNow) and
	// each version a row reads is the one its path wrote. The browser peers type W4's and W5's
	// edits, and the file store holds W10's picture.
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour, browserPeers: true, files: filestest.NewMemory()})
	documents := deps.Docs.(*docs.Service)
	root := createInteractionIssue(t, handler, "TEST", "Root of the tree", "A spec")
	child := createTitledChild(t, handler, "Middle of the tree", root.Key)
	grandchild, room := createRoomIssue(t, handler, database, "TEST", "Leaf of the tree", "The leaf's words.\n", child.Key)
	if room == "" {
		t.Fatalf("%s was created with no issue room, which W5 settles", grandchild.Key)
	}
	chain := newestEventPayload[issueEventFields](t, database, "issue.created", grandchild.Key).Ancestors
	if want := []string{child.Key, root.Key}; !slices.Equal(chain, want) {
		t.Fatalf("the grandchild's issue.created ancestors = %#v, want %#v", chain, want)
	}
	rootChain := newestEventPayload[issueEventFields](t, database, "issue.created", root.Key).Ancestors
	if rootChain == nil || len(rootChain) != 0 {
		t.Fatalf("the root's issue.created ancestors = %#v, want []", rootChain)
	}

	grandchildPath := "/api/v1/issues/" + grandchild.Key
	notes := uploadDocument(t, handler, grandchildPath, "notes.md", "# Notes\n\nOne.\n").Artifact
	created := readStoredDocumentEvent(t, database, versionEvent(t, database, notes.ID, 1))
	t.Run("W1 the upload that creates a document", func(t *testing.T) {
		requireIssueAncestors(t, "notes.md's first upload", created, grandchild.Key, chain)
	})

	uploadDocument(t, handler, grandchildPath, "notes.md", "# Notes\n\nTwo.\n")
	versioned := readStoredDocumentEvent(t, database, versionEvent(t, database, notes.ID, 2))
	t.Run("W2 the upload that versions a document", func(t *testing.T) {
		requireIssueAncestors(t, "notes.md's second upload", versioned, grandchild.Key, chain)
	})

	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+notes.ID+"/versions", map[string]string{"summary": "A checkpoint"}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("name a version of notes.md: status=%d body=%s", response.Code, response.Body.String())
	}
	named := decodeBody[model.Version](t, response).Number
	namedEvent := readStoredDocumentEvent(t, database, versionEvent(t, database, notes.ID, named))
	t.Run("W3 a named version through commitArtifactVersionEvent", func(t *testing.T) {
		requireIssueAncestors(t, "notes.md's named version", namedEvent, grandchild.Key, chain)
	})

	// notes.md is no role document, so it keeps a room of its own in a converted issue
	// (documentJoinsIssueRoom), and its settlement is the first write.
	if own, _ := documentRoomOf(t, database.Pool, notes.ID); own != notes.ID {
		t.Fatalf("notes.md of %s is edited in the room %s, want a room of its own, which no document_rooms row joins", grandchild.Key, own)
	}
	typist := connectBrowserPeer(t, handler, documents, notes.ID)
	t.Cleanup(typist.close)
	typist.appendParagraph(t, "Typed in a browser.")
	typist.barrier(t)
	settledVersion(t, handler, documents, notes.ID, named+1)
	ownRoomSettled := readStoredDocumentEvent(t, database, versionEvent(t, database, notes.ID, named+1))
	t.Run("W4 the settlement of a document in a room of its own", func(t *testing.T) {
		requireIssueAncestors(t, "notes.md's settled version", ownRoomSettled, grandchild.Key, chain)
	})

	// The spec is a fragment of the issue's room, so its settlement is the room's, the second
	// write. The create wrote its version 1.
	spec := grandchild.PrimaryArtifactID
	if specRoom, _ := documentRoomOf(t, database.Pool, spec); specRoom != room {
		t.Fatalf("the spec of %s is edited in the room %s, want the issue's room %s", grandchild.Key, specRoom, room)
	}
	roomTypist := connectBrowserPeer(t, handler, documents, spec)
	t.Cleanup(roomTypist.close)
	roomTypist.appendParagraph(t, "Typed in the issue's room.")
	roomTypist.barrier(t)
	settledVersion(t, handler, documents, spec, 2)
	roomSettled := readStoredDocumentEvent(t, database, versionEvent(t, database, spec, 2))
	t.Run("W5 the settlement of a document of the issue room", func(t *testing.T) {
		requireIssueAncestors(t, "the spec's settled version", roomSettled, grandchild.Key, chain)
	})

	// The decisions step converts the one section of the grandchild's decisions document into a
	// decision of its spec, which it writes as a version of the spec.
	seedDecisionsDocument(t, database, documents, grandchild.Key,
		"# Decisions\n\n## The census runs first\n\nThe census reads the corpus before any view of it.\n")
	step := docs.NewDecisionStep(database, "", nil)
	step.UseDocuments(documents)
	done, failed, err := step.RestoreAndConvert(ctx, "TEST", true, false)
	if err != nil || len(failed) > 0 || len(done) != 1 || done[0].Issue != grandchild.Key || done[0].SpecVersion == 0 {
		t.Fatalf("the decisions step answered %+v, %+v, %v, want %s converted with a version of its spec", done, failed, err, grandchild.Key)
	}
	restored := readStoredDocumentEvent(t, database, versionEvent(t, database, spec, done[0].SpecVersion))
	t.Run("W6 the restore of the decisions step", func(t *testing.T) {
		requireIssueAncestors(t, "the spec's version the decisions step wrote", restored, grandchild.Key, chain)
	})

	untouched := emptyDocument(t, handler, grandchild.Key, "Launch notes")
	removedDocument(t, handler, untouched.ID)
	removed := readStoredDocumentEvent(t, database, artifactEvent(t, database, "artifact.removed", untouched.ID))
	t.Run("W7 the removal of an untouched document", func(t *testing.T) {
		requireIssueAncestors(t, "the removal of Launch notes", removed, grandchild.Key, chain)
	})

	rootPath := "/api/v1/issues/" + root.Key
	rootNotes := uploadDocument(t, handler, rootPath, "notes.md", "# Root notes\n\nOne.\n").Artifact
	uploadDocument(t, handler, rootPath, "notes.md", "# Root notes\n\nTwo.\n")
	rootCreated := readStoredDocumentEvent(t, database, versionEvent(t, database, rootNotes.ID, 1))
	rootVersioned := readStoredDocumentEvent(t, database, versionEvent(t, database, rootNotes.ID, 2))
	t.Run("W8 the uploads of a document of the root", func(t *testing.T) {
		requireIssueAncestors(t, "the root's notes.md's first upload", rootCreated, root.Key, rootChain)
		requireIssueAncestors(t, "the root's notes.md's second upload", rootVersioned, root.Key, rootChain)
	})

	// A project's document is the one document no issue holds: its events are stored under the
	// document itself (ownerForArtifact's documentOwner).
	projectPath := "/api/v1/projects/TEST"
	design := uploadDocument(t, handler, projectPath, "design.md", "# Design\n\nOne.\n").Artifact
	uploadDocument(t, handler, projectPath, "design.md", "# Design\n\nTwo.\n")
	projectCreated := readStoredDocumentEvent(t, database, versionEvent(t, database, design.ID, 1))
	projectVersioned := readStoredDocumentEvent(t, database, versionEvent(t, database, design.ID, 2))
	t.Run("W9 the uploads of a document of a project", func(t *testing.T) {
		requireNoAncestors(t, "the project's design.md's first upload", projectCreated, design.ID)
		requireNoAncestors(t, "the project's design.md's second upload", projectVersioned, design.ID)
	})

	picture := uploadImage(t, handler, grandchild.Key, "diagram.png", "first pixels").Artifact
	uploadImage(t, handler, grandchild.Key, "diagram.png", "second pixels")
	pictureCreated := readStoredDocumentEvent(t, database, versionEvent(t, database, picture.ID, 1))
	pictureVersioned := readStoredDocumentEvent(t, database, versionEvent(t, database, picture.ID, 2))
	t.Run("W10 the uploads of a file", func(t *testing.T) {
		requireIssueAncestors(t, "diagram.png's first upload", pictureCreated, grandchild.Key, chain)
		requireIssueAncestors(t, "diagram.png's second upload", pictureVersioned, grandchild.Key, chain)
	})

	// A second grandchild, created while a live process lists no issue room, is converted by the
	// conversion (docs.ConvertIssue), which writes a version of its spec in the issue's room.
	converting := createDigestChild(t, handler, database, "TEST", "A leaf the conversion converts", "The converted leaf's words.\n", child.Key, true)
	convertingChain := newestEventPayload[issueEventFields](t, database, "issue.created", converting.Key).Ancestors
	if want := []string{child.Key, root.Key}; !slices.Equal(convertingChain, want) {
		t.Fatalf("the converted grandchild's issue.created ancestors = %#v, want %#v", convertingChain, want)
	}
	// The conversion defers an issue one of whose documents owes its block index, so a tick runs
	// through whatever the writes before it left owed first.
	if _, err := documents.ReconcileBlockIndex(ctx, docs.BlockIndexCursors{}, nil); err != nil {
		t.Fatalf("run the block-index tick over what the writes of %s left owed: %v", converting.Key, err)
	}
	convertingSpec := converting.PrimaryArtifactID
	unconvertedSpec := newestQueryVersion(t, database, convertingSpec)
	if converted, err := documents.ConvertIssue(ctx, converting.Key, docs.IssueRoomGate, false); err != nil || converted.Outcome != docs.IssueConverted {
		t.Fatalf("the conversion of %s answered %+v, %v; want it converted", converting.Key, converted, err)
	}
	convertedSpec := newestQueryVersion(t, database, convertingSpec)
	if convertedSpec.number != unconvertedSpec.number+1 {
		t.Fatalf("the conversion of %s leaves its spec at version %d, want the conversion's version after %d",
			converting.Key, convertedSpec.number, unconvertedSpec.number)
	}
	conversionVersioned := readStoredDocumentEvent(t, database, versionEvent(t, database, convertingSpec, convertedSpec.number))
	t.Run("W11 the conversion of an issue that versions its document", func(t *testing.T) {
		requireIssueAncestors(t, "the spec's version the conversion wrote", conversionVersioned, converting.Key, convertingChain)
	})

	// A result recorded on the check of the converted grandchild's Verification document moves that
	// query's record, and the drain the route runs once the result has committed writes a version
	// of the document holding the bytes of the one before it (DrainOwedQueries).
	verification := uploadDocument(t, handler, "/api/v1/issues/"+converting.Key, "verification.md",
		":::check{#c1 surface=\"cli\"}\nThe converted leaf reads its tree.\n:::\n").Artifact
	checked := newestQueryVersion(t, database, verification.ID)
	if recorded := recordResult(t, handler, converting.Key, resultBody("c1", "fail", headCommit, map[string]any{"failed": 1})); recorded.Code != http.StatusCreated {
		t.Fatalf("record a result of c1 on %s: status=%d body=%s", converting.Key, recorded.Code, recorded.Body.String())
	}
	revised := newestQueryVersion(t, database, verification.ID)
	if revised.number != checked.number+1 || revised.markdown != checked.markdown {
		t.Fatalf("a result recorded on %s leaves its Verification document at version %d holding %q, want the drain's version after %d holding its bytes, %q",
			converting.Key, revised.number, revised.markdown, checked.number, checked.markdown)
	}
	drained := readStoredDocumentEvent(t, database, versionEvent(t, database, verification.ID, revised.number))
	t.Run("W12 the drain's revision of a query's document", func(t *testing.T) {
		requireIssueAncestors(t, "the Verification document's version the drain wrote", drained, converting.Key, convertingChain)
	})

	// The fixture holds one stored payload for each event type and kind of owner, its uuids and
	// times made placeholders as the ask-reply fixture's are (normalizeAskReplyFixture), and the
	// version events of W11 and W12. The issues' keys are TEST-1 to TEST-4 in every run, since each
	// test's database is new.
	fixture := documentEventAncestors{
		Issues: map[string]string{"root": root.Key, "child": child.Key, "grandchild": grandchild.Key, "converted grandchild": converting.Key},
		Events: []documentEventAncestorsEvent{
			created.fixture("grandchild"), versioned.fixture("grandchild"), removed.fixture("grandchild"),
			rootCreated.fixture("root"), rootVersioned.fixture("root"),
			projectCreated.fixture("project"), projectVersioned.fixture("project"),
			conversionVersioned.fixture("converted grandchild"), drained.fixture("converted grandchild"),
		},
	}
	encoded, err := json.MarshalIndent(fixture, "", "  ")
	if err != nil {
		t.Fatalf("encode the fixture: %v", err)
	}
	got := normalizeAskReplyFixture(append(encoded, '\n'))
	if *updateFixtures {
		// A row that failed stored a payload the server should not store, which the contracts'
		// tests would then hold the schemas to.
		if t.Failed() {
			t.Fatalf("%s is left as it is: a row above failed, so this run's payloads are not the ones to commit", documentEventAncestorsFixture)
		}
		if err := os.WriteFile(documentEventAncestorsFixture, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", documentEventAncestorsFixture)
		return
	}
	const rewrite = "go test ./internal/dispatch/api -run '^TestDocumentEventsNameTheirIssueAncestors$' -count=1 -update"
	want, err := os.ReadFile(documentEventAncestorsFixture)
	if err != nil {
		t.Fatalf("read %s (%s writes it): %v", documentEventAncestorsFixture, rewrite, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s differs from the payloads this run stored; when the change is meant, rewrite it with %s:\n%s",
			documentEventAncestorsFixture, rewrite, got)
	}
}
