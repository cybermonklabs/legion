package projectaccess_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
)

// personServer answers Dispatch's person route with what answer gives for the person asked about
// and the number of the call for that person, and counts every call.
func personServer(t *testing.T, answer func(person string, call int) (int, string)) (*httptest.Server, func() int) {
	t.Helper()
	var mu sync.Mutex
	calls := map[string]int{}
	total := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/access/person" {
			t.Errorf("the client asked %s, want only the person route", r.URL.Path)
		}
		person := r.URL.Query().Get("person")
		mu.Lock()
		calls[person]++
		total++
		call := calls[person]
		mu.Unlock()
		status, body := answer(person, call)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server, func() int { mu.Lock(); defer mu.Unlock(); return total }
}

// TestPersonAnswersHeldAndAsOf: Person reads the person route's held and as_of beside its member
// kind, asks about the canonical login, and stamps the answer with the moment it asked. A member
// kind the broker does not know is still an answer here, since its held decodes. The stand-in
// answers held as its test sets it, and an as_of that only moves forward.
func TestPersonAnswersHeldAndAsOf(t *testing.T) {
	answers := map[string]string{
		"ada@example.com": `{"person":"ada@example.com","member_kind":"full","held":true,"as_of":"2026-11-16T00:00:05.25Z"}`,
		"bob@example.com": `{"person":"bob@example.com","member_kind":"restricted","held":false,"as_of":"2026-11-16T00:00:06Z"}`,
		"gus@example.com": `{"person":"gus@example.com","member_kind":"guest","held":true,"as_of":"2026-11-16T00:00:07Z"}`,
	}
	server, _ := personServer(t, func(person string, _ int) (int, string) {
		if answers[person] == "" {
			t.Errorf("the client asked about %q, want a canonical login the test serves", person)
		}
		return http.StatusOK, answers[person]
	})
	client := projectaccess.New(server.URL, projectaccesstest.StaticAccount("token"), nil)
	clock := &fakeClock{now: time.Date(2026, 11, 16, 0, 0, 10, 0, time.UTC)}
	client.UseClock(clock.read)
	for _, c := range []struct {
		ask  string
		want projectaccess.PersonAnswer
	}{
		{" Ada@Example.com ", projectaccess.PersonAnswer{MemberKind: "full", Held: true, AsOf: time.Date(2026, 11, 16, 0, 0, 5, 250_000_000, time.UTC)}},
		{"bob@example.com", projectaccess.PersonAnswer{MemberKind: "restricted", Held: false, AsOf: time.Date(2026, 11, 16, 0, 0, 6, 0, time.UTC)}},
		{"gus@example.com", projectaccess.PersonAnswer{MemberKind: "guest", Held: true, AsOf: time.Date(2026, 11, 16, 0, 0, 7, 0, time.UTC)}},
	} {
		got, err := client.Person(t.Context(), c.ask)
		if err != nil {
			t.Fatalf("Person(%q) = %v, want the answer %+v", c.ask, err, c.want)
		}
		if got.MemberKind != c.want.MemberKind || got.Held != c.want.Held || !got.AsOf.Equal(c.want.AsOf) || !got.AskedAt.Equal(clock.read()) {
			t.Fatalf("Person(%q) = %+v, want %+v asked at %v", c.ask, got, c.want, clock.read())
		}
		clock.advance(time.Second)
	}

	standIn := projectaccesstest.NewServer(t, nil, map[string]string{"ada@example.com": projectaccess.MemberFull})
	standIn.SetHeld("ada@example.com", true)
	answered := projectaccess.New(standIn.URL, standIn.Account, nil)
	before := time.Now()
	ada, err := answered.Person(t.Context(), "ada@example.com")
	if err != nil || !ada.Held || ada.MemberKind != projectaccess.MemberFull {
		t.Fatalf("Person(ada) at the stand-in = %+v, %v; want held, a full member", ada, err)
	}
	bob, err := answered.Person(t.Context(), "bob@example.com")
	if err != nil || bob.Held || bob.MemberKind != projectaccess.MemberNone {
		t.Fatalf("Person(bob) at the stand-in = %+v, %v; want not held, no member", bob, err)
	}
	if ada.AsOf.Before(before.Truncate(time.Microsecond)) || !bob.AsOf.After(ada.AsOf) || bob.AsOf.After(time.Now()) {
		t.Fatalf("the stand-in answered ada as of %v and bob, asked after her, as of %v; want times from the asks, the later one later", ada.AsOf, bob.AsOf)
	}
}

