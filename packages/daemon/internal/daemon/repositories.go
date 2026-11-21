package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/errgroup"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghbranch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/store/storetext"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// repositories is what the daemon knows of the repositories its project works in: GitHub's answers
// for each name it was read under (repositoryEntry), Dispatch's answer of its webhook App on it,
// and the repositories the project's live records name (live). An issue's repository is Dispatch's
// resolution of that issue (intake.DispatchIssue.ResolvedRepo), and a project works in each
// repository both role Apps are installed on, so nothing here decides which repository an issue
// names: it holds what GitHub answered for the names the daemon has prepared — an admission's
// (prepareRead), the refresher's at its start and while an answer is bad (prepareLive, readPairs),
// and a launch's (launchRepository) — which the admission of the next issue on a name (lookup), the
// rename rule (serving, servesRepository) and legion state (view) read. Readers take the read lock
// and writers the write lock, and nothing that holds either calls out.
type repositories struct {
	project string
	// renames is the deployment's repository equivalence (`repository_renames`,
	// config.Config.RepositoryRenames): a configured pair's two names are one repository wherever
	// a name is looked up (lookup), so an issue Dispatch resolves to the repository as it was is
	// recorded on the one it moved to, and the rename rule (serving) maps a recorded name to the
	// one GitHub holds it under now. The zero value answers as a comparison without case does,
	// while the deployment configures none.
	renames ghrepo.Renames

	mu sync.RWMutex
	// entries is GitHub's answers for each name a preparation read, by its lowercased
	// <owner>/<name> (repositoryEntry): one entry per repository, whose reads happen once however
	// many issues name it, since an issue on a name an entry records is recorded from that entry
	// (lookup) and reads GitHub no further.
	//
	// An entry is never dropped, and the reason is the spec's own rule rather than any cost a
	// sweep would carry: the facts that record an issue are kept once the issue is recorded, and a
	// refusal decides no later preparation (dispatch://INFRA-2543/spec, "Reading a fact at the
	// moment of acting on it"). An entry's recorded read is the first of those two, the name and
	// the default branch the issues on it are recorded with; the answers beside it are what legion
	// state and the rename rule read, and a name a role App is not installed on is read again at
	// every preparation of an issue on it, since only a recorded read decides an admission.
	//
	// What that costs is one case, and it is written here rather than left to a reader to find:
	// after a role App is uninstalled from a repository an entry records issues on, an issue on
	// that repository is still admitted — until the process starts again and builds the entries
	// from the preparations that follow it — and GitHub then refuses its roles' requests on that
	// repository, though a public repository may still be read. A stale entry decides admission
	// alone and never what a token reaches. Every build of a role's gh bundle reads again the
	// owners its App is installed on and fails closed on a read that fails
	// (internal/appauth/credential.go, "The owners the App is installed on are read at every
	// bundle (source's InstalledOwners, through installedLayout) … failing closed is this rule's
	// whole point"); the bundle's directories are built from that answer and from the rename rule
	// (servesRepository). So the cost is that admission, and what the roles meet next depends on
	// what was uninstalled. The entry's read is per repository (appauth's GET
	// /repos/{owner}/{repo}/installation) and a role's mint is per owner (Manager.Token), so an
	// App taken off ONE repository of a selected-repositories installation still mints for that
	// owner, the bundle builds, and GitHub refuses the roles' requests on that repository. An App
	// taken off the WHOLE account leaves that owner's cached lease in use until its expiry is no
	// more than appauth's refreshWindow away (Manager.lease), and the default directory cannot be
	// left out, so the bundles built in that time succeed and hold a token GitHub would no longer
	// mint; the first build that mints that owner for real fails, naming that owner and GitHub's
	// answer for it (mintBundle returns the named failure of the default owner rather than leaving
	// it out). runtime.ErrNoGitHubOwner is neither case: it is the App installed nowhere at all,
	// which an issue that has a repository never reaches, since bundleDirs always holds a directory
	// for that repository's own owner. No token reaches a repository because of any of it.
	entries map[string]*repositoryEntry
	// live is each repository a live issue record of the project names, by its lowercased
	// <owner>/<name>, as the record keeps GitHub's spelling of it (record.Issue.GitHubRepo): what
	// the refresher reads again while its answer is bad (badLive) and what a read of the live
	// records compares with the set before it (uncheckedLive). It is read from the records where
	// the repository caches' set is, at the boot and at every read of the live records
	// (workflowRuntime.refreshCacheRepositories), and added to at each recording (hearRecorded).
	// legion state does not read it: the state lists the repositories of its own read of the
	// records (view), so its list and its issues come from one snapshot. It is not kept in the
	// entries: a restart empties them while the records still name the repositories their trees
	// work in, and an entry holds a read for the repository of an issue Legion never records.
	live map[string]ghrepo.Repository
}

// repositoryEntry is what GitHub, and Dispatch of its webhook App, last answered for one name.
// Each answer is kept in the entry of the name it was read under: a read under a configured
// pair's `to` fills the `to`'s entry and never the `from`'s, so whether the rename rule serves
// the `from` is read from what GitHub answered for the `from` itself (servesRepository).
//
// appInstallations is each role App's installation on the name, from the last read of it GitHub
// answered: a later read that does not answer keeps them, so one failed read never turns a served
// name into an unserved one. readError is why GitHub's last read under the name did not answer
// for it — a refusal such as a 404 of the read of its name, or a read that did not answer at all
// — and "" once a preparation's every read answered. webhookApp is why Dispatch's webhook App is
// not known to be installed on the name (checkWebhookApp), "" when Dispatch answered that it is or
// was never asked, which webhookChecked tells apart.
//
// recorded is GitHub's spelling and default branch of the repository an issue that resolves to
// this name is recorded on, the zero value until a preparation found both role Apps installed and
// GitHub named the repository (recordRepository): the name's own read, GitHub's own name for it
// where GitHub renamed it (a 301 the read follows), or a configured pair's other name where both
// Apps answered not installed on this one and installed on that one (prepareRead).
type repositoryEntry struct {
	appInstallations
	readError      string
	webhookApp     string
	webhookChecked bool
	recorded       ghbranch.Repository
}

// appInstallation is GitHub's answer of one role App's installation on one name: answered once a
// read of it answered, and installed what that read answered.
type appInstallation struct{ answered, installed bool }

// appInstallations is each role App's installation on one name, the one shape of GitHub's answer
// there: what one preparation's reads of the name answered (readInstallations), and what an entry
// keeps of the last answer of each (repositoryEntry, keepRead).
type appInstallations struct{ implement, review appInstallation }

// of is role's answer in a.
func (a *appInstallations) of(role appauth.AppRole) *appInstallation {
	if role == appauth.Review {
		return &a.review
	}
	return &a.implement
}

// answered is whether GitHub answered both role Apps' installation reads of the name.
func (a appInstallations) answered() bool { return a.implement.answered && a.review.answered }

// served is whether GitHub answered that both role Apps are installed on the name.
func (a appInstallations) served() bool {
	return a.answered() && a.implement.installed && a.review.installed
}

// missing is each role App GitHub answered is not installed on the name, in appauth.Roles order.
func (a appInstallations) missing() []appauth.AppRole {
	var missing []appauth.AppRole
	for _, role := range appauth.Roles {
		if answer := *a.of(role); answer.answered && !answer.installed {
			missing = append(missing, role)
		}
	}
	return missing
}

func newRepositories(project string, renames ghrepo.Renames) *repositories {
	return &repositories{project: project, renames: renames, entries: map[string]*repositoryEntry{}, live: map[string]ghrepo.Repository{}}
}

// entry is key's entry, a lowercased <owner>/<name>, made empty where none is held. Its caller
// holds the write lock.
func (r *repositories) entry(key string) *repositoryEntry {
	key = strings.ToLower(key)
	held := r.entries[key]
	if held == nil {
		held = &repositoryEntry{}
		r.entries[key] = held
	}
	return held
}

// entryOf is a copy of the entry repo's name holds, compared without case, and the zero entry
// where none is held: what GitHub last answered for one name, as the rename pass reads it of a
// configured pair's two names (renameTransfer).
func (r *repositories) entryOf(repo string) repositoryEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if held := r.entries[strings.ToLower(repo)]; held != nil {
		return *held
	}
	return repositoryEntry{}
}

// serving is the repository GitHub holds a recorded one under now (ghrepo.Renames.Serving): the
// other name of its configured pair exactly when the recorded name is that pair's `from`, the
// pair's `to` is served and its `from` is not (servesLocked); every other recorded name, the
// pair's `to` among them, is answered unchanged. It is the one rule every GitHub call the daemon
// makes for a recorded tree reads — the owner its App token is minted for and, where the call is
// a REST write or a GraphQL query, the repository it addresses — since a record keeps the name it
// was written under until this release's one-time rename pass writes the pair's `to` over it
// (renamePass), while GitHub, after a transfer, holds the repository under the other name, on the
// other owner's installation.
//
// What the rule asks the entries is whether each of the pair's two names is served, one map
// lookup apiece, and never whether any repository of their owners is; ghrepo.Renames.Serving is
// where why that is the question stands.
//
// A nil repository state — a test that builds an outbox or a workflow without one — serves every
// recorded repository under its own name, as a deployment that configures no rename does.
func (r *repositories) serving(recorded ghrepo.Repository) ghrepo.Repository {
	if r == nil {
		return recorded
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.renames.Serving(recorded, r.servesLocked)
}

// same is whether a and b name one repository: the same name, compared without case as GitHub
// compares names, or the two names of a configured rename (ghrepo.Renames.Same). A nil repository
// state compares without case alone.
func (r *repositories) same(a, b string) bool {
	if r == nil {
		return strings.EqualFold(a, b)
	}
	return r.renames.Same(a, b)
}

// servesRepository is the rename rule's one question of a repository — is repo served — as every
// site of the rule asks it: serving, and the gh bundles' owner aliases and pairs
// (appauth.GitHubCredential, ControllerCredential, GitHubDirs, which the daemon hands this method
// as their `served`). It answers from repo's own entry under the read lock, with no I/O
// (servesLocked). A nil repository state serves nothing.
func (r *repositories) servesRepository(repo ghrepo.Repository) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.servesLocked(repo)
}

// servesLocked is servesRepository for a caller that holds the read lock: whether GitHub answered
// that both role Apps are installed on repo, read from repo's own entry, one map lookup by its
// lowercased <owner>/<name>, however many times a pair asks. Until GitHub has answered both reads
// of a name a configured pair holds, the pair's `to` is served and its `from` is not, which is the
// claim the operator configured: a read that failed at boot must not send every tree on the old
// name to the old owner's installation (api/handoff_rename_test.go records the outage of
// 2026-11-06). Once GitHub has answered, a later read that does not answer keeps the last answer
// (keepRead). A name no pair holds is served exactly once GitHub answered both Apps installed,
// which the rule never asks of it.
func (r *repositories) servesLocked(repo ghrepo.Repository) bool {
	if held := r.entries[strings.ToLower(repo.String())]; held != nil && held.answered() {
		return held.served()
	}
	if _, moved := r.renames.MovedTo(repo); moved {
		return false
	}
	_, paired := r.renames.Partner(repo.String())
	return paired
}

// lookup is the repository an issue that resolves to resolved, in any spelling, is recorded on —
// GitHub's spelling and its default branch, as the preparation that found both role Apps installed
// read them (recordRepository) — and whether the entries hold one. It reads the entries alone,
// under the name resolved carries and then the other name of the configured pair that names it
// (ghrepo.Under), so an issue resolved to the repository as it was is recorded on the one GitHub
// holds now, under one name: Resolution.Repo and Resolution.GitHubRepo agree, as a record demands
// (record.Store.PutIssue). A name no preparation has recorded a read for answers false: the
// preparation of an issue admission would record on it reads GitHub (prepareRead), or, in the
// boot's listing, leaves it for the recovery to read (leaveForRecovery), and the issue is
// recorded or refused on what GitHub says then.
func (r *repositories) lookup(resolved string) (ghbranch.Repository, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	// Each read is a map lookup under the lock already held, so it fails for no reason and its
	// error is always nil; ghrepo.Under holds the order the two names are read in.
	read, _ := ghrepo.Under(r.renames, resolved, func(name string) (ghbranch.Repository, error) {
		if held := r.entries[strings.ToLower(name)]; held != nil {
			return held.recorded, nil
		}
		return ghbranch.Repository{}, nil
	})
	return read, read != (ghbranch.Repository{})
}

// recordRepository keeps read, GitHub's spelling of a repository both role Apps answered installed
// on and its default branch, as what an issue that resolves to asked is recorded on, so that one
// repository is read once however many issues name it. The preparation that read it calls this
// before the issue's fact is applied, and the apply reads it back through lookup, which performs
// no I/O (workflowRuntime.ResolveRepository).
//
// asked is the name the issue resolves to and asks for again at its apply and at every later
// preparation. Where GitHub answered under another name — the 301 of a repository GitHub renamed,
// which the read follows, or a configured pair's other name, which both Apps answered installed
// on where neither is on asked (prepareRead) — the read is kept under that name too, so either
// name finds it: one repository's one read, reached by either name.
func (r *repositories) recordRepository(asked string, read ghbranch.Repository) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entry(read.Name.String()).recorded = read
	if !strings.EqualFold(asked, read.Name.String()) {
		r.entry(asked).recorded = read
	}
}

// keepRead keeps what one preparation read under name in name's entry, in one write (prepareName):
// each role App's answer in apps where its read answered, the last answer kept where it did not,
// and why the preparation's reads did not answer for the name (readError, "" once every read it
// made answered). It reports whether what the entry says of the Apps or of the read changed, so a
// bad answer is logged once and not at every read again.
func (r *repositories) keepRead(name ghrepo.Repository, apps appInstallations, readError string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	held := r.entry(name.String())
	before := *held
	for _, role := range appauth.Roles {
		if answer := *apps.of(role); answer.answered {
			*held.of(role) = answer
		}
	}
	held.readError = readError
	return held.appInstallations != before.appInstallations || held.readError != before.readError
}

// keepWebhookAnswer keeps gap, Dispatch's answer of its webhook App check (checkWebhookApp) for
// repo, in place of the answer last kept for it: "", Dispatch answering that its App is
// installed, keeps none.
func (r *repositories) keepWebhookAnswer(repo ghrepo.Repository, gap string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	held := r.entry(repo.String())
	held.webhookApp, held.webhookChecked = gap, true
}

// servedEntryLocked is the name the rename rule serves repo under (ghrepo.Renames.Serving) and that
// name's entry, nil where none is held, for a caller that holds the read lock.
func (r *repositories) servedEntryLocked(repo ghrepo.Repository) (ghrepo.Repository, *repositoryEntry) {
	served := r.renames.Serving(repo, r.servesLocked)
	return served, r.entries[strings.ToLower(served.String())]
}

// setLive replaces the repositories the project's live issue records name (live) with repos, by
// their lowercased <owner>/<name>, as a read of the records found them. The set owns repos from
// here on.
func (r *repositories) setLive(repos map[string]ghrepo.Repository) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.live = repos
}

// addLive adds repo to the repositories the project's live issue records name (live), for a
// record that first names it between two reads of the records. One the set already holds keeps
// the spelling it has.
func (r *repositories) addLive(repo ghrepo.Repository) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := strings.ToLower(repo.String())
	if _, held := r.live[key]; !held {
		r.live[key] = repo
	}
}

// liveNames is the repositories the project's live records name (live), as the set holds them now,
// by their lowercased <owner>/<name>.
func (r *repositories) liveNames() map[string]ghrepo.Repository {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return maps.Clone(r.live)
}

// isLive is whether a live record of the project names repo, compared without case (live).
func (r *repositories) isLive(repo ghrepo.Repository) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, held := r.live[strings.ToLower(repo.String())]
	return held
}

// badLive is each repository a live record names whose answer is bad, as the refresher reads it
// again (rereadBadAnswers), and why, one line each, sorted: GitHub has not answered both role
// Apps' installation reads of the name it is served under, a read of it did not answer for it
// (readError), a role App is not installed on it, or Dispatch's webhook App is not known to be
// installed on it. Each is read from the entry of the name the rename rule serves the repository
// under, which is the name its reads are made under (prepareLive). reread is each repository whose
// answer from GitHub is bad, for the refresher to prepare again; recheck is the served name of
// each whose one bad answer is its webhook App gap, for the refresher to ask Dispatch alone again,
// since GitHub's answers there are good.
func (r *repositories) badLive() (reread, recheck []ghrepo.Repository, reasons []string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, key := range slices.Sorted(maps.Keys(r.live)) {
		repo := r.live[key]
		served, held := r.servedEntryLocked(repo)
		var why string
		switch {
		case held == nil || !held.answered():
			why = "GitHub has not answered both role Apps' installation reads of " + served.String()
		case held.readError != "":
			why = held.readError
		case len(held.missing()) != 0:
			why = appauth.NotInstalledOn(held.missing(), served.String())
		case held.webhookApp != "":
			recheck = append(recheck, served)
			reasons = append(reasons, repo.String()+": "+held.webhookApp)
			continue
		default:
			continue
		}
		reread = append(reread, repo)
		reasons = append(reasons, repo.String()+": "+why)
	}
	return reread, recheck, reasons
}

