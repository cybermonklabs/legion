package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// linkedWalk is a walk of the step records' rig (stepRecordsRig) whose Dispatch is the real one,
// publishing its events on the daemon's broker, behind a proxy that answers the webhook App check
// the scratch Dispatch has no GitHub App to answer, and whose GitHub holds a pull request of a
// second repository, linkedNumber, that the walk's issue links once it stands in merging.
type linkedWalk struct {
	*stepRecordsRig
}

const linkedBranch = "fix/deploy"

// linkedNumber is the number of the pull request of the second repository, linkedDeploy, that the
// walks of the rig link: a number no pull request of the issue's own repository holds.
func (r *stepRecordsRig) linkedNumber() int {
	return r.number + 7000000
}

func newLinkedWalk(t *testing.T) *linkedWalk {
	t.Helper()
	// The broker is taken first: testnats.URL empties it of every stream, and the scratch Dispatch
	// reconciles ENVOY_NOTIFICATIONS on it, which the daemon then consumes Dispatch's events from.
	natsURL := testnats.URL(t)
	through := behindTheWebhookCheck(t, startScratchDispatchOn(t, scratchDispatchOptions{natsURL: natsURL}))
	walk := &linkedWalk{stepRecordsRig: newStepRecordsRigWith(t, newLiveProofWith(t, natsURL, false, true), through, 1)}
	walk.github.openIn(linkedDeploy, linkedBranch, walk.linkedNumber())
	return walk
}

// behindTheWebhookCheck is scratch as the daemon reaches it through a proxy that answers the check
// of Dispatch's webhook App (checkWebhookApp) as installed on every repository, which the scratch
// Dispatch, holding no GitHub App, cannot answer: a repository only a link names is otherwise held
// unserved (linkUnserved). Every other request reaches scratch as it was sent.
func behindTheWebhookCheck(t *testing.T, scratch *scratchDispatch) *scratchDispatch {
	t.Helper()
	target, err := url.Parse(scratch.base)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/github/app-installation/") {
			_ = json.NewEncoder(w).Encode(map[string]bool{"installed": true})
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	through := *scratch
	through.base = front.URL
	return &through
}

// toMerging walks the issue to merging, with its own pull request open, as the walk of the step
// records does.
func (w *linkedWalk) toMerging() {
	w.plan()
	w.implement()
	w.test()
	w.review("APPROVED", phase.Accepting)
	w.accept()
	w.retro()
}

// link has Dispatch link the issue to the second repository's pull request, as a person does, and
// waits for the daemon to hold the pull request in the issue's set.
func (w *linkedWalk) link() {
	w.t.Helper()
	w.linkTo(linkedDeploy, w.linkedNumber())
}

// linkTo has Dispatch link the walk's issue to pull request number of repo, as a person does,
// and waits for the daemon to hold the pull request open in the issue's set.
func (r *stepRecordsRig) linkTo(repo string, number int) {
	r.t.Helper()
	r.person(http.MethodPatch, "/api/v1/issues/"+r.key, map[string]any{"external_links": []map[string]string{
		{"url": fmt.Sprintf("https://github.com/%s/pull/%d", repo, number)}}}, http.StatusOK, nil)
	testwait.WithinPolling(r.t, stepRecordsWait, 100*time.Millisecond, "the daemon to read the linked pull request", func() bool {
		return r.pullState(repo, number) == "open"
	})
}

// pullState is the state the daemon's record holds of pull request number of repo, "" for none.
func (r *stepRecordsRig) pullState(repo string, number int) string {
	var state string
	err := r.st.Pool().QueryRow(context.Background(), `select state from pull_requests where lower(repo) = lower($1) and number = $2`, repo, number).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return ""
	}
	if err != nil {
		r.t.Fatalf("read the state of %s#%d: %v", repo, number, err)
	}
	return state
}

// mergeOwn is GitHub merging the issue's own pull request, and the daemon recording it.
func (w *linkedWalk) mergeOwn() {
	w.t.Helper()
	w.github.merged(w.number)
	w.publishMergeOf(w.number, w.key, stepRecordsHead, "main")
	testwait.WithinPolling(w.t, stepRecordsWait, 100*time.Millisecond, "the daemon to record the merge of the own pull request", func() bool {
		return w.pullState(liveRepo, w.number) == "merged"
	})
}

// mergeLinked is GitHub merging the linked pull request, as Envoy relays it from acme/deploy.
func (w *linkedWalk) mergeLinked() {
	w.t.Helper()
	w.github.merged(w.linkedNumber())
	w.linkedMerged(w.linkedNumber(), linkedBranch, stepRecordsHead)
}

// linkedMerged is Envoy relaying GitHub's merge of pull request number of the second repository,
// linkedDeploy, from branch at head, which GitHub already answers merged; it waits for the daemon
// to record the merge.
func (r *stepRecordsRig) linkedMerged(number int, branch, head string) {
	r.t.Helper()
	payload, err := json.Marshal(map[string]string{"action": "closed", "kind": "pr", "repo": linkedDeploy, "number": fmt.Sprint(number), "merged": "true",
		"merge_commit_sha": "merge-" + head, "head_sha": head, "base_ref": "main", "head_ref": branch, "head_repo": linkedDeploy,
		"updated_at": time.Now().UTC().Format(time.RFC3339)})
	if err != nil {
		r.t.Fatal(err)
	}
	r.publish("notifications.github."+strings.ReplaceAll(linkedDeploy, "/", ".")+".pr."+fmt.Sprint(number), "github", fmt.Sprintf("pr-%d-merged", number), string(payload))
	testwait.WithinPolling(r.t, stepRecordsWait, 100*time.Millisecond, "the daemon to record the merge of the linked pull request", func() bool {
		return r.pullState(linkedDeploy, number) == "merged"
	})
}

// stands fails the test unless GET /legion/v1/state shows the issue in want.
func (r *stepRecordsRig) stands(after string, want phase.Phase) {
	r.t.Helper()
	got, ok := r.d.state().Issues[r.key]
	if !ok || string(got.Phase) != string(want) {
		r.t.Fatalf("after %s GET /legion/v1/state shows %s in %q, want %s", after, r.key, got.Phase, want)
	}
}

// An issue with two pull requests in two repositories is done at its last merge, whichever merges
// last and across a restart (dispatch://INFRA-5910 D2, D4). One issue walks to merging with its own
// pull request on acme/widgets, a person links a pull request of acme/deploy to it in Dispatch, and
// the two merge one after the other through GitHub's events, GET /legion/v1/state read after each
// merge: the issue is in merging after the first and done after the second. The walk's project has
// no production check, so the second merge closes it. It runs the own pull request first, the
// linked one first, and the own first with the daemon restarted between the merges.
func TestTwoPullRequestsInTwoRepositoriesCloseTheIssueAtTheLastMerge(t *testing.T) {
	t.Parallel()
	for _, order := range []struct {
		name                string
		linkedFirst, reboot bool
	}{
		{name: "the own pull request merges first"},
		{name: "the linked pull request merges first", linkedFirst: true},
		{name: "a restart between the merges", reboot: true},
	} {
		t.Run(order.name, func(t *testing.T) {
			w := newLinkedWalk(t)
			w.toMerging()
			w.link()
			first, second := w.mergeOwn, w.mergeLinked
			if order.linkedFirst {
				first, second = second, first
			}
			first()
			w.stands("the first merge", phase.Merging)
			if order.reboot {
				w.restart()
				w.stands("the restart", phase.Merging)
			}
			second()
			w.waitPhase(phase.Done)
			w.stands("the second merge", phase.Done)
		})
	}
}
