package gcp_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/infra/internal/pulumitest"
)

// dispatchClusterOnlyArgs are every input the cluster-only mode takes: clusterOnly itself and the
// region and hostname that name the cloud resources. deletionProtection and regionalNetwork are the
// fourth and fifth inputs the mode keeps live and are left to their defaults here; every other
// input is refused by name (TestDispatchClusterOnlyRefusesEveryDispatchInput).
func dispatchClusterOnlyArgs() *gcp.DispatchArgs {
	return &gcp.DispatchArgs{Region: "us-central1", Hostname: "example-dispatch", ClusterOnly: pulumi.BoolRef(true)}
}

// runDispatchClusterOnly runs args through the mock engine with a gcp provider and no random one,
// as a cluster-only consumer's program declares it: the mode registers no RandomPassword, so it
// requires no random provider.
func runDispatchClusterOnly(t *testing.T, args *gcp.DispatchArgs) (*pulumitest.Mocks, error) {
	t.Helper()
	return runDispatchWithProviders(t, "example-dispatch", args, newGCPProvider)
}

// dispatchClusterOnlyTypes are the resources the mode registers: the VPC, its subnetwork, the
// router and Cloud NAT the private nodes reach the internet through, and the Autopilot cluster.
var dispatchClusterOnlyTypes = []string{
	"gcp:compute/network:Network",
	"gcp:compute/subnetwork:Subnetwork",
	"gcp:compute/router:Router",
	"gcp:compute/routerNat:RouterNat",
	"gcp:container/cluster:Cluster",
}

// dispatchClusterOnlyGolden is the whole cluster-only declaration, taken from the default mode's
// registration golden: the component, the test's gcp provider and the five resources above, each
// under the name and the parent the default mode gives it. Reading those from the golden rather
// than writing them out again is what holds the two modes to one cluster: a resource the default
// mode renames or reparents moves this expectation with it, and a resource the cluster-only mode
// adds or drops is a line this file does not hold.
func dispatchClusterOnlyGolden(t *testing.T) string {
	t.Helper()
	keep := append([]string{dispatchType, gcpProviderType}, dispatchClusterOnlyTypes...)
	var lines []string
	for _, line := range strings.Split(readGolden(t, "dispatch-registrations.golden"), "\n") {
		if typ, _, ok := strings.Cut(line, " "); ok && slices.Contains(keep, typ) {
			lines = append(lines, line+"\n")
		}
	}
	require.Len(t, lines, len(keep), "the golden holds one line for the component, the provider and each cluster resource")
	return strings.Join(lines, "")
}

// dispatchGoldenWithout is the whole declaration of a mode that registers what the default mode
// registers less the resources dropped names, each a child of the component by its type and name:
// the default mode's registration golden less the one line of each. Reading it from the golden
// rather than writing it out again holds the two to one declaration: a resource the default
// renames or reparents moves this expectation with it. The golden must hold exactly one line for
// each resource dropped names.
func dispatchGoldenWithout(t *testing.T, dropped ...dispatchResourceName) string {
	t.Helper()
	var kept strings.Builder
	count := 0
	for _, line := range strings.SplitAfter(readGolden(t, "dispatch-registrations.golden"), "\n") {
		if slices.ContainsFunc(dropped, func(r dispatchResourceName) bool { return strings.HasPrefix(line, r.typ+" "+r.name+" ") }) {
			count++
			continue
		}
		kept.WriteString(line)
	}
	require.Equal(t, len(dropped), count, "the golden holds one line for each of %v", dropped)
	return kept.String()
}

// With clusterOnly the component declares the cluster and what the cluster is made of, and nothing
// else: no private services range or peering, no Cloud SQL instance, database or user, no
// generated password, no Secret Manager grant, no Kubernetes provider and no Kubernetes object.
// The comparison below is of the whole registration set against the cluster set the golden
// carries, so any of those registering is a line that comparison does not hold and fails naming
// it. The five resources it does declare keep the names and parents the default mode gives them
// (TestDispatchRegistrationsMatchTheGolden pins those for the default mode), so a consumer that
// later turns the mode off keeps its cluster rather than replacing it. The mode registers no
// RandomPassword, so the component requires no random provider: the program below declares the
// gcp provider alone, so the NoError is what holds the requirement gone, since with it back
// providers.Require would refuse the component before anything registered. With the mode off the
// random provider is still required before anything registers
// (TestDispatchRequiresGCPAndRandomProvidersBeforeRegistering).
func TestDispatchClusterOnlyRegistersTheClusterAlone(t *testing.T) {
	m, err := runDispatchClusterOnly(t, dispatchClusterOnlyArgs())
	require.NoError(t, err)
	require.Equal(t, dispatchClusterOnlyGolden(t), dispatchRegistrations(m))
}

