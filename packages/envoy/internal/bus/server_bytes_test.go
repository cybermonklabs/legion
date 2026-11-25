package bus

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/natsconn"
)

// fakeNATS is the address of a server that admits one client, answers its first PING, and then
// writes after, raw protocol lines, to it; it is closed when t ends.
func fakeNATS(t *testing.T, after string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.WriteString(conn, `INFO {"server_id":"fake","version":"2.10.0","proto":1,"max_payload":1048576}`+"\r\n")
		lines := bufio.NewReader(conn)
		for {
			line, err := lines.ReadString('\n')
			if err != nil {
				return
			}
			if strings.HasPrefix(line, "PING") {
				break
			}
		}
		_, _ = io.WriteString(conn, "PONG\r\n"+after)
		_, _ = io.Copy(io.Discard, conn)
	}()
	return listener.Addr().String()
}

// A server's bytes the NATS client cannot parse, after the handshake, end the connection with an
// error the client formats from them, quoting them - its parser's (`nats: Parse Error [0]: '…'`) and
// processMsgArgs' for a MSG line it cannot read - and the bus's `envoy nats disconnected` line
// names that cause as a protocol error from the server, printing no part of a made-up marker in
// those bytes; no line the bus logs for the client holds it. The control is the same server sending
// a valid PING: the connection stays up, and no disconnect is logged.
func TestAServersUnreadableBytesReachNoBusLine(t *testing.T) {
	const marker = "madeUpServerBytesMarker0123456789"
	for _, tc := range []struct {
		name, after string
		protocol    bool
	}{
		{"a protocol operation the parser does not know", "XYZ " + marker + "\r\n", true},
		{"a MSG line processMsgArgs cannot read", "MSG legion.x " + marker + "\r\n", true},
		{"control: a valid PING", "PING\r\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs()
			name := "server-bytes-" + strings.ReplaceAll(tc.name, " ", "-")
			nc, err := Dial(name, []string{"nats://" + fakeNATS(t, tc.after)}, lookupOf(nil), WithLogger(logs.logger))
			require.NoError(t, err)
			defer nc.Close()
			disconnect := func() map[string]any {
				for _, record := range logs.records(t) {
					if record["name"] == name && record["msg"] == "envoy nats disconnected" {
						return record
					}
				}
				return nil
			}
			if tc.protocol {
				if !testwait.Holds(10*time.Second, 20*time.Millisecond, func() bool { return disconnect() != nil }) {
					t.Fatalf("no disconnect logged within 10s:\n%s", logs)
				}
				nc.Close()
				var mine []string
				for _, record := range logs.records(t) {
					if record["name"] == name {
						mine = append(mine, fmt.Sprint(record))
					}
				}
				for i := 0; i+10 <= len(marker); i++ {
					require.NotContains(t, strings.Join(mine, "\n"), marker[i:i+10], "a bus line prints a run of the made-up marker")
				}
				require.Equal(t, natsconn.ConnectionProtocolError, disconnect()["error"], "the disconnect's cause is not the protocol class")
				return
			}
			// The fake answers only the handshake's PING, so the control flushes nothing: the client
			// answers the server's PING, and the connection stays up.
			time.Sleep(time.Second)
			require.True(t, nc.IsConnected(), "the connection did not stay up on a valid PING")
			require.Nil(t, disconnect(), "a disconnect was logged on a valid PING")
		})
	}
}

// The async error line names its error by its class, as every bus line does: an error nats.go
// formatted from what the server sent (its parser's, quoting the bytes), or any other it has no
// class for, prints none of its text, and nats.go's own fixed words print as they are. The control
// is the slow consumer's error, which prints whole.
func TestTheAsyncErrorLinePrintsItsClass(t *testing.T) {
	const marker = "madeUpAsyncErrorMarker0123456789"
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"the parser's error, quoting the bytes the server sent", fmt.Errorf("nats: Parse Error [%d]: '%s'", 0, "XYZ "+marker), natsconn.ConnectionProtocolError},
		{"an error with no class", errors.New("websocket read of " + marker + " failed"), natsconn.ConnectionUnprinted},
		{"control: the slow consumer's error", nats.ErrSlowConsumer, nats.ErrSlowConsumer.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs()
			name := "async-class-" + strings.ReplaceAll(tc.name, " ", "-")
			options(logs.logger, name, []string{"nats://127.0.0.1:4222"}, nil, nil, nil, nil, nil).AsyncErrorCB(nil, nil, tc.err)
			var line map[string]any
			for _, record := range logs.records(t) {
				if record["name"] == name && record["msg"] == "envoy nats async error" {
					line = record
				}
			}
			require.NotNil(t, line, "no async error line:\n%s", logs)
			for i := 0; i+10 <= len(marker); i++ {
				require.NotContains(t, fmt.Sprint(line), marker[i:i+10], "the async error line prints a run of the made-up marker")
			}
			require.Equal(t, tc.want, line["error"])
		})
	}
}
