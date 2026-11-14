package docs

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A version move keeps an approval request with the person when the document waits on the person
// and the new version stores byte for byte the text of the version the request names: the request
// names the new version at requested_version too, and its ask.edited is quiet, since the text the
// person was asked about did not change. A version of another text then moves the request onto its
// agent as every move does, and wakes its asker, being the first move since the request waited on
// the person. A version of that same text again, while the request already waits on its agent,
// keeps it there and is quiet: only a document that waits on the person keeps a turn so.
func TestAnApprovalRequestKeepsItsTurnOverAVersionOfTheTextThePersonWasAskedAbout(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	artifactID := createDocument(t, database, "# Spec\n\nShip it.\n")
	var asked string
	if err := database.Pool.QueryRow(ctx, `
		select markdown from artifact_versions where artifact_id = $1 and number = 1
	`, artifactID).Scan(&asked); err != nil {
		t.Fatalf("read version 1: %v", err)
	}
	if asked == "" {
		t.Fatal("version 1 stores no text, so no version can repeat it")
	}
	askID := openStoredApprovalAsk(t, database, artifactID)

	for _, step := range []struct {
		name                      string
		number                    int
		markdown                  string
		version, requestedVersion int
		quiet                     bool
	}{
		{"the text the person was asked about, byte for byte", 2, asked, 2, 2, true},
		{"another text", 3, "# Spec\n\nShip it on Monday.\n", 3, 2, false},
		{"the text the request names, while it waits on its agent", 4, "# Spec\n\nShip it on Monday.\n", 4, 2, true},
	} {
		moved := moveStoredApprovalAsk(t, database, artifactID, step.number, step.markdown)
		if len(moved) != 1 {
			t.Fatalf("%s: the move appended %d events, want one ask.edited", step.name, len(moved))
		}
		edit, ok := moved[0].Payload.(model.AskEditEventPayload)
		if moved[0].Type != "ask.edited" || !ok {
			t.Fatalf("%s: the move appended %s with payload %T, want an ask.edited", step.name, moved[0].Type, moved[0].Payload)
		}
		if edit.Quiet != step.quiet || moved[0].Notify == step.quiet {
			t.Fatalf("%s: ask.edited quiet=%t notify=%t, want quiet=%t", step.name, edit.Quiet, moved[0].Notify, step.quiet)
		}
		var version, requestedVersion int
		if err := database.Pool.QueryRow(ctx, `
			select (approval -> 'documents' -> 0 ->> 'version')::int, (approval -> 'documents' -> 0 ->> 'requested_version')::int
			from asks where id = $1 and state = 'open'
		`, askID).Scan(&version, &requestedVersion); err != nil {
			t.Fatalf("%s: read the request: %v", step.name, err)
		}
		if version != step.version || requestedVersion != step.requestedVersion {
			t.Fatalf("%s: the request names version %d, requested at %d; want version %d, requested at %d",
				step.name, version, requestedVersion, step.version, step.requestedVersion)
		}
	}
}

// openStoredApprovalAsk stores an open approval request over version 1 of artifactID, waiting on
// the person, as the approval route writes one, and returns its id.
func openStoredApprovalAsk(t *testing.T, database *store.Store, artifactID string) string {
	t.Helper()
	documents := []model.AskApprovalDocument{{ArtifactID: artifactID, Name: "document.md", Version: 1, RequestedVersion: 1}}
	approval, err := EncodeApproval(&model.AskApproval{Documents: documents})
	if err != nil {
		t.Fatal(err)
	}
	var id string
	if err := database.Pool.QueryRow(context.Background(), `
		insert into asks (issue_key, author, question, options, urgency, kind, approval)
		values ('DOC-1', '{"kind":"session","id":"asker"}', $1, '[{"label":"Approve"},{"label":"Request changes"}]', 'high', 'approval', $2)
		returning id::text
	`, ApprovalQuestion(documents, "Ships it."), approval).Scan(&id); err != nil {
		t.Fatalf("store the approval request: %v", err)
	}
	return id
}

// moveStoredApprovalAsk writes version number of artifactID with markdown and moves the open
// approval request to it in the same transaction, as a person's edit does, and returns the events
// the move appended.
func moveStoredApprovalAsk(t *testing.T, database *store.Store, artifactID string, number int, markdown string) []model.Event {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1, $2, $3, '[{"kind":"user","id":"alice"}]')
	`, artifactID, number, markdown); err != nil {
		t.Fatalf("write version %d: %v", number, err)
	}
	moved, err := MoveApprovalAsk(ctx, tx, events.NewBroker(), artifactID, model.Version{Number: number},
		model.Actor{Kind: "user", ID: "alice"}, "")
	if err != nil {
		t.Fatalf("move the request to version %d: %v", number, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit version %d: %v", number, err)
	}
	return moved
}
