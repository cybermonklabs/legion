// Package projectaccesstest is a stand-in for Dispatch's access routes, the ones the broker's
// clients of that Dispatch ask: whether a person may see a project and what kind of member a
// person is (projectaccess), and which repositories a project's issues resolve to (projectrepos).
// One stand-in serves all three, from the same rows, so a test points both clients at one Dispatch
// as a deployment does. It holds every caller to the one bearer it mints, counts the calls it
// answers, and can be made to fail, stall or answer bytes of a test's choosing, so a test can show
// what the broker does when Dispatch cannot be reached or gives no whole answer. It is an ordinary
// package rather than a _test.go file so every broker package's tests, and the real binary's, can
// serve it.
//
// It spells the wire on its own, field by field, rather than encoding the clients' own types: a
// client that read a field under a name Dispatch does not send would then disagree with this
// stand-in, where one declaration shared by both would agree with itself and prove nothing.
package projectaccesstest

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// The paths of Dispatch's access routes, as projectaccess and projectrepos ask them.
const (
	projectPath        = "/api/v1/access/projects/"
	repositoriesSuffix = "/repositories"
	personPath         = "/api/v1/access/person"
)

// Row is one project as Dispatch holds it: whether it is public or private, its owner, the
// people its access list names, and two lists of repositories Dispatch keeps apart: the ones its
// membership answer maps to the project, and the ones its repository answer says the project's
// issues resolve to.
type Row struct {
	// Visibility is "public" or "private".
	Visibility string
	// Owner is the owner's lowercase login, who sees the project whatever else is true.
	Owner string
	// Listed are the lowercase logins the project's access list names.
	Listed []string
	// Repositories are the "owner/name" repositories Dispatch maps to the project, which the
	// membership answer carries.
	Repositories []string
	// Resolved are the "owner/name" repositories the project's issues resolve to, which the
	// repository answer carries, sorted.
	Resolved []string
	// SlackChannels are the Slack channel ids the project's owner named. A real Dispatch always
	// carries the key, empty when the project names none, so this stand-in does too — unless
	// OmitSlackChannels, which is a Dispatch from before dispatch://INFRA-3862's #t-a-31 and
	// carries no such key at all.
	SlackChannels []string
	// OmitSlackChannels makes this project's answer carry no slack_channels key, the shape a
	// Dispatch from before #t-a-31 answers.
	OmitSlackChannels bool
}

// Server is the stand-in: URL is its base, Token the one bearer it takes, and Account that bearer
// as the reader the broker's client presents.
type Server struct {
	// URL is the stand-in's base URL, what BROKER_PROJECT_ACCESS_URL names.
	URL string
	// Token is the one bearer the stand-in takes; every other bearer is refused 403.
	Token string
	// Account reads Token, as ciphertoken.AccountToken reads a projected token from its file.
	Account StaticAccount

	mu     sync.Mutex
	rows   map[string]Row
	people map[string]string
	calls  int
	down   bool
	answer *rawAnswer
	stall  time.Duration
}

// rawAnswer is a status and body the stand-in answers every call with in place of its rows.
type rawAnswer struct {
	status int
	body   string
}

// StaticAccount is a bearer that never rotates: a ciphertoken.Account for a test.
type StaticAccount string

// Read answers the bearer.
func (a StaticAccount) Read() (string, error) { return string(a), nil }

// NewServer serves rows (by project key) and people (lowercase login to member kind: "full" or
// "restricted"; a login it does not hold is "none") until t ends. The maps are copied, so a test
// that goes on changing its own changes nothing here.
func NewServer(t testing.TB, rows map[string]Row, people map[string]string) *Server {
	t.Helper()
	var bearer [16]byte
	if _, err := rand.Read(bearer[:]); err != nil {
		t.Fatalf("mint the stand-in's bearer: %v", err)
	}
	s := &Server{Token: "dispatch-access-" + hex.EncodeToString(bearer[:]), rows: map[string]Row{}, people: map[string]string{}}
	for key, row := range rows {
		row.Listed, row.Repositories, row.Resolved = slices.Clone(row.Listed), slices.Clone(row.Repositories), slices.Clone(row.Resolved)
		row.SlackChannels = slices.Clone(row.SlackChannels)
		s.rows[key] = row
	}
	for login, kind := range people {
		s.people[login] = kind
	}
	s.Account = StaticAccount(s.Token)
	server := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(server.Close)
	s.URL = server.URL
	return s
}

