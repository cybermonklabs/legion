package daemon

// The stale-plugin lane's own wave, over a fleet larger than the gate: the before-merge proof of
// checks c-bound and c-pace of dispatch://INFRA-2712/artifact/verification-md, driven through the
// daemon itself on the wave rig beside this file (wake_wave_test.go) and read from what an
// operator of a running wave reads — the schedule the state serves (api.State.WakesWaiting), the
// count the state serves beside it (api.State.ClaimsToRefresh), the moments the launches reach
// the runtime, and the one line each wake writes where it runs (supervise's wakeRan).
//
// What the rig next door drives is one admitted wave end to end; what these two drive is a fleet:
// a hundred and twenty live claims on an earlier plugin contract, which at the shipped rate of
// eight wakes a minute is fifteen minutes of refresh, with the launches of twelve roots the same
// boot raises queued in the same pacing. The gate (supervise.RefreshGate) is what holds the lane
// to one minute of that pacing at a time, and these cases are what say the operator can see it.
//
// Three things of the rig are this file's own, because only a fleet needs them: the worker shim
// that puts a seeded claim in a turn and takes it out of one during the wave (waveTurn), the
// settled readings the gate's bound is asserted on (settledRefreshes, decision P8 of
// dispatch://INFRA-2712/artifact/plan-md), and the pairing of each wake's line to the moment its
// launch ran (pacedWakeMoments), which is how the 60-second windows of check c-pace are counted
// from the lines the daemon writes rather than from the runtime's record alone.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

const (
	// refreshWaveStale is how many live claims the wave starts with on an earlier plugin
	// contract: check c-bound asks for at least 120, which at rate 8 is fifteen minutes of
	// refresh and the shape of the INFRA fleet of 2026-11-10, where one reading counted 395 live
	// processes on the old contract.
	refreshWaveStale = 120
	// refreshWaveRoots is how many roots the same boot raises a first architect launch for: the
	// wakes that start a tree's work, which are what the gate exists to keep out of the fleet's
	// queue. Check c-bound asks for at least 12.
	refreshWaveRoots = 12
	// refreshWaveTurns is how many of the stale claims are in a turn when the boot reads them,
	// and so take no admission at all until that turn ends (supervise's holdStaleRelaunch). Check
	// c-bound asks for at least 8.
	refreshWaveTurns = 8
	// refreshWaveRounded is the stale claim seeded in a pod update round, which the lane passes
	// over for as long as the round stands (refreshStalePlugins, InPodRound) and which the count
	// of claims left to refresh leaves out (claimsToRefresh).
	refreshWaveRounded = refreshWaveTurns + 1
	// refreshWaveSweep is the orphan sweep of these cases, on the wall clock as the daemon's own
	// ticker is: the sweep is what re-fills the gate, so a wave of fifteen minutes' refresh would
	// otherwise take fifteen minutes of the test's own time.
	//
	// It is half a second rather than the tenth the rig next door uses, because the gate's
	// correction rests on a sweep being long enough for the admissions of the pass before it to
	// have reached the machines' published views: the floor it holds is the admissions granted
	// since the previous correction, so a wake still unparked two corrections after it was
	// admitted is counted by neither and the gate hands its credit out a second time (reading 6
	// and decision P2 of dispatch://INFRA-2712/artifact/plan-md). On the shipped daemon that is
	// a minute against the milliseconds a post takes; at a tenth of a second, over a hundred and
	// thirty machines and the race detector, it is not, and the lane was measured holding the
	// rate and one.
	refreshWaveSweep = 500 * time.Millisecond
	// refreshWaveRounds is the most rounds of the gate the drain of the fleet may take, a round
	// being one sweep's worth of admissions run to their turns. The lane's own number is the
	// claims it has to refresh divided by the rate — 118 at 8 is 15 — and this allows two thirds
	// again on top of it, for the three things a round can spend on no relaunch: an admission
	// granted for a claim in a turn, which that claim gives back after the pass has walked on
	// (refreshPlugin's hold); the killed claim's own re-post; and a correction that keeps a
	// credit one sweep longer because the claim views it reads lag a decision in flight (reading
	// 6 and decision P2 of dispatch://INFRA-2712/artifact/plan-md). It is the drain assertion of
	// check c-bound: a lane that admitted one claim a sweep would need 118 of these rounds.
	refreshWaveRounds = 25
	// refreshNoInstallRole is the role whose launch installs no plugin on these daemons: a Claude
	// Code role under Kubernetes, whose plugin is the worker image's own claude-envoy and which
	// no install of this daemon's reaches (internal/daemon's noPluginInstall). The lane never
	// posts such a claim and the count never names it, which is what check c-bound reads it for.
	refreshNoInstallRole = claim.RoleReviewer
	// refreshWaveCluster is the API server the daemon's kubeconfig names. Nothing dials it: the
	// runtime these cases run on is the rig's own fake, so boot returns before it builds a
	// Sandbox runtime or checks a cluster (prepareSandbox), and the kubeconfig is read and never
	// used.
	refreshWaveCluster = "https://127.0.0.1:1"
	// observationUnderLaunchLine is the line a machine writes where it drops a report of the
	// process a waiting launch is about to replace, whatever that report found (supervise's
	// judge). It is how the case below reads what the daemon did with a killed process whose
	// claim holds an admitted refresh wake: nothing, because the wake's own turn answers it.
	observationUnderLaunchLine = "supervise: an observation of the process a waiting launch replaces decides nothing"
)

