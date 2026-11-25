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
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/programbound"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/spf13/cobra"
)

// slowDaemon never answers the claims routes: it holds every request until the test ends, as a
// daemon whose decision is behind a launch that takes minutes does.
func slowDaemon(t *testing.T) string {
	t.Helper()
	held := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-held:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(held); server.Close() })
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Port()
}

// claimsAnswer is what one run of the legion command answered: its exit code and its streams.
type claimsAnswer struct {
	code      int
	out, errb string
}

// answerOf runs args as the legion command and waits for it to exit, with the test binary's
// deadline less 30 s as its one bound (testwait.Receive). A command its --wait never ends fails
// by what, and one a loaded machine makes slow passes: that the call returns at all, refused with
// a reason naming its --wait bound, which it prints only once that bound has run out, is what
// shows the --wait ended it.
func answerOf(t *testing.T, what string, args []string) claimsAnswer {
	t.Helper()
	done := make(chan claimsAnswer, 1)
	go func() {
		var out, errb bytes.Buffer
		code := run(context.Background(), args, &out, &errb)
		done <- claimsAnswer{code, out.String(), errb.String()}
	}()
	return testwait.Receive(t, what, done)
}

// Each subcommand takes a --wait that bounds the whole call, and a call that reaches it is
// refused with a reason naming the bound rather than hanging.
func TestEveryClaimsSubcommandAnswersWithinItsWait(t *testing.T) {
	t.Parallel()
	port := slowDaemon(t)
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
			full := append([]string{"legion", "claims"}, args...)
			full = append(full, "--port", port, "--operator-token-file", operatorTokenFile(t), "--wait", "200ms")

			answer := answerOf(t, "legion claims "+name+" with --wait 200ms against a daemon that never answers", full)

			if answer.code == 0 {
				t.Fatalf("the command exited 0 against a daemon that never answered; stdout %q", answer.out)
			}
			if !strings.Contains(answer.errb, "--wait bound of 200ms") {
				t.Fatalf("stderr is %q, want a refusal naming the --wait bound", answer.errb)
			}
		})
	}
}

// --wait defaults to one minute, and a command with no flag of its own runs under that default as
// the call reads it (programbound: in this binary, what is left of its deadline). The bound the
// call took (operatorCall.waited, the fact the call records) is read against programbound's value
// read just before the call and just after it: what is left of the deadline only shrinks, so the
// call's own read lies between the two, and a call that took any other value, a fixed minute
// among them, falls outside. No wall-clock span is read.
func TestClaimsWaitDefaultsToOneMinute(t *testing.T) {
	// Not parallel: it holds code to an upper bound on a measured time, which the load of tests running beside it could push past.
	cmd := &cobra.Command{Use: "suspend"}
	c := newClaimsCall(cmd, &bytes.Buffer{}, &bytes.Buffer{})
	if err := cmd.ParseFlags([]string{"--operator-token-file", operatorTokenFile(t)}); err != nil {
		t.Fatalf("the flags were refused: %v", err)
	}
	if *c.wait != time.Minute || defaultWait != time.Minute {
		t.Fatalf("--wait defaults to %s (defaultWait %s), want one minute", *c.wait, defaultWait)
	}
	most := programbound.Of(&defaultWait)
	if code, ok := c.check(); !ok {
		t.Fatalf("check exited %d, want it to accept a command with no --wait", code)
	}
	least := programbound.Of(&defaultWait)
	if until := time.Until(c.until); c.waited > most || c.waited < least || until <= 0 || until > most {
		t.Fatalf("the call took a bound of %s, %s away, want the default as programbound gives it, from %s to %s", c.waited, until, least, most)
	}
}

// The bound covers the whole call, not each request of it: a suspend the daemon answers 202 and
// then holds is refused once the command's own minute-or-less is gone, counted from its first
// request rather than restarted by each list it makes. The lists it made say so without a clock:
// one every suspendPoll, so a call held to its 400 ms makes at most 400 ms / suspendPoll of them
// and the two at its ends, and a slow machine makes fewer, never more; a call held to a later
// bound, such as the one-minute default, makes more.
func TestTheWaitBoundsTheWholeSuspendCall(t *testing.T) {
	t.Parallel()
	port, lists := heldSuspendDaemon(t, heldWorking)

	answer := answerOf(t, "legion claims suspend with --wait 400ms while its suspension stays held", []string{"legion", "claims", "suspend",
		"--port", port, "--operator-token-file", operatorTokenFile(t), "--claim", string(architectClaim), "--wait", "400ms"})

	if answer.code == 0 {
		t.Fatalf("suspend exited 0 while its suspension stayed held; stdout %q", answer.out)
	}
	if !strings.Contains(answer.errb, "--wait bound of 400ms") {
		t.Fatalf("stderr is %q, want a refusal naming the --wait bound", answer.errb)
	}
	if got, most := lists(), int(400*time.Millisecond/suspendPoll)+2; got < 1 || got > most {
		t.Fatalf("the command listed the claims %d times, want at least once and at most %d, the lists a call held to its 400 ms makes", got, most)
	}
}
