package daemon

// The boot's waits for GitHub's rate limit on its App calls (c-boot-mint-rate-limit): the tests of
// the start's rate-limit waits, the clock they run on (limitClock), the stand-in answers that refuse
// the start for the limit (spentUntil), and the readers of the lines the start writes for each wait.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// limitClock is a test's clock for every wait of the start's App calls (appCallBounds.clock) and
// for the GitHub stand-in that refuses the start for the rate limit (spentUntil): the time of day
// moved on by every wait the start has slept, each sleep returning at once, so a wait for a reset
// or for the minute a refusal naming no time asks costs the test nothing, while the start, the
// stand-in and the test's checks all read one time. The start sleeps on it for every wait of its
// App calls (bootprobe.RunOn): the rate-limit waits, and the 10 ms waits after an attempt that
// failed transiently, was pending or waited (quickAppMints), which move it 10 ms. A sleep longer
// than holdOver (holdOver above 0) holds until its ctx ends instead, so a test stops the start
// inside a wait of that length, whatever the waits before it were.
type limitClock struct {
	mu       sync.Mutex
	ahead    time.Duration
	holdOver time.Duration
}

// Now is the time of day moved on by every wait slept so far.
func (c *limitClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.ahead)
}

// Sleep moves the clock on by d and returns, or, for a d longer than holdOver, holds until ctx
// ends.
func (c *limitClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	hold := c.holdOver > 0 && d > c.holdOver
	if !hold && d > 0 {
		c.ahead += d
	}
	c.mu.Unlock()
	if hold {
		<-ctx.Done()
	}
	return ctx.Err()
}

// limitCalls are quickAppMints making the start's waits on a limitClock of the test's own, which
// it returns with them.
func limitCalls() (*appCallBounds, *limitClock) {
	calls, clock := quickAppMints(), &limitClock{}
	calls.clock = clock
	return calls, clock
}

// clockedLogger is a JSON logger to logs whose lines carry clock's time when written, so a test
// compares a line's time with the waits the start decided on that clock (loggedAt).
func clockedLogger(logs io.Writer, clock bootprobe.Clock) *slog.Logger {
	return slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{ReplaceAttr: func(groups []string, attr slog.Attr) slog.Attr {
		if len(groups) == 0 && attr.Key == slog.TimeKey {
			return slog.Time(slog.TimeKey, clock.Now())
		}
		return attr
	}}))
}

// rateLimitWaitLine is the line a start logs for each wait for GitHub's rate limit on an App call
// (bootprobe.Run's wait outcome).
const rateLimitWaitLine = "boot probe waits out GitHub's rate limit"

// spentUntil is an answer for appTokensWith or commandTokens: each request refused names is
// answered with GitHub's 403 for a spent rate limit (spentLimit) until the limit's reset, ahead of
// the first such request on the clock now, which it stores in reset; every request after the
// reset, and every other request, is left to the stand-in. A test whose start waits on a
// limitClock reads the reset on that clock too.
func spentUntil(now func() time.Time, ahead time.Duration, reset *atomic.Int64, refused func(r *http.Request) bool) func(w http.ResponseWriter, r *http.Request) bool {
	return func(w http.ResponseWriter, r *http.Request) bool {
		if !refused(r) {
			return false
		}
		reset.CompareAndSwap(0, now().Add(ahead).Unix())
		if !now().Before(time.Unix(reset.Load(), 0)) {
			return false
		}
		spentLimit(w, reset.Load())
		return true
	}
}

// keyProofRead is whether r is the boot's proof of an App's key: its read of the App's
// installations under the App's JWT (proveAppKeys).
func keyProofRead(r *http.Request) bool { return r.URL.Path == "/app/installations" }

// oneRateLimitWait is the one rate-limit wait line of the daemon's JSON log logs, and fails the
// test unless there is exactly one, for probe.
func oneRateLimitWait(t *testing.T, logs, probe string) map[string]any {
	t.Helper()
	waits := loggedLines(t, logs, rateLimitWaitLine)
	if len(waits) != 1 || waits[0]["probe"] != probe {
		t.Fatalf("rate-limit wait lines = %v, want one, for the %s", waits, probe)
	}
	return waits[0]
}

