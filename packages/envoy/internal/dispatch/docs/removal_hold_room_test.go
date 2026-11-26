package docs

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// restoreIssue is issue DOC-1 of a test database and its spec, written through the document
// service and settled. The decisions step's tests declared it; the removal holds below still read
// it, so it stands here.
type restoreIssue struct {
	t        *testing.T
	database *store.Store
	service  *Service
	spec     string
}

// newRestoreIssue creates project DOC, issue DOC-1 and its spec holding markdown, settled, so each
// ask block of it holds its ask.
func newRestoreIssue(t *testing.T, markdown string) *restoreIssue {
	t.Helper()
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(context.Background(), `
		insert into projects (key, name, default_assignee) values ('DOC', 'Documents', 'alice');
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('DOC-1', 'DOC', 1, 'Document', '{"kind":"user","id":"alice"}', 'U');
	`); err != nil {
		t.Fatalf("create the issue: %v", err)
	}
	service := newTestServiceOn(t, database)
	service.settle = time.Hour
	issue := &restoreIssue{t: t, database: database, service: service}
	issue.spec = issue.document("spec", "spec.md", true, "", markdown)
	return issue
}

// document creates a document of DOC-1 holding markdown as its version 1, written by alice and
// indexed as an upload of a new document is, and settles it, so each ask block holds its ask.
func (issue *restoreIssue) document(slug, name string, primary bool, role, markdown string) string {
	t := issue.t
	t.Helper()
	var id string
	var rolePointer *string
	if role != "" {
		rolePointer = &role
	}
	if err := issue.database.Pool.QueryRow(context.Background(), `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, role, created_by)
		values ('DOC-1', 'DOC', $1, $2, 'doc', $3, $4, '{"kind":"user","id":"alice"}')
		returning id::text
	`, slug, name, primary, rolePointer).Scan(&id); err != nil {
		t.Fatalf("create document %s: %v", slug, err)
	}
	if err := joinedWrite(issue.service, func(ctx context.Context) error {
		written, err := issue.service.SeedText(ctx, id, markdown, model.Actor{Kind: "user", ID: "alice"})
		if err != nil {
			return err
		}
		tx, _ := txFromContext(ctx)
		if _, err := tx.Exec(ctx, `
			insert into artifact_versions (artifact_id, number, markdown, authors)
			values ($1, 1, $2, '[{"kind":"user","id":"alice"}]')
		`, id, written); err != nil {
			return err
		}
		_, write, err := issue.service.IndexDocumentReferences(ctx, id, 1, written)
		if err != nil {
			return err
		}
		return causeInWritePhase(ctx, tx, write, graphstore.NoEvent)
	}); err != nil {
		t.Fatalf("write document %s: %v", slug, err)
	}
	settleCurrentGeneration(t, issue.service, id)
	return id
}

// latest is the markdown of document's newest version.
func (issue *restoreIssue) latest(document string) string {
	issue.t.Helper()
	var markdown string
	if err := issue.database.Pool.QueryRow(context.Background(), `
		select markdown from artifact_versions where artifact_id = $1 order by number desc limit 1
	`, document).Scan(&markdown); err != nil {
		issue.t.Fatalf("read the newest version of %s: %v", document, err)
	}
	return markdown
}

// removedRestoreAsk is an ask block of the spec the removal holds below write.
func removedRestoreAsk(id, question string) string {
	return ":::ask{#" + id + " urgency=\"med\" multiple=\"false\"}\n" + question + "\n:::\n\n"
}

// heldRemovalsOf is every removed_by tuple of ask's node, in the order they were appended.
func heldRemovalsOf(t *testing.T, issue *restoreIssue, ask string) []graphstore.Tuple {
	t.Helper()
	held, err := graphstore.History(context.Background(), issue.database.Pool, graphstore.KindAsk, ask, graphstore.TupleRemovedBy)
	if err != nil {
		t.Fatalf("read the removals of ask %s: %v", ask, err)
	}
	return held
}

