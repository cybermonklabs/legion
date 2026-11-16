package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/testbuild"
	"github.com/sjawhar/legion/daemon/internal/testtmp"
)

// idleRigListener is the real Envoy listener of packages/envoy/cmd/listener, built once per test
// process (testbuild.EnvoyBinary) and run as its own process for the rig: the one component that
// sees a message published to a role topic nobody holds and a message sent to the agent subject
// of a session that is gone, and reports both to the daemon on notifications.legion.wake.
//
// It is started the way scripts/e2e/envoy-session-delivery.sh starts one — a build of this
// checkout's cmd/listener, then PORT, ENVOY_LISTEN_HOST, ENVOY_MACHINE_ID, NATS_URLS and a HOME of
// its own, waited on until /healthz says healthy — on a listening socket the test holds for its
// whole run and hands every listener it starts as ENVOY_LISTEN_FD, so no other program can take the
// port, before the first start or between a stop and the next.
type idleRigListener struct {
	t      *testing.T
	url    string
	logs   string
	cmd    *exec.Cmd
	client *http.Client
	// socket is the test's copy of the listening socket, which each start hands over and port is
	// the port of.
	socket *os.File
	port   int
	stop   func()
}

// startIdleRigListener runs a listener of machine id against natsURL until t ends, and returns it
// once /healthz answers healthy.
func startIdleRigListener(t *testing.T, natsURL, machine string) *idleRigListener {
	t.Helper()
	socket, port := heldSocket(t)
	return startIdleRigListenerOn(t, natsURL, machine, socket, port)
}

// startIdleRigListenerOn is startIdleRigListener on the socket the test holds, listening on port,
// so a listener restarted mid-test comes back where every URL already handed out reaches it.
func startIdleRigListenerOn(t *testing.T, natsURL, machine string, socket *os.File, port int) *idleRigListener {
	t.Helper()
	binary := testbuild.EnvoyBinary(t, "./cmd/listener")
	// The listener's HOME is also where it runs, and the only path the test writes under it is
	// the log below.
	home := testtmp.Dir(t, "listener.log")
	logs := filepath.Join(home, "listener.log")
	file, err := os.Create(logs)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, binary)
	cmd.Dir = home
	cmd.Env = append(os.Environ(),
		"HOME="+home, "PORT="+fmt.Sprint(port), "ENVOY_LISTEN_HOST=127.0.0.1", "ENVOY_LISTEN_FD=3",
		"ENVOY_MACHINE_ID="+machine, "NATS_URLS="+natsURL)
	// ExtraFiles entry 0 is descriptor 3 in the listener. The test keeps its own copy open, so the
	// port stays held while no listener runs.
	cmd.ExtraFiles = []*os.File{socket}
	cmd.Stdout, cmd.Stderr = file, file
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start the Envoy listener: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = cmd.Wait()
	}()
	l := &idleRigListener{t: t, url: "http://127.0.0.1:" + fmt.Sprint(port), logs: logs, cmd: cmd, socket: socket, port: port,
		client: &http.Client{Timeout: 10 * time.Second}}
	var once sync.Once
	l.stop = func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(15 * time.Second):
				t.Error("the Envoy listener did not stop")
			}
		})
	}
	t.Cleanup(l.stop)
	defer func() {
		if t.Failed() {
			t.Logf("the Envoy listener's own log:\n%s", l.log())
		}
	}()
	idleRigEventually(t, idleRigListenerStartWait, "the Envoy listener answering /healthz healthy", func() bool {
		response, err := l.client.Get(l.url + "/healthz")
		if err != nil {
			return false
		}
		defer response.Body.Close()
		var body struct {
			Status string `json:"status"`
		}
		if json.NewDecoder(response.Body).Decode(&body) != nil {
			return false
		}
		return body.Status == "healthy"
	})
	return l
}

// log is the listener's own output so far, which a failure names.
func (l *idleRigListener) log() string {
	read, err := os.ReadFile(l.logs)
	if err != nil {
		return "(the listener's log could not be read: " + err.Error() + ")"
	}
	return string(read)
}

// publish publishes one message to a topic, as a sender's envoy_publish does. It answers the
// listener's status and body, since a publish to a role topic nobody holds is the 404 that
// reports the wake.
func (l *idleRigListener) publish(body map[string]any) (int, string) {
	l.t.Helper()
	return l.call("/v1/messages/publish", body)
}

// send sends one message to a session's agent subject, as a sender's envoy_send does.
func (l *idleRigListener) send(body map[string]any) (int, string) {
	l.t.Helper()
	return l.call("/v1/messages/send", body)
}

func (l *idleRigListener) call(path string, body map[string]any) (int, string) {
	l.t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		l.t.Fatal(err)
	}
	response, err := l.client.Post(l.url+path, "application/json", bytes.NewReader(encoded))
	if err != nil {
		l.t.Fatalf("POST %s: %v", path, err)
	}
	defer response.Body.Close()
	read, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		l.t.Fatal(err)
	}
	return response.StatusCode, string(read)
}
