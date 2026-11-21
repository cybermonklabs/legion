// Command envoy-infra is envoy-infra's command line. seed writes a NATS nkey user seed into a
// secret store and checks and verifies the one stored there (package seed); render prints what the
// AWS components register, for a program that needs it outside Pulumi (package render); routes
// records a clustered hub's pods at its route authorization's env stage and checks its routes once
// authorization is on (package routes); and `envoy-infra --version` prints the release it was built
// as.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/sjawhar/envoy/infra/render"
	"github.com/sjawhar/envoy/infra/routes"
	"github.com/sjawhar/envoy/infra/seed"
)

var version = "0.0.0-dev" // -ldflags "-X main.version=<X.Y.Z>" in the release job

// stores are the secret stores `envoy-infra seed --store` names.
var stores = seed.Stores{seed.AWSSecretsManager: seed.AWS, seed.GCPSecretManager: seed.GCP}

// main cancels the command's context on an interrupt or SIGTERM rather than dying at once, so a
// write in flight ends as the command reports any write whose answer never came: with the public
// key it printed before the write and an error telling the operator to check the secret.
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, stores, routes.OpenKubectl)
	stop()
	os.Exit(code)
}

// run runs the command line args, the words after the program's name, and returns its exit code.
// stores are the seed command's stores, and open the Source the routes command reads a hub through.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, stores seed.Stores, open routes.Opener) int {
	if len(args) > 0 && args[0] == "seed" {
		return seed.Command(ctx, args[1:], stdout, stderr, stores)
	}
	if len(args) > 0 && args[0] == "render" {
		return render.Command(args[1:], stdout, stderr)
	}
	if len(args) > 0 && args[0] == "routes" {
		return routes.Command(ctx, args[1:], stdout, stderr, open)
	}
	if len(args) == 1 && args[0] == "--version" {
		if _, err := fmt.Fprintln(stdout, version); err != nil {
			fmt.Fprintf(stderr, "envoy-infra: could not print the version: %v\n", err)
			return 1
		}
		return 0
	}
	help := len(args) > 0 && (args[0] == "-h" || args[0] == "-help" || args[0] == "--help")
	if !help {
		// The words given are not echoed: a seed pasted there must not reach the terminal.
		fmt.Fprintln(stderr, "envoy-infra: the command is seed, render or routes, or --version alone")
	}
	fmt.Fprintln(stderr, "usage: envoy-infra --version")
	seed.Command(ctx, []string{"--help"}, stdout, stderr, stores)
	render.Command([]string{"--help"}, stdout, stderr)
	routes.Command(ctx, []string{"--help"}, stdout, stderr, open)
	if help {
		return 0
	}
	return 2
}
