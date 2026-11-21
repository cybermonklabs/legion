package access

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A developer box holds no credential that lists a private project after that project's full
// switch: every agent on a box runs as the box's own user, so nothing on the box can keep such a
// credential to one of them. Two kinds of credential a box can hold list projects, a personal
// `dsp_` token Dispatch makes and a token the secrets broker mints a machine login or a person's
// sign-in on a machine. One rule says which projects neither may list, closedSQL: a project in its
// closing state; a project private by its full switch (full_switch_at, RecordFullSwitch); and a
// private project whose key the deployment names among the projects it provisioned as private
// (DISPATCH_PRIVATE_PROJECTS). A project private on its first day, before its key is on that list,
// is open to them; it closes the moment its key joins the list, before its owner's switch.
//
//   - The membership answer says whether a project is closed (Answer), and the broker mints and
//     renews no developer-box token of a closed one.
//   - RefuseClosedTokenProjects refuses a personal token that would list a closed project.
//   - StartClosing, the first step of the owner's switch to private, revokes every live personal
//     token that lists the project, and the switch completes only once the broker counts no
//     developer-box token of it still live (internal/dispatch/api's closeForDeveloperBoxes). The
//     switch's own commit then records the full switch, which keeps the project closed.
//   - RevokeClosedProjectTokens, which the boot runs, revokes every live personal token that lists
//     a closed project, so a key that joins the list closes the project's personal tokens with the
//     boot that reads it rather than at its owner's next switch.
//
// projects.closing_since holds when the closing state began, on the database's clock, and is null
// otherwise; SetVisibility ends the state either way, completing the switch or abandoning it.

// ErrClosedTokenProject is a personal token that would list a project closed to developer boxes:
// Dispatch makes none.
var ErrClosedTokenProject = errors.New("no personal token lists a project that is private or turning private")

// closedSQL is the rule above over the projects row p, with listed the bind parameter of the keys
// the deployment provisioned as private: true when no developer-box credential may list p.
func closedSQL(p, listed string) string {
	return `(` + p + `.closing_since is not null or (` + p + `.visibility = 'private' and (` +
		p + `.full_switch_at is not null or ` + p + `.key = any(` + listed + `::text[]))))`
}

// Closing is a project's closing state as a switch to private reads it, under the project's owner
// lock: when its closing state began, nil when it is in none, beside the database's own time at
// the read, which the switch ages that start against, and the Slack channel ids the project holds.
type Closing struct {
	Since         *time.Time
	Now           time.Time
	SlackChannels []string
}

// ReadClosing answers project key's closing state, read in tx; ErrProjectNotFound for a key no
// project holds.
func ReadClosing(ctx context.Context, tx pgx.Tx, key string) (Closing, error) {
	var state Closing
	err := tx.QueryRow(ctx, `select closing_since, statement_timestamp(), slack_channels from projects where key = $1`, key).
		Scan(&state.Since, &state.Now, &state.SlackChannels)
	if errors.Is(err, pgx.ErrNoRows) {
		return Closing{}, fmt.Errorf("%w: %s", ErrProjectNotFound, key)
	}
	if err != nil {
		return Closing{}, fmt.Errorf("read the closing state of project %s: %w", key, err)
	}
	return state, nil
}

// OweBrokerForget writes, in q, that the secrets broker is owed the forget of its record of
// project key: the record numbered record, which a switch to private left in the broker's store
// and did not commit, or with record nil the project's record whatever its number, once the
// project is public. The forget is sent by the broker_forgets sender (internal/dispatch/api's
// sendBrokerForgets), at once, until the broker takes it.
func OweBrokerForget(ctx context.Context, q interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, key string, record *int64) error {
	if _, err := q.Exec(ctx, `insert into broker_forgets (project, record) values ($1, $2)`, key, record); err != nil {
		return fmt.Errorf("owe the secrets broker the forget of project %s: %w", key, err)
	}
	return nil
}

