package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
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

// storeCompletenessInterval is how often a process's maintenance passes check, where the
// deployment names private projects, that the store can hold one (storeIncomplete), after the
// check the loop makes when it starts. A switch to private is refused until the project's own rows
// are complete; this names a row stored without its node afterwards. The read covers every
// project-owned table, so it runs hourly and not every pass.
const storeCompletenessInterval = time.Hour

// incompleteRowsNamed is how many of the incomplete rows a report names, the census's count
// (envoy-dispatch census --store names the same rows).
const incompleteRowsNamed = 20

// storeIncomplete answers nil when a private project's reads would answer no row wrongly: every
// row a project owns has a node naming its project, and no node stands behind no row
// (graphcensus.ProjectComplete), and both backfills a switch to private waits for have run
// (backfillMarkersMissing). Otherwise it answers why, naming the first rows and nodes, each with
// its cure, or the backfill not run and its command.
func storeIncomplete(ctx context.Context, q graphstore.Queryer) error {
	incomplete, err := graphcensus.ProjectComplete(ctx, q)
	if err != nil {
		return err
	}
	if len(incomplete) > 0 {
		return incompleteRowsError(incomplete, "rows and nodes a project owns")
	}
	return backfillMarkersMissing(ctx, q)
}

// incompleteRowsError says how many of the store's rows and nodes, those whose names ("rows of
// PRIV"), a private project's reads would answer wrongly, and names the first of them as the
// project census answers each line: with its own cure, the backfill, the erase that removes it, or
// none. It adds no cure of its own, since no one command cures every class.
func incompleteRowsError(incomplete []string, whose string) error {
	return fmt.Errorf("%d %s are incomplete in the graph store, so a private project's reads would answer them wrongly; "+
		"each names its own cure, and envoy-dispatch census --store names them all: %s",
		len(incomplete), whose, strings.Join(incomplete[:min(len(incomplete), incompleteRowsNamed)], ", "))
}

// backfillMarkersMissing answers nil when both backfills a switch to private waits for have
// written their marker row - backfill-session-projects (sessionProjectsFilled), then
// scrub-access-events (accessEventsScrubbed) - and otherwise why, naming the first one missing and
// its command. It is the one marker check of the store's report, at a process's start and hourly
// (storeIncomplete), and of the switch's 409 PRIVATE_PROJECTS_INCOMPLETE (privateProjectIncomplete).
// A marker missing answers an error errors.Is reads as errBackfillNotRun; a marker that could not
// be read answers the read's own error, which is not.
func backfillMarkersMissing(ctx context.Context, q graphstore.Queryer) error {
	if err := sessionProjectsFilled(ctx, q); err != nil {
		return err
	}
	return accessEventsScrubbed(ctx, q)
}

// errBackfillNotRun is a backfill a switch to private waits for that has not written its marker:
// what each missing marker's error is (backfillNotRun), so a caller tells it from a failed read.
var errBackfillNotRun = errors.New("a backfill a switch to private waits for has not run")

// backfillNotRun is a missing marker's error: its message is the sentence the report and the
// switch name it by, and it is errBackfillNotRun.
type backfillNotRun string

func (missing backfillNotRun) Error() string { return string(missing) }

func (backfillNotRun) Is(target error) bool { return target == errBackfillNotRun }

// sessionProjectsFilled answers nil when session_projects_filled holds its row: every older event's
// session has its projects recorded, which an agent's visibility reads.
func sessionProjectsFilled(ctx context.Context, q graphstore.Queryer) error {
	var filled bool
	if err := q.QueryRow(ctx, `select exists (select 1 from session_projects_filled)`).Scan(&filled); err != nil {
		return fmt.Errorf("read session_projects_filled: %w", err)
	}
	if !filled {
		return backfillNotRun("session_projects_filled holds no row, so the projects the sessions of earlier events acted in are not all recorded: run envoy-dispatch backfill-session-projects")
	}
	return nil
}

// accessEventsScrubbed answers nil when access_events_scrubbed holds its row: no stored
// project.access_changed event carries a project's access list any more.
func accessEventsScrubbed(ctx context.Context, q graphstore.Queryer) error {
	var scrubbed bool
	if err := q.QueryRow(ctx, `select exists (select 1 from access_events_scrubbed)`).Scan(&scrubbed); err != nil {
		return fmt.Errorf("read access_events_scrubbed: %w", err)
	}
	if !scrubbed {
		return backfillNotRun("access_events_scrubbed holds no row, so project.access_changed events stored before this release may still carry a project's access list: run envoy-dispatch scrub-access-events")
	}
	return nil
}

