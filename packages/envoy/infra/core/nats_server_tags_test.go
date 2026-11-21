package core_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/jwt/v2"
	natsserver "github.com/nats-io/nats-server/v2/server"
	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/natsgrant"
)

// privateProjectsTagLines are the two lines core renders after the callout's ping lines with
// privateProjects: the server tag the auth service reads off each authorization request as the
// proof that the hub was rendered private.
const privateProjectsTagLines = "# this hub is rendered with privateProjects: the auth service admits Dispatch's hub-admission proof only on a request that carries this tag, and there admits a listener only in required privacy mode\n" +
	`server_tags: ["envoy-private-projects"]` + "\n"

// TestAPrivateHubCarriesTheServerTag: with privateProjects the text ends with the server tag's two
// lines, after the ping lines (authorization-callout-private.golden), and CalloutLines ends with
// them, so a hub that rolls its servers on a change of the callout's lines rolls them when the tag
// comes or goes. Without privateProjects no line holds server_tags, and the text is what it was
// before the tag existed.
//
// Control: make calloutParts leave the two lines out; the golden row fails at the golden's last
// two lines.
func TestAPrivateHubCarriesTheServerTag(t *testing.T) {
	require.Equal(t, natsgrant.PrivateProjectsServerTag, "envoy-private-projects",
		"the golden spells the tag natsgrant names")

	private, err := privateOnly().Render(header)
	require.NoError(t, err)
	requireSameText(t, golden(t, "authorization-callout-private.golden"), private)
	require.True(t, strings.HasSuffix(private, calloutPingLines+privateProjectsTagLines), "%q", private)
	require.True(t, strings.HasSuffix(privateOnly().CalloutLines(), calloutPingLines+privateProjectsTagLines),
		"CalloutLines leaves out the server tag, so a roll keyed on it would not follow the tag: %q", privateOnly().CalloutLines())

	public := privateOnly()
	public.PrivateProjects = false
	for name, a := range map[string]core.Authorization{
		"the private hub's users without privateProjects": public,
		"a hub with a callout and its static users":       withCallout(),
		"a hub with no callout":                           hubShaped(),
	} {
		t.Run(name, func(t *testing.T) {
			text, err := a.Render(header)
			require.NoError(t, err)
			require.NotContains(t, text, "server_tags", "a hub rendered without privateProjects carries the server tag")
			require.NotContains(t, a.CalloutLines(), "server_tags")
		})
	}
	// Without privateProjects the text is the merge base's private render less the tag's lines: the
	// tag, and since then the accounts block (TestAnAllPublicHubRendersAsBefore), come only with it.
	withoutTag, err := public.Render(header)
	require.NoError(t, err)
	requireSameText(t, strings.TrimSuffix(golden(t, "authorization-callout-private-before.golden"), privateProjectsTagLines), withoutTag)
}

// TestTheServerTagReachesTheAuthorizationRequest: nats-server v2.15.1, started in this process on
// the rendered text, copies the server tag into the authorization request it sends the auth
// service (server/auth_callout.go, claim.Server.Tags), so the auth service can read from the
// request alone whether the hub was rendered private. A stub auth service, the rendered service
// user, opens each sealed request and refuses the client.
//
// Control: the same server rendered without privateProjects; its requests carry no tag, and the
// row fails if it sees one.
func TestTheServerTagReachesTheAuthorizationRequest(t *testing.T) {
	for _, row := range []struct {
		name    string
		private bool
	}{
		{"rendered with privateProjects", true},
		{"rendered without privateProjects", false},
	} {
		t.Run(row.name, func(t *testing.T) {
			tags := authorizationRequestTags(t, row.private)
			if row.private {
				require.Equal(t, jwt.TagList{natsgrant.PrivateProjectsServerTag}, tags,
					"the authorization request of a hub rendered private does not carry the server tag")
				return
			}
			require.False(t, slices.Contains(tags, natsgrant.PrivateProjectsServerTag),
				"a hub rendered without privateProjects sent the server tag: %v", tags)
			require.Empty(t, tags)
		})
	}
}

