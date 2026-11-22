package core_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/nats-io/nkeys"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
)

// The callout goldens' keys: the auth service's user, the account whose seed signs its answers,
// and the curve key its answers are sealed to. Public keys only; their seeds were discarded.
const (
	authServiceKey = "UD5BKU3BU7COAOUCXHGXWOSLNVLL4TEXCWPJHDUQE4C47ZNOYFOZVKUN"
	calloutIssuer  = "AB2TCXFBYSIN24KFPVJARBN5CHCL4ZCV5SBWSONTZ2WYXS3XTAEK6S3G"
	calloutXKey    = "XBVCTZK3PV5Y7TBIVYTDZBP3O2YGHUUL7MMENN4DTZIREJCPGLTAF6EL"
)

func authService() core.User {
	return core.User{
		Name:      "example-auth",
		NKey:      authServiceKey,
		Publish:   &core.Rules{Allow: []string{"$SYS._INBOX.>"}},
		Subscribe: &core.Rules{Allow: []string{"$SYS.REQ.USER.AUTH"}},
		HasSeed:   true,
	}
}

// withCallout is hubShaped with the auth service's user and the callout: every static user of
// today's hub stays, so the hub serves them as before while the service admits every other client.
func withCallout() core.Authorization {
	a := hubShaped()
	a.Users = append(a.Users, authService())
	a.Callout = &core.AuthCallout{Issuer: calloutIssuer, XKey: calloutXKey, ServiceUser: "example-auth"}
	return a
}

// privateOnly is the one authorization R12 lets hold a private project: the auth service's user
// alone, with NATS's own auth-callout grant, so every client connects through the service, and no
// user stands for a client presenting no credential.
func privateOnly() core.Authorization {
	return core.Authorization{
		Users:           []core.User{authService()},
		Callout:         &core.AuthCallout{Issuer: calloutIssuer, XKey: calloutXKey, ServiceUser: "example-auth"},
		PrivateProjects: true,
	}
}

// The callout block sits inside the authorization block after its users: auth_callout naming the
// issuer, every keyed user's key in users' order (the pending user has none, so it is not there),
// and the curve key.
func TestAuthCalloutRendersTheCalloutBlock(t *testing.T) {
	text, err := withCallout().Render(hubHeader)
	require.NoError(t, err)
	requireSameText(t, golden(t, "authorization-callout.golden"), text)
}

// The block sets no authorization timeout, so nats-server's default of 2 s stands: a longer one
// would promise an auth service time that a nats.go client does not wait
// (nats_callout_timeout_live_test.go).
func TestAuthCalloutRendersNoAuthorizationTimeout(t *testing.T) {
	text, err := privateOnly().Render(nil)
	require.NoError(t, err)
	require.Equal(t, "authorization {\n"+
		"  users: [\n"+
		"    # example-auth\n"+
		`    {"nkey": "`+authServiceKey+`", "permissions": {"publish": {"allow": ["$SYS._INBOX.>"]}, "subscribe": {"allow": ["$SYS.REQ.USER.AUTH"]}}}`+"\n"+
		"  ]\n"+
		"  auth_callout {\n"+
		`    issuer: "`+calloutIssuer+`"`+"\n"+
		`    auth_users: ["`+authServiceKey+`"]`+"\n"+
		`    xkey: "`+calloutXKey+`"`+"\n"+
		"  }\n"+
		"}\n"+calloutPingLines+privateProjectsTagLines, text)
}

// renderedCalloutBlock is the callout's lines cut from text, a Render text: what follows the users'
// closing "  ]" line, up to the authorization block's own closing "}" line.
func renderedCalloutBlock(t *testing.T, text string) string {
	t.Helper()
	_, afterUsers, found := strings.Cut(text, "\n  ]\n")
	require.True(t, found, "no users list in %q", text)
	block, _, found := strings.Cut(afterUsers, "\n}\n")
	require.True(t, found, "no closing authorization line in %q", text)
	return block + "\n"
}

// CalloutLines is every line Render writes for the callout, which a component that reloads its
// servers rolls them on: the auth_callout block exactly as Render writes it inside the
// authorization block, then the ping lines and, beside privateProjects, the server tag's lines
// (TestAPrivateHubCarriesTheServerTag), which end the text. It is empty without a callout.
//
// Control: make CalloutLines return the auth_callout block alone; the CutSuffix row fails, and
// gcp's TestClusteredNatsHubRollsThePodsForACalloutChange fails on the digest.
func TestCalloutLinesAreTheRenderedCalloutLines(t *testing.T) {
	text, err := withCallout().Render(hubHeader)
	require.NoError(t, err)
	lines := withCallout().CalloutLines()
	block, found := strings.CutSuffix(lines, calloutPingLines)
	require.True(t, found, "CalloutLines leaves out the ping lines: %q", lines)
	require.True(t, strings.HasPrefix(block, "  auth_callout {\n"), "%q", block)
	require.Equal(t, renderedCalloutBlock(t, text), block)
	require.True(t, strings.HasSuffix(text, calloutPingLines), "%q", text)
	require.Empty(t, hubShaped().CalloutLines())
}

