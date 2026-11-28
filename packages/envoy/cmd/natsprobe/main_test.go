package main

import (
	"bytes"
	"context"
	"crypto"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/cipher/natscallout"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testport"
)

const daemonAccount = "system:serviceaccount:example-ns:example-daemon"

// broker is a stub token endpoint that mints tok-N for each trade, bound to the key the trade's
// DPoP proof embeds, for the session the trade names: the binding the auth service's Introspect
// reads back.
type broker struct {
	mu     sync.Mutex
	bound  map[string]oauthwire.Introspection
	server *httptest.Server
}

func newBroker(t *testing.T) *broker {
	t.Helper()
	b := &broker{bound: map[string]oauthwire.Introspection{}}
	b.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header, _, _ := strings.Cut(r.Header.Get("DPoP"), ".")
		raw, err := base64.RawURLEncoding.DecodeString(header)
		if err != nil {
			http.Error(w, "no proof", http.StatusBadRequest)
			return
		}
		var fields struct {
			JWK jose.JSONWebKey `json:"jwk"`
		}
		if err := json.Unmarshal(raw, &fields); err != nil {
			http.Error(w, "no key", http.StatusBadRequest)
			return
		}
		sum, err := fields.JWK.Thumbprint(crypto.SHA256)
		if err != nil {
			http.Error(w, "no thumbprint", http.StatusBadRequest)
			return
		}
		require.NoError(t, r.ParseForm())
		b.mu.Lock()
		token := fmt.Sprintf("tok-%d", len(b.bound)+1)
		b.bound[token] = oauthwire.Introspection{
			Active: true, Session: r.PostForm.Get(oauthwire.FormSession), Person: "person@example.com",
			ClientID: ciphertoken.ExchangeClientID, ProjectScope: ciphertoken.ProjectScopePerson,
			Thumbprint: base64.RawURLEncoding.EncodeToString(sum), ExpiresAt: time.Now().Add(15 * time.Minute).Unix(),
		}
		b.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": token, "token_type": "DPoP", "expires_in": 900})
	}))
	t.Cleanup(b.server.Close)
	return b
}

func (b *broker) introspect(_ context.Context, token string) (oauthwire.Introspection, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.bound[token], nil
}

// platformToken is an unsigned JWT naming account, which the stub ServiceToken reads unverified.
func platformToken(account string, life time.Duration) string {
	encode := func(v any) string {
		raw, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	now := time.Now()
	return encode(map[string]string{"alg": "none"}) + "." +
		encode(map[string]any{"sub": account, "iat": now.Unix(), "exp": now.Add(life).Unix()}) + ".unsigned"
}

// serviceToken is the stub ServiceToken: the account, issue and expiry raw names, unverified, from
// the one issuer of a bare BROKER_K8S_OIDC_ISSUER URL, whose name is "".
func serviceToken(_ context.Context, raw string) (natscallout.ServiceIdentity, error) {
	parts := strings.Split(raw, ".")
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return natscallout.ServiceIdentity{}, err
	}
	var claims struct {
		Sub      string
		Iat, Exp int64
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return natscallout.ServiceIdentity{}, err
	}
	return natscallout.ServiceIdentity{Account: claims.Sub, Issued: time.Unix(claims.Iat, 0), Expiry: time.Unix(claims.Exp, 0)}, nil
}

