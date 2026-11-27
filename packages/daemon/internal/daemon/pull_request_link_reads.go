package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/errgroup"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// The reads of the pull requests an issue links in Dispatch (dispatch://INFRA-5910 decisions 4, 8
// and 17): the outbox row that reads one (record.PullRequestLinkRead), the boot's read of every
// running issue's links and of each linked pull request, and the decoder's index of the links the
// intake keeps (intake.Consumers.LoadLinks), which the heard repositories read from too.

// linkRead is one linked pull request read of GitHub in its own repository, as the outbox row and
// the boot make it: the fact it comes to (intake.PullRequestLinkReadDone) and the event id the
// outbox applies that fact under (intake.LinkRead), which the row's own id then follows.
type linkRead struct {
	fact    intake.PullRequestLinkReadDone
	eventID string
}

// readLinkedPullRequest reads pull request number of repo, a pull request issue links, from GitHub
// in its own repository, with the implement App of that repository's owner (implementClient), and
// answers what it comes to as the fact the workflow applies (workflow's pullRequestLinkRead):
//
//   - the state GitHub shows, open, merged or closed, and its head, base and head repository
//     (intake.LinkRead);
//   - NotFound, where GitHub answers 404: the pull request is not there to Legion's Apps;
//   - Unserved, where the repository fails the checks the daemon makes of a recorded repository
//     (linkUnserved): nothing is read then, and the fact names what is missing, in a clause that
//     reads in the notice's sentence ("Legion does not serve <repo>: <detail>.").
//
// An error is any other failure of the read, which the caller reads again: the outbox row on its
// backoff, the boot at the next boot and at each change of the issue's links. The link names the
// repository lowercased; where the pull request's head, in the repository itself, spells it
// otherwise, GitHub intake hears that spelling too, since the subjects Envoy publishes carry
// GitHub's own and NATS matches them case-sensitively.
func (w *workflowRuntime) readLinkedPullRequest(ctx context.Context, issue, repo string, number int) (linkRead, error) {
	return w.readLink(ctx, issue, repo, number, w.linkUnserved)
}

// readLink is readLinkedPullRequest with unserved its check of the repository: linkUnserved, or
// the boot's, which answers it once for each repository of its pass (unservedOnce).
func (w *workflowRuntime) readLink(ctx context.Context, issue, repo string, number int, unserved func(context.Context, ghrepo.Repository) (string, error)) (linkRead, error) {
	parsed, err := ghrepo.Parse("the repository of a linked pull request", repo)
	if err != nil {
		return linkRead{}, err
	}
	detail, err := unserved(ctx, parsed)
	if err != nil {
		return linkRead{}, err
	}
	if detail != "" {
		fact, eventID := intake.LinkUnserved(w.dispatchProject, issue, repo, number, detail)
		return linkRead{fact: fact, eventID: eventID}, nil
	}
	github, _, err := w.servedClient(ctx, parsed)
	if err != nil {
		return linkRead{}, err
	}
	bounded, cancel := context.WithTimeout(ctx, issueRead)
	defer cancel()
	var pull githubrest.PullRequest
	err = github.Get(bounded, fmt.Sprintf("/pulls/%d", number), &pull)
	if githubAnswered(err, http.StatusNotFound) {
		fact, eventID := intake.LinkNotFound(w.dispatchProject, issue, repo, number)
		return linkRead{fact: fact, eventID: eventID}, nil
	}
	if err != nil {
		return linkRead{}, fmt.Errorf("read the linked pull request %s#%d: %w", repo, number, err)
	}
	if spelled, err := ghrepo.Parse("the repository of a linked pull request's head", pull.HeadRepository()); err == nil &&
		strings.EqualFold(spelled.String(), repo) && spelled.String() != repo {
		w.hearGitHubSpelling(ctx, spelled)
	}
	fact, eventID := intake.LinkRead(w.dispatchProject, issue, repo, pull)
	return linkRead{fact: fact, eventID: eventID}, nil
}

