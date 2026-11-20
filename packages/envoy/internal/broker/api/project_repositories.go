package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sync"
)

// EdgeListener is one edge listener's entry of BROKER_NATS_SERVICES as the broker's API reads it
// (natscallout.EdgeListeners): the platform account its token names, whether that is a Google
// account, and the projects the entry lists, sorted.
type EdgeListener struct {
	Account  string
	Google   bool
	Projects []string
}

// ProjectRepositoriesReader reads Dispatch's answer to which repositories a project's issues
// resolve to, fresh on every call (projectrepos.Client): each as "owner/name", an empty list and
// never nil for none, and an error, with no list, whenever Dispatch gave no whole answer.
type ProjectRepositoriesReader interface {
	Repositories(ctx context.Context, project string) ([]string, error)
}

// maxRepositoryCallsInFlight is how many calls of one edge listener may wait on Dispatch at once
// on one broker process; the next is refused at once rather than queued behind them. It is a
// guard on Dispatch's load and not a security bound, and it is each process's: N replicas let one
// edge listener hold 16 × N calls on Dispatch.
const maxRepositoryCallsInFlight = 16

// projectRepositoriesResponse is GET /v1/project-repositories's answer: Dispatch's, relayed as it
// came.
type projectRepositoriesResponse struct {
	// The project asked about.
	Project string `json:"project"`
	// Each repository, as owner/name, that an issue of the project resolves to, as Dispatch lists
	// them: sorted, each once, and [] for none.
	Repositories []string `json:"repositories"`
}

// projectRepositories answers edge, an edge listener, the repositories the issues of the one
// project ?project= names resolve to, read from Dispatch now (Deps.ProjectRepositories). The
// project must be one its entry lists, which is decided before Dispatch is asked. No answer is
// kept, and no failure to read one becomes a list: a broker that reads no Dispatch, and every
// read that fails, is 503 REPOSITORIES_UNKNOWN, so the edge listener relays no repository's events
// it has not just been told of.
func (s *server) projectRepositories(w http.ResponseWriter, r *http.Request, edge EdgeListener) {
	projects := r.URL.Query()["project"]
	if len(projects) != 1 || projects[0] == "" {
		writeError(w, http.StatusBadRequest, "PROJECT_REQUIRED", "name exactly one project, as ?project=<KEY>")
		return
	}
	project := projects[0]
	if !slices.Contains(edge.Projects, project) {
		writeError(w, http.StatusForbidden, "PROJECT_NOT_LISTED", "the caller's edge-listener entry of BROKER_NATS_SERVICES does not list this project")
		return
	}
	if s.deps.ProjectRepositories == nil {
		writeError(w, http.StatusServiceUnavailable, "REPOSITORIES_UNKNOWN", "this broker reads no Dispatch (BROKER_PROJECT_ACCESS_URL is unset), so it cannot say which repositories the project's issues resolve to")
		return
	}
	if !s.repositoryCalls.enter(edge.Account) {
		writeError(w, http.StatusServiceUnavailable, "REPOSITORIES_BUSY",
			fmt.Sprintf("this edge listener already has %d calls waiting on Dispatch; send this one again once one is answered", maxRepositoryCallsInFlight))
		return
	}
	defer s.repositoryCalls.leave(edge.Account)
	repositories, err := s.deps.ProjectRepositories.Repositories(r.Context(), project)
	if err != nil {
		writeUnavailable(w, "REPOSITORIES_UNKNOWN", "read the project's repositories from Dispatch", err)
		return
	}
	writeJSON(w, http.StatusOK, projectRepositoriesResponse{Project: project, Repositories: repositories})
}

// callsInFlight counts each caller's calls still waiting on a dependency, and refuses one past the
// bound.
type callsInFlight struct {
	mu    sync.Mutex
	limit int
	calls map[string]int
}

func newCallsInFlight(limit int) *callsInFlight {
	return &callsInFlight{limit: limit, calls: map[string]int{}}
}

// enter counts one more call of caller's, or reports false, counting nothing, when caller already
// has limit in flight.
func (c *callsInFlight) enter(caller string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.calls[caller] >= c.limit {
		return false
	}
	c.calls[caller]++
	return true
}

// leave counts one of caller's calls answered.
func (c *callsInFlight) leave(caller string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls[caller]--
	if c.calls[caller] <= 0 {
		delete(c.calls, caller)
	}
}
