package gcp

import (
	"context"
	"fmt"
	"strconv"

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
)

// dispatchNetwork is what the network half gives the rest of the component: the VPC and its
// regional subnetwork, the private services peering, and the project. The network half registers
// apart from the cluster half, so the network, its peering and the project lookup hang on no
// cluster: the cluster half takes the network, never the other way round.
type dispatchNetwork struct {
	// network is the VPC the cluster's nodes and the database's private IP are on, and subnetwork
	// the range in region the component's own cluster takes its nodes' and pods' addresses from.
	network    *compute.Network
	subnetwork *compute.Subnetwork
	// connection is the private services peering the Cloud SQL instance takes its private IP
	// through. clusterOnly builds no database, so it registers no peering and connection is nil.
	connection *servicenetworking.Connection
	// project is the project whose number and id name the principal, the Workload Identity pool
	// and the issuer.
	project organizations.LookupProjectResultOutput
}

// dispatchCluster is what the cluster half gives the rest of the component: the cluster's name,
// service-account token issuer, kubeconfig and Workload Identity pool, each as an output, and the
// resources a child that reaches the cluster waits for. Both modes build the cluster half, so every
// field here is set in either mode.
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
	// component's own Cluster, so none of them is made before the cluster exists.
	waitFor []pulumi.Resource
}

