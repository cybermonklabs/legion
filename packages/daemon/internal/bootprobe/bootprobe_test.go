package bootprobe

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"testing"
	"time"
)

// plan answers one outcome per attempt, repeating the last, and counts the attempts.
type plan struct {
	outcomes []Outcome
	attempts int
}

func (p *plan) attempt(context.Context) Outcome {
	p.attempts++
	if p.attempts <= len(p.outcomes) {
		return p.outcomes[p.attempts-1]
	}
	return p.outcomes[len(p.outcomes)-1]
}

func transient(detail string) Outcome { return Outcome{Detail: detail} }

func waiting(detail string) Outcome { return Outcome{Waiting: detail} }

var passed = Outcome{Passed: true}

// quick is a policy whose waits cost the suite nothing: 1 ms doubling to a 4 ms cap.
func quick(attempts int) Retry {
	return Retry{Initial: time.Millisecond, Max: 4 * time.Millisecond, Attempts: attempts}
}

func logger(buffer *bytes.Buffer) *slog.Logger { return slog.New(slog.NewTextHandler(buffer, nil)) }

func TestRunStopsAtTheFirstPass(t *testing.T) {
	p := &plan{outcomes: []Outcome{transient("database is locked"), passed, transient("never run")}}
	var logged bytes.Buffer

	if err := Run(context.Background(), "OMP pi.agents", quick(6), logger(&logged), p.attempt); err != nil {
		t.Fatalf("Run = %v, want the pass", err)
	}
	if p.attempts != 2 {
		t.Errorf("attempts = %d, want a transient failure and the pass", p.attempts)
	}
	if got := strings.Count(logged.String(), "failed transiently"); got != 1 {
		t.Errorf("logged %d transient failures, want 1:\n%s", got, logged.String())
	}
}

// A refusal is an answer no retry changes: it is returned as it is, at once.
func TestRunReturnsARefusalWithoutRetrying(t *testing.T) {
	refusal := errors.New("pi-legion 1.57.0 is installed but not loaded by omp")
	p := &plan{outcomes: []Outcome{{Refusal: refusal}, passed}}

	err := Run(context.Background(), "pi-legion load", quick(6), logger(&bytes.Buffer{}), p.attempt)

	if !errors.Is(err, refusal) || err.Error() != refusal.Error() {
		t.Fatalf("Run = %v, want the refusal itself", err)
	}
	if p.attempts != 1 {
		t.Errorf("attempts = %d, want 1", p.attempts)
	}
}

// A bounded policy gives up after its attempts, naming the probe, the bound, and what the last
// attempt said — a probe that never answered, not one that answered no.
func TestRunGivesUpAfterItsAttemptsNamingTheLastDetail(t *testing.T) {
	p := &plan{outcomes: []Outcome{transient("first"), transient("second"), transient("command timed out after 300s")}}
	var logged bytes.Buffer

	err := Run(context.Background(), "OMP session storage setting", quick(3), logger(&logged), p.attempt)

	want := "the OMP session storage setting probe never completed within its retry budget (3 attempts): command timed out after 300s"
	if err == nil || err.Error() != want {
		t.Fatalf("Run = %v, want %q", err, want)
	}
	if p.attempts != 3 {
		t.Errorf("attempts = %d, want the bound, 3", p.attempts)
	}
	if got := strings.Count(logged.String(), "failed transiently"); got != 2 {
		t.Errorf("logged %d transient failures, want 2 (none after the last attempt):\n%s", got, logged.String())
	}
}

// Without a bound, transient failures are waited out however many there are, each wait doubling
// from Initial and capped at Max.
func TestRunWithoutABoundWaitsOutEveryTransientFailure(t *testing.T) {
	p := &plan{outcomes: []Outcome{transient("1"), transient("2"), transient("3"), transient("4"), transient("5"), passed}}
	var logged bytes.Buffer

	if err := Run(context.Background(), "pi-legion load", quick(0), logger(&logged), p.attempt); err != nil {
		t.Fatalf("Run = %v, want the pass after five transient failures", err)
	}
	if p.attempts != 6 {
		t.Errorf("attempts = %d, want 6", p.attempts)
	}
	var waits []string
	for _, match := range regexp.MustCompile(`retryIn=(\S+)`).FindAllStringSubmatch(logged.String(), -1) {
		waits = append(waits, match[1])
	}
	if got, want := strings.Join(waits, " "), "1ms 2ms 4ms 4ms 4ms"; got != want {
		t.Errorf("waits = %q, want %q", got, want)
	}
}