// unansweredPairs is each name of a configured pair whose role Apps' installation reads GitHub has
// not answered both of yet, which the rename rule answers by default until it has
// (servesLocked), and why, one line each.
func (r *repositories) unansweredPairs() ([]ghrepo.Repository, []string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var names []ghrepo.Repository
	var reasons []string
	for _, pair := range r.renames.Pairs() {
		for _, name := range []ghrepo.Repository{pair.From, pair.To} {
			if held := r.entries[strings.ToLower(name.String())]; held != nil && held.answered() {
				continue
			}
			names = append(names, name)
			reasons = append(reasons, name.String()+": a configured rename's name GitHub has not answered both role Apps' installation reads of")
		}
	}
	return names, reasons
}

// uncheckedLive is the name each repository live names now, and before (lowercased <owner>/<name>)
// did not, is served under, where Dispatch was never asked of its webhook App on that name: the
// repositories a read of the live records added that no preparation checked (refreshLive).
func (r *repositories) uncheckedLive(before map[string]ghrepo.Repository) []ghrepo.Repository {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var unchecked []ghrepo.Repository
	for key, repo := range r.live {
		if _, held := before[key]; held {
			continue
		}
		if served, held := r.servedEntryLocked(repo); held == nil || !held.webhookChecked {
			unchecked = append(unchecked, served)
		}
	}
	return unchecked
}

// view is the state's repositories block (api.RepositoriesView) over live, the repositories the
// live records of the state's own read name by their lowercased <owner>/<name>
// (repositoriesOf, source.State): one entry for each, sorted by that key, under the name the
// records keep it under; the name the rename rule serves it under, where that is the other name
// of its configured pair; from the served name's own entry, each role App's installation once
// GitHub answered both reads and readError; and its repository cache from caches, matched
// whatever the case, where the Kubernetes runtime has made one. Each of those whose served name's
// entry holds a webhookApp is the block's withoutWebhookApp, under the name the records keep it
// under with that as its reason, in the same order: the one place the block reports Dispatch's
// webhook App. The rest of caches, each cache of a repository no live record names, kept until
// its retention ends (workflowRuntime.cacheRepositories), is the block's caches. It reads the
// entries alone and performs no I/O.
func (r *repositories) view(live map[string]ghrepo.Repository, caches []sandbox.CacheStatus) api.RepositoriesView {
	byRepo := make(map[string]sandbox.CacheStatus, len(caches))
	for _, cache := range caches {
		byRepo[strings.ToLower(cache.Repo.String())] = cache
	}
	view := api.RepositoriesView{Live: make([]api.LiveRepositoryView, 0, len(live))}
	r.mu.RLock()
	for _, key := range slices.Sorted(maps.Keys(live)) {
		repo := live[key]
		entry := api.LiveRepositoryView{Repo: repo.String()}
		served, held := r.servedEntryLocked(repo)
		if served != repo {
			entry.ServedAs = served.String()
		}
		if held != nil {
			if held.answered() {
				entry.Apps = &api.AppInstallationsView{Implement: held.implement.installed, Review: held.review.installed}
			}
			entry.ReadError = held.readError
			if held.webhookApp != "" {
				view.WithoutWebhookApp = append(view.WithoutWebhookApp, api.WebhookAppGapView{Repo: entry.Repo, Reason: held.webhookApp})
			}
		}
		if cache, held := byRepo[key]; held {
			entry.Cache = new(cacheView(cache))
			delete(byRepo, key)
		}
		view.Live = append(view.Live, entry)
	}
	r.mu.RUnlock()
	for _, cache := range caches {
		if _, unnamed := byRepo[strings.ToLower(cache.Repo.String())]; unnamed {
			view.Caches = append(view.Caches, cacheView(cache))
		}
	}
	return view
}

// cacheView is a repository cache as the state shows it.
func cacheView(cache sandbox.CacheStatus) api.CacheView {
	view := api.CacheView{
		Repo: cache.Repo.String(), Sandbox: cache.Sandbox, State: cache.State, Reason: cache.Reason, Ready: cache.Ready,
		Address: cache.Address, HealthError: cache.HealthError, RetainedUntil: cache.RetainedUntil,
	}
	if health := cache.Health; health != nil {
		view.Head, view.LastFetchAt, view.Bytes = health.Head, health.LastFetchAt, health.Bytes
	}
	return view
}

// checkWebhookApp is why Dispatch's GitHub App, the one GitHub sends repo's webhook events through
// to Envoy, is not known to be installed on repo, "" when Dispatch answers that it is: not
// installed, the check failed, or a Dispatch older than the check answers its route 404.
func checkWebhookApp(ctx context.Context, client dispatch.Client, repo ghrepo.Repository) string {
	installed, err := client.WebhookAppInstalled(ctx, repo.String())
	switch {
	case dispatch.Missing(err):
		return "this Dispatch does not answer the webhook App check"
	case err != nil:
		// legion state serves the gap's reason, so Dispatch's error is kept as any far side's
		// error text is.
		return "the webhook App check failed: " + storetext.FarSideError(err.Error())
	case !installed:
		return fmt.Sprintf("Dispatch's GitHub App is not installed on %s: GitHub sends Legion none of its events, nothing redelivers them, and no periodic pass makes up for them, so its issues stand still until the App is installed", repo)
	}
	return ""
}

// checkWebhookAppOn asks Dispatch whether its webhook App is installed on repo (checkWebhookApp),
// bounded by installReadAttempt, and keeps the answer for repo (repositories.keepWebhookAnswer). A
// check ctx's end cut short answered for the stop, not for Dispatch, so it keeps nothing.
func (w *workflowRuntime) checkWebhookAppOn(ctx context.Context, repo ghrepo.Repository) {
	check, cancel := context.WithTimeout(ctx, installReadAttempt)
	gap := checkWebhookApp(check, w.dispatch, repo)
	cancel()
	if ctx.Err() == nil {
		w.repositories.keepWebhookAnswer(repo, gap)
	}
}

// installReadRetry is how long the refresher waits before it runs one of its jobs again of its own
// while that job's cause stands (refreshRepositories): 30 s doubling to 10 min, without limit
// (Attempts is unused), back to 30 s once the cause is gone.
var installReadRetry = bootprobe.Retry{Initial: 30 * time.Second, Max: 10 * time.Minute}

// recoveryHoldBound is how long a recovery's release waits stay untold while its listing is held
// for the Dispatch consumer (recoverRepositories): the hold watchdog's warning interval
// (defaultHoldWarnEvery), by which a hold that has not released is a fault worth naming.
var recoveryHoldBound = defaultHoldWarnEvery

// installReadAttempt bounds one read of GitHub or Dispatch a preparation or the rename pass makes:
// as long as any one GitHub call the boot waits for (appMintAttempt), so a GitHub that answers
// slowly is still answered for, while a read GitHub has not answered by then is a read that did
// not answer, and a repository GitHub hangs on is read again later rather than holding up the
// rest. It bounds each installation read (readInstallations), each webhook App check
// (checkWebhookAppOn), each read of a repository's name and default branch (readName) that a
// preparation makes (prepareName, whichever caller prepares: the refresher, a launch or an
// admission) or the rename pass makes of a configured pair's `to` (renameTransfer), an
// admission's read again of the default branch of a repository an entry records issues on
// (prepareUnrecorded), and each read of a record's repository for its backfill (backfillRead).
// Nothing bounds a preparation as a whole: repositoryReads repositories are prepared at once, so a
// preparation of n repositories, four reads each, takes at most ceil(n / repositoryReads) × 4 of
// these.
var installReadAttempt = 15 * time.Second

// repositoryReads is how many repositories the refresher prepares at once.
const repositoryReads = 8

// readInstallations reads both role Apps' installations on name (appauth.Installations.InstalledOn,
// per repository, since an owner's installation may cover only some of its repositories), one after
// the other, each bounded by installReadAttempt, and reads on past a read that fails: apps holds
// each read that answered, and unread is why the reads did not answer for name, naming the first
// role App whose read failed, "" when every one answered.
//
// A name a configured pair holds is read under that name alone, following no redirect
// (InstalledUnder): GitHub redirects the name a transfer moved a repository away from to where the
// repository is now, and a name GitHub redirects is no repository of its own (ghrepo.Renames), so
// what the rename rule reads of a pair's `from` is GitHub's answer for the `from` itself, not
// installed once the transfer has happened (servesLocked). Every other name is read as GitHub
// answers for the repository wherever it is now, a redirect followed, as a repository GitHub
// renamed with no pair configured is (InstalledOn).
func (w *workflowRuntime) readInstallations(ctx context.Context, name ghrepo.Repository) (apps appInstallations, unread string) {
	installed := w.installs.InstalledOn
	if _, paired := w.repositories.renames.Partner(name.String()); paired {
		installed = w.installs.InstalledUnder
	}
	for _, role := range appauth.Roles {
		read, cancel := context.WithTimeout(ctx, installReadAttempt)
		answered, err := installed(read, role, name)
		cancel()
		switch {
		case err == nil:
			*apps.of(role) = appInstallation{answered: true, installed: answered}
		case unread == "":
			// The reason is served by legion state (repositories.live's readError) and fails a
			// launch that prepares the name, so GitHub's error is kept as any far side's error
			// text is.
			unread = fmt.Sprintf("the %s App's installation on %s could not be read: %s", role, name, storetext.FarSideError(err.Error()))
		}
	}
	return apps, unread
}

// nameAnswer is what one preparation's reads under one name answered (prepareName): under, the name
// they were made under; missing, each role App GitHub answered is not installed on it, where both
// installation reads answered; read, GitHub's name and default branch of the repository read, where
// that read answered or a record's branch was served in its place; limited, the read GitHub's rate
// limit refused with no branch served in its place, nil for any other answer; and readError, why
// the reads did not answer for the name, "" where every read made answered. changed is whether the
// name's entry changed (keepRead), so a bad answer is logged once and not at every read again.
type nameAnswer struct {
	under     ghrepo.Repository
	missing   []appauth.AppRole
	read      ghbranch.Repository
	limited   *limitedRead
	readError string
	changed   bool
}

// refusal is why the name cannot be worked now, "" where it can: each role App GitHub answered is
// not installed on it, naming the Apps and the name, and otherwise the first read that did not
// answer, naming the read.
func (a nameAnswer) refusal() string {
	if len(a.missing) != 0 {
		return appauth.NotInstalledOn(a.missing, a.under.String())
	}
	return a.readError
}

