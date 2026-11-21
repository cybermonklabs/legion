package gcp

import (
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/pulumi/pulumi-go-provider/infer"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"golang.org/x/net/idna"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/infra/internal/providers"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/principal"
	"github.com/sjawhar/envoy/internal/weborigin"
)

// dispatchType is the component's token. pulumi-go-provider derives the same one from the Dispatch
// type's package and name, and serves it under that token; NewDispatch registers with the constant
// so it also runs outside the plugin, where no call carries a token.
const dispatchType = "envoy-infra:gcp:Dispatch"

const (
	defaultDispatchDatabaseVersion = "POSTGRES_18"
	// defaultDispatchDatabaseTier is the Cloud SQL tier when databaseTier is unset: 4 vCPU and
	// 15 GB. During a roll, every app pod that holds connections (the replicas, the surge pod and
	// pods still terminating, for up to their grace period) times dispatchConnectionsPerPod must
	// fit in the instance's max_connections less PostgreSQL's 3 superuser-reserved slots, so a
	// consumer sets databaseTier for its replica count.
	defaultDispatchDatabaseTier = "db-custom-4-15360"
	defaultDispatchNamespace    = "dispatch"
	defaultDispatchReplicas     = 1

	// defaultDispatchFrontDoorReplicas and defaultDispatchListenerReplicas are the front door's
	// and the listener's pod counts when frontDoorReplicas and listenerReplicas are unset. The
	// front door runs two by default, since with one pod every node drain, upgrade or scale-down
	// that moves it takes Dispatch off the tailnet until its replacement is ready.
	defaultDispatchFrontDoorReplicas = 2
	defaultDispatchListenerReplicas  = 1

	// minDispatchReplicas is the fewest pods a workload of this component may run, and the count
	// at which its Deployment is recreated rather than rolled: a roll runs the outgoing build and
	// the incoming one together, which is safe only once the outgoing build follows the rules for
	// two builds running side by side and keeps its assets in the shared store. An operator who
	// runs a workload above this count runs such builds. It is what an unset replicas happens to
	// default to, not the same rule.
	minDispatchReplicas = 1

	// dispatchStrategyRecreate stops every running pod before starting the new one;
	// dispatchStrategyRollingUpdate starts new pods beside the old ones. dispatchStrategyFor picks
	// between them.
	dispatchStrategyRecreate      = "Recreate"
	dispatchStrategyRollingUpdate = "RollingUpdate"

	// dispatchAvailabilityZonal and dispatchAvailabilityRegional are the two Cloud SQL
	// availability types databaseAvailabilityType takes: one zone, the default, or a standby in a
	// second zone that Cloud SQL fails over to.
	dispatchAvailabilityZonal    = "ZONAL"
	dispatchAvailabilityRegional = "REGIONAL"

	// defaultAzureFederationAudience is the audience of the keyless form's projected token when
	// azureFederationAudience is unset: the one Entra's federated credentials expect by default.
	defaultAzureFederationAudience = "api://AzureADTokenExchange"

	// dispatchAnswererAzure and dispatchAnswererEndpoint are the two places Dispatch generates
	// text, the values DISPATCH_ANSWERER takes: a chat deployment of an Azure AI Foundry resource
	// (azureChatEndpoint), or any OpenAI-shaped chat completions service (chatEndpoint). With
	// neither endpoint the component renders no DISPATCH_ANSWERER and Dispatch configures no model.
	dispatchAnswererAzure    = "azure"
	dispatchAnswererEndpoint = "endpoint"

	// defaultDispatchAppCPU and defaultDispatchAppMemory size the app's envoy-dispatch container
	// when appCpu and appMemory are unset.
	defaultDispatchAppCPU    = "1"
	defaultDispatchAppMemory = "4Gi"

	// defaultDispatchProxyCPU and defaultDispatchProxyMemory size the app pod's cloud-sql-proxy
	// sidecar when proxyCpu and proxyMemory are unset; Autopilot counts it in the pod's request
	// beside envoy-dispatch. Every database call of the pod passes through the proxy's TLS: up
	// to dispatchConnectionsPerPod connections at once. The proxy's own
	// Kubernetes sidecar example requests 1 CPU, since its CPU use scales linearly with the IO
	// between the database and the application, and 2Gi, since its memory scales linearly with
	// the active connections
	// (https://github.com/GoogleCloudPlatform/cloud-sql-proxy/blob/v2.26.3/examples/k8s-sidecar/README.md,
	// "Run the Cloud SQL Auth Proxy as a sidecar"). One server pod's pools are that example's
	// load, so these are its values. With the app's defaults the pod requests 2 CPU and 6Gi,
	// inside Autopilot's bounds below.
	defaultDispatchProxyCPU    = "1"
	defaultDispatchProxyMemory = "2Gi"

	// autopilotDefaultEphemeralStorage is the ephemeral storage Autopilot gives a container that
	// names none, which each of the app pod's two containers does while appEphemeralStorage or
	// proxyEphemeralStorage is unset
	// (https://cloud.google.com/kubernetes-engine/docs/concepts/autopilot-resource-requests).
	autopilotDefaultEphemeralStorage = "1Gi"
)

// dispatchConnectionsPerPod is the most connections one app pod opens to Postgres: envoy-dispatch's
// store.ConnectionsPerProcess (packages/envoy/internal/dispatch/store/pool.go), the sum of its
// counted pools and its permanent listeners. The listener's and the front door's pods open none.
// TestDispatchConnectionsPerPodIsTheServers fails when a counted pool's size changes and this
// does not.
const dispatchConnectionsPerPod = 113

// Autopilot's bounds for the app pod's total request, the general-purpose compute class's for a pod
// with pod anti-affinity, which every Dispatch pod carries, or with the safe-to-evict annotation,
// which a pod no budget covers carries (https://cloud.google.com/kubernetes-engine/docs/concepts/autopilot-resource-requests):
// below the floor or outside the memory-per-CPU ratio Autopilot raises the pod's request, so the
// container would not run at the size the component renders; above the ceiling it rejects the pod.
var (
	autopilotPodMinCPU    = big.NewRat(1, 2)
	autopilotPodMaxCPU    = big.NewRat(30, 1)
	autopilotPodMinMemory = new(big.Rat).SetInt64(512 << 20)
	autopilotPodMaxMemory = new(big.Rat).SetInt64(110 << 30)
	autopilotMinGiBPerCPU = big.NewRat(1, 1)
	autopilotMaxGiBPerCPU = big.NewRat(13, 2)
	// The pod's ephemeral storage, the sum over its containers, must be from 10Mi to 10Gi in the
	// general-purpose class: Autopilot raises a pod below the floor and rejects one above the
	// ceiling.
	autopilotPodMinEphemeralStorage = new(big.Rat).SetInt64(10 << 20)
	autopilotPodMaxEphemeralStorage = new(big.Rat).SetInt64(10 << 30)
)

