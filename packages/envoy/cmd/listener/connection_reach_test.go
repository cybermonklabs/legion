//go:build natslive

package main

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"encoding/base64"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cipher/natscallout"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// TestEachConnectionReceivesOnlyItsTokensProjects is the listener's half of the check that each
// connection reaches only its token's projects; internal/integration's test of this name is the
// auth service's half. It runs on testnats.HubImage, the nats-server image the hub deploys, behind
// the real auth service (natscallout.Start), whose broker answer for the session's token is the
// test's.
//
// A session whose token lists only PRIV, admitted by the auth service with its own grant
// (natsgrant.Session), registers an interest that names acme/pub, a repository only PUB's issues
// resolve to, and receives that repository's GitHub event through the listener's relay: a GitHub
// event is a message of no project, and the listener checks no project on it. The test stores an
// event of acme/priv first, which the interest does not name, and the session receives none of
// it. The listener's own connections are a static user of the hub that holds every subject: this
// row tests what the session receives through its interest, not the listener's grant. Two
// controls break the interest check (sessionConsumerFilter), each on its own: the relay
// delivering every GitHub subject without it, under which the test fails naming acme/priv's
// subject; and the relay dropping the named repository's events, under which it fails naming
// acme/pub's.
//
// The rows that need the listener's project checks, which are not on main, join this test with
// them: the listener in required mode with a privacy stand-in, and its all-public control; and,
// called with a token that lists only PUB, the listener refuses PRIV's interest, role claim,
// session consumer's resume and send, and serves each of PUB's.
func TestEachConnectionReceivesOnlyItsTokensProjects(t *testing.T) {
	keys := testnats.NewCalloutKeys(t)
	listenerSeed, listenerPublic := testnats.User(t)
	url := testnats.StartImageWithCallout(t, keys, testnats.StaticUser{NKey: listenerPublic, Publish: []string{">"}, Subscribe: []string{">"}})
	session := newReachSession(t, "ses_priv", "PRIV")
	startReachService(t, url, keys, session)
	// Every connection the listener's code opens reads its user from the process environment.
	t.Setenv(bus.NkeySeedEnvVar, listenerSeed)
	rig := newRelayRigUnownedOn(t, url, relayPauseFirst, relayPauseMax)
	rig.relays.setOwned(true)

	t.Run("a session whose token lists only PRIV receives acme/pub's GitHub event through its interest and none of acme/priv's", func(t *testing.T) {
		pubEvent := contracts.GithubSubject("acme", "pub", "pull_request")
		privEvent := contracts.GithubSubject("acme", "priv", "pull_request")
		standIn := startSessionStandInOn(t, connectReachSession(t, url, session), session.id, answerDone)
		rig.subscribe(t, consumerRegistration(session.id, "epoch-1", contracts.GithubRepositoryPrefix("acme", "pub")+".>"))
		// acme/priv's event is stored first, so it would be the first frame were it delivered.
		rig.storeEvent(t, privEvent)
		want, _ := rig.storeEvent(t, pubEvent)
		select {
		case frame := <-standIn.frames:
			if got := frame.msg.Header.Get(relayHeaderSubject); got != pubEvent || frame.item.EventID != want.EventID {
				t.Fatalf("the PRIV-only session received the GitHub event on %s, which its interest in %s.> does not name", got, contracts.GithubRepositoryPrefix("acme", "pub"))
			}
		case <-time.After(15 * time.Second):
			t.Fatalf("the PRIV-only session received no GitHub event on %s, which its interest names, within 15s", pubEvent)
		}
		select {
		case frame := <-standIn.frames:
			t.Fatalf("the PRIV-only session received the GitHub event on %s, which its interest does not name", frame.msg.Header.Get(relayHeaderSubject))
		case <-time.After(2 * time.Second):
		}
	})
}

// reachSession is one session's key, the broker access token bound to it, and the broker's answer
// about that token, which names the session's one project.
type reachSession struct {
	id     string
	key    *ecdsa.PrivateKey
	token  string
	answer oauthwire.Introspection
}

func newReachSession(t *testing.T, id, project string) *reachSession {
	t.Helper()
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	sum, err := (&jose.JSONWebKey{Key: key.Public()}).Thumbprint(crypto.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	return &reachSession{
		id: id, key: key, token: "access-token-of-" + id,
		answer: oauthwire.Introspection{
			Active: true, Session: id, Person: "person@example.com", ClientID: ciphertoken.ExchangeClientID,
			ProjectScope: ciphertoken.ProjectScopePerson, Projects: []string{project},
			Thumbprint: base64.RawURLEncoding.EncodeToString(sum), ExpiresAt: time.Now().Add(15 * time.Minute).Unix(),
		},
	}
}

// startReachService runs the auth service against the hub at url with keys' seeds, answering s's
// token as the broker answers it and every other token as not live, stopped when the test ends,
// and returns once its subscription has reached the hub.
func startReachService(t *testing.T, url string, keys testnats.CalloutKeys, s *reachSession) {
	t.Helper()
	dir := t.TempDir()
	write := func(name, seed string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(seed+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	service, err := natscallout.Start(t.Context(), natscallout.Settings{
		URLs:     []string{url},
		SeedFile: write("service.seed", keys.ServiceSeed), IssuerSeedFile: write("issuer.seed", keys.IssuerSeed),
		XKeySeedFile: write("xkey.seed", keys.XKeySeed),
	}, natscallout.Deps{
		Introspect: func(_ context.Context, token string) (oauthwire.Introspection, error) {
			if token == s.token {
				return s.answer, nil
			}
			return oauthwire.Introspection{}, nil
		},
		Log: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := service.Ready(ctx); err != nil {
		t.Fatalf("the auth service's subscription never reached the hub: %v", err)
	}
}

// connectReachSession connects to url as s does: its access token in the CONNECT's token field
// and, as its signature, the proof of its key over the nonce the server sends this connection.
func connectReachSession(t *testing.T, url string, s *reachSession) *natsgo.Conn {
	t.Helper()
	sign := func(nonce []byte) ([]byte, error) {
		signed, err := cipherclient.ConnectionProof(s.key, s.token, string(nonce))
		return []byte(signed), err
	}
	conn, err := natsgo.Connect(url, natsgo.UserJWT(func() (string, error) { return "-", nil }, sign), natsgo.Token(s.token),
		natsgo.Timeout(5*time.Second), natsgo.NoReconnect())
	if err != nil {
		t.Fatalf("the hub did not admit session %s: %v", s.id, err)
	}
	return conn
}
