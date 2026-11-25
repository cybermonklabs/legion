package backpressure

// The rows of INFRA-5802's checks c-hold-read (TestHoldRead…), c-hold-fails-open
// (TestHoldFailsOpen…) and the log half of c-hold-visible (TestHoldVisible…). Each drives the real
// poller against a local HTTP server and reads the hold on a clock the test moves; nothing here
// sleeps but the one row of the lease's own end between polls.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/testlog"
	"github.com/sjawhar/legion/daemon/internal/testport"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

const (
	team   = "team-a"
	bearer = "the-gateway-bearer"
	lease  = 90 * time.Second
)

var start = time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)

// movedClock is the daemon's clock as a test moves it.
type movedClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *movedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *movedClock) Set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = at
}

// gateway is a local stand-in for the hold route: it answers what the test sets, and records
// every request.
type gateway struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	answer http.HandlerFunc
	seen   []*http.Request
}

func (g *gateway) set(answer http.HandlerFunc) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.answer = answer
}

func (g *gateway) requests() []*http.Request {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]*http.Request(nil), g.seen...)
}

// rig is a poller of team's hold against a gateway, on a moved clock, logging into logs.
type rig struct {
	*gateway
	clock  *movedClock
	logs   *testlog.Buffer
	hold   *Hold
	poller *Poller
}

func newRig(t *testing.T) *rig {
	t.Helper()
	g := &gateway{t: t}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.seen = append(g.seen, r)
		answer := g.answer
		g.mu.Unlock()
		if answer == nil {
			http.Error(w, "the test set no answer", http.StatusTeapot)
			return
		}
		answer(w, r)
	}))
	t.Cleanup(g.srv.Close)
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte(bearer+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	logs := &testlog.Buffer{}
	clock := &movedClock{now: start}
	hold := New(team, slog.New(slog.NewTextHandler(logs, nil)))
	return &rig{gateway: g, clock: clock, logs: logs, hold: hold,
		poller: &Poller{URL: g.srv.URL, TokenFile: token, Hold: hold, Now: clock.Now, Timeout: 200 * time.Millisecond}}
}

// pollAt moves the clock to at and polls once.
func (r *rig) pollAt(at time.Time) {
	r.clock.Set(at)
	r.poller.Poll(context.Background())
}

// object is one object of the route's answer.
func object(team, state string, holdUntil, at time.Time, reason string) map[string]string {
	o := map[string]string{"team": team, "state": state, "at": at.Format(time.RFC3339), "reason": reason}
	if !holdUntil.IsZero() {
		o["hold_until"] = holdUntil.Format(time.RFC3339)
	}
	return o
}

func answers(objects ...map[string]string) http.HandlerFunc {
	if objects == nil {
		objects = []map[string]string{}
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(objects); err != nil {
			panic(err)
		}
	}
}

func body(text string, status int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(text))
	}
}

