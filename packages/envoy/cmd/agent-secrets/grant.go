// packages/envoy/cmd/agent-secrets/grant.go
//
// "agent-secrets grant list|revoke": the live grants of the operator's sessions and those the
// operator approved, listed and ended under this machine's login (the broker's operator routes,
// signed by the helper's sign-launcher op), as Dispatch's Live grants page lists and ends them.
// groupForms (main.go) dispatches them.
package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
)

// cmdGrantList implements "grant list": every live grant of a session the operator runs, given
// automatically or on anyone's approval, and every grant the operator approved on anyone's
// session, newest first. --json prints the broker's answer verbatim, the body Dispatch's Live
// grants page reads.
func cmdGrantList(args []string, stdout, stderr io.Writer) int {
	const form = "grant list"
	flagArgs, positional := splitArgs(args, nil)
	flags := newFlagSet(form, stderr)
	asJSON := flags.Bool("json", false, "print the broker's response body verbatim")
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	if len(positional) > 0 {
		fmt.Fprintf(stderr, "agent-secrets %s: unexpected argument %q\n", form, positional[0])
		return exitUsageError
	}
	base, signer, err := machineContext()
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets %s: %v\n", form, err)
		return exitUsageError
	}
	ctx := context.Background()
	c := newClient(base)
	grants, raw, err := c.OperatorGrants(ctx, signer)
	if err != nil {
		return operatorFail(stderr, form, c.source(ctx), err)
	}
	if *asJSON {
		writeVerbatim(stdout, raw)
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(grantColumns, "\t"))
	for _, g := range grants.Grants {
		fmt.Fprintln(tw, strings.Join(grantRow(g), "\t"))
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(stderr, "agent-secrets %s: %v\n", form, err)
		return 1
	}
	// The source line goes to stderr, so the table on stdout stays one row per grant for a
	// script. Every SECRETS cell names secrets of this one store, never another CLI's of the
	// same name.
	fmt.Fprintf(stderr, "Every row is a grant from %s.\n", c.source(ctx))
	return 0
}

// grantExpiry is a grant's EXPIRES cell: its expiry in RFC 3339, or session-end for a grant that
// lasts until its session ends. One word, so the table splits on white space.
func grantExpiry(expires *time.Time) string {
	if expires == nil {
		return "session-end"
	}
	return expires.UTC().Format(time.RFC3339)
}

// grantColumns is the table's header: one name for each cell grantRow builds, in that order.
var grantColumns = []string{"GRANT_ID", "SECRETS", "GRANTED", "APPROVER", "DECIDED_BY", "SESSION", "OPERATOR", "EXPIRES"}

// grantRow is one grant's cells, in grantColumns' order, each "-" where the grant carries no
// such value.
//
// APPROVER and DECIDED_BY sit side by side because they differ for one kind of grant: one the
// model judge approved, which records the record's own approver — the secret's owner, or anyone
// for a shared secret — and the judge as its decider, "judge:<model id>". APPROVER alone read as
// the owner's own approval of a request that owner never saw. An automatic grant nobody decided
// prints "-" under both.
func grantRow(g OperatorGrant) []string {
	session := g.Enrollment.Kind + "/" + g.Enrollment.RuntimeID
	if g.Enrollment.Slot != nil {
		session += "/" + *g.Enrollment.Slot
	}
	return []string{
		g.GrantID, strings.Join(g.Names, ","), g.Granted, orDashPtr(g.Approver), orDashPtr(g.DecidedBy),
		session, orDash(g.Enrollment.Operator), grantExpiry(g.ExpiresAt),
	}
}

// orDashPtr is what s points at, or "-" where it points at nothing: a cell the broker's answer
// leaves null.
func orDashPtr(s *string) string {
	if s == nil {
		return "-"
	}
	return orDash(*s)
}

// cmdGrantRevoke implements "grant revoke GRANT_ID": it ends a grant as the operator, as Dispatch's
// Live grants page does. The operator's revoke of a grant a session got automatically also
// withholds those secrets from that session: it asks before it gets them again.
func cmdGrantRevoke(args []string, stdout, stderr io.Writer) int {
	const form = "grant revoke"
	flagArgs, positional := splitArgs(args, nil)
	if err := newFlagSet(form, stderr).Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	if len(positional) != 1 {
		fmt.Fprintf(stderr, "agent-secrets %s: exactly one GRANT_ID is required\n", form)
		return exitUsageError
	}
	base, signer, err := machineContext()
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets %s: %v\n", form, err)
		return exitUsageError
	}
	ctx := context.Background()
	c := newClient(base)
	if err := c.RevokeOperatorGrant(ctx, signer, positional[0]); err != nil {
		return operatorFail(stderr, form, c.source(ctx), err)
	}
	fmt.Fprintf(stdout, "revoked %s at %s\n", positional[0], brokerSource(base))
	return 0
}
