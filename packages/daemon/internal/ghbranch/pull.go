package ghbranch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
)

// OpenPullRequest opens a pull request of head, a branch of repo, against base on repo, whose REST
// API github calls, with title and body (POST /repos/{owner}/{repo}/pulls), and answers its number.
// A head that has an open pull request already, which GitHub refuses with 422 "A pull request
// already exists", answers the number of the open one (GET /pulls?head=<owner>:<head>&state=open),
// so an open made again opens none. Every other answer is an error naming the repository and both
// branches and wrapping GitHub's *githubrest.Answer, GitHub's 422 for a head with no commit base
// lacks among them.
//
// The title and the body are published on GitHub as a created ref's name is, so each passes the
// deployment's private-name rules before anything is sent, as Create's ref name does: rules, the
// rule set the daemon compiled from the deployment's rule file at boot, run alone
// (leakguard.RuleSet.Check). A text they match, and a rule pass that could not be made to run, are
// each an error, and nothing is sent. Nil rules are a deployment that names no rule file, and no
// private-name rule runs. Either way the open reads no environment: the rules are what its caller
// hands it, never a rule file its process's environment may point at.
func OpenPullRequest(ctx context.Context, github githubrest.Client, repo ghrepo.Repository, head, base, title, body string, rules *leakguard.RuleSet) (int, error) {
	failed := func(err error) error {
		return fmt.Errorf("open the pull request of %s against %s on %s: %w", head, base, repo, err)
	}
	if rules != nil {
		if err := rules.Check(ctx, "the write",
			leakguard.Record{Label: "the pull request's title", Text: title},
			leakguard.Record{Label: "the pull request's body", Text: body}); err != nil {
			return 0, failed(err)
		}
	}
	var opened githubrest.PullRequest
	err := github.Post(ctx, "/pulls", map[string]string{"title": title, "head": head, "base": base, "body": body}, &opened)
	var answer *githubrest.Answer
	switch {
	case err == nil:
		return opened.Number, nil
	case errors.As(err, &answer) && answer.Status == http.StatusUnprocessableEntity && strings.Contains(answer.Body, "A pull request already exists"):
		open, ok, err := github.OpenPullRequestOn(ctx, repo, head)
		if err != nil {
			return 0, failed(err)
		}
		if !ok {
			return 0, failed(fmt.Errorf("GitHub answered that a pull request of %s exists and lists none open: %w", head, answer))
		}
		return open.Number, nil
	default:
		return 0, failed(err)
	}
}

// ClosePullRequest closes pull request number of repo, whose REST API github calls, with comment
// written on it first (POST /repos/{owner}/{repo}/issues/{number}/comments, then PATCH
// /repos/{owner}/{repo}/pulls/{number} with the state closed), and answers whether it closed it. A
// pull request GitHub shows closed already, merged or not, is left as it is, with no comment, so a
// close made again writes nothing. Every answer other than 2xx is an error naming the repository and
// the pull request and wrapping GitHub's *githubrest.Answer.
//
// The comment is published on GitHub as a pull request's body is, so it passes the deployment's
// private-name rules first: rules, the rule set the daemon compiled from the deployment's rule file
// at boot, run alone (leakguard.RuleSet.Check). A comment they match, and a rule pass that could
// not be made to run, are each an error, and nothing is sent. Nil rules are a deployment that names
// no rule file, and no private-name rule runs. Either way the close reads no environment: the rules
// are what its caller hands it, never a rule file its process's environment may point at.
func ClosePullRequest(ctx context.Context, github githubrest.Client, repo ghrepo.Repository, number int, comment string, rules *leakguard.RuleSet) (bool, error) {
	failed := func(err error) error {
		return fmt.Errorf("close pull request #%d on %s: %w", number, repo, err)
	}
	if rules != nil {
		if err := rules.Check(ctx, "the write", leakguard.Record{Label: "the pull request's comment", Text: comment}); err != nil {
			return false, failed(err)
		}
	}
	var pull githubrest.PullRequest
	if err := github.Get(ctx, fmt.Sprintf("/pulls/%d", number), &pull); err != nil {
		return false, failed(err)
	}
	if pull.State != "open" {
		return false, nil
	}
	if err := github.Post(ctx, fmt.Sprintf("/issues/%d/comments", number), map[string]string{"body": comment}, nil); err != nil {
		return false, failed(err)
	}
	if err := github.Patch(ctx, fmt.Sprintf("/pulls/%d", number), map[string]string{"state": "closed"}, nil); err != nil {
		return false, failed(err)
	}
	return true, nil
}
