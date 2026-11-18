package main

// Every `legion claims` command answers within its --wait bound: dispatch://INFRA-714 R10. A
// suspend and a retry of a starting worker did not answer in 120 s and 300 s, and an operator with
// no answer at all cannot tell a daemon that is working from one that is wedged.

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/clock/clocktest"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// slowDaemon never answers the claims routes: it holds every request until the test ends, as a
// daemon whose decision is behind a launch that takes minutes does. held receives once a request
// is held.
func slowDaemon(t *testing.T) (port string, held <-chan struct{}) {
	t.Helper()
	holding := make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case holding <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); server.Close() })
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Port(), holding
}

// operatorsOnAFakeClock has every operator command the test runs measure its --wait, and a
// suspend's poll inside it, on a fake clock that moves only when the test moves it, and answers
// that clock.
func operatorsOnAFakeClock(t *testing.T) *clocktest.Fake {
	t.Helper()
	fake := clocktest.New(time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC))
	was := operatorClock
	operatorClock = fake
	t.Cleanup(func() { operatorClock = was })
	return fake
}

// legionAnswer is what one run of `legion` answered: its exit code and what it printed.
type legionAnswer struct {
	code      int
	out, errb string
}

// runLegion runs args as the `legion` command and answers what it did.
func runLegion(args []string) legionAnswer {
	var out, errb bytes.Buffer
	code := run(context.Background(), args, &out, &errb)
	return legionAnswer{code, out.String(), errb.String()}
}

// inFlight runs f on a goroutine of its own and answers the channel its answer arrives on, for a
// test that moves a fake clock while the call is in flight.
func inFlight[T any](f func() T) <-chan T {
	answered := make(chan T, 1)
	go func() { answered <- f() }()
	return answered
}

// Each subcommand takes a --wait that bounds the whole call, and a call that reaches it is
// refused with a reason naming the bound rather than hanging. The bound is on a fake clock, which
// the test moves to it once the daemon holds the command's request: a command its --wait does not
// bound never answers, and the wait for its answer fails.
func TestEveryClaimsSubcommandAnswersWithinItsWait(t *testing.T) {
	for name, args := range map[string][]string{
		"suspend": {"suspend", "--claim", string(architectClaim)},
		"resume":  {"resume", "--claim", string(architectClaim)},
		"stop":    {"stop", "--claim", string(architectClaim)},
		"close":   {"close", "--claim", string(architectClaim)},
		"retry":   {"retry", "--claim", string(architectClaim)},
		"deliver": {"deliver", "--claim", string(architectClaim), "--task", "carry on"},
		"list":    {"list"},
		"spawn":   {"spawn", "--tree", "LEGION-208", "--issue", "LEGION-208", "--role", "architect"},
	} {
		t.Run(name, func(t *testing.T) {
			clock := operatorsOnAFakeClock(t)
			port, held := slowDaemon(t)
			full := append([]string{"legion", "claims"}, args...)
			full = append(full, "--port", port, "--operator-token-file", operatorTokenFile(t), "--wait", "200ms")

			answered := inFlight(func() legionAnswer { return runLegion(full) })
			testwait.Receive(t, "the daemon to hold the command's request", held)
			if armed := clock.AwaitArmed(t, "the command's --wait bound", 1); !armed[0].Equal(clock.Now().Add(200 * time.Millisecond)) {
				t.Fatalf("the command armed %v, want its --wait bound of 200ms from %v", armed, clock.Now())
			}
			clock.Advance(200 * time.Millisecond)
			got := testwait.Receive(t, "the command to answer once its --wait bound ran out", answered)

			if got.code == 0 {
				t.Fatalf("the command exited 0 against a daemon that never answered; stdout %q", got.out)
			}
			if !strings.Contains(got.errb, "--wait bound of 200ms") {
				t.Fatalf("stderr is %q, want a refusal naming the --wait bound", got.errb)
			}
		})
	}
}

// --wait defaults to one minute, which is the bound a command with no flag of its own runs under.
func TestClaimsWaitDefaultsToOneMinute(t *testing.T) {
	clock := operatorsOnAFakeClock(t)
	c := newClaimsCall("suspend", &bytes.Buffer{}, &bytes.Buffer{})
	if code, ok := c.parse([]string{"--operator-token-file", operatorTokenFile(t)}); !ok {
		t.Fatalf("parse exited %d, want it to accept a command with no --wait", code)
	}
	if *c.wait != time.Minute {
		t.Fatalf("--wait defaults to %s, want one minute", *c.wait)
	}
	if want := clock.Now().Add(time.Minute); !c.until.Equal(want) {
		t.Fatalf("the call's bound is %s, want the minute it was given from its parse, %s", c.until, want)
	}
}

// The bound covers the whole call, not each request of it: a suspend the daemon answers 202 and
// then holds is refused once the command's own minute-or-less is gone, counted from its first
// request rather than restarted by each list it makes. On the fake clock the first poll after the
// first list already leaves too little of a 400 ms bound for another, so a bound restarted by that
// list would poll on, never answer, and fail the wait for its answer.
func TestTheWaitBoundsTheWholeSuspendCall(t *testing.T) {
	clock := operatorsOnAFakeClock(t)
	port, lists := heldSuspendDaemon(t, heldWorking)
	args := suspendArgs(t, port, "400ms")

	answered := inFlight(func() legionAnswer { return runLegion(args) })
	testwait.Eventually(t, "the command to wait out its poll after a list that found the suspension held", func() bool {
		return slices.Contains(clock.Armed(), clock.Now().Add(suspendPoll))
	})
	clock.Advance(suspendPoll)
	got := testwait.Receive(t, "the command to answer once its --wait bound left no room for another poll", answered)

	if got.code == 0 {
		t.Fatalf("suspend exited 0 while its suspension stayed held; stdout %q", got.out)
	}
	if !strings.Contains(got.errb, "--wait bound of 400ms") {
		t.Fatalf("stderr is %q, want a refusal naming the --wait bound", got.errb)
	}
	if got := lists(); got < 1 {
		t.Fatalf("the command listed the claims %d times, want it to have waited at all", got)
	}
}
