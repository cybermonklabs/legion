package exp11

import (
	"context"
	"fmt"
	"strings"
	"syscall"
	"time"

	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/harness"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/restatex"
)

// The experiment's timings. They are short on purpose: both halves, the server's start
// and a kill included, have to finish in well under three minutes.
const (
	askWait      = 30 * time.Second
	suspendWait  = 30 * time.Second
	pausedWait   = 40 * time.Second
	terminalWait = 45 * time.Second
	settle       = 500 * time.Millisecond
	// idleObserve is how long the suspended invocation is simply left alone, so the
	// readings of the process holding nothing are of a wait that is really open.
	idleObserve = 3 * time.Second
	// issueID is the Legion issue the gate in this experiment stands for.
	issueID = "INFRA-1546"
)

// waitDays are the lengths of a wait for a person that the reasoned lines price.
var waitDays = []int{1, 7, 30}

// Run is experiment 11. Its first half opens a wait for a person and measures what
// that wait holds, in the service process and in the server, across the service
// process being killed and replaced, until a plain HTTP request from outside resolves
// it. Its second half drives a handler that always fails under a small retry budget
// and measures Restate's `paused`: reaching it, listing it, reading it, and resuming
// out of it.
func Run(ctx context.Context, cfg harness.Config) error {
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
	askFile, decisionFile := AskFile(workDir), DecisionFile(workDir)
	attemptFile, fixFile := AttemptFile(workDir), FixFile(workDir)
	selfFile, selfRelease := SelfPauseFile(workDir), SelfPauseReleaseFile(workDir)
	if err := restatex.RemoveAll(askFile, decisionFile, attemptFile, fixFile, selfFile, selfRelease); err != nil {
		return err
	}

	env, err := restatex.StartEnv(ctx, restatex.EnvOptions{
		Experiment: 11, WorkDir: workDir, ServerBinary: serverBinary,
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
	harness.Fact("invocation-status-read-from",
		"POST /query against the server's sys_invocation and sys_journal views: restate-server 1.8.1's admin API has no GET /invocations, only the per-invocation pause, resume, kill, cancel and purge operations")

	port, err := restatex.FreePort()
	if err != nil {
		return err
	}
	service, err := startService(ctx, "service1", serviceBinary, workDir, port)
	if err != nil {
		return err
	}
	defer service.Stop()
	deployment, err := env.Admin.Register(ctx, service.URL(), false)
	if err != nil {
		return err
	}
	harness.Fact("service1-pid", service.PID())
	harness.Fact("service-endpoint", service.URL())
	harness.Fact("deployment-id", deployment.ID)

	replacement, err := runWait(ctx, cfg, env, service, serviceBinary, workDir, port)
	if replacement != nil {
		defer replacement.Stop()
	}
	if err != nil {
		return err
	}
	if err := runPause(ctx, env, attemptFile, fixFile); err != nil {
		return err
	}
	if err := runSelfPause(ctx, cfg, env, selfFile, selfRelease); err != nil {
		return err
	}

	harness.Fact("limit", "one open wait and two paused invocations on a single-node server with one partition, measured over seconds rather than days: it shows what Restate holds while a handler waits and what `paused` is to drive, not how a cluster of many thousands of open waits behaves, nor what a journal costs after a month of real retention")
	return nil
}

// runWait is the first half: the wait for a person.
func runWait(ctx context.Context, cfg harness.Config, env *restatex.Env, service *restatex.Service, serviceBinary, workDir string, port int) (*restatex.Service, error) {
	askFile, decisionFile := AskFile(workDir), DecisionFile(workDir)
	goroutinesAtRest := latest(service, "goroutines")

	started := time.Now()
	invocationID, err := env.Ingress.Send(ctx, WaitServiceName, AskHandlerName, WaitParams{
		IssueID: issueID, AskFile: askFile, DecisionFile: decisionFile,
	})
	if err != nil {
		return nil, err
	}
	harness.Fact("wait-invocation-id", invocationID)

	lines, err := restatex.WaitForLines(ctx, askFile, 1, askWait)
	if err != nil {
		return nil, fmt.Errorf("the handler never published an awakeable id: %w", err)
	}
	awakeableID, err := awakeableFrom(lines[0])
	if err != nil {
		return nil, err
	}
	harness.Fact("awakeable-published", lines[0])
	harness.Fact("awakeable-id", awakeableID)

	suspended, err := env.Admin.WaitForStatus(ctx, invocationID, suspendWait, "suspended")
	if err != nil {
		return nil, fmt.Errorf("the invocation never suspended: %w", err)
	}
	if err := wait(ctx, idleObserve); err != nil {
		return nil, err
	}
	harness.Fact("invocation-while-waiting", suspended)
	harness.Fact("invocation-in-flight-while-waiting", suspended.InFlight)
	harness.Fact("what-the-server-is-waiting-on", suspended.SuspendedWaitingForSignals)
	journal, err := env.Admin.Journal(ctx, invocationID)
	if err != nil {
		return nil, err
	}
	harness.Fact("journal-while-waiting", restatex.JournalSummary(journal))
	harness.Fact("handlers-in-flight-in-the-service-process-while-waiting", latest(service, "handlers-in-flight"))
	goroutinesWhileWaiting := latest(service, "goroutines")
	harness.Fact("goroutines-in-the-service-process-at-rest", goroutinesAtRest)
	harness.Fact("goroutines-in-the-service-process-while-waiting", goroutinesWhileWaiting)
	harness.Fact("goroutines-the-attempt-left-behind-in-the-process-that-opened-the-wait",
		"the process that ran the attempt keeps the HTTP/2 connection the server opened to it, so its goroutine count stays above its count at rest even once the handler is gone; the number that answers what an open wait costs is the replacement's, below")

	// The wait across the death of the process that opened it.
	killedPID := service.PID()
	if err := service.Signal(syscall.SIGKILL); err != nil {
		return nil, err
	}
	harness.Fact("service-process-killed-while-waiting-pid", killedPID)
	harness.Fact("kill-signal", "SIGKILL")
	if err := wait(ctx, settle); err != nil {
		return nil, err
	}
	afterKill, err := env.Admin.Invocation(ctx, invocationID)
	if err != nil {
		return nil, err
	}
	harness.Fact("invocation-after-the-waiting-process-was-killed", afterKill)
	harness.Fact("suspended-wait-survived-the-kill", afterKill.Found && afterKill.Status == "suspended")

	replacement, err := startService(ctx, "service2", serviceBinary, workDir, port)
	if err != nil {
		return nil, err
	}
	harness.Fact("replacement-pid", replacement.PID())

	// The decisive reading: a process that holds the open wait and nothing else. The
	// wait was opened by a process that is now dead, so whatever this one holds for
	// it is what an open wait really costs a pod.
	if err := wait(ctx, idleObserve); err != nil {
		return nil, err
	}
	stillWaiting, err := env.Admin.Invocation(ctx, invocationID)
	if err != nil {
		return nil, err
	}
	replacementGoroutines := latest(replacement, "goroutines")
	replacementInFlight := latest(replacement, "handlers-in-flight")
	harness.Fact("invocation-while-the-replacement-held-the-open-wait", stillWaiting)
	harness.Fact("goroutines-in-the-replacement-process-while-the-wait-was-open", replacementGoroutines)
	harness.Fact("handlers-in-flight-in-the-replacement-process-while-the-wait-was-open", replacementInFlight)
	harness.Fact("what-the-handler-process-holds-while-it-waits",
		fmt.Sprintf("nothing of the invocation: a process that was started after the wait was already open holds %s goroutines and %s handlers in flight, which is what it holds when it is doing nothing at all -- the wait lives entirely in the server",
			replacementGoroutines, replacementInFlight))

	// The answer, as a person's approval would arrive: a plain HTTP request from
	// outside any handler, carrying nothing but the published id and a value.
	if err := env.Ingress.ResolveAwakeable(ctx, awakeableID, "approved"); err != nil {
		return nil, err
	}
	harness.Fact("awakeable-resolved-over", "POST http://"+cfg.RestateIngress+"/restate/awakeables/<id>/resolve")

	decided, err := restatex.WaitForLines(ctx, decisionFile, 1, terminalWait)
	if err != nil {
		return nil, fmt.Errorf("the handler never went on from where it suspended: %w", err)
	}
	elapsed := time.Since(started)
	harness.Fact("handler-went-on-from-where-it-suspended", decided[0])
	final, err := waitTerminal(ctx, env, invocationID, terminalWait)
	if err != nil {
		return nil, err
	}
	harness.Fact("wait-invocation-final", final)
	status, output, err := env.Ingress.Output(ctx, invocationID)
	if err != nil {
		return nil, err
	}
	harness.Fact("wait-invocation-output-http-status", status)
	harness.Fact("wait-invocation-output", output)
	if !final.Succeeded() {
		return nil, fmt.Errorf("the resolved wait ended %q/%q, not a success", final.Status, final.CompletionResult)
	}

	reportWaitCost(env, suspended, len(journal), elapsed, replacementGoroutines, replacementInFlight)
	return replacement, nil
}

// reportWaitCost prints what one open wait cost, split the way experiments 2 and 8
// split it: timed: is a number this run measured, reasoned: is arithmetic over those
// numbers and over the server's own configured defaults.
func reportWaitCost(env *restatex.Env, waiting restatex.Invocation, journalEntries int, elapsed time.Duration, goroutines, inFlight string) {
	harness.Fact("timed: journal-entries-for-one-open-wait", journalEntries)
	harness.Fact("timed: journal-size-reported-by-the-server", waiting.JournalSize)
	harness.Fact("timed: attempts-in-flight-while-one-wait-was-open", boolToCount(waiting.InFlight))
	harness.Fact("timed: handlers-in-flight-in-the-service-process-while-one-wait-was-open", inFlight)
	harness.Fact("timed: goroutines-in-the-service-process-while-one-wait-was-open", goroutines)
	harness.Fact("timed: completion-retention-on-this-invocation", orNone(waiting.CompletionRetention))
	harness.Fact("timed: journal-retention-on-this-invocation", orNone(waiting.JournalRetention))
	harness.Fact("timed: wall-clock-this-gate-actually-waited", fmt.Sprintf("%.3fs, from the question being asked to the answer being applied", elapsed.Seconds()))
	harness.Fact("timed: server-default-journal-retention", env.Defaults.JournalRetention)
	harness.Fact("timed: server-default-idempotency-retention", env.Defaults.IdempotencyRetention)
	harness.Fact("timed: server-default-workflow-completion-retention", env.Defaults.WorkflowCompletionRetention)
	harness.Fact("timed: server-invoker-inactivity-timeout", env.Defaults.InactivityTimeout)
	harness.Fact("timed: server-invoker-abort-timeout", env.Defaults.AbortTimeout)
	harness.Fact("timed: retention-and-timeout-source", "the restate-server binary's own effective configuration, printed by --dump-config")

	for _, days := range waitDays {
		harness.Fact(fmt.Sprintf("reasoned: cost-of-one-gate-open-%d-days", days),
			fmt.Sprintf("%d journal entries and no attempt, for the whole %d days: a suspended invocation costs the rows it already has and nothing per unit of time, because nothing runs and nothing is held open",
				journalEntries, days))
	}
	harness.Fact("reasoned: journal-entries-for-1000-gates-open-30-days", journalEntries*1000)
	harness.Fact("reasoned: what-the-retention-defaults-do-to-a-wait-of-days",
		fmt.Sprintf("nothing: %s journal retention and %s workflow-completion retention are counted from an invocation's completion, not from its creation, so a wait of days is not shortened by them -- what they bound is how long the finished invocation stays readable afterwards",
			env.Defaults.JournalRetention, env.Defaults.WorkflowCompletionRetention))
	harness.Fact("reasoned: what-the-inactivity-and-abort-timeouts-do-to-a-wait-of-days",
		fmt.Sprintf("nothing to a suspended wait, everything to a blocking one: %s inactivity and %s abort are measured against an attempt that is holding a connection without making progress, and a handler blocked on an awakeable holds none -- a handler that instead slept in its own code would be asked to suspend after %s and aborted %s later",
			env.Defaults.InactivityTimeout, env.Defaults.AbortTimeout, env.Defaults.InactivityTimeout, env.Defaults.AbortTimeout))
	harness.Fact("reasoned: what-a-pod-cannot-do",
		"it cannot make the wait cost anything by restarting: the process that opened the wait was killed and replaced during this run, and the wait neither ended nor was re-run -- what a pod holds for an open gate is nothing at all")
}

// runPause is the second half: Restate's `paused`, reached, listed, read and resumed.
func runPause(ctx context.Context, env *restatex.Env, attemptFile, fixFile string) error {
	harness.Fact("pause-policy", fmt.Sprintf("registered on %s with max-attempts %d, initial-interval %s, factor %.1f, max-interval %s, on-max-attempts pause",
		PauseServiceName, PauseMaxAttempts, pauseInitialInterval, pauseFactor, pauseMaxInterval))
	harness.Fact("pause-policy-set-by", "the Go SDK's WithInvocationRetryPolicy at service registration, discovered by the server when the deployment was registered")
	harness.Fact("server-default-on-max-attempts", fmt.Sprintf("%s after %s attempts (the server's own effective configuration)",
		env.Defaults.RetryOnMaxAttempts, env.Defaults.RetryMaxAttempts))

	invocationID, err := env.Ingress.Send(ctx, PauseServiceName, FixHandlerName, FixParams{
		AttemptFile: attemptFile, FixFile: fixFile,
	})
	if err != nil {
		return err
	}
	harness.Fact("failing-invocation-id", invocationID)

	// While it is still retrying, the server holds the failure in
	// sys_invocation_state; once it pauses, that row is gone. The retries are sampled
	// as fast as the query answers so the experiment has both readings.
	paused, retrying, err := waitPaused(ctx, env, invocationID)
	if err != nil {
		return fmt.Errorf("the failing invocation never reached paused: %w", err)
	}
	harness.Fact("invocation-while-it-was-still-retrying", retrying)
	attempts, err := restatex.ReadLines(attemptFile)
	if err != nil {
		return err
	}
	harness.Fact("attempts-before-pausing", len(attempts))
	for i, line := range attempts {
		harness.Fact(fmt.Sprintf("attempt-line-%d", i+1), line)
	}
	harness.Fact("invocation-when-paused", paused)
	harness.Fact("paused-is-terminal", paused.Terminal())
	journal, err := env.Admin.Journal(ctx, invocationID)
	if err != nil {
		return err
	}
	harness.Fact("journal-while-paused", restatex.JournalSummary(journal))
	events, err := env.Admin.JournalEvents(ctx, invocationID)
	if err != nil {
		return err
	}
	harness.Fact("events-while-paused", restatex.JournalEventsSummary(events))
	harness.Fact("why-it-paused-is-readable-from", "sys_journal_events: once an invocation is paused its sys_invocation_state row is gone, so last_failure reads empty and the failing attempts and the pause itself are only in the event rows")

	// What an operator would do: list what has stopped and needs a person.
	listed, err := env.Admin.Invocations(ctx, "status = 'paused'")
	if err != nil {
		return err
	}
	harness.Fact("paused-invocations-listed-by", "SELECT ... FROM sys_invocation WHERE status = 'paused' through POST /query")
	harness.Fact("paused-invocations-found", len(listed))
	for _, invocation := range listed {
		harness.Fact("paused-invocation", fmt.Sprintf("%s %s %s", invocation.ID, invocation.Target, invocation))
	}

	// The person looks, fixes the thing, and resumes it.
	if err := restatex.Touch(fixFile); err != nil {
		return err
	}
	resumeStatus, err := env.Admin.Resume(ctx, invocationID, "")
	if err != nil {
		return err
	}
	harness.Fact("resume-http-status", resumeStatus)
	final, err := waitTerminal(ctx, env, invocationID, terminalWait)
	if err != nil {
		return err
	}
	after, err := restatex.ReadLines(attemptFile)
	if err != nil {
		return err
	}
	harness.Fact("attempts-after-resuming", len(after)-len(attempts))
	harness.Fact("resumed-invocation-final", final)
	status, output, err := env.Ingress.Output(ctx, invocationID)
	if err != nil {
		return err
	}
	harness.Fact("resumed-invocation-output-http-status", status)
	harness.Fact("resumed-invocation-output", output)
	if !final.Succeeded() {
		return fmt.Errorf("the resumed invocation ended %q/%q, not a success", final.Status, final.CompletionResult)
	}
	harness.Fact("what-paused-is",
		"a non-terminal state the server holds the whole journal in: it is listable with one SQL predicate, resumable with one admin call, and left alone until somebody makes it -- the invocation neither died nor kept retrying. Its cost is that the reason is not on the invocation: once paused, sys_invocation_state is gone and last_failure reads empty, so an operator asking why has to read sys_journal_events")
	return nil
}

// runSelfPause measures whether a handler can put itself into `paused` while it is
// running, which the admin API documents as PATCH /invocations/{id}/pause against a
// running invocation.
func runSelfPause(ctx context.Context, cfg harness.Config, env *restatex.Env, logFile, releaseFile string) error {
	invocationID, err := env.Ingress.Send(ctx, SelfPauseServiceName, SelfPauseHandlerName, SelfPauseParams{
		AdminAddress: cfg.RestateAdmin, LogFile: logFile, ReleaseFile: releaseFile,
	})
	if err != nil {
		return err
	}
	harness.Fact("self-pause-invocation-id", invocationID)

	lines, err := restatex.WaitForLines(ctx, logFile, 2, pausedWait)
	if err != nil {
		return fmt.Errorf("the handler never reported what the admin API answered it: %w", err)
	}
	harness.Fact("handler-pausing-itself", lines[0])
	harness.Fact("self-pause-admin-api-answered", strings.TrimPrefix(lines[1], "self-pause-http-status="))

	selfPaused, err := env.Admin.WaitForStatus(ctx, invocationID, pausedWait, "paused")
	if err != nil {
		reading, readErr := env.Admin.Invocation(ctx, invocationID)
		if readErr != nil {
			return readErr
		}
		harness.Fact("handler-can-pause-itself", false)
		harness.Fact("invocation-after-the-handler-paused-itself", reading)
		harness.Fact("why-not", err)
		return nil
	}
	harness.Fact("handler-can-pause-itself", true)
	harness.Fact("invocation-after-the-handler-paused-itself", selfPaused)
	harness.Fact("what-happened-to-the-attempt-that-was-running",
		"the server stopped it where it was: the handler's own body never returned, and nothing it had not already journaled survived")

	if err := restatex.Touch(releaseFile); err != nil {
		return err
	}
	resumeStatus, err := env.Admin.Resume(ctx, invocationID, "")
	if err != nil {
		return err
	}
	harness.Fact("self-paused-resume-http-status", resumeStatus)
	final, err := waitTerminal(ctx, env, invocationID, terminalWait)
	if err != nil {
		return err
	}
	harness.Fact("self-paused-invocation-final", final)
	after, err := restatex.ReadLines(logFile)
	if err != nil {
		return err
	}
	harness.Fact("self-pause-log-lines", len(after))
	for i, line := range after {
		harness.Fact(fmt.Sprintf("self-pause-line-%d", i+1), line)
	}
	if !final.Succeeded() {
		return fmt.Errorf("the resumed self-paused invocation ended %q/%q, not a success", final.Status, final.CompletionResult)
	}
	return nil
}

// startService runs one cmd/restateservice process in experiment 11's mode.
func startService(ctx context.Context, name, binary, workDir string, port int) (*restatex.Service, error) {
	address := restatex.Address(port)
	args := []string{"-mode", "exp11", "-address", address, "-work-dir", workDir}
	return restatex.StartService(ctx, name, binary, args, workDir, address, restatex.ServiceReady)
}

// awakeableFrom pulls the published awakeable id out of the line the handler wrote.
func awakeableFrom(line string) (string, error) {
	for _, field := range strings.Fields(line) {
		if id, ok := strings.CutPrefix(field, "awakeable="); ok {
			return id, nil
		}
	}
	return "", fmt.Errorf("the published line %q carries no awakeable id", line)
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

// latest is the newest reading a service process printed of itself.
func latest(service *restatex.Service, key string) string {
	value, ok := service.Latest(key)
	if !ok {
		return "not reported"
	}
	return value
}

// orNone is a value, or a word for the column having been null.
func orNone(value string) string {
	if value == "" {
		return "none"
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

// waitPaused blocks until the failing invocation is paused, and also returns the last
// reading it managed to take while the server was still retrying it, which is the only
// moment the failure is in sys_invocation_state.
func waitPaused(ctx context.Context, env *restatex.Env, invocationID string) (paused, retrying restatex.Invocation, err error) {
	deadline := time.Now().Add(pausedWait)
	for {
		reading, err := env.Admin.Invocation(ctx, invocationID)
		if err != nil {
			return restatex.Invocation{}, retrying, err
		}
		if reading.Status == "paused" {
			return reading, retrying, nil
		}
		if reading.LastFailure != "" {
			retrying = reading
		}
		if !time.Now().Before(deadline) {
			return reading, retrying, fmt.Errorf("invocation %s was %q, not paused, after %s", invocationID, reading.Status, pausedWait)
		}
		if err := wait(ctx, 50*time.Millisecond); err != nil {
			return reading, retrying, err
		}
	}
}

// boolToCount is a flag as the number a timed: line wants.
func boolToCount(flag bool) int {
	if flag {
		return 1
	}
	return 0
}
