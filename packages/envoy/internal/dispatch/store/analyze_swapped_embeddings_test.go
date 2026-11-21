package store

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// analyzeExec answers each statement from answers, in order, and records what it was sent.
type analyzeExec struct {
	answers []error
	sent    []string
}

func (e *analyzeExec) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	e.sent = append(e.sent, sql)
	if len(e.answers) == 0 {
		return pgconn.CommandTag{}, nil
	}
	answer := e.answers[0]
	e.answers = e.answers[1:]
	return pgconn.CommandTag{}, answer
}

// The analyze after the swap reads the parent alone where the server takes ONLY (PostgreSQL 18),
// and analyzes embeddings whole where it refuses ONLY as a syntax error (42601, PostgreSQL 16 and
// 17, which a deployment's databaseVersion may still name). Any other failure of ANALYZE ONLY is
// answered as it is, with no second statement: only the syntax error says the server cannot read
// ONLY.
func TestTheSwapsAnalyzeFallsBackToTheWholeTableBeforePostgres18(t *testing.T) {
	ctx := context.Background()
	const only, whole = "analyze only embeddings", "analyze embeddings"
	for _, run := range []struct {
		name    string
		answers []error
		sent    []string
		fails   bool
	}{
		{name: "PostgreSQL 18", sent: []string{only}},
		{name: "PostgreSQL 16", answers: []error{&pgconn.PgError{Code: "42601", Message: `syntax error at or near "embeddings"`}}, sent: []string{only, whole}},
		{name: "a cancelled analyze", answers: []error{&pgconn.PgError{Code: "57014"}}, sent: []string{only}, fails: true},
		{name: "PostgreSQL 16, and the whole table fails too", answers: []error{&pgconn.PgError{Code: "42601"}, errors.New("connection reset")}, sent: []string{only, whole}, fails: true},
	} {
		exec := &analyzeExec{answers: run.answers}
		err := analyzeSwappedEmbeddings(ctx, exec)
		if (err != nil) != run.fails {
			t.Errorf("%s: analyze = %v, want failed %t", run.name, err, run.fails)
		}
		if !slices.Equal(exec.sent, run.sent) {
			t.Errorf("%s: sent %q, want %q", run.name, exec.sent, run.sent)
		}
	}
}
