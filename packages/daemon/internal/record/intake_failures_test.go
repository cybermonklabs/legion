package record

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/store/storetext"
)

// failureOf is the intake_failures row id as the table holds it, read whole, and false when the
// table holds none.
func failureOf(t *testing.T, st *store.Store, id int64) (IntakeFailure, bool) {
	t.Helper()
	var failure IntakeFailure
	found := true
	inTx(t, st, func(tx pgx.Tx) {
		var err error
		failure, err = scanIntakeFailure(tx.QueryRow(context.Background(), `select `+intakeFailureColumns+` from intake_failures where id = $1`, id))
		if errors.Is(err, pgx.ErrNoRows) {
			found = false
			return
		}
		must(t, err)
	})
	return failure, found
}

// holds fails the test unless the table holds want, field for field, at want.ID.
func holds(t *testing.T, st *store.Store, statement string, want IntakeFailure) {
	t.Helper()
	got, found := failureOf(t, st, want.ID)
	if !found {
		t.Fatalf("after %s the table holds no row %d, want %#v", statement, want.ID, want)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("after %s row %d reads\n%#v\nwant\n%#v", statement, want.ID, got, want)
	}
}

// wholeMessage is a failing message whose subject, payload and header value each hold a NUL and a
// byte that is no UTF-8, which the record keeps byte for byte: the columns are bytea, so neither
// the store's text rule nor any encoding touches them.
func wholeMessage(project string, seq uint64) IntakeMessage {
	message := failingMessage(project, seq)
	message.Subject += "\x00\xff"
	message.Headers = IntakeHeaders(map[string][]string{"Nats-Msg-Id": {"dispatch-\x00\xff"}, "Content-Type": {"application/json"}})
	message.Payload = []byte("{\"body\":\"\x00\xff\"}")
	return message
}

// asAnswered is failure as every statement but the replay's claim answers it: without its header
// block and payload, which those statements do not read back (intakeGiveUpColumns).
func asAnswered(failure IntakeFailure) IntakeFailure {
	failure.Headers, failure.Payload = nil, nil
	return failure
}

// noteFailureAt records, in a transaction of its own, that delivery delivered of message failed at
// now (NoteFailure), and answers the row as the table holds it after, read whole. It fails the
// test unless NoteFailure answered that row without the message's bytes.
func noteFailureAt(t *testing.T, st *store.Store, message IntakeMessage, delivered int, lastError string, now time.Time) IntakeFailure {
	t.Helper()
	var answered IntakeFailure
	inTx(t, st, func(tx pgx.Tx) {
		var err error
		answered, err = NewStore().NoteFailure(context.Background(), tx, message, delivered, lastError, now)
		must(t, err)
	})
	whole, found := failureOf(t, st, answered.ID)
	if !found || !reflect.DeepEqual(answered, asAnswered(whole)) {
		t.Fatalf("NoteFailure answered\n%#v\nwant the row it wrote without the message's bytes\n%#v", answered, asAnswered(whole))
	}
	return whole
}

