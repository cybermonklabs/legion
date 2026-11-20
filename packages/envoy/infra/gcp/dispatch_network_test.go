package gcp_test

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/plugin"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	pulumirpc "github.com/pulumi/pulumi/sdk/v3/proto/go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/infra/internal/pulumitest"
)

// The operations deriveDispatchPlan gives a resource, named after the engine's steps.
const (
	stepSame    = "same"
	stepChanged = "changed"
	stepCreate  = "create"
	stepDelete  = "delete"
)

const (
	// dispatchURN is the component's URN, the parent of every resource a transition table names.
	dispatchURN = "urn:pulumi:s::p::" + dispatchType + "::example-dispatch"
	// The types of the resources the transition tables and the tests of the three inputs name,
	// beside networkType and randomPasswordType.
	subnetworkType    = "gcp:compute/subnetwork:Subnetwork"
	routerType        = "gcp:compute/router:Router"
	routerNatType     = "gcp:compute/routerNat:RouterNat"
	globalAddressType = "gcp:compute/globalAddress:GlobalAddress"
	connectionType    = "gcp:servicenetworking/connection:Connection"
	clusterType       = "gcp:container/cluster:Cluster"
	sqlInstanceType   = "gcp:sql/databaseInstance:DatabaseInstance"
	sqlDatabaseType   = "gcp:sql/database:Database"
	sqlUserType       = "gcp:sql/user:User"
)

// dispatchPlanKey is a resource as the engine matches it from one run of a program to the next:
// its type, its name and its parent's URN, which together make its URN.
type dispatchPlanKey struct{ typ, name, parent string }

// dispatchPlanStep is the operation derived for one resource, one of stepSame, stepChanged,
// stepCreate and stepDelete, and for stepChanged the fields of its registration that differ.
type dispatchPlanStep struct {
	op     string
	differ []string
}

// dispatchPlan is the operation derived for every resource either of two runs registers.
type dispatchPlan map[dispatchPlanKey]dispatchPlanStep

// lines renders the plan one line per resource, in the order of its type, name and parent: the
// operation, the resource, and for a changed one the fields that differ.
func (plan dispatchPlan) lines() []string {
	keys := slices.SortedFunc(maps.Keys(plan), func(a, b dispatchPlanKey) int {
		return cmp.Or(strings.Compare(a.typ, b.typ), strings.Compare(a.name, b.name), strings.Compare(a.parent, b.parent))
	})
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		step := plan[key]
		line := fmt.Sprintf("%-7s %s %s parent=%s", step.op, key.typ, key.name, key.parent)
		if len(step.differ) > 0 {
			line += " differs in: " + strings.Join(step.differ, ", ")
		}
		lines = append(lines, line)
	}
	return lines
}

// deriveDispatchPlan runs before and then after, each on fresh newMocks with the providers a
// program in that mode declares (runDispatchForPlan), and derives the operation the engine would
// plan for every resource when a stack's program moves from before's arguments to after's, keyed
// by type, name and parent (dispatchPlanBetween): delete for a resource only the first run
// registers, create for one only the second registers, same for one both register equally, and
// changed, an update or a replace, for one both register differently.
//
// An equal registration is a same step because an equal URN, provider and inputs give the
// provider's Diff nothing to diff, and equal options give the engine nothing else to act on. That
// is the engine's rule; no engine runs here, so a derived same is an inference about the engine,
// not its output. The mocks give every resource the id <name>-id in both runs, so a provider whose
// own inputs change, which the engine may replace, keeps the same reference in its children's
// provider field: the derivation does not show a provider's replacement reaching its children,
// and a Kubernetes child's derived operation says nothing of whether its provider is replaced.
// Only resources on the consumer's own providers, which no input of the component replaces, are
// asserted: the network half's, which run on the consumer's own gcp provider, and the others a
// test's table names.
func deriveDispatchPlan(t *testing.T, before, after *gcp.DispatchArgs) dispatchPlan {
	t.Helper()
	return dispatchPlanBetween(t, runDispatchForPlan(t, before), runDispatchForPlan(t, after))
}

// runDispatchForPlan runs args on fresh newMocks with the providers a program in its mode
// declares: the gcp provider alone for clusterOnly, as runDispatchClusterOnly does, and otherwise
// the three runDispatch declares. The run must succeed.
func runDispatchForPlan(t *testing.T, args *gcp.DispatchArgs) *pulumitest.Mocks {
	t.Helper()
	var (
		m   *pulumitest.Mocks
		err error
	)
	if args.ClusterOnly != nil && *args.ClusterOnly {
		m, err = runDispatchClusterOnly(t, args)
	} else {
		m, err = runDispatch(t, args, true)
	}
	require.NoError(t, err)
	return m
}

