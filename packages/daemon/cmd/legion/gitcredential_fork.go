package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/ghroute"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/programbound"
)

// forkReadBound is the bound of the credential helper's whole fork read, the token read included:
// the token read is a file and the fork read one request to GitHub, and the bound keeps the git
// that asked for a credential from waiting without end on a read that does not answer. Tests
// shorten it, and give it their own deadline otherwise (programbound).
var forkReadBound = 10 * time.Second

func init() {
	programbound.Register(&forkReadBound)
}

// forkUpstreamDir is the gh directory that answers for repo, a repository of an owner the bundle
// under ownersDir holds no directory for (ghroute.ForkRepository), where GitHub says repo is a
// fork of a repository the bundle serves. Such a fork is where an outside author's pull request to
// a served repository keeps its head. The token of the served repository's owner is the one a push
// to that head can carry, and GitHub, not this helper, decides which branch of the fork it may
// update.
//
// It reads repo from GitHub once: `GET /repos/<owner>/<name>` through the leak guard's curl read
// (leakguard's Guard.CurlGet), with the token of GH_CONFIG_DIR's directory (forkReadToken). The
// read runs within forkReadBound, the token read included, and inside ctx. Where GitHub answers
// that repo is a fork, the answer is the directory ghroute.DirFor routes the fork's parent to
// where the bundle holds a directory for the parent's owner, else the one it routes the fork's
// source to where the bundle holds a directory for the source's owner (ghroute.ForkDir), with
// route, the words that say how: `the fork <repo> routes to <dir> through its parent <full
// name>`, or `through its source <full name>`. The caller then reads that directory's token as it
// reads every answer's, and puts route in front of a failure of that read, whose own error names
// the directory alone and so neither the fork nor which of its two upstreams led there.
//
// Anything else is an error that holds refused, ghroute.DirFor's refusal of the request, and then
// what the read found, on one line: no token to read with, a read that could not be made or did
// not finish, any answer of GitHub's but a 200 for repo, a repository that is no fork, a fork
// whose upstream that answers, its parent or else its source, routes nowhere (a parent that does
// leaves the source untried), and a fork whose parent's and source's owners the bundle holds no
// directory for. No second read is made, and no error holds a token.
func forkUpstreamDir(ctx context.Context, ownersDir string, repo ghrepo.Repository, refused error) (dir, route string, err error) {
	dir, route, err = readForkUpstream(ctx, ownersDir, repo)
	if err != nil {
		// A line break in the reason, such as one of curl's own, would make git show the refusal
		// as two lines.
		reason := strings.Join(strings.FieldsFunc(err.Error(), func(r rune) bool { return r == '\n' || r == '\r' }), " ")
		return "", "", fmt.Errorf("%w, and %s", refused, reason)
	}
	return dir, route, nil
}

// readForkUpstream is forkUpstreamDir's read and its answer, the directory and the route to it
// (ghroute.ForkDir), the error saying why there is none. A SIGHUP cancels the read
// (hangupCancels), so a pane that closes while git waits on the helper stops curl's process group,
// as `legion gh` and `legion leak-check` stop theirs, rather than killing the helper and leaving
// curl running until its --max-time.
func readForkUpstream(ctx context.Context, ownersDir string, repo ghrepo.Repository) (dir, route string, err error) {
	ctx, stopHangup := hangupCancels(ctx)
	defer stopHangup()
	ctx, cancel := context.WithTimeout(ctx, programbound.Of(&forkReadBound))
	defer cancel()
	token, err := forkReadToken(ownersDir, repo)
	if err != nil {
		return "", "", err
	}
	response, err := leakguard.New(leakguard.Options{Curl: leakguardCurl}).CurlGet(ctx, "repos/"+repo.String(), token)
	if err != nil {
		return "", "", fmt.Errorf("the helper could not read %s from GitHub: %w", repo, err)
	}
	answer, err := readForkAnswer(response, repo)
	if err != nil {
		return "", "", err
	}
	return ghroute.ForkDir(ownersDir, repo, answer.Parent.FullName, answer.Source.FullName)
}

