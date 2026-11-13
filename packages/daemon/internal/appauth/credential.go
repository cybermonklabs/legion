package appauth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// ErrOwnerOutsideProject is ControllerCredential's refusal of a controller asked for a default
// owner whose directory the review App's bundle does not hold: an owner that App is installed
// on none of, and that no configured rename moved a repository away from.
var ErrOwnerOutsideProject = errors.New("is no owner the controller's App is installed on")

// GitHubCredential is the one function every role's gh files come from (runtime.GitHubCredential)
// over tokens: a bundle whose directories (GitHubDirs, built from the same description) each hold
// one lease of the role's App, rendered as gh's hosts.yml and config.yml and named for the App and
// its expiry so a log can say whose token it wrote. A directory is an owner account and nothing
// smaller, because a lease is: every token this package mints reaches every repository its
// installation holds and names no repository at all (INFRA-2543), so the installation is the
// whole of what a role reaches and the bundle holds one directory per owner the role's App is
// installed on.
//
// Every one of those directories holds the lease of the owner it is named for, minted at the
// role's own permissions — the implement App's write set, the review App's review set
// (writePermissions, Tokens.Token) — and no directory holds a narrower one. A role's token is
// its App's installation lease on one account: the account chose what that installation reaches
// when it installed the App, the daemon asks GitHub to narrow it no further, and there is
// therefore nothing narrower for a bundle to hold. So a role pushes to, opens a pull request in
// and reviews a pull request of every repository its App reaches, whichever owner's it is, and
// which repository its own issue names decides the bundle's default directory and nothing else
// (INFRA-2543 R2). A reading lease in another owner's directory, which this bundle held until
// that ruling, refused a role the work its App is installed for.
//
// The one directory whose lease is minted on another account is the one a configured rename puts
// in the bundle — an owner the App is installed on none of, which a resolving pair moved a
// repository away from: it holds the lease of the owner whose installation serves the moved
// repository, which is what a reader spelling the old name means (bundleDirs).
//
// A controller's bundle is the one bundle of reading leases, and is built from this same
// description (ControllerCredential, Tokens.ReadToken): the two callers differ in the lease they
// ask for and not at all in the directories they hold.
//
// installations is read at every bundle, GitHub asked each time (OwnerInstallations): the
// daemon keeps no owner set of its own and runs no timer, so an account that installs the App
// is reachable at the next bundle and one that removes it stops minting there. A read that
// fails fails the bundle, naming the App: a bundle built from a set the daemon could not read
// would narrow or widen a role's reach silently, and failing closed is this rule's whole point
// (dispatch://INFRA-3136). What it read is recorded in installed for the drift check to compare
// a pod against (Installed, GitHubDirs), which may call no GitHub of its own.
//
// Default is the issue's own owner's directory, which GH_CONFIG_DIR names, lowercased: the owner
// the daemon's record names, which over a configured transfer is still the one the repository
// moved from. A pod's GH_CONFIG_DIR is fixed at its launch, and the directory's name authorises
// nothing — the token in it does. A call that names no repository at all, which a tmux tree pane
// whose claim the daemon records no repository for makes, takes the first owner the mint answered
// for, sorted, as its default; its directories hold the role's own leases as any other call's do,
// since what a role may do is its App's installation and not what its claim records. An App
// installed nowhere at all is runtime.ErrNoGitHubOwner. Every controller bundle, the launch's and
// the API route's both, is asked for by owner instead (ControllerCredential).
//
// A lease GitHub refuses (ErrNarrowingRefused) costs that owner's directory alone: it is left
// out, the rest of the bundle is minted and served, and the directory is said once on log
// (mintBundle). The one directory that cannot be left out is the bundle's own default, which the
// caller named: that is ErrNarrowingRefused, naming the owner, since a bundle whose GH_CONFIG_DIR
// holds no token is no bundle to launch a role on, and a role that cannot act on its own issue's
// repository has nothing to do (bundleDefault).
//
// A directory left out is recorded in left (LeftOut), which GitHubDirs reads, so the drift check
// never asks a pod for a directory no mint produces.
//
// A Manager holds every lease and re-mints it as it nears its expiry, so a launch and every
// refresh ask GitHub only when a lease turns over, and one lease is minted per scope however many
// directories carry it: a configured rename's two owner directories hold the one lease.
func GitHubCredential(tokens Tokens, installations OwnerInstallations, repos func() []ghrepo.Repository,
	renames ghrepo.Renames, installed *Installed, left *LeftOut, log *slog.Logger) runtime.GitHubCredential {
	return func(ctx context.Context, role claim.Role, repo ghrepo.Repository) (ghconfig.Bundle, error) {
		appRole := AppRoleFor(role)
		owners, err := installations.InstalledOwners(ctx, appRole)
		if err != nil {
			return ghconfig.Bundle{}, fmt.Errorf("read the owners the %s App is installed on for %s: %w", appRole, role, err)
		}
		installed.record(appRole, owners)
		layout, err := bundleDirs(owners, repos, renames, repo)
		if err != nil {
			return ghconfig.Bundle{}, err
		}
		if len(layout.owners) == 0 {
			return ghconfig.Bundle{}, fmt.Errorf("the %s App for %s: %w", appRole, role, runtime.ErrNoGitHubOwner)
		}
		return mintBundle(ctx, tokens, appRole, role, false, strings.ToLower(repo.Owner()), layout, left, log)
	}
}