// refreshWaveConfig is waveConfig's daemon under Kubernetes, with the reviewer's launches on the
// Claude Code backend: the one thing a wave of this lane needs that tmux cannot give it is a
// claim whose launch installs no plugin, which the daemon reads from the runtime and the role's
// backend together and from nothing else (noPluginInstall). Everything else is waveConfig's: the
// shipped pace, and every other timer put out past the wave's own span.
func refreshWaveConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := kubernetesConfig(t, refreshWaveCluster)
	cfg.WorkerWakesPerMinute, cfg.WorkerWakeBurst = testWakeRate, testWakeBurst
	cfg.WorkerBootTimeout = time.Hour
	cfg.ProbeInterval = time.Hour
	cfg.WorkerBackends = map[claim.Role]runtime.Backend{refreshNoInstallRole: runtime.BackendClaude}
	return cfg
}

// startRefreshWaveDaemon is startWaveDaemon with the sweep these cases run on and with the plugin
// source a Kubernetes daemon would read from runtime.kubernetes.plugins, which no test
// configuration carries: without it this daemon reads itself as one that can relaunch onto no
// plugin at all, and the lane posts nothing whatever the gate holds (prepare's pluginRelaunchable,
// refreshStalePlugins).
func startRefreshWaveDaemon(t *testing.T, cfg config.Config, clock *waveClock, rt *waveRuntime, logged *syncBuffer) *daemon {
	t.Helper()
	relaunchable := true
	return startWaveDaemon(t, cfg, clock, rt, logged, func(o *overrides) {
		o.orphanSweep = refreshWaveSweep
		o.pluginRelaunchable = &relaunchable
	})
}

// waveStreamAddress is the worker stream this daemon serves, read from the line it writes when it
// starts: under Kubernetes the stream is a TCP address whose port the kernel chose at bind time
// (prepareSandbox, worker_stream_port 0 in testConfig), so the daemon's own line is where a test
// learns it. The daemon answers /healthz only after it has written that line, so a started daemon
// always has one.
func waveStreamAddress(t *testing.T, logged *syncBuffer) string {
	t.Helper()
	for line := range strings.Lines(logged.String()) {
		entry := map[string]any{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		if entry["msg"] != "legion daemon started" {
			continue
		}
		address, named := entry["workerStream"].(string)
		if !named || address == "" {
			t.Fatalf("the daemon's start line names the worker stream %v, want an address a shim can dial: %v", entry["workerStream"], entry)
		}
		return address
	}
	t.Fatal("the daemon wrote no start line, so this test cannot find the worker stream to drive a turn over")
	return ""
}

// waveTurn is one seeded claim's own agent, on the daemon's worker stream: the turn driver a
// fleet's wave needs, since the claims of this rig are driven through a whole daemon and a
// machine of it is not a test's to post to (internal/daemon's podupdate_test.go rig posts
// straight to the machine, which only a rig that builds its own supervisor can do).
//
// It is the shape of the live rigs' own claimants (daemon_test.go's dialShim,
// handoff_records_live_test.go's handoffClaimant): it dials the stream, says hello with the boot
// token the claim records, answers the protocol negotiation and the daemon's question about
// whether the turn an earlier daemon saw begin is still running (askTurnOver), and ends that turn
// when the test says so. Two frames are the whole of the turn: the daemon asks, this answers
// "streaming", and the agent_end that follows is the end of the turn (turnEnded).
type waveTurn struct {
	t     *testing.T
	token claim.Token
	conn  net.Conn
	lines *bufio.Reader
	// mu orders the writes of the serving goroutine against the test's own endTurn.
	mu     sync.Mutex
	writer *shimwire.Writer
	// running is whether the agent says it is in a turn, which the daemon's question is answered
	// from and which the turn's end clears.
	running bool
}

// dialWaveTurn connects an agent to the claim's own process over the daemon's worker stream and
// returns once the daemon has acknowledged its hello, with the turn running.
func dialWaveTurn(t *testing.T, address, bootToken string, token claim.Token) *waveTurn {
	t.Helper()
	network, socket := "tcp", strings.TrimPrefix(address, "tcp://")
	if path, isUnix := strings.CutPrefix(address, "unix://"); isUnix {
		network, socket = "unix", path
	}
	conn, err := net.Dial(network, socket)
	if err != nil {
		t.Fatalf("dial the worker stream %s for %s: %v", address, token, err)
	}
	t.Cleanup(func() { conn.Close() })
	w := &waveTurn{t: t, token: token, conn: conn, lines: bufio.NewReader(conn), writer: shimwire.NewWriter(conn), running: true}
	w.send(shimwire.Hello2{BootToken: bootToken})
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("set the read deadline of %s: %v", token, err)
	}
	line, err := w.lines.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read the daemon's answer to the hello of %s: %v", token, err)
	}
	frame, err := shimwire.Decode(line)
	if err != nil {
		t.Fatalf("decode the daemon's answer to the hello of %s: %v", token, err)
	}
	if frame.FrameType() != shimwire.TypeHelloAck {
		t.Fatalf("the daemon answered the hello of %s with %#v, want hello_ack", token, frame)
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("clear the read deadline of %s: %v", token, err)
	}
	go w.serve()
	return w
}

// serve answers the frames the daemon sends until the connection closes. A write that fails is
// left to the assertions that follow it — a turn that never ended, or a claim that never came
// back — since this runs on a goroutine of its own and the test may already be over.
func (w *waveTurn) serve() {
	for {
		line, err := w.lines.ReadBytes('\n')
		if err != nil {
			return
		}
		frame, err := shimwire.Decode(line)
		if err != nil {
			continue
		}
		switch f := frame.(type) {
		case shimwire.NegotiateProtocol:
			w.send(shimwire.Response{ID: f.ID, Command: shimwire.TypeNegotiateProtocol, Success: true})
		case shimwire.GetState:
			w.mu.Lock()
			streaming := w.running
			w.mu.Unlock()
			data, err := json.Marshal(shimwire.StateData{IsStreaming: &streaming})
			if err != nil {
				return
			}
			w.send(shimwire.Response{ID: f.ID, Command: shimwire.TypeGetState, Success: true, Data: data})
		}
	}
}

