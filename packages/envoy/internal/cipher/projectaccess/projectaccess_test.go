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
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/dispatchread"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/testwait"
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

// TestTheReadNamesItself: every call names the project-access check and the broker's build in
// its User-Agent, so Dispatch's access log tells the check's calls from every other caller's; a
// broker whose build names no commit sends `(devel)`. The calls stay on /api/v1.
func TestTheReadNamesItself(t *testing.T) {
	const commit = "0123456789abcdef0123456789abcdef01234567"
	for _, tc := range []struct{ commit, want string }{
		{commit, "envoy-broker-project-access/" + commit},
		{"", "envoy-broker-project-access/(devel)"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			var mu sync.Mutex
			var sent []string
			recorder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				sent = append(sent, r.URL.Path+" "+r.UserAgent())
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if strings.HasPrefix(r.URL.Path, "/api/v1/access/person") {
					_, _ = w.Write([]byte(`{"person":"carol@example.com","member_kind":"full"}`))
					return
				}
				_, _ = w.Write([]byte(`{"project":"PRIV","visibility":"private","owner":"carol@example.com","person":"carol@example.com","member_kind":"full","visible":true,"repositories":[]}`))
			}))
			t.Cleanup(recorder.Close)
			client := projectaccess.New(recorder.URL, projectaccesstest.StaticAccount("token"), nil)
			client.Commit = tc.commit
			if _, err := client.Access(t.Context(), "PRIV", "carol@example.com"); err != nil {
				t.Fatal(err)
			}
			if _, err := client.MemberKind(t.Context(), "carol@example.com"); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			want := []string{"/api/v1/access/projects/PRIV " + tc.want, "/api/v1/access/person " + tc.want}
			if !reflect.DeepEqual(sent, want) {
				t.Fatalf("the client asked %q, want %q", sent, want)
			}
		})
	}
}

// TestAccessReadsTheK3Answer: every field of Dispatch's answer reaches Answer, for a person the
// project's list names and for one it does not. The answer names no repositories, as Dispatch's
// does not (GET /api/v1/access/projects/{key}).
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
		// The stand-in answers as a Dispatch with dispatch://INFRA-3862's #t-a-31 does: the key is
		// always there, empty for a project naming no channel.
		SlackChannels: []string{}, SlackChannelsPresent: true,
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
	client.UseClock(func() time.Time { return now })
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

