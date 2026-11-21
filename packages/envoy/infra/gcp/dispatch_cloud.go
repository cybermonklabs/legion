package gcp

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/pulumi/pulumi-gcp/sdk/v10/go/gcp/compute"
	"github.com/pulumi/pulumi-gcp/sdk/v10/go/gcp/container"
	"github.com/pulumi/pulumi-gcp/sdk/v10/go/gcp/organizations"
	"github.com/pulumi/pulumi-gcp/sdk/v10/go/gcp/secretmanager"
	"github.com/pulumi/pulumi-gcp/sdk/v10/go/gcp/servicenetworking"
	"github.com/pulumi/pulumi-gcp/sdk/v10/go/gcp/sql"
	random "github.com/pulumi/pulumi-random/sdk/v4/go/random"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/internals"

	"github.com/sjawhar/envoy/infra/core"
)

// dispatchNetwork is what the network half gives the rest of the component: the VPC and its
// regional subnetwork, the private services peering, and the project. The network half registers
// apart from the cluster half, so the network, its peering and the project lookup hang on no
// cluster: the cluster half takes the network, never the other way round.
type dispatchNetwork struct {
	// network is the VPC the cluster's nodes are on, the component's own cluster's or the one
	// existingCluster names (newDispatchExistingCluster refuses a cluster on another network), and
	// the component's own Cloud SQL instance takes its private IP on; subnetwork is the range in
	// region the component's own cluster takes its nodes' and pods' addresses from.
	// With regionalNetwork false the network half registers no subnetwork and subnetwork is nil:
	// the resolvers allow that only beside existingCluster, where the component makes no cluster.
	network    *compute.Network
	subnetwork *compute.Subnetwork
	// connection is the private services peering the component's own Cloud SQL instance takes its
	// private IP through. It registers beside existingDatabaseInstance too, unchanged, so naming an
	// instance the component does not make removes no peering. clusterOnly builds no database, so
	// it registers no peering and connection is nil.
	connection *servicenetworking.Connection
	// project is the project whose number and id name the principal and the issuer, and whose id
	// names the Workload Identity pool of the component's own cluster.
	project organizations.LookupProjectResultOutput
}

// dispatchCluster is what the cluster half gives the rest of the component: the cluster's name,
// service-account token issuer, kubeconfig and Workload Identity pool, each as an output, the
// resources a child that reaches the cluster waits for, and what the component's Kubernetes
// provider identifies the cluster by. The cluster is the component's own (newDispatchCluster) or
// the one existingCluster names (newDispatchExistingCluster). Both modes build the cluster half,
// clusterOnly always on the component's own cluster, so every field here is set in either mode,
// but waitFor, which holds nothing for a cluster existingCluster names, and clusterIdentifier,
// which is nil for the component's own.
type dispatchCluster struct {
	// name is the cluster's name, which the component exports as clusterName.
	name pulumi.StringOutput
	// issuer is the cluster's service-account token issuer, and kubeconfig the cluster's
	// exec-plugin kubeconfig, which the component exports and its Kubernetes provider takes.
	issuer     pulumi.StringOutput
	kubeconfig pulumi.StringOutput
	// pool is the Workload Identity pool as the cluster reports it, which names the pods'
	// principal. It is a *string the provider can leave nil, which the principal's apply refuses
	// (outputString).
	pool pulumi.StringPtrOutput
	// waitFor is the resources every Kubernetes child and every grant to the pool depends on: the
	// component's own Cluster, so none of them is made before the cluster exists, and none for a
	// cluster existingCluster names, which exists before the component runs.
	waitFor []pulumi.Resource
	// clusterIdentifier is the Kubernetes provider's clusterIdentifier: the issuer for a cluster
	// existingCluster names, and nil for the component's own cluster, whose provider keeps the
	// inputs it always had. pulumi-kubernetes replaces a provider that carries one only when the
	// identifier changes, not when the cluster's endpoint or CA rotates or the kubeconfig is
	// unknown in a preview, so a looked-up cluster's provider, and every Kubernetes child on it,
	// stays as long as the cluster does.
	clusterIdentifier pulumi.StringPtrInput
}