// loggedDuration is the duration a JSON log line holds under key.
func loggedDuration(t *testing.T, line map[string]any, key string) time.Duration {
	t.Helper()
	text, _ := line[key].(string)
	duration, err := time.ParseDuration(text)
	if err != nil {
		t.Fatalf("the line's %s = %v, want a duration: %v", key, line[key], err)
	}
	return duration
}

// A start whose proof of an App's key GitHub refuses for a spent rate limit waits until the reset
// GitHub names and serves after it (c-boot-mint-rate-limit case 1, on the key proof), saying so in
// one line that names the call, the time it waits until and the total waited. The refusals outlast
// the three quick attempts, which the wait does not spend: before, the start exited once they were
// spent, before the reset. The start waits on a clock of the test's own (limitClock), on which the
// stand-in names its reset, so the wait costs the test nothing.
func TestABootWaitsOutARateLimitOnTheAppKeyProofAndServesAfterTheReset(t *testing.T) {
	t.Parallel()
	calls, clock := limitCalls()
	var reset atomic.Int64
	tokens, _ := appTokensWith(t, discoveredOnAcme, spentUntil(clock.Now, 3*time.Second, &reset, keyProofRead))
	logs := &syncBuffer{}
	if err := bootsOn(t, workflowConfig(t, workflowNATS(t)), tokens, calls, 60*time.Second, slog.New(slog.NewJSONHandler(logs, nil))); err != nil {
		t.Fatalf("boot = %v, want it to wait out GitHub's rate limit on the key proof and serve after the reset", err)
	}
	resetAt := time.Unix(reset.Load(), 0)
	if served := clock.Now(); served.Before(resetAt) {
		t.Fatalf("the daemon served at %s, before GitHub's reset at %s", served, resetAt)
	}
	wait := oneRateLimitWait(t, logs.String(), "GitHub App key proof")
	if want := resetAt.UTC().Format(time.RFC3339); wait["until"] != want {
		t.Errorf("the wait line's until = %v, want GitHub's reset, %s", wait["until"], want)
	}
	if waited := loggedDuration(t, wait, "waited"); waited <= 0 || waited > 3*time.Second+100*time.Millisecond {
		t.Errorf("the wait line's waited = %s, want the time to the reset, at most 3s", waited)
	}
}