// prepareName is the one step every preparation of a name runs — the refresher's and a launch's
// (prepareLive), an admission's (prepareRead) and a configured pair's own (readPairs) — so what
// legion state and the rename rule read of a name is written by one sequence. Every read is made
// under under, and every answer is kept in under's entry:
//
//  1. both role Apps' installations on under (readInstallations);
//  2. once both reads answered, installed or not, the name and default branch of named as GitHub
//     has them (readName: defaultBranches.Named, which reads the repository GitHub holds named
//     under and keeps its read, and its TTL, under named itself), bounded by installReadAttempt.
//     A read GitHub's rate limit refused is served on the default branch a record of the project
//     holds for named, where branches is set and a record holds one (holdLimited); one not so
//     served is a read that did not answer, and is answered as limited for the caller's own rule;
//  3. once both role Apps answered installed and GitHub named the repository, Dispatch's webhook
//     App on under (checkWebhookAppOn), spelled as GitHub spells it where GitHub named it under
//     that name, which is the spelling the record keeps and legion state shows;
//  4. one write of the entry (keepRead): each installation read that answered, the last answer
//     kept where one did not, and why the reads did not answer for under — the first installation
//     read that failed, and otherwise the read of named.
//
// A configured pair's own read names no repository (named is the zero value) and reads the
// installations alone. Answers ctx's end cut short are the stop's: none is kept, and ctx's error
// is returned.
func (w *workflowRuntime) prepareName(ctx context.Context, under, named ghrepo.Repository, branches recordBranches) (nameAnswer, error) {
	apps, unread := w.readInstallations(ctx, under)
	answer := nameAnswer{under: under, readError: unread}
	if unread == "" {
		answer.missing = apps.missing()
	}
	if unread == "" && named != (ghrepo.Repository{}) {
		read, readError, err := w.readName(ctx, named)
		if limit, wait, limited := defaultBranchLimit(err); limited {
			refused := limitedRead{repo: named, answer: limit, err: err, wait: wait, until: w.defaultBranches.now().Add(wait)}
			answer.limited = &refused
			if branches != nil {
				if held, ok, holdErr := w.holdLimited(refused, branches); holdErr == nil && ok {
					read, readError, answer.limited = held, "", nil
				}
			}
		}
		if readError != "" {
			answer.readError = readError
		} else {
			answer.read = read
			if len(answer.missing) == 0 {
				checked := under
				if strings.EqualFold(read.Name.String(), under.String()) {
					checked = read.Name
				}
				w.checkWebhookAppOn(ctx, checked)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nameAnswer{}, err
	}
	answer.changed = w.repositories.keepRead(under, apps, answer.readError)
	return answer, nil
}

// readName is GitHub's name and default branch of repo (defaultBranches.Named), one read bounded
// by installReadAttempt, as a preparation (prepareName) and the rename pass (renameTransfer) read
// it: the read where it answered, and otherwise why it did not, in the words legion state and the
// rename pass's line name it with, beside the read's own error for the caller's rule of a rate
// limit (defaultBranchLimit). The reason legion state serves (repositories.live's readError) holds
// err's text as the daemon keeps any far side's error text (storetext.FarSideError).
func (w *workflowRuntime) readName(ctx context.Context, repo ghrepo.Repository) (read ghbranch.Repository, unread string, err error) {
	attempt, cancel := context.WithTimeout(ctx, installReadAttempt)
	defer cancel()
	read, err = w.defaultBranches.Named(attempt, repo)
	if err != nil {
		unread = fmt.Sprintf("the name and default branch of %s could not be read: %s", repo, storetext.FarSideError(err.Error()))
	}
	return read, unread, err
}

// prepareLive is the preparation of repo, a repository a record names or a launch works: the one
// preparation step (prepareName) — both role Apps' installations, the name and default branch, and
// Dispatch's webhook App — made under the name the rename rule serves repo under (serving) and
// kept in that name's entry. The read of the name and default branch is of repo itself, so
// defaultBranches keeps it under the name the record keeps, and a read of it GitHub's rate limit
// refused is served on the default branch a record of the project holds for repo, where one does
// (holdLimited, from branches, the records read once for the pass or the launch that prepares).
// Each read is bounded by installReadAttempt, so a read GitHub or Dispatch never answers holds up
// neither the refresher nor a launch.
//
// It answers GitHub's read of repo, and why repo cannot be worked now (nameAnswer.refusal): a role
// App GitHub answered is not installed on the served name, naming the App and the name, and
// otherwise the first read that did not answer, naming the read. A bad answer wakes the
// refresher's second job, changes no record, and is logged once at WARN, when it is new
// (keepRead), saying whether a live record names repo: only such a repository is in legion state
// and is read again by the refresher (badLive), while one only finished records name, which the
// refresher's first job prepares too (prepareAtStart), is shown and read again only once a live
// record names it. Where both Apps are installed and GitHub named the repository, the read is
// what an issue resolving to repo is recorded on (recordRepository), so an admission on it reads
// GitHub no further. Either answer of the installation reads that GitHub gave begins the
// repository's recovery where an admission's preparation left it unread (readAnswered).
func (w *workflowRuntime) prepareLive(ctx context.Context, repo ghrepo.Repository, branches recordBranches) (ghbranch.Repository, error) {
	served := w.repositories.serving(repo)
	answer, err := w.prepareName(ctx, served, repo, branches)
	if err != nil {
		return ghbranch.Repository{}, err
	}
	if refusal := answer.refusal(); refusal != "" {
		if answer.changed {
			attrs := []any{"repo", repo.String(), "servedAs", served.String(), "reason", refusal}
			if w.repositories.isLive(repo) {
				attrs = append(attrs, "live", true, "retryIn", installReadRetry.Initial.String())
			} else {
				attrs = append(attrs, "live", false)
			}
			w.log.Warn("workflow: GitHub's answer for a repository leaves it unworkable for now; no record changes, and legion state shows it and the refresher reads it again while a live record names it", attrs...)
		}
		wake(w.rereadWake)
		if len(answer.missing) != 0 {
			w.readAnswered(repo)
		}
		return ghbranch.Repository{}, errors.New(refusal)
	}
	w.repositories.recordRepository(repo.String(), answer.read)
	w.readAnswered(repo)
	return answer.read, nil
}

// readPairs reads both names of each configured pair (readPairNames), or, with onlyUnanswered set,
// only the names GitHub has not answered both role Apps' installation reads of yet
// (unansweredPairs).
func (w *workflowRuntime) readPairs(ctx context.Context, onlyUnanswered bool) {
	var names []ghrepo.Repository
	if onlyUnanswered {
		names, _ = w.repositories.unansweredPairs()
	} else {
		for _, pair := range w.repositories.renames.Pairs() {
			names = append(names, pair.From, pair.To)
		}
	}
	w.readPairNames(ctx, names)
}

// readPairNames reads both role Apps' installations on each of names, names a configured pair
// holds, repositoryReads at a time, each under its own name, following no redirect
// (readInstallations), and never through serving, with no read of its name and no webhook App
// check (prepareName, naming no repository), and keeps each answer in that name's own entry: what
// GitHub answers there is what the rename rule reads (servesLocked), and until it answers, the
// rule serves each pair's `to` and not its `from`. The refresher reads the pairs so (readPairs),
// and so does the rename pass for a pair's name the repository state holds no answer for yet
// (renameTransfer).
func (w *workflowRuntime) readPairNames(ctx context.Context, names []ghrepo.Repository) {
	eachRepository(names, func(name ghrepo.Repository) {
		answer, err := w.prepareName(ctx, name, ghrepo.Repository{}, nil)
		if err == nil && answer.changed && answer.readError != "" {
			w.log.Warn("workflow: GitHub did not answer the installation reads of a configured rename's name, so the rename rule serves the pair as configured until it does",
				"repo", name.String(), "reason", answer.readError, "retryIn", installReadRetry.Initial.String())
		}
	})
}

// eachRepository runs read for each of repos, repositoryReads at a time, and returns once every one
// has.
func eachRepository(repos []ghrepo.Repository, read func(ghrepo.Repository)) {
	var group errgroup.Group
	group.SetLimit(repositoryReads)
	for _, repo := range repos {
		group.Go(func() error {
			read(repo)
			return nil
		})
	}
	_ = group.Wait() // each read keeps its own answer, so none fails the group
}

// prepareAtStart is the refresher's first job, the boot's preparation of the repositories its
// records name, which runs once the API serves rather than inside the boot, whose reads of GitHub
// would otherwise bound how long it takes and refuse it on a repository GitHub does not answer for.
// It reads both role Apps' installations on both names of each configured pair, each under its own
// name (readPairs), and then prepares each distinct repository a record of the project names
// (prepareLive), repositoryReads at a time, with the records' default branches read once for the
// pass where GitHub's rate limit asks for them (onceRecordedBranches); a record with no GitHub
// spelling of its repository is passed by. A 404, or a role App that is not installed, is kept in
// the entry and logged once; a read that does not answer is kept as such. Either is shown in
// legion state, and read again by the refresher's next job (rereadBadAnswers), where a live record
// names the repository, and not for a repository only finished records name; no record changes.
// Until it has answered, the state shows each repository with no Apps, and the rename rule serves
// each pair as configured (servesLocked).
func (w *workflowRuntime) prepareAtStart(ctx context.Context) {
	w.readPairs(ctx, false)
	repos, err := w.recordedRepositories(ctx)
	if err != nil {
		w.log.Warn("workflow: the repositories the records name were not read, so the refresher prepares the live records' repositories as it reads them again",
			"error", err)
		return
	}
	branches := w.onceRecordedBranches(ctx)
	eachRepository(repos, func(repo ghrepo.Repository) { _, _ = w.prepareLive(ctx, repo, branches) })
	if ctx.Err() == nil {
		w.log.Info("legion workflow prepared the repositories its records name", "repositories", repositoryNames(repos))
	}
}

// recordedRepositories is each distinct repository a record of the project names, as the record
// keeps GitHub's spelling of it (record.Issue.GitHubRepo, repositoriesOf), sorted, read in one
// read-only transaction bounded by appMintAttempt; a record that keeps no GitHub spelling is passed
// by, and one whose spelling is no <owner>/<name> refuses the read, as it refuses the boot
// (heardAtBoot).
func (w *workflowRuntime) recordedRepositories(ctx context.Context) ([]ghrepo.Repository, error) {
	read, cancel := context.WithTimeout(ctx, appMintAttempt)
	defer cancel()
	var issues []record.Issue
	if err := pgx.BeginTxFunc(read, w.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var err error
		issues, err = w.records.Issues(read, tx)
		return err
	}); err != nil {
		return nil, fmt.Errorf("read the repositories the project's records name: %w", err)
	}
	named, err := repositoriesOf(issues)
	if err != nil {
		return nil, err
	}
	return sortedRepositories(named), nil
}

// rereadBadAnswers is the refresher's second job, run on installReadRetry while any of its causes
// stands (badAnswers): it reads again both role Apps' installations on each configured pair's name
// GitHub has not answered both reads of yet, under its own name (readPairs); prepares again each
// repository a live record names whose answer from GitHub is bad (badLive, prepareLive, with the
// records' default branches read once for the pass where GitHub's rate limit asks for them), since
// nothing else tells the daemon that an App was installed or a repository came back after the
// boot, and legion state and the rename rule read these answers; asks Dispatch alone again of its
// webhook App on each whose one bad answer is that App's gap (checkWebhookAppOn), since only
// Dispatch's answer there can change; and tries again the backfill of each record still without
// its repository columns (backfillLater).
func (w *workflowRuntime) rereadBadAnswers(ctx context.Context) {
	w.readPairs(ctx, true)
	reread, recheck, _ := w.repositories.badLive()
	branches := w.onceRecordedBranches(ctx)
	eachRepository(reread, func(repo ghrepo.Repository) { _, _ = w.prepareLive(ctx, repo, branches) })
	eachRepository(recheck, func(served ghrepo.Repository) { w.checkWebhookAppOn(ctx, served) })
	if w.backfillPending.Load() {
		w.backfillLater(ctx)
	}
}

// badAnswers is why the refresher's second job reads GitHub again (rereadBadAnswers), one line each:
// each live repository whose answer is bad (badLive), each pair's name GitHub has not answered
// (unansweredPairs), and a record still without its repository columns (backfillPending).
func (w *workflowRuntime) badAnswers() []string {
	_, _, reasons := w.repositories.badLive()
	_, pairs := w.repositories.unansweredPairs()
	reasons = append(reasons, pairs...)
	if w.backfillPending.Load() {
		reasons = append(reasons, "a record without its repository columns, whose backfill is tried again")
	}
	return reasons
}

// wake asks the refresher for one run of the job ch wakes, holding one request at most: a request
// made while one waits is covered by the same run, since each job reads whole what it acts on. A
// nil channel, a runtime a test built without its refresher, wakes nothing.
func wake(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// wakeLive asks the refresher for one read of the live records (refreshLive), the project records'
// wake at the commit of each write that can change them (projectRecords.live).
func (w *workflowRuntime) wakeLive() { wake(w.liveWake) }

// retrying is one of the refresher's jobs on installReadRetry: wait is the wait it runs again
// after, 30 s doubling to 10 min, and due when it next runs of its own, zero while its cause does
// not stand.
type retrying struct {
	wait time.Duration
	due  time.Time
}

// stand arms r from now while its cause stands, once, and sets it back to the first wait once the
// cause is gone.
func (r *retrying) stand(standing bool, now time.Time) {
	switch {
	case !standing:
		r.wait, r.due = installReadRetry.Initial, time.Time{}
	case r.due.IsZero():
		if r.wait == 0 {
			r.wait = installReadRetry.Initial
		}
		r.due = now.Add(r.wait)
	}
}

// ran moves r's wait on, its job having run of its own: the next run is armed from then.
func (r *retrying) ran() {
	r.wait, r.due = min(2*r.wait, installReadRetry.Max), time.Time{}
}

// isDue is whether at, a job's due time, has come by now; a zero at never does.
func isDue(at, now time.Time) bool { return !at.IsZero() && !at.After(now) }

// refreshRepositories is the refresher: one goroutine that runs six jobs until ctx ends, each
// started by its own cause, and none of them a read of anything Dispatch holds about repositories.
//
//  1. The boot's preparation of the repositories the records name (prepareAtStart), once, as it
//     starts.
//  2. Bad answers read again (rereadBadAnswers), on installReadRetry while any stands
//     (badAnswers): a live repository with no answer, a role App that is not installed, a read
//     that did not answer or a webhook App gap, which alone has only Dispatch asked again; a
//     configured pair's name GitHub has not answered; a record still without its repository
//     columns. A preparation outside the refresher that leaves a bad answer wakes it
//     (rereadWake), so its clock is taken at once.
//  3. The unread repositories recovered (recoverRepositories), at once when leaveUnread,
//     leaveForRecovery or readAnswered wakes it (recoverWake, markUnread), and on installReadRetry
//     while any repository is unread or to recover (unreadReasons, recoveringReasons). While a held
//     recovery has release waits to tell and time left to recoveryHoldBound (recoveryHoldLeft), the
//     wait is cut to that time, so the bound is checked when it falls, not up to a backoff later;
//     such a cut wait does not move the backoff on. Once the bound has passed, a run that did not
//     tell the waits waits the backoff as any failed run does, so a failure past the bound never
//     turns the refresher into a busy loop.
//  4. The heard set published again (republishHeard), at once when a record's own consumer update
//     was refused (hearRecorded, heardWake), and on installReadRetry while it fails (heardPending).
//  5. A held default branch read again (rereadHeldBranches), at the end of each rate limit a
//     default branch was held through (heldThroughLimit, heldWake).
//  6. The live records read again (refreshLive), at the commit of each write that can change them
//     (projectRecords, liveWake), and on installReadRetry while that read fails.
//
// Each job's clock is its own (retrying), so a wake of one never moves another's wait. The jobs
// share the goroutine, so one waits while another runs: a recovery woken during the first job's
// preparation runs once it is done, at most ceil(n / repositoryReads) × 4 × installReadAttempt
// later for n repositories.
func (w *workflowRuntime) refreshRepositories(ctx context.Context) {
	after := w.recheckAfter
	if after == nil {
		after = time.After
	}
	w.prepareAtStart(ctx)
	var reread, recovery, heard, live retrying
	liveFailed := false
	for {
		now := time.Now()
		bad := w.badAnswers()
		reread.stand(len(bad) != 0, now)
		unread := append(w.recoveringReasons(), w.unreadReasons()...)
		recovery.stand(len(unread) != 0, now)
		heard.stand(w.heardPending.Load(), now)
		live.stand(liveFailed, now)
		recoverAt, cut := recovery.due, false
		if left, held := w.recoveryHoldLeft(); held && left > 0 && !recoverAt.IsZero() && now.Add(left).Before(recoverAt) {
			recoverAt, cut = now.Add(left), true
		}
		heldAt := w.heldDue()
		var next time.Time
		for _, at := range []time.Time{reread.due, recoverAt, heard.due, live.due, heldAt} {
			if !at.IsZero() && (next.IsZero() || at.Before(next)) {
				next = at
			}
		}
		var timer <-chan time.Time
		if !next.IsZero() {
			timer = after(max(0, next.Sub(now)))
		}
		select {
		case <-ctx.Done():
			return
		case <-w.rereadWake:
			// A bad answer was kept outside the refresher: the next turn arms job 2's clock.
		case <-w.heldWake:
			// A default branch was held through a rate limit: the next turn arms job 5's clock.
		case <-w.recoverWake:
			w.recoverRepositories(ctx)
		case <-w.heardWake:
			_ = w.republishHeard(ctx)
		case <-w.liveWake:
			liveFailed = w.refreshLive(ctx) != nil
		case <-timer:
			now := time.Now()
			if isDue(reread.due, now) {
				w.log.Info("workflow: reading GitHub again for the repositories whose last answer was bad",
					"waiting", bad, "waited", reread.wait.String())
				w.rereadBadAnswers(ctx)
				reread.ran()
			}
			if isDue(recoverAt, now) {
				w.log.Info("workflow: recovering again the issues of the repositories GitHub did not answer for",
					"waiting", unread, "waited", recovery.wait.String())
				w.recoverRepositories(ctx)
				if cut {
					recovery.due = time.Time{}
				} else {
					recovery.ran()
				}
			}
			if isDue(heard.due, now) {
				_ = w.republishHeard(ctx)
				heard.ran()
			}
			if isDue(live.due, now) {
				liveFailed = w.refreshLive(ctx) != nil
				live.ran()
			}
			if isDue(heldAt, now) {
				w.rereadHeldBranches(ctx, now)
			}
		}
	}
}

// republishHeard is the refresher's fourth job: it publishes the whole heard set again after
// JetStream refused an update a repository the daemon works asked for (heardPending, hearOrPend).
// The set is every repository the records name (heardRepositories), read in a read-only
// transaction of its own, with every repository the held set names and every one whose own update
// was refused (heardMissed), so a publication adds and never takes away: the records are never
// deleted, and a launch's repository is heard as a record's is (workLaunched). It tells GitHub
// intake of the set under no transaction of the pool (updateSubscription): the update is a stream
// call, and a missing durable consumer has it create one, which reads the same pool for the
// stream's stored position, so a transaction waiting on it asks the pool for a second connection
// while holding one, and every connection of the pool sat idle in a transaction of that shape once
// the stream answered slowly (INFRA-3169). It holds heardMu for both steps, the lock every write of
// the held set takes (hearRepository), so a recording whose repository the snapshot missed adds it
// after this publication and is never narrowed away by it. It clears heardPending once the
// publication applies, and a failure is logged and left pending, for the refresher to run it again
// on installReadRetry.
func (w *workflowRuntime) republishHeard(ctx context.Context) error {
	w.heardMu.Lock()
	defer w.heardMu.Unlock()
	publish, cancel := context.WithTimeout(ctx, appMintAttempt)
	defer cancel()
	var recorded []ghrepo.Repository
	err := pgx.BeginTxFunc(publish, w.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var err error
		recorded, err = w.heardRepositories(publish, tx)
		return err
	})
	var heard []ghrepo.Repository
	if err == nil {
		named := map[string]ghrepo.Repository{}
		if held := w.heard.Load(); held != nil {
			for _, repo := range *held {
				named[strings.ToLower(repo.String())] = repo
			}
		}
		for _, repo := range slices.Concat(w.heardMissed, recorded) {
			named[strings.ToLower(repo.String())] = repo
		}
		heard = sortedRepositories(named)
		err = w.updateSubscription(publish, heard)
	}
	if err != nil {
		if ctx.Err() == nil {
			w.log.Warn("workflow: the repositories GitHub intake hears were not published again; publishing them again",
				"retryIn", installReadRetry.Initial.String(), "error", err)
		}
		return err
	}
	w.heard.Store(&heard)
	w.heardMissed = nil
	w.heardPending.Store(false)
	w.log.Info("legion workflow published again the repositories GitHub intake hears", "heard", repositoryNames(heard))
	return nil
}

// heldDue is when the first default branch held through a rate limit is to be read again
// (heldReads), zero while none is held.
func (w *workflowRuntime) heldDue() time.Time {
	w.heldMu.Lock()
	defer w.heldMu.Unlock()
	var due time.Time
	for _, read := range w.heldReads {
		if due.IsZero() || read.until.Before(due) {
			due = read.until
		}
	}
	return due
}

// rereadHeldBranches is the refresher's fifth job: each default branch held through a rate limit
// whose limit has ended by now (heldThroughLimit) is read again (defaultBranches.Named, bounded by
// appMintAttempt), and one GitHub's limit refuses again is held again, which names it once more
// (Named, onHold).
func (w *workflowRuntime) rereadHeldBranches(ctx context.Context, now time.Time) {
	w.heldMu.Lock()
	var due []limitedRead
	for key, read := range w.heldReads {
		if !read.until.After(now) {
			due = append(due, read)
			delete(w.heldReads, key)
		}
	}
	w.heldMu.Unlock()
	for _, read := range due {
		attempt, cancel := context.WithTimeout(ctx, appMintAttempt)
		_, err := w.defaultBranches.Named(attempt, read.repo)
		cancel()
		if err != nil && ctx.Err() == nil {
			w.log.Warn("workflow: the read of a default branch held through GitHub's rate limit failed after the limit, so the next read of it asks GitHub again",
				"repo", read.repo.String(), "error", err)
		}
	}
}

// refreshLive is the refresher's sixth job, the live records read again: the cache set and the
// repositories the live records name (refreshCacheRepositories), read in one transaction under the
// lock every fact applies under (intake.SerializeFacts), so a record committed meanwhile is either
// in the read or names its repository after it (hearRecorded), and then Dispatch's webhook App
// checked on each repository the read added that no preparation checked (uncheckedLive), each
// check bounded by installReadAttempt and repositoryReads of them at once. Each write that can
// change the live records asks for it at its commit (projectRecords), one read covering every
// request made before it starts; a read that fails is logged and run again on installReadRetry.
func (w *workflowRuntime) refreshLive(ctx context.Context) error {
	before := w.repositories.liveNames()
	read, cancel := context.WithTimeout(ctx, appMintAttempt)
	err := serializedTx(read, w.pool, "serialize the read of the live records with admission", func(tx pgx.Tx) error {
		return w.refreshCacheRepositories(read, tx)
	})
	cancel()
	if err != nil {
		if ctx.Err() == nil {
			w.log.Warn("workflow: the live records were not read again for the repository caches; reading them again",
				"retryIn", installReadRetry.Initial.String(), "error", err)
		}
		return err
	}
	eachRepository(w.repositories.uncheckedLive(before), func(repo ghrepo.Repository) { w.checkWebhookAppOn(ctx, repo) })
	return nil
}

// recoveryHoldLeft is the time left to recoveryHoldBound for a recovery whose listing is held with
// release waits to tell, zero once the bound has passed, and whether one is. The waits are those of
// the repositories to recover and of the repositories left unread alike: a pass that would let one
// of the latter go puts it back while the listing is held (recoverRepositories), and its waits are
// told at the bound as the former's are, so the refresher's wait is cut to the bound for both.
func (w *workflowRuntime) recoveryHoldLeft() (time.Duration, bool) {
	w.recoveringMu.Lock()
	defer w.recoveringMu.Unlock()
	if w.recoveryHeldSince.IsZero() {
		return 0, false
	}
	waits := false
	for repo := range w.recovering {
		waits = waits || len(w.releaseWaits[repo]) != 0
	}
	for repo := range w.unread {
		waits = waits || len(w.releaseWaits[repo]) != 0
	}
	if !waits {
		return 0, false
	}
	return max(0, recoveryHoldBound-time.Since(w.recoveryHeldSince)), true
}

// recoveringReasons is every repository still to recover, as the refresher logs what it waits on.
func (w *workflowRuntime) recoveringReasons() []string {
	w.recoveringMu.Lock()
	defer w.recoveringMu.Unlock()
	reasons := make([]string, 0, len(w.recovering))
	for repo := range w.recovering {
		reasons = append(reasons, repo+": its issues are to be recovered from Dispatch's listing")
	}
	slices.Sort(reasons)
	return reasons
}

// recoverRepositories recovers the repositories readAnswered named, each one a preparation GitHub
// answered for after an earlier one's reads went unanswered: one read of Dispatch's project
// listing, applied as the
// boot's reconcile applies it (reconcileListing), scoped to the issues the workflow does not record
// that resolve to one of them, whose parent chain in the listing reaches one that does (a child on
// a served repository under a parent that was unread), or whose apply said nothing while one of
// them was unread (noteUnread: an issue that took its repository from a parent, or from the
// project's default, changed meanwhile). Those are what an event applied while the repository was
// unread left: it said nothing and recorded nothing. A recorded issue is left out: its own events
// carried it, and a snapshot with no actor applied mid-run could overwrite a session's write. Each
// issue then ends where its event would have ended had the repository been served: a labelled
// root admitted or queued, a child of a live tree recorded under it (parents first), and a
// person's issue left as it is. A listing the consumer may still be behind is held as the boot's
// is (admission's pending), and released by pollHoldRelease. Then the architects are told, once,
// of every child a release_children left unrecorded on one of them that is still unrecorded: each
// child's architect is the one that owns its parent when the record holds that parent in the tree,
// else the tree's (tellReleaseWaits). While the listing stays held, that telling waits, but for
// recoveryHoldBound at most: a hold that outlasts it (the consumer stuck below the stream's last
// position, an event that fails at every delivery, say) is told the waits still unrecorded then,
// once, and the full recovery still runs when the hold releases. The clock starts when a run first
// finds the listing held with waits to tell, and runs on across holds released and made again,
// so a hold that flaps is told at the bound all the same; it stops when the recovery commits. The
// repositories are let go once
// both commit; a failed one stays to recover, tried again on the refresher's backoff. A restart
// loses the set, and the boot's reconcile is that recovery; what a release left waiting is told to
// no one then.
//
// Every repository whose reads GitHub has not answered (leaveUnread), or that
// the boot's listing left unread without reading GitHub (leaveForRecovery), is listed with them,
// at every pass while it is unread: its issues' applies said nothing and recorded nothing, and
// the one thing that reads GitHub for such a repository is the preparation of an issue admission
// would record on it, which this listing makes. Being listed is not being recovered. A repository
// is recovered by a pass only once GitHub has answered for it (recovering, which readAnswered moves
// it to, and not left unread again) and nothing fell silent for it while the pass ran
// (settleRecovery): a pass whose own re-preparation GitHub failed again tells the architect
// nothing, keeps its release waits and lists it again on the refresher's backoff, never in a
// loop. The architect's notice says GitHub answered for the repository again, so a pass that ran
// while GitHub still did not answer must not send it, nor drop the waits that it would tell.
//
// The silenced issues of every listed repository are taken out of the set for the pass and put
// back only by a pass that does not finish (putBackSilenced). Each of them is in the listing's
// scope, so its apply here either ends it or notes it again (noteUnread), and what the set holds
// for a repository once the listing is applied is what fell silent for it since the pass began:
// an issue GitHub failed for before another issue's preparation got GitHub's answer in the same
// pass, or an event that fell silent meanwhile. That issue is in the next pass's scope, which
// readAnswered's request runs at once.
func (w *workflowRuntime) recoverRepositories(ctx context.Context) {
	w.recoveringMu.Lock()
	repos := make(map[string]bool, len(w.recovering)+len(w.unread))
	maps.Copy(repos, w.recovering)
	for repo := range w.unread {
		repos[repo] = true
	}
	taken := make(map[string]map[string]bool, len(repos))
	silenced := map[string]bool{}
	waits := make(map[string]map[string]releaseWait, len(repos))
	// marks is each listed repository's last mark as the pass takes its set (unreadMarks, zero
	// for none), which settleRecovery compares to let a repository go only where no preparation
	// left it unread while the pass ran.
	marks := make(map[string]uint64, len(repos))
	for repo := range repos {
		if keys := w.silenced[repo]; len(keys) != 0 {
			taken[repo] = keys
			maps.Copy(silenced, keys)
			delete(w.silenced, repo)
		}
		if held := w.releaseWaits[repo]; len(held) != 0 {
			waits[repo] = maps.Clone(held)
		}
		marks[repo] = w.unreadMarks[repo]
	}
	w.recoveringMu.Unlock()
	if len(repos) == 0 {
		return
	}
	names := slices.Sorted(maps.Keys(repos))
	attempt, cancel := context.WithTimeout(ctx, appMintAttempt)
	defer cancel()
	issues, err := w.dispatch.ListIssues(attempt, w.dispatchProject, nil)
	// todo is each repository, lowercased, that an unrecorded todo issue of the listing that
	// admission would record (admissionBound: handed to Legion, or under a parent) resolves to:
	// the only issue whose preparation reads GitHub for a repository no entry records issues on
	// (prepareUnrecorded passes every other by, a person's own todo issue among them), so the
	// only one that can drop its marker.
	// keep is asked of every summary of the listing, against every record, which is where the
	// records that say unrecorded are read.
	todo := map[string]bool{}
	if err == nil {
		scope := recoveredOn(repos, silenced, issues)
		err = w.reconcileListing(attempt, issues, func(records map[string]*record.Issue, summary dispatch.IssueSummary) bool {
			if records[summary.Key] == nil && admissionBound(intake.ListedIssue(summary)) {
				todo[strings.ToLower(summary.ResolvedRepo)] = true
			}
			return scope(records, summary)
		}, false)
	}
	if err != nil {
		w.putBackSilenced(taken)
		w.log.Warn("workflow: the issues of the repositories to recover were not recovered from Dispatch's listing; recovering them again",
			"repositories", names, "error", err)
		return
	}
	answered, told, dropped := w.settleRecovery(names, waits, todo, marks)
	held := w.admission != nil && w.admission.Held()
	// heldBack is whether this pass put repositories it would let go back for the hold, which
	// keeps the hold's clock running for them: a pass that otherwise commits must not reset it.
	heldBack := false
	if waiting := waitingLetGo(dropped); len(waiting) != 0 {
		// The listing waits on the consumer (admission's pending), so a child it records is not
		// recorded yet: telling an architect now that nothing will ever record one would be
		// false of a child this very listing is about to record. Those repositories are left
		// unread with their waits, and a later pass lets them go once the hold has released, or
		// tells them once the hold has lasted recoveryHoldBound, on the clock the waits of the
		// repositories to recover are told on: a hold that never releases would otherwise keep
		// them untold, and listed again on the backoff, for the life of the daemon.
		if held && !w.heldPastBound() {
			w.putBackLetGo(waiting)
			heldBack = true
			w.log.Info("workflow: the recovery's listing waits on the Dispatch consumer, so the repositories it would stop reading GitHub for are read again until it is released",
				"repositories", letGoNames(waiting))
		} else if err := w.tellLetGoWaits(attempt, waiting, issues); err != nil {
			w.putBackLetGo(waiting)
			w.putBackSilenced(taken)
			w.log.Warn("workflow: the architects of the children left on repositories the daemon stops reading GitHub for were not told, so the repositories stay unread and a later pass tells them",
				"repositories", letGoNames(waiting), "error", err)
			return
		}
	}
	if len(told) != 0 && held {
		// The listing waits on the consumer (admission's pending), so a child it records is not
		// recorded yet: the waits are told once it is released, or once the hold has lasted
		// recoveryHoldBound, whichever comes first. The recovery itself runs again until the
		// hold releases, so its silenced issues stay in its scope.
		w.putBackSilenced(taken)
		if !w.heldPastBound() {
			w.log.Info("workflow: the recovery's listing waits on the Dispatch consumer; its release waits are told once it is released", "repositories", answered)
			return
		}
		if err := w.tellReleaseWaits(attempt, answered, told); err != nil {
			w.log.Warn("workflow: the release waits of a recovery held past its bound were not told; telling them again", "repositories", answered, "error", err)
			return
		}
		w.log.Warn("workflow: the recovery's listing has waited on the Dispatch consumer past its bound, so its release waits still unrecorded are told now",
			"repositories", answered, "bound", recoveryHoldBound.String())
		w.recoveringMu.Lock()
		for _, repo := range answered {
			for key := range waits[repo] {
				delete(w.releaseWaits[repo], key)
			}
		}
		w.recoveringMu.Unlock()
		return
	}
	if err := w.tellReleaseWaits(attempt, answered, told); err != nil {
		w.putBackSilenced(taken)
		w.log.Warn("workflow: the release waits of the repositories GitHub answered for again were not told; recovering them again",
			"repositories", answered, "error", err)
		return
	}
	w.recoveringMu.Lock()
	if !heldBack {
		w.recoveryHeldSince = time.Time{}
	}
	for _, repo := range answered {
		delete(w.recovering, repo)
		for key := range waits[repo] {
			delete(w.releaseWaits[repo], key)
		}
	}
	w.recoveringMu.Unlock()
	if len(answered) != 0 {
		w.log.Info("workflow: recovered the issues of the repositories GitHub answered for again", "repositories", answered)
	}
}

// heldPastBound is whether a recovery's listing, found held now with release waits to tell, has
// been held for recoveryHoldBound: the clock starts at the first pass that finds it so
// (recoveryHeldSince) and runs on across holds released and made again, so a hold that flaps is
// told at the bound all the same. The waits of the repositories to recover and of the repositories
// a pass would let go run on this one clock (recoveryHoldLeft).
func (w *workflowRuntime) heldPastBound() bool {
	w.recoveringMu.Lock()
	defer w.recoveringMu.Unlock()
	if w.recoveryHeldSince.IsZero() {
		w.recoveryHeldSince = time.Now()
	}
	return time.Since(w.recoveryHeldSince) >= recoveryHoldBound
}

// settleRecovery is, of names, the repositories a pass whose listing has applied recovered, with
// the release waits to tell for them (of waits, each repository's as the pass began): each
// GitHub answered for (recovering) that is not left unread again and that nothing fell silent
// for since the pass took its silenced issues (recoverRepositories). A repository still unread
// is never one of them, whatever the pass did: its re-preparation's failure was swallowed
// (prepareRead), so the listing succeeded while GitHub answered nothing.
//
// It also lets go of each repository left unread that no preparation will ever read GitHub for
// again, with nothing fallen silent for it: one the listing holds no unrecorded todo issue on
// that admission would record (todo), its only such issue having left todo, or having lost its
// label and its parent, before a read answered, and one an entry now records issues on (lookup),
// which a preparation reads no further (prepareUnrecorded). Only an answered read drops a marker
// otherwise (readAnswered), so for either kind the refresher would list the whole project on its
// backoff, up to every 10 min, for ever. An issue that comes back to todo
// on it is prepared afresh and leaves it unread again if GitHub still does not answer.
//
// What a release left to that repository's recovery is taken out with its marker, in this one
// step, and returned for its architects to be told (letGo, tellLetGoWaits): the two belong
// together, since once the marker is gone no pass lists the repository again, so a wait left
// behind is one no recovery would ever read and no architect would ever hear of, while its
// release_children answered that the child was released. A telling that fails puts both back
// (putBackLetGo), so the next pass finds the repository as this one did. Each is let go with why
// (letGoWhy), which is what its architects are told.
//
// Neither is settled while a preparation marked the repository during the pass: its mark is not
// the one the pass took (marks, unreadMarks). A marker already present is no evidence that nothing
// happened, since leaveUnread writes nothing new over one, and the pass's own window is where it
// matters: an issue a person moves to todo after the listing was read is prepared, finds GitHub
// failing and the marker held, and its apply then waits on the fact lock the pass's reconcile
// holds, so it notes its silence only after this settles. Let go then, the repository is listed by
// no later pass, and the note is one nothing reads again: the issue stands unrecorded, which is
// the defect readAnswered's recovery exists to end. Kept, it is listed again on the backoff, with
// that issue in its scope. An answer GitHub gave during the pass marks the repository too
// (readAnswered), so a repository is counted recovered only by a pass that began after it.
func (w *workflowRuntime) settleRecovery(names []string, waits map[string]map[string]releaseWait, todo map[string]bool, marks map[string]uint64) (answered []string, told map[string]releaseWait, dropped []letGo) {
	// The lock order is recoveringMu first and the repository state's lock (repositories.mu)
	// nested inside it, which is sound because nothing holding the state's lock ever takes
	// recoveringMu (awaitRecovery says the same). These lookups are made before recoveringMu all
	// the same, not because the order requires it, but because what they read is whether an entry
	// records issues on each repository, which is a fact about the entries and not about this
	// pass's set.
	readElsewhere := make(map[string]bool, len(names))
	for _, repo := range names {
		_, readElsewhere[repo] = w.repositories.lookup(repo)
	}
	told = map[string]releaseWait{}
	w.recoveringMu.Lock()
	for _, repo := range names {
		reason, unread := w.unread[repo]
		quiet := len(w.silenced[repo]) == 0 && w.unreadMarks[repo] == marks[repo]
		switch {
		case unread:
			if !quiet || (todo[repo] && !readElsewhere[repo]) {
				continue
			}
			// A repository the listing holds no todo issue on is let go for that, whatever else
			// answers for it now; one it holds a todo issue on is let go only because another
			// answer than a preparation's read of GitHub stands for it.
			why := letGoNoTodo
			if todo[repo] {
				why = letGoAnsweredElsewhere
			}
			dropped = append(dropped, letGo{repo: repo, reason: reason, why: why, waits: w.releaseWaits[repo]})
			delete(w.unread, repo)
			delete(w.releaseWaits, repo)
			delete(w.unreadMarks, repo)
		case w.recovering[repo] && quiet:
			answered = append(answered, repo)
			maps.Copy(told, waits[repo])
			delete(w.unreadMarks, repo)
		}
	}
	w.recoveringMu.Unlock()
	if len(dropped) != 0 {
		w.log.Info("workflow: no issue is left for a preparation to read GitHub for on these repositories GitHub did not answer for, so the daemon stops reading them again",
			"repositories", letGoNames(dropped))
	}
	return answered, told, dropped
}

// letGo is a repository a pass stopped waiting for GitHub to answer for (settleRecovery), with
// everything that left the daemon's memory with its marker: the reason it was left unread, and
// the children a release_children left to its recovery, by key, with why it was let go.
type letGo struct {
	repo   string
	reason string
	why    letGoWhy
	waits  map[string]releaseWait
}

// letGoWhy is why a pass let a repository left unread go, which is the first thing its architects
// are told (tellLetGoWaits): each kind has its own opening, so the notice says what is true of the
// repository it names.
type letGoWhy uint8

const (
	// letGoNoTodo is a repository the pass's listing holds no unrecorded todo issue on that
	// admission would record, the only issue whose preparation reads GitHub for it.
	letGoNoTodo letGoWhy = iota
	// letGoAnsweredElsewhere is a repository such an issue is still todo on that a read GitHub
	// answered at another preparation (recordRepository) now answers for (lookup): an issue on it
	// is prepared from that answer and asks GitHub nothing a marker could wait on.
	letGoAnsweredElsewhere
)

// waitingLetGo is each of dropped a release left a child to: the repositories whose letting go
// an architect is owed a word about. A repository no release waits on is let go whatever the
// pass found, since there is nobody to tell.
func waitingLetGo(dropped []letGo) []letGo {
	var waiting []letGo
	for _, gone := range dropped {
		if len(gone.waits) != 0 {
			waiting = append(waiting, gone)
		}
	}
	return waiting
}

// letGoNames is the repository of each of dropped, as the refresher logs them.
func letGoNames(dropped []letGo) []string {
	names := make([]string, 0, len(dropped))
	for _, gone := range dropped {
		names = append(names, gone.repo)
	}
	return names
}

// putBackLetGo leaves each repository of dropped unread again, with the children a release left
// to its recovery, when the telling that goes with letting it go did not reach the architects
// (tellLetGoWaits): the marker and those waits leave together or not at all, so a pass that
// could not tell them leaves the next one exactly what it found. A marker a preparation wrote
// since the pass let the repository go is kept as it stands, with the mark it carries.
func (w *workflowRuntime) putBackLetGo(dropped []letGo) {
	w.recoveringMu.Lock()
	defer w.recoveringMu.Unlock()
	for _, gone := range dropped {
		if w.unread == nil {
			w.unread = map[string]string{}
		}
		if _, marked := w.unread[gone.repo]; !marked {
			w.unread[gone.repo] = gone.reason
		}
		if len(gone.waits) == 0 {
			continue
		}
		if w.releaseWaits == nil {
			w.releaseWaits = map[string]map[string]releaseWait{}
		}
		if w.releaseWaits[gone.repo] == nil {
			w.releaseWaits[gone.repo] = map[string]releaseWait{}
		}
		maps.Copy(w.releaseWaits[gone.repo], gone.waits)
	}
}

// putBackSilenced returns to the silenced set the issues a pass took out of it
// (recoverRepositories) when the pass does not finish, beside any noted since.
func (w *workflowRuntime) putBackSilenced(taken map[string]map[string]bool) {
	w.recoveringMu.Lock()
	defer w.recoveringMu.Unlock()
	for repo, keys := range taken {
		if w.silenced == nil {
			w.silenced = map[string]map[string]bool{}
		}
		if w.silenced[repo] == nil {
			w.silenced[repo] = map[string]bool{}
		}
		maps.Copy(w.silenced[repo], keys)
	}
}

// releaseWait is a child release_children left unrecorded while a repository was unread
// (awaitRecovery): the tree it was released in, and its Dispatch parent.
type releaseWait struct {
	tree, parent string
}

// noteUnread is the workflow's Config.NoteUnread: key's apply said nothing because resolved was
// unread. It is called inside the apply's transaction, so it is noted before any recovery can
// take resolved's issues, under the fact lock, and so before that recovery's scope is
// read. A rolled-back apply leaves a note that only takes an unrecorded issue into the scope.
func (w *workflowRuntime) noteUnread(key, resolved string) {
	repo := strings.ToLower(resolved)
	w.recoveringMu.Lock()
	defer w.recoveringMu.Unlock()
	if w.silenced == nil {
		w.silenced = map[string]map[string]bool{}
	}
	if w.silenced[repo] == nil {
		w.silenced[repo] = map[string]bool{}
	}
	w.silenced[repo][key] = true
}

// leaveUnread records that GitHub did not answer the reads a preparation made for repo
// (prepareRead): the issue's apply says nothing and records nothing, and the refresher lists the
// repository's issues again on installReadRetry's backoff (refreshRepositories, whose third job
// runs while unreadReasons names any), each listing's preparation reading GitHub again, and
// recovers the issues it left from Dispatch's listing (recoverRepositories) once a read answers.
//
// The recovery is woken only when nothing reads the repository again already: it was not left
// unread before, and it is not among the repositories to recover (recovering), which the
// refresher recovers of its own on the same backoff. The marker is dropped by an answer
// (readAnswered), which wakes a recovery of its own, or by a recovery that finds nothing left on
// the repository for a preparation to read GitHub for (settleRecovery). So a GitHub that keeps
// failing is read again on the backoff alone, and one that fails and answers by turns runs at
// most one recovery between two of the backoff's own: the recovery's own re-preparation finds the
// marker still here, or the repository still to recover, and asks for nothing, which is what
// keeps the two out of a loop.
//
// It says so once: when the repository was not left unread before, or was left so by the boot's
// listing, which asked GitHub nothing (unreadAtBoot), since this is then the first read of it
// GitHub failed.
func (w *workflowRuntime) leaveUnread(repo ghrepo.Repository, reason string) {
	if previous, held := w.markUnread(repo, reason); held && previous != unreadAtBoot {
		return
	}
	w.log.Warn("workflow: GitHub did not answer for the repository an issue names, so the issue is left and the repository read again",
		"repo", repo.String(), "reason", reason)
}

// leaveForRecovery leaves repo, a repository no entry records issues on that a todo issue of the
// boot's listing resolves to, unread without a read of GitHub (prepareListed says why). It writes the
// marker leaveUnread writes, under the same rules, so it is one kind of unread repository and not
// a second: the refresher's recovery (recoverRepositories) lists the issues on it and its own
// preparation reads GitHub, as for a repository whose reads went unanswered, and lets it go only
// where settleRecovery would let that one go. The issue's apply at the boot says nothing, records
// nothing and is noted for that recovery (noteUnread). No release wait is held on the repository
// at the boot, since a restart loses them all, so a recovery that lets it go — its issue having
// left todo before GitHub answered — tells an architect only of a child released after the boot,
// and what its notice says is true of that child: no todo issue labelled for Legion or under a
// parent names the repository any longer.
func (w *workflowRuntime) leaveForRecovery(repo ghrepo.Repository) {
	if _, held := w.markUnread(repo, unreadAtBoot); held {
		return
	}
	w.log.Info("workflow: the boot's listing reads GitHub for no repository, so the issues on this one are left for the recovery after the boot to read it",
		"repo", repo.String())
}

// unreadAtBoot is why the boot's listing leaves a repository unread (leaveForRecovery).
const unreadAtBoot = "GitHub has not been read for it yet: the boot's listing reads GitHub for no repository, and the recovery after the boot reads it"

// markUnread leaves repo unread with reason, and wakes the refresher's recovery, which reads it
// again (recoverRepositories), unless something reads it again already: it was left unread
// before, or it is among the repositories to recover (recovering), which the refresher recovers
// again of its own. It answers the reason the repository was left unread with before, and
// whether it was.
//
// Every call marks the repository afresh (unreadMarks), the marker held before or not: a marker
// already present says nothing about whether a preparation failed since a pass took its set, and
// leaveUnread's callers return at once when it was, so the mark is the one trace the pass can
// compare (settleRecovery).
func (w *workflowRuntime) markUnread(repo ghrepo.Repository, reason string) (previous string, held bool) {
	key := strings.ToLower(repo.String())
	w.recoveringMu.Lock()
	previous, held = w.unread[key]
	recovering := w.recovering[key]
	if w.unread == nil {
		w.unread = map[string]string{}
	}
	w.unread[key] = reason
	w.markLocked(key)
	w.recoveringMu.Unlock()
	if !held && !recovering {
		wake(w.recoverWake)
	}
	return previous, held
}

// markLocked gives repo a mark no repository has held before (unreadMarked), so a mark that
// changed is told from one that did not by equality alone, however often a repository is marked,
// let go and marked again. Its caller holds recoveringMu.
func (w *workflowRuntime) markLocked(repo string) {
	if w.unreadMarks == nil {
		w.unreadMarks = map[string]uint64{}
	}
	w.unreadMarked++
	w.unreadMarks[repo] = w.unreadMarked
}

// readAnswered drops repo from the repositories left unread: GitHub answered the preparation's
// reads, whether the answer recorded the issue or refused it, so nothing waits on a read of it
// any longer. A repository that was left unread is moved to the repositories to recover
// (recovering), and the refresher's recovery is woken to recover it (recoverRepositories). The
// issues the earlier preparations left said nothing and recorded nothing,
// and the preparation that got the answer may be another issue's event: dropping the marker
// alone took the repository out of every recovery, and those issues stood unadmitted until the
// next boot.
func (w *workflowRuntime) readAnswered(repo ghrepo.Repository) {
	key := strings.ToLower(repo.String())
	w.recoveringMu.Lock()
	_, waiting := w.unread[key]
	if waiting {
		delete(w.unread, key)
		if w.recovering == nil {
			w.recovering = map[string]bool{}
		}
		w.recovering[key] = true
		// A pass already running took its set before this answer, so it may have listed the
		// repository before the issues the answer frees were in its scope: the mark keeps it from
		// counting the repository recovered, and the wake below runs the pass that does.
		w.markLocked(key)
	}
	w.recoveringMu.Unlock()
	if waiting {
		wake(w.recoverWake)
	}
}

// unreadReasons is every repository whose reads GitHub has not answered, as the refresher logs
// what it waits on: while any is, it recovers again of its own, its listing's preparations
// reading GitHub again, and so recovers the issues each left.
func (w *workflowRuntime) unreadReasons() []string {
	w.recoveringMu.Lock()
	defer w.recoveringMu.Unlock()
	reasons := make([]string, 0, len(w.unread))
	for repo, reason := range w.unread {
		reasons = append(reasons, repo+": "+reason)
	}
	slices.Sort(reasons)
	return reasons
}

// awaitRecovery is api.Options.AwaitRecovery: child, released in tree under parent, is left
// unrecorded while repo is unread, and the recovery of repo tells an architect if it is still
// unrecorded then (tellReleaseWaits). It answers whether it held the wait, which is whether repo is
// unread now: no entry records issues on it (lookup), and GitHub's reads for it went unanswered
// (the marker). A repository a pass has stopped waiting on since release_children read it unread
// (settleRecovery) is no longer, and a wait held on it would be read by no recovery and told to no
// architect, so it is refused and the route falls back to the child-release retry.
//
// Both readings, the entry's and the marker, are made under recoveringMu, the lock a pass takes
// its set and its waits under (recoverRepositories) and settleRecovery drops the marker under, so
// the answer and the filing are one step against them. With the reads made outside the lock, a
// call descheduled between the two read the repository unread, then filed the wait after the pass
// had taken its set, or after it had committed: no pass held the repository to recover any
// longer, so the child was told to no architect and recorded by nothing until the next boot.
// Nesting the repository state's lock (repositories.mu) inside this one is sound: nothing holding
// the state's lock ever takes recoveringMu, since the state calls out to nothing.
func (w *workflowRuntime) awaitRecovery(repo, tree, child, parent string) bool {
	w.recoveringMu.Lock()
	defer w.recoveringMu.Unlock()
	if _, recorded := w.repositories.lookup(repo); recorded {
		return false
	}
	repo = strings.ToLower(repo)
	if _, unread := w.unread[repo]; !unread {
		return false
	}
	if w.releaseWaits == nil {
		w.releaseWaits = map[string]map[string]releaseWait{}
	}
	if w.releaseWaits[repo] == nil {
		w.releaseWaits[repo] = map[string]releaseWait{}
	}
	w.releaseWaits[repo][child] = releaseWait{tree: tree, parent: parent}
	return true
}

// tellReleaseWaits tells the architects of the children of waits the recovery of repos left
// unrecorded what to release again, each child with whether its parent is recorded now: one
// child-release-stalled notice for each issue a child's notice is queued on, which is its parent
// when the record holds that parent in the child's tree, and else the tree root. The owner walk
// takes a notice on the parent to the architect that owns the parent, which is the architect that
// releases the parent's children; a parent the record does not hold, or holds in another tree, has
// no architect of this tree, so the child is the tree's architect's to release.
func (w *workflowRuntime) tellReleaseWaits(ctx context.Context, repos []string, waits map[string]releaseWait) error {
	return w.tellWaitingArchitects(ctx, []waitTelling{{waits: waits, why: fmt.Sprintf(
		"GitHub answered for %s again (both role Apps' installation reads, and the read of its name and default branch), and the daemon recovered its issues from Dispatch's listing, but these children a release_children released while it was unread are still not recorded",
		strings.Join(repos, ", "))}},
		func(child string, wait releaseWait, parentRecorded bool) string {
			if !parentRecorded {
				return fmt.Sprintf("%s (%s)", child, releaseParentFirst(child, wait))
			}
			return fmt.Sprintf("%s (its parent %s is recorded now: release %s again)", child, wait.parent, child)
		})
}

// tellLetGoWaits tells the architects, in the same notices tellReleaseWaits writes, which children a
// release_children left to the recovery of the repositories a pass has just let go
// (settleRecovery), so that no recovery will ever record them. The notice opens with why the
// repository was let go, since that is what decides what is true of it (letGoWhy): no todo issue
// labelled for Legion or under a parent names it now, the only issues whose preparation reads
// GitHub for such a repository, or such an issue does and an earlier answer of GitHub's answers
// for it without that read. Repositories let go for the two are told in two
// notices, each with its own opening, written in one transaction.
//
// Each child is named with the status listing, Dispatch's own project listing of this pass, gives
// it, since that status is why nothing records it: a child a person moved out of todo is recorded
// again only once it is todo again. A child whose parent the workflow does not record is named
// with that parent instead, as tellReleaseWaits names it: a release_children files a child under
// its parent's unread repository when the parent is what waits (enterOnUnreadRepository), and the
// child, todo all along, is recorded only under a recorded parent, so the step is the parent's
// release first. A child no wait is left for is told nothing, and so is a repository whose waits
// are empty.
//
// It is the pair of tellReleaseWaits and not a second telling of its kind: the two share one
// body, which decides which children are still unrecorded, reads each one's parent and writes one
// notice per issue a child's notice is queued on, and differ in the reason's opening and in the line each child is named on
// alone. The architect hears child-release-stalled either way, so no reader of the notices learns
// a new kind.
func (w *workflowRuntime) tellLetGoWaits(ctx context.Context, dropped []letGo, listing []dispatch.IssueSummary) error {
	repos := map[letGoWhy][]string{}
	waits := map[letGoWhy]map[string]releaseWait{}
	for _, gone := range dropped {
		if len(gone.waits) == 0 {
			continue
		}
		repos[gone.why] = append(repos[gone.why], gone.repo)
		if waits[gone.why] == nil {
			waits[gone.why] = map[string]releaseWait{}
		}
		maps.Copy(waits[gone.why], gone.waits)
	}
	if len(waits) == 0 {
		return nil
	}
	listed := make(map[string]string, len(listing))
	for _, summary := range listing {
		listed[summary.Key] = summary.Status
	}
	openings := map[letGoWhy]string{
		letGoNoTodo:            "the daemon no longer waits for GitHub to answer for %s, because no todo issue labelled for Legion or under a parent names it now; these children a release_children released while it was unread were never recorded",
		letGoAnsweredElsewhere: "the daemon no longer waits for GitHub to answer for %s, because a read GitHub answered for it at another issue's preparation answers for it now; these children a release_children released while it was unread were never recorded",
	}
	var tellings []waitTelling
	for _, why := range []letGoWhy{letGoNoTodo, letGoAnsweredElsewhere} {
		if len(waits[why]) != 0 {
			tellings = append(tellings, waitTelling{waits: waits[why], why: fmt.Sprintf(openings[why], strings.Join(repos[why], ", "))})
		}
	}
	return w.tellWaitingArchitects(ctx, tellings, func(child string, wait releaseWait, parentRecorded bool) string {
		status := listed[child]
		if status == "" {
			status = "no longer listed"
		}
		if !parentRecorded {
			return fmt.Sprintf("%s (%s in Dispatch now, and %s)", child, status, releaseParentFirst(child, wait))
		}
		return fmt.Sprintf("%s (%s in Dispatch now: release it again once it is todo)", child, status)
	})
}

// releaseParentFirst is the step for a child whose parent the workflow does not record, which
// both tellings name it with: the daemon records a child only under a recorded parent.
func releaseParentFirst(child string, wait releaseWait) string {
	return fmt.Sprintf("its parent %s is not recorded: release %s first, then %s", wait.parent, wait.parent, child)
}

// waitTelling is the children of waits, told in notices that open with why.
type waitTelling struct {
	why   string
	waits map[string]releaseWait
}

// tellWaitingArchitects is the body both tellings share: for each of tellings, one
// child-release-stalled notice for each issue a child's notice is queued on, naming every child of
// its waits the workflow still does not record, as its why opens the notice and line names each
// child, with whether the workflow records that child's parent, read here once for both callers.
// A child's notice is queued on its parent when the record holds that parent in the child's tree,
// and else on the tree root, so the owner walk takes it to the architect that releases the
// parent's children, or to the tree's architect where no architect of the tree owns the parent
// (tellReleaseWaits). A child recorded since is told nothing; its record's own notices carry it. Every notice is written in one transaction, so a
// failure tells no architect anything and the caller's put-back leaves nothing half told, and
// nothing is written where every child is recorded.
func (w *workflowRuntime) tellWaitingArchitects(ctx context.Context, tellings []waitTelling,
	line func(child string, wait releaseWait, parentRecorded bool) string) error {
	type notice struct{ issue, reason string }
	var notices []notice
	for _, telling := range tellings {
		byIssue := map[string][]string{}
		for _, child := range slices.Sorted(maps.Keys(telling.waits)) {
			wait := telling.waits[child]
			recorded, err := w.recordOf(ctx, child)
			if err != nil {
				return err
			}
			if recorded != nil {
				continue
			}
			parent, err := w.recordOf(ctx, wait.parent)
			if err != nil {
				return err
			}
			on := wait.tree
			if parent != nil && parent.Tree == wait.tree {
				on = parent.Key
			}
			byIssue[on] = append(byIssue[on], line(child, wait, parent != nil))
		}
		for _, issue := range slices.Sorted(maps.Keys(byIssue)) {
			notices = append(notices, notice{issue: issue, reason: telling.why + ": " + strings.Join(byIssue[issue], "; ")})
		}
	}
	if len(notices) == 0 {
		return nil
	}
	now := time.Now().UTC()
	scoped, committed := intake.WithCommitHooks(ctx)
	if err := pgx.BeginFunc(scoped, w.pool, func(tx pgx.Tx) error {
		for _, told := range notices {
			row, err := record.NewOutboxRow(told.issue, record.Notice{Kind: "child-release-stalled", Role: claim.RoleArchitect, Reason: told.reason}, now)
			if err != nil {
				return err
			}
			if err := w.records.Enqueue(scoped, tx, row); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("tell the architects of the children the recovery left unrecorded: %w", err)
	}
	committed()
	return nil
}

// recoveredOn is recoverRepositories' scope over listing: an issue records does not hold whose
// repository repos names (lowercased), one of whose ancestors in listing resolves to one, or one
// silenced names.
func recoveredOn(repos, silenced map[string]bool, listing []dispatch.IssueSummary) func(records map[string]*record.Issue, summary dispatch.IssueSummary) bool {
	byKey := make(map[string]dispatch.IssueSummary, len(listing))
	for _, summary := range listing {
		byKey[summary.Key] = summary
	}
	return func(records map[string]*record.Issue, summary dispatch.IssueSummary) bool {
		if records[summary.Key] != nil {
			return false
		}
		if silenced[summary.Key] || repos[strings.ToLower(summary.ResolvedRepo)] {
			return true
		}
		seen := map[string]bool{summary.Key: true}
		for parent := parentKey(summary.Parent); parent != "" && !seen[parent]; {
			above, listed := byKey[parent]
			if !listed {
				return false
			}
			if silenced[parent] || repos[strings.ToLower(above.ResolvedRepo)] {
				return true
			}
			seen[parent] = true
			parent = parentKey(above.Parent)
		}
		return false
	}
}

// repositoryUnread is whether the daemon is waiting on a read of resolved before it can record an
// issue on it: release_children's answer for a child whose apply recorded nothing
// (api.Options.RepositoryUnread), which that route reads as a recovery that is coming.
//
// One an entry records issues on is never unread: GitHub answered for both its installations and
// its name, and an issue on it is recorded from that answer. Any other is unread only while a
// preparation's reads went unanswered, or the boot's listing left it for the recovery to read,
// which is the marker leaveUnread and leaveForRecovery write and readAnswered drops, and which the
// refresher is reading GitHub for. A refused one is not, because nothing is reading GitHub for it
// and no recovery will record the child. So a child left by a refusal is answered as one the
// recovery will not record, and its architect is told to act rather than to wait.
func (w *workflowRuntime) repositoryUnread(resolved string) bool {
	if _, recorded := w.repositories.lookup(resolved); recorded {
		return false
	}
	w.recoveringMu.Lock()
	defer w.recoveringMu.Unlock()
	_, waiting := w.unread[strings.ToLower(resolved)]
	return waiting
}

// admissionBound is whether issue's event is one admission records an unrecorded issue from: in
// todo, handed to Legion or a child of a tree.
func admissionBound(issue intake.DispatchIssue) bool {
	return issue.Status == "todo" && (issue.HandedOver || issue.Parent != "")
}

// heardAtBoot is what GitHub intake hears when the boot opens it: every repository a record of
// the project names and each configured rename's partner of them (heardRepositories). It keeps
// that set and the repository caches' own (refreshCacheRepositories), so a record written after
// it widens both without reading the records again (hearRecorded).
func (w *workflowRuntime) heardAtBoot(ctx context.Context) ([]ghrepo.Repository, error) {
	var heard []ghrepo.Repository
	err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		var err error
		if heard, err = w.heardRepositories(ctx, tx); err != nil {
			return err
		}
		return w.refreshCacheRepositories(ctx, tx)
	})
	if err != nil {
		return nil, err
	}
	w.heardMu.Lock()
	defer w.heardMu.Unlock()
	w.heard.Store(&heard)
	return heard, nil
}

// hearRecorded has GitHub intake hear the repository an issue has just been recorded on, the
// runtime keep a repository cache of it, and legion state report it, from the moment a record
// first names it, with no restart. The workflow calls it inside the transaction that writes the
// record (workflow.Config.HearRepository), and the consumer's filters change once that
// transaction has committed and never before (intake.OnCommit), so an admission rolled back
// subscribes to nothing. That is what keeps the stream call out of the transaction and out of the
// fact serializer as well (INFRA-3169): a commit hook runs after the commit returned the
// connection to the pool and released the advisory lock the serializer holds for the
// transaction's life (intake.ApplyFact, intake.SerializeFacts), so the call waits on JetStream
// holding neither. A write outside any commit scope — a test applying a record of its own —
// changes nothing here; the boot's own set covers it (heardAtBoot).
//
// A consumer update JetStream refuses leaves the record as it is, marks the heard set pending
// (heardPending) and wakes the refresher's fourth job, which publishes the set whole
// (republishHeard): nothing inside the transaction can fail on it, so no JetStream trouble rolls
// an admission back.
func (w *workflowRuntime) hearRecorded(ctx context.Context, recorded string) {
	if recorded == "" || !intake.CommitScoped(ctx) {
		return
	}
	repo, err := ghrepo.Parse("GitHub's spelling of the repository an issue was recorded on", recorded)
	if err != nil {
		w.log.Warn("workflow: a record names a repository GitHub intake cannot hear", "repo", recorded, "error", err)
		return
	}
	intake.OnCommit(ctx, func() {
		w.repositories.addLive(repo)
		w.cacheRepository(repo)
		w.hearOrPend(ctx, repo)
	})
}

// hearOrPend has GitHub intake hear repo (hearRepository), and where JetStream refuses the update,
// keeps repo for the next publication of the whole set (heardMissed), marks the set pending
// (heardPending) and wakes the refresher's fourth job, which publishes it (republishHeard).
func (w *workflowRuntime) hearOrPend(ctx context.Context, repo ghrepo.Repository) {
	if err := w.hearRepository(ctx, repo); err != nil {
		w.heardMu.Lock()
		w.heardMissed = append(w.heardMissed, repo)
		w.heardMu.Unlock()
		w.heardPending.Store(true)
		wake(w.heardWake)
		w.log.Warn("workflow: GitHub intake was not told of a repository the daemon works, so the refresher publishes the whole heard set again",
			"repo", repo.String(), "error", err)
	}
}

// hearRepository adds repo to what GitHub intake hears, through the one route the runtime
// reaches that call by (updateSubscription). A repository the held set already names (heard)
// costs one atomic load of the set and a comparison of owner and name, without case, against each
// repository in it: no lock, no allocation and no call. So the ordinary recording, on a
// repository another record already named, waits on nothing and asks JetStream nothing; a
// recording runs on the Dispatch consumer's goroutine or in release_children's handler, so a lock
// taken first would have intake stand still behind every publication of the set.
//
// Answering from the held set without the lock is sound because that set is what intake was last
// told and it only grows: every write of it holds heardMu — the boot's (heardAtBoot), the whole
// set published again (republishHeard, which adds the records' repositories to the set it holds
// and drops none) and an addition here — and stores the set only once intake has taken it. Adding
// a repository takes heardMu, so an addition and a publication never overwrite one another's set:
// the held set is read again under the lock, since a publication that ran while this call waited
// may hold the repository by then, and a publication whose read of the records missed a record
// committed meanwhile is followed by that record's own call here, which finds its repository
// missing and adds it.
func (w *workflowRuntime) hearRepository(ctx context.Context, repo ghrepo.Repository) error {
	if heardNames(w.heard.Load(), repo) {
		return nil
	}
	w.heardMu.Lock()
	defer w.heardMu.Unlock()
	held := w.heard.Load()
	if heardNames(held, repo) {
		return nil
	}
	var current []ghrepo.Repository
	if held != nil {
		current = *held
	}
	heard := append(slices.Clip(current), repo)
	slices.SortFunc(heard, func(a, b ghrepo.Repository) int {
		return strings.Compare(strings.ToLower(a.String()), strings.ToLower(b.String()))
	})
	update, cancel := context.WithTimeout(context.WithoutCancel(ctx), appMintAttempt)
	defer cancel()
	if err := w.updateSubscription(update, heard); err != nil {
		return err
	}
	w.heard.Store(&heard)
	w.log.Info("legion workflow hears a repository it works", "repo", repo.String(), "heard", repositoryNames(heard))
	return nil
}

// updateSubscription has GitHub intake hear repositories: the one route the runtime reaches
// intake's SetRepositories by — the whole set published again (republishHeard) and a repository
// added (hearRepository) both publish through it — so no caller escapes the stand-in a test puts in
// its place (setRepositories, which the boot leaves nil because the daemon's own answer is the
// consumers it opened). It is called under no transaction of the pool: the call reaches JetStream
// and, for a durable consumer that is missing, the pool itself (INFRA-3169).
func (w *workflowRuntime) updateSubscription(ctx context.Context, repositories []ghrepo.Repository) error {
	if w.setRepositories != nil {
		return w.setRepositories(ctx, repositories)
	}
	return w.consumers.SetRepositories(ctx, repositories)
}

// heardNames is whether held, a set the daemon publishes to its readers — the one GitHub intake
// was given (heard), or the cache set (cacheRepos) — names repo, compared without case as GitHub
// compares names. A nil set, before the boot has published one, names nothing.
func heardNames(held *[]ghrepo.Repository, repo ghrepo.Repository) bool {
	if held == nil {
		return false
	}
	return slices.ContainsFunc(*held, func(name ghrepo.Repository) bool {
		return strings.EqualFold(name.Owner(), repo.Owner()) && strings.EqualFold(name.Name(), repo.Name())
	})
}

// cacheRepositories is the repositories the runtime keeps a repository cache of, read at every
// orphan sweep and handed to it as a function that performs no I/O
// (sandbox.Options.CacheRepositories): the repositories of the project's live issue records, which
// is where the work is, and each repository a launch worked since the last read of them
// (workLaunched). A repository gets its cache — one volume and one pod — from the moment a record
// names it, and loses it after its retention once no live record names it. Before the first read
// of the records it is none.
func (w *workflowRuntime) cacheRepositories() []ghrepo.Repository {
	if cached := w.cacheRepos.Load(); cached != nil {
		return *cached
	}
	return nil
}

// refreshCacheRepositories reads the cache set again: the repository of every live issue record of
// the project (record.Store.LiveIssues), as tx reads them. It runs at the boot (heardAtBoot) and at
// every read of the live records (refreshLive), so a repository whose last issue is done leaves
// the set at the next read and its cache retires after its retention; a repository a record first
// names joins it at the recording itself (hearRecorded), and one a launch works at the launch
// (workLaunched), until the next read.
//
// The same read gives the repository state the repositories the live records name
// (repositories.setLive), which the refresher reads again while their answers are bad (badLive).
// legion state reads the live records itself, in the state's own snapshot (source.State), and
// names their repositories by the same rule (repositoriesOf), so a daemon that boots over
// a live tree reports its repository from the records alone, before any event of it.
func (w *workflowRuntime) refreshCacheRepositories(ctx context.Context, tx pgx.Tx) error {
	issues, err := w.records.LiveIssues(ctx, tx, w.dispatchProject)
	if err != nil {
		return fmt.Errorf("read the repositories the project's live records name for the repository caches: %w", err)
	}
	live, err := repositoriesOf(issues)
	if err != nil {
		return err
	}
	w.cacheReposMu.Lock()
	w.storeCacheRepositories(live)
	w.cacheReposMu.Unlock()
	w.repositories.setLive(live)
	return nil
}

// repositoriesOf is the repository each of issues, any slice of records, names, once each by its
// lowercased <owner>/<name>, as the record keeps GitHub's spelling of it (record.Issue.GitHubRepo),
// the first record's spelling where several name one; a record that keeps none names nothing, and
// one whose spelling is no <owner>/<name> refuses the read, naming the issue. It is the one reading
// of the repositories records name: the live records' for the cache set and the repository state's
// live set (refreshCacheRepositories) and for legion state's repositories block (source.State),
// and every record's for what GitHub intake hears (heardRepositories) and for the refresher's
// preparation at its start (recordedRepositories).
func repositoriesOf(issues []record.Issue) (map[string]ghrepo.Repository, error) {
	live := map[string]ghrepo.Repository{}
	for _, issue := range issues {
		if issue.GitHubRepo == "" {
			continue
		}
		if _, held := live[strings.ToLower(issue.GitHubRepo)]; held {
			continue
		}
		repo, err := ghrepo.Parse("GitHub's spelling of the repository of "+issue.Key, issue.GitHubRepo)
		if err != nil {
			return nil, err
		}
		live[strings.ToLower(repo.String())] = repo
	}
	return live, nil
}

// cacheRepository adds repo to the cache set, for a record that first names it or a launch that
// works it: the sweep makes its cache at its next pass, with no restart. A repository the set
// already names costs one atomic load of the set and a comparison against each repository in it
// (heardNames), with no lock and no allocation, since every answer for an issue the workflow does
// not record adds its repository (unrecordedRepository), a tmux pane's credential refresh at each
// tick among them. Two records committed at once run their commit hooks on two goroutines, so the
// read and the write of an addition are one step (cacheReposMu) and neither loses the other's
// repository.
func (w *workflowRuntime) cacheRepository(repo ghrepo.Repository) {
	if heardNames(w.cacheRepos.Load(), repo) {
		return
	}
	w.cacheReposMu.Lock()
	defer w.cacheReposMu.Unlock()
	cached := map[string]ghrepo.Repository{}
	for _, held := range w.cacheRepositories() {
		cached[strings.ToLower(held.String())] = held
	}
	if _, held := cached[strings.ToLower(repo.String())]; held {
		return
	}
	cached[strings.ToLower(repo.String())] = repo
	w.storeCacheRepositories(cached)
}

// storeCacheRepositories publishes cached as the cache set, sorted by name, for the sweep to read
// without a lock of its own. Its caller holds cacheReposMu.
func (w *workflowRuntime) storeCacheRepositories(cached map[string]ghrepo.Repository) {
	repos := sortedRepositories(cached)
	w.cacheRepos.Store(&repos)
}

// sortedRepositories is the repositories of named, a map by each one's lowercased <owner>/<name>,
// sorted by that key.
func sortedRepositories(named map[string]ghrepo.Repository) []ghrepo.Repository {
	repos := make([]ghrepo.Repository, 0, len(named))
	for _, key := range slices.Sorted(maps.Keys(named)) {
		repos = append(repos, named[key])
	}
	return repos
}

// heardRepositories is what GitHub intake hears: every repository a record of the project names,
// as tx reads the records, and every configured rename's partner of those
// (`repository_renames`), once each, sorted. A record is heard whatever its tree's state, since a
// root that lingers or closed comes back, by re-admission, a hand-over or an orphan's carry, on
// the repository it keeps, with no refresh between; a fact from a repository an issue is recorded
// on reaches only that issue (workflow's issueForBranch and ownPullRequest). A record that keeps
// no GitHub spelling of its repository, which its backfill has not filled yet (backfillLater), is
// passed by, so it stops nothing. A rename's partner is heard in both directions, so the
// consumer's FilterSubjects carry both subjects over a transfer: the events arrive under the name
// GitHub now has while the records keep the name they were written under, and neither side may
// be deaf. Each is spelled as GitHub spells it, which Envoy's subjects carry and NATS matches
// case-sensitively, whatever spelling a record keeps for its workspace (a backfilled record keeps
// its clone's): a record's repository as it keeps GitHub's spelling (record.Issue.GitHubRepo), and
// a partner, for a name no record names, as the pair configures it, so a restart subscribes with
// no read of GitHub.
func (w *workflowRuntime) heardRepositories(ctx context.Context, tx pgx.Tx) ([]ghrepo.Repository, error) {
	issues, err := w.records.Issues(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("read the repositories the project's records name: %w", err)
	}
	heard, err := repositoriesOf(issues)
	if err != nil {
		return nil, err
	}
	// One pass over the configured pairs, not over everything heard: a deployment configures no
	// pair at all nearly always, and this runs at every boot and consumer re-open. A partner is
	// added only when nothing heard already names it, so the spelling GitHub itself gave — a
	// record's — always wins over the pair's, which NATS would otherwise match against no
	// published subject at all; the pair's spelling is used only for a name nothing else names.
	// Both sides are read before either is added, so neither addition is seen by the other's test.
	for _, pair := range w.repositories.renames.Pairs() {
		from, to := strings.ToLower(pair.From.String()), strings.ToLower(pair.To.String())
		_, heardFrom := heard[from]
		_, heardTo := heard[to]
		if heardFrom && !heardTo {
			heard[to] = pair.To
		}
		if heardTo && !heardFrom {
			heard[from] = pair.From
		}
	}
	return sortedRepositories(heard), nil
}

// issueRepository is the repository issue key works and its default branch: the record's
// repository and recorded default branch for an issue the workflow records, and for one it does
// not — an operator's claim — the repository Dispatch resolves the issue to, read and prepared
// once for as long as the issue holds its workspace (unrecordedRepository). It is what a working
// copy's adoption mints its commit identity for (adoptIdentity) and what a tmux pane's credential
// refresh reads (claimRepository); a launch reads launchRepository, which prepares a recorded
// issue's repository the cache set does not hold as well. The identity is not minted for this
// repository's own owner: identity maps it through serving first, so a tree recorded before a
// transfer is authored by the App on the installation that holds the repository now.
func (w *workflowRuntime) issueRepository(ctx context.Context, key string) (ghrepo.Repository, string, error) {
	return w.repositoryOf(ctx, key, false)
}

// launchRepository is issueRepository as every launch of an issue's claims reads it
// (specs.repository): a pod provisions its workspace from the repository and its default branch,
// so a launch on none would be refused. A recorded issue whose repository the cache set does not
// hold — a production check's look on a done issue, whose repository no live record names any
// longer — has that repository prepared first, as the refresher prepares each record's at its
// start (prepareLive), and the repository joins the cache set and the heard set (workLaunched); a
// read there that fails or refuses is kept in its entry and logged, and refuses nothing, since the
// record decides the repository. The next read of the live records replaces the cache set, so a
// repository no live record names is stamped at the next sweep and retires after its retention,
// unless a later launch names it again.
func (w *workflowRuntime) launchRepository(ctx context.Context, key string) (ghrepo.Repository, string, error) {
	return w.repositoryOf(ctx, key, true)
}

// repositoryOf is issueRepository, and launchRepository with launch set.
func (w *workflowRuntime) repositoryOf(ctx context.Context, key string, launch bool) (ghrepo.Repository, string, error) {
	issue, err := w.recordOf(ctx, key)
	if err != nil {
		return ghrepo.Repository{}, "", err
	}
	if issue == nil {
		return w.unrecordedRepository(ctx, key)
	}
	repo, err := issue.Repository()
	if err != nil {
		return ghrepo.Repository{}, "", err
	}
	if launch && !heardNames(w.cacheRepos.Load(), repo) {
		if _, err := w.prepareLive(ctx, repo, w.onceRecordedBranches(ctx)); err != nil && ctx.Err() == nil {
			w.log.Warn("workflow: a launch's preparation of its record's repository did not answer, so the launch works the record's repository on what the record holds",
				"issue", key, "repo", repo.String(), "reason", err)
		}
		w.workLaunched(ctx, repo)
	}
	return repo, issue.DefaultBranch, nil
}

// unrecordedRepository is the repository an issue the workflow does not record works, and its
// default branch as GitHub names it: the repository Dispatch resolves the issue to
// (dispatch.Issue.ResolvedRepo: the issue's own, else its nearest ancestor's, else the project's
// default), read from Dispatch at the first launch of a claim on the issue while it holds no
// workspace, and prepared as the refresher prepares a record's repository (prepareLive): both role
// Apps' installations under the name the rename rule serves it under, its name and default branch,
// and Dispatch's webhook App. The answer is kept by issue key (keptRepository) for as long as the
// issue holds its workspace, so every claim of the issue works one repository — its later
// launches, its working copy's adoption and a tmux pane's credential refresh, which runs at every
// refresh tick — and a relaunch never restores a workspace saved from one repository into
// another's. The repository joins the cache set and the heard set (workLaunched) at every answer,
// the kept one's too: a read of the live records replaces the cache set (refreshLive), and no
// record of the issue names the repository there, so without it a later launch of the issue would
// leave its repository's cache to be stamped and retired while the issue's pod works in it.
//
// Each refusal fails that one launch, naming its cause, and the daemon stays up: Dispatch not
// answering, an issue that resolves to no repository, a role App that is not installed on the
// repository, and a read of GitHub that did not answer.
func (w *workflowRuntime) unrecordedRepository(ctx context.Context, key string) (ghrepo.Repository, string, error) {
	if kept, held := w.keptRepository(key); held {
		w.workLaunched(ctx, kept.Name)
		return kept.Name, kept.DefaultBranch, nil
	}
	issue, err := w.dispatch.GetIssue(ctx, key)
	if err != nil {
		return ghrepo.Repository{}, "", fmt.Errorf("read the repository of %s from Dispatch: %w", key, err)
	}
	if issue.ResolvedRepo == "" {
		return ghrepo.Repository{}, "", fmt.Errorf("%s names no repository: set the repository of %s, or the default repository of project %s, in Dispatch", key, key, cmp.Or(issue.Project, w.dispatchProject))
	}
	repo, err := ghrepo.Parse("the repository Dispatch resolves "+key+" to", issue.ResolvedRepo)
	if err != nil {
		return ghrepo.Repository{}, "", err
	}
	read, err := w.prepareLive(ctx, repo, w.onceRecordedBranches(ctx))
	if err != nil {
		return ghrepo.Repository{}, "", fmt.Errorf("prepare %s, the repository Dispatch resolves %s to: %w", repo, key, err)
	}
	w.workLaunched(ctx, read.Name)
	w.launchedMu.Lock()
	defer w.launchedMu.Unlock()
	if w.launched == nil {
		w.launched = map[string]ghbranch.Repository{}
	}
	w.launched[strings.ToLower(key)] = read
	return read.Name, read.DefaultBranch, nil
}

// keptRepository is the repository kept for key, an issue the workflow does not record, at its
// first launch (unrecordedRepository), while the issue still holds its workspace (workspaceHeld:
// under Kubernetes its Sandbox, under tmux its workspace directory); one whose workspace is gone
// is dropped, so the next launch reads Dispatch again. A restart keeps nothing, and the end of the
// issue's pod drops it too (dropLaunchedRepository).
func (w *workflowRuntime) keptRepository(key string) (ghbranch.Repository, bool) {
	w.launchedMu.Lock()
	defer w.launchedMu.Unlock()
	kept, held := w.launched[strings.ToLower(key)]
	if !held {
		return ghbranch.Repository{}, false
	}
	if w.workspaceHeld != nil && w.workspaceHeld(key, kept.Name) {
		return kept, true
	}
	delete(w.launched, strings.ToLower(key))
	return ghbranch.Repository{}, false
}

// dropLaunchedRepository drops the repository kept for issue (keptRepository): what the end of
// the issue's pod calls (podEndsOn). Each of the three hooks of that end (outbox.podEnded) runs
// for an issue the workflow records, which works its record's repository (repositoryOf), so the
// drop clears only a repository kept before the issue was recorded.
func (w *workflowRuntime) dropLaunchedRepository(issue string) {
	w.launchedMu.Lock()
	defer w.launchedMu.Unlock()
	delete(w.launched, strings.ToLower(issue))
}

// workLaunched has the runtime keep a repository cache of repo and GitHub intake hear it, for a
// launch that works it (launchRepository, unrecordedRepository), until the next read of the live
// records replaces the cache set (refreshLive).
func (w *workflowRuntime) workLaunched(ctx context.Context, repo ghrepo.Repository) {
	w.cacheRepository(repo)
	w.hearOrPend(ctx, repo)
}

// recordOf is key's issue record, nil when the workflow does not record it.
func (w *workflowRuntime) recordOf(ctx context.Context, key string) (*record.Issue, error) {
	var issue *record.Issue
	if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		var err error
		issue, err = w.records.Issue(ctx, tx, key)
		return err
	}); err != nil {
		return nil, fmt.Errorf("read the record of %s: %w", key, err)
	}
	return issue, nil
}

