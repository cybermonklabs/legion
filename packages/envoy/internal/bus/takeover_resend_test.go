package bus_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The stream stores the claim's initial record but the claimant loses its reply. The sweeper
// finishes that record before the claimant reconnects. The claimant must still succeed, but its
// resend must not make the finished record unfinished again.
func TestATakeoverRecordResendCannotUndoAFinishedPass(t *testing.T) {
	uri := testnats.URL(t)
	sweeper, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sweeper.Close)
	proxy := testnats.StartProxy(t, uri)
	claimant, err := bus.ConnectOwningStream([]string{proxy.URL()}, bus.WithReplicas(1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(claimant.Close)
	const role, previous = "resend-pass", "ses_previous"
	subject := bus.TakeoverSubject(role, previous, id.New())
	initial := bus.TakeoverRecord{Role: role, Previous: previous, ClaimedAt: time.Now().UnixMilli()}
	ctx := testwait.Context(t)
	proxy.FreezeServerReplies()
	answered := make(chan error, 1)
	go func() { answered <- claimant.WriteTakeoverRecord(ctx, subject, initial) }()
	stored := testwait.Until(t, "the initial takeover record to be stored before reconnect", func() (bus.StoredTakeoverRecord, bool) {
		records, err := sweeper.TakeoverRecords(ctx, role)
		if err != nil {
			t.Fatal(err)
		}
		for _, record := range records {
			if record.Subject == subject {
				return record, true
			}
		}
		return bus.StoredTakeoverRecord{}, false
	})
	finished := stored.Record
	finished.Finished = true
	seq, err := sweeper.WriteTakeoverRecordAt(ctx, stored, finished)
	if err != nil {
		t.Fatalf("finish the takeover record while the claimant waits for its reply: %v", err)
	}
	if err := claimant.Connection().ForceReconnect(); err != nil {
		t.Fatal(err)
	}
	proxy.Restore()
	if err := testwait.Receive(t, "the claimant's write to answer after reconnect", answered); err != nil {
		t.Fatalf("the claimant's first write was stored and finished by a pass, want success after reconnect: %v", err)
	}
	last, err := sweeper.JS().GetLastMsg(bus.Stream, subject)
	if err != nil {
		t.Fatal(err)
	}
	var record bus.TakeoverRecord
	if err := json.Unmarshal(last.Data, &record); err != nil {
		t.Fatal(err)
	}
	if record != finished {
		t.Fatalf("the newest record after the resend is %+v at sequence %d, want the finished record %+v from sequence %d", record, last.Sequence, finished, seq)
	}
}
