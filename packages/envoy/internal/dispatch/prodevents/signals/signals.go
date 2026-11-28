// Package signals writes the record's rows that no person and no system posts
// (dispatch://INFRA-978, R6 and R7): the GitHub adapter's deploy rows, one per deploy run of a
// deploy repository that the repository's rule calls a deploy, and the failures of the three
// automatic signals: a failed after-merge check (check_fail), a merged revert (revert), and an issue
// labelled incident that links the pull request or the run it blames (incident_issue).
//
// One woken loop under one lease (Lease) reads three feeds, each from a cursor of its own
// (production_signal_cursors, migration 20261123041501), as legionfacts reads its three:
//
//   - deploys: the delivery change log (delivery_changes), whose run, job and pull request changes
//     a run write, a job write or a pull request write of the intake, the job intake and the
//     reconcile log once it commits. Each moves the deploy rows of its repository (passDeploys).
//     After the rows land it writes the failures a landed deploy row completes: a revert's, once the
//     deploy that ships the revert has its row, and a check_fail whose result came before its
//     deploy.
//   - checks: the event log (events), whose result.recorded of a fail on an after_merge check makes
//     a check_fail, and whose result.retracted rejects the row its result made (passChecks).
//   - incidents: the event log, whose issue.created and issue.updated of an issue labelled incident
//     make an incident_issue from the issue's own links (passIncidents).
//
// Every write is a transaction of its own that takes the delivery change log's lock nowhere
// (prodevents), after the write that caused it has committed, so no transaction that takes that
// lock locks a production_events row. A feed's cursor moves after the rows of its page are written,
// and every write is idempotent by its source_ref, a check_fail by its result's binding as well
// (check_fail_bindings), so a page read again after a crash writes nothing twice. The record
// route's handlers write check_fail and its retraction's reject at once as well (api's
// recordCheckResult and retractCheckResult); the loop is what a write lost to a crash, or a result
// that came before its deploy, waits on.
package signals

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/prodevents"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// loopActor writes every row the loop writes but a check_fail and an incident_issue, which carry
// the actor of their result and of their event: the deploy rows, a revert's failure and its
// restore, and the re-count of a failure a drop rejected when a later rule calls its deploy a
// deploy again. It writes no reject: a drop's is prodevents.DropWriter's, so a later rule undoes
// that reject alone, and every other reject, a person's, a controller's or a retraction's, stays
// (prodevents.UpsertGitHubDeploy, RejectRetractedCheck).
var loopActor = model.Actor{Kind: "system", ID: "production-signals"}

// Lease names the lease every pass takes (duty.Exclusive with no period): two processes woken by
// one commit would otherwise derive the same repository's rows at once.
const Lease = "production-signals"

// PassRetry is how long a process that found the lease held waits, plus up to a tenth again,
// before it tries once more (duty.WokenLoop.Retry), and how far on a pass that failed asks for the
// next, its cursors standing where they were. It is also the longest a pass whose safe horizon held
// an event back waits for the next (horizonRetry doubling up to it).
const PassRetry = 10 * time.Second

// horizonRetry is how far on a pass whose safe horizon held an event back asks for the next, the
// first time in a row.
const horizonRetry = 250 * time.Millisecond

// RevertsChannel is the Postgres notification channel a write of delivery_reverts announces itself
// on at commit (migration 20261123041500): a revert the reconcile records after its deploy row
// landed makes its failure on the pass it wakes.
const RevertsChannel = "dispatch_delivery_reverts"

// page is the most rows a page of one feed reads.
const page = 500

// The names of the three cursors (production_signal_cursors.name).
const (
	deploysCursor   = "deploys"
	checksCursor    = "checks"
	incidentsCursor = "incidents"
)

// Deps is what Run needs of the server.
type Deps struct {
	Pool *store.Pool
	// Horizon is the process's one events.Horizon: the event feeds are paged by the events' global
	// id, so they stop at its safe horizon.
	Horizon *events.Horizon
	// Leases is the pool a pass takes its lease connection from (duty.Exclusive).
	Leases duty.Leases
	// Events is signalled when an event commits (api.EventsChannel): a result, a retraction, an
	// issue's labels and links, and a project.updated whose default repository moved a source.
	Events <-chan struct{}
	// Deliveries is signalled when the delivery change log moved (delivery.ChangesChannel).
	Deliveries <-chan struct{}
	// Settings is signalled when a project's delivery settings moved (delivery.SettingsChannel).
	Settings <-chan struct{}
	// Reverts is signalled when a revert is recorded (RevertsChannel).
	Reverts <-chan struct{}
}