// prepareRepository readies a Dispatch issue event before it is applied (intake's
// ConsumerSpec.PrepareRepository), and is the event to apply. An issue's repository is its
// record's once recorded, so a recorded issue's event asks nothing of GitHub, nor of Dispatch about
// its repository: a retained event that names no repository (ResolvedRepoMissing) takes the
// record's, and one naming another is applied as it is, for the workflow to keep the record's and
// say so. For an issue not yet recorded, a retained event's repository is read from Dispatch's
// issue detail, a read that fails leaving the event to be retried; then, for an event in todo that
// admission would record (handed to Legion, or a child of a tree), what the workflow reads in
// memory when it records the issue (ResolveRepository) is readied: a repository an entry records
// issues on has its default branch read again once the read Legion holds is older than its TTL,
// and any other is read from GitHub now (prepareRead), so a repository just made, or an App just
// installed, is taken at this preparation. An unrecorded issue's event in any other status asks
// GitHub nothing.
//
// An unrecorded issue has no sequence to fence a stale event with, and its admission event may come
// after the issue's newer events: redelivered after its preparation or apply failed or its
// acknowledgement window ran out, or put off by Dispatch's outbox, which publishes the events behind
// a failed publish first. So an admission event of an unrecorded issue is prepared as Dispatch's
// issue detail now shows the issue, at the sequence Dispatch has run to (intake.ObservedIssue),
// not as the event says: a person who withdrew the issue meanwhile is not overruled, and an issue
// whose repository changed with no event of its own (an ancestor's, or the project's default) is
// admitted on the one it resolves to now. That view applies only while the issue stays unrecorded
// (intake.DispatchIssue.Observed): once release_children records it first, the event applies as
// it was delivered, and once the issue is recorded, a stale event below its sequence is fenced as
// any is.
//
// Every labelled todo of an issue unrecorded or recorded as a root, under a Dispatch parent recorded
// or not, also reads that parent's chain from Dispatch (withAncestry), which keeps the issue out
// of admission's line while any live root is on it. An event of an issue the workflow does not
// record, or a fresh event of a recorded issue whose Dispatch parent is not its record's, reads
// again the chain of every queued root whose recorded chain holds the issue: that issue may be the
// ancestor a person just moved.
//
// An unrecorded root in triage that carries the label also has its Dispatch event log read, from
// its start, for who set the label (labelSetter, intake.DispatchIssue.LabelSetter): a person's
// label queues the root through the outbox, a session's wakes the controller as today, and the
// event that reaches the daemon says the label is there, not who put it there. It is one paged
// read per such event, outside the fact lock as the rest of the preparation reads. A failed read
// fails the preparation, so the event is delivered again; a log that does not decode fails it at
// every delivery, so the intake gives the event up at its bound. A recorded root a person moved
// back to triage is never queued again, so its log is not read.
func (w *workflowRuntime) prepareRepository(ctx context.Context, issue intake.DispatchIssue) (intake.DispatchIssue, error) {
	return w.prepareIssue(ctx, issue, true)
}

