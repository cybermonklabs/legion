package outbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// answerRoute is a Dispatch API server of its own on database and broker, as cmd/dispatch runs one
// beside the outbox, whose answer route a test posts a person's answer to: the event it appends is
// the one the outbox then publishes.
func answerRoute(t *testing.T, database *store.Store, broker *events.Broker) http.Handler {
	t.Helper()
	documents := docs.New(docs.Deps{Store: database, Events: broker, ServerURL: "https://dispatch.example", Settle: 20 * time.Millisecond})
	t.Cleanup(func() {
		if err := documents.Shutdown(context.Background()); err != nil {
			t.Errorf("shut down the document service: %v", err)
		}
	})
	tokens, err := auth.ParseSharedAgentTokens("agent-token")
	if err != nil {
		t.Fatalf("parse agent token: %v", err)
	}
	people := store.NewPgPeopleStore(database.Pool, auth.SingleSigningKey("signing-key"), nil)
	apiDeps, err := api.NewDeps(api.DepsInput{
		Store: database, Identity: identity.HeaderIdentity{Header: "X-Dispatch-User", People: people},
		AgentTokens: tokens, ServerURL: "https://dispatch.example", Docs: documents, Events: broker,
	})
	if err != nil {
		t.Fatalf("API dependencies: %v", err)
	}
	mux := http.NewServeMux()
	background := api.Register(mux, t.Context(), apiDeps)
	t.Cleanup(background.Wait)
	return mux
}

// answerAs posts login's answer text to askID through the answer route, as a person's browser
// sends it, and fails unless the route takes it.
func answerAs(t *testing.T, handler http.Handler, askID, login, text string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"text": text, "expected_edited_at": nil})
	if err != nil {
		t.Fatalf("encode the answer: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/asks/"+askID+"/answer", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", login)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("answer ask %s: status=%d body=%s", askID, response.Code, response.Body.String())
	}
}

// answeredEvent is the ask.answered event the answer to askID appended.
func answeredEvent(t *testing.T, database *store.Store, askID string) int64 {
	t.Helper()
	var id int64
	if err := database.Pool.QueryRow(context.Background(), `
		select id from events where type = 'ask.answered' and payload->>'id' = $1 order by id desc limit 1
	`, askID).Scan(&id); err != nil {
		t.Fatalf("read the ask.answered event of ask %s: %v", askID, err)
	}
	return id
}

// R14: a person's answer goes to each agent and each role its text names, beside whoever asked,
// on the road that takes it to the followers. The answer is written by the answer route itself,
// which reads the parties out of the text and records them on its event, and the outbox sends the
// event to them: the agent on its own topic, the role on its role topic, forwarded to the role's
// holder. A named person and an agent written inside code get nothing, and the asker, whom the
// text names as well, gets one copy. With the party read taken out of the answer route, the named
// agent's topic gets no answer and neither does the role's.
func TestAnAnswerGoesToEachAgentAndRoleItsTextNames(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	asker := model.Actor{Kind: "session", ID: "session-asker"}
	askID := seedAsk(t, database, "T-1", asker, "Which region goes first?")
	// The ask route appends the ask's ask.opened with its row, the event the answer route reads
	// the ask's identity from (events.OpenedEventIDs).
	appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "ask.opened", Actor: asker,
		Payload: model.NewAskEventPayload(model.Ask{
			ID: askID, IssueKey: new("T-1"), Author: asker, Question: "Which region goes first?", State: "open",
		}, model.ReferenceChanges{}),
	})
	handler := answerRoute(t, database, broker)
	answerAs(t, handler, askID, "alice",
		"East first. [@Planner](dispatch://agent/session-named) and [@Reviewer](dispatch://role/answer-reviewer) take it "+
			"from here; [Bob](dispatch://person/bob@example.com) reads it, `dispatch://agent/session-in-code` is an address, "+
			"and [@Asker](dispatch://agent/session-asker) asked.")
	event := answeredEvent(t, database, askID)
	publisher := &recordingPublisher{}
	stop := run(t, database, publisher, broker)
	defer stop()

	want := []string{
		"notifications.dispatch.issue.T-1.ask.answered",
		"notifications.agent.session-asker",
		"notifications.agent.session-named",
		"notifications.role.answer-reviewer",
	}
	waitFor(t, "the answer's publication", func() bool { return publishedAt(t, database, event) != nil })
	// The copies of the answer's own event; the ask's opening publishes on the issue's topic too.
	var copies []contracts.Envelope
	for _, item := range publisher.all() {
		if item.EventID == fmt.Sprintf("dispatch-%d", event) {
			copies = append(copies, item)
		}
	}
	if topics := topicsOf(copies); !reflect.DeepEqual(topics, want) {
		t.Fatalf("the answer went to %#v, want %#v: the issue, whoever asked once, and the agent and the role its text names", topics, want)
	}
	for _, item := range copies {
		if item.InReplyTo != askID {
			t.Fatalf("the copy on %s answers %q, want the ask %q", item.Topic, item.InReplyTo, askID)
		}
	}
}