// forkReadToken is the token the fork read of repo carries: the github.com token of
// GH_CONFIG_DIR's hosts.yml, read as every answer's token is (ghDirToken). GH_CONFIG_DIR must name
// a directory of the bundle under ownersDir, one owner's directory directly under its root, so
// the read carries a lease of the role's own App and no other credential the environment names.
// An unset GH_CONFIG_DIR, one outside the bundle, and a hosts.yml that cannot be read or names no
// token are each an error naming which, and no read is made.
func forkReadToken(ownersDir string, repo ghrepo.Repository) (string, error) {
	configDir := os.Getenv("GH_CONFIG_DIR")
	if configDir == "" {
		return "", fmt.Errorf("GH_CONFIG_DIR is unset, so there is no token to read %s from GitHub with", repo)
	}
	dir := filepath.Clean(configDir)
	if filepath.Dir(dir) != filepath.Clean(ownersDir) || !ghconfig.IsOwner(filepath.Base(dir)) {
		return "", fmt.Errorf("GH_CONFIG_DIR %s is no directory of the bundle %s, so the helper does not read %s from GitHub with its token",
			configDir, ownersDir, repo)
	}
	token, err := ghDirToken(dir)
	if err != nil {
		return "", fmt.Errorf("there is no token to read %s from GitHub with: %w", repo, err)
	}
	return token, nil
}

// forkAnswer is what the fork read takes of GitHub's answer to `GET /repos/<owner>/<name>`: the
// repository's own full name; whether it is a fork, nil where the answer holds no such field; and
// the full names of the repository it was forked from (its parent) and of the root of its fork
// network (its source), which GitHub gives for a fork alone and ghroute.ForkDir routes it by.
// Message is the one field of an error answer it reads.
type forkAnswer struct {
	FullName string       `json:"full_name"`
	Fork     *bool        `json:"fork"`
	Parent   forkUpstream `json:"parent"`
	Source   forkUpstream `json:"source"`
	Message  string       `json:"message"`
}

// forkUpstream is a fork's parent or source as GitHub's answer names it.
type forkUpstream struct {
	FullName string `json:"full_name"`
}

// readForkAnswer is GitHub's answer to the fork read of repo, where it is a 200 that says repo is
// a fork, and an error naming the answer otherwise. In order: a curl that got no answer
// (CurlResponse.Unreached) or that failed after it heard one, a rate limit, with its reset or its
// wait (CurlResponse.RateLimit), any status but 200, with GitHub's message and, for a redirect,
// where it points, since curl follows none; and, of a 200, a body that is not JSON, an answer for
// another repository than repo (a full_name that differs from it beyond case), and an answer
// with no "fork" field or a false one.
func readForkAnswer(response leakguard.CurlResponse, repo ghrepo.Repository) (forkAnswer, error) {
	if response.Exit != 0 {
		if unreached := response.Unreached(); unreached != nil {
			return forkAnswer{}, fmt.Errorf("the read of %s failed: %w", repo, unreached)
		}
		return forkAnswer{}, fmt.Errorf("curl's read of %s exited %d: %s", repo, response.Exit, strings.TrimSpace(response.Stderr))
	}
	if limited := response.RateLimit(); limited != nil {
		return forkAnswer{}, fmt.Errorf("GitHub refused the read of %s: %w", repo, limited)
	}
	status, headers := response.Status()
	if status == 0 {
		return forkAnswer{}, fmt.Errorf("curl printed no status line for the read of %s", repo)
	}
	var answer forkAnswer
	decodeErr := json.Unmarshal([]byte(response.Body()), &answer)
	if status != http.StatusOK {
		said := fmt.Sprintf("GitHub answered the read of %s with the status %d", repo, status)
		if answer.Message != "" {
			said += " (" + answer.Message + ")"
		}
		if location := headers["location"]; location != "" {
			said += ", pointing at " + location + ", a redirect the helper does not follow"
		}
		return forkAnswer{}, errors.New(said)
	}
	switch {
	case decodeErr != nil:
		return forkAnswer{}, fmt.Errorf("GitHub's answer for %s is not JSON of one repository: %v", repo, decodeErr)
	case !strings.EqualFold(answer.FullName, repo.String()):
		return forkAnswer{}, fmt.Errorf("GitHub answered the read of %s with another repository, %q", repo, answer.FullName)
	case answer.Fork == nil:
		return forkAnswer{}, fmt.Errorf(`GitHub's answer for %s holds no "fork" field`, repo)
	case !*answer.Fork:
		return forkAnswer{}, fmt.Errorf("GitHub answers that %s is no fork, so no repository it was forked from answers for it", repo)
	}
	return answer, nil
}
