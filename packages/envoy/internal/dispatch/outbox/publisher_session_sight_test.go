package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
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
	read := &lookups{failed: map[string]error{},
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
		_, sees, err := holderSeesTheCopy(ctx, deps, roleHolder, of("TT"))
		if err != nil || sees != visible {
			t.Fatalf("with TT's list naming the holder's person %v, the holder sees TT's kept copy: %v, %v", visible, sees, err)
		}
		if _, sees, err := holderSeesTheCopy(ctx, deps, roleHolder, of("")); err != nil || !sees {
			t.Fatalf("a kept copy naming no project is shown to the holder: %v, %v; want true", sees, err)
		}
	}
	if _, _, err := holderSeesTheCopy(context.Background(), Deps{}, roleHolder, contracts.Envelope{Payload: `{"project":"TT"}`}); !errors.Is(err, errNoSightReader) {
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

// TestTheOutboxAsksTheListenerAgainBeforeItJudgesAnExpiredList holds every outbox read of a
// session's sight to the listener record as the listener answers now (ListenerRefresh.settle,
// Deps.ListenerRefresh). The stored record has expired; it lists TT for session-listed and
// role-holder, both tied to nobody, and PUB alone for session-member, the session of a full member
// TT's access list names. While TT is private every verdict depends on a list, so each reader runs
// the refresh first. Each row runs once with a refresh that renews the record, after which the
// sessions whose list names TT are sent TT's event and session-member is not, and once with a
// refresh that leaves the record expired, as a listener that does not answer does, after which
// nothing is sent on a list the read could not judge: the batch's event backs off, the role
// forward is held and its event backs off, the fallback row counts an attempt and stays held, and
// the handover answers the error. Last, with TT public and its member full, no verdict depends on
// a list: the event reaches both sessions in the same pass, and the refresh, which would fail, is
// never run.
func TestTheOutboxAsksTheListenerAgainBeforeItJudgesAnExpiredList(t *testing.T) {
	const (
		member  = "member@example.com"
		listed  = "session-listed"
		claimer = "session-member"
		record  = `[{"session_id":"session-listed","projects":["TT"]},{"session_id":"session-member","projects":["PUB"]},{"session_id":"role-holder","projects":["TT"]}]`
	)
	ctx := context.Background()
	alice := model.Actor{Kind: "user", ID: "alice"}
	// seed stands up T-1 of TT, routed to route, TT private unless public, the full member on TT's
	// list and tied to session-member, and the listener record, expired a minute ago.
	seed := func(t *testing.T, route *string, public bool) *store.Store {
		t.Helper()
		database := storetest.Open(t)
		seedIssue(t, database, "T-1", route)
		tieSession(t, database, claimer, member)
		visibility := "private"
		if public {
			visibility = "public"
		}
		for _, statement := range []struct {
			sql  string
			args []any
		}{
			{`update projects set visibility = $1 where key = 'TT'`, []any{visibility}},
			{`insert into people (email, member_kind) values ($1, 'full')`, []any{member}},
			{`insert into project_access (project_key, principal_kind, principal, added_by) values ('TT', 'person', $1, '{"kind":"user","id":"alice"}')`, []any{member}},
			{`insert into route_roster (answered, sessions, sent_at, expires_at) values (true, $1, now() - interval '1 hour', now() - interval '1 minute')`, []any{record}},
		} {
			if _, err := database.Pool.Exec(ctx, statement.sql, statement.args...); err != nil {
				t.Fatalf("seed %q: %v", statement.sql, err)
			}
		}
		return database
	}
	// comment writes a comment of alice's on T-1 and its event.
	comment := func(t *testing.T, database *store.Store, broker *events.Broker) (string, model.Event) {
		t.Helper()
		commentID := seedComment(t, database, "T-1", alice, "Is the order right?", nil)
		return commentID, appendEvent(t, database, broker, model.Event{
			IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
			Payload: model.CommentEventPayload{Comment: model.Comment{ID: commentID, IssueKey: new("T-1"), Body: "Is the order right?"}},
		})
	}
	// refresh is a bound refresh counting its runs, which renews the stored record when renews.
	refresh := func(t *testing.T, database *store.Store, renews bool) (*ListenerRefresh, *atomic.Int32) {
		runs, listener := &atomic.Int32{}, &ListenerRefresh{}
		listener.Bind(func(ctx context.Context) {
			runs.Add(1)
			if !renews {
				return
			}
			if _, err := database.Pool.Exec(ctx, `update route_roster set sent_at = now(), expires_at = now() + interval '1 hour'`); err != nil {
				t.Errorf("renew the listener record: %v", err)
			}
		})
		return listener, runs
	}
	// settled reads whether event id was published, and whether it waits on a back-off.
	settled := func(t *testing.T, database *store.Store, id int64) (published, backedOff bool) {
		t.Helper()
		if err := database.Pool.QueryRow(ctx, `select published_at is not null, next_attempt_at is not null from events where id = $1`,
			id).Scan(&published, &backedOff); err != nil {
			t.Fatalf("read event %d: %v", id, err)
		}
		return published, backedOff
	}
	route := "session:" + listed
	for _, renews := range []bool{true, false} {
		arm := "/a refresh that renews the record"
		if !renews {
			arm = "/a refresh that leaves the record expired"
		}
		t.Run("the batch's read"+arm, func(t *testing.T) {
			database := seed(t, &route, false)
			claimIssue(t, database, "T-1", model.Actor{Kind: "session", ID: claimer})
			_, event := comment(t, database, events.NewBroker())
			listener, runs := refresh(t, database, renews)
			publisher := &recordingPublisher{}
			deps := Deps{Store: database, Publisher: publisher, ListenerRefresh: listener}
			if _, err := scanBatch(ctx, deps, unfenced{}, holding(t, deps).instance); err != nil {
				t.Fatalf("scan batch: %v", err)
			}
			published, backedOff := settled(t, database, event.ID)
			sentListed, sentMember := topicsNaming(publisher, listed), topicsNaming(publisher, claimer)
			switch {
			case renews && (len(sentListed) != 1 || len(sentMember) != 0 || !published):
				t.Fatalf("after a refresh: %s sent %v, %s sent %v, published %v; want the listed session alone sent TT's event",
					listed, sentListed, claimer, sentMember, published)
			case !renews && (len(sentListed) != 0 || len(sentMember) != 0 || published || !backedOff):
				t.Fatalf("with the record still expired: %s sent %v, %s sent %v, published %v, backed off %v; want neither sent and the event backed off",
					listed, sentListed, claimer, sentMember, published, backedOff)
			case runs.Load() == 0:
				t.Fatalf("the batch judged an expired list with no refresh")
			}
		})
		t.Run("a role forward's holder"+arm, func(t *testing.T) {
			roleRoute := "role:reviewer"
			database := seed(t, &roleRoute, false)
			_, event := comment(t, database, events.NewBroker())
			listener, runs := refresh(t, database, renews)
			publisher := &recordingPublisher{}
			deps := withHeldRoles(Deps{Store: database, Publisher: publisher, ListenerRefresh: listener})
			if _, err := scanBatch(ctx, deps, unfenced{}, holding(t, deps).instance); err != nil {
				t.Fatalf("scan batch: %v", err)
			}
			published, backedOff := settled(t, database, event.ID)
			forwarded := topicsNaming(publisher, contracts.RoleTopicPrefix+"reviewer")
			switch {
			case renews && (len(forwarded) != 1 || !published):
				t.Fatalf("after a refresh: forwarded %v, published %v; want %s forwarded TT's event", forwarded, published, roleHolder)
			case !renews && (len(forwarded) != 0 || published || !backedOff):
				t.Fatalf("with the record still expired: forwarded %v, published %v, backed off %v; want nothing forwarded and the event backed off",
					forwarded, published, backedOff)
			case runs.Load() == 0:
				t.Fatalf("the role forward judged its holder by an expired list with no refresh")
			}
		})
		t.Run("the fallback pass"+arm, func(t *testing.T) {
			database := seed(t, nil, false)
			broker := events.NewBroker()
			commentID, event := comment(t, database, broker)
			destination := contracts.LegacyAgentSubject(listed)
			if _, err := database.Pool.Exec(ctx, `update events set published_at = now() where id = $1`, event.ID); err != nil {
				t.Fatalf("publish the comment's event: %v", err)
			}
			if _, err := database.Pool.Exec(ctx, `
				insert into comment_fallback_copies (comment_id, destination, event_id, session_id) values ($1, $2, $3, $4)
			`, commentID, destination, event.ID, listed); err != nil {
				t.Fatalf("hold the session's copy: %v", err)
			}
			listener, runs := refresh(t, database, renews)
			publisher := &recordingPublisher{}
			deps := Deps{Store: database, Publisher: publisher, Broker: broker, ListenerRefresh: listener}
			publishFallbackCopies(ctx, deps, unfenced{}, holding(t, deps).instance)
			row, sent := readHeldCopy(t, database, commentID, destination), topicsNaming(publisher, listed)
			switch {
			case renews && (!row.settled || !row.published || len(sent) != 1):
				t.Fatalf("after a refresh the row is settled %v, published %v, sent to %v; want it published once", row.settled, row.published, sent)
			case !renews && (row.settled || row.attempts == 0 || len(sent) != 0):
				t.Fatalf("with the record still expired the row is settled %v after %d attempts, sent to %v; want it held, an attempt counted and nothing sent",
					row.settled, row.attempts, sent)
			case runs.Load() == 0:
				t.Fatalf("the fallback pass judged an expired list with no refresh")
			}
		})
		t.Run("the kept copy's handover"+arm, func(t *testing.T) {
			database := seed(t, nil, false)
			listener, runs := refresh(t, database, renews)
			_, sees, err := holderSeesTheCopy(ctx, Deps{Store: database, ListenerRefresh: listener}, roleHolder,
				contracts.Envelope{EventID: "dispatch-9", Payload: `{"project":"TT"}`})
			switch {
			case renews && (err != nil || !sees):
				t.Fatalf("after a refresh %s sees TT's kept copy: %v, %v; want true", roleHolder, sees, err)
			case !renews && (sees || !errors.Is(err, errListenerRecordExpired)):
				t.Fatalf("with the record still expired %s sees TT's kept copy: %v, %v; want errListenerRecordExpired", roleHolder, sees, err)
			case runs.Load() == 0:
				t.Fatalf("the handover judged its holder by an expired list with no refresh")
			}
		})
	}
	t.Run("with every project public and each person a full member", func(t *testing.T) {
		database := seed(t, &route, true)
		claimIssue(t, database, "T-1", model.Actor{Kind: "session", ID: claimer})
		_, event := comment(t, database, events.NewBroker())
		listener, runs := refresh(t, database, false)
		publisher := &recordingPublisher{}
		deps := Deps{Store: database, Publisher: publisher, ListenerRefresh: listener}
		if _, err := scanBatch(ctx, deps, unfenced{}, holding(t, deps).instance); err != nil {
			t.Fatalf("scan batch: %v", err)
		}
		published, _ := settled(t, database, event.ID)
		if sentListed, sentMember := topicsNaming(publisher, listed), topicsNaming(publisher, claimer); len(sentListed) != 1 || len(sentMember) != 1 || !published || runs.Load() != 0 {
			t.Fatalf("with no private project: %s sent %v, %s sent %v, published %v, refreshed %d times; want both sent in the pass and no refresh",
				listed, sentListed, claimer, sentMember, published, runs.Load())
		}
	})
}

// TestTheOutboxAsksTheListenerAgainWithItsGateFree holds the refresh of an expired listener record
// outside the outbox's gate (ListenerRefresh.askAgain): the refresh may wait on the listener for
// the API's whole fetch bound, and the gate is the one statement at a time of every batch of a
// scan (lanes.statement), so a refresh run under it holds every other batch's statements back
// while it waits. Each reader that runs under a scan's gate - the batch's read, a role forward's
// holder and the kept copy's handover - runs here with a gate, on a context that carries the mark
// a scan's batches share (store.WithTransactionTracking), over the private TT and an expired record
// that lists TT for the session it judges. The refresh finds the gate free, and renews the record
// while another batch of the scan holds a connection on that shared mark (store.HoldsConnection),
// which the pool refuses (store.ErrNestedAcquire) unless the refresh reads under a mark of its own
// (store.ForConcurrentWorker); the reader then sends TT's event to that session. The fallback pass
// runs under no gate.
func TestTheOutboxAsksTheListenerAgainWithItsGateFree(t *testing.T) {
	const record = `[{"session_id":"session-listed","projects":["TT"]},{"session_id":"role-holder","projects":["TT"]}]`
	ctx := context.Background()
	// seed stands up T-1 of the private TT, routed to route, and the listener record, expired a
	// minute ago.
	seed := func(t *testing.T, route *string) *store.Store {
		t.Helper()
		database := storetest.Open(t)
		seedIssue(t, database, "T-1", route)
		for _, statement := range []string{
			`update projects set visibility = 'private' where key = 'TT'`,
			`insert into route_roster (answered, sessions, sent_at, expires_at) values (true, '` + record + `', now() - interval '1 hour', now() - interval '1 minute')`,
		} {
			if _, err := database.Pool.Exec(ctx, statement); err != nil {
				t.Fatalf("seed %q: %v", statement, err)
			}
		}
		return database
	}
	// comment writes a comment of alice's on T-1 and its event.
	comment := func(t *testing.T, database *store.Store) {
		t.Helper()
		alice := model.Actor{Kind: "user", ID: "alice"}
		commentID := seedComment(t, database, "T-1", alice, "Is the order right?", nil)
		appendEvent(t, database, events.NewBroker(), model.Event{
			IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
			Payload: model.CommentEventPayload{Comment: model.Comment{ID: commentID, IssueKey: new("T-1"), Body: "Is the order right?"}},
		})
	}
	// gated is deps as a scan runs its batches, with a gate and on a context carrying the scan's
	// shared mark, and a bound refresh that counts its runs, checks the gate and the mark, and
	// renews the record.
	gated := func(t *testing.T, deps Deps) (Deps, context.Context, *atomic.Int32) {
		t.Helper()
		scan, runs := store.WithTransactionTracking(ctx), &atomic.Int32{}
		gate := newLanes(func() {})
		deps.lanes, deps.ListenerRefresh = gate, &ListenerRefresh{}
		deps.ListenerRefresh.Bind(func(refresh context.Context) {
			runs.Add(1)
			if !gate.statements.TryLock() {
				t.Errorf("the listener was asked again with the outbox's gate held")
			} else {
				gate.statements.Unlock()
			}
			_, release := store.HoldsConnection(scan)
			defer release()
			if _, err := deps.Store.Pool.Exec(refresh, `update route_roster set sent_at = now(), expires_at = now() + interval '1 hour'`); err != nil {
				t.Errorf("renew the listener record while another batch of the scan holds a connection: %v", err)
			}
		})
		return deps, scan, runs
	}
	t.Run("the batch's read", func(t *testing.T) {
		route := "session:session-listed"
		database := seed(t, &route)
		comment(t, database)
		publisher := &recordingPublisher{}
		deps, scan, runs := gated(t, Deps{Store: database, Publisher: publisher})
		if _, err := scanBatch(scan, deps, unfenced{}, holding(t, deps).instance); err != nil {
			t.Fatalf("scan batch: %v", err)
		}
		if sent := topicsNaming(publisher, "session-listed"); runs.Load() == 0 || len(sent) != 1 {
			t.Fatalf("the batch asked the listener %d times and sent session-listed %v; want it asked and TT's event sent", runs.Load(), sent)
		}
	})
	t.Run("a role forward's holder", func(t *testing.T) {
		route := "role:reviewer"
		database := seed(t, &route)
		comment(t, database)
		publisher := &recordingPublisher{}
		deps, scan, runs := gated(t, withHeldRoles(Deps{Store: database, Publisher: publisher}))
		if _, err := scanBatch(scan, deps, unfenced{}, holding(t, deps).instance); err != nil {
			t.Fatalf("scan batch: %v", err)
		}
		if forwarded := topicsNaming(publisher, contracts.RoleTopicPrefix+"reviewer"); runs.Load() == 0 || len(forwarded) != 1 {
			t.Fatalf("the role forward asked the listener %d times and forwarded %v; want it asked and TT's event forwarded to %s", runs.Load(), forwarded, roleHolder)
		}
	})
	t.Run("the kept copy's handover", func(t *testing.T) {
		deps, scan, runs := gated(t, Deps{Store: seed(t, nil)})
		_, sees, err := holderSeesTheCopy(scan, deps, roleHolder, contracts.Envelope{EventID: "dispatch-9", Payload: `{"project":"TT"}`})
		if err != nil || !sees || runs.Load() == 0 {
			t.Fatalf("the handover asked the listener %d times and answered %v, %v; want it asked and %s shown TT's kept copy", runs.Load(), sees, err, roleHolder)
		}
	})
}

// TestAnExpiredListBacksOffOnlyTheCopiesThatDependOnIt holds the back-off of a sight the outbox
// could not read to the pairs whose verdict depends on a listener list (access.SightListSQL,
// lookups.sees): one batch holds an event of public PUB, routed to session-member, the session of
// a full member, and an event of private TT, routed to session-listed, tied to nobody, whose list
// the stored record names TT in. The record has expired and the listener does not answer the
// refresh. PUB's copy depends on no list, so it is sent in that pass and its event published; TT's
// depends on session-listed's list, so it is sent nothing and its event backs off.
func TestAnExpiredListBacksOffOnlyTheCopiesThatDependOnIt(t *testing.T) {
	const (
		listed = "session-listed"
		member = "session-member"
	)
	ctx := context.Background()
	database := storetest.Open(t)
	route := "session:" + listed
	seedIssue(t, database, "T-1", &route)
	tieSession(t, database, member, "member@example.com")
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`update projects set visibility = 'private' where key = 'TT'`, nil},
		{`insert into people (email, member_kind) values ('member@example.com', 'full')`, nil},
		{`insert into projects (key, name, default_assignee) values ('PUB', 'Public', 'alice')`, nil},
		{`insert into issues (key, project_key, number, title, route, created_by, rank)
			values ('PUB-1', 'PUB', 1, 'Public issue', $1, '{"kind":"user","id":"alice"}', 'V')`, []any{"session:" + member}},
		{`insert into route_roster (answered, sessions, sent_at, expires_at)
			values (true, $1, now() - interval '1 hour', now() - interval '1 minute')`, []any{`[{"session_id":"` + listed + `","projects":["TT"]}]`}},
	} {
		if _, err := database.Pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("seed %q: %v", statement.sql, err)
		}
	}
	broker, alice := events.NewBroker(), model.Actor{Kind: "user", ID: "alice"}
	comment := func(issue string) model.Event {
		t.Helper()
		commentID := seedComment(t, database, issue, alice, "Is the order right?", nil)
		return appendEvent(t, database, broker, model.Event{
			IssueKey: new(issue), Type: "comment.created", Actor: alice,
			Payload: model.CommentEventPayload{Comment: model.Comment{ID: commentID, IssueKey: new(issue), Body: "Is the order right?"}},
		})
	}
	public, private := comment("PUB-1"), comment("T-1")
	runs, listener := &atomic.Int32{}, &ListenerRefresh{}
	listener.Bind(func(context.Context) { runs.Add(1) })
	publisher := &recordingPublisher{}
	deps := Deps{Store: database, Publisher: publisher, ListenerRefresh: listener}
	if _, err := scanBatch(ctx, deps, unfenced{}, holding(t, deps).instance); err != nil {
		t.Fatalf("scan batch: %v", err)
	}
	settled := func(id int64) (published, backedOff bool) {
		t.Helper()
		if err := database.Pool.QueryRow(ctx, `select published_at is not null, next_attempt_at is not null from events where id = $1`,
			id).Scan(&published, &backedOff); err != nil {
			t.Fatalf("read event %d: %v", id, err)
		}
		return published, backedOff
	}
	if published, backedOff := settled(public.ID); len(topicsNaming(publisher, member)) != 1 || !published || backedOff {
		t.Fatalf("PUB's event, whose copy to %s depends on no list: sent %v, published %v, backed off %v; want it sent and published in the pass",
			member, topicsNaming(publisher, member), published, backedOff)
	}
	if published, backedOff := settled(private.ID); len(topicsNaming(publisher, listed)) != 0 || published || !backedOff {
		t.Fatalf("TT's event, whose copy to %s depends on the expired list: sent %v, published %v, backed off %v; want nothing sent and the event backed off",
			listed, topicsNaming(publisher, listed), published, backedOff)
	}
	if runs.Load() == 0 {
		t.Fatalf("the batch judged TT's copy by an expired list with no refresh")
	}
}
