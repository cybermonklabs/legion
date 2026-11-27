package gcp_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/infra/internal/pulumitest"
)

const (
	// existingClusterIssuer is the issuer GKE signs the example existing cluster's tokens as: the
	// project newMocks' getProject answers, and the location and the name existingCluster gives.
	existingClusterIssuer = "https://container.googleapis.com/v1/projects/example-project/locations/us-central1/clusters/example-existing"
	// existingClusterPrincipal is the pods' principal in the Workload Identity pool newMocks'
	// getCluster answers, the project's own, which the component's own cluster takes too.
	existingClusterPrincipal = "principal://iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/example-project.svc.id.goog/subject/ns/dispatch/sa/example-dispatch"
	// clusterURNPart is what the URN of any Cluster the component registers holds.
	clusterURNPart = "$" + clusterType + "::"
)

// exampleExistingCluster is existingCluster naming the example existing cluster newMocks answers
// getCluster for.
func exampleExistingCluster() *gcp.DispatchClusterArgs {
	return &gcp.DispatchClusterArgs{Name: "example-existing", Location: "us-central1"}
}

// existingClusterArgs is dispatchArgs with existingCluster naming the example existing cluster.
func existingClusterArgs() *gcp.DispatchArgs {
	args := dispatchArgs()
	args.ExistingCluster = exampleExistingCluster()
	return args
}

// existingClusterConstructInput is exampleExistingCluster as a consumer's SDK sends it to the
// plugin: an object holding the cluster's name and location.
func existingClusterConstructInput() property.Value {
	return property.New(property.NewMap(map[string]property.Value{
		"name":     property.New("example-existing"),
		"location": property.New("us-central1"),
	}))
}

// existingClusterKubeconfig is the kubeconfig the component renders for the example existing
// cluster, from the endpoint and the CA certificate newMocks' getCluster answers.
func existingClusterKubeconfig() string {
	return gcp.DispatchKubeconfig("example-existing-endpoint", "ZXhhbXBsZS1leGlzdGluZy1jYQ==", "example-existing")
}

// existingClusterMocks is newMocks with its getCluster answer passed through answer first, which
// edits the example cluster in place, so a row's cluster differs from it in what the row names, or
// returns an error, which fails the lookup with it as the provider fails one it cannot make.
func existingClusterMocks(answer func(cluster resource.PropertyMap) error) *pulumitest.Mocks {
	m := newMocks()
	call := m.CallResult
	m.CallResult = func(args pulumi.MockCallArgs) (resource.PropertyMap, error) {
		out, err := call(args)
		if err != nil || args.Token != getClusterToken {
			return out, err
		}
		if err := answer(out); err != nil {
			return nil, err
		}
		return out, nil
	}
	return m
}

// runExistingClusterOnMocks runs existingClusterArgs on m with the three providers a Dispatch
// program with a front door declares.
func runExistingClusterOnMocks(t *testing.T, m *pulumitest.Mocks) error {
	t.Helper()
	return runDispatchOnMocks(t, m, "example-dispatch", existingClusterArgs(), newGCPProvider, newRandomProvider, newTailscaleProvider)
}

// With existingCluster the component registers exactly what the default inputs register, less its
// own Cluster: the network half, the database half and every Kubernetes object keep the names and
// parents the default gives them (TestDispatchRegistrationsMatchTheGolden pins those). The
// comparison is of the whole registration set, so a Cluster registered while one is named, or any
// resource the default does not register, is a line it does not hold.
func TestDispatchExistingClusterRegistersNoClusterOfItsOwn(t *testing.T) {
	m, err := runDispatch(t, existingClusterArgs(), true)
	require.NoError(t, err)
	require.Equal(t, dispatchGoldenWithout(t, dispatchResourceName{clusterType, "example-dispatch"}), dispatchRegistrations(m))
}

