package outbox

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// withIssueMessages is deps posting through a Dispatch API server of its own on the rig's database
// and broker, as cmd/dispatch passes it (api.Background.PostSystemMessage): the message about the
// events a role did not take is written by the issue message route's own writer.
func (r *roleRig) withIssueMessages(t *testing.T, deps Deps) Deps {
	t.Helper()
	documents := docs.New(docs.Deps{Store: r.database, Events: r.broker, ServerURL: "https://dispatch.example", Settle: 20 * time.Millisecond})
	t.Cleanup(func() {
		if err := documents.Shutdown(context.Background()); err != nil {
			t.Errorf("shut down the document service: %v", err)
		}
	})
	tokens, err := auth.ParseSharedAgentTokens("agent-token")
	if err != nil {
		t.Fatalf("parse agent token: %v", err)
	}
	people := store.NewPgPeopleStore(r.database.Pool, auth.SingleSigningKey("signing-key"), nil)
	apiDeps, err := api.NewDeps(api.DepsInput{
		Store: r.database, Identity: identity.HeaderIdentity{Header: "X-Dispatch-User", People: people},
		AgentTokens: tokens, ServerURL: "https://dispatch.example", Docs: documents, Events: r.broker,
	})
	if err != nil {
		t.Fatalf("API dependencies: %v", err)
	}
	background := api.Register(http.NewServeMux(), t.Context(), apiDeps)
	t.Cleanup(background.Wait)
	deps.PostIssueMessage = background.PostSystemMessage
	return deps
}

// systemMessage is one message an outbox's system actor posted on an issue.
type systemMessage struct {
	id   string
	body string
}

// systemMessages is every message on issue key written by a system actor, oldest first.
func systemMessages(t *testing.T, database *store.Store, key string) []systemMessage {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select id::text, body from messages where issue_key = $1 and author ->> 'kind' = 'system' order by created_at, id
	`, key)
	if err != nil {
		t.Fatalf("read the system messages of %s: %v", key, err)
	}
	defer rows.Close()
	var found []systemMessage
	for rows.Next() {
		var message systemMessage
		if err := rows.Scan(&message.id, &message.body); err != nil {
			t.Fatalf("scan a system message of %s: %v", key, err)
		}
		found = append(found, message)
	}
	return found
}

// expireToTheBound ends eventID's copies for role undelivered: it runs the event's copies through
// every keep of the bound (expireUpTo), so the outbox keeps the event again until its bound and
// then records it undelivered.
func expireToTheBound(t *testing.T, rig *roleRig, outbox *Outbox, role string, eventID int64) {
	t.Helper()
	expireUpTo(t, rig, outbox, unfenced{}, role, eventID, maxRoleKeeps)
	var records int
	if err := rig.database.Pool.QueryRow(context.Background(), `
		select count(*) from events where type = $1 and (payload ->> 'event_id')::bigint = $2
	`, model.RoleUndeliveredEventType, eventID).Scan(&records); err != nil || records != 1 {
		t.Fatalf("event %d has %d role.undelivered records after its %d keeps (%v), want one", eventID, records, maxRoleKeeps, err)
	}
}

// endWindows moves every unposted window of role back past its hour, as the hour passing would.
func endWindows(t *testing.T, database *store.Store, role string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		update outbox_role_undelivered_notes set window_opened_at = window_opened_at - $2::interval
		where role = $1 and posted_at is null
	`, role, undeliveredNoteWindow+time.Minute); err != nil {
		t.Fatalf("end the windows of role %s: %v", role, err)
	}
}

// restarted is a second outbox on deps, as a Dispatch process started after the first one stopped
// has: an instance of its own, holding the publisher hold, and nothing in memory of the first.
func restarted(t *testing.T, deps Deps) *Outbox {
	t.Helper()
	return holding(t, deps)
}