// StartClosing puts project key in its closing state, in tx and under the project's owner lock,
// which its caller holds (LockForAccessWrite): it sets closing_since to the statement's own start,
// revokes every live personal token that lists the project, and appends project.access_changed.
// It answers the project's access as tx left it, the event, and how many tokens it revoked.
//
// It holds the project's owner lock to its commit, so a personal token's mint or edit that lists
// the project, which takes that lock shared before it reads the project (RefuseClosedTokenProjects),
// either commits first, and its token is revoked here, or reads the closing state and is refused.
func StartClosing(ctx context.Context, tx pgx.Tx, ev *events.Broker, key string, by model.Actor) (model.ProjectAccess, model.Event, int64, error) {
	if err := lockProject(ctx, tx, key); err != nil {
		return model.ProjectAccess{}, model.Event{}, 0, err
	}
	started, err := tx.Exec(ctx, `update projects set closing_since = statement_timestamp() where key = $1`, key)
	if err != nil {
		return model.ProjectAccess{}, model.Event{}, 0, fmt.Errorf("start the closing state of project %s: %w", key, err)
	}
	if started.RowsAffected() == 0 {
		return model.ProjectAccess{}, model.Event{}, 0, fmt.Errorf("%w: %s", ErrProjectNotFound, key)
	}
	tag, err := tx.Exec(ctx, `update agent_tokens set revoked_at = now() where revoked_at is null and $1 = any(projects)`, key)
	if err != nil {
		return model.ProjectAccess{}, model.Event{}, 0, fmt.Errorf("revoke the personal tokens that list project %s: %w", key, err)
	}
	written, event, err := appendProjectChange(ctx, tx, ev, key, model.AccessVisibilityChanged, "", by)
	if err != nil {
		return model.ProjectAccess{}, model.Event{}, 0, err
	}
	return written, event, tag.RowsAffected(), nil
}

// RefuseClosedTokenProjects is the check a personal token's mint and its edit make, in the
// transaction that writes the token: it takes the owner lock of each of projects shared
// (store.LockOwnersShared), then answers ErrClosedTokenProject naming the first, in key order, that
// is closed to developer boxes (closedSQL, listed being the keys the deployment provisioned as
// private), nil when none is. The shared lock waits for StartClosing, which holds the project's
// owner lock exclusively to its commit, and holds off the next one until the token is written, so
// no token that lists the project commits after the closing state's revocation has run. projects
// are keys the caller sees, so the refusal names nothing the caller may not see.
func RefuseClosedTokenProjects(ctx context.Context, tx pgx.Tx, projects, listed []string) error {
	if len(projects) == 0 {
		return nil
	}
	owners := make([]store.Owner, 0, len(projects))
	for _, key := range projects {
		owners = append(owners, store.ProjectOwner(key))
	}
	if err := lockTimedOut(LockOwnerRow, store.LockOwnersShared(ctx, tx, owners...)); err != nil {
		return err
	}
	var closed string
	err := tx.QueryRow(ctx, `
		select coalesce(min(p.key) filter (where `+closedSQL("p", "$2")+`), '')
		from projects p where p.key = any($1)
	`, projects, listedKeys(listed)).Scan(&closed)
	if err != nil {
		return fmt.Errorf("read the projects a personal token lists: %w", err)
	}
	if closed != "" {
		return fmt.Errorf("%w: %s is one", ErrClosedTokenProject, closed)
	}
	return nil
}

// RevokeClosedProjectTokens revokes every live personal token that lists a project closed to
// developer boxes (closedSQL, listed being the keys the deployment provisioned as private), and
// answers the keys of those projects whose tokens it revoked, sorted, with how many it revoked.
// The boot runs it once it has read DISPATCH_PRIVATE_PROJECTS, so a project private on its first
// day loses its personal tokens with the first boot that names its key there.
func RevokeClosedProjectTokens(ctx context.Context, q Querier, listed []string) ([]string, int64, error) {
	var keys []string
	var revoked int64
	err := q.QueryRow(ctx, `
		with closed as (
			select p.key from projects p where `+closedSQL("p", "$1")+`
		), revoked as (
			update agent_tokens t set revoked_at = now()
			where t.revoked_at is null and t.projects && array(select key from closed)
			returning t.projects
		)
		select coalesce(array(select distinct k from revoked, unnest(revoked.projects) k
				where k in (select key from closed) order by k), '{}'),
			(select count(*) from revoked)
	`, listedKeys(listed)).Scan(&keys, &revoked)
	if err != nil {
		return nil, 0, fmt.Errorf("revoke the personal tokens of the projects closed to developer boxes: %w", err)
	}
	return keys, revoked, nil
}

// listedKeys is listed as a bind parameter: never nil, which pgx would send as SQL null and any()
// would then answer null for, not false.
func listedKeys(listed []string) []string {
	if listed == nil {
		return []string{}
	}
	return listed
}