// Bundles holds the gh bundles one walk of a runtime has built, so the walk builds each bundle
// once: a runtime makes one per walk and drops it at the walk's end (sandbox's
// refreshGitHubCredentials and gitHubBundles, tmux's refreshGitHubCredentials).
//
// It is worth having because a bundle is a function of two things and no more: the App role the
// claim role runs as (AppRoleFor) and the repository it is built for. The six roles of an issue
// pod are therefore two bundles, where each build opens with a live read of every page of its
// App's installations under a fresh JWT (GitHubCredential) — the walk of a twenty-pod project
// made 120 of those reads a minute where 40 answer every bundle it needs, and a pod's launch
// made six serial round trips to GitHub before its first role could start.
//
// It is a memo of one walk and no cache: every walk reads GitHub for what its bundles hold, as
// INFRA-2543 R2 asks, and what stops is one walk asking the same question four times. A build
// that fails is not kept either, so the walk's next role of that App asks again and fails
// naming its own role, exactly as it did when every role built its own.
//
// A controller's bundle is asked for by owner (ControllerCredential) and belongs here not at
// all: a controller pod and a controller pane hold the one role.
type Bundles struct {
	credential runtime.GitHubCredential
	built      map[bundleKey]ghconfig.Bundle
}

// bundleKey is what one bundle of a walk is held under: the App role it is minted as and the
// repository it is built for, lowercased as GitHub compares a repository's two halves.
type bundleKey struct {
	app  AppRole
	repo string
}

// NewBundles is one walk's memo over credential, the runtime's own bundle function
// (sandbox.Options.GitHubCredential, tmux.Options.GitHubCredential).
func NewBundles(credential runtime.GitHubCredential) *Bundles {
	return &Bundles{credential: credential, built: map[bundleKey]ghconfig.Bundle{}}
}

// Bundle is role's gh bundle for repo, built once per App role and repository of the walk. A
// walk is one goroutine, so the memo needs no lock of its own.
func (b *Bundles) Bundle(ctx context.Context, role claim.Role, repo ghrepo.Repository) (ghconfig.Bundle, error) {
	key := bundleKey{app: AppRoleFor(role), repo: strings.ToLower(repo.String())}
	if built, held := b.built[key]; held {
		return built, nil
	}
	built, err := b.credential(ctx, role, repo)
	if err != nil {
		return ghconfig.Bundle{}, err
	}
	b.built[key] = built
	return built, nil
}

