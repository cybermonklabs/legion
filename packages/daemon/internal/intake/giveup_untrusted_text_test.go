package intake

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store/storetext"
)

// A subject is the publisher's text, which Envoy and NATS refuse only whitespace in, and so is an
// envelope's event id. A NUL or a byte that is no UTF-8 in either never keeps a message from its
// record: a message that does not decode is recorded and given up at its first delivery, as the
// daemon before the record terminated it then, and a decoded message whose handling always fails
// is recorded at its first failure and given up at the bound. The record keeps the subject's bytes
// exactly as sent, so a replay decodes the subject the delivery decoded, and the event id as the
// store keeps text; the controller's notice names the subject as the store keeps text, since its
// jsonb row refuses the escape a NUL becomes.
func TestAMessageWhoseSubjectOrEventIDIsNoStorableTextIsRecordedAndGivenUp(t *testing.T) {
	base := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	consumer := dispatchConsumerName("CAPTURE")
	captured := capturedIssueUpdatedEnvelope(t)
	for i, row := range []struct {
		name    string
		subject string
		// eventID is the envelope's event id of a decoded message, whose handler always fails;
		// empty for a message that does not decode.
		eventID string
	}{
		{name: "a message that does not decode, with a NUL in its subject",
			subject: "notifications.dispatch.issue.CAPTURE-3.issue.updated\x00"},
		{name: "a message that does not decode, with a byte that is no UTF-8 in its subject",
			subject: "notifications.dispatch.issue.CAPTURE-3.issue.\xffupdated"},
		{name: "a decoded message whose handling fails, with a NUL in its event id",
			subject: "notifications.dispatch.issue.CAPTURE-3.issue.updated", eventID: "dispatch-\x00-untrusted"},
		{name: "a decoded message whose handling fails, with a byte that is no UTF-8 in its subject",
			subject: "notifications.dispatch.issue.CAPTURE-3.issue.\xffupdated", eventID: "dispatch-untrusted-subject"},
	} {
		t.Run(row.name, func(t *testing.T) {
			pool := migratedPool(t)
			registerCaptureController(t, pool)
			now := base
			run := productionRun(t, pool, &lockedBuffer{}, func() time.Time { return now })
			seq := uint64(900 + i)
			data := []byte(`{`)
			if row.eventID != "" {
				data = envelopeWithID(t, captured, row.eventID)
			}
			first := newDelivery(t, consumer, row.subject, data, seq, base.Add(-time.Minute))
			calls := 0
			consumeDelivery(context.Background(), first, run, failingHandler(&calls))

			delivery := first
			if row.eventID != "" {
				open, found := intakeRecordOf(t, pool, consumer, seq)
				if !found || open.GivenUp() || open.EndedDelivery != 1 || open.Subject != row.subject || open.EventID != storetext.Text(row.eventID) {
					t.Fatalf("the record after the first failure = %+v (found %t), want an open failure of subject %q and event id %q",
						open, found, row.subject, storetext.Text(row.eventID))
				}
				now = base.Add(time.Hour)
				delivery = first.deliveredAgain(21)
				consumeDelivery(context.Background(), delivery, run, failingHandler(&calls))
			}
			if acks, terms, naks := delivery.asked(); acks != 0 || terms != 1 || len(naks) != 0 {
				t.Fatalf("the deciding delivery asked %d acks, %d terms and naks %v of JetStream, want the message terminated", acks, terms, naks)
			}
			given, found := intakeRecordOf(t, pool, consumer, seq)
			want := record.GiveUpUndecodable
			if row.eventID != "" {
				want = record.GiveUpBound
			}
			if !found || given.GiveUpReason != want || given.Subject != row.subject || given.EventID != storetext.Text(row.eventID) {
				t.Fatalf("the record = %+v (found %t), want it given up for %q with the subject %q byte for byte and event id %q",
					given, found, want, row.subject, storetext.Text(row.eventID))
			}
			var notices int
			var named string
			if err := pool.QueryRow(context.Background(), `select count(*), coalesce(max(payload->'giveUps'->0->>'subject'), '') from outbox
				where kind = $1 and payload->>'kind' = $2`, string(record.OutboxKindControllerNotice), string(record.IntakeGaveUpNotice)).
				Scan(&notices, &named); err != nil {
				t.Fatalf("read the intake-gave-up notice: %v", err)
			}
			if notices != 1 || named != storetext.Text(row.subject) {
				t.Fatalf("the outbox holds %d intake-gave-up notices naming %q, want one naming %q", notices, named, storetext.Text(row.subject))
			}
		})
	}
}
