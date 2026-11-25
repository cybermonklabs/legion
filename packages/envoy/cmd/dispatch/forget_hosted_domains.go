package main

import (
	"context"
	"fmt"
	"io"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// forgetHostedDomainsUsage is what `envoy-dispatch forget-hosted-domains` prints for arguments it
// cannot take.
const forgetHostedDomainsUsage = "forget-hosted-domains: takes no argument; run `envoy-dispatch forget-hosted-domains` " +
	"once, with every Dispatch process stopped, after the Workspace's domain changed"

// forgetHostedDomains runs `envoy-dispatch forget-hosted-domains`, the deployment operator's step
// in an intended change of the Workspace's domain: it forgets the Workspace domain every people
// row recorded at its last sign-in or confirmation (store.PgPeopleStore.ForgetHostedDomains), so
// the next boot, under the new DISPATCH_SIGNIN_DOMAIN, has no recorded domain to refuse it against
// (checkPeopleAtBoot). Each row records its domain again at its next confirmation. The operator
// stops every Dispatch process first and runs this once Google gives the new hd: a process still
// running with the old domain records it again at its next confirmation, and one with the new
// domain before Google's change holds each full member it confirms. It changes nothing else, holds
// nobody and releases nobody. It exits 0 once it forgot the domains, printing how many rows it
// changed, 1 when the database cannot be opened or written, and 2 for any argument.
func forgetHostedDomains(ctx context.Context, args []string, databaseURL string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(stderr, forgetHostedDomainsUsage)
		return 2
	}
	database, ok := openMigrated(ctx, "forget-hosted-domains", databaseURL, stderr)
	if !ok {
		return 1
	}
	defer database.Pool.Close()
	// The people store seals refresh tokens under a signing key, and the forget reads and writes
	// only hosted_domain: a key of this run alone, which seals and opens nothing, so the command
	// needs no DISPATCH_SIGNING_KEY.
	key, err := auth.NewSigningKey()
	if err != nil {
		fmt.Fprintf(stderr, "forget-hosted-domains: %v\n", err)
		return 1
	}
	forgotten, err := store.NewPgPeopleStore(database.Pool, auth.SingleSigningKey(key), nil).ForgetHostedDomains(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "forget-hosted-domains: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "forget-hosted-domains: %d people rows no longer record a Workspace domain; each records one again at its next confirmation\n", forgotten)
	return 0
}
