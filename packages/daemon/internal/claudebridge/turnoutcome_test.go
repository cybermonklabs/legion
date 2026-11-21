package claudebridge

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// reportingRig is a rig whose pane names a daemon: every turn the bridge ends is reported to the
// stub, which answers as the daemon's route does.
type reportingRig struct {
	*rig
	reports chan claim.TurnOutcomeRequest
	answer  func(claim.TurnOutcomeRequest) claim.TurnOutcomeResponse
	// daemon is the stub, and address where the pane is told it listens. listening is how the
	// stub of a daemon that is down at first comes to listen there (refusingPort).
	daemon    *httptest.Server
	address   string
	listening func() net.Listener
}

const testBootToken = "boot-token-of-this-launch"

// newReportingRig is a reportingRig whose daemon serves from the start.
func newReportingRig(t *testing.T, args ...string) *reportingRig {
	t.Helper()
	return startReportingRig(t, true, args...)
}

// newDownReportingRig is a reportingRig whose daemon is not serving: every connection to its
// address is refused, as a restarting daemon's is, until serve.
func newDownReportingRig(t *testing.T, args ...string) *reportingRig {
	t.Helper()
	return startReportingRig(t, false, args...)
}

func startReportingRig(t *testing.T, serving bool, args ...string) *reportingRig {
	t.Helper()
	r := &reportingRig{
		reports: make(chan claim.TurnOutcomeRequest, 16),
		answer:  func(claim.TurnOutcomeRequest) claim.TurnOutcomeResponse { return claim.TurnOutcomeResponse{} },
	}
	r.daemon = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/legion/v1/claims/turn-outcome" {
			t.Errorf("the bridge posted to %s, want the turn-outcome route", request.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read the bridge's report: %v", err)
			return
		}
		var report claim.TurnOutcomeRequest
		if err := json.Unmarshal(body, &report); err != nil {
			t.Errorf("decode the bridge's report %s: %v", body, err)
			return
		}
		r.reports <- report
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(r.answer(report))
	}))
	if serving {
		r.address = r.daemon.Listener.Addr().String()
		r.listen(t, r.daemon.Listener)
	} else {
		if err := r.daemon.Listener.Close(); err != nil {
			t.Fatal(err)
		}
		r.address, r.listening = refusingPort(t)
	}
	tokenFile := filepath.Join(t.TempDir(), "boot-token")
	if err := os.WriteFile(tokenFile, []byte(testBootToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.rig = newRigWithEnv(t, []string{
		"LEGION_DAEMON_URL=http://" + r.address,
		"LEGION_BOOT_TOKEN_FILE=" + tokenFile,
		"LEGION_GENERATION=4",
	}, args...)
	return r
}

// listen has the stub daemon serve on listener from now on.
func (r *reportingRig) listen(t *testing.T, listener net.Listener) {
	r.daemon.Listener = listener
	r.daemon.Start()
	t.Cleanup(r.daemon.Close)
}

// serve has the daemon of a newDownReportingRig serve at its address from now on.
func (r *reportingRig) serve() {
	r.t.Helper()
	r.listen(r.t, r.listening())
}

// refusingPort holds a loopback port bound and not listening for the rest of the test: every
// connection to it is refused, as a restarting daemon's port refuses it, and since the port stays
// bound no other socket of the machine is given it meanwhile, as a port a test closed to refuse
// connections and listened on again later could be. listen has the port listen from then on,
// and is the listener's only maker.
func refusingPort(t *testing.T) (address string, listen func() net.Listener) {
	t.Helper()
	syscall.ForkLock.RLock()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err == nil {
		syscall.CloseOnExec(fd)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatalf("make the daemon's socket: %v", err)
	}
	socket := os.NewFile(uintptr(fd), "the daemon's port")
	t.Cleanup(func() { _ = socket.Close() })
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatalf("bind the daemon's port: %v", err)
	}
	bound, err := syscall.Getsockname(fd)
	if err != nil {
		t.Fatalf("read the daemon's bound port: %v", err)
	}
	address = net.JoinHostPort("127.0.0.1", strconv.Itoa(bound.(*syscall.SockaddrInet4).Port))
	return address, func() net.Listener {
		t.Helper()
		if err := syscall.Listen(fd, syscall.SOMAXCONN); err != nil {
			t.Fatalf("listen on the daemon's port %s: %v", address, err)
		}
		listener, err := net.FileListener(socket)
		if err != nil {
			t.Fatalf("serve on the daemon's port %s: %v", address, err)
		}
		return listener
	}
}