// A probe stopped while it waits to run again runs no other attempt, and says why it ended.
func TestRunStopsWithItsContext(t *testing.T) {
	p := &plan{outcomes: []Outcome{transient("hung")}}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)

	err := Run(ctx, "worker image", Retry{Initial: time.Minute, Max: time.Minute}, logger(&bytes.Buffer{}), p.attempt)

	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "the worker image probe was abandoned") {
		t.Fatalf("Run = %v, want the abandoned-probe error wrapping the cancellation", err)
	}
	if p.attempts != 1 {
		t.Errorf("attempts = %d, want the one before the stop", p.attempts)
	}
}

// An attempt the stop interrupted is not judged: whatever it returned, Run reports the stop.
func TestRunReportsTheStopOverAnInterruptedAttempt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	refused := func(context.Context) Outcome {
		cancel()
		return Outcome{Refusal: errors.New("OMP launch probe failed (exit -1)")}
	}

	err := Run(ctx, "pi-legion load", quick(6), logger(&bytes.Buffer{}), refused)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want the stop, not the interrupted attempt's refusal", err)
	}
}

// An attempt that waits on capacity says nothing about the probed thing, so no number of them is a
// verdict: more waits than the bound allows attempts, then a pass, is a pass. Each is logged with
// what it waited on and retried after the policy's longest wait, never a transient failure.
func TestRunWaitsOutCapacityBeyondItsAttemptBudget(t *testing.T) {
	unschedulable := waiting("probe pod legion-probe-legion-1d10089a0000 is Unschedulable: 0/83 nodes are available: 25 Insufficient cpu")
	p := &plan{outcomes: []Outcome{unschedulable, unschedulable, unschedulable, unschedulable, passed}}
	var logged bytes.Buffer

	if err := Run(context.Background(), "worker image", quick(3), logger(&logged), p.attempt); err != nil {
		t.Fatalf("Run = %v, want the pass after four waits under a bound of three attempts", err)
	}
	if p.attempts != 5 {
		t.Errorf("attempts = %d, want four waits and the pass", p.attempts)
	}
	if got := strings.Count(logged.String(), "boot probe is waiting on capacity; running it again"); got != 4 {
		t.Errorf("logged %d capacity waits, want 4:\n%s", got, logged.String())
	}
	if strings.Contains(logged.String(), "failed transiently") {
		t.Errorf("a capacity wait was logged as a transient failure:\n%s", logged.String())
	}
	var waits []string
	for _, match := range regexp.MustCompile(`retryIn=(\S+)`).FindAllStringSubmatch(logged.String(), -1) {
		waits = append(waits, match[1])
	}
	if got, want := strings.Join(waits, " "), "4ms 4ms 4ms 4ms"; got != want {
		t.Errorf("waits = %q, want %q: the policy's longest wait every time", got, want)
	}
	if !strings.Contains(logged.String(), `detail="probe pod legion-probe-legion-1d10089a0000 is Unschedulable: 0/83 nodes are available: 25 Insufficient cpu"`) {
		t.Errorf("the wait was not logged with what it waited on:\n%s", logged.String())
	}
}

// A capacity wait neither counts as a transient failure nor resets the count: two transient
// failures with a wait between them are the two the bound counts, so a bound of two gives up at the
// second, naming it.
func TestRunCountsTransientFailuresAcrossACapacityWait(t *testing.T) {
	p := &plan{outcomes: []Outcome{transient("first"), waiting("the pool is full"), transient("second"), passed}}
	var logged bytes.Buffer

	err := Run(context.Background(), "worker image", quick(2), logger(&logged), p.attempt)

	want := "the worker image probe never completed within its retry budget (2 attempts): second"
	if err == nil || err.Error() != want {
		t.Fatalf("Run = %v, want %q", err, want)
	}
	if p.attempts != 3 {
		t.Errorf("attempts = %d, want a transient failure, a wait, and the transient failure that spends the bound", p.attempts)
	}
	if !strings.Contains(logged.String(), "attempt=1/2") || strings.Contains(logged.String(), "attempt=2/2") {
		t.Errorf("the transient failures were not counted 1/2 then the bound, with the wait counted as neither:\n%s", logged.String())
	}
}

