package leakguard

import "github.com/sjawhar/legion/daemon/internal/ghrepo"

// Setting is the deployment's shared public leak check as its operator names it: the daemon's
// `leak_check` block in legion.yaml, or RepositoryVar and SHA256Var in the daemon's environment
// (internal/config). Legion holds no name of it: the deployment supplies both values, and a
// turnkey install runs with neither.
//
// Repository is the repository the check is published from by tag, whose `leak-check` directory
// the daemon reads at a pod's pin and pushes to the pod's launcher (Source). CheckerSHA256 is the
// sha256, in lowercase hex, of that check's public_leak_check.py, which the runtime tells every
// role it starts under CheckerVar, and which a role's shim (Installed) and guard
// (Guard.sharedCheck) hold the check they find to.
//
// Both are set, or neither, which the loader holds the setting to. The zero Setting is a
// deployment that runs no shared leak check of its own: the daemon reads and pushes none, a role
// pod starts without it, and the guard refuses no write for its absence (Guard.sharedCheck).
type Setting struct {
	Repository    ghrepo.Repository
	CheckerSHA256 string
}

// Configured is whether the deployment names a shared leak check at all.
func (s Setting) Configured() bool { return !s.Repository.IsZero() }

// RepositoryVar is the daemon's environment fallback for Setting.Repository, which the file's
// `leak_check.repository` overrides.
const RepositoryVar = "LEGION_LEAK_CHECK_REPOSITORY"

// SHA256Var is the daemon's environment fallback for Setting.CheckerSHA256, which the file's
// `leak_check.checker_sha256` overrides. It is the daemon's own and told to no role: a role is told
// the checker under CheckerVar, a name the daemon's configuration never reads, so a configuration
// loaded in a role — `legion config check` in a pod reads the process environment — never
// meets one half of the daemon's setting without the other.
const SHA256Var = "LEGION_LEAK_CHECK_SHA256"

// CheckerVar is the name the runtime tells every role it starts the deployment's checker
// (Setting.CheckerSHA256) under (runtime/sandbox's mainEnvironment, runtime/tmux's panePairs). In a
// role's environment it names the one check the role may write under: a role's shim reads the
// pod's install only under it and starts no agent unless the install's program hashes to it
// (Installed), and the guard refuses a write for want of a check, or under a check whose program
// hashes to anything else, only under it (Guard.sharedCheck). It does not decide `legion gh`'s
// checked-only reads, which only the resolved check's program does (GhRequest.ProvenCheck). It is
// exported because each of those reads it, and a second copy of the string in any of them is one
// an edit here would silently part from.
const CheckerVar = "LEGION_LEAK_CHECKER_SHA256"

// configured is whether env names the deployment's checker (CheckerVar): the role runs under a
// deployment that runs a shared leak check.
func configured(env []string) bool { return value(env, CheckerVar) != "" }