// dispatchDatabase is what the database half gives the rest of the component: the Cloud SQL
// instance the proxy connects to, the component's own or the one existingDatabaseInstance names,
// the generated secrets, and the pods' principals with the grants they hold: the app's
// (principal) and the listener's, each the Workload Identity principal of its own Kubernetes
// ServiceAccount. clusterOnly registers none of them, so that mode builds no dispatchDatabase at
// all.
type dispatchDatabase struct {
	// connectionName is the Cloud SQL instance's connection name, <project>:<region>:<instance>,
	// which the app pod's cloud-sql-proxy sidecar connects to and the component exports as
	// databaseInstance: the component's own instance's, or the one existingDatabaseInstance gives,
	// once it resolves and is checked (dispatchDatabaseConnectionName).
	connectionName pulumi.StringOutput
	// waitFor is the resources the Tailscale Service and the front door's Deployment depend on, so
	// that the name readers reach Dispatch at comes back only once the instance's in-place restart
	// is over: the component's own DatabaseInstance, and none for an instance
	// existingDatabaseInstance names, which the component neither makes nor changes.
	waitFor  []pulumi.Resource
	password *random.RandomPassword
	// signing is the signing-key set, one generated key per generation of
	// signingKeyGenerations and in that order: the first signs every new cookie, every one
	// verifies.
	signing       []*random.RandomPassword
	agentToken    *random.RandomPassword
	listenerToken *random.RandomPassword
	principal     pulumi.StringOutput
	// seedAccessor is the NATS seed's grant to the app's account, and listenerSeedAccessor its
	// grant to the listener's own account, ServiceAccount <hostname>-listener, the one grant that
	// account holds. Both are nil with natsAudience, where no pod mounts the seed.
	seedAccessor         *secretmanager.SecretIamMember
	listenerSeedAccessor *secretmanager.SecretIamMember
	// azureKeyAccessor is set only with azureEmbedKeySecret, and chatTokenAccessor only with
	// chatTokenSecret: each is the per-secret grant that one feature's mounted secret needs.
	azureKeyAccessor  *secretmanager.SecretIamMember
	chatTokenAccessor *secretmanager.SecretIamMember
}

func providerFor(ctx context.Context, pkg string, opts ...pulumi.ResourceOption) (pulumi.ProviderResource, error) {
	resources, err := pulumi.NewResourceOptions(opts...)
	if err != nil {
		return nil, err
	}
	for _, provider := range resources.Providers {
		out, err := internals.UnsafeAwaitOutput(ctx, provider.URN())
		if err != nil {
			return nil, err
		}
		if urn, ok := out.Value.(pulumi.URN); ok && resource.URN(urn).Type().Name() == tokens.TypeName(pkg) {
			return provider, nil
		}
	}
	return nil, fmt.Errorf("missing %s provider", pkg)
}

// newDispatchNetwork is the network half: it registers the network the component runs on, apart
// from any cluster. That is the VPC, Network <hostname>-network; in region, unless regionalNetwork
// is false, its subnetwork and the router and Cloud NAT that private nodes reach the internet
// through, Subnetwork, Router and RouterNat <hostname>; and, outside clusterOnly, which builds no
// database, the private services range and its peering, GlobalAddress and Connection
// <hostname>-services, through which the component's own Cloud SQL instance takes its private IP.
// Each registers identically whether the component makes its cluster and its instance or runs on
// the ones existingCluster and existingDatabaseInstance name, and the Network, the GlobalAddress
// and the Connection whatever regionalNetwork says too, so turning it false deletes the regional
// three alone. It also looks the project up through gcpProvider, the consumer's gcp provider that
// NewDispatch found. The cluster half takes the network and the subnetwork from it, and the
// database half the network, the connection and the project.
func newDispatchNetwork(ctx *pulumi.Context, comp *Dispatch, s dispatchSettings, gcpProvider pulumi.ProviderResource) (*dispatchNetwork, error) {
	child := pulumi.Parent(comp)
	project := organizations.LookupProjectOutput(ctx, organizations.LookupProjectOutputArgs{}, pulumi.Provider(gcpProvider))
	network, err := compute.NewNetwork(ctx, s.hostname+"-network", &compute.NetworkArgs{AutoCreateSubnetworks: pulumi.Bool(false)}, child)
	if err != nil {
		return nil, err
	}
	vpc := &dispatchNetwork{network: network, project: project}
	// The regional part of the network, which only a cluster in region uses: the subnetwork the
	// component's own cluster takes its addresses from, and the router and Cloud NAT its private
	// nodes reach the internet through. regionalNetwork false, which the resolvers take only beside
	// existingCluster, registers none of the three, so a consumer whose cluster runs elsewhere can
	// empty the region once nothing uses the subnetwork.
	if s.regionalNetwork {
		subnetwork, err := compute.NewSubnetwork(ctx, s.hostname, &compute.SubnetworkArgs{
			Network: network.ID(), Region: pulumi.String(s.region), IpCidrRange: pulumi.String("10.64.0.0/20"), PrivateIpGoogleAccess: pulumi.Bool(true),
		}, child)
		if err != nil {
			return nil, err
		}
		router, err := compute.NewRouter(ctx, s.hostname, &compute.RouterArgs{Network: network.ID(), Region: pulumi.String(s.region)}, child)
		if err != nil {
			return nil, err
		}
		_, err = compute.NewRouterNat(ctx, s.hostname, &compute.RouterNatArgs{
			Router: router.Name, Region: pulumi.String(s.region), NatIpAllocateOption: pulumi.String("AUTO_ONLY"), SourceSubnetworkIpRangesToNat: pulumi.String("ALL_SUBNETWORKS_ALL_IP_RANGES"),
		}, child)
		if err != nil {
			return nil, err
		}
		vpc.subnetwork = subnetwork
	}
	if s.clusterOnly {
		return vpc, nil
	}
	address, err := compute.NewGlobalAddress(ctx, s.hostname+"-services", &compute.GlobalAddressArgs{
		Purpose: pulumi.String("VPC_PEERING"), AddressType: pulumi.String("INTERNAL"), PrefixLength: pulumi.Int(16), Network: network.ID(),
	}, child)
	if err != nil {
		return nil, err
	}
	vpc.connection, err = servicenetworking.NewConnection(ctx, s.hostname+"-services", &servicenetworking.ConnectionArgs{
		Network: network.ID(), Service: pulumi.String("servicenetworking.googleapis.com"), ReservedPeeringRanges: pulumi.StringArray{address.Name},
	}, child)
	if err != nil {
		return nil, err
	}
	return vpc, nil
}

