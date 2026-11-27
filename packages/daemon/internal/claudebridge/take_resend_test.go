package claudebridge

// What the bridge sends again until the daemon answers (the plan's departure 7 and decision 8): a
// seen report and a take on each failure the bridge lists, a read that timed out in either shape
// the transport gives it, and a turn's end report on any transport failure; a coded refusal is an
// answer and is sent once, and a newer value ends the resend of the older take. The stand-in
// daemon and the taking rig are take_test.go's.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// A seen report and a take that get no answer from the daemon are sent again, the same request,
// until it answers (dispatch://INFRA-2811): on a refused, a reset and a closed connection, an
// unreachable host and network, a name that does not resolve, a dial and a read that timed out,
// an attempt with no answer within its bound, and a proxy's page answering an outage (a 408, a
// 429, a 500, a 502, a 503 and a 504: httpoutage.Is), whose body is not the daemon's refusal
// shape. Once the daemon answers, the task is taken once and runs once.
func TestATakeAndASeenReportAreSentAgainOnEachListedFailure(t *testing.T) {
	proxyStatus := func(status int) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "upstream connect error", status) }
	}
	// failedDial is a dial that fails as net.Dial fails with err, before the request leaves.
	failedDial := func(err error) func(context.Context, string, string) (net.Conn, error) {
		return func(_ context.Context, network, _ string) (net.Conn, error) {
			return nil, &net.OpError{Op: "dial", Net: network, Err: err}
		}
	}
	for _, tc := range []struct {
		name, why string
		// fault fails each request at the daemon's side; nil for a failure a request meets
		// before it reaches the daemon, which refused and dial make.
		fault   func(t *testing.T) func(http.ResponseWriter, *http.Request)
		refused bool
		// dial is what a dial of the daemon meets until the failure lifts.
		dial func(ctx context.Context, network, address string) (net.Conn, error)
	}{
		{name: "a refused connection", why: "the connection was refused", refused: true},
		{name: "a reset connection", why: "the connection was reset", fault: resetConnection},
		{name: "a closed connection", why: "the connection was closed", fault: func(t *testing.T) func(http.ResponseWriter, *http.Request) {
			return func(w http.ResponseWriter, _ *http.Request) {
				if conn := hijack(t, w); conn != nil {
					_ = conn.Close()
				}
			}
		}},
		{name: "an unreachable host", why: "the daemon's host was unreachable",
			dial: failedDial(os.NewSyscallError("connect", syscall.EHOSTUNREACH))},
		{name: "an unreachable network", why: "the daemon's network was unreachable",
			dial: failedDial(os.NewSyscallError("connect", syscall.ENETUNREACH))},
		{name: "a name that does not resolve", why: "the daemon's name did not resolve",
			dial: failedDial(&net.DNSError{Err: "no such host", Name: "legion-daemon", IsNotFound: true})},
		// The dialer's own timeout, as net.Dialer makes it: a deadline already past.
		{name: "a dial that timed out", why: "the connection timed out",
			dial: func(ctx context.Context, network, address string) (net.Conn, error) {
				dialer := net.Dialer{Deadline: time.Now().Add(-time.Second)}
				return dialer.DialContext(ctx, network, address)
			}},
		// The kernel's timeout of a connection whose far side stopped answering, met at the
		// read of the answer; the request goes nowhere.
		{name: "a read that timed out", why: "the connection timed out",
			dial: func(context.Context, string, string) (net.Conn, error) {
				local, far := net.Pipe()
				_ = far.Close()
				return timedOutConn{local}, nil
			}},
		{name: "no answer within the attempt's bound", why: "no answer within 200ms", fault: func(*testing.T) func(http.ResponseWriter, *http.Request) {
			return func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }
		}},
		{name: "a proxy's 408", why: "408 whose body is not the daemon's refusal shape", fault: func(*testing.T) func(http.ResponseWriter, *http.Request) {
			return proxyStatus(http.StatusRequestTimeout)
		}},
		{name: "a proxy's 429", why: "429 whose body is not the daemon's refusal shape", fault: func(*testing.T) func(http.ResponseWriter, *http.Request) {
			return proxyStatus(http.StatusTooManyRequests)
		}},
		{name: "a proxy's 500", why: "500 whose body is not the daemon's refusal shape", fault: func(*testing.T) func(http.ResponseWriter, *http.Request) {
			return proxyStatus(http.StatusInternalServerError)
		}},
		{name: "a 502 with no code", why: "502 whose body is not the daemon's refusal shape", fault: func(*testing.T) func(http.ResponseWriter, *http.Request) {
			return proxyStatus(http.StatusBadGateway)
		}},
		{name: "a 503 with no code", why: "503 whose body is not the daemon's refusal shape", fault: func(*testing.T) func(http.ResponseWriter, *http.Request) {
			return proxyStatus(http.StatusServiceUnavailable)
		}},
		{name: "a 504 with no code", why: "504 whose body is not the daemon's refusal shape", fault: func(*testing.T) func(http.ResponseWriter, *http.Request) {
			return proxyStatus(http.StatusGatewayTimeout)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newTaskDaemon(t)
			calls, url := quickCalls, ""
			var lift func()
			switch {
			case tc.refused:
				// The daemon's address, which nothing listens on until the daemon starts.
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				addr := listener.Addr().String()
				_ = listener.Close()
				url = "http://" + addr
				lift = func() { d.startAt(addr) }
			case tc.dial != nil:
				var mu sync.Mutex
				lifted := false
				calls.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
					mu.Lock()
					up := lifted
					mu.Unlock()
					if !up {
						return tc.dial(ctx, network, address)
					}
					var dialer net.Dialer
					return dialer.DialContext(ctx, network, address)
				}
				lift = func() {
					mu.Lock()
					defer mu.Unlock()
					lifted = true
				}
			default:
				d.setFault(tc.fault(t))
				lift = func() { d.setFault(nil) }
			}
			r := newTakingRig(t, d, url, calls)
			r.handReady()

			r.handTask(testTask, 1)
			r.logged("claims/task-seen of task 7 got no answer from the daemon (" + tc.why + ")")
			r.logged("claims/take of task 7 got no answer from the daemon (" + tc.why + ")")
			lift()
			r.expect("agent_start", nil)
			r.untilTurnEnds()
			r.quiet()

			if seen := pairsOf(t, d.seenReports()); !slices.Equal(seen, []string{"7 at 1"}) {
				t.Errorf("the daemon answered the seen reports %q, want one", seen)
			}
			if takes := pairsOf(t, d.takesMade()); !slices.Equal(takes, []string{"7 at 1"}) {
				t.Errorf("the daemon answered the takes %q, want one", takes)
			}
			if report := d.reportOf(1); report.Task != testTask {
				t.Errorf("the turn was reported %+v, want the turn of task %d", report, testTask)
			}
			if texts := r.userTexts(); !slices.Equal(texts, []string{"say the task"}) {
				t.Errorf("Claude Code read %q, want the task's text once", texts)
			}
		})
	}
}

