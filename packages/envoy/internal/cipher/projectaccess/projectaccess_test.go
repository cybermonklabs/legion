package projectaccess_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
)

// The rows every test here serves: PRIV is private, owned by carol and listing bob; PUB is
// public. bob is a restricted member, carol a full one, and alice has no row at all.
func standIn(t *testing.T) *projectaccesstest.Server {
	t.Helper()
	return projectaccesstest.NewServer(t, map[string]projectaccesstest.Row{
		"PRIV": {Visibility: "private", Owner: "carol@example.com", Listed: []string{"bob@example.com"}},
		"PUB":  {Visibility: "public", Owner: "carol@example.com"},
	}, map[string]string{"bob@example.com": projectaccess.MemberRestricted, "carol@example.com": projectaccess.MemberFull})
}

func clientOf(server *projectaccesstest.Server) *projectaccess.Client {
	return projectaccess.New(server.URL, server.Account, nil)
}

// TestAccessReadsTheK3Answer: every field of Dispatch's answer reaches Answer, for a person the
// project's list names and for one it does not.
func TestAccessReadsTheK3Answer(t *testing.T) {
	client := clientOf(standIn(t))
	before := time.Now()
	got, err := client.Access(t.Context(), "PRIV", "carol@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got.AskedAt.Before(before) || got.AskedAt.After(time.Now()) {
		t.Fatalf("Access(PRIV, carol).AskedAt = %v, want the moment the question was asked", got.AskedAt)
	}
	got.AskedAt = time.Time{}
	want := projectaccess.Answer{
		Project: "PRIV", Visibility: "private", Owner: "carol@example.com", Person: "carol@example.com",
		MemberKind: projectaccess.MemberFull, Visible: true,
	}
	if !reflect.DeepEqual(got, want) || !got.Private() {
		t.Fatalf("Access(PRIV, carol) = %+v (private %v), want %+v, private", got, got.Private(), want)
	}
	got, err = client.Access(t.Context(), "PRIV", "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got.Visible || got.MemberKind != projectaccess.MemberNone || got.Person != "alice@example.com" {
		t.Fatalf("Access(PRIV, alice) = %+v, want not visible to a person with no row, member kind none", got)
	}
	got, err = client.Access(t.Context(), "PUB", "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Visible || got.Private() {
		t.Fatalf("Access(PUB, alice) = %+v, want a public project alice sees", got)
	}
}

// TestAccessAnswersNotFound: a project Dispatch does not hold is ErrProjectNotFound, which no
// transport failure is.
func TestAccessAnswersNotFound(t *testing.T) {
	server := standIn(t)
	client := clientOf(server)
	if _, err := client.Access(t.Context(), "NOPE", "carol@example.com"); !errors.Is(err, projectaccess.ErrProjectNotFound) {
		t.Fatalf("Access(NOPE) = %v, want ErrProjectNotFound", err)
	}
	server.SetDown(true)
	if _, err := client.Access(t.Context(), "GONE", "carol@example.com"); err == nil || errors.Is(err, projectaccess.ErrProjectNotFound) {
		t.Fatalf("Access while Dispatch is down = %v, want a failure that is not ErrProjectNotFound", err)
	}
}

