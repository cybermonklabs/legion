package delivery

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// SettingsChannel is the Postgres notification channel a delivery_settings change announces itself
// on at commit (delivery_settings_changed, migration 0156): any change but the freshness columns
// every GitHub event and reconcile pass rewrite. It is the channel whose pglisten.Hub wake the
// server passes to StartIntakes.
const SettingsChannel = "dispatch_delivery_settings"

// settingsRetryInterval is how soon a SettingsCache reads delivery_settings again after a read
// that failed. Only a failure arms it: a cache whose last read succeeded reads again on its wake
// alone. It is the period the intake polled the row at before the cache, so no path reads it more
// often than that did.
const settingsRetryInterval = 30 * time.Second

// SettingsCache holds the delivery settings both intakes route by, read from delivery_settings at
// start and again each time its wake is signalled, rather than on a timer or per message. In the
// server the wake is the shared listener's (pglisten.Hub) on SettingsChannel, which the hub also
// signals after every LISTEN it makes: a change made while the listener had no connection is read
// once it has one again. A cache with no wake reads the row once.
//
// While the listener is down, a change waits for its reconnect; the release before this one, which
// still polls every 30 s, may run beside it during a roll and read the change sooner.
type SettingsCache struct {
	pool *store.Pool
	wake <-chan struct{}
	// log is where the cache writes a read that failed and a malformed deploy repository: the
	// intakes' logger (intakeTuning.log), given at construction.
	log *slog.Logger

	current atomic.Pointer[DeliverySettings]

	mu sync.Mutex
	// changed is closed by the next read that stores settings, and replaced by a fresh channel.
	changed chan struct{}
}

// newSettingsCache builds a cache over pool's delivery_settings that reads again on each signal on
// wake and logs to logger, the intakes' (startIntakes passes intakeTuning.log); a nil logger is the
// default logger as it is now. It holds nothing until Run (or reload) has read the row.
func newSettingsCache(pool *store.Pool, wake <-chan struct{}, logger *slog.Logger) *SettingsCache {
	return &SettingsCache{pool: pool, wake: wake, log: cmp.Or(logger, slog.Default()), changed: make(chan struct{})}
}

// Load returns the latest settings the cache read whose deploy repository is a well-formed
// owner/repo (refs.SplitGitHubRepo), or nil while it has read none. A later read that finds no
// row, or a malformed repository, leaves the settings before it in place.
func (c *SettingsCache) Load() *DeliverySettings {
	return c.current.Load()
}

// Changed returns a channel that is closed once the cache next stores settings. Taken before a
// Load, it cannot miss a store that lands after that Load: the intake's bind waits on it for the
// first settings.
func (c *SettingsCache) Changed() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.changed
}

// Run reads delivery_settings at once, then each time the wake is signalled, until ctx ends. A read
// that fails is made again after settingsRetryInterval unless a wake comes first.
func (c *SettingsCache) Run(ctx context.Context) {
	for {
		var retry <-chan time.Time
		var timer *time.Timer
		if !c.reload(ctx) {
			timer = time.NewTimer(settingsRetryInterval)
			retry = timer.C
		}
		select {
		case <-ctx.Done():
		case <-c.wake:
		case <-retry:
		}
		if timer != nil {
			timer.Stop()
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// reload reads delivery_settings and stores what it read when its deploy repository is well
// formed. It reports false only for a read that failed, which Run makes again: no row, or a
// malformed repository, is an answer, and the write that fixes it notifies.
func (c *SettingsCache) reload(ctx context.Context) bool {
	settings, err := GetSettings(ctx, c.pool)
	switch {
	case errors.Is(err, ErrNoSettings):
		return true
	case err != nil:
		if ctx.Err() == nil {
			c.log.Error("dispatch delivery: read settings for the intakes", "error", err)
		}
		return false
	}
	if _, _, err := refs.SplitGitHubRepo(settings.DeployRepo); err != nil {
		c.log.Error("dispatch delivery: deploy_repo setting", "error", err)
		return true
	}
	c.current.Store(&settings)
	c.mu.Lock()
	close(c.changed)
	c.changed = make(chan struct{})
	c.mu.Unlock()
	return true
}
