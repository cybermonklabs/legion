package aws_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	envoyaws "github.com/sjawhar/envoy/infra/aws"
	"github.com/sjawhar/envoy/infra/core"
	envoynats "github.com/sjawhar/envoy/infra/nats"
)

// The callout keys of these tests: the auth service's user, the account whose seed signs its
// answers, and the curve key its requests are sealed to. Public keys only; their seeds were
// discarded when they were made.
const (
	keyAuth       = "UDOLL5K64CAO7IWO3XJTHILQNTX2L4MQ7IWIAATEEPU42KK2I4QTPOT4"
	calloutIssuer = "AB2TCXFBYSIN24KFPVJARBN5CHCL4ZCV5SBWSONTZ2WYXS3XTAEK6S3G"
	calloutXKey   = "XBVCTZK3PV5Y7TBIVYTDZBP3O2YGHUUL7MMENN4DTZIREJCPGLTAF6EL"
)

func authServiceUser() envoyaws.UserArgs {
	return envoyaws.UserArgs{
		Name:      "example-auth",
		NKey:      keyAuth,
		Publish:   &envoynats.RulesArgs{Allow: []string{"$SYS._INBOX.>"}},
		Subscribe: &envoynats.RulesArgs{Allow: []string{"$SYS.REQ.USER.AUTH"}},
		HasSeed:   true,
	}
}

// privateNatsService is the one aws:NatsService R12 lets hold a private project: the auth
// service's user alone behind the callout, and no noAuthUser.
func privateNatsService() *envoyaws.NatsServiceArgs {
	args := exampleArgs()
	args.Authorization = &envoyaws.AuthorizationArgs{
		Users:           []envoyaws.UserArgs{authServiceUser()},
		Callout:         &envoynats.AuthCalloutArgs{Issuer: calloutIssuer, XKey: calloutXKey, ServiceUser: "example-auth"},
		PrivateProjects: true,
	}
	return args
}

// natsContainer is the NATS container the run registered.
func natsContainer(t *testing.T, args *envoyaws.NatsServiceArgs) map[string]any {
	t.Helper()
	m, _, err := run(t, args, true)
	require.NoError(t, err)
	var containers []map[string]any
	require.NoError(t, json.Unmarshal([]byte(m.One(t, taskDefinitionType).Inputs["containerDefinitions"].StringValue()), &containers))
	for _, c := range containers {
		if c["name"] == "nats" {
			return c
		}
	}
	require.Fail(t, "the task definition has no nats container")
	return nil
}

// natsCommand is the NATS container's command the run registered: the script its entry point,
// /bin/sh -c, runs.
func natsCommand(t *testing.T, args *envoyaws.NatsServiceArgs) []any {
	t.Helper()
	return natsContainer(t, args)["command"].([]any)
}

