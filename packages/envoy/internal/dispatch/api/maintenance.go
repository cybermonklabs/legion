package api

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// MaintenanceSweepInterval is how often one process deletes the Idempotency-Keys and spent DPoP
// proofs past their windows and sends the deliveries a process that died left owed. Nothing here
// is in a request's latency budget: a key is read by a repeat long before it is swept, a proof
// record only refuses a replay until its window ends, and an owed delivery is one whose sender
// already died, so a minute of delay costs nothing a caller can see, and a minutes-scale interval
// keeps these statements off any tight loop in production.
const MaintenanceSweepInterval = time.Minute

// maintenanceLease names the lease one pass of this sweep runs under. The name is the one the
// suggestion outcome sweep carried this work under before it moved here, and it stays so: during
// a rolling deploy the earlier build's pass, which swept the keys and the owed deliveries inside
// its own suggestion pass, and this build's timer still exclude each other on that work, which a
// new name would not.
const maintenanceLease = "suggestion-outcomes"

// StoreCompletenessInterval is how often a process's maintenance passes check, where the
// deployment names private projects, that the store can hold one (StoreIncomplete). A switch to
// private is refused until the project's own rows are complete; this names a row stored without
// its node afterwards. The read covers every project-owned table, so it runs hourly and not every
// pass.
const StoreCompletenessInterval = time.Hour

// incompleteRowsNamed is how many of the incomplete rows a report names, the census's count
// (envoy-dispatch census --store names the same rows).
const incompleteRowsNamed = 20

// StoreIncomplete answers nil when a private project's reads would answer no row wrongly: every
// row a project owns has a node naming its project (graphcensus.ProjectComplete), and
// session_projects_filled holds its row. Otherwise it answers why, naming the first rows and the
// command that repairs it.
func StoreIncomplete(ctx context.Context, q graphstore.Queryer) error {
	incomplete, err := graphcensus.ProjectComplete(ctx, q)
	if err != nil {
		return err
	}
	if len(incomplete) > 0 {
		return incompleteRowsError(incomplete, "a project owns")
	}
	return sessionProjectsFilled(ctx, q)
}

// incompleteRowsError says how many rows, those whose names ("a project owns", "of PRIV"), have no
// node naming their project, names the first of them, and the command that writes their nodes.
func incompleteRowsError(incomplete []string, whose string) error {
	return fmt.Errorf("%d rows %s have no node naming their project, so a private project's reads would answer them wrongly (%s): "+
		"envoy-dispatch backfill-graph-store writes their nodes, and envoy-dispatch census --store names them",
		len(incomplete), whose, strings.Join(incomplete[:min(len(incomplete), incompleteRowsNamed)], ", "))
}

// sessionProjectsFilled answers nil when session_projects_filled holds its row: every older event's
// session has its projects recorded, which an agent's visibility reads.
func sessionProjectsFilled(ctx context.Context, q graphstore.Queryer) error {
	var filled bool
	if err := q.QueryRow(ctx, `select exists (select 1 from session_projects_filled)`).Scan(&filled); err != nil {
		return fmt.Errorf("read session_projects_filled: %w", err)
	}
	if !filled {
		return fmt.Errorf("session_projects_filled holds no row, so the projects the sessions of earlier events acted in are not all recorded: run envoy-dispatch backfill-session-projects")
	}
	return nil
}

// ReportUnprovisionedPrivateProjects logs one ERROR line for each project the database holds
// private that list, the projects the deployment has provisioned as private, does not name
// (access.PrivateOffList), with both repairs, and answers their keys. Such a project stays
// private: its reads keep their rule, and only its owner's switch to public changes its
// visibility (patchProjectAccess).
func ReportUnprovisionedPrivateProjects(ctx context.Context, q access.Querier, list []string) ([]string, error) {
	keys, err := access.PrivateOffList(ctx, q, list)
	if err != nil {
		return nil, err
	}
	LogUnprovisionedPrivateProjects(keys)
	return keys, nil
}