// TestAFreshQuestionIsAskedOfDispatchWhateverTheCacheHolds: under Fresh, Access and MemberKind ask
// Dispatch though the cache holds a young answer, and what Dispatch answers replaces it, so a
// check that must see a change younger than CacheTTL (the model judge's approval) sees it. A
// question without Fresh still reads the cache, as before.
func TestAFreshQuestionIsAskedOfDispatchWhateverTheCacheHolds(t *testing.T) {
	server := standIn(t)
	client := clientOf(server)
	if got, err := client.Access(t.Context(), "PUB", ""); err != nil || got.Private() {
		t.Fatalf("Access(PUB) = %+v, %v; want the public answer", got, err)
	}
	if kind, err := client.MemberKind(t.Context(), "bob@example.com"); err != nil || kind != projectaccess.MemberRestricted {
		t.Fatalf("MemberKind(bob) = %q, %v; want restricted", kind, err)
	}
	server.SetRow("PUB", projectaccesstest.Row{Visibility: "private", Owner: "carol@example.com"})
	if got, err := client.Access(t.Context(), "PUB", ""); err != nil || got.Private() {
		t.Fatalf("Access(PUB) inside CacheTTL after the change = %+v, %v; want the cached public answer", got, err)
	}
	asked := server.Calls()
	fresh := projectaccess.Fresh(t.Context())
	if got, err := client.Access(fresh, "PUB", ""); err != nil || !got.Private() {
		t.Fatalf("Access(Fresh, PUB) = %+v, %v; want Dispatch's private answer, not the cached one", got, err)
	}
	if kind, err := client.MemberKind(fresh, "bob@example.com"); err != nil || kind != projectaccess.MemberRestricted {
		t.Fatalf("MemberKind(Fresh, bob) = %q, %v; want restricted", kind, err)
	}
	if calls := server.Calls() - asked; calls != 2 {
		t.Fatalf("the two fresh questions asked Dispatch %d times, want 2", calls)
	}
	if got, err := client.Access(t.Context(), "PUB", ""); err != nil || !got.Private() {
		t.Fatalf("Access(PUB) after the fresh question = %+v, %v; want the fresh private answer, which replaced the cached one", got, err)
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
		_, _ = w.Write([]byte(`{"project":"PRIV","visibility":"private","owner":"carol@example.com","person":"carol@example.com","member_kind":"full","visible":true,"slack_channels":[]}`))
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
	if err := projectaccess.ListAllowed(t.Context(), stranger, "bob@example.com", nil); !errors.Is(err, dispatchread.ErrBrokerRefused) || errors.Is(err, projectaccess.ErrUnchecked) {
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

// TestRestrictedRefusesAKindItDoesNotKnow: Restricted decides from the three kinds alone, whatever
// Access answers: full and none are no restricted member, restricted is one, and any other kind is
// a MemberKindUncheckable naming the person, ErrUnknownMemberKind, in the words that say Dispatch
// answered a kind the broker does not know. So a check that cannot decide refuses even through an
// Access that is not this package's Client.
func TestRestrictedRefusesAKindItDoesNotKnow(t *testing.T) {
	recording := &recordingAccess{kinds: map[string]string{
		"bob@example.com": projectaccess.MemberRestricted, "carol@example.com": projectaccess.MemberFull, "gus@example.com": "guest",
	}}
	for person, want := range map[string]bool{"bob@example.com": true, "carol@example.com": false, "alice@example.com": false} {
		if restricted, err := projectaccess.Restricted(t.Context(), recording, person); err != nil || restricted != want {
			t.Errorf("Restricted(%s) = %t, %v; want %t", person, restricted, err, want)
		}
	}
	restricted, err := projectaccess.Restricted(t.Context(), recording, "gus@example.com")
	var uncheckable *projectaccess.MemberKindUncheckable
	if restricted || !errors.As(err, &uncheckable) || uncheckable.Person != "gus@example.com" || !errors.Is(err, projectaccess.ErrUnknownMemberKind) ||
		err.Error() != "whether Dispatch calls gus@example.com a restricted member cannot be read: Dispatch answered a member kind this broker does not know" {
		t.Fatalf("Restricted(gus, answered %q) = %t, %v; want a MemberKindUncheckable naming gus, ErrUnknownMemberKind", "guest", restricted, err)
	}
}

// TestStatesAnswersOneEntryPerProject: States answers each project Dispatch holds, as it is to a
// caller naming no person, and Missing for the one it does not, asked at a time of its own so a
// caller can age it as it ages any other answer.
func TestStatesAnswersOneEntryPerProject(t *testing.T) {
	client := clientOf(standIn(t))
	states, err := client.States(t.Context(), []string{"PRIV", "PUB", "NOPE"})
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 3 || !states["PRIV"].Private() || states["PUB"].Private() || states["PUB"].Visible != true || states["PRIV"].Missing || states["PUB"].Missing {
		t.Fatalf("States(PRIV PUB NOPE) = %+v, want PRIV private and PUB public, both held, and an entry for NOPE", states)
	}
	if nope := states["NOPE"]; !nope.Missing || nope.Project != "NOPE" || nope.AskedAt.IsZero() || nope.Visible || nope.Private() {
		t.Fatalf("States answered %+v for NOPE, which Dispatch does not hold; want Missing, named NOPE, with the time it was asked and nothing else", nope)
	}
	down := standIn(t)
	down.SetDown(true)
	if _, err := clientOf(down).States(t.Context(), []string{"PRIV"}); err == nil {
		t.Fatal("States while Dispatch is down answered, want a failure")
	}
}

// TestAccessRefusesAVisibilityItDoesNotKnow is the side review's finding 1 on
// cybermonklabs/legion#4135 (review 5927943331): Dispatch answers a project's visibility as
// "public" or "private", and a 200 answer with no visibility, an empty one or another word is
// neither. The broker reads none of them as public: Access answers ErrUnknownVisibility, which is
// no answer and is never kept, so States and Visible fail with it too and each later question
// asks Dispatch again, as an answer with an unknown member kind does.
func TestAccessRefusesAVisibilityItDoesNotKnow(t *testing.T) {
	for _, c := range []struct{ name, body string }{
		{"an answer with no visibility field", `{"project":"PRIV","owner":"carol@example.com","member_kind":"full","visible":true}`},
		{"an empty visibility", `{"project":"PRIV","visibility":"","owner":"carol@example.com","member_kind":"full","visible":true}`},
		{"a visibility of another word", `{"project":"PRIV","visibility":"internal","owner":"carol@example.com","member_kind":"full","visible":true}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			var mu sync.Mutex
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				mu.Lock()
				calls++
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(c.body))
			}))
			t.Cleanup(server.Close)
			client := projectaccess.New(server.URL, projectaccesstest.StaticAccount("token"), nil)
			if answer, err := client.Access(t.Context(), "PRIV", ""); !errors.Is(err, projectaccess.ErrUnknownVisibility) {
				t.Fatalf("Access of %s = %+v, %v; want ErrUnknownVisibility, never an answer read as public", c.body, answer, err)
			}
			if states, err := client.States(t.Context(), []string{"PRIV"}); !errors.Is(err, projectaccess.ErrUnknownVisibility) {
				t.Fatalf("States of %s = %+v, %v; want ErrUnknownVisibility", c.body, states, err)
			}
			if err := projectaccess.Visible(t.Context(), client, "carol@example.com", []string{"PRIV"}); !errors.Is(err, projectaccess.ErrUnknownVisibility) || errors.Is(err, projectaccess.ErrNotVisible) {
				t.Fatalf("Visible of %s = %v; want ErrUnknownVisibility, a failure to ask and no refusal", c.body, err)
			}
			mu.Lock()
			defer mu.Unlock()
			if calls != 3 {
				t.Fatalf("Dispatch was asked %d times for three questions; want 3: an answer the broker cannot read is never kept", calls)
			}
		})
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
	client.UseClock(clock.read)
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
	client.UseClock(clock.read)
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

// TestStatesAsksAgainForAnAnswerThatAgedDuringTheCall is the client's half of the High on
// cybermonklabs/legion#4135 (TNDeep4135 and ReviewPR4135, round 6): States asks one project at a
// time, so an answer that was young when the call began, TEAM's while it was public, can be older
// than CacheTTL by its end; States asks again for each such answer, so every answer it returns is
// younger than CacheTTL as it returns, and one whose answers go on aging out faster than Dispatch
// answers them fails rather than answering an aged one.
func TestStatesAsksAgainForAnAnswerThatAgedDuringTheCall(t *testing.T) {
	t.Run("an answer that aged is asked again", func(t *testing.T) {
		clock := &fakeClock{now: time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)}
		server, calls := projectServer(t, func(project string, _ int) bool {
			if project == "SLOW" {
				clock.advance(10 * time.Second)
			}
			return true
		})
		client := projectaccess.New(server.URL, projectaccesstest.StaticAccount("token"), nil)
		client.UseClock(clock.read)
		if _, err := client.Access(t.Context(), "AGED", ""); err != nil {
			t.Fatal(err)
		}
		clock.advance(projectaccess.CacheTTL - 5*time.Second)
		states, err := client.States(t.Context(), []string{"AGED", "SLOW"})
		if err != nil {
			t.Fatal(err)
		}
		if got := calls("AGED"); got != 2 {
			t.Fatalf("AGED was asked %d times, want 2: its answer was %s old when States began and older than %s by its end, so States asks again", got, projectaccess.CacheTTL-5*time.Second, projectaccess.CacheTTL)
		}
		for project, answer := range states {
			if age := clock.read().Sub(answer.AskedAt); age >= projectaccess.CacheTTL {
				t.Errorf("States answered %s asked %s before it returned; want every answer younger than %s", project, age, projectaccess.CacheTTL)
			}
		}
	})
	t.Run("answers that go on aging out fail the call", func(t *testing.T) {
		clock := &fakeClock{now: time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)}
		server, calls := projectServer(t, func(string, int) bool {
			clock.advance(20 * time.Second)
			return true
		})
		client := projectaccess.New(server.URL, projectaccesstest.StaticAccount("token"), nil)
		client.UseClock(clock.read)
		states, err := client.States(t.Context(), []string{"ONE", "TWO"})
		if err == nil {
			t.Fatalf("States, each answer taking 20 s, answered %+v; want a failure, since each pass leaves one answer older than %s", states, projectaccess.CacheTTL)
		}
		if calls("ONE")+calls("TWO") < 4 {
			t.Fatalf("States asked Dispatch %d times before it failed; want it to have asked again, more than once", calls("ONE")+calls("TWO"))
		}
	})
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
	client.UseClock(clock.read)
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

// TestOnePersonsMemberKindReadsInFlightTogetherShareOneCall: reads of one person's member kind
// that are in flight together make one call to Dispatch, and each gets its answer. No clock
// decides it: the stand-in holds the first call until every other read has joined it (SetJoined)
// or, where reads do not share, until a second call reaches it, and answers only then; the bound
// is for a box too stalled to get there at all, and can only fail the test.
func TestOnePersonsMemberKindReadsInFlightTogetherShareOneCall(t *testing.T) {
	const readers = 8
	var mu sync.Mutex
	calls := 0
	called := func() int { mu.Lock(); defer mu.Unlock(); return calls }
	release := make(chan struct{})
	var releaseOnce sync.Once
	free := func() { releaseOnce.Do(func() { close(release) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"person":"bob@example.com","member_kind":"restricted"}`)
	}))
	t.Cleanup(server.Close)
	t.Cleanup(free)
	client := projectaccess.New(server.URL, projectaccesstest.StaticAccount("token"), nil)
	var joined atomic.Int32
	projectaccess.SetJoined(client, func() { joined.Add(1) })
	type read struct {
		kind string
		err  error
	}
	reads := make(chan read, readers)
	for range readers {
		go func() {
			kind, err := client.MemberKind(context.Background(), "bob@example.com")
			reads <- read{kind, err}
		}()
	}
	if !testwait.Holds(time.Minute, 5*time.Millisecond, func() bool { return joined.Load() == readers-1 || called() > 1 }) {
		t.Fatalf("in a minute %d of %d reads joined the one in flight and Dispatch had %d calls", joined.Load(), readers-1, called())
	}
	free()
	for range readers {
		if got := <-reads; got.err != nil || got.kind != projectaccess.MemberRestricted {
			t.Fatalf("a read in flight with others = %q, %v; want restricted", got.kind, got.err)
		}
	}
	if n := called(); n != 1 {
		t.Fatalf("%d reads of one person's member kind in flight together made %d calls to Dispatch, want one", readers, n)
	}
}

// TestOnePersonsCallInFlightIsNoOtherPersonsAnswer: a read of one person's member kind never
// joins another person's call in flight. bob, a restricted member, is asked first and his call is
// held at the stand-in; carol, a full member, is asked while it is held, and makes a call of her
// own; each gets their own kind. A key shared between people would hand carol bob's
// "restricted", or, the other way round, a restricted member a full member's "full". No clock
// decides it: bob's call is released only once carol's read has answered or joined his (SetJoined),
// and the minute bounds a box too stalled to get there at all, which can only fail the test.
func TestOnePersonsCallInFlightIsNoOtherPersonsAnswer(t *testing.T) {
	kinds := map[string]string{"bob@example.com": projectaccess.MemberRestricted, "carol@example.com": projectaccess.MemberFull}
	var mu sync.Mutex
	calls := map[string]int{}
	called := func(person string) int { mu.Lock(); defer mu.Unlock(); return calls[person] }
	bobHeld, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	free := func() { releaseOnce.Do(func() { close(release) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		person := r.URL.Query().Get("person")
		mu.Lock()
		calls[person]++
		first := calls[person] == 1
		mu.Unlock()
		if person == "bob@example.com" && first {
			close(bobHeld)
			<-release
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"person":%q,"member_kind":%q}`, person, kinds[person])
	}))
	t.Cleanup(server.Close)
	t.Cleanup(free)
	client := projectaccess.New(server.URL, projectaccesstest.StaticAccount("token"), nil)
	var joined atomic.Int32
	projectaccess.SetJoined(client, func() { joined.Add(1) })
	type read struct {
		kind string
		err  error
	}
	bob, carol := make(chan read, 1), make(chan read, 1)
	go func() {
		kind, err := client.MemberKind(context.Background(), "bob@example.com")
		bob <- read{kind, err}
	}()
	select {
	case <-bobHeld:
	case <-time.After(time.Minute):
		t.Fatal("bob's call did not reach the stand-in in a minute")
	}
	go func() {
		kind, err := client.MemberKind(context.Background(), "carol@example.com")
		carol <- read{kind, err}
	}()
	var carolRead read
	carolAnswered := false
	if !testwait.Holds(time.Minute, 5*time.Millisecond, func() bool {
		select {
		case carolRead = <-carol:
			carolAnswered = true
		default:
		}
		return carolAnswered || joined.Load() > 0
	}) {
		t.Fatalf("in a minute carol's read neither answered nor joined a call in flight (calls: bob %d, carol %d)", called("bob@example.com"), called("carol@example.com"))
	}
	free()
	bobRead := <-bob
	if !carolAnswered {
		carolRead = <-carol
	}
	if bobRead.err != nil || bobRead.kind != projectaccess.MemberRestricted || carolRead.err != nil || carolRead.kind != projectaccess.MemberFull ||
		joined.Load() != 0 || called("bob@example.com") != 1 || called("carol@example.com") != 1 {
		t.Fatalf("bob = %q, %v; carol = %q, %v (calls: bob %d, carol %d; joined %d); want bob restricted and carol full, each from a call of their own",
			bobRead.kind, bobRead.err, carolRead.kind, carolRead.err, called("bob@example.com"), called("carol@example.com"), joined.Load())
	}
}

// TestAFailedMemberKindReadIsTheAnswerForTwoSecondsAndNoLonger: while Dispatch does not answer, a
// person's member-kind read that failed is the answer to the next reads of that person for
// FailureTTL from when it came back, and they ask Dispatch nothing; once FailureTTL has passed a
// read asks again and gets Dispatch's answer. It is the one person's: another person's read asks
// Dispatch at once. The client reads a fake clock, so no wait decides it.
func TestAFailedMemberKindReadIsTheAnswerForTwoSecondsAndNoLonger(t *testing.T) {
	server := standIn(t)
	client := clientOf(server)
	clock := &fakeClock{now: time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)}
	client.UseClock(clock.read)
	server.SetDown(true)
	if _, err := client.MemberKind(t.Context(), "bob@example.com"); err == nil {
		t.Fatal("MemberKind(bob) while Dispatch is down answered, want a failure")
	}
	asked := server.Calls()
	server.SetDown(false)
	clock.advance(projectaccess.FailureTTL - time.Millisecond)
	if kind, err := client.MemberKind(t.Context(), "bob@example.com"); err == nil || server.Calls() != asked {
		t.Fatalf("MemberKind(bob) %s after the failure = %q, %v with %d more calls to Dispatch; want the failure again and no call", projectaccess.FailureTTL-time.Millisecond, kind, err, server.Calls()-asked)
	}
	if kind, err := client.MemberKind(t.Context(), "carol@example.com"); err != nil || kind != projectaccess.MemberFull || server.Calls() != asked+1 {
		t.Fatalf("MemberKind(carol) beside bob's failure = %q, %v with %d more calls; want full from one call of its own", kind, err, server.Calls()-asked)
	}
	clock.advance(time.Millisecond)
	if kind, err := client.MemberKind(t.Context(), "bob@example.com"); err != nil || kind != projectaccess.MemberRestricted || server.Calls() != asked+2 {
		t.Fatalf("MemberKind(bob) %s after the failure = %q, %v with %d more calls; want restricted from a call of its own: a failure is kept no longer", projectaccess.FailureTTL, kind, err, server.Calls()-asked)
	}
}

// TestAReaderThatGivesUpFailsNoOtherReadOfThatPerson: the first reader of bob's member kind
// starts the one call and then gives up (its context is cancelled) while a second reader waits
// on the same call. The second reader gets Dispatch's answer, and a third read is answered from
// the cache with no new call. The stand-in holds the call until the first reader has given up,
// so no clock decides it; the bounds can only fail.
func TestAReaderThatGivesUpFailsNoOtherReadOfThatPerson(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"person":"bob@example.com","member_kind":"restricted"}`)
	}))
	t.Cleanup(server.Close)
	t.Cleanup(free)
	client := projectaccess.New(server.URL, projectaccesstest.StaticAccount("token"), nil)
	joined := make(chan struct{}, 1)
	projectaccess.SetJoined(client, func() { joined <- struct{}{} })

	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() {
		_, err := client.MemberKind(ctx, "bob@example.com")
		first <- err
	}()
	if !testwait.Holds(time.Minute, 5*time.Millisecond, func() bool { return calls.Load() == 1 }) {
		t.Fatal("the first read's call did not reach Dispatch in a minute")
	}
	type read struct {
		kind string
		err  error
	}
	second := make(chan read, 1)
	go func() {
		kind, err := client.MemberKind(context.Background(), "bob@example.com")
		second <- read{kind, err}
	}()
	select {
	case <-joined:
	case <-time.After(time.Minute):
		t.Fatal("the second read did not join the call in flight in a minute")
	}
	cancel()
	select {
	case err := <-first:
		if err == nil {
			t.Fatal("the first read, cancelled, answered nil; want its context's error")
		}
	case <-time.After(time.Minute):
		t.Fatal("the cancelled first read did not return in a minute")
	}
	free()
	select {
	case got := <-second:
		if got.err != nil || got.kind != projectaccess.MemberRestricted {
			t.Fatalf("the second read, beside a first that gave up, = %q, %v; want restricted", got.kind, got.err)
		}
	case <-time.After(time.Minute):
		t.Fatal("the second read did not return in a minute")
	}
	if kind, err := client.MemberKind(context.Background(), "bob@example.com"); err != nil || kind != projectaccess.MemberRestricted || calls.Load() != 1 {
		t.Fatalf("a third read = %q, %v after %d calls; want restricted from the cache and one call", kind, err, calls.Load())
	}
}

