package intake

import (
	"bytes"
	"strings"
	"testing"
)

// A message that does not decode is recorded at its first delivery, given up with the decode
// error, its payload kept, and no event id, since it named none the daemon could read; then it is
// terminated, so the consumer's floor moves past it and nothing waits for it.
func TestAMessageThatDoesNotDecodeIsRecordedAtItsFirstDelivery(t *testing.T) {
	pool := migratedPool(t)
	js, stream := testJetStream(t)
	spec := consumerSpec(&lockedBuffer{})
	stop := startConsume(t, js, spec, pool)
	defer stop()

	sent := publish(t, js, "notifications.dispatch.issue.CAPTURE-208.issue.updated", []byte(`{`))
	dispatch := dispatchConsumerName(spec.Project)
	given := waitGivenUp(t, pool, dispatch, sent.Sequence)
	if given.GiveUpReason != "undecodable" || given.Deliveries != 1 || given.EventID != "" || !bytes.Equal(given.Payload, []byte(`{`)) ||
		!strings.Contains(given.LastError, "decode Envoy envelope") {
		t.Fatalf("the record = %+v, want the payload given up as undecodable at its first delivery with the decode error and no event id", given)
	}
	assertAcknowledgedThrough(t, stream, dispatch, sent)
	if records := intakeRecords(t, pool); len(records) != 1 {
		t.Fatalf("intake_failures holds %d rows, want the one give-up", len(records))
	}
}
