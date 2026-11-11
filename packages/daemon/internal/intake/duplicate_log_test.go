package intake

import (
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
	// and cuts nothing the row does. That guard is what makes the count of one sound: a
	// redelivery the count would see needs the whole minute to pass with a handler still
	// running. The progress keeper makes a redelivery unlikely while a handler runs and promises
	// none: a report, or the acknowledgement, that reaches the server after the window has
	// lapsed still has the message delivered again (keepInProgress, ackprogress.go). Measured in
	// this issue's pod at the base commit, the held-fact row of this package failed 11 of 300
	// runs at the 200 ms window under 16 busy loops on 2 CPUs, and the keeper's first report was
	// up to 403.7 ms late against a 66.7 ms period.
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
	waitAcknowledged(t, stream, consumer, publishAt(t, js, reviewSubject, review))
	waitAcknowledged(t, stream, consumer, publishAt(t, js, reviewSubject, review))

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
	// The seven fields an operator tells a redelivery from a copy of the listener's mention
	// fan-out by, and reads the rate per consumer and source from. What each one holds is the
	// delivery's, so the row asks that it is reported and not what it reads.
	for _, field := range []string{"consumer=", "source=", "event_id=", "subject=", "stream_seq=", "delivery_seq=", "delivered="} {
		if !strings.Contains(line, field) {
			t.Errorf("the dropped duplicate's line carries no %s, which is what the rate is read by: %s", field, line)
		}
	}
}
