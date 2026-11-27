package testnats

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"

	"github.com/sjawhar/envoy/internal/testport"
)

// ProcessReady opens the line a helper process prints once it serves (StartProcess): what follows
// it on that line is the helper's answer to the test that started it, a URL say.
const ProcessReady = "testnats helper process ready: "

// Process is this test binary run again as one test of its own, a helper, in a process of its own
// (StartProcess). A test runs part of its rig there when that part must not share this process's
// Go scheduler: in a reconnect storm, nats-server's reading of the auth service's answers, the
// service's decisions and the storm's own clients all wait for the same few threads here, and the
// hub gave up on answers that had already reached it.
//
// The helper runs until its stdin closes, which Stop does and which this process's end does
// whatever ends it, so a helper never outlives the test binary that started it.
type Process struct {
	name string
	cmd  *exec.Cmd
	// ended closes once the helper's output has ended, which it does as the helper exits.
	ended chan struct{}

	mu      sync.Mutex
	out     strings.Builder
	stdin   io.WriteCloser
	waited  bool
	waitErr error
}

// StartProcess runs helper, a test of this binary, alone in a process of its own, with env added
// to this process's environment, and returns once the helper prints its ProcessReady line, with
// what follows it on that line. env must carry the variable the helper checks first, so that the
// helper runs only as a Process. A helper that ends before that line, or that has not printed it
// by the test's deadline (testDeadline), fails the test naming the helper and all it printed.
func StartProcess(t testing.TB, helper string, env ...string) (*Process, string) {
	t.Helper()
	p := &Process{name: helper, ended: make(chan struct{})}
	p.cmd = exec.Command(os.Args[0], "-test.run=^"+helper+"$", "-test.count=1", "-test.v")
	p.cmd.Env = append(os.Environ(), env...)
	stdin, err := p.cmd.StdinPipe()
	if err != nil {
		t.Fatalf("the helper process %s: %v", helper, err)
	}
	p.stdin = stdin
	stdout, err := p.cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("the helper process %s: %v", helper, err)
	}
	p.cmd.Stderr = p.cmd.Stdout
	if err := p.cmd.Start(); err != nil {
		t.Fatalf("start the helper process %s: %v", helper, err)
	}
	t.Cleanup(func() { p.stop(t) })
	ready := make(chan string, 1)
	go func() {
		defer close(p.ended)
		lines := bufio.NewScanner(stdout)
		for lines.Scan() {
			line := lines.Text()
			p.mu.Lock()
			p.out.WriteString(line + "\n")
			p.mu.Unlock()
			if answer, ok := strings.CutPrefix(line, ProcessReady); ok {
				select {
				case ready <- answer:
				default:
				}
			}
		}
	}()
	var deadline <-chan time.Time
	bound := boundOf(t)
	if bound.bounded {
		timer := time.NewTimer(time.Until(bound.end))
		defer timer.Stop()
		deadline = timer.C
	}
	select {
	case answer := <-ready:
		return p, answer
	case <-p.ended:
		select {
		case answer := <-ready:
			return p, answer
		default:
		}
		p.forget()
		t.Fatalf("the helper process %s (pid %d) ended before it was ready (%v); it printed:\n%s", helper, p.cmd.Process.Pid, p.wait(), p.Output())
	case <-deadline:
		_ = p.cmd.Process.Kill()
		<-p.ended
		p.forget()
		t.Fatalf("the helper process %s (pid %d) was not ready by %s, short of the test's deadline, and was killed (%v); it printed:\n%s", helper, p.cmd.Process.Pid, bound, p.wait(), p.Output())
	}
	return nil, ""
}

// forget drops the helper's stdin, so that the cleanup's stop of a helper the start already
// failed on reports nothing more: the start's failure named it.
func (p *Process) forget() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stdin = nil
}

// wait is the helper's exit, once its output has ended (ended): exec.Cmd.Wait closes the output
// pipe, so it runs only after the last read, and once.
func (p *Process) wait() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.waited {
		p.waited, p.waitErr = true, p.cmd.Wait()
	}
	return p.waitErr
}

// Output is all the helper has printed so far.
func (p *Process) Output() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.out.String()
}

// Stop closes the helper's stdin, which ends it, and returns once it has exited. A helper that
// fails, or that has not exited by the test's deadline (testDeadline), fails the test naming it.
// Stopping a stopped helper does nothing.
func (p *Process) Stop(t testing.TB) {
	t.Helper()
	p.stop(t)
}

func (p *Process) stop(t testing.TB) {
	t.Helper()
	p.mu.Lock()
	stdin := p.stdin
	p.stdin = nil
	p.mu.Unlock()
	if stdin == nil {
		return
	}
	_ = stdin.Close()
	bound := boundOf(t)
	if !awaitSignal(bound, p.ended) {
		_ = p.cmd.Process.Kill()
		<-p.ended
		t.Errorf("the helper process %s (pid %d) had not exited by %s, short of the test's deadline, and was killed (%v); it printed:\n%s", p.name, p.cmd.Process.Pid, bound, p.wait(), p.Output())
		return
	}
	if err := p.wait(); err != nil {
		t.Errorf("the helper process %s (pid %d) failed (%v); it printed:\n%s", p.name, p.cmd.Process.Pid, err, p.Output())
	}
}