// dispatchPlanBetween is deriveDispatchPlan's derivation on two runs already made, before and
// after. Every registration of either run is keyed, the program's own providers included. Two
// registrations are equal when their inputs are, decoded from each register request with unknowns
// and secrets kept and compared with DeepEquals, so a secret compares as a secret and by its
// element, and an unknown as unknown; when their dependencies are, compared as a set, and their
// property dependencies, key by key with each list as a set; and when every other field of the
// request is, which compares provider, protect, retainOnDelete, deleteBeforeReplace,
// ignoreChanges, replaceOnChanges, aliases and aliasSpecs, customTimeouts, deletedWith,
// additionalSecretOutputs, version and pluginDownloadURL among them. Only the fields
// dispatchPlanFieldsApart names are not compared there. A read, which none of the fixtures here
// makes, fails the test: it is no registration this derivation plans.
func dispatchPlanBetween(t *testing.T, before, after *pulumitest.Mocks) dispatchPlan {
	t.Helper()
	first, second := dispatchPlanRequests(t, before), dispatchPlanRequests(t, after)
	plan := dispatchPlan{}
	for key, request := range first {
		next, ok := second[key]
		if !ok {
			plan[key] = dispatchPlanStep{op: stepDelete}
			continue
		}
		if differ := dispatchRegistrationDifferences(t, request, next); len(differ) > 0 {
			plan[key] = dispatchPlanStep{op: stepChanged, differ: differ}
		} else {
			plan[key] = dispatchPlanStep{op: stepSame}
		}
	}
	for key := range second {
		if _, ok := first[key]; !ok {
			plan[key] = dispatchPlanStep{op: stepCreate}
		}
	}
	return plan
}

// dispatchPlanRequests is every register request of a run by its key, failing the test on a read
// and on two registrations under one key, which would be one URN registered twice.
func dispatchPlanRequests(t *testing.T, m *pulumitest.Mocks) map[dispatchPlanKey]*pulumirpc.RegisterResourceRequest {
	t.Helper()
	requests := map[dispatchPlanKey]*pulumirpc.RegisterResourceRequest{}
	for _, r := range m.Registrations() {
		require.NotNil(t, r.RPC, "%s %s is a read, which the derivation does not plan", r.Type, r.Name)
		key := dispatchPlanKey{r.Type, r.Name, r.RPC.GetParent()}
		_, twice := requests[key]
		require.False(t, twice, "%s %s is registered twice under %s", r.Type, r.Name, key.parent)
		requests[key] = r.RPC
	}
	return requests
}

// dispatchPlanFieldsApart are the register request's fields dispatchPlanBetween does not compare
// as they are: object, the inputs, and dependencies and propertyDependencies, which it compares
// itself; and sourcePosition, stackTrace and parentStackTraceHandle, which locate the
// registration in the program's source and stack and are no input or option of the resource.
var dispatchPlanFieldsApart = []string{"object", "dependencies", "propertyDependencies", "sourcePosition", "stackTrace", "parentStackTraceHandle"}

// dispatchRegistrationDifferences names every field in which two register requests of one
// resource differ: inputs.<name> for each input, dependencies, propertyDependencies.<name> for
// each input's dependencies, and every other field by its name; nil when they are equal as
// dispatchPlanBetween says.
func dispatchRegistrationDifferences(t *testing.T, before, after *pulumirpc.RegisterResourceRequest) []string {
	t.Helper()
	var differ []string
	beforeInputs, afterInputs := dispatchRegistrationInputs(t, before), dispatchRegistrationInputs(t, after)
	for _, key := range sortedUnionKeys(beforeInputs, afterInputs) {
		b, inBefore := beforeInputs[key]
		a, inAfter := afterInputs[key]
		if inBefore != inAfter || !b.DeepEquals(a) {
			differ = append(differ, "inputs."+string(key))
		}
	}
	if !slices.Equal(urnSet(before.GetDependencies()), urnSet(after.GetDependencies())) {
		differ = append(differ, "dependencies")
	}
	beforeDependencies, afterDependencies := before.GetPropertyDependencies(), after.GetPropertyDependencies()
	for _, key := range sortedUnionKeys(beforeDependencies, afterDependencies) {
		b, inBefore := beforeDependencies[key]
		a, inAfter := afterDependencies[key]
		if inBefore != inAfter || !slices.Equal(urnSet(b.GetUrns()), urnSet(a.GetUrns())) {
			differ = append(differ, "propertyDependencies."+key)
		}
	}
	b, a := before.ProtoReflect(), after.ProtoReflect()
	fields := b.Descriptor().Fields()
	for i := range fields.Len() {
		field := fields.Get(i)
		if slices.Contains(dispatchPlanFieldsApart, field.JSONName()) {
			continue
		}
		if b.Has(field) != a.Has(field) || !b.Get(field).Equal(a.Get(field)) {
			differ = append(differ, field.JSONName())
		}
	}
	return differ
}