// An issue event a role never took is told on the issue: once its window's hour has ended, one
// message written by the outbox's system actor names the event, its type, the post it carries, the
// role and its keeps. Inside the hour nothing is posted. The window is opened by one outbox process
// and posted by another, started afresh with nothing in memory of it, so its state is in Postgres.
// At the head before this message (role.undelivered alone) nothing is posted on the issue.
func TestAnIssueEventARoleNeverTookIsToldOnTheIssueWhenItsWindowEnds(t *testing.T) {
	ctx := context.Background()
	rig := newRoleRig(t)
	const role = "undelivered-on-the-issue"
	rig.silent(t, contracts.RoleTopicPrefix+role)
	event := rig.routedEvent(t, "T-1", role, model.Actor{Kind: "user", ID: "alice"})
	deps := rig.withIssueMessages(t, rig.deps())
	first := holding(t, deps)
	first.scan(ctx, unfenced{})
	expireToTheBound(t, rig, first, role, event.ID)

	first.passKeptExpiries(ctx, unfenced{})
	if posted := systemMessages(t, rig.database, "T-1"); len(posted) != 0 {
		t.Fatalf("T-1 holds system messages %+v inside the window's hour, want none yet", posted)
	}

	endWindows(t, rig.database, role)
	second := restarted(t, deps)
	second.passKeptExpiries(ctx, unfenced{})

	posted := systemMessages(t, rig.database, "T-1")
	if len(posted) != 1 {
		t.Fatalf("T-1 holds %d system messages once the window ended, want one", len(posted))
	}
	t.Logf("the message on T-1: %s", posted[0].body)
	var messageID string
	if err := rig.database.Pool.QueryRow(ctx, `select payload ->> 'id' from events where id = $1`, event.ID).Scan(&messageID); err != nil {
		t.Fatalf("read event %d's message id: %v", event.ID, err)
	}
	for _, want := range []string{
		"role `" + role + "`", fmt.Sprintf("event %d (message.created)", event.ID),
		"dispatch://T-1/message/" + messageID, fmt.Sprintf("kept %d times", maxRoleKeeps), "role.undelivered",
	} {
		if !strings.Contains(posted[0].body, want) {
			t.Errorf("the message on T-1 does not say %q:\n%s", want, posted[0].body)
		}
	}
	var notify bool
	if err := rig.database.Pool.QueryRow(ctx, `
		select notify from events where type = 'message.created' and payload ->> 'id' = $1
	`, posted[0].id).Scan(&notify); err != nil || notify {
		t.Fatalf("the message's message.created notifies %v (%v), want an event that wakes no route", notify, err)
	}
	second.passKeptExpiries(ctx, unfenced{})
	if posted := systemMessages(t, rig.database, "T-1"); len(posted) != 1 {
		t.Fatalf("T-1 holds %d system messages after a further pass, want the one", len(posted))
	}
}

// Two events one role did not take on one issue within the window's hour are named in one message.
// At the head before this message nothing is posted.
func TestTwoEventsOneRoleDidNotTakeWithinTheHourAreNamedInOneMessage(t *testing.T) {
	ctx := context.Background()
	rig := newRoleRig(t)
	const role = "undelivered-twice-in-the-hour"
	rig.silent(t, contracts.RoleTopicPrefix+role)
	alice := model.Actor{Kind: "user", ID: "alice"}
	first := rig.routedEvent(t, "T-1", role, alice)
	second := rig.laterEvent(t, "T-1", alice)
	outbox := holding(t, rig.withIssueMessages(t, rig.deps()))
	outbox.scan(ctx, unfenced{})
	expireToTheBound(t, rig, outbox, role, first.ID)
	expireToTheBound(t, rig, outbox, role, second.ID)

	endWindows(t, rig.database, role)
	outbox.passKeptExpiries(ctx, unfenced{})

	posted := systemMessages(t, rig.database, "T-1")
	if len(posted) != 1 {
		t.Fatalf("T-1 holds %d system messages, want one naming both events", len(posted))
	}
	for _, want := range []string{"2 events", fmt.Sprintf("event %d (", first.ID), fmt.Sprintf("event %d (", second.ID)} {
		if !strings.Contains(posted[0].body, want) {
			t.Errorf("the message on T-1 does not say %q:\n%s", want, posted[0].body)
		}
	}
}

