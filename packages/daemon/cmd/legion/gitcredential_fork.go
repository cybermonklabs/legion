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

	"github.com/sjawhar/envoy/oauth2call"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
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

// forkRepository is the repository the credential helper reads from GitHub after ghConfigDirFor
// refused a request (forkUpstreamDir): owner, the path's owner already lowercased, and name, the
// path's second segment already keyed as the bundle keys a repository (ghconfig.RepositoryName:
// lowercased, without `.git`), the keyed name the caller routed with ghConfigDirFor. Its caller
// asks only where that refusal is, or wraps, a leakguard.OwnerNotServedError: a pairs file that
// ghconfig.ReadPairs refuses makes no read, since neither the fork's parent nor its source could
// be routed by it either. There is one only where the bundle holds no directory for the path's own
// owner (ownerGHConfigDir), an owner the role's App is not installed on, and where the two make an
// `<owner>/<name>` that ghrepo.Parse accepts. ok is false for anything else, and no read is made.
// A served owner whose repository a configured rename moved to an owner with no directory keeps
// ghConfigDirFor's refusal, which is about that pair. A path with no second segment, or with one
// no GitHub repository could be named for, names no repository to read.
func forkRepository(ownersDir, owner, name string) (repo ghrepo.Repository, ok bool) {
	var unserved *leakguard.OwnerNotServedError
	if _, err := ownerGHConfigDir(ownersDir, owner); !errors.As(err, &unserved) {
		return ghrepo.Repository{}, false
	}
	repo, err := ghrepo.Parse("the repository the path names", owner+"/"+name)
	return repo, err == nil
}

// forkUpstreamDir is the gh directory that answers for repo, a repository of an owner the bundle
// under ownersDir holds no directory for (forkRepository), where GitHub says repo is a fork of a
// repository the bundle serves. Such a fork is where an outside author's pull request to a served
// repository keeps its head. The token of the served repository's owner is the one a push to that
// head can carry, and GitHub, not this helper, decides which branch of the fork it may update.
//
// It reads repo from GitHub once: `GET /repos/<owner>/<name>` through the leak guard's curl read
// (leakguard's Guard.CurlGet), with the token of GH_CONFIG_DIR's directory (forkReadToken). The
// read runs within forkReadBound, the token read included, and inside ctx. Where GitHub answers
// that repo is a fork, the answer is the directory ghConfigDirFor routes the fork's parent to
// where the bundle holds a directory for the parent's owner, else the one it routes the fork's
// source to where the bundle holds a directory for the source's owner (forkAnswer.dir), with
// route, the words that say how: `the fork <repo> routes to <dir> through its parent <full
// name>`, or `through its source <full name>`. The caller then reads that directory's token as it
// reads every answer's, and puts route in front of a failure of that read, whose own error names
// the directory alone and so neither the fork nor which of its two upstreams led there.
//
// Anything else is an error that holds refused, ghConfigDirFor's refusal of the request, and then
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

// readForkUpstream is forkUpstreamDir's read and its answer, the directory and the route to it,
// the error saying why there is none. A SIGHUP cancels the read (hangupCancels), so a pane that
// closes while git waits on the helper stops curl's process group, as `legion gh` and `legion
// leak-check` stop theirs, rather than killing the helper and leaving curl running until its
// --max-time.
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
	answer, err := readForkAnswer(response, repo, token)
	if err != nil {
		return "", "", err
	}
	return answer.dir(ownersDir, repo, token)
}

