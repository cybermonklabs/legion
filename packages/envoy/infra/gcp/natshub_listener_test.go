package gcp_test

import (
	"strings"
	"testing"

	"github.com/nats-io/nkeys"
	gcpsdk "github.com/pulumi/pulumi-gcp/sdk/v10/go/gcp"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/infra/internal/pulumitest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The three values of a hub listener's broker check as a consumer once gave them: the secrets
// broker, the audience of the VM's own Google identity token, and the origin a client reached the
// listener at.
const (
	brokerURL      = "https://example-broker.internal.example"
	brokerAudience = "example-broker-audience"
	listenerOrigin = "http://example-hub.example-tailnet.ts.net:9020"
)

// hubListenerRefusal is what gcp:NatsHub answers every listener with (t-e-19): the forward that
// reached the listener's loopback bind, and why only a bearer kind, which the hub renders none of,
// would close /v1 there.
const hubListenerRefusal = "listener binds 127.0.0.1:9020, which TCPForward 127.0.0.1:9020 forwards to"

// A hub without a listener registers no grant on a seed secret, and its group waits for the key's
// grant alone.
func TestNatsHubGrantsNoSeedSecret(t *testing.T) {
	m, err := run(t, exampleArgs(), nil, true)
	require.NoError(t, err)

	var accessors []string
	for _, r := range m.Registrations() {
		if r.Type == accessorType {
			accessors = append(accessors, r.Name)
		}
	}
	require.Equal(t, []string{"example-hub-tailnet-auth-key-accessor"}, accessors)
	deps := m.One(t, groupType).RPC.GetDependencies()
	require.NotContains(t, strings.Join(deps, "\n"), "listener-seed-accessor")
}

// The hub runs no listener, so the tailnet DNS name is nobody's address: the hub resolves whether
// the name is set, where its shape is still checked, or left out entirely.
func TestNatsHubNeedsNoTailnetDnsName(t *testing.T) {
	for _, row := range []struct{ name, tailnet string }{
		{"with a tailnet DNS name", "example-tailnet.ts.net"},
		{"without one", ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			args := exampleArgs()
			args.TailnetDnsName = row.tailnet
			require.NoError(t, gcp.ResolveNatsHub(args))
		})
	}
}

// The resolver refuses every invalid session user input, naming it.
func TestNatsHubResolverRefusesInvalidSessionUserInputs(t *testing.T) {
	user, err := nkeys.CreateUser()
	require.NoError(t, err)
	seed, err := user.Seed()
	require.NoError(t, err)
	requireResolverRefusals(t, []resolverRefusal{
		{
			name: "a session user without a public key",
			edit: func(a *gcp.NatsHubArgs) { a.SessionUser.NKey = "" },
			err:  []string{"sessionUser.nkey is required"},
		},
		{
			name: "a session user whose public key is a seed",
			edit: func(a *gcp.NatsHubArgs) { a.SessionUser.NKey = string(seed) },
			err:  []string{`sessionUser "example-sessions"`, "never a seed"},
		},
		{
			name: "a session user with an invalid name",
			edit: func(a *gcp.NatsHubArgs) { a.SessionUser.Name = "Example Sessions" },
			err:  []string{`sessionUser "Example Sessions"`, "name"},
		},
	})
}

