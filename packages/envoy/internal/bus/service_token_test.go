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

// natscredGoroutines reads one dump of every goroutine. all is how many run natscred's code, a
// keeper or the watcher of its connection's close, and keepers is how many of those are a
// service's keeper (serviceCredential.Keep). stuck is whether some run and none of them can run
// again but by its own timer: each is parked in a select, and every nats.go callback dispatcher is
// parked waiting for a callback (asyncCBDispatcher), so no close and no callback is on its way to
// any of them. A keeper ends only when its context ends (natscred's sleep), and only its watcher,
// which wakes on its connection's CLOSED event (natscred's untilClosed), or a callback could end
// that context. nats.go hands the CLOSED event to the watcher, and a callback to the dispatcher,
// before Close returns, and readies the goroutine it hands one to in that step; the runtime readies
// every goroutine parked on a context's Done channel when that context ends. So once each of
// them has been closed, a keeper whose connection's close ends it is never parked with its watcher
// and every dispatcher: one of them is ready to run until the keeper has gone. A keeper that ends
// only at its token's renewal is stuck as soon as its connection's close has been handed out.
func natscredGoroutines() (all, keepers int, stuck bool) {
	stuck = true
	for _, trace := range goroutinetest.Traces() {
		state := traceState(trace)
		if strings.Contains(trace, "github.com/sjawhar/envoy/natscred.") {
			all++
			stuck = stuck && state == "select"
		}
		if strings.Contains(trace, "github.com/sjawhar/envoy/natscred.(*serviceCredential).Keep(") {
			keepers++
		}
		if strings.Contains(trace, "github.com/nats-io/nats.go.(*asyncCallbacksHandler).asyncCBDispatcher(") {
			stuck = stuck && state == "sync.Cond.Wait"
		}
	}
	return all, keepers, stuck && all > 0
}

// traceState is the state a goroutine trace's header line names, "goroutine <id> [<state>]:",
// without how long the goroutine has waited in it (", 2 minutes").
func traceState(trace string) string {
	header, _, _ := strings.Cut(trace, "\n")
	_, state, _ := strings.Cut(header, "[")
	state, _, _ = strings.Cut(state, "]")
	state, _, _ = strings.Cut(state, ",")
	return state
}

// requireNoNatscredGoroutine waits for no natscred goroutine to run, and fails, naming how many
// still do and how many of those are service keepers, when, as soon as the ones left are stuck
// (natscredGoroutines). It ends on one of those two facts, never on a span: a keeper left beside
// its closed connection ends by itself at its token's renewal, about 40 minutes on with this
// file's 50-minute tokens (natscred.ServiceRenewBefore is 10), so a wait that ended only on the
// count would pass that keeper under any deadline past that, and a span short of it would be a
// guess at how slow a keeper's exit may be. The test binary's deadline (testwait.UntilPolling) is
// only a hang guard, for a goroutine that neither ends nor parks.
func requireNoNatscredGoroutine(t *testing.T, when string) {
	t.Helper()
	var all, keepers int
	var stuck bool
	testwait.UntilPolling(t, 50*time.Millisecond, "no natscred goroutine to run "+when, func() (string, bool) {
		all, keepers, stuck = natscredGoroutines()
		return fmt.Sprintf("%d natscred goroutines, %d of them service keepers, stuck %t", all, keepers, stuck), all == 0 || stuck
	})
	if all != 0 {
		t.Fatalf("%d natscred goroutines, %d of them service keepers, still run %s, each parked where only its own timer can wake it, want none", all, keepers, when)
	}
}

// A replacement connection the client discards leaves no keeper running. The discard closes the
// connection with its ClosedCB cleared, so that its close starts no recovery
// (ensureConnWithContext), and a keeper that only that callback stopped ran on beside the closed
// connection until its token's renewal, most of an hour later. The test first dials one
// connection presenting a service token and sees its keeper, so the count can see one, then runs
// the client of TestAClosedConnectionIsReplacedOnceAndCloseLeavesNoneOpen on that token until a
// replacement is discarded and the client closed, and requires every natscred goroutine gone. The
// control is the keeper stopped by ClosedCB alone, as at ac2334299e: one keeper is left, parked
// until its token's renewal, and the row fails on it as soon as the client's Close has returned
// (requireNoNatscredGoroutine), not when a span runs out.
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
		_, keepers, _ := natscredGoroutines()
		return keepers == 1
	})
	nc.Close()
	requireNoNatscredGoroutine(t, "after that connection closed")

	closedConnectionIsReplacedOnce(t, func(string) []bus.ConnectOption {
		return []bus.ConnectOption{bus.WithEnvironment(lookup)}
	})
	requireNoNatscredGoroutine(t, "after the discard and the client's Close")
}
