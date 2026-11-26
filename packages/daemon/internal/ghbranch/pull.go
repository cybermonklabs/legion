package ghbranch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
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

// Closed is what a close of a pull request (ClosePullRequest) found and made. Now is true where
// this call closed it. By is the login GitHub names as its closer where it was closed already: the
// implement App's login for a close Legion made, by this row's earlier try or by another row, and
// another login for someone else's close. Merged is true where it was merged, which nothing
// closes. Unread is why GitHub named no closer of a pull request closed already (ClosedByNull, or
// GitHub's answer to the read, which it would answer again), and empty otherwise.
type Closed struct {
	Now    bool
	By     string
	Merged bool
	Unread string
}

// ClosedByNull is Closed.Unread of a pull request closed already whose closed_by GitHub answers
// as null: GitHub names no closer, and gives no cause.
const ClosedByNull = "closed_by is null"

// ClosePullRequest closes pull request number of repo, whose REST API github calls, and then
// writes comment on it once, as login, the implement App's bot login, whose token github holds. A
// retry of a row that closes pull requests runs it again from the top, so each step reads what an
// earlier try made:
//  1. The comment passes the deployment's private-name rules first, since it is published on
//     GitHub as a pull request's body is: rules, the rule set the daemon compiled from the
//     deployment's rule file at boot, run alone (leakguard.RuleSet.Check). A comment they match,
//     and a rule pass that could not be made to run, are each an error, and nothing is sent. Nil
//     rules are a deployment that names no rule file, and no private-name rule runs. Either way
//     the close reads no environment: the rules are what its caller hands it, never a rule file
//     its process's environment may point at.
//  2. GET /pulls/{number}. A merged pull request is left as it is: no close and no comment.
//  3. An open one is closed (PATCH /pulls/{number} with the state closed), then commented (step
//     5). A close GitHub refuses sends no comment, so no comment says Legion closed a pull request
//     that stays open.
//  4. One closed already is read for who closed it (GET /issues/{number}, its closed_by, which the
//     pull request object does not carry). Where it is login, Legion made the close, by an earlier
//     try of this row or by another row, and it is commented (step 5): a second row can so write a
//     second comment, each true. Where it is another login, someone else closed it, and it gets no
//     close and no comment. Where GitHub names no closer (a closed_by of null, or an answer it
//     would give again, RefusedForGood), it gets no comment on a guess, and the call answers why
//     (Closed.Unread). Any other failure of the read is an error, and the row tries again.
//  5. Its comments are read (GET /issues/{number}/comments, every page), and comment is written
//     (POST /issues/{number}/comments) unless one of them is login's with that body, byte for
//     byte: the row fixes the comment's text, so it needs no hidden marker.
//
// Every answer other than 2xx is an error naming the repository and the pull request and wrapping
// GitHub's *githubrest.Answer. An error at step 5 answers the close that stands (Closed.Now or
// Closed.By), so the caller can tell a pull request closed with no comment from one left open.
func ClosePullRequest(ctx context.Context, github githubrest.Client, repo ghrepo.Repository, number int, comment, login string, rules *leakguard.RuleSet) (Closed, error) {
	failed := func(closed Closed, err error) (Closed, error) {
		return closed, fmt.Errorf("close pull request #%d on %s: %w", number, repo, err)
	}
	if rules != nil {
		if err := rules.Check(ctx, "the write", leakguard.Record{Label: "the pull request's comment", Text: comment}); err != nil {
			return failed(Closed{}, err)
		}
	}
	var pull githubrest.PullRequest
	if err := github.Get(ctx, fmt.Sprintf("/pulls/%d", number), &pull); err != nil {
		return failed(Closed{}, err)
	}
	var closed Closed
	switch {
	case pull.Merged || pull.MergedAt != nil:
		return Closed{Merged: true}, nil
	case pull.State == "open":
		if err := github.Patch(ctx, fmt.Sprintf("/pulls/%d", number), map[string]string{"state": "closed"}, nil); err != nil {
			return failed(Closed{}, err)
		}
		closed = Closed{Now: true}
	default:
		by, unread, err := closedBy(ctx, github, number)
		if err != nil {
			return failed(Closed{}, err)
		}
		if unread != "" || by != login {
			return Closed{By: by, Unread: unread}, nil
		}
		closed = Closed{By: login}
	}
	if err := commentOnce(ctx, github, number, comment, login); err != nil {
		return failed(closed, err)
	}
	return closed, nil
}

