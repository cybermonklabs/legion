package record

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/store/storetext"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// registerIntakeController records project's controller as the daemon does when
// `legion controller start` mints a capability and its session registers with it, under the
// project token (ControllerRegistered reads that row).
func registerIntakeController(t *testing.T, st *store.Store, project string) {
	t.Helper()
	ctx := context.Background()
	token, err := claim.ProjectToken(project)
	must(t, err)
	generation, err := st.MintController(ctx, token, []byte("capability"))
	if err != nil {
		t.Fatalf("mint the controller capability of %s: %v", project, err)
	}
	if ok, err := st.RegisterController(ctx, token, generation, "ses-controller", []byte("secret"), time.Now(), 0); err != nil || !ok {
		t.Fatalf("register the controller of %s = %t, %v", project, ok, err)
	}
}

// intakeConsumers are a project's three durable consumers, named as intake names them.
func intakeConsumers(project string) []string {
	return []string{"legion-go-" + project + "-dispatch", "legion-go-" + project + "-github", "legion-go-" + project + "-wake"}
}

// failingMessage is a decoded message of project's Dispatch consumer at stream sequence seq, with
// a header, as the intake hands it to the record.
func failingMessage(project string, seq uint64) IntakeMessage {
	return IntakeMessage{
		Project:     project,
		Consumer:    intakeConsumers(project)[0],
		Subject:     fmt.Sprintf("notifications.dispatch.issue.%s-208.issue.updated", project),
		StreamSeq:   seq,
		StoredAt:    time.Date(2026, 11, 18, 4, 0, 0, 0, time.UTC).Add(time.Duration(seq) * time.Millisecond),
		Headers:     IntakeHeaders(map[string][]string{"Nats-Msg-Id": {fmt.Sprintf("dispatch-%d", seq)}}),
		Payload:     []byte(fmt.Sprintf(`{"type":"issue.updated","seq":%d}`, seq)),
		EventSource: "dispatch",
		EventID:     fmt.Sprintf("dispatch-%d", seq),
	}
}

// giveUpAt fails message once at now and gives it up at the bound in the same transaction, as the
// intake does at the delivery that reaches the count and the hour: NoteFailure, then
// GiveUpIntakeMessage. It answers the record as the table holds it after, read whole, and fails
// the test unless the give-up answered that record without the message's bytes.
func giveUpAt(t *testing.T, st *store.Store, message IntakeMessage, now time.Time) IntakeFailure {
	t.Helper()
	ctx := context.Background()
	records := NewStore()
	var given IntakeFailure
	inTx(t, st, func(tx pgx.Tx) {
		failed, err := records.NoteFailure(ctx, tx, message, 21, "apply the fact: the database refused the write", now)
		must(t, err)
		var gaveUp bool
		given, gaveUp, err = records.GiveUpIntakeMessage(ctx, tx, message.Project, failed.ID, GiveUpBound, failed.LastError, now)
		must(t, err)
		if !gaveUp {
			t.Fatalf("the give-up of the open failure %d gave nothing up", failed.ID)
		}
	})
	whole, found := failureOf(t, st, given.ID)
	if !found || !reflect.DeepEqual(given, asAnswered(whole)) {
		t.Fatalf("GiveUpIntakeMessage answered\n%#v\nwant the record it left without the message's bytes\n%#v", given, asAnswered(whole))
	}
	return whole
}

// intakeNotice is one intake-gave-up controller notice row and its payload.
type intakeNotice struct {
	row    OutboxRow
	notice ControllerNotice
}

// intakeNotices is every intake-gave-up controller notice row of the outbox, oldest first, each
// decoded by the outbox's own strict decode, so a row that would fail at its publish fails here.
func intakeNotices(t *testing.T, st *store.Store) []intakeNotice {
	t.Helper()
	ctx := context.Background()
	var notices []intakeNotice
	inTx(t, st, func(tx pgx.Tx) {
		rows, err := tx.Query(ctx, `select `+outboxColumns+` from outbox
			where kind = 'controller_notice' and payload->>'kind' = 'intake-gave-up' order by id`)
		must(t, err)
		defer rows.Close()
		for rows.Next() {
			row, err := scanOutbox(rows)
			must(t, err)
			payload, err := DecodeOutboxPayload(row)
			if err != nil {
				t.Fatalf("decode the intake-gave-up notice %d: %v", row.ID, err)
			}
			notices = append(notices, intakeNotice{row: row, notice: payload.(ControllerNotice)})
		}
		must(t, rows.Err())
	})
	return notices
}

