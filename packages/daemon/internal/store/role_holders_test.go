package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// holderIssue is the issue every claim of this file works, in its own tree.
const holderIssue = "LEGION-301"

// holderClaim is the tester claim of holderIssue in state, with session.
func holderClaim(state supervise.ClaimState, session string) supervise.Claim {
	return supervise.Claim{
		Token: "legion-legion-legion-301-tester", Project: "legion", Tree: holderIssue, Issue: holderIssue,
		Role: claim.RoleTester, Generation: 1, State: state, Session: session,
	}
}

// queuedRoleHolders is the issue of each role_holders_publish row the outbox holds, in id order.
// Each payload is decoded as the daemon's executor decodes it (record.DecodeOutboxPayload), so a
// row this package writes in a shape the executor cannot read fails here rather than in the lane.
func queuedRoleHolders(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.pool.Query(context.Background(), `select id, kind, issue, payload from outbox where kind = $1 order by id`,
		string(record.OutboxKindRoleHoldersPublish))
	if err != nil {
		t.Fatalf("read the queued role-holder publications: %v", err)
	}
	queued, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (record.OutboxRow, error) {
		var r record.OutboxRow
		var kind string
		err := row.Scan(&r.ID, &kind, &r.Issue, &r.Payload)
		r.Kind = record.OutboxKind(kind)
		return r, err
	})
	if err != nil {
		t.Fatalf("read the queued role-holder publications: %v", err)
	}
	issues := []string{}
	for _, row := range queued {
		payload, err := record.DecodeOutboxPayload(row)
		if err != nil {
			t.Fatalf("decode the role-holder publication of %s as the executor does: %v", row.Issue, err)
		}
		if payload != (record.RoleHoldersPublish{Issue: row.Issue}) {
			t.Fatalf("the role-holder publication of %s carries %+v, want the issue alone", row.Issue, payload)
		}
		issues = append(issues, row.Issue)
	}
	return issues
}

// sentRoleHolders is the lane's run of every queued role-holder publication: the rows go, as a
// lane that sent each one deletes it.
func sentRoleHolders(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(), `delete from outbox where kind = $1`, string(record.OutboxKindRoleHoldersPublish)); err != nil {
		t.Fatalf("send the queued role-holder publications: %v", err)
	}
}

// publishingStore is a migrated store whose claim writes queue the role-holder publication, as the
// store of a daemon with a workflow does (PublishRoleHolders).
func publishingStore(t *testing.T) *Store {
	t.Helper()
	s := migratedStore(t)
	s.PublishRoleHolders(nil)
	return s
}

