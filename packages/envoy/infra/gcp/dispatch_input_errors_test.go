package gcp_test

import (
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/internal/testnats"
)

// Every refusal of Dispatch's resolver (newDispatchSettings and the functions it calls) is a
// core.InputError naming the input to correct as the schema names it, a list by its own name and
// never by an entry's index, as the package's rule has every refusal be, so a caller finds that
// input with errors.As rather than by parsing the message. The messages themselves are the other
// tests' (TestDispatchSandboxRefusesWhatItDoesNotBuild, TestDispatchRefusesWhatTheIAPDoorCannotServe,
// TestDispatchRefusesOutsideInputsItCannotServe, TestDispatchPartialTailnetGroupNamesEveryMissingInput,
// TestDispatchRefusesAnNKeySeedInAPlainInput and the resource and Azure tables); each row here also
// holds that the message names the input, so a row cannot pass on some other refusal. One row for
// each place the resolver refuses; the refusals of signInPasskey, signInWorkspaceDomains and
// clusterOnly have their own tables (TestDispatchRefusesASignInPasskeyItCannotServe,
// TestDispatchRefusesASignInWorkspaceDomainsItCannotServe, TestDispatchClusterOnlyRefusesEveryDispatchInput).
func TestDispatchResolverRefusalsAreInputErrors(t *testing.T) {
	seed, _ := testnats.User(t)
	lower := strings.ToLower(seed)
	with := func(base func() *gcp.DispatchArgs, edit func(*gcp.DispatchArgs)) func() *gcp.DispatchArgs {
		return func() *gcp.DispatchArgs {
			args := base()
			edit(args)
			return args
		}
	}
	// d is the default fixture, the front door with browser sign-in, with edit applied.
	d := func(edit func(*gcp.DispatchArgs)) func() *gcp.DispatchArgs { return with(dispatchArgs, edit) }
	publicOriginWithoutSignIn := func() *gcp.DispatchArgs {
		args := dispatchPublicOriginArgs()
		args.SignInClientId, args.SignInClientSecret, args.SignInDomain = "", nil, ""
		return args
	}
	str := pulumi.StringRef
	type row struct {
		name, input string
		args        func() *gcp.DispatchArgs
	}
	var rows []row
	// The sandbox refuses each sign-in input for being sent at all.
	for _, in := range dispatchSandboxSignInGroup {
		rows = append(rows, row{"sandbox " + in.input, in.input, with(dispatchSandboxArgs, in.set)})
	}
	rows = append(rows, []row{
		// A seed in a plain input, named as the schema names it.
		{"a seed as the region", "region", d(func(a *gcp.DispatchArgs) { a.Region = seed })},
		{"a seed in a tag", "tags", d(func(a *gcp.DispatchArgs) { a.Tags = []string{"tag:" + seed} })},
		{"a seed in an allowed namespace", "allowedNamespaces", d(func(a *gcp.DispatchArgs) { a.AllowedNamespaces = []string{"envoy", lower} })},
		{"a seed in the existing cluster's name", "existingCluster.name", d(func(a *gcp.DispatchArgs) {
			a.ExistingCluster = &gcp.DispatchClusterArgs{Name: lower, Location: "us-central1"}
		})},
		// The hostname and the region.
		{"hostname not the rule's shape", "hostname", d(func(a *gcp.DispatchArgs) { a.Hostname = "Example-dispatch" })},
		{"hostname of the reserved form", "hostname", d(func(a *gcp.DispatchArgs) { a.Hostname = "ab--cd" })},
		{"region unset", "region", d(func(a *gcp.DispatchArgs) { a.Region = "" })},
		// The tailnet group.
		{"tailnetDnsName malformed", "tailnetDnsName", d(func(a *gcp.DispatchArgs) { a.TailnetDnsName = "example-tailnet.example.com" })},
		{"tags empty", "tags", d(func(a *gcp.DispatchArgs) { a.Tags = nil })},
		{"a tag malformed", "tags", d(func(a *gcp.DispatchArgs) { a.Tags = []string{"example-dispatch"} })},
		{"tailscaleImage not pinned", "tailscaleImage", d(func(a *gcp.DispatchArgs) { a.TailscaleImage = "example.invalid/tailscale:latest" })},
		{"frontDoorToolsImage not pinned", "frontDoorToolsImage", d(func(a *gcp.DispatchArgs) { a.FrontDoorToolsImage = "example.invalid/envoy:latest" })},
		{"tailnetAuthKey unset", "tailnetAuthKey", d(func(a *gcp.DispatchArgs) { a.TailnetAuthKey = nil })},
		// publicOrigin.
		{"publicOrigin beside the tailnet inputs", "publicOrigin", d(func(a *gcp.DispatchArgs) { a.PublicOrigin = dispatchPublicOrigin })},
		{"publicOrigin with no scheme", "publicOrigin", with(dispatchPublicOriginArgs, func(a *gcp.DispatchArgs) { a.PublicOrigin = "dispatch.example.com" })},
		{"publicOrigin over http", "publicOrigin", with(dispatchPublicOriginArgs, func(a *gcp.DispatchArgs) { a.PublicOrigin = "http://dispatch.example.com" })},
		{"publicOrigin an IP address with browser sign-in", "publicOrigin", with(dispatchPublicOriginArgs, func(a *gcp.DispatchArgs) { a.PublicOrigin = "https://10.0.0.1" })},
		// outsideOrigin.
		{"outsideOrigin with no scheme", "outsideOrigin", d(func(a *gcp.DispatchArgs) { a.OutsideOrigin = "dispatch-outside.example.com" })},
		{"outsideOrigin over http", "outsideOrigin", d(func(a *gcp.DispatchArgs) { a.OutsideOrigin = "http://dispatch-outside.example.com" })},
		{"outsideOrigin on the front door's host", "outsideOrigin", d(func(a *gcp.DispatchArgs) { a.OutsideOrigin = "https://example-dispatch.example-tailnet.ts.net" })},
		{"outsideOrigin an IP address with browser sign-in", "outsideOrigin", d(func(a *gcp.DispatchArgs) { a.OutsideOrigin = "https://10.0.0.1" })},
		// The deployment's own entrance.
		{"loadBalancerSourceRanges without publicOrigin", "loadBalancerSourceRanges", d(func(a *gcp.DispatchArgs) { a.LoadBalancerSourceRanges = dispatchLoadBalancerRanges })},
		{"networkEndpointGroupName without publicOrigin", "networkEndpointGroupName", d(func(a *gcp.DispatchArgs) { a.NetworkEndpointGroupName = dispatchNEGName })},
		{"a source range that is no CIDR", "loadBalancerSourceRanges", with(dispatchEntranceArgs, func(a *gcp.DispatchArgs) { a.LoadBalancerSourceRanges = []string{"not-a-cidr"} })},
		{"a source range not written as its network", "loadBalancerSourceRanges", with(dispatchEntranceArgs, func(a *gcp.DispatchArgs) { a.LoadBalancerSourceRanges = []string{"10.1.2.3/8"} })},
		{"a source range admitting every address", "loadBalancerSourceRanges", with(dispatchEntranceArgs, func(a *gcp.DispatchArgs) { a.LoadBalancerSourceRanges = []string{"0.0.0.0/0"} })},
		{"networkEndpointGroupName malformed", "networkEndpointGroupName", with(dispatchEntranceArgs, func(a *gcp.DispatchArgs) { a.NetworkEndpointGroupName = "Example-neg" })},
		// The sandbox.
		{"sandbox signInOutside", "signInOutside", with(dispatchSandboxArgs, func(a *gcp.DispatchArgs) { a.SignInOutside = true })},
		{"sandbox outsideOrigin", "outsideOrigin", with(dispatchSandboxArgs, func(a *gcp.DispatchArgs) { a.OutsideOrigin = dispatchOutsideOrigin })},
		{"sandbox allowedNamespaces empty", "allowedNamespaces", with(dispatchSandboxArgs, func(a *gcp.DispatchArgs) { a.AllowedNamespaces = nil })},
		// What any Dispatch without a front door cannot make good, in the sandbox and at publicOrigin.
		{"sandbox natsInCluster unset", "natsInCluster", with(dispatchSandboxArgs, func(a *gcp.DispatchArgs) { a.NatsInCluster = nil })},
		{"sandbox frontDoorReplicas", "frontDoorReplicas", with(dispatchSandboxArgs, func(a *gcp.DispatchArgs) { a.FrontDoorReplicas = pulumi.IntRef(2) })},
		{"sandbox frontDoorPdbMinAvailable", "frontDoorPdbMinAvailable", with(dispatchSandboxArgs, func(a *gcp.DispatchArgs) { a.FrontDoorPdbMinAvailable = pulumi.IntRef(1) })},
		{"publicOrigin natsInCluster false", "natsInCluster", with(dispatchPublicOriginArgs, func(a *gcp.DispatchArgs) { a.NatsInCluster = pulumi.BoolRef(false) })},
		// The IAP door.
		{"iapAudience in the sandbox", "iapAudience", with(dispatchSandboxArgs, func(a *gcp.DispatchArgs) { a.IapAudience = pulumi.String(dispatchIAPAudience) })},
		{"iapAudience behind the front door", "iapAudience", d(func(a *gcp.DispatchArgs) { a.IapAudience = pulumi.String(dispatchIAPAudience) })},
		{"iapAudience beside signInClientId", "signInClientId", with(dispatchIAPArgs, func(a *gcp.DispatchArgs) { a.SignInClientId = "example-client-id" })},
		{"iapAudience beside signInClientSecret", "signInClientSecret", with(dispatchIAPArgs, func(a *gcp.DispatchArgs) {
			a.SignInClientSecret = pulumi.String("example-client-secret")
		})},
		{"iapAudience beside signInOutside", "signInOutside", with(dispatchIAPArgs, func(a *gcp.DispatchArgs) { a.SignInOutside = true })},
		{"iapAudience beside outsideOrigin", "outsideOrigin", with(dispatchIAPArgs, func(a *gcp.DispatchArgs) { a.OutsideOrigin = dispatchOutsideOrigin })},
		{"iapAudience without signInDomain", "signInDomain", with(dispatchIAPArgs, func(a *gcp.DispatchArgs) { a.SignInDomain = "" })},
		// signInOutside without browser sign-in to extend.
		{"signInOutside beside the front door with no Google sign-in", "signInOutside",
			with(dispatchTailnetWithoutSignInArgs, func(a *gcp.DispatchArgs) { a.SignInOutside = true })},
		{"signInOutside beside publicOrigin with no Google sign-in", "signInOutside",
			with(publicOriginWithoutSignIn, func(a *gcp.DispatchArgs) { a.SignInOutside = true })},
		// The sign-in group set in part.
		{"sign-in group without signInClientId", "signInClientId", d(func(a *gcp.DispatchArgs) { a.SignInClientId = "" })},
		{"sign-in group with an uppercase signInDomain", "signInDomain", d(func(a *gcp.DispatchArgs) { a.SignInDomain = "Example.com" })},
		{"sign-in group without signInClientSecret", "signInClientSecret", d(func(a *gcp.DispatchArgs) { a.SignInClientSecret = nil })},
		// The app's two images.
		{"envoyImage not pinned", "envoyImage", d(func(a *gcp.DispatchArgs) { a.EnvoyImage = "example.invalid/envoy:latest" })},
		{"cloudSqlProxyImage not pinned", "cloudSqlProxyImage", d(func(a *gcp.DispatchArgs) { a.CloudSqlProxyImage = "example.invalid/proxy:latest" })},
		// NATS.
		{"natsUrl unset", "natsUrl", d(func(a *gcp.DispatchArgs) { a.NatsUrl = nil })},
		{"natsSeedSecret unset", "natsSeedSecret", d(func(a *gcp.DispatchArgs) { a.NatsSeedSecret = nil })},
		// The secrets broker's relay.
		{"agentSecretsAudience without agentSecretsUrl", "agentSecretsUrl", d(func(a *gcp.DispatchArgs) { a.AgentSecretsAudience = "example-broker-audience" })},
		{"agentSecretsUrl without agentSecretsAudience", "agentSecretsAudience", d(func(a *gcp.DispatchArgs) { a.AgentSecretsUrl = "https://example-broker.internal.example" })},
		{"agentSecretsUrl with a path", "agentSecretsUrl", d(func(a *gcp.DispatchArgs) {
			a.AgentSecretsUrl, a.AgentSecretsAudience = "https://example-broker.internal.example/v1", "example-broker-audience"
		})},
		{"agentSecretsUrl with a user", "agentSecretsUrl", d(func(a *gcp.DispatchArgs) {
			a.AgentSecretsUrl, a.AgentSecretsAudience = "https://example-user@example-broker.internal.example", "example-broker-audience"
		})},
		// Dispatch's own service-account verifier.
		{"oidcIssuer without oidcAudience", "oidcAudience", d(func(a *gcp.DispatchArgs) { a.OidcIssuer = dispatchOidcIssuer })},
		{"oidcIssuer over http", "oidcIssuer", d(func(a *gcp.DispatchArgs) {
			a.OidcAudience, a.OidcIssuer = dispatchOidcAudience, "http://container.googleapis.com/v1/projects/example"
		})},
		// The GitHub App.
		{"the GitHub App set in part", "appId", d(func(a *gcp.DispatchArgs) { a.AppClientId = "example-app-client-id" })},
		{"appId not digits", "appId", d(func(a *gcp.DispatchArgs) { setApp(a); a.AppId = "example" })},
		{"appPrivateKeySecretName not a DNS subdomain", "appPrivateKeySecretName", d(func(a *gcp.DispatchArgs) { setApp(a); a.AppPrivateKeySecretName = "Example_Key" })},
		{"appPrivateKeySecretKey malformed", "appPrivateKeySecretKey", d(func(a *gcp.DispatchArgs) { setApp(a); a.AppPrivateKeySecretKey = "private key" })},
		// The shared Azure sign-in and the Azure embedder.
		{"the Azure sign-in with neither endpoint", "azureTenantId", d(setAzureSignIn)},
		{"azureTenantId without azureClientId", "azureClientId", d(func(a *gcp.DispatchArgs) { setAzureEndpoint(a); a.AzureTenantId = pulumi.String(azureTenantID) })},
		{"azureClientId without azureTenantId", "azureTenantId", d(func(a *gcp.DispatchArgs) { setAzureEndpoint(a); a.AzureClientId = pulumi.String(azureClientID) })},
		{"azureFederationAudience with the key form", "azureFederationAudience", d(func(a *gcp.DispatchArgs) {
			setAzureKey(a)
			a.AzureFederationAudience = str("api://AzureADTokenExchange")
		})},
		{"azureFederationAudience empty", "azureFederationAudience", d(func(a *gcp.DispatchArgs) { setAzureFederation(a); a.AzureFederationAudience = str("") })},
		{"an azureEmbed input without azureEmbedEndpoint", "azureEmbedDeployment", d(func(a *gcp.DispatchArgs) {
			a.AzureEmbedDeployment = pulumi.String("example-embed-deployment")
		})},
		{"azureEmbedEndpoint without azureEmbedTokensPerMinute", "azureEmbedTokensPerMinute", d(func(a *gcp.DispatchArgs) {
			setAzureKey(a)
			a.AzureEmbedTokensPerMinute = nil
		})},
		{"azureEmbedTokensPerMinute 0", "azureEmbedTokensPerMinute", d(func(a *gcp.DispatchArgs) { setAzureKey(a); a.AzureEmbedTokensPerMinute = pulumi.IntRef(0) })},
		{"both Azure credential forms", "azureEmbedKeySecret", d(func(a *gcp.DispatchArgs) {
			setAzureFederation(a)
			a.AzureEmbedKeySecret = pulumi.String(azureEmbedKeySecret)
		})},
		{"azureEmbedEndpoint with no credential", "azureEmbedEndpoint", d(setAzureEndpoint)},
		// The answerer.
		{"an azureChat input without azureChatEndpoint", "azureChatDeployment", d(func(a *gcp.DispatchArgs) {
			a.AzureChatDeployment = pulumi.String("example-chat-deployment")
		})},
		{"a chat input without chatEndpoint", "chatModel", d(func(a *gcp.DispatchArgs) { a.ChatModel = str("example-chat-model") })},
		{"both answerers", "chatEndpoint", d(func(a *gcp.DispatchArgs) { setAzureChat(a); setChatEndpoint(a) })},
		{"azureChatEndpoint without azureChatTokensPerMinute", "azureChatTokensPerMinute", d(func(a *gcp.DispatchArgs) {
			setAzureChat(a)
			a.AzureChatTokensPerMinute = nil
		})},
		{"azureChatTokensPerMinute 0", "azureChatTokensPerMinute", d(func(a *gcp.DispatchArgs) { setAzureChat(a); a.AzureChatTokensPerMinute = pulumi.IntRef(0) })},
		{"azureChatEndpoint without azureChatDeployment", "azureChatDeployment", d(func(a *gcp.DispatchArgs) {
			setAzureChat(a)
			a.AzureChatDeployment = nil
		})},
		{"azureChatEndpoint without the Azure sign-in", "azureChatEndpoint", d(func(a *gcp.DispatchArgs) {
			setAzureChat(a)
			a.AzureTenantId, a.AzureClientId = nil, nil
		})},
		{"azureChatApiVersion empty", "azureChatApiVersion", d(func(a *gcp.DispatchArgs) { setAzureChat(a); a.AzureChatApiVersion = str("") })},
		{"azureChatScope empty", "azureChatScope", d(func(a *gcp.DispatchArgs) { setAzureChat(a); a.AzureChatScope = str("") })},
		{"chatEndpoint without chatTokenSecret", "chatTokenSecret", d(func(a *gcp.DispatchArgs) { setChatEndpoint(a); a.ChatTokenSecret = nil })},
		{"chatEndpoint without chatModel", "chatModel", d(func(a *gcp.DispatchArgs) { setChatEndpoint(a); a.ChatModel = nil })},
		{"chatModel empty", "chatModel", d(func(a *gcp.DispatchArgs) { setChatEndpoint(a); a.ChatModel = str("") })},
		// The database.
		{"databaseVersion empty", "databaseVersion", d(func(a *gcp.DispatchArgs) { a.DatabaseVersion = str("") })},
		{"databaseTier empty", "databaseTier", d(func(a *gcp.DispatchArgs) { a.DatabaseTier = str("") })},
		{"databaseAvailabilityType unknown", "databaseAvailabilityType", d(func(a *gcp.DispatchArgs) { a.DatabaseAvailabilityType = str("MULTI") })},
		// Replicas and disruption budgets.
		{"replicas 0", "replicas", d(func(a *gcp.DispatchArgs) { a.Replicas = pulumi.IntRef(0) })},
		{"frontDoorReplicas 0", "frontDoorReplicas", d(func(a *gcp.DispatchArgs) { a.FrontDoorReplicas = pulumi.IntRef(0) })},
		{"appPdbMinAvailable above replicas", "appPdbMinAvailable", d(func(a *gcp.DispatchArgs) { a.AppPdbMinAvailable = pulumi.IntRef(2) })},
		// The app pod's sizes.
		{"an empty appCpu", "appCpu", d(func(a *gcp.DispatchArgs) { a.AppCpu = str("") })},
		{"an appMemory exponent out of range", "appMemory", d(func(a *gcp.DispatchArgs) { a.AppMemory = str("1e99999999999999999999") })},
		{"a proxyCpu that is no quantity", "proxyCpu", d(func(a *gcp.DispatchArgs) { a.ProxyCpu = str("one") })},
		{"a proxyMemory of 0", "proxyMemory", d(func(a *gcp.DispatchArgs) { a.ProxyMemory = str("0") })},
		{"CPU below Autopilot's floor", "appCpu", d(func(a *gcp.DispatchArgs) { a.AppCpu, a.ProxyCpu = str("100m"), str("200m") })},
		{"CPU above Autopilot's ceiling", "appCpu", d(func(a *gcp.DispatchArgs) { a.ProxyCpu = str("30") })},
		{"memory below Autopilot's floor", "appMemory", d(func(a *gcp.DispatchArgs) { a.AppMemory, a.ProxyMemory = str("256Mi"), str("128Mi") })},
		{"memory above Autopilot's ceiling", "appMemory", d(func(a *gcp.DispatchArgs) { a.AppMemory = str("109Gi") })},
		{"too little memory per CPU", "appMemory", d(func(a *gcp.DispatchArgs) { a.AppCpu = str("8") })},
		{"too much memory per CPU", "appCpu", d(func(a *gcp.DispatchArgs) { a.AppMemory = str("32Gi") })},
		{"a proxyEphemeralStorage of 0", "proxyEphemeralStorage", d(func(a *gcp.DispatchArgs) { a.ProxyEphemeralStorage = str("0") })},
		{"ephemeral storage below Autopilot's floor", "appEphemeralStorage", d(func(a *gcp.DispatchArgs) {
			a.AppEphemeralStorage, a.ProxyEphemeralStorage = str("5Mi"), str("4Mi")
		})},
		{"ephemeral storage above Autopilot's ceiling", "appEphemeralStorage", d(func(a *gcp.DispatchArgs) {
			a.AppEphemeralStorage, a.ProxyEphemeralStorage = str("9Gi"), str("1025Mi")
		})},
		// The profiler's port.
		{"pprofPort outside 1-65535", "pprofPort", d(func(a *gcp.DispatchArgs) { a.PprofPort = pulumi.IntRef(70000) })},
		{"pprofPort the app's own", "pprofPort", d(func(a *gcp.DispatchArgs) { a.PprofPort = pulumi.IntRef(8766) })},
		{"pprofPort the proxy's", "pprofPort", d(func(a *gcp.DispatchArgs) { a.PprofPort = pulumi.IntRef(5432) })},
		// The namespaces.
		{"namespace not a DNS label", "namespace", d(func(a *gcp.DispatchArgs) { a.Namespace = str("Example") })},
		{"an allowed namespace not a DNS label", "allowedNamespaces", d(func(a *gcp.DispatchArgs) { a.AllowedNamespaces = []string{"envoy", "Example"} })},
	}...)
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			_, err := gcp.ResolveDispatch(row.args())
			require.ErrorContains(t, err, row.input, "the refusal names the input")
			var refused *core.InputError
			require.ErrorAs(t, err, &refused)
			require.Equal(t, row.input, refused.Input)
		})
	}
}
