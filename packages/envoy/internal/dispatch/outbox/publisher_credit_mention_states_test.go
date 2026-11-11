package outbox

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/session"
)

// creditMentionFixture is what one row of
// TestTheCreditsFilterKeepsACreditExactlyWhereAMentionDecidesItsCopy seeded and what the batch
// must leave behind: the comment the row's mention attempts are of, the events to read through
// checkSessionRecipients, the destinations a comment_fallback_copies row owns once the batch is
// done, the event the mention rule holds back unpublished - nil wherever the pass publishes -
// with the sessions that pass reached meanwhile, and whether every attempt of the comment must
// have been settled failed by the pass.
type creditMentionFixture struct {
	comment       string
	checks        []recipientCheck
	heldRows      []string
	held          *model.Event
	heldReached   []string
	settledFailed bool
}

// TestTheCreditsFilterKeepsACreditExactlyWhereAMentionDecidesItsCopy walks a person's comment on
// an issue a session claims through every state a mention of that comment can be in, one row per
// mentionState, and reads what the batch did with the session the comment's document credits and
// nothing else names. That session is the one the credits filter drops
// (addAuthorRoutes.considerCredit), and it is kept exactly where a mention decides its copy
// (mentionDeliveries.decidesCopyFor, mentionDecidesTheCopy): mentionFailed is the one state that
// leaves the copy to the event, and every other state is a mention's.
//
// Every row is read from what the pass did - the event's published_destinations in order
// (checkSessionRecipients), the comment_fallback_copies rows it wrote, the attempts it settled,
// and the event it held back - and never by asking the filter what it would answer: the filter
// calls decidesCopyFor, so a row that compared the two would pass with the rule wrong in both
// places, which is the failure this round exists to stop. That is also this table's job: one
// decision has two readings on purpose (leftTo reads mentionHolds.state, decidesCopyFor reads
// mentionHolds.named), and the moment a third reading of the decision appears, in either place,
// a row here stops agreeing with the state it names.
//
// The one place the two readings differ is a row of its own: mentionDeliveries.held, which leftTo
// answers before any state and the predicate does not read at all.
func TestTheCreditsFilterKeepsACreditExactlyWhereAMentionDecidesItsCopy(t *testing.T) {
	sami := model.Actor{Kind: "user", ID: "sami"}
	claimant := model.Actor{Kind: "session", ID: "session-claimant"}
	credit := model.Actor{Kind: "session", ID: "session-credit"}
	other := model.Actor{Kind: "session", ID: "session-other"}
	for _, tc := range []struct {
		name string
		// arrange seeds the comment, its mention attempts and the events of it, on an issue
		// claimant claims whose document notes credits credit and no one else.
		arrange func(t *testing.T, database *store.Store, broker *events.Broker, notes string) creditMentionFixture
	}{
		{
			// The ordinary credit, and the state the whole filter rests on: the comment names
			// another session, so the batch has a reading of it, and the credit's own key holds
			// no attempt of it and no attempt resolved to it. A rule that kept a credit at
			// mentionFailed would answer a person's one comment twice.
			name: "a credit with no attempt of its own is dropped, mentionFailed deciding no copy",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker, notes string) creditMentionFixture {
				head := seedThreadComment(t, database, "T-1", notes, sami, nil)
				seedMentionDelivery(t, database, head, other.ID, "sent")
				created := appendCommentEvent(t, database, broker, "comment.created", sami,
					model.Comment{ID: head, Anchor: &model.Anchor{ArtifactID: notes}}, other.ID)
				return creditMentionFixture{
					comment: head,
					checks:  []recipientCheck{{event: created, want: []string{claimant.ID}}},
				}
			},
		},
		{
			// The zero reading: a comment.created that names no mention carries no reading of
			// any attempt, and the edit of such a comment reads its failed mentions alone
			// (lookup.mentionFallbackKeys). Both answer mentionFailed for every session, so the
			// filter applies plainly to both and the credit hears neither.
			name: "a credit of a comment that names no mention is dropped by its creation and by its edit",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker, notes string) creditMentionFixture {
				head := seedThreadComment(t, database, "T-1", notes, sami, nil)
				anchored := model.Comment{ID: head, Anchor: &model.Anchor{ArtifactID: notes}}
				created := appendCommentEvent(t, database, broker, "comment.created", sami, anchored)
				edited := appendCommentEvent(t, database, broker, "comment.edited", sami, anchored)
				return creditMentionFixture{
					comment: head,
					checks: []recipientCheck{
						{event: created, want: []string{claimant.ID}},
						{event: edited, want: []string{claimant.ID}},
					},
				}
			},
		},
		{
			// mentionFailed with the attempt there: the mention settled before the batch read
			// it, so no mention decides this copy and the filter's other exemption gives the
			// session the copy instead, keyed by that failed send (mentionDeliveries.
			// fallbackKeys). The session hears the comment once, by the copy rather than by the
			// mention.
			name: "a credit whose mention settled failed takes the copy, mentionFailed deciding none of it",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker, notes string) creditMentionFixture {
				head := seedThreadComment(t, database, "T-1", notes, sami, nil)
				seedMentionDelivery(t, database, head, credit.ID, "failed")
				created := appendCommentEvent(t, database, broker, "comment.created", sami,
					model.Comment{ID: head, Anchor: &model.Anchor{ArtifactID: notes}}, credit.ID)
				return creditMentionFixture{
					comment: head,
					checks:  []recipientCheck{{event: created, want: []string{claimant.ID, credit.ID}}},
				}
			},
		},
		{
			// mentionSending: the mention's send is live, so the copy is the mention's to make
			// and the event waits for it (destinations.leftTo, batchEvent.mentionPending). The
			// keeping is what holds the event: a credit dropped here would publish the event to
			// the claimant alone, leaving the session with neither the mention nor a copy should
			// that send fail (TestACreditedSessionsPendingMentionDefersTheEventOfAClaimedIssue).
			name: "a credit whose mention is being sent is kept and holds its event back, mentionSending deciding the copy",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker, notes string) creditMentionFixture {
				head := seedThreadComment(t, database, "T-1", notes, sami, nil)
				seedMentionDelivery(t, database, head, credit.ID, "pending")
				created := appendCommentEvent(t, database, broker, "comment.created", sami,
					model.Comment{ID: head, Anchor: &model.Anchor{ArtifactID: notes}}, credit.ID)
				return creditMentionFixture{comment: head, held: &created, heldReached: []string{claimant.ID}}
			},
		},
		{
			// mentionResending: the send is past the lease the outbox waits one out for, so the
			// copy is neither the event's nor a wait of it but a comment_fallback_copies row,
			// which the pass that publishes those rows decides once that send has landed or
			// failed. A credit dropped here would leave no row at all, and the session would
			// hear nothing should the resend fail.
			name: "a credit whose mention is past its lease is kept as a fallback row, mentionResending deciding the copy",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker, notes string) creditMentionFixture {
				head := seedThreadComment(t, database, "T-1", notes, sami, nil)
				seedMentionDelivery(t, database, head, credit.ID, "resumed")
				created := appendCommentEvent(t, database, broker, "comment.created", sami,
					model.Comment{ID: head, Anchor: &model.Anchor{ArtifactID: notes}}, credit.ID)
				return creditMentionFixture{
					comment:  head,
					checks:   []recipientCheck{{event: created, want: []string{claimant.ID}}},
					heldRows: []string{contracts.AgentSubject(credit.ID)},
				}
			},
		},
		{
			// mentionSent: the mention delivered the comment, so the session is kept by the
			// filter and given no copy by the mention rule - two roads to "no copy", and the
			// keeping costs nothing. A rule that published a copy at mentionSent would show this
			// session the comment twice, under a key its host never claimed.
			name: "a credit the mention reached takes no copy of its own, mentionSent deciding the copy",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker, notes string) creditMentionFixture {
				head := seedThreadComment(t, database, "T-1", notes, sami, nil)
				seedMentionDelivery(t, database, head, credit.ID, "sent")
				created := appendCommentEvent(t, database, broker, "comment.created", sami,
					model.Comment{ID: head, Anchor: &model.Anchor{ArtifactID: notes}}, credit.ID)
				return creditMentionFixture{
					comment: head,
					checks:  []recipientCheck{{event: created, want: []string{claimant.ID}}},
				}
			},
		},
		{
			// mentionAbandoned: no send is coming for the attempt and no sender holds it, so the
			// outbox settles it failed, with its comment.delivery receipt, and then publishes
			// the copy (mentionComment.settleAbandoned). The settle is the half of this a
			// dropped credit loses silently: the session gets no copy and the attempt stays
			// pending for ever, since the settle runs inside the copy's own rule.
			name: "a credit whose mention was abandoned takes the copy and has its attempt settled, mentionAbandoned deciding the copy",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker, notes string) creditMentionFixture {
				head := seedThreadComment(t, database, "T-1", notes, sami, nil)
				seedMentionDelivery(t, database, head, credit.ID, "abandoned")
				created := appendCommentEvent(t, database, broker, "comment.created", sami,
					model.Comment{ID: head, Anchor: &model.Anchor{ArtifactID: notes}}, credit.ID)
				return creditMentionFixture{
					comment:       head,
					checks:        []recipientCheck{{event: created, want: []string{claimant.ID, credit.ID}}},
					settledFailed: true,
				}
			},
		},
		{
			// The one place the two readings of the decision differ, and the row that says so.
			// The mention failed, as in the row above, so the filter keeps this session and
			// builds its copy - and leftTo answers mentionDeliveries.held before it reads any
			// state, that destination being a row's already (loadMentionDeliveries), so the copy
			// is left to the pass that publishes or settles that row and this pass sends none.
			// Nothing is lost by the predicate reading no held: at mentionFailed with such a row
			// it says drop, where leftTo says the copy is the row's, and neither gives this pass
			// a copy to send.
			name: "a credit a fallback row already owns takes no copy from the event, the row owning it",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker, notes string) creditMentionFixture {
				head := seedThreadComment(t, database, "T-1", notes, sami, nil)
				seedMentionDelivery(t, database, head, credit.ID, "failed")
				created := appendCommentEvent(t, database, broker, "comment.created", sami,
					model.Comment{ID: head, Anchor: &model.Anchor{ArtifactID: notes}}, credit.ID)
				if _, err := database.Pool.Exec(context.Background(), `
					insert into comment_fallback_copies (comment_id, destination, event_id, session_id)
					values ($1, $2, $3, $4)
				`, head, contracts.AgentSubject(credit.ID), created.ID, credit.ID); err != nil {
					t.Fatalf("hold the fallback row that owns the credit's destination: %v", err)
				}
				return creditMentionFixture{
					comment:  head,
					checks:   []recipientCheck{{event: created, want: []string{claimant.ID}}},
					heldRows: []string{contracts.AgentSubject(credit.ID)},
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := storetest.Open(t)
			broker := events.NewBroker()
			seedIssue(t, database, "T-1", nil)
			claimIssue(t, database, "T-1", claimant)
			notes := seedDocument(t, database, new("T-1"), "notes", credit, []model.Actor{credit})
			fixture := tc.arrange(t, database, broker, notes)
			if fixture.held != nil {
				checkEventHeldForItsMention(t, database, *fixture.held, fixture.heldReached)
			} else {
				checkSessionRecipients(t, database, fixture.checks)
			}
			if got := heldDestinations(t, database, fixture.comment); !slices.Equal(got, fixture.heldRows) {
				t.Fatalf("the comment holds fallback rows for %v, want %v: a copy a mention decides past its lease is kept as a row, and one no mention decides is the event's",
					got, fixture.heldRows)
			}
			if !fixture.settledFailed {
				return
			}
			var pending int
			if err := database.Pool.QueryRow(context.Background(), `
				select count(*) from comment_deliveries where comment_id = $1 and state <> 'failed'
			`, fixture.comment).Scan(&pending); err != nil {
				t.Fatalf("read the comment's attempts: %v", err)
			}
			if pending != 0 {
				t.Fatalf("%d attempt(s) of the comment are not settled failed, want none: the copy's own rule settles an abandoned attempt before it publishes the copy (mentionComment.settleAbandoned), so a session dropped before that rule leaves the attempt pending for ever",
					pending)
			}
		})
	}
}

