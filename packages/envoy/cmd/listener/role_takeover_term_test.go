package main

// The takeover passes the kept lane's sweep runs are the lease holder's: each copy and record write
// of them is held to the receive term the sweep runs under and to the lease's loss deadline
// (keptSweepConfig, takeoverWriteContext). And a record's write lands only over the body its pass
// read (bus.Client.WriteTakeoverRecord), so a pass that comes late writes nothing over another
// pass's newer body or over the record's removal.

import (
	"context"
	"errors"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
)

const (
	// termRole is the role a row's takeover took, termPrevious the session it was taken from and
	// termTaker the session that took it.
	termRole     = "term-role"
	termPrevious = "ses_term_previous"
	termTaker    = "ses_term_taker"
)

// startTermTakeover stores one role message of termRole on termPrevious's own subject, has
// termTaker claim the role, and writes the takeover record a claim's try writes for termPrevious,
// with its floor already missed, so the record's next pass reads termPrevious's subject from the
// hour before the claim and copies that message. It returns the message's event id and the record
// as the stream holds it.
func startTermTakeover(t *testing.T, harness listenerDeliveryHarness) (string, bus.StoredTakeoverRecord) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	item := listenerTestEnvelope(contracts.RoleTopicPrefix+termRole, "term-row."+id.New())
	if _, err := harness.client.JS().Publish(contracts.LegacyAgentSubject(termPrevious), marshalListenerEnvelope(t, item)); err != nil {
		t.Fatalf("store a role message of %s on %s's subject: %v", termRole, termPrevious, err)
	}
	if _, err := harness.registry.SetRole(termTaker, "test-machine", termRole, false); err != nil {
		t.Fatalf("%s claims %s: %v", termTaker, termRole, err)
	}
	record := bus.TakeoverRecord{Role: termRole, Previous: termPrevious, ClaimedAt: time.Now().UnixMilli(), FloorMissed: true}
	if _, err := harness.client.WriteTakeoverRecord(ctx, bus.TakeoverSubject(termRole, termPrevious, id.New()), record, 0); err != nil {
		t.Fatalf("write the takeover record of %s: %v", termRole, err)
	}
	records := termRecords(t, harness)
	if len(records) != 1 {
		t.Fatalf("the stream holds %d takeover records of %s, want the one this row wrote", len(records), termRole)
	}
	return item.EventID, records[0]
}

// termRecords is every takeover record of termRole the stream holds.
func termRecords(t *testing.T, harness listenerDeliveryHarness) []bus.StoredTakeoverRecord {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	records, err := harness.client.TakeoverRecords(ctx, termRole)
	if err != nil {
		t.Fatalf("read the takeover records of %s: %v", termRole, err)
	}
	return records
}

// A takeover pass the kept lane's sweep reaches once its receive term is over, or once the lease's
// loss deadline has passed - the lease stepped down, or another pod may hold it - writes nothing:
// no copy on the role's kept lane, and the record as the stream held it. The control is the same
// sweep within a live term, which copies the message and writes the record on.
func TestATakeoverPassReachedAfterTheLeaseSteppedDownWritesNothing(t *testing.T) {
	ended, end := context.WithCancel(context.Background())
	end()
	for _, tc := range []struct {
		name   string
		term   *relayTerm
		writes bool
	}{
		{name: "the lease stepped down: its loss deadline has passed", term: &relayTerm{ctx: context.Background(), untilLoss: func() time.Duration { return 0 }}},
		{name: "the receive term ended", term: &relayTerm{ctx: ended, untilLoss: func() time.Duration { return time.Minute }}},
		{name: "control: a live term", term: &relayTerm{ctx: context.Background(), untilLoss: func() time.Duration { return time.Minute }}, writes: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			harness := newListenerDeliveryHarness(t)
			eventID, stored := startTermTakeover(t, harness)
			relays := &sessionRelays{term: tc.term}
			cfg := keptSweepConfig(harness.config, relays)

			ctx, cancel := context.WithTimeout(context.Background(), roleKeepHandoverWindow)
			defer cancel()
			handOverSweptLane(ctx, cfg, termRole, []bus.StoredTakeoverRecord{stored})

			copies := copiesOf(storedEnvelopes(t, harness.client, bus.KeptRoleSubject(termRole)), eventID)
			records := termRecords(t, harness)
			if len(records) != 1 {
				t.Fatalf("the stream holds %d takeover records of %s after the sweep's pass, want 1", len(records), termRole)
			}
			after := records[0]
			if !tc.writes {
				if copies != 0 {
					t.Errorf("the role's kept lane holds %d copies of %s after a pass this pod's term no longer allows; want none", copies, eventID)
				}
				if after.Sequence != stored.Sequence || after.Record != stored.Record {
					t.Errorf("the takeover record is %+v at %d after a pass this pod's term no longer allows; want it as it stood, %+v at %d", after.Record, after.Sequence, stored.Record, stored.Sequence)
				}
				return
			}
			if copies != 1 {
				t.Errorf("the role's kept lane holds %d copies of %s after a pass within a live term; want 1", copies, eventID)
			}
			if after.Sequence == stored.Sequence || after.Record.Next == 0 {
				t.Errorf("the takeover record is %+v at %d after a pass within a live term; want it written on past %d", after.Record, after.Sequence, stored.Sequence)
			}
		})
	}
}