// GitHub's 403 naming its rate limit in its message alone, with calls left in its headers, is a
// rate limit to the start's key proof too (c-boot-mint-rate-limit case 2, on the key proof). It
// names no time, so the start waits a minute, says so, and serves after it. Before, the start read
// it as GitHub's answer about the App and exited at the first attempt. The minute is on a clock of
// the test's own (limitClock).
func TestABootWaitsOutARateLimitNamedOnlyByItsMessageOnTheAppKeyProof(t *testing.T) {
	t.Parallel()
	calls, clock := limitCalls()
	var refusals atomic.Int64
	tokens, _ := appTokensWith(t, discoveredOnAcme, func(w http.ResponseWriter, r *http.Request) bool {
		if !keyProofRead(r) || refusals.Add(1) > 1 {
			return false
		}
		w.Header().Set("X-RateLimit-Limit", "15000")
		w.Header().Set("X-RateLimit-Remaining", "14671")
		w.Header().Set("X-RateLimit-Resource", "core")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"API rate limit exceeded for installation ID 7.","status":"403"}`)
		return true
	})
	logs := &syncBuffer{}
	started := clock.Now()
	if err := bootsOn(t, workflowConfig(t, workflowNATS(t)), tokens, calls, 90*time.Second, slog.New(slog.NewJSONHandler(logs, nil))); err != nil {
		t.Fatalf("boot = %v, want it to wait out the rate limit GitHub's message names and serve", err)
	}
	if took := clock.Now().Sub(started); took < time.Minute {
		t.Errorf("the daemon served %s after it started, want after the minute's wait", took)
	}
	// The line gives the wait left when it is written, to a tenth of a second, so a slow runner
	// can show a little under the minute decided; never more.
	if wait := loggedDuration(t, oneRateLimitWait(t, logs.String(), "GitHub App key proof"), "wait"); wait <= time.Minute-time.Second || wait > time.Minute {
		t.Errorf("the wait line's wait = %s, want 1m0s, the wait for a refusal that names no time", wait)
	}
}

// A start whose read of the Apps' logins GitHub refuses for a spent rate limit waits until the reset
// and serves after it (c-boot-mint-rate-limit case 3). Before, the start exited once the three quick
// attempts were spent, before the reset. The reset is on a clock of the test's own (limitClock).
func TestABootWaitsOutARateLimitOnTheAppLoginsRead(t *testing.T) {
	t.Parallel()
	calls, clock := limitCalls()
	var reset atomic.Int64
	tokens, _ := appTokensWith(t, discoveredOnAcme, spentUntil(clock.Now, 3*time.Second, &reset, func(r *http.Request) bool { return r.URL.Path == "/app" }))
	logs := &syncBuffer{}
	if err := bootsOn(t, workflowConfig(t, workflowNATS(t)), tokens, calls, 60*time.Second, slog.New(slog.NewJSONHandler(logs, nil))); err != nil {
		t.Fatalf("boot = %v, want it to wait out GitHub's rate limit on the logins read and serve after the reset", err)
	}
	resetAt := time.Unix(reset.Load(), 0)
	if served := clock.Now(); served.Before(resetAt) {
		t.Fatalf("the daemon served at %s, before GitHub's reset at %s", served, resetAt)
	}
	if wait := oneRateLimitWait(t, logs.String(), "GitHub App logins read"); wait["until"] != resetAt.UTC().Format(time.RFC3339) {
		t.Errorf("the wait line's until = %v, want GitHub's reset, %s", wait["until"], resetAt.UTC().Format(time.RFC3339))
	}
}

// A start whose tokens come from a token_command waits out GitHub's rate limit on the App read that
// checks a released token (appauth's tokenIdentity: the GraphQL viewer), which the logins read makes
// under the token the command releases for every installation, and serves after the reset
// (c-boot-mint-rate-limit case 4, on the logins read). The limit refuses the implement App's token
// so released. Before, the start exited once the three quick attempts were spent, before the reset.
// The reset is on a clock of the test's own (limitClock).
func TestABootWaitsOutARateLimitOnATokenCommandsAppRead(t *testing.T) {
	t.Parallel()
	calls, clock := limitCalls()
	var reset atomic.Int64
	tokens := commandTokens(t, spentUntil(clock.Now, 3*time.Second, &reset, func(r *http.Request) bool {
		return r.URL.Path == "/graphql" && r.Header.Get("Authorization") == "token ghs_implement_any_write"
	}))
	logs := &syncBuffer{}
	if err := bootsOn(t, workflowConfig(t, workflowNATS(t)), tokens, calls, 60*time.Second, slog.New(slog.NewJSONHandler(logs, nil))); err != nil {
		t.Fatalf("boot = %v, want it to wait out GitHub's rate limit on the released token's App read and serve after the reset", err)
	}
	resetAt := time.Unix(reset.Load(), 0)
	if served := clock.Now(); served.Before(resetAt) {
		t.Fatalf("the daemon served at %s, before GitHub's reset at %s", served, resetAt)
	}
	if wait := oneRateLimitWait(t, logs.String(), "GitHub App logins read"); wait["until"] != resetAt.UTC().Format(time.RFC3339) {
		t.Errorf("the wait line's until = %v, want GitHub's reset, %s", wait["until"], resetAt.UTC().Format(time.RFC3339))
	}
}

// bootStoppedWhen runs the daemon on cfg with tokens, logging to logs as JSON, until ready reads
// what the test waits for in the log, then stops it, and says how long run took to return after the
// stop. The test fails if ready does not hold within within, if run returns before the stop, or if
// it returns an error after it. With calls on a clock of the test's own (limitCalls), each line
// carries that clock's time (clockedLogger).
func bootStoppedWhen(t *testing.T, cfg config.Config, tokens appauth.Source, calls *appCallBounds, logs *syncBuffer, within time.Duration, ready func(logs string) bool) time.Duration {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.NewJSONHandler(logs, nil))
	if calls.clock != nil {
		log = clockedLogger(logs, calls.clock)
	}
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, cfg, log, overrides{
			listen:         heldListen,
			runtime:        fakeRuntime(fake.NewRuntime(), &built{}).runtime,
			clock:          stillClock{},
			workflowTokens: tokens,
			githubAPI:      repositoriesGitHub(t),
			appCalls:       calls,
		})
	}()
	var ended error
	returned := false
	if !testwait.Holds(within, testwait.Poll, func() bool {
		select {
		case ended = <-done:
			returned = true
			return true
		default:
		}
		return ready(logs.String())
	}) {
		t.Fatalf("the daemon's log did not hold what the test waits for within %s; it holds:\n%s", within, logs.String())
	}
	if returned {
		t.Fatalf("run returned %v before the test stopped it; the log holds:\n%s", ended, logs.String())
	}
	cancel()
	stopped := time.Now()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run stopped by the test = %v, want a clean stop", err)
		}
		return time.Since(stopped)
	case <-time.After(30 * time.Second):
		t.Fatal("run did not return within 30s of its stop")
	}
	return 0
}

// A Retry-After GitHub sends on the start's key proof is waited as GitHub asks, at most the maximum
// (c-boot-mint-rate-limit case 5, first half, on the key proof; the maximum shortened to 2 s): a 429
// asking for a second waits a second, one asking for 30 s waits the 2 s, and the start serves after
// each.
//
// The wait line's checks follow from the start's own decisions, whatever the machine's load: the
// start's count of its waits (waited) is the wait it decided; the wait left can be any amount up to
// that wait, nothing or less once the line comes late; and the until (whole seconds) is at most the
// wait past the line and less than a second short of the wait past the refused read, which the
// start answered after. One bound still follows the clock: the second read at most 3 s past the
// wait, the bound on a wait that runs past its until. An earlier version held the wait left to
// equal the wait decided; with a 1 s delay injected before the line, it failed. The clock is the
// test's own (limitClock): the start waits on it, the stand-in stamps its reads with it, and the
// daemon's lines carry its time (clockedLogger), so no wait costs the test its seconds.
func TestABootWaitsAsLongAsGitHubAsksAtMostTheMaximum(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		retryAfter string
		want       time.Duration
	}{
		{name: "a Retry-After of a second", retryAfter: "1", want: time.Second},
		{name: "a Retry-After above the maximum", retryAfter: "30", want: 2 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls, clock := limitCalls()
			calls.rateLimits.Max = 2 * time.Second
			var mu sync.Mutex
			var reads []time.Time
			tokens, _ := appTokensWith(t, discoveredOnAcme, func(w http.ResponseWriter, r *http.Request) bool {
				if !keyProofRead(r) {
					return false
				}
				mu.Lock()
				reads = append(reads, clock.Now())
				first := len(reads) == 1
				mu.Unlock()
				if !first {
					return false
				}
				w.Header().Set("Retry-After", tc.retryAfter)
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"message":"You have exceeded a secondary rate limit."}`)
				return true
			})
			logs := &syncBuffer{}
			if err := bootsOn(t, workflowConfig(t, workflowNATS(t)), tokens, calls, 60*time.Second, clockedLogger(logs, clock)); err != nil {
				t.Fatalf("boot = %v, want it to wait out GitHub's Retry-After and serve", err)
			}
			wait := oneRateLimitWait(t, logs.String(), "GitHub App key proof")
			if waited := loggedDuration(t, wait, "waited"); waited != tc.want {
				t.Errorf("the start's waits so far = %s, want %s: the one wait it decided", waited, tc.want)
			}
			if left := loggedDuration(t, wait, "wait"); left > tc.want {
				t.Errorf("the wait left = %s, want at most the %s wait decided", left, tc.want)
			}
			until, err := time.Parse(time.RFC3339, fmt.Sprint(wait["until"]))
			if err != nil {
				t.Fatalf("the wait line's until = %v: %v", wait["until"], err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(reads) < 2 || reads[1].Sub(reads[0]) < tc.want || reads[1].Sub(reads[0]) > tc.want+3*time.Second {
				t.Fatalf("GitHub was read for the App's installations at %v, want the refused read and one more %s after it", reads, tc.want)
			}
			if line := loggedAt(t, wait); until.After(line.Add(tc.want)) || !until.After(reads[0].Add(tc.want-time.Second)) {
				t.Errorf("the wait line at %s says until %s, want at most %s past the line and less than a second short of %s past the refused read at %s",
					line, until, tc.want, tc.want, reads[0])
			}
		})
	}
}

