package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// seedThreadComment inserts a comment author wrote on issueKey's issue and returns its id. The
// comment is anchored on the document anchoredOn names when that is not empty, and replies to
// replyTo when replyTo is set.
func seedThreadComment(t *testing.T, database *store.Store, issueKey, anchoredOn string, author model.Actor, replyTo *string) string {
	t.Helper()
	authorJSON, err := json.Marshal(author)
	if err != nil {
		t.Fatalf("encode comment author: %v", err)
	}
	var anchor []byte
	if anchoredOn != "" {
		if anchor, err = json.Marshal(anchorOn(anchoredOn)); err != nil {
			t.Fatalf("encode comment anchor: %v", err)
		}
	}
	var id string
	if err := database.Pool.QueryRow(context.Background(), `
		insert into comments (issue_key, author, body, anchor, reply_to) values ($1, $2, 'In the thread.', $3, $4)
		returning id::text
	`, issueKey, authorJSON, anchor, replyTo).Scan(&id); err != nil {
		t.Fatalf("create thread comment: %v", err)
	}
	return id
}

// appendCommentEvent appends a comment event on T-1 with the payload the API writes for it.
func appendCommentEvent(t *testing.T, database *store.Store, broker *events.Broker, eventType string, actor model.Actor, comment model.Comment, mentioned ...string) model.Event {
	t.Helper()
	return appendIssueCommentEvent(t, database, broker, "T-1", eventType, actor, comment, mentioned...)
}

// appendIssueCommentEvent appends a comment event on issueKey's issue with the payload the API
// writes for it: the comment's mentions resolved to each session of mentioned, whose attempt
// rows the outbox's session routes read (destinations.leftToMention).
func appendIssueCommentEvent(t *testing.T, database *store.Store, broker *events.Broker, issueKey, eventType string, actor model.Actor, comment model.Comment, mentioned ...string) model.Event {
	t.Helper()
	comment.IssueKey = new(issueKey)
	comment.Author = actor
	for _, sessionID := range mentioned {
		comment.Mentions = append(comment.Mentions, mentionOf(sessionID)...)
	}
	return appendEvent(t, database, broker, model.Event{
		IssueKey: new(issueKey),
		Type:     eventType,
		Actor:    actor,
		Payload:  model.CommentEventPayload{Comment: comment},
	})
}

// makePrimary makes the document the issue's primary document, as its spec is.
func makePrimary(t *testing.T, database *store.Store, artifactID string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `update artifacts set is_primary = true where id = $1`, artifactID); err != nil {
		t.Fatalf("make document %s primary: %v", artifactID, err)
	}
}

