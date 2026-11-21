package core_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nkeys"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/natsgrant"
)

// streamConfigurationCalls are the JetStream API calls that make a stream, change its
// configuration or remove it.
var streamConfigurationCalls = []string{
	"$JS.API.STREAM.CREATE.>", "$JS.API.STREAM.UPDATE.>", "$JS.API.STREAM.DELETE.>", "$JS.API.STREAM.RESTORE.>",
}

// grantedUser is a keyed user named name holding permissions.
func grantedUser(t *testing.T, name string, permissions core.Permissions) core.User {
	t.Helper()
	pair, err := nkeys.CreateUser()
	require.NoError(t, err)
	key, err := pair.PublicKey()
	require.NoError(t, err)
	publish, subscribe := permissions.Publish, permissions.Subscribe
	return core.User{Name: name, NKey: key, Publish: &publish, Subscribe: &subscribe, HasSeed: true}
}

// sessions is u as the sessions' user, which a client presenting no credential becomes: no seed
// authenticates it.
func sessions(u core.User) core.User {
	u.HasSeed = false
	return u
}

// realGrants is a hub's users as the hub renders them from natsgrant's sets (core's permission
// functions): a listener of its own, Dispatch's user, a host's, the sessions' and a daemon's.
func realGrants(t *testing.T) []core.User {
	t.Helper()
	return []core.User{
		grantedUser(t, "example-listener", core.ListenerHostPermissions("example-hub")),
		grantedUser(t, "example-dispatch-user", core.DispatchPermissions("example-dispatch")),
		grantedUser(t, "example-host", core.HostPermissions("example-host-a")),
		sessions(grantedUser(t, "example-session-user", core.SessionPermissions())),
		grantedUser(t, "example-daemon-user", core.DaemonPermissions()),
	}
}

// renderedPublish is each rendered user's publish allow list, by the name its comment line gives.
func renderedPublish(t *testing.T, text string) map[string][]string {
	t.Helper()
	users := map[string][]string{}
	for name, rules := range renderedPublishRules(t, text) {
		users[name] = rules.Allow
	}
	return users
}

// renderedPublishRules is each rendered user's publish rules, by the name its comment line gives.
func renderedPublishRules(t *testing.T, text string) map[string]core.Rules {
	t.Helper()
	users := map[string]core.Rules{}
	lines := strings.Split(text, "\n")
	for index, line := range lines {
		name, isUser := strings.CutPrefix(line, "    # ")
		if !isUser || index+1 >= len(lines) {
			continue
		}
		var rendered struct {
			Permissions struct {
				Publish core.Rules `json:"publish"`
			} `json:"permissions"`
		}
		require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(lines[index+1])), &rendered), "user %s", name)
		users[name] = rendered.Permissions.Publish
	}
	require.NotEmpty(t, users, "the text renders no user, so the rule below reads nothing")
	return users
}

// streamWriters names each rendered user whose publish rules still allow a stream configuration
// call, with the allow entry that does: an entry that reaches the call (server.SubjectsCollide)
// where no deny entry covers the whole call (server.SubjectMatchesFilter), as nats-server applies a
// deny after the allow list.
func streamWriters(users map[string]core.Rules) []string {
	var writers []string
	for name, rules := range users {
		for _, entry := range rules.Allow {
			if slices.ContainsFunc(streamConfigurationCalls, func(call string) bool {
				return server.SubjectsCollide(entry, call) && !slices.ContainsFunc(rules.Deny, func(deny string) bool { return server.SubjectMatchesFilter(call, deny) })
			}) {
				writers = append(writers, name+": "+entry)
			}
		}
	}
	slices.Sort(writers)
	return writers
}

// Beside a callout no rendered static user can make, change or remove a stream: the auth service
// answers no client but the operator with one (natsgrant.ForCallout), and a static user beside it
// is narrowed the same way, so the callout golden's placeholder users and a hub's users built from
// natsgrant's sets each render no stream create, update, delete or restore, every other entry kept.
func TestNoRenderedStaticUserCreatesOrChangesAStream(t *testing.T) {
	text, err := withCallout().Render(hubHeader)
	require.NoError(t, err)
	placeholders := renderedPublishRules(t, text)
	require.Empty(t, streamWriters(placeholders), "a static user rendered beside a callout may make, change or remove a stream")
	require.Equal(t, core.Rules{
		Allow: []string{"notifications.dispatch.>", "$JS.API.>"},
		Deny:  []string{"$JS.API.STREAM.CREATE.>", "$JS.API.STREAM.UPDATE.>", "$JS.API.STREAM.DELETE.>", "$JS.API.STREAM.RESTORE.>"},
	}, placeholders["example-dispatch"], "a wide entry beside a callout is kept beside a deny of each stream call")

	real := withCallout()
	real.Users = append(realGrants(t), authService())
	real.NoAuthUser = "example-session-user"
	text, err = real.Render(hubHeader)
	require.NoError(t, err)
	require.Empty(t, streamWriters(renderedPublishRules(t, text)), "a static user rendered beside a callout may make, change or remove a stream")
	users := renderedPublish(t, text)
	require.Equal(t, natsgrant.ForCallout(natsgrant.ListenerHost("example-hub")).Publish, users["example-listener"],
		"the listener's rendered list beside a callout is not its answer from the auth service")
	require.Equal(t, []string{"$SYS._INBOX.>"}, users["example-auth"], "the auth service's own user was changed")
}