// TestAccessCachesThirtySeconds: an answer, a not-found and a member kind are each asked once per
// CacheTTL, and asked again once it has passed; a failure is not remembered.
func TestAccessCachesThirtySeconds(t *testing.T) {
	server := standIn(t)
	client := clientOf(server)
	now := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	projectaccess.SetClock(client, func() time.Time { return now })
	ask := func() {
		t.Helper()
		if _, err := client.Access(t.Context(), "PRIV", "carol@example.com"); err != nil {
			t.Fatal(err)
		}
		if _, err := client.Access(t.Context(), "NOPE", "carol@example.com"); !errors.Is(err, projectaccess.ErrProjectNotFound) {
			t.Fatalf("Access(NOPE) = %v, want ErrProjectNotFound", err)
		}
		if kind, err := client.MemberKind(t.Context(), "bob@example.com"); err != nil || kind != projectaccess.MemberRestricted {
			t.Fatalf("MemberKind(bob) = %q, %v, want restricted", kind, err)
		}
	}
	ask()
	if calls := server.Calls(); calls != 3 {
		t.Fatalf("the first round asked Dispatch %d times, want 3", calls)
	}
	now = now.Add(projectaccess.CacheTTL - time.Second)
	ask()
	if calls := server.Calls(); calls != 3 {
		t.Fatalf("a round %s later asked Dispatch %d times in all, want still 3: an answer is kept for %s", projectaccess.CacheTTL-time.Second, calls, projectaccess.CacheTTL)
	}
	now = now.Add(time.Second)
	ask()
	if calls := server.Calls(); calls != 6 {
		t.Fatalf("a round %s after the first asked Dispatch %d times in all, want 6: an answer is kept no longer than %s", projectaccess.CacheTTL, calls, projectaccess.CacheTTL)
	}
	server.SetDown(true)
	now = now.Add(projectaccess.CacheTTL)
	if _, err := client.Access(t.Context(), "PRIV", "carol@example.com"); err == nil {
		t.Fatal("Access while Dispatch is down answered, want a failure")
	}
	server.SetDown(false)
	if _, err := client.Access(t.Context(), "PRIV", "carol@example.com"); err != nil {
		t.Fatalf("Access once Dispatch is back = %v, want the answer: a failure is not remembered", err)
	}
}

// TestAccessSendsTheAccountToken: every call presents the broker's own account token and puts the
// person in the query, never in the path; any other bearer is refused, and that refusal is no
// answer.
func TestAccessSendsTheAccountToken(t *testing.T) {
	type seen struct{ path, person, bearer string }
	var requests []seen
	recorder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, seen{r.URL.EscapedPath(), r.URL.Query().Get("person"), r.Header.Get("Authorization")})
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/api/v1/access/person") {
			_, _ = w.Write([]byte(`{"person":"carol@example.com","member_kind":"full"}`))
			return
		}
		_, _ = w.Write([]byte(`{"project":"PRIV","visibility":"private","owner":"carol@example.com","person":"carol@example.com","member_kind":"full","visible":true}`))
	}))
	t.Cleanup(recorder.Close)
	client := projectaccess.New(recorder.URL+"/", projectaccesstest.StaticAccount("the-broker-account-token-not-a-secret"), nil)
	if _, err := client.Access(t.Context(), "PRIV", "Carol@Example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.MemberKind(t.Context(), "carol@example.com"); err != nil {
		t.Fatal(err)
	}
	want := []seen{
		{"/api/v1/access/projects/PRIV", "carol@example.com", "Bearer the-broker-account-token-not-a-secret"},
		{"/api/v1/access/person", "carol@example.com", "Bearer the-broker-account-token-not-a-secret"},
	}
	if !reflect.DeepEqual(requests, want) {
		t.Fatalf("the client asked %+v, want %+v: the base URL's trailing slash dropped, the person lowercased in the query and the account token as the bearer", requests, want)
	}

	server := standIn(t)
	stranger := projectaccess.New(server.URL, projectaccesstest.StaticAccount("not-the-stand-ins-token"), nil)
	if _, err := stranger.Access(t.Context(), "PRIV", "carol@example.com"); err == nil || errors.Is(err, projectaccess.ErrProjectNotFound) {
		t.Fatalf("Access with a bearer Dispatch refuses = %v, want a failure that is not an answer", err)
	}
}

// TestAccessWithNoPersonReadsVisibilityAlone: a question naming no person is whether the project
// is public.
func TestAccessWithNoPersonReadsVisibilityAlone(t *testing.T) {
	client := clientOf(standIn(t))
	for project, visible := range map[string]bool{"PUB": true, "PRIV": false} {
		got, err := client.Access(t.Context(), project, "")
		if err != nil {
			t.Fatal(err)
		}
		if got.Visible != visible || got.Person != "" {
			t.Errorf("Access(%s, no person) = %+v, want visible %v naming no person", project, got, visible)
		}
	}
}

