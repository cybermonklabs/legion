package outbox

// rls_withhold_test.go holds the outbox rows of plan.md (INFRA-6168, "The outbox and write rows",
// O1-O6): before the outbox publishes to a session's own subject, a role's holder or a held
// fallback copy, it asks whether the session sees the event's project (plan D11, "Outbox"), and
// withholds the copy, logged at Info, when it does not. The write rows of the same test name are
// package api's (rls_session_writes_test.go).

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// The people and sessions of the outbox rows. The project is seedIssue's TT, made private.
const (
	// withholdOutsider is a full member TT's access list names only in a row's visible arm.
	withholdOutsider = "outsider@rls.example"
	// withholdSession is a session tied to withholdOutsider: it sees TT exactly when the list names
	// that person.
	withholdSession = "session-outside"
	// withholdListed is a session tied to nobody whose listener list names TT: it sees TT while
	// the stored listener record that lists it has not expired.
	withholdListed = "session-listed"
	// withholdMember is a session tied to withholdMemberLogin, a full member.
	withholdMember      = "session-member"
	withholdMemberLogin = "member@rls.example"
)

// withholdAlice is the person whose writes the rows' events record.
var withholdAlice = model.Actor{Kind: "user", ID: "alice"}

// withholdExec runs each statement on database, failing t on the first that fails.
func withholdExec(t *testing.T, database *store.Store, statements ...string) {
	t.Helper()
	for _, statement := range statements {
		if _, err := database.Pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
}

// seedWithholdProject is seedIssue's T-1, routed to route, of TT made private, with
// withholdOutsider a full member whom TT's list names when visible, and withholdSession tied to
// that person, as a broker token that proved the session records it (session_owners).
func seedWithholdProject(t *testing.T, route *string, visible bool) *store.Store {
	t.Helper()
	database := storetest.Open(t)
	seedIssue(t, database, "T-1", route)
	withholdExec(t, database,
		`update projects set visibility = 'private' where key = 'TT'`,
		`insert into people (email, member_kind) values ('`+withholdOutsider+`', 'full')`,
		`insert into session_owners (session_id, login) values ('`+withholdSession+`', '`+withholdOutsider+`')`,
	)
	if visible {
		withholdExec(t, database, `insert into project_access (project_key, principal_kind, principal, added_by)
			values ('TT', 'person', '`+withholdOutsider+`', '{"kind":"user","id":"alice"}')`)
	}
	return database
}

// listenerRecord stores the listener's answer listing TT for withholdListed, expired a minute ago
// when expired and fresh for an hour otherwise.
func listenerRecord(t *testing.T, database *store.Store, expired bool) {
	t.Helper()
	expires := `now() + interval '1 hour'`
	if expired {
		expires = `now() - interval '1 minute'`
	}
	withholdExec(t, database, `insert into route_roster (answered, sessions, sent_at, expires_at)
		values (true, '[{"session_id":"`+withholdListed+`","projects":["TT"]}]', now() - interval '1 hour', `+expires+`)`)
}

// commentOnT1 writes a comment of alice's on T-1 and appends its event.
func commentOnT1(t *testing.T, database *store.Store, broker *events.Broker) (string, model.Event) {
	t.Helper()
	commentID := seedComment(t, database, "T-1", withholdAlice, "Is the order right?", nil)
	return commentID, appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: withholdAlice,
		Payload: model.CommentEventPayload{Comment: model.Comment{ID: commentID, IssueKey: new("T-1"), Body: "Is the order right?"}},
	})
}

// scanOnce runs one batch of the outbox over deps, as its scan does, under a hold of its own.
func scanOnce(t *testing.T, deps Deps) {
	t.Helper()
	if _, err := scanBatch(context.Background(), deps, unfenced{}, holding(t, deps).instance); err != nil {
		t.Fatalf("scan batch: %v", err)
	}
}

// sentOn is how many envelopes publisher was given on topic.
func sentOn(publisher *recordingPublisher, topic string) int {
	count := 0
	for _, item := range publisher.all() {
		if item.Topic == topic {
			count++
		}
	}
	return count
}

