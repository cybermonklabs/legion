package gcp_test

import (
	"encoding/json"
	"fmt"
	"maps"
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

// listenerCredentialProblem is what is wrong with a listener container's environment for a
// listener that requires a credential, "" when nothing is: it must carry the token check the
// resolver renders, ENVOY_OIDC_ISSUER, ENVOY_OIDC_AUDIENCE and ENVOY_OIDC_ACCOUNTS with the
// fixture's values and accounts, and none of ENVOY_API_ALLOW_UNAUTHENTICATED, which would let a
// listener with no bearer serve /v1 with no credential, ENVOY_ALLOWED_HOSTS, which a listener that
// requires a bearer never reads, or ENVOY_API_TOKEN, a shared bearer no caller is given.
func listenerCredentialProblem(env map[string]any, accounts string) string {
	for _, want := range [][2]string{
		{"ENVOY_OIDC_ISSUER", clusteredListenerIssuer},
		{"ENVOY_OIDC_AUDIENCE", clusteredListenerAudience},
		{"ENVOY_OIDC_ACCOUNTS", accounts},
	} {
		if env[want[0]] != want[1] {
			return fmt.Sprintf("%s is %v, want %q", want[0], env[want[0]], want[1])
		}
	}
	for _, name := range []string{"ENVOY_API_ALLOW_UNAUTHENTICATED", "ENVOY_ALLOWED_HOSTS", "ENVOY_API_TOKEN"} {
		if value, ok := env[name]; ok {
			return fmt.Sprintf("%s is rendered as %v", name, value)
		}
	}
	return ""
}

// withoutListenerDeployment is a clusteredRender with the listener's Deployment taken out, and how
// many lines that took out.
func withoutListenerDeployment(render string) (string, int) {
	var kept []string
	removed := 0
	for _, line := range strings.Split(render, "\n") {
		if strings.HasPrefix(line, "kubernetes:apps/v1:Deployment "+clusteredListenerName+" ") {
			removed++
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n"), removed
}

// A listener declared as the hub's own input beside the tailnet entry requires a credential, where
// the same listener declared as tailnet.listener serves the tailnet hosts with none. Every object
// the hub registers is the same but the listener's Deployment, and in that the container's
// environment differs by exactly the token check in place of the open flag and the hosts it
// serves with no credential. The controls feed the check an environment that forces the flag back
// in, and one that drops a part of the token check, and require it to see each.
func TestClusteredNatsHubListenerOutsideTheTailnetEntryRequiresACredential(t *testing.T) {
	insideHub := runClusteredHub(t, clusteredArgs())
	args := clusteredArgs()
	ownListener(args)
	require.Nil(t, args.Tailnet.Listener, "the listener is declared outside the tailnet entry alone")
	outsideHub := runClusteredHub(t, args)

	inside, insideRemoved := withoutListenerDeployment(clusteredRender(t, insideHub))
	outside, outsideRemoved := withoutListenerDeployment(clusteredRender(t, outsideHub))
	require.Equal(t, 1, insideRemoved)
	require.Equal(t, 1, outsideRemoved)
	require.Equal(t, inside, outside, "only the listener's Deployment differs")

	insideEnv := containerEnv(t, podContainer(t, deploymentPodTemplate(t, insideHub, clusteredListenerName), "listener"))
	outsideEnv := containerEnv(t, podContainer(t, deploymentPodTemplate(t, outsideHub, clusteredListenerName), "listener"))
	require.Empty(t, listenerCredentialProblem(outsideEnv, clusteredListenerAccount))
	require.Equal(t, "1", insideEnv["ENVOY_API_ALLOW_UNAUTHENTICATED"], "tailnet.listener keeps serving the tailnet hosts with no credential")
	require.Equal(t, "envoy-hub.example-tailnet.ts.net", insideEnv["ENVOY_ALLOWED_HOSTS"])
	for _, name := range []string{"ENVOY_OIDC_ISSUER", "ENVOY_OIDC_AUDIENCE", "ENVOY_OIDC_ACCOUNTS"} {
		require.NotContains(t, insideEnv, name)
	}
	shared := func(env map[string]any, drop ...string) map[string]any {
		kept := maps.Clone(env)
		for _, name := range drop {
			delete(kept, name)
		}
		return kept
	}
	require.Equal(t, shared(insideEnv, "ENVOY_API_ALLOW_UNAUTHENTICATED", "ENVOY_ALLOWED_HOSTS"),
		shared(outsideEnv, "ENVOY_OIDC_ISSUER", "ENVOY_OIDC_AUDIENCE", "ENVOY_OIDC_ACCOUNTS"))

	forced := maps.Clone(outsideEnv)
	forced["ENVOY_API_ALLOW_UNAUTHENTICATED"] = "1"
	require.Equal(t, "ENVOY_API_ALLOW_UNAUTHENTICATED is rendered as 1", listenerCredentialProblem(forced, clusteredListenerAccount))
	require.Contains(t, listenerCredentialProblem(shared(outsideEnv, "ENVOY_OIDC_ACCOUNTS"), clusteredListenerAccount), "ENVOY_OIDC_ACCOUNTS is <nil>")
}

// Without tailnet a hub's own listener runs as a workload of its own, reached inside the cluster
// alone: its ServiceAccount, Deployment, Service, NetworkPolicy and budget, and none of the
// tailnet hosts' objects. Its policy admits the pods of its client namespaces to port 9020 and
// nothing else, it requires each caller's token, and the hub admits it to NATS and renders its
// user; the hub's tailnet URLs stay null.
func TestClusteredNatsHubRunsItsOwnListenerWithoutTailnet(t *testing.T) {
	args := clusteredArgs()
	standaloneListener(args).ClientNamespaces = []string{"legion", "agents"}
	args.Listener.OidcAccounts = []string{clusteredListenerAccount, "system:serviceaccount:agents:envoy-client"}
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
	require.Equal(t, "nats://envoy-nats.envoy.svc.cluster.local:4222", env["NATS_URLS"])
	require.Empty(t, listenerCredentialProblem(env, clusteredListenerAccount+",system:serviceaccount:agents:envoy-client"))

	hubRules := m.Named(t, networkPolicyType, "envoy-nats").Inputs.Mappable()["spec"].(map[string]any)["ingress"].([]any)
	require.Contains(t, hubRules, podRule(4222, listenerPodLabels), "the listener reaches NATS")
	require.NotContains(t, hubRules, podRule(4222, tailnetPodLabels), "no tailnet host exists to admit")

	s, err := gcp.ResolveClusteredNatsHub(args)
	require.NoError(t, err)
	require.Contains(t, s.AuthConf, keyListener, "the hub renders the listener's user")
	require.Nil(t, s.Tailnet)
}

// A tailnet.listener given client namespaces serves both, and so requires a credential of every
// caller, the tailnet hosts' included: its policy takes the tailnet hosts' rule first, then the
// namespaces', and its container renders the token check and no open flag. Without client
// namespaces it renders what it always rendered (TestClusteredNatsHubListenerNetworkPolicy and
// TestClusteredNatsHubListenerEnvironment).
func TestClusteredNatsHubTailnetListenerWithClientNamespacesRequiresACredential(t *testing.T) {
	args := clusteredArgs()
	withCredential(listener(args)).ClientNamespaces = []string{"legion"}
	m := runClusteredHub(t, args)

	rules := m.Named(t, networkPolicyType, clusteredListenerName).Inputs.Mappable()["spec"].(map[string]any)["ingress"].([]any)
	require.Equal(t, []any{podRule(9020, tailnetPodLabels), namespaceRule(9020, "legion")}, rules)
	env := containerEnv(t, podContainer(t, deploymentPodTemplate(t, m, clusteredListenerName), "listener"))
	require.Empty(t, listenerCredentialProblem(env, clusteredListenerAccount))
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
// path does: no tailnet object, the listener's token check, and no tailnet URL.
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
		"oidcIssuer":       property.New(clusteredListenerIssuer),
		"oidcAudience":     property.New(clusteredListenerAudience),
		"oidcAccounts":     stringArray(clusteredListenerAccount),
	}))
	m, resp, err := constructClustered(t, inputs, false)
	require.NoError(t, err)
	requireClusteredOutputs(t, resp, false)
	env := containerEnv(t, podContainer(t, deploymentPodTemplate(t, m, clusteredListenerName), "listener"))
	require.Empty(t, listenerCredentialProblem(env, clusteredListenerAccount))
	for _, r := range m.Registrations() {
		require.NotContains(t, r.Name, "tailnet", "%s %s", r.Type, r.Name)
	}
}
