package outbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/session"
)

// What an event still owes, as the record writes it to events.owed_keys, for each kind of
// destination its list can hold: the owner's topic family as model.OwedTopicFamily; a session's
// own subject as itself; a role route as its role topic, whichever holder it resolved to, so a
// holder that changes before the next record is still asked about; a copy that waits on a mention
// as its key; and nothing for a destination already reached, by an earlier batch or this one, a
// copy a fallback row owns, or a role route reached with nothing to send. An event whose recipient
// read failed owes model.OwedAnySession besides; one whose row scanned but whose payload does not
// decode owes nothing, as does a row that did not scan (TestAnEventTheOutboxCannotReadOwesNothing
// records both). Whether the record writes the answer is the outcome's owedKnown, so an answer
// that owes nothing may be nil or empty alike.
func TestOwedKeysRecordWhatEachDestinationStillOwes(t *testing.T) {
	t.Parallel()
	topic := "notifications.dispatch.issue.T-1.message.created"
	s1 := contracts.LegacyAgentSubject("s1")
	roleTopic := contracts.RoleTopicPrefix + "reviewer"
	for _, row := range []struct {
		name    string
		event   batchEvent
		reached []string
		want    []string
	}{
		{
			name:  "the topic family and a session's subject, neither reached",
			event: batchEvent{destinationsOf: []destination{{subject: topic}, {subject: s1}}},
			want:  []string{model.OwedTopicFamily, s1},
		},
		{
			name: "a role route resolved to a holder, recorded as its role topic",
			event: batchEvent{destinationsOf: []destination{
				{subject: topic},
				{subject: roleTopic, role: "reviewer", forward: &roleForward{holder: "ses_holder"}},
			}},
			reached: []string{topic},
			want:    []string{roleTopic},
		},
		{
			name:  "a role route with no holder known",
			event: batchEvent{destinationsOf: []destination{{subject: roleTopic, role: "reviewer"}}},
			want:  []string{roleTopic},
		},
		{
			name:  "a copy that waits on a mention",
			event: batchEvent{destinationsOf: []destination{{subject: s1, waiting: true}}},
			want:  []string{s1},
		},
		{
			name: "reached by an earlier batch, and by this one",
			event: batchEvent{
				delivered:      map[string]struct{}{topic: {}},
				destinationsOf: []destination{{subject: topic}, {subject: s1}},
			},
			reached: []string{s1},
			want:    []string{},
		},
		{
			name:  "a copy a fallback row owns",
			event: batchEvent{destinationsOf: []destination{{subject: s1, heldAsARow: true}}},
			want:  []string{},
		},
		{
			name: "a role route reached with nothing to send",
			event: batchEvent{destinationsOf: []destination{
				{subject: roleTopic, role: "reviewer", forward: &roleForward{holder: "ses_holder", skip: "the holder wrote it"}},
			}},
			want: []string{},
		},
		{
			name:  "a recipient read that failed",
			event: batchEvent{incomplete: true, destinationsOf: []destination{{subject: topic}}},
			want:  []string{model.OwedAnySession, model.OwedTopicFamily},
		},
		{
			name: "an event whose payload does not decode",
			event: batchEvent{
				pendingEvent:   pendingEvent{failure: errors.New("the payload does not decode")},
				destinationsOf: []destination{{subject: topic}},
			},
			want: []string{},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			got := owedKeys(&row.event, row.reached)
			if !slices.Equal(got, row.want) {
				t.Fatalf("owedKeys = %#v, want %#v", got, row.want)
			}
		})
	}
}

