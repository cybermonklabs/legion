package bus_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A claim's takeover record write whose first send goes unanswered finds, at its resend, another
// claim's record on its subject: the same Role and Previous, another ClaimedAt. The stream refuses
// the resend, since the subject holds a message, and the refusal wraps bus.ErrUnacknowledged,
// since the first send might have stored the record. A refused resend counts as stored only when
// the subject's newest record is this same claim (bus.Client.WriteTakeoverRecord), so this write
// is an error, which the claim route answers as a record that did not store, and the other claim's
// record stays the subject's newest. A write that counted every refused resend wrapping
// bus.ErrUnacknowledged as stored would answer nil here.
func TestATakeoverRecordResendThatMeetsAnotherClaimOnItsSubjectIsNoWrite(t *testing.T) {
	uri := testnats.URL(t)
	other, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(other.Close)
	proxy := testnats.StartProxy(t, uri)
	claimant, err := bus.ConnectOwningStream([]string{proxy.URL()}, bus.WithReplicas(1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(claimant.Close)
	const role, previous = "resend-another-claim", "ses_previous"
	subject := bus.TakeoverSubject(role, previous, id.New())
	claimedAt := time.Now().UnixMilli()
	theirs := bus.TakeoverRecord{Role: role, Previous: previous, ClaimedAt: claimedAt}
	mine := bus.TakeoverRecord{Role: role, Previous: previous, ClaimedAt: claimedAt + 1}
	ctx := testwait.Context(t)
	if _, err := other.WriteTakeoverRecordAt(ctx, bus.StoredTakeoverRecord{Subject: subject}, theirs); err != nil {
		t.Fatalf("store the other claim's takeover record: %v", err)
	}
	// A plain subscriber on the subject receives every send the server takes there, stored or
	// refused, so the row knows the claimant's first send reached the server before it cuts the
	// claimant's connection.
	sent := make(chan *nats.Msg, 4)
	if _, err := other.Connection().ChanSubscribe(subject, sent); err != nil {
		t.Fatalf("watch %s: %v", subject, err)
	}
	if err := other.Connection().Flush(); err != nil {
		t.Fatalf("watch %s: %v", subject, err)
	}
	proxy.FreezeServerReplies()
	answered := make(chan error, 1)
	go func() { answered <- claimant.WriteTakeoverRecord(ctx, subject, mine) }()
	testwait.Receive(t, "the claimant's first send to reach the server", sent)
	if err := claimant.Connection().ForceReconnect(); err != nil {
		t.Fatal(err)
	}
	proxy.Restore()
	got := testwait.Receive(t, "the claimant's write to answer after reconnect", answered)
	if got == nil {
		t.Fatalf("a resend refused over another claim's record on %s answered nil, want an error: the subject does not hold this claim", subject)
	}
	if !errors.Is(got, bus.ErrUnacknowledged) {
		t.Fatalf("a resend refused after a send nothing answered = %v, want it to wrap bus.ErrUnacknowledged", got)
	}
	last, err := other.JS().GetLastMsg(bus.Stream, subject)
	if err != nil {
		t.Fatal(err)
	}
	var record bus.TakeoverRecord
	if err := json.Unmarshal(last.Data, &record); err != nil {
		t.Fatal(err)
	}
	if record != theirs {
		t.Fatalf("the newest record on %s is %+v at sequence %d, want the other claim's %+v", subject, record, last.Sequence, theirs)
	}
}
