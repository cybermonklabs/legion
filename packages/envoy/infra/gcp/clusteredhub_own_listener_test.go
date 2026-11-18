package gcp_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/property"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/infra/internal/pulumitest"
)

// clusteredRender is everything a hub registered, one line per registration, sorted: its type,
// name, parent, provider, dependencies and property dependencies, each sorted, and its inputs as
// JSON with sorted keys. Two renders are equal exactly when the two hubs registered the same
// resources with the same inputs, whatever order the SDK's goroutines registered them in.
func clusteredRender(t *testing.T, m *pulumitest.Mocks) string {
	t.Helper()
	var lines []string
	for _, r := range m.Registrations() {
		if r.RPC == nil {
			lines = append(lines, fmt.Sprintf("read %s %s id=%s", r.Type, r.Name, r.ID))
			continue
		}
		deps := slices.Clone(r.RPC.GetDependencies())
		slices.Sort(deps)
		propertyDeps := map[string][]string{}
		for input, urns := range r.RPC.GetPropertyDependencies() {
			sorted := slices.Clone(urns.GetUrns())
			slices.Sort(sorted)
			propertyDeps[input] = sorted
		}
		inputs, err := json.Marshal(r.RPC.GetObject().AsMap())
		require.NoError(t, err)
		propertyDepsJSON, err := json.Marshal(propertyDeps)
		require.NoError(t, err)
		lines = append(lines, fmt.Sprintf("%s %s parent=%s provider=%s deps=%v propertyDeps=%s inputs=%s",
			r.Type, r.Name, r.RPC.GetParent(), r.RPC.GetProvider(), deps, propertyDepsJSON, inputs))
	}
	require.NotEmpty(t, lines, "the hub registered its children")
	slices.Sort(lines)
	return strings.Join(lines, "\n")
}

// A listener declared as the hub's own input beside the tailnet entry is the listener
// tailnet.listener declares: the hub registers the same resources with the same inputs, so a
// consumer moving it out of the tailnet entry changes nothing deployed. The control renders the
// same move with the listener's image changed and requires a difference, so the comparison is
// shown able to see one.
func TestClusteredNatsHubListenerOutsideTheTailnetEntryRendersTheSame(t *testing.T) {
	inside := clusteredRender(t, runClusteredHub(t, clusteredArgs()))

	args := clusteredArgs()
	ownListener(args)
	require.Nil(t, args.Tailnet.Listener, "the listener is declared outside the tailnet entry alone")
	require.Equal(t, inside, clusteredRender(t, runClusteredHub(t, args)))

	control := clusteredArgs()
	ownListener(control).Image = "example.invalid/envoy@sha256:" + strings.Repeat("e", 64)
	require.NotEqual(t, inside, clusteredRender(t, runClusteredHub(t, control)), "the comparison sees one changed input")
}

// Without tailnet a hub's own listener runs as a workload of its own, reached inside the cluster
// alone: its ServiceAccount, Deployment, Service, NetworkPolicy and budget, and none of the
// tailnet hosts' objects. Its policy admits the pods of its client namespaces to port 9020 and
// nothing else, it answers at its cluster Service's name, and the hub admits it to NATS and
// renders its user; the hub's tailnet URLs stay null.
func TestClusteredNatsHubRunsItsOwnListenerWithoutTailnet(t *testing.T) {
	args := clusteredArgs()
	standaloneListener(args).ClientNamespaces = []string{"legion", "agents"}
	var tailnetNatsURL, tailnetEnvoyURL any
	m, err := runClustered(t, "example-clustered-hub", args, func(h *gcp.ClusteredNatsHub) {
		tailnetNatsURL, tailnetEnvoyURL = awaitAny(t, h.TailnetNatsUrl), awaitAny(t, h.TailnetEnvoyUrl)
	})
	require.NoError(t, err)
	require.ElementsMatch(t, slices.Concat(clusteredHubBaseTypes, clusteredHubListenerTypes), m.Types())
	require.Equal(t, (*string)(nil), tailnetNatsURL)
	require.Equal(t, (*string)(nil), tailnetEnvoyURL)

	rules := m.Named(t, networkPolicyType, clusteredListenerName).Inputs.Mappable()["spec"].(map[string]any)["ingress"].([]any)
	require.Equal(t, []any{namespaceRule(9020, "legion", "agents")}, rules, "the client namespaces alone reach the listener")

	env := containerEnv(t, podContainer(t, deploymentPodTemplate(t, m, clusteredListenerName), "listener"))
	require.Equal(t, "envoy-hub", env["ENVOY_MACHINE_ID"])
	require.Equal(t, "envoy-nats-listener.envoy.svc.cluster.local", env["ENVOY_ALLOWED_HOSTS"])
	require.Equal(t, "nats://envoy-nats.envoy.svc.cluster.local:4222", env["NATS_URLS"])
	require.Equal(t, "1", env["ENVOY_API_ALLOW_UNAUTHENTICATED"])

	hubRules := m.Named(t, networkPolicyType, "envoy-nats").Inputs.Mappable()["spec"].(map[string]any)["ingress"].([]any)
	require.Contains(t, hubRules, podRule(4222, listenerPodLabels), "the listener reaches NATS")
	require.NotContains(t, hubRules, podRule(4222, tailnetPodLabels), "no tailnet host exists to admit")

	s, err := gcp.ResolveClusteredNatsHub(args)
	require.NoError(t, err)
	require.Contains(t, s.AuthConf, keyListener, "the hub renders the listener's user")
	require.Nil(t, s.Tailnet)
}

