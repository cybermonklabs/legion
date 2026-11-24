package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// backfillRepositories brings each record of project to carry its repository, its default branch
// and GitHub's spelling of the repository, reading GitHub and Dispatch outside every fact
// transaction: fillRepositories fills the records written before the record carried its
// repository, and nameRepositories the records written before it carried GitHub's spelling. The
// boot runs it before it subscribes to any repository's events, and the refresher runs it again
// while a record is left (backfillLater).
//
// A record whose repository the backfill cannot settle — Dispatch resolving none for it, a read of
// GitHub that fails, a shared clone or a pull request that names another repository — is left as
// it is, and the issue, the repository and the cause are logged: one record's repository never
// stops the boot, and the record is tried again by the refresher on its retry (reads.retry) and at the
// next boot. Until it is filled, nothing hears or caches its repository (heardRepositories,
// refreshCacheRepositories, which pass a record with no GitHub spelling by), and each launch of its
// issue refuses, naming the cause (record.Issue.Repository). It answers whether a record is left
// (left), and an error only where a read or a write of the store, or Dispatch's listing, failed.
// What it wrote, each write says at its commit: its log line, and whatever records hooks (the
// refresher's backfillRecords). Each read of GitHub is bounded by reads.attempt (backfillRead), and a
// record left is logged with the refresher's first wait, reads.retry.Initial.
func backfillRepositories(ctx context.Context, pool *store.Pool, records record.Store, client dispatch.Client, project, stateDir string, named *defaultBranches, renames ghrepo.Renames, reads repositoryReadBounds, log *slog.Logger) (left bool, err error) {
	left, err = fillRepositories(ctx, pool, records, client, project, stateDir, named, renames, reads, log)
	if err != nil {
		return left, err
	}
	unnamed, err := nameRepositories(ctx, pool, records, project, named, renames, reads, log)
	return left || unnamed, err
}

// leaveUnfilled says that key's record is left without its repository columns, naming the
// repository the backfill read for it, why it was left, and the refresher's wait before it tries
// again, retryIn.
func leaveUnfilled(log *slog.Logger, retryIn time.Duration, key, repo, why string) {
	log.Warn("workflow backfill: a record is left as it is, without its repository columns, and the backfill tries it again",
		"issue", key, "repo", repo, "cause", why, "retryIn", retryIn.String())
}

// leaveUnnamed says that key's record, which carries its repository, repo, is left without
// GitHub's spelling of it (nameRepositories), why it was left, and the refresher's wait before it
// tries again, retryIn.
func leaveUnnamed(log *slog.Logger, retryIn time.Duration, key, repo, why string) {
	log.Warn("workflow backfill: a record is left as it is, without GitHub's spelling of its repository, and the backfill tries it again",
		"issue", key, "repo", repo, "cause", why, "retryIn", retryIn.String())
}