// A refusal after a capacity wait is returned as it is: waiting changes what the pool holds, not
// what the probed thing answers.
func TestRunReturnsARefusalAfterACapacityWait(t *testing.T) {
	refusal := errors.New("worker image sha256:1d10 failed its probe: the OK line confirms contract 2, not 3")
	p := &plan{outcomes: []Outcome{waiting("the pool is full"), {Refusal: refusal}, passed}}

	err := Run(context.Background(), "worker image", quick(6), logger(&bytes.Buffer{}), p.attempt)

	if !errors.Is(err, refusal) || err.Error() != refusal.Error() {
		t.Fatalf("Run = %v, want the refusal itself", err)
	}
	if p.attempts != 2 {
		t.Errorf("attempts = %d, want the wait and the refusal", p.attempts)
	}
}

// rateLimited is an attempt GitHub's rate limit refused, to run again at until, the start having
// waited waited for the limit so far: the wait outcome of the start's App calls (internal/daemon's
// appAttempt).
func rateLimited(until time.Time, waited time.Duration) Outcome {
	return Outcome{RateLimit: `403 (Retry-After "", X-RateLimit-Remaining "0", X-RateLimit-Reset "1790000000", X-RateLimit-Resource "core")`,
		Until: until, Waited: waited, Detail: "GitHub App bot identity lookup failed (403)"}
}

// rateLimitWaitLine is the line Run logs for each wait for GitHub's rate limit.
const rateLimitWaitLine = "boot probe waits out GitHub's rate limit; it runs again at the time GitHub names"

// A wait for GitHub's rate limit says nothing about the probed thing, so it counts against no
// attempt budget: under a budget of two attempts, a transient failure, three rate-limit waits and
// then a pass is a pass. Each wait lasts until the time its outcome names and is one line naming
// the probe, that time, the wait, the total waited, the limit and the detail, never a transient
// failure. The wait and the total are logged to a tenth of a second.
func TestRunWaitsOutARateLimitOutsideItsAttemptBudget(t *testing.T) {
	const wait = 100 * time.Millisecond
	attempts := 0
	var untils []time.Time
	attempt := func(context.Context) Outcome {
		attempts++
		switch attempts {
		case 1:
			return transient("GitHub answered 502")
		case 2, 3, 4:
			until := time.Now().Add(wait)
			untils = append(untils, until)
			return rateLimited(until, time.Duration(attempts-1)*wait)
		}
		if since := time.Since(untils[len(untils)-1]); since < 0 {
			t.Errorf("attempt %d ran %s before the time its wait named", attempts, -since)
		}
		return passed
	}
	var logged bytes.Buffer

	if err := Run(context.Background(), "GitHub App tokens mint", quick(2), logger(&logged), attempt); err != nil {
		t.Fatalf("Run = %v, want the pass after a transient failure and three rate-limit waits under a bound of two attempts", err)
	}
	if attempts != 5 {
		t.Errorf("attempts = %d, want a transient failure, three rate-limit waits and the pass", attempts)
	}
	if got := strings.Count(logged.String(), "failed transiently"); got != 1 {
		t.Errorf("logged %d transient failures, want the one:\n%s", got, logged.String())
	}
	lines := regexp.MustCompile(`(?m)^.*`+regexp.QuoteMeta(rateLimitWaitLine)+`.*$`).FindAllString(logged.String(), -1)
	if len(lines) != 3 {
		t.Fatalf("logged %d rate-limit waits, want 3:\n%s", len(lines), logged.String())
	}
	for index, line := range lines {
		for _, want := range []string{
			`probe="GitHub App tokens mint"`,
			"until=" + untils[index].UTC().Format(time.RFC3339) + " ",
			" wait=",
			" waited=" + (time.Duration(index+1) * wait).String() + " ",
			`limit="403 (Retry-After \"\", X-RateLimit-Remaining \"0\", X-RateLimit-Reset \"1790000000\", X-RateLimit-Resource \"core\")"`,
			`detail="GitHub App bot identity lookup failed (403)"`,
		} {
			if !strings.Contains(line, want) {
				t.Errorf("rate-limit wait %d = %s, want it to hold %s", index+1, line, want)
			}
		}
	}
}

