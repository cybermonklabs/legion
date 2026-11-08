// Command restateservice is the process Restate calls.
//
// Restate is a server rather than a library: the handlers under test do not run inside
// the driver and do not run inside the server, they run here, behind an HTTP/2 endpoint
// the server invokes and re-invokes. That is why the Restate experiments need this
// binary -- and it is also what makes their central measurement possible, since this
// process can be killed without taking the driver or the server with it.
//
// It prints `restateservice: key=value` lines for everything its driver waits on, the
// first of them always its own process id, and then serves until it is stopped.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
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
	address := flag.String("address", "", "the loopback HOST:PORT this endpoint listens on, which is also the deployment URL Restate is given (required)")
	workDir := flag.String("work-dir", "", "the scratch directory the handlers' files live in (required)")
	variant := flag.String("variant", exp12.VariantRecorded, "experiment 12 only: which version of the handler's code to serve, recorded or changed")
	flag.Parse()

	if err := run(*mode, *address, *workDir, *variant); err != nil {
		fmt.Fprintf(os.Stderr, "restateservice: %v\n", err)
		os.Exit(1)
	}
}

func run(mode, address, workDir, variant string) error {
	for name, value := range map[string]string{"-address": address, "-work-dir": workDir} {
		if value == "" {
			return fmt.Errorf("%s is required", name)
		}
	}

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

	if err := restatex.Serve(ctx, address, definitions...); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
