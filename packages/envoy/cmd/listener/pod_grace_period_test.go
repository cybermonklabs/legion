package main

import (
	"encoding/json"
	"os"
	"strconv"
	"testing"
	"time"
)

// listenerPodGoldens are the two pods that envoy-infra's gcp components render for the
// Deployments which run this command, recorded byte for byte: read is the path from this
// package's directory, path names the same file from the repository root, for a failure read in
// CI, and container is the container of that pod which runs this command. The components live in
// packages/envoy/infra, which is a Go module of its own, so this test reads what they render
// rather than calling them.
var listenerPodGoldens = []struct {
	read      string
	path      string
	container string
}{
	{
		read:      "../../infra/gcp/testdata/clustered-hub-listener-pod.golden",
		path:      "packages/envoy/infra/gcp/testdata/clustered-hub-listener-pod.golden",
		container: "listener",
	},
	{
		read:      "../../infra/gcp/testdata/dispatch-listener-pod.golden",
		path:      "packages/envoy/infra/gcp/testdata/dispatch-listener-pod.golden",
		container: "envoy-listener",
	},
}

// listenerPod is as much of a rendered pod as this test reads. Every pointer is nil when the
// golden does not carry that field, which is a failure here rather than a zero: the shape this
// test reads is the shape the components are required to render.
type listenerPod struct {
	Spec *struct {
		TerminationGracePeriodSeconds *int `json:"terminationGracePeriodSeconds"`
		Containers                    []struct {
			Name      string `json:"name"`
			Lifecycle *struct {
				PreStop *struct {
					Exec *struct {
						Command []string `json:"command"`
					} `json:"exec"`
				} `json:"preStop"`
			} `json:"lifecycle"`
		} `json:"containers"`
	} `json:"spec"`
}

// TestListenerPodGracesCoverTheStopBudget holds the Kubernetes side of stopBudget, which nothing
// else does: the pods gcp renders are the runtimes that grant these seconds, each grace period is
// written out as a number in another Go module's component, and this test is what holds those
// numbers to stopBudget. So raising a deadline here breaks nothing over there, and this test is
// what tells whoever raises it that both pods have to be raised with it.
//
// The kubelet runs the pre-stop hook and the shutdown inside the one
// terminationGracePeriodSeconds - it sends SIGTERM only once the hook returns - so the seconds
// left for the shutdown are the grace period less the hook's sleep, and they must cover
// stopBudget.
func TestListenerPodGracesCoverTheStopBudget(t *testing.T) {
	read := 0
	for _, golden := range listenerPodGoldens {
		t.Run(golden.container, func(t *testing.T) {
			grace, wait := readListenerPodGrace(t, golden.read, golden.path, golden.container)
			left := grace - wait
			if time.Duration(left)*time.Second < stopBudget {
				t.Fatalf("%s asks for %d seconds of termination grace and spends %d of them on %s's pre-stop sleep, leaving its shutdown %d seconds where shutdownListener spends %s of them (stopBudget in packages/envoy/cmd/listener/main.go): the pod asks for less than the shutdown needs, so the kubelet kills %s mid-drain and every delivery and role forward it still holds is lost. Whoever moves one of the two moves the other.",
					golden.path, grace, wait, golden.container, left, stopBudget, golden.container)
			}
			read++
			t.Logf("%s grants %s %d seconds of termination grace, spends %d on its pre-stop sleep and leaves its shutdown %d, where the stop budget is %s",
				golden.path, golden.container, grace, wait, left, stopBudget)
		})
	}

	t.Logf("read %d listener pod goldens", read)
	if read != len(listenerPodGoldens) {
		t.Fatalf("read %d of the %d listener pod goldens; a run that reads a pod less covers a pod less, so it fails here rather than passing", read, len(listenerPodGoldens))
	}
}

// readListenerPodGrace reads out of the golden at read the termination grace period of the pod it
// records and the seconds container's pre-stop hook spends before SIGTERM. Each field it reads is
// one the component is required to render, so a missing one fails here; path names the golden
// from the repository root, for a failure read in CI.
func readListenerPodGrace(t *testing.T, read, path, container string) (grace, wait int) {
	t.Helper()
	golden, err := os.ReadFile(read)
	if err != nil {
		t.Fatalf("read %s, a pod gcp renders for this command: %v", path, err)
	}
	var pod listenerPod
	if err := json.Unmarshal(golden, &pod); err != nil {
		t.Fatalf("parse %s as the pod JSON it records: %v", path, err)
	}
	if pod.Spec == nil || pod.Spec.TerminationGracePeriodSeconds == nil {
		t.Fatalf("%s carries no spec.terminationGracePeriodSeconds; this test cannot tell whether the pod covers the stop budget", path)
	}

	var command []string
	found := false
	for _, c := range pod.Spec.Containers {
		if c.Name != container {
			continue
		}
		found = true
		if c.Lifecycle == nil || c.Lifecycle.PreStop == nil || c.Lifecycle.PreStop.Exec == nil {
			t.Fatalf("container %s in %s has no lifecycle.preStop.exec; this test cannot tell how much of the grace period the kubelet spends before SIGTERM", container, path)
		}
		command = c.Lifecycle.PreStop.Exec.Command
	}
	if !found {
		t.Fatalf("%s has no container named %s; this test cannot tell which container's shutdown the grace period has to cover", path, container)
	}
	if len(command) != 2 || command[0] != "sleep" {
		t.Fatalf("the preStop hook of %s in %s is %q, not a sleep of some seconds; this test reads the wait out of a sleep", container, path, command)
	}
	wait, err = strconv.Atoi(command[1])
	if err != nil {
		t.Fatalf("the preStop sleep of %s in %s waits %q, which is no whole number of seconds: %v", container, path, command[1], err)
	}

	return *pod.Spec.TerminationGracePeriodSeconds, wait
}
