package pgsetting_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/sjawhar/envoy/pgsetting"
)

// A server that refuses the sign-in answers in words of its own, and Postgres's name the user it
// refused (`password authentication failed for user "..."`, `role "..." does not exist`), which
// can be a token written where the user stands. So a failed connect names each server's answer by
// its severity and SQLSTATE alone, at each host the connect tried, and still names the host, and
// what failed at a host that answered nothing (a refused port) as pgx wrote it, which is the
// control; the words a caller wrapped around the failure stay. The server here answers the startup
// message as Postgres answers a wrong password, naming the user the startup message carried. The
// user and the password are made at each run, so no credential is written in this file.
func TestAFailedConnectNamesAServersAnswerByItsSQLSTATE(t *testing.T) {
	user, password := "made-up-user-"+rand.Text(), "made-up-"+rand.Text()
	const refused = "127.0.0.1:1"
	refusing := refusingServer(t)
	for _, tc := range []struct {
		name, hosts string
		names       []string
		answered    bool
	}{
		{"a wrong password", refusing, []string{refusing, "server error: FATAL (SQLSTATE 28P01)"}, true},
		{"a refused port, then a wrong password", refused + "," + refusing, []string{refused, refusing, "server error: FATAL (SQLSTATE 28P01)"}, true},
		{"a refused port alone, the control", refused, []string{refused}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			dsn := (&url.URL{Scheme: "postgres", User: url.UserPassword(user, password), Host: tc.hosts, Path: "/x", RawQuery: "sslmode=disable&connect_timeout=3"}).String()
			config, err := pgsetting.ParseConfig("DATABASE_URL", dsn)
			if err != nil {
				t.Fatalf("ParseConfig: %v", err)
			}
			_, err = pgx.ConnectConfig(ctx, config)
			var connect *pgconn.ConnectError
			if !errors.As(err, &connect) || !strings.Contains(err.Error(), user) {
				t.Fatalf("UNKNOWN: pgx's connect failure does not name the user, so this row saw nothing: %v", err)
			}
			if tc.answered && !strings.Contains(errors.Unwrap(connect).Error(), user) {
				t.Fatalf("UNKNOWN: the server's answer under pgx's prefix does not name the user, so this row saw nothing: %v", err)
			}
			failed := pgsetting.ConnectFailure(err).Error()
			for _, part := range []string{user, password} {
				if strings.Contains(failed, part) {
					t.Errorf("ConnectFailure holds a part of the user part: %q", failed)
				}
			}
			for _, part := range tc.names {
				if !strings.Contains(failed, part) {
					t.Errorf("ConnectFailure: %q, want it to name %q", failed, part)
				}
			}
			var server *pgconn.PgError
			if tc.answered && (!errors.As(pgsetting.ConnectFailure(err), &server) || server.Code != "28P01") {
				t.Errorf("ConnectFailure unwraps to %v, want the server's 28P01 still there for errors.As", server)
			}
			if !tc.answered {
				if want := "failed to connect: " + errors.Unwrap(connect).Error(); failed != want {
					t.Errorf("ConnectFailure: %q, want what failed as pgx wrote it, %q", failed, want)
				}
			}
			if wrapped := pgsetting.ConnectFailure(fmt.Errorf("probe the database: %w", err)).Error(); wrapped != "probe the database: "+failed {
				t.Errorf("ConnectFailure of a wrapped failure: %q, want the caller's words and then %q", wrapped, failed)
			}
		})
	}
}

// refusingServer is the address of a server that answers every startup message as Postgres
// answers a wrong password: FATAL, SQLSTATE 28P01, naming the user the message carried. It refuses
// an SSL or GSS encryption request first, as a server with neither does.
func refusingServer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				backend := pgproto3.NewBackend(conn, conn)
				for {
					message, err := backend.ReceiveStartupMessage()
					if err != nil {
						return
					}
					switch startup := message.(type) {
					case *pgproto3.SSLRequest, *pgproto3.GSSEncRequest:
						if _, err := conn.Write([]byte("N")); err != nil {
							return
						}
					case *pgproto3.StartupMessage:
						backend.Send(&pgproto3.ErrorResponse{
							Severity:            "FATAL",
							SeverityUnlocalized: "FATAL",
							Code:                "28P01",
							Message:             fmt.Sprintf("password authentication failed for user %q", startup.Parameters["user"]),
						})
						_ = backend.Flush()
						return
					default:
						return
					}
				}
			}()
		}
	}()
	return listener.Addr().String()
}