// deletionProtection is one of the five inputs the mode keeps live, and it guards the cluster as
// well as the database the mode does not build: it reaches the cluster's own deletionProtection
// rather than being ignored, and is true when the consumer sets nothing.
func TestDispatchClusterOnlyPassesDeletionProtectionToTheCluster(t *testing.T) {
	for _, row := range []struct {
		name string
		set  *bool
		want bool
	}{
		{"unset", nil, true},
		{"false", pulumi.BoolRef(false), false},
		{"true", pulumi.BoolRef(true), true},
	} {
		t.Run(row.name, func(t *testing.T) {
			args := dispatchClusterOnlyArgs()
			args.DeletionProtection = row.set
			m, err := runDispatchClusterOnly(t, args)
			require.NoError(t, err)
			cluster := m.One(t, "gcp:container/cluster:Cluster").Inputs.Mappable()
			require.Equal(t, row.want, cluster["deletionProtection"])
		})
	}
}

// regionalNetwork is one of the five inputs the mode keeps live, since it shapes the network half
// both modes build, but the mode always makes its own cluster on the regional subnetwork and
// refuses existingCluster, the one input beside which false is taken. So false is refused, by the
// resolver and before anything registers, as a core.InputError naming regionalNetwork, rather than
// leaving the cluster no subnetwork; and true, the value it has when unset, changes nothing: it
// resolves, and the mode registers exactly the cluster-only declaration.
func TestDispatchClusterOnlyTakesRegionalNetworkTrueAndRefusesFalse(t *testing.T) {
	const refusal = "regionalNetwork false leaves the cluster clusterOnly builds no subnetwork: unset regionalNetwork"
	args := dispatchClusterOnlyArgs()
	args.RegionalNetwork = pulumi.BoolRef(false)
	_, err := gcp.ResolveDispatch(args)
	require.EqualError(t, err, refusal)
	var refused *core.InputError
	require.ErrorAs(t, err, &refused)
	require.Equal(t, "regionalNetwork", refused.Input)
	m, err := runDispatchClusterOnly(t, args)
	require.ErrorContains(t, err, "envoy-infra:gcp:Dispatch example-dispatch: "+refusal)
	require.Equal(t, []string{gcpProviderType}, m.Types(), "the program's own gcp provider registered and nothing of the component's")

	args.RegionalNetwork = pulumi.BoolRef(true)
	_, err = gcp.ResolveDispatch(args)
	require.NoError(t, err)
	m, err = runDispatchClusterOnly(t, args)
	require.NoError(t, err)
	require.Equal(t, dispatchClusterOnlyGolden(t), dispatchRegistrations(m))
}

// dispatchInput is one row of an input table: the input's schema name, and what sets that input,
// and nothing else, to a value a consumer would plausibly send. A row is applied to whatever
// DispatchArgs a test hands it, alone on a cluster-only fixture here and on top of other inputs
// in the sandbox's own tables, so it never assumes what the rest of the arguments hold.
type dispatchInput struct {
	input string
	set   func(*gcp.DispatchArgs)
}

