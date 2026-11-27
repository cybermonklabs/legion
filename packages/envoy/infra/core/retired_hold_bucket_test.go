package core

import (
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/natsgrant"
)

// No grant a hub can hold names the retired hold bucket, KV_envoy_maintenance or its keys: neither
// a static user's set this package renders nor a grant only the auth service answers (the
// operator's, in each phase of the writer hold, and the hold probe's). The hold lives in the
// secrets broker's Postgres, since any NATS user with a JetStream API grant can have the server
// store an answer over a key of a bucket. natsgrant's own TestNoGrantNamesTheRetiredHoldBucket walks
// every grant function by its declarations; this one walks what a hub renders.
func TestNoGrantNamesTheRetiredHoldBucket(t *testing.T) {
	static := map[string]Permissions{
		"SessionPermissions()": SessionPermissions(),
		"DaemonPermissions()":  DaemonPermissions(),
	}
	for _, id := range []string{"example-host-a", "example-dispatch", "example-hub"} {
		static["ListenerPermissions("+id+")"] = ListenerPermissions(id)
		static["ListenerHostPermissions("+id+")"] = ListenerHostPermissions(id)
		static["DispatchPermissions("+id+")"] = DispatchPermissions(id)
		static["HostPermissions("+id+")"] = HostPermissions(id)
	}
	for _, phase := range []natsgrant.OperatorPhase{natsgrant.OperatorPhaseReleased, natsgrant.OperatorPhasePrepare, natsgrant.OperatorPhaseHeld} {
		static["natsgrant.Operator("+string(phase)+")"] = rules(natsgrant.Operator(phase))
	}
	static["natsgrant.HoldProbe()"] = rules(natsgrant.HoldProbe())
	for name, set := range static {
		entries := slices.Concat(set.Publish.Allow, set.Publish.Deny, set.Subscribe.Allow, set.Subscribe.Deny)
		if len(entries) == 0 {
			t.Fatalf("%s holds no entry: the walk read nothing", name)
		}
		for _, entry := range entries {
			if strings.Contains(entry, "KV_envoy_maintenance") || strings.Contains(entry, "$KV.envoy_maintenance.") {
				t.Errorf("%s names the retired hold bucket in %q", name, entry)
			}
		}
	}
}