// removalHoldFixture is issue DOC-1 of a test database and its spec, settled, so each ask block
// of the spec holds its ask: room is the artifact its live edits and its asks are keyed by, and
// fragment the fragment of that room's Yjs document the spec is - the spec itself and its one
// fragment for a document with a room of its own, the issue room's tree and the spec's own
// fragment of it for a document converted into its issue's room.
type removalHoldFixture struct {
	issue    *restoreIssue
	room     string
	fragment string
}

// newOwnRoomRemovalHold is the fixture over a spec with a room of its own, holding markdown, as
// TestAPersonsUnsettledBrowserDeletionOfAnAnsweredAskStaysThroughAnAgentsVersion writes it.
func newOwnRoomRemovalHold(t *testing.T, markdown string) removalHoldFixture {
	t.Helper()
	issue := newRestoreIssue(t, markdown)
	return removalHoldFixture{issue: issue, room: issue.spec, fragment: fragmentName}
}

// newConvertedRemovalHold is the fixture over the spec of an issue converted into its room
// (createIssueRoom), holding markdown: written into the spec's fragment of the room by alice with
// its version, and the room settled.
func newConvertedRemovalHold(t *testing.T, markdown string) removalHoldFixture {
	t.Helper()
	database := storetest.Open(t)
	service := newTestServiceOn(t, database)
	service.settle = time.Hour
	room := createIssueRoom(t, database, 1)
	alice := model.Actor{Kind: "user", ID: "alice"}
	base := liveDocumentToken(t, service, room.spec)
	if err := joinedWrite(service, func(ctx context.Context) error {
		if _, err := service.ReplaceText(ctx, room.spec, markdown, alice, base); err != nil {
			return err
		}
		written, err := service.SnapshotVersion(ctx, room.spec, alice)
		if err != nil {
			return err
		}
		tx, _ := txFromContext(ctx)
		return causeInWritePhase(ctx, tx, written.Blocks, graphstore.NoEvent)
	}); err != nil {
		t.Fatalf("write the spec of the issue room: %v", err)
	}
	settleCurrentGeneration(t, service, room.tree)
	return removalHoldFixture{
		issue: &restoreIssue{t: t, database: database, service: service, spec: room.spec},
		room:  room.tree, fragment: room.spec,
	}
}

// answer has alice answer the ask of the spec's block blockID in text, as restoreIssue.answer
// does, and answers its id: the ask is read under the artifact its row is keyed by.
func (f removalHoldFixture) answer(t *testing.T, blockID, text string) string {
	t.Helper()
	ask := blockAskOn(t, f.issue.service, f.room, blockID)
	alice := model.Actor{Kind: "user", ID: "alice"}
	at := time.Now().UTC()
	change := asks.Answered(model.AskAnswer{User: "alice", Selected: []string{}, Text: &text, At: at})
	change.Who, change.At = alice, at
	settleAskAs(t, f.issue.service, f.issue.spec, blockID, ask.ID, alice, change)
	return ask.ID
}

// browserReplace writes markdown as the spec's whole live text the way a browser peer's edit
// reaches its room, as browserReplaceText writes it, into the spec's fragment of that room, from
// a peer connected to the room as actor, whom the room credits with it.
func (f removalHoldFixture) browserReplace(t *testing.T, markdown string, actor model.Actor) {
	t.Helper()
	written, err := pmdoc.Parse(markdown)
	if err != nil {
		t.Fatalf("parse the browser's document: %v", err)
	}
	service := f.issue.service
	connection := service.nextConnection.Add(1)
	service.addConnection(f.room, connection, actor)
	defer service.removeConnection(f.room, connection)
	if err := service.srv.Apply(context.Background(), f.room, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		fragment := doc.GetXmlFragment(f.fragment)
		transact(func(txn *crdt.Transaction) {
			if err := pmdoc.Update(txn, fragment, written); err != nil {
				t.Errorf("update the spec's live tree: %v", err)
			}
		})
	}); err != nil {
		t.Fatalf("apply the browser's edit: %v", err)
	}
}