// newDispatchCluster is the cluster half of the component's own cluster: it registers the
// Autopilot cluster, Cluster <hostname> in region, on the network half's network and subnetwork,
// and computes what the rest of the component reads off it: the service-account token issuer, the
// kubeconfig and the Workload Identity pool. With the network half it is all clusterOnly builds.
func newDispatchCluster(ctx *pulumi.Context, comp *Dispatch, name string, s dispatchSettings, vpc *dispatchNetwork) (*dispatchCluster, error) {
	dispatch := dispatchType + " " + name
	child := pulumi.Parent(comp)
	// The cluster's Workload Identity pool; GKE accepts only the project's own, <project id>.svc.id.goog.
	workloadPool := vpc.project.ProjectId().Elem().ApplyT(func(id string) string { return id + ".svc.id.goog" }).(pulumi.StringOutput)
	cluster, err := container.NewCluster(ctx, s.hostname, &container.ClusterArgs{
		Name: pulumi.String(s.hostname), Location: pulumi.String(s.region), EnableAutopilot: pulumi.Bool(true), Network: vpc.network.ID(), Subnetwork: vpc.subnetwork.ID(),
		IpAllocationPolicy:   &container.ClusterIpAllocationPolicyArgs{},
		PrivateClusterConfig: &container.ClusterPrivateClusterConfigArgs{EnablePrivateNodes: pulumi.Bool(true), EnablePrivateEndpoint: pulumi.Bool(false)},
		ReleaseChannel:       &container.ClusterReleaseChannelArgs{Channel: pulumi.String("REGULAR")},
		SecretManagerConfig:  &container.ClusterSecretManagerConfigArgs{Enabled: pulumi.Bool(true)},
		SecretSyncConfig: &container.ClusterSecretSyncConfigArgs{
			Enabled:        pulumi.Bool(true),
			RotationConfig: &container.ClusterSecretSyncConfigRotationConfigArgs{Enabled: pulumi.Bool(true)},
		},
		WorkloadIdentityConfig: &container.ClusterWorkloadIdentityConfigArgs{WorkloadPool: workloadPool},
		DeletionProtection:     pulumi.Bool(s.deletionProtection),
	}, child)
	if err != nil {
		return nil, err
	}
	// GKE signs a cluster's service-account tokens as the issuer gkeIssuer builds, the URL
	// Google's Workload Identity documentation names the cluster by (its kubernetes.cluster
	// principal set). It is built from the project, the location and the name rather than taken from
	// the cluster's selfLink, which the provider documents only as "the server-defined URL for the
	// resource", a form it does not promise.
	issuer := pulumi.All(vpc.project.ProjectId(), cluster.Location, cluster.Name).ApplyT(func(values []interface{}) (string, error) {
		parts, err := outputStrings(dispatch, values, "the project's id", "the cluster's location", "the cluster's name")
		if err != nil {
			return "", err
		}
		return gkeIssuer(parts[0], parts[1], parts[2]), nil
	}).(pulumi.StringOutput)
	return &dispatchCluster{
		name: cluster.Name, issuer: issuer, kubeconfig: dispatchKubeconfig(dispatch, cluster),
		pool: cluster.WorkloadIdentityConfig.WorkloadPool(), waitFor: []pulumi.Resource{cluster},
	}, nil
}

