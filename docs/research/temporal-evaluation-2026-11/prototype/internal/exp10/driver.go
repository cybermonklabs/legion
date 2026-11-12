package exp10

import (
	"context"
	"fmt"
	"syscall"
	"time"

	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/harness"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/restatex"
)

// The experiment's timings. They are short on purpose: the whole run, the server's
// start, the kill and the recovery included, has to finish in well under three minutes.
const (
	effectWait   = 30 * time.Second
	recoveryWait = 45 * time.Second
	terminalWait = 45 * time.Second
	// deadEndpointObserve is how long the invocation is left with nothing listening
	// on the endpoint it is pinned to, which is how the experiment sees what Restate
	// does to work whose deployment is gone.
	deadEndpointObserve = 8 * time.Second
	sample              = 2 * time.Second
)

// Run is experiment 10. It starts a Restate server and a service process, has the
// server invoke a handler that performs a side effect and blocks inside it, kills that
// service process with SIGKILL, and then measures what Restate did: what the server
// says about the invocation at each stage, whether the journal survived, whether the
// handler was re-dispatched, and above all how many times the side effect happened.
//
// suppressKill is the run's control: with it the driver sends no signal, so nothing is
// interrupted and nothing is recovered, and the experiment must report that it measured
// nothing.
func Run(ctx context.Context, cfg harness.Config, suppressKill bool) error {
	workDir, err := harness.WorkDir(cfg)
	if err != nil {
		return err
	}
	serviceBinary, err := harness.RestateService(cfg)
	if err != nil {
		return err
	}
	serverBinary, err := harness.RestateServer(cfg)
	if err != nil {
		return err
	}
	sideEffects := SideEffectFile(workDir)
	release := ReleaseFile(workDir)
	if err := restatex.RemoveAll(sideEffects, release); err != nil {
		return err
	}

	env, err := restatex.StartEnv(ctx, restatex.EnvOptions{
		Experiment: 10, WorkDir: workDir, ServerBinary: serverBinary,
		Ingress: cfg.RestateIngress, Admin: cfg.RestateAdmin,
	})
	if err != nil {
		return err
	}
	defer env.Stop()

	harness.Fact("restate-server-version", env.Version)
	harness.Fact("restate-server-started-by-this-experiment", env.StartedServer)
	harness.Fact("restate-ingress-address", cfg.RestateIngress)
	harness.Fact("restate-admin-address", cfg.RestateAdmin)
	harness.Fact("restate-data-directory", env.Dir)
	harness.Fact("invocation-status-read-from",
		"POST /query against the server's sys_invocation and sys_journal views: restate-server 1.8.1's admin API has no GET /invocations, only the per-invocation pause, resume, kill, cancel and purge operations")
	harness.Fact("side-effect-file", sideEffects)

	// 1. The process that will be killed, and the deployment that points at it.
	firstPort, err := restatex.FreePort()
	if err != nil {
		return err
	}
	first, err := startService(ctx, "service1", serviceBinary, workDir, firstPort)
	if err != nil {
		return err
	}
	defer first.Stop()
	harness.Fact("service1-pid", first.PID())
	harness.Fact("service1-endpoint", first.URL())

	deployment, err := env.Admin.Register(ctx, first.URL(), false)
	if err != nil {
		return err
	}
	harness.Fact("deployment-id", deployment.ID)
	harness.Fact("deployment-services", fmt.Sprintf("%v", deployment.Services))

	// 2. The invocation. It is started one-way, so the driver holds no connection to
	// it and can watch what the server does with it.
	invocationID, err := env.Ingress.Send(ctx, ServiceName, HandlerName, NewParams(workDir))
	if err != nil {
		return err
	}
	harness.Fact("invocation-id", invocationID)

	// 3. Wait until the handler has performed its side effect and is blocked in it.
	// Only then can a kill land inside the step rather than before or after it.
	lines, err := restatex.WaitForLines(ctx, sideEffects, 1, effectWait)
	if err != nil {
		return fmt.Errorf("the handler never performed its side effect: %w", err)
	}
	harness.Fact("side-effect-1", lines[0])

	before, err := env.Admin.Invocation(ctx, invocationID)
	if err != nil {
		return err
	}
	beforeJournal, err := env.Admin.Journal(ctx, invocationID)
	if err != nil {
		return err
	}
	harness.Fact("invocation-before-kill", before)
	harness.Fact("journal-before-kill", restatex.JournalSummary(beforeJournal))
	harness.Fact("handlers-in-flight-in-service1-before-kill", latest(first, "handlers-in-flight"))
	harness.Fact("in-flight-step-marker-in-the-journal", stepMarker(beforeJournal))

	// 4. The kill itself, by the exact pid the driver recorded.
	killSent := false
	killedAt := time.Now()
	switch {
	case suppressKill:
		harness.Fact("kill-sent", false)
		harness.Fact("killed-pid", "none")
		harness.Fact("kill-signal", "none")
		harness.Fact("kill-suppressed", true)
	case !first.Alive():
		harness.Fact("kill-sent", false)
		harness.Fact("killed-pid", "none")
		harness.Fact("kill-signal", "none")
		harness.Fact("service1-already-gone", true)
	default:
		killedAt = time.Now()
		if err := first.Signal(syscall.SIGKILL); err != nil {
			return fmt.Errorf("kill service1: %w", err)
		}
		killSent = true
		harness.Fact("kill-sent", true)
		harness.Fact("killed-pid", first.PID())
		harness.Fact("kill-signal", "SIGKILL")
		harness.Fact("killed-process-still-alive", first.Alive())
	}

	var replacement *restatex.Service
	if killSent {
		// 5. The endpoint the invocation is pinned to is now dead. This is the only
		// binding Restate has -- a deployment is a URL -- so what the server does
		// here is its answer to "the process holding this work never came back".
		if err := observeDeadEndpoint(ctx, env, invocationID, sideEffects); err != nil {
			return err
		}

		// 6. A second deployment of the same service, at a URL that is alive, while
		// the invocation's own endpoint is still dead: does the invocation move?
		secondPort, err := restatex.FreePort()
		if err != nil {
			return err
		}
		other, err := startService(ctx, "service-other-endpoint", serviceBinary, workDir, secondPort)
		if err != nil {
			return err
		}
		defer other.Stop()
		otherDeployment, err := env.Admin.Register(ctx, other.URL(), false)
		if err != nil {
			return err
		}
		harness.Fact("second-deployment-id", otherDeployment.ID)
		harness.Fact("second-deployment-endpoint", other.URL())
		revision, latestDeployment, err := env.Admin.ServiceRevisions(ctx, ServiceName)
		if err != nil {
			return err
		}
		harness.Fact("service-revision-after-the-second-deployment", revision)
		harness.Fact("deployment-new-invocations-would-go-to", latestDeployment)
		if err := wait(ctx, deadEndpointObserve); err != nil {
			return err
		}
		pinned, err := env.Admin.Invocation(ctx, invocationID)
		if err != nil {
			return err
		}
		effectsNow, err := restatex.ReadLines(sideEffects)
		if err != nil {
			return err
		}
		harness.Fact("invocation-while-a-live-deployment-of-the-same-service-existed", pinned)
		harness.Fact("invocation-moved-to-the-live-deployment", pinned.PinnedDeployment == otherDeployment.ID)
		harness.Fact("side-effects-while-only-the-other-deployment-was-alive", len(effectsNow))

		// 7. The replacement: a new process at the very URL the dead one served,
		// which is the only thing that makes the pinned deployment reachable again.
		replacement, err = startService(ctx, "service2", serviceBinary, workDir, firstPort)
		if err != nil {
			return err
		}
		defer replacement.Stop()
		harness.Fact("replacement-pid", replacement.PID())
		harness.Fact("replacement-endpoint", replacement.URL())
		lines, err = restatex.WaitForLines(ctx, sideEffects, 2, recoveryWait)
		if err != nil {
			harness.Fact("handler-redispatched-to-the-replacement", false)
			harness.Fact("redispatch-wait-error", err)
		} else {
			harness.Fact("handler-redispatched-to-the-replacement", true)
			harness.Fact("redispatch-latency-seconds", fmt.Sprintf("%.3f", time.Since(killedAt).Seconds()))
			harness.Fact("side-effect-2", lines[1])
			during, err := env.Admin.Invocation(ctx, invocationID)
			if err != nil {
				return err
			}
			harness.Fact("invocation-during-the-re-dispatched-attempt", during)
		}
	}

	// 8. Release the step, so the attempt running now can finish and the server can
	// record what it did.
	if err := restatex.Touch(release); err != nil {
		return err
	}
	final, err := waitTerminal(ctx, env, invocationID, terminalWait)
	if err != nil {
		return err
	}
	harness.Fact("invocation-final", final)
	finalJournal, err := env.Admin.Journal(ctx, invocationID)
	if err != nil {
		return err
	}
	harness.Fact("journal-final", restatex.JournalSummary(finalJournal))
	finalEvents, err := env.Admin.JournalEvents(ctx, invocationID)
	if err != nil {
		return err
	}
	harness.Fact("events-recorded-against-the-invocation", restatex.JournalEventsSummary(finalEvents))
	harness.Fact("journal-survived-the-kill", len(finalJournal) >= len(beforeJournal) && len(beforeJournal) > 0)
	status, output, err := env.Ingress.Output(ctx, invocationID)
	if err != nil {
		return err
	}
	harness.Fact("invocation-output-http-status", status)
	harness.Fact("invocation-output", output)

	// 9. The number the experiment exists to produce.
	lines, err = restatex.ReadLines(sideEffects)
	if err != nil {
		return err
	}
	effects := len(lines)
	for i, line := range lines {
		harness.Fact(fmt.Sprintf("side-effect-line-%d", i+1), line)
	}
	harness.Fact("side-effect-performed-times", effects)
	if effects > 1 {
		harness.Fact("what-that-means",
			"a durable step whose result never reached the journal is run again by the replacement process: Restate's guarantee for a Run block is at-least-once, and exactly-once only for a Run whose result the server recorded")
	} else {
		harness.Fact("what-that-means",
			"nothing was interrupted, so this run saw only that an uninterrupted step runs once; what an interrupted one does is what the kill measures")
	}
	harness.Fact("state-for-a-handler-that-ran-with-its-outcome-unknown", unknownOutcomeState(beforeJournal, effects))

	// 10. What binding the work to one process means here.
	harness.Fact("work-bound-to", "a deployment, which is an endpoint URL: sys_invocation.pinned_deployment_id, set on the first attempt and kept")
	harness.Fact("work-bound-to-a-process", "no: Restate has no notion of the process behind the URL, so any process listening on that URL may be handed the work, and a process is replaceable without the server noticing")
	harness.Fact("if-the-pinned-endpoint-never-comes-back",
		fmt.Sprintf("the invocation retries against that URL with an exponential backoff -- initial-interval %s, factor %s, max-interval %s -- for max-attempts %s, and then does what on-max-attempts says, which in this server's own effective configuration is %q",
			env.Defaults.RetryInitialInterval, env.Defaults.RetryFactor, env.Defaults.RetryMaxInterval,
			env.Defaults.RetryMaxAttempts, env.Defaults.RetryOnMaxAttempts))

	harness.Fact("limit", "one invocation, one durable step, one kill, on a single-node server with one partition: it shows what Restate records and re-runs when the process behind a deployment dies inside a step, not how a cluster behaves, nor what happens when the server rather than the service process is the thing that fails")

	// 11. The verdict. A run that killed nothing measured nothing, whatever else it saw.
	switch {
	case !killSent:
		return fmt.Errorf("no process was killed, so this run measured nothing about what Restate does to a handler that was interrupted: the side effect happened %d time(s) and nothing was re-dispatched", effects)
	case !final.Found:
		return fmt.Errorf("the server holds no row for invocation %s, so its fate could not be read back", invocationID)
	case !final.Succeeded():
		return fmt.Errorf("the re-dispatched invocation ended %q/%q, not a success, so what Restate recorded for the step was not read back", final.Status, final.CompletionResult)
	case effects < 2:
		return fmt.Errorf("the side effect happened %d time(s): the kill landed but nothing re-ran the step, so the re-dispatch outcome was not measured", effects)
	}
	return nil
}

