package api

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// results_ci.go answers a check CI proves (validCICheck) on the read of an issue's results. No
// role records a result for such a check (refuseCheckProvedByCI), and nothing is stored for it
// either: each read takes the newest pull request head of the issue
// (delivery.NewestPullRequestHead), reads that commit's check runs from GitHub once, and derives
// the check's one result from the run its `ci_check` names. The derived result goes through the
// same comparisonReader a recorded result does, so one rule judges where both kinds stand.

// ciReadDeadline bounds the whole read of one head's check runs, the token mint and every page
// together: the route is read inside a role's turn and inside the reviewer's gate, and a read
// that runs out is a failed read the next one repairs. githubapp.RequestTimeout bounds one call
// at twice this, so the bound is the context's. A var so a test can shorten it.
var ciReadDeadline = 10 * time.Second

// ciCheckRunPages bounds the pages of one head's check runs, a hundred runs a page, as
// cireconcile bounds its own reads of the same listing: a listing GitHub keeps reporting full
// past it is a failed read rather than a loop.
const ciCheckRunPages = 100

// ciResultRole is the role a derived result names: the producer, never a role that recorded it.
const ciResultRole = "ci"

// ciResultActorKind is the kind of actor a derived result names, whose id is the slug of the
// GitHub App that created the run.
const ciResultActorKind = "github_app"

// ciResultIDPrefix is what a derived result's id begins with, before the check run's own id, so
// no derived id can be read as a stored row's uuid.
const ciResultIDPrefix = "ci-run:"

// ciObservedOutputRunes is how much of the run's output title and summary, taken together, a
// derived result's observed text carries.
const ciObservedOutputRunes = 2000

// The cases that name why a check CI proves holds no derived result, beside the conclusions GitHub
// writes. A conclusion is its own case: success is a pass; neutral, skipped and cancelled prove
// nothing; failure, timed_out, action_required, startup_failure and stale are a fail, and so is
// a conclusion this list does not name, as GitHub wrote it.
const (
	ciConclusionSuccess   = "success"
	ciConclusionNeutral   = "neutral"
	ciConclusionSkipped   = "skipped"
	ciConclusionCancelled = "cancelled"
	ciCaseNotCompleted    = "not_completed"
	ciCaseAbsent          = "absent"
	ciCaseNoHead          = "no_head"
	ciCaseUnreadable      = "github_unreadable"
)

// ciRunCompleted is the status of a check run that has concluded.
const ciRunCompleted = "completed"

// ciRun is one GitHub check run, with the fields a derived result is made of.
type ciRun struct {
	id          int64
	name        string
	status      string
	conclusion  string
	completedAt time.Time
	htmlURL     string
	title       string
	summary     string
	appSlug     string
}

// deriveCIResults answers every check CI proves among checks: its `ci` member, and, where the
// run that decides it passed or failed, its one result. Each check carries the results the read
// loaded for it, which say whether it holds a recorded result and so is CI's at all. An issue
// holding no check CI proves asks nothing of the database or of GitHub. issueKey is the route's
// issue, whose Verification document holds the checks and whose pull request heads they are read
// at. Only a failed read of Dispatch's own database is an error; whatever GitHub answers is a
// case of the answer.
func (s *server) deriveCIResults(ctx context.Context, issueKey string, checks []model.CheckBlock) error {
	var proved []*model.CheckBlock
	names := map[string]struct{}{}
	for index := range checks {
		if validCICheck(checks[index], len(checks[index].Results) > 0) {
			proved = append(proved, &checks[index])
			names[checks[index].CICheck] = struct{}{}
		}
	}
	if len(proved) == 0 {
		return nil
	}
	head, found, err := delivery.NewestPullRequestHead(ctx, s.deps.Store.Pool, issueKey)
	if err != nil {
		return err
	}
	if !found {
		for _, check := range proved {
			check.CI = ciUnconcluded(*check, model.CIStateNoResult, ciCaseNoHead,
				fmt.Sprintf("Dispatch holds no open or merged pull request head of %s.", issueKey))
		}
		return nil
	}
	runs, err := s.readCIRuns(ctx, head, names)
	for _, check := range proved {
		if err != nil {
			check.CI = ciUnconcluded(*check, model.CIStateUnread, ciCaseUnreadable,
				fmt.Sprintf("GitHub could not be read for the check runs of %s at %s: %v.", head.Repo, head.HeadSHA, err))
			continue
		}
		var result *model.CheckResult
		check.CI, result = ciAnswerOf(*check, head, runs[check.CICheck])
		if result != nil {
			check.Results = []model.CheckResult{*result}
		}
	}
	return nil
}

