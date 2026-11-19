package dtx

import (
	"fmt"
	"strings"
	"testing"
)

// A DSN the durabletask options cannot be made from is refused by its length, and with none of
// its user part: a / in the password makes net/url read a piece of it as the port and quote the
// value whole, and strconv's error quotes the port it could not read.
func TestARefusalOfTheDSNNamesNoPartOfIt(t *testing.T) {
	for _, dsn := range []string{
		"postgres://made-up-user:PieceA1/PieceB2-not-a-secret@127.0.0.1:1/postgres",
		"postgres://made-up-user:PieceA1-not-a-secret@127.0.0.1:99999/postgres",
		"postgres://made-up-user:PieceA1-not-a-secret@PieceB2/x@127.0.0.1:1/postgres",
	} {
		t.Run(dsn[len(dsn)-20:], func(t *testing.T) {
			_, err := options(Options{DSN: dsn})
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