// reported is the next turn the bridge reported.
func (r *reportingRig) reported() claim.TurnOutcomeRequest {
	r.t.Helper()
	select {
	case report := <-r.reports:
		return report
	case <-time.After(10 * time.Second):
		r.t.Fatal("the bridge reported no turn within 10s")
		return claim.TurnOutcomeRequest{}
	}
}

// untilTurnEnds reads the bridge's frames up to and including the turn's end, and the
// `prompt_result` behind it. A turn the daemon's prompt started ends with two frames, not one:
// `agent_end`, which names no prompt, because a turn Claude Code started by itself when a
// background task finished ends with one just like it, and then the `prompt_result` that says
// whose turn it was. Every turn of these cases is a prompt's, so leaving the second frame unread
// would hand it to the next read of the stream.
func (r *rig) untilTurnEnds() {
	r.t.Helper()
	for r.next()["type"] != "agent_end" {
	}
	r.expect("prompt_result", nil)
}

// The bridge reports every turn it ends, clean and refused alike, with its own sequence counted
// from 1 within the launch generation and the launch's boot token as its name: the daemon's count
// of refused turns is made of these reports and of nothing else. A step Claude Code ended with an
// error is the turn's refusal, carrying what it said.
func TestTheBridgeReportsEveryTurnItEndsCleanAndRefusedAlike(t *testing.T) {
	r := newReportingRig(t, "-p", "--resume", sessionID)

	r.prompt("p1", "say hello")
	r.expect("agent_start", nil)
	r.untilTurnEnds()

	clean := r.reported()
	want := claim.TurnOutcomeRequest{
		BootToken: testBootToken, Generation: 4, Sequence: 1, Outcome: claim.TurnClean,
	}
	if clean != want {
		t.Fatalf("the clean turn was reported %+v, want %+v", clean, want)
	}

	r.prompt("p2", "api-error-529 the provider refused")
	r.expect("agent_start", nil)
	r.untilTurnEnds()

	refused := r.reported()
	if refused.Outcome != claim.TurnRefused || refused.Sequence != 2 || refused.Generation != 4 {
		t.Fatalf("the refused turn was reported %+v, want the second turn of generation 4 refused", refused)
	}
	if refused.Message == "" || refused.BootToken != testBootToken {
		t.Fatalf("the refused turn was reported %+v, want the provider's message and the launch's boot token", refused)
	}
}

// What the daemon counts is the provider refusing a turn, keyed on the result's own terminal
// reason as the pinned Claude Code writes it (providerFailed): a step
// Claude Code ended with its own cap on the steps of one turn (error_max_turns) is the worker
// having worked, not the provider refusing, and a turn whose newest step ran is the provider
// having answered after all. Counting either would walk a working claim to the launch failure
// limit, fail it, and abandon the session it is working in (dispatch://INFRA-714).
func TestOnlyTheStepsTheProviderRefusedAreReportedRefused(t *testing.T) {
	r := newReportingRig(t, "-p", "--resume", sessionID)

	r.prompt("p1", "maxturns worked to the cap")
	r.expect("agent_start", nil)
	r.untilTurnEnds()

	if report := r.reported(); report.Outcome != claim.TurnClean || report.Message != "" {
		t.Fatalf("the turn Claude Code's own turn cap ended was reported %+v, want it clean", report)
	}

	// The second turn's step is refused with a delivery already queued behind it, so the bridge
	// sends that delivery as the next step of the same turn and the turn ends on a step that ran.
	// The order is the test's to fix, not a race to be hoped for: the delivery goes in first, the
	// bridge's own log says it is queued, and only then is the refusal let go.
	inbox := r.dialInbox()
	r.prompt("p2", "fail-when go")
	r.expect("agent_start", nil)
	inbox.deliver("ev1", "say the CI is green")
	r.logged("Envoy delivery ev1 waits for the next step")
	r.release("go")
	r.untilTurnEnds()

	if report := r.reported(); report.Outcome != claim.TurnClean || report.Message != "" {
		t.Fatalf("the turn whose last step ran was reported %+v, want it clean: the provider answered after all", report)
	}
}