// send writes one frame, from the test's goroutine or the serving one.
func (w *waveTurn) send(frame shimwire.Frame) {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.writer.WriteFrame(frame)
}

// endTurn is the agent's turn over: the frame Oh My Pi writes at the end of one, which is what
// the daemon decides the held relaunch at (turnEnded, afterTurn).
func (w *waveTurn) endTurn() {
	w.mu.Lock()
	w.running = false
	w.mu.Unlock()
	w.send(shimwire.AgentEnd{})
}

// settledRefreshes is a settled reading of the refreshes the pacing holds, by the lane's own wake
// kind, with the gate's bound asserted on it: no reading of api.State.WakesWaiting holds more
// wakes of that kind than worker_wakes_per_minute (check c-bound, supervise.RefreshGate).
//
// Settled means two readings alike in a row, which is the discipline awaitWave and awaitGate use
// and which decision P8 of dispatch://INFRA-2712/artifact/plan-md asks of exactly this assertion:
// the machines publish their views at the end of each decision (supervise's publish, read by the
// daemon's waitingWakes), so a single reading taken while a decision is in flight can hold the
// wake of a claim whose credit is already free and would read K+1 for a gate that never granted
// more than K. what is the moment of the run this reading is of, named in the failure.
func settledRefreshes(t *testing.T, d *daemon, what string) []api.WakeWaiting {
	t.Helper()
	var waiting, settled []api.WakeWaiting
	reads := 0
	testwait.Eventually(t, "a settled reading of the refreshes the pacing holds at "+what, func() bool {
		waiting, settled = refreshWakesWaiting(t, d), waiting
		reads++
		return reads > 1 && slices.Equal(waiting, settled)
	})
	if len(waiting) > testWakeRate {
		t.Fatalf("the lane holds %d refreshes in the pacing at %s, want at most the rate, %d: the gate grants one minute of the pacing at a time; the wakes are %+v",
			len(waiting), what, testWakeRate, waiting)
	}
	return waiting
}

// awaitFullGate waits a few sweeps for the lane to hold exactly the rate in the pacing, and says
// whether it came to hold it. It fails nothing of its own: the two cases that must read a full
// gate — a turn's end the gate refuses, and the credit a dropped wake takes away — say at the
// end of the run whether they were read, and a lane that cannot fill its gate at a sweep has
// the drain assertion to answer for, which is where that failure belongs rather than in a wait
// that timed out.
func awaitFullGate(t *testing.T, d *daemon) bool {
	t.Helper()
	// The bound is generous rather than a few sweeps, because what it waits on is a pass over
	// every machine of the rig and then each admitted machine's own park, published through each
	// machine's view: a run of this package whole, with the race detector on and every other
	// test's goroutines beside it, takes that longer than a run of this test alone. A tight
	// bound read a gate the pass had not finished filling and reported a run that never filled
	// one, which left assertions 2 and 5 unread and failed the case at its end for a fault of
	// the wait rather than of the lane.
	return testwait.Holds(testwait.Bounded(t, 30*time.Second, time.Second), testwait.Poll, func() bool {
		return len(refreshWakesWaiting(t, d)) == testWakeRate
	})
}

// staleClaimsListed is how many claims the stale-plugin lane still has to refresh, counted from
// the operator's own claim list rather than from the state body: the rule requirement R4 states —
// a recorded session, a process that holds a capability, a recorded contract that is not this
// daemon's, no update round, and a launch that installs a plugin — read off a surface of its own,
// so that the count the state serves is checked against something other than itself.
func staleClaimsListed(t *testing.T, d *daemon) int {
	t.Helper()
	left := 0
	for _, c := range d.claims() {
		if c.Session == "" || c.PluginContract == api.DaemonAPIVersion || !supervise.HoldsCapability(supervise.ClaimState(c.State)) {
			continue
		}
		if c.UpdateRound != "" || c.Role == refreshNoInstallRole {
			continue
		}
		left++
	}
	return left
}

// claimsLeftToRefresh is `claimsToRefresh` of the state body at this moment, with the claim list's
// own count of the same set beside it: check c-bound's "equals the stale set at every reading".
//
// The two are read one after the other rather than together, so a relaunch decided between them
// would show as a difference of one; the reading is taken again when they differ, up to five
// times, and a set that is truly not the lane's own never agrees however often it is read. The
// fleet is quiet between the moments this test advances the clock, so a settled pair is the
// ordinary case and a disagreement that stands is the finding.
func claimsLeftToRefresh(t *testing.T, d *daemon, what string) int {
	t.Helper()
	var served, listed int
	for attempt := 1; attempt <= 5; attempt++ {
		served, listed = d.state().ClaimsToRefresh, staleClaimsListed(t, d)
		if served == listed {
			return served
		}
	}
	t.Fatalf("the state says %d claims are left to refresh at %s and the claim list holds %d of the set requirement R4 names, over five readings; the count is of another set",
		served, what, listed)
	return 0
}

// awaitLaunchAtItsTurn advances nothing: it waits for the claim's next launch and requires it at
// the moment the state promised, which is what says a wake ran at its turn and not before or
// after. before is how many launches of that claim the runtime had already been asked for, since
// a claim of a fleet's wave can be launched more than once.
func awaitLaunchAtItsTurn(t *testing.T, rt *waveRuntime, wake api.WakeWaiting, before int) {
	t.Helper()
	testwait.Eventually(t, fmt.Sprintf("the launch of %s at its turn, %s", wake.Claim, wake.DueAt), func() bool {
		return rt.launchesOf(wake.Claim) > before
	})
	launches := rt.launches()
	for i := len(launches) - 1; i >= 0; i-- {
		if launches[i].claim != wake.Claim {
			continue
		}
		if !launches[i].at.Equal(wake.DueAt) {
			t.Fatalf("the launch of %s ran at %s, want the turn the state promised it, %s", wake.Claim, launches[i].at, wake.DueAt)
		}
		return
	}
}

