package specgraphtest

import "testing"

// A check written as a query or an agent check loses its test part and nothing else: the printer
// writes time_limit between test and claim, so a cut from the test to the claim would lose the
// time limit too. Written as a program check, the document keeps its test part.
func TestWrittenAsTakesOutOnlyTheTestPart(t *testing.T) {
	t.Parallel()
	const head = "---\ntype: check\nid: a-check\nserves: a-statement\nsubject: code\nprocedure: program\n"
	const test = "test:\n  runner: go\n  selector: example.com/m TestIt\n  job: go-checks\n"
	const tail = "time_limit: 1h0m0s\nclaim: The page shows it.\nprocedure_text:\n  - It opens the page.\nfailing_example:\n  - A page that shows nothing.\n"
	for _, row := range []struct {
		procedure, want string
	}{
		{"agent", "---\ntype: check\nid: a-check\nserves: a-statement\nsubject: production\nprocedure: agent\n" + tail},
		{"query", "---\ntype: check\nid: a-check\nserves: a-statement\nsubject: production\nprocedure: query\n" + tail},
		{"program", "---\ntype: check\nid: a-check\nserves: a-statement\nsubject: production\nprocedure: program\n" + test + tail},
	} {
		got, err := writtenAs(head+test+tail, row.procedure, "production")
		if err != nil {
			t.Fatal(err)
		}
		if got != row.want {
			t.Errorf("as %s:\n got %q\nwant %q", row.procedure, got, row.want)
		}
	}
}
