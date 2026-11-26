package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/ghbranch"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// planRefusal is the part of GitHub's 403 message that refuses rulesets to a private repository
// whose plan has none: "Upgrade to GitHub Pro or make this repository public to enable this
// feature." on a user's repository, "Upgrade to GitHub Team or ..." on an organization's, each
// ending with its period inside the JSON `message` (docs/solutions/legion/
// controller-gate-2-required-checks-live-reads.md). It is matched as a substring of the message,
// never as its suffix or the whole of it, so either plan's wording and its period match.
const planRefusal = "make this repository public to enable this feature"

// githubBranchRule is one rule GET /rules/branches/{branch} answers: every rule of every ruleset
// that applies to the branch, its parameters read only for a `required_status_checks` rule
// (requiredStatusChecks), since each type of rule has parameters of its own.
type githubBranchRule struct {
	Type       string          `json:"type"`
	Parameters json.RawMessage `json:"parameters"`
}

// requiredStatusChecks is the parameters of a `required_status_checks` rule: the contexts it
// requires, and whether it requires a pull request to be current with the branch before it
// merges.
type requiredStatusChecks struct {
	RequiredStatusChecks []struct {
		Context string `json:"context"`
	} `json:"required_status_checks"`
	Strict bool `json:"strict_required_status_checks_policy"`
}

// githubBranch is what GET /branches/{branch} answers of the branch's classic protection: whether
// it is enabled, and its required status checks, which name their contexts twice over (`contexts`
// and each `checks[].context`) and carry no `strict`. The read that does carry it
// (/branches/{branch}/protection) needs Administration read, which the implement App lacks.
type githubBranch struct {
	Protection struct {
		Enabled              bool `json:"enabled"`
		RequiredStatusChecks struct {
			EnforcementLevel string   `json:"enforcement_level"`
			Contexts         []string `json:"contexts"`
			Checks           []struct {
				Context string `json:"context"`
			} `json:"checks"`
		} `json:"required_status_checks"`
	} `json:"protection"`
}

// readBaseRules reads the rules base, the base branch of pr, holds for its required status checks
// (record.BaseRules), with github, the implement App's client of pr's repository that the read of
// the pull request built (readPullRequest). applyMergeability calls it for a read GitHub answered
// `mergeable: false`, and the answer rides on that read's fact (intake.PullRequestMergeability.Rules),
// which R0 decides a confirmed conflict on (dispatch://INFRA-4374 R0, classify.ConflictNeedsMerge).
// These are the two reads:
//
//   - GET /rules/branches/{base}, every page (githubrest.GetPages): each `required_status_checks`
//     rule of the rulesets that apply to the branch gives its contexts, and Strict holds when one
//     of them sets `strict_required_status_checks_policy`.
//   - GET /branches/{base}: where classic protection is enabled and its required status checks are
//     not enforced `off`, its `contexts` and each `checks[].context` are required too. That answer
//     carries no `strict`, so a classic rule is read as not strict (departure 5 of the plan of
//     dispatch://INFRA-4374).
//
// base is path-escaped with its slashes kept (ghbranch.RefPath), so a branch whose name holds a
// `#`, a `?` or a `%` reads that branch and no other. Required holds each context once, sorted.
//
// GitHub's 403 whose message carries planRefusal, on either read, is the answer of a plan that
// holds neither rulesets nor classic protection, so it reads as no rules: Required empty, Strict
// false, Unread "". Any other failure of either read - another 403, a 404 (GitHub's answer for a
// branch that does not exist), a 5xx, a read that runs out of issueRead, which bounds each read as
// it bounds the per-issue read's - is a read whose rules the daemon does not know: Unread says why,
// R0 acts on it at once (record.ConflictRulesUnread), and nothing else fails. Each call logs one
// line with what it read.
//
// Both reads are conditional, every page of the rulesets too (githubrest.Client.ETags, the
// runtime's etags): a base's rules change seldom and every conflicting read of every pull request
// on it reads them, so GitHub's 304, which costs the installation's rate limit nothing, answers
// most of them with the rules read before.
func (w *workflowRuntime) readBaseRules(ctx context.Context, github githubrest.Client, pr record.PullRequest, base string) record.BaseRules {
	rules := baseRules(ctx, github.WithETags(w.etags), base)
	attributes := []any{"repo", pr.Repo, "pullRequest", pr.Number, "base", base,
		"required", rules.Required, "strict", rules.Strict, "unread", rules.Unread}
	if rules.Unread != "" {
		w.log.Warn("github: read the base's rules for a conflicting head", attributes...)
	} else {
		w.log.Info("github: read the base's rules for a conflicting head", attributes...)
	}
	return rules
}

// baseRules is readBaseRules' two reads and what they come to, without its log line.
func baseRules(ctx context.Context, github githubrest.Client, base string) record.BaseRules {
	path := ghbranch.RefPath(base)
	read, cancel := context.WithTimeout(ctx, issueRead)
	rulesets, err := githubrest.GetPages[githubBranchRule](read, github, "/rules/branches/"+path)
	cancel()
	if err != nil {
		return unreadRules(err)
	}
	var required []string
	strict := false
	for _, rule := range rulesets {
		if rule.Type != "required_status_checks" {
			continue
		}
		var checks requiredStatusChecks
		if err := json.Unmarshal(rule.Parameters, &checks); err != nil {
			return record.BaseRules{Unread: fmt.Sprintf("decode a required_status_checks rule of %s: %v", base, err)}
		}
		for _, check := range checks.RequiredStatusChecks {
			required = append(required, check.Context)
		}
		strict = strict || checks.Strict
	}
	var branch githubBranch
	read, cancel = context.WithTimeout(ctx, issueRead)
	err = github.Get(read, "/branches/"+path, &branch)
	cancel()
	if err != nil {
		return unreadRules(err)
	}
	if protection := branch.Protection; protection.Enabled && protection.RequiredStatusChecks.EnforcementLevel != "off" {
		required = append(required, protection.RequiredStatusChecks.Contexts...)
		for _, check := range protection.RequiredStatusChecks.Checks {
			required = append(required, check.Context)
		}
	}
	slices.Sort(required)
	return record.BaseRules{Required: slices.Compact(required), Strict: strict}
}

// unreadRules is what a failed read of the base's rules comes to: no rules for GitHub's refusal of
// rulesets to a plan that has none (planRefusal), and otherwise rules the daemon could not read,
// Unread naming GitHub's answer - the request, its status and GitHub's message - or the error.
func unreadRules(err error) record.BaseRules {
	var answer *githubrest.Answer
	if !errors.As(err, &answer) {
		return record.BaseRules{Unread: err.Error()}
	}
	var body struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal([]byte(answer.Body), &body)
	if answer.Status == http.StatusForbidden && !answer.RateLimited && strings.Contains(body.Message, planRefusal) {
		return record.BaseRules{}
	}
	unread := fmt.Sprintf("GitHub answered %s %s with %d", answer.Method, answer.Path, answer.Status)
	if body.Message != "" {
		unread += ": " + body.Message
	}
	return record.BaseRules{Unread: unread}
}