// ControllerCredential is the controller's own bundle, and the one producer of it: a controller's
// launch asks for it, and the API's controller route serves `legion controller start` from it
// (internal/api's controllerGitHubCredential). It is the review App's read-only lease for every
// owner that App is installed on, each reaching every repository its installation holds, since a
// controller writes to none and reads them all: it is the one bundle whose directories hold a
// lease narrower than the App's own installation (Tokens.ReadToken, where a role's bundle holds
// Tokens.Token in every directory), and it holds exactly the directories a role's bundle holds
// for the same App (bundleDirs). Its default is the owner it is asked for — the
// one its launch recorded, which its GH_CONFIG_DIR names, and which over a configured rename may
// be an alias owner — or the first owner the mint answered for, sorted, when it is asked for none.
//
// An owner the bundle holds no directory for is refused (ErrOwnerOutsideProject) before anything
// is minted: the controller asks for its default over the API, and a default is no authority to
// read an owner the review App is not installed on. That refusal is what bounds the owner this
// route may be asked for, as the directories of a role's bundle bound `legion gh`. An App
// installed nowhere at all is runtime.ErrNoGitHubOwner, which the route answers as a daemon with
// nothing to act as. An owner whose lease GitHub refuses is left out of the bundle, as it is of a
// role's, and recorded in left (mintBundle, LeftOut); the one it is asked for cannot be left out,
// and is ErrNarrowingRefused naming that owner, since the controller's GH_CONFIG_DIR names that
// directory and nothing else.
func ControllerCredential(tokens Tokens, installations OwnerInstallations, repos func() []ghrepo.Repository,
	renames ghrepo.Renames, installed *Installed, left *LeftOut, log *slog.Logger) runtime.ControllerCredential {
	return func(ctx context.Context, defaultOwner string) (ghconfig.Bundle, error) {
		appRole := AppRoleFor(claim.RoleController)
		owners, err := installations.InstalledOwners(ctx, appRole)
		if err != nil {
			return ghconfig.Bundle{}, fmt.Errorf("read the owners the %s App is installed on for %s: %w", appRole, claim.RoleController, err)
		}
		installed.record(appRole, owners)
		layout, err := bundleDirs(owners, repos, renames, ghrepo.Repository{})
		if err != nil {
			return ghconfig.Bundle{}, err
		}
		owner := strings.ToLower(defaultOwner)
		if owner != "" && !layout.owners[owner] {
			return ghconfig.Bundle{}, fmt.Errorf("the controller's default owner %q %w", owner, ErrOwnerOutsideProject)
		}
		if len(layout.owners) == 0 {
			return ghconfig.Bundle{}, fmt.Errorf("the %s App for %s: %w", appRole, claim.RoleController, runtime.ErrNoGitHubOwner)
		}
		return mintBundle(ctx, tokens, appRole, claim.RoleController, true, owner, layout, left, log)
	}
}

// LeftOut is the daemon's record of the gh bundle directories a mint found GitHub refuses a lease
// for: one record per project, made by internal/daemon beside the Installed record and handed to
// GitHubCredential, ControllerCredential and GitHubDirs alike, so the set a pod's gh projection
// is compared against is the set a mint really produces.
//
// Without it an owner whose lease GitHub refuses costs the pod more than that owner:
// no bundle holds the directory, so no pod projects it, while the comparison names it at every
// sweep (sandbox's movedGhDirs), an update round asks every worker in the pod to push, the pod is
// replaced, and its new bundle lacks the directory again — for ever (INFRA-513).
//
// It is sticky, not timed: a directory stays recorded left out until a mint of it answers, which
// clears it, and the comparison then names it once and the pod is replaced once, which is right.
// It is kept per App role, and read for the App roles of the pod an answer is about alone
// (left, GitHubDirs): a pod's projection is its roles' directory sets intersected (sandbox's
// gitHubBundles), so a directory one of its own Apps does not reach is in no such pod's
// projection, however readily the other App mints it — while what the other App's mint found
// says nothing about a pod that does not run it. The project controller's pod runs the review
// App alone: were the implement App's record read for it too, a controller launched before an
// owner was served would never be found to lack that owner's directory, its gh would hold no
// token for an owner the review App reaches, and in a project with no live issue pod nothing
// would mint for the implement App to clear the record (INFRA-513).
//
// A nil record holds nothing and names nothing left out: the API's controller route builds a
// bundle of its own per request (internal/api's controllerGitHubCredential), and no pod's
// projection is ever compared against that one.
type LeftOut struct {
	mu   sync.Mutex
	dirs map[AppRole]map[string]bool
}

