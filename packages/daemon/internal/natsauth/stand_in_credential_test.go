package natsauth_test

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// fakeServer serves one client, so another process's connection that it served first would take
// the scripted lines from the test's client and leave the test waiting. A connection that names
// no client of the test is closed and takes nothing: one that sends an HTTP request, one whose
// CONNECT names another client, and one that sends nothing. The connection whose CONNECT names
// the test's client, opened after all three, still gets its PONG and the scripted lines.
func TestAFakeServerServesOnlyTheClientOfItsTest(t *testing.T) {
	const then = "-ERR 'Unknown Protocol Operation'\r\n"
	address := strings.TrimPrefix(fakeServer(t, then), "nats://")
	dial := func() net.Conn {
		t.Helper()
		conn, err := net.Dial("tcp", address)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		if deadline, bounded := testwait.Context(t).Deadline(); bounded {
			if err := conn.SetDeadline(deadline); err != nil {
				t.Fatal(err)
			}
		}
		return conn
	}

	dial() // one that sends nothing, which holds up no other
	metadata := dial()
	if _, err := io.WriteString(metadata, "GET /computeMetadata/v1/instance/service-accounts/default/identity HTTP/1.1\r\nHost: metadata\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	other := dial()
	if _, err := io.WriteString(other, `CONNECT {"verbose":false,"name":"another-process"}`+"\r\nPING\r\n"); err != nil {
		t.Fatal(err)
	}
	answered, err := io.ReadAll(other)
	if err != nil {
		t.Fatalf("read the other client's answer: %v", err)
	}
	if strings.Contains(string(answered), "PONG") || strings.Contains(string(answered), then) {
		t.Fatalf("a client the test did not make was answered %q, want its connection closed after the server's INFO", answered)
	}

	name, err := json.Marshal(t.Name())
	if err != nil {
		t.Fatal(err)
	}
	own := dial()
	if _, err := io.WriteString(own, `CONNECT {"verbose":false,"name":`+string(name)+"}\r\nPING\r\n"); err != nil {
		t.Fatal(err)
	}
	lines := bufio.NewReader(own)
	var got []string
	for len(got) < 3 {
		line, err := lines.ReadString('\n')
		if err != nil {
			t.Fatalf("the test's own client read %q, then: %v", got, err)
		}
		got = append(got, line)
	}
	if !strings.HasPrefix(got[0], "INFO ") || got[1] != "PONG\r\n" || got[2] != then {
		t.Fatalf("the test's own client read %q, want INFO, PONG and %q", got, then)
	}
}