// gkeIssuer is the issuer GKE signs a cluster's service-account tokens as:
// https://container.googleapis.com/v1/projects/<project id>/locations/<location>/clusters/<name>.
// Both cluster halves build it, so the component's own cluster and a looked-up one are named alike.
func gkeIssuer(project, location, name string) string {
	return fmt.Sprintf("https://container.googleapis.com/v1/projects/%s/locations/%s/clusters/%s", project, location, name)
}

// dispatchLookedUpCluster is what the component reads off the cluster existingCluster names, as
// the gcp provider's getCluster reports it before the component registers anything. Every field is
// a plain value, so the kubeconfig built from them is known whenever the component's network's
// name is. name is the input's; location is the cluster's own as getCluster reports it, or the
// input's where it reports none; pool is the cluster's Workload Identity pool; and network is the
// name of the network the cluster runs on, the last element of the path getCluster reports.
type dispatchLookedUpCluster struct {
	name, location          string
	endpoint, caCertificate string
	pool, network           string
}

// advancedDatapath is the datapath provider of GKE Dataplane V2, which enforces NetworkPolicy
// whether or not the cluster's network policy setting is on.
const advancedDatapath = "ADVANCED_DATAPATH"

// lookupDispatchCluster looks existing, the cluster existingCluster names, up with the plain
// getCluster invoke, through gcpProvider, the consumer's gcp provider, and refuses a cluster
// Dispatch cannot run in, each refusal a core.InputError naming existingCluster whose message
// names the cluster and its location: one the lookup fails for; one with no endpoint or no CA
// certificate, which the component's Kubernetes provider reaches and trusts it by; one with no
// Workload Identity pool, where the pods' principal and the NATS seed's grant to it live; one with
// the Secret Manager add-on off, through which every pod mounts the NATS seed; and one that
// enforces no NetworkPolicy (neither Dataplane V2 nor network policy on), since the component's
// NetworkPolicies are its only fence. NewDispatch calls it after the providers checks and before
// it registers anything, so a refused cluster leaves nothing registered. Whether the cluster runs
// on the component's network can be judged only once that network's name resolves, so
// newDispatchExistingCluster judges it.
func lookupDispatchCluster(ctx *pulumi.Context, existing *DispatchClusterArgs, gcpProvider pulumi.ProviderResource) (*dispatchLookedUpCluster, error) {
	cluster, err := container.LookupCluster(ctx, &container.LookupClusterArgs{Name: existing.Name, Location: pulumi.StringRef(existing.Location)}, pulumi.Provider(gcpProvider))
	if err != nil {
		return nil, core.InputErrorf("existingCluster", "existingCluster: look up cluster %s in %s: %w", existing.Name, existing.Location, err)
	}
	refuse := func(why string) error {
		return core.InputErrorf("existingCluster", "existingCluster: cluster %s in %s %s", existing.Name, existing.Location, why)
	}
	switch {
	case cluster.Endpoint == "":
		return nil, refuse("has no endpoint, which the component's Kubernetes provider reaches it at")
	case len(cluster.MasterAuths) == 0 || cluster.MasterAuths[0].ClusterCaCertificate == "":
		return nil, refuse("has no CA certificate, which the component's Kubernetes provider trusts its endpoint by")
	case len(cluster.WorkloadIdentityConfigs) == 0 || cluster.WorkloadIdentityConfigs[0].WorkloadPool == "":
		return nil, refuse("has no Workload Identity pool: the pods' principal, and the NATS seed's grant to it, live in that pool")
	case len(cluster.SecretManagerConfigs) == 0 || !cluster.SecretManagerConfigs[0].Enabled:
		return nil, refuse("has the Secret Manager add-on off: every pod mounts the NATS seed through it")
	case cluster.DatapathProvider != advancedDatapath && (len(cluster.NetworkPolicies) == 0 || !cluster.NetworkPolicies[0].Enabled):
		return nil, refuse(fmt.Sprintf("enforces no NetworkPolicy: its datapathProvider is %q, not %s, and its network policy is off; the component's NetworkPolicies are its only fence",
			cluster.DatapathProvider, advancedDatapath))
	}
	location := existing.Location
	if cluster.Location != nil && *cluster.Location != "" {
		location = *cluster.Location
	}
	return &dispatchLookedUpCluster{
		name: existing.Name, location: location,
		endpoint: cluster.Endpoint, caCertificate: cluster.MasterAuths[0].ClusterCaCertificate,
		pool: cluster.WorkloadIdentityConfigs[0].WorkloadPool, network: cluster.Network[strings.LastIndex(cluster.Network, "/")+1:],
	}, nil
}

