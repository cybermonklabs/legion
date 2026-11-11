package store

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The three documents the connection-count check reads, and the two constants they state. Three
// rounds of dispatch://INFRA-558 went to one class: a merge raises LeasePoolSize or
// ConnectionsPerProcess, every test of both sides stays green because no test reads a sentence,
// and the number a person reads is left behind. This row reads those sentences, so the next raise
// fails here instead of in a person's reading.
//
// Each pattern below captures the number one sentence states and names the constant it must
// equal. Every pattern reads a word as readily as digits, in all three files: the sentence of
// .dispatch/architecture/dispatch-store.md spells its count, two searches of this tree missed it
// for that reason, and a row that read digits alone would pass the very file that failed the
// round this row was written in.
//
// A pattern that matches nothing fails too: a sentence someone reworded or deleted would
// otherwise leave this row passing over nothing, which is the same fault in a different place.
//
// Keep it to these three documents and these two constants. A document that states no count needs
// no entry, and a sentence that states a number of something else (a migration version, a review
// round) belongs to no pattern here.
func TestTheDocumentsStateTheCountsThisPackageHolds(t *testing.T) {
	root := filepath.Join("..", "..", "..", "..", "..")
	const (
		site      = "docs/site/src/content/docs/dispatch/running-dispatch.md"
		readme    = "packages/envoy/cmd/dispatch/README.md"
		storeArch = ".dispatch/architecture/dispatch-store.md"
	)
	// `number` is the one group of each pattern: digits, or the count written as a word.
	const number = `(\d+|[a-z]+(?:-[a-z]+)?)`
	twoProcesses := 2 * ConnectionsPerProcess
	for _, want := range []struct {
		file    string
		pattern string
		number  int
		says    string
	}{
		{site, `up to ` + number + ` for the work it runs once across the servers`, LeasePoolSize,
			"the connections one server opens for the work it runs once across the servers"},
		{site, `That is\s+` + number + `\s+connections for one server at its ceiling`, ConnectionsPerProcess,
			"the connections one server opens at its ceiling"},
		{readme, `callers of the two intakes' holds, ` + number, LeasePoolSize,
			"the lease pool's connections, one per leased name and per hold caller"},
		{readme, `= ` + number + ` connections to Postgres`, ConnectionsPerProcess,
			"the sum of the counted pools and the permanent listeners"},
		{readme, `opens at most 64 \+ ` + number + ` \+`, LeasePoolSize,
			"the lease term of that sum, which states the pool's own size inside it"},
		{readme, `\| Leases \(` + "`store.LeasePoolSize`" + `\) \| ` + number + ` \|`, LeasePoolSize,
			"the lease pool's row of the pool table"},
		{readme, `Two processes at their ceiling hold ` + number, twoProcesses,
			"two processes at their ceiling"},
		{storeArch, `per caller of a delivery intake's hold, ` + number, LeasePoolSize,
			"the lease pool's connections, one per leased loop and per hold caller"},
	} {
		body, err := os.ReadFile(filepath.Join(root, want.file))
		if err != nil {
			t.Fatalf("read %s: %v", want.file, err)
		}
		stated := regexp.MustCompile(want.pattern).FindAllStringSubmatch(string(body), -1)
		if len(stated) == 0 {
			t.Errorf("%s states nothing this pattern reads, so the number it holds is unchecked: %s (%s)",
				want.file, want.pattern, want.says)
			continue
		}
		digits, word := strconv.Itoa(want.number), numberAsAWord(t, want.number)
		for _, match := range stated {
			said := match[1]
			if said == digits || said == word {
				continue
			}
			t.Errorf("%s says %q of %s, want %s or %q: %s",
				want.file, said, want.says, digits, word, strings.TrimSpace(match[0]))
		}
	}
}

// numberAsAWord gives the count as a sentence that spells it writes it. A count these documents
// would spell and this table has no word for is a count the row cannot read: it fails here, where
// the fault is, rather than reporting a document that states its own number correctly. A count of
// a hundred or more is never spelled in these three documents - they write 109 and 218 in digits -
// so a word is neither wanted nor wanted-for there.
func numberAsAWord(t *testing.T, number int) string {
	t.Helper()
	spelled := map[int]string{
		13: "thirteen", 14: "fourteen", 15: "fifteen", 16: "sixteen", 17: "seventeen",
		18: "eighteen", 19: "nineteen", 20: "twenty", 21: "twenty-one", 22: "twenty-two",
		23: "twenty-three", 24: "twenty-four", 25: "twenty-five", 26: "twenty-six",
		27: "twenty-seven", 28: "twenty-eight", 29: "twenty-nine", 30: "thirty",
	}
	if number >= 100 {
		return strconv.Itoa(number)
	}
	word, ok := spelled[number]
	if !ok {
		t.Fatalf("this row has no word for %d, which a sentence spelling its count would write: "+
			"add it to the table rather than reading a document as wrong", number)
	}
	return word
}
