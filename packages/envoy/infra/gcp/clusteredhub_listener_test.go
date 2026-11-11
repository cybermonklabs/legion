package gcp_test

import (
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/infra/internal/pulumitest"
)

// clusteredListenerName is clusteredArgs' session listener's name, <name>-listener: its
// ServiceAccount's, Deployment's, Service's, NetworkPolicy's and budget's.
const clusteredListenerName = "envoy-nats-listener"

// listenerPodLabels are the session listener pod's labels: what its Deployment selects, what its
// Service sends to, what its NetworkPolicy fences and what its budget counts.
var listenerPodLabels = map[string]any{
	"app.kubernetes.io/name":      "nats-listener",
	"app.kubernetes.io/instance":  "envoy-nats",
	"app.kubernetes.io/component": "listener",
}

// clusteredListenerSpec is the session listener's Deployment spec, as the mocks recorded it.
func clusteredListenerSpec(t *testing.T, m *pulumitest.Mocks) map[string]any {
	t.Helper()
	return m.Named(t, deploymentType, clusteredListenerName).Inputs.Mappable()["spec"].(map[string]any)
}

// clusteredListenerPodSpec is the session listener's pod spec.
func clusteredListenerPodSpec(t *testing.T, m *pulumitest.Mocks) map[string]any {
	t.Helper()
	return deploymentPodTemplate(t, m, clusteredListenerName)["spec"].(map[string]any)
}

// The session listener's pod, whole: the listener container alone, as the uid its image's own
// user has, with the NATS seed from the operator's Secret by reference and nothing of the tailnet
// host beside it.
func TestClusteredNatsHubListenerPodMatchesGolden(t *testing.T) {
	requirePodGolden(t, runClusteredHub(t, clusteredArgs()), clusteredListenerName, "clustered-hub-listener-pod.golden")
}

// The Deployment runs one listener, wearing the listener's selector labels, in the hub's
// namespace; it surges the new pod in before it stops the old one, so the durable the old pod
// holds is free by the time the new one has waited its bind deadline out; the pod has 35 s to
// stop; and it waits on its own ServiceAccount, and on the priority class's read where there is
// one.
func TestClusteredNatsHubListenerDeployment(t *testing.T) {
	m := runClusteredHub(t, clusteredArgs())
	r := m.Named(t, deploymentType, clusteredListenerName)
	requireClusteredKubernetesChild(t, r)
	require.Equal(t, map[string]any{"name": clusteredListenerName, "namespace": "envoy"}, r.Inputs.Mappable()["metadata"])

	spec := clusteredListenerSpec(t, m)
	require.Equal(t, 1.0, spec["replicas"], "one listener: a second copy would wait the first's durable out for ever")
	require.Equal(t, map[string]any{
		"type": "RollingUpdate", "rollingUpdate": map[string]any{"maxUnavailable": 0.0, "maxSurge": 1.0},
	}, spec["strategy"])
	require.Equal(t, map[string]any{"matchLabels": listenerPodLabels}, spec["selector"])
	require.Equal(t, map[string]any{"labels": listenerPodLabels}, spec["template"].(map[string]any)["metadata"])

	pod := clusteredListenerPodSpec(t, m)
	require.Equal(t, 35.0, pod["terminationGracePeriodSeconds"])
	require.Equal(t, clusteredListenerName, pod["serviceAccountName"])
	require.Equal(t, false, pod["enableServiceLinks"])
	require.NotContains(t, pod, "priorityClassName")
	require.Contains(t, r.RPC.GetDependencies(), clusteredChildURN(serviceAccountType, clusteredListenerName))

	args := clusteredArgs()
	args.PriorityClassName = pulumi.StringRef("envoy-critical")
	m = runClusteredHub(t, args)
	require.Equal(t, "envoy-critical", clusteredListenerPodSpec(t, m)["priorityClassName"])
	require.Subset(t, m.Named(t, deploymentType, clusteredListenerName).RPC.GetDependencies(), []string{
		clusteredChildURN(serviceAccountType, clusteredListenerName),
		clusteredChildURN(priorityClassType, "envoy-nats-priority-class"),
	})
}

