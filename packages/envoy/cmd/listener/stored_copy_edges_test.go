// Two edges of the stored-copy answer: a read that does not answer within the request's budget,
// and a copy the stream deleted whose id it later forgets.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/testwait"
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
	t.Parallel()
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
		duplicate, err := publishHeld(context.Background(), reader, duplicateVerdict, &requestRecord{}, sender, &repeat)
		if err != nil || !duplicate || repeat.EventID != copied.EventID {
			t.Fatalf("a read that answers answered duplicate %t, event %s (%v); want duplicate and the copy's %s", duplicate, repeat.EventID, err, copied.EventID)
		}
	})

	t.Run("case: a read that does not answer within the budget is the stored-copy 503", func(t *testing.T) {
		repeat := item
		reader := &standInReader{}
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		_, err := publishHeld(ctx, reader, duplicateVerdict, &requestRecord{}, sender, &repeat)
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
// own envelope with no duplicate. The 503 row runs under the stream's own duplicate window, so the
// stream remembers the id however long the publish, the read and the delete take. Only then is
// the window cut to seconds, standing in for the 72 hours of the deployed one, so the stream
// forgets the id.
//
// The cut is two stream updates, which cutTheDuplicateWindow below carries with the nats-server
// defect they work around. The listener's keeper puts the deployed 72 hours back every 30 s from
// its start (bus.defaultKeepInterval), and a pass between the cut and the forget keeps the id
// under 72 hours again, so the row holds the keeper off.
func TestARepeatAfterTheStreamForgetsTheIdStoresANewCopy(t *testing.T) {
	t.Parallel()
	rig, _ := newSenderLimitRigOnServer(t, bus.WithKeepInterval(backgroundReadsHeld))
	js := rig.harness.client.JS()
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

	info, err := js.StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	if err := cutTheDuplicateWindow(js, info.Config, forgottenIdWindow); err != nil {
		t.Fatal(err)
	}
	fresh := awaitARepeatStoredOnceForgotten(t, rig, sender, subject, key, first)
	fresh.requireHeld(t, "the repeat after the stream forgot the id", heldCopy(t, rig.harness.client, subject), false)
}

// forgottenIdWindow is the duplicate window the forgotten-id rows cut the stream's to: seconds,
// standing in for the 72 hours of the deployed one.
const forgottenIdWindow = 3 * time.Second

// awaitARepeatStoredOnceForgotten waits for a repeat of first, the publish from sender on subject
// under key, to be stored as a new copy once the stream forgets the id: a 200 that is no
// duplicate, with an envelope of its own. It returns that answer. The forgotten-id rows share it,
// so the bound and the reading they hold the cut to are one.
func awaitARepeatStoredOnceForgotten(t *testing.T, rig *senderLimitRig, sender, subject string, key map[string]any, first heldAnswer) heldAnswer {
	t.Helper()
	// The wait is for a product time, the stream's duplicate window, and each caller has just cut
	// it, so its failure says how long it has been since the cut, the window cut to and the window
	// the stream reports now. A repeat not yet stored as a new copy, refused or answered as a
	// duplicate, after many times the window is either a window that is not the one the row set or
	// a server that has not pruned its duplicate table, and the two read differently here.
	var fresh heldAnswer
	cutAt := time.Now()
	if !testwait.Holds(listenerWaitBound(t), testwait.Poll, func() bool {
		fresh = held(t, rig.publishFrom(t, sender, subject, key))
		return fresh.status == http.StatusOK && !fresh.Duplicate
	}) {
		window := "unreadable"
		if latest, infoErr := rig.harness.client.JS().StreamInfo(bus.Stream); infoErr == nil {
			window = latest.Config.Duplicates.String()
		} else {
			window = "unreadable: " + infoErr.Error()
		}
		t.Fatalf("a repeat was still not stored as a new copy %s after the duplicate window was cut to %s (the stream now reports %s): the last answer was %d, duplicate %t, %s; want a 200 that is no duplicate once the stream forgets the id",
			time.Since(cutAt).Round(time.Millisecond), forgottenIdWindow, window, fresh.status, fresh.Duplicate, fresh.raw)
	}
	if fresh.envelope.EventID == first.envelope.EventID {
		t.Fatalf("the repeat after the stream forgot the id answered the first copy's event %s, want its own", fresh.envelope.EventID)
	}
	return fresh
}

// cutTheDuplicateWindow cuts the stream config's stream to the duplicate window window, in two
// stream updates: one to a second more than window, then one to window itself. config is the
// stream's own, as StreamInfo answered it, and is left as it was.
//
// Two updates work around a defect of nats-server v2.15.1 and v2.15.3, not a choice of style. A
// stream update whose duplicate window changes re-arms the stream's purge of remembered ids to
// fire in a microsecond (server/stream.go:2795-2800) before it stores the new window
// (server/stream.go:2979-2981). It holds the dedupe lock during each of those steps and drops it
// between them; across both it holds the stream's own lock, which the purge
// (server/stream.go:5628) never takes, so a purge can run between the two and read the window
// being replaced. After a cut from 72 hours it keeps the id and re-arms itself for about 72
// hours, and the duplicate check is a lookup of the id with no test of its age
// (server/stream.go:5619), so the stream never forgets the id and every repeat answers the
// stored-copy 503. The second update re-arms the purge once the first one's window is stored, so
// whichever window that purge reads, the old or the new, is seconds long.
//
// This cut can go back to one update when packages/envoy/go.mod moves to a nats-server release
// that stores the new window before it re-arms the purge. The proof to run then is the
// forced-order row TestForcedOrderARepeatIsStoredThoughThePurgeRanAtTheCut
// (forced_order_dedupe_window_test.go), ported to that release first: its hold point is this
// defect's own order, so on a release without it testnats.DedupeReset is gone (hooks.go's
// missing), and the row must then hold wherever that release lets a purge run during an update
// and await the window that purge reads, which is the new one. With the row ported, cut this
// helper to one update: it must pass, where on v2.15.1 it fails 5 of 5.
func cutTheDuplicateWindow(js natsgo.JetStreamContext, config natsgo.StreamConfig, window time.Duration) error {
	for _, step := range []time.Duration{window + time.Second, window} {
		config.Duplicates = step
		if _, err := js.UpdateStream(&config); err != nil {
			return fmt.Errorf("cut the stream's duplicate window to %s: %w", step, err)
		}
	}
	return nil
}