// TestPrivateProjectsNeedTheAuthService (c-private-needs-callout, R12), for aws:NatsService, whose
// authorization input mirrors core.Authorization: privateProjects beside a noAuthUser, whom a
// client presenting no credential becomes, beside a client user with a seed, without a callout,
// and with a service user granted more than an auth service needs, is each refused as a
// core.InputError naming authorization, with the user or the missing callout in its message; with
// the callout and the service's user alone, it registers. The hub components' rows are in
// gcp/private_projects_test.go, under the same name, and its comment names the negative controls.
func TestPrivateProjectsNeedTheAuthService(t *testing.T) {
	for _, row := range []struct {
		name, who string
		edit      func(a *envoyaws.AuthorizationArgs)
		want      string
	}{
		{
			name: "beside a session user", who: `the session user "example-fallback"`,
			edit: func(a *envoyaws.AuthorizationArgs) {
				a.Users = append(a.Users, exampleAuthorization().Users[2])
				a.NoAuthUser = "example-fallback"
			},
			want: `privateProjects: users[1] "example-fallback" is noAuthUser: a client presenting no credential would become it while a project is private`,
		},
		{
			name: "beside a client user with a seed", who: `the user "example-listener", whose seed its host stores`,
			edit: func(a *envoyaws.AuthorizationArgs) { a.Users = append(a.Users, exampleAuthorization().Users[0]) },
			want: `privateProjects: users[1] "example-listener": a stored client seed while a project is private`,
		},
		{
			name: "without a callout", who: "no callout",
			edit: func(a *envoyaws.AuthorizationArgs) { a.Callout = nil },
			want: "privateProjects: a project can be private only while the server admits each client through the auth service, and the authorization has no callout",
		},
		{
			// A client's user named as the auth service's: it keeps the client's grant and seed.
			name: "with a client user as the auth service's user", who: `the user "example-listener" as the auth service's user`,
			edit: func(a *envoyaws.AuthorizationArgs) {
				a.Users = []envoyaws.UserArgs{exampleAuthorization().Users[0]}
				a.Callout.ServiceUser = "example-listener"
			},
			want: `privateProjects: callout.serviceUser "example-listener": publish.allow[0] "envoy.example.>" is wider than the auth service's grant`,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			args := privateNatsService()
			row.edit(args.Authorization)
			err := args.Validate()
			require.ErrorContains(t, err, row.want, "privateProjects was taken beside %s", row.who)
			var input *core.InputError
			require.ErrorAs(t, err, &input)
			require.Equal(t, "authorization", input.Input, "%v", err)
			m, _, err := run(t, args, true)
			require.ErrorContains(t, err, row.want)
			require.Equal(t, []string{awsProviderType}, m.Types(), "nothing but the provider is registered")
		})
	}
	t.Run("with the callout and the service's user alone, it registers", func(t *testing.T) {
		require.NoError(t, privateNatsService().Validate())
		m, _, err := run(t, privateNatsService(), true)
		require.NoError(t, err)
		require.Contains(t, m.Types(), taskDefinitionType)
	})
}

// The callout reaches the NATS container's start script as core renders it, and without one the
// script is the golden's (TestNatsServiceRegistersTheReferenceChildren holds the rest unchanged).
// The script is private, so its users sit in the accounts block (core's accountsBlock) and the
// authorization block holds the callout alone, run in the client account.
func TestNatsServiceRendersTheCallout(t *testing.T) {
	args := privateNatsService()
	authorization := core.Authorization{
		Users: []core.User{{Name: "example-auth", NKey: keyAuth, Publish: &core.Rules{Allow: []string{"$SYS._INBOX.>"}},
			Subscribe: &core.Rules{Allow: []string{"$SYS.REQ.USER.AUTH"}}, HasSeed: true}},
		Callout:         &core.AuthCallout{Issuer: calloutIssuer, XKey: calloutXKey, ServiceUser: "example-auth"},
		PrivateProjects: true,
	}
	script, err := authorization.StartScript(args.ConfigHeader, args.ServerArgs, *args.ConfigPath)
	require.NoError(t, err)
	require.Contains(t, script, "\nauthorization {\n  auth_callout {\n")
	require.Contains(t, script, "    account: \""+core.ClientAccount+"\"\n")
	require.NotContains(t, script, "timeout")
	require.Equal(t, []any{script}, natsCommand(t, args))
}

// Beside privateProjects the start script binds monitoring to core.PrivateMonitorHost, so the NATS
// container's health check reads /healthz there: on the hub's image busybox wget takes ::1 for
// localhost when the loopback has IPv6, where nothing listens (core's
// TestThePrivateTasksHealthCheckPasses runs it). Without privateProjects the check reads localhost,
// as before (nats-authorized.golden and nats-unauthorized.golden pin the whole task).
//
// Control: make NatsLaunch leave monitorOnLoopback unset; the private row fails.
func TestNatsServiceHealthCheckReadsWhereMonitoringListens(t *testing.T) {
	healthCheck := func(t *testing.T, args *envoyaws.NatsServiceArgs) any {
		t.Helper()
		return natsContainer(t, args)["healthCheck"].(map[string]any)["command"]
	}
	require.Equal(t, []any{"CMD-SHELL", "wget -qO- http://" + core.PrivateMonitorHost + ":8222/healthz || exit 1"}, healthCheck(t, privateNatsService()))
	public := privateNatsService()
	public.Authorization.PrivateProjects = false
	require.Equal(t, []any{"CMD-SHELL", "wget -qO- http://localhost:8222/healthz || exit 1"}, healthCheck(t, public))
	require.Equal(t, []any{"CMD-SHELL", "wget -qO- http://localhost:8222/healthz || exit 1"}, healthCheck(t, exampleArgs()))
}