// newDispatchExistingCluster is the cluster half for the cluster existingCluster names, which
// NewDispatch looked up (lookupDispatchCluster) and the component does not make: it registers
// nothing, and gives the rest of the component what newDispatchCluster gives for the component's
// own cluster, from the lookup's plain values. The name is the input's. The kubeconfig waits on the
// component's own Network's name alone, and refuses, as a core.InputError naming existingCluster,
// a cluster on another network, since the Cloud SQL proxy reaches the instance's private address
// on the component's network. The issuer is gkeIssuer's, from the project's id and the looked-up
// location and name, and the Workload Identity pool is the cluster's. No child waits on anything
// of this half, since the cluster exists before the component runs, and the issuer is the
// Kubernetes provider's clusterIdentifier, which only this half sets.
func newDispatchExistingCluster(name string, vpc *dispatchNetwork, cluster *dispatchLookedUpCluster) *dispatchCluster {
	dispatch := dispatchType + " " + name
	kubeconfig := vpc.network.Name.ApplyT(func(network string) (string, error) {
		if cluster.network != network {
			return "", fmt.Errorf("%s: %w", dispatch, core.InputErrorf("existingCluster",
				"existingCluster: cluster %s in %s runs on network %s, not on the component's own network %s: the Cloud SQL proxy reaches the instance's private address on the component's network",
				cluster.name, cluster.location, cluster.network, network))
		}
		return renderKubeconfig(cluster.endpoint, cluster.caCertificate, cluster.name), nil
	}).(pulumi.StringOutput)
	issuer := vpc.project.ProjectId().ApplyT(func(id *string) (string, error) {
		parts, err := outputStrings(dispatch, []any{id}, "the project's id")
		if err != nil {
			return "", err
		}
		return gkeIssuer(parts[0], cluster.location, cluster.name), nil
	}).(pulumi.StringOutput)
	return &dispatchCluster{
		name: pulumi.String(cluster.name).ToStringOutput(), issuer: issuer, kubeconfig: kubeconfig,
		pool: pulumi.StringPtr(cluster.pool).ToStringPtrOutput(), clusterIdentifier: issuer.ToStringPtrOutput(),
	}
}

// dispatchKubeconfig renders the exec-plugin kubeconfig of the component's own cluster, which the
// component exports and its Kubernetes provider takes; newDispatchExistingCluster renders the one
// of a cluster existingCluster names from the lookup instead. The cluster half computes it, so a
// cluster-only component, which registers no Kubernetes provider and no Kubernetes object, still
// exports it.
func dispatchKubeconfig(dispatch string, cluster *container.Cluster) pulumi.StringOutput {
	return pulumi.All(cluster.Endpoint, cluster.MasterAuth.ClusterCaCertificate(), cluster.Name).ApplyT(func(values []interface{}) (string, error) {
		parts, err := outputStrings(dispatch, values, "the cluster's endpoint", "the cluster's CA certificate", "the cluster's name")
		if err != nil {
			return "", err
		}
		return renderKubeconfig(parts[0], parts[1], parts[2]), nil
	}).(pulumi.StringOutput)
}

