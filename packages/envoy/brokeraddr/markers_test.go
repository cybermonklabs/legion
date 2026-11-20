package brokeraddr_test

import (
	"testing"

	"github.com/sjawhar/envoy/brokeraddr"
	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestEachMarkerNamesItsOwnCase: beside an address that is not clean, Words prints
// brokeraddr.WordsNotPrinted in place of any words, readable or not, since whoever answers there
// can repeat the address's credential; beside a clean address it prints words the rule for remote
// text cannot read as redact.WordsNotReadable, and words it can read as it reads them. Neither
// marker stands in for the other, and the two read differently.
func TestEachMarkerNamesItsOwnCase(t *testing.T) {
	clean := brokeraddr.New("https://broker.example")
	credentialed := brokeraddr.New("https://" + redacttest.User + ":" + redacttest.Password + "@broker.example")
	for _, row := range []struct {
		name  string
		addr  brokeraddr.Address
		words string
		want  string
	}{
		{"a clean address, words the rule cannot read", clean, redacttest.Unreadable, redact.WordsNotReadable},
		{"a clean address, words the rule can read", clean, redacttest.Words, redacttest.Shown},
		{"an address that is not clean, words the rule cannot read", credentialed, redacttest.Unreadable, brokeraddr.WordsNotPrinted},
		{"an address that is not clean, words the rule can read", credentialed, redacttest.Words, brokeraddr.WordsNotPrinted},
		{"an address that is not clean, plain words", credentialed, "no entry lists this account", brokeraddr.WordsNotPrinted},
	} {
		t.Run(row.name, func(t *testing.T) {
			got := row.addr.Words(row.words)
			redacttest.RequireNone(t, "brokeraddr.Words", got)
			if got != row.want {
				t.Errorf("Words(%q) = %q, want %q", row.words, got, row.want)
			}
		})
	}
	if brokeraddr.WordsNotPrinted == redact.WordsNotReadable {
		t.Errorf("the two markers read the same, %q", brokeraddr.WordsNotPrinted)
	}
}