// firstLaunchMoments is how long after origin each of these claims' first launch ran, earliest
// first. A claim that never launched fails the read: a baseline of eleven launches read as twelve
// would compare the wave against a moment nothing happened at.
func firstLaunchMoments(t *testing.T, rt *waveRuntime, tokens []claim.Token, origin time.Time) []time.Duration {
	t.Helper()
	first := map[claim.Token]time.Time{}
	for _, launch := range rt.launches() {
		if _, seen := first[launch.claim]; !seen {
			first[launch.claim] = launch.at
		}
	}
	moments := make([]time.Duration, 0, len(tokens))
	for _, token := range tokens {
		at, launched := first[token]
		if !launched {
			t.Fatalf("%s never launched, so this run measures no first launch for it", token)
		}
		moments = append(moments, at.Sub(origin))
	}
	slices.Sort(moments)
	return moments
}

// baselineRootLaunches is when the same twelve roots' first architect launches run on a boot with
// no stale claim in the rig at all: the moment check c-bound compares the wave's own roots
// against, measured rather than written down, because what twelve launches cost in the pacing is
// the limiter's to say and the daemon jitters every release.
func baselineRootLaunches(t *testing.T) []time.Duration {
	t.Helper()
	cfg := refreshWaveConfig(t)
	roots := waveClaimsFrom(t, cfg, claim.RoleArchitect, refreshWaveStale+1, refreshWaveRoots,
		func(i int, _ claim.Token) supervise.Claim { return unfinishedLaunch(t, i) })
	clock, logged := newWaveClock(), &syncBuffer{}
	rt := newWaveRuntime(clock)
	origin := clock.Now()

	d := startRefreshWaveDaemon(t, cfg, clock, rt, logged)
	defer d.stop()

	waiting := awaitWave(t, d, refreshWaveRoots)
	runWave(t, clock, rt, waiting, 1)
	return firstLaunchMoments(t, rt, roots, origin)
}

