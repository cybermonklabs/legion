// Package accounthub runs, in this process, a nats-server on one of the layouts a hub renders
// around the JetStream account move, with a stand-in auth service, for jetstream-account-move's
// rehearsal and the account probe's test. The layouts are core's own render, read from the goldens
// packages/envoy/infra/core pins against its Render (core's TestAPrivateHubCarriesTheServerTag and
// TestAPrivateRenderPutsJetStreamInAnAccountOfItsOwn), with the goldens' public keys replaced by
// keys this package holds the seeds of: this module cannot import that one, and the goldens are its
// render byte for byte.
package accounthub

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	"github.com/sjawhar/envoy/natscred"
	"github.com/sjawhar/envoy/natsgrant"
)

// Layout is one of the renders the move goes through.
type Layout int

const (
	// MergeBase is the merge base's private render: every user in $G, JetStream in $G
	// (authorization-callout-private-before.golden). The rollback renders it again. Beside the
	// auth service's user it holds, as a static user of $G with every subject, the operator of the
	// account that holds JetStream there (Keys.Operator), written as core writes a static user and
	// listed in auth_users: the account probe's remover on this layout.
	MergeBase Layout = iota
	// Accounts is the private render with JetStream in an account of its own and the move's
	// operator in it (authorization-accounts.golden).
	Accounts
	// AccountsWithoutAPIImport is Accounts with ENVOY's import of $JS.API.> removed: a layout the
	// account probe must read as UNKNOWN.
	AccountsWithoutAPIImport
)

// The goldens' public keys, which Config replaces with a hub's own.
const (
	goldenService  = "UD5BKU3BU7COAOUCXHGXWOSLNVLL4TEXCWPJHDUQE4C47ZNOYFOZVKUN"
	goldenOperator = "UDXREYGN6OYHDDYF45MCYWJGYYAPJ7VH6EJBFUQYMLNMI5XCW54ZAIQD"
	goldenIssuer   = "AB2TCXFBYSIN24KFPVJARBN5CHCL4ZCV5SBWSONTZ2WYXS3XTAEK6S3G"
	goldenXKey     = "XBVCTZK3PV5Y7TBIVYTDZBP3O2YGHUUL7MMENN4DTZIREJCPGLTAF6EL"
)

// The accounts of the Accounts layout, as core names them.
const (
	JetStreamAccount = "JETSTREAM"
	ClientAccount    = "ENVOY"
	GlobalAccount    = "$G"
)

// Keys are a hub's keys: the auth service's user, the JETSTREAM operator's, the issuer that signs
// every answer and the curve key requests are sealed to.
type Keys struct {
	ServiceSeed, OperatorSeed, IssuerSeed, XKeySeed string
	Service, Operator, Issuer, XKey                 string
}

// NewKeys makes a hub's keys.
func NewKeys() (Keys, error) {
	var k Keys
	for _, key := range []struct {
		make       func() (nkeys.KeyPair, error)
		seed, used *string
	}{
		{nkeys.CreateUser, &k.ServiceSeed, &k.Service},
		{nkeys.CreateUser, &k.OperatorSeed, &k.Operator},
		{nkeys.CreateAccount, &k.IssuerSeed, &k.Issuer},
		{nkeys.CreateCurveKeys, &k.XKeySeed, &k.XKey},
	} {
		pair, err := key.make()
		if err != nil {
			return Keys{}, err
		}
		seed, err := pair.Seed()
		if err != nil {
			return Keys{}, err
		}
		public, err := pair.PublicKey()
		if err != nil {
			return Keys{}, err
		}
		*key.seed, *key.used = string(seed), public
	}
	return k, nil
}

// Config is the server configuration of layout: the golden's text from testdata, its keys keys',
// after a listen line and JetStream's store in storeDir.
func Config(testdata string, layout Layout, keys Keys, storeDir string) (string, error) {
	name := "authorization-accounts.golden"
	if layout == MergeBase {
		name = "authorization-callout-private-before.golden"
	}
	raw, err := os.ReadFile(filepath.Join(testdata, name))
	if err != nil {
		return "", err
	}
	text := string(raw)
	if layout == MergeBase {
		operator := `    # example-jetstream` + "\n" +
			`    {"nkey": "` + goldenOperator + `", "permissions": {"publish": {"allow": [">"]}, "subscribe": {"allow": [">"]}}}` + "\n"
		users, auth := "  ]\n  auth_callout {\n", `auth_users: ["`+goldenService+`"]`
		if !strings.Contains(text, users) || !strings.Contains(text, auth) {
			return "", errors.New("the merge base's golden holds no users list and auth_users to add the operator to")
		}
		text = strings.Replace(text, users, operator+users, 1)
		text = strings.Replace(text, auth, `auth_users: ["`+goldenService+`", "`+goldenOperator+`"]`, 1)
	}
	for golden, own := range map[string]string{goldenService: keys.Service, goldenOperator: keys.Operator, goldenIssuer: keys.Issuer, goldenXKey: keys.XKey} {
		text = strings.ReplaceAll(text, golden, own)
	}
	if layout == AccountsWithoutAPIImport {
		line := `      {"service": {"account": "JETSTREAM", "subject": "$JS.API.>"}}` + "\n"
		if !strings.Contains(text, line) {
			return "", errors.New("the accounts golden holds no import of $JS.API.>")
		}
		text = strings.Replace(text, line, "", 1)
	}
	return fmt.Sprintf("listen: \"127.0.0.1:-1\"\njetstream: {store_dir: %q}\n%s", storeDir, text), nil
}