// newDispatchDatabase registers what Dispatch itself runs on, which clusterOnly builds none of:
// the generated passwords, which it keeps whichever Cloud SQL instance the proxy connects to;
// that instance, which it decides once, from existingDatabaseInstance alone: the component's own,
// with its database and user (newDispatchOwnInstance), or else the one existingDatabaseInstance
// names, which the component does not make (dispatchDatabaseConnectionName); and the Secret
// Manager grants the pods' principal needs. It takes the network half's network and peering, which
// only the component's own instance reads, and its project, and the cluster half's Workload
// Identity pool and the resources its grants wait for, and answers with its own.
func newDispatchDatabase(ctx *pulumi.Context, comp *Dispatch, name string, args *DispatchArgs, s dispatchSettings, vpc *dispatchNetwork, cloud *dispatchCluster) (*dispatchDatabase, error) {
	dispatch := dispatchType + " " + name
	child := pulumi.Parent(comp)
	network, connection, project := vpc.network, vpc.connection, vpc.project
	password, err := random.NewRandomPassword(ctx, s.hostname+"-password", &random.RandomPasswordArgs{Length: pulumi.Int(32), Special: pulumi.Bool(false)}, child)
	if err != nil {
		return nil, err
	}
	// The instance the proxy connects to: the one existingDatabaseInstance names, for which the
	// component registers no DatabaseInstance, Database or User, since that instance holds the
	// database dispatch and the login dispatch already, as a promoted replica of the component's
	// own instance does, and which nothing waits for, since it exists before the component runs
	// and the component never restarts it; or else the component's own, DatabaseInstance
	// <hostname>, with its database and its login (newDispatchOwnInstance).
	var (
		connectionName pulumi.StringOutput
		waitFor        []pulumi.Resource
	)
	if s.existingDatabaseInstance {
		connectionName = dispatchDatabaseConnectionName(dispatch, args.ExistingDatabaseInstance)
	} else if connectionName, waitFor, err = newDispatchOwnInstance(ctx, dispatch, args, s, child, network, connection, password); err != nil {
		return nil, err
	}
	// One key per generation the list names, in its order. Generation 0 keeps the name and the
	// arguments the single signing key always had, so the key a running deployment already has
	// is the one it is and no apply of this release replaces it; a generation above 0 is a key
	// of its own, which the operator adds and later drops by changing the list alone.
	signing := make([]*random.RandomPassword, 0, len(s.signingKeyGenerations))
	for _, generation := range s.signingKeyGenerations {
		keyName := s.hostname + "-signing-key"
		if generation > 0 {
			keyName += "-" + strconv.Itoa(generation)
		}
		key, err := random.NewRandomPassword(ctx, keyName, &random.RandomPasswordArgs{Length: pulumi.Int(64), Special: pulumi.Bool(false)}, child)
		if err != nil {
			return nil, err
		}
		signing = append(signing, key)
	}
	agentToken, err := random.NewRandomPassword(ctx, s.hostname+"-agent-token", &random.RandomPasswordArgs{Length: pulumi.Int(48), Special: pulumi.Bool(false)}, child)
	if err != nil {
		return nil, err
	}
	// The listener binds 0.0.0.0 in a pod of its own, where its API refuses to start without a
	// bearer, and its credential is its own, never a key of the Secret holding the database URL
	// and the signing key.
	listenerToken, err := random.NewRandomPassword(ctx, s.hostname+"-listener-token", &random.RandomPasswordArgs{Length: pulumi.Int(48), Special: pulumi.Bool(false)}, child)
	if err != nil {
		return nil, err
	}
	// The principal lives in the cluster's Workload Identity pool, which Google creates with the
	// cluster. It is built from the pool the cluster half reports, the cluster's own
	// WorkloadIdentityConfig, so it resolves only once the pool exists, and the component's own
	// seed grant below, built from it, waits for the cluster half's resources: the component's own
	// Cluster, since IAM may refuse a member of a pool that does not exist yet (unproven: the
	// plan's hardening ledger), and nothing for a cluster existingCluster names, whose pool exists
	// before the component runs. A consumer's grant taken from the `principal` output would
	// resolve later still, once every child has registered, the app Deployment's readiness wait
	// included, and that wait needs roles/cloudsql.client already granted, so the consumer grants
	// that role from the principal's known form, not from the output; whether IAM accepts it
	// before the pool exists is the same unproven question.
	principalOf := func(account string) pulumi.StringOutput {
		return pulumi.All(project.Number(), cloud.pool).ApplyT(func(values []interface{}) (string, error) {
			parts, err := outputStrings(dispatch, values, "the project's number", "the cluster's Workload Identity pool")
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("principal://iam.googleapis.com/projects/%s/locations/global/workloadIdentityPools/%s/subject/ns/%s/sa/%s", parts[0], parts[1], s.namespace, account), nil
		}).(pulumi.StringOutput)
	}
	principal := principalOf(s.hostname)
	// The NATS seed is granted only where the pods mount it: with natsAudience each pod presents
	// its own account's projected token instead (newDispatchNATSCredential), and nothing of the
	// seed is mounted, classed or granted.
	var seedAccessor, listenerSeedAccessor *secretmanager.SecretIamMember
	if s.natsAudience == "" {
		seedAccessor, err = secretmanager.NewSecretIamMember(ctx, s.hostname+"-nats-seed-accessor", &secretmanager.SecretIamMemberArgs{SecretId: refuseDispatchSeed(args.NatsSeedSecret, dispatch, "natsSeedSecret"), Role: pulumi.String("roles/secretmanager.secretAccessor"), Member: principal}, child, pulumi.DependsOn(cloud.waitFor))
		if err != nil {
			return nil, err
		}
		// The listener runs as an account of its own, <hostname>-listener, so that no listener pod
		// can project a token naming the app's account, the one principal the listener takes a
		// send's role mark from (ENVOY_DISPATCH_PRINCIPAL). The NATS seed it mounts is granted to
		// that account too, and nothing else is.
		listenerSeedAccessor, err = secretmanager.NewSecretIamMember(ctx, s.hostname+"-listener-nats-seed-accessor", &secretmanager.SecretIamMemberArgs{SecretId: refuseDispatchSeed(args.NatsSeedSecret, dispatch, "natsSeedSecret"), Role: pulumi.String("roles/secretmanager.secretAccessor"), Member: principalOf(s.hostname + "-listener")}, child, pulumi.DependsOn(cloud.waitFor))
		if err != nil {
			return nil, err
		}
	}
	var azureKeyAccessor *secretmanager.SecretIamMember
	if s.azure.key {
		// The Azure key is read by the app at run time through the CSI driver, so the pods'
		// principal needs the same per-secret accessor grant the NATS seed has.
		azureKeyAccessor, err = secretmanager.NewSecretIamMember(ctx, s.hostname+"-azure-embed-key-accessor", &secretmanager.SecretIamMemberArgs{SecretId: refuseDispatchSeed(args.AzureEmbedKeySecret, dispatch, "azureEmbedKeySecret"), Role: pulumi.String("roles/secretmanager.secretAccessor"), Member: principal}, child, pulumi.DependsOn(cloud.waitFor))
		if err != nil {
			return nil, err
		}
	}
	var chatTokenAccessor *secretmanager.SecretIamMember
	if s.answerer.place == dispatchAnswererEndpoint {
		// The endpoint answerer's bearer token is read by the app on every call through the CSI
		// driver, so the pods' principal needs the same per-secret accessor grant.
		chatTokenAccessor, err = secretmanager.NewSecretIamMember(ctx, s.hostname+"-chat-token-accessor", &secretmanager.SecretIamMemberArgs{SecretId: refuseDispatchSeed(args.ChatTokenSecret, dispatch, "chatTokenSecret"), Role: pulumi.String("roles/secretmanager.secretAccessor"), Member: principal}, child, pulumi.DependsOn(cloud.waitFor))
		if err != nil {
			return nil, err
		}
	}
	return &dispatchDatabase{
		connectionName: connectionName, waitFor: waitFor, password: password, signing: signing,
		agentToken: agentToken, listenerToken: listenerToken,
		principal: principal, seedAccessor: seedAccessor, listenerSeedAccessor: listenerSeedAccessor,
		azureKeyAccessor: azureKeyAccessor, chatTokenAccessor: chatTokenAccessor,
	}, nil
}