// TestVisibleNamesTheFirstProjectNotSeen: Visible refuses naming the first project of the list
// its person may not see, and a project Dispatch does not hold is refused in the same words.
func TestVisibleNamesTheFirstProjectNotSeen(t *testing.T) {
	client := clientOf(standIn(t))
	if err := projectaccess.Visible(t.Context(), client, "carol@example.com", []string{"PRIV", "PUB"}); err != nil {
		t.Fatalf("Visible(carol, PRIV PUB) = %v, want nil", err)
	}
	if err := projectaccess.Visible(t.Context(), client, "bob@example.com", nil); err != nil {
		t.Fatalf("Visible of no project = %v, want nil", err)
	}
	unseen := projectaccess.Visible(t.Context(), client, "alice@example.com", []string{"PRIV", "PUB", "ZZZ"})
	if !errors.Is(unseen, projectaccess.ErrNotVisible) || !strings.Contains(unseen.Error(), "PRIV") || strings.Contains(unseen.Error(), "ZZZ") {
		t.Fatalf("Visible(alice, PRIV PUB ZZZ) = %v, want ErrNotVisible naming PRIV alone", unseen)
	}
	missing := projectaccess.Visible(t.Context(), client, "alice@example.com", []string{"NOPE"})
	if !errors.Is(missing, projectaccess.ErrNotVisible) || errors.Is(missing, projectaccess.ErrProjectNotFound) ||
		missing.Error() != strings.ReplaceAll(unseen.Error(), "PRIV", "NOPE") {
		t.Fatalf("Visible(alice, NOPE) = %v, want the very refusal %q gives with NOPE in its place, so it never says whether a project exists", missing, unseen)
	}
	var named *projectaccess.NotVisibleError
	if !errors.As(unseen, &named) || named.Project != "PRIV" {
		t.Fatalf("Visible(alice, PRIV PUB ZZZ) = %#v, want a *NotVisibleError naming PRIV", unseen)
	}
}

// recordingAccess answers member kinds from a map and records every question, for the rule that a
// non-empty list never asks one.
type recordingAccess struct {
	kinds   map[string]string
	down    bool
	asked   []string
	visible bool
}

func (r *recordingAccess) Access(_ context.Context, project, person string) (projectaccess.Answer, error) {
	r.asked = append(r.asked, "access "+project+" "+person)
	return projectaccess.Answer{Project: project, Person: person, Visible: r.visible}, nil
}

func (r *recordingAccess) MemberKind(_ context.Context, person string) (string, error) {
	r.asked = append(r.asked, "kind "+person)
	if r.down {
		return "", errors.New("dispatch could not be reached")
	}
	kind, held := r.kinds[person]
	if !held {
		return projectaccess.MemberNone, nil
	}
	return kind, nil
}

func (r *recordingAccess) States(context.Context, []string) (map[string]projectaccess.Answer, error) {
	return nil, errors.New("not asked here")
}

