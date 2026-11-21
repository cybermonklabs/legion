package core_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/nats-io/nkeys"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/internal/bus"
)

// jetStreamOperatorKey is the operator's key in authorization-accounts.golden. A public key only;
// its seed was discarded when it was made.
const jetStreamOperatorKey = "UDXREYGN6OYHDDYF45MCYWJGYYAPJ7VH6EJBFUQYMLNMI5XCW54ZAIQD"

// jetStreamOperator is the operator of the account that holds JetStream: a keyed user whose seed
// the move's Job holds, granted everything in that account, where no client is.
func jetStreamOperator() core.User {
	return core.User{
		Name:      "example-jetstream",
		NKey:      jetStreamOperatorKey,
		Publish:   &core.Rules{Allow: []string{">"}},
		Subscribe: &core.Rules{Allow: []string{">"}},
		HasSeed:   true,
	}
}

// testSeed is a user nkey seed made for one test, which no refusal may quote.
func testSeed(t *testing.T) string {
	t.Helper()
	user, err := nkeys.CreateUser()
	require.NoError(t, err)
	seed, err := user.Seed()
	require.NoError(t, err)
	return string(seed)
}

// privateWithOperator is privateOnly with the operator of the account that holds JetStream, the
// authorization a hub renders while its streams move into that account
// (cmd/jetstream-account-move): authorization-accounts.golden, whose keys that command's rehearsal
// and the account probe's test hub replace with keys of their own.
func privateWithOperator() core.Authorization {
	a := privateOnly()
	a.Users = append(a.Users, jetStreamOperator())
	a.JetStreamOperator = "example-jetstream"
	return a
}

// TestAnAllPublicHubRendersAsBefore (R9): without privateProjects no render holds an accounts
// block, and each is byte for byte the text its golden holds, every golden written by a render at
// this change's merge base: authorization-before.golden, authorization-callout.golden and
// authorization.golden, which this change leaves as they were, and
// authorization-callout-private-before.golden, the merge base's private render of privateOnly(),
// copied from it, whose text without the server tag's two lines is that authorization without
// privateProjects.
//
// Control: make calloutParts render the accounts block beside any callout, whatever
// PrivateProjects says; the callout rows fail at line 3, where the golden opens the authorization
// block and the render opens the accounts block.
func TestAnAllPublicHubRendersAsBefore(t *testing.T) {
	public := privateOnly()
	public.PrivateProjects = false
	for _, row := range []struct {
		name   string
		a      core.Authorization
		header []string
		golden string
	}{
		{"a hub with no callout", hubShaped(), hubHeader, golden(t, "authorization-before.golden")},
		{"a hub with a callout and its static users", withCallout(), hubHeader, golden(t, "authorization-callout.golden")},
		{"the reference authorization", placeholder(), header, golden(t, "authorization.golden")},
		{"the private hub's users without privateProjects", public, header,
			strings.TrimSuffix(golden(t, "authorization-callout-private-before.golden"), privateProjectsTagLines)},
	} {
		t.Run(row.name, func(t *testing.T) {
			text, err := row.a.Render(row.header)
			require.NoError(t, err)
			requireSameText(t, row.golden, text)
			for _, line := range []string{"accounts {", core.JetStreamAccount + " {", core.ClientAccount + " {", "account:"} {
				require.NotContains(t, text, line, "an all-public render holds %q", line)
				require.NotContains(t, row.a.CalloutLines(), line)
			}
		})
	}
}

