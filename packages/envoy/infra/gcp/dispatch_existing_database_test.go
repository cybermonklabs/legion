package gcp_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/property"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/infra/internal/pulumitest"
)

const (
	// replicaConnectionName is the connection name of a Cloud SQL instance the component does not
	// make, as a consumer's replica's connectionName gives it: the instance example-replica of
	// project example-project in us-central1.
	replicaConnectionName = "example-project:us-central1:example-replica"
	// domainScopedConnectionName is an instance of a domain-scoped project, whose connection name
	// has four parts: the domain, the project, the region and the instance.
	domainScopedConnectionName = "example.com:example-project:us-central1:example-replica"
	// randomPasswordType is the random provider's RandomPassword, every generated secret's type.
	randomPasswordType = "random:index/randomPassword:RandomPassword"
	// databaseInstanceURNPart is what the URN of any DatabaseInstance the component registers
	// holds.
	databaseInstanceURNPart = "$gcp:sql/databaseInstance:DatabaseInstance::"
)

// existingDatabaseInstanceArgs is dispatchArgs with existingDatabaseInstance naming the replica.
func existingDatabaseInstanceArgs() *gcp.DispatchArgs {
	args := dispatchArgs()
	args.ExistingDatabaseInstance = pulumi.String(replicaConnectionName)
	return args
}

// proxyConnectionName is the app pod's cloud-sql-proxy sidecar's last argument, the connection
// name of the instance it connects to.
func proxyConnectionName(t *testing.T, m *pulumitest.Mocks) any {
	t.Helper()
	args := proxyContainer(t, deploymentPodTemplate(t, m, "example-dispatch"))["args"].([]any)
	require.NotEmpty(t, args, "the proxy takes arguments")
	return args[len(args)-1]
}

// dispatchExistingDatabaseInstanceGolden is the whole declaration of a component on an instance it
// does not make, taken from the default mode's registration golden: every line of it less the
// lines of DatabaseInstance, Database and User example-dispatch, the component's own instance, its
// database and its login. Reading it from the golden rather than writing it out again holds the two
// to one declaration: a resource the default renames or reparents moves this expectation with it.
func dispatchExistingDatabaseInstanceGolden(t *testing.T) string {
	t.Helper()
	own := []string{
		"gcp:sql/databaseInstance:DatabaseInstance example-dispatch ",
		"gcp:sql/database:Database example-dispatch ",
		"gcp:sql/user:User example-dispatch ",
	}
	var kept strings.Builder
	dropped := 0
	for _, line := range strings.SplitAfter(readGolden(t, "dispatch-registrations.golden"), "\n") {
		if slices.ContainsFunc(own, func(prefix string) bool { return strings.HasPrefix(line, prefix) }) {
			dropped++
			continue
		}
		kept.WriteString(line)
	}
	require.Equal(t, len(own), dropped, "the golden holds one line each for the component's own instance, database and login")
	return kept.String()
}

// With existingDatabaseInstance the component registers exactly what the default inputs register,
// less its own DatabaseInstance, Database and User: the network half, the cluster half, the
// generated passwords, the grants and every Kubernetes object keep the names and parents the
// default gives them (TestDispatchRegistrationsMatchTheGolden pins those). The comparison is of the
// whole registration set, so any of the three registered while an instance is named, or any
// resource the default does not register, is a line it does not hold.
func TestDispatchExistingDatabaseInstanceRegistersNoInstanceOfItsOwn(t *testing.T) {
	m, err := runDispatch(t, existingDatabaseInstanceArgs(), true)
	require.NoError(t, err)
	require.Equal(t, dispatchExistingDatabaseInstanceGolden(t), dispatchRegistrations(m))
}

