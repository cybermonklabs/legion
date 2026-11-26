package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// waitLine is the line a start writes while its slot's gh files may not hold its credential yet.
const waitLine = "sandbox runtime: the start waits for its slot's gh files to hold its credential"

// Key a's claim `…-implementer` took the role tester, and it launches into the issue pod that
// stands. The launch keeps the pod, writes the slot `implementer`'s Secret for the review App at
// once, and its start frame names the digest of the token that Secret's default hosts.yml now holds,
// with a bound on the launcher's wait for it inside the launch's own wait on the start, and one log
// line naming the slot and the App. A later launch of the same claim with no change of App writes
// no Secret, names the same digest and logs nothing. A re-roled claim whose slot the refresher
// already wrote for its new App still logs the line, since its files may not hold the credential
// yet whoever wrote it. A launch that builds its pod names no credential.
func TestALaunchOfAReroledClaimWritesItsSlotsCredentialAtOnce(t *testing.T) {
	keyed := claim.Token("legion-legion-legion-208-implementer")
	refreshed := claim.Token("legion-legion-legion-208-planner")
	logs := &lockedLog{}
	g := newRig(t, nil, withOptions(func(o *Options) {
		o.Log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
		o.GitHubCredential = func(_ context.Context, role claim.Role, repo ghrepo.Repository) (ghconfig.Bundle, error) {
			owner := strings.ToLower(repo.Owner())
			if owner == "" {
				owner = "sjawhar"
			}
			appRole, err := appauth.AppRoleFor(role, nil)
			if err != nil {
				return ghconfig.Bundle{}, err
			}
			app := string(appRole)
			return ghconfig.Bundle{Default: owner, Dirs: map[string]ghconfig.Rendered{
				"acme": ghconfig.Render("ghs_"+app+"_read", app, staticCredentialExpiry),
				owner:  ghconfig.Render("ghs_"+app, app, staticCredentialExpiry),
			}}, nil
		}
	}))
	g.spawn(rootSpec(t))
	issue := SandboxName(rootToken)
	if built := lastStart(t, g, rootToken); built.GitHubCredential != nil {
		t.Fatalf("the launch that built the pod names the credential %+v, want none", built.GitHubCredential)
	}
	before := g.pod(issue).UID
	digest := func(token string) string {
		sum := sha256.Sum256([]byte(token))
		return hex.EncodeToString(sum[:])
	}
	held := func(slot claim.Role) string {
		t.Helper()
		token, err := ghconfig.TokenFromHosts(g.secret(roleSecretName(issue, slot)).Data[GitHubHostsKey("sjawhar")])
		if err != nil {
			t.Fatalf("the %s slot's hosts.yml: %v", slot, err)
		}
		return token
	}
	waits := func() []string { return linesHolding(logs.String(), waitLine) }

	g.clearActions()
	g.spawn(testSpec(t, keyed, claim.RoleTester, testTree))
	if after := g.pod(issue).UID; after != before {
		t.Fatalf("the launch replaced the pod (%s, then %s), want it kept", before, after)
	}
	if got := held(claim.RoleImplementer); got != "ghs_review" {
		t.Fatalf("the implementer slot's hosts.yml holds %q after the launch, want the review App's", got)
	}
	start := lastStart(t, g, keyed)
	want := shimwire.LauncherGitHubCredential{HostsFile: "sjawhar/" + ghconfig.HostsFile, TokenDigest: digest("ghs_review")}
	// The launcher may wait for the files no longer than the launch waits on its start before the
	// stop grace: what remains of the start bound its launcher wait answered (awaitRoleLauncher).
	names := func(got *shimwire.LauncherGitHubCredential) bool {
		bound := max(g.r.bootTimeout, g.r.keptVolumeBound)
		return got != nil && got.HostsFile == want.HostsFile && got.TokenDigest == want.TokenDigest &&
			got.WaitMs > 0 && time.Duration(got.WaitMs)*time.Millisecond <= bound
	}
	if !names(start.GitHubCredential) {
		t.Fatalf("the start frame names the credential %+v, want %+v with a bound inside the launch's own wait on its start", start.GitHubCredential, want)
	}
	if lines := waits(); len(lines) != 1 || !strings.Contains(lines[0], "slot=implementer") || !strings.Contains(lines[0], "app=review") {
		t.Fatalf("the launch logged %q, want one line naming the slot implementer and the App review", lines)
	}

	g.clearActions()
	again := testSpec(t, keyed, claim.RoleTester, testTree)
	again.Generation++
	g.spawn(again)
	if updated := secretUpdates(g.writes()); slices.Contains(updated, roleSecretName(issue, claim.RoleImplementer)) {
		t.Fatalf("the launch with no change of App updated %v, want the implementer slot's Secret untouched", updated)
	}
	if start := lastStart(t, g, keyed); !names(start.GitHubCredential) {
		t.Fatalf("the second start frame names %+v, want %+v with a bound inside the launch's own wait on its start", start.GitHubCredential, want)
	}
	if lines := waits(); len(lines) != 1 {
		t.Fatalf("the launch with no change of App logged %q, want no second line", lines)
	}

	g.store.mu.Lock()
	g.store.claims = append(g.store.claims, supervise.Claim{Token: refreshed, Project: testProject, Tree: testTree, Issue: testTree, Role: claim.RoleTester})
	g.store.mu.Unlock()
	g.r.refreshGitHubCredentials(g.ctx)
	if got := held(claim.RolePlanner); got != "ghs_review" {
		t.Fatalf("the planner slot's hosts.yml holds %q after the refresh, want the review App's: the case's premise", got)
	}
	g.spawn(testSpec(t, refreshed, claim.RoleTester, testTree))
	if lines := waits(); len(lines) != 2 || !strings.Contains(lines[1], "slot=planner") || !strings.Contains(lines[1], "app=review") {
		t.Fatalf("the launch after the refresher's write logged %q, want a line naming the slot planner and the App review", lines)
	}
}

