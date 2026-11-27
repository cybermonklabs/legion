package daemon

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/envoy/githubrate"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/ghbranch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
)

// defaultBranchTTL is how long a repository's default branch, as GitHub named it, stands before the
// next read of it asks GitHub again: an issue recorded after a repository's default branch is
// renamed takes the new name within it. An issue already recorded keeps the default branch it was
// recorded with (record.Issue.DefaultBranch), so a rename mid-issue is not followed.
const defaultBranchTTL = 10 * time.Minute

// defaultBranches is each repository as GitHub names it (ghbranch.Read: its own spelling of owner
// and name, and its default branch), read as the implement App — the App that creates an issue's
// branch and provisions its workspace — and kept per repository, in any spelling, for ttl from
// the read. Reads of one repository that overlap share one request to GitHub. A read GitHub
// refuses for any reason but its rate limit is returned and keeps nothing; one GitHub's rate limit
// refuses is answered from the read kept, however old, which then stands until the limit ends
// (Named), and one held from a record stands until the limit ends too (hold).
//
// A caller hands it a name a record or Dispatch carries, which after a configured rename's
// transfer is the name the repository moved away from, on an installation that no longer covers
// it. So the read is made of the repository GitHub holds that one under now (repos, the daemon's
// repository state, through repositories.serving): the token is minted for that repository's
// owner and the call addresses it. The rename rule is read here, where the token is minted, so
// that no caller hands an owner in and none can forget. A repository the rule leaves as it is is
// its own serving repository, so its read is the read it always was, and a nil repository state
// serves every name as it is — which only a test passes, since the boot builds one whatever the
// deployment configures (newRepositories, in daemon.go's plan).
//
// What is kept is keyed by the name the caller asked for, never the served one, since that is the
// name the caller holds and asks Kept for; the answer itself carries GitHub's own spelling of the
// repository it served, which is the name a record compares against the pair.
type defaultBranches struct {
	tokens    appauth.Tokens
	repos     *repositories
	githubAPI string
	ttl       time.Duration
	now       func() time.Time
	// onHold, when set, is told of each read GitHub refused for its rate limit that Named answered
	// from the branch it held instead (the workflow's heldThroughLimit, which says so and has the
	// refresher read it again once the limit has passed).
	onHold func(read limitedRead, held ghbranch.Repository)

	mu      sync.Mutex
	read    map[string]defaultBranchRead
	pending map[string]*pendingDefaultBranch
}

type defaultBranchRead struct {
	repository ghbranch.Repository
	at         time.Time
}

type pendingDefaultBranch struct {
	done       chan struct{}
	repository ghbranch.Repository
	err        error
}

// newDefaultBranches reads under githubAPI, the GitHub REST root ("" is https://api.github.com),
// with tokens' implement App, for the repository repos serves each asked-for one under (nil: each
// under its own name), keeping each answer it reads for ttl on now's clock (a branch held through
// GitHub's rate limit stands until the limit ends instead).
func newDefaultBranches(tokens appauth.Tokens, repos *repositories, githubAPI string, ttl time.Duration, now func() time.Time) *defaultBranches {
	return &defaultBranches{
		tokens: tokens, repos: repos, githubAPI: githubAPI, ttl: ttl, now: now,
		read: map[string]defaultBranchRead{}, pending: map[string]*pendingDefaultBranch{},
	}
}

// Named is repo as GitHub names it, with its default branch: the read made within ttl, or GitHub's
// answer now (fetch, which reads the repository GitHub holds repo under). The answer is kept, and
// asked for, under repo itself, whatever name the read was addressed at.
//
// A read GitHub refuses for its rate limit (defaultBranchLimit) is answered from the last read of
// repo, however old, when there is one: that answer then stands until the limit ends, so nothing
// reads repo inside it, and onHold is told. The limit says nothing about the repository, so every
// caller — an admission's preparation, a launch's, the refresher's — is served on the branch the
// daemon last read or held rather than failed by it. With no read of repo held, the refusal is
// returned.
func (d *defaultBranches) Named(ctx context.Context, repo ghrepo.Repository) (ghbranch.Repository, error) {
	key := strings.ToLower(repo.String())
	d.mu.Lock()
	if kept, ok := d.read[key]; ok && d.now().Before(kept.at.Add(d.ttl)) {
		d.mu.Unlock()
		return kept.repository, nil
	}
	if waiting, ok := d.pending[key]; ok {
		d.mu.Unlock()
		select {
		case <-waiting.done:
			return waiting.repository, waiting.err
		case <-ctx.Done():
			return ghbranch.Repository{}, ctx.Err()
		}
	}
	waiting := &pendingDefaultBranch{done: make(chan struct{})}
	d.pending[key] = waiting
	d.mu.Unlock()

	repository, err := d.fetch(ctx, repo)

	d.mu.Lock()
	var held *limitedRead
	if err == nil {
		d.read[key] = defaultBranchRead{repository: repository, at: d.now()}
	} else if kept, ok := d.read[key]; ok {
		if answer, wait, limited := defaultBranchLimit(err); limited {
			until := d.now().Add(wait)
			d.read[key] = defaultBranchRead{repository: kept.repository, at: until.Add(-d.ttl)}
			held = &limitedRead{repo: repo, answer: answer, err: err, wait: wait, until: until}
			repository, err = kept.repository, nil
		}
	}
	waiting.repository, waiting.err = repository, err
	delete(d.pending, key)
	close(waiting.done)
	d.mu.Unlock()
	if held != nil && d.onHold != nil {
		d.onHold(*held, repository)
	}
	return repository, err
}

// Kept is the last read of repo, in any spelling, however old, and asks GitHub nothing: what the
// workflow records an issue's repository and default branch from inside its transaction, which
// performs no I/O, once the read was made before it.
func (d *defaultBranches) Kept(repo string) (ghbranch.Repository, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	kept, ok := d.read[strings.ToLower(repo)]
	return kept.repository, ok
}

// hold keeps answer as the read of repo until until, when the wait GitHub asked for, in refusing
// the read of repo for its rate limit, ends: answer is the default branch a record of the project
// holds (holdLimited), served in place of the read GitHub refused. Until then Named answers it and
// asks GitHub nothing, so no read of repo is sent inside the limit; after it, Named reads GitHub
// again, and answers this one again if GitHub's limit refuses that read too. Kept answers it either
// way. A read Named keeps stands for ttl from when it was made, so the held one is kept as made ttl
// before until.
func (d *defaultBranches) hold(repo ghrepo.Repository, answer ghbranch.Repository, until time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.read[strings.ToLower(repo.String())] = defaultBranchRead{repository: answer, at: until.Add(-d.ttl)}
}

// fetch asks GitHub about the repository it holds repo under now (repositories.serving), on the
// implement App's token for that repository: after a configured rename's transfer the
// installation covering the repository is the owner it moved to, so a token minted for the owner
// repo names would reach nothing, and the address follows the token even though GitHub answers
// the old one by redirect.
func (d *defaultBranches) fetch(ctx context.Context, repo ghrepo.Repository) (ghbranch.Repository, error) {
	ctx = githubrate.WithCaller(ctx, callerDefaultBranch)
	served := d.repos.serving(repo)
	lease, err := d.tokens.Token(ctx, appauth.Implement, served.Owner())
	if err != nil {
		return ghbranch.Repository{}, fmt.Errorf("mint the implement App token to read the default branch of %s: %w", served, err)
	}
	return ghbranch.Read(ctx, githubrest.Client{Token: lease.Token, API: githubrest.RepositoryAPI(d.githubAPI, served)}, served)
}
