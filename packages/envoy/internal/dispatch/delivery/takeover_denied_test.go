package delivery

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// deniedIntakeServer is a server whose intake user may publish anything but deny
// (testnats.StartNkeyDenied), with the stream created by the server's admin (the returned client),
// and the intake user's seed, which the row's process connects with (bus.WithEnvironment,
// testnats.SeedOnly) rather than the process's environment, so the row runs beside the others.
func deniedIntakeServer(t *testing.T, deny ...string) (uri string, owner *bus.Client, seed string) {
	t.Helper()
	seed, public := testnats.User(t)
	uri = testnats.StartNkeyDenied(t, public, deny...)
	owner, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("the admin creates the stream: %v", err)
	}
	t.Cleanup(owner.Close)
	return uri, owner, seed
}

// takeOverStepRetried is the line retryHeld logs for a take-over step it tries again.
const takeOverStepRetried = "dispatch delivery: take-over step failed; trying it again"

// A take-over step the intake's grant refuses -- the durable's create, or the delete of a durable
// no process delivers from -- gives up at its first try with the server's refusal, and the take-over
// releases the consumer lease, rather than trying the step again every backoff until
// takeOverFinishWithin with the lease held: the grant refuses the step again however often it is
// sent. It once gave up after 9 tries, 10 s after the hold was taken.
//
// The take-over's bound, each try's bound and the waits between its tries run on the intake's hold
// clock (heldDurable.holdClock, retryHeld), a fake the row never moves. So no try's bound
// (takeOverCallTimeout) can end the step, and a step tried again waits for ever on its first retry
// (takeOverFirstRetry): the row waits for the step's ERROR or for its first retry line, whichever
// comes, and requires the ERROR, with the server's refusal, no retry, and the hold taken before it.
// The give-up therefore came at the first try, with no time of the take-over's clock gone by.
func TestATakeOverStepTheGrantRefusesGivesUpAtOnce(t *testing.T) {
	t.Parallel()
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
			t.Parallel()
			logs := captureIntakeLogs(t)
			pool, ctx := deliveryTestPool(t)
			seedDeliverySettings(t, ctx, pool)
			uri, owner, seed := deniedIntakeServer(t, row.deny...)
			if row.unbound {
				// A durable no process delivers from, so the take-over deletes it and creates it again.
				unbound := deliveryConsumerConfig(tuningFor(t))
				if _, err := owner.JS().AddConsumer(bus.Stream, &unbound); err != nil {
					t.Fatalf("the admin creates the unbound durable: %v", err)
				}
			}
			takeOverClock := fakeIntakeClock()
			startIntakeProcessWith(t, pool, uri, nil, func(p *intakeProcess) { p.intake.holdClock = takeOverClock },
				bus.WithEnvironment(testnats.SeedOnly(seed)))

			given := testwait.Until(t, fmt.Sprintf("the refused step %q to be given up at ERROR or tried again", row.step), func() (map[string]any, bool) {
				for _, record := range logs() {
					if record["name"] != bus.DispatchDeliveryDurable {
						continue
					}
					switch {
					case record["level"] == "ERROR" && record["msg"] == "dispatch delivery: "+row.step:
						return record, true
					case record["msg"] == takeOverStepRetried && record["step"] == row.step:
						return record, true
					}
				}
				return nil, false
			})
			if given["msg"] == takeOverStepRetried {
				t.Fatalf("the refused step was tried again rather than given up at its first try: %v", given)
			}
			if errText := fmt.Sprint(given["error"]); !strings.Contains(errText, `Permissions Violation for Publish to "`) {
				t.Fatalf("the step's ERROR carries %q, want the server's refusal", errText)
			}
			tookHold, retries := false, 0
			for _, record := range logs() {
				switch {
				case record["msg"] == "dispatch delivery: took the intake's hold":
					tookHold = true
				case record["msg"] == takeOverStepRetried && record["step"] == row.step:
					retries++
				}
			}
			if retries != 0 || !tookHold || !takeOverClock.Now().Equal(fakeIntakeClock().Now()) {
				t.Fatalf("the refused step was tried again %d times (hold taken: %t, take-over clock moved to %s), want no retry after the hold was taken, with no time of the take-over's clock gone by",
					retries, tookHold, takeOverClock.Now())
			}
		})
	}
}