// A named party that also asked or follows the ask gets one copy, and an answer that names nobody
// goes where it went before: the issue's topic and each follower. Each event is appended as the
// answer route appends it, its parties recorded on it (model.AskAnsweredEventPayload.NamedTargets),
// and published by one outbox; the record of each event (published_destinations) holds every
// session's topic once.
func TestAnAnswersNamedPartiesTakeOneCopyAndAnAnswerNamingNobodyGoesWhereItWent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		named []string
		want  []string
	}{
		{
			name: "named parties, two of whom asked or follow",
			named: []string{
				"session:session-asker", "session:session-follower", "session:session-named", "role:answer-reviewer",
			},
			want: []string{
				"notifications.dispatch.issue.T-1.ask.answered",
				"notifications.agent.session-asker",
				"notifications.agent.session-follower",
				"notifications.agent.session-named",
				"notifications.role.answer-reviewer",
			},
		},
		{
			name: "no named party",
			want: []string{
				"notifications.dispatch.issue.T-1.ask.answered",
				"notifications.agent.session-asker",
				"notifications.agent.session-follower",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			database := storetest.Open(t)
			broker := events.NewBroker()
			seedIssue(t, database, "T-1", nil)
			askID := seedAsk(t, database, "T-1", model.Actor{Kind: "session", ID: "session-asker"}, "Which region goes first?")
			seedFollower(t, database, askID, "session-follower")
			text := "East first."
			ask := model.Ask{
				ID: askID, IssueKey: new("T-1"), Question: "Which region goes first?", State: "answered",
				Answer: &model.AskAnswer{User: "alice", Selected: []string{}, Text: &text, At: time.Now().UTC()},
			}
			event := appendEvent(t, database, broker, model.Event{
				IssueKey: new("T-1"), Type: "ask.answered", Actor: model.Actor{Kind: "user", ID: "alice"},
				Payload: model.AskAnsweredEventPayload{
					AskEventPayload: model.NewAskEventPayload(ask, model.ReferenceChanges{}),
					NamedTargets:    tc.named,
				},
			})
			publisher := &recordingPublisher{}
			stop := run(t, database, publisher, broker)
			defer stop()

			waitFor(t, "the answer's publication", func() bool { return publishedAt(t, database, event.ID) != nil })
			if topics := topicsOf(publisher.all()); !reflect.DeepEqual(topics, tc.want) {
				t.Fatalf("the answer went to %#v, want %#v", topics, tc.want)
			}
			recorded, _ := publishedDestinations(t, database, event.ID)
			for _, topic := range tc.want {
				if count := countOf(recorded, topic); topic != "notifications.role.answer-reviewer" && count != 1 {
					t.Fatalf("the record holds %s %d times (%#v), want once", topic, count, recorded)
				}
			}
		})
	}
}

// countOf is how many times value is in values.
func countOf(values []string, value string) int {
	count := 0
	for _, item := range values {
		if item == value {
			count++
		}
	}
	return count
}

// The roles an answer's text names are roles its event addresses (addressedRoles), as the store
// census's role row side reads them (graphcensus's addressedRoles): the record of the answer's
// event gives each such role its node and its page, whatever destination of the batch resolved
// it, and the agents it names are no roles.
func TestTheRolesAnAnswerNamesAreRolesItsEventAddresses(t *testing.T) {
	t.Parallel()
	event := model.Event{
		ID: 41, IssueKey: new("T-1"), Type: "ask.answered", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: map[string]any{
			"id":            "ask-1",
			"named_targets": []any{"session:session-named", "role:answer-reviewer"},
		},
	}
	if roles := addressedRoles(event); !slices.Equal(roles, []string{"answer-reviewer"}) {
		t.Fatalf("the answer addresses the roles %#v, want answer-reviewer alone", roles)
	}
	write := deliveredGraph([]*batchEvent{{pendingEvent: pendingEvent{event: event}}}, nil, time.Now())
	for _, node := range []graphstore.Node{
		{Kind: graphstore.KindRole, ID: "answer-reviewer"},
		{Kind: graphstore.KindPage, ID: graphstore.RolePageID("answer-reviewer")},
	} {
		if !slices.ContainsFunc(write.nodes, func(written graphstore.Node) bool {
			return written.Kind == node.Kind && written.ID == node.ID && written.SourceEvent == event.ID
		}) {
			t.Fatalf("the record of the answer writes the nodes %+v, want %s %s caused by event %d", write.nodes, node.Kind, node.ID, event.ID)
		}
	}
}