// The Kubernetes provider of a component on an existing cluster takes the kubeconfig rendered from
// what getCluster reported, and the cluster's issuer as its clusterIdentifier, so pulumi-kubernetes
// replaces it, and every Kubernetes child, only when the cluster changes. The provider on the
// component's own cluster carries no clusterIdentifier at all, so its inputs are what they always
// were. The kubeconfig waits on plain values and the component's Network's name alone, so it is
// known in a preview as well, where an unknown kubeconfig would replace the provider.
func TestDispatchExistingClusterProviderTakesTheLookedUpCluster(t *testing.T) {
	m, err := runDispatch(t, existingClusterArgs(), true)
	require.NoError(t, err)
	provider := m.Named(t, kubernetesProviderType, "example-dispatch-kubernetes")
	kubeconfig := unwrappedProperty(provider.Inputs["kubeconfig"])
	require.True(t, kubeconfig.IsString(), "the kubeconfig is a known string, not %s", kubeconfig.TypeString())
	require.Equal(t, existingClusterKubeconfig(), kubeconfig.StringValue())
	identifier := unwrappedProperty(provider.Inputs["clusterIdentifier"])
	require.True(t, identifier.IsString(), "the clusterIdentifier is a known string, not %s", identifier.TypeString())
	require.Equal(t, existingClusterIssuer, identifier.StringValue())

	defaults, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)
	_, identified := defaults.Named(t, kubernetesProviderType, "example-dispatch-kubernetes").Inputs["clusterIdentifier"]
	require.False(t, identified, "the provider on the component's own cluster carries no clusterIdentifier")

	preview := newMocks()
	preview.Preview = true
	require.NoError(t, runExistingClusterOnMocks(t, preview))
	previewed := preview.Named(t, kubernetesProviderType, "example-dispatch-kubernetes").Inputs["kubeconfig"]
	known := unwrappedProperty(previewed)
	require.True(t, known.IsString(), "the previewed kubeconfig is a known string, not %s", known.TypeString())
	require.Equal(t, existingClusterKubeconfig(), known.StringValue())
}

// On an existing cluster no Kubernetes child waits on a Cluster: none is registered, and the
// cluster exists before the component runs. Dispatch verifies a caller's projected token against
// the looked-up cluster's issuer. The pods' principal, which the NATS seed's grant names, is the
// one the component's own cluster gives, since a cluster in the same project shares its Workload
// Identity pool. TestConstructTakesAnExistingCluster holds the serviceAccountIssuer and principal
// outputs to the same issuer and principal.
func TestDispatchExistingClusterChildrenTakeTheLookedUpCluster(t *testing.T) {
	args := existingClusterArgs()
	args.OidcAudience = dispatchOidcAudience
	m, err := runDispatch(t, args, true)
	require.NoError(t, err)
	var children int
	for _, r := range m.Registrations() {
		if !strings.HasPrefix(r.Type, "kubernetes:") {
			continue
		}
		children++
		for _, dependency := range r.RPC.GetDependencies() {
			require.NotContains(t, dependency, clusterURNPart, "%s %s waits on a Cluster", r.Type, r.Name)
		}
	}
	require.Positive(t, children, "the component registers Kubernetes children")
	require.Equal(t, existingClusterIssuer, appEnv(t, m)["DISPATCH_OIDC_ISSUER"])

	defaults, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)
	member := func(m *pulumitest.Mocks, accessor string) any {
		return normalizedProperty(m.Named(t, accessorType, accessor).Inputs["member"])
	}
	require.Equal(t, existingClusterPrincipal, member(m, "example-dispatch-nats-seed-accessor"))
	require.Equal(t, member(defaults, "example-dispatch-nats-seed-accessor"), member(m, "example-dispatch-nats-seed-accessor"),
		"the seed's grant names the principal the component's own cluster gives")
	// The listener's own account, <hostname>-listener, is granted the seed from the same pool.
	require.Equal(t, existingClusterPrincipal+"-listener", member(m, "example-dispatch-listener-nats-seed-accessor"))
	require.Equal(t, member(defaults, "example-dispatch-listener-nats-seed-accessor"), member(m, "example-dispatch-listener-nats-seed-accessor"),
		"the listener's seed grant names the principal the component's own cluster gives it")
	// The grants by uid take the same pool, and each account's own uid.
	for accessor, account := range dispatchSeedGrantsByUID {
		require.Equal(t, dispatchPoolRoot+"/kubernetes.serviceaccount.uid/"+mockServiceAccountUID(account), member(m, accessor),
			"%s names ServiceAccount %s by its uid, in the pool the looked-up cluster reports", accessor, account)
	}
	// No seed grant waits on a Cluster: none is registered here.
	for _, accessor := range dispatchSeedAccessors {
		for _, dependency := range m.Named(t, accessorType, accessor).RPC.GetDependencies() {
			require.NotContains(t, dependency, clusterURNPart, "%s waits on a Cluster", accessor)
		}
	}
}