// An earlier event of T-1 is routed to role:reviewer, whose holder is not answering: its topic
// family is reached, its forward waits for the holder, and the event backs off, its record owing
// the role's topic. The role then moves to a live session, next, before the event's next record. A
// later comment of T-1 mentions next. next's copy waits behind the earlier event, on the fallback
// pass's road (copyHolder) as on the API's (TestTheAPIsMentionWaitsForARoleRouteItsSessionHoldsNow),
// because the event's next try forwards it to the role's holder now, next.
//
// The first row moves the role after a record made as of the comment, while the old holder still
// resolved, and the move writes no event of T-1 (a role's claim is the session registry's): that
// record is current, so only what it owes decides, and it owes the role's topic, whoever holds the
// role now. A record that named the holder it resolved to would owe the old holder alone, and
// next's copy would wait behind nothing while the earlier event's retry reaches next first. The
// second row moves the role before the comment, so the record made as of the comment reads next as
// the holder, and the run shows the retry: next's first copy is the earlier event.
//
// Controls in the same run: a copy for a session that does not hold the role waits for nothing,
// and the outbox's own order holds the comment's forward to next behind the earlier event.
func TestARoleHolderChangeKeepsAMentionCopyBehindAnEarlierForward(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// afterRecord moves the role once the record made as of the comment is written.
		afterRecord bool
	}{
		{name: "the role moves after the record made as of the comment, with no event of the issue", afterRecord: true},
		{name: "the role moves before the comment", afterRecord: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			t.Logf("start %s", time.Now().UTC().Format(time.RFC3339Nano))
			ctx := context.Background()
			rig := newRoleRig(t)
			const role, stale, next = "reviewer", "ses_stale_holder", "ses_next_holder"
			rig.claimRegisteredAt(t, role, stale, session.LiveWindow+time.Minute)
			recv := rig.receiver(t, next)
			alice := model.Actor{Kind: "user", ID: "alice"}
			earlier := rig.routedEvent(t, "T-1", role, alice)

			holding(t, rig.deps()).scan(ctx, unfenced{})
			var owed []string
			var published bool
			if err := rig.database.Pool.QueryRow(ctx, `
				select coalesce(owed_keys, array['<null>']), published_at is not null from events where id = $1
			`, earlier.ID).Scan(&owed, &published); err != nil {
				t.Fatalf("read the earlier event: %v", err)
			}
			t.Logf("after the first scan: event %d published %v, owed_keys %v", earlier.ID, published, owed)
			if published {
				t.Fatal("the earlier event was published, want it backed off on the holder that is not answering")
			}

			if !tc.afterRecord {
				rig.claim(t, role, next, session.SessionEntry{SelfSubscribed: true})
			}
			body := "@session:" + next + " look"
			commentID := seedComment(t, rig.database, "T-1", alice, body, nil)
			comment := appendEvent(t, rig.database, rig.broker, model.Event{
				IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
				Payload: model.CommentEventPayload{Comment: model.Comment{
					ID: commentID, IssueKey: new("T-1"), Body: body,
					Mentions: []model.Mention{{Target: "session:" + next, Delivery: "steer"}},
				}},
			})
			// A scan while the earlier event is not due records it again (owedAgain) as of the
			// comment, so the rule below reads its role route, not a record older than the comment
			// (owed_seq behind T-1's last_seq), which would hold every session's copy. Where next
			// holds the role already, the same scan keys the earlier event's route on next, so the
			// outbox's own order holds the comment's forward to next behind it.
			holding(t, rig.deps()).scan(ctx, unfenced{})
			owed, _, _, _ = eventRecord(t, rig.database, earlier.ID)
			var lastSeq int
			if err := rig.database.Pool.QueryRow(ctx, `select last_seq from issues where key = 'T-1'`).Scan(&lastSeq); err != nil {
				t.Fatalf("read T-1's last_seq: %v", err)
			}
			t.Logf("after the record as of the comment: the earlier event owes %v; T-1's last_seq %d", owed, lastSeq)
			if tc.afterRecord {
				rig.claim(t, role, next, session.SessionEntry{SelfSubscribed: true})
				var after int
				if err := rig.database.Pool.QueryRow(ctx, `select last_seq from issues where key = 'T-1'`).Scan(&after); err != nil || after != lastSeq {
					t.Fatalf("the role's move wrote an event of T-1 (last_seq %d, was %d; %v), want none", after, lastSeq, err)
				}
			}
			forNext, err := copyHolder(ctx, rig.deps(), comment.ID, contracts.LegacyAgentSubject(next), next)
			if err != nil {
				t.Fatalf("read the rule for %s: %v", next, err)
			}
			forOther, err := copyHolder(ctx, rig.deps(), comment.ID, contracts.LegacyAgentSubject("ses_other"), "ses_other")
			if err != nil {
				t.Fatalf("read the rule for ses_other: %v", err)
			}
			t.Logf("the rule: the copy for %s waits behind %s; the copy for ses_other waits behind %s", next, describeHolder(forNext), describeHolder(forOther))
			if forOther != nil {
				t.Fatalf("control: the copy for ses_other, which holds no role, waits behind %s, want nothing", describeHolder(forOther))
			}
			if forNext == nil || forNext.EventID != earlier.ID {
				t.Fatalf("the copy of comment %d for %s waits behind %s, want event %d, whose route reaches %s as the role's holder now",
					comment.ID, next, describeHolder(forNext), earlier.ID, next)
			}
			if tc.afterRecord {
				t.Logf("end %s", time.Now().UTC().Format(time.RFC3339Nano))
				return
			}
			if got := recv.receivedNow(t, rig.client); len(got) != 0 {
				t.Fatalf("control: %s took %d copies while the earlier event was not due, want none", next, len(got))
			}
			dueNow(t, rig.database, earlier.ID)
			holding(t, rig.deps()).scan(ctx, unfenced{})
			first := handedNow(t, recv, "the earlier event's retry")
			t.Logf("%s first took event %s (earlier %d, comment %d)", next, first.SourceEventID, earlier.ID, comment.ID)
			t.Logf("end %s", time.Now().UTC().Format(time.RFC3339Nano))
			if first.SourceEventID != fmt.Sprint(earlier.ID) {
				t.Fatalf("control: %s first took event %s, want the earlier event %d forwarded on its retry", next, first.SourceEventID, earlier.ID)
			}
		})
	}
}