// gcp:NatsHub ran its listener on the VM's loopback, 127.0.0.1, and Tailscale Serve forwarded the
// tailnet's TCP 9020 to that bind, so the listener was off its host in effect, and the listener
// cannot see the forward: it judges loopback by its own bind, where only a bearer kind closes /v1
// and its broker settings close nothing. The hub renders no bearer kind, so it passes the forward
// to the listener's check (core.CheckBindCredential) and refuses every listener at preview naming
// the forward, before anything registers: one with no credential, one with its broker settings
// (t-e-19's departure from the plan's row that this one passes), and production's own VM hub call
// (prodRenderVMHub), through the plugin's Construct. The check passes the forwarded bind only with
// a bearer kind, and a loopback bind with no forwarder, as dev stacks and compose run one. No hub
// renders the forward any more: its Serve configuration is the empty one, which clears a prior
// forward. Negative control, run by hand: the check reading the bind alone; the hub's refusals then
// name no forward, and every refused row fails naming it. The check counting the broker settings
// beside a forward is core's to catch (TestCheckBindCredential), since the hub passes the check
// no broker settings: it renders none for a listener it never runs.
func TestAForwardedLoopbackListenerIsOffLoopback(t *testing.T) {
	const forward = core.ForwardedToBind("TCPForward 127.0.0.1:9020")
	preview := func(t *testing.T, args *gcp.NatsHubArgs) (*pulumitest.Mocks, error) {
		t.Helper()
		m := newMocks()
		m.Preview = true
		return m, m.Run(func(ctx *pulumi.Context) error {
			prov, err := gcpsdk.NewProvider(ctx, "gcp-example", &gcpsdk.ProviderArgs{})
			if err != nil {
				return err
			}
			_, err = gcp.NewNatsHub(ctx, "example-hub", args, pulumi.Providers(prov))
			return err
		})
	}
	for _, row := range []struct {
		name     string
		listener *gcp.HubListenerArgs
	}{
		{"gcp:NatsHub's listener with no bearer kind and no broker settings",
			&gcp.HubListenerArgs{Image: listenerImage, SeedSecret: listenerSeed, NKey: keyListener}},
		{"the same listener with its broker settings",
			&gcp.HubListenerArgs{Image: listenerImage, SeedSecret: listenerSeed, NKey: keyListener,
				AgentSecretsUrl: brokerURL, AgentSecretsAudience: brokerAudience, PublicUrl: listenerOrigin}},
		{"a listener behind the auth service, with its broker settings", calloutListener()},
	} {
		t.Run(row.name, func(t *testing.T) {
			args := exampleArgs()
			args.Listener = row.listener
			m, err := preview(t, args)
			require.ErrorContains(t, err, hubListenerRefusal, "the preview is not refused naming the forward")
			require.ErrorContains(t, err, "its broker settings do not")
			require.Equal(t, []string{gcpProviderType}, m.Types(), "nothing but the provider is registered")
			var refused *core.InputError
			require.ErrorAs(t, gcp.ResolveNatsHub(args), &refused)
			require.Equal(t, "listener", refused.Input)
		})
	}
	t.Run("production's VM hub call, through Construct", func(t *testing.T) {
		engine := newProdRenderEngine()
		_, err := prodRenderConstruct(t, engine, prodRenderVMHub())
		require.ErrorContains(t, err, hubListenerRefusal, "production's VM hub call is not refused naming the forward")
		require.Empty(t, engine.mocks.Types(), "nothing is registered")
	})
	t.Run("a hub writes no forward", func(t *testing.T) {
		doc, _, err := gcp.CloudConfigOf(exampleArgs())
		require.NoError(t, err)
		require.JSONEq(t, `{}`, fileOf(t, doc, "/etc/tailscale-serve/serve.json"))
	})
	hubListener := core.ListenerConfig{MachineID: "example-hub", Port: core.DefaultListenerPort, ListenHost: "127.0.0.1"}
	t.Run("the same forwarded bind with a bearer kind", func(t *testing.T) {
		require.NoError(t, core.CheckBindCredential("listener", hubListener, true, forward))
	})
	t.Run("a loopback listener with no forwarder", func(t *testing.T) {
		require.NoError(t, core.CheckBindCredential("listener", hubListener, false, ""))
	})
}

// A seed in any of the listener's inputs reaches nothing: the hub refuses the listener naming the
// forward, whatever it holds, and the refusal quotes none of its values, so it never quotes the
// seed.
func TestNatsHubRefusesASeedInTheListenersInputs(t *testing.T) {
	seed, _ := testnats.User(t)
	lower := strings.ToLower(seed)
	digest := "@sha256:" + strings.Repeat("0", 64)
	for _, row := range []struct {
		name string
		edit func(*gcp.HubListenerArgs)
	}{
		{"a seed in the listener's image", func(l *gcp.HubListenerArgs) { l.Image = "example.invalid/" + lower + digest }},
		{"a seed as the listener's seed secret", func(l *gcp.HubListenerArgs) { l.SeedSecret = seed }},
		{"a seed in an allowed host", func(l *gcp.HubListenerArgs) { l.AllowedHosts = []string{lower + ".example.com"} }},
		{"a seed as the listener's broker audience", func(l *gcp.HubListenerArgs) { l.AgentSecretsAudience = seed }},
	} {
		t.Run(row.name, func(t *testing.T) {
			args := exampleArgs()
			args.Listener = &gcp.HubListenerArgs{Image: listenerImage, SeedSecret: listenerSeed, NKey: keyListener}
			row.edit(args.Listener)
			err := gcp.ResolveNatsHub(args)
			require.ErrorContains(t, err, hubListenerRefusal)
			require.NotContains(t, strings.ToLower(err.Error()), lower, "the refusal quotes the seed")
		})
	}
}
