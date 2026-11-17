// Command fixturebuckets provisions a test's or a dev rig's own NATS server for a listener: it
// creates every bucket a listener copies, which no listener creates, and marks the
// server as a fixture's (bustest.Provision). A script runs it after it starts its NATS server and
// before it starts a listener on it:
//
//	fixturebuckets -url nats://127.0.0.1:4222 [-replicas 1]
//
// It is a test fixture, not a product: it refuses a server that is not on this machine's loopback,
// and nothing deployed runs it. The operator creates a deployment's buckets with `envoy-infra
// buckets`: its normal create once every writer of them has ended, its break-glass create while
// they run.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/bustest"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "fixturebuckets: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("fixturebuckets", flag.ContinueOnError)
	url := flags.String("url", "", "the fixture's own NATS server, on loopback")
	replicas := flags.Int("replicas", 1, "the copies each bucket is created with")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *url == "" || flags.NArg() > 0 {
		return fmt.Errorf("usage: fixturebuckets -url nats://127.0.0.1:<port> [-replicas N]")
	}
	if !bus.LocalNATSURL(*url) {
		return fmt.Errorf("%s is not a server on this machine's loopback: a fixture provisions only its own server", *url)
	}
	conn, err := nats.Connect(*url, nats.Name("fixturebuckets"))
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := bustest.Provision(conn, *replicas); err != nil {
		return err
	}
	fmt.Printf("provisioned %s: %v and %s\n", *url, bus.CopiedBuckets(), bustest.MarkerStream)
	return nil
}
