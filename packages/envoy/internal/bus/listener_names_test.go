package bus_test

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
)

// The listener's durable and role-lane queue keep the names a deployed listener's durable and
// queue group carry: a space in the machine id becomes a hyphen in both. Each other character NATS
// refuses in a consumer name becomes '%' and its byte in two upper-case hex digits. ListenerSpelling
// is the same spelling with no prefix, which an edge listener's inbox prefix and receipt family
// carry, so it is each name with its prefix cut off.
func TestListenerDurableAndQueueKeepTheListenersNames(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ machineID, durable, queue string }{
		{"example-host-a", "listener-example-host-a", "envoy-listener-example-host-a"},
		{"example host", "listener-example-host", "envoy-listener-example-host"},
		{"host.example.internal", "listener-host%2Eexample%2Einternal", "envoy-listener-host%2Eexample%2Einternal"},
		{"a*b>c/d\\e\tf\rg\nh\fi", "listener-a%2Ab%3Ec%2Fd%5Ce%09f%0Dg%0Ah%0Ci", "envoy-listener-a%2Ab%3Ec%2Fd%5Ce%09f%0Dg%0Ah%0Ci"},
	} {
		if got := bus.ListenerDurable(tc.machineID); got != tc.durable {
			t.Errorf("ListenerDurable(%q) = %q, want %q", tc.machineID, got, tc.durable)
		}
		if got := bus.ListenerQueue(tc.machineID); got != tc.queue {
			t.Errorf("ListenerQueue(%q) = %q, want %q", tc.machineID, got, tc.queue)
		}
		if got, want := bus.ListenerSpelling(tc.machineID), strings.TrimPrefix(tc.durable, "listener-"); got != want {
			t.Errorf("ListenerSpelling(%q) = %q, want %q", tc.machineID, got, want)
		}
	}
}

// CheckListenerMachineID refuses an id holding '%', which could spell another id's escape and so
// give that id's names, and an id whose durable, escapes included, is over NATS's 255 bytes.
func TestCheckListenerMachineIDRefusesAnIDWhoseNamesAreNotItsOwn(t *testing.T) {
	t.Parallel()
	for _, row := range []struct{ name, machineID, want string }{
		{"a plain id", "gke", ""},
		{"a fully qualified host name", "host.example.internal", ""},
		{"an id spelling a dot's escape", "host%2Eexample%2Einternal", "holds '%', which starts an escape in the listener's NATS names, so another machine id can give the same names"},
		{"a lone escape character", "example%", "holds '%', which starts an escape in the listener's NATS names, so another machine id can give the same names"},
		{"the longest plain id", strings.Repeat("a", 246), ""},
		{"a plain id one byte too long", strings.Repeat("a", 247), "makes the durable name listener-<id> 256 bytes long, over NATS's 255-byte limit for a consumer name"},
		{"the longest dotted id", strings.Repeat("a.", 41) + strings.Repeat("a", 82), ""},
		{"a dotted id whose escapes take it over", strings.Repeat("a.", 41) + strings.Repeat("a", 83), "makes the durable name listener-<id> 256 bytes long, over NATS's 255-byte limit for a consumer name"},
	} {
		t.Run(row.name, func(t *testing.T) {
			err := bus.CheckListenerMachineID(row.machineID)
			if row.want == "" {
				if err != nil {
					t.Fatalf("CheckListenerMachineID(%q) = %v, want nil", row.machineID, err)
				}
				return
			}
			if err == nil || err.Error() != row.want {
				t.Fatalf("CheckListenerMachineID(%q) = %v, want %q", row.machineID, err, row.want)
			}
		})
	}
}

func TestDispatchNamesKeepDispatchsExistingNames(t *testing.T) {
	t.Parallel()
	if got := bus.DispatchDeliveryDurable; got != "delivery-events" {
		t.Errorf("DispatchDeliveryDurable = %q, want %q", got, "delivery-events")
	}
	if got := bus.DispatchJobDurable; got != "delivery-jobs" {
		t.Errorf("DispatchJobDurable = %q, want %q", got, "delivery-jobs")
	}
	if got := bus.WebhookRedeliveryBucket; got != "envoy_webhook_redelivery" {
		t.Errorf("WebhookRedeliveryBucket = %q, want %q", got, "envoy_webhook_redelivery")
	}
}

// The Legion agent registry's bucket is the name the INFRA-204 architect fixed, which the listener's
// NATS grant (packages/envoy/infra/core/permissions.go) and every reader of the bucket share.
func TestLegionAgentBucketKeepsItsName(t *testing.T) {
	t.Parallel()
	if got := bus.LegionAgentBucket; got != "legion_agents" {
		t.Errorf("LegionAgentBucket = %q, want %q", got, "legion_agents")
	}
}