// One read that timed out reaches the bridge in either of two shapes, as net/http's transport
// decides by a race between its read and write loops: the read's own error under the client's
// *url.Error, or that error wrapped first as "readLoopPeekFailLocked: %w". The tester's probe in
// round 4 of dispatch://INFRA-2811, 400 attempts of the bridge's own attempt through
// timedOutConn, met the second shape 1 to 2 times in 400, and TestATakeAndASeenReportAreSentAgainOnEachListedFailure's
// "a read that timed out" fails whenever it meets it. Both are a read that timed out, which the
// bridge sends again until the daemon answers; a take sent once and lost so is charged at the
// first bound once its seen report reached the daemon.
func TestAReadThatTimedOutIsSentAgainInEitherShapeTheTransportGivesIt(t *testing.T) {
	read := &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ETIMEDOUT)}
	d := &daemonLink{calls: daemonCalls{answerTimeout: time.Second}}
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "the read's own error", err: &url.Error{Op: "Post", URL: "http://daemon/legion/v1/claims/take", Err: read}},
		{name: "the read's error wrapped by the transport's read loop", err: &url.Error{Op: "Post", URL: "http://daemon/legion/v1/claims/take",
			Err: fmt.Errorf("readLoopPeekFailLocked: %w", read)}},
	} {
		if why := d.unanswered(0, nil, tc.err); why != "the connection timed out" {
			t.Errorf("%s (%v): unanswered = %q, want \"the connection timed out\": a read that timed out is sent again until the daemon answers", tc.name, tc.err, why)
		}
	}
}