// dispatchDatabase is what the component's database resources give the rest of it: the Cloud SQL
// instance, the generated secrets, and the pods' principal with the grants it holds. clusterOnly
// registers none of them, so that mode builds no dispatchDatabase at all.
type dispatchDatabase struct {
	instance *sql.DatabaseInstance
	password *random.RandomPassword
	// signing is the signing-key set, one generated key per generation of
	// signingKeyGenerations and in that order: the first signs every new cookie, every one
	// verifies.
	signing       []*random.RandomPassword
	agentToken    *random.RandomPassword
	listenerToken *random.RandomPassword
	principal     pulumi.StringOutput
	seedAccessor  *secretmanager.SecretIamMember
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
// from any cluster. That is the VPC, Network <hostname>-network; in region, its subnetwork and
// the router and Cloud NAT that private nodes reach the internet through, Subnetwork, Router and
// RouterNat <hostname>; and, outside clusterOnly, which builds no database, the private services
// range and its peering, GlobalAddress and Connection <hostname>-services, through which the Cloud
// SQL instance takes its private IP. It also looks the project up through the gcp provider. The
// cluster half takes the network and the subnetwork from it, and the database half the network,
// the connection and the project.
func newDispatchNetwork(ctx *pulumi.Context, comp *Dispatch, s dispatchSettings, opts ...pulumi.ResourceOption) (*dispatchNetwork, error) {
	gcpProvider, err := providerFor(ctx.Context(), "gcp", opts...)
	if err != nil {
		return nil, err
	}
	child := pulumi.Parent(comp)
	project := organizations.LookupProjectOutput(ctx, organizations.LookupProjectOutputArgs{}, pulumi.Provider(gcpProvider))
	network, err := compute.NewNetwork(ctx, s.hostname+"-network", &compute.NetworkArgs{AutoCreateSubnetworks: pulumi.Bool(false)}, child)
	if err != nil {
		return nil, err
	}
	// The regional part of the network, which only a cluster in region uses: the subnetwork the
	// component's own cluster takes its addresses from, and the router and Cloud NAT its private
	// nodes reach the internet through.
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
	vpc := &dispatchNetwork{network: network, subnetwork: subnetwork, project: project}
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
	// GKE signs a cluster's service-account tokens as the issuer
	// https://container.googleapis.com/v1/projects/<project id>/locations/<location>/clusters/<name>,
	// the URL Google's Workload Identity documentation names the cluster by (its kubernetes.cluster
	// principal set). It is built from the project, the location and the name rather than taken from
	// the cluster's selfLink, which the provider documents only as "the server-defined URL for the
	// resource", a form it does not promise.
	issuer := pulumi.All(vpc.project.ProjectId(), cluster.Location, cluster.Name).ApplyT(func(values []interface{}) (string, error) {
		parts, err := outputStrings(dispatch, values, "the project's id", "the cluster's location", "the cluster's name")
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("https://container.googleapis.com/v1/projects/%s/locations/%s/clusters/%s", parts[0], parts[1], parts[2]), nil
	}).(pulumi.StringOutput)
	return &dispatchCluster{
		name: cluster.Name, issuer: issuer, kubeconfig: dispatchKubeconfig(dispatch, cluster),
		pool: cluster.WorkloadIdentityConfig.WorkloadPool(), waitFor: []pulumi.Resource{cluster},
	}, nil
}

// dispatchKubeconfig renders the cluster's exec-plugin kubeconfig, which the component exports and
// its Kubernetes provider takes. The cluster half computes it, so a cluster-only component, which
// registers no Kubernetes provider and no Kubernetes object, still exports it.
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
// the Cloud SQL instance with its database and user, the generated passwords, and the Secret
// Manager grants the pods' principal needs. It takes the network half's network, peering and
// project, and the cluster half's Workload Identity pool and the resources its grants wait for,
// and answers with its own.
func newDispatchDatabase(ctx *pulumi.Context, comp *Dispatch, name string, args *DispatchArgs, s dispatchSettings, vpc *dispatchNetwork, cloud *dispatchCluster) (*dispatchDatabase, error) {
	dispatch := dispatchType + " " + name
	child := pulumi.Parent(comp)
	network, connection, project := vpc.network, vpc.connection, vpc.project
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
		return nil, err
	}
	password, err := random.NewRandomPassword(ctx, s.hostname+"-password", &random.RandomPasswordArgs{Length: pulumi.Int(32), Special: pulumi.Bool(false)}, child)
	if err != nil {
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
	// deletionProtection also keeps the database itself: PREVENT refuses its delete, which an
	// instance replacement would otherwise make first.
	databasePolicy := "DELETE"
	if s.deletionProtection {
		databasePolicy = "PREVENT"
	}
	_, err = sql.NewDatabase(ctx, s.hostname, &sql.DatabaseArgs{Instance: instance.Name, Name: pulumi.String("dispatch"), DeletionPolicy: pulumi.String(databasePolicy)}, child)
	if err != nil {
		return nil, err
	}
	_, err = sql.NewUser(ctx, s.hostname, &sql.UserArgs{Instance: instance.Name, Name: pulumi.String("dispatch"), Password: password.Result}, child)
	if err != nil {
		return nil, err
	}
	// The principal lives in the cluster's Workload Identity pool, which Google creates with the
	// cluster. It is built from the pool the cluster half reports, the cluster's own
	// WorkloadIdentityConfig, so it resolves only once the pool exists, and the component's own
	// seed grant below, built from it, waits for the cluster half's resources, the cluster: IAM may
	// refuse a member of a pool that does not exist yet (unproven: the plan's hardening ledger). A
	// consumer's grant taken from the `principal` output would resolve later still, once every
	// child has registered, the app Deployment's readiness wait included, and that wait needs
	// roles/cloudsql.client already granted, so the consumer grants that role from the principal's
	// known form, not from the output; whether IAM accepts it before the pool exists is the same
	// unproven question.
	principal := pulumi.All(project.Number(), cloud.pool).ApplyT(func(values []interface{}) (string, error) {
		parts, err := outputStrings(dispatch, values, "the project's number", "the cluster's Workload Identity pool")
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("principal://iam.googleapis.com/projects/%s/locations/global/workloadIdentityPools/%s/subject/ns/%s/sa/%s", parts[0], parts[1], s.namespace, s.hostname), nil
	}).(pulumi.StringOutput)
	seedAccessor, err := secretmanager.NewSecretIamMember(ctx, s.hostname+"-nats-seed-accessor", &secretmanager.SecretIamMemberArgs{SecretId: refuseDispatchSeed(args.NatsSeedSecret, dispatch, "natsSeedSecret"), Role: pulumi.String("roles/secretmanager.secretAccessor"), Member: principal}, child, pulumi.DependsOn(cloud.waitFor))
	if err != nil {
		return nil, err
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
		instance: instance, password: password, signing: signing,
		agentToken: agentToken, listenerToken: listenerToken,
		principal: principal, seedAccessor: seedAccessor,
		azureKeyAccessor: azureKeyAccessor, chatTokenAccessor: chatTokenAccessor,
	}, nil
}