// Every statement of intake_failures.go keeps its row as its rule says, read back whole from the
// table: what it writes, what it keeps, and the row it leaves alone. One row per statement, each
// on a database of its own.
func TestEveryIntakeFailuresStatementKeepsItsRow(t *testing.T) {
	ctx := context.Background()
	records := NewStore()
	at := time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)
	later := at.Add(5 * time.Minute)
	const failure = "apply the fact: the database refused the write"

	// opened is message's open failure as NoteFailure writes it at delivery delivered, read whole.
	opened := func(t *testing.T, st *store.Store, message IntakeMessage, delivered int, now time.Time) IntakeFailure {
		t.Helper()
		return noteFailureAt(t, st, message, delivered, failure, now)
	}
	// givenUp is message failed at at and given up at the bound at at, read whole.
	givenUp := func(t *testing.T, st *store.Store, message IntakeMessage) IntakeFailure {
		t.Helper()
		return giveUpAt(t, st, message, at)
	}

	for _, row := range []struct {
		statement string
		run       func(t *testing.T, st *store.Store)
	}{
		{"NoteRedelivery of a message with no row", func(t *testing.T, st *store.Store) {
			message := wholeMessage("LEGION", 41)
			var got IntakeFailure
			inTx(t, st, func(tx pgx.Tx) {
				var err error
				got, err = records.NoteRedelivery(ctx, tx, message, 2, at)
				must(t, err)
			})
			want := IntakeFailure{ID: got.ID, IntakeMessage: message, FirstFailedAt: at, UpdatedAt: at, Deliveries: 2}
			if !reflect.DeepEqual(got, asAnswered(want)) {
				t.Fatalf("NoteRedelivery answered %#v, want %#v", got, want)
			}
			holds(t, st, "NoteRedelivery", want)
		}},
		{"NoteRedelivery of an open failure", func(t *testing.T, st *store.Store) {
			message := failingMessage("LEGION", 41)
			first := opened(t, st, message, 1, at)
			var got IntakeFailure
			inTx(t, st, func(tx pgx.Tx) {
				var err error
				got, err = records.NoteRedelivery(ctx, tx, message, 2, later)
				must(t, err)
			})
			want := first
			want.UpdatedAt, want.Deliveries = later, 2
			if !reflect.DeepEqual(got, asAnswered(want)) {
				t.Fatalf("NoteRedelivery answered %#v, want %#v: the first failure's time and its ended delivery kept", got, want)
			}
			holds(t, st, "NoteRedelivery", want)
		}},
		{"NoteRedelivery of a given-up record", func(t *testing.T, st *store.Store) {
			gave := givenUp(t, st, failingMessage("LEGION", 41))
			var got IntakeFailure
			inTx(t, st, func(tx pgx.Tx) {
				var err error
				got, err = records.NoteRedelivery(ctx, tx, failingMessage("LEGION", 41), 22, later)
				must(t, err)
			})
			if !reflect.DeepEqual(got, asAnswered(gave)) {
				t.Fatalf("NoteRedelivery of a given-up record answered %#v, want the record as it stood, %#v", got, gave)
			}
			holds(t, st, "NoteRedelivery", gave)
		}},
		{"NoteFailure at the first delivery", func(t *testing.T, st *store.Store) {
			message := wholeMessage("LEGION", 41)
			got := opened(t, st, message, 1, at)
			want := IntakeFailure{ID: got.ID, IntakeMessage: message, FirstFailedAt: at, UpdatedAt: at, Deliveries: 1, EndedDelivery: 1,
				LastError: failure}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("NoteFailure wrote %#v, want %#v", got, want)
			}
			holds(t, st, "NoteFailure", want)
		}},
		{"NoteFailure of an open failure, with a far side's error past its bound", func(t *testing.T, st *store.Store) {
			message := failingMessage("LEGION", 41)
			first := opened(t, st, message, 1, at)
			long := "GitHub answered: " + strings.Repeat("rate limit exceeded; ", 200) + "\x00"
			var got IntakeFailure
			inTx(t, st, func(tx pgx.Tx) {
				var err error
				got, err = records.NoteFailure(ctx, tx, message, 3, long, later)
				must(t, err)
			})
			want := first
			want.UpdatedAt, want.Deliveries, want.EndedDelivery, want.LastError = later, 3, 3, storetext.FarSideError(long)
			if !reflect.DeepEqual(got, asAnswered(want)) {
				t.Fatalf("NoteFailure answered %#v, want %#v", got, want)
			}
			holds(t, st, "NoteFailure", want)
		}},
		{"NoteFailure of a given-up record", func(t *testing.T, st *store.Store) {
			gave := givenUp(t, st, failingMessage("LEGION", 41))
			var got IntakeFailure
			inTx(t, st, func(tx pgx.Tx) {
				var err error
				got, err = records.NoteFailure(ctx, tx, failingMessage("LEGION", 41), 1, "a later failure", later)
				must(t, err)
			})
			if !reflect.DeepEqual(got, asAnswered(gave)) {
				t.Fatalf("NoteFailure of a given-up record answered %#v, want the record as it stood, %#v", got, gave)
			}
			holds(t, st, "NoteFailure", gave)
		}},
		{"GiveUpIntakeMessage", func(t *testing.T, st *store.Store) {
			first := opened(t, st, failingMessage("LEGION", 41), 21, at)
			const sentence = "delivery 22 did not end: the daemon stopped or crashed while it ran; " + failure
			var got IntakeFailure
			inTx(t, st, func(tx pgx.Tx) {
				var ok bool
				var err error
				got, ok, err = records.GiveUpIntakeMessage(ctx, tx, "LEGION", first.ID, GiveUpDidNotEnd, sentence, later)
				must(t, err)
				if !ok {
					t.Fatalf("the give-up of open failure %d gave nothing up", first.ID)
				}
				if _, again, err := records.GiveUpIntakeMessage(ctx, tx, "LEGION", first.ID, GiveUpBound, failure, later); err != nil || again {
					t.Fatalf("a second give-up of %d = %t, %v; want nothing given up twice", first.ID, again, err)
				}
			})
			want := first
			want.GivenUpAt, want.GiveUpReason, want.LastError, want.UpdatedAt = later, GiveUpDidNotEnd, sentence, later
			if !reflect.DeepEqual(got, asAnswered(want)) {
				t.Fatalf("GiveUpIntakeMessage answered %#v, want %#v", got, want)
			}
			holds(t, st, "GiveUpIntakeMessage", want)
		}},
		{"RecordUndecodable", func(t *testing.T, st *store.Store) {
			message := wholeMessage("LEGION", 41)
			const decode = "Envoy envelope payload is not JSON"
			var got IntakeFailure
			inTx(t, st, func(tx pgx.Tx) {
				var ok bool
				var err error
				got, ok, err = records.RecordUndecodable(ctx, tx, message, 1, decode, at)
				must(t, err)
				if !ok {
					t.Fatalf("RecordUndecodable of a message with no row recorded nothing")
				}
			})
			undecoded := message
			undecoded.EventSource, undecoded.EventID = "", ""
			want := IntakeFailure{ID: got.ID, IntakeMessage: undecoded, FirstFailedAt: at, UpdatedAt: at, Deliveries: 1, EndedDelivery: 1,
				LastError: decode, GivenUpAt: at, GiveUpReason: GiveUpUndecodable}
			if !reflect.DeepEqual(got, asAnswered(want)) {
				t.Fatalf("RecordUndecodable answered %#v, want %#v", got, want)
			}
			holds(t, st, "RecordUndecodable", want)
			// The message comes back when its termination was lost: no second record, no change.
			inTx(t, st, func(tx pgx.Tx) {
				if _, ok, err := records.RecordUndecodable(ctx, tx, message, 2, decode, later); err != nil || ok {
					t.Fatalf("RecordUndecodable of a message it recorded = %t, %v; want nothing recorded again", ok, err)
				}
			})
			holds(t, st, "a second RecordUndecodable", want)
		}},
		{"ClearIntakeFailure", func(t *testing.T, st *store.Store) {
			open := opened(t, st, failingMessage("LEGION", 41), 1, at)
			gave := givenUp(t, st, failingMessage("LEGION", 42))
			inTx(t, st, func(tx pgx.Tx) {
				for _, failure := range []IntakeFailure{open, gave} {
					cleared, err := records.ClearIntakeFailure(ctx, tx, "LEGION", failure.Consumer, failure.StreamSeq, failure.StoredAt)
					must(t, err)
					if cleared != !failure.GivenUp() {
						t.Fatalf("ClearIntakeFailure of record %d (given up %t) cleared %t, want the open failure alone cleared", failure.ID, failure.GivenUp(), cleared)
					}
				}
			})
			if _, found := failureOf(t, st, open.ID); found {
				t.Fatalf("the open failure %d stands after ClearIntakeFailure", open.ID)
			}
			holds(t, st, "ClearIntakeFailure", gave)
		}},
		{"ListPassedFailures", func(t *testing.T, st *store.Store) {
			below := opened(t, st, failingMessage("LEGION", 41), 1, at)
			at42 := opened(t, st, failingMessage("LEGION", 42), 1, later)
			opened(t, st, failingMessage("LEGION", 43), 1, at)
			givenUp(t, st, failingMessage("LEGION", 40))
			var got []OpenFailure
			inTx(t, st, func(tx pgx.Tx) {
				var err error
				got, err = records.ListPassedFailures(ctx, tx, "LEGION", below.Consumer, 42)
				must(t, err)
			})
			want := []OpenFailure{{ID: below.ID, StreamSeq: 41}, {ID: at42.ID, StreamSeq: 42}}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("ListPassedFailures through 42 = %#v, want the open failures at or below it in stream order, %#v", got, want)
			}
		}},
		{"SweepPassedFailures", func(t *testing.T, st *store.Store) {
			listed := at.Add(time.Minute)
			passed := opened(t, st, failingMessage("LEGION", 41), 3, at)
			touched := opened(t, st, failingMessage("LEGION", 42), 3, listed.Add(time.Second))
			var got []IntakeFailure
			inTx(t, st, func(tx pgx.Tx) {
				var err error
				got, err = records.SweepPassedFailures(ctx, tx, "LEGION", []int64{passed.ID, touched.ID}, listed, later)
				must(t, err)
			})
			want := passed
			want.GivenUpAt, want.GiveUpReason, want.LastError, want.UpdatedAt = later, GiveUpConsumerMovedPast, ConsumerMovedPastError, later
			if !reflect.DeepEqual(got, []IntakeFailure{asAnswered(want)}) {
				t.Fatalf("SweepPassedFailures answered %#v, want the one record no statement touched since the listing, %#v", got, want)
			}
			holds(t, st, "SweepPassedFailures", want)
			holds(t, st, "SweepPassedFailures", touched)
		}},
		{"OpenIntakeGiveUps", func(t *testing.T, st *store.Store) {
			dispatch := givenUp(t, st, failingMessage("LEGION", 41))
			wake := failingMessage("LEGION", 7)
			wake.Consumer = intakeConsumers("LEGION")[2]
			newest := giveUpAt(t, st, wake, later)
			newer := giveUpAt(t, st, failingMessage("LEGION", 44), at.Add(time.Minute))
			opened(t, st, failingMessage("LEGION", 50), 1, at)
			closed := givenUp(t, st, failingMessage("LEGION", 60))
			inTx(t, st, func(tx pgx.Tx) {
				if ok, err := records.CloseIntakeGiveUp(ctx, tx, "LEGION", closed.ID, IntakeApplied, "operator", later); err != nil || !ok {
					t.Fatalf("close record %d = %t, %v", closed.ID, ok, err)
				}
			})
			var got IntakeGiveUps
			inTx(t, st, func(tx pgx.Tx) {
				var err error
				got, err = records.OpenIntakeGiveUps(ctx, tx, "LEGION", 2)
				must(t, err)
			})
			wantCounts := []IntakeGiveUpCount{{Consumer: dispatch.Consumer, Open: 2, NewestSeq: 44}, {Consumer: wake.Consumer, Open: 1, NewestSeq: 7}}
			if !reflect.DeepEqual(got.Consumers, wantCounts) {
				t.Errorf("OpenIntakeGiveUps counts %#v, want %#v", got.Consumers, wantCounts)
			}
			var ids []int64
			for _, failure := range got.Newest {
				ids = append(ids, failure.ID)
				if failure.Headers != nil || failure.Payload != nil {
					t.Errorf("OpenIntakeGiveUps read the message bytes of record %d, which the state does not show", failure.ID)
				}
			}
			if want := []int64{newest.ID, newer.ID}; !reflect.DeepEqual(ids, want) {
				t.Errorf("OpenIntakeGiveUps lists %v, want the two newest open give-ups %v", ids, want)
			}
			if len(got.Newest) == 2 {
				want := newest
				want.Headers, want.Payload = nil, nil
				if !reflect.DeepEqual(got.Newest[0], want) {
					t.Errorf("OpenIntakeGiveUps lists %#v, want %#v", got.Newest[0], want)
				}
			}
		}},
		{"ClaimIntakeReplay", func(t *testing.T, st *store.Store) {
			gave := givenUp(t, st, failingMessage("LEGION", 41))
			var got IntakeReplayClaim
			inTx(t, st, func(tx pgx.Tx) {
				var err error
				got, err = records.ClaimIntakeReplay(ctx, tx, "LEGION", IntakeSelector{ID: gave.ID}, 100, time.Minute, later)
				must(t, err)
			})
			want := gave
			want.ReplayClaimedAt, want.UpdatedAt = later, later
			if !reflect.DeepEqual(got, IntakeReplayClaim{Records: []IntakeFailure{want}}) {
				t.Fatalf("ClaimIntakeReplay answered %#v, want %#v", got, want)
			}
			holds(t, st, "ClaimIntakeReplay", want)
		}},
		{"CloseIntakeGiveUp", func(t *testing.T, st *store.Store) {
			gave := givenUp(t, st, failingMessage("LEGION", 41))
			inTx(t, st, func(tx pgx.Tx) {
				_, err := records.ClaimIntakeReplay(ctx, tx, "LEGION", IntakeSelector{ID: gave.ID}, 100, time.Minute, later)
				must(t, err)
				if ok, err := records.CloseIntakeGiveUp(ctx, tx, "LEGION", gave.ID, IntakeAlreadyApplied, "controller session ses-1", later); err != nil || !ok {
					t.Fatalf("CloseIntakeGiveUp = %t, %v", ok, err)
				}
				if ok, err := records.CloseIntakeGiveUp(ctx, tx, "LEGION", gave.ID, IntakeApplied, "operator", later); err != nil || ok {
					t.Fatalf("a second CloseIntakeGiveUp = %t, %v; want a record a replay closed closed once", ok, err)
				}
			})
			want := gave
			want.ClosedAt, want.ClosedHow, want.ClosedBy, want.UpdatedAt = later, IntakeAlreadyApplied, "controller session ses-1", later
			holds(t, st, "CloseIntakeGiveUp", want)
		}},
		{"FailIntakeReplay", func(t *testing.T, st *store.Store) {
			gave := givenUp(t, st, failingMessage("LEGION", 41))
			inTx(t, st, func(tx pgx.Tx) {
				_, err := records.ClaimIntakeReplay(ctx, tx, "LEGION", IntakeSelector{ID: gave.ID}, 100, time.Minute, later)
				must(t, err)
				if ok, err := records.FailIntakeReplay(ctx, tx, "LEGION", gave.ID, "the handler failed again", later); err != nil || !ok {
					t.Fatalf("FailIntakeReplay = %t, %v", ok, err)
				}
			})
			want := gave
			want.LastError, want.UpdatedAt = "the handler failed again", later
			holds(t, st, "FailIntakeReplay", want)
		}},
		{"DiscardIntakeGiveUps", func(t *testing.T, st *store.Store) {
			gave := givenUp(t, st, failingMessage("LEGION", 41))
			var ids []int64
			inTx(t, st, func(tx pgx.Tx) {
				var err error
				ids, err = records.DiscardIntakeGiveUps(ctx, tx, "LEGION", IntakeSelector{ID: gave.ID}, "operator", "the event is of an issue deleted since", time.Minute, later)
				must(t, err)
			})
			if !reflect.DeepEqual(ids, []int64{gave.ID}) {
				t.Fatalf("DiscardIntakeGiveUps answered %v, want [%d]", ids, gave.ID)
			}
			want := gave
			want.ClosedAt, want.ClosedHow, want.ClosedBy, want.UpdatedAt = later, IntakeDiscarded, "operator", later
			want.DiscardedAt, want.DiscardedBy, want.DiscardReason = later, "operator", "the event is of an issue deleted since"
			holds(t, st, "DiscardIntakeGiveUps", want)
		}},
		{"countIntakeGiveUp", func(t *testing.T, st *store.Store) {
			giveUp := NoticeGiveUp{ID: 41, Consumer: "legion-go-LEGION-dispatch", Subject: "notifications.dispatch.issue.LEGION-208.issue.updated"}
			var id int64
			inTx(t, st, func(tx pgx.Tx) {
				var err error
				id, err = records.countIntakeGiveUp(ctx, tx, "LEGION", giveUp, at)
				must(t, err)
			})
			notices := intakeNotices(t, st)
			if len(notices) != 1 || notices[0].row.ID != id {
				t.Fatalf("countIntakeGiveUp answered row %d and the outbox holds %#v, want that one row", id, notices)
			}
			if want := (ControllerNotice{Kind: IntakeGaveUpNotice, Count: 1, GiveUps: []NoticeGiveUp{giveUp}}); !reflect.DeepEqual(notices[0].notice, want) {
				t.Fatalf("the notice reads %#v, want %#v", notices[0].notice, want)
			}
		}},
	} {
		t.Run(row.statement, func(t *testing.T) {
			row.run(t, migratedStore(t))
		})
	}
}