// A refusal the daemon made names a Legion error code, whatever its status, and is an answer: a
// seen report and a take refused so are sent once and never again, and the take holds the queue
// back no more.
func TestACodedRefusalIsNotSentAgain(t *testing.T) {
	d := newTaskDaemon(t)
	d.answerSeen = func(claim.TaskSeenRequest) (int, any) {
		return http.StatusConflict, map[string]string{"code": "claim_superseded", "error": "this launch is not the claim's current one"}
	}
	d.answerTake = func(claim.TakeRequest) (int, any) {
		return http.StatusServiceUnavailable, map[string]string{"code": "daemon_stopping", "error": "the daemon is stopping"}
	}
	r := newTakingRig(t, d, "", quickCalls)
	r.handReady()
	inbox := r.dialInbox()

	r.handTask(testTask, 1)
	r.logged("the take of task 7 failed (the daemon refused it: 503 daemon_stopping: the daemon is stopping)")
	r.logged("the daemon refused the seen report of task 7 at revision 1: 409 claim_superseded")
	// Ten of the longest waits a re-send would take pass, and nothing is sent again.
	time.Sleep(10 * quickCalls.resendMax)
	if seen, takes := len(d.seenReports()), len(d.takesMade()); seen != 1 || takes != 1 {
		t.Fatalf("the daemon was sent %d seen reports and %d takes, want one of each", seen, takes)
	}
	if strings.Contains(r.log.String(), "got no answer") {
		t.Fatalf("a coded refusal was taken for no answer:\n%s", r.log)
	}

	inbox.deliver("ev1", "say the card")
	r.logged("Envoy delivery ev1 starts a turn")
	r.expect("agent_start", nil)
	r.expect("agent_end", nil)
}

// A newer task line replaces the take still being sent for the older one: the older take is sent no
// more, whether the newer value names a task — taken and run ahead of the card that waited — or the
// value was deleted, which lets the card go.
func TestANewerTaskLineEndsTheResendOfTheOlderTake(t *testing.T) {
	for _, tc := range []struct {
		name   string
		number uint64
		texts  []string
	}{
		{name: "a newer value", number: testTask + 1, texts: []string{"say the newer task", "say the card"}},
		{name: "a deleted value", number: 0, texts: []string{"say the card"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newTaskDaemon(t)
			d.answerTake = func(take claim.TakeRequest) (int, any) {
				if take.Revision == 1 {
					return http.StatusServiceUnavailable, nil
				}
				return accepted("say the newer task")
			}
			r := newTakingRig(t, d, "", quickCalls)
			r.handReady()
			inbox := r.dialInbox()

			r.handTask(testTask, 1)
			r.waitFor("the take to be sent again", func() bool { return d.takesAt(1) >= 2 })
			inbox.deliver("ev1", "say the card")
			r.logged("Envoy delivery ev1 waits for the take of the claim's task")

			r.handTask(tc.number, 2)
			r.logged("the take of task 7 at revision 1 is replaced by a newer value and sent no more")
			r.expect("agent_start", nil)
			r.untilTurnEnds()
			older := d.takesAt(1)
			// Ten of the longest waits a re-send would take pass, and the older take is not sent.
			time.Sleep(10 * quickCalls.resendMax)
			if again := d.takesAt(1); again != older {
				t.Fatalf("the take at revision 1 was sent %d times more after the newer value", again-older)
			}
			if texts := r.userTexts(); !slices.Equal(texts, tc.texts) {
				t.Fatalf("Claude Code read %q, want %q", texts, tc.texts)
			}
		})
	}
}

