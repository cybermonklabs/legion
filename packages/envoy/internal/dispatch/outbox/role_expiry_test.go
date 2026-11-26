package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/retry"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/session"
)

// keptLane is every message the stream holds on role's kept lane, in stream order, each with the
// message id it was stored under, which is what names the attempt a copy was kept for
// (bus.KeptAttemptMsgID).
func keptLane(t *testing.T, client *bus.Client, role string) []bus.StoredMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var lane []bus.StoredMessage
	for seq := uint64(1); ; {
		stored, err := client.NextStoredMessage(ctx, bus.KeptRoleSubject(role), seq)
		if errors.Is(err, natsgo.ErrMsgNotFound) {
			return lane
		}
		if err != nil {
			t.Fatalf("read role %s's kept lane after %d: %v", role, seq, err)
		}
		lane = append(lane, stored)
		seq = stored.Sequence + 1
	}
}

// envelopeOf decodes the envelope a kept copy holds.
func envelopeOf(t *testing.T, stored bus.StoredMessage) contracts.Envelope {
	t.Helper()
	var item contracts.Envelope
	if err := json.Unmarshal(stored.Data, &item); err != nil {
		t.Fatalf("decode the kept copy at %d: %v", stored.Sequence, err)
	}
	return item
}

// expiryNoticeOf is the notice a listener's walk stores when it drops the copy stored on role's
// kept lane at the end of its hour (cmd/listener/role_keep.go, endKeptCopyWithExpiryNotice): the
// copy's event, its attempt as its message id names it, its key and its stream sequence.
func expiryNoticeOf(t *testing.T, role string, stored bus.StoredMessage) bus.KeptCopyExpiry {
	t.Helper()
	item := envelopeOf(t, stored)
	return bus.KeptCopyExpiry{
		EventID: item.EventID, SourceEventID: item.SourceEventID, Role: role,
		Attempt:   bus.KeptCopyAttempt(stored.MsgID, item.DedupeKey, role),
		DedupeKey: item.DedupeKey, Sequence: stored.Sequence,
		KeptAt: stored.Time.UnixMilli(), DroppedAt: stored.Time.Add(time.Hour).UnixMilli(),
	}
}

// expire stores the expiry notice of the copy stored on role's kept lane, as the listener's walk
// stores it, and returns it.
func expire(t *testing.T, client *bus.Client, role string, stored bus.StoredMessage) bus.KeptCopyExpiry {
	t.Helper()
	notice := expiryNoticeOf(t, role, stored)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.PublishKeptCopyExpiry(ctx, notice); err != nil {
		t.Fatalf("store the expiry notice of role %s's copy %d: %v", role, stored.Sequence, err)
	}
	return notice
}

// expiryRow is one outbox_role_copy_expiries row as a test reads it: its keep attempt, the copy's
// stream sequence, its outcome, whether it is settled, its failed sends (attempt_count) and when it
// is due again.
type expiryRow struct {
	attempt  int
	sequence uint64
	outcome  *string
	settled  bool
	failures int
	due      *time.Time
}

// expiryRows is every row the outbox recorded for eventID's copies on role, by attempt.
func expiryRows(t *testing.T, database *store.Store, eventID int64, role string) []expiryRow {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select keep_attempt, copy_sequence, outcome, settled_at is not null, attempt_count, next_attempt_at
		from outbox_role_copy_expiries where event_id = $1 and role = $2 order by keep_attempt
	`, eventID, role)
	if err != nil {
		t.Fatalf("read the expiries of event %d on role %s: %v", eventID, role, err)
	}
	defer rows.Close()
	var found []expiryRow
	for rows.Next() {
		var row expiryRow
		if err := rows.Scan(&row.attempt, &row.sequence, &row.outcome, &row.settled, &row.failures, &row.due); err != nil {
			t.Fatalf("scan an expiry of event %d: %v", eventID, err)
		}
		found = append(found, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the expiries of event %d: %v", eventID, err)
	}
	return found
}

// undeliveredRecords is every role.undelivered event on issue key, oldest first.
func undeliveredRecords(t *testing.T, database *store.Store, key string) []model.Event {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select id, type, actor, payload, notify from events where issue_key = $1 and type = $2 order by id
	`, key, model.RoleUndeliveredEventType)
	if err != nil {
		t.Fatalf("read the role.undelivered events of %s: %v", key, err)
	}
	defer rows.Close()
	var found []model.Event
	for rows.Next() {
		var event model.Event
		var actor, payload []byte
		if err := rows.Scan(&event.ID, &event.Type, &actor, &payload, &event.Notify); err != nil {
			t.Fatalf("scan a role.undelivered event of %s: %v", key, err)
		}
		if err := json.Unmarshal(actor, &event.Actor); err != nil {
			t.Fatalf("decode the actor of event %d: %v", event.ID, err)
		}
		var record model.RoleUndeliveredEventPayload
		if err := json.Unmarshal(payload, &record); err != nil {
			t.Fatalf("decode the payload of event %d: %v", event.ID, err)
		}
		event.Payload = record
		found = append(found, event)
	}
	return found
}

