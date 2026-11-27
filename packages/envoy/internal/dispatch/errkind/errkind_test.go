package errkind

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/cipher"
)

// person is the address each failure below quotes, as the people store's and the broker client's
// errors quote the person they were asked about.
const person = "ada@example.com"

// Each failure a Dispatch store or broker call can end in is named by its kind, wrapped as the
// people store wraps it (quoting the address) and wrapped again as a caller wraps it, and the kind
// holds neither the address nor any other word of the error's own text. The database connection is
// a real one, refused on a port nothing listens on.
//
// Control: make Of return err.Error(), and every row fails on the address.
func TestOfNamesTheKindAndNoneOfTheErrorsText(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, refused := pgconn.Connect(ctx, "postgres://nobody@127.0.0.1:1/nothing?sslmode=disable&connect_timeout=5")
	if refused == nil {
		t.Fatal("a connection to 127.0.0.1:1 was made; want it refused")
	}
	for _, row := range []struct {
		name  string
		cause error
		want  string
	}{
		{"a canceled context", context.Canceled, "canceled"},
		{"a context past its deadline", fmt.Errorf("timeout: %w", context.DeadlineExceeded), "deadline exceeded"},
		{"a refused database connection", refused, "database connection"},
		{"an error the database answered", &pgconn.PgError{Severity: "ERROR", Code: "57014", Message: "canceling statement due to user request " + person}, "database SQLSTATE 57014"},
		{"a broker answer that was no success", &cipher.Error{Status: 503, Body: []byte(`{"error":"` + person + `"}`)}, "broker HTTP 503"},
		{"a broker account token that could not be read", &cipher.AccountTokenError{Path: "/var/run/secrets/" + person, Err: os.ErrNotExist}, "broker account token"},
		{"a network timeout", &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}, "network timeout"},
		{"a refused network connection", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, "network"},
		{"any other failure", errors.New("the answer named " + person), "*errors.errorString"},
		{"a failure wrapped beside a sentinel", fmt.Errorf("%w: %w", errors.New("the lists could not be read"), &json.SyntaxError{Offset: 3}), "*json.SyntaxError"},
		{"a joined failure", errors.Join(errors.New("first "+person), &json.InvalidUnmarshalError{}), "*json.InvalidUnmarshalError"},
	} {
		t.Run(row.name, func(t *testing.T) {
			stored := fmt.Errorf("read membership of %q: %w", person, row.cause)
			err := fmt.Errorf("dispatch: the sign-out of %s: %w", person, stored)
			got := Of(err)
			if got != row.want {
				t.Errorf("Of(%q) = %q, want %q", err, got, row.want)
			}
			for _, word := range []string{person, "read membership", "sign-out"} {
				if strings.Contains(got, word) {
					t.Errorf("Of(%q) = %q holds %q, a word of the error's own text", err, got, word)
				}
			}
		})
	}
	if got := Of(nil); got != "" {
		t.Errorf("Of(nil) = %q, want \"\"", got)
	}
}
