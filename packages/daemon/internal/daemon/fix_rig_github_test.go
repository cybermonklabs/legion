// The GitHub stand-in of the fix rig's daemon (fix_rig_skeleton_test.go): GitHub's REST API for
// acme/widgets as that daemon reads it, serving the pull request of each issue of the rig in step
// with the events the rig sends.
package daemon

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/githubtest"
)

// fixRigBase is the commit the base branch main stands at on the stand-in, which every event of
// a pull request of the rig names as its base_sha.
const fixRigBase = "0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b"

// fixRigGitHub is GitHub's REST API as the fix rig's daemon reads it: any repository, on its
// default branch main at fixRigBase, and the create of an issue branch, which admission and the
// launches read; each issue's pull request, by its number, once the rig has opened it (open), at
// the head and with the updated_at the rig last gave it (move), mergeable and clean while it is
// open, and closed and merged at its merge commit once the rig has merged it (merge); the list of
// the open pull requests of a branch; each pull request's reviews as none; and a GraphQL root that
// knows no pull request. Every other request is answered 404, so nothing reaches github.com and a
// read the rig did not plan is named.
//
// Each request is kept, in the order it came, with its status and, for a read of a pull request,
// what it served (requests). The daemon's merge-state ladder reads the pull request alone
// (readMergeState), and its per-issue read reads it and then its reviews (readIssuePullRequest), so
// the order tells which read brought each merge state the daemon recorded.
type fixRigGitHub struct {
	t   *testing.T
	url string

	mu       sync.Mutex
	pulls    map[int]*fixRigStandInPull
	answered []fixRigRequest
}

// fixRigStandInPull is one pull request of the stand-in: its number, branch and title, whether the rig
// has opened it, its head and GitHub's clock of it, and whether and at what commit it merged.
type fixRigStandInPull struct {
	number        int
	branch, title string
	opened        bool
	head          string
	updated       time.Time
	merged        bool
	mergeSHA      string
}

// fixRigRequest is one request the stand-in answered: when, the method and the path with its
// query, the status, and for a read of a pull request what it served.
type fixRigRequest struct {
	at     time.Time
	method string
	path   string
	status int
	served string
}

func (request fixRigRequest) String() string {
	line := fmt.Sprintf("%s %s answered %d", request.method, request.path, request.status)
	if request.served != "" {
		line += ": " + request.served
	}
	return line
}

// newFixRigGitHub starts the stand-in, which holds no pull request until the rig adds one.
func newFixRigGitHub(t *testing.T) *fixRigGitHub {
	t.Helper()
	g := &fixRigGitHub{t: t, pulls: map[int]*fixRigStandInPull{}}
	g.url = githubtest.Server(t, githubtest.Spec{Handle: g.serve})
	return g
}

// add gives the stand-in pull request number of acme/widgets, on branch, titled title, which the
// rig has not opened yet.
func (g *fixRigGitHub) add(number int, branch, title string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pulls[number] = &fixRigStandInPull{number: number, branch: branch, title: title}
}

// open opens pull request number at head, GitHub's clock of it at updated.
func (g *fixRigGitHub) open(number int, head string, updated time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	pull := g.pull(number)
	pull.opened, pull.head, pull.updated = true, head, updated
}

// move moves pull request number's head to head, as a push does on GitHub, GitHub's clock of it at
// updated.
func (g *fixRigGitHub) move(number int, head string, updated time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	pull := g.pull(number)
	pull.head, pull.updated = head, updated
}

// merge merges pull request number at its head into mergeSHA, GitHub's clock of it at updated.
func (g *fixRigGitHub) merge(number int, mergeSHA string, updated time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	pull := g.pull(number)
	pull.merged, pull.mergeSHA, pull.updated = true, mergeSHA, updated
}

// pull is pull request number, which the rig must have added. Its caller holds g.mu.
func (g *fixRigGitHub) pull(number int) *fixRigStandInPull {
	pull, ok := g.pulls[number]
	if !ok {
		g.t.Fatalf("the fix rig's GitHub stand-in holds no pull request #%d", number)
	}
	return pull
}

// requests is each request the stand-in answered, in order.
func (g *fixRigGitHub) requests() []fixRigRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.answered)
}

// notAnswered is each request the stand-in answered 404, as "METHOD /path?query", in order.
func (g *fixRigGitHub) notAnswered() []string {
	var missed []string
	for _, request := range g.requests() {
		if request.status == http.StatusNotFound {
			missed = append(missed, request.method+" "+request.path)
		}
	}
	return missed
}

