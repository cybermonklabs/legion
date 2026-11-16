// packages/envoy/cmd/agent-secrets/grant.go
//
// "agent-secrets grant list|revoke": the live grants of the operator's sessions and those the
// operator approved, listed and ended under this machine's login (the broker's operator routes,
// signed by the helper's sign-launcher op), as Dispatch's Live grants page lists and ends them.
// groupForms (main.go) dispatches them.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/sjawhar/envoy/internal/broker/helper"
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

// inSession reports whether this process is inside a registered machine session: its key dir
// holds a launcher's key.pem, or the helper's identity probe answers that it is (OK, or
// NOT_ENROLLED while it is still enrolling). It makes no broker call. A process in no session —
// the operator's own shell — is false, and takes the operator route below.
func inSession(dir string) bool {
	if exists(filepath.Join(dir, keyFile)) {
		return true
	}
	answer := probeHelper()
	if !answer.answered() {
		return false
	}
	return answer.resp.OK || answer.resp.Code == helper.CodeNotEnrolled
}

// cmdGrantRevoke implements "grant revoke GRANT_ID". Inside a session it ends a grant that session
// was given, signing with the identity its own use command takes (R4), so the grant its use
// command was given is the one it can end: the broker's own session revoke (POST
// /v1/grants/{id}/revoke), which refuses a grant of another session (403 NOT_YOURS). Outside a
// session it is the operator's revoke, as Dispatch's Live grants page does it: the operator's
// revoke of a grant a session got automatically also withholds those secrets from that session,
// so it asks before it gets them again. One run never tries both routes.
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
	grantID := positional[0]
	if inSession(keyDir()) {
		return grantRevokeInSession(form, grantID, stdout, stderr)
	}
	base, signer, err := machineContext()
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets %s: %v\n", form, err)
		return exitUsageError
	}
	ctx := context.Background()
	c := newClient(base)
	if err := c.RevokeOperatorGrant(ctx, signer, grantID); err != nil {
		return operatorFail(stderr, form, c.source(ctx), err)
	}
	fmt.Fprintf(stdout, "revoked %s at %s\n", grantID, brokerSource(base))
	return 0
}

// grantRevokeInSession ends the grant through the session's own broker route, signing with the
// identity R4's chain gives this process. A grant of another session is refused (NOT_YOURS) with
// no operator route tried, so a session can never end another's grant by falling through to the
// operator's revoke.
func grantRevokeInSession(form, grantID string, stdout, stderr io.Writer) int {
	base := brokerBaseURL()
	if base == "" {
		fmt.Fprintf(stderr, "agent-secrets %s: %s is required: it names the secrets broker the grant is at\n", form, brokerURLVar)
		return exitUsageError
	}
	_, signer, err := selectSigner(keyDir())
	if err != nil {
		reportError(stderr, "agent-secrets "+form, err)
		return 1
	}
	ctx := context.Background()
	c := newClient(base)
	err = c.RevokeGrant(ctx, signer, grantID)
	var broker *apiError
	switch {
	case err == nil:
		fmt.Fprintf(stdout, "revoked %s at %s\n", grantID, brokerSource(base))
		return 0
	case errors.As(err, &broker) && broker.Code == "NOT_YOURS":
		fmt.Fprintf(stderr, "agent-secrets %s: grant %s is not this session's; its holder ends it, or its operator from their own shell (agent-secrets grant revoke %s), or its approver on Dispatch's Live grants page\n", form, grantID, grantID)
		return 1
	default:
		refuse(stderr, "agent-secrets "+form, c.source(ctx), err)
		return 1
	}
}