// NewLeftOut is a record of nothing: every directory the mapping names is served until a mint
// says GitHub refuses it.
func NewLeftOut() *LeftOut { return &LeftOut{dirs: map[AppRole]map[string]bool{}} }

// record holds what role's mint of the directory dir found: left, that GitHub refuses a lease for
// every repository it would hold, or, false, that the mint answered for it, which clears what an
// earlier mint recorded.
func (l *LeftOut) record(role AppRole, dir string, left bool) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !left {
		delete(l.dirs[role], dir)
		return
	}
	if l.dirs == nil {
		l.dirs = map[AppRole]map[string]bool{}
	}
	if l.dirs[role] == nil {
		l.dirs[role] = map[string]bool{}
	}
	l.dirs[role][dir] = true
}

// left reports that the last mint of dir as one of roles — the App roles of the pod the answer
// is for — left it out of that role's bundle, so no pod of those Apps created now projects it
// (GitHubDirs). A pod running one of them is told nothing by what the mints of an App it does
// not run found.
func (l *LeftOut) left(dir string, roles []AppRole) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, role := range roles {
		if l.dirs[role][dir] {
			return true
		}
	}
	return false
}

// Installed is the daemon's record of the owner accounts each App's last bundle build read from
// GitHub (OwnerInstallations): one record per project, made by internal/daemon beside the
// LeftOut record and handed to the same three, so the drift check has an owner set to compare a
// pod against without asking GitHub for one.
//
// It is what it says and no more: the installations as the last bundle build of that App read
// them, which may be a minute old and may have been read for another role of the same App.
// That is what an honest comparison a pod is replaced on may use, while every bundle reads
// GitHub itself: a pod is a thing of the last launch, so comparing it against the last read is
// the question the drift check asks, and the bundle a replacement pod is handed is read fresh
// whenever it is built. A role whose App no bundle has built for yet is recorded nothing, and
// GitHubDirs then names no directory at all and leaves every pod where it is, as it does for a
// bundle it cannot describe.
//
// A nil record records nothing and answers for nothing, as a nil LeftOut does: the API's
// controller route builds a bundle of its own per request (internal/api's
// controllerGitHubCredential), and no pod's projection is ever compared against that one.
type Installed struct {
	mu sync.Mutex
	// owners holds, per App role, the owner accounts that App's last bundle build read,
	// lowercased and sorted.
	owners map[AppRole][]string
}

// NewInstalled is a record of nothing: no App has been read yet, so no pod is compared against
// anything until the first bundle of its Apps is built.
func NewInstalled() *Installed { return &Installed{owners: map[AppRole][]string{}} }