var (
	tailnetDNSNameRule = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.ts\.net$`)
	namespaceRule      = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	secretKeyRule      = regexp.MustCompile(`^[-._a-zA-Z0-9]+$`)
	// quantityRule is a Kubernetes resource quantity (k8s.io/apimachinery's resource.Quantity):
	// a signed decimal, then a binary suffix, a decimal suffix or a decimal exponent.
	quantityRule = regexp.MustCompile(`^([+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+))(Ki|Mi|Gi|Ti|Pi|Ei|n|u|m|k|M|G|T|P|E|[eE][+-]?[0-9]+)?$`)
	// clusterLocationRule is a Google Cloud region (us-central1) or zone (us-central1-a), where a
	// cluster runs.
	clusterLocationRule = regexp.MustCompile(`^[a-z]+-[a-z]+[0-9]+(?:-[a-z])?$`)
	// connectionNameRule is a Cloud SQL instance's connection name, <project>:<region>:<instance>,
	// the form the instance's connectionName takes: a project id (a lowercase letter, then 4 to 28
	// lowercase letters, digits or hyphens, and a lowercase letter or digit), which a domain-scoped
	// project prefixes with its domain (example.com:example-project, so the whole name then has
	// four parts); a region (us-central1); and the instance's name (a lowercase letter, then at most
	// 97 lowercase letters, digits or hyphens, ending in a letter or digit).
	connectionNameRule = regexp.MustCompile(`^(?:[a-z0-9](?:[-a-z0-9]*[a-z0-9])?(?:\.[a-z0-9](?:[-a-z0-9]*[a-z0-9])?)+:)?[a-z][-a-z0-9]{4,28}[a-z0-9]:[a-z]+-[a-z]+[0-9]+:[a-z](?:[-a-z0-9]{0,96}[a-z0-9])?$`)
)

// DispatchArgs are the inputs of an envoy-infra:gcp:Dispatch. Every field of a plain Go type is a
// value the consumer's program knows when it runs; the pulumi.*Input fields may come from another
// resource or stack.
type DispatchArgs struct {
	Region   string `pulumi:"region"`
	Hostname string `pulumi:"hostname"`
	// Every input from tailnetDnsName through signInDomain configures Dispatch itself, and is
	// optional in the schema while the resolver is what requires it: a generated Go SDK emits a
	// nil check in its constructor for every required input whose type can be nil, so a
	// cluster-only consumer, which sets none of these, could not call the component at all while
	// they stayed required. With clusterOnly off, four of the twelve — the digest-pinned images
	// envoyImage and cloudSqlProxyImage, natsUrl and natsSeedSecret — every deployment sets, and
	// newDispatchSettings refuses each one missing by name, in the wording it always used. The
	// other eight are two groups, each set whole or left wholly unset: the tailnet group
	// (dispatchTailnetInputs), which builds the front door and serves Dispatch at its tailnet
	// name, and the sign-in group (dispatchSignInInputs), which runs people's browser sign-in.
	// Both unset, with publicOrigin unset too, is the sandbox, which the resolver accepts; the
	// tailnet group set with the sign-in group unset is a tailnet Dispatch that runs no browser
	// sign-in, which it accepts too. A sign-in input sent with the tailnet group and publicOrigin
	// unset is refused for being sent at all (refuseDispatchSandbox). Otherwise a group set in
	// part is refused by the per-input refusal it always gave, trailing one clause naming every
	// input of that group still unset (dispatchGroupClause). With clusterOnly on the resolver
	// refuses each of the twelve that is set.
	TailnetDnsName string             `pulumi:"tailnetDnsName,optional"`
	Tags           []string           `pulumi:"tags,optional"`
	TailnetAuthKey pulumi.StringInput `pulumi:"tailnetAuthKey,optional" provider:"secret"`

	EnvoyImage         string `pulumi:"envoyImage,optional"`
	TailscaleImage     string `pulumi:"tailscaleImage,optional"`
	CloudSqlProxyImage string `pulumi:"cloudSqlProxyImage,optional"`
	// FrontDoorToolsImage is the image the front door's cert-publish container runs on, pinned by
	// digest: it needs /bin/sh, curl, openssl 3, base64, GNU date, sed, grep and tr, which the envoy
	// image carries. An input of its own, so a release of the app's image never rolls the front door.
	FrontDoorToolsImage string `pulumi:"frontDoorToolsImage,optional"`

	NatsUrl        pulumi.StringInput `pulumi:"natsUrl,optional"`
	NatsSeedSecret pulumi.StringInput `pulumi:"natsSeedSecret,optional"`

	SignInClientId     string             `pulumi:"signInClientId,optional"`
	SignInClientSecret pulumi.StringInput `pulumi:"signInClientSecret,optional" provider:"secret"`
	SignInDomain       string             `pulumi:"signInDomain,optional"`
	// SignInWorkspaceDomains are the other domains the Google Workspace owns beside signInDomain,
	// rendered comma-joined, in the order given, as DISPATCH_SIGNIN_WORKSPACE_DOMAINS, which Dispatch
	// reads to tell a Workspace address from an outside one: it treats an address on any of them, or
	// on a subdomain of one, as the Workspace's, which no invitation or outside sign-in may use.
	// Dispatch reads it only with Google sign-in, so it needs the sign-in inputs and is no member of
	// their group; it is refused in the sandbox and beside iapAudience. Each entry is held to
	// Dispatch's own rule for the setting (workspaceDomainRule). Unset renders nothing.
	SignInWorkspaceDomains []string `pulumi:"signInWorkspaceDomains,optional"`
	// OutsideOrigin is a second origin Dispatch is reached at, by people from outside the
	// company: an https origin weborigin.Parse takes, whose host is not that of Dispatch's own
	// origin, the front door's or publicOrigin. It renders DISPATCH_OUTSIDE_ORIGIN and nothing
	// else: whatever serves that origin and forwards it to the app is the deployment's, and the
	// app's NetworkPolicy admits an in-cluster one only through allowedNamespaces. It needs the
	// tailnet inputs or publicOrigin: without either the component builds the sandbox, whose own
	// origin is an in-cluster http:// address where browser sign-in is refused. With the sign-in
	// inputs set its host must also be one weborigin.GoogleRedirectHost takes: the Google client
	// then lists its callback, or, with signInPasskey, the host is the passkey's relying party ID,
	// a domain name. Unset renders nothing.
	OutsideOrigin string `pulumi:"outsideOrigin,optional"`
	// SignInOutside renders DISPATCH_SIGNIN_OUTSIDE=1, which lets a verified Google account outside
	// signInDomain sign in as a restricted member while some project's owner or access list names
	// its email directly. It needs the sign-in inputs and is no member of their group, so a
	// deployment that sets them leaves it unset and renders what it always rendered.
	SignInOutside bool `pulumi:"signInOutside,optional"`
	// SignInPasskey renders DISPATCH_SIGNIN_PASSKEY=1, which turns on Dispatch's passkey sign-in at
	// outsideOrigin for people from outside the company: the passkey's relying party is that origin
	// and its relying party ID that origin's host, so it needs outsideOrigin and renders right after
	// DISPATCH_OUTSIDE_ORIGIN. It needs the sign-in inputs too, though it is no member of their
	// group: a person from outside enrolls a passkey with an invitation a project owner makes while
	// signed in through Google, so without them nobody could invite anyone. It replaces the Google
	// sign-in of people from outside that signInOutside turns on, so it is refused beside
	// signInOutside. It is refused beside iapAudience, since IAP signs a person in, and in the
	// sandbox, which serves no https origin. With it the Google client lists no
	// <outsideOrigin>/auth/callback, which Dispatch then answers 404; outsideOrigin's host is still
	// held to weborigin.GoogleRedirectHost, which keeps it a domain name, never an IP address, as a
	// relying party ID must be. A deployment that sets it lists every other domain its Workspace
	// owns in signInWorkspaceDomains: Dispatch takes an address on a domain left out for an outside
	// person's, so a Workspace account there could hold a passkey, which Workspace offboarding does
	// not end. Unset renders nothing.
	SignInPasskey bool `pulumi:"signInPasskey,optional"`
	// PublicOrigin is the https origin Dispatch is served at when it has no front door: the
	// address a reader is sent back to, DISPATCH_SERVER_URL and the url output, served by an
	// entrance the deployment builds itself, such as a load balancer, which forwards to the app's
	// Service <hostname> on 8766. The app's pods (labelled app=<hostname>) admit that entrance by
	// loadBalancerSourceRanges, which the component renders into the app's own NetworkPolicy, or by
	// a NetworkPolicy of the deployment's own, since policies add up. It is an origin
	// weborigin.Parse takes, https, and with the sign-in inputs set its host is one
	// weborigin.GoogleRedirectHost takes, since the Google client then lists
	// <publicOrigin>/auth/callback. It is refused beside the tailnet inputs, whose front door's
	// name is the origin of a Dispatch on the tailnet; a second origin beside that one is
	// outsideOrigin. With it the sign-in inputs, signInWorkspaceDomains, signInOutside,
	// outsideOrigin and signInPasskey are taken without the tailnet inputs, natsInCluster is
	// required, as it is in the sandbox, and allowedNamespaces is not, since the entrance admits its
	// own traffic. Unset, Dispatch is reached as it always was: at the front door's name or, in the
	// sandbox, inside its cluster.
	PublicOrigin string `pulumi:"publicOrigin,optional"`
	// IapAudience turns on the IAP door: a browser reaches Dispatch through Identity-Aware Proxy
	// on the deployment's load balancer, and Dispatch names its person from the signed header IAP
	// adds to each request it lets through (DISPATCH_IDENTITY=iap), which Dispatch verifies
	// itself, holding the token's aud claim to this value (DISPATCH_IAP_AUDIENCE). It is the
	// audience Google documents for a load balancer's backend service,
	// /projects/<project number>/global/backendServices/<backend service id>
	// (https://cloud.google.com/iap/docs/signed-headers-howto), and may be another resource's
	// output, such as one built from the backend service's own id. It needs publicOrigin, the
	// origin that load balancer serves, and signInDomain, the Workspace domain whose accounts may
	// use Dispatch, rendered as DISPATCH_SIGNIN_DOMAIN, which Dispatch holds the signed header's hd
	// claim to; it is refused beside signInClientId, signInClientSecret, signInWorkspaceDomains,
	// signInOutside, signInPasskey and outsideOrigin: IAP signs a person in, and a browser reaches
	// Dispatch through it alone. Unset renders none of the three variables, and Dispatch keeps its
	// default identity.
	IapAudience pulumi.StringInput `pulumi:"iapAudience,optional"`
	// LoadBalancerSourceRanges are the CIDRs the deployment's load balancer reaches the app's pods
	// from, such as Google's health-check and proxy ranges, which the component renders as ipBlock
	// peers on 8766 in the app's own NetworkPolicy, beside allowedNamespaces' peers, so one policy
	// selects the Dispatch pods. Each is a CIDR net.ParseCIDR takes, written as its network
	// (10.0.0.0/8, not 10.1.2.3/8), with a prefix longer than /0: 0.0.0.0/0 or ::/0 would admit
	// every address to 8766, past the load balancer and IAP alike. It needs publicOrigin, the
	// origin that load balancer serves. Unset renders no such peer.
	LoadBalancerSourceRanges []string `pulumi:"loadBalancerSourceRanges,optional"`
	// NetworkEndpointGroupName names the standalone network endpoint group GKE makes for the app's
	// Service on 8766, which the deployment's load balancer takes as its backend: the Service gets
	// the annotation cloud.google.com/neg: {"exposed_ports":{"8766":{"name":"<name>"}},"zones":["*"]}
	// (https://cloud.google.com/kubernetes-engine/docs/how-to/standalone-neg). "zones":["*"] has GKE
	// pre-provision the group in every zone of the cluster's region, empty until a pod runs there,
	// so the load balancer's backend can name it in each zone; that needs GKE 1.36.2-gke.3104000 or
	// later, a Preview feature. It is a Compute Engine resource name, a lowercase letter, then up to
	// 62 lowercase letters, digits or hyphens, not ending in a hyphen. It needs publicOrigin. Unset
	// renders no annotation.
	NetworkEndpointGroupName string `pulumi:"networkEndpointGroupName,optional"`

	// AgentSecretsUrl and agentSecretsAudience turn on Dispatch's secrets-broker relay together:
	// DISPATCH_AGENT_SECRETS_URL (an absolute http(s) URL with no path; a trailing slash is
	// trimmed, as Dispatch trims it) and the audience of the projected ServiceAccount token the
	// app presents to the broker on every call, read at call time from the file
	// DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE names. AgentSecretsAudience is the broker's
	// BROKER_K8S_OIDC_AUDIENCE: the kubelet mints the token for it and refreshes it in place, so
	// no token is an input, a stored secret or part of the component's state. Set both or neither.
	AgentSecretsUrl      string `pulumi:"agentSecretsUrl,optional"`
	AgentSecretsAudience string `pulumi:"agentSecretsAudience,optional"`

	// OidcAudience turns on Dispatch's own service-account verifier, the one the secrets
	// broker's judge reads Dispatch with: DISPATCH_OIDC_AUDIENCE is the audience a projected
	// ServiceAccount token must carry, and DISPATCH_OIDC_ISSUER the issuer that signed it,
	// which defaults to the cluster Dispatch runs in, the component's own or the one
	// existingCluster names (the serviceAccountIssuer output), and which oidcIssuer replaces
	// for a token another cluster mints. Dispatch reads the two as a pair
	// (oidc.ConfigFromEnv refuses half of one), so the component renders both or neither, and
	// an oidcIssuer with no oidcAudience is refused.
	//
	// Turning this on opens Dispatch to every workload that can get a token for that audience
	// under that issuer: such a caller reaches every one of Dispatch's authAny routes, the
	// writes among them (an issue's create and status, a message), as a service actor, and a
	// service actor holds no scope, so with oidcAccounts unset Dispatch admits whatever subject
	// the token names (serviceTokenActor, internal/dispatch/api/server.go) and only the
	// audience bounds it. Pick one no other workload of the cluster asks the kubelet for, set
	// oidcAccounts to the subjects that call, and leave the audience unset where no caller
	// needs it.
	OidcAudience string `pulumi:"oidcAudience,optional"`
	OidcIssuer   string `pulumi:"oidcIssuer,optional"`
	// OidcAccounts is DISPATCH_OIDC_ACCOUNTS, comma-joined: the Kubernetes service-account
	// subjects, each system:serviceaccount:<namespace>:<name>, whose tokens the verifier admits.
	// Unset or empty renders nothing, and Dispatch admits every account of the issuer and the
	// audience, as it did before the input existed. It narrows the verifier oidcAudience turns
	// on, so it is refused without oidcAudience, as oidcIssuer is.
	OidcAccounts []string `pulumi:"oidcAccounts,optional"`
	// AccessAccounts is DISPATCH_ACCESS_ACCOUNTS, comma-joined: the service-account subjects that
	// may read Dispatch's membership answer (GET /api/v1/access/projects/{key}, GET
	// /api/v1/access/person and GET /api/v1/access/private-projects), the secrets broker's
	// system:serviceaccount:dispatch:envoy-broker in production. Where oidcAccounts is set,
	// Dispatch refuses to boot with an access account it leaves out; with oidcAccounts unset, every
	// account is admitted and any may be an access account. Unset or empty renders nothing, and no
	// account reads the answer. It is refused without oidcAudience, as oidcIssuer is.
	AccessAccounts []string `pulumi:"accessAccounts,optional"`

	// ProjectOwnerFallback is DISPATCH_PROJECT_OWNER_FALLBACK: the lowercase email of a full
	// member, whom Dispatch makes, at boot, the owner of every project the migration left without
	// one. Unset renders nothing.
	ProjectOwnerFallback string `pulumi:"projectOwnerFallback,optional"`
	// PrivateProjects true renders DISPATCH_PRIVATE_PROJECTS=1; false or unset renders nothing.
	// No envoy-dispatch reads the setting yet, so true makes no project private, while the stack
	// would say it had: leave it unset.
	PrivateProjects *bool `pulumi:"privateProjects,optional"`

	// AppClientId, appId, appPrivateKeySecretName and appPrivateKeySecretKey turn on Dispatch's
	// GitHub App together: DISPATCH_APP_CLIENT_ID and DISPATCH_APP_ID as plain values, and
	// DISPATCH_APP_PEM_B64 read from the key AppPrivateKeySecretKey of the Kubernetes Secret
	// AppPrivateKeySecretName in the component's namespace. That Secret is created at run time
	// (GKE SecretSync), never by this component, and the key never enters it or its state. Set
	// all four or none.
	AppClientId             string `pulumi:"appClientId,optional"`
	AppId                   string `pulumi:"appId,optional"`
	AppPrivateKeySecretName string `pulumi:"appPrivateKeySecretName,optional"`
	AppPrivateKeySecretKey  string `pulumi:"appPrivateKeySecretKey,optional"`

	DatabaseVersion    *string `pulumi:"databaseVersion,optional"`
	DatabaseTier       *string `pulumi:"databaseTier,optional"`
	DeletionProtection *bool   `pulumi:"deletionProtection,optional"`
	// DatabaseAvailabilityType is the availability type of the Cloud SQL instance the component
	// makes: ZONAL, one zone and the default, or REGIONAL, a standby in a second zone that Cloud
	// SQL fails over to. Switching changes the instance in place, restarting it once.
	DatabaseAvailabilityType *string `pulumi:"databaseAvailabilityType,optional"`
	// DatabaseWaitsFor is a value of the resource whose update must come before the update of the
	// Cloud SQL instance the component makes, so that a step of the consumer's own — deleting the
	// old tailnet device before the front door comes back, for instance — runs before the instance
	// restarts. The component never reads the value, and it reaches no resource: only the
	// resources the value came from are used, which is what pulumi.DependsOnInputs reads off it.
	// Unset, the instance's ordering is what it is today.
	DatabaseWaitsFor pulumi.StringInput `pulumi:"databaseWaitsFor,optional"`
	Namespace        *string            `pulumi:"namespace,optional"`
	// ExistingNamespace: use the namespace as it exists in the cluster rather than creating it
	// (another program owns the namespace).
	ExistingNamespace *bool `pulumi:"existingNamespace,optional"`
	// ExistingCluster names a GKE cluster the component does not make: the component runs
	// Dispatch in that cluster and registers no Cluster of its own. The cluster must run on the
	// component's own network, Network <hostname>-network, which the component registers whether
	// or not it makes a cluster: the Cloud SQL proxy reaches the instance's private address on that
	// network. It is a plain object, not a Pulumi Input, because the component looks the cluster up
	// through the gcp provider before it registers anything and builds its Kubernetes provider's
	// kubeconfig from what the lookup reports: an Input would make the kubeconfig unknown whenever
	// the cluster's name is, and pulumi-kubernetes replaces its provider, and so every Kubernetes
	// child, whenever the kubeconfig is unknown. The lookup refuses a cluster Dispatch cannot run
	// in (lookupDispatchCluster). Unset, the component makes the Autopilot cluster <hostname>.
	ExistingCluster *DispatchClusterArgs `pulumi:"existingCluster,optional"`
	// ExistingDatabaseInstance is the connection name, <project>:<region>:<instance>, of a Cloud
	// SQL instance the component does not make: the form of the databaseInstance output, so a
	// consumer can pass its own replica's connectionName. With it the component registers no
	// DatabaseInstance, Database or User, and the app pod's cloud-sql-proxy sidecar connects to
	// that instance. It keeps every RandomPassword, under the names and with the arguments it
	// always had, so the contract is that the instance holds database dispatch and login dispatch,
	// whose password is the component's generated one: a promoted replica of the component's own
	// instance does. It is a Pulumi Input because a consumer takes it from another resource or
	// stack, and it is checked for a seed and for its shape once it resolves
	// (dispatchDatabaseConnectionName). databaseVersion, databaseTier, databaseAvailabilityType and
	// databaseWaitsFor configure the instance the component makes, and are refused beside it.
	// Unset, the component makes the Cloud SQL instance <hostname>.
	ExistingDatabaseInstance pulumi.StringInput `pulumi:"existingDatabaseInstance,optional"`
	// RegionalNetwork is the regional part of the component's network: Subnetwork, Router and
	// RouterNat <hostname> in region, the subnetwork the component's own cluster takes its
	// addresses from and the router and Cloud NAT its private nodes reach the internet through.
	// True when unset. False registers none of the three, while the Network, its GlobalAddress and
	// its private services Connection stay exactly as they are, so the Cloud SQL instance keeps its
	// private address. It is the step that empties the region once nothing uses the subnetwork, and
	// so is allowed only beside existingCluster: the component's own cluster runs on that
	// subnetwork, and false without existingCluster is refused rather than leaving it none.
	RegionalNetwork *bool `pulumi:"regionalNetwork,optional"`

	// ClusterOnly builds the cluster alone: the VPC, its subnetwork, the router, Cloud NAT and an
	// Autopilot cluster of the component's own, and nothing else. No Cloud SQL instance, database
	// or user, no generated password, no Secret Manager grant, no Kubernetes provider and no
	// Kubernetes object is registered, and the component requires no random provider. Every input
	// that configures Dispatch rather than the cluster is refused by name, existingCluster and
	// existingDatabaseInstance among them, since this mode always makes its own cluster and uses
	// no database. So only region, hostname, deletionProtection and regionalNetwork come with it:
	// regionalNetwork shapes the network half both modes build, and since this mode always makes
	// its own cluster on that subnetwork, true (or unset) changes nothing and false is refused.
	// clusterName, kubeconfig and serviceAccountIssuer carry the cluster's own values and url,
	// databaseInstance, principal, agentToken and serviceAccountSubject are "". A stack already
	// running Dispatch cannot turn it on: such a stack sets the inputs this mode refuses, so the
	// update is refused before anything is deleted.
	ClusterOnly *bool `pulumi:"clusterOnly,optional"`

	// Replicas is the app Deployment's replica count, 1 by default. Above 1 the Deployment rolls
	// its pods instead of being recreated.
	Replicas *int `pulumi:"replicas,optional"`
	// FrontDoorReplicas is the front door's (<hostname>-tailnet) replica count, 2 by default, and
	// ListenerReplicas the listener's (<hostname>-listener), 1 by default; each at least 1. Each
	// Deployment is recreated at one replica and rolls above one, by its own count.
	FrontDoorReplicas *int `pulumi:"frontDoorReplicas,optional"`
	ListenerReplicas  *int `pulumi:"listenerReplicas,optional"`

	// NatsInCluster: the app and the listener dial natsUrl directly, at an address their own
	// cluster resolves, rather than through the front door's SOCKS5 proxy. Neither NATS client
	// carries ENVOY_NATS_SOCKS5_PROXY then, and the component renders no SOCKS5 proxy on the front
	// door's pods, no port-1055 Service and no ingress rule admitting the two workloads to it.
	// Unset, the SOCKS5 path renders as it does today, for a deployment whose NATS hub is outside
	// this cluster.
	NatsInCluster *bool `pulumi:"natsInCluster,optional"`
	// NatsReplicas is the number of copies JetStream keeps of every stream and bucket, from 1 to
	// bus.MaxReplicas: ENVOY_NATS_REPLICAS on the app's and the listener's containers, which every
	// Envoy program of a deployment carries alike. It is not replicas, the app Deployment's pod
	// count above: more than one copy needs a clustered NATS, which no pod count gives. Unset
	// renders nothing and both binaries keep one copy (bus.ParseNATSReplicas).
	NatsReplicas *int `pulumi:"natsReplicas,optional"`
	// StreamMaxBytes is the stream's byte limit, at least 1: ENVOY_STREAM_MAX_BYTES on the same two
	// containers. A deployed limit is never lowered, so programs carrying different limits leave
	// the largest. Unset renders nothing and both binaries set no limit, so a deployed stream keeps
	// the one it has (bus.ParseStreamMaxBytes).
	StreamMaxBytes *int `pulumi:"streamMaxBytes,optional"`
	// SenderMessagesPerMinute and SenderMessageBurst limit the messages each agent session sends
	// through the listener: ENVOY_SENDER_MESSAGES_PER_MINUTE, from 1 to 60000, and
	// ENVOY_SENDER_MESSAGE_BURST, at least 1 and only with a rate, on the listener's container
	// alone, since the app reads neither. Each of the listenerReplicas pods keeps its own count.
	// Unset renders nothing: no limit, and a rate with no burst takes the rate as its burst.
	SenderMessagesPerMinute *int `pulumi:"senderMessagesPerMinute,optional"`
	SenderMessageBurst      *int `pulumi:"senderMessageBurst,optional"`

	// SigningKeyGenerations is the ordered key set the server holds, the shape
	// auth.ParseSigningKeys reads DISPATCH_SIGNING_KEY into: the first entry signs every new
	// cookie and seals every new refresh token, and every entry verifies a cookie and opens a
	// seal. Each entry is a generation, which names one generated key: generation 0 is
	// <hostname>-signing-key, the key a running deployment already holds, and generation n above
	// 0 is <hostname>-signing-key-<n>. Unset is [0], the single key this component has always
	// registered. Only the generations the list names are registered, so one it drops is
	// destroyed and that key cannot come back. A rotation is three applies, nobody signed out:
	// [0] to [0, 1] adds the new key, which every process then verifies; [0, 1] to [1, 0] makes
	// it the key every new cookie and seal is made with; and [1, 0] to [1] drops the old key once
	// nothing made under it is left.
	SigningKeyGenerations []int `pulumi:"signingKeyGenerations,optional"`

	// AppCpu and appMemory size the app Deployment's envoy-dispatch container, as Kubernetes
	// quantities set as both its request and its limit: 1 CPU and 4Gi by default. The pod's
	// cloud-sql-proxy sidecar is sized by proxyCpu and proxyMemory, and the front door and the
	// listener are unchanged.
	AppCpu    *string `pulumi:"appCpu,optional"`
	AppMemory *string `pulumi:"appMemory,optional"`
	// ProxyCpu and proxyMemory size the app pod's cloud-sql-proxy sidecar, through whose TLS every
	// database call of the pod passes, as Kubernetes quantities set as both its request and its
	// limit: 1 CPU and 2Gi by default, sized for one server pod's connection pools. Autopilot's
	// bounds apply to the pod's total, envoy-dispatch's and the proxy's together.
	ProxyCpu    *string `pulumi:"proxyCpu,optional"`
	ProxyMemory *string `pulumi:"proxyMemory,optional"`
	// AppEphemeralStorage and proxyEphemeralStorage are the ephemeral-storage request and limit,
	// as Kubernetes quantities, of envoy-dispatch and of the cloud-sql-proxy sidecar: the disk a
	// container's writable layer, temporary files and logs may use before the kubelet evicts the
	// pod. Each unset renders nothing, and Autopilot gives that container its default 1Gi.
	// Autopilot's bounds apply to the pod's total, the two containers' together.
	AppEphemeralStorage   *string `pulumi:"appEphemeralStorage,optional"`
	ProxyEphemeralStorage *string `pulumi:"proxyEphemeralStorage,optional"`

	// PprofPort turns on envoy-dispatch's Go profiler: DISPATCH_PPROF_PORT on its container alone,
	// so it serves net/http/pprof on 127.0.0.1 at this port, reachable through kubectl
	// port-forward and from nowhere outside the pod. Unset, nothing is rendered and no profiler
	// listens.
	PprofPort *int `pulumi:"pprofPort,optional"`

	// PriorityClassName is the priorityClassName of every pod of the three Deployments (the app,
	// the front door and the listener), the name of a PriorityClass that already exists in the
	// cluster, which this component reads once but never creates: a missing class fails the
	// preview or update before any Deployment changes, since Kubernetes refuses every pod naming
	// one. Unset, the pods name none and run at the cluster's default priority.
	PriorityClassName *string `pulumi:"priorityClassName,optional"`

	// AppPdbMinAvailable turns on a PodDisruptionBudget, <hostname>, over the app's pods: its
	// minAvailable, from 1 to replicas. FrontDoorPdbMinAvailable turns on <hostname>-tailnet over
	// the front door's pods, from 1 to frontDoorReplicas, and ListenerPdbMinAvailable
	// <hostname>-listener over the listener's, from 1 to listenerReplicas. Unset, the component
	// creates no budget for that Deployment, and its pods carry the cluster autoscaler's
	// safe-to-evict "false" annotation instead.
	AppPdbMinAvailable       *int `pulumi:"appPdbMinAvailable,optional"`
	FrontDoorPdbMinAvailable *int `pulumi:"frontDoorPdbMinAvailable,optional"`
	ListenerPdbMinAvailable  *int `pulumi:"listenerPdbMinAvailable,optional"`

	// AllowedNamespaces names the Kubernetes namespaces, in the Dispatch pod's own cluster, whose
	// pods the app's NetworkPolicy admits on port 8766. Unset or empty admits no in-cluster
	// namespace: only the component's own tailnet front door, which Tailscale Serve on 443 proxies
	// through, reaches the app.
	AllowedNamespaces []string `pulumi:"allowedNamespaces,optional"`
	// AllowedApps names the apps, by their pods' app label, whose pods in the component's own
	// namespace the app's NetworkPolicy admits on port 8766: one rule per app, beside the
	// allowedNamespaces rule. The secrets broker, envoy-broker in production, runs in Dispatch's
	// own namespace, which an allowedNamespaces entry would open to every pod. Unset or empty
	// admits no such pod.
	AllowedApps []string `pulumi:"allowedApps,optional"`

	// AzureEmbedEndpoint turns on meaning search through Azure AI Foundry: DISPATCH_EMBEDDER=azure
	// and DISPATCH_AZURE_EMBED_ENDPOINT, the base URL the embeddings route is appended to (such as
	// https://<resource>.services.ai.azure.com/models), which may be an Azure resource's output.
	// AzureEmbedTokensPerMinute, the deployment's quota, comes with it, and AzureEmbedDeployment
	// (DISPATCH_AZURE_EMBED_DEPLOYMENT, Dispatch's default when unset) may. Every other azureEmbed
	// input needs the endpoint.
	AzureEmbedEndpoint        pulumi.StringInput `pulumi:"azureEmbedEndpoint,optional"`
	AzureEmbedDeployment      pulumi.StringInput `pulumi:"azureEmbedDeployment,optional"`
	AzureEmbedTokensPerMinute *int               `pulumi:"azureEmbedTokensPerMinute,optional"`

	// AzureChatEndpoint turns on the model Dispatch generates text with, a chat deployment of an
	// Azure AI Foundry resource: DISPATCH_ANSWERER=azure and DISPATCH_AZURE_CHAT_ENDPOINT, the
	// base URL /chat/completions is appended to, which may be an Azure resource's output.
	// AzureChatDeployment (DISPATCH_AZURE_CHAT_DEPLOYMENT, the deployment's name, which each
	// request sends as its model) and AzureChatTokensPerMinute (the deployment's quota, a positive
	// integer, which paces the answerer's calls) come with it; AzureChatApiVersion (the api-version
	// query value each request carries; unset, a request sends none) and AzureChatScope (the token
	// scope the federated sign-in asks for; unset, Dispatch's own default) may. The chat form takes
	// no API key: it signs in with the shared keyless credential below, so AzureTenantId and
	// AzureClientId come with it. Every other azureChat input needs the endpoint.
	AzureChatEndpoint        pulumi.StringInput `pulumi:"azureChatEndpoint,optional"`
	AzureChatDeployment      pulumi.StringInput `pulumi:"azureChatDeployment,optional"`
	AzureChatApiVersion      *string            `pulumi:"azureChatApiVersion,optional"`
	AzureChatScope           *string            `pulumi:"azureChatScope,optional"`
	AzureChatTokensPerMinute *int               `pulumi:"azureChatTokensPerMinute,optional"`

	// ChatEndpoint is the answerer's other form, any OpenAI-shaped chat completions service:
	// DISPATCH_ANSWERER=endpoint and DISPATCH_CHAT_ENDPOINT, the base URL /chat/completions is
	// appended to. ChatModel (DISPATCH_CHAT_MODEL, the model each request names) and
	// ChatTokenSecret come with it, and the form is refused together with AzureChatEndpoint: one
	// deployment names one answerer. ChatTokenSecret is the Secret Manager secret's resource path
	// (projects/<project>/secrets/<id>) whose value is the bearer token every call to that endpoint
	// presents, the one secret of this group: it is mounted through the Secret Manager CSI driver
	// as the file DISPATCH_CHAT_TOKEN_FILE names, which Dispatch re-reads on every call, so the
	// token never enters the component or its state. Every other chat input needs the endpoint.
	ChatEndpoint    pulumi.StringInput `pulumi:"chatEndpoint,optional"`
	ChatModel       *string            `pulumi:"chatModel,optional"`
	ChatTokenSecret pulumi.StringInput `pulumi:"chatTokenSecret,optional" provider:"secret"`

	// The embedder's endpoint takes exactly one credential. AzureEmbedKeySecret is the key form:
	// the Secret Manager secret's resource path (projects/<project>/secrets/<id>) holding the
	// deployment's API key, mounted through the Secret Manager CSI driver as the file
	// DISPATCH_AZURE_EMBED_KEY_FILE names, so the key never enters the component or its state.
	// AzureTenantId and AzureClientId are the keyless form, set together (a managed identity's
	// outputs), and they are shared: whichever of the embedder and the answerer is on Azure reads
	// them, so either endpoint accepts them and a deployment that embeds on Bedrock and answers on
	// Azure sets them for the answerer. The pod's projected service-account token, for audience
	// AzureFederationAudience (api://AzureADTokenExchange when unset), is the assertion Dispatch
	// exchanges with Entra, through the federated credential the identity holds for this
	// component's serviceAccountIssuer and serviceAccountSubject.
	AzureEmbedKeySecret     pulumi.StringInput `pulumi:"azureEmbedKeySecret,optional"`
	AzureTenantId           pulumi.StringInput `pulumi:"azureTenantId,optional"`
	AzureClientId           pulumi.StringInput `pulumi:"azureClientId,optional"`
	AzureFederationAudience *string            `pulumi:"azureFederationAudience,optional"`
}

// DispatchClusterArgs name the GKE cluster existingCluster runs Dispatch in, as the gcp provider's
// getCluster takes it: the cluster's name and its location.
type DispatchClusterArgs struct {
	// Name is the cluster's name: a lowercase letter, then lowercase letters, digits and hyphens,
	// ending in a letter or digit, at most 40 characters.
	Name string `pulumi:"name"`
	// Location is the region (us-central1) or the zone (us-central1-a) the cluster runs in.
	Location string `pulumi:"location"`
}

// Annotate names the type gcp:DispatchCluster in the schema, for the reason NATSUserArgs's
// gives.
func (c *DispatchClusterArgs) Annotate(a infer.Annotator) { a.SetToken("gcp", "DispatchCluster") }

// dispatchSeededInput names the first plain string input that holds an nkey seed. No component
// input takes an nkey seed here: each value reaches a cloud resource, a Kubernetes object, or the
// pod configuration, so a seed would be disclosed or used as the wrong credential. An int input,
// such as replicas, cannot carry an nkey seed, so it takes no row here.
func dispatchSeededInput(args *DispatchArgs) (string, bool) {
	for _, value := range []struct {
		input string
		value string
	}{
		{"region", args.Region},
		{"hostname", args.Hostname},
		{"tailnetDnsName", args.TailnetDnsName},
		{"envoyImage", args.EnvoyImage},
		{"tailscaleImage", args.TailscaleImage},
		{"cloudSqlProxyImage", args.CloudSqlProxyImage},
		{"frontDoorToolsImage", args.FrontDoorToolsImage},
		{"signInClientId", args.SignInClientId},
		{"signInDomain", args.SignInDomain},
		{"outsideOrigin", args.OutsideOrigin},
		{"publicOrigin", args.PublicOrigin},
		{"networkEndpointGroupName", args.NetworkEndpointGroupName},
		{"agentSecretsUrl", args.AgentSecretsUrl},
		{"agentSecretsAudience", args.AgentSecretsAudience},
		{"oidcAudience", args.OidcAudience},
		{"oidcIssuer", args.OidcIssuer},
		{"projectOwnerFallback", args.ProjectOwnerFallback},
		{"appClientId", args.AppClientId},
		{"appId", args.AppId},
		{"appPrivateKeySecretName", args.AppPrivateKeySecretName},
		{"appPrivateKeySecretKey", args.AppPrivateKeySecretKey},
	} {
		if bus.HoldsSeed(value.value) {
			return value.input, true
		}
	}
	for i, tag := range args.Tags {
		if bus.HoldsSeed(tag) {
			return fmt.Sprintf("tags[%d]", i), true
		}
	}
	for i, domain := range args.SignInWorkspaceDomains {
		if bus.HoldsSeed(domain) {
			return fmt.Sprintf("signInWorkspaceDomains[%d]", i), true
		}
	}
	if args.DatabaseVersion != nil && bus.HoldsSeed(*args.DatabaseVersion) {
		return "databaseVersion", true
	}
	if args.DatabaseTier != nil && bus.HoldsSeed(*args.DatabaseTier) {
		return "databaseTier", true
	}
	if args.DatabaseAvailabilityType != nil && bus.HoldsSeed(*args.DatabaseAvailabilityType) {
		return "databaseAvailabilityType", true
	}
	if args.AppCpu != nil && bus.HoldsSeed(*args.AppCpu) {
		return "appCpu", true
	}
	if args.AppMemory != nil && bus.HoldsSeed(*args.AppMemory) {
		return "appMemory", true
	}
	if args.ProxyCpu != nil && bus.HoldsSeed(*args.ProxyCpu) {
		return "proxyCpu", true
	}
	if args.ProxyMemory != nil && bus.HoldsSeed(*args.ProxyMemory) {
		return "proxyMemory", true
	}
	if args.AppEphemeralStorage != nil && bus.HoldsSeed(*args.AppEphemeralStorage) {
		return "appEphemeralStorage", true
	}
	if args.ProxyEphemeralStorage != nil && bus.HoldsSeed(*args.ProxyEphemeralStorage) {
		return "proxyEphemeralStorage", true
	}
	if args.PriorityClassName != nil && bus.HoldsSeed(*args.PriorityClassName) {
		return "priorityClassName", true
	}
	if args.Namespace != nil && bus.HoldsSeed(*args.Namespace) {
		return "namespace", true
	}
	for i, namespace := range args.AllowedNamespaces {
		if bus.HoldsSeed(namespace) {
			return fmt.Sprintf("allowedNamespaces[%d]", i), true
		}
	}
	for _, list := range []struct {
		input  string
		values []string
	}{
		{"loadBalancerSourceRanges", args.LoadBalancerSourceRanges},
		{"oidcAccounts", args.OidcAccounts},
		{"accessAccounts", args.AccessAccounts},
		{"allowedApps", args.AllowedApps},
	} {
		for i, value := range list.values {
			if bus.HoldsSeed(value) {
				return fmt.Sprintf("%s[%d]", list.input, i), true
			}
		}
	}
	if args.AzureFederationAudience != nil && bus.HoldsSeed(*args.AzureFederationAudience) {
		return "azureFederationAudience", true
	}
	if args.AzureChatApiVersion != nil && bus.HoldsSeed(*args.AzureChatApiVersion) {
		return "azureChatApiVersion", true
	}
	if args.AzureChatScope != nil && bus.HoldsSeed(*args.AzureChatScope) {
		return "azureChatScope", true
	}
	if args.ChatModel != nil && bus.HoldsSeed(*args.ChatModel) {
		return "chatModel", true
	}
	if args.ExistingCluster != nil {
		if bus.HoldsSeed(args.ExistingCluster.Name) {
			return "existingCluster.name", true
		}
		if bus.HoldsSeed(args.ExistingCluster.Location) {
			return "existingCluster.location", true
		}
	}
	return "", false
}

// Dispatch is Dispatch on GKE with Cloud SQL: on an Autopilot cluster of its own, or in the
// cluster existingCluster names, and on a Cloud SQL instance of its own, or on the one
// existingDatabaseInstance names, the two choices independent of each other. It is reached over a
// tailnet, at a public origin the deployment's own entrance serves, or inside its cluster alone.
type Dispatch struct {
	pulumi.ResourceState
	Url              pulumi.StringOutput `pulumi:"url"`
	ClusterName      pulumi.StringOutput `pulumi:"clusterName"`
	DatabaseInstance pulumi.StringOutput `pulumi:"databaseInstance"`
	Kubeconfig       pulumi.StringOutput `pulumi:"kubeconfig"`
	Principal        pulumi.StringOutput `pulumi:"principal"`
	AgentToken       pulumi.StringOutput `pulumi:"agentToken" provider:"secret"`
	// ServiceAccountIssuer and ServiceAccountSubject are the issuer and subject of the app's
	// service-account tokens: the cluster's OIDC issuer, and those pods' Kubernetes service
	// account. A federated credential trusting the pair (an Azure identity's, for the keyless form)
	// admits exactly those pods; the listener's and the front door's pods run as accounts of their
	// own.
	ServiceAccountIssuer  pulumi.StringOutput `pulumi:"serviceAccountIssuer"`
	ServiceAccountSubject pulumi.StringOutput `pulumi:"serviceAccountSubject"`
}

// dispatchSettings are the plain inputs, validated, with the defaults applied, so they are known at
// preview. images.tools is frontDoorToolsImage. With clusterOnly set only hostname, region,
// deletionProtection and regionalNetwork are resolved, regionalNetwork because it shapes the
// network half both modes build: no other field is read by the cluster and the network the mode
// builds.
// natsReplicas, streamMaxBytes, senderMessagesPerMinute and senderMessageBurst are 0 when their
// inputs are unset, where the component renders none of those settings, and signingKeyGenerations
// is the generations the component registers a signing key for, in the order the server holds
// them, [0] when its input is unset. frontDoor and signInOn are the two groups of inputs a
// consumer sets together or not at all: frontDoor is set when any tailnet input is, where the
// component builds the front door and Dispatch is reached at its tailnet name, and signInOn when
// any sign-in input is, where the app runs browser sign-in. publicOrigin is the origin Dispatch
// is served at without a front door, "" when its input is unset, and iap is iapAudience set,
// where a browser's person is the one IAP's signed header names; the Input itself is rendered
// from args, since it may be another resource's output.
// With neither group and no publicOrigin the component is the sandbox: Dispatch is reached in the
// cluster alone, by readers the app's NetworkPolicy admits, and tailnetDNSName, tags, the
// tailscale and tools images, outsideOrigin, signIn, signInWorkspaceDomains, signInOutside and
// signInPasskey are empty, since nothing reads them. signInWorkspaceDomains and signInOutside are
// set only with signInOn, outsideOrigin is "" when its input is unset, where the component renders
// no second origin, and signInPasskey is set only with outsideOrigin and signInOn.
// oidcIssuer is "" when its input is unset and oidcAudience is set, where the issuer rendered is
// the cluster's own, which only the cluster half knows (dispatchCluster.issuer) and so is no
// field here. oidcAccounts and accessAccounts are set only beside oidcAudience, and
// projectOwnerFallback "" and privateProjects false render nothing. existingCluster is the input of
// that name, its name and location checked (resolveDispatchExistingCluster), which NewDispatch
// looks the cluster up by and runs Dispatch in, and nil without that input, where the component
// makes a cluster of its own.
// existingDatabaseInstance is true while the input of that name is set, where the component makes
// no Cloud SQL instance, database or user and the proxy connects to the instance it names; the
// connection name itself is a Pulumi Input, judged once it resolves, and so no field here.
// regionalNetwork is true unless the input of that name is false, which both resolvers allow only
// beside existingCluster, where the network half registers no Subnetwork, Router or RouterNat.
type dispatchSettings struct {
	clusterOnly                                 bool
	frontDoor, signInOn                         bool
	hostname, tailnetDNSName, region, namespace string
	outsideOrigin, publicOrigin                 string
	iap                                         bool
	tags                                        []string
	images                                      struct{ envoy, tailscale, proxy, tools string }
	signIn                                      struct{ clientID, domain string }
	signInOutside, signInPasskey                bool
	signInWorkspaceDomains                      []string
	databaseVersion, databaseTier               string
	databaseAvailabilityType                    string
	agentSecretsURL, agentSecretsAudience       string
	oidcAudience, oidcIssuer                    string
	oidcAccounts, accessAccounts                []string
	projectOwnerFallback                        string
	privateProjects                             bool
	appClientID, appID                          string
	appKeySecretName, appKeySecretKey           string
	deletionProtection, existingNamespace       bool
	replicas                                    int
	frontDoorReplicas, listenerReplicas         int
	natsInCluster                               bool
	natsReplicas, streamMaxBytes                int
	senderMessagesPerMinute, senderMessageBurst int
	signingKeyGenerations                       []int
	appResources                                dispatchAppResources
	pprofPort                                   int
	disruption                                  dispatchDisruption
	azure                                       dispatchAzure
	answerer                                    dispatchAnswerer
	existingCluster                             *DispatchClusterArgs
	existingDatabaseInstance                    bool
	regionalNetwork                             bool
}

// socks5Proxy is the address of the front door's SOCKS5 proxy, the one the app and the listener
// dial the NATS hub through, and "" with natsInCluster, where no proxy runs and the two dial
// natsUrl themselves. One derivation, so the mode and the address can never disagree.
func (s dispatchSettings) socks5Proxy() string {
	if s.natsInCluster {
		return ""
	}
	return clusterLocal(s.hostname+"-tailnet", s.namespace) + ":" + strconv.Itoa(dispatchSOCKS5Port)
}

// clusterURL is the app's own Service inside the cluster, the address a caller already in the
// cluster reaches Dispatch at: http because that Service terminates no TLS, on the 8766 it
// publishes. It is what a sandbox tells a reader it is at (serverURL) and what the front door's
// serve configuration forwards a tailnet request to, so both come from here and can never
// disagree.
func (s dispatchSettings) clusterURL() string {
	return "http://" + clusterLocal(s.hostname, s.namespace) + ":8766"
}

// serverURL is the address Dispatch tells a reader it is at, the url output and the app's
// DISPATCH_SERVER_URL: the tailnet name the front door hosts, publicOrigin where the deployment's
// own entrance serves Dispatch, or, in the sandbox, the app's own Service inside the cluster,
// which is all there is to reach. It carries no trailing slash, it is http in the sandbox because
// that Service terminates no TLS, and its port is the 8766 that Service publishes. One
// derivation, so the output and the variable can never disagree.
func (s dispatchSettings) serverURL() string {
	switch {
	case s.frontDoor:
		return "https://" + core.TailnetHostName(s.hostname, s.tailnetDNSName)
	case s.publicOrigin != "":
		return s.publicOrigin
	}
	return s.clusterURL()
}

// dispatchAzure is the Azure embedder's plain configuration together with the Azure sign-in the
// embedder and the answerer share, the zero value when no input of either is set. embedder is
// azureEmbedEndpoint set, which turns meaning search onto Azure and takes exactly one of key
// (azureEmbedKeySecret) and keyless (azureTenantId with azureClientId); keyless is that shared
// sign-in, which the azure answerer takes too, and audience is the token audience it asks for. So
// keyless with embedder unset is a deployment that embeds on Bedrock and answers on Azure.
type dispatchAzure struct {
	embedder        bool
	key, keyless    bool
	tokensPerMinute int
	audience        string
}

// newDispatchAzure validates the Azure embedder's inputs and the shared sign-in's: the embed
// endpoint turns the embedder on and needs the quota and exactly one credential form, and no other
// azureEmbed input is taken without it. The shared sign-in is taken with either endpoint, since
// whichever of the embedder and the answerer is on Azure reads it, and refused with neither, where
// nothing would sign in. The Inputs are judged by being set alone, so a value unknown at preview
// passes as it will at update.
func newDispatchAzure(args *DispatchArgs) (dispatchAzure, error) {
	keyless := args.AzureTenantId != nil || args.AzureClientId != nil
	azure := dispatchAzure{embedder: args.AzureEmbedEndpoint != nil, keyless: keyless}
	if args.AzureEmbedEndpoint == nil && args.AzureChatEndpoint == nil {
		if err := refuseDispatchInputsWithout("azureEmbedEndpoint or azureChatEndpoint", "the Azure sign-in is shared, read by whichever of the embedder and the answerer is on Azure", []dispatchInput{
			{"azureTenantId", args.AzureTenantId != nil},
			{"azureClientId", args.AzureClientId != nil},
			{"azureFederationAudience", args.AzureFederationAudience != nil},
		}); err != nil {
			return dispatchAzure{}, err
		}
	}
	if args.AzureTenantId != nil && args.AzureClientId == nil {
		return dispatchAzure{}, core.InputErrorf("azureClientId", "azureClientId is required when azureTenantId is set: the keyless form takes both")
	}
	if args.AzureClientId != nil && args.AzureTenantId == nil {
		return dispatchAzure{}, core.InputErrorf("azureTenantId", "azureTenantId is required when azureClientId is set: the keyless form takes both")
	}
	if args.AzureFederationAudience != nil && !keyless {
		return dispatchAzure{}, core.InputErrorf("azureFederationAudience", "azureFederationAudience is set without azureTenantId and azureClientId: it is the keyless form's token audience")
	}
	if keyless {
		azure.audience = defaultAzureFederationAudience
		if args.AzureFederationAudience != nil {
			if *args.AzureFederationAudience == "" {
				return dispatchAzure{}, core.InputErrorf("azureFederationAudience", "azureFederationAudience is empty; leave it unset for %s", defaultAzureFederationAudience)
			}
			azure.audience = *args.AzureFederationAudience
		}
	}
	if !azure.embedder {
		if err := refuseDispatchInputsWithout("azureEmbedEndpoint", "every azureEmbed input configures the Azure embedder, which the endpoint turns on", []dispatchInput{
			{"azureEmbedDeployment", args.AzureEmbedDeployment != nil},
			{"azureEmbedTokensPerMinute", args.AzureEmbedTokensPerMinute != nil},
			{"azureEmbedKeySecret", args.AzureEmbedKeySecret != nil},
		}); err != nil {
			return dispatchAzure{}, err
		}
		return azure, nil
	}
	if args.AzureEmbedTokensPerMinute == nil {
		return dispatchAzure{}, core.InputErrorf("azureEmbedTokensPerMinute", "azureEmbedTokensPerMinute is required when azureEmbedEndpoint is set: the deployment's tokens-per-minute quota, which Dispatch paces its embedding by")
	}
	if *args.AzureEmbedTokensPerMinute <= 0 {
		return dispatchAzure{}, core.InputErrorf("azureEmbedTokensPerMinute", "azureEmbedTokensPerMinute %d must be a positive integer", *args.AzureEmbedTokensPerMinute)
	}
	if args.AzureEmbedKeySecret != nil && keyless {
		return dispatchAzure{}, core.InputErrorf("azureEmbedKeySecret", "azureEmbedKeySecret is set with azureTenantId or azureClientId: the key and keyless credential forms are exclusive, so set exactly one")
	}
	if args.AzureEmbedKeySecret == nil && !keyless {
		return dispatchAzure{}, core.InputErrorf("azureEmbedEndpoint", "azureEmbedEndpoint needs one credential: azureEmbedKeySecret, or azureTenantId with azureClientId")
	}
	azure.key = !keyless
	azure.tokensPerMinute = *args.AzureEmbedTokensPerMinute
	return azure, nil
}

// dispatchAnswerer is where Dispatch's server generates text, as newDispatchAnswerer checked it:
// place is "" (no model configured and no DISPATCH_ANSWERER rendered), dispatchAnswererAzure or
// dispatchAnswererEndpoint. tokensPerMinute, apiVersion and scope belong to the azure place alone,
// each of the last two "" when its input is unset, where the component renders no such variable
// and Dispatch takes its own default; model is the endpoint place's chatModel, since the azure
// place's deployment name is an Input the rendering reads itself.
type dispatchAnswerer struct {
	place           string
	tokensPerMinute int
	apiVersion      string
	scope           string
	model           string
}

// newDispatchAnswerer validates the answerer's inputs: azureChatEndpoint turns the Azure chat
// deployment on and chatEndpoint the OpenAI-shaped service, each needing the inputs of its own
// form and refusing every input of a form its endpoint does not turn on, and the two are refused
// together, since one deployment names one answerer. keyless is whether the shared Azure sign-in
// is set (newDispatchAzure), which the azure place needs: its form takes no API key. The Inputs
// are judged by being set alone, as the embedder's are.
func newDispatchAnswerer(args *DispatchArgs, keyless bool) (dispatchAnswerer, error) {
	if args.AzureChatEndpoint == nil {
		if err := refuseDispatchInputsWithout("azureChatEndpoint", "every azureChat input configures the Azure answerer, which the endpoint turns on", []dispatchInput{
			{"azureChatDeployment", args.AzureChatDeployment != nil},
			{"azureChatApiVersion", args.AzureChatApiVersion != nil},
			{"azureChatScope", args.AzureChatScope != nil},
			{"azureChatTokensPerMinute", args.AzureChatTokensPerMinute != nil},
		}); err != nil {
			return dispatchAnswerer{}, err
		}
	}
	if args.ChatEndpoint == nil {
		if err := refuseDispatchInputsWithout("chatEndpoint", "every chat input configures the endpoint answerer, which the endpoint turns on", []dispatchInput{
			{"chatModel", args.ChatModel != nil},
			{"chatTokenSecret", args.ChatTokenSecret != nil},
		}); err != nil {
			return dispatchAnswerer{}, err
		}
	}
	if args.AzureChatEndpoint != nil && args.ChatEndpoint != nil {
		return dispatchAnswerer{}, core.InputErrorf("chatEndpoint", "chatEndpoint is set with azureChatEndpoint: one deployment names one answerer, so set exactly one")
	}
	switch {
	case args.AzureChatEndpoint != nil:
		return newDispatchAzureAnswerer(args, keyless)
	case args.ChatEndpoint != nil:
		return newDispatchEndpointAnswerer(args)
	}
	return dispatchAnswerer{}, nil
}

// newDispatchAzureAnswerer is newDispatchAnswerer for azureChatEndpoint: the quota and the
// deployment come with the endpoint, the api-version and the scope may, and the sign-in is the
// shared keyless credential, since the Azure chat form takes no API key.
func newDispatchAzureAnswerer(args *DispatchArgs, keyless bool) (dispatchAnswerer, error) {
	answerer := dispatchAnswerer{place: dispatchAnswererAzure}
	if args.AzureChatTokensPerMinute == nil {
		return dispatchAnswerer{}, core.InputErrorf("azureChatTokensPerMinute", "azureChatTokensPerMinute is required when azureChatEndpoint is set: the deployment's tokens-per-minute quota, which Dispatch paces its answers by")
	}
	if *args.AzureChatTokensPerMinute <= 0 {
		return dispatchAnswerer{}, core.InputErrorf("azureChatTokensPerMinute", "azureChatTokensPerMinute %d must be a positive integer", *args.AzureChatTokensPerMinute)
	}
	answerer.tokensPerMinute = *args.AzureChatTokensPerMinute
	if args.AzureChatDeployment == nil {
		return dispatchAnswerer{}, core.InputErrorf("azureChatDeployment", "azureChatDeployment is required when azureChatEndpoint is set: the chat deployment's name, which each request sends as its model")
	}
	if !keyless {
		return dispatchAnswerer{}, core.InputErrorf("azureChatEndpoint", "azureChatEndpoint needs the keyless Azure sign-in: azureTenantId with azureClientId, since the Azure chat form takes no API key")
	}
	if args.AzureChatApiVersion != nil {
		if *args.AzureChatApiVersion == "" {
			return dispatchAnswerer{}, core.InputErrorf("azureChatApiVersion", "azureChatApiVersion is empty; leave it unset for a request that sends no api-version")
		}
		answerer.apiVersion = *args.AzureChatApiVersion
	}
	if args.AzureChatScope != nil {
		if *args.AzureChatScope == "" {
			return dispatchAnswerer{}, core.InputErrorf("azureChatScope", "azureChatScope is empty; leave it unset for the scope Dispatch asks for by default")
		}
		answerer.scope = *args.AzureChatScope
	}
	return answerer, nil
}

// newDispatchEndpointAnswerer is newDispatchAnswerer for chatEndpoint: the bearer token's secret
// and the model come with the endpoint, and the token is the one secret of the group, read from
// Secret Manager at run time rather than carried as a value.
func newDispatchEndpointAnswerer(args *DispatchArgs) (dispatchAnswerer, error) {
	answerer := dispatchAnswerer{place: dispatchAnswererEndpoint}
	if args.ChatTokenSecret == nil {
		return dispatchAnswerer{}, core.InputErrorf("chatTokenSecret", "chatTokenSecret is required when chatEndpoint is set: the Secret Manager secret whose value is the bearer token every call presents")
	}
	if args.ChatModel == nil {
		return dispatchAnswerer{}, core.InputErrorf("chatModel", "chatModel is required when chatEndpoint is set: the model each request names")
	}
	if *args.ChatModel == "" {
		return dispatchAnswerer{}, core.InputErrorf("chatModel", "chatModel is empty; it is the model each request names")
	}
	answerer.model = *args.ChatModel
	return answerer, nil
}

// dispatchRollingUpdate bounds a rolling update: how many of the wanted pods may be missing at
// once, and how many may run above that count.
type dispatchRollingUpdate struct {
	maxUnavailable, maxSurge int
}

// dispatchStrategy is one Deployment's update strategy. rollingUpdate is set only when
// strategyType is dispatchStrategyRollingUpdate; a Recreate Deployment carries no such block.
type dispatchStrategy struct {
	strategyType  string
	rollingUpdate *dispatchRollingUpdate
}

// dispatchStrategyFor turns a workload's replica count into its Deployment's update strategy; each
// of the three Deployments takes it from its own count. At minDispatchReplicas the Deployment is
// recreated, stopping the old pod before starting the new one: a roll would run the outgoing and
// the incoming builds together, which is safe only once the outgoing build follows the rules for
// two builds running side by side and keeps its assets in the shared store, and a one-pod release
// cannot know that of the build it replaces. Above that count the operator runs such builds and
// wants no gap in service, so the Deployment rolls: one new pod comes up before any old one goes
// down.
func dispatchStrategyFor(replicas int) dispatchStrategy {
	if replicas <= minDispatchReplicas {
		return dispatchStrategy{strategyType: dispatchStrategyRecreate}
	}
	return dispatchStrategy{
		strategyType:  dispatchStrategyRollingUpdate,
		rollingUpdate: &dispatchRollingUpdate{maxUnavailable: 0, maxSurge: 1},
	}
}

// dispatchInput is one input of a refusal table: the input's schema name, and whether the
// consumer set it. A plain string is set when it is not empty, a slice when it has an element,
// and a pointer or a pulumi.*Input when it is not nil, as newDispatchAzure judges its pointers
// and Inputs, so a value unknown at preview is set as it will be at update.
type dispatchInput struct {
	input string
	set   bool
}

// dispatchTailnetInputs is the tailnet group: every input that exists only because Dispatch has a
// front door on a tailnet, from the name it is served at to the images its pods run. A consumer
// sets all five, for a Dispatch readers reach over the tailnet, or none, for the sandbox, where no
// front door is built and the app is reached inside its cluster alone. Each row's set-predicate is
// written here and nowhere else, dispatchClusterOnlyRefusals taking the group from here, so the
// resolver's mode, the refusals of a half-set group and clusterOnly's can never read an input two
// ways. The rows are in the order a refusal names them in (dispatchGroupClause).
func dispatchTailnetInputs(args *DispatchArgs) []dispatchInput {
	return []dispatchInput{
		{"tailnetDnsName", args.TailnetDnsName != ""},
		{"tags", len(args.Tags) > 0},
		{"tailnetAuthKey", args.TailnetAuthKey != nil},
		{"tailscaleImage", args.TailscaleImage != ""},
		{"frontDoorToolsImage", args.FrontDoorToolsImage != ""},
	}
}

// dispatchSignInInputs is the sign-in group: the Google client Dispatch hands a browser to, and
// the domain whose accounts it admits. All three or none, as the tailnet group; with none the app
// runs no browser sign-in at all, which is what the sandbox is for. Written here alone, as the
// tailnet group is.
func dispatchSignInInputs(args *DispatchArgs) []dispatchInput {
	return []dispatchInput{
		{"signInClientId", args.SignInClientId != ""},
		{"signInClientSecret", args.SignInClientSecret != nil},
		{"signInDomain", args.SignInDomain != ""},
	}
}

// dispatchDatabaseInputs are the four inputs that configure the Cloud SQL instance the component
// makes, in the order a refusal judges them in: newDispatchExistingDatabaseInstance refuses each
// beside existingDatabaseInstance, which replaces that instance, and dispatchClusterOnlyRefusals
// takes them from here. Written here alone, as the two groups are.
func dispatchDatabaseInputs(args *DispatchArgs) []dispatchInput {
	return []dispatchInput{
		{"databaseVersion", args.DatabaseVersion != nil},
		{"databaseTier", args.DatabaseTier != nil},
		{"databaseAvailabilityType", args.DatabaseAvailabilityType != nil},
		{"databaseWaitsFor", args.DatabaseWaitsFor != nil},
	}
}

// dispatchClusterOnlyRefusals is every pulumi-tagged input of DispatchArgs that configures
// Dispatch rather than the cluster, which clusterOnly refuses by name. Only the five inputs both
// modes use are absent: clusterOnly itself, region and hostname, which name every cloud resource,
// deletionProtection, which guards the cluster this mode makes as it guards whatever else the
// component makes (dispatchDeletionProtection), and regionalNetwork, which shapes the network half
// both modes build; newDispatchClusterOnlySettings refuses it false by a rule of its own, since
// true is the only value that leaves the cluster this mode makes a subnetwork. existingCluster and
// existingDatabaseInstance are among the refused: one names the cluster Dispatch runs in and the
// other the database instance it uses, and clusterOnly runs no Dispatch, always makes its own
// cluster and uses no database. The eight inputs of the two groups and the four database inputs
// come from their own definitions (dispatchTailnetInputs, dispatchSignInInputs and
// dispatchDatabaseInputs), so an input any of them gains is refused here without being written
// down a second time.
func dispatchClusterOnlyRefusals(args *DispatchArgs) []dispatchInput {
	return slices.Concat(dispatchTailnetInputs(args), dispatchSignInInputs(args), []dispatchInput{
		{"outsideOrigin", args.OutsideOrigin != ""},
		{"signInOutside", args.SignInOutside},
		{"signInPasskey", args.SignInPasskey},
		{"signInWorkspaceDomains", len(args.SignInWorkspaceDomains) > 0},
		{"publicOrigin", args.PublicOrigin != ""},
		{"iapAudience", args.IapAudience != nil},
		{"loadBalancerSourceRanges", len(args.LoadBalancerSourceRanges) > 0},
		{"networkEndpointGroupName", args.NetworkEndpointGroupName != ""},
		{"envoyImage", args.EnvoyImage != ""},
		{"cloudSqlProxyImage", args.CloudSqlProxyImage != ""},
		{"natsUrl", args.NatsUrl != nil},
		{"natsSeedSecret", args.NatsSeedSecret != nil},
		{"agentSecretsUrl", args.AgentSecretsUrl != ""},
		{"agentSecretsAudience", args.AgentSecretsAudience != ""},
		{"oidcAudience", args.OidcAudience != ""},
		{"oidcIssuer", args.OidcIssuer != ""},
		{"oidcAccounts", len(args.OidcAccounts) > 0},
		{"accessAccounts", len(args.AccessAccounts) > 0},
		{"projectOwnerFallback", args.ProjectOwnerFallback != ""},
		{"privateProjects", args.PrivateProjects != nil},
		{"appClientId", args.AppClientId != ""},
		{"appId", args.AppId != ""},
		{"appPrivateKeySecretName", args.AppPrivateKeySecretName != ""},
		{"appPrivateKeySecretKey", args.AppPrivateKeySecretKey != ""},
	}, dispatchDatabaseInputs(args), []dispatchInput{
		{"namespace", args.Namespace != nil},
		{"existingNamespace", args.ExistingNamespace != nil},
		{"existingCluster", args.ExistingCluster != nil},
		{"existingDatabaseInstance", args.ExistingDatabaseInstance != nil},
		{"replicas", args.Replicas != nil},
		{"frontDoorReplicas", args.FrontDoorReplicas != nil},
		{"listenerReplicas", args.ListenerReplicas != nil},
		{"natsInCluster", args.NatsInCluster != nil},
		{"natsReplicas", args.NatsReplicas != nil},
		{"streamMaxBytes", args.StreamMaxBytes != nil},
		{"senderMessagesPerMinute", args.SenderMessagesPerMinute != nil},
		{"senderMessageBurst", args.SenderMessageBurst != nil},
		{"signingKeyGenerations", len(args.SigningKeyGenerations) > 0},
		{"appCpu", args.AppCpu != nil},
		{"appMemory", args.AppMemory != nil},
		{"proxyCpu", args.ProxyCpu != nil},
		{"proxyMemory", args.ProxyMemory != nil},
		{"appEphemeralStorage", args.AppEphemeralStorage != nil},
		{"proxyEphemeralStorage", args.ProxyEphemeralStorage != nil},
		{"pprofPort", args.PprofPort != nil},
		{"priorityClassName", args.PriorityClassName != nil},
		{"appPdbMinAvailable", args.AppPdbMinAvailable != nil},
		{"frontDoorPdbMinAvailable", args.FrontDoorPdbMinAvailable != nil},
		{"listenerPdbMinAvailable", args.ListenerPdbMinAvailable != nil},
		{"allowedNamespaces", len(args.AllowedNamespaces) > 0},
		{"allowedApps", len(args.AllowedApps) > 0},
		{"azureEmbedEndpoint", args.AzureEmbedEndpoint != nil},
		{"azureEmbedDeployment", args.AzureEmbedDeployment != nil},
		{"azureEmbedTokensPerMinute", args.AzureEmbedTokensPerMinute != nil},
		{"azureEmbedKeySecret", args.AzureEmbedKeySecret != nil},
		{"azureTenantId", args.AzureTenantId != nil},
		{"azureClientId", args.AzureClientId != nil},
		{"azureFederationAudience", args.AzureFederationAudience != nil},
		{"azureChatEndpoint", args.AzureChatEndpoint != nil},
		{"azureChatDeployment", args.AzureChatDeployment != nil},
		{"azureChatApiVersion", args.AzureChatApiVersion != nil},
		{"azureChatScope", args.AzureChatScope != nil},
		{"azureChatTokensPerMinute", args.AzureChatTokensPerMinute != nil},
		{"chatEndpoint", args.ChatEndpoint != nil},
		{"chatModel", args.ChatModel != nil},
		{"chatTokenSecret", args.ChatTokenSecret != nil},
	})
}

// dispatchGroupSet reports whether the consumer set any input of group, which is what turns the
// feature the group configures on: one input is enough, so a consumer who meant to set the group
// and missed an input is refused by that group's own checks rather than silently given the mode
// it did not ask for.
func dispatchGroupSet(group []dispatchInput) bool {
	return slices.ContainsFunc(group, func(in dispatchInput) bool { return in.set })
}

// dispatchGroupClause is the clause every refusal of one of feature's own inputs trails while the
// group is set in part: the rule the consumer broke, then unset, every input of the group left
// unset, comma-joined and in the group's order. One run then names the whole shortfall whatever
// the refusal was about, the value an input carries or an input missing: the checks stop at the
// first input that is wrong, and a consumer told about that one alone would correct it and be
// refused again for the next. A whole group is missing nothing to name, so its clause is "".
func dispatchGroupClause(feature string, group []dispatchInput) string {
	unset := make([]string, 0, len(group))
	for _, in := range group {
		if !in.set {
			unset = append(unset, in.input)
		}
	}
	if len(unset) == 0 {
		return ""
	}
	return fmt.Sprintf(" (the %s inputs are set together or not at all; unset: %s)", feature, strings.Join(unset, ", "))
}

// refuseDispatchInputsWithout is the refusal a group of inputs an endpoint turns on shares: with
// that endpoint unset, the first input of the group the consumer did set is named, together with
// reason, which says what the endpoint the group needs is for. The first and not all of them,
// because a consumer who set a whole group has one thing to do about it - set the endpoint - and
// the group's own order is the order the table is written in. Nothing set is no refusal.
func refuseDispatchInputsWithout(endpoint, reason string, inputs []dispatchInput) error {
	for _, in := range inputs {
		if in.set {
			return core.InputErrorf(in.input, "%s is required when %s is set: %s", endpoint, in.input, reason)
		}
	}
	return nil
}

// refuseDispatchTailnetGroup judges the tailnet group alone: the tailnet name Dispatch is served
// at, the tags its front-door nodes carry, the two images those pods run, held to the digest rule
// the app's own images are held to, and the key the nodes join the tailnet with. It runs only
// while that group is set, so the sandbox, which sets none of it, reaches none of these checks,
// and every refusal trails clause, which names every tailnet input still unset
// (dispatchGroupClause): a group set in part is refused for the first input that is wrong and
// told the whole shortfall at once.
func refuseDispatchTailnetGroup(args *DispatchArgs, clause string) error {
	if !tailnetDNSNameRule.MatchString(args.TailnetDnsName) {
		return core.InputErrorf("tailnetDnsName", "tailnetDnsName %q must match %s%s", args.TailnetDnsName, tailnetDNSNameRule, clause)
	}
	if len(args.Tags) == 0 {
		return core.InputErrorf("tags", "tags: at least one tag is required%s", clause)
	}
	for i, tag := range args.Tags {
		if !tagRule.MatchString(tag) {
			return core.InputErrorf("tags", "tags[%d] %q must match %s%s", i, tag, tagRule, clause)
		}
	}
	for _, image := range []struct{ field, ref string }{
		{"tailscaleImage", args.TailscaleImage},
		{"frontDoorToolsImage", args.FrontDoorToolsImage},
	} {
		if !imageRule.MatchString(image.ref) {
			return core.InputErrorf(image.field, "%s %q must be pinned by digest, as <name>@sha256:<64 lowercase hex digits>%s", image.field, image.ref, clause)
		}
	}
	if args.TailnetAuthKey == nil {
		return core.InputErrorf("tailnetAuthKey", "tailnetAuthKey is required%s", clause)
	}
	return nil
}

const dispatchSandboxSignInRefusal = "%s configures browser sign-in, which needs an https origin, the tailnet inputs' or publicOrigin: without either Dispatch is reached at an in-cluster http:// address, which no OAuth provider takes as a redirect origin, so no browser could sign in; unset %s, or set the tailnet inputs or publicOrigin"

// refuseDispatchSandbox judges what the sandbox cannot make good, and requires the two inputs the
// mode needs. It runs only while the tailnet group is wholly unset and publicOrigin is unset,
// which is what asks for the mode: no front door is built and no origin is served, so a sign-in
// input, signInWorkspaceDomains and signInOutside among them, would ask an OAuth provider to
// redirect to an in-cluster http:// address, signInPasskey would sign people in at a second origin
// the sandbox cannot have, outsideOrigin would add an origin for outside sign-in to a Dispatch
// where browser sign-in is refused, and the app's own policy is its only ingress; what any
// Dispatch without a front door cannot make good is refuseDispatchWithoutFrontDoor's. Each is
// refused by its own name rather than accepted and quietly dropped. The sign-in inputs are refused
// here, before the sign-in group's own checks, so one sent to a sandbox is refused for being sent
// at all and not for the rest of its group being missing.
func refuseDispatchSandbox(args *DispatchArgs, signInGroup []dispatchInput) error {
	for _, in := range signInGroup {
		if in.set {
			return core.InputErrorf(in.input, dispatchSandboxSignInRefusal, in.input, "the sign-in inputs")
		}
	}
	if args.SignInOutside {
		return core.InputErrorf("signInOutside", dispatchSandboxSignInRefusal, "signInOutside", "signInOutside")
	}
	if len(args.SignInWorkspaceDomains) > 0 {
		return core.InputErrorf("signInWorkspaceDomains", dispatchSandboxSignInRefusal, "signInWorkspaceDomains", "signInWorkspaceDomains")
	}
	if args.SignInPasskey {
		return core.InputErrorf("signInPasskey", "signInPasskey needs outsideOrigin, which needs the tailnet inputs or publicOrigin: without either the component builds the sandbox, whose own origin is an in-cluster http:// address where browser sign-in is refused; unset signInPasskey, or set the tailnet inputs or publicOrigin")
	}
	if args.OutsideOrigin != "" {
		return core.InputErrorf("outsideOrigin", "outsideOrigin needs the tailnet inputs or publicOrigin: without either the component builds the sandbox, whose own origin is an in-cluster http:// address where browser sign-in is refused, and outside sign-in is what a second origin is for; unset outsideOrigin, or set the tailnet inputs or publicOrigin")
	}
	if err := refuseDispatchWithoutFrontDoor(args); err != nil {
		return err
	}
	if len(args.AllowedNamespaces) == 0 {
		return core.InputErrorf("allowedNamespaces", "allowedNamespaces must name at least one namespace without the tailnet inputs or publicOrigin: with no entrance its rules are the app's only ingress, and a NetworkPolicy with no rule admits nobody, so nothing could reach Dispatch at all")
	}
	return nil
}

// refuseDispatchWithoutFrontDoor judges what a Dispatch without the tailnet inputs cannot make
// good, in the sandbox and at publicOrigin alike: the pods reach a NATS hub only inside their own
// cluster, since no front door runs the SOCKS5 proxy a tailnet hub is reached through, and
// frontDoorReplicas and frontDoorPdbMinAvailable size and protect a Deployment that does not
// exist. Each is refused by its own name rather than accepted and quietly dropped, as a
// core.InputError naming it.
func refuseDispatchWithoutFrontDoor(args *DispatchArgs) error {
	if args.NatsInCluster == nil || !*args.NatsInCluster {
		return core.InputErrorf("natsInCluster", "natsInCluster is required without the tailnet inputs: the pods reach a tailnet NATS hub only through the front door's SOCKS5 proxy, and no front door is built, so the hub must run in the cluster")
	}
	if args.FrontDoorReplicas != nil {
		return core.InputErrorf("frontDoorReplicas", "frontDoorReplicas sizes a front door, which is not built without the tailnet inputs: unset it, or set the tailnet inputs")
	}
	if args.FrontDoorPdbMinAvailable != nil {
		return core.InputErrorf("frontDoorPdbMinAvailable", "frontDoorPdbMinAvailable covers a front door, which is not built without the tailnet inputs: unset it, or set the tailnet inputs")
	}
	return nil
}

// refuseDispatchPublicOrigin judges publicOrigin where the tailnet group is unset, the one place
// it may be set, and returns it as a URL holding its scheme and host alone, whose String is the
// origin with a trailing slash trimmed, as Dispatch trims one. It must be a web origin
// weborigin.Parse takes, whose refusals it prefixes with the input's name, and https, since
// Dispatch's session cookie, browser sign-in and IAP all need TLS there, as they do at the front
// door's name; with the sign-in inputs set its host must be one weborigin.GoogleRedirectHost
// takes, since the Google client must then list <publicOrigin>/auth/callback, and that function's
// refusals it prefixes with "publicOrigin's ".
func refuseDispatchPublicOrigin(args *DispatchArgs, signInOn bool) (*url.URL, error) {
	origin, err := weborigin.Parse(args.PublicOrigin)
	if err != nil {
		return nil, core.InputErrorf("publicOrigin", "publicOrigin: %w", err)
	}
	if origin.Scheme != "https" {
		return nil, core.InputErrorf("publicOrigin", "publicOrigin must be an https origin: Dispatch's session cookie, browser sign-in and IAP need TLS at it, as they do at the front door's name")
	}
	if signInOn {
		if err := weborigin.GoogleRedirectHost(origin.Hostname()); err != nil {
			return nil, core.InputErrorf("publicOrigin", "publicOrigin's %w", err)
		}
	}
	return origin, nil
}

// refuseDispatchIAP judges what the IAP door, which iapAudience turns on, needs beside it. It needs
// publicOrigin: IAP runs on the deployment's own load balancer, which serves Dispatch there, and
// no request through the tailnet front door or into the sandbox carries IAP's signed header. It
// needs signInDomain, the Workspace domain whose accounts may use Dispatch, which Dispatch holds
// the signed header's hd claim to by the membership rule its own sign-in uses, and which it
// refuses to boot without under IAP. And it refuses the Google client (signInClientId,
// signInClientSecret), signInWorkspaceDomains, signInOutside, signInPasskey and outsideOrigin: IAP
// signs a person in and a browser reaches Dispatch through it alone, so Dispatch runs no sign-in of
// its own, by Google or by passkey, and serves no second origin for outside sign-in. Each is
// refused by its own name, signInPasskey before outsideOrigin, which a consumer sets for it, and
// all of them before the sign-in group's own checks, which the IAP door does not run, so a client
// input sent beside IAP is refused for being sent at all. The audience's own value may be another
// resource's output, so it is judged once it resolves (dispatchIAPAudience).
func refuseDispatchIAP(args *DispatchArgs) error {
	if args.PublicOrigin == "" {
		return core.InputErrorf("iapAudience", "iapAudience needs publicOrigin: IAP runs on the deployment's own load balancer, which serves Dispatch at that origin, and no request through the tailnet front door or into the sandbox carries IAP's signed header; set publicOrigin, or unset iapAudience")
	}
	const signIn = "%s configures Google sign-in, which iapAudience replaces: IAP signs a person in and Dispatch names them from IAP's signed header, so it runs no sign-in of its own; unset %s, or unset iapAudience"
	for _, in := range []dispatchInput{
		{"signInClientId", args.SignInClientId != ""},
		{"signInClientSecret", args.SignInClientSecret != nil},
		{"signInOutside", args.SignInOutside},
	} {
		if in.set {
			return core.InputErrorf(in.input, signIn, in.input, in.input)
		}
	}
	if len(args.SignInWorkspaceDomains) > 0 {
		return core.InputErrorf("signInWorkspaceDomains", signIn, "signInWorkspaceDomains", "signInWorkspaceDomains")
	}
	if args.SignInPasskey {
		return core.InputErrorf("signInPasskey", "signInPasskey is refused beside iapAudience: IAP signs a person in and Dispatch names them from IAP's signed header, so it runs no passkey sign-in of its own; unset signInPasskey, or unset iapAudience")
	}
	if args.OutsideOrigin != "" {
		return core.InputErrorf("outsideOrigin", "outsideOrigin is refused beside iapAudience: a browser reaches Dispatch through IAP alone, at publicOrigin, and a second origin is for outside sign-in, which the IAP door does not run; unset outsideOrigin, or unset iapAudience")
	}
	if !dispatchDomainName(args.SignInDomain) {
		return core.InputErrorf("signInDomain", "signInDomain %q must be a lowercase DNS name with iapAudience: the Workspace domain whose accounts may use Dispatch, which Dispatch holds IAP's signed header's hd claim to and refuses to boot without", args.SignInDomain)
	}
	return nil
}

// refuseDispatchSignInGroup judges the sign-in group alone: the Google client Dispatch hands a
// browser to, the domain whose accounts it admits and that client's secret. It runs only while
// that group is set, so a deployment that runs no browser sign-in reaches none of these checks,
// and every refusal trails clause, which names every sign-in input still unset, as the tailnet
// group's refusals do. Each is a core.InputError naming the input to correct. A client ID of
// whitespace alone is no client: Dispatch trims DISPATCH_SIGNIN_CLIENT_ID and refuses to boot
// without one. The domain is held to the rule Dispatch holds DISPATCH_SIGNIN_DOMAIN to
// (dispatchDomainName).
func refuseDispatchSignInGroup(args *DispatchArgs, clause string) error {
	if args.SignInClientId == "" {
		return core.InputErrorf("signInClientId", "signInClientId is required%s", clause)
	}
	if strings.TrimSpace(args.SignInClientId) == "" {
		return core.InputErrorf("signInClientId", "signInClientId is blank, which no Google client ID is: Dispatch trims it and refuses to boot without one%s", clause)
	}
	if !dispatchDomainName(args.SignInDomain) {
		return core.InputErrorf("signInDomain", "signInDomain %q must be a lowercase DNS name%s", args.SignInDomain, clause)
	}
	if args.SignInClientSecret == nil {
		return core.InputErrorf("signInClientSecret", "signInClientSecret is required%s", clause)
	}
	return nil
}

// dispatchDomainShape is a DNS name of two labels or more, each a lowercase letter or digit, then
// up to 61 letters, digits or hyphens, then a letter or digit (63 characters at most): the shape
// Dispatch holds DISPATCH_SIGNIN_DOMAIN and each DISPATCH_SIGNIN_WORKSPACE_DOMAINS entry's A-labels
// to, with maxDispatchDomainLength, the longest DNS name in its dotted text form. The domains
// Dispatch takes and refuses are packages/envoy/cmd/dispatch/testdata/signin-domain-vectors.json,
// which this package's tests and Dispatch's read alike.
var dispatchDomainShape = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

const maxDispatchDomainLength = 253

// dispatchDomainName reports whether domain is one Dispatch takes as DISPATCH_SIGNIN_DOMAIN, under
// Google sign-in and under IAP alike: once trimmed of surrounding whitespace (strings.TrimSpace),
// as Dispatch's boot trims the setting before it checks it, in ASCII, of dispatchDomainShape and
// at most maxDispatchDomainLength characters. The component renders the trimmed value.
func dispatchDomainName(domain string) bool {
	domain = strings.TrimSpace(domain)
	return len(domain) <= maxDispatchDomainLength && dispatchDomainShape.MatchString(domain)
}

// workspaceDomainRule reports whether entry is one Dispatch takes in DISPATCH_SIGNIN_WORKSPACE_DOMAINS:
// once trimmed of surrounding whitespace, as Dispatch trims each entry of the list, lowercase,
// taken by IDNA lookup (idna.Lookup.ToASCII), and then, as its A-labels, a dispatchDomainName. An
// internationalized domain may be written in Unicode (bücher.example) or as its A-labels
// (xn--bcher-kva.example); a label such as xn--a, which decodes to nothing, is refused.
// dispatchDomainShape admits no comma, so no entry splits into two when the list is comma-joined.
// The component renders each entry trimmed.
func workspaceDomainRule(entry string) bool {
	entry = strings.TrimSpace(entry)
	ascii, err := idna.Lookup.ToASCII(entry)
	return err == nil && entry == strings.ToLower(entry) && dispatchDomainName(ascii)
}

// refuseDispatchWorkspaceDomains judges signInWorkspaceDomains where the sign-in inputs are set,
// the one place it may be, so a bad entry fails the resolver rather than the pod's boot: each entry
// must be one workspaceDomainRule takes, and an empty one is refused on its own; one of whitespace
// alone, which Dispatch trims to nothing, is refused by that rule. Each refusal is a
// core.InputError naming signInWorkspaceDomains, as the schema names the input, whose message names
// the entry by its index. dispatchSeededInput has refused a seed in any entry already, so a
// refusal may quote it.
func refuseDispatchWorkspaceDomains(domains []string) error {
	for i, domain := range domains {
		entry := fmt.Sprintf("signInWorkspaceDomains[%d]", i)
		if domain == "" {
			return core.InputErrorf("signInWorkspaceDomains", "%s is empty: each entry is a domain the Workspace owns", entry)
		}
		if !workspaceDomainRule(domain) {
			return core.InputErrorf("signInWorkspaceDomains", "%s %q must be a lowercase domain name that IDNA lookup takes, such as example.net or bücher.example, of two labels or more, each of at most 63 characters, and at most 253 characters in all, as Dispatch requires of each DISPATCH_SIGNIN_WORKSPACE_DOMAINS entry", entry, domain)
		}
	}
	return nil
}

// refuseDispatchOutsideOrigin judges outsideOrigin where Dispatch serves an origin of its own, the
// front door's or publicOrigin, the only places it may be set, and returns it with a trailing
// slash trimmed, as Dispatch trims one: "" when it is unset. ownHost is that origin's host and
// owner names it in a refusal ("the front door's", "publicOrigin's"). It must be a web origin
// weborigin.Parse takes, whose refusals it prefixes with the input's name; the component adds that
// the origin is https, as Dispatch's own is, that its host is not ownHost, whose origin Dispatch
// already is, and, with the sign-in inputs set, that its host is one weborigin.GoogleRedirectHost
// takes: without signInPasskey the Google client must then list <outsideOrigin>/auth/callback,
// and with it, where Dispatch answers that path 404 and the client lists no callback there, the
// host is the passkey's relying party ID, which must be a domain name, never an IP address. That
// function's refusals it prefixes with "outsideOrigin's ".
func refuseDispatchOutsideOrigin(args *DispatchArgs, ownHost, owner string, signInOn bool) (string, error) {
	if args.OutsideOrigin == "" {
		return "", nil
	}
	origin, err := weborigin.Parse(args.OutsideOrigin)
	if err != nil {
		return "", core.InputErrorf("outsideOrigin", "outsideOrigin: %w", err)
	}
	if origin.Scheme != "https" {
		return "", core.InputErrorf("outsideOrigin", "outsideOrigin must be an https origin: browser sign-in at it needs TLS, as %s does", owner)
	}
	host := origin.Hostname()
	if host == ownHost {
		return "", core.InputErrorf("outsideOrigin", "outsideOrigin's host %s is %s: a second origin needs a host of its own", host, owner)
	}
	if !signInOn {
		return origin.String(), nil
	}
	if err := weborigin.GoogleRedirectHost(host); err != nil {
		return "", core.InputErrorf("outsideOrigin", "outsideOrigin's %w", err)
	}
	return origin.String(), nil
}

// negNameRule is a Compute Engine resource name, which a network endpoint group's is: a lowercase
// letter, then up to 62 lowercase letters, digits or hyphens, the last not a hyphen
// (https://cloud.google.com/compute/docs/naming-resources).
var negNameRule = regexp.MustCompile(`^[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?$`)

// appLabelValueRule is a non-empty Kubernetes label value, which an allowedApps entry is: the app
// label of the pods the app's NetworkPolicy admits. It is at most 63 characters, a letter or digit
// at each end, with letters, digits, dashes, underscores and dots between. An empty value would
// make a rule for pods labelled app="", and a value of another shape fails only at apply.
var appLabelValueRule = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$`)

// refuseDispatchAccessValues refuses, when the stack plans, the access values Dispatch refuses at
// boot (cmd/dispatch checkAccessSettings), naming the input, so one typo never reaches a Deployment
// whose one replica would then not start: an account that is not a service account's subject, an
// access account a set oidcAccounts leaves out, and an owner fallback that is not a lowercase email.
func refuseDispatchAccessValues(args *DispatchArgs) error {
	for _, list := range []struct {
		input    string
		accounts []string
	}{{"oidcAccounts", args.OidcAccounts}, {"accessAccounts", args.AccessAccounts}} {
		for i, account := range list.accounts {
			if !principal.ServiceAccountSubject.MatchString(account) {
				return core.InputErrorf(list.input, "%s[%d] %q is not a Kubernetes service account's subject, system:serviceaccount:<namespace>:<name>, one account to an entry", list.input, i, account)
			}
		}
	}
	if len(args.OidcAccounts) > 0 {
		for i, account := range args.AccessAccounts {
			if !slices.Contains(args.OidcAccounts, account) {
				return core.InputErrorf("accessAccounts", "accessAccounts[%d] %q is not among oidcAccounts: an account that reads the membership answer must be one the deployment admits", i, account)
			}
		}
	}
	if fallback := args.ProjectOwnerFallback; fallback != "" && principal.CheckPersonLogin(fallback) != nil {
		return core.InputErrorf("projectOwnerFallback", "projectOwnerFallback must be a lowercase email, the one a full member signed in with")
	}
	return nil
}

// refuseDispatchEntrance judges what the deployment's own entrance, a load balancer serving
// publicOrigin, hands the component: loadBalancerSourceRanges, the CIDRs it reaches the app's pods
// from, and networkEndpointGroupName, the endpoint group it takes as its backend. Each needs
// publicOrigin: a Dispatch on the tailnet is reached through its front door and one in the sandbox
// inside its cluster, and neither has a load balancer to admit or to name a group for. Each range
// must be a CIDR written as its network, which net.ParseCIDR takes and which is the network it
// names, so the rule admits what it says, and with a prefix longer than /0: 0.0.0.0/0 or ::/0
// would admit every address to 8766, past the load balancer, and IAP with it. The name must be
// negNameRule's.
func refuseDispatchEntrance(args *DispatchArgs) error {
	for _, in := range []dispatchInput{
		{"loadBalancerSourceRanges", len(args.LoadBalancerSourceRanges) > 0},
		{"networkEndpointGroupName", args.NetworkEndpointGroupName != ""},
	} {
		if in.set && args.PublicOrigin == "" {
			return core.InputErrorf(in.input, "%s needs publicOrigin: it describes the deployment's own load balancer, which serves Dispatch at that origin, and a Dispatch reached through the tailnet front door or inside its cluster has none; set publicOrigin, or unset %s", in.input, in.input)
		}
	}
	for i, cidr := range args.LoadBalancerSourceRanges {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			return core.InputErrorf("loadBalancerSourceRanges", "loadBalancerSourceRanges[%d] %q must be a CIDR, such as 130.211.0.0/22", i, cidr)
		}
		if network.String() != cidr {
			return core.InputErrorf("loadBalancerSourceRanges", "loadBalancerSourceRanges[%d] %q must be written as the network it names, %s", i, cidr, network)
		}
		if ones, _ := network.Mask.Size(); ones == 0 {
			return core.InputErrorf("loadBalancerSourceRanges", "loadBalancerSourceRanges[%d] %q admits every address to 8766, past the load balancer and IAP alike: name the load balancer's own ranges, such as 130.211.0.0/22", i, cidr)
		}
	}
	if name := args.NetworkEndpointGroupName; name != "" && !negNameRule.MatchString(name) {
		return core.InputErrorf("networkEndpointGroupName", "networkEndpointGroupName %q must match %s, a Compute Engine resource name", name, negNameRule)
	}
	return nil
}

