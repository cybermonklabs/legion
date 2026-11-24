package tmux

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// gitHubRefreshInterval is how often refreshGitHubCredentials walks the tracked panes. The daemon's
// function (Options.GitHubCredential) holds each App's lease and re-mints it as the lease nears its
// expiry, so most ticks render the hosts.yml the pane's directory already holds and write nothing;
// a minute keeps the time a role's gh runs on a token the daemon has already replaced short against
// an installation token's hour, at one file read per tracked pane.
const gitHubRefreshInterval = time.Minute

// refreshGitHubCredentialsEvery runs refreshGitHubCredentials every interval until ctx ends: the
// sweep's lifetime (Observe), so panes are refreshed exactly while they are watched.
func (r *Runtime) refreshGitHubCredentialsEvery(ctx context.Context, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			r.refreshGitHubCredentials(ctx)
		}
	}
}

// refreshGitHubCredentials brings the gh bundle of every tracked pane to the current render of the
// GitHub credential of the role whose App the pane holds (paneRole), so the pane's gh (the
// `legion gh` wrapper's real gh) and the clone's `legion git-credential` helper read the tokens
// the daemon holds now. It walks a snapshot of the watch (trackedProcesses), skipping a pane whose
// role the runtime cannot know, and renders a tree pane's credential for the pane's own
// repository: the one its launch minted the write token for, recorded on the tracked entry. For a
// tree pane adopted after a restart, whose launch the runtime never saw, the repository is the one
// the claim's issue works, read from the daemon's own state (Options.ClaimRepository).
//
// The controller's bundle is fetched by owner instead (Options.ControllerCredential), the default
// its launch recorded: it works no repository, so a bundle asked for by repository would take its
// default from the first owner the project serves, sorted, and a project that gained an owner
// sorting before the recorded one would move the default away from the directory the pane's
// GH_CONFIG_DIR was fixed at. An adopted controller, whose launch this runtime never saw, records
// no owner and asks for none, which is the first owner, as its launch took it.
//
// Nothing under the pane's gh directory, which the pane's own user can write, decides a repository
// or an owner, and a bundle's default owner, the directory GH_CONFIG_DIR names, is never minting
// authority. It rewrites only the directories whose hosts.yml differs
// (ghconfig.WriteBundle), recreating a root that is gone, and only while the pane it rendered for
// is still watched, judged again under the launch lock with the pane's role (writePaneCredential):
// the claim's gh directory is the claim's, for every pane it runs in, and a relaunch of a re-roled
// claim writes its new role's bundle there. A repository that cannot be read, a render that fails,
// or a render whose default is not the directory the pane's own repository routes to by the
// bundle's pairs (ghconfig.RouteDir, which the bundle's default is chosen by: the repository's
// owner, or the owner a configured pair moved it to) is logged and the pane keeps its last tokens
// until the next tick. A controller whose project has
// no GitHub owner to mint for (runtime.ErrNoGitHubOwner) launched with gh acting as nobody, its
// gh directory empty, and has nothing to refresh: it is passed over without a word, as the
// sandbox runtime passes over such a controller's pod. A runtime with no credential function
// (Options.GitHubCredential nil: a daemon with no GitHub Apps) wrote no pane any gh files, so it
// has nothing to refresh.
//
// The walk builds each bundle once (appauth.Bundles), held by the App role the pane works as on
// its issue (paneRole, runtime.SlotRole: its claim's role, or the role the architect acts as, the
// implementer while a merge forward asked of it has not ended, whatever step it does then, else the
// step's while it does a step of its own or the retro of one; appauth.AppRoleFor), the role itself
// where it is none of Legion's, and the pane's repository, and each build opens with a live read of
// every page of its App's installations under a fresh JWT (appauth.GitHubCredential), so the panes
// of one issue whose roles are Legion's and run as one App cost the tick one read rather than one
// apiece. The memo is the walk's and is dropped at its end, so every tick reads GitHub for what its
// bundles hold (INFRA-2543 R2).
//
// A pane's bundle is asked for on its claim's issue, whose plan can define the pane's role and
// then decides its App (appauth.RoleApps.Of): the issue the launch recorded, or, for a pane this
// runtime adopted, the issue key its claim's token names (claim.Token.IssueKey), which is exact.
func (r *Runtime) refreshGitHubCredentials(ctx context.Context) {
	if r.gitHubCredential == nil {
		return
	}
	bundles := appauth.NewBundles(r.gitHubCredential, r.controllerCredential, r.roleApps)
	for _, entry := range r.trackedProcesses() {
		token := entry.locator.Claim
		written, err := r.writePaneCredential(ctx, bundles, entry)
		if written.role == claim.RoleController && errors.Is(err, runtime.ErrNoGitHubOwner) {
			continue
		}
		if err != nil {
			r.log.Warn("tmux runtime: github credential refresh failed", "claim", token, "role", written.role, "err", err)
			continue
		}
		if len(written.changed) > 0 {
			held := written.bundle.Dirs[written.bundle.Default]
			r.log.Info("tmux runtime: github credential refreshed", "claim", token, "role", written.role, "app", held.App,
				"expiresAt", held.ExpiresAt, "dirs", written.changed)
		}
	}
}

// paneWrite is what one write of a pane's gh files did (writePaneCredential): the role whose App
// it wrote, judged under the launch lock; the bundle rendered for that role; the directories it
// rewrote; and whether the pane was still watched. A pane no longer watched, or whose role the
// runtime cannot know, is written nothing.
type paneWrite struct {
	role    claim.Role
	bundle  ghconfig.Bundle
	changed []string
	watched bool
}

