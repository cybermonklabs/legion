// Command natsd runs one nats-server in this process, in the foreground, until it is signalled.
// It is the bus of packages/envoy/scripts/e2e-local.sh and packages/envoy/scripts/e2e-refill.sh,
// each of which builds it beside the listener it builds: an agent's machine has no container
// runtime, so neither script can run the NATS image. e2e-refill.sh also stops, kills and restarts
// it under a running listener, and a restart reads back the JetStream store the directory holds.
//
// It is a test fixture, not a product. Nothing deployed runs it: it listens on loopback only, has
// no users, keeps its JetStream store wherever the caller says and never cleans it up, and lives
// under internal/testnats so nothing outside this module can import it. The server the tests run
// is the one testnats starts (nats.go); this is the same server for a shell script's sake.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
)

// readyPace is how often the wait for the server to accept connections asks again; it bounds
// nothing. The server's own failures end this command: Start reports a listen error, a port
// another process holds among them, through the logger's Fatalf, which exits 1 naming it, and a
// server that shuts down before it accepts any connection ends the wait below. A server that is
// merely slow to start, as on a loaded machine, is waited for for as long as it takes, since every
// caller's own wait for the port has no bound either.
const readyPace = time.Second

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "natsd: %v\n", err)
		os.Exit(1)
	}
}

// run parses args as nats-server parses its own command line (-p for the client port, -store_dir
// for the JetStream store), starts the server with JetStream on 127.0.0.1 and its log on stdout,
// and returns once a signal has shut it down.
func run(args []string) error {
	flags := flag.NewFlagSet("natsd", flag.ContinueOnError)
	help := func() { flags.PrintDefaults() }
	opts, err := natsserver.ConfigureOptions(flags, args, help, help, help)
	if err != nil {
		return err
	}
	if opts == nil {
		// -v, -h or -help_tls: ConfigureOptions has printed what was asked for.
		return nil
	}
	opts.Host = "127.0.0.1"
	opts.JetStream = true
	opts.NoSigs = true
	srv, err := natsserver.NewServer(opts)
	if err != nil {
		return err
	}
	srv.ConfigureLogger()
	stopped := make(chan struct{})
	go func() {
		srv.WaitForShutdown()
		close(stopped)
	}()
	go srv.Start()
	for !srv.ReadyForConnections(readyPace) {
		select {
		case <-stopped:
			return fmt.Errorf("the server on %s:%d shut down before it accepted a connection", opts.Host, opts.Port)
		default:
		}
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	<-signals
	srv.Shutdown()
	srv.WaitForShutdown()
	return nil
}
