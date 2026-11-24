package kubernetes

import "github.com/sjawhar/envoy/internal/cipher/exchange"

// AccountEntry lists one Kubernetes service account and what a pod running as it may write.
// Account is required. Person is required of an entry that names a project, and Job and Person
// are never both set: an entry names the person its pods act for, with a project or none, or a
// job list, or neither, for an entry whose pods act as the account itself. Session is not
// required.
type AccountEntry struct {
	// Account is the subject a pod's projected token carries,
	// system:serviceaccount:<namespace>:<name>, named exactly: there is no wildcard.
	Account string
	// Project is the one project key the minted token names, or empty for an entry that names
	// none: the shared public-session account, whose named tokens act for Person and name no
	// project, or an entry that names no person either. Only an account entry may leave it empty.
	Project string
	// Person is who the account's pods act for, or empty for an entry that names no project
	// either: such an entry's tokens act as the account itself, which they name as their service
	// (exchange.Claims.Service) and Dispatch records their writes under. An entry that names a
	// project names its person, so no account entry's token that names a project is personless.
	Person string
	// Job, when set, is the job list of an entry that names no person and no project: its pods'
	// tokens name Account as their service and carry the list (BROKER_EXCHANGE_JOB_ACCOUNTS).
	Job exchange.JobProjects
	// Session, when set, is the session name every token minted for this account carries, and a
	// request naming any other is refused. It is how a long-running service keeps writing under
	// one name it does not get to choose per request. The name is one of the exchange.Set's
	// Reservations, held for the account however many of its pods exchange and refused to every
	// other caller. oauthwire.LegionDaemonSessionPrefix + Project makes the entry a Legion
	// daemon's own (exchange.Claims.LegionStatus).
	Session string
}

// ReadEntry lists one Kubernetes service account whose pods may read and may write nothing: the
// token they are minted holds the OAuth scope oauthwire.ScopeRead and the project scope
// exchange.ProjectScopeNone, names Service and Session, and names no person and no project. Every
// field is required. A resource server accepts such a token only where it asks to, and Dispatch
// serves it on every GET route it classes as a read and nowhere else.
//
// It is a list of its own, BROKER_EXCHANGE_READ_ACCOUNTS, rather than a kind of AccountEntry, so
// that a broker of a build that predates it reads none of its entries and mints such an account
// nothing, where the same entry in BROKER_EXCHANGE_ACCOUNTS would read as a project and a person.
type ReadEntry struct {
	// Account is the subject a pod's projected token carries,
	// system:serviceaccount:<namespace>:<name>, named exactly, as AccountEntry.Account is.
	Account string
	// Service is the service the minted token names in place of a person: the one a resource
	// server reads to decide what the caller is.
	Service string
	// Session is the session name every token minted for this account carries, and a request
	// naming any other is refused. It is required because a token that names no person has
	// nobody whose session a request could name, and it is one of the exchange.Set's
	// Reservations, held for the account as AccountEntry.Session is.
	Session string
}