// startService runs one cmd/restateservice process in experiment 10's mode on a
// loopback port of its own.
func startService(ctx context.Context, name, binary, workDir string, port int) (*restatex.Service, error) {
	address := restatex.Address(port)
	args := []string{"-mode", "exp10", "-address", address, "-work-dir", workDir}
	return restatex.StartService(ctx, name, binary, args, workDir, address, restatex.ServiceReady)
}

// observeDeadEndpoint samples the invocation while nothing is listening on the URL it
// is pinned to, so the experiment can say what Restate does with work whose deployment
// is unreachable: retry it, move it, or stop.
func observeDeadEndpoint(ctx context.Context, env *restatex.Env, invocationID, sideEffects string) error {
	harness.Fact("dead-endpoint-observed-for", deadEndpointObserve.String())
	deadline := time.Now().Add(deadEndpointObserve)
	for i := 1; time.Now().Before(deadline); i++ {
		if err := wait(ctx, sample); err != nil {
			return err
		}
		reading, err := env.Admin.Invocation(ctx, invocationID)
		if err != nil {
			return err
		}
		harness.Fact(fmt.Sprintf("invocation-with-its-endpoint-dead-sample-%d", i), reading)
	}
	lines, err := restatex.ReadLines(sideEffects)
	if err != nil {
		return err
	}
	events, err := env.Admin.JournalEvents(ctx, invocationID)
	if err != nil {
		return err
	}
	harness.Fact("side-effects-while-the-pinned-endpoint-was-dead", len(lines))
	harness.Fact("events-while-the-pinned-endpoint-was-dead", restatex.JournalEventsSummary(events))
	harness.Fact("status-while-the-pinned-endpoint-was-dead",
		"ready, not backing-off: in restate-server 1.8.1 an invocation whose endpoint will not answer sits in ready with no row in sys_invocation_state, so sys_invocation.retry_count reads 0 and the only record of the failed attempts is in sys_journal_events")
	return nil
}