// hearGitHubSpelling makes repo, GitHub's own spelling of a repository, the spelling GitHub intake
// hears it under, in place of any other spelling of it that differs in case alone, a link's,
// lowercased: the decoder matches a repository without case, but NATS matches the subject Envoy
// publishes, which carries GitHub's spelling, with it. A held set that names repo under that
// spelling already costs one atomic load. A publication JetStream refuses leaves the set as it
// was, keeps repo for the next one (heardMissed) and wakes the refresher's job that publishes it
// (republishHeard).
func (w *workflowRuntime) hearGitHubSpelling(ctx context.Context, repo ghrepo.Repository) {
	spelledAs := func(held *[]ghrepo.Repository) bool {
		return held != nil && slices.Contains(*held, repo)
	}
	if spelledAs(w.heard.Load()) {
		return
	}
	w.heardMu.Lock()
	defer w.heardMu.Unlock()
	held := w.heard.Load()
	if spelledAs(held) {
		return
	}
	named := map[string]ghrepo.Repository{}
	if held != nil {
		for _, name := range *held {
			named[strings.ToLower(name.String())] = name
		}
	}
	named[strings.ToLower(repo.String())] = repo
	heard := sortedRepositories(named)
	update, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.callBounds().attempt)
	defer cancel()
	if err := w.updateSubscription(update, heard); err != nil {
		w.heardMissed = append(w.heardMissed, repo)
		w.heardPending.Store(true)
		wake(w.heardWake)
		w.log.Warn("workflow: GitHub intake was not told GitHub's spelling of a linked repository, so the refresher publishes the whole heard set again",
			"repo", repo.String(), "error", err)
		return
	}
	w.heard.Store(&heard)
	w.log.Info("legion workflow hears a linked repository under GitHub's spelling", "repo", repo.String(), "heard", repositoryNames(heard))
}

// linkUnserved is why the daemon serves no repository repo, a repository a link names, in a clause
// that reads in the notice's sentence ("Legion does not serve <repo>: <detail>."), "" where it
// serves it: the checks prepareName makes of a recorded repository, both role Apps installed on it
// (the Apps it names missing) and Dispatch's webhook App (checkWebhookApp), made under the name the
// rename rule serves repo under and kept in that name's entry as every preparation's are, so
// legion state shows them. A read of GitHub or Dispatch that did not answer is an error and no
// refusal, since a read that failed says nothing of the repository, and the link is read again.
func (w *workflowRuntime) linkUnserved(ctx context.Context, repo ghrepo.Repository) (string, error) {
	served := w.repositories.serving(repo)
	answer, err := w.prepareName(ctx, served, repo, nil)
	if err != nil {
		return "", err
	}
	if len(answer.missing) != 0 {
		return appauth.NotInstalledOn(answer.missing, served.String()), nil
	}
	if answer.readError != "" {
		return "", errors.New(answer.readError)
	}
	return w.repositories.entryOf(served.String()).webhookApp, nil
}

// unservedOnce is check (linkUnserved) answered once for each repository, lowercased, for the
// reads of one pass: the boot's, which reads every link of every running issue, many of them into
// one repository, where each answer costs two reads of GitHub's installations, a read of the
// default branch and a check of Dispatch's webhook. A check that failed answers nothing and is not
// kept, so the next link of that repository checks it again. It is safe for concurrent use.
func unservedOnce(check func(context.Context, ghrepo.Repository) (string, error)) func(context.Context, ghrepo.Repository) (string, error) {
	var mu sync.Mutex
	answers := map[string]string{}
	return func(ctx context.Context, repo ghrepo.Repository) (string, error) {
		key := strings.ToLower(repo.String())
		mu.Lock()
		detail, known := answers[key]
		mu.Unlock()
		if known {
			return detail, nil
		}
		detail, err := check(ctx, repo)
		if err != nil {
			return "", err
		}
		mu.Lock()
		answers[key] = detail
		mu.Unlock()
		return detail, nil
	}
}