// refuseDispatchHostname refuses a hostname that is not hostnameRule's shape, and one
// weborigin.ReservedLabel refuses, since the front door puts <hostname>.<tailnetDnsName> on its
// certificate (dispatch_frontdoor.go, its certificate field). Both resolvers run it, in every mode:
// a stack in the cluster-only mode or the sandbox, which builds no front door, keeps a hostname a
// front door can later serve, and its hostname, which names every cloud resource, is held to the
// rule the full resolver holds it to, with the same refusals.
func refuseDispatchHostname(hostname string) error {
	if !hostnameRule.MatchString(hostname) {
		return core.InputErrorf("hostname", "hostname %q must match %s", hostname, hostnameRule)
	}
	if err := weborigin.ReservedLabel(hostname); err != nil {
		return core.InputErrorf("hostname", "hostname: %w", err)
	}
	return nil
}

// refuseDispatchRegion refuses an unset region, which both modes need: it is the location of the
// cluster and of every resource around it.
func refuseDispatchRegion(region string) error {
	if region == "" {
		return core.InputErrorf("region", "region is required")
	}
	return nil
}

// resolveDispatchExistingCluster resolves existingCluster: nil when it is unset, where the
// component makes a cluster of its own, and otherwise the input itself, once its name and location
// have the shape GKE gives them, each refused as a core.InputError naming its path when it does
// not, so the lookup never asks for a cluster no GKE cluster can be. A GKE cluster's name is a
// DNS-1035 label (dns1035LabelRule) of at most 40 characters. dispatchSeededInput has refused a
// seed in either already, so each refusal may quote its value.
func resolveDispatchExistingCluster(args *DispatchArgs) (*DispatchClusterArgs, error) {
	cluster := args.ExistingCluster
	if cluster == nil {
		return nil, nil
	}
	if len(cluster.Name) > 40 || !dns1035LabelRule.MatchString(cluster.Name) {
		return nil, core.InputErrorf("existingCluster.name",
			"existingCluster.name %q must be a GKE cluster name: a lowercase letter, then lowercase letters, digits and hyphens, ending in a letter or digit, at most 40 characters", cluster.Name)
	}
	if !clusterLocationRule.MatchString(cluster.Location) {
		return nil, core.InputErrorf("existingCluster.location",
			"existingCluster.location %q must be a region or a zone, such as us-central1 or us-central1-a", cluster.Location)
	}
	return cluster, nil
}

