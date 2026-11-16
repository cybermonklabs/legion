package exp12

import (
	"context"
	"fmt"
	"strings"
	"syscall"
	"time"

	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/harness"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/restatex"
)

// The experiment's timings.
const (
	stepWait     = 30 * time.Second
	terminalWait = 45 * time.Second
	// replayObserve is how long the invocation whose code changed underneath it is
	// watched, which is long enough for several of the server's retries.
	replayObserve = 10 * time.Second
	sample        = 2 * time.Second
	settle        = 500 * time.Millisecond
)

// The three runs this experiment drives. Each is one invocation with a release file of
// its own, so one can be held in the middle of its journal while another finishes.
const (
	// runHeldOnOldCode is started on the old code and is still unfinished when the
	// changed code is deployed at a new endpoint: the supported way.
	runHeldOnOldCode = "held-on-old-code"
	// runOnNewCode is started after that deployment, so it is the first run of the
	// changed code.
	runOnNewCode = "on-new-code"
	// runReplayedAgainstChangedCode is started on the old code and then has the
	// changed code put behind the very endpoint it is pinned to: the unsupported way.
	runReplayedAgainstChangedCode = "replayed-against-changed-code"
)

// Run is experiment 12. It records a run's journal under one version of a handler and
// then measures two things: what Restate does when that journal is replayed against a
// changed handler at the same endpoint, and whether the supported way -- a new
// deployment, with the old one kept for the invocations already running on it -- works
// as documented.
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
	stepFile := StepFile(workDir)
	if err := restatex.RemoveAll(stepFile,
		ReleaseFile(workDir, runHeldOnOldCode),
		ReleaseFile(workDir, runOnNewCode),
		ReleaseFile(workDir, runReplayedAgainstChangedCode)); err != nil {
		return err
	}

	env, err := restatex.StartEnv(ctx, restatex.EnvOptions{
		Experiment: 12, WorkDir: workDir, ServerBinary: serverBinary,
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
	harness.Fact("the-change", "one new durable step, review, inserted between plan and implement -- the ordinary Legion change")
	harness.Fact("recorded-code-steps", fmt.Sprintf("%v then %s", variants[VariantRecorded], StepDeliver))
	harness.Fact("changed-code-steps", fmt.Sprintf("%v then %s", variants[VariantChanged], StepDeliver))

	if err := runSupportedWay(ctx, env, serviceBinary, workDir); err != nil {
		return err
	}
	if err := runUnsupportedWay(ctx, env, serviceBinary, workDir); err != nil {
		return err
	}

	harness.Fact("limit", "one service, one inserted step, two deployments and one replay on a single-node server with one partition: it shows what Restate's shared core does with a journal whose code moved on and whether deployment pinning keeps an in-flight invocation on the code it started with, not what a change of a handler's input or output type does, nor what happens when many versions are live at once")
	return nil
}

// runSupportedWay is Restate's documented answer to changing a handler: deploy the new
// code at a new endpoint and register it. The invocation already running stays pinned
// to the deployment it started on and finishes on the old code; the next invocation
// goes to the new one.
func runSupportedWay(ctx context.Context, env *restatex.Env, serviceBinary, workDir string) error {
	oldPort, err := restatex.FreePort()
	if err != nil {
		return err
	}
	old, err := startService(ctx, "service-recorded", serviceBinary, workDir, oldPort, VariantRecorded)
	if err != nil {
		return err
	}
	defer old.Stop()
	oldDeployment, err := env.Admin.Register(ctx, old.URL(), false)
	if err != nil {
		return err
	}
	revision, latest, err := env.Admin.ServiceRevisions(ctx, ServiceName)
	if err != nil {
		return err
	}
	harness.Fact("old-deployment-id", oldDeployment.ID)
	harness.Fact("old-deployment-endpoint", old.URL())
	harness.Fact("service-revision-after-the-old-deployment", revision)
	harness.Fact("deployment-new-invocations-go-to-after-the-old-deployment", latest)

	// A run on the old code, caught in its last step with its journal half recorded.
	held, err := env.Ingress.Send(ctx, ServiceName, HandlerName, NewParams(workDir, runHeldOnOldCode))
	if err != nil {
		return err
	}
	harness.Fact("held-invocation-id", held)
	if _, err := waitForSteps(ctx, workDir, runHeldOnOldCode, 3); err != nil {
		return fmt.Errorf("the run on the old code never reached its last step: %w", err)
	}
	beforeDeploy, err := env.Admin.Invocation(ctx, held)
	if err != nil {
		return err
	}
	beforeJournal, err := env.Admin.Journal(ctx, held)
	if err != nil {
		return err
	}
	harness.Fact("held-invocation-before-the-new-deployment", beforeDeploy)
	harness.Fact("held-invocation-journal-before-the-new-deployment", restatex.JournalSummary(beforeJournal))

	// The change, deployed the supported way: a new endpoint, registered as a new
	// deployment of the same service.
	newPort, err := restatex.FreePort()
	if err != nil {
		return err
	}
	changed, err := startService(ctx, "service-changed", serviceBinary, workDir, newPort, VariantChanged)
	if err != nil {
		return err
	}
	defer changed.Stop()
	newDeployment, err := env.Admin.Register(ctx, changed.URL(), false)
	if err != nil {
		return err
	}
	revision, latest, err = env.Admin.ServiceRevisions(ctx, ServiceName)
	if err != nil {
		return err
	}
	harness.Fact("new-deployment-id", newDeployment.ID)
	harness.Fact("new-deployment-endpoint", changed.URL())
	harness.Fact("service-revision-after-the-new-deployment", revision)
	harness.Fact("deployment-new-invocations-go-to-after-the-new-deployment", latest)
	harness.Fact("both-deployments-registered", mustJoin(env.Admin.DeploymentEndpoints(ctx)))

	if err := wait(ctx, settle); err != nil {
		return err
	}
	afterDeploy, err := env.Admin.Invocation(ctx, held)
	if err != nil {
		return err
	}
	harness.Fact("held-invocation-after-the-new-deployment", afterDeploy)
	harness.Fact("held-invocation-stayed-on-the-old-deployment", afterDeploy.PinnedDeployment == oldDeployment.ID)

	// The next invocation, which must reach the new code.
	if err := restatex.Touch(ReleaseFile(workDir, runOnNewCode)); err != nil {
		return err
	}
	fresh, err := env.Ingress.Send(ctx, ServiceName, HandlerName, NewParams(workDir, runOnNewCode))
	if err != nil {
		return err
	}
	harness.Fact("new-invocation-id", fresh)
	freshFinal, err := waitTerminal(ctx, env, fresh, terminalWait)
	if err != nil {
		return err
	}
	freshSteps, err := stepsOf(workDir, runOnNewCode)
	if err != nil {
		return err
	}
	harness.Fact("new-invocation-final", freshFinal)
	harness.Fact("new-invocation-pinned-to-the-new-deployment", freshFinal.PinnedDeployment == newDeployment.ID)
	harness.Fact("new-invocation-steps", strings.Join(freshSteps, ","))

	// And the held one finishes on the code it started with.
	if err := restatex.Touch(ReleaseFile(workDir, runHeldOnOldCode)); err != nil {
		return err
	}
	heldFinal, err := waitTerminal(ctx, env, held, terminalWait)
	if err != nil {
		return err
	}
	heldSteps, err := stepsOf(workDir, runHeldOnOldCode)
	if err != nil {
		return err
	}
	harness.Fact("held-invocation-final", heldFinal)
	harness.Fact("held-invocation-steps", strings.Join(heldSteps, ","))
	harness.Fact("held-invocation-ran-the-inserted-step", contains(heldSteps, StepReview))
	harness.Fact("new-invocation-ran-the-inserted-step", contains(freshSteps, StepReview))

	worked := heldFinal.Succeeded() && freshFinal.Succeeded() &&
		!contains(heldSteps, StepReview) && contains(freshSteps, StepReview) &&
		heldFinal.PinnedDeployment == oldDeployment.ID && freshFinal.PinnedDeployment == newDeployment.ID
	harness.Fact("deployment-versioning-works-as-documented", worked)
	if !worked {
		return fmt.Errorf("the supported way did not behave as documented: the held run ended %q with steps %v on deployment %s, and the new run ended %q with steps %v on deployment %s",
			heldFinal.Status, heldSteps, heldFinal.PinnedDeployment, freshFinal.Status, freshSteps, freshFinal.PinnedDeployment)
	}
	return nil
}

// runUnsupportedWay is the ordinary mistake: the changed binary is deployed behind the
// very endpoint an unfinished invocation is pinned to, so the server replays a journal
// recorded by the old code into the new code.
func runUnsupportedWay(ctx context.Context, env *restatex.Env, serviceBinary, workDir string) error {
	port, err := restatex.FreePort()
	if err != nil {
		return err
	}
	recorded, err := startService(ctx, "service-recorded-2", serviceBinary, workDir, port, VariantRecorded)
	if err != nil {
		return err
	}
	defer recorded.Stop()
	deployment, err := env.Admin.Register(ctx, recorded.URL(), false)
	if err != nil {
		return err
	}
	harness.Fact("replay-deployment-id", deployment.ID)
	harness.Fact("replay-deployment-endpoint", recorded.URL())

	invocation, err := env.Ingress.Send(ctx, ServiceName, HandlerName, NewParams(workDir, runReplayedAgainstChangedCode))
	if err != nil {
		return err
	}
	harness.Fact("replayed-invocation-id", invocation)
	if _, err := waitForSteps(ctx, workDir, runReplayedAgainstChangedCode, 3); err != nil {
		return fmt.Errorf("the run to be replayed never reached its last step: %w", err)
	}
	recordedJournal, err := env.Admin.Journal(ctx, invocation)
	if err != nil {
		return err
	}
	stepsBefore, err := stepsOf(workDir, runReplayedAgainstChangedCode)
	if err != nil {
		return err
	}
	harness.Fact("journal-recorded-by-the-old-code", restatex.JournalSummary(recordedJournal))
	harness.Fact("steps-the-old-code-ran", strings.Join(stepsBefore, ","))

	// The change, deployed the way that is not supported: the same endpoint URL, a
	// different binary behind it, and no new deployment registered.
	killed := recorded.PID()
	if err := recorded.Signal(syscall.SIGKILL); err != nil {
		return err
	}
	harness.Fact("old-code-process-killed-pid", killed)
	changed, err := startService(ctx, "service-changed-2", serviceBinary, workDir, port, VariantChanged)
	if err != nil {
		return err
	}
	defer changed.Stop()
	harness.Fact("changed-code-process-pid", changed.PID())
	harness.Fact("changed-code-endpoint", changed.URL())
	harness.Fact("deployment-was-re-registered", false)

	// What the server does with the journal it recorded, now that the code behind
	// the endpoint it is pinned to has moved on.
	var last restatex.Invocation
	deadline := time.Now().Add(replayObserve)
	for i := 1; time.Now().Before(deadline); i++ {
		if err := wait(ctx, sample); err != nil {
			return err
		}
		last, err = env.Admin.Invocation(ctx, invocation)
		if err != nil {
			return err
		}
		harness.Fact(fmt.Sprintf("replayed-invocation-sample-%d", i), last)
		if last.Terminal() {
			break
		}
	}
	stepsAfter, err := stepsOf(workDir, runReplayedAgainstChangedCode)
	if err != nil {
		return err
	}
	events, err := env.Admin.JournalEvents(ctx, invocation)
	if err != nil {
		return err
	}
	replayJournal, err := env.Admin.Journal(ctx, invocation)
	if err != nil {
		return err
	}
	harness.Fact("steps-after-the-code-changed", strings.Join(stepsAfter, ","))
	harness.Fact("journal-after-the-code-changed", restatex.JournalSummary(replayJournal))
	harness.Fact("events-after-the-code-changed", restatex.JournalEventsSummary(events))
	failure := restatex.Message(firstNonEmpty(last.CompletionFailure, last.LastFailure, errorFrom(events)))
	harness.Fact("divergence-detected-by-the-server", failure != "")
	harness.Fact("divergence-error", firstNonEmpty(failure, "none: the server reported no failure, so it did not detect the change"))
	harness.Fact("divergence-error-code", orNone(firstNonEmpty(last.LastFailureErrorCode, codeFrom(events))))
	harness.Fact("invocation-after-the-divergence", fate(last))
	harness.Fact("is-a-journal-mismatch-terminal", "no: the server records it as a TransientError and retries the invocation against the same pinned deployment, so a change deployed this way leaves the invocation retrying rather than failing")
	harness.Fact("if-nobody-intervenes", fmt.Sprintf("it keeps retrying on the server-wide policy -- initial-interval %s, factor %s, max-interval %s -- until max-attempts %s, and then %s",
		env.Defaults.RetryInitialInterval, env.Defaults.RetryFactor, env.Defaults.RetryMaxInterval,
		env.Defaults.RetryMaxAttempts, env.Defaults.RetryOnMaxAttempts))
	harness.Fact("what-an-operator-can-do-about-it", "put the old code back behind that endpoint, or resume the invocation onto another deployment with PATCH /invocations/{id}/resume?deployment=<id>, or kill it")

	// Whatever it did, the invocation is brought to an end, so the run leaves nothing
	// retrying behind it.
	if !last.Terminal() {
		if err := restatex.Touch(ReleaseFile(workDir, runReplayedAgainstChangedCode)); err != nil {
			return err
		}
		status, err := env.Admin.Kill(ctx, invocation)
		if err != nil {
			return err
		}
		harness.Fact("kill-http-status", status)
		last, err = waitTerminal(ctx, env, invocation, terminalWait)
		if err != nil {
			return err
		}
	}
	harness.Fact("replayed-invocation-final", last)
	harness.Fact("the-supported-way",
		"a new deployment: register the changed code at a new endpoint URL, leave the old endpoint running, and Restate keeps every in-flight invocation on the deployment it was pinned to while sending new ones to the latest -- which is what the first half of this experiment measured and found to hold")
	return nil
}

// startService runs one cmd/restateservice process in experiment 12's mode, serving
// one variant of the code.
func startService(ctx context.Context, name, binary, workDir string, port int, variant string) (*restatex.Service, error) {
	address := restatex.Address(port)
	args := []string{"-mode", "exp12", "-address", address, "-work-dir", workDir, "-variant", variant}
	return restatex.StartService(ctx, name, binary, args, workDir, address, restatex.ServiceReady)
}

// waitForSteps blocks until one run has written at least want step lines.
func waitForSteps(ctx context.Context, workDir, run string, want int) ([]string, error) {
	deadline := time.Now().Add(stepWait)
	for {
		steps, err := stepsOf(workDir, run)
		if err != nil {
			return nil, err
		}
		if len(steps) >= want {
			return steps, nil
		}
		if !time.Now().Before(deadline) {
			return steps, fmt.Errorf("run %s had written %d steps, not %d, after %s", run, len(steps), want, stepWait)
		}
		if err := wait(ctx, 100*time.Millisecond); err != nil {
			return steps, err
		}
	}
}

// stepsOf is the names of the steps one run really ran, in order, picked out of the
// one file every run writes to.
func stepsOf(workDir, run string) ([]string, error) {
	lines, err := restatex.ReadLines(StepFile(workDir))
	if err != nil {
		return nil, err
	}
	var steps []string
	for _, line := range lines {
		if !strings.Contains(line, "run="+run+" ") {
			continue
		}
		for _, field := range strings.Fields(line) {
			if name, ok := strings.CutPrefix(field, "step="); ok {
				steps = append(steps, name)
			}
		}
	}
	return steps, nil
}

// fate is the one-line answer to "killed, paused or retried" for an invocation whose
// code changed underneath it.
func fate(invocation restatex.Invocation) string {
	switch {
	case invocation.Succeeded():
		return "ran to the end: the server accepted the replay against the changed code"
	case invocation.Terminal():
		return "ended with a failure and will not be tried again: " + restatex.Message(invocation.CompletionFailure)
	case invocation.Status == "paused":
		return "paused: the server stopped retrying and is waiting for a person"
	case invocation.Status == "backing-off", invocation.Status == "ready", invocation.Status == "running":
		return "retried: the server is attempting it again, against the same pinned deployment, and has not given up"
	default:
		return invocation.Status
	}
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

// contains reports whether a step ran.
func contains(steps []string, name string) bool {
	for _, step := range steps {
		if step == name {
			return true
		}
	}
	return false
}

// firstNonEmpty is the first of the given strings that says something.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// orNone is a value, or a word for its being absent.
func orNone(value string) string {
	if value == "" {
		return "none"
	}
	return value
}

// mustJoin turns a listing and its error into one fact value, so a reading that failed
// says so in the line rather than ending the run.
func mustJoin(values []string, err error) string {
	if err != nil {
		return "unreadable: " + err.Error()
	}
	return strings.Join(values, " ")
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

// errorFrom is the message of the last failure an invocation's events recorded, which
// is where the reason lives once sys_invocation_state has gone.
func errorFrom(events []restatex.JournalEvent) string {
	message := ""
	for _, event := range events {
		if event.Message != "" {
			message = event.Message
		}
	}
	return restatex.Message(message)
}

// codeFrom is the code of the last failure an invocation's events recorded.
func codeFrom(events []restatex.JournalEvent) string {
	code := ""
	for _, event := range events {
		if event.Code != 0 {
			code = fmt.Sprint(event.Code)
		}
	}
	return code
}
