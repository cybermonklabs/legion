package gcp_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/infra/internal/pulumitest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// clusteredTailnetName is clusteredArgs' tailnet hosts' name, <name>-tailnet: their ConfigMap's,
// ServiceAccount's, Deployment's and budget's.
const clusteredTailnetName = "envoy-nats-tailnet"

// clusteredChildURN is the URN of the hub's child of type typ named name.
func clusteredChildURN(typ, name string) string {
	return "urn:pulumi:s::p::" + clusteredHubType + "$" + typ + "::" + name
}

// clusteredTailnetSpec is the tailnet hosts' Deployment spec, as the mocks recorded it.
func clusteredTailnetSpec(t *testing.T, m *pulumitest.Mocks) map[string]any {
	t.Helper()
	return m.Named(t, deploymentType, clusteredTailnetName).Inputs.Mappable()["spec"].(map[string]any)
}

// clusteredTailnetPodSpec is the tailnet hosts' pod spec.
func clusteredTailnetPodSpec(t *testing.T, m *pulumitest.Mocks) map[string]any {
	t.Helper()
	return deploymentPodTemplate(t, m, clusteredTailnetName)["spec"].(map[string]any)
}

// The tailnet host's pod, whole: the init container creating tailscaled's state directory, the
// tailscale container serving svc:envoy-hub and the advertiser deciding when it does, both as
// the one uid, with the state volume and the serve config. The session listener is a workload of
// its own now (TestClusteredNatsHubListenerPodMatchesGolden), and the golden is what pins the
// absence of its container and of its seed volume: it is the whole pod template, so a listener
// container or a nats-seed volume back in this pod would not match it. It prints how many
// objects the hub rendered and how many containers that pod holds, so a reader of the run knows
// the set the golden was compared over was not empty.
func TestClusteredNatsHubTailnetPodMatchesGolden(t *testing.T) {
	m := runClusteredHub(t, clusteredArgs())
	requirePodGolden(t, m, clusteredTailnetName, "clustered-hub-tailnet-pod.golden")
	pod := clusteredTailnetPodSpec(t, m)
	t.Logf("%d objects rendered, %d container(s) and %d init container(s) in the tailnet pod",
		len(m.Registrations()), len(pod["containers"].([]any)), len(pod["initContainers"].([]any)))
}

// The serve config forwards the Service's NATS port to the hub's client Service and its listener
// port to the session listener's own cluster Service, as plain TCP, from ConfigMap <name>-tailnet
// in the hub's namespace.
func TestClusteredNatsHubTailnetServeConfigMatchesGolden(t *testing.T) {
	r := runClusteredHub(t, clusteredArgs()).Named(t, configMapType, clusteredTailnetName)
	requireClusteredKubernetesChild(t, r)
	inputs := r.Inputs.Mappable()
	require.Equal(t, map[string]any{"name": clusteredTailnetName, "namespace": "envoy"}, inputs["metadata"])
	require.Equal(t, map[string]any{"serve.json": readGolden(t, "clustered-hub-serve.golden")}, inputs["data"])
}

// The Deployment runs tailnet.replicas hosts, two by default, wearing the tailnet hosts' selector
// labels; it surges a new host before it stops an old one; each pod has 60 s to stop; and hosts
// spread hard over zones, with minDomains the smaller of the replica count and three, and over
// nodes.
func TestClusteredNatsHubTailnetDeployment(t *testing.T) {
	for _, row := range []struct {
		replicas            *int
		want, wantMinDomain float64
	}{
		{nil, 2, 2},
		{pulumi.IntRef(3), 3, 3},
		{pulumi.IntRef(5), 5, 3},
	} {
		t.Run(strconv.Itoa(int(row.want)), func(t *testing.T) {
			args := clusteredArgs()
			args.Tailnet.Replicas = row.replicas
			m := runClusteredHub(t, args)
			r := m.Named(t, deploymentType, clusteredTailnetName)
			requireClusteredKubernetesChild(t, r)
			require.Equal(t, map[string]any{"name": clusteredTailnetName, "namespace": "envoy"}, r.Inputs.Mappable()["metadata"])
			spec := clusteredTailnetSpec(t, m)
			require.Equal(t, row.want, spec["replicas"])
			require.Equal(t, map[string]any{
				"type": "RollingUpdate", "rollingUpdate": map[string]any{"maxUnavailable": 0.0, "maxSurge": 1.0},
			}, spec["strategy"])
			require.Equal(t, map[string]any{"matchLabels": tailnetPodLabels}, spec["selector"])
			require.Equal(t, map[string]any{"labels": tailnetPodLabels}, spec["template"].(map[string]any)["metadata"])

			pod := clusteredTailnetPodSpec(t, m)
			require.Equal(t, 60.0, pod["terminationGracePeriodSeconds"])
			require.Equal(t, clusteredTailnetName, pod["serviceAccountName"])
			require.NotContains(t, pod, "priorityClassName")
			selector := map[string]any{"matchLabels": tailnetPodLabels}
			require.Equal(t, []any{
				map[string]any{
					"topologyKey": "topology.kubernetes.io/zone", "maxSkew": 1.0, "whenUnsatisfiable": "DoNotSchedule",
					"minDomains": row.wantMinDomain, "labelSelector": selector,
				},
				map[string]any{
					"topologyKey": "kubernetes.io/hostname", "maxSkew": 1.0, "whenUnsatisfiable": "DoNotSchedule",
					"labelSelector": selector,
				},
			}, pod["topologySpreadConstraints"])
		})
	}
}

