package natscallout

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// raisedBudget is what the control raises decideBudget to: past the two seconds a Go client's
// connect allows, so the same blocking stub makes that client fail rather than be refused.
const raisedBudget = 2500 * time.Millisecond

// A client's connect allows two seconds (nats.go sets one deadline of Opts.Timeout over the whole
// handshake, processConnectInit; the server gives up at its own AUTH_TIMEOUT, also two seconds),
// so every decision is answered inside the budget: a stub that blocks for three seconds is
// refused with the budget's reason, naming the step, inside two seconds, and the server logs no
// authorization timeout. A stub that answers in 100 ms is admitted.
//
// The control is what makes the budget mean anything: raise decideBudget to 2.5 s with the same
// blocking stub, and the client fails its connect rather than being refused, and the server logs
// giving up on the request (hubGaveUp, which the server writes to its debug log only). Without it
// a shorter budget would look like caution; with it, an answer the budget would have stopped is
// shown to cost the connection, and the server's log is shown to name it.
func TestTheServiceAnswersInsideTheGoClientsConnectDeadline(t *testing.T) {
	keys := testnats.NewCalloutKeys(t)
	server, url := startDebugCalloutServer(t, keys)
	account := "system:serviceaccount:example-ns:example-daemon"

	// blockingFor is a ServiceToken that takes wait, as a verifier waiting on a key set would,
	// and answers a live token of the listed account after it.
	blockingFor := func(wait time.Duration) Deps {
		return Deps{ServiceToken: func(ctx context.Context, _ string) (identity.Caller, error) {
			select {
			case <-ctx.Done():
				return identity.Caller{}, ctx.Err()
			case <-time.After(wait):
			}
			now := time.Now()
			return identity.Caller{Account: inCluster(account), IssuedAt: now, Expiry: now.Add(time.Hour)}, nil
		}}
	}
	// connectService connects as that daemon, as a service with a platform token does, and
	// answers how long the connect took.
	connectService := func(t *testing.T) (error, time.Duration) {
		t.Helper()
		token := unsignedServiceToken(account, time.Hour)
		began := time.Now()
		nc, err := nats.Connect(url, nats.UserInfo("service", token), nats.NoReconnect())
		took := time.Since(began)
		if err == nil {
			t.Cleanup(nc.Close)
		}
		return err, took
	}

	t.Run("a verifier that answers in 100ms", func(t *testing.T) {
		service := startService(t, url, keys, blockingFor(100*time.Millisecond), Decide)
		t.Cleanup(service.Close)
		err, took := connectService(t)
		require.NoError(t, err, "a connection decided in 100 ms was not admitted (%s)", took)
	})

	t.Run("a verifier that blocks for 3s is refused inside the client's deadline", func(t *testing.T) {
		service := startService(t, url, keys, blockingFor(3*time.Second), Decide)
		t.Cleanup(service.Close)
		before := len(server.Log())
		err, took := connectService(t)
		require.ErrorIs(t, err, nats.ErrAuthorization,
			"the client failed its connect after %s rather than being refused", took)
		require.Less(t, took, 2*time.Second, "the refusal reached the client after %s, past its connect's own deadline", took)
		// The server gives up one authorization timeout after it sent the request, so by then it
		// has said so, had it given up.
		time.Sleep(testnats.CalloutAuthTimeout*time.Second - took + 100*time.Millisecond)
		require.Zero(t, strings.Count(server.Log()[before:], hubGaveUp),
			"the server gave up on the service rather than reading its refusal")
	})

	t.Run("control: the same stub with the budget raised to 2.5s", func(t *testing.T) {
		was := decideBudget
		decideBudget = raisedBudget
		t.Cleanup(func() { decideBudget = was })
		service := startService(t, url, keys, blockingFor(3*time.Second), Decide)
		t.Cleanup(service.Close)
		before := len(server.Log())
		err, took := connectService(t)
		// The client is gone before the raised budget would have answered. Which of the two
		// two-second timers fires first — nats.go's own handshake deadline or the server giving
		// up and refusing — is a race, and the point is that neither waits for the answer: it
		// would have come at the raised budget, when the stub returns on its cancelled context.
		require.Error(t, err, "a client admitted with the budget at %s: the budget would be arbitrary", raisedBudget)
		require.Less(t, took, raisedBudget,
			"the client waited %s, so the answer at %s could still have reached it and the control proves nothing", took, raisedBudget)
		testwait.WithinPolling(t, 5*time.Second, 20*time.Millisecond, "the server's log to say it gave up on the request", func() bool {
			return strings.Contains(server.Log()[before:], hubGaveUp)
		})
	})
}

// unsignedServiceToken is a platform identity token naming account and lapsing after life, which
// the stubs above read without verifying.
func unsignedServiceToken(account string, life time.Duration) string {
	encode := func(value any) string {
		raw, _ := json.Marshal(value)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	now := time.Now()
	return strings.Join([]string{
		encode(map[string]string{"alg": "none"}),
		encode(map[string]any{"sub": account, "iat": now.Unix(), "exp": now.Add(life).Unix()}),
		"unsigned",
	}, ".")
}