// A static user that would hold nothing to publish once narrowed is refused, naming it: nats-server
// reads an empty allow list as every subject.
func TestAStaticUserThatHoldsOnlyStreamWritesIsRefusedBesideACallout(t *testing.T) {
	a := withCallout()
	a.Users = append(a.Users, grantedUser(t, "example-mover", core.Permissions{
		Publish:   core.Rules{Allow: []string{"$JS.API.STREAM.UPDATE.*", "$JS.API.STREAM.DELETE.*"}},
		Subscribe: core.Rules{Allow: []string{"_INBOX.>"}},
	}))
	_, err := a.Render(hubHeader)
	require.ErrorContains(t, err, "example-mover")
}

// mergeBaseGrants is natsgrant's record of every static set as the hub rendered it before the sets
// moved there (natsgrant/testdata/merge-base-grants.json, written once and never regenerated), for
// each machine id it names.
type mergeBaseGrants struct {
	MachineIDs   []string               `json:"machine_ids"`
	Listener     map[string]recordedSet `json:"listener"`
	ListenerHost map[string]recordedSet `json:"listener_host"`
	Dispatch     map[string]recordedSet `json:"dispatch"`
	Host         map[string]recordedSet `json:"host"`
	Daemon       recordedSet            `json:"daemon"`
}

type recordedSet struct {
	Publish   []string `json:"publish"`
	Subscribe []string `json:"subscribe"`
}

// Without a callout every static user's grant set is the one the hub rendered before this
// package's narrowing existed, its stream creates and updates included, element for element, and
// the static goldens render byte for byte: a hub with no auth service keeps its programs making
// their own streams (R9). The record is fixed, so a narrowing written into natsgrant's sets
// themselves fails here, which a comparison with natsgrant's own functions could not see.
func TestTheStaticGrantSetsAreUnchanged(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "natsgrant", "testdata", "merge-base-grants.json"))
	require.NoError(t, err)
	var base mergeBaseGrants
	require.NoError(t, json.Unmarshal(raw, &base))
	require.NotEmpty(t, base.MachineIDs, "the record names no machine id, so the sets below are checked against nothing")
	for _, id := range base.MachineIDs {
		for name, pair := range map[string]struct {
			got  core.Permissions
			want recordedSet
		}{
			"ListenerPermissions":     {core.ListenerPermissions(id), base.Listener[id]},
			"ListenerHostPermissions": {core.ListenerHostPermissions(id), base.ListenerHost[id]},
			"DispatchPermissions":     {core.DispatchPermissions(id), base.Dispatch[id]},
			"HostPermissions":         {core.HostPermissions(id), base.Host[id]},
		} {
			require.NotEmpty(t, pair.want.Publish, "%s(%s): the record holds no set", name, id)
			require.Equal(t, pair.want.Publish, pair.got.Publish.Allow, "%s(%s) publish", name, id)
			require.Equal(t, pair.want.Subscribe, pair.got.Subscribe.Allow, "%s(%s) subscribe", name, id)
		}
	}
	require.Equal(t, base.Daemon.Publish, core.DaemonPermissions().Publish.Allow, "DaemonPermissions publish")

	static := core.Authorization{Users: realGrants(t), NoAuthUser: "example-session-user"}
	text, err := static.Render(hubHeader)
	require.NoError(t, err)
	users := renderedPublish(t, text)
	require.Equal(t, natsgrant.ListenerHost("example-hub").Publish, users["example-listener"], "a static listener was narrowed with no callout")

	text, err = placeholder().Render(header)
	require.NoError(t, err)
	requireSameText(t, golden(t, "authorization.golden"), text)
	text, err = hubShaped().Render(hubHeader)
	require.NoError(t, err)
	requireSameText(t, golden(t, "authorization-before.golden"), text)
}