// logStoreIncomplete logs one ERROR line when the store cannot yet hold a private project
// (storeIncomplete), naming why and the command that repairs it.
func logStoreIncomplete(ctx context.Context, q graphstore.Queryer) {
	if err := storeIncomplete(ctx, q); err != nil {
		slog.Error("dispatch: private projects: the store cannot hold a private project yet; every switch to private is refused until it can", "error", err)
	}
}

// reportUnprovisionedPrivateProjects logs one ERROR line for each project the database holds
// private that list, every project the deployment names as one that may be private (both of its
// lists), does not name (access.PrivateOffList), with both repairs. Such a project stays private:
// its reads keep their rule, and only its owner's switch to public changes its visibility
// (patchProjectAccess).
func reportUnprovisionedPrivateProjects(ctx context.Context, q access.Querier, list []string) error {
	keys, err := access.PrivateOffList(ctx, q, list)
	if err != nil {
		return err
	}
	LogUnprovisionedPrivateProjects(keys)
	return nil
}

// LogUnprovisionedPrivateProjects logs one ERROR line for each of keys, projects the database holds
// private that neither DISPATCH_PRIVATE_PROJECTS nor DISPATCH_PRIVATE_BEFORE_FULL_SWITCH names,
// with both repairs.
func LogUnprovisionedPrivateProjects(keys []string) {
	for _, key := range keys {
		slog.Error(fmt.Sprintf("dispatch: private project not provisioned: %s is private but DISPATCH_PRIVATE_PROJECTS does not name it, nor DISPATCH_PRIVATE_BEFORE_FULL_SWITCH; "+
			"it stays private, and its visibility changes only by its owner's switch to public. "+
			"Repair: put %s back on the infrastructure repository's private project list (shared/legion/privateprojects/lists.go) or on DISPATCH_PRIVATE_BEFORE_FULL_SWITCH, or make it public with `envoy-dispatch project-visibility %s public` (its owner decides which)",
			key, key, key), "project", key)
	}
}

// RunMaintenanceSweep deletes every Idempotency-Key past its window (sweepRequestKeys) and every
// spent DPoP proof past its acceptance window (store.DPoPProofs.Sweep), ends every answer run
// past its window that stored no terminal event and deletes every run past its TTL
// (sweepGoingRequests), and then sends every delivery a process that died left owed
// (resumeDeliveries, the server's resumeOwedDeliveries). resumeDeliveries is nil where nothing
// delivers. Every pass also logs an ERROR line for each project the database holds private that
// neither privateProjects (DISPATCH_PRIVATE_PROJECTS) nor beforeFullSwitch
// (DISPATCH_PRIVATE_BEFORE_FULL_SWITCH) names (reportUnprovisionedPrivateProjects). Where either
// names a project, it logs an ERROR line when the store cannot yet hold a private project
// (storeIncomplete) once as it starts, before its first tick and on no lease, so each process
// reports its own start, and then in one pass an hour: the start's check is the boot's report,
// read beside the serving process rather than before it. A project private before its full
// switch keeps that check, which its switch needs as a full switch does. It stops when ctx is
// done.
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
	resumeDeliveries func(context.Context) error, privateProjects, beforeFullSwitch []string,
) {
	mayBePrivate := slices.Concat(privateProjects, beforeFullSwitch)
	var storeChecked time.Time
	if len(mayBePrivate) > 0 {
		storeChecked = time.Now()
		logStoreIncomplete(store.WithTransactionTracking(ctx), pool)
	}
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
		if err := reportUnprovisionedPrivateProjects(ctx, pool, mayBePrivate); err != nil {
			slog.Warn("dispatch: private projects", "error", err)
		}
		if len(mayBePrivate) > 0 && time.Since(storeChecked) >= storeCompletenessInterval {
			storeChecked = time.Now()
			logStoreIncomplete(ctx, pool)
		}
		if resumeDeliveries == nil {
			return
		}
		if err := resumeDeliveries(ctx); err != nil {
			slog.Warn("dispatch: delivery sweep failed", "error", err)
		}
	})
}
