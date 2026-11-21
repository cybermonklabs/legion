package daemon

import (
	"context"
	"fmt"

	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// gateLaunch wraps store so every launch's recheck (CheckLaunch) and every admission of a claim
// that is started again (AdmitClaim) refuse first while the daemon's project release holds every
// start (release, errProjectReleased), whatever the claim, the controller's included. The daemon
// wraps its store with it in production, so the mark reaches every launch a machine makes: a
// spawn, a resume, a retry, a death's relaunch, a wake's turn and the stale-plugin lane alike.
//
// check, when non-nil, refuses next: a test's way to refuse a launch the way a reserved tree
// cleanup or a transient store error would (checkLaunch, internal/daemon/plugin_test.go), without
// a lifecycle this pod has none of to reserve. Then the real store's own check answers.
func gateLaunch(store supervise.Store, release *projectRelease, check func(context.Context, supervise.Claim) error) supervise.Store {
	return launchGatedStore{Store: store, release: release, check: check}
}

type launchGatedStore struct {
	supervise.Store
	release *projectRelease
	check   func(context.Context, supervise.Claim) error
}

func (s launchGatedStore) CheckLaunch(ctx context.Context, c supervise.Claim) error {
	if s.release.held() {
		return fmt.Errorf("launch %s: %w", c.Token, errProjectReleased)
	}
	if s.check != nil {
		if err := s.check(ctx, c); err != nil {
			return err
		}
	}
	return s.Store.CheckLaunch(ctx, c)
}

// AdmitClaim refuses a claim that is spawned, resumed or retried while the release holds, before
// the store binds it to its tree's epoch, so a refused start writes nothing (supervise's revive).
func (s launchGatedStore) AdmitClaim(ctx context.Context, c supervise.Claim) (supervise.Claim, error) {
	if s.release.held() {
		return supervise.Claim{}, fmt.Errorf("admit %s: %w", c.Token, errProjectReleased)
	}
	return s.Store.AdmitClaim(ctx, c)
}