// wantUntil is the hold that stands at now ending at want, the zero time for none.
func (r *rig) wantUntil(now, want time.Time) {
	r.t.Helper()
	if got := r.hold.Until(now); !got.Equal(want) {
		if want.IsZero() {
			r.t.Fatalf("at %s the hold stands until %s, want none", now.Format(time.RFC3339), got.Format(time.RFC3339))
		}
		r.t.Fatalf("at %s the hold stands until %s, want %s", now.Format(time.RFC3339), got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

func (r *rig) lines(substrings ...string) []string {
	var found []string
	for line := range strings.Lines(r.logs.String()) {
		matches := true
		for _, s := range substrings {
			matches = matches && strings.Contains(line, s)
		}
		if matches {
			found = append(found, strings.TrimSpace(line))
		}
	}
	return found
}

func (r *rig) wantLines(n int, substrings ...string) []string {
	r.t.Helper()
	found := r.lines(substrings...)
	if len(found) != n {
		r.t.Fatalf("log lines with %q = %d, want %d; the log:\n%s", substrings, len(found), n, r.logs.String())
	}
	return found
}

// A hold for the daemon's own team holds until its lease ends, and the poll that read it is a GET
// carrying the bearer from the token file.
func TestHoldReadForTheDaemonsOwnTeam(t *testing.T) {
	r := newRig(t)
	r.set(answers(object(team, StateHold, start.Add(lease), start, "the team's guard refuses")))

	r.pollAt(start)

	r.wantUntil(start, start.Add(lease))
	r.wantUntil(start.Add(lease-time.Second), start.Add(lease))
	r.wantUntil(start.Add(lease), time.Time{})
	seen := r.requests()
	if len(seen) != 1 || seen[0].Method != http.MethodGet || seen[0].Header.Get("Authorization") != "Bearer "+bearer {
		t.Fatalf("the gateway saw %d requests, the first %s with Authorization %q, want one GET with the token file's bearer",
			len(seen), seen[0].Method, seen[0].Header.Get("Authorization"))
	}
}

// A hold for every team holds this daemon too.
func TestHoldReadForEveryTeam(t *testing.T) {
	r := newRig(t)
	r.set(answers(object(AllTeams, StateHold, start.Add(lease), start, "an organization's bucket is empty")))

	r.pollAt(start)

	r.wantUntil(start, start.Add(lease))
	if view, ok := r.hold.View(start); !ok || view.Team != AllTeams || view.Reason != "an organization's bucket is empty" {
		t.Fatalf("the hold's view is %+v (standing %t), want the star team's hold with its reason", view, ok)
	}
}

// A hold for another team holds nothing here.
func TestHoldReadIgnoresAnotherTeam(t *testing.T) {
	r := newRig(t)
	r.set(answers(object("team-b", StateHold, start.Add(lease), start, "another team's guard refuses")))

	r.pollAt(start)

	r.wantUntil(start, time.Time{})
	if _, ok := r.hold.View(start); ok {
		t.Fatal("a hold for another team stands here")
	}
	r.wantLines(0, BeganMessage)
}

// A hold_until ten minutes ahead is read as two minutes ahead of the daemon's own clock.
func TestHoldReadCutsALeaseTenMinutesAheadToTwoMinutes(t *testing.T) {
	r := newRig(t)
	r.set(answers(object(team, StateHold, start.Add(10*time.Minute), start, "a far lease")))

	r.pollAt(start)

	r.wantUntil(start, start.Add(MaxLease))
	r.wantUntil(start.Add(MaxLease-time.Nanosecond), start.Add(MaxLease))
	r.wantUntil(start.Add(MaxLease), time.Time{})
}

// A renewal the gateway sends with a newer `at` extends the hold; the same object sent again does
// not, so a gateway that keeps answering one stale object holds nothing past its first lease.
func TestHoldReadExtendsOnlyForANewerAnswer(t *testing.T) {
	r := newRig(t)
	first := object(team, StateHold, start.Add(10*time.Minute), start, "stale")
	r.set(answers(first))
	r.pollAt(start)
	r.pollAt(start.Add(time.Minute))
	r.wantUntil(start.Add(time.Minute), start.Add(MaxLease))

	r.set(answers(object(team, StateHold, start.Add(time.Minute+lease), start.Add(time.Minute), "renewed")))
	r.pollAt(start.Add(time.Minute))
	r.wantUntil(start.Add(MaxLease), start.Add(time.Minute+lease))
}

// An `open` older than the hold that stands ends nothing: the newest `at` decides.
func TestHoldReadKeepsAHoldAgainstAnOlderOpen(t *testing.T) {
	r := newRig(t)
	r.set(answers(object(team, StateHold, start.Add(lease), start, "held")))
	r.pollAt(start)

	r.set(answers(object(team, StateOpen, time.Time{}, start.Add(-time.Second), "")))
	r.pollAt(start.Add(10 * time.Second))

	r.wantUntil(start.Add(10*time.Second), start.Add(lease))
}

// failsOpen is the shape of every failure row: with no hold standing, the failed poll holds
// nothing; with one standing, the failed poll neither ends nor extends it, and it ends at its own
// lease. fail turns the route into the failure before each failed poll.
func failsOpen(t *testing.T, fail func(*rig)) *rig {
	t.Helper()
	cold := newRig(t)
	fail(cold)
	cold.pollAt(start)
	cold.wantUntil(start, time.Time{})
	if _, ok := cold.hold.View(start); ok {
		t.Fatal("a failed poll with no hold before it holds")
	}

	r := newRig(t)
	r.set(answers(object(team, StateHold, start.Add(lease), start, "held")))
	r.pollAt(start)
	r.wantUntil(start, start.Add(lease))
	fail(r)
	for _, at := range []time.Duration{15 * time.Second, 30 * time.Second, lease - time.Second} {
		r.pollAt(start.Add(at))
		r.wantUntil(start.Add(at), start.Add(lease))
	}
	r.pollAt(start.Add(lease))
	r.wantUntil(start.Add(lease), time.Time{})
	r.wantUntil(start.Add(lease+MaxLease), time.Time{})
	r.wantLines(1, EndedMessage, "endedBy="+EndedByLease)
	r.wantLines(1, FailingMessage)
	return r
}

func TestHoldFailsOpenOnAServerError(t *testing.T) {
	failsOpen(t, func(r *rig) { r.set(body("upstream exploded", http.StatusInternalServerError)) })
}

func TestHoldFailsOpenOnAnUnauthorizedAnswer(t *testing.T) {
	failsOpen(t, func(r *rig) { r.set(body(`[]`, http.StatusUnauthorized)) })
}

func TestHoldFailsOpenOnABodyThatIsNotTheList(t *testing.T) {
	for name, text := range map[string]string{
		"not JSON":            "<html>gateway</html>",
		"an object":           `{"team":"team-a","state":"hold","hold_until":"2026-11-25T06:01:30Z","at":"2026-11-25T06:00:00Z"}`,
		"null":                "null",
		"empty":               "",
		"cut short":           `[{"team":"team-a","state":"hold"`,
		"an unknown state":    `[{"team":"team-a","state":"throttle","hold_until":"2026-11-25T06:01:30Z","at":"2026-11-25T06:00:00Z"}]`,
		"a bad hold_until":    `[{"team":"*","state":"hold","hold_until":"soon","at":"2026-11-25T06:00:00Z"}]`,
		"no at":               `[{"team":"team-a","state":"hold","hold_until":"2026-11-25T06:01:30Z"}]`,
		"a team not a string": `[{"team":7,"state":"hold","hold_until":"2026-11-25T06:01:30Z","at":"2026-11-25T06:00:00Z"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			failsOpen(t, func(r *rig) { r.set(body(text, http.StatusOK)) })
		})
	}
}

// An answer longer than the poll reads is unreadable, however it would have parsed.
func TestHoldFailsOpenOnAnAnswerTooLongToRead(t *testing.T) {
	long := `[` + strings.Repeat(`{"team":"team-b","state":"open","at":"2026-11-25T06:00:00Z"},`, maxBody/50) +
		`{"team":"team-a","state":"hold","hold_until":"2026-11-25T06:10:00Z","at":"2026-11-25T06:00:30Z"}]`
	failsOpen(t, func(r *rig) { r.set(body(long, http.StatusOK)) })
}

// A gateway that takes the request and never answers it is cut at the poll's bound.
func TestHoldFailsOpenWhenTheGatewayStopsAnswering(t *testing.T) {
	failsOpen(t, func(r *rig) {
		r.set(func(_ http.ResponseWriter, req *http.Request) { <-req.Context().Done() })
	})
}

// A host that does not resolve, and a port nothing listens on, are failed polls.
func TestHoldFailsOpenWhenTheHostDoesNotResolve(t *testing.T) {
	failsOpen(t, func(r *rig) { r.poller.URL = "http://backpressure.invalid/hold" })
}

func TestHoldFailsOpenWhenTheConnectionIsRefused(t *testing.T) {
	failsOpen(t, func(r *rig) { r.poller.URL = "http://" + testport.Refused(t) + "/hold" })
}

// A token file the poll cannot read, or one with nothing in it, is a failed poll: the daemon
// presents no request it has no bearer for.
func TestHoldFailsOpenWithNoBearerToPresent(t *testing.T) {
	failsOpen(t, func(r *rig) { r.poller.TokenFile = filepath.Join(t.TempDir(), "missing") })
	failsOpen(t, func(r *rig) {
		empty := filepath.Join(t.TempDir(), "empty")
		if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		r.poller.TokenFile = empty
	})
}

// A lease that passes ends the hold with no answer at all, and the same object answered again
// after it does not bring the hold back.
func TestHoldFailsOpenWhenTheLeasePasses(t *testing.T) {
	r := newRig(t)
	r.set(answers(object(team, StateHold, start.Add(lease), start, "held")))
	r.pollAt(start)

	r.hold.Settle(start.Add(lease - time.Nanosecond))
	r.wantLines(0, EndedMessage)
	r.hold.Settle(start.Add(lease))

	r.wantUntil(start.Add(lease), time.Time{})
	r.wantLines(1, EndedMessage, "endedBy="+EndedByLease, "team="+team)
	r.pollAt(start.Add(lease + 10*time.Second))
	r.wantUntil(start.Add(lease+10*time.Second), time.Time{})
	r.wantLines(1, BeganMessage)
}

// An `open` with a newer `at` ends the hold at once, whatever lease it had left.
func TestHoldFailsOpenAtOnceOnANewerOpen(t *testing.T) {
	r := newRig(t)
	r.set(answers(object(team, StateHold, start.Add(lease), start, "held")))
	r.pollAt(start)

	r.set(answers(object(team, StateOpen, time.Time{}, start.Add(10*time.Second), "")))
	r.pollAt(start.Add(10 * time.Second))

	r.wantUntil(start.Add(10*time.Second), time.Time{})
	r.wantLines(1, EndedMessage, "endedBy="+EndedByGateway)
}

// A route that failed and answers again holds again, with no restart: the reading has nothing of
// the failures left in it.
func TestHoldFailsOpenAndHoldsAgainOnceTheRouteAnswers(t *testing.T) {
	r := failsOpen(t, func(r *rig) { r.set(body("down", http.StatusBadGateway)) })
	later := start.Add(3 * time.Minute)
	r.set(answers(object(AllTeams, StateHold, later.Add(lease), later, "back")))

	r.pollAt(later)

	r.wantUntil(later, later.Add(lease))
	r.wantLines(1, AnswersMessage, "failedPolls=4")
	r.wantLines(2, BeganMessage)
}

// The begin line names the team, the reason and the end; the end line says who ended it.
func TestHoldVisibleBeginsAndEndsWithOneLineEach(t *testing.T) {
	r := newRig(t)
	r.set(answers(object(AllTeams, StateHold, start.Add(lease), start, "organization bucket empty")))
	r.pollAt(start)
	r.set(answers(object(AllTeams, StateHold, start.Add(20*time.Second+lease), start.Add(20*time.Second), "organization bucket empty")))
	r.pollAt(start.Add(20 * time.Second))
	r.set(answers(object(AllTeams, StateOpen, time.Time{}, start.Add(40*time.Second), "")))
	r.pollAt(start.Add(40 * time.Second))
	r.pollAt(start.Add(50 * time.Second))

	r.wantLines(1, BeganMessage, "team=*", `reason="organization bucket empty"`, "holdUntil="+start.Add(lease).Format(time.RFC3339))
	r.wantLines(1, EndedMessage, "endedBy="+EndedByGateway, "heldFor=40s")
}

// A run of failed polls is one line however long it runs, and the poll that answers again ends
// the run with one line; the next run of failures writes one line again.
func TestHoldVisibleLogsARunOfFailuresOnce(t *testing.T) {
	r := newRig(t)
	r.set(body("down", http.StatusServiceUnavailable))
	for i := range 5 {
		r.pollAt(start.Add(time.Duration(i) * 12 * time.Second))
	}
	r.wantLines(1, FailingMessage, "team="+team, "503")

	r.set(answers())
	r.pollAt(start.Add(time.Minute))
	r.wantLines(1, AnswersMessage, "failedPolls=5")

	r.set(body("down", http.StatusServiceUnavailable))
	r.pollAt(start.Add(2 * time.Minute))
	r.pollAt(start.Add(3 * time.Minute))
	r.wantLines(2, FailingMessage)
}

// The hold counts each wake and each issue it puts back once, and a new hold counts from nothing.
func TestHoldVisibleCountsTheWakesAndAdmissionsItHolds(t *testing.T) {
	r := newRig(t)
	r.set(answers(object(team, StateHold, start.Add(lease), start, "held")))
	r.pollAt(start)

	for _, c := range []claim.Token{"legion-LEGION-1-architect", "legion-LEGION-2-tester", "legion-LEGION-1-architect"} {
		if until, held := r.hold.HoldWake(c, start.Add(time.Second)); !held || !until.Equal(start.Add(lease)) {
			t.Fatalf("HoldWake(%s) = %s, %t, want the hold's end and true", c, until, held)
		}
	}
	if !r.hold.HoldAdmission("LEGION-3", start.Add(time.Second)) {
		t.Fatal("HoldAdmission under a standing hold = false")
	}
	view, ok := r.hold.View(start.Add(time.Second))
	if !ok || view.Wakes != 2 || view.Admissions != 1 || !view.Since.Equal(start) || !view.Until.Equal(start.Add(lease)) {
		t.Fatalf("the hold's view is %+v (standing %t), want 2 wakes, 1 admission, since %s until %s", view, ok, start, start.Add(lease))
	}

	if _, held := r.hold.HoldWake("legion-LEGION-4-tester", start.Add(lease)); held {
		t.Fatal("HoldWake after the lease = held")
	}
	r.hold.Settle(start.Add(lease))
	r.wantLines(1, EndedMessage, "heldWakes=2", "heldAdmissions=1")

	later := start.Add(5 * time.Minute)
	r.set(answers(object(team, StateHold, later.Add(lease), later, "again")))
	r.pollAt(later)
	if view, ok := r.hold.View(later); !ok || view.Wakes != 0 || view.Admissions != 0 {
		t.Fatalf("the next hold's view is %+v (standing %t), want a standing hold counting nothing yet", view, ok)
	}
}

// Between two polls the poller wakes at the lease's end, so the end line is written when the hold
// ends rather than at the next poll, which here is an hour away.
func TestHoldVisibleEndsAtTheLeaseBetweenPolls(t *testing.T) {
	r := newRig(t)
	r.poller.Now = nil
	r.poller.Interval = func() time.Duration { return time.Hour }
	now := time.Now()
	r.set(answers(object(team, StateHold, now.Add(1500*time.Millisecond), now, "held")))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.poller.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	testwait.Eventually(t, "the hold's begin line", func() bool { return len(r.lines(BeganMessage)) == 1 })
	testwait.Eventually(t, "the hold's end line at its lease", func() bool { return len(r.lines(EndedMessage, "endedBy="+EndedByLease)) == 1 })
	if n := len(r.requests()); n != 1 {
		t.Fatalf("the gateway saw %d polls, want 1: the end came from the lease, not from a poll", n)
	}
}
