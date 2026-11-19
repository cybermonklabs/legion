package gcp_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The session listener's liveness and readiness probes read /livez, its startup probe reads
// /metrics, and none of the three reads /healthz. /healthz flushes to NATS, pings each key-value
// cache and reads the durable consumer's state under a three-second budget, while liveness and
// readiness are status-blind: none of that work can change their outcome, so each run of them on
// /healthz would be bus traffic that decides nothing. A status-blind probe reads /livez, which
// costs none, as gcp:Dispatch's listener's startup and liveness probes do (dispatch_kube.go,
// answersProbe), and this test holds that for this pod. The startup probe asks whether /v1
// serves, which /healthz's status does not answer, since the listener answers it 200 "starting"
// from its first answer, before its /v1 gate opens; /metrics answers it, with no credential,
// through the gauge sample the listener writes only once /v1 serves. The three probes are
// counted, so a pod that rendered none fails here rather than passing on an empty read.
func TestClusteredNatsHubListenerProbesReadLivezAndMetricsNeverHealthz(t *testing.T) {
	template := deploymentPodTemplate(t, runClusteredHub(t, clusteredArgs()), clusteredListenerName)
	listener := podContainer(t, template, "listener")

	read := 0
	for _, probe := range []struct{ name, route string }{
		{"startupProbe", "http://127.0.0.1:9020/metrics"},
		{"livenessProbe", "http://127.0.0.1:9020/livez"},
		{"readinessProbe", "http://127.0.0.1:9020/livez"},
	} {
		rendered, ok := listener[probe.name]
		require.True(t, ok, "%s is rendered", probe.name)
		command := rendered.(map[string]any)["exec"].(map[string]any)["command"].([]any)
		require.NotEmpty(t, command, "%s runs a command", probe.name)
		words := make([]string, 0, len(command))
		for _, word := range command {
			words = append(words, word.(string))
		}
		line := strings.Join(words, " ")
		require.Contains(t, line, probe.route, "%s reads %s", probe.name, probe.route)
		require.NotContains(t, line, "/healthz",
			"%s reads /healthz, whose handler flushes to NATS and reads the durable", probe.name)
		read++
	}

	require.Equal(t, 3, read, "the listener container's three probes were examined")
	t.Logf("%d probe(s) examined on the listener container", read)
}