// Run keeps the record's GitHub deploy rows and its automatic failures in step with their inputs
// until ctx ends: a woken loop (duty.RunWoken) under Lease, one pass at start, a pass on each wake,
// which starts as soon as the pass running when it came ends, a pass PassRetry after one that
// failed, a pass soon after one whose safe horizon held an event back, and no timer otherwise.
func Run(ctx context.Context, deps Deps) {
	ctx = store.WithTransactionTracking(ctx)
	held := &duty.Backoff{First: horizonRetry, Max: PassRetry}
	duty.RunWoken(ctx, duty.WokenLoop{
		Lease:  Lease,
		Leases: deps.Leases,
		Wake:   duty.MergeWakes(ctx, deps.Events, deps.Deliveries, deps.Settings, deps.Reverts),
		Retry:  PassRetry,
		Pass: func(ctx context.Context) time.Time {
			heldBack, err := pass(ctx, deps)
			switch {
			case ctx.Err() != nil:
				return time.Time{}
			case err != nil:
				slog.Warn("dispatch: the production signals' pass failed", "error", err)
				return time.Now().Add(PassRetry)
			case heldBack:
				return time.Now().Add(held.Next())
			}
			held.Reset()
			return time.Time{}
		},
	})
}

// pass is one pass of the loop: the deploys feed first, so a deploy row a page of the event feeds
// blames is there when it is read, then the two event feeds up to the safe horizon the pass reads
// once. It answers whether that horizon held an event back.
func pass(ctx context.Context, deps Deps) (bool, error) {
	if err := passDeploys(ctx, deps.Pool); err != nil {
		return false, fmt.Errorf("the deploys feed: %w", err)
	}
	safe, err := deps.Horizon.SafeOrAwait(ctx)
	if err != nil {
		return false, err
	}
	if err := passChecks(ctx, deps.Pool, safe); err != nil {
		return false, fmt.Errorf("the checks feed: %w", err)
	}
	if err := passIncidents(ctx, deps.Pool, safe); err != nil {
		return false, fmt.Errorf("the incidents feed: %w", err)
	}
	return deps.Horizon.HeldBack(safe), nil
}

// cursor is one row of production_signal_cursors: the position, the time of the change at it (the
// deploys feed's), and its stamp. held is false where no row exists, a feed this database has read
// nothing of.
type cursor struct {
	position int64
	at       *time.Time
	stamped  time.Time
	held     bool
}

// readCursor reads the cursor name.
func readCursor(ctx context.Context, q delivery.Querier, name string) (cursor, error) {
	var c cursor
	err := q.QueryRow(ctx, `select position, at, updated_at from production_signal_cursors where name = $1`, name).
		Scan(&c.position, &c.at, &c.stamped)
	if errors.Is(err, pgx.ErrNoRows) {
		return cursor{}, nil
	}
	if err != nil {
		return cursor{}, fmt.Errorf("read the %s cursor: %w", name, err)
	}
	c.held = true
	return c, nil
}

// moveCursor writes the cursor name at position, with the time of the change there and stamped.
func moveCursor(ctx context.Context, pool *store.Pool, name string, position int64, at *time.Time, stamped time.Time) error {
	if _, err := pool.Exec(ctx, `
		insert into production_signal_cursors (name, position, at, updated_at) values ($1, $2, $3, $4)
		on conflict (name) do update set position = excluded.position, at = excluded.at, updated_at = excluded.updated_at
	`, name, position, at, stamped); err != nil {
		return fmt.Errorf("move the %s cursor: %w", name, err)
	}
	return nil
}

// The write every automatic signal makes of its failure, the check_fail (RecordCheckFailure), the
// incident_issue (recordIncident) and the revert (revertFailures): writeSignal writes the row only
// where the deploy it blames counts for the row's project, judged in the write's transaction, and
// keeps for good the signal's refusal of a deploy that counts for other projects alone
// (dispatch://INFRA-978/comment/d57e8d06-afb3-4456-9e3d-516923e2b706).

