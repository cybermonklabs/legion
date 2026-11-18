package natsauth_test

import (
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/natsauth"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A hub that stops answering without closing the connection is noticed within seconds, because
// Connect itself pings every second and takes the connection for lost at the third unanswered ping
// (detectSilentCuts). The daemon opens one NATS connection, and Connect is the one place it opens
// it, so the pings belong to Connect rather than to the options its caller passes: this test
// connects as the daemon's workflow does and so covers the production path, not the wiring. With
// nats.go's own defaults the daemon reads the connection as connected for minutes into such a cut,
// and reads no event in them.
//
// The pings run on nats.go's own timers, which no clock of the daemon's reaches, so "within
// seconds" is read from the pings the connection was given, and the cut is then waited for.
//
// fakeServer with nothing to write after the handshake is that hub: it answers the handshake PING
// and then reads every later line, the client's pings included, and answers none, with the socket
// open and no reset.
func TestAHubThatStopsAnsweringIsNoticedWithinSeconds(t *testing.T) {
	var out lockedBuffer
	conn, err := natsauth.Connect([]string{fakeServer(t, "")}, "", t.Name(),
		natsauth.LogEvents(slog.New(slog.NewTextHandler(&out, nil))), natsauth.ReconnectForever())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	if conn.Opts.PingInterval != time.Second || conn.Opts.MaxPingsOut != 2 {
		t.Fatalf("the connection pings every %s and is lost past %d unanswered, want a ping every second and the connection lost at the third unanswered one, which notices a quiet hub within seconds",
			conn.Opts.PingInterval, conn.Opts.MaxPingsOut)
	}

	// The server answered the handshake and nothing after it, so it is quiet from here.
	testwait.Until(t, "the connection reported lost once the server went quiet", func() (string, bool) {
		connected, logged := conn.IsConnected(), strings.Contains(out.String(), "NATS connection lost")
		return fmt.Sprintf("IsConnected = %t, the warning logged = %t; log: %s", connected, logged, out.String()), !connected && logged
	})
}