// checkEventHeldForItsMention runs one batch and fails unless event was left unpublished, having
// reached the own topics of reached's sessions and of no other. It is what the mentionSending row
// reads in place of checkSessionRecipients: keeping a credit whose mention is still being sent is
// what holds the event itself back (destinations.leftTo, batchEvent.mentionPending), so the pass
// leaves published_at unset and checkSessionRecipients, which fails an event that was not
// published, cannot say it. The destinations the pass did record are read here as that helper
// reads them, in order.
func checkEventHeldForItsMention(t *testing.T, database *store.Store, event model.Event, reached []string) {
	t.Helper()
	publisher := &recordingPublisher{}
	deps := Deps{Store: database, Publisher: publisher, Broker: events.NewBroker()}
	if _, err := scanBatch(context.Background(), deps, unfenced{}, holding(t, deps).instance); err != nil {
		t.Fatalf("scan batch: %v", err)
	}
	if at := publishedAt(t, database, event.ID); at != nil {
		t.Fatalf("%s event %d was published at %v while the mention of a session the document credits was being sent, want it held: the credit's copy stays in the list and the event waits for that send",
			event.Type, event.ID, at)
	}
	want := make([]string, 0, len(reached))
	for _, sessionID := range reached {
		want = append(want, contracts.AgentSubject(sessionID))
	}
	destinations, _ := publishedDestinations(t, database, event.ID)
	if got := agentTopics(destinations); !slices.Equal(got, want) {
		t.Fatalf("%s event %d reached sessions %v, want %v (every topic: %v)", event.Type, event.ID, got, want, destinations)
	}
}