// readCIRuns reads every page of the check runs of head's commit, in head's own repository,
// under one ciReadDeadline, and answers the run of the highest id of each name in names: GitHub
// keeps every attempt of a run, and the newest decides (listenerSettlement in the daemon reads
// one name the same way). A deployment holding no App key, a token GitHub would not mint, an
// HTTP error, a rate limit and the deadline are each the error, worded as GitHub answered it.
// The token never reaches it: it is sent in a header, and no answer names it.
func (s *server) readCIRuns(ctx context.Context, head delivery.NewestHead, names map[string]struct{}) (map[string]*ciRun, error) {
	owner, repo, split := strings.Cut(head.Repo, "/")
	if !split || owner == "" || repo == "" {
		return nil, fmt.Errorf("the pull request row names repository %q, which is not owner/repo", head.Repo)
	}
	ctx, cancel := context.WithTimeout(ctx, ciReadDeadline)
	defer cancel()
	runs, err := s.readCIRunPages(ctx, owner, repo, head.HeadSHA, names)
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("no answer within %s: %w", ciReadDeadline, err)
	}
	return runs, err
}

// readCIRunPages is readCIRuns's own read, pages 1..N until the first page that is not full, as
// cireconcile reads the same listing.
func (s *server) readCIRunPages(ctx context.Context, owner, repo, sha string, names map[string]struct{}) (map[string]*ciRun, error) {
	token, err := s.deps.GitHub.RepositoryToken(ctx, owner, repo)
	if err != nil {
		return nil, fmt.Errorf("mint an installation token: %w", err)
	}
	newest := map[string]*ciRun{}
	for number := 1; number <= ciCheckRunPages; number++ {
		page, err := s.deps.GitHub.CommitCheckRuns(ctx, token, owner, repo, sha, number, "")
		if err != nil {
			return nil, err
		}
		for _, object := range page.Objects {
			name, _ := object["name"].(string)
			if _, wanted := names[name]; !wanted {
				continue
			}
			// encoding/json decodes a number into float64, which holds a check run's id exactly:
			// GitHub's ids are far below 2^53.
			id, _ := object["id"].(float64)
			if held, ok := newest[name]; !ok || int64(id) > held.id {
				newest[name] = ciRunOf(int64(id), name, object)
			}
		}
		if !page.Full {
			return newest, nil
		}
	}
	return nil, fmt.Errorf("the check runs did not end within %d pages", ciCheckRunPages)
}

// ciRunOf reads the check run id named name out of the object GitHub listed. A field GitHub
// left null or out reads as its zero value: a run not completed carries no conclusion and no
// completion time, and GitHub Actions fills no output for its own jobs.
func ciRunOf(id int64, name string, object map[string]any) *ciRun {
	run := &ciRun{id: id, name: name}
	run.status, _ = object["status"].(string)
	run.conclusion, _ = object["conclusion"].(string)
	run.htmlURL, _ = object["html_url"].(string)
	if completed, ok := object["completed_at"].(string); ok {
		run.completedAt, _ = time.Parse(time.RFC3339, completed)
	}
	if output, ok := object["output"].(map[string]any); ok {
		run.title, _ = output["title"].(string)
		run.summary, _ = output["summary"].(string)
	}
	if app, ok := object["app"].(map[string]any); ok {
		run.appSlug, _ = app["slug"].(string)
	}
	return run
}

