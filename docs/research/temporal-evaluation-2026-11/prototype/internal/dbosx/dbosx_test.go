package dbosx

import (
	"fmt"
	"strings"
	"testing"
)

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
			_, err := DatabaseURL(dsn)
			if err == nil {
				t.Fatal("admitted")
			}
			for _, piece := range []string{"made-up-user", "PieceA1", "PieceB2"} {
				if strings.Contains(err.Error(), piece) {
					t.Errorf("the error names %q of the user part: %v", piece, err)
				}
			}
			if want := fmt.Sprintf("a value of %d characters", len(dsn)); !strings.Contains(err.Error(), want) {
				t.Errorf("the error does not say %q: %v", want, err)
			}
		})
	}
}
