package delivery

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/logtest"
)

// A line that comes after its delivery's lines were written keeps that delivery's cadence: at
// DEBUG where the delivery handed its envelope back outside the hand-back's cadence, and at its own
// level otherwise (deliveryLines). The GitHub client's lookup of an installation is one fetch for
// every caller waiting on it, which runs on a context that outlives its first caller
// (githubapp.Client.RepositoryToken, context.WithoutCancel), so its line can come once that
// delivery has answered. The fetch's read of the shared rate limit is held on channels until the
// caller has given up and the delivery's lines are written, and then fails. At 3a21edb2c9 such a
// line went to the default logger at its own level whatever the delivery's cadence was: one WARN
// line of a quiet delivery (the quality audit of cybermonklabs/legion#4209 at 3a21edb2c9, its
// probe quality_late_logger_test.go). The control, a delivery that was not quiet, writes it at
// WARN, so the test sees the line it bounds.
func TestALineThatComesAfterItsDeliveryKeepsTheDeliverysCadence(t *testing.T) {
	for _, quiet := range []bool{false, true} {
		t.Run(map[bool]string{false: "a_delivery_at_its_own_levels", true: "a_delivery_off_the_cadence"}[quiet], func(t *testing.T) {
			logs := captureLogsAt(t, slog.LevelDebug)
			limits := &heldLimitRead{entered: make(chan struct{}), release: make(chan struct{})}
			release := sync.OnceFunc(func() { close(limits.release) })
			t.Cleanup(release)
			served := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				close(served)
			}))
			t.Cleanup(server.Close)
			_, pemText := githubapptest.Key(t)
			client, err := githubapp.New(&auth.AppConfig{ClientID: "Iv1.late-line", PEM: pemText}, server.URL, githubapp.WithLimits(limits))
			if err != nil {
				t.Fatal(err)
			}
			lines := newDeliveryLines(slog.Default())
			ctx, cancel := context.WithCancel(logging.NewContext(context.Background(), lines.logger()))
			defer cancel()
			returned := make(chan error, 1)
			go func() {
				_, err := client.RepositoryToken(ctx, "acme", "widgets")
				returned <- err
			}()
			waitOn(t, limits.entered, "the shared fetch's read of the rate limit")
			cancel()
			if err := <-returned; !errors.Is(err, context.Canceled) {
				t.Fatalf("the token's caller answered %v, want its cancellation while the shared fetch is held", err)
			}
			lines.write(quiet)
			release()
			waitOn(t, served, "the shared fetch's request")
			var late []map[string]any
			for _, record := range logs() {
				if message, _ := record["msg"].(string); strings.Contains(message, "the shared GitHub rate limit was not read") {
					late = append(late, record)
				}
			}
			want := map[bool]string{false: "WARN", true: "DEBUG"}[quiet]
			t.Logf("quiet=%v late_lines=%v", quiet, late)
			if len(late) != 1 || late[0]["level"] != want {
				t.Fatalf("the late line of the shared fetch = %v, want one line at %s: a delivery's line keeps its cadence after the delivery answered", late, want)
			}
		})
	}
}

// heldLimitRead is the shared rate limits of a database whose read of a limit is held until
// release, and then fails; it records and strikes as unreadableLimits does.
type heldLimitRead struct {
	unreadableLimits
	entered chan struct{}
	release chan struct{}
}

func (l *heldLimitRead) Wait(context.Context, string) (time.Duration, time.Time, error) {
	close(l.entered)
	<-l.release
	return 0, time.Time{}, errors.New("the test's read of the shared limit failed after its caller ended")
}

// A delivery writes its lines before it answers NATS, so a reader that sees the answer, the
// durable's ack floor past the envelope, finds them in the log however long the log takes to write
// a line (Intake.deliver, deliveryLines). The log here takes 500 ms to write the ERROR line of a
// malformed envelope. At 4684370d14 the delivery answered first and wrote its lines after, so the
// line was not in the log once the ack floor passed the envelope: main's
// TestIntakeLogsAMalformedEnvelopePayload, which reads the log so, failed once that way under
// -race. The control is the same envelope with a log that takes no time over that line.
func TestADeliveryWritesItsLinesBeforeItAnswers(t *testing.T) {
	const line = "dispatch delivery: decode envelope payload"
	topic := contracts.GithubResourceSubject("acme", "widgets", "pr", "7")
	for _, delay := range []time.Duration{0, 500 * time.Millisecond} {
		t.Run("line_takes_"+delay.String(), func(t *testing.T) {
			pool, ctx := deliveryTestPool(t)
			seedDeliverySettings(t, ctx, pool)
			var buffer lockedBuffer
			logtest.SetDefault(t, slowLine{Handler: slog.NewJSONHandler(&buffer, &slog.HandlerOptions{Level: slog.LevelInfo}), message: line, delay: delay})
			fake := newFakeGitHub(t)
			natsClient := intakeTestClient(t)
			startIntake(t, newIntake(tuningFor(t), natsClient, pool, deliveryTestLeases(t, pool), fake.newTestClient(), events.NewBroker(), loadedSettingsCache(t, ctx, pool), unrenamed()))
			awaitBoundDurable(t, natsClient)

			publishGitHubEnvelope(t, natsClient, topic, `["pr","closed"]`)
			awaitAcknowledged(t, natsClient)
			if !strings.Contains(buffer.String(), line) {
				t.Errorf("the durable's ack floor passed the malformed envelope before its ERROR %q was in the log (the line takes %v to write): the delivery answered NATS before it wrote its lines; logged %s", line, delay, buffer.String())
			}
		})
	}
}

// slowLine is a slog.Handler that takes delay to write a record whose message is message, as a log
// sink under load may, and writes every other record at once.
type slowLine struct {
	slog.Handler
	message string
	delay   time.Duration
}

func (h slowLine) Handle(ctx context.Context, record slog.Record) error {
	if record.Message == h.message {
		time.Sleep(h.delay)
	}
	return h.Handler.Handle(ctx, record)
}

func (h slowLine) WithAttrs(attrs []slog.Attr) slog.Handler {
	return slowLine{Handler: h.Handler.WithAttrs(attrs), message: h.message, delay: h.delay}
}

func (h slowLine) WithGroup(name string) slog.Handler {
	return slowLine{Handler: h.Handler.WithGroup(name), message: h.message, delay: h.delay}
}