// A replay's claim holds its record against a second replay and a discard for as long as the
// caller's bound says a replay can run (the intake passes its HandlerLimit): a second claim of the
// id and a discard of the id are refused as being replayed, and a bulk discard passes over it. A
// claim older than the bound is stale, so its record is claimed again, and so is a record whose
// replay failed, since the failure gives the claim back.
func TestIntakeFailuresReplayClaimsGoStaleAtTheCallersBound(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	at := time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)
	const bound = 15 * time.Minute
	claimed := giveUpAt(t, st, failingMessage("LEGION", 41), at)
	other := giveUpAt(t, st, failingMessage("LEGION", 42), at)

	claim := func(id int64, now time.Time) error {
		return st.Tx(ctx, func(tx pgx.Tx) error {
			_, err := records.ClaimIntakeReplay(ctx, tx, "LEGION", IntakeSelector{ID: id}, 100, bound, now)
			return err
		})
	}
	must(t, claim(claimed.ID, at))
	if err := claim(claimed.ID, at.Add(bound-time.Second)); !errors.Is(err, ErrIntakeGiveUpBeingReplayed) {
		t.Fatalf("a second claim within the bound = %v, want %v", err, ErrIntakeGiveUpBeingReplayed)
	}
	inTx(t, st, func(tx pgx.Tx) {
		if _, err := records.DiscardIntakeGiveUps(ctx, tx, "LEGION", IntakeSelector{ID: claimed.ID}, "operator", "not wanted", bound, at.Add(time.Minute)); !errors.Is(err, ErrIntakeGiveUpBeingReplayed) {
			t.Fatalf("a discard of the claimed id = %v, want %v", err, ErrIntakeGiveUpBeingReplayed)
		}
		ids, err := records.DiscardIntakeGiveUps(ctx, tx, "LEGION", IntakeSelector{Consumer: claimed.Consumer, ThroughSeq: 50}, "operator", "not wanted", bound, at.Add(time.Minute))
		must(t, err)
		if !reflect.DeepEqual(ids, []int64{other.ID}) {
			t.Fatalf("a bulk discard closed %v, want the unclaimed record %d alone", ids, other.ID)
		}
	})
	if err := claim(claimed.ID, at.Add(bound+time.Second)); err != nil {
		t.Fatalf("a claim past the bound of a stale claim = %v, want it taken", err)
	}
	inTx(t, st, func(tx pgx.Tx) {
		if ok, err := records.FailIntakeReplay(ctx, tx, "LEGION", claimed.ID, "still failing", at.Add(bound+2*time.Second)); err != nil || !ok {
			t.Fatalf("FailIntakeReplay = %t, %v", ok, err)
		}
	})
	if err := claim(claimed.ID, at.Add(bound+3*time.Second)); err != nil {
		t.Fatalf("a claim after a failed replay gave the claim back = %v, want it taken", err)
	}
}