// newDispatchOwnInstance registers the component's own Cloud SQL instance, which
// existingDatabaseInstance replaces: DatabaseInstance <hostname> in region, which takes its
// private IP on network through connection, the private services peering, and on it Database and
// User <hostname>, the database dispatch and the login dispatch with password. It answers with the
// instance's connection name and the resources that must come before anything that waits out its
// in-place restart (dispatchDatabase.waitFor): the DatabaseInstance alone.
func newDispatchOwnInstance(ctx *pulumi.Context, dispatch string, args *DispatchArgs, s dispatchSettings, child pulumi.ResourceOption, network *compute.Network, connection *servicenetworking.Connection, password *random.RandomPassword) (pulumi.StringOutput, []pulumi.Resource, error) {
	// databaseWaitsFor names a value of a resource the consumer wants the instance's update to
	// follow, its own step before the restart (deleting the old tailnet device, say). The value
	// itself reaches no resource here: only the dependencies it carries do, which is all
	// dependsOnInput reads off it. A seed is still refused once it resolves, as it is in every
	// other Pulumi input of this component, since refuseDispatchSeed's ApplyT keeps the input's
	// own dependencies.
	instanceOpts := []pulumi.ResourceOption{child, pulumi.DependsOn([]pulumi.Resource{connection})}
	if args.DatabaseWaitsFor != nil {
		instanceOpts = append(instanceOpts, dependsOnInput(refuseDispatchSeed(args.DatabaseWaitsFor, dispatch, "databaseWaitsFor")))
	}
	// AvailabilityType is databaseAvailabilityType. The bridged Terraform provider declares
	// settings.availability_type without ForceNew (terraform-provider-google-beta v8.6.0, commit
	// e6f3d61a347c, the upstream pulumi-gcp v10.1.0 bridges:
	// google-beta/services/sql/resource_sql_database_instance.go:449-462), so switching between
	// ZONAL and REGIONAL updates the instance in place: Cloud SQL restarts it once, and the change
	// cannot be stopped once started.
	instance, err := sql.NewDatabaseInstance(ctx, s.hostname, &sql.DatabaseInstanceArgs{
		DatabaseVersion: pulumi.String(s.databaseVersion), Region: pulumi.String(s.region), DeletionProtection: pulumi.Bool(s.deletionProtection),
		Settings: &sql.DatabaseInstanceSettingsArgs{
			DeletionProtectionEnabled: pulumi.Bool(s.deletionProtection),
			Tier:                      pulumi.String(s.databaseTier), Edition: pulumi.String("ENTERPRISE"), AvailabilityType: pulumi.String(s.databaseAvailabilityType), DiskType: pulumi.String("PD_SSD"), DiskSize: pulumi.Int(10), DiskAutoresize: pulumi.Bool(true),
			IpConfiguration:     &sql.DatabaseInstanceSettingsIpConfigurationArgs{Ipv4Enabled: pulumi.Bool(false), PrivateNetwork: network.ID(), SslMode: pulumi.String("ENCRYPTED_ONLY")},
			BackupConfiguration: &sql.DatabaseInstanceSettingsBackupConfigurationArgs{Enabled: pulumi.Bool(true), PointInTimeRecoveryEnabled: pulumi.Bool(true), StartTime: pulumi.String("08:00"), TransactionLogRetentionDays: pulumi.Int(7), BackupRetentionSettings: &sql.DatabaseInstanceSettingsBackupConfigurationBackupRetentionSettingsArgs{RetainedBackups: pulumi.Int(14)}},
		},
	}, instanceOpts...)
	if err != nil {
		return pulumi.StringOutput{}, nil, err
	}
	// deletionProtection also keeps the database itself: PREVENT refuses its delete, which an
	// instance replacement would otherwise make first.
	databasePolicy := "DELETE"
	if s.deletionProtection {
		databasePolicy = "PREVENT"
	}
	_, err = sql.NewDatabase(ctx, s.hostname, &sql.DatabaseArgs{Instance: instance.Name, Name: pulumi.String("dispatch"), DeletionPolicy: pulumi.String(databasePolicy)}, child)
	if err != nil {
		return pulumi.StringOutput{}, nil, err
	}
	_, err = sql.NewUser(ctx, s.hostname, &sql.UserArgs{Instance: instance.Name, Name: pulumi.String("dispatch"), Password: password.Result}, child)
	if err != nil {
		return pulumi.StringOutput{}, nil, err
	}
	return instance.ConnectionName, []pulumi.Resource{instance}, nil
}

