package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/requests"
)

// heldRounds is a stand-in Dispatch that holds every answer until a whole round of
// requests.MaySeeAtOnce questions has arrived, so each round is that many questions at once, and
// counts the connections opened to it. Each answer is a public project's.
type heldRounds struct {
	server  *httptest.Server
	opened  atomic.Int32
	arrived chan struct{}
	mu      sync.Mutex
	release chan struct{}
}

func newHeldRounds(t *testing.T) *heldRounds {
	t.Helper()
	h := &heldRounds{arrived: make(chan struct{}), release: make(chan struct{})}
	h.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		wait := h.release
		h.mu.Unlock()
		h.arrived <- struct{}{}
		<-wait
		project := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		_, _ = fmt.Fprintf(w, `{"project":%q,"visibility":"public","owner":"alice@example.com","visible":true}`, project)
	}))
	h.server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			h.opened.Add(1)
		}
	}
	h.server.Start()
	t.Cleanup(h.server.Close)
	return h
}

// round asks requests.MaySeeAtOnce questions at once through ask, releases them once all have
// arrived, and answers how many connections the round opened.
func (h *heldRounds) round(t *testing.T, ask func(i int) error) int {
	t.Helper()
	before := h.opened.Load()
	var wg sync.WaitGroup
	for i := range requests.MaySeeAtOnce {
		wg.Go(func() {
			if err := ask(i); err != nil {
				t.Errorf("question %d: %v", i, err)
			}
		})
	}
	for range requests.MaySeeAtOnce {
		<-h.arrived
	}
	h.mu.Lock()
	close(h.release)
	h.release = make(chan struct{})
	h.mu.Unlock()
	wg.Wait()
	return int(h.opened.Load() - before)
}

// TestTheProjectAccessClientReusesItsConnectionsRoundAfterRound: the client the broker reads
// Dispatch's project access with (projectAccessHTTPClient) asks requests.MaySeeAtOnce questions at
// once, the most one row or one list asks, twice over, and the second round opens no connection:
// each of the first round's is kept and taken again. Go's default client keeps two, so its second
// round opens all but two again, which the control row shows.
func TestTheProjectAccessClientReusesItsConnectionsRoundAfterRound(t *testing.T) {
	for _, c := range []struct {
		name   string
		client *http.Client
		opened int
	}{
		{"the broker's client", projectAccessHTTPClient(), 0},
		{"Go's default transport, the control", &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}, requests.MaySeeAtOnce - 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			dispatch := newHeldRounds(t)
			t.Cleanup(c.client.CloseIdleConnections)
			get := func(int) error {
				res, err := c.client.Get(dispatch.server.URL)
				if err != nil {
					return err
				}
				_, _ = io.Copy(io.Discard, res.Body)
				return res.Body.Close()
			}
			if first := dispatch.round(t, get); first != requests.MaySeeAtOnce {
				t.Fatalf("the first round opened %d connections; want %d, one for each question at once", first, requests.MaySeeAtOnce)
			}
			if second := dispatch.round(t, get); second != c.opened {
				t.Errorf("the second round opened %d connections; want %d", second, c.opened)
			}
		})
	}
}

// TestTheBrokerAsksProjectAccessOnTheClientThatKeepsItsConnections: main's project access is the
// one projectAccessClients builds, and its questions go over projectAccessHTTPClient: two rounds
// of requests.MaySeeAtOnce questions at once, each about projects the client has not asked about,
// and the second round opens no connection. Built on Go's default client, as a nil client would
// build it, the second round opens all but two again, and this test fails. With no
// BROKER_PROJECT_ACCESS_URL there is neither client: a nil *projectaccess.Client and a nil reader.
func TestTheBrokerAsksProjectAccessOnTheClientThatKeepsItsConnections(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("the-broker-account-token\n"), 0o600); err != nil {
		t.Fatalf("write the token file: %v", err)
	}
	if access, repositories := projectAccessClients("", tokenFile); access != nil || repositories != nil {
		t.Fatalf("projectAccessClients with no URL = %v, %v; want nil, nil", access, repositories)
	}

	dispatch := newHeldRounds(t)
	access, repositories := projectAccessClients(dispatch.server.URL, tokenFile)
	if access == nil || repositories == nil {
		t.Fatalf("projectAccessClients(%s) = %v, %v; want both clients", dispatch.server.URL, access, repositories)
	}
	ask := func(round int) func(i int) error {
		return func(i int) error {
			project := fmt.Sprintf("R%dP%d", round, i)
			answer, err := access.Access(context.Background(), project, "")
			if err == nil && (answer.Project != project || !answer.Visible) {
				err = fmt.Errorf("answered %+v; want %s public", answer, project)
			}
			return err
		}
	}
	if first := dispatch.round(t, ask(1)); first != requests.MaySeeAtOnce {
		t.Fatalf("the first round opened %d connections; want %d, one for each question at once", first, requests.MaySeeAtOnce)
	}
	if second := dispatch.round(t, ask(2)); second != 0 {
		t.Errorf("the second round opened %d connections; want 0: main's project access must keep requests.MaySeeAtOnce idle connections (projectAccessHTTPClient), where Go's default client keeps two", second)
	}
}

// TestEndUnrecordedSignInsFollowsProjectAccess holds the production choice that a trade ends a
// pre-0047 sign-in only where the broker can ask Dispatch. The setting's project-access client is
// nil without BROKER_PROJECT_ACCESS_URL and non-nil with it; dropping either half of that choice
// ends old sign-ins on every broker or on none.
func TestEndUnrecordedSignInsFollowsProjectAccess(t *testing.T) {
	for _, c := range []struct {
		name          string
		projectAccess projectaccess.Access
		want          bool
	}{
		{name: "without project access", want: false},
		{name: "with project access", projectAccess: &projectaccess.Client{}, want: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := endUnrecordedSignIns(c.projectAccess); got != c.want {
				t.Fatalf("endUnrecordedSignIns(%T) = %t, want %t", c.projectAccess, got, c.want)
			}
		})
	}
}