// An outbox copy whose hour ended with no session taking it is kept again, under the attempt the
// listener reads back from its message id, and the session that claims the role before the next
// keep is forwarded the copy: the expiry notice reopens the event's delivery to that role alone.
// At legion main the keep settled the row and nothing reads the notice, so the lane holds the one
// copy and the holder takes nothing.
func TestAnExpiredOutboxCopyIsKeptAgainAndReachesAHolderThatClaimsTheRoleInTime(t *testing.T) {
	ctx := context.Background()
	rig := newRoleRig(t)
	const role, holder = "expiry-kept-again", "ses_expiry_claims_in_time"
	rig.silent(t, contracts.RoleTopicPrefix+role)
	event := rig.routedEvent(t, "T-1", role, model.Actor{Kind: "user", ID: "alice"})
	key := fmt.Sprintf("dispatch-%d", event.ID)
	outbox := holding(t, rig.deps())
	outbox.scan(ctx, unfenced{})
	lane := keptLane(t, rig.client, role)
	if len(lane) != 1 || lane[0].MsgID != bus.KeptMsgID(key, role) {
		t.Fatalf("role %s's kept lane holds %d copies (first id %q), want the scan's one under %q", role, len(lane), msgIDOf(lane), bus.KeptMsgID(key, role))
	}

	expire(t, rig.client, role, lane[0])
	outbox.passKeptExpiries(ctx, unfenced{})

	lane = keptLane(t, rig.client, role)
	if len(lane) != 2 {
		t.Fatalf("role %s's kept lane holds %d copies after the first copy's hour ended, want the copy kept again", role, len(lane))
	}
	again := envelopeOf(t, lane[1])
	if lane[1].MsgID != bus.KeptAttemptMsgID(key, role, 2) || again.DedupeKey != key || again.EventID != key ||
		bus.KeptCopyAttempt(lane[1].MsgID, again.DedupeKey, role) != 2 {
		t.Fatalf("the copy kept again is %q under id %q, want event %s under its own key and id %q (attempt 2)",
			again.EventID, lane[1].MsgID, key, bus.KeptAttemptMsgID(key, role, 2))
	}
	if rows := expiryRows(t, rig.database, event.ID, role); len(rows) != 1 || rows[0].attempt != 1 ||
		rows[0].sequence != lane[0].Sequence || !rows[0].settled || rows[0].outcome == nil || *rows[0].outcome != "kept" {
		t.Fatalf("the outbox recorded %+v, want attempt 1's expiry settled with the copy kept again", rows)
	}

	recv := rig.receiver(t, holder)
	rig.claim(t, role, holder, session.SessionEntry{SelfSubscribed: true})
	expire(t, rig.client, role, lane[1])
	outbox.passKeptExpiries(ctx, unfenced{})

	forward := handedNow(t, recv, "the second copy's expiry with the role claimed")
	if forward.EventID != key || forward.DedupeKey != contracts.RoleForwardDedupeKey(again, holder) {
		t.Fatalf("the holder took event %q under key %q, want event %s under %q", forward.EventID, forward.DedupeKey, key, contracts.RoleForwardDedupeKey(again, holder))
	}
	if lane := keptLane(t, rig.client, role); len(lane) != 2 {
		t.Fatalf("role %s's kept lane holds %d copies, want no third keep once a session holds the role", role, len(lane))
	}
	rows := expiryRows(t, rig.database, event.ID, role)
	if len(rows) != 2 || rows[1].attempt != 2 || !rows[1].settled || rows[1].outcome == nil || *rows[1].outcome != "forwarded" {
		t.Fatalf("the outbox recorded %+v, want attempt 2's expiry settled with the copy forwarded", rows)
	}
	if records := undeliveredRecords(t, rig.database, "T-1"); len(records) != 0 {
		t.Fatalf("T-1 holds role.undelivered events %+v, want none: the holder took the copy", records)
	}
}