// A stack that moves onto an existing cluster keeps its region, where its regional network and its
// own Cloud SQL instance stay, and names a cluster in another location. The other fixtures put the
// cluster in the component's own region, us-central1, so this one does not: region is us-west1 and
// the cluster is in us-central1. The issuer is the cluster's, with the location getCluster
// reports, wherever the component's region is: the Kubernetes provider's clusterIdentifier,
// DISPATCH_OIDC_ISSUER and the serviceAccountIssuer output, in an update and in a preview, all
// name locations/us-central1. An issuer built from the region names a cluster that does not exist,
// so a federated credential that trusts serviceAccountIssuer would refuse the cluster's tokens.
func TestDispatchExistingClusterIssuerTakesTheClustersLocationNotTheRegion(t *testing.T) {
	args := existingClusterArgs()
	args.Region = "us-west1"
	args.OidcAudience = dispatchOidcAudience
	m, err := runDispatch(t, args, true)
	require.NoError(t, err)
	identifier := unwrappedProperty(m.Named(t, kubernetesProviderType, "example-dispatch-kubernetes").Inputs["clusterIdentifier"])
	require.True(t, identifier.IsString(), "the clusterIdentifier is a known string, not %s", identifier.TypeString())
	require.Equal(t, existingClusterIssuer, identifier.StringValue())
	require.Equal(t, existingClusterIssuer, appEnv(t, m)["DISPATCH_OIDC_ISSUER"])

	for _, preview := range []bool{false, true} {
		inputs := dispatchConstructBaseInputs()
		inputs["region"] = property.New("us-west1")
		inputs["existingCluster"] = existingClusterConstructInput()
		constructed := newMocks()
		constructed.Preview = preview
		response, err := constructDispatchOnMocks(t, constructed, inputs)
		require.NoError(t, err, "preview %t", preview)
		issuer, ok := response.State.GetOk("serviceAccountIssuer")
		require.True(t, ok, "serviceAccountIssuer is in the state, preview %t", preview)
		require.True(t, issuer.IsString(), "serviceAccountIssuer is a known string, not %s, preview %t", issuer, preview)
		require.Equal(t, existingClusterIssuer, issuer.AsString(), "preview %t", preview)
	}
}