// dispatchClusterOnlyRefusals is every input the cluster-only mode refuses, one row each, holding
// a value a consumer would plausibly send. The eight rows of the tailnet and sign-in groups come
// from the groups' own tables, which are where those values are written
// (dispatchSandboxTailnetGroup, dispatchSandboxSignInGroup), so a group that gains an input is
// refused here without that input being written down a second time.
// TestDispatchEveryInputIsLiveOrRefusedUnderClusterOnly holds the table complete against
// DispatchArgs itself.
func dispatchClusterOnlyRefusals() []dispatchInput {
	return slices.Concat(dispatchSandboxTailnetGroup, dispatchSandboxSignInGroup, []dispatchInput{
		{"outsideOrigin", func(a *gcp.DispatchArgs) { a.OutsideOrigin = dispatchOutsideOrigin }},
		{"signInOutside", func(a *gcp.DispatchArgs) { a.SignInOutside = true }},
		{"signInPasskey", func(a *gcp.DispatchArgs) { a.SignInPasskey = true }},
		{"signInWorkspaceDomains", func(a *gcp.DispatchArgs) { a.SignInWorkspaceDomains = []string{"example.net"} }},
		{"parentStatusRule", func(a *gcp.DispatchArgs) { a.ParentStatusRule = true }},
		{"legionStatusScope", func(a *gcp.DispatchArgs) { a.LegionStatusScope = true }},
		{"publicOrigin", func(a *gcp.DispatchArgs) { a.PublicOrigin = dispatchPublicOrigin }},
		{"iapAudience", func(a *gcp.DispatchArgs) { a.IapAudience = pulumi.String(dispatchIAPAudience) }},
		{"loadBalancerSourceRanges", func(a *gcp.DispatchArgs) { a.LoadBalancerSourceRanges = dispatchLoadBalancerRanges }},
		{"networkEndpointGroupName", func(a *gcp.DispatchArgs) { a.NetworkEndpointGroupName = dispatchNEGName }},
		{"envoyImage", func(a *gcp.DispatchArgs) { a.EnvoyImage = dispatchEnvoyImage }},
		{"cloudSqlProxyImage", func(a *gcp.DispatchArgs) { a.CloudSqlProxyImage = dispatchProxyImage }},
		{"natsUrl", func(a *gcp.DispatchArgs) { a.NatsUrl = pulumi.String("nats://example-hub.example-tailnet.ts.net:4222") }},
		{"natsSeedSecret", func(a *gcp.DispatchArgs) {
			a.NatsSeedSecret = pulumi.String("projects/example-project/secrets/example-nats-seed")
		}},
		{"agentSecretsUrl", func(a *gcp.DispatchArgs) { a.AgentSecretsUrl = "https://example-broker.internal.example" }},
		{"agentSecretsAudience", func(a *gcp.DispatchArgs) { a.AgentSecretsAudience = "example-broker-audience" }},
		{"oidcAudience", func(a *gcp.DispatchArgs) { a.OidcAudience = dispatchOidcAudience }},
		{"oidcIssuer", func(a *gcp.DispatchArgs) { a.OidcIssuer = dispatchOidcIssuer }},
		{"projectOwnerFallback", func(a *gcp.DispatchArgs) { a.ProjectOwnerFallback = "owner@example.com" }},
		{"appClientId", func(a *gcp.DispatchArgs) { a.AppClientId = "example-app-client-id" }},
		{"appId", func(a *gcp.DispatchArgs) { a.AppId = "123456" }},
		{"appPrivateKeySecretName", func(a *gcp.DispatchArgs) { a.AppPrivateKeySecretName = "example-app-key" }},
		{"appPrivateKeySecretKey", func(a *gcp.DispatchArgs) { a.AppPrivateKeySecretKey = "private-key.b64" }},
		{"verdictAppId", func(a *gcp.DispatchArgs) { a.VerdictAppId = pulumi.StringRef("5341240") }},
		{"databaseVersion", func(a *gcp.DispatchArgs) { a.DatabaseVersion = pulumi.StringRef("POSTGRES_18") }},
		{"databaseTier", func(a *gcp.DispatchArgs) { a.DatabaseTier = pulumi.StringRef("db-custom-4-15360") }},
		{"databaseAvailabilityType", func(a *gcp.DispatchArgs) { a.DatabaseAvailabilityType = pulumi.StringRef("REGIONAL") }},
		{"databaseWaitsFor", func(a *gcp.DispatchArgs) { a.DatabaseWaitsFor = pulumi.String("example-ordering-value") }},
		{"namespace", func(a *gcp.DispatchArgs) { a.Namespace = pulumi.StringRef("example-namespace") }},
		{"existingNamespace", func(a *gcp.DispatchArgs) { a.ExistingNamespace = pulumi.BoolRef(true) }},
		{"existingCluster", func(a *gcp.DispatchArgs) { a.ExistingCluster = exampleExistingCluster() }},
		{"existingDatabaseInstance", func(a *gcp.DispatchArgs) { a.ExistingDatabaseInstance = pulumi.String(replicaConnectionName) }},
		{"replicas", func(a *gcp.DispatchArgs) { a.Replicas = pulumi.IntRef(2) }},
		{"frontDoorReplicas", func(a *gcp.DispatchArgs) { a.FrontDoorReplicas = pulumi.IntRef(2) }},
		{"listenerReplicas", func(a *gcp.DispatchArgs) { a.ListenerReplicas = pulumi.IntRef(1) }},
		{"natsInCluster", func(a *gcp.DispatchArgs) { a.NatsInCluster = pulumi.BoolRef(true) }},
		{"natsReplicas", func(a *gcp.DispatchArgs) { a.NatsReplicas = pulumi.IntRef(dispatchNatsReplicas) }},
		{"streamMaxBytes", func(a *gcp.DispatchArgs) { a.StreamMaxBytes = pulumi.IntRef(dispatchStreamMaxBytes) }},
		{"senderMessagesPerMinute", func(a *gcp.DispatchArgs) { a.SenderMessagesPerMinute = pulumi.IntRef(60) }},
		{"senderMessageBurst", func(a *gcp.DispatchArgs) { a.SenderMessageBurst = pulumi.IntRef(10) }},
		{"signingKeyGenerations", func(a *gcp.DispatchArgs) { a.SigningKeyGenerations = []int{0, 1} }},
		{"appCpu", func(a *gcp.DispatchArgs) { a.AppCpu = pulumi.StringRef("1") }},
		{"appMemory", func(a *gcp.DispatchArgs) { a.AppMemory = pulumi.StringRef("4Gi") }},
		{"proxyCpu", func(a *gcp.DispatchArgs) { a.ProxyCpu = pulumi.StringRef("1") }},
		{"proxyMemory", func(a *gcp.DispatchArgs) { a.ProxyMemory = pulumi.StringRef("2Gi") }},
		{"appEphemeralStorage", func(a *gcp.DispatchArgs) { a.AppEphemeralStorage = pulumi.StringRef("2Gi") }},
		{"proxyEphemeralStorage", func(a *gcp.DispatchArgs) { a.ProxyEphemeralStorage = pulumi.StringRef("1Gi") }},
		{"pprofPort", func(a *gcp.DispatchArgs) { a.PprofPort = pulumi.IntRef(6060) }},
		{"apiV1End", func(a *gcp.DispatchArgs) { a.ApiV1End = dispatchAPIV1End }},
		{"priorityClassName", func(a *gcp.DispatchArgs) { a.PriorityClassName = pulumi.StringRef("example-priority") }},
		{"appPdbMinAvailable", func(a *gcp.DispatchArgs) { a.AppPdbMinAvailable = pulumi.IntRef(1) }},
		{"frontDoorPdbMinAvailable", func(a *gcp.DispatchArgs) { a.FrontDoorPdbMinAvailable = pulumi.IntRef(1) }},
		{"listenerPdbMinAvailable", func(a *gcp.DispatchArgs) { a.ListenerPdbMinAvailable = pulumi.IntRef(1) }},
		{"allowedNamespaces", func(a *gcp.DispatchArgs) { a.AllowedNamespaces = []string{"envoy"} }},
		{"azureEmbedEndpoint", func(a *gcp.DispatchArgs) { a.AzureEmbedEndpoint = pulumi.String(azureEmbedEndpoint) }},
		{"azureEmbedDeployment", func(a *gcp.DispatchArgs) { a.AzureEmbedDeployment = pulumi.String("example-embed-deployment") }},
		{"azureEmbedTokensPerMinute", func(a *gcp.DispatchArgs) { a.AzureEmbedTokensPerMinute = pulumi.IntRef(300000) }},
		{"azureEmbedKeySecret", func(a *gcp.DispatchArgs) { a.AzureEmbedKeySecret = pulumi.String(azureEmbedKeySecret) }},
		{"azureTenantId", func(a *gcp.DispatchArgs) { a.AzureTenantId = pulumi.String(azureTenantID) }},
		{"azureClientId", func(a *gcp.DispatchArgs) { a.AzureClientId = pulumi.String(azureClientID) }},
		{"azureFederationAudience", func(a *gcp.DispatchArgs) {
			a.AzureFederationAudience = pulumi.StringRef("api://AzureADTokenExchange")
		}},
		{"azureChatEndpoint", func(a *gcp.DispatchArgs) { a.AzureChatEndpoint = pulumi.String(azureChatEndpoint) }},
		{"azureChatDeployment", func(a *gcp.DispatchArgs) { a.AzureChatDeployment = pulumi.String("example-chat-deployment") }},
		{"azureChatApiVersion", func(a *gcp.DispatchArgs) { a.AzureChatApiVersion = pulumi.StringRef("2024-05-01-preview") }},
		{"azureChatScope", func(a *gcp.DispatchArgs) {
			a.AzureChatScope = pulumi.StringRef("https://cognitiveservices.azure.com/.default")
		}},
		{"azureChatTokensPerMinute", func(a *gcp.DispatchArgs) { a.AzureChatTokensPerMinute = pulumi.IntRef(120000) }},
		{"chatEndpoint", func(a *gcp.DispatchArgs) { a.ChatEndpoint = pulumi.String(chatEndpointURL) }},
		{"chatModel", func(a *gcp.DispatchArgs) { a.ChatModel = pulumi.StringRef("example-chat-model") }},
		{"chatTokenSecret", func(a *gcp.DispatchArgs) { a.ChatTokenSecret = pulumi.String(chatTokenSecret) }},
		{"consumerNames", func(a *gcp.DispatchArgs) { a.ConsumerNames = "fresh" }},
		{"replaceUnderHold", func(a *gcp.DispatchArgs) { a.ReplaceUnderHold = true }},
	})
}

