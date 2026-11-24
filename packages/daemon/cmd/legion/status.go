package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/spf13/cobra"

	"github.com/sjawhar/legion/daemon/internal/api"
)

// newIssueStatusCommand is `legion issue status <issue> <status>`, the operator's: Dispatch moves
// the issue to one of the three statuses a controller sets, through the daemon's issue status
// route, which takes a controller grant. --operator-token-file, required, presents the operator's
// bearer to the grants route's operator form for that grant; the bearer is read and sent as every
// operator command reads and sends it (operatorCall). The daemon is found as `legion state` finds
// it.
func newIssueStatusCommand(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status <issue> todo|backlog|icebox --operator-token-file <file>",
		Short: "set an issue's Dispatch status to todo, backlog or icebox, as a controller does",
		Args:  exactWords(2, "the issue and its status, todo, backlog or icebox"),
	}
	c := newOperatorCall(cmd, "file holding the operator bearer, which mints the controller grant (required)", stdout, stderr)
	cmd.RunE = runs(func(ctx context.Context, args []string) int { return runIssueStatus(ctx, c, args[0], args[1]) })
	return cmd
}

func runIssueStatus(ctx context.Context, c *operatorCall, issue, status string) int {
	if code, ok := c.check("operator-token-file"); !ok {
		return code
	}
	if status != "todo" && status != "backlog" && status != "icebox" {
		return refuse(c.cmd, "the status is todo, backlog or icebox, not %q", status)
	}
	op, ok := c.connect()
	if !ok {
		return 1
	}
	var grant string
	if code := c.send(ctx, op, http.MethodPost, "/legion/v1/grants", struct{}{}, func(_ io.Writer, answer []byte) error {
		// The answer is named by its member, never printed: it is the daemon's word, and the
		// grant it should hold is a credential.
		var minted api.GrantResponse
		if err := json.Unmarshal(answer, &minted); err != nil || minted.GrantID == "" {
			return errors.New("it names no grant (grantId)")
		}
		grant = minted.GrantID
		return nil
	}); code != 0 {
		return code
	}
	// The grant authenticates the status request; the bearer has done its one job.
	return c.send(ctx, operator{base: op.base}, http.MethodPost, "/legion/v1/issues/status",
		api.IssueStatusRequest{GrantID: grant, Issue: issue, Status: status}, writeLine)
}

// writeLine writes a 2xx answer as the daemon served it, ending in a newline.
func writeLine(w io.Writer, answer []byte) error {
	if len(answer) == 0 {
		return nil
	}
	if _, err := w.Write(answer); err != nil {
		return err
	}
	if answer[len(answer)-1] != '\n' {
		_, err := fmt.Fprintln(w)
		return err
	}
	return nil
}