// noticeGiveUps is n give-ups as a notice names them, with ids 1 to n.
func noticeGiveUps(n int) []NoticeGiveUp {
	giveUps := make([]NoticeGiveUp, n)
	for i := range giveUps {
		giveUps[i] = NoticeGiveUp{ID: int64(i + 1), Consumer: "legion-go-LEGION-dispatch", Subject: fmt.Sprintf("notifications.dispatch.issue.LEGION-%d.issue.updated", i+1)}
	}
	return giveUps
}

// noticeIDOf is the notice id the record of give-up id keeps, 0 for none.
func noticeIDOf(t *testing.T, st *store.Store, id int64) int64 {
	t.Helper()
	var notice int64
	inTx(t, st, func(tx pgx.Tx) {
		must(t, tx.QueryRow(context.Background(), `select coalesce(notice_id, 0) from intake_failures where id = $1`, id).Scan(&notice))
	})
	return notice
}

// named is the name a notice gives the give-up of failure: its subject as the store keeps text.
func named(failure IntakeFailure) NoticeGiveUp {
	return NoticeGiveUp{ID: failure.ID, Consumer: failure.Consumer, Subject: storetext.Text(failure.Subject)}
}

// leaseNotice leases the notice row id as the outbox's lane does when it comes due at now, and
// answers the lease's token.
func leaseNotice(t *testing.T, st *store.Store, id int64, now time.Time) string {
	t.Helper()
	ctx := context.Background()
	records := NewStore()
	var token string
	inTx(t, st, func(tx pgx.Tx) {
		claimed, ok, err := records.ClaimRow(ctx, tx, id, now, time.Minute)
		must(t, err)
		if !ok {
			t.Fatalf("the lane's claim of the notice row %d at %s claimed nothing", id, now)
		}
		token = claimed.LeaseToken
	})
	return token
}

// A give-up made while the project's notice waits for its publish joins that notice: the row
// opened by the first give-up of a burst is due 60 seconds after it, so the burst is one wake of
// the controller, and each later give-up raises its count and is named in it. The row keeps the
// due time of the first give-up, and each record keeps the row's id.
func TestAGiveUpBeforeTheNoticeIsLeasedJoinsIt(t *testing.T) {
	now := time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)
	st := migratedStore(t)
	registerIntakeController(t, st, "LEGION")

	first := giveUpAt(t, st, failingMessage("LEGION", 41), now)
	second := giveUpAt(t, st, failingMessage("LEGION", 42), now.Add(10*time.Second))

	notices := intakeNotices(t, st)
	if len(notices) != 1 {
		t.Fatalf("the outbox holds %d intake-gave-up notices after two give-ups 10 seconds apart, want one: %#v", len(notices), notices)
	}
	got := notices[0]
	if got.row.Issue != "LEGION" {
		t.Errorf("the notice is queued on %q, want the project key LEGION, as the tick's notice is", got.row.Issue)
	}
	if want := now.Add(giveUpNoticeDelay); !got.row.NextAt.Equal(want) {
		t.Errorf("the notice is due at %s, want %s: the first give-up's time plus %s", got.row.NextAt.UTC(), want, giveUpNoticeDelay)
	}
	want := ControllerNotice{Kind: IntakeGaveUpNotice, Count: 2, GiveUps: []NoticeGiveUp{named(first), named(second)}}
	if !reflect.DeepEqual(got.notice, want) {
		t.Errorf("the notice reads %#v, want %#v", got.notice, want)
	}
	for _, failure := range []IntakeFailure{first, second} {
		if failure.NoticeID != got.row.ID || noticeIDOf(t, st, failure.ID) != got.row.ID {
			t.Errorf("the record %d keeps notice %d (answered %d), want %d", failure.ID, noticeIDOf(t, st, failure.ID), failure.NoticeID, got.row.ID)
		}
	}
}

