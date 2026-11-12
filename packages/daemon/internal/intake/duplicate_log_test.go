package intake

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// droppedDuplicateLine is the line handleMessage writes for a fact ApplyFact answers
// Result{Duplicate: true} for, which the row below is here for.
const droppedDuplicateLine = "an intake fact was delivered again and dropped as already applied"

// Nothing else in the daemon reports a fact delivered a second time: a duplicate is dropped before
// any handler runs (ApplyFact, facts.go) and acknowledged like an applied one, so this line is the
// only place the rate of it can be read from (dispatch://INFRA-1113). A change that dropped the
// line, or that left one of its fields behind, would take that rate away with nothing else
// failing; hence a row of its own.
//
// The row proves the line and nothing else. That a delivery arriving again is applied once by its
// dedupe key is TestARedeliveredGitHubDeliveryIsAppliedOnceByItsDedupeKey's claim (dedupe_test.go),
// so nothing here reads processed_events or counts a handler's writes.
func TestTheIntakeSaysSoWhenItDropsAFactDeliveredAgain(t *testing.T) {
	pool := migratedPool(t)
	js, stream := testJetStream(t)
	logs := &lockedBuffer{}
	spec := consumerSpec(logs)
	// A hang guard, not the subject: the row counts the lines one dropped duplicate writes, and a
	// delivery either message got again would write one more. AckWait bounds each read this
	// handling makes, which here is the one read of whether the pull request is recorded
	// (recordedPullRequest), since the row sets no ReviewPermission resolver, and consumerSpec's
	// 200 ms is about the length of that work on a loaded runner, so a minute is far above it
	// and cuts nothing the row does. The margin is what makes the count of one sound: a
	// redelivery the count would see needs the whole minute to pass before the handling's
	// acknowledgement reaches the server. The campaign of dispatch://INFRA-1113 measured that
	// trip at 516.8 ms at its largest, under 16 busy loops on 2 CPUs, and the handler's own work
	// and that trip together, about 717 ms, are a factor of about 80 below this window.
	// The progress keeper reports nothing here: it ticks at a third of the window
	// (spec.AckWait/3 in consume.go), so at 20 s, and each handling of this row acknowledges
	// long before the first tick is due (keepInProgress, ackprogress.go).
	spec.AckWait = time.Minute
	stop := startConsume(t, js, spec, pool)
	defer stop()
	recordPullRequest(t, pool, "LEGION-208", 42)

	// The same envelope twice, each publication a stream message of its own: nothing sets
	// Nats-Msg-Id, so the stream's own duplicate window collapses neither, and both carry the
	// dedupe key the decoder takes the fact's id from (factID, decode.go). The first is applied,
	// the second dropped as already applied. The first is waited for before the second is
	// published, so the delivery the line is asked about is the second.
	const reviewSubject = "notifications.github.sjawhar.legion.pr.42.review"
	review := capturedGitHubEnvelope(t, "review.json")
	consumer := githubConsumerName(spec.Project)
	first := publishAt(t, js, reviewSubject, review)
	waitAcknowledged(t, stream, consumer, first)
	second := publishAt(t, js, reviewSubject, review)
	waitAcknowledged(t, stream, consumer, second)

	written := logs.String()
	line := logLine(written, droppedDuplicateLine)
	if line == "" {
		t.Fatalf("no line says a fact was delivered again and dropped as already applied, and the review envelope was published twice on %s, the second of them dropped, in:\n%s",
			reviewSubject, written)
	}
	// One line, because the applied delivery wrote none: both publications were handled, and only
	// the duplicate is reported.
	if said := strings.Count(written, droppedDuplicateLine); said != 1 {
		t.Errorf("%d lines say a fact was delivered again and dropped, want one, for the second publication alone: the delivery the daemon applied writes none, in:\n%s", said, written)
	}
	if !strings.Contains(line, "level=INFO") {
		t.Errorf("the dropped duplicate's line is not at info, the level the rate is read at: %s", line)
	}
	// The consumer the rate is read per, by value: a line carrying consumer="" is the metadata
	// read of the duplicate branch failing (consume.go), and reads as a drop on no consumer at
	// all.
	if said := logValue(line, "consumer"); said != consumer {
		t.Errorf("the dropped duplicate's line says consumer=%s, want %s, the GitHub durable whose share of the drops is read from it: %s", said, consumer, line)
	}
	// One delivery, by value: each publication is a stream message of its own, so the dropped
	// copy is that message's own first delivery, and a count above one would be a JetStream
	// redelivery. This 1 rests on the same margin the count of one line above rests on.
	// A delivered=0 is the metadata read failing, and no reading of a drop.
	if said := logValue(line, "delivered"); said != "1" {
		t.Errorf("the dropped duplicate's line says delivered=%s, want 1, the dropped copy's own first delivery: %s", said, line)
	}
	// The second publication's own stream sequence, by value: this is the one field that holds
	// the branch's polarity. Both publications are the same envelope on the same subject, so
	// both copies carry the same consumer, source, event id and subject, and each publication is
	// a stream message of its own delivered once, so both read delivered=1. Read by key alone,
	// every field of this line says as much about the applied fact as about the dropped copy,
	// and the row passes whether the branch reports the duplicate or its opposite. The stream
	// sequence is what separates them, so it is read by value.
	if said := logValue(line, "stream_seq"); said != strconv.FormatUint(second, 10) {
		t.Errorf("the dropped duplicate's line says stream_seq=%s, want %d, the second publication's own message: %s", said, second, line)
	}
	// The other four fields an operator tells a redelivery from a copy of the listener's mention
	// fan-out by. What each one holds is the fixture's — its subject, its ids and the stream's
	// numbering — so the row asks that it is reported and not what it reads.
	for _, field := range []string{"source=", "event_id=", "subject=", "delivery_seq="} {
		if !strings.Contains(line, field) {
			t.Errorf("the dropped duplicate's line carries no %s, which is what the rate is read by: %s", field, line)
		}
	}
}

// logValue is what a slog text line holds for key: what follows " key=" up to the next space, or
// "" when the line carries no such key. Call it only for a value slog writes unquoted: slog quotes
// a value that holds a space, and this reads such a value only as far as that space.
func logValue(line, key string) string {
	_, after, found := strings.Cut(line, " "+key+"=")
	if !found {
		return ""
	}
	value, _, _ := strings.Cut(after, " ")
	return value
}