// A boot onto a new plugin contract over a fleet: a hundred and twenty live claims on an earlier
// one, twelve roots whose first architect launch the same boot raises, eight of those claims in a
// turn that ends while the wave runs, one in a pod update round, one whose launch installs no
// plugin, and one whose process is killed while it holds an admitted refresh wake. It is the
// surface of check c-bound of dispatch://INFRA-2712/artifact/verification-md, and it proves what
// that check names, each assertion below marked where it stands:
//
//  1. No settled reading of api.State.WakesWaiting holds more wakes of the lane's own kind than
//     worker_wakes_per_minute, at every sweep of the run (settledRefreshes, which says why the
//     reading must settle).
//  2. A turn's end puts no wake into the pacing beyond that bound: a claim in a turn takes no
//     admission, and its turn's end asks the gate itself, so eight turns ending while the gate is
//     full relaunch nothing there and the lane still holds the rate and no more.
//  3. Each of those roots' first architect launch runs no later than one minute of refresh wakes
//     after the moment it would run with no stale claim in the rig (baselineRootLaunches).
//  4. The whole wave is refreshed within about the number of stale claims divided by the rate,
//     counted in rounds of the gate (refreshWaveRounds, which states the margin and why).
//  5. A credit the killed claim's own death relaunch took away is back within one sweep, with the
//     wave still draining.
//  6. `claimsToRefresh` of the state body equals the stale set at every reading, leaves out the
//     claim in a round and the claim no install reaches, and is zero when the wave has drained.
//
// The four counts the check asks to be recorded are logged at the end, and a run of zero of any
// of them fails: a wave that never ran and a wave that ran well look the same to a bound.
//
// The negative controls are the gate's own two: the gate taken off, so the pass admits every
// stale claim, which fails assertions 1 and 3 — the whole fleet is in the pacing at once and the
// roots' launches are due behind all of it — and a gate that admits one claim a sweep instead of
// the rate, which fails assertion 4, since 118 claims then need 118 rounds.
func TestTheRefreshLaneHoldsOneMinuteOfThePacingWhileAWholeFleetsWaveDrains(t *testing.T) {
	baseline := baselineRootLaunches(t)

	cfg := refreshWaveConfig(t)
	bootTokenOf := func(i int) string { return fmt.Sprintf("the-turn-of-%s-%d", cfg.Project, i) }
	stale := waveClaimsFrom(t, cfg, claim.RoleImplementer, 1, refreshWaveStale-1, func(i int, token claim.Token) supervise.Claim {
		c := liveClaim(i, token, supervise.PodUpdateContract-1)
		c.State = supervise.StateIdle
		switch {
		case i <= refreshWaveTurns:
			// In a turn from the boot: the lane admits none of these at all, and the end of the
			// turn is what asks the gate (holdStaleRelaunch, afterTurn). The boot token is what
			// lets this test's own agent say the turn is running and, later, that it is over.
			c.State = supervise.StateWorking
			c.BootTokenHash = supervise.HashBootToken(bootTokenOf(i))
		case i == refreshWaveRounded:
			c.UpdateRound = supervise.PodRound(fmt.Sprintf("pod-of-%s-%d", cfg.Project, i))
		}
		return c
	})
	noInstall := waveClaimsFrom(t, cfg, refreshNoInstallRole, refreshWaveStale, 1, func(i int, token claim.Token) supervise.Claim {
		c := liveClaim(i, token, supervise.PodUpdateContract-1)
		c.State = supervise.StateIdle
		return c
	})[0]
	roots := waveClaimsFrom(t, cfg, claim.RoleArchitect, refreshWaveStale+1, refreshWaveRoots,
		func(i int, _ claim.Token) supervise.Claim { return unfinishedLaunch(t, i) })
	rounded := stale[refreshWaveRounded-1]

	clock, logged := newWaveClock(), &syncBuffer{}
	rt := newWaveRuntime(clock)
	origin := clock.Now()

	d := startRefreshWaveDaemon(t, cfg, clock, rt, logged)

	// The eight agents in a turn, each on its own claim's process: the daemon asks each whether
	// the turn an earlier daemon saw begin is still running, and each says it is.
	address := waveStreamAddress(t, logged)
	turns := make([]*waveTurn, 0, refreshWaveTurns)
	for i := 1; i <= refreshWaveTurns; i++ {
		turns = append(turns, dialWaveTurn(t, address, bootTokenOf(i), stale[i-1]))
	}
	for _, turn := range turns {
		testwait.Eventually(t, fmt.Sprintf("%s to stand in its turn with its agent connected", turn.token), func() bool {
			return d.claim(turn.token).State == string(supervise.StateWorking)
		})
	}

	// Assertion 6, at the boot: the count is the lane's own set — every live claim on the earlier
	// contract but the one in a round and the one no install reaches — less whatever the boot's
	// own first round has already relaunched by the time it is read, since the burst lets one
	// wake straight out and a claim whose process it replaced holds no capability any more. That
	// those two claims are left out of the set, rather than merely late in it, is what the
	// drained end of this test reads: the count stands at zero there with both of them still
	// live, idle and on the contract the boot found.
	staleAtBoot := claimsLeftToRefresh(t, d, "the boot")
	if most := refreshWaveStale - 2; staleAtBoot > most || staleAtBoot < most-testWakeRate {
		t.Fatalf("the state says the lane has %d claims to refresh at the boot, want between %d and %d: the %d live claims on the earlier contract less the one in a round (%s), the one no install reaches (%s) and at most the first round's own relaunches",
			staleAtBoot, most-testWakeRate, most, refreshWaveStale, rounded, noInstall)
	}

	// The drain, round by round: each round is one sweep's admissions, read settled and then run
	// to their turns. Assertion 1 stands at every one of them.
	var (
		rounds, admitted, turnsEnded, turnEndsRefused int
		killed, drained                               claim.Token
		creditBackIn                                  int
		refreshed                                     = map[claim.Token]bool{}
	)
	// A pass of this loop that finds the gate empty waits for the sweep that fills it and is no
	// round of its own, so the loop is bounded by the rounds it counts and by its own passes
	// apart: a lane that stalls altogether leaves the drain below to say so rather than running
	// here for ever.
	for pass := 1; rounds < refreshWaveRounds && pass <= 4*refreshWaveRounds; pass++ {
		left := claimsLeftToRefresh(t, d, fmt.Sprintf("round %d", rounds+1))
		if left == 0 {
			break
		}
		// A round is a sweep's worth of admissions, so it waits for the gate to fill where the
		// lane has more than the rate left: the pass posts its round over one goroutine per
		// claim, and a reading taken inside that fan-out holds part of a round and would count
		// the drain below in rounds of its own making rather than the lane's. The wait is
		// bounded and its failure is no failure of its own: a lane that cannot fill the gate is
		// what the drain assertion is there to report.
		if left > testWakeRate {
			testwait.Holds(3*refreshWaveSweep+time.Second, testwait.Poll, func() bool {
				return len(refreshWakesWaiting(t, d)) >= testWakeRate
			})
		}
		waiting := settledRefreshes(t, d, fmt.Sprintf("round %d", rounds+1))
		if len(waiting) == 0 {
			// The lane has claims left and nothing of its own in the pacing: the sweep that
			// follows posts the next round's worth, and every wake it raises is the gate's.
			testwait.Eventually(t, "the sweep after an empty gate to admit the next round", func() bool {
				return len(refreshWakesWaiting(t, d)) > 0 || claimsLeftToRefresh(t, d, "an empty gate") == 0
			})
			continue
		}

		// Assertion 2, once, at a full gate: the eight turns end together. A claim in a turn took
		// no admission, so its turn's end asks the gate itself (afterTurn) — and the gate is
		// full, so none of them is relaunched there and none of their wakes joins the pacing.
		// They are refreshed at a later round, on credits of their own.
		//
		// The round is the second rather than the first, and the gate is waited for rather than
		// read: the boot's own burst lets one refresh straight out, so the first round holds the
		// rate less one, and a gate with a credit free would relaunch one of these turns where
		// it ended and prove the opposite of what this reads. A lane that never fills its gate
		// is a lane with a drain to answer for, and the drain below is what answers it, so the
		// wait here reports rather than fails — and the end of this case requires the refusal to
		// have been read for every one of the eight turns, so nothing is quietly skipped.
		if turnsEnded == 0 && rounds > 0 {
			full := awaitFullGate(t, d)
			waiting = settledRefreshes(t, d, "the gate the turns end against")
			for _, turn := range turns {
				turn.endTurn()
			}
			for _, turn := range turns {
				testwait.Eventually(t, fmt.Sprintf("the turn of %s to end", turn.token), func() bool {
					return d.claim(turn.token).State == string(supervise.StateIdle)
				})
				turnsEnded++
				if !full {
					continue
				}
				if got := rt.launchesOf(turn.token); got != 0 {
					t.Fatalf("%s was relaunched %d times at its turn's end, want none: the gate is full, so its relaunch waits for a later sweep (dispatch://INFRA-2712 R2)", turn.token, got)
				}
				turnEndsRefused++
			}
			// Assertion 2 itself: the pacing holds no more of the lane than it held before the
			// turns ended, and every wake in it is one the gate admitted earlier.
			held := settledRefreshes(t, d, "the eight turns ending")
			if full {
				for _, wake := range held {
					if !claimsOfWakes(waiting)[wake.Claim] {
						t.Fatalf("the refresh of %s joined the pacing as eight turns ended, want none of them in it: a turn's end takes a credit of the gate or waits for the next sweep",
							wake.Claim)
					}
				}
			}
			waiting = held
		}

		// Assertion 5, once, at a full gate after those turns have ended. It is in two parts,
		// because the daemon answers a killed process under a waiting launch in a way the check
		// does not describe.
		//
		// The process of a claim that holds an admitted refresh wake is killed. Nothing is
		// decided by it: an observation of the process a waiting launch replaces is dropped
		// where it is judged, whatever it found, because that launch already answers it (judge's
		// own guard, launchWaits). So no death relaunch is raised over that wake, the wake keeps
		// its turn and its credit, and the process the wake's turn lets go is the dead one.
		//
		// What does take a credit away with no return is every path that drops a waiting wake
		// without running it (letGo), and an operator's suspension of such a claim is one of
		// them: it ends the very process the refresh wake was parked over. That is the credit
		// the next sweep's correction takes back, which is what requirement R1 promises of every
		// loss outside the three paths that return a credit directly (correctRefreshGate,
		// decision P2 of dispatch://INFRA-2712/artifact/plan-md).
		//
		// It reads a full gate too, and waits for one in the same way, for the same reason: a
		// lane that never fills one has the drain below to answer for, and the end of this case
		// requires this part to have been read at all.
		if killed == "" && turnsEnded > 0 && rounds <= 4 && awaitFullGate(t, d) {
			waiting = settledRefreshes(t, d, "the gate the kill reads")
			killed, drained = waiting[len(waiting)-1].Claim, waiting[0].Claim
			killedClaim := d.claim(killed)
			if killedClaim.Locator == nil {
				t.Fatalf("the claim %s whose refresh wake waits records no process to kill: %+v", killed, killedClaim)
			}
			rt.fake().Emit(runtime.Observation{Locator: *killedClaim.Locator, Kind: runtime.Gone, At: clock.Now()})
			testwait.Eventually(t, fmt.Sprintf("the daemon to read the death of %s under its waiting refresh wake", killed), func() bool {
				for _, line := range loggedLines(t, logged.String(), observationUnderLaunchLine) {
					if line["claim"] == string(killed) {
						return true
					}
				}
				return false
			})
			after := settledRefreshes(t, d, "the death of a claim holding a refresh wake")
			if !claimsOfWakes(after)[killed] || len(after) != len(waiting) {
				t.Fatalf("the lane holds %d refreshes after the death of %s, and its own wake is %v, want the %d it held with that wake among them: an observation of a process a waiting launch replaces decides nothing",
					len(after), killed, claimsOfWakes(after)[killed], len(waiting))
			}

			// The credit a dropped wake does take away, and the sweep that takes it back. The
			// sweeps are counted from the moment the wake is seen gone rather than from the
			// request that dropped it, because the sweep runs on the wall clock while the drop
			// and the reading of it are the test's own round trips: what is being measured is
			// the correction, which is the next pass's first act (correctRefreshGate), not how
			// long this test took to see the wake go.
			status, body := d.request(http.MethodPost, "/legion/v1/operator/claims/"+string(drained)+"/suspend", nil, true)
			if status != http.StatusOK {
				t.Fatalf("the suspend of %s, which holds an admitted refresh wake, = %d; body %s", drained, status, body)
			}
			testwait.Eventually(t, fmt.Sprintf("the refresh wake of %s to go with the process its suspension ended", drained), func() bool {
				return !claimsOfWakes(refreshWakesWaiting(t, d))[drained]
			})
			sweptAt := len(rt.fake().CallsOf("ReconcileOrphans"))
			testwait.Eventually(t, "the credit that suspension took away to come back and admit another claim", func() bool {
				return len(refreshWakesWaiting(t, d)) == testWakeRate
			})
			creditBackIn = len(rt.fake().CallsOf("ReconcileOrphans")) - sweptAt
			if creditBackIn > 2 {
				t.Fatalf("the credit the suspension of %s took away came back %d sweeps after the wake was seen gone, want the next sweep and no later than the one after it: the pass that follows the drop corrects the gate from the wakes the machines hold and admits another claim on it",
					drained, creditBackIn)
			}
			if left := claimsLeftToRefresh(t, d, "the credit coming back"); left == 0 {
				t.Fatal("the lane had nothing left to refresh when the credit came back, so this says nothing about a wave still draining")
			}
			waiting = settledRefreshes(t, d, "the credit coming back")
		}

		rounds++
		admitted += len(waiting)

		// The turns of the round: every wake the pacing holds now, of whatever lane, each at the
		// moment the state promised it. The whole queue rather than this lane's own share of it,
		// because what check c-bound reads of the roots is when their launches ran, and a clock
		// advanced straight from one refresh's turn to the next would run every launch between
		// them at the moment of the jump rather than at its own.
		for _, wake := range waitingWakes(t, d) {
			before := rt.launchesOf(wake.Claim)
			clock.advanceTo(wake.DueAt)
			awaitLaunchAtItsTurn(t, rt, wake, before)
			if wake.Kind == supervise.WakePluginRefresh {
				refreshed[wake.Claim] = true
			}
		}
	}

	// Assertion 4 and assertion 6's own end: the wave has drained, in rounds of the gate rather
	// than one claim a sweep, and the count the state serves reads zero.
	if left := claimsLeftToRefresh(t, d, "the drained wave"); left != 0 {
		t.Fatalf("the lane still has %d claims to refresh after %d rounds of the gate, want none: %d claims at the rate of %d is %d rounds, and this case allows %d",
			left, rounds, staleAtBoot, testWakeRate, (staleAtBoot+testWakeRate-1)/testWakeRate, refreshWaveRounds)
	}

	// The claim in a round and the claim no install reaches are where the boot found them: the
	// lane posted neither, and the count named neither.
	for _, each := range []struct {
		token claim.Token
		why   string
	}{{rounded, "its pod update round is carrying it"}, {noInstall, "no install of this daemon's reaches its plugin"}} {
		if got := rt.launchesOf(each.token); got != 0 {
			t.Fatalf("%s was relaunched %d times, want none: the lane passes it over while %s", each.token, got, each.why)
		}
		if c := d.claim(each.token); c.PluginContract != supervise.PodUpdateContract-1 || c.State != string(supervise.StateIdle) {
			t.Fatalf("%s is %s on contract %d after the wave, want it left idle on the contract the boot found: %s", each.token, c.State, c.PluginContract, each.why)
		}
	}

	// Assertion 3: every root's first architect launch, against the same boot with no stale claim
	// in the rig. The two runs are compared turn by turn, earliest against earliest, because
	// which root takes which turn is the order the boot's own fan-out gave it and what the check
	// reads is the schedule.
	//
	// The margin is one minute of refresh wakes read as the clock pays for it, and it is stated
	// here rather than assumed: a paced release is at least one interval and less than two after
	// the one before it, because the daemon jitters every release by a part of an interval and
	// the limiter charges that part forward (wakepace.Reserve, assertSpread's own gap bound). So
	// the rate's worth of refresh wakes ahead of a launch costs between one and two minutes of
	// the clock, and the roots ahead of the ith in each run — the same count in both, drawn
	// afresh in each — can differ by up to one interval apiece. Two minutes plus the roots' own
	// worth is the bound; what the fleet's wave measures is about ninety seconds, and what the
	// gate taken off measures is the whole fleet's fifteen.
	rootMoments := firstLaunchMoments(t, rt, roots, origin)
	behind := 2*testWakeRate*testWakeInterval + refreshWaveRoots*testWakeInterval
	worst := time.Duration(0)
	for i, moment := range rootMoments {
		if late := moment - baseline[i]; late > worst {
			worst = late
		}
		if moment > baseline[i]+behind {
			t.Fatalf("the %dth root launch of the wave ran %s after the boot and the same launch with no stale claim ran %s after it, %s later, want no more than %s: a newly admitted tree waits one minute of refreshes, not a fleet's worth\nthe wave's own roots: %v\nthe baseline: %v",
				i+1, moment, baseline[i], moment-baseline[i], behind, rootMoments, baseline)
		}
	}

	// The four counts, recorded, and a zero of any of them failing: the stale claims the lane read
	// at the boot, the roots whose first launch was measured, the turns that ended during the
	// wave, and the claims refreshed.
	t.Logf("check c-bound: %d stale claims at the boot, %d roots measured, %d turns ended during the wave, %d claims refreshed in %d rounds of the gate (%d admissions); the killed claim's credit came back in %d sweep(s), and the latest root launch ran %s behind the same launch with no stale claim, within the %s this case allows",
		staleAtBoot, len(rootMoments), turnsEnded, len(refreshed), rounds, admitted, creditBackIn, worst, behind)
	for _, count := range []struct {
		what string
		got  int
	}{
		{"stale claims at the boot", staleAtBoot},
		{"roots whose first launch was measured", len(rootMoments)},
		{"turns that ended during the wave", turnsEnded},
		{"claims refreshed", len(refreshed)},
	} {
		if count.got == 0 {
			t.Fatalf("%s = 0: a wave that never ran and a wave that ran well look the same to a bound", count.what)
		}
	}
	if turnsEnded != refreshWaveTurns || len(rootMoments) != refreshWaveRoots {
		t.Fatalf("%d turns ended and %d roots were measured, want the %d and the %d the surface of check c-bound names",
			turnsEnded, len(rootMoments), refreshWaveTurns, refreshWaveRoots)
	}
	// The two cases that need a full gate are read against one or they are not read at all, so
	// each says here that it was: a run where the lane never filled its gate would otherwise
	// pass them over in silence, and the whole of assertions 2 and 5 with them.
	if turnEndsRefused != refreshWaveTurns {
		t.Fatalf("%d of the %d turns ended against a full gate, want every one: the refusal a full gate makes is what assertion 2 reads, and a run that never filled the gate read none of it",
			turnEndsRefused, refreshWaveTurns)
	}
	if killed == "" {
		t.Fatal("no claim was killed under an admitted refresh wake and no credit was taken away: assertion 5 needs a full gate and this run never filled one")
	}
}

