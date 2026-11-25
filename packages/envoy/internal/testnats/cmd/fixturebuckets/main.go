// Command fixturebuckets provisions a test's or a dev rig's own NATS server for a listener: it
// creates every bucket a listener copies, which no listener creates, and marks the
// server as a fixture's (bustest.Provision). A script runs it after it starts its NATS server and
// before it starts a listener on it:
//
//	fixturebuckets -url nats://127.0.0.1:4222 [-replicas 1]
//	FIXTUREBUCKETS_URL=nats://127.0.0.1:4222 fixturebuckets [-replicas 1]
//
// A script whose URL can carry a credential, a password or a token, gives it in FIXTUREBUCKETS_URL:
// every user of the machine reads a process's arguments, and only its owner its environment.
//
// It is a test fixture, not a product: it refuses a server that is not on this machine's loopback,
// and nothing deployed runs it. The operator creates a deployment's buckets with `envoy-infra
// buckets`: its normal create once every writer of them has ended, its break-glass create while
// they run.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/bustest"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "fixturebuckets: %v\n", err)
		os.Exit(1)
	}
}

// run provisions the server args name, and prints what it provisioned to stdout. Its output and
// its errors name the server without the URL's user info (bus.NATSURLWithoutUserinfo): a rig can
// hand it a URL an operator gave, from which nats.go reads a password or a token, and the rig
// keeps the output in a log and prints it on a failure. The server is -url's, or FIXTUREBUCKETS_URL's
// when -url is not given.
func run(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("fixturebuckets", flag.ContinueOnError)
	url := flags.String("url", "", "the fixture's own NATS server, on loopback (FIXTUREBUCKETS_URL when not given)")
	replicas := flags.Int("replicas", 1, "the copies each bucket is created with")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *url == "" {
		*url = os.Getenv("FIXTUREBUCKETS_URL")
	}
	if *url == "" || flags.NArg() > 0 {
		return fmt.Errorf("usage: fixturebuckets -url nats://127.0.0.1:<port> [-replicas N], or FIXTUREBUCKETS_URL in place of -url")
	}
	// nats.go splits its URL on ',' into a pool it dials in random order, while the loopback check
	// below reads one host, the one after the authority's last '@'. A fixture provisions one
	// server, its own, so a list is refused whole rather than each server read: nothing a fixture
	// runs gives it one, and no reading of the list can drift from the one nats.go makes.
	if strings.Contains(*url, ",") {
		return fmt.Errorf("the URL names a list of servers (it holds a ','), and nats.go would dial any of them: a fixture provisions one server, its own, so give it that server alone")
	}
	server := bus.NATSURLWithoutUserinfo(*url)
	if !bus.LocalNATSURL(*url) {
		return fmt.Errorf("%s is not a server on this machine's loopback: a fixture provisions only its own server", server)
	}
	conn, err := nats.Connect(*url, nats.Name("fixturebuckets"))
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := bustest.Provision(conn, *replicas); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "provisioned %s: %v and %s\n", server, bus.CopiedBuckets(), bustest.MarkerStream)
	return nil
}
