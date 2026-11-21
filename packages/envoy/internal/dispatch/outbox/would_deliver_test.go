package outbox

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// writeToolEvents is the event type each write tool of the command bar appends (api's
// goingWriteTools, which this package's tests cannot import): every one WouldDeliver is asked of.
var writeToolEvents = []string{
	"issue.created", "issue.updated", "issue.claimed", "issue.released",
	"ask.opened", "ask.edited", "ask.resolved", "ask.answered", "ask.asked_of", "ask.follower_added",
	"comment.created", "comment.resolved", "comment.reopened", "comment.edited", "comment.delivery", "comment.answered",
	"message.created", "message.delivery", "message.accepted",
	"artifact.version", "artifact.approved", "artifact.created", "artifact.renamed", "artifact.removed",
	"suggestion.accepted", "suggestion.rejected", "decision.kept",
	"result.recorded", "handoff.recorded", "project.created", "architecture.synced",
	"settings.repo_project.updated", "settings.architecture_source.updated", "settings.project_default_assignee.updated",
	"priority.updated", "subscription.remove_requested",
	"user_thread_state.updated", "user_state.updated", "user_agent_state.updated",
}

// seedRoutedIssue inserts the issue key, number number of the project TT, routed to route, nil
// for none.
func seedRoutedIssue(t *testing.T, database *store.Store, key string, number int, route *string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into issues (key, project_key, number, title, route, created_by, rank)
		values ($1, 'TT', $2::int, 'Test issue', $3, '{"kind":"user","id":"alice"}', 'U' || $2::int::text)
	`, key, number, route); err != nil {
		t.Fatalf("create issue %s: %v", key, err)
	}
}

// TestWouldDeliverAgreesWithEventDestinations: the event of every write tool of the command bar,
// as a person's write appends it, on an issue routed to a session, on one routed to a role, on
// one a session claims and on one routed to nobody, and on the issues' documents and the
// project: WouldDeliver, asked before the event is appended, answers whether the outbox, once it
// published the event, sent it to a session's own subject or a role's topic. Among the events are
// those whose destination their payload decides: a comment's resolution in the thread of an ask
// a session follows and one outside any ask, an ask's answer a session follows, a message to the
// session it targets and one to the issue's route, and a document's named and unnamed version.
func TestWouldDeliverAgreesWithEventDestinations(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	broker := events.NewBroker()
	alice := model.Actor{Kind: "user", ID: "alice"}
	writer := model.Actor{Kind: "session", ID: "session-writer"}
	if _, err := database.Pool.Exec(ctx, `insert into projects (key, name, default_assignee) values ('TT', 'Test', 'alice')`); err != nil {
		t.Fatalf("create project: %v", err)
	}
	sessionRoute, roleRoute := "session:session-route", "role:reviewer"
	seedRoutedIssue(t, database, "T-1", 1, &sessionRoute)
	seedRoutedIssue(t, database, "T-2", 2, &roleRoute)
	seedRoutedIssue(t, database, "T-3", 3, nil)
	seedRoutedIssue(t, database, "T-4", 4, nil)
	claimIssue(t, database, "T-3", model.Actor{Kind: "session", ID: "session-claim"})

	// The items the events are about: on each issue an ask a session follows, a comment in its
	// thread, a comment outside any ask and a document; and a document of the project.
	type items struct{ ask, askComment, comment, document string }
	of := map[string]items{}
	for _, key := range []string{"T-1", "T-2", "T-3", "T-4"} {
		issue := key
		var made items
		made.ask = seedAsk(t, database, issue, alice, "Ship it?")
		seedFollower(t, database, made.ask, "session-follower")
		made.askComment = seedComment(t, database, issue, alice, "In the ask's thread.", nil)
		if _, err := database.Pool.Exec(ctx, `update comments set ask_id = $2 where id = $1::uuid`, made.askComment, made.ask); err != nil {
			t.Fatalf("put a comment in an ask's thread: %v", err)
		}
		made.comment = seedComment(t, database, issue, alice, "Outside any ask.", nil)
		made.document = seedDocument(t, database, &issue, "spec", writer, []model.Actor{writer})
		of[key] = made
	}
	projectDocument := seedDocument(t, database, nil, "guide", writer, []model.Actor{writer})

	// documentEvents are the events of writeToolEvents about a document.
	documentEvents := []string{"artifact.version", "artifact.approved", "artifact.created", "artifact.renamed", "artifact.removed",
		"suggestion.accepted", "suggestion.rejected", "decision.kept"}
	// write is the event of eventType a person's write appends on issue, its document or its
	// project, with the one owner the broker appends it under - an issue's document's event is
	// its issue's - and the members of its payload the outbox reads that its item gives: an ask's
	// or a comment's id and a comment's ask, the document, and a version's name.
	write := func(issue, eventType string) model.Event {
		key, made := issue, of[issue]
		event := model.Event{IssueKey: &key, Type: eventType, Actor: alice, Payload: map[string]any{}}
		switch {
		case eventType == "ask.opened":
			event.Payload = model.AskEventPayload{Ask: model.Ask{ID: made.ask, IssueKey: &key, Question: "Ship it?", State: "open"}}
		case slices.Contains([]string{"ask.edited", "ask.resolved", "ask.answered", "ask.asked_of", "ask.follower_added"}, eventType):
			event.Payload = map[string]any{"id": made.ask}
		case slices.Contains([]string{"comment.resolved", "comment.reopened", "comment.edited", "comment.answered", "comment.delivery"}, eventType):
			event.Payload = map[string]any{"id": made.askComment, "ask_id": made.ask}
		case eventType == "artifact.version":
			event.Payload = map[string]any{"artifact_id": made.document, "version": map[string]any{"named": true}}
		case slices.Contains(documentEvents, eventType):
			event.Payload = map[string]any{"artifact_id": made.document}
		case slices.Contains([]string{"project.created", "settings.repo_project.updated", "settings.architecture_source.updated",
			"settings.project_default_assignee.updated", "architecture.synced"}, eventType):
			event.IssueKey, event.ProjectKey, event.Project = nil, new("TT"), "TT"
		case eventType == "priority.updated":
			event.IssueKey, event.Payload = nil, model.PriorityEventPayload{ID: "priority-" + issue, Change: model.PriorityChanged}
		}
		return event
	}
	type row struct {
		name  string
		event model.Event
		// want is what both answer, for the rows whose answer the table states.
		want *bool
	}
	yes, no := true, false
	var rows []row
	for _, issue := range []string{"T-1", "T-2", "T-3", "T-4"} {
		for _, eventType := range writeToolEvents {
			rows = append(rows, row{name: eventType + " on " + issue, event: write(issue, eventType)})
		}
	}
	for _, eventType := range documentEvents {
		rows = append(rows, row{name: eventType + " on the project's document", event: model.Event{
			ArtifactID: &projectDocument, Type: eventType, Actor: alice,
			Payload: map[string]any{"artifact_id": projectDocument, "version": map[string]any{"named": true}},
		}})
	}
	t4 := of["T-4"]
	key4, key1 := "T-4", "T-1"
	rows = append(rows,
		row{name: "a comment's resolution in the thread of an ask a session follows", want: &yes, event: model.Event{
			IssueKey: &key4, Type: "comment.resolved", Actor: alice, Payload: map[string]any{"id": t4.askComment, "ask_id": t4.ask},
		}},
		row{name: "a comment's resolution outside any ask", want: &no, event: model.Event{
			IssueKey: &key4, Type: "comment.resolved", Actor: alice, Payload: map[string]any{"id": t4.comment},
		}},
		row{name: "an answer to an ask a session follows", want: &yes, event: model.Event{
			IssueKey: &key4, Type: "ask.answered", Actor: alice, Payload: map[string]any{"id": t4.ask},
		}},
		row{name: "a message to the session it targets", want: &no, event: model.Event{
			IssueKey: &key1, Type: "message.created", Actor: alice, Payload: map[string]any{"target": "session-target"},
		}},
		row{name: "a message to the issue's route", want: &yes, event: model.Event{
			IssueKey: &key1, Type: "message.created", Actor: alice, Payload: map[string]any{},
		}},
		row{name: "a named version of a document of an issue routed to a session", want: &yes, event: model.Event{
			IssueKey: &key1, Type: "artifact.version", Actor: alice,
			Payload: map[string]any{"artifact_id": of["T-1"].document, "version": map[string]any{"named": true}},
		}},
		row{name: "an unnamed version of that document", want: &no, event: model.Event{
			IssueKey: &key1, Type: "artifact.version", Actor: alice,
			Payload: map[string]any{"artifact_id": of["T-1"].document, "version": map[string]any{"named": false}},
		}},
	)

	answers := make([]bool, len(rows))
	ids := make([]int64, len(rows))
	for index, row := range rows {
		delivers, err := WouldDeliver(ctx, database, broker, row.event)
		if err != nil {
			t.Fatalf("%s: WouldDeliver: %v", row.name, err)
		}
		answers[index] = delivers
		ids[index] = appendEvent(t, database, broker, row.event).ID
	}
	stop := run(t, database, &recordingPublisher{}, broker)
	waitFor(t, 30*time.Second, "the outbox to publish every event", func() bool {
		for _, id := range ids {
			if publishedAt(t, database, id) == nil {
				return false
			}
		}
		return true
	})
	stop()

	var delivered, kept int
	for index, row := range rows {
		destinations, _ := publishedDestinations(t, database, ids[index])
		sent := slices.ContainsFunc(destinations, reachesSession)
		if answers[index] != sent {
			t.Errorf("%s: WouldDeliver answers %v, and the outbox sent it to %v", row.name, answers[index], destinations)
		}
		if row.want != nil && sent != *row.want {
			t.Errorf("%s: the outbox sent it to %v, want a session reached: %v", row.name, destinations, *row.want)
		}
		if sent {
			delivered++
		} else {
			kept++
		}
	}
	if delivered == 0 || kept == 0 {
		t.Errorf("of %d events %d reached a session, want the table to hold both answers", len(rows), delivered)
	}
}
