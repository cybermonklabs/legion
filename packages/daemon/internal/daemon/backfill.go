package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
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
// stops the boot, and the record is tried again by the refresher on installReadRetry and at the
// next boot. Until it is filled, nothing hears or caches its repository (heardRepositories,
// refreshCacheRepositories, which pass a record with no GitHub spelling by), and each launch of its
// issue refuses, naming the cause (record.Issue.Repository). It answers whether a record was
// filled or named (filled), whether one is left (left), and an error only where a read or a write
// of the store, or Dispatch's listing, failed.
func backfillRepositories(ctx context.Context, pool *store.Pool, records record.Store, client dispatch.Client, project, stateDir string, named *defaultBranches, renames ghrepo.Renames, log *slog.Logger) (filled, left bool, err error) {
	filled, left, err = fillRepositories(ctx, pool, records, client, project, stateDir, named, renames, log)
	if err != nil {
		return filled, left, err
	}
	wrote, unnamed, err := nameRepositories(ctx, pool, records, project, named, renames, log)
	return filled || wrote, left || unnamed, err
}

// leaveUnfilled says that key's record is left without its repository columns, naming the
// repository the backfill read for it and why it was left.
func leaveUnfilled(log *slog.Logger, key, repo, why string) {
	log.Warn("workflow backfill: a record is left as it is, without its repository columns, and the backfill tries it again",
		"issue", key, "repo", repo, "cause", why, "retryIn", installReadRetry.Initial.String())
}