// pullRequestLinkRead runs a linked pull request's read (record.PullRequestLinkRead): it reads the
// pull request the row names in its own repository (readLinkedPullRequest) and applies what it
// comes to as intake.PullRequestLinkReadDone, under the event id of the answer (intake.LinkRead)
// and this row's own, so a row run again after its finish was lost applies the answer once, and a
// link removed and added again, whose pull request nothing changed, is read and applied afresh.
// The workflow records the pull request, or holds the link refused (unserved, unreadable, a fork's,
// another issue's), and judges the issue's close again. Once the fact has committed the decoder's
// index takes the pull request's head branch (linkHead), so a push of that branch is decoded as the
// push of a linked pull request. A failed read is the row's error, which the outbox tries again on
// its backoff.
func (r *outbox) pullRequestLinkRead(ctx context.Context, row record.OutboxRow, payload record.PullRequestLinkRead) error {
	if r.readLinkedPullRequest == nil {
		return errors.New("linked pull request read executor has no read of a pull request from GitHub")
	}
	read, err := r.readLinkedPullRequest(ctx, row.Issue, payload.Repo, payload.Number)
	if err != nil {
		return err
	}
	eventID := fmt.Sprintf("%s:row-%d", read.eventID, row.ID)
	if _, err := intake.ApplyFact(ctx, r.pool, "outbox", eventID, read.fact, r.handlers...); err != nil {
		return fmt.Errorf("apply the read of %s's linked pull request %s#%d: %w", row.Issue, payload.Repo, payload.Number, err)
	}
	if r.linkHead != nil && read.fact.Branch != "" {
		r.linkHead(payload.Repo, payload.Number, read.fact.Branch)
	}
	return nil
}