// TestAnEmptyListAsksDispatchAfreshOnceItAnswersAgain: ListAllowed's empty list is the one place
// a failure to ask lets something through (ErrUnchecked, which the exchange mints on), so it never
// takes a failure an earlier read kept: once Dispatch answers again, the next ListAllowed asks it,
// and a restricted member's empty list is refused ErrListRequired even within FailureTTL of the
// failure. A secrets path's read in that window still gets the kept failure, which refuses. The
// client reads a fake clock, so no wait decides it.
func TestAnEmptyListAsksDispatchAfreshOnceItAnswersAgain(t *testing.T) {
	server := standIn(t)
	client := clientOf(server)
	clock := &fakeClock{now: time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)}
	client.UseClock(clock.read)
	server.SetDown(true)
	if err := projectaccess.ListAllowed(t.Context(), client, "bob@example.com", nil); !errors.Is(err, projectaccess.ErrUnchecked) {
		t.Fatalf("ListAllowed(bob, none) while Dispatch is down = %v, want ErrUnchecked", err)
	}
	server.SetDown(false)
	clock.advance(time.Second)
	if _, err := client.MemberKind(t.Context(), "bob@example.com"); err == nil {
		t.Fatalf("MemberKind(bob) %s after the failure answered, want the kept failure", time.Second)
	}
	asked := server.Calls()
	if err := projectaccess.ListAllowed(t.Context(), client, "bob@example.com", nil); !errors.Is(err, projectaccess.ErrListRequired) || server.Calls() != asked+1 {
		t.Fatalf("ListAllowed(bob, none) %s after the failure, with Dispatch answering = %v with %d more calls; want ErrListRequired from a call of its own, not the kept failure", time.Second, err, server.Calls()-asked)
	}
}