// The other order, which is the rule the one above could be mistaken for: a step the provider
// refused with nothing queued behind it ends its turn, and that turn is reported refused. A
// delivery that arrives after it starts a turn of its own rather than continuing the one that
// ended, and the step that runs there is a turn of its own, reported clean — which is what clears
// the refused run the daemon counts (bridge.go's result).
func TestARefusalWithNothingQueuedEndsItsTurnAndTheNextDeliveryIsANewOne(t *testing.T) {
	r := newReportingRig(t, "-p", "--resume", sessionID)
	inbox := r.dialInbox()

	r.prompt("p1", "fail-when now")
	r.expect("agent_start", nil)
	r.release("now")
	r.untilTurnEnds()

	refused := r.reported()
	if refused.Outcome != claim.TurnRefused || refused.Sequence != 1 || refused.Message == "" {
		t.Fatalf("the refused turn was reported %+v, want the run's first turn refused with the provider's words", refused)
	}

	inbox.deliver("ev1", "say the CI is green")
	r.expect("agent_start", nil)
	for r.next()["type"] != "agent_end" {
	}

	clean := r.reported()
	if clean.Outcome != claim.TurnClean || clean.Sequence != 2 || clean.Message != "" {
		t.Fatalf("the delivery's turn was reported %+v, want the run's second turn clean: it is a turn of its own, and it clears the refusal", clean)
	}
	if answer := inbox.answer(); !answer.OK || answer.ID != "ev1" {
		t.Fatalf("the inbox answered %+v, want ev1 taken", answer)
	}
}

// A pane that names no daemon reports nothing and bridges as it always did: a bridge run by hand
// has no claim to report to.
func TestABridgeWithNoDaemonReportsNothingAndStillBridges(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)

	r.prompt("p1", "say hello")
	r.expect("agent_start", nil)
	r.untilTurnEnds()
	r.quiet()
}

// A report the daemon cannot take because it is not serving is not lost: the bridge waits for a
// daemon that refuses the connection, as a restarting one does, as long as an Oh My Pi worker's
// call waits (pi-shared's retryWhileDaemonRestarts, R1), and the report lands once the daemon
// serves again. The daemon here refuses for longer than the 15 s that one report's post was
// bounded by, on a port it holds for the whole run (refusingPort).
func TestATurnOutcomeWaitsForADaemonThatRefusesAndLandsOnceItServes(t *testing.T) {
	r := newDownReportingRig(t, "-p", "--resume", sessionID)

	r.prompt("p1", "say hello")
	r.expect("agent_start", nil)
	r.untilTurnEnds()
	// The daemon's port is bound and not listening meanwhile, so every post the bridge makes is
	// refused.
	time.Sleep(16 * time.Second)
	r.serve()

	select {
	case report := <-r.reports:
		want := claim.TurnOutcomeRequest{BootToken: testBootToken, Generation: 4, Sequence: 1, Outcome: claim.TurnClean}
		if report != want {
			t.Fatalf("the turn was reported %+v, want %+v", report, want)
		}
	case <-time.After(time.Minute):
		t.Fatalf("the daemon refused for 16 s and then served for a minute, and the turn's outcome never reached it; the bridge logged:\n%s", r.log)
	}
}
