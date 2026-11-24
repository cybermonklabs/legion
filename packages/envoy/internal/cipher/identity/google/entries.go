package google

import "github.com/sjawhar/envoy/internal/cipher/exchange"

// GoogleEntry lists one Google service account and what a machine holding its identity token may
// write. Account and Service are required; the rest is what the entry chooses to pin and to
// grant.
//
// The two pins go together: an entry that pins both holds the machine to one Compute Engine
// project and to the instance names its pattern matches, and refuses a token carrying no Compute
// Engine claims at all. An entry that pins neither takes such a token, which is what a token
// minted through a workload identity pool and a token a pod's metadata server answers both carry.
//
// Project and Person go together too: an entry that names them mints a token that acts for that
// person and names that one project, exactly as the kubernetes provider's AccountEntry does for a
// pod, and an entry that names neither mints a token that names its service and no person at all.
// An entry that names a job list names neither of them: its token names the account's address as
// its service and carries the list.
type GoogleEntry struct {
	// Account is the service account's email address, the `email` claim of its identity tokens,
	// named exactly: there is no wildcard.
	Account string
	// Service is the service a machine of this account is, which the minted token names when the
	// entry names neither a person nor a job list, and which the launcher credential of every
	// machine of the account records (exchange.Claims.Fleet).
	Service string
	// CloudProject, when set, is the Google Cloud project the machine must run in, compared
	// exactly with the token's google.compute_engine.project_id.
	CloudProject string
	// Instance, when set, is the pattern the machine's instance name must match, in path.Match's
	// syntax, such as example-vm-*.
	Instance string
	// Project, when set, is the one Dispatch project key the minted token names. It is set
	// with Person and with nothing else.
	Project string
	// Person, when set, is who the machine's token acts for. An entry that names none mints a
	// token that names Service and no person, or the account when it names a job list.
	Person string
	// Job, when set, is the job list of an entry that names no person and no project: its
	// machines' tokens name Account as their service and carry the list, and the launcher
	// credential each exchange mints still records Service (BROKER_VM_EXCHANGE_JOB_ACCOUNTS).
	Job exchange.JobProjects
	// Session, when set, is the session name every token minted for this account carries, as
	// the kubernetes provider's AccountEntry.Session is for an account.
	Session string
}
