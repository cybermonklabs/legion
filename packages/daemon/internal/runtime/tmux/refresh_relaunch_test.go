package tmux

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// A claim's pane launched as the implementer is stopped and let go, the claim is re-roled to the
// tester, and its relaunch writes the tester's bundle into the claim's gh directory, the one
// directory every incarnation of the claim reads, and watches the new pane as the tester. A
// refresh whose snapshot of the watch still held the let-go pane leaves that directory on the
// tester's App: it writes a pane's bundle under the launch lock the relaunch writes under, and
// only while the pane it rendered for is still watched. The relaunch is the launch's own three
// steps under that lock (refuseLiveIncarnation's untrack, writeGitHubCredential's bundle,
// openPane's track); no tmux server runs. The control is the relaunch made before the tick.
func TestARefreshOfALetGoPaneLeavesTheRelaunchsBundle(t *testing.T) {
	for _, tc := range []struct {
		name       string
		interleave bool
	}{
		{name: "control: the relaunch lands before the refresh takes its snapshot of the watch", interleave: false},
		{name: "the relaunch lands between the refresh's snapshot and its write of the claim's gh directory", interleave: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			keyed, err := claim.NewToken("omp", "LEGION-48", claim.RoleImplementer)
			if err != nil {
				t.Fatal(err)
			}
			repo := ghrepo.MustParse("acme/widgets")
			old := runtime.Locator{Runtime: runtime.RuntimeTmux, Claim: keyed, Incarnation: "4242:1",
				Tmux: &runtime.TmuxLocator{Window: "@1", Pane: "%1"}}
			next := runtime.Locator{Runtime: runtime.RuntimeTmux, Claim: keyed, Incarnation: "4242:2",
				Tmux: &runtime.TmuxLocator{Window: "@1", Pane: "%2"}}
			var r *Runtime
			refreshing, interleaved := false, false
			relaunch := func() {
				r.launchMu.Lock()
				defer r.launchMu.Unlock()
				r.untrack(old)
				bundle, err := staticCredential(ctx, "", claim.RoleTester, repo)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := ghconfig.WriteBundle(runtime.GHConfigDir(r.stateDir, keyed), bundle); err != nil {
					t.Fatal(err)
				}
				r.track(next, "LEGION-48", repo, "acme", claim.RoleTester)
			}
			r = &Runtime{
				stateDir: t.TempDir(),
				gitHubCredential: appauth.CredentialOf(func(ctx context.Context, issue string, role claim.Role, repo ghrepo.Repository) (ghconfig.Bundle, error) {
					if refreshing && tc.interleave && !interleaved {
						interleaved = true
						relaunch()
					}
					return staticCredential(ctx, issue, role, repo)
				}),
				controllerCredential: staticControllerCredential,
				claimRepository:      acmeWidgets,
				log:                  slog.New(slog.NewTextHandler(io.Discard, nil)),
				tracked:              map[string]*trackedProcess{},
				writeBundle:          ghconfig.WriteBundle,
			}
			// The claim's first pane, launched as the implementer, with the implementer's bundle.
			first, err := staticCredential(ctx, "", claim.RoleImplementer, repo)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ghconfig.WriteBundle(runtime.GHConfigDir(r.stateDir, keyed), first); err != nil {
				t.Fatal(err)
			}
			r.track(old, "LEGION-48", repo, "acme", claim.RoleImplementer)
			if !tc.interleave {
				relaunch()
			}
			refreshing = true
			r.refreshGitHubCredentials(ctx)
			refreshing = false
			if tc.interleave && !interleaved {
				t.Fatal("the relaunch never landed inside the refresh: the case saw no window")
			}
			want := staticCredentialToken(claim.RoleTester)
			if got := readHostsToken(t, ghconfig.Dir(runtime.GHConfigDir(r.stateDir, keyed), "acme")); got != want {
				t.Fatalf("after the refresh the claim's gh directory, which the tester pane %s reads, holds %q, the App of the role the let-go pane %s was launched as; want the tester's %q",
					next.Incarnation, got, old.Incarnation, want)
			}
		})
	}
}