// The listener's ServiceAccount mounts no token: the listener calls no Kubernetes API, so its pod
// carries no credential for one.
func TestClusteredNatsHubListenerServiceAccount(t *testing.T) {
	r := runClusteredHub(t, clusteredArgs()).Named(t, serviceAccountType, clusteredListenerName)
	requireClusteredKubernetesChild(t, r)
	require.Equal(t, map[string]any{
		"apiVersion": "v1", "kind": "ServiceAccount",
		"metadata":                     map[string]any{"name": clusteredListenerName, "namespace": "envoy"},
		"automountServiceAccountToken": false,
	}, r.Inputs.Mappable())
}

// The listener's own cluster Service, which each tailnet host's serve config forwards the
// Tailscale Service's listener port to: one port, 9020 to the pod's 9020, selecting the
// listener's pods alone.
func TestClusteredNatsHubListenerService(t *testing.T) {
	r := runClusteredHub(t, clusteredArgs()).Named(t, serviceType, clusteredListenerName)
	requireClusteredKubernetesChild(t, r)
	inputs := r.Inputs.Mappable()
	ports := inputs["spec"].(map[string]any)["ports"].([]any)
	require.Len(t, ports, 1, "the listener serves one port")
	require.Equal(t, map[string]any{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{"name": clusteredListenerName, "namespace": "envoy"},
		"spec": map[string]any{
			"selector": listenerPodLabels,
			"ports":    []any{map[string]any{"port": 9020.0, "targetPort": 9020.0}},
		},
	}, inputs)
}

// The listener's port is open to the tailnet entry's pods and to nothing else, which is what lets
// it serve /v1 with no bearer; every other port of the pod is admitted to nobody. The rules are
// counted before the absence of any other is asserted, since an empty list holds no rule either.
func TestClusteredNatsHubListenerNetworkPolicy(t *testing.T) {
	r := runClusteredHub(t, clusteredArgs()).Named(t, networkPolicyType, clusteredListenerName)
	requireClusteredKubernetesChild(t, r)
	inputs := r.Inputs.Mappable()
	require.Equal(t, map[string]any{"name": clusteredListenerName, "namespace": "envoy"}, inputs["metadata"])
	spec := inputs["spec"].(map[string]any)
	require.Equal(t, []any{"Ingress"}, spec["policyTypes"])
	require.Equal(t, map[string]any{"matchLabels": listenerPodLabels}, spec["podSelector"])

	rules := spec["ingress"].([]any)
	require.NotEmpty(t, rules, "the policy admits the tailnet entry")
	require.Len(t, rules, 1, "one rule, and no other source reaches the listener")
	require.Equal(t, podRule(9020, tailnetPodLabels), rules[0])
}

// The budget lets the one listener pod go at a time, so a node drain or an Autopilot upgrade
// moves it rather than stalling on it, and selects exactly the Deployment's pods.
func TestClusteredNatsHubListenerBudget(t *testing.T) {
	r := runClusteredHub(t, clusteredArgs()).Named(t, podDisruptionBudgetType, clusteredListenerName)
	requireClusteredKubernetesChild(t, r)
	require.Equal(t, map[string]any{
		"apiVersion": "policy/v1", "kind": "PodDisruptionBudget",
		"metadata": map[string]any{"name": clusteredListenerName, "namespace": "envoy"},
		"spec":     map[string]any{"maxUnavailable": 1.0, "selector": map[string]any{"matchLabels": listenerPodLabels}},
	}, r.Inputs.Mappable())
}

