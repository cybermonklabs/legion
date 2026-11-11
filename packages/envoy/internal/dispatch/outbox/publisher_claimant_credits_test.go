package outbox

import (
	"context"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestAPersonsCommentOnASessionClaimedIssueObligesTheClaimantAlone: a person's comment on an
// issue a session claims obliges that claimant, and the credits of the document the comment
// hangs on are no second reason to deliver what the claimant already hears on its own topic
// (addAuthorRoutes). So the session that wrote the document gets no copy of the person's comment
// on an issue it does not hold - at its head, in a reply under it, or in that comment's edit.
//
// A session that wrote in the comment's own thread is no credit of that kind: the thread names
// it, so it keeps the copy whatever the issue's claim is, and keeps it where the document it
// wrote credits it too - the read marks such a session a member of the thread and the member
// wins (commentRecipientsQuery, threadRecipient.fromThread).
func TestAPersonsCommentOnASessionClaimedIssueObligesTheClaimantAlone(t *testing.T) {
	sami := model.Actor{Kind: "user", ID: "sami"}
	claimant := model.Actor{Kind: "session", ID: "session-claimant"}
	second := model.Actor{Kind: "session", ID: "session-second"}
	for _, tc := range []struct {
		name    string
		arrange func(t *testing.T, database *store.Store, broker *events.Broker) []recipientCheck
	}{
		{
			name: "a person's comment at the head of a thread on another session's document reaches the claimant alone",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) []recipientCheck {
				notes := seedDocument(t, database, new("T-1"), "notes", second, []model.Actor{second})
				head := seedThreadComment(t, database, "T-1", notes, sami, nil)
				created := appendCommentEvent(t, database, broker, "comment.created", sami,
					model.Comment{ID: head, Anchor: &model.Anchor{ArtifactID: notes}})
				return []recipientCheck{{event: created, want: []string{claimant.ID}}}
			},
		},
		{
			name: "a person's reply in a thread on another session's document reaches the claimant alone",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) []recipientCheck {
				notes := seedDocument(t, database, new("T-1"), "notes", second, []model.Actor{second})
				head := seedThreadComment(t, database, "T-1", notes, sami, nil)
				reply := seedThreadComment(t, database, "T-1", "", sami, &head)
				created := appendCommentEvent(t, database, broker, "comment.created", sami,
					model.Comment{ID: reply, ReplyTo: &head})
				return []recipientCheck{{event: created, want: []string{claimant.ID}}}
			},
		},
		{
			// The case the marking can get wrong: the same session is a member of the thread and
			// a credit of its document, and the member wins.
			name: "a person's reply reaches the session that wrote in the thread, its document's credit notwithstanding",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) []recipientCheck {
				plan := seedDocument(t, database, new("T-1"), "plan", second, []model.Actor{second})
				head := seedThreadComment(t, database, "T-1", plan, second, nil)
				reply := seedThreadComment(t, database, "T-1", "", sami, &head)
				created := appendCommentEvent(t, database, broker, "comment.created", sami,
					model.Comment{ID: reply, ReplyTo: &head})
				return []recipientCheck{{event: created, want: []string{claimant.ID, second.ID}}}
			},
		},
		{
			name: "a person's edit of their comment on another session's document reaches the claimant alone",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) []recipientCheck {
				notes := seedDocument(t, database, new("T-1"), "notes", second, []model.Actor{second})
				head := seedThreadComment(t, database, "T-1", notes, sami, nil)
				edited := appendCommentEvent(t, database, broker, "comment.edited", sami,
					model.Comment{ID: head, Anchor: &model.Anchor{ArtifactID: notes}})
				return []recipientCheck{{event: edited, want: []string{claimant.ID}}}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := storetest.Open(t)
			broker := events.NewBroker()
			seedIssue(t, database, "T-1", nil)
			claimIssue(t, database, "T-1", claimant)
			checkSessionRecipients(t, database, tc.arrange(t, database, broker))
		})
	}
}

// The same rule under a role mention the read can name no holder for. The filter keeps a session
// whose copy of the comment a mention still decides (mentionDeliveries.decidesCopyFor), and it
// asks that of mentionHolds.state, which folds unpinnedRoleMentions.resendingUnnamed into every
// key rather than into the keys that attempt could be pinned to. So once such an attempt is past
// the lease the outbox waits a send out for, every credit of the document reads a state other
// than mentionFailed, is kept, and takes a comment_fallback_copies row of its own, which the
// pass that publishes those rows sends once nothing decides it: the person's one comment on an
// issue a session claims is answered by the claimant and by the session that wrote the document,
// which is what dispatch://INFRA-1368 is about.
//
// The copies are counted wherever this pass left them - sent now, or held as a row for that
// later pass - because a copy held is a copy that goes.
func TestARoleMentionPastItsLeaseLeavesTheCreditsOfAClaimedIssuesDocumentOut(t *testing.T) {
	sami := model.Actor{Kind: "user", ID: "sami"}
	claimant := model.Actor{Kind: "session", ID: "session-claimant"}
	second := model.Actor{Kind: "session", ID: "session-second"}
	const unheld = "role:nobody-holds-this-role"

	database := storetest.Open(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	claimIssue(t, database, "T-1", claimant)
	notes := seedDocument(t, database, new("T-1"), "notes", second, []model.Actor{second})
	head := seedThreadComment(t, database, "T-1", notes, sami, nil)
	// The attempt of that role's mention as the outbox reads it once its sender's claim has
	// lapsed: past the lease, and naming no session, since only a sender's own resolution learns
	// which session holds a role. No session registry answers this batch, so the read can name no
	// holder for the role either (resolveUnpinnedRoleMentions).
	seedDelivery(t, database, head, unheld, "", "resumed")
	created := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: sami,
		Payload: model.CommentEventPayload{Comment: model.Comment{
			ID: head, IssueKey: new("T-1"), Author: sami,
			Anchor:   &model.Anchor{ArtifactID: notes},
			Mentions: []model.Mention{{Target: unheld, Delivery: "steer"}},
		}},
	})

	publisher := &recordingPublisher{}
	deps := Deps{Store: database, Publisher: publisher}
	if _, err := scanBatch(context.Background(), deps, unfenced{}, holding(t, deps).instance); err != nil {
		t.Fatalf("scan batch: %v", err)
	}

	owed := agentTopics(readEventRow(t, database, created.ID).destinations)
	rows, err := database.Pool.Query(context.Background(), `
		select destination from comment_fallback_copies where comment_id = $1 and session_id is not null
	`, head)
	if err != nil {
		t.Fatalf("read the rows held for the pass that publishes them: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var destination string
		if err := rows.Scan(&destination); err != nil {
			t.Fatalf("read a held row: %v", err)
		}
		owed = append(owed, destination)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the rows held for the pass that publishes them: %v", err)
	}
	slices.Sort(owed)
	want := []string{contracts.AgentSubject(claimant.ID)}
	if !slices.Equal(slices.Compact(owed), want) {
		t.Fatalf("the comment is owed to %v, want %v: on an issue a session claims a person's comment obliges the claimant alone, and a role mention nobody holds is no reason to deliver it to the document's credits",
			owed, want)
	}
}