// The publication of an issue's role sessions is queued in the transaction of the claim write
// that changes the session holding a role: the row the write replaces named one holder and the
// row it writes names another. A claim's session holds its role in the states
// supervise.HoldsCapability answers true for, so the agent's registration, a new session while
// the role is held, and each end — retirement, failure, suspension and a move back to launching —
// queue it, through every write that stores a claim: PutClaim, PutClaimAndDelivery,
// RetireDelivery and AdmitClaim. A write that changes no holder queues nothing: a ready, a turn's
// start and end, and a second write of an ended claim. A pending row of the issue is made due
// again rather than queued beside, so the outbox never holds two.
func TestAClaimWriteThatChangesTheRoleHolderQueuesThePublicationWithIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := publishingStore(t)
	openIssueLifecycle(t, s, holderIssue)
	step := func(name string, write func() error, want []string) {
		t.Helper()
		if err := write(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := queuedRoleHolders(t, s); !slices.Equal(got, want) {
			t.Fatalf("%s queued the role-holder publications of %v, want %v", name, got, want)
		}
		sentRoleHolders(t, s)
	}
	queued, none := []string{holderIssue}, []string{}
	put := func(c supervise.Claim) func() error { return func() error { return s.PutClaim(ctx, c) } }

	step("the admission of a claim that has not launched", func() error {
		_, err := s.AdmitClaim(ctx, holderClaim(supervise.StateLaunching, ""))
		return err
	}, none)
	step("the registration", put(holderClaim(supervise.StateRegistered, "ses-1")), queued)
	step("the ready", put(holderClaim(supervise.StateReady, "ses-1")), none)
	step("a turn's start", put(holderClaim(supervise.StateWorking, "ses-1")), none)
	step("a turn's end", put(holderClaim(supervise.StateIdle, "ses-1")), none)
	step("a new session while the role is held", put(holderClaim(supervise.StateIdle, "ses-2")), queued)
	step("an admission over a row the store still holds live", func() error {
		_, err := s.AdmitClaim(ctx, holderClaim(supervise.StateLaunching, "ses-2"))
		return err
	}, queued)
	step("the registration of the admitted claim", put(holderClaim(supervise.StateRegistered, "ses-2")), queued)

	task := supervise.Delivery{ID: "delivery-1", Task: "test it", QueuedAt: at(1), DeliveredAt: at(2), ConfirmedAt: at(3), Generation: 1}
	step("a confirmed task", func() error {
		return s.PutClaimAndDelivery(ctx, holderClaim(supervise.StateWorking, "ses-2"), task)
	}, none)
	step("a failure written with its task", func() error {
		return s.PutClaimAndDelivery(ctx, holderClaim(supervise.StateFailed, "ses-2"), task)
	}, queued)
	step("a registration over the failure", put(holderClaim(supervise.StateRegistered, "ses-3")), queued)
	step("a retirement written with the retirement of its task", func() error {
		return s.RetireDelivery(ctx, holderClaim(supervise.StateRetired, "ses-3"), task.ID)
	}, queued)
	step("a second write of the retired claim", put(holderClaim(supervise.StateRetired, "ses-3")), none)
	step("a registration over the retirement", put(holderClaim(supervise.StateRegistered, "ses-4")), queued)
	step("a suspension", put(holderClaim(supervise.StateSuspended, "ses-4")), queued)

	// Two changes before the lane sends either leave one row.
	if err := s.PutClaim(ctx, holderClaim(supervise.StateRegistered, "ses-5")); err != nil {
		t.Fatalf("the registration after the suspension: %v", err)
	}
	if err := s.PutClaim(ctx, holderClaim(supervise.StateRetired, "ses-5")); err != nil {
		t.Fatalf("the retirement after it: %v", err)
	}
	if got := queuedRoleHolders(t, s); !slices.Equal(got, queued) {
		t.Fatalf("two changes of one issue queued %v, want the one row of %s", got, holderIssue)
	}
}

// The controller holds no role of an issue, and neither does a claim on no issue, so no write of
// either queues a publication.
func TestAClaimOfNoIssueRoleQueuesNoPublication(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := publishingStore(t)
	controller := supervise.Claim{Token: claim.ControllerToken("legion"), Project: "legion", Role: claim.RoleController,
		Generation: 1, State: supervise.StateRegistered, Session: "ses-controller"}
	if err := s.PutClaim(ctx, controller); err != nil {
		t.Fatalf("the controller's registration: %v", err)
	}
	noIssue := supervise.Claim{Token: "legion-operator-tree-implementer", Project: "legion", Tree: "operator-tree",
		Role: claim.RoleImplementer, Generation: 1, State: supervise.StateRegistered, Session: "ses-operator"}
	if err := s.PutClaim(ctx, noIssue); err != nil {
		t.Fatalf("the registration of a claim on no issue: %v", err)
	}
	if got := queuedRoleHolders(t, s); len(got) != 0 {
		t.Fatalf("the writes of the controller and of a claim on no issue queued %v, want nothing", got)
	}
}

