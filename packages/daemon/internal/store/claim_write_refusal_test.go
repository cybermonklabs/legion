package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// wantRefusedRow asserts err is a refusal the store made itself, in Go, under rule: an error of
// supervise's refusal type (supervise.RowRefusedError), which supervise's rule reads as refused
// (supervise.SortClaimWriteFailure), so the machine neither owes the write nor makes it again.
func wantRefusedRow(t *testing.T, err error, rule string) {
	t.Helper()
	if err == nil {
		t.Fatalf("the write landed, want the store's own refusal under %q", rule)
	}
	var refusal *supervise.RowRefusedError
	if !errors.As(err, &refusal) {
		t.Fatalf("the write's error %v (%T) carries no supervise.RowRefusedError, want the refusal under %q", err, err, rule)
	}
	if refusal.Rule != rule {
		t.Fatalf("the refusal carries rule %q, want %q: %v", refusal.Rule, rule, err)
	}
	if got := supervise.SortClaimWriteFailure(err); got != supervise.WriteRefused {
		t.Fatalf("the rule sorts the store's own refusal %v as %v, want refused", err, got)
	}
}

// A refusal the store makes in Go, before any statement — a value that fits no bigint column, a
// value whose JSON does not encode, an integer past its integer column, and a pause's stop it does
// not take — carries the refusal type supervise's rule reads (D8 of dispatch://INFRA-4099's plan),
// so a claim write refused that way is refused for good rather than owed and repaired at every
// decision of the claim.
//
// JSON: no field of a locator can fail to encode (runtime.Locator holds strings and numbers), so
// the claim's row is the abandoned session's record whose time no RFC 3339 year holds, which
// encoding/json refuses (time.Time.MarshalJSON), and the delivery's row is a resume mark that is
// no JSON at all (storedResume's json.RawMessage).
func TestTheStoresOwnRefusalsCarryTheRefusalType(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := migratedStore(t)
	seedPauseClaims(t, st)
	pause, stops := pauseLegion(t, st, pausedAt)
	implementer := stops[0]
	held := tmuxClaim("legion-LEGION-209-implementer")
	if err := st.PutClaim(ctx, held); err != nil {
		t.Fatalf("seed the claim the deliveries ride on: %v", err)
	}
	claimWith := func(change func(*supervise.Claim)) supervise.Claim {
		c := tmuxClaim("legion-LEGION-209-implementer")
		change(&c)
		return c
	}
	delivery := func(change func(*supervise.Delivery)) supervise.Delivery {
		d := supervise.Delivery{ID: "delivery-1", Task: "test it", QueuedAt: at(1)}
		change(&d)
		return d
	}
	inTx := func(write func(ctx context.Context, tx pgx.Tx) error) error {
		return Transact(ctx, st.pool, write)
	}
	for _, tc := range []struct {
		name  string
		write func() error
		rule  string
	}{
		{"a generation no bigint holds", func() error {
			return st.PutClaim(ctx, claimWith(func(c *supervise.Claim) { c.Generation = math.MaxUint64 }))
		}, supervise.RuleBigintFit},
		{"a tree epoch no bigint holds", func() error {
			return st.PutClaim(ctx, claimWith(func(c *supervise.Claim) { c.TreeEpoch = math.MaxUint64 }))
		}, supervise.RuleBigintFit},
		{"a serving generation no bigint holds", func() error {
			return st.PutClaim(ctx, claimWith(func(c *supervise.Claim) { c.ServingGeneration = math.MaxUint64 }))
		}, supervise.RuleBigintFit},
		{"a handshake's generation no bigint holds", func() error {
			return st.PutClaim(ctx, claimWith(func(c *supervise.Claim) { c.Handshake.Generation = math.MaxUint64 }))
		}, supervise.RuleBigintFit},
		{"a resume generation no bigint holds", func() error {
			return st.PutClaim(ctx, claimWith(func(c *supervise.Claim) { c.AgentResumeGeneration = math.MaxUint64 }))
		}, supervise.RuleBigintFit},
		{"a delivery's generation no bigint holds", func() error {
			return st.PutDelivery(ctx, held.Token, delivery(func(d *supervise.Delivery) { d.Generation = math.MaxUint64 }))
		}, supervise.RuleBigintFit},
		{"a claim whose JSON does not encode", func() error {
			return st.PutClaim(ctx, claimWith(func(c *supervise.Claim) {
				c.Abandoned = supervise.AbandonedSession{Session: "ses_abandoned", Why: "a retry asked for a new session",
					At: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}
			}))
		}, supervise.RuleJSONEncode},
		{"a delivery whose JSON does not encode", func() error {
			return st.PutDelivery(ctx, held.Token, delivery(func(d *supervise.Delivery) { d.Resume = []byte("{") }))
		}, supervise.RuleJSONEncode},
		{"a claim's integer past its column", func() error {
			return st.PutClaim(ctx, claimWith(func(c *supervise.Claim) { c.Budgets.LaunchFailures = math.MaxInt32 + 1 }))
		}, supervise.RuleInt4Fit},
		{"a delivery's integer past its column", func() error {
			return st.PutDelivery(ctx, held.Token, delivery(func(d *supervise.Delivery) { d.SendFailures = math.MaxInt32 + 1 }))
		}, supervise.RuleInt4Fit},
		{"a pause's stop that arms the backstop of another issue", func() error {
			return inTx(func(ctx context.Context, tx pgx.Tx) error {
				_, err := RecordPauseStopTx(ctx, tx, PauseStopRecord{Pause: pause, Stop: implementer, At: pausedAt.Add(time.Minute),
					Backstop: &PodWakeBackstop{Project: "legion", Issue: "LEGION-209", Role: implementer.Role, DueAt: pausedAt.Add(time.Hour)}})
				return err
			})
		}, supervise.RulePauseStop},
		{"a pause's stop of a claim it does not name", func() error {
			return inTx(func(ctx context.Context, tx pgx.Tx) error {
				_, err := RecordPauseStopTx(ctx, tx, PauseStopRecord{Pause: pause, Stop: PauseStopClaim{Issue: implementer.Issue, Role: implementer.Role},
					At: pausedAt.Add(time.Minute)})
				return err
			})
		}, supervise.RulePauseStop},
		{"a pause's stop whose backstop names no due time", func() error {
			return inTx(func(ctx context.Context, tx pgx.Tx) error {
				_, err := RecordPauseStopTx(ctx, tx, PauseStopRecord{Pause: pause, Stop: implementer, At: pausedAt.Add(time.Minute),
					Backstop: &PodWakeBackstop{Project: "legion", Issue: implementer.Issue, Role: implementer.Role}})
				return err
			})
		}, supervise.RulePauseStop},
		{"a pause's lock of something that is no issue key", func() error {
			return inTx(func(ctx context.Context, tx pgx.Tx) error { return LockIssueForPause(ctx, tx, "legion-208") })
		}, supervise.RulePauseStop},
		{"a pause's stop of a pause no row holds", func() error {
			return inTx(func(ctx context.Context, tx pgx.Tx) error {
				_, err := LockPauseTx(ctx, tx, pause.ID+1000)
				return err
			})
		}, supervise.RulePauseStop},
		{"a pause's owed launch whose wake names no project", func() error {
			return inTx(func(ctx context.Context, tx pgx.Tx) error {
				_, err := (&PodRecords{store: st}).TakePodWake(ctx, tx, implementer.Issue, implementer.Role, pods.WakePause, pausedAt, pause.ID)
				return err
			})
		}, supervise.RulePauseStop},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantRefusedRow(t, tc.write(), tc.rule)
		})
	}
	if stored := onlyClaimOf(t, st, held.Token); stored.Generation != held.Generation || stored.Pending != nil {
		t.Fatalf("the refused writes left the claim at generation %d with pending %+v, want the seeded row unchanged",
			stored.Generation, stored.Pending)
	}
}