func TestValidateRefusesACalloutThatCannotKeepAProjectApart(t *testing.T) {
	user, err := nkeys.CreateUser()
	require.NoError(t, err)
	userSeed, err := user.Seed()
	require.NoError(t, err)
	userKey, err := user.PublicKey()
	require.NoError(t, err)
	account, err := nkeys.CreateAccount()
	require.NoError(t, err)
	accountSeed, err := account.Seed()
	require.NoError(t, err)
	curve, err := nkeys.CreateCurveKeys()
	require.NoError(t, err)
	curveSeed, err := curve.Seed()
	require.NoError(t, err)

	callout := func(a *core.Authorization) *core.AuthCallout { return a.Callout }
	issuer := func(key string) func(a *core.Authorization) {
		return func(a *core.Authorization) { callout(a).Issuer = key }
	}
	xkey := func(key string) func(a *core.Authorization) {
		return func(a *core.Authorization) { callout(a).XKey = key }
	}
	sessions := func(a *core.Authorization) {
		a.Users = append(a.Users, hubShaped().Users[4])
		a.NoAuthUser = "example-sessions"
	}
	service := func(a *core.Authorization) *core.User { return &a.Users[0] }
	grant := func(publish, subscribe []string) func(a *core.Authorization) {
		return func(a *core.Authorization) {
			service(a).Publish = &core.Rules{Allow: publish}
			service(a).Subscribe = &core.Rules{Allow: subscribe}
		}
	}
	const wider = "is wider than the auth service's grant, which subscribes $SYS.REQ.USER.AUTH and publishes only its replies under $SYS._INBOX."
	// Each row names the input its refusal is a core.InputError for: callout, or privateProjects; or
	// "" for a seed, whose refusal is a plain error and must quote no seed.
	const callouts, private, seedRow = "callout", "privateProjects", ""
	rows := []struct {
		name   string
		base   func() core.Authorization
		change func(a *core.Authorization)
		input  string
		err    string
	}{
		// A seed is refused before anything quotes the field, and never echoed.
		{"an account seed for the issuer", withCallout, issuer(string(accountSeed)), seedRow, "callout.issuer holds an nkey seed (not echoed)"},
		{"a curve seed for the xkey", withCallout, xkey(string(curveSeed)), seedRow, "callout.xkey holds an nkey seed (not echoed)"},
		{"a seed for the service user", withCallout, func(a *core.Authorization) { callout(a).ServiceUser = string(userSeed) }, seedRow, "callout.serviceUser holds an nkey seed (not echoed)"},

		{"no issuer", withCallout, issuer(""), callouts, "callout.issuer is not an account public key"},
		{"a user key for the issuer", withCallout, issuer(userKey), callouts, "callout.issuer is not an account public key"},
		{"a curve key for the issuer", withCallout, issuer(calloutXKey), callouts, "callout.issuer is not an account public key"},
		{"an issuer whose checksum fails", withCallout, issuer(calloutIssuer[:55] + "A"), callouts, "callout.issuer is not an account public key"},
		{"a lower-case issuer", withCallout, issuer(strings.ToLower(calloutIssuer)), callouts, "callout.issuer is not an account public key"},
		{"a user key for the xkey", withCallout, xkey(userKey), callouts, "callout.xkey is not a curve public key"},
		{"an account key for the xkey", withCallout, xkey(calloutIssuer), callouts, "callout.xkey is not a curve public key"},
		{"an xkey of another length", withCallout, xkey(calloutXKey[:55]), callouts, "callout.xkey is not a curve public key"},
		// Without an xkey nats-server sends each client's token to the service unsealed.
		{"no xkey", withCallout, xkey(""), callouts, "callout.xkey is required: without it nats-server sends each connecting client's token to the auth service unsealed, on $SYS.REQ.USER.AUTH"},
		{"no service user", withCallout, func(a *core.Authorization) { callout(a).ServiceUser = "" }, callouts, `callout.serviceUser "" names no keyed user`},
		{"a service user naming nobody", withCallout, func(a *core.Authorization) { callout(a).ServiceUser = "example-nobody" }, callouts, `callout.serviceUser "example-nobody" names no keyed user`},
		{"a pending service user", withCallout, func(a *core.Authorization) { callout(a).ServiceUser = "example-pending" }, callouts, `callout.serviceUser "example-pending" names no keyed user`},
		// A client presenting no credential would hold the service's grant and answer every
		// connection's authorization itself.
		{"the session user as the service user", withCallout, func(a *core.Authorization) { callout(a).ServiceUser = "example-sessions" }, callouts,
			`callout.serviceUser "example-sessions" is noAuthUser: a client presenting no credential would become the auth service`},

		{"private projects without the callout", privateOnly, func(a *core.Authorization) { a.Callout = nil }, private,
			"privateProjects: a project can be private only while the server admits each client through the auth service, and the authorization has no callout"},
		{"private projects beside a session user", privateOnly, sessions, private,
			`privateProjects: users[1] "example-sessions" is noAuthUser: a client presenting no credential would become it while a project is private`},
		// The service user is admitted by its seed alone, so a grant wider than NATS's own for an
		// auth service makes it a client that connects past the service.
		{"private projects with a service user publishing everywhere", privateOnly, grant([]string{">"}, []string{"$SYS.REQ.USER.AUTH"}), private,
			`privateProjects: callout.serviceUser "example-auth": publish.allow[0] ">" ` + wider},
		{"private projects with a service user publishing beyond its replies", privateOnly, grant([]string{"$SYS._INBOX.>", "notifications.>"}, []string{"$SYS.REQ.USER.AUTH"}), private,
			`privateProjects: callout.serviceUser "example-auth": publish.allow[1] "notifications.>" ` + wider},
		{"private projects with a service user publishing to a look-alike inbox", privateOnly, grant([]string{"$SYS._INBOX_.>"}, []string{"$SYS.REQ.USER.AUTH"}), private,
			`privateProjects: callout.serviceUser "example-auth": publish.allow[0] "$SYS._INBOX_.>" ` + wider},
		{"private projects with a service user subscribing to $SYS", privateOnly, grant([]string{"$SYS._INBOX.>"}, []string{"$SYS.>"}), private,
			`privateProjects: callout.serviceUser "example-auth": subscribe.allow[0] "$SYS.>" ` + wider},
		{"private projects with a service user subscribing to its inbox too", privateOnly, grant([]string{"$SYS._INBOX.>"}, []string{"$SYS.REQ.USER.AUTH", "_INBOX.>"}), private,
			`privateProjects: callout.serviceUser "example-auth": subscribe.allow[1] "_INBOX.>" ` + wider},
		{"private projects beside a client user with a seed", privateOnly, func(a *core.Authorization) { a.Users = append(a.Users, hubShaped().Users[1]) }, private,
			`privateProjects: users[1] "example-dispatch": a stored client seed while a project is private`},
		{"private projects beside a pending client user", privateOnly, func(a *core.Authorization) { a.Users = append(a.Users, hubShaped().Users[3]) }, private,
			`privateProjects: users[1] "example-pending": a stored client seed while a project is private`},
		{"private projects beside a keyed client user without a seed", privateOnly, func(a *core.Authorization) {
			u := hubShaped().Users[1]
			u.HasSeed = false
			a.Users = append(a.Users, u)
		}, private, `privateProjects: users[1] "example-dispatch": a static client user while a project is private`},
		{"private projects beside a client user named before the service's", privateOnly, func(a *core.Authorization) {
			a.Users = append([]core.User{hubShaped().Users[0]}, a.Users...)
		}, private, `privateProjects: users[0] "example-hub": a stored client seed while a project is private`},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			a := r.base()
			require.NoError(t, a.Validate(), "the row's base must be accepted for its refusal to mean anything")
			r.change(&a)
			require.ErrorContains(t, a.Validate(), r.err)
			// A component that carries the callout or privateProjects under its own input name finds
			// which was refused with errors.As; a seed's refusal names no input.
			var input *core.InputError
			if r.input == seedRow {
				require.False(t, errors.As(a.Validate(), &input), "a seed's refusal is a plain error: %v", a.Validate())
			} else {
				requireInputError(t, a.Validate(), r.input, r.err)
			}
			_, err := a.Render(hubHeader)
			require.ErrorContains(t, err, r.err, "Render renders only what Validate accepts")
			_, err = a.StartScript(hubHeader, nil, "/tmp/nats-server.conf")
			requireInputError(t, err, "authorization", r.err)
			require.EqualError(t, err, "authorization: "+a.Validate().Error(), "StartScript renders only what Validate accepts")
			if r.input == seedRow {
				for _, seed := range [][]byte{userSeed, accountSeed, curveSeed} {
					require.NotContains(t, err.Error(), string(seed))
				}
			}
		})
	}
}

func TestValidateAcceptsACallout(t *testing.T) {
	rows := []struct {
		name string
		a    func() core.Authorization
	}{
		{"today's users beside the callout", withCallout},
		{"private projects with the service user alone", privateOnly},
		{"private projects with the service user in a queue group", func() core.Authorization {
			a := privateOnly()
			a.Users[0].Subscribe.Allow = []string{"$SYS.REQ.USER.AUTH auth-service"}
			return a
		}},
		{"private projects with the service user's replies narrowed", func() core.Authorization {
			a := privateOnly()
			a.Users[0].Publish = &core.Rules{Allow: []string{"$SYS._INBOX.*.*"}, Deny: []string{"$SYS._INBOX.x.>"}}
			return a
		}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			require.NoError(t, r.a().Validate())
		})
	}
}
