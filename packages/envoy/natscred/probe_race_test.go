//go:build race

// This file holds the probe's race test, which only the race build compiles: it passes in any
// other build whatever the probe reads, since only the race detector sees what it checks.

package natscred_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/natscred"
)

// A service's keeper probes while nats.go may be setting the connection's handlers, as
// internal/bus does on a replacement connection it discards (SetClosedHandler,
// SetDisconnectErrHandler, internal/bus/nats.go): the probe reads only the fields of the
// connection's options that say where and how it reaches the server, which no Set*Handler
// writes, so the race detector reports nothing. The control is a probe that copied the whole of
// nc.Opts, which the detector reports as a race with those writes.
func TestServiceProbeRacesNoHandlerSet(t *testing.T) {
	seen := &presented{}
	rig := newRig(t, nil, seen.serviceToken)
	file := filepath.Join(t.TempDir(), "token")
	first := platformToken(daemonAccount, time.Now().Add(natscred.ServiceRenewBefore+time.Second))
	require.NoError(t, os.WriteFile(file, []byte(first+"\n"), 0o600))

	option, keeper := natscred.Service(natscred.ServiceFile(file))
	nc, err := nats.Connect(rig.url, option, nats.Name("daemon"), nats.Timeout(10*time.Second))
	require.NoError(t, err, "the service's token was refused:\n%s", rig.log.String())
	defer nc.Close()

	// The auth service is down from here, so every probe is refused and the keeper keeps probing.
	rig.stopService()
	later := platformToken(daemonAccount, time.Now().Add(50*time.Minute))
	require.NoError(t, os.WriteFile(file, []byte(later+"\n"), 0o600))

	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			select {
			case <-stop:
				return
			default:
			}
			nc.SetClosedHandler(nil)
			nc.SetDisconnectErrHandler(nil)
			time.Sleep(time.Millisecond)
		}
	}()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go keeper.Keep(ctx, nc)

	testwait.WithinPolling(t, 15*time.Second, 100*time.Millisecond, "two probes while the handlers were being set", func() bool {
		probes := 0
		for _, name := range closedNames(t, rig.server) {
			if name == "daemon probe" {
				probes++
			}
		}
		return probes >= 2
	})
	close(stop)
	<-stopped
	require.True(t, nc.IsConnected())
}