// existingCluster's name and location are refused by the resolver, before anything registers and
// before the lookup asks for them, when they do not have the shape GKE gives them, each refusal a
// core.InputError naming the field's path. The shapes GKE does give resolve.
func TestDispatchResolverRefusesAnExistingClusterGKECannotName(t *testing.T) {
	const (
		nameRule     = " must be a GKE cluster name: a lowercase letter, then lowercase letters, digits and hyphens, ending in a letter or digit, at most 40 characters"
		locationRule = " must be a region or a zone, such as us-central1 or us-central1-a"
	)
	for _, row := range []struct {
		name, cluster, location, input, want string
	}{
		{"an uppercase letter in the name", "Example-existing", "us-central1", "existingCluster.name", `existingCluster.name "Example-existing"` + nameRule},
		{"a name starting with a digit", "1example", "us-central1", "existingCluster.name", `existingCluster.name "1example"` + nameRule},
		{"a name ending in a hyphen", "example-", "us-central1", "existingCluster.name", `existingCluster.name "example-"` + nameRule},
		{"an underscore in the name", "example_existing", "us-central1", "existingCluster.name", `existingCluster.name "example_existing"` + nameRule},
		{"an empty name", "", "us-central1", "existingCluster.name", `existingCluster.name ""` + nameRule},
		{"a name of 41 characters", "a" + strings.Repeat("b", 40), "us-central1", "existingCluster.name",
			`existingCluster.name "a` + strings.Repeat("b", 40) + `"` + nameRule},
		{"a region with no number", "example-existing", "us-central", "existingCluster.location", `existingCluster.location "us-central"` + locationRule},
		{"an empty location", "example-existing", "", "existingCluster.location", `existingCluster.location ""` + locationRule},
		{"a zone of two letters", "example-existing", "us-central1-ab", "existingCluster.location", `existingCluster.location "us-central1-ab"` + locationRule},
		{"an uppercase location", "example-existing", "US-CENTRAL1", "existingCluster.location", `existingCluster.location "US-CENTRAL1"` + locationRule},
		{"a location's resource path", "example-existing", "projects/example-project/locations/us-central1", "existingCluster.location",
			`existingCluster.location "projects/example-project/locations/us-central1"` + locationRule},
	} {
		t.Run(row.name, func(t *testing.T) {
			args := dispatchArgs()
			args.ExistingCluster = &gcp.DispatchClusterArgs{Name: row.cluster, Location: row.location}
			_, err := gcp.ResolveDispatch(args)
			require.EqualError(t, err, row.want)
			var refused *core.InputError
			require.ErrorAs(t, err, &refused)
			require.Equal(t, row.input, refused.Input)
		})
	}
	for _, cluster := range []gcp.DispatchClusterArgs{
		{Name: "a", Location: "us-central1"},
		{Name: "a" + strings.Repeat("b", 39), Location: "us-central1-a"},
		{Name: "example-existing-2", Location: "europe-west4"},
		{Name: "example-existing", Location: "northamerica-northeast1-c"},
	} {
		args := dispatchArgs()
		args.ExistingCluster = &cluster
		_, err := gcp.ResolveDispatch(args)
		require.NoError(t, err, "%s in %s", cluster.Name, cluster.Location)
	}
}