// wakeMomentsOfRun is the moment each wake of a run ran, read from the lines the daemon writes
// where a wake runs (supervise's wakeRan, wakeLines) and paired with the launch the runtime
// recorded for it, which is where the moment on the test's own clock is: the line itself carries
// the kind, the pace and how long the wake was held, and its timestamp is the wall clock's,
// which a rig that holds the machines' time has moved nowhere. It answers the moments and the
// claims of the lines that said no pacing could have held them.
//
// Every line counts towards the moments, whatever it says of itself, and that is the point:
// requirement R3 exempts two kinds of wake by name — the project controller's claim, and a wake
// a request marks exempt — and a wave that holds neither must therefore hold none. A lane that
// declared itself exempt would otherwise walk out of the 60-second windows it is meant to be
// counted in, which is exactly the negative control of check c-pace.
//
// The pairing is by claim and in order: the nth line of a claim is the nth launch of that claim,
// because the line is written immediately before the launch it belongs to runs (runWake) and
// both records are kept in the order they were made. A line with no launch left to pair with is
// a failure rather than a skip: it would mean a wake wrote its line and launched nothing, and
// the windows would then be counted over fewer wakes than the daemon says it ran.
func wakeMomentsOfRun(t *testing.T, logged *syncBuffer, rt *waveRuntime) ([]time.Time, []claim.Token) {
	t.Helper()
	launches := rt.launches()
	next := map[claim.Token]int{}
	lines := wakeLines(t, logged.String())
	moments := make([]time.Time, 0, len(lines))
	var unpaced []claim.Token
	for _, line := range lines {
		token := claim.Token(fmt.Sprint(line["claim"]))
		at, paired := time.Time{}, false
		for i := next[token]; i < len(launches); i++ {
			if launches[i].claim != token {
				continue
			}
			at, paired, next[token] = launches[i].at, true, i+1
			break
		}
		if !paired {
			t.Fatalf("the wake line %v names a wake of %s that launched nothing; the windows would then count fewer wakes than the daemon ran", line, token)
		}
		moments = append(moments, at)
		if line["paced"] != true {
			unpaced = append(unpaced, token)
			continue
		}
		if line["rate"] != float64(testWakeRate) || line["burst"] != float64(testWakeBurst) {
			t.Fatalf("a paced wake line says rate %v burst %v, want the operator's %d and %d: %v", line["rate"], line["burst"], testWakeRate, testWakeBurst, line)
		}
	}
	return moments, unpaced
}