// seedSecondIssue inserts a second issue, with no route, in the project seedIssue made.
func seedSecondIssue(t *testing.T, database *store.Store, key string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ($1, 'TT', 2, 'Second issue', '{"kind":"user","id":"alice"}', 'V')
	`, key); err != nil {
		t.Fatalf("create issue %s: %v", key, err)
	}
}

// recipientCheck is one event of a case and the sessions whose own topics it must reach, in order.
type recipientCheck struct {
	event model.Event
	want  []string
}

// TestAPersonsCommentReachesTheClaimantAndTheThreadsSessions appends comment events as the API
// writes them, runs one batch of the outbox over them, and reads which sessions' own topics each
// event reached. The claim and the route come from the issue's row, as the outbox reads them. The
// claimant's copy is #2225's (addClaimantRoute); the thread's sessions are #2366's.
func TestAPersonsCommentReachesTheClaimantAndTheThreadsSessions(t *testing.T) {
	sami := model.Actor{Kind: "user", ID: "sami"}
	alice := model.Actor{Kind: "user", ID: "alice"}
	claimant := model.Actor{Kind: "session", ID: "session-claimant"}
	writer := model.Actor{Kind: "session", ID: "session-writer"}
	rooter := model.Actor{Kind: "session", ID: "session-rooter"}
	replier := model.Actor{Kind: "session", ID: "session-replier"}
	for _, tc := range []struct {
		name    string
		route   *string
		arrange func(t *testing.T, database *store.Store, broker *events.Broker) []recipientCheck
	}{
		{
			// The reported case: a person comments on a passage of the issue's spec, which another session
			// wrote; the claimant replies and resolves; the person replies, which reopens the thread.
			// The spec's writer hears none of it: on an issue a session claims, the credits of a
			// document are no second reason to deliver what that claimant already hears on its own
			// topic (addAuthorRoutes), and the writer wrote no comment in this thread.
			name: "a person's reply on a claimed unrouted issue reaches the claimant alone, not the document's writer",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) []recipientCheck {
				claimIssue(t, database, "T-1", claimant)
				spec := seedDocument(t, database, new("T-1"), "spec", writer, []model.Actor{writer}, []model.Actor{claimant})
				root := seedThreadComment(t, database, "T-1", spec, sami, nil)
				seedThreadComment(t, database, "T-1", "", claimant, &root)
				reply := seedThreadComment(t, database, "T-1", "", sami, &root)
				reopened := appendCommentEvent(t, database, broker, "comment.reopened", sami, model.Comment{ID: root, Anchor: &model.Anchor{ArtifactID: spec}})
				created := appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: reply, ReplyTo: &root})
				return []recipientCheck{
					{event: reopened, want: []string{claimant.ID}},
					{event: created, want: []string{claimant.ID}},
				}
			},
		},
		{
			name: "a claimant who also wrote in the thread gets one copy of a person's reply",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) []recipientCheck {
				claimIssue(t, database, "T-1", claimant)
				root := seedThreadComment(t, database, "T-1", "", claimant, nil)
				seedThreadComment(t, database, "T-1", "", replier, &root)
				seedThreadComment(t, database, "T-1", "", claimant, &root)
				reply := seedThreadComment(t, database, "T-1", "", sami, &root)
				created := appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: reply, ReplyTo: &root})
				return []recipientCheck{{event: created, want: []string{claimant.ID, replier.ID}}}
			},
		},
		{
			name: "a reply reaches the session that started the thread once and every session that replied",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) []recipientCheck {
				root := seedThreadComment(t, database, "T-1", "", rooter, nil)
				seedThreadComment(t, database, "T-1", "", replier, &root)
				seedThreadComment(t, database, "T-1", "", rooter, &root)
				reply := seedThreadComment(t, database, "T-1", "", alice, &root)
				created := appendCommentEvent(t, database, broker, "comment.created", alice, model.Comment{ID: reply, ReplyTo: &root})
				return []recipientCheck{{event: created, want: []string{rooter.ID, replier.ID}}}
			},
		},
		{
			name:  "a reply on a routed claimed issue reaches the route's holder, the claimant and the thread's sessions",
			route: new("session:session-holder"),
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) []recipientCheck {
				claimIssue(t, database, "T-1", claimant)
				root := seedThreadComment(t, database, "T-1", "", alice, nil)
				seedThreadComment(t, database, "T-1", "", replier, &root)
				reply := seedThreadComment(t, database, "T-1", "", alice, &root)
				created := appendCommentEvent(t, database, broker, "comment.created", alice, model.Comment{ID: reply, ReplyTo: &root})
				return []recipientCheck{{event: created, want: []string{"session-holder", claimant.ID, replier.ID}}}
			},
		},
		{
			// An issue no session claims keeps the credits of the thread's document: the holder of
			// its route is no claimant, and nothing else tells the document's writer of the reply.
			name:  "a reply on a routed unclaimed issue reaches the route's holder, the thread's sessions and the document's writer",
			route: new("session:session-holder"),
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) []recipientCheck {
				spec := seedDocument(t, database, new("T-1"), "spec", writer, []model.Actor{writer})
				root := seedThreadComment(t, database, "T-1", spec, rooter, nil)
				reply := seedThreadComment(t, database, "T-1", "", alice, &root)
				created := appendCommentEvent(t, database, broker, "comment.created", alice, model.Comment{ID: reply, ReplyTo: &root})
				return []recipientCheck{{event: created, want: []string{"session-holder", rooter.ID, writer.ID}}}
			},
		},
		{
			name: "a thread session the reply mentions is left to the mention once it delivered, and published to when it failed",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) []recipientCheck {
				claimIssue(t, database, "T-1", claimant)
				root := seedThreadComment(t, database, "T-1", "", alice, nil)
				seedThreadComment(t, database, "T-1", "", claimant, &root)
				seedThreadComment(t, database, "T-1", "", replier, &root)
				seedThreadComment(t, database, "T-1", "", rooter, &root)
				reply := seedThreadComment(t, database, "T-1", "", alice, &root)
				seedMentionDelivery(t, database, reply, claimant.ID, "sent")
				seedMentionDelivery(t, database, reply, replier.ID, "sent")
				seedMentionDelivery(t, database, reply, rooter.ID, "failed")
				created := appendCommentEvent(t, database, broker, "comment.created", alice, model.Comment{ID: reply, ReplyTo: &root},
					claimant.ID, replier.ID, rooter.ID)
				return []recipientCheck{{event: created, want: []string{rooter.ID}}}
			},
		},
		{
			name: "a person's claim adds no recipient",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) []recipientCheck {
				claimIssue(t, database, "T-1", model.Actor{Kind: "user", ID: "bob"})
				root := seedThreadComment(t, database, "T-1", "", alice, nil)
				seedThreadComment(t, database, "T-1", "", replier, &root)
				reply := seedThreadComment(t, database, "T-1", "", alice, &root)
				created := appendCommentEvent(t, database, broker, "comment.created", alice, model.Comment{ID: reply, ReplyTo: &root})
				return []recipientCheck{{event: created, want: []string{replier.ID}}}
			},
		},
		{
			// A person's claim leaves routing.claimant empty (readBatch), so the rule that leaves a
			// document's credits out on an issue a session claims is inert here: the writer of the
			// document this thread hangs on still gets the person's reply. A rule keyed on the
			// issue having a claim at all, rather than on the claimant being a session, would drop it.
			name: "a person's claim leaves the credits of the thread's document in",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) []recipientCheck {
				claimIssue(t, database, "T-1", model.Actor{Kind: "user", ID: "bob"})
				spec := seedDocument(t, database, new("T-1"), "spec", writer, []model.Actor{writer})
				root := seedThreadComment(t, database, "T-1", spec, alice, nil)
				reply := seedThreadComment(t, database, "T-1", "", alice, &root)
				created := appendCommentEvent(t, database, broker, "comment.created", alice, model.Comment{ID: reply, ReplyTo: &root})
				return []recipientCheck{{event: created, want: []string{writer.ID}}}
			},
		},
		{
			// The reply reaches the thread's sessions and the claimant; the writer of the document
			// the thread hangs on is a credit of it alone, which a session's claim leaves out.
			name: "the reopening a reply makes reaches the claimant and the thread's root author, and the reply the thread's sessions",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) []recipientCheck {
				claimIssue(t, database, "T-1", claimant)
				spec := seedDocument(t, database, new("T-1"), "spec", writer, []model.Actor{writer})
				root := seedThreadComment(t, database, "T-1", spec, rooter, nil)
				seedThreadComment(t, database, "T-1", "", replier, &root)
				reply := seedThreadComment(t, database, "T-1", "", alice, &root)
				reopened := appendCommentEvent(t, database, broker, "comment.reopened", alice, model.Comment{ID: root, Anchor: &model.Anchor{ArtifactID: spec}})
				created := appendCommentEvent(t, database, broker, "comment.created", alice, model.Comment{ID: reply, ReplyTo: &root})
				return []recipientCheck{
					{event: reopened, want: []string{claimant.ID, rooter.ID}},
					{event: created, want: []string{claimant.ID, rooter.ID, replier.ID}},
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := storetest.Open(t)
			broker := events.NewBroker()
			seedIssue(t, database, "T-1", tc.route)
			checkSessionRecipients(t, database, tc.arrange(t, database, broker))
		})
	}
}

// checkSessionRecipients runs one batch of the outbox and fails unless each check's event was
// published to the own topics of its sessions, in that order, and of no other session. The batch
// sends each destination's chain beside the others (planChains), so an event's destinations reach
// the publisher in no order of the event's: the order is the one the batch records them in
// (published_destinations), and the publisher must have been sent exactly those.
//
// The batch runs with an event broker, as every outbox does: the mention rule appends a
// comment.delivery receipt through it where it settles an abandoned attempt before it publishes
// that session's copy (mentionComment.settleAbandoned,
// TestTheCreditsFilterKeepsACreditExactlyWhereAMentionDecidesItsCopy), and a batch without one
// fails that event rather than publishing it.
func checkSessionRecipients(t *testing.T, database *store.Store, checks []recipientCheck) {
	t.Helper()
	checkSessionRecipientsReading(t, database, nil, checks)
}

// checkSessionRecipientsReading is checkSessionRecipients on a batch whose Envoy registry is
// registry (Deps.Registry, nil for none), and returns every envelope the batch sent.
func checkSessionRecipientsReading(t *testing.T, database *store.Store, registry RegistryReader, checks []recipientCheck) []contracts.Envelope {
	t.Helper()
	publisher := &recordingPublisher{}
	deps := Deps{Store: database, Publisher: publisher, Broker: events.NewBroker(), Registry: registry}
	if _, err := scanBatch(context.Background(), deps, unfenced{}, holding(t, deps).instance); err != nil {
		t.Fatalf("scan batch: %v", err)
	}
	return checkSentRecipients(t, database, publisher, checks)
}

// checkSentRecipients checks, after a batch sent through publisher, that each event of checks was
// published, sent to the topics its row records, and reached the sessions it must reach, and
// returns every envelope the batch sent.
func checkSentRecipients(t *testing.T, database *store.Store, publisher *recordingPublisher, checks []recipientCheck) []contracts.Envelope {
	t.Helper()
	byEvent := map[string][]string{}
	for _, item := range publisher.all() {
		byEvent[item.SourceEventID] = append(byEvent[item.SourceEventID], item.Topic)
	}
	for _, check := range checks {
		row := readEventRow(t, database, check.event.ID)
		if row.publishedAt == nil {
			t.Fatalf("%s event %d was not published", check.event.Type, check.event.ID)
		}
		if sent := byEvent[fmt.Sprint(check.event.ID)]; !slices.Equal(slices.Sorted(slices.Values(sent)), slices.Sorted(slices.Values(row.destinations))) {
			t.Fatalf("%s event %d was sent to %v and recorded %v", check.event.Type, check.event.ID, sent, row.destinations)
		}
		want := make([]string, 0, len(check.want))
		for _, sessionID := range check.want {
			want = append(want, contracts.LegacyAgentSubject(sessionID))
		}
		topics := row.destinations
		if got := agentTopics(topics); !slices.Equal(got, want) {
			t.Fatalf("%s event %d reached sessions %v, want %v (every topic: %v)", check.event.Type, check.event.ID, got, want, topics)
		}
	}
	return publisher.all()
}

// seedSpec inserts issueKey's primary document, which creator made, with one version per entry of
// versions crediting that entry's authors, and returns its id.
func seedSpec(t *testing.T, database *store.Store, issueKey string, creator model.Actor, versions ...[]model.Actor) string {
	t.Helper()
	spec := seedDocument(t, database, new(issueKey), "spec", creator, versions...)
	makePrimary(t, database, spec)
	return spec
}

// TestAPersonsCommentOnAnIssueNobodyRoutesOrClaimsReachesItsSpecsSessions covers #2366: a
// person's comment on an issue with no route and no session claim reaches the sessions the issue's
// primary document credits, where a session claimant would get #2225's copy. The spec of each case
// credits session-spec (its creator) and session-editor, and also a person and a session credit
// with no id, which have no topic.
func TestAPersonsCommentOnAnIssueNobodyRoutesOrClaimsReachesItsSpecsSessions(t *testing.T) {
	sami := model.Actor{Kind: "user", ID: "sami"}
	alice := model.Actor{Kind: "user", ID: "alice"}
	specWriter := model.Actor{Kind: "session", ID: "session-spec"}
	editor := model.Actor{Kind: "session", ID: "session-editor"}
	claimant := model.Actor{Kind: "session", ID: "session-claimant"}
	replier := model.Actor{Kind: "session", ID: "session-replier"}
	specSessions := []string{specWriter.ID, editor.ID}
	for _, tc := range []struct {
		name    string
		route   *string
		arrange func(t *testing.T, database *store.Store, broker *events.Broker, spec string) []recipientCheck
	}{
		{
			// A spec written without a claim: the session that wrote every version of the spec held
			// no claim.
			name: "a person's conversation comment, its edit and its delete reach the spec's sessions, and a reopening does not",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker, spec string) []recipientCheck {
				root := seedThreadComment(t, database, "T-1", "", alice, nil)
				comment := seedThreadComment(t, database, "T-1", "", sami, nil)
				created := appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: comment})
				edited := appendCommentEvent(t, database, broker, "comment.edited", sami, model.Comment{ID: comment})
				deleted := appendCommentEvent(t, database, broker, "comment.deleted", sami, model.Comment{ID: comment})
				reopened := appendCommentEvent(t, database, broker, "comment.reopened", sami, model.Comment{ID: root})
				return []recipientCheck{
					{event: created, want: specSessions},
					{event: edited, want: specSessions},
					{event: deleted, want: specSessions},
					{event: reopened, want: []string{}},
				}
			},
		},
		{
			name: "a person's reply in a conversation thread with no session reaches the spec's sessions",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker, spec string) []recipientCheck {
				root := seedThreadComment(t, database, "T-1", "", alice, nil)
				reply := seedThreadComment(t, database, "T-1", "", sami, &root)
				created := appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: reply, ReplyTo: &root})
				return []recipientCheck{{event: created, want: specSessions}}
			},
		},
		{
			name: "a person's reply reaches the spec's sessions, then the thread's",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker, spec string) []recipientCheck {
				root := seedThreadComment(t, database, "T-1", "", alice, nil)
				seedThreadComment(t, database, "T-1", "", replier, &root)
				reply := seedThreadComment(t, database, "T-1", "", sami, &root)
				created := appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: reply, ReplyTo: &root})
				return []recipientCheck{{event: created, want: append(slices.Clone(specSessions), replier.ID)}}
			},
		},
		{
			name: "a person's comment on a passage of the spec reaches each of its sessions once",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker, spec string) []recipientCheck {
				comment := seedThreadComment(t, database, "T-1", spec, sami, nil)
				created := appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: comment, Anchor: &model.Anchor{ArtifactID: spec}})
				return []recipientCheck{{event: created, want: specSessions}}
			},
		},
		{
			name: "a person's comment in an ask's thread reaches the spec's sessions and the followers",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker, spec string) []recipientCheck {
				askID := seedAsk(t, database, "T-1", model.Actor{Kind: "session", ID: "session-asker"}, "Ship it?")
				created := appendCommentEvent(t, database, broker, "comment.created", sami,
					model.Comment{ID: "8f14e45f-ceea-467a-9c1e-1b4d9a3f1c2b", AskID: &askID})
				return []recipientCheck{{event: created, want: append(slices.Clone(specSessions), "session-asker")}}
			},
		},
		{
			name: "on an issue a person claimed, a person's comment reaches the spec's sessions",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker, spec string) []recipientCheck {
				claimIssue(t, database, "T-1", model.Actor{Kind: "user", ID: "bob"})
				comment := seedThreadComment(t, database, "T-1", "", sami, nil)
				created := appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: comment})
				return []recipientCheck{{event: created, want: specSessions}}
			},
		},
		{
			name: "on an issue a session claimed, a person's comment reaches the claimant and not the spec's sessions",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker, spec string) []recipientCheck {
				claimIssue(t, database, "T-1", claimant)
				comment := seedThreadComment(t, database, "T-1", "", sami, nil)
				created := appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: comment})
				return []recipientCheck{{event: created, want: []string{claimant.ID}}}
			},
		},
		{
			name:  "on a routed issue, a person's comment reaches the route and not the spec's sessions",
			route: new("session:session-holder"),
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker, spec string) []recipientCheck {
				comment := seedThreadComment(t, database, "T-1", "", sami, nil)
				created := appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: comment})
				return []recipientCheck{{event: created, want: []string{"session-holder"}}}
			},
		},
		{
			name: "a spec session the comment mentions is left to the mention once it delivered, and published to when it failed",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker, spec string) []recipientCheck {
				delivered := seedThreadComment(t, database, "T-1", "", sami, nil)
				seedMentionDelivery(t, database, delivered, editor.ID, "sent")
				failed := seedThreadComment(t, database, "T-1", "", sami, nil)
				seedMentionDelivery(t, database, failed, editor.ID, "failed")
				onDelivered := appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: delivered}, editor.ID)
				onFailed := appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: failed}, editor.ID)
				return []recipientCheck{
					{event: onDelivered, want: []string{specWriter.ID}},
					{event: onFailed, want: specSessions},
				}
			},
		},
		{
			name: "in one batch, a comment on each of two issues reaches that issue's spec's sessions",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker, spec string) []recipientCheck {
				seedSecondIssue(t, database, "T-2")
				seedSpec(t, database, "T-2", model.Actor{Kind: "session", ID: "session-other"})
				first := seedThreadComment(t, database, "T-1", "", sami, nil)
				second := seedThreadComment(t, database, "T-2", "", sami, nil)
				onFirst := appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: first})
				onSecond := appendIssueCommentEvent(t, database, broker, "T-2", "comment.created", sami, model.Comment{ID: second})
				return []recipientCheck{
					{event: onFirst, want: specSessions},
					{event: onSecond, want: []string{"session-other"}},
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := storetest.Open(t)
			broker := events.NewBroker()
			seedIssue(t, database, "T-1", tc.route)
			spec := seedSpec(t, database, "T-1", specWriter,
				[]model.Actor{specWriter}, []model.Actor{alice, {Kind: "session"}}, []model.Actor{editor, specWriter})
			checkSessionRecipients(t, database, tc.arrange(t, database, broker, spec))
		})
	}
}

// fakeRegistry is Envoy's registry as a test sets it (Deps.Registry): each session's answer to a
// read, and how often each session was read. A session it holds no answer for is one the registry
// holds no record of, which envoy.Client.Session answers envoy.ErrNotFound.
type fakeRegistry struct {
	answers map[string]registryAnswer
	mu      sync.Mutex
	reads   map[string]int
}

// registryAnswer is one session's answer: its record's state, or err.
type registryAnswer struct {
	state string
	err   error
}

func newFakeRegistry(answers map[string]registryAnswer) *fakeRegistry {
	return &fakeRegistry{answers: answers, reads: map[string]int{}}
}

func (r *fakeRegistry) Session(_ context.Context, sessionID string) (envoy.Session, error) {
	r.mu.Lock()
	r.reads[sessionID]++
	r.mu.Unlock()
	answer, ok := r.answers[sessionID]
	if !ok {
		return envoy.Session{}, envoy.ErrNotFound
	}
	if answer.err != nil {
		return envoy.Session{}, answer.err
	}
	return envoy.Session{SessionID: sessionID, State: answer.state}, nil
}

func (r *fakeRegistry) readsOf(sessionID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reads[sessionID]
}

// threadRuleIssue is the issue the thread rule's tests share: T-1, which no route and no session
// claims, whose primary document session A created and session C edited, with a second document
// of the issue that session D created. Session B writes in each thread a person replies in.
type threadRuleIssue struct {
	person, a, b, c, d model.Actor
	notes              string
}

func seedThreadRuleIssue(t *testing.T, database *store.Store) threadRuleIssue {
	t.Helper()
	issue := threadRuleIssue{
		person: model.Actor{Kind: "user", ID: "sami"},
		a:      model.Actor{Kind: "session", ID: "session-a"},
		b:      model.Actor{Kind: "session", ID: "session-b"},
		c:      model.Actor{Kind: "session", ID: "session-c"},
		d:      model.Actor{Kind: "session", ID: "session-d"},
	}
	seedIssue(t, database, "T-1", nil)
	seedSpec(t, database, "T-1", issue.a, []model.Actor{issue.a}, []model.Actor{issue.c})
	issue.notes = seedDocument(t, database, new("T-1"), "notes", issue.d, []model.Actor{issue.d})
	return issue
}

// replyAndEdit appends the person's reply as the API writes it, and then its edit.
func (issue threadRuleIssue) replyAndEdit(t *testing.T, database *store.Store, broker *events.Broker, reply model.Comment) (model.Event, model.Event) {
	t.Helper()
	created := appendCommentEvent(t, database, broker, "comment.created", issue.person, reply)
	reply.Body = "Edited."
	edited := appendCommentEvent(t, database, broker, "comment.edited", issue.person, reply)
	return created, edited
}

// inIssueThread is the person's reply, and its edit, in a comment thread on the issue whose root
// B wrote.
func (issue threadRuleIssue) inIssueThread(t *testing.T, database *store.Store, broker *events.Broker) (model.Event, model.Event) {
	t.Helper()
	root := seedThreadComment(t, database, "T-1", "", issue.b, nil)
	reply := seedThreadComment(t, database, "T-1", "", issue.person, &root)
	return issue.replyAndEdit(t, database, broker, model.Comment{ID: reply, ReplyTo: &root})
}

// inNotesThread is the person's reply, and its edit, in a comment thread anchored on D's
// document, where a person wrote the root and B replied.
func (issue threadRuleIssue) inNotesThread(t *testing.T, database *store.Store, broker *events.Broker) (model.Event, model.Event) {
	t.Helper()
	root := seedThreadComment(t, database, "T-1", issue.notes, model.Actor{Kind: "user", ID: "alice"}, nil)
	seedThreadComment(t, database, "T-1", "", issue.b, &root)
	reply := seedThreadComment(t, database, "T-1", "", issue.person, &root)
	return issue.replyAndEdit(t, database, broker, model.Comment{ID: reply, ReplyTo: &root})
}

// inAskThread is the person's comment, and its edit, in the thread of an ask B asked, which B
// follows as its asker unless unfollow says it stopped.
func (issue threadRuleIssue) inAskThread(t *testing.T, database *store.Store, broker *events.Broker, unfollow bool) (model.Event, model.Event) {
	t.Helper()
	askID := seedAsk(t, database, "T-1", issue.b, "Ship it?")
	if unfollow {
		if _, err := asks.Unfollow(context.Background(), database.Pool, askID, issue.b.ID); err != nil {
			t.Fatalf("unfollow ask %s for %s: %v", askID, issue.b.ID, err)
		}
	}
	return issue.replyAndEdit(t, database, broker, model.Comment{ID: "8f14e45f-ceea-467a-9c1e-1b4d9a3f1c2b", AskID: &askID})
}

// inEveryThread appends the person's reply and its edit in each of the three threads, and checks
// each against the sessions it must reach: inIssueThread's and inAskThread's against onIssue,
// inNotesThread's against onNotes.
func (issue threadRuleIssue) inEveryThread(t *testing.T, database *store.Store, broker *events.Broker, onIssue, onNotes []string) []recipientCheck {
	t.Helper()
	var checks []recipientCheck
	for _, thread := range []struct {
		events func() (model.Event, model.Event)
		want   []string
	}{
		{func() (model.Event, model.Event) { return issue.inIssueThread(t, database, broker) }, onIssue},
		{func() (model.Event, model.Event) { return issue.inNotesThread(t, database, broker) }, onNotes},
		{func() (model.Event, model.Event) { return issue.inAskThread(t, database, broker, false) }, onIssue},
	} {
		created, edited := thread.events()
		checks = append(checks, recipientCheck{event: created, want: thread.want}, recipientCheck{event: edited, want: thread.want})
	}
	return checks
}

// A person's reply in a thread whose session B is live, or unresponsive, answers the thread: the
// reply and its edit reach B and none of the sessions that only a document credits - A and C,
// which the issue's primary document credits, and D, which the document the thread is anchored on
// credits - in a thread on the issue, in a thread on D's document and in the thread of an ask B
// asked and follows (takesThreadRule, threadHearing.hear, creditFilter). The batch reads B once
// for all six events, and reads no session that only a document credits.
func TestAPersonsReplyInAThreadOfALiveOrUnresponsiveSessionReachesTheThreadAndNotTheCredits(t *testing.T) {
	for _, state := range []string{envoy.StateLive, envoy.StateUnresponsive} {
		t.Run("B "+state, func(t *testing.T) {
			database := storetest.Open(t)
			broker := events.NewBroker()
			issue := seedThreadRuleIssue(t, database)
			registry := newFakeRegistry(map[string]registryAnswer{issue.b.ID: {state: state}})
			onlyB := []string{issue.b.ID}
			checkSessionRecipientsReading(t, database, registry, issue.inEveryThread(t, database, broker, onlyB, onlyB))
			if reads := registry.readsOf(issue.b.ID); reads != 1 {
				t.Fatalf("the batch read %s %d times, want once for every thread it is in", issue.b.ID, reads)
			}
			for _, credited := range []model.Actor{issue.a, issue.c, issue.d} {
				if reads := registry.readsOf(credited.ID); reads != 0 {
					t.Fatalf("the batch read %s, which only a document credits, %d times", credited.ID, reads)
				}
			}
		})
	}
}

// A person's reply in a thread none of whose sessions counts - B gone, stopped, or unknown to the
// registry - reaches whom it reaches without the thread rule: B, A and C in the thread on the issue
// and in the ask's thread, and B, A, C and D in the thread on D's document. The primary
// document's sessions come first (addPrimaryDocumentAuthors), then the thread's and its document's
// (addAuthorRoutes), then the ask's followers (addFollowerRoutes).
func TestAPersonsReplyInAThreadWithNoLiveSessionReachesTheCredits(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer *registryAnswer
	}{
		{name: "B gone", answer: &registryAnswer{state: envoy.StateGone}},
		// envoy.Client.Session answers a session that stopped as one it holds no record of: only
		// SessionIncludingStopped names it stopped.
		{name: "B stopped", answer: &registryAnswer{err: envoy.ErrNotFound}},
		{name: "B unknown to the registry"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := storetest.Open(t)
			broker := events.NewBroker()
			issue := seedThreadRuleIssue(t, database)
			answers := map[string]registryAnswer{}
			if tc.answer != nil {
				answers[issue.b.ID] = *tc.answer
			}
			registry := newFakeRegistry(answers)
			checkSessionRecipientsReading(t, database, registry, issue.inEveryThread(t, database, broker,
				[]string{issue.a.ID, issue.c.ID, issue.b.ID},
				[]string{issue.a.ID, issue.c.ID, issue.b.ID, issue.d.ID}))
			if reads := registry.readsOf(issue.b.ID); reads != 1 {
				t.Fatalf("the batch read %s %d times, want once", issue.b.ID, reads)
			}
		})
	}
}

// Where Envoy's registry does not answer, a person's reply reaches the credits of its documents as
// it does without the thread rule, whatever B's state: a listener that answers 503, a read the
// client's own window ends, and an outbox with no registry client at all. The first two read
// through envoy.Client, so its own timeout is what bounds the batch's wait.
func TestAFailedRegistryReadSendsTheCreditCopy(t *testing.T) {
	for _, tc := range []struct {
		name     string
		registry func(t *testing.T) RegistryReader
	}{
		{
			name: "the listener answers 503",
			registry: func(t *testing.T) RegistryReader {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					http.Error(w, "starting", http.StatusServiceUnavailable)
				}))
				t.Cleanup(server.Close)
				return envoy.New(server.URL)
			},
		},
		{
			name: "the read times out",
			registry: func(t *testing.T) RegistryReader {
				server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
					<-r.Context().Done()
				}))
				t.Cleanup(server.Close)
				return envoy.New(server.URL, envoy.WithTimeout(200*time.Millisecond))
			},
		},
		{
			name:     "no registry client",
			registry: func(*testing.T) RegistryReader { return nil },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := storetest.Open(t)
			broker := events.NewBroker()
			issue := seedThreadRuleIssue(t, database)
			checkSessionRecipientsReading(t, database, tc.registry(t), issue.inEveryThread(t, database, broker,
				[]string{issue.a.ID, issue.c.ID, issue.b.ID},
				[]string{issue.a.ID, issue.c.ID, issue.b.ID, issue.d.ID}))
		})
	}
}

// hungRegistry is Envoy's registry when its listener takes a read and never answers it: every
// read waits until the test lets it go, or until its own context ends, and then fails as the
// client's timeout fails it. started is closed at the first read.
type hungRegistry struct {
	started chan struct{}
	once    sync.Once
	release chan struct{}
}

func (r *hungRegistry) Session(ctx context.Context, sessionID string) (envoy.Session, error) {
	r.once.Do(func() { close(r.started) })
	select {
	case <-r.release:
	case <-ctx.Done():
	}
	return envoy.Session{}, fmt.Errorf("read session %s: %w", sessionID, context.DeadlineExceeded)
}

// A registry that does not answer costs the person's reply whose thread it is asked about its
// wait, and holds no other event of the batch: the issue.released of T-2, in the same batch as the
// reply and its edit in a thread on T-1 whose session B the batch reads, is published to the
// session that lost the claim and recorded while the read of B is still open, and the reply waits.
// Once the read fails, as the client's timeout fails it, the reply and its edit reach the credits
// of their documents, as they do where the registry does not answer
// (TestAFailedRegistryReadSendsTheCreditCopy).
func TestAHungRegistryHoldsNoIssueReleasedOfAnotherIssue(t *testing.T) {
	database := storetest.Open(t)
	broker := events.NewBroker()
	issue := seedThreadRuleIssue(t, database)
	seedSecondIssue(t, database, "T-2")
	created, edited := issue.inIssueThread(t, database, broker)
	released := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-2"), Type: "issue.released", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: map[string]any{
			"reason": "released",
			"previous_claim": map[string]any{
				"actor": map[string]any{"kind": "session", "id": "session-one"},
				"at":    "2026-11-18T05:00:00Z",
			},
		},
	})
	registry := &hungRegistry{started: make(chan struct{}), release: make(chan struct{})}
	letGo := sync.OnceFunc(func() { close(registry.release) })
	publisher := &recordingPublisher{}
	deps := Deps{Store: database, Publisher: publisher, Broker: events.NewBroker(), Registry: registry}
	holder := holding(t, deps).instance
	scanned := make(chan struct{})
	var scanErr error
	go func() {
		defer close(scanned)
		_, scanErr = scanBatch(context.Background(), deps, unfenced{}, holder)
	}()
	// However the test ends, the registry lets the read go and the batch ends before the
	// database it writes to is dropped.
	t.Cleanup(func() {
		letGo()
		select {
		case <-scanned:
		case <-time.After(10 * time.Second):
		}
	})

	select {
	case <-registry.started:
	case <-scanned:
		t.Fatalf("the batch ended (%v) before it read %s from the registry", scanErr, issue.b.ID)
	case <-time.After(10 * time.Second):
		t.Fatalf("the batch did not read %s from the registry", issue.b.ID)
	}
	deadline := time.Now().Add(10 * time.Second)
	for readEventRow(t, database, released.ID).publishedAt == nil {
		if time.Now().After(deadline) {
			t.Fatalf("issue.released of T-2 was not published while the registry read of %s was open", issue.b.ID)
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, item := range publisher.all() {
		if item.SourceEventID == fmt.Sprint(created.ID) || item.SourceEventID == fmt.Sprint(edited.ID) {
			t.Fatalf("event %s went to %s before the registry answered for %s", item.SourceEventID, item.Topic, issue.b.ID)
		}
	}

	letGo()
	select {
	case <-scanned:
		if scanErr != nil {
			t.Fatalf("scan batch: %v", scanErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the batch did not end once the registry read failed")
	}
	credits := []string{issue.a.ID, issue.c.ID, issue.b.ID}
	checkSentRecipients(t, database, publisher, []recipientCheck{
		{event: created, want: credits},
		{event: edited, want: credits},
		{event: released, want: []string{"session-one"}},
	})
}

// The thread rule takes copies away and adds none. B asked an ask and stopped following it, so no
// rule reaches B, and B does not count as the ask thread's session although the registry reads it
// live: the person's comment in that thread, and its edit, reach A and C, as they do without the
// rule, and not B. The registry is not asked about B at all.
func TestAnAskerThatStoppedFollowingDoesNotCount(t *testing.T) {
	database := storetest.Open(t)
	broker := events.NewBroker()
	issue := seedThreadRuleIssue(t, database)
	registry := newFakeRegistry(map[string]registryAnswer{issue.b.ID: {state: envoy.StateLive}})
	created, edited := issue.inAskThread(t, database, broker, true)
	credits := []string{issue.a.ID, issue.c.ID}
	checkSessionRecipientsReading(t, database, registry, []recipientCheck{
		{event: created, want: credits},
		{event: edited, want: credits},
	})
	if reads := registry.readsOf(issue.b.ID); reads != 0 {
		t.Fatalf("the batch read %s, which no rule reaches, %d times", issue.b.ID, reads)
	}
}

// A person's reply in the thread on the issue, whose session B is live, names C, which the
// primary document credits, and C's mention fails. C still gets the copy that the author route
// sends, under the key of the failed mention, so a host the send reached before it failed drops
// it (dispatch://INFRA-1368's R2; creditFilter, addPrimaryDocumentAuthors). A, which only the
// document credits, does not, and B does. The person's delete of the reply follows the reply
// (dispatch://INFRA-3686's R6): it reads the reply's mentions from the store, as an edit does
// (newCreditFilter, mentionDeliveries.editCreditCopyOf), so C and B get it and A does not, and
// C's copy of it carries the delete's own key, since no mention sent the delete.
func TestAReplyThatNamesACreditedSessionWhoseMentionFailedStillReachesIt(t *testing.T) {
	database := storetest.Open(t)
	broker := events.NewBroker()
	issue := seedThreadRuleIssue(t, database)
	registry := newFakeRegistry(map[string]registryAnswer{issue.b.ID: {state: envoy.StateLive}})
	root := seedThreadComment(t, database, "T-1", "", issue.b, nil)
	reply := seedThreadComment(t, database, "T-1", "", issue.person, &root)
	seedMentionDelivery(t, database, reply, issue.c.ID, "failed")
	created := appendCommentEvent(t, database, broker, "comment.created", issue.person, model.Comment{ID: reply, ReplyTo: &root}, issue.c.ID)
	deleted := appendCommentEvent(t, database, broker, "comment.deleted", issue.person, model.Comment{ID: reply, ReplyTo: &root})
	items := checkSessionRecipientsReading(t, database, registry, []recipientCheck{
		{event: created, want: []string{issue.c.ID, issue.b.ID}},
		{event: deleted, want: []string{issue.c.ID, issue.b.ID}},
	})
	wantKeys := map[string]string{
		fmt.Sprint(created.ID): contracts.AgentSendDedupeKey(issue.c.ID, model.CommentMentionIdempotencyKey(reply, "session:"+issue.c.ID, "steer")),
		fmt.Sprint(deleted.ID): fmt.Sprintf("dispatch-%d", deleted.ID),
	}
	for _, item := range items {
		if item.Topic == contracts.LegacyAgentSubject(issue.c.ID) && item.DedupeKey != wantKeys[item.SourceEventID] {
			t.Fatalf("the copy of event %s to %s is keyed %q, want %q", item.SourceEventID, issue.c.ID, item.DedupeKey, wantKeys[item.SourceEventID])
		}
	}
}

// A person's delete of a reply withdraws its text: the delete fails each attempt of the reply's
// mentions still pending, appending each one's receipt, and settles every fallback copy of the
// reply not yet settled with nothing published (deleteComment in internal/dispatch/api). The
// person's reply in B's thread (B live) names @role:R, whose claim the outbox cannot read, and its
// sender holds the attempt past unnamedForwardBound, so A's copy of the reply is a credit's row
// (creditHeldAsARow). Where the sender has pinned the attempt to A when the person deletes the
// reply, the receipt of that attempt, which the delete failed, can reopen A's row
// (reopenOnReceipt), and the withdrawal check refuses its publish (fallbackCopyWithdrawn): A's
// row ends settled with nothing published, and A never gets the withdrawn text. Where the reply
// stands, a person's failed Retry pinned to A still reopens the row the pass settled with nothing
// sent once the first attempt failed with no session, and the row gives A the reply
// (reopenOnReceipt).
func TestADeletesFailedAttemptPublishesNoCreditRowWhileAFailedRetryStillDoes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		steps func(w *replyOrder)
		// published is whether A's row publishes the reply, which then reaches A once.
		published bool
	}{
		{
			name:      "a failed Retry of the standing reply",
			steps:     func(w *replyOrder) { w.do("read"); w.do("fail"); w.do("read"); w.do("retry A") },
			published: true,
		},
		{
			name:  "the delete of the reply while its attempt pinned to A is being sent",
			steps: func(w *replyOrder) { w.do("read"); w.do("pin A"); deleteReplyAsTheAPIDoes(w) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := creditRowReply(t)
			tc.steps(w)
			w.settle()
			wantCopies := 0
			if tc.published {
				wantCopies = 1
			}
			row := readHeldCopy(t, w.database, w.reply, contracts.LegacyAgentSubject(w.issue.a.ID))
			if got := copiesOf(w.publisher.all(), w.created, w.issue.a.ID); len(got) != wantCopies || !row.settled || row.published != tc.published {
				t.Fatalf("A got %d copies of the reply, and its credit row is settled %v and published %v; want %d copies, the row settled and published %v",
					len(got), row.settled, row.published, wantCopies, tc.published)
			}
		})
	}
}

// creditRowReply is the world of TestADeletesFailedAttemptPublishesNoCreditRowWhileAFailedRetryStillDoes:
// the person's reply in B's thread on T-1, B live, names @role:R, whose claim the outbox cannot
// read, and its one attempt names no session yet and is held by a live sender past
// unnamedForwardBound, so the first read holds A's and C's copies of the reply as credits' rows.
func creditRowReply(t *testing.T) *replyOrder {
	t.Helper()
	database := storetest.Open(t)
	issue := seedThreadRuleIssue(t, database)
	w := &replyOrder{
		t: t, ctx: context.Background(), database: database, broker: events.NewBroker(), issue: issue,
		publisher: &recordingPublisher{},
		sessions:  map[string]string{"A": issue.a.ID},
		obliged:   map[string]bool{},
	}
	w.root = seedThreadComment(t, database, "T-1", "", issue.b, nil)
	w.reply = seedThreadComment(t, database, "T-1", "", issue.person, &w.root)
	seedDelivery(t, database, w.reply, replyOrderTarget, "", "pending")
	agedUnnamedAttempts(t, database, w.reply, unnamedForwardBound+30*time.Second)
	w.created = appendEvent(t, database, w.broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: issue.person,
		Payload: model.CommentEventPayload{Comment: model.Comment{
			ID: w.reply, IssueKey: new("T-1"), Author: issue.person, ReplyTo: &w.root,
			Mentions: []model.Mention{{Target: replyOrderTarget, Delivery: "steer"}},
		}},
	})
	w.deps = Deps{
		Store: database, Publisher: w.publisher, Broker: events.NewBroker(),
		Registry:   newFakeRegistry(map[string]registryAnswer{issue.b.ID: {state: envoy.StateLive}}),
		RoleClaims: unreadableRoleClaims{}, Sessions: heldRoles{},
	}
	w.instance = holding(t, w.deps).instance
	return w
}

// deleteReplyAsTheAPIDoes deletes w's reply as deleteComment in internal/dispatch/api writes the
// delete: every attempt of its mentions still pending fails, with its comment.delivery receipt,
// every fallback copy of the reply not yet settled is settled with nothing published, the reply's
// body is cleared and marked deleted, and its comment.deleted event follows.
func deleteReplyAsTheAPIDoes(w *replyOrder) {
	t := w.t
	rows, err := w.database.Pool.Query(w.ctx, `
		update comment_deliveries set state = 'failed', error = $2, claimed_at = now()
		where comment_id = $1 and state = 'pending'
		returning target, attempt, session_id
	`, w.reply, contracts.DeletedPostDeliveryError)
	if err != nil {
		t.Fatalf("fail the reply's pending attempts: %v", err)
	}
	var failed []model.CommentDeliveryEventPayload
	for rows.Next() {
		receipt := model.CommentDeliveryEventPayload{CommentID: w.reply, Delivery: "steer", State: "failed", Error: contracts.DeletedPostDeliveryError}
		if err := rows.Scan(&receipt.Target, &receipt.Attempt, &receipt.SessionID); err != nil {
			t.Fatalf("read a failed attempt of the reply: %v", err)
		}
		failed = append(failed, receipt)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("fail the reply's pending attempts: %v", err)
	}
	for _, receipt := range failed {
		appendEvent(t, w.database, w.broker, model.Event{IssueKey: new("T-1"), Type: "comment.delivery", Actor: w.issue.person, Payload: receipt})
	}
	if _, err := w.database.Pool.Exec(w.ctx, `
		update comment_fallback_copies set settled_at = now(), published = false
		where comment_id = $1 and settled_at is null
	`, w.reply); err != nil {
		t.Fatalf("settle the reply's fallback copies: %v", err)
	}
	deletedBy, err := json.Marshal(w.issue.person)
	if err != nil {
		t.Fatalf("encode the delete's actor: %v", err)
	}
	var deletedAt time.Time
	if err := w.database.Pool.QueryRow(w.ctx, `
		update comments set body = '', deleted_at = now(), deleted_by = $2 where id = $1 returning deleted_at
	`, w.reply, deletedBy).Scan(&deletedAt); err != nil {
		t.Fatalf("delete the reply: %v", err)
	}
	appendEvent(t, w.database, w.broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.deleted", Actor: w.issue.person,
		Payload: model.CommentEventPayload{Comment: model.Comment{
			ID: w.reply, IssueKey: new("T-1"), Author: w.issue.person, ReplyTo: &w.root,
			DeletedAt: &deletedAt, DeletedBy: &w.issue.person,
		}},
	})
}

// breakDocumentCredits makes every version of the document artifactID names list its authors as a
// JSON string, which documentCreditsSQL's jsonb_array_elements refuses: every recipient query that
// reads the document's credits then fails, as a query failing in the store does.
func breakDocumentCredits(t *testing.T, database *store.Store, artifactID string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(),
		`update artifact_versions set authors = '"not a list"'::jsonb where artifact_id = $1`, artifactID); err != nil {
		t.Fatalf("break the credits of document %s: %v", artifactID, err)
	}
}

// eventRow is what a batch left on an event's row.
type eventRow struct {
	destinations []string
	attempts     int
	nextAttempt  *time.Time
	publishedAt  *time.Time
}

func readEventRow(t *testing.T, database *store.Store, eventID int64) eventRow {
	t.Helper()
	var row eventRow
	if err := database.Pool.QueryRow(context.Background(), `
		select published_destinations, attempt_count, next_attempt_at, published_at from events where id = $1
	`, eventID).Scan(&row.destinations, &row.attempts, &row.nextAttempt, &row.publishedAt); err != nil {
		t.Fatalf("read event %d: %v", eventID, err)
	}
	return row
}

// appendIssueMessage appends a person's message on T-1, an event that takes no comment recipient.
func appendIssueMessage(t *testing.T, database *store.Store, broker *events.Broker) model.Event {
	t.Helper()
	return appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.MessageEventPayload{Message: model.Message{ID: "message-1", IssueKey: new("T-1"), Body: "Next"}},
	})
}

// A recipient query that fails fails the comment event that needed it at the step that needed it,
// as the lookup did when it ran per event: the event's owner topic, its route and its claimant are
// published and recorded first, so the issue's subject still gets the event in id order and the
// retry sends only the rest. The batch's other events publish as they would have.
func TestAFailedRecipientQueryBacksOffOnlyTheEventsThatNeededItAfterTheirOwnerTopic(t *testing.T) {
	sami := model.Actor{Kind: "user", ID: "sami"}
	alice := model.Actor{Kind: "user", ID: "alice"}
	writer := model.Actor{Kind: "session", ID: "session-writer"}
	commentTopic := "notifications.dispatch.issue.T-1.comment.created"
	messageTopic := "notifications.dispatch.issue.T-1.message.created"
	for _, tc := range []struct {
		name  string
		route *string
		// arrange appends the comment event whose recipient query fails.
		arrange      func(t *testing.T, database *store.Store, broker *events.Broker) model.Event
		wantComment  []string
		wantMessages []string
	}{
		{
			name: "the primary document's sessions, on an issue nobody routes or claims",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) model.Event {
				breakDocumentCredits(t, database, seedSpec(t, database, "T-1", writer, []model.Actor{writer}))
				comment := seedThreadComment(t, database, "T-1", "", sami, nil)
				return appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: comment})
			},
			wantComment:  []string{commentTopic},
			wantMessages: []string{messageTopic},
		},
		{
			name:  "a reply's thread sessions, on a routed claimed issue",
			route: new("role:reviewer"),
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) model.Event {
				claimIssue(t, database, "T-1", model.Actor{Kind: "session", ID: "session-claimant"})
				spec := seedDocument(t, database, new("T-1"), "spec", writer, []model.Actor{writer})
				breakDocumentCredits(t, database, spec)
				root := seedThreadComment(t, database, "T-1", spec, alice, nil)
				reply := seedThreadComment(t, database, "T-1", "", sami, &root)
				return appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: reply, ReplyTo: &root})
			},
			wantComment:  []string{commentTopic, roleTopic, contracts.LegacyAgentSubject("session-claimant")},
			wantMessages: []string{messageTopic, roleTopic, contracts.LegacyAgentSubject("session-claimant")},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := storetest.Open(t)
			broker := events.NewBroker()
			seedIssue(t, database, "T-1", tc.route)
			comment := tc.arrange(t, database, broker)
			message := appendIssueMessage(t, database, broker)

			deps := withHeldRoles(Deps{Store: database, Publisher: &recordingPublisher{}})
			if _, err := scanBatch(context.Background(), deps, unfenced{}, holding(t, deps).instance); err != nil {
				t.Fatalf("scan batch: %v", err)
			}
			got := readEventRow(t, database, comment.ID)
			if got.publishedAt != nil || got.attempts != 1 || got.nextAttempt == nil {
				t.Fatalf("the comment whose recipient query failed has published_at %v, attempt_count %d and next_attempt_at %v, want it backed off: none, 1 and a retry",
					got.publishedAt, got.attempts, got.nextAttempt)
			}
			if !slices.Equal(got.destinations, tc.wantComment) {
				t.Fatalf("the comment whose recipient query failed recorded published_destinations %v, want %v", got.destinations, tc.wantComment)
			}
			other := readEventRow(t, database, message.ID)
			if other.publishedAt == nil || other.attempts != 0 || !slices.Equal(other.destinations, tc.wantMessages) {
				t.Fatalf("the batch's message has published_at %v, attempt_count %d and published_destinations %v, want it published to %v",
					other.publishedAt, other.attempts, other.destinations, tc.wantMessages)
			}
		})
	}
}

// A reply whose reply_to is not a uuid replies under no comment, so its thread reaches no session:
// the reply is published to its owner topic, its route and its claimant, with no attempt counted,
// and holds nothing back.
func TestAReplyToAnIDThatIsNoUUIDReachesNoThreadSessionAndPublishes(t *testing.T) {
	sami := model.Actor{Kind: "user", ID: "sami"}
	claimant := model.Actor{Kind: "session", ID: "session-claimant"}
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", new("role:reviewer"))
	claimIssue(t, database, "T-1", claimant)
	comment := seedThreadComment(t, database, "T-1", "", sami, nil)
	replied := appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: comment, ReplyTo: new("not-a-uuid")})
	message := appendIssueMessage(t, database, broker)

	deps := withHeldRoles(Deps{Store: database, Publisher: &recordingPublisher{}})
	if _, err := scanBatch(context.Background(), deps, unfenced{}, holding(t, deps).instance); err != nil {
		t.Fatalf("scan batch: %v", err)
	}
	got := readEventRow(t, database, replied.ID)
	want := []string{"notifications.dispatch.issue.T-1.comment.created", roleTopic, contracts.LegacyAgentSubject(claimant.ID)}
	if got.publishedAt == nil || got.attempts != 0 || got.nextAttempt != nil || !slices.Equal(got.destinations, want) {
		t.Fatalf("the reply to a non-uuid id has published_at %v, attempt_count %d, next_attempt_at %v and published_destinations %v, want it published to %v with no attempt counted",
			got.publishedAt, got.attempts, got.nextAttempt, got.destinations, want)
	}
	if other := readEventRow(t, database, message.ID); other.publishedAt == nil || other.attempts != 0 {
		t.Fatalf("the issue's later message has published_at %v and attempt_count %d, want it published", other.publishedAt, other.attempts)
	}
}

// repairDocumentCredits gives every version of the document artifactID names an empty credit list,
// which every recipient query reads again: the document then credits its creator alone.
func repairDocumentCredits(t *testing.T, database *store.Store, artifactID string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(),
		`update artifact_versions set authors = '[]'::jsonb where artifact_id = $1`, artifactID); err != nil {
		t.Fatalf("repair the credits of document %s: %v", artifactID, err)
	}
}

// makeDue makes each event's back-off fall due now, as the passing of time would.
func makeDue(t *testing.T, database *store.Store, events ...model.Event) {
	t.Helper()
	ids := make([]int64, 0, len(events))
	for _, event := range events {
		ids = append(ids, event.ID)
	}
	if _, err := database.Pool.Exec(context.Background(),
		`update events set next_attempt_at = now() where id = any($1) and next_attempt_at is not null`, ids); err != nil {
		t.Fatalf("make events %v due: %v", ids, err)
	}
}

// An event whose recipient read failed lists its owner topic, route and claimant and nothing
// after, so the sessions it is still bound for are unknown. A later event of its owner that is
// bound for such a session - the thread root's author, told of the thread's resolution - is held
// back from it until the failed event is tried again, and reaches it after the failed event does;
// the destinations the failed event did list, the claimant here, chain as usual. The cases are
// the three recipient reads: a reply's thread sessions, the sessions of the primary document of an
// issue nobody routes or claims, and the sessions of the document a comment is anchored on. That
// last read still runs, and a failure of it still backs its event off, where the credits it
// answers reach nobody: on an issue a session claims they are no second reason to deliver
// (addAuthorRoutes, creditOnly), and the hold is the same - a read that failed names no session,
// so the later event waits for the retry whatever the repaired read would have said.
func TestALaterEventReachesNoSessionTheFailedRecipientReadOfAnEarlierOneMissedBeforeIt(t *testing.T) {
	sami := model.Actor{Kind: "user", ID: "sami"}
	rooter := model.Actor{Kind: "session", ID: "session-rooter"}
	claimant := model.Actor{Kind: "session", ID: "session-claimant"}
	writer := model.Actor{Kind: "session", ID: "session-writer"}
	for _, tc := range []struct {
		name string
		// arrange returns the event whose recipient read fails, the later event that reaches
		// rooter, and the document whose credits break and are then repaired.
		arrange func(t *testing.T, database *store.Store, broker *events.Broker) (failed, later model.Event, document string)
		// wantFirst is the later event's sessions in the first batch: those the failed event
		// listed.
		wantFirst []string
		// creditOnly is that the failed event's read names rooter as a credit of its document
		// alone, no comment of the thread being rooter's: the issue's session claim leaves such a
		// credit out, so the repaired read sends rooter the later event alone.
		creditOnly bool
	}{
		{
			name: "a reply's thread sessions",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) (model.Event, model.Event, string) {
				claimIssue(t, database, "T-1", claimant)
				spec := seedDocument(t, database, new("T-1"), "spec", writer, []model.Actor{writer})
				root := seedThreadComment(t, database, "T-1", spec, rooter, nil)
				reply := seedThreadComment(t, database, "T-1", "", sami, &root)
				replied := appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: reply, ReplyTo: &root})
				resolved := appendCommentEvent(t, database, broker, "comment.resolved", sami, model.Comment{ID: root, Anchor: &model.Anchor{ArtifactID: spec}})
				return replied, resolved, spec
			},
			wantFirst: []string{claimant.ID},
		},
		{
			name: "the primary document's sessions, on an issue nobody routes or claims",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) (model.Event, model.Event, string) {
				spec := seedSpec(t, database, "T-1", rooter, []model.Actor{rooter})
				comment := seedThreadComment(t, database, "T-1", "", sami, nil)
				root := seedThreadComment(t, database, "T-1", "", rooter, nil)
				commented := appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: comment})
				resolved := appendCommentEvent(t, database, broker, "comment.resolved", sami, model.Comment{ID: root})
				return commented, resolved, spec
			},
			wantFirst: []string{},
		},
		{
			name: "the sessions of the document a comment is anchored on",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) (model.Event, model.Event, string) {
				claimIssue(t, database, "T-1", claimant)
				notes := seedDocument(t, database, new("T-1"), "notes", rooter, []model.Actor{rooter})
				comment := seedThreadComment(t, database, "T-1", notes, sami, nil)
				root := seedThreadComment(t, database, "T-1", "", rooter, nil)
				commented := appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: comment, Anchor: &model.Anchor{ArtifactID: notes}})
				resolved := appendCommentEvent(t, database, broker, "comment.resolved", sami, model.Comment{ID: root})
				return commented, resolved, notes
			},
			wantFirst:  []string{claimant.ID},
			creditOnly: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := storetest.Open(t)
			broker := events.NewBroker()
			seedIssue(t, database, "T-1", nil)
			failed, later, document := tc.arrange(t, database, broker)
			breakDocumentCredits(t, database, document)

			publisher := &recordingPublisher{}
			deps := Deps{Store: database, Publisher: publisher}
			holder := holding(t, deps).instance
			scan := func(t *testing.T) {
				t.Helper()
				if _, err := scanBatch(context.Background(), deps, unfenced{}, holder); err != nil {
					t.Fatalf("scan batch: %v", err)
				}
			}
			scan(t)
			first, held := readEventRow(t, database, failed.ID), readEventRow(t, database, later.ID)
			if first.attempts != 1 || first.nextAttempt == nil {
				t.Fatalf("the event whose recipient read failed has attempt_count %d and next_attempt_at %v, want it backed off", first.attempts, first.nextAttempt)
			}
			if held.publishedAt != nil || held.attempts != 0 || held.nextAttempt == nil || held.nextAttempt.After(*first.nextAttempt) {
				t.Fatalf("the later event has published_at %v, attempt_count %d and next_attempt_at %v, want it held back no later than the failed event's retry at %v",
					held.publishedAt, held.attempts, held.nextAttempt, *first.nextAttempt)
			}
			want := []string{}
			for _, sessionID := range tc.wantFirst {
				want = append(want, contracts.LegacyAgentSubject(sessionID))
			}
			if got := agentTopics(held.destinations); !slices.Equal(got, want) {
				t.Fatalf("the later event reached sessions %v in the first batch, want %v, the ones the failed event listed", got, want)
			}

			repairDocumentCredits(t, database, document)
			makeDue(t, database, failed, later)
			scan(t)
			for _, event := range []model.Event{failed, later} {
				if row := readEventRow(t, database, event.ID); row.publishedAt == nil {
					t.Fatalf("%s event %d was not published once the credits were repaired: %+v", event.Type, event.ID, row)
				}
			}
			var order []string
			for _, item := range publisher.all() {
				if item.Topic == contracts.LegacyAgentSubject(rooter.ID) {
					order = append(order, item.SourceEventID)
				}
			}
			wantOrder := []string{fmt.Sprint(failed.ID), fmt.Sprint(later.ID)}
			if tc.creditOnly {
				wantOrder = []string{fmt.Sprint(later.ID)}
			}
			if !slices.Equal(order, wantOrder) {
				t.Fatalf("%s received events %v, want %v: the failed event first and then the later one, or the later one alone where the repaired read names %s as a credit of the document and the issue's claimant is a session",
					rooter.ID, order, wantOrder, rooter.ID)
			}
		})
	}
}

// A document whose credit list no read can take backs off the comment anchored on it, and holds
// back nothing but the later session copies of that comment's issue: the batch is recorded, the
// issue's later event reaches the claimant its failed event listed and waits for the failed
// event's retry for the thread root's author, and another issue's events in the same batch
// publish - among them a comment anchored on a healthy document, whose credits the batch reads in
// the same statement as the broken document's (loadRecipients runs that statement again in halves,
// here each id alone, once Postgres refuses it).
func TestABrokenCreditListBacksOffItsEventAndHoldsOnlyItsOwnersLaterSessionCopies(t *testing.T) {
	sami := model.Actor{Kind: "user", ID: "sami"}
	writer := model.Actor{Kind: "session", ID: "session-writer"}
	claimant := model.Actor{Kind: "session", ID: "session-claimant"}
	rooter := model.Actor{Kind: "session", ID: "session-rooter"}
	other := model.Actor{Kind: "session", ID: "session-other"}
	credited := model.Actor{Kind: "session", ID: "session-credited"}
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	seedSecondIssue(t, database, "T-2")
	claimIssue(t, database, "T-1", claimant)
	notes := seedDocument(t, database, new("T-1"), "notes", writer, []model.Actor{writer})
	breakDocumentCredits(t, database, notes)
	comment := seedThreadComment(t, database, "T-1", notes, sami, nil)
	root := seedThreadComment(t, database, "T-1", "", rooter, nil)
	otherRoot := seedThreadComment(t, database, "T-2", "", other, nil)
	otherReply := seedThreadComment(t, database, "T-2", "", sami, &otherRoot)
	healthy := seedDocument(t, database, new("T-2"), "plan", credited, []model.Actor{credited})
	healthyComment := seedThreadComment(t, database, "T-2", healthy, sami, nil)
	commented := appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: comment, Anchor: &model.Anchor{ArtifactID: notes}})
	resolved := appendCommentEvent(t, database, broker, "comment.resolved", sami, model.Comment{ID: root})
	otherReplied := appendIssueCommentEvent(t, database, broker, "T-2", "comment.created", sami, model.Comment{ID: otherReply, ReplyTo: &otherRoot})
	otherResolved := appendIssueCommentEvent(t, database, broker, "T-2", "comment.resolved", sami, model.Comment{ID: otherRoot})
	anchored := appendIssueCommentEvent(t, database, broker, "T-2", "comment.created", sami, model.Comment{ID: healthyComment, Anchor: &model.Anchor{ArtifactID: healthy}})

	publisher := &recordingPublisher{}
	deps := Deps{Store: database, Publisher: publisher}
	if _, err := scanBatch(context.Background(), deps, unfenced{}, holding(t, deps).instance); err != nil {
		t.Fatalf("scan batch: %v", err)
	}
	failed := readEventRow(t, database, commented.ID)
	if failed.publishedAt != nil || failed.attempts != 1 || failed.nextAttempt == nil {
		t.Fatalf("the comment on the broken document has published_at %v, attempt_count %d and next_attempt_at %v, want it backed off",
			failed.publishedAt, failed.attempts, failed.nextAttempt)
	}
	if want := []string{"notifications.dispatch.issue.T-1.comment.created", contracts.LegacyAgentSubject(claimant.ID)}; !slices.Equal(failed.destinations, want) {
		t.Fatalf("the comment on the broken document recorded published_destinations %v, want %v", failed.destinations, want)
	}
	held := readEventRow(t, database, resolved.ID)
	if want := []string{"notifications.dispatch.issue.T-1.comment.resolved", contracts.LegacyAgentSubject(claimant.ID)}; !slices.Equal(held.destinations, want) {
		t.Fatalf("the issue's later event recorded published_destinations %v, want %v: its owner topic and the claimant the failed event listed", held.destinations, want)
	}
	if held.publishedAt != nil || held.attempts != 0 || held.nextAttempt == nil || held.nextAttempt.After(*failed.nextAttempt) {
		t.Fatalf("the issue's later event has published_at %v, attempt_count %d and next_attempt_at %v, want it held back no later than the failed event's retry at %v",
			held.publishedAt, held.attempts, held.nextAttempt, *failed.nextAttempt)
	}
	for _, item := range publisher.all() {
		if item.SourceEventID == fmt.Sprint(resolved.ID) && item.Topic == contracts.LegacyAgentSubject(rooter.ID) {
			t.Fatalf("the issue's later event reached %s, a session the failed event may yet reach first", rooter.ID)
		}
	}
	for _, event := range []model.Event{otherReplied, otherResolved} {
		row := readEventRow(t, database, event.ID)
		if row.publishedAt == nil || row.attempts != 0 || !slices.Equal(agentTopics(row.destinations), []string{contracts.LegacyAgentSubject(other.ID)}) {
			t.Fatalf("the other issue's %s event has published_at %v, attempt_count %d and published_destinations %v, want it published to %s",
				event.Type, row.publishedAt, row.attempts, row.destinations, other.ID)
		}
	}
	row := readEventRow(t, database, anchored.ID)
	if want := []string{"notifications.dispatch.issue.T-2.comment.created", contracts.LegacyAgentSubject(credited.ID)}; row.publishedAt == nil || row.attempts != 0 || !slices.Equal(row.destinations, want) {
		t.Fatalf("the other issue's comment on a healthy document has published_at %v, attempt_count %d and published_destinations %v, want it published to %v",
			row.publishedAt, row.attempts, row.destinations, want)
	}
}

// An event that is not due still reads its recipients, since they are what it holds its owner's
// later events back from (resolveDestinations). One backed off on a thread whose document's credit
// list no read can take puts that row in every batch's statement beside another issue's reply, and
// the batch runs the refused statement again in halves (loadRecipients): the other issue's reply
// publishes, and so does the broken reply's issue's message, bound for the claimant the broken
// reply names.
func TestABrokenRowOfAnEventThatIsNotDueFailsNoOtherOwnersRecipientRead(t *testing.T) {
	sami := model.Actor{Kind: "user", ID: "sami"}
	writer := model.Actor{Kind: "session", ID: "session-writer"}
	other := model.Actor{Kind: "session", ID: "session-other"}
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	seedSecondIssue(t, database, "T-2")
	claimIssue(t, database, "T-1", model.Actor{Kind: "session", ID: "session-claimant"})
	spec := seedDocument(t, database, new("T-1"), "spec", writer, []model.Actor{writer})
	breakDocumentCredits(t, database, spec)
	root := seedThreadComment(t, database, "T-1", spec, writer, nil)
	reply := seedThreadComment(t, database, "T-1", "", sami, &root)
	broken := appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: reply, ReplyTo: &root})

	deps := Deps{Store: database, Publisher: &recordingPublisher{}}
	holder := holding(t, deps).instance
	if _, err := scanBatch(context.Background(), deps, unfenced{}, holder); err != nil {
		t.Fatalf("scan the broken reply's batch: %v", err)
	}
	if row := readEventRow(t, database, broken.ID); row.attempts != 1 {
		t.Fatalf("the reply on the broken thread has attempt_count %d after its batch, want 1", row.attempts)
	}
	if _, err := database.Pool.Exec(context.Background(),
		`update events set next_attempt_at = now() + interval '1 hour' where id = $1`, broken.ID); err != nil {
		t.Fatalf("move the broken reply's back-off out: %v", err)
	}
	message := appendIssueMessage(t, database, broker)
	otherRoot := seedThreadComment(t, database, "T-2", "", other, nil)
	otherReply := seedThreadComment(t, database, "T-2", "", sami, &otherRoot)
	otherReplied := appendIssueCommentEvent(t, database, broker, "T-2", "comment.created", sami, model.Comment{ID: otherReply, ReplyTo: &otherRoot})

	if _, err := scanBatch(context.Background(), deps, unfenced{}, holder); err != nil {
		t.Fatalf("scan the batch beside the broken reply: %v", err)
	}
	row := readEventRow(t, database, otherReplied.ID)
	if row.publishedAt == nil || row.attempts != 0 || !slices.Equal(agentTopics(row.destinations), []string{contracts.LegacyAgentSubject(other.ID)}) {
		t.Fatalf("the other issue's reply has published_at %v, attempt_count %d and published_destinations %v, want it published to %s",
			row.publishedAt, row.attempts, row.destinations, other.ID)
	}
	if row := readEventRow(t, database, message.ID); row.publishedAt == nil {
		t.Fatalf("the broken reply's issue's message was not published: %+v", row)
	}
	if row := readEventRow(t, database, broken.ID); row.attempts != 1 || row.publishedAt != nil {
		t.Fatalf("the reply on the broken thread, not due, has attempt_count %d and published_at %v, want it left as it was", row.attempts, row.publishedAt)
	}
}

// waitOut makes event an earlier attempt's: not due for the next hour, one attempt counted, and
// reached already on each of reached.
func waitOut(t *testing.T, database *store.Store, event model.Event, reached ...string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		update events set next_attempt_at = now() + interval '1 hour', attempt_count = 1, published_destinations = $2
		where id = $1
	`, event.ID, reached); err != nil {
		t.Fatalf("back event %d off: %v", event.ID, err)
	}
}