// The listener container's environment in its own pod: its machine id on every address of that
// pod at port 9020, the Service's tailnet name first among the hosts it answers, the hub's client
// Service as its NATS URL, marked remote, its seed as a file from the operator's Secret, mounted
// read-only, and the flag that lets /v1 serve with no credential. No ENVOY_API_TOKEN: what keeps
// /v1 to its callers is the listener's NetworkPolicy.
func TestClusteredNatsHubListenerEnvironment(t *testing.T) {
	args := clusteredArgs()
	args.Tailnet.Listener.AllowedHosts = []string{"envoy.example.com"}
	template := deploymentPodTemplate(t, runClusteredHub(t, args), clusteredListenerName)
	listener := podContainer(t, template, "listener")

	env := containerEnv(t, listener)
	require.NotEmpty(t, env, "the container carries an environment")
	require.Equal(t, map[string]any{
		"PORT":                            "9020",
		"ENVOY_MACHINE_ID":                "envoy-hub",
		"ENVOY_LISTEN_HOST":               "0.0.0.0",
		"ENVOY_ALLOWED_HOSTS":             "envoy-hub.example-tailnet.ts.net,envoy.example.com",
		"NATS_URLS":                       "nats://envoy-nats.envoy.svc.cluster.local:4222",
		"ENVOY_ALLOW_REMOTE_NATS":         "1",
		"NATS_NKEY_SEED_FILE":             "/var/run/secrets/envoy/nats-seed",
		"ENVOY_API_ALLOW_UNAUTHENTICATED": "1",
	}, env)
	require.NotContains(t, env, "ENVOY_API_TOKEN")

	require.Equal(t, []any{map[string]any{"name": "nats-seed", "mountPath": "/var/run/secrets/envoy", "readOnly": true}}, listener["volumeMounts"])
	volumes := template["spec"].(map[string]any)["volumes"].([]any)
	require.NotEmpty(t, volumes)
	var seed any
	for _, v := range volumes {
		if v.(map[string]any)["name"] == "nats-seed" {
			seed = v
		}
	}
	require.Equal(t, map[string]any{"name": "nats-seed", "secret": map[string]any{
		"secretName": "envoy-hub-listener-seed", "items": []any{map[string]any{"key": "seed", "path": "nats-seed"}},
	}}, seed)
}

// The listener's three probes all read /healthz on its own loopback. Startup and liveness are
// status-blind, curl -sS of it: a listener answering 503 while NATS is down or its caches warm is
// alive and keeps its warm caches, and only one answering nothing at all is restarted, after two
// minutes of silence at startup (5 s × 24) or one minute later on (10 s × 6). Readiness is curl
// -fsS, which reads the HTTP status and never the body, so a pod still waiting for its durable,
// which /healthz answers 200 with a starting body, is ready and the rolling update never waits
// for the stream.
func TestClusteredNatsHubListenerProbes(t *testing.T) {
	template := deploymentPodTemplate(t, runClusteredHub(t, clusteredArgs()), clusteredListenerName)
	listener := podContainer(t, template, "listener")

	blind := []any{"curl", "-sS", "-o", "/dev/null", "--max-time", "6", "http://127.0.0.1:9020/healthz"}
	want := map[string]any{
		"startupProbe": map[string]any{
			"exec": map[string]any{"command": blind}, "periodSeconds": 5.0, "timeoutSeconds": 7.0, "failureThreshold": 24.0,
		},
		"livenessProbe": map[string]any{
			"exec": map[string]any{"command": blind}, "periodSeconds": 10.0, "timeoutSeconds": 7.0, "failureThreshold": 6.0,
		},
		"readinessProbe": map[string]any{
			"exec":          map[string]any{"command": []any{"curl", "-fsS", "http://127.0.0.1:9020/healthz"}},
			"periodSeconds": 5.0, "timeoutSeconds": 6.0, "failureThreshold": 2.0,
		},
	}
	read := 0
	for _, name := range []string{"startupProbe", "livenessProbe", "readinessProbe"} {
		probe, ok := listener[name]
		require.True(t, ok, "%s is rendered", name)
		require.NotNil(t, probe, name)
		read++
		require.Equal(t, want[name], probe, name)
	}
	require.Equal(t, 3, read, "the three probes were read")

	ready := listener["readinessProbe"].(map[string]any)["exec"].(map[string]any)["command"].([]any)
	require.Contains(t, ready, "-fsS", "readiness fails on a status curl -f refuses")
	require.Equal(t, []any{"curl", "-fsS", "http://127.0.0.1:9020/healthz"}, ready)
	require.Len(t, ready, 3)
	for _, word := range ready {
		require.NotContains(t, []string{"-o", "--output", "-w", "--write-out", "-d", "--data", "grep"}, word,
			"readiness reads the status alone, never the body")
	}
}

