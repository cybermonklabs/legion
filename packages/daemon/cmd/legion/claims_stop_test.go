package main

// `legion claims stop` over a stop the daemon holds (rule two of T3 of dispatch://INFRA-2811's
// plan v150, row 39i of its state table): the daemon holds the operator's stop of a claim whose
// take's turn ended with its report still to come, and answers it 202 with the claim as it is, as
// it answers a held suspension (api's claimEvent). The command says the stop is held, naming the
// task and the bound, and waits for it as `claims suspend` waits for a held suspension.
//
// The negative control of each is e63fe1f5 (e63fe1f57842a88c48f18bee9054ef6b6a97d8a0), where
// `claims stop` is claimRequest("stop") (claims.go:37): it prints the claim the 202 answered, still
// idle, and exits 0 without a word of the hold and without waiting for the stop.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/api"
	legionclaim "github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// stoppedWorker is the claim the held-stop daemon answers about.
const stoppedWorker = legionclaim.Token("legion-legion-legion-209-implementer")

// stopSeen is one answer the list gives about stoppedWorker: its state, and the incarnation of the
// process it records, "" for none.
type stopSeen struct {
	state       string
	incarnation string
}

var (
	heldIdle    = stopSeen{"idle", "first"}
	stopRetired = stopSeen{"retired", ""}
)

// heldStopDaemon answers the stop of stoppedWorker 202 with the claim idle over its process,
// holding the task whose take's turn ended, and each later list with the next of answers, the last
// repeated (heldDaemon).
func heldStopDaemon(t *testing.T, answers ...stopSeen) (port string, lists func() int) {
	t.Helper()
	return heldDaemon(t, stoppedWorker, "stop", func(seen stopSeen) api.OperatorClaim {
		c := api.OperatorClaim{Token: stoppedWorker, Tree: "LEGION-208", Issue: "LEGION-209", Role: "implementer",
			State: seen.state, Generation: 1, Pending: &api.DeliveryView{ID: "delivery-7", Task: "implement the plan"}}
		if seen.incarnation != "" {
			c.Locator = &runtime.Locator{Runtime: "tmux", Claim: stoppedWorker, Incarnation: seen.incarnation}
		}
		return c
	}, heldIdle, answers...)
}

// stopAgainst runs `legion claims stop` of stoppedWorker against port, waiting up to wait, with any
// extra flags (claimsAgainst).
func stopAgainst(t *testing.T, port, wait string, extra ...string) (int, string, string) {
	t.Helper()
	return claimsAgainst(t, "stop", stoppedWorker, port, wait, extra...)
}

// Row 39i: a stop the daemon holds for the report of the take's turn (202) is said to be held,
// naming the task and the bound, and waited out: the command polls the claims until the claim is
// retired and prints it so, exiting 0.
func TestClaimsStopSaysAStopHeldForTheReportIsHeldAndWaitsForIt(t *testing.T) {
	t.Parallel()
	port, lists := heldStopDaemon(t, heldIdle, heldIdle, stopRetired)

	code, out, errb := stopAgainst(t, port, "10s")

	if code != 0 {
		t.Fatalf("stop exited %d printing %q; stderr %q", code, out, errb)
	}
	if want := "legion-legion-legion-209-implementer implementer LEGION-209 retired - 1 0 - - - -\n"; out != want {
		t.Fatalf("stop printed %q, want %q", out, want)
	}
	for _, want := range []string{
		"the stop of legion-legion-legion-209-implementer is held for the report of the turn of its task delivery-7",
		"the report wait's bound (worker_turn_start_timeout_seconds)",
		"counted from that turn's end, or from the reconnect or the stop where the daemon restarted or the worker's connection was cut since that turn ended",
		"the daemon's log line of the hold names when it comes (due)",
		"the reconnect deadline",
	} {
		if !strings.Contains(errb, want) {
			t.Fatalf("stop said %q, want it to say %q", errb, want)
		}
	}
	if got := lists(); got != 3 {
		t.Fatalf("the command listed the claims %d times, want until the third answer said retired", got)
	}
}

// Under --json the claim a held stop ended with prints as one JSON line, as every other --json
// answer does; the word of the hold goes to stderr.
func TestClaimsStopJSONPrintsTheStoppedClaimAsOneLine(t *testing.T) {
	t.Parallel()
	port, _ := heldStopDaemon(t, heldIdle, stopRetired)

	code, out, errb := stopAgainst(t, port, "10s", "--json")

	if code != 0 {
		t.Fatalf("stop exited %d; stderr %q", code, errb)
	}
	var got api.OperatorClaim
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.State != "retired" || !strings.HasSuffix(out, "}\n") || strings.Count(out, "\n") != 1 {
		t.Fatalf("stop --json printed %q (%v), want the retired claim as one JSON line", out, err)
	}
	if !strings.Contains(errb, "is held for the report") {
		t.Fatalf("stop --json said %q, want the word of the hold on stderr", errb)
	}
}

// Every other end stops the poll at once, exiting 1 and saying what happened: the claim failed or
// was suspended, or the process the stop waited on was let go with the claim not stopped, rather
// than waiting out --wait.
func TestClaimsStopStopsAtEveryOtherEndOfAHeldStop(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		seen stopSeen
		want string
	}{
		{stopSeen{"failed", ""}, "is failed, so the held stop was dropped and it is not stopped"},
		{stopSeen{"suspended", ""}, "is suspended, so the held stop was dropped and it is not stopped"},
		{stopSeen{"launching", ""}, "no longer holds the stop of legion-legion-legion-209-implementer, which is launching: the process the stop waited on was let go"},
		{stopSeen{"idle", "second"}, "no longer holds the stop of legion-legion-legion-209-implementer, which is idle: the process the stop waited on was let go"},
	} {
		t.Run(tc.seen.state+" "+tc.seen.incarnation, func(t *testing.T) {
			port, lists := heldStopDaemon(t, heldIdle, tc.seen)

			code, out, errb := stopAgainst(t, port, "1h")

			if code != 1 || out != "" {
				t.Fatalf("stop exited %d printing %q, want 1 and nothing printed", code, out)
			}
			if !strings.Contains(errb, tc.want) {
				t.Fatalf("stop said %q, want %q", errb, tc.want)
			}
			if got := lists(); got != 2 {
				t.Fatalf("the command listed the claims %d times, want it to stop at the second answer", got)
			}
		})
	}
}

// A stop still held when --wait runs out fails the command, naming the claim's state and why a held
// stop can outlast it.
func TestClaimsStopFailsWhenTheHeldStopOutlastsTheWait(t *testing.T) {
	// Not parallel: its call has 1 s to be refused as still idle, time the load of tests running beside it could use up, which ends the call as unanswered instead.
	port, _ := heldStopDaemon(t, heldIdle)

	code, out, errb := stopAgainst(t, port, "1s")

	if code != 1 || out != "" {
		t.Fatalf("stop exited %d printing %q, want 1 and nothing printed", code, out)
	}
	if want := "is still idle after the --wait bound of 1s, its stop held for the report of its take's turn"; !strings.Contains(errb, want) {
		t.Fatalf("stop said %q, want %q", errb, want)
	}
}
