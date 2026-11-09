package testdb

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"syscall"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// The retry's whole judgement, as a function of the error a connection answered with: the socket
// going is tried again, the server's own answer is not. Getting that the wrong way round is how a
// retry turns a wrong password into a wait and a reset into a failed test, so both directions are
// driven here rather than inferred from the one failure that prompted it.
func TestATransientConnectIsTheSocketGoingAndNotTheServerAnswering(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "a reset, as the server answers under connection churn", err: wrapped(syscall.ECONNRESET), want: true},
		{name: "a refusal, as a server not yet listening answers", err: wrapped(syscall.ECONNREFUSED), want: true},
		{name: "a broken pipe", err: wrapped(syscall.EPIPE), want: true},
		{name: "an end of file, as a socket closed mid-handshake gives", err: fmt.Errorf("receive message: %w", io.EOF), want: true},
		{name: "a closed connection of this process's own", err: fmt.Errorf("write: %w", net.ErrClosed), want: true},
		{name: "a bad password, which the server answered", err: &pgconn.PgError{Code: "28P01", Message: "password authentication failed"}},
		{name: "a database that does not exist, which the server answered", err: &pgconn.PgError{Code: "3D000", Message: "database does not exist"}},
		// The one answer it would be worst to retry. A refusal for max_connections is how the cap
		// reports itself, and the absence of that refusal across a campaign is what k6's headroom
		// rests on: a retry that absorbed it would make the absence true by construction and leave
		// the evidence saying nothing (dispatch://INFRA-1524, k6).
		{name: "the connection cap's own refusal, which must fail at once", err: &pgconn.PgError{Code: "53300", Message: "sorry, too many clients already"}},
		{name: "the cap's refusal for one role", err: &pgconn.PgError{Code: "53300", Message: "too many connections for role \"legion_test\""}},
		{name: "a server answer wrapped in a connect error", err: fmt.Errorf("connect: %w", &pgconn.PgError{Code: "28P01"})},
		{name: "any other error", err: errors.New("parse the URL")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// A retry of the cap's own refusal is the failure this table exists to refuse, so it
			// is named rather than left to the reader of the want column.
			if got := transientConnect(tc.err); got != tc.want {
				t.Fatalf("transientConnect(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// What OnConnect does with that judgement: it opens again while the socket goes, up to its own
// bound, and returns the server's own answer at once. The count it keeps is the evidence the
// retry exists for — a run says how many connections needed one — so it is asserted here too.
func TestOnConnectRetriesTheSocketAndNotTheServer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		answers []error
		opened  int64
		retried int64
		wantErr string
	}{
		{name: "a connection that opens at once", answers: []error{nil}, opened: 1},
		{name: "a reset, then a connection", answers: []error{wrapped(syscall.ECONNRESET), nil}, opened: 2, retried: 1},
		{name: "two resets, then a connection", answers: []error{wrapped(syscall.ECONNRESET), wrapped(syscall.ECONNRESET), nil}, opened: 3, retried: 1},
		{
			name:    "a reset at every attempt the bound allows",
			answers: []error{wrapped(syscall.ECONNRESET), wrapped(syscall.ECONNRESET), wrapped(syscall.ECONNRESET), nil},
			opened:  3, wantErr: "connection reset by peer",
		},
		{
			name:    "a password the server refused, which is not tried again",
			answers: []error{&pgconn.PgError{Code: "28P01", Message: "password authentication failed"}, nil},
			opened:  1, wantErr: "password authentication failed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Not parallel: it reads the process-wide connect counters, which every other test of
			// this package's fixtures also writes.
			opened, retried := connectCounts.opened.Load(), connectCounts.retried.Load()
			attempt := 0
			err := OnConnect(func() error {
				answer := tc.answers[attempt]
				attempt++
				return answer
			})
			if tc.wantErr == "" && err != nil {
				t.Fatalf("OnConnect = %v, want a connection", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("OnConnect = %v, want an error naming %q", err, tc.wantErr)
			}
			if got := int64(attempt); got != tc.opened {
				t.Fatalf("open was called %d times, want %d", got, tc.opened)
			}
			if got := connectCounts.opened.Load() - opened; got != tc.opened {
				t.Fatalf("the counter says %d connections were opened, want %d", got, tc.opened)
			}
			if got := connectCounts.retried.Load() - retried; got != tc.retried {
				t.Fatalf("the counter says %d connections needed a retry, want %d", got, tc.retried)
			}
		})
	}
}

// wrapped is a syscall error inside the layers a pgx connection answers with, so the judgement is
// read through the same chain production gives it rather than from a bare errno.
func wrapped(errno syscall.Errno) error {
	return fmt.Errorf("failed to connect: %w", &net.OpError{
		Op: "read", Net: "tcp", Err: &net.OpError{Op: "read", Err: errno},
	})
}
