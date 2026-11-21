package outbox

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// seedDocument inserts a document creator made, owned by issueKey's issue when it is set and by
// the project TT otherwise, with one version per entry of versions crediting that entry's
// authors, and returns its id.
func seedDocument(t *testing.T, database *store.Store, issueKey *string, slug string, creator model.Actor, versions ...[]model.Actor) string {
	t.Helper()
	ctx := context.Background()
	creatorJSON, err := json.Marshal(creator)
	if err != nil {
		t.Fatalf("encode document creator: %v", err)
	}
	var id string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
		values ($1, 'TT', $2, $2 || '.md', 'doc', $3)
		returning id::text
	`, issueKey, slug, creatorJSON).Scan(&id); err != nil {
		t.Fatalf("create document %s: %v", slug, err)
	}
	for index, authors := range versions {
		encoded, err := json.Marshal(authors)
		if err != nil {
			t.Fatalf("encode version authors: %v", err)
		}
		if _, err := database.Pool.Exec(ctx, `
			insert into artifact_versions (artifact_id, number, markdown, authors) values ($1, $2, 'Text.', $3)
		`, id, index+1, encoded); err != nil {
			t.Fatalf("create version %d of %s: %v", index+1, slug, err)
		}
	}
	return id
}

// seedDocumentComment inserts a comment author wrote on a project document, and returns its id.
func seedDocumentComment(t *testing.T, database *store.Store, artifactID string, author model.Actor) string {
	t.Helper()
	authorJSON, err := json.Marshal(author)
	if err != nil {
		t.Fatalf("encode comment author: %v", err)
	}
	var id string
	if err := database.Pool.QueryRow(context.Background(), `
		insert into comments (artifact_id, author, body) values ($1, $2, 'On the document.') returning id::text
	`, artifactID, authorJSON).Scan(&id); err != nil {
		t.Fatalf("create document comment: %v", err)
	}
	return id
}

// agentTopics is every notifications.agent.<id> topic among topics, in order.
func agentTopics(topics []string) []string {
	agents := []string{}
	for _, topic := range topics {
		if strings.HasPrefix(topic, contracts.AgentTopicPrefix) {
			agents = append(agents, topic)
		}
	}
	return agents
}

func anchorOn(artifactID string) map[string]any {
	return map[string]any{"artifact_id": artifactID, "mark_id": "mark-1", "quote": "Text", "version": 1}
}

// TestACommentOnADocumentReachesTheSessionsThatWroteIt publishes comment events whose Notify the
// broker's own rule decides, as the outbox does, and reads which sessions' own topics they reached.
// A case that names a claimant publishes as the batch does for an issue a session claims, where
// the credits of a document are left out (addAuthorRoutes).
func TestACommentOnADocumentReachesTheSessionsThatWroteIt(t *testing.T) {
	t.Parallel()
	alice := model.Actor{Kind: "user", ID: "alice"}
	bob := model.Actor{Kind: "user", ID: "bob"}
	writer := model.Actor{Kind: "session", ID: "session-writer"}
	editor := model.Actor{Kind: "session", ID: "session-editor"}
	for _, tc := range []struct {
		name string
		// arrange seeds what the event is about and returns it with the slug of its document
		// owner, empty for an issue's event.
		arrange func(t *testing.T, database *store.Store) (model.Event, string)
		// claimant is the session the issue's claim is on, as the batch reads it (routing),
		// empty for an issue no session claims.
		claimant string
		// deliveries is the outcome of the comment's mention of each session (seedMentionDelivery).
		deliveries map[string]string
		wantAgents []string
		// wantKeys is the dedupe key of every session topic the event reaches, read where the
		// case turns on it.
		wantKeys map[string]string
	}{
		{
			name: "a person's comment on a passage of an issue's document reaches the session that wrote it",
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				doc := seedDocument(t, database, new("T-1"), "notes", writer, []model.Actor{writer})
				return model.Event{IssueKey: new("T-1"), Type: "comment.created", Actor: alice, Payload: map[string]any{
					"id": "00000000-0000-0000-0000-000000000001", "anchor": anchorOn(doc),
				}}, ""
			},
			wantAgents: []string{"notifications.agent.session-writer"},
		},
		{
			name: "a person's suggestion on an issue's document reaches the session that wrote it",
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				doc := seedDocument(t, database, new("T-1"), "notes", writer, []model.Actor{writer})
				return model.Event{IssueKey: new("T-1"), Type: "comment.created", Actor: alice, Payload: map[string]any{
					"id": "00000000-0000-0000-0000-000000000002", "anchor": anchorOn(doc),
					"suggestion": map[string]any{"replace_with": "Other text."},
				}}, ""
			},
			wantAgents: []string{"notifications.agent.session-writer"},
		},
		{
			name: "a person's comment on a whole project document reaches the session that wrote it",
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				doc := seedDocument(t, database, nil, "plan", writer, []model.Actor{writer})
				return model.Event{ArtifactID: new(doc), Project: "TT", Type: "comment.created", Actor: alice, Payload: map[string]any{
					"id": "00000000-0000-0000-0000-000000000003",
				}}, "plan"
			},
			wantAgents: []string{"notifications.agent.session-writer"},
		},
		{
			name: "a comment on a project image reaches no document author",
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				creator, err := json.Marshal(writer)
				if err != nil {
					t.Fatalf("encode image creator: %v", err)
				}
				var image string
				if err := database.Pool.QueryRow(context.Background(), `
					insert into artifacts (project_key, slug, name, kind, created_by)
					values ('TT', 'diagram', 'diagram.png', 'image', $1)
					returning id::text
				`, creator).Scan(&image); err != nil {
					t.Fatalf("create image: %v", err)
				}
				return model.Event{ArtifactID: new(image), Project: "TT", Type: "comment.created", Actor: alice, Payload: map[string]any{
					"id": "00000000-0000-0000-0000-000000000014",
				}}, "diagram"
			},
			wantAgents: []string{},
		},
		{
			name: "every session the document credits is reached once, the creator first",
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				doc := seedDocument(t, database, new("T-1"), "notes", writer,
					[]model.Actor{writer}, []model.Actor{editor, bob}, []model.Actor{writer, editor})
				return model.Event{IssueKey: new("T-1"), Type: "comment.created", Actor: alice, Payload: map[string]any{
					"id": "00000000-0000-0000-0000-000000000004", "anchor": anchorOn(doc),
				}}, ""
			},
			wantAgents: []string{"notifications.agent.session-writer", "notifications.agent.session-editor"},
		},
		{
			name: "a document only people wrote reaches no session",
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				doc := seedDocument(t, database, new("T-1"), "notes", alice, []model.Actor{alice}, []model.Actor{bob})
				return model.Event{IssueKey: new("T-1"), Type: "comment.created", Actor: model.Actor{Kind: "user", ID: "carol"}, Payload: map[string]any{
					"id": "00000000-0000-0000-0000-000000000005", "anchor": anchorOn(doc),
				}}, ""
			},
			wantAgents: []string{},
		},
		{
			name: "a version whose authors are JSON null credits no one and stops nothing",
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				doc := seedDocument(t, database, new("T-1"), "notes", writer, nil, []model.Actor{editor})
				return model.Event{IssueKey: new("T-1"), Type: "comment.created", Actor: alice, Payload: map[string]any{
					"id": "00000000-0000-0000-0000-000000000013", "anchor": anchorOn(doc),
				}}, ""
			},
			wantAgents: []string{"notifications.agent.session-writer", "notifications.agent.session-editor"},
		},
		{
			name: "a person's edit of their comment on a passage reaches the document's writer",
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				doc := seedDocument(t, database, new("T-1"), "notes", writer, []model.Actor{writer})
				return model.Event{IssueKey: new("T-1"), Type: "comment.edited", Actor: alice, Payload: map[string]any{
					"id": "00000000-0000-0000-0000-000000000006", "anchor": anchorOn(doc),
				}}, ""
			},
			wantAgents: []string{"notifications.agent.session-writer"},
		},
		{
			name: "an author the comment mentions is left to the mention that reached it, the other authors reached",
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				doc := seedDocument(t, database, new("T-1"), "notes", writer, []model.Actor{writer, editor})
				return model.Event{IssueKey: new("T-1"), Type: "comment.created", Actor: alice, Payload: map[string]any{
					"id": "00000000-0000-0000-0000-000000000007", "anchor": anchorOn(doc),
					"mentions": mentionsOf("session-writer"),
				}}, ""
			},
			deliveries: map[string]string{"session-writer": "sent"},
			wantAgents: []string{"notifications.agent.session-editor"},
		},
		{
			name: "an author whose mention failed is reached by the author route",
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				doc := seedDocument(t, database, new("T-1"), "notes", writer, []model.Actor{writer, editor})
				return model.Event{IssueKey: new("T-1"), Type: "comment.created", Actor: alice, Payload: map[string]any{
					"id": "00000000-0000-0000-0000-000000000015", "anchor": anchorOn(doc),
					"mentions": mentionsOf("session-writer"),
				}}, ""
			},
			deliveries: map[string]string{"session-writer": "failed"},
			wantAgents: []string{"notifications.agent.session-writer", "notifications.agent.session-editor"},
		},
		{
			// On an issue a session claims the credits of a document are no second reason to
			// deliver what that claimant already hears on its own topic (addAuthorRoutes), so the
			// editor, a credit of the document alone, is left out. The writer is not: the comment
			// names it and its mention never reached it, so it takes this copy instead, under the
			// key that mention's send went under.
			name:     "a credited author whose mention failed is reached on a claimed issue, under the mention's key",
			claimant: "session-claimant",
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				doc := seedDocument(t, database, new("T-1"), "notes", writer, []model.Actor{writer, editor})
				return model.Event{IssueKey: new("T-1"), Type: "comment.created", Actor: alice, Payload: map[string]any{
					"id": "00000000-0000-0000-0000-000000000016", "anchor": anchorOn(doc),
					"mentions": mentionsOf("session-writer"),
				}}, ""
			},
			deliveries: map[string]string{"session-writer": "failed"},
			wantAgents: []string{"notifications.agent.session-claimant", "notifications.agent.session-writer"},
			wantKeys: map[string]string{
				"notifications.agent.session-claimant": "dispatch-100",
				"notifications.agent.session-writer": contracts.AgentSendDedupeKey("session-writer",
					model.CommentMentionIdempotencyKey("00000000-0000-0000-0000-000000000016", "session:session-writer", "steer")),
			},
		},
		{
			// The rule that keeps a credit whose copy a mention still decides exempts this one
			// too - its mention reads mentionSent, not mentionFailed
			// (mentionDeliveries.decidesCopyFor) - and it still gets nothing, by the other road:
			// the mention reached it, so the mention rule leaves its copy to that mention and
			// builds none (destinations.addSession, destinations.leftToMention). The two roads to
			// "no copy" are what a later reader confuses: this session's copy is the mention's,
			// not a session the credits filter dropped, which is why exempting a mention that was
			// sent costs nothing.
			name:     "a credited author the comment's mention reached is left to that mention on a claimed issue",
			claimant: "session-claimant",
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				doc := seedDocument(t, database, new("T-1"), "notes", writer, []model.Actor{writer, editor})
				return model.Event{IssueKey: new("T-1"), Type: "comment.created", Actor: alice, Payload: map[string]any{
					"id": "00000000-0000-0000-0000-000000000018", "anchor": anchorOn(doc),
					"mentions": mentionsOf("session-writer"),
				}}, ""
			},
			deliveries: map[string]string{"session-writer": "sent"},
			wantAgents: []string{"notifications.agent.session-claimant"},
		},
		{
			// An edit reads which of its comment's mentions failed of that comment alone
			// (lookup.mentionFallbackKeys), so the writer, which the comment names and no mention
			// of it reached, takes the edit as well, and the editor, a credit of the document
			// alone, does not. The edit's copy carries the event's own key, not the mention's:
			// the keyed copy is a new comment's, read from the attempts the destination build
			// itself holds (destinations.addSession, mentionDeliveries.fallbackKeys), and an edit
			// holds none.
			name:     "a credited author whose mention of the comment failed is reached by its edit on a claimed issue, under the event's own key",
			claimant: "session-claimant",
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				doc := seedDocument(t, database, new("T-1"), "notes", writer, []model.Actor{writer, editor})
				return model.Event{IssueKey: new("T-1"), Type: "comment.edited", Actor: alice, Payload: map[string]any{
					"id": "00000000-0000-0000-0000-000000000017", "anchor": anchorOn(doc),
					"mentions": mentionsOf("session-writer"),
				}}, ""
			},
			deliveries: map[string]string{"session-writer": "failed"},
			wantAgents: []string{"notifications.agent.session-claimant", "notifications.agent.session-writer"},
			wantKeys: map[string]string{
				"notifications.agent.session-claimant": "dispatch-100",
				"notifications.agent.session-writer":   "dispatch-100",
			},
		},
		{
			name: "a reply on a project document reaches its thread's author and the document's writer",
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				doc := seedDocument(t, database, nil, "plan", writer, []model.Actor{writer})
				root := seedDocumentComment(t, database, doc, model.Actor{Kind: "session", ID: "session-root"})
				return model.Event{ArtifactID: new(doc), Project: "TT", Type: "comment.created", Actor: alice, Payload: map[string]any{
					"id": "00000000-0000-0000-0000-000000000008", "reply_to": root,
				}}, "plan"
			},
			wantAgents: []string{"notifications.agent.session-root", "notifications.agent.session-writer"},
		},
		{
			name: "a reply to an ask on a project document reaches the ask's followers, not the document's writer",
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				doc := seedDocument(t, database, nil, "plan", writer, []model.Actor{writer})
				var askID string
				if err := database.Pool.QueryRow(context.Background(), `
					insert into asks (artifact_id, author, question) values ($1, '{"kind":"session","id":"session-asker"}', 'Ship it?')
					returning id::text
				`, doc).Scan(&askID); err != nil {
					t.Fatalf("create document ask: %v", err)
				}
				seedFollower(t, database, askID, "session-asker")
				return model.Event{ArtifactID: new(doc), Project: "TT", Type: "comment.created", Actor: alice, Payload: map[string]any{
					"id": "00000000-0000-0000-0000-000000000009", "ask_id": askID,
				}}, "plan"
			},
			wantAgents: []string{"notifications.agent.session-asker"},
		},
		{
			name: "a comment on the issue's conversation, which names no document, reaches no document's writer",
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				seedDocument(t, database, new("T-1"), "spec", writer, []model.Actor{writer})
				return model.Event{IssueKey: new("T-1"), Type: "comment.created", Actor: alice, Payload: map[string]any{
					"id": "00000000-0000-0000-0000-000000000010",
				}}, ""
			},
			wantAgents: []string{},
		},
		{
			// An agent's comment does not notify (events.Broker.Notify: only a person's event
			// does), so no author route runs for it: the writer commenting on its own document
			// is not told, and neither is the document's other writer.
			name: "an agent's comment reaches no author, the commenter included",
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				doc := seedDocument(t, database, new("T-1"), "notes", writer, []model.Actor{writer, editor})
				return model.Event{IssueKey: new("T-1"), Type: "comment.created", Actor: writer, Payload: map[string]any{
					"id": "00000000-0000-0000-0000-000000000011", "anchor": anchorOn(doc),
				}}, ""
			},
			wantAgents: []string{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			database := storetest.Open(t)
			seedIssue(t, database, "T-1", nil)
			event, slug := tc.arrange(t, database)
			for sessionID, outcome := range tc.deliveries {
				seedMentionDelivery(t, database, payloadString(event.Payload, "id"), sessionID, outcome)
			}
			event.ID = 100
			event.CreatedAt = time.Now()
			event.Notify = events.NewBroker().Notify(event)
			publisher := &recordingPublisher{}
			if err := publish(context.Background(), Deps{Store: database, Publisher: publisher}, event, slug, routing{claimant: tc.claimant}, map[string]struct{}{}); err != nil {
				t.Fatalf("publish %s: %v", event.Type, err)
			}
			topics := topicsOf(publisher.all())
			if got := agentTopics(topics); !slices.Equal(got, tc.wantAgents) {
				t.Fatalf("sessions reached = %v, want %v (every topic: %v)", got, tc.wantAgents, topics)
			}
			if tc.wantKeys != nil {
				keys := map[string]string{}
				for _, item := range publisher.all() {
					if strings.HasPrefix(item.Topic, contracts.AgentTopicPrefix) {
						keys[item.Topic] = item.DedupeKey
					}
				}
				if !maps.Equal(keys, tc.wantKeys) {
					t.Fatalf("dedupe keys by session topic = %#v, want %#v", keys, tc.wantKeys)
				}
			}
		})
	}
}

// The author routes skip the event's own actor wherever they find it, so a writer is never told
// of its own comment even should an agent's comment notify.
func TestAddAuthorRoutesSkipsTheCommenterAmongTheDocumentsWriters(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	seedIssue(t, database, "T-1", nil)
	writer := model.Actor{Kind: "session", ID: "session-writer"}
	editor := model.Actor{Kind: "session", ID: "session-editor"}
	doc := seedDocument(t, database, new("T-1"), "notes", writer, []model.Actor{writer, editor})
	event := model.Event{
		IssueKey: new("T-1"),
		Type:     "comment.created",
		Actor:    writer,
		Payload:  map[string]any{"id": "00000000-0000-0000-0000-000000000012", "anchor": anchorOn(doc)},
	}
	if got := routeTopics(t, database, event, addAuthorRoutes); !slices.Equal(got, []string{"notifications.agent.session-editor"}) {
		t.Fatalf("author routes = %v, want the document's other writer alone", got)
	}
}

// A person's spec edit, once its quiet window ends, reaches each session the document credits once
// and the issue's route once - its role route forwarded to the role's holder (heldRoles), which
// carries the role's topic - and never the person: a person has no Envoy topic, and the edit's
// own actor is skipped. The event names its document by the payload's artifact_id, as the notice
// sweep writes it (docs.sendEditNotice).
func TestAPersonsSpecEditReachesEachSessionThatWroteTheSpecOnce(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	route := "role:reviewer"
	seedIssue(t, database, "T-1", &route)
	alice := model.Actor{Kind: "user", ID: "alice@example.com"}
	writer := model.Actor{Kind: "session", ID: "session-writer"}
	editor := model.Actor{Kind: "session", ID: "session-editor"}
	spec := seedDocument(t, database, new("T-1"), "spec", writer, []model.Actor{writer}, []model.Actor{editor, writer}, []model.Actor{alice})
	event := model.Event{
		ID: 100, CreatedAt: time.Now(), IssueKey: new("T-1"), Type: "artifact.human_edited", Actor: alice,
		Payload: map[string]any{
			"artifact_id": spec, "name": "spec.md", "from_version": 2, "to_version": 3,
			"editors": []any{alice.ID}, "diff": "--- previous\n+++ current\n", "diff_truncated": false,
		},
	}
	event.Notify = events.NewBroker().Notify(event)
	if !event.Notify {
		t.Fatal("a person's artifact.human_edited does not notify, so no route would carry it")
	}
	publisher := &recordingPublisher{}
	if err := publish(context.Background(), withHeldRoles(Deps{Store: database, Publisher: publisher}), event, "", routing{route: &route}, map[string]struct{}{}); err != nil {
		t.Fatalf("publish artifact.human_edited: %v", err)
	}
	topics := topicsOf(publisher.all())
	if got, want := agentTopics(topics), []string{"notifications.agent.session-writer", "notifications.agent.session-editor"}; !slices.Equal(got, want) {
		t.Fatalf("sessions reached = %v, want %v (every topic: %v)", got, want, topics)
	}
	routed := 0
	for _, topic := range topics {
		if topic == "notifications.role.reviewer" {
			routed++
		}
		if strings.Contains(topic, alice.ID) {
			t.Fatalf("the person was reached on %q", topic)
		}
	}
	if routed != 1 {
		t.Fatalf("the issue's route was reached %d times, want once (every topic: %v)", routed, topics)
	}
}

// A person's edit of a document still reaches every session that wrote it where a session claims
// the document's issue. The rule that leaves the credits of a document out on such an issue is a
// rule about a person's comment and its edit alone (addAuthorRoutes): the claimant hears those on
// its own topic (addClaimantRoute), while an artifact.human_edited takes no claimant route at all,
// so nothing else would tell the document's writers that a person changed what they wrote.
func TestAPersonsSpecEditOnAClaimedIssueStillReachesEachSessionThatWroteIt(t *testing.T) {
	database := storetest.Open(t)
	seedIssue(t, database, "T-1", nil)
	claimant := model.Actor{Kind: "session", ID: "session-claimant"}
	claimIssue(t, database, "T-1", claimant)
	alice := model.Actor{Kind: "user", ID: "alice@example.com"}
	writer := model.Actor{Kind: "session", ID: "session-writer"}
	editor := model.Actor{Kind: "session", ID: "session-editor"}
	spec := seedDocument(t, database, new("T-1"), "spec", writer, []model.Actor{writer}, []model.Actor{editor})
	event := model.Event{
		ID: 100, CreatedAt: time.Now(), IssueKey: new("T-1"), Type: "artifact.human_edited", Actor: alice,
		Payload: map[string]any{
			"artifact_id": spec, "name": "spec.md", "from_version": 1, "to_version": 2,
			"editors": []any{alice.ID}, "diff": "--- previous\n+++ current\n", "diff_truncated": false,
		},
	}
	event.Notify = events.NewBroker().Notify(event)
	publisher := &recordingPublisher{}
	deps := Deps{Store: database, Publisher: publisher}
	if err := publish(context.Background(), deps, event, "", routing{claimant: claimant.ID}, map[string]struct{}{}); err != nil {
		t.Fatalf("publish artifact.human_edited: %v", err)
	}
	topics := topicsOf(publisher.all())
	want := []string{"notifications.agent.session-writer", "notifications.agent.session-editor"}
	if got := agentTopics(topics); !slices.Equal(got, want) {
		t.Fatalf("sessions reached = %v, want %v, the document's writers whatever the issue's claim is (every topic: %v)", got, want, topics)
	}
}
