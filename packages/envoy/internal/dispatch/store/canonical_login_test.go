package store

import (
	"context"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// canonical_login (migration 20261117000831) is model.CanonicalLogin, character for character: for
// every character of the Basic Multilingual Plane that Postgres text can hold (all but U+0000 and
// the surrogates) and a few past it, an address wrapped on both sides in that character and holding
// it inside, with capitals around it, reads in SQL exactly as the Go function reads it. So every
// White_Space character is trimmed and none other is, A-Z is lowered and no other letter is (U+212A
// KELVIN SIGN stays itself, where lower() would read it as k), and a character inside the address is
// kept. The cases go to the database in one statement.
func TestCanonicalLoginIsModelCanonicalLogin(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	var inputs []string
	add := func(r rune) {
		c := string(r)
		inputs = append(inputs, c+"Ab"+c+"@Example.COM"+c, c+c)
	}
	for r := rune(1); r <= 0xFFFF; r++ {
		if utf8.ValidRune(r) {
			add(r)
		}
	}
	for _, r := range []rune{0x10000, 0x1D400, 0x1F600, 0xE0020, 0x10FFFF} {
		add(r)
	}
	rows, err := database.Pool.Query(ctx, `select canonical_login(x) from unnest($1::text[]) with ordinality as u(x, n) order by n`, inputs)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		got = append(got, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(inputs) {
		t.Fatalf("canonical_login answered %d rows for %d inputs", len(got), len(inputs))
	}
	failed, spaces := 0, 0
	for i, input := range inputs {
		r, _ := utf8.DecodeRuneInString(input)
		if i%2 == 0 && unicode.IsSpace(r) {
			spaces++
		}
		if want := model.CanonicalLogin(input); got[i] != want {
			failed++
			if failed <= 10 {
				t.Errorf("canonical_login(%+q) = %+q, model.CanonicalLogin = %+q (U+%04X)", input, got[i], want, r)
			}
		}
	}
	if failed > 10 {
		t.Errorf("and %d more characters on which canonical_login and model.CanonicalLogin disagree", failed-10)
	}
	// The cases above hold every White_Space character Go trims; a run that read none would pass
	// on a function that trims nothing.
	if spaces != 25 {
		t.Fatalf("the cases held %d characters unicode.IsSpace reads as space, want the 25 of Unicode's White_Space", spaces)
	}
}