// A record is an open failure until it is given up, then an open give-up until a replay or a
// discard closes it. A discard closes an open give-up and keeps why, who and when; a discarded
// record can be claimed by its id and replayed, and the replay's close keeps the discard; a
// record a replay closed, or one discarded, is refused by its id with how and when it closed; an
// open failure is no give-up and is not found; and a discard with no reason is refused.
func TestIntakeFailuresDiscardClosesAnOpenRecordAndRefusesAClosedOne(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	at := time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)
	discardAt, replayAt := at.Add(time.Minute), at.Add(2*time.Minute)
	const reason = "the issue was deleted"
	gave := giveUpAt(t, st, failingMessage("LEGION", 41), at)
	var open IntakeFailure
	inTx(t, st, func(tx pgx.Tx) {
		var err error
		open, err = records.NoteFailure(ctx, tx, failingMessage("LEGION", 42), 1, "the handler failed", at)
		must(t, err)
	})

	closedAs := func(err error, how IntakeClosedHow, when time.Time) bool {
		var closed *IntakeGiveUpClosedError
		return errors.As(err, &closed) && closed.ID == gave.ID && closed.How == how && closed.At.Equal(when) &&
			strings.Contains(err.Error(), string(how)) && strings.Contains(err.Error(), when.Format(time.RFC3339))
	}
	inTx(t, st, func(tx pgx.Tx) {
		if _, err := records.DiscardIntakeGiveUps(ctx, tx, "LEGION", IntakeSelector{ID: gave.ID}, "operator", "", time.Minute, discardAt); err == nil {
			t.Fatalf("a discard with no reason was taken")
		}
		for _, selector := range []IntakeSelector{{ID: open.ID}, {ID: gave.ID + open.ID + 100}} {
			if _, err := records.DiscardIntakeGiveUps(ctx, tx, "LEGION", selector, "operator", reason, time.Minute, discardAt); !errors.Is(err, ErrIntakeGiveUpNotFound) {
				t.Fatalf("a discard of %#v = %v, want %v: an open failure is no give-up", selector, err, ErrIntakeGiveUpNotFound)
			}
			if _, err := records.ClaimIntakeReplay(ctx, tx, "LEGION", selector, 100, time.Minute, discardAt); !errors.Is(err, ErrIntakeGiveUpNotFound) {
				t.Fatalf("a replay of %#v = %v, want %v", selector, err, ErrIntakeGiveUpNotFound)
			}
		}
		ids, err := records.DiscardIntakeGiveUps(ctx, tx, "LEGION", IntakeSelector{ID: gave.ID}, "operator", reason, time.Minute, discardAt)
		must(t, err)
		if !reflect.DeepEqual(ids, []int64{gave.ID}) {
			t.Fatalf("the discard closed %v, want [%d]", ids, gave.ID)
		}
		if _, err := records.DiscardIntakeGiveUps(ctx, tx, "LEGION", IntakeSelector{ID: gave.ID}, "operator", reason, time.Minute, replayAt); !closedAs(err, IntakeDiscarded, discardAt) {
			t.Fatalf("a second discard = %v, want it refused as discarded at %s", err, discardAt)
		}
		bulk, err := records.ClaimIntakeReplay(ctx, tx, "LEGION", IntakeSelector{Consumer: gave.Consumer, ThroughSeq: 100}, 100, time.Minute, replayAt)
		must(t, err)
		if len(bulk.Records) != 0 || bulk.Unclaimed != 0 {
			t.Fatalf("a bulk replay took %#v, want no discarded record", bulk)
		}
		one, err := records.ClaimIntakeReplay(ctx, tx, "LEGION", IntakeSelector{ID: gave.ID}, 100, time.Minute, replayAt)
		must(t, err)
		if len(one.Records) != 1 || one.Records[0].ID != gave.ID {
			t.Fatalf("a replay of the discarded record by its id took %#v, want it", one)
		}
		if ok, err := records.CloseIntakeGiveUp(ctx, tx, "LEGION", gave.ID, IntakeApplied, "operator", replayAt); err != nil || !ok {
			t.Fatalf("the replay's close of the discarded record = %t, %v", ok, err)
		}
	})
	want := gave
	want.ClosedAt, want.ClosedHow, want.ClosedBy, want.UpdatedAt = replayAt, IntakeApplied, "operator", replayAt
	want.DiscardedAt, want.DiscardedBy, want.DiscardReason = discardAt, "operator", reason
	holds(t, st, "the replay of a discarded record", want)
	inTx(t, st, func(tx pgx.Tx) {
		if _, err := records.ClaimIntakeReplay(ctx, tx, "LEGION", IntakeSelector{ID: gave.ID}, 100, time.Minute, replayAt.Add(time.Minute)); !closedAs(err, IntakeApplied, replayAt) {
			t.Fatalf("a replay of a replayed record = %v, want it refused as applied at %s", err, replayAt)
		}
		if _, err := records.DiscardIntakeGiveUps(ctx, tx, "LEGION", IntakeSelector{ID: gave.ID}, "operator", reason, time.Minute, replayAt.Add(time.Minute)); !closedAs(err, IntakeApplied, replayAt) {
			t.Fatalf("a discard of a replayed record = %v, want it refused as applied at %s", err, replayAt)
		}
	})
}