// A due event behind an earlier event of its issue that is waiting out a back-off is held back
// only on the keys the earlier event could still owe. A session the earlier event already reached
// is no such key, even when the earlier event's recipient read fails now; a session its failed
// read may still name is, until the earlier event's retry; and an earlier event whose reads answer
// holds back only the key it stalled on - a session whose send failed, or its role route - so the
// later event reaches every other session at once. Each later event replies in a thread of its
// own, so no due event asks the earlier event's recipient read.
func TestAnEventBehindAnotherThatIsNotDueIsHeldOnlyOnTheKeysTheOtherCouldStillOwe(t *testing.T) {
	sami := model.Actor{Kind: "user", ID: "sami"}
	reached := model.Actor{Kind: "session", ID: "session-reached"}
	unreached := model.Actor{Kind: "session", ID: "session-unreached"}
	stalled := model.Actor{Kind: "session", ID: "session-stalled"}
	topic := "notifications.dispatch.issue.T-1.comment.created"
	// reply appends sami's reply in a new thread on the document on names, or on none, whose root
	// author writes and each of members then replies in, and returns its event.
	reply := func(t *testing.T, database *store.Store, broker *events.Broker, on string, author model.Actor, members ...model.Actor) model.Event {
		t.Helper()
		root := seedThreadComment(t, database, "T-1", on, author, nil)
		for _, member := range members {
			seedThreadComment(t, database, "T-1", "", member, &root)
		}
		comment := seedThreadComment(t, database, "T-1", "", sami, &root)
		return appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: comment, ReplyTo: &root})
	}
	for _, tc := range []struct {
		name  string
		route *string
		// arrange appends the earlier event, backed off, and the later one.
		arrange func(t *testing.T, database *store.Store, broker *events.Broker) (earlier, later model.Event)
		// want is every subject the later event reaches in the batch, in order; the rest of its
		// destinations wait for the earlier event's retry.
		want []string
	}{
		{
			// The earlier event reached the session in an attempt before its document's credit
			// list broke; the later one is bound for it and for a session the earlier one never
			// reached.
			name: "the earlier event's recipient read fails",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) (model.Event, model.Event) {
				spec := seedDocument(t, database, new("T-1"), "spec", reached, []model.Actor{reached})
				breakDocumentCredits(t, database, spec)
				earlier := reply(t, database, broker, spec, reached)
				waitOut(t, database, earlier, topic, contracts.LegacyAgentSubject(reached.ID))
				return earlier, reply(t, database, broker, "", reached, unreached)
			},
			want: []string{topic, contracts.LegacyAgentSubject(reached.ID)},
		},
		{
			name: "the earlier event's reads answer and its send to one session failed",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) (model.Event, model.Event) {
				earlier := reply(t, database, broker, "", reached, stalled)
				waitOut(t, database, earlier, topic, contracts.LegacyAgentSubject(reached.ID))
				return earlier, reply(t, database, broker, "", reached, unreached, stalled)
			},
			want: []string{topic, contracts.LegacyAgentSubject(reached.ID), contracts.LegacyAgentSubject(unreached.ID)},
		},
		{
			name:  "the earlier event's reads answer and its role route is pending",
			route: new("role:reviewer"),
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) (model.Event, model.Event) {
				earlier := reply(t, database, broker, "", reached)
				waitOut(t, database, earlier, topic, contracts.LegacyAgentSubject(reached.ID))
				return earlier, reply(t, database, broker, "", reached, unreached)
			},
			want: []string{topic, contracts.LegacyAgentSubject(reached.ID), contracts.LegacyAgentSubject(unreached.ID)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := storetest.Open(t)
			broker := events.NewBroker()
			seedIssue(t, database, "T-1", tc.route)
			earlier, later := tc.arrange(t, database, broker)
			before := readEventRow(t, database, earlier.ID)

			publisher := &recordingPublisher{}
			deps := withHeldRoles(Deps{Store: database, Publisher: publisher})
			if _, err := scanBatch(context.Background(), deps, unfenced{}, holding(t, deps).instance); err != nil {
				t.Fatalf("scan batch: %v", err)
			}
			var sent []string
			for _, item := range publisher.all() {
				if item.SourceEventID == fmt.Sprint(later.ID) {
					sent = append(sent, item.Topic)
				}
			}
			if !slices.Equal(sent, tc.want) {
				t.Fatalf("the later event was sent to %v, want %v", sent, tc.want)
			}
			got := readEventRow(t, database, later.ID)
			if !slices.Equal(got.destinations, tc.want) {
				t.Fatalf("the later event recorded published_destinations %v, want %v", got.destinations, tc.want)
			}
			if got.publishedAt != nil || got.attempts != 0 || got.nextAttempt == nil || !got.nextAttempt.Equal(*before.nextAttempt) {
				t.Fatalf("the later event has published_at %v, attempt_count %d and next_attempt_at %v, want it held back until the earlier event's retry at %v",
					got.publishedAt, got.attempts, got.nextAttempt, *before.nextAttempt)
			}
			if after := readEventRow(t, database, earlier.ID); !reflect.DeepEqual(after, before) {
				t.Fatalf("the earlier event, not due, has %+v after the batch, want it left as it was: %+v", after, before)
			}
		})
	}
}

