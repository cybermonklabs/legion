package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// breakAdviceSavepoint is an adviceQueryHook that, for the advice query named query, aborts the
// savepoint the query runs behind and closes it as pgx sees it: a statement that fails, then the
// savepoint's release, which the aborted savepoint refuses. The advice's own rollback of that
// savepoint can then no longer run, and the write's transaction stays aborted, as a rollback the
// savepoint's 500 ms statement_timeout cancelled leaves it on a starved database.
func breakAdviceSavepoint(query string) func(context.Context, pgx.Tx, string) error {
	return func(ctx context.Context, tx pgx.Tx, got string) error {
		if got != query {
			return nil
		}
		if _, err := tx.Exec(ctx, "select 1/0"); err == nil {
			return errors.New("select 1/0 answered")
		}
		if err := tx.Commit(ctx); err == nil {
			return errors.New("the aborted savepoint was released")
		}
		return nil
	}
}

// A session's question whose advice savepoint cannot be ended answers that failure, and never
// sends its write phase on the aborted transaction (adviceTxError). The question's write reads its
// advice behind savepoints of its own: writeAdvice's, and inside it the declined asks' own; then
// the standing review's (standingReviewAdvice). A failed advice read is undone by its savepoint's
// rollback and costs the question its advice alone. A savepoint whose rollback cannot run leaves
// the question's own transaction aborted: before this fix that failure was dropped, and the
// question's next statement - its Commit's batch, for the standing review - failed with 25P02,
// which was all the route answered and logged. Each case breaks one savepoint
// (breakAdviceSavepoint). Where writeAdvice's or the standing review's savepoint is broken, the
// question answers 500, the error it logs names that advice query and the aborted transaction,
// no 25P02 is logged, and no ask is stored. Where the declined asks' savepoint, inside
// writeAdvice's, is broken, writeAdvice's own rollback undoes it: the question is written, 201,
// with its advice omitted. Not parallel: each case reads the default logger's lines.
func TestAQuestionWhoseAdviceSavepointCannotBeEndedAnswersThatFailure(t *testing.T) {
	for _, step := range []struct {
		query   string
		written bool
	}{
		{query: "session_writes_since_human"},
		{query: "standing_review"},
		{query: "declined_asks", written: true},
	} {
		t.Run(step.query, func(t *testing.T) {
			logs := captureLogs(t)
			handler, database, _, served := newRegisteredTestServer(t, testServerOptions{})
			createAdviceProject(t, handler, "SAVE")
			spec := "# Savepoints\n"
			issue := createAdviceIssue(t, handler, "SAVE", "A question whose advice savepoint breaks", &spec)
			served.adviceQueryHook = breakAdviceSavepoint(step.query)
			asked := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
				"question": "Which savepoint broke?",
				"options":  []map[string]string{{"label": "Yes"}, {"label": "No"}},
				"actor":    sessionActor(),
			})
			stored := countRows(t, database, `select count(*) from asks where issue_key = $1 and question = 'Which savepoint broke?'`, issue.Key)
			failed := logs.Lines("API handler failed")
			if strings.Contains(logs.String(), "25P02") {
				t.Errorf("the question logged a 25P02, a statement sent on its aborted transaction:\n%s", logs.Lines("25P02"))
			}
			if step.written {
				if asked.Code != http.StatusCreated || stored != 1 || failed != "" {
					t.Errorf("the question answered %d %s with %d ask(s) stored and logged %q, want 201 and its ask, its advice omitted",
						asked.Code, asked.Body.String(), stored, failed)
				}
				if omitted := logs.Lines("write advice omitted"); !strings.Contains(omitted, "query="+step.query) {
					t.Errorf("the question logged no omitted advice of %s: %q", step.query, omitted)
				}
				return
			}
			if asked.Code != http.StatusInternalServerError || stored != 0 {
				t.Errorf("the question answered %d %s with %d ask(s) stored, want 500 and none", asked.Code, asked.Body.String(), stored)
			}
			if !strings.Contains(failed, "write advice "+step.query+" left the write's transaction aborted") {
				t.Errorf("the question's failure logged %q, want the savepoint of %s that left its transaction aborted", failed, step.query)
			}
		})
	}
}
