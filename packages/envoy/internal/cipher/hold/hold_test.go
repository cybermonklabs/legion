package hold_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/hold"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/store"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/testport"
)

// The people the tests check.
const (
	ada   = "ada@example.com"
	bob   = "bob@example.com"
	carol = "carol@example.com"
	dave  = "dave@example.com"
)

// servedWarning is the line the gate logs, once a minute per person, for a person it serves while
// Dispatch cannot be asked.
const servedWarning = "broker: Dispatch could not be asked whether a person is held; serving on its last answer"

// fakeClock is a clock a test moves by hand: the gate's and its Dispatch client's.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) read() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// line is one line a gate logged: its level, its message and each attribute as text.
type line struct {
	level   slog.Level
	message string
	attrs   map[string]string
}

// logs keeps every line a gate logs.
type logs struct{ logtest.Recorder }

// at is every line logged at level whose message is message (any message for "") and whose
// person attribute is person (any for "").
func (l *logs) at(level slog.Level, message, person string) []line {
	var found []line
	for _, record := range l.Records() {
		attrs := map[string]string{}
		record.Attrs(func(a slog.Attr) bool {
			attrs[a.Key] = a.Value.String()
			return true
		})
		if record.Level == level && (message == "" || record.Message == message) && (person == "" || attrs["person"] == person) {
			found = append(found, line{level: record.Level, message: record.Message, attrs: attrs})
		}
	}
	return found
}

// rig is one broker process's gate over a store: its Dispatch client, the clock both read, the
// lines it logs and the registry it counts on.
type rig struct {
	gate     *hold.Gate
	client   *projectaccess.Client
	clock    *fakeClock
	logs     *logs
	registry *metrics.Registry
}

func newRig(dispatchURL string, account ciphertoken.Account, st *store.Store) *rig {
	return newRigWith(dispatchURL, account, st, false)
}

// newRigWith is newRig with the gate's RequireDeadline set as requireDeadline says, as
// BROKER_HOLD_REQUIRES_DEADLINE sets it on an IAP deployment's broker.
func newRigWith(dispatchURL string, account ciphertoken.Account, st *store.Store, requireDeadline bool) *rig {
	clock := &fakeClock{now: time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)}
	client := projectaccess.New(dispatchURL, account, nil)
	client.UseClock(clock.read)
	logged := &logs{}
	registry := metrics.New()
	gate := hold.New(hold.Config{Access: client, Store: st, Log: slog.New(logged), Metrics: registry, Now: clock.read, RequireDeadline: requireDeadline})
	return &rig{gate: gate, client: client, clock: clock, logs: logged, registry: registry}
}

// unchecked is the broker_hold_unchecked_total sample labelled lastAnswer, 0 when there is none.
func (r *rig) unchecked(t *testing.T, lastAnswer string) int {
	t.Helper()
	recorder := httptest.NewRecorder()
	r.registry.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	series := `broker_hold_unchecked_total{last_answer="` + lastAnswer + `"} `
	for sample := range strings.Lines(recorder.Body.String()) {
		if value, ok := strings.CutPrefix(strings.TrimSpace(sample), series); ok {
			n, err := strconv.Atoi(value)
			if err != nil {
				t.Fatalf("the sample %q holds no count", sample)
			}
			return n
		}
	}
	return 0
}

// openStore opens a second pool on the schema databaseURL names, closed when t ends: a second
// broker process's, or one a test closes.
func openStore(t *testing.T, databaseURL string) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return st
}

