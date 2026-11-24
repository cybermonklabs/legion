package config

import (
	"reflect"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/capabilities"
)

// reserved is a pod's CPU and memory reservation, both set, each request equal to its limit.
var reserved = Reservation{CPU: "500m", CPURequest: "500m", Memory: "1Gi", MemoryRequest: "1Gi"}

// bursting requests less memory than its limit, and 1000m of cpu under a limit of 1: a quantity
// compare, so only the memory makes it Burstable.
var bursting = Reservation{CPU: "1", CPURequest: "1000m", Memory: "32Gi", MemoryRequest: "8Gi"}

// spelledApart requests what it is limited to, each spelled two ways (1000m against 1, 1024Mi
// against 1Gi): a string compare would read it Burstable, a quantity compare reads it Guaranteed.
var spelledApart = Reservation{CPU: "1", CPURequest: "1000m", Memory: "1Gi", MemoryRequest: "1024Mi"}

// Deployment reads the configuration alone: the decisions, the runtime, whether a broker is
// configured, the pods that reserve no CPU and memory — a pod lacking either quantity counts, the
// issue pod first, and the controller's pod only under controller: daemon (a Kubernetes block the
// loader settled holds both, so only a value built by hand names one) — and the pods that request
// less than their limit, the controller's pod among them only under controller: daemon. A tmux
// configuration names no pod: the tmux runtime sets no limits at all, which the report says in its
// own words.
func TestDeploymentReadsTheConfigurationAlone(t *testing.T) {
	t.Parallel()
	decided := map[capabilities.Name]string{capabilities.Secrets: "dispatch://LEGION-205 enrolls pods later"}
	kubernetes := func(resources PodResources, agentSecrets *AgentSecretsConfig, launch ControllerLaunch) Config {
		return Config{
			Runtime:          Runtime{Name: "kubernetes", Kubernetes: &Kubernetes{Resources: resources, AgentSecrets: agentSecrets}},
			ControllerLaunch: launch,
			Capabilities:     Capabilities{Decided: decided},
		}
	}
	for _, tc := range []struct {
		name string
		cfg  Config
		want capabilities.Deployment
	}{
		{"tmux", Config{Runtime: Runtime{Name: "tmux"}, Capabilities: Capabilities{Decided: decided}},
			capabilities.Deployment{Runtime: "tmux", Decided: decided}},
		{"nothing reserved, no broker", kubernetes(PodResources{}, nil, ControllerLaunchOperator),
			capabilities.Deployment{Runtime: "kubernetes", Decided: decided, UnreservedPods: []capabilities.Pod{capabilities.PodIssue}}},
		{"the issue pod reserved, a broker configured", kubernetes(PodResources{Issue: reserved},
			&AgentSecretsConfig{URL: "https://secrets.internal.example"}, ControllerLaunchOperator),
			capabilities.Deployment{Runtime: "kubernetes", Decided: decided, AgentSecrets: true}},
		{"the issue pod missing one quantity, and the controller under controller: daemon", kubernetes(PodResources{Issue: Reservation{CPU: "2"}}, nil, ControllerLaunchDaemon),
			capabilities.Deployment{Runtime: "kubernetes", Decided: decided, UnreservedPods: []capabilities.Pod{capabilities.PodIssue, capabilities.PodController}}},
		{"both pods requesting less than their limits", kubernetes(PodResources{Issue: bursting, Controller: bursting}, nil, ControllerLaunchDaemon),
			capabilities.Deployment{Runtime: "kubernetes", Decided: decided, BurstablePods: []capabilities.Pod{capabilities.PodIssue, capabilities.PodController}}},
		{"a Burstable controller the daemon does not launch", kubernetes(PodResources{Issue: reserved, Controller: bursting}, nil, ControllerLaunchOperator),
			capabilities.Deployment{Runtime: "kubernetes", Decided: decided}},
		{"a request spelled apart from its equal limit", kubernetes(PodResources{Issue: spelledApart, Controller: spelledApart}, nil, ControllerLaunchDaemon),
			capabilities.Deployment{Runtime: "kubernetes", Decided: decided}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.Deployment(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("cfg.Deployment() = %+v, want %+v", got, tc.want)
			}
		})
	}
}