// The bulk form takes a consumer's open give-ups at or below a stream sequence, in stream order
// whatever order their ids are in, at most the caller's batch, and says how many it left: neither
// an open failure, nor another consumer's record, nor one above the sequence is taken.
func TestIntakeFailuresBulkSelectorTakesAConsumersOpenRecordsInStreamOrder(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	at := time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)
	var bySeq []IntakeFailure
	for _, seq := range []uint64{44, 41, 43, 42} {
		bySeq = append(bySeq, giveUpAt(t, st, failingMessage("LEGION", seq), at))
	}
	giveUpAt(t, st, failingMessage("LEGION", 90), at)
	wake := failingMessage("LEGION", 10)
	wake.Consumer = intakeConsumers("LEGION")[2]
	giveUpAt(t, st, wake, at)
	inTx(t, st, func(tx pgx.Tx) {
		_, err := records.NoteFailure(ctx, tx, failingMessage("LEGION", 40), 1, "the handler failed", at)
		must(t, err)
	})

	var claim IntakeReplayClaim
	inTx(t, st, func(tx pgx.Tx) {
		var err error
		claim, err = records.ClaimIntakeReplay(ctx, tx, "LEGION", IntakeSelector{Consumer: bySeq[0].Consumer, ThroughSeq: 44}, 3, time.Minute, at)
		must(t, err)
	})
	var seqs []uint64
	for _, record := range claim.Records {
		seqs = append(seqs, record.StreamSeq)
	}
	if !reflect.DeepEqual(seqs, []uint64{41, 42, 43}) || claim.Unclaimed != 1 {
		t.Fatalf("a bulk claim of three through 44 took the sequences %v and left %d, want [41 42 43] and 1", seqs, claim.Unclaimed)
	}
}