// eventRecord is what the record left on an event: its owed_keys, its back-off and attempts, and
// the row version (xmin), which moves on any write of the row.
func eventRecord(t *testing.T, database *store.Store, id int64) (owed []string, next *time.Time, attempts int, version string) {
	t.Helper()
	if err := database.Pool.QueryRow(context.Background(), `
		select coalesce(owed_keys, array['<null>']), next_attempt_at, attempt_count, xmin::text from events where id = $1
	`, id).Scan(&owed, &next, &attempts, &version); err != nil {
		t.Fatalf("read event %d: %v", id, err)
	}
	return owed, next, attempts, version
}

// An event that is not due is read by every batch of its owner as a blocker, and the record
// writes what it owes now and nothing else: a stale owed_keys - here a session it no longer goes
// to - is replaced by its list as this batch read it, while its back-off and attempt count stay as
// they were. A batch that finds what it owes unchanged writes nothing to it: the row's version is
// the same after it, which is the control that the test can tell a write from none.
func TestAnEventNotDueHasWhatItOwesWrittenAgain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := storetest.Open(t)
	broker := events.NewBroker()
	route := "session:s1"
	seedIssue(t, database, "T-1", &route)
	alice := model.Actor{Kind: "user", ID: "alice"}
	earlier := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "message.created", Actor: alice,
		Payload: model.Message{ID: "6b1b1c5e-0d3a-4c38-9b7e-0a5f8a0c0202", IssueKey: new("T-1"), Body: "earlier"},
	})
	publisher := &failingPublisher{topic: "notifications.dispatch.issue.T-1.message.created", answer: errors.New("no responders")}
	deps := Deps{Store: database, Publisher: publisher, Broker: broker}
	holding(t, deps).scan(ctx, unfenced{})
	owed, next, attempts, _ := eventRecord(t, database, earlier.ID)
	if next == nil || attempts == 0 {
		t.Fatalf("the earlier event has next_attempt_at %v and %d attempts, want it backed off", next, attempts)
	}
	t.Logf("backed off: owed_keys %v, next_attempt_at %v, %d attempts", owed, next, attempts)
	if _, err := database.Pool.Exec(ctx, `update events set owed_keys = array['notifications.agent.ses_gone'] where id = $1`, earlier.ID); err != nil {
		t.Fatalf("make the earlier event's owed_keys stale: %v", err)
	}

	commentID := seedComment(t, database, "T-1", alice, "a later comment", nil)
	later := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{Comment: model.Comment{ID: commentID, IssueKey: new("T-1"), Body: "a later comment"}},
	})
	holding(t, deps).scan(ctx, unfenced{})
	owedAfter, nextAfter, attemptsAfter, version := eventRecord(t, database, earlier.ID)
	t.Logf("after a batch that read it not due: owed_keys %v, next_attempt_at %v, %d attempts", owedAfter, nextAfter, attemptsAfter)
	// The route to s1 was reached by the first batch; the topic family alone is still owed.
	want := []string{model.OwedTopicFamily}
	if !slices.Equal(owedAfter, want) {
		t.Fatalf("the earlier event owes %v after a batch read it not due, want %v", owedAfter, want)
	}
	if nextAfter == nil || !nextAfter.Equal(*next) || attemptsAfter != attempts {
		t.Fatalf("the earlier event's back-off moved to %v with %d attempts, want %v and %d: the record writes only what it owes", nextAfter, attemptsAfter, next, attempts)
	}

	dueNow(t, database, later.ID)
	holding(t, deps).scan(ctx, unfenced{})
	if _, _, _, again := eventRecord(t, database, earlier.ID); again != version {
		t.Fatalf("a batch that found the earlier event owing the same moved its row from version %s to %s, want no write", version, again)
	}
}

