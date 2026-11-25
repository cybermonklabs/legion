package main

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAGrantCellOfSeveralFieldsPrintsNothingBesideAnAddressThatIsNotClean: grantRow's SECRETS and
// SESSION cells join the readings of several of the broker's fields, and beside an address that is
// not clean each prints cipheraddr.WordsNotPrinted (cipheraddr.Address.Words), whatever its fields
// hold (secrets, session): whoever answers there can repeat a credential the address carries where
// no request's record lists it - here a token in its path, which the '=' before it makes not clean -
// and a field's own reading prints such a token as it came. EXPIRES, this CLI's own words, prints
// the time whatever the address. Twin: the address is not clean, and a field holding the token reads
// as the token (RemoteText.String), so the row can see it printed. Captures
// agent-secrets.grant-cells/<case>.
func TestAGrantCellOfSeveralFieldsPrintsNothingBesideAnAddressThatIsNotClean(t *testing.T) {
	base := cipheraddr.New("https://broker.example/k=" + redacttest.Token)
	echo, plain := redact.NewRemoteText(redacttest.Token), redact.NewRemoteText("plain")
	at := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	t.Run("twin", func(t *testing.T) {
		if base.Clean() {
			t.Fatal("the address is clean; the row needs one that is not")
		}
		if got := echo.String(); !strings.Contains(got, redacttest.Token) {
			t.Fatalf("the field reads %q; want the token as it came, so the row can see it printed", got)
		}
	})
	for _, c := range []struct {
		name   string
		column int
		grant  OperatorGrant
	}{
		{"secrets", 1, OperatorGrant{GrantID: plain, Names: []redact.RemoteText{plain, echo},
			Enrollment: OperatorGrantEnrollment{Kind: plain, RuntimeID: plain}, ExpiresAt: &at}},
		{"session", 5, OperatorGrant{GrantID: plain, Names: []redact.RemoteText{plain},
			Enrollment: OperatorGrantEnrollment{Kind: plain, RuntimeID: echo, Slot: &echo}, ExpiresAt: &at}},
	} {
		t.Run(c.name, func(t *testing.T) {
			row := grantRow(base, c.grant)
			redacttest.RequireNoPiece(t, "agent-secrets.grant-cells/"+c.name, strings.Join(row, "\t"))
			if row[c.column] != cipheraddr.WordsNotPrinted {
				t.Errorf("the %s cell is %q; want %q", grantColumns[c.column], row[c.column], cipheraddr.WordsNotPrinted)
			}
			if want := at.Format(time.RFC3339); row[7] != want {
				t.Errorf("the EXPIRES cell is %q; want %q", row[7], want)
			}
		})
	}
}