// stepStarted and stepFinished read a journal for the two entries a durable step
// leaves: the command, appended when the body starts, and the notification carrying its
// result, appended when the body returns.
func stepStarted(entries []restatex.JournalEntry) bool {
	for _, entry := range entries {
		if entry.Type == "Command: Run" && entry.Name == StepName {
			return true
		}
	}
	return false
}

func stepFinished(entries []restatex.JournalEntry) bool {
	for _, entry := range entries {
		if entry.Type == "Notification: Run" {
			return true
		}
	}
	return false
}

// stepMarker says what the journal held for the step while its body was running.
func stepMarker(entries []restatex.JournalEntry) string {
	switch {
	case stepStarted(entries) && !stepFinished(entries):
		return "a Run command entry with no result notification beside it: the server can see that the step was begun and has not returned"
	case stepStarted(entries):
		return "a Run command entry and its result notification: the step had already returned"
	default:
		return "none: the journal holds no entry for the step at all"
	}
}

// unknownOutcomeState is the experiment's answer to whether anything in Restate means
// "ran, outcome unknown, do not re-run", derived from what the journal held while the
// step's body was in the middle of running and from what the next attempt then did.
func unknownOutcomeState(beforeKill []restatex.JournalEntry, effects int) string {
	if !stepStarted(beforeKill) || stepFinished(beforeKill) {
		return "nothing was observed: the journal did not hold a begun-and-unfinished step at the moment of the kill, so this run cannot say"
	}
	if effects > 1 {
		return "no: the journal does hold a Run command with no result notification, so the server can see the step was begun and did not return -- but that state is not do-not-re-run, it is re-run. The next attempt executed the body again, and the side effect happened twice. Restate has no third state between 'not done' and 'done'"
	}
	return "the journal held a begun-and-unfinished step and the side effect happened once, so nothing was re-run in this run"
}

// waitTerminal blocks until the invocation reaches a state it cannot leave on its own.
func waitTerminal(ctx context.Context, env *restatex.Env, invocationID string, timeout time.Duration) (restatex.Invocation, error) {
	deadline := time.Now().Add(timeout)
	for {
		reading, err := env.Admin.Invocation(ctx, invocationID)
		if err != nil {
			return restatex.Invocation{}, err
		}
		if reading.Found && reading.Terminal() {
			return reading, nil
		}
		if !time.Now().Before(deadline) {
			return reading, fmt.Errorf("invocation %s was still %q after %s", invocationID, reading.Status, timeout)
		}
		if err := wait(ctx, 200*time.Millisecond); err != nil {
			return reading, err
		}
	}
}

// latest is the newest reading a service process printed of itself, or a note that it
// printed none -- which is what a process that has already been killed shows.
func latest(service *restatex.Service, key string) string {
	value, ok := service.Latest(key)
	if !ok {
		return "not reported"
	}
	return value
}

// wait sleeps, and gives up early when the experiment is interrupted.
func wait(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
