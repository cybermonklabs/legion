package projectaccess_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/dispatchread"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
)

// *Client is the one PrivateLister the broker wires.
var _ projectaccess.PrivateLister = (*projectaccess.Client)(nil)

// fixedAccount is the account fixedAnswer's server takes, a label and no credential.
var fixedAccount = projectaccesstest.StaticAccount("fixed-answer-account")

// fixedAnswer serves status and body to every request presenting fixedAccount, counts each, and
// answers its base URL and that count: one Dispatch answer, whatever its shape, for the rows below.
func fixedAnswer(t *testing.T, status int, body string) (string, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/access/private-projects" || r.Header.Get("Authorization") != "Bearer "+string(fixedAccount) {
			t.Errorf("the client asked %s %s with %q, want GET /api/v1/access/private-projects with its own bearer", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server.URL, &calls
}

// The two reads of every private project, by the name each row's failure names it by.
var privateReads = []struct {
	name string
	read func(*projectaccess.Client, context.Context) ([]projectaccess.PrivateProject, error)
}{
	{"PrivateProjects", (*projectaccess.Client).PrivateProjects},
	{"PrivateProjectsFresh", (*projectaccess.Client).PrivateProjectsFresh},
}

// TestThePrivateProjectsAnswerIsNeverReadShort: an answer that is no list of private projects is a
// failure to read one, never an empty list: a caller that read it as empty would serve every
// private project's channel as a public one's. Each such answer is an error that is not
// ErrBrokerRefused, beside a nil list, and is not kept, so the next call asks Dispatch again. Only
// "projects": [] is the empty list. An answer holding one key twice in an object is no list
// either: encoding/json would keep the last member, so a second "projects": [] would read as no
// private project, and a second "slack_channels": [] as a project with no channel. Each row runs
// through both reads; a private project with no slack_channels key is the fresh read's failure
// alone, and PrivateProjects keeps it, marked.
func TestThePrivateProjectsAnswerIsNeverReadShort(t *testing.T) {
	failures := []struct {
		status int
		body   string
	}{
		{http.StatusNotFound, `{"code":"NOT_FOUND","error":"/api/v1/access/private-projects"}`},
		{http.StatusNotFound, `{"code":"PROJECT_NOT_FOUND","error":"no such project"}`},
		{http.StatusOK, `{}`},
		{http.StatusOK, `{"projects": null}`},
		{http.StatusOK, `{"projects": {}}`},
		{http.StatusOK, `{"projects": "PRIV"}`},
		// Beyond the plan's six: any other status, a body that does not decode, and an entry that
		// names no project, which no caller could match against a key.
		{http.StatusServiceUnavailable, `{"code":"UNAVAILABLE"}`},
		{http.StatusOK, `{"projects": [`},
		{http.StatusOK, `{"projects": [{"slack_channels": ["C0PRIV00001"]}]}`},
		// Two readings of one answer, which encoding/json reads as the last member's: projects
		// twice, in the same case and in another, and an entry's slack_channels twice.
		{http.StatusOK, `{"projects": [{"project": "PRIV", "slack_channels": ["C0PRIV00001"]}], "projects": []}`},
		{http.StatusOK, `{"projects": [{"project": "PRIV", "slack_channels": ["C0PRIV00001"]}], "PROJECTS": []}`},
		{http.StatusOK, `{"projects": [{"project": "PRIV", "slack_channels": ["C0PRIV00001"], "slack_channels": []}]}`},
	}
	for _, read := range privateReads {
		for _, row := range failures {
			url, calls := fixedAnswer(t, row.status, row.body)
			client := projectaccess.New(url, fixedAccount, nil)
			for call := 1; call <= 2; call++ {
				projects, err := read.read(client, t.Context())
				if err == nil || errors.Is(err, dispatchread.ErrBrokerRefused) || projects != nil {
					t.Errorf("%s call %d on %d %s = %#v, %v; want a nil list and an error that is not ErrBrokerRefused", read.name, call, row.status, row.body, projects, err)
				}
				if got := calls.Load(); got != int64(call) {
					t.Errorf("%s call %d on %d %s asked Dispatch %d times in all, want %d: a failure is never kept", read.name, call, row.status, row.body, got, call)
				}
			}
		}
		url, _ := fixedAnswer(t, http.StatusOK, `{"projects": []}`)
		client := projectaccess.New(url, fixedAccount, nil)
		if projects, err := read.read(client, t.Context()); err != nil || projects == nil || len(projects) != 0 {
			t.Errorf(`%s on {"projects": []} = %#v, %v; want the empty list and no error`, read.name, projects, err)
		}
		url, _ = fixedAnswer(t, http.StatusForbidden, `{"code":"ACCESS_ACCOUNT_REQUIRED"}`)
		client = projectaccess.New(url, fixedAccount, nil)
		if projects, err := read.read(client, t.Context()); !errors.Is(err, dispatchread.ErrBrokerRefused) || projects != nil {
			t.Errorf("%s on 403 = %#v, %v; want ErrBrokerRefused and a nil list", read.name, projects, err)
		}
	}

	unmarked := `{"projects": [{"project": "PRIV", "slack_channels_changed_at": null}]}`
	url, _ := fixedAnswer(t, http.StatusOK, unmarked)
	client := projectaccess.New(url, fixedAccount, nil)
	if projects, err := client.PrivateProjectsFresh(t.Context()); err == nil || projects != nil {
		t.Errorf("PrivateProjectsFresh on %s = %#v, %v; want a nil list and an error", unmarked, projects, err)
	}
	want := []projectaccess.PrivateProject{{Key: "PRIV"}}
	if projects, err := client.PrivateProjects(t.Context()); err != nil || !reflect.DeepEqual(projects, want) {
		t.Errorf("PrivateProjects on %s = %#v, %v; want %#v, the entry kept with SlackChannelsPresent false", unmarked, projects, err, want)
	}
}

// TestThePrivateProjectsListReadsEveryPrivateRow: the list Dispatch answers reaches
// PrivateProject field by field, private rows alone and in key order, with a row that carries no
// slack_channels marked so, and the time each list changed; and a bearer Dispatch refuses is
// ErrBrokerRefused.
func TestThePrivateProjectsListReadsEveryPrivateRow(t *testing.T) {
	changed := time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC)
	server := projectaccesstest.NewServer(t, map[string]projectaccesstest.Row{
		"PRIV":  {Visibility: "private", Owner: "carol@example.com", SlackChannels: []string{"C0PRIV00001", "C0PRIV00002"}, SlackChannelsChangedAt: changed},
		"SYNTH": {Visibility: "private", Owner: "carol@example.com", OmitSlackChannels: true},
		"EMPTY": {Visibility: "private", Owner: "carol@example.com"},
		"PUB":   {Visibility: "public", Owner: "carol@example.com", SlackChannels: []string{"C0PUB000001"}},
	}, nil)
	client := projectaccess.New(server.URL, server.Account, nil)
	projects, err := client.PrivateProjects(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []projectaccess.PrivateProject{
		{Key: "EMPTY", SlackChannels: []string{}, SlackChannelsPresent: true},
		{Key: "PRIV", SlackChannels: []string{"C0PRIV00001", "C0PRIV00002"}, SlackChannelsPresent: true, SlackChannelsChangedAt: changed},
		{Key: "SYNTH"},
	}
	if !reflect.DeepEqual(projects, want) {
		t.Fatalf("PrivateProjects = %#v, want %#v", projects, want)
	}
	stranger := projectaccess.New(server.URL, projectaccesstest.StaticAccount("not-the-stand-ins-token"), nil)
	if _, err := stranger.PrivateProjects(t.Context()); !errors.Is(err, dispatchread.ErrBrokerRefused) {
		t.Fatalf("PrivateProjects with a bearer Dispatch refuses = %v, want ErrBrokerRefused", err)
	}
}

// TestTheFreshPrivateProjectsReadAsksDispatchEachTime: PrivateProjectsFresh asks Dispatch on every
// call, with no clock advance, while PrivateProjects on the same client answers what it kept for
// CacheTTL; a fresh call that fails is an error, and the next one that succeeds answers what
// Dispatch holds then, never the last good list; and no fresh call touches what PrivateProjects
// kept. A reader that must see a project the moment it turns private (a listener deciding where a
// message may go) can then rely on it.
func TestTheFreshPrivateProjectsReadAsksDispatchEachTime(t *testing.T) {
	server := projectaccesstest.NewServer(t, map[string]projectaccesstest.Row{
		"PRIV": {Visibility: "private", Owner: "carol@example.com", SlackChannels: []string{"C0PRIV00001"}},
		"PUB":  {Visibility: "public", Owner: "carol@example.com"},
	}, nil)
	client := projectaccess.New(server.URL, server.Account, nil)
	now := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	projectaccess.SetClock(client, func() time.Time { return now })
	first := []projectaccess.PrivateProject{{Key: "PRIV", SlackChannels: []string{"C0PRIV00001"}, SlackChannelsPresent: true}}
	second := []projectaccess.PrivateProject{
		{Key: "PRIV", SlackChannels: []string{"C0PRIV00001"}, SlackChannelsPresent: true},
		{Key: "PUB", SlackChannels: []string{}, SlackChannelsPresent: true},
	}
	third := []projectaccess.PrivateProject{
		{Key: "PRIV", SlackChannels: []string{"C0PRIV00002"}, SlackChannelsPresent: true},
		{Key: "PUB", SlackChannels: []string{}, SlackChannelsPresent: true},
	}
	expect := func(call string, read func(context.Context) ([]projectaccess.PrivateProject, error), want []projectaccess.PrivateProject, asks int) {
		t.Helper()
		before := server.Calls()
		got, err := read(t.Context())
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("%s = %#v, %v; want %#v", call, got, err, want)
		}
		if asked := server.Calls() - before; asked != asks {
			t.Fatalf("%s asked Dispatch %d times, want %d", call, asked, asks)
		}
	}

	expect("PrivateProjects, the first call", client.PrivateProjects, first, 1)
	server.SetRow("PUB", projectaccesstest.Row{Visibility: "private", Owner: "carol@example.com"})
	expect("PrivateProjectsFresh, the second call, after PUB turned private", client.PrivateProjectsFresh, second, 1)
	expect("PrivateProjects within CacheTTL, after the fresh call", client.PrivateProjects, first, 0)

	server.SetDown(true)
	before := server.Calls()
	if got, err := client.PrivateProjectsFresh(t.Context()); err == nil || got != nil {
		t.Fatalf("PrivateProjectsFresh while Dispatch is down = %#v, %v; want a nil list and an error, never the last good list", got, err)
	}
	if asked := server.Calls() - before; asked != 1 {
		t.Fatalf("PrivateProjectsFresh while Dispatch is down asked it %d times, want 1", asked)
	}
	server.SetRow("PRIV", projectaccesstest.Row{Visibility: "private", Owner: "carol@example.com", SlackChannels: []string{"C0PRIV00002"}})
	server.SetDown(false)
	expect("PrivateProjectsFresh, the call after the failed one", client.PrivateProjectsFresh, third, 1)
	expect("PrivateProjects within CacheTTL, after every fresh call", client.PrivateProjects, first, 0)

	now = now.Add(projectaccess.CacheTTL)
	expect("PrivateProjects once CacheTTL has passed", client.PrivateProjects, third, 1)

	// A fresh call on a client that has kept nothing keeps nothing either.
	empty := projectaccess.New(server.URL, server.Account, nil)
	projectaccess.SetClock(empty, func() time.Time { return now })
	expect("PrivateProjectsFresh on a new client", empty.PrivateProjectsFresh, third, 1)
	expect("PrivateProjects on that client, after its one fresh call", empty.PrivateProjects, third, 1)
}

