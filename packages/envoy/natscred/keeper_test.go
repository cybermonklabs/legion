package natscred_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/natscred"
)

// statusListeners is how many status listeners nc holds (Conn.StatusChanged). nats.go reports the
// number nowhere, so it is read from the connection's own map (Conn.statListeners, nats.go
// v1.55.0), and only after the goroutine that last wrote it has handed over through a channel.
func statusListeners(t *testing.T, nc *nats.Conn) int {
	t.Helper()
	listeners := reflect.ValueOf(nc).Elem().FieldByName("statListeners")
	require.True(t, listeners.IsValid(), "nats.Conn holds no statListeners: read nats.go's status listeners again")
	count := 0
	for iter := listeners.MapRange(); iter.Next(); {
		count += iter.Value().Len()
	}
	return count
}

// keepUntilEnded runs keeper.Keep on nc under ctx, and answers a channel closed when it returns.
func keepUntilEnded(ctx context.Context, keeper natscred.Keeper, nc *nats.Conn) <-chan struct{} {
	kept := make(chan struct{})
	go func() {
		defer close(kept)
		keeper.Keep(ctx, nc)
	}()
	return kept
}

// requireEnded fails unless kept closes within two seconds.
func requireEnded(t *testing.T, kept <-chan struct{}, why string) {
	t.Helper()
	select {
	case <-kept:
	case <-time.After(2 * time.Second):
		t.Fatalf("the keeper still runs two seconds after %s", why)
	}
}

// A keeper ends when its connection closes, whoever closes it and whatever the keeper is doing. A
// discarded replacement connection is closed with its ClosedCB cleared (internal/bus,
// ensureConnWithContext), and a service's keeper beside it waited for its token's renewal, most of
// an hour, before it looked at the connection again. A session's keeper closed in the middle of a
// renewal call returned only at the next reconnect it would make, which a renewal that lands the
// presented token never makes. Each row closes the connection and requires the keeper to end
// within two seconds, its status listener given back. The control is each Keep without
// untilClosed: the service's sleeps on, and the session's waits for a renewal that never comes.
func TestAKeeperEndsWhenItsConnectionCloses(t *testing.T) {
	_, url := testnats.Start(t)

	t.Run("a service's keeper waiting for its renewal, closed with its ClosedCB cleared", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "token")
		require.NoError(t, os.WriteFile(file, []byte(platformToken(daemonAccount, time.Now().Add(time.Hour))), 0o600))
		option, keeper := natscred.Service(natscred.ServiceFile(file, "NATS_SERVICE_TOKEN_FILE"))
		nc, err := connect(t, url, option)
		require.NoError(t, err)
		before := statusListeners(t, nc)
		kept := keepUntilEnded(t.Context(), keeper, nc)
		time.Sleep(200 * time.Millisecond)
		nc.SetClosedHandler(nil)
		nc.Close()
		requireEnded(t, kept, "its connection closed")
		require.Equal(t, before, statusListeners(t, nc), "the keeper left its status listener on the connection")
	})

	t.Run("a session's keeper closed inside a renewal call", func(t *testing.T) {
		nc, err := connect(t, url)
		require.NoError(t, err)
		presented := &cipherclient.Token{AccessToken: "presented-not-a-secret", Expiry: time.Now().Add(15 * time.Minute)}
		inRenewal, release := make(chan struct{}), make(chan struct{})
		// run is a renewal loop inside its trade when the connection closes: the trade answers
		// the token the connection presented, and the loop then waits for its next renewal, as
		// cipherclient.Client.Run does, until its context ends.
		run := func(ctx context.Context, onToken func(context.Context, *cipherclient.Token) error) error {
			close(inRenewal)
			<-release
			if err := onToken(ctx, presented); err != nil {
				return err
			}
			<-ctx.Done()
			return ctx.Err()
		}
		renewal := func(token *cipherclient.Token) time.Time { return token.Expiry.Add(-cipherclient.DefaultRenewBefore) }
		before := statusListeners(t, nc)
		kept := keepUntilEnded(t.Context(), natscred.SessionKeeper(run, renewal, presented), nc)
		<-inRenewal
		require.Equal(t, before+1, statusListeners(t, nc), "the keeper registered no status listener: this row cannot see one given back")
		nc.Close()
		close(release)
		requireEnded(t, kept, "its connection closed during a renewal")
		require.Equal(t, before, statusListeners(t, nc), "the keeper left its status listener on the connection")
	})
}

// A keeper that ends for its own reason, its context ended or its renewal loop ended (as
// cipherclient.Client.Run ends on a refusal no retry cures), gives its connection's status
// listener back, so no listener outlives it on a connection that stays open. The control is
// untilClosed's stop without RemoveStatusListener: the listener count stays one above.
func TestAKeeperThatEndsOnItsOwnGivesBackItsStatusListener(t *testing.T) {
	_, url := testnats.Start(t)
	presented := &cipherclient.Token{AccessToken: "presented-not-a-secret", Expiry: time.Now().Add(15 * time.Minute)}
	renewal := func(token *cipherclient.Token) time.Time { return token.Expiry.Add(-cipherclient.DefaultRenewBefore) }
	for _, row := range []struct {
		name string
		// end ends the keeper: cancel ends its context, release lets its renewal loop return.
		end func(cancel context.CancelFunc, release chan struct{})
	}{
		{"its context ended", func(cancel context.CancelFunc, _ chan struct{}) { cancel() }},
		{"its renewal loop ended", func(_ context.CancelFunc, release chan struct{}) { close(release) }},
	} {
		t.Run(row.name, func(t *testing.T) {
			nc, err := connect(t, url)
			require.NoError(t, err)
			inRenewal, release := make(chan struct{}), make(chan struct{})
			run := func(ctx context.Context, _ func(context.Context, *cipherclient.Token) error) error {
				close(inRenewal)
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-release:
					return errors.New("the broker refused the session for good")
				}
			}
			before := statusListeners(t, nc)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			kept := keepUntilEnded(ctx, natscred.SessionKeeper(run, renewal, presented), nc)
			<-inRenewal
			require.Equal(t, before+1, statusListeners(t, nc), "the keeper registered no status listener: this row cannot see one given back")
			row.end(cancel, release)
			requireEnded(t, kept, "it was ended")
			require.True(t, nc.IsConnected(), "the connection closed: this row is the keeper ending on its own")
			require.Equal(t, before, statusListeners(t, nc), "the keeper ended and left its status listener on the open connection")
		})
	}
}
