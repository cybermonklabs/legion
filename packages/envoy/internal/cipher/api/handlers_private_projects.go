// handlers_private_projects.go: GET /v1/private-projects — the broker's own list of every private
// project, for the shared session watch alone. Part of the routes a listed service calls with its
// own ServiceAccount token (accountAuth), and the one route the readsPrivateList power reaches,
// which the caller name session-watch alone holds: the watch must never judge a private project's
// session, and the broker already decides with this list, so the watch reads it here rather than
// keep a copy of its own.
package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/redact"
)

// PrivateProjects is the broker's list of every private project, which GET /v1/private-projects
// answers the shared session watch (Deps.PrivateProjects). It answers every project the broker
// counts private, or an error whenever it cannot say which, never a shorter list: a short list is
// the one answer the watch cannot tell from the truth. How old a list it answers can be, while
// Dispatch answers and while it does not, is the implementation's own rule, which its doc states;
// the route adds no hold and no cache of its own.
type PrivateProjects func(ctx context.Context) ([]string, error)

// privateProjectsResponse is GET /v1/private-projects' answer.
type privateProjectsResponse struct {
	// Every project the broker counts private, sorted, each key once. An empty array when none
	// is.
	PrivateProjects []string `json:"private_projects"`
}

// privateProjects answers the shared session watch the list Deps.PrivateProjects answers, sorted
// and each key once. It keeps nothing between calls. A broker that wires no list (Deps.PrivateProjects
// nil), and a list that cannot be read, is 503 PRIVATE_LIST_UNKNOWN, never a list: the watch reads
// a 503 as no list and fails closed. A failure is named by its kind alone (redact.Class, or the
// fixed text of ErrBrokerRefused), never in the words of what failed, which can repeat the bearer
// the broker presented to Dispatch.
func (s *server) privateProjects(w http.ResponseWriter, r *http.Request, _ string) {
	if s.deps.PrivateProjects == nil {
		writeError(w, http.StatusServiceUnavailable, "PRIVATE_LIST_UNKNOWN",
			"the broker cannot say which projects are private: its list of private projects is not wired to this route")
		return
	}
	keys, err := s.deps.PrivateProjects(r.Context())
	if err != nil {
		if errors.Is(err, projectaccess.ErrBrokerRefused) {
			err = redact.Masked(err, projectaccess.ErrBrokerRefused.Error())
		} else {
			err = redact.Class(err)
		}
		s.logPrivateListUnknown(err)
		writeError(w, http.StatusServiceUnavailable, "PRIVATE_LIST_UNKNOWN", "the broker cannot say which projects are private: "+err.Error())
		return
	}
	keys = slices.Clone(keys)
	slices.Sort(keys)
	keys = slices.Compact(keys)
	if keys == nil {
		keys = []string{}
	}
	writeJSON(w, http.StatusOK, privateProjectsResponse{PrivateProjects: keys})
}

// logPrivateListUnknown writes the failure the route could not read the list for, already put in
// its kind's words, at most once per projectaccess.CacheTTL in this process however often the
// watch asks: each refusal already writes its own answer line (answer.Middleware), and this one
// adds only the kind of failure.
func (s *server) logPrivateListUnknown(err error) {
	now := time.Now().UnixNano()
	last := s.privateListLogged.Load()
	if last != 0 && now-last < int64(projectaccess.CacheTTL) {
		return
	}
	if !s.privateListLogged.CompareAndSwap(last, now) {
		return
	}
	slog.Warn("broker: the list of private projects is unknown", "error", err)
}