// closedBy is the login GitHub names as the closer of issue or pull request number (GET
// /issues/{number}, its closed_by), or why it names none (Closed.Unread): ClosedByNull for a
// closed_by that is null or names no login, and GitHub's answer where it refuses the read as it
// would refuse it again (RefusedForGood). Any other failure of the read is the error.
func closedBy(ctx context.Context, github githubrest.Client, number int) (by, unread string, err error) {
	var issue struct {
		ClosedBy *struct {
			Login string `json:"login"`
		} `json:"closed_by"`
	}
	if err := github.Get(ctx, fmt.Sprintf("/issues/%d", number), &issue); err != nil {
		var answer *githubrest.Answer
		if errors.As(err, &answer) && RefusedForGood(err) {
			return "", answer.Error(), nil
		}
		return "", "", err
	}
	if issue.ClosedBy == nil || issue.ClosedBy.Login == "" {
		return "", ClosedByNull, nil
	}
	return issue.ClosedBy.Login, "", nil
}

// issueComment is a comment GitHub lists on an issue or a pull request: its body and its author.
type issueComment struct {
	Body string `json:"body"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
}

// commentOnce writes comment on pull request number as login unless it holds login's comment of
// that body already.
func commentOnce(ctx context.Context, github githubrest.Client, number int, comment, login string) error {
	comments, err := githubrest.GetPages[issueComment](ctx, github, fmt.Sprintf("/issues/%d/comments", number))
	if err != nil {
		return err
	}
	if slices.ContainsFunc(comments, func(c issueComment) bool { return c.User.Login == login && c.Body == comment }) {
		return nil
	}
	return github.Post(ctx, fmt.Sprintf("/issues/%d/comments", number), map[string]string{"body": comment}, nil)
}

// RefusedForGood says whether err, a failure of a step of a close of a pull request
// (ClosePullRequest), is one a try again would meet again: the private-name rules' refusal of the
// comment (*leakguard.Refusal), whose text the row fixes, and a GitHub answer
// (*githubrest.Answer) of 403 that is no rate limit (the App lacks the permission, or the
// repository is archived), 404 (no such pull request on the repository), 410 or 422 (GitHub refuses
// the write as sent). Every other failure can pass and is tried again: a rate limit, any 5xx, a 401,
// a 409, any other status, and an error that carries no answer (a network fault, a timeout, the
// rules' own setup).
func RefusedForGood(err error) bool {
	var refusal *leakguard.Refusal
	if errors.As(err, &refusal) {
		return true
	}
	var answer *githubrest.Answer
	if !errors.As(err, &answer) {
		return false
	}
	switch answer.Status {
	case http.StatusForbidden:
		return !answer.RateLimited
	case http.StatusNotFound, http.StatusGone, http.StatusUnprocessableEntity:
		return true
	}
	return false
}

// SetBase sets the base of pull request number of repo, whose REST API github calls, to base
// (PATCH /repos/{owner}/{repo}/pulls/{number} with the base), and answers whether it changed it. A
// pull request GitHub shows against base already is left as it is, with no PATCH, so a set made
// again changes nothing. Every answer other than 2xx is an error naming the repository, the pull
// request and the base and wrapping GitHub's *githubrest.Answer.
func SetBase(ctx context.Context, github githubrest.Client, repo ghrepo.Repository, number int, base string) (bool, error) {
	failed := func(err error) error {
		return fmt.Errorf("set the base of pull request #%d on %s to %s: %w", number, repo, base, err)
	}
	var pull githubrest.PullRequest
	if err := github.Get(ctx, fmt.Sprintf("/pulls/%d", number), &pull); err != nil {
		return false, failed(err)
	}
	if pull.Base.Ref == base {
		return false, nil
	}
	if err := github.Patch(ctx, fmt.Sprintf("/pulls/%d", number), map[string]string{"base": base}, nil); err != nil {
		return false, failed(err)
	}
	return true, nil
}
