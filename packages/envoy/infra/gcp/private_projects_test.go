package gcp_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/infra/nats"
)

// The callout keys of the tests in this package: the auth service's user, the account whose seed
// signs its answers, and the curve key its requests are sealed to. Public keys only; their seeds
// were discarded when they were made.
const (
	keyAuth       = "UDOLL5K64CAO7IWO3XJTHILQNTX2L4MQ7IWIAATEEPU42KK2I4QTPOT4"
	calloutIssuer = "AB2TCXFBYSIN24KFPVJARBN5CHCL4ZCV5SBWSONTZ2WYXS3XTAEK6S3G"
	calloutXKey   = "XBVCTZK3PV5Y7TBIVYTDZBP3O2YGHUUL7MMENN4DTZIREJCPGLTAF6EL"
)

func authServiceUser() gcp.NATSUserArgs {
	return gcp.NATSUserArgs{Name: "example-auth", NKey: keyAuth, Permissions: &gcp.PermissionsArgs{
		Publish:   nats.RulesArgs{Allow: []string{"$SYS._INBOX.>"}},
		Subscribe: nats.RulesArgs{Allow: []string{"$SYS.REQ.USER.AUTH"}},
	}}
}

func authCallout() *nats.AuthCalloutArgs {
	return &nats.AuthCalloutArgs{Issuer: calloutIssuer, XKey: calloutXKey, ServiceUser: "example-auth"}
}

// privateNatsHub is the one gcp:NatsHub R12 lets hold a private project: the auth service's user
// alone behind the callout, with no listener of its own and no session user.
func privateNatsHub() *gcp.NatsHubArgs {
	args := exampleArgs()
	args.Users = []gcp.NATSUserArgs{authServiceUser()}
	args.Listener, args.SessionUser = nil, nil
	args.AuthCallout, args.PrivateProjects = authCallout(), true
	return args
}

// privateClusteredHub is the one gcp:ClusteredNatsHub R12 lets hold a private project, as
// privateNatsHub is gcp:NatsHub's.
func privateClusteredHub() *gcp.ClusteredNatsHubArgs {
	args := withoutTailnet(clusteredArgs())
	args.Users = []gcp.NATSUserArgs{authServiceUser()}
	args.SessionUser = nil
	args.AuthCallout, args.PrivateProjects = authCallout(), true
	return args
}

// privateProjectsRefusal is one hub privateProjects cannot be set on: edit makes the private hub's
// users, session user and callout into it, who names what a client could connect as past the auth
// service, and want is the refusal.
type privateProjectsRefusal struct {
	name, who string
	edit      func(users *[]gcp.NATSUserArgs, session **gcp.SessionUserArgs, callout **nats.AuthCalloutArgs)
	want      string
}