// prepareObserved readies release_children's todo child, already observed as Dispatch's issue
// detail shows it now (intake.ObservedIssue), as prepareRepository readies an event's, but reads no
// other root's chain again: the view is no re-parent.
func (w *workflowRuntime) prepareObserved(ctx context.Context, issue intake.DispatchIssue) (intake.DispatchIssue, error) {
	return w.prepareIssue(ctx, issue, false)
}

// prepareIssue is prepareRepository, preparing an admission event from Dispatch's current view, and
// reading other roots' chains again, only when observe is set: release_children's child is that
// view already.
func (w *workflowRuntime) prepareIssue(ctx context.Context, issue intake.DispatchIssue, observe bool) (intake.DispatchIssue, error) {
	recorded, err := w.recordOf(ctx, issue.Key)
	if err != nil {
		return issue, err
	}
	if recorded != nil {
		return w.withAncestry(ctx, w.prepareRecorded(issue, recorded), recorded, observe)
	}
	if !admissionBound(issue) {
		if issue.LabelledTriageRoot() {
			if issue.LabelSetter, err = w.labelSetter(ctx, issue.Key); err != nil {
				return issue, err
			}
		}
		prepared, err := w.prepareUnrecorded(ctx, issue, false)
		if err != nil {
			return prepared, err
		}
		return w.withAncestry(ctx, prepared, nil, observe)
	}
	delivered := issue.Seq
	if observe {
		now, err := w.dispatch.GetIssue(ctx, issue.Key)
		if err != nil {
			return issue, fmt.Errorf("read %s from Dispatch before applying its admission event %d: %w", issue.Key, delivered, err)
		}
		event := issue
		issue = intake.ObservedIssue(now)
		issue.Seq, issue.Delivered = max(delivered, issue.Seq), &event
	}
	prepared, err := w.prepareUnrecorded(ctx, issue, false)
	if err != nil {
		return prepared, err
	}
	return w.withAncestry(ctx, prepared, nil, observe)
}