// LogUnprovisionedPrivateProjects logs one ERROR line for each of keys, projects the database holds
// private that DISPATCH_PRIVATE_PROJECTS does not name, with both repairs.
func LogUnprovisionedPrivateProjects(keys []string) {
	for _, key := range keys {
		slog.Error(fmt.Sprintf("dispatch: private project not provisioned: %s is private but DISPATCH_PRIVATE_PROJECTS does not name it; "+
			"it stays private, and its visibility changes only by its owner's switch to public. "+
			"Repair: put %s back on agent-infra's legion.PrivateProjects, or make it public with `envoy-dispatch project-visibility %s public` (its owner decides which)",
			key, key, key), "project", key)
	}
}

// RunMaintenanceSweep deletes every Idempotency-Key past its window (sweepRequestKeys) and every
// spent DPoP proof past its acceptance window (store.DPoPProofs.Sweep), ends every answer run
// past its window that stored no terminal event and deletes every run past its TTL
// (sweepGoingRequests), and then sends every delivery a process that died left owed
// (resumeDeliveries, the server's resumeOwedDeliveries). resumeDeliveries is nil where nothing
// delivers. Every pass also logs an ERROR line for each project the database holds private that
// privateProjects, the projects the deployment has provisioned as private, does not name
// (ReportUnprovisionedPrivateProjects); where privateProjects names any, its first pass and then
// one pass an hour log an ERROR line when the store cannot yet hold a private project
// (StoreIncomplete). It stops when ctx is done.
//
// It is a timer and not a woken loop (duty.RunWoken), the shape every other sweep of this server
// took, because no commit announces any of its work: a key and a proof record pass their windows
// by the clock alone, an answer run passes its ends_at by the clock too, and a delivery is owed
// because the process that claimed it died, which commits nothing for a notification to carry.
// They share one loop rather than taking one each: a loop of its own would need a lease
// connection of its own (store.LeasePoolSize, whose headroom is zero) for statements that cost
// milliseconds.
//
// Each pass runs under maintenanceLease for one interval, so one process sweeps per interval
// however many serve the database and a second process's tick, which falls between the first's
// rather than on it, finds that interval served; leases is required, as it is of every leased
// loop (cmd/dispatch exits at boot if the lease pool cannot be opened). The period is interval
// itself and not MaintenanceSweepInterval: this loop ticks on what its caller asked for. The
// ticks are duty.RunLeased's, a ticker's: the first one interval after the call, then one each
// interval; the first tick a pass overran comes at once with the moment it was due, and only the
// ones after it are dropped. The sweeps fail apart: one's failure is logged and the others still
// run.
func RunMaintenanceSweep(
	ctx context.Context, pool *store.Pool, interval time.Duration, leases duty.Leases,
	resumeDeliveries func(context.Context) error, privateProjects []string,
) {
	var storeChecked time.Time
	duty.RunLeased(ctx, leases, maintenanceLease, interval, duty.TickAfterInterval, func(ctx context.Context) {
		if err := sweepRequestKeys(ctx, pool); err != nil {
			slog.Warn("dispatch: idempotency key sweep failed", "error", err)
		}
		if _, err := store.NewDPoPProofs(pool).Sweep(ctx); err != nil {
			slog.Warn("dispatch: DPoP proof sweep failed", "error", err)
		}
		if err := sweepGoingRequests(ctx, pool); err != nil {
			slog.Warn("dispatch: going request sweep failed", "error", err)
		}
		if _, err := ReportUnprovisionedPrivateProjects(ctx, pool, privateProjects); err != nil {
			slog.Warn("dispatch: private projects", "error", err)
		}
		if len(privateProjects) > 0 && time.Since(storeChecked) >= StoreCompletenessInterval {
			storeChecked = time.Now()
			if err := StoreIncomplete(ctx, pool); err != nil {
				slog.Error("dispatch: private projects: the store cannot hold a private project yet", "error", err)
			}
		}
		if resumeDeliveries == nil {
			return
		}
		if err := resumeDeliveries(ctx); err != nil {
			slog.Warn("dispatch: delivery sweep failed", "error", err)
		}
	})
}
