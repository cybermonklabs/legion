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

// ErrDefaultOwnerNotInstalled is ControllerCredential's refusal of a controller asked for a
// default owner whose directory the review App's bundle does not hold: an owner that App is
// installed on none of, and that no configured rename moved a repository away from. A bundle's
// directories are the owners the App is installed on (bundleDirs), so the refusal says "not
// installed", and no repository the project works has a part in it. The API answers it as
// DEFAULT_OWNER_NOT_INSTALLED (internal/api's controllerGitHubCredential).
var ErrDefaultOwnerNotInstalled = errors.New("is no owner the controller's App is installed on")

// GitHubCredential is the one function every role's gh files come from (runtime.GitHubCredential)
// over source: a bundle whose directories (GitHubDirs, built from the same description) each hold
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
// Where the App is installed on both accounts of a configured pair, each of the two directories
// holds its own account's lease, since the pair moved one repository and not the account, and
// the repository it moved is reached by the routing the bundle's pairs carry: a request for the
// pair's `from` repository is answered from the `to` owner's directory, and every other
// repository of the `from` owner from its own (bundleDirs, ghconfig.Bundle.Pairs). That is this
// deployment's case, where the implement App's sjawhar installation holds 151 repositories and
// not cybermonklabs/legion.
//
// A controller's bundle is the one bundle of reading leases, and is built from this same
// description (ControllerCredential, Tokens.ReadToken): the two callers differ in the lease they
// ask for and not at all in the directories they hold.
//
// The owners the App is installed on are read at every bundle (source's InstalledOwners, through
// installedLayout): under the App's JWT GitHub is asked each time, and in token mode the read is
// the App's token_command's release of every installation, which may be that of a run that
// finished, with no caller waiting for it, up to finishedRunKeep before the bundle. The daemon
// keeps no owner set of its own, so an account that installs the App is reachable at the first
// bundle whose read began after the change, under the App's JWT the next one, and one that
// removes it stops minting there. A read that fails fails the bundle, naming the App: a bundle
// built from a set the daemon could not read would narrow or widen a role's reach silently, and
// failing closed is this rule's whole point (dispatch://INFRA-3136). What it read is recorded in
// installed for the drift check to compare a pod against (Installed, GitHubDirs), which may call
// no GitHub of its own.
//
// Default is the directory the issue's own repository routes to, which GH_CONFIG_DIR names: the
// one `legion gh` and `legion git-credential` answer that repository from (ghconfig.RouteDir,
// ghroute.DirFor), which is the recorded owner's own directory for every repository
// no configured pair moved, and the directory of the owner it moved to for the one a pair did.
// The recorded owner alone is the wrong answer over a transfer: where the App is installed on
// both accounts of the pair, the `from` owner's directory holds that owner's own lease, which
// cannot write the repository that moved, and a gh command run under GH_CONFIG_DIR alone would
// be refused at the remote. A pod's GH_CONFIG_DIR is fixed at its launch, and the directory's
// name authorises nothing — the token in it does. A call that names no repository at all, which
// a tmux tree pane whose claim the daemon records no repository for makes, takes the first owner
// the mint answered for, sorted, as its default; its directories hold the role's own leases as
// any other call's do, since what a role may do is its App's installation and not what its claim
// records. Such a call for an App installed on no account has no GitHub owner to mint for:
// runtime.ErrNoGitHubOwner. Every controller bundle, the launch's and the API route's both, is
// asked for by owner instead (ControllerCredential).
//
// A mint for any owner but the default directory's that GitHub answers definitively about that
// owner's installation — not installed, suspended, granted none of the role's permissions, the
// permissions refused (leaveOutReason) — costs that owner's directory alone: it is left out, the
// rest of the bundle is minted and served, and the directory is said on log naming the owner, the
// App and the reason (mintBundle). One such installation would otherwise cost every role of the
// App its gh and git within the hour its tokens last, for an owner the role's issue may never
// touch; leaving it out narrows a role's reach and never widens it. An owner whose name is no
// directory's is left out and said in the same way, with no mint asked of it (bundleDirs). Every
// other failure — GitHub's trouble, a rate limit — fails the bundle, which keeps the role's last
// tokens and is asked again a minute later. The one directory that cannot be left out is the
// bundle's own default, whose failure fails the bundle naming the owner: a bundle whose
// GH_CONFIG_DIR holds no token is no bundle to launch a role on, and a role that cannot act on
// its own issue's repository has nothing to do. The installation read is no mint, and its
// failure fails the bundle as said above.
//
// A directory left out is recorded in left (LeftOut), which GitHubDirs reads, so the drift check
// never asks a pod for a directory no mint produces.
//
// A Manager holds every lease and re-mints it as it nears its expiry, so a launch and every
// refresh ask GitHub only when a lease turns over, and one lease is minted per scope however many
// directories carry it: a configured rename's two owner directories hold the one lease.
func GitHubCredential(source Source, served func(ghrepo.Repository) bool, renames ghrepo.Renames,
	installed *Installed, left *LeftOut, log *slog.Logger) runtime.GitHubCredential {
	return func(ctx context.Context, role claim.Role, repo ghrepo.Repository) (ghconfig.Bundle, error) {
		appRole := AppRoleFor(role)
		layout, err := installedLayout(ctx, source, appRole, role, served, renames, installed, repo)
		if err != nil {
			return ghconfig.Bundle{}, err
		}
		return mintBundle(ctx, source, appRole, role, false, layout.dflt, layout, left, log)
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
// An owner the bundle holds no directory for is refused (ErrDefaultOwnerNotInstalled) before
// anything is minted: the controller asks for its default over the API, and a default is no
// authority to read an owner the review App is not installed on. That refusal is what bounds the
// owner this route may be asked for, as the directories of a role's bundle bound `legion gh`. A
// review App installed on no account leaves the controller no GitHub owner
// (runtime.ErrNoGitHubOwner), which the route answers as a daemon with nothing to act as, saying
// that the App is installed on no account. An owner whose lease cannot be minted is left out of
// the bundle, as it is of a role's, and recorded in left (mintBundle, LeftOut); the one it is
// asked for cannot be left out, and its failure fails the bundle naming that owner —
// ErrNarrowingRefused where GitHub refused the lease — since the controller's GH_CONFIG_DIR names
// that directory and nothing else. An owner whose name is no directory's is left out in the same
// way (bundleDirs), and fails the bundle in the same way where it is the owner asked for.
func ControllerCredential(source Source, served func(ghrepo.Repository) bool, renames ghrepo.Renames,
	installed *Installed, left *LeftOut, log *slog.Logger) runtime.ControllerCredential {
	return func(ctx context.Context, defaultOwner string) (ghconfig.Bundle, error) {
		appRole := AppRoleFor(claim.RoleController)
		layout, err := installedLayout(ctx, source, appRole, claim.RoleController, served, renames, installed, ghrepo.Repository{})
		if err != nil {
			return ghconfig.Bundle{}, err
		}
		owner := strings.ToLower(defaultOwner)
		if owner != "" && !layout.owners[owner] {
			return ghconfig.Bundle{}, fmt.Errorf("the controller's default owner %q %w", owner, ErrDefaultOwnerNotInstalled)
		}
		if owner != "" && slices.Contains(layout.refused, owner) {
			return ghconfig.Bundle{}, noDirectoryName(owner)
		}
		return mintBundle(ctx, source, appRole, claim.RoleController, true, owner, layout, left, log)
	}
}

// installedLayout is how both credentials open: it reads the owners appRole's App is installed
// on (InstalledOwners: GitHub asked under the App's JWT, the token_command's release of every
// installation in token mode), records them in installed for the drift check (Installed), and
// describes the bundle those owners make for repo (bundleDirs). A read that fails fails the
// bundle, naming the App and role, the role the bundle is for.
func installedLayout(ctx context.Context, installations OwnerInstallations, appRole AppRole, role claim.Role,
	served func(ghrepo.Repository) bool, renames ghrepo.Renames, installed *Installed, repo ghrepo.Repository) (bundleLayout, error) {
	owners, err := installations.InstalledOwners(ctx, appRole)
	if err != nil {
		return bundleLayout{}, fmt.Errorf("read the owners the %s App is installed on for %s: %w", appRole, role, err)
	}
	installed.record(appRole, owners)
	return bundleDirs(owners, served, renames, repo)
}

// LeftOut is the daemon's record of the gh bundle directories a mint could not fill, its lease
// refused or failed (mintBundle): one record per project, made by internal/daemon beside the
// Installed record and handed to GitHubCredential, ControllerCredential and GitHubDirs alike, so
// the set a pod's gh projection is compared against is the set a mint really produces.
//
// Without it an owner whose lease cannot be minted costs the pod more than that owner:
// no bundle holds the directory, so no pod projects it, while the comparison names it at every
// sweep (sandbox's movedGhDirs), an update round asks every worker in the pod to do the steps of
// its update message (supervise.UpdateTask), the pod is replaced, and its new bundle lacks the
// directory again — for ever (INFRA-513).
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

// NewLeftOut is a record of nothing: every directory is served until a mint of it fails.
func NewLeftOut() *LeftOut { return &LeftOut{dirs: map[AppRole]map[string]bool{}} }

// record holds what role's mint of the directory dir found: left, that the directory's lease
// could not be minted, or, false, that the mint answered for it, which clears what an earlier
// mint recorded.
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

// Installed is the daemon's record of the owner accounts each App was last read installed on
// (OwnerInstallations), kept as two reads apart: the read of that App's last bundle build
// (record), and the read the boot's proof of that App's key made (RecordProof). One record per
// project, made by internal/daemon beside the LeftOut record and handed to the same three, so
// the drift check has an owner set to compare a pod against without asking GitHub for one, and
// a launch's addressing has its App's owners from the first launch on.
//
// It is what it says and no more: the installations as the last bundle build of that App read
// them, which may be a minute old and may have been read for another role of the same App.
// That is what an honest comparison a pod is replaced on may use, while every bundle makes its
// own read (InstalledOwners): a pod is a thing of the last launch, so comparing it against the
// last read is the question the drift check asks, and the bundle a replacement pod is handed is
// built on the read made for it. The boot's read is no such comparison: no mint has run at boot,
// so nothing says which of its owners a bundle leaves out (LeftOut, which only a bundle's mint
// fills), and a set built from it would name an owner whose mint GitHub refuses and read every
// pod as lacking that owner's directory. So the drift check reads the bundles' reads alone: a
// role whose App no bundle has built for yet is recorded nothing there, and GitHubDirs then
// names no directory at all and leaves every pod where it is, as it does for a bundle it cannot
// describe. The boot's read stands in for a bundle's only where a launch's addressing asks
// (Owners, AddressingDirs) and no bundle of the App has been built yet.
//
// A nil record records nothing and answers for nothing, as a nil LeftOut does: the API's
// controller route builds a bundle of its own per request (internal/api's
// controllerGitHubCredential), and no pod's projection is ever compared against that one.
type Installed struct {
	mu sync.Mutex
	// owners holds, per App role, the owner accounts that App's last bundle build read,
	// lowercased and sorted (record).
	owners map[AppRole][]string
	// proved holds, per App role, the owner accounts the boot's proof of that App's key read, in
	// the same form (RecordProof).
	proved map[AppRole][]string
}

// NewInstalled is a record of nothing: no App has been read yet, so no pod is compared against
// anything until the first bundle of its Apps is built, whatever the boot's proof records in it.
func NewInstalled() *Installed {
	return &Installed{owners: map[AppRole][]string{}, proved: map[AppRole][]string{}}
}

// record holds the owners role's App was read installed on by a bundle's build
// (installedLayout), replacing what an earlier build's read held: an account that removed the
// App is gone from the answer at once, as it is from the bundle that read it. It is the one read
// a pod is compared against (GitHubDirs); the boot's read is RecordProof's.
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

// RecordProof holds the owners role's App was read installed on by the boot's proof of its key
// (internal/daemon's proveAppKeys), which reads the same installations a bundle does, so a
// launch's addressing has its App's owners from the first launch on (Owners, AddressingDirs). It
// is kept apart from a bundle's read and is never what a pod is compared against (GitHubDirs):
// no mint has run yet, so nothing says which of those owners a bundle leaves out.
func (i *Installed) RecordProof(role AppRole, owners []string) {
	if i == nil {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.proved == nil {
		i.proved = map[AppRole][]string{}
	}
	i.proved[role] = slices.Clone(owners)
}

// Owners is the owner accounts role's App was installed on when its last bundle was built, or,
// while no bundle of it has been built, when the boot proved its key (RecordProof), and whether
// either has read it at all: what internal/daemon tells a launch its own App reaches
// (daemon.RepositoriesClause), which may be a minute old and may have been read for another role
// of the same App, as this record's own doc says. It is no set to mint from — a bundle makes its
// own read at every build (GitHubCredential, InstalledOwners) — and the answer is the record's
// own slice copied, so a caller that sorts or appends to it moves nothing here.
func (i *Installed) Owners(role AppRole) ([]string, bool) {
	if i == nil {
		return nil, false
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	owners, read := i.lastRead(role, true)
	return slices.Clone(owners), read
}

// lastRead is the owners role's App was last read installed on, and whether it has been read: by
// its last bundle build, or, where proofs and no bundle of it has been built yet, by the boot's
// proof of its key. The caller holds mu.
func (i *Installed) lastRead(role AppRole, proofs bool) ([]string, bool) {
	if owners, built := i.owners[role]; built || !proofs {
		return owners, built
	}
	owners, proved := i.proved[role]
	return owners, proved
}

// shared is the owners every one of roles' Apps was last read installed on — their
// intersection — and whether every one of them has been read at all, by a bundle build or,
// where proofs, by the boot's proof of its key (lastRead). A pod's gh projection is its roles'
// bundles intersected (sandbox's gitHubBundles), so an owner one of its Apps is not installed on
// is in no such pod's projection however readily the other App mints it. A role nothing has been
// read for leaves the answer unknown: a set built without it would name a pod lacking for a
// directory an App that has never been read may well not hold.
func (i *Installed) shared(roles []AppRole, proofs bool) ([]string, bool) {
	if i == nil {
		return nil, false
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	var shared []string
	for at, role := range roles {
		owners, read := i.lastRead(role, proofs)
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
// for: the owners the last bundle of each of those Apps read installed, intersected (installed,
// Installed), less every directory a mint of those Apps could not fill (left, LeftOut). An issue
// pod runs both Apps and projects what both their bundles hold, so an owner either App is not
// installed on, and a directory either App left out, is in no projection of it; the project
// controller's pod runs the review App alone and is handed every directory that App mints,
// whatever the implement App's installation reaches (sandbox's movedGhDirs, which maps its pod's
// kind's roles through AppRoleFor).
//
// It mints nothing and calls GitHub not at all — the drift check reads it on every evaluation of
// every pod, so a call to GitHub here would be one per pod per sweep — which is what the
// Installed record is for: a bundle reads the installations itself, and this answers from what
// the last bundle of those Apps read. Beside that it reads the same inputs a bundle does, the
// configured renames and which repositories both role Apps answered installed on (served),
// through the same one description (bundleDirs), so the set a pod is compared against and the
// set a pod would be handed cannot drift apart.
//
// Two answers are no set at all, and leave every pod where it is: an App no bundle has read the
// installations of yet (Installed.shared), and a bundle no launch could write — a default
// directory ghconfig refuses a name for, which is what GitHubCredential fails on. An App the
// boot's proof of its key has read (Installed.RecordProof) is still the first: that read is no
// bundle's, and before any mint of the App nothing says which of its owners a bundle leaves out,
// so a set built from it would name an owner whose mint GitHub refuses (INFRA-513) and read every
// pod as lacking that owner's directory, sending an issue pod an update round and replacing the
// controller's pod at its relaunch. Any other owner ghconfig refuses a name for is in no bundle
// and so in no answer here (bundleDirs).
func GitHubDirs(served func(ghrepo.Repository) bool, renames ghrepo.Renames, installed *Installed, left *LeftOut, roles []AppRole, repo ghrepo.Repository) []string {
	return gitHubDirs(served, renames, installed, left, roles, repo, false)
}

// AddressingDirs is GitHubDirs as a launch's addressing asks it (internal/daemon's
// plan.appOwners): the same rule over the same description, but an App of roles no bundle has
// been built of yet is read as the boot's proof of its key read it (Installed.RecordProof), so a
// launch from the first one on is told the owners its container holds a directory for. No mint of
// such an App has left anything out yet (LeftOut), so its read stands for every directory its
// installations make; once a bundle of each App of roles is built, the answer is GitHubDirs' own.
// It is never what a pod is compared against, for the reason GitHubDirs gives.
func AddressingDirs(served func(ghrepo.Repository) bool, renames ghrepo.Renames, installed *Installed, left *LeftOut, roles []AppRole, repo ghrepo.Repository) []string {
	return gitHubDirs(served, renames, installed, left, roles, repo, true)
}

// gitHubDirs is GitHubDirs and AddressingDirs, which differ only in proofs: whether the boot's
// proof of an App's key stands in for a bundle of it not built yet (Installed.shared).
func gitHubDirs(served func(ghrepo.Repository) bool, renames ghrepo.Renames, installed *Installed, left *LeftOut, roles []AppRole, repo ghrepo.Repository, proofs bool) []string {
	owners, read := installed.shared(roles, proofs)
	if !read {
		return nil
	}
	layout, err := bundleDirs(owners, served, renames, repo)
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
// the owner each one's lease is minted on; owners, every owner name the bundle answers for as a
// set, which a controller's asked-for owner is read against; pairs, the configured renames the
// bundle routes a request by (ghconfig.Bundle.Pairs); dflt, the directory the issue's own
// repository routes to by those pairs, which is the bundle's default, or "" for a call naming
// no repository; and refused, the names among owners that are no directory's
// (ghconfig.IsOwner), sorted, which dirs leaves out.
type bundleLayout struct {
	dirs    []bundleDir
	owners  map[string]bool
	pairs   []ghrepo.Rename
	dflt    string
	refused []string
}

// mintOwner is the owner whose installation mints the lease of the directory name: the
// directory's own owner but for a configured rename's alias directory (bundleDirs), and name
// itself for a name the layout holds no directory for.
func (l bundleLayout) mintOwner(name string) string {
	if at := slices.IndexFunc(l.dirs, func(dir bundleDir) bool { return dir.name == name }); at >= 0 {
		return l.dirs[at].owner
	}
	return name
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
// whole of a role's reach and there is nothing smaller for a directory to stand for. served is
// whether both role Apps answered installed on a repository, as the daemon's repository state
// holds GitHub's answers (internal/daemon's repositories.servesRepository), which the rename rule
// reads and nothing else does: a pair resolves on whether each of its two repositories is served,
// never on which owners an App is installed on, which is the production failure the paragraph
// below records.
//
// The directories are one per installed owner, every owner a configured rename moved a
// repository away from (ghrepo.Renames.OwnerAliases) beside them, and the directory repo routes
// to by the layout's own pairs (ghconfig.RouteDir), which is the role's and not the project's:
// repo's recorded owner, or, for the repository a resolving pair moved, the owner it moved to,
// the recorded owner being in the set already as that pair's alias. That last one is the
// bundle's default and the directory the pod's GH_CONFIG_DIR names (GitHubCredential), routed
// by the rule `legion gh` and `legion git-credential` route a request by, so GH_CONFIG_DIR holds
// the lease the issue's own repository is answered from. It is in the set whatever the
// installations say so that an owner no installation of the App answers for is GitHub's own
// refusal of that mint, said where the ask was made, rather than a bundle quietly missing the
// directory it is served from.
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
// The rename rule is asked one question of each repository — is it served (served) — and never
// whether any repository of a pair's owners is.
// An owner test is the wrong question and was the production failure this rule replaces: a
// project that serves one more repository of the owner a pair moved a repository away from
// serves no more of the moved repository, yet an owner test reads that as the old owner being
// the project's own, stops resolving the pair, and mints every token of it on the installation
// the repository has left. Asked per repository, a pair resolves or does not on its own two
// names, so this is a pure function of its inputs and may be built per call.
//
// The pairs the layout carries are the configured renames that resolve and move a repository to
// another owner's installation (ghrepo.Renames.Moving), which is the same walk the alias map is
// keyed from. They go into the bundle, and from there into its root's pairs file
// (ghconfig.Bundle.Pairs, ghconfig.PairsFile), because a directory per owner cannot answer for
// both repositories of a pair whose two owners the App is installed on: `legion gh` and `legion
// git-credential` route the pair's `from` repository to the `to` owner's directory and every
// other repository of that owner to its own, which is a question about the repository and not
// about the owner (ghroute.DirFor).
//
// A name ghconfig holds to be no owner's directory (ghconfig.IsOwner) is left out of the layout
// and kept in refused, for the bundle to say once it stands (mintBundle), as an owner whose
// mint GitHub answers definitively is. ghconfig's grammar is ghrepo's owner grammar lowercased,
// a managed user's underscore included, so every owner a parsed repository or a configured
// rename names is a directory; what refused is for is a name that is still no directory of a
// bundle — one past GitHub's 39 characters, or a path — which discovery would hand on, since it
// takes any login GitHub lists, and which would otherwise fail the bundle and cost every role of
// the App every other owner. No directory is written from such a name, and no lease is minted
// for it. The one name that cannot be left out is the default's: a bundle with no directory for
// its issue's own repository is no bundle to launch a role on, so that name is an error, as a
// controller's asked-for owner is (ControllerCredential).
func bundleDirs(installed []string, served func(ghrepo.Repository) bool, renames ghrepo.Renames, repo ghrepo.Repository) (bundleLayout, error) {
	// The rename rule is read per repository: a pair resolves exactly when its `to` is served and
	// its `from` is not, which is asked of the two repositories themselves.
	aliases := renames.OwnerAliases(served)
	moving := renames.Moving(served)
	owners := make(map[string]bool, len(installed))
	for _, owner := range installed {
		owners[strings.ToLower(owner)] = true
	}
	layout := bundleLayout{owners: maps.Clone(owners), pairs: moving}
	for from := range aliases {
		layout.owners[from] = true
	}
	if !repo.IsZero() {
		layout.dflt, _, _ = ghconfig.RouteDir(moving, repo.Owner(), repo.Name())
		layout.owners[layout.dflt] = true
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
		if ghconfig.IsOwner(dir.name) {
			layout.dirs = append(layout.dirs, dir)
			continue
		}
		if dir.name == layout.dflt {
			return bundleLayout{}, noDirectoryName(dir.name)
		}
		layout.refused = append(layout.refused, dir.name)
	}
	return layout, nil
}

// noDirectoryName is the refusal of an owner whose name is no directory's (ghconfig.IsOwner):
// the failure of a bundle whose default it is, and what is said of every other such owner,
// which is left out (bundleDirs, mintBundle).
func noDirectoryName(name string) error {
	return fmt.Errorf("the gh bundle directory %q is no lowercased GitHub login", name)
}

// leftOutDirectory is the key one line of a directory left out of a role's gh bundle is said
// under: the App role, the owner, lowercased as a scope's is, and the reason (leaveOutReason).
type leftOutDirectory struct {
	role   AppRole
	owner  string
	reason string
}

// saidLeftOut holds every directory already said to be left out of a bundle, so the line is said
// once for the life of the process however many bundles are built after it, as a Manager says a
// grant shortfall once (sayShortfalls). It is the package's own, not a bundle's or a Manager's:
// the API builds a controller's bundle per request (internal/api's controllerGitHubCredential),
// and an owner whose lease cannot be minted fails at every one of them.
var (
	saidLeftOutMu sync.Mutex
	saidLeftOut   = map[leftOutDirectory]bool{}
)

// sayLeftOut says on log, at warn, that owner's directory is left out of role's gh bundle for
// reason (leaveOutReason, or noDirectoryReason for a name that is no directory's), with status,
// the mint's own failure or the refusal of the name, once per App role, owner and reason for
// the life of the process (saidLeftOut): the role's other owners are served, and what its
// bundle does not hold is said once rather than at every launch and every refresh, while a
// reason that changes is news and costs one line. It carries no token: a mint's failure names
// the ask and GitHub's answer to it and never a token (exchange, mintFromCommand), and the
// refusal of a name carries the name alone. A caller that holds no logger of its own
// (sandbox.Options.Log is one) says it on slog.Default.
func sayLeftOut(log *slog.Logger, role AppRole, owner, reason string, status error) {
	key := leftOutDirectory{role: role, owner: strings.ToLower(strings.TrimSpace(owner)), reason: reason}
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
		"app", string(role), "owner", owner, "reason", reason, "status", status.Error())
}

// noDirectoryReason is the reason an owner is left out of a bundle because its name is no
// directory's (bundleDirs), beside the reasons a mint's failure gives (leaveOutReason).
const noDirectoryReason = "no directory name"

// leaveOutReason is why a mint's failure is GitHub's definitive answer about the owner's
// installation, which leaves that owner's directory out of a bundle (mintBundle), and "" for
// every failure that is not one. Four answers are: the App is installed on no such account
// (ErrNotInstalled, the secrets broker's release naming no installation among it, which it gives
// a suspended one); the installation's account suspended it (errInstallationSuspended, read
// from the suspended_at of the installation itself before any token is asked of it, and never
// from a 403 to the token request, which is also GitHub's secondary rate limit); the
// installation is granted none of what the lease asks for (errGrantedNone); and GitHub
// refuses the permissions the request carried (ErrNarrowingRefused, NarrowingRefused).
//
// Nothing else is: GitHub's own trouble — a 5xx, no answer at all, any rate limit, the 403 of its
// secondary limit among them (TransientError) — and every failure the daemon cannot read as an
// answer about the installation fail the bundle. That costs a retry and nothing more: a refresh
// that fails keeps the role's last tokens, which last the hour, and is tried again a minute
// later, where a directory left out on a blip narrows a role's reach with nothing for anyone to
// read it by.
func leaveOutReason(err error) string {
	var trouble *TransientError
	switch {
	case errors.As(err, &trouble):
		return ""
	case errors.Is(err, ErrNotInstalled):
		return "not installed"
	case errors.Is(err, errInstallationSuspended):
		return "suspended"
	case errors.Is(err, errGrantedNone):
		return "granted none"
	case errors.Is(err, ErrNarrowingRefused):
		return "permissions refused"
	}
	return ""
}

// mintBundle is the bundle layout describes: one mint per scope, shared by every directory that
// holds it, each rendered as gh's two files, with dflt its default directory — the one the
// caller named, or, for a caller that named none, the first owner the mint answered for, sorted
// (bundleDefault). read is the lease every directory of this bundle holds, which is the caller's
// and not the directory's: the reading one for a controller, the App's own for a role, whose
// every directory holds the lease of the owner it is named for (GitHubCredential).
//
// A mint whose failure is GitHub's definitive answer about that owner's installation
// (leaveOutReason) costs the one scope it was for, every directory holding that lease: those are
// left out, and every other directory is minted and served, where failing would cost the role
// every owner and the launch for one account's installation (INFRA-513); leaving it out narrows
// what a role reaches and never widens it, and the record of it clears at the next mint that
// answers (LeftOut). Every other failure fails the bundle, naming the role, the App and the owner
// the lease would have been minted on, and never the token: GitHub's trouble and a rate limit are
// no answer about the owner. So does any failure of the default directory's own lease, a
// definitive one included: a role that cannot act on its own issue's repository has nothing to
// do, and a bundle whose GH_CONFIG_DIR holds no token is no bundle to launch on. So does a mint
// the caller's own context ended, which says nothing of the owner's installation and must record
// nothing of it.
//
// An owner whose name is no directory's is left out before any mint (bundleDirs, layout.refused):
// no lease is asked for it, and it is said beside the others.
//
// What was left out is recorded in left and said on log once the bundle stands, and not as each
// mint answers: a bundle that fails was served to nobody, so it leaves no directory out of
// anything, and the one line a directory gets for the life of the process (sayLeftOut) is not
// spent on it.
//
// Every directory of the layout is recorded in left as this mint found it, minted or left out
// (LeftOut), so the drift check's own answer (GitHubDirs) names what a bundle built now holds and
// nothing more.
//
// A layout with no owner at all is an App installed on no account, and no configured rename's
// owner beside it: runtime.ErrNoGitHubOwner, for both credentials.
func mintBundle(ctx context.Context, tokens Tokens, appRole AppRole, role claim.Role, read bool, dflt string, layout bundleLayout, left *LeftOut, log *slog.Logger) (ghconfig.Bundle, error) {
	if len(layout.owners) == 0 {
		return ghconfig.Bundle{}, fmt.Errorf("the %s App for %s: %w", appRole, role, runtime.ErrNoGitHubOwner)
	}
	// The pairs are the layout's, whatever the mints answer: a pair whose `to` owner is left out
	// of the bundle stays in the file and is refused where a request routes to that missing
	// directory, which is the one answer that cannot send a request the wrong owner's token
	// (ghconfig.Bundle.Pairs).
	bundle := ghconfig.Bundle{Dirs: make(map[string]ghconfig.Rendered, len(layout.dirs)), Pairs: layout.pairs}
	how := ""
	if read {
		how = "read-only "
	}
	// own is the lease the default directory holds, which every directory sharing it holds too:
	// a configured rename's alias directory shares the lease of the owner the repository moved
	// to, so the scope is what says whether a failure is the default's, never the name.
	own := dirLease{read: read, owner: layout.mintOwner(dflt)}.scope(appRole)
	minted := make(map[scope]ghconfig.Rendered, len(layout.dirs))
	failed := map[scope]error{}
	// failures is why each owner left out was, naming it, for a bundle every one of whose
	// owners was left out (bundleDefault): the names that are no directory's first, then each
	// left-out scope's error in the layout's order.
	var failures []error
	for _, name := range layout.refused {
		failures = append(failures, noDirectoryName(name))
	}
	for _, dir := range layout.dirs {
		hold := dirLease{read: read, owner: dir.owner}
		key := hold.scope(appRole)
		if _, done := minted[key]; !done && failed[key] == nil {
			rendered, err := dirEntry(ctx, tokens, appRole, hold)
			if err != nil {
				named := fmt.Errorf("mint the %s%s App lease for %s on %s: %w", how, appRole, role, hold.owner, err)
				if (dflt != "" && key == own) || ctx.Err() != nil || leaveOutReason(err) == "" {
					return ghconfig.Bundle{}, named
				}
				failed[key] = err
				failures = append(failures, named)
			} else {
				minted[key] = rendered
			}
		}
		if entry, held := minted[key]; held {
			bundle.Dirs[dir.name] = entry
		}
	}
	dflt, err := bundleDefault(bundle, appRole, role, dflt, layout, failures)
	if err != nil {
		return ghconfig.Bundle{}, err
	}
	for _, dir := range layout.dirs {
		_, held := bundle.Dirs[dir.name]
		left.record(appRole, dir.name, !held)
		if !held {
			err := failed[dirLease{read: read, owner: dir.owner}.scope(appRole)]
			sayLeftOut(log, appRole, dir.owner, leaveOutReason(err), err)
		}
	}
	for _, name := range layout.refused {
		sayLeftOut(log, appRole, name, noDirectoryReason, noDirectoryName(name))
	}
	bundle.Default = dflt
	return bundle, nil
}

// bundleDefault is the directory the bundle's GH_CONFIG_DIR names: dflt, the directory the caller
// asked for, which mintBundle has already failed the bundle for where its lease could not be
// minted, or, where it asked for none, the first directory the mint answered for, sorted
// (layout.dirs comes sorted by name).
//
// A caller that named none and whose every owner failed has no directory to be served from, and
// is refused with every owner's failure (failures): a bundle without a default would be refused
// later as one whose default holds no gh files (ghconfig.Bundle.Validate), long after the asks
// that could say why. Where every one of those was GitHub refusing the lease, the refusal is
// ErrNarrowingRefused, as each of them is.
func bundleDefault(bundle ghconfig.Bundle, appRole AppRole, role claim.Role, dflt string, layout bundleLayout, failures []error) (string, error) {
	if dflt != "" {
		return dflt, nil
	}
	for _, dir := range layout.dirs {
		if _, held := bundle.Dirs[dir.name]; held {
			return dir.name, nil
		}
	}
	return "", fmt.Errorf("the %s App's gh bundle for %s holds no owner the App is installed on (%s): %w",
		appRole, role, strings.Join(slices.Sorted(maps.Keys(layout.owners)), ", "), errors.Join(failures...))
}

// dirEntry is the directory's lease, rendered as gh's two files. A lease is the owner's whole
// installation, so there is nothing narrower to ask for and nothing to probe: a mint that fails
// is the whole directory, which mintBundle leaves out or fails the bundle for.
func dirEntry(ctx context.Context, tokens Tokens, appRole AppRole, hold dirLease) (ghconfig.Rendered, error) {
	lease, err := hold.mint(ctx, tokens, appRole)
	if err != nil {
		return ghconfig.Rendered{}, err
	}
	return ghconfig.Render(lease.Token, string(appRole), lease.ExpiresAt), nil
}
