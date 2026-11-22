package daemon

import (
	"log/slog"

	"github.com/sjawhar/legion/daemon/internal/leakguard"
)

// leakCheckNotConfigured is the boot's line for a deployment that names no shared public leak
// check (config's leak_check): what is left out, the two settings that would name one, and what
// running without one means. It is a plain Info line and not a warning: a deployment with no
// check of its own, a turnkey install among them, is one the daemon runs as designed.
const leakCheckNotConfigured = "daemon: the deployment leak check is not configured (" +
	leakguard.RepositoryVar + " and " + leakguard.ManifestVar + ", or leak_check.repository and " +
	"leak_check.manifest_sha256 in legion.yaml): role pods start without it, and no write is refused for its absence"

// leakCheckConfigured is the boot's line for a deployment that names its shared public leak
// check, which carries the repository the daemon reads it from and the manifest digest every role
// is held to, so an operator reads in the daemon's own log which check its roles run.
const leakCheckConfigured = "daemon: the deployment leak check is configured: role pods install it from the repository at the worker image's pin, and every role is held to its manifest digest"

// logLeakCheck is the boot's one line on the deployment's shared public leak check:
// leakCheckConfigured with the setting's repository and manifest digest under a setting that
// names one, and leakCheckNotConfigured under one that names none.
func logLeakCheck(log *slog.Logger, setting leakguard.Setting) {
	if setting.Configured() {
		log.Info(leakCheckConfigured, "repository", setting.Repository.String(), "manifest_sha256", setting.ManifestSHA256)
		return
	}
	log.Info(leakCheckNotConfigured)
}
