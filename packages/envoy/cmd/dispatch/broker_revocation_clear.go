package main

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// clearBrokerRevocationUsage is what `envoy-dispatch clear-broker-revocation` prints for arguments it
// cannot take.
const clearBrokerRevocationUsage = "clear-broker-revocation: list the broker revocations owed for an address with " +
	"`envoy-dispatch clear-broker-revocation <email>`, then settle the ones the secrets broker never confirms with " +
	"`envoy-dispatch clear-broker-revocation <email> <subject>:<generation>...`, naming each as the list printed it"

// clearBrokerRevocation runs `envoy-dispatch clear-broker-revocation <email> [<subject>:<generation>...]`,
// the operator's way out of a revocation Dispatch owes the secrets broker that the broker never
// confirms, which envoy_dispatch_broker_revocation_oldest_owed_seconds shows and which the loop
// keeps asking the broker for. With the address alone it lists every revocation of the address
// still owed, one line each naming the replaced account and the generation as the clear takes
// them, and settles nothing. Given those names it settles exactly the revocations they name, each
// only at the generation named and only while it is owed (store.BrokerRevocations.Clear): one
// recorded after the operator's list, or owed again under a later generation, stays owed, for the
// operator to list and clear again. It changes nothing Dispatch admits, which holds every broker
// token to its own account or creation time (api.brokerAccountReplaced), and it revokes nothing at
// the broker: a credential of the replaced account that the broker still holds goes on reading the
// secrets the broker serves without asking Dispatch, so the operator revokes it at the broker first.
// It exits 0 once it listed what is owed or settled every revocation named, printing each, 1 when
// nothing of the address is owed, when a revocation named is not owed at that generation, or when
// the database cannot be opened, read or written, and 2 for anything but one non-empty address and
// well-formed names.
func clearBrokerRevocation(ctx context.Context, args []string, databaseURL string, stdout, stderr io.Writer) int {
	if len(args) == 0 || strings.TrimSpace(args[0]) == "" {
		fmt.Fprintln(stderr, clearBrokerRevocationUsage)
		return 2
	}
	seen := make([]store.BrokerRevocationSeen, 0, len(args)-1)
	for _, arg := range args[1:] {
		at := strings.LastIndex(arg, ":")
		subject, generation := arg[:max(at, 0)], arg[at+1:]
		number, err := strconv.ParseInt(generation, 10, 64)
		if at < 0 || subject == "" || err != nil || number < 1 {
			fmt.Fprintf(stderr, "clear-broker-revocation: %q is not <subject>:<generation> as the list prints it\n%s\n", arg, clearBrokerRevocationUsage)
			return 2
		}
		seen = append(seen, store.BrokerRevocationSeen{Subject: subject, Generation: number})
	}
	database, ok := openMigrated(ctx, "clear-broker-revocation", databaseURL, stderr)
	if !ok {
		return 1
	}
	defer database.Pool.Close()
	email := model.CanonicalLogin(args[0])
	revocations := store.NewBrokerRevocations(database.Pool)
	if len(seen) == 0 {
		owed, err := revocations.Owing(ctx, email)
		if err != nil {
			fmt.Fprintf(stderr, "clear-broker-revocation: %v\n", err)
			return 1
		}
		if len(owed) == 0 {
			fmt.Fprintf(stderr, "clear-broker-revocation: no broker revocation of %s is owed\n", email)
			return 1
		}
		for _, revocation := range owed {
			fmt.Fprintf(stdout, "clear-broker-revocation: %s owes %s\n", email, revocation.Listed())
		}
		fmt.Fprintf(stdout, "clear-broker-revocation: nothing cleared; settle the ones the broker never confirms with `envoy-dispatch clear-broker-revocation %s <subject>:<generation>...`\n", email)
		return 0
	}
	cleared, err := revocations.Clear(ctx, email, seen)
	if err != nil {
		fmt.Fprintf(stderr, "clear-broker-revocation: %v\n", err)
		return 1
	}
	code := 0
	for _, named := range seen {
		settled := slices.ContainsFunc(cleared, func(revocation store.OwedBrokerRevocation) bool {
			return revocation.Subject == named.Subject && revocation.Generation == named.Generation
		})
		if settled {
			fmt.Fprintf(stdout, "clear-broker-revocation: %s %s:%d cleared\n", email, named.Subject, named.Generation)
			continue
		}
		fmt.Fprintf(stderr, "clear-broker-revocation: %s %s:%d is not owed: settled already, owed again under a later generation, or never recorded; list it again\n",
			email, named.Subject, named.Generation)
		code = 1
	}
	return code
}
