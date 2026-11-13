//go:build e2e

// The implement App the Stage 4a harness mints provisioning tokens from. The rig is live_test.go.

package sandbox

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghbranch"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// implementTokens is the harness's ProvisionTokens: the implement App's installation token on
// the owner of the repository it is minted for, narrowed to reading, from the token command the
// script hands the harness (resolveApp), so the harness holds no App key. repos are the run's
// repositories, the first the run's own. installed and left are the two records the bundles
// write and the directory set reads — the owners each App's last bundle read installed, and
// what those mints find GitHub refuses — as the daemon's three share one pair
// (appauth.Installed, appauth.LeftOut, internal/daemon's sandboxRuntime).
type implementTokens struct {
	apps      *appauth.Manager
	repos     []ghrepo.Repository
	installed *appauth.Installed
	left      *appauth.LeftOut
}

func (t implementTokens) Token(ctx context.Context, repo ghrepo.Repository) (string, error) {
	// The reading lease and nothing else, as the daemon's own adapter mints it (internal/daemon's
	// implementTokens): a refusal is returned as it came, since the App is this run's own and a
	// refusal here is a misconfigured proof that must fail the run loudly.
	lease, err := t.apps.ReadToken(ctx, appauth.Implement, repo.Owner())
	return lease.Token, err
}

// Credential is the harness's GitHubCredential: the daemon's own bundle
// (appauth.GitHubCredential) over the owners the implement App is installed on, with every
// lease minted as that App, the one the harness holds — one directory per owner, each holding
// that owner's own installation lease at the App's permissions (INFRA-2543). Every
// role is rendered as the
// implementer's, so Stage 4a proves the files reach each role, follow the lease and pick the
// directory a command names, not which App a role writes as; Stage 4b's daemon hands each role
// its own.
func (t implementTokens) Credential(ctx context.Context, _ claim.Role, repo ghrepo.Repository) (ghconfig.Bundle, error) {
	return appauth.GitHubCredential(t.apps, implementApp{t.apps}, t.repositories, ghrepo.Renames{},
		t.installed, t.left, slog.Default())(ctx, claim.RoleImplementer, repo)
}

// ControllerCredential is the harness's ControllerCredential (Options.ControllerCredential): the
// controller's own bundle by default owner (appauth.ControllerCredential), which a controller
// pod's refresh asks for the owner its Secret records. It mints as the implement App, the one App
// the harness holds, where the daemon's own mints as the review App (implementApp).
func (t implementTokens) ControllerCredential(ctx context.Context, defaultOwner string) (ghconfig.Bundle, error) {
	return appauth.ControllerCredential(implementApp{t.apps}, implementApp{t.apps}, t.repositories, ghrepo.Renames{},
		t.installed, t.left, slog.Default())(ctx, defaultOwner)
}

// implementApp mints every lease, and reads every installation, as the implement App, whichever
// App role asks: the harness holds that App alone.
type implementApp struct{ apps *appauth.Manager }

func (a implementApp) Token(ctx context.Context, _ appauth.AppRole, owner string) (appauth.Lease, error) {
	return a.apps.Token(ctx, appauth.Implement, owner)
}

func (a implementApp) ReadToken(ctx context.Context, _ appauth.AppRole, owner string) (appauth.Lease, error) {
	return a.apps.ReadToken(ctx, appauth.Implement, owner)
}

func (a implementApp) InstalledOwners(ctx context.Context, _ appauth.AppRole) ([]string, error) {
	return a.apps.InstalledOwners(ctx, appauth.Implement)
}

// ProjectDirs is the harness's GitHubDirs (Options.GitHubDirs): the directories a bundle built
// now for repo would hold (appauth.GitHubDirs), from the record the last bundle of each App
// wrote, which is what Credential renders, so the two answers move together as the daemon's own
// do.
func (t implementTokens) ProjectDirs(repo ghrepo.Repository, roles []appauth.AppRole) []string {
	return appauth.GitHubDirs(t.repositories, ghrepo.Renames{}, t.installed, t.left, roles, repo)
}

// repositories is the run's repositories, as the daemon's mapping would serve them.
func (t implementTokens) repositories() []ghrepo.Repository { return t.repos }

// resolveApp mints the implement App's tokens through the token command the script hands the
// harness (LEGION_E2E_IMPLEMENT_APP_TOKEN_COMMAND): lib/github-apps.sh's own app_token_command,
// which asks the secrets broker's github-app secret for a release of installation tokens, so the
// broker client's argv lives in the lib alone, as real_github_test.go's liveProofImplementApp runs the same
// lib's output. The token manager runs it in token mode (appauth's TokenCommand) for each of the
// run's two repositories' owners, and with those tokens reads each repository's default branch as
// GitHub names it, the base every launch on it provisions at. No key reaches the harness.
func (r *liveRig) resolveApp() (err error) {
	apps := appauth.New(config.GitHubApps{Implement: config.GitHubApp{AppID: r.env.appID, TokenCommand: r.env.appTokenCommand}}, appauth.Options{})
	// The rig owns the manager once it is stored in r.tokens, and closes it in close; until then a
	// failure closes it here, so a token command still running never outlives the harness.
	defer func() {
		if err != nil {
			apps.Close()
		}
	}()
	branches := make([]string, 2)
	var identity runtime.GitIdentity
	for i, repo := range []ghrepo.Repository{r.env.repo, r.env.repoSecondOwner} {
		lease, err := apps.Token(r.ctx, appauth.Implement, repo.Owner())
		if err != nil {
			return fmt.Errorf("minting the implement App's installation token for %s: %w", repo.Owner(), err)
		}
		if branches[i], err = ghbranch.DefaultBranch(r.ctx, githubrest.Client{Token: lease.Token, API: githubrest.RepositoryAPI("", repo)}, repo); err != nil {
			return err
		}
		if i == 0 {
			identity = lease.Identity
		}
	}
	r.tokens = implementTokens{apps: apps, repos: []ghrepo.Repository{r.env.repo, r.env.repoSecondOwner},
		installed: appauth.NewInstalled(), left: appauth.NewLeftOut()}
	r.identity, r.defaultBranch, r.secondDefaultBranch = identity, branches[0], branches[1]
	return nil
}