// authorizationRequestTags starts nats-server in this process on the rendered text of an
// authorization whose only user is the auth service's, with privateProjects as private, connects
// a stub auth service as that user, connects one client with a token, and returns the server
// tags of the authorization request the server sent the service for it.
func authorizationRequestTags(t *testing.T, private bool) jwt.TagList {
	t.Helper()
	serviceSeed, serviceKey := testnats.User(t)
	issuer, err := nkeys.CreateAccount()
	require.NoError(t, err)
	issuerKey, err := issuer.PublicKey()
	require.NoError(t, err)
	curve, err := nkeys.CreateCurveKeys()
	require.NoError(t, err)
	curveKey, err := curve.PublicKey()
	require.NoError(t, err)
	a := core.Authorization{
		Users: []core.User{{
			Name:      "example-auth",
			NKey:      serviceKey,
			Publish:   &core.Rules{Allow: []string{"$SYS._INBOX.>"}},
			Subscribe: &core.Rules{Allow: []string{"$SYS.REQ.USER.AUTH"}},
			HasSeed:   true,
		}},
		Callout:         &core.AuthCallout{Issuer: issuerKey, XKey: curveKey, ServiceUser: "example-auth"},
		PrivateProjects: private,
	}
	text, err := a.Render(header)
	require.NoError(t, err)
	configPath := filepath.Join(t.TempDir(), "nats-server.conf")
	require.NoError(t, os.WriteFile(configPath, []byte(text), 0o600))
	opts, err := natsserver.ProcessConfigFile(configPath)
	require.NoError(t, err)
	opts.NoSigs, opts.Host, opts.Port = true, "127.0.0.1", natsserver.RANDOM_PORT
	server, err := natsserver.NewServer(opts)
	require.NoError(t, err)
	go server.Start()
	t.Cleanup(server.Shutdown)
	require.True(t, server.ReadyForConnections(10*time.Second), "nats-server did not start")

	service, err := testnats.ConnectSeed(t, server.ClientURL(), serviceSeed)
	require.NoError(t, err, "the auth service's user, in auth_users, connects without a callout")
	t.Cleanup(service.Close)
	requests := make(chan *jwt.AuthorizationRequestClaims, 1)
	_, err = service.Subscribe("$SYS.REQ.USER.AUTH", func(m *natsgo.Msg) {
		opened, err := curve.Open(m.Data, m.Header.Get("Nats-Server-Xkey"))
		if err != nil {
			t.Errorf("open the sealed authorization request: %v", err)
			return
		}
		claims, err := jwt.DecodeAuthorizationRequestClaims(string(opened))
		if err != nil {
			t.Errorf("decode the authorization request: %v", err)
			return
		}
		select {
		case requests <- claims:
		default:
		}
		response := jwt.NewAuthorizationResponseClaims(claims.UserNkey)
		response.Audience = claims.Server.ID
		response.Error = "example: this stub admits no client"
		token, err := response.Encode(issuer)
		if err != nil {
			t.Errorf("sign the refusal: %v", err)
			return
		}
		sealed, err := curve.Seal([]byte(token), claims.Server.XKey)
		if err != nil {
			t.Errorf("seal the refusal: %v", err)
			return
		}
		if err := m.Respond(sealed); err != nil {
			t.Errorf("answer the authorization request: %v", err)
		}
	})
	require.NoError(t, err)
	require.NoError(t, service.Flush())

	client, err := natsgo.Connect(server.ClientURL(), natsgo.Token("example-client-token"), natsgo.NoReconnect(), natsgo.Timeout(5*time.Second))
	if err == nil {
		client.Close()
	}
	require.Error(t, err, "the stub refuses every client")
	select {
	case claims := <-requests:
		return claims.Server.Tags
	case <-time.After(10 * time.Second):
		t.Fatal("the server sent the auth service no authorization request for the client")
		return nil
	}
}
