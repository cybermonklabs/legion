package natscred_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/natscred"
)

// stubBroker answers every trade with a fresh DPoP-bound token, tok-1, tok-2 and so on, the first
// living firstLife and each after it 15 minutes.
func stubBroker(t *testing.T, firstLife time.Duration) (url string, trades *atomic.Int64) {
	t.Helper()
	trades = &atomic.Int64{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != cipherclient.TokenPath {
			http.NotFound(w, r)
			return
		}
		trade := trades.Add(1)
		life := 15 * time.Minute
		if trade == 1 {
			life = firstLife
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": fmt.Sprintf("tok-%d", trade), "token_type": "DPoP",
			"issued_token_type": "urn:ietf:params:oauth:token-type:access_token", "expires_in": int(life.Seconds()),
		})
	}))
	t.Cleanup(server.Close)
	return server.URL, trades
}

func connect(t *testing.T, url string, options ...nats.Option) (*nats.Conn, error) {
	t.Helper()
	nc, err := nats.Connect(url, append([]nats.Option{nats.Timeout(10 * time.Second), nats.NoReconnect()}, options...)...)
	if err == nil {
		t.Cleanup(nc.Close)
	}
	return nc, err
}

// The session credential's proof is the one ciphertoken checks: a session trading at its broker
// connects through the auth service and is admitted to its own channel. One token is taken per
// CONNECT, so a token that rotates between the signature and the token handler is not what the
// connection presents: the proof's 'ath' names the token sent, and the connection is admitted.
// The control reads the token again in its token handler, sends the rotated one beside a proof of
// the first, and is refused for the proof's 'ath'.
func TestConnectionProofVerifiesWithBrokertoken(t *testing.T) {
	key, err := proof.NewKey()
	require.NoError(t, err)
	tokens := func(token string) bool {
		return strings.HasPrefix(token, "tok-") || strings.HasPrefix(token, "rotated-") || strings.HasPrefix(token, "control-")
	}
	rig := newRig(t, liveFor(thumbprint(t, key), tokens), nil)

	t.Run("a session trading at its broker", func(t *testing.T) {
		brokerURL, trades := stubBroker(t, 15*time.Minute)
		identity := filepath.Join(t.TempDir(), "token")
		require.NoError(t, os.WriteFile(identity, []byte("pod-token"), 0o600))
		client, err := cipherclient.New(cipherclient.Config{BrokerURL: cipheraddr.New(brokerURL), Key: key, Subject: cipherclient.NewProjectedToken(identity), Session: "ses_a"})
		require.NoError(t, err)
		option, _ := natscred.Session(client)
		nc, err := connect(t, rig.url, option)
		require.NoError(t, err, "the session's credential was refused:\n%s", rig.log.String())
		_, err = nc.SubscribeSync(bus.DeliverSubject("ses_a"))
		require.NoError(t, err)
		require.NoError(t, nc.Flush())
		require.EqualValues(t, 1, trades.Load())
	})

	// rotating answers first, then second for every later read: the rotation lands between the
	// one read a CONNECT makes for its signature and anything read after it.
	rotating := func(first, second string) (func(context.Context) (string, error), *atomic.Int64) {
		reads := &atomic.Int64{}
		return func(context.Context) (string, error) {
			if reads.Add(1) == 1 {
				return first, nil
			}
			return second, nil
		}, reads
	}
	t.Run("a token rotated between the signature and the token", func(t *testing.T) {
		current, reads := rotating("rotated-1", "rotated-2")
		option, _ := natscred.SessionFrom(key, current)
		_, err := connect(t, rig.url, option)
		require.NoError(t, err, "the snapshot's connection was refused:\n%s", rig.log.String())
		require.EqualValues(t, 1, reads.Load(), "the CONNECT read the token more than once")
		rotated, _ := current(t.Context())
		require.Equal(t, "rotated-2", rotated, "the token did rotate after the signature")
	})
	t.Run("control: a token handler that reads the token again", func(t *testing.T) {
		current, _ := rotating("control-1", "control-2")
		_, sign := natscred.SessionFrom(key, current)
		_, err := connect(t, rig.url,
			nats.UserJWT(func() (string, error) { return natscred.PlaceholderJWT, nil }, sign),
			nats.TokenHandler(func() string { token, _ := current(context.Background()); return token }))
		require.ErrorIs(t, err, nats.ErrAuthorization)
		require.Eventually(t, func() bool { return rig.refused(t, "proof_invalid") >= 1 },
			5*time.Second, 50*time.Millisecond, "the service did not refuse the rotated token for its proof:\n%s", rig.log.String())
	})
}