// TestListAllowedRefusesAnEmptyListOnlyForARestrictedMember: an empty list is refused for a
// restricted member alone; a full member and a person Dispatch holds no row for pass; an
// unreachable Dispatch answers ErrUnchecked, which a caller minting as today tells from a refusal
// and logs; Dispatch refusing the broker's own token answers ErrBrokerRefused, which no caller
// mints through; and a non-empty list is Visible's question and never asks a member kind.
func TestListAllowedRefusesAnEmptyListOnlyForARestrictedMember(t *testing.T) {
	client := clientOf(standIn(t))
	if err := projectaccess.ListAllowed(t.Context(), client, "bob@example.com", nil); !errors.Is(err, projectaccess.ErrListRequired) || !strings.Contains(err.Error(), "bob@example.com") {
		t.Fatalf("ListAllowed(bob, none) = %v, want ErrListRequired naming bob", err)
	}
	for _, person := range []string{"carol@example.com", "alice@example.com"} {
		if err := projectaccess.ListAllowed(t.Context(), client, person, nil); err != nil {
			t.Errorf("ListAllowed(%s, none) = %v, want nil", person, err)
		}
	}
	down := standIn(t)
	down.SetDown(true)
	if err := projectaccess.ListAllowed(t.Context(), clientOf(down), "bob@example.com", nil); !errors.Is(err, projectaccess.ErrUnchecked) || errors.Is(err, projectaccess.ErrListRequired) {
		t.Fatalf("ListAllowed(bob, none) while Dispatch is down = %v, want ErrUnchecked: an outage mints as today, and the caller logs it", err)
	}
	stranger := projectaccess.New(standIn(t).URL, projectaccesstest.StaticAccount("not-the-stand-ins-token"), nil)
	if err := projectaccess.ListAllowed(t.Context(), stranger, "bob@example.com", nil); !errors.Is(err, projectaccess.ErrBrokerRefused) || errors.Is(err, projectaccess.ErrUnchecked) {
		t.Fatalf("ListAllowed(bob, none) with a bearer Dispatch refuses = %v, want ErrBrokerRefused and not ErrUnchecked", err)
	}
	if err := projectaccess.ListAllowed(t.Context(), client, "bob@example.com", []string{"PRIV"}); err != nil {
		t.Fatalf("ListAllowed(bob, PRIV) = %v, want nil: PRIV lists bob", err)
	}
	if err := projectaccess.ListAllowed(t.Context(), client, "bob@example.com", []string{"PUB"}); !errors.Is(err, projectaccess.ErrNotVisible) {
		t.Fatalf("ListAllowed(bob, PUB) = %v, want ErrNotVisible: a restricted member sees no project that does not list them", err)
	}
	recording := &recordingAccess{kinds: map[string]string{"bob@example.com": projectaccess.MemberRestricted}, visible: true}
	if err := projectaccess.ListAllowed(t.Context(), recording, "bob@example.com", []string{"PRIV"}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"access PRIV bob@example.com"}; !reflect.DeepEqual(recording.asked, want) {
		t.Fatalf("ListAllowed of a non-empty list asked %v, want %v and no member kind", recording.asked, want)
	}
}

// TestStatesAnswersOneEntryPerProjectItHolds: States answers each project Dispatch holds, as it
// is to a caller naming no person, and leaves out the one it does not.
func TestStatesAnswersOneEntryPerProjectItHolds(t *testing.T) {
	client := clientOf(standIn(t))
	states, err := client.States(t.Context(), []string{"PRIV", "PUB", "NOPE"})
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 2 || !states["PRIV"].Private() || states["PUB"].Private() || states["PUB"].Visible != true {
		t.Fatalf("States(PRIV PUB NOPE) = %+v, want PRIV private and PUB public, and no entry for NOPE", states)
	}
	if _, held := states["NOPE"]; held {
		t.Fatalf("States answered an entry for NOPE, which Dispatch does not hold: %+v", states["NOPE"])
	}
	down := standIn(t)
	down.SetDown(true)
	if _, err := clientOf(down).States(t.Context(), []string{"PRIV"}); err == nil {
		t.Fatal("States while Dispatch is down answered, want a failure")
	}
}

// TestAccessRefusesAnOversizedAnswer: an answer past 64 KiB is not Dispatch's and is refused
// rather than read.
func TestAccessRefusesAnOversizedAnswer(t *testing.T) {
	huge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"project":"PRIV","owner":"` + strings.Repeat("x", 64<<10) + `"}`))
	}))
	t.Cleanup(huge.Close)
	client := projectaccess.New(huge.URL, projectaccesstest.StaticAccount("token"), nil)
	_, err := client.Access(t.Context(), "PRIV", "carol@example.com")
	if err == nil || !strings.Contains(err.Error(), "is over 65536 bytes") {
		t.Fatalf("Access of an answer past 64 KiB = %v, want it refused as over 65536 bytes, not read or failed some other way", err)
	}
}

// fakeClock is a clock a test and the handlers it serves move together.
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

// projectServer answers every project route visible, after running during (which may move the
// clock or block), and counts the questions per project.
func projectServer(t *testing.T, during func(project string, call int) bool) (*httptest.Server, func(string) int) {
	t.Helper()
	var mu sync.Mutex
	calls := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		project := strings.TrimPrefix(r.URL.Path, "/api/v1/access/projects/")
		mu.Lock()
		calls[project]++
		call := calls[project]
		mu.Unlock()
		visible := during(project, call)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"project":%q,"visibility":"private","person":"carol@example.com","member_kind":"full","visible":%t}`, project, visible)
	}))
	t.Cleanup(server.Close)
	return server, func(project string) int { mu.Lock(); defer mu.Unlock(); return calls[project] }
}

