package main

import (
	"testing"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/redact"
)

// TestATableCellOfOneFieldPrintsOneWay: a grant's and a machine login's cells of one field of the
// broker's answer print through one rule (fieldCell): the field as it prints itself beside a clean
// address, "-" for a field the answer leaves null or empty there - the granted time among them,
// which printed as an empty cell - and cipheraddr.WordsNotPrinted beside an address that is not
// clean, whatever the field holds.
func TestATableCellOfOneFieldPrintsOneWay(t *testing.T) {
	clean := cipheraddr.New("https://broker.example")
	credentialed := cipheraddr.New("https://" + "ada:pass-7Qz4k@broker.example")
	approver := redact.NewRemoteText("ada@example.com")
	none := OperatorGrant{GrantID: redact.NewRemoteText("0f9a6c1e-2b7d-4c3a-9e5f-8a1b2c3d4e5f")}
	some := none
	some.Granted, some.Approver = redact.NewRemoteText("2026-11-24T00:00:00Z"), &approver
	for _, c := range []struct {
		name string
		got  []string
		want []string
	}{
		{"grant-none-clean", grantRow(clean, none), []string{"-", "-", "-", "-"}},
		{"grant-some-clean", grantRow(clean, some), []string{"2026-11-24T00:00:00Z", "ada@example.com", "-", "-"}},
		{"grant-none-credentialed", grantRow(credentialed, none), []string{cipheraddr.WordsNotPrinted, cipheraddr.WordsNotPrinted, cipheraddr.WordsNotPrinted, cipheraddr.WordsNotPrinted}},
	} {
		// GRANTED, APPROVER, DECIDED_BY and OPERATOR are the four cells of one field.
		got := []string{c.got[2], c.got[3], c.got[4], c.got[6]}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: the one-field cells are %q; want %q", c.name, got, c.want)
				break
			}
		}
	}
	for _, c := range []struct {
		name string
		base cipheraddr.Address
		m    OperatorMachine
		want string
	}{
		{"machine-none-clean", clean, OperatorMachine{}, "-"},
		{"machine-some-clean", clean, OperatorMachine{ApprovedBy: approver}, "ada@example.com"},
		{"machine-none-credentialed", credentialed, OperatorMachine{}, cipheraddr.WordsNotPrinted},
	} {
		if got := machineRow(c.base, c.m)[2]; got != c.want {
			t.Errorf("%s: the approver cell is %q; want %q", c.name, got, c.want)
		}
	}
}