// ownerTopicSent reports whether publisher was given an envelope on T-1's own topic family.
func ownerTopicSent(publisher *recordingPublisher) bool {
	for _, item := range publisher.all() {
		if strings.HasPrefix(item.Topic, "notifications.dispatch.issue.T-1.") {
			return true
		}
	}
	return false
}

// eventOutcome reads whether event id was published, and whether it waits on a back-off.
func eventOutcome(t *testing.T, database *store.Store, id int64) (published, backedOff bool) {
	t.Helper()
	if err := database.Pool.QueryRow(context.Background(), `select published_at is not null, next_attempt_at is not null from events where id = $1`,
		id).Scan(&published, &backedOff); err != nil {
		t.Fatalf("read event %d: %v", id, err)
	}
	return published, backedOff
}

// withheldLogged reports whether recorder holds an Info record naming session and project among
// its attributes: the line a withheld copy is logged with.
func withheldLogged(recorder *logtest.Recorder, session, project string) bool {
	for _, record := range recorder.Records() {
		if record.Level != slog.LevelInfo {
			continue
		}
		var names, of bool
		record.Attrs(func(attr slog.Attr) bool {
			value := attr.Value.String()
			names = names || value == session
			of = of || value == project
			return true
		})
		if names && of {
			return true
		}
	}
	return false
}

// withholdDestination is one way an event of T-1 reaches withholdSession's own subject.
type withholdDestination struct {
	label string
	route *string
	// claim gives T-1's claim to withholdSession before the event.
	claim bool
	// arrange writes what the destination reads and appends the event.
	arrange func(t *testing.T, database *store.Store, broker *events.Broker) model.Event
}

// withholdDestinations is the follower (O1) and every other destination of a session's own
// subject (O1b).
func withholdDestinations() (follower withholdDestination, others []withholdDestination) {
	outside := model.Actor{Kind: "session", ID: withholdSession}
	sessionRoute := "session:" + withholdSession
	comment := func(t *testing.T, database *store.Store, broker *events.Broker) model.Event {
		_, event := commentOnT1(t, database, broker)
		return event
	}
	follower = withholdDestination{label: "follower", arrange: func(t *testing.T, database *store.Store, broker *events.Broker) model.Event {
		askID := seedAsk(t, database, "T-1", outside, "Ship it?")
		return appendEvent(t, database, broker, model.Event{IssueKey: new("T-1"), Type: "ask.answered", Actor: withholdAlice, Payload: model.Ask{
			ID: askID, IssueKey: new("T-1"), Question: "Ship it?", State: "answered",
		}})
	}}
	others = []withholdDestination{
		{label: "route_session", route: &sessionRoute, arrange: comment},
		{label: "claimant", claim: true, arrange: comment},
		{label: "primary_document_author", arrange: func(t *testing.T, database *store.Store, broker *events.Broker) model.Event {
			makePrimary(t, database, seedDocument(t, database, new("T-1"), "spec", outside, []model.Actor{outside}))
			return comment(t, database, broker)
		}},
		{label: "author_route", arrange: func(t *testing.T, database *store.Store, broker *events.Broker) model.Event {
			answered := seedMessage(t, database, "T-1", outside, "Status?", nil)
			reply := seedMessage(t, database, "T-1", withholdAlice, "Fine", &answered)
			return appendEvent(t, database, broker, model.Event{IssueKey: new("T-1"), Type: "message.created", Actor: withholdAlice,
				Payload: model.MessageEventPayload{Message: model.Message{ID: reply, IssueKey: new("T-1"), Body: "Fine", InReplyTo: &answered}}})
		}},
		{label: "mentioned_session", arrange: func(t *testing.T, database *store.Store, broker *events.Broker) model.Event {
			// A reply on the session's thread that mentions it, whose mention failed: the outbox
			// sends the session the copy under the mention's key (mentionDeliveries.fallbackKeys).
			root := seedComment(t, database, "T-1", outside, "Root", nil)
			reply := seedComment(t, database, "T-1", withholdAlice, "Over to you.", &root)
			seedMentionDelivery(t, database, reply, withholdSession, "failed")
			return appendEvent(t, database, broker, model.Event{IssueKey: new("T-1"), Type: "comment.created", Actor: withholdAlice,
				Payload: map[string]any{"id": reply, "reply_to": root, "body": "Over to you.", "mentions": mentionsOf(withholdSession)}})
		}},
		{label: "previous_claimant", arrange: func(t *testing.T, database *store.Store, broker *events.Broker) model.Event {
			return appendEvent(t, database, broker, model.Event{IssueKey: new("T-1"), Type: "issue.claimed",
				Actor:   model.Actor{Kind: "session", ID: "session-taker"},
				Payload: map[string]any{"key": "T-1", "previous_claim": map[string]any{"actor": map[string]any{"kind": "session", "id": withholdSession}}}})
		}},
	}
	return follower, others
}