// privateProjectsRefusals are the ways a hub could hold a private project while a client can
// connect past the auth service, which c-private-needs-callout names: a session user, whom a client
// presenting no credential becomes; a client user with a seed; no callout at all; and, from the
// review of #4118 at e7c174fa1b, a client's user named as the auth service's (a listener's own user
// here, whose grant and seed are its listener's), or a service user granted more than an auth
// service needs, either of which nats-server admits by its seed alone, past the service.
func privateProjectsRefusals() []privateProjectsRefusal {
	return []privateProjectsRefusal{
		{
			name: "beside a session user", who: `the session user "example-sessions"`,
			edit: func(_ *[]gcp.NATSUserArgs, session **gcp.SessionUserArgs, _ **nats.AuthCalloutArgs) {
				*session = &gcp.SessionUserArgs{Name: "example-sessions", NKey: keySessions}
			},
			want: `privateProjects: sessionUser "example-sessions" is noAuthUser: a client presenting no credential would become it while a project is private`,
		},
		{
			name: "beside a client user with a seed", who: `the user "example-host-a", whose seed its host stores`,
			edit: func(users *[]gcp.NATSUserArgs, _ **gcp.SessionUserArgs, _ **nats.AuthCalloutArgs) {
				*users = append(*users, exampleArgs().Users[0])
			},
			want: `privateProjects: users[1] "example-host-a": a stored client seed while a project is private`,
		},
		{
			name: "without authCallout", who: "no callout",
			edit: func(_ *[]gcp.NATSUserArgs, _ **gcp.SessionUserArgs, callout **nats.AuthCalloutArgs) { *callout = nil },
			want: "privateProjects: a project can be private only while the server admits each client through the auth service, and the authorization has no callout",
		},
		{
			name: "with a listener's own user as the auth service's user", who: `the listener's own user "example-host-a" as the auth service's user`,
			edit: func(users *[]gcp.NATSUserArgs, _ **gcp.SessionUserArgs, callout **nats.AuthCalloutArgs) {
				*users = []gcp.NATSUserArgs{exampleArgs().Users[0]}
				(*callout).ServiceUser = "example-host-a"
			},
			want: `privateProjects: authCallout.serviceUser "example-host-a" names users[0], a listenerMachineId user, whose seed its client stores; the auth service connects as a permissions user of its own`,
		},
		{
			name: "with a service user granted everything", who: `the service user "example-auth" granted everything`,
			edit: func(users *[]gcp.NATSUserArgs, _ **gcp.SessionUserArgs, _ **nats.AuthCalloutArgs) {
				(*users)[0].Permissions = &gcp.PermissionsArgs{Publish: nats.RulesArgs{Allow: []string{">"}}, Subscribe: nats.RulesArgs{Allow: []string{">"}}}
			},
			want: `privateProjects: callout.serviceUser "example-auth": publish.allow[0] ">" is wider than the auth service's grant`,
		},
	}
}

// requirePrivateProjectsRefusal requires err to refuse privateProjects as row says, as a
// core.InputError naming privateProjects. A hub that took the row instead fails naming who it
// would have admitted past the auth service.
func requirePrivateProjectsRefusal(t *testing.T, row privateProjectsRefusal, err error) {
	t.Helper()
	require.ErrorContains(t, err, row.want, "privateProjects was taken beside %s", row.who)
	var input *core.InputError
	require.ErrorAs(t, err, &input)
	require.Equal(t, "privateProjects", input.Input, "%v", err)
}