// fillRepositories fills each record of project written before the record carried its
// repository (record.Store.UnfilledIssues) with the repository Dispatch resolves the issue to now
// (its own, an ancestor's, or the project's default), that repository's default branch and GitHub's
// spelling of it, both from one read of GitHub per repository (backfillRead). One read of
// Dispatch's listing serves every record. A record Dispatch resolves to no repository, or does not
// list, is left, as is one whose pull request, or failing one, whose workspace (a tmux daemon's,
// issueWorkspaces), is in another repository than the one Dispatch resolves, as GitHub names
// either: it names the repository the issue worked in, and a record on another would create its
// branch there, disarm a stranger's pull request, and strand the workspace's unpushed work; the
// log says to set the issue's repository in Dispatch to that one.
//
// The record keeps the spelling of the shared clone under stateDir that spells the repository
// case-insensitively, since the issue's workspace and its sessions were made under that spelling
// (a tmux daemon's; Kubernetes pods provision their own and leave none), and otherwise takes
// GitHub's. Two clones that spell one repository leave no spelling to keep, and the record is
// left, naming both. GitHub's own spelling is kept beside it either way. A repository GitHub
// renamed, which Dispatch still resolves under its old name, takes GitHub's new name, unless a
// shared clone keeps the old one: the record is left, naming the clone, since the record's
// spelling and GitHub's would name two repositories; the log says to set the issue's repository in
// Dispatch to the new name or remove the clone. A rename the deployment configures (renames) names
// one repository, so a transfer the pair already covers leaves nothing. The read of GitHub is made
// of the repository the rename rule serves Dispatch's resolution as (named, which maps every read
// through repositories.serving): after the transfer only the new owner's installation covers the
// repository, so a read minted for the resolved name's owner would be refused. What that read
// answers is the name GitHub holds, the pair's other side, and the record is filled with the name
// its clone keeps and with the pair's spelling of that same name as GitHub's spelling, never with
// the name the repository moved to, since a record's two repository columns must name one
// repository (record.Store.FillIssueRepository). The record keeps the name it was recorded under
// until this release's one-time rename pass writes the pair's `to` over it (renamePass), and
// heardRepositories hears the pair's other name too, so the events GitHub now publishes under the
// name it moved to reach it before that pass as after it.
func fillRepositories(ctx context.Context, pool *store.Pool, records record.Store, client dispatch.Client, project, stateDir string, named *defaultBranches, renames ghrepo.Renames, reads repositoryReadBounds, log *slog.Logger) (left bool, err error) {
	var unfilled []record.Issue
	var pulls map[string]record.PullRequest
	if err := store.TransactWith(ctx, pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if unfilled, err = records.UnfilledIssues(ctx, tx, project); err != nil {
			return err
		}
		// Only each pull request's repository and number are read, so the rows alone, in one read.
		keys := make([]string, len(unfilled))
		for i, issue := range unfilled {
			keys[i] = issue.Key
		}
		pulls, err = records.PullRequestsOf(ctx, tx, keys)
		return err
	}); err != nil {
		return false, fmt.Errorf("read the records with no repository: %w", err)
	}
	if len(unfilled) == 0 {
		return false, nil
	}
	listed, err := client.ListIssues(ctx, project, nil)
	if err != nil {
		return true, fmt.Errorf("list Dispatch issues to fill the records with no repository: %w", err)
	}
	resolved := make(map[string]string, len(listed))
	for _, summary := range listed {
		resolved[summary.Key] = summary.ResolvedRepo
	}
	clones, err := sharedClones(stateDir)
	if err != nil {
		return true, err
	}
	type fill struct {
		key string
		filling
	}
	fills := make([]fill, 0, len(unfilled))
	for _, issue := range unfilled {
		if resolved[issue.Key] == "" {
			leaveUnfilled(log, reads.retry.Initial, issue.Key, "", "Dispatch resolves no repository for it: set its repository in Dispatch")
			left = true
			continue
		}
		repo, err := ghrepo.Parse("the repository Dispatch resolves "+issue.Key+" to", resolved[issue.Key])
		if err != nil {
			leaveUnfilled(log, reads.retry.Initial, issue.Key, resolved[issue.Key], err.Error())
			left = true
			continue
		}
		values, err := fillOf(ctx, issue, repo, pulls, clones, stateDir, named, renames, reads.attempt)
		if err != nil {
			leaveUnfilled(log, reads.retry.Initial, issue.Key, repo.String(), err.Error())
			left = true
			continue
		}
		fills = append(fills, fill{issue.Key, values})
	}
	if len(fills) == 0 {
		return left, nil
	}
	// Each fill's line is a hook of the write's commit: a write whose reply is lost and that the
	// store says committed still logs what it filled, and its retry, which selects no filled
	// record, logs nothing twice.
	if err := serializedTx(ctx, pool, "serialize the backfill with every fact", func(ctx context.Context, tx pgx.Tx) error {
		for _, fill := range fills {
			if err := records.FillIssueRepository(ctx, tx, fill.key, fill.repo, fill.branch, fill.github.String()); err != nil {
				return err
			}
			store.OnCommit(ctx, func() {
				log.Info("legion workflow backfill filled "+fill.key+" with "+fill.repo+"@"+fill.branch, "issue", fill.key, "repo", fill.repo,
					"defaultBranch", fill.branch, "githubRepo", fill.github.String())
			})
		}
		return nil
	}); err != nil {
		return true, err
	}
	return left, nil
}