// A stop that arrives while the probe waits out GitHub's rate limit ends the wait at once, with
// the abandoned-probe error wrapping the cancellation, and runs no other attempt.
func TestRunEndsARateLimitWaitWhenItsContextEnds(t *testing.T) {
	p := &plan{outcomes: []Outcome{rateLimited(time.Now().Add(time.Minute), time.Minute)}}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	started := time.Now()

	err := Run(ctx, "GitHub App tokens mint", quick(6), logger(&bytes.Buffer{}), p.attempt)

	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "the GitHub App tokens mint probe was abandoned") {
		t.Fatalf("Run = %v, want the abandoned-probe error wrapping the cancellation", err)
	}
	if took := time.Since(started); took > time.Second {
		t.Errorf("Run returned %s after it started, want soon after the stop, not at the minute's end", took)
	}
	if p.attempts != 1 {
		t.Errorf("attempts = %d, want the one before the stop", p.attempts)
	}
}

// The OK line is the wire between the image's `legion probe-image` and the daemon's probe
// Sandbox: the session-storage mark, the extensions mark, the agent-models mark, the capabilities
// mark, the model-fallback mark, the omp-version mark, the omp-session-id mark, then the contract
// last.
func TestOKLineCarriesTheMarksAndTheContract(t *testing.T) {
	if got, want := OKLine("/opt/omp/bin/omp", 3, AgentModelsResolved, CapabilitiesAll, ModelFallbackOff, "omp/18.18.6", SessionIDFlagTaken), "probe-image: OK (/opt/omp/bin/omp) session-storage=probed extensions=discovered agent-models=resolved capabilities=checked model-fallback=off omp-version=omp/18.18.6 omp-session-id=taken daemon-api-version=3"; got != want {
		t.Errorf("OKLine = %q, want %q", got, want)
	}
	if got, want := OKLine("/opt/omp/bin/omp", 3, AgentModelsResolved, CapabilitiesAll, ModelFallbackUnread, OmpVersionUnread, SessionIDFlagUnread), "probe-image: OK (/opt/omp/bin/omp) session-storage=probed extensions=discovered agent-models=resolved capabilities=checked model-fallback=unread omp-version=unread omp-session-id=unread daemon-api-version=3"; got != want {
		t.Errorf("OKLine = %q, want %q", got, want)
	}
	if got, want := OKLine("/opt/omp/bin/omp", 3, AgentModelsSkipped, CapabilitiesBrowserSkipped, ModelFallbackOn, "omp/18.18.6", SessionIDFlagAbsent), "probe-image: OK (/opt/omp/bin/omp) session-storage=probed extensions=discovered agent-models=skipped capabilities=browser-skipped model-fallback=on omp-version=omp/18.18.6 omp-session-id=absent daemon-api-version=3"; got != want {
		t.Errorf("OKLine = %q, want %q", got, want)
	}
}

// The omp-version mark is Oh My Pi's own words, which the line keeps as one token: a version
// printed with spaces in it has each whitespace run written as one underscore, and an empty one is
// written unread, so neither breaks the marks after it nor reads as a mark with no value.
func TestOKLineKeepsTheOmpVersionOneToken(t *testing.T) {
	for _, testCase := range []struct{ name, version, want string }{
		{"spaces and a tab", " omp 18.18.6 \t(sami build) ", "omp_18.18.6_(sami_build)"},
		{"empty", "", OmpVersionUnread},
		{"only whitespace", " \t ", OmpVersionUnread},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			line := OKLine("/opt/omp/bin/omp", 3, AgentModelsResolved, CapabilitiesAll, ModelFallbackOff, testCase.version, SessionIDFlagTaken)
			if got := OmpVersion(line); got != testCase.want {
				t.Errorf("OmpVersion(%q) = %q, want %q", line, got, testCase.want)
			}
			if got := SessionIDFlag(line); got != SessionIDFlagTaken {
				t.Errorf("SessionIDFlag(%q) = %q, want the mark after the version read whole: %q", line, got, SessionIDFlagTaken)
			}
		})
	}
}