// preStopSeconds is a container's preStop hook's sleep: the hook must be `sleep <seconds>`.
func preStopSeconds(t *testing.T, container map[string]any) float64 {
	t.Helper()
	command := container["lifecycle"].(map[string]any)["preStop"].(map[string]any)["exec"].(map[string]any)["command"].([]any)
	require.Len(t, command, 2)
	require.Equal(t, "sleep", command[0])
	seconds, err := strconv.Atoi(command[1].(string))
	require.NoError(t, err)
	return float64(seconds)
}

// The kubelet spends the tailscale container's preStop hook and its shutdown on SIGTERM inside
// the one grace period, and kills what is still running at its end. The tailnet host pod now
// holds the tailscale side alone: its hook (drain 5 s, wait 20 s, logout 5 s) and containerboot's
// 25 s shutdown fit, and the session listener, whose own stop is held within its own pod's grace
// (TestClusteredNatsHubListenerStopFitsItsGrace), no longer spends any of this one.
func TestClusteredNatsHubTailnetShutdownFitsTheGrace(t *testing.T) {
	m := runClusteredHub(t, clusteredArgs())
	template := deploymentPodTemplate(t, m, clusteredTailnetName)
	grace := template["spec"].(map[string]any)["terminationGracePeriodSeconds"].(float64)

	require.Equal(t, 60.0, grace)
	require.Equal(t, 55, gcp.TailnetShutdownSeconds)
	require.LessOrEqual(t, float64(gcp.TailnetShutdownSeconds), grace, "the tailscale container's hook and containerboot's shutdown")
}

// The tailscale container joins with the component's auth key and every tag, and never advertises
// the Service by itself: the advertiser does, on the host's own readiness alone. The session
// listener runs in a pod of its own, and no advertiser can reach that pod's loopback, so a
// listener fault no longer takes NATS off the tailnet.
func TestClusteredNatsHubTailnetHostJoinsWithTheTagsAndAdvertisesWithNoHealthGate(t *testing.T) {
	args := clusteredArgs()
	args.Tailnet.Tags = []string{"tag:envoy-hub-host", "tag:example"}
	template := deploymentPodTemplate(t, runClusteredHub(t, args), clusteredTailnetName)
	env := containerEnv(t, podContainer(t, template, "tailscale"))
	require.Equal(t, "--advertise-tags=tag:envoy-hub-host,tag:example", env["TS_EXTRA_ARGS"])
	require.Equal(t, "false", env["TS_EXPERIMENTAL_SERVICE_AUTO_ADVERTISEMENT"])
	require.Equal(t, map[string]any{"name": "envoy-nats-tailnet-authkey", "key": "TS_AUTHKEY"}, env["TS_AUTHKEY"])
	require.Equal(t, "/etc/tailscale/serve.json", env["TS_SERVE_CONFIG"])

	command := podContainer(t, template, "advertiser")["command"].([]any)
	require.Equal(t, []any{"/bin/sh", "-c"}, command[:2])
	require.Contains(t, command[2], "\nservice=svc:envoy-hub\n")
	require.Contains(t, command[2], "\ngrace=60\n")
	t.Logf("%d host container(s) examined in the tailnet pod",
		len(template["spec"].(map[string]any)["containers"].([]any)))
}

// The budget lets one tailnet host go at a time, and selects exactly the Deployment's pods.
func TestClusteredNatsHubTailnetBudget(t *testing.T) {
	r := runClusteredHub(t, clusteredArgs()).Named(t, podDisruptionBudgetType, clusteredTailnetName)
	requireClusteredKubernetesChild(t, r)
	require.Equal(t, map[string]any{
		"apiVersion": "policy/v1", "kind": "PodDisruptionBudget",
		"metadata": map[string]any{"name": clusteredTailnetName, "namespace": "envoy"},
		"spec":     map[string]any{"maxUnavailable": 1.0, "selector": map[string]any{"matchLabels": tailnetPodLabels}},
	}, r.Inputs.Mappable())
}