// newDispatchExistingDatabaseInstance resolves existingDatabaseInstance: false when it is unset,
// where the component makes a Cloud SQL instance of its own, and otherwise true, with each of the
// four inputs that configure that instance (dispatchDatabaseInputs) refused when it is set, in that
// order, as a core.InputError naming it. The instance existingDatabaseInstance names replaces the
// component's own, so each of the four would configure nothing, and is refused rather than
// dropped. The check runs before the resolver judges the four's own values, so one sent beside it
// is refused for being sent at all, an empty databaseTier as much as a valid one. The connection
// name is a Pulumi Input, judged once it resolves (dispatchDatabaseConnectionName).
func newDispatchExistingDatabaseInstance(args *DispatchArgs) (bool, error) {
	if args.ExistingDatabaseInstance == nil {
		return false, nil
	}
	for _, in := range dispatchDatabaseInputs(args) {
		if in.set {
			return false, core.InputErrorf(in.input,
				"%s configures the Cloud SQL instance the component makes, which existingDatabaseInstance replaces: unset it, or unset existingDatabaseInstance", in.input)
		}
	}
	return true, nil
}

// dispatchDeletionProtection resolves deletionProtection, true when the consumer sets nothing. It
// guards what the component makes: its own cluster, its own Cloud SQL instance and the database
// dispatch on it, and the Tailscale Service. A cluster existingCluster names and an instance
// existingDatabaseInstance names are not the component's, and it guards neither. clusterOnly makes
// its own cluster too, so both resolvers default it the same way.
func dispatchDeletionProtection(value *bool) bool {
	if value == nil {
		return true
	}
	return *value
}

