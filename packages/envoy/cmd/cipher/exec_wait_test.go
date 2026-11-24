package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/cipher/record"
)

// waitRequestID is the one request waitBroker serves, and waitValue the value its grant releases
// for WAIT_ME.
const (
	waitRequestID = "req-wait"
	waitValue     = "wait-value"
)

// waitBroker is a fake broker with one request, WAIT_ME's waitRequestID, which turns granted
// grantAfter after its start: the moment the first status read reaches it, or, with
// afterFirstHold, the moment the first status read it held for its whole wait answers pending.
// With honoursWait it answers a status read as the broker does a GET /v1/requests/{id}?wait=N:
// held until the request is granted or N seconds pass, whichever is first; without it, it answers
// at once, as a broker from before the parameter does. It checks every proof as the broker does
// (proofKey.verify), so a client that signed its query would be refused. It marks (waitMark) each
// status read's arrival and answer and the release, so a row can time the client, another process,
// from the read that answered granted.
type waitBroker struct {
	*httptest.Server
	meter     *stallMeter
	client    chan int // the client's pid, once runWaiting has started it
	mu        sync.Mutex
	grantAt   time.Time  // when the request turns granted; zero until its start
	reads     []waitRead // each status read, in the order they reached the broker
	release   waitMark   // POST /v1/grants/{id}/values, zero until it comes
	badProofs int
}

// waitRead is one status read: the wait it asked ("" for none), its arrival and its answer, and
// whether it answered granted.
type waitRead struct {
	wait              string
	reached, answered waitMark
	granted           bool
}

// waitMark is a moment of the fake's: when it came, and how long the client's main thread, the
// one thread that carries its steps (backoffHook), had waited to run by then
// (clientStepThreadDelay), so a span between two marks can be cleared of both processes'
// hold-ups (requireClientStep).
type waitMark struct {
	at     time.Time
	client time.Duration
}