// TestAnAnswerIsAgedFromBeforeItsQuestion: a slow answer's age runs from before the question was
// asked, so an answer that took 20 s to arrive is kept 10 s more and no longer.
func TestAnAnswerIsAgedFromBeforeItsQuestion(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)}
	server, calls := projectServer(t, func(_ string, call int) bool {
		if call == 1 {
			clock.advance(20 * time.Second)
		}
		return true
	})
	client := projectaccess.New(server.URL, projectaccesstest.StaticAccount("token"), nil)
	projectaccess.SetClock(client, clock.read)
	ask := func() {
		t.Helper()
		if _, err := client.Access(t.Context(), "PRIV", "carol@example.com"); err != nil {
			t.Fatal(err)
		}
	}
	ask()
	clock.advance(projectaccess.CacheTTL - 20*time.Second - time.Second)
	ask()
	if got := calls("PRIV"); got != 1 {
		t.Fatalf("a question %s after the first was asked was answered by Dispatch %d times, want 1: the answer is still young", projectaccess.CacheTTL-time.Second, got)
	}
	clock.advance(time.Second)
	ask()
	if got := calls("PRIV"); got != 2 {
		t.Fatalf("a question %s after the first was asked was answered by Dispatch %d times in all, want 2: an answer is aged from before its question, so a slow one is kept no longer than %s", projectaccess.CacheTTL, got, projectaccess.CacheTTL)
	}
}

// TestVisibleAsksAgainForAnAnswerThatAgedDuringTheCheck: a list is checked one project at a time,
// so an answer that was young when the check began can be older than CacheTTL by its end; Visible
// asks again for each such answer, so every answer it passes is younger than CacheTTL when it
// returns.
func TestVisibleAsksAgainForAnAnswerThatAgedDuringTheCheck(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)}
	server, calls := projectServer(t, func(project string, _ int) bool {
		if project == "SLOW" {
			clock.advance(10 * time.Second)
		}
		return true
	})
	client := projectaccess.New(server.URL, projectaccesstest.StaticAccount("token"), nil)
	projectaccess.SetClock(client, clock.read)
	if _, err := client.Access(t.Context(), "AGED", "carol@example.com"); err != nil {
		t.Fatal(err)
	}
	clock.advance(projectaccess.CacheTTL - 5*time.Second)
	if err := projectaccess.Visible(t.Context(), client, "carol@example.com", []string{"AGED", "SLOW"}); err != nil {
		t.Fatal(err)
	}
	if got := calls("AGED"); got != 2 {
		t.Fatalf("AGED was asked %d times, want 2: its answer was %s old when the check began and older than %s by its end, so Visible asks again", got, projectaccess.CacheTTL-5*time.Second, projectaccess.CacheTTL)
	}
}

// TestAnOlderAnswerDoesNotReplaceANewerOne: two questions about one project and person in flight
// at once; the one asked first answers last, and the cache keeps the answer to the question asked
// later.
func TestAnOlderAnswerDoesNotReplaceANewerOne(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)}
	// The first question's handler signals once it is blocked, so the second question is sent
	// only after the first has reached Dispatch; release is closed on every way out of the test,
	// so the server's cleanup never waits on a handler still blocked.
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	free := func() { releaseOnce.Do(func() { close(release) }) }
	server, _ := projectServer(t, func(_ string, call int) bool {
		if call == 1 {
			close(entered)
			<-release
			return true
		}
		return false
	})
	t.Cleanup(free)
	client := projectaccess.New(server.URL, projectaccesstest.StaticAccount("token"), nil)
	projectaccess.SetClock(client, clock.read)
	first := make(chan error, 1)
	go func() {
		_, err := client.Access(context.Background(), "PRIV", "carol@example.com")
		first <- err
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the first question never reached Dispatch")
	}
	clock.advance(time.Second)
	if answer, err := client.Access(t.Context(), "PRIV", "carol@example.com"); err != nil || answer.Visible {
		t.Fatalf("the later question = %+v, %v; want not visible", answer, err)
	}
	free()
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if answer, err := client.Access(t.Context(), "PRIV", "carol@example.com"); err != nil || answer.Visible {
		t.Fatalf("the cached answer after both arrived = %+v, %v; want the later question's, not visible", answer, err)
	}
}