// TestPersonAnswersNotHeldUntil: Person reads the person route's not_held_until, the deadline an
// IAP deployment's Dispatch puts on every answer that is not held, and passes it on as Dispatch
// spelled it; an answer naming none, as every answer outside IAP and every held one, answers the
// zero time. Every read of one cached answer carries the same deadline, never one moved from the
// moment it was read, and the stand-in answers the deadline its test sets on a not-held answer
// alone, as Dispatch does.
//
// Control: askPerson decoding no not_held_until; ada's answer reads back with none, and the test
// fails naming her.
func TestPersonAnswersNotHeldUntil(t *testing.T) {
	adaUntil := time.Date(2026, 11, 17, 0, 0, 5, 250_000_000, time.UTC)
	answers := map[string]string{
		"ada@example.com": `{"person":"ada@example.com","member_kind":"full","held":false,"as_of":"2026-11-16T00:00:05.25Z","not_held_until":"2026-11-17T00:00:05.25Z"}`,
		"bob@example.com": `{"person":"bob@example.com","member_kind":"full","held":false,"as_of":"2026-11-16T00:00:06Z"}`,
		"cy@example.com":  `{"person":"cy@example.com","member_kind":"full","held":true,"as_of":"2026-11-16T00:00:07Z"}`,
	}
	server, calls := personServer(t, func(person string, _ int) (int, string) { return http.StatusOK, answers[person] })
	client := projectaccess.New(server.URL, projectaccesstest.StaticAccount("token"), nil)
	clock := &fakeClock{now: time.Date(2026, 11, 16, 0, 0, 10, 0, time.UTC)}
	client.UseClock(clock.read)
	for _, c := range []struct {
		person string
		until  time.Time
	}{{"ada@example.com", adaUntil}, {"bob@example.com", time.Time{}}, {"cy@example.com", time.Time{}}} {
		got, err := client.Person(t.Context(), c.person)
		if err != nil || !got.NotHeldUntil.Equal(c.until) {
			t.Fatalf("Person(%s) = %+v, %v; want not_held_until %v", c.person, got, err, c.until)
		}
	}
	clock.advance(projectaccess.CacheTTL - time.Second)
	if got, err := client.Person(t.Context(), "ada@example.com"); err != nil || !got.NotHeldUntil.Equal(adaUntil) || calls() != 3 {
		t.Fatalf("Person(ada) from the cache = %+v, %v after %d calls; want the deadline Dispatch answered, %v, and no new call", got, err, calls(), adaUntil)
	}

	standIn := projectaccesstest.NewServer(t, nil, map[string]string{"ada@example.com": projectaccess.MemberFull})
	standIn.SetNotHeldUntil("ada@example.com", adaUntil)
	answered := projectaccess.New(standIn.URL, standIn.Account, nil)
	if got, err := answered.Person(t.Context(), "ada@example.com"); err != nil || got.Held || !got.NotHeldUntil.Equal(adaUntil) {
		t.Fatalf("Person(ada) at the stand-in = %+v, %v; want not held until %v", got, err, adaUntil)
	}
	standIn.SetHeld("dee@example.com", true)
	standIn.SetNotHeldUntil("dee@example.com", adaUntil)
	if got, err := answered.Person(t.Context(), "dee@example.com"); err != nil || !got.Held || !got.NotHeldUntil.IsZero() {
		t.Fatalf("Person(dee) at the stand-in, held with a deadline set = %+v, %v; want held and no deadline, as Dispatch answers a held person", got, err)
	}
}

// TestPersonAnswersErrNoHeldFieldAndAsksAgainAfterFiveSeconds: an answer with no held, from a
// Dispatch older than the hold, is ErrNoHeldField and never read as not held. Person answers it
// from the cache for five seconds, and asks Dispatch again once five seconds have passed since it
// asked, though the entry is kept thirty for MemberKind.
func TestPersonAnswersErrNoHeldFieldAndAsksAgainAfterFiveSeconds(t *testing.T) {
	server, calls := personServer(t, func(person string, _ int) (int, string) {
		return http.StatusOK, `{"person":"` + person + `","member_kind":"full","as_of":"2026-11-16T00:00:05Z"}`
	})
	client := projectaccess.New(server.URL, projectaccesstest.StaticAccount("token"), nil)
	clock := &fakeClock{now: time.Date(2026, 11, 16, 0, 0, 10, 0, time.UTC)}
	client.UseClock(clock.read)
	ask := func(wantCalls int, when string) {
		t.Helper()
		answer, err := client.Person(t.Context(), "ada@example.com")
		if !errors.Is(err, projectaccess.ErrNoHeldField) {
			t.Fatalf("Person(ada) %s = %+v, %v; want ErrNoHeldField", when, answer, err)
		}
		if got := calls(); got != wantCalls {
			t.Fatalf("Person(ada) %s asked Dispatch %d times in all, want %d", when, got, wantCalls)
		}
	}
	ask(1, "first")
	clock.advance(5*time.Second - time.Millisecond)
	ask(1, "just under five seconds later")
	clock.advance(time.Millisecond)
	ask(2, "five seconds later")
}