// labelSetter is the kind of the Dispatch actor that added the label to key, by
// intake.LabelSetter's rule. It reads the issue's whole Dispatch event log from sequence 0, in one
// paged read, outside the fact lock. A read that fails is an error that wraps the client's error,
// which can be a *dispatch.EventLogError, for a page of the log the client cannot read, or a
// Dispatch refusal (*dispatch.Error). A log that does not decode is an error that wraps a
// *dispatch.EventLogError too. Each error names the key. No error comes with a setter, since a
// caller must not apply a setter nobody read: a setter of "" would keep today's triage wake for a
// label a person set.
func (w *workflowRuntime) labelSetter(ctx context.Context, key string) (string, error) {
	events, err := w.dispatch.EventsAfter(ctx, key, 0)
	if err != nil {
		return "", fmt.Errorf("read the event log of %s from Dispatch for who set its label: %w", key, err)
	}
	setter, err := intake.LabelSetter(events)
	if err != nil {
		return "", fmt.Errorf("read who set the label of %s from its event log: %w", key, err)
	}
	return setter, nil
}

// prepareArtifact readies a Dispatch review event before it is applied (intake's
// ConsumerSpec.PrepareArtifact), and is the event to apply. The workflow performs no I/O, and
// decides from Dispatch's view of the issue the review is on whether a person's approval of a
// parked labelled root's primary document queues that root (intake.DispatchArtifact.Issue), so for
// an approval a person made that is not Dispatch's carry of an earlier one (CarriedApproval) the
// issue is read from Dispatch's detail, one read per such event, a read that fails leaving the
// event to be retried. Every other review or version event — a session's or Dispatch's own review,
// a changes request, a carried approval, a version — asks Dispatch nothing and is applied as
// decoded, with no view, which queues nothing.
//
// The view is the issue as it is now, so it cannot say whether the status changed after the
// approval. For an approval that needs the issue's whole Dispatch event log
// (intake.DispatchArtifact.NeedsEventLog: one that can queue its root by that view and carries a
// sequence to place it by), a second read takes that log, and the first change of the issue's
// status after the approval is set on the fact (StatusChangedSeq, by
// intake.DispatchArtifact.WithStatusChange), so the workflow refuses an approval after which the
// person parked the root again, however late it is applied or replayed. That read is one paged
// read of the issue's whole log, the same read labelSetter makes for an unrecorded issue's
// admission event, made under the same AckWait bound the intake puts on the whole preparation, the
// issue read's included. A failed read, or a log the daemon cannot decode, fails the preparation
// with an error naming the key, so the event is delivered again, as a failed issue read is.
func (w *workflowRuntime) prepareArtifact(ctx context.Context, fact intake.DispatchArtifact) (intake.DispatchArtifact, error) {
	if !fact.PersonApproval() {
		return fact, nil
	}
	issue, err := w.dispatch.GetIssue(ctx, fact.Key)
	if err != nil {
		return fact, fmt.Errorf("read %s from Dispatch before applying a person's approval of its document %s: %w", fact.Key, fact.ArtifactID, err)
	}
	fact.Issue = &issue
	if !fact.NeedsEventLog() {
		return fact, nil
	}
	events, err := w.dispatch.EventsAfter(ctx, fact.Key, 0)
	if err != nil {
		return fact, fmt.Errorf("read the event log of %s from Dispatch for a change of its status after the approval at %d: %w", fact.Key, fact.Mark.Seq, err)
	}
	if fact, err = fact.WithStatusChange(events); err != nil {
		return fact, fmt.Errorf("read a change of the status of %s after the approval at %d from its event log: %w", fact.Key, fact.Mark.Seq, err)
	}
	return fact, nil
}

