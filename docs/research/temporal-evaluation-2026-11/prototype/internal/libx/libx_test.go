package libx

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// The pieces of the made-up user parts below that no line may print.
var userParts = []string{"made-up-user", "PieceA1", "PieceB2"}

func holdsAPiece(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("no error")
	}
	for _, piece := range userParts {
		if strings.Contains(err.Error(), piece) {
			t.Errorf("the error names %q of the user part: %v", piece, err)
		}
	}
}

// A DSN the evaluation cannot read is refused by its length, and with none of its user part: a
// / in the password makes net/url read a piece of it as the port and quote the value whole, and a
// value with no scheme is read with its user as the scheme.
func TestARefusalOfTheDSNNamesNoPartOfIt(t *testing.T) {
	for _, dsn := range []string{
		"postgres://made-up-user:PieceA1/PieceB2-not-a-secret@127.0.0.1:1/postgres",
		"postgresx://made-up-user:PieceA1-not-a-secret@127.0.0.1:1/postgres",
		"made-up-user:PieceA1-not-a-secret@127.0.0.1:1/postgres",
		"postgres://made-up-user:PieceA1-not-a-secret@PieceB2/x@127.0.0.1:1/postgres",
	} {
		t.Run(dsn[:12], func(t *testing.T) {
			_, err := DatabaseURL(dsn, "")
			holdsAPiece(t, err)
			if want := fmt.Sprintf("a value of %d characters", len(dsn)); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("the error does not say %q: %v", want, err)
			}
			holdsAPiece(t, EnsureDatabase(context.Background(), dsn))
		})
	}
}

// A failed connect names no user and no database of the DSN, which pgx puts in front of what failed.
func TestAFailedConnectNamesNoUser(t *testing.T) {
	_, err := Open(context.Background(), "postgres://made-up-user:PieceA1-not-a-secret@127.0.0.1:1/postgres?connect_timeout=5", "")
	holdsAPiece(t, err)
	if err == nil || !strings.Contains(err.Error(), "reach PostgreSQL: failed to connect: ") {
		t.Errorf("the error does not say what failed: %v", err)
	}
}