// A give-up that finds the project's notice leased by the outbox's lane opens the next notice,
// due 60 seconds after it, since the leased row is being published and a count raised in it now
// may never reach the controller.
func TestAGiveUpThatFindsTheNoticeLeasedOpensTheNext(t *testing.T) {
	now := time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)
	st := migratedStore(t)
	registerIntakeController(t, st, "LEGION")

	first := giveUpAt(t, st, failingMessage("LEGION", 41), now)
	leasedAt := now.Add(giveUpNoticeDelay)
	leaseNotice(t, st, first.NoticeID, leasedAt)

	secondAt := leasedAt.Add(time.Second)
	second := giveUpAt(t, st, failingMessage("LEGION", 42), secondAt)
	if second.NoticeID == first.NoticeID {
		t.Fatalf("the give-up made while notice %d was leased joined it; want a notice of its own", first.NoticeID)
	}
	notices := intakeNotices(t, st)
	if len(notices) != 2 {
		t.Fatalf("the outbox holds %d intake-gave-up notices, want the leased one and the next: %#v", len(notices), notices)
	}
	leased, next := notices[0], notices[1]
	if want := (ControllerNotice{Kind: IntakeGaveUpNotice, Count: 1, GiveUps: []NoticeGiveUp{named(first)}}); !reflect.DeepEqual(leased.notice, want) {
		t.Errorf("the leased notice reads %#v, want it as it was leased, %#v", leased.notice, want)
	}
	if next.row.ID != second.NoticeID || !next.row.NextAt.Equal(secondAt.Add(giveUpNoticeDelay)) {
		t.Errorf("the next notice is row %d due at %s, want row %d due at %s", next.row.ID, next.row.NextAt.UTC(), second.NoticeID, secondAt.Add(giveUpNoticeDelay))
	}
	if want := (ControllerNotice{Kind: IntakeGaveUpNotice, Count: 1, GiveUps: []NoticeGiveUp{named(second)}}); !reflect.DeepEqual(next.notice, want) {
		t.Errorf("the next notice reads %#v, want %#v", next.notice, want)
	}
}

// A give-up after a failed attempt of the project's notice opens the next notice rather than
// joining the one the lane gave back to retry (RetryOutbox clears the lease): the attempt may have
// reached the controller though its answer was an error, and the retry publishes under the same
// key, which Envoy does not deliver twice, so a count raised after the attempt may never be heard.
// The give-up after it joins the new notice, which no attempt has run, and each give-up is counted
// in exactly one notice: the one its record keeps, which names it.
func TestAGiveUpAfterAFailedAttemptOfTheNoticeOpensTheNext(t *testing.T) {
	now := time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	registerIntakeController(t, st, "LEGION")

	first := giveUpAt(t, st, failingMessage("LEGION", 41), now)
	leasedAt := now.Add(giveUpNoticeDelay)
	token := leaseNotice(t, st, first.NoticeID, leasedAt)
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.RetryOutbox(ctx, tx, first.NoticeID, token, leasedAt.Add(time.Second), "the controller's topic timed out"))
	})

	secondAt := leasedAt.Add(time.Minute)
	second := giveUpAt(t, st, failingMessage("LEGION", 42), secondAt)
	if second.NoticeID == first.NoticeID {
		t.Fatalf("the give-up after a failed attempt of notice %d joined it; want a notice of its own", first.NoticeID)
	}
	third := giveUpAt(t, st, failingMessage("LEGION", 43), secondAt.Add(5*time.Second))
	if third.NoticeID != second.NoticeID {
		t.Fatalf("the give-up after the second was counted in notice %d, want the second's notice %d, which no attempt has run", third.NoticeID, second.NoticeID)
	}

	notices := intakeNotices(t, st)
	if len(notices) != 2 {
		t.Fatalf("the outbox holds %d intake-gave-up notices, want the retried one and the next: %#v", len(notices), notices)
	}
	retried, next := notices[0], notices[1]
	if want := (ControllerNotice{Kind: IntakeGaveUpNotice, Count: 1, GiveUps: []NoticeGiveUp{named(first)}}); retried.row.ID != first.NoticeID ||
		!reflect.DeepEqual(retried.notice, want) {
		t.Errorf("the retried notice is row %d reading %#v, want row %d as its attempt read it, %#v", retried.row.ID, retried.notice, first.NoticeID, want)
	}
	if !next.row.NextAt.Equal(secondAt.Add(giveUpNoticeDelay)) {
		t.Errorf("the next notice is due at %s, want %s", next.row.NextAt.UTC(), secondAt.Add(giveUpNoticeDelay))
	}
	if want := (ControllerNotice{Kind: IntakeGaveUpNotice, Count: 2, GiveUps: []NoticeGiveUp{named(second), named(third)}}); !reflect.DeepEqual(next.notice, want) {
		t.Errorf("the next notice reads %#v, want %#v", next.notice, want)
	}
	namedIn := map[int64][]int64{}
	counted := 0
	for _, n := range notices {
		counted += n.notice.Count
		for _, g := range n.notice.GiveUps {
			namedIn[g.ID] = append(namedIn[g.ID], n.row.ID)
		}
	}
	if counted != 3 {
		t.Errorf("the notices count %d give-ups, want the 3", counted)
	}
	for _, failure := range []IntakeFailure{first, second, third} {
		if kept := noticeIDOf(t, st, failure.ID); !reflect.DeepEqual(namedIn[failure.ID], []int64{kept}) {
			t.Errorf("give-up %d is named in notices %v and its record keeps notice %d, want it named in that one notice alone", failure.ID, namedIn[failure.ID], kept)
		}
	}
}

