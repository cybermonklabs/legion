package delivery

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/bustest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// deniedIntakeServer is a server whose intake user may publish anything but deny
// (testnats.StartNkeyDenied), with the stream created by the server's admin (the returned client),
// and the intake user's seed set for the process the row starts.
func deniedIntakeServer(t *testing.T, deny ...string) (string, *bus.Client) {
	t.Helper()
	seed, public := testnats.User(t)
	uri := testnats.StartNkeyDenied(t, public, deny...)
	owner, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("the admin creates the stream: %v", err)
	}
	t.Cleanup(func() { bustest.Close(t, owner) })
	t.Setenv("NATS_NKEY_SEED", seed)
	return uri, owner
}

// A take-over step the intake's grant refuses -- the durable's create, or the delete of a durable
// no process delivers from -- gives up at its first try with the server's refusal, and the take-over
// releases the consumer lease, rather than trying the step again every backoff until
// takeOverFinishWithin with the lease held: the grant refuses the step again however often it is
// sent. It once gave up after 9 tries, 10 s after the hold was taken. The tries are counted, not
// timed: every failed try retryHeld sends again is logged at WARN before it waits, so a step given
// up at its first try logs no WARN for it between the taking of the hold and its ERROR.
func TestATakeOverStepTheGrantRefusesGivesUpAtOnce(t *testing.T) {
	create := "$JS.API.CONSUMER.CREATE." + bus.Stream + "." + bus.DispatchDeliveryDurable
	for _, row := range []struct {
		name, step string
		deny       []string
		unbound    bool
	}{
		{"the create", "create NATS consumer", []string{create, create + ".>", "$JS.API.CONSUMER.DURABLE.CREATE." + bus.Stream + "." + bus.DispatchDeliveryDurable}, false},
		{"the delete", "delete NATS consumer to take it over", []string{"$JS.API.CONSUMER.DELETE." + bus.Stream + "." + bus.DispatchDeliveryDurable}, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			logs := captureIntakeLogs(t)
			pool, ctx := deliveryTestPool(t)
			seedDeliverySettings(t, ctx, pool)
			uri, owner := deniedIntakeServer(t, row.deny...)
			if row.unbound {
				// A durable no process delivers from, so the take-over deletes it and creates it again.
				unbound := deliveryConsumerConfig(tuningFor(t))
				if _, err := owner.JS().AddConsumer(bus.Stream, &unbound); err != nil {
					t.Fatalf("the admin creates the unbound durable: %v", err)
				}
			}
			startIntakeProcess(t, pool, uri, nil)

			step := "dispatch delivery: " + row.step
			given := testwait.Until(t, fmt.Sprintf("an ERROR %q for %s", step, bus.DispatchDeliveryDurable), func() (loggedStep, bool) {
				records := logs()
				for index, record := range records {
					if record["level"] == "ERROR" && record["msg"] == step && record["name"] == bus.DispatchDeliveryDurable {
						return loggedStep{records: records, index: index}, true
					}
				}
				return loggedStep{records: records, index: -1}, false
			})
			if errText := fmt.Sprint(given.records[given.index]["error"]); !strings.Contains(errText, `Permissions Violation for Publish to "`) {
				t.Fatalf("the step's ERROR carries %q, want the server's refusal", errText)
			}
			tookHold, retries := false, 0
			for _, record := range given.records[:given.index] {
				switch {
				case record["msg"] == "dispatch delivery: took the intake's hold":
					tookHold = true
				case record["msg"] == "dispatch delivery: take-over step failed; trying it again" && record["step"] == row.step:
					retries++
				}
			}
			if !tookHold || retries != 0 {
				t.Fatalf("before the refused step's ERROR the hold was taken: %t, and the step was tried again %d times; want the hold taken and no retry",
					tookHold, retries)
			}
		})
	}
}

// loggedStep is one read of the logs of TestATakeOverStepTheGrantRefusesGivesUpAtOnce: every record
// logged so far, and the index among them of the step's ERROR, -1 until it is logged.
type loggedStep struct {
	records []map[string]any
	index   int
}

func (l loggedStep) String() string {
	messages := make([]string, 0, len(l.records))
	for _, record := range l.records {
		messages = append(messages, fmt.Sprint(record["msg"]))
	}
	return fmt.Sprintf("the step's ERROR at %d of %d records logged: %q", l.index, len(l.records), messages)
}