// TestThePrivateProjectsListAnswersWhenItWasAsked: PrivateProjectsAsOf answers a kept list beside
// the time its question was asked, not the time it was read back, so a caller ages it as it ages
// any other answer; and under a context Fresh made it asks Dispatch whatever the cache holds,
// answers that question's time, and keeps that answer.
func TestThePrivateProjectsListAnswersWhenItWasAsked(t *testing.T) {
	server := projectaccesstest.NewServer(t, map[string]projectaccesstest.Row{
		"PRIV": {Visibility: "private", Owner: "carol@example.com", SlackChannels: []string{"C0PRIV00001"}},
	}, nil)
	client := projectaccess.New(server.URL, server.Account, nil)
	asked := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	now := asked
	projectaccess.SetClock(client, func() time.Time { return now })
	read := func(call string, ctx context.Context, wantAsked time.Time, asks int) {
		t.Helper()
		before := server.Calls()
		projects, at, err := client.PrivateProjectsAsOf(ctx)
		if err != nil || len(projects) != 1 || !at.Equal(wantAsked) {
			t.Fatalf("%s = %#v, %v, %v; want PRIV, asked at %v", call, projects, at, err, wantAsked)
		}
		if got := server.Calls() - before; got != asks {
			t.Fatalf("%s asked Dispatch %d times, want %d", call, got, asks)
		}
	}
	read("PrivateProjectsAsOf, the first call", t.Context(), asked, 1)
	now = asked.Add(projectaccess.CacheTTL - time.Second)
	read("PrivateProjectsAsOf within CacheTTL", t.Context(), asked, 0)
	read("PrivateProjectsAsOf under Fresh", projectaccess.Fresh(t.Context()), now, 1)
	read("PrivateProjectsAsOf after the fresh call", t.Context(), now, 0)
}