// The auth key reaches its Secret as a secret, under the key TS_AUTHKEY the tailscale container
// names; the ServiceAccount mounts no token, since no container calls the Kubernetes API; the
// Deployment waits on both and on the serve config.
func TestClusteredNatsHubTailnetAuthKeyAndServiceAccount(t *testing.T) {
	m := runClusteredHub(t, clusteredArgs())
	secret := m.Named(t, kubeSecretType, "envoy-nats-tailnet-authkey")
	requireClusteredKubernetesChild(t, secret)
	require.Equal(t, map[string]any{"name": "envoy-nats-tailnet-authkey", "namespace": "envoy"}, secret.Inputs.Mappable()["metadata"])
	data, isSecret := pulumitest.Plain(secret.Inputs["stringData"])
	require.True(t, isSecret, "the auth key stays secret")
	require.Equal(t, map[string]any{"TS_AUTHKEY": authKey}, normalizedProperty(data))

	account := m.Named(t, serviceAccountType, clusteredTailnetName)
	requireClusteredKubernetesChild(t, account)
	require.Equal(t, map[string]any{
		"apiVersion": "v1", "kind": "ServiceAccount",
		"metadata":                     map[string]any{"name": clusteredTailnetName, "namespace": "envoy"},
		"automountServiceAccountToken": false,
	}, account.Inputs.Mappable())

	require.Subset(t, m.Named(t, deploymentType, clusteredTailnetName).RPC.GetDependencies(), []string{
		clusteredChildURN(kubeSecretType, "envoy-nats-tailnet-authkey"),
		clusteredChildURN(configMapType, clusteredTailnetName),
		clusteredChildURN(serviceAccountType, clusteredTailnetName),
	})
}

// priorityClassName reaches the tailnet hosts' pods too, and their Deployment waits on the class's
// read, as the chart does.
func TestClusteredNatsHubTailnetTakesThePriorityClass(t *testing.T) {
	args := clusteredArgs()
	args.PriorityClassName = pulumi.StringRef("envoy-critical")
	m := runClusteredHub(t, args)
	require.Equal(t, "envoy-critical", clusteredTailnetPodSpec(t, m)["priorityClassName"])
	require.Contains(t, m.Named(t, deploymentType, clusteredTailnetName).RPC.GetDependencies(),
		clusteredChildURN(priorityClassType, "envoy-nats-priority-class"))
}

// Without tailnet the hub registers none of the tailnet hosts' children.
func TestClusteredNatsHubWithoutTailnetRegistersNoTailnetHost(t *testing.T) {
	m := runClusteredHub(t, withoutTailnet(clusteredArgs()))
	for _, typ := range clusteredHubTailnetTypes {
		require.Zero(t, countRegistrations(m, typ), typ)
	}
	require.ElementsMatch(t, clusteredHubBaseTypes, m.Types())
}

// The Secret is what reads tailnetAuthKey: a blank key and one holding an nkey seed, each another
// resource's secret output sent through Construct, fail the update naming tailnetAuthKey and never
// the value; neither the Secret nor the tailnet hosts' Deployment, which waits on it, registers,
// and no registration carries the value. The session listener's Deployment, which takes no auth
// key, is another workload's, so the hosts' absence is read by name over every registration,
// counted.
func TestConstructRefusesABlankOrSeededTailnetAuthKey(t *testing.T) {
	seed, _ := testnats.User(t)
	for _, row := range []struct{ name, value, want string }{
		{"blank", " \n\t", "tailnetAuthKey is blank, which no credential is"},
		{"a seed", strings.ToLower(seed), "tailnetAuthKey holds an nkey seed (not echoed)"},
	} {
		t.Run(row.name, func(t *testing.T) {
			inputs := clusteredPlainInputs()
			inputs["kubeconfig"] = property.New(kubeconfig).WithSecret(true)
			inputs["tailnetAuthKey"] = property.New(row.value).WithSecret(true).WithDependencies([]resource.URN{upstream})
			m, _, err := constructClustered(t, inputs, false)
			require.ErrorContains(t, err, clusteredHubType+" example-clustered-hub: "+row.want)
			require.NotContains(t, strings.ToUpper(err.Error()), seed)
			require.NotContains(t, m.Types(), kubeSecretType, "no Secret holds it")
			registrations := m.Registrations()
			require.NotEmpty(t, registrations, "the hub registers its other children")
			for _, r := range registrations {
				require.False(t, r.Type == deploymentType && r.Name == clusteredTailnetName, "no host joins with it")
				for _, s := range allStrings(resource.NewProperty(r.Inputs)) {
					require.NotContains(t, strings.ToUpper(s), seed, "%s %s holds the seed", r.Type, r.Name)
				}
			}
		})
	}
}