// dispatchRegionalNetwork resolves regionalNetwork, true when the consumer sets nothing. It shapes
// the network half both modes build, so both resolvers default it the same way; each refuses false
// in its own words, since only the full resolver can be given the existingCluster false needs.
func dispatchRegionalNetwork(value *bool) bool {
	return value == nil || *value
}

// newDispatchClusterOnlySettings resolves the inputs clusterOnly leaves live, hostname, region,
// deletionProtection and regionalNetwork, each as the full resolver resolves it, and refuses every
// input that configures Dispatch, which this mode does not build. regionalNetwork is live because
// it shapes the network half both modes build, but this mode always makes its own cluster on the
// subnetwork false would drop, and refuses existingCluster, so true (or unset) changes nothing and
// false is refused, as a core.InputError naming it. It validates nothing else and applies no other
// default: no other value reaches a resource here.
func newDispatchClusterOnlySettings(args *DispatchArgs) (dispatchSettings, error) {
	if err := refuseDispatchHostname(args.Hostname); err != nil {
		return dispatchSettings{}, err
	}
	if err := refuseDispatchRegion(args.Region); err != nil {
		return dispatchSettings{}, err
	}
	regionalNetwork := dispatchRegionalNetwork(args.RegionalNetwork)
	if !regionalNetwork {
		return dispatchSettings{}, core.InputErrorf("regionalNetwork",
			"regionalNetwork false leaves the cluster clusterOnly builds no subnetwork: unset regionalNetwork")
	}
	s := dispatchSettings{
		clusterOnly: true, hostname: args.Hostname, region: args.Region,
		deletionProtection: dispatchDeletionProtection(args.DeletionProtection), regionalNetwork: regionalNetwork,
	}
	for _, in := range dispatchClusterOnlyRefusals(args) {
		if in.set {
			return dispatchSettings{}, core.InputErrorf(in.input, "%s configures Dispatch, which clusterOnly does not build: unset it, or unset clusterOnly", in.input)
		}
	}
	return s, nil
}