// msgIDOf is the message id of the first stored message, for a failure that names it.
func msgIDOf(lane []bus.StoredMessage) string {
	if len(lane) == 0 {
		return ""
	}
	return lane[0].MsgID
}

// A role no session ever holds ends recorded undelivered after maxRoleKeeps keeps, and the outbox
// stops: the third copy's expiry keeps nothing, and a role.undelivered event on the event's own
// issue says so, written by the outbox as a system actor, which notifies nobody and so reaches no
// role - publishing it keeps nothing on the role's kept lane. At legion main nothing reads the
// notices, so the lane holds the scan's one copy and nothing is recorded.
func TestARoleNeverHeldEndsRecordedUndeliveredOnTheIssueAfterTheBound(t *testing.T) {
	ctx := context.Background()
	rig := newRoleRig(t)
	const role = "expiry-never-held"
	rig.silent(t, contracts.RoleTopicPrefix+role)
	rig.silent(t, contracts.AgentTopicPrefix+">")
	event := rig.routedEvent(t, "T-1", role, model.Actor{Kind: "user", ID: "alice"})
	outbox := holding(t, rig.deps())
	outbox.scan(ctx, unfenced{})

	last := expireUpTo(t, rig, outbox, unfenced{}, role, event.ID, maxRoleKeeps)

	if lane := keptLane(t, rig.client, role); len(lane) != maxRoleKeeps {
		t.Fatalf("role %s's kept lane holds %d copies, want the %d keeps of the bound and no more", role, len(lane), maxRoleKeeps)
	}
	rows := expiryRows(t, rig.database, event.ID, role)
	if len(rows) != maxRoleKeeps || rows[maxRoleKeeps-1].outcome == nil || *rows[maxRoleKeeps-1].outcome != "undelivered" || !rows[maxRoleKeeps-1].settled {
		t.Fatalf("the outbox recorded %+v, want the last expiry settled undelivered", rows)
	}
	records := undeliveredRecords(t, rig.database, "T-1")
	if len(records) != 1 {
		t.Fatalf("T-1 holds %d role.undelivered events, want one", len(records))
	}
	record := records[0].Payload.(model.RoleUndeliveredEventPayload)
	if record.EventID != event.ID || record.EventType != event.Type || record.Role != role || record.Keeps != maxRoleKeeps ||
		record.Reason != model.RoleUndeliveredExpired || record.KeptAt.UnixMilli() != last.KeptAt || record.DroppedAt.UnixMilli() != last.DroppedAt {
		t.Fatalf("the record is %+v, want event %d (%s) on role %s, %d keeps, expired, the last copy kept at %d and dropped at %d",
			record, event.ID, event.Type, role, maxRoleKeeps, last.KeptAt, last.DroppedAt)
	}
	if actor := records[0].Actor; actor.Kind != "system" || records[0].Notify {
		t.Fatalf("the record's actor is %+v and notify %v, want a system actor that notifies nobody", actor, records[0].Notify)
	}

	// The record itself is published to the issue's topic and to no role: the scan after it keeps
	// nothing more on the role's lane, and a further read of the notices reopens nothing.
	outbox.scan(ctx, unfenced{})
	outbox.passKeptExpiries(ctx, unfenced{})
	if at := publishedAt(t, rig.database, records[0].ID); at == nil {
		t.Fatal("the outbox did not publish the role.undelivered event to the issue's topic")
	}
	if _, _, destinations := eventState(t, rig.database, records[0].ID); len(destinations) != 1 || destinations[0] != "notifications.dispatch.issue.T-1."+model.RoleUndeliveredEventType {
		t.Fatalf("the record reached %v, want the issue's topic alone", destinations)
	}
	if lane := keptLane(t, rig.client, role); len(lane) != maxRoleKeeps {
		t.Fatalf("role %s's kept lane holds %d copies after the record was published, want %d", role, len(lane), maxRoleKeeps)
	}
	if records := undeliveredRecords(t, rig.database, "T-1"); len(records) != 1 {
		t.Fatalf("T-1 holds %d role.undelivered events after a further pass, want one", len(records))
	}
}

