package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"

	"github.com/sjawhar/envoy/oauth2call"
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
// that applies to the branch, its parameters read as a `required_status_checks` rule's
// (requiredStatusChecks) and kept only for one, since each type of rule has parameters of its own
// and none of the others names their two fields.
type githubBranchRule struct {
	Type       string               `json:"type"`
	Parameters requiredStatusChecks `json:"parameters"`
}

// requiredStatusChecks is the parameters of a `required_status_checks` rule: the contexts it
// requires, and whether it requires a pull request to be current with the branch before it
// merges.
type requiredStatusChecks struct {
	RequiredStatusChecks []struct {
		Context githubText `json:"context"`
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
			EnforcementLevel string       `json:"enforcement_level"`
			Contexts         []githubText `json:"contexts"`
			Checks           []struct {
				Context githubText `json:"context"`
			} `json:"checks"`
		} `json:"required_status_checks"`
	} `json:"protection"`
}

// githubText is a string member of GitHub's answer the daemon both decides on and prints - a
// status check context, a pull request's base branch - read twice: Name as it came, which the
// daemon keeps and decides on (record.BaseRules.Required, the base the rules read asks for), and
// Said, GitHub's words, which the answer's Decode holds with the read's record
// (oauth2call.Answer.Decode), so a line that names the member prints it as the rule reads it.
type githubText struct {
	Name string
	Said oauth2call.RemoteText
}

// UnmarshalJSON reads data, a JSON string, as the member as it came and as GitHub's words.
func (t *githubText) UnmarshalJSON(data []byte) error {
	if err := json.Unmarshal(data, &t.Name); err != nil {
		return err
	}
	return json.Unmarshal(data, &t.Said)
}

// readBaseRules reads the rules base, the base branch of pr, holds for its required status checks
// (record.BaseRules), with github, the implement App's client of pr's repository that the read of
// the pull request built (readPullRequest). applyMergeability calls it for a read GitHub answered
// `mergeable: false`, and the answer rides on that read's fact (intake.PullRequestMergeability.Rules),
// which R0 decides a confirmed conflict on (dispatch://INFRA-4374 R0, classify.ConflictNeedsMerge).
// These are the two reads the reviewer's prompt makes of the same rules (prompts/go/reviewer.md):
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
// base is GitHub's base.ref, or the record's base where GitHub's answer named none
// (applyMergeability), and the line and Unread name it as shownBase does.
//
// GitHub's 403 whose message carries planRefusal, on either read, is the answer of a plan that
// holds neither rulesets nor classic protection, so it reads as no rules: Required empty, Strict
// false, Unread "". Any other failure of either read - another 403, a 404 (GitHub's answer for a
// branch that does not exist), a 5xx, a read that runs out of issueRead, which bounds each read as
// it bounds the per-issue read's - is a read whose rules the daemon does not know: Unread says why,
// R0 acts on it at once (record.ConflictRulesUnread), and nothing else fails. Each call logs one
// line with what it read, the base and the contexts as the rule reads GitHub's words.
func (w *workflowRuntime) readBaseRules(ctx context.Context, github githubrest.Client, pr record.PullRequest, base githubText) record.BaseRules {
	rules, said := baseRules(ctx, github, base)
	attributes := []any{"repo", pr.Repo, "pullRequest", pr.Number, "base", shownBase(base),
		"required", said, "strict", rules.Strict, "unread", rules.Unread}
	if rules.Unread != "" {
		w.log.Warn("github: read the base's rules for a conflicting head", attributes...)
	} else {
		w.log.Info("github: read the base's rules for a conflicting head", attributes...)
	}
	return rules
}

// baseRules is readBaseRules' two reads and what they come to, without its log line, with each
// required context as GitHub's words (said), in the order of Required.
func baseRules(ctx context.Context, github githubrest.Client, base githubText) (rules record.BaseRules, said []oauth2call.RemoteText) {
	path := ghbranch.RefPath(base.Name)
	read, cancel := context.WithTimeout(ctx, issueRead)
	rulesets, err := githubrest.GetPages[githubBranchRule](read, github, "/rules/branches/"+path)
	cancel()
	if err != nil {
		return unreadRules(err, "GET /rules/branches/"+shownBase(base)), nil
	}
	words := map[string]oauth2call.RemoteText{}
	var required []string
	add := func(named githubText) {
		required = append(required, named.Name)
		if _, seen := words[named.Name]; !seen {
			words[named.Name] = named.Said
		}
	}
	strict := false
	for _, rule := range rulesets {
		if rule.Type != "required_status_checks" {
			continue
		}
		for _, check := range rule.Parameters.RequiredStatusChecks {
			add(check.Context)
		}
		strict = strict || rule.Parameters.Strict
	}
	var branch githubBranch
	read, cancel = context.WithTimeout(ctx, issueRead)
	err = github.Get(read, "/branches/"+path, &branch)
	cancel()
	if err != nil {
		return unreadRules(err, "GET /branches/"+shownBase(base)), nil
	}
	if protection := branch.Protection; protection.Enabled && protection.RequiredStatusChecks.EnforcementLevel != "off" {
		for _, named := range protection.RequiredStatusChecks.Contexts {
			add(named)
		}
		for _, check := range protection.RequiredStatusChecks.Checks {
			add(check.Context)
		}
	}
	slices.Sort(required)
	required = slices.Compact(required)
	for _, name := range required {
		said = append(said, words[name])
	}
	return record.BaseRules{Required: required, Strict: strict}, said
}

// shownBase is base as the rules read names it on its line and in Unread: GitHub's words for it as
// the rule reads them (githubText.Said), and the record's base as it is where GitHub named none
// (applyMergeability), which has no Said.
func shownBase(base githubText) string {
	if base.Said.IsEmpty() {
		return base.Name
	}
	return base.Said.String()
}

// unreadRules is what a failed read of the base's rules comes to: no rules for GitHub's refusal of
// rulesets to a plan that has none (planRefusal), and otherwise rules the daemon could not read,
// Unread naming read - the read's method and route, its base as shownBase names it - and GitHub's
// status and message, or why it got no answer. The path the read asked holds the base as it came,
// and GitHub's answer (githubrest.Answer.Path) and net/http's error for a request that got no
// answer (url.Error.URL) name that path, so Unread names neither. The refusal is decided on
// GitHub's message as it came (oauth2call.GitHubRefusal, decoded from the answer), and the message
// prints as the rule reads it.
func unreadRules(err error, read string) record.BaseRules {
	var answer *githubrest.Answer
	if !errors.As(err, &answer) {
		var unanswered *url.Error
		if errors.As(err, &unanswered) {
			return record.BaseRules{Unread: fmt.Sprintf("%s got no answer: %v", read, unanswered.Err)}
		}
		return record.BaseRules{Unread: fmt.Sprintf("%s: %v", read, err)}
	}
	var refusal oauth2call.GitHubRefusal
	_ = answer.Decode(&refusal)
	if answer.Status == http.StatusForbidden && !answer.RateLimited && refusal.Message.Contains(planRefusal) {
		return record.BaseRules{}
	}
	unread := fmt.Sprintf("GitHub answered %s with %d", read, answer.Status)
	if !refusal.Message.IsEmpty() {
		unread += ": " + refusal.Message.String()
	}
	return record.BaseRules{Unread: unread}
}