// TestMemberKindReadsAnAnswerWithNoHeld: MemberKind reads member_kind from the one cached person
// answer whether or not it has held, so an older Dispatch still answers it, and keeps it thirty
// seconds; Person reads the same entry.
func TestMemberKindReadsAnAnswerWithNoHeld(t *testing.T) {
	server, calls := personServer(t, func(person string, _ int) (int, string) {
		return http.StatusOK, `{"person":"` + person + `","member_kind":"restricted"}`
	})
	client := projectaccess.New(server.URL, projectaccesstest.StaticAccount("token"), nil)
	clock := &fakeClock{now: time.Date(2026, 11, 16, 0, 0, 10, 0, time.UTC)}
	client.UseClock(clock.read)
	kind := func(wantCalls int, when string) {
		t.Helper()
		got, err := client.MemberKind(t.Context(), "ada@example.com")
		if err != nil || got != projectaccess.MemberRestricted {
			t.Fatalf("MemberKind(ada) %s = %q, %v; want restricted", when, got, err)
		}
		if n := calls(); n != wantCalls {
			t.Fatalf("MemberKind(ada) %s asked Dispatch %d times in all, want %d", when, n, wantCalls)
		}
	}
	kind(1, "first")
	if _, err := client.Person(t.Context(), "ada@example.com"); !errors.Is(err, projectaccess.ErrNoHeldField) || calls() != 1 {
		t.Fatalf("Person(ada) right after MemberKind = %v with %d calls in all, want ErrNoHeldField from the same answer, 1 call", err, calls())
	}
	clock.advance(projectaccess.CacheTTL - time.Second)
	kind(1, "just under thirty seconds later")
	clock.advance(time.Second)
	kind(2, "thirty seconds later")
}

// TestConcurrentAsksForOnePersonShareOneCall: Person and MemberKind asked at once about one person
// make one call to Dispatch, and every caller gets its answer.
func TestConcurrentAsksForOnePersonShareOneCall(t *testing.T) {
	entered, release := make(chan struct{}, 16), make(chan struct{})
	var releaseOnce sync.Once
	free := func() { releaseOnce.Do(func() { close(release) }) }
	server, calls := personServer(t, func(person string, _ int) (int, string) {
		entered <- struct{}{}
		<-release
		return http.StatusOK, `{"person":"` + person + `","member_kind":"full","held":false,"as_of":"2026-11-16T00:00:05Z"}`
	})
	t.Cleanup(free)
	client := projectaccess.New(server.URL, projectaccesstest.StaticAccount("token"), nil)
	errs := make(chan error, 8)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			if i%2 == 0 {
				answer, err := client.Person(context.Background(), "ada@example.com")
				if err == nil && (answer.Held || answer.MemberKind != projectaccess.MemberFull) {
					err = errors.New("Person answered " + answer.MemberKind)
				}
				errs <- err
				return
			}
			kind, err := client.MemberKind(context.Background(), "ada@example.com")
			if err == nil && kind != projectaccess.MemberFull {
				err = errors.New("MemberKind answered " + kind)
			}
			errs <- err
		})
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("no ask reached Dispatch")
	}
	// Every caller has had time to reach Dispatch on its own, which only a shared call stops.
	time.Sleep(200 * time.Millisecond)
	free()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("a concurrent caller got %v, want ada's answer: a full member, not held", err)
		}
	}
	if got := calls(); got != 1 {
		t.Fatalf("eight concurrent asks about ada made %d calls to Dispatch, want 1", got)
	}
}

// TestACallerThatStopsWaitingLeavesTheSharedAskRunning: the call a person's askers share is not
// the first caller's: that caller stops waiting when its own context ends, and a caller that
// joined the same ask still gets Dispatch's answer from that one call.
func TestACallerThatStopsWaitingLeavesTheSharedAskRunning(t *testing.T) {
	entered, release := make(chan struct{}, 16), make(chan struct{})
	var releaseOnce sync.Once
	free := func() { releaseOnce.Do(func() { close(release) }) }
	server, calls := personServer(t, func(person string, _ int) (int, string) {
		entered <- struct{}{}
		<-release
		return http.StatusOK, `{"person":"` + person + `","member_kind":"full","held":true,"as_of":"2026-11-16T00:00:05Z"}`
	})
	t.Cleanup(free)
	client := projectaccess.New(server.URL, projectaccesstest.StaticAccount("token"), nil)
	first, stop := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() {
		_, err := client.Person(first, "ada@example.com")
		firstDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the first ask never reached Dispatch")
	}
	stop()
	select {
	case err := <-firstDone:
		if err == nil {
			t.Fatal("the first caller, whose context ended while Dispatch had not answered, got an answer, want its context's end")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the first caller went on waiting after its context ended")
	}
	second := make(chan error, 1)
	go func() {
		answer, err := client.Person(context.Background(), "ada@example.com")
		if err == nil && !answer.Held {
			err = errors.New("Person answered ada not held")
		}
		second <- err
	}()
	time.Sleep(200 * time.Millisecond)
	free()
	if err := <-second; err != nil {
		t.Fatalf("the second caller = %v, want ada's answer, held", err)
	}
	if got := calls(); got != 1 {
		t.Fatalf("the two callers made %d calls to Dispatch, want 1: the first caller's end does not end the ask", got)
	}
}