// One copy's expiry notice stored twice - two listeners whose stores of it fell outside the
// stream's duplicate window, here under two message ids - reopens the event's delivery once: the
// expiry is recorded by event, role and attempt, and the second notice finds that row and keeps
// nothing. A pass that reads the notices again keeps nothing either. At legion main nothing reads
// either notice and the lane holds one copy.
func TestTheSameExpiryNoticeTwiceReopensTheRowOnce(t *testing.T) {
	ctx := context.Background()
	rig := newRoleRig(t)
	const role = "expiry-noticed-twice"
	rig.silent(t, contracts.RoleTopicPrefix+role)
	event := rig.routedEvent(t, "T-1", role, model.Actor{Kind: "user", ID: "alice"})
	outbox := holding(t, rig.deps())
	outbox.scan(ctx, unfenced{})
	lane := keptLane(t, rig.client, role)
	if len(lane) != 1 {
		t.Fatalf("role %s's kept lane holds %d copies, want the scan's one", role, len(lane))
	}
	notice := expire(t, rig.client, role, lane[0])
	data, err := json.Marshal(notice)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rig.client.JS().Publish(bus.KeptExpiredSubject(role), data, natsgo.MsgId("another-listener's-store")); err != nil {
		t.Fatalf("store the same notice under a second message id: %v", err)
	}
	if notices := storedCopies(t, rig.client, bus.KeptExpiredSubject(role)); notices != 2 {
		t.Fatalf("the stream holds %d notices of the copy, want the two the test stored", notices)
	}

	outbox.passKeptExpiries(ctx, unfenced{})
	outbox.passKeptExpiries(ctx, unfenced{})

	if lane := keptLane(t, rig.client, role); len(lane) != 2 {
		t.Fatalf("role %s's kept lane holds %d copies, want the first and one keep again", role, len(lane))
	}
	if rows := expiryRows(t, rig.database, event.ID, role); len(rows) != 1 || rows[0].attempt != 1 {
		t.Fatalf("the outbox recorded %+v, want attempt 1's expiry once", rows)
	}
}

// R18's stop rows end with Dispatch's outbox reading the one notice and reopening the row once
// (cmd/listener, TestAnExpiryNoticeReachesDispatchWhenAListenerStopsBetweenItsWrites). Their two
// walks store one copy's notice as a listener does, through bus.Client.PublishKeptCopyExpiry
// under the copy's own message id (bus.KeptExpiryMsgID): the stopped walk, and the walk that took
// its claim over. Here the notice is stored twice that way before the outbox reads it, and once
// more after, as a walk that took over late would store it. The stream holds one notice, and the
// outbox records one expiry, at attempt 1, and keeps the event again once. With the reopen taken
// away (recordExpiry records nothing), no row is recorded and the lane holds the scan's copy
// alone.
func TestANoticeStoredAgainByTheWalkThatTookTheClaimOverReopensTheRowOnce(t *testing.T) {
	ctx := context.Background()
	rig := newRoleRig(t)
	const role = "expiry-stored-again"
	rig.silent(t, contracts.RoleTopicPrefix+role)
	event := rig.routedEvent(t, "T-1", role, model.Actor{Kind: "user", ID: "alice"})
	outbox := holding(t, rig.deps())
	outbox.scan(ctx, unfenced{})
	lane := keptLane(t, rig.client, role)
	if len(lane) != 1 {
		t.Fatalf("role %s's kept lane holds %d copies, want the scan's one", role, len(lane))
	}
	expire(t, rig.client, role, lane[0])
	expire(t, rig.client, role, lane[0])
	if notices := storedCopies(t, rig.client, bus.KeptExpiredSubject(role)); notices != 1 {
		t.Fatalf("the stream holds %d notices of the copy, want the one both stores make", notices)
	}

	outbox.passKeptExpiries(ctx, unfenced{})
	expire(t, rig.client, role, lane[0])
	outbox.passKeptExpiries(ctx, unfenced{})

	rows := expiryRows(t, rig.database, event.ID, role)
	if len(rows) != 1 || rows[0].attempt != 1 || rows[0].sequence != lane[0].Sequence || rows[0].outcome == nil || *rows[0].outcome != expiryKept {
		t.Fatalf("the outbox recorded %+v, want the copy's expiry once, at attempt 1, kept again", rows)
	}
	after := keptLane(t, rig.client, role)
	if len(after) != 2 || after[1].MsgID != bus.KeptAttemptMsgID(envelopeOf(t, lane[0]).DedupeKey, role, 2) {
		t.Fatalf("role %s's kept lane holds %d copies (last id %q), want the scan's and one keep again under attempt 2", role, len(after), lastMsgID(after))
	}
	if notices := storedCopies(t, rig.client, bus.KeptExpiredSubject(role)); notices != 1 {
		t.Fatalf("the stream holds %d notices of the copy after the late store, want one", notices)
	}
}

