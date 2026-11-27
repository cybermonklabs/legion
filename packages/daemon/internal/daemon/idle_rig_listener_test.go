package daemon

//ci:builds packages/envoy ./cmd/listener ./internal/testnats/cmd/fixturebuckets startIdleRigListenerOn testbuild.EnvoyBinary and testbuild.ProvisionFixtureBuckets (`packages/daemon/internal/testbuild/testbuild.go#ProvisionFixtureBuckets`), which run go build ./cmd/listener and ./internal/testnats/cmd/fixturebuckets in packages/envoy (`packages/daemon/internal/testbuild/testbuild.go#buildEnvoy`)

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
	"time"

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
// checkout's cmd/listener, on a server fixturebuckets gave the buckets a listener copies, then
// PORT, ENVOY_LISTEN_HOST, ENVOY_MACHINE_ID, NATS_URLS and a HOME of its own, waited on until
// /healthz says healthy — but on a listening socket the test holds from the listener's first start
// until the test ends, handed to each start as ENVOY_LISTEN_FD, so no other program can take the
// port before the first start or between a stop and the next.
type idleRigListener struct {
	t   *testing.T
	url string
	// ctx bounds every request the rig makes of the listener (idleRigListenerContext).
	ctx context.Context
	// socket is the test's descriptor of the listening socket on port, which each start inherits
	// as ENVOY_LISTEN_FD.
	socket *os.File
	logs   string
	cmd    *exec.Cmd
	port   int
	stop   func()
}

// idleRigListenerSpare is how long before the test binary's deadline the rig's requests to the
// listener end: half of testwait's margin. Every wait of the rig gives up at that margin, 30 s
// before the deadline, and asks its question once more as it does, so a question that is a
// request of the listener must still be answered then: a request that ended with the waits would
// fail its parent test as a listener error from inside the wait, and the wait's own failure, which
// names what it waited for, would never be written. Ending 15 s before the deadline still leaves
// the failure and the cleanups time to run before Go's alarm.
const idleRigListenerSpare = 15 * time.Second

// idleRigListenerContext is the context of the rig's requests to the listener: cancelled when t
// ends, and ending idleRigListenerSpare before the test binary's deadline, after every wait of the
// rig has given up. Under -timeout 0 it has no deadline.
func idleRigListenerContext(t *testing.T) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	if deadline, bounded := t.Deadline(); bounded {
		ctx, cancel = context.WithDeadline(context.Background(), deadline.Add(-idleRigListenerSpare))
	}
	t.Cleanup(cancel)
	return ctx
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
	testbuild.ProvisionFixtureBuckets(t, natsURL)
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
	// The unplaceable window is ten seconds rather than the shipped minute: a copy stored on the
	// agent subject of a session with no record is held for that window before the listener gives
	// it up and reports the wake (cmd/listener's unplaceableAgentDelivery), so the wake report of a
	// pod woken that way comes ten seconds after the message rather than a minute after it, and
	// the pod comes back after its launch on top of that.
	cmd.Env = append(os.Environ(),
		"HOME="+home, "PORT="+fmt.Sprint(port), "ENVOY_LISTEN_HOST=127.0.0.1", "ENVOY_LISTEN_FD=3",
		"ENVOY_MACHINE_ID="+machine, "NATS_URLS="+natsURL, "ENVOY_UNPLACEABLE_RETRY_WINDOW=10s")
	// ExtraFiles' first entry is the listener's descriptor 3. The test keeps its own descriptor of
	// the socket open, so the port stays held while no listener runs.
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
	// Every request the rig makes of the listener carries idleRigListenerContext and no timeout of
	// the rig's own, so a listener a loaded machine answers late is answered, and a wait that asks
	// the listener as it gives up still has its answer.
	l := &idleRigListener{t: t, url: "http://127.0.0.1:" + fmt.Sprint(port), ctx: idleRigListenerContext(t), socket: socket, logs: logs, cmd: cmd, port: port}
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
	idleRigEventually(t, "the Envoy listener answering /healthz healthy", func() bool {
		request, err := http.NewRequestWithContext(l.ctx, http.MethodGet, l.url+"/healthz", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := http.DefaultClient.Do(request)
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
	response, err := http.DefaultClient.Do(request)
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