// onlyClaimOf is the stored claim of token.
func onlyClaimOf(t *testing.T, st *Store, token claim.Token) supervise.Claim {
	t.Helper()
	claims, err := st.Claims(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range claims {
		if c.Token == token {
			return c
		}
	}
	t.Fatalf("the store holds no claim %s", token)
	return supervise.Claim{}
}

// readOnlyStore is a store on st's database through a pool whose every transaction is read-only
// (default_transaction_read_only, sent at each connection's startup): the server a failover left
// read-only, which refuses every write with 25006.
func readOnlyStore(t *testing.T, st *Store) *Store {
	t.Helper()
	base, err := url.Parse(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	base.Path = "/" + st.Pool().Config().ConnConfig.Database
	// The option is written out percent-encoded whole: pgx reads a + in the query as itself rather
	// than a space, and refuses a second = in a value.
	if base.RawQuery != "" {
		base.RawQuery += "&"
	}
	base.RawQuery += "options=-c%20default_transaction_read_only%3Don"
	readOnly, err := Open(context.Background(), base.String())
	if err != nil {
		t.Fatalf("open the read-only store: %v", err)
	}
	t.Cleanup(readOnly.Close)
	return readOnly
}

// The refusals the pod's own Postgres gives a claim write are sorted by supervise's rule as the
// rule says (D8 of dispatch://INFRA-4099's plan): a CHECK the claim's row breaks (23514) and a
// unique index two claims share (23505) are the server judging the row, and are refused, with the
// constraint the server names; a server a failover left read-only (25006), a column and a table a
// binary ahead of its migration names (42703, 42P01), are the database and the schema rather than
// the row, and are owed. An integer past its column is refused by the store itself, before the
// statement, so pgx's own refusal of the value is never what the rule reads.
//
// The rows run in order on one database: the last two take the column and then the table away.
func TestTheServersRefusalsOfAClaimWriteAreSortedByTheRule(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := migratedStore(t)
	seeded := tmuxClaim("legion-LEGION-209-implementer")
	if err := st.PutClaim(ctx, seeded); err != nil {
		t.Fatalf("seed the claim: %v", err)
	}
	for _, tc := range []struct {
		name       string
		write      func(t *testing.T) error
		want       supervise.WriteFailure
		sqlstate   string
		constraint string
	}{
		{
			name: "a state the CHECK refuses",
			write: func(*testing.T) error {
				c := seeded
				c.State = "napping"
				return st.PutClaim(ctx, c)
			},
			want: supervise.WriteRefused, sqlstate: "23514", constraint: "claims_state_check",
		},
		{
			name: "a boot token hash another claim carries",
			write: func(*testing.T) error {
				c := tmuxClaim("legion-LEGION-209-tester")
				c.Role, c.BootTokenHash = claim.RoleTester, seeded.BootTokenHash
				return st.PutClaim(ctx, c)
			},
			want: supervise.WriteRefused, sqlstate: "23505", constraint: "claims_boot_token_hash",
		},
		{
			name: "a server a failover left read-only",
			write: func(t *testing.T) error {
				c := seeded
				c.State = supervise.StateWorking
				return readOnlyStore(t, st).PutClaim(ctx, c)
			},
			want: supervise.WriteOwed, sqlstate: "25006",
		},
		{
			name: "an integer past its column",
			write: func(*testing.T) error {
				c := seeded
				c.Budgets.LaunchFailures = math.MaxInt32 + 1
				return st.PutClaim(ctx, c)
			},
			want: supervise.WriteRefused,
		},
		{
			name: "a column the schema has not got",
			write: func(t *testing.T) error {
				if _, err := st.Pool().Exec(ctx, `alter table claims drop column leak_check_cause`); err != nil {
					t.Fatalf("take the column away: %v", err)
				}
				return st.PutClaim(ctx, seeded)
			},
			want: supervise.WriteOwed, sqlstate: "42703",
		},
		{
			name: "a table the schema has not got",
			write: func(t *testing.T) error {
				if _, err := st.Pool().Exec(ctx, `alter table pending_task_deliveries rename to pending_task_deliveries_gone`); err != nil {
					t.Fatalf("take the table away: %v", err)
				}
				return st.PutDelivery(ctx, seeded.Token, supervise.Delivery{ID: "delivery-1", Task: "test it", QueuedAt: at(1)})
			},
			want: supervise.WriteOwed, sqlstate: "42P01",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.write(t)
			if err == nil {
				t.Fatal("the write landed, want the server's refusal")
			}
			if got := supervise.SortClaimWriteFailure(err); got != tc.want {
				t.Fatalf("the rule sorts %v as %v, want %v", err, got, tc.want)
			}
			if tc.sqlstate == "" {
				wantRefusedRow(t, err, supervise.RuleInt4Fit)
				return
			}
			var answer *pgconn.PgError
			if !errors.As(err, &answer) {
				t.Fatalf("the write's error %v (%T) carries no server answer, want SQLSTATE %s", err, err, tc.sqlstate)
			}
			if answer.Code != tc.sqlstate {
				t.Fatalf("the server answered %s (%v), want %s", answer.Code, answer, tc.sqlstate)
			}
			if answer.ConstraintName != tc.constraint {
				t.Fatalf("the server's answer names constraint %q, want %q", answer.ConstraintName, tc.constraint)
			}
		})
	}
}

// intColumnWrite is one integer column of claims or pending_task_deliveries as a claim write sends
// it: the field that column is written from, and the write of a value of it.
type intColumnWrite func(st *Store, value int) error

// The integer columns a claim write sends, each with the write that sends it: every integer and
// smallint column of claims and pending_task_deliveries but step_args_bytes, which PutClaimStep
// writes alone (no claim write sends it, and the step is no part of a machine's claim).
func intColumnWrites(ctx context.Context, token claim.Token) map[string]intColumnWrite {
	claimWrite := func(set func(*supervise.Claim, int)) intColumnWrite {
		return func(st *Store, value int) error {
			c := tmuxClaim(token)
			set(&c, value)
			return st.PutClaim(ctx, c)
		}
	}
	deliveryWrite := func(set func(*supervise.Delivery, int)) intColumnWrite {
		return func(st *Store, value int) error {
			d := supervise.Delivery{ID: "delivery-1", Task: "test it", QueuedAt: at(1)}
			set(&d, value)
			return st.PutDelivery(ctx, token, d)
		}
	}
	return map[string]intColumnWrite{
		"claims.launch_failures":                claimWrite(func(c *supervise.Claim, v int) { c.Budgets.LaunchFailures = v }),
		"claims.deaths":                         claimWrite(func(c *supervise.Claim, v int) { c.Budgets.Deaths = v }),
		"claims.prompt_failures":                claimWrite(func(c *supervise.Claim, v int) { c.Budgets.PromptFailures = v }),
		"claims.prompt_retires":                 claimWrite(func(c *supervise.Claim, v int) { c.Budgets.PromptRetires = v }),
		"claims.uncertain_streak":               claimWrite(func(c *supervise.Claim, v int) { c.UncertainStreak = v }),
		"claims.plugin_contract":                claimWrite(func(c *supervise.Claim, v int) { c.PluginContract = v }),
		"claims.plugin_relaunch_for":            claimWrite(func(c *supervise.Claim, v int) { c.PluginRelaunchFor = v }),
		"claims.provider_error_run":             claimWrite(func(c *supervise.Claim, v int) { c.ProviderErrorRun = v }),
		"claims.serving_check_look":             claimWrite(func(c *supervise.Claim, v int) { c.ServingCheckLook.Look = v }),
		"pending_task_deliveries.check_look":    deliveryWrite(func(d *supervise.Delivery, v int) { d.CheckLook.Look = v }),
		"pending_task_deliveries.send_failures": deliveryWrite(func(d *supervise.Delivery, v int) { d.SendFailures = v }),
		"pending_task_deliveries.provider_error_retries": deliveryWrite(func(d *supervise.Delivery, v int) {
			d.ProviderErrorRetries = v
		}),
	}
}

// Every integer column of claims and pending_task_deliveries a claim write sends is checked by the
// store before the statement (D8 of dispatch://INFRA-4099's plan): a value past the column's range
// is the store's own refusal, which names the column and carries the refusal type supervise's rule
// reads as refused, and never pgx's refusal of the value, which carries no SQLSTATE and the rule
// would owe and repair at every decision of the claim. The columns are read from the migrated
// schema, so a column a later migration adds fails this test until it has a write here and a range
// check in the store. step_args_bytes is out of scope: PutClaimStep writes it alone.
func TestEveryIntegerColumnOfAClaimWriteIsCheckedBeforeTheStatement(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := migratedStore(t)
	token := claim.Token("legion-LEGION-209-implementer")
	seeded := tmuxClaim(token)
	if err := st.PutClaim(ctx, seeded); err != nil {
		t.Fatalf("seed the claim: %v", err)
	}
	rows, err := st.Pool().Query(ctx, `select table_name, column_name, data_type from information_schema.columns
		where table_schema = current_schema() and table_name in ('claims', 'pending_task_deliveries')
			and data_type in ('integer', 'smallint')
		order by table_name, column_name`)
	if err != nil {
		t.Fatal(err)
	}
	columns := map[string]string{}
	for rows.Next() {
		var table, column, kind string
		if err := rows.Scan(&table, &column, &kind); err != nil {
			t.Fatal(err)
		}
		columns[table+"."+column] = kind
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	const outOfScope = "claims.step_args_bytes"
	if _, ok := columns[outOfScope]; !ok {
		t.Fatalf("the schema has no %s; the scope this test names is stale", outOfScope)
	}
	delete(columns, outOfScope)
	if len(columns) == 0 {
		t.Fatal("the schema has no integer column a claim write sends; the read found nothing to check")
	}

	writes := intColumnWrites(ctx, token)
	names := make([]string, 0, len(columns))
	for name := range columns {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			write, ok := writes[name]
			if !ok {
				t.Fatalf("%s (%s) is an integer column of a claim write with no write in this test: add it here, and a range check of it to the store's claim write", name, columns[name])
			}
			if kind := columns[name]; kind != "integer" {
				t.Fatalf("%s is a %s column, which the store's integer range check does not bound", name, kind)
			}
			for _, value := range []int{math.MaxInt32 + 1, math.MinInt32 - 1} {
				err := write(st, value)
				wantRefusedRow(t, err, supervise.RuleInt4Fit)
				column := name[strings.Index(name, ".")+1:]
				if !strings.Contains(err.Error(), column) {
					t.Fatalf("the refusal of %d in %s does not name the column: %v", value, name, err)
				}
			}
		})
	}
	for name := range writes {
		if _, ok := columns[name]; !ok {
			t.Errorf("this test writes %s, which is no integer column of the schema: %s", name, fmt.Sprint(names))
		}
	}
	if stored := onlyClaimOf(t, st, token); stored.Budgets != seeded.Budgets || stored.Pending != nil {
		t.Fatalf("the refused writes left the claim with budgets %+v and pending %+v, want the seeded row unchanged",
			stored.Budgets, stored.Pending)
	}
}
