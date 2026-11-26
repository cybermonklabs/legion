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
	"github.com/sjawhar/envoy/internal/oidc"
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
	// The one issuer of a bare BROKER_K8S_OIDC_ISSUER URL, whose name, "", the stub answers.
	services, err := config.ParseNATSServices("listener/example-hub/*="+serviceTokenAccount, []oidc.NamedIssuer{{URL: "https://cluster.example"}})
	require.NoError(t, err)
	service, err := natscallout.Start(t.Context(), natscallout.Settings{
		URLs: []string{url}, SeedFile: write("service.seed", keys.ServiceSeed),
		IssuerSeedFile: write("issuer.seed", keys.IssuerSeed), XKeySeedFile: write("xkey.seed", keys.XKeySeed), Services: services,
	}, natscallout.Deps{
		ServiceToken: func(_ context.Context, raw string) (natscallout.ServiceIdentity, error) {
			mu.Lock()
			presented = append(presented, raw)
			mu.Unlock()
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
	testwait.Until(t, "the probe and the reconnect with the later token", func() (string, bool) {
		presented, reconnects := count(later), nc.Stats().Reconnects
		return fmt.Sprintf("the later token presented %d time(s), %d reconnect(s)", presented, reconnects), presented == 2 && reconnects == 1
	})
	testwait.Until(t, "the connection to be back up", func() (fmt.Stringer, bool) { return nc.Status(), nc.IsConnected() })
	require.Never(t, func() bool { return count(later) > 2 || nc.Stats().Reconnects > 1 }, 3*time.Second, 100*time.Millisecond, "the client reconnected more than once")
	require.Equal(t, 1, count(first))
}

// natscredDump is what one dump of every goroutine says of natscred's goroutines
// (natscredGoroutines).
type natscredDump struct {
	// all is how many goroutines run natscred's code, a keeper or the watcher of its connection's
	// close, and keepers how many of those are a service's keeper (serviceCredential.Keep).
	all, keepers int
	// dispatchers is how many goroutines run a nats.go connection's callback dispatcher
	// (asyncCBDispatcher), which runs a connection's ClosedCB.
	dispatchers int
	// stuck is whether some natscred goroutine runs and nothing the dump shows is on its way to end
	// any of them: every natscred goroutine is parked, and every dispatcher is idle, parked waiting
	// for a callback (sync.Cond.Wait).
	stuck bool
}

// natscredGoroutines reads one dump of every goroutine (natscredDump).
//
// Why a correct tree is never stuck once its connection's Close has returned: a keeper ends only
// when its context ends, and only its watcher (natscred's untilClosed, woken by the connection's
// CLOSED event) or a callback (a ClosedCB, on the dispatcher) can end that context. nats.go hands
// the CLOSED event to the watcher, and queues a callback for the dispatcher, before Close returns,
// and readies the goroutine it hands one to in that step; the runtime readies every goroutine
// waiting on a context's Done channel when the context ends, and every goroutine waiting on a
// channel when it is closed. So until the keeper has gone, one of the keeper, its watcher and the
// dispatchers is runnable, running or waiting on a lock. A keeper that ends only at a timer of its
// own (its token's renewal), however it waits for that timer, is stuck as soon as its connection's
// close has been handed out.
func natscredGoroutines() natscredDump {
	dump := natscredDump{stuck: true}
	for _, trace := range goroutinetest.Traces() {
		state := traceState(trace)
		// The runtime prints the package path's last element with its dots escaped,
		// github.com/nats-io/nats%2ego, so the match is on the method alone.
		dispatcher := strings.Contains(trace, ".(*asyncCallbacksHandler).asyncCBDispatcher(")
		if dispatcher {
			dump.dispatchers++
		}
		if strings.Contains(trace, "github.com/sjawhar/envoy/natscred.") {
			// A goroutine running natscred's code is judged by that rule alone, even on a
			// dispatcher: a keeper started there without its go waits in its own select, and the
			// dispatcher's rule would keep it from ever being stuck.
			dump.all++
			dump.stuck = dump.stuck && parked(state)
			if strings.Contains(trace, "github.com/sjawhar/envoy/natscred.(*serviceCredential).Keep(") {
				dump.keepers++
			}
			continue
		}
		if dispatcher {
			dump.stuck = dump.stuck && state == "sync.Cond.Wait"
		}
	}
	dump.stuck = dump.stuck && dump.all > 0
	return dump
}

// parked reports whether a goroutine in state waits on an event no running code is about to
// bring: a select, a channel's receive or send, a sleep, a condition or a wait group, whatever
// timer it waits with. A goroutine waiting on a lock (sync.Mutex, sync.RWMutex, semacquire) is not
// parked, since the lock's holder lets it go: a correct keeper takes its connection's lock on its
// way out (RemoveStatusListener), which nats.go's own goroutines also take. Neither is one running,
// runnable, preempted, in a system call or in IO wait, which the kernel ends.
func parked(state string) bool {
	switch state {
	case "select", "select (no cases)", "chan receive", "chan send", "chan receive (nil chan)", "chan send (nil chan)",
		"sleep", "sync.Cond.Wait", "sync.WaitGroup.Wait":
		return true
	}
	return false
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
// only a hang guard, for a natscred goroutine that never parks and never ends: one that runs for
// ever, or waits on a lock its holder never lets go.
func requireNoNatscredGoroutine(t *testing.T, when string) {
	t.Helper()
	var last natscredDump
	testwait.UntilPolling(t, 50*time.Millisecond, "no natscred goroutine to run "+when, func() (string, bool) {
		last = natscredGoroutines()
		return fmt.Sprintf("%+v", last), last.all == 0 || last.stuck
	})
	if last.all != 0 {
		t.Fatalf("%d natscred goroutines, %d of them service keepers, still run %s, each parked where no running code can wake it, want none", last.all, last.keepers, when)
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
	// The control of the stuck rule: with the connection live, its keeper and the keeper's watcher
	// wait for the token's renewal and the connection's close, and its callback dispatcher waits for a
	// callback, which is the stuck state the row fails on once the connection has closed. So the dump
	// must show the keeper, a dispatcher and stuck, or the rule cannot fire with this nats.go and this
	// runtime (a dispatcher name or a wait state it does not know), and every row it judges would pass.
	testwait.UntilPolling(t, 50*time.Millisecond, "the live connection's keeper, its callback dispatcher and the stuck state", func() (string, bool) {
		dump := natscredGoroutines()
		return fmt.Sprintf("%+v", dump), dump.keepers == 1 && dump.dispatchers >= 1 && dump.stuck
	})
	nc.Close()
	requireNoNatscredGoroutine(t, "after that connection closed")

	closedConnectionIsReplacedOnce(t, func(string) []bus.ConnectOption {
		return []bus.ConnectOption{bus.WithEnvironment(lookup)}
	})
	requireNoNatscredGoroutine(t, "after the discard and the client's Close")
}
