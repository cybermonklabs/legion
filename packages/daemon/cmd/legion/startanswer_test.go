package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/registry"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// inheritedDescriptor is the descriptor number `legion start` reads out of LEGION_LISTEN_FD in
// this process: a copy of the held socket's, as a harness hands one over, with the holder's own
// descriptor left open — the harness keeps the port, and only the copy the start adopts is this
// process's to close. It returns the port the socket listens on.
func inheritedDescriptor(t *testing.T) int {
	t.Helper()
	holder, port := heldSocket(t)
	descriptor, err := syscall.Dup(int(holder.Fd()))
	if err != nil {
		t.Fatalf("copy the held socket's descriptor: %v", err)
	}
	t.Setenv(listenFDVariable, strconv.Itoa(descriptor))
	return port
}

// heldConfigOn is a legion.yaml for port whose load waits: its implement App's private key comes
// from a command that reads a pipe, and `config.Load` runs that command, so the start is held
// between the adoption of the inherited socket and everything the configuration decides. The
// returned function lets the load finish.
func heldConfigOn(t *testing.T, port int) (string, func()) {
	t.Helper()
	config := legionConfig(t, "LEGION", port)
	pipe := filepath.Join(filepath.Dir(config), "private-key")
	if err := syscall.Mkfifo(pipe, 0o600); err != nil {
		t.Fatalf("make the pipe the key command reads: %v", err)
	}
	appendTo(t, config, fmt.Sprintf(`projects:
  LEGION: {}
github_apps:
  implement: { app_id: "1", private_key_command: "cat %s" }
  review: { app_id: "2", private_key: review-test-key }
`, pipe))
	return config, func() {
		if err := os.WriteFile(pipe, []byte("implement-test-key\n"), 0o600); err != nil {
			t.Errorf("write the private key into the pipe: %v", err)
		}
	}
}

// A registration that arrives while `legion start` runs is answered from the moment the socket is
// adopted — before the configuration is read, which is why the start handler reads none
// (dispatch://INFRA-2883 R5). Without it the kernel completes the handshake on the inherited
// socket and the agent waits out its whole phase with no answer.
func TestAnAdoptedListenerAnswersFromTheMomentItIsAdopted(t *testing.T) {
	legionState(t)
	port := inheritedDescriptor(t)
	config, loadTheConfiguration := heldConfigOn(t, port)

	var stderr bytes.Buffer
	exited := make(chan int, 1)
	go func() { exited <- start(context.Background(), config, &stderr) }()

	base := "http://127.0.0.1:" + strconv.Itoa(port)
	client := &http.Client{Timeout: time.Second}
	var status int
	var body []byte
	if !testwait.Holds(30*time.Second, 20*time.Millisecond, func() bool {
		// The start is held inside its configuration's load: an answer that arrives while it runs
		// is the start handler's, and an exit before one is this test losing its subject.
		select {
		case code := <-exited:
			t.Fatalf("the start exited (%d) before the adopted socket answered; stderr = %q", code, stderr.String())
		default:
		}
		registration := strings.NewReader(`{"bootToken":"boot-token","sessionId":"ses_1","ompSessionFile":"/sessions/ses_1.jsonl","agentId":"ses_1"}`)
		request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, base+"/legion/v1/claims/register", registration)
		if err != nil {
			t.Fatalf("build the registration: %v", err)
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := client.Do(request)
		if err != nil {
			return false
		}
		defer response.Body.Close()
		status = response.StatusCode
		body, _ = io.ReadAll(response.Body)
		return true
	}) {
		t.Fatalf("the adopted socket answered nothing while the start ran; stderr = %q", stderr.String())
	}

	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d while the start runs; body %s", status, http.StatusServiceUnavailable, body)
	}
	var answer map[string]any
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	sentence, _ := answer["error"].(string)
	if len(answer) != 1 || !strings.Contains(sentence, "the daemon is starting") || !strings.Contains(sentence, "ask again") {
		t.Fatalf("the answer is %s, want the uncoded 503 naming the daemon's start", body)
	}

	loadTheConfiguration()
	select {
	case code := <-exited:
		// The configuration names a Postgres nothing answers, so this start refuses once it is
		// past the load; what it refuses on is beside the point of this test.
		if code != 1 {
			t.Fatalf("exit code = %d, want 1; stderr = %q", code, stderr.String())
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("the start never exited; stderr = %q", stderr.String())
	}
}

// A start that refuses after it has adopted the socket — here, a team another legion is already
// running — stops its own server, and with it this process's copy of the socket. The holder keeps
// the port, so the kernel goes on accepting: a caller then meets no answer at all, which is what
// the next process to serve the socket is for.
func TestARefusedStartLeavesAnInheritedSocketUnanswered(t *testing.T) {
	legions := legionState(t)
	port := inheritedDescriptor(t)
	config := legionConfig(t, "LEGION", port)
	claim(t, legions, registry.Entry{
		Team: "LEGION", ConfigPath: "/srv/legion.yaml", PID: runningLegion(t), Port: port,
		Bind: "127.0.0.1", StartedAt: time.Now().UTC(),
	})

	var stderr bytes.Buffer
	code := start(context.Background(), config, &stderr)

	if code != 1 || !strings.Contains(stderr.String(), "already running") {
		t.Fatalf("exit code = %d, stderr = %q; want the refusal of a team already running", code, stderr.String())
	}
	address := "127.0.0.1:" + strconv.Itoa(port)
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatalf("dial %s once the start refused: %v; the holder keeps the socket listening", address, err)
	}
	defer conn.Close()
	request := "POST /legion/v1/claims/register HTTP/1.1\r\nHost: " + address + "\r\nContent-Length: 0\r\n\r\n"
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatalf("write the registration: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("bound the read: %v", err)
	}
	answer := make([]byte, 1)
	read, err := conn.Read(answer)
	var timeout net.Error
	if read != 0 || !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("the read answered %d bytes and %v, want a read that times out: nothing serves an inherited socket a refused start left behind", read, err)
	}
}
