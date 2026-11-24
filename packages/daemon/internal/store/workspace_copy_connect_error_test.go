package store

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// A take, and a drop of what a kept volume holds, of an issue's saved workspace copies on a
// connection that never opened is an ask the store did not answer (copyAskUnanswered's arm of
// *pgconn.ConnectError), whatever the server said in refusing it, so the start it fails is held
// and tried again charging nothing. The server here refuses the login of a role that does not
// exist: FATAL 28000 under the pod's trust auth, or 28P01 where the server asks a password. That
// is an answer OpenRetryable reads as definite, and the ask answers it before its ctx ends, so no
// other arm decides the rows. The pool is built as claims_connect_error_test.go builds one,
// lazily, so the ask itself meets the refusal.
//
// The control (the pull request records the run): copyAskUnanswered without its arm of
// *pgconn.ConnectError, which answers both rows' errors unwrapped.
func TestAWorkspaceCopyAskOnAConnectionThatNeverOpenedIsTheStores(t *testing.T) {
	dsn, err := url.Parse(testDSN(t))
	if err != nil {
		t.Fatalf("parse LEGION_TEST_PG_DSN: %v", err)
	}
	role := "legion_no_such_role_" + randomSuffix(t)
	if password, ok := dsn.User.Password(); ok {
		dsn.User = url.UserPassword(role, password)
	} else {
		dsn.User = url.User(role)
	}
	pool, err := pgxpool.New(context.Background(), dsn.String())
	if err != nil {
		t.Fatalf("build a pool for a role that does not exist: %v", err)
	}
	t.Cleanup(pool.Close)
	s := &Store{pool: NewPool(pool)}

	const issue, pod = "INFRA-1117", "pod-whose-ask-never-connects"
	for _, row := range []struct {
		name string
		ask  func(ctx context.Context) error
	}{
		{name: "the take", ask: func(ctx context.Context) error {
			_, err := s.TakeWorkspaceCopyForRestore(ctx, copyProject, issue, pod)
			return err
		}},
		{name: "the drop of what a kept volume holds", ask: func(ctx context.Context) error {
			_, err := s.DropHeldWorkspaceCopies(ctx, copyProject, issue, pod, time.Now())
			return err
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			err := row.ask(ctx)
			var connect *pgconn.ConnectError
			if connected := errors.As(err, &connect); !connected || OpenRetryable(err) || ctx.Err() != nil {
				t.Fatalf("premise: the ask answered %v (a *pgconn.ConnectError: %t, OpenRetryable: %t, its ctx's end: %v); want a connection that never opened, refused with an answer OpenRetryable reads as definite, before its ctx ended",
					err, connected, OpenRetryable(err), ctx.Err())
			}
			if !errors.Is(err, runtime.ErrWorkspaceCopyStore) {
				t.Fatalf("the ask on a connection that never opened answered %v, want runtime.ErrWorkspaceCopyStore: the store did not answer it", err)
			}
		})
	}
}