// TestAReadWhoseCallerEndedKeepsNoFailure: a member-kind read asked for itself (under Fresh, as
// the judge's approval asks, or for an empty list, as ListAllowed asks) on a caller's context that
// ends before Dispatch answers, its deadline passed or its caller cancelled, leaves no failure for
// the person's next reads to take: that ending says nothing of Dispatch. The next read asks
// Dispatch and gets its answer. The client reads a fake clock, never advanced, so no wait decides
// it.
func TestAReadWhoseCallerEndedKeepsNoFailure(t *testing.T) {
	ended := func() context.Context {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx
	}
	for _, c := range []struct {
		name string
		ctx  func() context.Context
	}{
		{"a deadline that passed", func() context.Context {
			ctx, cancel := context.WithDeadline(context.Background(), time.Unix(0, 0))
			t.Cleanup(cancel)
			return ctx
		}},
		{"a cancelled caller", ended},
	} {
		t.Run(c.name, func(t *testing.T) {
			server := standIn(t)
			client := clientOf(server)
			clock := &fakeClock{now: time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)}
			client.UseClock(clock.read)
			if _, err := client.MemberKind(projectaccess.Fresh(c.ctx()), "bob@example.com"); err == nil {
				t.Fatal("MemberKind(bob) on an ended context answered, want its context's error")
			}
			if kind, err := client.MemberKind(t.Context(), "bob@example.com"); err != nil || kind != projectaccess.MemberRestricted {
				t.Fatalf("MemberKind(bob) after a read whose caller ended = %q, %v; want restricted from Dispatch: an ended caller says nothing of Dispatch", kind, err)
			}
		})
	}
}

