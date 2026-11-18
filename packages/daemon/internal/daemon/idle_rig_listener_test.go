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
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testbuild"
	"github.com/sjawhar/legion/daemon/internal/testtmp"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// idleRigListener is the real Envoy listener of packages/envoy/cmd/listener, built once per test
// process (testbuild.EnvoyBinary) and run as its own process for the rig: the one component that
// sees a message published to a role topic no live session holds and a message sent to the agent
// subject of a session that is gone, and reports both to the daemon on notifications.legion.wake.
//
// It is started the way scripts/e2e/envoy-session-delivery.sh starts one — a build of this
// checkout's cmd/listener, then PORT, ENVOY_LISTEN_HOST, ENVOY_MACHINE_ID, NATS_URLS and a HOME of
// its own, waited on until /healthz says healthy — and serves on a listening socket the test
// holds (ENVOY_LISTEN_FD), as the listener's own harness starts it (cmd/listener's
// startListenerProcess).
type idleRigListener struct {
	t   *testing.T
	url string
	ctx context.Context
	// socket is the listening socket the listener inherits as ENVOY_LISTEN_FD, which the test
	// holds from its bind until the test ends (heldSocket): no other program can take the port,
	// before the first listener serves on it or while a restarted one is coming back.
	socket *os.File
	logs   string
	cmd    *exec.Cmd
	client *http.Client
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

// startIdleRigListenerOn is startIdleRigListener on socket, listening on port, which the test
// holds: a listener restarted mid-test serves on the socket the first one served on, so it comes
// back where every URL already handed out reaches it.
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
		"HOME="+home, "PORT="+fmt.Sprint(port), "ENVOY_LISTEN_HOST=127.0.0.1",
		"ENVOY_MACHINE_ID="+machine, "NATS_URLS="+natsURL,
		// ExtraFiles' first entry is descriptor 3 in the listener.
		"ENVOY_LISTEN_FD=3")
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
	// Every request the rig makes of the listener carries the shared wait's bound
	// (testwait.Context) and no timeout of the rig's own, so a listener a loaded machine answers
	// late is answered.
	l := &idleRigListener{t: t, url: "http://127.0.0.1:" + fmt.Sprint(port), ctx: testwait.Context(t), socket: socket, logs: logs, cmd: cmd, port: port,
		client: &http.Client{}}
	// The stop kills the process (exec's default Cancel, SIGKILL) and returns only once it has
	// ended, so its NATS clients are gone before the test's server checks that it holds none
	// (testnats' leak check). It waited a fixed 15 s before, and on a loaded machine (load5 1,100
	// to 2,000) a killed listener had not ended when that span did: the stop returned, the leak
	// check found the listener's clients still on the server, and the next test to take the server
	// failed for them. A SIGKILL cannot be caught, so only the kernel can make a killed process end
	// late, and the wait is the shared one: its failure names the process's state.
	var once sync.Once
	l.stop = func() {
		once.Do(func() {
			cancel()
			pid := cmd.Process.Pid
			testwait.Until(t, fmt.Sprintf("the Envoy listener (pid %d) to end after its kill (the reading is its state in /proc)", pid), func() (string, bool) {
				select {
				case <-done:
					return "ended", true
				default:
					return idleRigProcessState(pid), false
				}
			})
		})
	}
	t.Cleanup(l.stop)
	defer func() {
		if t.Failed() {
			t.Logf("the Envoy listener's own log:\n%s", l.log())
		}
	}()
	testwait.Eventually(t, "the Envoy listener answering /healthz healthy", func() bool {
		request, err := http.NewRequestWithContext(l.ctx, http.MethodGet, l.url+"/healthz", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := l.client.Do(request)
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

// idleRigProcessState is what /proc says of a process that has not ended: its State line (`D` is
// a wait in the kernel that a kill does not interrupt) and the kernel function it waits in.
func idleRigProcessState(pid int) string {
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return fmt.Sprintf("/proc/%d/status: %v", pid, err)
	}
	state := "no State line"
	for line := range strings.SplitSeq(string(status), "\n") {
		if strings.HasPrefix(line, "State:") {
			state = strings.TrimSpace(line)
		}
	}
	wchan, err := os.ReadFile(fmt.Sprintf("/proc/%d/wchan", pid))
	if err != nil {
		return state
	}
	return state + ", wchan " + string(wchan)
}

// publish publishes one message to a topic, as a sender's envoy_publish does. It answers the
// listener's status and body, since a publish to a role topic no live session holds is answered
// 202 kept or 404, and either answer says what the listener found of the role's holder.
func (l *idleRigListener) publish(body map[string]any) (int, string) {
	l.t.Helper()
	return l.call("/v1/messages/publish", body)
}

// send sends one message to a session's agent subject, as a sender's envoy_send does.
func (l *idleRigListener) send(body map[string]any) (int, string) {
	l.t.Helper()
	return l.call("/v1/messages/send", body)
}

// freeClaims removes a session's interest record and frees every role claim the session holds,
// as an operator's DELETE /v1/interests/<session> does (the listener's forced release). It is
// how the rig frees the claim a released session left standing at once: the listener's role
// claim reaper frees it only past the live window and at its own interval, neither of which the
// rig's listener shortens. It answers the listener's status and body.
func (l *idleRigListener) freeClaims(session string) (int, string) {
	l.t.Helper()
	request, err := http.NewRequestWithContext(l.ctx, http.MethodDelete, l.url+"/v1/interests/"+session, nil)
	if err != nil {
		l.t.Fatal(err)
	}
	return l.answer(request)
}

func (l *idleRigListener) call(path string, body map[string]any) (int, string) {
	l.t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		l.t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(l.ctx, http.MethodPost, l.url+path, bytes.NewReader(encoded))
	if err != nil {
		l.t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	return l.answer(request)
}

// answer makes one request of the listener and answers its status and body.
func (l *idleRigListener) answer(request *http.Request) (int, string) {
	l.t.Helper()
	response, err := l.client.Do(request)
	if err != nil {
		l.t.Fatalf("%s %s: %v", request.Method, request.URL.Path, err)
	}
	defer response.Body.Close()
	read, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		l.t.Fatal(err)
	}
	return response.StatusCode, string(read)
}
