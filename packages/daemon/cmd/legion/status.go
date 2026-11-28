package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/spf13/cobra"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/intake"
)

// newIssueStatusCommand is `legion issue status <issue> <status>`, the operator's: Dispatch moves
// the issue to one of the three statuses a controller sets, through the daemon's issue status
// route, which takes a controller grant. --children and --parent are the two arguments of
// Dispatch's parent rule the route takes, as the controller's set_status sends them (R24 of
// dispatch://INFRA-3320): --children include or unlink with a backlog or icebox that would leave
// a child in work under the issue, and --parent include with a todo that would put the issue in
// work under a parent that is not. --operator-token-file, required, presents the operator's
// bearer to the grants route's operator form for that grant; the bearer is read and sent as every
// operator command reads and sends it (operatorCall). The daemon is found as `legion state` finds
// it, and its answer, each status write the call queued, is printed as it came.
func newIssueStatusCommand(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status <issue> todo|backlog|icebox [--children include|unlink] [--parent include] --operator-token-file <file>",
		Short: "set an issue's Dispatch status to todo, backlog or icebox, as a controller does",
		Args:  exactWords(2, "the issue and its status, todo, backlog or icebox"),
	}
	c := newOperatorCall(cmd, "file holding the operator bearer, which mints the controller grant (required)", stdout, stderr)
	var children, parent string
	cmd.Flags().StringVar(&children, "children", "", "with backlog or icebox: include moves each open issue under the issue with it, unlink takes each open child out from under it")
	cmd.Flags().StringVar(&parent, "parent", "", "with todo: include moves each issue above the issue that is not in work to in_progress with it")
	cmd.RunE = runs(func(ctx context.Context, args []string) int {
		return runIssueStatus(ctx, c, api.IssueStatusRequest{Issue: args[0], Status: args[1], Children: children, Parent: parent})
	})
	return cmd
}

// runIssueStatus sends req, its grant minted here, once its words are ones the route takes: the
// route's own refusals of a status, a children or a parent argument (INVALID_STATUS,
// INVALID_CHILDREN, INVALID_PARENT) are usage errors here, before the daemon is reached.
func runIssueStatus(ctx context.Context, c *operatorCall, req api.IssueStatusRequest) int {
	if code, ok := c.check("operator-token-file"); !ok {
		return code
	}
	switch {
	case req.Status != "todo" && req.Status != "backlog" && req.Status != "icebox":
		return refuse(c.cmd, "the status is todo, backlog or icebox, not %q", req.Status)
	case req.Children != "" && req.Children != intake.IncludeArgument && req.Children != intake.UnlinkArgument:
		return refuse(c.cmd, "--children is %s or %s, not %q", intake.IncludeArgument, intake.UnlinkArgument, req.Children)
	case req.Children != "" && req.Status == "todo":
		return refuse(c.cmd, "--children goes with backlog or icebox, which move an issue out of work")
	case req.Parent != "" && req.Parent != intake.IncludeArgument:
		return refuse(c.cmd, "--parent is %s, not %q", intake.IncludeArgument, req.Parent)
	case req.Parent != "" && req.Status != "todo":
		return refuse(c.cmd, "--parent goes with todo, which moves an issue into work")
	}
	op, ok := c.connect()
	if !ok {
		return 1
	}
	if code := c.send(ctx, op, http.MethodPost, "/legion/v1/grants", struct{}{}, func(_ io.Writer, answer []byte) error {
		var minted api.GrantResponse
		if err := json.Unmarshal(answer, &minted); err != nil || minted.GrantID == "" {
			return fmt.Errorf("no grant in it: %s", answer)
		}
		req.GrantID = minted.GrantID
		return nil
	}); code != 0 {
		return code
	}
	// The grant authenticates the status request; the bearer has done its one job.
	return c.send(ctx, operator{base: op.base}, http.MethodPost, "/legion/v1/issues/status", req, writeLine)
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