// errCountsElsewhere is deployCountsForRow's refusal of the deploy an automatic signal's row blames
// where that deploy counts for other projects than the row's alone: writeSignal refuses the signal
// on that deploy for good.
var errCountsElsewhere = errors.New("the blamed deploy counts for other projects than the row's alone")

// refusedMessage is the line each refusal of a signal logs (commitRefusal).
const refusedMessage = "dispatch: an automatic signal is refused on the deploy it blames and makes no failure of that deploy, now or later"

// refusal is how an automatic signal keeps its refusal of a deploy it blames, in its own table
// (check_fail_refusals, incident_refusals, revert_refusals; migration 20261128030142): signal and
// key name the signal as that table keys it, in the refusal's log line, and record inserts the
// signal's refusal of deploy in tx, which it does not commit.
type refusal struct {
	signal string
	key    string
	record func(ctx context.Context, tx pgx.Tx, deploy string) error
}

// commitRefusal records in tx, and commits, refused's refusal of deploy, the deploy its signal
// blames, and logs it with the signal's key, the deploy and why: the signal makes no row blaming
// that deploy, then or on any later write of it, whatever makes the deploy count for the row's
// project later.
func commitRefusal(ctx context.Context, tx pgx.Tx, refused refusal, deploy, why string) error {
	if err := refused.record(ctx, tx, deploy); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit the refusal of %s %s on deploy %s: %w", refused.signal, refused.key, deploy, err)
	}
	slog.Info(refusedMessage, "signal", refused.signal, "key", refused.key, "deploy", deploy, "reason", why)
	return nil
}

// writeSignal writes event, an automatic signal's failure, in tx, the transaction its caller began
// and read the signal in, which takes the delivery change log's lock nowhere, then commits tx and,
// where it created the row, drains the change rows the write left in the outbox, as
// prodevents.Write does; it reports whether it created the row. The row is written only where the
// deploy it blames counts for its project (countsForRow). bind, where not nil, runs in tx before
// the commit, on the row written or on the row the project holds under event's source_ref already.
// Where the blamed deploy counts for other projects alone, refused records the signal's refusal of
// it in tx, which commits it (commitRefusal), so no later write of the signal makes a row blaming
// that deploy. Where it counts for no project, as a deploy a rule change dropped since the signal's
// lookup does, or where its row is gone since then, the write records nothing and commits nothing:
// the signal stays unmatched, and a later write of it asks again.
func writeSignal(ctx context.Context, pool *store.Pool, tx pgx.Tx, event prodevents.Event, refused refusal,
	bind func(context.Context, pgx.Tx, prodevents.Event) error) (bool, error) {
	blamed := *event.DeployID
	row, created, err := prodevents.WriteIn(ctx, tx, event, countsForRow(event.ProjectKey, blamed))
	switch {
	case errors.Is(err, errCountsElsewhere):
		return false, commitRefusal(ctx, tx, refused, blamed, "it counts for other projects than "+event.ProjectKey+" alone")
	case blamesNoDeploy(err):
		return false, nil
	case err != nil:
		return false, err
	}
	if bind != nil {
		if err := bind(ctx, tx, row); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit production event %s of %s: %w", event.SourceRef, event.ProjectKey, err)
	}
	if created {
		delivery.DrainAfterCommit(ctx, pool, "drain the change rows of an automatic signal's write")
	}
	return created, nil
}

// countsForRow is the question an automatic signal's write of a row of project asks of the deploy
// it blames, blamed (prodevents.DeployCheck), in the write's transaction while the store holds that
// deploy's row for key share: deployCountsForRow. A restoring deploy the row names is asked
// nothing.
func countsForRow(project, blamed string) prodevents.DeployCheck {
	return func(ctx context.Context, tx pgx.Tx, deploy prodevents.Event) error {
		if deploy.ID != blamed {
			return nil
		}
		return deployCountsForRow(ctx, tx, project, deploy)
	}
}

