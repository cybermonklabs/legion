// Command restateservice is the process Restate calls.
//
// Restate is a server rather than a library: the handlers under test do not run inside
// the driver and do not run inside the server, they run here, behind an HTTP/2 endpoint
// the server invokes and re-invokes. That is why the Restate experiments need this
// binary -- and it is also what makes their central measurement possible, since this
// process can be killed without taking the driver or the server with it.
//
// It prints `restateservice: key=value` lines for everything its driver waits on, the
// first of them always its own process id, and then serves until it is stopped, on the
// listening socket its driver hands over (-listen-fd): the driver holds the endpoint's
// port, so no other program can take it between this process's start and a
// replacement's.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	restate "github.com/restatedev/sdk-go"

	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/exp10"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/exp11"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/exp12"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/restatex"
)

func main() {
	mode := flag.String("mode", "", "which experiment's handlers to host: exp10, exp11 or exp12")
	listenFD := flag.Int("listen-fd", 0, "the descriptor of the socket listening on the endpoint's loopback port, which the driver holds and hands over; its address is also the deployment URL Restate is given (required)")
	workDir := flag.String("work-dir", "", "the scratch directory the handlers' files live in (required)")
	variant := flag.String("variant", exp12.VariantRecorded, "experiment 12 only: which version of the handler's code to serve, recorded or changed")
	flag.Parse()

	if err := run(*mode, *listenFD, *workDir, *variant); err != nil {
		fmt.Fprintf(os.Stderr, "restateservice: %v\n", err)
		os.Exit(1)
	}
}

func run(mode string, listenFD int, workDir, variant string) error {
	if listenFD < 3 {
		return fmt.Errorf("-listen-fd is required: the descriptor of the listening socket the driver hands over, 3 or above, not %d", listenFD)
	}
	if workDir == "" {
		return fmt.Errorf("-work-dir is required")
	}
	file := os.NewFile(uintptr(listenFD), "restateservice-listener")
	listener, err := net.FileListener(file)
	file.Close()
	if err != nil {
		return fmt.Errorf("-listen-fd %d is not a listening socket: %w", listenFD, err)
	}
	defer listener.Close()

	// The driver stops a service by killing its process group; a service that is
	// asked to stop politely gets here and shuts its endpoint down.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var definitions []restate.ServiceDefinition
	switch mode {
	case "exp10":
		definitions = []restate.ServiceDefinition{exp10.Service()}
	case "exp11":
		definitions = exp11.Services()
	case "exp12":
		definition, err := exp12.Service(variant)
		if err != nil {
			return err
		}
		definitions = []restate.ServiceDefinition{definition}
	default:
		return fmt.Errorf("-mode must be exp10, exp11 or exp12, got %q", mode)
	}

	if err := restatex.Serve(ctx, listener, definitions...); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