// writeBackfill runs write in one transaction under the lock every fact applies under
// (intake.SerializeFacts), so no fact applies with a copy of a record read before the backfill
// wrote it.
func writeBackfill(ctx context.Context, pool *store.Pool, write func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := intake.SerializeFacts(ctx, tx); err != nil {
			return fmt.Errorf("serialize the backfill with every fact: %w", err)
		}
		return write(tx)
	})
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
func fillRepositories(ctx context.Context, pool *store.Pool, records record.Store, client dispatch.Client, project, stateDir string, named *defaultBranches, renames ghrepo.Renames, log *slog.Logger) (filled, left bool, err error) {
	var unfilled []record.Issue
	pulls := map[string]record.PullRequest{}
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var err error
		if unfilled, err = records.UnfilledIssues(ctx, tx, project); err != nil {
			return err
		}
		for _, issue := range unfilled {
			pull, err := records.PullRequest(ctx, tx, issue.Key)
			if err != nil {
				return err
			}
			if pull != nil {
				pulls[issue.Key] = *pull
			}
		}
		return nil
	}); err != nil {
		return false, false, fmt.Errorf("read the records with no repository: %w", err)
	}
	if len(unfilled) == 0 {
		return false, false, nil
	}
	listed, err := client.ListIssues(ctx, project, nil)
	if err != nil {
		return false, true, fmt.Errorf("list Dispatch issues to fill the records with no repository: %w", err)
	}
	resolved := make(map[string]string, len(listed))
	for _, summary := range listed {
		resolved[summary.Key] = summary.ResolvedRepo
	}
	clones, err := sharedClones(stateDir)
	if err != nil {
		return false, true, err
	}
	type fill struct{ key, repo, branch, github string }
	fills := make([]fill, 0, len(unfilled))
	for _, issue := range unfilled {
		if resolved[issue.Key] == "" {
			leaveUnfilled(log, issue.Key, "", "Dispatch resolves no repository for it: set its repository in Dispatch")
			left = true
			continue
		}
		repo, err := ghrepo.Parse("the repository Dispatch resolves "+issue.Key+" to", resolved[issue.Key])
		if err != nil {
			leaveUnfilled(log, issue.Key, resolved[issue.Key], err.Error())
			left = true
			continue
		}
		why, values := fillOf(ctx, issue, repo, pulls, clones, stateDir, named, renames)
		if why != "" {
			leaveUnfilled(log, issue.Key, repo.String(), why)
			left = true
			continue
		}
		fills = append(fills, fill{issue.Key, values.repo, values.branch, values.github})
	}
	if len(fills) == 0 {
		return false, left, nil
	}
	if err := writeBackfill(ctx, pool, func(tx pgx.Tx) error {
		for _, fill := range fills {
			if err := records.FillIssueRepository(ctx, tx, fill.key, fill.repo, fill.branch, fill.github); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return false, true, err
	}
	for _, fill := range fills {
		log.Info("legion workflow backfill filled "+fill.key+" with "+fill.repo+"@"+fill.branch, "issue", fill.key, "repo", fill.repo,
			"defaultBranch", fill.branch, "githubRepo", fill.github)
	}
	return true, left, nil
}

// filling is what fillRepositories writes into one record: its repository, as the shared clone or
// GitHub spells it, its default branch and GitHub's spelling beside it (recordedSpelling).
type filling struct{ repo, branch, github string }

// fillOf is what issue's record is filled with from repo, the repository Dispatch resolves it to,
// as fillRepositories says, or why it is left, naming the step that settles it.
func fillOf(ctx context.Context, issue record.Issue, repo ghrepo.Repository, pulls map[string]record.PullRequest, clones []workspace.Workspace,
	stateDir string, named *defaultBranches, renames ghrepo.Renames) (string, filling) {
	read, err := backfillRead(ctx, named, repo)
	if err != nil {
		return fmt.Sprintf("the read of %s from GitHub failed: %v", repo, err), filling{}
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
		return fmt.Sprintf("two shared clones spell %s (%s): remove the one no issue works in", repo, strings.Join(spelled, ", ")), filling{}
	}
	if !renames.Same(spelling, read.Name.String()) {
		return fmt.Sprintf("GitHub renamed %s to %s, and the shared clone %s still has the old name: set the repository of %s in Dispatch to %s, or remove that clone if no issue works in it",
			repo, read.Name, spelled[0], issue.Key, read.Name), filling{}
	}
	if pull, ok := pulls[issue.Key]; ok && !renames.Same(pull.Repo, repo.String()) && !renames.Same(pull.Repo, read.Name.String()) {
		return fmt.Sprintf("Dispatch resolves it to %s, but its pull request #%d is on %s: set the issue's repository in Dispatch to %s (dispatch issue-update --issue %s --repo %s)",
			repo, pull.Number, pull.Repo, pull.Repo, issue.Key, pull.Repo), filling{}
	}
	workspaces, err := issueWorkspaces(stateDir, issue.Key)
	if err != nil {
		return err.Error(), filling{}
	}
	if dir, in, ok := workspaceElsewhere(workspaces, repo, read.Name, renames); ok {
		return fmt.Sprintf("Dispatch resolves it to %s, but its workspace %s is in %s: set the issue's repository in Dispatch to %s (dispatch issue-update --issue %s --repo %s)",
			repo, dir, in, in, issue.Key, in), filling{}
	}
	return "", filling{repo: spelling, branch: read.DefaultBranch, github: recordedSpelling(renames, spelling, read.Name)}
}

// backfillLater is the backfill the refresher's second job tries again while a record is left
// without its repository columns (backfillPending, rereadBadAnswers): backfillRepositories as the
// boot runs it, whose writes take the lock every fact applies under, so no fact applies with a
// copy of a record read before the fill. A fill that succeeds has the refresher publish the heard
// set again and read the live records again (republishHeard, refreshLive), so the filled record's
// repository is heard and cached; a failure of the store or of Dispatch's listing is logged and
// tried again on the job's backoff.
func (w *workflowRuntime) backfillLater(ctx context.Context) {
	filled, left, err := backfillRepositories(ctx, w.pool, w.records, w.dispatch, w.dispatchProject, w.stateDir, w.defaultBranches, w.repositories.renames, w.log)
	if err != nil {
		if ctx.Err() == nil {
			w.log.Warn("workflow backfill: the records without their repository columns were not read, listed or written, so the backfill tries them again",
				"retryIn", installReadRetry.Initial.String(), "error", err)
		}
		return
	}
	w.backfillPending.Store(left)
	if filled {
		w.heardPending.Store(true)
		wake(w.heardWake)
		w.wakeLive()
	}
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
func recordedSpelling(renames ghrepo.Renames, recorded string, github ghrepo.Repository) string {
	if partner, paired := renames.Partner(github.String()); paired && strings.EqualFold(partner.String(), recorded) {
		return partner.String()
	}
	return github.String()
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
func nameRepositories(ctx context.Context, pool *store.Pool, records record.Store, project string, named *defaultBranches, renames ghrepo.Renames, log *slog.Logger) (wrote, left bool, err error) {
	var unnamed []record.Issue
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var err error
		unnamed, err = records.UnnamedIssues(ctx, tx, project)
		return err
	}); err != nil {
		return false, false, fmt.Errorf("read the records with no GitHub spelling of their repository: %w", err)
	}
	names := make(map[string]string, len(unnamed))
	for _, issue := range unnamed {
		repo, err := issue.Repository()
		if err != nil {
			leaveUnfilled(log, issue.Key, issue.Repo, err.Error())
			left = true
			continue
		}
		read, err := backfillRead(ctx, named, repo)
		if err != nil {
			leaveUnfilled(log, issue.Key, issue.Repo, fmt.Sprintf("the read of %s from GitHub failed: %v", repo, err))
			left = true
			continue
		}
		if !renames.Same(read.Name.String(), issue.Repo) {
			leaveUnfilled(log, issue.Key, issue.Repo, fmt.Sprintf("GitHub renamed %s to %s, and a record keeps its repository name unless a pair of repository_renames names both; only a pre-release build of this daemon leaves a record with no GitHub spelling, so recreate that build's database",
				issue.Repo, read.Name))
			left = true
			continue
		}
		names[issue.Key] = recordedSpelling(renames, issue.Repo, read.Name)
	}
	if len(names) == 0 {
		return false, left, nil
	}
	if err := writeBackfill(ctx, pool, func(tx pgx.Tx) error {
		for _, issue := range unnamed {
			if name, ok := names[issue.Key]; ok {
				if err := records.NameIssueRepository(ctx, tx, issue.Key, issue.Repo, name); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		return false, true, err
	}
	for _, issue := range unnamed {
		if name, ok := names[issue.Key]; ok {
			log.Info("legion workflow backfill named "+issue.Key+"'s repository "+issue.Repo+" as GitHub spells it, "+name,
				"issue", issue.Key, "repo", issue.Repo, "githubRepo", name)
		}
	}
	return true, left, nil
}
