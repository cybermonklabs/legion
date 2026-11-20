package daemon

import (
	"context"
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

// repositories is the project's repository mapping as the daemon serves it: Dispatch's mapping of
// the project (Settings → Repositories → Projects), read at boot and on its
// settings.repo_project.updated, each repository served once both GitHub Apps are installed on it
// and every other one left out with the reason, for legion state. The mapping also files people's
// issues, so it can name a repository that lacks one of Legion's Apps; leaving it out costs only its
// issues' admission. unverified is each served repository a later read of the mapping could not
// read the installation of: it stays served on its last answer, and is read again
// (refreshOnMappingChange). withoutWebhookApp is each repository the mapping serves that
// Dispatch's GitHub App is not known to be installed on (checkWebhookApp), and recordedWebhookGaps
// the same of the repositories no mapping names, both for legion state alone (WithoutWebhookApp):
// each is served all the same. Readers take the read lock; the readers of the mapping swap it
// whole under the write lock.
type repositories struct {
	project string
	// renames is the deployment's repository equivalence (`repository_renames`,
	// config.Config.RepositoryRenames): a configured pair's two names are one repository wherever
	// the mapping is read (lookup, kept), so an issue Dispatch resolves to the repository as it
	// was is served by the one it moved to. It never widens the served set itself, which stays
	// Dispatch's mapping: a rename configured before the transfer names a repository GitHub does
	// not have yet, and the mapping's read of GitHub (nameAccepted) fails whole on the first
	// repository it cannot read for any reason but GitHub's rate limit. The zero value, which
	// answers as a comparison without case does, while the deployment configures none.
	renames ghrepo.Renames

	mu       sync.RWMutex
	accepted map[string]mappedRepository
	refused  map[string]refusedRepository
	// recorded is GitHub's kept read of each repository no mapping names that the preparation of
	// an issue on it found both role Apps installed on, by its lowercased <owner>/<name>:
	// GitHub's own spelling of it and its default branch, as that preparation read them
	// (prepareUnmapped). An issue of any project may name any repository both role Apps are
	// installed on (INFRA-2543), so what the mapping decides is the project's default repository
	// and the repositories the daemon prepares before an issue names one, never which
	// repositories an issue may be recorded on. The set is what makes one repository's reads
	// happen once however many issues name it; nothing else of those reads is kept, no
	// installation answer and no refusal, so a repository a role App is not installed on is read
	// again at the next preparation of an issue on it and recorded the moment GitHub answers
	// installed. It says what GitHub answered and never what the project works in, which live
	// holds: the preparation of an issue Legion does not record in the end, a todo child under a
	// parent no live tree holds, keeps a read here as any other does.
	//
	// One read is held under two keys where GitHub answered the name an issue asked under with
	// another: a repository GitHub itself renamed, whose old name it answers with a 301 the
	// read follows (recordRepository). An issue keeps the name it resolves to for the whole of
	// its preparation, so a read held under GitHub's name alone would be found by no lookup of
	// that issue's own name, and the issue would stand unread for ever while GitHub answered
	// every read it was asked.
	//
	// An entry is never dropped, and the reason is the spec's own rule rather than any cost a
	// sweep would carry: the facts that record an issue are kept once the issue is recorded, and
	// a refusal is kept not at all (dispatch://INFRA-2543/spec, "Reading a fact at the moment of
	// acting on it"). This set holds the first of those two, the name and the default branch a
	// preparation read, so the entries it holds are the recordings themselves; no refusal and no
	// installation answer is kept beside them, which is why a repository a role App is not
	// installed on is read again at every preparation.
	//
	// What that costs is one case, and it is written here rather than left to a reader to find:
	// after a role App is uninstalled from a repository this set already holds, an issue on that
	// repository is still admitted — until the process starts again and builds the map from the
	// preparations that follow it — and GitHub then refuses its roles' requests on that
	// repository, though a public repository may still be read. A stale entry decides admission
	// alone and never what a token reaches. Every build of a role's gh bundle reads from GitHub
	// again the owners its App is installed on and fails closed on a read that fails
	// (internal/appauth/credential.go, "The owners the App is installed on are read at every
	// bundle, GitHub asked each time … failing closed is this rule's whole point"); the bundle's
	// directories are built from that answer and from the mapping (Accepted), and the repository
	// caches and the repositories block of legion state from the mapping and the live records
	// (workflowRuntime.cacheRepositories, served), neither from this set. So the cost is that
	// admission, and what the roles meet next depends on what was uninstalled. The record's read
	// is per repository (appauth's GET /repos/{owner}/{repo}/installation) and a role's mint is
	// per owner (Manager.Token), so an App taken off ONE repository of a selected-repositories
	// installation still mints for that owner, the bundle builds, and GitHub refuses the roles'
	// requests on that repository, as the first paragraph says. An App taken off the WHOLE
	// account leaves that owner's cached lease in use until its expiry is no more than
	// appauth's refreshWindow away (Manager.lease), and the default directory cannot be left
	// out, so the bundles built in that time succeed and hold a token GitHub would no longer
	// mint; the first build that mints that owner for real fails, naming that owner and
	// GitHub's answer for it (mintBundle returns the named failure of the default owner rather
	// than leaving it out). runtime.ErrNoGitHubOwner is neither case: it is the App installed
	// nowhere at all, which an issue that has a repository never reaches, since bundleDirs always
	// holds a directory for that repository's own owner. No token reaches a repository because
	// of any of it.
	recorded map[string]ghbranch.Repository
	// live is each repository a live issue record of the project names, by its lowercased
	// <owner>/<name>, as the record keeps GitHub's spelling of it (record.Issue.GitHubRepo): the
	// records' half of what the daemon serves (served), which legion state reports beside the
	// mapping's. It is read from the records where the repository caches' set is, at the boot
	// and at every read of the mapping (workflowRuntime.refreshCacheRepositories), and added to
	// at each recording (cacheRepository). It is not recorded: a restart empties recorded while
	// the records still name the repositories their trees work in, and recorded holds a read
	// for the repository of an issue Legion never records.
	live              map[string]ghrepo.Repository
	unverified        []string
	withoutWebhookApp []webhookAppGap
	// recordedWebhookGaps is why Dispatch's GitHub App is not known to be installed on a
	// repository no mapping names, by its lowercased <owner>/<name>, as Dispatch last answered
	// its webhook App check (checkWebhookApp) for it: at the preparation that read GitHub for it
	// (prepareUnmapped), and of the repositories the live records name at the refresher's start
	// and after every read of the mapping (checkRecordedWebhookApps). A repository Dispatch last
	// answered installed has no entry. legion state lists an entry only while live names its
	// repository and no mapping does (WithoutWebhookApp), which is exactly while served lists it
	// as the records' own, so the answers are never pruned: the preparation of an issue keeps one
	// before the record that names its repository commits, and a read of the records that ran
	// between the two would otherwise drop it. A restart empties the set, and the refresher's
	// first check fills it again from the records, as the boot's read fills live.
	recordedWebhookGaps map[string]string
}

// mappedRepository is a repository Legion serves, as GitHub names it, and whether it is the
// project's default in Dispatch.
type mappedRepository struct {
	Repo    ghrepo.Repository
	Default bool
}

// refusedRepository is a mapped repository Legion leaves out, as Dispatch maps it, and why.
// readErr is the first installation read that failed to answer, of either name checkMapping read
// for it, or the default-branch read GitHub did not answer past its rate limit
// (unreadDefaultBranch): GitHub gave no answer, so the refusal is for now, and
// refreshOnMappingChange reads the mapping again until GitHub answers. A refusal every read
// answered carries none and stands until the mapping changes. mappedDefault is whether Dispatch
// maps it as the project's default. apps is what GitHub answered of each role App's installation
// on Repo, for legion state (view), so a reader learns which App is missing without parsing
// Reason: answered wherever GitHub answered both Apps' reads of the name Dispatch maps, and not
// where a read of it failed or the name is no <owner>/<name> at all, which Reason then names. One
// both Apps answered installed on that is left out for its default-branch read
// (unreadDefaultBranch) carries both as installed, since that is what GitHub answered.
type refusedRepository struct {
	Repo          string
	Reason        string
	apps          appAnswers
	readErr       error
	mappedDefault bool
}

// appAnswers is what GitHub answered of each role App's installation on one name, for a refusal
// (refusedRepository.apps): answered is whether it answered both reads, and implement and review,
// each App's answer, say nothing where it did not. It is a value rather than a pointer to the
// state's view so a refusal stays comparable as a whole.
type appAnswers struct {
	answered, implement, review bool
}

// view is a as legion state reports it (api.RefusedRepositoryView.Apps): nil unless GitHub
// answered both reads.
func (a appAnswers) view() *api.AppInstallationsView {
	if !a.answered {
		return nil
	}
	return &api.AppInstallationsView{Implement: a.implement, Review: a.review}
}

// webhookAppGap is a served repository that Dispatch's GitHub App is not known to be installed on,
// and why (checkWebhookApp): a mapped one under the name its entry is served under, and one only a
// live record names under the name the record keeps (WithoutWebhookApp).
type webhookAppGap struct {
	Repo   string
	Reason string
}

func newRepositories(project string, renames ghrepo.Renames) *repositories {
	return &repositories{project: project, renames: renames, accepted: map[string]mappedRepository{}, refused: map[string]refusedRepository{},
		recorded: map[string]ghbranch.Repository{}, live: map[string]ghrepo.Repository{}, recordedWebhookGaps: map[string]string{}}
}

// set replaces the mapping with accepted and refused, and reports the repositories that became
// served and those that stopped being served, by name.
//
// Two entries of accepted that name one repository are served as one entry, and that entry is the
// project's default when either of the two is. The merge is here because this is the one point
// every list the daemon serves passes through: the boot's accepted list, and refreshMapping's
// accepted plus its kept, which appends mappedRepository{Repo: mapped.Repo, Default:
// failed.mappedDefault} last for a served repository whose installation read failed. Writing each
// entry over the key it shares, as the map write alone did, lets the last entry of the slice win,
// so the kept one clears a default the entry before it carried. A mapping that names a repository
// under both names of a configured rename collides exactly so: the entry whose mapped name GitHub
// no longer holds is served under the name both Apps answered for (checkMapping), which is the
// name the pair's other entry is served under too. What the loss costs is the project's default
// repository: the daemon refuses the launch of an issue that names no repository when the project
// serves no default (issueRepository).
//
// The added and removed answers are read from the merged map rather than from accepted, so two
// entries that become one report their repository added once, and a repository both name is never
// reported removed while it is still served.
func (r *repositories) set(accepted []mappedRepository, refused []refusedRepository) (added, removed []string) {
	next := make(map[string]mappedRepository, len(accepted))
	for _, mapped := range accepted {
		key := strings.ToLower(mapped.Repo.String())
		if held, merged := next[key]; merged && held.Default {
			mapped.Default = true
		}
		next[key] = mapped
	}
	left := make(map[string]refusedRepository, len(refused))
	for _, refusal := range refused {
		left[strings.ToLower(refusal.Repo)] = refusal
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, mapped := range next {
		if _, ok := r.accepted[key]; !ok {
			added = append(added, mapped.Repo.String())
		}
	}
	for key, mapped := range r.accepted {
		if _, ok := next[key]; !ok {
			removed = append(removed, mapped.Repo.String())
		}
	}
	r.accepted, r.refused = next, left
	slices.Sort(added)
	slices.Sort(removed)
	return added, removed
}

// Owners is every mapped repository's owner, lowercased, sorted and once each: what the daemon's
// state reports of the mapping (view, api.RepositoriesView.Owners), what the boot mints each
// App's token on (openWorkflow), and what a role's addressing names while no bundle of that
// role's App has been built yet (plan.appOwners). Each of them is an account both role Apps are
// installed on, since the mapping serves a repository only once they are (checkMapping).
//
// No gh bundle is built from it. A role's bundle holds one directory per owner its own App is
// installed on, read from GitHub at the build (appauth.GitHubCredential): a lease reaches every
// repository its installation holds and names no repository at all (INFRA-2543), so the
// installation is the whole of a role's reach and the mapping bounds none of it. What a bundle
// reads the mapping for is the rename rule alone — whether the project serves each of a
// configured pair's two repositories (Accepted, appauth's bundleDirs).
func (r *repositories) Owners() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	owners := []string{}
	for _, mapped := range r.accepted {
		owners = append(owners, strings.ToLower(mapped.Repo.Owner()))
	}
	slices.Sort(owners)
	return slices.Compact(owners)
}

// serving is the repository GitHub holds a recorded one under now (ghrepo.Renames.Serving): the
// other name of its configured pair exactly when the recorded name is that pair's from, the
// project serves the pair's to, and the project does not serve the pair's from; every other
// recorded name, the pair's to among them, is answered unchanged. It is the one rule every
// GitHub call the daemon makes for a recorded tree reads — the owner its App token is minted for
// and, where the call is a REST write or a GraphQL query, the repository it addresses — since a
// record keeps the name it was written under for its whole life while GitHub, after a transfer,
// holds the repository under the other name, on the other owner's installation.
//
// What the rule asks the mapping is whether it serves each of the pair's two repositories, one
// map lookup apiece (servesRepository), and never whether it serves any repository of their
// owners; ghrepo.Renames.Serving is where why that is the question stands.
//
// A nil mapping — a test that builds an outbox or a workflow without one — serves every recorded
// repository under its own name, as a deployment that configures no rename does.
func (r *repositories) serving(recorded ghrepo.Repository) ghrepo.Repository {
	if r == nil {
		return recorded
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.renames.Serving(recorded, r.servesRepository)
}

// same is whether a and b name one repository: the same name, compared without case as GitHub
// compares names, or the two names of a configured rename (ghrepo.Renames.Same). A nil mapping
// compares without case alone.
func (r *repositories) same(a, b string) bool {
	if r == nil {
		return strings.EqualFold(a, b)
	}
	return r.renames.Same(a, b)
}

// servesRepository is whether the mapping serves repo, the predicate the rename rule reads
// (serving). The served repositories are held by their lowercased <owner>/<name>, which is how
// GitHub compares both halves, so the answer is one map lookup rather than a walk of the
// mapping, however many repositories it holds and however many times a pair asks. Its caller
// holds the read lock.
func (r *repositories) servesRepository(repo ghrepo.Repository) bool {
	_, served := r.accepted[strings.ToLower(repo.String())]
	return served
}

// Accepted is every served repository, as GitHub names it, sorted by name: what GitHub intake
// hears, beside the repositories the project's records name.
func (r *repositories) Accepted() []ghrepo.Repository {
	mapped := r.Mapped()
	repos := make([]ghrepo.Repository, 0, len(mapped))
	for _, m := range mapped {
		repos = append(repos, m.Repo)
	}
	return repos
}

// Mapped is every served repository with whether it is the project's default, sorted by name.
//
// The key each entry is held under is the lowercased <owner>/<name> this sorts by (set), so
// sorting the keys is sorting the entries and no comparator is needed. The comparator this
// replaces lowercased and joined both names again at every comparison, some 2·n·log n strings
// per call, on the per-pod credential and drift paths that read the mapping through Accepted.
func (r *repositories) Mapped() []mappedRepository {
	r.mu.RLock()
	defer r.mu.RUnlock()
	mapped := make([]mappedRepository, 0, len(r.accepted))
	for _, key := range slices.Sorted(maps.Keys(r.accepted)) {
		mapped = append(mapped, r.accepted[key])
	}
	return mapped
}

// Refused is every mapped repository Legion leaves out, with why, sorted by name.
func (r *repositories) Refused() []refusedRepository {
	r.mu.RLock()
	defer r.mu.RUnlock()
	refused := make([]refusedRepository, 0, len(r.refused))
	for _, refusal := range r.refused {
		refused = append(refused, refusal)
	}
	slices.SortFunc(refused, func(a, b refusedRepository) int { return strings.Compare(a.Repo, b.Repo) })
	return refused
}

// lookup is the repository resolved names, in any spelling, as Legion records an issue on it, or
// why Legion does not: left out (leftOut) with its reason, or not read from GitHub yet, and which
// set gave the answer (source). The mapping is read first, under both names of a configured
// rename, and the recorded set second, under both names too, so the project's own mapping
// decides a repository both name and the repositories of other projects' issues are answered for
// all the same. unread is a repository Legion has no answer for now: one the mapping leaves out
// because GitHub did not answer its installation read, or its default-branch read past GitHub's
// rate limit (refusedRepository.readErr), and one neither set names, which is a repository no
// preparation has read GitHub for yet — the preparation of an issue admission would record on it
// reads it (prepareUnmapped), or, in the boot's listing, leaves it for the recovery to read
// (leaveForRecovery), and the issue is recorded or refused on what GitHub says then, so nothing
// is refused here for the want of a mapping entry.
//
// The source is what each caller that tells the two kinds of repository apart reads
// (prepareUnrecorded, ResolveRepository, repositoryUnread), so the branch it takes and the answer
// it acts on are one read of the sets. A second question asked of the mapping after this one reads
// it again, and a refresh that publishes between the two reads answers them from two mappings.
//
// Each set is read under the name resolved carries and then, when that set does not name it,
// under the other name of the configured rename that names it (ghrepo.Under), so an issue
// resolved to the repository as it was is recorded on the one GitHub holds now, under one name:
// Resolution.Repo and Resolution.GitHubRepo agree, as a record demands (record.Store.PutIssue).
// The mapping is read under both names before the recorded set is read under either, since the
// recorded set is written only for a repository no mapping names (prepareUnmapped) and is never
// pruned: an entry kept from before Dispatch mapped the pair's other name would otherwise answer
// for a repository the mapping now serves, from the one read made of it at its first preparation.
func (r *repositories) lookup(resolved string) heldRepository {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if answer := r.mappingAnswer(resolved); answer.source != fromNeither {
		return answer
	}
	// As in mappingAnswer, the read is a map lookup that fails for no reason.
	if read, _ := ghrepo.Under(r.renames, resolved, func(name string) (ghbranch.Repository, error) {
		return r.recorded[strings.ToLower(name)], nil
	}); read != (ghbranch.Repository{}) {
		return heldRepository{mapped: mappedRepository{Repo: read.Name}, read: read, source: fromRecorded}
	}
	return heldRepository{refusal: fmt.Sprintf("GitHub has not been read for %s yet", resolved), unread: true}
}

// repositorySource is the set that answered a lookup.
type repositorySource uint8

const (
	// fromNeither is a repository neither set names: no preparation has read GitHub for it yet,
	// or GitHub's last answer for it refused it, which is not kept. The preparation of an issue
	// admission would record on it reads GitHub (prepareUnmapped), outside the boot's listing.
	fromNeither repositorySource = iota
	// fromMapping is a repository Dispatch's mapping of the project names, served or left out,
	// whose installations the mapping's own reads answer for (refreshMapping).
	fromMapping
	// fromRecorded is a repository no mapping names that a preparation read GitHub for and found
	// both role Apps installed on, answered from that read (recordRepository), which nothing
	// reads again.
	fromRecorded
)

// heldRepository is lookup's answer: the repository Legion records an issue on, or the refusal
// that leaves it out, and the set that answered (source). read is GitHub's kept read where the
// recorded set answered, its spelling and default branch, which is what an issue on it is
// recorded with. Its zero value is a name the set being read does not name, which is what
// ghrepo.Under reads the other name of the pair on.
type heldRepository struct {
	mapped          mappedRepository
	refusal         string
	leftOut, unread bool
	read            ghbranch.Repository
	source          repositorySource
}

// mappingAnswer is the mapping's answer for the repository resolved names, in any spelling and
// under either name of a configured rename: served, left out, or, the zero value, not named.
// Its caller holds the read lock.
func (r *repositories) mappingAnswer(resolved string) heldRepository {
	// Each read is a map lookup under the lock already held, so it fails for no reason and its
	// error is always nil; ghrepo.Under holds the order the two names are read in.
	answer, _ := ghrepo.Under(r.renames, resolved, func(name string) (heldRepository, error) {
		key := strings.ToLower(name)
		if mapped, ok := r.accepted[key]; ok {
			return heldRepository{mapped: mapped, source: fromMapping}, nil
		}
		if refused, ok := r.refused[key]; ok {
			return heldRepository{refusal: refused.Reason, leftOut: true, unread: refused.readErr != nil, source: fromMapping}, nil
		}
		return heldRepository{}, nil
	})
	return answer
}

// recordRepository keeps GitHub's answer for a repository no mapping names — its own spelling and
// its default branch — so that one repository is read once however many issues name it. The
// preparation that read it calls this before the issue's fact is applied, and the apply reads it
// back through lookup, which performs no I/O (workflowRuntime.ResolveRepository).
//
// asked is the name the preparation read GitHub under, which is the name the issue resolves to
// and asks for again at its apply and at every later preparation. Where GitHub answered under
// another name — the 301 of a repository GitHub renamed, which the read follows — the answer is
// kept under that name too, so the asked name finds it: one repository's one read, reached by
// either name.
func (r *repositories) recordRepository(asked string, read ghbranch.Repository) {
	r.mu.Lock()
	defer r.mu.Unlock()
	named := strings.ToLower(read.Name.String())
	r.recorded[named] = read
	if key := strings.ToLower(asked); key != named {
		r.recorded[key] = read
	}
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

// setUnverified records the served repositories the last read of the mapping kept on their last
// answer, its installation read having failed.
func (r *repositories) setUnverified(names []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.unverified = names
}

// kept is whether repo, in any spelling or under a configured rename, is served on its last
// answer (setUnverified).
func (r *repositories) kept(repo string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.ContainsFunc(r.unverified, func(name string) bool { return r.renames.Same(name, repo) })
}

// setWithoutWebhookApp records the repositories the last read of the mapping serves and found
// Dispatch's GitHub App not known to be installed on.
func (r *repositories) setWithoutWebhookApp(gaps []webhookAppGap) {
	sorted := slices.Clone(gaps)
	slices.SortFunc(sorted, func(a, b webhookAppGap) int { return strings.Compare(a.Repo, b.Repo) })
	r.mu.Lock()
	defer r.mu.Unlock()
	r.withoutWebhookApp = sorted
}

// keepWebhookAnswer keeps gap, Dispatch's answer of its webhook App check (checkWebhookApp) for
// repo, a repository no mapping names, in place of the answer last kept for it
// (recordedWebhookGaps): "", Dispatch answering that its App is installed, keeps none.
func (r *repositories) keepWebhookAnswer(repo ghrepo.Repository, gap string) {
	key := strings.ToLower(repo.String())
	r.mu.Lock()
	defer r.mu.Unlock()
	if gap == "" {
		delete(r.recordedWebhookGaps, key)
		return
	}
	r.recordedWebhookGaps[key] = gap
}

// WithoutWebhookApp is every served repository Dispatch's GitHub App is not known to be installed
// on, with why, sorted by name: the mapping's (setWithoutWebhookApp), and each repository served
// lists as the records' own whose last check Dispatch answered with a gap (recordedWebhookGaps),
// under the name served lists it by. The latter are read against live and the mapping as they
// stand now, as served reads them, so the state names such a gap for exactly as long as it lists
// the repository, whichever of the two changed since Dispatch was asked, and never beside the
// mapping's own entry for it.
func (r *repositories) WithoutWebhookApp() []webhookAppGap {
	r.mu.RLock()
	defer r.mu.RUnlock()
	gaps := slices.Clone(r.withoutWebhookApp)
	for key, reason := range r.recordedWebhookGaps {
		if named, held := r.live[key]; held && r.unmapped(named) {
			gaps = append(gaps, webhookAppGap{Repo: named.String(), Reason: reason})
		}
	}
	// The mapping's gaps are held sorted, so only a list that gained one of the records' needs
	// sorting again.
	if len(gaps) > len(r.withoutWebhookApp) {
		slices.SortFunc(gaps, func(a, b webhookAppGap) int { return strings.Compare(a.Repo, b.Repo) })
	}
	return gaps
}

// recordedOnly is every repository served lists as the records' own — one a live record names
// and no mapping does — as live holds it: the repositories Dispatch is asked of its GitHub App
// apart from the mapping's reads (checkRecordedWebhookApps).
func (r *repositories) recordedOnly() []ghrepo.Repository {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var repos []ghrepo.Repository
	for _, named := range r.live {
		if r.unmapped(named) {
			repos = append(repos, named)
		}
	}
	return repos
}

// unmapped is whether no mapping names repo, in any spelling or under either name of a configured
// rename (mappingAnswer): a repository a live record names is the records' own to report exactly
// then, and the mapping's otherwise (served). Its caller holds the read lock.
func (r *repositories) unmapped(repo ghrepo.Repository) bool {
	return r.mappingAnswer(repo.String()).source == fromNeither
}

// awaitingRead is every repository a read GitHub did not answer left as it stands, by reason: each
// left out for its failed installation read or its default-branch read past GitHub's rate limit,
// and each served one kept on its last answer. While any is, the
// mapping is read again of the daemon's own (refreshOnMappingChange).
func (r *repositories) awaitingRead() []string {
	waiting := refusalReasons(readFailures(r.Refused()))
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, name := range r.unverified {
		waiting = append(waiting, name+": served on its last answer, its installation read having failed")
	}
	return waiting
}

// served is every repository the daemon serves, sorted by the lowercased <owner>/<name> each is
// held under: the mapping's accepted entries and every repository a live issue record of the
// project names that no mapping names (live). Since INFRA-2543 the mapping no longer decides
// where the project may work — the App installation does — so the two sets are one served set
// wherever the daemon reports what it works in. The mapping alone (Accepted) bounds none of
// the intake, the caches and the credentials: GitHub intake hears it and every repository a
// record names (heardRepositories), the caches cover it and the live records' repositories
// (cacheRepositories), and a gh bundle reads it only to tell which configured renames resolve,
// its directories being the owners the role's App is installed on (appauth's bundleDirs).
//
// The records' half is read from the records themselves, never from the reads the preparations
// kept (recorded). A restart empties those reads, so a daemon that booted over a live tree on a
// repository no mapping names would report it nowhere until some issue on it was prepared
// again; and a preparation keeps a read for an issue Legion does not record in the end, a todo
// child under a parent no live tree holds, so the state would name a repository no tree of the
// project works in.
//
// A repository a record names that the mapping names too, in either name of a configured
// rename, is the mapping's to report and is left out here. Served, it is the mapping's own
// entry, which carries the project's default where a record holds none, and under the name the
// mapping serves it under: a record keeps the name it was written under for its whole life, so
// after a configured rename's transfer it names the repository the mapping serves under the
// pair's other name, which is one repository, served once. Left out, it stands under Refused
// with why, and an entry of its own here would say both role Apps are installed on it (view).
func (r *repositories) served() []mappedRepository {
	r.mu.RLock()
	defer r.mu.RUnlock()
	all := make(map[string]mappedRepository, len(r.live)+len(r.accepted))
	for key, named := range r.live {
		if r.unmapped(named) {
			all[key] = mappedRepository{Repo: named}
		}
	}
	maps.Copy(all, r.accepted)
	served := make([]mappedRepository, 0, len(all))
	for _, key := range slices.Sorted(maps.Keys(all)) {
		served = append(served, all[key])
	}
	return served
}

// view is what the daemon serves as its state's repositories: every repository the daemon works
// in (served, api.RepositoriesView.Mapped), each with the Kubernetes runtime's repository cache
// for it where the runtime has made one (none under tmux), matched whatever the case, and the
// rest of the caches listed apart — the cache of a repository the daemon serves no longer,
// whose last record is done and whose cache has not been retired yet (cacheRepositories).
//
// Mapped holds the repositories the records name beside the mapped ones because that is what
// the daemon serves (INFRA-2543 R6): reporting the mapping alone left a repository only a
// record named in neither Mapped nor Refused, while its live cache fell among the caches of
// repositories the daemon no longer serves.
//
// Every served entry carries both role Apps as installed (api.AppInstallationsView), which is
// the admission rule of each of the two doors rather than a second answer about it: a mapped
// repository either App is not installed on, or whose installation GitHub did not answer for,
// is refused (checkMapping), and an issue on a repository no mapping names is recorded only once
// GitHub answered that both Apps are installed on it, which is what a record on one means
// (prepareUnmapped). The answer that tells the two Apps apart is a refused entry's: each carries
// what GitHub answered of both Apps on the name Dispatch maps (refusedRepository.apps), and none
// where a read of that name failed or the name is no <owner>/<name>, its reason saying so.
func (r *repositories) view(caches []sandbox.CacheStatus) api.RepositoriesView {
	byRepo := map[string]sandbox.CacheStatus{}
	for _, cache := range caches {
		byRepo[strings.ToLower(cache.Repo.String())] = cache
	}
	view := api.RepositoriesView{Owners: r.Owners()}
	for _, served := range r.served() {
		entry := api.MappedRepositoryView{
			Repo:    served.Repo.String(),
			Default: served.Default,
			Apps:    api.AppInstallationsView{Implement: true, Review: true},
		}
		key := strings.ToLower(served.Repo.String())
		if cache, held := byRepo[key]; held {
			entry.Cache = new(cacheView(cache))
			delete(byRepo, key)
		}
		view.Mapped = append(view.Mapped, entry)
	}
	for _, refused := range r.Refused() {
		view.Refused = append(view.Refused, api.RefusedRepositoryView{Repo: refused.Repo, Reason: refused.Reason, Apps: refused.apps.view()})
	}
	for _, gap := range r.WithoutWebhookApp() {
		view.WithoutWebhookApp = append(view.WithoutWebhookApp, api.WebhookAppGapView{Repo: gap.Repo, Reason: gap.Reason})
	}
	for _, cache := range caches {
		if _, unserved := byRepo[strings.ToLower(cache.Repo.String())]; unserved {
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

// readMapping reads Dispatch's repository mapping (listMapping) and checks it for project
// against both GitHub Apps' installations and Dispatch's (checkMapping), over renames, the
// deployment's configured pairs, so a caller of this helper reads the mapping under exactly the
// rule loadMapping and refreshMapping read it under, the partner read among it.
func readMapping(ctx context.Context, client dispatch.Client, project string, installs appauth.Installations, renames ghrepo.Renames) ([]mappedRepository, []refusedRepository, []webhookAppGap, error) {
	mappings, err := listMapping(ctx, client)
	if err != nil {
		return nil, nil, nil, err
	}
	return checkMapping(ctx, client, mappings, project, installs, renames)
}

// listMapping is Dispatch's repository mapping, every project's.
func listMapping(ctx context.Context, client dispatch.Client) ([]dispatch.RepoProject, error) {
	mappings, err := client.ListRepoProjects(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the repository mapping from Dispatch: %w", err)
	}
	return mappings, nil
}

// checkMapping checks, for each repository mappings maps to project, that both GitHub Apps are
// installed on it (installs, under each App's JWT; per repository, since an installation on an
// owner may cover only some of its repositories).
//
// When GitHub answers that no App of the deployment is installed on the mapped name and renames
// pairs that repository with another name, both Apps' installations are read for the pair's
// other name too (ghrepo.Renames.Partner, which answers from either side: this is about which
// name GitHub has the repository under, not about which way a resolution runs). An App is
// installed on an owner, so a transfer to another owner leaves the mapped name covered by no
// installation of the owner it still names, neither App's, while the name it moved to is covered
// by the ones that have it. The entry is served under the name both Apps answer for, and
// recorded under that name from here on, so the boot's own read of GitHub's spelling
// (nameAccepted, minted as the implement App) is made for an owner whose installation exists.
//
// A mapped name that holds one of the two Apps and not the other is the operator's own gap
// rather than a transfer: the pair's other name is read for neither App, and the entry is left
// out naming the App that is missing. Reading the pair there would serve a repository under a
// name the operator never mapped on the strength of one missing installation, which before any
// transfer — a pair configured early, and the Apps installed one at a time — would move a whole
// project onto the repository the pair names. A mapped name both Apps answer for costs no second
// read either: that is the state before a transfer, where the mapping names the repository
// GitHub still holds.
//
// A repository neither name answers for is refused, naming each App that is not installed on each
// name read and each read that failed, under Dispatch's own spelling of the mapped name, which is
// what a refusal is keyed on. The refusal is for now, read again by refreshOnMappingChange on
// installReadRetry (refusedRepository.readErr), whenever any read of either name failed to answer
// (any failure, a read longer than installReadAttempt among them), and for good only when every
// read of both names answered and at least one said not installed. The older rule, where an App
// answering not installed made the refusal permanent whatever else failed, cannot survive a
// partner read: a transient failure on the name a repository moved to would leave the entry out
// for good, with the mapped name's honest "not installed" as the reason no retry ever revisits,
// which is the shape of the outage the partner read exists to end. The rest of the mapping is
// checked as it stands either way: one repository GitHub will not answer for leaves out only its
// own issues, whose admission waits for it (prepareUnrecorded), so nothing is left out for good
// on a guess. The check fails only when ctx ends.
//
// Accepted repositories carry the spelling of the name that answered until nameAccepted reads
// GitHub's. Beside the two role Apps, it asks client whether Dispatch's GitHub App is installed
// on each mapped repository (checkWebhookApp), under the mapped name, since that is the name
// Dispatch's own mapping holds and the only one known while an entry's three reads run at once.
// gaps is every repository it is not known to be on, refused ones included, for the caller to
// keep the served ones (repositories.servedGaps), each recorded against the name its entry is
// served under and under the mapped name when no name is served: that answer
// never refuses a repository and never fails the check, since Legion serves a repository whose
// events GitHub does not send by the pass. The reads run at once, mappingReads at a time, so a
// check takes about as long as its slowest read, twice where a partner read is made, rather than
// the sum of them: the partner reads are one more round, made only for the entries that asked
// for one.
func checkMapping(ctx context.Context, client dispatch.Client, mappings []dispatch.RepoProject, project string, installs appauth.Installations, renames ghrepo.Renames) ([]mappedRepository, []refusedRepository, []webhookAppGap, error) {
	// nameReads is both Apps' answers for one name of an entry: installed and failed are in
	// appauth.Roles order, and each read sets exactly one of the two.
	type nameReads struct {
		repo      ghrepo.Repository
		installed []bool
		failed    []error
	}
	type mapped struct {
		mapping dispatch.RepoProject
		repo    ghrepo.Repository
		parse   error
		// reads is each name both Apps were read for, in the order they were read: the mapped
		// name, and the pair's other name when no App of the deployment answered that it is
		// installed on the mapped one. Both are allocated at once, so an entry that reads a
		// partner costs one allocation rather than a one-element slice and a grow that copies
		// it. No read is
		// in flight across that append: the first round is collected whole (group.Wait) before
		// any second element is appended, and each round is handed an element's address only
		// after the round before it has finished.
		reads []nameReads
		// webhookGap is why Dispatch's GitHub App is not known to be installed on it, "" when it is.
		webhookGap string
	}
	var entries []mapped
	for _, mapping := range mappings {
		if mapping.Project != project {
			continue
		}
		repo, err := ghrepo.Parse("the repository Dispatch maps to "+project, mapping.Repo)
		entries = append(entries, mapped{mapping: mapping, repo: repo, parse: err})
	}
	// readBoth reads both Apps' installations of reads.repo into it, one read per App, each
	// bounded on its own, and keeps every failure rather than failing the group.
	readBoth := func(group *errgroup.Group, reads *nameReads) {
		for index, role := range appauth.Roles {
			group.Go(func() error {
				read, cancel := context.WithTimeout(ctx, installReadAttempt)
				defer cancel()
				reads.installed[index], reads.failed[index] = installs.InstalledOn(read, role, reads.repo)
				return nil
			})
		}
	}
	var group errgroup.Group
	group.SetLimit(mappingReads)
	for i := range entries {
		m := &entries[i]
		if m.parse != nil {
			continue
		}
		m.reads = append(make([]nameReads, 0, 2), nameReads{
			repo: m.repo, installed: make([]bool, len(appauth.Roles)), failed: make([]error, len(appauth.Roles))})
		readBoth(&group, &m.reads[0])
		group.Go(func() error {
			read, cancel := context.WithTimeout(ctx, installReadAttempt)
			defer cancel()
			m.webhookGap = checkWebhookApp(read, client, m.repo)
			return nil
		})
	}
	_ = group.Wait() // every read keeps its own failure, so none fails the group
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, fmt.Errorf("check the GitHub Apps' installations: %w", err)
	}
	var partners errgroup.Group
	partners.SetLimit(mappingReads)
	for i := range entries {
		m := &entries[i]
		if m.parse != nil {
			continue
		}
		// noAppInstalled is GitHub answering, for every App of the deployment, that it is not
		// installed on the mapped name, which is the answer a partner read is made for: a
		// transfer takes the repository out of every installation of the owner it names, both
		// Apps' alike, since an App is installed on an owner. A mapped name that holds one of
		// the two Apps and not the other is the operator's own gap rather than a transfer, so
		// reading the pair's other name there would serve a repository under a name the
		// operator never mapped on the strength of a missing installation, and switch a project
		// onto the repository a pair names before any transfer has happened; that entry is left
		// out naming the App that is missing. A read that failed is no answer at all — that
		// entry is read again instead (installReadRetry) — so an entry one of whose reads failed
		// asks for no partner either, and neither does one every App is installed on.
		noAppInstalled := true
		for index := range appauth.Roles {
			noAppInstalled = noAppInstalled && m.reads[0].failed[index] == nil && !m.reads[0].installed[index]
		}
		if !noAppInstalled {
			continue
		}
		// The pair is looked up only for an entry that asked for one, so a deployment that
		// configures no rename joins no name here at all.
		partner, paired := renames.Partner(m.repo.String())
		if !paired {
			continue
		}
		m.reads = append(m.reads, nameReads{
			repo: partner, installed: make([]bool, len(appauth.Roles)), failed: make([]error, len(appauth.Roles))})
		readBoth(&partners, &m.reads[1])
	}
	_ = partners.Wait() // as above: a partner read's failure is the entry's, not the check's
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, fmt.Errorf("check the GitHub Apps' installations: %w", err)
	}
	var gaps []webhookAppGap
	var accepted []mappedRepository
	var refused []refusedRepository
	for _, m := range entries {
		if m.parse != nil {
			// A name Dispatch spells unparseably is read for no App at all, Dispatch's own
			// among them, so it carries no gap to record. The parse error quotes Dispatch's
			// spelling, which legion state and a child-release-stalled notice carry on, so it is
			// kept as a far side's text is.
			refused = append(refused, refusedRepository{Repo: m.mapping.Repo, Reason: storetext.FarSideError(m.parse.Error())})
			continue
		}
		var reasons []string
		var readErr error
		var served ghrepo.Repository
		var serve bool
		for _, reads := range m.reads {
			var missing, unread []string
			for index, role := range appauth.Roles {
				switch {
				case reads.failed[index] != nil:
					// The reason is served by legion state and quoted in a child-release-stalled
					// notice, so GitHub's error is kept as any far side's error text is.
					unread = append(unread, fmt.Sprintf("the %s App's installation on %s could not be read: %s", role, reads.repo, storetext.FarSideError(reads.failed[index].Error())))
					if readErr == nil {
						readErr = fmt.Errorf("check the %s App's installation on %s: %w", role, reads.repo, reads.failed[index])
					}
				case !reads.installed[index]:
					missing = append(missing, string(role))
				}
			}
			if len(missing) == 0 && len(unread) == 0 {
				served, serve = reads.repo, true
				break
			}
			switch len(missing) {
			case 0:
			case 1:
				reasons = append(reasons, fmt.Sprintf("the %s App is not installed on %s", missing[0], reads.repo))
			default:
				reasons = append(reasons, fmt.Sprintf("the %s Apps are not installed on %s", strings.Join(missing, " and "), reads.repo))
			}
			reasons = append(reasons, unread...)
		}
		// Dispatch was asked under the mapped name, the name its own mapping holds and the only
		// name known while an entry's three reads run at once, and the gap is recorded against
		// the name the entry is served under, which the installation reads have decided by here:
		// legion state names a repository the daemon serves, and a transfer leaves the mapped
		// name one the project serves nothing under. An entry no name is served for keeps the
		// mapped name, which is what its refusal is keyed on too.
		if m.webhookGap != "" {
			recorded := m.mapping.Repo
			if serve {
				recorded = served.String()
			}
			gaps = append(gaps, webhookAppGap{Repo: recorded, Reason: m.webhookGap})
		}
		if serve {
			accepted = append(accepted, mappedRepository{Repo: served, Default: m.mapping.Default})
			continue
		}
		// readErr is set exactly when a read of either name failed to answer, and keeping it is
		// the refusal's kind: Legion reads this entry again (installReadRetry) until GitHub has
		// answered every read of both names, and leaves it out for good only once it has and an
		// App is not installed. Clearing it on a "not installed" answer, as the check did while
		// it read one name, would make a transient failure on the pair's other name permanent —
		// the mapped name's own answer is a true "not installed" after a transfer, so the refusal
		// would read as settled and no retry would ever revisit it.
		if readErr != nil {
			reasons = append(reasons, "Legion reads it again")
		}
		// The Apps' answers kept for legion state are the mapped name's, the first name read, which
		// is the name the refusal is keyed on and the state names: a partner read answers for
		// another name, which the reason names with what GitHub said of it.
		refused = append(refused, refusedRepository{Repo: m.mapping.Repo, Reason: strings.Join(reasons, "; "),
			apps: appsAnswered(m.reads[0].installed, m.reads[0].failed), readErr: readErr, mappedDefault: m.mapping.Default})
	}
	return accepted, refused, gaps, nil
}

// appsAnswered is both role Apps' installation on one name as legion state reports a refusal's
// (refusedRepository.apps), from installed and failed in appauth.Roles order as checkMapping reads
// them: not answered while any read of the name failed, since a read GitHub did not answer says
// nothing of the App it was for, and the refusal's reason names that read instead.
func appsAnswered(installed []bool, failed []error) appAnswers {
	if slices.ContainsFunc(failed, func(err error) bool { return err != nil }) {
		return appAnswers{}
	}
	apps := appAnswers{answered: true}
	for index, role := range appauth.Roles {
		switch role {
		case appauth.Implement:
			apps.implement = installed[index]
		case appauth.Review:
			apps.review = installed[index]
		}
	}
	return apps
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

// checkRecordedWebhookApps asks Dispatch whether its GitHub App is installed on each repository
// served lists as the records' own — one a live record of the project names and no mapping does
// (recordedOnly) — and keeps each answer for legion state (keepWebhookAnswer, WithoutWebhookApp),
// as checkMapping asks of each mapped repository. A missing App refuses nothing here either: the
// repository is served whatever Dispatch answers, and a failed check is a gap with its failure,
// never a reason to read again.
//
// It is the mapping's check made of the records' half of what the daemon serves, so it runs
// where that half is read again: the refresher runs it as it starts, over the live set the boot
// read from the records (heardAtBoot), so a restarted daemon names the gap of a repository only a
// live tree works in from the records, as served lists the repository from them; and after every
// read of the mapping that applies, over the live set that read took (refreshCacheRepositories),
// so an App installed since is taken, and a repository the mapping stopped naming is checked as
// the records' own, at the same pace as every mapped repository's check. A repository a record
// first names between two reads was checked at the preparation that read GitHub for it
// (prepareUnmapped).
//
// It is bounded as checkMapping bounds its own reads: each check by installReadAttempt and
// mappingReads of them at once, and nothing bounds the whole, so a check of n repositories takes
// at most ceil(n / mappingReads) of installReadAttempt. That is also why the boot does not run
// it: the boot's read of the records runs inside its connect attempt, which bootTimeout bounds,
// and Dispatch answers the check by reading GitHub, so a GitHub slow through Dispatch could use
// that bound up on a few dozen such repositories and fail the boot's attempt over a check that
// refuses nothing. Answers cut short by ctx's end are the stop's, not Dispatch's, and none is
// kept.
func (w *workflowRuntime) checkRecordedWebhookApps(ctx context.Context) {
	repos := w.repositories.recordedOnly()
	if len(repos) == 0 {
		return
	}
	gaps := make([]string, len(repos))
	var group errgroup.Group
	group.SetLimit(mappingReads)
	for index, repo := range repos {
		group.Go(func() error {
			read, cancel := context.WithTimeout(ctx, installReadAttempt)
			defer cancel()
			gaps[index] = checkWebhookApp(read, w.dispatch, repo)
			return nil
		})
	}
	_ = group.Wait() // every check keeps its own answer, a failure among them, so none fails the group
	if ctx.Err() != nil {
		return
	}
	var found []webhookAppGap
	for index, repo := range repos {
		w.repositories.keepWebhookAnswer(repo, gaps[index])
		if gaps[index] != "" {
			found = append(found, webhookAppGap{Repo: repo.String(), Reason: gaps[index]})
		}
	}
	if len(found) != 0 {
		w.log.Info("legion workflow checked Dispatch's GitHub App on the repositories only a live record names",
			"withoutWebhookApp", webhookGapReasons(found))
	}
}

// servedGaps is each of gaps whose repository the mapping serves, once per repository: the
// mapping's half of what legion state names without the webhook App, the records' half being
// checked apart (checkRecordedWebhookApps).
//
// A gap whose repository the mapping serves is never dropped. servedGaps runs after set, so the
// accepted set is what the daemon serves from here on and is authoritative over any refusal: a
// configured pair's two names may both be mapped, one entry served while the other is left out,
// and the entry left out is no reason to say nothing about the webhook App on the repository the
// daemon does serve. That shape is not rare — an entry whose read failed is refused for now, and
// one whose mapped name holds one App and not the other is refused for good, since it asks for no
// partner read — and matching every gap against the refusals alone dropped the served
// repository's own gap with it.
//
// The rest are dropped by the mapping's own rename equivalence (same) rather than by a comparison
// of spellings, which is why this is a method of the mapping and not the package function it was:
// a gap is recorded against the name its entry is served under (checkMapping) while a refusal is
// keyed on Dispatch's spelling of the mapped name, so after a transfer the two name one
// repository under two names and strings.EqualFold alone would keep a gap for a repository the
// daemon leaves out.
//
// Two mapping entries that name one repository are served as one (set), and each was asked of
// Dispatch under its own mapped name, so both may carry a gap for the one served repository and
// legion state would print it twice. The second is dropped on the lowercased <owner>/<name> the
// served entries are keyed by, which is the collision set merges on: a gap left under a mapped
// name of its own is a repository of its own.
func (r *repositories) servedGaps(gaps []webhookAppGap, refused []refusedRepository) []webhookAppGap {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var served []webhookAppGap
	for _, gap := range gaps {
		// The served repositories are held by their lowercased <owner>/<name> (set), which is the
		// name a served entry's gap is recorded against, so this is one map lookup per gap.
		_, serves := r.accepted[strings.ToLower(gap.Repo)]
		if !serves && slices.ContainsFunc(refused, func(refusal refusedRepository) bool { return r.same(refusal.Repo, gap.Repo) }) {
			continue
		}
		if slices.ContainsFunc(served, func(held webhookAppGap) bool { return strings.EqualFold(held.Repo, gap.Repo) }) {
			continue
		}
		served = append(served, gap)
	}
	return served
}

// installReadRetry is how long refreshOnMappingChange waits before it reads the mapping again of
// its own while a read GitHub did not answer, an installation read or a default-branch read past
// GitHub's rate limit, leaves a repository as it stands (awaitingRead): 30 s doubling to 10 min,
// without limit (Attempts is unused), back to 30 s once none is left.
var installReadRetry = bootprobe.Retry{Initial: 30 * time.Second, Max: 10 * time.Minute}

// recoveryHoldBound is how long a recovery's release waits stay untold while its listing is held
// for the Dispatch consumer (recoverRepositories): the hold watchdog's warning interval
// (defaultHoldWarnEvery), by which a hold that has not released is a fault worth naming.
var recoveryHoldBound = defaultHoldWarnEvery

// installReadAttempt bounds one installation read: as long as any one GitHub call the boot waits
// for (appMintAttempt), so a GitHub that answers slowly is still answered for, while a read GitHub
// has not answered by then is a failed read, and a repository GitHub hangs on is left out for now
// rather than holding up the mapping. It bounds every webhook App check (checkWebhookApp) the same
// way: the mapping's, a preparation's (prepareUnmapped) and the refresher's of the repositories
// only a live record names (checkRecordedWebhookApps). Nothing bounds a check as a whole:
// mappingReads reads run at once, so a check of n mapped repositories, three reads each, takes at
// most ceil(3n / mappingReads) of these.
var installReadAttempt = 15 * time.Second

// readFailures is the refusals of refused that a read GitHub did not answer made (readErr), its
// installation read or its default-branch read past GitHub's rate limit (unreadDefaultBranch).
func readFailures(refused []refusedRepository) []refusedRepository {
	var failed []refusedRepository
	for _, refusal := range refused {
		if refusal.readErr != nil {
			failed = append(failed, refusal)
		}
	}
	return failed
}

// mappingReads is how many of a mapping read's GitHub reads run at once.
const mappingReads = 8

// nameAccepted is accepted as GitHub names each repository (named: its own spelling of owner and
// name, which its events and the subjects Envoy publishes them on carry), each read with its
// default branch as the implement App, mappingReads at a time. A read GitHub refuses for its rate
// limit fails no other read: its repository is returned in limited rather than among the named
// ones, in accepted's order, for the caller to serve on what the daemon holds or to wait out
// (holdLimited, waitOutLimits), since the limit says nothing about the repository. Any other
// failed read fails the whole read.
func nameAccepted(ctx context.Context, accepted []mappedRepository, named *defaultBranches) ([]mappedRepository, []limitedRead, error) {
	out := make([]mappedRepository, len(accepted))
	limits := make([]limitedRead, len(accepted))
	group, read := errgroup.WithContext(ctx)
	group.SetLimit(mappingReads)
	for index, mapped := range accepted {
		group.Go(func() error {
			repo, err := named.Named(read, mapped.Repo)
			if answer, wait, limited := defaultBranchLimit(err); limited {
				limits[index] = limitedRead{mapped: mapped, answer: answer, err: err, wait: wait, until: named.now().Add(wait)}
				return nil
			}
			if err != nil {
				return err
			}
			out[index] = mappedRepository{Repo: repo.Name, Default: mapped.Default}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, nil, err
	}
	// Both are compacted in place: each entry is written at or before the index it was read from.
	served, limited := out[:0], limits[:0]
	for index := range accepted {
		if limits[index].answer != nil {
			limited = append(limited, limits[index])
			continue
		}
		served = append(served, out[index])
	}
	return served, limited, nil
}

// loadMapping is the boot's read of the repository mapping. Dispatch's read (listMapping) waits
// out an unreachable Dispatch (dispatch.Unreachable) on readinessRetry, without limit, as the
// boot's Dispatch listing for admission does: a Dispatch down for longer than any bound (no ready
// endpoint, every dial refused) would otherwise exit the daemon into a supervisor that restarts it
// straight back into the same refusal, and the API does not listen until the read passes, so the
// daemon serves nothing meanwhile; any other failure of it refuses the boot. The installation
// check (checkMapping) never refuses the boot: a repository whose read fails is left out for now
// and read again by refreshOnMappingChange, on installReadRetry, until GitHub answers, and served
// then with no restart. A boot whose every repository's read fails comes up serving none and logs
// so at ERROR with the first failure, since a daemon that exits instead restarts straight into
// GitHub's same refusal with its API and its recovery down, while one that runs heals once GitHub
// answers. A project with no repository mapped refuses the boot. One whose every mapped
// repository lacks an App boots serving none, each left out and reported: it admits nothing,
// saying why on each issue, until an App is installed and a read of the mapping finds it, one the
// next mapping change or an issue held for it asks for (requestMappingRead). It gives the check
// the deployment's configured renames, so a mapped repository GitHub holds under the other name
// of its pair is served under that name from this load onward, and nameMapping's read of GitHub
// is made for an owner whose App installations exist. The webhook App check
// neither refuses the boot nor is read again of its own: each served repository it finds Dispatch's
// App not known to be on is kept for legion state (setWithoutWebhookApp), and every later read of
// the mapping checks it again. nameMapping, once the Apps' tokens are minted, reads GitHub's
// spelling.
func (w *workflowRuntime) loadMapping(ctx context.Context) error {
	var mappings []dispatch.RepoProject
	err := bootprobe.Run(ctx, "repository mapping read", readinessRetry, w.log, readinessAttempt(func(ctx context.Context) error {
		var err error
		mappings, err = listMapping(ctx, w.dispatch)
		return err
	}, appMintAttempt, dispatch.Unreachable))
	if err != nil {
		return err
	}
	accepted, refused, gaps, err := checkMapping(ctx, w.dispatch, mappings, w.dispatchProject, w.installs, w.repositories.renames)
	if err != nil {
		return err
	}
	if len(accepted) == 0 && len(refused) == 0 {
		return fmt.Errorf("project %s has no repository mapped in Dispatch (Settings → Repositories → Projects)", w.dispatchProject)
	}
	switch failed := readFailures(refused); {
	case len(failed) == 0:
	case len(accepted) == 0 && len(failed) == len(refused):
		w.log.Error("workflow: every mapped repository's GitHub App installation read failed, so the daemon boots serving none and reads them again",
			"repositories", len(failed), "retryIn", installReadRetry.Initial.String(), "error", failed[0].readErr)
	default:
		w.log.Warn("workflow: a mapped repository's GitHub App installation read failed, so the daemon boots leaving it out and reads it again",
			"refused", refusalReasons(failed), "retryIn", installReadRetry.Initial.String())
	}
	w.repositories.set(accepted, refused)
	w.repositories.setWithoutWebhookApp(w.repositories.servedGaps(gaps, refused))
	w.logUnservedRenames(accepted)
	w.logUnmappedCommitIssueTrailerRepos(accepted)
	return nil
}

// logUnservedRenames says, once at the boot's mapping load, that a configured rename names a
// repository the project does not serve, naming both names: the pair is read wherever a name is
// compared, but nothing is heard or served under the repository it moved to until Dispatch maps
// it, which is how a pair configured before the transfer stands. It changes nothing else: the
// served set is Dispatch's mapping alone, and no rename is ever read from GitHub.
func (w *workflowRuntime) logUnservedRenames(accepted []mappedRepository) {
	for _, pair := range w.repositories.renames.Pairs() {
		if slices.ContainsFunc(accepted, func(mapped mappedRepository) bool { return strings.EqualFold(mapped.Repo.String(), pair.To.String()) }) {
			continue
		}
		w.log.Warn("workflow: a configured repository rename names a repository this project does not serve, so the pair is read wherever a name is compared but nothing is served under it",
			"from", pair.From.String(), "to", pair.To.String(), "project", w.dispatchProject)
	}
}

// logUnmappedCommitIssueTrailerRepos says, once at the boot's mapping load, that
// `commit_issue_trailer_repos` names a repository Dispatch does not map to this project, naming
// the name and the project. It is a statement about the mapping and nothing else, which catches
// the typo this check exists for: the stamping decision reads no mapping at all (specs.issueTrailer
// compares the launch's own repository with the configured names), so a pane working a repository
// no mapping holds — one an issue of this project names as its own, which since INFRA-2543 may be
// any repository both Apps reach — stamps its commits exactly as a pane on a mapped one does.
//
// The two names are compared by the mapping's own equivalence (same: without case, and through a
// configured rename), the one the stamping decision reads, so a configured name the mapping holds
// only under the other name of a configured pair is said nothing about. It is said here, and
// refuses nothing, because `legion config check` reads no Dispatch at all — it runs
// config.LoadForValidation and daemon.CheckStart and nothing else (checkConfig,
// cmd/legion/main.go) — so it can refuse only what the file alone decides, and a name mapped the
// day after the file is written is no fault of the file.
func (w *workflowRuntime) logUnmappedCommitIssueTrailerRepos(accepted []mappedRepository) {
	for _, repo := range w.commitIssueTrailerRepos {
		name := repo.String()
		if slices.ContainsFunc(accepted, func(mapped mappedRepository) bool { return w.repositories.same(mapped.Repo.String(), name) }) {
			continue
		}
		w.log.Warn("workflow: commit_issue_trailer_repos names a repository Dispatch does not map to this project; a pane working that repository stamps its commits with the issue key all the same",
			"repository", name, "project", w.dispatchProject)
	}
}

// nameMapping replaces the served repositories with GitHub's spelling of each, on the boot's
// retry, and logs the mapping the daemon serves. A read GitHub refuses for its rate limit never
// refuses the boot, however long the limit stands: a repository the daemon holds a default branch
// for is served on it at once and read again after the limit's reset (holdLimited), and one it
// holds none for is waited out and read once more, and left out for now if GitHub still does not
// answer (waitOutLimits). A 404, and every other answer that is no limit, refuses the boot, and a
// 5xx is retried on appMintRetry (bootReadTransient).
func (w *workflowRuntime) nameMapping(ctx context.Context) error {
	var named []mappedRepository
	var limited []limitedRead
	mapped := w.repositories.Mapped()
	err := bootprobe.Run(ctx, "repository read", appMintRetry, w.log, readinessAttempt(func(ctx context.Context) error {
		var err error
		named, limited, err = nameAccepted(ctx, mapped, w.defaultBranches)
		return err
	}, appMintAttempt, bootReadTransient))
	if err != nil {
		return err
	}
	held, unheld, err := w.holdLimited(ctx, limited)
	if err != nil {
		return err
	}
	waited, unread, err := w.waitOutLimits(ctx, unheld)
	if err != nil {
		return err
	}
	named = append(append(named, held...), waited...)
	w.repositories.set(named, append(w.repositories.Refused(), unread...))
	w.log.Info("legion workflow boot stage", "stage", "repositories", "accepted", repositoryNames(w.repositories.Accepted()), "refused", refusalReasons(w.repositories.Refused()),
		"withoutWebhookApp", webhookGapReasons(w.repositories.WithoutWebhookApp()))
	return nil
}

// refreshMapping reads the repository mapping again: refreshOnMappingChange's read, which a
// mapping change in Dispatch, an admission held for it (requestMappingRead) or a read GitHub did
// not answer (installReadRetry) asks for. A repository the daemon serves whose
// installation read fails stays served on its last answer, with the name GitHub last gave it and
// no read of GitHub, recorded as unverified so it is read again: GitHub's failure to answer is no
// reason to stop serving a repository it answered for, and no reason either to hold back the rest
// of the mapping. One left out whose read fails stays left out, with the new failure. A
// repository whose default-branch read GitHub's rate limit refuses is served on the default
// branch the daemon holds for it and read again after the limit's reset (holdLimited), or, when
// it holds none, left out for now and read again as a failed installation read is. Dispatch's
// read, and the naming and publication after the check, are each bounded by appMintAttempt; the
// check itself is bounded per read (installReadAttempt), so a GitHub that answers slowly is
// answered for whatever the mapping's size. Each repository the mapping as last read left out for
// its failed read, and that this read no longer does (now served, refused for a missing App, or no
// longer mapped), is to be recovered (recoverRepositories): an event of an issue on it applied
// while it was unread said nothing and recorded nothing, so the issues it left are taken from
// Dispatch's listing once. It has GitHub intake hear every repository the new mapping serves and
// every repository a record of the project names, so a repository unmapped while an issue is
// recorded on it is still heard, and it does so before it publishes the new mapping, in the
// three steps the publication is written in below: a repository is never served while GitHub
// intake does not hear it, and a read that fails before the publication publishes nothing and is
// read again by the refresher. Which repositories an issue may be recorded on is no longer the
// mapping's to say (R4), so what the steps answer for is the served set alone: a repository no
// mapping names is heard from the record that first names it (hearRecorded), not from this read.
// The webhook App check's served repositories (servedGaps, kept ones included) replace the last
// read's with the publication; a failed check keeps nothing out and asks for no read again. The
// repositories only a live record names are checked once the read has applied, by its caller
// (refreshOnMappingChange, checkRecordedWebhookApps), outside mappingMu and over the live set
// this read took.
func (w *workflowRuntime) refreshMapping(ctx context.Context) error {
	w.mappingMu.Lock()
	defer w.mappingMu.Unlock()
	listing, cancel := context.WithTimeout(ctx, appMintAttempt)
	mappings, err := listMapping(listing, w.dispatch)
	cancel()
	if err != nil {
		return err
	}
	accepted, refused, gaps, err := checkMapping(ctx, w.dispatch, mappings, w.dispatchProject, w.installs, w.repositories.renames)
	if err != nil {
		return err
	}
	var kept []mappedRepository
	var unverified []string
	refused = slices.DeleteFunc(refused, func(failed refusedRepository) bool {
		last := w.repositories.lookup(failed.Repo)
		mapped := last.mapped
		if failed.readErr == nil || last.refusal != "" {
			return false
		}
		kept = append(kept, mappedRepository{Repo: mapped.Repo, Default: failed.mappedDefault})
		unverified = append(unverified, mapped.Repo.String())
		// The entry is served again under the name it was last served under, so its webhook App
		// gap is relabelled here: checkMapping refused the entry, so it served no name for it and
		// recorded the gap under Dispatch's mapped name, while legion state names the repository
		// the daemon serves (R5). After a transfer the two are two names, so without this the
		// state would name the owner the repository left for as long as the read keeps failing.
		for index, gap := range gaps {
			if strings.EqualFold(gap.Repo, failed.Repo) {
				gaps[index].Repo = mapped.Repo.String()
			}
		}
		w.log.Warn("workflow: a served repository's GitHub App installation read failed, so the daemon keeps serving it on its last answer and reads it again",
			"repo", mapped.Repo.String(), "error", failed.readErr)
		return true
	})
	ctx, cancel = context.WithTimeout(ctx, appMintAttempt)
	defer cancel()
	// A read GitHub's rate limit refuses is served on the default branch the daemon holds, or
	// left out for now and read again on installReadRetry when it holds none: the refresher never
	// waits a limit out, since it holds mappingMu, which every later read of the mapping waits on.
	var limited []limitedRead
	if accepted, limited, err = nameAccepted(ctx, accepted, w.defaultBranches); err != nil {
		return err
	}
	held, unheld, err := w.holdLimited(ctx, limited)
	if err != nil {
		return err
	}
	for _, read := range unheld {
		w.log.Warn("workflow: GitHub's rate limit refused the read of a mapped repository's default branch and the daemon holds none for it, so it leaves the repository out and reads it again",
			append([]any{"repo", read.mapped.Repo.String(), "retryIn", installReadRetry.Initial.String()}, limitAttributes(read)...)...)
		refused = append(refused, unreadDefaultBranch(read.mapped, read.err))
	}
	accepted = append(append(accepted, held...), kept...)
	served := make([]ghrepo.Repository, 0, len(accepted))
	for _, mapped := range accepted {
		served = append(served, mapped.Repo)
	}
	// The subscription update is a stream call, and a stream call is made under no transaction of
	// this pool: a missing durable consumer has it create one, which reads the same pool for the
	// stream's stored position (storedStreamTime), so a transaction waiting on it asks the pool
	// for a second connection while holding one, and every connection of the pool sat idle in a
	// transaction of that shape once the stream answered slowly (INFRA-3169). Holding the fact
	// serializer across it is worse still: a slow NATS stops every fact of the project for as
	// long as the call takes. So the read publishes in three steps.
	//
	// First, GitHub intake is given a superset to hear, snapshotted in a transaction of its own
	// that takes no serializer lock, and told of it outside any transaction. The superset is the
	// heard set over the union of the repositories the daemon serves now and the ones this read
	// will serve, so it holds both mappings' repositories and every repository a record of the
	// project names (heardRepositories), and a repository is never served while GitHub intake
	// does not hear it. An admission in this window records an issue on a repository of the old
	// mapping or of the new one, which the union covers, or on one no mapping names at all (R4),
	// which it covers itself: such an admission has intake hear its repository at its own commit
	// (hearRecorded), and a call that adds a repository takes mappingMu, which this read holds for
	// its whole run, so an addition is made before this step or after the third one and never
	// between them; one whose repository the held set names already adds nothing and takes no
	// lock, which the second step's own hold of the set makes sound. Hearing more
	// than the daemon serves costs an event that reaches no issue, which is why the third step
	// narrows it again. A failure here changes nothing, as a failed subscription update did
	// before.
	//
	// It is skipped whole when this read will serve exactly the repositories the daemon already
	// serves, which is what a refresh asked for by a mapping event that moved another project's
	// repository, by a held admission or by an installation read's retry usually finds. With
	// nothing added there is nothing that could be served while intake does not hear it, which is
	// the only thing this step is for, and the superset it would publish is the set the third
	// step publishes again: one JetStream consumer update and one read of the whole issues table,
	// both for a mapping that did not change.
	prepublished := !sameRepositories(w.repositories.Accepted(), served)
	var superset []ghrepo.Repository
	if prepublished {
		union := append(w.repositories.Accepted(), served...)
		if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
			var err error
			superset, err = w.heardRepositories(ctx, tx, union)
			return err
		}); err != nil {
			return err
		}
		if err := w.updateSubscription(ctx, superset); err != nil {
			return err
		}
		// What intake was last told is what the runtime holds as heard, so a read that fails
		// before its second step leaves the superset both heard and held as heard: the next
		// repository a record names is added to that set (hearRepository) rather than to a
		// smaller one, which would drop a repository this call published while the daemon
		// serves it.
		w.heard.Store(&superset)
	}
	// Second, the record snapshot and the publication run together under the lock every fact
	// applies under (intake.SerializeFacts), which an admission holds from resolving an issue's
	// repository through committing its record: one that resolved a repository of the old mapping
	// commits before the snapshot reads it, so the repository it recorded stays heard, and one
	// that resolves after resolves against the new mapping. An admission that recorded an issue
	// on a repository no mapping names is read the same way, since the snapshot holds every
	// repository a record names, whichever mapping named it or none (R4). The two have to be one
	// transaction holding that lock, which is why the stream calls are on either side of it and
	// not in it.
	var heard []ghrepo.Repository
	var added, removed []string
	if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		if err := intake.SerializeFacts(ctx, tx); err != nil {
			return fmt.Errorf("serialize the mapping's publication with admission: %w", err)
		}
		var err error
		if heard, err = w.heardRepositories(ctx, tx, served); err != nil {
			return err
		}
		if err := w.refreshCacheRepositories(ctx, tx, served); err != nil {
			return err
		}
		before := readFailures(w.repositories.Refused())
		added, removed = w.repositories.set(accepted, refused)
		w.repositories.setUnverified(unverified)
		w.repositories.setWithoutWebhookApp(w.repositories.servedGaps(gaps, refused))
		w.markRecovered(before, readFailures(w.repositories.Refused()))
		// The set this read publishes is held as heard here, under the fact lock, and not once
		// the third step has told intake of it. A recording whose repository the held set names
		// takes no lock and tells intake nothing (hearRepository), and the third step narrows to
		// this set: a record committed before the snapshot is in it, and one committed after it
		// took the fact lock only once this transaction released it, so its commit hook reads
		// this set or a later one. Held after the third step instead, the superset would answer
		// that hook, and a record on a repository the old mapping alone served, committed between
		// this snapshot and the narrowing, would find its repository held, add nothing, and be
		// narrowed out of what intake hears while the record names it.
		w.heard.Store(&heard)
		return nil
	}); err != nil {
		return err
	}
	// Third, the superset is narrowed to the snapshot's own set, again outside any transaction.
	// Every admission that committed before the snapshot is in it, on whatever repository it
	// resolved — the old mapping's, the new mapping's, or one no mapping names (R4) — since the
	// set holds every repository a record names. Every admission that commits after it reads the
	// set the second step held, so one on a repository outside that set tells intake of it at its
	// own commit, behind the mappingMu this read holds (hearRepository), and adds to the set this
	// call publishes instead of being narrowed away by it. Nothing recorded is dropped from what
	// intake hears. A failure here leaves the superset heard, which is sound — it contains the new
	// mapping's repositories and every repository the snapshot read — and is reported, so the
	// refresher reads again, with no limit (refreshOnMappingChange), and narrows then. The set held
	// as heard is the snapshot's meanwhile, so a recording on a repository the snapshot read that
	// intake was not told of yet adds nothing; that read again publishes it, as it does a record's
	// repository whose own update JetStream refused (heardPending).
	//
	// It is skipped only when the first step ran and published this very set, where narrowing
	// would set the subscription to what it already is. Where the first step was skipped this one
	// always runs, whatever the served set did: a record written since the last refresh names a
	// repository of its own, so the heard set can have grown while the served set stood still.
	if !prepublished || !slices.Equal(superset, heard) {
		if err := w.updateSubscription(ctx, heard); err != nil {
			return err
		}
	}
	// The set the read published is the set the second step held as heard, and it answers a
	// record whose own update JetStream refused (heardPending): the snapshot read that record, so
	// the repository it names is in what was just published.
	w.heardPending.Store(false)
	w.log.Info("legion workflow repositories changed", "added", added, "removed", removed,
		"refused", refusalReasons(w.repositories.Refused()), "withoutWebhookApp", webhookGapReasons(w.repositories.WithoutWebhookApp()),
		"heard", repositoryNames(heard))
	return nil
}

// sameRepositories is whether next names exactly the repositories of current, whatever the order
// and however often a name repeats in next. Order is not the comparison because the two lists do
// not share one: the mapping answers its served repositories sorted by their lowercased
// <owner>/<name> (repositories.Mapped), while the list a read of the mapping builds keeps
// Dispatch's own order, with every repository kept on its last answer appended after it
// (refreshMapping). Repeats are not the comparison either: two mapping entries that name one
// repository are served as one (repositories.set), so next may hold a name current holds once
// twice.
//
// ghrepo.Repository is its owner and its name, so a value is the comparison and a difference of
// spelling is a difference. That is the safe way round for the caller: a spelling GitHub changed
// costs it the step it would have skipped, where treating two spellings as one would skip a step
// that had something to do.
func sameRepositories(current, next []ghrepo.Repository) bool {
	seen := make(map[ghrepo.Repository]bool, len(current))
	for _, repo := range current {
		seen[repo] = false
	}
	matched := 0
	for _, repo := range next {
		found, ok := seen[repo]
		if !ok {
			return false
		}
		if !found {
			seen[repo] = true
			matched++
		}
	}
	return matched == len(seen)
}

// updateSubscription has GitHub intake hear repositories: the one route the runtime reaches
// intake's SetRepositories by — the read of the mapping's three steps (refreshMapping) and the
// repository a record first names (hearRepository) both publish through it — so no caller
// escapes the stand-in a test puts in its place (setRepositories, which the boot leaves nil
// because the daemon's own answer is the consumers it opened). It is called under no transaction
// of the pool: the call reaches JetStream and, for a durable consumer that is missing, the pool
// itself (INFRA-3169).
func (w *workflowRuntime) updateSubscription(ctx context.Context, repositories []ghrepo.Repository) error {
	if w.setRepositories != nil {
		return w.setRepositories(ctx, repositories)
	}
	return w.consumers.SetRepositories(ctx, repositories)
}

// mappingChange is Dispatch's consumer's RepositoryMapping: the project's
// settings.repo_project.updated asks refreshOnMappingChange to read the mapping again
// (requestMappingRead), and its event is acknowledged at once. A read takes Dispatch and GitHub
// and can take longer than the event's acknowledgement window (AckWait), the bound intake puts on
// the mapping read a handler makes inline (readMapping), so a refresh run there would be cut off
// part way and never finish. The event's delivery is not what is at risk — intake reports a
// message in progress for as long as its handler runs — the refresh itself is, which is why it
// runs on its own.
func (w *workflowRuntime) mappingChange(context.Context) error {
	w.requestMappingRead()
	return nil
}

// requestMappingRead asks refreshOnMappingChange for a read of the mapping and is the request's
// number: a read that finishes having started after the request covers it (mappingApplied). A
// request made while one waits is covered by the same read, since a read takes the whole mapping.
func (w *workflowRuntime) requestMappingRead() uint64 {
	request := w.mappingRequested.Add(1)
	select {
	case w.mappingChanged <- struct{}{}:
	default:
	}
	return request
}

// refreshOnMappingChange reads the mapping (refreshMapping, which bounds each of its own parts) on
// each request, until ctx ends. A failed read is logged and tried again on the App mint's backoff,
// with no limit, until it applies: a read takes the whole mapping, so retrying loses no request,
// and a daemon stopped before one applies reads the mapping at boot. A finished read covers every
// request made before it started, and then recovers each repository the read stopped leaving out
// for a read GitHub did not answer (recoverRepositories). While such a read leaves a repository as
// it stands (awaitingRead: left out for now, for its installation read or its default-branch read
// past GitHub's rate limit, or served on its last answer, its installation read having failed), or
// a recovery has not committed yet, it reads the mapping again of its own on installReadRetry's
// backoff, 30 s doubling to 10 min, reset once nothing waits, so the repository is served, or
// confirmed, and its issues recovered, once GitHub answers, with no restart and no mapping change.
// While a held recovery has release waits to tell and time left to recoveryHoldBound
// (recoveryHoldLeft), the next wait is cut to that time, so the bound is checked when it falls, not
// up to a backoff later; such a cut wait does not move the backoff on. Once the bound has passed,
// a run that did not tell the waits (its listing or its notice failed) waits the backoff as any
// failed read does, so a failure past the bound never turns the refresher into a busy loop.
//
// It asks Dispatch of its GitHub App on each repository only a live record names
// (checkRecordedWebhookApps) first, as it starts, and then after each read that applies, over the
// live set that read took, once the read has marked its requests covered (mappingApplied) and its
// recovery is done, so the admissions and the recovery of THAT read do not wait on it. The check
// holds no lock across its reads of Dispatch, so no recording waits on it either. What does wait
// is a read requested while a check runs: both are the refresher's own goroutine
// (awaitMappingRead, readAnswered), so such a read begins once the check ends, at most
// ceil(n/mappingReads) × installReadAttempt later for n repositories.
func (w *workflowRuntime) refreshOnMappingChange(ctx context.Context) {
	after := w.recheckAfter
	if after == nil {
		after = time.After
	}
	w.checkRecordedWebhookApps(ctx)
	recheckIn := installReadRetry.Initial
	for {
		var recheck <-chan time.Time
		waiting := append(w.repositories.awaitingRead(), w.recoveringReasons()...)
		waiting = append(waiting, w.unreadReasons()...)
		if w.heardPending.Load() {
			waiting = append(waiting, "the repositories GitHub intake hears: the update a record's own repository asked for was refused")
		}
		wait, cut := recheckIn, false
		if len(waiting) == 0 {
			recheckIn = installReadRetry.Initial
		} else {
			if left, held := w.recoveryHoldLeft(); held && left > 0 && left < wait {
				wait, cut = left, true
			}
			recheck = after(wait)
		}
		select {
		case <-ctx.Done():
			return
		case <-w.mappingChanged:
		case <-recheck:
			w.log.Info("workflow: reading the repository mapping again for what a read GitHub did not answer (an installation read, or a default-branch read past GitHub's rate limit) left as it stands",
				"waiting", waiting, "waited", wait.String())
			if !cut {
				recheckIn = min(2*recheckIn, installReadRetry.Max)
			}
		}
		retry := appMintRetry.Initial
		for {
			covers := w.mappingRequested.Load()
			err := w.refreshMapping(ctx)
			if err == nil {
				w.mappingApplied.Store(covers)
				w.recoverRepositories(ctx)
				w.checkRecordedWebhookApps(ctx)
				break
			}
			if ctx.Err() != nil {
				return
			}
			w.log.Warn("workflow: the repository mapping read failed, so the daemon serves the mapping it last read; reading it again", "retryIn", retry.String(), "error", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(retry):
			}
			retry = min(2*retry, appMintRetry.Max)
		}
	}
}

// markRecovered adds to the repositories to recover (recovering) each repository before names, a
// refusal for a read GitHub did not answer (its installation read, or its default-branch read past
// GitHub's rate limit: readFailures) in the mapping as last read, that after, the same of the
// mapping just published, does not name: GitHub answered for it, or it is no longer mapped. A
// repository after names is dropped from the set: it is unread again, and the read that next stops
// leaving it out adds it back. It runs under the fact lock, with the publication, so every event
// applied after it sees the new mapping, and every one applied before it is in the listing the
// recovery reads afterwards.
func (w *workflowRuntime) markRecovered(before, after []refusedRepository) {
	unread := make(map[string]bool, len(after))
	for _, refusal := range after {
		unread[strings.ToLower(refusal.Repo)] = true
	}
	w.recoveringMu.Lock()
	defer w.recoveringMu.Unlock()
	for repo := range w.recovering {
		if unread[repo] {
			delete(w.recovering, repo)
			// The repository left the recovery: a later one is held on a clock of its own.
			w.recoveryHeldSince = time.Time{}
		}
	}
	for _, refusal := range before {
		repo := strings.ToLower(refusal.Repo)
		if unread[repo] {
			continue
		}
		if w.recovering == nil {
			w.recovering = map[string]bool{}
		}
		w.recovering[repo] = true
	}
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

// recoverRepositories recovers the repositories markRecovered and readAnswered named, after a
// read of the mapping that published them as no longer unread or a preparation GitHub answered
// for one no mapping names: one read of Dispatch's project listing, applied as the
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
// so a hold that flaps is told at the bound all the same; it stops when the recovery commits or a
// repository leaves the recovery (markRecovered). The repositories are let go once
// both commit; a failed one stays to recover, tried again on the refresher's backoff. A restart
// loses the set, and the boot's reconcile is that recovery; what a release left waiting is told to
// no one then.
//
// Every repository no mapping names whose reads GitHub has not answered (leaveUnread), or that
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
	// the only issue whose preparation reads GitHub for a repository no mapping names
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
// (prepareUnmapped), so the listing succeeded while GitHub answered nothing.
//
// It also lets go of each repository left unread that no preparation will ever read GitHub for
// again, with nothing fallen silent for it: one the listing holds no unrecorded todo issue on
// that admission would record (todo), its only such issue having left todo, or having lost its
// label and its parent, before a read answered, and one the mapping or the recorded set now
// answers for (lookup), which a preparation sends to the mapping's reads or
// reads no further (prepareUnrecorded). Only an answered read drops a marker otherwise
// (readAnswered), so for either kind the refresher would read the whole mapping and list the
// whole project on its backoff, up to every 10 min, for ever. An issue that comes back to todo
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
	// The lock order is recoveringMu first and the mapping's lock (repositories.mu) nested inside
	// it, which is sound because nothing holding the mapping's lock ever takes recoveringMu
	// (awaitRecovery says the same). These lookups are made before recoveringMu all the same, not
	// because the order requires it, but because what they read is each repository's source as
	// another pass left it, which is a fact about the mapping and not about this pass's set.
	readElsewhere := make(map[string]bool, len(names))
	for _, repo := range names {
		readElsewhere[repo] = w.repositories.lookup(repo).source != fromNeither
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
	// letGoAnsweredElsewhere is a repository such an issue is still todo on that Dispatch's
	// mapping, or a read GitHub answered earlier (recordRepository), now answers for (lookup): an
	// issue on it is prepared from that answer and asks GitHub nothing a marker could wait on.
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
// unread. It is called inside the apply's transaction, so it is noted before any read of the
// mapping can publish resolved again, under the fact lock, and so before that recovery's scope is
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

// leaveUnread records that GitHub did not answer the reads a preparation made for repo, a
// repository no mapping names (prepareUnmapped): the issue's apply says nothing and records
// nothing, as it does for a mapped repository whose installation read failed, and the refresher
// reads the repository again on installReadRetry's backoff (refreshOnMappingChange, which waits
// while unreadReasons names any) and recovers the issues it left from Dispatch's listing
// (recoverRepositories) once a read answers.
//
// The read is asked for only when nothing reads the repository again already: it was not left
// unread before, and it is not among the repositories to recover (recovering), which the
// refresher reads again of its own on the same backoff. The marker is dropped by an answer
// (readAnswered), which asks for a read of its own, or by a recovery that finds nothing left on
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
	w.log.Warn("workflow: GitHub did not answer for a repository an issue names that no mapping names, so the issue is left and the repository read again",
		"repo", repo.String(), "reason", reason)
}

// leaveForRecovery leaves repo, a repository no mapping names that a todo issue of the boot's
// listing resolves to, unread without a read of GitHub (prepareListed says why). It writes the
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
	w.log.Info("workflow: the boot's listing reads GitHub for no repository no mapping names, so the issues on this one are left for the recovery after the boot to read it",
		"repo", repo.String())
}

// unreadAtBoot is why the boot's listing leaves a repository unread (leaveForRecovery).
const unreadAtBoot = "GitHub has not been read for it yet: the boot's listing reads no repository no mapping names, and the recovery after the boot reads it"

// markUnread leaves repo unread with reason, and asks for a read of the mapping, which runs the
// recovery that reads it again (refreshOnMappingChange), unless something reads it again already:
// it was left unread before, or it is among the repositories to recover (recovering), which the
// refresher reads again of its own. It answers the reason the repository was left unread with
// before, and whether it was.
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
		w.requestMappingRead()
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
// (recovering) and a read of the mapping is asked for, which runs that recovery
// (refreshOnMappingChange), as markRecovered marks a mapped repository a read of the mapping
// stops leaving out. The issues the earlier preparations left said nothing and recorded nothing,
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
		// counting the repository recovered, and the read asked for below runs the pass that does.
		w.markLocked(key)
	}
	w.recoveringMu.Unlock()
	if waiting {
		w.requestMappingRead()
	}
}

// unreadReasons is every repository no mapping names whose reads GitHub has not answered, as the
// refresher logs what it waits on: while any is, it reads again of its own and recovers the
// issues each left.
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
// unread now: mapped and left out for a read GitHub did not answer (lookup), or no mapping names it
// and GitHub's reads for it went unanswered (the marker). A repository a pass has stopped waiting
// on since release_children read it unread (settleRecovery) is no longer either, and a wait held on
// it would be read by no recovery and told to no architect, so it is refused and the route falls
// back to the child-release retry.
//
// Both readings, the mapping's and the marker, are made under recoveringMu, the lock a pass takes
// its set and its waits under (recoverRepositories) and settleRecovery drops the marker under, so
// the answer and the filing are one step against them. A mapped repository is published served
// before a read of the mapping marks it to recover (refreshMapping, markRecovered), and the pass
// that recovers it runs after: a wait whose lookup ran before that publication is filed before the
// pass takes its set, and told by it, and one whose lookup runs after is refused. With the lookup
// made outside the lock, a call descheduled between the two read the repository unread, then
// filed the wait after the pass had taken its set, or after it had committed: no pass held the
// repository to recover any longer, so the child was told to no architect and recorded by
// nothing until the next boot. Nesting the mapping's lock (repositories.mu) inside this one is
// sound: nothing holding the mapping's lock ever takes recoveringMu, since the mapping calls out to
// nothing.
func (w *workflowRuntime) awaitRecovery(repo, tree, child, parent string) bool {
	w.recoveringMu.Lock()
	defer w.recoveringMu.Unlock()
	answer := w.repositories.lookup(repo)
	repo = strings.ToLower(repo)
	switch answer.source {
	case fromMapping:
		if !answer.unread {
			return false
		}
	case fromRecorded:
		return false
	default:
		if _, unread := w.unread[repo]; !unread {
			return false
		}
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
		"GitHub answered for %s again (its App installation read, or its default-branch read past GitHub's rate limit), and the daemon recovered its issues from Dispatch's listing, but these children a release_children released while it was unread are still not recorded",
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
// GitHub for such a repository, or such an issue does and the mapping or an earlier answer of
// GitHub's answers for it without that read. Repositories let go for the two are told in two
// notices, each with its own opening, written in one transaction.
//
// Each child is named with the status listing, Dispatch's own project listing of this pass, gives
// it, since that status is why nothing records it: a child a person moved out of todo is recorded
// again only once it is todo again. A child whose parent the workflow does not record is named
// with that parent instead, as tellReleaseWaits names it: a release_children files a child under
// its parent's unread repository when the parent is what waits (enterOnUnreadRepository), and the
// child, todo all along, is recorded only under a recorded parent, so the step is the parent's
// release first. A child on a repository Dispatch's mapping now leaves out is named with that
// refusal, which is what stops it: a release does nothing while it stands. Left out for good — a
// role App not installed — the step is the one refuseChild's own notice gives, the App installed
// or another repository set; left out for now, for a read GitHub did not answer, the mapping's own
// recovery records it once GitHub answers. A child no wait is left for is told nothing, and so is
// a repository whose waits are empty.
//
// It is the pair of tellReleaseWaits and not a second telling of its kind: the two share one
// body, which decides which children are still unrecorded, reads each one's parent and writes one
// notice per issue a child's notice is queued on, and differ in the reason's opening and in the line each child is named on
// alone. The architect hears child-release-stalled either way, so no reader of the notices learns
// a new kind.
func (w *workflowRuntime) tellLetGoWaits(ctx context.Context, dropped []letGo, listing []dispatch.IssueSummary) error {
	repos := map[letGoWhy][]string{}
	waits := map[letGoWhy]map[string]releaseWait{}
	// childRepo is the repository each child's wait was filed under, whose answer now names what
	// stops the child.
	childRepo := map[string]string{}
	for _, gone := range dropped {
		if len(gone.waits) == 0 {
			continue
		}
		repos[gone.why] = append(repos[gone.why], gone.repo)
		if waits[gone.why] == nil {
			waits[gone.why] = map[string]releaseWait{}
		}
		maps.Copy(waits[gone.why], gone.waits)
		for child := range gone.waits {
			childRepo[child] = gone.repo
		}
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
		letGoAnsweredElsewhere: "the daemon no longer waits for GitHub to answer for %s, because Dispatch's repository mapping, or a read GitHub answered for it at another issue's preparation, answers for it now; these children a release_children released while it was unread were never recorded",
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
		repo := childRepo[child]
		switch answer := w.repositories.lookup(repo); {
		case answer.source == fromMapping && answer.refusal != "" && answer.unread:
			return fmt.Sprintf("%s (%s in Dispatch now; Dispatch's mapping leaves %s out for now: %s; the daemon recovers the issues on it once GitHub answers, and records %s then if it is todo)",
				child, status, repo, answer.refusal, child)
		case answer.source == fromMapping && answer.refusal != "":
			return fmt.Sprintf("%s (%s in Dispatch now, and the daemon records no issue on %s: %s; install the App on it, or set a repository both Legion Apps are installed on, then release %s again)",
				child, status, repo, answer.refusal, child)
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
// The answer is read from the set lookup answered from (source), so it is one read of the sets.
// A repository the mapping answers for is unread when GitHub did not answer its installation
// read, or its default-branch read past GitHub's rate limit, which the mapping's own retry
// clears. One the recorded set answers for is never unread: GitHub answered for both its
// installations and its name, and an issue on it is recorded from that answer. One neither set
// names, never read or refused, since no refusal is kept (prepareUnmapped), is one lookup answers
// unread, as Legion holds no answer for it; here it is unread only while a preparation's reads
// went unanswered, or the boot's listing left it for the recovery to read, which is the marker
// leaveUnread and leaveForRecovery write and readAnswered drops, and which the refresher is
// reading GitHub for. A refused one is not, because nothing is reading GitHub
// for it and no recovery will record the child. So a child left by a refusal is answered as one
// the recovery will not record, and its architect is told to act rather than to wait.
func (w *workflowRuntime) repositoryUnread(resolved string) bool {
	switch answer := w.repositories.lookup(resolved); answer.source {
	case fromMapping:
		return answer.unread
	case fromRecorded:
		return false
	}
	w.recoveringMu.Lock()
	defer w.recoveringMu.Unlock()
	_, waiting := w.unread[strings.ToLower(resolved)]
	return waiting
}

// awaitMappingRead holds back the admission event of issue delivered at seq, on a repository the
// mapping did not serve as last read, until a read of the mapping that started after the event
// first came covers it: a repository mapped, or an App installed, just before the event is then
// served for it. The first call for an event (by issue key and the sequence it was delivered at,
// which a redelivery keeps whatever Dispatch has run to since) requests the read; until it is
// covered, every call answers an intake.RetryLater, so the event is delivered again after its
// delay, off its acknowledgement window; once covered, it answers nil, for the caller to look the
// repository up again.
func (w *workflowRuntime) awaitMappingRead(issue intake.DispatchIssue, seq int64) error {
	w.awaitingMu.Lock()
	defer w.awaitingMu.Unlock()
	request, asked := w.awaiting[issue.Key][seq]
	if !asked {
		if w.awaiting == nil {
			w.awaiting = map[string]map[int64]uint64{}
		}
		if w.awaiting[issue.Key] == nil {
			w.awaiting[issue.Key] = map[int64]uint64{}
		}
		request = w.requestMappingRead()
		w.awaiting[issue.Key][seq] = request
	}
	if w.mappingApplied.Load() >= request {
		return nil
	}
	return &intake.RetryLater{After: mappingReadWait,
		Err: fmt.Errorf("%s resolves to %s, which the repository mapping did not serve when last read: the mapping is being read again before the issue is admitted", issue.Key, issue.ResolvedRepo)}
}

// forgetHolds drops issue's holds (awaitMappingRead) for every event delivered at or below seq,
// its event prepared at that sequence at last: every event at or below it is reflected in what
// was prepared, and one still held is held again on its next delivery.
func (w *workflowRuntime) forgetHolds(issue string, seq int64) {
	w.awaitingMu.Lock()
	defer w.awaitingMu.Unlock()
	for held := range w.awaiting[issue] {
		if held <= seq {
			delete(w.awaiting[issue], held)
		}
	}
	if len(w.awaiting[issue]) == 0 {
		delete(w.awaiting, issue)
	}
}

// forgetIssue drops every hold of issue's events, its record written: no event of it is prepared
// as an admission any longer.
func (w *workflowRuntime) forgetIssue(issue string) {
	w.awaitingMu.Lock()
	defer w.awaitingMu.Unlock()
	delete(w.awaiting, issue)
}

// admissionBound is whether issue's event is one admission records an unrecorded issue from: in
// todo, handed to Legion or a child of a tree.
func admissionBound(issue intake.DispatchIssue) bool {
	return issue.Status == "todo" && (issue.HandedOver || issue.Parent != "")
}

// mappingReadWait is how long an admission event held back for a read of the mapping
// (awaitMappingRead) waits before it is tried again.
const mappingReadWait = time.Second

// heardAtBoot is what GitHub intake hears when the boot opens it: the served repositories of the
// mapping the boot read and every repository a record of the project names. It keeps that set and
// the repository caches' own (refreshCacheRepositories), so a record written after it widens both
// without reading the records again (hearRecorded).
func (w *workflowRuntime) heardAtBoot(ctx context.Context) ([]ghrepo.Repository, error) {
	var heard []ghrepo.Repository
	served := w.repositories.Accepted()
	err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		var err error
		if heard, err = w.heardRepositories(ctx, tx, served); err != nil {
			return err
		}
		return w.refreshCacheRepositories(ctx, tx, served)
	})
	if err != nil {
		return nil, err
	}
	w.mappingMu.Lock()
	defer w.mappingMu.Unlock()
	w.heard.Store(&heard)
	return heard, nil
}

// hearRecorded has GitHub intake hear the repository an issue has just been recorded on, and the
// runtime keep a repository cache of it, from the moment a record first names it: work in a
// repository no project maps gets the same event and cache service as work in a mapped one (R5),
// with no mapping change and no restart. The workflow calls it inside the transaction that writes
// the record (workflow.Config.HearRepository), and the consumer's filters change once that
// transaction has committed and never before (intake.OnCommit), so an admission rolled back
// subscribes to nothing. That is what keeps the stream call out of the transaction and out of
// the fact serializer as well (INFRA-3169): a commit hook runs after the commit returned the
// connection to the pool and released the advisory lock the serializer holds for the
// transaction's life (intake.ApplyFact, intake.SerializeFacts), so the call waits on JetStream
// holding neither. A write outside any commit scope — a test applying a record of its own —
// changes nothing here; the boot's own set covers it (heardAtBoot).
//
// A consumer update JetStream refuses leaves the record as it is and marks the heard set pending
// (heardPending), which the refresher's next read of the mapping publishes in full: nothing
// inside the transaction can fail on it, so no JetStream trouble rolls an admission back.
func (w *workflowRuntime) hearRecorded(ctx context.Context, recorded string) {
	if w.consumers == nil || recorded == "" || !intake.CommitScoped(ctx) {
		return
	}
	repo, err := ghrepo.Parse("GitHub's spelling of the repository an issue was recorded on", recorded)
	if err != nil {
		w.log.Warn("workflow: a record names a repository GitHub intake cannot hear", "repo", recorded, "error", err)
		return
	}
	intake.OnCommit(ctx, func() {
		w.cacheRepository(repo)
		if err := w.hearRepository(ctx, repo); err != nil {
			w.heardPending.Store(true)
			w.log.Warn("workflow: GitHub intake was not told of the repository an issue was just recorded on, so the next read of the mapping tells it",
				"repo", repo.String(), "error", err)
			w.requestMappingRead()
		}
	})
}

// hearRepository adds repo to what GitHub intake hears, through the one route the runtime
// reaches that call by (updateSubscription). A repository the held set already names (heard)
// costs one atomic load of the set and a comparison of owner and name, without case, against each
// repository in it: no lock, no allocation and no call. So the ordinary recording, on a
// repository the mapping serves or another record already named, waits on nothing and asks
// JetStream nothing. Taking mappingMu first, as this did before, made every recording wait behind
// a whole read of the mapping — its Dispatch listing, its installation reads and its reads of
// GitHub's names — and a recording runs on the Dispatch consumer's goroutine or in
// release_children's handler, so intake stood still for as long as the read ran.
//
// Answering from the held set without the lock is sound only because of where a read of the
// mapping holds its set: its second step holds the set its third step narrows to, under the fact
// lock (refreshMapping). Checking whatever set intake was last told, as the plain fast path
// would, is not sound: until the third step has run, that is the first step's superset, which
// still names a repository only the old mapping served, so a record on it committed after the
// second step's snapshot would find it held, add nothing, and be narrowed out of what intake
// hears while the record names it. Held at the second step, before it releases the fact lock that
// every recording's own transaction takes, the set any record committed after the snapshot reads
// is the narrowed one or a later one, so a repository it does not name sends the record down the
// slow path below: the ordering of the two commits decides, and no timing does.
//
// Adding a repository takes mappingMu, the lock a read of the mapping holds for its whole run,
// so the two never overwrite one another's set: the addition is made before such a read's
// superset or after its narrowing, never between them, and the set it adds to is the one that
// read last held. The held set is read again under the lock, since a read that ran while this
// call waited may hold the repository by then.
func (w *workflowRuntime) hearRepository(ctx context.Context, repo ghrepo.Repository) error {
	if heardNames(w.heard.Load(), repo) {
		return nil
	}
	w.mappingMu.Lock()
	defer w.mappingMu.Unlock()
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
	w.log.Info("legion workflow hears a repository a record names", "repo", repo.String(), "heard", repositoryNames(heard))
	return nil
}

// heardNames is whether held, a set GitHub intake was given (heard), names repo, compared without
// case as GitHub compares names. A nil set, before the boot has given intake one, names nothing.
func heardNames(held *[]ghrepo.Repository, repo ghrepo.Repository) bool {
	if held == nil {
		return false
	}
	return slices.ContainsFunc(*held, func(name ghrepo.Repository) bool {
		return strings.EqualFold(name.Owner(), repo.Owner()) && strings.EqualFold(name.Name(), repo.Name())
	})
}

// cacheRepositories is the repositories the runtime keeps a repository cache of, read at every
// orphan sweep and handed to it as a function that performs no I/O (sandbox.Options.Mapped): the
// repositories the mapping names and the repositories of the project's live issue records, which
// is where the work is (R5). A repository no mapping names gets its cache — one volume and one
// pod — from the moment a record names it, and loses it after its retention once no live record
// and no mapping names it. Before the first read of the records it is the mapping alone, which is
// what a daemon with no workflow serves.
func (w *workflowRuntime) cacheRepositories() []ghrepo.Repository {
	if cached := w.cacheRepos.Load(); cached != nil {
		return *cached
	}
	return w.repositories.Accepted()
}

// refreshCacheRepositories reads the cache set again: served, the repositories the mapping names
// now, and the repository of every live issue record of the project (record.Store.LiveIssues), as
// tx reads them. It runs where the daemon reads the served set — the boot and every read of the
// mapping, inside the transaction that publishes it — so a repository whose last issue is done
// leaves the set at the next read of the mapping and its cache retires after its retention; a
// repository a record first names joins it at the recording itself (cacheRepository).
//
// The same read gives the mapping the records' half of what the daemon serves
// (repositories.setLive), which legion state reports beside the mapping's (served): the two are
// one read of the records so the state names a repository only a record names for exactly as
// long as the runtime keeps its cache as a served repository's, and a daemon that boots over a
// live tree reports its repository from the records alone, before any event of it.
func (w *workflowRuntime) refreshCacheRepositories(ctx context.Context, tx pgx.Tx, served []ghrepo.Repository) error {
	issues, err := w.records.LiveIssues(ctx, tx, w.dispatchProject)
	if err != nil {
		return fmt.Errorf("read the repositories the project's live records name for the repository caches: %w", err)
	}
	cached := make(map[string]ghrepo.Repository, len(served)+len(issues))
	for _, repo := range served {
		cached[strings.ToLower(repo.String())] = repo
	}
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
			return err
		}
		key := strings.ToLower(repo.String())
		live[key] = repo
		if _, held := cached[key]; !held {
			cached[key] = repo
		}
	}
	w.repositories.setLive(live)
	w.cacheReposMu.Lock()
	defer w.cacheReposMu.Unlock()
	w.storeCacheRepositories(cached)
	return nil
}

// cacheRepository adds repo to the cache set, for a record that first names it: the sweep makes
// its cache at its next pass, with no read of the mapping and no restart. Two records committed
// at once run their commit hooks on two goroutines, so the read and the write are one step
// (cacheReposMu) and neither loses the other's repository. It adds repo to the repositories the
// live records name as well (repositories.addLive), so the state reports it from the same
// moment.
func (w *workflowRuntime) cacheRepository(repo ghrepo.Repository) {
	w.repositories.addLive(repo)
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
	repos := make([]ghrepo.Repository, 0, len(cached))
	for _, key := range slices.Sorted(maps.Keys(cached)) {
		repos = append(repos, cached[key])
	}
	w.cacheRepos.Store(&repos)
}

// heardRepositories is what GitHub intake hears: every repository of served, every repository a
// record of the project names, as tx reads the records, and every configured rename's partner of
// those (`repository_renames`), once each, sorted. A record is heard
// whatever its tree's state, since a root that lingers or closed comes back, by re-admission, a
// hand-over or an orphan's carry, on the repository it keeps, with no refresh between; a fact
// from a repository an issue is recorded on reaches only that issue (workflow's issueForBranch
// and ownPullRequest). A rename's partner is heard in both directions, so the consumer's
// FilterSubjects carry both subjects over a transfer: the events arrive under the name GitHub now
// has while the records keep the name they were written under, and neither side may be deaf.
// Each is spelled as GitHub spells it, which Envoy's subjects carry and NATS
// matches case-sensitively, whatever spelling a record keeps for its workspace (a backfilled
// record keeps its clone's): a served repository as GitHub names it (nameAccepted), any other
// as its record keeps GitHub's spelling (record.Issue.GitHubRepo), and a partner, for a name
// nothing else heard names, as the pair configures it, so a restart subscribes with no read of
// GitHub.
func (w *workflowRuntime) heardRepositories(ctx context.Context, tx pgx.Tx, served []ghrepo.Repository) ([]ghrepo.Repository, error) {
	heard := map[string]ghrepo.Repository{}
	for _, repo := range served {
		heard[strings.ToLower(repo.String())] = repo
	}
	issues, err := w.records.Issues(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("read the repositories the project's records name: %w", err)
	}
	for _, issue := range issues {
		if _, ok := heard[strings.ToLower(issue.GitHubRepo)]; ok {
			continue
		}
		repo, err := ghrepo.Parse("GitHub's spelling of the repository of "+issue.Key, issue.GitHubRepo)
		if err != nil {
			return nil, err
		}
		heard[strings.ToLower(repo.String())] = repo
	}
	// One pass over the configured pairs, not over everything heard: a deployment configures no
	// pair at all nearly always, and this runs at every boot and consumer re-open. A partner is
	// added only when nothing heard already names it, so the spelling GitHub itself gave — the
	// served repository's (nameAccepted) or a record's — always wins over the pair's, which NATS
	// would otherwise match against no published subject at all; the pair's spelling is used only
	// for a name nothing else names. Both sides are read before either is added, so neither
	// addition is seen by the other's test.
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
	repos := make([]ghrepo.Repository, 0, len(heard))
	for _, repo := range heard {
		repos = append(repos, repo)
	}
	slices.SortFunc(repos, func(a, b ghrepo.Repository) int {
		return strings.Compare(strings.ToLower(a.String()), strings.ToLower(b.String()))
	})
	return repos, nil
}

// issueRepository is the repository issue's record names and its recorded default branch: what
// every launch of the issue's claims works (specs.repository) and what its commit identity is
// read from (adoptIdentity). The identity is not minted for this repository's own owner: identity
// maps it through serving first, so a tree recorded before a transfer is authored by the App on
// the installation that holds the repository now. An issue the workflow does not record, such as
// an operator's claim,
// works the project's default repository, as Dispatch resolves an issue that names none, at its
// default branch as GitHub names it (defaultBranches): a pod provisions its workspace from one, so
// a launch on none would be refused. A project that serves no default repository refuses that
// launch, naming the fix.
func (w *workflowRuntime) issueRepository(ctx context.Context, key string) (ghrepo.Repository, string, error) {
	issue, err := w.recordOf(ctx, key)
	if err != nil {
		return ghrepo.Repository{}, "", err
	}
	if issue == nil {
		for _, mapped := range w.repositories.Mapped() {
			if mapped.Default {
				read, err := w.defaultBranches.Named(ctx, mapped.Repo)
				if err != nil {
					return ghrepo.Repository{}, "", fmt.Errorf("the default repository of project %s, which %s works since it is not recorded: %w", w.dispatchProject, key, err)
				}
				return read.Name, read.DefaultBranch, nil
			}
		}
		return ghrepo.Repository{}, "", fmt.Errorf("%s is not recorded, so it works the project's default repository, and project %s serves none: set the project's default repository in Dispatch (Settings → Repositories → Projects) to one both GitHub Apps are installed on",
			key, w.dispatchProject)
	}
	repo, err := issue.Repository()
	if err != nil {
		return ghrepo.Repository{}, "", err
	}
	return repo, issue.DefaultBranch, nil
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
// issue detail, a read that fails leaving the event to be retried; then, for an event in todo, what
// the workflow reads in memory when it records the issue (ResolveRepository) is readied: an issue
// coming up for admission (handed to Legion, or a child of a tree) on a repository the mapping did
// not serve when last read waits for a read of the mapping that covers it (awaitMappingRead), so a
// repository just mapped, or an App just installed, is served for it, and a served repository's
// default branch is read when the read Legion holds is older than its TTL. An unrecorded issue's
// event in any other status asks GitHub nothing.
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
// read per such event, outside the fact lock as the rest of the preparation reads, and a failed
// read leaves the event to be retried. A recorded root a person moved back to triage is never
// queued again, so its log is not read.
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
		prepared, err := w.prepareUnrecorded(ctx, issue, false, 0, false)
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
	prepared, err := w.prepareUnrecorded(ctx, issue, true, delivered, false)
	if err != nil {
		return prepared, err
	}
	if prepared, err = w.withAncestry(ctx, prepared, nil, observe); err != nil {
		return prepared, err
	}
	w.forgetHolds(issue.Key, prepared.Seq)
	return prepared, nil
}

// labelSetter is the kind of the Dispatch actor that added the label to key, read from the issue's
// whole Dispatch event log, from sequence 0, by intake.LabelSetter's rule: one paged read per call,
// outside the fact lock as the rest of a preparation reads, for an event's preparation
// (prepareIssue) and the boot listing's (reconcileListing) alike. A read that fails, or a log the
// daemon cannot decode, is an error naming the key, for the caller to fail the preparation on, so
// the event or the listing is read again rather than applied with a setter nobody read: a setter
// of "" would keep today's triage wake for a label a person set.
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
func (w *workflowRuntime) prepareArtifact(ctx context.Context, fact intake.DispatchArtifact) (intake.DispatchArtifact, error) {
	if !fact.PersonApproval() {
		return fact, nil
	}
	issue, err := w.dispatch.GetIssue(ctx, fact.Key)
	if err != nil {
		return fact, fmt.Errorf("read %s from Dispatch before applying a person's approval of its document %s: %w", fact.Key, fact.ArtifactID, err)
	}
	fact.Issue = &issue
	return fact, nil
}

// prepareListed readies a todo issue of a listing — the boot's (reconcile, boot set) or a
// recovery's (recoverRepositories) — as prepareRepository readies an event's, on the mapping the
// daemon has just read and from the listing it has just read: an issue records holds a record of
// takes that record's repository, and an unrecorded one's served repository's default branch is
// read, with no hold for a read of the mapping and no read of Dispatch's view. Its Dispatch chain
// is read up walk, the one cache over the listing (listingAncestry), and no other root's is read
// again: the listing holds every queued root's own todo.
//
// The boot's listing reads GitHub for no repository no mapping names. It runs inside the boot's
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
		prepared, err := w.prepareUnrecorded(ctx, issue, false, 0, boot)
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
// listing's: no event of it is held as an admission any longer (forgetIssue), and one that names no
// repository (ResolvedRepoMissing) takes the record's.
func (w *workflowRuntime) prepareRecorded(issue intake.DispatchIssue, recorded *record.Issue) intake.DispatchIssue {
	w.forgetIssue(issue.Key)
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
// prepareRepository says. A repository Dispatch's mapping names is readied as it always was:
// an admission on one the mapping does not serve is held back for a read of the mapping, under
// the sequence the event was delivered at (held), when await is set, so a repository just mapped,
// or an App just installed, is served for it. An admission on a repository left out for a read
// GitHub did not answer (its installation read, or its default-branch read past GitHub's
// rate limit) is not held back: GitHub is being read again for it already, so the event is
// returned to be applied and acknowledged, its apply says nothing and keeps nothing
// (workflow.Resolution.Unread), and the issue is taken from Dispatch's listing once GitHub
// answers for the repository (recoverRepositories). A served repository kept on its last answer
// (repositories.kept) is read from GitHub no further: its default branch is the last read of it,
// so an admission on it waits for no GitHub read that is failing. A read of the
// default branch GitHub refuses for its rate limit, of a repository the daemon holds no read
// of (defaultBranches.Named answers every other from what it holds), is an intake.RetryLater
// after the wait GitHub asked for (defaultBranchLimit), so the event is delivered again once
// the limit has passed, not every second into it, each delivery reading GitHub again.
//
// A repository no mapping names is read from GitHub here instead (prepareUnmapped), and waits for
// no read of the mapping: a read of the mapping would answer nothing about it, since the mapping
// does not name it and never will (R4). One whose reads GitHub already answered installed is
// read no further: the recorded set holds GitHub's spelling and its default branch
// (repositories.recordRepository), so one repository is read once however many issues name it.
// Only an issue admission would record reads it (admissionBound): admission records an
// unrecorded issue only from a todo handed to Legion or released under a parent (admission's
// putNewRoot, the engine's recordChild), so a person's own todo issue, whose apply never
// resolves its repository, reads GitHub for nothing and leaves nothing unread, since nothing
// would ever recover it. In the boot's listing (boot) no issue reads it, and the repository is
// left for the recovery to read (prepareListed says why).
//
// The kind of repository is the source of the one lookup the rest of the preparation reads
// (lookup), and the lookup a held admission makes again once a read of the mapping covers it is
// read the same way: a read that stopped mapping the repository leaves it one GitHub is read for
// here, rather than one whose apply says nothing while nothing reads GitHub for it.
func (w *workflowRuntime) prepareUnrecorded(ctx context.Context, issue intake.DispatchIssue, await bool, held int64, boot bool) (intake.DispatchIssue, error) {
	if issue.ResolvedRepoMissing {
		detail, err := w.dispatch.GetIssue(ctx, issue.Key)
		if err != nil {
			return issue, fmt.Errorf("read the repository Dispatch resolves for %s, which its event %d predates: %w", issue.Key, issue.Seq, err)
		}
		issue.ResolvedRepo, issue.ResolvedRepoMissing = detail.ResolvedRepo, false
	}
	if issue.Status != "todo" || issue.ResolvedRepo == "" {
		return issue, nil
	}
	answer := w.repositories.lookup(issue.ResolvedRepo)
	if answer.source == fromMapping && answer.refusal != "" && !answer.unread && await && admissionBound(issue) {
		if err := w.awaitMappingRead(issue, held); err != nil {
			return issue, err
		}
		answer = w.repositories.lookup(issue.ResolvedRepo)
	}
	switch answer.source {
	case fromNeither:
		if !admissionBound(issue) {
			return issue, nil
		}
		return w.prepareUnmapped(ctx, issue, boot)
	case fromRecorded:
		return issue, nil
	}
	if answer.refusal != "" {
		return issue, nil
	}
	mapped := answer.mapped
	if w.repositories.kept(mapped.Repo.String()) {
		if _, read := w.defaultBranches.Kept(mapped.Repo.String()); read {
			return issue, nil
		}
	}
	_, err := w.defaultBranches.Named(ctx, mapped.Repo)
	if _, wait, limited := defaultBranchLimit(err); limited {
		return issue, &intake.RetryLater{After: wait, Err: fmt.Errorf("prepare %s on %s: GitHub's rate limit, retry in %s: %w", issue.Key, mapped.Repo, wait, err)}
	}
	return issue, err
}

// prepareUnmapped readies the event of an issue the workflow does not record whose repository no
// mapping names: the reads R4 asks for, made here because the workflow's own resolution performs
// no I/O (ResolveRepository) and the mapping has nothing to say about this repository. What
// decides is GitHub, read now: each role App's installation on the repository
// (appauth.Installations.InstalledOn, per repository, since an owner's installation may cover
// only some of its repositories) and the repository's own name and default branch
// (defaultBranches.Named).
//
// It is called only for a repository neither set names and only for an issue admission would
// record (prepareUnrecorded), so it reads GitHub no second time for one whose read the recorded set
// keeps, nor for an issue nothing records. With boot set, for the boot's listing, it reads GitHub
// for nothing and leaves the repository to the recovery (leaveForRecovery; prepareListed says
// why). Every other one is read at each preparation, which is what keeping no refusal means: an
// operator who installs the App, or a person who corrects a misspelled repository, is taken at
// the next preparation of the issue, with no wait, no restart and no mapping change.
//
// The three answers. Both Apps installed and GitHub naming the repository: the read is kept and
// the issue is recorded and admitted on it. A role App that is not installed, a repository GitHub
// does not have, and a name that is no <owner>/<name> at all: the event carries the refusal to
// its own apply (intake.DispatchIssue.RepositoryRefusal), which says it on the issue and keeps
// nothing of it. A read GitHub did not answer: the repository is left unread (leaveUnread), as a
// mapped repository whose installation read failed stands, so the event is applied and
// acknowledged, its apply says nothing and records nothing, and the refresher's recovery takes
// the issue once a later read answers.
//
// Where GitHub named the repository, Dispatch is asked as well whether its GitHub App is installed
// on it (checkWebhookApp), bounded by installReadAttempt as each of the mapping's checks is, and
// under GitHub's name, which is the name the record keeps and legion state lists it under, and
// the answer is kept for the state (repositories.keepWebhookAnswer, WithoutWebhookApp): the
// repository is served from its first record, so its gap is named from that record on, and not
// from a later read of the mapping, which nothing may ask for. The answer refuses nothing, as it
// never does for a mapped repository: the issue is recorded whatever Dispatch says, and a check
// that failed is the gap's reason. It is asked once for each read the recorded set keeps, as the
// reads beside it are, and a restart's repositories are asked by the refresher from the live
// records (checkRecordedWebhookApps).
//
// GitHub may answer the read under a name other than the one asked: a repository GitHub itself
// renamed is answered with a 301 to the name it holds it under now, which the read follows,
// while both role Apps answer installed on the old name, since an installation is the owner's.
// The issue is recorded on GitHub's name and at GitHub's default branch, as R4 asks, and the
// read is kept under the asked name too (repositories.recordRepository): the apply of this very
// event, and every later preparation of an issue on it, ask under the name the issue resolves
// to, which is the one GitHub renamed away from.
func (w *workflowRuntime) prepareUnmapped(ctx context.Context, issue intake.DispatchIssue, boot bool) (intake.DispatchIssue, error) {
	if w.installs == nil {
		// A daemon with no GitHub Apps reads no installation, so nothing is recorded on a
		// repository its mapping does not name: the issue's apply leaves it as an unread one.
		return issue, nil
	}
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
	for _, role := range appauth.Roles {
		read, cancel := context.WithTimeout(ctx, installReadAttempt)
		installed, err := w.installs.InstalledOn(read, role, repo)
		cancel()
		if err != nil {
			w.leaveUnread(repo, fmt.Sprintf("the %s App's installation on %s could not be read: %v; Legion reads it again", role, repo, err))
			return issue, nil
		}
		if !installed {
			w.readAnswered(repo)
			issue.RepositoryRefusal = fmt.Sprintf("the %s App is not installed on %s", role, repo)
			return issue, nil
		}
	}
	read, err := w.defaultBranches.Named(ctx, repo)
	if err != nil {
		w.leaveUnread(repo, fmt.Sprintf("the name and default branch of %s could not be read: %v; Legion reads it again", repo, err))
		return issue, nil
	}
	check, cancel := context.WithTimeout(ctx, installReadAttempt)
	gap := checkWebhookApp(check, w.dispatch, read.Name)
	cancel()
	// A check ctx's end cut short answered for the stop, not for Dispatch, so it keeps nothing.
	if ctx.Err() == nil {
		w.repositories.keepWebhookAnswer(read.Name, gap)
	}
	// The read is kept before the marker is dropped: readAnswered starts the recovery of the
	// issues the repository left, and each of those preparations is to find the read here rather
	// than read GitHub again, which could fail and leave the repository unread once more.
	w.repositories.recordRepository(repo.String(), read)
	w.readAnswered(repo)
	return issue, nil
}

// ResolveRepository answers the workflow (workflow.Repositories) from memory: the repository
// resolved names as GitHub names it, at the default branch last read for it, or why Legion does
// not record an issue on it. The mapping answers first and the recorded set second (lookup), and
// a repository the recorded set answers for is answered with the read kept for it, which is the
// one GitHub answer made for it however many issues name it. A served repository whose default
// branch was never read is an error: its event's preparation (prepareRepository) did not run.
//
// A repository GitHub renamed, which the issue resolves to under the name GitHub renamed it away
// from, is answered here under that old name: the recorded set holds the read under it as well
// as under GitHub's own name (repositories.recordRepository), and the answer carries GitHub's
// name and default branch, which is what the record keeps.
//
// A mapped repository is answered at the default branch last read for it (defaultBranches.Kept),
// never from the recorded set, even where that set holds a read kept from before the mapping
// named it: that read is the one GitHub made at its first preparation, which no later read of
// the default branch ever replaces, while Kept's TTL exists so a branch renamed on GitHub is
// taken. lookup's source says which of the two answered, and lookup reads the mapping under both
// names of a configured pair before it reads the recorded set, so a repository whose mapped name
// a configured pair moved is not answered under the name it moved away from.
func (w *workflowRuntime) ResolveRepository(resolved string) (workflow.Resolution, error) {
	answer := w.repositories.lookup(resolved)
	if answer.refusal != "" {
		return workflow.Resolution{Refusal: answer.refusal, LeftOut: answer.leftOut, Unread: answer.unread}, nil
	}
	read := answer.read
	if answer.source == fromMapping {
		var ok bool
		if read, ok = w.defaultBranches.Kept(answer.mapped.Repo.String()); !ok {
			return workflow.Resolution{}, fmt.Errorf("the default branch of %s was never read before an issue on it was recorded", answer.mapped.Repo)
		}
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

func refusalReasons(refused []refusedRepository) []string {
	reasons := make([]string, 0, len(refused))
	for _, refusal := range refused {
		reasons = append(reasons, refusal.Reason)
	}
	return reasons
}

// webhookGapReasons is each of gaps as "<repo>: <reason>", as the boot and refresh log them.
func webhookGapReasons(gaps []webhookAppGap) []string {
	reasons := make([]string, 0, len(gaps))
	for _, gap := range gaps {
		reasons = append(reasons, gap.Repo+": "+gap.Reason)
	}
	return reasons
}
