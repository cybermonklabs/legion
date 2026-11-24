package bus_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/cipher/natscallout"
	"github.com/sjawhar/envoy/internal/goroutinetest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/natscred"
)

const serviceTokenAccount = "system:serviceaccount:example-ns:example-listener"

// unsignedToken is a platform token naming serviceTokenAccount and lapsing at expiry, which the
// test's ServiceToken reads unverified.
func unsignedToken(expiry time.Time) string {
	encode := func(v any) string {
		raw, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	return encode(map[string]string{"alg": "none"}) + "." +
		encode(map[string]any{"sub": serviceTokenAccount, "iat": time.Now().Unix(), "exp": expiry.Unix()}) + ".unsigned"
}

// A bus connection presenting a service's token file moves to the later token the file holds,
// natscred.ServiceRenewBefore ahead of the one it connected with, by one reconnect, and stays up.
// The keeper first dials a throwaway connection with the later token and reconnects only once
// that is admitted, so the auth service is presented the later token twice, by that probe and by
// the connection's one reconnect.
func TestAServiceTokenClientReconnectsBeforeItsTokenLapses(t *testing.T) {
	keys := testnats.NewCalloutKeys(t)
	url := testnats.StartWithCallout(t, keys)
	dir := t.TempDir()
	write := func(name, text string) string {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(text), 0o600))
		return path
	}
	var mu sync.Mutex
	var presented []string
	services, err := config.ParseNATSServices("listener/example-hub/*=" + serviceTokenAccount)
	require.NoError(t, err)
	service, err := natscallout.Start(t.Context(), natscallout.Settings{
		URLs: []string{url}, SeedFile: write("service.seed", keys.ServiceSeed),
		IssuerSeedFile: write("issuer.seed", keys.IssuerSeed), XKeySeedFile: write("xkey.seed", keys.XKeySeed), Services: services,
	}, natscallout.Deps{
		ServiceToken: func(_ context.Context, raw string) (string, bool, time.Time, time.Time, error) {
			mu.Lock()
			presented = append(presented, raw)
			mu.Unlock()
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
		},
		Log: slog.New(slog.DiscardHandler),
	})
	require.NoError(t, err)
	t.Cleanup(service.Close)
	// No span of its own: the service can report ready late on a loaded machine with nothing wrong.
	ready := testwait.Context(t)
	require.NoError(t, service.Ready(ready))
	count := func(token string) int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, each := range presented {
			if each == token {
				n++
			}
		}
		return n
	}

	first := unsignedToken(time.Now().Add(natscred.ServiceRenewBefore + 2*time.Second))
	tokenFile := write("token", first)
	environment := map[string]string{bus.ServiceTokenFileEnvVar: tokenFile, bus.AllowRemoteEnvVar: "1"}
	nc, err := bus.Dial("service-token-test", []string{url}, func(name string) (string, bool) {
		value, set := environment[name]
		return value, set
	})
	require.NoError(t, err)
	defer nc.Close()
	require.True(t, nc.IsConnected())

	later := unsignedToken(time.Now().Add(50 * time.Minute))
	require.NoError(t, os.WriteFile(tokenFile, []byte(later), 0o600))
	testwait.WithinPolling(t, 10*time.Second, 50*time.Millisecond, "the probe and the reconnect with the later token", func() bool {
		return count(later) == 2 && nc.Stats().Reconnects == 1
	})
	testwait.Eventually(t, "the connection back up", nc.IsConnected)
	require.Never(t, func() bool { return count(later) > 2 || nc.Stats().Reconnects > 1 }, 3*time.Second, 100*time.Millisecond, "the client reconnected more than once")
	require.Equal(t, 1, count(first))
}

// natscredGoroutines is how many goroutines run natscred's code, a keeper or the watcher of its
// connection's close, and how many of those are a service's keeper (serviceCredential.Keep).
func natscredGoroutines() (all, keepers int) {
	for _, trace := range goroutinetest.Traces() {
		if strings.Contains(trace, "github.com/sjawhar/envoy/natscred.") {
			all++
		}
		if strings.Contains(trace, "github.com/sjawhar/envoy/natscred.(*serviceCredential).Keep(") {
			keepers++
		}
	}
	return all, keepers
}

// requireNoNatscredGoroutine waits for no natscred goroutine to run, bounded only by the test
// binary's deadline (testwait.UntilPolling), and fails naming how many still do, and how many of
// those are service keepers, when.
func requireNoNatscredGoroutine(t *testing.T, when string) {
	t.Helper()
	testwait.UntilPolling(t, 50*time.Millisecond, "no natscred goroutine to run "+when, func() (string, bool) {
		all, keepers := natscredGoroutines()
		return fmt.Sprintf("%d natscred goroutines, %d of them service keepers", all, keepers), all == 0
	})
}

// A replacement connection the client discards leaves no keeper running. The discard closes the
// connection with its ClosedCB cleared, so that its close starts no recovery
// (ensureConnWithContext), and a keeper that only that callback stopped ran on beside the closed
// connection until its token's renewal, most of an hour later. The test first dials one
// connection presenting a service token and sees its keeper, so the count can see one, then runs
// the client of TestAClosedConnectionIsReplacedOnceAndCloseLeavesNoneOpen on that token until a
// replacement is discarded and the client closed, and requires every natscred goroutine gone. The
// control is the keeper stopped by ClosedCB alone, as at ac2334299e: one keeper is left.
func TestADiscardedReplacementConnectionLeavesNoKeeperRunning(t *testing.T) {
	unsetNkeyEnvironment(t)
	requireNoNatscredGoroutine(t, "before the test, left by an earlier one")
	tokenFile := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte(unsignedToken(time.Now().Add(50*time.Minute))), 0o600))
	environment := map[string]string{bus.ServiceTokenFileEnvVar: tokenFile, bus.AllowRemoteEnvVar: "1"}
	lookup := func(name string) (string, bool) {
		value, ok := environment[name]
		return value, ok
	}

	_, url := testnats.Start(t)
	nc, err := bus.Dial("keeper-count", []string{url}, lookup)
	require.NoError(t, err)
	testwait.WithinPolling(t, 5*time.Second, 50*time.Millisecond, "the live connection's keeper", func() bool {
		_, keepers := natscredGoroutines()
		return keepers == 1
	})
	nc.Close()
	requireNoNatscredGoroutine(t, "after that connection closed")

	closedConnectionIsReplacedOnce(t, func(string) []bus.ConnectOption {
		return []bus.ConnectOption{bus.WithEnvironment(lookup)}
	})
	requireNoNatscredGoroutine(t, "after the discard and the client's Close")
}