// A burst of 150 give-ups is one notice that counts 150 and names the first 100: the count is
// every give-up, and the names stop at giveUpNoticeNames, so the notice stays small whatever the
// burst. The row still passes the outbox's validation, which is what its publish decodes.
func TestANoticeNamesAtMostAHundredGiveUps(t *testing.T) {
	now := time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	const burst = 150
	var names []NoticeGiveUp
	inTx(t, st, func(tx pgx.Tx) {
		for i := range burst {
			giveUp := NoticeGiveUp{ID: int64(i + 1), Consumer: intakeConsumers("LEGION")[i%3], Subject: fmt.Sprintf("notifications.dispatch.issue.LEGION-%d.issue.updated", i)}
			if _, err := records.countIntakeGiveUp(ctx, tx, "LEGION", giveUp, now.Add(time.Duration(i)*time.Millisecond)); err != nil {
				t.Fatalf("count give-up %d: %v", i+1, err)
			}
			if i < giveUpNoticeNames {
				names = append(names, giveUp)
			}
		}
	})
	notices := intakeNotices(t, st)
	if len(notices) != 1 {
		t.Fatalf("the outbox holds %d intake-gave-up notices after a burst, want one", len(notices))
	}
	got := notices[0].notice
	if got.Count != burst || len(got.GiveUps) != giveUpNoticeNames {
		t.Fatalf("the notice counts %d and names %d, want %d and %d", got.Count, len(got.GiveUps), burst, giveUpNoticeNames)
	}
	if !reflect.DeepEqual(got.GiveUps, names) {
		t.Errorf("the notice names %v, want the first %d give-ups in order", got.GiveUps, giveUpNoticeNames)
	}
}

// Each give-up is counted in exactly one notice, whichever consumer gave it up and whether or not
// a lease came between: the counts of the notices sum to the give-ups, each give-up is named in
// the one notice its record keeps, and in no other. Two give-ups whose transactions overlap join
// one notice too: the second waits on the project's lock until the first commits, and then finds
// the row the first opened, where without the lock each would read no row and open one.
func TestEachGiveUpIsCountedInExactlyOneNotice(t *testing.T) {
	now := time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	registerIntakeController(t, st, "LEGION")

	var given []IntakeFailure
	for i, consumer := range intakeConsumers("LEGION") {
		message := failingMessage("LEGION", uint64(100+i))
		message.Consumer = consumer
		given = append(given, giveUpAt(t, st, message, now.Add(time.Duration(i)*time.Second)))
	}
	leaseNotice(t, st, given[0].NoticeID, now.Add(giveUpNoticeDelay))

	// Two give-ups at once: the first's transaction holds the lock while the second's waits on it.
	later := now.Add(2 * giveUpNoticeDelay)
	first, err := st.Pool().BeginTx(ctx, pgx.TxOptions{})
	must(t, err)
	defer func() { _ = first.Rollback(ctx) }()
	failed, err := records.NoteFailure(ctx, first, failingMessage("LEGION", 200), 21, "the handler failed", later)
	must(t, err)
	gave, ok, err := records.GiveUpIntakeMessage(ctx, first, "LEGION", failed.ID, GiveUpBound, failed.LastError, later)
	if err != nil || !ok {
		t.Fatalf("the first of two overlapping give-ups = %t, %v", ok, err)
	}
	type answer struct {
		failure IntakeFailure
		err     error
	}
	second := make(chan answer, 1)
	go func() {
		var got answer
		got.err = store.Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
			failed, err := records.NoteFailure(ctx, tx, failingMessage("LEGION", 201), 21, "the handler failed", later)
			if err != nil {
				return err
			}
			got.failure, _, err = records.GiveUpIntakeMessage(ctx, tx, "LEGION", failed.ID, GiveUpBound, failed.LastError, later)
			return err
		})
		second <- got
	}()
	returned := false
	var got answer
	testwait.Eventually(t, "the second give-up to wait on the project's notice lock or to return", func() bool {
		select {
		case got = <-second:
			returned = true
			return true
		default:
		}
		var waiting int
		err := st.Pool().QueryRow(ctx, `select count(*) from pg_stat_activity
			where datname = current_database() and wait_event_type = 'Lock' and wait_event = 'advisory'`).Scan(&waiting)
		return err == nil && waiting > 0
	})
	must(t, first.Commit(ctx))
	if !returned {
		got = <-second
	}
	must(t, got.err)
	given = append(given, gave, got.failure)
	if gave.NoticeID != got.failure.NoticeID {
		t.Errorf("two overlapping give-ups were counted in notices %d and %d, want one", gave.NoticeID, got.failure.NoticeID)
	}

	notices := intakeNotices(t, st)
	counted := 0
	namedIn := map[int64][]int64{}
	for _, n := range notices {
		counted += n.notice.Count
		for _, g := range n.notice.GiveUps {
			namedIn[g.ID] = append(namedIn[g.ID], n.row.ID)
		}
	}
	if counted != len(given) {
		t.Errorf("the notices count %d give-ups, want %d: %#v", counted, len(given), notices)
	}
	for _, failure := range given {
		kept := noticeIDOf(t, st, failure.ID)
		if !reflect.DeepEqual(namedIn[failure.ID], []int64{kept}) || kept == 0 {
			t.Errorf("give-up %d is named in notices %v and its record keeps notice %d, want it named in that one notice alone", failure.ID, namedIn[failure.ID], kept)
		}
	}
}

