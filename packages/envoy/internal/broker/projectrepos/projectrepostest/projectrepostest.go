// Package projectrepostest is a stand-in for Dispatch's answer to "which repositories do a
// project's issues resolve to" (GET /api/v1/access/projects/{key}/repositories), the route the
// broker's projectrepos client asks. It answers from the lists a test gives it and changes them
// when told to, holds every caller to the one bearer it mints, counts the calls it answers and the
// ones still in flight, and can be made to fail, stall, answer bytes of a test's choosing or hold a
// project's calls until released, so a test can show what the broker relays when Dispatch gives no
// whole answer. It is an ordinary package rather than a _test.go file so every broker package's
// tests, and the real binary's, can serve it.
//
// It spells the wire on its own, field by field, rather than encoding the client's own type: a
// client that read a field under a name Dispatch does not send would then disagree with this
// stand-in, where one declaration shared by both would agree with itself and prove nothing.
package projectrepostest

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// The route's path around the project key, as projectrepos asks it.
const (
	pathPrefix = "/api/v1/access/projects/"
	pathSuffix = "/repositories"
)

// Server is the stand-in: URL is its base, Token the one bearer it takes, and Account that bearer
// as the reader the broker's client presents.
type Server struct {
	URL     string
	Token   string
	Account StaticAccount

	mu           sync.Mutex
	repositories map[string][]string
	answer       *rawAnswer
	stall        time.Duration
	held         map[string]chan struct{}
	calls        int
	inFlight     int
}

// rawAnswer is a status and body the stand-in answers every call with in place of its lists.
type rawAnswer struct {
	status int
	body   string
}

// StaticAccount is a bearer that never rotates: a brokertoken.Account for a test.
type StaticAccount string

// Read answers the bearer.
func (a StaticAccount) Read() (string, error) { return string(a), nil }

// NewServer serves repositories (by project key) until t ends. A project it does not hold is
// Dispatch's 404 PROJECT_NOT_FOUND. The map and its lists are copied, so a test that goes on
// changing its own changes nothing here.
func NewServer(t testing.TB, repositories map[string][]string) *Server {
	t.Helper()
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	s := &Server{Token: hex.EncodeToString(raw), repositories: map[string][]string{}, held: map[string]chan struct{}{}}
	s.Account = StaticAccount(s.Token)
	for project, list := range repositories {
		s.repositories[project] = append([]string{}, list...)
	}
	server := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(server.Close)
	t.Cleanup(s.releaseAll)
	s.URL = server.URL
	return s
}

// SetRepositories makes project's answer list from now on, a copy of it; an empty list is
// answered as [].
func (s *Server) SetRepositories(project string, list []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repositories[project] = append([]string{}, list...)
}

// SetDown makes every later call fail 503 (true) or be answered again (false), which is what a
// Dispatch the broker cannot reach looks like to it.
func (s *Server) SetDown(down bool) {
	if down {
		s.SetAnswer(http.StatusServiceUnavailable, `{"code":"UNAVAILABLE","error":"down"}`)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.answer = nil
}

// SetAnswer makes every later call, with the right bearer, be answered status and body as they
// are, in place of the lists: an answer that is no answer, for the client to refuse.
func (s *Server) SetAnswer(status int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.answer = &rawAnswer{status: status, body: body}
}

// SetStall makes every later call wait d before it is answered, or until its caller gives up.
func (s *Server) SetStall(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stall = d
}

// Hold makes every later call about project wait, before it is answered, until release is
// called or its caller gives up. release may be called more than once.
func (s *Server) Hold(project string) (release func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	gate := make(chan struct{})
	s.held[project] = gate
	return func() {
		// A gate is closed by whoever takes it out of the map, this or releaseAll, so it is
		// closed once whichever runs first.
		s.mu.Lock()
		taken := s.held[project] == gate
		if taken {
			delete(s.held, project)
		}
		s.mu.Unlock()
		if taken {
			close(gate)
		}
	}
}

// releaseAll lets every held call go, so the server's Close does not wait on one.
func (s *Server) releaseAll() {
	s.mu.Lock()
	gates := s.held
	s.held = map[string]chan struct{}{}
	s.mu.Unlock()
	for _, gate := range gates {
		close(gate)
	}
}

// Calls is how many calls the stand-in has taken, refusals, failures and held ones included.
func (s *Server) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// InFlight is how many calls the stand-in has taken and not yet answered.
func (s *Server) InFlight() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inFlight
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.calls++
	s.inFlight++
	stall := s.stall
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.inFlight--
		s.mu.Unlock()
	}()
	key, found := strings.CutPrefix(r.URL.Path, pathPrefix)
	if found {
		key, found = strings.CutSuffix(key, pathSuffix)
	}
	if r.Method != http.MethodGet || !found || key == "" || strings.Contains(key, "/") {
		writeJSON(w, http.StatusNotFound, map[string]string{"code": "NOT_FOUND", "error": "no such route"})
		return
	}
	// Dispatch's route refuses every credential but an access account's with 403.
	if r.Header.Get("Authorization") != "Bearer "+s.Token {
		writeJSON(w, http.StatusForbidden, map[string]string{"code": "FORBIDDEN", "error": "not an access account"})
		return
	}
	s.mu.Lock()
	gate := s.held[key]
	s.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
	}
	if stall > 0 {
		select {
		case <-time.After(stall):
		case <-r.Context().Done():
			return
		}
	}
	s.mu.Lock()
	answer := s.answer
	list, held := s.repositories[key]
	list = append([]string{}, list...)
	s.mu.Unlock()
	switch {
	case answer != nil:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(answer.status)
		_, _ = w.Write([]byte(answer.body))
	case !held:
		writeJSON(w, http.StatusNotFound, map[string]string{"code": "PROJECT_NOT_FOUND", "error": "no such project"})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"project": key, "repositories": list})
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
