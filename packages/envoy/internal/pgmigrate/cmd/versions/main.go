// Command versions reads what a Dispatch or broker database's version table records of each
// migration against the set this build embeds, and what the runner's next boot would do with each
// row: fill a row that records no name, move one that records an identity a file replaces, or
// refuse (pgmigrate.CheckVersions). It writes nothing: it reads in one read-only transaction.
//
//	DATABASE_URL=<url> go run ./internal/pgmigrate/cmd/versions dispatch
//	DATABASE_URL=<url> go run ./internal/pgmigrate/cmd/versions broker
//
// It exits 0 when the table records at least one version, every recorded version has a file in
// this build and the next boot refuses none; 1 when a version has no file, a row would be refused
// (the report names each) or the table records nothing, which it reports as NOTHING CHECKED; and
// 2 when the check could not be taken, as on a search_path that names no schema.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	brokerstore "github.com/sjawhar/envoy/internal/broker/store"
	dispatchstore "github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/pgmigrate"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Getenv("DATABASE_URL")))
}

func run(ctx context.Context, args []string, databaseURL string) int {
	checks := map[string]func(context.Context, string) (*pgmigrate.VersionCheck, error){
		"dispatch": dispatchstore.CheckVersions,
		"broker":   brokerstore.CheckVersions,
	}
	if len(args) != 1 || checks[args[0]] == nil {
		fmt.Fprintln(os.Stderr, "usage: DATABASE_URL=<url> versions dispatch|broker")
		return 2
	}
	if strings.TrimSpace(databaseURL) == "" {
		fmt.Fprintln(os.Stderr, "versions: DATABASE_URL is required")
		return 2
	}
	check, err := checks[args[0]](ctx, databaseURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "versions: %v\n", err)
		return 2
	}
	check.Write(os.Stdout)
	if !check.OK() {
		return 1
	}
	return 0
}
