package natsauth_test

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/legion/daemon/internal/natsauth"
	"github.com/sjawhar/legion/daemon/internal/testnkeys"
)

// A hub that stops answering without closing the connection is noticed within seconds, because
// every connection the daemon opens pings every second and takes the connection for lost at the
// third unanswered ping (detectSilentCuts). The daemon opens one NATS connection, through Connect
// with a seed or no credential, or through ConnectService with its service token, and both dial
// it the one way (dial), so the pings belong to this package rather than to the options its caller
// passes: each case connects as the daemon's workflow does on one of those two paths and so covers
// the production path, not the wiring. With nats.go's own defaults the daemon reads the connection
// as connected for minutes into such a cut, and reads no event in them.
//
// fakeServer with nothing to write after the handshake is that hub: it answers the handshake PING
// and then reads every later line, the client's pings included, and answers none, with the socket
// open and no reset. It answers the handshake whatever the CONNECT presents, so the service
// token's connection is made there as a seed's is, with no auth service to decide it: the token's
// file is written whole, as the kubelet writes it (tokenFile), and the CONNECT presents it as user
// service.
func TestAHubThatStopsAnsweringIsNoticedWithinSeconds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		connect func(t *testing.T, url string, options ...nats.Option) (*nats.Conn, error)
	}{
		{"no credential, through Connect", func(t *testing.T, url string, options ...nats.Option) (*nats.Conn, error) {
			return natsauth.Connect([]string{url}, "", t.Name(), options...)
		}},
		{"the service token, through ConnectService", func(t *testing.T, url string, options ...nats.Option) (*nats.Conn, error) {
			file := tokenFile(t, testnkeys.ServiceToken(daemonAccount, time.Now().Add(time.Hour)))
			return natsauth.ConnectService([]string{url}, file, natsauth.ServiceTokenFileKey, t.Name(), options...)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out lockedBuffer
			conn, err := tc.connect(t, fakeServer(t, ""),
				natsauth.LogEvents(slog.New(slog.NewTextHandler(&out, nil))), natsauth.ReconnectForever())
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			t.Cleanup(conn.Close)

			// The server answered the handshake and nothing after it, so it is quiet from here.
			quiet := time.Now()
			const within = 5 * time.Second
			for {
				connected, logged := conn.IsConnected(), strings.Contains(out.String(), "NATS connection lost")
				if !connected && logged {
					break
				}
				if waited := time.Since(quiet); waited > within {
					t.Fatalf("the connection was not reported lost within %s of the server going quiet: IsConnected = %t, the warning logged = %t; log: %s",
						waited, connected, logged, out.String())
				}
				time.Sleep(20 * time.Millisecond)
			}
			t.Logf("the cut was noticed %s after the server went quiet", time.Since(quiet))
		})
	}
}