// filling is what fillRepositories writes into one record: its repository, as the shared clone or
// GitHub spells it, its default branch and GitHub's spelling beside it (recordedSpelling).
type filling struct {
	repo, branch string
	github       ghrepo.Repository
}

// fillOf is what issue's record is filled with from repo, the repository Dispatch resolves it to,
// as fillRepositories says, or why it is left, naming the step that settles it. Its read of GitHub
// is bounded by attempt (backfillRead).
func fillOf(ctx context.Context, issue record.Issue, repo ghrepo.Repository, pulls map[string]record.PullRequest, clones []workspace.Workspace,
	stateDir string, named *defaultBranches, renames ghrepo.Renames, attempt time.Duration) (filling, error) {
	read, err := backfillRead(ctx, named, repo, attempt)
	if err != nil {
		return filling{}, err
	}
	spelling := read.Name.String()
	var spelled []string
	for _, clone := range clones {
		if strings.EqualFold(clone.Repo.String(), repo.String()) {
			spelling = clone.Repo.String()
			spelled = append(spelled, clone.Clone)
		}
	}
	if len(spelled) > 1 {
		return filling{}, fmt.Errorf("two shared clones spell %s (%s): remove the one no issue works in", repo, strings.Join(spelled, ", "))
	}
	if !renames.Same(spelling, read.Name.String()) {
		return filling{}, fmt.Errorf("GitHub renamed %s to %s, and the shared clone %s still has the old name: set the repository of %s in Dispatch to %s, or remove that clone if no issue works in it",
			repo, read.Name, spelled[0], issue.Key, read.Name)
	}
	if pull, ok := pulls[issue.Key]; ok && !renames.Same(pull.Repo, repo.String()) && !renames.Same(pull.Repo, read.Name.String()) {
		return filling{}, fmt.Errorf("Dispatch resolves it to %s, but its pull request #%d is on %s: set the issue's repository in Dispatch to %s (dispatch issue-update --issue %s --repo %s)",
			repo, pull.Number, pull.Repo, pull.Repo, issue.Key, pull.Repo)
	}
	workspaces, err := issueWorkspaces(stateDir, issue.Key)
	if err != nil {
		return filling{}, err
	}
	if dir, in, ok := workspaceElsewhere(workspaces, repo, read.Name, renames); ok {
		return filling{}, fmt.Errorf("Dispatch resolves it to %s, but its workspace %s is in %s: set the issue's repository in Dispatch to %s (dispatch issue-update --issue %s --repo %s)",
			repo, dir, in, in, issue.Key, in)
	}
	return filling{repo: spelling, branch: read.DefaultBranch, github: recordedSpelling(renames, spelling, read.Name)}, nil
}

// backfillLater is the backfill the refresher's second job tries again while a record is left
// without its repository columns (backfillPending, rereadBadAnswers): backfillRepositories as the
// boot runs it, whose writes take the lock every fact applies under, so no fact applies with a
// copy of a record read before the fill, and which writes through backfillRecords, so each record
// it writes is heard and read into the live set at its write's commit. A failure of the store or
// of Dispatch's listing is logged and tried again on the job's backoff; a record written before
// the failure is heard all the same.
func (w *workflowRuntime) backfillLater(ctx context.Context) {
	left, err := backfillRepositories(ctx, w.pool, backfillRecords{Store: w.records, w: w}, w.dispatch, w.dispatchProject, w.stateDir, w.defaultBranches, w.repositories.renames, w.reads(), w.log)
	if err != nil {
		if ctx.Err() == nil {
			w.log.Warn("workflow backfill: the records without their repository columns were not read, listed or written, so the backfill tries them again",
				"retryIn", w.reads().retry.Initial.String(), "error", err)
		}
		return
	}
	w.backfillPending.Store(left)
}

