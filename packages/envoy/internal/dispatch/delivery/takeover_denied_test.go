package delivery

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
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
	t.Cleanup(owner.Close)
	t.Setenv("NATS_NKEY_SEED", seed)
	return uri, owner
}

// A take-over step the intake's grant refuses -- the durable's create, or the delete of a durable
// no process delivers from -- gives up at its first try with the server's refusal, and the take-over
// releases the consumer lease, rather than trying the step again every backoff until
// takeOverFinishWithin with the lease held: the grant refuses the step again however often it is
// sent. It once gave up after 9 tries, 10 s after the hold was taken.
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

			var given map[string]any
			deadline := time.Now().Add(30 * time.Second)
			for given == nil && time.Now().Before(deadline) {
				for _, record := range logs() {
					if record["level"] == "ERROR" && record["msg"] == "dispatch delivery: "+row.step && record["name"] == bus.DispatchDeliveryDurable {
						given = record
						break
					}
				}
				time.Sleep(50 * time.Millisecond)
			}
			if given == nil {
				t.Fatalf("no ERROR %q within 30s", "dispatch delivery: "+row.step)
			}
			if errText := fmt.Sprint(given["error"]); !strings.Contains(errText, `Permissions Violation for Publish to "`) {
				t.Fatalf("the step's ERROR carries %q, want the server's refusal", errText)
			}
			var tookHold time.Time
			retries := 0
			for _, record := range logs() {
				switch {
				case record["msg"] == "dispatch delivery: took the intake's hold" && tookHold.IsZero():
					tookHold, _ = time.Parse(time.RFC3339Nano, fmt.Sprint(record["time"]))
				case record["msg"] == "dispatch delivery: take-over step failed; trying it again" && record["step"] == row.step:
					retries++
				}
			}
			ended, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(given["time"]))
			if retries != 0 || tookHold.IsZero() || ended.Sub(tookHold) > 3*time.Second {
				t.Fatalf("the refused step was tried again %d times and given up %s after the hold was taken, want no retry and within 3s",
					retries, ended.Sub(tookHold).Round(time.Millisecond))
			}
		})
	}
}
