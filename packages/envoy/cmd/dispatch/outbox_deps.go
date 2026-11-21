package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/outbox"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/roles"
)

// projectPrivacyReadTimeout bounds one read of a project's privacy within the caller's own
// context, as each read of the role and session buckets beside it is bounded (roles.Buckets): a
// read that has not answered by then is an answer that is not known, and the role route it was
// for waits as for any read that did not answer (roles.ResolveHolder, roles.PrivacyRead).
const projectPrivacyReadTimeout = 2 * time.Second

// outboxDeps is the events outbox's dependencies as the server runs them, built in this one place
// so a test proves the outbox's rules through the Deps main hands outbox.New
// (outbox_deps_test.go). pool is the shared pool: the outbox's LISTEN is dialed from it, and a
// role route's privacy read goes through it (committedProjectPrivacy), since the outbox's own
// pool, outboxPool, holds two connections, one statement at a time and the publisher hold's
// renewal, and a batch resolves its roles at once.
//
// One reader of the role and session buckets serves both RoleClaims and Sessions: a role route's
// holder is read straight from the buckets on the bus client's connection, with no watcher, judged
// by the windows this deployment gave its listeners (roleHolderWindows), in the privacy mode it
// gave Dispatch (projectPrivacyMode), and with each project's privacy read from Dispatch's own
// committed rows.
func outboxDeps(
	boot bootConfig, pool, outboxPool *store.Pool, natsClient *bus.Client, broker *events.Broker,
	publishFailures, roleForwardsHeld *metrics.Counter, met *metrics.Registry,
) outbox.Deps {
	holders := roles.NewBuckets(natsClient, boot.RoleHolderWindows, committedProjectPrivacy(pool), boot.ProjectPrivacy)
	return outbox.Deps{
		Store:            &store.Store{Pool: outboxPool},
		Publisher:        natsClient,
		Broker:           broker,
		Listen:           pool.Listener,
		RoleClaims:       holders,
		Sessions:         holders,
		KeptLanes:        natsClient,
		PublishFailures:  publishFailures,
		RoleForwardsHeld: roleForwardsHeld,
		Metrics:          met,
	}
}

// projectPrivacyMode is DISPATCH_PROJECT_PRIVACY_REQUIRED, read once at start: `true` is
// roles.Required, in which a role route's holder is taken only when its stored projects see the
// project the event is of (roles.Sees); unset or blank is roles.AllPublic, in which a holder
// is taken as before the modes existed. Any other value refuses the start, naming the variable.
// Its value is the deployment's, set beside its listeners' ENVOY_PROJECT_PRIVACY_REQUIRED, and it
// is never taken from DISPATCH_PRIVATE_HUBS, which a deployment sets only after its listeners
// already apply each token's list.
func projectPrivacyMode(env settingValues) (roles.Mode, error) {
	switch value := strings.TrimSpace(env.get("DISPATCH_PROJECT_PRIVACY_REQUIRED")); value {
	case "":
		return roles.AllPublic, nil
	case "true":
		return roles.Required, nil
	default:
		return roles.AllPublic, fmt.Errorf("invalid DISPATCH_PROJECT_PRIVACY_REQUIRED %q: the one value is true, and the all-public mode is DISPATCH_PROJECT_PRIVACY_REQUIRED unset", value)
	}
}

// committedProjectPrivacy answers whether a project is private from Dispatch's own committed
// projects row, the truth the privacy answer every listener reads is written from, so no record
// on the bus can change its answer: a row whose visibility is `private` is private, and one whose
// visibility is `public` is public. A key no row holds counts as private, as a project whose
// privacy is not known counts everywhere: the role route then keeps its event for a holder whose
// token lists the key. A read that fails, or does not answer within projectPrivacyReadTimeout,
// answers its error, which holds the role route rather than send it to a holder the answer would
// have refused.
func committedProjectPrivacy(pool *store.Pool) roles.ProjectPrivacy {
	return func(ctx context.Context, project string) (bool, error) {
		ctx, cancel := context.WithTimeout(ctx, projectPrivacyReadTimeout)
		defer cancel()
		var private bool
		err := pool.QueryRow(ctx, `select visibility = 'private' from projects where key = $1`, project).Scan(&private)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return true, nil
		case err != nil:
			return false, fmt.Errorf("read project %s's visibility: %w", project, err)
		}
		return private, nil
	}
}