// AwaitStop returns once the test that started this helper (StartProcess) stops it, or ends.
func AwaitStop() { _, _ = io.Copy(io.Discard, os.Stdin) }

// hubProcessEnv names, in a hub process's environment, the directory of the hub it serves: the
// configuration (nats.conf), the client address (address) and the JetStream store. It is the
// variable ServeHubProcess checks first, so that the helper test serves nothing when it runs among
// its package's other tests.
const hubProcessEnv = "TESTNATS_HUB_PROCESS_DIR"

// HubProcess is a nats-server in a process of its own (Process), served by a helper test of the
// binary that calls ServeHubProcess. Its client URL stays the same across every Restart; each
// start gives it a new monitoring port, which MonitorURL reads.
type HubProcess struct {
	helper string
	dir    string
	url    string

	mu      sync.Mutex
	process *Process
	monitor string
}

// StartCalloutHubProcess is StartWithCalloutServer with the server in a process of its own, served
// by helper (ServeHubProcess): it returns the hub and its client URL once the hub admits the
// service's own user.
func StartCalloutHubProcess(t testing.TB, helper string, keys CalloutKeys, static ...StaticUser) (*HubProcess, string) {
	t.Helper()
	listener, err := testport.ListenLoopback()
	if err != nil {
		t.Fatalf("choose the hub process's address: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("free the hub process's address: %v", err)
	}
	h := &HubProcess{helper: helper, dir: t.TempDir(), url: "nats://" + address}
	if err := os.WriteFile(filepath.Join(h.dir, "address"), []byte(address), 0o600); err != nil {
		t.Fatalf("write the hub process's address: %v", err)
	}
	h.launch(t, CalloutConfig(keys, static...))
	awaitSeed(t, h.url, keys.ServiceSeed)
	return h, h.url
}

// URL is the hub's client URL, the same across every Restart.
func (h *HubProcess) URL() string { return h.url }

// MonitorURL is the monitoring endpoint of the hub's current process, where /varz and /connz are.
func (h *HubProcess) MonitorURL() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.monitor
}

// Restart stops the hub's process and starts a new one on config, at the same client address and
// on the same store, as an operator restarts a hub: every client and service connected to it is
// dropped, reconnects to the same URL and sends its subscriptions again.
func (h *HubProcess) Restart(t testing.TB, config string) {
	t.Helper()
	h.mu.Lock()
	process := h.process
	h.process, h.monitor = nil, ""
	h.mu.Unlock()
	process.Stop(t)
	h.launch(t, config)
}

func (h *HubProcess) launch(t testing.TB, config string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h.dir, "nats.conf"), []byte(config), 0o600); err != nil {
		t.Fatalf("write the hub process's configuration: %v", err)
	}
	process, monitor := StartProcess(t, h.helper, hubProcessEnv+"="+h.dir)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.process, h.monitor = process, monitor
}

// ServeHubProcess serves the hub of a HubProcess when this binary runs as one (hubProcessEnv), and
// skips otherwise. A package whose tests start a HubProcess names a test of its own that calls it,
// and passes that test's name to StartCalloutHubProcess. The hub runs as start runs one, behind no
// relay: it listens on the hub's own client address. It is never reloaded, so the race
// outliveResponseTimers keeps a reloaded server from does not arise in it.
func ServeHubProcess(t *testing.T) {
	dir := os.Getenv(hubProcessEnv)
	if dir == "" {
		t.Skip("serves a hub only in the process a HubProcess starts")
	}
	address, err := os.ReadFile(filepath.Join(dir, "address"))
	if err != nil {
		t.Fatalf("read the hub's address: %v", err)
	}
	host, port, err := net.SplitHostPort(string(address))
	if err != nil {
		t.Fatalf("the hub's address %q: %v", address, err)
	}
	opts, err := serverOptions(filepath.Join(dir, "nats.conf"), []string{"-js"}, filepath.Join(dir, "store"), host)
	if err != nil {
		t.Fatalf("read the hub's configuration: %v", err)
	}
	if opts.Port, err = strconv.Atoi(port); err != nil {
		t.Fatalf("the hub's port %q: %v", port, err)
	}
	srv, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("create the hub: %v", err)
	}
	log := &logCapture{}
	srv.SetLoggerV2(log, opts.Debug, opts.Trace, false)
	go srv.Start()
	if err := awaitReady(t, srv, log); err != nil {
		srv.Shutdown()
		srv.WaitForShutdown()
		t.Fatalf("the hub did not accept connections: %v: %s", err, log.String())
	}
	monitor := srv.MonitorAddr()
	if monitor == nil {
		t.Fatal("the hub has no monitoring listener")
	}
	fmt.Println(ProcessReady + "http://" + monitor.String())
	AwaitStop()
	srv.Shutdown()
	srv.WaitForShutdown()
}