// A copy that expired under an attempt already recorded for another copy of the same event - a
// takeover's copy of a forward, which keeps the event's envelope under a key of its own, so its
// message id names no attempt and it reads as attempt 1 - is a later expiry, not a repeat: the
// outbox counts it as the attempt after the last one it recorded and keeps the event again. The
// repeat the previous test sends is told from it by the copy's stream sequence.
func TestAnExpiryOfAnotherCopyUnderARecordedAttemptCountsAsTheNextAttempt(t *testing.T) {
	ctx := context.Background()
	rig := newRoleRig(t)
	const role = "expiry-other-copy"
	outbox := holding(t, rig.deps())
	event := expiredOnce(t, rig, outbox, role)
	outbox.passKeptExpiries(ctx, unfenced{})
	lane := keptLane(t, rig.client, role)
	if len(lane) != 2 {
		t.Fatalf("role %s's kept lane holds %d copies after the first copy's hour ended, want the copy kept again", role, len(lane))
	}

	// The copy kept again is forwarded, taken over, and copied to the lane under a minted key; that
	// copy's hour ends and its notice reads attempt 1, as the takeover's message id names none.
	taken := envelopeOf(t, lane[1])
	taken.DedupeKey = "agent.takeover.0123456789abcdef0123456789abcdef"
	if err := rig.client.PublishStoredAs(ctx, bus.KeptRoleSubject(role), taken, "takeover-copy"); err != nil {
		t.Fatalf("store the takeover's copy: %v", err)
	}
	lane = keptLane(t, rig.client, role)
	if notice := expire(t, rig.client, role, lane[2]); notice.Attempt != 1 {
		t.Fatalf("the takeover's copy names attempt %d, want 1", notice.Attempt)
	}
	outbox.passKeptExpiries(ctx, unfenced{})

	rows := expiryRows(t, rig.database, event.ID, role)
	if len(rows) != 2 || rows[1].attempt != 2 || rows[1].sequence != lane[2].Sequence {
		t.Fatalf("the outbox recorded %+v, want the takeover copy's expiry as attempt 2", rows)
	}
	if lane := keptLane(t, rig.client, role); len(lane) != 4 || lane[3].MsgID != bus.KeptAttemptMsgID(taken.DedupeKey, role, 3) {
		t.Fatalf("role %s's kept lane holds %d copies (last id %q), want the event kept a third time under attempt 3", role, len(lane), lastMsgID(lane))
	}
}

// lastMsgID is the message id of the last stored message, for a failure that names it.
func lastMsgID(lane []bus.StoredMessage) string {
	if len(lane) == 0 {
		return ""
	}
	return lane[len(lane)-1].MsgID
}

// expiryCursor is the stream sequence of the last expiry notice the outbox recorded, 0 for none.
func expiryCursor(t *testing.T, database *store.Store) int64 {
	t.Helper()
	var cursor int64
	if err := database.Pool.QueryRow(context.Background(),
		`select coalesce((select sequence from outbox_kept_expiry_cursor), 0)`).Scan(&cursor); err != nil {
		t.Fatalf("read the expiry cursor: %v", err)
	}
	return cursor
}

// newestCopyOf is the copy of eventID that role's kept lane stored last.
func newestCopyOf(t *testing.T, rig *roleRig, role string, eventID int64) bus.StoredMessage {
	t.Helper()
	var newest *bus.StoredMessage
	for _, stored := range keptLane(t, rig.client, role) {
		if envelopeOf(t, stored).SourceEventID == fmt.Sprint(eventID) {
			newest = &stored
		}
	}
	if newest == nil {
		t.Fatalf("role %s's kept lane holds no copy of event %d", role, eventID)
	}
	return *newest
}