// A pass's write of a takeover record lands only over the body that pass read: one another pass
// wrote over, and one the stream removed, between the pass's read and its write, refuse it
// (bus.ErrTakeoverRecordMoved), and the stream holds the newer body, or no record, as that left
// it. The control is the pass with nothing between, whose write lands.
func TestALateTakeoverRecordWriteIsRefusedOverAWriteOrARemovalMadeAfterItsRead(t *testing.T) {
	newer := bus.TakeoverRecord{Role: termRole, Previous: termPrevious, ClaimedAt: 1, Next: 7}
	for _, tc := range []struct {
		name string
		// late is what another writer does between the pass's read and its write.
		late func(t *testing.T, harness listenerDeliveryHarness, stored bus.StoredTakeoverRecord)
		// want is the record the stream holds after the pass, nil for none.
		want *bus.TakeoverRecord
	}{
		{
			name: "another pass wrote the record",
			late: func(t *testing.T, harness listenerDeliveryHarness, stored bus.StoredTakeoverRecord) {
				if _, err := harness.client.WriteTakeoverRecord(context.Background(), stored.Subject, newer, stored.Sequence); err != nil {
					t.Errorf("another pass writes the record: %v", err)
				}
			},
			want: &newer,
		},
		{
			name: "the stream removed the record",
			late: func(t *testing.T, harness listenerDeliveryHarness, stored bus.StoredTakeoverRecord) {
				if err := harness.client.JS().PurgeStream(bus.Stream, &natsgo.StreamPurgeRequest{Subject: stored.Subject}); err != nil {
					t.Errorf("remove the record from the stream: %v", err)
				}
			},
		},
		{name: "control: nothing between the read and the write"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			harness := newListenerDeliveryHarness(t)
			_, stored := startTermTakeover(t, harness)
			writes := 0
			setTakeoverFaults(t, harness.config, takeoverFaultSet{
				beforeRecordWrite: func(string, string, uint64, bool) {
					writes++
					if writes == 1 && tc.late != nil {
						tc.late(t, harness, stored)
					}
				},
			})

			ctx, cancel := context.WithTimeout(context.Background(), roleKeepHandoverWindow)
			defer cancel()
			passed, _, err := runTakeoverPass(ctx, harness.config, stored)
			if writes == 0 {
				t.Fatal("the pass wrote no record, so nothing came between its read and its write")
			}
			records := termRecords(t, harness)
			if tc.late == nil {
				if err != nil {
					t.Fatalf("the pass with nothing between its read and its write answered %v; want its record written", err)
				}
				if len(records) != 1 || records[0].Sequence != passed.Sequence || records[0].Record != passed.Record {
					t.Fatalf("the stream holds %+v after the pass; want its record %+v at %d", records, passed.Record, passed.Sequence)
				}
				return
			}
			switch {
			case tc.want == nil && len(records) != 0:
				t.Errorf("the stream holds %+v after the late pass; want no record, as its removal left it", records)
			case tc.want != nil && (len(records) != 1 || records[0].Record != *tc.want):
				t.Errorf("the stream holds %+v after the late pass; want the newer body %+v", records, *tc.want)
			}
			if !errors.Is(err, bus.ErrTakeoverRecordMoved) {
				t.Errorf("the late pass answered %v; want %v", err, bus.ErrTakeoverRecordMoved)
			}
		})
	}
}
