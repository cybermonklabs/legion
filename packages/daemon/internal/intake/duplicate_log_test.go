package intake

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// droppedDuplicateLine is the line logDuplicate writes for a fact ApplyFact answers
// Result{Duplicate: true} for, which the rows below are here for.
const droppedDuplicateLine = "an intake fact was delivered again and dropped as already applied"

// Nothing else in the daemon reports a fact delivered a second time: a duplicate is dropped before
// any handler runs (ApplyFact, facts.go) and acknowledged like an applied one, so this line is the
// only place the rate of it can be read from (dispatch://INFRA-1113). A change that dropped the
// line, or that left one of its fields behind, would take that rate away with nothing else
// failing; hence a row of its own. A replay of a give-up whose fact was applied already writes the
// same line, naming the record it replays, so a reader tells it from a redelivery.
//
// The rows prove the line and nothing else. That a delivery arriving again is applied once by its
// dedupe key is TestARedeliveredGitHubDeliveryIsAppliedOnceByItsDedupeKey's claim (dedupe_test.go),
// so nothing here reads processed_events or counts a handler's writes.
func TestTheIntakeSaysSoWhenItDropsAFactDeliveredAgain(t *testing.T) {
	t.Parallel()
	const reviewSubject = "notifications.github.sjawhar.legion.pr.42.review"
	t.Run("a delivery of a fact applied already", func(t *testing.T) {
		pool := migratedPool(t)
		js, stream := testJetStream(t)
		logs := &lockedBuffer{}
		spec := consumerSpec(logs)
		// A hang guard, not the subject: the row counts the lines one dropped duplicate writes, and a
		// delivery either message got again would write one more. AckWait bounds each read this
		// handling makes, which here is the one read of whether the pull request is recorded
		// (recordedPullRequest), since the row sets no ReviewPermission resolver, and any fixed
		// window lets the machine decide whether that read is cut: consumerSpec's 200 ms is about the
		// length of that work on a loaded runner, and a minute is cut by a read of 61 s. So the
		// window, and the row's wait for each acknowledgement (waitAcknowledged), are what is left of
		// the test binary's deadline less 30 s (testwait.Remaining): a read however slow is
		// waited out, and only one that never answers fails the row, by name, before the binary's own
		// timeout. The window is also what makes the count of one sound: a redelivery the count would
		// see needs the whole window to pass before the handling's acknowledgement reaches the server.
		// The campaign of dispatch://INFRA-1113 measured that trip at 516.8 ms at its largest, under
		// 16 busy loops on 2 CPUs, and the handler's own work and that trip together, about 717 ms, are
		// far below this window. The progress keeper reports nothing here: it ticks at a third of the
		// window (spec.AckWait/3 in consume.go), and each handling of this row acknowledges long
		// before the first tick is due (keepInProgress, ackprogress.go).
		spec.AckWait = testwait.Remaining(t)
		stop := startConsume(t, js, spec, pool)
		defer stop()
		recordPullRequest(t, pool, "LEGION-208", 42)

		// The same envelope twice, each publication a stream message of its own: nothing sets
		// Nats-Msg-Id, so the stream's own duplicate window collapses neither, and both carry the
		// dedupe key the decoder takes the fact's id from (factID, decode.go). The first is applied,
		// the second dropped as already applied. The first is waited for before the second is
		// published, so the delivery the line is asked about is the second.
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
		// The consumer the rate is read per, by value: a line carrying consumer="" is a delivery
		// whose metadata did not parse (consume.go), and reads as a drop on no consumer at all.
		if said := logValue(line, "consumer"); said != consumer {
			t.Errorf("the dropped duplicate's line says consumer=%s, want %s, the GitHub durable whose share of the drops is read from it: %s", said, consumer, line)
		}
		// One delivery, by value: each publication is a stream message of its own, so the dropped
		// copy is that message's own first delivery, and a count above one would be a JetStream
		// redelivery. This 1 rests on the same margin the count of one line above rests on.
		// A delivered=0 is metadata that did not parse, and no reading of a drop.
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
		if strings.Contains(line, " replay=") {
			t.Errorf("the dropped delivery's line names a replay: %s", line)
		}
	})
	// The same review given up twice, as two stream messages: a replay of the first applies it, and
	// a replay of the second drops it. Its line names the record's consumer, stream sequence and
	// delivery count, and the record it replays; a replay has no delivery, so no delivery sequence.
	t.Run("a replay of a fact applied already", func(t *testing.T) {
		pool := migratedPool(t)
		logs := &lockedBuffer{}
		recordPullRequest(t, pool, "LEGION-208", 42)
		review := capturedGitHubEnvelope(t, "review.json")
		consumer := githubConsumerName("CAPTURE")
		applied := storedGiveUp(t, pool, consumer, reviewSubject, review, 7, 21)
		dropped := storedGiveUp(t, pool, consumer, reviewSubject, review, 8, 23)
		giveUps := giveUpsOf(t, pool, replaySpec(t, logs))
		for _, given := range []record.IntakeFailure{applied, dropped} {
			if _, err := giveUps.Replay(context.Background(), record.IntakeSelector{ID: given.ID}, "operator"); err != nil {
				t.Fatalf("replay give-up %d: %v", given.ID, err)
			}
		}

		written := logs.String()
		if said := strings.Count(written, droppedDuplicateLine); said != 1 {
			t.Fatalf("%d lines say a fact was dropped as already applied, want one, the second replay's, in:\n%s", said, written)
		}
		line := logLine(written, droppedDuplicateLine)
		if !strings.Contains(line, "level=INFO") {
			t.Errorf("the dropped replay's line is not at info, the level the rate is read at: %s", line)
		}
		for key, want := range map[string]string{"consumer": consumer, "delivered": "23", "stream_seq": "8", "replay": strconv.FormatInt(dropped.ID, 10)} {
			if said := logValue(line, key); said != want {
				t.Errorf("the dropped replay's line says %s=%s, want %s: %s", key, said, want, line)
			}
		}
		for _, field := range []string{"source=", "event_id=", "subject="} {
			if !strings.Contains(line, field) {
				t.Errorf("the dropped replay's line carries no %s: %s", field, line)
			}
		}
		if strings.Contains(line, "delivery_seq=") {
			t.Errorf("the dropped replay's line names a delivery sequence, and a replay has no delivery: %s", line)
		}
	})
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