// oldOKLine is the line a CLI that predates the discovery-on pod lane and the capability check
// prints: no extensions mark, no capabilities mark, no model-fallback mark. The daemon still parses
// what it does carry.
const oldOKLine = "probe-image: OK (/opt/omp/bin/omp) session-storage=probed agent-models=resolved daemon-api-version=5"

// The daemon reads whether the image's probe ran the pod's lane with extension discovery on from
// its OK line; a CLI that predates that lane prints no mark, and a token the mark merely begins is
// not the mark.
func TestExtensionsDiscoveredReadsTheMarkOnAnOKLine(t *testing.T) {
	for _, testCase := range []struct {
		name, output string
		want         bool
	}{
		{"the mark, among other output", "[legion] probe retried\n" + OKLine("/opt/omp/bin/omp", 5, AgentModelsResolved, CapabilitiesAll, ModelFallbackOff, "omp/18.18.6", SessionIDFlagTaken) + "\n", true},
		{"the mark on a build-time probe's line", OKLine("/opt/omp/bin/omp", 5, AgentModelsSkipped, CapabilitiesAll, ModelFallbackOn, "omp/18.18.6", SessionIDFlagAbsent), true},
		{"a CLI that predates the discovery-on pod lane", oldOKLine, false},
		{"a CLI that predates the agent-model check", "probe-image: OK (/opt/omp/bin/omp) session-storage=probed daemon-api-version=5", false},
		{"the mark on a line that is not the OK line", "[legion] extensions=discovered agent-models=resolved daemon-api-version=5", false},
		{"a token the mark merely begins", "probe-image: OK (/opt/omp/bin/omp) session-storage=probed extensions=discovered-not agent-models=resolved daemon-api-version=5", false},
		{"nothing", "", false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := ExtensionsDiscovered(testCase.output); got != testCase.want {
				t.Fatalf("ExtensionsDiscovered = %t, want %t", got, testCase.want)
			}
		})
	}
}

// The daemon reads whether the image's probe resolved the agents' models from its OK line, on the
// line's current shape and on the shape before the capability marks; a CLI that predates the
// agent-model check prints no mark, which reads as none.
func TestAgentModelsReadsTheMarkOnAnOKLine(t *testing.T) {
	for _, testCase := range []struct {
		name, output, want string
	}{
		{"resolved, among other output", "[legion] probe retried\n" + OKLine("/opt/omp/bin/omp", 5, AgentModelsResolved, CapabilitiesAll, ModelFallbackOff, "omp/18.18.6", SessionIDFlagTaken) + "\n", AgentModelsResolved},
		{"skipped", OKLine("/opt/omp/bin/omp", 5, AgentModelsSkipped, CapabilitiesAll, ModelFallbackOn, "omp/18.18.6", SessionIDFlagAbsent), AgentModelsSkipped},
		{"a CLI that predates the capability check", oldOKLine, AgentModelsResolved},
		{"a CLI that predates the check", "probe-image: OK (/opt/omp/bin/omp) session-storage=probed daemon-api-version=5", ""},
		{"the mark on a line that is not the OK line", "[legion] agent-models=resolved daemon-api-version=5", ""},
		{"nothing", "", ""},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := AgentModels(testCase.output); got != testCase.want {
				t.Fatalf("AgentModels = %q, want %q", got, testCase.want)
			}
		})
	}
}

