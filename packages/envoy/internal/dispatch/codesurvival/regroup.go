package codesurvival

// regroup.go is the regroup (plan, "The run", "The regroup"): a save of a project's code groups
// rebuilds its day rows of the two code evals from the stored results, in one transaction, with no
// GitHub request and no patch read (R3, k9), so the head branches and logins the run stored are
// all a new grouping needs.

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The regroup loop's pacing: passes start at least regroupGap apart, so a burst of saves costs one
// pass, and a pass that left a project unbuilt, for a failure or a project the seed has not reached
// yet, asks for another regroupRetry later.
const (
	regroupGap   = 2 * time.Second
	regroupRetry = time.Minute
)

// Regroup rebuilds the code evals' day rows of each project whose code groups moved since its rows
// were built. Pool is Dispatch's database, Build the running build, which the rows record, Now its
// clock (time.Now where nil), and Logger where it logs (slog.Default() where nil). It holds no
// GitHub client: a server with no GitHub App, as the eval bench's, regroups as well.
type Regroup struct {
	Pool   *store.Pool
	Build  string
	Now    func() time.Time
	Logger *slog.Logger

	// afterRows runs, where set, between a regroup's row locks and its series writes (prepare).
	afterRows func()
}

// Run runs the regroup's woken loop (duty.RunWoken) until ctx ends, with no lease: one pass at
// start, which regroups every project whose groups moved while no process ran, then a pass on
// each wake, the dispatch_delivery_settings notification a save of a project's delivery settings
// sends (delivery.SettingsChannel), and a pass a minute after one that left a project unbuilt. Two
// processes woken by one save each regroup; the second finds the digest stored under the row's
// lock and writes nothing (regroup).
func (g *Regroup) Run(ctx context.Context, wake <-chan struct{}) {
	duty.RunWoken(ctx, duty.WokenLoop{Wake: wake, MinGap: regroupGap, Pass: g.Pass})
}

func (g *Regroup) now() time.Time {
	if g.Now != nil {
		return g.Now().UTC()
	}
	return time.Now().UTC()
}

func (g *Regroup) logger() *slog.Logger {
	if g.Logger != nil {
		return g.Logger
	}
	return slog.Default()
}

// Pass is one regroup pass: each project whose code groups' digest (groupsDigest) differs from the
// one code_projects says its rows stand built under, or whose rows were never built, is rebuilt
// (regroup). It answers when to pass again: regroupRetry from now where a project was left unbuilt,
// and never otherwise.
func (g *Regroup) Pass(ctx context.Context) time.Time {
	projects, err := readProjects(ctx, g.Pool, nil)
	if err != nil {
		if ctx.Err() != nil {
			return time.Time{}
		}
		g.logger().Warn("codesurvival: read the projects to regroup", "error", err)
		return g.now().Add(regroupRetry)
	}
	retry := false
	for _, p := range projects {
		if ctx.Err() != nil {
			return time.Time{}
		}
		stood, err := ProjectDigest(ctx, g.Pool, p.Key)
		if err == nil && stood.Built() && stood.GroupsDigest == groupsDigest(p.Groups) {
			continue
		}
		if err == nil {
			var built bool
			if built, err = g.regroup(ctx, p.Key); err == nil && !built {
				retry = true
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				return time.Time{}
			}
			retry = true
			g.logger().Warn("codesurvival: regroup a project's code day rows", "project", p.Key, "error", err)
		}
	}
	if retry {
		return g.now().Add(regroupRetry)
	}
	return time.Time{}
}

// regroup rebuilds every day row of key's code evals from the stored results in one transaction
// (prepare, builder.write), under the lock of its code_projects row, which the run's builder takes
// too, and drains the outbox once it has committed. A project whose rows another builder has just
// built under its groups is left as it is. It answers false, with no error, where the seed has not
// written the project's code evals yet.
func (g *Regroup) regroup(ctx context.Context, key string) (bool, error) {
	now := g.now()
	tx, err := g.Pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin the regroup of %s: %w", key, err)
	}
	defer tx.Rollback(ctx)
	b, unseeded, err := prepare(ctx, tx, []string{key}, g.Build, now, g.afterRows)
	if err != nil {
		return false, err
	}
	if len(unseeded) > 0 || len(b.projects) == 0 {
		return false, nil
	}
	p := b.projects[0]
	if stood := b.stood[key]; stood.Built() && stood.GroupsDigest == groupsDigest(p.Groups) {
		if err := tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("commit the regroup of %s: %w", key, err)
		}
		g.drain(ctx)
		return true, nil
	}
	if err := b.write(ctx, nil); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit the regroup of %s: %w", key, err)
	}
	g.drain(ctx)
	g.logger().Info("codesurvival: regroup", "project", key, "groups", len(p.Groups))
	return true, nil
}

// drain moves the regroup's changes into the change log once its transaction committed.
func (g *Regroup) drain(ctx context.Context) {
	delivery.DrainAfterCommit(ctx, g.Pool, "codesurvival: a regroup's day rows could not move their changes into the delivery change log; the next drain moves them")
}