// A relaunch of a claim runs at the moment the claim's refresh writes the claim's gh directory.
// The refresh's snapshot holds the claim's pane launched as the implementer, still watched when the
// refresh judges it, so the refresh writes the implementer's bundle; the test holds that write on a
// barrier (Runtime.writeBundle) while a relaunch of the same claim runs in a goroutine of its own,
// as the launch's three steps under the launch lock (TestARefreshOfALetGoPaneLeavesTheRelaunchsBundle):
// it lets the old pane go, writes the tester's bundle and watches the new pane as the tester. The
// refresh writes under the launch lock, so the relaunch waits for that write and writes last, and
// the directory ends on the tester's App, the App of the pane that is watched. A refresh that wrote
// outside the lock would let the relaunch run to its end inside the held write, and the let-go
// pane's App would land last.
func TestARelaunchAtTheMomentOfARefreshsWriteWaitsForItAndWritesLast(t *testing.T) {
	ctx := context.Background()
	keyed, err := claim.NewToken("omp", "LEGION-48", claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	repo := ghrepo.MustParse("acme/widgets")
	old := runtime.Locator{Runtime: runtime.RuntimeTmux, Claim: keyed, Incarnation: "4242:1",
		Tmux: &runtime.TmuxLocator{Window: "@1", Pane: "%1"}}
	next := runtime.Locator{Runtime: runtime.RuntimeTmux, Claim: keyed, Incarnation: "4242:2",
		Tmux: &runtime.TmuxLocator{Window: "@1", Pane: "%2"}}
	stateDir := t.TempDir()
	writing, held := make(chan struct{}), make(chan struct{})
	// A test that fails before it releases the refresh's write still releases it, before its
	// directory is removed, so no goroutine is left holding the launch lock.
	release := sync.OnceFunc(func() { close(held) })
	t.Cleanup(release)
	r := &Runtime{
		stateDir:             stateDir,
		gitHubCredential:     appauth.CredentialOf(staticCredential),
		controllerCredential: staticControllerCredential,
		claimRepository:      acmeWidgets,
		log:                  slog.New(slog.NewTextHandler(io.Discard, nil)),
		tracked:              map[string]*trackedProcess{},
		// The refresh's one write, of the one pane it watches: held until the test releases it.
		writeBundle: func(root string, bundle ghconfig.Bundle) ([]string, error) {
			close(writing)
			<-held
			return ghconfig.WriteBundle(root, bundle)
		},
	}
	dir := runtime.GHConfigDir(r.stateDir, keyed)
	first, err := staticCredential(ctx, "", claim.RoleImplementer, repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ghconfig.WriteBundle(dir, first); err != nil {
		t.Fatal(err)
	}
	r.track(old, "LEGION-48", repo, "acme", claim.RoleImplementer)

	refreshed := make(chan struct{})
	go func() {
		defer close(refreshed)
		r.refreshGitHubCredentials(ctx)
	}()
	closedWithin(t, writing, "the refresh never reached its write of the claim's gh directory")
	waited, relaunched := make(chan struct{}), make(chan struct{})
	var relaunchErr error
	go func() {
		defer close(relaunched)
		// The relaunch takes the launch lock as a launch does, and says first whether it had to
		// wait for it: TryLock fails only while another holder has the lock.
		if !r.launchMu.TryLock() {
			close(waited)
			r.launchMu.Lock()
		}
		defer r.launchMu.Unlock()
		r.untrack(old)
		bundle, err := staticCredential(ctx, "", claim.RoleTester, repo)
		if err == nil {
			_, err = ghconfig.WriteBundle(dir, bundle)
		}
		if err != nil {
			relaunchErr = err
			return
		}
		r.track(next, "LEGION-48", repo, "acme", claim.RoleTester)
	}()
	// The refresh's write is held until the relaunch has either waited for the launch lock or run to
	// its end; only then does the refresh write.
	select {
	case <-waited:
	case <-relaunched:
	case <-time.After(time.Minute):
		t.Fatal("the relaunch neither waited for the launch lock nor ran to its end")
	}
	release()
	closedWithin(t, refreshed, "the refresh never ended")
	closedWithin(t, relaunched, "the relaunch never ended")
	if relaunchErr != nil {
		t.Fatalf("the relaunch: %v", relaunchErr)
	}

	want := staticCredentialToken(claim.RoleTester)
	if got := readHostsToken(t, ghconfig.Dir(dir, "acme")); got != want {
		t.Fatalf("after the relaunch and the refresh the claim's gh directory, which the tester pane %s reads, holds %q, the App of the role the let-go pane %s was launched as; want the tester's %q",
			next.Incarnation, got, old.Incarnation, want)
	}
	select {
	case <-waited:
	default:
		t.Fatal("the relaunch took the launch lock while the refresh was writing: the case saw no wait")
	}
	if watched := r.trackedProcesses(); len(watched) != 1 || watched[0].locator.Incarnation != next.Incarnation || watched[0].role != claim.RoleTester {
		t.Fatalf("the watch holds %+v, want the tester pane %s alone", watched, next.Incarnation)
	}
}

// closedWithin fails the test with what when ch is not closed within a minute: a goroutine the test
// waits on that never ends.
func closedWithin(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Minute):
		t.Fatal(what)
	}
}