// A claim of another project's issue — the operator's spawn of it, which runs under this project's
// token — holds no role this store publishes (queuesRoleHolders, claim.KeyOfProject): a row keyed
// by that issue is run only by the other project's daemon, against that daemon's own claims. So no
// write of the claim queues a publication, and each leaves the row that daemon holds of the issue
// as it stands, its lease and its due time unchanged. The same writes of a claim of this project's
// issue queue its own.
func TestAClaimOfAnotherProjectsIssueQueuesNoPublication(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := publishingStore(t)
	const theirs = "OTHERPROJECT-9"
	spawn := func(state supervise.ClaimState, session string) supervise.Claim {
		return supervise.Claim{Token: "legion-legion-otherproject-9-architect", Project: "legion", Tree: theirs, Issue: theirs,
			Role: claim.RoleArchitect, Generation: 1, State: state, Session: session}
	}
	// The other project's daemon is sending its issue's publication: it holds the row's lease.
	if _, err := s.pool.Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error, lease_token, lease_until)
		values ($1, $2, jsonb_build_object('issue', $2::text), 0, now() + interval '1 minute', '', 'otherproject-lease', now() + interval '5 minutes')`,
		roleHoldersPublishKind, theirs); err != nil {
		t.Fatalf("queue the other project's daemon's publication of %s: %v", theirs, err)
	}
	held := func() string {
		t.Helper()
		var row string
		if err := s.pool.QueryRow(ctx, `select string_agg(concat_ws(' ', id, next_at, attempts, lease_token, lease_until), '; ')
			from outbox where kind = $1 and issue = $2`, roleHoldersPublishKind, theirs).Scan(&row); err != nil {
			t.Fatalf("read the publication rows of %s: %v", theirs, err)
		}
		return row
	}
	before := held()
	for _, c := range []supervise.Claim{
		spawn(supervise.StateRegistered, "ses-1"), spawn(supervise.StateSuspended, "ses-1"),
		spawn(supervise.StateRegistered, "ses-2"), spawn(supervise.StateRetired, "ses-2"),
	} {
		if err := s.PutClaim(ctx, c); err != nil {
			t.Fatalf("write the %s claim of %s: %v", c.State, theirs, err)
		}
		if after := held(); after != before {
			t.Errorf("after the %s write of the claim of %s its publication rows are %q, want the other project's daemon's row as it stood, %q",
				c.State, theirs, after, before)
		}
	}

	openIssueLifecycle(t, s, holderIssue)
	if _, err := s.AdmitClaim(ctx, holderClaim(supervise.StateLaunching, "")); err != nil {
		t.Fatalf("admit the claim of %s: %v", holderIssue, err)
	}
	if err := s.PutClaim(ctx, holderClaim(supervise.StateRegistered, "ses-1")); err != nil {
		t.Fatalf("the registration of the claim of %s: %v", holderIssue, err)
	}
	if got, want := queuedRoleHolders(t, s), []string{theirs, holderIssue}; !slices.Equal(got, want) {
		t.Fatalf("the outbox holds the role-holder publications of %v, want %v: the other project's daemon's, and the one this project's registration queued", got, want)
	}
}

// A store the daemon has not turned the publication on for queues nothing, whatever its claim
// writes change: a daemon with no dispatch_url opens no workflow and so runs no outbox, and a row
// queued there would be neither sent nor deleted. The same writes on a store that publishes queue
// one row, the control that these writes are ones that change a holder.
func TestAStoreThatDoesNotPublishQueuesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, publishes := range []bool{false, true} {
		s := migratedStore(t)
		if publishes {
			s.PublishRoleHolders(nil)
		}
		openIssueLifecycle(t, s, holderIssue)
		if _, err := s.AdmitClaim(ctx, holderClaim(supervise.StateLaunching, "")); err != nil {
			t.Fatalf("admit the claim: %v", err)
		}
		for _, c := range []supervise.Claim{holderClaim(supervise.StateRegistered, "ses-1"), holderClaim(supervise.StateRetired, "ses-1")} {
			if err := s.PutClaim(ctx, c); err != nil {
				t.Fatalf("write the %s claim: %v", c.State, err)
			}
		}
		want := []string{}
		if publishes {
			want = []string{holderIssue}
		}
		if got := queuedRoleHolders(t, s); !slices.Equal(got, want) {
			t.Fatalf("a registration and a retirement on a store that publishes: %t queued %v, want %v", publishes, got, want)
		}
	}
}

// transactionsBegun counts the transactions a pool's connections begin: each `begin` statement
// pgx sends, which is the first statement of every transaction the store's Tx opens.
type transactionsBegun struct{ begun atomic.Int64 }

func (c *transactionsBegun) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(data.SQL)), "begin") {
		c.begun.Add(1)
	}
	return ctx
}

func (c *transactionsBegun) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// tracedStore is a store on s's database whose pool counts the transactions it begins, with the
// role-holder publication on when publishes is.
func tracedStore(t *testing.T, s *Store, publishes bool) (*Store, *transactionsBegun) {
	t.Helper()
	config := s.pool.Config()
	counter := &transactionsBegun{}
	config.ConnConfig.Tracer = counter
	raw, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("open the traced pool: %v", err)
	}
	t.Cleanup(raw.Close)
	traced := &Store{pool: NewPool(raw)}
	if publishes {
		traced.PublishRoleHolders(nil)
	}
	return traced, counter
}

// Every claim write opens one transaction, including a write that can queue no publication. Two
// conditions make such a write: a store the daemon has not turned the publication on for (each
// claim write of a daemon with no dispatch_url), and a claim that holds no role of an issue (each
// write of the project's controller, of a claim on no issue, or of another project's issue).
// The last row can queue, so it also checks that the transaction writes the publication with the claim.
func TestEveryClaimWriteOpensOneTransaction(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := migratedStore(t)
	quiet, quietBegun := tracedStore(t, s, false)
	publishing, publishingBegun := tracedStore(t, s, true)
	controller := supervise.Claim{Token: claim.ControllerToken("legion"), Project: "legion", Role: claim.RoleController,
		Generation: 1, State: supervise.StateRegistered, Session: "ses-controller"}
	noIssue := supervise.Claim{Token: "legion-operator-tree-implementer", Project: "legion", Tree: "operator-tree",
		Role: claim.RoleImplementer, Generation: 1, State: supervise.StateRegistered, Session: "ses-operator"}
	otherProject := supervise.Claim{Token: "legion-legion-otherproject-9-architect", Project: "legion", Tree: "OTHERPROJECT-9",
		Issue: "OTHERPROJECT-9", Role: claim.RoleArchitect, Generation: 1, State: supervise.StateRegistered, Session: "ses-spawn"}
	for _, write := range []struct {
		name  string
		store *Store
		begun *transactionsBegun
		claim supervise.Claim
		want  int64
	}{
		{"the tester's registration on a store that does not publish", quiet, quietBegun, holderClaim(supervise.StateRegistered, "ses-1"), 1},
		{"the controller's registration on a store that publishes", publishing, publishingBegun, controller, 1},
		{"the registration of a claim on no issue on a store that publishes", publishing, publishingBegun, noIssue, 1},
		{"the registration of a claim of another project's issue on a store that publishes", publishing, publishingBegun, otherProject, 1},
		{"the tester's retirement on a store that publishes", publishing, publishingBegun, holderClaim(supervise.StateRetired, "ses-1"), 1},
	} {
		before := write.begun.begun.Load()
		if err := write.store.PutClaim(ctx, write.claim); err != nil {
			t.Fatalf("%s: %v", write.name, err)
		}
		if got := write.begun.begun.Load() - before; got != write.want {
			t.Errorf("%s began %d transactions, want %d", write.name, got, write.want)
		}
		var state string
		if err := s.pool.QueryRow(ctx, `select state from claims where token = $1`, string(write.claim.Token)).Scan(&state); err != nil {
			t.Fatalf("read the row %s wrote: %v", write.name, err)
		}
		if state != string(write.claim.State) {
			t.Errorf("after %s the row reads %s, want %s", write.name, state, write.claim.State)
		}
	}
	if got := queuedRoleHolders(t, s); !slices.Equal(got, []string{holderIssue}) {
		t.Fatalf("the writes queued %v, want the retirement's publication of %s alone", got, holderIssue)
	}
}

// Once the transaction of a claim write that queued a publication commits, the store tells the
// issue (PublishRoleHolders), and the daemon wakes the issue's board lane there. The row is
// already visible to another connection when the store tells, since the lane reads it on one of
// its own. A write that queues nothing tells nobody, and neither does a write whose transaction
// rolled back: it queued nothing that stands.
func TestTheStoreTellsTheIssueOnceItsQueuedPublicationCommits(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := publishingStore(t)
	openIssueLifecycle(t, s, holderIssue)
	if _, err := s.AdmitClaim(ctx, holderClaim(supervise.StateLaunching, "")); err != nil {
		t.Fatalf("admit the claim: %v", err)
	}
	type tell struct {
		issue   string
		visible int
	}
	var told []tell
	s.PublishRoleHolders(func(issue string) {
		var visible int
		if err := s.pool.QueryRow(context.Background(), `select count(*) from outbox where kind = $1 and issue = $2`,
			string(record.OutboxKindRoleHoldersPublish), issue).Scan(&visible); err != nil {
			t.Errorf("read the queued row of %s when the store told it: %v", issue, err)
		}
		told = append(told, tell{issue, visible})
	})

	if err := s.PutClaim(ctx, holderClaim(supervise.StateRegistered, "ses-1")); err != nil {
		t.Fatalf("the registration: %v", err)
	}
	if want := []tell{{holderIssue, 1}}; !slices.Equal(told, want) {
		t.Fatalf("the registration told %+v, want %+v: the issue once, with its row committed", told, want)
	}
	sentRoleHolders(t, s)
	if err := s.PutClaim(ctx, holderClaim(supervise.StateReady, "ses-1")); err != nil {
		t.Fatalf("the ready: %v", err)
	}
	// A generation no bigint holds: the delivery's write is refused inside the transaction that
	// queued the end's publication, so both roll back.
	refused := supervise.Delivery{ID: "delivery-1", Task: "test it", QueuedAt: at(1), Generation: 1 << 63}
	if err := s.PutClaimAndDelivery(ctx, holderClaim(supervise.StateFailed, "ses-1"), refused); err == nil {
		t.Fatal("wrote a delivery whose generation no bigint holds")
	}
	if len(told) != 1 {
		t.Fatalf("a ready and a rolled-back end told %+v, want nothing past the registration", told[1:])
	}
	if got := queuedRoleHolders(t, s); len(got) != 0 {
		t.Fatalf("the rolled-back end left the role-holder publications of %v, want none", got)
	}
}

// refuseRoleHoldersQueue makes the outbox refuse every role_holders_publish row from now on, as a
// database that cannot take the publication's row does.
func refuseRoleHoldersQueue(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(), `
		create function refuse_role_holders_publish() returns trigger language plpgsql as $$
		begin raise exception 'the outbox refuses the role-holder publication of %', new.issue using errcode = 'P0001'; end $$;
		create trigger refuse_role_holders_publish before insert or update on outbox
			for each row when (new.kind = 'role_holders_publish') execute function refuse_role_holders_publish();`); err != nil {
		t.Fatalf("make the outbox refuse the role-holder publication: %v", err)
	}
}

// A claim write that changes the role holder and the queue of its publication commit together or
// not at all: when the outbox refuses the row, each of the four writes fails and the claim row is
// the row it was, so no change of a holder stands that nothing owes a publication. The machine
// owes such a write as it owes any other (supervise's writeOwed), and its repair queues the row
// when it lands. A write that changes no holder queues nothing, so the refusal does not reach it.
func TestAClaimWriteAndThePublicationItQueuesCommitTogether(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := publishingStore(t)
	openIssueLifecycle(t, s, holderIssue)
	idle := holderClaim(supervise.StateIdle, "ses-1")
	task := supervise.Delivery{ID: "delivery-1", Task: "test it", QueuedAt: at(1), DeliveredAt: at(2), ConfirmedAt: at(3), Generation: 1}
	if err := s.PutClaimAndDelivery(ctx, idle, task); err != nil {
		t.Fatalf("seed the idle claim with its task: %v", err)
	}
	sentRoleHolders(t, s)
	refuseRoleHoldersQueue(t, s)

	if err := s.PutClaim(ctx, holderClaim(supervise.StateWorking, "ses-1")); err != nil {
		t.Fatalf("a turn's start, which changes no holder, failed on the refused queue: %v", err)
	}
	if err := s.PutClaim(ctx, idle); err != nil {
		t.Fatalf("a turn's end, which changes no holder, failed on the refused queue: %v", err)
	}
	for _, write := range []struct {
		name string
		call func() error
	}{
		{"PutClaim", func() error { return s.PutClaim(ctx, holderClaim(supervise.StateSuspended, "ses-1")) }},
		{"PutClaimAndDelivery", func() error {
			return s.PutClaimAndDelivery(ctx, holderClaim(supervise.StateFailed, "ses-1"), task)
		}},
		{"RetireDelivery", func() error { return s.RetireDelivery(ctx, holderClaim(supervise.StateRetired, "ses-1"), task.ID) }},
		{"AdmitClaim", func() error {
			_, err := s.AdmitClaim(ctx, holderClaim(supervise.StateLaunching, "ses-1"))
			return err
		}},
	} {
		err := write.call()
		var refused *pgconn.PgError
		if !errors.As(err, &refused) || refused.Code != "P0001" {
			t.Fatalf("%s of an end whose publication the outbox refused answered %v, want the refusal", write.name, err)
		}
		stored := onlyClaim(t, s)
		if stored.State != supervise.StateIdle || stored.Session != "ses-1" || stored.Pending == nil || stored.Pending.ID != task.ID {
			t.Fatalf("after %s's refused queue the claim row reads %s with session %q and task %+v, want the idle row with its task it was",
				write.name, stored.State, stored.Session, stored.Pending)
		}
	}
	if got := queuedRoleHolders(t, s); len(got) != 0 {
		t.Fatalf("the refused writes left the role-holder publications of %v, want none", got)
	}
}
