package delivery

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// SettingsChannel is the Postgres notification channel a change of the delivery settings announces
// itself on at commit, with the project's key: the triggers of project_delivery_settings and
// project_deploy_sources (migration 20261123010254), on any change but the bookkeeping columns
// every GitHub event and reconcile pass rewrite and who saved the settings and when (updated_by,
// updated_at). It is the channel whose pglisten.Hub wake the server passes to StartIntakes.
const SettingsChannel = "dispatch_delivery_settings"

// settingsRetryInterval is how soon a SettingsCache reads the settings again after a read that
// failed. Only a failure arms it: a cache whose last read succeeded reads again on its wakes alone.
// It is the period the intake polled the settings at before the cache, so no path reads them more
// often than that did.
const settingsRetryInterval = 30 * time.Second

// SettingsWakes is the two wakes a SettingsCache reads the delivery settings again on, each nil
// for none. In the server both are the shared listener's (pglisten.Hub), which the hub also
// signals after every LISTEN it makes: a change made while the listener had no connection is read
// once it has one again.
//
// Settings is the wake on SettingsChannel: a save of any project's settings, which the cache reads
// again whole. Projects is the wake on the events every commit of an event announces
// (api.EventsChannel): a project.updated event is how a write of a project's default repository
// shows (PUT /api/v1/projects/{key}/default-repository), and a source that names no repository
// reads that default (SourceRepoOf), so a move of it moves the source's repository. On that wake
// the cache reads the default repositories of the projects whose sources read them, and reads the
// settings again only when one moved (defaultsMoved); with no such project it reads nothing.
type SettingsWakes struct {
	Settings <-chan struct{}
	Projects <-chan struct{}
}

// SettingsCache holds the rules both intakes route by (SourceRules): every source repository's
// rule and the population, read from the projects' delivery settings at start and again each time
// one of its wakes (SettingsWakes) is signalled, rather than on a timer or per message. A cache
// with no wake reads the settings once.
//
// While the listener is down, a change waits for its reconnect, as it does in the release before
// this one, whose cache of the one delivery_settings row is woken on the same channel.
type SettingsCache struct {
	pool  *store.Pool
	wakes SettingsWakes
	// log is where the cache writes a read that failed: the intakes' logger (intakeTuning.log),
	// given at construction.
	log *slog.Logger

	current atomic.Pointer[SourceRules]

	// read is closed by the first read that answered (reload), and readOnce closes it once.
	read     chan struct{}
	readOnce sync.Once
}

// newSettingsCache builds a cache over pool's delivery settings that reads them again on each
// signal on wakes.Settings, and on each signal on wakes.Projects that finds a moved default
// repository, and logs to logger, the intakes' (startIntakes passes intakeTuning.log); a nil logger
// is the default logger as it is now. It holds nothing until Run (or reload) has read the settings.
func newSettingsCache(pool *store.Pool, wakes SettingsWakes, logger *slog.Logger) *SettingsCache {
	return &SettingsCache{pool: pool, wakes: wakes, log: cmp.Or(logger, slog.Default()), read: make(chan struct{})}
}

// Read returns a channel closed once the cache's first read of the delivery settings has answered,
// whatever it found: the rules of every source repository, or none where no project's delivery is
// on. A read that failed is no answer, and Run makes it again. The delivery intake binds its
// durable only after it (Intake.Run), so a deployment with settings never routes an envelope by the
// nil the cache holds before then.
func (c *SettingsCache) Read() <-chan struct{} {
	return c.read
}

// Load returns the rules of the latest read, or nil while the cache has read none. A read that
// finds no delivery-on project stores rules with no repository: every run is then dropped and no
// pull request admitted.
func (c *SettingsCache) Load() *SourceRules {
	return c.current.Load()
}

// Run reads the settings at once, then each time wakes.Settings is signalled, and each time
// wakes.Projects is signalled and a default repository a source reads moved, until ctx ends. A read
// that fails is made again after settingsRetryInterval unless a wake comes first; a whole read that
// failed stays owed until one succeeds, whichever wake comes.
func (c *SettingsCache) Run(ctx context.Context) {
	owed := true
	for {
		var ok bool
		if owed {
			ok = c.reload(ctx)
			owed = !ok
		} else {
			ok = c.reloadIfDefaultsMoved(ctx)
		}
		var retry <-chan time.Time
		var timer *time.Timer
		if !ok {
			timer = time.NewTimer(settingsRetryInterval)
			retry = timer.C
		}
		select {
		case <-ctx.Done():
		case <-c.wakes.Settings:
			owed = true
		case <-c.wakes.Projects:
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

// reload reads every project's delivery settings and stores their rules. It reports false only for
// a read that failed, which Run makes again.
func (c *SettingsCache) reload(ctx context.Context) bool {
	rules, err := ReadSourceRules(ctx, c.pool)
	if err != nil {
		if ctx.Err() == nil {
			c.log.Error("dispatch delivery: read settings for the intakes", "error", err)
		}
		return false
	}
	c.current.Store(rules)
	c.readOnce.Do(func() { close(c.read) })
	return true
}

// reloadIfDefaultsMoved reads the settings again when a default repository a source of the held
// rules reads has moved since they were read (defaultsMoved). Run calls it only once a read has
// stored rules. It reports false only for a read that failed.
func (c *SettingsCache) reloadIfDefaultsMoved(ctx context.Context) bool {
	moved, err := defaultsMoved(ctx, c.pool, c.Load().defaults)
	if err != nil {
		if ctx.Err() == nil {
			c.log.Error("dispatch delivery: read the default repositories the deploy sources read", "error", err)
		}
		return false
	}
	if !moved {
		return true
	}
	return c.reload(ctx)
}

// defaultsMoved reports whether the default repository of a project in defaults is no longer the
// one it holds (a project deleted counts as moved), reading the projects only where defaults
// names one.
func defaultsMoved(ctx context.Context, q Querier, defaults map[string]*string) (bool, error) {
	if len(defaults) == 0 {
		return false, nil
	}
	keys := make([]string, 0, len(defaults))
	for key := range defaults {
		keys = append(keys, key)
	}
	rows, err := q.Query(ctx, `select key, default_repo from projects where key = any($1)`, keys)
	if err != nil {
		return false, fmt.Errorf("read the projects' default repositories: %w", err)
	}
	defer rows.Close()
	read := 0
	moved := false
	for rows.Next() {
		var key string
		var repo *string
		if err := rows.Scan(&key, &repo); err != nil {
			return false, fmt.Errorf("read the projects' default repositories: %w", err)
		}
		read++
		if deref(repo) != deref(defaults[key]) {
			moved = true
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("read the projects' default repositories: %w", err)
	}
	return moved || read != len(defaults), nil
}