// The kubelet spends the listener's preStop hook and its shutdown on SIGTERM inside the pod's one
// grace period, and kills what is still running at its end: the 5 s hook, which holds the pod
// open while its removal from its Service's endpoints reaches every node's proxy, and the 28 s
// stop both fit in 35 s. The grace is also below the deadline a starting listener waits for the
// durable the stopping one holds, so the old pod is gone before the new one gives up.
func TestClusteredNatsHubListenerStopFitsItsGrace(t *testing.T) {
	template := deploymentPodTemplate(t, runClusteredHub(t, clusteredArgs()), clusteredListenerName)
	grace := template["spec"].(map[string]any)["terminationGracePeriodSeconds"].(float64)
	require.Equal(t, 35.0, grace)

	wait := preStopSeconds(t, podContainer(t, template, "listener"))
	require.Equal(t, 5.0, wait, "the endpoint removal's reach")
	require.Equal(t, 28, gcp.ClusteredHubListenerShutdownSeconds)
	require.LessOrEqual(t, wait+float64(gcp.ClusteredHubListenerShutdownSeconds), grace, "the listener's wait and its shutdown")
	// 135 is durableBindDeadline, the listener's own (packages/envoy/cmd/listener/durable.go:176),
	// which this package cannot import: it is a constant of package main.
	require.Less(t, grace, 135.0, "the stopping pod frees its durable before the starting one gives up")
}

// No workload of the hub adopts another's pods: the hub pods', the tailnet entry's and the
// listener's selectors each name a label the other two's pods do not wear, so a Deployment, a
// Service, a NetworkPolicy or a budget of one never selects a pod of another. Each ordered pair
// is compared, and the pairs are counted.
func TestClusteredNatsHubSelectorsSelectNoOtherWorkload(t *testing.T) {
	s, err := gcp.ResolveClusteredNatsHub(clusteredArgs())
	require.NoError(t, err)
	hub := map[string]any{}
	for key, value := range s.PodSelector {
		hub[key] = value
	}
	require.Equal(t, hubPodLabels, hub, "the hub's podSelector output is the selector the policy fences")

	selectors := []struct {
		name   string
		labels map[string]any
	}{
		{"the hub pods", hub},
		{"the tailnet entry's pods", tailnetPodLabels},
		{"the listener's pod", listenerPodLabels},
	}
	pairs := 0
	for i, a := range selectors {
		for j, b := range selectors {
			if i == j {
				continue
			}
			pairs++
			require.NotEmpty(t, a.labels, "%s has a selector", a.name)
			distinct := false
			for key, value := range a.labels {
				if other, ok := b.labels[key]; !ok || other != value {
					distinct = true
				}
			}
			require.True(t, distinct, "%s's selector selects %s", a.name, b.name)
		}
	}
	require.Equal(t, 6, pairs, "the three selectors were compared in both orders")
}

// Without tailnet there is no session listener to run: no object of the hub's is named
// <name>-listener. The registrations are read by name and counted, since the hub's own
// NetworkPolicy shares the listener's type and a count of none would prove nothing.
func TestClusteredNatsHubWithoutTailnetRegistersNoListener(t *testing.T) {
	m := runClusteredHub(t, withoutTailnet(clusteredArgs()))
	registrations := m.Registrations()
	require.NotEmpty(t, registrations, "the hub registers its own children")
	for _, r := range registrations {
		require.NotEqual(t, clusteredListenerName, r.Name, "%s %s", r.Type, r.Name)
	}
	require.ElementsMatch(t, clusteredHubBaseTypes, m.Types())
}
