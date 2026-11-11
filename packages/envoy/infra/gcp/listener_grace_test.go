package gcp_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/gcp"
)

// One assertion spans two components here because the clustered hub's listener pod and
// gcp:Dispatch's listener pod run the same binary with the same stop budget: the same preStop
// wait and the same three deadlines of packages/envoy/cmd/listener/main.go. A shared constant
// would be the other way to hold them equal, and it is the wrong one: naming
// clusteredHubListenerGraceSeconds in gcp:Dispatch's code would couple the Dispatch listener to
// the hub's name, and a reader of either pod would have to look the other component up. This
// test holds them equal instead. It reads the app pod too, which keeps its own 35 s, coupled to
// stopGrace in packages/envoy/cmd/dispatch/shutdown.go, so a change that moves every grace at
// once fails here.
func TestDispatchAndClusteredHubListenerPodsStopInsideTheirGrace(t *testing.T) {
	hub := runClusteredHub(t, clusteredArgs())
	dispatch, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)

	pods := []struct {
		name      string
		container string
		listener  bool
		template  map[string]any
	}{
		{clusteredListenerName, "listener", true, deploymentPodTemplate(t, hub, clusteredListenerName)},
		{"example-dispatch-listener", "envoy-listener", true, deploymentPodTemplate(t, dispatch, "example-dispatch-listener")},
		{"example-dispatch", "envoy-dispatch", false, deploymentPodTemplate(t, dispatch, "example-dispatch")},
	}

	read := 0
	for _, pod := range pods {
		grace := pod.template["spec"].(map[string]any)["terminationGracePeriodSeconds"].(float64)
		wait := preStopSeconds(t, podContainer(t, pod.template, pod.container))
		if pod.listener {
			require.Equal(t, float64(gcp.ClusteredHubListenerGraceSeconds), grace, "%s carries the listener's grace", pod.name)
			require.LessOrEqual(t, wait+float64(gcp.ClusteredHubListenerShutdownSeconds), grace,
				"%s: its preStop wait and the listener's own stop", pod.name)
			// 135 is durableBindDeadline, the listener's own
			// (packages/envoy/cmd/listener/durable.go:176), which this package cannot import: it
			// is a constant of package main. A hung pod holds its durable for the whole grace, so
			// the replacement pod gives up waiting for it unless the grace stays under that
			// deadline.
			require.Less(t, grace, 135.0, "%s frees its durable before a starting listener gives up", pod.name)
		} else {
			require.Equal(t, 35.0, grace, "%s keeps the app's own grace, the one stopGrace pins", pod.name)
		}
		read++
		t.Logf("%s: terminationGracePeriodSeconds %.0f, preStop %.0f s", pod.name, grace, wait)
	}
	require.Equal(t, 3, read, "the two listener pods and the app pod were read")
	t.Logf("%d pod template(s) examined", read)
}