// TestOnlyDispatchsProjectNotFoundIsAnAnswer: a 404 is Dispatch holding no such project only when
// its body's code is PROJECT_NOT_FOUND. Any other 404 — a Dispatch that does not serve the
// project-access routes, or a wrong path behind an ingress — is a failure, never cached and never
// read as a refusal of the project.
func TestOnlyDispatchsProjectNotFoundIsAnAnswer(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"NOT_FOUND","error":"no route"}`))
	}))
	t.Cleanup(server.Close)
	client := projectaccess.New(server.URL, projectaccesstest.StaticAccount("token"), nil)
	for range 2 {
		_, err := client.Access(t.Context(), "PRIV", "carol@example.com")
		if err == nil || errors.Is(err, projectaccess.ErrProjectNotFound) || !strings.Contains(err.Error(), "NOT_FOUND") {
			t.Fatalf("Access answered by a router's 404 = %v, want a failure naming the code NOT_FOUND, not ErrProjectNotFound", err)
		}
	}
	if calls != 2 {
		t.Fatalf("Dispatch was asked %d times for two questions, want 2: a failure is never cached", calls)
	}
	if err := projectaccess.Visible(t.Context(), client, "carol@example.com", []string{"PRIV"}); errors.Is(err, projectaccess.ErrNotVisible) || err == nil {
		t.Fatalf("Visible answered by a router's 404 = %v, want a failure that is no refusal", err)
	}
}

// TestARefusalOfTheBrokersOwnTokenIsErrBrokerRefused: Dispatch answering 401 or 403 to the
// broker's own account token is ErrBrokerRefused, from both routes: a deployment that has not
// listed the broker as one that may ask, which no caller reads as an outage.
func TestARefusalOfTheBrokersOwnTokenIsErrBrokerRefused(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"code":"ACCESS_ACCOUNT_REQUIRED","error":"not listed"}`))
		}))
		t.Cleanup(server.Close)
		client := projectaccess.New(server.URL, projectaccesstest.StaticAccount("token"), nil)
		if _, err := client.Access(t.Context(), "PRIV", "carol@example.com"); !errors.Is(err, projectaccess.ErrBrokerRefused) || !strings.Contains(err.Error(), "ACCESS_ACCOUNT_REQUIRED") {
			t.Errorf("Access answered %d = %v, want ErrBrokerRefused naming the code", status, err)
		}
		if _, err := client.MemberKind(t.Context(), "carol@example.com"); !errors.Is(err, projectaccess.ErrBrokerRefused) {
			t.Errorf("MemberKind answered %d = %v, want ErrBrokerRefused", status, err)
		}
	}
}

// TestARefusalIsErrBrokerRefusedWhateverItsBody: the status is the refusal. A 401 or a 403 whose
// body runs past what the client reads of an answer, or ends before the length its header
// declares, is ErrBrokerRefused from both routes all the same: never a failure to ask Dispatch,
// which a caller mints through.
func TestARefusalIsErrBrokerRefusedWhateverItsBody(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		for _, body := range projectaccesstest.RefusalBodies {
			client := projectaccess.New(projectaccesstest.NewRefusing(t, status, body), projectaccesstest.StaticAccount("token"), nil)
			if _, err := client.Access(t.Context(), "PRIV", "carol@example.com"); !errors.Is(err, projectaccess.ErrBrokerRefused) {
				t.Errorf("Access answered %d with a body %s = %v, want ErrBrokerRefused", status, body, err)
			}
			if _, err := client.MemberKind(t.Context(), "carol@example.com"); !errors.Is(err, projectaccess.ErrBrokerRefused) {
				t.Errorf("MemberKind answered %d with a body %s = %v, want ErrBrokerRefused", status, body, err)
			}
		}
	}
}
