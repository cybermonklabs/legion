// packages/envoy/cmd/agent-secrets/grant.go
//
// "agent-secrets grant list|revoke": the live grants of the operator's sessions and those the
// operator approved, listed and ended under this machine's login (the broker's operator routes,
// signed by the helper's sign-launcher op), as Dispatch's Live grants page lists and ends them.
// Inside a session, grant revoke is the session's own revoke instead (cmdGrantRevoke). groupForms
// (main.go) dispatches them.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/sjawhar/envoy/internal/broker/helper"
	"github.com/sjawhar/envoy/internal/redact"
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
		return operatorFail(stderr, form, c.source(ctx), signer.failure, err)
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
// such value. Each cell is the broker's, so it prints on one line (redact.OneLine): as it came, or
// Go-quoted where a control character in it, a tab among them, would start a line of its own or
// split the cell.
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
	cells := []string{
		g.GrantID, strings.Join(g.Names, ","), g.Granted, orDashPtr(g.Approver), orDashPtr(g.DecidedBy),
		session, orDash(g.Enrollment.Operator), grantExpiry(g.ExpiresAt),
	}
	for i, cell := range cells {
		cells[i] = redact.OneLine(cell)
	}
	return cells
}

// orDashPtr is what s points at, or "-" where it points at nothing: a cell the broker's answer
// leaves null.
func orDashPtr(s *string) string {
	if s == nil {
		return "-"
	}
	return orDash(*s)
}

// inSession reports whether this process is inside a session: its key dir is a launcher's
// (launcherKeyDir, the rule identity reads it by), or the helper's identity probe answers that it
// is (OK, or NOT_ENROLLED while it is still enrolling). It asks the broker nothing. A process in
// no session — the operator's own shell — is false, and takes the operator route below.
func inSession(dir string) bool {
	if launcherKeyDir(dir) {
		return true
	}
	answer := probeHelper()
	if !answer.answered() {
		return false
	}
	return answer.resp.OK || answer.resp.Code == helper.CodeNotEnrolled
}

// cmdGrantRevoke implements "grant revoke GRANT_ID". Inside a session it is the session's own
// revoke, which `agent-secrets revoke` makes too (revokeAsSession): the broker ends only a grant
// of the enrollment that signs and refuses any other NOT_YOURS. So a session ends the grant its
// use command was given, and so does every session that signs as the same broker identity, such
// as each session of a person who ran `agent-secrets login`. Outside a session it is the
// operator's revoke, as Dispatch's Live grants page does it: the operator's revoke of a grant a
// session got automatically also withholds those secrets from that session, so it asks before it
// gets them again. One run never tries both routes.
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
		return operatorFail(stderr, form, c.source(ctx), signer.failure, err)
	}
	fmt.Fprintf(stdout, "revoked %s at %s\n", grantID, brokerSource(base))
	return 0
}

// grantRevokeInSession is grant revoke inside a session: the session's own revoke, with
// NOT_YOURS named for a reader and no operator route tried after it, so a session can never end
// another broker identity's grant by falling through to the operator's revoke.
func grantRevokeInSession(form, grantID string, stdout, stderr io.Writer) int {
	prefix := "agent-secrets " + form
	c, failed, err := revokeAsSession(grantID)
	var broker *apiError
	switch {
	case failed != nil:
		return sessionFailed(stderr, prefix, failed)
	case err == nil:
		fmt.Fprintf(stdout, "revoked %s at %s\n", grantID, brokerSource(c.baseURL))
		return 0
	case errors.As(err, &broker) && broker.Code == "NOT_YOURS":
		fmt.Fprintf(stderr, "%s: grant %s was given to another broker identity (enrollment) than the one this process signs as; its holder ends it, or its operator from their own shell (agent-secrets grant revoke %s), or its approver on Dispatch's Live grants page\n", prefix, grantID, grantID)
		return 1
	default:
		refuse(stderr, prefix, c.source(context.Background()), err)
		return 1
	}
}
