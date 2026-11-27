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
// is still watched, judged again under the launch lock (writeWatchedBundle): the claim's gh
// directory is the claim's, for every pane it runs in, and a relaunch of a re-roled claim writes
// its new role's bundle there. A repository that cannot be read, a render that fails, or a render
// whose default is not the directory the pane's own repository routes to by the bundle's pairs
// (ghconfig.RouteDir, which the bundle's default is chosen by: the repository's owner, or the
// owner a configured pair moved it to) is logged and the pane keeps its last tokens until the
// next tick. A controller whose project has
// no GitHub owner to mint for (runtime.ErrNoGitHubOwner) launched with gh acting as nobody, its
// gh directory empty, and has nothing to refresh: it is passed over without a word, as the
// sandbox runtime passes over such a controller's pod. A runtime with no credential function
// (Options.GitHubCredential nil: a daemon with no GitHub Apps) wrote no pane any gh files, so it
// has nothing to refresh.
//
// The walk builds each bundle once (appauth.Bundles), held by the App role the pane's claim role
// runs as on its issue (appauth.AppRoleFor), the role itself where it is none of Legion's, and the
// pane's repository, and each build opens with a live read of every page of its App's
// installations under a fresh JWT (appauth.GitHubCredential), so the panes of one issue whose
// roles are Legion's and run as one App cost the tick one
// read rather than one apiece. The memo is the walk's and is dropped at its end, so every tick
// reads GitHub for what its bundles hold (INFRA-2543 R2).
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
		role, ok, err := r.paneRole(ctx, entry)
		refreshFailed := func(err error) {
			r.log.Warn("tmux runtime: github credential refresh failed", "claim", token, "role", role, "err", err)
		}
		if err != nil {
			refreshFailed(err)
			continue
		}
		if !ok {
			continue
		}
		repo := entry.ghRepo
		if entry.issue == "" && role != claim.RoleController {
			recorded, err := r.claimRepository(ctx, token)
			if err != nil {
				refreshFailed(fmt.Errorf("the claim's repository: %w", err))
				continue
			}
			repo = recorded
		}
		issue := entry.issue
		if issue == "" {
			issue, _ = token.IssueKey()
		}
		bundle, err := bundles.Bundle(ctx, issue, role, repo, entry.ghDefault)
		if role == claim.RoleController && errors.Is(err, runtime.ErrNoGitHubOwner) {
			continue
		}
		if err != nil {
			refreshFailed(err)
			continue
		}
		if !repo.IsZero() {
			if routed, _, _ := ghconfig.RouteDir(bundle.Pairs, repo.Owner(), repo.Name()); bundle.Default != routed {
				refreshFailed(fmt.Errorf("the credential's default owner %s is not the pane's own owner %s", bundle.Default, routed))
				continue
			}
		}
		root := runtime.GHConfigDir(r.stateDir, token)
		changed, watched, err := r.writeWatchedBundle(entry, root, bundle)
		if err != nil {
			refreshFailed(fmt.Errorf("write the github credential: %w", err))
			continue
		}
		if !watched {
			continue
		}
		if len(changed) > 0 {
			written := bundle.Dirs[bundle.Default]
			r.log.Info("tmux runtime: github credential refreshed", "claim", token, "role", role, "app", written.App,
				"expiresAt", written.ExpiresAt, "dirs", changed)
		}
	}
}

// writeWatchedBundle writes bundle, rendered for entry's pane, into root, the gh directory of
// entry's claim, and answers the directories it rewrote and true, or nothing and false where the
// pane is no longer watched. It judges that under the launch lock, the lock a launch holds from
// its refusal of the claim's earlier process, which lets that process go (refuseLiveIncarnation),
// through its own write of the claim's bundle to its watch of the new pane (launch): a pane the
// refresh's snapshot held and a relaunch has since let go is passed over, since the relaunch has
// written the bundle of the role its new pane runs as, and a relaunch that starts after this
// judgment writes its bundle after this write. The next tick refreshes the new pane.
func (r *Runtime) writeWatchedBundle(entry trackedProcess, root string, bundle ghconfig.Bundle) ([]string, bool, error) {
	r.launchMu.Lock()
	defer r.launchMu.Unlock()
	r.trackedMu.Lock()
	watched, ok := r.tracked[entry.locator.Incarnation]
	r.trackedMu.Unlock()
	if !ok || watched.locator.Claim != entry.locator.Claim {
		return nil, false, nil
	}
	changed, err := r.writeBundle(root, bundle)
	return changed, true, err
}

// paneRole is the role whose App entry's pane holds (runtime.SlotRole): the controller's for the
// controller's pane, else the role the pane was launched as, which the claim held then and the
// process keeps for its whole life, and for a pane this runtime adopted without seeing its launch,
// the daemon's record of the claim's role (Options.ClaimRole). It answers false for a pane whose
// role it cannot know: an adopted pane with no record to ask, or a token that names no claim.
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