// A refresh leaves the slot of a launch into a kept pod alone from just before the launch writes
// the slot's credential until its start is answered, whatever lease the daemon has minted since:
// the start names the digest of the token the launch left in the slot, and the launcher waits for
// the files to hold exactly that token, so a newer token of the same App written over it would
// leave the files never holding it. That holds for a refresh that begins while the start waits,
// and for one that read the slot's Secret before the launch began and writes after it, where the
// launch found the Secret holding its token already and wrote nothing a resourceVersion could
// refuse the refresh's write over. The control is the refresher's next tick after the start,
// which writes the newer token as usual.
func TestARefreshLeavesTheSlotOfAKeptPodsLaunchOnTheStartsToken(t *testing.T) {
	for _, tc := range []struct {
		name string
		// spec is the launch into the kept pod. early is whether it lands inside the refresh, in
		// its build of the review App's bundle for the launch's own slot, which it read first.
		spec  func(t *testing.T) runtime.SpawnSpec
		early bool
	}{
		{name: "the refresh begins while the start waits", spec: workerSpec},
		{name: "the refresh read the slot before the launch began", early: true, spec: func(t *testing.T) runtime.SpawnSpec {
			again := rootSpec(t)
			again.Generation++
			return again
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first, newer := "ghs_review_1", "ghs_review_2"
			var lease atomic.Pointer[string]
			lease.Store(&first)
			spec := tc.spec(t)
			var g *rig
			launched := make(chan error, 1)
			// launch makes the launch into the kept pod and returns once its start has reached the
			// launcher, which holds it unanswered.
			launch := func() {
				go func() { _, err := g.r.Spawn(g.ctx, spec); launched <- err }()
				testwait.WithinPolling(t, 10*time.Second, 5*time.Millisecond, "the launch's start to reach its launcher", func() bool {
					return slices.ContainsFunc(g.sent(spec.Claim), func(frame shimwire.Frame) bool {
						start, ok := frame.(shimwire.LauncherStart)
						return ok && start.Generation == spec.Generation
					})
				})
			}
			var refreshing, landing atomic.Bool
			var landedIn claim.Role
			g = newRig(t, nil, withOptions(func(o *Options) {
				o.GitHubCredential = func(_ context.Context, role claim.Role, repo ghrepo.Repository) (ghconfig.Bundle, error) {
					appRole, err := appauth.AppRoleFor(role, nil)
					if err != nil {
						return ghconfig.Bundle{}, err
					}
					if tc.early && refreshing.Load() && appRole == appauth.Review && landing.CompareAndSwap(false, true) {
						landedIn = role
						launch()
						lease.Store(&newer)
					}
					owner := strings.ToLower(repo.Owner())
					if owner == "" {
						owner = "sjawhar"
					}
					token := "ghs_" + string(appRole)
					if appRole == appauth.Review {
						token = *lease.Load()
					}
					return ghconfig.Bundle{Default: owner, Dirs: map[string]ghconfig.Rendered{
						"acme": ghconfig.Render(token+"_read", string(appRole), staticCredentialExpiry),
						owner:  ghconfig.Render(token, string(appRole), staticCredentialExpiry),
					}}, nil
				}
			}))
			g.spawn(rootSpec(t))
			issue := SandboxName(rootToken)
			slot, _ := spec.Claim.Slot()
			held := func() string {
				t.Helper()
				token, err := ghconfig.TokenFromHosts(g.secret(roleSecretName(issue, launcherOf(slot))).Data[GitHubHostsKey("sjawhar")])
				if err != nil {
					t.Fatalf("the %s slot's hosts.yml: %v", slot, err)
				}
				return token
			}
			g.launcher(spec.Claim)
			hold := make(chan struct{})
			g.commandsMu.Lock()
			g.holdStart = hold
			g.commandsMu.Unlock()
			if !tc.early {
				launch()
				lease.Store(&newer)
			}
			refreshing.Store(true)
			g.r.refreshGitHubCredentials(g.ctx)
			refreshing.Store(false)
			if tc.early && landedIn != launcherOf(slot) {
				t.Fatalf("the launch landed in the refresh's build for the %q slot, want its own slot %s's: the case's premise", landedIn, slot)
			}
			sum := sha256.Sum256([]byte(first))
			if start := lastStart(t, g, spec.Claim); start.Generation != spec.Generation || start.GitHubCredential == nil ||
				start.GitHubCredential.TokenDigest != hex.EncodeToString(sum[:]) {
				t.Fatalf("the launch's start names %+v at generation %d, want the digest of %s at %d: the case's premise",
					start.GitHubCredential, start.Generation, first, spec.Generation)
			}
			if got := held(); got != first {
				t.Fatalf("after a refresh while the launch's start waits, the %s slot's hosts.yml holds %q, a newer token of its App; the start names the digest of %q, which the slot's files would never hold",
					slot, got, first)
			}

			g.commandsMu.Lock()
			g.holdStart = nil
			g.commandsMu.Unlock()
			close(hold)
			if err := <-launched; err != nil {
				t.Fatalf("the launch into the kept pod: %v", err)
			}
			g.r.refreshGitHubCredentials(g.ctx)
			if got := held(); got != newer {
				t.Fatalf("the refresh after the start left the %s slot's hosts.yml holding %q, want the newer %q", slot, got, newer)
			}
		})
	}
}
