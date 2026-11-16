//go:build natslive

package gcp_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/internal/testnats"
)

func authConfOf(t *testing.T, args *gcp.ClusteredNatsHubArgs) string {
	t.Helper()
	s, err := gcp.ResolveClusteredNatsHub(args)
	require.NoError(t, err)
	return s.AuthConf
}

// The measurement behind envoy-infra.sjawhar.dev/auth-callout: nats-server v2.15.1, the version
// the clustered hub's chart runs and this test binary links in, takes a users change in auth.conf
// on a reload, as the chart's reloader signals one, but refuses the whole reload when the callout
// changes, adding it or changing its lines alike ("config reload not supported for AuthCallout"),
// and keeps serving what it had. So a callout change reaches a server only when it restarts, which
// the annotation forces by changing the pod template (TestClusteredNatsHubRollsThePodsForACalloutChange).
// The users-change reload is the control: it shows a reload is taken at all, so each refusal is
// the callout's and not the path's.
func TestClusteredNatsHubReloadRefusesACalloutChange(t *testing.T) {
	without := withoutTailnet(clusteredArgs())
	without.Users = append(without.Users, authServiceUser())
	with := calloutClusteredHub()
	server, _ := testnats.StartConfig(t, authConfOf(t, without), nil, true)

	t.Run("a users change without a callout reloads", func(t *testing.T) {
		changed := withoutTailnet(clusteredArgs())
		changed.Users = append(changed.Users, authServiceUser())
		changed.Users[1].Permissions.Publish.Allow = []string{"notifications.>"}
		require.NoError(t, server.Reload(t, authConfOf(t, changed)))
	})
	t.Run("adding the callout is refused", func(t *testing.T) {
		err := server.Reload(t, authConfOf(t, with))
		require.ErrorContains(t, err, "config reload not supported for AuthCallout")
	})
	t.Run("on a server that started with the callout", func(t *testing.T) {
		server.Restart(t, authConfOf(t, with))
		t.Run("a grant change, which leaves the callout's lines, reloads", func(t *testing.T) {
			changed := calloutClusteredHub()
			changed.Users[1].Permissions.Publish.Allow = []string{"notifications.>"}
			require.NoError(t, server.Reload(t, authConfOf(t, changed)))
		})
		t.Run("a user added to auth_users is refused", func(t *testing.T) {
			_, added := testnats.User(t)
			changed := calloutClusteredHub()
			changed.Users = append(changed.Users, authServiceUser())
			changed.Users[len(changed.Users)-1].Name, changed.Users[len(changed.Users)-1].NKey = "example-auth-b", added
			err := server.Reload(t, authConfOf(t, changed))
			require.ErrorContains(t, err, "config reload not supported for AuthCallout")
		})
		t.Run("another xkey is refused", func(t *testing.T) {
			changed := calloutClusteredHub()
			changed.AuthCallout.XKey = otherXKey(t)
			err := server.Reload(t, authConfOf(t, changed))
			require.ErrorContains(t, err, "config reload not supported for AuthCallout")
		})
	})
}
