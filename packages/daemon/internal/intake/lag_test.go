package intake

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// storedMessage is a delivery JetStream stored at stored, with pending more behind it, seen by a lag
// watch: it answers its metadata and its subject, and every other call fails the row by the call's
// name (unexpectedMessage), since watching a consumer's lag reads a delivery and ends none.
type storedMessage struct {
	unexpectedMessage
	stored  time.Time
	pending uint64
}

var _ jetstream.Msg = storedMessage{}

func (m storedMessage) Metadata() (*jetstream.MsgMetadata, error) {
	return &jetstream.MsgMetadata{Timestamp: m.stored, NumPending: m.pending, NumDelivered: 1, Sequence: jetstream.SequencePair{Stream: 1013506}}, nil
}

func (storedMessage) Subject() string { return "notifications.github.acme.widgets.push.branch.main" }

// observeStored has watch observe m with its metadata, as consumeConsumer parses it once per
// delivery and hands it on.
func observeStored(watch *lagWatch, m storedMessage) {
	metadata, _ := m.Metadata()
	watch.observe(m, metadata)
}

// A durable consumer hours behind its stream held phase moves and pull request records back with
// nothing in the log to say so. lagWatch says so, with how far behind and how many messages wait,
// for a message stored more than five minutes before it is handled, at most once a minute however
// many such messages a backlog delivers; a consumer keeping up says nothing.
func TestLagWatchSaysOnceAMinuteThatAConsumerIsBehind(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 26, 20, 54, 24, 0, time.UTC)
	var logs bytes.Buffer
	watch := &lagWatch{consumer: "legion-capture-github", logger: slog.New(slog.NewTextHandler(&logs, nil)), now: func() time.Time { return now }}
	lines := func() int { return strings.Count(logs.String(), "a durable consumer is behind its stream") }
	stored := func(at time.Time, pending uint64) storedMessage {
		return storedMessage{unexpectedMessage: unexpectedMessage{t: t}, stored: at, pending: pending}
	}

	observeStored(watch, stored(now.Add(-time.Minute), 3))
	if got := lines(); got != 0 {
		t.Fatalf("a consumer a minute behind logged %d lines, want none:\n%s", got, logs.String())
	}
	observeStored(watch, stored(now.Add(-2*time.Hour), 4102))
	if got := lines(); got != 1 {
		t.Fatalf("a consumer two hours behind logged %d lines, want one:\n%s", got, logs.String())
	}
	for _, want := range []string{"consumer=legion-capture-github", "behindSeconds=7200", "pending=4102", "delivered=1", "stream_seq=1013506"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("the line does not say %s:\n%s", want, logs.String())
		}
	}
	now = now.Add(30 * time.Second)
	observeStored(watch, stored(now.Add(-2*time.Hour), 4101))
	if got := lines(); got != 1 {
		t.Fatalf("a second message within the minute logged %d lines in all, want still one", got)
	}
	now = now.Add(30 * time.Second)
	observeStored(watch, stored(now.Add(-2*time.Hour), 4000))
	if got := lines(); got != 2 {
		t.Fatalf("a message a minute after the first line logged %d lines in all, want two", got)
	}
}
