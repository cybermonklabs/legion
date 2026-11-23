// Command loopbackhost prints a loopback address for one NATS server a script starts itself, claimed
// for the script, so the server's listeners, and its clients' URL, name an address no other stack
// of this user listens on (testport.Claim). A script that runs natsd may leave the choice to natsd,
// which claims an address of its own when it is given none (-a); a script that restarts its server
// on the same address, or runs a nats-server binary or container, asks here once per run and passes
// the address to every start (`-a <address>`, `docker run -p <address>::4222`).
//
//	loopbackhost -owner PID          claim an address for PID and print it
//	loopbackhost -owner PID -release ADDRESS
//	                                 remove PID's claim on ADDRESS
//
// The owner is the script's own process id ($$), since a claim whose owner no longer runs is free
// again: a script that ends without releasing costs the next one nothing. On Linux the address is
// one testport.LoopbackIP draws, never 127.0.0.1, and is bound once on port 0 before it is printed,
// so an address that is not up fails here and names itself. Elsewhere it is 127.0.0.1.
//
// It imports the standard library and testport alone, so a script builds it in a second.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/sjawhar/envoy/internal/testport"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "loopbackhost: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("loopbackhost", flag.ContinueOnError)
	owner := flags.Int("owner", 0, "the process id the claim is held for (a script's $$)")
	release := flags.String("release", "", "remove the owner's claim on this address instead of claiming one")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments %q", flags.Args())
	}
	if *owner <= 0 {
		return fmt.Errorf("-owner names no process id")
	}
	if *release != "" {
		return testport.Release(testport.ClaimDir(), *release, *owner)
	}
	host, err := testport.Claim(testport.ClaimDir(), *owner)
	if err != nil {
		return err
	}
	fmt.Println(host)
	return nil
}