// requireWithheld runs destination's event through one batch in both arms: TT's list names the
// session's person (visible), the control, and does not (hidden). Hidden, the session's subject is
// sent nothing, the line naming the session and TT is logged at Info, and the event is published
// to its owner's topic; visible, the session's subject is sent the event once.
func requireWithheld(t *testing.T, destination withholdDestination) {
	t.Helper()
	eachArm(t, func(t *testing.T, visible bool) {
		recorder := &logtest.Recorder{}
		logtest.SetDefault(t, recorder)
		database := seedWithholdProject(t, destination.route, visible)
		if destination.claim {
			claimIssue(t, database, "T-1", model.Actor{Kind: "session", ID: withholdSession})
		}
		broker := events.NewBroker()
		event := destination.arrange(t, database, broker)
		publisher := &recordingPublisher{}
		scanOnce(t, Deps{Store: database, Publisher: publisher, Broker: broker})
		reached := sentOn(publisher, contracts.LegacyAgentSubject(withholdSession))
		published, _ := eventOutcome(t, database, event.ID)
		switch {
		case visible && reached != 1:
			t.Fatalf("%s, TT's list naming the session's person: the session was sent the event %d times, want once: the row proves nothing", destination.label, reached)
		case !visible && reached != 0:
			t.Errorf("%s, TT's list not naming the session's person: the session was sent the event %d times, want it withheld", destination.label, reached)
		case !visible && !withheldLogged(recorder, withholdSession, "TT"):
			t.Errorf("%s: the withheld copy logged no Info line naming %s and TT", destination.label, withholdSession)
		case !visible && (!published || !ownerTopicSent(publisher)):
			t.Errorf("%s: the event with its session copy withheld was published %v, its owner's topic sent %v; want both", destination.label, published, ownerTopicSent(publisher))
		}
	})
}

// eachArm runs row twice, each a subtest of t's: once with TT's list naming the person of the
// session it judges (visible), the control in the same run, and once not (hidden).
func eachArm(t *testing.T, row func(t *testing.T, visible bool)) {
	t.Helper()
	for _, visible := range []bool{true, false} {
		arm := "hidden"
		if visible {
			arm = "visible"
		}
		t.Run(arm, func(t *testing.T) { row(t, visible) })
	}
}