// A person's deletion of an answered ask's block in the browser is held as that person's removal
// by each write that versions the room in its settlement's place (holdRoomRemovals) - an agent's
// upload (ReplaceText, with its version), a snapshot of the room and a named version, each in an
// agent's name - on a document converted into its issue's room as on one with a room of its own.
// The converted spec is a fragment of the room: its asks and its live edits are keyed by the
// room's tree, and its own id names the fragment. Alice answers q-own and her browser deletes its
// block; before any settlement runs, the agent's write versions the spec, once, without the
// block, and the ask's node holds one asserted removed_by to alice, which the same write states
// on the spec with a room of its own: no removal a person made is lost because the document
// moved into its issue's room.
func TestARemovalHoldStandsOnADocumentConvertedIntoAnIssueRoom(t *testing.T) {
	const head = "# Spec\n\n:::requirement{#r1}\nThe store holds the corpus.\n:::\n\n"
	agent := model.Actor{Kind: "session", ID: "session-unsettled-removal"}
	alice := model.Actor{Kind: "user", ID: "alice"}
	// Each write is prepared outside its transaction, as its caller prepares it - the upload names
	// the token the spec stands at, read before the write as a caller reads it - and run inside it.
	for _, write := range []struct {
		name    string
		prepare func(t *testing.T, service *Service, spec string) func(ctx context.Context) (VersionResult, error)
	}{
		{"an agent's upload", func(t *testing.T, service *Service, spec string) func(ctx context.Context) (VersionResult, error) {
			base := liveDocumentToken(t, service, spec)
			return func(ctx context.Context) (VersionResult, error) {
				if _, err := service.ReplaceText(ctx, spec, strings.Replace(head, "holds the corpus", "holds the whole corpus", 1), agent, base); err != nil {
					return VersionResult{}, err
				}
				return service.SnapshotVersion(ctx, spec, agent)
			}
		}},
		{"a snapshot of the room in an agent's name", func(_ *testing.T, service *Service, spec string) func(ctx context.Context) (VersionResult, error) {
			return func(ctx context.Context) (VersionResult, error) { return service.SnapshotVersion(ctx, spec, agent) }
		}},
		{"a named version in an agent's name", func(_ *testing.T, service *Service, spec string) func(ctx context.Context) (VersionResult, error) {
			return func(ctx context.Context) (VersionResult, error) {
				return service.NamedVersion(ctx, spec, "Named by the agent", agent)
			}
		}},
	} {
		t.Run(write.name, func(t *testing.T) {
			for _, room := range []struct {
				name string
				open func(t *testing.T, markdown string) removalHoldFixture
			}{
				{"a document with a room of its own", newOwnRoomRemovalHold},
				{"a document converted into an issue room", newConvertedRemovalHold},
			} {
				t.Run(room.name, func(t *testing.T) {
					fixture := room.open(t, head+removedRestoreAsk("q-own", "Which store holds the corpus?"))
					issue := fixture.issue
					before := latestVersionNumber(t, issue.service, issue.spec)
					ask := fixture.answer(t, "q-own", "The database we run.")
					fixture.browserReplace(t, head, alice)
					version := write.prepare(t, issue.service, issue.spec)
					if err := joinedWrite(issue.service, func(ctx context.Context) error {
						written, err := version(ctx)
						if err != nil {
							return err
						}
						tx, _ := txFromContext(ctx)
						return causeInWritePhase(ctx, tx, written.Blocks, graphstore.NoEvent)
					}); err != nil {
						t.Fatalf("%s of the spec: %v", write.name, err)
					}
					if written := latestVersionNumber(t, issue.service, issue.spec); written != before+1 {
						t.Fatalf("the spec stands at version %d after %d, want the agent's write the one version after it", written, before)
					}
					if spec := issue.latest(issue.spec); strings.Contains(spec, "#q-own ") {
						t.Fatalf("the agent's version holds q-own, which alice's browser deleted:\n%s", spec)
					}
					if held := heldRemovalsOf(t, issue, ask); len(held) != 1 || !held[0].Asserted || held[0].ToKind != graphstore.KindPerson || held[0].ToID != "alice" {
						t.Errorf("after %s, the ask %s holds the removals %+v, want one asserted to alice: her browser deleted its block", write.name, ask, held)
					}
				})
			}
		})
	}
}