// expireUpTo runs eventID's copies on role's kept lane through keeps 1 to last: for each, it stores
// the expiry notice of the event's newest copy, as a listener's walk does at the end of its hour,
// and runs outbox's expiry pass under hold. At maxRoleKeeps the outbox records the event
// undelivered; below it, the outbox has kept the event's copy last+1 times.
func expireUpTo(t *testing.T, rig *roleRig, outbox *Outbox, hold fence, role string, eventID int64, last int) bus.KeptCopyExpiry {
	t.Helper()
	var notice bus.KeptCopyExpiry
	for keep := 1; keep <= last; keep++ {
		if notice = expire(t, rig.client, role, newestCopyOf(t, rig, role, eventID)); notice.Attempt != keep {
			t.Fatalf("event %d's newest copy names attempt %d, want %d", eventID, notice.Attempt, keep)
		}
		outbox.passKeptExpiries(context.Background(), hold)
	}
	return notice
}

// failingKeeps is a publisher whose window answers every keep on role's kept lane with err, and
// passes every other send on.
type failingKeeps struct {
	*bus.Client
	role string
	err  error
}

func (p failingKeeps) WindowUntil(deadline func() time.Time) bus.Sender {
	return failingKeepsWindow{Sender: p.Client.WindowUntil(deadline), keeps: p}
}

type failingKeepsWindow struct {
	bus.Sender
	keeps failingKeeps
}

func (w failingKeepsWindow) PublishStoredAsync(subject string, item contracts.Envelope, msgID string) <-chan error {
	if subject == bus.KeptRoleSubject(w.keeps.role) {
		return answered(fmt.Errorf("test window: %w", w.keeps.err))
	}
	return w.Sender.PublishStoredAsync(subject, item, msgID)
}

// expiredOnce routes an event to role, keeps its copy through outbox's scan and ends that copy's
// hour, and answers the event: what a test of the expiry pass's first keep starts from.
func expiredOnce(t *testing.T, rig *roleRig, outbox *Outbox, role string) model.Event {
	t.Helper()
	rig.silent(t, contracts.RoleTopicPrefix+role)
	event := rig.routedEvent(t, "T-1", role, model.Actor{Kind: "user", ID: "alice"})
	outbox.scan(context.Background(), unfenced{})
	lane := keptLane(t, rig.client, role)
	if len(lane) != 1 {
		t.Fatalf("role %s's kept lane holds %d copies, want the scan's one", role, len(lane))
	}
	expire(t, rig.client, role, lane[0])
	return event
}

// An expired copy whose keep NATS refuses for good cannot be kept again, and no other copy of the
// event carries its delivery, so its row settles undelivered and one role.undelivered record is
// appended, with the reason expired, after the one keep that ended. With a refused keep counted as
// a failed attempt instead, the row stays owed and is retried with backoff for good.
func TestARefusedKeepEndsItsEventRecordedUndelivered(t *testing.T) {
	ctx := context.Background()
	rig := newRoleRig(t)
	const role = "expiry-keep-refused"
	event := expiredOnce(t, rig, holding(t, rig.deps()), role)

	holding(t, rig.publisherOn(failingKeeps{Client: rig.client, role: role, err: bus.ErrRefused})).passKeptExpiries(ctx, unfenced{})

	rows := expiryRows(t, rig.database, event.ID, role)
	if len(rows) != 1 || !rows[0].settled || rows[0].outcome == nil || *rows[0].outcome != expiryUndelivered {
		t.Fatalf("event %d's expiries are %+v, want its one settled undelivered", event.ID, rows)
	}
	records := undeliveredRecords(t, rig.database, "T-1")
	if len(records) != 1 {
		t.Fatalf("T-1 holds %d role.undelivered events, want one", len(records))
	}
	if record := records[0].Payload.(model.RoleUndeliveredEventPayload); record.Reason != model.RoleUndeliveredExpired || record.Keeps != 1 {
		t.Fatalf("the record is %+v, want reason expired after 1 keep", record)
	}
}

// A notice whose own attempt disagrees with the outbox's count - here it says 5 for the event's
// first expiry - is recorded at the outbox's next attempt, 1, and the event is kept again. With
// the notice's attempt deciding the row, this notice reaches the bound at once.
func TestTheOutboxsCountDecidesAnExpirysAttempt(t *testing.T) {
	ctx := context.Background()
	rig := newRoleRig(t)
	const role = "expiry-attempt-count"
	rig.silent(t, contracts.RoleTopicPrefix+role)
	event := rig.routedEvent(t, "T-1", role, model.Actor{Kind: "user", ID: "alice"})
	outbox := holding(t, rig.deps())
	outbox.scan(ctx, unfenced{})
	lane := keptLane(t, rig.client, role)
	notice := expiryNoticeOf(t, role, lane[0])
	notice.Attempt = 5
	if err := rig.client.PublishKeptCopyExpiry(ctx, notice); err != nil {
		t.Fatalf("store the notice: %v", err)
	}

	outbox.passKeptExpiries(ctx, unfenced{})

	rows := expiryRows(t, rig.database, event.ID, role)
	if len(rows) != 1 || rows[0].attempt != 1 || rows[0].outcome == nil || *rows[0].outcome != expiryKept {
		t.Fatalf("event %d's expiries are %+v, want attempt 1 kept again", event.ID, rows)
	}
	if records := undeliveredRecords(t, rig.database, "T-1"); len(records) != 0 {
		t.Fatalf("T-1 holds %d role.undelivered events, want none after the first expiry", len(records))
	}
}