// writePaneCredential writes entry's pane's gh files for the App of the role the pane works as
// (paneRole), and is every write of a watched pane's gh files: the refresh's and the one the
// daemon asks of a slot for an act of the owner's, a step, a retro or a merge forward asked of it
// (WriteSlotCredential). The role is judged once to mint its bundle before the launch lock, since a
// mint is a network call every launch would wait out, and judged again under the launch lock, which
// the write is made under and every launch's write of the claim's gh directory too
// (writeGitHubCredential); where the two differ, an act of the owner having started or ended in the
// mint, the bundle is minted again there for the role judged under the lock. So the App a pane's
// files hold is decided in the one critical section that writes it, and no write judged before
// another lands after it: a refresh that judged the architect's own App before the owner's act
// started cannot write it over the act's App once the act's write is made. The write is made only
// while the pane is still watched, judged under the same lock: the lock a launch holds from its
// refusal of the claim's earlier process, which lets that process go (refuseLiveIncarnation),
// through its own write of the claim's bundle to its watch of the new pane (launch), so a pane the
// caller's snapshot held and a relaunch has since let go is passed over, the relaunch having
// written the bundle of the role its new pane runs as.
func (r *Runtime) writePaneCredential(ctx context.Context, bundles *appauth.Bundles, entry trackedProcess) (paneWrite, error) {
	role, known, err := r.paneRole(ctx, entry)
	if err != nil || !known {
		return paneWrite{role: role}, err
	}
	bundle, err := r.renderPane(ctx, bundles, entry, role)
	if err != nil {
		return paneWrite{role: role}, err
	}
	r.launchMu.Lock()
	defer r.launchMu.Unlock()
	if !r.watching(entry) {
		return paneWrite{role: role}, nil
	}
	now, known, err := r.paneRole(ctx, entry)
	if err != nil || !known {
		return paneWrite{role: now}, err
	}
	if now != role {
		role = now
		if bundle, err = r.renderPane(ctx, bundles, entry, role); err != nil {
			return paneWrite{role: role}, err
		}
	}
	changed, err := r.writeBundle(runtime.GHConfigDir(r.stateDir, entry.locator.Claim), bundle)
	if err != nil {
		return paneWrite{role: role, bundle: bundle, watched: true}, fmt.Errorf("write the github credential: %w", err)
	}
	return paneWrite{role: role, bundle: bundle, changed: changed, watched: true}, nil
}

// renderPane is the bundle of role's App for entry's pane: the one bundles, the caller's memo,
// builds on the pane's claim's issue (appauth.Bundles.Bundle, which asks the controller's by the
// default owner its launch recorded), rendered for the pane's own repository — the one its launch
// minted the write token for, and for a tree pane this runtime adopted, the one the daemon records
// for the claim's issue (Options.ClaimRepository). A render whose default is not the directory the
// pane's own repository routes to (ghconfig.RouteDir) is refused. It writes nothing.
func (r *Runtime) renderPane(ctx context.Context, bundles *appauth.Bundles, entry trackedProcess, role claim.Role) (ghconfig.Bundle, error) {
	token := entry.locator.Claim
	repo := entry.ghRepo
	if entry.issue == "" && role != claim.RoleController {
		recorded, err := r.claimRepository(ctx, token)
		if err != nil {
			return ghconfig.Bundle{}, fmt.Errorf("the claim's repository: %w", err)
		}
		repo = recorded
	}
	issue := entry.issue
	if issue == "" {
		issue, _ = token.IssueKey()
	}
	bundle, err := bundles.Bundle(ctx, issue, role, repo, entry.ghDefault)
	if err != nil {
		return ghconfig.Bundle{}, err
	}
	if !repo.IsZero() {
		if routed, _, _ := ghconfig.RouteDir(bundle.Pairs, repo.Owner(), repo.Name()); bundle.Default != routed {
			return ghconfig.Bundle{}, fmt.Errorf("the credential's default owner %s is not the pane's own owner %s", bundle.Default, routed)
		}
	}
	return bundle, nil
}

// watching is whether entry's pane is still watched: its incarnation is tracked, for the same
// claim. Its callers hold the launch lock, under which a launch lets a pane go and watches its
// next one.
func (r *Runtime) watching(entry trackedProcess) bool {
	r.trackedMu.Lock()
	defer r.trackedMu.Unlock()
	watched, ok := r.tracked[entry.locator.Incarnation]
	return ok && watched.locator.Claim == entry.locator.Claim
}

// paneRole is the role whose App entry's pane holds (runtime.SlotRole): the controller's for the
// controller's pane; the role the runtime's ActsAs gives the claim, the implementer while a merge
// forward asked of the architect has not ended, whatever step it does then, else the step's while
// it does a step of its own or the retro of one; else the role the pane was launched as, which the
// claim held then and the process keeps for its whole life, and for a pane this runtime adopted
// without seeing its launch, the daemon's record of the claim's role (Options.ClaimRole). It
// answers false for a pane whose role it cannot know: an adopted pane with no record to ask, or a
// token that names no claim.
func (r *Runtime) paneRole(ctx context.Context, entry trackedProcess) (claim.Role, bool, error) {
	token := entry.locator.Claim
	if token.IsController() {
		return claim.RoleController, true, nil
	}
	stored := entry.role
	if stored == "" {
		if _, _, named := token.Cut(); !named || r.claimRole == nil {
			return "", false, nil
		}
		recorded, err := r.claimRole(ctx, token)
		if err != nil {
			return "", false, fmt.Errorf("the claim's role: %w", err)
		}
		stored = recorded
	}
	role, err := runtime.SlotRole(ctx, r.actsAs, token, stored)
	return role, err == nil, err
}