func (g *fixRigGitHub) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	status, served := g.answer(w, r)
	g.answered = append(g.answered, fixRigRequest{at: time.Now(), method: r.Method, path: r.URL.RequestURI(), status: status, served: served})
}

// answer writes the stand-in's answer to r, and says its status and, for a read of a pull
// request, what it served. Its caller holds g.mu.
func (g *fixRigGitHub) answer(w http.ResponseWriter, r *http.Request) (int, string) {
	rest, isRepository := strings.CutPrefix(r.URL.Path, "/repos/")
	parts := strings.SplitN(rest, "/", 3)
	tail := ""
	if len(parts) == 3 {
		tail = parts[2]
	}
	number, reviews := 0, false
	if rest, ok := strings.CutPrefix(tail, "pulls/"); ok {
		rest, reviews = strings.CutSuffix(rest, "/reviews")
		if n, err := strconv.Atoi(rest); err == nil {
			number = n
		}
	}
	pull := g.pulls[number]
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/graphql":
		_, _ = io.WriteString(w, `{"data":{"repository":{"pullRequest":null}}}`)
		return http.StatusOK, ""
	case !isRepository || len(parts) < 2:
	case r.Method == http.MethodGet && len(parts) == 2:
		g.write(w, map[string]any{"full_name": parts[0] + "/" + parts[1], "default_branch": "main"})
		return http.StatusOK, ""
	case parts[0]+"/"+parts[1] != liveRepo:
	case r.Method == http.MethodGet && tail == "git/ref/heads/main":
		g.write(w, map[string]any{"ref": "refs/heads/main", "object": map[string]string{"sha": fixRigBase, "type": "commit"}})
		return http.StatusOK, ""
	case r.Method == http.MethodPost && tail == "git/refs":
		var create struct {
			Ref string `json:"ref"`
		}
		_ = json.NewDecoder(r.Body).Decode(&create)
		w.WriteHeader(http.StatusCreated)
		g.write(w, map[string]any{"ref": create.Ref, "object": map[string]string{"sha": fixRigBase}})
		return http.StatusCreated, ""
	case r.Method == http.MethodGet && tail == "pulls":
		// The daemon lists the open pull request of a branch (openPullRequestOn), naming it as
		// `<owner>:<branch>`: each pull request of that branch the rig opened and did not merge.
		_, branch, named := strings.Cut(r.URL.Query().Get("head"), ":")
		listed := []any{}
		for _, number := range slices.Sorted(maps.Keys(g.pulls)) {
			if open := g.pulls[number]; open.opened && !open.merged && (!named || open.branch == branch) {
				listed = append(listed, open.answer())
			}
		}
		g.write(w, listed)
		return http.StatusOK, fmt.Sprintf("%d open pull request(s)", len(listed))
	case r.Method == http.MethodGet && pull != nil && pull.opened && !reviews:
		answer := pull.answer()
		g.write(w, answer)
		return http.StatusOK, fmt.Sprintf("#%d state %s, head %s, mergeable_state %s, updated_at %s", number, answer["state"], pull.head,
			answer["mergeable_state"], answer["updated_at"])
	case r.Method == http.MethodGet && pull != nil && pull.opened && reviews:
		g.write(w, []any{})
		return http.StatusOK, fmt.Sprintf("#%d no review", number)
	}
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	return http.StatusNotFound, ""
}

// answer is the pull request as GitHub's REST API answers it now: open, mergeable and clean at its
// head, or closed and merged at its merge commit, where GitHub answers `unknown` for good.
func (p *fixRigStandInPull) answer() map[string]any {
	state, mergeState := "open", "clean"
	if p.merged {
		state, mergeState = "closed", "unknown"
	}
	return map[string]any{
		"number": p.number, "state": state, "title": p.title, "body": "",
		"html_url":   fmt.Sprintf("https://github.com/%s/pull/%d", liveRepo, p.number),
		"base":       map[string]string{"ref": "main", "sha": fixRigBase},
		"head":       map[string]any{"ref": p.branch, "sha": p.head, "repo": map[string]string{"full_name": liveRepo}},
		"updated_at": p.updated.UTC().Format(time.RFC3339), "mergeable": true, "mergeable_state": mergeState,
		"merged": p.merged, "merge_commit_sha": p.mergeSHA,
	}
}

// write encodes value as the answer.
func (g *fixRigGitHub) write(w http.ResponseWriter, value any) {
	if err := json.NewEncoder(w).Encode(value); err != nil {
		g.t.Errorf("write the fix rig's GitHub stand-in's answer: %v", err)
	}
}