// backfillRecords is the records the refresher's backfill writes through (backfillLater). Each
// record a fill or a name writes has GitHub intake hear the GitHub spelling it now keeps, as
// intake hears the one a new record names (hearRecorded): through hearOrPend, which marks the
// heard set pending only where JetStream refuses the update, and the other name of a configured
// pair that holds it as well, as heardRepositories hears it beside a record's own. It also asks
// the refresher to read the live records again (refreshLive), so a live record it wrote joins the
// live set and the cache set. Both are hooks of the write's commit (store.OnCommit): a write whose
// reply is lost and that the store says committed is heard, and its retry, which finds no record
// left to write, hears nothing twice. The boot's backfill writes through the bare records, since
// it runs before GitHub intake hears anything and the boot's own read covers what it writes
// (heardAtBoot).
type backfillRecords struct {
	record.Store
	w *workflowRuntime
}

func (r backfillRecords) FillIssueRepository(ctx context.Context, tx pgx.Tx, key, repo, defaultBranch, githubRepo string) error {
	if err := r.Store.FillIssueRepository(ctx, tx, key, repo, defaultBranch, githubRepo); err != nil {
		return err
	}
	return r.w.hearBackfilled(ctx, githubRepo)
}

func (r backfillRecords) NameIssueRepository(ctx context.Context, tx pgx.Tx, key, repo, githubRepo string) error {
	if err := r.Store.NameIssueRepository(ctx, tx, key, repo, githubRepo); err != nil {
		return err
	}
	return r.w.hearBackfilled(ctx, githubRepo)
}

// hearBackfilled registers, on the transaction of ctx, the hook that hears spelled, a record's
// GitHub spelling the backfill has just written, and its pair's other name, and asks for a read of
// the live records (backfillRecords).
func (w *workflowRuntime) hearBackfilled(ctx context.Context, spelled string) error {
	repo, err := ghrepo.Parse("GitHub's spelling of the repository the backfill wrote on a record", spelled)
	if err != nil {
		return err
	}
	store.OnCommit(ctx, func() {
		w.hearOrPend(ctx, repo)
		if partner, paired := w.repositories.renames.Partner(repo.String()); paired {
			w.hearOrPend(ctx, partner)
		}
		w.wakeLive()
	})
	return nil
}

// issueWorkspaces is every workspace a tmux daemon provisioned for issue under stateDir, by the
// repository its path names (workspace.Location: <state>/workspaces/<owner>/<name>/<issue>).
// Kubernetes pods provision their own, so a daemon on that runtime has none.
func issueWorkspaces(stateDir, issue string) (map[string]string, error) {
	dirs, err := filepath.Glob(filepath.Join(stateDir, "workspaces", "*", "*", strings.ToLower(issue)))
	if err != nil {
		return nil, fmt.Errorf("list the workspaces of %s: %w", issue, err)
	}
	workspaces := map[string]string{}
	for _, dir := range dirs {
		name := filepath.Dir(dir)
		workspaces[filepath.Base(filepath.Dir(name))+"/"+filepath.Base(name)] = dir
	}
	return workspaces, nil
}

// workspaceElsewhere is a workspace of workspaces in another repository than resolved or GitHub's
// name of it, as renames compares two names (without case, and a configured rename's two names as
// one repository), with that repository.
func workspaceElsewhere(workspaces map[string]string, resolved, named ghrepo.Repository, renames ghrepo.Renames) (dir, in string, ok bool) {
	for repo, dir := range workspaces {
		if !renames.Same(repo, resolved.String()) && !renames.Same(repo, named.String()) {
			return dir, repo, true
		}
	}
	return "", "", false
}

