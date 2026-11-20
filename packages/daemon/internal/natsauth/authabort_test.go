package natsauth_test

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/legion/daemon/internal/natsauth"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testnkeys"
)

// Connect presents only an nkey seed or no credential: none the NATS auth service decides
// (packages/envoy/internal/cipher/natscallout), so it leaves nats.go's IgnoreAuthErrorAbort at its
// default, as at the merge base, and a connection the server refuses twice in a row with the same
// error closes for good (nats.go v1.53.1, nats.go:4086). The option belongs to a credential the
// auth service decides (natscred sets it), which this package does not present; when the daemon
// gains one, the connection that presents it sets the option and this test keeps the seed's path
// as it is.
func TestADaemonSeedRefusedTwiceCloses(t *testing.T) {
	_, allowed := testnkeys.User(t)
	url := testnats.StartNkeyAuthorized(t, allowed)
	stranger := testnkeys.UserSeed(t)
	closed := make(chan struct{})
	conn, err := natsauth.Connect([]string{url}, stranger, "authabort-test",
		nats.RetryOnFailedConnect(true), natsauth.ReconnectForever(), nats.ReconnectWait(50*time.Millisecond),
		nats.ClosedHandler(func(*nats.Conn) { close(closed) }))
	if err != nil {
		t.Fatalf("Connect with RetryOnFailedConnect answered %v; it should hand back a connection that keeps trying", err)
	}
	defer conn.Close()
	select {
	case <-closed:
	case <-time.After(30 * time.Second):
		t.Fatalf("the connection kept trying after the same refusal twice: the daemon's seed path no longer gives up as at the merge base (connected: %t)", conn.IsConnected())
	}
}