// A notice that names no copy this outbox kept for its role is passed over, and only the cursor
// moves past it: one for a role the event was never routed to, whose kept lane holds a copy of the
// event all the same, so the routing check alone refuses it, and one stored on another role's
// expiry subject than its own role's. With the routing and subject checks taken away, both are
// recorded and kept again.
func TestANoticeForACopyTheOutboxNeverKeptIsPassedOver(t *testing.T) {
	ctx := context.Background()
	rig := newRoleRig(t)
	const role, other = "expiry-notice-routed", "expiry-notice-elsewhere"
	rig.silent(t, contracts.RoleTopicPrefix+role)
	rig.silent(t, contracts.RoleTopicPrefix+other)
	event := rig.routedEvent(t, "T-1", role, model.Actor{Kind: "user", ID: "alice"})
	outbox := holding(t, rig.deps())
	outbox.scan(ctx, unfenced{})
	lane := keptLane(t, rig.client, role)

	planted, err := rig.client.JS().Publish(bus.KeptRoleSubject(other), lane[0].Data)
	if err != nil {
		t.Fatalf("store the event's copy on role %s's kept lane: %v", other, err)
	}
	unrouted := expiryNoticeOf(t, role, lane[0])
	unrouted.Role, unrouted.Sequence = other, planted.Sequence
	if err := rig.client.PublishKeptCopyExpiry(ctx, unrouted); err != nil {
		t.Fatalf("store the notice for a role the event was never routed to: %v", err)
	}
	misfiled, err := json.Marshal(expiryNoticeOf(t, role, lane[0]))
	if err != nil {
		t.Fatal(err)
	}
	ack, err := rig.client.JS().Publish(bus.KeptExpiredSubject(other), misfiled)
	if err != nil {
		t.Fatalf("store the notice on another role's expiry subject: %v", err)
	}

	outbox.passKeptExpiries(ctx, unfenced{})

	for _, r := range []string{role, other} {
		if rows := expiryRows(t, rig.database, event.ID, r); len(rows) != 0 {
			t.Fatalf("the outbox recorded %+v for role %s, want nothing", rows, r)
		}
	}
	if cursor := expiryCursor(t, rig.database); cursor != int64(ack.Sequence) {
		t.Fatalf("the expiry cursor is %d, want it at the misfiled notice's sequence %d, past both notices", cursor, ack.Sequence)
	}
	if lane := keptLane(t, rig.client, role); len(lane) != 1 {
		t.Fatalf("role %s's kept lane holds %d copies, want the scan's one", role, len(lane))
	}
	if lane := keptLane(t, rig.client, other); len(lane) != 1 {
		t.Fatalf("role %s's kept lane holds %d copies, want the one the test planted", other, len(lane))
	}
}

