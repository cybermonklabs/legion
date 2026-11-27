package record

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// statementTx is tx that keeps each statement a store method sent through QueryRow or Query, with
// its arguments, so a test can ask PostgreSQL how it plans exactly those statements.
type statementTx struct {
	pgx.Tx
	sent []sentStatement
}

// sentStatement is one statement statementTx kept, with the arguments it was sent with.
type sentStatement struct {
	sql  string
	args []any
}

func (t *statementTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	t.sent = append(t.sent, sentStatement{sql: sql, args: args})
	return t.Tx.QueryRow(ctx, sql, args...)
}

func (t *statementTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	t.sent = append(t.sent, sentStatement{sql: sql, args: args})
	return t.Tx.Query(ctx, sql, args...)
}

// plan is PostgreSQL's plan of every statement t kept, each with the arguments it was sent with,
// one after the other.
func (t *statementTx) plan(ctx context.Context, test *testing.T) string {
	test.Helper()
	var plan []string
	for _, statement := range t.sent {
		rows, err := t.Tx.Query(ctx, "explain "+statement.sql, statement.args...)
		must(test, err)
		lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
		must(test, err)
		plan = append(plan, lines...)
	}
	return strings.Join(plan, "\n")
}

// SessionClaimsTree is read for each status change a session writes on a recorded issue
// (sessionStatusWrite) and on the ask events of a recorded root (the ask mirror's
// dispatchAskOpened, closer and replyToAsk), all under the one fact lock. Its statement finds the
// claims of one tree by the index on claims (tree), and the session among them: over a claims
// table of 10,000 rows, 100 trees of 100 claims each, PostgreSQL's plan of the statement the
// method sends, with the arguments it sends, reads that index and scans no table whole. Without
// the index the plan is a sequential scan of claims, which one deployment's daemon made 27,862
// times in fifteen minutes at 6.4 ms each.
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
	var read string
	inTx(t, st, func(tx pgx.Tx) {
		sent := &statementTx{Tx: tx}
		inTree, err := NewStore().SessionClaimsTree(ctx, sent, "LEGION-42", "ses-4217")
		must(t, err)
		if !inTree {
			t.Fatal("SessionClaimsTree(LEGION-42, ses-4217) = false, want the claim the table holds")
		}
		read = sent.plan(ctx, t)
	})
	if strings.Contains(read, "Seq Scan") || !strings.Contains(read, "claims_tree") {
		t.Fatalf("the plan of SessionClaimsTree's statement is\n%s\nwant it to read the index claims_tree and scan no table whole", read)
	}
}

// SessionOfTree and SessionClaim are read for each claim release the outbox runs (its
// releaseClaim), and SessionOfTree on every admission pass for each root waiting on a person whose
// Dispatch claim a session holds (workflow.Unresumable), under the one fact lock, as
// SessionClaimsTree is. claims has no index on session, and claim_sessions holds every name each
// claim of the project recorded, so neither read may scan either table whole: SessionOfTree finds
// the claims of one tree by claims_tree, and a name one of them left behind by claim_sessions'
// primary key, in one statement; SessionClaim finds the name by that key and its claim by claims'
// own. Over 10,000 claims in 100 trees, each holding its current session, a session it left behind
// and its file's name, PostgreSQL's plan of every statement each read sends, with the arguments it
// sends, reads those indexes and scans no table whole.
func TestSessionOfTreeReadsTheClaimsAndTheirSessionsByTheirIndexes(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	inTx(t, st, func(tx pgx.Tx) {
		_, err := tx.Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file,
			state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
			select 'claim-' || n, 'LEGION', 'LEGION-' || (n / 100), 'LEGION-' || (n / 100), 'architect', 0, 'ses-' || n, '',
				'working', 0, 0, 0, 0
			from generate_series(0, 9999) as n`)
		must(t, err)
		_, err = tx.Exec(ctx, `insert into claim_sessions (project, kind, name, claim)
			select 'LEGION', 'session', 'ses-' || n, 'claim-' || n from generate_series(0, 9999) as n
			union all select 'LEGION', 'session', 'left-' || n, 'claim-' || n from generate_series(0, 9999) as n
			union all select 'LEGION', 'file-name', 'ses-' || n || '.jsonl', 'claim-' || n from generate_series(0, 9999) as n`)
		must(t, err)
	})
	if _, err := st.Pool().Exec(ctx, "analyze claims, claim_sessions"); err != nil {
		t.Fatalf("analyze claims and claim_sessions: %v", err)
	}

	var ofTree string
	inTx(t, st, func(tx pgx.Tx) {
		sent := &statementTx{Tx: tx}
		found, err := NewStore().SessionOfTree(ctx, sent, "LEGION-42", "left-4217")
		must(t, err)
		if !found {
			t.Fatal("SessionOfTree(LEGION-42, left-4217) = false, want true: claim-4217 of that tree left the session behind")
		}
		if len(sent.sent) != 1 {
			t.Fatalf("SessionOfTree sent %d statements, want one that answers both halves", len(sent.sent))
		}
		ofTree = sent.plan(ctx, t)
	})
	if strings.Contains(ofTree, "Seq Scan") || !strings.Contains(ofTree, "claims_tree") || !strings.Contains(ofTree, "claim_sessions_pkey") {
		t.Fatalf("the plan of SessionOfTree's statement is\n%s\nwant it to read the indexes claims_tree and claim_sessions_pkey and scan no table whole", ofTree)
	}

	var ofClaim string
	inTx(t, st, func(tx pgx.Tx) {
		sent := &statementTx{Tx: tx}
		got, found, err := NewStore().SessionClaim(ctx, sent, "LEGION", "left-4217")
		must(t, err)
		want := SessionClaim{Token: "claim-4217", Tree: "LEGION-42", Issue: "LEGION-42", Role: claim.RoleArchitect, Session: "ses-4217"}
		if !found || got != want {
			t.Fatalf("SessionClaim(LEGION, left-4217) = %+v, %t, want %+v, the claim that left the session behind", got, found, want)
		}
		ofClaim = sent.plan(ctx, t)
	})
	if strings.Contains(ofClaim, "Seq Scan") || !strings.Contains(ofClaim, "claim_sessions_pkey") || !strings.Contains(ofClaim, "claims_pkey") {
		t.Fatalf("the plan of SessionClaim's statements is\n%s\nwant it to read the indexes claim_sessions_pkey and claims_pkey and scan no table whole", ofClaim)
	}
}
