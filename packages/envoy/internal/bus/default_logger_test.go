package bus

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/logtest"
)

// A client built without WithLogger wrote each line through slog.Default() as the line was
// written, before the client took a logger, so a program that set the default after it built a
// client found that client's lines where it set them. The logger such a client takes still does:
// the row builds the options first, sets the default after, and reads the line, attributes and
// group included, from the default set last. Serial: it sets the process's default logger.
func TestAClientBuiltWithoutALoggerWritesWhereTheDefaultPointsAtEachLine(t *testing.T) {
	opts := parseConnectOptions([]ConnectOption{})
	var logs logtest.Buffer
	logtest.SetDefault(t, slog.NewJSONHandler(&logs, nil))

	opts.logger.With("client", "the row's").WithGroup("nats").Info("envoy nats connected", "url", "nats://127.0.0.1:4222")
	got := logs.String()
	for _, want := range []string{`"msg":"envoy nats connected"`, `"client":"the row's"`, `"nats":{"url":"nats://127.0.0.1:4222"}`} {
		if !strings.Contains(got, want) {
			t.Fatalf("the default set after the options were built got %q, want it to hold %s", got, want)
		}
	}
	if loggerOwning(nil) != followsDefault {
		t.Fatal("a bucket call on a connection no client owns does not write where the default points at each line")
	}
}

// The default can change between a logger's Enabled and its handler's Handle. followsDefault's
// Handle checks the record against the handler it hands it to, so a record below the level of the
// default in force when it is written is dropped, as slog.Default() would have dropped it. The row
// sets a WARN-level default and hands an INFO record straight to the handler, the call Enabled
// would have let through under an earlier default. Serial: it sets the process's default logger.
// Serial: it swaps the process default, which is the thing under test. It stays because it
// catches a real regression and the other rows cannot: a handler that forwards to slog.Default()
// but reads Enabled on itself, or on the default it was built over, takes a record the current
// default would drop. followsDefault reads the target handler once per record and asks that
// handler, and this row fails the moment it stops doing so.
func TestTheDefaultFollowingHandlerDropsARecordTheCurrentDefaultDoesNotTake(t *testing.T) {
	var logs logtest.Buffer
	logtest.SetDefault(t, slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))

	record := slog.NewRecord(time.Now(), slog.LevelInfo, "envoy nats connected", 0)
	if err := followsDefault.Handler().Handle(context.Background(), record); err != nil {
		t.Fatalf("handle an INFO record: %v", err)
	}
	if got := logs.String(); got != "" {
		t.Fatalf("a WARN-level default was handed an INFO record: %q", got)
	}
	warn := slog.NewRecord(time.Now(), slog.LevelWarn, "envoy nats disconnected", 0)
	if err := followsDefault.Handler().Handle(context.Background(), warn); err != nil {
		t.Fatalf("handle a WARN record: %v", err)
	}
	if got := logs.String(); !strings.Contains(got, `"msg":"envoy nats disconnected"`) {
		t.Fatalf("a WARN-level default was not handed a WARN record: %q", got)
	}
}

// A Client built as a literal has no logger. Its lines go through log, which answers
// followsDefault there rather than a nil logger a line would panic on; and the session consumers
// built on a client with no logger of their own write to the client's.
func TestAClientWithNoLoggerAndItsSessionConsumersWriteThroughTheClients(t *testing.T) {
	t.Parallel()
	var bare Client
	if bare.log() != followsDefault {
		t.Fatal("a zero-value Client does not write where the default points at each line")
	}
	var logs logtest.Buffer
	own := slog.New(slog.NewJSONHandler(&logs, nil))
	client := &Client{logger: own}
	if got := sessionConsumersOn(client, 0, nil).logger; got != own {
		t.Fatal("session consumers given no logger do not write to their client's")
	}
	given := slog.New(slog.NewJSONHandler(&logs, nil))
	if got := sessionConsumersOn(client, 0, given).logger; got != given {
		t.Fatal("session consumers given a logger do not write to it")
	}
}
