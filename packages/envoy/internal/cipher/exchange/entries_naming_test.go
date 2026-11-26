package exchange_test

import (
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
)

// TestEntriesNamingCountsEveryEntryThatNamesThePerson: the listed set names ada in five entries,
// two account entries, two workflow entries and one Google entry, and every one of them counts,
// whichever way the address is spelled; the read entry and the two Google entries that name no
// person count for nobody, not even for an empty person, and a person no entry names counts none.
// An entry that spells its own person in capitals names that person too, as the hold gate reads
// the person a token names (record.CanonicalLogin).
func TestEntriesNamingCountsEveryEntryThatNamesThePerson(t *testing.T) {
	set := listed(t)
	for _, c := range []struct {
		name, person string
		want         int
	}{
		{"the person five entries name", person, 5},
		{"that person in capitals with space around", " Ada@Example.COM\t", 5},
		{"a person no entry names", stranger, 0},
		{"no person, which the three entries naming nobody must not match", "", 0},
		{"a person of only space", " \t ", 0},
	} {
		if got := set.EntriesNaming(c.person); got != c.want {
			t.Errorf("EntriesNaming(%q), %s = %d, want %d", c.person, c.name, got, c.want)
		}
	}

	spelled, err := exchange.NewSet(
		[]exchange.AccountEntry{{Account: workerAccount, Project: project, Person: "Grace@Example.com"}},
		nil,
		[]exchange.WorkflowEntry{{Repository: repository, Workflow: workflow, Ref: ref, Project: project, Person: " grace@example.com"}},
		nil, signInDomain)
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}
	if got := spelled.EntriesNaming("grace@example.com"); got != 2 {
		t.Errorf("EntriesNaming(grace@example.com) on entries naming %q and %q = %d, want 2: an entry's own person is read as the hold reads it",
			"Grace@Example.com", " grace@example.com", got)
	}
}
