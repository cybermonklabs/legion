package api

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
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

// ciUnreadDetailRunes is how long the detail of a check whose runs GitHub could not be read for
// is, at most: the bound ciObservedOutputRunes puts on a run's output.
const ciUnreadDetailRunes = ciObservedOutputRunes

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
	// ciCaseHeadNotRead is a newest head that a read of GitHub is owed for, and
	// ciCaseIssueNotKnown a pull request whose answer is not known that can come before the newest
	// head. Each has the word of its reason (model.ReasonHeadNotRead, model.ReasonIssueNotKnown).
	ciCaseHeadNotRead   = model.ReasonHeadNotRead
	ciCaseIssueNotKnown = model.ReasonIssueNotKnown
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
//
// Two heads Dispatch does not know stop the read of the check runs, where Dispatch can read the
// repository (comparisonReader.decides, rule 2). A pull request of the repository whose answer
// is not known, and which comes before the newest head in the order of
// delivery.NewestPullRequestHead, or any one where no head is found, can be the issue's newest
// head: each check answers unread with ciCaseIssueNotKnown (decision D1). Else a newest head that
// a read of GitHub is owed for may not be the head: each check answers unread with
// ciCaseHeadNotRead. Either way no check run is read, and passStandsOf answers unknown.
func (s *server) deriveCIResults(ctx context.Context, reader *comparisonReader, issueKey string, checks []model.CheckBlock) error {
	var proved []provedCI
	names := map[string]struct{}{}
	for index := range checks {
		if validCICheck(checks[index], len(checks[index].Results) > 0) {
			name := docs.CheckAttributesOfBlock(checks[index]).CICheck
			proved = append(proved, provedCI{check: &checks[index], name: name})
			names[name] = struct{}{}
		}
	}
	if len(proved) == 0 {
		return nil
	}
	// Every case below writes a line, and every line names the run, so the rule its words are kept
	// by is read before any of them; an issue holding no check CI proves still reads nothing.
	words, err := s.readLineWords(ctx, s.deps.Store.Pool)
	if err != nil {
		return err
	}
	head, found, err := delivery.NewestPullRequestHead(ctx, s.deps.Store.Pool, issueKey)
	if err != nil {
		return err
	}
	unread, err := s.ciHeadNotKnown(ctx, reader, issueKey, head, found)
	if err != nil {
		return err
	}
	if unread != nil {
		for _, p := range proved {
			p.check.CI = ciUnconcluded(*p.check, p.name, model.CIStateUnread, unread.ciCase, unread.detail, words)
		}
		return nil
	}
	if !found {
		for _, p := range proved {
			p.check.CI = ciUnconcluded(*p.check, p.name, model.CIStateNoResult, ciCaseNoHead,
				fmt.Sprintf("Dispatch holds no open or merged pull request head of %s.", issueKey), words)
		}
		return nil
	}
	runs, err := s.readCIRuns(ctx, head, names)
	if err != nil {
		detail := ciUnreadDetail(head, err)
		for _, p := range proved {
			p.check.CI = ciUnconcluded(*p.check, p.name, model.CIStateUnread, ciCaseUnreadable, detail, words)
		}
		return nil
	}
	for _, p := range proved {
		var result *model.CheckResult
		p.check.CI, result = ciAnswerOf(*p.check, p.name, head, runs[p.name], words)
		if result != nil {
			p.check.Results = []model.CheckResult{*result}
		}
	}
	return nil
}

// ciUnreadHead is why a check CI proves reads no check run: its case and the sentence that names
// the pull request.
type ciUnreadHead struct {
	ciCase, detail string
}

// ciHeadNotKnown is why the newest head of issueKey cannot be the head that the check runs are
// read at (deriveCIResults), and nil where it can, or where Dispatch cannot read the repository:
// a deployment with no App key and a repository the App cannot read answer as before (rule 2).
// The pull requests whose answer is not known are read from the database only where the issue
// resolves to a repository. A failed read of them is an error, as a failed read of the heads is.
func (s *server) ciHeadNotKnown(
	ctx context.Context, reader *comparisonReader, issueKey string, head delivery.NewestHead, found bool,
) (*ciUnreadHead, error) {
	repo, answered := reader.repositoryOf(ctx, issueKey)
	if !answered || repo == nil || s.deps.GitHub == nil {
		return nil, nil
	}
	pulls, err := delivery.PullRequestsNotKnown(ctx, s.deps.Store.Pool, *repo)
	if err != nil {
		return nil, err
	}
	var before *delivery.NotKnownPullRequest
	for index := range pulls {
		pull := &pulls[index]
		if found && !newestBefore(pull.State, pull.Number, head.State, head.Number) {
			continue
		}
		if before == nil || newestBefore(pull.State, pull.Number, before.State, before.Number) {
			before = pull
		}
	}
	if before == nil && !(found && head.ReadOwed) {
		return nil, nil
	}
	if !reader.decides(ctx, issueKey) {
		return nil, nil
	}
	if before != nil {
		return &ciUnreadHead{ciCase: ciCaseIssueNotKnown, detail: fmt.Sprintf(
			"Dispatch does not know yet whether pull request #%d is %s's: its commit messages are not read.",
			before.Number, issueKey)}, nil
	}
	return &ciUnreadHead{ciCase: ciCaseHeadNotRead, detail: fmt.Sprintf(
		"Dispatch has not read the head of pull request #%d of %s yet: a read of GitHub is owed for it.",
		head.Number, head.Repo)}, nil
}

