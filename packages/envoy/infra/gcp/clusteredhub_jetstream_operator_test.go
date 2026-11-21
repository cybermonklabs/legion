package gcp_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/infra/nats"
	"github.com/sjawhar/envoy/internal/testnats"
)

// TestTheClusteredHubPlacesItsJetStreamOperator: jetStreamOperator names a permissions entry of
// users, which a private hub's auth.conf places in the account holding JetStream, where no client
// is, and lists in auth_users, so the server never sends it to the auth service. It is refused,
// naming jetStreamOperator, when it names a client's entry (a machineId user here, whose grant and
// seed are a host's), and core refuses it without privateProjects.
//
// Control: drop jetStreamOperator's kind check; the client-entry row registers the host in
// JETSTREAM and fails.
func TestTheClusteredHubPlacesItsJetStreamOperator(t *testing.T) {
	_, operatorKey := testnats.User(t)
	operator := gcp.NATSUserArgs{Name: "example-jetstream", NKey: operatorKey, Permissions: &gcp.PermissionsArgs{
		Publish:   nats.RulesArgs{Allow: []string{">"}},
		Subscribe: nats.RulesArgs{Allow: []string{">"}},
	}}
	args := privateClusteredHub()
	args.Users = append(args.Users, operator)
	args.JetStreamOperator = "example-jetstream"
	s, err := gcp.ResolveClusteredNatsHub(args)
	require.NoError(t, err)
	jetstream, envoy, found := strings.Cut(s.AuthConf, "  "+core.ClientAccount+" {\n")
	require.True(t, found, "no %s account in %q", core.ClientAccount, s.AuthConf)
	require.Contains(t, jetstream, "  "+core.JetStreamAccount+" {\n    jetstream: enabled\n    users: [\n      # example-jetstream\n")
	require.NotContains(t, envoy, "# example-jetstream")
	require.Contains(t, s.AuthConf, `auth_users: ["`+keyAuth+`", "`+operatorKey+`"]`)

	refused := func(t *testing.T, args *gcp.ClusteredNatsHubArgs, want string) {
		t.Helper()
		_, err := gcp.ResolveClusteredNatsHub(args)
		require.ErrorContains(t, err, want)
		var input *core.InputError
		require.True(t, errors.As(err, &input), "%v", err)
		require.Equal(t, "jetStreamOperator", input.Input)
	}
	t.Run("naming a client's entry", func(t *testing.T) {
		args := privateClusteredHub()
		args.Users = append(args.Users, gcp.NATSUserArgs{Name: "example-host", NKey: operatorKey, MachineID: pulumi.StringRef("example-host")})
		args.JetStreamOperator = "example-host"
		refused(t, args, "jetStreamOperator names users[1], a machineId user, which is a client of the hub")
	})
	t.Run("without privateProjects", func(t *testing.T) {
		args := privateClusteredHub()
		args.PrivateProjects = false
		args.Users = append(args.Users, operator)
		args.JetStreamOperator = "example-jetstream"
		refused(t, args, `jetStreamOperator "example-jetstream": only a render with privateProjects has the JETSTREAM account`)
	})
}