// A record carries its project, and no statement of one project reads or writes another's, so
// the daemons of two projects sharing one database never touch each other's records: another
// project's id is not found, its failures are not listed, cleared, swept or given up, and its
// give-ups are not counted in the state.
func TestIntakeFailuresOfAnotherProjectAreNeitherReadNorWritten(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	at := time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)
	later := at.Add(time.Hour)
	theirs := giveUpAt(t, st, failingMessage("OTHER", 41), at)
	open := noteFailureAt(t, st, failingMessage("OTHER", 42), 1, "the handler failed", at)

	inTx(t, st, func(tx pgx.Tx) {
		if _, err := records.ClaimIntakeReplay(ctx, tx, "LEGION", IntakeSelector{ID: theirs.ID}, 100, time.Minute, later); !errors.Is(err, ErrIntakeGiveUpNotFound) {
			t.Errorf("LEGION's replay of OTHER's record = %v, want %v", err, ErrIntakeGiveUpNotFound)
		}
		if _, err := records.DiscardIntakeGiveUps(ctx, tx, "LEGION", IntakeSelector{ID: theirs.ID}, "operator", "not ours", time.Minute, later); !errors.Is(err, ErrIntakeGiveUpNotFound) {
			t.Errorf("LEGION's discard of OTHER's record = %v, want %v", err, ErrIntakeGiveUpNotFound)
		}
		if ids, err := records.DiscardIntakeGiveUps(ctx, tx, "LEGION", IntakeSelector{Consumer: theirs.Consumer, ThroughSeq: 100}, "operator", "not ours", time.Minute, later); err != nil || len(ids) != 0 {
			t.Errorf("LEGION's bulk discard of OTHER's consumer = %v, %v; want nothing closed", ids, err)
		}
		if ok, err := records.CloseIntakeGiveUp(ctx, tx, "LEGION", theirs.ID, IntakeApplied, "operator", later); err != nil || ok {
			t.Errorf("LEGION's close of OTHER's record = %t, %v; want nothing closed", ok, err)
		}
		if ok, err := records.FailIntakeReplay(ctx, tx, "LEGION", theirs.ID, "not ours", later); err != nil || ok {
			t.Errorf("LEGION's failed replay of OTHER's record = %t, %v; want nothing written", ok, err)
		}
		if listed, err := records.ListPassedFailures(ctx, tx, "LEGION", open.Consumer, 100); err != nil || len(listed) != 0 {
			t.Errorf("LEGION's list of OTHER's consumer = %v, %v; want none", listed, err)
		}
		if swept, err := records.SweepPassedFailures(ctx, tx, "LEGION", []int64{open.ID}, later, later); err != nil || len(swept) != 0 {
			t.Errorf("LEGION's sweep of OTHER's open failure = %v, %v; want none given up", swept, err)
		}
		if _, ok, err := records.GiveUpIntakeMessage(ctx, tx, "LEGION", open.ID, GiveUpBound, "not ours", later); err != nil || ok {
			t.Errorf("LEGION's give-up of OTHER's open failure = %t, %v; want nothing given up", ok, err)
		}
		if cleared, err := records.ClearIntakeFailure(ctx, tx, "LEGION", open.Consumer, open.StreamSeq, open.StoredAt); err != nil || cleared {
			t.Errorf("LEGION's clear of OTHER's open failure = %t, %v; want nothing cleared", cleared, err)
		}
		giveUps, err := records.OpenIntakeGiveUps(ctx, tx, "LEGION", 50)
		must(t, err)
		if len(giveUps.Consumers) != 0 || len(giveUps.Newest) != 0 {
			t.Errorf("LEGION's state counts %#v, want none of OTHER's", giveUps)
		}
	})
	holds(t, st, "LEGION's statements", theirs)
	holds(t, st, "LEGION's statements", open)
}

