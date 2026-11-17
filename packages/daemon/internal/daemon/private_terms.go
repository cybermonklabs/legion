package daemon

import (
	"log/slog"

	"github.com/sjawhar/legion/daemon/internal/leakguard"
)

// privateNameTermsNotConfigured is the boot's line for a deployment that hands the daemon no term
// list (leakguard.TermsVar): what is left out, the one variable that would hand it, and what
// running without it means. A plain Info line, as the leak check's is: a deployment with no
// private names of its own, a turnkey install among them, is one the daemon runs as designed.
const privateNameTermsNotConfigured = "daemon: the deployment's private-name term list is not configured (" +
	leakguard.TermsVar + " in the daemon's environment): every role's leak guard runs this repository's own rules alone, and refuses no term of the deployment's own"

// deploymentTerms is the deployment's term list as the daemon's environment (lookup) holds it,
// whole, or "" where it holds no term (leakguard.Terms: blank lines and `key:` lines are none).
// The daemon hands it to every launch as a launch secret (launchSecrets), and its own guard, in
// this process, reads it from the same environment.
func deploymentTerms(lookup func(string) (string, bool)) string {
	value, _ := lookup(leakguard.TermsVar)
	if terms, _ := leakguard.Terms([]string{leakguard.TermsVar + "=" + value}); len(terms) == 0 {
		return ""
	}
	return value
}

// logPrivateNameTerms is the boot's one line on the deployment's term list: under an environment
// that holds none, privateNameTermsNotConfigured, once; under one that holds a list, nothing, since
// every role's guard then reads it.
func logPrivateNameTerms(log *slog.Logger, lookup func(string) (string, bool)) {
	if deploymentTerms(lookup) != "" {
		return
	}
	log.Info(privateNameTermsNotConfigured)
}