// An event whose role route reached the role's holder of an attempt before is held back no more
// on that route: the role topic it recorded names the route reached for good, whoever holds the
// role now. When its recipient read fails now, the role's current holder is a session its failed
// read may still name - here the thread's document credits it - so a later event of its issue is
// held back from the current holder until the earlier event's retry, its role forward included,
// even though the earlier event's role route is resolved to that same holder in the batch.
func TestAnEarlierEventsReachedRoleRouteDoesNotNameTheRolesCurrentHolder(t *testing.T) {
	sami := model.Actor{Kind: "user", ID: "sami"}
	holder := model.Actor{Kind: "session", ID: roleHolder}
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", new("role:reviewer"))
	spec := seedDocument(t, database, new("T-1"), "spec", holder, []model.Actor{holder})
	breakDocumentCredits(t, database, spec)
	root := seedThreadComment(t, database, "T-1", spec, sami, nil)
	reply := seedThreadComment(t, database, "T-1", "", sami, &root)
	earlier := appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: reply, ReplyTo: &root})
	// The earlier event's role route reached the role's holder then, another session than
	// roleHolder, which holds the role now (withHeldRoles).
	waitOut(t, database, earlier, "notifications.dispatch.issue.T-1.comment.created", roleTopic)
	later := appendIssueMessage(t, database, broker)
	before := readEventRow(t, database, earlier.ID)

	publisher := &recordingPublisher{}
	deps := withHeldRoles(Deps{Store: database, Publisher: publisher})
	if _, err := scanBatch(context.Background(), deps, unfenced{}, holding(t, deps).instance); err != nil {
		t.Fatalf("scan batch: %v", err)
	}
	want := []string{"notifications.dispatch.issue.T-1.message.created"}
	var sent []string
	for _, item := range publisher.all() {
		if item.SourceEventID == fmt.Sprint(later.ID) {
			sent = append(sent, item.Topic)
		}
	}
	if !slices.Equal(sent, want) {
		t.Fatalf("the later event was sent to %v, want %v: its role forward to %s waits for the earlier event's retry", sent, want, roleHolder)
	}
	got := readEventRow(t, database, later.ID)
	if !slices.Equal(got.destinations, want) {
		t.Fatalf("the later event recorded published_destinations %v, want %v", got.destinations, want)
	}
	if got.publishedAt != nil || got.attempts != 0 || got.nextAttempt == nil || got.nextAttempt.After(*before.nextAttempt) {
		t.Fatalf("the later event has published_at %v, attempt_count %d and next_attempt_at %v, want it held back no later than the earlier event's retry at %v",
			got.publishedAt, got.attempts, got.nextAttempt, *before.nextAttempt)
	}
	if after := readEventRow(t, database, earlier.ID); !reflect.DeepEqual(after, before) {
		t.Fatalf("the earlier event, not due, has %+v after the batch, want it left as it was: %+v", after, before)
	}
}

