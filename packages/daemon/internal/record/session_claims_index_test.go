package record

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// statementTx is tx that keeps every statement a store method sends through Query and QueryRow
// with its arguments, so a test can ask PostgreSQL how it plans exactly what the method sent.
type statementTx struct {
	pgx.Tx
	sent []sentStatement
}

type sentStatement struct {
	sql  string
	args []any
}

func (t *statementTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	t.sent = append(t.sent, sentStatement{sql, args})
	return t.Tx.Query(ctx, sql, args...)
}

func (t *statementTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	t.sent = append(t.sent, sentStatement{sql, args})
	return t.Tx.QueryRow(ctx, sql, args...)
}

// readsClaims is a statement that reads the claims table itself, not dispatch_claims.
var readsClaims = regexp.MustCompile(`(?i)\bfrom claims\b`)

// The claims a store method of the admission pass reads go by the issue they are on, or by their
// project, and the table holds the claims of every project that shares the database. Over a claims
// table of 10,000 rows, 50 projects of 200 claims each and an issue holding at most one claim of
// each role, PostgreSQL's plan of each statement the method sends, with the arguments it sends,
// reads the index on the column the statement names and scans no table whole. Without the indexes
// (20261126035722_claims_issue_and_project) the plan is a sequential scan of claims, as the read by
// tree was before 20261125212800, which one deployment's daemon made 27,862 times in fifteen
// minutes at 6.4 ms each.
//
// SessionClaimsOf is read on every admission pass for each root waiting on a person whose Dispatch
// claim a session holds (workflow.Unresumable), under the one lock every fact takes. The others are
// the reads of an architect's launch and of what it last served (ArchitectLaunchesOf,
// ArchitectServedOf), of the claims of an issue that hold a session (LiveRoleHolders,
// RoleHoldersAt), of the pod an issue's claims run in (IssuePodUID), and of the claims of a project
// that the stalled-tree notice reads (ProjectClaims).
func TestTheClaimReadsOfTheAdmissionPassUseAnIndex(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	inTx(t, st, func(tx pgx.Tx) {
		_, err := tx.Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file,
			state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
			select 'claim-' || n, 'p' || (n % 50), 'P' || (n % 50) || '-0', 'P' || (n % 50) || '-' || ((n / 50) / 6),
				(array['architect', 'planner', 'implementer', 'tester', 'reviewer', 'merger'])[1 + (n / 50) % 6], 0,
				'ses-' || n, '', 'working', 0, 0, 0, 0
			from generate_series(0, 9999) as n`)
		must(t, err)
	})
	if _, err := st.Pool().Exec(ctx, "analyze claims"); err != nil {
		t.Fatalf("analyze claims: %v", err)
	}
	records := NewStore()
	issue, more := "P7-12", "P7-13"
	for _, tc := range []struct {
		name  string
		read  func(tx pgx.Tx) error
		index string
	}{
		{"SessionClaimsOf", func(tx pgx.Tx) error {
			inIssue, err := records.SessionClaimsOf(ctx, tx, []string{issue, more}, "ses-3607")
			if err == nil && !inIssue {
				t.Error("SessionClaimsOf(P7-12 and P7-13, ses-3607) = false, want the claim the table holds")
			}
			return err
		}, "claims_issue"},
		{"ArchitectLaunchesOf", func(tx pgx.Tx) error {
			_, err := records.ArchitectLaunchesOf(ctx, tx, []string{issue, more})
			return err
		}, "claims_issue"},
		{"ArchitectServedOf", func(tx pgx.Tx) error {
			_, _, err := records.ArchitectServedOf(ctx, tx, issue)
			return err
		}, "claims_issue"},
		{"LiveRoleHolders", func(tx pgx.Tx) error {
			_, err := records.LiveRoleHolders(ctx, tx, issue)
			return err
		}, "claims_issue"},
		{"RoleHoldersAt", func(tx pgx.Tx) error {
			_, err := records.RoleHoldersAt(ctx, tx, issue)
			return err
		}, "claims_issue"},
		{"IssuePodUID", func(tx pgx.Tx) error {
			_, err := records.IssuePodUID(ctx, tx, issue)
			return err
		}, "claims_issue"},
		{"ProjectClaims", func(tx pgx.Tx) error {
			held, err := records.ProjectClaims(ctx, tx, "p7")
			if err == nil && len(held) != 200 {
				t.Errorf("ProjectClaims(p7) = %d claims, want the 200 of the project", len(held))
			}
			return err
		}, "claims_project"},
		{"RoleRuns", func(tx pgx.Tx) error {
			_, err := records.RoleRuns(ctx, tx, []RoleRunKey{{Token: claim.Token("claim-3607"), Issue: issue, Role: claim.RoleArchitect}})
			return err
		}, "claims_pkey"},
		{"ClaimSession", func(tx pgx.Tx) error {
			_, err := records.ClaimSession(ctx, tx, claim.Token("claim-3607"))
			return err
		}, "claims_pkey"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var plans []string
			inTx(t, st, func(tx pgx.Tx) {
				sent := &statementTx{Tx: tx}
				must(t, tc.read(sent))
				for _, statement := range sent.sent {
					if !readsClaims.MatchString(statement.sql) {
						continue
					}
					rows, err := tx.Query(ctx, "explain "+statement.sql, statement.args...)
					must(t, err)
					lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
					must(t, err)
					plans = append(plans, strings.Join(lines, "\n"))
				}
			})
			if len(plans) == 0 {
				t.Fatalf("%s sent no statement that reads claims", tc.name)
			}
			read := strings.Join(plans, "\n")
			if strings.Contains(read, "Seq Scan on claims") || !strings.Contains(read, tc.index) {
				t.Fatalf("the plan of %s's statement is\n%s\nwant it to read the index %s and scan no claims table whole", tc.name, read, tc.index)
			}
		})
	}
}