// loadLinksAtBoot loads the decoder's index of the pull requests the issues link
// (intake.Consumers.LoadLinks) from the record: each row of pull_request_links with the head branch
// of the pull request the record holds for it, "" where it holds none yet, in one statement
// (record.Store.PullRequestLinksWithRows). The boot runs it once the consumers are open and before
// they run, so no event of a linked pull request that is not on legion/<KEY> is decoded as nobody's
// while the index is empty. A link held refused as a fork's or another issue's is left out: such a
// pull request is in no set. A failed read stops the boot, as the boot's read of the records that
// the heard repositories come from does (heardAtBoot).
func (w *workflowRuntime) loadLinksAtBoot(ctx context.Context) error {
	var linked []intake.LinkedPullRequest
	if err := store.TransactWith(ctx, w.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		links, err := w.records.PullRequestLinksWithRows(ctx, tx)
		if err != nil {
			return err
		}
		for _, link := range links {
			if !link.Refused.OutOfSet() {
				linked = append(linked, intake.LinkedPullRequest{Issue: link.Issue, Repo: link.Repo, Number: link.Number, Branch: link.RowBranch})
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("read the links of the issues to pull requests: %w", err)
	}
	w.consumers.LoadLinks(linked)
	return nil
}

// linksTaken is workflow.Config.LinksTaken: the whole of an issue's links as the transaction that
// replaced them wrote them. Once that transaction has committed (store.OnCommit, as hearRecorded's
// is) the decoder's index takes them (intake.Consumers.ReplaceLinks) and GitHub intake hears the
// repository of each, with the repository cache and the live set a recorded issue's repository
// joins (decision 17): a new link of a pull request in another repository is heard in the same
// pass, with no restart. A write outside any commit scope changes nothing here; the boot's own set
// covers it (loadLinksAtBoot, heardRepositories).
func (w *workflowRuntime) linksTaken(ctx context.Context, issue string, links []intake.PullRequestLink) {
	if !store.CommitScoped(ctx) {
		return
	}
	named := slices.Clone(links)
	store.OnCommit(ctx, func() {
		if w.consumers != nil {
			w.consumers.ReplaceLinks(issue, named)
		}
		for _, link := range named {
			repo, err := ghrepo.Parse("the repository of a linked pull request", link.Repo)
			if err != nil {
				w.log.Warn("workflow: a link names a repository GitHub intake cannot hear", "issue", issue, "repo", link.Repo, "error", err)
				continue
			}
			w.repositories.addLive(repo)
			w.cacheRepository(repo)
			w.hearOrPend(ctx, repo)
		}
	})
}

// setLinkHead is the decoder's index taking branch as the head branch of the linked pull request
// repo#number, once its read has committed (intake.Consumers.SetLinkHead).
func (w *workflowRuntime) setLinkHead(repo string, number int, branch string) {
	if w.consumers != nil {
		w.consumers.SetLinkHead(repo, number, branch)
	}
}

// bootLinksBound bounds the boot's whole read of the pull requests the running issues link
// (readLinksAtBoot), so a Dispatch or a GitHub that answers slowly delays the boot by this long and
// no more: what the read did not reach stands as the record holds it, and the next change of the
// issue's links, or the next boot, takes it.
const bootLinksBound = 2 * time.Minute

// readLinksAtBoot is the boot's read of the pull requests each running issue links
// (dispatch://INFRA-5910 decision 9, "The boot reads the links before its sweep"): the links of
// each issue in the workflow as Dispatch's detail shows them now, and, for each linked pull
// request, GitHub's answer in its own repository (readLink), with the state it shows: open, merged
// or closed. The workflow applies all of it as the first act of the boot's fact (workflow's
// takeBootLinks), before sweepMerged judges any close, so a link that went while the daemon was
// down is removed, a link that came is recorded, and a linked pull request that merged meanwhile
// closes its issue at the sweep as its merge would have. It answers nil where it read nothing.
//
// The issues are read repositoryReads at a time, each issue's links one after another, and the
// answer holds them in the project's order of issues, each issue's reads in Dispatch's order of
// its links, whatever order the reads ended in. Each repository is checked once for the whole pass
// (unservedOnce). An issue already done is never read: its links reopen nothing (D10). A read that
// fails is logged and leaves what it could not read to the next one: an issue whose links Dispatch
// could not show keeps the links the record holds, a pull request GitHub could not answer for is
// read again by the outbox (workflow's takeBootLinks queues it), and neither stops the boot. The
// whole read is bounded (bootLinksBound): an issue whose read had not begun at the bound keeps the
// links the record holds.
func (w *workflowRuntime) readLinksAtBoot(ctx context.Context) *intake.BootLinks {
	if w.dispatch == nil {
		return nil
	}
	var issues []record.Issue
	if err := store.TransactWith(ctx, w.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		issues, err = w.records.LiveIssues(ctx, tx, w.dispatchProject)
		return err
	}); err != nil {
		w.log.Warn("workflow: the boot could not list the issues whose links to pull requests it reads; the links stand as the record holds them", "error", err)
		return nil
	}
	issues = slices.DeleteFunc(issues, func(issue record.Issue) bool { return !record.InWorkflow(issue) })
	bounded, cancel := context.WithTimeout(ctx, bootLinksBound)
	defer cancel()
	unserved := unservedOnce(w.linkUnserved)
	read := make([]issueLinksAtBoot, len(issues))
	var group errgroup.Group
	group.SetLimit(repositoryReads)
	for at, issue := range issues {
		group.Go(func() error {
			read[at] = w.readIssueLinksAtBoot(ctx, bounded, issue.Key, unserved)
			return nil
		})
	}
	_ = group.Wait() // each issue's read keeps its own answer, so none fails the group
	boot := intake.BootLinks{Issues: map[string]intake.IssueLinks{}}
	var unreached []string
	for at, links := range read {
		switch {
		case links.unreached:
			unreached = append(unreached, issues[at].Key)
		case links.known:
			boot.Issues[issues[at].Key] = links.links
			boot.Reads = append(boot.Reads, links.reads...)
		}
	}
	if len(unreached) > 0 && ctx.Err() == nil {
		w.log.Warn("workflow: the boot's read of the pull requests the issues link ran out of its bound; the issues it did not reach keep the links the record holds",
			"bound", bootLinksBound, "next", unreached[0], "unreached", len(unreached))
	}
	if len(boot.Issues) == 0 {
		return nil
	}
	return &boot
}

// issueLinksAtBoot is what the boot's read of one issue's links came to (readIssueLinksAtBoot):
// unreached where the bound had passed before it began; else known where Dispatch's detail named
// the issue's links, with those links at the detail's sequence and the read of each linked pull
// request that GitHub answered, in the order of the links.
type issueLinksAtBoot struct {
	unreached, known bool
	links            intake.IssueLinks
	reads            []intake.PullRequestLinkReadDone
}

// readIssueLinksAtBoot is the boot's read of issue's links and of each pull request they name
// (readLinksAtBoot), bounded by bounded, the pass's bound, each failure logged where ctx, the
// boot's own, has not ended.
func (w *workflowRuntime) readIssueLinksAtBoot(ctx, bounded context.Context, issue string, unserved func(context.Context, ghrepo.Repository) (string, error)) issueLinksAtBoot {
	if bounded.Err() != nil {
		return issueLinksAtBoot{unreached: true}
	}
	detail, err := w.dispatch.GetIssue(bounded, issue)
	if err != nil {
		if ctx.Err() == nil {
			w.log.Warn("workflow: the boot could not read an issue's links to pull requests from Dispatch; they stand as the record holds them",
				"issue", issue, "error", err)
		}
		return issueLinksAtBoot{}
	}
	given, known := intake.DetailLinks(detail)
	if !known {
		return issueLinksAtBoot{}
	}
	links := issueLinksAtBoot{known: true, links: intake.IssueLinks{Seq: detail.LastSeq, Links: given}}
	for _, link := range given {
		read, err := w.readLink(bounded, issue, link.Repo, link.Number, unserved)
		if err != nil {
			if ctx.Err() == nil {
				w.log.Warn("workflow: the boot could not read a linked pull request; its read is queued in the outbox",
					"issue", issue, "pullRequest", fmt.Sprintf("%s#%d", link.Repo, link.Number), "error", err)
			}
			continue
		}
		links.reads = append(links.reads, read.fact)
	}
	return links
}

// linkRepositories is each distinct repository the record's links name, once each by its
// lowercased <owner>/<name>, as the heard repositories add them (heardRepositories): the rows of
// pull_request_links, whatever their issue's state, since a repository a link names is heard for as
// long as the link stands, and a link refused as a fork's or another issue's names none that is
// worked. Each is spelled as the record spells the pull request the link names, which is GitHub's
// own (workflow's pullRequestLinkRead), where the record holds that pull request for the link's
// issue, and as the link spells it, lowercased, where it does not; the links and those rows are
// one statement (record.Store.PullRequestLinksWithRows). A link whose repository is no
// <owner>/<name> is passed by with a log line, and stops nothing.
func (w *workflowRuntime) linkRepositories(ctx context.Context, tx pgx.Tx) (map[string]ghrepo.Repository, error) {
	links, err := w.records.PullRequestLinksWithRows(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("read the repositories the links of the issues name: %w", err)
	}
	named := map[string]ghrepo.Repository{}
	for _, link := range links {
		if link.Refused.OutOfSet() {
			continue
		}
		repo, err := ghrepo.Parse("the repository of a link of "+link.Issue, cmp.Or(link.RowRepo, link.Repo))
		if err != nil {
			w.log.Warn("workflow: a link names a repository GitHub intake cannot hear", "issue", link.Issue, "repo", link.Repo, "error", err)
			continue
		}
		if held, seen := named[strings.ToLower(repo.String())]; !seen || held.String() == strings.ToLower(held.String()) {
			named[strings.ToLower(repo.String())] = repo
		}
	}
	return named, nil
}
