package main

import (
	"bytes"
	"context"
	"crypto"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/broker/natscallout"
	"github.com/sjawhar/envoy/internal/broker/oauthwire"
	"github.com/sjawhar/envoy/internal/brokerclient"
	"github.com/sjawhar/envoy/internal/brokertoken"
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
			ClientID: brokertoken.ExchangeClientID, ProjectScope: brokertoken.ProjectScopePerson,
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

func serviceToken(_ context.Context, raw string) (string, bool, time.Time, time.Time, error) {
	parts := strings.Split(raw, ".")
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false, time.Time{}, time.Time{}, err
	}
	var claims struct {
		Sub      string
		Iat, Exp int64
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", false, time.Time{}, time.Time{}, err
	}
	return claims.Sub, false, time.Unix(claims.Iat, 0), time.Unix(claims.Exp, 0), nil
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
	services, err := natscallout.ParseServices("daemon/-/*=" + daemonAccount)
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
		brokerclient.BrokerURLVar:        stub.server.URL,
		brokerclient.IdentityVar:         brokerclient.IdentityKubernetes,
		brokerclient.MachineTokenFileVar: write("pod-token", "pod-token"),
		brokerclient.SessionVar:          "ses_a",
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
// refused nothing. The control is the probe printing brokerclient's error without that hint.
func TestNatsprobeNamesTheCredentialsAHubWithNoAuthServiceTakes(t *testing.T) {
	_, url := testnats.Start(t)
	stub := newBroker(t)
	pod := filepath.Join(t.TempDir(), "pod-token")
	require.NoError(t, os.WriteFile(pod, []byte("pod-token"), 0o600))
	environment := map[string]string{
		brokerclient.BrokerURLVar:        stub.server.URL,
		brokerclient.IdentityVar:         brokerclient.IdentityKubernetes,
		brokerclient.MachineTokenFileVar: pod,
		brokerclient.SessionVar:          "ses_a",
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