// A process that stops mid-batch releases the batch's events (settle), and an event whose recipient
// query failed is no exception: it is neither backed off nor left claimed, whether the stop is what
// failed the query or the query failed in the store and the stop came while the batch's answers
// were still due.
func TestAStoppingBatchReleasesTheEventsWhoseRecipientQueryFailed(t *testing.T) {
	sami := model.Actor{Kind: "user", ID: "sami"}
	writer := model.Actor{Kind: "session", ID: "session-writer"}
	assertReleased := func(t *testing.T, database *store.Store, events ...model.Event) {
		t.Helper()
		for _, event := range events {
			if got := readEventRow(t, database, event.ID); got.publishedAt != nil || got.attempts != 0 || got.nextAttempt != nil {
				t.Errorf("%s event %d has published_at %v, attempt_count %d and next_attempt_at %v, want it released: none, 0 and none",
					event.Type, event.ID, got.publishedAt, got.attempts, got.nextAttempt)
			}
		}
	}

	t.Run("the stop cancels the recipient queries", func(t *testing.T) {
		database := storetest.Open(t)
		broker := events.NewBroker()
		seedIssue(t, database, "T-1", nil)
		seedSpec(t, database, "T-1", writer, []model.Actor{writer})
		root := seedThreadComment(t, database, "T-1", "", writer, nil)
		reply := seedThreadComment(t, database, "T-1", "", sami, &root)
		comment := seedThreadComment(t, database, "T-1", "", sami, nil)
		replied := appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: reply, ReplyTo: &root})
		commented := appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: comment})
		message := appendIssueMessage(t, database, broker)

		deps := Deps{Store: database, Publisher: &recordingPublisher{}}
		holder := holding(t, deps).instance
		pending, _, _, err := readBatch(context.Background(), deps)
		if err != nil {
			t.Fatalf("scan the batch: %v", err)
		}
		stopped, stop := context.WithCancel(context.Background())
		stop()
		if _, err := publishBatch(stopped, deps, unfenced{}, holder, pending, false); err != nil {
			t.Fatalf("publish the batch: %v", err)
		}
		assertReleased(t, database, replied, commented, message)
	})

	// Every destination the event lists before its recipient read is reached already, so the
	// batch has no send of it to lose: the read the stop cancelled is what releases it
	// (batchEvent.resolveLost), rather than backing it off.
	t.Run("the stop cancels the recipient query of an event with nothing else to send", func(t *testing.T) {
		database := storetest.Open(t)
		broker := events.NewBroker()
		seedIssue(t, database, "T-1", nil)
		seedSpec(t, database, "T-1", writer, []model.Actor{writer})
		root := seedThreadComment(t, database, "T-1", "", writer, nil)
		reply := seedThreadComment(t, database, "T-1", "", sami, &root)
		replied := appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: reply, ReplyTo: &root})
		if _, err := database.Pool.Exec(context.Background(),
			`update events set published_destinations = array['notifications.dispatch.issue.T-1.comment.created'] where id = $1`, replied.ID); err != nil {
			t.Fatalf("record the reply's owner topic reached: %v", err)
		}

		deps := Deps{Store: database, Publisher: &recordingPublisher{}}
		holder := holding(t, deps).instance
		pending, _, _, err := readBatch(context.Background(), deps)
		if err != nil {
			t.Fatalf("scan the batch: %v", err)
		}
		stopped, stop := context.WithCancel(context.Background())
		stop()
		if _, err := publishBatch(stopped, deps, unfenced{}, holder, pending, false); err != nil {
			t.Fatalf("publish the batch: %v", err)
		}
		assertReleased(t, database, replied)
	})

	t.Run("the query fails in the store and the stop comes before the answers", func(t *testing.T) {
		database := storetest.Open(t)
		broker := events.NewBroker()
		seedIssue(t, database, "T-1", nil)
		breakDocumentCredits(t, database, seedSpec(t, database, "T-1", writer, []model.Actor{writer}))
		comment := seedThreadComment(t, database, "T-1", "", sami, nil)
		commented := appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: comment})
		message := appendIssueMessage(t, database, broker)

		publisher := &heldPublisher{}
		stop := run(t, database, publisher, broker)
		// The batch's sends to the issue topic go one at a time (planChains), so the comment's,
		// which is held unanswered, is the one sent.
		waitFor(t, 5*time.Second, "the comment sent", func() bool {
			return slices.Contains(publisher.sent(), "notifications.dispatch.issue.T-1.comment.created")
		})
		stop()
		assertReleased(t, database, commented, message)
	})
}