// Hub is a running server with its stand-in auth service.
type Hub struct {
	URL    string
	server *server.Server
	auth   *nats.Conn
	place  atomic.Pointer[string]
}

// Start runs the server on layout, its JetStream store in storeDir, and connects the stand-in auth
// service, which admits each token grants names with its grant, placed in place.
func Start(testdata string, layout Layout, keys Keys, storeDir, place string, grants map[string]natsgrant.Permissions) (*Hub, error) {
	config, err := Config(testdata, layout, keys, storeDir)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(filepath.Dir(storeDir), fmt.Sprintf("nats-server-%d.conf", time.Now().UnixNano()))
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		return nil, err
	}
	opts, err := server.ProcessConfigFile(path)
	if err != nil {
		return nil, fmt.Errorf("nats-server refuses the layout: %w", err)
	}
	opts.NoSigs, opts.NoLog = true, true
	s, err := server.NewServer(opts)
	if err != nil {
		return nil, err
	}
	go s.Start()
	if !s.ReadyForConnections(10 * time.Second) {
		s.Shutdown()
		return nil, errors.New("nats-server did not start")
	}
	h := &Hub{URL: s.ClientURL(), server: s}
	h.place.Store(&place)
	if h.auth, err = serveCallout(h, keys, grants); err != nil {
		h.Stop()
		return nil, err
	}
	return h, nil
}

// Place makes the stand-in place every client it admits from now on in account: the broker's
// BROKER_NATS_ACCOUNT moving at the switch.
func (h *Hub) Place(account string) { h.place.Store(&account) }

// Stop shuts the server down and waits for it.
func (h *Hub) Stop() {
	if h.auth != nil {
		h.auth.Close()
	}
	h.server.Shutdown()
	h.server.WaitForShutdown()
}

// Token is the token a client presents: as a token, or as a service's password beside
// natscred.ServiceUser, the shape natscred.Service sends.
func token(request *jwt.AuthorizationRequestClaims) string {
	if request.ConnectOptions.Username == natscred.ServiceUser {
		return request.ConnectOptions.Password
	}
	return request.ConnectOptions.Token
}

// serveCallout connects the stand-in auth service as the rendered service user and answers each
// authorization request as natscallout does: the token's grant, placed in h's account, signed by
// the issuer and sealed to the server's curve key; a token grants does not name is refused.
func serveCallout(h *Hub, keys Keys, grants map[string]natsgrant.Permissions) (*nats.Conn, error) {
	issuer, err := nkeys.FromSeed([]byte(keys.IssuerSeed))
	if err != nil {
		return nil, err
	}
	curve, err := nkeys.FromSeed([]byte(keys.XKeySeed))
	if err != nil {
		return nil, err
	}
	conn, err := ConnectSeed(h.URL, keys.ServiceSeed)
	if err != nil {
		return nil, fmt.Errorf("the auth service's user: %w", err)
	}
	var mu sync.Mutex
	_, err = conn.Subscribe("$SYS.REQ.USER.AUTH", func(m *nats.Msg) {
		mu.Lock()
		defer mu.Unlock()
		opened, err := curve.Open(m.Data, m.Header.Get("Nats-Server-Xkey"))
		if err != nil {
			return
		}
		request, err := jwt.DecodeAuthorizationRequestClaims(string(opened))
		if err != nil {
			return
		}
		response := jwt.NewAuthorizationResponseClaims(request.UserNkey)
		response.Audience = request.Server.ID
		name := token(request)
		if grant, ok := grants[name]; ok {
			user := jwt.NewUserClaims(request.UserNkey)
			user.Audience = *h.place.Load()
			user.Name = name
			user.Pub.Allow.Add(grant.Publish...)
			user.Sub.Allow.Add(grant.Subscribe...)
			user.Pub.Deny.Add(grant.PublishDeny...)
			user.Sub.Deny.Add(grant.SubscribeDeny...)
			if response.Jwt, err = user.Encode(issuer); err != nil {
				return
			}
		} else {
			response.Error = "accounthub: no grant for this token"
		}
		answer, err := response.Encode(issuer)
		if err != nil {
			return
		}
		sealed, err := curve.Seal([]byte(answer), request.Server.XKey)
		if err != nil {
			return
		}
		_ = m.Respond(sealed)
	})
	if err != nil {
		conn.Close()
		return nil, err
	}
	if err := conn.Flush(); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// ConnectSeed connects to url as the nkey user whose seed is seed, without reconnecting.
func ConnectSeed(url, seed string, opts ...nats.Option) (*nats.Conn, error) {
	user, err := nkeys.FromSeed([]byte(seed))
	if err != nil {
		return nil, err
	}
	public, err := user.PublicKey()
	if err != nil {
		return nil, err
	}
	return nats.Connect(url, append([]nats.Option{nats.Nkey(public, user.Sign), nats.NoReconnect(), nats.Timeout(5 * time.Second)}, opts...)...)
}

// ConnectToken connects to url presenting token, without reconnecting.
func ConnectToken(url, token string, opts ...nats.Option) (*nats.Conn, error) {
	return nats.Connect(url, append([]nats.Option{nats.Token(token), nats.NoReconnect(), nats.Timeout(5 * time.Second)}, opts...)...)
}

// Everything is the grant of an operator during the move: every subject.
func Everything() natsgrant.Permissions {
	return natsgrant.Permissions{Publish: []string{">"}, Subscribe: []string{">"}}
}