// Every input that configures Dispatch rather than the cluster is refused by name when it arrives
// with clusterOnly, as a core.InputError naming it, so a stack already running Dispatch cannot
// turn the mode on: such a stack sets these inputs, and the update is refused before anything it
// would no longer build is deleted. The resolver is what refuses, and it needs no engine.
func TestDispatchClusterOnlyRefusesEveryDispatchInput(t *testing.T) {
	for _, row := range dispatchClusterOnlyRefusals() {
		t.Run(row.input, func(t *testing.T) {
			args := dispatchClusterOnlyArgs()
			row.set(args)
			_, err := gcp.ResolveDispatch(args)
			require.EqualError(t, err, row.input+" configures Dispatch, which clusterOnly does not build: unset it, or unset clusterOnly")
			var refused *core.InputError
			require.ErrorAs(t, err, &refused)
			require.Equal(t, row.input, refused.Input)
		})
	}
	// The same inputs unset, which is what a cluster-only consumer sends, resolve.
	_, err := gcp.ResolveDispatch(dispatchClusterOnlyArgs())
	require.NoError(t, err)
}

// The refusal table above is exactly DispatchArgs' inputs less the five the mode keeps live, so an
// input added later is either refused or deliberately live and cannot be dropped in silence: it
// would otherwise reach a cluster-only component that builds nothing it configures. Shaped as
// aws/seeds_test.go's TestEveryStringInputHasASeedRow, which holds the seed tables complete.
func TestDispatchEveryInputIsLiveOrRefusedUnderClusterOnly(t *testing.T) {
	args := reflect.TypeFor[gcp.DispatchArgs]()
	inputs := make([]string, 0, args.NumField())
	for i := range args.NumField() {
		field := args.Field(i)
		name, _, _ := strings.Cut(field.Tag.Get("pulumi"), ",")
		require.NotEmpty(t, name, "%s carries a pulumi tag", field.Name)
		inputs = append(inputs, name)
	}
	// The five inputs both modes use: clusterOnly itself, region and hostname, which name every
	// cloud resource, deletionProtection, which guards the cluster as well as the database, and
	// regionalNetwork, which shapes the network half both modes build (false is refused here by a
	// rule of its own, TestDispatchClusterOnlyTakesRegionalNetworkTrueAndRefusesFalse).
	covered := []string{"clusterOnly", "region", "hostname", "deletionProtection", "regionalNetwork"}
	for _, row := range dispatchClusterOnlyRefusals() {
		covered = append(covered, row.input)
	}
	slices.Sort(inputs)
	slices.Sort(covered)
	require.Equal(t, inputs, covered)
}