// R6: a thread's sessions "count while Envoy's registry reads them as live or unresponsive. Where
// none of them counts, or the registry does not answer, the reply reaches the sessions that the
// documents credit" (dispatch://INFRA-3686/spec). One session X, the root author of a thread on
// T-1, has an interest record the listener cannot read: GET /v1/registry/session-x answers 500
// (writeInterestRecordUnreadable), while the registry answers every other read. The person replies
// in X's thread and, in the same batch, in the thread of session B, which the registry reads live.
// B's thread has a session that counts and a registry that answers for it, so the reply in it
// reaches B and none of A and C, whatever X's read answered.
func TestOneSessionsUnreadableRecordLeavesAnotherThreadsRuleAlone(t *testing.T) {
	database := storetest.Open(t)
	broker := events.NewBroker()
	issue := seedThreadRuleIssue(t, database)
	x := model.Actor{Kind: "session", ID: "session-x"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/v1/registry/")
		switch {
		case id == x.ID:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"envoy: the interest record of session session-x is unreadable: malformed interest"}`))
		case id == issue.b.ID:
			_, _ = w.Write([]byte(`{"session_id":"session-b","state":"live"}`))
		default:
			// The other writers of X's thread are gone from the registry; their reads answer a
			// little later than X's.
			time.Sleep(300 * time.Millisecond)
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no live session ` + id + `","state":"gone"}`))
		}
	}))
	t.Cleanup(server.Close)
	rootX := seedThreadComment(t, database, "T-1", "", x, nil)
	for i := 2; i <= 8; i++ {
		seedThreadComment(t, database, "T-1", "", model.Actor{Kind: "session", ID: fmt.Sprintf("session-gone-%d", i)}, &rootX)
	}
	replyX := seedThreadComment(t, database, "T-1", "", issue.person, &rootX)
	appendCommentEvent(t, database, broker, "comment.created", issue.person, model.Comment{ID: replyX, ReplyTo: &rootX})
	rootB := seedThreadComment(t, database, "T-1", "", issue.b, nil)
	replyB := seedThreadComment(t, database, "T-1", "", issue.person, &rootB)
	inB := appendCommentEvent(t, database, broker, "comment.created", issue.person, model.Comment{ID: replyB, ReplyTo: &rootB})
	checkSessionRecipientsReading(t, database, envoy.New(server.URL), []recipientCheck{
		{event: inB, want: []string{issue.b.ID}},
	})
}