// dispatchRegistrationInputs decodes a register request's inputs as the engine receives them,
// unknowns, secrets and resource references kept: the mocks' own decoded copy drops an unknown.
func dispatchRegistrationInputs(t *testing.T, request *pulumirpc.RegisterResourceRequest) resource.PropertyMap {
	t.Helper()
	inputs, err := plugin.UnmarshalProperties(request.GetObject(), plugin.MarshalOptions{
		KeepUnknowns: true, KeepSecrets: true, KeepResources: true, KeepOutputValues: true,
	})
	require.NoError(t, err)
	return inputs
}

// sortedUnionKeys is every key of a or b, once each, sorted.
func sortedUnionKeys[M ~map[K]V, K cmp.Ordered, V any](a, b M) []K {
	keys := slices.Collect(maps.Keys(a))
	for key := range b {
		if _, ok := a[key]; !ok {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return keys
}

// urnSet is urns sorted, each once.
func urnSet(urns []string) []string {
	return slices.Compact(slices.Sorted(slices.Values(urns)))
}

// requireDispatchPlan logs plan, one line per resource (dispatchPlan.lines), and fails unless
// every resource of want, a child of the component, is planned the operation want gives it,
// printing the plan with the failure. A resource neither run registers has no operation, which no
// row gives, so a resource renamed or reparented in both runs fails its row too.
func requireDispatchPlan(t *testing.T, plan dispatchPlan, want map[dispatchResourceName]string) {
	t.Helper()
	lines := plan.lines()
	for _, line := range lines {
		t.Log(line)
	}
	got := make(map[dispatchResourceName]string, len(want))
	for r := range want {
		got[r] = plan[dispatchPlanKey{r.typ, r.name, dispatchURN}].op
	}
	require.Equal(t, want, got, "the derived plan, by type, name and parent:\n%s", strings.Join(lines, "\n"))
}

// otherExistingClusterArgs is dispatchArgs with existingCluster naming a second cluster, by
// another name and in another location than exampleExistingCluster, which newMocks answers
// getCluster for as it answers for any: on the component's own network, example-dispatch-network.
func otherExistingClusterArgs() *gcp.DispatchArgs {
	args := dispatchArgs()
	args.ExistingCluster = &gcp.DispatchClusterArgs{Name: "example-existing-b", Location: "us-central1-b"}
	return args
}

// A stack that ran Dispatch on the component's own cluster names existingCluster: the component
// stops registering its Cluster, which the engine then deletes unless the consumer keeps it in
// state first, while the network the existing cluster runs on stays exactly as it was. The table
// holds the network half, Network, Subnetwork, Router, RouterNat, GlobalAddress and Connection,
// same, and the component's own Cluster delete.
func TestDispatchNetworkPlanOwnClusterToExistingCluster(t *testing.T) {
	requireDispatchPlan(t, deriveDispatchPlan(t, dispatchArgs(), existingClusterArgs()), map[dispatchResourceName]string{
		{networkType, "example-dispatch-network"}:        stepSame,
		{subnetworkType, "example-dispatch"}:             stepSame,
		{routerType, "example-dispatch"}:                 stepSame,
		{routerNatType, "example-dispatch"}:              stepSame,
		{globalAddressType, "example-dispatch-services"}: stepSame,
		{connectionType, "example-dispatch-services"}:    stepSame,
		{clusterType, "example-dispatch"}:                stepDelete,
	})
}

// A stack on a cluster existingCluster names moves to another one, by another name and in another
// location, on the same network: the Kubernetes provider and its children follow the new
// kubeconfig, and nothing of the network half moves. The table holds the network half same.
func TestDispatchNetworkPlanExistingClusterToAnother(t *testing.T) {
	requireDispatchPlan(t, deriveDispatchPlan(t, existingClusterArgs(), otherExistingClusterArgs()), map[dispatchResourceName]string{
		{networkType, "example-dispatch-network"}:        stepSame,
		{subnetworkType, "example-dispatch"}:             stepSame,
		{routerType, "example-dispatch"}:                 stepSame,
		{routerNatType, "example-dispatch"}:              stepSame,
		{globalAddressType, "example-dispatch-services"}: stepSame,
		{connectionType, "example-dispatch-services"}:    stepSame,
	})
}

// A stack that ran Dispatch on the component's own Cloud SQL instance names
// existingDatabaseInstance, a promoted replica of that instance, say: the component stops
// registering its DatabaseInstance, Database and User, which the engine then deletes unless the
// consumer keeps them in state first, and keeps every generated password, so DATABASE_URL keeps
// the password login dispatch holds and DISPATCH_SERVE_DATABASE_URL the serving login's. The table
// holds the network half and the five RandomPasswords same, and the component's own instance, its
// database and its login delete.
func TestDispatchNetworkPlanOwnInstanceToExistingInstance(t *testing.T) {
	requireDispatchPlan(t, deriveDispatchPlan(t, dispatchArgs(), existingDatabaseInstanceArgs()), map[dispatchResourceName]string{
		{networkType, "example-dispatch-network"}:               stepSame,
		{subnetworkType, "example-dispatch"}:                    stepSame,
		{routerType, "example-dispatch"}:                        stepSame,
		{routerNatType, "example-dispatch"}:                     stepSame,
		{globalAddressType, "example-dispatch-services"}:        stepSame,
		{connectionType, "example-dispatch-services"}:           stepSame,
		{randomPasswordType, "example-dispatch-password"}:       stepSame,
		{randomPasswordType, "example-dispatch-serve-password"}: stepSame,
		{randomPasswordType, "example-dispatch-signing-key"}:    stepSame,
		{randomPasswordType, "example-dispatch-agent-token"}:    stepSame,
		{randomPasswordType, "example-dispatch-listener-token"}: stepSame,
		{sqlInstanceType, "example-dispatch"}:                   stepDelete,
		{sqlDatabaseType, "example-dispatch"}:                   stepDelete,
		{sqlUserType, "example-dispatch"}:                       stepDelete,
	})
}

// A stack that ran Dispatch on the component's own cluster and instance names existingCluster and
// existingDatabaseInstance in one apply. The table holds the network half same, and the
// component's own Cluster, DatabaseInstance, Database and User delete.
func TestDispatchNetworkPlanOwnClusterAndInstanceToExistingOnes(t *testing.T) {
	args := existingClusterArgs()
	args.ExistingDatabaseInstance = pulumi.String(replicaConnectionName)
	requireDispatchPlan(t, deriveDispatchPlan(t, dispatchArgs(), args), map[dispatchResourceName]string{
		{networkType, "example-dispatch-network"}:        stepSame,
		{subnetworkType, "example-dispatch"}:             stepSame,
		{routerType, "example-dispatch"}:                 stepSame,
		{routerNatType, "example-dispatch"}:              stepSame,
		{globalAddressType, "example-dispatch-services"}: stepSame,
		{connectionType, "example-dispatch-services"}:    stepSame,
		{clusterType, "example-dispatch"}:                stepDelete,
		{sqlInstanceType, "example-dispatch"}:            stepDelete,
		{sqlDatabaseType, "example-dispatch"}:            stepDelete,
		{sqlUserType, "example-dispatch"}:                stepDelete,
	})
}

// A stack on a cluster existingCluster names sets regionalNetwork false once nothing uses the
// subnetwork: the Subnetwork, Router and RouterNat go, and the Network, the GlobalAddress and the
// Connection, through which the Cloud SQL instance keeps its private address, stay. The table
// holds those three same and the regional three delete.
func TestDispatchNetworkPlanRegionalNetworkFalse(t *testing.T) {
	requireDispatchPlan(t, deriveDispatchPlan(t, existingClusterArgs(), existingClusterRegionalNetworkArgs(false)), map[dispatchResourceName]string{
		{networkType, "example-dispatch-network"}:        stepSame,
		{globalAddressType, "example-dispatch-services"}: stepSame,
		{connectionType, "example-dispatch-services"}:    stepSame,
		{subnetworkType, "example-dispatch"}:             stepDelete,
		{routerType, "example-dispatch"}:                 stepDelete,
		{routerNatType, "example-dispatch"}:              stepDelete,
	})
}

// The default declaration against the clusterOnly one: the two modes share the network and the
// cluster, so a stack moving between them keeps both. A stack already running Dispatch cannot turn
// clusterOnly on (it sets inputs the mode refuses); the real move is the other way, a cluster made
// with clusterOnly first and Dispatch turned on later, whose plan is this one read backwards. The
// table holds the five resources clusterOnly registers, Network, Subnetwork, Router, RouterNat and
// Cluster, same, and the GlobalAddress and the Connection, which only Dispatch registers, delete.
func TestDispatchNetworkPlanDefaultToClusterOnly(t *testing.T) {
	requireDispatchPlan(t, deriveDispatchPlan(t, dispatchArgs(), dispatchClusterOnlyArgs()), map[dispatchResourceName]string{
		{networkType, "example-dispatch-network"}:        stepSame,
		{subnetworkType, "example-dispatch"}:             stepSame,
		{routerType, "example-dispatch"}:                 stepSame,
		{routerNatType, "example-dispatch"}:              stepSame,
		{clusterType, "example-dispatch"}:                stepSame,
		{globalAddressType, "example-dispatch-services"}: stepDelete,
		{connectionType, "example-dispatch-services"}:    stepDelete,
	})
}
