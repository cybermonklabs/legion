package githubtest

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
)

// Refs is a stand-in of the branches of repositories on GitHub, for the code that lists and
// deletes them: it serves GET /repos/{owner}/{repo}/git/matching-refs/heads/{prefix}, the exact
// GET /repos/{owner}/{repo}/git/ref/heads/{branch} and DELETE
// /repos/{owner}/{repo}/git/refs/heads/{branch} from the branches Set gives it, as GitHub answers
// them: a list in name order, one ref or 404, and 204 or GitHub's 422 "Reference does not exist"
// for a branch it does not hold. Fail plans one answer of its own for the next request of a method
// and a branch. It keeps each request it answers (Requests).
type Refs struct {
	mu       sync.Mutex
	branches map[string]map[string]string
	planned  map[refKey][]plannedAnswer
	requests []RefRequest
}

// RefRequest is one request Refs answered: its method, the repository and the path under
// /repos/{owner}/{repo}, and the status it answered.
type RefRequest struct {
	Method, Repo, Path string
	Status             int
}

// refKey is one method on one branch of one repository.
type refKey struct{ method, repo, branch string }

// plannedAnswer is a status Refs answers instead of its own, and whether the branch is gone after
// it: a 422 GitHub answered for a branch someone deleted at the same time leaves it gone.
type plannedAnswer struct {
	status int
	remove bool
}

// NewRefs is a Refs that holds no branch.
func NewRefs() *Refs {
	return &Refs{branches: map[string]map[string]string{}, planned: map[refKey][]plannedAnswer{}}
}

// Set makes branch of repo, owner/name, stand at sha.
func (r *Refs) Set(repo, branch, sha string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.branches[repo] == nil {
		r.branches[repo] = map[string]string{}
	}
	r.branches[repo][branch] = sha
}

// Fail plans the next request of method on branch of repo to be answered status, and removes the
// branch with that answer where remove is set. Plans of one method and branch are answered in the
// order they were made.
func (r *Refs) Fail(method, repo, branch string, status int, remove bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := refKey{method, repo, branch}
	r.planned[key] = append(r.planned[key], plannedAnswer{status: status, remove: remove})
}

// Branches is every branch of repo whose name starts with prefix, in name order.
func (r *Refs) Branches(repo, prefix string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.matching(repo, prefix)
}

// Requests is every request Refs answered, in order.
func (r *Refs) Requests() []RefRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.requests)
}

// Serve starts a fake GitHub that answers the refs paths from r and fails the test at any other
// request, and returns its API root.
func (r *Refs) Serve(t *testing.T) string {
	t.Helper()
	return Server(t, Spec{Handle: func(w http.ResponseWriter, req *http.Request) {
		if !r.Handle(w, req) {
			Unexpected(t, w, req)
		}
	}})
}

// Handle answers req where it is one of the refs requests Refs serves, and answers false, having
// written nothing, where it is not, so a fake of more paths can serve these beside its own.
func (r *Refs) Handle(w http.ResponseWriter, req *http.Request) bool {
	repo, rest, ok := strings.Cut(strings.TrimPrefix(req.URL.Path, "/repos/"), "/git/")
	if !ok || !strings.HasPrefix(req.URL.Path, "/repos/") || strings.Count(repo, "/") != 1 {
		return false
	}
	var answer func() (int, any)
	switch {
	case req.Method == http.MethodGet && strings.HasPrefix(rest, "matching-refs/heads/"):
		prefix := strings.TrimPrefix(rest, "matching-refs/heads/")
		answer = func() (int, any) {
			refs := []refObject{}
			for _, branch := range r.matching(repo, prefix) {
				refs = append(refs, refOf(branch, r.branches[repo][branch]))
			}
			return http.StatusOK, refs
		}
	case req.Method == http.MethodGet && strings.HasPrefix(rest, "ref/heads/"):
		branch := strings.TrimPrefix(rest, "ref/heads/")
		answer = func() (int, any) {
			if status, planned := r.plannedFor(http.MethodGet, repo, branch); planned {
				return status, message(status)
			}
			sha, held := r.branches[repo][branch]
			if !held {
				return http.StatusNotFound, map[string]string{"message": "Not Found"}
			}
			return http.StatusOK, refOf(branch, sha)
		}
	case req.Method == http.MethodDelete && strings.HasPrefix(rest, "refs/heads/"):
		branch := strings.TrimPrefix(rest, "refs/heads/")
		answer = func() (int, any) {
			if status, planned := r.plannedFor(http.MethodDelete, repo, branch); planned {
				return status, message(status)
			}
			if _, held := r.branches[repo][branch]; !held {
				return http.StatusUnprocessableEntity, map[string]string{"message": "Reference does not exist"}
			}
			delete(r.branches[repo], branch)
			return http.StatusNoContent, nil
		}
	default:
		return false
	}
	r.mu.Lock()
	status, body := answer()
	r.requests = append(r.requests, RefRequest{Method: req.Method, Repo: repo, Path: "/git/" + rest, Status: status})
	r.mu.Unlock()
	w.WriteHeader(status)
	if body != nil {
		_ = json.NewEncoder(w).Encode(body)
	}
	return true
}

// plannedFor takes the next answer Fail planned for method on branch of repo, removing the branch
// where the plan says so; false where none is planned. r.mu is held.
func (r *Refs) plannedFor(method, repo, branch string) (int, bool) {
	key := refKey{method, repo, branch}
	plans := r.planned[key]
	if len(plans) == 0 {
		return 0, false
	}
	r.planned[key] = plans[1:]
	if plans[0].remove {
		delete(r.branches[repo], branch)
	}
	return plans[0].status, true
}

// matching is every branch of repo that starts with prefix, in name order. r.mu is held.
func (r *Refs) matching(repo, prefix string) []string {
	var names []string
	for branch := range r.branches[repo] {
		if strings.HasPrefix(branch, prefix) {
			names = append(names, branch)
		}
	}
	slices.Sort(names)
	return names
}

// refObject is a ref as GitHub's git refs API answers it.
type refObject struct {
	Ref    string `json:"ref"`
	Object struct {
		SHA  string `json:"sha"`
		Type string `json:"type"`
	} `json:"object"`
}

// refOf is branch at sha as GitHub answers it.
func refOf(branch, sha string) refObject {
	ref := refObject{Ref: "refs/heads/" + branch}
	ref.Object.SHA, ref.Object.Type = sha, "commit"
	return ref
}

// message is the body GitHub sends with a planned status.
func message(status int) map[string]string {
	return map[string]string{"message": http.StatusText(status)}
}