func newWaitBroker(t *testing.T, key *proofKey, honoursWait, afterFirstHold bool, grantAfter time.Duration) *waitBroker {
	t.Helper()
	b := &waitBroker{meter: startStallMeter(t), client: make(chan int, 1)}
	var publicURL string
	verified := func(w http.ResponseWriter, r *http.Request) bool {
		if err := key.verify(r, publicURL); err != nil {
			b.mu.Lock()
			b.badProofs++
			b.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"PROOF_INVALID","error":"proof invalid: htm/htu do not match the request"}`))
			return false
		}
		return true
	}
	state := func(granted bool) map[string]any {
		answer := map[string]any{"state": "pending", "grant_id": nil, "record_id": "rec-wait", "approver": "ada@example.com", "decided_at": nil, "decision": nil}
		if granted {
			answer["state"], answer["grant_id"] = "granted", "grant-wait"
		}
		return answer
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/requests", func(w http.ResponseWriter, r *http.Request) {
		if !verified(w, r) {
			return
		}
		writeJSON(w, map[string]any{
			"request_id": waitRequestID, "state": "pending", "grant_id": nil, "record_id": "rec-wait",
			"approver": "ada@example.com", "secrets": []map[string]string{{"name": "WAIT_ME", "decision": "approval"}},
		})
	})
	mux.HandleFunc("GET /v1/requests/"+waitRequestID, func(w http.ResponseWriter, r *http.Request) {
		if !verified(w, r) {
			return
		}
		read := waitRead{wait: r.URL.Query().Get("wait"), reached: b.mark(r)}
		b.mu.Lock()
		first := len(b.reads) == 0
		if first && !afterFirstHold {
			b.grantAt = read.reached.at.Add(grantAfter)
		}
		grantAt := b.grantAt
		b.mu.Unlock()
		if seconds, err := strconv.Atoi(read.wait); honoursWait && err == nil && seconds >= 1 && seconds <= record.MaxRequestWaitSeconds {
			hold := time.Duration(seconds) * time.Second
			if !grantAt.IsZero() {
				hold = min(time.Until(grantAt), hold)
			}
			select {
			case <-time.After(hold):
			case <-r.Context().Done():
				return
			}
		}
		b.mu.Lock()
		if b.grantAt.IsZero() {
			// afterFirstHold: this read is the first, and its hold has just ended undecided.
			b.grantAt = time.Now().Add(grantAfter)
		}
		granted := !time.Now().Before(b.grantAt)
		b.mu.Unlock()
		read.granted, read.answered = granted, b.mark(r)
		b.mu.Lock()
		b.reads = append(b.reads, read)
		b.mu.Unlock()
		writeJSON(w, state(granted))
	})
	mux.HandleFunc("POST /v1/grants/grant-wait/values", func(w http.ResponseWriter, r *http.Request) {
		if !verified(w, r) {
			return
		}
		release := b.mark(r)
		b.mu.Lock()
		b.release = release
		b.mu.Unlock()
		writeJSON(w, map[string]any{"values": map[string]string{"WAIT_ME": waitValue}, "expires_at": time.Now().Add(time.Hour)})
	})
	mux.HandleFunc("GET /v1/settings", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, fakeSettings)
	})
	// The handlers read publicURL on the server's goroutines, and the request that reaches them
	// comes from another process, which orders nothing in this one. So the URL is written before
	// the server starts: the listener is bound unstarted, its address read, and only then served.
	b.Server = httptest.NewUnstartedServer(mux)
	publicURL = "http://" + b.Listener.Addr().String()
	b.Start()
	t.Cleanup(b.Close)
	return b
}

// mark is the fake's mark of now, for request r. The moment is read first; the client's run delay
// once runWaiting has named the client, which a request of the client's can only precede by the
// time runWaiting takes to name it after starting it.
func (b *waitBroker) mark(r *http.Request) waitMark {
	m := waitMark{at: time.Now()}
	select {
	case pid := <-b.client:
		b.client <- pid
		m.client = clientStepThreadDelay(pid)
	case <-r.Context().Done():
	}
	return m
}

// clientStepThreadDelay is how long the main thread of process pid, the thread whose id is pid,
// has waited on a CPU's run queue, runnable but not running, since it started
// (/proc/<pid>/task/<pid>/schedstat, its second field): the time the machine starved the thread
// that carries the client's steps, which this process's stall meter cannot see. runWaiting's build
// locks the client's main goroutine, which reads each status answer and sends the release, to that
// thread (backoffHook). It is only that thread's: a sum over every thread of the client counts
// threads that waited at once, and other threads' waits for work of their own, as the step's, and
// so clears a step of more than the machine held it up. It is 0 where /proc does not say, as on
// platforms other than Linux, so nothing is cleared.
func clientStepThreadDelay(pid int) time.Duration {
	id := strconv.Itoa(pid)
	data, err := os.ReadFile(filepath.Join("/proc", id, "task", id, "schedstat"))
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0
	}
	ns, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return time.Duration(ns)
}

// requireClientStep fails t where the client, another process, took from from to to, two of the
// fake's marks, longer than slack beyond the time either process was held up on the step's own
// path: this process only where its hold-ups fell on the taking of either mark (the stall meter's
// heldAround), since a hold-up of this process while it only waited delays no step of the
// client, and the client by the run delay of the thread that carries its steps
// (clientStepThreadDelay). Clearing the step of all this process was held up across it would
// clear a client's own delay on a machine that holds this process up while the client runs. It
// answers how long the step took. what names the step for the failure's text. b.mu is held.
func (b *waitBroker) requireClientStep(t *testing.T, what string, from, to waitMark, slack time.Duration) time.Duration {
	t.Helper()
	took := to.at.Sub(from.at)
	held := min(took, b.meter.heldAround(from.at)+b.meter.heldAround(to.at))
	starved := to.client - from.client
	late := took - held - starved
	if late > slack {
		t.Fatalf("%s took %s; this process was held up %s on the fake's two marks and the client's step thread starved %s, so the client was %s late on its own, more than %s",
			what, took.Round(time.Millisecond), held.Round(time.Millisecond), starved.Round(time.Millisecond), late.Round(time.Millisecond), slack)
	}
	t.Logf("on time: %s took %s, %s held up here on the fake's two marks, the client's step thread starved %s, %s late on its own (slack %s)",
		what, took.Round(time.Millisecond), held.Round(time.Millisecond), starved.Round(time.Millisecond), late.Round(time.Millisecond), slack)
	return took
}

// waits is each status read's wait parameter, in order. b.mu is held.
func (b *waitBroker) waits() []string {
	waits := make([]string, len(b.reads))
	for i, read := range b.reads {
		waits[i] = read.wait
	}
	return waits
}

// requireReleaseFromTheGrantedRead fails t unless the client released the value straight from the
// answer of its last status read, the one that answered granted: the release, timed from that
// answer, was not late on its own (requireClientStep) by more than lateSlack. Timing from the
// read's answer rather than from the grant leaves out what the fake itself took to answer, this
// process's own wake. It answers that read. b.mu is held.
func (b *waitBroker) requireReleaseFromTheGrantedRead(t *testing.T) waitRead {
	t.Helper()
	if len(b.reads) == 0 || b.release.at.IsZero() {
		t.Fatalf("the broker served %d status reads (waits %q) and released at %v; want reads, then a release", len(b.reads), b.waits(), b.release.at)
	}
	last := b.reads[len(b.reads)-1]
	if !last.granted {
		t.Fatalf("the last status read answered pending (waits %q); want the release to follow the read that answered granted", b.waits())
	}
	b.requireClientStep(t, "the release after the read that answered granted", last.answered, b.release, lateSlack)
	return last
}

// backoffLogEnv names the file the build runWaiting makes records each backoff sleep of the exec
// form in (pollBackoff), one line a sleep, the duration it asked.
const backoffLogEnv = "AGENT_SECRETS_TEST_BACKOFF_LOG"

// backoffHook is the file runWaiting's build of agent-secrets adds to the package. It locks the
// main goroutine to the process's main thread (runtime.LockOSThread in an init function runs main
// on that thread), so the exec form's loop, which reads each status answer and sends the release,
// runs on that one thread, whose run delay the fake reads (clientStepThreadDelay). And it makes
// pollBackoff record each sleep in backoffLogEnv's file before it sleeps. No build of agent-secrets
// but this test's has it.
const backoffHook = `package main

import (
	"fmt"
	"os"
	"runtime"
	"time"
)

func init() {
	runtime.LockOSThread()
	path := os.Getenv("` + backoffLogEnv + `")
	if path == "" {
		return
	}
	sleep := pollBackoff
	pollBackoff = func(d time.Duration) {
		if f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			fmt.Fprintln(f, d)
			_ = f.Close()
		}
		sleep(d)
	}
}
`

// buildRecordingBackoff builds agent-secrets with backoffHook added to its package, through the go
// build overlay cmdtest passes (ENVOY_TEST_GO_OVERLAY), joined with one already set.
func buildRecordingBackoff(t *testing.T) string {
	t.Helper()
	pkg, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	hook := filepath.Join(dir, "backoff_hook.go")
	if err := os.WriteFile(hook, []byte(backoffHook), 0o600); err != nil {
		t.Fatal(err)
	}
	overlay := struct {
		Replace map[string]string
	}{Replace: map[string]string{}}
	if prior := os.Getenv("ENVOY_TEST_GO_OVERLAY"); prior != "" {
		data, err := os.ReadFile(prior)
		if err == nil {
			err = json.Unmarshal(data, &overlay)
		}
		if err != nil {
			t.Fatalf("reading ENVOY_TEST_GO_OVERLAY %s: %v", prior, err)
		}
	}
	overlay.Replace[filepath.Join(pkg, "zz_test_backoff_hook.go")] = hook
	data, err := json.Marshal(overlay)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "overlay.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ENVOY_TEST_GO_OVERLAY", path)
	return buildAgentSecrets(t)
}

// runWaiting runs `agent-secrets WAIT_ME --wait 1m -- sh -c …` against b, built to record its
// backoff sleeps (buildRecordingBackoff), names it to b, and fails t unless the child ran with the
// granted value. It answers the backoff sleeps the run asked for, in order.
func runWaiting(t *testing.T, b *waitBroker, key *proofKey) []string {
	t.Helper()
	binary := buildRecordingBackoff(t)
	backoffs := filepath.Join(t.TempDir(), "backoffs")
	cmd := exec.Command(binary, "WAIT_ME", "--wait", "1m", "--", "sh", "-c", `test "$WAIT_ME" = `+waitValue+` && echo ran-the-child`)
	cmd.Env = append(append(os.Environ(), hostIsolation(t)...), "AGENT_SECRETS_URL="+b.URL, "AGENT_SECRETS_KEY_DIR="+key.keyDir, backoffLogEnv+"="+backoffs)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start agent-secrets: %v", err)
	}
	b.client <- cmd.Process.Pid
	err := cmd.Wait()
	exit := 0
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		exit = exitErr.ExitCode()
	case err != nil:
		t.Fatalf("run agent-secrets: %v (stderr %q)", err, stderr.String())
	}
	b.mu.Lock()
	badProofs := b.badProofs
	b.mu.Unlock()
	if badProofs != 0 {
		t.Fatalf("the broker refused %d proofs: a proof signs the path alone, never the query (stderr %q)", badProofs, stderr.String())
	}
	if exit != 0 || !strings.Contains(stdout.String(), "ran-the-child") {
		t.Fatalf("exit %d, stdout %q, stderr %q; want the child run with the granted value", exit, stdout.String(), stderr.String())
	}
	data, err := os.ReadFile(backoffs)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(data))
}

// TestExecLearnsOfTheGrantWithinASecond: against a broker that holds each `wait` read, a request
// granted 3 s after its first status read reached the broker is learnt of from a read the broker
// held across the grant, the read the grant ended, and its value released straight from that
// read's answer (R1), with no backoff sleep, in at most 2 status reads, each asking a wait of 1 to
// record.MaxRequestWaitSeconds seconds. The release is timed from that answer, not from the grant
// on the real clock, and cleared of the time either process was held up (requireClientStep), so
// neither the fake's own wake nor a machine that starved the client fails it. The loop before this
// change read at 2 s and 6 s, without a wait, so it learnt of the grant from a read made after it,
// which fails the row whatever the timing.
func TestExecLearnsOfTheGrantWithinASecond(t *testing.T) {
	const grantAfter = 3 * time.Second
	key := newProofKey(t)
	b := newWaitBroker(t, key, true, false, grantAfter)
	backoffs := runWaiting(t, b, key)
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(backoffs) != 0 {
		t.Errorf("the client slept its backoff %q, against a broker that held each read; want no sleep (waits %q)", backoffs, b.waits())
	}
	last := b.requireReleaseFromTheGrantedRead(t)
	if !last.reached.at.Before(b.grantAt) {
		t.Errorf("the read that answered granted reached the broker %s after the grant (waits %q); want the read the grant ended, held across it", last.reached.at.Sub(b.grantAt).Round(time.Millisecond), b.waits())
	}
	if len(b.reads) > 2 {
		t.Errorf("the broker served %d status reads (waits %q), want at most 2", len(b.reads), b.waits())
	}
	for _, wait := range b.waits() {
		if seconds, err := strconv.Atoi(wait); err != nil || seconds < 1 || seconds > record.MaxRequestWaitSeconds {
			t.Errorf("a status read asked wait=%q, want a whole number of seconds from 1 to %d (waits %q)", wait, record.MaxRequestWaitSeconds, b.waits())
		}
	}
}

// TestExecLearnsOfAGrantAfterAFullHold: R1's usual case, a person who decides after the first
// held read has run its whole wait. The fake holds that read for all record.MaxRequestWaitSeconds
// and grants 300 ms after it answers pending. The client sleeps no backoff after a held answer:
// the build records each sleep its loop asks for (pollBackoff), and any sleep fails the row by
// name, whatever the machine's load. Its next read follows the full hold's answer, and the value
// is released straight from the answer of the read that answered granted, each step cleared of the
// time either process was held up (requireClientStep), in 2 status reads. A loop that slept its 2 s
// backoff after every pending answer, a held one too, asks for that sleep.
func TestExecLearnsOfAGrantAfterAFullHold(t *testing.T) {
	key := newProofKey(t)
	b := newWaitBroker(t, key, true, true, 300*time.Millisecond)
	backoffs := runWaiting(t, b, key)
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(backoffs) != 0 {
		t.Errorf("the client slept its backoff %q after a read held for its whole wait; want no sleep after a held answer (waits %q)", backoffs, b.waits())
	}
	full := strconv.Itoa(record.MaxRequestWaitSeconds)
	if len(b.reads) != 2 || b.reads[0].wait != full || b.reads[0].granted {
		t.Fatalf("the broker served status reads with waits %q; want 2, the first asking %s, the whole wait, and answered pending", b.waits(), full)
	}
	b.requireClientStep(t, "the read after the full hold", b.reads[0].answered, b.reads[1].reached, lateSlack)
	b.requireReleaseFromTheGrantedRead(t)
}

// TestTheWaitLimitEndsInsideTheClientsCallTimeout: a held read of the longest wait the broker
// takes ends before the whole-call timeout of the client every call goes out on (newClient's
// http.Client) cuts it. A timeout that fell to the limit or under it would cut every undecided
// held read, which the exec form reads as a failed read and answers with its backoff, so R1's
// second would become up to ten.
func TestTheWaitLimitEndsInsideTheClientsCallTimeout(t *testing.T) {
	timeout := newClient(cipheraddr.New("http://broker.invalid")).http.Timeout
	if limit := time.Duration(record.MaxRequestWaitSeconds) * time.Second; timeout <= limit {
		t.Fatalf("record.MaxRequestWaitSeconds is %s, want under the client's whole-call timeout (%s)", limit, timeout)
	}
}

// TestExecBacksOffAgainstABrokerThatIgnoresWait: a broker from before the parameter answers each
// read at once, pending until 9 s after the first status read reached it. The loop does not read
// it in a tight loop: an answer that is still pending well before the wait it asked has run out
// makes it sleep its old backoff (pollBackoff, which the build records), so the child runs after
// at most 6 reads. A client held up for the whole wait it asked (record.MaxRequestWaitSeconds)
// sees its first answer only after that wait has run out, so it rightly sleeps no backoff and
// learns of the grant from its second read: a run with no sleep fails only when it read more than
// twice.
func TestExecBacksOffAgainstABrokerThatIgnoresWait(t *testing.T) {
	key := newProofKey(t)
	b := newWaitBroker(t, key, false, false, 9*time.Second)
	backoffs := runWaiting(t, b, key)
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(backoffs) == 0 && len(b.reads) > 2 {
		t.Errorf("the client slept no backoff against a broker that answered each read at once, and read %d times (waits %q); want its backoff", len(b.reads), b.waits())
	}
	if len(b.reads) > 6 {
		t.Errorf("the broker served %d status reads (waits %q), want at most 6: the loop must back off from a broker that answers at once", len(b.reads), b.waits())
	}
}