// newDispatchSettings validates the component's plain inputs and renders the values known before
// any cloud resource is registered. Every refusal is a core.InputError naming the input that needs
// correction, as the schema names it.
func newDispatchSettings(args *DispatchArgs) (dispatchSettings, error) {
	if input, ok := dispatchSeededInput(args); ok {
		// The refusal names the input as the schema does, a list without the entry's index.
		name, _, _ := strings.Cut(input, "[")
		return dispatchSettings{}, core.InputErrorf(name, "%w", core.SeedRefusal(input))
	}
	if args.ClusterOnly != nil && *args.ClusterOnly {
		return newDispatchClusterOnlySettings(args)
	}
	if err := refuseDispatchHostname(args.Hostname); err != nil {
		return dispatchSettings{}, err
	}
	// Each group is judged by its own function while it is set; what a Dispatch serving
	// publicOrigin cannot make good by a third while the tailnet group is unset and publicOrigin
	// is set, and what the sandbox cannot by a fourth while both are unset. The IAP door is judged
	// first, so a sign-in input sent beside it is refused for being sent at all; it takes
	// signInDomain alone of the sign-in group and runs no Google sign-in, so the group is off
	// beside it.
	tailnetGroup, signInGroup := dispatchTailnetInputs(args), dispatchSignInInputs(args)
	frontDoor, signInOn := dispatchGroupSet(tailnetGroup), dispatchGroupSet(signInGroup)
	if args.IapAudience != nil {
		if err := refuseDispatchIAP(args); err != nil {
			return dispatchSettings{}, err
		}
		signInOn = false
	}
	if err := refuseDispatchEntrance(args); err != nil {
		return dispatchSettings{}, err
	}
	var outsideOrigin, publicOrigin string
	switch {
	case frontDoor:
		if err := refuseDispatchTailnetGroup(args, dispatchGroupClause("tailnet", tailnetGroup)); err != nil {
			return dispatchSettings{}, err
		}
		if args.PublicOrigin != "" {
			return dispatchSettings{}, core.InputErrorf("publicOrigin", "publicOrigin is refused beside the tailnet inputs: the front door serves Dispatch at <hostname>.<tailnetDnsName>, which is then its origin, and a second origin beside that one is outsideOrigin; unset publicOrigin, or unset the tailnet inputs")
		}
		var err error
		if outsideOrigin, err = refuseDispatchOutsideOrigin(args, core.TailnetHostName(args.Hostname, args.TailnetDnsName), "the front door's", signInOn); err != nil {
			return dispatchSettings{}, err
		}
	case args.PublicOrigin != "":
		origin, err := refuseDispatchPublicOrigin(args, signInOn)
		if err != nil {
			return dispatchSettings{}, err
		}
		if err := refuseDispatchWithoutFrontDoor(args); err != nil {
			return dispatchSettings{}, err
		}
		publicOrigin = origin.String()
		if outsideOrigin, err = refuseDispatchOutsideOrigin(args, origin.Hostname(), "publicOrigin's", signInOn); err != nil {
			return dispatchSettings{}, err
		}
	default:
		if err := refuseDispatchSandbox(args, signInGroup); err != nil {
			return dispatchSettings{}, err
		}
	}
	if signInOn {
		if err := refuseDispatchSignInGroup(args, dispatchGroupClause("sign-in", signInGroup)); err != nil {
			return dispatchSettings{}, err
		}
		if err := refuseDispatchWorkspaceDomains(args.SignInWorkspaceDomains); err != nil {
			return dispatchSettings{}, err
		}
	} else if args.SignInOutside {
		return dispatchSettings{}, core.InputErrorf("signInOutside", "signInOutside needs browser sign-in: it lets Google accounts outside signInDomain sign in beside the domain's own people%s",
			dispatchGroupClause("sign-in", signInGroup))
	} else if len(args.SignInWorkspaceDomains) > 0 {
		return dispatchSettings{}, core.InputErrorf("signInWorkspaceDomains", "signInWorkspaceDomains needs browser sign-in: it names the other domains of the Google Workspace that signInDomain names, which Dispatch reads only with Google sign-in; set the sign-in inputs, or unset signInWorkspaceDomains%s",
			dispatchGroupClause("sign-in", signInGroup))
	}
	// Passkey sign-in is served at the second origin alone, whose host the passkey is made for, and
	// a person from outside enrolls a passkey with an invitation a project owner makes while signed
	// in through Google, so it needs the sign-in inputs too; their clause names each one still unset.
	// It replaces the Google sign-in of people from outside that signInOutside turns on, so the two
	// are refused together. The sandbox and the IAP door have refused it above, each by a rule of
	// its own.
	if args.SignInPasskey {
		if outsideOrigin == "" {
			return dispatchSettings{}, core.InputErrorf("signInPasskey", "signInPasskey needs outsideOrigin: Dispatch serves passkey sign-in at that origin alone, and a passkey is made for its host; set outsideOrigin, or unset signInPasskey")
		}
		if !signInOn {
			return dispatchSettings{}, core.InputErrorf("signInPasskey", "signInPasskey needs browser sign-in: a project owner, signed in through Google, makes the invitation a person from outside enrolls a passkey with, so without it nobody could invite anyone; set the sign-in inputs, or unset signInPasskey%s",
				dispatchGroupClause("sign-in", signInGroup))
		}
		if args.SignInOutside {
			return dispatchSettings{}, core.InputErrorf("signInPasskey", "signInPasskey is refused beside signInOutside: passkey sign-in replaces the Google sign-in of people from outside the company that signInOutside turns on; unset signInOutside, or unset signInPasskey")
		}
	}
	// The app's two images, which the app's pod runs in either mode: they belong to no group, so
	// every consumer sets both and neither refusal trails a clause. The front door's two are the
	// tailnet group's, held to this same digest rule by that group's own function.
	for _, image := range []struct{ field, ref string }{
		{"envoyImage", args.EnvoyImage},
		{"cloudSqlProxyImage", args.CloudSqlProxyImage},
	} {
		if !imageRule.MatchString(image.ref) {
			return dispatchSettings{}, core.InputErrorf(image.field, "%s %q must be pinned by digest, as <name>@sha256:<64 lowercase hex digits>", image.field, image.ref)
		}
	}
	if err := refuseDispatchRegion(args.Region); err != nil {
		return dispatchSettings{}, err
	}
	existingCluster, err := resolveDispatchExistingCluster(args)
	if err != nil {
		return dispatchSettings{}, err
	}
	// The component's own cluster runs on the regional subnetwork, so regionalNetwork false is
	// taken only beside a cluster the component does not make, and refused rather than dropped
	// without one.
	regionalNetwork := dispatchRegionalNetwork(args.RegionalNetwork)
	if !regionalNetwork && existingCluster == nil {
		return dispatchSettings{}, core.InputErrorf("regionalNetwork",
			"regionalNetwork false leaves the component's own cluster no subnetwork: set existingCluster, or unset regionalNetwork")
	}
	existingDatabaseInstance, err := newDispatchExistingDatabaseInstance(args)
	if err != nil {
		return dispatchSettings{}, err
	}
	if args.NatsUrl == nil {
		return dispatchSettings{}, core.InputErrorf("natsUrl", "natsUrl is required")
	}
	if args.NatsSeedSecret == nil {
		return dispatchSettings{}, core.InputErrorf("natsSeedSecret", "natsSeedSecret is required")
	}
	if args.AgentSecretsAudience != "" && args.AgentSecretsUrl == "" {
		return dispatchSettings{}, core.InputErrorf("agentSecretsUrl", "agentSecretsUrl is required when agentSecretsAudience is set")
	}
	agentSecretsURL := strings.TrimSuffix(args.AgentSecretsUrl, "/")
	if args.AgentSecretsUrl != "" {
		if args.AgentSecretsAudience == "" {
			return dispatchSettings{}, core.InputErrorf("agentSecretsAudience", "agentSecretsAudience is required when agentSecretsUrl is set")
		}
		// The shape Dispatch itself requires of DISPATCH_AGENT_SECRETS_URL, checked here so a bad
		// URL fails the resolver rather than the pod's boot. It is rendered as a plain environment
		// variable, so a user, a password, a query or a fragment, any of which could carry a
		// credential, is refused too, and no refusal quotes it.
		parsed, err := url.Parse(agentSecretsURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.Path != "" {
			return dispatchSettings{}, core.InputErrorf("agentSecretsUrl", "agentSecretsUrl must be an absolute http(s) URL with no path, as Dispatch requires of DISPATCH_AGENT_SECRETS_URL (not echoed)")
		}
		if urlCarriesExtras(agentSecretsURL, parsed) {
			return dispatchSettings{}, core.InputErrorf("agentSecretsUrl", "agentSecretsUrl must carry no user, password, query or fragment: it is rendered as a plain environment variable (not echoed)")
		}
	}
	// The service-account verifier is one required input, one override and two allow-lists, not
	// a group: the audience turns it on, the issuer defaults to the cluster's own, and the two
	// lists narrow which accounts it admits and which of them read the membership answer, so any
	// of the three sent without an audience would configure nothing and is refused rather than
	// dropped. Dispatch itself refuses half the issuer-audience pair (oidc.ConfigFromEnv), which
	// is why the component renders DISPATCH_OIDC_ISSUER and DISPATCH_OIDC_AUDIENCE together or not
	// at all. The refusal names oidcAudience, the input the operator has to set.
	if args.OidcAudience == "" {
		if err := refuseDispatchInputsWithout("oidcAudience", "the audience turns on Dispatch's service-account verifier, which oidcIssuer, oidcAccounts and accessAccounts only configure", []dispatchInput{
			{"oidcIssuer", args.OidcIssuer != ""},
			{"oidcAccounts", len(args.OidcAccounts) > 0},
			{"accessAccounts", len(args.AccessAccounts) > 0},
		}); err != nil {
			return dispatchSettings{}, core.InputErrorf("oidcAudience", "%w", errors.Unwrap(err))
		}
	}
	// The issuer is the URL Dispatch discovers keys from and compares every token's iss to, so it is
	// held to the shape the hub listener's is: an empty query or fragment, which url.Parse reads as
	// none, is not the issuer a token names.
	if args.OidcIssuer != "" && !issuerURL(args.OidcIssuer) {
		return dispatchSettings{}, core.InputErrorf("oidcIssuer", "oidcIssuer %q must be an https URL with a host and no user, query or fragment, such as https://container.googleapis.com/v1/projects/<project>/locations/<location>/clusters/<cluster>", args.OidcIssuer)
	}
	if err := refuseDispatchAccessValues(args); err != nil {
		return dispatchSettings{}, err
	}
	var appSet, appMissing []string
	for _, in := range []struct{ input, value string }{
		{"appClientId", args.AppClientId},
		{"appId", args.AppId},
		{"appPrivateKeySecretName", args.AppPrivateKeySecretName},
		{"appPrivateKeySecretKey", args.AppPrivateKeySecretKey},
	} {
		if in.value != "" {
			appSet = append(appSet, in.input)
		} else {
			appMissing = append(appMissing, in.input)
		}
	}
	if len(appSet) > 0 && len(appMissing) > 0 {
		return dispatchSettings{}, core.InputErrorf(appMissing[0], "%s required when %s is set: appClientId, appId, appPrivateKeySecretName and appPrivateKeySecretKey are set together or not at all",
			strings.Join(appMissing, ", "), appSet[0])
	}
	if len(appSet) > 0 {
		if id, err := strconv.ParseUint(args.AppId, 10, 64); err != nil || id == 0 {
			return dispatchSettings{}, core.InputErrorf("appId", "appId must be the GitHub App's numeric ID: digits only, not 0 (not echoed)")
		}
		if !dnsSubdomain(args.AppPrivateKeySecretName) {
			return dispatchSettings{}, core.InputErrorf("appPrivateKeySecretName", "appPrivateKeySecretName %q must be a DNS subdomain", args.AppPrivateKeySecretName)
		}
		if !secretKeyRule.MatchString(args.AppPrivateKeySecretKey) {
			return dispatchSettings{}, core.InputErrorf("appPrivateKeySecretKey", "appPrivateKeySecretKey %q must match %s", args.AppPrivateKeySecretKey, secretKeyRule)
		}
	}
	azure, err := newDispatchAzure(args)
	if err != nil {
		return dispatchSettings{}, err
	}
	// The answerer is resolved after the embedder, since the Azure chat form signs in with the
	// credential newDispatchAzure judged: the two features share it.
	answerer, err := newDispatchAnswerer(args, azure.keyless)
	if err != nil {
		return dispatchSettings{}, err
	}

	s := dispatchSettings{
		frontDoor:       frontDoor,
		signInOn:        signInOn,
		hostname:        args.Hostname,
		tailnetDNSName:  args.TailnetDnsName,
		region:          args.Region,
		tags:            args.Tags,
		databaseVersion: defaultDispatchDatabaseVersion,
		databaseTier:    defaultDispatchDatabaseTier,
		namespace:       defaultDispatchNamespace,
		existingCluster: existingCluster,
	}
	s.existingDatabaseInstance = existingDatabaseInstance
	s.regionalNetwork = regionalNetwork
	s.images.envoy = args.EnvoyImage
	s.images.tailscale = args.TailscaleImage
	s.images.proxy = args.CloudSqlProxyImage
	s.images.tools = args.FrontDoorToolsImage
	s.signIn.clientID = args.SignInClientId
	// The domain and each Workspace domain render trimmed, which is what Dispatch reads of them
	// (dispatchDomainName, workspaceDomainRule).
	s.signIn.domain = strings.TrimSpace(args.SignInDomain)
	s.signInOutside = args.SignInOutside
	s.outsideOrigin = outsideOrigin
	s.signInPasskey = args.SignInPasskey
	if len(args.SignInWorkspaceDomains) > 0 {
		s.signInWorkspaceDomains = make([]string, len(args.SignInWorkspaceDomains))
		for i, domain := range args.SignInWorkspaceDomains {
			s.signInWorkspaceDomains[i] = strings.TrimSpace(domain)
		}
	}
	s.publicOrigin = publicOrigin
	s.iap = args.IapAudience != nil
	s.agentSecretsURL = agentSecretsURL
	s.agentSecretsAudience = args.AgentSecretsAudience
	s.oidcAudience = args.OidcAudience
	s.oidcIssuer = args.OidcIssuer
	s.oidcAccounts = args.OidcAccounts
	s.accessAccounts = args.AccessAccounts
	s.projectOwnerFallback = args.ProjectOwnerFallback
	s.privateProjects = args.PrivateProjects != nil && *args.PrivateProjects
	s.appClientID = args.AppClientId
	s.appID = args.AppId
	s.appKeySecretName = args.AppPrivateKeySecretName
	s.appKeySecretKey = args.AppPrivateKeySecretKey
	s.azure = azure
	s.answerer = answerer
	if args.DatabaseVersion != nil {
		if *args.DatabaseVersion == "" {
			return dispatchSettings{}, core.InputErrorf("databaseVersion", "databaseVersion is empty; leave it unset for %s", defaultDispatchDatabaseVersion)
		}
		s.databaseVersion = *args.DatabaseVersion
	}
	if args.DatabaseTier != nil {
		if *args.DatabaseTier == "" {
			return dispatchSettings{}, core.InputErrorf("databaseTier", "databaseTier is empty; leave it unset for %s", defaultDispatchDatabaseTier)
		}
		s.databaseTier = *args.DatabaseTier
	}
	s.databaseAvailabilityType = dispatchAvailabilityZonal
	if args.DatabaseAvailabilityType != nil {
		switch availability := *args.DatabaseAvailabilityType; availability {
		case dispatchAvailabilityZonal, dispatchAvailabilityRegional:
			s.databaseAvailabilityType = availability
		default:
			return dispatchSettings{}, core.InputErrorf("databaseAvailabilityType", "databaseAvailabilityType %q must be %s or %s; leave it unset for %s",
				availability, dispatchAvailabilityZonal, dispatchAvailabilityRegional, dispatchAvailabilityZonal)
		}
	}
	s.deletionProtection = dispatchDeletionProtection(args.DeletionProtection)
	if s.replicas, err = dispatchReplicas("replicas", args.Replicas, defaultDispatchReplicas); err != nil {
		return dispatchSettings{}, err
	}
	if s.frontDoorReplicas, err = dispatchReplicas("frontDoorReplicas", args.FrontDoorReplicas, defaultDispatchFrontDoorReplicas); err != nil {
		return dispatchSettings{}, err
	}
	if s.listenerReplicas, err = dispatchReplicas("listenerReplicas", args.ListenerReplicas, defaultDispatchListenerReplicas); err != nil {
		return dispatchSettings{}, err
	}
	s.natsInCluster = args.NatsInCluster != nil && *args.NatsInCluster
	// The two deployment settings both NATS clients carry, each unset (0) rendering nothing: the
	// binaries then keep one copy and set no byte limit (bus.ParseNATSReplicas,
	// bus.ParseStreamMaxBytes).
	if args.NatsReplicas != nil {
		replicas := *args.NatsReplicas
		switch {
		case replicas < 1:
			return dispatchSettings{}, core.InputErrorf("natsReplicas", "natsReplicas %d is below 1; leave it unset for one copy", replicas)
		case replicas > bus.MaxReplicas:
			return dispatchSettings{}, core.InputErrorf("natsReplicas", "natsReplicas %d is above %d, the most copies NATS keeps", replicas, bus.MaxReplicas)
		}
		s.natsReplicas = replicas
	}
	if args.StreamMaxBytes != nil {
		if limit := *args.StreamMaxBytes; limit < 1 {
			return dispatchSettings{}, core.InputErrorf("streamMaxBytes", "streamMaxBytes %d is below 1; leave it unset for no limit", limit)
		}
		s.streamMaxBytes = *args.StreamMaxBytes
	}
	// The listener's sender limit, which the app does not read. It is checked here, before anything
	// registers: dispatchListenerEnv runs only once natsUrl resolves, and reports every refusal of
	// core's as natsUrl's.
	if s.senderMessagesPerMinute, s.senderMessageBurst, err = core.CheckSenderLimit("", args.SenderMessagesPerMinute, args.SenderMessageBurst); err != nil {
		return dispatchSettings{}, err
	}
	if s.signingKeyGenerations, err = newDispatchSigningKeyGenerations(args); err != nil {
		return dispatchSettings{}, err
	}
	if s.appResources, err = newDispatchAppResources(args); err != nil {
		return dispatchSettings{}, err
	}
	if s.pprofPort, err = newDispatchPprofPort(args); err != nil {
		return dispatchSettings{}, err
	}
	if s.disruption, err = newDispatchDisruption(args, s.replicas, s.frontDoorReplicas, s.listenerReplicas); err != nil {
		return dispatchSettings{}, err
	}
	s.existingNamespace = args.ExistingNamespace != nil && *args.ExistingNamespace
	if args.Namespace != nil {
		if !dnsLabel(*args.Namespace) {
			return dispatchSettings{}, core.InputErrorf("namespace", "namespace %q must be a DNS label", *args.Namespace)
		}
		s.namespace = *args.Namespace
	}
	for i, namespace := range args.AllowedNamespaces {
		if !dnsLabel(namespace) {
			return dispatchSettings{}, core.InputErrorf("allowedNamespaces", "allowedNamespaces[%d] %q must be a DNS label", i, namespace)
		}
	}
	for i, app := range args.AllowedApps {
		if !appLabelValueRule.MatchString(app) {
			return dispatchSettings{}, core.InputErrorf("allowedApps", "allowedApps[%d] %q must be a pod's app label value: 1 to 63 letters, digits, dashes, underscores and dots, a letter or digit at each end", i, app)
		}
	}
	return s, nil
}

// dnsLabel reports whether value is a DNS-1123 label: namespaceRule's shape, at most 63 characters.
func dnsLabel(value string) bool {
	return len(value) <= 63 && namespaceRule.MatchString(value)
}

// dnsSubdomain reports whether value is a DNS-1123 subdomain, the shape of a Kubernetes Secret's
// name: dot-separated DNS labels, at most 253 characters.
func dnsSubdomain(value string) bool {
	if value == "" || len(value) > 253 {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if !dnsLabel(label) {
			return false
		}
	}
	return true
}

// podPriorityClass resolves input, the name of a PriorityClass every pod of a component names, ""
// when unset: gcp:Dispatch's and gcp:ClusteredNatsHub's priorityClassName. It refuses a name that
// is empty or not a DNS subdomain, and one with the system- prefix, which Kubernetes reserves for
// its own system-cluster-critical and system-node-critical classes: those outrank every workload
// so the cluster's own components are scheduled first, and an application pod holding one could
// preempt them. Each refusal is a core.InputError naming input.
func podPriorityClass(input string, name *string) (string, error) {
	if name == nil {
		return "", nil
	}
	switch {
	case *name == "":
		return "", core.InputErrorf(input, "%s is empty; leave it unset for no priority class", input)
	case !dnsSubdomain(*name):
		return "", core.InputErrorf(input, "%s %q must be a DNS subdomain, the shape of a PriorityClass's name", input, *name)
	case strings.HasPrefix(*name, "system-"):
		return "", core.InputErrorf(input, "%s %q is a class Kubernetes reserves for its own system components (the system- prefix); name a PriorityClass of your own", input, *name)
	}
	return *name, nil
}

// newDispatchPprofPort resolves pprofPort, the port envoy-dispatch's profiler binds on the pod's
// loopback: 0, rendering nothing, when it is unset. It refuses a value outside 1-65535, which
// Dispatch would refuse at startup, and the two ports the app pod already binds on its loopback,
// envoy-dispatch's own 8766 and the cloud-sql-proxy sidecar's 5432, where the bind would fail and
// the pod would never start.
func newDispatchPprofPort(args *DispatchArgs) (int, error) {
	if args.PprofPort == nil {
		return 0, nil
	}
	port := *args.PprofPort
	switch {
	case port < 1 || port > 65535:
		return 0, core.InputErrorf("pprofPort", "pprofPort %d must be a port number 1-65535; leave it unset for no profiler", port)
	case port == 8766:
		return 0, core.InputErrorf("pprofPort", "pprofPort %d is envoy-dispatch's own port; choose another, such as 6060", port)
	case port == 5432:
		return 0, core.InputErrorf("pprofPort", "pprofPort %d is the cloud-sql-proxy sidecar's port in the same pod; choose another, such as 6060", port)
	}
	return port, nil
}

// newDispatchSigningKeyGenerations resolves signingKeyGenerations, the generations the component
// registers a signing key for and the order the server holds them in: [0] when the input is unset
// or empty, which is the single key this component has always registered, since generation 0
// keeps the name <hostname>-signing-key and is never replaced. A generation above 0 is a key of
// its own, <hostname>-signing-key-<n>, and only the generations the list names are registered, so
// one the list drops is destroyed and that key cannot come back.
//
// Two refusals, both before any resource is registered. A negative generation names no resource.
// A repeated generation would render the same key twice, which parseSettingEntries refuses at the
// server's boot, so it is refused here rather than shipped as a Secret no pod can start on.
//
// The resolver sees one state and never the one before it, so the rest is what an operator keeps
// and the component cannot refuse:
//   - [0] to [1] with no add step. Every process refuses to start beside a live one whose key set
//     does not share its current key both ways (processes.keySetsShared), and at one replica,
//     whose Deployment strategy is Recreate, the running pod is gone before the new one starts,
//     so that apply signs everyone out rather than being refused.
//   - [0, 1] to [1], the same skip from the other end: the old key is dropped in the very apply
//     that promotes the new one.
//   - dropping the old generation sooner than 30 days after the promote, a session cookie's life
//     (auth's sessionMaxAgeSeconds): every cookie and every refresh token still under that key
//     dies with it.
//   - a rollback to a generation the list already dropped, whose key the random provider
//     destroyed and which no later apply brings back.
func newDispatchSigningKeyGenerations(args *DispatchArgs) ([]int, error) {
	if len(args.SigningKeyGenerations) == 0 {
		return []int{0}, nil
	}
	for i, generation := range args.SigningKeyGenerations {
		switch {
		case generation < 0:
			return nil, core.InputErrorf("signingKeyGenerations", "signingKeyGenerations[%d] is generation %d, which names no resource: a generation is 0, the key a running deployment already holds, or a number above it", i, generation)
		case slices.Contains(args.SigningKeyGenerations[:i], generation):
			return nil, core.InputErrorf("signingKeyGenerations", "signingKeyGenerations[%d] repeats generation %d, which would render the same key twice; the server refuses a repeated DISPATCH_SIGNING_KEY entry at boot", i, generation)
		}
	}
	return args.SigningKeyGenerations, nil
}

// dispatchReplicas resolves one Deployment's replica count, input, to fallback when it is unset,
// refusing a count below minDispatchReplicas, which leaves the workload with no pod.
func dispatchReplicas(input string, value *int, fallback int) (int, error) {
	if value == nil {
		return fallback, nil
	}
	if *value < minDispatchReplicas {
		return 0, core.InputErrorf(input, "%s %d must be at least %d", input, *value, minDispatchReplicas)
	}
	return *value, nil
}

// dispatchDisruption is the pods' resolved disruption protection: the one priority class every
// pod of the three Deployments names, and each Deployment's PodDisruptionBudget minAvailable. ""
// and 0, rendering nothing, are unset.
type dispatchDisruption struct {
	priorityClass                                string
	appPDBMinAvailable, frontDoorPDBMinAvailable int
	listenerPDBMinAvailable                      int
}

// newDispatchDisruption resolves priorityClassName, by podPriorityClass's rule, and the three
// budgets, appPdbMinAvailable, frontDoorPdbMinAvailable and listenerPdbMinAvailable, each refusal
// naming its own input. It refuses a budget below 1, which protects no pod, and above its
// Deployment's replica count, which no number of ready pods can meet.
func newDispatchDisruption(args *DispatchArgs, replicas, frontDoorReplicas, listenerReplicas int) (dispatchDisruption, error) {
	var d dispatchDisruption
	var err error
	if d.priorityClass, err = podPriorityClass("priorityClassName", args.PriorityClassName); err != nil {
		return dispatchDisruption{}, err
	}
	for _, budget := range []struct {
		input, replicasInput string
		value                *int
		replicas             int
		into                 *int
	}{
		{"appPdbMinAvailable", "replicas", args.AppPdbMinAvailable, replicas, &d.appPDBMinAvailable},
		{"frontDoorPdbMinAvailable", "frontDoorReplicas", args.FrontDoorPdbMinAvailable, frontDoorReplicas, &d.frontDoorPDBMinAvailable},
		{"listenerPdbMinAvailable", "listenerReplicas", args.ListenerPdbMinAvailable, listenerReplicas, &d.listenerPDBMinAvailable},
	} {
		if budget.value == nil {
			continue
		}
		if *budget.value < 1 || *budget.value > budget.replicas {
			return dispatchDisruption{}, core.InputErrorf(budget.input, "%s %d must be from 1 to %s (%d); leave it unset for no PodDisruptionBudget",
				budget.input, *budget.value, budget.replicasInput, budget.replicas)
		}
		*budget.into = *budget.value
	}
	return d, nil
}

// dispatchAppResources is the app pod's resolved container sizes: envoy-dispatch's request and
// limit, appCPU and appMemory, and the cloud-sql-proxy sidecar's, proxyCPU and proxyMemory; and
// each container's ephemeral storage, "" (rendering nothing) when its input is unset.
type dispatchAppResources struct {
	appCPU, appMemory, proxyCPU, proxyMemory   string
	appEphemeralStorage, proxyEphemeralStorage string
}

// newDispatchAppResources resolves appCpu and appMemory, the envoy-dispatch container's request
// and limit, and proxyCpu and proxyMemory, the cloud-sql-proxy sidecar's, and refuses a size at
// which Autopilot would not run the app pod as rendered: the two containers' total below
// autopilotPodMinCPU or autopilotPodMinMemory or outside the memory-per-CPU ratio (Autopilot
// raises it), or above autopilotPodMaxCPU or autopilotPodMaxMemory (Autopilot rejects it). Each
// refusal's message names the inputs whose sum it judges, and its core.InputError the one to
// correct (podSizeInput).
func newDispatchAppResources(args *DispatchArgs) (dispatchAppResources, error) {
	cpu, appCPU, err := appQuantity("appCpu", args.AppCpu, defaultDispatchAppCPU, "1, 1500m or 2")
	if err != nil {
		return dispatchAppResources{}, err
	}
	memory, appMemory, err := appQuantity("appMemory", args.AppMemory, defaultDispatchAppMemory, "4Gi or 6144Mi")
	if err != nil {
		return dispatchAppResources{}, err
	}
	proxyCPU, proxyCPUValue, err := appQuantity("proxyCpu", args.ProxyCpu, defaultDispatchProxyCPU, "1, 1500m or 2")
	if err != nil {
		return dispatchAppResources{}, err
	}
	proxyMemory, proxyMemoryValue, err := appQuantity("proxyMemory", args.ProxyMemory, defaultDispatchProxyMemory, "2Gi or 1536Mi")
	if err != nil {
		return dispatchAppResources{}, err
	}
	podCPU := new(big.Rat).Add(appCPU, proxyCPUValue)
	podMemory := new(big.Rat).Add(appMemory, proxyMemoryValue)
	cpuInput := podSizeInput("appCpu", "proxyCpu", args.AppCpu != nil, args.ProxyCpu != nil)
	memoryInput := podSizeInput("appMemory", "proxyMemory", args.AppMemory != nil, args.ProxyMemory != nil)
	if podCPU.Cmp(autopilotPodMinCPU) < 0 {
		return dispatchAppResources{}, core.InputErrorf(cpuInput, "appCpu %q and proxyCpu %q must make at least %s together: below it Autopilot raises the app pod's request, since the pod carries pod anti-affinity (and safe-to-evict when no budget covers it)",
			cpu, proxyCPU, milliCPU(autopilotPodMinCPU))
	}
	if podCPU.Cmp(autopilotPodMaxCPU) > 0 {
		return dispatchAppResources{}, core.InputErrorf(cpuInput, "appCpu %q and proxyCpu %q must make at most %s CPU together: Autopilot rejects an app pod that requests more",
			cpu, proxyCPU, autopilotPodMaxCPU.RatString())
	}
	if podMemory.Cmp(autopilotPodMinMemory) < 0 {
		return dispatchAppResources{}, core.InputErrorf(memoryInput, "appMemory %q and proxyMemory %q must make at least %s together: below it Autopilot raises the app pod's request, since the pod carries pod anti-affinity (and safe-to-evict when no budget covers it)",
			memory, proxyMemory, mebibytes(autopilotPodMinMemory))
	}
	if podMemory.Cmp(autopilotPodMaxMemory) > 0 {
		return dispatchAppResources{}, core.InputErrorf(memoryInput, "appMemory %q and proxyMemory %q must make at most 110Gi together: Autopilot rejects an app pod that requests more",
			memory, proxyMemory)
	}
	gibPerCPU := new(big.Rat).Quo(podMemory, podCPU)
	gibPerCPU.Quo(gibPerCPU, new(big.Rat).SetInt64(1<<30))
	if gibPerCPU.Cmp(autopilotMinGiBPerCPU) < 0 || gibPerCPU.Cmp(autopilotMaxGiBPerCPU) > 0 {
		// Autopilot raises the smaller of the two, so the refusal names an input of that one, as
		// autopilotPodSize's does: too little memory per CPU raises the memory.
		smaller := cpuInput
		if gibPerCPU.Cmp(autopilotMinGiBPerCPU) < 0 {
			smaller = memoryInput
		}
		return dispatchAppResources{}, core.InputErrorf(smaller, "appCpu %q, appMemory %q, proxyCpu %q and proxyMemory %q: the app pod must hold between 1 and 6.5 GiB of memory per CPU, or Autopilot raises the smaller of the two",
			cpu, memory, proxyCPU, proxyMemory)
	}
	appEphemeral, proxyEphemeral, err := newDispatchEphemeralStorage(args)
	if err != nil {
		return dispatchAppResources{}, err
	}
	return dispatchAppResources{appCPU: cpu, appMemory: memory, proxyCPU: proxyCPU, proxyMemory: proxyMemory,
		appEphemeralStorage: appEphemeral, proxyEphemeralStorage: proxyEphemeral}, nil
}

// podSizeInput is the input a refusal of the app pod's summed size names as the one to correct:
// app, the envoy-dispatch container's, unless the consumer set only proxy, the cloud-sql-proxy
// sidecar's, which is then the value that took the sum out of Autopilot's bounds.
func podSizeInput(app, proxy string, appSet, proxySet bool) string {
	if proxySet && !appSet {
		return proxy
	}
	return app
}

// newDispatchEphemeralStorage resolves appEphemeralStorage and proxyEphemeralStorage, each "" when
// unset, and refuses a pair at which Autopilot would not run the app pod as rendered: their sum,
// an unset container counted at the autopilotDefaultEphemeralStorage Autopilot gives it, below
// autopilotPodMinEphemeralStorage (Autopilot raises it) or above autopilotPodMaxEphemeralStorage
// (Autopilot rejects it). The refusal's message names both inputs, an unset one as Autopilot's
// default, and its core.InputError the one to correct (podSizeInput).
func newDispatchEphemeralStorage(args *DispatchArgs) (app, proxy string, err error) {
	resolve := func(input string, value *string) (rendered, named string, quantity *big.Rat, err error) {
		if value == nil {
			quantity, _ = quantityValue(autopilotDefaultEphemeralStorage)
			return "", fmt.Sprintf("%s unset (Autopilot's %s)", input, autopilotDefaultEphemeralStorage), quantity, nil
		}
		rendered, quantity, err = appQuantity(input, value, "Autopilot's default of "+autopilotDefaultEphemeralStorage, "4Gi or 4096Mi")
		return rendered, fmt.Sprintf("%s %q", input, rendered), quantity, err
	}
	app, appNamed, appValue, err := resolve("appEphemeralStorage", args.AppEphemeralStorage)
	if err != nil {
		return "", "", err
	}
	proxy, proxyNamed, proxyValue, err := resolve("proxyEphemeralStorage", args.ProxyEphemeralStorage)
	if err != nil {
		return "", "", err
	}
	pod := new(big.Rat).Add(appValue, proxyValue)
	input := podSizeInput("appEphemeralStorage", "proxyEphemeralStorage", args.AppEphemeralStorage != nil, args.ProxyEphemeralStorage != nil)
	if pod.Cmp(autopilotPodMinEphemeralStorage) < 0 {
		return "", "", core.InputErrorf(input, "%s and %s must make at least %s together: below it Autopilot raises the app pod's ephemeral storage, and each container's logs count against its own",
			appNamed, proxyNamed, mebibytes(autopilotPodMinEphemeralStorage))
	}
	if pod.Cmp(autopilotPodMaxEphemeralStorage) > 0 {
		return "", "", core.InputErrorf(input, "%s and %s must make at most 10Gi together: Autopilot rejects an app pod with more ephemeral storage; a larger working set belongs in a generic ephemeral volume",
			appNamed, proxyNamed)
	}
	return app, proxy, nil
}

// appQuantity resolves one container size input of the app pod: fallback when it is unset, and
// its exact value, or a refusal, a core.InputError naming input. It refuses an empty value, which
// is no quantity, and one of 0 or less, which sizes no container: the pod's floor is judged on the
// two containers' sum, so a negative size beside a large one would otherwise pass it.
func appQuantity(input string, value *string, fallback, examples string) (string, *big.Rat, error) {
	resolved := fallback
	if value != nil {
		if *value == "" {
			return "", nil, core.InputErrorf(input, "%s is empty; leave it unset for %s", input, fallback)
		}
		resolved = *value
	}
	quantity, err := quantityValue(resolved)
	switch {
	case errors.Is(err, errQuantityExponent):
		return "", nil, core.InputErrorf(input, "%s %q has a decimal exponent outside e%d to e%d, the range of the suffixes n to E; write it with a suffix, such as %s", input, resolved, minQuantityExponent, maxQuantityExponent, examples)
	case err != nil:
		return "", nil, core.InputErrorf(input, "%s %q is not a Kubernetes quantity, such as %s", input, resolved, examples)
	case quantity.Sign() <= 0:
		return "", nil, core.InputErrorf(input, "%s %q must be more than 0, such as %s", input, resolved, examples)
	}
	return resolved, quantity, nil
}

var (
	quantityBinarySuffixes  = map[string]uint{"Ki": 10, "Mi": 20, "Gi": 30, "Ti": 40, "Pi": 50, "Ei": 60}
	quantityDecimalSuffixes = map[string]int{"n": -9, "u": -6, "m": -3, "": 0, "k": 3, "M": 6, "G": 9, "T": 12, "P": 15, "E": 18}

	errNotQuantity      = errors.New("not a Kubernetes quantity")
	errQuantityExponent = errors.New("decimal exponent out of range")
)

// minQuantityExponent and maxQuantityExponent bound a quantity's decimal exponent (the e<n> form)
// to the range its suffixes cover, n to E. Every value inside it is computed exactly, whatever
// its mantissa; one outside it is refused rather than computed, since 10^n costs time without
// bound, and no size this component takes needs one.
const (
	minQuantityExponent = -9
	maxQuantityExponent = 18
)

// quantityValue is the exact value of a Kubernetes quantity (CPU in cores, memory in bytes):
// errNotQuantity when value is not one, errQuantityExponent when its decimal exponent is outside
// minQuantityExponent to maxQuantityExponent.
func quantityValue(value string) (*big.Rat, error) {
	match := quantityRule.FindStringSubmatch(value)
	if match == nil {
		return nil, errNotQuantity
	}
	number, ok := new(big.Rat).SetString(match[1])
	if !ok {
		return nil, errNotQuantity
	}
	suffix := match[2]
	if shift, ok := quantityBinarySuffixes[suffix]; ok {
		return number.Mul(number, new(big.Rat).SetInt(new(big.Int).Lsh(big.NewInt(1), shift))), nil
	}
	exponent, ok := quantityDecimalSuffixes[suffix]
	if !ok {
		// A decimal exponent, whose digits quantityRule guarantees; Atoi fails only past int's
		// range, which is outside the bounds too.
		var err error
		exponent, err = strconv.Atoi(suffix[1:])
		if err != nil || exponent < minQuantityExponent || exponent > maxQuantityExponent {
			return nil, errQuantityExponent
		}
	}
	power := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(abs(exponent))), nil)
	if exponent < 0 {
		return number.Quo(number, new(big.Rat).SetInt(power)), nil
	}
	return number.Mul(number, new(big.Rat).SetInt(power)), nil
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// milliCPU writes a whole number of millicores as a quantity, such as 300m.
func milliCPU(cores *big.Rat) string {
	return new(big.Rat).Mul(cores, big.NewRat(1000, 1)).FloatString(0) + "m"
}

