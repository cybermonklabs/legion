// packages/envoy/cmd/dev-proxy-stands/probe.go
// The `probe-private` and `probe-bound` subcommands: the two Go clients the airlock's checks
// drive, so that what a Go consumer behind the airlock does is read off a Go program rather
// than inferred from curl.
//
// They are subcommands of this binary rather than programs scripts/airlock-check.sh writes out
// and builds, so that CI compiles and vets them with everything else here. Each is still a
// plain Go HTTP client: it reads the proxy, NO_PROXY and the trust bundle from its own
// environment, as net/http does for any Go program, and that is the whole point of the probe.
// probe-bound's request carries a bearer, so its client follows no redirect and its failure
// prints through redact; neither changes how it reads the proxy or the trust bundle.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/redirects"
)

// privateAddress is the private address literal check k4's probe asks for. A literal, not a
// name: what the check reads is what a Go program does with an address the airlock refuses,
// and a name would measure the resolver as well.
const privateAddress = "https://10.11.12.13/"

// runProbePrivate is `dev-airlock-stands probe-private`: one GET of a private address literal,
// printing the status or the error with the milliseconds it took. It takes the proxy from the
// environment as any Go program does, so what it does with the literal is what a Go consumer
// behind the airlock would do.
func runProbePrivate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dev-airlock-stands probe-private", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsageError
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "dev-airlock-stands probe-private: unexpected argument %q\n", fs.Arg(0))
		return exitUsageError
	}
	start := time.Now()
	client := &http.Client{Timeout: 8 * time.Second}
	answer, err := client.Get(privateAddress)
	if err != nil {
		fmt.Fprintf(stdout, "error after %dms: %v\n", time.Since(start).Milliseconds(), err)
		return 0
	}
	defer func() { _ = answer.Body.Close() }()
	fmt.Fprintf(stdout, "status %d after %dms\n", answer.StatusCode, time.Since(start).Milliseconds())
	return 0
}

// runProbeBound is `dev-airlock-stands probe-bound`: check k7's Go client, one GET of
// AIRLOCK_S_URL/go carrying AIRLOCK_DEMO_TOKEN as its Bearer, printing the status code alone.
// The two names come from the environment because the grant the check runs it under is what
// puts them there, beside the proxy and the trust bundle net/http reads for itself.
func runProbeBound(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dev-airlock-stands probe-bound", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsageError
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "dev-airlock-stands probe-bound: unexpected argument %q\n", fs.Arg(0))
		return exitUsageError
	}
	address := os.Getenv("AIRLOCK_S_URL") + "/go"
	request, err := http.NewRequest(http.MethodGet, address, nil)
	if err != nil {
		fmt.Fprintln(stdout, "error", err)
		return 0
	}
	request.Header.Set("Authorization", "Bearer "+os.Getenv("AIRLOCK_DEMO_TOKEN"))
	// The request carries the bearer, so its client follows no redirect (redirects.Refused): a
	// Location is the answering party's words, and a 3xx prints as its status like any answer.
	// net/http's error can still quote one (a Location that does not parse, read before any
	// redirect policy), so it prints as the bound host's address allows (redact.Cause): a
	// transport failure's own text, or its class.
	answer, err := redirects.Refused(http.DefaultClient).Do(request)
	if err != nil {
		fmt.Fprintln(stdout, "error", redact.Cause(err, address))
		return 0
	}
	defer func() { _ = answer.Body.Close() }()
	fmt.Fprintln(stdout, answer.StatusCode)
	return 0
}
