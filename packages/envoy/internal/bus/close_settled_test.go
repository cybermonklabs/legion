package bus

import (
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

const (
	// closeHeldWindow is how long TestAFailedStartTryHasFinishedClosingWhenItReturns holds a
	// failed start's close, once the close's handling has begun, before it releases it. A
	// constructor that waits for that handling cannot return inside it: the test sets its wait's
	// bound to closeHeldBound, far past it. A constructor that does not wait returns as soon as it
	// has closed its connection, which takes microseconds, so the window is what the test gives it
	// to show that; a scheduler that kept such a constructor from running for the whole window would
	// let it through, and the test's control (the close not waited for) is run to show it does not.
	closeHeldWindow = 500 * time.Millisecond
	// closeHeldBound is the bound of the close's wait in that test (WithCloseSettleBound), far past
	// the window, so the bound never ends a wait the test is holding.
	closeHeldBound = 2 * time.Minute
)

// A start that failed has finished closing the client it made when the constructor returns, as long
// as the close's callbacks run within closeSettled's bound: nats.go's callbacks for the
// connection's close, which log the disconnect and the close, and the recovery the close starts,
// which logs that it was cancelled, have all run. So no line of such a close comes after the next
// line its caller writes: cmd/listener's last line, which names the credential and the refusal its
// start ended on, was once followed by "envoy nats recovery cancelled". (A callback that hangs past
// the bound is not waited for: closeSettled logs a WARN and returns, and that close's lines can
// then follow.)
//
// The test holds the client's handling of the close (WithClosedHook) until it releases it, and
// runs the constructor on a goroutine of its own, so it sees when the constructor returns: not
// before the handling has begun, not while it is held, and, once it is released, with the three
// lines already logged. Both ways a start's client fails are here: a start check that fails, which
// closes a client that was the constructor's to return (checkedStart), and an ensure of the stream
// the grant refuses, which closes the connection before it is the client's (startClient).
func TestAFailedStartTryHasFinishedClosingWhenItReturns(t *testing.T) {
	errCheck := errors.New("the start check failed")
	for _, row := range []struct {
		name string
		// server starts the row's server and answers its URL, on the test's goroutine.
		server func(t *testing.T) string
		// connect is the constructor under test, run on a goroutine of its own.
		connect func(urls []string, options ...ConnectOption) (*Client, error)
		want    error
	}{
		{
			name:   "a start check that fails",
			server: func(t *testing.T) string { return testnats.URL(t) },
			connect: func(urls []string, options ...ConnectOption) (*Client, error) {
				return Connect(urls, append(options, WithStartCheck(func(*Client) error { return errCheck }))...)
			},
			want: errCheck,
		},
		{
			name: "an ensure of the stream the grant refuses",
			server: func(t *testing.T) string {
				seed, public := testnats.User(t)
				grant := testnats.StartNkeyGranted(t, public, "_INBOX.>", jsAPI("INFO"), jsAPI("STREAM.CREATE", Stream))
				t.Setenv("NATS_NKEY_SEED", seed)
				return grant.URL
			},
			connect: ConnectOwningStream,
			want:    ErrPublishDenied,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			var records logtest.Recorder
			logtest.SetDefault(t, &records)
			name := "failed-start-" + t.Name()
			url := row.server(t)

			entered, release := make(chan struct{}), make(chan struct{})
			var enter, releasing sync.Once
			releaseClose := func() { releasing.Do(func() { close(release) }) }
			// A test that fails while it holds the close lets it go, so the constructor's goroutine
			// and the client's callbacks end with the test.
			t.Cleanup(releaseClose)
			hold := func() {
				enter.Do(func() { close(entered) })
				<-release
			}
			type outcome struct {
				client *Client
				err    error
			}
			returned := make(chan outcome, 1)
			go func() {
				client, err := row.connect([]string{url}, WithName(name), WithClosedHook(hold), WithCloseSettleBound(closeHeldBound))
				returned <- outcome{client, err}
			}()
			fail := func(got outcome, when string) {
				t.Helper()
				if got.client != nil {
					got.client.Close()
				}
				t.Fatalf("the constructor returned %s, with %v", when, got.err)
			}

			select {
			case <-entered:
			case got := <-returned:
				fail(got, "before the client's handling of its close began")
			case <-testwait.Context(t).Done():
				t.Fatal("the client's handling of the failed start's close had not begun by the test's deadline")
			}
			// A window, not a wait: the constructor must not return while the close's handling is
			// held, and closeHeldWindow is how long the test holds it to show that.
			select {
			case got := <-returned:
				fail(got, "while the client's handling of its close was held")
			case <-time.After(closeHeldWindow):
			}
			releaseClose()
			got := testwait.Receive(t, "the constructor to return once the close it held was released", returned)
			if got.client != nil {
				got.client.Close()
				t.Fatalf("the failed start returned a client, with %v", got.err)
			}
			if !errors.Is(got.err, row.want) {
				t.Fatalf("the start returned %v, want an error wrapping %v", got.err, row.want)
			}
			logged := map[string]bool{}
			for _, record := range records.Records() {
				record.Attrs(func(attr slog.Attr) bool {
					if attr.Key == "name" && attr.Value.String() == name {
						logged[record.Message] = true
					}
					return true
				})
			}
			for _, line := range []string{"envoy nats disconnected", "envoy nats connection closed", "envoy nats recovery cancelled"} {
				if !logged[line] {
					t.Errorf("the failed start had not logged %q when its constructor returned; it logged %v", line, logged)
				}
			}
		})
	}
}