// shownName is the repository a full name of GitHub's answer names, where it is `owner/name` as
// GitHub names one (ghrepo.Parse) and prints: it holds no GitHub token form (githubrest.HoldsToken)
// and no run of token, the token the read carried (oauth2call.Tokens), which whatever answers in
// GitHub's place can echo into a name ghrepo.Parse takes (`owner/ghs_...`, since a repository's
// name may hold "_"). A full name that is neither is false: it is named by what it is, never
// printed or routed through.
func shownName(fullName, token string) (ghrepo.Repository, bool) {
	named, err := ghrepo.Parse("GitHub's full_name", fullName)
	if err != nil || githubrest.HoldsToken(named.String()) || oauth2call.Tokens(named.String(), -1, token) != named.String() {
		return ghrepo.Repository{}, false
	}
	return named, true
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
// network (its source), which GitHub gives for a fork alone. Message is the one field of an error
// answer it reads.
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

// readForkAnswer is GitHub's answer to the fork read of repo, sent with token, where it is a 200
// that says repo is a fork, and an error naming the answer otherwise. In order: a curl that got no
// answer (CurlResponse.Unreached) or that failed after it heard one, quoting what curl said as the
// rule reads it (CurlResponse.Said), a rate limit, with its reset or its wait
// (CurlResponse.RateLimit), any status but 200, with GitHub's message and, for a redirect, where
// it points, since curl follows none; and, of a 200, a body that is not JSON, an answer for
// another repository than repo (a full_name that differs from it beyond case), and an answer with
// no "fork" field or a false one.
//
// Each value of GitHub's answer an error names is GitHub's word, read with no record of the
// token the read carried, so it prints only in a shape that holds no credential: a message of
// plain words (githubrest.PlainMessage), a redirect to a repository by its id
// (githubrest.RepositoryRedirect), a full name that is `owner/name` as GitHub names one and holds
// no token (shownName). A value of any other shape is named by what it is and not printed.
func readForkAnswer(response leakguard.CurlResponse, repo ghrepo.Repository, token string) (forkAnswer, error) {
	if response.Exit != 0 {
		if unreached := response.Unreached(); unreached != nil {
			return forkAnswer{}, fmt.Errorf("the read of %s failed: %w", repo, unreached)
		}
		return forkAnswer{}, fmt.Errorf("curl's read of %s exited %d: %s", repo, response.Exit, response.Said())
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
		switch {
		case githubrest.PlainMessage(answer.Message):
			said += " (" + answer.Message + ")"
		case answer.Message != "":
			said += " (with a message that is more than plain words, which the helper does not print)"
		}
		switch location := headers["location"]; {
		case githubrest.RepositoryRedirect(location):
			said += ", pointing at " + location + ", a redirect the helper does not follow"
		case location != "":
			said += ", pointing at an address that names no repository by its id, a redirect the helper does not follow"
		}
		return forkAnswer{}, errors.New(said)
	}
	switch {
	case decodeErr != nil:
		return forkAnswer{}, fmt.Errorf("GitHub's answer for %s is not JSON of one repository: %v", repo, decodeErr)
	case !strings.EqualFold(answer.FullName, repo.String()):
		if another, shown := shownName(answer.FullName, token); shown {
			return forkAnswer{}, fmt.Errorf("GitHub answered the read of %s with another repository, %q", repo, another.String())
		}
		return forkAnswer{}, fmt.Errorf("GitHub answered the read of %s with a full_name that names no repository GitHub could hold", repo)
	case answer.Fork == nil:
		return forkAnswer{}, fmt.Errorf(`GitHub's answer for %s holds no "fork" field`, repo)
	case !*answer.Fork:
		return forkAnswer{}, fmt.Errorf("GitHub answers that %s is no fork, so no repository it was forked from answers for it", repo)
	}
	return answer, nil
}

// dir is the gh directory that answers for the fork repo, by R2's rule: where the bundle holds a
// directory for the owner of the fork's parent, the nearer of the two repositories it comes from,
// it is the answer a request for the parent would get; otherwise, where the bundle holds one for
// the owner of the fork's source, the answer a request for the source would get; and in every
// other case there is none. The owner's directory alone decides which of the two answers
// (upstreamOwnerServed): one GitHub does not name, or whose owner the bundle holds no directory
// for, is not routed at all, even where a configured rename would move it to an owner that has
// one. The one that answers is routed as a request for it would be (upstreamGHConfigDir), so a
// configured rename that moved it applies, and where that routes nowhere, as a rename that moved
// it to an owner with no directory does, the fork gets nothing, as a request for it does, and a
// parent that routes nowhere leaves the source untried. route names repo, that directory and the
// repository that routed there, as GitHub spells it: `the fork <repo> routes to <dir> through its
// parent <full name>`, or `its source`. Where the parent routes nowhere, the error names repo and
// the parent, why the parent did not route, and that the source was not tried; otherwise it names
// repo, the parent and the source, and why each did not answer. A full name that is not
// `owner/name` as GitHub names one, or that holds a token form or a run of token, the token the
// read carried (shownName), routes nowhere and is named as no repository, never printed: it is
// GitHub's word, and only a name of that shape prints.
func (a forkAnswer) dir(ownersDir string, repo ghrepo.Repository, token string) (dir, route string, err error) {
	tried := make([]string, 0, 2)
	for _, upstream := range []struct {
		role     string
		fullName string
	}{{"parent", a.Parent.FullName}, {"source", a.Source.FullName}} {
		if upstream.fullName == "" {
			tried = append(tried, "GitHub names no "+upstream.role)
			continue
		}
		named, shown := shownName(upstream.fullName, token)
		if !shown {
			tried = append(tried, fmt.Sprintf("the %s, whose full_name names no repository GitHub could hold", upstream.role))
			continue
		}
		if err = upstreamOwnerServed(ownersDir, named); err != nil {
			tried = append(tried, fmt.Sprintf("the %s %s, since %v", upstream.role, named, err))
			continue
		}
		if dir, err = upstreamGHConfigDir(ownersDir, named); err == nil {
			return dir, fmt.Sprintf("the fork %s routes to %s through its %s %s", repo, dir, upstream.role, named), nil
		}
		if upstream.role == "parent" {
			return "", "", fmt.Errorf("GitHub answers that %s is a fork of %s, whose owner the bundle holds a directory for, and a request for %s answers nothing, so the fork gets nothing either and its source is not tried: %v",
				repo, named, named, err)
		}
		tried = append(tried, fmt.Sprintf("the %s %s, since %v", upstream.role, named, err))
	}
	return "", "", fmt.Errorf("GitHub answers that %s is a fork, and neither repository it comes from routes to a directory of the bundle: %s",
		repo, strings.Join(tried, "; "))
}

// upstreamGHConfigDir is the gh directory ghConfigDirFor routes the repository upstream to, as
// GitHub spells it: its owner lowercased, as a request's owner is, and its name keyed as the
// bundle keys one (ghconfig.RepositoryName). upstream is a name ghrepo.Parse took, so no name in
// GitHub's answer can name a path outside the bundle.
func upstreamGHConfigDir(ownersDir string, upstream ghrepo.Repository) (string, error) {
	return ghConfigDirFor(ownersDir, strings.ToLower(upstream.Owner()), ghconfig.RepositoryName(upstream.Name()))
}

// upstreamOwnerServed is nil where the bundle under ownersDir holds a directory for the owner of
// the repository upstream, as GitHub spells it, keyed as upstreamGHConfigDir keys it
// (lowercased), and otherwise ownerGHConfigDir's refusal, in the words a request for a repository
// of that owner is refused with.
func upstreamOwnerServed(ownersDir string, upstream ghrepo.Repository) error {
	_, err := ownerGHConfigDir(ownersDir, strings.ToLower(upstream.Owner()))
	return err
}