// prepareListed readies a todo issue of a listing — the boot's (reconcile, boot set) or a
// recovery's (recoverRepositories) — as prepareRepository readies an event's, from the listing the
// daemon has just read: an issue records holds a record of takes that record's repository, and an
// unrecorded one's repository is readied as an event's is (prepareUnrecorded), with no read of
// Dispatch's view. Its Dispatch chain
// is read up walk, the one cache over the listing (listingAncestry), and no other root's is read
// again: the listing holds every queued root's own todo.
//
// The boot's listing reads GitHub for no repository. It runs inside the boot's
// bounded reconcile (readinessAttempt(workflow.reconcile, bootTimeout, …), 30 s), and a
// per-repository installation read can take up to installReadAttempt, so the reads of tens of
// todo issues on such repositories, or two slow ones, used that bound up: every read after it
// failed at once and left its repository unread, and the boot attempt itself then failed on
// DispatchTarget. Such a repository is left unread instead (leaveForRecovery), and the
// refresher's recovery, which runs as soon as the boot is over, reads it outside any boot bound
// and takes the issue from its own listing (recoverRepositories): the issue waits, and is not
// dropped.
func (w *workflowRuntime) prepareListed(ctx context.Context, records map[string]*record.Issue, walk ancestry, issue intake.DispatchIssue, boot bool) (intake.DispatchIssue, error) {
	recorded := records[issue.Key]
	if recorded != nil {
		issue = w.prepareRecorded(issue, recorded)
	} else {
		prepared, err := w.prepareUnrecorded(ctx, issue, boot)
		if err != nil {
			return prepared, err
		}
		issue = prepared
	}
	if !queuesUnderParent(issue, recorded) {
		return issue, nil
	}
	chain, err := w.chainOf(ctx, walk, issue.Key, issue.Parent)
	issue.Ancestry = chain
	return issue, err
}

