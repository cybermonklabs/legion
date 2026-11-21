package gcp_test

import (
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
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
// host beside it. It prints how many objects the hub rendered and how many containers that pod
// holds, so a reader of the run knows the set the golden was compared over was not empty.
func TestClusteredNatsHubListenerPodMatchesGolden(t *testing.T) {
	m := runClusteredHub(t, clusteredArgs())
	requirePodGolden(t, m, clusteredListenerName, "clustered-hub-listener-pod.golden")
	containers := clusteredListenerPodSpec(t, m)["containers"].([]any)
	t.Logf("%d objects rendered, %d container(s) in the listener pod", len(m.Registrations()), len(containers))
}

// The Deployment runs one listener, wearing the listener's selector labels, in the hub's
// namespace; it surges the new pod in before it stops the old one, so the durable the old pod
// holds is free by the time the new one has waited its bind deadline out; the pod has 40 s to
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
	require.Equal(t, 40.0, pod["terminationGracePeriodSeconds"])
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
// pod at port 9020, the hub's client Service as its NATS URL, marked remote, its broker settings,
// the credential every session presents (t-e-19), with the introspection limit every tailnet
// session's host shares and the projected token of its own account mounted read-only, and its seed
// as a file from the operator's Secret, mounted read-only. No
// ENVOY_API_TOKEN, no ENVOY_API_ALLOW_UNAUTHENTICATED and no ENVOY_ALLOWED_HOSTS: off loopback the
// listener refuses the first two and reads no third, and a request that presents nothing gets 401.
func TestClusteredNatsHubListenerEnvironment(t *testing.T) {
	template := deploymentPodTemplate(t, runClusteredHub(t, clusteredArgs()), clusteredListenerName)
	listener := podContainer(t, template, "listener")

	env := containerEnv(t, listener)
	require.Equal(t, map[string]any{
		"PORT":                                   "9020",
		"ENVOY_MACHINE_ID":                       "envoy-hub",
		"ENVOY_LISTEN_HOST":                      "0.0.0.0",
		"NATS_URLS":                              "nats://envoy-nats.envoy.svc.cluster.local:4222",
		"ENVOY_ALLOW_REMOTE_NATS":                "1",
		"ENVOY_AGENT_SECRETS_URL":                clusteredBrokerURL,
		"ENVOY_AGENT_SECRETS_ACCOUNT_TOKEN_FILE": "/var/run/secrets/envoy-broker/token",
		"ENVOY_PUBLIC_URL":                       clusteredListenerOrigin,
		"ENVOY_AGENT_SECRETS_INTROSPECTIONS_PER_SECOND": "50",
		"ENVOY_AGENT_SECRETS_INTROSPECTION_BURST":       "200",
		"NATS_NKEY_SEED_FILE":                           "/var/run/secrets/envoy/nats-seed",
	}, env)

	require.Equal(t, []any{
		map[string]any{"name": "nats-seed", "mountPath": "/var/run/secrets/envoy", "readOnly": true},
		map[string]any{"name": "envoy-broker-token", "mountPath": "/var/run/secrets/envoy-broker", "readOnly": true},
	}, listener["volumeMounts"])
	volumes := template["spec"].(map[string]any)["volumes"].([]any)
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

// The listener's three probes, each on its own loopback. Startup is /bin/sh -c of a scrape of
// /metrics, which passes only once the scrape holds the sample line of envoy_active_interests,
// which the listener writes only once its /v1 gate is open and its interest list can be served.
// The kubelet keeps the pod unready until then, which keeps the old pod serving through the
// rolling update until the new one's /v1 serves, and it restarts a listener that has not opened
// /v1 within ten minutes (10 s × 60). Readiness and liveness are status-blind, curl -sS of /livez,
// which the listener answers 204 without touching the bus: a fault of the hub, which every pod
// shares, neither takes the one pod out of its Service nor restarts it, and only a listener that
// answers nothing at all is taken out, after 30 s of silence (10 s × 3), or restarted, after one
// minute (10 s × 6). TestClusteredNatsHubListenerStartupProbePassesOnlyOnTheSample runs the
// startup script.
func TestClusteredNatsHubListenerProbes(t *testing.T) {
	template := deploymentPodTemplate(t, runClusteredHub(t, clusteredArgs()), clusteredListenerName)
	listener := podContainer(t, template, "listener")

	blind := []any{"curl", "-sS", "-o", "/dev/null", "--max-time", "8", "http://127.0.0.1:9020/livez"}
	served := []any{"/bin/sh", "-c", "curl -fsS --max-time 5 http://127.0.0.1:9020/metrics | grep -q '^envoy_active_interests '"}
	want := map[string]any{
		"startupProbe": map[string]any{
			"exec": map[string]any{"command": served}, "periodSeconds": 10.0, "timeoutSeconds": 6.0, "failureThreshold": 60.0,
		},
		"livenessProbe": map[string]any{
			"exec": map[string]any{"command": blind}, "periodSeconds": 10.0, "timeoutSeconds": 9.0, "failureThreshold": 6.0,
		},
		"readinessProbe": map[string]any{
			"exec": map[string]any{"command": blind}, "periodSeconds": 10.0, "timeoutSeconds": 9.0, "failureThreshold": 3.0,
		},
	}
	read := 0
	for _, name := range []string{"startupProbe", "livenessProbe", "readinessProbe"} {
		probe, ok := listener[name]
		require.True(t, ok, "%s is rendered", name)
		require.NotNil(t, probe, name)
		require.Equal(t, want[name], probe, name)
		read++
	}
	t.Logf("%d probe(s) examined on the listener container", read)
}

// The kubelet spends the listener's preStop hook and its shutdown on SIGTERM inside the pod's one
// grace period, and kills what is still running at its end: the 5 s hook, which holds the pod
// open while its removal from its Service's endpoints reaches every node's proxy, and the 28 s
// stop both fit in 40 s. The grace is also under the bind deadline a starting listener waits the
// durable out for, which is necessary and not sufficient: that deadline counts from the new pod's
// /v1 open, right after which its listener starts the bind, so what has to fit under it is the new
// pod's startup probe's lag behind that open, at most one 10 s period, plus this grace.
func TestClusteredNatsHubListenerStopFitsItsGrace(t *testing.T) {
	template := deploymentPodTemplate(t, runClusteredHub(t, clusteredArgs()), clusteredListenerName)
	grace := template["spec"].(map[string]any)["terminationGracePeriodSeconds"].(float64)
	require.Equal(t, float64(gcp.ClusteredHubListenerGraceSeconds), grace, "the pod renders the grace the package holds its stop within")

	wait := preStopSeconds(t, podContainer(t, template, "listener"))
	require.Equal(t, 5.0, wait, "the endpoint removal's reach")
	require.Equal(t, 28, gcp.ClusteredHubListenerShutdownSeconds)
	require.LessOrEqual(t, wait+float64(gcp.ClusteredHubListenerShutdownSeconds), grace, "the listener's wait and its shutdown")
	require.Less(t, grace, listenerDurableBindDeadlineSeconds, "the grace stays under the deadline a starting listener waits the durable out for")
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

// Without tailnet and without a listener of the hub's own there is no session listener to run: no
// object of the hub's is named <name>-listener. The registrations are read by name and counted,
// since the hub's own NetworkPolicy shares the listener's type and a count of none would prove
// nothing.
func TestClusteredNatsHubWithoutTailnetRegistersNoListener(t *testing.T) {
	m := runClusteredHub(t, withoutTailnet(clusteredArgs()))
	registrations := m.Registrations()
	require.NotEmpty(t, registrations, "the hub registers its own children")
	for _, r := range registrations {
		require.NotEqual(t, clusteredListenerName, r.Name, "%s %s", r.Type, r.Name)
	}
	require.ElementsMatch(t, clusteredHubBaseTypes, m.Types())
}

// A session listener binds off loopback, where on an all-public hub its one credential is its
// broker check (t-e-19): listener args that leave agentSecretsUrl unset would render a listener with
// no credential, which refuses to start and crash-loops, so the hub refuses them at preview, naming
// the input, before anything registers, wherever the listener is declared and whatever
// privateProjects says. Negative control, run by hand: today's render, which renders that listener
// without broker settings; the test fails naming the missing refusal.
func TestTheClusteredHubRefusesASessionListenerWithoutTheBroker(t *testing.T) {
	for _, row := range []struct {
		name, input string
		args        func() *gcp.ClusteredNatsHubArgs
	}{
		{"an all-public hub's tailnet.listener", "tailnet.listener.agentSecretsUrl", func() *gcp.ClusteredNatsHubArgs {
			a := clusteredArgs()
			a.Tailnet.Listener.AgentSecretsUrl = ""
			return a
		}},
		{"an all-public hub's own listener", "listener.agentSecretsUrl", func() *gcp.ClusteredNatsHubArgs {
			a := clusteredArgs()
			ownListener(a).AgentSecretsUrl = ""
			return a
		}},
		{"a private hub's listener", "listener.agentSecretsUrl", func() *gcp.ClusteredNatsHubArgs {
			a := privateListenerHub()
			a.Listener.AgentSecretsUrl = ""
			return a
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			_, err := gcp.ResolveClusteredNatsHub(row.args())
			require.ErrorContains(t, err, row.input+" is required", "the hub does not refuse the listener naming %s", row.input)
			var input *core.InputError
			require.ErrorAs(t, err, &input)
			require.Equal(t, row.input, input.Input)
			m := newMocks()
			m.Preview = true
			err = m.Run(func(ctx *pulumi.Context) error {
				_, err := gcp.NewClusteredNatsHub(ctx, "example-clustered-hub", row.args())
				return err
			})
			require.ErrorContains(t, err, row.input+" is required")
			require.Empty(t, m.Registrations(), "nothing registered")
		})
	}
}
