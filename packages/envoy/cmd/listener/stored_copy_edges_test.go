// Two edges of the stored-copy answer: a read that does not answer within the request's budget,
// and a copy the stream deleted whose id it later forgets.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/logging"
)

// standInReader stands in for the stream's read of a stored message: it answers stored, or, when
// stored is nil, answers nothing until the read's ctx ends, as a read the server never answers.
type standInReader struct {
	stored *bus.StoredMessage
	reads  int
}

func (r *standInReader) NextStoredMessage(ctx context.Context, _ string, _ uint64) (bus.StoredMessage, error) {
	r.reads++
	if r.stored != nil {
		return *r.stored, nil
	}
	<-ctx.Done()
	return bus.StoredMessage{}, ctx.Err()
}

// A repeat whose read of the stored copy does not answer within the request's budget is the
// stored-copy 503: Retry-After 1, no envelope, the read's own reason after the prefix, recorded
// stored_copy_unreadable, and the request's envelope untouched. The stream's duplicate verdict is
// stood in for, and so is the read, which answers nothing until the request's budget ends. The
// control is the same verdict with a read that answers the copy at once, which is the answer.
func TestARepeatWhoseCopyReadDoesNotAnswerAnswers503(t *testing.T) {
	const seq = 7
	item := contracts.MessageEnvelope(contracts.Message{SourceSession: "ses_slow_read", Text: "read slowly"},
		"notifications.team.slow-read", publishDedupePrefix+uuid.NewString())
	sender := requestSender{source: item.Source, session: item.SourceSession}
	duplicateVerdict := func(context.Context, contracts.Envelope) (bus.StoredAck, error) {
		return bus.StoredAck{Sequence: seq, Duplicate: true}, nil
	}

	t.Run("control: a read that answers the copy at once is the answer", func(t *testing.T) {
		copied := item
		copied.EventID = id.New()
		data, err := json.Marshal(copied)
		if err != nil {
			t.Fatal(err)
		}
		repeat := item
		reader := &standInReader{stored: &bus.StoredMessage{Sequence: seq, Subject: item.Topic, Data: data}}
		duplicate, err := publishHeld(context.Background(), reader, duplicateVerdict, nil, sender, &repeat)
		if err != nil || !duplicate || repeat.EventID != copied.EventID {
			t.Fatalf("a read that answers answered duplicate %t, event %s (%v); want duplicate and the copy's %s", duplicate, repeat.EventID, err, copied.EventID)
		}
	})

	t.Run("case: a read that does not answer within the budget is the stored-copy 503", func(t *testing.T) {
		repeat := item
		reader := &standInReader{}
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		_, err := publishHeld(ctx, reader, duplicateVerdict, nil, sender, &repeat)
		if reader.reads != 1 || !errors.Is(err, errStoredCopyUnread) || repeat.EventID != item.EventID {
			t.Fatalf("a read that did not answer gave %d reads, error %v, event %s; want one read, errStoredCopyUnread and the request's own envelope untouched",
				reader.reads, err, repeat.EventID)
		}
		var logs strings.Builder
		record := &requestRecord{logger: logging.NewWithWriter("test", &logs), line: "listener publish", laneKey: "topic", item: &repeat}
		recorder := httptest.NewRecorder()
		record.refuseBusUnavailable(recorder, err)
		body := recorder.Body.String()
		if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") != "1" {
			t.Fatalf("a read that did not answer was answered %d with Retry-After %q, want 503 with 1; body %s", recorder.Code, recorder.Header().Get("Retry-After"), body)
		}
		if !strings.Contains(body, storedCopyUnreadablePrefix+": read sequence 7 of notifications.team.slow-read: context deadline exceeded") || strings.Contains(body, `"event_id"`) {
			t.Fatalf("a read that did not answer was answered %s, want the stored-copy error naming the read's deadline once, and no envelope", body)
		}
		lines := linesIn(logs.String(), "listener publish", nil)
		if len(lines) != 1 || lines[0]["outcome"] != string(outcomeStoredCopyUnreadable) {
			t.Fatalf("a read that did not answer wrote %v, want one line recorded %s", lines, outcomeStoredCopyUnreadable)
		}
	})
}

// A copy the stream deleted while it still remembers the copy's id keeps a repeat's answer the
// stored-copy 503, and once the stream forgets the id, a repeat stores a new copy and answers its
// own envelope with no duplicate. The listener's own server, whose stream's duplicate window is cut
// to three seconds for the test, stands in for the 72 hours of the deployed one.
func TestARepeatAfterTheStreamForgetsTheIdStoresANewCopy(t *testing.T) {
	rig, _ := newSenderLimitRigOnServer(t)
	js := rig.harness.client.JS()
	info, err := js.StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	config := info.Config
	config.Duplicates = 3 * time.Second
	if _, err := js.UpdateStream(&config); err != nil {
		t.Fatalf("cut the stream's duplicate window: %v", err)
	}
	const sender = "ses_forgotten_id"
	subject := "notifications.team.forgotten-" + id.New()
	key := map[string]any{"idempotency_key": uuid.NewString()}

	first := held(t, rig.publishFrom(t, sender, subject, key))
	first.requireStatus(t, "the first publish", http.StatusOK)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stored, err := readStored(ctx, rig.harness.client, subject)
	if err != nil || len(stored) != 1 {
		t.Fatalf("the stream holds %d copies on %s (%v), want the first publish's one", len(stored), subject, err)
	}
	if err := js.DeleteMsg(bus.Stream, stored[0].Sequence); err != nil {
		t.Fatalf("delete the first copy: %v", err)
	}

	remembered := rig.publishFrom(t, sender, subject, key)
	remembered.requireStatus(t, "the repeat while the stream remembers the id", http.StatusServiceUnavailable)
	remembered.requireOutcome(t, "the repeat while the stream remembers the id", "stored_copy_unreadable")

	var fresh heldAnswer
	waitFor(t, 30*time.Second, "a repeat to be stored once the stream forgets the id", func() bool {
		fresh = held(t, rig.publishFrom(t, sender, subject, key))
		return fresh.status == http.StatusOK
	})
	if fresh.envelope.EventID == first.envelope.EventID {
		t.Fatalf("the repeat after the stream forgot the id answered the first copy's event %s, want its own", fresh.envelope.EventID)
	}
	fresh.requireHeld(t, "the repeat after the stream forgot the id", heldCopy(t, rig.harness.client, subject), false)
}
