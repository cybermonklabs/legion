package natscred_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/cipher/natscallout"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/natscred"
)

// presented records each service token the auth service was presented, in order.
type presented struct {
	mu     sync.Mutex
	tokens []string
}

func (p *presented) serviceToken(_ context.Context, raw string) (natscallout.ServiceIdentity, error) {
	p.mu.Lock()
	p.tokens = append(p.tokens, raw)
	p.mu.Unlock()
	return readPlatformToken(raw)
}

func (p *presented) count(token string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, each := range p.tokens {
		if each == token {
			n++
		}
	}
	return n
}

// A service's keeper reads the expiry of the token its connection presented and, ServiceRenewBefore
// ahead of it, dials a throwaway connection with the later token its file holds by then, and once
// that is admitted reconnects once with it: the auth service is presented the later token twice,
// by the probe and by the reconnect, the connection is cut once, after the probe was admitted, and
// it stays up.
func TestServiceKeeperReconnectsBeforeExpiry(t *testing.T) {
	seen := &presented{}
	rig := newRig(t, nil, seen.serviceToken)
	file := filepath.Join(t.TempDir(), "token")
	first := platformToken(daemonAccount, time.Now().Add(natscred.ServiceRenewBefore+2*time.Second))
	later := platformToken(daemonAccount, time.Now().Add(50*time.Minute))
	require.NoError(t, os.WriteFile(file, []byte(first+"\n"), 0o600))

	option, keeper := natscred.Service(natscred.ServiceFile(file))
	var disconnected, reconnected, laterAtCut atomic.Int64
	nc, err := nats.Connect(rig.url, option, nats.Timeout(10*time.Second),
		nats.DisconnectErrHandler(func(*nats.Conn, error) {
			if disconnected.Add(1) == 1 {
				laterAtCut.Store(int64(seen.count(later)))
			}
		}),
		nats.ReconnectHandler(func(*nats.Conn) { reconnected.Add(1) }))
	require.NoError(t, err, "the service's token was refused:\n%s", rig.log.String())
	defer nc.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go keeper.Keep(ctx, nc)

	require.NoError(t, os.WriteFile(file, []byte(later+"\n"), 0o600))
	testwait.WithinPolling(t, 10*time.Second, 50*time.Millisecond, "the keeper's reconnect with the later token", func() bool {
		return reconnected.Load() == 1 && seen.count(later) == 2
	})
	require.Eventually(t, nc.IsConnected, 5*time.Second, 50*time.Millisecond)
	require.Never(t, func() bool { return seen.count(later) > 2 || disconnected.Load() > 1 || reconnected.Load() > 1 },
		3*time.Second, 100*time.Millisecond, "the keeper probed or reconnected again with a token it had already presented")
	require.EqualValues(t, 1, disconnected.Load())
	require.EqualValues(t, 1, laterAtCut.Load(), "the connection was cut before a probe with the later token was admitted")
	require.Equal(t, 1, seen.count(first))
}

// While the auth service is down at a service's renewal point, its keeper leaves the connection
// as it is: a reconnect then is refused until the service answers again, and the user the
// connection holds still has ServiceRenewBefore to live. It dials a throwaway connection, named for
// the service's with " probe", with the later token until one is admitted, and only then cuts
// over, once. The control is the keeper that reconnected unconditionally, which cut the connection
// at the renewal point with nothing up to admit it again.
func TestServiceKeeperHoldsTheConnectionWhileTheAuthServiceIsDown(t *testing.T) {
	seen := &presented{}
	rig := newRig(t, nil, seen.serviceToken)
	file := filepath.Join(t.TempDir(), "token")
	renewal := time.Now().Add(3 * time.Second)
	first := platformToken(daemonAccount, renewal.Add(natscred.ServiceRenewBefore))
	later := platformToken(daemonAccount, time.Now().Add(50*time.Minute))
	require.NoError(t, os.WriteFile(file, []byte(first+"\n"), 0o600))

	option, keeper := natscred.Service(natscred.ServiceFile(file))
	var disconnected, reconnected atomic.Int64
	nc, err := nats.Connect(rig.url, option, nats.Name("daemon"), nats.Timeout(10*time.Second),
		nats.DisconnectErrHandler(func(*nats.Conn, error) { disconnected.Add(1) }),
		nats.ReconnectHandler(func(*nats.Conn) { reconnected.Add(1) }))
	require.NoError(t, err, "the service's token was refused:\n%s", rig.log.String())
	defer nc.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go keeper.Keep(ctx, nc)

	rig.stopService()
	require.NoError(t, os.WriteFile(file, []byte(later+"\n"), 0o600))
	require.Never(t, func() bool { return disconnected.Load() > 0 }, max(time.Until(renewal), 0)+5*time.Second,
		100*time.Millisecond, "the keeper cut the connection while the auth service was down")
	require.True(t, nc.IsConnected())
	testwait.WithinPolling(t, 5*time.Second, 100*time.Millisecond, "the hub's record of a refused probe", func() bool {
		return slices.Contains(closedNames(t, rig.server), "daemon probe")
	})
	require.Zero(t, seen.count(later), "the auth service was presented the later token while it was down")

	rig.startService(t)
	testwait.WithinPolling(t, 15*time.Second, 50*time.Millisecond, "the keeper's cut-over once the auth service answered", func() bool {
		return reconnected.Load() == 1 && nc.IsConnected()
	})
	require.Never(t, func() bool { return disconnected.Load() > 1 || reconnected.Load() > 1 || seen.count(later) > 2 },
		3*time.Second, 100*time.Millisecond, "the keeper cut the connection again")
	require.EqualValues(t, 1, disconnected.Load())
	require.Equal(t, 2, seen.count(later), "the later token was presented other than by the admitted probe and the reconnect")
	require.Equal(t, 1, seen.count(first))
}