// TestAWrapperOfTheClientKeepsTheEmptyListsFreshRead: ListAllowed's empty list asks the member kind
// afresh, never answered by a kept failure, through any Access that hands the context on, a
// wrapper such as projectaccesstest.Swappable included, not only through *Client itself.
func TestAWrapperOfTheClientKeepsTheEmptyListsFreshRead(t *testing.T) {
	server := standIn(t)
	wrapper := &projectaccesstest.Swappable{}
	wrapper.Serve(server)
	server.SetDown(true)
	if _, err := wrapper.MemberKind(t.Context(), "bob@example.com"); err == nil {
		t.Fatal("MemberKind(bob) while Dispatch is down answered, want a failure")
	}
	server.SetDown(false)
	asked := server.Calls()
	if err := projectaccess.ListAllowed(t.Context(), wrapper, "bob@example.com", nil); !errors.Is(err, projectaccess.ErrListRequired) || server.Calls() != asked+1 {
		t.Fatalf("ListAllowed(bob, none) through the wrapper, with Dispatch answering = %v with %d more calls; want ErrListRequired from a call of its own, not the kept failure", err, server.Calls()-asked)
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
		if _, err := client.Access(t.Context(), "PRIV", "carol@example.com"); !errors.Is(err, dispatchread.ErrBrokerRefused) || !strings.Contains(err.Error(), "ACCESS_ACCOUNT_REQUIRED") {
			t.Errorf("Access answered %d = %v, want ErrBrokerRefused naming the code", status, err)
		}
		if _, err := client.MemberKind(t.Context(), "carol@example.com"); !errors.Is(err, dispatchread.ErrBrokerRefused) {
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
			if _, err := client.Access(t.Context(), "PRIV", "carol@example.com"); !errors.Is(err, dispatchread.ErrBrokerRefused) {
				t.Errorf("Access answered %d with a body %s = %v, want ErrBrokerRefused", status, body, err)
			}
			if _, err := client.MemberKind(t.Context(), "carol@example.com"); !errors.Is(err, dispatchread.ErrBrokerRefused) {
				t.Errorf("MemberKind answered %d with a body %s = %v, want ErrBrokerRefused", status, body, err)
			}
		}
	}
}