// A notice whose role is no role name (roles.Valid) is passed over, even when the event names that
// role's topic among its destinations, so the routing check alone would take it, when that role's
// kept lane holds a copy of the event, so the copy check alone would take it too, and when it was
// stored on that role's own expiry subject: only the cursor moves past it. With the role check
// taken away, the notice is recorded.
func TestANoticeWhoseRoleIsNoRoleNameIsPassedOver(t *testing.T) {
	ctx := context.Background()
	rig := newRoleRig(t)
	const role, invalid = "expiry-notice-valid", "Not-A-Role"
	rig.silent(t, contracts.RoleTopicPrefix+role)
	event := rig.routedEvent(t, "T-1", role, model.Actor{Kind: "user", ID: "alice"})
	outbox := holding(t, rig.deps())
	outbox.scan(ctx, unfenced{})
	lane := keptLane(t, rig.client, role)
	if _, err := rig.database.Pool.Exec(ctx, `
		update events set published_destinations = array_append(published_destinations, $2) where id = $1
	`, event.ID, contracts.RoleTopicPrefix+invalid); err != nil {
		t.Fatalf("name the invalid role's topic among event %d's destinations: %v", event.ID, err)
	}
	planted, err := rig.client.JS().Publish(bus.KeptRoleSubject(invalid), lane[0].Data)
	if err != nil {
		t.Fatalf("store the event's copy on role %s's kept lane: %v", invalid, err)
	}
	notice := expiryNoticeOf(t, role, lane[0])
	notice.Role, notice.Sequence = invalid, planted.Sequence
	if err := rig.client.PublishKeptCopyExpiry(ctx, notice); err != nil {
		t.Fatalf("store the notice for role %s: %v", invalid, err)
	}
	stored, err := rig.client.NextStoredMessage(ctx, bus.KeptExpiredSubject(invalid), 1)
	if err != nil {
		t.Fatalf("read the notice back: %v", err)
	}

	outbox.passKeptExpiries(ctx, unfenced{})

	if rows := expiryRows(t, rig.database, event.ID, invalid); len(rows) != 0 {
		t.Fatalf("the outbox recorded %+v for role %s, want nothing", rows, invalid)
	}
	if cursor := expiryCursor(t, rig.database); cursor != int64(stored.Sequence) {
		t.Fatalf("the expiry cursor is %d, want it at the notice's sequence %d", cursor, stored.Sequence)
	}
}

// A keep of the expiry pass that finds NATS unavailable counts on the outbox's publish failures as
// failed, and its row is released after retry.BaseDelay with no attempt counted: the outage is no
// fault of the row. With the send released before it is counted, the count is missing.
func TestAnUnavailableKeepIsCountedAndReleased(t *testing.T) {
	ctx := context.Background()
	rig := newRoleRig(t)
	const role = "expiry-keep-unavailable"
	event := expiredOnce(t, rig, holding(t, rig.deps()), role)
	deps, registry := rig.countingDeps(t)
	deps.Publisher = failingKeeps{Client: rig.client, role: role, err: bus.ErrUnavailable}

	before := time.Now()
	holding(t, deps).passKeptExpiries(ctx, unfenced{})
	after := time.Now()

	if body := failureReasons(t, registry); !strings.Contains(body, `envoy_dispatch_outbox_publish_failures_total{reason="failed"} 1`) {
		t.Fatalf("/metrics body = %q, want the unavailable keep counted as failed once", body)
	}
	rows := expiryRows(t, rig.database, event.ID, role)
	if len(rows) != 1 || rows[0].failures != 0 || rows[0].settled || rows[0].due == nil ||
		rows[0].due.Before(before.Add(retry.BaseDelay-time.Second)) || rows[0].due.After(after.Add(retry.BaseDelay+time.Second)) {
		t.Fatalf("event %d's expiries are %+v, want one with no failed send, unsettled, due retry.BaseDelay (%v) after the pass",
			event.ID, rows, retry.BaseDelay)
	}
}

// A keep of the expiry pass that the stream never acknowledged counts on the outbox's publish
// failures as unanswered, as the batch counts a role-route send it never heard back on
// (forwardUnanswered), and its row counts one failed attempt. With the keep not counted as a
// role-route send, it is counted as failed.
func TestAnUnacknowledgedKeepIsCountedUnanswered(t *testing.T) {
	ctx := context.Background()
	rig := newRoleRig(t)
	const role = "expiry-keep-unacknowledged"
	event := expiredOnce(t, rig, holding(t, rig.deps()), role)
	deps, registry := rig.countingDeps(t)
	deps.Publisher = failingKeeps{Client: rig.client, role: role, err: bus.ErrNotAcknowledged}

	holding(t, deps).passKeptExpiries(ctx, unfenced{})

	body := failureReasons(t, registry)
	if !strings.Contains(body, `envoy_dispatch_outbox_publish_failures_total{reason="unanswered"} 1`) || strings.Contains(body, `reason="failed"`) {
		t.Fatalf("/metrics body = %q, want the unacknowledged keep counted as unanswered once and nothing as failed", body)
	}
	if rows := expiryRows(t, rig.database, event.ID, role); len(rows) != 1 || rows[0].failures != 1 || rows[0].settled {
		t.Fatalf("event %d's expiries are %+v, want one with one failed send, unsettled", event.ID, rows)
	}
}