// The end report of a task's turn is what finishes the task, so it is sent until the daemon
// answers it (the plan's decision 8): its first attempt, lost to a reset connection, and its
// second, which the daemon answered 500 — its failure to record the report — are sent again, the
// same report, and the third lands. The task finishes once: one report of its turn is recorded,
// and its text ran once.
func TestTheFirstEndReportDroppedThenTheTaskFinishesOnce(t *testing.T) {
	d := newTaskDaemon(t)
	d.answerReports(resetConnection(t), func(w http.ResponseWriter, _ *http.Request) {
		writeAnswer(w, http.StatusInternalServerError, map[string]string{"error": "turn outcome failed: the daemon could not record it"})
	})
	r := newTakingRig(t, d, "", quickCalls)
	r.handReady()

	r.handTask(testTask, 1)
	r.expect("agent_start", nil)
	r.untilTurnEnds()
	r.logged("claims/turn-outcome of turn 1 got no answer from the daemon (the connection was reset)")
	if report := d.reportOf(1); report.Task != testTask || report.Outcome != claim.TurnClean {
		t.Fatalf("the daemon recorded %+v, want the clean report of task %d's turn, the bridge's first", report, testTask)
	}
	r.quiet()

	attempts := d.reportsAttempted()
	if len(attempts) != 3 || attempts[1] != attempts[0] || attempts[2] != attempts[0] {
		t.Fatalf("the bridge sent the reports %+v, want the one report three times", attempts)
	}
	if takes := pairsOf(t, d.takesMade()); !slices.Equal(takes, []string{"7 at 1"}) {
		t.Errorf("the bridge asked to take %q, want task 7 once", takes)
	}
	if texts := r.userTexts(); !slices.Equal(texts, []string{"say the task"}) {
		t.Errorf("Claude Code read %q, want the task's text once", texts)
	}
}

// An end report whose attempt fails with no answer the daemon gave is sent again whatever the
// transport names the failure, not only where unanswered's list names it: an attempt that never
// reached the daemon applied nothing, and one that did is applied once. Here something in the
// daemon's place answers what is no HTTP response, and then a response whose header is malformed,
// two failures no case of the list names; the third attempt lands, and the task finishes once.
func TestAnEndReportIsSentAgainOnATransportFailureNoListNames(t *testing.T) {
	notHTTP := func(answer string) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, _ *http.Request) {
			if conn := hijack(t, w); conn != nil {
				_, _ = conn.Write([]byte(answer))
				_ = conn.Close()
			}
		}
	}
	d := newTaskDaemon(t)
	d.answerReports(notHTTP("not an answer of HTTP\r\n\r\n"), notHTTP("HTTP/1.1 200 OK\r\nContent-Length: many\r\n\r\n"))
	r := newTakingRig(t, d, "", quickCalls)
	r.handReady()

	r.handTask(testTask, 1)
	r.expect("agent_start", nil)
	r.untilTurnEnds()
	r.logged("claims/turn-outcome of turn 1 got no answer from the daemon (the transport failed:")
	if report := d.reportOf(1); report.Task != testTask || report.Outcome != claim.TurnClean {
		t.Fatalf("the daemon recorded %+v, want the clean report of task %d's turn", report, testTask)
	}
	r.quiet()

	attempts := d.reportsAttempted()
	if len(attempts) != 3 || attempts[1] != attempts[0] || attempts[2] != attempts[0] {
		t.Fatalf("the bridge sent the reports %+v, want the one report three times", attempts)
	}
	if texts := r.userTexts(); !slices.Equal(texts, []string{"say the task"}) {
		t.Errorf("Claude Code read %q, want the task's text once", texts)
	}
}