// With no controller registered a give-up is counted in no notice and keeps no notice id, and it
// stays that way: a controller that registers later is told of the give-ups after it, and reads
// the earlier ones in the state.
func TestAGiveUpWithNoControllerRegisteredGetsNoNotice(t *testing.T) {
	now := time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)
	st := migratedStore(t)

	alone := giveUpAt(t, st, failingMessage("LEGION", 41), now)
	if alone.NoticeID != 0 || noticeIDOf(t, st, alone.ID) != 0 {
		t.Fatalf("a give-up with no controller registered keeps notice %d (answered %d), want none", noticeIDOf(t, st, alone.ID), alone.NoticeID)
	}
	if notices := intakeNotices(t, st); len(notices) != 0 {
		t.Fatalf("a give-up with no controller registered queued %d notices, want none: %#v", len(notices), notices)
	}

	registerIntakeController(t, st, "LEGION")
	told := giveUpAt(t, st, failingMessage("LEGION", 42), now.Add(time.Minute))
	notices := intakeNotices(t, st)
	if len(notices) != 1 {
		t.Fatalf("the outbox holds %d intake-gave-up notices after the controller registered, want one", len(notices))
	}
	if want := (ControllerNotice{Kind: IntakeGaveUpNotice, Count: 1, GiveUps: []NoticeGiveUp{named(told)}}); !reflect.DeepEqual(notices[0].notice, want) {
		t.Errorf("the notice reads %#v, want %#v: the give-up before the controller registered is in none", notices[0].notice, want)
	}
	if noticeIDOf(t, st, alone.ID) != 0 {
		t.Errorf("the give-up made with no controller registered keeps notice %d after one registered, want none for good", noticeIDOf(t, st, alone.ID))
	}
}

// The intake-gave-up notice is a controller notice and no architect's: the give-up is the
// daemon's own, of no issue an architect runs. The validation takes it in a ControllerNotice with
// its count and the give-ups it names, and refuses it in a Notice, and refuses it naming none.
func TestTheIntakeGaveUpNoticeIsOneOfTheControllerOnlyNoticeKinds(t *testing.T) {
	if !controllerOnlyNoticeKind(IntakeGaveUpNotice) {
		t.Fatalf("the %s kind is not controller-only, so a notice of it would be queued to an architect's topic", IntakeGaveUpNotice)
	}
	if validNoticeKind(IntakeGaveUpNotice) {
		t.Fatalf("the %s kind is on the architect's own list, so an architect would be told a give-up meant for the controller", IntakeGaveUpNotice)
	}
	at := time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)
	giveUps := []NoticeGiveUp{{ID: 41, Consumer: "legion-go-LEGION-dispatch", Subject: "notifications.dispatch.issue.LEGION-208.issue.updated"}}
	if _, err := NewOutboxRow("LEGION", ControllerNotice{Kind: IntakeGaveUpNotice, Count: 1, GiveUps: giveUps}, at); err != nil {
		t.Fatalf("the notice the daemon writes is refused: %v", err)
	}
	if _, err := NewOutboxRow("LEGION", ControllerNotice{Kind: IntakeGaveUpNotice}, at); err == nil {
		t.Fatalf("a notice that counts and names no give-up is taken, and a controller reading it would learn nothing")
	}
	if _, err := NewOutboxRow("LEGION", Notice{Kind: IntakeGaveUpNotice, Count: 1, GiveUps: giveUps}, at); err == nil {
		t.Fatalf("the notice is taken as an architect's notice, which is the one reader that cannot act on it")
	}
}
