package main

import (
	"testing"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
)

// roleLaneProjectDropLine is the role lane's line for a message on a lane that names a Legion role
// under a project it does not work for (deliverRoleMessage, roleLaneProjectError).
const roleLaneProjectDropLine = "listener role lane refused a role named under a project it does not work for; no holder is sent it"

// A message on one project's lane that names a Legion role of another project reaches no holder
// even once the listener stops: the lane drops it, reported no_holder, before the stop's keep, so
// no kept copy waits under the role's bare claim key for the role's own project's holder. A lane
// that judged the project only where it resolves a holder keeps the message at the stop on
// legion-infra-reviewer's kept lane, whose handover gives INFRA's reviewer PRIV's message.
// Control: a message on the role's own lane at the stop is kept.
func TestARoleLaneOfAnotherProjectIsDroppedNotKeptAtTheStop(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	cfg := wireTestRoleForwarder(t, harness.config)
	cfg.roleForwards.stopHolding()
	deliver := func(project, key string) contracts.Envelope {
		t.Helper()
		item := listenerTestEnvelope(contracts.RoleSubject(project, "legion-infra-reviewer"), key)
		coreNATSDeliveryHandler(cfg)(&natsgo.Msg{Subject: item.Topic, Data: marshalListenerEnvelope(t, item)})
		return item
	}
	other := deliver("PRIV", "project-at-the-stop-other")
	own := deliver("INFRA", "project-at-the-stop-own")
	cfg.roleForwards.settled(t)

	logs := harness.logs.String()
	if got := countLogged(logs, keptLine, own.EventID); got != 1 {
		t.Fatalf("control: the message on INFRA's own lane was kept %d times at the stop, want 1:\n%s", got, logs)
	}
	if got := countLogged(logs, keptLine, other.EventID); got != 0 {
		t.Fatalf("the message on %s was kept %d times at the stop, for INFRA's reviewer, want none:\n%s", other.Topic, got, logs)
	}
	if got := countLogged(logs, roleLaneProjectDropLine, other.EventID); got != 1 {
		t.Fatalf("the message on %s was dropped as another project's %d times, want 1:\n%s", other.Topic, got, logs)
	}
}
