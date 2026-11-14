package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"k8s.io/client-go/rest"

	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
)

// imageProber is the image probe ProbeBoot runs: sandbox.Prober's.
type imageProber interface {
	ProbeImage(ctx context.Context, p sandbox.ImageProbe) (bootprobe.ImageReport, error)
}

// ProbeBoot is `legion probe-boot`: the part of a Kubernetes daemon's boot its worker pod and its
// NATS grant can refuse, run from the daemon's own configuration with nothing the workflow needs. It
// makes boot's reads of the configuration, the caller's environment and the files they name
// (readBoot), then runs the cluster check a booting daemon runs, then the open of the task bucket a
// booting daemon makes before it supervises anything (probeTaskBucket), and then its image probe
// (sandboxBootOf: one value for the check and the probe), the probe on a runtime built from the
// Options boot reads (sandbox.NewProber), under the credentials runtime.kubernetes names. The task
// bucket is opened on Envoy's NATS as the daemon's own user (natsConnection), and created where it
// does not exist yet, as a booting daemon creates it; a server that refuses that user a subject of
// the bucket refuses the probe with the boot's own refusal, naming the bucket and the subject, and
// the pane seed's user when that is who the daemon connects as. The probe Sandbox it creates is the
// one a booting daemon creates, under a name of its own run: built from the operator pod's
// ServiceAccount, volumes, mounts and environment, so a pod the cluster would not admit, mount or
// start fails here. It skips every boot stage that reads nothing of the worker pod or the grant —
// the store and its claims fence and migration and the GitHub App mint, which a booting daemon runs
// before the probe, the workflow's durable consumers it opens beside the bucket, and the Dispatch
// listing it runs after — and writes nothing but the task bucket, where it creates it, and its
// probe Sandbox, which it deletes.
func ProbeBoot(ctx context.Context, cfg config.Config, lookup func(string) (string, bool), log *slog.Logger) (bootprobe.ImageReport, error) {
	return probeBoot(ctx, cfg, lookup, log, func(ctx context.Context, rc *rest.Config, opts sandbox.Options) (imageProber, error) {
		return sandbox.NewProber(ctx, rc, opts)
	})
}

// probeBoot is ProbeBoot over newProber, which builds the probe's runtime.
func probeBoot(ctx context.Context, cfg config.Config, lookup func(string) (string, bool), log *slog.Logger,
	newProber func(context.Context, *rest.Config, sandbox.Options) (imageProber, error),
) (bootprobe.ImageReport, error) {
	if cfg.Runtime.Name != "kubernetes" {
		return bootprobe.ImageReport{}, fmt.Errorf("probe-boot runs the Kubernetes runtime's boot probe, and runtime %q has none", cfg.Runtime.Name)
	}
	reads, err := readBoot(cfg, lookup, os.Getenv, true, log)
	if err != nil {
		return bootprobe.ImageReport{}, err
	}
	boot, err := sandboxBootOf(cfg, reads.sandbox)
	if err != nil {
		return bootprobe.ImageReport{}, err
	}
	checking, cancel := context.WithTimeout(ctx, bootTimeout)
	defer cancel()
	if err := boot.clusterCheck(checking); err != nil {
		return bootprobe.ImageReport{}, err
	}
	if err := probeTaskBucket(checking, cfg, reads, log); err != nil {
		return bootprobe.ImageReport{}, err
	}
	// The prober's informers run until this context ends, so they stop when the probe returns.
	probing, stop := context.WithCancel(ctx)
	defer stop()
	prober, err := newProber(probing, reads.sandbox.client, reads.sandbox.opts)
	if err != nil {
		return bootprobe.ImageReport{}, err
	}
	return prober.ProbeImage(probing, boot.probe)
}

// probeTaskBucket opens the task bucket as a booting daemon does (openTaskBucket), creating it where
// it does not exist, on a connection of its own as the daemon's own user, which reads once: one
// dial and no reconnect, since the probe reports and exits rather than waiting a NATS out.
func probeTaskBucket(ctx context.Context, cfg config.Config, reads bootReads, log *slog.Logger) error {
	conn, err := dialNATS(cfg, reads.nats, log)
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = openTaskBucket(ctx, conn, reads.project, reads.nats, log)
	return err
}