// mebibytes writes a whole number of mebibytes as a quantity, such as 256Mi.
func mebibytes(bytes *big.Rat) string {
	return new(big.Rat).Quo(bytes, new(big.Rat).SetInt64(1<<20)).FloatString(0) + "Mi"
}

// NewDispatch registers a Dispatch component after resolving all plain inputs, requiring the
// providers its network, cluster, database, generated secrets, and Kubernetes resources use, and
// looking up the cluster existingCluster names, when it names one. The cluster is then that one,
// which the component registers nothing for, or else the component's own; and the Cloud SQL
// instance is the one existingDatabaseInstance names, for which the component registers no
// DatabaseInstance, Database or User, or else the component's own. Whichever they are, the
// component registers its network (newDispatchNetwork).
func NewDispatch(ctx *pulumi.Context, name string, args *DispatchArgs, opts ...pulumi.ResourceOption) (*Dispatch, error) {
	// The name is quoted in every refusal below and the component registers under it, so a seed
	// there is refused first, as NewNatsHub refuses one in its own name.
	if bus.HoldsSeed(name) {
		return nil, fmt.Errorf("%s: %w", dispatchType, core.SeedRefusal("the component's name"))
	}
	s, err := newDispatchSettings(args)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", dispatchType, name, err)
	}
	if err := providers.Require(ctx.Context(), dispatchType, "gcp", opts...); err != nil {
		return nil, err
	}
	// clusterOnly registers no RandomPassword, so it requires no random provider: a cluster-only
	// program would otherwise have to declare one it never uses.
	if !s.clusterOnly {
		if err := providers.Require(ctx.Context(), dispatchType, "random", opts...); err != nil {
			return nil, err
		}
	}
	// Only the front door declares Dispatch's Tailscale Service, so a consumer that builds none —
	// the sandbox, and clusterOnly, which leave the mode off — declares no tailscale provider.
	if s.frontDoor {
		if err := providers.Require(ctx.Context(), dispatchType, "tailscale", opts...); err != nil {
			return nil, err
		}
	}
	// The gcp provider among opts, which the first providers check found there, read once: the
	// lookup of the cluster existingCluster names and the network half's project lookup both go
	// through it.
	gcpProvider, err := providerFor(ctx.Context(), "gcp", opts...)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", dispatchType, name, err)
	}
	// The cluster half is decided once, from existingCluster alone: the component's own cluster
	// (newDispatchCluster), or else the cluster existingCluster names (newDispatchExistingCluster).
	// That cluster is looked up here, after the providers checks and before anything registers, so
	// a cluster Dispatch cannot run in is refused with nothing registered, and its half builds the
	// kubeconfig from the lookup's plain values. Either half runs after the network half, whose
	// network both read.
	newCluster := newDispatchCluster
	if s.existingCluster != nil {
		existing, err := lookupDispatchCluster(ctx, s.existingCluster, gcpProvider)
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w", dispatchType, name, err)
		}
		newCluster = func(_ *pulumi.Context, _ *Dispatch, name string, _ dispatchSettings, vpc *dispatchNetwork) (*dispatchCluster, error) {
			return newDispatchExistingCluster(name, vpc, existing), nil
		}
	}

	comp := &Dispatch{}
	if err := ctx.RegisterComponentResource(dispatchType, name, comp, opts...); err != nil {
		return nil, err
	}
	vpc, err := newDispatchNetwork(ctx, comp, s, gcpProvider)
	if err != nil {
		return nil, err
	}
	cloud, err := newCluster(ctx, comp, name, s, vpc)
	if err != nil {
		return nil, err
	}
	// The three outputs the cluster alone decides, which both modes carry.
	comp.ClusterName = cloud.name
	comp.Kubeconfig = cloud.kubeconfig
	comp.ServiceAccountIssuer = cloud.issuer
	if s.clusterOnly {
		// The five outputs Dispatch would decide are an explicit empty string: an output field
		// left unset awaits as unknown, which is one value in preview and another in update,
		// while "" is the same in both and is what a consumer already reads as "not applied".
		empty := pulumi.String("").ToStringOutput()
		comp.Url, comp.DatabaseInstance, comp.Principal = empty, empty, empty
		comp.AgentToken, comp.ServiceAccountSubject = empty, empty
		return comp, nil
	}
	database, err := newDispatchDatabase(ctx, comp, name, args, s, vpc, cloud)
	if err != nil {
		return nil, err
	}
	if err := newDispatchKubernetes(ctx, comp, name, args, s, cloud, database); err != nil {
		return nil, err
	}
	comp.DatabaseInstance = database.connectionName
	comp.Principal = database.principal
	comp.AgentToken = database.agentToken.Result
	comp.Url = pulumi.String(s.serverURL()).ToStringOutput()
	// The app's pods run as ServiceAccount <hostname> in the component's namespace, the account
	// newDispatchKubernetes registers, and Kubernetes names a token's subject after it. The
	// listener's pods run as <hostname>-listener and the front door's as <hostname>-tailnet, which
	// this subject never names.
	comp.ServiceAccountSubject = pulumi.String(dispatchServiceAccountSubject(s)).ToStringOutput()
	return comp, nil
}