// deployCountsForRow reads in tx whether deploy, the deploy an automatic signal's row of project
// blames, counts for project: the projects it counts for by the rule the failure measures count a
// deploy by (prodevents.DeployProjects), over the stored facts as the write's transaction reads
// them (storedDeployFacts), and not the refresher's published copy of them (eval_days_rules,
// eval_days_defaults), so a row whose deploy's rule is saved and not yet published is written, and
// counts once the refresher's next pass publishes it
// (dispatch://INFRA-978/comment/6da2d490-27d2-4d03-816a-a164a8d24dde). It answers nil where they
// hold project, and errCountsElsewhere where they hold other projects alone. Where they hold none,
// as for a dropped deploy, it answers prodevents.NoDeploy, the store's refusal of a deploy row gone
// since the signal's lookup: only a deploy that counted for other projects alone refuses a signal
// for good (dispatch://INFRA-978/comment/d57e8d06-afb3-4456-9e3d-516923e2b706), and one that counts
// for none may count for the row's project once a later rule calls it a deploy again.
func deployCountsForRow(ctx context.Context, tx pgx.Tx, project string, deploy prodevents.Event) error {
	facts, err := storedDeployFacts(ctx, tx, deploy)
	if err != nil {
		return err
	}
	switch projects := prodevents.DeployProjects(deploy, facts); {
	case slices.Contains(projects, project):
		return nil
	case len(projects) == 0:
		return prodevents.NoDeploy(deploy.ID)
	}
	return errCountsElsewhere
}

// storedDeployFacts reads in q the facts deploy is counted by (prodevents.DeployFacts) from what is
// stored: each project's default_repo, the project of the issue of each pull request deploy ships
// (a pull request no issue Dispatch holds attributes counts for none, so for the projects whose
// default repository is the deploy's), and, where it ships no pull request, the project whose
// deploy source reads its repository by the stored settings' rules (delivery.SourceRules). A posted
// and a dropped deploy read none of it.
func storedDeployFacts(ctx context.Context, q delivery.Querier, deploy prodevents.Event) (prodevents.DeployFacts, error) {
	facts := prodevents.DeployFacts{Defaults: map[string]string{}}
	if deploy.Source != prodevents.SourceGitHub || deploy.Dropped() {
		return facts, nil
	}
	var repos []string
	var numbers []int32
	for _, ref := range deploy.PullRequests {
		if repo, number, ok := prodevents.ParsePullRequest(ref); ok {
			repos, numbers = append(repos, repo), append(numbers, int32(number))
		}
	}
	if len(repos) == 0 {
		rules, err := delivery.ReadSourceRules(ctx, q)
		if err != nil {
			return prodevents.DeployFacts{}, err
		}
		if rule, read := rules.For(deploy.Repo); read {
			facts.Source = rule.Project
		}
	} else {
		rows, err := q.Query(ctx, `
			select coalesce(issue.project_key, '')
			from unnest($1::text[], $2::int[]) with ordinality as given(repo, number, at)
			left join delivery_pull_requests request on lower(request.repo) = lower(given.repo) and request.number = given.number
			left join issues issue on issue.key = request.issue_key
			order by given.at`, repos, numbers)
		if err != nil {
			return prodevents.DeployFacts{}, fmt.Errorf("read the projects of the pull requests deploy %s ships: %w", deploy.ID, err)
		}
		terminals, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return prodevents.DeployFacts{}, fmt.Errorf("read the projects of the pull requests deploy %s ships: %w", deploy.ID, err)
		}
		for _, terminal := range terminals {
			facts.Pulls = append(facts.Pulls, prodevents.PullAttribution{Terminal: terminal})
		}
	}
	rows, err := q.Query(ctx, `select key, coalesce(default_repo, '') from projects`)
	if err != nil {
		return prodevents.DeployFacts{}, fmt.Errorf("read the projects' default repositories: %w", err)
	}
	var project, repo string
	if _, err := pgx.ForEachRow(rows, []any{&project, &repo}, func() error {
		facts.Defaults[project] = repo
		return nil
	}); err != nil {
		return prodevents.DeployFacts{}, fmt.Errorf("read the projects' default repositories: %w", err)
	}
	return facts, nil
}

// blamesNoDeploy reports whether err is the store's refusal of the deploy an automatic row blames
// as no deploy row (prodevents.CodeNoDeploy): a deploy row gone since the signal's lookup, or a
// deploy that counts for no project (deployCountsForRow). The signal writes no row of it, as it
// writes none where no deploy holds what it names, and is not refused.
func blamesNoDeploy(err error) bool {
	var refused *prodevents.Error
	return errors.As(err, &refused) && refused.Code == prodevents.CodeNoDeploy
}