// answering is a Dispatch whose person route answers every question status and body, until t
// ends.
func answering(t *testing.T, status int, body string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// never is a Dispatch that answers nothing until the caller hangs up or t ends, and the number of
// questions it was asked.
func never(t *testing.T) (string, func() int) {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	return server.URL, func() int { mu.Lock(); defer mu.Unlock(); return calls }
}

// stalling is a Dispatch that holds each question open until the caller hangs up or the test ends
// the stall (end), and answers 503 at once to every question after that; with the number of
// questions it was asked. end returns once every question it held has had its answer written.
func stalling(t *testing.T) (url string, calls func() int, end func()) {
	t.Helper()
	var mu sync.Mutex
	asked := 0
	release := make(chan struct{})
	var held sync.WaitGroup
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked++
		held.Add(1)
		mu.Unlock()
		defer held.Done()
		select {
		case <-r.Context().Done():
		case <-release:
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	var once sync.Once
	end = func() {
		once.Do(func() { close(release) })
		held.Wait()
	}
	t.Cleanup(server.Close)
	t.Cleanup(end)
	return server.URL, func() int { mu.Lock(); defer mu.Unlock(); return asked }, end
}

// settle returns once r's client has no question about person in flight: it asks the client about
// person itself, which joins a question still in flight and returns once that ends, or, with none
// in flight, asks one of its own, which stalling answers 503 at once once its stall has ended. The
// gate is not asked, so it remembers nothing of this question.
func (r *rig) settle(t *testing.T, person string) {
	t.Helper()
	if _, err := r.client.Person(t.Context(), person); err == nil {
		t.Fatalf("the client's own question about %s was answered; want the stalled Dispatch's failure", person)
	}
}

// standing answers what the broker keeps for person, failing t when it cannot read it.
func standing(t *testing.T, st *store.Store, person string) (held bool, answeredAt time.Time, found bool) {
	t.Helper()
	held, answeredAt, _, found = standingUntil(t, st, person)
	return held, answeredAt, found
}

// standingUntil is standing with the deadline kept beside the answer, nil for none.
func standingUntil(t *testing.T, st *store.Store, person string) (held bool, answeredAt time.Time, notHeldUntil *time.Time, found bool) {
	t.Helper()
	held, answeredAt, notHeldUntil, found, err := st.PersonStanding(context.Background(), person)
	if err != nil {
		t.Fatalf("PersonStanding(%s): %v", person, err)
	}
	return held, answeredAt, notHeldUntil, found
}

// refuseWrites makes person_standing refuse every insert and update until allowWrites, as a
// database that takes reads and no writes.
func refuseWrites(t *testing.T, st *store.Store) {
	t.Helper()
	for _, statement := range []string{
		`create or replace function refuse_person_standing_writes() returns trigger language plpgsql as $$
			begin raise exception 'this test refuses every write to person_standing'; end $$`,
		`create trigger person_standing_refused before insert or update on person_standing
			for each row execute function refuse_person_standing_writes()`,
	} {
		if _, err := st.Pool.Exec(context.Background(), statement); err != nil {
			t.Fatal(err)
		}
	}
}

func allowWrites(t *testing.T, st *store.Store) {
	t.Helper()
	if _, err := st.Pool.Exec(context.Background(), `drop trigger person_standing_refused on person_standing`); err != nil {
		t.Fatal(err)
	}
}

// isHeld reports whether err refuses a held person and decides, matching hold.ErrHeld and not
// hold.ErrUncheckable.
func isHeld(err error) bool {
	return errors.Is(err, hold.ErrHeld) && !errors.Is(err, hold.ErrUncheckable)
}

// TestHeldWhenDispatchCannotBeAsked: with Dispatch's stand-in stopped, the gate answers from the
// last answer the broker kept. ada, whom Dispatch last answered held, is refused; bob, last
// answered not held, is served with one WARN line a minute and the counter labelled not_held;
// carol, of whom no answer is kept, is served the same way labelled none; and dave, whose kept
// answer the broker's database cannot read, is hold.ErrUncheckable. A new gate over the same
// database, Dispatch still stopped, answers ada, bob and carol alike.
func TestHeldWhenDispatchCannotBeAsked(t *testing.T) {
	ctx := t.Context()
	databaseURL := storetest.URL(t)
	st := openStore(t, databaseURL)
	dispatch := projectaccesstest.NewServer(t, nil, map[string]string{ada: projectaccess.MemberFull, bob: projectaccess.MemberFull})
	dispatch.SetHeld(ada, true)
	first := newRig(dispatch.URL, dispatch.Account, st)
	if err := first.gate.Check(ctx, ada); !isHeld(err) {
		t.Fatalf("Check(%s) while Dispatch answers her held = %v, want hold.ErrHeld", ada, err)
	}
	if err := first.gate.Check(ctx, bob); err != nil {
		t.Fatalf("Check(%s) while Dispatch answers him not held = %v, want nil", bob, err)
	}
	dispatch.SetDown(true)
	first.clock.advance(projectaccess.CacheTTL)

	answersAlike := func(r *rig, when string) {
		t.Helper()
		if err := r.gate.Check(ctx, ada); !isHeld(err) {
			t.Fatalf("Check(%s) %s = %v, want hold.ErrHeld: Dispatch last answered her held", ada, when, err)
		}
		for _, served := range []struct{ person, lastAnswer string }{{bob, "not_held"}, {carol, "none"}} {
			for range 2 {
				if err := r.gate.Check(ctx, served.person); err != nil {
					t.Fatalf("Check(%s) %s = %v, want nil: Dispatch's last answer kept for them is %s", served.person, when, err, served.lastAnswer)
				}
			}
			warned := r.logs.at(slog.LevelWarn, servedWarning, served.person)
			if len(warned) != 1 || warned[0].attrs["last_answer"] != served.lastAnswer || warned[0].attrs["error"] == "" {
				t.Fatalf("serving %s twice in a minute %s logged %+v, want one WARN %q with last_answer %s and the error", served.person, when, warned, servedWarning, served.lastAnswer)
			}
			if got := r.unchecked(t, served.lastAnswer); got != 1 {
				t.Fatalf("broker_hold_unchecked_total{last_answer=%q} after serving %s twice in a minute %s = %d, want 1", served.lastAnswer, served.person, when, got)
			}
		}
	}
	answersAlike(first, "once Dispatch cannot be asked")
	first.clock.advance(time.Minute)
	if err := first.gate.Check(ctx, bob); err != nil {
		t.Fatalf("Check(%s) a minute later = %v, want nil", bob, err)
	}
	if warned, counted := len(first.logs.at(slog.LevelWarn, servedWarning, bob)), first.unchecked(t, "not_held"); warned != 2 || counted != 2 {
		t.Fatalf("serving %s again a minute later left %d WARN lines and a count of %d, want 2 of each", bob, warned, counted)
	}

	unreadable := openStore(t, databaseURL)
	unreadable.Close()
	closed := newRig(dispatch.URL, dispatch.Account, unreadable)
	if err := closed.gate.Check(ctx, dave); !errors.Is(err, hold.ErrUncheckable) || errors.Is(err, hold.ErrHeld) || !strings.Contains(err.Error(), dave) {
		t.Fatalf("Check(%s) with Dispatch stopped and the store closed = %v, want hold.ErrUncheckable naming him", dave, err)
	}

	answersAlike(newRig(dispatch.URL, dispatch.Account, st), "at a new gate over the same database")
}

// TestHoldGateReadsEachFailureAsCannotBeAsked: each way Dispatch fails to answer is a failure to
// ask, which the gate answers from the answer it kept: a person kept held stays held through
// every one. An answer naming a member kind the broker does not know, whose held decodes, is an
// answer: it serves the person and is kept. Dispatch refusing the broker's own account is
// hold.ErrUncheckable for every person, one kept held among them, logged at ERROR.
func TestHoldGateReadsEachFailureAsCannotBeAsked(t *testing.T) {
	ctx := t.Context()
	st := storetest.Open(t)
	kept := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	asOf := time.Now().UTC().Truncate(time.Microsecond)
	failures := []struct {
		name     string
		dispatch func(t *testing.T) string
	}{
		// A port the test holds and nothing listens on, so every dial is refused.
		{"a refused connection", func(t *testing.T) string { return "http://" + testport.Refused(t) }},
		{"a timeout", func(t *testing.T) string { url, _ := never(t); return url }},
		{"a 500", func(t *testing.T) string {
			return answering(t, http.StatusInternalServerError, `{"code":"INTERNAL","error":"boom"}`)
		}},
		{"a 404", func(t *testing.T) string {
			return answering(t, http.StatusNotFound, `{"code":"NOT_FOUND","error":"no route"}`)
		}},
		{"a 404 naming PROJECT_NOT_FOUND", func(t *testing.T) string {
			return answering(t, http.StatusNotFound, `{"code":"PROJECT_NOT_FOUND","error":"no such project"}`)
		}},
		{"a 400", func(t *testing.T) string {
			return answering(t, http.StatusBadRequest, `{"code":"INVALID_QUERY","error":"person is required"}`)
		}},
		{"a 429", func(t *testing.T) string {
			return answering(t, http.StatusTooManyRequests, `{"code":"RATE_LIMITED","error":"slow down"}`)
		}},
		{"an answer with no held field", func(t *testing.T) string {
			return answering(t, http.StatusOK, `{"person":"x","member_kind":"full","as_of":"`+asOf.Format(time.RFC3339Nano)+`"}`)
		}},
		{"an answer with held and no as_of", func(t *testing.T) string {
			return answering(t, http.StatusOK, `{"person":"x","member_kind":"full","held":false}`)
		}},
		{"an answer that does not decode", func(t *testing.T) string { return answering(t, http.StatusOK, `not json`) }},
	}
	for i, c := range failures {
		t.Run(c.name, func(t *testing.T) {
			person := "held" + strconv.Itoa(i) + "@example.com"
			if err := st.KeepPersonStanding(ctx, person, true, kept, nil); err != nil {
				t.Fatal(err)
			}
			r := newRig(c.dispatch(t), projectaccesstest.StaticAccount("token"), st)
			start := time.Now()
			err := r.gate.Check(ctx, person)
			if !isHeld(err) {
				t.Fatalf("Check(%s), kept held, against %s = %v, want hold.ErrHeld: %s is no answer", person, c.name, err, c.name)
			}
			if took := time.Since(start); took > 3500*time.Millisecond {
				t.Fatalf("Check(%s) against %s took %s, want at most 3.5 s", person, c.name, took)
			}
		})
	}

	t.Run("an answer naming a member kind the broker does not know", func(t *testing.T) {
		const person = "guest@example.com"
		if err := st.KeepPersonStanding(ctx, person, true, kept, nil); err != nil {
			t.Fatal(err)
		}
		r := newRig(answering(t, http.StatusOK, `{"person":"`+person+`","member_kind":"guest","held":false,"as_of":"`+asOf.Format(time.RFC3339Nano)+`"}`),
			projectaccesstest.StaticAccount("token"), st)
		if err := r.gate.Check(ctx, person); err != nil {
			t.Fatalf("Check(%s) answered not held under an unknown member kind = %v, want nil: its held decodes, so it is an answer", person, err)
		}
		if held, at, found := standing(t, st, person); !found || held || !at.Equal(asOf) {
			t.Fatalf("person_standing for %s = held %t as of %v (found %t), want the answer kept: not held as of %v", person, held, at, found, asOf)
		}
	})

	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(fmt.Sprintf("a %d refusing the broker's own account", status), func(t *testing.T) {
			person := "refused" + strconv.Itoa(status) + "@example.com"
			if err := st.KeepPersonStanding(ctx, person, true, kept, nil); err != nil {
				t.Fatal(err)
			}
			r := newRig(answering(t, status, `{"code":"ACCESS_ACCOUNT_REQUIRED","error":"not listed"}`), projectaccesstest.StaticAccount("token"), st)
			for _, checked := range []string{person, "nobody" + strconv.Itoa(status) + "@example.com"} {
				if err := r.gate.Check(ctx, checked); !errors.Is(err, hold.ErrUncheckable) || errors.Is(err, hold.ErrHeld) {
					t.Fatalf("Check(%s) with Dispatch answering %d = %v, want hold.ErrUncheckable alone", checked, status, err)
				}
			}
			if logged := r.logs.at(slog.LevelError, "", ""); len(logged) != 1 {
				t.Fatalf("two checks refused by Dispatch's %d in a minute logged %d ERROR lines, want 1: %+v", status, len(logged), logged)
			}
			if warned := r.logs.at(slog.LevelWarn, "", ""); len(warned) != 0 {
				t.Fatalf("a refusal of the broker's own account logged %+v at WARN, want nothing served", warned)
			}
		})
	}
}

// noHeldWarning is the line the gate logs, once a minute for every person at once, while Dispatch
// answers with no held field.
const noHeldWarning = "broker: Dispatch answers no held, as a Dispatch older than the hold does; serving each person on the last answer Dispatch gave for them"

// TestADispatchThatAnswersNoHeldIsLoggedOnceAMinuteForEveryone: a Dispatch older than the hold
// answers every person with no held field. That is the deployment's state, not each person's, and
// Dispatch was asked: the gate answers each person from the answer it kept, as it does while
// Dispatch cannot be asked (ada, kept held, stays held; bob, kept not held, and carol, of whom no
// answer is kept, are served), and logs one WARN line a minute for everyone, naming the error. It
// logs no line per person that Dispatch could not be asked, and counts nobody in
// broker_hold_unchecked_total. A caller with no time left to ask, whose person the client holds a
// young answer naming no held for, is answered the same way: a second gate over the same client
// stands for the gate once its five-second memory of the answer has passed and the client's cache
// has not.
//
// The fault: the gate reading the answer as a failure to ask, as it did; bob and carol each get
// the per-person WARN line and a count, and the one line a minute is missing. The same fault on
// the path with no time to ask alone (its cached answer read as no answer, unasked for want of
// time): the second gate logs bob's per-person line and no line a minute.
func TestADispatchThatAnswersNoHeldIsLoggedOnceAMinuteForEveryone(t *testing.T) {
	ctx := t.Context()
	st := storetest.Open(t)
	kept := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if err := st.KeepPersonStanding(ctx, ada, true, kept, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.KeepPersonStanding(ctx, bob, false, kept, nil); err != nil {
		t.Fatal(err)
	}
	asOf := time.Now().UTC().Truncate(time.Microsecond)
	r := newRig(answering(t, http.StatusOK, `{"person":"x","member_kind":"full","as_of":"`+asOf.Format(time.RFC3339Nano)+`"}`),
		projectaccesstest.StaticAccount("token"), st)
	if err := r.gate.Check(ctx, ada); !isHeld(err) {
		t.Fatalf("Check(%s), kept held, against a Dispatch that answers no held = %v, want hold.ErrHeld", ada, err)
	}
	for _, person := range []string{bob, carol, bob, carol} {
		if err := r.gate.Check(ctx, person); err != nil {
			t.Fatalf("Check(%s) against a Dispatch that answers no held = %v, want nil: served on the answer kept for them", person, err)
		}
	}
	if warned := r.logs.at(slog.LevelWarn, servedWarning, ""); len(warned) != 0 {
		t.Fatalf("a Dispatch that answers no held logged %+v, want no line per person that Dispatch could not be asked", warned)
	}
	for _, lastAnswer := range []string{"not_held", "none"} {
		if got := r.unchecked(t, lastAnswer); got != 0 {
			t.Fatalf("broker_hold_unchecked_total{last_answer=%q} against a Dispatch that answers no held = %d, want 0", lastAnswer, got)
		}
	}
	noHeld := r.logs.at(slog.LevelWarn, noHeldWarning, "")
	if len(noHeld) != 1 || !strings.Contains(noHeld[0].attrs["error"], "names no held") {
		t.Fatalf("five checks in a minute against a Dispatch that answers no held logged %+v, want one WARN %q naming the error", noHeld, noHeldWarning)
	}
	if all := r.logs.at(slog.LevelWarn, "", ""); len(all) != 1 {
		t.Fatalf("five checks in a minute against a Dispatch that answers no held logged %d WARN lines, want 1: %+v", len(all), all)
	}
	r.clock.advance(time.Minute)
	if err := r.gate.Check(ctx, bob); err != nil {
		t.Fatalf("Check(%s) a minute later = %v, want nil", bob, err)
	}
	if got := len(r.logs.at(slog.LevelWarn, noHeldWarning, "")); got != 2 {
		t.Fatalf("a check a minute later left %d lines %q, want 2", got, noHeldWarning)
	}

	logged := &logs{}
	second := &rig{client: r.client, clock: r.clock, logs: logged, registry: metrics.New()}
	second.gate = hold.New(hold.Config{Access: r.client, Store: st, Log: slog.New(logged), Metrics: second.registry, Now: r.clock.read})
	noTime, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if err := second.gate.Check(noTime, bob); err != nil {
		t.Fatalf("Check(%s) with 0.3 s left and a young answer naming no held cached = %v, want nil", bob, err)
	}
	if warned := second.logs.at(slog.LevelWarn, servedWarning, ""); len(warned) != 0 || second.unchecked(t, "not_held") != 0 {
		t.Fatalf("Check(%s) with 0.3 s left logged %+v and counted %d, want no line per person and no count", bob, warned, second.unchecked(t, "not_held"))
	}
	if got := len(second.logs.at(slog.LevelWarn, noHeldWarning, "")); got != 1 {
		t.Fatalf("Check(%s) with 0.3 s left logged %d lines %q, want 1", bob, got, noHeldWarning)
	}
}

// TestHoldGateKeepsAnswerThirtySeconds: the gate answers from Dispatch's answer as long as the
// broker caches it, thirty seconds from its ask, so a hold Dispatch records after that answer is
// read at the first check after those thirty seconds. An answer the gate has written is not
// written again, and one another caller fetched (the exchange's MemberKind) is written at the
// gate's next check of that person.
func TestHoldGateKeepsAnswerThirtySeconds(t *testing.T) {
	ctx := t.Context()
	st := storetest.Open(t)
	dispatch := projectaccesstest.NewServer(t, nil, map[string]string{ada: projectaccess.MemberFull, bob: projectaccess.MemberFull})
	r := newRig(dispatch.URL, dispatch.Account, st)
	check := func(person string, wantHeld bool, wantCalls int, when string) {
		t.Helper()
		err := r.gate.Check(ctx, person)
		if wantHeld && !isHeld(err) || !wantHeld && err != nil {
			t.Fatalf("Check(%s) %s = %v, want held %t", person, when, err, wantHeld)
		}
		if got := dispatch.Calls(); got != wantCalls {
			t.Fatalf("Check(%s) %s left %d calls to Dispatch in all, want %d", person, when, got, wantCalls)
		}
	}
	check(ada, false, 1, "first")
	dispatch.SetHeld(ada, true)
	r.clock.advance(projectaccess.CacheTTL - time.Second)
	check(ada, false, 1, "just under thirty seconds later, after Dispatch held her")
	r.clock.advance(time.Second)
	check(ada, true, 2, "thirty seconds later")
	refuseWrites(t, st)
	check(ada, true, 2, "again, with person_standing refusing writes: the answer it already wrote is not written again")
	allowWrites(t, st)

	if kind, err := r.client.MemberKind(ctx, bob); err != nil || kind != projectaccess.MemberFull {
		t.Fatalf("MemberKind(%s) = %q, %v; want full", bob, kind, err)
	}
	check(bob, false, 3, "after MemberKind fetched his answer")
	if held, _, found := standing(t, st, bob); !found || held {
		t.Fatalf("person_standing for %s after the gate checked the answer MemberKind fetched = held %t (found %t), want it kept, not held", bob, held, found)
	}
}

// TestHoldGateBoundsEachAsk: an ask of a Dispatch that never answers ends within the gate's three
// seconds, and the failure is remembered five seconds: a check in that time asks Dispatch nothing,
// which the number of questions Dispatch was asked decides. The first question has ended before the
// second check (stalling's end, settle), so a second check that asked would make a question of its
// own, not join the first: the client shares one call per person for up to its ten seconds.
//
// Control: the failure not remembered (the gate's `if ctx.Err() != nil` read as true); the second
// check asks Dispatch a question of its own, and the test fails naming one more question.
func TestHoldGateBoundsEachAsk(t *testing.T) {
	ctx := t.Context()
	url, calls, end := stalling(t)
	r := newRig(url, projectaccesstest.StaticAccount("token"), storetest.Open(t))
	start := time.Now()
	if err := r.gate.Check(ctx, ada); err != nil {
		t.Fatalf("Check(%s) against a Dispatch that never answers, with no answer kept = %v, want nil, served unchecked", ada, err)
	}
	if took := time.Since(start); took > 3500*time.Millisecond {
		t.Fatalf("Check(%s) against a Dispatch that never answers took %s, want at most 3.5 s", ada, took)
	}
	end()
	r.settle(t, ada)
	before := calls()
	r.clock.advance(5*time.Second - time.Millisecond)
	if err := r.gate.Check(ctx, ada); err != nil {
		t.Fatalf("Check(%s) again within five seconds = %v, want nil", ada, err)
	}
	if got := calls() - before; got != 0 {
		t.Fatalf("a check within five seconds of a failed ask asked Dispatch %d more question(s), want none", got)
	}
}

// calloutBudget is the NATS auth service's whole decision (natscallout's decideBudget), the
// deadline of the context its introspection, and so this gate's check, runs under.
const calloutBudget = 1500 * time.Millisecond

// TestAHangingDispatchIsAnsweredFromTheKeptRowWithinTheCalloutsBudget: the NATS auth service gives
// a check the 1.5 s of its whole decision. While Dispatch hangs, a check with that budget is
// answered from the row person_standing keeps, and returns while the budget still has time: ada,
// kept not held, is served, with one WARN line and broker_hold_unchecked_total counting her; bob,
// kept held, is refused hold.ErrHeld. The ask's own end is Dispatch's failure, remembered: once the
// first questions have ended (stalling's end, settle), so that a check that asked would make a
// question of its own rather than join one the client still shares, ada's redial at once asks
// Dispatch nothing and is served the same way; a redial after the failure is forgotten asks again,
// meets a Dispatch that answers 503, and is served from the kept row too.
//
// Controls: the ask bounded by the caller's deadline alone, as it was; the ask then ends only with
// the caller's budget, the kept row is read with a spent context, and ada and bob are each
// hold.ErrUncheckable past the budget. The failure not remembered (the gate's
// `if ctx.Err() != nil` read as true); ada's redial at once asks a question of its own, and the
// test fails naming it.
func TestAHangingDispatchIsAnsweredFromTheKeptRowWithinTheCalloutsBudget(t *testing.T) {
	databaseURL := storetest.URL(t)
	asOf := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	keep := func(person string, held bool) {
		t.Helper()
		body := fmt.Sprintf(`{"person":%q,"member_kind":"full","held":%t,"as_of":%q}`, person, held, asOf.Format(time.RFC3339Nano))
		_ = newRig(answering(t, http.StatusOK, body), projectaccesstest.StaticAccount("token"), openStore(t, databaseURL)).gate.Check(t.Context(), person)
		if kept, _, found := standing(t, openStore(t, databaseURL), person); !found || kept != held {
			t.Fatalf("person_standing for %s = held %t (found %t), want held %t kept", person, kept, found, held)
		}
	}
	keep(ada, false)
	keep(bob, true)

	url, calls, end := stalling(t)
	r := newRig(url, projectaccesstest.StaticAccount("token"), openStore(t, databaseURL))
	check := func(person, when string) error {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), calloutBudget)
		defer cancel()
		err := r.gate.Check(ctx, person)
		if ctx.Err() != nil {
			t.Errorf("Check(%s) %s returned after the callout's %s budget had run out (%v), want it answered within the budget", person, when, calloutBudget, err)
		}
		return err
	}

	if err := check(ada, "while Dispatch hangs"); err != nil {
		t.Errorf("Check(%s) while Dispatch hangs, kept not held = %v, want nil, served from the kept row", ada, err)
	}
	if got := r.unchecked(t, "not_held"); got != 1 {
		t.Errorf("broker_hold_unchecked_total{last_answer=\"not_held\"} after ada's check = %d, want 1", got)
	}
	if got := len(r.logs.at(slog.LevelWarn, "broker: Dispatch could not be asked whether a person is held; serving on its last answer", ada)); got != 1 {
		t.Errorf("WARN lines serving %s on her last answer = %d, want 1", ada, got)
	}
	if err := check(bob, "while Dispatch hangs"); !isHeld(err) {
		t.Errorf("Check(%s) while Dispatch hangs, kept held = %v, want hold.ErrHeld alone", bob, err)
	}
	end()
	r.settle(t, ada)
	before := calls()
	if err := check(ada, "on her redial at once"); err != nil {
		t.Errorf("Check(%s) on her redial at once = %v, want nil, served from the kept row", ada, err)
	}
	if got := calls() - before; got != 0 {
		t.Errorf("ada's redial at once asked Dispatch %d question(s), want none: the ask's end is remembered", got)
	}
	r.clock.advance(5 * time.Second)
	if err := check(ada, "on her redial once the failure is forgotten"); err != nil {
		t.Errorf("Check(%s) on her redial once the failure is forgotten = %v, want nil, served from the kept row", ada, err)
	}
	if got := calls() - before; got != 1 {
		t.Errorf("ada's redial once the failure is forgotten asked Dispatch %d question(s) in all since the stall ended, want 1", got)
	}
}