// The lookup refuses a cluster Dispatch cannot run in, before the component registers anything, so
// nothing beyond the test's providers exists: a lookup that fails; a cluster with no endpoint or no
// CA certificate, which the Kubernetes provider reaches and trusts it by; with no Workload Identity
// pool, where the pods' principal and the NATS seed's grant live; with the Secret Manager add-on
// off, through which every pod mounts the NATS seed; and one that enforces no NetworkPolicy, since
// the component's NetworkPolicies are its only fence. Each refusal names the component and
// existingCluster, and the cluster and its location. A cluster on the legacy datapath with network
// policy on enforces NetworkPolicy, and is taken.
func TestDispatchRefusesAnExistingClusterDispatchCannotRunIn(t *testing.T) {
	none := resource.NewArrayProperty([]resource.PropertyValue{})
	const refused = "envoy-infra:gcp:Dispatch example-dispatch: existingCluster: cluster example-existing in us-central1 "
	for _, row := range []struct {
		name   string
		answer func(cluster resource.PropertyMap) error
		// want are what the refusal holds, its start first; none is a cluster the component takes.
		want []string
	}{
		{"a lookup that fails", func(resource.PropertyMap) error { return errors.New("example lookup failure") },
			[]string{"envoy-infra:gcp:Dispatch example-dispatch: existingCluster: look up cluster example-existing in us-central1: ", "example lookup failure"}},
		{"no endpoint", func(c resource.PropertyMap) error { c["endpoint"] = resource.NewProperty(""); return nil },
			[]string{refused + "has no endpoint, which the component's Kubernetes provider reaches it at"}},
		{"no master auth", func(c resource.PropertyMap) error { c["masterAuths"] = none; return nil },
			[]string{refused + "has no CA certificate, which the component's Kubernetes provider trusts its endpoint by"}},
		{"an empty CA certificate", func(c resource.PropertyMap) error {
			c["masterAuths"] = oneBlock(resource.PropertyMap{"clusterCaCertificate": resource.NewProperty("")})
			return nil
		}, []string{refused + "has no CA certificate, which the component's Kubernetes provider trusts its endpoint by"}},
		{"no Workload Identity config", func(c resource.PropertyMap) error { c["workloadIdentityConfigs"] = none; return nil },
			[]string{refused + "has no Workload Identity pool: the pods' principal, and the NATS seed's grant to it, live in that pool"}},
		{"an empty Workload Identity pool", func(c resource.PropertyMap) error {
			c["workloadIdentityConfigs"] = oneBlock(resource.PropertyMap{"workloadPool": resource.NewProperty("")})
			return nil
		}, []string{refused + "has no Workload Identity pool: the pods' principal, and the NATS seed's grant to it, live in that pool"}},
		{"no Secret Manager config", func(c resource.PropertyMap) error { c["secretManagerConfigs"] = none; return nil },
			[]string{refused + "has the Secret Manager add-on off: every pod mounts the NATS seed through it"}},
		{"the Secret Manager add-on off", func(c resource.PropertyMap) error {
			c["secretManagerConfigs"] = oneBlock(resource.PropertyMap{"enabled": resource.NewProperty(false)})
			return nil
		}, []string{refused + "has the Secret Manager add-on off: every pod mounts the NATS seed through it"}},
		{"the legacy datapath with no network policy", func(c resource.PropertyMap) error {
			c["datapathProvider"] = resource.NewProperty("LEGACY_DATAPATH")
			return nil
		}, []string{refused + `enforces no NetworkPolicy: its datapathProvider is "LEGACY_DATAPATH", not ADVANCED_DATAPATH, and its network policy is off; the component's NetworkPolicies are its only fence`}},
		{"the legacy datapath with network policy off", func(c resource.PropertyMap) error {
			c["datapathProvider"] = resource.NewProperty("LEGACY_DATAPATH")
			c["networkPolicies"] = oneBlock(resource.PropertyMap{"enabled": resource.NewProperty(false), "provider": resource.NewProperty("CALICO")})
			return nil
		}, []string{refused + `enforces no NetworkPolicy: its datapathProvider is "LEGACY_DATAPATH", not ADVANCED_DATAPATH, and its network policy is off; the component's NetworkPolicies are its only fence`}},
		{"the legacy datapath with network policy on", func(c resource.PropertyMap) error {
			c["datapathProvider"] = resource.NewProperty("LEGACY_DATAPATH")
			c["networkPolicies"] = oneBlock(resource.PropertyMap{"enabled": resource.NewProperty(true), "provider": resource.NewProperty("CALICO")})
			return nil
		}, nil},
	} {
		t.Run(row.name, func(t *testing.T) {
			m := existingClusterMocks(row.answer)
			err := runExistingClusterOnMocks(t, m)
			if row.want == nil {
				require.NoError(t, err)
				kubeconfig := unwrappedProperty(m.Named(t, kubernetesProviderType, "example-dispatch-kubernetes").Inputs["kubeconfig"])
				require.True(t, kubeconfig.IsString(), "the kubeconfig is a known string, not %s", kubeconfig.TypeString())
				require.Equal(t, existingClusterKubeconfig(), kubeconfig.StringValue())
				return
			}
			for _, want := range row.want {
				require.ErrorContains(t, err, want)
			}
			require.ElementsMatch(t, []string{gcpProviderType, randomProviderType, tailscaleProviderType}, m.Types(),
				"the program's own three providers registered and nothing of the component's")
		})
	}
}

// Whether the cluster runs on the component's own network can be judged only once the Network's
// name resolves, so a cluster on another network fails the run there, naming existingCluster: the
// Cloud SQL proxy reaches the instance's private address on the component's network.
func TestDispatchRefusesAnExistingClusterOnAnotherNetwork(t *testing.T) {
	m := existingClusterMocks(func(c resource.PropertyMap) error {
		c["network"] = resource.NewProperty("projects/example-project/global/networks/example-other-network")
		return nil
	})
	err := runExistingClusterOnMocks(t, m)
	require.ErrorContains(t, err, "envoy-infra:gcp:Dispatch example-dispatch: existingCluster: cluster example-existing in us-central1 "+
		"runs on network example-other-network, not on the component's own network example-dispatch-network: "+
		"the Cloud SQL proxy reaches the instance's private address on the component's network")
}