// record holds the owners role's App was read installed on, replacing what an earlier bundle
// read: an account that removed the App is gone from the answer at once, as it is from the
// bundle that read it.
func (i *Installed) record(role AppRole, owners []string) {
	if i == nil {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.owners == nil {
		i.owners = map[AppRole][]string{}
	}
	i.owners[role] = slices.Clone(owners)
}

// Owners is the owner accounts role's App was installed on when its last bundle was built, and
// whether any bundle of it has been built at all: what internal/daemon tells a launch its own
// App reaches (daemon.RepositoriesClause), which may be a minute old and may have been read for
// another role of the same App, as this record's own doc says. It is no set to mint from — a
// bundle reads GitHub itself, at every build (GitHubCredential) — and the answer is the
// record's own slice copied, so a caller that sorts or appends to it moves nothing here.
func (i *Installed) Owners(role AppRole) ([]string, bool) {
	if i == nil {
		return nil, false
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	owners, read := i.owners[role]
	return slices.Clone(owners), read
}

// shared is the owners every one of roles' Apps was last read installed on — their
// intersection — and whether every one of them has been read at all. A pod's gh projection is
// its roles' bundles intersected (sandbox's gitHubBundles), so an owner one of its Apps is not
// installed on is in no such pod's projection however readily the other App mints it. A role
// nothing has been read for leaves the answer unknown: a set built without it would name a pod
// lacking for a directory an App that has never been read may well not hold.
func (i *Installed) shared(roles []AppRole) ([]string, bool) {
	if i == nil {
		return nil, false
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	var shared []string
	for at, role := range roles {
		owners, read := i.owners[role]
		if !read {
			return nil, false
		}
		if at == 0 {
			shared = slices.Clone(owners)
			continue
		}
		shared = slices.DeleteFunc(shared, func(owner string) bool { return !slices.Contains(owners, owner) })
	}
	return shared, true
}

// GitHubDirs is the directory names a gh bundle built now for repo would hold, sorted: the answer
// the Sandbox runtime's drift check reads a running pod's gh projection against
// (sandbox.Options.GitHubDirs), since a pod's projection is fixed at the directories of its
// creation. A zero repo is the controller's owner-only set, the bundle its pod was launched with.
//
// roles are the App roles of the pod that pod projection belongs to, which both records are read
// for: the owners those Apps were last read installed on, intersected (installed, Installed),
// less every directory a mint of those Apps found GitHub refuses (left, LeftOut). An issue pod
// runs both Apps and projects what both their bundles hold, so an owner either App is not
// installed on, and a directory either App is refused, is in no projection of it; the project
// controller's pod runs the review App alone and is handed every directory that App mints,
// whatever the implement App's installation reaches (sandbox's movedGhDirs, which maps its pod's
// kind's roles through AppRoleFor).
//
// It mints nothing and calls GitHub not at all — the drift check reads it on every evaluation of
// every pod, so a call to GitHub here would be one per pod per sweep — which is what the
// Installed record is for: a bundle reads the installations itself, and this answers from what
// the last bundle of those Apps read. Beside that it reads the same inputs a bundle does, the
// configured renames and the repositories Dispatch maps to the project, through the same one
// description (bundleDirs), so the set a pod is compared against and the set a pod would be
// handed cannot drift apart.
//
// Two answers are no set at all, and leave every pod where it is: an App no bundle has read the
// installations of yet (Installed.shared), and a bundle no launch could write — an owner
// directory ghconfig refuses a name for, which is what GitHubCredential fails on.
func GitHubDirs(repos func() []ghrepo.Repository, renames ghrepo.Renames, installed *Installed, left *LeftOut, roles []AppRole, repo ghrepo.Repository) []string {
	owners, read := installed.shared(roles)
	if !read {
		return nil
	}
	layout, err := bundleDirs(owners, repos, renames, repo)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(layout.dirs))
	for _, dir := range layout.dirs {
		if left.left(dir.name, roles) {
			continue
		}
		names = append(names, dir.name)
	}
	return names
}

// bundleDir is one directory of a gh bundle: name, its path under the bundle's root, and owner,
// the account whose installation mints the lease it holds. The two are the same name but for a
// configured rename's alias directory, which holds the lease of the owner that serves the
// repository the pair moved (bundleDirs).
type bundleDir struct {
	name  string
	owner string
}

// dirLease is the ask that mints one directory's lease: the owner whose installation mints it,
// and whether it is that installation's reading lease — a controller's bundle, which holds
// nothing else — or the App's own, which every directory of a role's bundle holds. Either is
// minted on one account's installation and reaches every repository that installation holds
// (INFRA-2543), which is why a directory names an owner and never a repository: there is no
// narrower lease for a narrower directory to hold.
type dirLease struct {
	read  bool
	owner string
}

// scope is what two directories hold the one lease under: the lease's kind and the owner whose
// installation mints it, as the Manager keys its cache (leaseScope), so the grouping here and
// the Manager's are the one rule. A configured rename's two owner directories share one scope.
func (d dirLease) scope(role AppRole) scope { return leaseScope(d.read, role, d.owner) }

// mint asks tokens for the lease, as the App role.
func (d dirLease) mint(ctx context.Context, tokens Tokens, role AppRole) (Lease, error) {
	if d.read {
		return tokens.ReadToken(ctx, role, d.owner)
	}
	return tokens.Token(ctx, role, d.owner)
}

// bundleLayout is a gh bundle as bundleDirs describes it: dirs, every directory it holds with
// the owner each one's lease is minted on, and owners, the same names as a set, which the
// bundle's default is chosen from and a controller's asked-for owner is read against.
type bundleLayout struct {
	dirs   []bundleDir
	owners map[string]bool
}

// bundleDirs is the one description of a gh bundle's directories: every directory it holds,
// sorted by name, with the owner whose installation mints the lease each one carries.
// GitHubCredential mints the role's own lease of those owners from it, ControllerCredential
// the reading lease of the same owners, and GitHubDirs names it, so no two of the three can
// hold a directory the others do not.
//
// It takes two inputs rather than one. installed is the owner accounts the role's App is
// installed on, which decides the directories: a lease reaches every repository its
// installation holds and names no repository at all (INFRA-2543), so the installation is the
// whole of a role's reach and there is nothing smaller for a directory to stand for. repos is
// the repositories Dispatch maps to the project, which the rename rule reads and nothing else
// does: a pair resolves on whether the project serves each of its two repositories, never on
// where an App is installed, which is the production failure the paragraph below records.
//
// The directories are one per installed owner, every owner a configured rename moved a
// repository away from (ghrepo.Renames.OwnerAliases) beside them, and repo's own recorded
// owner, which is the role's and not the project's. That last one is here because the bundle's
// default is that directory and the pod's GH_CONFIG_DIR names it (GitHubCredential): an owner
// no installation of the App answers for is then GitHub's own refusal of that mint, said where
// the ask was made, rather than a bundle quietly missing the directory it is served from.
//
// A rename's alias owner is no owner of its own where the App is installed on no such account:
// its directory's lease is minted for the owner the repository moved to, whose installation
// holds it now, which makes the pair's two directories one entry — the deployment's own case,
// where the bundle holds sjawhar for sjawhar/legion, which GitHub serves as
// cybermonklabs/legion. Where the App is installed on that account, the owner is one in its own
// right and its directory holds its own lease, since the pair moved one repository and not the
// account: that lease is the only one in the bundle that reaches the account's other
// repositories, while the repository the pair moved is reached through the directory of the
// owner whose installation holds it now — the name every GitHub call the daemon makes for such
// a tree already uses (internal/daemon's repositories.serving).
//
// The rename rule is asked one question of the served set — does the project serve this
// repository (ghrepo.Serves) — and never whether it serves any repository of a pair's owners.
// An owner test is the wrong question and was the production failure this rule replaces: a
// project that serves one more repository of the owner a pair moved a repository away from
// serves no more of the moved repository, yet an owner test reads that as the old owner being
// the project's own, stops resolving the pair, and mints every token of it on the installation
// the repository has left. Asked per repository, a pair resolves or does not on its own two
// names, so this is a pure function of its inputs and may be built per call.
//
// A directory ghconfig holds to be no directory's name (ghconfig.IsDirName) is an error: every
// directory here carries an owner's whole lease and may be the bundle's default, so a bundle is
// refused rather than written from a name that is not one.
func bundleDirs(installed []string, repos func() []ghrepo.Repository, renames ghrepo.Renames, repo ghrepo.Repository) (bundleLayout, error) {
	serves := repos()
	// The rename rule is read per repository: a pair resolves exactly when the project serves
	// its `to` and not its `from`, which is asked of the two repositories themselves.
	servesRepository := func(candidate ghrepo.Repository) bool { return ghrepo.Serves(serves, candidate) }
	aliases := renames.OwnerAliases(servesRepository)
	owners := make(map[string]bool, len(installed))
	for _, owner := range installed {
		owners[strings.ToLower(owner)] = true
	}
	layout := bundleLayout{owners: maps.Clone(owners)}
	for from := range aliases {
		layout.owners[from] = true
	}
	recorded := strings.ToLower(repo.Owner())
	if recorded != "" {
		layout.owners[recorded] = true
	}
	dirs := make([]bundleDir, 0, len(layout.owners))
	for _, name := range slices.Sorted(maps.Keys(layout.owners)) {
		// A directory holds the lease of the owner its own name means, which is what a `gh`
		// command or a git remote spelling that name asks for. The one name that means another
		// account is one no installation of the App answers for — an owner a resolving pair
		// moved a repository away from, which is the only reason such a name is in the set at
		// all: it means the repository the pair moved, GitHub redirects that to the owner it
		// moved to, and the directory therefore holds that owner's lease, which makes the
		// pair's two directories one entry.
		mint := name
		if alias := aliases[name]; !owners[name] && alias != "" {
			mint = alias
		}
		dirs = append(dirs, bundleDir{name: name, owner: mint})
	}
	for _, dir := range dirs {
		if !ghconfig.IsDirName(dir.name) {
			return bundleLayout{owners: layout.owners}, fmt.Errorf("the gh bundle directory %q is no lowercased GitHub login", dir.name)
		}
		layout.dirs = append(layout.dirs, dir)
	}
	return layout, nil
}

// leftOutDirectory is the key one line of a directory left out of a role's gh bundle is said
// under: the App role and the owner, lowercased as a scope's is.
type leftOutDirectory struct {
	role  AppRole
	owner string
}

// saidLeftOut holds every directory already said to be left out of a bundle, so the line is said
// once for the life of the process however many bundles are built after it, as a Manager says a
// grant shortfall once (sayShortfalls). It is the package's own, not a bundle's or a Manager's:
// the API builds a controller's bundle per request (internal/api's controllerGitHubCredential),
// and an owner GitHub refuses a lease for is refused at every one of them.
var (
	saidLeftOutMu sync.Mutex
	saidLeftOut   = map[leftOutDirectory]bool{}
)

// sayLeftOut says on log, at warn, that owner's directory is left out of role's gh bundle
// because GitHub refuses a lease on that installation, once per App role and owner for the
// life of the process (saidLeftOut): the role's other owners are served, and what its bundle
// does not hold is said once rather than at every launch and every refresh. It carries no
// token, a bundle's shortfall being about the ask. A caller that holds no logger of its own
// (sandbox.Options.Log is one) says it on slog.Default.
func sayLeftOut(log *slog.Logger, role AppRole, owner string) {
	key := leftOutDirectory{role: role, owner: strings.ToLower(strings.TrimSpace(owner))}
	saidLeftOutMu.Lock()
	said := saidLeftOut[key]
	saidLeftOut[key] = true
	saidLeftOutMu.Unlock()
	if said {
		return
	}
	if log == nil {
		log = slog.Default()
	}
	log.Warn("appauth: an owner's directory is left out of the role's gh bundle",
		"app", string(role), "owner", owner,
		"because", "GitHub refused a token on that installation: it does not hold the permissions the lease asks for")
}

// mintBundle is the bundle layout describes: one mint per scope, shared by every directory that
// holds it, each rendered as gh's two files, with dflt its default directory — the owner the
// caller named, or, for a caller that named none, the first owner the mint answered for, sorted
// (bundleDefault). read is the lease every directory of this bundle holds, which is the caller's
// and not the directory's: the reading one for a controller, the App's own for a role, whose
// every directory holds the lease of the owner it is named for (GitHubCredential).
//
// A mint GitHub refuses (ErrNarrowingRefused) costs that one owner's directory: it is left out,
// and every other directory is minted and served, where failing would cost the role every owner
// and the launch (INFRA-513). The one directory that cannot be left out is the bundle's default,
// which bundleDefault refuses the whole bundle for: a role that cannot act on its own issue's
// repository has nothing to do, and a bundle whose GH_CONFIG_DIR holds no token is no bundle to
// launch on. Every other failure fails the bundle, naming the role, the App and the owner the
// lease would have been minted on, and never the token: a refusal is about the ask.
//
// What was left out is said on log once the bundle stands and not as each mint answers
// (sayLeftOut): a line is said once for the life of the process, so a bundle that fails on its
// own default would otherwise spend that one line saying a directory was left out of a bundle
// nothing was served from.
//
// Every directory of the layout is recorded in left as this mint found it, minted or left out
// (LeftOut), so the drift check's own answer (GitHubDirs) names what a bundle built now holds and
// nothing more.
func mintBundle(ctx context.Context, tokens Tokens, appRole AppRole, role claim.Role, read bool, dflt string, layout bundleLayout, left *LeftOut, log *slog.Logger) (ghconfig.Bundle, error) {
	bundle := ghconfig.Bundle{Dirs: make(map[string]ghconfig.Rendered, len(layout.dirs))}
	minted := make(map[scope]ghconfig.Rendered, len(layout.dirs))
	refused := map[scope]bool{}
	var leftOut []string
	for _, dir := range layout.dirs {
		hold := dirLease{read: read, owner: dir.owner}
		key := hold.scope(appRole)
		entry, held := minted[key]
		if !held && !refused[key] {
			rendered, reached, err := dirEntry(ctx, tokens, appRole, hold)
			if err != nil {
				how := ""
				if read {
					how = "read-only "
				}
				return ghconfig.Bundle{}, fmt.Errorf("mint the %s%s App lease for %s on %s: %w", how, appRole, role, hold.owner, err)
			}
			if reached {
				entry, held = rendered, true
				minted[key] = rendered
			} else {
				refused[key] = true
			}
		}
		left.record(appRole, dir.name, !held)
		if !held {
			leftOut = append(leftOut, hold.owner)
			continue
		}
		bundle.Dirs[dir.name] = entry
	}
	dflt, err := bundleDefault(bundle, appRole, role, dflt, layout)
	if err != nil {
		return ghconfig.Bundle{}, err
	}
	for _, owner := range leftOut {
		sayLeftOut(log, appRole, owner)
	}
	bundle.Default = dflt
	return bundle, nil
}

// bundleDefault is the directory the bundle's GH_CONFIG_DIR names: dflt, the owner the caller
// asked for, or, where it asked for none, the first directory the mint answered for, sorted
// (layout.dirs comes sorted by name).
//
// An owner the caller named whose directory no mint answered for is ErrNarrowingRefused, naming
// the owner the lease would have been minted on: the bundle is served from that directory, so a
// bundle without it would be refused later as one whose default holds no gh files
// (ghconfig.Bundle.Validate), long after the ask that could say why. A caller that named none
// and whose every owner was refused is the same refusal over every owner at once.
func bundleDefault(bundle ghconfig.Bundle, appRole AppRole, role claim.Role, dflt string, layout bundleLayout) (string, error) {
	if dflt == "" {
		for _, dir := range layout.dirs {
			if _, held := bundle.Dirs[dir.name]; held {
				return dir.name, nil
			}
		}
		return "", fmt.Errorf("the %s App's gh bundle for %s holds no owner the App is installed on (%s): %w",
			appRole, role, strings.Join(slices.Sorted(maps.Keys(layout.owners)), ", "), ErrNarrowingRefused)
	}
	if _, held := bundle.Dirs[dflt]; held {
		return dflt, nil
	}
	reaches := dflt
	if at := slices.IndexFunc(layout.dirs, func(dir bundleDir) bool { return dir.name == dflt }); at >= 0 {
		reaches = layout.dirs[at].owner
	}
	return "", fmt.Errorf("the %s App's gh bundle for %s holds no directory for its default owner %s: the lease on %s: %w",
		appRole, role, dflt, reaches, ErrNarrowingRefused)
}

// dirEntry is the directory's lease, rendered as gh's two files; reached is false where GitHub
// refuses the lease, which leaves the directory out of the bundle (mintBundle).
//
// A refused lease is the owner's whole directory, since the lease is the owner's whole
// installation: there is nothing narrower to ask for and nothing to probe, so the directory is
// left out and the bundle's other owners are served, where failing would cost a launch every
// owner for one account's grant (INFRA-513). That is what an account that has not granted the
// App what the lease asks for makes of the ask. The one directory a refusal may not be
// answered this way for is the bundle's own default, which its caller is served from and which
// bundleDefault refuses the whole bundle for.
//
// No other failure is read as a refusal of the ask: GitHub's own trouble (TransientError, a
// rate-limited 422 among it) fails the bundle, so the caller asks again rather than record an
// owner unreachable on the strength of a rate limit (NarrowingRefused).
func dirEntry(ctx context.Context, tokens Tokens, appRole AppRole, hold dirLease) (ghconfig.Rendered, bool, error) {
	lease, err := hold.mint(ctx, tokens, appRole)
	switch {
	case err == nil:
		return ghconfig.Render(lease.Token, string(appRole), lease.ExpiresAt), true, nil
	case NarrowingRefused(err):
		return ghconfig.Rendered{}, false, nil
	}
	return ghconfig.Rendered{}, false, err
}
