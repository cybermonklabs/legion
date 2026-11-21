package main

// Red test D6 of the acceptance of t-format at ffdc8fef0f8e85a4773c37fec90e09232a974cdd, in Go from
// the tester's script d6-way-out-names-a-whole-id.sh, so that the package's run holds it.
//
// The requirement: when print refuses a run because a file holds a node that is not in its printed
// form, its line names the command that writes that file whole ("name each node of it: --id <id>
// ..."). A writer who runs exactly those ids must get a run that print accepts.
//
// The test builds spec-check, writes a tree whose file acme-keeps-each-order.yaml holds a valid
// statement id of 154 bytes (over the cut of 100 bytes) and a space at the end of one line of
// another node of that file, runs print --id each-order-is-kept, takes each --id of the line as it
// is, and runs print with them. At ffdc8fef0f the line cuts the long id ("w-aaaa…aaaa-z"), and print
// refuses the ids of its own line: the approved spec holds no node "w-aaa…aaa-z".

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cmdtest"
)

// TestTheWayOutOfPrintNamesAWholeID holds that a line of spec-check does not cut an id of the way
// out of print, a value that the line tells the reader to type: print refuses a run over a file
// that holds a node that is not in its printed form and a node whose id is longer than the cut of
// 100 bytes, and a run of print with exactly the --id words of that line exits 0.
func TestTheWayOutOfPrintNamesAWholeID(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	longID := "w-" + strings.Repeat("a", 150) + "-z"
	check := func(id, serves, claim string) string {
		return "---\ntype: check\nid: " + id + "\nserves: " + serves + "\nsubject: code\nprocedure: program\ntest:\n  runner: go\n" +
			"  selector: example.com/acme/orders TestEachOrderIsKept\n  job: go-checks\nclaim: " + claim + "\n" +
			"procedure_text:\n  - The program stores one order and reads the orders.\n  - It must find the order.\n" +
			"failing_example:\n  - An order service that keeps no order.\n"
	}
	documents := map[string]string{
		"acme-keeps-each-order":     "---\ntype: requirement\nid: acme-keeps-each-order\nkind: behavior\ntext:\n  - ACME keeps each order that a customer sends.\n",
		"each-order-is-kept":        check("each-order-is-kept", "acme-keeps-each-order", "The order service keeps each order."),
		longID:                      "---\ntype: requirement\nid: " + longID + "\nkind: behavior\nparent: acme-keeps-each-order\ntext:\n  - ACME keeps the way-out order.\n",
		"the-way-out-order-is-kept": check("the-way-out-order-is-kept", longID, "The order service keeps the way-out order."),
	}
	approved := writeApproved(t, documents)
	spec := filepath.Join(t.TempDir(), "repo", ".dispatch", "spec")
	printAll := []string{"print", "--approved", approved}
	for _, id := range []string{"acme-keeps-each-order", "each-order-is-kept", longID, "the-way-out-order-is-kept"} {
		printAll = append(printAll, "--id", id)
	}
	if got := runBinary(t, binary, "", append(printAll, "--dir", spec)...); got.code != 0 {
		t.Fatalf("SETUP: print of the tree: exit %d, stderr %q", got.code, got.stderr)
	}
	replaceIn(t, filepath.Join(spec, "acme-keeps-each-order.yaml"), "kind: behavior\n", "kind: behavior \n")
	refused := runBinary(t, binary, "", "print", "--approved", approved, "--id", "each-order-is-kept", "--dir", spec)
	const marker = "name each node of it: "
	_, after, found := strings.Cut(refused.stderr, marker)
	if refused.code != 2 || !found {
		t.Fatalf("SETUP: print must refuse and name the way out: exit %d: %s", refused.code, refused.stderr)
	}
	line, _, _ := strings.Cut(after, "\n")
	words := strings.Fields(line)
	again := []string{"print", "--approved", approved}
	var ids []string
	for i := 0; i+1 < len(words); i++ {
		if words[i] == "--id" {
			ids = append(ids, words[i+1])
			again = append(again, "--id", words[i+1])
		}
	}
	t.Logf("the line names: %q", ids)
	if got := runBinary(t, binary, "", append(again, "--dir", spec)...); got.code != 0 {
		t.Errorf("print refuses the ids that its own way out names (a valid id longer than the cut): print with exactly those ids: exit %d: %s", got.code, got.stdout+got.stderr)
	}
}