// dispatchDatabaseConnectionName is existingDatabaseInstance, the connection name of a Cloud SQL
// instance the component does not make, once it resolves: a seed is refused first, naming the
// input (refuseDispatchSeed), then a value that is not <project>:<region>:<instance>
// (connectionNameRule), as a core.InputError naming existingDatabaseInstance. Each fails the
// output, wrapped with the component's type and name, so no child is registered with the value
// and the proxy is never handed a name it cannot connect to. Neither refusal quotes the value: it
// arrives from another resource or stack, as a secret or not, so the shape's refusal says what the
// form is instead.
func dispatchDatabaseConnectionName(dispatch string, value pulumi.StringInput) pulumi.StringOutput {
	return refuseDispatchSeed(value, dispatch, "existingDatabaseInstance").ApplyT(func(name string) (string, error) {
		if !connectionNameRule.MatchString(name) {
			return "", fmt.Errorf("%s: %w", dispatch, core.InputErrorf("existingDatabaseInstance",
				"existingDatabaseInstance must be a Cloud SQL instance's connection name, <project>:<region>:<instance>, as its connectionName gives it: a project id, or a domain-scoped one as <domain>:<project>, a region, and the instance's name, each in lowercase (not echoed)"))
		}
		return name, nil
	}).(pulumi.StringOutput)
}