// An event a role did not take after its window's hour has ended opens a window of its own, posted
// in a message of its own once that hour ends; the first window's message names the first event
// alone. At the head before this message nothing is posted.
func TestAnEventARoleDidNotTakeAfterItsWindowEndedOpensANewWindow(t *testing.T) {
	ctx := context.Background()
	rig := newRoleRig(t)
	const role = "undelivered-after-the-window"
	rig.silent(t, contracts.RoleTopicPrefix+role)
	alice := model.Actor{Kind: "user", ID: "alice"}
	first := rig.routedEvent(t, "T-1", role, alice)
	second := rig.laterEvent(t, "T-1", alice)
	outbox := holding(t, rig.withIssueMessages(t, rig.deps()))
	outbox.scan(ctx, unfenced{})
	expireToTheBound(t, rig, outbox, role, first.ID)
	endWindows(t, rig.database, role)
	expireToTheBound(t, rig, outbox, role, second.ID)

	posted := systemMessages(t, rig.database, "T-1")
	if len(posted) != 1 {
		t.Fatalf("T-1 holds %d system messages once the first window ended, want its one", len(posted))
	}
	if !strings.Contains(posted[0].body, fmt.Sprintf("event %d (", first.ID)) || strings.Contains(posted[0].body, fmt.Sprintf("event %d (", second.ID)) {
		t.Fatalf("the first window's message names the wrong events:\n%s", posted[0].body)
	}
	endWindows(t, rig.database, role)
	outbox.passKeptExpiries(ctx, unfenced{})
	posted = systemMessages(t, rig.database, "T-1")
	if len(posted) != 2 || !strings.Contains(posted[1].body, fmt.Sprintf("event %d (", second.ID)) ||
		strings.Contains(posted[1].body, fmt.Sprintf("event %d (", first.ID)) {
		t.Fatalf("T-1 holds %d system messages, want a second naming event %d alone: %+v", len(posted), second.ID, posted)
	}
}

// A document event a role did not take is told on the document's issue, whose route the role is,
// as an issue message that names the document and links it. A document of an issue writes its
// events on that issue (store.OwnerOf), naming the document in their payload, as a person's settled
// edit does here. At the head before this message nothing is posted.
func TestADocumentEventARoleNeverTookIsToldOnTheDocumentsIssue(t *testing.T) {
	ctx := context.Background()
	rig := newRoleRig(t)
	const role = "undelivered-on-the-document"
	rig.silent(t, contracts.RoleTopicPrefix+role)
	rig.routedEvent(t, "T-1", role, model.Actor{Kind: "user", ID: "alice"})
	if _, err := rig.database.Pool.Exec(ctx, `update events set published_at = now() where issue_key = 'T-1'`); err != nil {
		t.Fatalf("publish T-1's own event: %v", err)
	}
	var artifactID string
	if err := rig.database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
		values ('T-1', 'TT', 'spec', 'spec.md', 'doc', '{"kind":"user","id":"alice"}')
		returning id::text
	`).Scan(&artifactID); err != nil {
		t.Fatalf("create T-1's document: %v", err)
	}
	event := appendEvent(t, rig.database, rig.broker, model.Event{
		IssueKey: new("T-1"), Type: "artifact.human_edited", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.ArtifactHumanEditedEventPayload{ArtifactID: artifactID, Name: "spec.md", FromVersion: 1, ToVersion: 2, Editors: []string{"alice"}},
	})
	outbox := holding(t, rig.withIssueMessages(t, rig.deps()))
	outbox.scan(ctx, unfenced{})
	if lane := keptLane(t, rig.client, role); len(lane) != 1 || envelopeOf(t, lane[0]).SourceEventID != fmt.Sprint(event.ID) {
		t.Fatalf("role %s's kept lane holds %d copies, want the document event's one", role, len(lane))
	}
	expireToTheBound(t, rig, outbox, role, event.ID)
	endWindows(t, rig.database, role)
	outbox.passKeptExpiries(ctx, unfenced{})

	posted := systemMessages(t, rig.database, "T-1")
	if len(posted) != 1 {
		t.Fatalf("T-1 holds %d system messages, want one about its document's event", len(posted))
	}
	for _, want := range []string{fmt.Sprintf("event %d (artifact.human_edited)", event.ID), "on the document dispatch://T-1/artifact/spec"} {
		if !strings.Contains(posted[0].body, want) {
			t.Errorf("the message on T-1 does not say %q:\n%s", want, posted[0].body)
		}
	}
}