// prepareRecorded readies an event of an issue recorded as recorded, an event's or the boot
// listing's: one that names no repository (ResolvedRepoMissing) takes the record's.
func (w *workflowRuntime) prepareRecorded(issue intake.DispatchIssue, recorded *record.Issue) intake.DispatchIssue {
	if issue.ResolvedRepoMissing {
		issue.ResolvedRepo, issue.ResolvedRepoMissing = recorded.Repo, false
	}
	return issue
}

// listingAncestry is what reconcile's boot prepares its listing from: every record, by key, read
// once in a read-only transaction, and the walk every chain is read up, holding the Dispatch parent
// of every listed issue as the listing shows it, so only an ancestor outside the listing is read
// from Dispatch.
func (w *workflowRuntime) listingAncestry(ctx context.Context, listed []dispatch.IssueSummary) (map[string]*record.Issue, ancestry, error) {
	var issues []record.Issue
	if err := pgx.BeginTxFunc(ctx, w.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var err error
		issues, err = w.records.Issues(ctx, tx)
		return err
	}); err != nil {
		return nil, nil, fmt.Errorf("read the records for the boot listing: %w", err)
	}
	records := make(map[string]*record.Issue, len(issues))
	for i := range issues {
		records[issues[i].Key] = &issues[i]
	}
	walk := make(ancestry, len(listed))
	for _, summary := range listed {
		walk[summary.Key] = parentKey(summary.Parent)
	}
	return records, walk, nil
}

// withAncestry is an event's issue, recorded as recorded (nil when the workflow does not record
// it), with what its preparation reads up Dispatch's parent chains, outside the fact lock as the
// rest of the preparation reads. Its Ancestry (intake.DispatchIssue.Ancestry) is read for a
// labelled todo of an issue unrecorded or recorded as a root under a parent, recorded or not
// (queuesUnderParent); any other issue's is empty: an unlabelled todo never queues, and a child
// already runs in its tree.
//
// With reread set, an event of an issue the workflow does not record, or a fresh event of a
// recorded issue whose Dispatch parent differs from its record's (a person moved it), reads again
// the Ancestry of every queued root whose recorded Ancestry holds the issue's key
// (record.Store.QueuedRootsUnder), and carries each that differs from its record's as Reread: the
// issue may be the ancestor a person moved, whose roots below it have no event of their own. A
// re-parent of any issue on a chain is an event of a key already on that chain, so no other root
// can be moved by it. A recorded issue's event at or below its record's sequence is stale and
// reads no root again, so a late event never brings back an ancestry hold its newer re-parent
// removed; a fresh one is read as the event shows it, so the moved ancestor is read as moved, and an
// unrecorded issue is read as Dispatch shows it now, as any other ancestor is. A failed read of
// Dispatch while reading one such root again leaves that root's record as it stands, and says
// so; only a failed read of the issue's own chain fails the preparation, and the event is
// delivered again. Every walk the preparation takes reads each Dispatch issue once, reads no
// record, and holds no transaction open across a read of Dispatch.
func (w *workflowRuntime) withAncestry(ctx context.Context, issue intake.DispatchIssue, recorded *record.Issue, reread bool) (intake.DispatchIssue, error) {
	walk := ancestry{}
	if queuesUnderParent(issue, recorded) {
		chain, err := w.chainOf(ctx, walk, issue.Key, issue.Parent)
		if err != nil {
			return issue, err
		}
		issue.Ancestry = chain
	}
	// The record holds what the store kept of a parent and a chain, so Dispatch's are compared as
	// the store would keep them (storetext.Text, storetext.Strings); the walk itself reads Dispatch
	// by the keys as Dispatch gave them.
	if !reread || recorded != nil && (storetext.Text(issue.Parent) == parentKey(recorded.Parent) || issue.Seq != 0 && issue.Seq <= recorded.LastDispatchSeq) {
		return issue, nil
	}
	var roots []record.Issue
	if err := pgx.BeginTxFunc(ctx, w.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var err error
		roots, err = w.records.QueuedRootsUnder(ctx, tx, issue.Key)
		return err
	}); err != nil {
		return issue, fmt.Errorf("read the queued roots whose chain holds %s: %w", issue.Key, err)
	}
	if len(roots) != 0 && recorded != nil {
		walk[issue.Key] = issue.Parent
	}
	for i := range roots {
		root := &roots[i]
		chain, err := w.chainOf(ctx, walk, root.Key, parentKey(root.Parent))
		if err != nil {
			w.log.Warn("workflow: a queued root's Dispatch chain was not read again, so its record keeps the one it has",
				"root", root.Key, "event", issue.Key, "ancestry", root.Ancestry, "error", err)
			continue
		}
		if slices.Equal(storetext.Strings(chain), root.Ancestry) {
			continue
		}
		if issue.Reread == nil {
			issue.Reread = map[string][]string{}
		}
		issue.Reread[root.Key] = chain
	}
	return issue, nil
}

// queuesUnderParent says whether issue, recorded as recorded (nil when the workflow does not record
// it), is a root the waiting line may hold under an unreleased child of an ancestor's tree (the
// ancestry hold): a labelled todo under a Dispatch parent, unrecorded or recorded as a root.
func queuesUnderParent(issue intake.DispatchIssue, recorded *record.Issue) bool {
	return issue.Status == "todo" && issue.HandedOver && issue.Parent != "" &&
		(recorded == nil || claim.IsTreeRoot(recorded.Key, recorded.Tree))
}

// ancestry is what one preparation's walks up Dispatch's parent chains read, each issue once: the
// Dispatch parent ("" for none) of every issue read from Dispatch or shown to the walk.
type ancestry map[string]string

// chainOf is the keys of key's Dispatch parent chain, read up walk from parent upward to the top,
// stopping only at a repeated key or back at key; nil when parent is "". It reads no record: it is
// the chain release_children's own walk reads (api's releasableChild), so an ancestry hold lasts
// exactly while a live root on it could release the issue into its tree (record.Waiting).
func (w *workflowRuntime) chainOf(ctx context.Context, walk ancestry, key, parent string) ([]string, error) {
	var chain []string
	for ancestor := parent; ancestor != "" && ancestor != key && !slices.Contains(chain, ancestor); {
		chain = append(chain, ancestor)
		next, ok := walk[ancestor]
		if !ok {
			detail, err := w.dispatch.GetIssue(ctx, ancestor)
			if err != nil {
				return nil, fmt.Errorf("read %s from Dispatch, an ancestor of %s: %w", ancestor, key, err)
			}
			next = parentKey(detail.Parent)
			walk[ancestor] = next
		}
		ancestor = next
	}
	return chain, nil
}

// parentKey is the key a Dispatch parent names, "" for none.
func parentKey(parent *string) string {
	if parent == nil {
		return ""
	}
	return *parent
}

// prepareUnrecorded readies the event of an issue the workflow does not record, as
// prepareRepository says. Only an issue admission would record reads anything (admissionBound):
// admission records an unrecorded issue only from a todo handed to Legion or released under a
// parent (admission's putNewRoot, the engine's recordChild), so a person's own todo issue, whose
// apply never resolves its repository, reads GitHub for nothing and leaves nothing unread, since
// nothing would ever recover it.
//
// A repository an entry records issues on (lookup) is read from GitHub no further than its
// default branch, which defaultBranches.Named reads again once the read Legion holds is older
// than its TTL, bounded by installReadAttempt, so a branch renamed on GitHub is taken by the
// issues recorded after the rename; the read is kept as the entry's (recordRepository), and a read
// that fails, GitHub's rate limit and a read GitHub does not answer in time among them, leaves the
// entry's own read to record the issue on, since one failed read of a repository GitHub answered
// for is no reason to hold an issue back. Any other repository is read from GitHub here
// (prepareRead); in the boot's listing (boot) no issue reads it, and the repository is left for
// the recovery to read (prepareListed says why).
func (w *workflowRuntime) prepareUnrecorded(ctx context.Context, issue intake.DispatchIssue, boot bool) (intake.DispatchIssue, error) {
	if issue.ResolvedRepoMissing {
		detail, err := w.dispatch.GetIssue(ctx, issue.Key)
		if err != nil {
			return issue, fmt.Errorf("read the repository Dispatch resolves for %s, which its event %d predates: %w", issue.Key, issue.Seq, err)
		}
		issue.ResolvedRepo, issue.ResolvedRepoMissing = detail.ResolvedRepo, false
	}
	if !admissionBound(issue) || issue.ResolvedRepo == "" {
		return issue, nil
	}
	read, recorded := w.repositories.lookup(issue.ResolvedRepo)
	if !recorded {
		return w.prepareRead(ctx, issue, boot)
	}
	if boot {
		return issue, nil
	}
	attempt, cancel := context.WithTimeout(ctx, installReadAttempt)
	refreshed, err := w.defaultBranches.Named(attempt, read.Name)
	cancel()
	if err != nil {
		if _, _, limited := defaultBranchLimit(err); !limited && ctx.Err() == nil {
			w.log.Warn("workflow: the default branch of a repository an issue is recorded on was not read again, so the issue is recorded at the branch last read",
				"issue", issue.Key, "repo", read.Name.String(), "defaultBranch", read.DefaultBranch, "error", err)
		}
		return issue, nil
	}
	w.repositories.recordRepository(issue.ResolvedRepo, refreshed)
	return issue, nil
}

// prepareRead readies the event of an issue the workflow does not record whose repository no
// entry records issues on yet: the reads admission asks for, made here because the workflow's own
// resolution performs no I/O (ResolveRepository). What decides is GitHub, read now, in the one
// preparation step every preparation runs (prepareName) under the name the issue resolves to:
// each role App's installation on it, its own name and default branch (defaultBranches.Named),
// each read bounded by installReadAttempt, and, once both Apps answered installed and GitHub named
// the repository, Dispatch's webhook App on it. Every answer is kept in the entry of the name it
// was read under (keepRead, keepWebhookAnswer).
//
// It is called only for a repository no entry records issues on and only for an issue admission
// would record (prepareUnrecorded), so it reads GitHub no second time for one whose read an entry
// keeps, nor for an issue nothing records. With boot set, for the boot's listing, it reads GitHub
// for nothing and leaves the repository to the recovery (leaveForRecovery; prepareListed says
// why). Every other one is read at each preparation, which is what a refusal that decides no
// later preparation means: an operator who installs the App, or a person who corrects a
// misspelled repository, is taken at the next preparation of the issue, with no wait and no
// restart.
//
// A repository GitHub answers both role Apps are not installed on, whose name a configured pair
// holds, has the pair's other name prepared too, under its own name: a transfer takes the
// repository out of every installation of the owner it names, both Apps' alike, since an App is
// installed on an owner, while the name it moved to is covered by the ones that have it. The issue
// is recorded on the name both Apps answer installed on, and the read is kept under both names
// (recordRepository). A name that holds one of the two Apps and not the other is the operator's
// own gap rather than a transfer, so the other name is read for neither App there.
//
// The three answers. Both Apps installed and GitHub naming the repository: the read is kept and
// the issue is recorded and admitted on it. A role App that is not installed, on the name and on
// the other name of its pair, and a name that is no <owner>/<name> at all: the event carries the
// refusal to its own apply (intake.DispatchIssue.RepositoryRefusal), which says it on the issue
// and decides nothing after it; a repository GitHub does not have is refused the same way, since
// GitHub answers that no App is installed on it. A read GitHub did not answer: the repository is
// left unread (leaveUnread), so the event is applied and acknowledged, its apply says nothing and
// records nothing, and the refresher's recovery takes the issue once a later read answers. The
// one exception is the admission's own rule for a read of the name and default branch GitHub
// refuses for its rate limit, of a repository the daemon holds no read of (defaultBranches.Named
// answers every other from what it holds, and an admission serves no record's branch in its
// place): that is an intake.RetryLater after the wait GitHub asked for (defaultBranchLimit), so
// the event is delivered again once the limit has passed, not every second into it, each delivery
// reading GitHub again. The limit is kept as the name's read error until a read answers.
//
// Dispatch's webhook App check refuses nothing: the issue is recorded whatever Dispatch says, and
// a check that failed is the gap's reason. It is made, and its answer kept, under the name the
// reads were made under, in GitHub's spelling, which is the name the record keeps and legion state
// lists it under, so the repository's gap is named from its first record on — but for a repository
// GitHub renamed: there the record keeps GitHub's new name, whose entry holds no installation
// answer yet, and the refresher's second job prepares it under that name (badLive, prepareLive).
//
// GitHub may answer the read under a name other than the one asked: a repository GitHub itself
// renamed is answered with a 301 to the name it holds it under now, which the read follows,
// while both role Apps answer installed on the old name, since an installation is the owner's.
// The issue is recorded on GitHub's name and at GitHub's default branch, and the read is kept
// under the asked name too (repositories.recordRepository): the apply of this very event, and
// every later preparation of an issue on it, ask under the name the issue resolves to, which is
// the one GitHub renamed away from.
func (w *workflowRuntime) prepareRead(ctx context.Context, issue intake.DispatchIssue, boot bool) (intake.DispatchIssue, error) {
	repo, err := ghrepo.Parse("the repository Dispatch resolves "+issue.Key+" to", issue.ResolvedRepo)
	if err != nil {
		// The refusal is posted on the issue (intake.DispatchIssue.RepositoryRefusal), and the
		// error quotes what Dispatch resolved the issue to, so it is kept as a far side's text is.
		issue.RepositoryRefusal = storetext.FarSideError(err.Error())
		return issue, nil
	}
	if boot {
		w.leaveForRecovery(repo)
		return issue, nil
	}
	answer, err := w.prepareName(ctx, repo, repo, nil)
	if err != nil {
		return issue, err
	}
	refusal := answer.refusal()
	if partner, paired := w.repositories.renames.Partner(repo.String()); paired && len(answer.missing) == len(appauth.Roles) {
		moved, err := w.prepareName(ctx, partner, partner, nil)
		if err != nil {
			return issue, err
		}
		if len(moved.missing) != 0 {
			refusal += "; " + moved.refusal()
		} else {
			refusal = moved.refusal()
		}
		answer = moved
	}
	// answer.under is now the name both role Apps answered installed on, which the issue is
	// recorded on, unless the answer refuses it.
	switch {
	case len(answer.missing) != 0:
		w.readAnswered(repo)
		issue.RepositoryRefusal = refusal
		return issue, nil
	case answer.limited != nil:
		wait := answer.limited.wait
		return issue, &intake.RetryLater{After: wait, Err: fmt.Errorf("prepare %s on %s: GitHub's rate limit, retry in %s: %w", issue.Key, answer.under, wait, answer.limited.err)}
	case refusal != "":
		w.leaveUnread(repo, refusal+"; Legion reads it again")
		return issue, nil
	}
	// The read is kept before the marker is dropped: readAnswered starts the recovery of the
	// issues the repository left, and each of those preparations is to find the read here rather
	// than read GitHub again, which could fail and leave the repository unread once more.
	w.repositories.recordRepository(repo.String(), answer.read)
	if answer.under != repo {
		w.repositories.recordRepository(answer.under.String(), answer.read)
	}
	w.readAnswered(repo)
	return issue, nil
}

// ResolveRepository answers the workflow (workflow.Repositories) from memory: the repository
// resolved names as GitHub names it, at the default branch its entry holds, or why Legion does
// not record an issue on it. It answers from the entries alone (lookup), with the read kept for
// the repository, which is the one GitHub answer made for it however many issues name it and
// which the preparation of the issue's event made, or read again once it was older than its TTL,
// just before the apply (prepareUnrecorded), so a branch renamed on GitHub is taken. A repository
// no entry records issues on is one GitHub has not answered for: the issue is left as unread
// (workflow.Resolution.Unread), and recovered once a read answers (recoverRepositories); a
// refusal GitHub answered is carried on the event itself (intake.DispatchIssue.RepositoryRefusal).
//
// A repository GitHub renamed, which the issue resolves to under the name GitHub renamed it away
// from, is answered here under that old name: the entries hold the read under it as well as under
// GitHub's own name (repositories.recordRepository), and the answer carries GitHub's name and
// default branch, which is what the record keeps. So is a configured pair's `from` whose `to`
// both role Apps answered installed on (prepareRead).
func (w *workflowRuntime) ResolveRepository(resolved string) (workflow.Resolution, error) {
	read, recorded := w.repositories.lookup(resolved)
	if !recorded {
		return workflow.Resolution{Refusal: fmt.Sprintf("GitHub has not been read for %s yet", resolved), Unread: true}, nil
	}
	return workflow.Resolution{Repo: read.Name.String(), DefaultBranch: read.DefaultBranch, GitHubRepo: read.Name.String()}, nil
}

func repositoryNames(repos []ghrepo.Repository) []string {
	names := make([]string, 0, len(repos))
	for _, repo := range repos {
		names = append(names, repo.String())
	}
	return names
}