// TestAPrivateRenderPutsJetStreamInAnAccountOfItsOwn: with privateProjects the text opens with the
// accounts block (authorization-accounts.golden): JETSTREAM, with JetStream on, holds the
// jetStreamOperator alone and exports every service and stream its clients reach, $JS.API.> as a
// stream of responses; ENVOY holds every other user and imports each export; the authorization
// block holds the callout alone, run in ENVOY, whose auth_users names every keyed user, the
// operator's included, so neither is sent to the auth service. The exports name each subject the
// notification stream carries and no wider one, so a core subject such as a session's relay
// subject stays in ENVOY. Without a jetStreamOperator, JETSTREAM holds no user
// (authorization-callout-private.golden). CalloutLines holds the whole accounts block, so a change
// of it rolls the clustered hub's pods.
//
// Controls: drop response_type from the $JS.API.> export, or export notifications.> in place of
// the stream's subjects; the golden row fails at that line.
func TestAPrivateRenderPutsJetStreamInAnAccountOfItsOwn(t *testing.T) {
	text, err := privateWithOperator().Render(header)
	require.NoError(t, err)
	requireSameText(t, golden(t, "authorization-accounts.golden"), text)

	_, fromAccounts, found := strings.Cut(text, "\naccounts {\n")
	require.True(t, found, "no accounts block after the header in %q", text)
	accounts, rest, found := strings.Cut("accounts {\n"+fromAccounts, "authorization {\n")
	require.True(t, found)
	jetstream, envoy, found := strings.Cut(accounts, "  "+core.ClientAccount+" {\n")
	require.True(t, found, "no %s account in %q", core.ClientAccount, accounts)
	require.Contains(t, jetstream, "  "+core.JetStreamAccount+" {\n    jetstream: enabled\n")
	require.Contains(t, jetstream, "# example-jetstream\n")
	require.NotContains(t, jetstream, "# example-auth\n")
	require.Contains(t, envoy, "# example-auth\n")
	require.NotContains(t, envoy, "# example-jetstream\n")
	require.NotContains(t, envoy, "jetstream", "ENVOY has JetStream of its own, which would answer its calls in place of the import")
	require.Contains(t, jetstream, `{"response_type": "stream", "service": "$JS.API.>"}`)
	for _, subject := range bus.StreamCarriedSubjects() {
		require.Contains(t, jetstream, `{"service": "`+subject+`"}`)
		require.Contains(t, envoy, `{"service": {"account": "JETSTREAM", "subject": "`+subject+`"}}`)
	}
	for _, wide := range []string{`"notifications.>"`, `"` + bus.DeliverSubjectPrefix + `>"`} {
		require.NotContains(t, accounts, wide)
	}
	require.True(t, strings.HasPrefix(rest, "  auth_callout {\n"), "%q", rest)
	require.Contains(t, rest, `    account: "ENVOY"`+"\n")
	require.Contains(t, rest, `    auth_users: ["`+authServiceKey+`", "`+jetStreamOperatorKey+`"]`+"\n")
	require.True(t, strings.HasSuffix(text, calloutPingLines+privateProjectsTagLines), "%q", text)

	lines := privateWithOperator().CalloutLines()
	require.True(t, strings.HasPrefix(lines, accounts), "CalloutLines leaves out the accounts block, so a roll keyed on it would not follow it")

	alone, err := privateOnly().Render(header)
	require.NoError(t, err)
	requireSameText(t, golden(t, "authorization-callout-private.golden"), alone)
	jetstream, _, _ = strings.Cut(alone, "  "+core.ClientAccount+" {\n")
	require.NotContains(t, jetstream, "users", "a JETSTREAM account with no operator holds a user")
}

// TestValidateRefusesAJetStreamOperatorItCannotPlace: each refusal is a core.InputError naming
// jetStreamOperator: the input without privateProjects, whose render has no JETSTREAM account; one
// naming no user, or a pending one; one naming the auth service's user, which must sit where the
// callout runs; and one holding a seed, which is named by its path and never quoted. A second
// static user beside the operator is still refused, as privateProjects refuses any client user.
func TestValidateRefusesAJetStreamOperatorItCannotPlace(t *testing.T) {
	seed := testSeed(t)
	for _, row := range []struct {
		name  string
		edit  func(a *core.Authorization)
		input string
		want  string
	}{
		{"without privateProjects", func(a *core.Authorization) { a.PrivateProjects = false }, "jetStreamOperator",
			`jetStreamOperator "example-jetstream": only a render with privateProjects has the JETSTREAM account it names a user of`},
		{"naming no user", func(a *core.Authorization) { a.JetStreamOperator = "example-nobody" }, "jetStreamOperator",
			`jetStreamOperator "example-nobody" names no keyed user`},
		{"naming a pending user", func(a *core.Authorization) {
			a.Users = append(a.Users, core.User{Name: "example-pending", HasSeed: true})
			a.JetStreamOperator = "example-pending"
		}, "jetStreamOperator", `jetStreamOperator "example-pending" names no keyed user`},
		{"naming the auth service", func(a *core.Authorization) { a.JetStreamOperator = "example-auth" }, "jetStreamOperator",
			`jetStreamOperator "example-auth" is callout.serviceUser`},
		{"holding a seed", func(a *core.Authorization) { a.JetStreamOperator = seed }, "",
			"jetStreamOperator holds an nkey seed"},
		{"beside another static user", func(a *core.Authorization) {
			other := jetStreamOperator()
			other.Name, other.NKey = "example-dispatch", "UAUK2YFQ7SM5YNV4VH6NPEKN3UZVY43S4JUE5767FR4CX76R7E7JTWYJ"
			a.Users = append(a.Users, other)
		}, "privateProjects", `privateProjects: users[2] "example-dispatch": a stored client seed while a project is private`},
	} {
		t.Run(row.name, func(t *testing.T) {
			a := privateWithOperator()
			row.edit(&a)
			err := a.Validate()
			require.ErrorContains(t, err, row.want)
			require.NotContains(t, err.Error(), seed, "the refusal quotes the seed")
			if row.input != "" {
				var input *core.InputError
				require.True(t, errors.As(err, &input), "%v", err)
				require.Equal(t, row.input, input.Input)
			}
		})
	}
	require.NoError(t, privateWithOperator().Validate())
	require.True(t, slices.ContainsFunc(privateWithOperator().Users, func(u core.User) bool { return u.Name == "example-jetstream" }))
}
