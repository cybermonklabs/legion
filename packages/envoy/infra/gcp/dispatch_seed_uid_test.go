package gcp_test

import (
	"slices"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/infra/internal/pulumitest"
)

// dispatchSeedGrantsByUID are the NATS seed's grants by uid, each with the ServiceAccount it
// names.
var dispatchSeedGrantsByUID = map[string]string{
	"example-dispatch-nats-seed-uid-accessor":          "example-dispatch",
	"example-dispatch-listener-nats-seed-uid-accessor": "example-dispatch-listener",
}

// withServiceAccountsAnsweringNoUID makes m answer the create of each ServiceAccount accounts
// names, or of every ServiceAccount when it names none, with the registered inputs alone: metadata
// and no uid. That is what pulumi-kubernetes' Create answers in a preview that cannot dry-run the
// object (a namespace the same update creates, a cluster it cannot reach, server-side apply's
// AlreadyExists), and what no API server answers in an update.
func withServiceAccountsAnsweringNoUID(m *pulumitest.Mocks, accounts ...string) *pulumitest.Mocks {
	computed := m.Computed
	m.Computed = func(args pulumi.MockResourceArgs) (string, resource.PropertyMap) {
		if args.TypeToken == "kubernetes:core/v1:ServiceAccount" && (len(accounts) == 0 || slices.Contains(accounts, args.Name)) {
			return "", nil
		}
		return computed(args)
	}
	return m
}

// previewDispatch previews args on m with the three providers a Dispatch program declares.
func previewDispatch(t *testing.T, m *pulumitest.Mocks, args *gcp.DispatchArgs) error {
	t.Helper()
	m.Preview = true
	return runDispatchOnMocks(t, m, "example-dispatch", args, newGCPProvider, newRandomProvider, newTailscaleProvider)
}

// A preview that cannot dry-run the two ServiceAccounts' creates passes, with each grant by uid's
// member unknown, on the three paths where the namespace is created in the same update: Dispatch
// turned on after clusterOnly, the move onto existingCluster without existingNamespace, and a
// change of namespace. A refusal of the missing uid would fail the preview there, and so
// `pulumi up`, which previews first. The control: the same preview, with dry runs that answer a
// uid, gives each grant its member by that uid, so the check can see a known member.
func TestDispatchPreviewWithNoDryRunOfItsAccountsLeavesTheGrantsByUIDUnknown(t *testing.T) {
	renamed := dispatchArgs()
	renamed.Namespace = pulumi.StringRef("example-namespace")
	for _, row := range []struct {
		name string
		args func() *gcp.DispatchArgs
	}{
		{"Dispatch turned on after clusterOnly, its namespace created in the same update", dispatchArgs},
		{"the move onto existingCluster without existingNamespace", existingClusterArgs},
		{"a change of namespace", func() *gcp.DispatchArgs { return renamed }},
	} {
		t.Run(row.name, func(t *testing.T) {
			noDryRun := withServiceAccountsAnsweringNoUID(newMocks())
			require.NoError(t, previewDispatch(t, noDryRun, row.args()), "a preview with no dry run of the accounts fails")
			dryRun := newMocks()
			require.NoError(t, previewDispatch(t, dryRun, row.args()))
			for grant, account := range dispatchSeedGrantsByUID {
				member, ok := noDryRun.Named(t, accessorType, grant).Inputs["member"]
				require.True(t, !ok || member.IsNull() || member.IsComputed(), "%s's member is %v, want unknown while %s has no uid", grant, member, account)
				require.Equal(t, dispatchPoolRoot+"/kubernetes.serviceaccount.uid/"+mockServiceAccountUID(account), normalizedProperty(dryRun.Named(t, accessorType, grant).Inputs["member"]),
					"control: with a dry run that answers a uid, %s names %s by it", grant, account)
			}
		})
	}
}

// Over the plugin's Construct, the path a consumer's preview takes, a preview that cannot dry-run
// the app's ServiceAccount answers serviceAccountUid unknown: neither a refusal nor "", which is
// what clusterOnly answers for "not applied". The control: a preview whose dry run answers a uid
// answers that uid.
func TestConstructPreviewWithNoDryRunLeavesServiceAccountUidUnknown(t *testing.T) {
	noDryRun := withServiceAccountsAnsweringNoUID(newMocks())
	noDryRun.Preview = true
	response, err := constructDispatchOnMocks(t, noDryRun, dispatchConstructBaseInputs())
	require.NoError(t, err)
	uid, ok := response.State.GetOk("serviceAccountUid")
	require.True(t, ok, "serviceAccountUid is in the state")
	require.True(t, uid.IsComputed(), "serviceAccountUid is %v, want unknown", uid)

	dryRun := newMocks()
	dryRun.Preview = true
	response, err = constructDispatchOnMocks(t, dryRun, dispatchConstructBaseInputs())
	require.NoError(t, err)
	uid, ok = response.State.GetOk("serviceAccountUid")
	require.True(t, ok, "serviceAccountUid is in the state")
	require.True(t, uid.IsString(), "control: serviceAccountUid is %v, want the dry run's uid", uid)
	require.Equal(t, mockServiceAccountUID("example-dispatch"), uid.AsString())
}

// In an update, a ServiceAccount the API server gave no uid fails the apply, naming the account,
// since no grant can name it by uid. Only the app's account answers no uid here, so the refusal
// names it and not the listener's.
func TestDispatchRefusesAnAccountWithNoUIDInAnUpdate(t *testing.T) {
	m := withServiceAccountsAnsweringNoUID(newMocks(), "example-dispatch")
	err := runDispatchOnMocks(t, m, "example-dispatch", dispatchArgs(), newGCPProvider, newRandomProvider, newTailscaleProvider)
	require.ErrorContains(t, err, "envoy-infra:gcp:Dispatch example-dispatch: ServiceAccount example-dispatch has no uid, so no grant can name it by uid")
}