// newestBefore is whether a pull request in state with number comes before one in otherState
// with otherNumber in the order of delivery.NewestPullRequestHead: open before merged, then the
// higher number.
func newestBefore(state string, number int, otherState string, otherNumber int) bool {
	if (state == "open") != (otherState == "open") {
		return state == "open"
	}
	return number > otherNumber
}

// provedCI is a check CI proves and name, the run its `ci_check` names as the rules read it,
// trimmed (docs.CheckAttributesOfBlock): the block as served keeps `ci_check` as written.
type provedCI struct {
	check *model.CheckBlock
	name  string
}

// readCIRuns reads every page of the check runs of head's commit, in head's own repository,
// under one ciReadDeadline, and answers the run of the highest id of each name in names: GitHub
// keeps every attempt of a run, and the newest decides (classify.mergeAttemptSets in the daemon
// keeps one name's run the same way). A row whose repository is no name GitHub allows
// (refs.SplitGitHubRepo) is the error before any GitHub call: a rename stored before the routes
// held names to that rule can make `acme/..` the canonical name of a row, and its halves would go
// into the paths as they are. A deployment holding no App key, a token GitHub would not mint, an
// HTTP error, a rate limit and the deadline are each the error, worded as GitHub answered it. The
// token never reaches it: it is sent in a header, and no answer names it.
func (s *server) readCIRuns(ctx context.Context, head delivery.NewestHead, names map[string]struct{}) (map[string]*ciRun, error) {
	owner, repo, err := refs.SplitGitHubRepo(head.Repo)
	if err != nil {
		return nil, fmt.Errorf("the pull request row's %w", err)
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

// ciUnreadDetail is the detail of a check whose runs at head GitHub could not be read for, err
// the read's error, worded by gitHubErrorText: the error's text ends with GitHub's body as
// githubapp.ShownBody shows it, its first 512 characters of a page of check runs that may take
// 16 MiB, where GitHub refused or rate-limited the read, and that body gives way to GitHub's
// `message`, read from the whole body, so the status stays. The sentence is then cut to
// ciUnreadDetailRunes.
func ciUnreadDetail(head delivery.NewestHead, err error) string {
	return text.HeadRunes(fmt.Sprintf("GitHub could not be read for the check runs of %s at %s: %s.",
		head.Repo, head.HeadSHA, gitHubErrorText(err)), ciUnreadDetailRunes)
}

// gitHubErrorText is the text of err, a GitHub read's error. Where GitHub refused the read
// (githubapp.StatusError) or rate-limited it (githubapp.RateLimitError), the body the error's
// text ends with, as githubapp.ShownBody shows it, is replaced by the `message` GitHub writes in
// the JSON it refuses a request with, read from the whole body, and left out where it holds none
// (withGitHubMessage). Any other error keeps its text.
func gitHubErrorText(err error) string {
	var refused *githubapp.StatusError
	if errors.As(err, &refused) {
		return withGitHubMessage(err.Error(), githubapp.ShownBody(refused.Body), refused.Message())
	}
	if limited, ok := githubapp.AsRateLimit(err); ok {
		return withGitHubMessage(err.Error(), githubapp.ShownBody(limited.Body), limited.Message())
	}
	return err.Error()
}

// gitHubReason is err's text as a refusal sentence carries it: gitHubErrorText, cut to its first
// gitHubReasonBytes at a whole character (text.HeadBytes) where it is longer, saying so. The cut
// is the one bound of every 502 GITHUB_UNREADABLE that names GitHub's error
// (getAppInstallation), since an error GitHub answered with a page that is no JSON carries the
// shown part of that page, and an error of a token mint carries what githubapp read of it.
func gitHubReason(err error) string {
	reason := gitHubErrorText(err)
	head, cut := text.HeadBytes(reason, gitHubReasonBytes)
	if !cut {
		return reason
	}
	return fmt.Sprintf("%s… (GitHub's error, cut to its first %d of %d bytes)", head, len(head), len(reason))
}

// gitHubReasonBytes is the most bytes of GitHub's error gitHubReason keeps, about a kilobyte:
// room for the status and a message as GitHub writes one, while the refusal stays a sentence a
// caller prints whole.
const gitHubReasonBytes = 1024

// withGitHubMessage is reason, an error's text, with body, which ends it after ": ", replaced by
// ": " and message, or by nothing where message is "". A text that does not end so is kept.
func withGitHubMessage(reason, body, message string) string {
	kept, ends := strings.CutSuffix(reason, body)
	if !ends {
		return reason
	}
	if kept, ends = strings.CutSuffix(kept, ": "); !ends {
		return reason
	}
	if message == "" {
		return kept
	}
	return kept + ": " + message
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

// ciAnswerOf decides one check CI proves from run, the run named name at head, nil where head
// carries none: the `ci` member, and the derived result for a pass or a fail, nil for every other
// case. words is the rule the pull-request body line keeps the run's name and the check's words by.
func ciAnswerOf(check model.CheckBlock, name string, head delivery.NewestHead, run *ciRun, words lineWords) (*model.CheckCI, *model.CheckResult) {
	at := fmt.Sprintf("%s, the head of %s#%d", head.HeadSHA, head.Repo, head.Number)
	switch {
	case run == nil:
		return ciUnconcluded(check, name, model.CIStateNoResult, ciCaseAbsent,
			fmt.Sprintf("No CI run named %s is on %s.", name, at), words), nil
	case run.status != ciRunCompleted:
		return ciUnconcluded(check, name, model.CIStateNoResult, ciCaseNotCompleted,
			fmt.Sprintf("CI run %s is %s at %s, and has not concluded.", run.name, run.status, at), words), nil
	}
	switch run.conclusion {
	case ciConclusionNeutral, ciConclusionSkipped, ciConclusionCancelled:
		return ciUnconcluded(check, name, model.CIStateNoResult, run.conclusion,
			fmt.Sprintf("CI run %s concluded %s at %s, which proves nothing.", run.name, run.conclusion, at), words), nil
	}
	verdict := model.CIStateFail
	if run.conclusion == ciConclusionSuccess {
		verdict = model.CIStatePass
	}
	answer := &model.CheckCI{
		Run: run.name, State: verdict, Case: run.conclusion,
		Detail: fmt.Sprintf("CI run %s concluded %s at %s.", run.name, run.conclusion, at),
		Line:   ciVerdictLine(check, name, verdict, head.HeadSHA, words),
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

// ciUnconcluded is the `ci` member of a check its run, the one named name, did not decide, for
// the case given.
func ciUnconcluded(check model.CheckBlock, name, state, ciCase, detail string, words lineWords) *model.CheckCI {
	return &model.CheckCI{
		Run: name, State: state, Case: ciCase, Detail: detail,
		Line: fmt.Sprintf("- Check %s (%s): %s has no result (%s)", check.BlockID, check.When, ciRunOfLine(name, words), ciCase),
	}
}

// ciRunOfLine is the run a check's ci_check names as a pull-request body line names it, `CI run`
// and the words words keeps of the name, since the name is text the check's author wrote; where
// words keeps none of it, the line says `the CI run the check names`.
func ciRunOfLine(name string, words lineWords) string {
	kept := words.keep(name)
	if kept == "" {
		return "the CI run the check names"
	}
	return "CI run " + kept
}

// ciVerdictLine is the pull-request body line of a derived result: the recorded line's grammar,
// with the run that proved it (ciRunOfLine), where a recorded line names its recorder, and the
// check's own words where a recorded line carries the role's reading, since no role read anything.
// The words are kept by words and cut as checkResultLine keeps and cuts a reading; a check of whose
// words nothing is left after the cut ends the line at the run, as a check with no words does.
func ciVerdictLine(check model.CheckBlock, name, verdict, sha string, words lineWords) string {
	line := fmt.Sprintf("- Check %s (%s): %s at %s, from %s.", check.BlockID, check.When, verdict, sha, ciRunOfLine(name, words))
	tail := words.cut(words.keepThrough(check.Text, checkResultReadingRunes))
	if tail == "" {
		return line
	}
	return line + " " + tail
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