// A registry whose answers stop after their headers - the body never comes, so each read ends at
// the client's timeout - is a registry that does not answer (registryUnanswered), and costs one
// read's wait: "A registry that hangs then costs one read's wait, which the reader's own timeout
// bounds" (countedSessions). The reply's thread holds 24 sessions, three rounds of
// registryReadsAtOnce, and the client's timeout is 700 ms; no read starts after the first round's,
// and the reply reaches the credits (A and C), as R6 says where the registry does not answer.
func TestAReadThatRunsOutOfTimeInItsBodyStopsFurtherReads(t *testing.T) {
	database := storetest.Open(t)
	broker := events.NewBroker()
	issue := seedThreadRuleIssue(t, database)
	var reads atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "200")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"session_id":`))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	root := seedThreadComment(t, database, "T-1", "", issue.b, nil)
	for i := range 23 {
		seedThreadComment(t, database, "T-1", "", model.Actor{Kind: "session", ID: fmt.Sprintf("session-stall-%02d", i)}, &root)
	}
	reply := seedThreadComment(t, database, "T-1", "", issue.person, &root)
	created := appendCommentEvent(t, database, broker, "comment.created", issue.person, model.Comment{ID: reply, ReplyTo: &root})
	publisher := &recordingPublisher{}
	deps := Deps{Store: database, Publisher: publisher, Broker: events.NewBroker(), Registry: envoy.New(server.URL, envoy.WithTimeout(700*time.Millisecond))}
	start := time.Now()
	if _, err := scanBatch(context.Background(), deps, unfenced{}, holding(t, deps).instance); err != nil {
		t.Fatalf("scan batch: %v", err)
	}
	took := time.Since(start)
	var toA, toC bool
	for _, item := range publisher.all() {
		if item.SourceEventID != fmt.Sprint(created.ID) {
			continue
		}
		toA = toA || item.Topic == contracts.LegacyAgentSubject(issue.a.ID)
		toC = toC || item.Topic == contracts.LegacyAgentSubject(issue.c.ID)
	}
	t.Logf("24 sessions, each read ending at the client's 700 ms timeout in its body: %d reads, batch %s; credit copy to A %v, C %v", reads.Load(), took.Round(time.Millisecond), toA, toC)
	if !toA || !toC {
		t.Fatalf("the reply reached A %v, C %v: want the credit copy where the registry does not answer", toA, toC)
	}
	if n := reads.Load(); n > registryReadsAtOnce {
		t.Fatalf("%d reads were made after reads that never got their answer: want at most %d, one read's wait", n, registryReadsAtOnce)
	}
}
