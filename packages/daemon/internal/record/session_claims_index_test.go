package record

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// statementTx is tx that keeps the last statement a store method sent through QueryRow and its
// arguments, so a test can ask PostgreSQL how it plans exactly that statement.
type statementTx struct {
	pgx.Tx
	sql  string
	args []any
}

func (t *statementTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	t.sql, t.args = sql, args
	return t.Tx.QueryRow(ctx, sql, args...)
}

// SessionClaimsTree is read on every admission pass for each root waiting on a person whose
// Dispatch claim a session holds (workflow.Unresumable), and on the ask events of a recorded
// root, all under the one fact lock. Its statement finds the claims of one tree by the index on
// claims (tree), and the session among them: over a claims table of 10,000 rows, 100 trees of 100
// claims each, PostgreSQL's plan of the statement the method sends, with the arguments it sends,
// reads that index and scans no table whole. Without the index the plan is a sequential scan of
// claims, which one deployment's daemon made 27,862 times in fifteen minutes at 6.4 ms each.
func TestSessionClaimsTreeReadsTheClaimsOfATreeByTheirIndex(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	inTx(t, st, func(tx pgx.Tx) {
		_, err := tx.Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file,
			state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
			select 'claim-' || n, 'LEGION', 'LEGION-' || (n / 100), 'LEGION-' || (n / 100), 'architect', 0, 'ses-' || n, '',
				'working', 0, 0, 0, 0
			from generate_series(0, 9999) as n`)
		must(t, err)
	})
	if _, err := st.Pool().Exec(ctx, "analyze claims"); err != nil {
		t.Fatalf("analyze claims: %v", err)
	}
	var plan []string
	inTx(t, st, func(tx pgx.Tx) {
		sent := &statementTx{Tx: tx}
		inTree, err := NewStore().SessionClaimsTree(ctx, sent, "LEGION-42", "ses-4217")
		must(t, err)
		if !inTree {
			t.Fatal("SessionClaimsTree(LEGION-42, ses-4217) = false, want the claim the table holds")
		}
		rows, err := tx.Query(ctx, "explain "+sent.sql, sent.args...)
		must(t, err)
		plan, err = pgx.CollectRows(rows, pgx.RowTo[string])
		must(t, err)
	})
	read := strings.Join(plan, "\n")
	if strings.Contains(read, "Seq Scan") || !strings.Contains(read, "claims_tree") {
		t.Fatalf("the plan of SessionClaimsTree's statement is\n%s\nwant it to read the index claims_tree and scan no table whole", read)
	}
}
