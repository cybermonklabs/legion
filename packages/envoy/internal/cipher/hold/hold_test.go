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
// seconds, and the failure is remembered five seconds: a check in that time asks nothing and
// answers at once.
func TestHoldGateBoundsEachAsk(t *testing.T) {
	ctx := t.Context()
	url, calls := never(t)
	r := newRig(url, projectaccesstest.StaticAccount("token"), storetest.Open(t))
	start := time.Now()
	if err := r.gate.Check(ctx, ada); err != nil {
		t.Fatalf("Check(%s) against a Dispatch that never answers, with no answer kept = %v, want nil, served unchecked", ada, err)
	}
	if took := time.Since(start); took > 3500*time.Millisecond {
		t.Fatalf("Check(%s) against a Dispatch that never answers took %s, want at most 3.5 s", ada, took)
	}
	r.clock.advance(5*time.Second - time.Millisecond)
	start = time.Now()
	if err := r.gate.Check(ctx, ada); err != nil {
		t.Fatalf("Check(%s) again within five seconds = %v, want nil", ada, err)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("Check(%s) within five seconds of a failure to ask took %s, want it answered at once from the remembered failure", ada, took)
	}
	if got := calls(); got != 1 {
		t.Fatalf("two checks within five seconds asked Dispatch %d times, want 1", got)
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
