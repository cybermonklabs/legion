package gcp_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The session listener's startup and liveness probes read /livez, and only its readiness probe
// reads /healthz (dispatch://INFRA-2739/artifact/open-items-md, decision 15). /healthz flushes to
// NATS, pings each key-value cache and reads the durable consumer's state under a three-second
// budget, while startup and liveness are status-blind: none of that work can change their
// outcome, so each run of them is bus traffic that decides nothing. gcp:Dispatch's listener
// already pairs this readiness probe with a /livez check that costs none (dispatch_kube.go,
// answersProbe). Readiness stays on /healthz, which is what tells a starting pod from a serving
// one. The three probes are counted, so a pod that rendered none fails here rather than passing
// on an empty read.
func TestClusteredNatsHubListenerStartupAndLivenessReadLivez(t *testing.T) {
	template := deploymentPodTemplate(t, runClusteredHub(t, clusteredArgs()), clusteredListenerName)
	listener := podContainer(t, template, "listener")

	read := 0
	for _, name := range []string{"startupProbe", "livenessProbe"} {
		probe, ok := listener[name]
		require.True(t, ok, "%s is rendered", name)
		command := probe.(map[string]any)["exec"].(map[string]any)["command"].([]any)
		require.NotEmpty(t, command, "%s runs a command", name)
		require.Contains(t, command, "http://127.0.0.1:9020/livez", "%s reads /livez", name)
		require.NotContains(t, command, "http://127.0.0.1:9020/healthz",
			"%s reads /healthz, whose handler flushes to NATS and reads the durable", name)
		read++
	}

	ready := listener["readinessProbe"].(map[string]any)["exec"].(map[string]any)["command"].([]any)
	require.NotEmpty(t, ready, "the readiness probe runs a command")
	require.Contains(t, ready, "http://127.0.0.1:9020/healthz", "readiness reads /healthz")
	read++

	require.Equal(t, 3, read, "the listener container's three probes were examined")
	t.Logf("%d probe(s) examined on the listener container", read)
}
