package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// drainDeadline supplies the shutdown deadline without arming a timer in the test.
// drain uses only the deadline to choose whether to drain or close.
type drainDeadline struct {
	context.Context
	at time.Time
}

func (d drainDeadline) Deadline() (time.Time, bool) { return d.at, true }

// The session-consumer connection's drain at shutdown gets what is left of the HTTP stop's window,
// and a shutdown that spent the window before it closes the connection without a drain. That close
// can lose an ack the connection had not yet sent, so the server delivers its message again, and
// it is said at WARN; a drain with time left says nothing.
func TestTheSessionConsumerDrainSaysWhenTheShutdownLeftItNoTime(t *testing.T) {
	t.Parallel()
	const undrained = "session consumer connection closed without a drain"
	for _, test := range []struct {
		name      string
		remaining time.Duration
		want      bool
	}{
		{name: "the window spent", remaining: -time.Second, want: true},
		{name: "time left", remaining: time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, uri := testnats.Start(t)
			logs := &logtest.Buffer{}
			delivery, err := connectSessionDelivery([]string{uri}, time.Hour, nil, logging.NewWithWriter("session-drain", logs))
			if err != nil {
				t.Fatalf("connect the session-consumer connection: %v", err)
			}
			t.Cleanup(delivery.close)
			// Both deadlines are in the wall clock's past. Only the delivery's clock
			// sees time left in the second case.
			fake := clocktest.New(time.Unix(0, 0))
			delivery.clock = fake
			delivery.drain(drainDeadline{Context: testwait.Context(t), at: fake.Now().Add(test.remaining)})
			if got := strings.Contains(logs.String(), undrained); got != test.want {
				t.Fatalf("the drain logged %q: %v, want %v:\n%s", undrained, got, test.want, logs.String())
			}
		})
	}
}
