package principal

import (
	"strings"
	"testing"
)

// TestCheckPersonLoginTakesOnlyAStoredEmail: a login is taken only in the form the people table
// holds it, and each refusal says what is wrong with it.
func TestCheckPersonLoginTakesOnlyAStoredEmail(t *testing.T) {
	for _, login := range []string{"alice@example.com", "dave.o'neil+x@example.co.uk", "\u00e4@example.com"} {
		if err := CheckPersonLogin(login); err != nil {
			t.Errorf("CheckPersonLogin(%q) = %v, want nil", login, err)
		}
	}
	for _, row := range []struct{ login, says string }{
		{"", "no email"},
		{"alice", "no email"},
		{"@example.com", "no email"},
		{"alice@", "no email"},
		{"alice smith@example.com", "no email"},
		{"alice@example.com,bob@example.com", "no email"},
		{strings.Repeat("a", 310) + "@example.com", "no email"},
		{"alice\x07@example.com", "control"},
		{"alice\u202e@example.com", "bidi"},
		{"\u00a0alice@example.com", "space around"},
		{"Alice@example.com", "lowercase"},
		{"\u00c4@example.com", "lowercase"},
	} {
		err := CheckPersonLogin(row.login)
		if err == nil || !strings.Contains(err.Error(), row.says) {
			t.Errorf("CheckPersonLogin(%q) = %v, want a refusal saying %q", row.login, err, row.says)
		}
	}
}
