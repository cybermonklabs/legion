package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The session-consumer connection's drain at shutdown gets what is left of the HTTP stop's window,
// and a shutdown that spent the window before it closes the connection without a drain. That close
// can lose an ack the connection had not yet sent, so the server delivers its message again, and
// it is said at WARN; a drain with time left says nothing.
func TestTheSessionConsumerDrainSaysWhenTheShutdownLeftItNoTime(t *testing.T) {
	const undrained = "session consumer connection closed without a drain"
	for _, test := range []struct {
		name string
		left time.Duration
		want bool
	}{
		{name: "the window spent", left: -time.Second, want: true},
		{name: "time left", left: time.Minute},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, uri := testnats.Start(t)
			logs := &logtest.Buffer{}
			delivery, err := connectSessionDelivery([]string{uri}, time.Hour, nil, logging.NewWithWriter("session-drain", logs))
			if err != nil {
				t.Fatalf("connect the session-consumer connection: %v", err)
			}
			t.Cleanup(delivery.close)
			ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(test.left))
			defer cancel()

			delivery.drain(ctx)
			if got := strings.Contains(logs.String(), undrained); got != test.want {
				t.Fatalf("the drain logged %q: %v, want %v:\n%s", undrained, got, test.want, logs.String())
			}
		})
	}
}