// assertMinutesHoldTheRate is requirement R3 over every wake a run ran, counting each lane
// together with the rest: no 60-second window of the test clock holds more of them than the
// rate plus the burst. Every window that begins at a wake is counted, which is every window
// that can hold the most: one that begins between two wakes holds no more than the window
// beginning at the wake after it.
func assertMinutesHoldTheRate(t *testing.T, what string, moments []time.Time) {
	t.Helper()
	slices.SortFunc(moments, func(a, b time.Time) int { return a.Compare(b) })
	for i, moment := range moments {
		inMinute := 0
		for _, other := range moments[i:] {
			if other.Sub(moment) < time.Minute {
				inMinute++
			}
		}
		if inMinute > testWakeRate+testWakeBurst {
			t.Fatalf("%s: the minute from %s holds %d paced wakes, want at most %d, the rate plus the burst: %v",
				what, moment, inMinute, testWakeRate+testWakeBurst, moments)
		}
	}
}

// The same wave read as check c-pace of dispatch://INFRA-2712/artifact/verification-md reads it:
// the lane's refreshes and the roots' own launches drawing on one limiter, with no 60-second
// window of the run holding more paced wakes than the rate plus the burst, counted from the
// `supervise: wake ran` lines the daemon writes and recorded as the number of lines read.
//
// It drives a wave of its own, three gates' worth of stale claims beside four roots, rather than
// the fleet of the case above: what the windows read is the one limiter every lane reserves
// from, and a wave of a hundred and twenty says no more about a minute of it than a wave of
// twenty-four does. The whole-fleet wave is next door, and the rate-0 half of this check —
// a daemon with no pacing refreshing every stale claim in one tick — is
// TestWithNoPacingTheStaleRelaunchWaveReplacesEveryProcessInOneTick, which stands unchanged.
//
// The drive takes whatever the pacing holds, of whatever lane, and advances to its turns until
// every claim of the wave has been relaunched: a lane taken out of the pacing would run its
// wakes where they were raised, which is exactly what the windows then count.
//
// The negative control is the lane's relaunch marked exempt from the pacing (supervise's
// reserveWake): the refreshes then all run in the instant the pass posts them and the first
// window holds the whole wave.
func TestNoMinuteOfAFleetsRefreshWaveHoldsMoreThanTheRateAndTheBurst(t *testing.T) {
	cfg := waveConfig(t)
	paceStale := 3 * testWakeRate
	const paceRoots = 4
	stale := waveClaimsFrom(t, cfg, claim.RoleImplementer, 1, paceStale, func(i int, token claim.Token) supervise.Claim {
		c := liveClaim(i, token, supervise.PodUpdateContract-1)
		c.State = supervise.StateIdle
		return c
	})
	roots := waveClaimsFrom(t, cfg, claim.RoleArchitect, paceStale+1, paceRoots,
		func(i int, _ claim.Token) supervise.Claim { return unfinishedLaunch(t, i) })
	clock, logged := newWaveClock(), &syncBuffer{}
	rt := newWaveRuntime(clock)

	relaunchable := true
	d := startWaveDaemon(t, cfg, clock, rt, logged, func(o *overrides) {
		o.orphanSweep = refreshWaveSweep
		o.pluginRelaunchable = &relaunchable
	})

	wave := append(append([]claim.Token{}, stale...), roots...)
	launched := func() int {
		done := 0
		for _, token := range wave {
			if rt.launchesOf(token) > 0 {
				done++
			}
		}
		return done
	}
	for round := 1; round <= refreshWaveRounds && launched() < len(wave); round++ {
		waiting := waitingWakes(t, d)
		if len(waiting) == 0 {
			testwait.Eventually(t, "the sweep after an empty queue to raise the next wakes", func() bool {
				return len(waitingWakes(t, d)) > 0 || launched() == len(wave)
			})
			continue
		}
		for _, wake := range waiting {
			before := rt.launchesOf(wake.Claim)
			clock.advanceTo(wake.DueAt)
			awaitLaunchAtItsTurn(t, rt, wake, before)
		}
	}
	if done := launched(); done != len(wave) {
		t.Fatalf("%d of the wave's %d claims were launched, want every one: the windows below read the whole wave or nothing", done, len(wave))
	}

	moments, unpaced := wakeMomentsOfRun(t, logged, rt)
	assertMinutesHoldTheRate(t, "the fleet's refresh wave and the roots beside it", moments)
	t.Logf("check c-pace: %d `supervise: wake ran` lines read over %d stale claims and %d roots, every one of them a wake the pacing held",
		len(moments), paceStale, paceRoots)
	// No claim of this wave is the project controller's and no request of it is exempt, so every
	// wake of it is one the pacing held and one the windows above counted. A lane taken out of
	// the pacing would say so on its own lines, which is what this reads.
	if len(unpaced) > 0 {
		t.Fatalf("%d of the run's wakes say no pacing could have held them, want none: this wave holds no controller's claim and no exempt request, so every wake of it is paced; the claims are %v",
			len(unpaced), unpaced)
	}
	if len(moments) < len(wave) {
		t.Fatalf("%d wakes were read from the daemon's lines, want at least the wave's %d: a window counted over fewer wakes than ran proves nothing",
			len(moments), len(wave))
	}
}
