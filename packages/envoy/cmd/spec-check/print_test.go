package main

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/specgraph"
)

// testPrint runs print over the fixtures of testdata/compare: it writes the current approved block
// of each node it is named, and of each check of a statement it is named, byte for byte as the
// files hold them. A copy of what it writes passes the comparison, and the same copy with one word
// more fails it on that statement (v-two-specs: "A change copies the text that Dispatch prints for
// an approved statement and its checks, and it passes the comparison. The same text with one word
// changed fails it").
func testPrint(t *testing.T, binary string) {
	t.Run("print writes a statement and its two checks as the files hold them", func(t *testing.T) {
		t.Parallel()
		approved := fixture(t, "compare", "two-checks", "approved.json")
		want := `:::requirement{#an-order-survives-a-restart kind="behavior" parent="acme-keeps-each-order"}
ACME keeps each stored order after the order service starts again.
:::

:::check{#a-crash-keeps-each-order serves="an-order-survives-a-restart" subject="code" procedure="program" runner="go" selector="example.com/acme/orders TestACrashKeepsEachOrder" job="go-checks"}
Claim: The order service reads each stored order after a crash stops it and it starts again.
Procedure: The program stores one order, stops the order service with a kill signal, starts it again and reads the orders.
It must find the order one time.
Failing example: An order service that writes its orders to disk only when it stops normally.
:::

:::check{#a-restart-keeps-each-order serves="an-order-survives-a-restart" subject="code" procedure="program" runner="go" selector="example.com/acme/orders TestARestartKeepsEachOrder" job="go-checks"}
Claim: The order service reads each stored order after it starts again.
Procedure: The program stores one order, starts the order service again and reads the orders.
It must find the order one time.
Failing example: An order service that keeps its orders in memory only.
:::
`
		expect(t, runBinary(t, binary, "", "print", "--approved", approved, "--id", "an-order-survives-a-restart"), result{stdout: want})
		files, err := os.ReadFile(fixture(t, "compare", "two-checks", "spec", "orders.md"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(files), want) {
			t.Errorf("the files of two-checks do not hold the printed text %q", want)
		}
	})
	t.Run("print writes a component, an interface and a term, and each block once", func(t *testing.T) {
		t.Parallel()
		approved := fixture(t, "compare", "approved-text", "approved.json")
		want := ":::component{#orders-to-billing name=\"Orders to billing\" kind=\"interface\" joins=\"billing orders\"}\nOrders sends each order to billing through it.\n:::\n\n" +
			":::term{#order name=\"order\"}\nAn order is a request of a customer for one or more products.\n:::\n\n" +
			":::component{#billing name=\"Billing\"}\nIt sends a bill for each order.\n:::\n"
		expect(t, runBinary(t, binary, "", "print", "--approved", approved, "--id", "orders-to-billing", "--id", "order", "--id", "billing", "--id", "order"), result{stdout: want})
	})
	t.Run("print exits 2 on an id the approved spec does not hold or removed", func(t *testing.T) {
		t.Parallel()
		approved := fixture(t, "compare", "removed-kept", "approved.json")
		expect(t, runBinary(t, binary, "", "print", "--approved", approved, "--id", "acme-keeps-each-order", "--id", "an-order-is-paid-once", "--id", "each-order-gets-one-bill"), result{
			code: 2,
			stderr: "spec-check: " + approved + `: the approved spec removed node "an-order-is-paid-once", so it has no current text` + "\n" +
				"spec-check: " + approved + `: the approved spec holds no node "each-order-gets-one-bill"` + "\n",
		})
	})

	entries, err := os.ReadDir(fixture(t, "compare"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		t.Run("a copy of the printed text of "+entry.Name()+" passes compare, and one word more fails it", func(t *testing.T) {
			t.Parallel()
			approved := fixture(t, "compare", entry.Name(), "approved.json")
			content, err := os.ReadFile(approved)
			if err != nil {
				t.Fatal(err)
			}
			a, err := specgraph.ReadApproved(strings.NewReader(string(content)))
			if err != nil {
				t.Fatal(err)
			}
			current, _ := a.Current()
			args := []string{"print", "--approved", approved}
			for id, n := range a.Nodes {
				if !*n.Removed {
					args = append(args, "--id", id)
				}
			}
			printed := runBinary(t, binary, "", args...)
			if printed.code != 0 || printed.stderr != "" {
				t.Fatalf("print: exit %d, stderr %q", printed.code, printed.stderr)
			}
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "spec.md"), []byte(printed.stdout))
			n := strconv.Itoa(len(current.Statements))
			expect(t, runBinary(t, binary, "", "compare", "--dir", dir, "--approved", approved), result{
				stdout: passLine(dir, n+" of "+n),
			})

			lines := strings.Split(printed.stdout, "\n")
			opening := slices.IndexFunc(lines, func(line string) bool { return strings.HasPrefix(line, ":::requirement{#") })
			if opening < 0 {
				t.Fatal("the printed text holds no statement")
			}
			id, _, _ := strings.Cut(strings.TrimPrefix(lines[opening], ":::requirement{#"), " ")
			lines[opening+1] = "planted " + lines[opening+1]
			writeFile(t, filepath.Join(dir, "spec.md"), []byte(strings.Join(lines, "\n")))
			expect(t, runBinary(t, binary, "", "compare", "--dir", dir, "--approved", approved), result{
				code:   1,
				stdout: "spec-check: " + dir + ": the comparison fails on 1 node\n",
				stderr: "spec-check: " + filepath.Join(dir, "spec.md") + ":" + strconv.Itoa(opening+1) + ": " + id + ": " + unapprovedText + "\n",
			})
		})
	}
}