// The probe says what the hub answered: a session's own channel served, another session's refused
// in the server's own words, a connection with no credential not admitted, and a service's grant
// served.
func TestNatsprobeSaysWhatTheHubAnswered(t *testing.T) {
	keys := testnats.NewCalloutKeys(t)
	url := testnats.StartWithCallout(t, keys)
	stub := newBroker(t)
	dir := t.TempDir()
	write := func(name, text string) string {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(text), 0o600))
		return path
	}
	services, err := config.ParseNATSServices("daemon/-/*="+daemonAccount, []oidc.NamedIssuer{{URL: "https://cluster.example"}})
	require.NoError(t, err)
	service, err := natscallout.Start(t.Context(), natscallout.Settings{
		URLs: []string{url}, SeedFile: write("service.seed", keys.ServiceSeed),
		IssuerSeedFile: write("issuer.seed", keys.IssuerSeed), XKeySeedFile: write("xkey.seed", keys.XKeySeed), Services: services,
	}, natscallout.Deps{Introspect: stub.introspect, ServiceToken: serviceToken, Log: slog.New(slog.DiscardHandler)})
	require.NoError(t, err)
	t.Cleanup(service.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	require.NoError(t, service.Ready(ctx))

	environment := map[string]string{
		cipherclient.BrokerURLVar:        stub.server.URL,
		cipherclient.IdentityVar:         cipherclient.IdentityKubernetes,
		cipherclient.MachineTokenFileVar: write("pod-token", "pod-token"),
		cipherclient.SessionVar:          "ses_a",
	}
	getenv := func(name string) string { return environment[name] }
	serviceFile := write("service-token", platformToken(daemonAccount, time.Hour))
	for _, row := range []struct {
		name   string
		args   []string
		exit   int
		output []string
	}{
		{"a session's own channel", []string{"--subscribe", "notifications.envoy.deliver.ses_a"}, exitServed,
			[]string{"connected as session/ses_a", "served notifications.envoy.deliver.ses_a"}},
		{"another session's channel", []string{"--subscribe", "notifications.envoy.deliver.ses_b"}, exitRefused,
			[]string{"connected as session/ses_a", `refused: Permissions Violation for Subscription to "notifications.envoy.deliver.ses_b"`}},
		{"no credential", []string{"--no-credential", "--subscribe", "notifications.envoy.deliver.ses_a"}, exitNotAdmitted,
			[]string{"not admitted: Authorization Violation"}},
		{"a service's grant", []string{"--service-token-file", serviceFile, "--subscribe", "_INBOX.probe"}, exitServed,
			[]string{"connected as service " + daemonAccount, "served _INBOX.probe"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			exit := run(append([]string{"--servers", url, "--wait", "1s"}, row.args...), getenv, &stdout, &stderr)
			require.Equal(t, row.exit, exit, "stdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
			for _, line := range row.output {
				require.Contains(t, stdout.String(), line)
			}
			require.NotContains(t, stdout.String()+stderr.String(), "tok-")
		})
	}
}

// A hub that sends no nonce, as this plain one does, gives the probe nothing to sign a session's
// proof over: it says so, names the credentials such a hub does take, and exits 1, since the hub
// refused nothing. The control is the probe printing cipherclient's error without that hint.
func TestNatsprobeNamesTheCredentialsAHubWithNoAuthServiceTakes(t *testing.T) {
	_, url := testnats.Start(t)
	stub := newBroker(t)
	pod := filepath.Join(t.TempDir(), "pod-token")
	require.NoError(t, os.WriteFile(pod, []byte("pod-token"), 0o600))
	environment := map[string]string{
		cipherclient.BrokerURLVar:        stub.server.URL,
		cipherclient.IdentityVar:         cipherclient.IdentityKubernetes,
		cipherclient.MachineTokenFileVar: pod,
		cipherclient.SessionVar:          "ses_a",
	}
	var stdout, stderr bytes.Buffer
	exit := run([]string{"--servers", url, "--wait", "1s", "--subscribe", "notifications.envoy.deliver.ses_a"},
		func(name string) string { return environment[name] }, &stdout, &stderr)
	require.Equal(t, exitFailed, exit, "stdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	require.Empty(t, stdout.String(), "the probe reported an answer of a hub it never sent a CONNECT")
	require.Contains(t, stderr.String(), "the NATS server sent no nonce to sign, so it runs no auth service and takes no session's broker token")
	require.Contains(t, stderr.String(), "name the credential it does take: --no-credential, --service-token-file or --service-google-audience")
	require.NotContains(t, stderr.String(), "tok-")
}

// The probe says "not admitted", exit 4, only for the hub's own refusal of the connection
// (nats.ErrAuthorization). A connection that fails any other way, here a hub that is not
// listening, is no answer of the hub's: it exits 1, prints nothing on stdout, and says on stderr
// why it could not connect. The control is the TestNatsprobeSaysWhatTheHubAnswered row of a
// connection with no credential, which the hub refuses and the probe reports exit 4.
func TestAnUnreachableHubIsNoRefusal(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exit := run([]string{"--servers", "nats://" + testport.Refused(t), "--wait", "1s", "--no-credential",
		"--subscribe", "notifications.envoy.deliver.ses_a"}, func(string) string { return "" }, &stdout, &stderr)
	require.Equal(t, exitFailed, exit, "stdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	require.Empty(t, stdout.String(), "the probe reported an answer of a hub it never reached")
	require.Contains(t, stderr.String(), "natsprobe: connect: ")
}

// A made-up password holding a comma and then nats://, split as --servers is split, leaves its
// head a URL whose port is the password's digits and its tail a URL that names a NATS scheme,
// which the rules let through (a later URL that names one is one a person writes). nats.go dials
// the head, and net's error quotes the port (`dial tcp: address <digits>: invalid port`); the
// probe prints the connection's failure by its class and how many servers it tried, never the
// error's text. nats.go shuffles the servers and returns the last one's error, a refused dial's
// as ErrNoServers, so the tail's port is one net cannot read either (`address 99999: invalid
// port`), and the probe runs ten times: the control, the probe printing nats.Connect's error
// whole, prints the digits in all but about one run in a thousand. The wording is checked without
// stopping the run, so the control reaches the run that prints them.
func TestAnUnreachableHubsErrorPrintsNoPieceOfASplitCredential(t *testing.T) {
	const digits = "8472915603918274"
	// The URL is built here, so no source line holds a URL with a password. Its user is an IP
	// address, so the head's host is one nats.go dials without a lookup.
	split := "nats://127.0.0.1:" + digits + ",nats://madeUpProbeTail@" + "127.0.0.1:99999"
	for range 10 {
		var stdout, stderr bytes.Buffer
		exit := run([]string{"--servers", split, "--wait", "1s", "--no-credential",
			"--subscribe", "notifications.envoy.deliver.ses_a"}, func(string) string { return "" }, &stdout, &stderr)
		printed := stdout.String() + stderr.String()
		for i := 0; i+10 <= len(digits); i++ {
			require.NotContains(t, printed, digits[i:i+10], "the probe prints a run of the made-up password")
		}
		assert.Equal(t, exitFailed, exit, "stdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
		assert.Contains(t, stderr.String(), "natsprobe: connect: the server's address is no host and port the system can dial (tried the 2 servers --servers names)")
	}
}

// A --servers URL nats.go would read other than as written is refused before any dial, naming the
// URL by its position and printing none of it: here a made-up password holding a "/", which
// nats.go's URL parse reads as a port and quotes. The control is the same probe without the check,
// whose `natsprobe: connect:` line quotes the password's head.
func TestTheProbeRefusesAServerURLItCannotReadPrintingNoneOfIt(t *testing.T) {
	const madeUp = "madeUpProbePassword"
	var stdout, stderr bytes.Buffer
	exit := run([]string{"--servers", "nats://127.0.0.1:4222,nats://legion:" + madeUp + "/rest@127.0.0.1:1", "--wait", "1s", "--no-credential",
		"--subscribe", "notifications.envoy.deliver.ses_a"}, func(string) string { return "" }, &stdout, &stderr)
	require.Equal(t, exitUsage, exit, "stdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	require.Empty(t, stdout.String())
	require.Contains(t, stderr.String(), "natsprobe: --servers: NATS server URL 2 of 2 cannot be read")
	require.NotContains(t, stderr.String(), madeUp, "the refusal quotes the password")
}

// A --servers list that begins with an empty entry, then a ws:// URL, then a URL with no scheme
// whose path stops parsing once decoded and whose query holds a made-up token: nats.go drops the
// empty entry, makes the pool ws from the ws:// URL and dials the later URL as ws, whose handshake
// error quotes it, query and all. The probe refuses it by its position in --servers, the empty
// entry counted, before any dial. With the pool read from the first entry, empty or not, the probe
// dialled it and printed the token in most runs.
func TestTheProbeRefusesAWebsocketListsLaterURLAfterAnEmptyEntry(t *testing.T) {
	const madeUp = "madeUpProbeQueryToken0123"
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	var stdout, stderr bytes.Buffer
	exit := run([]string{"--servers", ",ws://127.0.0.1:1," + listener.Addr().String() + "/%25zz?token=" + madeUp, "--wait", "1s", "--no-credential",
		"--subscribe", "notifications.envoy.deliver.ses_a"}, func(string) string { return "" }, &stdout, &stderr)
	require.Equal(t, exitUsage, exit, "stdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	require.Contains(t, stderr.String(), "natsprobe: --servers: NATS server URL 3 of 3 cannot be read: its path does not parse once decoded")
	require.NotContains(t, stderr.String(), madeUp, "the refusal quotes the token")
}