// On an instance it does not make the component keeps every RandomPassword it always generated,
// each under its name, with the inputs, the parent, the provider and the dependencies the default
// path gives it: the database password, which DATABASE_URL carries and the login dispatch of a
// promoted replica of the component's own instance holds, the signing key, the agent token and
// the listener's token. A switch to existingDatabaseInstance therefore replaces none of them.
func TestDispatchExistingDatabaseInstanceKeepsEveryGeneratedPassword(t *testing.T) {
	defaults, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)
	m, err := runDispatch(t, existingDatabaseInstanceArgs(), true)
	require.NoError(t, err)
	names := []string{"example-dispatch-password", "example-dispatch-signing-key", "example-dispatch-agent-token", "example-dispatch-listener-token"}
	require.Equal(t, len(names), countRegistrations(m, randomPasswordType), "the four passwords, and no other")
	for _, name := range names {
		want, got := defaults.Named(t, randomPasswordType, name), m.Named(t, randomPasswordType, name)
		require.Equal(t, want.Inputs, got.Inputs, "%s's inputs", name)
		require.Equal(t, want.RPC.GetParent(), got.RPC.GetParent(), "%s's parent", name)
		require.Equal(t, want.RPC.GetProvider(), got.RPC.GetProvider(), "%s's provider", name)
		require.ElementsMatch(t, want.RPC.GetDependencies(), got.RPC.GetDependencies(), "%s's dependencies", name)
	}
}

// The app pod's cloud-sql-proxy sidecar connects to the instance existingDatabaseInstance names,
// and the databaseInstance output is that name. Neither the Tailscale Service nor the front
// door's Deployment waits for a DatabaseInstance then: they waited for the component's own
// instance because an availability-type change restarts it in place, and the component makes no
// instance to restart. In the default path both still wait for the component's own instance
// (TestDispatchTheFrontDoorWaitsForTheDatabaseInstance holds why).
func TestDispatchExistingDatabaseInstanceIsWhatTheProxyConnectsTo(t *testing.T) {
	m, err := runDispatch(t, existingDatabaseInstanceArgs(), true)
	require.NoError(t, err)
	require.Equal(t, replicaConnectionName, proxyConnectionName(t, m))

	defaults, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)
	waitsForAnInstance := func(dependencies []string) bool {
		return slices.ContainsFunc(dependencies, func(urn string) bool { return strings.Contains(urn, databaseInstanceURNPart) })
	}
	for _, frontDoor := range []struct{ typ, name string }{
		{"tailscale:index/service:Service", "example-dispatch"},
		{deploymentType, "example-dispatch-tailnet"},
	} {
		require.False(t, waitsForAnInstance(m.Named(t, frontDoor.typ, frontDoor.name).RPC.GetDependencies()),
			"%s %s waits for a DatabaseInstance beside existingDatabaseInstance", frontDoor.typ, frontDoor.name)
		require.True(t, waitsForAnInstance(defaults.Named(t, frontDoor.typ, frontDoor.name).RPC.GetDependencies()),
			"%s %s waits for the component's own instance in the default path", frontDoor.typ, frontDoor.name)
	}

	inputs := dispatchConstructBaseInputs()
	inputs["existingDatabaseInstance"] = property.New(replicaConnectionName)
	_, response, err := constructDispatch(t, inputs, false)
	require.NoError(t, err)
	output, ok := response.State.GetOk("databaseInstance")
	require.True(t, ok, "databaseInstance is in the state")
	require.True(t, output.IsString(), "databaseInstance is a known string, not %s", output)
	require.Equal(t, replicaConnectionName, output.AsString())
}