// The daemon reads the capabilities state of the image's probe from its OK line, and the
// model-fallback mark beside it: checked, or browser-skipped from a probe run with --skip-browser,
// which the daemon's probe Sandbox refuses; a CLI that predates the check prints neither mark,
// which reads as none.
func TestCapabilityMarksAreReadFromTheOKLine(t *testing.T) {
	for _, testCase := range []struct {
		name, output string
		capabilities string
		fallback     string
	}{
		{"fallback off, among other output", "probe-image: capability browser: present (chromium)\n" + OKLine("/opt/omp/bin/omp", 5, AgentModelsResolved, CapabilitiesAll, ModelFallbackOff, "omp/18.18.6", SessionIDFlagTaken) + "\n", CapabilitiesAll, ModelFallbackOff},
		{"fallback on", OKLine("/opt/omp/bin/omp", 5, AgentModelsResolved, CapabilitiesAll, ModelFallbackOn, "omp/18.18.6", SessionIDFlagTaken), CapabilitiesAll, ModelFallbackOn},
		{"fallback unread", OKLine("/opt/omp/bin/omp", 5, AgentModelsResolved, CapabilitiesAll, ModelFallbackUnread, "omp/18.18.6", SessionIDFlagTaken), CapabilitiesAll, ModelFallbackUnread},
		{"the browser row skipped", OKLine("/opt/omp/bin/omp", 5, AgentModelsSkipped, CapabilitiesBrowserSkipped, ModelFallbackOn, "omp/18.18.6", SessionIDFlagTaken), CapabilitiesBrowserSkipped, ModelFallbackOn},
		{"a CLI that predates the capability check", oldOKLine, "", ""},
		{"the marks on a line that is not the OK line", "[legion] capabilities=checked model-fallback=on daemon-api-version=5", "", ""},
		{"the marks after the contract", "probe-image: OK (/opt/omp/bin/omp) daemon-api-version=5 capabilities=checked model-fallback=on", "", ""},
		{"nothing", "", "", ""},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := Capabilities(testCase.output); got != testCase.capabilities {
				t.Errorf("Capabilities = %q, want %q", got, testCase.capabilities)
			}
			if got := ModelFallback(testCase.output); got != testCase.fallback {
				t.Errorf("ModelFallback = %q, want %q", got, testCase.fallback)
			}
		})
	}
}

// lineBeforeTheOmpMarks is the OK line a CLI that predates the omp-version and omp-session-id
// marks prints: every other mark of the current line, and neither of those two.
const lineBeforeTheOmpMarks = "probe-image: OK (/opt/omp/bin/omp) session-storage=probed extensions=discovered agent-models=resolved capabilities=checked model-fallback=off daemon-api-version=5"

// The daemon reads which Oh My Pi the image carries and whether it takes --session-id from the OK
// line's omp-version and omp-session-id marks: each state as the line spells it, and none from a
// CLI that predates the marks — the line of the release before them and the older line alike —
// from a line that is not the OK line, or from marks after the contract.
func TestOmpMarksAreReadFromTheOKLine(t *testing.T) {
	for _, testCase := range []struct {
		name, output       string
		version, sessionID string
	}{
		{"taken, among other output", "probe-image: capability browser: present (chromium)\n" + OKLine("/opt/omp/bin/omp", 5, AgentModelsResolved, CapabilitiesAll, ModelFallbackOff, "omp/18.18.6", SessionIDFlagTaken) + "\n", "omp/18.18.6", SessionIDFlagTaken},
		{"absent", OKLine("/opt/omp/bin/omp", 5, AgentModelsResolved, CapabilitiesAll, ModelFallbackOff, "omp/18.18.6", SessionIDFlagAbsent), "omp/18.18.6", SessionIDFlagAbsent},
		{"both unread", OKLine("/opt/omp/bin/omp", 5, AgentModelsResolved, CapabilitiesAll, ModelFallbackOff, OmpVersionUnread, SessionIDFlagUnread), OmpVersionUnread, SessionIDFlagUnread},
		{"a CLI that predates the marks", lineBeforeTheOmpMarks, "", ""},
		{"a CLI that predates the capability check", oldOKLine, "", ""},
		{"the marks on a line that is not the OK line", "[legion] omp-version=omp/18.18.6 omp-session-id=taken daemon-api-version=5", "", ""},
		{"the marks after the contract", "probe-image: OK (/opt/omp/bin/omp) daemon-api-version=5 omp-version=omp/18.18.6 omp-session-id=taken", "", ""},
		{"nothing", "", "", ""},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := OmpVersion(testCase.output); got != testCase.version {
				t.Errorf("OmpVersion = %q, want %q", got, testCase.version)
			}
			if got := SessionIDFlag(testCase.output); got != testCase.sessionID {
				t.Errorf("SessionIDFlag = %q, want %q", got, testCase.sessionID)
			}
		})
	}
}