// TestPrivateProjectsNeedTheAuthService (c-private-needs-callout, R12): each hub component refuses
// privateProjects beside a session user, beside a client user with a seed, without authCallout,
// with a client's user named as the auth service's, and with a service user granted more than an
// auth service needs, naming the input and who could connect past the auth service, and registers
// nothing; with authCallout and the service's user alone, it registers. aws:NatsService's rows
// are in aws/natsservice_callout_test.go, under the same name.
//
// Negative controls, each run on its own, and each making the component take the configuration
// R12 forbids rather than refuse it in other words (core/nats_callout.go validateCallout, and for
// the last two gcp/natshub.go authorization):
//   - Let the session user through: delete the noAuthUser refusal, and make the users loop skip
//     a.NoAuthUser beside the service user. The session-user rows of all three components fail
//     with "privateProjects was taken beside the session user …", naming it.
//   - Let a client user with a seed through: delete the users loop. The seed rows fail with
//     "privateProjects was taken beside the user …", naming "example-host-a" ("example-listener"
//     on aws:NatsService).
//   - Let privateProjects through without a callout: make the a.Callout == nil branch return nil
//     (deleting it would dereference the nil callout in the users loop). The callout rows fail
//     with "privateProjects was taken beside no callout".
//   - Let a wide service user through: delete the service user's grant check. The rows of the
//     service user granted everything fail with "privateProjects was taken beside the service user
//     "example-auth" granted everything". The listener's own user's row stays refused, by the
//     hubs' kind check.
//   - Let a client's user be the service's: delete both the hubs' kind check and the grant check.
//     The listener's own user's rows fail with "privateProjects was taken beside the listener's
//     own user "example-host-a" as the auth service's user". Deleting the kind check alone leaves
//     the grant check refusing the listener-host set's wide grant, with its own message: the two
//     refusals are two doors.
//   - Let a private clustered hub open its monitoring port: delete gcp/clusteredhub.go's refusal
//     of monitorNamespaces beside privateProjects. The monitorNamespaces row fails with
//     "privateProjects was taken beside the monitoring port open to the namespace "legion"".
func TestPrivateProjectsNeedTheAuthService(t *testing.T) {
	t.Run("gcp:NatsHub", func(t *testing.T) {
		for _, row := range privateProjectsRefusals() {
			t.Run(row.name, func(t *testing.T) {
				args := privateNatsHub()
				row.edit(&args.Users, &args.SessionUser, &args.AuthCallout)
				requirePrivateProjectsRefusal(t, row, gcp.ResolveNatsHub(args))
				m, err := run(t, args, nil, true)
				require.ErrorContains(t, err, row.want)
				require.Equal(t, []string{gcpProviderType}, m.Types(), "nothing registered beyond the test's provider")
			})
		}
		t.Run("with authCallout and the service's user alone, it registers", func(t *testing.T) {
			require.NoError(t, gcp.ResolveNatsHub(privateNatsHub()))
			m, err := run(t, privateNatsHub(), nil, true)
			require.NoError(t, err)
			require.Contains(t, m.Types(), hubType)
		})
	})
	t.Run("gcp:ClusteredNatsHub", func(t *testing.T) {
		for _, row := range privateProjectsRefusals() {
			t.Run(row.name, func(t *testing.T) {
				args := privateClusteredHub()
				row.edit(&args.Users, &args.SessionUser, &args.AuthCallout)
				_, err := gcp.ResolveClusteredNatsHub(args)
				requirePrivateProjectsRefusal(t, row, err)
				m, err := runClustered(t, "example-clustered-hub", args, nil)
				require.ErrorContains(t, err, row.want)
				require.Empty(t, m.Registrations(), "nothing registered")
			})
		}
		// nats-server's monitoring port answers /connz, /subsz and /jsz with no authentication,
		// naming the subjects, connections and consumers of every client, a private project's
		// among them, so a private hub opens it to no namespace.
		t.Run("beside monitorNamespaces", func(t *testing.T) {
			args := privateClusteredHub()
			args.MonitorNamespaces = []string{"legion"}
			_, err := gcp.ResolveClusteredNatsHub(args)
			const want = `monitorNamespaces and privateProjects: nats-server's monitoring port, 8222, answers /connz, /subsz and /jsz with no authentication`
			require.ErrorContains(t, err, want, `privateProjects was taken beside the monitoring port open to the namespace "legion"`)
			var input *core.InputError
			require.ErrorAs(t, err, &input)
			require.Equal(t, "monitorNamespaces", input.Input, "%v", err)
			m, err := runClustered(t, "example-clustered-hub", args, nil)
			require.ErrorContains(t, err, want)
			require.Empty(t, m.Registrations(), "nothing registered")
		})
		t.Run("with authCallout and the service's user alone, it registers, opening no monitoring port", func(t *testing.T) {
			_, err := gcp.ResolveClusteredNatsHub(privateClusteredHub())
			require.NoError(t, err)
			m, err := runClustered(t, "example-clustered-hub", privateClusteredHub(), nil)
			require.NoError(t, err)
			require.Contains(t, m.Types(), clusteredHubType)
			for _, rule := range clusteredPolicyRules(t, m) {
				for _, port := range rule.(map[string]any)["ports"].([]any) {
					require.NotEqual(t, 8222.0, port.(map[string]any)["port"], "the private hub's NetworkPolicy admits the monitoring port: %v", rule)
				}
			}
		})
	})
}
