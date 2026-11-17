// Command natsfloor answers, for each nats-server version it is given, whether the floor the Envoy
// listener and the Legion daemon refuse a server by takes it: natsversion.Refusal, the one rule.
// It is that rule for a caller that is not Go, so none holds a copy of it: the host smoke of
// claude-envoy (packages/claude-envoy/scripts/smoke-session-title.sh), which refuses to start a
// nats-server the listener would refuse, and the pin scanner of the live proofs
// (scripts/e2e/lib/nats-image.test.ts), which refuses a host install below the floor.
//
//	natsfloor VERSION...  one line per version: "taken\tVERSION" or "refused\tVERSION\tREASON"
//	natsfloor -releases   natsversion.FixedReleases, the releases every refusal names
//
// It exits 0 once it has answered, whatever the answers, so a caller reads the answer and not the
// exit, and 2 when it is given nothing to answer.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/sjawhar/envoy/natsversion"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "-releases" {
		fmt.Fprintln(stdout, natsversion.FixedReleases)
		return 0
	}
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: natsfloor VERSION... | natsfloor -releases")
		return 2
	}
	for _, version := range args {
		if err := natsversion.Refusal(version); err != nil {
			fmt.Fprintf(stdout, "refused\t%s\t%s\n", version, err)
			continue
		}
		fmt.Fprintf(stdout, "taken\t%s\n", version)
	}
	return 0
}