// recordedSpelling is what the backfill writes into a record's GitHubRepo beside the name the
// record keeps, `recorded`: `github`, GitHub's own name, when the two name one repository without
// case, and the configured pair's spelling of `recorded`'s own side when they are a pair. In that
// second case the value is not GitHub's spelling of anything GitHub still serves — after the
// transfer GitHub publishes nothing under it — which is why the helper is not called after
// GitHub's spelling, and which record.Issue.GitHubRepo's own doc says.
//
// A record's two repository columns must name one repository, compared without case
// (record.Store.FillIssueRepository and NameIssueRepository refuse anything else), and the
// record keeps the name it was recorded under until this release's one-time rename pass writes the
// pair's `to` over both columns (renamePass). So the pair's own side is what goes in: the record
// stays one repository and the boot goes on, and the events GitHub publishes under the name the
// repository moved to still reach the record, because heardRepositories hears the pair's other
// name as well.
func recordedSpelling(renames ghrepo.Renames, recorded string, github ghrepo.Repository) ghrepo.Repository {
	if partner, paired := renames.Partner(github.String()); paired && strings.EqualFold(partner.String(), recorded) {
		return partner
	}
	return github
}

// nameRepositories names each record of project that carries its repository but was written
// before the record carried GitHub's spelling of it (record.Store.UnnamedIssues) with that
// spelling, read from GitHub once per repository (backfillRead): SQL cannot know it, since a record
// may keep a clone's spelling that differs from GitHub's in case. A read GitHub refuses or does not
// answer leaves the record unnamed, naming it, so nothing subscribes to a repository in a spelling
// its events are not published under; so does a repository GitHub renamed, since only the rename
// pass of a pair in `repository_renames` writes a new name on a record, and only a pre-release
// build of this daemon left records with no GitHub spelling. A rename the deployment configures
// (renames) is no such rename: the read is made of the repository the rename rule serves the
// record's one as (named, which maps every read through repositories.serving), since after the
// transfer only the new owner's installation covers it, and GitHub answers with the pair's other
// name. The record is then named with the pair's spelling of its own name (recordedSpelling),
// which keeps it one repository.
func nameRepositories(ctx context.Context, pool *store.Pool, records record.Store, project string, named *defaultBranches, renames ghrepo.Renames, reads repositoryReadBounds, log *slog.Logger) (left bool, err error) {
	var unnamed []record.Issue
	if err := store.TransactWith(ctx, pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		unnamed, err = records.UnnamedIssues(ctx, tx, project)
		return err
	}); err != nil {
		return false, fmt.Errorf("read the records with no GitHub spelling of their repository: %w", err)
	}
	names := make(map[string]ghrepo.Repository, len(unnamed))
	for _, issue := range unnamed {
		repo, err := issue.Repository()
		if err != nil {
			leaveUnnamed(log, reads.retry.Initial, issue.Key, issue.Repo, err.Error())
			left = true
			continue
		}
		read, err := backfillRead(ctx, named, repo, reads.attempt)
		if err != nil {
			leaveUnnamed(log, reads.retry.Initial, issue.Key, issue.Repo, err.Error())
			left = true
			continue
		}
		if !renames.Same(read.Name.String(), issue.Repo) {
			leaveUnnamed(log, reads.retry.Initial, issue.Key, issue.Repo, fmt.Sprintf("GitHub renamed %s to %s, and a record keeps its repository name unless a pair of repository_renames names both; only a pre-release build of this daemon leaves a record with no GitHub spelling, so recreate that build's database",
				issue.Repo, read.Name))
			left = true
			continue
		}
		names[issue.Key] = recordedSpelling(renames, issue.Repo, read.Name)
	}
	if len(names) == 0 {
		return left, nil
	}
	if err := serializedTx(ctx, pool, "serialize the backfill with every fact", func(ctx context.Context, tx pgx.Tx) error {
		for _, issue := range unnamed {
			name, ok := names[issue.Key]
			if !ok {
				continue
			}
			if err := records.NameIssueRepository(ctx, tx, issue.Key, issue.Repo, name.String()); err != nil {
				return err
			}
			store.OnCommit(ctx, func() {
				log.Info("legion workflow backfill named "+issue.Key+"'s repository "+issue.Repo+" as GitHub spells it, "+name.String(),
					"issue", issue.Key, "repo", issue.Repo, "githubRepo", name.String())
			})
		}
		return nil
	}); err != nil {
		return true, err
	}
	return left, nil
}
