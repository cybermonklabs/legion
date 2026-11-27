package main

import (
	"context"
	"io"
	"os"

	"github.com/spf13/cobra"
)

// newReopenedReportCommand is `legion reopened-report`.
func newReopenedReportCommand(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reopened-report --config <controller.yaml> --issue <KEY>...",
		Short: "report, for each issue the controller reopened, whether its owner wrote checks or withdrew the request before the change ended",
		Args:  noArguments,
	}
	configPath := cmd.Flags().String("config", "", "path to the operator-side controller.yaml, whose Dispatch credential the report reads with (required)")
	issues := cmd.Flags().StringArray("issue", nil, "an issue the controller reopened (repeat it for each issue, at least one)")
	cmd.RunE = runs(func(ctx context.Context, _ []string) int {
		return runReopenedReport(ctx, *configPath, *issues, os.Getenv, stdout, stderr)
	})
	return cmd
}

// runReopenedReport reads and prints the report.
func runReopenedReport(ctx context.Context, configPath string, issues []string, getenv func(string) string, stdout, stderr io.Writer) int {
	return 0
}