// A session's keeper reconnects on each token its renewal loop lands after the first: the loop
// trades on its first pass (cipherclient.Client.Run) just after the connection presented a live
// token, so the first is no renewal while the presented token outlives its renewal, and a
// reconnect on it would cut the connection for nothing. A later token the connection did not
// present is a renewal; one it did present is not. The control is the keeper without the skip of
// the first, which reconnects on it.
func TestSessionKeeperReconnectsOnEachTokenAfterTheFirst(t *testing.T) {
	_, url := testnats.Start(t)
	var reconnected atomic.Int64
	nc, err := nats.Connect(url, nats.Timeout(10*time.Second), nats.ReconnectWait(50*time.Millisecond),
		nats.ReconnectHandler(func(*nats.Conn) { reconnected.Add(1) }))
	require.NoError(t, err)
	defer nc.Close()

	// run lands each token the test sends it, as cipherclient.Client.Run lands each it trades,
	// each living 15 minutes and renewed 5 minutes ahead of its expiry; the token the connection
	// presented lives as long, so it outlives the first token's renewal.
	landed, handled := make(chan string), make(chan struct{})
	run := func(ctx context.Context, onToken func(context.Context, *cipherclient.Token) error) error {
		for {
			select {
			case <-ctx.Done():
				return nil
			case token := <-landed:
				if err := onToken(ctx, &cipherclient.Token{AccessToken: token, Expiry: time.Now().Add(15 * time.Minute)}); err != nil {
					return err
				}
				handled <- struct{}{}
			}
		}
	}
	renewal := func(token *cipherclient.Token) time.Time { return token.Expiry.Add(-cipherclient.DefaultRenewBefore) }
	presented := &cipherclient.Token{AccessToken: "presented-not-a-secret", Expiry: time.Now().Add(15 * time.Minute)}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	kept := keepUntilEnded(ctx, natscred.SessionKeeper(run, renewal, presented), nc)
	land := func(token string) {
		t.Helper()
		select {
		case landed <- token:
		case <-kept:
			t.Fatalf("the keeper stopped before %s landed", token)
		}
		select {
		case <-handled:
		case <-kept:
			t.Fatalf("the keeper stopped on %s", token)
		}
	}

	land("tok-first")
	require.Never(t, func() bool { return reconnected.Load() > 0 }, 2*time.Second, 50*time.Millisecond,
		"the keeper reconnected on the first token its loop landed")
	land("tok-second")
	testwait.WithinPolling(t, 5*time.Second, 50*time.Millisecond, "the keeper's reconnect with the second token", func() bool {
		return reconnected.Load() == 1
	})
	land("presented-not-a-secret")
	require.Never(t, func() bool { return reconnected.Load() > 1 }, time.Second, 50*time.Millisecond,
		"the keeper reconnected on the token the connection presented")
}

// A session's keeper moves its connection off a presented token that lapses before the renewal
// loop's first token is renewed. The connection presents the token the client holds until that
// token's renewal moment (cipherclient.Client.Token, Client.RenewalAt), and the loop trades at
// once and renews the token it traded only at that token's Client.RenewalAt (Client.Run), so a
// presented token that lapses before then would be dropped by the server at its own expiry before
// any later token landed. The keeper reconnects on the first token then, presenting the token the
// loop holds, and skips the first only when the presented token outlives that token's renewal.
// The control is the keeper that skips the first token whatever the presented one has left,
// which leaves the connection on the token that lapses first.
func TestSessionKeeperMovesOffAPresentedTokenThatLapsesFirst(t *testing.T) {
	key, err := proof.NewKey()
	require.NoError(t, err)
	rig := newRig(t, liveFor(thumbprint(t, key), func(token string) bool { return strings.HasPrefix(token, "tok-") }), nil)
	identity := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(identity, []byte("pod-token"), 0o600))
	for _, row := range []struct {
		name string
		// presented is the life of the token the connection presents; the loop's first token lives
		// 15 minutes and is renewed 5 minutes (DefaultRenewBefore) ahead of its expiry.
		presented  time.Duration
		reconnects int64
	}{
		{"a presented token that lapses before the first token's renewal", 7 * time.Minute, 1},
		{"a presented token that outlives the first token's renewal", 15 * time.Minute, 0},
	} {
		t.Run(row.name, func(t *testing.T) {
			brokerURL, trades := stubBroker(t, row.presented)
			client, err := cipherclient.New(cipherclient.Config{BrokerURL: cipheraddr.New(brokerURL), Key: key, Subject: cipherclient.NewProjectedToken(identity), Session: "ses_a"})
			require.NoError(t, err)
			option, keeper := natscred.Session(client)
			var reconnected atomic.Int64
			nc, err := nats.Connect(rig.url, option, nats.Timeout(10*time.Second), nats.ReconnectWait(50*time.Millisecond),
				nats.ReconnectHandler(func(*nats.Conn) { reconnected.Add(1) }))
			require.NoError(t, err, "the session's credential was refused:\n%s", rig.log.String())
			defer nc.Close()
			ctx, cancel := context.WithCancel(t.Context())
			kept := keepUntilEnded(ctx, keeper, nc)
			defer func() {
				cancel()
				<-kept
			}()
			testwait.WithinPolling(t, 5*time.Second, 50*time.Millisecond, "the renewal loop's first trade", func() bool {
				return trades.Load() == 2
			})
			if row.reconnects == 0 {
				require.Never(t, func() bool { return reconnected.Load() > 0 }, 2*time.Second, 50*time.Millisecond,
					"the keeper reconnected on the first token though the presented one outlives its renewal")
				return
			}
			testwait.WithinPolling(t, 5*time.Second, 50*time.Millisecond, "the keeper's reconnect off the presented token that lapses first", func() bool {
				return reconnected.Load() == 1 && nc.IsConnected()
			})
			require.Never(t, func() bool { return reconnected.Load() > 1 }, time.Second, 50*time.Millisecond,
				"the keeper reconnected more than once")
			require.EqualValues(t, 2, trades.Load(), "the reconnect traded again rather than presenting the loop's token")
		})
	}
}
