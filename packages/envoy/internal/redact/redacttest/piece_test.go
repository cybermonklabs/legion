package redacttest

import (
	"fmt"
	"strings"
	"testing"
)

// recorder is a testing.TB that keeps the failures an assertion reports, so a test of the
// assertion reads them instead of failing with them.
type recorder struct {
	testing.TB
	failures []string
}

func (r *recorder) Helper() {}

func (r *recorder) Logf(string, ...any) {}

func (r *recorder) Errorf(format string, args ...any) {
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}

// TestRequireNoPieceFailsOnEveryRunPieceReads: RequireNoPiece and KeptPrintTest read a shown run
// of a credential the request carried one way (Piece): 8 characters of it as written, or the 16
// hex digits of 8 of its bytes as fmt's %x and %X write them. So RequireNoPiece fails on each of
// those, and passes text that shows none: 7 characters of it, and their hex, which is 14 digits.
// The credential is none of Secrets, as a key's signed JWT is not, so RequireNone's own reading
// of a secret sees none of it.
func TestRequireNoPieceFailsOnEveryRunPieceReads(t *testing.T) {
	const credential = "eyJhbGciOiJFUzI1NiJ9.signed"
	for _, c := range []struct {
		name  string
		text  string
		fails bool
	}{
		{"as-written", "echoed " + credential[4:12], true},
		{"hex", fmt.Sprintf("echoed %x", credential[4:12]), true},
		{"upper-hex", fmt.Sprintf("echoed %X", credential[4:12]), true},
		{"seven", "echoed " + credential[4:11] + fmt.Sprintf(" %x", credential[4:11]), false},
		{"none", "echoed nothing of it", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := &recorder{TB: t}
			capture := "redacttest.piece/" + c.name
			RequireNoPiece(r, capture, c.text, credential)
			if fails := len(r.failures) > 0; fails != c.fails {
				t.Fatalf("RequireNoPiece of %q failed %v (%q), want %v", c.text, fails, r.failures, c.fails)
			}
			if c.fails && !strings.HasPrefix(r.failures[0], "REDACT-ASSERT "+capture+":") {
				t.Errorf("RequireNoPiece's failure %q is not its designated assertion's", r.failures[0])
			}
		})
	}
}
