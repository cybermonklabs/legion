package daemon

import (
	"context"
	"log/slog"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/ghbranch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// launchReadRuntime is a workflow runtime over pool and records whose launch read
// (issueLaunchRepository) prepares no repository: each of cached is in the cache set, as the read
// of the live records leaves each repository a live record names, so a launch on one reads GitHub
// nothing and the read is the record's alone.
func launchReadRuntime(pool *store.Pool, records record.Store, log *slog.Logger, cached ...string) *workflowRuntime {
	w := &workflowRuntime{pool: pool, records: records, log: log}
	held := map[string]ghrepo.Repository{}
	for _, repo := range cached {
		held[strings.ToLower(repo)] = ghrepo.MustParse(repo)
	}
	w.storeCacheRepositories(held)
	return w
}

// launchedRepository is the repository and default branch a launch of key's claims works, as w's
// launch read answers them (workflowRuntime.issueLaunchRepository), for the tests that read those
// two and not what a correction left on the record.
func launchedRepository(ctx context.Context, w *workflowRuntime, key string) (ghrepo.Repository, string, error) {
	read, err := w.issueLaunchRepository(ctx, key)
	return read.Repo, read.DefaultBranch, err
}

// testRepositories serves every repository the daemon tests record issues on, each at main. A
// repository it does not name is one no preparation has read GitHub for, which the daemon answers
// as unread (repositories.lookup): GitHub's answer for a repository refuses it, and nothing else
// does.
var testRepositories = servedRepositories{"acme/widgets": "main", "sjawhar/legion": "main"}

type servedRepositories map[string]string

func (r servedRepositories) ResolveRepository(resolved string) (workflow.Resolution, error) {
	if branch, ok := r[resolved]; ok {
		return workflow.Resolution{Repo: resolved, DefaultBranch: branch, GitHubRepo: resolved}, nil
	}
	return workflow.Resolution{Refusal: "GitHub has not been read for " + resolved + " yet", Unread: true}, nil
}

// fill is the tests' seam that fills the repository state with repos, each at main, as a
// preparation that found both role Apps installed on it and GitHub naming it would leave it
// (prepareName), with no read of GitHub: fillRead of each.
func (r *repositories) fill(repos ...ghrepo.Repository) {
	for _, repo := range repos {
		r.fillRead(ghbranch.Repository{Name: repo, DefaultBranch: "main"})
	}
}

// fillRead fills read's entry as a preparation that found both role Apps installed on read.Name
// and GitHub naming it at read.DefaultBranch would: both Apps answered installed
// (answerInstalled), so the rename rule serves the name (servesRepository), and the read recorded
// as what an issue resolving to it is recorded on (recordRepository), so an admission on it reads
// GitHub no further (lookup).
func (r *repositories) fillRead(read ghbranch.Repository) {
	answerInstalled(r, read.Name.String(), true)
	r.recordRepository(read.Name.String(), read)
}

// answerInstalled keeps in name's entry GitHub's answer that both role Apps are installed on it,
// or that neither is, as one preparation's reads of it would (keepRead).
func answerInstalled(repos *repositories, name string, installed bool) {
	answer := appInstallation{answered: true, installed: installed}
	repos.keepRead(ghrepo.MustParse(name), appInstallations{implement: answer, review: answer}, "")
}