// Calls is how many requests the stand-in has answered, refusals and the ones it failed while
// down included.
func (s *Server) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// SetDown makes every later request fail 503 (true) or be answered again (false), which is what a
// Dispatch the broker cannot reach looks like to it.
func (s *Server) SetDown(down bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.down = down
}

// SetRepositories makes the list project's issues resolve to, which the repository route answers,
// a copy of list from now on; an empty list is answered as []. The project's mapped list, which
// the membership answer carries, does not change.
func (s *Server) SetRepositories(project string, list []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.rows[project]
	row.Resolved = slices.Clone(list)
	s.rows[project] = row
}

// SetAnswer makes every later request with the right bearer be answered status and body as they
// are, in place of the rows: an answer that is no answer, for a client to refuse.
func (s *Server) SetAnswer(status int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.answer = &rawAnswer{status: status, body: body}
}

// SetStall makes every later request wait d before it is answered, or until its caller gives up.
func (s *Server) SetStall(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stall = d
}

// SetRow makes Dispatch hold row for key from the next request on, as an owner's change of a
// project's visibility or access list does, so a test can change Dispatch under a client whose
// cache already holds the earlier answer.
func (s *Server) SetRow(key string, row Row) {
	row.Listed, row.Repositories, row.Resolved = slices.Clone(row.Listed), slices.Clone(row.Repositories), slices.Clone(row.Resolved)
	row.SlackChannels = slices.Clone(row.SlackChannels)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[key] = row
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.calls++
	down, canned, stall := s.down, s.answer, s.stall
	s.mu.Unlock()
	if down {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"code": "UNAVAILABLE", "error": "the stand-in is down"})
		return
	}
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"code": "METHOD_NOT_ALLOWED", "error": r.Method})
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+s.Token {
		writeJSON(w, http.StatusForbidden, map[string]any{"code": "ACCESS_ACCOUNT_REQUIRED", "error": "the stand-in takes its own bearer alone"})
		return
	}
	if stall > 0 {
		select {
		case <-time.After(stall):
		case <-r.Context().Done():
			return
		}
	}
	if canned != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(canned.status)
		_, _ = w.Write([]byte(canned.body))
		return
	}
	person := r.URL.Query().Get("person")
	key, project := strings.CutPrefix(r.URL.Path, projectPath)
	key, repositories := strings.CutSuffix(key, repositoriesSuffix)
	switch {
	case r.URL.Path == personPath:
		if person == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"code": "INVALID_QUERY", "error": "person is required"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"person": person, "member_kind": s.kind(person)})
	case project:
		row, held := s.row(key)
		if !held || key == "" || strings.Contains(key, "/") {
			writeJSON(w, http.StatusNotFound, map[string]any{"code": "PROJECT_NOT_FOUND", "error": "no such project"})
			return
		}
		if repositories {
			// Dispatch's repository answer: the project and the list its issues resolve to,
			// sorted byte-wise, [] for none. The mapped list is the membership answer's alone.
			resolved := append([]string{}, row.Resolved...)
			slices.Sort(resolved)
			writeJSON(w, http.StatusOK, map[string]any{"project": key, "repositories": resolved})
			return
		}
		list := row.Repositories
		if list == nil {
			list = []string{}
		}
		answer := map[string]any{
			"project":      key,
			"visibility":   row.Visibility,
			"owner":        row.Owner,
			"person":       person,
			"member_kind":  s.kind(person),
			"visible":      s.visible(row, person),
			"repositories": list,
		}
		if !row.OmitSlackChannels {
			channels := row.SlackChannels
			if channels == nil {
				channels = []string{}
			}
			answer["slack_channels"] = channels
		}
		writeJSON(w, http.StatusOK, answer)
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"code": "NOT_FOUND", "error": r.URL.Path})
	}
}

// row is the project key's row, and whether the stand-in holds it.
func (s *Server) row(key string) (Row, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, held := s.rows[key]
	return row, held
}

// kind is a login's member kind, "none" for a login the stand-in does not hold or no login at all.
func (s *Server) kind(person string) string {
	if kind, held := s.people[person]; held && person != "" {
		return kind
	}
	return "none"
}

// visible is Dispatch's rule as the spec states it: a person sees a project its access list names
// them on (its owner too), or a public project when they are not a restricted member; a query
// naming no person sees the public projects alone.
func (s *Server) visible(row Row, person string) bool {
	if person == "" {
		return row.Visibility == "public"
	}
	if person == row.Owner || slices.Contains(row.Listed, person) {
		return true
	}
	return row.Visibility == "public" && s.kind(person) != "restricted"
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