// Refusals of one call of the start that name no time wait a minute, then two (c-boot-mint-rate-limit
// case 5, second half, at the production minute, on the key proof): GitHub answers every read of the
// App's installations with a 429 and no rate-limit header. The start's count of its waits (waited)
// is a minute at the first wait line and three at the second, so the second wait is two minutes; the
// second wait begins only once the first has ended, and ends two minutes after it begins, so its
// until is two minutes or more past the first's and at most two minutes past its own line. A stop
// during that wait ends the start at once.
//
// The waited, wait-left and until checks follow from the start's own decisions, whatever the
// machine's load: a line is written some time after its wait was decided, so its wait left can be
// any amount under the wait decided, and an until (whole seconds) any amount under two minutes past
// its line. Two bounds still follow the clock: the 90 s the test gives the two lines, and the 1 s
// the start may take to return after its stop. The 90 s is the only bound on how long the first wait
// runs past its until. An earlier version also held the second line to between 60 and 70 s after
// the first, and each wait left to within 1 s of the wait decided; with a 1 s delay injected before
// each line, both failed. The waits are on a clock of the test's own (limitClock), whose lines the
// daemon stamps with its time (bootStoppedWhen): the first wait moves it on by the minute at once,
// and the second holds on it until the stop, so the test waits out neither.
func TestABootWaitsOneMinuteThenTwoForRefusalsOfOneCallThatNameNoTime(t *testing.T) {
	t.Parallel()
	calls, clock := limitCalls()
	// The minute's wait passes at once, the transient attempts' 10 ms waits too however slow an
	// attempt is, and the wait of two minutes holds until the stop.
	clock.holdOver = time.Minute
	tokens, _ := appTokensWith(t, discoveredOnAcme, func(w http.ResponseWriter, r *http.Request) bool {
		if !keyProofRead(r) {
			return false
		}
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"message":"Too many requests."}`)
		return true
	})
	logs := &syncBuffer{}
	took := bootStoppedWhen(t, workflowConfig(t, workflowNATS(t)), tokens, calls, logs, 90*time.Second, func(logs string) bool {
		return strings.Count(logs, rateLimitWaitLine) >= 2
	})
	if took > time.Second {
		t.Errorf("run returned %s after its stop, want within 1s", took)
	}
	waits := loggedLines(t, logs.String(), rateLimitWaitLine)
	if len(waits) != 2 {
		t.Fatalf("rate-limit wait lines = %v, want two", waits)
	}
	if first, second := loggedDuration(t, waits[0], "waited"), loggedDuration(t, waits[1], "waited"); first != time.Minute || second != 3*time.Minute {
		t.Errorf("the start's waits so far = %s and then %s, want 1m0s and then 3m0s: a wait of a minute, then one of two", first, second)
	}
	if first, second := loggedDuration(t, waits[0], "wait"), loggedDuration(t, waits[1], "wait"); first <= 0 || first > time.Minute || second <= 0 || second > 2*time.Minute {
		t.Errorf("the waits left = %s and %s, want each above nothing and at most its wait, 1m0s and then 2m0s", first, second)
	}
	untils := make([]time.Time, len(waits))
	for i, wait := range waits {
		until, err := time.Parse(time.RFC3339, fmt.Sprint(wait["until"]))
		if err != nil {
			t.Fatalf("wait line %d's until = %v: %v", i+1, wait["until"], err)
		}
		untils[i] = until
	}
	if first := loggedAt(t, waits[0]); untils[0].After(first.Add(time.Minute)) {
		t.Errorf("the first wait's until = %s, want at most a minute after its line, %s", untils[0], first)
	}
	if second := loggedAt(t, waits[1]); second.Before(untils[0]) || untils[1].Before(untils[0].Add(2*time.Minute)) || untils[1].After(second.Add(2*time.Minute)) {
		t.Errorf("the second wait's line at %s and until %s, want both past the first wait's until %s, and the until two minutes or more past it and at most two minutes past the line",
			second, untils[1], untils[0])
	}
}

// All rate-limit waits of one start end at the bound after the first of them (c-boot-mint-rate-limit
// case 6, on the key proof, the bound shortened to 3 s): GitHub refuses every read of the App's
// installations with a reset a minute ahead, the one wait ends at the bound, and the refusal after
// it ends the start, naming the call, GitHub's last answer with its headers and message, the 3 s
// waited and the App whose key the read proves. The reset is past the 15 s ceiling below, so a
// start that waited for it in place of the bound fails there. The bound and the reset are on a
// clock of the test's own (limitClock).
func TestABootEndsWhenItsRateLimitWaitsReachTheirBound(t *testing.T) {
	t.Parallel()
	calls, clock := limitCalls()
	calls.rateLimits.Bound = 3 * time.Second
	tokens, _ := appTokensWith(t, discoveredOnAcme, func(w http.ResponseWriter, r *http.Request) bool {
		if !keyProofRead(r) {
			return false
		}
		spentLimit(w, clock.Now().Add(time.Minute).Unix())
		return true
	})
	logs := &syncBuffer{}
	// The clock starts once the test holds its NATS server and its database: workflowNATS waits for
	// a server of the package's pool, which every test holding one at the same moment delays, and
	// that wait is no part of the start the bound ends.
	cfg := workflowConfig(t, workflowNATS(t))
	started := clock.Now()
	err := bootsOn(t, cfg, tokens, calls, 60*time.Second, slog.New(slog.NewJSONHandler(logs, nil)))
	took := clock.Now().Sub(started)
	for _, want := range []string{
		"the GitHub App key proof waited 3s for GitHub's rate limit, the most a start waits, and GitHub still refuses it: ",
		"GitHub answered GET /app/installations with 403",
		`X-RateLimit-Remaining "0"`,
		"API rate limit exceeded for installation ID 7.",
		"prove the implement App's key by reading its installations: ",
	} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("boot = %v, want the start ended naming %q", err, want)
		}
	}
	if took < 3*time.Second || took > 15*time.Second {
		t.Errorf("the start ended %s after it began, want soon after the 3s bound", took)
	}
	if waits := loggedLines(t, logs.String(), rateLimitWaitLine); len(waits) != 1 {
		t.Errorf("rate-limit wait lines = %v, want the one that ended at the bound", waits)
	}
}

// A stop that arrives while the start waits out GitHub's rate limit ends the start at once and
// cleanly (c-boot-mint-rate-limit case 9, on the key proof): the App's installations are refused
// until a reset a minute ahead, and run returns nil within a second of the stop. The start waits on
// the production clock here, the wall clock, since the test needs a wait that holds until the
// stop, and this test is what shows that clock's wait holds: the stop comes half a second after the
// first wait line, and that line must still be the only one. A Sleep that did not block would have
// the start ask again at once, be refused again, and write a wait line each time.
func TestABootStoppedWhileItWaitsOutARateLimitEndsAtOnce(t *testing.T) {
	t.Parallel()
	calls := quickAppMints()
	var reset atomic.Int64
	tokens, _ := appTokensWith(t, discoveredOnAcme, spentUntil(time.Now, time.Minute, &reset, keyProofRead))
	logs := &syncBuffer{}
	var waitSeen time.Time
	took := bootStoppedWhen(t, workflowConfig(t, workflowNATS(t)), tokens, calls, logs, 30*time.Second, func(logs string) bool {
		if !strings.Contains(logs, rateLimitWaitLine) {
			return false
		}
		if waitSeen.IsZero() {
			waitSeen = time.Now()
		}
		return time.Since(waitSeen) >= 500*time.Millisecond
	})
	if took > time.Second {
		t.Errorf("run returned %s after its stop, want within 1s", took)
	}
	if waits := loggedLines(t, logs.String(), rateLimitWaitLine); len(waits) != 1 {
		t.Errorf("rate-limit wait lines before the stop = %d, want the one: the wall clock's wait held for half a second", len(waits))
	}
	if stopped := loggedMessages(t, logs.String(), "legion daemon stopped before it proved its GitHub Apps' keys"); len(stopped) != 1 {
		t.Errorf("the stop's lines = %v, want one", stopped)
	}
}
