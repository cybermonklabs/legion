package natsauth_test

import (
	"bufio"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/legion/daemon/internal/natsauth"
)

// connectName runs a server that speaks NATS's handshake alone, and answers the name the CONNECT
// of the one client that dials it carries: the name the server sees.
func connectName(t *testing.T) (url string, name <-chan string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	names := make(chan string, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write([]byte(`INFO {"server_id":"connection-name-test","version":"2.15.1","proto":1,"max_payload":1048576}` + "\r\n"))
		reader := bufio.NewReader(conn)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			switch {
			case strings.HasPrefix(line, "CONNECT "):
				var fields struct {
					Name string `json:"name"`
				}
				_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "CONNECT ")), &fields)
				names <- fields.Name
			case strings.HasPrefix(line, "PING"):
				_, _ = conn.Write([]byte("PONG\r\n"))
			}
		}
	}()
	return "nats://" + listener.Addr().String(), names
}

// TestTheDaemonsConnectionNameTheServerSeesEndsWithItsBuild holds the daemon's connection to its
// build after its name: legion@<revision>, which legacy-subjects-gate.sh's fact (a2) reads from the
// end of the name the hub's census of connections lists. A build nothing stamped names none.
func TestTheDaemonsConnectionNameTheServerSeesEndsWithItsBuild(t *testing.T) {
	const revision = "0123456789abcdef0123456789abcdef01234567"
	url, names := connectName(t)
	conn, err := natsauth.Connect([]string{url}, "", natsauth.ConnectionName("legion-daemon ACME", revision), nats.Timeout(5*time.Second), nats.NoReconnect())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	select {
	case name := <-names:
		if want := "legion-daemon ACME legion@" + revision; name != want {
			t.Fatalf("the server saw the connection named %q, want %q", name, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the server saw no CONNECT")
	}
	for _, unstamped := range []string{"", "0123456", "not-a-commit"} {
		if got := natsauth.ConnectionName("legion-daemon ACME", unstamped); got != "legion-daemon ACME" {
			t.Fatalf("ConnectionName with revision %q = %q, want the name alone", unstamped, got)
		}
	}
}