// ciAnswerOf decides one check CI proves from run, the run of its name at head, nil where head
// carries none: the `ci` member, and the derived result for a pass or a fail, nil for every other
// case.
func ciAnswerOf(check model.CheckBlock, head delivery.NewestHead, run *ciRun) (*model.CheckCI, *model.CheckResult) {
	at := fmt.Sprintf("%s, the head of %s#%d", head.HeadSHA, head.Repo, head.Number)
	switch {
	case run == nil:
		return ciUnconcluded(check, model.CIStateNoResult, ciCaseAbsent,
			fmt.Sprintf("No CI run named %s is on %s.", check.CICheck, at)), nil
	case run.status != ciRunCompleted:
		return ciUnconcluded(check, model.CIStateNoResult, ciCaseNotCompleted,
			fmt.Sprintf("CI run %s is %s at %s, and has not concluded.", run.name, run.status, at)), nil
	}
	switch run.conclusion {
	case ciConclusionNeutral, ciConclusionSkipped, ciConclusionCancelled:
		return ciUnconcluded(check, model.CIStateNoResult, run.conclusion,
			fmt.Sprintf("CI run %s concluded %s at %s, which proves nothing.", run.name, run.conclusion, at)), nil
	}
	verdict := model.CIStateFail
	if run.conclusion == ciConclusionSuccess {
		verdict = model.CIStatePass
	}
	answer := &model.CheckCI{
		Run: run.name, State: verdict, Case: run.conclusion,
		Detail: fmt.Sprintf("CI run %s concluded %s at %s.", run.name, run.conclusion, at),
		Line:   ciVerdictLine(check, verdict, head.HeadSHA),
	}
	result := model.CheckResult{
		ID:       ciResultIDPrefix + strconv.FormatInt(run.id, 10),
		IssueKey: check.IssueKey, CheckIssueKey: check.IssueKey,
		CheckArtifactID: check.ArtifactID, CheckBlockID: check.BlockID,
		CheckVersion: check.Version, CheckSHA256: check.SHA256,
		Role: ciResultRole, Surface: check.Surface, Run: run.name, Observed: ciObserved(*run),
		CommitSHA: head.HeadSHA, NegativeControl: check.NegativeControl, Verdict: verdict,
		Screenshots: []string{}, Actor: model.Actor{Kind: ciResultActorKind, ID: run.appSlug},
		CreatedAt: run.completedAt,
	}
	return answer, &result
}

// ciUnconcluded is the `ci` member of a check its run did not decide, for the case given.
func ciUnconcluded(check model.CheckBlock, state, ciCase, detail string) *model.CheckCI {
	return &model.CheckCI{
		Run: check.CICheck, State: state, Case: ciCase, Detail: detail,
		Line: fmt.Sprintf("- Check %s (%s): CI run %s has not concluded (%s)", check.BlockID, check.When, check.CICheck, ciCase),
	}
}

// ciVerdictLine is the pull-request body line of a derived result: the recorded line's grammar,
// with the run that proved it where a recorded line names its recorder, and the check's own
// words where a recorded line carries the role's reading, since no role read anything. The words
// are cut as checkResultLine cuts a reading.
func ciVerdictLine(check model.CheckBlock, verdict, sha string) string {
	line := fmt.Sprintf("- Check %s (%s): %s at %s, from CI run %s.", check.BlockID, check.When, verdict, sha, check.CICheck)
	if check.Text == "" {
		return line
	}
	return line + " " + cutWords(check.Text)
}

// ciObserved is what a derived result says was observed, one part a line: the run's conclusion,
// then its output title and summary together cut to ciObservedOutputRunes, then the run's URL. A
// part GitHub left empty is left out.
func ciObserved(run ciRun) string {
	parts := []string{"conclusion: " + run.conclusion}
	output := run.title
	if output != "" && run.summary != "" {
		output += "\n"
	}
	output += run.summary
	if output != "" {
		parts = append(parts, text.HeadRunes(output, ciObservedOutputRunes))
	}
	if run.htmlURL != "" {
		parts = append(parts, run.htmlURL)
	}
	return strings.Join(parts, "\n")
}
