package bus

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
)

// Every sweep pass read the newest message of every takeover record subject, finished or not, and a
// finished record, which no pass runs again, cost one stored-message read a pass for as long as the
// stream held it. A client reads a finished record once now, and again only once the count of its
// subject's messages moves: a message stored on that subject after it is read, and so is every
// unfinished record, every time. A pass cannot write over a finished record
// (ErrTakeoverRecordFinished), so the row stores that later message with a plain publish. The
// reads are read off the wire from a second connection, up to a mark the client publishes after
// each read of the records.
func TestAFinishedTakeoverRecordIsReadOnceUntilItsSubjectChanges(t *testing.T) {
	uri := testnats.URL(t)
	client, err := ConnectOwningStream([]string{uri}, WithReplicas(1))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	watcher, err := Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect the second connection: %v", err)
	}
	t.Cleanup(watcher.Close)
	const mark = "takeoverrecords.test.mark"
	wire := make(chan *nats.Msg, 64)
	for _, subject := range []string{"$JS.API.STREAM.MSG.GET." + Stream, mark} {
		if _, err := watcher.Conn.ChanSubscribe(subject, wire); err != nil {
			t.Fatalf("subscribe to %s: %v", subject, err)
		}
	}
	if err := watcher.Conn.Flush(); err != nil {
		t.Fatalf("flush the subscriptions: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	finished := TakeoverSubject("cache", "ses_finished", "attempt1")
	unfinished := TakeoverSubject("cache", "ses_unfinished", "attempt2")
	// Each write names the subject's newest message as a pass does (WriteTakeoverRecordAt); a
	// subject's first write names none.
	at := map[string]uint64{}
	write := func(subject string, record TakeoverRecord) {
		t.Helper()
		sequence, err := client.WriteTakeoverRecordAt(ctx, StoredTakeoverRecord{Subject: subject, Sequence: at[subject]}, record)
		if err != nil {
			t.Fatalf("write %s: %v", subject, err)
		}
		at[subject] = sequence
	}
	write(finished, TakeoverRecord{Role: "cache", Previous: "ses_finished", ClaimedAt: 1, Next: 7, Finished: true})
	write(unfinished, TakeoverRecord{Role: "cache", Previous: "ses_unfinished", ClaimedAt: 1, Next: 9})

	// read reads the records and returns them by subject, with how many reads of each subject's
	// newest message the wire carried.
	read := func() (map[string]TakeoverRecord, map[string]int) {
		t.Helper()
		records, err := client.TakeoverRecords(ctx, "")
		if err != nil {
			t.Fatalf("read the takeover records: %v", err)
		}
		bySubject := map[string]TakeoverRecord{}
		for _, stored := range records {
			bySubject[stored.Subject] = stored.Record
		}
		if err := client.Conn.Publish(mark, nil); err != nil {
			t.Fatalf("publish the mark: %v", err)
		}
		reads := map[string]int{}
		for {
			select {
			case msg := <-wire:
				if msg.Subject == mark {
					return bySubject, reads
				}
				var request struct {
					Last string `json:"last_by_subj"`
				}
				if err := json.Unmarshal(msg.Data, &request); err != nil {
					t.Fatalf("decode %q: %v", msg.Data, err)
				}
				reads[request.Last]++
			case <-time.After(30 * time.Second):
				t.Fatal("the mark did not come back within 30 s")
			}
		}
	}

	records, reads := read()
	if len(records) != 2 || !records[finished].Finished || records[unfinished].Finished || reads[finished] != 1 || reads[unfinished] != 1 {
		t.Fatalf("the first read answered %+v with reads %v, want both records, each read once", records, reads)
	}
	again, reads := read()
	if reads[finished] != 0 {
		t.Fatalf("the second read read the finished record %d times, want none: its subject holds what it held", reads[finished])
	}
	if reads[unfinished] != 1 || again[finished] != records[finished] || again[unfinished] != records[unfinished] {
		t.Fatalf("the second read answered %+v with reads %v, want the same records and the unfinished one read again", again, reads)
	}

	raced := TakeoverRecord{Role: "cache", Previous: "ses_finished", ClaimedAt: 1, Next: 8}
	if _, err := client.WriteTakeoverRecordAt(ctx, StoredTakeoverRecord{Subject: finished, Sequence: at[finished], Record: records[finished]}, raced); !errors.Is(err, ErrTakeoverRecordFinished) {
		t.Fatalf("a write over the finished record answered %v; want ErrTakeoverRecordFinished", err)
	}
	data, err := json.Marshal(raced)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.JS().Publish(finished, data); err != nil {
		t.Fatalf("store a later message on %s: %v", finished, err)
	}
	after, reads := read()
	if reads[finished] != 1 || after[finished] != raced {
		t.Fatalf("after a write to the finished record's subject the read answered %+v with %d reads of it, want the write, read once", after[finished], reads[finished])
	}
}

// A handover record names its role, its previous session and its successor in its subject alone:
// its stored body holds neither the role nor the previous session, so a listener from before
// handover records, which leaves out a body naming no role, never runs its passes as a takeover
// record's, and this client reads both back from the subject. A role under a project, two tokens,
// reads back whole; a second write of the handover record, a claim made again, lands nothing over
// the first; a takeover record of a claim that named no session it replaces names no successor.
func TestAHandoverRecordNamesItsRoleAndSessionsInItsSubjectAlone(t *testing.T) {
	uri := testnats.URL(t)
	client, err := ConnectOwningStream([]string{uri}, WithReplicas(1))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	const role, previous, successor = "ACME.reviewer", "ses_replaced", "ses_successor"
	subject := HandoverSubject(role, previous, successor)
	if err := client.WriteHandoverRecord(ctx, subject, TakeoverRecord{Role: role, Previous: previous, ClaimedAt: 1, Next: 4}); err != nil {
		t.Fatalf("write the handover record: %v", err)
	}
	if err := client.WriteHandoverRecord(ctx, subject, TakeoverRecord{Role: role, Previous: previous, ClaimedAt: 2}); err != nil {
		t.Fatalf("write the handover record again: %v", err)
	}
	plain := TakeoverSubject(role, previous, "attempt1")
	if err := client.WriteTakeoverRecord(ctx, plain, TakeoverRecord{Role: role, Previous: previous, ClaimedAt: 1}); err != nil {
		t.Fatalf("write the takeover record: %v", err)
	}

	stored, err := client.LastStoredMessage(ctx, subject)
	if err != nil {
		t.Fatalf("read the handover record's body: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(stored.Data, &body); err != nil {
		t.Fatalf("decode the handover record's body %s: %v", stored.Data, err)
	}
	if body["role"] != "" || body["previous"] != "" {
		t.Errorf("the handover record's body is %s; want its role and previous session empty", stored.Data)
	}

	records, err := client.TakeoverRecords(ctx, role)
	if err != nil {
		t.Fatalf("read role %s's takeover records: %v", role, err)
	}
	got := map[string]TakeoverRecord{}
	for _, record := range records {
		got[record.Subject] = record.Record
	}
	if record := got[subject]; record.Role != role || record.Previous != previous || record.Next != 4 || record.ClaimedAt != 1 {
		t.Errorf("the handover record reads back as %+v; want role %s, previous %s, next 4, claimed at 1 (the first write's)", record, role, previous)
	}
	if named, ok := TakeoverSuccessor(subject); !ok || named != successor {
		t.Errorf("TakeoverSuccessor(%s) = %q, %v; want %s", subject, named, ok, successor)
	}
	if record := got[plain]; record.Role != role || record.Previous != previous {
		t.Errorf("the takeover record reads back as %+v; want role %s, previous %s", record, role, previous)
	}
	if named, ok := TakeoverSuccessor(plain); ok {
		t.Errorf("TakeoverSuccessor(%s) = %q; want none", plain, named)
	}
}