// TestOutboxAndRoutesWithholdFromHiddenSessions (plan R4, D11 "Outbox"): the outbox withholds every
// copy bound for a session that does not see the event's project, a role forward to such a holder
// and a held fallback copy, logging each at Info, publishes to a session whose listener list names
// the project, refreshes an expired list before it judges by it, and with no private project and no
// limited member withholds nothing and refreshes nothing (R5, O6). The write rows of this name are
// package api's.
func TestOutboxAndRoutesWithholdFromHiddenSessions(t *testing.T) {
	// Not parallel: it swaps the default logger (logtest.SetDefault).
	follower, others := withholdDestinations()
	t.Run("O1_follower_outside_priv_is_withheld_and_logged", func(t *testing.T) {
		requireWithheld(t, follower)
	})
	t.Run("O1b_each_session_destination_outside_priv_is_withheld_and_logged", func(t *testing.T) {
		for _, destination := range others {
			t.Run(destination.label, func(t *testing.T) { requireWithheld(t, destination) })
		}
	})
	t.Run("O2_role_forward_to_a_holder_outside_priv_is_withheld", func(t *testing.T) {
		eachArm(t, func(t *testing.T, visible bool) {
			recorder := &logtest.Recorder{}
			logtest.SetDefault(t, recorder)
			route := "role:reviewer"
			database := seedWithholdProject(t, &route, visible)
			withholdExec(t, database, `insert into session_owners (session_id, login) values ('`+roleHolder+`', '`+withholdOutsider+`')`)
			broker := events.NewBroker()
			_, event := commentOnT1(t, database, broker)
			publisher := &recordingPublisher{}
			scanOnce(t, withHeldRoles(Deps{Store: database, Publisher: publisher, Broker: broker}))
			forwarded := sentOn(publisher, contracts.RoleTopicPrefix+"reviewer")
			published, _ := eventOutcome(t, database, event.ID)
			switch {
			case visible && forwarded != 1:
				t.Fatalf("the role's holder, whose person TT's list names, was forwarded the event %d times, want once: the row proves nothing", forwarded)
			case !visible && forwarded != 0:
				t.Errorf("the role's holder, whose person does not see TT, was forwarded the event %d times, want it withheld", forwarded)
			case !visible && !withheldLogged(recorder, roleHolder, "TT"):
				t.Errorf("the withheld forward logged no Info line naming %s and TT", roleHolder)
			case !visible && !published:
				t.Errorf("the event whose forward was withheld was not published, want the forward counted as reached")
			}
		})
	})
	t.Run("O3_fallback_copy_to_a_session_outside_priv_is_withheld", func(t *testing.T) {
		eachArm(t, func(t *testing.T, visible bool) {
			recorder := &logtest.Recorder{}
			logtest.SetDefault(t, recorder)
			ctx := context.Background()
			database := seedWithholdProject(t, nil, visible)
			broker := events.NewBroker()
			commentID, event := commentOnT1(t, database, broker)
			destination := contracts.LegacyAgentSubject(withholdSession)
			if _, err := database.Pool.Exec(ctx, `update events set published_at = now() where id = $1`, event.ID); err != nil {
				t.Fatalf("publish the comment's event: %v", err)
			}
			if _, err := database.Pool.Exec(ctx, `insert into comment_fallback_copies (comment_id, destination, event_id, session_id) values ($1, $2, $3, $4)`,
				commentID, destination, event.ID, withholdSession); err != nil {
				t.Fatalf("hold the session's copy: %v", err)
			}
			publisher := &recordingPublisher{}
			deps := Deps{Store: database, Publisher: publisher, Broker: broker}
			publishFallbackCopies(ctx, deps, unfenced{}, holding(t, deps).instance)
			row, sent := readHeldCopy(t, database, commentID, destination), sentOn(publisher, destination)
			switch {
			case visible && (!row.settled || !row.published || sent != 1):
				t.Fatalf("the fallback copy of a session that sees TT: settled %v, published %v, sent %d; want it published once: the row proves nothing", row.settled, row.published, sent)
			case !visible && (!row.settled || row.published || sent != 0):
				t.Errorf("the fallback copy of a session that does not see TT: settled %v, published %v, sent %d; want it settled with nothing sent", row.settled, row.published, sent)
			case !visible && !withheldLogged(recorder, withholdSession, "TT"):
				t.Errorf("the withheld fallback copy logged no Info line naming %s and TT", withholdSession)
			}
		})
	})
	t.Run("O4_session_whose_listener_list_names_priv_is_published", func(t *testing.T) {
		route := "session:" + withholdListed
		database := seedWithholdProject(t, &route, false)
		listenerRecord(t, database, false)
		broker := events.NewBroker()
		_, event := commentOnT1(t, database, broker)
		publisher := &recordingPublisher{}
		scanOnce(t, Deps{Store: database, Publisher: publisher, Broker: broker})
		if sent := sentOn(publisher, contracts.LegacyAgentSubject(withholdListed)); sent != 1 {
			t.Errorf("%s, whose fresh listener list names TT, was sent TT's event %d times, want once", withholdListed, sent)
		}
		if published, _ := eventOutcome(t, database, event.ID); !published {
			t.Errorf("the event reaching %s was not published", withholdListed)
		}
	})
	t.Run("O5_an_expired_list_is_refreshed_before_the_verdict", func(t *testing.T) {
		for _, renews := range []bool{true, false} {
			arm := "a_refresh_that_renews_the_record"
			if !renews {
				arm = "a_refresh_that_leaves_the_record_expired"
			}
			t.Run(arm, func(t *testing.T) {
				route := "session:" + withholdListed
				database := seedWithholdProject(t, &route, false)
				listenerRecord(t, database, true)
				broker := events.NewBroker()
				_, event := commentOnT1(t, database, broker)
				refresh, runs := countedRefresh(t, database, renews)
				publisher := &recordingPublisher{}
				scanOnce(t, Deps{Store: database, Publisher: publisher, Broker: broker, ListenerRefresh: refresh})
				sent := sentOn(publisher, contracts.LegacyAgentSubject(withholdListed))
				published, backedOff := eventOutcome(t, database, event.ID)
				if runs.Load() == 0 {
					t.Errorf("the batch judged %s by an expired listener list with no refresh", withholdListed)
				}
				switch {
				case renews && (sent != 1 || !published):
					t.Errorf("after a refresh that renewed the record: %s sent %d times, published %v; want it sent TT's event once", withholdListed, sent, published)
				case !renews && (sent != 0 || published || !backedOff):
					t.Errorf("with the record still expired: %s sent %d times, published %v, backed off %v; want nothing sent and the event backed off",
						withholdListed, sent, published, backedOff)
				}
			})
		}
	})
	t.Run("O6_all_public_nothing_withheld_and_no_refresh", func(t *testing.T) {
		recorder := &logtest.Recorder{}
		logtest.SetDefault(t, recorder)
		route := "session:" + withholdListed
		database := storetest.Open(t)
		seedIssue(t, database, "T-1", &route)
		withholdExec(t, database,
			`insert into people (email, member_kind) values ('`+withholdMemberLogin+`', 'full')`,
			`insert into session_owners (session_id, login) values ('`+withholdMember+`', '`+withholdMemberLogin+`')`,
		)
		claimIssue(t, database, "T-1", model.Actor{Kind: "session", ID: withholdMember})
		listenerRecord(t, database, true)
		broker := events.NewBroker()
		_, event := commentOnT1(t, database, broker)
		refresh, runs := countedRefresh(t, database, false)
		publisher := &recordingPublisher{}
		scanOnce(t, Deps{Store: database, Publisher: publisher, Broker: broker, ListenerRefresh: refresh})
		published, _ := eventOutcome(t, database, event.ID)
		for _, session := range []string{withholdListed, withholdMember} {
			if sent := sentOn(publisher, contracts.LegacyAgentSubject(session)); sent != 1 {
				t.Errorf("with no private project, %s was sent the event %d times, want once", session, sent)
			}
			if withheldLogged(recorder, session, "TT") {
				t.Errorf("with no private project, a copy to %s was logged as withheld", session)
			}
		}
		if !published || runs.Load() != 0 {
			t.Errorf("with no private project: published %v, the listener refreshed %d times; want published and no refresh", published, runs.Load())
		}
	})
}

// countedRefresh is a bound refresh counting its runs, which renews the stored listener record
// when renews.
func countedRefresh(t *testing.T, database *store.Store, renews bool) (*ListenerRefresh, *atomic.Int32) {
	t.Helper()
	runs, refresh := &atomic.Int32{}, &ListenerRefresh{}
	refresh.Bind(func(ctx context.Context) {
		runs.Add(1)
		if !renews {
			return
		}
		if _, err := database.Pool.Exec(ctx, `update route_roster set sent_at = now(), expires_at = now() + interval '1 hour'`); err != nil {
			t.Errorf("renew the listener record: %v", err)
		}
	})
	return refresh, runs
}