// The daemon reads the contract an image confirmed from the probe pod's log. Only an OK line that
// ends with the contract token confirms one; a CLI that predates the contract check prints none.
func TestConfirmedContractReadsOnlyTheContractTokenOnAnOKLine(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		output   string
		contract int
		ok       bool
	}{
		{"the line among other output", "[legion] OMP pi.agents probe failed transiently\n" + OKLine("/opt/omp/bin/omp", 3, AgentModelsResolved, CapabilitiesAll, ModelFallbackOff, "omp/18.18.6", SessionIDFlagTaken) + "\n", 3, true},
		{"another number", OKLine("/opt/omp/bin/omp", 12, AgentModelsSkipped, CapabilitiesAll, ModelFallbackOn, "omp/18.18.6", SessionIDFlagAbsent), 12, true},
		{"a CLI that predates the capability check", oldOKLine, 5, true},
		{"a CLI that predates the contract check", "probe-image: OK (/opt/omp/bin/omp) session-storage=probed", 0, false},
		{"a CLI that predates the session-storage probe", "probe-image: OK (/opt/omp/bin/omp)", 0, false},
		{"the token before the line's end", "probe-image: OK (/opt/omp/bin/omp) daemon-api-version=3 session-storage=probed", 0, false},
		{"the token on a line that is not the OK line", "[legion] daemon-api-version=3", 0, false},
		{"no digits", "probe-image: OK (/opt/omp/bin/omp) daemon-api-version=", 0, false},
		{"a number no int holds", "probe-image: OK (/opt/omp/bin/omp) daemon-api-version=99999999999999999999", 0, false},
		{"nothing", "", 0, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			contract, ok := ConfirmedContract(testCase.output)
			if contract != testCase.contract || ok != testCase.ok {
				t.Fatalf("ConfirmedContract = (%d, %t), want (%d, %t)", contract, ok, testCase.contract, testCase.ok)
			}
		})
	}
}

// The daemon reads a probe's no-verdict line from the probe pod's log: the first line that starts
// with NoVerdictPrefix, and only that line's text after the prefix, never the lines after it. The
// prefix anywhere but at a line's start is no such line, so another line that quotes it is not
// read as one.
func TestNoVerdictReadsOnlyALineOfItsOwn(t *testing.T) {
	const detail = "capability browser: not checked; role overlays: implementer: signal: terminated"
	for _, testCase := range []struct {
		name, output, detail string
		ok                   bool
	}{
		{"the line among other output", "probe-image: capability browser: unanswered (not checked: context deadline exceeded)\n" + NoVerdictPrefix + detail + "\nlegion probe-image: exiting\n", detail, true},
		{"the line last, with no line break", NoVerdictPrefix + detail, detail, true},
		{"the first of two lines", NoVerdictPrefix + "capability lsp: first\n" + NoVerdictPrefix + "capability browser: second\n", "capability lsp: first", true},
		{"the prefix inside another line", "probe-image: quoted " + NoVerdictPrefix + detail + "\n", "", false},
		{"the failure's line", "legion probe-image: capability browser is missing: chromium is not on PATH, and PUPPETEER_EXECUTABLE_PATH names no executable\n", "", false},
		{"nothing", "", "", false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got, ok := NoVerdict(testCase.output)
			if got != testCase.detail || ok != testCase.ok {
				t.Fatalf("NoVerdict = (%q, %t), want (%q, %t)", got, ok, testCase.detail, testCase.ok)
			}
		})
	}
}