// TestACallerWithNoTimeToAskIsServedFromTheCache: a check whose caller has no more than the gate's
// half-second reserve left asks Dispatch nothing, and is still answered from the answer the client
// cached young (thirty seconds), as Dispatch's answer: with Dispatch answering, ada's check fills
// the cache and the kept row, and a callout's check of her with 0.3 s left is served with no WARN
// line and no broker_hold_unchecked_total count, and asks no second question.
//
// Control: the gate answering every such caller from the kept row alone, as it did; ada is served
// with the WARN line and the count, as a person Dispatch could not be asked about, and the test
// fails naming both.
func TestACallerWithNoTimeToAskIsServedFromTheCache(t *testing.T) {
	asOf := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	var mu sync.Mutex
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"person":%q,"member_kind":"full","held":false,"as_of":%q}`, ada, asOf.Format(time.RFC3339Nano))
	}))
	t.Cleanup(server.Close)
	r := newRig(server.URL, projectaccesstest.StaticAccount("token"), storetest.Open(t))
	if err := r.gate.Check(t.Context(), ada); err != nil {
		t.Fatalf("Check(%s) with Dispatch answering her not held = %v, want nil", ada, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	if err := r.gate.Check(ctx, ada); err != nil {
		t.Errorf("Check(%s) with 0.3 s left and her answer cached = %v, want nil, served from the cache", ada, err)
	}
	if got := r.unchecked(t, "not_held"); got != 0 {
		t.Errorf("broker_hold_unchecked_total{last_answer=\"not_held\"} after the check with 0.3 s left = %d, want 0: the cached answer is Dispatch's", got)
	}
	if got := len(r.logs.at(slog.LevelWarn, servedWarning, ada)); got != 0 {
		t.Errorf("WARN lines serving %s on her last answer after the check with 0.3 s left = %d, want 0", ada, got)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("Dispatch was asked %d questions, want 1: the check with 0.3 s left asks none", calls)
	}
}

// TestHoldGateKeepsTheLatestAnswer: two broker processes write the answers Dispatch gave them out
// of order, the later answer first; the earlier answer, written second, changes nothing, so the
// row keeps the later answered_at. Each gate answers from the answer it was given.
func TestHoldGateKeepsTheLatestAnswer(t *testing.T) {
	ctx := t.Context()
	databaseURL := storetest.URL(t)
	t2 := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	t1 := t2.Add(-time.Second)
	later := newRig(answering(t, http.StatusOK, `{"person":"ada","member_kind":"full","held":false,"as_of":"`+t2.Format(time.RFC3339Nano)+`"}`),
		projectaccesstest.StaticAccount("token"), openStore(t, databaseURL))
	earlier := newRig(answering(t, http.StatusOK, `{"person":"ada","member_kind":"full","held":true,"as_of":"`+t1.Format(time.RFC3339Nano)+`"}`),
		projectaccesstest.StaticAccount("token"), openStore(t, databaseURL))
	if err := later.gate.Check(ctx, ada); err != nil {
		t.Fatalf("Check(%s) given not held as of t2 = %v, want nil", ada, err)
	}
	earlier.clock.advance(time.Second)
	if err := earlier.gate.Check(ctx, ada); !isHeld(err) {
		t.Fatalf("Check(%s) given held as of t1, asked later = %v, want hold.ErrHeld: the gate answers from Dispatch's answer", ada, err)
	}
	if held, at, found := standing(t, openStore(t, databaseURL), ada); !found || held || !at.Equal(t2) {
		t.Fatalf("person_standing for %s = held %t as of %v (found %t), want not held as of t2 %v: an answer Dispatch gave earlier never replaces a later one", ada, held, at, found, t2)
	}
}

// TestTheHoldGatesAnswerLeavesItsKeptKindServed: on a broker whose Dispatch client keeps the
// member kind of each answer it fetches (projectaccess.Client.Keep) beside the gate's own
// person_standing, as cmd/cipherd builds one, the gate writes the hold of the very answer whose
// kind the client keeps, at the same as_of, and that kind stands: once Dispatch is down ada,
// answered full and not held, is served full by the client and not held by the gate. A later
// answer the gate keeps whose kind no write kept leaves no kind served.
//
// The fault: store.KeptMemberKind reading a kind no later than the hold's answer as none, not only
// an earlier one; ada is refused once Dispatch is down.
func TestTheHoldGatesAnswerLeavesItsKeptKindServed(t *testing.T) {
	ctx := t.Context()
	st := storetest.Open(t)
	dispatch := projectaccesstest.NewServer(t, nil, map[string]string{ada: projectaccess.MemberFull})
	r := newRig(dispatch.URL, dispatch.Account, st)
	r.client.Keep(st, slog.New(&logs{}), metrics.New())
	if err := r.gate.Check(ctx, ada); err != nil {
		t.Fatalf("Check(%s) answered full, not held = %v, want nil", ada, err)
	}
	_, heldAsOf, found := standing(t, st, ada)
	kind, kindAsOf, kept, err := st.KeptMemberKind(ctx, ada)
	if !found || err != nil || !kept || kind != projectaccess.MemberFull || !kindAsOf.Equal(heldAsOf) {
		t.Fatalf("after the gate checked ada: hold kept %t as of %v; kind %q kept %t as of %v, %v; want both kept, full, at one as_of", found, heldAsOf, kind, kept, kindAsOf, err)
	}
	dispatch.SetDown(true)
	r.clock.advance(projectaccess.CacheTTL)
	if kind, err := r.client.MemberKind(ctx, ada); err != nil || kind != projectaccess.MemberFull {
		t.Fatalf("MemberKind(%s) while Dispatch is down = %q, %v; want full, the kept kind beside the gate's hold", ada, kind, err)
	}
	if err := r.gate.Check(ctx, ada); err != nil {
		t.Fatalf("Check(%s) while Dispatch is down = %v, want nil: kept not held", ada, err)
	}
	if err := st.KeepPersonStanding(ctx, ada, false, heldAsOf.Add(time.Second), nil); err != nil {
		t.Fatal(err)
	}
	if kind, err := r.client.MemberKind(ctx, ada); err == nil {
		t.Fatalf("MemberKind(%s) while Dispatch is down, a later hold kept with no kind = %q; want refused", ada, kind)
	}
}

// TestHoldGateKeepsTheHeldAnswerAgainstAnOlderNotHeld: a held answer as of t2 is written; a
// not-held answer as of t1, earlier than t2 though asked later, is not; and with Dispatch then
// stopped, the person is held.
func TestHoldGateKeepsTheHeldAnswerAgainstAnOlderNotHeld(t *testing.T) {
	ctx := t.Context()
	databaseURL := storetest.URL(t)
	t2 := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	t1 := t2.Add(-time.Second)
	newerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"person":"ada","member_kind":"full","held":true,"as_of":"` + t2.Format(time.RFC3339Nano) + `"}`))
	}))
	t.Cleanup(newerServer.Close)
	olderServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"person":"ada","member_kind":"full","held":false,"as_of":"` + t1.Format(time.RFC3339Nano) + `"}`))
	}))
	t.Cleanup(olderServer.Close)
	newer := newRig(newerServer.URL, projectaccesstest.StaticAccount("token"), openStore(t, databaseURL))
	older := newRig(olderServer.URL, projectaccesstest.StaticAccount("token"), openStore(t, databaseURL))
	if err := newer.gate.Check(ctx, ada); !isHeld(err) {
		t.Fatalf("Check(%s) given held as of t2 = %v, want hold.ErrHeld", ada, err)
	}
	older.clock.advance(time.Second)
	if err := older.gate.Check(ctx, ada); err != nil {
		t.Fatalf("Check(%s) given not held as of t1, asked later = %v, want nil: the gate answers from Dispatch's answer", ada, err)
	}
	newerServer.Close()
	olderServer.Close()
	older.clock.advance(projectaccess.CacheTTL)
	if err := older.gate.Check(ctx, ada); !isHeld(err) {
		t.Fatalf("Check(%s) with Dispatch stopped, after a held answer as of t2 and a not-held one as of t1 = %v, want hold.ErrHeld: the older not-held answer must not replace the held one", ada, err)
	}
	if err := newRig(olderServer.URL, projectaccesstest.StaticAccount("token"), openStore(t, databaseURL)).gate.Check(ctx, ada); !isHeld(err) {
		t.Fatalf("Check(%s) at a new gate with Dispatch stopped = %v, want hold.ErrHeld", ada, err)
	}
}

// TestHoldGateRefusesWhenAHeldAnswerCannotBeWritten: a held answer the broker cannot write to
// person_standing refuses the person as both hold.ErrUncheckable and hold.ErrHeld, since a later
// outage of Dispatch could otherwise serve them. With Dispatch then stopped and the write still
// failing, the person is held from the gate's memory; once the store takes writes, the next ask
// writes the row. A not-held answer it cannot write is served, logged at WARN once, and the next
// answer from Dispatch is written as any answer is. The write runs apart from the caller's
// request, so a caller that hangs up does not stop it.
func TestHoldGateRefusesWhenAHeldAnswerCannotBeWritten(t *testing.T) {
	t.Run("while the write fails and once it is taken", func(t *testing.T) {
		ctx := t.Context()
		st := storetest.Open(t)
		dispatch := projectaccesstest.NewServer(t, nil, map[string]string{ada: projectaccess.MemberFull, bob: projectaccess.MemberFull})
		dispatch.SetHeld(ada, true)
		r := newRig(dispatch.URL, dispatch.Account, st)
		refuseWrites(t, st)
		err := r.gate.Check(ctx, ada)
		if !errors.Is(err, hold.ErrUncheckable) || !errors.Is(err, hold.ErrHeld) {
			t.Fatalf("Check(%s) held, with person_standing refusing writes = %v, want hold.ErrUncheckable and hold.ErrHeld", ada, err)
		}
		err = r.gate.Check(ctx, ada)
		if !errors.Is(err, hold.ErrUncheckable) || !errors.Is(err, hold.ErrHeld) {
			t.Fatalf("Check(%s) again from the cached answer, the write still failing = %v, want hold.ErrUncheckable and hold.ErrHeld", ada, err)
		}
		for range 2 {
			if err := r.gate.Check(ctx, bob); err != nil {
				t.Fatalf("Check(%s) not held, with person_standing refusing writes = %v, want nil", bob, err)
			}
		}
		if warned := r.logs.at(slog.LevelWarn, "", bob); len(warned) != 1 || warned[0].attrs["error"] == "" {
			t.Fatalf("two checks of the one not-held answer for %s that could not be written logged %+v, want one WARN line with the error: that answer is tried once", bob, warned)
		}

		dispatch.SetDown(true)
		r.clock.advance(projectaccess.CacheTTL)
		err = r.gate.Check(ctx, ada)
		if !errors.Is(err, hold.ErrHeld) || !errors.Is(err, hold.ErrUncheckable) {
			t.Fatalf("Check(%s) with Dispatch stopped and the write still failing = %v, want her held from the gate's memory, still unwritten", ada, err)
		}
		if _, _, found := standing(t, st, ada); found {
			t.Fatalf("person_standing holds a row for %s while it refuses writes", ada)
		}

		allowWrites(t, st)
		dispatch.SetDown(false)
		r.clock.advance(projectaccess.CacheTTL)
		if err := r.gate.Check(ctx, ada); !isHeld(err) {
			t.Fatalf("Check(%s) once the store takes writes and Dispatch answers = %v, want hold.ErrHeld alone", ada, err)
		}
		if held, _, found := standing(t, st, ada); !found || !held {
			t.Fatalf("person_standing for %s once the store takes writes = held %t (found %t), want held", ada, held, found)
		}
	})

	t.Run("a caller that hangs up does not stop the write", func(t *testing.T) {
		ctx := t.Context()
		st := storetest.Open(t)
		dispatch := projectaccesstest.NewServer(t, nil, map[string]string{ada: projectaccess.MemberFull})
		dispatch.SetHeld(ada, true)
		r := newRig(dispatch.URL, dispatch.Account, st)
		blocker, err := st.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = blocker.Rollback(context.Background()) })
		if _, err := blocker.Exec(ctx, `lock table person_standing in share mode`); err != nil {
			t.Fatal(err)
		}
		caller, hangUp := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- r.gate.Check(caller, ada) }()
		storetest.AwaitLockWaiters(t, st.Pool, blocker, 1)
		hangUp()
		// The sleep decides nothing this test asserts; it only feeds its control. A write bound to
		// the caller would be cancelled by pgx asynchronously, and nothing observable marks that
		// cancellation reaching Postgres, so the sleep gives such a write time to end before the
		// lock is let go: without it, that write could still win the lock and the control pass.
		time.Sleep(100 * time.Millisecond)
		if err := blocker.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-done; !isHeld(err) {
			t.Fatalf("Check(%s) whose caller hung up while the write waited = %v, want hold.ErrHeld alone: the write goes on", ada, err)
		}
		if held, _, found := standing(t, st, ada); !found || !held {
			t.Fatalf("person_standing for %s after its caller hung up mid-write = held %t (found %t), want held", ada, held, found)
		}
	})
}

// TestHoldGateRemembersNoFailureOfACallerThatHungUp: an ask that ends because its own caller hung
// up is no failure of Dispatch's, so the next caller asks Dispatch rather than answer from the
// kept row for five seconds.
func TestHoldGateRemembersNoFailureOfACallerThatHungUp(t *testing.T) {
	st := storetest.Open(t)
	dispatch := projectaccesstest.NewServer(t, nil, map[string]string{ada: projectaccess.MemberFull})
	dispatch.SetHeld(ada, true)
	r := newRig(dispatch.URL, dispatch.Account, st)
	gone, hangUp := context.WithCancel(t.Context())
	hangUp()
	_ = r.gate.Check(gone, ada)
	if err := r.gate.Check(t.Context(), ada); !isHeld(err) {
		t.Fatalf("Check(%s) right after a caller that had hung up = %v, want hold.ErrHeld from Dispatch's answer", ada, err)
	}
}

// TestHoldGateChecksTheCanonicalLogin: the gate asks about and keeps a person as
// record.CanonicalLogin spells them, so " Ada@Example.com " is ada; an empty person is an error;
// and a nil gate, a broker with no Dispatch to ask, holds nobody.
func TestHoldGateChecksTheCanonicalLogin(t *testing.T) {
	ctx := t.Context()
	st := storetest.Open(t)
	dispatch := projectaccesstest.NewServer(t, nil, map[string]string{ada: projectaccess.MemberFull})
	dispatch.SetHeld(ada, true)
	r := newRig(dispatch.URL, dispatch.Account, st)
	if err := r.gate.Check(ctx, " Ada@Example.com "); !isHeld(err) || !strings.Contains(err.Error(), ada) {
		t.Fatalf("Check(%q) = %v, want hold.ErrHeld naming %s", " Ada@Example.com ", err, ada)
	}
	if held, _, found := standing(t, st, ada); !found || !held {
		t.Fatalf("person_standing for %s after checking %q = held %t (found %t), want held", ada, " Ada@Example.com ", held, found)
	}
	if err := r.gate.Check(ctx, "  "); err == nil || errors.Is(err, hold.ErrHeld) {
		t.Fatalf("Check of an empty person = %v, want an error that is not a hold", err)
	}
	var none *hold.Gate
	if err := none.Check(ctx, ada); err != nil {
		t.Fatalf("a nil gate's Check(%s) = %v, want nil", ada, err)
	}
}