// The four inputs that configure the Cloud SQL instance the component makes are refused beside
// existingDatabaseInstance, which replaces that instance, rather than dropped, each a
// core.InputError naming it, before the resolver judges its value: an empty databaseTier, or an
// availability type the resolver would refuse on its own, is refused for being sent at all. With
// several sent, databaseVersion, databaseTier, databaseAvailabilityType and databaseWaitsFor are
// judged in that order. existingDatabaseInstance alone resolves.
func TestDispatchResolverRefusesADatabaseInputBesideAnExistingInstance(t *testing.T) {
	const replaces = " configures the Cloud SQL instance the component makes, which existingDatabaseInstance replaces: unset it, or unset existingDatabaseInstance"
	for _, row := range []struct {
		name, input string
		set         func(*gcp.DispatchArgs)
	}{
		{"databaseVersion", "databaseVersion", func(a *gcp.DispatchArgs) { a.DatabaseVersion = pulumi.StringRef("POSTGRES_18") }},
		{"databaseTier", "databaseTier", func(a *gcp.DispatchArgs) { a.DatabaseTier = pulumi.StringRef("db-custom-4-15360") }},
		{"an empty databaseTier", "databaseTier", func(a *gcp.DispatchArgs) { a.DatabaseTier = pulumi.StringRef("") }},
		{"databaseAvailabilityType", "databaseAvailabilityType", func(a *gcp.DispatchArgs) { a.DatabaseAvailabilityType = pulumi.StringRef("REGIONAL") }},
		{"an availability type the resolver refuses on its own", "databaseAvailabilityType", func(a *gcp.DispatchArgs) {
			a.DatabaseAvailabilityType = pulumi.StringRef("MULTI_REGIONAL")
		}},
		{"databaseWaitsFor", "databaseWaitsFor", func(a *gcp.DispatchArgs) { a.DatabaseWaitsFor = pulumi.String("example-ordering-value") }},
		{"all four, databaseVersion first", "databaseVersion", func(a *gcp.DispatchArgs) {
			a.DatabaseWaitsFor = pulumi.String("example-ordering-value")
			a.DatabaseAvailabilityType = pulumi.StringRef("REGIONAL")
			a.DatabaseTier = pulumi.StringRef("db-custom-4-15360")
			a.DatabaseVersion = pulumi.StringRef("POSTGRES_18")
		}},
		{"the last three, databaseTier first", "databaseTier", func(a *gcp.DispatchArgs) {
			a.DatabaseWaitsFor = pulumi.String("example-ordering-value")
			a.DatabaseAvailabilityType = pulumi.StringRef("REGIONAL")
			a.DatabaseTier = pulumi.StringRef("db-custom-4-15360")
		}},
		{"the last two, databaseAvailabilityType first", "databaseAvailabilityType", func(a *gcp.DispatchArgs) {
			a.DatabaseWaitsFor = pulumi.String("example-ordering-value")
			a.DatabaseAvailabilityType = pulumi.StringRef("REGIONAL")
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			args := existingDatabaseInstanceArgs()
			row.set(args)
			_, err := gcp.ResolveDispatch(args)
			require.EqualError(t, err, row.input+replaces)
			var refused *core.InputError
			require.ErrorAs(t, err, &refused)
			require.Equal(t, row.input, refused.Input)
		})
	}
	_, err := gcp.ResolveDispatch(existingDatabaseInstanceArgs())
	require.NoError(t, err)
}

// existingDatabaseInstance is checked for its shape once it resolves: a value that is not a
// connection name, <project>:<region>:<instance>, fails the run naming the component and
// existingDatabaseInstance, and never quotes the value, which may come from another resource or
// stack. A domain-scoped project's instance, whose connection name has four parts, is taken, and
// its name is what the proxy connects to.
func TestDispatchRefusesAnExistingDatabaseInstanceThatIsNoConnectionName(t *testing.T) {
	for _, row := range []struct{ name, value string }{
		{"two parts", "example-project:example-replica"},
		{"five parts", "example.com:example-project:us-central1:example-replica:example-extra"},
		{"an upper-case project", "Example-Project:us-central1:example-replica"},
		{"an underscore in the instance's name", "example-project:us-central1:example_replica"},
		{"an empty region", "example-project::example-replica"},
		{"an empty string", ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			args := dispatchArgs()
			args.ExistingDatabaseInstance = pulumi.String(row.value)
			_, err := runDispatch(t, args, true)
			require.ErrorContains(t, err, "envoy-infra:gcp:Dispatch example-dispatch: existingDatabaseInstance")
			// Every string holds the empty one, so only a value with something in it can be looked
			// for in the refusal.
			if row.value != "" {
				require.NotContains(t, err.Error(), row.value, "the refusal quotes the value")
			}
		})
	}
	args := dispatchArgs()
	args.ExistingDatabaseInstance = pulumi.String(domainScopedConnectionName)
	m, err := runDispatch(t, args, true)
	require.NoError(t, err)
	require.Equal(t, domainScopedConnectionName, proxyConnectionName(t, m))
}
