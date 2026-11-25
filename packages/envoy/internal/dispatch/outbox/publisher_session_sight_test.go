package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/sessionanswers"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The people the session-sight tests tie a session to: an outside person, a restricted member,
// whom TT's access list names only in a row's visible arm.
const sightOutsider = "outsider@other.example"

// seedSightProject makes seedIssue's project TT private and gives it the outside person, a
// restricted member TT's access list names only when visible: the event's project is then one the
// sessions tied to that person see, or one they do not, and nothing else differs between the two.
func seedSightProject(t *testing.T, database *store.Store, visible bool) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `update projects set visibility = 'private' where key = 'TT'`); err != nil {
		t.Fatalf("make TT private: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `insert into people (email, member_kind) values ($1, 'restricted')`, sightOutsider); err != nil {
		t.Fatalf("seed the outside person: %v", err)
	}
	if !visible {
		return
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into project_access (project_key, principal_kind, principal, added_by)
		values ('TT', 'person', $1, '{"kind":"user","id":"alice"}')
	`, sightOutsider); err != nil {
		t.Fatalf("name the outside person on TT's access list: %v", err)
	}
}

// tieSession ties session to person, as a broker-issued token that proved it records
// (access.TieSession).
func tieSession(t *testing.T, database *store.Store, session, person string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(),
		`insert into session_owners (session_id, login) values ($1, $2)`, session, person); err != nil {
		t.Fatalf("tie %s to %s: %v", session, person, err)
	}
}

// TestADestinationWhoseSessionDoesNotSeeTheEventsProjectIsWithheld holds every destination that
// reaches a session's own subject, and the role route's forward to its holder, to the session's
// sight (destinations.withheld, withholdUnseenHolders, access.SessionSeesProjectClause): each row
// publishes one event of the private project TT whose one session destination is that kind,
// reached by a session tied to an outside person. In the row's hidden arm TT's access list does
// not name that person, and the event reaches its issue's topic alone; in its visible arm, the
// control in the same run, the list names the person and the event reaches the session too. A row
// fails naming its destination.
func TestADestinationWhoseSessionDoesNotSeeTheEventsProjectIsWithheld(t *testing.T) {
	alice := model.Actor{Kind: "user", ID: "alice"}
	outside := model.Actor{Kind: "session", ID: "session-outside"}
	const (
		issueTopic   = "notifications.dispatch.issue.T-1."
		outsideTopic = "notifications.agent.session-outside"
		commentID    = "6f1d3a5e-1b9a-4c39-9a51-2f4e8b7c0d11"
		messageID    = "0b6e2f94-7f3c-4d1a-8e25-3c9a1d4b7e62"
	)
	issue := new("T-1")
	comment := func() model.Event {
		return model.Event{IssueKey: issue, Type: "comment.created", Actor: alice, Payload: model.CommentEventPayload{
			Comment: model.Comment{ID: commentID, IssueKey: issue, Body: "Is the order right?"},
		}}
	}
	sessionRoute, roleRoute := "session:"+outside.ID, "role:reviewer"
	for _, tc := range []struct {
		// destination is the kind of destination the row holds, which a failure names.
		destination string
		route       *string
		claim       *model.Actor
		// tied is the session the row ties to the outside person: the destination's session.
		tied    string
		roles   bool
		arrange func(t *testing.T, database *store.Store) model.Event
		// reached is the topic the destination is sent on when the session sees TT.
		reached string
	}{
		{
			destination: "the owner's session route (addRoute)",
			route:       &sessionRoute,
			tied:        outside.ID,
			arrange:     func(*testing.T, *store.Store) model.Event { return comment() },
			reached:     outsideTopic,
		},
		{
			destination: "the claimant (addClaimantRoute)",
			claim:       &outside,
			tied:        outside.ID,
			arrange:     func(*testing.T, *store.Store) model.Event { return comment() },
			reached:     outsideTopic,
		},
		{
			destination: "a primary document's author (addPrimaryDocumentAuthors)",
			tied:        outside.ID,
			arrange: func(t *testing.T, database *store.Store) model.Event {
				makePrimary(t, database, seedDocument(t, database, issue, "spec", outside, []model.Actor{outside}))
				return comment()
			},
			reached: outsideTopic,
		},
		{
			destination: "an author route (addAuthorRoutes: the message a reply answers)",
			tied:        outside.ID,
			arrange: func(t *testing.T, database *store.Store) model.Event {
				answered := seedMessage(t, database, "T-1", outside, "Status?", nil)
				return model.Event{IssueKey: issue, Type: "message.created", Actor: alice, Payload: model.MessageEventPayload{
					Message: model.Message{ID: messageID, IssueKey: issue, Body: "Fine", InReplyTo: &answered},
				}}
			},
			reached: outsideTopic,
		},
		{
			destination: "a follower route (addFollowerRoutes)",
			tied:        outside.ID,
			arrange: func(t *testing.T, database *store.Store) model.Event {
				askID := seedAsk(t, database, "T-1", outside, "Ship it?")
				return model.Event{IssueKey: issue, Type: "ask.answered", Actor: alice, Payload: model.Ask{
					ID: askID, IssueKey: issue, Question: "Ship it?", State: "answered",
				}}
			},
			reached: outsideTopic,
		},
		{
			destination: "the previous claimant (addPreviousClaimant)",
			tied:        outside.ID,
			arrange: func(*testing.T, *store.Store) model.Event {
				return model.Event{IssueKey: issue, Type: "issue.claimed", Actor: model.Actor{Kind: "session", ID: "session-taker"},
					Payload: map[string]any{"key": "T-1", "previous_claim": map[string]any{"actor": outside}}}
			},
			reached: outsideTopic,
		},
		{
			destination: "a role route's forward to its holder (resolveRoleHolders)",
			route:       &roleRoute,
			tied:        roleHolder,
			roles:       true,
			arrange:     func(*testing.T, *store.Store) model.Event { return comment() },
			reached:     contracts.RoleTopicPrefix + "reviewer",
		},
	} {
		for _, visible := range []bool{false, true} {
			arm := "hidden"
			if visible {
				arm = "visible"
			}
			t.Run(tc.destination+"/"+arm, func(t *testing.T) {
				database := storetest.Open(t)
				seedIssue(t, database, "T-1", tc.route)
				seedSightProject(t, database, visible)
				tieSession(t, database, tc.tied, sightOutsider)
				if tc.claim != nil {
					claimIssue(t, database, "T-1", *tc.claim)
				}
				event := tc.arrange(t, database)
				event.ID, event.CreatedAt, event.Project = 100, time.Now(), "TT"
				event.Notify = events.NewBroker().Notify(event)
				raw, err := json.Marshal(event.Payload)
				if err != nil {
					t.Fatalf("encode payload: %v", err)
				}
				event.Payload = nil
				if err := json.Unmarshal(raw, &event.Payload); err != nil {
					t.Fatalf("decode payload: %v", err)
				}
				var claimant string
				if tc.claim != nil {
					claimant = tc.claim.ID
				}
				publisher := &recordingPublisher{}
				deps := Deps{Store: database, Publisher: publisher, Broker: events.NewBroker()}
				if tc.roles {
					deps = withHeldRoles(deps)
				}
				if err := publish(context.Background(), deps, event, "", routing{route: tc.route, claimant: claimant}, map[string]struct{}{}); err != nil {
					t.Fatalf("%s: publish %s: %v", tc.destination, event.Type, err)
				}
				want := []string{issueTopic + event.Type}
				if visible {
					want = append(want, tc.reached)
				}
				if got := topicsOf(publisher.all()); !slices.Equal(got, want) {
					t.Fatalf("%s, TT %s to the session: topics = %v, want %v", tc.destination, arm, got, want)
				}
			})
		}
	}
}

// TestASessionWhoseSightTheBatchDidNotReadIsWithheldAndBacksTheEventOff: a lookup that read no
// sight of the route's session answers an error, never a guess, and the destination build leaves
// the session out and returns that error, which backs the event off once its owner topic is sent
// (destinations.withheld, destinations.err).
func TestASessionWhoseSightTheBatchDidNotReadIsWithheldAndBacksTheEventOff(t *testing.T) {
	route := "session:session-unread"
	issue := new("T-1")
	event := model.Event{ID: 7, IssueKey: issue, Project: "TT", Type: "comment.created", Notify: true,
		Actor: model.Actor{Kind: "user", ID: "alice"}, CreatedAt: time.Now(),
		Payload: map[string]any{"id": "6f1d3a5e-1b9a-4c39-9a51-2f4e8b7c0d12", "body": "Hello"}}
	item, err := envelope(event, "")
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}
	read := &lookups{failed: map[string]error{}, sight: map[string]map[string]bool{},
		sightSessions: map[string]struct{}{}, sightProjects: map[string]struct{}{"TT": {}}}
	to, err := eventDestinations(context.Background(), Deps{}, &batchEvent{
		pendingEvent: pendingEvent{event: event, routing: routing{route: &route}}, item: item,
	}, read)
	if err == nil || !strings.Contains(err.Error(), "no sight of session session-unread") {
		t.Fatalf("the destinations of a session the batch read no sight of answered %v, want the unread sight", err)
	}
	if got := len(to.list); got != 1 || to.list[0].subject != item.Topic {
		t.Fatalf("the destinations are %v, want the owner topic alone", to.list)
	}
}

// TestTheFallbackPassWithholdsAHeldCopyFromASessionThatDoesNotSeeItsProject: a copy the batch held
// as a comment_fallback_copies row for a session's own subject is judged again when the pass
// publishes it (decideFallbackCopy): a session that does not see the event's project now has the
// row settled with nothing published; the control, the same row whose session sees the project,
// is published.
func TestTheFallbackPassWithholdsAHeldCopyFromASessionThatDoesNotSeeItsProject(t *testing.T) {
	for _, visible := range []bool{false, true} {
		ctx := context.Background()
		database := storetest.Open(t)
		broker := events.NewBroker()
		seedIssue(t, database, "T-1", nil)
		seedSightProject(t, database, visible)
		tieSession(t, database, "session-outside", sightOutsider)
		alice := model.Actor{Kind: "user", ID: "alice"}
		commentID := seedComment(t, database, "T-1", alice, "a comment", nil)
		event := appendEvent(t, database, broker, model.Event{
			IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
			Payload: model.CommentEventPayload{Comment: model.Comment{ID: commentID, IssueKey: new("T-1"), Body: "a comment"}},
		})
		const destination = "notifications.agent.session-outside"
		if _, err := database.Pool.Exec(ctx, `
			update events set published_at = now() where id = $1;
		`, event.ID); err != nil {
			t.Fatalf("publish the comment's event: %v", err)
		}
		if _, err := database.Pool.Exec(ctx, `
			insert into comment_fallback_copies (comment_id, destination, event_id, session_id) values ($1, $2, $3, 'session-outside')
		`, commentID, destination, event.ID); err != nil {
			t.Fatalf("hold the session's copy: %v", err)
		}
		publisher := &recordingPublisher{}
		deps := Deps{Store: database, Publisher: publisher, Broker: broker}
		publishFallbackCopies(ctx, deps, unfenced{}, holding(t, deps).instance)
		row := readHeldCopy(t, database, commentID, destination)
		sent := topicsNaming(publisher, "session-outside")
		switch {
		case !visible && (!row.settled || row.published || len(sent) != 0):
			t.Fatalf("the fallback row of a session that does not see TT is settled %v, published %v, sent to %v; want it settled with nothing sent",
				row.settled, row.published, sent)
		case visible && (!row.settled || !row.published || len(sent) != 1):
			t.Fatalf("the fallback row of a session that sees TT is settled %v, published %v, sent to %v; want it published once",
				row.settled, row.published, sent)
		}
	}
}

// TestTheKeptCopysHandoverAsksTheHoldersSight: the handover behind a keep forwards the kept copy
// to the role's holder only when that holder may be shown the event's project (holderSeesTheCopy):
// a holder tied to the outside person sees TT only while TT's access list names that person, and
// an event whose payload names no project is shown to any holder.
func TestTheKeptCopysHandoverAsksTheHoldersSight(t *testing.T) {
	for _, visible := range []bool{false, true} {
		ctx := context.Background()
		database := storetest.Open(t)
		seedIssue(t, database, "T-1", nil)
		seedSightProject(t, database, visible)
		tieSession(t, database, roleHolder, sightOutsider)
		deps := Deps{Store: database}
		of := func(project string) contracts.Envelope {
			payload, err := json.Marshal(model.Event{ID: 9, IssueKey: new("T-1"), Project: project, Type: "comment.created"})
			if err != nil {
				t.Fatalf("encode the event: %v", err)
			}
			return contracts.Envelope{EventID: "dispatch-9", Payload: string(payload)}
		}
		sees, err := holderSeesTheCopy(ctx, deps, roleHolder, of("TT"))
		if err != nil || sees != visible {
			t.Fatalf("with TT's list naming the holder's person %v, the holder sees TT's kept copy: %v, %v", visible, sees, err)
		}
		if sees, err := holderSeesTheCopy(ctx, deps, roleHolder, of("")); err != nil || !sees {
			t.Fatalf("a kept copy naming no project is shown to the holder: %v, %v; want true", sees, err)
		}
	}
	if _, err := holderSeesTheCopy(context.Background(), Deps{}, roleHolder, contracts.Envelope{Payload: `{"project":"TT"}`}); !errors.Is(err, errNoSightReader) {
		t.Fatalf("a handover with no store answered %v, want errNoSightReader", err)
	}
}

// TestAWithheldDestinationIsRecordedNowhereAReaderSees: an event the outbox publishes with a
// session's copy withheld (destinations.withheld) records nothing for that session - its
// published_destinations hold the issue's topic alone, and the session's list of what people
// wrote that reached it (sessionanswers.List, GET /api/v1/agents/{session_id}/answers) is empty -
// while the event itself is published. The control in the same run is the same scan with TT's list
// naming the session's person: the session's topic is recorded and the comment is listed.
func TestAWithheldDestinationIsRecordedNowhereAReaderSees(t *testing.T) {
	const session = "session-outside"
	for _, visible := range []bool{false, true} {
		ctx := context.Background()
		database := storetest.Open(t)
		broker := events.NewBroker()
		route := "session:" + session
		seedIssue(t, database, "T-1", &route)
		seedSightProject(t, database, visible)
		tieSession(t, database, session, sightOutsider)
		alice := model.Actor{Kind: "user", ID: "alice"}
		commentID := seedComment(t, database, "T-1", alice, "Is the order right?", nil)
		event := appendEvent(t, database, broker, model.Event{
			IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
			Payload: model.CommentEventPayload{Comment: model.Comment{ID: commentID, IssueKey: new("T-1"), Body: "Is the order right?"}},
		})
		deps := Deps{Store: database, Publisher: &recordingPublisher{}}
		if _, err := scanBatch(ctx, deps, unfenced{}, holding(t, deps).instance); err != nil {
			t.Fatalf("scan batch: %v", err)
		}
		var destinations []string
		if err := database.Pool.QueryRow(ctx, `select published_destinations from events where id = $1 and published_at is not null`,
			event.ID).Scan(&destinations); err != nil {
			t.Fatalf("read the published event's destinations (visible %v): %v", visible, err)
		}
		items, err := sessionanswers.List(ctx, database.Pool, refs.VisibilityArg(model.ServerReader), session, time.Now().Add(-time.Hour))
		if err != nil {
			t.Fatalf("list what reached %s: %v", session, err)
		}
		recorded := slices.Contains(destinations, contracts.LegacyAgentSubject(session))
		switch {
		case !visible && (recorded || len(items) != 0):
			t.Fatalf("a copy withheld from %s, which does not see TT: destinations %v, answers %d; want neither to name it", session, destinations, len(items))
		case visible && (!recorded || len(items) != 1):
			t.Fatalf("a copy %s took, which sees TT: destinations %v, answers %d; want its topic and the comment", session, destinations, len(items))
		}
	}
}