// With tailnet and client namespaces the listener serves both: the tailnet hosts' rule first,
// then the namespaces', and its tailnet name first, then its cluster Service's name, then the
// operator's own hosts. Without client namespaces it renders what it always rendered
// (TestClusteredNatsHubListenerNetworkPolicy and TestClusteredNatsHubListenerEnvironment).
func TestClusteredNatsHubListenerServesClientNamespacesBesideTheTailnet(t *testing.T) {
	args := clusteredArgs()
	listener(args).ClientNamespaces = []string{"legion"}
	listener(args).AllowedHosts = []string{"envoy.example.com"}
	m := runClusteredHub(t, args)

	rules := m.Named(t, networkPolicyType, clusteredListenerName).Inputs.Mappable()["spec"].(map[string]any)["ingress"].([]any)
	require.Equal(t, []any{podRule(9020, tailnetPodLabels), namespaceRule(9020, "legion")}, rules)
	env := containerEnv(t, podContainer(t, deploymentPodTemplate(t, m, clusteredListenerName), "listener"))
	require.Equal(t, "envoy-hub.example-tailnet.ts.net,envoy-nats-listener.envoy.svc.cluster.local,envoy.example.com", env["ENVOY_ALLOWED_HOSTS"])
}

// natsReplicas is the session listener's, so a hub without tailnet takes it beside its own
// listener and renders it there, as it refuses it with no listener at all
// (TestClusteredNatsHubResolverRefusesInvalidInputs).
func TestClusteredNatsHubTakesNatsReplicasBesideItsOwnListener(t *testing.T) {
	args := clusteredArgs()
	standaloneListener(args)
	args.NatsReplicas = pulumi.IntRef(3)
	s, err := gcp.ResolveClusteredNatsHub(args)
	require.NoError(t, err)
	require.Contains(t, s.Listener.Env, "ENVOY_NATS_REPLICAS=3")
}

// The hub's own listener reaches the plugin over the wire a consumer's SDK uses, as an object
// input of its own beside no tailnet entry, and Construct decodes and renders it as the direct
// path does: no tailnet object, the listener at its cluster Service's name, and no tailnet URL.
func TestConstructClusteredHubWithItsOwnListener(t *testing.T) {
	inputs := clusteredPlainInputs()
	delete(inputs, "tailnet")
	inputs["kubeconfig"] = property.New(kubeconfig).WithSecret(true)
	inputs["listener"] = property.New(property.NewMap(map[string]property.Value{
		"image":            property.New(listenerImage),
		"machineId":        property.New("envoy-hub"),
		"nkey":             property.New(keyListener),
		"seedSecretName":   property.New("envoy-hub-listener-seed"),
		"seedSecretKey":    property.New("seed"),
		"clientNamespaces": stringArray("legion"),
	}))
	m, resp, err := constructClustered(t, inputs, false)
	require.NoError(t, err)
	requireClusteredOutputs(t, resp, false)
	env := containerEnv(t, podContainer(t, deploymentPodTemplate(t, m, clusteredListenerName), "listener"))
	require.Equal(t, "envoy-nats-listener.envoy.svc.cluster.local", env["ENVOY_ALLOWED_HOSTS"])
	for _, r := range m.Registrations() {
		require.NotContains(t, r.Name, "tailnet", "%s %s", r.Type, r.Name)
	}
}