// A selector names one record id, or a consumer and a stream sequence of at least 1, and never
// both or neither: a selector that does is refused before any statement runs.
func TestIntakeFailuresSelectorNamesExactlyOneForm(t *testing.T) {
	for _, tc := range []struct {
		selector IntakeSelector
		valid    bool
	}{
		{IntakeSelector{ID: 41}, true},
		{IntakeSelector{Consumer: "legion-go-LEGION-dispatch", ThroughSeq: 1}, true},
		{IntakeSelector{}, false},
		{IntakeSelector{ID: 41, Consumer: "legion-go-LEGION-dispatch", ThroughSeq: 1}, false},
		{IntakeSelector{ID: -1}, false},
		{IntakeSelector{Consumer: "legion-go-LEGION-dispatch"}, false},
		{IntakeSelector{ThroughSeq: 5}, false},
		{IntakeSelector{ID: 41, ThroughSeq: 5}, false},
	} {
		if err := tc.selector.Validate(); (err == nil) != tc.valid {
			t.Errorf("Validate(%#v) = %v, want valid %t", tc.selector, err, tc.valid)
		}
	}
}

// The header block is the NATS/1.0 form: the version line, each key in sorted order with each of
// its values in order, then the blank line, each key and value written as its raw bytes, so a
// value that is no UTF-8 is kept as it came. A message with no header has no block.
func TestIntakeFailuresKeepTheHeaderBlockInTheNATSForm(t *testing.T) {
	got := IntakeHeaders(map[string][]string{"Nats-Msg-Id": {"dispatch-\x00\xff"}, "B": {"2", "1"}, "A": {"x"}})
	want := "NATS/1.0\r\nA: x\r\nB: 2\r\nB: 1\r\nNats-Msg-Id: dispatch-\x00\xff\r\n\r\n"
	if string(got) != want {
		t.Errorf("IntakeHeaders = %q, want %q", got, want)
	}
	if got := IntakeHeaders(nil); got != nil {
		t.Errorf("IntakeHeaders of no header = %q, want nil, which the column keeps as null", got)
	}
}
