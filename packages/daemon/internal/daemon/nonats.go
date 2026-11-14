package daemon

import (
	"context"
	"errors"

	"github.com/sjawhar/envoy/legiontasks"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// NoNATSLine is the operator's sentence for a daemon whose configuration names no NATS
// (`nats_urls`), and "" for one that names it. A worker is handed its task through the task bucket
// on Envoy's NATS (legiontasks.Bucket, dispatch://INFRA-2811): with no NATS the daemon has no bucket
// to write a claim's value in, so a worker it launched would never learn of a task. Such a daemon
// boots as it always did and serves its API, and launches no worker: the operator's spawn, a
// retry, a resume and every relaunch are refused with this sentence (noLaunch).
//
// One sentence, from one place, for its two readers, as IntakeHandlerLimitLine is: the boot logs it
// at Warn (run), and `legion start --check-config` prints it after its OK line, so an operator who
// reads one has read the other word for word.
func NoNATSLine(cfg config.Config) string {
	if len(cfg.NatsURLs) > 0 {
		return ""
	}
	return "nats: nats_urls names no NATS, so this daemon has no task bucket (" + legiontasks.Bucket +
		") to hand a worker its task through: it launches no worker, and refuses every spawn, retry and resume of one until nats_urls names Envoy's NATS"
}

// noLaunch is the refusal of every launch of a daemon that launches no worker (NoNATSLine), as the
// launch path's lifecycle recheck answers it (supervise.Store.CheckLaunch, gateLaunch): a spawn, a
// retry or a resume is refused with it and charged nothing, and a relaunch the machine raises
// itself waits at the probe interval, as a refused recheck always does (supervise's
// relaunchRefused). nil, a daemon that launches, refuses nothing.
func noLaunch(reason error) func(context.Context, supervise.Claim) error {
	if reason == nil {
		return nil
	}
	return func(_ context.Context, c supervise.Claim) error {
		return &supervise.RefusedError{State: c.State, Request: "launch", Err: reason}
	}
}

// errNoLaunch is reason as the refusal of a claim the daemon would create to launch
// (supervisor.Create): the claim is never stored, so a refused spawn leaves nothing queued behind.
func errNoLaunch(reason error) error {
	if reason == nil {
		return nil
	}
	return &supervise.RefusedError{State: supervise.StateQueued, Request: "spawn", Err: reason}
}

// launchRefusal is the reason a daemon of cfg with the task bucket values launches no worker: its
// configuration names no NATS, and nothing stands in for the bucket it would open on it (a test's,
// overrides.taskValues). nil for a daemon that launches.
func launchRefusal(cfg config.Config, values supervise.TaskValues) error {
	line := NoNATSLine(cfg)
	if line == "" || values != nil {
		return nil
	}
	return errors.New(line)
}