// An earlier event of T-1 that the outbox cannot read owes nothing, as the spec says, and so holds
// no later copy of a mention: a row whose published_destinations holds a null, which the batch
// reads and cannot publish, and a row that fails the batch's scan - a created_at of infinity,
// which no time.Time holds, as an operator's write or a restore can leave one. Each is backed off
// with the read's failure, and the record writes it owing nothing ('{}') as of T-1's last_seq as
// the batch read it, so s1's copy of a later comment waits behind nothing. Before the batch the
// same event, never recorded (null), holds the copy: the control that the rig sees a hold.
func TestAnEventTheOutboxCannotReadOwesNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, unreadable, failure string
	}{
		{
			name:       "its published_destinations holds a null: read, and not publishable",
			unreadable: `published_destinations = array[null]::text[]`, failure: "published_destinations holds a null",
		},
		{
			name:       "its created_at is infinity: the row does not scan",
			unreadable: `created_at = 'infinity'`, failure: "cannot scan Infinity",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			database := storetest.Open(t)
			broker := events.NewBroker()
			earlier, comment, _ := earlierEventAndComment(t, database, broker)
			if _, err := database.Pool.Exec(ctx, `update events set `+tc.unreadable+` where id = $1`, earlier.ID); err != nil {
				t.Fatalf("make the earlier event unreadable: %v", err)
			}
			s1 := contracts.LegacyAgentSubject("s1")
			if by, err := copyHolder(ctx, Deps{Store: database}, comment.ID, s1, "s1"); err != nil || by == nil || by.EventID != earlier.ID {
				t.Fatalf("control: before any batch s1's copy waits behind %s (%v), want event %d, which no record has written", describeHolder(by), err, earlier.ID)
			}

			holding(t, Deps{Store: database, Publisher: &recordingPublisher{}, Broker: broker}).scan(ctx, unfenced{})
			var owed []string
			var owedSeq, lastSeq *int
			var failure *string
			var published bool
			if err := database.Pool.QueryRow(ctx, `
				select coalesce(e.owed_keys, array['<null>']), e.owed_seq, i.last_seq, e.publish_error, e.published_at is not null
				from events e join issues i on i.key = e.issue_key where e.id = $1
			`, earlier.ID).Scan(&owed, &owedSeq, &lastSeq, &failure, &published); err != nil {
				t.Fatalf("read the earlier event's record: %v", err)
			}
			by, err := copyHolder(ctx, Deps{Store: database}, comment.ID, s1, "s1")
			if err != nil {
				t.Fatalf("read the rule: %v", err)
			}
			t.Logf("after a batch: published %t, publish_error %v, owed_keys %v, owed_seq %v of last_seq %v; s1's copy waits behind %s",
				published, nullable(failure), owed, nullable(owedSeq), nullable(lastSeq), describeHolder(by))
			if published || failure == nil || !strings.Contains(*failure, "read event") || !strings.Contains(*failure, tc.failure) {
				t.Fatalf("the earlier event is published %t with publish_error %v, want it backed off with the read's failure naming %q",
					published, nullable(failure), tc.failure)
			}
			if !slices.Equal(owed, []string{}) || owedSeq == nil || lastSeq == nil || *owedSeq != *lastSeq {
				t.Fatalf("the record left the earlier event owing %v as of owed_seq %v (last_seq %v), want nothing as of the last_seq the batch read",
					owed, nullable(owedSeq), nullable(lastSeq))
			}
			if by != nil {
				t.Fatalf("s1's copy waits behind %s, want nothing: an event the outbox cannot read owes nothing", describeHolder(by))
			}
		})
	}
}

// nullable is what a nullable column read holds, "null" for none.
func nullable[T any](value *T) any {
	if value == nil {
		return "null"
	}
	return *value
}