// TestACreditThisReadResolvedTheRoleMentionToKeepsItsCopy is the other side of
// TestARoleMentionPastItsLeaseLeavesTheCreditsOfAClaimedIssuesDocumentOut, and the arm of
// mentionHolds.named that the key's own attempts cannot answer: on an issue a session claims, a
// session a document credits alone keeps its copy when this very read resolved the comment's
// @role: mention to it (unpinnedRoleMentions.state). The attempt names no session - only a
// sender's own resolution pins one - so that session's own attempts read mentionFailed, and what
// names it here is the role's claim, which this batch resolves
// (resolveUnpinnedRoleMentions).
//
// The mutation it fails on is mentionHolds.named simplified to mentionOutcomes.state: with the
// key's own attempts alone the credits filter drops this session, no row is held for it, and the
// comment the mention was on its way to it with reaches it by neither road should that send fail.
// The tester's case fails on the other mutation, mentionHolds.state in place of named.
//
// The attempt is past the lease the outbox waits a send out for, so the copy it decides is a
// comment_fallback_copies row rather than a wait of the event's: the pass that publishes those
// rows reads the attempts as they stand then, by when the sender has pinned the session it
// resolved (destinations.leftTo, publishFallbackCopies).
func TestACreditThisReadResolvedTheRoleMentionToKeepsItsCopy(t *testing.T) {
	const role = "held-by-the-session-the-document-credits"
	sami := model.Actor{Kind: "user", ID: "sami"}
	claimant := model.Actor{Kind: "session", ID: "session-claimant"}
	credit := model.Actor{Kind: "session", ID: "ses_credits_the_document_and_holds_the_role"}
	target := "role:" + role

	rig := newRoleRig(t)
	rig.claim(t, role, credit.ID, session.SessionEntry{SelfSubscribed: true})
	seedIssue(t, rig.database, "T-1", nil)
	claimIssue(t, rig.database, "T-1", claimant)
	notes := seedDocument(t, rig.database, new("T-1"), "notes", credit, []model.Actor{credit})
	head := seedThreadComment(t, rig.database, "T-1", notes, sami, nil)
	// The attempt as the outbox reads it once its sender's claim has lapsed: past the lease, and
	// naming no session, since only a sender's own resolution learns which session holds a role.
	// This batch's own resolution names one, the role being claimed here.
	seedDelivery(t, rig.database, head, target, "", "resumed")
	created := appendEvent(t, rig.database, rig.broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: sami,
		Payload: model.CommentEventPayload{Comment: model.Comment{
			ID: head, IssueKey: new("T-1"), Author: sami,
			Anchor:   &model.Anchor{ArtifactID: notes},
			Mentions: []model.Mention{{Target: target, Delivery: "steer"}},
		}},
	})

	deps := rig.publisherOn(&recordingPublisher{})
	if _, err := scanBatch(context.Background(), deps, unfenced{}, holding(t, deps).instance); err != nil {
		t.Fatalf("scan batch: %v", err)
	}

	want := []string{contracts.AgentSubject(claimant.ID)}
	if got := agentTopics(readEventRow(t, rig.database, created.ID).destinations); !slices.Equal(got, want) {
		t.Fatalf("the comment reached sessions %v, want %v: the claimant hears it on its own topic, and the session the mention is on its way to takes the copy that mention decides, not one of the event's",
			got, want)
	}
	rows := []string{contracts.AgentSubject(credit.ID)}
	if got := heldDestinations(t, rig.database, head); !slices.Equal(got, rows) {
		t.Fatalf("the comment holds fallback rows for %v, want %v: this read resolved the comment's mention of %s to that session, so its copy is the one that mention decides and is kept for the pass that publishes it",
			got, rows, fmt.Sprintf("@%s", target))
	}
}