// closedNames is the name of each client connection server has closed, as its monitoring endpoint
// reports them (/connz?state=closed).
func closedNames(t *testing.T, server *testnats.Server) []string {
	t.Helper()
	response, err := http.Get(server.MonitorURL() + "/connz?state=closed")
	require.NoError(t, err)
	defer response.Body.Close()
	var connz struct {
		Connections []struct {
			Name string `json:"name"`
		} `json:"connections"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&connz))
	names := make([]string, 0, len(connz.Connections))
	for _, connection := range connz.Connections {
		names = append(names, connection.Name)
	}
	return names
}

// A service whose token the auth service refuses keeps trying, and is admitted once the service
// answers: here the first two decisions refuse it. The control is the same client without
// IgnoreAuthErrorAbort, which nats.go closes after the same refusal twice.
func TestServiceCredentialRetriesAfterRefusals(t *testing.T) {
	var asked atomic.Int64
	refuseTwice := func(_ context.Context, raw string) (natscallout.ServiceIdentity, error) {
		if asked.Add(1) <= 2 {
			return natscallout.ServiceIdentity{}, errors.New("the broker could not verify the token yet")
		}
		return readPlatformToken(raw)
	}
	rig := newRig(t, nil, refuseTwice)
	file := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(file, []byte(platformToken(daemonAccount, time.Now().Add(time.Hour))), 0o600))
	retrying := []nats.Option{nats.RetryOnFailedConnect(true), nats.MaxReconnects(-1), nats.ReconnectWait(50 * time.Millisecond), nats.Timeout(5 * time.Second)}

	option, _ := natscred.Service(natscred.ServiceFile(file))
	nc, err := nats.Connect(rig.url, append(retrying, option)...)
	require.NoError(t, err)
	defer nc.Close()
	testwait.WithinPolling(t, 20*time.Second, 50*time.Millisecond, "the service's connection after two refusals", nc.IsConnected)
	require.GreaterOrEqual(t, asked.Load(), int64(3))

	t.Run("control: without IgnoreAuthErrorAbort", func(t *testing.T) {
		asked.Store(0)
		reader := natscred.ServiceFile(file)
		closed := make(chan struct{})
		control, err := nats.Connect(rig.url, append(retrying,
			nats.UserInfoHandler(func() (string, string) { token, _ := reader.Read(); return natscred.ServiceUser, token }),
			nats.ClosedHandler(func(*nats.Conn) { close(closed) }))...)
		require.NoError(t, err)
		defer control.Close()
		select {
		case <-closed:
		case <-time.After(20 * time.Second):
			t.Fatalf("the control kept trying after the same refusal twice; connected: %t", control.IsConnected())
		}
		require.False(t, control.IsConnected())
	})
}

// Identity names the account a platform token is for, unverified: a Kubernetes token's subject.
func TestIdentityNamesTheTokensAccount(t *testing.T) {
	account, err := natscred.Identity(platformToken(daemonAccount, time.Now().Add(time.Hour)))
	require.NoError(t, err)
	require.Equal(t, daemonAccount, account)
	_, err = natscred.Identity("not-a-jwt")
	require.Error(t, err)
}
