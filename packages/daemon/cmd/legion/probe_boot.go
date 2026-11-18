package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/daemon"
)

// newProbeBootCommand is `legion probe-boot --config <legion.yaml>`: the part of a Kubernetes
// daemon's boot its worker pod can refuse, run from the daemon's own configuration with nothing
// the workflow needs (daemon.ProbeBoot). The configuration is read through the loader that runs
// neither App's private_key_command; then come boot's reads of the files it names, the cluster's
// refusals, and the daemon's own image probe Sandbox, under the credentials runtime.kubernetes
// names (in a pod, the pod's ServiceAccount). It prints one OK line on a pass and exits 0, or the
// refusal and exits 1. A configuration change to the worker pod is proven with it before the
// daemon restarts on it.
func newProbeBootCommand(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "probe-boot",
		Short: "run a Kubernetes daemon's cluster check and worker image probe from legion.yaml, with no workflow, and exit",
		Args:  configOnly,
	}
	configPath := cmd.Flags().String("config", defaultConfigPath, "path to legion.yaml")
	cmd.RunE = runs(func(ctx context.Context, _ []string) int { return runProbeBoot(ctx, *configPath, stdout, stderr) })
	return cmd
}

func runProbeBoot(ctx context.Context, configPath string, stdout, stderr io.Writer) int {
	cfg, err := config.LoadForValidation(configPath, nil)
	if err != nil {
		fmt.Fprintf(stderr, "legion probe-boot: %v\n", err)
		return 1
	}
	log := slog.New(slog.NewJSONHandler(stderr, nil))
	report, err := daemon.ProbeBoot(ctx, cfg, os.LookupEnv, log)
	if err != nil {
		fmt.Fprintf(stderr, "legion probe-boot: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "probe-boot: OK project=%s image=%s model-fallback=%s capacity-waits=%d\n",
		cfg.Project, cfg.Runtime.Kubernetes.Image, report.ModelFallback, report.CapacityWaits)
	return 0
}
