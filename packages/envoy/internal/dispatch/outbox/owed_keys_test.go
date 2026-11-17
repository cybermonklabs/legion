package outbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/session"
)

// What an event still owes, as the record writes it to events.owed_keys, for each kind of
// destination its list can hold: the owner's topic family as model.OwedTopicFamily; a session's
// own subject as itself; a role route as its role topic, whichever holder it resolved to, so a
// holder that changes before the next record is still asked about; a copy that waits on a mention
// as its key; and nothing for a destination already reached, by an earlier batch or this one, a
// copy a fallback row owns, or a role route reached with nothing to send. An event whose recipient
// read failed owes model.OwedAnySession besides; one that cannot be read owes nothing.
func TestOwedKeysRecordWhatEachDestinationStillOwes(t *testing.T) {
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
			name: "an event that cannot be read",
			event: batchEvent{
				pendingEvent:   pendingEvent{failure: errors.New("the payload does not decode")},
				destinationsOf: []destination{{subject: topic}},
			},
			want: []string{},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			got := owedKeys(&row.event, row.reached)
			if got == nil || !slices.Equal(got, row.want) {
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
// because the event's next try forwards it to the role's holder now, next - which the run then
// shows: next's first copy is the earlier event.
//
// Controls in the same run: a copy for a session that does not hold the role waits for nothing,
// and the outbox's own order holds the comment's forward to next behind the earlier event.
func TestARoleHolderChangeKeepsAMentionCopyBehindAnEarlierForward(t *testing.T) {
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

	rig.claim(t, role, next, session.SessionEntry{SelfSubscribed: true})
	body := "@session:" + next + " look"
	commentID := seedComment(t, rig.database, "T-1", alice, body, nil)
	comment := appendEvent(t, rig.database, rig.broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{Comment: model.Comment{
			ID: commentID, IssueKey: new("T-1"), Body: body,
			Mentions: []model.Mention{{Target: "session:" + next, Delivery: "steer"}},
		}},
	})
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

	// The outbox's own order: a scan while the earlier event is not due keys its route on next now,
	// so the comment's own forward to next waits behind it.
	holding(t, rig.deps()).scan(ctx, unfenced{})
	time.Sleep(time.Second)
	if got := recv.received(); len(got) != 0 {
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
	if forNext == nil || forNext.EventID != earlier.ID {
		t.Fatalf("the copy of comment %d for %s waits behind %s, want event %d, whose route reaches %s as the role's holder now",
			comment.ID, next, describeHolder(forNext), earlier.ID, next)
	}
}
